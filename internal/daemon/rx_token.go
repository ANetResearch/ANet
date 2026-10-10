package daemon

// rx_token.go is the delivery token of the receive pipeline v3 (engine §4.4):
// the one place that decides whether an envelope is answered with a token,
// which outcome it carries and what its note says. It is a pure function of
// the pipeline's verdict (engine ENG-G2): no I/O, no clock; the pipeline makes
// the token with the envelope's ack_key once the decision says to
// (receive.go, answerEnvelope).

import "github.com/ANetResearch/ANetCore/seal"

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

// rxTokenFor decides the delivery token of an envelope from the pipeline's
// verdict (engine §4.2, §4.5; P-ENG-6).
func rxTokenFor(v rxVerdict) tokenDecision {
	return tokenDecision{}
}
