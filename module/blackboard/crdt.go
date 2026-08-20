// Package blackboard is the v3 runtime org shared-cognition store: an add-wins OR-Set of
// content-addressed, signed CogUnits with Hybrid-Logical-Clock causal stamps, merged centrally
// at org-central (design decision D6 / §O4). The CRDT (HLC + ORSet) is ported from the proven
// v1 internal/crdt (OQ-2) rather than imported, so v3rt carries no dependency on retiring v1.
//
// This file holds the CRDT primitives; cogunit.go is the v3 signed unit; blackboard.go is the
// merge surface used by the org-central bb.* methods.
package blackboard

import (
	"sync"
	"time"
)

// HLC is a Hybrid Logical Clock providing causal ordering. Order: Wall → Logical → NodeID.
type HLC struct {
	Wall    int64  `cbor:"1,keyasint"`           // unix-millis
	Logical uint16 `cbor:"2,keyasint,omitempty"` // Lamport counter within one ms
	NodeID  string `cbor:"3,keyasint,omitempty"` // node tag (disambiguates equal wall+logical)
}

// Less reports whether a is causally before b.
func (a HLC) Less(b HLC) bool {
	if a.Wall != b.Wall {
		return a.Wall < b.Wall
	}
	if a.Logical != b.Logical {
		return a.Logical < b.Logical
	}
	return a.NodeID < b.NodeID
}

// IsZero reports an uninitialized clock.
func (a HLC) IsZero() bool { return a.Wall == 0 && a.Logical == 0 && a.NodeID == "" }

// Clock advances a node's HLC monotonically.
type Clock struct {
	mu     sync.Mutex
	nodeID string
	last   HLC
}

// NewClock makes a clock tagged with nodeID (truncated to 8 chars to bound the tag).
func NewClock(nodeID string) *Clock {
	if len(nodeID) > 8 {
		nodeID = nodeID[:8]
	}
	return &Clock{nodeID: nodeID}
}

// Now returns a fresh HLC strictly greater than every prior Now/Merge on this clock.
func (c *Clock) Now() HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UnixMilli()
	if now > c.last.Wall {
		c.last = HLC{Wall: now, Logical: 0, NodeID: c.nodeID}
	} else {
		c.last.Logical++
		c.last.NodeID = c.nodeID
	}
	return c.last
}

// Merge advances the clock past a remote HLC and returns the new local time.
func (c *Clock) Merge(remote HLC) HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	maxWall := time.Now().UnixMilli()
	if c.last.Wall > maxWall {
		maxWall = c.last.Wall
	}
	if remote.Wall > maxWall {
		maxWall = remote.Wall
	}
	logical := uint16(0)
	if maxWall == c.last.Wall && maxWall == remote.Wall {
		logical = max16(c.last.Logical, remote.Logical) + 1
	} else if maxWall == c.last.Wall {
		logical = c.last.Logical + 1
	} else if maxWall == remote.Wall {
		logical = remote.Logical + 1
	}
	c.last = HLC{Wall: maxWall, Logical: logical, NodeID: c.nodeID}
	return c.last
}

func max16(a, b uint16) uint16 {
	if a > b {
		return a
	}
	return b
}

// ORSet is an add-wins set: each element carries a set of unique (tagID→HLC) tags; Add creates a
// tag (concurrent adds of one element union their tags). The blackboard is grow-only in practice
// — retraction is modeled as a new CogUnit of Type "retraction", NOT an OR-Set remove — so no
// remove/delta machinery is exposed (a correct observed-remove would need tag-enumerating deltas;
// omitted rather than shipped half-built). Safe for concurrent use.
type ORSet struct {
	mu    sync.Mutex
	elems map[string]map[string]HLC // element → tagID → HLC
}

// NewORSet builds an empty OR-Set.
func NewORSet() *ORSet { return &ORSet{elems: map[string]map[string]HLC{}} }

// Add inserts elem with a tag derived from hlc (Wall:Logical:NodeID is unique per Clock.Now()).
func (s *ORSet) Add(elem string, hlc HLC) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.elems[elem] == nil {
		s.elems[elem] = map[string]HLC{}
	}
	s.elems[elem][tagID(hlc)] = hlc
}

// Contains reports membership (any live tag).
func (s *ORSet) Contains(elem string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.elems[elem]) > 0
}

// Elements returns the live members.
func (s *ORSet) Elements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.elems))
	for e, tags := range s.elems {
		if len(tags) > 0 {
			out = append(out, e)
		}
	}
	return out
}

// Len returns the live element count.
func (s *ORSet) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, tags := range s.elems {
		if len(tags) > 0 {
			n++
		}
	}
	return n
}

func tagID(h HLC) string {
	// Wall:Logical:NodeID is unique per Clock.Now() (monotone), so it identifies one add.
	var b [16]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(h.Wall >> (56 - 8*i))
	}
	b[8] = byte(h.Logical >> 8)
	b[9] = byte(h.Logical)
	return string(b[:10]) + h.NodeID
}
