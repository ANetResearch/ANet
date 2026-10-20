//go:build windows

package localpeer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// D1: Windows has no socket table, so Verify answers through the challenge, exactly as on macOS: a
// listener that proves the token passes, one holding another token is ErrNotOurs, and the client
// sends the token only to the first.
func TestWindowsChallenge(t *testing.T) {
	const token = "the-control-token"
	daemon := func(tok string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			out := `{"anet":true}`
			if p, ok := Answer(r, tok); ok {
				out = `{"anet":true,"proof":"` + p + `"}`
			}
			io.WriteString(w, out)
		}
	}
	ours := newCapture(t, daemon(token))
	theirs := newCapture(t, daemon("another-token"))

	if _, err := ListenerUIDs(ours.addr()); !errors.Is(err, ErrNoSocketTable) {
		t.Fatalf("ListenerUIDs: %v, want ErrNoSocketTable", err)
	}
	for _, c := range []struct {
		addr string
		ok   bool
	}{{ours.addr(), true}, {theirs.addr(), false}} {
		conn, err := net.Dial("tcp", c.addr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := OwnerUID(conn); !errors.Is(err, ErrNoSocketTable) {
			t.Fatalf("OwnerUID: %v, want ErrNoSocketTable", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = Verify(ctx, conn, token)
		cancel()
		conn.Close()
		if c.ok && err != nil {
			t.Fatalf("Verify of the listener holding the token: %v", err)
		}
		if !c.ok && !errors.Is(err, ErrNotOurs) {
			t.Fatalf("Verify of a listener with another token: %v, want ErrNotOurs", err)
		}
	}

	// Through the client: the token reaches the listener that proves it, and not the other.
	for _, c := range []struct {
		cap *capture
		ok  bool
	}{{ours, true}, {theirs, false}} {
		req, err := http.NewRequest(http.MethodGet, "http://"+c.cap.addr()+"/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := Client(token, 5*time.Second).Do(req)
		if err == nil {
			resp.Body.Close()
		}
		if c.ok != (err == nil) {
			t.Fatalf("client to %s: %v", c.cap.addr(), err)
		}
		c.cap.mu.Lock()
		got := len(c.cap.auth)
		c.cap.mu.Unlock()
		if !c.ok && got != 0 {
			t.Fatalf("the token reached a listener that does not hold it: %v", c.cap.auth)
		}
		if c.ok && got == 0 {
			t.Fatal("the request did not reach the listener holding the token")
		}
	}
}
