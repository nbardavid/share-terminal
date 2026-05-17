package compress

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type pipeConn struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (c *pipeConn) Read(p []byte) (int, error)         { return c.r.Read(p) }
func (c *pipeConn) Write(p []byte) (int, error)        { return c.w.Write(p) }
func (c *pipeConn) Close() error                       { c.r.Close(); c.w.Close(); return nil }
func (c *pipeConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (c *pipeConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (c *pipeConn) SetDeadline(time.Time) error        { return nil }
func (c *pipeConn) SetReadDeadline(time.Time) error    { return nil }
func (c *pipeConn) SetWriteDeadline(time.Time) error   { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "pipe" }
func (dummyAddr) String() string  { return "pipe" }

func newPair() (net.Conn, net.Conn) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	return &pipeConn{r: r1, w: w2}, &pipeConn{r: r2, w: w1}
}

func TestRoundTrip(t *testing.T) {
	a, b := newPair()
	ca, err := Wrap(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := Wrap(b)
	if err != nil {
		t.Fatal(err)
	}

	payloads := [][]byte{
		[]byte("hello"),
		bytes.Repeat([]byte("x"), 10000),         // très compressible
		[]byte(strings.Repeat("\x1b[31mred\x1b[0m ", 200)), // sequences ANSI répétées
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, p := range payloads {
			if _, err := ca.Write(p); err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}
	}()

	for _, p := range payloads {
		got := make([]byte, len(p))
		if _, err := io.ReadFull(cb, got); err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, p) {
			t.Fatalf("mismatch payload len=%d", len(p))
		}
	}
	wg.Wait()
}

func TestActualCompressionRatio(t *testing.T) {
	// On vérifie qu'un payload répétitif sort réellement plus petit côté wire.
	var wire bytes.Buffer
	// Faux net.Conn : on n'a besoin que d'un io.Writer pour la mesure.
	z, err := Wrap(&fakeConn{w: &wire})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("hello world! "), 1000) // 13 000 octets, très compressible
	if _, err := z.Write(payload); err != nil {
		t.Fatal(err)
	}
	ratio := float64(wire.Len()) / float64(len(payload))
	t.Logf("wire=%d / payload=%d / ratio=%.3f", wire.Len(), len(payload), ratio)
	if ratio > 0.1 {
		t.Errorf("compression ratio too poor: %.3f (expected < 0.1 for highly repetitive data)", ratio)
	}
}

func TestRandomDataDoesntExplode(t *testing.T) {
	// Random data ne compresse pas, mais elle ne doit pas exploser non plus
	// (overhead deflate ≤ ~0.1% pour gros buffers).
	var wire bytes.Buffer
	z, _ := Wrap(&fakeConn{w: &wire})
	payload := make([]byte, 100_000)
	_, _ = rand.Read(payload)
	if _, err := z.Write(payload); err != nil {
		t.Fatal(err)
	}
	overhead := float64(wire.Len()-len(payload)) / float64(len(payload))
	t.Logf("payload=%d wire=%d overhead=%.3f%%", len(payload), wire.Len(), overhead*100)
	if overhead > 0.02 {
		t.Errorf("overhead too high on random data: %.3f%%", overhead*100)
	}
}

type fakeConn struct {
	w io.Writer
}

func (f *fakeConn) Read(p []byte) (int, error)         { return 0, io.EOF }
func (f *fakeConn) Write(p []byte) (int, error)        { return f.w.Write(p) }
func (f *fakeConn) Close() error                       { return nil }
func (f *fakeConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (f *fakeConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (f *fakeConn) SetDeadline(time.Time) error        { return nil }
func (f *fakeConn) SetReadDeadline(time.Time) error    { return nil }
func (f *fakeConn) SetWriteDeadline(time.Time) error   { return nil }
