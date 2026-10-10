package daemon

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// outboxRows are d's queued rows of task ix, oldest first.
func outboxRows(t testing.TB, d *Daemon, ix string) []interactions.OutboxRow {
	t.Helper()
	rows, err := d.ix.OutboxRows(ix)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// allOutboxRows are every queued row of d, oldest first.
func allOutboxRows(t testing.TB, d *Daemon) []interactions.OutboxRow {
	t.Helper()
	var rows []interactions.OutboxRow
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		rows, err = tx.AllOutboxRows()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// rowVersions are the versions of d's row id, oldest first.
func rowVersions(t testing.TB, d *Daemon, id int64) []interactions.OutboxVer {
	t.Helper()
	_, vers, err := d.ix.OutboxState(id)
	if err != nil {
		t.Fatal(err)
	}
	return vers
}

// askHubNow makes d ask its hub about every row the hub took, as if each
// status query were due (A4): rows whose recipient acknowledged them with
// a token are confirmed and leave the queue.
func askHubNow(t testing.TB, d *Daemon) {
	t.Helper()
	ctx := context.Background()
	hub := d.configuredHub()
	for _, r := range allOutboxRows(t, d) {
		unlock := d.outboxLocks.lock(outboxLockKey(r.ID))
		row, vers, err := d.ix.OutboxState(r.ID)
		if err == nil {
			d.queryOutbox(ctx, row, vers, hub)
		}
		unlock()
	}
}

// settleDelivery is a round trip of the mailbox: to polls (acknowledging
// what it took with its tokens) and from asks its hub about what it sent,
// so every message from → to that arrived is confirmed.
func settleDelivery(t testing.TB, from, to *Daemon) {
	t.Helper()
	if err := to.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	askHubNow(t, from)
}

// retryNow makes an attempt at d's row id now, whatever its backoff, as the
// first pass after a start does (ENG-Q8); it returns what the attempt did.
func retryNow(t testing.TB, d *Daemon, id int64) error {
	t.Helper()
	return d.attemptOutboxAt(context.Background(), id, true)
}

// confirmDelegation lets prov take req's delegations and req hear that it
// did: the delegation rows are confirmed, so what the requester sends next
// on the task goes (engine ENG-O2).
func confirmDelegation(t testing.TB, req, prov *Daemon) {
	t.Helper()
	settleDelivery(t, req, prov)
}

// deadHubURL is a hub address nothing listens on: a request to it fails to
// connect, so no byte of what it carried was handed over.
const deadHubURL = "http://127.0.0.1:1"

// setHubURL points d at the hub at url.
func setHubURL(d *Daemon, url string) {
	d.mu.Lock()
	d.cfg.HubURL = url
	d.mu.Unlock()
}

// knowPeer has d look peer up while its hub answers, so it holds peer's
// keys and can seal to it later without asking.
func knowPeer(t testing.TB, d *Daemon, peer string) {
	t.Helper()
	if _, err := d.recipientKeys(context.Background(), peer, ""); err != nil {
		t.Fatal(err)
	}
}

// holdGateOpen holds d's outbound gate open, whatever its notice says: a
// test that moves d's clock past the notice's age, about something else.
func holdGateOpen(d *Daemon) {
	d.setIdentityGate(func() GateState { return GateState{true, IdentityOK, reasonIdentityOK} })
}
