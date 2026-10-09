package main

// UX-T11: the exit statuses of ux UX-C4. 0 — the command did what it was
// asked, including the outcomes that wait and finish on their own
// (hub_pending_admission, hub_retrying, a join without a terminal that
// leaves the identity without a recovery passphrase); 1 — refused or failed
// (any control-plane answer other than 2xx, hub_refused, hub_unreachable, no
// terminal for a confirmation, confirm.declined); 2 — used wrongly
// (a usage error, cli_removed, api_key_on_argv, aid_invalid); 3 — only task
// watch and delegate --watch, for a task that stops at input-required.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// scriptedControl is a control plane whose answers a test writes: answer(path, body) gives the status
// and the body. The CLI in h is pointed at it (config.json and the token file).
type scriptedControl struct {
	calls  atomic.Int64
	mu     sync.Mutex
	paths  []string
	bodies []map[string]any
}

func newScriptedControl(t *testing.T, h *cliHome, answer func(path string, body map[string]any) (int, string)) *scriptedControl {
	t.Helper()
	sc := &scriptedControl{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		sc.calls.Add(1)
		sc.mu.Lock()
		sc.paths = append(sc.paths, r.URL.Path)
		sc.bodies = append(sc.bodies, body)
		sc.mu.Unlock()
		code, out := answer(r.URL.Path, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(out))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	cfg := `{"control_addr": "` + ln.Addr().String() + `"}`
	if err := os.WriteFile(filepath.Join(h.data, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.data, "control_token.txt"), []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return sc
}

// sent is the paths the control plane was asked, in order.
func (sc *scriptedControl) sent() []string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return append([]string(nil), sc.paths...)
}

// lastBody is the body of the last request to path.
func (sc *scriptedControl) lastBody(path string) map[string]any {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for i := len(sc.paths) - 1; i >= 0; i-- {
		if sc.paths[i] == path {
			return sc.bodies[i]
		}
	}
	return nil
}

// wholeAID is an AID as anetcid.Sum writes one (identity.ValidAID).
const wholeAID = "bafyreie6k7xot2qpitfcoseuevnuq7zcztdpdgx4coe3p2jlqjjl65hqu4"

// scriptedJoin is a control plane that answers hub-register's dry run with a first join and the join with
// state (and, with unprotected, the recovery reminder of a join without a terminal, ux §3.4 (g)).
func scriptedJoin(state string, unprotected bool) func(string, map[string]any) (int, string) {
	return func(path string, body map[string]any) (int, string) {
		if path != "/hub-register" {
			return 404, `{"error":"no such route"}`
		}
		if body["dry_run"] == true {
			return 200, `{"hub":"https://hub.example","action":"join","recovery_needed":true}`
		}
		lang := a2ashape.English
		out := map[string]any{"hub": "https://hub.example", "state": state}
		switch state {
		case "pending_admission":
			args := a2ashape.Args{"hub": "https://hub.example", "aid": wholeAID}
			out["message"], out["next"] = a2ashape.Sentence("hub_pending_admission", lang, args), a2ashape.Next("hub_pending_admission", lang, args)
		case "retrying":
			args := a2ashape.Args{"hub": "https://hub.example", "why": "unreachable", "retry": 30 * time.Second}
			out["message"] = a2ashape.Sentence("hub_retrying", lang, args)
		case "registered":
			out["message"] = a2ashape.Sentence("registered", lang, a2ashape.Args{"hub": "https://hub.example"})
		case "refused":
			out["message"] = a2ashape.Sentence("hub_refused", lang, a2ashape.Args{"hub": "https://hub.example", "code": "banned"})
		}
		if unprotected {
			out["recovery"] = map[string]any{"state": "unprotected", "message": a2ashape.Sentence("identity.unprotected", lang, nil),
				"next": "anet identity protect"}
		}
		b, _ := json.Marshal(out)
		return 200, string(b)
	}
}

func TestExitStatus(t *testing.T) {
	en := a2ashape.English
	task := func(state string) string {
		return `{"id":"ix_1","contextId":"c","status":{"state":"` + state + `"},"metadata":{"anet.role":"requester"}}`
	}
	refusal := func(code int, key string, args a2ashape.Args) func(string, map[string]any) (int, string) {
		b, _ := json.Marshal(a2ashape.Refuse(key, args).Body(en))
		return func(string, map[string]any) (int, string) { return code, string(b) }
	}
	for _, tc := range []struct {
		name   string
		args   []string
		answer func(string, map[string]any) (int, string)
		code   int
		// stdout says whether the command prints an answer (JSON or the daemon's sentences) there.
		stdout bool
	}{
		// 0: done, including what finishes on its own.
		{"task get", []string{"task", "get", "ix_1"}, func(string, map[string]any) (int, string) { return 200, task("TASK_STATE_COMPLETED") }, 0, true},
		{"a failed task read", []string{"task", "get", "ix_1"}, func(string, map[string]any) (int, string) { return 200, task("TASK_STATE_FAILED") }, 0, true},
		{"hub_pending_admission", []string{"hub-register", "https://hub.example"}, scriptedJoin("pending_admission", false), 0, true},
		{"hub_retrying", []string{"hub-register", "https://hub.example"}, scriptedJoin("retrying", false), 0, true},
		{"registered", []string{"hub-register", "https://hub.example"}, scriptedJoin("registered", false), 0, true},
		{"joined without a terminal, no recovery passphrase yet", []string{"hub-register", "https://hub.example"},
			scriptedJoin("pending_admission", true), 0, true},
		// 1: refused or failed.
		{"a control-plane refusal", []string{"task", "get", "ix_9"}, refusal(404, "no_local_task", a2ashape.Args{"task": "ix_9"}), 1, false},
		{"a refused send", []string{"delegate", wholeAID, "hello"}, refusal(503, "hub_pending_admission",
			a2ashape.Args{"hub": "https://hub.example", "aid": wholeAID}), 1, false},
		{"hub_refused", []string{"hub-register", "https://hub.example"}, scriptedJoin("refused", false), 1, true},
		{"hub_unreachable", []string{"hub-register", "https://hub.example"}, refusal(502, "hub_unreachable",
			a2ashape.Args{"hub": "https://hub.example"}), 1, false},
		{"no terminal for a confirmation (tty_required)", []string{"peers", "allow", wholeAID}, refusal(500, "unknown_code", nil), 1, false},
		// 2: used wrongly.
		{"a usage error", []string{"task", "get"}, nil, 2, false},
		{"an unknown command", []string{"frobnicate"}, nil, 2, false},
		{"an unknown flag", []string{"find", "--capabilty", "x"}, nil, 2, false},
		{"cli_removed", []string{"accept", "on"}, nil, 2, false},
		{"api_key_on_argv", []string{"autoreply", "set", "--api-key", "sk-x"}, nil, 2, false},
		{"aid_invalid", []string{"card", "…7hq2abcd"}, nil, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCLIHome(t, "C")
			var sc *scriptedControl
			if tc.answer != nil {
				sc = newScriptedControl(t, h, tc.answer)
			} else {
				sc = newScriptedControl(t, h, func(string, map[string]any) (int, string) { return 200, "{}" })
			}
			stdout, stderr, code := runCLIWithin(t, h, 30*time.Second, tc.args)
			if code != tc.code {
				t.Fatalf("exit %d, want %d (stdout %q, stderr %q)", code, tc.code, stdout, stderr)
			}
			if (stdout != "") != tc.stdout {
				t.Errorf("stdout %q (stderr %q)", stdout, stderr)
			}
			if tc.code == 2 && sc.calls.Load() != 0 {
				t.Errorf("a command used wrongly asked the daemon %v", sc.sent())
			}
			if tc.code != 0 && stderr == "" && stdout == "" {
				t.Error("a non-zero exit said nothing")
			}
		})
	}
}

// confirm.declined (UX-R12): an answer on the terminal other than yes says so, changes nothing and is
// exit status 1, as tty_required is.
func TestDeclinedIsExit1(t *testing.T) {
	c, sent := recordingDaemon(t)
	withTTY(t, "no")
	var said bytes.Buffer
	ttyStderr = &said
	err := runPeers(c, []string{"allow", wholeAID})
	if err != errDeclined || said.String() != a2ashape.Sentence("confirm.declined", a2ashape.Lang(), nil)+"\n" {
		t.Fatalf("declined: %v, said %q", err, said.String())
	}
	for _, p := range sent() {
		if strings.HasPrefix(p, "/peers/") {
			t.Fatalf("declined, yet the daemon was asked %s", p)
		}
	}
	if code := exitStatus(err, io.Discard); code != 1 {
		t.Fatalf("exit %d", code)
	}
	// The input ends on the terminal without an answer (Ctrl+D): that is a "no" said at a terminal,
	// confirm.declined, not tty_required. [mut] an empty read is tty_required again → red.
	openTTY = func() (io.ReadWriteCloser, error) { return &fakeTTY{r: strings.NewReader("")}, nil }
	said.Reset()
	if err := runPeers(c, []string{"allow", wholeAID}); err != errDeclined ||
		said.String() != a2ashape.Sentence("confirm.declined", a2ashape.Lang(), nil)+"\n" {
		t.Fatalf("no answer on the terminal: %v, said %q", err, said.String())
	}
	for _, p := range sent() {
		if strings.HasPrefix(p, "/peers/") {
			t.Fatalf("no answer, yet the daemon was asked %s", p)
		}
	}
	if code := exitStatus(errNoTTY, io.Discard); code != 1 {
		t.Fatalf("no terminal: exit %d", code)
	}
	if code := exitStatus(usageError{"anet x"}, io.Discard); code != 2 {
		t.Fatalf("usage: exit %d", code)
	}
}

// The statuses this binary does not reach yet, with the package that brings each.
func TestExitStatusPending(t *testing.T) {
	t.Run("task watch: 3 at input-required, 1 at failed, rejected or canceled", func(t *testing.T) {
		t.Skip("v030-todo(UX-10)")
	})
	t.Run("rotation_pending: 0 after 60 seconds of identity recover/protect, 1 for a refused new request", func(t *testing.T) {
		t.Skip("v030-todo(KEL-10)")
	})
	t.Run("identity.protect.already: 0", func(t *testing.T) {
		t.Skip("v030-todo(KEL-10)")
	})
}
