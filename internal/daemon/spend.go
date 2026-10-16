package daemon

// spend.go is the node's spending policy (A2A-DESIGN §8.6, HPP PAY-D6,
// PAY-D7): the single place that decides whether this node may sign a
// payment, reached by the payment module through
// module.PaymentSeam.AdmitSpend before every signature over money. Whether
// a quote is worth paying at all — its signed price, which tier decides —
// is decidePay's (paynote.go); this file holds the limits it reads and
// enforces them again under the lock that records the payment.
//
//	purpose       surface                                   per payment    daily                         payees.allow
//	task-auto     the daemon paying a quote on its own      auto_max       daily_max                     yes
//	task-agent    POST /tasks/pay (MCP, local A2A client)   agent_max      agent_daily_max and daily_max yes
//	task-manual   POST /tasks/pay-manual (`anet pay`)       explicit_max   daily_max                     no
//	redeem        /redeem                                   explicit_max   daily_max                     no
//
// The automatic tier is not counted against agent_daily_max (HPP PAY-D6,
// countsAgentDaily): it is bound already by auto_max, the signed price and
// the payee list, and daily_max caps the day; counted twice, a node that set
// only auto_max and its payee list (agent_daily_max 0 by default) would never
// pay automatically.
//
// "Today" is the local calendar day (localDayBounds, core G14), the day the
// sandbox budget counts too. The daily totals count what this node SIGNED
// today, not what settled: an authorization is spendable by whoever holds it
// until it expires, so it is the signature that commits the money. One
// mutex covers the check and the record, so two payments cannot both pass
// against the same headroom. At start the totals are rebuilt from the
// anet.payment.authorized events since local midnight, which the module
// writes with the purpose; one written without a purpose (none should be)
// counts as task-manual, against daily_max only.
//
// The payee list is always on and always <data>/payees.allow (N-UX1): one
// AID per line, empty on a new node. It binds the automatic and the agent
// tiers only; a person's `anet pay` and a redemption are not bound by it.

import (
	"errors"
	"fmt"
	"log"
	"math"
	"sync"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// PaymentsConfig is the payments block of config.json.
//
//	"payments": {"auto_max": 0, "agent_max": 0, "agent_daily_max": 0,
//	             "explicit_max": 10, "daily_max": 50}
//
// A config without the block gets exactly these values (SI-5): nothing is
// paid without a person, and a person may pay small amounts. The payee list
// is not a setting: it is payees.allow in the data directory, empty at
// first (payeesFile).
type PaymentsConfig struct {
	AutoMax       uint64 `json:"auto_max"`
	AgentMax      uint64 `json:"agent_max"`
	AgentDailyMax uint64 `json:"agent_daily_max"`
	// ExplicitMax and DailyMax are pointers so that an operator's explicit
	// 0 ("no manual payments at all") is not read as "use the default".
	ExplicitMax *uint64 `json:"explicit_max,omitempty"`
	DailyMax    *uint64 `json:"daily_max,omitempty"`
}

// Defaults of the payments block (A2A-DESIGN §8.6), and the payee list's
// one place (N-UX1).
const (
	defaultExplicitMax = 10
	defaultDailyMax    = 50
	payeesFile         = "payees.allow"
)

// SpendLimits is the payments block with every default applied: what
// AdmitSpend enforces, and what `anet doctor` reports from the file.
type SpendLimits struct {
	AutoMax       uint64 `json:"auto_max"`
	AgentMax      uint64 `json:"agent_max"`
	AgentDailyMax uint64 `json:"agent_daily_max"`
	ExplicitMax   uint64 `json:"explicit_max"`
	DailyMax      uint64 `json:"daily_max"`
}

// Limits returns the effective limits of a payments block; a nil block
// has every default.
func (p *PaymentsConfig) Limits() SpendLimits { return p.limits() }

// limits returns the effective limits of a payments block.
func (p *PaymentsConfig) limits() SpendLimits {
	l := SpendLimits{ExplicitMax: defaultExplicitMax, DailyMax: defaultDailyMax}
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
	return l
}

// single is a tier's per-payment limit and the key that sets it; ok is
// false for a purpose that is not a spending tier.
func (l SpendLimits) single(purpose string) (limit uint64, key string, ok bool) {
	switch purpose {
	case module.PurposeTaskAuto:
		return l.AutoMax, "auto_max", true
	case module.PurposeTaskAgent:
		return l.AgentMax, "agent_max", true
	case module.PurposeTaskManual, module.PurposeRedeem:
		return l.ExplicitMax, "explicit_max", true
	}
	return 0, "", false
}

// defaultPayments is the payments block of a fresh install with every key
// written out (SI-5): nothing is paid without a person, and a person may
// pay small amounts. `anet init` and a config loaded without the block
// write exactly this.
func defaultPayments() PaymentsConfig {
	em, dm := uint64(defaultExplicitMax), uint64(defaultDailyMax)
	return PaymentsConfig{ExplicitMax: &em, DailyMax: &dm}
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

// Refusal codes of AdmitSpend: ux §1.4 D4, and the two no tier pays.
// Stable: the control plane returns them as "reason" and tests pin them.
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
// caller finds it with errors.As. The numbers are those of the check that
// failed, for its sentence (ux §1.4 D4).
type SpendRefusal struct {
	Code    string
	Purpose string
	Detail  string
	// Amount is the payment; Single and Key the per-payment limit and its
	// setting (over_single_limit); Spent, Daily and Need what was signed
	// today in the limit's count, the daily limit, and the daily limit
	// that would let the payment through (the two daily codes); PayTo the
	// payee.
	Amount, Single, Spent, Daily, Need uint64
	Key, PayTo                         string
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

// spendRecord is one admitted payment.
type spendRecord struct {
	at      int64 // unix ms
	amount  uint64
	purpose string
}

// spendBook holds the payments admitted today (and, until the next check
// drops them, earlier). The zero value is ready once load has run.
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

// leftOf is limit − spent, 0 when spent is at or above it (HPP §0.3:
// a negative remainder counts as 0).
func leftOf(limit, spent uint64) uint64 {
	if spent >= limit {
		return 0
	}
	return limit - spent
}

// countsAgentDaily reports whether a purpose counts against
// agent_daily_max: the agent tier only (HPP PAY-D6). The automatic tier,
// the manual tier and redemptions count against daily_max alone. decidePay
// and AdmitSpend read today's totals through it.
func countsAgentDaily(purpose string) bool {
	return purpose == module.PurposeTaskAgent
}

// spentSince sums the records at or after from (unix ms): everything, and
// what counts against agent_daily_max. Saturating.
func spentSince(records []spendRecord, from int64) (all, agent uint64) {
	for _, r := range records {
		if r.at < from {
			continue
		}
		all = spendSum(all, r.amount)
		if countsAgentDaily(r.purpose) {
			agent = spendSum(agent, r.amount)
		}
	}
	return all, agent
}

// readPayees reads the payee list (payees.allow in the data directory).
func (d *Daemon) readPayees() (map[string]bool, error) {
	return readPeerFile(d.peerFile(payeesFile))
}

// AdmitSpend is the policy (see the file comment). On a yes it records the
// payment before returning, under the same lock as the check.
func (d *Daemon) AdmitSpend(payTo string, amount uint64, purpose string) error {
	lim := d.config().Payments.limits()
	refuse := func(r SpendRefusal, format string, args ...any) error {
		r.Purpose, r.Amount, r.PayTo, r.Detail = purpose, amount, payTo, fmt.Sprintf(format, args...)
		return &r
	}
	single, key, ok := lim.single(purpose)
	if !ok {
		return refuse(SpendRefusal{Code: SpendUnknownPurpose}, "%q is not a spending purpose", purpose)
	}
	if amount == 0 {
		return refuse(SpendRefusal{Code: SpendZeroAmount}, "nothing to pay")
	}
	if amount > single {
		return refuse(SpendRefusal{Code: SpendOverSingle, Single: single, Key: key},
			"%d is above the %s limit of %d per payment", amount, purpose, single)
	}
	if purpose == module.PurposeTaskAuto || purpose == module.PurposeTaskAgent {
		payees, err := d.readPayees()
		if err != nil {
			return refuse(SpendRefusal{Code: SpendPayeesUnread}, "the payee list %s cannot be read: %v", payeesFile, err)
		}
		if !payees[payTo] {
			return refuse(SpendRefusal{Code: SpendPayeeNotAllowed}, "%s is not on the payee list %s", payTo, payeesFile)
		}
	}
	b := &d.spend
	b.mu.Lock()
	defer b.mu.Unlock()
	d.loadSpendLocked()
	now := d.now()
	start, _ := localDayBounds(now)
	from := start.UnixMilli()
	kept := b.records[:0]
	for _, r := range b.records {
		if r.at >= from {
			kept = append(kept, r)
		}
	}
	b.records = kept
	all, agent := spentSince(b.records, from)
	if countsAgentDaily(purpose) && spendSum(agent, amount) > lim.AgentDailyMax {
		return refuse(SpendRefusal{Code: SpendOverAgentDaily, Spent: agent, Daily: lim.AgentDailyMax,
			Need: spendSum(agent, amount)}, "%d more would bring agent payments today to %d, above %d",
			amount, spendSum(agent, amount), lim.AgentDailyMax)
	}
	if spendSum(all, amount) > lim.DailyMax {
		return refuse(SpendRefusal{Code: SpendOverDaily, Spent: all, Daily: lim.DailyMax, Need: spendSum(all, amount)},
			"%d more would bring payments today to %d, above %d", amount, spendSum(all, amount), lim.DailyMax)
	}
	b.records = append(b.records, spendRecord{at: now.UnixMilli(), amount: amount, purpose: purpose})
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
// Every anet.payment.authorized event since local midnight counts, in the
// purpose it was signed for. An event without a purpose — none is written
// any more; the gateway tier that wrote some is gone — counts as
// task-manual, against daily_max only: what it was for is not known, and
// counting it nowhere would let a restart forget money already committed.
func (d *Daemon) loadSpendLocked() {
	b := &d.spend
	if b.loaded {
		return
	}
	b.loaded = true
	if d.ledger == nil {
		return
	}
	start, _ := localDayBounds(d.now())
	d.ledger.scan(EvPaymentAuthorized, start.UnixMilli(), func(at int64, p map[string]any) {
		amount, err := payment.ParseAmount(fmt.Sprint(p["amount"]))
		if err != nil {
			return
		}
		purpose, _ := p["purpose"].(string)
		if purpose == "" {
			purpose = module.PurposeTaskManual
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
	// The records a pending key event holds back count as well
	// (evidence_queue.go): they were written, only not yet signed.
	queued := l.queuedOf(eventType, sinceMS)
	l.mu.Unlock()
	for _, r := range recs {
		fn(r.Timestamp, plainMap(r.Payload))
	}
	for _, r := range queued {
		var p any
		if coredet.Unmarshal(r.Payload, &p) == nil {
			fn(r.Timestamp, plainMap(p))
		}
	}
}

// SpendStatus is the payments block and today's totals, for a person
// (/payments/status, HPP §2.7).
type SpendStatus struct {
	AutoMax       uint64 `json:"auto_max"`
	AgentMax      uint64 `json:"agent_max"`
	AgentDailyMax uint64 `json:"agent_daily_max"`
	ExplicitMax   uint64 `json:"explicit_max"`
	DailyMax      uint64 `json:"daily_max"`
	// SpentToday and AgentSpentToday are what this node signed since local
	// midnight, in total and in the agent tier (countsAgentDaily).
	SpentToday      uint64 `json:"spent_today"`
	AgentSpentToday uint64 `json:"agent_spent_today"`
	// LeftToday is what the manual and the automatic tier may still pay
	// today (daily_max − spent_today); AgentLeftToday the agent tier's,
	// the smaller of its two remainders (HPP §0.3).
	LeftToday      uint64 `json:"left_today"`
	AgentLeftToday uint64 `json:"agent_left_today"`
	// Payees is how many payees are on the payee list; 0 when it cannot be
	// read, which PayeesError then says.
	Payees      uint64 `json:"payees"`
	PayeesError string `json:"payees_error,omitempty"`
}

// tierLeft is what a tier may still pay today (HPP §0.3).
func (st SpendStatus) tierLeft(purpose string) uint64 {
	if countsAgentDaily(purpose) {
		return st.AgentLeftToday
	}
	return st.LeftToday
}

// SpendStatus reports the effective limits and the totals they apply to.
func (d *Daemon) SpendStatus() SpendStatus {
	lim := d.config().Payments.limits()
	out := SpendStatus{AutoMax: lim.AutoMax, AgentMax: lim.AgentMax, AgentDailyMax: lim.AgentDailyMax,
		ExplicitMax: lim.ExplicitMax, DailyMax: lim.DailyMax}
	b := &d.spend
	b.mu.Lock()
	d.loadSpendLocked()
	start, _ := localDayBounds(d.now())
	out.SpentToday, out.AgentSpentToday = spentSince(b.records, start.UnixMilli())
	b.mu.Unlock()
	out.LeftToday = leftOf(lim.DailyMax, out.SpentToday)
	out.AgentLeftToday = min(leftOf(lim.AgentDailyMax, out.AgentSpentToday), out.LeftToday)
	if payees, err := d.readPayees(); err != nil {
		out.PayeesError = err.Error()
	} else {
		out.Payees = uint64(len(payees))
	}
	return out
}

// SetSpendLimits replaces keys of the payments block. The CLI asks for a
// terminal confirmation before it calls this (§8.6: changing a limit is a
// TTY command); the change is recorded as anet.policy.changed.
//
// Saved first and in force after, like the other policy writes
// (SetInboundPolicy): a caller told the change failed must not find it in
// force, not even while the save is being tried.
func (d *Daemon) SetSpendLimits(set map[string]uint64) error {
	d.cfgWrite.Lock()
	defer d.cfgWrite.Unlock()
	next := d.config()
	if err := d.carryDiskConfig(&next); err != nil {
		return err
	}
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
			return fmt.Errorf("anet: %q is not a payments limit (auto_max, agent_max, agent_daily_max, explicit_max, daily_max)", k)
		}
	}
	next.Payments = &cur
	if err := SaveConfig(d.layout, next); err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg.Payments = next.Payments
	d.mu.Unlock()
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
