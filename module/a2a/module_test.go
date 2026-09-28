//go:build !no_a2a

package a2a

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// testHost is a module.Host with a state directory and, optionally, a
// task seam.
type testHost struct {
	dir       string
	seam      module.TaskSeam
	untrusted bool
}

func (h *testHost) AID() string                                      { return selfAID }
func (h *testHost) Providers() *provider.Registry                    { return provider.NewRegistry() }
func (h *testHost) RecordEvidence(string, any) error                 { return nil }
func (h *testHost) ResolveKEL(string) ([]identity.SignedEvent, bool) { return nil, false }
func (h *testHost) PaymentSeam() (module.PaymentSeam, bool)          { return nil, false }
func (h *testHost) HubSeam() (module.HubSeam, bool)                  { return nil, false }
func (h *testHost) Admit(string, string, int) (func(), string)       { return func() {}, "" }
func (h *testHost) DeclareUntrustedBackend()                         { h.untrusted = true }
func (h *testHost) StateDir(string) string                           { return h.dir }
func (h *testHost) TaskSeam() (module.TaskSeam, bool)                { return h.seam, h.seam != nil }

func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("ANET_HOME", t.TempDir())
}

// Without a configuration block the module is built and serves; stopping
// the daemon's context stops it.
func TestModuleStartsWithoutConfig(t *testing.T) {
	isolateHome(t)
	m, err := module.BuildOne(name, nil)
	if err != nil || m == nil {
		t.Fatalf("BuildOne without config: %v %v", m, err)
	}
	h := &testHost{dir: t.TempDir(), seam: newFakeSeam()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx, h); err != nil {
		t.Fatal(err)
	}
	addr := m.(*Module).Addr()
	recorded, err := os.ReadFile(filepath.Join(h.dir, AddrFile))
	if err != nil || strings.TrimSpace(string(recorded)) != addr {
		t.Fatalf("a2a_addr.txt %q, listening on %s (%v)", recorded, addr, err)
	}
	tok, err := os.ReadFile(filepath.Join(h.dir, TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(h.dir, TokenFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %v", fi.Mode().Perm())
	}
	req, _ := http.NewRequest("GET", "http://"+addr+agentsPath, nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("list agents: %v %v", resp, err)
	}
	resp.Body.Close()

	cancel()
	m.(*Module).shutdown() // waits for the Serve goroutine, like the ctx watcher
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("still serving after the daemon's context ended")
	}
}

// A kernel without a task seam gets no listener, and no error: the rest of
// the daemon runs.
func TestModuleWithoutTaskSeamDoesNotListen(t *testing.T) {
	isolateHome(t)
	m, _ := New(nil)
	h := &testHost{dir: t.TempDir()}
	if err := m.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if m.(*Module).Addr() != "" {
		t.Fatal("listening without a task seam")
	}
	if _, err := os.Stat(filepath.Join(h.dir, TokenFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a token was made for an interface that does not run")
	}
}

func TestModuleNeedsAPrivateStateDir(t *testing.T) {
	m, _ := New(nil)
	if err := m.Start(context.Background(), &testHost{seam: newFakeSeam()}); err == nil {
		t.Fatal("started without a state directory")
	}
}

func TestBackendConfig(t *testing.T) {
	bad := []string{
		`{"backends":[{"allow_tcp":true,"match":"*","url":"http://127.0.0.1:9900","accept_untrusted":true}]}`,
		`{"backends":[{"allow_tcp":true,"match":"*","url":"http://example.com:9900"}]}`,
		`{"backends":[{"allow_tcp":true,"match":"","url":"http://127.0.0.1:9900"}]}`,
		`{"backends":[{"allow_tcp":true,"match":"*","url":"ftp://127.0.0.1"}]}`,
		`{"backends":[{"allow_tcp":true,"match":"*","url":"http://127.0.0.1:1"},{"allow_tcp":true,"match":"*","url":"http://127.0.0.1:2"}]}`,
		`{"backend":[]}`,
	}
	for _, raw := range bad {
		if _, err := New([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"", "null", "{}", `{"backends":[{"allow_tcp":true,"match":"*","url":"https://agent.example/a2a"}]}`} {
		if _, err := New([]byte(raw)); err != nil {
			t.Errorf("refused %q: %v", raw, err)
		}
	}

	isolateHome(t)
	m, err := New([]byte(`{"backends":[{"allow_tcp":true,"match":"*","url":"http://localhost:9900","accept_untrusted":true,"toolless":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	h := &testHost{dir: t.TempDir()}
	if err := m.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if !h.untrusted {
		t.Fatal("an accept_untrusted backend was not declared to the kernel")
	}
	m2, _ := New([]byte(`{"backends":[{"allow_tcp":true,"match":"*","url":"http://127.0.0.1:9900"}]}`))
	h2 := &testHost{dir: t.TempDir()}
	_ = m2.Start(context.Background(), h2)
	if h2.untrusted {
		t.Fatal("a trusted-only backend was declared untrusted")
	}
}

func TestAddressIsKeptAcrossRestarts(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	ln, fresh, err := listen(dir)
	if err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	first := ln.Addr().String()
	ln.Close()
	ln, fresh, err = listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ln.Addr().String() != first || fresh {
		t.Fatalf("restart moved %s to %s (fresh %v)", first, ln.Addr(), fresh)
	}
	// Taken by someone else meanwhile: refused, not moved, and the file is
	// left naming the address the clients were given (F18: portsquat_test.go).
	taken := ln
	defer taken.Close()
	if ln, _, err = listen(dir); err == nil {
		ln.Close()
		t.Fatal("moved away from the recorded address")
	}
	b, _ := os.ReadFile(filepath.Join(dir, AddrFile))
	if strings.TrimSpace(string(b)) != first {
		t.Fatalf("collision rewrote the file to %q (first %s)", b, first)
	}
}

func TestAddressRules(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, AddrFile), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("0.0.0.0:41999\n")
	if ln, _, err := listen(dir); err == nil {
		ln.Close()
		t.Fatal("bound a non-loopback address")
	}
	write("192.168.1.10:41999")
	if ln, _, err := listen(dir); err == nil {
		ln.Close()
		t.Fatal("bound a LAN address")
	}
	// A hand-picked port that is taken is an error, not a silent move —
	// as is any recorded port now (F18).
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	write(hold.Addr().String())
	if ln, _, err := listen(dir); err == nil {
		ln.Close()
		t.Fatal("moved away from an operator's address")
	}
}

// A first start skips the port another identity recorded, even when that
// identity is not running.
func TestAllocationSkipsOtherIdentities(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANET_HOME", home)
	mine := filepath.Join(home, "ids", "me", "modules", name)
	other := filepath.Join(home, "ids", "other", "modules", name)
	for _, d := range []string{mine, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The first free port in range is what a first start would take.
	probe, p, err := allocate(nil)
	if err != nil {
		t.Fatal(err)
	}
	probe.Close()
	if err := os.WriteFile(filepath.Join(other, AddrFile), []byte("127.0.0.1:"+strconv.Itoa(p)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, _, err := listen(mine)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, ps, _ := net.SplitHostPort(ln.Addr().String()); ps == strconv.Itoa(p) {
		t.Fatalf("took port %d, which another identity recorded", p)
	}
	if used := otherIdentityPorts(mine); !used[p] || len(used) != 1 {
		t.Fatalf("other identities' ports: %v", used)
	}
}

func TestTokenIsMadeOnceAndPrivate(t *testing.T) {
	dir := t.TempDir()
	a, err := loadOrCreateToken(dir)
	if err != nil || len(a) != 2*tokenBytes {
		t.Fatalf("token %q %v", a, err)
	}
	path := filepath.Join(dir, TokenFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := loadOrCreateToken(dir)
	if err != nil || b != a {
		t.Fatalf("second start: %q %v", b, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode left at %v", fi.Mode().Perm())
	}
	// A link in place of the token is not followed.
	link := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte(a), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(link, TokenFile)); err != nil {
		t.Skip("no symlinks here")
	}
	if _, err := loadOrCreateToken(link); err == nil {
		t.Fatal("read the token through a symbolic link")
	}
}
