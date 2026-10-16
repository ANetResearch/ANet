package daemon

// privacy_status.go is /status.privacy (FS §2.10, FS-U7): what the node
// says about how long it keeps content, whether the clock lets it delete,
// how the encryption keys stand, and whether its data went back to an older
// state. The privacy line of the status summary and doctor's clock,
// keys.epoch and local_state checks read the same object.

import (
	"os"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

const (
	// clockClosedShowAfter is how long the hub gate must have been closed
	// before the privacy line says so (FS-U7 row 2): ten minutes is the age
	// at which a check goes stale, so a short loss of the hub says nothing.
	clockClosedShowAfter uint64 = 10 * 60 * 1000
	// keptLongExtraH is the extra time, in hours, from which an expired key
	// still kept is told (FS-U7 row 3).
	keptLongExtraH = 6
	privacyHourMS  = uint64(60 * 60 * 1000)
)

// epochSummary is /status.privacy.epoch_keys (FS §2.10) before it is a
// map: published_days_ahead, expired_kept, oldest_kept_extra_h, kept_reason.
type epochSummary struct {
	DaysAhead    uint64 `json:"published_days_ahead"`
	ExpiredKept  uint64 `json:"expired_kept"`
	OldestExtraH uint64 `json:"oldest_kept_extra_h"`
	KeptReason   string `json:"kept_reason"`
}

// summarizeKeys is the epoch_keys object for keys at now. A key is expired
// once now is past A(k) + M (lastAcceptance, drainMarginMS), the moment the
// early branch could let it go; it is kept while epochKeyDeletable says
// no. The reason is the one of the key kept longest, and only a reason the
// user can act on is named (mailbox_not_drained, held_messages, clock);
// without a gate (reasonKnown false, doctor reading the file) it is empty.
func summarizeKeys(keys []*ringKey, now uint64, gate clockLevel, drainMark uint64, held kidSet, reasonKnown bool) epochSummary {
	var s epochSummary
	end := satAdd(epochStart(now), seal.EpochMS)
	var latest uint64
	for _, k := range keys {
		if publishable(k) && k.Pair.Public.NotAfter > latest {
			latest = k.Pair.Public.NotAfter
		}
	}
	if latest > end {
		s.DaysAhead = (latest - end) / seal.EpochMS
	}
	var worst uint64
	for _, k := range keys {
		due := satAdd(lastAcceptance(k), drainMarginMS)
		if now <= due {
			continue
		}
		s.ExpiredKept++
		if extra := now - due; extra >= worst {
			worst = extra
			if reasonKnown {
				s.KeptReason = ""
				if ok, why := epochKeyDeletable(k, now, gate, drainMark, held); !ok {
					switch why {
					case keyMailboxNotDrained, keyHeldMessages, keyClock:
						s.KeptReason = why
					}
				}
			}
		}
	}
	s.OldestExtraH = worst / privacyHourMS
	return s
}

// clockGateNow is the clock gate as a maintenance period starting now would
// find it (clockLevelNow's rule), with the check it read: the level, the
// clock state (FS-K16) and the observation.
func (d *Daemon) clockGateNow() (clockLevel, string, clockObs) {
	wall, mono := d.nowMS(), d.monoMS()
	obs := d.clockObservation()
	d.clk.mu.Lock()
	var prev *cycleMark
	if d.clk.prev != nil {
		p := *d.clk.prev
		prev = &p
	}
	d.clk.mu.Unlock()
	var run uint64
	if d.enc != nil {
		run = d.enc.unverifiedRun()
	}
	open := hubGateOpen(obs, wall, mono)
	run = nextUnverifiedRun(run, open, prev, wall, mono)
	level, state := clockGate(obs, run, wall, mono)
	d.noteGate(open, mono)
	return level, state, obs
}

// noteGate records, on the boot clock, when the hub gate was first seen
// closed and clears it when it is seen open. Called by every maintenance
// period and every read of the privacy status.
func (d *Daemon) noteGate(open bool, mono uint64) {
	d.clk.mu.Lock()
	defer d.clk.mu.Unlock()
	switch {
	case open:
		d.clk.closedSince = 0
	case d.clk.closedSince == 0:
		d.clk.closedSince = max(mono, 1)
	}
}

// gateClosedFor is how long the hub gate has been closed, on the boot
// clock, as far as this process can tell: since the earlier of when it was
// first seen closed and when the last check went stale or found the clocks
// apart. 0 while it is open.
func (d *Daemon) gateClosedFor(obs clockObs, wall, mono uint64) uint64 {
	if hubGateOpen(obs, wall, mono) {
		return 0
	}
	d.clk.mu.Lock()
	since := d.clk.closedSince
	d.clk.mu.Unlock()
	if obs.checked && since != 0 && since <= obs.mono {
		since = 0 // a check since then: what was seen closed then has been decided again
	}
	if obs.checked && !wallJumped(obs.wall, obs.mono, wall, mono) {
		derived := obs.mono
		if absMS(obs.offset) <= seal.ClockSkewMS {
			derived = satAdd(obs.mono, clockCheckMaxAge)
		}
		if since == 0 || derived < since {
			since = derived
		}
	}
	if since == 0 || mono < since {
		return 0
	}
	return mono - since
}

// privacyStatus is /status.privacy and the facts the privacy line is chosen
// from.
type privacyStatus struct {
	body  map[string]any
	input a2ashape.StatusInput
}

// privacy builds /status.privacy (FS §2.10) at the time of the call.
func (d *Daemon) privacy() privacyStatus {
	var ps privacyStatus
	r := d.ContentRetention()
	body := map[string]any{"content_retention": r.String(), "public_retention": r.Public().String()}
	wall, mono := d.nowMS(), d.monoMS()
	level, state, obs := d.clockGateNow()
	body["clock"] = state
	if state != clockNoHub && obs.checked {
		body["clock_skew_s"] = obs.offset / 1000
	}
	body["deletion"] = deletionOf(level)
	var ep epochSummary
	if d.enc != nil {
		d.enc.mu.RLock()
		keys := append([]*ringKey(nil), d.enc.keys...)
		d.enc.mu.RUnlock()
		mark, held := d.drainInputs()
		ep = summarizeKeys(keys, wall, level, mark, held, true)
	}
	body["epoch_keys"] = map[string]any{"published_days_ahead": ep.DaysAhead, "expired_kept": ep.ExpiredKept,
		"oldest_kept_extra_h": ep.OldestExtraH, "kept_reason": ep.KeptReason}
	drops := d.unknownKeyDrops24h()
	body["unknown_key_drops_24h"] = drops
	lost := d.localStateLost()
	if lost == nil {
		body["local_state_lost"] = nil
	} else {
		body["local_state_lost"] = lost
	}

	in := &ps.input
	in.HubRegistered = d.HubView().State == JoinRegistered
	in.Retention = a2ashape.Said{Key: "retention.summary", Args: a2ashape.Args{"retention": r.String()}}
	if s := lost.said(); s != nil {
		if lost.Repeated {
			in.LocalStateRepeated = s
		} else {
			in.LocalStateDetected = s
		}
	}
	closed := d.gateClosedFor(obs, wall, mono)
	if closed > 0 {
		// Not in FS §2.10: how long the hub gate has been closed, so that
		// doctor tells a clock that stays unconfirmed (FS §5.4: ten
		// minutes) and not one the hub has not answered yet.
		body["clock_closed_s"] = closed / 1000
	}
	if (state == clockSkewed || state == clockUnverified) && closed >= clockClosedShowAfter {
		key := "clock." + state
		args := a2ashape.Args{}
		if state == clockSkewed {
			args["dur"] = time.Duration(absMS(obs.offset)) * time.Millisecond
		}
		in.Clock = &a2ashape.Said{Key: key, Args: args}
	}
	switch {
	case ep.OldestExtraH >= keptLongExtraH && (ep.KeptReason == keyMailboxNotDrained || ep.KeptReason == keyHeldMessages):
		in.Keys = &a2ashape.Said{Key: "keys.kept_long", Args: a2ashape.Args{"n": ep.ExpiredKept,
			"dur": time.Duration(ep.OldestExtraH) * time.Hour}}
	case drops > 0:
		in.Keys = &a2ashape.Said{Key: "keys.unknown_drops", Args: a2ashape.Args{"n": drops}}
	}
	line := a2ashape.PrivacyLine(*in)
	lang := a2ashape.Lang()
	body["reason"] = line.Key
	body["message"] = a2ashape.Sentence(line.Key, lang, line.Args)
	if n := a2ashape.Next(line.Key, lang, line.Args); n != "" {
		body["next"] = n
	}
	ps.body = body
	return ps
}

// FillPrivacy sets the privacy line's facts of the status summary (ux
// §4.7, FS-U7): StatusSummary picks the sentence.
func (d *Daemon) FillPrivacy(in *a2ashape.StatusInput) {
	p := d.privacy().input
	in.HubRegistered, in.Retention, in.Clock, in.Keys = p.HubRegistered, p.Retention, p.Clock, p.Keys
	in.LocalStateRepeated, in.LocalStateDetected = p.LocalStateRepeated, p.LocalStateDetected
}

// KeyRingSummary is what doctor reads of enc_keys.cbor while the daemon is
// not running (FS §5.4 keys.epoch): the keys' dates and the rollback mark,
// no private key kept.
type KeyRingSummary struct {
	DaysAhead, ExpiredKept, OldestExtraH uint64
	// LocalState is the rollback to show, or nil (FS-B4).
	LocalState *LocalStateNotice
}

// LocalStateNotice is /status.privacy.local_state_lost.
type LocalStateNotice = localStateNotice

// Said is the sentence key and values of the notice's line (local_state.*).
func (n *localStateNotice) Said() *a2ashape.Said { return n.said() }

// ReadKeyRingSummary reads the key ring file of layout at now. The error is
// os.ErrNotExist for no file, and any other for one that cannot be read or
// is not a ring of this version.
func ReadKeyRingSummary(layout Layout, now uint64) (*KeyRingSummary, error) {
	b, err := os.ReadFile(layout.EncKeysPath())
	if err != nil {
		return nil, err
	}
	f, err := decodeEncKeyFile(b)
	clear(b)
	if err != nil {
		return nil, err
	}
	keys := make([]*ringKey, len(f.Keys))
	for i := range f.Keys {
		keys[i] = &f.Keys[i]
	}
	s := summarizeKeys(keys, now, clockClosed, 0, nil, false)
	for i := range f.Keys {
		clear(f.Keys[i].Pair.Private)
	}
	return &KeyRingSummary{DaysAhead: s.DaysAhead, ExpiredKept: s.ExpiredKept, OldestExtraH: s.OldestExtraH,
		LocalState: localStateNoticeAt(f.Rollback, now)}, nil
}
