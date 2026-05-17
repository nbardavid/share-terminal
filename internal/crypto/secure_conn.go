// Package crypto : handshake PAKE + chiffrement de stream AEAD.
//
// Wrap() exécute un handshake SPAKE2 (schollz/pake/v3) sur une net.Conn
// quelconque en utilisant le code de pairing comme passphrase faible, puis
// retourne une nouvelle net.Conn dont les écritures et lectures sont
// chiffrées avec XChaCha20-Poly1305 et framées par préfixe de longueur.
//
// Côté API : les deux pairs appellent Wrap avec le même code et des rôles
// opposés (RoleHost / RoleClient). On récupère une net.Conn chiffrée et une
// empreinte courte de la clé de session — à afficher aux deux côtés pour
// permettre une vérification visuelle (très optionnelle car PAKE garantit
// déjà l'authentification mutuelle via le code).
package crypto

import (
	"context"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/schollz/pake/v3"
	"golang.org/x/crypto/chacha20poly1305"
)

// HandshakeTimeout est le délai max pour terminer le handshake PAKE +
// confirmation HMAC. Un peer qui ne répond pas dans ce délai voit la
// connexion fermée.
const HandshakeTimeout = 30 * time.Second

// ErrCodeMismatch est retourné quand l'étape de confirmation montre que
// les deux peers ont dérivé des clés différentes (= codes différents).
var ErrCodeMismatch = errors.New("pairing code mismatch (codes différents ou peer hostile)")

// Role indique qui initie le handshake. Le host (celui qui partage le
// terminal) est l'initiateur, le client est le répondeur.
type Role int

const (
	RoleHost   Role = 0
	RoleClient Role = 1
)

// maxFrame est la taille maximale d'une frame chiffrée transportée sur le wire.
// Au-delà, on découpe l'écriture en plusieurs frames.
const maxFrame = 64 * 1024

// curve : siec donne le meilleur compromis perf/sécu d'après le README de pake/v3.
const curve = "siec"

// Wrap exécute le handshake PAKE puis retourne une net.Conn chiffrée et
// l'empreinte hex courte (16 caractères) de la clé de session.
//
// Important sur le timing du handshake : le host appelle Wrap dès qu'il s'est
// connecté au relay, AVANT qu'un peer ne se présente. Le premier read peut
// donc bloquer arbitrairement (jusqu'au pairingTimeout côté relay, 10 min).
// On n'arme HandshakeTimeout (30s) qu'après réception du premier message du
// peer — à partir de ce moment-là le handshake doit aboutir vite. Cela évite
// que le deadline expire pendant l'attente du peer.
func Wrap(ctx context.Context, conn net.Conn, code []byte, role Role) (net.Conn, string, error) {
	// Goroutine d'avortement : si ctx est annulé (SIGINT etc), on force la
	// conn à échouer immédiatement même si on est en train d'attendre un peer.
	abortCh := make(chan struct{})
	defer close(abortCh)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Unix(1, 0))
		case <-abortCh:
		}
	}()

	// armDeadline : à appeler une fois qu'on a la preuve qu'un peer est en
	// face (premier read réussi). À partir de là, le handshake est borné.
	armDeadline := func() error {
		d := time.Now().Add(HandshakeTimeout)
		if cd, ok := ctx.Deadline(); ok && cd.Before(d) {
			d = cd
		}
		return conn.SetDeadline(d)
	}

	p, err := pake.InitCurve(code, int(role), curve)
	if err != nil {
		return nil, "", fmt.Errorf("pake init: %w", err)
	}

	// Protocole : A envoie ses bytes ; B les Update et renvoie ses bytes ;
	// A les Update. Ensuite SessionKey() converge des deux côtés.
	if role == RoleHost {
		// Le write est non-bloquant côté wire (bufferisé par le WS jusqu'au
		// pairing). On ne pose pas de deadline ici.
		if err := writeMsg(conn, p.Bytes()); err != nil {
			return nil, "", fmt.Errorf("pake send: %w", err)
		}
		// Ce read peut attendre longtemps si le peer n'est pas encore là.
		// Pas de deadline — relay s'occupe d'éjecter après pairingTimeout.
		peer, err := readMsg(conn)
		if err != nil {
			return nil, "", fmt.Errorf("pake recv: %w", err)
		}
		// Peer présent : maintenant on borne la suite du handshake.
		if err := armDeadline(); err != nil {
			return nil, "", fmt.Errorf("arm handshake deadline: %w", err)
		}
		if err := p.Update(peer); err != nil {
			return nil, "", fmt.Errorf("pake update: %w", err)
		}
	} else {
		// Côté client : on lit d'abord ce que le host a déjà mis en file.
		// Peut attendre si le host n'est pas encore arrivé côté relay.
		peer, err := readMsg(conn)
		if err != nil {
			return nil, "", fmt.Errorf("pake recv: %w", err)
		}
		if err := armDeadline(); err != nil {
			return nil, "", fmt.Errorf("arm handshake deadline: %w", err)
		}
		if err := p.Update(peer); err != nil {
			return nil, "", fmt.Errorf("pake update: %w", err)
		}
		if err := writeMsg(conn, p.Bytes()); err != nil {
			return nil, "", fmt.Errorf("pake send: %w", err)
		}
	}

	key, err := p.SessionKey()
	if err != nil {
		return nil, "", fmt.Errorf("session key: %w", err)
	}

	// Confirmation : chaque côté envoie un HMAC d'une chaîne fixe sous la clé
	// dérivée. Si les codes diffèrent, les clés diffèrent, donc les HMAC aussi
	// et la vérification échoue. C'est l'étape qui transforme PAKE en
	// "authentification mutuelle qui rate proprement" en cas de mauvais code.
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("control-key-confirm-v1"))
	myMAC := mac.Sum(nil)

	if role == RoleHost {
		if err := writeMsg(conn, myMAC); err != nil {
			return nil, "", fmt.Errorf("confirm send: %w", err)
		}
		peerMAC, err := readMsg(conn)
		if err != nil {
			return nil, "", fmt.Errorf("confirm recv: %w", err)
		}
		if subtle.ConstantTimeCompare(myMAC, peerMAC) != 1 {
			return nil, "", ErrCodeMismatch
		}
	} else {
		peerMAC, err := readMsg(conn)
		if err != nil {
			return nil, "", fmt.Errorf("confirm recv: %w", err)
		}
		if subtle.ConstantTimeCompare(myMAC, peerMAC) != 1 {
			// Renvoyer quand même un MAC bidon pour que l'autre côté
			// détecte aussi le mismatch et ferme proprement.
			_ = writeMsg(conn, make([]byte, len(myMAC)))
			return nil, "", ErrCodeMismatch
		}
		if err := writeMsg(conn, myMAC); err != nil {
			return nil, "", fmt.Errorf("confirm send: %w", err)
		}
	}

	// XChaCha20-Poly1305 demande une clé de 32 bytes ; on hash pour garantir la longueur.
	derived := sha256.Sum256(key)
	aead, err := chacha20poly1305.NewX(derived[:])
	if err != nil {
		return nil, "", fmt.Errorf("aead init: %w", err)
	}

	fp := sha256.Sum256(append([]byte("control-fp:"), key...))
	// Le handshake est terminé : on rétablit la conn sans deadline pour
	// que le streaming ne timeout pas après HandshakeTimeout.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, "", fmt.Errorf("clear handshake deadline: %w", err)
	}
	return &secureConn{Conn: conn, aead: aead}, hex.EncodeToString(fp[:8]), nil
}

// secureConn implémente net.Conn en chiffrant chaque Write en une frame
// indépendante et en déchiffrant frame par frame côté Read.
type secureConn struct {
	net.Conn
	aead cipher.AEAD

	readMu  sync.Mutex
	readBuf []byte // reste d'une frame déchiffrée pas encore consommée

	writeMu sync.Mutex
}

func (c *secureConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	if len(c.readBuf) == 0 {
		frame, err := readMsg(c.Conn)
		if err != nil {
			return 0, err
		}
		ns := c.aead.NonceSize()
		if len(frame) < ns+c.aead.Overhead() {
			return 0, errors.New("frame too short")
		}
		nonce, ct := frame[:ns], frame[ns:]
		pt, err := c.aead.Open(nil, nonce, ct, nil)
		if err != nil {
			return 0, fmt.Errorf("decrypt: %w", err)
		}
		c.readBuf = pt
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

func (c *secureConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	ns := c.aead.NonceSize()
	chunkMax := maxFrame - ns - c.aead.Overhead() - 4 // -4 pour la longueur

	sent := 0
	for sent < len(p) {
		end := sent + chunkMax
		if end > len(p) {
			end = len(p)
		}
		nonce := make([]byte, ns)
		if _, err := rand.Read(nonce); err != nil {
			return sent, err
		}
		ct := c.aead.Seal(nil, nonce, p[sent:end], nil)
		frame := make([]byte, 0, ns+len(ct))
		frame = append(frame, nonce...)
		frame = append(frame, ct...)
		if err := writeMsg(c.Conn, frame); err != nil {
			return sent, err
		}
		sent = end
	}
	return sent, nil
}

// writeMsg / readMsg : préfixe de longueur uint32 big-endian.
func writeMsg(w io.Writer, b []byte) error {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(b)))
	if _, err := w.Write(l[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readMsg(r io.Reader) ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(l[:])
	if n == 0 || n > maxFrame {
		return nil, fmt.Errorf("frame length out of range: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
