// Package crypto: PAKE handshake plus AEAD stream encryption.
//
// Wrap() runs a SPAKE2 handshake (schollz/pake/v3) over an arbitrary
// net.Conn using the pairing code as a weak passphrase, then returns a new
// net.Conn whose reads and writes are encrypted with XChaCha20-Poly1305
// and length-prefixed.
//
// API: both peers call Wrap with the same code and opposite roles
// (RoleHost / RoleClient). The result is an encrypted net.Conn and a short
// fingerprint of the session key — display it on both sides to allow a
// visual check (mostly optional, since PAKE already provides mutual
// authentication via the code).
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

// HandshakeTimeout is the maximum time allowed to complete the PAKE
// handshake plus HMAC confirmation. A peer that doesn't respond within
// this window has its connection closed.
const HandshakeTimeout = 30 * time.Second

// ErrCodeMismatch is returned when the confirmation step shows the two
// peers derived different keys (i.e. different codes).
var ErrCodeMismatch = errors.New("pairing code mismatch (codes differ or hostile peer)")

// Role indicates who initiates the handshake. The host (the one sharing
// the terminal) is the initiator, the client is the responder.
type Role int

const (
	RoleHost   Role = 0
	RoleClient Role = 1
)

// maxFrame is the maximum size of an encrypted frame on the wire. Larger
// writes are split into multiple frames.
const maxFrame = 64 * 1024

// curve: siec gives the best perf/security trade-off per the pake/v3 README.
const curve = "siec"

// Wrap runs the PAKE handshake then returns an encrypted net.Conn and the
// short hex fingerprint (16 chars) of the session key.
//
// Note on handshake timing: the host calls Wrap as soon as it has connected
// to the relay, BEFORE a peer shows up. The first read can therefore block
// arbitrarily (up to pairingTimeout on the relay side, 10 min). We only
// arm HandshakeTimeout (30s) after receiving the first peer message — from
// that point on the handshake must complete quickly. This prevents the
// deadline from firing while waiting for a peer.
func Wrap(ctx context.Context, conn net.Conn, code []byte, role Role) (net.Conn, string, error) {
	// Abort goroutine: if ctx is cancelled (SIGINT etc.), force the conn to
	// fail immediately even if we're currently waiting on a peer.
	abortCh := make(chan struct{})
	defer close(abortCh)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Unix(1, 0))
		case <-abortCh:
		}
	}()

	// armDeadline: call this once we have evidence a peer is on the other
	// end (first successful read). From then on the handshake is bounded.
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

	// Protocol: A sends its bytes; B Updates them and sends its own bytes;
	// A Updates. Then SessionKey() converges on both sides.
	if role == RoleHost {
		// The write is non-blocking on the wire (buffered by the WS until
		// pairing). No deadline here.
		if err := writeMsg(conn, p.Bytes()); err != nil {
			return nil, "", fmt.Errorf("pake send: %w", err)
		}
		// This read can wait a long time if the peer is not there yet.
		// No deadline — the relay evicts after pairingTimeout.
		peer, err := readMsg(conn)
		if err != nil {
			return nil, "", fmt.Errorf("pake recv: %w", err)
		}
		// Peer is present: bound the rest of the handshake.
		if err := armDeadline(); err != nil {
			return nil, "", fmt.Errorf("arm handshake deadline: %w", err)
		}
		if err := p.Update(peer); err != nil {
			return nil, "", fmt.Errorf("pake update: %w", err)
		}
	} else {
		// Client side: first read what the host has already queued.
		// May block if the host hasn't reached the relay yet.
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

	// Confirmation: each side sends an HMAC of a fixed string under the
	// derived key. If the codes differ the keys differ, so the HMACs
	// differ and the check fails. This turns PAKE into "mutual auth that
	// fails cleanly" on a wrong code.
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
			// Still send a dummy MAC so the other side also detects the
			// mismatch and closes cleanly.
			_ = writeMsg(conn, make([]byte, len(myMAC)))
			return nil, "", ErrCodeMismatch
		}
		if err := writeMsg(conn, myMAC); err != nil {
			return nil, "", fmt.Errorf("confirm send: %w", err)
		}
	}

	// XChaCha20-Poly1305 needs a 32-byte key; hash to guarantee the length.
	derived := sha256.Sum256(key)
	aead, err := chacha20poly1305.NewX(derived[:])
	if err != nil {
		return nil, "", fmt.Errorf("aead init: %w", err)
	}

	fp := sha256.Sum256(append([]byte("control-fp:"), key...))
	// Handshake done: clear the deadline so streaming doesn't time out
	// after HandshakeTimeout.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, "", fmt.Errorf("clear handshake deadline: %w", err)
	}
	return &secureConn{Conn: conn, aead: aead}, hex.EncodeToString(fp[:8]), nil
}

// secureConn implements net.Conn by encrypting each Write into an
// independent frame and decrypting frame by frame on Read.
type secureConn struct {
	net.Conn
	aead cipher.AEAD

	readMu  sync.Mutex
	readBuf []byte // leftover of a decrypted frame not yet consumed

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
	chunkMax := maxFrame - ns - c.aead.Overhead() - 4 // -4 for the length prefix

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

// writeMsg / readMsg: big-endian uint32 length prefix.
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
