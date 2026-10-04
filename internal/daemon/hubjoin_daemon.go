package daemon

// hubjoin_daemon.go runs the join machinery of hubjoin.go inside the daemon:
// the background loop (AD-3, AD-5), /hub-register (AD-1, LEC §2.3.2), the
// hub object of /status (§2.3.3), the send gate (AD-6) and hub-leave (AD-8).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
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
}

// initJoin loads the join record of the configured hub (New). A record of
// another hub than config.json names, or one that cannot be read, is
// replaced by a fresh one for the configured hub: config.json's hub_url is
// what the operator last chose.
func (d *Daemon) initJoin(cfg Config) {
	d.join.kick = make(chan struct{}, 1)
	d.join.wake = make(chan struct{}, 1)
	if d.join.hooks == nil {
		d.join.hooks = kernelJoinHooks{d}
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
	d.join.rec = rec
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
		if r := d.joinSnapshot(); r != nil && r.NextAttemptAt > 0 {
			timer = time.NewTimer(time.Until(time.UnixMilli(r.NextAttemptAt)))
			due = timer.C
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
	hubAID, _, err := d.hubIdentity(ctx, hubURL)
	if err != nil {
		// No hub identity, no signed request: the hub cannot be reached
		// (or does not serve one) right now, or it speaks another wire.
		a := transientAnswer(phaseRegister, whyUnreachable)
		var we *hubWireError
		if errors.As(err, &we) {
			a = joinAnswer{phase: phaseRegister, kind: ansRefused, refusal: reasonWireTooOld, code: "wire_too_old"}
			if we.hubOlder {
				a.refusal, a.code = reasonWireTooNew, "wire_too_new"
			}
		}
		r, _ = joinStep(r, a, time.Now())
		d.keepJoinRecord(hubURL, r)
		return r
	}
	r.HubAID = hubAID
	kelB, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		r, _ = joinStep(r, transientAnswer(phaseRegister, whyLocal), time.Now())
		d.keepJoinRecord(hubURL, r)
		return r
	}
	env := joinEnv{
		hub:       &httpJoinHub{d: d, hubURL: hubURL, name: name, caps: caps, ownKEL: kelB},
		hooks:     d.join.hooks,
		hubURL:    hubURL,
		hubAID:    hubAID,
		aid:       d.AID(),
		witnessed: d.identityWitness() == hubAID,
		ownKEL:    kelB,
		tip:       d.self.CurrentSeq(),
		prepared:  &d.join.prepared,
		solve: func(ctx context.Context, ch powChallenge) ([]byte, error) {
			return regpow.Solve(ctx, hubAID, d.AID(), ch.Seed, ch.Bits)
		},
		now:  time.Now,
		logf: log.Printf,
	}
	out := runJoinAttempt(ctx, r, env)
	if out.reg != nil {
		d.join.okAt.Store(start)
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
// none (KEL §2.2: the tip's witness).
func (d *Daemon) identityWitness() string {
	if d.join.witness != nil {
		return d.join.witness()
	}
	states, err := identity.Replay(d.self.KEL())
	if err != nil || len(states) == 0 {
		return ""
	}
	return states[len(states)-1].Witness
}

// kernelJoinHooks are the join hooks until the identity area's land: the
// identity registers its own KEL as it stands and adopts nothing.
// v030-todo(KEL-9): identity_ops.go implements Prepare (the witness
// designation rot, KEL D-13) and Adopt.
type kernelJoinHooks struct{ d *Daemon }

func (k kernelJoinHooks) Prepare(context.Context, string) ([]byte, error) {
	return identity.MarshalKEL(k.d.self.KEL())
}

func (kernelJoinHooks) Adopt(context.Context, RegisterResult) error { return nil }

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
	if r == nil {
		v := hubView{State: JoinNone, Reason: reasonNoHub, key: reasonNoHub, Since: d.started.UTC().Format(time.RFC3339)}
		v.Message = a2ashape.Sentence(v.key, lang, nil)
		v.Next = a2ashape.Next(v.key, lang, nil)
		return v
	}
	key := r.Reason
	args := a2ashape.Args{"hub": r.Hub, "aid": d.AID(), "now": now}
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
		if n := IdentityNameForDir(d.layout.Root); n != "" {
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
func (d *Daemon) sendGate() *a2ashape.Refusal {
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
	writeJSON(w, status, ref.Body(a2ashape.Lang()))
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
	ctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hubURL, "/")+"/hub/identity", nil)
	if err != nil {
		return hubContact{}, unreachable
	}
	rep := d.hubRoundTrip(req)
	if rep.Status == 0 {
		return hubContact{}, unreachable
	}
	var out struct {
		hubapi.HubIdentity
		Wire *int `json:"wire"`
	}
	if rep.Status != http.StatusOK || json.Unmarshal(rep.Body, &out) != nil || out.AID == "" {
		// An answer that is not a hub's identity: not a hub at this
		// address, as far as this node can tell.
		return hubContact{}, unreachable
	}
	wire := -1
	if v, err := strconv.Atoi(rep.Wire); err == nil {
		wire = v
	} else if out.Wire != nil {
		wire = *out.Wire
	}
	switch {
	case wire > hubapi.WireVersion:
		return hubContact{}, &joinError{http.StatusConflict, a2ashape.Refuse(reasonWireTooOld, a2ashape.Args{"hub": hubURL})}
	case wire < hubapi.WireVersion:
		return hubContact{}, &joinError{http.StatusConflict, a2ashape.Refuse(reasonWireTooNew, a2ashape.Args{"hub": hubURL})}
	}
	if !pin {
		if !hubKELReplays(out.HubIdentity) {
			return hubContact{}, unreachable
		}
		return hubContact{hubAID: out.AID}, nil
	}
	// Pinned as every signed call pins it (hubIdentity), from this answer.
	aid, _, err := d.pinHubIdentity(hubURL, out.HubIdentity)
	if err != nil {
		return hubContact{}, unreachable
	}
	return hubContact{hubAID: aid}, nil
}

// hubKELReplays reports whether a /hub/identity answer's KEL reads and
// replays to its AID: what pinHubIdentity checks before it writes anything.
func hubKELReplays(h hubapi.HubIdentity) bool {
	raw, err := base64.StdEncoding.DecodeString(h.KEL)
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
	c, jerr := d.contactHub(ctx, req.Hub, true)
	if jerr != nil {
		return hubRegisterResp{}, jerr
	}
	switch d.joinAction(c.hubAID) {
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
	}
	caps := withServedCapabilities(req.Caps, d.providers)

	d.regMu.Lock()
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
		if cfg.Summary != "" || cfg.Readme != "" || cfg.Pricing != "" {
			pctx, pcancel := context.WithTimeout(ctx, hubCallTimeout)
			if err := d.PublishProfile(pctx, req.Hub, cfg.Summary, cfg.Readme, cfg.Pricing); err != nil {
				log.Printf("anet: publish the profile at %s: %v", req.Hub, err)
			}
			pcancel()
		}
	}
	d.writeRegistry() // the identity entry shows the (possibly new) name
	return d.registerAnswer(&rec, req.Unprotected, a2ashape.Lang()), nil
}

// adoptJoinTarget writes config.json's hub_url, name and caps and the hub's
// join record (AD-1), and returns the hub configured before. A join of the
// hub already configured keeps its record and starts its backoff over
// (AD-3: running hub-register again retries at once).
func (d *Daemon) adoptJoinTarget(req joinRequest, hubAID string, caps []string) (string, error) {
	var prevHub string
	var adopted *AutoReplyConfig
	set := func(c *Config) {
		c.HubURL = req.Hub
		if req.Name != "" {
			c.Name = req.Name
		}
		if caps != nil {
			c.Caps = caps
		}
	}
	_, err := d.updateConfig(func(c *Config) error {
		prevHub = c.HubURL
		set(c)
		// Don't clobber an auto_reply block hand-added to config.json after
		// this daemon started (its in-memory cfg would not know it): adopt
		// the on-disk value before writing back.
		if c.AutoReply == nil {
			if prev, err := LoadConfig(d.layout); err == nil && prev.AutoReply != nil {
				c.AutoReply, adopted = prev.AutoReply, prev.AutoReply
			}
		}
		return nil
	}, func(live *Config) {
		set(live)
		if adopted != nil && live.AutoReply == nil {
			live.AutoReply = adopted
		}
	})
	if err != nil {
		return prevHub, err
	}
	now := time.Now()
	d.join.mu.Lock()
	r := d.join.rec
	if r == nil || !sameHub(r.Hub, req.Hub) {
		r = freshJoinRecord(req.Hub, now)
	} else {
		cp := *r
		r = &cp
		r.Attempts = 0
	}
	r.HubAID = hubAID
	if req.NextDigest != nil {
		// Before the rot is built a new commitment replaces the old one
		// (KEL D-14). v030-todo(KEL-9): with unprotected, local_commit
		// (identity file key 7) is written first (KEL D-11), and a
		// commitment arriving after the rot was built is rotation_pending.
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
func (d *Daemon) registerAnswer(r *joinRecord, unprotected bool, lang a2ashape.Language) hubRegisterResp {
	return d.registerAnswerAt(r, unprotected, lang, time.Now())
}

// registerAnswerAt is registerAnswer at now.
func (d *Daemon) registerAnswerAt(r *joinRecord, unprotected bool, lang a2ashape.Language, now time.Time) hubRegisterResp {
	v := d.viewOfJoin(r, now, lang)
	return hubRegisterResp{Hub: v.URL, AID: d.AID(), State: v.State, Reason: v.Reason, Message: v.Message,
		Next: v.Next, NextAttemptAt: v.NextAttemptAt, Recovery: recoveryForJoin(unprotected, lang),
		HubCode: v.HubCode, HubError: v.HubError}
}

// recoveryForJoin is the recovery object of the /hub-register answer: set
// while the identity has no recovery passphrase (KEL D-15).
// v030-todo(KEL-8): recoveryStateOf (identity_recovery.go) decides from
// the identity file's local_commit and the KEL; until then the request's
// unprotected says it.
func recoveryForJoin(unprotected bool, lang a2ashape.Language) *recoveryView {
	if !unprotected {
		return nil
	}
	const key = "identity.unprotected"
	return &recoveryView{State: "unprotected", Message: a2ashape.Sentence(key, lang, nil),
		Next: a2ashape.Next(key, lang, nil)}
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
//   - the identity's witness hub: refused, witness_hub_leave;
//   - the configured hub, not registered yet: the join stops, hub_join.json
//     and hub_url go, the state is none;
//   - the configured hub, registered (an identity without a witness): the
//     node deregisters there and forgets the hub;
//   - another hub (the one before a switch): deregistered there; the
//     configured hub's join is not touched (core X-KEL-7).
func (d *Daemon) leaveHub(ctx context.Context, target string) (map[string]any, *joinError, error) {
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
	if current && r.HubAID != "" && d.identityWitness() == r.HubAID {
		return nil, &joinError{http.StatusConflict, a2ashape.Refuse(reasonWitnessHubLeave, a2ashape.Args{"hub": r.Hub})}, nil
	}
	if current && r.State != JoinRegistered {
		d.forgetJoin(target)
		return map[string]any{"hub": target, "state": JoinNone}, nil, nil
	}
	out, err := d.LeaveHub(ctx, target)
	if err != nil {
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
