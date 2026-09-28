//go:build taskboard

package taskboard_test

// A mutation of the board that may have reached it and lost its answer:
// the card may have been created, or claimed. That is an outcome nobody
// knows (provider.OutcomeUnknownError), not FAILED, which says it did not
// happen and lets a caller create the card twice (SI-6, the rule of the
// service module, A2A-DESIGN §4.3).

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ANetResearch/ANet/module/taskboard"
	"github.com/ANetResearch/ANet/provider"
)

func TestABoardChangeWhoseAnswerIsLostIsNotFailed(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		reason string
	}{
		{"connection", func(w http.ResponseWriter) {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close() // the card is on the board; the answer is lost
			}
		}, provider.ReasonConnectionLost},
		{"body", func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "64")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"card":{"id":`))
			w.(http.Flusher).Flush()
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		}, provider.ReasonConnectionLost},
		{"gateway 504", func(w http.ResponseWriter) {
			// The proxy in front of the hub answering for it: the hub did
			// not answer in time, and may have made the change.
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte("<html>504 Gateway Time-out</html>"))
		}, provider.ReasonTimeout},
		{"gateway 502", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
		}, provider.ReasonConnectionLost},
	} {
		t.Run(c.name, func(t *testing.T) {
			var changes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				changes.Add(1) // the board made the change
				c.answer(w)
			}))
			defer srv.Close()

			p := start(t, srv.URL, ctrlFor(t))
			_, err := p.Invoke(context.Background(), provider.Call{
				Capability: taskboard.CapCreate,
				Args:       map[string]any{"title": "t", "taskdoc_cid": "bafy-doc"},
			})
			if changes.Load() != 1 {
				t.Fatalf("the board was asked %d times, want 1", changes.Load())
			}
			unknown, ok := provider.OutcomeOf(err)
			if !ok {
				t.Fatalf("err %v: a change the board may have made is reported as FAILED; want an unknown outcome", err)
			}
			if unknown.Reason != c.reason {
				t.Fatalf("reason %q (%v), want %s", unknown.Reason, err, c.reason)
			}
		})
	}
}

// A board that could not be reached had nothing asked of it: not an
// unknown outcome.
func TestABoardThatCannotBeReachedMadeNoChange(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens there now: the dial is refused

	p := start(t, "http://"+addr, ctrlFor(t))
	_, err = p.Invoke(context.Background(), provider.Call{
		Capability: taskboard.CapClaim, Args: map[string]any{"card_id": "card-1"},
	})
	if err == nil {
		t.Fatal("a claim on a board nobody serves succeeded")
	}
	if _, unknown := provider.OutcomeOf(err); unknown || errors.As(err, new(*provider.OutcomeUnknownError)) {
		t.Fatalf("err %v: a refused connection is reported as a change that may have been made", err)
	}
}
