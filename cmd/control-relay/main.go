// control-relay : serveur d'appariement pour `control share` / `control join`.
//
// Usage :
//
//	control-relay              # écoute sur :8080
//	control-relay :443         # écoute sur :443
//	control-relay 9000         # écoute sur :9000 (port nu accepté)
//	control-relay :443 --tls-cert cert.pem --tls-key key.pem
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nbardavid/control/internal/relay"
)

func main() {
	certFile := flag.String("tls-cert", "", "fichier de certificat TLS (active TLS si fourni)")
	keyFile := flag.String("tls-key", "", "fichier de clé privée TLS")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [port] [--tls-cert FILE --tls-key FILE]\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "  port : :8080 (défaut), :443, 9000, ...")
		flag.PrintDefaults()
	}
	flag.Parse()

	addr := normalizeAddr(":8080")
	if a := flag.Arg(0); a != "" {
		addr = normalizeAddr(a)
	}

	srv := relay.NewServer()
	mux := http.NewServeMux()
	mux.Handle("/ws", srv)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		scheme := "ws"
		if *certFile != "" && *keyFile != "" {
			scheme = "wss"
		}
		log.Printf("control-relay listening on %s://0.0.0.0%s/ws", scheme, addr)
		var err error
		if *certFile != "" && *keyFile != "" {
			err = httpSrv.ListenAndServeTLS(*certFile, *keyFile)
		} else {
			err = httpSrv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server error: %v", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// normalizeAddr accepte "8080", ":8080", "127.0.0.1:8080" et renvoie une
// forme valide pour net.Listen ("[host]:port"). Permet d'écrire juste un
// numéro de port au lieu de `:8080`.
func normalizeAddr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ":8080"
	}
	if strings.Contains(s, ":") {
		return s
	}
	if _, err := strconv.Atoi(s); err == nil {
		return ":" + s
	}
	return s
}
