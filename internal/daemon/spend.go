package daemon

// spend.go is the node's spending policy (A2A-DESIGN §8.6): the single
// place that decides whether this node may sign a payment, reached by the
// payment module through module.PaymentSeam.AdmitSpend before every
// signature over money.
//
//	purpose       surface                                   per payment    daily
//	task-auto     the daemon paying a quote on its own      auto_max       agent_daily_max and daily_max
//	task-agent    POST /tasks/pay (MCP, local A2A client)   agent_max      agent_daily_max and daily_max
//	task-manual   POST /tasks/pay-manual (`anet pay`)       explicit_max   daily_max
//	gateway       /x402-authorize, /delegate pay:true       explicit_max   daily_max
//	redeem        /redeem                                   explicit_max   daily_max, payees not applied
//
// The daily totals count what this node SIGNED, not what settled: an
// authorization is spendable by whoever holds it until it expires, so it
// is the signature that commits the money. One mutex covers the check and
// the record, so two payments cannot both pass against the same headroom.
// At start the totals are rebuilt from the anet.payment.authorized events
// of the last 24 hours, which the module writes with the purpose.

import (
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// PaymentsConfig is the payments block of config.json.
//
//	"payments": {"auto_max": 0, "agent_max": 0, "agent_daily_max": 0,
//	             "explicit_max": 10, "daily_max": 50, "payees_file": "payees.allow"}
//
// A config without the block gets exactly these values (SI-5): nothing is
// paid without a person, a person may pay small amounts, and every payee
// must be on the list, which starts empty.
type PaymentsConfig struct {
	AutoMax       uint64 `json:"auto_max"`
	AgentMax      uint64 `json:"agent_max"`
	AgentDailyMax uint64 `json:"agent_daily_max"`
	// ExplicitMax and DailyMax are pointers so that an operator's explicit
	// 0 ("no manual payments at all") is not read as "use the default".
	ExplicitMax *uint64 `json:"explicit_max,omitempty"`
	DailyMax    *uint64 `json:"daily_max,omitempty"`
	// PayeesFile is the payee allow list, one AID per line, relative to the
	// data directory. Absent means payees.allow; an empty string turns the
	// list off. A missing file is an empty list.
	PayeesFile *string `json:"payees_file,omitempty"`
	// PublishPrices says whether this node's cards carry a price per
	// skill: the A2A card's anet-pricing/v1 and the ADP card's price list,
	// which the hub gateway sells from. Absent means true. With false the
	// price is given only in the quote, inside the end-to-end encrypted
	// task: a settlement's payee and amount (which the hub sees, and the
	// public issuance chain shows for a cross-hub payment) no longer name
	// the skill through the published list, and the hub gateway cannot
	// sell this node's skills (A2A-DESIGN §21) [redteam:F1].
	PublishPrices *bool `json:"publish_prices,omitempty"`
}

// publishesPrices is payments.publish_prices with its default (true); a nil
// block publishes.
func (p *PaymentsConfig) publishesPrices() bool {
	return p == nil || p.PublishPrices == nil || *p.PublishPrices
}

// Defaults of the payments block (A2A-DESIGN §8.6).
const (
	defaultExplicitMax = 10
	defaultDailyMax    = 50
	defaultPayeesFile  = "payees.allow"
)

// SpendLimits is the payments block with every default applied: what
// AdmitSpend enforces, and what `anet doctor` reports from the file.
type SpendLimits struct {
	AutoMax       uint64 `json:"auto_max"`
	AgentMax      uint64 `json:"agent_max"`
	AgentDailyMax uint64 `json:"agent_daily_max"`
	ExplicitMax   uint64 `json:"explicit_max"`
	DailyMax      uint64 `json:"daily_max"`
	PayeesFile    string `json:"payees_file"` // "" = no payee list
}

// Limits returns the effective limits of a payments block; a nil block
// has every default.
func (p *PaymentsConfig) Limits() SpendLimits { return p.limits() }

// limits returns the effective limits of a payments block.
func (p *PaymentsConfig) limits() SpendLimits {
	l := SpendLimits{ExplicitMax: defaultExplicitMax, DailyMax: defaultDailyMax, PayeesFile: defaultPayeesFile}
	if p == nil {
		return l
	}
	l.AutoMax, l.AgentMax, l.AgentDailyMax = p.AutoMax, p.AgentMax, p.AgentDailyMax
	if p.ExplicitMax != nil {
		l.ExplicitMax = *p.ExplicitMax
	}
	if p.DailyMax != nil {
		l.DailyMax = *p.DailyMax
	}
	if p.PayeesFile != nil {
		l.PayeesFile = *p.PayeesFile
	}
	return l
}

// defaultPayments is the payments block of a fresh install with every key
// written out (SI-5): nothing is paid without a person, a person may pay
// small amounts, and the payee list is on and empty. `anet init` and a
// config loaded without the block write exactly this.
func defaultPayments() PaymentsConfig {
	em, dm, pf := uint64(defaultExplicitMax), uint64(defaultDailyMax), defaultPayeesFile
	return PaymentsConfig{ExplicitMax: &em, DailyMax: &dm, PayeesFile: &pf}
}

// migratePayments gives a config without a payments block the defaults and
// marks it for rewriting, so the file on disk states the limits explicitly.
func migratePayments(c *Config) {
	if c.Payments == nil {
		p := defaultPayments()
		c.Payments = &p
		c.rewriteConfig = true
	}
}

// Refusal codes of AdmitSpend. Stable: the control plane returns them as
// "reason" and tests pin them.
const (
	SpendUnknownPurpose  = "unknown_purpose"
	SpendZeroAmount      = "zero_amount"
	SpendOverSingle      = "over_single_limit"
	SpendOverAgentDaily  = "over_agent_daily_limit"
	SpendOverDaily       = "over_daily_limit"
	SpendPayeeNotAllowed = "payee_not_allowed"
	SpendPayeesUnread    = "payees_unreadable"
)

// SpendRefusal is AdmitSpend's no. The payment module wraps it, so a
// caller finds it with errors.As.
type SpendRefusal struct {
	Code    string
	Purpose string
	Detail  string
}

func (e *SpendRefusal) Error() string {
	return fmt.Sprintf("spending policy refused a %s payment (%s): %s", e.Purpose, e.Code, e.Detail)
}

// isSpendRefusal reports whether err carries a spending-policy refusal.
func isSpendRefusal(err error) (*SpendRefusal, bool) {
	var r *SpendRefusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// spendWindow is how far back the daily totals look.
const spendWindow = 24 * time.Hour

// spendRecord is one admitted payment.
type spendRecord struct {
	at      int64 // unix ms
	amount  uint64
	purpose string
}

// spendBook holds the admitted payments of the last 24 hours. The zero
// value is ready once load has run.
type spendBook struct {
	mu      sync.Mutex
	loaded  bool
	records []spendRecord
}

// spendSum is a+b, or math.MaxUint64 when that does not fit.
//
// The daily totals are compared as "spent + this payment > limit". Added
// plainly, a payment close to 2^64 wrapped the sum round to a small number
// and passed any daily limit, on a node whose per-payment limit was set
// that high ("no limit"); a provider chooses the quoted amount. A sum that
// saturates is above every limit below the top of the range.
func spendSum(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// agentTier reports whether a purpose counts against agent_daily_max.
func agentTier(purpose string) bool {
	return purpose == module.PurposeTaskAuto || purpose == module.PurposeTaskAgent
}

// AdmitSpend is the policy (see the file comment). On a yes it records the
// payment before returning, under the same lock as the check.
func (d *Daemon) AdmitSpend(payTo string, amount uint64, purpose string) error {
	lim := d.config().Payments.limits()
	refuse := func(code, format string, args ...any) error {
		return &SpendRefusal{Code: code, Purpose: purpose, Detail: fmt.Sprintf(format, args...)}
	}
	var single uint64
	switch purpose {
	case module.PurposeTaskAuto:
		single = lim.AutoMax
	case module.PurposeTaskAgent:
		single = lim.AgentMax
	case module.PurposeTaskManual, module.PurposeGateway, module.PurposeRedeem:
		single = lim.ExplicitMax
	default:
		return refuse(SpendUnknownPurpose, "%q is not a spending purpose", purpose)
	}
	if amount == 0 {
		return refuse(SpendZeroAmount, "nothing to pay")
	}
	if amount > single {
		return refuse(SpendOverSingle, "%d is above the %s limit of %d per payment", amount, purpose, single)
	}
	if purpose != module.PurposeRedeem && lim.PayeesFile != "" {
		payees, err := readPeerFile(d.peerFile(lim.PayeesFile))
		if err != nil {
			return refuse(SpendPayeesUnread, "the payee list %s cannot be read: %v", lim.PayeesFile, err)
		}
		if !payees[payTo] {
			return refuse(SpendPayeeNotAllowed, "%s is not on the payee list %s", payTo, lim.PayeesFile)
		}
	}
	b := &d.spend
	b.mu.Lock()
	defer b.mu.Unlock()
	d.loadSpendLocked()
	now := time.Now().UnixMilli()
	cut := now - spendWindow.Milliseconds()
	kept := b.records[:0]
	var all, agent uint64
	for _, r := range b.records {
		if r.at <= cut {
			continue
		}
		kept = append(kept, r)
		all = spendSum(all, r.amount)
		if agentTier(r.purpose) {
			agent = spendSum(agent, r.amount)
		}
	}
	b.records = kept
	if agentTier(purpose) && spendSum(agent, amount) > lim.AgentDailyMax {
		return refuse(SpendOverAgentDaily, "%d more would bring agent payments in 24h to %d, above %d",
			amount, spendSum(agent, amount), lim.AgentDailyMax)
	}
	if spendSum(all, amount) > lim.DailyMax {
		return refuse(SpendOverDaily, "%d more would bring payments in 24h to %d, above %d",
			amount, spendSum(all, amount), lim.DailyMax)
	}
	b.records = append(b.records, spendRecord{at: now, amount: amount, purpose: purpose})
	return nil
}

// AdmitSpend implements module.PaymentSeam.
func (s paymentSeam) AdmitSpend(payTo string, amount uint64, purpose string) error {
	return s.d.AdmitSpend(payTo, amount, purpose)
}

// loadSpend rebuilds the daily totals from this node's chain, once.
func (d *Daemon) loadSpend() {
	d.spend.mu.Lock()
	defer d.spend.mu.Unlock()
	d.loadSpendLocked()
}

// loadSpendLocked is loadSpend with d.spend.mu held.
//
// Every anet.payment.authorized event of the last 24 hours counts, in the
// purpose it was signed for. An event written before the purpose was
// recorded counts against daily_max only: what it was for is not known,
// and counting it nowhere would let a restart forget money already
// committed.
func (d *Daemon) loadSpendLocked() {
	b := &d.spend
	if b.loaded {
		return
	}
	b.loaded = true
	if d.ledger == nil {
		return
	}
	cut := time.Now().Add(-spendWindow).UnixMilli()
	d.ledger.scan(EvPaymentAuthorized, cut, func(at int64, p map[string]any) {
		amount, err := payment.ParseAmount(fmt.Sprint(p["amount"]))
		if err != nil {
			return
		}
		purpose, _ := p["purpose"].(string)
		if purpose == "" {
			purpose = module.PurposeGateway
		}
		b.records = append(b.records, spendRecord{at: at, amount: amount, purpose: purpose})
	})
}

// EvPaymentAuthorized is the event the payment module writes for every
// authorization it signs (module/x402 pay.go). Named here because the
// spending policy reads it back.
const EvPaymentAuthorized = "anet.payment.authorized"

// scan calls fn for every event of eventType with a timestamp at or after
// sinceMS, oldest first, walking the whole verified chain rather than the
// bounded tail Evidence serves.
func (l *evidenceLedger) scan(eventType string, sinceMS int64, fn func(at int64, payload map[string]any)) {
	l.mu.Lock()
	var recs []*ael.EventRecord
	for _, r := range l.led.Events(l.did) {
		if r.EventType == eventType && r.Timestamp >= sinceMS {
			recs = append(recs, r)
		}
	}
	l.mu.Unlock()
	for _, r := range recs {
		fn(r.Timestamp, plainMap(r.Payload))
	}
}

// SpendStatus is the payments block and today's totals, for a person.
type SpendStatus struct {
	AutoMax       uint64 `json:"auto_max"`
	AgentMax      uint64 `json:"agent_max"`
	AgentDailyMax uint64 `json:"agent_daily_max"`
	ExplicitMax   uint64 `json:"explicit_max"`
	DailyMax      uint64 `json:"daily_max"`
	PayeesFile    string `json:"payees_file"`
	// Spent24h and AgentSpent24h are what this node signed in the last 24
	// hours, in total and in the agent tiers.
	Spent24h      uint64 `json:"spent_24h"`
	AgentSpent24h uint64 `json:"agent_spent_24h"`
}

// SpendStatus reports the effective limits and the totals they apply to.
func (d *Daemon) SpendStatus() SpendStatus {
	lim := d.config().Payments.limits()
	out := SpendStatus{AutoMax: lim.AutoMax, AgentMax: lim.AgentMax, AgentDailyMax: lim.AgentDailyMax,
		ExplicitMax: lim.ExplicitMax, DailyMax: lim.DailyMax, PayeesFile: lim.PayeesFile}
	b := &d.spend
	b.mu.Lock()
	defer b.mu.Unlock()
	d.loadSpendLocked()
	cut := time.Now().Add(-spendWindow).UnixMilli()
	for _, r := range b.records {
		if r.at <= cut {
			continue
		}
		out.Spent24h = spendSum(out.Spent24h, r.amount)
		if agentTier(r.purpose) {
			out.AgentSpent24h = spendSum(out.AgentSpent24h, r.amount)
		}
	}
	return out
}

// SetSpendLimits replaces keys of the payments block. The CLI asks for a
// terminal confirmation before it calls this (§8.6: changing a limit is a
// TTY command); the change is recorded as anet.policy.changed.
func (d *Daemon) SetSpendLimits(set map[string]uint64, payeesFile *string) error {
	d.mu.Lock()
	next := d.cfg
	var cur PaymentsConfig
	if next.Payments != nil {
		cur = *next.Payments
	}
	before := cur.limits()
	for k, v := range set {
		v := v
		switch k {
		case "auto_max":
			cur.AutoMax = v
		case "agent_max":
			cur.AgentMax = v
		case "agent_daily_max":
			cur.AgentDailyMax = v
		case "explicit_max":
			cur.ExplicitMax = &v
		case "daily_max":
			cur.DailyMax = &v
		default:
			d.mu.Unlock()
			return fmt.Errorf("anet: %q is not a payments limit (auto_max, agent_max, agent_daily_max, explicit_max, daily_max)", k)
		}
	}
	if payeesFile != nil {
		pf := *payeesFile
		cur.PayeesFile = &pf
	}
	prev := d.cfg.Payments
	next.Payments = &cur
	d.cfg = next
	d.mu.Unlock()
	if err := SaveConfig(d.layout, next); err != nil {
		// A caller told the change failed must not find it in force: put
		// the limits back, unless another write has replaced them since.
		d.mu.Lock()
		if d.cfg.Payments == next.Payments {
			d.cfg.Payments = prev
		}
		d.mu.Unlock()
		return err
	}
	after := cur.limits()
	for _, c := range []struct {
		key      string
		from, to any
	}{
		{"auto_max", before.AutoMax, after.AutoMax},
		{"agent_max", before.AgentMax, after.AgentMax},
		{"agent_daily_max", before.AgentDailyMax, after.AgentDailyMax},
		{"explicit_max", before.ExplicitMax, after.ExplicitMax},
		{"daily_max", before.DailyMax, after.DailyMax},
		{"payees_file", before.PayeesFile, after.PayeesFile},
	} {
		if c.from != c.to {
			d.recordPolicyChange("payments."+c.key, c.from, c.to, nil)
		}
	}
	return nil
}

// logSpendRefusal notes a refused payment in the daemon log.
func logSpendRefusal(ix string, r *SpendRefusal) {
	log.Printf("anet: %s: payment not signed: %s", ix, r.Detail)
}
