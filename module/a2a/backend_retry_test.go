//go:build !no_a2a

package a2a

// A forward that failed for a reason a later attempt may fix is tried again
// (docs/notes/0035 §5.1: a backend that was down once kept its tasks
// unanswered until the daemon restarted).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/backendconn"
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
	f := &flaky{fail: 1, status: http.StatusServiceUnavailable}
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

// takenThen is a backend whose task calls (not its card) are read whole and
// then, for the first fail of them, answered by fail — a connection closed
// with no answer, or a status — as when the backend took the message and
// went down, or failed, while it worked.
type takenThen struct {
	mu    sync.Mutex
	left  int
	fail  func(w http.ResponseWriter)
	calls int
}

func (f *takenThen) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rpc" {
			next.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.calls++
		failing := f.left > 0
		if failing {
			f.left--
		}
		f.mu.Unlock()
		if failing {
			f.fail(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (f *takenThen) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Once the request carrying the message was written to the backend, the
// backend may be doing the work: a connection lost then, or a 500, is not
// followed by the same message again — that would run the requester's task
// twice, and a backend need not recognise the repeated id. The failure is
// recorded once, not given up after retries; the task stays in the inbox.
func TestAMessageTheBackendMayHaveTakenIsNotSentAgain(t *testing.T) {
	quickRetries(t)
	for name, fail := range map[string]func(http.ResponseWriter){
		"connection lost": func(w http.ResponseWriter) {
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				c.Close()
			}
		},
		"500": func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) },
		"502": func(w http.ResponseWriter) { http.Error(w, "bad gateway", http.StatusBadGateway) },
	} {
		t.Run(name, func(t *testing.T) {
			f := &takenThen{left: 1 << 20, fail: fail}
			b := newTestBackend(t)
			b.srv.Config.Handler = f.wrap(b.srv.Config.Handler)
			h := newInboundHost(t)
			task := inboundTask("ix1", "bafypeer", true, "hello")
			kernelSays(h, task, true)
			startBackendModule(t, h, `{"backends":[{"allow_tcp":true,"match":"*","url":"`+b.srv.URL+`","token_file":"`+tokenFile(t)+`"}]}`)
			h.tasks <- task
			deadline := time.Now().Add(5 * time.Second)
			for {
				h.mu.Lock()
				done := len(h.evidence) > 0
				h.mu.Unlock()
				if done || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(300 * time.Millisecond) // retries, were there any, would be under way
			h.mu.Lock()
			defer h.mu.Unlock()
			if n := f.n(); n != 1 || !h.refusedOnce() || h.evidence[0]["attempts"] != 1 || h.evidence[0]["gave_up"] != false {
				t.Fatalf("the backend got the message %d times; evidence %v, replies %v", n, h.evidence, h.replies)
			}
		})
	}
}

// retryable, rule by rule: before the message was written whole, a failure
// to reach the backend or a 5xx (its card) is retried and a 4xx is not;
// after, only a 503.
func TestRetryableOnlyWhenTheBackendCannotHaveTheMessage(t *testing.T) {
	boom := errors.New("boom")
	note := func(sent, failed bool, code int32) *httpNote {
		n := newHTTPNote()
		if sent {
			n.markSent()
		}
		n.failed.Store(failed)
		n.code.Store(code)
		return n
	}
	for _, tc := range []struct {
		name string
		n    *httpNote
		err  error
		want bool
	}{
		{"not reached", note(false, true, 0), boom, true},
		{"card 503", note(false, false, 503), boom, true},
		{"card 404", note(false, false, 404), boom, false},
		{"card unusable", note(false, false, 200), boom, false},
		{"path refused", note(false, true, 0), fmt.Errorf("dial: %w", backendconn.ErrRefused), false},
		{"written, connection lost", note(true, true, 0), boom, false},
		{"written, 500", note(true, false, 500), boom, false},
		{"written, 504", note(true, false, 504), boom, false},
		{"written, 503", note(true, false, 503), boom, true},
		{"written, 400", note(true, false, 400), boom, false},
	} {
		if got := retryable(tc.err, tc.n); got != tc.want {
			t.Errorf("%s: retryable %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A first turn the backend is slow to answer is announced to the requester
// as working (a ReplyTask with state working and no message) before the
// answer, so that the requester does not fail it as no_response (A2A-DESIGN
// §4.2); a quick answer, and a later turn (the task already working), are
// not announced.
func TestASlowFirstTurnIsAnnouncedAsWorking(t *testing.T) {
	old := announceAfter
	announceAfter = 50 * time.Millisecond
	t.Cleanup(func() { announceAfter = old })
	slow := func(b *testBackend) {
		next := b.srv.Config.Handler
		b.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/rpc" {
				time.Sleep(400 * time.Millisecond)
			}
			next.ServeHTTP(w, r)
		})
	}
	b := newTestBackend(t)
	slow(b)
	h := newInboundHost(t)
	startBackendModule(t, h, `{"backends":[{"allow_tcp":true,"match":"*","url":"`+b.srv.URL+`","token_file":"`+tokenFile(t)+`"}]}`)
	h.tasks <- inboundTask("ix1", "bafypeer", true, "hello")
	if r := h.wait(t); r.taskID != "ix1" || r.state != a2ashape.TaskStateWorking || len(r.msg.Parts) != 0 {
		t.Fatalf("first reply %+v, want working with no message", r)
	}
	if r := h.wait(t); r.state != a2ashape.TaskStateCompleted || r.msg.Parts[0].Text != "backend: hello" {
		t.Fatalf("second reply %+v", r)
	}

	later := inboundTask("ix2", "bafypeer", true, "hello", "which one?", "this one")
	later.Status.State = a2ashape.TaskStateWorking
	h.tasks <- later
	if r := h.wait(t); r.taskID != "ix2" || r.state != a2ashape.TaskStateCompleted {
		t.Fatalf("a later turn: first reply %+v, want the answer", r)
	}

	quick := newTestBackend(t)
	h2 := newInboundHost(t)
	startBackendModule(t, h2, `{"backends":[{"allow_tcp":true,"match":"*","url":"`+quick.srv.URL+`","token_file":"`+tokenFile(t)+`"}]}`)
	h2.tasks <- inboundTask("ix3", "bafypeer", true, "hello")
	if r := h2.wait(t); r.state != a2ashape.TaskStateCompleted {
		t.Fatalf("a quick answer: first reply %+v", r)
	}
	time.Sleep(200 * time.Millisecond)
	h2.mu.Lock()
	defer h2.mu.Unlock()
	if len(h2.replies) != 1 {
		t.Fatalf("a quick answer: replies %+v", h2.replies)
	}
}

// A task waiting to be tried again gives up its place among the forwards
// running at once: with maxForwards tasks for a backend that is down, a
// task for one that is up is still forwarded at once, not after they give
// up.
func TestTasksWaitingToBeTriedAgainDoNotHoldEveryPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix sockets with owners here")
	}
	old := retryBase
	retryBase = 300 * time.Millisecond
	t.Cleanup(func() { retryBase = old })
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	up := newTestBackend(t)
	h := newInboundHost(t)
	h.tasks = make(chan module.Task, maxForwards+1)
	down := func(id string) module.Task {
		d := inboundTask(id, "bafypeer", true, "down")
		d.Metadata[a2ashape.KeySkill] = "down"
		return d
	}
	h.mu.Lock()
	h.now = func(id string) (module.Task, bool) { return down(id), strings.HasPrefix(id, "down") }
	h.mu.Unlock()
	startBackendModule(t, h, `{"retry":{"max_interval":"300ms","give_up_after":"5s"},"backends":[`+
		`{"match":"down","url":"unix://`+filepath.Join(dir, "gone.sock")+`","token_file":"`+tokenFile(t)+`"},`+
		`{"allow_tcp":true,"match":"*","url":"`+up.srv.URL+`","token_file":"`+tokenFile(t)+`"}]}`)
	for i := 0; i < maxForwards; i++ {
		h.tasks <- down("down" + strconv.Itoa(i))
	}
	time.Sleep(100 * time.Millisecond) // every place taken by a task for the backend that is down
	start := time.Now()
	h.tasks <- inboundTask("ix-up", "bafypeer", true, "hello")
	r := h.wait(t)
	if r.taskID != "ix-up" || r.state != a2ashape.TaskStateCompleted {
		t.Fatalf("reply %+v", r)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the task for the backend that is up waited %s", took)
	}
}
