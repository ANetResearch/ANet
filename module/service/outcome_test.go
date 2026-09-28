//go:build !no_service

package service_test

// What a transport error says about the effect (A2A-DESIGN §4.3; red-team
// finding F10). UNAVAILABLE says nothing was attempted at the far end, and
// is only true when the request never went out. A call that went out and
// lost its answer — the deadline passed while the service worked, or the
// connection dropped before the reply — may have had its effect: it is a
// provider.OutcomeUnknownError, which the daemon reports as failed with
// effect UNVERIFIED, never as UNAVAILABLE (which invites a retry that runs
// the effect again).

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/provider"
)

// invokeUnknown invokes capID under ctx and requires the outcome-unknown
// error.
func invokeUnknown(t *testing.T, ctx context.Context, capID, cfg string) (effect.Effect, *provider.OutcomeUnknownError) {
	t.Helper()
	reg := start(t, cfg)
	p, ok := reg.Resolve(capID)
	if !ok {
		t.Fatalf("capability %q did not register", capID)
	}
	eff, err := p.Invoke(ctx, provider.Call{Capability: capID, Args: map[string]any{"to": "x@example.org"}})
	var unknown *provider.OutcomeUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("status %s, err %v: want a provider.OutcomeUnknownError (never UNAVAILABLE, \"nothing attempted\")",
			eff.Status, err)
	}
	if eff.Status == effect.Unavailable {
		t.Fatalf("an effect that may have happened is reported UNAVAILABLE")
	}
	return eff, unknown
}

// deadline is a call's deadline that passes when the test says: once the
// service has the call, not after a time the test hopes is long enough for
// the call to get there. Reaching a TCP backend reads the kernel's socket
// tables (internal/backendconn), which alone can take longer than a short
// bound on a loaded host; a bound that passed then was a call that never
// went out, rightly UNAVAILABLE, and a failed test.
type deadline struct {
	context.Context // Background: no values, no deadline of its own
	done            chan struct{}
	once            sync.Once
}

func newDeadline() *deadline {
	return &deadline{Context: context.Background(), done: make(chan struct{})}
}

func (d *deadline) Done() <-chan struct{} { return d.done }

func (d *deadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// pass makes the deadline pass.
func (d *deadline) pass() { d.once.Do(func() { close(d.done) }) }

// until waits for the call's end on the service's side, bounded so that a
// call that is never cut off fails its test instead of hanging it. The
// request's context ends when the node hangs up only once its body has been
// read (net/http watches the connection from then on).
func until(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	select {
	case <-r.Context().Done():
	case <-time.After(30 * time.Second):
	}
}

// The service performs the effect, and the call's deadline passes before
// it answers.
func TestTimeoutAfterTheCallWentOutIsNotUnavailable(t *testing.T) {
	var effects atomic.Int32
	d := newDeadline()
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effects.Add(1) // the side effect happens here (mail sent, order placed, ...)
		d.pass()
		until(r)
		_, _ = w.Write([]byte(`{"sent":true}`))
	}))
	defer svc.Close()

	_, unknown := invokeUnknown(t, d, "mail.send", `{"allow_tcp":true,"capabilities":[{"id":"mail.send","url":"`+svc.URL+`"}]}`)
	if effects.Load() != 1 {
		t.Fatalf("the service ran %d times, want 1", effects.Load())
	}
	if unknown.Reason != provider.ReasonTimeout || !errors.Is(unknown, context.DeadlineExceeded) {
		t.Fatalf("reason %q, err %v: want timeout", unknown.Reason, unknown.Err)
	}
}

// The service performs the effect and the connection drops before any
// response: not a timing race, just a lost answer.
func TestConnectionLostAfterTheCallWentOutIsNotUnavailable(t *testing.T) {
	var effects atomic.Int32
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		effects.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close() // effect done, answer lost
	}))
	defer svc.Close()

	_, unknown := invokeUnknown(t, context.Background(), "mail.send", `{"allow_tcp":true,"capabilities":[{"id":"mail.send","url":"`+svc.URL+`"}]}`)
	if effects.Load() != 1 {
		t.Fatalf("the service ran %d times, want 1", effects.Load())
	}
	if unknown.Reason != provider.ReasonConnectionLost {
		t.Fatalf("reason %q, want connection_lost", unknown.Reason)
	}
}

// A service that cannot be reached at all had nothing attempted: that is
// still UNAVAILABLE, the answer that tells a requester trying elsewhere is
// safe.
func TestUnreachableServiceIsStillUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens there now: the dial is refused

	reg := start(t, `{"allow_tcp":true,"capabilities":[{"id":"mail.send","url":"http://`+addr+`/send","timeout_ms":2000}]}`)
	eff := invoke(t, reg, "mail.send", nil)
	if eff.Status != effect.Unavailable {
		t.Fatalf("a refused connection: status %s (%s), want UNAVAILABLE", eff.Status, eff.Message)
	}
}

// The service takes the call — 200 and the start of its reply — and the
// answer breaks off: the deadline passes while the body is still coming,
// or the connection drops mid-reply. Its effect has happened as surely as
// one whose reply never started, so it is the same unknown outcome; it was
// FAILED ("reply is not a JSON object"), which says the effect did not
// happen (a bypass of F10 found on review).
//
// The deadline passes once the service has sent the start of its answer;
// whether the node has read the headers by then or not, the outcome is the
// same.
func TestAnAnswerThatBreaksOffAfterItsHeadersIsNotFailed(t *testing.T) {
	for _, c := range []struct {
		name   string
		cut    func(w http.ResponseWriter, r *http.Request, d *deadline)
		reason string
	}{
		{"deadline", func(_ http.ResponseWriter, r *http.Request, d *deadline) {
			d.pass()
			until(r)
		}, provider.ReasonTimeout},
		{"connection", func(w http.ResponseWriter, _ *http.Request, _ *deadline) {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		}, provider.ReasonConnectionLost},
	} {
		t.Run(c.name, func(t *testing.T) {
			var effects atomic.Int32
			d := newDeadline()
			svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				effects.Add(1) // the effect happens
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "64") // more than will come
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"sen`))
				w.(http.Flusher).Flush()
				c.cut(w, r, d)
			}))
			defer svc.Close()

			_, unknown := invokeUnknown(t, d, "mail.send",
				`{"allow_tcp":true,"capabilities":[{"id":"mail.send","url":"`+svc.URL+`"}]}`)
			if effects.Load() != 1 {
				t.Fatalf("the service ran %d times, want 1", effects.Load())
			}
			if unknown.Reason != c.reason {
				t.Fatalf("reason %q (%v), want %s", unknown.Reason, unknown.Err, c.reason)
			}
		})
	}
}

// A refusal is its status whatever happens to its body: a 503 whose body
// breaks off is still UNAVAILABLE (the service said "not now").
func TestARefusalWhoseBodyBreaksOffIsItsStatus(t *testing.T) {
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("busy"))
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	defer svc.Close()
	reg := start(t, `{"allow_tcp":true,"capabilities":[{"id":"mail.send","url":"`+svc.URL+`","timeout_ms":2000}]}`)
	if eff := invoke(t, reg, "mail.send", nil); eff.Status != effect.Unavailable {
		t.Fatalf("status %s (%s), want UNAVAILABLE", eff.Status, eff.Message)
	}
}

// A reverse proxy in front of the service answers for it when the service
// is slow (504) or its answer breaks (502): the call reached the service,
// which may have acted. Not FAILED — a proxy_read_timeout shorter than the
// work would make every slow call "did not happen" (F10, on review).
func TestAGatewayAnsweringForTheServiceIsAnUnknownOutcome(t *testing.T) {
	for _, c := range []struct {
		status int
		reason string
	}{{http.StatusGatewayTimeout, provider.ReasonTimeout}, {http.StatusBadGateway, provider.ReasonConnectionLost}} {
		var effects atomic.Int32
		svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			effects.Add(1) // the service behind the gateway acted
			http.Error(w, "upstream timed out", c.status)
		}))
		_, unknown := invokeUnknown(t, context.Background(), "mail.send", `{"allow_tcp":true,"capabilities":[{"id":"mail.send","url":"`+svc.URL+`"}]}`)
		svc.Close()
		if unknown.Reason != c.reason || effects.Load() != 1 {
			t.Fatalf("HTTP %d: reason %q (effects %d), want %s", c.status, unknown.Reason, effects.Load(), c.reason)
		}
	}
}

// A call cut off while its request is still being written: the service has
// the start of it — here it stops reading after the request line, and a
// header block too large for the socket's buffer leaves the rest unwritten
// when the deadline passes. Whether it acted on what it had is not known on
// this side. It was UNAVAILABLE, "nothing was sent", because the mark that
// a request went out was set only once its whole header block had been
// written (provider.TrackSent, SI-6).
func TestACallCutOffWhileItsRequestIsWrittenIsNotUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix sockets with owners here")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "backend.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d := newDeadline()
	var line string
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ = bufio.NewReader(c).ReadString('\n') // the call has arrived
		d.pass()
		<-stop // and the service reads no more of it
	}()

	reg := start(t, `{"capabilities":[{"id":"mail.send","url":"unix://`+sock+`"}]}`)
	p, _ := reg.Resolve("mail.send")
	// An AF_UNIX socket holds some hundreds of KiB (net.core.wmem_default);
	// a header block several times that cannot all be written.
	call := provider.Call{Capability: "mail.send", CallID: strings.Repeat("c", 8<<20), Args: map[string]any{"to": "x@example.org"}}
	eff, err := p.Invoke(d, call)
	if !strings.HasPrefix(line, "POST ") {
		t.Fatalf("the service read %q", line)
	}
	var unknown *provider.OutcomeUnknownError
	if !errors.As(err, &unknown) || eff.Status == effect.Unavailable {
		t.Fatalf("status %s (%s), err %v: a call the service has begun to receive is reported as never sent",
			eff.Status, eff.Message, err)
	}
	if eff.Status != effect.Unverified || unknown.Reason != provider.ReasonTimeout {
		t.Fatalf("status %s, reason %q (%v): want UNVERIFIED, timeout", eff.Status, unknown.Reason, err)
	}
}
