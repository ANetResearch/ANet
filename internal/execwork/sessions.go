package execwork

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// SessionStore maps anet interaction_id -> agent-native chat/session id, persisted as JSON in sessions.json under dir, which is the auto-reply module's own state directory (<data>/modules/autoreply).
type SessionStore struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

func NewSessionStore(dir string) *SessionStore {
	s := &SessionStore{path: filepath.Join(dir, "sessions.json"), m: map[string]string{}}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.m)
	}
	return s
}

func (s *SessionStore) Get(key string) string {
	if s == nil || key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[key]
}

func (s *SessionStore) Set(key, id string) {
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

func (s *SessionStore) Del(key string) {
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
// binding orphaned by an abrupt requester exit). The work dirs of the same interactions go in the
// maintenance period (PruneEnded, PruneExecWorkDirs).
func (s *SessionStore) PruneExcept(active map[string]bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(active)
}

// pruneEnded drops the bindings of the interactions that are not open, in the maintenance period
// (FS-L11): open is asked under the store's lock, so a binding set for a task that started since is
// never dropped as an ended one's.
func (s *SessionStore) PruneEnded(open func() (map[string]bool, error)) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) == 0 {
		return nil
	}
	ids, err := open()
	if err != nil {
		return err
	}
	s.pruneLocked(ids)
	return nil
}

// pruneLocked drops every binding whose interaction is not in keep. Under mu.
func (s *SessionStore) pruneLocked(keep map[string]bool) {
	changed := false
	for k := range s.m {
		if !keep[k] {
			delete(s.m, k)
			changed = true
		}
	}
	if changed {
		s.flushLocked()
	}
}

func (s *SessionStore) flushLocked() {
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(s.path), 0o700) != nil {
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
	sessMu    sync.Mutex
	sessByDir = map[string]*SessionStore{}
)

// Sessions is the one store of the state directory dir.
func Sessions(dir string) *SessionStore {
	sessMu.Lock()
	defer sessMu.Unlock()
	if s, ok := sessByDir[dir]; ok {
		return s
	}
	s := NewSessionStore(dir)
	sessByDir[dir] = s
	return s
}
