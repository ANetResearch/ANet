package main

// identity_rotate.go is `anet identity status`, `protect`, `rotate` and `deactivate` (KEL L-3, L-6,
// L-7, L-10, D-16, D-31a). A change of the recovery seed is always two rotations, one after the
// other: the first reveals a key the old seed derives and commits the new seed's, the second reveals
// the new seed's key, so that from then on the current key and the next one come from the new seed
// alone.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// cells is the display width of s in a terminal: a wide character takes two.
func cells(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x1100 && (r <= 0x115f || (r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) ||
			(r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe30 && r <= 0xfe6f) || (r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6)) {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func padCells(s string, w int) string {
	if n := cells(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// statusCommand is `anet identity status [--json]` (KEL L-7).
func (x *identityCLI) statusCommand() error {
	if len(x.pos) > 0 {
		return usageError{"anet identity status [--json]"}
	}
	st, err := x.statusNow(true)
	if err != nil {
		return err
	}
	if x.c != nil || !daemon.DataLockHeld(x.layout) {
		x.settleLeftovers(st)
	}
	rf := x.recoveryFileInfo(st)
	if x.flags["json"] == "true" {
		b, err := json.Marshal(st)
		if err != nil {
			return err
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		m["recovery_file"] = rf
		out, _ := json.MarshalIndent(m, "", "  ")
		fmt.Fprintln(x.out, string(out))
		return nil
	}
	lab := statusLabels[x.lang]
	width := 0
	for _, l := range lab {
		width = max(width, cells(l)+2)
	}
	row := func(i int, v string) { fmt.Fprintf(x.out, "%s%s\n", padCells(lab[i], width), v) }
	row(0, fmt.Sprintf("%s %s", printable(st.Name, 64), a2ashape.Short8(st.AID)))
	switch {
	case st.LastRotation != nil:
		row(1, x.sentence("identity.status.key", a2ashape.Args{"n": st.KeySeq,
			"time": rfc3339Time(st.LastRotation.At), "how": st.LastRotation.Mode}))
	default:
		row(1, x.sentence("identity.status.key_never", nil))
	}
	switch {
	case st.Witness == nil:
		row(2, "—")
	case st.Notice != nil:
		row(2, x.sentence("identity.status.witness", a2ashape.Args{"hub": st.Witness.URL,
			"ago": time.Duration(st.Notice.AgeS) * time.Second}))
	default:
		row(2, st.Witness.URL)
	}
	switch {
	case rf.Path == "":
		row(3, "—")
	case rf.Unprotected:
		row(3, x.sentence("identity.status.recovery_unprotected", a2ashape.Args{"path": rf.Path}))
	case rf.Present:
		row(3, x.sentence("identity.status.recovery", a2ashape.Args{"path": rf.Path}))
	default:
		row(3, x.sentence("identity.status.recovery_elsewhere", a2ashape.Args{"path": rf.Path}))
	}
	row(4, st.Message)
	for _, o := range st.OldWitnessUnacked {
		fmt.Fprintln(x.out, x.sentence("old_hub_unacked", a2ashape.Args{"hub": o.URL}))
	}
	if r := st.Recovery; r.State == "unprotected" && r.Message != "" {
		say(x.out, r.Message, r.Next)
	} else if st.Next != "" {
		fmt.Fprintln(x.out, nextPrefix+st.Next)
	}
	return nil
}

// changeRefusal words why an identity in this state cannot change keys (KEL D-30 step 1), for the
// commands that check before they ask anything.
func (x *identityCLI) changeRefusal(st daemon.IdentityStatus) error {
	switch st.State {
	case "no_hub":
		return x.refuse("no_hub_yet", nil)
	case "publishing":
		return x.refuse("rotation_pending", nil)
	case "stopped":
		return x.refuse("identity_stopped", a2ashape.Args{"name": st.Name})
	case "deactivated":
		return x.refuse("identity_deactivated", nil)
	}
	return nil
}

// dryRun asks the daemon whether the change would go ahead (KEL D-30, D-32, L-4): nothing is
// changed and no seed is needed. A refusal is said as the daemon words it. down is true when the
// answer was witness_unreachable, which an emergency rotation goes on with.
func (x *identityCLI) dryRun(mode, hubURL string, emergencyOK bool) (down bool, err error) {
	body := map[string]any{"mode": mode, "dry_run": true}
	if hubURL != "" {
		body["hub_url"] = hubURL
	}
	b, code, err := x.postRaw("/identity/rotate", body)
	if err != nil {
		return false, err
	}
	if code == 200 {
		return false, nil
	}
	if emergencyOK && code == 503 {
		return true, nil
	}
	return false, defaultHubCLI().refusedBy(code, b)
}

// swapSeeds changes the recovery seed with two rotations of the same mode (KEL D-16, D-31a): the new
// seed is made, each rotation writes the new recovery file in two phases, and the second starts as
// soon as the first is taken. done is false when the witness had not taken the last one within
// idSettle; the rotation is on disk and finishes on its own.
func (x *identityCLI) swapSeeds(st daemon.IdentityStatus, u *unlocked, newPass []byte, mode, hubURL string) (fin daemon.IdentityStatus, done bool, path string, err error) {
	path, err = x.recoveryPath(st.AID)
	if err != nil {
		return st, false, "", err
	}
	rsNew, err := newSeed()
	if err != nil {
		return st, false, "", err
	}
	defer clear(rsNew[:])
	hint := hubOf(st)
	if hubURL != "" {
		hint = hubURL
	}
	if err := x.step(stepSpec{st: st, rsOld: u.rs, rsNew: rsNew, mode: mode, hubURL: hubURL,
		newPath: path, newPass: newPass, fileHub: hint}); err != nil {
		return st, false, path, err
	}
	st1, ok, err := x.settle(st.KeySeq + 1)
	if err != nil || !ok {
		return st1, false, path, err
	}
	if err := x.step(stepSpec{st: st1, rsOld: rsNew, rsNew: rsNew, mode: mode,
		newPath: path, newPass: newPass, fileHub: hint}); err != nil {
		return st1, false, path, err
	}
	st2, ok, err := x.settle(st1.KeySeq + 1)
	return st2, ok, path, err
}

// rotate is `anet identity rotate [--new-recovery] [--emergency]` (KEL L-3).
func (x *identityCLI) rotate() error {
	if len(x.pos) > 0 {
		return usageError{"anet identity rotate [--new-recovery] [--emergency]"}
	}
	emergency := x.flags["emergency"] == "true"
	mode := "scheduled"
	if emergency {
		mode = "emergency"
	}
	t, err := x.openTerminal("recovery_passphrase")
	if err != nil {
		return err
	}
	defer t.Close()
	st, err := x.statusNow()
	if err != nil {
		return err
	}
	if x.c == nil {
		return x.needDaemon()
	}
	x.settleLeftovers(st)
	if err := x.changeRefusal(st); err != nil {
		return err
	}
	down, err := x.dryRun(mode, "", emergency)
	if err != nil {
		return err
	}
	unprotected := st.Recovery.State == "unprotected"
	switch {
	case emergency:
		return x.rotateEmergency(t, st, unprotected, down)
	case unprotected:
		// A rotation of an identity without a passphrase is `identity protect` (KEL L-3, D-16).
		return x.protectWithSeed(t, st, mode, "rotate")
	}
	fmt.Fprintln(x.out, x.sentence("identity.rotate.confirm", idArgs(st, nil)))
	u, err := x.unlock(t, st, "")
	if err != nil {
		return err
	}
	defer u.clear()
	if x.flags["new-recovery"] == "true" {
		return x.replaceSeed(t, st, u, mode, "rotate")
	}
	// The recovery file is written again with the same seed and passphrase: its commitment
	// names the key after this rotation, which is how `identity recover` knows the history it
	// is given is not a stale one (T-1 RT-F8).
	path, err := x.recoveryPath(st.AID)
	if err != nil {
		return err
	}
	if err := x.step(stepSpec{st: st, rsOld: u.rs, rsNew: u.rs, mode: mode,
		newPath: path, newPass: u.pass, fileHub: hubOf(st)}); err != nil {
		return err
	}
	fin, ok, err := x.settle(st.KeySeq + 1)
	if err != nil {
		return err
	}
	if !ok {
		return x.stillPublishing()
	}
	x.tell("identity.rotate.done", a2ashape.Args{"from": st.KeySeq, "to": fin.KeySeq, "hub": hubOf(fin)})
	return nil
}

// replaceSeed asks for the new passphrase and runs the two rotations of a seed change, then says
// how it went; kind is "rotate" or "protect" for the closing sentence.
func (x *identityCLI) replaceSeed(t *ttySession, st daemon.IdentityStatus, u *unlocked, mode, kind string) error {
	newPass, err := x.askNewPassphrase(t)
	if err != nil {
		return err
	}
	defer clear(newPass)
	fin, ok, path, err := x.swapSeeds(st, u, newPass, mode, "")
	if err != nil {
		return err
	}
	if !ok {
		return x.stillPublishing()
	}
	if st.Recovery.State == "unprotected" {
		_ = removeLocalRecovery(x.layout.Root)
	}
	x.tell("identity.recovery_written", a2ashape.Args{"path": path})
	args := a2ashape.Args{"from": st.KeySeq, "to": fin.KeySeq, "hub": hubOf(fin)}
	if kind == "protect" {
		x.tell("identity.protect.done", args)
	} else {
		x.tell("identity.rotate.done", args)
	}
	return nil
}

// protectWithSeed is the rotation of an identity that has no passphrase yet (KEL D-16): its local
// recovery seed opens the files without a passphrase, the user sets a new one, and two rotations
// follow. Used by `identity protect` and by `identity rotate` on such an identity.
func (x *identityCLI) protectWithSeed(t *ttySession, st daemon.IdentityStatus, mode, kind string) error {
	x.tell("identity.protect.confirm", idArgs(st, nil))
	u, err := x.unlock(t, st, "")
	if err != nil {
		return err
	}
	defer u.clear()
	if kind == "rotate" || kind == "protect" {
		return x.replaceSeed(t, st, u, mode, "protect")
	}
	return nil
}

// rotateEmergency is `anet identity rotate --emergency` (KEL D-40–D-43, D-31a).
func (x *identityCLI) rotateEmergency(t *ttySession, st daemon.IdentityStatus, unprotected, witnessDown bool) error {
	a := idArgs(st, nil)
	for i := 1; i <= 4; i++ {
		fmt.Fprintln(x.out, x.sentence(fmt.Sprintf("identity.rotate.emergency_summary.%d", i), a))
	}
	if unprotected {
		fmt.Fprintln(x.out, x.sentence("identity.rotate.emergency_summary.5.unprotected", a))
	} else {
		fmt.Fprintln(x.out, x.sentence("identity.rotate.emergency_summary.5", a))
	}
	if witnessDown {
		fmt.Fprintln(x.out, x.sentence("identity.rotate.emergency_witness_down", a2ashape.Args{"hub": hubOf(st)}))
	}
	u, err := x.unlock(t, st, "")
	if err != nil {
		return err
	}
	defer u.clear()
	newPass, err := x.askNewPassphrase(t)
	if err != nil {
		return err
	}
	defer clear(newPass)
	ans, err := t.line(x.sentence("confirm.yes", nil))
	if err != nil || strings.ToLower(strings.TrimSpace(ans)) != "yes" {
		fmt.Fprintln(x.err, x.sentence("confirm.declined", nil))
		return errExit1
	}
	fin, ok, path, err := x.swapSeeds(st, u, newPass, "emergency", "")
	if err != nil {
		return err
	}
	if !ok {
		return x.stillPublishing()
	}
	if unprotected {
		_ = removeLocalRecovery(x.layout.Root)
	}
	x.tell("identity.recovery_written", a2ashape.Args{"path": path})
	x.tell("identity.rotate.emergency_done_seq", a2ashape.Args{"from": st.KeySeq, "to": fin.KeySeq})
	x.tell("rotation_emergency_done", nil)
	return nil
}

// protect is `anet identity protect [--recovery-file F]` (KEL L-10).
func (x *identityCLI) protect() error {
	if len(x.pos) > 0 {
		return usageError{"anet identity protect [--recovery-file F]"}
	}
	t, err := x.openTerminal("set_recovery_passphrase")
	if err != nil {
		return err
	}
	defer t.Close()
	st, err := x.statusNow()
	if err != nil {
		return err
	}
	if x.c == nil {
		return x.needDaemon()
	}
	x.settleLeftovers(st)
	switch {
	case st.Recovery.State == "protected":
		x.tell("identity.protect.already", nil)
		return nil
	case st.Recovery.State == "none":
		return x.refuse("no_hub_yet", nil)
	case st.State == "stopped" || st.State == "deactivated":
		return x.changeRefusal(st)
	case st.Pending != nil:
		return x.refuse("rotation_pending", nil)
	}
	// The first of the two rotations was taken last time and the second was not: the recovery file
	// of the new passphrase already commits to the key the daemon is waiting for.
	if path, perr := x.recoveryPath(st.AID); perr == nil {
		if b, rerr := os.ReadFile(path); rerr == nil {
			info, ierr := identity.ParseRecoveryFile(b)
			if ierr == nil && info.AID == st.AID && !info.Unprotected && st.Commitment != nil &&
				string(info.Commits.Digest[:]) == string(st.Commitment) {
				return x.protectResume(t, st, path)
			}
		}
	}
	if st.Witness == nil {
		return x.protectUnjoined(t, st)
	}
	if _, err := x.dryRun("scheduled", "", false); err != nil {
		return err
	}
	return x.protectWithSeed(t, st, "scheduled", "protect")
}

// protectResume finishes a `protect` that stopped after its first rotation (L-10 step 2): the
// passphrase set then opens the file, and the second rotation follows.
func (x *identityCLI) protectResume(t *ttySession, st daemon.IdentityStatus, path string) error {
	x.tell("identity.protect.resume", nil)
	u, err := x.unlock(t, st, path)
	if err != nil {
		return err
	}
	defer u.clear()
	mode := "scheduled"
	if st.LastRotation != nil && st.LastRotation.Mode == "emergency" {
		mode = "emergency"
	}
	if path == "" {
		if path, err = x.recoveryPath(st.AID); err != nil {
			return err
		}
	}
	if err := x.step(stepSpec{st: st, rsOld: u.rs, rsNew: u.rs, mode: mode,
		newPath: path, newPass: u.pass, fileHub: hubOf(st)}); err != nil {
		return err
	}
	fin, ok, err := x.settle(st.KeySeq + 1)
	if err != nil {
		return err
	}
	if !ok {
		return x.stillPublishing()
	}
	_ = removeLocalRecovery(x.layout.Root)
	from := st.KeySeq
	if from > 0 {
		from--
	}
	x.tell("identity.protect.done", a2ashape.Args{"from": from, "to": fin.KeySeq, "hub": hubOf(fin)})
	return nil
}

// protectUnjoined sets the passphrase before the hub has taken the identity in (L-10 step 4): no
// rotation happens; the join commits to the new seed once it goes through (KEL D-14).
func (x *identityCLI) protectUnjoined(t *ttySession, st daemon.IdentityStatus) error {
	cfg, err := daemon.LoadConfig(x.layout)
	if err != nil {
		return err
	}
	if cfg.HubURL == "" {
		return x.refuse("no_hub_yet", nil)
	}
	x.tell("identity.protect.confirm", idArgs(st, nil))
	u, err := x.unlock(t, st, "")
	if err != nil {
		return err
	}
	u.clear()
	newPass, err := x.askNewPassphrase(t)
	if err != nil {
		return err
	}
	defer clear(newPass)
	path, err := x.recoveryPath(st.AID)
	if err != nil {
		return err
	}
	rs, err := newSeed()
	if err != nil {
		return err
	}
	defer clear(rs[:])
	next, err := identity.NextDigest(rs, st.AID, 2)
	if err != nil {
		return err
	}
	file, err := sealRecovery(rs, st.AID, cfg.HubURL, identity.Commit{Seq: 2, Digest: next}, newPass)
	if err != nil {
		return err
	}
	np := path + ".new"
	if err := writeFileDurable(np, file, 0o600); err != nil {
		return err
	}
	if err := x.post("/hub-register", map[string]any{"hub": cfg.HubURL,
		"next_digest": b64enc(next[:])}, nil); err != nil {
		_ = os.Remove(np)
		return err
	}
	if err := renameDurable(np, path); err != nil {
		return err
	}
	syncDir(parentDir(path))
	_ = removeLocalRecovery(x.layout.Root)
	x.tell("identity.recovery_written", a2ashape.Args{"path": path})
	x.tell("identity.protect.done_unjoined", nil)
	return nil
}

// deactivate is `anet identity deactivate` (KEL L-6, D-46).
func (x *identityCLI) deactivate() error {
	if len(x.pos) > 0 {
		return usageError{"anet identity deactivate"}
	}
	t, err := x.openTerminal("recovery_passphrase")
	if err != nil {
		return err
	}
	defer t.Close()
	st, err := x.statusNow()
	if err != nil {
		return err
	}
	if x.c == nil {
		return x.needDaemon()
	}
	x.settleLeftovers(st)
	if err := x.changeRefusal(st); err != nil {
		return err
	}
	var dry struct {
		ClosedTasks int `json:"closed_tasks"`
	}
	if err := x.post("/identity/deactivate", map[string]any{"dry_run": true}, &dry); err != nil {
		return err
	}
	fmt.Fprintln(x.out, x.sentence("identity.deactivate.confirm", idArgs(st, nil)))
	if dry.ClosedTasks > 0 {
		fmt.Fprintln(x.out, x.sentence("identity.deactivate.tasks", a2ashape.Args{"n": dry.ClosedTasks}))
	}
	u, err := x.unlock(t, st, "")
	if err != nil {
		return err
	}
	defer u.clear()
	// An irreversible step is confirmed by typing part of the AID, never by yes (KEL L-6).
	ans, err := t.line(x.sentence("identity.deactivate.type_prefix", nil))
	want := st.AID
	if len(want) > 8 {
		want = want[len(want)-8:]
	}
	if err != nil || strings.TrimSpace(ans) != want {
		fmt.Fprintln(x.err, x.sentence("confirm.declined", nil))
		return errExit1
	}
	seed, err := seedAt(u.rs, st.AID, st.KeySeq+1, st.Commitment)
	if errors.Is(err, errMismatch) {
		return x.refuse("recovery_mismatch", nil)
	}
	if err != nil {
		return err
	}
	defer clear(seed)
	if err := x.post("/identity/deactivate", map[string]any{"revealed_seed": b64enc(seed)}, nil); err != nil {
		return err
	}
	// The dip is on disk: the files that could rotate this identity without a passphrase go (C-36).
	_ = removeLocalRecovery(x.layout.Root)
	hub := hubOf(st)
	deadline := time.Now().Add(idSettle)
	for {
		cur, _, err := x.pollStatus()
		if err != nil {
			return err
		}
		if cur.State == "deactivated" && len(cur.OldWitnessUnacked) == 0 {
			x.tell("identity.deactivate.done", a2ashape.Args{"path": x.layout.Root, "name": st.Name})
			return nil
		}
		if time.Now().After(deadline) {
			x.tell("identity_deactivated.unconfirmed", a2ashape.Args{"hub": hub})
			x.tell("identity.deactivate.done", a2ashape.Args{"path": x.layout.Root, "name": st.Name})
			return nil
		}
		time.Sleep(idPoll)
	}
}

// pollStatus reads /identity/status once.
func (x *identityCLI) pollStatus() (st daemon.IdentityStatus, code int, err error) {
	b, code, err := x.postRaw("/identity/status", map[string]any{})
	if err != nil {
		return st, 0, err
	}
	if code != 200 {
		return st, code, defaultHubCLI().refusedBy(code, b)
	}
	return st, code, json.Unmarshal(b, &st)
}

// rmWarnings are the two sentences `anet identity rm` adds before it deletes (KEL L-8): an identity
// that is on a hub and was not deactivated is not removed from the network by deleting its data, and
// a deactivation the witness has not confirmed stops being retried. The data directory is only read.
func rmWarnings(layout daemon.Layout, lang a2ashape.Language) {
	st, err := daemon.ReadIdentityStatus(layout, lang)
	if err != nil {
		return
	}
	switch {
	case st.State == "deactivated" && len(st.OldWitnessUnacked) > 0:
		fmt.Fprintln(os.Stderr, a2ashape.Sentence("identity.rm.dip_unacked", lang, nil))
	case st.State != "deactivated" && st.Witness != nil:
		fmt.Fprintln(os.Stderr, a2ashape.Sentence("identity.rm.not_deactivated", lang, nil))
	}
}

// oldHubAcked waits at most idOldHub for the old witness to confirm the hub switch (KEL L-4): the
// daemon pushes the key history to it as soon as the new hub has taken the rotation.
func (x *identityCLI) oldHubAcked(old string) bool {
	deadline := time.Now().Add(idOldHub)
	for {
		st, _, err := x.pollStatus()
		if err != nil {
			return false
		}
		pending := false
		for _, o := range st.OldWitnessUnacked {
			if sameURL(o.URL, old) {
				pending = true
			}
		}
		if !pending {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(idPoll)
	}
}

func sameURL(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }
