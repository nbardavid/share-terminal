// Package host: host side of the terminal sharing (tmate-style model).
//
// Given a net.Conn already encrypted by internal/crypto, host.Run:
//
//  1. reads the client's FrameMeta then calls OnPeerMeta for the y/N prompt
//  2. sends its own FrameMeta (who I am, --write mode)
//  3. attaches the host terminal (raw mode + size) unless disabled
//  4. spawns $SHELL in a PTY initially sized to the host
//  5. fans PTY output out to (host stdout + conn as FrameData)
//  6. pumps host stdin → PTY (the host types in the shared shell)
//  7. pumps conn → PTY (FrameInput when --write, FrameResize, FrameClose)
//  8. reconciles the PTY size to min(host, client) on every SIGWINCH
//
// When the shell exits (`exit` / Ctrl+D) or the conn dies, we restore the
// terminal BEFORE returning so that the goodbye messages render in normal
// line mode.
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
	// Write allows client keyboard input (FrameInput → PTY).
	Write bool
	// Shell is the shell binary to spawn. If empty: $SHELL or /bin/sh.
	Shell string
	// OnPeerMeta is called with the client metadata right after the
	// handshake. Returning false refuses the connection (the host sends a
	// FrameClose and Run returns ErrPeerRefused). If nil: accept without a
	// prompt.
	OnPeerMeta func(proto.Meta) bool
	// NoLocalAttach disables local attachment (raw mode, fan-out to
	// stdout, stdin pump). Used in tests that don't have a TTY. Default
	// (false): the host's terminal becomes the shared shell.
	NoLocalAttach bool
}

// ErrPeerRefused: OnPeerMeta returned false, the host refused the client.
var ErrPeerRefused = errors.New("peer refused by host")

// defaultSize: used when running without a local terminal attached.
var defaultSize = &pty.Winsize{Cols: 80, Rows: 24}

// Run drives the host-side session. Blocks until the shell or the conn ends.
func Run(ctx context.Context, conn net.Conn, opts Options) error {
	if err := exchangeMeta(conn, opts); err != nil {
		return err
	}

	// Attach the host's local terminal (raw mode + size).
	var localFD int
	var localOldState *term.State
	hostSize := *defaultSize

	if !opts.NoLocalAttach {
		localFD = int(os.Stdin.Fd())
		if !term.IsTerminal(localFD) {
			return errors.New("control share must run in an interactive terminal (stdin TTY)")
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
	// Guaranteed: the terminal is restored on every exit path (normal
	// return, panic, ctx cancel). Must happen BEFORE any goodbye message
	// is written to stdout, so it's deferred at the top of the function.
	defer func() {
		if localOldState != nil {
			_ = term.Restore(localFD, localOldState)
		}
	}()

	// Spawn the shell at the initial size.
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

	// Size manager: keep host and client separately and apply
	// min(host, client) to the PTY on every change.
	sizeMgr := newSizeMgr(ptyFile, hostSize, hostSize)

	// SIGWINCH on the host side: only when locally attached.
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

	// Pumps. Four directions, so four goroutines:
	//   1) PTY → (local stdout + conn FrameData) — fan-out
	//   2) local stdin → PTY (when attached)
	//   3) conn → PTY (FrameInput/FrameResize/FrameClose)
	//   4) cmd.Wait ("normal" end signal)
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
			// io.Copy from stdin to the PTY. When the PTY closes (defer),
			// the Write fails and io.Copy returns.
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
			return nil // non-zero shell exit: normal
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

// exchangeMeta: read the client's meta, call OnPeerMeta, send ours.
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

// ptyFanOut reads the PTY and writes each chunk to localStdout (when
// non-nil) AND to conn (FrameData). Loops until EOF or error.
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

// connToPTY reads frames from the client and dispatches (input, resize, close).
func connToPTY(conn net.Conn, ptyf *os.File, allowWrite bool, sizes *sizeMgr) error {
	for {
		t, payload, err := proto.Read(conn)
		if err != nil {
			return err
		}
		switch t {
		case proto.FrameInput:
			if !allowWrite {
				continue // silently dropped in read-only mode
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
			// unknown frames ignored (forward compatibility)
		}
	}
}

// sizeMgr keeps host and client sizes and applies min() to the PTY.
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
