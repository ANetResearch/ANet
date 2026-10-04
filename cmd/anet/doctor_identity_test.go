package main

// doctor's identity checks (ux §5.2, KEL §5.4): identity.format,
// identity.witness, identity.recovery, identity.rotation.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// identityFindings are doctor's findings of the identity checks, by id.
func identityFindings(t *testing.T, l daemon.Layout, env doctorEnv) map[string]doctorCheck {
	t.Helper()
	before := snapshotDir(t, l.Root)
	rep, err := collectDoctor(l, env)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSnapshot(before, snapshotDir(t, l.Root)) {
		t.Error("doctor changed the data directory (UX-K1)")
	}
	out := map[string]doctorCheck{}
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.ID, "identity.") {
			if _, dup := out[c.ID]; dup {
				t.Errorf("%s twice", c.ID)
			}
			out[c.ID] = c
			if c.Status != stOK && c.Hint == "" {
				t.Errorf("%s is %s without a hint (UX-K4)", c.ID, c.Status)
			}
		}
	}
	return out
}

// writeIdentity replaces l's identity file with c's.
func writeIdentity(t *testing.T, l daemon.Layout, c *identity.Controller) {
	t.Helper()
	b, err := c.Export()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.IdentityPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// unprotectedIdentity is an identity that joined without a terminal (KEL
// D-11): local_commit is the commitment of its witness designation.
func unprotectedIdentity(t *testing.T) (*identity.Controller, [32]byte) {
	t.Helper()
	c, err := identity.InceptAgent(1)
	if err != nil {
		t.Fatal(err)
	}
	var rs [32]byte
	rs[0] = 9
	d, err := identity.NextDigest(rs, c.AID(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetLocalCommit(d[:]); err != nil {
		t.Fatal(err)
	}
	ev, err := c.BuildWitnessDesignation(identitytest.Witness, d[:], 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	return c, rs
}

func checkFinding(t *testing.T, step string, f map[string]doctorCheck, id, status, reason string, args a2ashape.Args) {
	t.Helper()
	c, ok := f[id]
	if !ok {
		t.Fatalf("%s: no %s finding", step, id)
	}
	if c.Status != status || c.Reason != reason {
		t.Fatalf("%s: %s is %s %q, want %s %q", step, id, c.Status, c.Reason, status, reason)
	}
	if reason != "" && c.Detail != a2ashape.Sentence(reason, a2ashape.English, args) {
		t.Fatalf("%s: %s says %q, want the sentence of %s", step, id, c.Detail, reason)
	}
}

// The four checks over a fresh identity, a 0.2 identity file, a witnessed
// one without a notice, one publishing a rotation, and one the running
// daemon reports.
func TestDoctorIdentityChecks(t *testing.T) {
	env := testDoctorEnv(t)
	env.getenv = func(k string) string {
		if k == "LANG" {
			return "C"
		}
		return ""
	}

	l := freshInit(t)
	if f := identityFindings(t, l, env); len(f) != 0 {
		t.Fatalf("no identity file yet: %v", f)
	}
	icp, err := identity.InceptAgent(1)
	if err != nil {
		t.Fatal(err)
	}
	writeIdentity(t, l, icp)
	f := identityFindings(t, l, env)
	checkFinding(t, "fresh", f, "identity.format", stOK, "", nil)
	checkFinding(t, "fresh", f, "identity.witness", stInfo, "no_hub_yet", nil)
	checkFinding(t, "fresh", f, "identity.recovery", stInfo, "identity.recovery_note", nil)
	checkFinding(t, "fresh", f, "identity.rotation", stOK, "", nil)

	old := freshInit(t)
	kel, err := coredet.Marshal(map[uint64]any{1: bytes.Repeat([]byte{1}, 32), 2: bytes.Repeat([]byte{2}, 32), 3: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old.IdentityPath(), kel, 0o600); err != nil {
		t.Fatal(err)
	}
	f = identityFindings(t, old, env)
	checkFinding(t, "0.2", f, "identity.format", stFail, "identity_v02", nil)
	if len(f) != 1 {
		t.Fatalf("0.2: %v, want identity.format alone", f)
	}

	// Witnessed, never confirmed by a notice.
	w := freshInit(t)
	c, err := identity.InceptAgent(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := identitytest.SwitchWitness(c, identitytest.Witness, 2); err != nil {
		t.Fatal(err)
	}
	writeIdentity(t, w, c)
	f = identityFindings(t, w, env)
	if c := f["identity.witness"]; c.Status != stFail || c.Reason != "notice_stale" {
		t.Fatalf("no notice: %+v", c)
	}
	checkFinding(t, "witnessed", f, "identity.recovery", stInfo, "identity.recovery_note", nil)

	// A rotation being published.
	next, err := identitytest.NextKey(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.BuildRotation(next, identitytest.NewNext(), identitytest.Witness, false, 3); err != nil {
		t.Fatal(err)
	}
	writeIdentity(t, w, c)
	f = identityFindings(t, w, env)
	checkFinding(t, "publishing", f, "identity.rotation", stWarn, "rotation_pending", nil)
	checkFinding(t, "publishing", f, "identity.witness", stWarn, "rotation_pending", nil)

	// The running daemon knows what the data directory does not: the hub
	// is in quarantine.
	live := freshInit(t)
	writeIdentity(t, live, c)
	lenv := env
	lenv.running = func(string, string) bool { return true }
	lenv.status = func(string, string) (map[string]any, error) {
		return map[string]any{"identity": map[string]any{"state": "notice_stale", "reason": "identity.quarantine",
			"message": a2ashape.Sentence("identity.quarantine", a2ashape.English, nil), "recovery": map[string]any{"state": "protected"}}}, nil
	}
	f = identityFindings(t, live, lenv)
	checkFinding(t, "live", f, "identity.witness", stWarn, "identity.quarantine", nil)
}

// identity.recovery for a protected identity: the recovery file at its
// default place and mode 0600 is ok; another mode is a warning, and so is
// a leftover of an unfinished change; no file there is the note (it may be
// kept elsewhere).
func TestDoctorRecoveryFile(t *testing.T) {
	env := testDoctorEnv(t)
	l := freshInit(t)
	c, err := identity.InceptAgent(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := identitytest.SwitchWitness(c, identitytest.Witness, 2); err != nil {
		t.Fatal(err)
	}
	writeIdentity(t, l, c)
	path := defaultRecoveryFile(env.home, c.AID())
	if !strings.HasSuffix(path, c.AID()[len(c.AID())-12:]+".anetrec") || filepath.Dir(filepath.Dir(path)) != env.home {
		t.Fatalf("the default place is %s", path)
	}
	checkFinding(t, "no file", identityFindings(t, l, env), "identity.recovery", stInfo, "identity.recovery_note", nil)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkFinding(t, "the file", identityFindings(t, l, env), "identity.recovery", stOK, "identity.status.recovery",
		a2ashape.Args{"path": filepath.Join("~", ".anet-recovery", filepath.Base(path))})
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	checkFinding(t, "readable", identityFindings(t, l, env), "identity.recovery", stWarn, "identity.recovery_perm",
		a2ashape.Args{"path": path})
	if err := os.WriteFile(path+".new", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkFinding(t, "leftover", identityFindings(t, l, env), "identity.recovery", stWarn, "identity.recovery_leftover",
		a2ashape.Args{"path": path + ".new"})
}

// M47 (KEL D-15), doctor's place: identity.recovery warns with the
// reminder the daemon gives in /identity/status, the status summary, MCP
// and the task projection (daemon TestUnprotectedReminderEverywhere) —
// the same sentence of the reason table — before anything else about
// the recovery file; .resume after the first of protect's two rotations;
// and nothing once both are done.
func TestDoctorSaysTheUnprotectedReminder(t *testing.T) {
	env := testDoctorEnv(t)
	l := freshInit(t)
	c, rs := unprotectedIdentity(t)
	writeIdentity(t, l, c)
	if err := os.MkdirAll(filepath.Join(env.home, recoveryDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultRecoveryFile(env.home, c.AID())+".new", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := identityFindings(t, l, env)
	checkFinding(t, "unprotected", f, "identity.recovery", stWarn, "identity.unprotected", nil)
	if f["identity.recovery"].Hint != "anet identity protect" {
		t.Fatalf("hint %q", f["identity.recovery"].Hint)
	}
	st, err := daemon.ReadIdentityStatus(l, a2ashape.English)
	if err != nil || st.Recovery.Message != f["identity.recovery"].Detail {
		t.Fatalf("doctor says %q, /identity/status %q (%v)", f["identity.recovery"].Detail, st.Recovery.Message, err)
	}
	var pass [32]byte
	pass[0] = 10
	rotate := func(from, to [32]byte, j uint64) {
		t.Helper()
		revealed, err := identity.RecoverySeed(from, c.AID(), j)
		if err != nil {
			t.Fatal(err)
		}
		next, err := identity.NextDigest(to, c.AID(), j+1)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := c.BuildRotation(revealed, next[:], identitytest.Witness, false, j+1)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Adopt(ev); err != nil {
			t.Fatal(err)
		}
		writeIdentity(t, l, c)
	}
	rotate(rs, pass, 2)
	checkFinding(t, "half protected", identityFindings(t, l, env), "identity.recovery", stWarn, "identity.unprotected.resume", nil)
	rotate(pass, pass, 3)
	f = identityFindings(t, l, env)
	if r := f["identity.recovery"].Reason; r == "identity.unprotected" || r == "identity.unprotected.resume" {
		t.Fatalf("protected: %+v", f["identity.recovery"])
	}
}
