package crypto

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// pipeConn wraps two io.Pipes to simulate a net.Conn in memory. Used to
// validate the handshake and encryption without real networking.
type pipeConn struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (c *pipeConn) Read(p []byte) (int, error)         { return c.r.Read(p) }
func (c *pipeConn) Write(p []byte) (int, error)        { return c.w.Write(p) }
func (c *pipeConn) Close() error                       { c.r.Close(); c.w.Close(); return nil }
func (c *pipeConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (c *pipeConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (c *pipeConn) SetDeadline(_ time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(_ time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "pipe" }
func (dummyAddr) String() string  { return "pipe" }

func newPipePair() (a, b net.Conn) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	a = &pipeConn{r: r1, w: w2}
	b = &pipeConn{r: r2, w: w1}
	return a, b
}

func TestHandshakeAndRoundTrip(t *testing.T) {
	a, b := newPipePair()
	code := []byte("meteor-cobalt-jungle")

	var wg sync.WaitGroup
	var hostConn, clientConn net.Conn
	var hostFP, clientFP string
	var hostErr, clientErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		hostConn, hostFP, hostErr = Wrap(context.Background(), a, code, RoleHost)
	}()
	go func() {
		defer wg.Done()
		clientConn, clientFP, clientErr = Wrap(context.Background(), b, code, RoleClient)
	}()
	wg.Wait()

	if hostErr != nil {
		t.Fatalf("host wrap: %v", hostErr)
	}
	if clientErr != nil {
		t.Fatalf("client wrap: %v", clientErr)
	}
	if hostFP != clientFP {
		t.Fatalf("fingerprint mismatch: host=%s client=%s", hostFP, clientFP)
	}

	// Round-trip both ways.
	payload := make([]byte, 200000) // > maxFrame to exercise chunking
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 2)
	go func() {
		_, err := hostConn.Write(payload)
		done <- err
	}()

	got := make([]byte, len(payload))
	go func() {
		_, err := io.ReadFull(clientConn, got)
		done <- err
	}()

	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("transfer error: %v", err)
		}
	}
	if !bytes.Equal(payload, got) {
		t.Fatalf("payload mismatch")
	}
}

func TestWrongCodeFails(t *testing.T) {
	a, b := newPipePair()

	var hostErr, clientErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, hostErr = Wrap(context.Background(), a, []byte("right-code-here"), RoleHost)
	}()
	go func() {
		defer wg.Done()
		_, _, clientErr = Wrap(context.Background(), b, []byte("wrong-code-here"), RoleClient)
	}()
	wg.Wait()

	// The PAKE handshake must fail or produce different keys; either way,
	// at least one of the two sides must surface an error during Wrap or
	// key derivation, OR the round-trip that follows fails.
	// schollz/pake/v3 detects the mismatch and returns an error in Update.
	if hostErr == nil && clientErr == nil {
		t.Fatal("expected at least one side to fail with mismatched codes")
	}
}
