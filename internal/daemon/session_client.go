package daemon

// session_client.go is the first-contact session client (sealed-sender
// SSR-D27, §2.6; core L7, L8).
//
// A first contact (outer access kind 0) is the one send in which this
// node's hub learns who is sending: the daemon opens a session with one
// signed request (relayauth action session), the hub answers with a session
// id and a session key sealed to a one-time key, and every first contact of
// the session then carries a MAC under that key instead of a signature. The
// hub can compute the MAC too, so it is no evidence a third party would
// take; it authenticates the sender to its own hub and to nobody else.
//
// There is at most one current session per hub. It is opened on the signed
// side the first time a kind-0 send needs one, reused until five minutes
// before it ends, opened again when the hub answers 401 session_invalid,
// and dropped with every key it holds when this node rotates in an
// emergency (SSR-D23). The session key lives in this process's memory only.

import (
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// sessionRenewBefore is how long before its end a session stops being used:
// a send made just before the end could reach the hub after it.
const sessionRenewBefore = 5 * time.Minute

// sessionOpenTimeout bounds one opening of a session, whoever waits for
// it.
const sessionOpenTimeout = 30 * time.Second

// sessionMaxLife bounds the end a hub may give a session, from now. A hub
// gives an hour (sealed-sender §2.12); an end further out is the hub's
// mistake, and a session believed open for longer than the hub keeps it
// would only cost a 401 and a reopen.
const sessionMaxLife = 2 * time.Hour

// counterSessionUnavailable counts the first contacts that found no
// session: the hub refused to open one, or its answer was unusable.
const counterSessionUnavailable = "session-unavailable"

// errSessionUnavailable is a first contact that could not get a session
// (engine ENG-A6): nothing was handed to the hub, and the message waits
// with anet.delivery_wait = session_unavailable.
var errSessionUnavailable = errors.New("session_unavailable")

// sessionUnavailableError says why a session could not be opened.
type sessionUnavailableError struct{ cause error }

func (e *sessionUnavailableError) Error() string {
	return fmt.Sprintf("session_unavailable: this node's hub did not open a first-contact session: %v", e.cause)
}
func (e *sessionUnavailableError) Is(target error) bool { return target == errSessionUnavailable }
func (e *sessionUnavailableError) Unwrap() error        { return e.cause }

// errSessionGone is a session dropped after it was handed to a send (the
// hub said session_invalid to another send, a newer one replaced it, or
// this node rotated in an emergency): its key is zeroed, so no MAC is
// made with it, and the send takes the current session instead.
var errSessionGone = errors.New("the first-contact session was dropped")

// firstContactSession is one open session: the hub it is with, the hub's
// AID (the session key is bound to it), the id, the key and when it ends
// (unix ms, the hub's clock).
//
// A session is shared by every send to its hub, so its key is read and
// zeroed under mu: a MAC is never made from a key half zeroed, and a key
// is never zeroed while a MAC is being made with it.
type firstContactSession struct {
	hubAID string
	sid    []byte
	exp    time.Time
	mu     sync.RWMutex
	key    []byte // nil once zeroed
}

// usable reports whether s may carry a send made at now.
func (s *firstContactSession) usable(now time.Time) bool {
	return s != nil && now.Before(s.exp.Add(-sessionRenewBefore))
}

// mac is the session MAC of one send (core L8), or errSessionGone once s
// has been zeroed.
func (s *firstContactSession) mac(ts uint64, method, target, home string, body []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.key == nil {
		return nil, errSessionGone
	}
	return relayauth.SessionMAC(s.key, s.sid, ts, method, target, home, body)
}

// zero clears the session key; s makes no MAC after it.
func (s *firstContactSession) zero() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.key)
	s.key = nil
}

// sessionTable holds the current session of each hub (by hub URL), and
// serialises the opening of one per hub.
type sessionTable struct {
	mu    sync.Mutex
	byHub map[string]*firstContactSession
	// opening is the one opening in flight per hub: the others wait for it
	// rather than open sessions of their own.
	opening map[string]*sessionOpening
	// ok is, per hub, whether the last opening succeeded (/status
	// sender_privacy.session_ok).
	ok map[string]bool
	// gen counts dropSessions: an opening that began before the last one
	// was signed with a key this node has since retired, and its session
	// is zeroed instead of kept (SSR-D23).
	gen uint64
}

// sessionOpening is one opening in flight; done closes when it ended.
type sessionOpening struct {
	done chan struct{}
	s    *firstContactSession
	err  error
}

// firstContactSession returns the current session with the hub at hubURL,
// opening one when there is none or it is about to end.
func (d *Daemon) firstContactSession(ctx context.Context, hubURL string) (*firstContactSession, error) {
	key := strings.TrimRight(hubURL, "/")
	t := &d.hubc.sessions
	t.mu.Lock()
	if s := t.byHub[key]; s.usable(time.Now()) {
		t.mu.Unlock()
		return s, nil
	}
	if op := t.opening[key]; op != nil {
		t.mu.Unlock()
		select {
		case <-op.done:
			return op.s, op.err
		case <-ctx.Done():
			return nil, &sessionUnavailableError{ctx.Err()}
		}
	}
	op := &sessionOpening{done: make(chan struct{})}
	if t.opening == nil {
		t.opening = map[string]*sessionOpening{}
	}
	t.opening[key] = op
	gen := t.gen
	t.mu.Unlock()

	// The opening is every waiting send's, so it runs under a deadline of
	// its own and not under the first caller's context: one caller giving
	// up does not leave the others without a session.
	octx, cancel := context.WithTimeout(d.ctx, sessionOpenTimeout)
	op.s, op.err = d.openSession(octx, key)
	cancel()

	t.mu.Lock()
	delete(t.opening, key)
	if stale := op.err == nil && t.gen != gen; stale {
		// This node rotated in an emergency while the session opened. The
		// hub opened it, so session_ok keeps what it said.
		op.s.zero()
		op.s, op.err = nil, &sessionUnavailableError{errSessionGone}
	} else {
		if t.ok == nil {
			t.ok = map[string]bool{}
		}
		t.ok[key] = op.err == nil
	}
	if op.err == nil {
		if t.byHub == nil {
			t.byHub = map[string]*firstContactSession{}
		}
		if old := t.byHub[key]; old != nil {
			old.zero()
		}
		t.byHub[key] = op.s
	} else {
		d.countErr(counterSessionUnavailable, op.err)
	}
	t.mu.Unlock()
	close(op.done)
	return op.s, op.err
}

// openSession opens a session with the hub at hubURL (POST /relay/session,
// relayauth session): a one-time X25519 key, its public half to the hub,
// and the session key the hub sealed to it opened with the hub's AID and
// the session id the answer names. Any failure is a
// sessionUnavailableError.
func (d *Daemon) openSession(ctx context.Context, hubURL string) (*firstContactSession, error) {
	hubAID, _, err := d.hubIdentity(ctx, hubURL)
	if err != nil {
		return nil, &sessionUnavailableError{err}
	}
	esk, epk, err := relayauth.NewSessionKey()
	if err != nil {
		return nil, &sessionUnavailableError{err}
	}
	rep := d.hubSignedRaw(ctx, hubURL, http.MethodPost, hubapi.RelaySessionPath, relayauth.ActionSession,
		hubapi.SessionRequest{EPK: hubapi.B64(epk)})
	if rep.Status == 0 {
		return nil, &sessionUnavailableError{rep.Err}
	}
	if we := hubWireCheck(hubURL, rep); we != nil {
		return nil, &sessionUnavailableError{we}
	}
	if rep.Status != http.StatusOK {
		if rep.Status == http.StatusUnauthorized {
			// The witness refused this node's signature: what it holds of
			// this identity decides why (KEL D-27).
			d.triggerHubKELCheck(hubURL, triggerSession401)
		}
		return nil, &sessionUnavailableError{newHubError(hubapi.RelaySessionPath, rep)}
	}
	return openSessionAnswer(esk, hubAID, rep.Body, time.Now())
}

// openSessionAnswer reads a 200 answer to POST /relay/session (CORE-E5,
// CORE-E7: unknown members are ignored, every binary value strict) and
// opens the session key in it under esk, the hub's AID and the session id.
func openSessionAnswer(esk *ecdh.PrivateKey, hubAID string, body []byte, now time.Time) (*firstContactSession, error) {
	var out hubapi.SessionResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &sessionUnavailableError{fmt.Errorf("the session answer is not JSON: %w", err)}
	}
	sid, exp, enc, ct, err := out.Decode()
	if err != nil {
		return nil, &sessionUnavailableError{err}
	}
	k, err := relayauth.OpenSessionKey(esk, hubAID, sid, enc, ct)
	if err != nil {
		return nil, &sessionUnavailableError{err}
	}
	end := time.UnixMilli(int64(min(exp, uint64(1)<<62)))
	if lim := now.Add(sessionMaxLife); end.After(lim) {
		end = lim
	}
	return &firstContactSession{hubAID: hubAID, sid: sid, key: k, exp: end}, nil
}

// dropSession ends s as the current session of the hub at hubURL (the hub
// answered session_invalid): the next first contact opens another.
func (d *Daemon) dropSession(hubURL string, s *firstContactSession) {
	key := strings.TrimRight(hubURL, "/")
	t := &d.hubc.sessions
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byHub[key] == s {
		delete(t.byHub, key)
		s.zero()
	}
}

// dropSessions ends every session of this node and zeroes its keys
// (SSR-D23): part of an emergency rotation's first step, whose hub drops
// the sessions of the rotated key as well (SSR-H9). An opening in flight
// is signed with the retired key; its session is zeroed when it arrives.
func (d *Daemon) dropSessions() {
	t := &d.hubc.sessions
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gen++
	for k, s := range t.byHub {
		s.zero()
		delete(t.byHub, k)
	}
}

// sessionOK reports whether the last session opened with the hub at hubURL
// opened (/status sender_privacy.session_ok, SSR-U2); true before any was
// needed.
func (d *Daemon) sessionOK(hubURL string) bool {
	t := &d.hubc.sessions
	t.mu.Lock()
	defer t.mu.Unlock()
	ok, tried := t.ok[strings.TrimRight(hubURL, "/")]
	return !tried || ok
}
