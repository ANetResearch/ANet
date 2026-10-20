package main

// hubjoin_recovery.go is what `anet hub-register` does with the recovery seed (KEL D-10, D-11, L-2,
// L-4): a first join commits to the key the recovery seed derives for the first rotation, and
// switching hubs is a rotation that needs the seed open.

import (
	"errors"
	"os"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// joinRecovery is the next_digest a first join commits to, whether the seed it comes from has no
// passphrase (no terminal, KEL D-11), and what to do once the daemon has taken the digest (delete
// the unprotected files a join with a passphrase replaces). With a terminal the user sets or enters
// the recovery passphrase (D-10); without one, or with --passphrase-later, the seed is kept under a
// local key in the data directory (C-36) and the identity is reported as having no passphrase. A
// variable so that a test can replace it.
var joinRecovery = func(h hubCLI, dataDir, hub string, flags map[string]string) (digest []byte, unprotected bool, done func(), err error) {
	layout := daemon.Layout{Root: dataDir}
	st, err := daemon.ReadIdentityStatus(layout, h.lang)
	if err != nil {
		return nil, false, nil, err
	}
	x := &identityCLI{layout: layout, flags: flags, lang: h.lang, out: h.stdout, err: h.stderr}
	if passphraseLater(flags) || !h.terminal() {
		d, err := x.joinWithoutTerminal(st, hub)
		return d, true, nil, err
	}
	return x.joinWithTerminal(st, hub)
}

// passphraseLater reports `hub-register --passphrase-later`: the join takes the path of KEL D-11 even
// when a terminal is there, and the recovery passphrase is set afterwards with anet identity protect.
func passphraseLater(flags map[string]string) bool { return flags["passphrase-later"] == "true" }

// joinWithoutTerminal is KEL D-11: the join goes through, the passphrase comes later. A file or a
// new seed asked for by flag needs a terminal, and says so.
func (x *identityCLI) joinWithoutTerminal(st daemon.IdentityStatus, hub string) ([]byte, error) {
	for _, f := range []string{"recovery-file", "new-recovery"} {
		if _, ok := x.flags[f]; ok {
			return nil, x.refuse("tty_required", a2ashape.Args{"action": "set_recovery_passphrase", "command": typedCommand(commandLine())})
		}
	}
	// Run again after an interruption: the files that are there are used again (D-11 step 2).
	if rs, aid, _, _, err := readLocalRecovery(x.layout.Root); err == nil && aid == st.AID {
		defer clear(rs[:])
		next, err := identity.NextDigest(rs, st.AID, 2)
		return next[:], err
	}
	rs, err := newSeed()
	if err != nil {
		return nil, err
	}
	defer clear(rs[:])
	next, err := identity.NextDigest(rs, st.AID, 2)
	if err != nil {
		return nil, err
	}
	if err := writeLocalRecovery(x.layout.Root, rs, st.AID, hub, identity.Commit{Seq: 2, Digest: next},
		recoveryNote("identity.recovery_file_note_unprotected")...); err != nil {
		return nil, err
	}
	return next[:], nil
}

// joinWithTerminal is KEL D-10: an existing recovery file of this identity is opened with its
// passphrase and used again; otherwise the user sets a passphrase and the file is written before the
// daemon is asked. A pair of unprotected files from an earlier join without a terminal is not used:
// it is deleted once the daemon has the new digest.
func (x *identityCLI) joinWithTerminal(st daemon.IdentityStatus, hub string) (digest []byte, unprotected bool, done func(), err error) {
	t, err := x.openTerminal("set_recovery_passphrase")
	if err != nil {
		return nil, false, nil, err
	}
	defer t.Close()
	newRec := x.flags["new-recovery"] == "true"
	if newRec && (st.KeySeq != 0 || st.Pending != nil) {
		return nil, false, nil, x.refuse("rotation_pending", nil)
	}
	path, err := x.recoveryPath(st.AID)
	if err != nil {
		return nil, false, nil, err
	}
	if b, rerr := os.ReadFile(path); rerr == nil && !newRec {
		next, err := x.reuseRecoveryFile(t, st, b)
		return next, false, x.afterJoin(), err
	} else if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return nil, false, nil, rerr
	}
	x.tell("identity.prompt.first_join", nil)
	pass, err := x.askNewPassphrase(t)
	if err != nil {
		return nil, false, nil, err
	}
	defer clear(pass)
	rs, err := newSeed()
	if err != nil {
		return nil, false, nil, err
	}
	defer clear(rs[:])
	next, err := identity.NextDigest(rs, st.AID, 2)
	if err != nil {
		return nil, false, nil, err
	}
	file, err := sealRecovery(rs, st.AID, hub, identity.Commit{Seq: 2, Digest: next}, pass)
	if err != nil {
		return nil, false, nil, err
	}
	if err := writeFileDurable(path, file, 0o600); err != nil {
		return nil, false, nil, err
	}
	syncDir(parentDir(path))
	x.tell("identity.recovery_written", a2ashape.Args{"path": path})
	return next[:], false, x.afterJoin(), nil
}

// reuseRecoveryFile opens the recovery file b of this identity with its passphrase and returns the
// digest of the first rotation's commitment.
func (x *identityCLI) reuseRecoveryFile(t *ttySession, st daemon.IdentityStatus, b []byte) ([]byte, error) {
	info, err := identity.ParseRecoveryFile(b)
	switch {
	case err != nil:
		return nil, x.refuse("recovery_wrong_identity", idArgs(st, a2ashape.Args{"file_aid": "unknown"}))
	case info.Unprotected:
		return nil, x.refuse("recovery_file_unprotected", nil)
	case info.AID != st.AID:
		return nil, x.refuse("recovery_wrong_identity", idArgs(st, a2ashape.Args{"file_aid": info.AID}))
	}
	pass, err := readPassphrase(t, x.sentence("identity.prompt.passphrase", nil))
	if err != nil {
		return nil, x.refuse("tty_required", a2ashape.Args{"action": "recovery_passphrase", "command": typedCommand(commandLine())})
	}
	defer clear(pass)
	if err := x.passphraseIsUTF8(pass); err != nil {
		return nil, err
	}
	rs, aid, _, _, err := identity.OpenRecoveryFile(b, pass)
	defer clear(rs[:])
	switch {
	case errors.Is(err, identity.ErrRecoveryPassphrase):
		return nil, x.refuse("recovery_passphrase_wrong", nil)
	case err != nil || aid != st.AID:
		return nil, x.refuse("recovery_wrong_identity", idArgs(st, a2ashape.Args{"file_aid": info.AID}))
	}
	next, err := identity.NextDigest(rs, st.AID, 2)
	return next[:], err
}

// afterJoin deletes the unprotected files a join with a passphrase leaves behind (KEL D-10 step 2).
func (x *identityCLI) afterJoin() func() {
	return func() { _ = removeLocalRecovery(x.layout.Root) }
}

// switchHub is `anet hub-register <other hub>` on an identity that has a witness (KEL L-4): the new
// hub is checked first, without a passphrase; then the recovery seed is opened and one key rotation
// names the new hub as the witness. An identity without a passphrase does it in two (D-16).
func (h hubCLI) switchHub(c *client, hub string, flags map[string]string) error {
	x := &identityCLI{layout: daemon.Layout{Root: c.dataDir}, explicit: true, flags: flags, lang: h.lang,
		out: h.stdout, err: h.stderr, c: c}
	st, _, err := x.pollStatus()
	if err != nil {
		return err
	}
	x.settleLeftovers(st)
	if err := x.changeRefusal(st); err != nil {
		return err
	}
	if _, err := x.dryRun("scheduled", hub, false); err != nil {
		return err
	}
	t, err := x.openTerminal("recovery_passphrase")
	if err != nil {
		return err
	}
	defer t.Close()
	old := hubOf(st)
	x.tell("identity.switch.confirm", a2ashape.Args{"old": old, "hub": hub})
	unprotected := st.Recovery.State == "unprotected"
	if unprotected {
		x.tell("identity.protect.confirm", idArgs(st, nil))
	}
	u, err := x.unlock(t, st, "")
	if err != nil {
		return err
	}
	defer u.clear()
	var fin daemon.IdentityStatus
	var ok bool
	if unprotected {
		newPass, err := x.askNewPassphrase(t)
		if err != nil {
			return err
		}
		defer clear(newPass)
		var path string
		if fin, ok, path, err = x.swapSeeds(st, u, newPass, "scheduled", hub); err != nil {
			return err
		}
		if ok {
			_ = removeLocalRecovery(x.layout.Root)
			x.tell("identity.recovery_written", a2ashape.Args{"path": path})
		}
	} else {
		path, err := x.recoveryPath(st.AID)
		if err != nil {
			return err
		}
		sp := stepSpec{st: st, rsOld: u.rs, rsNew: u.rs, mode: "scheduled", hubURL: hub, newPath: path, newPass: u.pass, fileHub: hub}
		if err := x.step(sp); err != nil {
			return err
		}
		if fin, ok, err = x.settle(st.KeySeq + 1); err != nil {
			return err
		}
	}
	if !ok {
		return x.stillPublishing()
	}
	x.tell("identity.switch.done", a2ashape.Args{"hub": hub, "from": st.KeySeq, "to": fin.KeySeq})
	if x.oldHubAcked(old) {
		x.tell("identity.switch.old_acked", a2ashape.Args{"old": old})
	} else {
		x.tell("old_hub_unacked", a2ashape.Args{"hub": old})
	}
	return nil
}
