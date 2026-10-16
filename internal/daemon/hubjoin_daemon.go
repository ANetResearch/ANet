package daemon

// hubjoin_daemon.go runs the join machinery of hubjoin.go inside the daemon:
// the background loop (AD-3, AD-5), /hub-register (AD-1, LEC §2.3.2), the
// hub object of /status (§2.3.3), the send gate (AD-6) and hub-leave (AD-8).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/regpow"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/hubapi"
)

// joinAttemptTimeout bounds one join attempt, proof of work included (LEC
// AD-1; PAY-1: Solve takes the attempt's 60 seconds as its context). An
// attempt that runs out is a temporary failure, and the loop tries again.
const joinAttemptTimeout = 60 * time.Second

// hubJoin is the daemon's join loop state.
type hubJoin struct {
	mu sync.Mutex
	// rec is the configured hub's join record; nil when there is no hub.
	rec *joinRecord
	// kick wakes the loop for an attempt now; wake makes it look at the
	// record again (another attempt changed when the next one is due).
	kick, wake chan struct{}
	// hooks are the identity area's (KEL-9); tests set fakeJoinHooks.
	hooks    JoinHooks
	prepared preparedKELs
	// witness, when set, replaces the lookup of the identity's witness hub
	// AID (tests).
	witness func() string
	// kickAt is when the loop was last asked for an attempt, and okAt when
	// the last attempt whose registration was accepted began (unix ns): a
	// kick older than that attempt has been answered by it.
	kickAt, okAt atomic.Int64
	// probeAt is when the hub was last asked whether this node is still on
	// probation (unix ms; refreshProbation).
	probeAt atomic.Int64
	// waitHook, when set, is told each time the loop starts to wait. Tests
	// only, through setJoinWait (testhooks.go).
	waitHook atomic.Pointer[func(nextAttemptAt int64)]
}

// joinHooks are the join hooks now in force. Read under the join lock because
// a test replaces them (setJoinHooks) while the join loop of a daemon it
// started may be attempting.
func (d *Daemon) joinHooks() JoinHooks {
	d.join.mu.Lock()
	defer d.join.mu.Unlock()
	return d.join.hooks
}

// initJoin loads the join record of the configured hub (New). A record of
// another hub than config.json names, or one that cannot be read, is
// replaced by a fresh one for the configured hub: config.json's hub_url is
// what the operator last chose.
func (d *Daemon) initJoin(cfg Config) {
	d.join.kick = make(chan struct{}, 1)
	d.join.wake = make(chan struct{}, 1)
	if d.join.hooks == nil {
		d.join.hooks = identityJoinHooks{d}
		if f := testJoinHooks; f != nil {
			if h := f(d); h != nil {
				d.join.hooks = h
			}
		}
	}
	if cfg.HubURL == "" {
		return
	}
	rec, err := loadJoinRecord(d.layout)
	if err != nil {
		log.Printf("anet: %v; joining %s again", err, cfg.HubURL)
		rec = nil
	}
	if rec == nil || !sameHub(rec.Hub, cfg.HubURL) {
		rec = freshJoinRecord(cfg.HubURL, time.Now())
	}
	// Under the record's lock: loops New started before this one read it
	// already (the outbound gate does, KEL D-1).
	d.join.mu.Lock()
	d.join.rec = rec
	d.join.mu.Unlock()
}

// freshJoinRecord is the record of a hub nothing has been asked of yet: the
// first attempt is about to run, and until it answers the node is not
// registered there. The loop runs it at once.
func freshJoinRecord(hub string, now time.Time) *joinRecord {
	return &joinRecord{V: joinRecordVersion, Hub: hub, State: JoinRetrying, Reason: reasonRetrying,
		Why: whyUnreachable, Since: now.UnixMilli(), NextAttemptAt: now.UnixMilli()}
}

// sameHub compares two hub URLs as config.json and hub_join.json write them.
func sameHub(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}

// joinSnapshot is a copy of the record; nil when there is no hub.
func (d *Daemon) joinSnapshot() *joinRecord {
	d.join.mu.Lock()
	defer d.join.mu.Unlock()
	if d.join.rec == nil {
		return nil
	}
	r := *d.join.rec
	return &r
}

// joinRegistered reports whether this node is registered at its hub now.
func (d *Daemon) joinRegistered() bool {
	d.join.mu.Lock()
	defer d.join.mu.Unlock()
	return d.join.rec != nil && d.join.rec.State == JoinRegistered
}

// kickJoin asks the loop for an attempt now.
func (d *Daemon) kickJoin() {
	d.join.kickAt.Store(time.Now().UnixNano())
	select {
	case d.join.kick <- struct{}{}:
	default:
	}
}

// wakeJoin makes the loop read the record again: an attempt it did not make
// (hub-register's) scheduled the next one, or the record went away.
func (d *Daemon) wakeJoin() {
	select {
	case d.join.wake <- struct{}{}:
	default:
	}
}

// startJoinLoop runs the join loop for the life of the daemon. A node with
// a hub makes its first attempt at once: registering again at every start
// is how the hub learns what changed while the node was down (AD-5), and a
// node that was refused or waiting tries once more (AD-3: `anet update`
// clears a wire_too_old this way).
func (d *Daemon) startJoinLoop() {
	d.goBackground(func() { d.joinLoop(d.ctx) })
	if d.joinSnapshot() != nil {
		d.kickJoin()
	}
}

func (d *Daemon) joinLoop(ctx context.Context) {
	for {
		var due <-chan time.Time
		var timer *time.Timer
		var next int64
		if r := d.joinSnapshot(); r != nil && r.NextAttemptAt > 0 {
			next = r.NextAttemptAt
			timer = time.NewTimer(time.Until(time.UnixMilli(next)))
			due = timer.C
		}
		if f := d.testJoinWait(); f != nil {
			f(next) // tests: the loop waits now, for the attempt due at next
		}
		attempt, kicked := false, false
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-d.join.kick:
			attempt, kicked = true, true
		case <-d.join.wake:
		case <-due:
			attempt = true
		}
		if timer != nil {
			timer.Stop()
		}
		if attempt {
			d.joinAttempt(ctx, !kicked)
		}
	}
}

// joinAttempt makes one attempt at the configured hub; dueOnly skips it
// when another attempt (an explicit hub-register) moved the next one later
// meanwhile.
func (d *Daemon) joinAttempt(ctx context.Context, dueOnly bool) {
	actx, cancel := context.WithTimeout(ctx, joinAttemptTimeout)
	defer cancel()
	d.regMu.Lock()
	defer d.regMu.Unlock()
	r := d.joinSnapshot()
	if r == nil {
		return
	}
	if dueOnly && (r.NextAttemptAt == 0 || time.Now().UnixMilli() < r.NextAttemptAt) {
		return
	}
	if !dueOnly && d.join.okAt.Load() > d.join.kickAt.Load() {
		// A registration was accepted after the kick (an explicit
		// hub-register, say): what the kick asked for is done.
		return
	}
	cfg := d.config()
	if !sameHub(cfg.HubURL, r.Hub) {
		return // the hub changed under this attempt; the new one has its own
	}
	caps := withServedCapabilities(cfg.Caps, d.providers)
	if out := d.attemptLocked(actx, r.Hub, cfg.Name, caps); out.State == JoinRegistered && !sameStrings(cfg.Caps, caps) {
		// What the hub was sent, which is only the public ones
		// (registerWithKEL, an internal note item). The served list printed here read
		// as if the private ones had been published (an internal note).
		log.Printf("anet: refreshed capabilities at %s: %v", r.Hub, d.publicOnly(caps))
	}
}

// sameStrings reports whether two capability lists have the same members.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
		if seen[x] < 0 {
			return false
		}
	}
	return true
}

// attemptLocked makes one join attempt at hubURL with d.regMu held, and
// keeps its result when hubURL is the configured hub. The registration
// carries name and caps.
func (d *Daemon) attemptLocked(ctx context.Context, hubURL, name string, caps []string) joinRecord {
	// The join flow's requests pass the outbound gate (KEL D-1) and reach a
	// hub stopped for another wire, to check it again (core CORE-V1).
	ctx = withJoinFlow(ctx)
	// When this attempt began: a registration it gets accepted answers the
	// kicks made before it, not those made while it ran (their inputs,
	// a changed card say, may be newer than what it sent).
	start := time.Now().UnixNano()
	cur := d.joinSnapshot()
	var r joinRecord
	if cur != nil && sameHub(cur.Hub, hubURL) {
		r = *cur
	} else {
		r = *freshJoinRecord(hubURL, time.Now())
	}
	if !d.joinMayRegister() {
		// Nothing is registered for a stopped or deactivated identity, nor
		// while the publication sequence owns the registration.
		return r
	}
	hubAID, _, err := d.hubIdentity(ctx, hubURL)
	if err != nil {
		// No hub identity, no signed request: the hub cannot be reached
		// (or does not serve one) right now, or it speaks another wire.
		a := transientAnswer(phaseRegister, whyUnreachable)
		var we *hubWireError
		if errors.As(err, &we) {
			a = joinAnswer{phase: phaseRegister, kind: ansRefused, refusal: reasonWireTooOld, code: "wire_too_old",
				version: d.wireTooOldVersion(hubURL, we)}
			if we.hubOlder {
				a.refusal, a.code, a.version = reasonWireTooNew, "wire_too_new", ""
			}
		}
		r, _ = joinStep(r, a, time.Now())
		d.keepJoinRecord(hubURL, r)
		return r
	}
	r.HubAID = hubAID
	// The join flow checked the hub's identity, and it speaks this wire.
	d.clearHubWire(hubURL)
	kelB, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		r, _ = joinStep(r, transientAnswer(phaseRegister, whyLocal), time.Now())
		d.keepJoinRecord(hubURL, r)
		return r
	}
	env := joinEnv{
		hub:       &httpJoinHub{d: d, hubURL: hubURL, name: name, caps: caps, ownKEL: kelB},
		hooks:     d.joinHooks(),
		hubURL:    hubURL,
		hubAID:    hubAID,
		aid:       d.AID(),
		witnessed: d.identityWitness() == hubAID,
		ownKEL:    kelB,
		tip:       uint64(len(d.self.Accepted()) - 1),
		prepared:  &d.join.prepared,
		solve: func(ctx context.Context, ch powChallenge) ([]byte, error) {
			return regpow.Solve(ctx, hubAID, d.AID(), ch.Seed, ch.Bits)
		},
		now:  time.Now,
		logf: log.Printf,
	}
	out := runJoinAttempt(ctx, r, env)
	if out.last != nil {
		if out.prepared {
			// What the hub said of the designation is the identity's to
			// read (KEL D-14a, D-36).
			d.afterDesignationAnswer(*out.last)
		} else if code := out.last.code; isAuthRefusal(code) || code == "kel_fork" || code == "kel_rollback" {
			// The witness refuses this node's key or holds another record
			// of it: what it holds decides why (KEL D-27).
			d.triggerHubKELCheck(hubURL, triggerRegister)
		}
	}
	if out.reg != nil {
		d.join.okAt.Store(start)
		// What the hub said of the key state, after Adopt made the rot the
		// tip a notice about it confirms (KEL D-13 step 4, D-50).
		d.noteHubNotice(out.reg.KeyStateNotice, out.reg.KeyStateNoticeError)
	}
	d.keepJoinRecord(hubURL, out.rec)
	return out.rec
}

// keepJoinRecord stores r as the configured hub's record when hubURL is the
// configured hub, writes hub_join.json, and records a change of the state
// or its sentence on the evidence chain (AD-7).
func (d *Daemon) keepJoinRecord(hubURL string, r joinRecord) {
	if !sameHub(d.config().HubURL, hubURL) {
		return
	}
	d.join.mu.Lock()
	prev := d.join.rec
	d.join.mu.Unlock()
	if prev != nil && !sameHub(prev.Hub, hubURL) {
		return
	}
	// The file and the evidence first, then the state others read: a
	// reader that sees the new state finds its record on the chain.
	cp := r
	if cp.WitnessRot != nil && d.self != nil && d.identityWitness() != "" {
		// The designation is the accepted tip: its material is spent (KEL
		// D-13 step 4).
		cp.WitnessRot = nil
	}
	if err := saveJoinRecord(d.layout, &cp); err != nil {
		log.Printf("anet: save hub_join.json: %v", err)
	}
	if prev == nil || prev.State != r.State || prev.Reason != r.Reason {
		d.recordJoinChange(r)
	}
	d.join.mu.Lock()
	if cur := d.join.rec; cur == nil || sameHub(cur.Hub, hubURL) {
		d.join.rec = &cp
	}
	d.join.mu.Unlock()
	d.wakeJoin()
}

// recordJoinChange writes anet.hub.admission (LEC §2.5, AD-7).
func (d *Daemon) recordJoinChange(r joinRecord) {
	if d.ledger == nil {
		return
	}
	ev := map[string]any{"hub": r.Hub, "state": r.State}
	if r.Reason != "" {
		ev["reason"] = r.Reason
	}
	if _, err := d.ledger.Append(evtypes.HubAdmission, ev); err != nil {
		log.Printf("anet: evidence %s: %v", evtypes.HubAdmission, err)
	}
}

// identityWitness is the AID of the identity's witness hub, "" while it has
// none (KEL §2.2: the tip's witness). The tip is the one the witness
// accepted: a designation being published names a hub that is not the
// witness until it accepts it (KEL §4.3.1 row 3), so its join still goes
// through the hooks, which adopt it.
func (d *Daemon) identityWitness() string {
	if d.join.witness != nil {
		return d.join.witness()
	}
	states, err := identity.Replay(d.self.Accepted())
	if err != nil || len(states) == 0 {
		return ""
	}
	return states[len(states)-1].Witness
}

// joinMayRegister reports whether the join loop may register now: the
// identity is neither stopped nor deactivated (KEL D-29, D-46), and no rot
// or dip is being published, which the publication sequence registers
// itself (identity_ops.go).
func (d *Daemon) joinMayRegister() bool {
	if d.self == nil {
		return true
	}
	if d.self.State() != identity.StateOK {
		return false
	}
	if p, ok := d.self.Pending(); ok && p.Kind != identity.PendingWitness {
		return false
	}
	return true
}

// --- the hub over HTTP ---

// httpJoinHub is the hub as the daemon talks to it during a join.
type httpJoinHub struct {
	d      *Daemon
	hubURL string
	name   string
	caps   []string
	ownKEL []byte
}

// checkOnly is /register with check_only: the AID and its KEL, nothing
// else (HN-2: no enc_keys; a proof of work only when one was asked for).
func (h *httpJoinHub) checkOnly(ctx context.Context, pow *powProof) hubReply {
	body := hubapi.RegisterRequest{AID: h.d.AID(), KEL: base64.RawURLEncoding.EncodeToString(h.ownKEL),
		CheckOnly: true, PoW: wirePow(pow)}
	return h.d.hubSignedRaw(ctx, h.hubURL, http.MethodPost, "/register", relayauth.ActionRegister, body)
}

// register is /register with kel, and what follows an accepted one: the
// card's and the key set's answers (registerWithKEL).
func (h *httpJoinHub) register(ctx context.Context, kel []byte, pow *powProof) hubReply {
	return h.d.registerWithKEL(ctx, h.hubURL, h.name, h.caps, kel, pow)
}

// wirePow is /register's pow member (base64url, hub-payment-privacy §2.2).
func wirePow(p *powProof) *hubapi.RegisterPoW {
	if p == nil {
		return nil
	}
	return &hubapi.RegisterPoW{Seed: base64.RawURLEncoding.EncodeToString(p.Seed[:]),
		Nonce: base64.RawURLEncoding.EncodeToString(p.Nonce)}
}

// --- what the join state says (LEC §2.3.2, §2.3.3) ---

// hubView is the hub object of /status and the state part of the
// /hub-register answer.
type hubView struct {
	URL           string `json:"url"`
	State         string `json:"state"`
	Reason        string `json:"reason"`
	Message       string `json:"message"`
	Next          string `json:"next,omitempty"`
	Since         string `json:"since,omitempty"`
	NextAttemptAt string `json:"next_attempt_at,omitempty"`
	HubCode       string `json:"hub_code,omitempty"`
	HubError      string `json:"hub_error,omitempty"`

	// key and args are the sentence the view says, for a refusal.
	key        string
	args       a2ashape.Args
	retryAfter time.Duration
}

// viewOfJoin is what a record says now, in lang; nil is no hub (none).
func (d *Daemon) viewOfJoin(r *joinRecord, now time.Time, lang a2ashape.Language) hubView {
	return joinViewOf(r, now, lang, d.AID(), d.started, d.layout.Root)
}

// HubStatus is the hub object of /status, as HubView and ReadHubView give it.
type HubStatus = hubView

// ReadHubView is the hub object of /status read from the data directory, for a process that is not the
// daemon (doctor): the same sentences, from hub_join.json alone. A record that cannot be read is an
// error; no record is no hub.
func ReadHubView(l Layout, lang a2ashape.Language) (HubStatus, error) {
	r, err := loadJoinRecord(l)
	if err != nil {
		return hubView{}, err
	}
	return joinViewOf(r, time.Now(), lang, ReadIdentityAID(l), time.Time{}, l.Root), nil
}

// joinViewOf is viewOfJoin for any process: aid and root name the identity, started the moment the
// daemon did (zero: not shown).
func joinViewOf(r *joinRecord, now time.Time, lang a2ashape.Language, aid string, started time.Time, root string) hubView {
	if r == nil {
		v := hubView{State: JoinNone, Reason: reasonNoHub, key: reasonNoHub}
		if !started.IsZero() {
			v.Since = started.UTC().Format(time.RFC3339)
		}
		v.Message = a2ashape.Sentence(v.key, lang, nil)
		v.Next = a2ashape.Next(v.key, lang, nil)
		return v
	}
	key := r.Reason
	args := a2ashape.Args{"hub": r.Hub, "aid": aid, "now": now}
	switch key {
	case reasonProbation:
		if r.ProbationUntil <= uint64(now.UnixMilli()) {
			key = reasonRegistered
		} else {
			args["time"] = time.UnixMilli(int64(r.ProbationUntil))
		}
	case reasonRetrying:
		args["why"] = r.Why
		args["time"] = time.UnixMilli(r.NextAttemptAt)
	case reasonRetryingQuota:
		args["time"] = time.UnixMilli(r.NextAttemptAt)
	case reasonHubRefused:
		args["code"] = r.HubCode
	case reasonWireTooOld:
		if r.Version != "" {
			args["version"] = r.Version
		}
	case reasonStopped:
		if n := IdentityNameForDir(root); n != "" {
			args["name"] = n
		}
	}
	v := hubView{URL: r.Hub, State: r.State, Reason: key, key: key, args: args,
		Message: a2ashape.Sentence(key, lang, args), Next: a2ashape.Next(key, lang, args),
		HubCode: r.HubCode, HubError: r.HubError}
	if r.Since > 0 {
		v.Since = time.UnixMilli(r.Since).UTC().Format(time.RFC3339)
	}
	if r.NextAttemptAt > 0 && (r.State == JoinPendingAdmission || r.State == JoinRetrying) {
		at := time.UnixMilli(r.NextAttemptAt)
		v.NextAttemptAt = at.UTC().Format(time.RFC3339)
		v.retryAfter = at.Sub(now)
	}
	if r.State != JoinRefused || key != reasonHubRefused {
		v.HubCode, v.HubError = "", ""
	}
	return v
}

// probationRecheck is the least time between two questions to the hub about
// a probation the join record still shows.
const probationRecheck = 30 * time.Second

// refreshProbation brings the join record's probation up to date. The record
// holds the end of the probation the hub named when this node registered; an
// operator who admits the account at the hub ends it earlier, and nothing in
// the record would say so. While the record shows a probation, the account
// is read off the hub (at most once in probationRecheck, within a few
// seconds, and a hub that cannot be reached leaves the record as it is) and
// a probation the hub no longer reports is taken out of the record.
func (d *Daemon) refreshProbation(ctx context.Context) {
	r := d.joinSnapshot()
	now := time.Now()
	if r == nil || r.State != JoinRegistered || r.Reason != reasonProbation || r.ProbationUntil <= uint64(now.UnixMilli()) {
		return
	}
	last := d.join.probeAt.Load()
	if now.UnixMilli()-last < probationRecheck.Milliseconds() || !d.join.probeAt.CompareAndSwap(last, now.UnixMilli()) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	acct, err := d.HubAccount(ctx)
	if err != nil {
		return
	}
	var until uint64
	if p := acct.ProbationUntil; p != nil && *p > uint64(now.UnixMilli()) && *p <= math.MaxInt64 {
		until = *p
	}
	if until == r.ProbationUntil {
		return
	}
	cp := *r
	cp.ProbationUntil = until
	if until == 0 {
		cp.set(JoinRegistered, reasonRegistered, now)
	}
	d.keepJoinRecord(r.Hub, cp)
}

// HubView is the hub object of /status (§2.3.3), in this process's
// language.
func (d *Daemon) HubView() hubView {
	return d.viewOfJoin(d.joinSnapshot(), time.Now(), a2ashape.Lang())
}

// refusal is the view as the refusal a send gets (AD-6): the state's code
// and sentence; a passing state with the wait until the next attempt
// (at least a second).
func (v hubView) refusal() *a2ashape.Refusal {
	ref := a2ashape.Refuse(v.key, v.args)
	ref.HubCode = v.HubCode
	if v.State == JoinPendingAdmission || v.State == JoinRetrying {
		ref.RetryAfter = v.retryAfter
		if ref.RetryAfter < time.Second {
			ref.RetryAfter = time.Second
		}
	}
	return ref
}

// sendGate is nil when this node may send now, and otherwise the refusal
// every send surface gives (LEC AD-6, ux UX-R7): until the hub registered
// this node, a send fails at once with the join state's sentence and
// nothing is queued. Messages already queued are the outbound gate's.
//
// An identity that is stopped or deactivated is refused first, with its own
// code (KEL D-3): it says the more fundamental thing, which no hub can
// change, so it wins when both gates are closed (ux UX-R7). A key event
// being published or a stale notice is not a reason to refuse: the send is
// queued and waits for the outbound gate (KEL D-4).
func (d *Daemon) sendGate() *a2ashape.Refusal {
	if ref := d.identitySendRefusal(); ref != nil {
		return ref
	}
	v := d.viewOfJoin(d.joinSnapshot(), time.Now(), a2ashape.Lang())
	if v.State == JoinRegistered {
		return nil
	}
	return v.refusal()
}

// writeRefusal answers a control-plane request with a refusal of the reason
// table (ux UX-R9): {error, reason, next?, hub_code?}, in this process's
// language. A passing state carries Retry-After.
func writeRefusal(w http.ResponseWriter, status int, ref *a2ashape.Refusal) {
	if ref.RetryAfter > 0 {
		secs := int((ref.RetryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	writeJSON(w, status, ref.ControlBody(a2ashape.Lang()))
}

// passSendGate reports whether a send may go on, and answers the request
// with the refusal when it may not (AD-6). A request is checked for its own
// form first: a malformed one is told what is wrong with it.
func (d *Daemon) passSendGate(w http.ResponseWriter) bool {
	if ref := d.sendGate(); ref != nil {
		writeGateRefusal(w, ref)
		return false
	}
	return true
}

// writeGateRefusal answers a refused send (AD-6): 409 and the body.
func writeGateRefusal(w http.ResponseWriter, ref *a2ashape.Refusal) {
	writeRefusal(w, http.StatusConflict, ref)
}

// --- /hub-register (AD-1, LEC §2.3.2) ---

// hubRegisterReq is the /hub-register body.
type hubRegisterReq struct {
	Hub  string   `json:"hub"`
	Name string   `json:"name,omitempty"`
	Caps []string `json:"caps,omitempty"`
	// NextDigest is the commitment the witness designation will carry
	// (KEL D-10, D-11): 32 bytes, standard base64.
	NextDigest []byte `json:"next_digest,omitempty"`
	// Unprotected: NextDigest comes from the recovery file without a
	// passphrase in the data directory (KEL D-11). It goes with
	// NextDigest only.
	Unprotected bool `json:"unprotected,omitempty"`
	DryRun      bool `json:"dry_run,omitempty"`
}

// hubRegisterResp is the /hub-register answer to a join, an update or a
// refusal by the hub (§2.3.2).
type hubRegisterResp struct {
	Hub           string        `json:"hub"`
	AID           string        `json:"aid"`
	State         string        `json:"state"`
	Reason        string        `json:"reason"`
	Message       string        `json:"message"`
	Next          string        `json:"next,omitempty"`
	NextAttemptAt string        `json:"next_attempt_at,omitempty"`
	Recovery      *recoveryView `json:"recovery,omitempty"`
	HubCode       string        `json:"hub_code,omitempty"`
	HubError      string        `json:"hub_error,omitempty"`
}

// recoveryView is the identity's recovery state as /hub-register and
// /identity/status give it (KEL D-15): shown while the identity has no
// recovery passphrase.
type recoveryView struct {
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	Next    string `json:"next,omitempty"`
}

// hubDryRunResp is the /hub-register answer to dry_run (§2.3.2).
type hubDryRunResp struct {
	Hub            string `json:"hub"`
	HubAID         string `json:"hub_aid"`
	Action         string `json:"action"`
	RecoveryNeeded bool   `json:"recovery_needed"`
}

// The actions of dry_run.
const (
	joinActionJoin   = "join"
	joinActionUpdate = "update"
	joinActionSwitch = "switch"
)

// joinRequest is a join as hub-register asks for it.
type joinRequest struct {
	Hub  string
	Name string
	Caps []string
	// NextDigest and Unprotected as in hubRegisterReq.
	NextDigest  []byte
	Unprotected bool
	// requireDigest: a first join must bring next_digest (the control
	// plane; LEC §2.3.2). The Go API of tests registers without one.
	requireDigest bool
}

// joinError is a refusal of /hub-register with its HTTP status.
type joinError struct {
	status int
	ref    *a2ashape.Refusal
}

// hubContact is what GET /hub/identity said: the hub's AID, or why there is
// no answer to go on.
type hubContact struct {
	hubAID string
}

// contactHub asks hubURL for its identity, afresh: an address that gives no
// HTTP answer, or no hub identity, is hub_unreachable (UX-O1; nothing is
// written); a hub that states another wire is wire_too_old or
// wire_too_new (CORE-V1). With pin the hub's KEL is pinned as every signed
// call pins it (hubIdentity); without it (dry_run, which writes nothing,
// LEC §2.3.2) it is only checked to replay to the hub's AID.
func (d *Daemon) contactHub(ctx context.Context, hubURL string, pin bool) (hubContact, *joinError) {
	unreachable := &joinError{http.StatusBadGateway, a2ashape.Refuse(reasonHubUnreachable, a2ashape.Args{"hub": hubURL})}
	ctx, cancel := context.WithTimeout(withJoinFlow(ctx), hubCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hubURL, "/")+"/hub/identity", nil)
	if err != nil {
		return hubContact{}, unreachable
	}
	rep := d.hubExchange(req, hubURL, "/hub/identity", -1)
	if rep.Status == 0 {
		return hubContact{}, unreachable
	}
	wireTooOld := func(version string) *joinError {
		args := a2ashape.Args{"hub": hubURL}
		if version != "" {
			args["version"] = version
		}
		return &joinError{http.StatusConflict, a2ashape.Refuse(reasonWireTooOld, args)}
	}
	if rep.Status == http.StatusUpgradeRequired {
		// The hub refuses this node's wire even for its identity: the 426
		// names what it needs (required_anet, else required_wire).
		return hubContact{}, wireTooOld(hubWireCheck(hubURL, rep).version)
	}
	var out hubapi.HubIdentity
	if rep.Status != http.StatusOK || json.Unmarshal(rep.Body, &out) != nil || out.AID == "" {
		// An answer that is not a hub's identity: not a hub at this
		// address, as far as this node can tell.
		return hubContact{}, unreachable
	}
	// The wire the hub states: its X-ANet-Wire, else the identity's wire
	// member; neither is a hub older than wire 3 (CORE-V1).
	wire := -1
	if v, err := strconv.Atoi(rep.Wire); err == nil {
		wire = v
	} else if rep.Wire == "" && out.Wire != nil {
		wire = *out.Wire
	}
	switch {
	case wire > hubapi.WireVersion:
		version := ""
		if wire < 10000 {
			version = wireVersionText(wire)
		}
		return hubContact{}, wireTooOld(version)
	case wire < hubapi.WireVersion:
		return hubContact{}, &joinError{http.StatusConflict, a2ashape.Refuse(reasonWireTooNew, a2ashape.Args{"hub": hubURL})}
	}
	if !pin {
		if !hubKELReplays(out) {
			return hubContact{}, unreachable
		}
		return hubContact{hubAID: out.AID}, nil
	}
	// Pinned as every signed call pins it (hubIdentity), from this answer.
	aid, _, err := d.pinHubIdentity(hubURL, out)
	if err != nil {
		return hubContact{}, unreachable
	}
	d.clearHubWire(hubURL)
	return hubContact{hubAID: aid}, nil
}

// hubKELReplays reports whether a /hub/identity answer's KEL reads and
// replays to its AID: what pinHubIdentity checks before it writes anything.
func hubKELReplays(h hubapi.HubIdentity) bool {
	raw, err := hubapi.DecodeB64(h.KEL, -1)
	if err != nil || h.AID == "" {
		return false
	}
	kel, err := seal.ParseKEL(raw)
	if err != nil {
		return false
	}
	got, err := replayedAID(kel)
	return err == nil && got == h.AID
}

// joinAction is what hub-register at a hub of AID hubAID does (§2.3.2).
func (d *Daemon) joinAction(hubAID string) string {
	switch w := d.identityWitness(); {
	case w == "":
		return joinActionJoin
	case w == hubAID:
		return joinActionUpdate
	default:
		return joinActionSwitch
	}
}

// hasWitnessRot reports whether the record of hubURL holds witness_rot.
func (d *Daemon) hasWitnessRot(hubURL string) bool {
	r := d.joinSnapshot()
	return r != nil && sameHub(r.Hub, hubURL) && r.WitnessRot != nil
}

// dryRunJoin is /hub-register with dry_run: GET /hub/identity and nothing
// else; no file written, no signed request.
func (d *Daemon) dryRunJoin(ctx context.Context, hubURL string) (hubDryRunResp, *joinError) {
	c, jerr := d.contactHub(ctx, hubURL, false)
	if jerr != nil {
		return hubDryRunResp{}, jerr
	}
	action := d.joinAction(c.hubAID)
	return hubDryRunResp{Hub: hubURL, HubAID: c.hubAID, Action: action,
		RecoveryNeeded: action == joinActionJoin && !d.hasWitnessRot(hubURL)}, nil
}

// joinHub is hub-register (AD-1): the reachability check, then config.json
// and hub_join.json, then one attempt of at most 60 seconds, after which
// the loop goes on in the background. It answers with the state the
// attempt left.
func (d *Daemon) joinHub(ctx context.Context, req joinRequest) (hubRegisterResp, *joinError) {
	ctx = withJoinFlow(ctx)
	c, jerr := d.contactHub(ctx, req.Hub, true)
	if jerr != nil {
		return hubRegisterResp{}, jerr
	}
	action := d.joinAction(c.hubAID)
	switch action {
	case joinActionSwitch:
		// Switching hubs is a key rotation (KEL L-4, /identity/rotate).
		return hubRegisterResp{}, &joinError{http.StatusConflict,
			a2ashape.Refuse(reasonSwitch, a2ashape.Args{"hub": req.Hub})}
	case joinActionJoin:
		if req.requireDigest && req.NextDigest == nil && !d.hasWitnessRot(req.Hub) {
			return hubRegisterResp{}, &joinError{http.StatusBadRequest, a2ashape.Refuse(reasonConfigInvalid,
				a2ashape.Args{"key": "next_digest", "rule": ruleText("首次接入 hub 时必须带上(anet hub-register 会自动带上)",
					"a first join must carry it (anet hub-register adds it)")})}
		}
		if d.designationFixed(c.hubAID, req.NextDigest) {
			// The designation is built: its hub and its commitment are fixed
			// until it is accepted or refused for good (KEL D-14).
			return hubRegisterResp{}, &joinError{http.StatusConflict, a2ashape.Refuse(reasonRotationPending, nil)}
		}
	default:
		// The identity has its witness: hub-register only updates the
		// registration, and a commitment in the request is not used (KEL
		// D-14).
		req.NextDigest, req.Unprotected = nil, false
	}
	caps := withServedCapabilities(req.Caps, d.providers)

	d.regMu.Lock()
	// local_commit goes first (KEL D-11), under regMu like the join
	// attempts: the witness designation is built in one, so whether it has
	// been built cannot change between keepLocalCommit's look and its write.
	if action == joinActionJoin {
		if err := d.keepLocalCommit(req); err != nil {
			d.regMu.Unlock()
			log.Printf("anet: save local_commit: %v", err)
			return hubRegisterResp{}, &joinError{http.StatusInternalServerError, a2ashape.Refuse(reasonConfigInvalid,
				a2ashape.Args{"key": "next_digest", "rule": ruleText("无法写入身份文件 identity.kel", "identity.kel could not be written")})}
		}
	}
	prevHub, err := d.adoptJoinTarget(req, c.hubAID, caps)
	if err != nil {
		d.regMu.Unlock()
		return hubRegisterResp{}, &joinError{http.StatusInternalServerError, a2ashape.Refuse(reasonConfigInvalid,
			a2ashape.Args{"key": "hub_url", "rule": ruleText("无法写入 config.json", "config.json could not be written")})}
	}
	if !sameHub(prevHub, req.Hub) || !d.relayRunning() {
		d.startRelayLoop(req.Hub)
	}
	actx, cancel := context.WithTimeout(d.ctx, joinAttemptTimeout)
	cfg := d.config()
	rec := d.attemptLocked(actx, req.Hub, cfg.Name, caps)
	cancel()
	d.regMu.Unlock()

	if rec.State == JoinRegistered {
		if !sameHub(prevHub, req.Hub) && d.CardPublicationStatus().Sent {
			// The payment module learns the hub, and so the ledger its
			// prices are on, from the config, written only now: publish
			// once more with it in place (a card that comes out the same
			// is answered "unchanged").
			d.cardInputsChanged()
		}
		// A fresh registration keeps the agent's self-description.
		if cfg.Summary != "" || cfg.Readme != "" {
			pctx, pcancel := context.WithTimeout(ctx, hubCallTimeout)
			if err := d.PublishProfile(pctx, req.Hub, cfg.Summary, cfg.Readme); err != nil {
				log.Printf("anet: publish the profile at %s: %v", req.Hub, err)
			}
			pcancel()
		}
	}
	d.writeRegistry() // the identity entry shows the (possibly new) name
	return d.registerAnswer(&rec, a2ashape.Lang()), nil
}

// designationFixed reports whether a witness designation is built for
// another hub than hubAID, or with another commitment than next (when
// given): after it is built, only its own hub and commitment can be asked
// for (KEL D-14, M35).
func (d *Daemon) designationFixed(hubAID string, next []byte) bool {
	p, ok := d.self.Pending()
	if !ok || p.Kind != identity.PendingWitness {
		return false
	}
	ev, err := pendingEvent(p)
	if err != nil || ev == nil {
		return true
	}
	if len(ev.Event.Witnesses) != 1 || ev.Event.Witnesses[0] != hubAID {
		return true
	}
	return next != nil && !bytes.Equal(next, ev.Event.NextDigest)
}

// keepLocalCommit writes local_commit, identity file key 7, for a first
// join whose next_digest comes from the recovery file without a passphrase
// (KEL D-11): before witness_rot, so the identity reads as unprotected from
// the moment the commitment can be used. Once the witness designation is
// built its commitment is fixed (KEL D-14), and nothing is written. The
// caller holds d.regMu.
func (d *Daemon) keepLocalCommit(req joinRequest) error {
	if !req.Unprotected || req.NextDigest == nil {
		return nil
	}
	if _, building := d.self.Pending(); building {
		return nil
	}
	return d.saveLocalCommit(req.NextDigest)
}

// adoptJoinTarget writes config.json's hub_url, name and caps and the hub's
// join record (AD-1), and returns the hub configured before. A join of the
// hub already configured keeps its record and starts its backoff over
// (AD-3: running hub-register again retries at once).
func (d *Daemon) adoptJoinTarget(req joinRequest, hubAID string, caps []string) (string, error) {
	var prevHub string
	set := func(c *Config) {
		c.HubURL = req.Hub
		if req.Name != "" {
			c.Name = req.Name
		}
		if caps != nil {
			c.Caps = caps
		}
	}
	// updateConfig carries `without` and the blocks the kernel does not own
	// (modules.*, auto_reply) over from the disk (K-2), so a block hand-added
	// while this daemon ran is not lost by this write.
	_, err := d.updateConfig(func(c *Config) error {
		prevHub = c.HubURL
		set(c)
		return nil
	}, set)
	if err != nil {
		return prevHub, err
	}
	now := time.Now()
	d.join.mu.Lock()
	r := d.join.rec
	switch {
	case r == nil || !sameHub(r.Hub, req.Hub):
		r = freshJoinRecord(req.Hub, now)
	case r.State == JoinRefused:
		// Asked again after a refusal: the record starts over, so what it
		// says is the answer to this attempt — a temporary failure is
		// hub_retrying, not a refusal with a retry behind it (KEL-9, EXE-13
		// note). The commitment stays.
		fresh := freshJoinRecord(req.Hub, now)
		fresh.WitnessRot = r.WitnessRot
		r = fresh
	default:
		cp := *r
		r = &cp
		r.Attempts = 0
	}
	r.HubAID = hubAID
	if req.NextDigest != nil {
		// Before the rot is built a new commitment replaces the old one
		// (KEL D-14); with unprotected, joinHub wrote local_commit first
		// (KEL D-11). After it is built joinHub refused a new one
		// (rotation_pending, designationFixed).
		r.WitnessRot = &WitnessRot{NextDigest: append([]byte(nil), req.NextDigest...), HubAID: hubAID}
	}
	d.join.rec = r
	cp := *r
	d.join.mu.Unlock()
	d.wakeJoin()
	if err := saveJoinRecord(d.layout, &cp); err != nil {
		return prevHub, err
	}
	return prevHub, nil
}

// registerAnswer is the /hub-register answer for the state r is in: the
// state's sentence and, while the identity has no recovery passphrase,
// the reminder (the CLI prints it first).
func (d *Daemon) registerAnswer(r *joinRecord, lang a2ashape.Language) hubRegisterResp {
	return d.registerAnswerAt(r, d.recoveryState(), lang, time.Now())
}

// registerAnswerAt is registerAnswer at now, for the recovery attribute rec.
func (d *Daemon) registerAnswerAt(r *joinRecord, rec RecoveryState, lang a2ashape.Language, now time.Time) hubRegisterResp {
	v := d.viewOfJoin(r, now, lang)
	return hubRegisterResp{Hub: v.URL, AID: d.AID(), State: v.State, Reason: v.Reason, Message: v.Message,
		Next: v.Next, NextAttemptAt: v.NextAttemptAt, Recovery: recoveryForJoin(rec, d.identityState().State, lang),
		HubCode: v.HubCode, HubError: v.HubError}
}

// recoveryForJoin is the recovery object of the /hub-register answer (KEL
// D-15): there while the identity has no recovery passphrase, decided by
// recoveryStateOf from local_commit and the KEL, whatever the request said.
// An update of a registration made without a terminal carries it too.
func recoveryForJoin(rec RecoveryState, identityState string, lang a2ashape.Language) *recoveryView {
	if rec.reminderKey(identityState) == "" {
		return nil
	}
	v := rec.view(identityState, lang)
	return &v
}

// ruleText is config_invalid's {rule} in this process's language.
func ruleText(zh, en string) string {
	if a2ashape.Lang() == a2ashape.Chinese {
		return zh
	}
	return en
}

// relayRunning reports whether the relay poll loop runs.
func (d *Daemon) relayRunning() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.relayStop != nil
}

// --- hub-leave (AD-8) ---

// leaveHub is hub-leave for target (the configured hub when empty):
//   - the identity's witness hub: refused, witness_hub_leave. Which hub
//     target is comes from its AID — the join record's, the one this
//     process learned, or the hub's answer now — compared with the witness
//     the identity's KEL names; when it cannot be told (no record, and the
//     hub does not answer), leaving is refused, hub_unreachable.leave: a
//     node never leaves its witness by not knowing it is that hub;
//   - the configured hub, not registered yet: the join stops, hub_join.json
//     and hub_url go, the state is none;
//   - the configured hub, registered (an identity without a witness): the
//     node deregisters there and forgets the hub;
//   - another hub (the one before a switch): deregistered there; the
//     configured hub's join is not touched (core X-KEL-7).
func (d *Daemon) leaveHub(ctx context.Context, target string) (map[string]any, *joinError, error) {
	ctx = withJoinFlow(ctx) // hub-leave is the join flow's (LEC AD-8)
	cfg := d.config()
	if strings.TrimSpace(target) == "" {
		target = cfg.HubURL
	}
	target = strings.TrimRight(strings.TrimSpace(target), "/")
	if target == "" {
		return nil, &joinError{http.StatusConflict, a2ashape.Refuse(reasonNoHub, nil)}, nil
	}
	r := d.joinSnapshot()
	current := r != nil && sameHub(r.Hub, target) && sameHub(cfg.HubURL, target)
	if witness := d.identityWitness(); witness != "" {
		targetAID := ""
		switch {
		case r != nil && sameHub(r.Hub, target) && r.HubAID != "":
			targetAID = r.HubAID
		case d.knownHubAID(target) != "":
			targetAID = d.knownHubAID(target)
		default:
			c, jerr := d.contactHub(ctx, target, false)
			if jerr != nil {
				return nil, &joinError{http.StatusBadGateway, a2ashape.Refuse(reasonHubUnreachableLeave, a2ashape.Args{"hub": target})}, nil
			}
			targetAID = c.hubAID
		}
		if targetAID == witness {
			return nil, &joinError{http.StatusConflict, a2ashape.Refuse(reasonWitnessHubLeave, a2ashape.Args{"hub": target})}, nil
		}
	}
	if current && r.State != JoinRegistered {
		d.forgetJoin(target)
		return map[string]any{"hub": target, "state": JoinNone}, nil, nil
	}
	out, err := d.LeaveHub(ctx, target)
	if err != nil {
		if hubStatus(err) == 0 && !errors.Is(err, errGateClosed) && !errors.Is(err, errHubWire) {
			// No answer from the hub: nothing was left and nothing changed
			// here either (config.json and hub_join.json as they were).
			return nil, &joinError{http.StatusBadGateway, a2ashape.Refuse(reasonHubUnreachableLeave, a2ashape.Args{"hub": target})}, nil
		}
		return out, nil, err
	}
	if current {
		d.forgetJoin(target)
	}
	return out, nil, nil
}

// forgetJoin ends the join of hub: no record, no hub_url, no relay loop.
func (d *Daemon) forgetJoin(hub string) {
	d.regMu.Lock()
	defer d.regMu.Unlock()
	d.join.mu.Lock()
	if d.join.rec != nil && sameHub(d.join.rec.Hub, hub) {
		d.join.rec = nil
	}
	d.join.mu.Unlock()
	d.wakeJoin()
	if err := removeJoinRecord(d.layout); err != nil {
		log.Printf("anet: remove hub_join.json: %v", err)
	}
	d.forgetHubIfCurrent(hub)
	d.recordJoinChange(joinRecord{Hub: hub, State: JoinNone})
}

// --- the Go API ---

// HubRegister joins hubURL as `anet hub-register` does (AD-1), without a
// witness_rot, and reports an error unless the node ended registered.
func (d *Daemon) HubRegister(ctx context.Context, hubURL, name string, caps []string) error {
	resp, jerr := d.joinHub(ctx, joinRequest{Hub: hubURL, Name: name, Caps: caps})
	if jerr != nil {
		return jerr.ref
	}
	if resp.State != JoinRegistered {
		return d.viewOfJoin(d.joinSnapshot(), time.Now(), a2ashape.Lang()).refusal()
	}
	return nil
}

// RegisterWithHub makes one join attempt at hubURL now (check, the hooks,
// register) and reports an error unless it ended registered. The
// configured hub's join state follows it; another hub's is not kept.
func (d *Daemon) RegisterWithHub(ctx context.Context, hubURL, name string, caps []string) error {
	d.regMu.Lock()
	r := d.attemptLocked(ctx, hubURL, name, caps)
	d.regMu.Unlock()
	if r.State != JoinRegistered {
		return d.viewOfJoin(&r, time.Now(), a2ashape.Lang()).refusal()
	}
	return nil
}

// knownReasonCode reports whether code is a code of this node's reason
// table (then the hub's sentence is not shown beside it, CORE-K20).
func knownReasonCode(code string) bool {
	r, ok := a2ashape.Lookup(code)
	return ok && r.Code == code
}

// maxHubRegisterBody bounds a /hub-register body: a URL, a name, a list of
// capability ids and 32 bytes.
const maxHubRegisterBody = 256 << 10

// decodeHubRegisterReq reads a /hub-register body (LEC §2.3.2). A member
// token or invite is invite_removed (HN-5); the hub must be an http:// or
// https:// URL; next_digest is 32 bytes in standard base64; unprotected
// goes with next_digest only. Every refusal is config_invalid naming the
// member, and nothing has changed.
func decodeHubRegisterReq(b []byte) (hubRegisterReq, *a2ashape.Refusal) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil {
		return hubRegisterReq{}, configInvalid("hub", ruleText("请求体不是 JSON 对象", "the request body is not a JSON object"))
	}
	var hub string
	if raw, ok := members["hub"]; ok {
		_ = json.Unmarshal(raw, &hub)
	}
	if _, ok := members["token"]; ok {
		return hubRegisterReq{}, a2ashape.Refuse(reasonInviteRemoved, a2ashape.Args{"hub": hubOrPlaceholder(hub)})
	}
	if _, ok := members["invite"]; ok {
		return hubRegisterReq{}, a2ashape.Refuse(reasonInviteRemoved, a2ashape.Args{"hub": hubOrPlaceholder(hub)})
	}
	var w struct {
		Hub         string   `json:"hub"`
		Name        string   `json:"name"`
		Caps        []string `json:"caps"`
		NextDigest  *string  `json:"next_digest"`
		Unprotected bool     `json:"unprotected"`
		DryRun      bool     `json:"dry_run"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return hubRegisterReq{}, configInvalid("hub", ruleText("请求体的成员类型不对", "a member of the request has the wrong type"))
	}
	w.Hub = strings.TrimSpace(w.Hub)
	if checkHubURL(w.Hub) != nil {
		return hubRegisterReq{}, configInvalid("hub", ruleText("必须是以 http:// 或 https:// 开头的 hub 地址",
			"it must be a hub address starting with http:// or https://"))
	}
	req := hubRegisterReq{Hub: strings.TrimRight(w.Hub, "/"), Name: w.Name, Caps: w.Caps,
		Unprotected: w.Unprotected, DryRun: w.DryRun}
	if w.NextDigest != nil {
		d, err := base64.StdEncoding.Strict().DecodeString(*w.NextDigest)
		if err != nil || len(d) != nextDigestLen {
			return hubRegisterReq{}, configInvalid("next_digest", ruleText("必须是 32 字节(标准 base64)",
				"it must be 32 bytes (standard base64)"))
		}
		req.NextDigest = d
	}
	if req.Unprotected && req.NextDigest == nil {
		return hubRegisterReq{}, configInvalid("unprotected", ruleText("只能与 next_digest 一起出现", "it goes only with next_digest"))
	}
	return req, nil
}

// configInvalid is config_invalid for key, with rule in this process's
// language.
func configInvalid(key, rule string) *a2ashape.Refusal {
	return a2ashape.Refuse(reasonConfigInvalid, a2ashape.Args{"key": key, "rule": rule})
}

// hubOrPlaceholder is the hub to name in invite_removed's command: the one
// asked for when it is an address, else a hint the user replaces.
func hubOrPlaceholder(hub string) string {
	if checkHubURL(strings.TrimSpace(hub)) == nil {
		return strings.TrimRight(strings.TrimSpace(hub), "/")
	}
	if a2ashape.Lang() == a2ashape.Chinese {
		return "<网址>"
	}
	return "<url>"
}

// hHubRegister is POST /hub-register (LEC §2.3.2, AD-1): dry_run says what
// a hub-register would do; otherwise the node joins (or updates its
// registration at) the hub and answers with the state the attempt left.
func (d *Daemon) hHubRegister(w http.ResponseWriter, r *http.Request) {
	body, err := readLimited(r, maxHubRegisterBody)
	if err != nil {
		writeRefusal(w, http.StatusBadRequest, configInvalid("hub", ruleText("请求体太大或读不出", "the request body is too large or unreadable")))
		return
	}
	req, ref := decodeHubRegisterReq(body)
	if ref != nil {
		writeRefusal(w, http.StatusBadRequest, ref)
		return
	}
	if req.DryRun {
		out, jerr := d.dryRunJoin(r.Context(), req.Hub)
		if jerr != nil {
			writeRefusal(w, jerr.status, jerr.ref)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	out, jerr := d.joinHub(r.Context(), joinRequest{Hub: req.Hub, Name: req.Name, Caps: req.Caps,
		NextDigest: req.NextDigest, Unprotected: req.Unprotected, requireDigest: true})
	if jerr != nil {
		writeRefusal(w, jerr.status, jerr.ref)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// hHubLeave is POST /hub-leave (AD-8).
func (d *Daemon) hHubLeave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hub string `json:"hub"`
	}
	if err := readJSON(r, &req); err != nil {
		writeRefusal(w, http.StatusBadRequest, configInvalid("hub", ruleText("请求体不是 JSON 对象", "the request body is not a JSON object")))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	out, jerr, err := d.leaveHub(ctx, req.Hub)
	switch {
	case jerr != nil:
		writeRefusal(w, jerr.status, jerr.ref)
	case err != nil:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "result": out})
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

// readLimited reads a request body of at most max bytes.
func readLimited(r *http.Request, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("request body too large")
	}
	return b, nil
}
