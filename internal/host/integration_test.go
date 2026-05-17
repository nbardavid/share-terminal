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

// TestEndToEnd : relay réel via httptest + host.Run (PTY + shell réels) +
// un "test client" minimal qui parle directement le protocole. On lance une
// commande "echo hello-control" via FrameInput et on vérifie qu'on récupère
// la chaîne dans une FrameData.
//
// Ce test exerce tout : WS, splice du relay, PAKE, AEAD, framing, PTY.
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

	// Host : se connecte, dit hello, wrap, host.Run avec --write activé.
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
				NoLocalAttach: true, // pas de TTY dans le test runner
				OnPeerMeta:    func(_ proto.Meta) bool { return true },
			})
		}()
	}()

	// Client : connecte, hello, wrap, lit meta, envoie une commande, lit la sortie.
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

			// 1. Envoyer notre meta (client → host) en premier.
			myMeta := proto.Meta{User: "alice", Host: "test-client"}
			mb, _ := myMeta.Bytes()
			if err := proto.Write(secure, proto.FrameMeta, mb); err != nil {
				return err
			}

			// 2. Lire la meta du host (host accepte et envoie sa meta).
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

			// 3. Envoyer une taille initiale (utile pour /bin/sh).
			rs := proto.ResizePayload{Cols: 80, Rows: 24}
			if err := proto.Write(secure, proto.FrameResize, rs.Bytes()); err != nil {
				return err
			}

			// 4. Envoyer une commande qui produit une sortie identifiable + exit.
			cmd := "printf 'CONTROL_OK\\n'; exit\n"
			if err := proto.Write(secure, proto.FrameInput, []byte(cmd)); err != nil {
				return err
			}

			// 5. Lire jusqu'à voir CONTROL_OK ou jusqu'à FrameClose.
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

	// Cleanup : attendre les goroutines (avec timeout court).
	cancel()
	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-hostDone:
		case <-clientDone:
		case <-timeout:
			return // pas grave, le test principal est passé
		}
	}
}
