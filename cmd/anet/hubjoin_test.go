package main

// `anet hub-register` and `anet hub-leave` against a fake control plane
// (LEC CL-4, AD-8, AD-9; ux UX-C3, UX-C4, UX-T11).

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

const (
	testHub = "https://hub.example"
	testAID = "bafyreiujzdegxdncf32epf3dhodzdocis2jhtlgmxgedn73u557hq2abcd"
)

// hubControl is a control plane that answers /hub-register and /hub-leave
// as scripted, and records the bodies it was sent.
type hubControl struct {
	mu     sync.Mutex
	bodies []map[string]any
	// dry and join are the answers to dry_run and to the join.
	dryStatus, joinStatus int
	dry, join             any
	leaveStatus           int
	leave                 any
}

func (hc *hubControl) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	hc.mu.Lock()
	hc.bodies = append(hc.bodies, body)
	status, ans := hc.joinStatus, hc.join
	switch {
	case r.URL.Path == "/hub-leave":
		status, ans = hc.leaveStatus, hc.leave
	case body["dry_run"] == true:
		status, ans = hc.dryStatus, hc.dry
	}
	hc.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ans)
}

func (hc *hubControl) requests() []map[string]any {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	return append([]map[string]any(nil), hc.bodies...)
}

// serve starts hc on a loopback port and returns a client for it; with h,
// h's data directory points the CLI at it too.
func (hc *hubControl) serve(t *testing.T, h *cliHome) *client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(hc.handler)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	if h != nil {
		cfg := `{"control_addr": "` + ln.Addr().String() + `"}`
		if err := os.WriteFile(filepath.Join(h.data, "config.json"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.data, "control_token.txt"), []byte("test-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &client{base: "http://" + ln.Addr().String(), token: "test-token", timeout: 5 * time.Second}
}

// joinAnswer is a /hub-register answer of a state, in English.
func joinAnswer(state, reason string, args a2ashape.Args, recovery bool) map[string]any {
	m := map[string]any{"hub": testHub, "aid": testAID, "state": state, "reason": reason,
		"message": a2ashape.Sentence(reason, a2ashape.English, args)}
	if n := a2ashape.Next(reason, a2ashape.English, args); n != "" {
		m["next"] = n
	}
	if recovery {
		m["recovery"] = map[string]any{"state": "unprotected",
			"message": a2ashape.Sentence("identity.unprotected", a2ashape.English, nil), "next": "anet identity protect"}
	}
	return m
}

func dryJoin() map[string]any {
	return map[string]any{"hub": testHub, "hub_aid": "bafyreihub", "action": "join", "recovery_needed": true}
}

// testHubCLI writes to buffers, without a terminal and with env.
func testHubCLI(env map[string]string) (hubCLI, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return hubCLI{stdout: &out, stderr: &errb, getenv: func(k string) string { return env[k] },
		terminal: func() bool { return false }, lang: a2ashape.English}, &out, &errb
}

// CL-4, AD-9: without a terminal, hub-register asks what it would
// do, joins with next_digest and unprotected, and prints the recovery
// reminder first and the admit line last (ux §3.4 (g) text G), exit 0.
func TestHubRegisterWithoutATerminal(t *testing.T) {
	pending := joinAnswer(daemon.JoinPendingAdmission, "hub_pending_admission", a2ashape.Args{"hub": testHub, "aid": testAID}, true)
	hc := &hubControl{dryStatus: 200, dry: dryJoin(), joinStatus: 200, join: pending}
	c := hc.serve(t, nil)
	h, out, errb := testHubCLI(nil)
	if err := h.hubRegister(c, []string{testHub, "--name", "coder", "--caps", "a,b"}); err != nil {
		t.Fatalf("exit: %v", err)
	}
	want := a2ashape.Sentence("identity.unprotected", a2ashape.English, nil) + "\n" +
		"  → anet identity protect\n" +
		a2ashape.Sentence("hub_pending_admission", a2ashape.English, a2ashape.Args{"hub": testHub, "aid": testAID}) + "\n" +
		"  → anet-hub admit " + testAID + "\n"
	if out.String() != want || errb.Len() != 0 {
		t.Fatalf("stdout:\n%s\nwant:\n%s\nstderr: %s", out, want, errb)
	}
	reqs := hc.requests()
	if len(reqs) != 2 || reqs[0]["dry_run"] != true || reqs[0]["hub"] != testHub {
		t.Fatalf("requests %v", reqs)
	}
	j := reqs[1]
	digest, err := base64.StdEncoding.DecodeString(j["next_digest"].(string))
	if err != nil || len(digest) != 32 || j["unprotected"] != true || j["name"] != "coder" || j["dry_run"] != nil {
		t.Fatalf("join request %v", j)
	}
	if caps, _ := j["caps"].([]any); len(caps) != 2 {
		t.Fatalf("caps %v", j["caps"])
	}
}

// UX-O1: an address the daemon cannot reach is said at once, on stderr,
// with nothing asked and nothing joined (exit 1).
func TestHubRegisterUnreachable(t *testing.T) {
	ref := a2ashape.Refuse("hub_unreachable", a2ashape.Args{"hub": testHub}).Body(a2ashape.English)
	hc := &hubControl{dryStatus: http.StatusBadGateway, dry: ref}
	c := hc.serve(t, nil)
	asked := false
	prev := joinRecovery
	joinRecovery = func(string, bool, map[string]string) ([]byte, bool, error) {
		asked = true
		return prev("", false, nil)
	}
	t.Cleanup(func() { joinRecovery = prev })
	h, out, errb := testHubCLI(nil)
	if err := h.hubRegister(c, []string{testHub}); err != errExit1 {
		t.Fatalf("exit: %v", err)
	}
	if want := ref.Error + "\n  → " + ref.Next + "\n"; errb.String() != want || out.Len() != 0 {
		t.Fatalf("stderr %q, want %q; stdout %q", errb, want, out)
	}
	if asked || len(hc.requests()) != 1 {
		t.Fatalf("after hub_unreachable: recovery asked %v, %d requests", asked, len(hc.requests()))
	}
}

// UX-T11, ux UX-C4: the exit status of each outcome of hub-register.
func TestHubRegisterExitStatus(t *testing.T) {
	args := a2ashape.Args{"hub": testHub, "aid": testAID, "why": "unreachable", "time": time.Now().Add(time.Minute), "code": "kel_invalid"}
	for _, c := range []struct {
		state, reason string
		want          error
	}{
		{daemon.JoinRegistered, "registered", nil},
		{daemon.JoinRegistered, "registered.probation", nil},
		{daemon.JoinPendingAdmission, "hub_pending_admission", nil},
		{daemon.JoinRetrying, "hub_retrying", nil},
		{daemon.JoinRetrying, "hub_retrying.quota", nil},
		{daemon.JoinRefused, "hub_refused", errExit1},
		{daemon.JoinRefused, "wire_too_old", errExit1},
	} {
		hc := &hubControl{dryStatus: 200, dry: dryJoin(), joinStatus: 200, join: joinAnswer(c.state, c.reason, args, false)}
		cl := hc.serve(t, nil)
		h, out, _ := testHubCLI(nil)
		if err := h.hubRegister(cl, []string{testHub}); err != c.want {
			t.Errorf("%s: %v, want %v", c.reason, err, c.want)
		}
		if !strings.HasPrefix(out.String(), a2ashape.Sentence(c.reason, a2ashape.English, args)) {
			t.Errorf("%s: %q", c.reason, out)
		}
	}
	// A refusal of the request itself (a switch of hubs) is 1, on stderr.
	sw := a2ashape.Refuse("hub_switch_requires_rotation", a2ashape.Args{"hub": testHub}).Body(a2ashape.English)
	hc := &hubControl{dryStatus: 200, dry: map[string]any{"hub": testHub, "hub_aid": "x", "action": "switch"},
		joinStatus: http.StatusConflict, join: sw}
	cl := hc.serve(t, nil)
	h, _, errb := testHubCLI(nil)
	if err := h.hubRegister(cl, []string{testHub}); err != errExit1 || !strings.HasPrefix(errb.String(), sw.Error) {
		t.Errorf("switch: %v %q", err, errb)
	}
	if reqs := hc.requests(); len(reqs) != 2 || reqs[1]["next_digest"] != nil {
		t.Errorf("a switch sent %v", reqs)
	}
	// No URL: usage, exit 2.
	if err := (hubCLI{}).hubRegister(cl, nil); !errors.As(err, new(usageError)) {
		t.Errorf("no url: %v", err)
	}
}

// An identity whose witness is the hub updates its registration: no
// next_digest, no reminder.
func TestHubRegisterUpdate(t *testing.T) {
	hc := &hubControl{dryStatus: 200, dry: map[string]any{"hub": testHub, "hub_aid": "x", "action": "update", "recovery_needed": false},
		joinStatus: 200, join: joinAnswer(daemon.JoinRegistered, "registered", a2ashape.Args{"hub": testHub}, false)}
	c := hc.serve(t, nil)
	h, out, _ := testHubCLI(nil)
	if err := h.hubRegister(c, []string{testHub}); err != nil {
		t.Fatal(err)
	}
	if reqs := hc.requests(); len(reqs) != 2 || reqs[1]["next_digest"] != nil || reqs[1]["unprotected"] != nil {
		t.Fatalf("update sent %v", reqs)
	}
	if out.String() != a2ashape.Sentence("registered", a2ashape.English, a2ashape.Args{"hub": testHub})+"\n" {
		t.Fatalf("stdout %q", out)
	}
}

// AV-5: ANET_INVITE is ignored with one line (config_key_removed) and the
// command goes on; the invite reaches neither the daemon nor its argv.
func TestHubRegisterIgnoresANETINVITE(t *testing.T) {
	hc := &hubControl{dryStatus: 200, dry: dryJoin(), joinStatus: 200,
		join: joinAnswer(daemon.JoinRegistered, "registered", a2ashape.Args{"hub": testHub}, false)}
	c := hc.serve(t, nil)
	h, _, errb := testHubCLI(map[string]string{"ANET_INVITE": "anetinv_SECRET"})
	if err := h.hubRegister(c, []string{testHub}); err != nil {
		t.Fatal(err)
	}
	want := a2ashape.Sentence("config_key_removed", a2ashape.English, a2ashape.Args{"key": "ANET_INVITE", "instead": "invite_env"}) + "\n"
	if errb.String() != want {
		t.Fatalf("stderr %q, want %q", errb, want)
	}
	for _, r := range hc.requests() {
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), "SECRET") || r["token"] != nil || r["invite"] != nil {
			t.Fatalf("the invite reached the daemon: %s", b)
		}
	}
}

// AD-8: hub-leave says the daemon's refusal (witness_hub_leave) on stderr,
// exit 1.
func TestHubLeaveRefusal(t *testing.T) {
	ref := a2ashape.Refuse("witness_hub_leave", a2ashape.Args{"hub": testHub}).Body(a2ashape.English)
	hc := &hubControl{leaveStatus: http.StatusConflict, leave: ref}
	c := hc.serve(t, nil)
	var errb bytes.Buffer
	prevErr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	err := runHubLeave(c, []string{testHub})
	w.Close()
	os.Stderr = prevErr
	_, _ = io.Copy(&errb, r)
	if err != errExit1 || errb.String() != ref.Error+"\n  → "+ref.Next+"\n" {
		t.Fatalf("%v %q", err, errb.String())
	}
}

// UX-T11 through the binary: the exit status a person sees, and that the
// removed flags stop before the daemon (UX-T7).
func TestHubRegisterExitStatusOfTheBinary(t *testing.T) {
	args := a2ashape.Args{"hub": testHub, "aid": testAID}
	for _, c := range []struct {
		name   string
		dry    int
		join   map[string]any
		args   []string
		code   int
		asked  int
		stdout string
	}{
		{"pending", 200, joinAnswer(daemon.JoinPendingAdmission, "hub_pending_admission", args, true), []string{"hub-register", testHub}, 0, 2,
			"  → anet-hub admit " + testAID + "\n"},
		{"refused", 200, joinAnswer(daemon.JoinRefused, "hub_refused", a2ashape.Args{"hub": testHub, "code": "kel_invalid"}, false),
			[]string{"hub-register", testHub}, 1, 2, ""},
		{"unreachable", http.StatusBadGateway, nil, []string{"hub-register", testHub}, 1, 1, ""},
		{"no url", 200, nil, []string{"hub-register"}, 2, 0, ""},
		{"--token", 200, nil, []string{"hub-register", testHub, "--token", "x"}, 2, 0, ""},
		{"--token-file", 200, nil, []string{"hub-register", testHub, "--token-file=f"}, 2, 0, ""},
		{"--accept-delegations", 200, nil, []string{"hub-register", testHub, "--accept-delegations", "false"}, 2, 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newCLIHome(t, "C")
			hc := &hubControl{dryStatus: c.dry, dry: dryJoin(), joinStatus: 200, join: c.join}
			if c.dry != 200 {
				hc.dry = a2ashape.Refuse("hub_unreachable", a2ashape.Args{"hub": testHub}).Body(a2ashape.English)
			}
			hc.serve(t, h)
			stdout, stderr, code := runCLI(t, h, c.args)
			if code != c.code || len(hc.requests()) != c.asked {
				t.Fatalf("exit %d, %d requests; want %d, %d\nstdout %s\nstderr %s", code, len(hc.requests()), c.code, c.asked, stdout, stderr)
			}
			if c.stdout != "" && !strings.HasSuffix(stdout, c.stdout) {
				t.Fatalf("stdout %q does not end with %q", stdout, c.stdout)
			}
		})
	}
}
