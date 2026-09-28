package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
)

// TrackSent returns req traced so that sent reports whether the request may
// have reached its target, the point after which the target may have begun
// the call (OutcomeUnknownError). An error met while sent is false is one
// of reaching the target: a dial refused, a far side that failed
// internal/backendconn's checks, a deadline that passed before a
// connection was had. Nothing of the request was written anywhere.
//
// It turns true when the transport hands the request a connection
// (httptrace GotConn), before a byte of it is written, and not later.
// net/http calls GotConn on the goroutine that called Do, before the
// request is written; it writes the request on another goroutine and
// returns as soon as the context ends. A mark set by a write callback
// (WroteHeaders) could come after Do had returned, and over HTTP/2 it
// always comes after the header block was flushed, which is when the
// server starts the handler: a call cut off by its deadline in that window
// reached the target and was reported as never sent (SI-6: "not known"
// must never be reported as "did not happen"). Marking at the connection
// can also report as unknown a call that failed before its first byte went
// out — on a pooled connection the far side had closed, for one — which
// errs the side that is only less precise.
//
// When the transport tries again on another connection (GetConn once more),
// sent turns false again only for a request that is not idempotent, as
// net/http defines it (a POST): net/http sends one of those again only when
// nothing of it was written. An idempotent request it may send again after
// it went out, so for one of those sent stays true once set.
//
// A provider that calls its target over HTTP uses it to tell "could not
// reach it" (UNAVAILABLE) from "sent, and the answer was lost" (an
// OutcomeUnknownError, AnswerLost).
func TrackSent(req *http.Request) (traced *http.Request, sent func() bool) {
	var s atomic.Bool
	retriedOnlyUnwritten := !idempotent(req)
	traced = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GetConn: func(string) {
			if retriedOnlyUnwritten {
				s.Store(false)
			}
		},
		GotConn: func(httptrace.GotConnInfo) { s.Store(true) },
		// Never first (GotConn comes before any write), and never harmful:
		// it can only say "may have been sent" where that is true.
		WroteHeaders: func() { s.Store(true) },
	}))
	return traced, s.Load
}

// idempotent is net/http's rule for a request it may retry after it was
// written (http.Transport): GET, HEAD, OPTIONS or TRACE, or one that
// carries an Idempotency-Key or X-Idempotency-Key header.
func idempotent(req *http.Request) bool {
	switch req.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	_, k := req.Header["Idempotency-Key"]
	_, x := req.Header["X-Idempotency-Key"]
	return k || x
}

// AnswerLost is the OutcomeUnknownError for err, an error met after the
// call went out: waiting for the answer, or reading it. Its reason is
// timeout when the call's deadline passed, connection_lost otherwise.
func AnswerLost(err error) *OutcomeUnknownError {
	reason := ReasonConnectionLost
	if timedOut(err) {
		reason = ReasonTimeout
	}
	return &OutcomeUnknownError{Reason: reason, Err: err}
}

// OutcomeOf reports whether an Invoke error leaves the effect unknown, and
// as what. An *OutcomeUnknownError is that; so is a call whose deadline
// passed, from any provider: the call was running when it did (the daemon
// set the deadline around Invoke), and a provider that says nothing more
// about it cannot say the effect did not happen. Any other error is not.
//
// The daemon and the voucher door both read an Invoke error through this,
// so that no provider has to know the rule for its timeouts to be reported
// as "not known" rather than FAILED (redteam F10).
func OutcomeOf(err error) (*OutcomeUnknownError, bool) {
	if err == nil {
		return nil, false
	}
	var unknown *OutcomeUnknownError
	if errors.As(err, &unknown) {
		return unknown, true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &OutcomeUnknownError{Reason: ReasonTimeout, Err: err}, true
	}
	return nil, false
}

// timedOut reports a deadline, as a context's or as a network timeout.
func timedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// OutcomeMessage is the message for err, which OutcomeOf read as unknown:
// err's own, which names the capability, when the provider said so; for a
// deadline it did not name, unknown's, which says the outcome is unknown.
func OutcomeMessage(err error, unknown *OutcomeUnknownError) string {
	if errors.As(err, new(*OutcomeUnknownError)) {
		return err.Error()
	}
	return unknown.Error()
}
