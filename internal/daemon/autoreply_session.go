package daemon

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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// execSessionStore maps anet interaction_id -> agent-native chat/session id, persisted as JSON.
type execSessionStore struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

func newExecSessionStore(root string) *execSessionStore {
	s := &execSessionStore{path: filepath.Join(root, "exec_sessions.json"), m: map[string]string{}}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.m)
	}
	return s
}

func (s *execSessionStore) get(key string) string {
	if s == nil || key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[key]
}

func (s *execSessionStore) set(key, id string) {
	if s == nil || key == "" || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[key] == id {
		return
	}
	s.m[key] = id
	s.flushLocked()
}

func (s *execSessionStore) del(key string) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; ok {
		delete(s.m, key)
		s.flushLocked()
	}
}

// pruneExcept drops session bindings whose interaction is no longer active (ended, failed, or gone).
// Interaction ids are globally unique and never reused, so an id absent from the live active set will
// never be resumed again — pruning it just keeps the file bounded as tasks complete (and mops up any
// binding orphaned by an abrupt requester exit).
func (s *execSessionStore) pruneExcept(active map[string]bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k := range s.m {
		if !active[k] {
			delete(s.m, k)
			changed = true
		}
	}
	if changed {
		s.flushLocked()
	}
}

func (s *execSessionStore) flushLocked() {
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// Process-wide registry so the exec replier and the end-cleanup path share ONE instance (hence one
// mutex + one in-memory map), keyed by the identity's data root.
var (
	execSessMu    sync.Mutex
	execSessByDir = map[string]*execSessionStore{}
)

func sharedExecSessionStore(root string) *execSessionStore {
	execSessMu.Lock()
	defer execSessMu.Unlock()
	if s, ok := execSessByDir[root]; ok {
		return s
	}
	s := newExecSessionStore(root)
	execSessByDir[root] = s
	return s
}

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
