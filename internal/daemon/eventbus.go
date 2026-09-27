package daemon

// eventbus.go is the daemon's in-process event bus, keyed by interaction
// id. Every state write and every stored message publishes one event after
// its transaction commits. Waiting (/tasks/wait) and streaming (A2A
// SubscribeToTask, SSE) are built on Watch.
//
// Watch takes the subscription and the snapshot together under the bus
// lock. A state event whose state_seq is not newer than the snapshot's is
// dropped, so a watcher sees each state change once, and no state change
// that commits after the snapshot is missed. A message event may repeat a
// message stored just before the snapshot; message events carry the
// message seq for the watcher to compare.
//
// Delivery never blocks a writer. A subscriber whose buffer is full has its
// channel closed and is removed; it can Watch again and read the current
// state from the new snapshot.

import (
	"sync"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// Event kinds.
const (
	EventState   = "state"   // the interaction's state changed
	EventMessage = "message" // a message was stored
	EventResult  = "result"  // a result or receipt was stored
)

// Event is one change to an interaction.
type Event struct {
	IX       string             `json:"interaction_id"`
	Kind     string             `json:"kind"`
	State    interactions.State `json:"state,omitempty"`
	StateSeq int64              `json:"state_seq,omitempty"`
	// MsgSeq is the stored message's seq, for EventMessage.
	MsgSeq  int64  `json:"msg_seq,omitempty"`
	MsgKind string `json:"msg_kind,omitempty"`
	At      uint64 `json:"at"`
}

// eventBufferSize bounds the events one subscriber may have undelivered.
const eventBufferSize = 64

type subscriber struct {
	ch       chan Event
	minState int64 // state events at or below this state_seq are dropped
	closed   bool
}

// eventBus fans events out to the subscribers of one interaction.
type eventBus struct {
	mu   sync.Mutex
	subs map[string]map[*subscriber]struct{}
}

func (b *eventBus) unsubscribe(ix string, s *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removeLocked(ix, s)
}

func (b *eventBus) removeLocked(ix string, s *subscriber) {
	if set := b.subs[ix]; set != nil {
		if _, ok := set[s]; ok {
			delete(set, s)
			if !s.closed {
				s.closed = true
				close(s.ch)
			}
		}
		if len(set) == 0 {
			delete(b.subs, ix)
		}
	}
}

func (b *eventBus) publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs[e.IX] {
		if e.Kind == EventState && e.StateSeq != 0 && e.StateSeq <= s.minState {
			continue
		}
		select {
		case s.ch <- e:
		default:
			// A subscriber that does not keep up is dropped rather than
			// allowed to hold up the writer.
			b.removeLocked(e.IX, s)
		}
	}
}

// subscribers reports how many subscriptions exist for ix. Tests only.
func (b *eventBus) subscribers(ix string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[ix])
}

// Watch returns the interaction as it is now and a channel of the changes
// that follow. cancel ends the subscription and closes the channel; it is
// safe to call more than once. The channel is also closed if the watcher
// falls eventBufferSize events behind.
//
// The subscription and the snapshot are taken under the bus lock, which
// every publish also takes. A write that committed before the snapshot is
// in it, and its state event, if published later, is dropped by the
// state_seq floor; a write that commits after the snapshot is delivered.
func (d *Daemon) Watch(ix string) (*interactions.Interaction, <-chan Event, func(), error) {
	b := &d.bus
	b.mu.Lock()
	snap, err := d.ix.Get(ix)
	if err != nil {
		b.mu.Unlock()
		return nil, nil, func() {}, err
	}
	s := &subscriber{ch: make(chan Event, eventBufferSize), minState: snap.StateSeq}
	if b.subs == nil {
		b.subs = map[string]map[*subscriber]struct{}{}
	}
	if b.subs[ix] == nil {
		b.subs[ix] = map[*subscriber]struct{}{}
	}
	b.subs[ix][s] = struct{}{}
	b.mu.Unlock()
	return snap, s.ch, func() { b.unsubscribe(ix, s) }, nil
}

// publishState publishes the interaction's current state after a write.
func (d *Daemon) publishState(ix string) {
	cur, err := d.ix.Get(ix)
	if err != nil {
		return
	}
	d.bus.publish(Event{IX: ix, Kind: EventState, State: cur.State, StateSeq: cur.StateSeq, At: d.nowMS()})
}

// publishMessage publishes a stored message.
func (d *Daemon) publishMessage(ix string, seq int64, kind string) {
	d.bus.publish(Event{IX: ix, Kind: EventMessage, MsgSeq: seq, MsgKind: kind, At: d.nowMS()})
}

// publishResult publishes a stored result, followed by the state it set.
func (d *Daemon) publishResult(ix string) {
	d.bus.publish(Event{IX: ix, Kind: EventResult, At: d.nowMS()})
	d.publishState(ix)
}
