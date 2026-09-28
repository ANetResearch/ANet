//go:build !no_service

package service_test

// The far side of a capability is checked before anything is written to it (docs/notes/0030 N1,
// internal/backendconn): a Unix socket in a directory only trusted accounts can change, whose
// listener is who it should be; TCP only when the operator allows it, and a loopback listener only
// when it is this daemon's user's.

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/internal/localpeer"
	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// seen records what a backend was sent.
type seen struct {
	mu   sync.Mutex
	reqs []string // "host path authorization caller"
}

func (s *seen) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Host+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+r.Header.Get("X-ANet-Caller"))
	s.mu.Unlock()
	io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"ok":true}`)
}

func (s *seen) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

// unixBackend serves s on a socket in a fresh directory only this user can write.
func unixBackend(t *testing.T, s *seen) (dir, sock string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no Unix sockets with owners here")
	}
	dir = t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock = filepath.Join(dir, "backend.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(s.handler)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return dir, sock
}

// refusedQuietly: a refused backend is UNAVAILABLE, and the message — which goes to the caller — says
// nothing was sent without naming this host's paths, addresses or accounts (the log has the reason).
func refusedQuietly(t *testing.T, eff effect.Effect, where string) {
	t.Helper()
	if eff.Status != effect.Unavailable || !strings.Contains(eff.Message, "nothing was sent") {
		t.Fatalf("status %s: %s", eff.Status, eff.Message)
	}
	if strings.Contains(eff.Message, where) || strings.Contains(eff.Message, "uid") {
		t.Fatalf("the caller is told local details: %s", eff.Message)
	}
}

func relayCall(capID string) provider.Call {
	return provider.Call{Capability: capID, CallerAID: "aid-caller", Via: provider.ViaRelay, CallID: "ix-1", Args: map[string]any{"a": 1}}
}

// The recommended form: the call, the token and the verified caller reach the service on its
// socket, at the request path after the colon, with Host: localhost.
func TestASocketServiceIsCalled(t *testing.T) {
	var s seen
	_, sock := unixBackend(t, &s)
	tok := writeToken(t, "socket-token-0123456789", 0o600)
	reg := start(t, `{"token_file":"`+tok+`","capabilities":[{"id":"x.do","url":"unix://`+sock+`:/v1/x/do"}]}`)
	eff := invokeCall(t, reg, relayCall("x.do"))
	if eff.Status != effect.OK {
		t.Fatalf("status %s: %s", eff.Status, eff.Message)
	}
	got := s.all()
	if len(got) != 1 || got[0] != "localhost /v1/x/do Bearer socket-token-0123456789 aid-caller" {
		t.Fatalf("the service saw %q", got)
	}
}

// A socket in a directory others can write could be anyone's: the call is not made, the token and
// arguments are not sent, and the requester is told the service is unavailable.
func TestASocketInAWritableDirectoryIsNotCalled(t *testing.T) {
	var s seen
	dir, sock := unixBackend(t, &s)
	tok := writeToken(t, "socket-token-0123456789", 0o600)
	reg := start(t, `{"token_file":"`+tok+`","capabilities":[{"id":"x.do","url":"unix://`+sock+`"}]}`)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	eff := invokeCall(t, reg, relayCall("x.do"))
	refusedQuietly(t, eff, sock)
	if got := s.all(); len(got) != 0 {
		t.Fatalf("the listener was sent %q", got)
	}
}

// SO_PEERCRED: a listener that is not the expected account gets nothing. (The test cannot listen as a
// second user; it expects a uid it does not run as.)
func TestASocketListenerThatIsNotTheExpectedUserIsNotCalled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_PEERCRED is read on Linux only")
	}
	var s seen
	_, sock := unixBackend(t, &s)
	tok := writeToken(t, "socket-token-0123456789", 0o600)
	other := strconv.Itoa(os.Getuid() + 1)
	reg := start(t, `{"token_file":"`+tok+`","expected_uid":`+other+`,"capabilities":[{"id":"x.do","url":"unix://`+sock+`"}]}`)
	eff := invokeCall(t, reg, relayCall("x.do"))
	refusedQuietly(t, eff, sock)
	if got := s.all(); len(got) != 0 {
		t.Fatalf("the listener was sent %q", got)
	}
	// Expected as what it is, it is called.
	reg = start(t, `{"token_file":"`+tok+`","expected_uid":`+strconv.Itoa(os.Getuid())+`,"capabilities":[{"id":"x.do","url":"unix://`+sock+`"}]}`)
	if eff := invokeCall(t, reg, relayCall("x.do")); eff.Status != effect.OK {
		t.Fatalf("the expected listener: %s %s", eff.Status, eff.Message)
	}
}

// A service that is down is unavailable, and the message says why.
func TestAMissingSocketIsUnavailable(t *testing.T) {
	reg := start(t, `{"capabilities":[{"id":"x.do","url":"unix://`+filepath.Join(t.TempDir(), "gone.sock")+`"}]}`)
	eff := invokeCall(t, reg, relayCall("x.do"))
	if eff.Status != effect.Unavailable || !strings.Contains(eff.Message, "is the backend running") {
		t.Fatalf("status %s: %s", eff.Status, eff.Message)
	}
}

// TCP is refused unless the operator allows it, before the node advertises anything.
func TestTCPServicesNeedAllowTCP(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:8080/x", "https://svc.example/x"} {
		_, err := module.Build(map[string][]byte{"service": []byte(`{"capabilities":[{"id":"x","url":"` + u + `"}]}`)})
		if err == nil || !strings.Contains(err.Error(), "allow_tcp") {
			t.Errorf("%s without allow_tcp: %v", u, err)
		}
	}
	for _, cfg := range []string{
		`{"expected_uid":-1,"capabilities":[{"id":"x","url":"unix:///run/x.sock"}]}`,
		`{"expected_user":"no-such-user-anet-test","capabilities":[{"id":"x","url":"unix:///run/x.sock"}]}`,
		`{"socket_group":"no-such-group-anet-test","capabilities":[{"id":"x","url":"unix:///run/x.sock"}]}`,
		`{"capabilities":[{"id":"x","url":"unix://run/x.sock"}]}`,
	} {
		if _, err := module.Build(map[string][]byte{"service": []byte(cfg)}); err == nil {
			t.Errorf("%s was accepted", cfg)
		}
	}
}

// allow_tcp: a loopback listener that another uid holds — the port taken while the service was down —
// is sent nothing, not the token, not the arguments.
func TestATCPListenerOfAnotherUserIsNotCalled(t *testing.T) {
	if _, err := localpeer.ListenerUIDs("127.0.0.1:1"); err != nil {
		t.Skipf("no socket table here: %v", err)
	}
	var s seen
	svc := httptest.NewServer(http.HandlerFunc(s.handler))
	defer svc.Close()
	tok := writeToken(t, "tcp-token-0123456789ab", 0o600)
	cfg := `{"allow_tcp":true,"token_file":"` + tok + `","capabilities":[{"id":"x.do","url":"` + svc.URL + `/do"}]}`
	if eff := invokeCall(t, start(t, cfg), relayCall("x.do")); eff.Status != effect.OK {
		t.Fatalf("this user's listener: %s %s", eff.Status, eff.Message)
	}
	defer localpeer.TreatAsForeignForTest(strings.TrimPrefix(svc.URL, "http://"))()
	eff := invokeCall(t, start(t, cfg), relayCall("x.do"))
	refusedQuietly(t, eff, strings.TrimPrefix(svc.URL, "http://"))
	if got := s.all(); len(got) != 1 {
		t.Fatalf("the listener was sent %q after it changed hands", got[1:])
	}
}
