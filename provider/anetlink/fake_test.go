package anetlink

// fake_test.go is a C1 v1 server for the tests: it speaks the documented wire (CBOR, integer keys,
// /v1/*) from generic maps, never through this package's structs or decoders, so that a test of the
// client is a test against the contract and not against the client's own idea of it. (Importing
// ANetLink's server is not an option: apps do not import apps.)

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"

	"github.com/ANetResearch/ANet/internal/backendconn"
	"github.com/ANetResearch/ANet/provider"
)

const lampCap = "light.onoff@sim/lamp-1"

// seenInvoke is what the fake was sent on POST /v1/invoke, read from the CBOR map by key number.
type seenInvoke struct {
	contentType, host string
	keys              []uint64 // the request's top-level keys
	capability        string
	args              map[any]any
	callID            string
	via               any // nil when absent
	aid               any // nil when absent
	callerKeys        []uint64
}

// fakeLinkd is the server's state: what it serves and what it was sent.
type fakeLinkd struct {
	sock string

	mu      sync.Mutex
	conns   int          // connections accepted
	invokes []seenInvoke // POST /v1/invoke bodies that decoded
	// invoke, when set, answers POST /v1/invoke instead of the default (an OK effect with
	// provenance).
	invoke func(w http.ResponseWriter, r *http.Request)
	caps   []map[uint64]any
}

func enc(t testing.TB, v any) []byte {
	t.Helper()
	b, err := coredet.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeCBOR(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", contentCBOR)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// refuse answers a C1 refusal with the given reason and status.
func refuse(t testing.TB, w http.ResponseWriter, status int, reason string) {
	writeCBOR(w, status, enc(t, map[uint64]any{1: reason}))
}

// startFakeLinkd serves C1 v1 on a socket in a private temporary directory.
func startFakeLinkd(t *testing.T) *fakeLinkd {
	t.Helper()
	f := &fakeLinkd{
		sock: filepath.Join(t.TempDir(), "c1.sock"),
		caps: []map[uint64]any{{1: lampCap, 2: uint64(1)}, {1: "sensor.temperature@modbus/plc-1", 2: uint64(0)}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		caps := make([]any, len(f.caps))
		for i, c := range f.caps {
			caps[i] = c
		}
		f.mu.Unlock()
		writeCBOR(w, http.StatusOK, enc(t, map[uint64]any{1: caps}))
	})
	mux.HandleFunc("GET /v1/describe", func(w http.ResponseWriter, _ *http.Request) {
		writeCBOR(w, http.StatusOK, enc(t, map[uint64]any{1: "bafyexample"}))
	})
	mux.HandleFunc("POST /v1/invoke", func(w http.ResponseWriter, r *http.Request) {
		var m map[uint64]any
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			refuse(t, w, http.StatusRequestEntityTooLarge, "too_large")
			return
		}
		if err := coredet.Unmarshal(raw, &m); err != nil {
			refuse(t, w, http.StatusBadRequest, "bad_request")
			return
		}
		s := seenInvoke{contentType: r.Header.Get("Content-Type"), host: r.Host}
		for k := range m {
			s.keys = append(s.keys, k)
		}
		s.capability, _ = m[1].(string)
		s.args, _ = m[2].(map[any]any)
		s.callID, _ = m[3].(string)
		if c, ok := m[5].(map[any]any); ok {
			s.via, s.aid = c[uint64(1)], c[uint64(2)]
			for k := range c {
				if n, ok := k.(uint64); ok {
					s.callerKeys = append(s.callerKeys, n)
				}
			}
		}
		f.mu.Lock()
		f.invokes = append(f.invokes, s)
		answer := f.invoke
		f.mu.Unlock()
		if answer != nil {
			answer(w, r)
			return
		}
		if s.capability != lampCap {
			refuse(t, w, http.StatusNotFound, "unknown_capability")
			return
		}
		// The documented answer carries provenance: a fake that serves less than the contract
		// tests less than the contract.
		writeCBOR(w, http.StatusOK, enc(t, map[uint64]any{
			1: "OK", 2: map[string]float64{"power_state": 1},
			4: map[uint64]any{1: "light.onoff=on", 2: "zigbee", 3: true, 4: "on", 5: uint64(12), 6: uint64(3), 7: uint64(2),
				8: "aqara.onoff.invert"},
		}))
	})
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ConnState: func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			f.mu.Lock()
			f.conns++
			f.mu.Unlock()
		}
	}}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeLinkd) seen() (conns int, invokes []seenInvoke) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns, append([]seenInvoke(nil), f.invokes...)
}

func (f *fakeLinkd) answer(h func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	f.invoke = h
	f.mu.Unlock()
}

// rulesFor is the default policy: the socket file's owner must be the one listening.
func rulesFor(t testing.TB, p backendconn.Policy) backendconn.Rules {
	t.Helper()
	r, err := p.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// newClient is a provider for the fake's socket with the default policy.
func newClient(t testing.TB, sock string) *Provider {
	t.Helper()
	p, err := New("anetlink", sock, rulesFor(t, backendconn.Policy{}))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// otherAccount is a uid that is not this process's: a listener expected to run as it is an
// impostor when this process is the one listening.
func otherAccount() string { return strconv.Itoa(os.Getuid() + 1) }

func relayCall(capability, aid string) provider.Call {
	return provider.Call{Capability: capability, Args: map[string]any{"on": true}, CallID: "ix-1", CallerAID: aid, Via: provider.ViaRelay}
}

var bg = context.Background()

func listenUnix(sock string) (net.Listener, error) { return net.Listen("unix", sock) }
