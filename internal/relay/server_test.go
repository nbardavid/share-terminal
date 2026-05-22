package relay

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestPairAndSplice(t *testing.T) {
	srv := httptest.NewServer(NewServer())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dial := func() *websocket.Conn {
		c, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := c.Write(ctx, websocket.MessageText, []byte("tasty-orange-meteor")); err != nil {
			t.Fatalf("hello: %v", err)
		}
		return c
	}

	a := dial()
	b := dial()

	ac := websocket.NetConn(ctx, a, websocket.MessageBinary)
	bc := websocket.NetConn(ctx, b, websocket.MessageBinary)

	const payload = "hello-from-a"
	go func() {
		_, _ = ac.Write([]byte(payload))
		_ = ac.Close()
	}()

	got, err := io.ReadAll(bc)
	if err != nil {
		t.Fatalf("read from b: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func TestMaxWaitingRejects(t *testing.T) {
	s := NewServer()
	s.SetMaxWaiting(2)
	srv := httptest.NewServer(s)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Two connections with different codes: both must stay pending.
	for i, code := range []string{"code-1-aaa", "code-2-bbb"} {
		c, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		if err := c.Write(ctx, websocket.MessageText, []byte(code)); err != nil {
			t.Fatalf("hello %d: %v", i, err)
		}
		t.Cleanup(func() { _ = c.CloseNow() })
	}

	// Third connection: must be rejected (cap reached).
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial 3: %v", err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte("code-3-ccc")); err != nil {
		t.Fatalf("hello 3: %v", err)
	}
	// The server closes the conn with StatusTryAgainLater. The next Read fails.
	_, _, err = c.Read(ctx)
	if err == nil {
		t.Fatal("expected close on 3rd connection (cap reached)")
	}
}

func TestHelloRequired(t *testing.T) {
	srv := httptest.NewServer(NewServer())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Sending binary instead of text → the server must reject.
	if err := c.Write(ctx, websocket.MessageBinary, []byte("nope")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err = c.Read(ctx)
	if err == nil {
		t.Fatal("expected close after invalid hello")
	}
}
