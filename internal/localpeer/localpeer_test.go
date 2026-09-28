package localpeer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// hexWord prints 4 address bytes the way the kernel's table does: as one host-order word.
func hexWord(b []byte) string { return fmt.Sprintf("%08X", binary.NativeEndian.Uint32(b)) }

func TestParseHexAddr(t *testing.T) {
	v4 := hexWord([]byte{127, 0, 0, 1}) + ":9B63"
	v6 := hexWord([]byte{0, 0, 0, 0}) + hexWord([]byte{0, 0, 0, 0}) + hexWord([]byte{0, 0, 0, 0}) + hexWord([]byte{0, 0, 0, 1}) + ":0050"
	mapped := hexWord([]byte{0, 0, 0, 0}) + hexWord([]byte{0, 0, 0, 0}) + hexWord([]byte{0, 0, 0xff, 0xff}) + hexWord([]byte{127, 0, 0, 1}) + ":A2A3"
	for in, want := range map[string]string{v4: "127.0.0.1:39779", v6: "[::1]:80", mapped: "127.0.0.1:41635"} {
		got, err := parseHexAddr(in)
		if err != nil || got.String() != want {
			t.Errorf("parseHexAddr(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0100007F", "0100007F:", "0100007:9B63", "ZZ00007F:9B63", "0100007F:1FFFF"} {
		if _, err := parseHexAddr(bad); err == nil {
			t.Errorf("parseHexAddr(%q) accepted", bad)
		}
	}
}

func TestScanTable(t *testing.T) {
	lo := hexWord([]byte{127, 0, 0, 1})
	table := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: " + lo + ":9B63 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1002        0 1 1\n" +
		"   1: " + lo + ":9B63 " + lo + ":C350 01 00000000:00000000 00:00000000 00000000  1002        0 2 1\n" +
		"   2: " + lo + ":C350 " + lo + ":9B63 01 00000000:00000000 00:00000000 00000000  1001        0 3 1\n"
	got, err := scanTable(strings.NewReader(table), func(e sockEntry) bool {
		return e.local == netip.MustParseAddrPort("127.0.0.1:39779") && e.remote == netip.MustParseAddrPort("127.0.0.1:50000")
	})
	if err != nil || len(got) != 1 || got[0].uid != 1002 || got[0].state != stateEstablished {
		t.Fatalf("scanTable: %+v %v", got, err)
	}
}

func haveSocketTable(t *testing.T) {
	t.Helper()
	if _, err := ListenerUIDs("127.0.0.1:1"); err != nil {
		t.Skipf("no socket table here: %v", err)
	}
}

// capture is a listener standing in for whoever holds a port: it records every Authorization header it
// is sent, and every byte count it reads.
type capture struct {
	ln   net.Listener
	srv  *http.Server
	mu   sync.Mutex
	auth []string
}

func newCapture(t *testing.T, h http.HandlerFunc) *capture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &capture{ln: ln}
	c.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			c.mu.Lock()
			c.auth = append(c.auth, a)
			c.mu.Unlock()
		}
		if h != nil {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})}
	go c.srv.Serve(ln)
	t.Cleanup(func() { c.srv.Close() })
	return c
}

func (c *capture) addr() string { return c.ln.Addr().String() }

func (c *capture) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.auth...)
}

// A listener of this same user passes the socket-table check; the client sends its request.
func TestClientReachesAListenerOfThisUser(t *testing.T) {
	haveSocketTable(t)
	c := newCapture(t, nil)
	resp, err := Client("tok-own", 5*time.Second).Get("http://" + c.addr() + "/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if s := c.seen(); len(s) != 1 || s[0] != "Bearer tok-own" {
		// Get sends no Authorization; the request itself arriving is what counts.
		if len(s) != 0 {
			t.Fatalf("unexpected headers %v", s)
		}
	}
}

// A connection the server has not accepted yet belongs to no process, and kernels before 6.10 print uid
// 0 for it; it is judged by its listener. (Found by a red run of an unrelated test: a daemon slow to
// accept looked like root's.)
func TestVerifyBeforeTheServerAccepts(t *testing.T) {
	haveSocketTable(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0") // never accepts
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for i := 0; i < 5; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		err = Verify(context.Background(), c, "t")
		c.Close()
		if err != nil {
			t.Fatalf("a queued connection to this user's listener: %v", err)
		}
	}
	// And another user's listener is still refused while the connection waits in its queue.
	defer TreatAsForeignForTest(ln.Addr().String())()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := Verify(context.Background(), c, "t"); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("queued connection to another user's listener: %v", err)
	}
}

// F18: a listener another local user holds is refused before the request is written — the token never
// leaves this process.
func TestClientDoesNotSendTheTokenToAnotherUsersListener(t *testing.T) {
	haveSocketTable(t)
	c := newCapture(t, nil)
	defer TreatAsForeignForTest(c.addr())()
	req, _ := http.NewRequest(http.MethodPost, "http://"+c.addr()+"/status", nil)
	req.Header.Set("Authorization", "Bearer secret-control-token")
	resp, err := Client("secret-control-token", 5*time.Second).Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("request to another user's listener went through")
	}
	if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("error %v does not say the listener is not ours", err)
	}
	time.Sleep(50 * time.Millisecond)
	if s := c.seen(); len(s) != 0 {
		t.Fatalf("the listener received %v", s)
	}
	if !strings.Contains(err.Error(), "the token was not sent") {
		t.Fatalf("error %q does not tell the operator the token stayed here", err)
	}
}

// The challenge, used where there is no socket table: a daemon holding the token answers it on the very
// connection; a listener without the token, an older daemon without the proof, and a squatter relaying
// the challenge to the real daemon on another port all fail it.
func TestChallenge(t *testing.T) {
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
	real := newCapture(t, daemon(token))
	relay := newCapture(t, func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get("http://" + real.addr() + r.URL.RequestURI())
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		io.Copy(w, resp.Body)
	})
	for _, c := range []struct {
		name string
		addr string
		ok   bool
	}{
		{"daemon holding the token", real.addr(), true},
		{"listener with another token", newCapture(t, daemon("another-token")).addr(), false},
		{"daemon without the proof (or a squatter answering 200)", newCapture(t, daemon("")).addr(), false},
		{"squatter relaying to the real daemon", relay.addr(), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", c.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			far, _ := addrPort(conn.RemoteAddr())
			err = challenge(context.Background(), conn, token, far)
			if c.ok && err != nil {
				t.Fatalf("challenge: %v", err)
			}
			if !c.ok && !errors.Is(err, ErrNotOurs) {
				t.Fatalf("challenge passed or failed oddly: %v", err)
			}
			if !c.ok {
				return
			}
			// The verified connection goes on to carry the request.
			fmt.Fprintf(conn, "GET /x HTTP/1.1\r\nHost: %s\r\n\r\n", far)
			buf := make([]byte, 12)
			if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "HTTP/1.1 200" {
				t.Fatalf("the connection after the challenge: %q %v", buf, err)
			}
		})
	}
}

func TestAnswerRefusesMalformedNonces(t *testing.T) {
	for _, q := range []string{"", "zz", "00", strings.Repeat("00", 65)} {
		r := httptest.NewRequest(http.MethodGet, "/ping?proof="+q, nil)
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}))
		if _, ok := Answer(r, "t"); ok {
			t.Errorf("answered nonce %q", q)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/ping?proof="+strings.Repeat("ab", 32), nil)
	if _, ok := Answer(r, "t"); ok {
		t.Error("answered a request that arrived on no connection")
	}
}

func TestListenerUIDs(t *testing.T) {
	haveSocketTable(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	uids, err := ListenerUIDs(addr)
	if err != nil || len(uids) != 1 || uids[0] != os.Getuid() {
		t.Fatalf("ListenerUIDs(%s) = %v, %v", addr, uids, err)
	}
	restore := TreatAsForeignForTest(addr)
	if uids, _ := ListenerUIDs(addr); len(uids) != 1 || uids[0] == os.Getuid() {
		t.Fatalf("stand-in for another user: %v", uids)
	}
	restore()
	ln.Close()
	if uids, err := ListenerUIDs(addr); err != nil || len(uids) != 0 {
		t.Fatalf("after close: %v, %v", uids, err)
	}
}
