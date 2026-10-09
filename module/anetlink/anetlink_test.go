//go:build !no_anetlink

package anetlink_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/anetlink"
	"github.com/ANetResearch/ANet/module/moduletest"
	"github.com/ANetResearch/ANet/provider"
)

// The factory is the only thing between an operator's typo and a daemon
// that comes up without the capabilities they asked for.
func TestTheFactoryRefusesConfigurationsThatCannotWork(t *testing.T) {
	build := func(raw string) (module.Module, error) {
		return module.BuildOne("anetlink", []byte(raw))
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if m, err := build(""); err != nil || m != nil {
		t.Errorf("compiled in and not configured must be a quiet no-op, got (%v, %v)", m, err)
	}
	if _, err := build(`{`); err == nil {
		t.Error("unparseable config was accepted")
	}
	for raw, want := range map[string]string{
		`{"allow_tcp": true}`:                         "allow_tcp",
		`{"expected_user": "no-such-user-anet-test"}`: "modules.anetlink.expected_user",
		`{"socket_group": "no-such-group-anet-test"}`: "modules.anetlink.socket_group",
		`{"peer_check": "loose"}`:                     "modules.anetlink.peer_check",
		`{"socket": "relative/c1.sock"}`:              "socket",
		`{"socket": "/run/a:b/c1.sock"}`:              "socket",
	} {
		if _, err := build(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want a refusal naming %s", raw, err, want)
		}
	}
	m, err := build(`{"socket":"/tmp/linkd.sock"}`)
	if err != nil || m == nil {
		t.Fatalf("a valid config was refused: %v", err)
	}
	if m.Name() != "anetlink" {
		t.Errorf("name = %q — it must match the build tag, or no_<name> switches off nothing",
			m.Name())
	}
}

// CC-6: `"anetlink": {}` is a complete configuration where XDG_RUNTIME_DIR
// gives the socket its default, anetlinkd's own; without XDG_RUNTIME_DIR
// there is no default, and the refusal says which key to set.
func TestTheSocketDefaultsToANetLinks(t *testing.T) {
	rd := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", rd)
	f := serveC1(t, filepath.Join(rd, "anetlink", "c1.sock"))
	m, err := module.BuildOne("anetlink", []byte(`{}`))
	if err != nil {
		t.Fatalf("{} with XDG_RUNTIME_DIR set: %v", err)
	}
	reg := provider.NewRegistry()
	if err := m.Start(context.Background(), host{reg: reg}); err != nil {
		t.Fatalf("start on the default socket: %v", err)
	}
	if _, ok := reg.Resolve("light.onoff@sim/lamp-1"); !ok || f.hits.Load() == 0 {
		t.Fatal("the default socket was not the one dialled")
	}

	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := module.BuildOne("anetlink", []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "modules.anetlink.socket") {
		t.Fatalf("{} without XDG_RUNTIME_DIR: %v", err)
	}
}

// CC-7: the registration handshake has a fixed bound, and the block has no
// timeout_s (a removed key: the daemon refuses it, config_key_removed).
func TestTheHandshakeTimeoutIsNotASetting(t *testing.T) {
	typ := reflect.TypeOf(anetlink.Config{})
	for i := range typ.NumField() {
		if tag := typ.Field(i).Tag.Get("json"); strings.HasPrefix(tag, "timeout") || strings.HasPrefix(tag, "expected_uid") {
			t.Errorf("Config has a %q field", tag)
		}
	}
	if anetlink.RegisterTimeout != 10e9 {
		t.Errorf("the handshake bound is %v, want 10s", anetlink.RegisterTimeout)
	}
}

// CC-1 at start: a socket whose listener is not the expected account
// registers nothing, and the error says who listens.
func TestStartRefusesAnImpostorListener(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "c1.sock")
	f := serveC1(t, sock)
	m, err := module.BuildOne("anetlink", []byte(`{"socket":"`+sock+`","expected_user":"`+strconv.Itoa(os.Getuid()+1)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	err = m.Start(context.Background(), host{reg: provider.NewRegistry()})
	if err == nil || !strings.Contains(err.Error(), "uid") {
		t.Fatalf("start through an impostor: %v", err)
	}
	if f.hits.Load() != 0 {
		t.Fatalf("the impostor was asked %d times", f.hits.Load())
	}
}

// An anetlinkd of another C1 version is named as that at start: both
// programs need the same release (ux §1.4 J, c1_version).
func TestStartNamesAnotherC1Version(t *testing.T) {
	t.Setenv("LANG", "C")
	sock := filepath.Join(t.TempDir(), "c1.sock")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"capabilities":["light.onoff@sim/lamp-1"]}`))
	})
	serve(t, sock, mux)
	m, err := module.BuildOne("anetlink", []byte(`{"socket":"`+sock+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	err = m.Start(context.Background(), host{reg: provider.NewRegistry()})
	if err == nil || !strings.Contains(err.Error(), "upgrade both to the same release") {
		t.Fatalf("start against a C1 v0 anetlinkd: %v", err)
	}
}

// C1's red line, enforced by the compiler rather than by review.
//
// This module exists to offer whatever hardware ANetLink reaches, and the
// thing it must never do is hand the daemon a notion of a device. If it
// ever grew a method returning devices, or a Host that asked for one, the
// daemon would start knowing about them — and knowing is how the org
// feature reached thirty-five daemon files. A reflection test is blunt,
// and blunt is right: the failure it guards against is somebody adding a
// helpful-looking method.
func TestTheModuleNeverHandsTheDaemonADevice(t *testing.T) {
	typ := reflect.TypeOf(&anetlink.Module{})
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		if strings.Contains(strings.ToLower(m.Name), "device") {
			t.Errorf("method %s: the daemon must never learn what a device is", m.Name)
		}
		for j := 0; j < m.Type.NumOut(); j++ {
			if strings.Contains(strings.ToLower(m.Type.Out(j).String()), "device") {
				t.Errorf("method %s returns %s — C1 says capabilities, not devices",
					m.Name, m.Type.Out(j))
			}
		}
	}
	// And what it does implement is exactly the module contract, nothing
	// wider. A module that satisfied Payer, say, would be a device layer
	// that could also spend money.
	var _ module.Module = (*anetlink.Module)(nil)
	if _, ok := any((*anetlink.Module)(nil)).(module.Payer); ok {
		t.Error("the device module implements Payer — it has no business with money")
	}
	if _, ok := any((*anetlink.Module)(nil)).(module.Confidential); ok {
		t.Error("the device module declares confidential tokens it does not have")
	}
}

// Stopping a module that never started must not panic. It is the ordinary
// path when the daemon fails to come up.
func TestStoppingAModuleThatNeverStartedIsSafe(t *testing.T) {
	m, err := module.BuildOne("anetlink", []byte(`{"socket":"/tmp/nope.sock"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Errorf("Stop before Start: %v", err)
	}
}

type host struct {
	moduletest.NopHost
	reg *provider.Registry
}

func (h host) Providers() *provider.Registry { return h.reg }

type c1Server struct{ hits atomic.Int32 }

// serveC1 is a minimal C1 v1 anetlinkd on sock: one capability.
func serveC1(t *testing.T, sock string) *c1Server {
	t.Helper()
	s := &c1Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		s.hits.Add(1)
		b, err := coredet.Marshal(map[uint64]any{1: []any{map[uint64]any{1: "light.onoff@sim/lamp-1", 2: uint64(1)}}})
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/cbor")
		_, _ = w.Write(b)
	})
	serve(t, sock, mux)
	return s
}

func serve(t *testing.T, sock string, h http.Handler) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}
