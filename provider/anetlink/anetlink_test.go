package anetlink

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/wire"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/backendconn"
	"github.com/ANetResearch/ANet/provider"
)

// The client speaks C1 v1 end to end: the catalogue, the description, health, and an invoke that
// carries the effect back with its provenance.
func TestTheClientSpeaksC1v1(t *testing.T) {
	f := startFakeLinkd(t)
	p := newClient(t, f.sock)

	if err := p.Health(bg); err != nil {
		t.Fatalf("health: %v", err)
	}
	caps, err := p.Capabilities(bg)
	if err != nil || !slices.Equal(caps, []string{lampCap, "sensor.temperature@modbus/plc-1"}) {
		t.Fatalf("capabilities: %v %v", caps, err)
	}
	cid, err := p.Describe(bg)
	if err != nil || cid != "bafyexample" {
		t.Fatalf("describe: %q %v", cid, err)
	}
	eff, err := p.Invoke(bg, relayCall(lampCap, "aid-caller"))
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["power_state"] != 1 {
		t.Fatalf("effect lost on the wire: %+v", eff)
	}
	_, seen := f.seen()
	if len(seen) != 1 {
		t.Fatalf("%d invokes reached ANetLink", len(seen))
	}
	s := seen[0]
	if s.contentType != contentCBOR || s.host != "anetlink" {
		t.Errorf("Content-Type %q, Host %q: C1 v1 is CBOR with Host anetlink", s.contentType, s.host)
	}
	if s.capability != lampCap || s.callID != "ix-1" || s.args["on"] != true {
		t.Errorf("the request carried capability %q, call_id %q, args %v", s.capability, s.callID, s.args)
	}
}

// Provenance has to survive C1, or the daemon records and forwards a value with no account of how
// it was obtained. It once did not: the client read status, metrics and message and ignored the
// evidence block, and every test passed because the fake sent none either.
func TestProvenanceSurvivesC1(t *testing.T) {
	f := startFakeLinkd(t)
	eff, err := newClient(t, f.sock).Invoke(bg, relayCall(lampCap, "aid-caller"))
	if err != nil {
		t.Fatal(err)
	}
	want := effect.Evidence{
		Requested: "light.onoff=on", Protocol: "zigbee", NativeAck: true,
		ObservedState: "on", LatencyMS: 12, VerifyTrust: 3, AuthTrust: 2, Quirk: "aqara.onoff.invert",
	}
	if eff.Evidence == nil || *eff.Evidence != want {
		t.Fatalf("provenance mangled crossing C1:\n got %+v\nwant %+v", eff.Evidence, want)
	}
}

func TestRegistryIntegration(t *testing.T) {
	f := startFakeLinkd(t)
	r := provider.NewRegistry()
	if err := r.Register(bg, newClient(t, f.sock)); err != nil {
		t.Fatal(err)
	}
	p, ok := r.Resolve(lampCap)
	if !ok || p.ID() != "anetlink" {
		t.Fatal("registry cannot resolve a capability id from the wire")
	}
	eff, err := p.Invoke(bg, relayCall(lampCap, "aid-caller"))
	if err != nil || eff.Status != effect.OK {
		t.Fatalf("end to end through the registry: %+v %v", eff, err)
	}
}

// CC-2: who ANetLink is told is asking. A call from the relay carries via 1 and the caller the
// daemon verified; a local surface carries via 0 and no aid. Nothing carries key 4 (C1 v0's
// caller_aid) or Caller key 3.
func TestTheCallerIsTheVerifiedOne(t *testing.T) {
	f := startFakeLinkd(t)
	p := newClient(t, f.sock)
	if _, err := p.Invoke(bg, relayCall(lampCap, "aid-verified")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Invoke(bg, provider.Call{Capability: lampCap, Args: map[string]any{"on": true}}); err != nil {
		t.Fatal(err)
	}
	_, seen := f.seen()
	if len(seen) != 2 {
		t.Fatalf("%d invokes", len(seen))
	}
	relay, local := seen[0], seen[1]
	if relay.via != uint64(1) || relay.aid != "aid-verified" {
		t.Errorf("relay call: via %v aid %v, want 1 and the verified caller", relay.via, relay.aid)
	}
	if local.via != uint64(0) || local.aid != nil {
		t.Errorf("local call: via %v aid %v, want 0 and no aid", local.via, local.aid)
	}
	for _, s := range seen {
		if slices.Contains(s.keys, 4) || slices.Contains(s.callerKeys, 3) {
			t.Errorf("a request carried a retired or reserved key: %v / caller %v", s.keys, s.callerKeys)
		}
		if !slices.Contains(s.keys, 5) {
			t.Errorf("a request without caller (key 5): %v", s.keys)
		}
	}
	if local.callID != "" {
		t.Errorf("a call without a call id sent one: %q", local.callID)
	}
}

// CC-2, M-CC-3: a call through any other door (the voucher door, or a Via this client does not
// know) is not dialled at all: the retired via 2, or a via 0 that hides a remote caller, is never
// sent. Neither is a relay call that came without a verified caller.
func TestOtherDoorsAreNotDialled(t *testing.T) {
	f := startFakeLinkd(t)
	p := newClient(t, f.sock)
	for _, c := range []provider.Call{
		{Capability: lampCap, CallerAID: "aid-payer", Via: provider.ViaVoucher},
		{Capability: lampCap, CallerAID: "aid-x", Via: "mcp"},
		{Capability: lampCap, Via: provider.ViaRelay},
	} {
		_, err := p.Invoke(bg, c)
		var re *provider.RefusalError
		if !errors.As(err, &re) || re.Reason != "backend_failed" {
			t.Errorf("Via %q: %v, want a backend_failed refusal", c.Via, err)
		}
	}
	if conns, seen := f.seen(); conns != 0 || len(seen) != 0 {
		t.Fatalf("ANetLink was dialled %d times and sent %d invokes for calls that cannot cross C1", conns, len(seen))
	}
}

// CC-2, M-CC-1: the client never reads Call.CallerAID; the caller it sends comes from
// VerifiedCaller, which is empty at every door but the relay. Structural, over the package's
// non-test files.
func TestTheClientNeverReadsCallerAID(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	read := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		read++
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "CallerAID" {
				t.Errorf("%s reads CallerAID; the caller sent to ANetLink is Call.VerifiedCaller()", fset.Position(sel.Pos()))
			}
			return true
		})
	}
	if read == 0 {
		t.Fatal("no source file read")
	}
}

// CC-1, M-CC-2: every connection goes through backendconn. A socket whose listener is not the
// account the rules expect (here: expected_user names another uid, and this process listens) is
// sent nothing: not an invoke, not a byte, and the call ends unavailable, never sent.
func TestAnImpostorListenerIsSentNothing(t *testing.T) {
	f := startFakeLinkd(t)
	p, err := New("anetlink", f.sock, rulesFor(t, backendconn.Policy{ExpectedUser: otherAccount()}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Invoke(bg, relayCall(lampCap, "aid-caller"))
	var re *provider.RefusalError
	var be *backendconn.RefusedError
	if !errors.As(err, &re) || re.Reason != "unavailable" || !errors.As(err, &be) || be.Code != backendconn.CodePeerUIDMismatch {
		t.Fatalf("invoke through an impostor: %v; want unavailable, refused for peer_uid_mismatch", err)
	}
	if _, err := p.Capabilities(bg); !errors.As(err, &be) {
		t.Fatalf("capabilities through an impostor: %v", err)
	}
	if _, seen := f.seen(); len(seen) != 0 {
		t.Fatalf("the impostor was sent %d invokes", len(seen))
	}
	// The same socket with the default rules (the listener owns the socket file) is reached.
	if _, err := newClient(t, f.sock).Invoke(bg, relayCall(lampCap, "aid-caller")); err != nil {
		t.Fatalf("the rightful listener: %v", err)
	}
}

// A socket nobody listens on: nothing was sent, so the far side certainly did not run it, and the
// requester may send it again later (unavailable, rejected).
func TestNothingThereIsUnavailable(t *testing.T) {
	p := newClient(t, filepath.Join(t.TempDir(), "c1.sock"))
	_, err := p.Invoke(bg, relayCall(lampCap, "aid-caller"))
	var re *provider.RefusalError
	if !errors.As(err, &re) || re.Reason != "unavailable" {
		t.Fatalf("no anetlinkd: %v, want unavailable", err)
	}
	if _, isUnknown := provider.OutcomeOf(err); isUnknown && !errors.As(err, &re) {
		t.Fatal("an invoke never sent must not be an unknown outcome")
	}
}

// CC-4 and ux §1.3: each C1 refusal becomes the user's code, and the C1 code itself is not what the
// daemon carries. An answer that is not a C1 v1 refusal (wrong media type, a reason outside C1 v1,
// a reason with another status) is backend_failed.
func TestRefusalsBecomeTheUsersReasons(t *testing.T) {
	want := map[string]string{
		"not_permitted": "provider_policy", "unknown_capability": "capability_not_served", "not_found": "capability_not_served",
		"too_large": "args_too_large", "bad_request": "backend_failed", "unsupported_media": "backend_failed",
		"internal": "backend_failed", "c1_version": "backend_failed",
	}
	if len(want) != len(c1Status) {
		t.Fatalf("core §2.15.3 has %d C1 reasons, this client knows %d", len(want), len(c1Status))
	}
	f := startFakeLinkd(t)
	p := newClient(t, f.sock)
	for code, status := range c1Status {
		if a2ashape.C1Reason(code) != want[code] {
			t.Errorf("C1 %s maps to %s, ux §1.3 says %s", code, a2ashape.C1Reason(code), want[code])
		}
		f.answer(func(w http.ResponseWriter, _ *http.Request) { refuse(t, w, status, code) })
		_, err := p.Invoke(bg, relayCall(lampCap, "aid-caller"))
		var re *provider.RefusalError
		if !errors.As(err, &re) || re.Reason != want[code] {
			t.Errorf("C1 %s (%d): %v, want the user's %s", code, status, err, want[code])
			continue
		}
		if !strings.Contains(err.Error(), code) {
			t.Errorf("C1 %s: the log line %q does not name ANetLink's code", code, err)
		}
	}
	for name, h := range map[string]func(w http.ResponseWriter, r *http.Request){
		"text":           func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "forbidden", http.StatusForbidden) },
		"unknown reason": func(w http.ResponseWriter, _ *http.Request) { refuse(t, w, http.StatusForbidden, "denied") },
		"status mismatch": func(w http.ResponseWriter, _ *http.Request) {
			refuse(t, w, http.StatusInternalServerError, "not_permitted")
		},
		"refusal with 200": func(w http.ResponseWriter, _ *http.Request) { refuse(t, w, http.StatusOK, "not_permitted") },
		// A C1 v1 refusal's bytes under another media type: C1 is application/cbor both ways.
		"refusal not labelled cbor": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(enc(t, map[uint64]any{1: "not_permitted"}))
		},
	} {
		f.answer(h)
		_, err := p.Invoke(bg, relayCall(lampCap, "aid-caller"))
		var re *provider.RefusalError
		if !errors.As(err, &re) || re.Reason != "backend_failed" {
			t.Errorf("%s: %v, want backend_failed", name, err)
		}
	}
}

// A 200 answer this client cannot take (not CBOR, an unknown key, no status, a status that is not
// an effect status, a metric CoreDet cannot carry) is backend_failed: the far side ran the call, so
// the effect is not known, and nothing of the bad answer reaches the requester.
func TestAnAnswerThatIsNotC1v1IsBackendFailed(t *testing.T) {
	f := startFakeLinkd(t)
	p := newClient(t, f.sock)
	for name, body := range map[string]func() (string, []byte){
		"json": func() (string, []byte) { return "application/json", []byte(`{"status":"OK"}`) },
		// Good CBOR under another media type: C1 is application/cbor both ways, and this client does
		// not guess.
		"cbor labelled json": func() (string, []byte) { return "application/json", enc(t, map[uint64]any{1: "OK"}) },
		"unknown key":        func() (string, []byte) { return contentCBOR, enc(t, map[uint64]any{1: "OK", 5: uint64(1)}) },
		"no status":          func() (string, []byte) { return contentCBOR, enc(t, map[uint64]any{3: "done"}) },
		"bad status":         func() (string, []byte) { return contentCBOR, enc(t, map[uint64]any{1: "PAYMENT_REQUIRED"}) },
		"nan metric": func() (string, []byte) {
			// CoreDet will not encode NaN; a half-precision NaN by hand: {1: "OK", 2: {"x": NaN}}.
			return contentCBOR, []byte{0xa2, 0x01, 0x62, 'O', 'K', 0x02, 0xa1, 0x61, 'x', 0xf9, 0x7e, 0x00}
		},
	} {
		ct, b := body()
		f.answer(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write(b)
		})
		_, err := p.Invoke(bg, relayCall(lampCap, "aid-caller"))
		var re *provider.RefusalError
		if !errors.As(err, &re) || re.Reason != "backend_failed" {
			t.Errorf("%s: %v, want backend_failed", name, err)
		}
	}
}

// Arguments over C1's 1 MiB are not sent: ANetLink would answer 413 too_large, which is
// args_too_large to the requester, and nothing is gained by sending a megabyte to hear it.
func TestArgumentsOverTheLimitAreNotSent(t *testing.T) {
	f := startFakeLinkd(t)
	c := relayCall(lampCap, "aid-caller")
	c.Args = map[string]any{"blob": strings.Repeat("x", maxBody)}
	_, err := newClient(t, f.sock).Invoke(bg, c)
	var re *provider.RefusalError
	if !errors.As(err, &re) || re.Reason != "args_too_large" {
		t.Fatalf("oversize arguments: %v", err)
	}
	if conns, _ := f.seen(); conns != 0 {
		t.Fatal("oversize arguments were sent")
	}
}

// okAnswerOf is a C1 v1 invoke answer, {1: "OK", 3: message}, of exactly n bytes.
func okAnswerOf(t *testing.T, n int) []byte {
	t.Helper()
	// map(2), key 1, "OK", key 3, a text head of 5 bytes (length ≥ 65536): 11 bytes around the message.
	b := enc(t, map[uint64]any{1: "OK", 3: strings.Repeat("x", n-11)})
	if len(b) != n {
		t.Fatalf("an answer of %d bytes, not %d", len(b), n)
	}
	return b
}

// LEC §2.1.2: this client reads at most 1 MiB of an answer. One of exactly 1 MiB is taken; one a
// byte longer is backend_failed (the call ran; its answer cannot be read), and an endless one is
// not read to its end: the client stops after the limit and hangs up, so a far side that keeps
// writing costs this daemon a megabyte, not its memory.
func TestAnAnswerOverTheLimitIsNotRead(t *testing.T) {
	f := startFakeLinkd(t)
	p := newClient(t, f.sock)
	for _, c := range []struct {
		size int
		ok   bool
	}{{maxBody, true}, {maxBody + 1, false}} {
		body := okAnswerOf(t, c.size)
		f.answer(func(w http.ResponseWriter, _ *http.Request) { writeCBOR(w, http.StatusOK, body) })
		eff, err := p.Invoke(bg, relayCall(lampCap, "aid-caller"))
		var re *provider.RefusalError
		switch {
		case c.ok && (err != nil || eff.Status != effect.OK || len(eff.Message) != c.size-11):
			t.Errorf("an answer of %d bytes: %v, %v; want it taken", c.size, eff.Status, err)
		case !c.ok && (!errors.As(err, &re) || re.Reason != "backend_failed"):
			t.Errorf("an answer of %d bytes: %v; want backend_failed", c.size, err)
		}
	}

	// Without an end, in pieces, until the client hangs up or 32 MiB have gone.
	const endless = 32 << 20
	written := make(chan int, 1)
	f.answer(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentCBOR)
		w.WriteHeader(http.StatusOK)
		// A text string of 32 MiB inside the answer: well-formed for as long as it lasts.
		head := []byte{0xa2, 0x01, 0x62, 'O', 'K', 0x03, 0x7a, 0x02, 0x00, 0x00, 0x00}
		n, _ := w.Write(head)
		chunk := []byte(strings.Repeat("x", 64<<10))
		for n < endless {
			k, err := w.Write(chunk)
			n += k
			if err != nil {
				break
			}
		}
		written <- n
	})
	_, err := p.Invoke(bg, relayCall(lampCap, "aid-caller"))
	var re *provider.RefusalError
	if !errors.As(err, &re) || re.Reason != "backend_failed" {
		t.Fatalf("an endless answer: %v; want backend_failed", err)
	}
	select {
	case n := <-written:
		// What was read is at most the limit; what the far side got out besides sits in socket
		// buffers, a few hundred KiB.
		if n >= 8<<20 {
			t.Fatalf("the far side wrote %d bytes before the client hung up; the client reads at most %d", n, maxBody)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the far side is still writing: the client did not hang up")
	}
}

// CC-5: RemoteAllowed is ANetLink's remote flag per capability, 1 and nothing else allowing.
func TestRemoteAllowedIsTheRemoteFlag(t *testing.T) {
	f := startFakeLinkd(t)
	f.mu.Lock()
	f.caps = append(f.caps, map[uint64]any{1: "door.open@sim/front", 2: uint64(2)})
	f.mu.Unlock()
	got, err := newClient(t, f.sock).RemoteAllowed(bg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{lampCap: true, "sensor.temperature@modbus/plc-1": false, "door.open@sim/front": false}
	if len(got) != len(want) {
		t.Fatalf("RemoteAllowed = %v", got)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: remote allowed %v, want %v", id, got[id], w)
		}
	}
	// A catalogue entry without its remote flag is not C1 v1, and allows nothing.
	f.mu.Lock()
	f.caps = []map[uint64]any{{1: lampCap}}
	f.mu.Unlock()
	if m, err := newClient(t, f.sock).RemoteAllowed(bg); err == nil || m != nil {
		t.Fatalf("an entry with no remote flag: %v %v", m, err)
	}
}

// NotC1v1 is what the daemon's start and doctor's anetlink.socket say c1_version for: ANetLink's own
// c1_version refusal, or a far side that answers in no C1 v1 form at all (an anetlinkd of C1 v0).
// A C1 v1 refusal of another kind is not that.
func TestAFarSideOfAnotherC1VersionIsNamed(t *testing.T) {
	v0 := http.NewServeMux()
	v0.HandleFunc("GET /v0/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	sock := filepath.Join(t.TempDir(), "c1.sock")
	serveOn(t, sock, v0)
	err := newClient(t, sock).Health(bg)
	if err == nil || !NotC1v1(err) {
		t.Fatalf("a C1 v0 anetlinkd: %v, want NotC1v1", err)
	}
	if _, err := newClient(t, sock).Capabilities(bg); !NotC1v1(err) {
		t.Fatalf("capabilities from a C1 v0 anetlinkd: %v", err)
	}

	f := startFakeLinkd(t)
	f.answer(func(w http.ResponseWriter, _ *http.Request) { refuse(t, w, http.StatusUpgradeRequired, "c1_version") })
	if _, err := newClient(t, f.sock).Invoke(bg, relayCall(lampCap, "a")); !NotC1v1(err) {
		t.Fatalf("426 c1_version: %v", err)
	}
	f.answer(func(w http.ResponseWriter, _ *http.Request) { refuse(t, w, http.StatusForbidden, "not_permitted") })
	if _, err := newClient(t, f.sock).Invoke(bg, relayCall(lampCap, "a")); NotC1v1(err) {
		t.Fatalf("not_permitted is not a version mismatch: %v", err)
	}
	if NotC1v1(errors.New("dial unix: no such file")) {
		t.Fatal("a socket nobody listens on is not a version mismatch")
	}
}

// SpeaksC1v1 (doctor's anetlink.socket): a C1 v1 anetlinkd answers /v1/health 204 and every /v0
// path 426 c1_version; one that answers /v0/health any other way is not the C1 v1 this client speaks.
func TestSpeaksC1v1AsksBothVersions(t *testing.T) {
	f := startFakeLinkd(t)
	if err := newClient(t, f.sock).SpeaksC1v1(bg); err == nil || !NotC1v1(err) {
		t.Fatalf("a server that does not answer /v0/health 426: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /v0/", func(w http.ResponseWriter, _ *http.Request) { refuse(t, w, http.StatusUpgradeRequired, "c1_version") })
	sock := filepath.Join(t.TempDir(), "c1.sock")
	serveOn(t, sock, mux)
	if err := newClient(t, sock).SpeaksC1v1(bg); err != nil {
		t.Fatalf("a C1 v1 anetlinkd: %v", err)
	}
	both := http.NewServeMux()
	both.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	both.HandleFunc("GET /v0/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	sock2 := filepath.Join(t.TempDir(), "c1.sock")
	serveOn(t, sock2, both)
	if err := newClient(t, sock2).SpeaksC1v1(bg); err == nil || !NotC1v1(err) {
		t.Fatalf("a server that still answers C1 v0: %v", err)
	}

	// Any answer but 426 c1_version at /v0/health, a 200 included (LEC §5.5), and anything but 204
	// at /v1/health, is another C1 version. A C1 v1 refusal at /v1/health is not: that anetlinkd
	// speaks C1 v1 and is failing.
	for name, c := range map[string]struct {
		v1, v0  func(w http.ResponseWriter, r *http.Request)
		version bool
	}{
		"/v0/health 200": {
			v1:      func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
			v0:      func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			version: true,
		},
		"/v1/health 200": {
			v1:      func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			v0:      func(w http.ResponseWriter, _ *http.Request) { refuse(t, w, http.StatusUpgradeRequired, "c1_version") },
			version: true,
		},
		"/v1/health 500 internal": {
			v1: func(w http.ResponseWriter, _ *http.Request) { refuse(t, w, http.StatusInternalServerError, "internal") },
			v0: func(w http.ResponseWriter, _ *http.Request) { refuse(t, w, http.StatusUpgradeRequired, "c1_version") },
		},
	} {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/health", c.v1)
		mux.HandleFunc("GET /v0/health", c.v0)
		s := filepath.Join(t.TempDir(), "c1.sock")
		serveOn(t, s, mux)
		err := newClient(t, s).SpeaksC1v1(bg)
		var e *C1Error
		if !errors.As(err, &e) || NotC1v1(err) != c.version {
			t.Errorf("%s: %v (another C1 version: %v), want a C1 answer that is another version: %v", name, err, NotC1v1(err), c.version)
		}
	}
}

// The reasons this client knows are the registry's C1 namespace (core §2.15.3), with their statuses.
func TestTheClientKnowsEveryC1Reason(t *testing.T) {
	reg, err := wire.Load()
	if err != nil {
		t.Fatal(err)
	}
	codes := reg.ReasonCodes("c1")
	if len(codes) != len(c1Status) {
		t.Fatalf("the registry has C1 reasons %v, this client knows %d", codes, len(c1Status))
	}
	for _, c := range codes {
		if c1Status[c] == 0 {
			t.Errorf("C1 reason %s is not known to the client", c)
		}
	}
	want := map[string]int{"not_permitted": 403, "bad_request": 400, "too_large": 413, "unsupported_media": 415,
		"unknown_capability": 404, "c1_version": 426, "not_found": 404, "internal": 500}
	for c, st := range want {
		if c1Status[c] != st {
			t.Errorf("%s: HTTP %d, core §2.15.3 says %d", c, c1Status[c], st)
		}
	}
}

func TestDefaultSocketIsANetLinksDefault(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if s, err := DefaultSocket(); err != nil || s != "/run/user/1000/anetlink/c1.sock" {
		t.Fatalf("DefaultSocket = %q, %v", s, err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := DefaultSocket(); err == nil || !strings.Contains(err.Error(), "modules.anetlink.socket") {
		t.Fatalf("no XDG_RUNTIME_DIR: %v; the error must say which key to set", err)
	}
}

func TestNewRefusesASocketPathItCannotDial(t *testing.T) {
	for _, s := range []string{"", "relative/c1.sock", "/run/a:b/c1.sock", "/run/a b/c1.sock", "/run/../c1.sock", "/" + strings.Repeat("x", 200)} {
		if _, err := New("anetlink", s, rulesFor(t, backendconn.Policy{})); err == nil {
			t.Errorf("New accepted socket %q", s)
		}
	}
}

// serveOn serves h on a Unix socket at sock.
func serveOn(t *testing.T, sock string, h http.Handler) {
	t.Helper()
	ln, err := listenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = os.Remove(sock) })
}
