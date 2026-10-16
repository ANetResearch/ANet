package a2ashape

// spendcheck.go is the shape of anet.spend_check (CUX-S3, core §2.13): who
// can pay the quote a task waits on right now, and which tier's first
// failing check stands in the way. The daemon computes it (spendCheck); the
// projection places it.

// Who can pay a quote now.
const (
	SpendWhoAgent    = "agent"
	SpendWhoOperator = "operator"
	SpendWhoNobody   = "nobody"
)

// SpendCheck is anet.spend_check. Reason, ReasonTier and ReasonLimit speak
// of one thing: the tier that stops the payment and its first check that
// failed. ReasonTier is "auto", "agent" or "manual" ("" when the reason does
// not depend on a tier: quote_expired, rail_not_payable). ReasonLimit is set
// only for the three limit reasons, and is then below Amount. Amounts are
// decimal strings.
type SpendCheck struct {
	Who         string
	Amount      string
	Reason      string
	ReasonTier  string
	ReasonLimit *string
}

// Map is the value of the metadata key.
func (c SpendCheck) Map() map[string]any {
	m := map[string]any{"who": c.Who, "amount": c.Amount, "reason": c.Reason}
	if c.ReasonTier != "" {
		m["reason_tier"] = c.ReasonTier
	}
	if c.ReasonLimit != nil {
		m["reason_limit"] = *c.ReasonLimit
	}
	return m
}
