package anetlink

// A device command that went out and lost its answer (redteam F10, on the
// ANetLink shim): the device may have acted, so the shim reports an
// outcome nobody knows (provider.OutcomeUnknownError), which the daemon
// turns into failed + UNVERIFIED. It returned the bare transport error,
// which the daemon reports as FAILED — "it did not happen" — and a
// requester told that may send the command again.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ANetResearch/ANet/provider"
)

// cutLinkd serves /v0/invoke by acting and then losing the answer the way
// cut says.
func cutLinkd(t *testing.T, acted *atomic.Int32, cut func(w http.ResponseWriter)) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "l.sock")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/invoke", func(w http.ResponseWriter, _ *http.Request) {
		acted.Add(1) // the door opens
		cut(w)
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func TestALostDeviceAnswerIsAnUnknownOutcome(t *testing.T) {
	hangUp := func(w http.ResponseWriter) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}
	for _, c := range []struct {
		name string
		cut  func(w http.ResponseWriter)
	}{
		{"before the reply", hangUp},
		{"mid-reply", func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "64")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"stat`))
			w.(http.Flusher).Flush()
			hangUp(w)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var acted atomic.Int32
			p := New("link", cutLinkd(t, &acted, c.cut))
			_, err := p.Invoke(context.Background(), provider.Call{Capability: "door.open@sim/front"})
			var unknown *provider.OutcomeUnknownError
			if !errors.As(err, &unknown) || unknown.Reason != provider.ReasonConnectionLost {
				t.Fatalf("err %v: want an OutcomeUnknownError (connection_lost), never a plain failure", err)
			}
			if acted.Load() != 1 {
				t.Fatalf("the device acted %d times", acted.Load())
			}
		})
	}
}

// A socket nobody serves had nothing attempted: still a plain error.
func TestAnUnreachableLinkdIsNotAnUnknownOutcome(t *testing.T) {
	p := New("link", filepath.Join(t.TempDir(), "absent.sock"))
	_, err := p.Invoke(context.Background(), provider.Call{Capability: "door.open@sim/front"})
	if err == nil {
		t.Fatal("no error")
	}
	if _, unknown := provider.OutcomeOf(err); unknown {
		t.Fatalf("a call that never went out is reported as an unknown outcome: %v", err)
	}
}
