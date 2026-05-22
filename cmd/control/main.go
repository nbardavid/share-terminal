// control: CLI client to share (`share`) or join (`join`) a terminal
// through a relay.
//
// Quick usage:
//
//	control share              # share your terminal (read-only)
//	control share -w           # let the client type as well
//	control join CODE          # join a shared terminal
//
// Relay selection (priority order):
//
//	--relay / -r URL
//	$CONTROL_RELAY
//	ws://localhost:8080         (default, for local dev)
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/coder/websocket"
	"github.com/nbardavid/control/internal/client"
	"github.com/nbardavid/control/internal/crypto"
	"github.com/nbardavid/control/internal/host"
	"github.com/nbardavid/control/internal/pairing"
	"github.com/nbardavid/control/internal/proto"
	"github.com/nbardavid/control/internal/update"
	"github.com/spf13/cobra"
)

// version is injected at build time via ldflags ("-X main.version=v0.1.0").
// Defaults to "dev" for local builds.
var version = "dev"

// defaultRelay is the URL used when neither --relay nor $CONTROL_RELAY are
// provided. Can be overridden at build time to ship a binary that points
// directly at a specific relay (typically your own):
//
//	go build -ldflags "-X main.defaultRelay=wss://relay.example.com" ./cmd/control
//
// Security note: hardcoding a URL here does not weaken encryption. The relay
// is UNTRUSTED by construction (PAKE + E2E AEAD), it never sees plaintext.
// The only real risk is availability: if the VPS goes down, the binary can
// no longer establish a session without a --relay override.
var defaultRelay = "ws://localhost:8080"

func main() {
	root := &cobra.Command{
		Use:     "control",
		Short:   "CLI terminal sharing through a relay (E2E encrypted).",
		Version: version,
		Long: `control shares an interactive terminal between two machines through a
relay. Inspired by croc (3-word pairing code) and tmate (the host sees
its own shell, the client follows in real time).

The relay is selected in this order:
  --relay / -r URL  >  $CONTROL_RELAY  >  ` + defaultRelay,
	}
	root.AddCommand(newShareCmd(), newJoinCmd(), newUpdateCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// --- update ---

func newUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Update control from the latest GitHub Release.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return update.Run(ctx, version)
		},
	}
}

// --- share ---

func newShareCmd() *cobra.Command {
	var relay string
	var write bool
	var insecure bool
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Share your terminal — prints a code to hand off.",
		RunE: func(cmd *cobra.Command, args []string) error {
			relay = resolveRelay(relay)
			if err := validateRelayURL(relay, insecure); err != nil {
				return err
			}

			code := pairing.Generate()
			fmt.Printf("\nShare code:  \x1b[1m%s\x1b[0m\n", code)
			fmt.Printf("    (hand this off to whoever is connecting)\n\n")
			if write {
				fmt.Printf("    Mode: \x1b[33mREAD + WRITE\x1b[0m  (the client can type)\n")
			} else {
				fmt.Printf("    Mode: READ-ONLY  (-w to allow input)\n")
			}
			fmt.Printf("    The code expires in 10 minutes.\n\n")

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			fmt.Print("Waiting for a peer...\n")
			conn, err := dialRelay(ctx, relay, code)
			if err != nil {
				return err
			}
			defer conn.Close()

			secure, fp, err := crypto.Wrap(ctx, conn, []byte(code), crypto.RoleHost)
			if err != nil {
				if errors.Is(err, crypto.ErrCodeMismatch) {
					return errors.New("peer provided the wrong code")
				}
				return fmt.Errorf("handshake: %w", err)
			}
			fmt.Printf("Peer paired (fingerprint \x1b[2m%s\x1b[0m)\n", fp)

			opts := host.Options{
				Write: write,
				OnPeerMeta: func(m proto.Meta) bool {
					fmt.Printf("  Connection request: \x1b[1m%s@%s\x1b[0m\n", m.User, m.Host)
					fmt.Print("  Accept? [y/N] ")
					reader := bufio.NewReader(os.Stdin)
					line, _ := reader.ReadString('\n')
					ok := strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
					if ok {
						fmt.Print("\n  Shell starting. `exit` or Ctrl+D to end.\n\n")
					} else {
						fmt.Println("  Refused.")
					}
					return ok
				},
			}
			err = host.Run(ctx, secure, opts)
			if errors.Is(err, host.ErrPeerRefused) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVarP(&relay, "relay", "r", "", "Relay URL (default: $CONTROL_RELAY or "+defaultRelay+")")
	cmd.Flags().BoolVarP(&write, "write", "w", false, "Allow the client to type")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "Allow unencrypted ws:// to a non-local host")
	return cmd
}

// --- join ---

func newJoinCmd() *cobra.Command {
	var relay string
	var insecure bool
	cmd := &cobra.Command{
		Use:   "join <code>",
		Short: "Join a shared terminal by code.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			relay = resolveRelay(relay)
			if err := validateRelayURL(relay, insecure); err != nil {
				return err
			}

			code, err := pairing.Normalize(args[0])
			if err != nil {
				return fmt.Errorf("invalid code: %w", err)
			}

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			fmt.Println("Connecting to relay...")
			conn, err := dialRelay(ctx, relay, code)
			if err != nil {
				return err
			}
			defer conn.Close()

			secure, fp, err := crypto.Wrap(ctx, conn, []byte(code), crypto.RoleClient)
			if err != nil {
				if errors.Is(err, crypto.ErrCodeMismatch) {
					return errors.New("wrong code — derived key does not match the host's")
				}
				return fmt.Errorf("handshake: %w", err)
			}

			opts := client.Options{
				OnMeta: func(m proto.Meta) {
					mode := "read-only"
					if m.Write {
						mode = "read + write"
					}
					fmt.Printf("Paired with \x1b[1m%s@%s\x1b[0m (fingerprint \x1b[2m%s\x1b[0m)\n", m.User, m.Host, fp)
					fmt.Printf("  Mode: %s · Quit: Ctrl+]\n\n", mode)
				},
			}
			err = client.Run(ctx, secure, opts)
			switch {
			case errors.Is(err, client.ErrUserQuit):
				fmt.Println("\nDisconnected.")
				return nil
			case errors.Is(err, client.ErrRefused):
				fmt.Println("\nHost refused the connection.")
				return nil
			default:
				return err
			}
		},
	}
	cmd.Flags().StringVarP(&relay, "relay", "r", "", "Relay URL (default: $CONTROL_RELAY or "+defaultRelay+")")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "Allow unencrypted ws:// to a non-local host")
	return cmd
}

// --- helpers ---

// resolveRelay applies the flag > env > default cascade.
func resolveRelay(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("CONTROL_RELAY"); v != "" {
		return v
	}
	return defaultRelay
}

// isLocalHost reports whether the URL points to the local machine. In that
// case ws:// is allowed without --insecure (no network risk). 0.0.0.0 is
// excluded: it's a bind address, not a connect target.
func isLocalHost(host string) bool {
	if host == "" {
		return false
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(strings.Trim(host, "[]")) // handle [::1]:port
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func validateRelayURL(s string, allowInsecure bool) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("invalid relay URL: %w", err)
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		if allowInsecure || isLocalHost(u.Host) {
			return nil
		}
		return fmt.Errorf("unencrypted ws:// to %s — use wss:// or pass --insecure if this is intentional", u.Host)
	default:
		return fmt.Errorf("unexpected scheme %q (use ws:// or wss://)", u.Scheme)
	}
}

// dialRelay opens a WebSocket connection to the relay, sends the code as
// the first text message ("hello"), and returns the conn adapted to net.Conn.
func dialRelay(ctx context.Context, relayURL, code string) (net.Conn, error) {
	if !strings.HasSuffix(relayURL, "/ws") {
		relayURL = strings.TrimRight(relayURL, "/") + "/ws"
	}
	wsConn, _, err := websocket.Dial(ctx, relayURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial relay: %w", err)
	}
	// Must be >= the max AEAD frame size (~64 KiB) emitted by
	// internal/crypto. 1 MiB leaves a comfortable margin.
	wsConn.SetReadLimit(1 << 20)
	if err := wsConn.Write(ctx, websocket.MessageText, []byte(code)); err != nil {
		_ = wsConn.Close(websocket.StatusInternalError, "hello write failed")
		return nil, fmt.Errorf("send hello: %w", err)
	}
	return websocket.NetConn(context.Background(), wsConn, websocket.MessageBinary), nil
}
