package main

// audit_hub.go holds the human lines of `anet audit-hub` and `anet balance`
// (HPP §5.2 texts F and G): the JSON of the answer goes to stdout as for
// every control route, and the sentences of the reason table to stderr.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// auditHubAnswer is the part of the payment module's AuditReport the lines
// are made from.
type auditHubAnswer struct {
	Epochs   int  `json:"epochs"`
	ChainOK  bool `json:"chain_ok"`
	ProofsOK int  `json:"proofs_ok"`
	Latest   *struct {
		ToDay   uint64 `json:"to_day"`
		Balance uint64 `json:"balance"`
	} `json:"latest"`
	Findings []struct {
		Code  string         `json:"code"`
		Level string         `json:"level"`
		Args  map[string]any `json:"args"`
	} `json:"findings"`
	Verified bool `json:"verified"`
}

const msPerDay = 86_400_000

// runAuditHub is `anet audit-hub`: the JSON, then the lines of text F, and
// exit status 1 when a check failed. A hint that is not a failure (periods
// that could not be checked in time) does not change the status.
func runAuditHub(c *client) error {
	out, code, err := c.fetch("/audit-hub", map[string]any{})
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if err := printAnswer(out, code, false); err != nil {
		return err
	}
	var a auditHubAnswer
	if json.Unmarshal(out, &a) != nil {
		return nil
	}
	lines := auditHubLines(a, a2ashape.Lang(), time.Now())
	for _, l := range lines {
		fmt.Fprintln(errW(), l)
	}
	if !a.Verified {
		return errExit1
	}
	return nil
}

// auditHubLines are text F: the chain line, the proof line, then each
// finding with its command, or "no problems".
func auditHubLines(a auditHubAnswer, lang a2ashape.Language, now time.Time) []string {
	var lines []string
	if a.ChainOK && a.Epochs > 0 {
		lines = append(lines, a2ashape.Sentence("audit.chain_ok", lang, a2ashape.Args{"n": a.Epochs}))
	}
	if a.ProofsOK > 0 && a.Latest != nil {
		lines = append(lines, a2ashape.Sentence("audit.proofs_ok", lang, a2ashape.Args{
			"n": a.ProofsOK, "time": time.UnixMilli(int64(a.Latest.ToDay+1) * msPerDay).UTC(), "amount": a.Latest.Balance}))
	}
	if len(a.Findings) == 0 {
		return append(lines, a2ashape.Sentence("audit.none", lang, nil))
	}
	for _, f := range a.Findings {
		args := findingArgs(f.Args)
		lines = append(lines, oneLine(a2ashape.Sentence(f.Code, lang, args), maxLineRunes))
		if n := a2ashape.Next(f.Code, lang, args); n != "" {
			lines = append(lines, nextPrefix+n)
		}
	}
	return lines
}

// findingArgs makes the placeholders of a finding from its JSON: "time" is
// unix milliseconds, every other value is shown as it is.
func findingArgs(in map[string]any) a2ashape.Args {
	args := a2ashape.Args{}
	for k, v := range in {
		switch x := v.(type) {
		case float64:
			if k == "time" {
				args[k] = time.UnixMilli(int64(x)).UTC()
			} else {
				args[k] = fmt.Sprintf("%.0f", x)
			}
		case json.Number:
			if ms, err := x.Int64(); err == nil && k == "time" {
				args[k] = time.UnixMilli(ms).UTC()
			} else {
				args[k] = x.String()
			}
		case string:
			args[k] = x
		default:
			args[k] = fmt.Sprint(x)
		}
	}
	return args
}

// runBalance is `anet balance`: the JSON, then text G, the balance and the
// state of a new account.
func runBalance(c *client) error {
	out, code, err := c.fetch("/balance", map[string]any{})
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if err := printAnswer(out, code, false); err != nil {
		return err
	}
	for _, l := range balanceLines(out, a2ashape.Lang(), time.Now()) {
		fmt.Fprintln(errW(), l)
	}
	return nil
}

// balanceLines are text G. The second line is there only for an account
// that came in through open registration or a sign-up window.
func balanceLines(out []byte, lang a2ashape.Language, now time.Time) []string {
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var b struct {
		Hub            string         `json:"hub"`
		Balance        any            `json:"balance"`
		ProbationUntil json.Number    `json:"probation_until"`
		Trial          string         `json:"trial"`
		Entries        []balanceEntry `json:"entries"`
	}
	if dec.Decode(&b) != nil {
		return nil
	}
	var lines []string
	// The balance is a number, or "module_absent" without a payment module.
	if n, ok := b.Balance.(json.Number); ok {
		if _, err := n.Int64(); err == nil {
			lines = append(lines, a2ashape.Sentence("balance.summary", lang, a2ashape.Args{"amount": n.String(), "hub": b.Hub}))
		}
	}
	until, _ := b.ProbationUntil.Int64()
	switch {
	case until > now.UnixMilli():
		lines = append(lines, a2ashape.Sentence("balance.probation", lang, a2ashape.Args{"time": time.UnixMilli(until).UTC()}))
	case b.Trial == "pending":
		lines = append(lines, a2ashape.Sentence("balance.trial_pending", lang, nil))
	case b.Trial == "granted":
		// When the credits arrived is on the ledger, while the ledger still
		// has it; later the line has nothing to say.
		for _, e := range b.Entries {
			if strings.EqualFold(e.Reason, "trial credit") && e.Delta > 0 {
				if t, err := time.Parse(time.RFC3339Nano, e.At); err == nil {
					lines = append(lines, a2ashape.Sentence("balance.trial_granted", lang,
						a2ashape.Args{"amount": fmt.Sprint(e.Delta), "time": t.UTC()}))
				}
				break
			}
		}
	}
	return lines
}

type balanceEntry struct {
	Delta  int64  `json:"delta"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}
