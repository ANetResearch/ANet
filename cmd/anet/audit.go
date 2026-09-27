package main

// audit.go holds `anet audit` and `anet verify --chain` (A2A-DESIGN §14).
//
// `anet audit` reads this node's evidence chain from its data directory —
// verified record by record against the node's key history, with or without
// a running daemon — and shows it with four rules:
//
//   - receipt_verified=false is shown as "could not verify (未能核验)": the
//     receipt did not check out or could not be checked, which is not the
//     same as forged and is never shown as fine;
//   - a capability effect counts as a success only with status OK; an
//     UNVERIFIED effect is listed and not counted;
//   - an event type this build does not know is listed as it is, payload
//     and all, never dropped;
//   - every section names its source, and every event names where the fact
//     it records came from (this node, a peer, the hub).
//
// --export DIR writes the chain as stored, the key history it verifies
// under, a manifest and the audit itself; `anet verify --chain DIR` checks
// such a directory with no daemon and no data directory.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// Where the fact an event records came from.
const (
	srcNode = "this node"                         // its own action, signed by it
	srcPeer = "peer, checked by this node"        // content from a peer; the verdict is this node's
	srcHub  = "hub"                               // a hub's statement
	srcAny  = "as recorded (type not known here)" // unknown event type
)

// Verdicts.
const (
	verdictVerified   = "verified"
	verdictUnverified = "unverified"
	verdictUnknown    = "unknown"
)

// verdictText is how a verdict is shown to a person.
func verdictText(v string) string {
	switch v {
	case verdictVerified:
		return "verified"
	case verdictUnverified:
		return "could not verify (未能核验)"
	}
	return "not recorded"
}

// eventKind describes a known event type.
type eventKind struct {
	label  string
	source string
}

var knownEvents = map[string]eventKind{
	"anet.delegation.sent":            {"delegation sent", srcNode},
	"anet.delegation.received":        {"delegation received", srcPeer},
	"anet.delegation.refused_summary": {"delegations refused (aggregated)", srcNode},
	"anet.result.accepted":            {"result accepted", srcPeer},
	"anet.interaction.receipt":        {"receipt issued", srcNode},
	"anet.capability.effect":          {"capability executed", srcNode},
	"anet.payment.settled":            {"payment settled", srcHub},
	"anet.payment.authorized":         {"payment authorized", srcNode},
	"anet.payment.quoted":             {"payment quoted", srcNode},
	"anet.credit.redeemed":            {"credit redeemed", srcHub},
	"anet.voucher.redeemed":           {"voucher redeemed", srcHub},
	"anet.voucher.refused":            {"voucher refused", srcNode},
	"anet.issuance.head_seen":         {"hub issuance head seen", srcHub},
	"anet.shell.command":              {"shell command run", srcNode},
	"anet.shell.refused":              {"shell command refused", srcNode},
	"anet.policy.changed":             {"policy changed", srcNode},
	"anet.autoreply.invoked":          {"auto-reply invoked", srcNode},
	"anet.backend.forwarded":          {"forwarded to A2A backend", srcNode},
	"anet.delivery.expired":           {"delivery expired", srcNode},
	"anet.evidence.gap":               {"evidence gap (a torn record was lost)", srcNode},
	"anet.message.sent":               {"message sent", srcNode},
	"anet.message.received":           {"message received", srcPeer},
}

type auditEvent struct {
	Seq    uint64 `json:"seq"`
	ID     string `json:"id"`
	Time   string `json:"time"`
	Type   string `json:"type"`
	Known  bool   `json:"known"`
	Label  string `json:"label"`
	Source string `json:"source"`
	// Verdict is this node's check of what the event reports, where it has
	// one: a result's receipt, a settlement's hub receipt.
	Verdict string `json:"verdict,omitempty"`
	// Status and Success are set for a capability effect; Success is true
	// only for status OK.
	Status  string `json:"status,omitempty"`
	Success *bool  `json:"success,omitempty"`
	// Flag marks what an auditor has to look at: a second verified
	// settlement for one interaction (A2A-DESIGN §8.3).
	Flag    string         `json:"flag,omitempty"`
	Payload map[string]any `json:"payload"`
}

type auditSummary struct {
	Source                   string         `json:"source"`
	Events                   int            `json:"events"`
	CapabilityByStatus       map[string]int `json:"capability_calls_by_status"`
	CapabilitySucceeded      int            `json:"capability_calls_succeeded"`
	ResultsAccepted          int            `json:"results_accepted"`
	ResultsReceiptVerified   int            `json:"results_receipt_verified"`
	ResultsReceiptUnverified int            `json:"results_receipt_unverified"`
	PaymentsSettled          int            `json:"payments_settled"`
	PaymentsVerified         int            `json:"payments_hub_receipt_verified"`
	PaymentsUnverified       int            `json:"payments_hub_receipt_unverified"`
	PaymentsProviderSide     int            `json:"payments_received_hub_response"`
	PaymentsRepeated         int            `json:"payments_repeated_for_interaction"`
	PolicyChanges            int            `json:"policy_changes"`
	Unknown                  int            `json:"unknown_events"`
}

type auditReport struct {
	Source struct {
		Kind     string `json:"kind"`
		Path     string `json:"path"`
		Verified bool   `json:"verified"`
		Detail   string `json:"detail"`
	} `json:"source"`
	Chain struct {
		ChainDID  string `json:"chain_did"`
		SignerAID string `json:"signer_aid"`
		HeadID    string `json:"head_id"`
		Length    uint64 `json:"length"`
		State     string `json:"state"`
		TornTail  bool   `json:"torn_tail"`
	} `json:"chain"`
	Filter  map[string]string `json:"filter"`
	Events  []auditEvent      `json:"events"`
	Summary auditSummary      `json:"summary"`
}

type auditFilter struct {
	since       int64 // unix ms; 0 = none
	peer        string
	interaction string
	// peerIX are the interactions whose records name peer; buildAudit
	// fills it, so that a record naming only the interaction (a result
	// accepted, a settlement) is kept too.
	peerIX map[string]bool
}

// runAudit is `anet audit [--since D] [--peer AID] [--interaction ID]
// [--json] [--export DIR]`.
func runAudit(layout daemon.Layout, rest []string) error {
	pos, flags := splitFlags(rest)
	if len(pos) > 0 {
		return fmt.Errorf("audit [--since 24h|2006-01-02|RFC3339] [--peer AID] [--interaction ID] [--json] [--export DIR]\n" +
			"       audit hub    (the hub's issuance chain; same as audit-hub)")
	}
	var f auditFilter
	fl := map[string]string{}
	if v := strings.TrimSpace(flags["since"]); v != "" && v != "true" {
		ms, err := parseSince(v, time.Now())
		if err != nil {
			return err
		}
		f.since, fl["since"] = ms, time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	if v := strings.TrimSpace(flags["peer"]); v != "" && v != "true" {
		f.peer, fl["peer"] = v, v
	}
	if v := strings.TrimSpace(flags["interaction"]); v != "" && v != "true" {
		f.interaction, fl["interaction"] = v, v
	}
	ch, err := daemon.ReadEvidenceChain(layout)
	if err != nil {
		return err
	}
	rep := buildAudit(ch, layout.EvidenceLedgerPath(), f)
	rep.Filter = fl
	if dir := flags["export"]; dir != "" && dir != "true" {
		if err := exportChain(dir, ch, rep); err != nil {
			return err
		}
		if flags["json"] != "true" {
			defer fmt.Printf("\nexported the whole chain (%d records) to %s — check it with: anet verify --chain %s\n",
				ch.Head.Length, dir, dir)
		}
	}
	if flags["json"] == "true" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	renderAudit(os.Stdout, rep)
	return nil
}

// parseSince reads --since: a duration back from now ("24h", "7d"), a date
// ("2006-01-02", local time) or an RFC 3339 time.
func parseSince(v string, now time.Time) (int64, error) {
	if strings.HasSuffix(v, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil && n >= 0 {
			return now.Add(-time.Duration(n) * 24 * time.Hour).UnixMilli(), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return now.Add(-d).UnixMilli(), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		return t.UnixMilli(), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli(), nil
	}
	return 0, fmt.Errorf("audit: --since takes a duration (24h, 7d), a date (2006-01-02) or an RFC 3339 time, not %q", v)
}

// matches applies the filter to one record.
func (f auditFilter) matches(r daemon.EvidenceRecord) bool {
	if f.since > 0 && r.Timestamp < f.since {
		return false
	}
	if f.interaction != "" && !namesInteraction(r, f.interaction) {
		return false
	}
	if f.peer != "" && !payloadMentions(r.Payload, f.peer) {
		ix, _ := r.Payload["interaction_id"].(string)
		if ix == "" || !f.peerIX[ix] {
			return false
		}
	}
	return true
}

// namesInteraction reports whether a record is about interaction ix: its
// interaction_id, the interactions a deny canceled or left running because
// they were paid for, or the held delegation an approval or rejection
// decided (anet.policy.changed field inbound.pending names it as "from").
func namesInteraction(r daemon.EvidenceRecord, ix string) bool {
	if payloadMentions(r.Payload, ix, "interaction_id", "canceled", "skipped_paid") {
		return true
	}
	return r.EventType == "anet.policy.changed" && r.Payload["field"] == "inbound.pending" && r.Payload["from"] == ix
}

// peerInteractions collects the interactions the records that name peer
// belong to.
func peerInteractions(recs []daemon.EvidenceRecord, peer string) map[string]bool {
	out := map[string]bool{}
	for _, r := range recs {
		if !payloadMentions(r.Payload, peer) {
			continue
		}
		if ix, _ := r.Payload["interaction_id"].(string); ix != "" {
			out[ix] = true
		}
		for _, key := range []string{"canceled", "skipped_paid"} {
			l, _ := r.Payload[key].([]any)
			for _, e := range l {
				if ix, _ := e.(string); ix != "" {
					out[ix] = true
				}
			}
		}
	}
	return out
}

// payloadMentions reports whether a top-level string value, or a string in
// a top-level list, equals want. With keys, only those keys are looked at.
func payloadMentions(p map[string]any, want string, keys ...string) bool {
	look := func(v any) bool {
		switch t := v.(type) {
		case string:
			return t == want
		case []any:
			for _, e := range t {
				if s, ok := e.(string); ok && s == want {
					return true
				}
			}
		}
		return false
	}
	if len(keys) > 0 {
		for _, k := range keys {
			if look(p[k]) {
				return true
			}
		}
		return false
	}
	for _, v := range p {
		if look(v) {
			return true
		}
	}
	return false
}

func buildAudit(ch *daemon.EvidenceChain, path string, f auditFilter) *auditReport {
	rep := &auditReport{}
	rep.Source.Kind = "local_evidence_chain"
	rep.Source.Path = path
	rep.Source.Verified = true
	rep.Source.Detail = "this node's evidence chain on disk; every record's id, signature and link to the previous one " +
		"verified against the key history of " + ch.SignerAID
	rep.Chain.ChainDID, rep.Chain.SignerAID = ch.Head.ChainDID, ch.SignerAID
	rep.Chain.HeadID, rep.Chain.Length, rep.Chain.State = ch.Head.HeadID, ch.Head.Length, ch.Head.State
	rep.Chain.TornTail = ch.TornTail
	rep.Events = []auditEvent{}
	s := &rep.Summary
	s.Source = "counted here from the events listed"
	s.CapabilityByStatus = map[string]int{}
	if f.peer != "" {
		f.peerIX = peerInteractions(ch.Records, f.peer)
	}
	// A second verified settlement for one interaction is flagged (§8.3),
	// counted over the whole chain so that a filter cannot hide which one
	// came first.
	settled := map[string]int{}
	for _, r := range ch.Records {
		ev := classify(r)
		repeated := repeatedSettlement(ev, settled)
		if !f.matches(r) {
			continue
		}
		if repeated {
			ev.Flag = "a second verified settlement for this interaction (A2A-DESIGN §8.3)"
			s.PaymentsRepeated++
		}
		rep.Events = append(rep.Events, ev)
		s.Events++
		switch r.EventType {
		case "anet.capability.effect":
			s.CapabilityByStatus[ev.Status]++
			if ev.Success != nil && *ev.Success {
				s.CapabilitySucceeded++
			}
		case "anet.result.accepted":
			s.ResultsAccepted++
			switch ev.Verdict {
			case verdictVerified:
				s.ResultsReceiptVerified++
			default:
				s.ResultsReceiptUnverified++
			}
		case "anet.payment.settled":
			s.PaymentsSettled++
			switch ev.Verdict {
			case verdictVerified:
				s.PaymentsVerified++
			case verdictUnverified:
				s.PaymentsUnverified++
			default:
				s.PaymentsProviderSide++
			}
		case "anet.policy.changed":
			s.PolicyChanges++
		}
		if !ev.Known {
			s.Unknown++
		}
	}
	return rep
}

// classify applies the display rules to one record.
func classify(r daemon.EvidenceRecord) auditEvent {
	ev := auditEvent{Seq: r.Seq, ID: r.ID, Type: r.EventType, Payload: r.Payload,
		Time: time.UnixMilli(r.Timestamp).UTC().Format(time.RFC3339)}
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	k, known := knownEvents[r.EventType]
	ev.Known = known
	if !known {
		ev.Label, ev.Source = r.EventType, srcAny
		return ev
	}
	ev.Label, ev.Source = k.label, k.source
	p := ev.Payload
	switch r.EventType {
	case "anet.result.accepted":
		ev.Verdict = boolVerdict(p["receipt_verified"])
	case "anet.payment.settled":
		// The payer's record carries its own check of the hub's signed
		// receipt; the payee's record is the hub's settlement response.
		if _, ok := p["verified"]; ok {
			ev.Label, ev.Source = "payment settled (paid)", "hub receipt, checked by this node"
			ev.Verdict = boolVerdict(p["verified"])
		} else {
			ev.Label, ev.Source = "payment settled (received)", "hub settlement response"
		}
	case "anet.capability.effect":
		st, _ := p["status"].(string)
		if st == "" {
			st = "(none)"
		}
		ok := st == "OK"
		ev.Status, ev.Success = st, &ok
	case "anet.delegation.received":
		if agg, _ := p["aggregated"].(bool); agg {
			ev.Label = "delegations received (aggregated)"
		}
	}
	return ev
}

// repeatedSettlement counts a verified settlement of the paying side
// against its interaction, and reports whether one was counted before.
func repeatedSettlement(ev auditEvent, seen map[string]int) bool {
	if ev.Type != "anet.payment.settled" || ev.Verdict != verdictVerified {
		return false
	}
	ix, _ := ev.Payload["interaction_id"].(string)
	if ix == "" {
		return false
	}
	seen[ix]++
	return seen[ix] > 1
}

func boolVerdict(v any) string {
	switch t := v.(type) {
	case bool:
		if t {
			return verdictVerified
		}
		return verdictUnverified
	case string:
		switch t {
		case "verified":
			return verdictVerified
		case "unverified":
			return verdictUnverified
		}
	}
	return verdictUnknown
}

// fields renders a payload as sorted key=value pairs, long values cut.
func fields(p map[string]any, skip ...string) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		if !contains(skip, k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := compactJSON(p[k])
		if s, ok := p[k].(string); ok {
			v = s
		}
		parts = append(parts, k+"="+printable(v, 72))
	}
	return strings.Join(parts, " ")
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func renderAudit(w io.Writer, rep *auditReport) {
	c := rep.Chain
	fmt.Fprintf(w, "Evidence chain %s\n", c.ChainDID)
	fmt.Fprintf(w, "  source: %s\n  %s\n", rep.Source.Path, rep.Source.Detail)
	fmt.Fprintf(w, "  %d records, head %s, %s\n", c.Length, shortID(c.HeadID), c.State)
	if c.TornTail {
		fmt.Fprintln(w, "  the file's last line is incomplete (being written, or torn by a crash) and is not included")
	}
	if len(rep.Filter) > 0 {
		keys := make([]string, 0, len(rep.Filter))
		for k := range rep.Filter {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var fs []string
		for _, k := range keys {
			fs = append(fs, k+"="+rep.Filter[k])
		}
		fmt.Fprintf(w, "  filter: %s\n", strings.Join(fs, " "))
	}

	fmt.Fprintln(w, "\nEvents — source: records this node signed; [brackets] say where each recorded fact came from")
	if len(rep.Events) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, ev := range rep.Events {
		head := fmt.Sprintf("  #%-5d %s  %-34s", ev.Seq, ev.Time, printable(ev.Label, 34))
		switch {
		case !ev.Known:
			fmt.Fprintf(w, "%s unknown event, as recorded: %s\n", head, printable(compactJSON(ev.Payload), 400))
			continue
		case ev.Type == "anet.capability.effect":
			note := ""
			if ev.Success != nil && !*ev.Success {
				note = " (not counted as success)"
			}
			fmt.Fprintf(w, "%s status %s%s  %s  [%s]\n", head, printable(ev.Status, 40), note,
				fields(ev.Payload, "status", "metrics", "evidence"), ev.Source)
		case ev.Verdict != "":
			what := "receipt"
			if ev.Type == "anet.payment.settled" {
				what = "hub receipt"
			}
			fmt.Fprintf(w, "%s %s: %s  %s  [%s]\n", head, what, verdictText(ev.Verdict),
				fields(ev.Payload, "receipt_verified", "verified", "receipt"), ev.Source)
		default:
			fmt.Fprintf(w, "%s %s  [%s]\n", head, fields(ev.Payload, "receipt", "evidence"), ev.Source)
		}
		if ev.Flag != "" {
			fmt.Fprintf(w, "         !! %s\n", ev.Flag)
		}
	}

	s := rep.Summary
	fmt.Fprintf(w, "\nSummary — source: %s (%d events)\n", s.Source, s.Events)
	if len(s.CapabilityByStatus) > 0 {
		var parts []string
		for _, st := range sortedStatus(s.CapabilityByStatus) {
			parts = append(parts, fmt.Sprintf("%s %d", st, s.CapabilityByStatus[st]))
		}
		fmt.Fprintf(w, "  capability calls   succeeded (OK) %d of %d  [%s]; UNVERIFIED is not counted as success\n",
			s.CapabilitySucceeded, sum(s.CapabilityByStatus), strings.Join(parts, ", "))
	}
	if s.ResultsAccepted > 0 {
		fmt.Fprintf(w, "  results accepted   %d: receipt verified %d, %s %d\n", s.ResultsAccepted,
			s.ResultsReceiptVerified, verdictText(verdictUnverified), s.ResultsReceiptUnverified)
	}
	if s.PaymentsSettled > 0 {
		fmt.Fprintf(w, "  payments settled   %d: paid with hub receipt verified %d, %s %d; received (hub response) %d\n",
			s.PaymentsSettled, s.PaymentsVerified, verdictText(verdictUnverified), s.PaymentsUnverified, s.PaymentsProviderSide)
		if s.PaymentsRepeated > 0 {
			fmt.Fprintf(w, "  !! %d verified settlement(s) repeat an interaction already paid (marked above)\n", s.PaymentsRepeated)
		}
	}
	fmt.Fprintf(w, "  policy changes     %d\n", s.PolicyChanges)
	if s.Unknown > 0 {
		fmt.Fprintf(w, "  unknown events     %d (listed as recorded)\n", s.Unknown)
	}
}

func sortedStatus(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func shortID(id string) string {
	if id == "" {
		return "(none)"
	}
	if len(id) > 20 {
		return id[:20] + "…"
	}
	return id
}

// Files of an export directory.
const (
	exportChainFile    = "evidence.ael.jsonl"
	exportKELFile      = "kel.b64"
	exportManifestFile = "manifest.json"
	exportAuditFile    = "audit.json"
	exportFormat       = "anet.evidence-export/1"
)

type exportManifest struct {
	Format      string            `json:"format"`
	ChainDID    string            `json:"chain_did"`
	SignerAID   string            `json:"signer_aid"`
	HeadID      string            `json:"head_id"`
	Length      uint64            `json:"length"`
	State       string            `json:"state"`
	ExportedAt  string            `json:"exported_at"`
	AnetVersion string            `json:"anet_version"`
	SHA256      map[string]string `json:"sha256"`
	Note        string            `json:"note"`
}

// exportChain writes the whole chain (filters apply to the audit only) to
// dir. It refuses to overwrite: an export is evidence, and one silently
// replaced by another is not.
func exportChain(dir string, ch *daemon.EvidenceChain, rep *auditReport) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("audit --export: %w", err)
	}
	var chain []byte
	for _, l := range ch.Lines {
		chain = append(chain, l...)
		chain = append(chain, '\n')
	}
	kel := []byte(base64.StdEncoding.EncodeToString(ch.KEL) + "\n")
	auditJSON, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	digest := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	man := exportManifest{Format: exportFormat, ChainDID: ch.Head.ChainDID, SignerAID: ch.SignerAID,
		HeadID: ch.Head.HeadID, Length: ch.Head.Length, State: ch.Head.State,
		ExportedAt: time.Now().UTC().Format(time.RFC3339), AnetVersion: daemon.Version,
		SHA256: map[string]string{exportChainFile: digest(chain), exportKELFile: digest(kel)},
		Note: "evidence.ael.jsonl is the chain as stored (base64 CoreDet-CBOR, one record per line); " +
			"kel.b64 is the signer's key history; audit.json is a rendering and is not checked. " +
			"Check with: anet verify --chain <dir>"}
	manJSON, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
	}{
		{exportChainFile, chain}, {exportKELFile, kel}, {exportManifestFile, append(manJSON, '\n')},
		{exportAuditFile, append(auditJSON, '\n')},
	} {
		if err := writeNew(filepath.Join(dir, f.name), f.data); err != nil {
			return fmt.Errorf("audit --export: %w", err)
		}
	}
	return nil
}

// writeNew creates path (0600) and fails if it exists.
func writeNew(path string, data []byte) error {
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; export into a new directory", path)
		}
		return err
	}
	if _, err := fh.Write(data); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

// verifyChain is `anet verify --chain DIR [--kel B64 | --hub URL] [--head
// ID]`: check an exported chain with nothing but the directory.
//
// What passing means, printed with the result: every record was signed by
// the key history's AID, re-derives its id, and links to the one before it
// back to genesis. It does not mean nothing was cut off the end — a chain
// that stops early is still a valid chain — unless --head names a record
// the reader learned elsewhere (a witness, an earlier export) and it is
// found. And a key history taken from the directory itself proves the
// chain belongs to the AID it names, not that the AID is who you think.
func verifyChain(dir, kelB64, hubURL, head string) error {
	chainBytes, err := os.ReadFile(filepath.Join(dir, exportChainFile))
	if err != nil {
		return fmt.Errorf("verify --chain: %w", err)
	}
	var man *exportManifest
	if b, err := os.ReadFile(filepath.Join(dir, exportManifestFile)); err == nil {
		man = &exportManifest{}
		if err := json.Unmarshal(b, man); err != nil {
			return fmt.Errorf("verify --chain: manifest.json: %w", err)
		}
	}
	kelFrom := ""
	switch {
	case kelB64 != "":
		kelFrom = "the key history given with --kel"
	case hubURL != "":
		if man == nil || man.SignerAID == "" {
			return fmt.Errorf("verify --chain: --hub needs manifest.json to know whose key history to fetch; pass --kel instead")
		}
		if printable(man.SignerAID, 256) != man.SignerAID || strings.ContainsAny(man.SignerAID, " /") {
			return fmt.Errorf("verify --chain: manifest.json names %q as the signer, which is not an AID", printable(man.SignerAID, 80))
		}
		if kelB64, err = fetchKELFor(hubURL, man.SignerAID); err != nil {
			return err
		}
		kelFrom = "the key history " + hubURL + " publishes for " + man.SignerAID
	default:
		b, err := os.ReadFile(filepath.Join(dir, exportKELFile))
		if err != nil {
			return fmt.Errorf("verify --chain: no --kel or --hub given and %w", err)
		}
		kelB64 = string(b)
		kelFrom = "the key history in the export itself (" + exportKELFile + ")"
	}
	kb, err := base64.StdEncoding.DecodeString(strings.TrimSpace(kelB64))
	if err != nil {
		return fmt.Errorf("verify --chain: key history is not base64: %w", err)
	}
	kel, err := identity.UnmarshalKEL(kb)
	if err != nil {
		return fmt.Errorf("verify --chain: undecodable key history: %w", err)
	}
	// An export is someone else's file: what it says is printed through
	// printable, so it cannot rewrite the verdict on the terminal.
	ch, err := daemon.VerifyEvidenceLines(daemon.SplitEvidenceLines(chainBytes), kel)
	if err != nil {
		fmt.Printf("✗ the chain does NOT verify: %s\n", printable(err.Error(), 400))
		return errQuiet
	}
	fmt.Printf("✓ %d records verify under %s\n", ch.Head.Length, ch.SignerAID)
	fmt.Printf("  chain  %s\n  head   %s\n  state  %s\n", ch.Head.ChainDID, ch.Head.HeadID, ch.Head.State)
	fmt.Printf("  keys   %s\n", kelFrom)
	bad := false
	if man != nil {
		h := sha256.Sum256(chainBytes)
		if want := man.SHA256[exportChainFile]; want != "" && want != hex.EncodeToString(h[:]) {
			fmt.Printf("✗ %s does not match the sha256 in manifest.json\n", exportChainFile)
			bad = true
		}
		if man.HeadID != ch.Head.HeadID || man.Length != ch.Head.Length || man.ChainDID != ch.Head.ChainDID {
			fmt.Printf("✗ manifest.json says head %s at length %d on %s; the chain has head %s at length %d\n",
				printable(man.HeadID, 80), man.Length, printable(man.ChainDID, 120), ch.Head.HeadID, ch.Head.Length)
			bad = true
		}
	}
	if head != "" {
		found := -1
		for i, r := range ch.Records {
			if r.ID == head {
				found = i
			}
		}
		if found < 0 {
			fmt.Printf("✗ the chain does not contain %s: it was cut short or is a different chain\n", printable(head, 80))
			bad = true
		} else {
			fmt.Printf("✓ contains the given head %s at seq %d (%d records after it)\n", shortID(head),
				ch.Records[found].Seq, len(ch.Records)-found-1)
		}
	}
	if ch.Head.State != "" && ch.Head.State != "ACTIVE" {
		fmt.Printf("✗ the chain is %s, not ACTIVE\n", ch.Head.State)
		bad = true
	}
	if bad {
		return errQuiet
	}
	fmt.Println("\n  this shows every record was signed under that key history and none was altered,")
	fmt.Println("  removed or reordered before the head. A chain cut short after some record is still")
	fmt.Println("  a valid chain: pass --head <id> with a head you learned elsewhere to rule that out.")
	if kelFrom == "the key history in the export itself ("+exportKELFile+")" {
		fmt.Println("  the key history came with the export: compare the AID above with the one you expect,")
		fmt.Println("  or pass --hub <url> to fetch it from a hub.")
	}
	return nil
}
