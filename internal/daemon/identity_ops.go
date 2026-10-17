package daemon

// identity_ops.go orchestrates the identity's key events in the daemon (KEL
// §4.3.2–§4.3.7, core §4.7):
//
//   - the join hooks (KEL D-12–D-14a): the witness designation built from
//     witness_rot when the hub admits this AID, adopted when it accepts the
//     registration, dropped when it refuses it for good;
//   - /identity/rotate and /identity/deactivate (KEL §2.8, P-1–P-5): a
//     planned rotation (D-30), a hub switch (D-32), an emergency rotation
//     (D-40–D-42, E0–E5) and the deactivation (D-46);
//   - the publication sequence of a pending rot, its retries, the time
//     window (D-36), and what follows its acceptance;
//   - the push of the key history to former witnesses (D-33), the old hub's
//     mailbox drained and left after a switch (D-32 step 5);
//   - the check of the witness's record of this identity (D-27) and the
//     stop it may lead to (D-29, D-1 second part);
//   - the resumption of all that after a crash (D-35).
//
// Every state it needs after a crash is on disk: the identity file (the
// pending event, witnesses_to_push, state), hub_join.json (witness_rot),
// the key ring (regen_tag, E2), and the evidence chain (E5). What is only
// in memory — when to try again, which former witness confirmed — starts
// over at the next start, and every step is idempotent (engine CR11).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/regpow"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The rhythm of the identity's background work (KEL §2.12). None is
// configurable; the knobs are for tests.
var (
	// pushRetryMin and pushRetryMax bound the backoff of an unconfirmed
	// push to a former witness (D-33): from a minute, doubling, up to an
	// hour.
	pushRetryMin = newKnob(time.Minute)
	pushRetryMax = newKnob(time.Hour)
	// pushRepeat is how often a confirmed former witness is pushed the
	// whole key history again, until the identity's dip is confirmed.
	pushRepeat = newKnob(6 * time.Hour)
	// oldHubPollEvery is how often the old hub's mailbox is read while it
	// drains after a switch (D-32 step 5).
	oldHubPollEvery = newKnob(30 * time.Second)
	// hubKELCheckEvery is HubKELCheck: the current witness is asked for its
	// record of this identity at most once a minute (D-27).
	hubKELCheckEvery = newKnob(time.Minute)
	// restampEvery: a pending event whose time the witness refused is
	// signed again at most every 10 minutes (D-36).
	restampEvery = newKnob(10 * time.Minute)
	// publishRetryMax bounds the wait between two attempts to publish a
	// pending event.
	publishRetryMax = newKnob(10 * time.Minute)
)

// OldHubDrain is how long the old hub's mailbox is read after a hub switch,
// counted from the switch's rot (KEL §2.12): the longest a message queued
// there before the switch can live.
const OldHubDrain = seal.MaxMessageLifetimeMS

// The modes of anet.identity.rotated (KEL §2.10) besides those of
// last_rotation (identity_status.go).
const rotationRecover = "recover"

// identityOps is the daemon's state of the identity's key events.
type identityOps struct {
	// change lets one request change the identity at a time (KEL P-5); a
	// second one is answered rotation_pending.
	change sync.Mutex
	// pub runs one step of the publication sequence, the emergency
	// rotation's E1–E5 or the pushes at a time.
	pub sync.Mutex
	// kick wakes the background loop.
	kick chan struct{}
	// emergencyOpen: an emergency rotation (or a recover) was accepted and
	// its E1–E5 are not done. The gate stays closed (publishing), and the
	// key sets are held until E2 (keysHeld).
	emergencyOpen atomic.Bool
	// hubRecordInvalid: the last check of the witness's record (D-27) found
	// one that does not verify (identity.hub_record_invalid).
	hubRecordInvalid atomic.Bool
	// started: emergencyOpen has been read from disk (initIdentityOps).
	started atomic.Bool

	mu sync.Mutex
	// recover: the pending rot is a recover's (KEL D-45), so its evidence
	// says recover.
	recover bool
	// restampAt is when the pending event was last signed again (D-36).
	restampAt time.Time
	// pubFailures counts the attempts to publish the pending event that
	// failed in a row; pubNextAt is when the next is due (zero: on a kick).
	pubFailures int
	pubNextAt   time.Time
	// emNextAt is when E1–E5 are tried again after a step failed.
	emNextAt time.Time
	// checkAt is when the witness's record was last fetched, and checking
	// that a check runs (D-27).
	checkAt  time.Time
	checking bool
	// acked are the former witnesses (by AID) that confirmed the key
	// history pushed to them; failures the pushes that failed in a row.
	acked    map[string]bool
	failures map[string]int
	// drains are the old hubs read after a switch, by AID.
	drains map[string]*oldHubDrain
	// crash, when set, is asked at each point a crash can cut the
	// orchestration short; true stops it there (tests only).
	crash func(point string) bool
}

// oldHubDrain is an old hub read after a switch (D-32 step 5).
type oldHubDrain struct {
	url      string
	until    uint64 // unix ms: the switch rot's ts + OldHubDrain
	cur      relayCursor
	nextPoll time.Time
	left     bool
}

// crashAt reports whether a test asked the orchestration to stop at point,
// as a crash there would.
func (d *Daemon) crashAt(point string) bool {
	d.idOps.mu.Lock()
	f := d.idOps.crash
	d.idOps.mu.Unlock()
	return f != nil && f(point)
}

// errSimulatedCrash is the orchestration stopped where a test simulates a crash.
var errSimulatedCrash = errors.New("anet: stopped at a simulated crash")

// kickIdentity wakes the background loop.
func (d *Daemon) kickIdentity() {
	if d.idOps.kick == nil {
		return
	}
	select {
	case d.idOps.kick <- struct{}{}:
	default:
	}
}

// --- start (KEL D-35) ---

// initIdentityOps reads, at start, where the identity's key events stand:
// an emergency rotation accepted and not finished, a recover's rot waiting
// to be built. The background loop, started after the join loop, finishes
// them first.
func (d *Daemon) initIdentityOps() {
	d.idOps.kick = make(chan struct{}, 1)
	if d.self == nil {
		return
	}
	if p, ok := d.self.Pending(); ok && p.Kind == identity.PendingRot && p.Event == nil && p.Emergency {
		// Material without an event, emergency: what `anet identity
		// recover` writes into a new data directory (D-45 step 5).
		d.idOps.mu.Lock()
		d.idOps.recover = true
		d.idOps.mu.Unlock()
	}
	d.idOps.emergencyOpen.Store(d.emergencyUnfinished())
	d.idOps.started.Store(true)
}

// startIdentityLoop runs the identity's background work for the life of the
// daemon, starting with whatever a crash cut short (D-35).
func (d *Daemon) startIdentityLoop() {
	d.goBackground(func() { d.identityLoop(d.ctx) })
	d.kickIdentity()
}

// emergencyUnfinished reports whether the accepted tip is an emergency rot
// whose E5 has not run: E5 writes anet.identity.rotated for its seq, last.
func (d *Daemon) emergencyUnfinished() bool {
	kel := d.self.Accepted()
	tip := kel[len(kel)-1].Event
	if tip.Type != identity.Rotation || !tip.Emergency {
		return false
	}
	return d.rotationEvidence(tip.Seq) == ""
}

// rotationEvidence is the mode anet.identity.rotated records for the event
// of seq on this node's chain, "" when there is none.
func (d *Daemon) rotationEvidence(seq uint64) string {
	if d.ledger == nil {
		return ""
	}
	mode := ""
	d.ledger.scan(evtypes.IdentityRotated, 0, func(_ int64, p map[string]any) {
		if fmt.Sprint(p["seq"]) == fmt.Sprint(seq) {
			if m, ok := p["mode"].(string); ok {
				mode = m
			}
		}
	})
	return mode
}

// identityLoop is the background loop: each pass does what is due and
// waits for the next due time or a kick.
func (d *Daemon) identityLoop(ctx context.Context) {
	for {
		wait := d.identityPass(ctx)
		var due <-chan time.Time
		var t *time.Timer
		if wait > 0 {
			t = time.NewTimer(wait)
			due = t.C
		}
		select {
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return
		case <-d.idOps.kick:
		case <-due:
		}
		if t != nil {
			t.Stop()
		}
	}
}

// identityPass does what is due now and returns how long until the next
// thing is (0: nothing until a kick).
func (d *Daemon) identityPass(ctx context.Context) time.Duration {
	if d.self == nil || ctx.Err() != nil {
		return 0
	}
	var waits []time.Duration
	if st := d.self.State(); st == identity.StateStopped {
		return 0 // nothing more is published or pushed (KEL D-29)
	}
	if w := d.publishDue(ctx); w > 0 {
		waits = append(waits, w)
	}
	if w := d.emergencyDue(ctx); w > 0 {
		waits = append(waits, w)
	}
	if w := d.pushDue(ctx); w > 0 {
		waits = append(waits, w)
	}
	if w := d.drainDue(ctx); w > 0 {
		waits = append(waits, w)
	}
	var least time.Duration
	for _, w := range waits {
		if least == 0 || w < least {
			least = w
		}
	}
	return least
}

// --- the join hooks (KEL D-12–D-14a) ---

// identityJoinHooks are the join's hooks into the identity (LEC AD-4): the
// witness designation (KEL D-13).
type identityJoinHooks struct{ d *Daemon }

// Prepare builds the rot that designates hubAID the identity's witness from
// witness_rot (KEL D-13 steps 1, 2): rot{seq 1, keys [k1], next
// witness_rot.next_digest, witnesses [hubAID], toad 1}, written to the
// identity file before it is returned. A designation already built for the
// hub is returned as it is (D-14), unless the hub refused its time and the
// last one was signed more than restampEvery ago (D-36). The key set is
// signed again under the rot's key, which the registration carries (the
// hub refuses a set signed under a key its KEL retires).
//
// Without witness_rot — a join through the Go API, or a record lost — there
// is no commitment to designate the hub with, and the identity registers
// its key history as it stands.
func (h identityJoinHooks) Prepare(_ context.Context, hubAID string) ([]byte, error) {
	d := h.d
	c := d.self
	if p, ok := c.Pending(); ok {
		ev, err := pendingEvent(p)
		if err != nil {
			return nil, err
		}
		if p.Kind != identity.PendingWitness || ev == nil || len(ev.Event.Witnesses) != 1 || ev.Event.Witnesses[0] != hubAID {
			return nil, fmt.Errorf("anet: another key event is being published (%s); the join waits for it", p.Kind)
		}
		if _, _, perr := d.idGate.read(); perr == hubKELTSOutOfWindow && d.restampDue() {
			if err := d.restampPending(); err != nil {
				return nil, err
			}
		}
		// The key set goes with it, signed under its key (a restart may
		// have come between the two).
		d.resignForPending()
		return identity.MarshalKEL(c.KEL())
	}
	r := d.joinSnapshot()
	if r == nil || r.WitnessRot == nil || r.WitnessRot.HubAID != hubAID || len(c.Accepted()) != 1 {
		return identity.MarshalKEL(c.KEL())
	}
	next := append([]byte(nil), r.WitnessRot.NextDigest...)
	err := d.ledger.whileKeyEvent(nowMillis(), func(ts uint64) error {
		if _, err := c.BuildWitnessDesignation(hubAID, next, ts); err != nil {
			return err
		}
		if err := c.SetPendingHubURL(r.Hub); err != nil {
			return err
		}
		if err := saveIdentity(d.layout, c); err != nil {
			// Not on disk, not sent: drop it, so nothing a crash would
			// lose ever reaches the hub.
			_ = c.DiscardPending()
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if d.crashAt("designation-saved") {
		return nil, errSimulatedCrash
	}
	d.resignForPending()
	return identity.MarshalKEL(c.KEL())
}

// Adopt takes the registration the hub accepted: the designation becomes the
// accepted tip, witness_rot goes, the event is on the evidence chain (KEL
// D-13 steps 4, 5). Adopting what is already adopted does nothing.
func (h identityJoinHooks) Adopt(ctx context.Context, res RegisterResult) error {
	d := h.d
	p, ok := d.self.Pending()
	if !ok || p.Kind != identity.PendingWitness {
		return nil
	}
	ev, err := pendingEvent(p)
	if err != nil || ev == nil {
		return fmt.Errorf("anet: the pending designation cannot be read: %v", err)
	}
	sent, err := identity.UnmarshalKEL(res.KEL)
	if err != nil || len(sent) == 0 || sent[len(sent)-1].EventID != ev.EventID {
		return errors.New("anet: the hub accepted a key history that does not end with the pending designation")
	}
	if d.crashAt("designation-accepted") {
		return errSimulatedCrash
	}
	if err := d.adoptPending(*ev); err != nil {
		return err
	}
	d.idGate.notePublishError("")
	d.dropWitnessRot()
	d.recordRotation(ev.Event.Seq, rotationWitness, res.HubAID)
	return nil
}

// pendingEvent decodes the pending event of p; nil when none is built.
func pendingEvent(p identity.Pending) (*identity.SignedEvent, error) {
	if p.Event == nil {
		return nil, nil
	}
	var ev identity.SignedEvent
	if err := coredet.Unmarshal(p.Event, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// adoptPending makes ev, the pending event, the accepted tip and writes the
// identity file; the evidence the event held back is signed then.
func (d *Daemon) adoptPending(ev identity.SignedEvent) error {
	return d.ledger.whileKeyEvent(nowMillis(), func(uint64) error {
		if err := d.self.Adopt(ev); err != nil {
			return err
		}
		if err := saveIdentity(d.layout, d.self); err != nil {
			// The witness holds it; the file still says pending. The next
			// start publishes the same bytes again, and the witness answers
			// them as held.
			log.Printf("anet: save identity.kel after the witness accepted seq %d: %v", ev.Event.Seq, err)
		}
		return nil
	})
}

// dropWitnessRot removes witness_rot from the join record once the
// designation is the tip (KEL D-12).
func (d *Daemon) dropWitnessRot() {
	d.join.mu.Lock()
	r := d.join.rec
	if r == nil || r.WitnessRot == nil {
		d.join.mu.Unlock()
		return
	}
	cp := *r
	cp.WitnessRot = nil
	d.join.rec = &cp
	d.join.mu.Unlock()
	if err := saveJoinRecord(d.layout, &cp); err != nil {
		log.Printf("anet: save hub_join.json: %v", err)
	}
}

// afterDesignationAnswer reads the hub's last answer to the registration of
// the pending designation (runJoinAttempt), for what only the identity
// decides: the time refused (D-36: clock_skew, a new time at the next
// Prepare), a final refusal (D-14a: the designation is dropped), and
// every other refusal of the event (notePublishError).
func (d *Daemon) afterDesignationAnswer(a joinAnswer) {
	p, ok := d.self.Pending()
	if !ok || p.Kind != identity.PendingWitness {
		return
	}
	switch {
	case a.kind == ansRegistered:
		d.idGate.notePublishError("")
	case a.kind == ansClockSkew && a.code == hubKELTSOutOfWindow:
		d.idGate.notePublishError(hubKELTSOutOfWindow)
	case a.kind == ansRefused && (a.refusal == reasonHubRefused || a.refusal == reasonSwitch ||
		a.refusal == reasonPublicationBlocked):
		// Refused for good: no hub took it — or it never left this node,
		// its registration kept back by the publication screen — so
		// dropping it forks nothing, and the identity can be joined to
		// another hub (KEL D-14a). witness_rot stays.
		ev, _ := pendingEvent(p)
		if err := d.discardDesignation(); err != nil {
			log.Printf("anet: drop the witness designation the hub refused: %v", err)
			return
		}
		if ev != nil && len(ev.Event.Witnesses) == 1 {
			// The KEL a retry would send is not the identity's any more.
			d.join.prepared.drop(preparedKey(ev.Event.Witnesses[0], ev.Event.Seq-1))
		}
		d.idGate.notePublishError("")
	case a.code != "":
		d.idGate.notePublishError(a.code)
	}
}

// discardDesignation drops the pending designation (KEL D-14a) and writes
// the identity file; the key set goes back to the accepted tip's key.
func (d *Daemon) discardDesignation() error {
	err := d.ledger.whileKeyEvent(nowMillis(), func(uint64) error {
		if err := d.self.DiscardPending(); err != nil {
			return err
		}
		return saveIdentity(d.layout, d.self)
	})
	if err != nil {
		return err
	}
	if d.enc != nil {
		if err := d.resignKeyRing(); err != nil {
			log.Printf("anet: sign the key set again after dropping the designation: %v", err)
		}
	}
	return nil
}

// resignForPending signs the key set under the key of the event being
// published, unless key sets are held (an emergency rotation's): the
// registration carries it (KEL D-30 step 4, FS-K17).
func (d *Daemon) resignForPending() {
	if d.enc == nil || d.keysHeld() {
		return
	}
	if err := d.resignKeyRing(); err != nil {
		log.Printf("anet: sign the key set under the new key: %v", err)
	}
}

// restampDue reports whether the pending event may be signed again (D-36).
func (d *Daemon) restampDue() bool {
	d.idOps.mu.Lock()
	defer d.idOps.mu.Unlock()
	return d.idOps.restampAt.IsZero() || time.Since(d.idOps.restampAt) >= restampEvery.get()
}

// restampPending signs the pending event again at a new time (D-36): only
// after the witness refused these very bytes for their time, which proves
// it never accepted them.
func (d *Daemon) restampPending() error {
	err := d.ledger.whileKeyEvent(nowMillis(), func(ts uint64) error {
		if _, err := d.self.Restamp(ts); err != nil {
			return err
		}
		return saveIdentity(d.layout, d.self)
	})
	if err != nil {
		return err
	}
	d.idOps.mu.Lock()
	d.idOps.restampAt = time.Now()
	d.idOps.mu.Unlock()
	d.resignForPending()
	return nil
}

// --- the evidence of a key event (KEL §2.10) ---

// recordRotation writes anet.identity.rotated for the event of seq, signed
// by the key it revealed.
func (d *Daemon) recordRotation(seq uint64, mode, witness string) {
	if d.ledger == nil {
		return
	}
	if _, err := d.ledger.Append(evtypes.IdentityRotated, map[string]any{"seq": seq, "mode": mode, "witness": witness}); err != nil {
		log.Printf("anet: evidence %s: %v", evtypes.IdentityRotated, err)
	}
}

// --- /identity/rotate and /identity/deactivate (KEL §2.8) ---

// identityRotateReq is the /identity/rotate body.
type identityRotateReq struct {
	Mode         string
	RevealedSeed []byte
	NextDigest   []byte
	HubURL       string
	DryRun       bool
}

// The modes of /identity/rotate.
const (
	rotateScheduled = "scheduled"
	rotateEmergency = "emergency"
)

// maxIdentityBody bounds an /identity/* body.
const maxIdentityBody = 64 << 10

// decodeIdentityReq reads an /identity/rotate or /identity/deactivate
// body: a JSON object of the members given, binary values in standard
// base64 (core CORE-E5), each of its form. Every refusal is config_invalid
// naming the member.
func decodeIdentityReq(b []byte, rotate bool) (identityRotateReq, *a2ashape.Refusal) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil {
		return identityRotateReq{}, configInvalid("body", ruleText("请求体不是 JSON 对象", "the request body is not a JSON object"))
	}
	allowed := map[string]bool{"revealed_seed": true, "dry_run": true}
	if rotate {
		allowed["mode"], allowed["next_digest"], allowed["hub_url"] = true, true, true
	}
	for k := range members {
		if !allowed[k] {
			return identityRotateReq{}, configInvalid(k, ruleText("不是这个请求的成员", "it is not a member of this request"))
		}
	}
	var w struct {
		Mode         string  `json:"mode"`
		RevealedSeed *string `json:"revealed_seed"`
		NextDigest   *string `json:"next_digest"`
		HubURL       string  `json:"hub_url"`
		DryRun       bool    `json:"dry_run"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return identityRotateReq{}, configInvalid("body", ruleText("请求体的成员类型不对", "a member of the request has the wrong type"))
	}
	req := identityRotateReq{Mode: w.Mode, DryRun: w.DryRun, HubURL: strings.TrimRight(strings.TrimSpace(w.HubURL), "/")}
	if rotate {
		switch req.Mode {
		case rotateScheduled, rotateEmergency:
		default:
			return identityRotateReq{}, configInvalid("mode", ruleText("必须是 scheduled 或 emergency", "it must be scheduled or emergency"))
		}
		if req.HubURL != "" && checkHubURL(req.HubURL) != nil {
			return identityRotateReq{}, configInvalid("hub_url", ruleText("必须是以 http:// 或 https:// 开头的 hub 地址",
				"it must be a hub address starting with http:// or https://"))
		}
	}
	bin := func(name string, v *string, n int) ([]byte, *a2ashape.Refusal) {
		if v == nil {
			return nil, nil
		}
		b, err := base64.StdEncoding.Strict().DecodeString(*v)
		if err != nil || len(b) != n {
			return nil, configInvalid(name, ruleText(fmt.Sprintf("必须是 %d 字节(标准 base64)", n),
				fmt.Sprintf("it must be %d bytes (standard base64)", n)))
		}
		return b, nil
	}
	var ref *a2ashape.Refusal
	if req.RevealedSeed, ref = bin("revealed_seed", w.RevealedSeed, ed25519.SeedSize); ref != nil {
		return identityRotateReq{}, ref
	}
	if req.NextDigest, ref = bin("next_digest", w.NextDigest, nextDigestLen); ref != nil {
		return identityRotateReq{}, ref
	}
	if !req.DryRun {
		if req.RevealedSeed == nil {
			return identityRotateReq{}, configInvalid("revealed_seed", ruleText("必须给出(anet identity 会自动带上)",
				"it must be given (anet identity adds it)"))
		}
		if rotate && req.NextDigest == nil {
			return identityRotateReq{}, configInvalid("next_digest", ruleText("必须给出(anet identity 会自动带上)",
				"it must be given (anet identity adds it)"))
		}
	}
	return req, nil
}

// identityChangeRefusal is why the identity cannot change now (KEL D-30
// step 1): it must be ok or notice_stale; no_hub, publishing, stopped and
// deactivated each say so in their own words.
func (d *Daemon) identityChangeRefusal() (int, *a2ashape.Refusal) {
	switch st := d.identityState(); st.State {
	case IdentityOK, IdentityNoticeStale:
		return 0, nil
	case IdentityNoHub:
		return http.StatusConflict, a2ashape.Refuse(reasonNoHub, nil)
	case IdentityPublishing:
		return http.StatusConflict, a2ashape.Refuse(reasonRotationPending, nil)
	case IdentityDeactivated:
		return http.StatusGone, a2ashape.Refuse(reasonDeactivated, nil)
	default:
		return http.StatusConflict, d.identitySendRefusal()
	}
}

// committedDigest is key 6 of the accepted KEL's last establishment event:
// the digest the next key revealed must have (P-1).
func (d *Daemon) committedDigest() []byte {
	kel := d.self.Accepted()
	for i := len(kel) - 1; i >= 0; i-- {
		if t := kel[i].Event.Type; t == identity.Inception || t == identity.Rotation {
			return kel[i].Event.NextDigest
		}
	}
	return nil
}

// revealedKey is the key of seed, when SHA-256 of its public key is the
// committed digest (P-1); nil otherwise. A party that holds the control
// token but not the recovery seed cannot pass it. The key it returns is the
// one the controller signs with from the event on (P-2), and is kept.
func (d *Daemon) revealedKey(seed []byte) ed25519.PrivateKey {
	if len(seed) != ed25519.SeedSize {
		return nil
	}
	k := ed25519.NewKeyFromSeed(seed)
	sum := sha256.Sum256(k.Public().(ed25519.PublicKey))
	if !bytes.Equal(sum[:], d.committedDigest()) {
		clear(k)
		return nil
	}
	return k
}

// rotateAnswer is the /identity/rotate answer (KEL §2.8): the state, the
// key seq, the witness and the former witnesses not yet confirmed.
type rotateAnswer struct {
	State             string               `json:"state"`
	KeySeq            uint64               `json:"key_seq"`
	Witness           *IdentityWitness     `json:"witness"`
	OldWitnessUnacked []IdentityOldWitness `json:"old_witness_unacked"`
}

func (d *Daemon) rotateAnswerNow() rotateAnswer {
	s := d.IdentityStatus()
	return rotateAnswer{State: s.State, KeySeq: s.KeySeq, Witness: s.Witness, OldWitnessUnacked: s.OldWitnessUnacked}
}

// hIdentityRotate is POST /identity/rotate (KEL §2.8, D-30, D-32, D-40).
func (d *Daemon) hIdentityRotate(w http.ResponseWriter, r *http.Request) {
	body, err := readLimited(r, maxIdentityBody)
	if err != nil {
		writeRefusal(w, http.StatusBadRequest, configInvalid("body", ruleText("请求体太大或读不出", "the request body is too large or unreadable")))
		return
	}
	req, ref := decodeIdentityReq(body, true)
	if ref != nil {
		writeRefusal(w, http.StatusBadRequest, ref)
		return
	}
	if !d.idOps.change.TryLock() {
		writeRefusal(w, http.StatusConflict, a2ashape.Refuse(reasonRotationPending, nil))
		return
	}
	defer d.idOps.change.Unlock()
	status, out, ref := d.rotateIdentity(r.Context(), req)
	if ref != nil {
		writeRefusal(w, status, ref)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// rotateIdentity is /identity/rotate with the change lock held.
func (d *Daemon) rotateIdentity(ctx context.Context, req identityRotateReq) (int, rotateAnswer, *a2ashape.Refusal) {
	if status, ref := d.identityChangeRefusal(); ref != nil {
		return status, rotateAnswer{}, ref
	}
	witness := identityFactsOf(d.self).Witness
	curURL := d.witnessURL(witness)
	target, targetAID := curURL, witness
	emergency := req.Mode == rotateEmergency
	switching := req.HubURL != "" && !sameHub(req.HubURL, curURL)
	if switching && emergency {
		// An emergency rotation and a hub switch are two steps: switch
		// first, then rotate in an emergency (KEL §1.4).
		return http.StatusBadRequest, rotateAnswer{}, configInvalid("hub_url",
			ruleText("紧急轮换不能同时换 hub;请先换 hub,再做紧急轮换", "an emergency rotation does not switch hubs; switch first, then rotate"))
	}
	if switching {
		aid, status, ref := d.checkSwitchTarget(ctx, req.HubURL, req.DryRun)
		if ref != nil {
			return status, rotateAnswer{}, ref
		}
		target, targetAID = req.HubURL, aid
	} else if !emergency || req.DryRun {
		// A planned rotation needs its witness now (D-30 step 2); an
		// emergency one is built at once and published when the witness
		// comes back (D-40), and its dry run tells the CLI whether it is
		// up (L-3).
		if _, jerr := d.contactHub(ctx, curURL, false); jerr != nil {
			return http.StatusServiceUnavailable, rotateAnswer{}, a2ashape.Refuse(reasonWitnessUnreachable, a2ashape.Args{"hub": curURL})
		}
	}
	if req.DryRun {
		return 0, d.rotateAnswerNow(), nil
	}
	revealed := d.revealedKey(req.RevealedSeed)
	if revealed == nil {
		return http.StatusConflict, rotateAnswer{}, a2ashape.Refuse(reasonRecoveryMismatch, nil)
	}
	err := d.ledger.whileKeyEvent(nowMillis(), func(ts uint64) error {
		if _, err := d.self.BuildRotation(revealed, req.NextDigest, targetAID, emergency, ts); err != nil {
			return err
		}
		if err := d.self.SetPendingHubURL(target); err != nil {
			return err
		}
		return saveIdentity(d.layout, d.self)
	})
	switch {
	case errors.Is(err, identity.ErrRecoveryMismatch):
		return http.StatusConflict, rotateAnswer{}, a2ashape.Refuse(reasonRecoveryMismatch, nil)
	case errors.Is(err, identity.ErrEventPending):
		return http.StatusConflict, rotateAnswer{}, a2ashape.Refuse(reasonRotationPending, nil)
	case err != nil:
		// Built but not written: the publication loop writes it before
		// it sends anything (publishPending), so nothing a crash would
		// lose reaches a hub.
		log.Printf("anet: save the rotation to identity.kel: %v (written before it is published)", err)
	}
	if d.crashAt("rot-saved") {
		return 0, d.rotateAnswerNow(), nil
	}
	d.resignForPending()
	// Published now, once, and by the background loop after: once the
	// event is written, a failure to publish is no error (KEL §2.8): the
	// state is publishing and the daemon retries.
	pctx, cancel := context.WithTimeout(ctx, joinAttemptTimeout)
	d.publishNow(pctx)
	cancel()
	d.kickIdentity()
	return 0, d.rotateAnswerNow(), nil
}

// checkSwitchTarget is D-32 step 1: the new hub answers its identity
// (else 502 hub_unreachable) and would take this AID (check_only; else
// hub_switch_pending_admission, or the hub's refusal in the user's words).
// A dry run asks without a proof of work: one asked for means the hub is
// open. It returns the hub's AID.
func (d *Daemon) checkSwitchTarget(ctx context.Context, hubURL string, dry bool) (string, int, *a2ashape.Refusal) {
	c, jerr := d.contactHub(ctx, hubURL, !dry)
	if jerr != nil {
		return "", jerr.status, jerr.ref
	}
	kel, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		return "", http.StatusInternalServerError, configInvalid("identity", err.Error())
	}
	h := &httpJoinHub{d: d, hubURL: hubURL, ownKEL: kel}
	jctx := withJoinFlow(ctx)
	a := classifyHubReply(phaseCheck, h.checkOnly(jctx, nil))
	if a.kind == ansPowRequired && a.challenge != nil {
		if dry {
			a.kind = ansOpen
		} else if nonce, err := regpow.Solve(jctx, c.hubAID, d.AID(), a.challenge.Seed, a.challenge.Bits); err == nil {
			a = classifyHubReply(phaseCheckPow, h.checkOnly(jctx, &powProof{Seed: a.challenge.Seed, Nonce: nonce}))
		}
	}
	args := a2ashape.Args{"hub": hubURL, "aid": d.AID()}
	switch a.kind {
	case ansAdmitted, ansOpen:
		return c.hubAID, 0, nil
	case ansAdmissionRequired:
		return "", http.StatusConflict, a2ashape.Refuse(reasonSwitchPendingAdmission, args)
	case ansRefused:
		if a.refusal == reasonHubRefused {
			args["code"] = a.code
		}
		return "", http.StatusConflict, a2ashape.Refuse(a.refusal, args)
	case ansClockSkew:
		return "", http.StatusConflict, a2ashape.Refuse(reasonClockSkew, args)
	}
	return "", http.StatusBadGateway, a2ashape.Refuse(reasonHubUnreachable, args)
}

// deactivateAnswer is the /identity/deactivate answer.
type deactivateAnswer struct {
	State       string `json:"state"`
	ClosedTasks int    `json:"closed_tasks"`
}

// hIdentityDeactivate is POST /identity/deactivate (KEL §2.8, D-46).
func (d *Daemon) hIdentityDeactivate(w http.ResponseWriter, r *http.Request) {
	body, err := readLimited(r, maxIdentityBody)
	if err != nil {
		writeRefusal(w, http.StatusBadRequest, configInvalid("body", ruleText("请求体太大或读不出", "the request body is too large or unreadable")))
		return
	}
	req, ref := decodeIdentityReq(body, false)
	if ref != nil {
		writeRefusal(w, http.StatusBadRequest, ref)
		return
	}
	if !d.idOps.change.TryLock() {
		writeRefusal(w, http.StatusConflict, a2ashape.Refuse(reasonRotationPending, nil))
		return
	}
	defer d.idOps.change.Unlock()
	if status, ref := d.identityChangeRefusal(); ref != nil {
		writeRefusal(w, status, ref)
		return
	}
	open, err := d.ix.Count(interactions.ListFilter{Active: true})
	if err != nil {
		writeRefusal(w, http.StatusInternalServerError, configInvalid("interactions", err.Error()))
		return
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, deactivateAnswer{State: d.identityState().State, ClosedTasks: open})
		return
	}
	revealed := d.revealedKey(req.RevealedSeed)
	if revealed == nil {
		writeRefusal(w, http.StatusConflict, a2ashape.Refuse(reasonRecoveryMismatch, nil))
		return
	}
	witness := identityFactsOf(d.self).Witness
	err = d.ledger.whileKeyEvent(nowMillis(), func(ts uint64) error {
		if _, err := d.self.BuildDeactivation(revealed, ts); err != nil {
			return err
		}
		if err := d.self.SetPendingHubURL(d.witnessURL(witness)); err != nil {
			return err
		}
		return saveIdentity(d.layout, d.self)
	})
	if err != nil {
		if errors.Is(err, identity.ErrRecoveryMismatch) {
			writeRefusal(w, http.StatusConflict, a2ashape.Refuse(reasonRecoveryMismatch, nil))
			return
		}
		if _, pending := d.self.Pending(); !pending {
			writeRefusal(w, http.StatusInternalServerError, configInvalid("identity", err.Error()))
			return
		}
		log.Printf("anet: save the deactivation to identity.kel: %v", err)
	}
	if d.crashAt("dip-saved") {
		writeJSON(w, http.StatusOK, deactivateAnswer{State: d.identityState().State, ClosedTasks: open})
		return
	}
	closed, err := d.finishDeactivation()
	if err != nil {
		log.Printf("anet: deactivate: %v (finished at the next start)", err)
	}
	writeJSON(w, http.StatusOK, deactivateAnswer{State: d.identityState().State, ClosedTasks: closed})
}

// finishDeactivation is D-46 steps 2–5 for the pending dip: the tasks end
// (identity_deactivated, nothing sent), every epoch and interaction private
// key goes, the dip becomes the tip, the state deactivated, and every
// witness, current and former, is pushed the dip until it confirms. Each
// step can be done again: a crash between two is finished at the next
// start (D-35).
func (d *Daemon) finishDeactivation() (int, error) {
	d.idOps.pub.Lock()
	defer d.idOps.pub.Unlock()
	p, ok := d.self.Pending()
	if !ok || p.Kind != identity.PendingDip {
		return 0, nil
	}
	ev, err := pendingEvent(p)
	if err != nil || ev == nil {
		return 0, fmt.Errorf("anet: the pending deactivation cannot be read: %v", err)
	}
	if err := saveIdentity(d.layout, d.self); err != nil {
		return 0, err
	}
	var c *closedSet
	err = d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		c, err = d.closeInteractionsTx(tx, closeDeactivated)
		return err
	})
	if err != nil {
		return 0, err
	}
	d.afterClose(c)
	if d.crashAt("dip-closed") {
		return 0, errSimulatedCrash
	}
	if d.enc != nil {
		if err := d.enc.deleteAll(); err != nil {
			return 0, err
		}
	}
	if d.crashAt("dip-keys-deleted") {
		return 0, errSimulatedCrash
	}
	witness := identityFactsOf(d.self).Witness
	push := d.self.WitnessesToPush()
	seen := map[string]bool{}
	for i := range push {
		push[i].AckedSeq, push[i].NextTry = ev.Event.Seq, 0
		seen[push[i].HubAID] = true
	}
	if witness != "" && !seen[witness] {
		push = append(push, identity.WitnessPush{HubAID: witness, HubURL: d.witnessURL(witness), AckedSeq: ev.Event.Seq})
	}
	err = d.ledger.whileKeyEvent(nowMillis(), func(uint64) error {
		if err := d.self.Adopt(*ev); err != nil {
			return err
		}
		if err := d.self.SetState(identity.StateDeactivated); err != nil {
			return err
		}
		if err := d.self.SetWitnessesToPush(push); err != nil {
			return err
		}
		return saveIdentity(d.layout, d.self)
	})
	if err != nil {
		return 0, err
	}
	d.idOps.mu.Lock()
	d.idOps.acked = nil
	d.idOps.mu.Unlock()
	d.closeP2P()
	d.stopRelayLoop()
	d.kickIdentity()
	n := 0
	if c != nil {
		n = len(c.tasks)
	}
	return n, nil
}

// --- the publication sequence (KEL D-30–D-42, D-35, D-36) ---

// publishDue publishes the pending rot when an attempt is due, and returns
// the wait until the next one.
func (d *Daemon) publishDue(ctx context.Context) time.Duration {
	p, ok := d.self.Pending()
	if !ok || p.Kind == identity.PendingWitness {
		return 0
	}
	if p.Kind == identity.PendingDip {
		if _, err := d.finishDeactivation(); err != nil {
			log.Printf("anet: finish the deactivation: %v", err)
			return publishRetryMax.get()
		}
		return 0
	}
	d.idOps.mu.Lock()
	next := d.idOps.pubNextAt
	d.idOps.mu.Unlock()
	if wait := time.Until(next); !next.IsZero() && wait > 0 {
		return wait
	}
	pctx, cancel := context.WithTimeout(ctx, joinAttemptTimeout)
	defer cancel()
	d.publishNow(pctx)
	d.idOps.mu.Lock()
	defer d.idOps.mu.Unlock()
	if d.idOps.pubNextAt.IsZero() {
		return 0
	}
	return max(time.Until(d.idOps.pubNextAt), time.Second)
}

// publishNow makes one attempt to publish the pending rot to its witness
// (the target hub of a switch): the key history with the rot, the key set
// signed under its key (none for an emergency one), the card. Accepted, the
// rot is adopted and what follows it runs; refused or not answered, the
// next attempt is scheduled.
func (d *Daemon) publishNow(ctx context.Context) {
	d.idOps.pub.Lock()
	defer d.idOps.pub.Unlock()
	if d.self.State() != identity.StateOK {
		return
	}
	p, ok := d.self.Pending()
	if !ok || p.Kind != identity.PendingRot {
		return
	}
	if p.Event == nil {
		// Material without an event (a recover, KEL D-45): built now,
		// naming the witness of the KEL's tip.
		if err := d.ledger.whileKeyEvent(nowMillis(), func(ts uint64) error {
			if _, err := d.self.Restamp(ts); err != nil {
				return err
			}
			return saveIdentity(d.layout, d.self)
		}); err != nil {
			log.Printf("anet: build the recovered identity's rotation: %v", err)
			d.schedulePublish(false)
			return
		}
		if p, ok = d.self.Pending(); !ok {
			return
		}
	}
	ev, err := pendingEvent(p)
	if err != nil || ev == nil {
		log.Printf("anet: the pending rotation cannot be read: %v", err)
		return
	}
	// Written before anything is sent (S-6): a rot a hub accepts must
	// survive a crash.
	if err := saveIdentity(d.layout, d.self); err != nil {
		log.Printf("anet: save identity.kel before publishing: %v", err)
		d.schedulePublish(false)
		return
	}
	hubURL := p.HubURL
	if hubURL == "" && len(ev.Event.Witnesses) == 1 {
		hubURL = d.witnessURL(ev.Event.Witnesses[0])
	}
	kel, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		return
	}
	d.resignForPending()
	a := d.publishTo(withPublication(ctx), hubURL, kel)
	switch {
	case a.kind == ansRegistered:
		if d.crashAt("rot-accepted") {
			return
		}
		d.afterPublished(ctx, *ev, hubURL, a)
	case a.kind == ansClockSkew && a.code == hubKELTSOutOfWindow:
		d.idGate.notePublishError(hubKELTSOutOfWindow)
		if d.restampDue() {
			if err := d.restampPending(); err != nil {
				log.Printf("anet: sign the rotation again at a new time: %v", err)
			}
			d.schedulePublish(true)
			return
		}
		d.schedulePublish(false)
	default:
		if a.code != "" {
			d.idGate.notePublishError(a.code)
		}
		if isAuthRefusal(a.code) || a.code == "kel_fork" || a.code == "kel_rollback" {
			// The witness holds another record of this identity, or
			// refuses this node's key: what it holds decides (D-27).
			d.triggerHubKELCheck(hubURL, triggerRegister)
		}
		d.schedulePublish(false)
	}
}

// publishTo sends one registration of kel to hubURL, and once more with a
// proof of work when the hub asks for one (a hub switch to an open hub).
func (d *Daemon) publishTo(ctx context.Context, hubURL string, kel []byte) joinAnswer {
	cfg := d.config()
	caps := withServedCapabilities(cfg.Caps, d.providers)
	d.regMu.Lock()
	defer d.regMu.Unlock()
	a := classifyHubReply(phaseRegister, d.registerWithKEL(ctx, hubURL, cfg.Name, caps, kel, nil))
	if a.kind == ansPowRequired && a.challenge != nil {
		hubAID, _, err := d.hubIdentity(ctx, hubURL)
		if err != nil {
			return transientAnswer(phaseRegister, whyUnreachable)
		}
		nonce, err := regpow.Solve(ctx, hubAID, d.AID(), a.challenge.Seed, a.challenge.Bits)
		if err != nil {
			return transientAnswer(phaseRegisterPow, whyPow)
		}
		a = classifyHubReply(phaseRegisterPow, d.registerWithKEL(ctx, hubURL, cfg.Name, caps, kel,
			&powProof{Seed: a.challenge.Seed, Nonce: nonce}))
	}
	return a
}

// isAuthRefusal reports a hub's refusal of this node's signature.
func isAuthRefusal(code string) bool { return code == "unauthorized" || code == "key_retired" }

// schedulePublish sets when the pending event is published next: soon after
// a new time (now), else after a backoff.
func (d *Daemon) schedulePublish(now bool) {
	d.idOps.mu.Lock()
	defer d.idOps.mu.Unlock()
	if now {
		d.idOps.pubNextAt = time.Now()
		return
	}
	d.idOps.pubFailures++
	wait := min(slowBackoff(d.idOps.pubFailures), publishRetryMax.get())
	d.idOps.pubNextAt = time.Now().Add(wait)
}

// afterPublished is what follows the witness's acceptance of the pending
// rot ev (registered at hubURL, answer a): the rot is adopted; a switch
// moves the node to the new hub and starts pushing to the old (D-32 step
// 4); a planned rot is recorded and the card signed again (D-30 steps 5,
// 6); an emergency one goes on with E1–E5 (D-42).
func (d *Daemon) afterPublished(ctx context.Context, ev identity.SignedEvent, hubURL string, a joinAnswer) {
	prev := identityFactsOf(d.self).Witness
	prevURL := d.witnessURL(prev)
	witness := ""
	if len(ev.Event.Witnesses) == 1 {
		witness = ev.Event.Witnesses[0]
	}
	emergency := ev.Event.Emergency
	if emergency {
		// The gate stays closed through E1–E5 (core §4.7): set before the
		// rot is the tip, which would open it.
		d.idOps.emergencyOpen.Store(true)
	}
	if err := d.adoptPending(ev); err != nil {
		log.Printf("anet: adopt the rotation the witness accepted: %v", err)
		d.schedulePublish(false)
		return
	}
	d.idOps.mu.Lock()
	d.idOps.pubFailures, d.idOps.pubNextAt, d.idOps.restampAt = 0, time.Time{}, time.Time{}
	recover := d.idOps.recover
	d.idOps.recover = false
	d.idOps.mu.Unlock()
	d.idGate.notePublishError("")
	if witness != "" && prev != "" && witness != prev {
		d.switchedTo(hubURL, witness, prev, prevURL, ev, a)
	}
	d.noteHubNotice(a.notice, a.noticeError)
	if emergency {
		if recover {
			d.idOps.mu.Lock()
			d.idOps.recover = true // E5 records recover
			d.idOps.mu.Unlock()
		}
		d.finishEmergency(ctx)
		return
	}
	d.recordRotation(ev.Event.Seq, rotationScheduled, witness)
	// The extended card and what the modules put on the card are signed
	// again under the new key (D-30 step 6; O-2); queued rows are sealed
	// again by trigger T3 before they go (engine ENG-K14).
	d.cardInputsChanged()
	d.kickOutbox()
	d.kickIdentity()
}

// switchedTo moves the node to hub (AID witness) after the switch rot ev was
// accepted there (D-32 step 4): config.json and hub_join.json name it, the
// relay loop polls it, and the old witness is pushed the key history now
// and drained until OldHubDrain after the rot.
func (d *Daemon) switchedTo(hubURL, witness, prev, prevURL string, ev identity.SignedEvent, a joinAnswer) {
	push := d.self.WitnessesToPush()
	found := false
	for i := range push {
		if push[i].HubAID == prev {
			push[i].HubURL, push[i].AckedSeq, push[i].NextTry = prevURL, ev.Event.Seq, 0
			found = true
		}
	}
	if !found {
		push = append(push, identity.WitnessPush{HubAID: prev, HubURL: prevURL, AckedSeq: ev.Event.Seq})
	}
	if err := d.self.SetWitnessesToPush(push); err == nil {
		if err := saveIdentity(d.layout, d.self); err != nil {
			log.Printf("anet: save identity.kel: %v", err)
		}
	}
	d.idOps.mu.Lock()
	if d.idOps.acked != nil {
		delete(d.idOps.acked, prev)
	}
	d.idOps.mu.Unlock()
	if _, err := d.updateConfig(func(c *Config) error {
		c.HubURL = hubURL
		return nil
	}, func(live *Config) { live.HubURL = hubURL }); err != nil {
		log.Printf("anet: write the new hub to config.json: %v", err)
	}
	now := time.Now()
	r := *freshJoinRecord(hubURL, now)
	r.HubAID = witness
	r, _ = joinStep(r, a, now)
	d.join.mu.Lock()
	d.join.rec = &r
	d.join.mu.Unlock()
	if err := saveJoinRecord(d.layout, &r); err != nil {
		log.Printf("anet: save hub_join.json: %v", err)
	}
	d.recordJoinChange(r)
	d.startRelayLoop(hubURL)
	d.wakeJoin()
}

// --- the emergency rotation, E1–E5 (core §4.7, KEL D-41, D-42) ---

// emergencyDue runs E1–E5 when an accepted emergency rotation has not
// finished them, and returns the wait until the next try.
func (d *Daemon) emergencyDue(ctx context.Context) time.Duration {
	if !d.idOps.emergencyOpen.Load() {
		return 0
	}
	if _, pending := d.self.Pending(); pending {
		return 0
	}
	d.idOps.mu.Lock()
	next := d.idOps.emNextAt
	d.idOps.mu.Unlock()
	if wait := time.Until(next); !next.IsZero() && wait > 0 {
		return wait
	}
	d.idOps.pub.Lock()
	d.finishEmergency(ctx)
	d.idOps.pub.Unlock()
	if !d.idOps.emergencyOpen.Load() {
		return 0
	}
	return publishRetryMax.get() / 10
}

// finishEmergency is E1–E5 of the accepted emergency rot (the caller holds
// idOps.pub). Each step is done again when a crash or a failure cut the
// run short, and doing one again changes nothing that matters: E1 ends
// only the tasks still open and replaces the mailbox key once more; E2 is
// tagged with the rot's seq and done once; E3's tokens are new again.
// Which steps are done is read from disk: E2 from the key ring's
// regen_tag, E5 from the evidence chain.
//
//	E1  every open task fails with key_compromise, its interaction key
//	    deleted, a notice queued to its peer; a new mailbox key; both
//	    capability tables and the direct addresses learned emptied — one
//	    transaction, the mailbox key written and synced before it commits
//	E2  every epoch key regenerated (regen_tag = seq); the set of the new
//	    keys and the new mailbox key published
//	E3  the control token and the local A2A token replaced
//	E4  the card signed again (the extended card's cache is keyed by the
//	    key seq: nothing to do, PAY-10)
//	E5  anet.identity.rotated recorded; the gate opens, and the notices of
//	    E1 go out sealed under the new key
func (d *Daemon) finishEmergency(ctx context.Context) {
	kel := d.self.Accepted()
	tip := kel[len(kel)-1]
	if tip.Event.Type != identity.Rotation || !tip.Event.Emergency {
		d.idOps.emergencyOpen.Store(false)
		return
	}
	seq := tip.Event.Seq
	fail := func(step string, err error) {
		log.Printf("anet: emergency rotation, %s: %v (anet tries again)", step, err)
		d.idOps.mu.Lock()
		d.idOps.emNextAt = time.Now().Add(publishRetryMax.get() / 10)
		d.idOps.mu.Unlock()
	}
	if d.enc != nil && !d.enc.regeneratedFor(seq) {
		// E1.
		var c *closedSet
		err := d.ix.Update(func(tx *interactions.Tx) error {
			var err error
			if c, err = d.closeInteractionsTx(tx, closeKeyCompromise); err != nil {
				return err
			}
			return d.capsEmergencyTx(tx)
		})
		if err != nil {
			fail("E1", err)
			return
		}
		d.afterClose(c)
		if d.crashAt("E1") {
			return
		}
		// E2: the ring.
		if _, err := d.regenerateKeys(seq); err != nil {
			fail("E2", err)
			return
		}
		if d.crashAt("E2-regenerated") {
			return
		}
	}
	// E2: the publication of the new set (the registration of E0 carried
	// none).
	if hub := d.config().HubURL; hub != "" && d.enc != nil && d.publishedKeySeq.Load() != d.enc.Seq() {
		pctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
		err := d.publishKeys(withPublication(pctx), hub)
		cancel()
		if err != nil {
			fail("E2 publication", err)
			return
		}
	}
	if d.crashAt("E2-published") {
		return
	}
	// E3.
	if err := d.rotateLocalTokens(); err != nil {
		fail("E3", err)
		return
	}
	if d.crashAt("E3") {
		return
	}
	// E4: the card, under the new key.
	d.cardInputsChanged()
	// E5.
	mode := rotationEmergency
	d.idOps.mu.Lock()
	if d.idOps.recover {
		mode = rotationRecover
	}
	d.idOps.mu.Unlock()
	if d.rotationEvidence(seq) == "" {
		if d.ledger == nil {
			d.idOps.emergencyOpen.Store(false)
		} else if _, err := d.ledger.Append(evtypes.IdentityRotated, map[string]any{
			"seq": seq, "mode": mode, "witness": identityFactsOf(d.self).Witness}); err != nil {
			fail("E5", err)
			return
		}
	}
	d.idOps.mu.Lock()
	d.idOps.recover, d.idOps.emNextAt = false, time.Time{}
	d.idOps.mu.Unlock()
	d.idOps.emergencyOpen.Store(false)
	log.Printf("anet: emergency rotation to key seq %d done", seq)
	d.kickOutbox()
	d.kickIdentity()
}

// --- the former witnesses (KEL D-33, D-32 step 5) ---

// pushDue pushes the key history to each former witness that is due
// (D-33), and returns the wait until the next is.
func (d *Daemon) pushDue(ctx context.Context) time.Duration {
	list := d.self.WitnessesToPush()
	if len(list) == 0 {
		return 0
	}
	now := d.nowMS()
	var least time.Duration
	for _, w := range list {
		if w.NextTry > now {
			if wait := time.Duration(w.NextTry-now) * time.Millisecond; least == 0 || wait < least {
				least = wait
			}
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
		ok := d.pushKELTo(pctx, w.HubURL, w.AckedSeq)
		cancel()
		d.notePush(w.HubAID, ok)
		if wait := d.pushWait(w.HubAID, ok); least == 0 || wait < least {
			least = wait
		}
	}
	return least
}

// pushKELTo pushes the accepted key history to hubURL (POST
// /agents/kel:push, unauthenticated) and reports whether the hub holds it
// up to seq at least (D-33's confirmation).
func (d *Daemon) pushKELTo(ctx context.Context, hubURL string, seq uint64) bool {
	kel, err := identity.MarshalKEL(d.self.Accepted())
	if err != nil {
		return false
	}
	var out hubapi.KELPushResponse
	if err := d.hubPost(ctx, hubURL, hubapi.KELPushPath, hubapi.KELPushRequest{AID: d.AID(), KEL: hubapi.B64(kel)}, &out); err != nil {
		d.logOnce("kel-push:"+hubURL, "anet: push the key history to %s: %v (anet keeps trying)", hubURL, err)
		return false
	}
	return out.AID == d.AID() && out.Seq >= seq
}

// pushWait is how long until hubAID is pushed again: after a confirmation
// pushRepeat; after a failure a backoff from pushRetryMin doubling up to
// pushRetryMax.
func (d *Daemon) pushWait(hubAID string, ok bool) time.Duration {
	if ok {
		return pushRepeat.get()
	}
	d.idOps.mu.Lock()
	n := d.idOps.failures[hubAID]
	d.idOps.mu.Unlock()
	wait := pushRetryMin.get()
	for i := 1; i < n && wait < pushRetryMax.get(); i++ {
		wait *= 2
	}
	return min(wait, pushRetryMax.get())
}

// notePush records a push to hubAID and when the next is due; a confirmed
// dip of a deactivated identity is the last push to that hub (D-33).
func (d *Daemon) notePush(hubAID string, ok bool) {
	d.idOps.mu.Lock()
	if d.idOps.acked == nil {
		d.idOps.acked, d.idOps.failures = map[string]bool{}, map[string]int{}
	}
	if ok {
		d.idOps.acked[hubAID], d.idOps.failures[hubAID] = true, 0
	} else {
		d.idOps.failures[hubAID]++
	}
	d.idOps.mu.Unlock()
	wait := d.pushWait(hubAID, ok)
	d.idOps.pub.Lock()
	defer d.idOps.pub.Unlock()
	list := d.self.WitnessesToPush()
	var out []identity.WitnessPush
	deactivated := d.self.State() == identity.StateDeactivated
	for _, w := range list {
		if w.HubAID == hubAID {
			if ok && deactivated {
				continue // the dip is confirmed there: nothing more to push
			}
			w.NextTry = d.nowMS() + uint64(wait/time.Millisecond)
		}
		out = append(out, w)
	}
	if err := d.self.SetWitnessesToPush(out); err != nil {
		return
	}
	if err := saveIdentity(d.layout, d.self); err != nil {
		log.Printf("anet: save identity.kel: %v", err)
	}
}

// oldWitnessUnacked lists the former witnesses that have not confirmed the
// key history pushed to them (KEL §4.3.1, D-33): within OldHubDrain of the
// switch away from them, or, for a deactivated identity, until its dip is
// confirmed.
func (d *Daemon) oldWitnessUnacked() []IdentityOldWitness {
	out := []IdentityOldWitness{}
	if d.self == nil {
		return out
	}
	kel := d.self.Accepted()
	now := d.nowMS()
	deactivated := d.self.State() == identity.StateDeactivated
	d.idOps.mu.Lock()
	acked := map[string]bool{}
	for k, v := range d.idOps.acked {
		acked[k] = v
	}
	d.idOps.mu.Unlock()
	for _, w := range d.self.WitnessesToPush() {
		if acked[w.HubAID] {
			continue
		}
		var since uint64
		if w.AckedSeq < uint64(len(kel)) {
			since = kel[w.AckedSeq].Event.Timestamp
		}
		if !deactivated && (since == 0 || now > since+OldHubDrain) {
			continue
		}
		out = append(out, IdentityOldWitness{URL: w.HubURL, Since: rfc3339(since), NextTry: rfc3339(w.NextTry)})
	}
	return out
}

// drainDue reads the mailboxes of the old hubs a switch left (D-32 step 5)
// while OldHubDrain lasts, and leaves each once it is over; it returns the
// wait until the next read.
func (d *Daemon) drainDue(ctx context.Context) time.Duration {
	if d.self.State() != identity.StateOK {
		return 0
	}
	d.syncDrains()
	d.idOps.mu.Lock()
	var due []*oldHubDrain
	var least time.Duration
	now := time.Now()
	for _, dr := range d.idOps.drains {
		if dr.left {
			continue
		}
		if dr.nextPoll.After(now) {
			if w := dr.nextPoll.Sub(now); least == 0 || w < least {
				least = w
			}
			continue
		}
		due = append(due, dr)
	}
	d.idOps.mu.Unlock()
	for _, dr := range due {
		if d.nowMS() >= dr.until {
			// The drain is over: leave the old hub, once (failures are only
			// logged). Its join record is the new hub's, untouched.
			// The join flow's, as hub-leave is (LEC AD-8).
			lctx, cancel := context.WithTimeout(withJoinFlow(ctx), hubCallTimeout)
			if _, err := d.LeaveHub(lctx, dr.url); err != nil {
				log.Printf("anet: leave the old hub %s after the switch: %v", dr.url, err)
			}
			cancel()
			d.idOps.mu.Lock()
			dr.left = true
			d.idOps.mu.Unlock()
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, relayCallTimeout)
		d.pollMu.Lock()
		err := d.pollHubOnce(pctx, dr.url, &dr.cur, false)
		d.pollMu.Unlock()
		cancel()
		if err != nil && hubStatus(err) == http.StatusUnauthorized {
			// The old hub does not know the key yet: push it the history,
			// and read again next time (not D-27: it is not the witness).
			pctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
			d.pushKELTo(pctx, dr.url, 0)
			cancel()
		}
		d.idOps.mu.Lock()
		dr.nextPoll = time.Now().Add(oldHubPollEvery.get())
		d.idOps.mu.Unlock()
		if w := oldHubPollEvery.get(); least == 0 || w < least {
			least = w
		}
	}
	return least
}

// syncDrains keeps one drain per former witness whose switch is within
// OldHubDrain, from witnesses_to_push and the KEL (so a restart drains
// what it drained before).
func (d *Daemon) syncDrains() {
	kel := d.self.Accepted()
	cur := identityFactsOf(d.self).Witness
	d.idOps.mu.Lock()
	defer d.idOps.mu.Unlock()
	if d.idOps.drains == nil {
		d.idOps.drains = map[string]*oldHubDrain{}
	}
	for _, w := range d.self.WitnessesToPush() {
		if w.HubAID == cur || w.HubURL == "" || w.AckedSeq >= uint64(len(kel)) {
			continue
		}
		if _, ok := d.idOps.drains[w.HubAID]; ok {
			continue
		}
		d.idOps.drains[w.HubAID] = &oldHubDrain{url: strings.TrimRight(w.HubURL, "/"),
			until: kel[w.AckedSeq].Event.Timestamp + OldHubDrain}
	}
}

// drainingHubs are the old hubs whose mailboxes are still read (FS-K6a:
// the epoch keys their mail is sealed to stay until they are drained).
func (d *Daemon) drainingHubs() []string {
	d.idOps.mu.Lock()
	defer d.idOps.mu.Unlock()
	var out []string
	for _, dr := range d.idOps.drains {
		if !dr.left {
			out = append(out, dr.url)
		}
	}
	return out
}

// --- the check of the witness's record (KEL D-27, D-29) ---

// What made the daemon look at its witness's record of this identity.
const (
	triggerPoll401    = "poll-401"    // a poll refused this node's signature
	triggerSession401 = "session-401" // a first-contact session did
	triggerRegister   = "register"    // a registration: 401, kel_fork, kel_rollback
	triggerNoticeTip  = "notice-tip"  // a notice stating another tip
	triggerQuarantine = "quarantine"  // key_state_notice_error quarantine
)

// triggerHubKELCheck looks at the witness's record of this identity (D-27),
// in the background, when hubURL is the witness: the checks of other hubs
// (an old hub after a switch) decide nothing about the identity.
func (d *Daemon) triggerHubKELCheck(hubURL, why string) {
	if d.self == nil || d.self.State() != identity.StateOK {
		return
	}
	witness := identityFactsOf(d.self).Witness
	if witness == "" || !sameHub(hubURL, d.witnessURL(witness)) {
		return
	}
	d.idOps.mu.Lock()
	if d.idOps.checking {
		d.idOps.mu.Unlock()
		return
	}
	if !d.idOps.checkAt.IsZero() && time.Since(d.idOps.checkAt) < hubKELCheckEvery.get() {
		d.idOps.mu.Unlock()
		d.count("hub-kel-check-later")
		return
	}
	d.idOps.checking, d.idOps.checkAt = true, time.Now()
	d.idOps.mu.Unlock()
	d.goBackground(func() {
		defer func() {
			d.idOps.mu.Lock()
			d.idOps.checking = false
			d.idOps.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
		defer cancel()
		d.checkHubKEL(ctx, hubURL, why)
	})
}

// checkHubKEL is D-27 steps 1–3: the witness's record, classified, acted
// on. Only rows e and f change the identity file's state.
func (d *Daemon) checkHubKEL(ctx context.Context, hubURL, why string) {
	var out hubapi.AgentKELResponse
	err := d.hubGet(ctx, hubURL, "/agents/"+url.PathEscape(d.AID())+"/kel", nil, &out)
	if err != nil {
		d.logOnce("hub-kel-check:"+hubURL, "anet: read the witness's record of this identity at %s: %v", hubURL, err)
		return
	}
	var hub []identity.SignedEvent
	if raw, err := hubapi.DecodeB64(out.KEL, -1); err == nil && out.AID == d.AID() {
		hub, _ = seal.ParseKEL(raw)
	}
	var pending *identity.SignedEvent
	if p, ok := d.self.Pending(); ok {
		pending, _ = pendingEvent(p)
	}
	verdict := classifyHubKEL(d.self.Accepted(), pending, hub)
	d.idOps.hubRecordInvalid.Store(verdict == hubKELInvalid)
	switch verdict {
	case hubKELInvalid:
		d.count("hub-kel-invalid")
		d.logOnce("hub-kel-invalid", "anet: the witness hub %s holds a record of this identity that does not verify; the identity is not affected", hubURL)
	case hubKELAccepted:
		// The witness took the pending rot whatever its answer said.
		if pending == nil {
			return
		}
		d.idOps.pub.Lock()
		if p, ok := d.self.Pending(); ok && p.Kind == identity.PendingRot {
			d.afterPublished(ctx, *pending, hubURL, joinAnswer{kind: ansRegistered})
		}
		d.idOps.pub.Unlock()
	case hubKELBehind:
		d.count("hub-kel-behind")
		if _, publishing := d.self.Pending(); !publishing {
			// Pushed the longer history now (H-22: a quarantine ends within
			// seconds for an owner that is online), and registered again.
			d.pushKELTo(ctx, hubURL, uint64(len(d.self.Accepted())-1))
			d.kickJoin()
		}
	case hubKELSame:
		d.count("hub-kel-same")
		if !out.Registered && (why == triggerPoll401 || why == triggerSession401) {
			// The hub keeps this identity only as a tombstone: admission
			// revoked, removed, or swept. Back to joining (LEC AD-5), so the
			// hub row says what the operator has to do.
			d.rejoin(hubURL)
		}
	case hubKELNewer, hubKELFork:
		d.enterStopped(fmt.Sprintf("the witness hub %s holds a %s key history of this identity", hubURL, verdict))
	}
}

// rejoin sends the join of hubURL back to its start: the record is fresh
// (check_only first), the loop tries at once.
func (d *Daemon) rejoin(hubURL string) {
	d.join.mu.Lock()
	r := d.join.rec
	if r == nil || !sameHub(r.Hub, hubURL) {
		d.join.mu.Unlock()
		return
	}
	fresh := freshJoinRecord(r.Hub, time.Now())
	fresh.HubAID = r.HubAID
	d.join.rec = fresh
	cp := *fresh
	d.join.mu.Unlock()
	if err := saveJoinRecord(d.layout, &cp); err != nil {
		log.Printf("anet: save hub_join.json: %v", err)
	}
	d.recordJoinChange(cp)
	d.kickJoin()
}

// enterStopped stops the identity for good (D-27 rows e, f; D-29): its
// state is written stopped, every task in flight ends with
// identity_stopped (nothing sent), every interaction private key goes, the
// direct transports close (D-1) and nothing is polled or published again.
// The epoch keys stay: this node can still read what it received.
func (d *Daemon) enterStopped(why string) {
	if d.self.State() != identity.StateOK {
		return
	}
	if err := d.self.SetState(identity.StateStopped); err != nil {
		return
	}
	if err := saveIdentity(d.layout, d.self); err != nil {
		log.Printf("anet: save identity.kel: %v", err)
	}
	log.Printf("anet: identity stopped: %s", why)
	if d.crashAt("stopped-saved") {
		return
	}
	d.closeLeftAtStart()
	d.closeP2P()
	d.stopRelayLoop()
}

// closeP2P stops the direct transports (KEL D-1, second part): no
// envelope leaves through them, and none comes in (inbound refuses it).
func (d *Daemon) closeP2P() {
	d.transportMu.Lock()
	ts := d.extraTransports
	d.extraTransports = nil
	d.transportMu.Unlock()
	for _, t := range ts {
		if c, ok := t.(interface{ Close() error }); ok {
			if err := c.Close(); err != nil {
				log.Printf("anet: close transport %s: %v", t.Name(), err)
			}
		}
	}
}

// identityRefusesInbound reports whether nothing may come in: the identity
// is stopped or deactivated (D-1).
func (d *Daemon) identityRefusesInbound() bool {
	if d.self == nil {
		return false
	}
	switch d.self.State() {
	case identity.StateStopped, identity.StateDeactivated:
		return true
	}
	return false
}

// errIdentityGone refuses a direct delivery to a stopped or deactivated
// identity: the sender's transport answers it with a nack.
var errIdentityGone = errors.New("anet: this node's identity is stopped or deactivated; not accepting")

// The sentence keys of the identity operations (ux §1.4 E2).
const (
	reasonWitnessUnreachable     = "witness_unreachable"
	reasonRecoveryMismatch       = "recovery_mismatch"
	reasonSwitchPendingAdmission = "hub_switch_pending_admission"
)
