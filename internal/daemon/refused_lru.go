package daemon

// refused_lru.go is the in-memory list of refused envelopes (DLV-ST-9).

import "sync"

// refusedLimit is how many refused envelopes the list remembers.
const refusedLimit = 4096

// refusedLRU remembers up to limit refused envelopes, by (sender, message
// id), and the note reason each was refused with ("" for none). A replay of
// a refused envelope is dropped at step 5, before its KEL and signature are
// checked again, and is answered with the reason the first copy was: the
// same outcome and the same reason for every copy (engine ENG-V13).
//
// It is a cache. Past limit the oldest entry is forgotten — the one added
// first; a lookup does not keep an entry longer. For a delegation the
// refused table (interactions/refused.go) is what keeps a replay that
// outlived its entry refused, with its reason [redteam:F5]; for other types
// a replay that outlived it is judged again, and a message's step 9 writes
// nothing.
type refusedLRU struct {
	mu    sync.Mutex
	limit int
	order []string
	m     map[string]string // key → reason
}

// add remembers that the envelope key was refused with reason. An envelope
// already remembered keeps the reason it was first refused with: that is
// what its sender was told.
func (s *refusedLRU) add(key, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	if s.limit == 0 {
		s.limit = refusedLimit
	}
	if _, ok := s.m[key]; ok {
		return
	}
	s.m[key] = reason
	s.order = append(s.order, key)
	if len(s.order) > s.limit {
		delete(s.m, s.order[0])
		s.order = s.order[1:]
	}
}

// get reports whether the envelope key is remembered as refused, and with
// which reason.
func (s *refusedLRU) get(key string) (reason string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reason, ok = s.m[key]
	return reason, ok
}
