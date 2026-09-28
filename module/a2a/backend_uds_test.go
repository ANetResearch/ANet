//go:build !no_a2a

package a2a

// A backend is checked before a task's text or the token reaches it (docs/notes/0030 N1,
// internal/backendconn): a socket whose path only trusted accounts can change and whose listener is
// who it should be; TCP only with allow_tcp, and a loopback listener only when it is this daemon's
// user's.

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/localpeer"
)

// socketBackend is a test backend served on a Unix socket in a private directory, its card naming its
// interface iface; hits counts every request the socket receives, the card included.
func socketBackend(t *testing.T, iface func(sock string) string) (b *testBackend, sock string, hits *atomic.Int32) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no Unix sockets with owners here")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock = filepath.Join(dir, "a2a.sock")
	b = newTestBackendAt(t, func(string) string { return iface(sock) })
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	hits = new(atomic.Int32)
	h := b.srv.Config.Handler
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return b, sock, hits
}

func noReply(t *testing.T, h *inboundHost) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.refusedOnce() {
		t.Fatalf("forwarded: replies %+v evidence %+v", h.replies, h.evidence)
	}
}

// The recommended form: the card and every call go to the socket; the card's interface may name any
// http host (a label: the socket is dialled) or the socket itself.
func TestASocketBackendAnswers(t *testing.T) {
	for name, iface := range map[string]func(string) string{
		"http interface": func(string) string { return "http://localhost" },
		"unix interface": func(sock string) string { return "unix://" + sock + ":" },
	} {
		t.Run(name, func(t *testing.T) {
			b, sock, _ := socketBackend(t, iface)
			h := newInboundHost(t)
			startBackendModule(t, h, `{"backends":[{"match":"*","url":"unix://`+sock+`","token_file":"`+tokenFile(t)+`"}]}`)
			h.tasks <- inboundTask("ix1", "bafypeer", true, "hello")
			if r := h.wait(t); r.state != a2ashape.TaskStateCompleted || r.msg.Parts[0].Text != "backend: hello" {
				t.Fatalf("reply %+v", r)
			}
			if b.n() != 1 {
				t.Fatalf("%d calls", b.n())
			}
		})
	}
}

// A socket backend's card that points at another socket is not followed.
func TestASocketBackendsCardCannotPointElsewhere(t *testing.T) {
	_, other, otherHits := socketBackend(t, func(string) string { return "http://localhost" })
	b, sock, _ := socketBackend(t, func(string) string { return "unix://" + other + ":" })
	h := newInboundHost(t)
	startBackendModule(t, h, `{"backends":[{"match":"*","url":"unix://`+sock+`","token_file":"`+tokenFile(t)+`"}]}`)
	h.tasks <- inboundTask("ix1", "bafypeer", true, "hello")
	noReply(t, h)
	if b.n() != 0 || otherHits.Load() != 0 {
		t.Fatalf("calls %d, the other socket's requests %d", b.n(), otherHits.Load())
	}
}

// SO_PEERCRED: a listener that is not the expected account is sent nothing, not even the card
// request that carries the token. (The test cannot listen as a second user; it expects a uid it
// does not run as.)
func TestASocketBackendOfTheWrongUserGetsNothing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_PEERCRED is read on Linux only")
	}
	_, sock, hits := socketBackend(t, func(string) string { return "http://localhost" })
	h := newInboundHost(t)
	other := strconv.Itoa(os.Getuid() + 1)
	startBackendModule(t, h, `{"backends":[{"match":"*","url":"unix://`+sock+`","expected_uid":`+other+`,"token_file":"`+tokenFile(t)+`"}]}`)
	h.tasks <- inboundTask("ix1", "bafypeer", true, "hello")
	noReply(t, h)
	if n := hits.Load(); n != 0 {
		t.Fatalf("the listener received %d requests", n)
	}
}

// A socket in a directory every user can write is refused.
func TestASocketBackendInAWritableDirectoryGetsNothing(t *testing.T) {
	_, sock, hits := socketBackend(t, func(string) string { return "http://localhost" })
	if err := os.Chmod(filepath.Dir(sock), 0o777); err != nil {
		t.Fatal(err)
	}
	h := newInboundHost(t)
	startBackendModule(t, h, `{"backends":[{"match":"*","url":"unix://`+sock+`","token_file":"`+tokenFile(t)+`"}]}`)
	h.tasks <- inboundTask("ix1", "bafypeer", true, "hello")
	noReply(t, h)
	if n := hits.Load(); n != 0 {
		t.Fatalf("the listener received %d requests", n)
	}
}

// TCP needs allow_tcp.
func TestATCPBackendNeedsAllowTCP(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:9900", "https://agent.example/a2a"} {
		_, err := New([]byte(`{"backends":[{"match":"*","url":"` + u + `"}]}`))
		if err == nil || !strings.Contains(err.Error(), "allow_tcp") {
			t.Errorf("%s without allow_tcp: %v", u, err)
		}
	}
	if _, err := New([]byte(`{"backends":[{"match":"*","url":"unix:///run/agent/a2a.sock"}]}`)); err != nil {
		t.Errorf("a socket: %v", err)
	}
	if _, err := New([]byte(`{"backends":[{"match":"*","url":"unix:///run/agent/a2a.sock","expected_user":"no-such-user-anet-test"}]}`)); err == nil {
		t.Error("an expected_user that does not exist was accepted")
	}
}

// allow_tcp (Hermes' 127.0.0.1:9900): a loopback port another local user holds — taken while the
// backend was down — is sent nothing.
func TestATCPBackendHeldByAnotherUserGetsNothing(t *testing.T) {
	if _, err := localpeer.ListenerUIDs("127.0.0.1:1"); err != nil {
		t.Skipf("no socket table here: %v", err)
	}
	b := newTestBackend(t)
	// The backend's handler on a listener of its own that counts every request, the card's included.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	h0 := b.srv.Config.Handler
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h0.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	defer localpeer.TreatAsForeignForTest(ln.Addr().String())()
	h := newInboundHost(t)
	startBackendModule(t, h, `{"backends":[{"allow_tcp":true,"match":"*","url":"http://`+ln.Addr().String()+`","token_file":"`+tokenFile(t)+`"}]}`)
	h.tasks <- inboundTask("ix1", "bafypeer", true, "hello")
	noReply(t, h)
	if n := hits.Load(); n != 0 || b.n() != 0 {
		t.Fatalf("the listener received %d requests, the backend %d calls", n, b.n())
	}
}
