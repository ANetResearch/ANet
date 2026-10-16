package hubapi

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

// post sends one request under TraceHanded and reports what it says.
func post(t *testing.T, c *http.Client, ctx context.Context, url string) (bool, error) {
	t.Helper()
	tctx, handed := TraceHanded(ctx)
	req, err := http.NewRequestWithContext(tctx, http.MethodPost, url, strings.NewReader(`{"envelope":"AAAA"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return handed(), err
}

// quietLog keeps a test server's expected TLS errors out of the test output.
func quietLog() *log.Logger { return log.New(io.Discard, "", 0) }

// client is a client of its own, so no test reuses another's connection.
func client(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	tr := &http.Transport{DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second}
	if dial != nil {
		tr.DialContext = dial
	}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// T49, the HTTP half: before the client has a connection nothing has left
// this process; from the moment it has one, the envelope may have, whatever
// the hub answers.
func TestTraceHandedIsFalseBeforeAConnection(t *testing.T) {
	ctx := context.Background()

	t.Run("name does not resolve", func(t *testing.T) {
		r := &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("no resolver in this test")
		}}
		d := &net.Dialer{Resolver: r}
		handed, err := post(t, client(d.DialContext), ctx, "http://hub.invalid/relay/send")
		if err == nil {
			t.Fatal("a name that does not resolve was posted to")
		}
		if handed {
			t.Fatalf("a failed lookup (%v) counted as handed", err)
		}
	})

	t.Run("dial refused", func(t *testing.T) {
		handed, err := post(t, client(nil), ctx, "http://"+closedPort(t)+"/relay/send")
		if err == nil {
			t.Fatal("a closed port took the request")
		}
		if handed {
			t.Fatalf("a refused dial (%v) counted as handed", err)
		}
	})

	t.Run("tls handshake fails", func(t *testing.T) {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("the handler ran although the client does not trust the certificate")
		}))
		srv.Config.ErrorLog = quietLog()
		srv.StartTLS()
		defer srv.Close()
		handed, err := post(t, client(nil), ctx, srv.URL+"/relay/send")
		if err == nil {
			t.Fatal("an untrusted certificate was accepted")
		}
		if handed {
			t.Fatalf("a failed TLS handshake (%v) counted as handed", err)
		}
	})

	t.Run("tls peer hangs up in the handshake", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				// Read the ClientHello and drop the line.
				_, _ = c.Read(make([]byte, 512))
				c.Close()
			}
		}()
		handed, err := post(t, client(nil), ctx, "https://"+l.Addr().String()+"/relay/send")
		if err == nil {
			t.Fatal("a server that hung up in the handshake took the request")
		}
		if handed {
			t.Fatalf("a handshake cut short (%v) counted as handed", err)
		}
	})

	t.Run("cancelled before the dial", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("a cancelled request reached the hub")
		}))
		defer srv.Close()
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		handed, err := post(t, client(nil), cctx, srv.URL+"/relay/send")
		if err == nil {
			t.Fatal("a cancelled request was posted")
		}
		if handed {
			t.Fatal("a request cancelled before it had a connection counted as handed")
		}
	})
}

func TestTraceHandedIsTrueOnceConnected(t *testing.T) {
	ctx := context.Background()

	for _, code := range []int{200, 400, 401, 404, 413, 426, 429, 500, 502, 504} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		handed, err := post(t, client(nil), ctx, srv.URL+"/relay/send")
		srv.Close()
		if err != nil {
			t.Fatalf("status %d: %v", code, err)
		}
		if !handed {
			t.Errorf("status %d: an answered request counted as not handed", code)
		}
	}

	t.Run("the hub hangs up after reading", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		go func() {
			c, err := l.Accept()
			if err != nil {
				return
			}
			// The request line is in; the connection goes before any answer.
			_, _ = bufio.NewReader(c).ReadString('\n')
			c.Close()
		}()
		handed, err := post(t, client(nil), ctx, "http://"+l.Addr().String()+"/relay/send")
		if err == nil {
			t.Fatal("a connection dropped without an answer read as one")
		}
		if !handed {
			t.Fatalf("a request on a connection that then dropped (%v) counted as not handed", err)
		}
	})

	t.Run("the request times out after connecting", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			<-release
		}))
		defer srv.Close()
		defer close(release)
		cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		handed, err := post(t, client(nil), cctx, srv.URL+"/relay/send")
		if err == nil {
			t.Fatal("a request the hub never answered read as answered")
		}
		if !handed {
			t.Fatal("a request that timed out on a live connection counted as not handed")
		}
	})
}

// A trace the caller already put on the context keeps working: TraceHanded
// adds to it rather than replacing it.
func TestTraceHandedKeepsTheCallersTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	var outer bool
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { outer = true },
	})
	handed, err := post(t, client(nil), ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !handed || !outer {
		t.Fatalf("handed=%v, caller's trace ran=%v; want both", handed, outer)
	}
}
