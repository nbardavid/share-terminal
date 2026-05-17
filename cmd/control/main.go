// control : CLI client pour partager (`share`) ou rejoindre (`join`) un
// terminal via un relay.
//
// Usage rapide :
//
//	control share              # partage ton terminal (lecture seule)
//	control share -w           # autorise le client à taper aussi
//	control join CODE          # rejoint un terminal partagé
//
// Choix du relay (par ordre de priorité) :
//
//	--relay / -r URL
//	$CONTROL_RELAY
//	ws://localhost:8080         (défaut, pour dev local)
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

// version est injectée au build via ldflags ("-X main.version=v0.1.0").
// Valeur par défaut "dev" pour les builds locaux.
var version = "dev"

// defaultRelay est l'URL utilisée si ni --relay ni $CONTROL_RELAY ne sont
// fournis. Surchargeable au build pour shipper un binaire qui pointe
// directement vers un relay précis (typiquement le tien) :
//
//	go build -ldflags "-X main.defaultRelay=wss://relay.toi.com" ./cmd/control
//
// Note sécurité : hardcoder une URL ici n'affaiblit pas le chiffrement.
// Le relay est UNTRUSTED par construction (PAKE + E2E AEAD), il ne voit
// jamais le clair. Le seul vrai risque c'est l'availability : si ton VPS
// tombe, le binaire ne peut plus établir de session sans override --relay.
var defaultRelay = "ws://localhost:8080"

func main() {
	root := &cobra.Command{
		Use:     "control",
		Short:   "Partage de terminal CLI via relay (E2E chiffré).",
		Version: version,
		Long: `control partage interactivement un terminal entre deux machines via un
relay. Inspiré de croc (pairing par code 3 mots) et de tmate (le host
voit son propre shell, le client le suit en temps réel).

Le relay est choisi dans cet ordre :
  --relay / -r URL  >  $CONTROL_RELAY  >  ` + defaultRelay,
	}
	root.AddCommand(newShareCmd(), newJoinCmd(), newUpdateCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "erreur:", err)
		os.Exit(1)
	}
}

// --- update ---

func newUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Met à jour control depuis la dernière GitHub Release.",
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
		Short: "Partage ton terminal — affiche un code à transmettre.",
		RunE: func(cmd *cobra.Command, args []string) error {
			relay = resolveRelay(relay)
			if err := validateRelayURL(relay, insecure); err != nil {
				return err
			}

			code := pairing.Generate()
			fmt.Printf("\n🔗  Code de partage :  \x1b[1m%s\x1b[0m\n", code)
			fmt.Printf("    (à transmettre à la personne qui se connecte)\n\n")
			if write {
				fmt.Printf("    Mode : \x1b[33mLECTURE + ÉCRITURE\x1b[0m  (le client peut taper au clavier)\n")
			} else {
				fmt.Printf("    Mode : LECTURE SEULE  (-w pour autoriser la saisie)\n")
			}
			fmt.Printf("    Le code expire dans 10 minutes.\n\n")

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			fmt.Print("⠋ En attente d'un peer...\n")
			conn, err := dialRelay(ctx, relay, code)
			if err != nil {
				return err
			}
			defer conn.Close()

			secure, fp, err := crypto.Wrap(ctx, conn, []byte(code), crypto.RoleHost)
			if err != nil {
				if errors.Is(err, crypto.ErrCodeMismatch) {
					return errors.New("le peer a fourni un mauvais code")
				}
				return fmt.Errorf("handshake: %w", err)
			}
			fmt.Printf("✓ Peer apparié (empreinte \x1b[2m%s\x1b[0m)\n", fp)

			opts := host.Options{
				Write: write,
				OnPeerMeta: func(m proto.Meta) bool {
					fmt.Printf("  Demande de connexion : \x1b[1m%s@%s\x1b[0m\n", m.User, m.Host)
					fmt.Print("  Accepter ? [y/N] ")
					reader := bufio.NewReader(os.Stdin)
					line, _ := reader.ReadString('\n')
					ok := strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
					if ok {
						fmt.Print("\n  Le shell démarre. `exit` ou Ctrl+D pour terminer.\n\n")
					} else {
						fmt.Println("  Refusé.")
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
	cmd.Flags().StringVarP(&relay, "relay", "r", "", "URL du relay (défaut: $CONTROL_RELAY ou "+defaultRelay+")")
	cmd.Flags().BoolVarP(&write, "write", "w", false, "Autoriser le client à saisir au clavier")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "Autoriser ws:// non chiffré vers un hôte non-local")
	return cmd
}

// --- join ---

func newJoinCmd() *cobra.Command {
	var relay string
	var insecure bool
	cmd := &cobra.Command{
		Use:   "join <code>",
		Short: "Rejoint un terminal partagé via son code.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			relay = resolveRelay(relay)
			if err := validateRelayURL(relay, insecure); err != nil {
				return err
			}

			code, err := pairing.Normalize(args[0])
			if err != nil {
				return fmt.Errorf("code invalide : %w", err)
			}

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			fmt.Println("⠋ Connexion au relay...")
			conn, err := dialRelay(ctx, relay, code)
			if err != nil {
				return err
			}
			defer conn.Close()

			secure, fp, err := crypto.Wrap(ctx, conn, []byte(code), crypto.RoleClient)
			if err != nil {
				if errors.Is(err, crypto.ErrCodeMismatch) {
					return errors.New("code incorrect — la clé dérivée ne correspond pas à celle du host")
				}
				return fmt.Errorf("handshake: %w", err)
			}

			opts := client.Options{
				OnMeta: func(m proto.Meta) {
					mode := "lecture seule"
					if m.Write {
						mode = "lecture + écriture"
					}
					fmt.Printf("✓ Apparié avec \x1b[1m%s@%s\x1b[0m (empreinte \x1b[2m%s\x1b[0m)\n", m.User, m.Host, fp)
					fmt.Printf("  Mode : %s · Quitter : Ctrl+]\n\n", mode)
				},
			}
			err = client.Run(ctx, secure, opts)
			switch {
			case errors.Is(err, client.ErrUserQuit):
				fmt.Println("\nDéconnecté.")
				return nil
			case errors.Is(err, client.ErrRefused):
				fmt.Println("\nLe host a refusé la connexion.")
				return nil
			default:
				return err
			}
		},
	}
	cmd.Flags().StringVarP(&relay, "relay", "r", "", "URL du relay (défaut: $CONTROL_RELAY ou "+defaultRelay+")")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "Autoriser ws:// non chiffré vers un hôte non-local")
	return cmd
}

// --- helpers ---

// resolveRelay implémente la cascade flag > env > défaut.
func resolveRelay(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("CONTROL_RELAY"); v != "" {
		return v
	}
	return defaultRelay
}

// isLocalHost retourne true si l'URL pointe vers la machine locale.
// Dans ce cas on autorise ws:// sans --insecure (pas de risque réseau).
// 0.0.0.0 est exclu : c'est une adresse de bind, pas une cible de connect.
func isLocalHost(host string) bool {
	if host == "" {
		return false
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(strings.Trim(host, "[]")) // gère [::1]:port
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func validateRelayURL(s string, allowInsecure bool) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("URL relay invalide : %w", err)
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		if allowInsecure || isLocalHost(u.Host) {
			return nil
		}
		return fmt.Errorf("ws:// non chiffré vers %s — utilise wss:// ou ajoute --insecure si c'est volontaire", u.Host)
	default:
		return fmt.Errorf("scheme inattendu %q (utilise ws:// ou wss://)", u.Scheme)
	}
}

// dialRelay ouvre une connexion WebSocket au relay, envoie le code comme
// premier message texte ("hello"), et retourne la conn adaptée en net.Conn.
func dialRelay(ctx context.Context, relayURL, code string) (net.Conn, error) {
	if !strings.HasSuffix(relayURL, "/ws") {
		relayURL = strings.TrimRight(relayURL, "/") + "/ws"
	}
	wsConn, _, err := websocket.Dial(ctx, relayURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial relay: %w", err)
	}
	// Doit être >= la taille max d'une frame AEAD (~64 KiB) émise par
	// internal/crypto. 1 MiB laisse une marge confortable.
	wsConn.SetReadLimit(1 << 20)
	if err := wsConn.Write(ctx, websocket.MessageText, []byte(code)); err != nil {
		_ = wsConn.Close(websocket.StatusInternalError, "hello write failed")
		return nil, fmt.Errorf("send hello: %w", err)
	}
	return websocket.NetConn(context.Background(), wsConn, websocket.MessageBinary), nil
}
