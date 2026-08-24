//go:build !no_x402

package x402

import (
	"context"
	"fmt"
	"sort"
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
	// Balance is what the hub says, and Derived is what the entries it
	// published add up to. They come from the same hub, so agreement
	// proves consistency rather than honesty — but disagreement is the
	// hub contradicting itself, which is worth seeing.
	Balance int64 `json:"balance"`
	Derived int64 `json:"derived_from_entries"`
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
	if err := m.getJSON(ctx, "/agents/"+m.AID()+"/balance", &bal); err != nil {
		return rep, err
	}
	rep.Balance = bal.Credits

	var led struct {
		Entries []struct {
			Delta  int64  `json:"delta"`
			Reason string `json:"reason"`
			At     string `json:"at"`
		} `json:"entries"`
		Total     int   `json:"total"`
		Sum       int64 `json:"sum"`
		Truncated bool  `json:"truncated"`
	}
	if err := m.getJSON(ctx, "/agents/"+m.AID()+"/ledger?limit=500", &led); err != nil {
		return rep, err
	}
	// The account total, not the sum of the page.
	//
	// The endpoint returns the newest hundred by default and said nothing
	// about it, so summing what came back compared a page against a
	// balance and reported a discrepancy on every account with more
	// history than that. dmax showed 866 against entries summing to -109,
	// which was the cap, not the ledger.
	//
	// An older hub sends no total; falling back to the page is then the
	// best available and the truncation flag says whether to trust it.
	if led.Total > 0 {
		rep.Derived = led.Sum
	} else {
		for _, e := range led.Entries {
			rep.Derived += e.Delta
		}
	}
	rep.Entries, rep.Truncated = led.Total, led.Truncated
	// The hub's own two numbers must agree with each other. They are both
	// its statements, so this catches a hub that is inconsistent rather
	// than one that is dishonest — but an inconsistent ledger is a real
	// finding and nothing else was checking it.
	if rep.Derived != rep.Balance {
		rep.Problems = append(rep.Problems, fmt.Sprintf(
			"the hub reports a balance of %d but the entries it published sum to %d",
			rep.Balance, rep.Derived))
	}

	// This node's own signed record of what it did.
	auth := m.seam.ReadEvidence(EvPaymentAuthorized, 2000)
	settled := m.seam.ReadEvidence(EvPaymentSettled, 2000)
	redeemed := m.seam.ReadEvidence(EvCreditRedeemed, 2000)
	rep.Authorized, rep.Settled, rep.Redeemed = len(auth), len(settled), len(redeemed)

	// Every settlement this node verified should appear as a movement on
	// the hub's ledger. Matching on the transaction id, which both sides
	// carry.
	hubTx := map[string]bool{}
	for _, e := range led.Entries {
		if e.Reason != "" {
			hubTx[e.Reason] = true
		}
	}
	for _, ev := range settled {
		tx, _ := ev["transaction"].(string)
		verified, _ := ev["verified"].(bool)
		if tx == "" {
			continue
		}
		if hubTx[tx] {
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
	for _, ev := range redeemed {
		ref, _ := ev["reference"].(string)
		if ref != "" && !hubTx[ref] {
			rep.Missing = append(rep.Missing,
				fmt.Sprintf("redemption %q is not in the hub's entries", ref))
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
	switch reason {
	case "registration grant":
		return true
	}
	return len(reason) >= 6 && reason[:6] == "issued"
}
