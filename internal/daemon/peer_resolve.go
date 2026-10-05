package daemon

// peer_resolve.go is where this node learns another agent's key history
// (KEL), encryption key set, network card and witness notice without naming
// that agent to any hub (sealed-sender §4.3.7, SSR-D14 to SSR-D18).
//
// resolvePeer takes the material, in this order, from
//
//  1. this node's own record of the peer (peer_identity);
//  2. the discovery cache: the agents a listing of the hub's registry
//     returned (anet find, MCP list_agents, the local A2A agent list),
//     kept for an hour once they verified here;
//  3. a directory bucket: POST /directory:bucket, on the anonymous side,
//     for every entry whose directory hash starts with a few bits of the
//     peer's (hubapi.DirQueryBits: at most 10, whatever the hub reports),
//     and the one entry this node opens with the AID it already knows
//     (dirbox.Open). The hub learns a bucket of agents, never the agent.
//
// and concludes found, not_found (no entry, and the answer was whole),
// unverifiable (entries, none of which verifies, or two that fork) or
// unavailable (the hub could not be asked, or its answer was partial and
// did not hold the peer). The outbound path maps the three failures to
// recipient_unknown, recipient_unverifiable and "queued, the hub is out of
// reach" (engine ENG-Q5).
//
// What this node holds of a peer is refreshed through a bucket only when
// there is a reason to (SSR-D18): a key set with no usable key left, the
// hub saying kid_stale (refreshForKidStale), a capability refused
// (reviewPeer, SSR-D11), a recipient the hub does not know (refreshOnUnknown,
// SSR-D12), and once a day for an active contact (recheckActiveContact).
// There is no refresh on a timer.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/dirbox"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The resolver's bounds (sealed-sender §2.12). Code constants: there is
// nothing for an operator to tune.
const (
	// discoveryTTLMS and discoveryMax bound the discovery cache.
	discoveryTTLMS uint64 = 3600 * 1000
	discoveryMax          = 4096
	// bucketTTLMS is how long a whole bucket answer is kept; bucketCacheBytes
	// bounds the boxes kept across all of them.
	bucketTTLMS      uint64 = 3600 * 1000
	bucketCacheBytes        = 32 << 20
	// dirMaxBitsTTLMS is how long a hub's dir_max_bits is taken as said
	// before GET /hub/identity is asked again (a bucket answer restates it).
	dirMaxBitsTTLMS uint64 = 3600 * 1000
	// activeRecheckMS is the most often an active contact's key set is
	// fetched again (SSR-D18); activeWindowMS is how recently this node must
	// have written to a peer for it to be an active contact.
	activeRecheckMS uint64 = 24 * 3600 * 1000
	// reviewEveryMS: after a 403 access_invalid, a peer's material is
	// fetched again at most this often; refusals in between take the same
	// answer (SSR-D11 item 1).
	reviewEveryMS uint64 = 60 * 1000
	// unknownRefreshMS: a 404 recipient_unknown refreshes the recipient
	// through a bucket only when that was not done in this long, and one
	// queued message at most this often (SSR-D12).
	unknownRefreshMS uint64 = 3600 * 1000
	// smallBucketsWindowMS is the window of /status.sender_privacy.small_buckets_24h.
	smallBucketsWindowMS uint64 = 24 * 3600 * 1000
	// resolveTimeout bounds one bucket query, all its pages.
	resolveTimeout = hubCallTimeout
)

// Counters the resolver keeps (ReceiveStats).
const (
	cntDirBucketSmall    = "dir-bucket-small"      // a bucket of fewer than 32 entries; the hub's queries are made one bit wider (SSR-D15a)
	cntDirEntrySkipped   = "dir-entry-skipped"     // an entry under the target's hash that did not open or did not verify (SSR-W22)
	cntDirBucketNarrow   = "dir-bucket-too-narrow" // the hub refused a query as narrower than its dir_max_bits; asked again wider
	cntDirBucketBadEntry = "dir-bucket-bad-entry"  // an entry of a bucket answer that is not well formed or not in the bucket
)

// peerVerdict is what resolvePeer concluded (SSR-D15).
type peerVerdict uint8

const (
	verdictFound peerVerdict = iota + 1
	verdictNotFound
	verdictUnverifiable
	verdictUnavailable
)

func (v peerVerdict) String() string {
	switch v {
	case verdictFound:
		return "found"
	case verdictNotFound:
		return "not_found"
	case verdictUnverifiable:
		return "unverifiable"
	case verdictUnavailable:
		return "unavailable"
	}
	return "unknown"
}

// peerSource is where the material came from.
type peerSource uint8

const (
	sourceLocal peerSource = iota + 1
	sourceDiscovery
	sourceBucket
)

// resolveNeed is what the caller needs of the material for it to count as
// found.
type resolveNeed uint8

const (
	// needKeys: a key set that verifies against the KEL with a key usable
	// now: what sealing to the peer takes.
	needKeys resolveNeed = iota
	// needKeySet: a key set that verifies against the KEL, usable now or
	// not: its mailbox epoch is what a refusal review reads.
	needKeySet
	// needKEL: the key history alone.
	needKEL
	// needCard: the key history and, when the agent publishes one, its
	// card. A peer found without a card publishes none.
	needCard
)

// resolveOpts are resolvePeer's options.
type resolveOpts struct {
	need resolveNeed
	// fresh skips this node's own record (step 1): the daily recheck of an
	// active contact asks the directory, but may take a cached bucket.
	fresh bool
	// bypass skips this node's own record, the discovery cache and every
	// cached bucket answer: a kid_stale, a key set with no usable key, a
	// refusal review, a recipient the hub does not know.
	bypass bool
	// record writes what was found to peer_identity: this node is about to
	// write to the peer (first contact, `anet peers allow`, a refresh of a
	// peer it writes to). Reading a directory alone records nothing.
	record bool
	// pin is the peer_identity pin of a recorded peer ("" leaves it).
	pin string
}

// peerMaterial is what resolvePeer found of a peer.
type peerMaterial struct {
	AID    string
	Source peerSource
	KEL    []identity.SignedEvent
	// Keys is the key set, verified against KEL; nil when there is none
	// this node may use. Epoch is its mailbox epoch.
	Keys  *peerKeySet
	Epoch uint64
	// Card is the agent's network card, verified against KEL and signed by
	// AID; nil when the agent publishes none (or the source carries none).
	// CardErr says why a card the source carried is withheld: the a2acard
	// code, or this node's words — never the card's.
	Card    json.RawMessage
	CardErr string
	// Notice is the witness notice the source carried; NoticeOK when it
	// passed identity.VerifyNotice for the KEL's tip at arrival = now
	// (KEL C-51).
	Notice   []byte
	NoticeOK bool
	// Home is the AID of the hub the directory entry names as the agent's
	// home (SSR-D13's third source); "" when not from a bucket.
	Home string
	// At is when it was fetched (unix ms).
	At uint64
	// Err says why the verdict is not found.
	Err error
}

// satisfies reports whether m holds what need asks for at now.
func (m *peerMaterial) satisfies(need resolveNeed, now uint64) bool {
	if len(m.KEL) == 0 {
		return false
	}
	switch need {
	case needKeys:
		if m.Keys == nil {
			return false
		}
		_, err := seal.SelectKey(m.Keys.set, now)
		return err == nil
	case needKeySet:
		return m.Keys != nil
	}
	return true
}

// peerResolveError is a resolvePeer verdict other than found, as an error:
// what a send that needed the material fails with. Not found is the hub's
// answer about the recipient (permanentRefusal: recipient_unknown);
// unavailable is the hub out of reach (hubUnreachable: retried).
type peerResolveError struct {
	AID     string
	Verdict peerVerdict
	Cause   error
}

func (e *peerResolveError) Error() string {
	var what string
	switch e.Verdict {
	case verdictNotFound:
		what = "no hub knows " + e.AID
	case verdictUnverifiable:
		what = "the directory's entries for " + e.AID + " do not verify"
	default:
		what = "the directory cannot be read now"
	}
	if e.Cause != nil {
		return what + ": " + e.Cause.Error()
	}
	return what
}

func (e *peerResolveError) Unwrap() error { return e.Cause }

// resolveVerdictOf returns the verdict of a peerResolveError in err's
// chain, or 0.
func resolveVerdictOf(err error) peerVerdict {
	var re *peerResolveError
	if errors.As(err, &re) {
		return re.Verdict
	}
	return 0
}

// peerResolver is the resolver's state, in memory only: a restart forgets
// it (and with it every small-bucket cut, SSR-D15a).
type peerResolver struct {
	mu sync.Mutex
	// disc is the discovery cache by AID, discOrder its insertion order.
	disc      map[string]*peerMaterial
	discOrder []string
	// buckets are whole bucket answers by bucketKey; bucketBytes their size.
	buckets     map[string]*bucketAnswer
	bucketBytes int
	// bucketFlights and reviewFlights are the bucket queries and the
	// refusal reviews in progress, by key: a second caller waits for the
	// first one's answer rather than ask again.
	bucketFlights flights[*bucketAnswer]
	reviewFlights flights[peerReview]
	// dirMax is each hub's dir_max_bits as last said, and when.
	dirMax map[string]dirMaxBits
	// cut is each hub's small-bucket cut (SSR-D15a); small the times of the
	// small buckets of the last 24 hours, oldest first.
	cut   map[string]int
	small []uint64
	// refreshed is when each peer's material was last fetched through a
	// bucket (SSR-D12's "within the last hour").
	refreshed map[string]uint64
	// homes is the home hub each peer's directory entry named (SSR-D13).
	homes map[string]string
	// reviews are the refusal reviews of the last minute (SSR-D11).
	reviews map[string]peerReviewMemo
	// unknownRows is when each queued message last refreshed its recipient
	// on a 404 (SSR-D12).
	unknownRows map[int64]uint64
	// rechecks is when each active contact was last rechecked (SSR-D18).
	rechecks map[string]uint64
}

// dirMaxBits is a hub's dir_max_bits as it said it.
type dirMaxBits struct {
	bits int
	at   uint64
}

// peerReviewMemo is a refusal review and when it was made.
type peerReviewMemo struct {
	review peerReview
	at     uint64
}

// bucketAnswer is a whole bucket: its entries across every page.
type bucketAnswer struct {
	bits    int
	prefix  []byte
	entries []hubapi.BucketEntry
	partial bool
	at      uint64
	size    int
}

// flights are calls in progress by key, which later callers of the same
// key wait for and share.
type flights[T any] struct {
	mu sync.Mutex
	m  map[string]*flight[T]
}

type flight[T any] struct {
	done chan struct{}
	v    T
	err  error
}

// do runs fn for key unless a call for key is in progress, whose result it
// then takes.
func (fs *flights[T]) do(key string, fn func() (T, error)) (T, error) {
	fs.mu.Lock()
	if f, ok := fs.m[key]; ok {
		fs.mu.Unlock()
		<-f.done
		return f.v, f.err
	}
	if fs.m == nil {
		fs.m = map[string]*flight[T]{}
	}
	f := &flight[T]{done: make(chan struct{})}
	fs.m[key] = f
	fs.mu.Unlock()
	defer func() {
		fs.mu.Lock()
		delete(fs.m, key)
		fs.mu.Unlock()
		close(f.done)
	}()
	f.v, f.err = fn()
	return f.v, f.err
}

// ---- resolvePeer (SSR-D15)

// resolvePeer finds aid's material by SSR-D15's three sources, and
// records it when o.record. It names aid to no hub.
func (d *Daemon) resolvePeer(ctx context.Context, aid string, o resolveOpts) (peerMaterial, peerVerdict) {
	now := d.nowMS()
	if !identity.ValidAID(aid) {
		return peerMaterial{AID: aid, Err: fmt.Errorf("%q is not an agent id", aid)}, verdictNotFound
	}
	// 1. This node's own record.
	if !o.fresh && !o.bypass {
		if m, ok := d.localPeer(aid, now); ok && m.satisfies(o.need, now) && o.need != needCard {
			if o.record && o.pin != "" {
				d.pinIfUnpinned(aid, o.pin)
			}
			return m, verdictFound
		}
	}
	// 2. The discovery cache.
	if !o.bypass {
		if m, ok := d.peers.discovered(aid, now); ok && d.againstRecord(&m, now) == verdictFound && m.satisfies(o.need, now) {
			if o.record {
				d.recordPeer(m, o.pin)
			}
			return m, verdictFound
		}
	}
	// 3. A directory bucket.
	m, v := d.bucketPeer(ctx, aid, o, now)
	if v != verdictFound {
		return m, v
	}
	if !m.satisfies(o.need, now) {
		// The entry verified, but holds no key set this node may use now
		// (expired keys, or older than the one it holds): nothing to
		// conclude about the agent, and nothing to seal to yet.
		m.Err = errors.New("its key set holds no key usable now")
		return m, verdictUnavailable
	}
	if o.record {
		d.recordPeer(m, o.pin)
	}
	return m, verdictFound
}

// localPeer is SSR-D15 step 1: this node's record of aid — a KEL that
// replays to it, and the key set stored with it when that still verifies
// (storedKeySet).
func (d *Daemon) localPeer(aid string, now uint64) (peerMaterial, bool) {
	row, err := d.ix.PeerIdentity(aid)
	if err != nil {
		return peerMaterial{}, false
	}
	kel, ok := d.peerKEL(aid)
	if !ok {
		return peerMaterial{}, false
	}
	m := peerMaterial{AID: aid, Source: sourceLocal, KEL: kel, At: uint64(row.KeysCheckedAt)}
	if ks, ok := d.storedKeySet(row, now); ok {
		m.Keys, m.Epoch = ks, ks.set.Mbox.Epoch
	}
	return m, true
}

// recordPeer writes m to peer_identity (the KEL by the extension rule, the
// key set by the high-water rule, notePeer), so the capability rules run in
// the same transaction (UpdatePeerIdentity → peerWritten: SSR-D19, ENG-K4).
func (d *Daemon) recordPeer(m peerMaterial, pin string) {
	if m.Source == sourceLocal {
		return
	}
	// v030-todo(KEL-11): KEL D-26 — directory material is written, and
	// sealed to, only with a notice that passed C-51 at arrival = now about
	// its KEL's tip (m.NoticeOK); until the hub serves its notices in every
	// entry (KEL-5) and this node keeps the federation's hub KELs (D-24),
	// that would refuse every peer.
	if err := d.notePeer(m.AID, m.KEL, m.Keys, pin, true); err != nil {
		d.logOnce("record-peer:"+m.AID, "anet: record %s: %v", m.AID, err)
	}
}

// pinIfUnpinned gives aid's record the pin when it has none.
func (d *Daemon) pinIfUnpinned(aid, pin string) {
	if err := d.notePeer(aid, nil, nil, pin, false); err != nil {
		d.logOnce("pin-peer:"+aid, "anet: pin %s: %v", aid, err)
	}
}

// bucketPeer is SSR-D15 step 3: aid's bucket at this node's hub, the
// entries filed under aid's hash opened and verified, the one with the
// longest KEL taken (SSR-W22) and checked against this node's record.
func (d *Daemon) bucketPeer(ctx context.Context, aid string, o resolveOpts, now uint64) (peerMaterial, peerVerdict) {
	m := peerMaterial{AID: aid, Source: sourceBucket, At: now}
	hub := strings.TrimRight(d.config().HubURL, "/")
	if hub == "" {
		m.Err = errors.New("this node has no hub")
		return m, verdictUnavailable
	}
	h, err := dirbox.Hash(aid)
	if err != nil {
		m.Err = err
		return m, verdictNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	ans, cached, err := d.bucketOf(ctx, hub, h, o.bypass)
	if err != nil {
		m.Err = err
		return m, verdictUnavailable
	}
	opened, failed := hubapi.OpenEntries(aid, h, ans.entries)
	if cached && len(opened) == 0 {
		// A bucket kept from earlier does not hold the agent: it may have
		// registered since. Asked again before concluding anything.
		if ans, _, err = d.bucketOf(ctx, hub, h, true); err != nil {
			m.Err = err
			return m, verdictUnavailable
		}
		cached = false
		opened, failed = hubapi.OpenEntries(aid, h, ans.entries)
	}
	if !cached {
		d.peers.noteRefreshed(aid, now)
	}
	for range failed {
		d.count(cntDirEntrySkipped)
	}
	if len(opened) == 0 {
		if ans.partial {
			m.Err = errors.New("the hub's answer was partial: a peer hub did not answer")
			return m, verdictUnavailable
		}
		m.Err = errors.New("no entry in the directory")
		return m, verdictNotFound
	}
	best, err := d.selectDirEntry(aid, opened, now)
	if err != nil {
		m.Err = err
		if ans.partial && !errors.Is(err, identity.ErrKELFork) {
			// The entry that verifies may be the missing peer hub's.
			return m, verdictUnavailable
		}
		return m, verdictUnverifiable
	}
	best.Source, best.At = sourceBucket, now
	v := d.againstRecord(&best, now)
	if v == verdictFound && best.Home != "" {
		d.peers.noteHome(aid, best.Home)
	}
	return best, v
}

// againstRecord checks material from the directory against this node's
// record of the peer: a KEL that forks from the stored one is unverifiable;
// one the stored KEL extends gives way to it, and the key set must then
// verify against the stored KEL; a key set older than the stored one is not
// used, and one that differs at the same seq is unverifiable.
func (d *Daemon) againstRecord(m *peerMaterial, now uint64) peerVerdict {
	row, err := d.ix.PeerIdentity(m.AID)
	if err != nil && !errors.Is(err, interactions.ErrNotFound) {
		m.Err = err
		return verdictUnavailable
	}
	var stored []identity.SignedEvent
	if row != nil && len(row.KEL) > 0 {
		stored, _ = identity.UnmarshalKEL(row.KEL)
	}
	use, _, err := mergeKEL(stored, m.KEL)
	if err != nil {
		d.count(dropKELFork)
		m.Err = fmt.Errorf("the directory's KEL forks from the one this node holds: %w", err)
		return verdictUnverifiable
	}
	m.KEL = use
	if m.Keys == nil {
		return verdictFound
	}
	signed, err := seal.UnmarshalSignedEncKeySet(m.Keys.signed)
	if err != nil {
		m.Keys = nil
		return verdictFound
	}
	set, err := seal.VerifyEncKeySet(signed, m.AID, use, now)
	if err != nil {
		// Signed under a key state this node knows to be past.
		m.Keys = nil
		return verdictFound
	}
	switch seal.DecideHighWater(seenOf(row), set.Seq, signed.Set) {
	case seal.Ignore:
		d.count(keysRollback)
		m.Keys = nil
	case seal.Fork:
		d.count(keysFork)
		m.Keys = nil
		m.Err = fmt.Errorf("the directory's key set differs from the stored one at seq %d", set.Seq)
		return verdictUnverifiable
	default:
		m.Keys = &peerKeySet{signed: m.Keys.signed, set: set}
		m.Epoch = set.Mbox.Epoch
	}
	return verdictFound
}

// selectDirEntry is SSR-W22: every opened entry is verified alone (its KEL
// replays to aid; its key set verifies against that KEL; its card, when it
// has one, verifies against that KEL and is aid's; its notice is checked
// by C-51), and the one with the longest KEL is taken, the higher key set
// among equals. Two entries that verify and whose KELs disagree on an event
// are a fork: the agent is unverifiable. No entry that verifies is an
// error too. A card that does not verify is withheld (CardErr) without
// costing the entry its key set: the card is what a reader is shown, the
// key history and key set are what a message is sealed to, and a card no
// one can use must not make an agent unreachable.
func (d *Daemon) selectDirEntry(aid string, opened []*dirbox.DirEntry, now uint64) (peerMaterial, error) {
	var ok []peerMaterial
	var firstErr error
	for _, e := range opened {
		m, err := d.verifyDirEntry(aid, e, now)
		if err != nil {
			d.count(cntDirEntrySkipped)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok = append(ok, m)
	}
	if len(ok) == 0 {
		return peerMaterial{}, fmt.Errorf("no entry of the directory verifies: %w", firstErr)
	}
	best := 0
	for i := range ok {
		a, b := ok[i], ok[best]
		if len(a.KEL) > len(b.KEL) || len(a.KEL) == len(b.KEL) && a.Keys.set.Seq > b.Keys.set.Seq {
			best = i
		}
	}
	for i := range ok {
		if err := identity.ExtendsKEL(ok[i].KEL, ok[best].KEL); err != nil {
			d.count(dropKELFork)
			return peerMaterial{}, fmt.Errorf("two entries of the directory disagree on the KEL: %w", identity.ErrKELFork)
		}
		if ok[i].Keys.set.Seq == ok[best].Keys.set.Seq &&
			!bytes.Equal(mustSetBytes(ok[i].Keys.signed), mustSetBytes(ok[best].Keys.signed)) {
			d.count(keysFork)
			return peerMaterial{}, fmt.Errorf("two entries of the directory hold different key sets at seq %d: %w",
				ok[i].Keys.set.Seq, identity.ErrKELFork)
		}
	}
	return ok[best], nil
}

// verifyDirEntry verifies one opened entry on its own (SSR-W22).
func (d *Daemon) verifyDirEntry(aid string, e *dirbox.DirEntry, now uint64) (peerMaterial, error) {
	kel, err := seal.ParseKEL(e.KEL)
	if err != nil {
		return peerMaterial{}, err
	}
	if got, err := replayedAID(kel); err != nil || got != aid {
		return peerMaterial{}, fmt.Errorf("the entry's KEL does not replay to %s", aid)
	}
	signed, err := seal.UnmarshalSignedEncKeySet(e.Keys)
	if err != nil {
		return peerMaterial{}, err
	}
	set, err := seal.VerifyEncKeySet(signed, aid, kel, now)
	if err != nil {
		return peerMaterial{}, err
	}
	m := peerMaterial{AID: aid, KEL: kel, Keys: &peerKeySet{signed: e.Keys, set: set}, Epoch: set.Mbox.Epoch}
	if identity.ValidAID(e.Home) {
		m.Home = e.Home
	}
	if len(e.Card) > 0 {
		switch v, err := a2acard.Verify(e.Card, singleKEL(aid, kel), now); {
		case err != nil:
			m.CardErr = verificationReason(err)
		case v.AID != aid:
			m.CardErr = fmt.Sprintf("the card is signed by %s, not %s", v.AID, aid)
		default:
			m.Card = append(json.RawMessage(nil), e.Card...)
		}
	}
	if len(e.Notice) > 0 {
		m.Notice = append([]byte(nil), e.Notice...)
		m.NoticeOK = identity.VerifyNotice(e.Notice, kel, d.hubKELs, now) == nil
	}
	return m, nil
}

// singleKEL is a card resolver that knows one AID's KEL.
func singleKEL(aid string, kel []identity.SignedEvent) a2acard.Resolver {
	return func(signer string) ([]identity.SignedEvent, error) {
		if signer != aid {
			return nil, fmt.Errorf("no key history for %s here", signer)
		}
		return kel, nil
	}
}

// ---- the bucket query (SSR-D15, SSR-D15a)

// bucketOf returns the bucket of h at hub: from the cache unless bypass
// (cached is then true), else asked, every page. A query is as wide as
// DirQueryBits says and no wider; a hub that refuses it as too narrow is
// asked once more at the width it gave.
func (d *Daemon) bucketOf(ctx context.Context, hub string, h []byte, bypass bool) (a *bucketAnswer, cached bool, err error) {
	for attempt := 0; ; attempt++ {
		maxBits, err := d.dirMaxBitsOf(ctx, hub)
		if err != nil {
			return nil, false, err
		}
		bits := hubapi.DirQueryBits(maxBits, d.peers.cutOf(hub))
		prefix, err := dirbox.Prefix(h, bits)
		if err != nil {
			return nil, false, err
		}
		key := bucketKey(hub, bits, prefix)
		now := d.nowMS()
		if !bypass {
			if a, ok := d.peers.cachedBucket(key, now); ok {
				return a, true, nil
			}
		}
		a, err := d.peers.bucketFlights.do(key, func() (*bucketAnswer, error) { return d.fetchBucket(ctx, hub, prefix, bits) })
		var narrow *bucketTooNarrowError
		if errors.As(err, &narrow) && attempt == 0 {
			d.count(cntDirBucketNarrow)
			d.peers.noteDirMax(hub, narrow.maxBits, now)
			continue
		}
		if err != nil {
			return nil, false, err
		}
		return a, false, nil
	}
}

// bucketKey is the cache key of the bucket (prefix, bits) at hub.
func bucketKey(hub string, bits int, prefix []byte) string {
	return hub + "\x00" + strconv.Itoa(bits) + "\x00" + hex.EncodeToString(prefix)
}

// bucketTooNarrowError is a hub's 400 bucket_too_narrow, with the
// max_bits it gave.
type bucketTooNarrowError struct{ maxBits int }

func (e *bucketTooNarrowError) Error() string {
	return fmt.Sprintf("anet: the hub answers directory queries of at most %d bits", e.maxBits)
}

// fetchBucket asks the hub for the bucket (prefix, bits), page by page, on
// the anonymous side; caches a whole answer; and applies the small-bucket
// self check (SSR-D15a): a bucket narrower than the whole directory that,
// all pages together, holds fewer than 32 entries and is not partial makes
// every later query to this hub one bit wider.
func (d *Daemon) fetchBucket(ctx context.Context, hub string, prefix []byte, bits int) (*bucketAnswer, error) {
	a := &bucketAnswer{bits: bits, prefix: prefix}
	cursor := ""
	for page := 0; ; page++ {
		if page == hubapi.DirQueryMaxPages {
			return nil, fmt.Errorf("anet: the directory bucket is longer than %d pages", hubapi.DirQueryMaxPages)
		}
		body, err := json.Marshal(hubapi.DirBucketRequest{Prefix: hubapi.B64(prefix), Bits: bits, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, hub+hubapi.DirectoryBucketPath, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		rep := d.hubExchange(req, hub, hubapi.DirectoryBucketPath, -1)
		if rep.Status == 0 {
			if rep.Local {
				return nil, rep.Err
			}
			return nil, fmt.Errorf("anet: hub %s: %w", hubapi.DirectoryBucketPath, rep.Err)
		}
		if werr := hubWireCheck(hub, rep); werr != nil {
			return nil, werr
		}
		if rep.Status != http.StatusOK {
			herr := newHubError(hubapi.DirectoryBucketPath, rep)
			if herr.reason == "bucket_too_narrow" {
				var extra struct {
					MaxBits *int `json:"max_bits"`
				}
				if json.Unmarshal(rep.Body, &extra) == nil && extra.MaxBits != nil && *extra.MaxBits >= 0 && *extra.MaxBits < bits {
					return nil, &bucketTooNarrowError{maxBits: *extra.MaxBits}
				}
			}
			return nil, herr
		}
		p, err := hubapi.DecodeBucketPage(rep.Body, prefix, bits)
		if err != nil {
			return nil, err
		}
		for range p.Skipped {
			d.count(cntDirBucketBadEntry)
		}
		d.peers.noteDirMax(hub, p.MaxBits, d.nowMS())
		a.entries = append(a.entries, p.Entries...)
		a.partial = a.partial || p.Partial
		if p.Next == "" {
			break
		}
		if p.Next == cursor {
			return nil, errors.New("anet: the hub's bucket answer does not move past its cursor")
		}
		cursor = p.Next
	}
	now := d.nowMS()
	a.at = now
	for _, e := range a.entries {
		a.size += len(e.H) + len(e.Box)
	}
	if bits > 0 && !a.partial && len(a.entries) < hubapi.DirMinBucket {
		d.count(cntDirBucketSmall)
		d.peers.noteSmallBucket(hub, now)
	}
	if !a.partial {
		d.peers.cacheBucket(bucketKey(hub, bits, prefix), a)
	}
	return a, nil
}

// dirMaxBitsOf is the hub's dir_max_bits: as a bucket answer or GET
// /hub/identity said it within the hour, else asked of GET /hub/identity
// (on the anonymous side). A hub that does not say is taken as 0: the whole
// directory, which tells it nothing.
func (d *Daemon) dirMaxBitsOf(ctx context.Context, hub string) (int, error) {
	now := d.nowMS()
	if b, ok := d.peers.dirMaxOf(hub, now); ok {
		return b, nil
	}
	var out hubapi.HubIdentity
	if err := d.hubGet(ctx, hub, "/hub/identity", nil, &out); err != nil {
		return 0, err
	}
	b := 0
	if out.DirMaxBits != nil && *out.DirMaxBits > 0 {
		b = *out.DirMaxBits
	}
	d.peers.noteDirMax(hub, b, now)
	return b, nil
}

// ---- the refreshes (SSR-D18)

// peerReviewOf maps a verdict to what a refusal review is (caps.go).
func peerReviewOf(m peerMaterial, v peerVerdict) peerReview {
	switch v {
	case verdictFound:
		return peerReview{Verdict: reviewFound, Epoch: m.Epoch}
	case verdictNotFound:
		return peerReview{Verdict: reviewNotFound}
	case verdictUnverifiable:
		return peerReview{Verdict: reviewUnverifiable}
	}
	return peerReview{Verdict: reviewUnavailable}
}

// reviewPeer is SSR-D11 item 1: after a 403 access_invalid the peer's key
// set is fetched again through a bucket, bypassing every cache, at most
// once a minute per peer; a refusal within the minute takes the same
// review. A higher mailbox epoch it finds is written to the peer's record,
// which voids the capabilities of the older epoch in that write
// (SSR-D19). The review fills onAccessRefused's peerReview.
func (d *Daemon) reviewPeer(ctx context.Context, aid string) peerReview {
	now := d.nowMS()
	if r, ok := d.peers.reviewOf(aid, now); ok {
		return r
	}
	return d.peers.review(aid, now, func() peerReview {
		return peerReviewOf(d.resolvePeer(ctx, aid, resolveOpts{need: needKeySet, bypass: true, record: true}))
	})
}

// refreshForKidStale is engine ENG-R11's fetch: the hub said kid_stale, so
// the recipient's key set is fetched again through a bucket, bypassing
// every cache, verified and recorded; the caller seals again with what it
// returns.
func (d *Daemon) refreshForKidStale(ctx context.Context, aid string) (*peerKeySet, error) {
	m, v := d.resolvePeer(ctx, aid, resolveOpts{need: needKeys, bypass: true, record: true})
	if v != verdictFound {
		return nil, &peerResolveError{AID: aid, Verdict: v, Cause: m.Err}
	}
	return m.Keys, nil
}

// refreshOnUnknown is SSR-D12: the hub answered 404 recipient_unknown to a
// first contact of the queued message rowID to `to`. When the recipient's
// material was not fetched through a bucket in the last hour, and this
// message did not refresh it in the last hour either, it is fetched again,
// bypassing every cache; true when that changed the home hint the message
// goes with (SSR-D13), and the same envelope is to be sent once more at
// once. capHome is the home of a capability `to` gave (the hint's second
// source), which the refresh does not change.
func (d *Daemon) refreshOnUnknown(ctx context.Context, rowID int64, to, capHome string) bool {
	now := d.nowMS()
	if !d.peers.mayRefreshUnknown(rowID, to, now) {
		return false
	}
	before := d.kindZeroHome(to, capHome)
	d.resolvePeer(ctx, to, resolveOpts{need: needKEL, bypass: true, record: true})
	after := d.kindZeroHome(to, capHome)
	return after != "" && after != before
}

// recheckActiveContact is SSR-D18's daily recheck: when this node writes to
// an active contact — a peer on its allow or trust list, or one it started
// an interaction with — whose key set was last fetched more than a day ago,
// the peer is looked up in the directory again, in the background, at most
// once a day. What it finds is recorded by the usual rules; what it does
// not find changes nothing.
func (d *Daemon) recheckActiveContact(row *interactions.PeerIdentity, pin string, now uint64) {
	switch reason := row.PinnedReason; {
	case reason == interactions.PinAllow, reason == interactions.PinTrust, reason == interactions.PinOutbound,
		pin == interactions.PinOutbound:
	default:
		return
	}
	if row.KeysCheckedAt > 0 && now >= uint64(row.KeysCheckedAt) && now-uint64(row.KeysCheckedAt) < activeRecheckMS {
		return
	}
	aid := row.AID
	if !d.peers.mayRecheck(aid, now) {
		return
	}
	d.goBackground(func() {
		ctx, cancel := context.WithTimeout(d.ctx, resolveTimeout)
		defer cancel()
		// v030-todo(KEL-11): KEL D-26 item 4 — material without a fresh
		// notice is not written by this refresh.
		if _, v := d.resolvePeer(ctx, aid, resolveOpts{need: needKeySet, fresh: true, record: true}); v != verdictFound {
			d.logOnce("recheck:"+aid, "anet: rechecking %s in the directory: %s (keeping the stored record)", aid, v)
		}
	})
}

// discover keeps what a listing of the registry returned for aid, verified
// here, as SSR-D15's second source.
func (d *Daemon) discover(m peerMaterial) {
	m.Source = sourceDiscovery
	m.At = d.nowMS()
	d.peers.putDiscovered(m)
}

// peerDirHome is the home hub aid's directory entry named, when this
// process read one (SSR-D13's third source).
func (d *Daemon) peerDirHome(aid string) string {
	d.peers.mu.Lock()
	defer d.peers.mu.Unlock()
	return d.peers.homes[aid]
}

// smallBuckets24h is how many small buckets this process met in the 24
// hours up to now (SSR-U2 small_buckets_24h; cleared by a restart).
func (d *Daemon) smallBuckets24h(now uint64) int {
	d.peers.mu.Lock()
	defer d.peers.mu.Unlock()
	d.peers.trimSmallLocked(now)
	return len(d.peers.small)
}

// ---- the resolver's state

func (r *peerResolver) discovered(aid string, now uint64) (peerMaterial, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.disc[aid]
	if !ok || now < m.At || now-m.At >= discoveryTTLMS {
		return peerMaterial{}, false
	}
	return *m, true
}

func (r *peerResolver) putDiscovered(m peerMaterial) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disc == nil {
		r.disc = map[string]*peerMaterial{}
	}
	if _, ok := r.disc[m.AID]; !ok {
		r.discOrder = append(r.discOrder, m.AID)
		for len(r.discOrder) > discoveryMax {
			delete(r.disc, r.discOrder[0])
			r.discOrder = r.discOrder[1:]
		}
	}
	r.disc[m.AID] = &m
}

func (r *peerResolver) cachedBucket(key string, now uint64) (*bucketAnswer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.buckets[key]
	if !ok {
		return nil, false
	}
	if now < a.at || now-a.at >= bucketTTLMS {
		delete(r.buckets, key)
		r.bucketBytes -= a.size
		return nil, false
	}
	return a, true
}

// cacheBucket keeps a whole answer, dropping the oldest ones past the byte
// budget; an answer larger than the budget is not kept.
func (r *peerResolver) cacheBucket(key string, a *bucketAnswer) {
	if a.size > bucketCacheBytes {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.buckets == nil {
		r.buckets = map[string]*bucketAnswer{}
	}
	if old, ok := r.buckets[key]; ok {
		r.bucketBytes -= old.size
	}
	r.buckets[key] = a
	r.bucketBytes += a.size
	for r.bucketBytes > bucketCacheBytes {
		oldest := ""
		for k, b := range r.buckets {
			if oldest == "" || b.at < r.buckets[oldest].at {
				oldest = k
			}
		}
		r.bucketBytes -= r.buckets[oldest].size
		delete(r.buckets, oldest)
	}
}

func (r *peerResolver) dirMaxOf(hub string, now uint64) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.dirMax[hub]
	if !ok || now < s.at || now-s.at >= dirMaxBitsTTLMS {
		return 0, false
	}
	return s.bits, true
}

func (r *peerResolver) noteDirMax(hub string, bits int, now uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dirMax == nil {
		r.dirMax = map[string]dirMaxBits{}
	}
	r.dirMax[hub] = dirMaxBits{bits: max(bits, 0), at: now}
}

func (r *peerResolver) cutOf(hub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cut[hub]
}

// noteSmallBucket widens every later query to hub by one bit, down to the
// whole directory, and remembers the time for small_buckets_24h.
func (r *peerResolver) noteSmallBucket(hub string, now uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cut == nil {
		r.cut = map[string]int{}
	}
	if r.cut[hub] < hubapi.DirQueryMaxBits {
		r.cut[hub]++
	}
	r.small = append(r.small, now)
	r.trimSmallLocked(now)
}

func (r *peerResolver) trimSmallLocked(now uint64) {
	i := 0
	for i < len(r.small) && now >= r.small[i] && now-r.small[i] >= smallBucketsWindowMS {
		i++
	}
	r.small = r.small[i:]
}

func (r *peerResolver) noteRefreshed(aid string, now uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refreshed == nil {
		r.refreshed = map[string]uint64{}
	}
	r.refreshed[aid] = now
}

func (r *peerResolver) noteHome(aid, home string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.homes == nil {
		r.homes = map[string]string{}
	}
	r.homes[aid] = home
}

func (r *peerResolver) reviewOf(aid string, now uint64) (peerReview, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.reviews[aid]
	if !ok || now < m.at || now-m.at >= reviewEveryMS {
		return peerReview{}, false
	}
	return m.review, true
}

// review runs fetch for aid unless a review of aid is in progress or was
// made within the minute, whose review it then takes, and keeps it for the
// minute.
func (r *peerResolver) review(aid string, now uint64, fetch func() peerReview) peerReview {
	rv, _ := r.reviewFlights.do(aid, func() (peerReview, error) {
		if rv, ok := r.reviewOf(aid, now); ok {
			return rv, nil
		}
		rv := fetch()
		r.mu.Lock()
		if r.reviews == nil {
			r.reviews = map[string]peerReviewMemo{}
		}
		r.reviews[aid] = peerReviewMemo{review: rv, at: now}
		r.mu.Unlock()
		return rv, nil
	})
	return rv
}

// mayRefreshUnknown reports whether a 404 to the queued message rowID may
// refresh `to` now (SSR-D12), and if so notes it.
func (r *peerResolver) mayRefreshUnknown(rowID int64, to string, now uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.refreshed[to]; ok && now >= t && now-t < unknownRefreshMS {
		return false
	}
	if t, ok := r.unknownRows[rowID]; ok && now >= t && now-t < unknownRefreshMS {
		return false
	}
	if r.unknownRows == nil {
		r.unknownRows = map[int64]uint64{}
	}
	if len(r.unknownRows) >= discoveryMax {
		clear(r.unknownRows)
	}
	r.unknownRows[rowID] = now
	return true
}

// mayRecheck reports whether the active contact aid may be rechecked now,
// and if so notes it.
func (r *peerResolver) mayRecheck(aid string, now uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.rechecks[aid]; ok && now >= t && now-t < activeRecheckMS {
		return false
	}
	if r.rechecks == nil {
		r.rechecks = map[string]uint64{}
	}
	if len(r.rechecks) >= discoveryMax {
		clear(r.rechecks)
	}
	r.rechecks[aid] = now
	return true
}

// ---- anet verify (SSR-D16)

// hAgentsKEL answers POST /agents/kel {aid}: aid's key history for `anet
// verify`, as standard base64 (the control plane's encoding), and where it
// came from — this node itself, its record of the peer (a hub it pinned
// included), its hub's own identity, or the directory (resolvePeer, which
// names aid to no hub). An agent the directory does not hold is 404.
func (d *Daemon) hAgentsKEL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AID string `json:"aid"`
	}
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	if !validAgentID(req.AID) {
		badTaskRequest(w, "%q is not an agent id", req.AID)
		return
	}
	answer := func(kel []identity.SignedEvent, source string) {
		raw, err := identity.MarshalKEL(kel)
		if err != nil {
			writeTaskError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"aid": req.AID, "kel": base64.StdEncoding.EncodeToString(raw), "source": source})
	}
	if req.AID == d.AID() {
		answer(d.self.KEL(), "self")
		return
	}
	if kel, ok := d.peerKEL(req.AID); ok {
		answer(kel, "record")
		return
	}
	if hub := d.config().HubURL; hub != "" {
		if hubAID, kel, err := d.hubIdentity(r.Context(), hub); err == nil && hubAID == req.AID {
			answer(kel, "hub")
			return
		}
	}
	m, v := d.resolvePeer(r.Context(), req.AID, resolveOpts{need: needKEL})
	switch v {
	case verdictFound:
		answer(m.KEL, "directory")
	case verdictNotFound:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": (&peerResolveError{AID: req.AID, Verdict: v}).Error()})
	default:
		writeTaskError(w, a2ashape.Errorf(a2ashape.ErrUnavailable, "%v", &peerResolveError{AID: req.AID, Verdict: v, Cause: m.Err}))
	}
}

// DirectoryKEL is `anet verify` with no daemon running (SSR-D16): aid's key
// history from a directory bucket of the hub at hubURL — or the hub's own,
// when aid is the hub — asked on a connection of its own that carries
// nothing of this node, as wide as DirQueryBits(dir_max_bits, 0) says and
// no wider. The entries filed under aid's directory hash are opened, each
// KEL must replay to aid, and the longest is returned; two that disagree on
// an event are an error.
func DirectoryKEL(ctx context.Context, hubURL, aid string) ([]identity.SignedEvent, error) {
	if !identity.ValidAID(aid) {
		return nil, fmt.Errorf("%q is not an agent id", aid)
	}
	hub := strings.TrimRight(strings.TrimSpace(hubURL), "/")
	c := newHubHTTPClient(hubapi.SideAnonymous, nil)
	exchange := func(method, path string, body []byte) (int, []byte, error) {
		var rd io.Reader = http.NoBody
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, hub+path, rd)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set(hubapi.WireVersionHeader, strconv.Itoa(hubapi.WireVersion))
		req.Header.Set("User-Agent", hubUserAgent)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxHubResponse))
		return resp.StatusCode, b, err
	}
	code, body, err := exchange(http.MethodGet, "/hub/identity", nil)
	if err != nil {
		return nil, fmt.Errorf("ask %s for its identity: %w", hub, err)
	}
	var id hubapi.HubIdentity
	if code != http.StatusOK || json.Unmarshal(body, &id) != nil || id.AID == "" {
		return nil, fmt.Errorf("%s did not answer as a hub (%d)", hub, code)
	}
	if id.AID == aid {
		raw, err := hubapi.DecodeB64(id.KEL, -1)
		if err != nil {
			return nil, fmt.Errorf("%s served an unreadable identity", hub)
		}
		kel, err := seal.ParseKEL(raw)
		if err != nil {
			return nil, err
		}
		if got, err := replayedAID(kel); err != nil || got != aid {
			return nil, fmt.Errorf("%s's own key history does not replay to it", hub)
		}
		return kel, nil
	}
	maxBits := 0
	if id.DirMaxBits != nil {
		maxBits = *id.DirMaxBits
	}
	bits := hubapi.DirQueryBits(maxBits, 0)
	q, h, err := hubapi.BucketQuery(aid, bits)
	if err != nil {
		return nil, err
	}
	prefix, _ := hubapi.DecodeB64(q.Prefix, -1)
	var entries []hubapi.BucketEntry
	partial := false
	for page := 0; ; page++ {
		if page == hubapi.DirQueryMaxPages {
			return nil, fmt.Errorf("the directory bucket at %s is longer than %d pages", hub, hubapi.DirQueryMaxPages)
		}
		b, err := json.Marshal(q)
		if err != nil {
			return nil, err
		}
		code, body, err := exchange(http.MethodPost, hubapi.DirectoryBucketPath, b)
		if err != nil {
			return nil, fmt.Errorf("ask %s's directory: %w", hub, err)
		}
		if code != http.StatusOK {
			reason, _ := hubapi.ParseErrorBody(body)
			return nil, fmt.Errorf("%s's directory refused the query (%d %s)", hub, code, reason)
		}
		p, err := hubapi.DecodeBucketPage(body, prefix, bits)
		if err != nil {
			return nil, err
		}
		entries = append(entries, p.Entries...)
		partial = partial || p.Partial
		if p.Next == "" || p.Next == q.Cursor {
			break
		}
		q.Cursor = p.Next
	}
	opened, _ := hubapi.OpenEntries(aid, h, entries)
	var best []identity.SignedEvent
	var kels [][]identity.SignedEvent
	for _, e := range opened {
		kel, err := seal.ParseKEL(e.KEL)
		if err != nil {
			continue
		}
		if got, err := replayedAID(kel); err != nil || got != aid {
			continue
		}
		kels = append(kels, kel)
		if len(kel) > len(best) {
			best = kel
		}
	}
	if best == nil {
		if partial {
			return nil, fmt.Errorf("%s's directory could not ask every peer hub; try again later", hub)
		}
		return nil, fmt.Errorf("%s's directory has no entry for %s", hub, aid)
	}
	for _, k := range kels {
		if err := identity.ExtendsKEL(k, best); err != nil {
			return nil, fmt.Errorf("%s's directory holds key histories of %s that disagree", hub, aid)
		}
	}
	return best, nil
}
