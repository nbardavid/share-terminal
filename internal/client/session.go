// Package client : côté client du partage de terminal.
//
// Une fois la net.Conn déjà chiffrée par internal/crypto, client.Run :
//
//  1. lit la frame Meta envoyée par le host et la retourne via le callback OnMeta
//  2. passe stdin en raw mode
//  3. envoie la taille initiale du terminal (FrameResize)
//  4. boucle conn -> stdout (frames FrameData)
//  5. boucle stdin -> conn (frames FrameInput) ; détecte Ctrl+] pour quitter
//  6. relaie SIGWINCH côté local en frames FrameResize
//
// Le terminal est restauré dans tous les chemins de sortie (defer Restore).
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"os/user"
	"syscall"

	"github.com/nbardavid/control/internal/compress"
	"github.com/nbardavid/control/internal/proto"
	"golang.org/x/term"
)

// ctrlBracket : Ctrl+] — séquence d'échappement pour quitter proprement.
const ctrlBracket = 0x1d

type Options struct {
	// OnMeta est appelé une fois après réception de la metadata du host
	// (donc une fois que le host a accepté la connexion).
	OnMeta func(proto.Meta)
}

// ErrUserQuit : l'utilisateur a tapé Ctrl+], pas une vraie erreur.
var ErrUserQuit = errors.New("user quit (Ctrl+])")

// ErrRefused : le host a refusé la connexion (a renvoyé un FrameClose
// au lieu de sa metadata).
var ErrRefused = errors.New("host refused the connection")

// Run pilote la session côté client jusqu'à déconnexion (host close,
// Ctrl+], conn morte). Le terminal est restauré à l'état initial à la sortie.
//
// Séquence protocolaire :
//  1. Le client envoie d'abord sa FrameMeta (qui il est).
//  2. Le host répond soit FrameMeta (accepté) soit FrameClose (refusé).
//  3. Le streaming commence.
func Run(ctx context.Context, conn net.Conn, opts Options) error {
	// 1. Envoyer notre meta en premier.
	me, err := selfMeta()
	if err != nil {
		return fmt.Errorf("collect self meta: %w", err)
	}
	meBytes, err := me.Bytes()
	if err != nil {
		return err
	}
	if err := proto.Write(conn, proto.FrameMeta, meBytes); err != nil {
		return fmt.Errorf("send meta: %w", err)
	}

	// 2. Attendre la réponse du host.
	t, payload, err := proto.Read(conn)
	if err != nil {
		return fmt.Errorf("read host meta: %w", err)
	}
	if t == proto.FrameClose {
		return ErrRefused
	}
	if t != proto.FrameMeta {
		return fmt.Errorf("expected FrameMeta, got 0x%02x", t)
	}
	meta, err := proto.ParseMeta(payload)
	if err != nil {
		return err
	}
	if opts.OnMeta != nil {
		opts.OnMeta(meta)
	}

	// Négociation : compression activée si les deux côtés annoncent "deflate".
	// Tous les bytes de meta étaient en clair ; à partir d'ici on bascule
	// (les deux peers font la bascule au même point logique du protocole).
	if meta.HasFeature(proto.FeatureDeflate) && me.HasFeature(proto.FeatureDeflate) {
		cc, err := compress.Wrap(conn)
		if err != nil {
			return fmt.Errorf("compress wrap: %w", err)
		}
		conn = cc
	}

	// 2. Raw mode sur stdin (avec restore garanti).
	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		return errors.New("stdin n'est pas un terminal — control join doit tourner dans un vrai TTY")
	}
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer func() { _ = term.Restore(stdinFd, oldState) }()

	// 3. Envoyer la taille initiale.
	if err := sendCurrentSize(conn, stdinFd); err != nil {
		return err
	}

	// 4. SIGWINCH → FrameResize.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			_ = sendCurrentSize(conn, stdinFd)
		}
	}()

	// 5. Pumps.
	connErr := make(chan error, 1)
	go func() { connErr <- connToStdout(conn) }()

	stdinErr := make(chan error, 1)
	go func() { stdinErr <- stdinToConn(conn) }()

	select {
	case err := <-connErr:
		return ignoreEOF(err)
	case err := <-stdinErr:
		// Politesse : on prévient le host qu'on s'en va.
		_ = proto.Write(conn, proto.FrameClose, nil)
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func sendCurrentSize(conn net.Conn, fd int) error {
	cols, rows, err := term.GetSize(fd)
	if err != nil {
		return fmt.Errorf("get size: %w", err)
	}
	r := proto.ResizePayload{Cols: uint16(cols), Rows: uint16(rows)}
	return proto.Write(conn, proto.FrameResize, r.Bytes())
}

func connToStdout(conn net.Conn) error {
	for {
		t, payload, err := proto.Read(conn)
		if err != nil {
			return err
		}
		switch t {
		case proto.FrameData:
			if _, err := os.Stdout.Write(payload); err != nil {
				return err
			}
		case proto.FrameClose:
			return io.EOF
		case proto.FrameMeta:
			// Le host peut renvoyer meta si le mode change ; on ignore au MVP.
		default:
			// Frames inconnues : ignorées (compat ascendante).
		}
	}
}

func stdinToConn(conn net.Conn) error {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			// Détection Ctrl+] : on regarde si la séquence est présente.
			// En raw mode, chaque keypress arrive comme un ou quelques bytes,
			// donc une comparaison directe suffit.
			if hasByte(buf[:n], ctrlBracket) {
				return ErrUserQuit
			}
			if werr := proto.Write(conn, proto.FrameInput, buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			return err
		}
	}
}

func hasByte(b []byte, c byte) bool {
	for _, x := range b {
		if x == c {
			return true
		}
	}
	return false
}

func ignoreEOF(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func selfMeta() (proto.Meta, error) {
	u, err := user.Current()
	if err != nil {
		return proto.Meta{}, err
	}
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return proto.Meta{
		User:     u.Username,
		Host:     host,
		Features: []string{proto.FeatureDeflate},
	}, nil
}
