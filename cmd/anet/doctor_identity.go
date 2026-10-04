package main

// doctor_identity.go is doctor's four identity checks (ux §5.2, KEL §5.4):
// identity.format, identity.witness, identity.recovery and identity.rotation.
// They read the data directory (the identity file, the latest witness
// notice, the join record) and, while the daemon runs, its /status for what
// only it knows; they write nothing (UX-K1). Every finding is a sentence of
// the reason table, the same the daemon gives in /identity/status, anet
// status and MCP (UX-K7): identity.recovery's reminder is the one sentence
// the four places share (KEL D-15, M47).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// The identity states doctor grades (ux UX-R5).
const (
	idStateOK          = "ok"
	idStateNoHub       = "no_hub"
	idStatePublishing  = "publishing"
	idStateNoticeStale = "notice_stale"
	idStateStopped     = "stopped"
	idStateDeactivated = "deactivated"
)

// recoveryDirName is where a recovery file is kept by default: outside the
// data directory, in the home directory (KEL §2.4).
const recoveryDirName = ".anet-recovery"

// defaultRecoveryFile is ~/.anet-recovery/<the last 12 characters of the
// AID>.anetrec (KEL §2.4).
func defaultRecoveryFile(home, aid string) string {
	tail := aid
	if len(tail) > 12 {
		tail = tail[len(tail)-12:]
	}
	return filepath.Join(home, recoveryDirName, tail+".anetrec")
}

// identityChecks adds the four identity checks, shown when the data
// directory holds an identity file (ux §5.2).
func identityChecks(add func(id, status, detail, hint string), say func(id, status, key string, args a2ashape.Args),
	rep *doctorReport, env doctorEnv, layout daemon.Layout, lang a2ashape.Language) {
	if _, err := os.Stat(layout.IdentityPath()); err != nil {
		return
	}
	st, err := daemon.ReadIdentityStatus(layout, lang)
	var refusal *daemon.ConfigRefusal
	switch {
	case errors.As(err, &refusal):
		// An identity file of anet 0.2 (KEL S-2): the daemon refuses to
		// start, and nothing else about the identity can be said.
		for _, r := range refusal.Refusals {
			say("identity.format", stFail, r.Key, r.Args)
		}
		return
	case err != nil:
		add("identity.format", stFail, err.Error(), noHint)
		return
	}
	add("identity.format", stOK, layout.IdentityPath(), "")
	identityWitnessCheck(say, rep, env, layout, st)
	identityRecoveryCheck(say, env, st)
	if st.Pending != nil {
		say("identity.rotation", stWarn, "rotation_pending", nil)
	} else {
		add("identity.rotation", stOK, "", "")
	}
}

// identityWitnessCheck is identity.witness: the identity's state and its
// sentence, from the running daemon when it answers (it knows the hub's
// latest answer), else from the data directory.
func identityWitnessCheck(say func(id, status, key string, args a2ashape.Args), rep *doctorReport, env doctorEnv,
	layout daemon.Layout, st daemon.IdentityStatus) {
	if rep.Control.Running && env.status != nil {
		if live, ok := liveIdentity(env, rep, layout); ok {
			st = live
		}
	}
	status := identityWitnessLevel(st.State, st.Reason)
	if status == stOK && len(st.OldWitnessUnacked) > 0 {
		// The switch is done; the old hub's confirmation is the push
		// loop's, and needs nothing from the user (KEL §1.6 item 8).
		say("identity.witness", stInfo, "old_hub_unacked", a2ashape.Args{"hub": st.OldWitnessUnacked[0].URL})
		return
	}
	c := doctorCheck{ID: "identity.witness", Status: status, Reason: st.Reason, Detail: st.Message, Hint: st.Next}
	if c.Hint == "" && status != stOK {
		c.Hint = noHint
	}
	rep.Checks = append(rep.Checks, c)
}

// liveIdentity is /status.identity of the running daemon.
func liveIdentity(env doctorEnv, rep *doctorReport, layout daemon.Layout) (daemon.IdentityStatus, bool) {
	raw, err := env.status(rep.Control.Addr, layout.ControlTokenPath())
	if err != nil {
		return daemon.IdentityStatus{}, false
	}
	b, err := json.Marshal(raw["identity"])
	if err != nil {
		return daemon.IdentityStatus{}, false
	}
	var st daemon.IdentityStatus
	if json.Unmarshal(b, &st) != nil || st.State == "" || st.Reason == "" {
		return daemon.IdentityStatus{}, false
	}
	return st, true
}

// identityWitnessLevel grades an identity state for identity.witness (ux
// §5.2): ok while the hub confirms the key, warn while that is late or the
// hub is checking, fail while nothing can be sent.
func identityWitnessLevel(state, reason string) string {
	switch state {
	case idStateOK:
		if reason == "identity.ok" {
			return stOK
		}
		return stWarn
	case idStateNoHub:
		return stInfo
	case idStatePublishing:
		return stWarn
	case idStateNoticeStale:
		if reason == "notice_stale" {
			return stFail
		}
		return stWarn
	}
	return stFail // stopped, deactivated, and anything this binary does not know
}

// identityRecoveryCheck is identity.recovery: the reminder while the
// identity has no recovery passphrase (first: it is the one that matters),
// else the recovery file at its default place, its mode and a leftover of
// an unfinished change.
func identityRecoveryCheck(say func(id, status, key string, args a2ashape.Args), env doctorEnv, st daemon.IdentityStatus) {
	if key := st.RecoveryReminder(); key != "" {
		say("identity.recovery", stWarn, key, nil)
		return
	}
	path := defaultRecoveryFile(env.home, st.AID)
	if env.home == "" || st.Recovery.State != daemon.RecoveryProtected {
		say("identity.recovery", stInfo, "identity.recovery_note", nil)
		return
	}
	if _, err := os.Stat(path + ".new"); err == nil {
		say("identity.recovery", stWarn, "identity.recovery_leftover", a2ashape.Args{"path": path + ".new"})
		return
	}
	fs := statFile(path)
	switch {
	case !fs.Present:
		say("identity.recovery", stInfo, "identity.recovery_note", nil)
	case fs.Mode != "0600":
		say("identity.recovery", stWarn, "identity.recovery_perm", a2ashape.Args{"path": path})
	default:
		say("identity.recovery", stOK, "identity.status.recovery", a2ashape.Args{"path": tildeHome(env.home, path)})
	}
}

// tildeHome writes a path under home with ~ for it, as the sentences show
// the recovery file (KEL §5.2).
func tildeHome(home, path string) string {
	if home != "" {
		if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
			return "~" + string(filepath.Separator) + rest
		}
	}
	return path
}
