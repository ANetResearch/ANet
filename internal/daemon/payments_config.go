package daemon

// payments_config.go holds the payments block of config.json (A2A-DESIGN
// §8.6) — the limits of the three spending tiers and the payee allow list —
// and the control route that changes the limits.
//
// This file is configuration only. The one point that enforces it
// (module.PaymentSeam.AdmitSpend) reads Config.payments(), so a limit
// written here applies to the next authorization without a restart.
//
// Changing a limit is a human decision: `anet payments limits` reads a
// confirmation from /dev/tty and refuses without one. The route is
// bearer-only, not a console session route, and cannot tell how its caller
// obtained the control token (§21 item 13).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// PaymentsConfig is the payments block of config.json.
type PaymentsConfig struct {
	// AutoMax is the largest single payment the daemon makes on its own
	// (purpose task-auto). 0, the default, means it never pays by itself.
	AutoMax uint64 `json:"auto_max"`
	// AgentMax and AgentDailyMax bound what an agent submits through MCP
	// submit_payment or the local A2A interface (purpose task-agent): per
	// payment, and per 24 hours with task-auto payments counted in. Both 0
	// by default: an agent cannot pay until a person raises them.
	AgentMax      uint64 `json:"agent_max"`
	AgentDailyMax uint64 `json:"agent_daily_max"`
	// ExplicitMax and DailyMax bound payments a person confirms on a
	// terminal (anet pay, anet redeem) and gateway payments; DailyMax also
	// counts every other tier.
	ExplicitMax uint64 `json:"explicit_max"`
	DailyMax    uint64 `json:"daily_max"`
	// PayeesFile lists the payees this node may pay, one AID per line, '#'
	// starts a comment. A non-empty value enables the list and a missing
	// file is an empty list (nobody); an empty value disables it. A
	// relative path is relative to the data directory. redeem, whose payee
	// is the hub, is not subject to it.
	PayeesFile string `json:"payees_file"`
}

// Defaults of the payments block (§8.6).
const (
	defaultExplicitMax = 10
	defaultDailyMax    = 50
	defaultPayeesFile  = "payees.allow"
)

// defaultPayments is the payments block of a fresh install: nothing is paid
// without a person, and the payee list is on and empty (SI-5).
func defaultPayments() PaymentsConfig {
	return PaymentsConfig{ExplicitMax: defaultExplicitMax, DailyMax: defaultDailyMax, PayeesFile: defaultPayeesFile}
}

// UnmarshalJSON starts from the defaults, so a block that leaves a key out
// gets its default rather than zero: a missing payees_file keeps the payee
// list on, and a missing explicit_max does not silently stop manual
// payments.
func (p *PaymentsConfig) UnmarshalJSON(b []byte) error {
	type plain PaymentsConfig
	v := plain(defaultPayments())
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*p = PaymentsConfig(v)
	return nil
}

// payments returns the effective payments block.
func (c Config) payments() PaymentsConfig {
	if c.Payments == nil {
		return defaultPayments()
	}
	return *c.Payments
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

// PaymentLimits is a partial update of the spending limits: a nil field is
// left as it is.
type PaymentLimits struct {
	AutoMax       *uint64 `json:"auto_max,omitempty"`
	AgentMax      *uint64 `json:"agent_max,omitempty"`
	AgentDailyMax *uint64 `json:"agent_daily_max,omitempty"`
	ExplicitMax   *uint64 `json:"explicit_max,omitempty"`
	DailyMax      *uint64 `json:"daily_max,omitempty"`
}

// fields pairs each settable limit with its config key.
func (u PaymentLimits) fields(p *PaymentsConfig) []struct {
	key string
	set *uint64
	cur *uint64
} {
	return []struct {
		key string
		set *uint64
		cur *uint64
	}{
		{"auto_max", u.AutoMax, &p.AutoMax},
		{"agent_max", u.AgentMax, &p.AgentMax},
		{"agent_daily_max", u.AgentDailyMax, &p.AgentDailyMax},
		{"explicit_max", u.ExplicitMax, &p.ExplicitMax},
		{"daily_max", u.DailyMax, &p.DailyMax},
	}
}

// SetPaymentLimits applies u, saves the config and writes one
// anet.policy.changed per limit that changed. It returns the effective
// block and the keys that changed.
func (d *Daemon) SetPaymentLimits(u PaymentLimits) (PaymentsConfig, []string, error) {
	type change struct {
		key      string
		from, to uint64
	}
	d.mu.Lock()
	next := d.cfg
	p := next.payments()
	var changes []change
	for _, f := range u.fields(&p) {
		if f.set != nil && *f.set != *f.cur {
			changes = append(changes, change{f.key, *f.cur, *f.set})
			*f.cur = *f.set
		}
	}
	if len(changes) == 0 {
		d.mu.Unlock()
		return p, nil, nil
	}
	prev := d.cfg.Payments
	next.Payments = &p
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
		return d.config().payments(), nil, err
	}
	keys := make([]string, 0, len(changes))
	for _, c := range changes {
		keys = append(keys, "payments."+c.key)
		d.recordPolicyChange("payments."+c.key, c.from, c.to, nil)
	}
	return p, keys, nil
}

// hPaymentLimits reports the payments block, and changes the limits the
// request names. A key it does not know is refused rather than ignored: a
// caller that asked for a change and was told 200 must have got it.
func (d *Daemon) hPaymentLimits(w http.ResponseWriter, r *http.Request) {
	var req PaymentLimits
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	p, changed, err := d.SetPaymentLimits(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if changed == nil {
		changed = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"payments": p, "changed": changed})
}
