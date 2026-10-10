package daemon

// rx_token.go is the delivery token of the receive pipeline v3 (engine §4.4):
// the one place that decides whether an envelope is answered with a token,
// which outcome it carries and what its note says. It is a pure function of
// the pipeline's verdict (engine ENG-G2): no I/O, no clock; the pipeline makes
// the token with the envelope's ack_key once the decision says to
// (receive.go, answerEnvelope).
//
// What the sender does with the answer is the engine's (§2.7): tok(1)
// confirms its row; tok(2) ends it as refused, and on a delegation ends the
// task as rejected with the note's reason. So a refusal is told only when
// the same bytes would be refused again (P), never for a temporary failure
// (T, no token), and never before the step that decides it has made it
// stick: a delegation's refusal is in the refused table before its tok(2)
// (ENG-V12), and every later copy of the envelope gets the same outcome and
// note (ENG-V13).

import (
	"strings"

	"github.com/ANetResearch/ANetCore/seal"
)

// rxVerdict is what rxTokenFor decides on (engine ENG-V11): the pipeline's
// result for one envelope and the facts about the envelope the table needs.
type rxVerdict struct {
	// Class is the result class: A (accepted), P (refused for good, or a
	// duplicate, both acknowledged) or T (temporary, not acknowledged).
	Class rxClass
	// Step is the step that decided the envelope.
	Step rxStep
	// Reason is the counter reason the step gave.
	Reason string
	// OneShot: the outer carries key 8 (a preview frame, anet.card.get/1 or
	// anet.card/1). It never earns a token (ENG-V6).
	OneShot bool
	// Layer1: the first layer opened, so the envelope has an ack_key. Before
	// it there is none, and no token (ENG-V4).
	Layer1 bool
	// Note is the note a refusal was given where the step chose one: the
	// inbound policy's (noteReasonFor), or the one a refusal of the same
	// envelope was recorded with (the refused list and table, ENG-V13).
	Note seal.Note
}

// tokenDecision is rxTokenFor's answer: whether to make a token, its outcome
// and its note.
type tokenDecision struct {
	Make    bool
	Outcome seal.Outcome
	Note    seal.Note
}

// tokenRule is what a refusal reason is answered with.
type tokenRule uint8

const (
	tokNone         tokenRule = iota // no token (a temporary failure)
	tokProcessed                     // tok(1): taken, or a copy of one taken
	tokInvalid                       // tok(2) invalid: the bytes are not a valid message to this node
	tokKeyState                      // tok(2) key_state: the sender's key state cannot be checked or is not acceptable
	tokTaskNotFound                  // tok(2) task_not_found
	tokTaskEnded                     // tok(2) task_ended
	tokCanceled                      // tok(2) canceled: the requester canceled the task before it arrived
	tokEmpty                         // tok(2) without a reason: shown as refused
	tokNoted                         // tok(2) with the note the step chose (rxVerdict.Note)
)

// The counter reasons the pipeline itself gives (receive.go) that are not
// seal reasons; listed here with the rule each is answered with.
const (
	dropBadKindKeys   = "bad-kind-keys"  // step 4a: a key its type must or must not carry (core CORE-K3, K5, G7, G11)
	dropRKMismatch    = "rk-mismatch"    // step 9: rk is not the one recorded for the interaction (FS-R4)
	dropReplyMismatch = "reply-mismatch" // step 9: a result's reply does not match its receipt (CUX-R6)
	dropCanceled      = "canceled"       // step 9: a delegation its requester canceled before it arrived (tombstone)
)

// rxTokenRules is the token table of engine §4.2 and §4.5, by reason. A
// reason not here (none the pipeline gives; TestEveryReceiveReasonHasATokenRule)
// is answered tok(2) without a reason: the sender stops sending it, which is
// right for anything refused for good.
var rxTokenRules = map[string]tokenRule{
	// Step 3a: the Wrap and the second layer.
	seal.ReasonBadWrap:                   tokInvalid,
	seal.ReasonDecrypt:                   tokInvalid, // the second layer's AEAD; the first layer's has no token (Layer1)
	seal.ReasonSealedToClosedInteraction: tokProcessed,
	// Step 4: the inner.
	seal.ReasonBadInner:     tokInvalid,
	seal.ReasonUnknownField: tokInvalid,
	seal.ReasonToMismatch:   tokInvalid,
	seal.ReasonExpMismatch:  tokInvalid,
	seal.ReasonKELTooLarge:  tokInvalid,
	seal.ReasonBadKEL:       tokInvalid, // step 4 or 6
	seal.ReasonBadRK:        tokInvalid,
	seal.ReasonIKeyMismatch: tokInvalid,
	// Step 4a.
	dropBadKindKeys: tokInvalid,
	// Step 5.
	seal.ReasonExpired:     tokInvalid,
	seal.ReasonFromFuture:  tokInvalid,
	seal.ReasonBadLifetime: tokInvalid,
	// Steps 5a and 8½.
	dropRefusedReplay: tokNoted,
	dropDuplicate:     tokProcessed,
	dropRefusedFloor:  tokEmpty,
	// Step 6.
	dropKELFork:              tokInvalid,
	seal.ReasonKELFromFuture: tokKeyState,
	// Step 6½. witness-unknown is temporary (no token).
	seal.ReasonWitnessMissing:  tokKeyState,
	seal.ReasonWitnessStale:    tokKeyState,
	seal.ReasonWitnessMismatch: tokKeyState,
	seal.ReasonWitnessInvalid:  tokKeyState,
	// Step 7 (from-mismatch also at step 6).
	seal.ReasonBadSig:       tokInvalid,
	seal.ReasonFromMismatch: tokInvalid,
	seal.ReasonBadKeyState:  tokInvalid,
	seal.ReasonRevokedKey:   tokKeyState,
	seal.ReasonGraceExpired: tokKeyState,
	// Step 9: the inbound policy and admission (the prefix rule below for
	// refused-<code>).
	dropNotAccepting: tokNoted,
	dropCanceled:     tokCanceled,
	dropIXCollision:  tokEmpty,
	// Step 9: the interaction.
	dropPendingCollision: tokEmpty,
	dropUnknownIX:        tokTaskNotFound,
	dropAfterTerminal:    tokTaskEnded,
	dropRKMismatch:       tokInvalid,
	// Step 9: the body and what it verifies against.
	dropBadBody:       tokInvalid,
	dropBadTaskDoc:    tokInvalid,
	dropSignerIsOther: tokInvalid,
	dropIXMismatch:    tokInvalid,
	dropUnknownType:   tokInvalid,
	dropBadStatus:     tokInvalid,
	dropEmptyIX:       tokInvalid,
	dropResultRefused: tokInvalid,
	dropReplyMismatch: tokInvalid,
	// Step 9: who may send what.
	dropNotPeer:            tokEmpty,
	dropNotAllowed:         tokEmpty,
	dropPublicCapText:      tokEmpty,
	dropCancelFromProvider: tokEmpty,
	dropWrongRole:          tokEmpty,
	dropDenied:             tokEmpty,
	dropEndRequestIgnored:  tokEmpty,
	// Step 10.
	dropPendingFollowupsFull: tokEmpty,
	// A temporary failure that outlived the message (ENG-V5).
	dropExpiredWait: tokInvalid,
}

// ruleFor is the rule of reason (the table, then the refused-<code> prefix
// of a policy refusal).
func ruleFor(reason string) tokenRule {
	if r, ok := rxTokenRules[reason]; ok {
		return r
	}
	if strings.HasPrefix(reason, dropRefusedPrefix) {
		return tokNoted
	}
	return tokEmpty
}

// rxTokenFor decides the delivery token of an envelope from the pipeline's
// verdict (engine §4.2, §4.5; P-ENG-6):
//
//   - none for a one-shot envelope (ENG-V6), for a temporary failure, and
//     before the first layer opened (ENG-V4);
//   - tok(1) for an envelope step 10 committed, a duplicate of one, and one
//     sealed to an interaction this node closed;
//   - otherwise tok(2) with the note of its reason's row: a fixed code, the
//     empty reason, or the note the step chose (a policy refusal's, and the
//     one a refused envelope was first answered with, whose retry_after is
//     0 on every later copy, ENG-V13).
func rxTokenFor(v rxVerdict) tokenDecision {
	if v.OneShot || !v.Layer1 || v.Class == rxTransient {
		return tokenDecision{}
	}
	if v.Class == rxAccepted {
		return tokenDecision{Make: true, Outcome: seal.OutcomeProcessed}
	}
	refused := func(reason string, retryS uint32) tokenDecision {
		return tokenDecision{Make: true, Outcome: seal.OutcomeRefused, Note: seal.Note{Reason: reason, RetryAfterS: retryS}}
	}
	switch ruleFor(v.Reason) {
	case tokNone:
		return tokenDecision{}
	case tokProcessed:
		return tokenDecision{Make: true, Outcome: seal.OutcomeProcessed}
	case tokInvalid:
		return refused(seal.NoteInvalid, 0)
	case tokKeyState:
		return refused(seal.NoteKeyState, 0)
	case tokTaskNotFound:
		return refused(seal.NoteTaskNotFound, 0)
	case tokTaskEnded:
		return refused(seal.NoteTaskEnded, 0)
	case tokCanceled:
		return refused(seal.NoteCanceled, 0)
	case tokNoted:
		n := v.Note
		if n.Reason != "" && !seal.ValidNoteReason(n.Reason) {
			n = seal.Note{}
		}
		if n.RetryAfterS > seal.MaxRetryAfterS {
			n.RetryAfterS = seal.MaxRetryAfterS
		}
		if v.Reason == dropRefusedReplay {
			n.RetryAfterS = 0
		}
		return refused(n.Reason, n.RetryAfterS)
	}
	return refused("", 0)
}
