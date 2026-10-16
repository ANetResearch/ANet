//go:build !no_x402

package x402

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/module"
)

// Reconciling this node's own record against the hub's.
//
// Both halves already existed and nothing compared them. This node
// records every authorization it signs and every settlement it verifies
// on its own evidence chain; the hub publishes the ledger entries for the
// same account. A hub that altered this account's history contradicts a
// chain the hub cannot reach.
//
// The limit is worth stating precisely: this checks THIS account. It says
// nothing about credit issued to accounts the caller does not control,
// which is why the issuance chain and its witnesses exist separately.

// ReconcileReport is what a reconciliation found.
type ReconcileReport struct {
	AID string `json:"aid"`
	Hub string `json:"hub"`
	// Balance is what the hub says now. The ledger keeps WindowDays days
	// (HPP PAY-L6), so the entries do not add up to the balance: they add up
	// to how it moved in the window, WindowSum, and Balance − WindowSum is
	// what it was when the window began. Older times are vouched for by the
	// chain of balance proofs (anet audit-hub), not by this comparison.
	Balance        int64 `json:"balance"`
	WindowDays     int   `json:"window_days"`
	WindowSum      int64 `json:"window_sum"`
	OpeningBalance int64 `json:"balance_at_window_start"`
	// OutsideWindow counts events this node holds that are older than the
	// window, which the ledger no longer lists.
	OutsideWindow int `json:"outside_window,omitempty"`
	// Entries is how many the hub holds for this account, and Truncated
	// says the page returned was not all of them. Both are reported so a
	// reader can tell "the ledger disagrees" from "I only looked at part
	// of it".
	Entries   int  `json:"hub_entries"`
	Truncated bool `json:"hub_entries_truncated,omitempty"`
	// Chain events this node holds, and how many found a counterpart.
	Authorized int `json:"authorized_on_chain"`
	Settled    int `json:"settled_on_chain"`
	Redeemed   int `json:"redeemed_on_chain"`
	Matched    int `json:"matched"`
	// HubRedemptions is how many withdrawals the hub lists for this
	// account (its redemption list, read signed like the ledger), and
	// RedemptionsMatched how many of this node's recorded redemptions it
	// lists. RedemptionsUnchecked are recorded redemptions found in
	// neither the redemption page nor the ledger page while one of them
	// was truncated: absent from a page is not absent from the account.
	HubRedemptions       int  `json:"hub_redemptions"`
	RedemptionsTruncated bool `json:"hub_redemptions_truncated,omitempty"`
	RedemptionsMatched   int  `json:"redemptions_matched"`
	RedemptionsUnchecked int  `json:"redemptions_unchecked,omitempty"`
	// Unexplained entries are hub entries with no counterpart here.
	// Grants legitimately have none — credit arriving from the operator
	// has no agent-side event — so these are listed rather than reported
	// as fraud.
	Unexplained []string `json:"unexplained_hub_entries,omitempty"`
	// Missing are payments this node recorded that the hub's entries do
	// not account for. These are the serious ones.
	Missing  []string `json:"missing_from_hub,omitempty"`
	Problems []string `json:"problems,omitempty"`
	Agrees   bool     `json:"agrees"`
}

// Reconcile compares this node's payment history against the hub's
// ledger for this account.
func (m *Module) reconcile(ctx context.Context) (ReconcileReport, error) {
	rep := ReconcileReport{AID: m.AID(), Hub: m.hubURL()}
	if rep.Hub == "" {
		return rep, fmt.Errorf("x402: no hub configured, so there is nothing to reconcile against")
	}
	if m.seam == nil {
		return rep, fmt.Errorf("x402: no hub identity, so the hub's records cannot be checked")
	}

	var bal struct {
		Credits int64 `json:"credits"`
	}
	if err := m.signedGet(ctx, "/agents/"+m.AID()+"/balance", relayauth.ActionBalance, &bal); err != nil {
		return rep, err
	}
	rep.Balance = bal.Credits

	led, err := m.fetchLedger(ctx)
	if err != nil {
		return rep, err
	}
	// The window's total, not the sum of the page: the page is the newest
	// entries and says nothing about the rest of the window.
	rep.WindowDays = led.WindowDays
	if rep.WindowDays <= 0 {
		rep.WindowDays = 14
	}
	rep.WindowSum = led.Sum
	rep.OpeningBalance = rep.Balance - led.Sum
	rep.Entries, rep.Truncated = led.Total, led.Truncated
	// The newest balance proof this node checked, extended by the ledger
	// since its close, must give the balance now: the hub's number and its
	// own entries agree with what it signed to everybody.
	if proofs := m.liab.all(m.hubAID()); len(proofs) > 0 {
		var last *LiabProof
		for i := range proofs {
			if proofs[i].Result == resultOK {
				last = &proofs[i]
			}
		}
		if last != nil && windowCovers(&led, time.Now().UnixMilli(), int64(last.ToDay+1)*dayMillis, last.LedgerHW) {
			sum := new(big.Int).SetUint64(last.Balance)
			for _, e := range led.Entries {
				if e.Seq > last.LedgerHW {
					sum.Add(sum, big.NewInt(e.Delta))
				}
			}
			if sum.Cmp(big.NewInt(rep.Balance)) != 0 {
				rep.Problems = append(rep.Problems, fmt.Sprintf(
					"the hub reports a balance of %d, but the last balance proof (day %d) and the entries since add up to %s",
					rep.Balance, last.ToDay, sum.String()))
			}
		}
	}

	// This node's own signed record of what it did.
	auth := m.seam.ReadEvidence(EvPaymentAuthorized, 2000)
	settled := m.seam.ReadEvidence(EvPaymentSettled, 2000)
	redeemed := m.seam.ReadEvidence(EvCreditRedeemed, 2000)
	rep.Authorized, rep.Settled, rep.Redeemed = len(auth), len(settled), len(redeemed)

	// Every settlement this node verified should appear as a movement on
	// the hub's ledger. Matching on the transaction id, which both sides
	// carry.
	// Only the events the ledger can still show are compared: those from
	// the window, and from the page when it is not the whole window.
	from := time.Now().UnixMilli() - int64(rep.WindowDays)*dayMillis + 3600_000
	if t, ok := entryTime(&led); ok && led.Truncated && from < t {
		from = t
	}
	inWindow := func(evs []map[string]any) []map[string]any {
		var keep []map[string]any
		for _, ev := range evs {
			if ts, err := asInt64(ev[module.EvidenceAt]); err == nil && ts < from {
				rep.OutsideWindow++
				continue
			}
			keep = append(keep, ev)
		}
		return keep
	}
	settled, redeemed = inWindow(settled), inWindow(redeemed)
	hubTx := map[string]bool{}
	for _, e := range led.Entries {
		if e.Reason != "" {
			hubTx[e.Reason] = true
		}
	}
	for _, ev := range settled {
		tx, _ := ev["transaction"].(string)
		verified, checked := ev["verified"].(bool)
		if tx == "" {
			continue
		}
		if checked && !verified {
			// The payer's record of a provider's claim this node could not
			// verify: no payment of this node's stands behind it, so its
			// absence from the hub's entries says nothing about the hub.
			// Reported as missing, a provider's made-up receipt read as
			// the hub dropping a payment [redteam:F11].
			continue
		}
		authID, _ := ev["auth_id"].(string)
		if hubTx[tx] || (authID != "" && hubTx[authID]) {
			rep.Matched++
			continue
		}
		// A settlement this node holds a hub-signed receipt for, absent
		// from the hub's own published entries, is the hub contradicting
		// its own signature.
		note := fmt.Sprintf("settlement %s is not in the hub's entries", short(tx))
		if verified {
			note += " (this node holds the hub's signed receipt for it)"
		}
		rep.Missing = append(rep.Missing, note)
	}
	// Every redemption this node recorded should be in the hub's list of
	// this account's withdrawals, under the authorization id the hub
	// answered with (which is also the reason on its ledger entry).
	//
	// This used to look the reference up among the ledger entries' reasons.
	// The hub writes the authorization id there, never the reference, so
	// every redemption was reported missing from an honest hub; and the
	// list that does carry references was not read at all.
	red, err := m.redemptions(ctx)
	if err != nil {
		return rep, err
	}
	rep.HubRedemptions, rep.RedemptionsTruncated = red.Total, red.Truncated
	listedAuth, listedRef := map[string]bool{}, map[string]bool{}
	for _, r := range red.Redemptions {
		if r.AuthID != "" {
			listedAuth[r.AuthID] = true
		}
		if r.Reference != "" {
			listedRef[r.Reference] = true
		}
	}
	for _, ev := range redeemed {
		ref, _ := ev["reference"].(string)
		authID, _ := ev["auth_id"].(string)
		var found bool
		if authID != "" {
			found = listedAuth[authID] || hubTx[authID]
		} else {
			// An event without the hub's id (the hub refused, or an
			// older node): the reference is all there is to go by.
			found = ref != "" && listedRef[ref]
		}
		switch {
		case found:
			rep.RedemptionsMatched++
		case red.Truncated || led.Truncated:
			rep.RedemptionsUnchecked++
		case authID == "" && ref == "":
		default:
			note := fmt.Sprintf("redemption %q is not in the hub's list of this account's withdrawals", ref)
			if verified, _ := ev["verified"].(bool); verified {
				note += " (this node holds the hub's signed receipt for it)"
			}
			rep.Missing = append(rep.Missing, note)
		}
	}
	// Hub entries with no counterpart here. A grant is the ordinary case
	// and is not evidence of anything.
	for _, e := range led.Entries {
		if isGrantReason(e.Reason) {
			rep.Unexplained = append(rep.Unexplained,
				fmt.Sprintf("%+d %s (a grant has no counterpart on this node, which is normal)",
					e.Delta, e.Reason))
		}
	}
	sort.Strings(rep.Unexplained)
	rep.Agrees = len(rep.Problems) == 0 && len(rep.Missing) == 0
	return rep, nil
}

// isGrantReason reports whether a hub ledger entry is credit arriving
// from the operator rather than from an interaction.
func isGrantReason(reason string) bool {
	return len(reason) >= 6 && reason[:6] == "issued"
}
