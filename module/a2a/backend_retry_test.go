//go:build !no_a2a

package a2a

// A forward that failed for a reason a later attempt may fix is tried again
// (docs/notes/0035 §5.1: a backend that was down once kept its tasks
// unanswered until the daemon restarted).

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// quickRetries shortens the first wait between attempts for a test.
func quickRetries(t *testing.T) {
	t.Helper()
	old := retryBase
	retryBase = 20 * time.Millisecond
	t.Cleanup(func() { retryBase = old })
}

// flaky answers the backend's task calls (not its card) with status for
// the first fail of them, recording the message id each one carried.
type flaky struct {
	mu     sync.Mutex
	fail   int
	status int
	ids    []string
}

func (f *flaky) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rpc" {
			next.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var rpc struct {
			Params struct {
				Message struct {
					MessageID string `json:"messageId"`
				} `json:"message"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &rpc)
		f.mu.Lock()
		f.ids = append(f.ids, rpc.Params.Message.MessageID)
		failing := f.fail > 0
		if failing {
			f.fail--
		}
		f.mu.Unlock()
		if failing {
			http.Error(w, "busy", f.status)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (f *flaky) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ids...)
}

// flakyBackend is a test backend whose task calls go through f.
func flakyBackend(t *testing.T, f *flaky) *testBackend {
	t.Helper()
	b := newTestBackend(t)
	b.srv.Config.Handler = f.wrap(b.srv.Config.Handler)
	return b
}

// kernelSays makes h's InboundTask answer t while ok, as the kernel would
// for a task that may still go to a backend.
func kernelSays(h *inboundHost, t module.Task, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = func(id string) (module.Task, bool) { return t, ok && id == t.ID }
}

// A backend that answers 503 twice gets the task on the third attempt: one
// forward reaches it, one answer goes back, one forwarded record is written
// and no failure; every attempt carries the same message id.
func TestAFailedForwardIsTriedAgainUntilItGetsThrough(t *testing.T) {
	quickRetries(t)
	f := &flaky{fail: 2, status: http.StatusServiceUnavailable}
	b := flakyBackend(t, f)
	h := newInboundHost(t)
	task := inboundTask("ix1", "bafypeer", true, "hello")
	kernelSays(h, task, true)
	startBackendModule(t, h, `{"backends":[{"allow_tcp":true,"match":"*","url":"`+b.srv.URL+`","token_file":"`+tokenFile(t)+`"}]}`)
	h.tasks <- task
	if r := h.wait(t); r.state != a2ashape.TaskStateCompleted || r.msg.Parts[0].Text != "backend: hello" {
		t.Fatalf("reply %+v", r)
	}
	ids := f.seen()
	if len(ids) != 3 || ids[0] == "" || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("message ids of the attempts: %v, want three, all the same", ids)
	}
	if ids[0] == "mix1hello" {
		t.Fatal("the backend was sent the requester's own message id")
	}
	h.mu.Lock()
	k := h.kinds()
	h.mu.Unlock()
	if b.n() != 1 || k["anet.backend.forwarded"] != 1 || k["anet.backend.failed"] != 0 {
		t.Fatalf("backend calls %d, evidence %v", b.n(), k)
	}
}

// The case of 0035: the backend's socket is not there yet (it is starting,
// or restarting) — nothing was sent — and then it is.
func TestAForwardWaitsForABackendThatIsNotListeningYet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix sockets with owners here")
	}
	quickRetries(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "a2a.sock")
	h := newInboundHost(t)
	task := inboundTask("ix1", "bafypeer", true, "hello")
	kernelSays(h, task, true)
	startBackendModule(t, h, `{"backends":[{"match":"*","url":"unix://`+sock+`","token_file":"`+tokenFile(t)+`"}]}`)
	h.tasks <- task
	time.Sleep(100 * time.Millisecond) // a few attempts at a socket that is not there
	b := newTestBackendAt(t, func(string) string { return "http://localhost" })
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: b.srv.Config.Handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	if r := h.wait(t); r.state != a2ashape.TaskStateCompleted || r.msg.Parts[0].Text != "backend: hello" {
		t.Fatalf("reply %+v", r)
	}
	if b.n() != 1 {
		t.Fatalf("backend calls %d", b.n())
	}
}

// Before each retry the kernel decides again: once it says the task may no
// longer go to a backend (its peer left the trust list, it was answered),
// nothing more is sent, although the backend is up again — and no failure
// is recorded for a forward that was withdrawn rather than given up.
func TestRetriesEndWhenTheKernelSaysNo(t *testing.T) {
	quickRetries(t)
	f := &flaky{fail: 1, status: http.StatusBadGateway}
	b := flakyBackend(t, f)
	h := newInboundHost(t)
	task := inboundTask("ix1", "bafypeer", true, "hello")
	kernelSays(h, task, false)
	startBackendModule(t, h, `{"backends":[{"allow_tcp":true,"match":"*","url":"`+b.srv.URL+`","token_file":"`+tokenFile(t)+`"}]}`)
	h.tasks <- task
	time.Sleep(400 * time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(f.seen()); n != 1 || b.n() != 0 || len(h.replies) != 0 || len(h.evidence) != 0 {
		t.Fatalf("attempts %d, backend calls %d, replies %v, evidence %v; want one attempt and nothing else",
			n, b.n(), h.replies, h.evidence)
	}
}

// Retries end retry.give_up_after after the first attempt; the failure is
// recorded once, with the attempts made. An answer that is not "later" —
// here a 400 — is not tried again at all.
func TestRetriesGiveUpAndAnswersAreNotRetried(t *testing.T) {
	quickRetries(t)
	f := &flaky{fail: 1 << 20, status: http.StatusServiceUnavailable}
	b := flakyBackend(t, f)
	h := newInboundHost(t)
	task := inboundTask("ix1", "bafypeer", true, "hello")
	kernelSays(h, task, true)
	startBackendModule(t, h, `{"retry":{"max_interval":"40ms","give_up_after":"300ms"},`+
		`"backends":[{"allow_tcp":true,"match":"*","url":"`+b.srv.URL+`","token_file":"`+tokenFile(t)+`"}]}`)
	h.tasks <- task
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		k := h.kinds()
		ev := append([]map[string]any(nil), h.evidence...)
		h.mu.Unlock()
		if k["anet.backend.failed"] == 1 {
			if len(ev) != 1 || ev[0]["gave_up"] != true || ev[0]["attempts"].(int) < 3 || ev[0]["attempts"].(int) != len(f.seen()) {
				t.Fatalf("evidence %v after %d attempts", ev, len(f.seen()))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no failure recorded; evidence %v, %d attempts", ev, len(f.seen()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(f.seen()); b.n() != 0 || n > 12 {
		t.Fatalf("backend calls %d, attempts %d after giving up", b.n(), n)
	}

	f2 := &flaky{fail: 1 << 20, status: http.StatusBadRequest}
	b2 := flakyBackend(t, f2)
	h2 := newInboundHost(t)
	kernelSays(h2, task, true)
	startBackendModule(t, h2, `{"backends":[{"allow_tcp":true,"match":"*","url":"`+b2.srv.URL+`","token_file":"`+tokenFile(t)+`"}]}`)
	h2.tasks <- task
	time.Sleep(300 * time.Millisecond)
	h2.mu.Lock()
	defer h2.mu.Unlock()
	if n := len(f2.seen()); n != 1 || !h2.refusedOnce() || h2.evidence[0]["attempts"] != 1 {
		t.Fatalf("a 400: %d attempts, evidence %v", n, h2.evidence)
	}
}

// retry.max_interval and retry.give_up_after are Go durations; anything
// else is refused when the module is built.
func TestRetryConfig(t *testing.T) {
	isolateHome(t)
	for _, bad := range []string{`{"retry":{"max_interval":"soon"}}`, `{"retry":{"give_up_after":"-1m"}}`, `{"retry":{"tries":3}}`} {
		if _, err := New([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
	for cfg, want := range map[string][2]time.Duration{
		`{}`: {defaultRetryMax, defaultRetryGiveUp},
		`{"retry":{"max_interval":"30s","give_up_after":"0"}}`: {30 * time.Second, 0},
	} {
		m, err := New([]byte(cfg))
		if err != nil {
			t.Fatal(err)
		}
		p, err := m.(*Module).cfg.Retry.resolve()
		if err != nil || p.max != want[0] || p.giveUp != want[1] || p.base != retryBase {
			t.Errorf("%s: %+v %v", cfg, p, err)
		}
	}
}
