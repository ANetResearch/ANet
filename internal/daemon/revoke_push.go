package daemon

// revoke_push.go takes this node's withdrawals of the delivery credentials
// it issued to its hub, and keeps them in line with the peer lists
// (sealed-sender §4.3.8).
//
// A credential is withdrawn here first (caps_issued.revoked_at, in the
// transaction that decides it): from then on this node does not give it
// again and treats it as none on the direct path. The hub learns of it
// from the push loop, in batches of up to 1000 signed POST /mailbox/revoke,
// retried 5 seconds after a failure and doubling to 10 minutes until the
// hub has them (SSR-D20). At each registration the hub says how many of
// this node's withdrawals it holds; fewer than this node pushed means it
// lost some (a restore from a backup), and they are pushed again (SSR-D21).
// A hub that holds as many as it keeps answers revocation_list_full; this
// node then replaces its mailbox signing key, which voids every credential
// it issued at once (§4.3.8 item 4).
//
// Which credentials the peer lists withdraw (§4.3.8 item 1): a peer taken
// off the allow and trust lists loses its contact peer credential; a peer
// on the deny list loses every credential. The control routes that edit a
// list withdraw them after the list file is written and before they answer
// (inbound_api.go). The lists are files the operator may also edit by hand,
// and the daemon may stop between the file write and the withdrawal, so
// reconcileCapsWithLists applies the same rule to every credential at
// start and every five minutes.
//
// Nothing here is shown to the user: a withdrawal that waits for the hub
// is in doctor (sender_privacy.grants), the rest is logged.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// Retries of the push (SS §2.12): the first 5 seconds after a failure,
// doubling up to 10 minutes.
const (
	revokeRetryBase = 5 * time.Second
	revokeRetryMax  = 10 * time.Minute
)

// revokeTickEvery is how often the push loop looks for withdrawals to
// push; listsReconcileEvery how often it applies the peer lists to the
// credentials issued. Knobs so a test can drive the loop.
var (
	revokeTickEvery     = newKnob(5 * time.Second)
	listsReconcileEvery = newKnob(5 * time.Minute)
)

// reasonRevocationListFull is the hub's refusal of a batch when it holds as
// many withdrawals of this node as it keeps (core §2.15.1).
const reasonRevocationListFull = "revocation_list_full"

// revokeRetryAfter is the wait after failures consecutive failed pushes
// (failures >= 1).
func revokeRetryAfter(failures int) time.Duration {
	d := revokeRetryBase
	for i := 1; i < failures && d < revokeRetryMax; i++ {
		d *= 2
	}
	return min(d, revokeRetryMax)
}

// revokePush is the push loop's state.
type revokePush struct {
	kick chan struct{} // made by New; a send wakes the loop
	run  sync.Mutex    // one push at a time
	mu   sync.Mutex
	// failures and retryAt: the backoff after failed pushes (zero: none).
	failures int
	retryAt  time.Time
	// requests counts the POST /mailbox/revoke sent (tests).
	requests int
}

// kickRevokePush wakes the push loop: a withdrawal was committed.
func (d *Daemon) kickRevokePush() {
	select {
	case d.caps.push.kick <- struct{}{}:
	default:
	}
}

// revokeLoop pushes withdrawals when kicked and on every tick, and applies
// the peer lists every listsReconcileEvery.
func (d *Daemon) revokeLoop(ctx context.Context) {
	tick := time.NewTicker(revokeTickEvery.get())
	defer tick.Stop()
	lists := time.NewTicker(listsReconcileEvery.get())
	defer lists.Stop()
	d.revokeTick(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.caps.push.kick:
			d.revokeTick(ctx, time.Now())
		case now := <-tick.C:
			d.revokeTick(ctx, now)
		case <-lists.C:
			if _, err := d.reconcileCapsWithLists(); err != nil {
				log.Printf("anet: withdraw delivery credentials by the peer lists: %v", err)
			}
		}
	}
}

// revokeTick pushes what is due at now: nothing while a backoff runs, and
// nothing while this node is not registered at its hub (the hub refuses
// the request; the registration compares and kicks, SSR-D21).
func (d *Daemon) revokeTick(ctx context.Context, now time.Time) {
	p := &d.caps.push
	p.run.Lock()
	defer p.run.Unlock()
	p.mu.Lock()
	wait := !p.retryAt.IsZero() && now.Before(p.retryAt)
	p.mu.Unlock()
	if wait || d.ix == nil {
		return
	}
	hub := d.config().HubURL
	if hub == "" {
		return
	}
	if r := d.joinSnapshot(); r == nil || r.State != JoinRegistered || !sameHub(r.Hub, hub) {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	err := d.pushRevocations(cctx, hub)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.failures, p.retryAt = 0, time.Time{}
		return
	}
	p.failures++
	p.retryAt = now.Add(revokeRetryAfter(p.failures))
	if p.failures == 1 || p.failures%10 == 0 {
		log.Printf("anet: %s has not taken this node's withdrawn delivery credentials yet: %v (anet tries again on its own)", hub, err)
	}
}

// pushRevocations sends the withdrawals the hub does not have yet, oldest
// first, a batch at a time, until none is left or one fails.
func (d *Daemon) pushRevocations(ctx context.Context, hub string) error {
	for {
		ids, err := d.ix.UnpushedRevocations(hubapi.MailboxRevokeMaxIDs)
		if err != nil || len(ids) == 0 {
			return err
		}
		req := hubapi.MailboxRevokeRequest{CapIDs: make([]string, len(ids))}
		for i, id := range ids {
			req.CapIDs[i] = hubapi.B64(id)
		}
		d.caps.push.mu.Lock()
		d.caps.push.requests++
		d.caps.push.mu.Unlock()
		rep := d.hubSignedRaw(ctx, hub, http.MethodPost, hubapi.MailboxRevokePath, relayauth.ActionRevoke, req)
		switch {
		case rep.Status != 0 && hubWireCheck(hub, rep) != nil:
			// Another wire, whatever the status: not pushed (core CORE-V1).
			return hubWireCheck(hub, rep)
		case rep.Status == http.StatusOK:
			var out hubapi.MailboxRevokeResponse
			if err := json.Unmarshal(rep.Body, &out); err != nil {
				return fmt.Errorf("the hub's answer to %s does not read: %v", hubapi.MailboxRevokePath, err)
			}
			if _, err := d.ix.MarkRevocationsPushed(ids); err != nil {
				return err
			}
			if len(ids) < hubapi.MailboxRevokeMaxIDs {
				return nil
			}
		case rep.Status == http.StatusTooManyRequests && hubReplyReason(rep) == reasonRevocationListFull:
			return d.revocationListFull(ctx, hub)
		case rep.Status == 0:
			return rep.Err
		default:
			return fmt.Errorf("the hub answered %d %s", rep.Status, hubReplyReason(rep))
		}
	}
}

// hubReplyReason is the reason code of a hub's error body (core G1), ""
// when it has none.
func hubReplyReason(rep hubReply) string {
	var body struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(rep.Body, &body) != nil || !reasonCodeRe.MatchString(body.Reason) {
		return ""
	}
	return body.Reason
}

// listFullMinGapMS is the least time between two key replacements made
// because a hub's revocation list is full.
const listFullMinGapMS = 10 * 60 * 1000

// revocationListFull is SS §4.3.8 item 4: the hub keeps no more of this
// node's withdrawals. A new mailbox signing key (epoch + 1) replaces the
// old one in a transaction that also empties caps_issued, and the key set
// with the new key is published (SSR-W10, SSR-W11): every credential the
// old key signed stops delivering, which is every withdrawal at once. The
// peers that still talk to this node get new credentials with its next
// messages; until then they write by first contact. Logged, not shown.
func (d *Daemon) revocationListFull(ctx context.Context, hub string) error {
	// A new key every time the hub says full would send every contact back
	// to first contact each time; once per listFullMinGapMS is enough, as the
	// hub drops the revocations of a lower epoch when the new key arrives.
	now := d.nowMS()
	if last := d.listFullRotatedAt.Load(); last != 0 && now >= last && now-last < listFullMinGapMS {
		return fmt.Errorf("the hub keeps no more withdrawals; anet replaced the mailbox signing key a short while ago and waits before it does so again")
	}
	var m seal.Mbox
	err := d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		m, err = d.rotateMboxTx(tx)
		return err
	})
	if err != nil {
		return fmt.Errorf("the hub keeps no more withdrawals and the mailbox signing key could not be replaced: %w", err)
	}
	d.listFullRotatedAt.Store(now)
	log.Printf("anet: %s keeps no more withdrawn delivery credentials of this node; anet made a new mailbox signing key (number %d), "+
		"which withdraws them all at once", hub, m.Epoch)
	if err := d.republishKeys(ctx, hub); err != nil {
		// The mailbox loop publishes again (SSR-D4).
		log.Printf("anet: publish the new mailbox signing key to %s: %v (anet tries again on its own)", hub, err)
	}
	return nil
}

// noteRegisterRevocations is SSR-D21: a registration answer says the hub
// holds hubCount of this node's unexpired withdrawals (nil: it did not
// say). Fewer than this node pushed in the last 31 days means the hub lost
// some, and every withdrawal of that window is pushed again (the hub takes
// one it holds as a no-op). Withdrawals not pushed yet are not compared:
// the loop pushes them anyway.
func (d *Daemon) noteRegisterRevocations(hubCount *uint64) {
	if hubCount == nil || d.ix == nil {
		return
	}
	now := d.nowMS()
	since := uint64(0)
	if now > interactions.IssuedRevokedKeepMS {
		since = now - interactions.IssuedRevokedKeepMS
	}
	pushed, err := d.ix.PushedRevocationsSince(since)
	if err != nil {
		log.Printf("anet: count the withdrawn delivery credentials: %v", err)
		return
	}
	if uint64(pushed) <= *hubCount {
		return
	}
	var n int64
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		n, err = tx.RequeueRevocations(since)
		return err
	}); err != nil {
		log.Printf("anet: queue the withdrawn delivery credentials again: %v", err)
		return
	}
	log.Printf("anet: the hub holds %d of this node's withdrawn delivery credentials, %d were sent to it; sending %d again",
		*hubCount, pushed, n)
	d.kickRevokePush()
}

// listEdit is a change a control route made to the peer lists.
type listEdit uint8

const (
	listEditAllow  listEdit = iota + 1 // the peer went on the allow or trust list
	listEditDeny                       // the peer went on the deny list
	listEditRemove                     // the peer went off every list
)

// listRevocations is the rule of SS §4.3.8 item 1 for one peer, as the
// filters of what to withdraw: everything for a denied peer, the contact
// peer credential for a peer on neither the allow nor the trust list,
// nothing for a listed one.
func listRevocations(aid string, ps peerSets) []interactions.IssuedFilter {
	switch {
	case ps.denied(aid):
		return []interactions.IssuedFilter{{Grantee: aid}}
	case !ps.allowed(aid):
		return []interactions.IssuedFilter{{Grantee: aid, Scope: interactions.CapScopePeer, Class: seal.CapClassContact}}
	}
	return nil
}

// readPeersStrict reads the three peer lists, failing when one cannot be
// read: a withdrawal cannot be undone, so a list that cannot be read is
// not taken for an empty one here (readPeers fails closed for decisions
// that can be made again).
func (d *Daemon) readPeersStrict() (peerSets, error) {
	var ps peerSets
	var err error
	if ps.deny, err = readPeerFile(d.peerFile(peersDenyFile)); err != nil {
		return ps, err
	}
	if ps.allow, err = readPeerFile(d.peerFile(peersAllowFile)); err != nil {
		return ps, err
	}
	if ps.trust, err = readPeerFile(d.peerFile(peersTrustFile)); err != nil {
		return ps, err
	}
	return ps, nil
}

// revokeAfterListEdit withdraws what a list edit of aid takes away, in one
// transaction, once the list file is written and before the control route
// answers (SS §4.3.8 item 1). A deny withdraws everything and a removal the
// contact peer credential, whatever the files say otherwise; after an
// allow the lists are read, for a peer that is also denied.
func (d *Daemon) revokeAfterListEdit(aid string, edit listEdit) (int64, error) {
	var fs []interactions.IssuedFilter
	switch edit {
	case listEditDeny:
		fs = []interactions.IssuedFilter{{Grantee: aid}}
	case listEditRemove:
		fs = []interactions.IssuedFilter{{Grantee: aid, Scope: interactions.CapScopePeer, Class: seal.CapClassContact}}
	case listEditAllow:
		ps, err := d.readPeersStrict()
		if err != nil {
			d.count(cntRevokeListsSkipped)
			return 0, nil // the start and the five-minute pass apply the lists once they read
		}
		fs = listRevocations(aid, ps)
	}
	return d.revokeIssued(fs)
}

// revokeIssued withdraws what the filters select, in one transaction, and
// has it pushed.
func (d *Daemon) revokeIssued(fs []interactions.IssuedFilter) (int64, error) {
	if len(fs) == 0 || d.ix == nil {
		return 0, nil
	}
	now := d.nowMS()
	var n int64
	err := d.ix.Update(func(tx *interactions.Tx) error {
		n = 0
		for _, f := range fs {
			k, err := tx.RevokeIssued(f, now)
			if err != nil {
				return err
			}
			n += k
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		d.kickRevokePush()
	}
	return n, nil
}

// withdrawAfterListEdit is revokeAfterListEdit for a control route: an
// error is one sentence saying that the list did change, what failed and
// what happens next.
func (d *Daemon) withdrawAfterListEdit(aid string, edit listEdit) error {
	if _, err := d.revokeAfterListEdit(aid, edit); err != nil {
		return fmt.Errorf("anet: the peer list now has the change for %s, but anet could not withdraw the delivery credentials it gave that peer (%v); "+
			"anet tries again within five minutes and at its next start, or run this command again", aid, err)
	}
	return nil
}

// errListsUnreadable is reconcileCapsWithLists' answer when a peer list
// cannot be read.
var errListsUnreadable = errors.New("anet: a peer list cannot be read; no delivery credential was withdrawn by the lists")

// reconcileCapsWithLists applies SS §4.3.8 item 1 to every peer this node
// gave a credential it has not withdrawn: what a list edit by hand, or a
// stop between a list write and its withdrawal, left given (SSR-D29). It
// runs at start and every five minutes, and withdraws nothing while a list
// cannot be read. It returns how many it withdrew.
func (d *Daemon) reconcileCapsWithLists() (int64, error) {
	if d.ix == nil {
		return 0, nil
	}
	ps, err := d.readPeersStrict()
	if err != nil {
		d.count(cntRevokeListsSkipped)
		return 0, fmt.Errorf("%w: %v", errListsUnreadable, err)
	}
	grantees, err := d.ix.IssuedGrantees()
	if err != nil {
		return 0, err
	}
	var fs []interactions.IssuedFilter
	for _, g := range grantees {
		fs = append(fs, listRevocations(g, ps)...)
	}
	n, err := d.revokeIssued(fs)
	if n > 0 {
		d.countErr(cntRevokeListsReconcil, fmt.Errorf("withdrew %d delivery credentials the peer lists no longer allow", n))
	}
	return n, err
}
