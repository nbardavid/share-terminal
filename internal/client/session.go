// Package client: client side of the terminal sharing.
//
// Given a net.Conn already encrypted by internal/crypto, client.Run:
//
//  1. reads the Meta frame sent by the host and returns it via OnMeta
//  2. puts stdin into raw mode
//  3. sends the initial terminal size (FrameResize)
//  4. loops conn -> stdout (FrameData frames)
//  5. loops stdin -> conn (FrameInput frames); detects Ctrl+] to quit
//  6. relays local SIGWINCH as FrameResize frames
//
// The terminal is restored on every exit path (defer Restore).
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

	"github.com/nbardavid/control/internal/proto"
	"golang.org/x/term"
)

// ctrlBracket: Ctrl+] — escape sequence to quit cleanly.
const ctrlBracket = 0x1d

type Options struct {
	// OnMeta is called once, after receiving the host's metadata (i.e.
	// once the host has accepted the connection).
	OnMeta func(proto.Meta)
}

// ErrUserQuit: the user pressed Ctrl+], not a real error.
var ErrUserQuit = errors.New("user quit (Ctrl+])")

// ErrRefused: the host refused the connection (sent a FrameClose instead
// of its metadata).
var ErrRefused = errors.New("host refused the connection")

// Run drives the client-side session until disconnect (host close,
// Ctrl+], dead conn). The terminal is restored to its initial state on
// return.
//
// Protocol sequence:
//  1. The client sends its FrameMeta first (who it is).
//  2. The host replies with either FrameMeta (accepted) or FrameClose (refused).
//  3. Streaming begins.
func Run(ctx context.Context, conn net.Conn, opts Options) error {
	// 1. Send our meta first.
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

	// 2. Wait for the host's reply.
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

	// 2. Raw mode on stdin (with guaranteed restore).
	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		return errors.New("stdin is not a terminal — control join must run in a real TTY")
	}
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer func() { _ = term.Restore(stdinFd, oldState) }()

	// 3. Send the initial size.
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
		// Courtesy: let the host know we're leaving.
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
			// The host may resend meta if the mode changes; ignored in MVP.
		default:
			// Unknown frames: ignored (forward compatibility).
		}
	}
}

func stdinToConn(conn net.Conn) error {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			// Ctrl+] detection: check whether the byte appears in the chunk.
			// In raw mode every keypress arrives as one or a few bytes, so a
			// direct comparison is enough.
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
	return proto.Meta{User: u.Username, Host: host}, nil
}
