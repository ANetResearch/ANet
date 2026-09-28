package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
)

// TrackSent returns req traced so that sent reports whether its header
// block was written, the point after which the target may have begun the
// call (OutcomeUnknownError). Each attempt starts unsent: the transport
// tries again on a new connection only when nothing of the request reached
// the wire (a POST is not replayed otherwise).
//
// A provider that calls its target over HTTP uses it to tell "could not
// reach it" (UNAVAILABLE) from "sent, and the answer was lost" (an
// OutcomeUnknownError, AnswerLost).
func TrackSent(req *http.Request) (traced *http.Request, sent func() bool) {
	var s atomic.Bool
	traced = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GetConn:      func(string) { s.Store(false) },
		WroteHeaders: func() { s.Store(true) },
	}))
	return traced, s.Load
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
