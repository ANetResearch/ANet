package daemon

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/ANetResearch/ANetCore/adp"
)

// A registration says what this node offers. Nothing made that claim
// attributable to the node making it.
//
// The register challenge signs an action, an AID and a timestamp — it
// proves the caller holds the key, and covers none of what was
// registered. A hub could therefore change an agent's name or capability
// list and no party could tell, including the agent. That is tolerable
// for one hub a node chose to trust; it is not tolerable for a directory
// other people search, and it is fatal once directories federate, where
// the whole point is that a peer hub can hide a card but never invent
// one.
//
// So the node signs a card. ADP already defines it — subject, capability
// list, monotonic seq, detached JWS — and ANetCore already implements
// signing, admission and the high-water rule, so this is a use of the
// protocol rather than a new object.
const cardSchemaMajor = 1

// signedCard mints and signs this node's current card.
//
// Seq is the registration time in seconds. ADP admits a card only if its
// seq exceeds the highest already seen for that subject, which makes a
// replayed older card a no-op rather than a rollback — and using the
// clock means two registrations a second apart cannot tie.
func (d *Daemon) signedCard(name string, caps []string) (json.RawMessage, error) {
	now := time.Now()
	card := &adp.AgentCard{
		SubjectDID:         d.AID(),
		CardSchema:         adp.CardSchema{Major: cardSchemaMajor},
		Seq:                uint64(now.Unix()),
		IssuedAt:           now.Unix(),
		NotBefore:          now.Add(-1 * time.Minute).Unix(),
		Capabilities:       caps,
		CriticalExtensions: []string{},
		Name:               name,
	}
	if card.Capabilities == nil {
		card.Capabilities = []string{}
	}
	if err := card.Sign(d.self); err != nil {
		return nil, fmt.Errorf("anet: sign agent card: %w", err)
	}
	b, err := json.Marshal(card)
	if err != nil {
		return nil, err
	}
	return b, nil
}
