package loopguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoopbackName(t *testing.T) {
	for _, c := range []struct {
		host string
		ok   bool
	}{
		{"127.0.0.1", true}, {"localhost", true}, {"LocalHost", true}, {"::1", true}, {"[::1]", true},
		{"127.0.0.2", false}, {"0.0.0.0", false}, {"::", false}, {"", false}, {"localhost.", false},
		{"localtest.me", false}, {"127.0.0.1.nip.io", false}, {"::1%lo", false}, {"[::1]:80", false},
	} {
		if got := LoopbackName(c.host); got != c.ok {
			t.Errorf("LoopbackName(%q) = %v, want %v", c.host, got, c.ok)
		}
	}
}

// The three loopback names with the listener's port pass; everything else —
// another name, another port, a name without a port on a non-80 listener —
// is what a surface answers 421.
func TestAllowedHost(t *testing.T) {
	const port = "43811"
	for _, c := range []struct {
		host, port string
		ok         bool
	}{
		{"127.0.0.1:43811", port, true},
		{"localhost:43811", port, true},
		{"LOCALHOST:43811", port, true},
		{"[::1]:43811", port, true},
		{"127.0.0.1:43812", port, false},
		{"localhost:1", port, false},
		{"[::1]:39811", port, false},
		{"127.0.0.1", port, false},
		{"localhost", "80", true},
		{"localhost:80", "80", true},
		{"127.0.0.2:43811", port, false},
		{"0.0.0.0:43811", port, false},
		{"evil.example:43811", port, false},
		{"localtest.me:43811", port, false},
		{"::1:43811", port, false}, // unbracketed IPv6 is not host:port
		{"[::]:43811", port, false},
		{"", port, false},
		{"127.0.0.1:43811", "", false}, // no known port matches nothing
		{"127.0.0.1:", "", false},
	} {
		if got := AllowedHost(c.host, c.port); got != c.ok {
			t.Errorf("AllowedHost(%q, %q) = %v, want %v", c.host, c.port, got, c.ok)
		}
	}
}

func TestAllowedOrigin(t *testing.T) {
	const port = "39811"
	for _, c := range []struct {
		origin string
		ok     bool
	}{
		{"http://127.0.0.1:39811", true},
		{"http://localhost:39811", true},
		{"http://[::1]:39811", true},
		{"http://127.0.0.1:39811/", true},
		{"https://127.0.0.1:39811", false},
		{"http://127.0.0.1:39812", false},
		{"http://127.0.0.1", false},
		{"http://evil.example:39811", false},
		{"http://user@127.0.0.1:39811", false},
		{"http://127.0.0.1:39811/console", false},
		{"http://127.0.0.1:39811?x=1", false},
		{"null", false},
		{"", false},
		{"file://", false},
	} {
		if got := AllowedOrigin(c.origin, port); got != c.ok {
			t.Errorf("AllowedOrigin(%q) = %v, want %v", c.origin, got, c.ok)
		}
	}
}

func TestCheckLoopbackAddr(t *testing.T) {
	for _, c := range []struct {
		addr        string
		ok          bool
		notLoopback bool
	}{
		{"127.0.0.1:39811", true, false},
		{"localhost:39811", true, false},
		{"[::1]:39811", true, false},
		{"127.0.0.1:0", true, false}, // the port is the caller's rule
		{"0.0.0.0:39811", false, true},
		{":39811", false, true},
		{"192.168.1.5:39811", false, true},
		{"[::]:39811", false, true},
		{"example.org:39811", false, true},
		{"127.0.0.2:39811", false, true},
		{"127.0.0.1", false, false},
		{"", false, false},
	} {
		err := CheckLoopbackAddr(c.addr)
		if c.ok != (err == nil) {
			t.Errorf("CheckLoopbackAddr(%q) = %v", c.addr, err)
		}
		if got := errors.Is(err, ErrNotLoopback); got != c.notLoopback {
			t.Errorf("CheckLoopbackAddr(%q): errors.Is(ErrNotLoopback) = %v (%v)", c.addr, got, err)
		}
	}
}

// ListenerPort names the port the request actually arrived on, which is
// what the Host must carry; the configured address is only the fallback
// for a request that never crossed a listener.
func TestListenerPort(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = ListenerPort(r, "127.0.0.1:1")
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	_, want, _ := net.SplitHostPort(srv.Listener.Addr().String())
	if got != want {
		t.Fatalf("ListenerPort over a connection = %q, want %q", got, want)
	}

	r := httptest.NewRequest("GET", "/", nil).WithContext(context.Background())
	if p := ListenerPort(r, "127.0.0.1:39811"); p != "39811" {
		t.Fatalf("fallback port = %q", p)
	}
	if p := ListenerPort(r, "not-an-address"); p != "" {
		t.Fatalf("fallback of a bad address = %q, want empty", p)
	}
	if AllowedHost("127.0.0.1:39811", ListenerPort(r, "")) {
		t.Fatal("a request with no known port was allowed")
	}
}
