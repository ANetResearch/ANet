package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// The one place a request to the hub is signed (relayauth v2, RC1).
//
// The daemon's own requests (hubSigned) and the payment module's account
// reads (paymentSeam.SignHubRequest) take their signing time from the same
// per-hub clock, so the two never send the same time and one is never
// refused as replayed by the other's; and a hub that answers ts_stale is
// obeyed the same way for both (HPP PAY-R10, PAY-R11, PAY-R12).

// errTSFloorRejected is a request not retried: the hub's floor lies beyond
// what the other hubs accept, so the hub is treated as unreachable for this
// request (PAY-R11). Counted as ts-floor-rejected.
var errTSFloorRejected = errors.New("anet: the hub refused this node's signing time and asked for one the other hubs would refuse; " +
	"the clock of this machine or the hub's is wrong")

// Counters of the RC1 client (PAY-R11).
const (
	counterTSStaleRetry    = "ts-stale-retry"
	counterTSFloorRejected = "ts-floor-rejected"
)

// tsRetryWindow and tsRetryWarn are doctor's clock.ts_retries: more than
// ten retries in one hour.
const (
	tsRetryWindow = time.Hour
	tsRetryWarn   = 10
	tsRetryKeep   = 1024
)

// tsRetryLog remembers when requests were signed again after a ts_stale
// answer, for the last hour.
type tsRetryLog struct {
	mu sync.Mutex
	at []int64
}

func (l *tsRetryLog) note(nowMS int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(nowMS)
	if len(l.at) >= tsRetryKeep {
		l.at = l.at[1:]
	}
	l.at = append(l.at, nowMS)
}

// recent is how many retries the last hour saw.
func (l *tsRetryLog) recent(nowMS int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(nowMS)
	return len(l.at)
}

func (l *tsRetryLog) pruneLocked(nowMS int64) {
	cut := nowMS - tsRetryWindow.Milliseconds()
	i := 0
	for i < len(l.at) && l.at[i] < cut {
		i++
	}
	l.at = l.at[i:]
}

// TSRetries is the number of times, in the last hour, that a hub found this
// node's signing time behind its floor and the request was signed again
// (doctor clock.ts_retries).
func (d *Daemon) TSRetries() int { return d.tsRetries.recent(time.Now().UnixMilli()) }

// signHubRequest signs req as this node for the hub hubAID: the action, both
// AIDs, the next time of this hub's clock, the method, the request target
// and the exact body bytes, in the X-ANet-* headers. Ed25519 signatures are
// deterministic and a hub accepts a time once, so two requests are never
// signed with the same time.
//
// Wall clock, not d.nowMS: the hub checks the time against its own clock,
// and the test clock only moves the sealed wire.
func (d *Daemon) signHubRequest(req *http.Request, hubAID, action string, body []byte) {
	ts := uint64(d.hubClock.Next(hubAID, time.Now().UnixMilli()))
	sig, seq := d.self.Sign(relayauth.PreimageV2(action, d.AID(), hubAID, ts, req.Method, req.URL.RequestURI(), body))
	req.Header.Set(hubapi.HeaderAID, d.AID())
	req.Header.Set(hubapi.HeaderTS, strconv.FormatUint(ts, 10))
	req.Header.Set(hubapi.HeaderSeq, strconv.FormatUint(seq, 10))
	req.Header.Set(hubapi.HeaderSig, relayauth.EncodeSig(sig))
}

// tsStaleFloor is the floor of a 401 ts_stale answer, and whether it is one.
func tsStaleFloor(status int, body []byte) (int64, bool) {
	if status != http.StatusUnauthorized {
		return 0, false
	}
	var b struct {
		Reason string `json:"reason"`
		Floor  int64  `json:"floor"`
	}
	if json.Unmarshal(body, &b) != nil || b.Reason != "ts_stale" {
		return 0, false
	}
	return b.Floor, true
}

// noteTSStale answers a ts_stale refusal from hubAID that named floor: the
// retry is counted, and the hub's clock moves above the floor unless the
// floor is beyond what other hubs accept (retry false, counted rejected).
func (d *Daemon) noteTSStale(hubAID string, floor int64) (retry bool) {
	now := time.Now().UnixMilli()
	d.count(counterTSStaleRetry)
	d.tsRetries.note(now)
	if !d.hubClock.Raise(hubAID, floor, now) {
		d.count(counterTSFloorRejected)
		return false
	}
	return true
}

// tsStale reads a hub's answer to a signed request: retry says it was
// ts_stale and the request is to be signed again once; rejected says it was
// ts_stale with a floor that is not adopted.
func (d *Daemon) tsStale(hubAID string, rep hubReply) (retry, rejected bool) {
	floor, ok := tsStaleFloor(rep.Status, rep.Body)
	if !ok {
		return false, false
	}
	if d.noteTSStale(hubAID, floor) {
		return true, false
	}
	return false, true
}

// SignHubRequest is module.PaymentSeam.SignHubRequest: the payment module's
// account reads are signed here, on this node's clock for that hub and
// through the same outbound gate as the daemon's own (KEL D-1). The request
// must be addressed to the configured hub.
func (s paymentSeam) SignHubRequest(req *http.Request, action string, body []byte) error {
	d := s.d
	hub := strings.TrimRight(d.config().HubURL, "/")
	if hub == "" {
		return errors.New("anet: no hub configured")
	}
	base, err := url.Parse(hub)
	if err != nil {
		return err
	}
	prefix := strings.TrimRight(base.EscapedPath(), "/")
	if req.URL.Scheme != base.Scheme || req.URL.Host != base.Host || !strings.HasPrefix(req.URL.EscapedPath(), prefix+"/") {
		return errors.New("anet: a module's hub request goes to this node's hub only")
	}
	if err := d.signedGate(req.Context(), strings.TrimPrefix(req.URL.EscapedPath(), prefix)); err != nil {
		return err
	}
	hubAID, _, err := d.hubIdentity(req.Context(), hub)
	if err != nil {
		return err
	}
	d.signHubRequest(req, hubAID, action, body)
	return nil
}

// NoteHubStale is module.PaymentSeam.NoteHubStale: the hub answered a
// request SignHubRequest signed with ts_stale and floor. True: the clock for
// that hub is above the floor and the request is to be signed again once.
func (s paymentSeam) NoteHubStale(floor int64) bool {
	d := s.d
	hub := strings.TrimRight(d.config().HubURL, "/")
	if hub == "" {
		return false
	}
	hubAID, _, err := d.hubIdentity(d.ctx, hub)
	if err != nil {
		return false
	}
	return d.noteTSStale(hubAID, floor)
}

// RegisteredAt is module.PaymentSeam.RegisteredAt: when this node first
// registered at its hub (unix ms), and false when this node does not know.
func (s paymentSeam) RegisteredAt() (int64, bool) {
	at := s.d.joinSnapshot().RegisteredAt
	return at, at > 0
}

// RefreshHubIdentity is module.PaymentSeam.RefreshHubIdentity: the hub's
// identity is read again and pinned again (it may only extend the pinned
// key history, pinHubIdentity), and the answer says whether the history is
// longer than it was.
func (s paymentSeam) RefreshHubIdentity() bool {
	d := s.d
	hub := strings.TrimRight(d.config().HubURL, "/")
	if hub == "" {
		return false
	}
	d.hubIDMu.Lock()
	old, had := d.hubIDs[hub]
	delete(d.hubIDs, hub)
	d.hubIDMu.Unlock()
	ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
	defer cancel()
	_, kel, err := d.hubIdentity(ctx, hub)
	if err != nil {
		if had {
			d.hubIDMu.Lock()
			d.hubIDs[hub] = old
			d.hubIDMu.Unlock()
		}
		return false
	}
	return len(kel) != len(old.kel)
}
