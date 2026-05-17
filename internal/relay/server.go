// Package relay : serveur d'appariement par code court.
//
// Le relay ne déchiffre rien et n'inspecte pas le payload : il maintient une
// map des codes en attente et, dès que deux peers présentent le même code, il
// splice les deux connexions WebSocket en un tube bidirectionnel jusqu'à ce
// que l'un des deux ferme.
package relay

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	pairingTimeout = 10 * time.Minute
	helloTimeout   = 10 * time.Second
	// readLimit : taille max d'un message WS lu. Doit être >= la taille max
	// d'une frame AEAD écrite par internal/crypto (qui chunk à ~64 KiB).
	// On met 1 MiB pour une marge confortable.
	readLimit = 1 << 20
	// maxWaiting : nombre max de sessions en attente d'un peer. Au-delà, on
	// rejette les nouvelles connexions pour éviter qu'un attaquant n'épuise
	// la mémoire en ouvrant des milliers de connexions oisives.
	defaultMaxWaiting = 1024
)

// Server est un http.Handler qui accepte les connexions WebSocket et les
// apparie par code court (envoyé en premier message texte par chaque peer).
type Server struct {
	mu      sync.Mutex
	waiting map[string]*pending

	maxWaiting int
}

type pending struct {
	conn   *websocket.Conn
	paired chan *websocket.Conn // le second peer dépose sa conn ici
	done   chan struct{}        // fermé quand le splice est terminé
}

func NewServer() *Server {
	return &Server{
		waiting:    make(map[string]*pending),
		maxWaiting: defaultMaxWaiting,
	}
}

// SetMaxWaiting ajuste la limite de sessions en attente (utile pour tests
// ou environnements à charge connue).
func (s *Server) SetMaxWaiting(n int) {
	s.mu.Lock()
	s.maxWaiting = n
	s.mu.Unlock()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // pas de browser CSRF à craindre pour un CLI
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(readLimit)

	peerAddr := remoteAddr(r)
	helloCtx, cancelHello := context.WithTimeout(r.Context(), helloTimeout)
	defer cancelHello()

	code, err := readHello(helloCtx, conn)
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "missing or invalid hello")
		return
	}

	s.mu.Lock()
	first, exists := s.waiting[code]
	if exists {
		delete(s.waiting, code)
		s.mu.Unlock()
		log.Printf("pair  code=%s peer1=? peer2=%s — splicing", codeMask(code), peerAddr)
		started := time.Now()
		// Délivre notre conn au premier, attend que le splice se termine.
		first.paired <- conn
		<-first.done
		log.Printf("end   code=%s duration=%s", codeMask(code), time.Since(started).Round(time.Second))
		return
	}
	if len(s.waiting) >= s.maxWaiting {
		s.mu.Unlock()
		log.Printf("reject code=%s peer=%s reason=max_waiting_reached", codeMask(code), peerAddr)
		_ = conn.Close(websocket.StatusTryAgainLater, "relay capacity full, try again")
		return
	}
	p := &pending{
		conn:   conn,
		paired: make(chan *websocket.Conn, 1),
		done:   make(chan struct{}),
	}
	s.waiting[code] = p
	pendingCount := len(s.waiting)
	s.mu.Unlock()
	log.Printf("wait  code=%s peer=%s waiting=%d", codeMask(code), peerAddr, pendingCount)

	defer close(p.done)
	defer s.removeIfStillMine(code, p)

	select {
	case other := <-p.paired:
		splice(conn, other)
	case <-time.After(pairingTimeout):
		log.Printf("expire code=%s peer=%s after=%s", codeMask(code), peerAddr, pairingTimeout)
		_ = conn.Close(websocket.StatusGoingAway, "timeout en attente d'un peer")
	case <-r.Context().Done():
		_ = conn.Close(websocket.StatusGoingAway, "client gone")
	}
}

func (s *Server) removeIfStillMine(code string, p *pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.waiting[code]; ok && cur == p {
		delete(s.waiting, code)
	}
}

// readHello attend le premier message texte de la conn : c'est le code de pairing.
func readHello(ctx context.Context, conn *websocket.Conn) (string, error) {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return "", err
	}
	if typ != websocket.MessageText {
		return "", errors.New("hello must be text")
	}
	if len(data) == 0 || len(data) > 128 {
		return "", errors.New("hello length out of range")
	}
	return string(data), nil
}

// splice copie les bytes dans les deux sens entre deux conns WebSocket.
// Quand l'un des côtés ferme, on ferme l'autre.
func splice(a, b *websocket.Conn) {
	ctx := context.Background()
	ac := websocket.NetConn(ctx, a, websocket.MessageBinary)
	bc := websocket.NetConn(ctx, b, websocket.MessageBinary)

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(ac, bc); done <- struct{}{} }()
	go func() { _, _ = io.Copy(bc, ac); done <- struct{}{} }()
	<-done
	_ = ac.Close()
	_ = bc.Close()
}

// codeMask renvoie une version partielle du code pour les logs — on garde
// les 3 premiers caractères et on masque le reste. Pas de fuite du code
// dans les logs en cas de compromission.
func codeMask(c string) string {
	if len(c) <= 3 {
		return "***"
	}
	return c[:3] + "***"
}

func remoteAddr(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	return r.RemoteAddr
}
