package host_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nbardavid/control/internal/crypto"
	"github.com/nbardavid/control/internal/host"
	"github.com/nbardavid/control/internal/proto"
	"github.com/nbardavid/control/internal/relay"
)

// TestEndToEnd: real relay via httptest + host.Run (real PTY + shell) + a
// minimal "test client" that speaks the protocol directly. We run a
// "printf CONTROL_OK" command via FrameInput and check the string comes
// back in a FrameData.
//
// This exercises the whole path: WS, relay splice, PAKE, AEAD, framing, PTY.
func TestEndToEnd(t *testing.T) {
	srv := httptest.NewServer(relay.NewServer())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	code := "meteor-cobalt-jungle"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	hostDone := make(chan error, 1)
	clientDone := make(chan error, 1)
	collected := make(chan []byte, 1)

	// Host: connect, send hello, wrap, host.Run with --write enabled.
	go func() {
		hostDone <- func() error {
			ws, _, err := websocket.Dial(ctx, wsURL, nil)
			if err != nil {
				return err
			}
			if err := ws.Write(ctx, websocket.MessageText, []byte(code)); err != nil {
				return err
			}
			nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
			secure, _, err := crypto.Wrap(ctx, nc, []byte(code), crypto.RoleHost)
			if err != nil {
				return err
			}
			return host.Run(ctx, secure, host.Options{
				Write:         true,
				Shell:         "/bin/sh",
				NoLocalAttach: true, // no TTY in the test runner
				OnPeerMeta:    func(_ proto.Meta) bool { return true },
			})
		}()
	}()

	// Client: connect, hello, wrap, read meta, send a command, read the output.
	go func() {
		clientDone <- func() error {
			ws, _, err := websocket.Dial(ctx, wsURL, nil)
			if err != nil {
				return err
			}
			if err := ws.Write(ctx, websocket.MessageText, []byte(code)); err != nil {
				return err
			}
			nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
			secure, _, err := crypto.Wrap(ctx, nc, []byte(code), crypto.RoleClient)
			if err != nil {
				return err
			}

			// 1. Send our meta (client → host) first.
			myMeta := proto.Meta{User: "alice", Host: "test-client"}
			mb, _ := myMeta.Bytes()
			if err := proto.Write(secure, proto.FrameMeta, mb); err != nil {
				return err
			}

			// 2. Read the host meta (host accepts and sends its meta).
			ft, payload, err := proto.Read(secure)
			if err != nil {
				return err
			}
			if ft != proto.FrameMeta {
				t.Errorf("expected FrameMeta, got 0x%02x", ft)
			}
			m, err := proto.ParseMeta(payload)
			if err != nil {
				return err
			}
			if !m.Write {
				t.Errorf("expected Write=true in meta")
			}

			// 3. Send an initial size (helps /bin/sh).
			rs := proto.ResizePayload{Cols: 80, Rows: 24}
			if err := proto.Write(secure, proto.FrameResize, rs.Bytes()); err != nil {
				return err
			}

			// 4. Send a command that produces identifiable output, then exit.
			cmd := "printf 'CONTROL_OK\\n'; exit\n"
			if err := proto.Write(secure, proto.FrameInput, []byte(cmd)); err != nil {
				return err
			}

			// 5. Read until CONTROL_OK appears or FrameClose arrives.
			var buf bytes.Buffer
			for {
				ft, payload, err := proto.Read(secure)
				if err != nil {
					break
				}
				if ft == proto.FrameData {
					buf.Write(payload)
					if bytes.Contains(buf.Bytes(), []byte("CONTROL_OK")) {
						collected <- buf.Bytes()
						return nil
					}
				}
				if ft == proto.FrameClose {
					break
				}
			}
			collected <- buf.Bytes()
			return nil
		}()
	}()

	select {
	case got := <-collected:
		if !bytes.Contains(got, []byte("CONTROL_OK")) {
			t.Fatalf("expected CONTROL_OK in output, got:\n%s", got)
		}
	case err := <-hostDone:
		t.Fatalf("host returned before client got output: %v", err)
	case err := <-clientDone:
		t.Fatalf("client returned without producing output: %v", err)
	case <-ctx.Done():
		t.Fatalf("timeout: %v", ctx.Err())
	}

	// Cleanup: wait on the goroutines (short timeout).
	cancel()
	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-hostDone:
		case <-clientDone:
		case <-timeout:
			return // not a problem, the main test already passed
		}
	}
}
