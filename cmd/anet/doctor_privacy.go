package main

// doctor_privacy.go is doctor's four forward-secrecy checks (ux §5.2, FS
// §5.4): clock, keys.epoch, retention and local_state. While the daemon
// runs they read /status.privacy (FS §2.10); without it keys.epoch and
// local_state read enc_keys.cbor (public data only) and retention reads
// config.json. Every finding is a sentence of the reason table.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// The thresholds of FS §5.4 and FS-U7.
const (
	// clockClosedWarnS is how long the hub gate must have stayed closed
	// before doctor warns about the clock.
	clockClosedWarnS = 10 * 60
	// keysKeptLongH is the extra hours an expired key is kept from which
	// doctor warns when the mailbox is the cause.
	keysKeptLongH = 6
	// keysAheadDays is how far ahead keys are published (FS-K1).
	keysAheadDays = 14
)

// privacyView is /status.privacy as doctor reads it.
type privacyView struct {
	ContentRetention string `json:"content_retention"`
	Clock            string `json:"clock"`
	ClockSkewS       *int64 `json:"clock_skew_s"`
	ClockClosedS     uint64 `json:"clock_closed_s"`
	Deletion         string `json:"deletion"`
	EpochKeys        struct {
		DaysAhead   uint64 `json:"published_days_ahead"`
		ExpiredKept uint64 `json:"expired_kept"`
		ExtraH      uint64 `json:"oldest_kept_extra_h"`
		KeptReason  string `json:"kept_reason"`
	} `json:"epoch_keys"`
	UnknownDrops uint64 `json:"unknown_key_drops_24h"`
	LocalState   *struct {
		At       string `json:"at"`
		Tasks    uint64 `json:"tasks"`
		Repeated bool   `json:"repeated"`
	} `json:"local_state_lost"`
}

// privacyChecks adds clock, keys.epoch, retention and local_state, in that
// order.
func privacyChecks(say func(id, status, key string, args a2ashape.Args), rep *doctorReport, env doctorEnv,
	layout daemon.Layout, cfg daemon.Config, now time.Time) {
	var st map[string]any
	var pv *privacyView
	if rep.Control.Running && env.status != nil {
		st, _ = env.status(rep.Control.Addr, layout.ControlTokenPath())
	}
	if raw, ok := st["privacy"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			var v privacyView
			if json.Unmarshal(b, &v) == nil {
				pv = &v
			}
		}
	}
	clockCheck(say, rep, cfg, st, pv)
	var ring *daemon.KeyRingSummary
	var ringErr error
	if pv == nil {
		ring, ringErr = daemon.ReadKeyRingSummary(layout, uint64(max(now.UnixMilli(), 0)))
	}
	keysEpochCheck(say, rep, pv, ring, ringErr)
	retentionCheck(say, rep, layout, pv)
	localStateCheck(say, rep, pv, ring)
}

// clockCheck is the clock check: the hub's view of this computer's clock
// (FS-K16), or the signing-time retries (HPP PAY-R11).
func clockCheck(say func(id, status, key string, args a2ashape.Args), rep *doctorReport, cfg daemon.Config,
	st map[string]any, pv *privacyView) {
	if pv == nil {
		if cfg.HubURL == "" {
			say("clock", stInfo, "clock.no_hub", nil)
			return
		}
		say("clock", stUnknown, doctorDaemonDown, nil)
		return
	}
	var finding *doctorCheck
	lang := a2ashape.Lang()
	switch {
	case pv.Clock == "no_hub":
		say("clock", stInfo, "clock.no_hub", nil)
		return
	case (pv.Clock == "skewed" || pv.Clock == "unverified") && pv.ClockClosedS >= clockClosedWarnS:
		args := a2ashape.Args{}
		if pv.Clock == "skewed" && pv.ClockSkewS != nil {
			args["dur"] = time.Duration(absInt64(*pv.ClockSkewS)) * time.Second
		}
		c := sayCheck("clock", stWarn, "clock."+pv.Clock, args, lang)
		c.Deletion = pv.Deletion
		finding = &c
	default:
		c, _ := st["clock"].(map[string]any)
		if n, _ := c["ts_retries"].(float64); n > 10 {
			f := sayCheck("clock", stWarn, "clock.ts_retries", a2ashape.Args{"n": fmt.Sprintf("%.0f", n)}, lang)
			finding = &f
		}
	}
	if finding != nil {
		rep.Checks = append(rep.Checks, *finding)
		return
	}
	var skew int64
	if pv.ClockSkewS != nil {
		skew = absInt64(*pv.ClockSkewS)
	}
	say("clock", stOK, "clock.ok", a2ashape.Args{"n": skew})
}

func absInt64(n int64) int64 {
	if n == math.MinInt64 {
		return math.MaxInt64
	}
	if n < 0 {
		return -n
	}
	return n
}

// keysEpochCheck is keys.epoch (FS §5.4): shown once the node has a key
// ring. Without the daemon it judges only what the file shows, the days
// published ahead and the expired keys still kept; anything else is
// unknown.
func keysEpochCheck(say func(id, status, key string, args a2ashape.Args), rep *doctorReport, pv *privacyView,
	ring *daemon.KeyRingSummary, ringErr error) {
	if pv != nil {
		ep := pv.EpochKeys
		switch {
		case ep.ExtraH >= keysKeptLongH && (ep.KeptReason == "mailbox_not_drained" || ep.KeptReason == "held_messages"):
			say("keys.epoch", stWarn, "keys.kept_long", a2ashape.Args{"n": ep.ExpiredKept, "dur": time.Duration(ep.ExtraH) * time.Hour})
		case pv.UnknownDrops > 0:
			say("keys.epoch", stWarn, "keys.unknown_drops", a2ashape.Args{"n": pv.UnknownDrops})
		default:
			say("keys.epoch", stOK, "keys.epoch_ok", nil)
		}
		return
	}
	if ringErr != nil {
		if !errors.Is(ringErr, os.ErrNotExist) {
			say("keys.epoch", stUnknown, doctorDaemonDown, nil)
		}
		return
	}
	if ring.DaysAhead >= keysAheadDays && ring.ExpiredKept == 0 {
		say("keys.epoch", stOK, "keys.epoch_ok", nil)
		return
	}
	say("keys.epoch", stUnknown, doctorDaemonDown, nil)
}

// retentionCheck is retention (FS §5.4): the setting in force, a note on a
// copy-on-write filesystem, and a config.json value that is not a setting.
func retentionCheck(say func(id, status, key string, args a2ashape.Args), rep *doctorReport, layout daemon.Layout,
	pv *privacyView) {
	r, _, ref, err := daemon.ContentRetentionInFile(layout)
	if err == nil && ref != nil {
		say("retention", stFail, "retention_invalid", nil)
		return
	}
	value := r.String()
	if pv != nil && pv.ContentRetention != "" {
		value = pv.ContentRetention
	} else if err != nil {
		return // config.json cannot be read: the config check says so
	}
	say("retention", stInfo, "retention.summary", a2ashape.Args{"retention": value})
	if copyOnWriteFS(layout.Root) {
		say("retention", stInfo, "retention.cow", nil)
	}
}

// localStateCheck is local_state (FS §5.4): shown only for the 7 days a
// verified rollback is told, whether or not it ended any task.
func localStateCheck(say func(id, status, key string, args a2ashape.Args), rep *doctorReport, pv *privacyView,
	ring *daemon.KeyRingSummary) {
	var at string
	var tasks uint64
	var repeated, found bool
	switch {
	case pv != nil:
		if pv.LocalState != nil {
			at, tasks, repeated, found = pv.LocalState.At, pv.LocalState.Tasks, pv.LocalState.Repeated, true
		}
	case ring != nil && ring.LocalState != nil:
		at, tasks, repeated, found = ring.LocalState.At, ring.LocalState.Tasks, ring.LocalState.Repeated, true
	}
	if !found {
		return
	}
	args := a2ashape.Args{"n": tasks, "time": at}
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		args["time"] = t
	}
	if repeated {
		say("local_state", stFail, "local_state.repeated", args)
		return
	}
	say("local_state", stWarn, "local_state.detected", args)
}
