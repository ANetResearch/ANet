package autoreply

// autoreply_session.go — native chat-session support for the exec auto-reply backend.
//
// Design principle: anet is a TRANSPORT, not a memory layer. Instead of replaying the whole transcript
// into the agent on every turn, we bind each anet interaction to the agent's OWN chat session and, on
// each turn, send only the NEW message(s) — the agent keeps the conversation itself. anet still stores
// the full transcript (it is the source of truth for observation); we simply stop re-sending it, which
// removes the per-turn context-size blow-up and makes long interactions scalable.
//
// The mapping interaction_id -> backend session id is persisted (atomic file) so a daemon restart keeps
// resuming the right session. Interaction ids are globally unique and never reused, so a stale mapping
// is never resumed by accident; cleanup on end just bounds the file size.

import (
	"strings"
)

// looksLikeSessionID rejects output that is clearly not an id (e.g. a stub echoing a sentence), so a
// backend lacking `create-chat` degrades to the legacy full-transcript one-shot rather than trying to
// resume a bogus id.
func looksLikeSessionID(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && len(s) <= 100 && !strings.ContainsAny(s, " \t\r\n")
}

// turnsSinceLastAssistant returns the trailing turns that arrived after our last reply — the only
// content the agent has not already seen in its session. On the first turn (no assistant yet) this is
// the whole (short) opening, which seeds the fresh chat.
func turnsSinceLastAssistant(turns []chatTurn) []chatTurn {
	last := -1
	for i, t := range turns {
		if t.Role == "assistant" {
			last = i
		}
	}
	return turns[last+1:]
}
