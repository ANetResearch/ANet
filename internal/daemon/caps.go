package daemon

// caps.go is the daemon's delivery credential policy (sealed-sender §4.3.2
// to §4.3.10): which credential this node gives a peer with each message
// (grantFor), which one it sends under (accessFor), what it does when the
// hub refuses one (onAccessRefused), when a credential it gave was misused
// (capMisuse, grantMisuse), and which rate bucket a direct delivery is
// charged to (p2pRxBucket).
//
// The decisions are pure functions over facts the caller gathers (engine
// ENG-G2): no I/O, the time is a parameter, and each has a table-driven
// test. Beside each is the glue that gathers those facts from the daemon
// and applies a decision inside a transaction of the interactions store,
// whose two tables (caps_issued, caps_held) and row rules are SS-7's.
//
// Who calls what:
//
//   - the outbound engine (ENG-1a, 1b) calls grantForMessage when it seals
//     a version and commitGrantTx in the transaction that writes the
//     version (ENG §2.4 A8, SSR-D10); cardGetGrant before it seals an
//     anet.card.get/1; accessChoiceFor on every attempt (A9) and
//     accessFromChoice for outer key 7; accessRefusedTx on a 403
//     access_invalid and accessAcceptedTx on a 200 (SSR-D11);
//   - the receive pipeline (receive.go) calls p2pRxBucket at step 0 of a
//     direct delivery and at step 1a of a mailbox envelope (capUnder),
//     capMisuse when one under a capability fails at steps 3 to 7
//     (misused), grantMisuse at step 7½, and grantToKeep at step 8, whose
//     result it keeps in its step-10 transaction (SSR-W8).
//
// The revocations these decisions make reach the hub through
// revoke_push.go.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// Lifetimes and bounds of the credentials this node issues and holds (SS
// §2.12, SSR-W4, SSR-D7, SSR-D11). Code constants: there is nothing for an
// operator to tune.
const (
	// capLifeContactMS is the life of a contact peer capability.
	capLifeContactMS uint64 = 30 * 24 * 3600 * 1000
	// capLifeStrangerMS is the life of a stranger peer capability and of
	// every one-shot capability.
	capLifeStrangerMS uint64 = 7 * 24 * 3600 * 1000
	// capLifeCardMS is the life of the one-shot capability an
	// anet.card.get/1 carries for the price card to come back under.
	capLifeCardMS uint64 = 15 * 60 * 1000
	// capValidMarginMS: a held capability is used while its cap_exp is at
	// least this far ahead.
	capValidMarginMS uint64 = 60 * 1000
	// maxStrangerOneShots bounds the stranger one-shot capabilities one
	// peer holds from this node at once.
	maxStrangerOneShots = 8
	// refusalDowngradeN and refusalDowngradeAfterMS are the downgrade
	// threshold of SSR-D11: a held capability the hub refused this many
	// times without a reason anet could find, the first of them this long
	// ago, is given up for a first contact.
	refusalDowngradeN                = 3
	refusalDowngradeAfterMS   uint64 = 10 * 60 * 1000
	refusalDowngradesWindowMS uint64 = 24 * 3600 * 1000
)

// Counters (ReceiveStats) the policy keeps.
const (
	cntGrantInvalid        = "grant-invalid"              // a grant that failed seal.CheckGrant, ignored (CORE-K4)
	cntCapMisuse           = "cap-misuse"                 // a capability this node issued revoked for misuse (SS §4.3.8 item 2)
	cntRefusalUnexplained  = "access-invalid-unexplained" // a 403 access_invalid anet found no reason for (SSR-D11)
	cntRefusalDowngrade    = "access-invalid-downgrade"   // a held capability given up after such refusals
	cntRefusalExplained    = "access-invalid-explained"   // a 403 explained by a higher epoch or the expiry
	cntGrantNoMailboxKey   = "grant-no-mailbox-key"       // a message went without a grant: no mailbox key yet
	cntGrantNoHome         = "grant-no-home"              // a message went without a grant: no hub to name as home
	cntGrantStaleAtCommit  = "grant-stale"                // a planned grant no longer held at commit; sealed again
	cntRevokeListsSkipped  = "revoke-lists-unreadable"    // a peer list could not be read; nothing revoked by it
	cntRevokeListsReconcil = "revoke-lists-reconciled"    // capabilities revoked because the lists changed outside the CLI
)

// ---- what a message is to the policy

// capMsgKind is the kind of message grantFor and accessFor decide for.
type capMsgKind uint8

const (
	// capMsgOther is any message of the outbound queue that is none of the
	// below: a status, a result, a chat message, a cancel, a payment.
	capMsgOther capMsgKind = iota
	// capMsgDelegate is a delegation (anet.delegate/1).
	capMsgDelegate
	// capMsgNotice is an after-the-fact notice of a delegation this node
	// held or refused: pending_approval, pending_expired,
	// operator_rejected (engine ENG-Q7).
	capMsgNotice
	// capMsgPreview is a stream_preview frame.
	capMsgPreview
	// capMsgCardGet is an anet.card.get/1, the request for a price card.
	capMsgCardGet
	// capMsgCard is an anet.card/1, the price card that answers one.
	capMsgCard
)

// capMsg is the message: its kind and the interaction (inner ix) it is on.
type capMsg struct {
	Kind capMsgKind
	IX   string
}

// capRole is how this node stands in the message's interaction.
type capRole uint8

const (
	// capRoleNone: the ix names no interaction this node has with G.
	capRoleNone capRole = iota
	// capRoleRequester: this node started the interaction.
	capRoleRequester
	// capRoleUnaccepted: G started it and this node has not accepted it —
	// it is held for approval, or was refused or rejected (SS §4.3.2 rule
	// 2).
	capRoleUnaccepted
	// capRoleProvider: G started it and this node accepted it (public,
	// public_cap, open or approved).
	capRoleProvider
)

// capRel is what grantFor needs to know about the peer G a message goes to.
type capRel struct {
	// Listed: G is on the allow or trust list and not denied.
	Listed bool
	// Denied: G is on the deny list (or the list cannot be read, which
	// denies everyone).
	Denied bool
	Role   capRole
	// MaybePaid is maybePaid for the message's interaction.
	MaybePaid bool
	// StrangerOneShots counts the stranger one-shot capabilities valid now
	// that G holds from this node for other interactions than this one.
	StrangerOneShots int
	// P2P is modules.p2p.advertise, "" when not configured.
	P2P string
	// Prices: this node prices some capability (the grant's pay).
	Prices bool
}

// ---- issuing: grantFor (SS §4.3.2)

// GrantDecision is what grantFor decides for inner key 14 of one message:
// no grant, or a capability of Scope (and IX for a one-shot), Class and
// CapExp, with P2P (Grant key 5) and Pay (key 6, written only when true).
type GrantDecision struct {
	// Rule is the row of SS §4.3.2 that decided (0 to 5), or ruleDenied.
	Rule   int
	Issue  bool
	Scope  string
	IX     string
	Class  uint64
	CapExp uint64
	P2P    string
	Pay    bool
}

// ruleDenied is GrantDecision.Rule for a peer on the deny list.
const ruleDenied = -1

// classFor is the class of a one-shot capability for a possibly paid
// interaction (SS §4.3.2): contact for a listed peer or when this node is
// the requester, stranger otherwise.
func classFor(rel capRel) uint64 {
	if rel.Listed || rel.Role == capRoleRequester {
		return seal.CapClassContact
	}
	return seal.CapClassStranger
}

// grantFor is SSR-D5: the capability, if any, this node gives G with one
// message, by the first row of SS §4.3.2 that matches. Every message to G
// carries the grant current at sealing (engine ENG-K5); renewal and reuse
// are SSR-D6 (planGrant).
//
// One reading of the table (SS-8 notes): a peer on the deny list gets no
// capability at all, except the reply capability of this node's own
// anet.card.get/1. Entering the deny list revokes every capability given
// to the peer (§4.3.8 item 1), and a message sent to a denied peer — a
// cancel, a paid task this node still finishes — does not issue a new one;
// the peer answers by first contact.
func grantFor(msg capMsg, rel capRel, now uint64) GrantDecision {
	oneShot := func(rule int, class, life uint64) GrantDecision {
		if msg.IX == "" {
			return GrantDecision{Rule: rule}
		}
		return GrantDecision{Rule: rule, Issue: true, Scope: interactions.CapScopeIX, IX: msg.IX, Class: class,
			CapExp: now + life}
	}
	switch msg.Kind {
	case capMsgCardGet:
		// Rule 0: the price card comes back under this (core G11).
		return oneShot(0, seal.CapClassContact, capLifeCardMS)
	case capMsgPreview, capMsgNotice, capMsgCard:
		// Rule 1 (core CORE-K3, X-SS-12).
		return GrantDecision{Rule: 1}
	}
	if rel.Denied {
		return GrantDecision{Rule: ruleDenied}
	}
	if rel.Role == capRoleUnaccepted {
		return GrantDecision{Rule: 2}
	}
	if rel.MaybePaid {
		// Rule 3: inside a possibly paid interaction only one-shot
		// capabilities, so a settlement links the payer to this interaction
		// alone (HPP R4).
		class := classFor(rel)
		if class == seal.CapClassStranger && rel.StrangerOneShots >= maxStrangerOneShots {
			return GrantDecision{Rule: 3} // SSR-D7: the peer goes by first contact
		}
		return oneShot(3, class, capLifeStrangerMS)
	}
	if rel.Role == capRoleRequester && !rel.Listed {
		return oneShot(4, seal.CapClassContact, capLifeStrangerMS)
	}
	switch {
	case rel.Listed:
		return GrantDecision{Rule: 5, Issue: true, Scope: interactions.CapScopePeer, Class: seal.CapClassContact,
			CapExp: now + capLifeContactMS, P2P: rel.P2P, Pay: rel.Prices}
	case rel.Role == capRoleProvider:
		return GrantDecision{Rule: 5, Issue: true, Scope: interactions.CapScopePeer, Class: seal.CapClassStranger,
			CapExp: now + capLifeStrangerMS, Pay: rel.Prices}
	}
	return GrantDecision{Rule: 5}
}

// paidFacts is what maybePaid decides from, for one interaction with G.
type paidFacts struct {
	// Provider and Requester: this node's role in the interaction (neither
	// when the ix names none).
	Provider, Requester bool
	// Quoted: the interaction has a payment state: a payment-required went
	// out (provider) or came in (requester).
	Quoted bool
	// CallPriced (provider): the capability called is priced here.
	CallPriced bool
	// HeldPeerPay (requester): the peer capability G gave this node says G
	// prices something (Grant key 6).
	HeldPeerPay bool
}

// maybePaid is SS §4.3.2 "可能付费": a provider that sent payment-required
// or serves a priced capability; a requester whose provider's peer
// capability says it prices something, or that received payment-required.
func maybePaid(f paidFacts) bool {
	return f.Provider && (f.Quoted || f.CallPriced) || f.Requester && (f.HeldPeerPay || f.Quoted)
}

// ---- sending: accessFor (SS §4.3.3)

// accessMode is how a message is sent (engine ENG-K2).
type accessMode uint8

const (
	// accessOrdinary: a row of the outbound queue.
	accessOrdinary accessMode = iota
	// accessNotice: one of the three after-the-fact notices (ENG-Q7).
	accessNotice
	// accessPreview: a stream_preview frame.
	accessPreview
	// accessCardGet: an anet.card.get/1.
	accessCardGet
	// accessCard: an anet.card/1.
	accessCard
)

// accessRow is what accessFor needs about the message and its recipient R.
type accessRow struct {
	To        string
	IX        string
	Delegate  bool
	MaybePaid bool
	// PeerEpoch is the mailbox epoch of R's key set as this node holds it,
	// 0 when it holds none: a capability under another epoch no longer
	// delivers (SSR-W11).
	PeerEpoch uint64
}

// AccessChoice is outer key 7 for one attempt: Send false means the
// message is not sent at all (a notice, preview or card without a
// capability to go under); otherwise Kind is seal.AccessKindSession (a
// first contact) or seal.AccessKindCap with Cap, the held capability used.
// Home is the X-ANet-Home of a kind-1 send (the capability's home);
// HintHome, for a kind-0 send, is the home of any valid capability R gave
// (the second source of SS §4.3.6, after R's witness). Nothing of it is
// stored (ENG-I2).
type AccessChoice struct {
	Rule     int
	Send     bool
	Kind     uint64
	Cap      *interactions.HeldCap
	Home     string
	HintHome string
}

// heldValid reports whether this node may send to row.To under h: not
// given up, at least capValidMarginMS from expiry, and under R's current
// mailbox epoch when this node knows it (SS §4.3.3).
func heldValid(h *interactions.HeldCap, row accessRow, now uint64) bool {
	return h != nil && h.Issuer == row.To && h.InvalidAt == 0 && h.CapExp >= now+capValidMarginMS &&
		(row.PeerEpoch == 0 || h.Epoch == row.PeerEpoch)
}

// accessFor is SSR-D9: the credential an attempt goes under, by the first
// matching row of SS §4.3.3 for the mode (engine ENG-K1, K2). held is what
// R gave this node (caps_held rows of issuer R).
func accessFor(row accessRow, held []interactions.HeldCap, mode accessMode, now uint64) AccessChoice {
	var oneShot, peer *interactions.HeldCap
	for i := range held {
		h := &held[i]
		if !heldValid(h, row, now) {
			continue
		}
		switch {
		case h.Scope == interactions.CapScopeIX && row.IX != "" && h.IX == row.IX:
			oneShot = h
		case h.Scope == interactions.CapScopePeer:
			peer = h
		}
	}
	kind1 := func(rule int, h *interactions.HeldCap) AccessChoice {
		return AccessChoice{Rule: rule, Send: true, Kind: seal.AccessKindCap, Cap: h, Home: h.Home}
	}
	kind0 := func(rule int) AccessChoice {
		c := AccessChoice{Rule: rule, Send: true, Kind: seal.AccessKindSession}
		switch {
		case peer != nil:
			c.HintHome = peer.Home
		case oneShot != nil:
			c.HintHome = oneShot.Home
		}
		return c
	}
	none := func(rule int) AccessChoice { return AccessChoice{Rule: rule} }
	switch mode {
	case accessNotice, accessPreview:
		if oneShot != nil {
			return kind1(1, oneShot)
		}
		if !row.MaybePaid && peer != nil {
			return kind1(4, peer)
		}
		return none(0)
	case accessCardGet:
		// The paid path (core X-HPP-2): a provider that prices something
		// sees the request as a first contact.
		if peer != nil && !peer.Pay {
			return kind1(4, peer)
		}
		return kind0(5)
	case accessCard:
		if oneShot != nil {
			return kind1(1, oneShot)
		}
		return none(0)
	}
	switch {
	case oneShot != nil:
		return kind1(1, oneShot)
	case row.Delegate:
		// M17: a delegation never goes under a peer capability of a
		// provider that prices something.
		if peer != nil && !peer.Pay {
			return kind1(2, peer)
		}
		return kind0(2)
	case row.MaybePaid:
		return kind0(3)
	case peer != nil:
		return kind1(4, peer)
	}
	return kind0(5)
}

// accessFromChoice builds outer key 7 for the envelope whose env_digest is
// envDigest (seal.EnvDigest of the version's bytes): a first contact, or
// the capability with the holder's proof of possession signed now (core
// §3.2 L5). The proof is signed again for every attempt and never stored
// (engine ENG-I2, ENG-K1).
func accessFromChoice(c AccessChoice, envDigest [32]byte) (*seal.Access, error) {
	if !c.Send {
		return nil, errors.New("anet: this message has no credential to go under")
	}
	if c.Kind == seal.AccessKindSession {
		return &seal.Access{Kind: seal.AccessKindSession}, nil
	}
	if c.Kind != seal.AccessKindCap || c.Cap == nil {
		return nil, fmt.Errorf("anet: no credential of kind %d", c.Kind)
	}
	capw, err := seal.UnmarshalCap(c.Cap.Cap)
	if err != nil {
		return nil, fmt.Errorf("anet: the stored delivery credential from %s: %w", c.Cap.Issuer, err)
	}
	if len(c.Cap.HolderSeed) != ed25519.SeedSize {
		return nil, fmt.Errorf("anet: the stored delivery credential from %s has no holder key", c.Cap.Issuer)
	}
	holder := ed25519.NewKeyFromSeed(c.Cap.HolderSeed)
	defer clear(holder)
	proof := seal.SignAccess(holder, c.Cap.CapID, envDigest)
	if proof == nil {
		return nil, fmt.Errorf("anet: cannot sign the proof for the delivery credential from %s", c.Cap.Issuer)
	}
	return &seal.Access{Kind: seal.AccessKindCap, Cap: capw, Proof: proof}, nil
}

// ---- a refusal: onAccessRefused (SSR-D11)

// reviewVerdict is what resolvePeer concluded when it fetched R's material
// again, bypassing its caches, after a 403 (SS §4.3.7 SSR-D15).
type reviewVerdict uint8

const (
	reviewUnavailable  reviewVerdict = iota // the hub could not be asked, or its answer was partial
	reviewFound                             // a key set of R that verifies
	reviewNotFound                          // no entry for R
	reviewUnverifiable                      // entries, none that verifies
)

// peerReview is the result of that fetch: the verdict and, when found, the
// mailbox epoch of the verified key set.
type peerReview struct {
	Verdict reviewVerdict
	Epoch   uint64
}

// refusalAction is what to do with the held capability a 403 refused.
type refusalAction uint8

const (
	// refusalReselect: the refusal is explained by the capability being
	// about to expire, or the capability is not the row's any more, or was
	// given up already. The row is left as it is; choose again at once, the
	// same version again, no failure counted.
	refusalReselect refusalAction = iota + 1
	// refusalInvalidate: explained by a verified key set of R with a higher
	// mailbox epoch: invalid_at, choose again at once, no failure counted.
	refusalInvalidate
	// refusalWait: unexplained and below the threshold: the attempt is a
	// temporary failure (credential_refused), the capability stays in use.
	refusalWait
	// refusalDowngrade: unexplained at the threshold: invalid_at, choose
	// again at once (usually a first contact).
	refusalDowngrade
)

// RefusalDecision is onAccessRefused's answer. RefusedN and RefusedSince
// are the row's counters after this refusal (refusalWait and
// refusalDowngrade); RetryBy (refusalWait) is the latest time the next
// attempt may be scheduled for, refused_since + 10 minutes, so that the
// downgrade is not delayed past the threshold by a longer backoff.
type RefusalDecision struct {
	Action       refusalAction
	RefusedN     uint64
	RefusedSince uint64
	RetryBy      uint64
}

// Reselect reports whether the attempt chooses its credential again and
// sends the same version at once, without counting a failure.
func (r RefusalDecision) Reselect() bool { return r.Action != refusalWait }

// WaitCode is the last_error prefix and anet.delivery_wait of a waiting
// attempt (ENG-S4, ux C2), "" when the attempt does not wait.
func (r RefusalDecision) WaitCode() string {
	if r.Action == refusalWait {
		return "credential_refused"
	}
	return ""
}

// onAccessRefused is SSR-D11: the hub refused held, the capability an
// attempt went under, with 403 access_invalid, and review is what fetching
// R's material again found. The hub alone knows what it revoked, so a
// refusal is taken as explained only by what R signed (a higher mailbox
// epoch) or by this node's clock (the capability expiring); a verdict of
// not found, unverifiable or unavailable explains nothing, because a hub
// can hide an entry. An unexplained refusal is a wait until the same
// capability has been refused refusalDowngradeN times, the first
// refusalDowngradeAfterMS ago: a hub that lies once cannot take the
// sender's anonymity away, and one that keeps lying is seen (SI-86, doctor
// sender_privacy.hub).
func onAccessRefused(held interactions.HeldCap, review peerReview, now uint64) RefusalDecision {
	switch {
	case held.InvalidAt != 0:
		return RefusalDecision{Action: refusalReselect}
	case review.Verdict == reviewFound && review.Epoch > held.Epoch:
		return RefusalDecision{Action: refusalInvalidate}
	case held.CapExp < now+capValidMarginMS:
		// Not invalid_at: the issuer renews under the same cap_id, and that
		// renewal must be taken (SSR-D11 item 2).
		return RefusalDecision{Action: refusalReselect}
	}
	n, since := held.RefusedN+1, held.RefusedSince
	if since == 0 {
		since = now
	}
	if n >= refusalDowngradeN && now >= since && now-since >= refusalDowngradeAfterMS {
		return RefusalDecision{Action: refusalDowngrade, RefusedN: n, RefusedSince: since}
	}
	return RefusalDecision{Action: refusalWait, RefusedN: n, RefusedSince: since, RetryBy: since + refusalDowngradeAfterMS}
}

// accessRefusedTx applies onAccessRefused to the held capability an
// attempt used — the row (issuer, scope, ix) while it still holds capID —
// in the transaction that records the attempt's outcome (engine §2.4 A12).
// A row that no longer holds capID (a grant replaced it meanwhile, or it
// was pruned) is not counted against: what the hub said about one
// capability says nothing about another (refusalReselect).
func accessRefusedTx(tx *interactions.Tx, issuer, scope, ix string, capID []byte, review peerReview, now uint64) (RefusalDecision, error) {
	h, err := tx.HeldCap(issuer, scope, ix)
	if errors.Is(err, interactions.ErrNotFound) || err == nil && !bytes.Equal(h.CapID, capID) {
		return RefusalDecision{Action: refusalReselect}, nil
	}
	if err != nil {
		return RefusalDecision{}, err
	}
	dec := onAccessRefused(*h, review, now)
	switch dec.Action {
	case refusalInvalidate:
		_, err = tx.MarkHeldInvalid(issuer, scope, ix, capID, now)
	case refusalWait, refusalDowngrade:
		n, since, ok, nerr := tx.NoteHeldRefused(issuer, scope, ix, capID, now)
		if nerr != nil {
			return RefusalDecision{}, nerr
		}
		if !ok { // not reachable: the row was read valid in this transaction
			return RefusalDecision{Action: refusalReselect}, nil
		}
		dec.RefusedN, dec.RefusedSince = n, since
		if dec.Action == refusalDowngrade {
			_, err = tx.MarkHeldInvalid(issuer, scope, ix, capID, now)
		}
	}
	return dec, err
}

// accessAcceptedTx is SSR-D11 item 4: the hub accepted a send under the
// held capability capID (200 queued or duplicate), so its unexplained
// refusals are forgotten, in the transaction that records the outcome.
func accessAcceptedTx(tx *interactions.Tx, issuer, scope, ix string, capID []byte) error {
	_, err := tx.ClearHeldRefused(issuer, scope, ix, capID)
	return err
}

// noteRefusal counts a refusal decision once its transaction has
// committed, and remembers a downgrade for
// /status.sender_privacy.refusal_downgrades_24h (SS §4.8).
func (d *Daemon) noteRefusal(dec RefusalDecision, now uint64) {
	switch dec.Action {
	case refusalInvalidate, refusalReselect:
		d.count(cntRefusalExplained)
	case refusalWait:
		d.count(cntRefusalUnexplained)
	case refusalDowngrade:
		d.count(cntRefusalUnexplained)
		d.count(cntRefusalDowngrade)
		d.caps.noteDowngrade(now)
	}
}

// capsState is the policy's state in memory: the downgrades of the last
// 24 hours (SSR-U2 refusal_downgrades_24h; cleared by a restart), the
// direct delivery buckets (p2pRxLimiter) and the revocation push
// (revoke_push.go).
type capsState struct {
	mu         sync.Mutex
	downgrades []uint64 // times, oldest first
	rx         p2pRxLimiter
	push       revokePush
}

func (c *capsState) noteDowngrade(now uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.downgrades = append(c.downgrades, now)
	c.trimLocked(now)
}

func (c *capsState) trimLocked(now uint64) {
	i := 0
	for i < len(c.downgrades) && now >= c.downgrades[i] && now-c.downgrades[i] >= refusalDowngradesWindowMS {
		i++
	}
	c.downgrades = c.downgrades[i:]
}

// refusalDowngrades24h is how many held capabilities were given up after
// unexplained refusals in the 24 hours up to now (SSR-U2).
func (d *Daemon) refusalDowngrades24h(now uint64) int {
	d.caps.mu.Lock()
	defer d.caps.mu.Unlock()
	d.caps.trimLocked(now)
	return len(d.caps.downgrades)
}

// ---- misuse: capMisuse and grantMisuse (SS §4.3.8 item 2, §4.3.10)

// rxStep is a step of the receive pipeline v3 (engine §4.2), in order.
type rxStep uint8

const (
	rxStep0 rxStep = iota
	rxStep0a
	rxStep1
	rxStep1a
	rxStep2
	rxStep3
	rxStep3a
	rxStep4
	rxStep4a
	rxStep5
	rxStep5a
	rxStep6
	rxStep6b // 6½
	rxStep7
	rxStep7b // 7½
	rxStep8
	rxStep8b // 8½
	rxStep9
	rxStep10
)

// capMisuseReasons are the failures of steps 3 to 7 that an envelope
// arriving under a capability this node issued cannot have by accident:
// the holder used the capability for bytes that do not open, are not a
// valid message to this node, or are not signed by the sender they name.
var capMisuseReasons = map[string]bool{
	seal.ReasonDecrypt: true, seal.ReasonBadWrap: true, seal.ReasonBadRK: true, seal.ReasonIKeyMismatch: true,
	seal.ReasonBadInner: true, seal.ReasonUnknownField: true, seal.ReasonToMismatch: true,
	seal.ReasonExpMismatch: true, "bad-kind-keys": true, "kel-fork": true, seal.ReasonBadKEL: true,
	seal.ReasonWitnessMismatch: true, seal.ReasonWitnessInvalid: true, seal.ReasonFromMismatch: true, seal.ReasonBadSig: true,
	seal.ReasonBadKeyState: true, seal.ReasonRevokedKey: true, seal.ReasonGraceExpired: true,
}

// capBenignReasons are the failures of steps 3 to 7 that an honest holder
// can meet (a key this node deleted, a closed interaction, a clock or an
// expiry, a witness notice not yet seen): they never revoke. Every
// temporary failure (t-…) is benign as well. Listed for the table test;
// capMisuse revokes only for capMisuseReasons.
var capBenignReasons = []string{
	seal.ReasonUnknownKey, seal.ReasonSealedToClosedInteraction, seal.ReasonExpired, seal.ReasonFromFuture,
	seal.ReasonBadLifetime, seal.ReasonKELFromFuture, seal.ReasonWitnessMissing, seal.ReasonWitnessStale,
	seal.ReasonWitnessUnknown,
}

// capMisuse reports whether an envelope that arrived under a capability
// this node issued, checked at step 0 or 1a, and then failed at step with
// reason, revokes that capability (SS §4.3.8 item 2, engine ENG-V7). A
// failure before the capability was checked (steps 0 to 2) or after the
// message was authenticated (steps 8 on) is not the holder's misuse. A
// preview frame never revokes (the caller does not ask).
func capMisuse(step rxStep, reason string) bool {
	return step >= rxStep3 && step <= rxStep7 && capMisuseReasons[reason]
}

// grantMisuse is step 7½ (SS §4.3.10): a message that arrived under the
// capability c this node issued is authenticated as from from, in
// interaction ix. The capability is misused when its grantee is not the
// sender (it was passed on), or when it is a one-shot capability of another
// interaction. The message itself goes on as if it had no capability.
func grantMisuse(c *interactions.IssuedCap, from, ix string) bool {
	if c == nil {
		return false
	}
	return from != c.Grantee || c.Scope == interactions.CapScopeIX && ix != c.IX
}

// revokeMisused revokes the capability capID this node issued, for misuse,
// in a transaction of its own, and has the revocation pushed. It reports
// whether it revoked (false: unknown or already revoked).
func (d *Daemon) revokeMisused(capID []byte) (bool, error) {
	var done bool
	err := d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		done, err = tx.RevokeIssuedCap(capID, d.nowMS())
		return err
	})
	if err != nil {
		return false, err
	}
	if done {
		d.count(cntCapMisuse)
		d.kickRevokePush()
	}
	return done, nil
}

// ---- the direct path: p2pRxBucket (SS §4.3.10, engine step 0)

// Rates of the direct delivery buckets (engine §0.4).
const (
	p2pRxGlobalRate, p2pRxGlobalBurst     = 20, 100
	p2pRxContactRate, p2pRxContactBurst   = 20, 100
	p2pRxStrangerRate, p2pRxStrangerBurst = 1, 10
	p2pRxSharedRate, p2pRxSharedBurst     = 1, 10
	// p2pRxCapBuckets bounds the per-capability buckets kept; past it the
	// least recently used goes.
	p2pRxCapBuckets = 4096
)

// rxBucketKey names the bucket a direct delivery is charged to besides the
// global one: the capability's own (Class contact or stranger, CapID), or
// the shared bucket (Class 0) of everything else.
type rxBucketKey struct {
	Class uint64
	CapID string
}

// p2pRxBucket is step 0 for a direct delivery, before anything is
// decrypted: an envelope under a capability this node issued, unrevoked,
// unexpired and proven for this envelope (env_digest) is charged to that
// capability's bucket; every other envelope — a first contact, a
// credential that fails — to the shared one. issued is the caps_issued row
// of the capability's cap_id (nil: none), mbox this node's current mailbox
// key, self this node's AID.
func p2pRxBucket(access *seal.Access, envDigest [32]byte, issued *interactions.IssuedCap, self string, mbox seal.Mbox,
	now uint64) rxBucketKey {
	shared := rxBucketKey{}
	if access == nil || access.Kind != seal.AccessKindCap || issued == nil {
		return shared
	}
	if seal.VerifyAccess(access, envDigest, mbox, self, now) != nil {
		return shared
	}
	body, err := seal.UnmarshalCapBody(access.Cap.Body)
	if err != nil || !bytes.Equal(body.CapID, issued.CapID) {
		return shared
	}
	if issued.RevokedAt != 0 || issued.CapExp < now || issued.Epoch != mbox.Epoch || issued.Class != body.Class {
		return shared
	}
	return rxBucketKey{Class: issued.Class, CapID: string(issued.CapID)}
}

// capIDOf returns the cap_id of a kind-1 access, nil when it has none that
// decodes: what the caller looks up in caps_issued for p2pRxBucket.
func capIDOf(access *seal.Access) []byte {
	if access == nil || access.Kind != seal.AccessKindCap || access.Cap == nil {
		return nil
	}
	body, err := seal.UnmarshalCapBody(access.Cap.Body)
	if err != nil {
		return nil
	}
	return body.CapID
}

// p2pRxLimiter holds the direct delivery buckets: a global one, the
// shared one, and one per capability.
type p2pRxLimiter struct {
	mu     sync.Mutex
	init   bool
	global tokenBucket
	shared tokenBucket
	caps   map[string]*tokenBucket
	order  []string // least recently used first
}

// allow charges a delivery to the bucket k names and to the global one,
// and reports whether both had a token. A delivery the first bucket
// refuses takes nothing from the global one, and one the global bucket
// refuses gives its token back to the first.
func (l *p2pRxLimiter) allow(k rxBucketKey, now uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.init {
		l.global = newBucket(p2pRxGlobalRate, p2pRxGlobalBurst)
		l.shared = newBucket(p2pRxSharedRate, p2pRxSharedBurst)
		l.caps = map[string]*tokenBucket{}
		l.init = true
	}
	b := &l.shared
	if k.Class != 0 {
		b = l.capBucketLocked(k)
	}
	if !b.allow(now) {
		return false
	}
	if !l.global.allow(now) {
		b.tokens++
		return false
	}
	return true
}

func (l *p2pRxLimiter) capBucketLocked(k rxBucketKey) *tokenBucket {
	if b, ok := l.caps[k.CapID]; ok {
		for i, id := range l.order {
			if id == k.CapID {
				l.order = append(append(l.order[:i:i], l.order[i+1:]...), id)
				break
			}
		}
		return b
	}
	nb := newBucket(p2pRxStrangerRate, p2pRxStrangerBurst)
	if k.Class == seal.CapClassContact {
		nb = newBucket(p2pRxContactRate, p2pRxContactBurst)
	}
	l.caps[k.CapID] = &nb
	l.order = append(l.order, k.CapID)
	if len(l.order) > p2pRxCapBuckets {
		delete(l.caps, l.order[0])
		l.order = l.order[1:]
	}
	return &nb
}

// ---- receiving a grant (SSR-W7, SSR-W8)

// grantToKeep is receive step 8 for inner key 14: the grant a message
// carries, checked against keys (the sender's key set as step 8 verified
// it, nil when it did not verify), as the caps_held row the message's
// step-10 transaction keeps (PutHeldGrant). A one-shot envelope (outer key
// 8: a preview frame, anet.card.get/1) does not leave its grant (SSR-W8).
// A grant that fails the check is ignored and counted grant-invalid, and
// the message is not affected (CORE-K4).
func (d *Daemon) grantToKeep(in *seal.SealedInner, keys *seal.EncKeySet, oneShot bool, now uint64) *interactions.HeldCap {
	if in == nil || len(in.Grant) == 0 || oneShot {
		return nil
	}
	g, body, err := seal.CheckGrant(in, keys, now)
	if err != nil {
		d.countErr(cntGrantInvalid, err)
		return nil
	}
	h, err := interactions.HeldFromGrant(g, body, now)
	if err != nil {
		d.countErr(cntGrantInvalid, err)
		return nil
	}
	return &h
}

// ---- the daemon's facts and the issuing transaction

// errGrantStale is commitGrantTx's answer when the capability planned for a
// version is no longer the one to give: revoked, replaced or over a limit
// since planGrant read the table. The version is sealed again.
var errGrantStale = errors.New("anet: the delivery credential planned for this message changed; sealing it again")

// grantOp is what planGrant does with caps_issued.
type grantOp uint8

const (
	grantReuse grantOp = iota + 1 // the valid row as it is (SSR-D6)
	grantRenew                    // the valid row with a later cap_exp, same cap_id and holder key (SSR-D6)
	grantNew                      // a new capability
)

// grantPlan is a grant ready for inner key 14 (Grant) and the caps_issued
// write that commits with the version carrying it (commitGrantTx).
type grantPlan struct {
	Grant []byte
	op    grantOp
	row   interactions.IssuedCap
}

// grantRel gathers capRel for a message to `to`.
func (d *Daemon) grantRel(to string, msg capMsg, now uint64) (capRel, error) {
	ps := d.readPeers()
	rel := capRel{Listed: ps.allowed(to), Denied: ps.denied(to), P2P: p2pAdvertise(d.config()), Prices: d.pricesSomething()}
	held, err := d.ix.HeldCaps(to)
	if err != nil {
		return rel, err
	}
	facts, role, err := d.paidFactsFor(to, msg.IX, held)
	if err != nil {
		return rel, err
	}
	rel.Role, rel.MaybePaid = role, maybePaid(facts)
	cur, ok := d.mbox.Current()
	if !ok {
		return rel, nil
	}
	issued, err := d.ix.IssuedCaps(to)
	if err != nil {
		return rel, err
	}
	rel.StrangerOneShots = strangerOneShots(issued, msg.IX, cur.Epoch, now)
	return rel, nil
}

// strangerOneShots counts the stranger one-shot capabilities in rows valid
// at now under epoch, other than interaction ix's.
func strangerOneShots(rows []interactions.IssuedCap, ix string, epoch, now uint64) int {
	n := 0
	for _, r := range rows {
		if r.Scope == interactions.CapScopeIX && r.Class == seal.CapClassStranger && r.IX != ix &&
			r.RevokedAt == 0 && r.Epoch == epoch && r.CapExp >= now+capValidMarginMS {
			n++
		}
	}
	return n
}

// paidFactsFor gathers paidFacts and capRole for the interaction ix with
// `to`; held is what `to` gave this node.
func (d *Daemon) paidFactsFor(to, ix string, held []interactions.HeldCap) (paidFacts, capRole, error) {
	var f paidFacts
	role := capRoleNone
	for _, h := range held {
		// The flag is what the provider said of itself; a capability given
		// up or expired still says it (the conservative reading).
		if h.Scope == interactions.CapScopePeer && h.Pay {
			f.HeldPeerPay = true
		}
	}
	if ix == "" {
		return f, role, nil
	}
	row, err := d.ix.Get(ix)
	switch {
	case errors.Is(err, interactions.ErrNotFound):
		if p, perr := d.ix.GetPending(ix); perr == nil && p.FromAID == to {
			role = capRoleUnaccepted
		} else if perr != nil && !errors.Is(perr, interactions.ErrNotFound) {
			return f, role, perr
		}
		return f, role, nil
	case err != nil:
		return f, role, err
	case row.PeerAID != to:
		return f, role, nil
	}
	f.Quoted = row.PayState != interactions.PayNone
	switch row.Role {
	case interactions.RoleOutbound:
		role, f.Requester = capRoleRequester, true
	case interactions.RoleInbound:
		role, f.Provider = capRoleProvider, true
		if row.State == interactions.StateRejected {
			role = capRoleUnaccepted
		}
		f.CallPriced = row.IsCapability && d.capabilityPriced(row.Goal)
	}
	return f, role, nil
}

// capabilityPriced reports whether capID has a price here.
func (d *Daemon) capabilityPriced(capID string) bool {
	if d.payer() == nil || d.providers == nil || capID == "" {
		return false
	}
	p, ok := d.providers.Resolve(capID)
	if !ok {
		return false
	}
	price, priced := priceOfCapability(p, capID)
	return priced && price > 0
}

// pricesSomething is what the payment module answers through the host for
// the grant's pay (core X-SS-8, Q-SS-6): whether this node prices any
// capability it serves. A node without payments prices nothing.
func (d *Daemon) pricesSomething() bool {
	p := d.payer()
	return p != nil && p.HasPrices()
}

// p2pAdvertise is modules.p2p.advertise, the address a contact peer
// capability tells its holder (Grant key 5, SSR-D8), in the form the grant
// carries (grantP2PAddr); "" when not set.
func p2pAdvertise(cfg Config) string {
	raw, ok := cfg.Modules["p2p"]
	if !ok {
		return ""
	}
	var mc struct {
		Advertise string `json:"advertise"`
	}
	if json.Unmarshal(raw, &mc) != nil {
		return ""
	}
	return grantP2PAddr(mc.Advertise)
}

// grantP2PAddr writes an advertise value as Grant key 5 holds it (core
// §2.3): host:port with a decimal port, without the "tcp://" and the
// surrounding space the p2p module also accepts. A grant whose p2p is not
// such an address cannot be written at all (seal.Grant.Marshal), so a value
// that is not one even then is left out: the peer gets its capability
// without the direct address and reaches this node through the hub.
func grantP2PAddr(a string) string {
	a = strings.TrimPrefix(strings.TrimSpace(a), "tcp://")
	host, port, err := net.SplitHostPort(a)
	if err != nil {
		return ""
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return ""
	}
	a = net.JoinHostPort(host, strconv.Itoa(n))
	if !seal.ValidP2PAddr(a) {
		return ""
	}
	return a
}

// grantForMessage is the grant of one message to `to`: grantFor over the
// daemon's facts, then planGrant. nil is no grant.
func (d *Daemon) grantForMessage(to string, msg capMsg, now uint64) (*grantPlan, error) {
	rel, err := d.grantRel(to, msg, now)
	if err != nil {
		return nil, err
	}
	return d.planGrant(to, grantFor(msg, rel, now), now)
}

// planGrant turns a decision into the grant bytes and the caps_issued write
// that goes with them (SSR-D6): the valid row for the same scope,
// interaction and class is given again as it is, or renewed — same cap_id
// and holder key, a later cap_exp — once less than half of the decision's
// lifetime is left; otherwise a new capability with a new holder key. It
// reads the table and signs; the write is commitGrantTx's, in the
// transaction of the version. No mailbox key yet, or no hub to name as the
// grant's home: no grant (the peer answers by first contact).
func (d *Daemon) planGrant(to string, dec GrantDecision, now uint64) (*grantPlan, error) {
	if !dec.Issue {
		return nil, nil
	}
	cur, ok := d.mbox.Current()
	if !ok {
		d.count(cntGrantNoMailboxKey)
		return nil, nil
	}
	home := d.hubAID()
	if home == "" {
		d.count(cntGrantNoHome)
		return nil, nil
	}
	rows, err := d.ix.IssuedCaps(to)
	if err != nil {
		return nil, err
	}
	var found *interactions.IssuedCap
	for i := range rows {
		r := &rows[i]
		if issuedMatches(r, dec, cur.Epoch, now) && (found == nil || r.CapExp > found.CapExp) {
			found = r
		}
	}
	p := &grantPlan{}
	life := dec.CapExp - now
	switch {
	case found != nil && (found.CapExp-now >= life/2 || found.CapExp >= dec.CapExp):
		p.op, p.row = grantReuse, *found
	case found != nil:
		p.op, p.row = grantRenew, *found
		p.row.CapExp = dec.CapExp
	default:
		capID := make([]byte, seal.CapIDLen)
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(capID); err != nil {
			return nil, err
		}
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		p.op = grantNew
		p.row = interactions.IssuedCap{CapID: capID, Grantee: to, Class: dec.Class, Scope: dec.Scope, IX: dec.IX,
			HolderSeed: seed, HolderPub: bytes.Clone(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)),
			Epoch: cur.Epoch, CapExp: dec.CapExp, IssuedAt: now}
	}
	capw, err := d.mbox.SignCap(&seal.CapBody{Owner: d.AID(), Epoch: p.row.Epoch, CapID: p.row.CapID,
		Class: p.row.Class, CapExp: p.row.CapExp, Holder: p.row.HolderPub})
	if err != nil {
		return nil, err
	}
	g := seal.Grant{Cap: *capw, HolderSeed: p.row.HolderSeed, Home: home}
	if dec.Scope == interactions.CapScopeIX {
		ix := dec.IX
		g.IX = &ix
	} else {
		if dec.P2P != "" && dec.Class == seal.CapClassContact {
			addr := dec.P2P
			g.P2P = &addr
		}
		if dec.Pay {
			pay := true
			g.Pay = &pay
		}
	}
	if p.Grant, err = g.Marshal(); err != nil {
		return nil, err
	}
	return p, nil
}

// issuedMatches reports whether r is a valid capability for decision dec:
// the same scope, interaction and class, unrevoked, under the current
// mailbox epoch and not about to expire.
func issuedMatches(r *interactions.IssuedCap, dec GrantDecision, epoch, now uint64) bool {
	return r.Scope == dec.Scope && r.IX == dec.IX && r.Class == dec.Class && r.RevokedAt == 0 && r.Epoch == epoch &&
		r.CapExp >= now+capValidMarginMS
}

// commitGrantTx writes p's caps_issued change in tx, the transaction that
// writes the version carrying p.Grant (SSR-D10): if tx rolls back, no
// capability exists that bytes could carry, and none was carried. It
// answers errGrantStale when the plan no longer holds — the mailbox key
// changed, the row was revoked or removed, another version issued the
// same capability first, the stranger limit was reached — and the caller
// seals the version again. A new contact peer capability revokes the
// peer's stranger one in the same transaction (SSR-D7).
func (d *Daemon) commitGrantTx(tx *interactions.Tx, p *grantPlan, now uint64) error {
	if p == nil {
		return nil
	}
	cur, ok := d.mbox.Current()
	if !ok || cur.Epoch != p.row.Epoch {
		return errGrantStale
	}
	switch p.op {
	case grantReuse, grantRenew:
		r, err := tx.IssuedCap(p.row.CapID)
		if errors.Is(err, interactions.ErrNotFound) {
			return errGrantStale
		}
		if err != nil {
			return err
		}
		if r.RevokedAt != 0 {
			return errGrantStale
		}
		if p.op == grantRenew {
			_, err = tx.RenewIssuedCap(p.row.CapID, p.row.CapExp) // false: renewed further meanwhile
		}
		return err
	}
	rows, err := tx.IssuedCaps(p.row.Grantee)
	if err != nil {
		return err
	}
	dec := GrantDecision{Scope: p.row.Scope, IX: p.row.IX, Class: p.row.Class}
	for i := range rows {
		if issuedMatches(&rows[i], dec, cur.Epoch, now) {
			return errGrantStale // issued by another version meanwhile: give that one
		}
	}
	if p.row.Scope == interactions.CapScopeIX && p.row.Class == seal.CapClassStranger &&
		strangerOneShots(rows, p.row.IX, cur.Epoch, now) >= maxStrangerOneShots {
		return errGrantStale
	}
	if err := tx.PutIssuedCap(p.row); err != nil {
		return err
	}
	if p.row.Scope == interactions.CapScopePeer && p.row.Class == seal.CapClassContact {
		_, err = tx.RevokeIssued(interactions.IssuedFilter{Grantee: p.row.Grantee, Scope: interactions.CapScopePeer,
			Class: seal.CapClassStranger}, now)
	}
	return err
}

// cardGetGrant is SSR-D10 for the one-shot sender: the reply capability of
// an anet.card.get/1 to `to` in interaction ix (rule 0) is written in a
// transaction of its own, committed before the grant is returned, so it
// exists before any byte that carries it leaves the process. An error
// returns no grant, and the request must not be sent (its answer could not
// come back).
func (d *Daemon) cardGetGrant(to, ix string, now uint64) ([]byte, error) {
	msg := capMsg{Kind: capMsgCardGet, IX: ix}
	for range 2 {
		p, err := d.planGrant(to, grantFor(msg, capRel{}, now), now)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, errors.New("anet: no delivery credential can be given yet (the mailbox key or the hub is not ready)")
		}
		err = d.ix.Update(func(tx *interactions.Tx) error { return d.commitGrantTx(tx, p, now) })
		if errors.Is(err, errGrantStale) {
			d.count(cntGrantStaleAtCommit)
			continue
		}
		if err != nil {
			return nil, err
		}
		return p.Grant, nil
	}
	return nil, errGrantStale
}

// accessChoiceFor is accessFor over the daemon's facts for one attempt of a
// message to `to` in interaction ix.
func (d *Daemon) accessChoiceFor(to, ix string, delegate bool, mode accessMode, now uint64) (AccessChoice, error) {
	held, err := d.ix.HeldCaps(to)
	if err != nil {
		return AccessChoice{}, err
	}
	facts, _, err := d.paidFactsFor(to, ix, held)
	if err != nil {
		return AccessChoice{}, err
	}
	row := accessRow{To: to, IX: ix, Delegate: delegate, MaybePaid: maybePaid(facts), PeerEpoch: d.peerMboxEpoch(to, ix)}
	return accessFor(row, held, mode, now), nil
}

// peerMboxEpoch is the mailbox epoch of the key set this node holds for
// aid: the peer record's, else (a public requester, A2A-DESIGN §3.8) the
// one kept on interaction ix; 0 when it holds none.
func (d *Daemon) peerMboxEpoch(aid, ix string) uint64 {
	if row, err := d.ix.PeerIdentity(aid); err == nil && len(row.KeySet) > 0 {
		return keySetMboxEpoch(row.KeySet)
	}
	if ix == "" {
		return 0
	}
	if row, err := d.ix.Get(ix); err == nil && row.PeerAID == aid && len(row.PeerKeys) > 0 {
		return keySetMboxEpoch(row.PeerKeys)
	}
	return 0
}

// keySetMboxEpoch reads the mailbox epoch of a SignedEncKeySet encoding
// this node stored after verifying it; 0 when it does not decode.
func keySetMboxEpoch(signed []byte) uint64 {
	s, err := seal.UnmarshalSignedEncKeySet(signed)
	if err != nil {
		return 0
	}
	var set seal.EncKeySet
	if err := coredet.Unmarshal(s.Set, &set); err != nil {
		return 0
	}
	return set.Mbox.Epoch
}
