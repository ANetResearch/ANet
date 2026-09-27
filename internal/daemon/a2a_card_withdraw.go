package daemon

// a2a_card_withdraw.go withdraws this node's A2A network card from its hub
// when the node no longer has a public skill (0017 Q6, A2A-DESIGN §10.1).
//
// A registration without a2a_card leaves the hub's stored card in place —
// the field's absence means "no change", so that a refresh never
// unpublishes by accident. A node that had a card and has lost its last
// public skill therefore says so: its registration carries
// "a2a_card": null, and the hub deletes the card and its index rows and
// answers card_status "withdrawn".
//
// Only a node that issued a card sends the withdrawal (a fresh install
// sends nothing and the hub answers "absent"), and only until a hub
// confirms it: the confirmation is kept with the issued card
// (a2a_card.json), so a restart neither repeats it nor forgets that it is
// owed. Publishing a card again clears it.

import (
	"bytes"
	"encoding/json"
	"log"
	"time"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// isCardWithdrawal reports an a2a_card field that withdraws the card.
func isCardWithdrawal(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == hubapi.WithdrawCard
}

// cardWithdrawalLocked is the a2a_card of a registration at hubURL by a
// node with no public skill: the withdrawal when a card was issued and
// hubURL has not confirmed withdrawing it, else nothing. The caller holds
// netCard.mu.
func (d *Daemon) cardWithdrawalLocked(hubURL string) json.RawMessage {
	d.loadIssuedCardLocked()
	ic := d.netCard.issued
	if ic == nil || ic.WithdrawnFrom == hubURL {
		return nil
	}
	log.Printf("anet: no public skill any more; withdrawing network card seq %d from %s", ic.Seq, hubURL)
	return json.RawMessage(hubapi.WithdrawCard)
}

// cardPublishedAgainLocked forgets a confirmed withdrawal once a card is
// sent again. The caller holds netCard.mu.
func (d *Daemon) cardPublishedAgainLocked() {
	if ic := d.netCard.issued; ic != nil && ic.WithdrawnFrom != "" {
		ic.WithdrawnFrom = ""
		d.persistIssuedCardLocked()
	}
}

// noteWithdrawalAnswer records the hub's answer to a withdrawal. Only
// "withdrawn" is a confirmation; a hub that does not know the withdrawal
// (it answers "invalid", or ignores the field) is asked again at the next
// registration, which costs nothing and succeeds once the hub is upgraded.
func (d *Daemon) noteWithdrawalAnswer(hubURL string, out hubapi.RegisterResponse) {
	d.netCard.mu.Lock()
	defer d.netCard.mu.Unlock()
	d.netCard.lastPub = CardPublication{Hub: hubURL, Status: out.CardStatus, Error: out.CardError,
		At: time.Now().UTC().Format(time.RFC3339)}
	if out.CardStatus != hubapi.CardStatusWithdrawn {
		log.Printf("anet: %s did not withdraw this node's network card (card_status %q: %s); it may go on listing it",
			hubURL, out.CardStatus, out.CardError)
		return
	}
	if ic := d.netCard.issued; ic != nil && ic.WithdrawnFrom != hubURL {
		ic.WithdrawnFrom = hubURL
		d.persistIssuedCardLocked()
	}
}
