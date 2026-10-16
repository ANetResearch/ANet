//go:build !no_x402

package x402

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/liabtree"
	"github.com/ANetResearch/ANetCore/relayauth"
)

// Checking what the hub says about the money (HPP §4.3.5).
//
// Two things a node can do without asking anyone's permission:
//
//	Chain   fetch the hub's supply epochs, verify each signature under the
//	        hub's key history as it stood when the record was signed, the
//	        links between them, and the heads this node recorded before. A
//	        hub that rewrote its supply history contradicts a record this
//	        node already holds.
//	Proof   for every epoch, ask the hub for this account's leaf of the
//	        liability tree, and check that the path reaches the root the
//	        hub signed, that the sum at the root is what it signed as
//	        outstanding, that the balance continues the previous epoch's by
//	        exactly the ledger entries between them, and that the ledger
//	        agrees with what this node signed and was paid.
//
// Neither prevents anything. A hub is the custodian of its ledger and can
// issue credit at will — that is what being the issuer means. What these
// establish is that it cannot do so retroactively or silently, and that it
// cannot show one account a balance it did not sign to everybody, provided
// somebody looked. The check runs by itself every six hours and when asked
// (anet audit-hub).

// EvIssuanceHeadSeen records a hub supply-epoch head this node observed.
//
// On this node's own chain, which is what makes it evidence rather than a
// cache: the hub cannot alter it, and it is signed and ordered alongside
// everything else this node did. Payload {hub, seq, id}.
const EvIssuanceHeadSeen = "anet.issuance.head_seen"

// EvSupplyProof records one epoch's balance-proof check: {to_day, root,
// balance, result}.
const EvSupplyProof = "anet.supply.proof"

const (
	dayMillis = 86_400_000
	// proofKeptFor is how long the hub keeps an epoch's leaves, counted from
	// the epoch record's publication (HPP §2.3).
	proofKeptFor = 14 * dayMillis
	// epochTimeSlack is the clock difference tolerated between the hub's
	// record time and the end of the day it covers, or this node's clock.
	epochTimeSlack = 10 * 60 * 1000
	// auditEvery is the pace of the automatic check (PAY-S9).
	auditEvery = 6 * time.Hour
	// maxEpochs bounds what one audit reads of the chain.
	maxEpochs = 100_000
	maxPages  = 2_000
)

// Levels of a finding.
const (
	levelFail = "fail"
	levelWarn = "warn"
)

// Finding is one thing an audit found. Code is the key of a sentence of the
// reason table (ux §1.4 K) and Args its values: a time is "time", unix
// milliseconds. Detail is for the JSON only, in English.
type Finding struct {
	Code   string         `json:"code"`
	Level  string         `json:"level"`
	Args   map[string]any `json:"args,omitempty"`
	Detail string         `json:"detail,omitempty"`
}

// AuditReport is what an audit found.
type AuditReport struct {
	Hub    string `json:"hub"`
	HubAID string `json:"hub_aid"`
	// Epochs is how many supply epochs the hub has published; HeadSeq and
	// HeadID are the last of them.
	Epochs  int     `json:"epochs"`
	HeadSeq *uint64 `json:"head_seq,omitempty"`
	HeadID  string  `json:"head_id,omitempty"`
	// ChainOK: every record verifies, links and matches what this node
	// recorded of the chain before.
	ChainOK bool `json:"chain_ok"`
	// ProofsOK is how many epochs of this node's proof chain stand checked,
	// and Latest the last of them.
	ProofsOK int        `json:"proofs_ok"`
	Latest   *LiabProof `json:"latest,omitempty"`
	// Checked is how many epochs this run checked.
	Checked  int       `json:"checked"`
	Findings []Finding `json:"findings"`
	// Notes are hints that are not findings: continuity_unverifiable.
	Notes []string `json:"notes,omitempty"`
	// Verified is true when nothing failed.
	Verified bool `json:"verified"`
}

func (r *AuditReport) add(f Finding) { r.Findings = append(r.Findings, f) }

func chainFinding(detail string) Finding {
	return Finding{Code: "proof_invalid", Level: levelFail, Args: map[string]any{"code": "chain"}, Detail: detail}
}

// epochView is one supply epoch of the public chain, with its record
// decoded and verified.
type epochView struct {
	Seq         uint64 `json:"seq"`
	ID          string `json:"id"`
	PrevID      string `json:"prev_id"`
	FromDay     uint64 `json:"from_day"`
	ToDay       uint64 `json:"to_day"`
	Root        string `json:"root"`
	Outstanding uint64 `json:"outstanding"`
	Record      string `json:"record"`

	raw []byte
	rec *ael.EventRecord
	ep  *ael.SupplyEpoch
}

// closeMS is the instant the epoch's last day ends.
func (e *epochView) closeMS() int64 { return int64(e.ToDay+1) * dayMillis }

// fetchChain reads the hub's supply epochs from the first, a page at a time.
func (m *Module) fetchChain(ctx context.Context) ([]epochView, error) {
	var out []epochView
	var from uint64
	for page := 0; page < maxPages && len(out) < maxEpochs; page++ {
		var resp struct {
			Entries []epochView `json:"entries"`
		}
		if err := m.getJSON(ctx, "/x402/issuance?from="+strconv.FormatUint(from, 10), &resp); err != nil {
			return out, err
		}
		if len(resp.Entries) == 0 {
			break
		}
		out = append(out, resp.Entries...)
		next := resp.Entries[len(resp.Entries)-1].Seq + 1
		if next <= from {
			break // a hub that does not advance is not followed
		}
		from = next
	}
	return out, nil
}

// verifyChain decodes and verifies every epoch against the hub's key
// history (which verifies each signature as of the record's own time, CORE-L8),
// and returns the findings. Records that verify are left in e.rec and e.ep;
// sound says every record did.
func (m *Module) verifyChain(chain []epochView, hubAID string) (problems []Finding, sound bool) {
	kel, err := m.hubKEL()
	if err != nil {
		return []Finding{chainFinding(err.Error())}, false
	}
	sound = true
	refreshed := false
	var recs []*ael.EventRecord
	bad := func(format string, a ...any) {
		sound = false
		if len(problems) < 8 {
			problems = append(problems, chainFinding(fmt.Sprintf(format, a...)))
		}
	}
	for i := range chain {
		e := &chain[i]
		raw, err := base64.RawURLEncoding.Strict().DecodeString(e.Record)
		if err != nil {
			bad("seq %d: the record is not base64url", e.Seq)
			continue
		}
		rec, err := ael.UnmarshalEventRecord(raw)
		if err != nil {
			bad("seq %d: the record does not decode: %v", e.Seq, err)
			continue
		}
		err = rec.Verify(kel)
		if err != nil && !refreshed {
			// A key state the pinned history does not reach: the hub rotated
			// since it was pinned. Read it again once, and check again.
			refreshed = true
			if m.refreshHubKEL() {
				if k, kerr := m.hubKEL(); kerr == nil {
					kel = k
					err = rec.Verify(kel)
				}
			}
		}
		if err != nil {
			bad("seq %d does not verify under the hub's key history: %v", e.Seq, err)
			continue
		}
		// Verify authenticates the signer; it says nothing about which chain
		// the record belongs to.
		if rec.ChainDID != hubAID || rec.SignerAID != hubAID {
			bad("seq %d is a record of another chain or signer, not the hub's own", e.Seq)
			continue
		}
		ep, err := ael.ParseSupplyEpoch(rec)
		if err != nil {
			bad("seq %d is not a supply epoch: %v", e.Seq, err)
			continue
		}
		// The columns beside the record are rendered by the hub and are not
		// what it signed: a hub whose summary disagrees with its records is
		// what an audit should show rather than adopt.
		if e.Seq != rec.Seq || e.ID != rec.ID || e.PrevID != rec.PrevID || e.ToDay != ep.ToDay ||
			e.FromDay != ep.FromDay || e.Root != hex.EncodeToString(ep.Root[:]) || e.Outstanding != ep.Outstanding {
			bad("seq %d: the summary the hub serves beside the record is not what the record says", e.Seq)
			continue
		}
		e.raw, e.rec, e.ep = raw, rec, ep
		recs = append(recs, rec)
	}
	// Linking is checked only over records that all decoded and verified: a
	// gap reported across an undecodable record would name the hub for
	// something that could be damage in transit.
	if sound {
		for _, p := range checkChainLinks(recs) {
			if len(problems) < 8 {
				problems = append(problems, chainFinding(p))
			}
			sound = false
		}
		// The hub writes each record's time, and the 14-day rule below trusts
		// it: a time that precedes the end of the day the epoch covers, runs
		// backwards along the chain or lies in the future would let a hub age
		// a new epoch out of the audit.
		now := time.Now().UnixMilli()
		for i := 0; i < len(chain) && sound; i++ {
			ts, why := chain[i].rec.Timestamp, ""
			switch {
			case ts < chain[i].closeMS()-epochTimeSlack:
				why = "is older than the end of the last day it covers"
			case i > 0 && ts < chain[i-1].rec.Timestamp:
				why = "is older than the epoch before it"
			case ts > now+epochTimeSlack:
				why = "is in the future"
			}
			if why != "" {
				problems = append(problems, chainFinding(fmt.Sprintf(
					"seq %d: the time the hub wrote %s", chain[i].Seq, why)))
				sound = false
			}
		}
		// One epoch per day, in order: a day covered twice, or backwards, is
		// not a supply history.
		for i := 1; i < len(chain) && sound; i++ {
			if chain[i].ep.FromDay <= chain[i-1].ep.ToDay {
				problems = append(problems, chainFinding(fmt.Sprintf(
					"seq %d covers days from %d, which the epoch before it already covered up to %d",
					chain[i].Seq, chain[i].ep.FromDay, chain[i-1].ep.ToDay)))
				sound = false
			}
		}
	}
	return problems, sound
}

// priorHead is a head this node recorded earlier.
type priorHead struct {
	Seq    uint64
	HeadID string
}

// priorHeads reads back what this node has recorded of this hub's chain.
func (m *Module) priorHeads(hubAID string) []priorHead {
	if m.seam == nil {
		return nil
	}
	var out []priorHead
	for _, p := range m.seam.ReadEvidence(EvIssuanceHeadSeen, 5000) {
		if h, _ := p["hub"].(string); h != hubAID {
			continue
		}
		id, _ := p["id"].(string)
		if id == "" {
			continue
		}
		// A head whose seq cannot be read is skipped rather than compared
		// as seq 0, which would report a rewrite of the first epoch.
		seq, err := asInt64(p["seq"])
		if err != nil || seq < 0 {
			continue
		}
		out = append(out, priorHead{Seq: uint64(seq), HeadID: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// AuditIssuance and Reconcile satisfy module.Payer. They return `any` so
// the kernel does not have to know the report shapes: it carries them to
// a caller and never reads them, and a typed contract would put this
// module's vocabulary in the kernel for no gain.
func (m *Module) AuditIssuance(ctx context.Context) (any, error) { return m.audit(ctx) }
func (m *Module) Reconcile(ctx context.Context) (any, error)     { return m.reconcile(ctx) }

// getJSON fetches from this node's hub, anonymously.
func (m *Module) getJSON(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.hubURL()+path, nil)
	if err != nil {
		return err
	}
	resp, err := m.hubDo(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("x402: hub answered %s for %s", resp.Status, path)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(into)
}

// ledgerView is the account's ledger window as the hub serves it.
type ledgerView struct {
	Entries    []ledgerEntry `json:"entries"`
	Total      int           `json:"total"`
	Sum        int64         `json:"sum"`
	Truncated  bool          `json:"truncated"`
	WindowDays int           `json:"window_days"`
	OldestSeq  uint64        `json:"oldest_seq"`
}

type ledgerEntry struct {
	Seq    uint64 `json:"seq"`
	Delta  int64  `json:"delta"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// fetchLedger reads the newest page of this account's ledger window.
func (m *Module) fetchLedger(ctx context.Context) (ledgerView, error) {
	var led ledgerView
	err := m.signedGet(ctx, "/agents/"+url.PathEscape(m.AID())+"/ledger?limit=500", relayauth.ActionLedger, &led)
	return led, err
}

// pageFloor is the seq below which this page of the ledger may be missing
// entries: 0 when the page is the whole window.
func (l ledgerView) pageFloor() uint64 {
	if !l.Truncated || len(l.Entries) == 0 {
		return 0
	}
	return l.Entries[0].Seq
}

// audit checks the hub's chain and this node's balance proofs now (anet
// audit-hub, PAY-S9, PAY-S10). The automatic check calls the same code.
func (m *Module) audit(ctx context.Context) (AuditReport, error) {
	m.auditMu.Lock()
	defer m.auditMu.Unlock()
	rep := AuditReport{Hub: m.hubURL(), HubAID: m.hubAID(), Findings: []Finding{}}
	if rep.Hub == "" {
		return rep, fmt.Errorf("x402: no hub configured")
	}
	if m.seam == nil || rep.HubAID == "" {
		return rep, fmt.Errorf("x402: cannot learn this hub's identity, so its records cannot be checked")
	}
	chain, err := m.fetchChain(ctx)
	if err != nil {
		return rep, err
	}
	rep.Epochs = len(chain)
	problems, sound := m.verifyChain(chain, rep.HubAID)
	for _, p := range problems {
		rep.add(p)
	}
	if n := len(chain); n > 0 {
		seq := chain[n-1].Seq
		rep.HeadSeq, rep.HeadID = &seq, chain[n-1].ID
	}
	// Against what this node saw before: a hub that rewrote its supply
	// history contradicts a record already on this node's chain, which the
	// hub cannot reach. A head that is gone is as much a rewrite.
	byseq := map[uint64]string{}
	for _, e := range chain {
		byseq[e.Seq] = e.ID
	}
	var recorded bool
	for _, prior := range m.priorHeads(rep.HubAID) {
		got, ok := byseq[prior.Seq]
		switch {
		case !ok:
			rep.add(chainFinding(fmt.Sprintf(
				"seq %d was on the hub's chain when this node saw it, and the hub no longer serves it", prior.Seq)))
			sound = false
		case got != prior.HeadID:
			rep.add(chainFinding(fmt.Sprintf(
				"seq %d was %s when this node saw it, and is now %s: the hub rewrote its supply history",
				prior.Seq, short(prior.HeadID), short(got))))
			sound = false
		}
		if ok && got == prior.HeadID && rep.HeadSeq != nil && prior.Seq == *rep.HeadSeq {
			recorded = true
		}
	}
	rep.ChainOK = sound
	if sound && rep.HeadSeq != nil && !recorded {
		// On this node's own chain: that copy is the one the hub cannot
		// reach, and it is the reason any of this is worth doing.
		if err := m.record(EvIssuanceHeadSeen, map[string]any{"hub": rep.HubAID, "seq": *rep.HeadSeq, "id": rep.HeadID}); err != nil {
			return rep, err
		}
	}
	if sound {
		if err := m.checkProofs(ctx, chain, &rep); err != nil {
			m.finishReport(&rep)
			return rep, err
		}
	}
	m.finishReport(&rep)
	return rep, nil
}

// finishReport fills what the stored chain of proofs says.
func (m *Module) finishReport(rep *AuditReport) {
	rows := m.liab.all(rep.HubAID)
	for i := range rows {
		if rows[i].Result == resultOK {
			rep.ProofsOK++
			r := rows[i]
			rep.Latest = &r
		}
	}
	// A failure stored by an earlier run is still a failure until a later
	// epoch checks out again; it is shown when this run found nothing new.
	if len(rep.Findings) == 0 && len(rows) > 0 {
		if last := rows[len(rows)-1]; last.Result != resultOK && last.Result != resultExpired {
			rep.add(findingOfRow(last))
		}
	}
	rep.Verified = true
	for _, f := range rep.Findings {
		if f.Level == levelFail {
			rep.Verified = false
		}
	}
}

// findingOfRow is the finding a stored failing row stands for.
func findingOfRow(r LiabProof) Finding {
	if r.Finding != nil {
		return *r.Finding
	}
	f := Finding{Code: r.Result, Level: levelFail, Args: map[string]any{"time": (int64(r.ToDay+1) * dayMillis)}}
	switch r.Result {
	case "proof_invalid":
		f.Args["code"] = "proof"
	case "balance_mismatch":
		f.Args["amount"] = r.Balance
	}
	f.Detail = "recorded when the epoch to day " + strconv.FormatUint(r.ToDay, 10) + " was checked"
	return f
}

// checkProofs walks the chain's epochs that this node has not checked yet,
// oldest first, and checks this node's proof of each (PAY-S9).
func (m *Module) checkProofs(ctx context.Context, chain []epochView, rep *AuditReport) error {
	now := time.Now().UnixMilli()
	reg, regKnown := m.seam.RegisteredAt()
	rows := m.liab.all(rep.HubAID)
	have := map[uint64]LiabProof{}
	for i := range rows {
		have[rows[i].ToDay] = rows[i]
	}
	prevOf := func(toDay uint64) *LiabProof {
		var p *LiabProof
		for _, r := range m.liab.all(rep.HubAID) {
			if r.ToDay >= toDay {
				break
			}
			if r.Result != resultExpired && r.Result != "proof_invalid" && r.Result != "proof_unavailable" {
				rr := r
				p = &rr
			}
		}
		return p
	}
	var led *ledgerView
	expired, resumed := 0, int64(0)
	for i := range chain {
		e := &chain[i]
		if _, done := have[e.ToDay]; done {
			continue
		}
		if regKnown && e.closeMS() < reg {
			continue // before this node had an account at this hub
		}
		if now-e.rec.Timestamp > proofKeptFor {
			// The hub no longer holds the leaf. Only a node that has checked
			// before can have missed it; for a node new to this check there
			// is nothing missed.
			if len(rows) > 0 || len(have) > 0 {
				expired++
				if err := m.liab.put(LiabProof{Hub: rep.HubAID, ToDay: e.ToDay, FromDay: e.FromDay, Root: e.Root,
					Outstanding: e.Outstanding, Result: resultExpired, CheckedAt: now}); err != nil {
					return err
				}
			}
			continue
		}
		if resumed == 0 {
			resumed = e.closeMS()
		}
		if led == nil {
			l, err := m.fetchLedger(ctx)
			if err != nil {
				return err
			}
			led = &l
		}
		row, finds, notes, err := m.checkEpoch(ctx, e, prevOf(e.ToDay), led, now, reg, regKnown, len(rows) > 0)
		if err != nil {
			return err
		}
		if row == nil {
			continue // not this node's period
		}
		rep.Checked++
		rep.Notes = append(rep.Notes, notes...)
		for _, f := range finds {
			rep.add(f)
		}
		if err := m.liab.put(*row); err != nil {
			return err
		}
		if err := m.record(EvSupplyProof, map[string]any{
			"to_day": row.ToDay, "root": row.Root, "balance": row.Balance, "result": row.Result}); err != nil {
			return err
		}
	}
	if expired > 0 {
		if resumed == 0 {
			resumed = now
		}
		rep.add(Finding{Code: "proof_expired", Level: levelWarn,
			Args:   map[string]any{"n": expired, "time": resumed},
			Detail: "the hub keeps a balance proof for 14 days; this node was not running to check these periods"})
	}
	return nil
}

// checkEpoch checks this node's proof of one epoch. A nil row: the epoch is
// not this node's to check. The findings, in the order PAY-S10 lists them,
// are all reported, and the row records the first.
func (m *Module) checkEpoch(ctx context.Context, e *epochView, prev *LiabProof, led *ledgerView,
	now, reg int64, regKnown, checkedBefore bool) (*LiabProof, []Finding, []string, error) {
	aid := m.AID()
	at := e.closeMS()
	row := &LiabProof{Hub: m.hubAID(), ToDay: e.ToDay, FromDay: e.FromDay, Root: e.Root,
		Outstanding: e.Outstanding, CheckedAt: now}
	fail := func(code string, args map[string]any, detail string) Finding {
		if args == nil {
			args = map[string]any{}
		}
		f := Finding{Code: code, Level: levelFail, Args: args, Detail: detail}
		if row.Result == "" {
			row.Result, row.Finding = code, &f
		}
		return f
	}
	var finds []Finding
	var notes []string

	var pr struct {
		liabtree.WireProof
		FromDay  uint64 `json:"from_day"`
		LedgerHW uint64 `json:"ledger_hw"`
		Record   string `json:"record"`
	}
	path := "/agents/" + url.PathEscape(aid) + "/liability-proof?to_day=" + strconv.FormatUint(e.ToDay, 10)
	status, reason, err := m.signedGetStatus(ctx, path, relayauth.ActionLiabProof, &pr)
	if err != nil {
		return nil, nil, nil, err
	}
	if status != http.StatusOK {
		if status == http.StatusNotFound && reason == "proof_unavailable" {
			// Withheld, or this node had no account at that close. The hub
			// can say so for the second; a node that registered before the
			// close, or has checked an earlier period, was on the books.
			if (regKnown && reg < at) || (!regKnown && checkedBefore) {
				f := fail("proof_unavailable", map[string]any{"time": at},
					"the hub answered proof_unavailable for a period this node had an account in")
				return row, []Finding{f}, nil, nil
			}
			return nil, nil, nil, nil
		}
		return nil, nil, nil, &hubStatusError{path: path, status: status, reason: reason}
	}

	// The proof itself.
	proof, perr := pr.WireProof.Parse()
	switch {
	case perr != nil:
		finds = append(finds, fail("proof_invalid", map[string]any{"code": "malformed", "time": at}, perr.Error()))
	case proof.AID != aid:
		// liabtree.Verify does not compare the AID or the day: a proof for
		// another account, or another period, must not pass as this node's.
		finds = append(finds, fail("proof_invalid", map[string]any{"code": "wrong_account", "time": at},
			"the proof is for another account"))
	case proof.ToDay != e.ToDay || pr.ToDay != e.ToDay:
		finds = append(finds, fail("proof_invalid", map[string]any{"code": "wrong_period", "time": at},
			fmt.Sprintf("asked for the period ending day %d, the proof is for day %d", e.ToDay, pr.ToDay)))
	default:
		root := e.ep.Root
		if verr := liabtree.Verify(proof, root, e.ep.Outstanding, e.ep.Depth); verr != nil {
			code := "path"
			if errors.Is(verr, liabtree.ErrSum) {
				code = "root_sum_mismatch"
			}
			finds = append(finds, fail("proof_invalid", map[string]any{"code": code, "time": at}, verr.Error()))
		}
		// The record the proof carries is the record on the public chain,
		// byte for byte: a hub that shows this node another history than it
		// shows everyone has two.
		if raw, derr := base64.RawURLEncoding.Strict().DecodeString(pr.Record); derr != nil || string(raw) != string(e.raw) {
			finds = append(finds, fail("proof_invalid", map[string]any{"code": "record_mismatch", "time": at},
				"the record that came with the proof is not the one on the public chain"))
		}
	}
	if len(finds) > 0 {
		return row, finds, nil, nil
	}
	row.Balance, row.LedgerHW, row.Record = proof.Balance, pr.LedgerHW, pr.Record
	if proof.Balance > math.MaxInt64 {
		return row, []Finding{fail("proof_invalid", map[string]any{"code": "malformed", "time": at}, "balance over 2^63-1")}, nil, nil
	}

	// Continuity: the balance now is the one at the previous check plus
	// every ledger entry between the two closes.
	if prev != nil {
		switch {
		case prev.LedgerHW > pr.LedgerHW:
			finds = append(finds, fail("balance_mismatch", map[string]any{"time": at, "amount": proof.Balance, "need": prev.Balance},
				"the ledger's high-water mark went backwards between two periods"))
		case !windowCovers(led, now, int64(prev.ToDay+1)*dayMillis, prev.LedgerHW):
			notes = append(notes, "continuity_unverifiable")
		default:
			sum := new(big.Int).SetUint64(prev.Balance)
			for _, en := range led.Entries {
				if en.Seq > prev.LedgerHW && en.Seq <= pr.LedgerHW {
					sum.Add(sum, big.NewInt(en.Delta))
				}
			}
			if sum.Cmp(new(big.Int).SetUint64(proof.Balance)) != 0 {
				need := sum.String()
				finds = append(finds, fail("balance_mismatch", map[string]any{"time": at, "amount": proof.Balance, "need": need},
					fmt.Sprintf("balance %d at the close, %s from the previous period and the ledger", proof.Balance, need)))
			}
		}
	}

	// Against this node's own record. Each entry the hub signed a receipt
	// for must be in the ledger; each debit must be one this node signed.
	lo := int64(0)
	if prev != nil {
		lo = int64(prev.ToDay+1) * dayMillis
	}
	finds = append(finds, m.ledgerAgainstEvidence(led, lo, at, now, fail)...)

	if row.Result == "" {
		row.Result = resultOK
	}
	return row, finds, notes, nil
}

// windowCovers says whether the ledger page holds every entry after seq hw
// that exists now: the page reaches back to it, and the close it belongs to
// is not older than the window.
func windowCovers(led *ledgerView, now, closeMS int64, hw uint64) bool {
	days := led.WindowDays
	if days <= 0 {
		days = 14
	}
	if now-closeMS > int64(days)*dayMillis {
		return false
	}
	floor := led.pageFloor()
	return floor == 0 || floor <= hw
}

// ledgerAgainstEvidence compares the ledger page with the events this node
// holds, for those recorded between lo and hi (unix ms): omitted_from_ledger
// for a settlement or redemption the hub signed a receipt for that the
// ledger lacks, unknown_debit for a debit this node never authorized.
func (m *Module) ledgerAgainstEvidence(led *ledgerView, lo, hi, now int64, fail func(string, map[string]any, string) Finding) []Finding {
	var out []Finding
	reasons := map[string]bool{}
	for _, en := range led.Entries {
		reasons[en.Reason] = true
	}
	// Events older than the page can reach are not judged by it.
	from := lo
	if w := now - 14*dayMillis + 3600_000; from < w {
		from = w
	}
	if floorAt, ok := entryTime(led); ok && led.Truncated && from < floorAt {
		from = floorAt
	}
	authorized := map[string]bool{}
	for _, ev := range m.seam.ReadEvidence(EvPaymentAuthorized, 5000) {
		if id, _ := ev["authorization_id"].(string); id != "" {
			authorized[id] = true
		}
	}
	redeemed := m.seam.ReadEvidence(EvCreditRedeemed, 5000)
	for _, ev := range redeemed {
		if id, _ := ev["auth_id"].(string); id != "" {
			authorized[id] = true
		}
	}
	inRange := func(ev map[string]any) bool {
		ts, err := asInt64(ev["_recorded_at"])
		return err == nil && ts >= from && ts <= hi
	}
	omitted := func(ev map[string]any, ids ...string) {
		for _, id := range ids {
			if id != "" && reasons[id] {
				return
			}
		}
		amt, _ := asInt64(ev["amount"])
		if amt <= 0 {
			return
		}
		out = append(out, fail("omitted_from_ledger", map[string]any{"amount": amt},
			"a settlement or redemption with the hub's signed receipt is not in the ledger"))
	}
	for _, ev := range m.seam.ReadEvidence(EvPaymentSettled, 5000) {
		if v, _ := ev["verified"].(bool); !v || !inRange(ev) {
			continue
		}
		tx, _ := ev["transaction"].(string)
		aid, _ := ev["auth_id"].(string)
		omitted(ev, tx, aid)
	}
	for _, ev := range redeemed {
		if v, _ := ev["verified"].(bool); !v || !inRange(ev) {
			continue
		}
		id, _ := ev["auth_id"].(string)
		omitted(ev, id)
	}
	for _, en := range led.Entries {
		if en.Delta >= 0 || authorized[en.Reason] {
			continue
		}
		if t, ok := parseStamp(en.At); !ok || t < from || t > hi {
			continue
		}
		out = append(out, fail("unknown_debit", map[string]any{"amount": -en.Delta},
			"a debit with no authorization this node signed"))
	}
	return out
}

// parseStamp reads a ledger entry's time, RFC 3339 with nanoseconds, UTC.
func parseStamp(s string) (int64, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// entryTime is the time of the oldest entry on the page.
func entryTime(led *ledgerView) (int64, bool) {
	if len(led.Entries) == 0 {
		return 0, false
	}
	return parseStamp(led.Entries[0].At)
}

// auditErrorsBeforeAFinding is how many passes in a row (six hours apart)
// the hub must answer a balance-proof read with an error before doctor says so.
const auditErrorsBeforeAFinding = 2

// noteAuditError counts the passes that ended because the hub answered the
// read of a balance proof with an error (not because it could not be
// reached); a pass that checked resets it. A hub that keeps failing this read
// is a hub whose books cannot be checked, whatever the reason, and that must
// not look the same as a hub that checks out.
func (m *Module) noteAuditError(err error) {
	m.auditMu2.Lock()
	defer m.auditMu2.Unlock()
	if err == nil {
		m.auditErrs, m.auditErrAt = 0, 0
		return
	}
	var hs *hubStatusError
	if !errors.As(err, &hs) {
		return // the hub was not reached: that says nothing about its books
	}
	m.auditErrs++
	m.auditErrAt = time.Now().UnixMilli()
}

// AuditSummary is the last result of the check for doctor's payments.audit:
// the newest row of the proof chain, or, when the hub keeps answering the
// read of a proof with an error, that.
func (m *Module) AuditSummary() any {
	m.auditMu2.Lock()
	errs, errAt := m.auditErrs, m.auditErrAt
	m.auditMu2.Unlock()
	if errs >= auditErrorsBeforeAFinding {
		f := Finding{Code: "proof_unavailable", Level: levelFail, Args: map[string]any{"time": errAt},
			Detail: "the hub keeps answering the read of a balance proof with an error"}
		return map[string]any{"result": "proof_unavailable", "checked_at": errAt, "close": errAt, "finding": f}
	}
	rows := m.liab.all(m.hubAID())
	if len(rows) == 0 {
		return nil
	}
	last := rows[len(rows)-1]
	out := map[string]any{"to_day": last.ToDay, "result": last.Result, "checked_at": last.CheckedAt,
		"balance": last.Balance, "close": (int64(last.ToDay+1) * dayMillis)}
	if last.Result != resultOK {
		out["finding"] = findingOfRow(last)
	}
	return out
}

// auditLoop runs the check on its own: shortly after start, then every six
// hours, and only when the hub has published an epoch this node has not
// looked at (PAY-S9). It has no configuration.
func (m *Module) auditLoop(ctx context.Context) {
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	var seen string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		seen = m.auditPass(ctx, seen)
		t.Reset(auditEvery)
	}
}

// auditPass reads the hub's head and runs the check when it is new. It
// returns the head it looked at.
func (m *Module) auditPass(ctx context.Context, seen string) string {
	if m.hubURL() == "" || m.hubAID() == "" {
		return seen
	}
	var head struct {
		Seq *uint64 `json:"seq"`
		ID  string  `json:"id"`
	}
	if err := m.getJSON(ctx, "/x402/issuance/head", &head); err != nil {
		log.Printf("anet: reading the hub's supply head: %v", err)
		return seen
	}
	if head.ID == "" || head.ID == seen {
		return seen
	}
	rep, err := m.audit(ctx)
	if err != nil {
		log.Printf("anet: checking the hub's balance proofs: %v", err)
		m.noteAuditError(err)
		return seen
	}
	m.noteAuditError(nil)
	for _, f := range rep.Findings {
		log.Printf("anet: balance-proof check: %s (%s)", f.Code, f.Detail)
	}
	return head.ID
}

// checkChainLinks reports the ways a served issuance chain fails to be a
// chain: a first entry that is not the genesis record, a seq that does not
// follow the one before it, or a prev_id that does not name the record
// before it. The records handed to it have already been decoded and had
// their signatures checked.
//
// Kept apart from signature verification because the two answer different
// questions, and the case this exists for passes the first while failing
// the second: when a hub drops one of its own entries, every record it
// still serves is correctly signed and only the shape of what is left
// shows what happened.
//
// The chain was requested with from=0, so a first entry that is not the
// genesis record is the hub declining to serve the start of its history
// rather than a window the caller asked for.
func checkChainLinks(recs []*ael.EventRecord) []string {
	if len(recs) == 0 {
		return nil
	}
	// Order first, and nothing else if it is wrong. The genesis and gap
	// checks below read the served order as the chain order; run against a
	// reordered or duplicated page they would report entries missing where
	// nothing is missing, which is a worse answer than saying the page
	// cannot be followed.
	var problems []string
	for i := 1; i < len(recs); i++ {
		prev, cur := recs[i-1], recs[i]
		switch {
		case cur.Seq > prev.Seq:
			continue
		case cur.Seq == prev.Seq && cur.ID == prev.ID:
			problems = append(problems, fmt.Sprintf(
				"seq %d was served twice, so its amount is counted twice in the totals below",
				cur.Seq))
		case cur.Seq == prev.Seq:
			problems = append(problems, fmt.Sprintf(
				"seq %d was served as both %s and %s: the hub holds two different records "+
					"for one position on its chain",
				cur.Seq, short(prev.ID), short(cur.ID)))
		default:
			problems = append(problems, fmt.Sprintf(
				"seq %d was served after seq %d: the entries are not in chain order, "+
					"so what the hub holds cannot be followed from here",
				cur.Seq, prev.Seq))
		}
	}
	if len(problems) > 0 {
		return problems
	}
	if first := recs[0]; first.Seq != 0 || first.PrevID != ael.GenesisPrev() {
		problems = append(problems, fmt.Sprintf(
			"the chain begins at seq %d with prev_id %s rather than at the genesis entry: "+
				"the hub was asked for its history from seq 0 and did not serve the start of it",
			first.Seq, short(first.PrevID)))
	}
	for i := 1; i < len(recs); i++ {
		prev, cur := recs[i-1], recs[i]
		if cur.Seq > prev.Seq+1 {
			problems = append(problems, fmt.Sprintf(
				"the chain jumps from seq %d to seq %d: %s missing between them, "+
					"so the totals below are of what the hub chose to serve",
				prev.Seq, cur.Seq, countEntries(cur.Seq-prev.Seq-1)))
			// The prev_id of the entry after a gap names a record that was
			// not served, so checking the link here would only restate the
			// gap under a second heading.
			continue
		}
		if cur.PrevID != prev.ID {
			problems = append(problems, fmt.Sprintf(
				"seq %d names %s as the entry before it, but seq %d is %s",
				cur.Seq, short(cur.PrevID), prev.Seq, short(prev.ID)))
		}
	}
	return problems
}

// countEntries keeps the gap message readable for a gap of one, which is
// the common case: one entry removed.
func countEntries(n uint64) string {
	if n == 1 {
		return "1 entry is"
	}
	return fmt.Sprintf("%d entries are", n)
}

func short(id string) string {
	if len(id) > 14 {
		return id[:10] + "…"
	}
	return id
}

// asInt64 coerces whatever numeric type a decoder produced, and refuses
// what an int64 cannot hold exactly.
//
// Needed in more than one place for the same reason: values that pass
// through CBOR or JSON come back as int64, uint64 or float64 depending on
// the encoder, the width, and the sign. A single type assertion fails
// silently and yields zero, which reads as a real value.
//
// It used to convert as well as it could and never say so: a uint64 above
// math.MaxInt64 became a negative number, a float was truncated, and
// anything else became 0. Each of those is a real-looking value that is
// not the one on the record, so each is now an error.
func asInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case uint64:
		if n > math.MaxInt64 {
			return 0, fmt.Errorf("%d is above %d", n, int64(math.MaxInt64))
		}
		return int64(n), nil
	case int:
		return int64(n), nil
	case float64:
		// -2^63 is exact as a float64 and fits; 2^63 is exact and does not.
		if n != math.Trunc(n) || n < math.MinInt64 || n >= math.MaxInt64 {
			return 0, fmt.Errorf("%v is not a whole number an int64 can hold", n)
		}
		return int64(n), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s is not an int64: %w", n, err)
		}
		return i, nil
	case nil:
		return 0, fmt.Errorf("is missing")
	}
	return 0, fmt.Errorf("is a %T, not a number", v)
}

// refreshHubKEL has the node read the hub's key history again and drops the
// copy this module kept of it.
func (m *Module) refreshHubKEL() bool {
	if m.seam == nil || !m.seam.RefreshHubIdentity() {
		return false
	}
	m.mu.Lock()
	m.cachedURL = ""
	m.mu.Unlock()
	return true
}
