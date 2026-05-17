// Package host : côté hôte du partage de terminal (modèle tmate-style).
//
// Une fois la net.Conn déjà chiffrée par internal/crypto, host.Run :
//
//  1. lit la FrameMeta du client puis appelle OnPeerMeta pour le prompt y/N
//  2. envoie sa propre FrameMeta (qui je suis, mode --write)
//  3. attache le terminal du host (raw mode + dimensions) sauf si désactivé
//  4. spawn $SHELL dans un PTY de taille initialement = celle du host
//  5. fan-out PTY → (stdout host + conn en FrameData)
//  6. pump host stdin → PTY (le host tape dans le shell partagé)
//  7. pump conn → PTY (FrameInput si --write, FrameResize, FrameClose)
//  8. réconcilie la taille PTY en min(host, client) sur tout SIGWINCH
//
// Quand le shell se termine (`exit` / Ctrl+D) ou que la conn meurt, on
// restaure le terminal AVANT de retourner pour que les messages de fin
// s'affichent en mode ligne normal.
package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/nbardavid/control/internal/proto"
	"golang.org/x/term"
)

type Options struct {
	// Write autorise la saisie clavier du client (FrameInput → PTY).
	Write bool
	// Shell est le binaire shell à spawner. Si vide : $SHELL ou /bin/sh.
	Shell string
	// OnPeerMeta est appelé avec la metadata du client juste après le handshake.
	// Retourner false refuse la connexion (le host envoie un FrameClose et Run
	// renvoie ErrPeerRefused). Si nil : on accepte sans prompt.
	OnPeerMeta func(proto.Meta) bool
	// NoLocalAttach désactive l'attachement local (raw mode, fan-out vers
	// stdout, pump stdin). Utilisé dans les tests qui n'ont pas de TTY.
	// Par défaut (false), le terminal du host devient le shell partagé.
	NoLocalAttach bool
}

// ErrPeerRefused : OnPeerMeta a renvoyé false, le host a refusé le client.
var ErrPeerRefused = errors.New("peer refused by host")

// defaultSize : utilisé quand on tourne sans terminal local attaché.
var defaultSize = &pty.Winsize{Cols: 80, Rows: 24}

// Run pilote la session côté host. Bloque jusqu'à la fin du shell ou de la conn.
func Run(ctx context.Context, conn net.Conn, opts Options) error {
	if err := exchangeMeta(conn, opts); err != nil {
		return err
	}

	// Attache le terminal local du host (raw mode + lecture taille).
	var localFD int
	var localOldState *term.State
	hostSize := *defaultSize

	if !opts.NoLocalAttach {
		localFD = int(os.Stdin.Fd())
		if !term.IsTerminal(localFD) {
			return errors.New("control share doit tourner dans un terminal interactif (stdin TTY)")
		}
		cols, rows, err := term.GetSize(localFD)
		if err != nil {
			return fmt.Errorf("get host size: %w", err)
		}
		hostSize = pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}

		st, err := term.MakeRaw(localFD)
		if err != nil {
			return fmt.Errorf("make raw: %w", err)
		}
		localOldState = st
	}
	// Garanti : le terminal est restauré dans tous les chemins de sortie
	// (return normal, panic, ctx cancel). Doit se faire AVANT toute écriture
	// de message de fin sur stdout, donc on défère ici en tête de la fonction.
	defer func() {
		if localOldState != nil {
			_ = term.Restore(localFD, localOldState)
		}
	}()

	// Spawn shell avec la taille initiale.
	shell := chooseShell(opts.Shell)
	cmd := exec.CommandContext(ctx, shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptyFile, err := pty.StartWithSize(cmd, &hostSize)
	if err != nil {
		return fmt.Errorf("pty start: %w", err)
	}
	defer func() {
		_ = ptyFile.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	// Gestionnaire de tailles : on garde host et client séparément et on
	// applique min(host, client) au PTY à chaque changement.
	sizeMgr := newSizeMgr(ptyFile, hostSize, hostSize)

	// SIGWINCH côté host : si attaché localement.
	stopWinch := func() {}
	if !opts.NoLocalAttach {
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		stopWinch = func() { signal.Stop(winch); close(winch) }
		go func() {
			for range winch {
				c, r, err := term.GetSize(localFD)
				if err != nil {
					continue
				}
				sizeMgr.setHost(pty.Winsize{Cols: uint16(c), Rows: uint16(r)})
			}
		}()
	}
	defer stopWinch()

	// Pumps. Quatre directions, donc quatre goroutines :
	//   1) PTY → (stdout local + conn FrameData) — fan-out
	//   2) stdin local → PTY (si attaché)
	//   3) conn → PTY (FrameInput/FrameResize/FrameClose)
	//   4) cmd.Wait (signal de fin "normale")
	ptyOutErr := make(chan error, 1)
	go func() {
		var localStdout io.Writer
		if !opts.NoLocalAttach {
			localStdout = os.Stdout
		}
		ptyOutErr <- ptyFanOut(ptyFile, localStdout, conn)
	}()

	connInErr := make(chan error, 1)
	go func() {
		connInErr <- connToPTY(conn, ptyFile, opts.Write, sizeMgr)
	}()

	stdinErr := make(chan error, 1)
	if !opts.NoLocalAttach {
		go func() {
			// io.Copy de stdin vers le PTY. Quand le PTY se ferme (defer),
			// la Write échoue et io.Copy retourne.
			_, err := io.Copy(ptyFile, os.Stdin)
			stdinErr <- err
		}()
	}

	cmdWaitErr := make(chan error, 1)
	go func() { cmdWaitErr <- cmd.Wait() }()

	select {
	case err := <-cmdWaitErr:
		_ = proto.Write(conn, proto.FrameClose, nil)
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil // exit non-zero du shell : normal
		}
		return err
	case err := <-ptyOutErr:
		_ = proto.Write(conn, proto.FrameClose, nil)
		return ignoreEOF(err)
	case err := <-connInErr:
		return ignoreEOF(err)
	case err := <-stdinErr:
		return ignoreEOF(err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// exchangeMeta : lit la meta du client, appelle OnPeerMeta, envoie notre meta.
func exchangeMeta(conn net.Conn, opts Options) error {
	t, payload, err := proto.Read(conn)
	if err != nil {
		return fmt.Errorf("read peer meta: %w", err)
	}
	if t != proto.FrameMeta {
		return fmt.Errorf("expected FrameMeta from client, got 0x%02x", t)
	}
	peerMeta, err := proto.ParseMeta(payload)
	if err != nil {
		return err
	}

	if opts.OnPeerMeta != nil && !opts.OnPeerMeta(peerMeta) {
		_ = proto.Write(conn, proto.FrameClose, nil)
		return ErrPeerRefused
	}

	meta, err := selfMeta(opts.Write)
	if err != nil {
		return fmt.Errorf("collect meta: %w", err)
	}
	mb, err := meta.Bytes()
	if err != nil {
		return err
	}
	if err := proto.Write(conn, proto.FrameMeta, mb); err != nil {
		return fmt.Errorf("send meta: %w", err)
	}
	return nil
}

// ptyFanOut : lit le PTY et écrit chaque chunk sur localStdout (si non nil)
// ET sur conn (FrameData). Boucle jusqu'à EOF ou erreur.
func ptyFanOut(ptyf *os.File, localStdout io.Writer, conn net.Conn) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := ptyf.Read(buf)
		if n > 0 {
			if localStdout != nil {
				if _, werr := localStdout.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if werr := proto.Write(conn, proto.FrameData, buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			return err
		}
	}
}

// connToPTY : lit les frames du client et dispatch (input, resize, close).
func connToPTY(conn net.Conn, ptyf *os.File, allowWrite bool, sizes *sizeMgr) error {
	for {
		t, payload, err := proto.Read(conn)
		if err != nil {
			return err
		}
		switch t {
		case proto.FrameInput:
			if !allowWrite {
				continue // ignoré silencieusement en read-only
			}
			if _, err := ptyf.Write(payload); err != nil {
				return err
			}
		case proto.FrameResize:
			r, err := proto.ParseResize(payload)
			if err != nil {
				return err
			}
			sizes.setClient(pty.Winsize{Cols: r.Cols, Rows: r.Rows})
		case proto.FrameClose:
			return io.EOF
		default:
			// frames inconnues ignorées (compat ascendante)
		}
	}
}

// sizeMgr garde les tailles du host et du client et applique min() au PTY.
type sizeMgr struct {
	mu     sync.Mutex
	pty    *os.File
	host   pty.Winsize
	client pty.Winsize
}

func newSizeMgr(ptyf *os.File, host, client pty.Winsize) *sizeMgr {
	s := &sizeMgr{pty: ptyf, host: host, client: client}
	s.applyLocked()
	return s
}

func (s *sizeMgr) setHost(w pty.Winsize) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.host = w
	s.applyLocked()
}

func (s *sizeMgr) setClient(w pty.Winsize) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client = w
	s.applyLocked()
}

func (s *sizeMgr) applyLocked() {
	merged := pty.Winsize{
		Cols: minU16(s.host.Cols, s.client.Cols),
		Rows: minU16(s.host.Rows, s.client.Rows),
	}
	_ = pty.Setsize(s.pty, &merged)
}

func minU16(a, b uint16) uint16 {
	if a < b {
		return a
	}
	return b
}

func chooseShell(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

func selfMeta(write bool) (proto.Meta, error) {
	u, err := user.Current()
	if err != nil {
		return proto.Meta{}, err
	}
	hn, err := os.Hostname()
	if err != nil {
		hn = "unknown"
	}
	return proto.Meta{User: u.Username, Host: hn, Write: write}, nil
}

func ignoreEOF(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
