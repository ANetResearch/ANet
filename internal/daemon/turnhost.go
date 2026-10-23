package daemon

// turnhost.go — the kernel side of the auto-reply seams (MOD §4, K-6, K-7, K-8). The auto-reply
// module answers conversations through module.TurnHost, changes its own configuration block through
// module.ModuleConfigHost and reads its own evidence back through module.OwnEvidenceHost. What stays
// here is what only one place may decide: which turns are owed (OwedTurns), every send and its
// re-check (SendTurn and the others), the reasons a turn may be refused with, the write of
// config.json and its audit (anet.policy.changed), and the evidence ledger.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// turnPart is the module.TurnHost grant.
type turnPart struct{ d *Daemon }

// evidencePart is the module.OwnEvidenceHost grant, bound to the module's name.
type evidencePart struct {
	d    *Daemon
	name string
}

// configPart is the module.ModuleConfigHost grant, bound to the module's name.
type configPart struct {
	d    *Daemon
	name string
}

var (
	_ module.TurnHost         = turnPart{}
	_ module.OwnEvidenceHost  = evidencePart{}
	_ module.ModuleConfigHost = configPart{}
)

// kickTurns wakes whatever waits for an owed turn (the auto-reply module, through TurnHost.Wake) and
// the modules' A2A backends. It never blocks, and bursts coalesce.
func (d *Daemon) kickTurns() {
	select {
	case d.turnWake <- struct{}{}:
	default:
	}
	d.inFeed.wake() // and the modules' A2A backends (inbound_tasks.go)
}

// Wake implements module.TurnHost.
func (p turnPart) Wake() <-chan struct{} { return p.d.turnWake }

// OwedTurns implements module.TurnHost. The turns come from the active interactions (capability calls
// and public_cap ones are not among them); the kernel drops the ones an A2A backend takes, the ones of
// a peer on the deny list, and the ones whose last text message is ours. Before the daemon has finished
// starting there are none.
func (p turnPart) OwedTurns(ctx context.Context) ([]module.Turn, error) {
	d := p.d
	select {
	case <-d.ready:
	default:
		return nil, nil
	}
	if d.ix == nil {
		return nil, errors.New("anet: the store is closed")
	}
	threads, err := d.ActiveThreads()
	if err != nil {
		return nil, err
	}
	ps := d.readPeers()
	out := make([]module.Turn, 0, len(threads))
	for _, th := range threads {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if t, ok := d.owedTurn(th, ps); ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// owedTurn is th as a turn, and whether a reply to it is owed now (both directions).
func (d *Daemon) owedTurn(th Thread, ps peerSets) (module.Turn, bool) {
	if interactions.State(th.State).IsTerminal() || th.IsCapability || th.Trust == interactions.TrustPublicCap {
		return module.Turn{}, false
	}
	if ps.denied(th.Peer) || d.backendAnswers(th) {
		return module.Turn{}, false
	}
	t := turnOf(th, ps.trusted(th.Peer))
	if len(t.Messages) == 0 || t.Messages[len(t.Messages)-1].From != "them" {
		return module.Turn{}, false // the last word is ours, or there is no content yet
	}
	if th.Role == string(interactions.RoleInbound) {
		// Decided at every owed turn (design v0.3.9 §A): a member the
		// admission service removed is not admitted at the next one. A
		// member on the allow list is asked too (admittedTurn).
		t.Admitted = d.admittedTurn(th.Peer, th.IsCapability, ps)
	}
	return t, true
}

// turnOf renders a thread as a turn: the text messages without the control messages.
func turnOf(th Thread, trusted bool) module.Turn {
	t := module.Turn{Ix: th.InteractionID, Role: th.Role, Peer: th.Peer, Trusted: trusted, State: th.State,
		Goal: th.Goal, EndReqBy: th.EndReqBy, MsgCount: len(th.Messages)}
	for _, m := range th.Messages {
		if m.From == "them" {
			t.PeerMsgCount++
		}
		if m.Kind != interactions.MsgText || (m.Body == "" && len(m.Attachments) == 0) || hasControlMeta(string(m.Metadata)) {
			continue
		}
		tm := module.TurnMessage{From: m.From, Body: m.Body}
		for _, a := range m.Attachments {
			tm.Attachments = append(tm.Attachments, module.TurnAttachment{CID: a.CID, Name: a.Name, Mime: a.Mime, Size: a.Size})
		}
		t.Messages = append(t.Messages, tm)
	}
	return t
}

// turnCheck re-decides at the seam whether ix is still a turn the module may act on: the interaction is
// there and not ended, is not a capability call, and its peer is not denied. owed adds the condition
// that the last text message is still the peer's (a reply is still owed). The module's say-so is never
// taken for any of it (MOD-F7).
func (d *Daemon) turnCheck(ix string, owed bool) (Thread, error) {
	if d.ix == nil {
		return Thread{}, module.ErrTurnGone
	}
	th, err := d.Thread(ix)
	if err != nil {
		return Thread{}, fmt.Errorf("%w: %v", module.ErrTurnGone, err)
	}
	ps := d.readPeers()
	if interactions.State(th.State).IsTerminal() || th.IsCapability || th.Trust == interactions.TrustPublicCap || ps.denied(th.Peer) {
		return Thread{}, module.ErrTurnGone
	}
	if owed {
		if _, ok := d.owedTurn(th, ps); !ok {
			return Thread{}, module.ErrTurnGone
		}
	}
	return th, nil
}

// SendTurn implements module.TurnHost.
func (p turnPart) SendTurn(ctx context.Context, ix, text string, atts []module.Attachment, final bool) error {
	d := p.d
	th, err := d.turnCheck(ix, true)
	if err != nil {
		return err
	}
	da := make([]delegation.Attachment, 0, len(atts))
	for _, a := range atts {
		cid, err := anetcid.SumRaw(a.Data)
		if err != nil {
			return err
		}
		da = append(da, delegation.Attachment{Name: a.Name, Mime: a.Mime, Size: int64(len(a.Data)), CID: cid, Data: a.Data})
	}
	if final && th.Role == string(interactions.RoleInbound) {
		hctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
		defer cancel()
		return d.completeWithReply(hctx, ix, text, da)
	}
	sctx, cancel := context.WithTimeout(ctx, relayCallTimeout)
	defer cancel()
	return d.SendMessageOpts(sctx, ix, text, da, nil)
}

// RefuseTurn implements module.TurnHost. Only three reasons are taken; anything else is refused here.
func (p turnPart) RefuseTurn(ctx context.Context, ix, reason string, retryAfter time.Duration) error {
	d := p.d
	var state interactions.State
	var kernelReason string
	meta := map[string]any{}
	switch reason {
	case module.ReasonSandboxUnavailable:
		state, kernelReason = interactions.StateRejected, reasonSandboxUnavailble
	case module.ReasonBudget:
		state, kernelReason = interactions.StateRejected, "budget"
		meta[a2ashape.KeyRetryAfterMS] = max(retryAfter.Milliseconds(), 1)
	case module.ReasonBackendFailed:
		state, kernelReason = interactions.StateInputRequired, reasonBackendFailed
	default:
		return module.ErrRefuseReason
	}
	if _, err := d.turnCheck(ix, true); err != nil {
		return err
	}
	meta[a2ashape.KeyReason] = kernelReason
	sctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	return d.SendStatus(sctx, ix, state, "", meta)
}

// WorkingTurn implements module.TurnHost.
func (p turnPart) WorkingTurn(ctx context.Context, ix string) error {
	if _, err := p.d.turnCheck(ix, false); err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	return p.d.SendStatus(sctx, ix, interactions.StateWorking, "", nil)
}

// EndTurn implements module.TurnHost.
func (p turnPart) EndTurn(ctx context.Context, ix string) error {
	if _, err := p.d.turnCheck(ix, false); err != nil {
		return err
	}
	hctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	return p.d.RequestEnd(hctx, ix)
}

// TurnAttachment implements module.TurnHost: the bytes of an attachment of an interaction that is a turn
// (not a capability call, not a public_cap interaction).
func (p turnPart) TurnAttachment(ix, cid string) ([]byte, error) {
	if _, err := p.d.turnCheck(ix, false); err != nil {
		return nil, err
	}
	_, _, data, err := p.d.AttachmentBytes(ix, cid)
	return data, err
}

// PreviewTurn implements module.TurnHost: the sink for the turn's preview, when the preview module is
// available and the round is one that asked for it (CUX-P4).
func (p turnPart) PreviewTurn(ix string) (module.PreviewSink, bool) {
	d := p.d
	if !d.avail.Available("preview") {
		return nil, false
	}
	if _, ok := d.previewWanted(ix); !ok {
		return nil, false
	}
	return previewSink{d: d, ix: ix}, true
}

type previewSink struct {
	d  *Daemon
	ix string
}

func (s previewSink) Offer(text, reasoning string) { s.d.previewOffer(s.ix, text, reasoning) }

// ReadOwnEvidence implements module.OwnEvidenceHost: the events of the types the name table lists for
// the module, recorded at or after since.
func (p evidencePart) ReadOwnEvidence(eventType string, since time.Time) ([]module.Evidence, error) {
	info, ok := module.Lookup(p.name)
	if !ok || !info.MayRead(eventType) {
		return nil, fmt.Errorf("%w: %s may not read %q", module.ErrNotGranted, p.name, eventType)
	}
	if p.d.ledger == nil {
		return nil, nil
	}
	var out []module.Evidence
	p.d.ledger.scan(eventType, since.UnixMilli(), func(at int64, payload map[string]any) {
		out = append(out, module.Evidence{Type: eventType, At: time.UnixMilli(at), Payload: payload})
	})
	return out, nil
}

// policyConflict is a configuration write the inbound policy check refused (ErrPolicyConflict), seen by
// the module as module.ErrConfigConflict with the kernel's sentence.
type policyConflict struct{ err error }

func (c policyConflict) Error() string        { return c.err.Error() }
func (c policyConflict) Unwrap() error        { return c.err }
func (c policyConflict) Is(target error) bool { return target == module.ErrConfigConflict }

// moduleBlock is the configuration block the name table gives a module, raw: the top-level auto_reply
// for autoreply, modules.<name> for the others.
func moduleBlock(c Config, info module.Info) json.RawMessage {
	if info.Name == "autoreply" {
		return blockOrNil(c.AutoReply)
	}
	return blockOrNil(c.Modules[info.Name])
}

// setModuleBlock replaces the block in c.
func setModuleBlock(c *Config, info module.Info, raw json.RawMessage) {
	raw = blockOrNil(raw)
	if info.Name == "autoreply" {
		c.AutoReply = raw
		return
	}
	if c.Modules == nil {
		c.Modules = map[string]json.RawMessage{}
	}
	if raw == nil {
		delete(c.Modules, info.Name)
		return
	}
	c.Modules[info.Name] = raw
}

// blockOrNil reads an empty block and JSON null as no block.
func blockOrNil(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	return raw
}

// UpdateModuleConfig implements module.ModuleConfigHost (K-6). One write: under the configuration lock
// it reads the file as it is on the disk, calls fn with this module's block, runs the module's
// ConfigSpec.Check on what comes back, replaces this one key, saves atomically and puts the block in
// force. In the same step it writes anet.policy.changed for the key as a whole (the summaries differ)
// and for each audited field that changed; the module cannot write that event itself.
func (p configPart) UpdateModuleConfig(fn func(old json.RawMessage) (json.RawMessage, error)) error {
	d := p.d
	info, ok := module.Lookup(p.name)
	spec, hasSpec := module.ConfigSpecOf(p.name)
	if !ok || !hasSpec || !info.Granted(module.GrantModuleConfigHost) {
		return module.ErrNotGranted
	}
	d.cfgWrite.Lock()
	defer d.cfgWrite.Unlock()
	next := d.config()
	if err := d.carryDiskConfig(&next); err != nil {
		return err
	}
	old := moduleBlock(next, info)
	raw, err := fn(old)
	if err != nil {
		return err
	}
	raw = blockOrNil(raw)
	if spec.Check != nil {
		if err := spec.Check(raw, d.layout.Root); err != nil {
			return err
		}
	}
	setModuleBlock(&next, info, raw)
	// The same check as at start and on inbound policy writes (A2A-DESIGN §5.1).
	if err := validatePolicy(next, d.untrustedBackend.Load()); err != nil {
		if errors.Is(err, ErrPolicyConflict) {
			return policyConflict{err}
		}
		return err
	}
	if err := SaveConfig(d.layout, next); err != nil {
		return err
	}
	d.mu.Lock()
	setModuleBlock(&d.cfg, info, raw)
	d.mu.Unlock()
	d.auditModuleConfig(info, spec, old, raw)
	return nil
}

// auditModuleConfig writes anet.policy.changed for a module's configuration change (K-6): the key as a
// whole when the summaries differ, and each of the name table's AuditFields whose value differs, under the
// module's configuration key (a missing block has the defaults' values, so turning the module on with an
// allowed host or off with one is recorded too).
func (d *Daemon) auditModuleConfig(info module.Info, spec module.ConfigSpec, from, to json.RawMessage) {
	key := info.ConfigKey
	var fs, ts map[string]any
	if spec.Summary != nil {
		fs, ts = spec.Summary(from), spec.Summary(to)
	}
	if !reflect.DeepEqual(fs, ts) || (from == nil) != (to == nil) {
		d.recordPolicyChange(key, fs, ts, nil)
	}
	if spec.Audit == nil {
		return
	}
	fa, ta := spec.Audit(from), spec.Audit(to)
	for _, f := range info.AuditFields {
		if a, b := fa[f], ta[f]; !reflect.DeepEqual(a, b) {
			d.recordPolicyChange(key+"."+f, a, b, nil)
		}
	}
}

// moduleSI5 is the SI-5 values a configuration file holds for the modules that registered a ConfigSpec,
// by their ConfigSpec.SI5.
func moduleSI5(c Config) map[string]any {
	out := map[string]any{}
	for _, info := range module.Known() {
		if spec, ok := module.ConfigSpecOf(info.Name); ok && spec.SI5 != nil {
			for k, v := range spec.SI5(moduleBlock(c, info)) {
				out[k] = v
			}
		}
	}
	return out
}

// moduleConfigRefusal runs the registered ConfigSpec.Check of every module on c, as the daemon does
// at start (dataDir is the node's data directory; empty skips the checks that need it) and returns the
// refusals, in the name table's order.
func moduleConfigRefusals(c Config, dataDir string) []*a2ashape.Refusal {
	var out []*a2ashape.Refusal
	for _, info := range module.Known() {
		spec, ok := module.ConfigSpecOf(info.Name)
		if !ok || spec.Check == nil {
			continue
		}
		if err := spec.Check(moduleBlock(c, info), dataDir); err != nil {
			var ref *a2ashape.Refusal
			if errors.As(err, &ref) {
				out = append(out, ref)
			} else {
				out = append(out, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": info.ConfigKey, "rule": err.Error()}))
			}
		}
	}
	return out
}
