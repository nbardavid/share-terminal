// Package relay: short-code pairing server.
//
// The relay decrypts nothing and does not inspect the payload: it keeps a
// map of pending codes, and as soon as two peers present the same code it
// splices the two WebSocket connections into a bidirectional pipe until
// one side closes.
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
	// readLimit: max size of a WS message read. Must be >= the max AEAD
	// frame size written by internal/crypto (which chunks at ~64 KiB).
	// 1 MiB leaves a comfortable margin.
	readLimit = 1 << 20
	// maxWaiting: max number of sessions waiting for a peer. Beyond this,
	// new connections are rejected so an attacker can't exhaust memory by
	// opening thousands of idle connections.
	defaultMaxWaiting = 1024
)

// Server is an http.Handler that accepts WebSocket connections and pairs
// them by short code (sent as the first text message by each peer).
type Server struct {
	mu      sync.Mutex
	waiting map[string]*pending

	maxWaiting int
}

type pending struct {
	conn   *websocket.Conn
	paired chan *websocket.Conn // the second peer drops its conn here
	done   chan struct{}        // closed when the splice is done
}

func NewServer() *Server {
	return &Server{
		waiting:    make(map[string]*pending),
		maxWaiting: defaultMaxWaiting,
	}
}

// SetMaxWaiting adjusts the waiting-session limit (useful for tests or
// environments with a known load).
func (s *Server) SetMaxWaiting(n int) {
	s.mu.Lock()
	s.maxWaiting = n
	s.mu.Unlock()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // no browser CSRF concern for a CLI
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
		// Hand our conn off to the first peer, wait for the splice to end.
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
		_ = conn.Close(websocket.StatusGoingAway, "timeout waiting for a peer")
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

// readHello waits for the first text message on the conn: the pairing code.
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

// splice copies bytes in both directions between two WebSocket conns.
// When one side closes, we close the other.
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

// codeMask returns a partial version of the code for logging — keep the
// first 3 chars and mask the rest. No code leakage into the logs in case
// of a host compromise.
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
