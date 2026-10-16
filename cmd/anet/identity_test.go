package main

// Tests of `anet identity` and the recovery flow of `anet hub-register` (KEL-10) against a fake control plane
// that behaves like the daemon's /identity/* routes.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// freshIdentityDir is a data directory with a new agent identity in it, and its AID.
func freshIdentityDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	return dir, writeTestIdentity(t, dir)
}

// writeTestIdentity puts a new agent identity in dir and returns its AID.
func writeTestIdentity(t *testing.T, dir string) string {
	t.Helper()
	c, err := identity.InceptAgent(uint64(time.Now().UnixMilli()))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := c.Export()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.kel"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return c.AID()
}

// idFakeTTY is a terminal: what the user types, and what is written to it.
type idFakeTTY struct {
	in  *bytes.Buffer
	out bytes.Buffer
}

func (f *idFakeTTY) Read(p []byte) (int, error)  { return f.in.Read(p) }
func (f *idFakeTTY) Write(p []byte) (int, error) { return f.out.Write(p) }
func (f *idFakeTTY) Close() error                { return nil }

func idTTY(t *testing.T, typed ...string) *idFakeTTY {
	t.Helper()
	ft := &idFakeTTY{in: bytes.NewBufferString(strings.Join(typed, "\n") + "\n")}
	prev := openTTY
	openTTY = func() (io.ReadWriteCloser, error) { return ft, nil }
	t.Cleanup(func() { openTTY = prev })
	return ft
}

func withoutTTY(t *testing.T) {
	t.Helper()
	prev := openTTY
	openTTY = func() (io.ReadWriteCloser, error) { return nil, errors.New("no terminal") }
	t.Cleanup(func() { openTTY = prev })
}

// idDaemon is a control plane with the state machine of /identity/status and /identity/rotate: a
// rotation checks the revealed key against the commitment, and moves the commitment on.
type idDaemon struct {
	mu         sync.Mutex
	st         daemon.IdentityStatus
	posts      []map[string]any
	paths      []string
	publish    bool // a rotation stays pending until released
	witnessUp  bool
	unackedFor int // status calls the old witness stays unacked
}

func newIDDaemon(aid string, commitment []byte) *idDaemon {
	return &idDaemon{witnessUp: true, st: daemon.IdentityStatus{AID: aid, Name: "coder", State: "ok", KeySeq: 1, Commitment: commitment,
		Witness:           &daemon.IdentityWitness{URL: "https://hub.example", AID: "bafyreihub"},
		OldWitnessUnacked: []daemon.IdentityOldWitness{}}}
}

// daemonRecovery is the recovery object of a status, built from its JSON form.
func daemonRecovery(message, next string) (r struct {
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	Next    string `json:"next,omitempty"`
}) {
	r.State, r.Message, r.Next = "unprotected", message, next
	return r
}

func (d *idDaemon) setRecovery(state string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = json.Unmarshal([]byte(`{"recovery":{"state":"`+state+`"}}`), &d.st)
}

func (d *idDaemon) serve(t *testing.T, dataDir string) *client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(d.handle)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &client{base: "http://" + ln.Addr().String(), token: "t", timeout: 10 * time.Second, dataDir: dataDir}
}

func (d *idDaemon) handle(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paths = append(d.paths, r.URL.Path)
	reply := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	refuse := func(code int, key string, args a2ashape.Args) {
		reply(code, a2ashape.Refuse(key, args).Body(a2ashape.English))
	}
	switch r.URL.Path {
	case "/identity/status":
		if d.unackedFor > 0 {
			d.unackedFor--
		} else {
			d.st.OldWitnessUnacked = []daemon.IdentityOldWitness{}
		}
		reply(200, d.st)
	case "/identity/rotate":
		d.posts = append(d.posts, body)
		if body["dry_run"] == true {
			if !d.witnessUp {
				refuse(503, "witness_unreachable", a2ashape.Args{"hub": "https://hub.example"})
				return
			}
			reply(200, map[string]any{"state": d.st.State})
			return
		}
		seed, _ := base64.StdEncoding.DecodeString(body["revealed_seed"].(string))
		next, _ := base64.StdEncoding.DecodeString(body["next_digest"].(string))
		sum := sha256.Sum256(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
		if string(sum[:]) != string(d.st.Commitment) {
			refuse(409, "recovery_mismatch", nil)
			return
		}
		d.st.KeySeq++
		d.st.Commitment = next
		if u, _ := body["hub_url"].(string); u != "" {
			d.st.OldWitnessUnacked = []daemon.IdentityOldWitness{{URL: d.st.Witness.URL}}
			d.unackedFor = 1 << 20
			d.st.Witness.URL = u
		}
		if d.publish {
			d.st.Pending = &daemon.IdentityPending{Kind: "rot"}
			d.st.State = "publishing"
		}
		reply(200, map[string]any{"state": d.st.State, "key_seq": d.st.KeySeq})
	case "/identity/deactivate":
		d.posts = append(d.posts, body)
		if body["dry_run"] == true {
			reply(200, map[string]any{"state": "ok", "closed_tasks": 2})
			return
		}
		d.st.State = "deactivated"
		reply(200, map[string]any{"state": "deactivated", "closed_tasks": 2})
	case "/hub-register":
		d.posts = append(d.posts, body)
		reply(200, map[string]any{"hub": body["hub"], "state": "pending_admission"})
	default:
		reply(404, map[string]any{"error": "no route"})
	}
}

func (d *idDaemon) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.st.Pending, d.st.State = nil, "ok"
}

func (d *idDaemon) rotations() []map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []map[string]any
	for _, p := range d.posts {
		if p["dry_run"] != true && p["revealed_seed"] != nil {
			out = append(out, p)
		}
	}
	return out
}

// idFix is an identity with a recovery seed, a daemon that follows the rotations, and a CLI on them.
type idFix struct {
	t    *testing.T
	dir  string
	aid  string
	home string
	path string // the recovery file
	rs   [32]byte
	d    *idDaemon
	c    *client
	out  bytes.Buffer
	errb bytes.Buffer
}

const testPass = "correct horse battery"

func newIDFix(t *testing.T, protected bool) *idFix {
	t.Helper()
	f := &idFix{t: t}
	f.dir, f.aid = freshIdentityDir(t)
	f.home = t.TempDir()
	prevHome, prevPoll, prevSettle, prevOld := idHome, idPoll, idSettle, idOldHub
	idHome = func() (string, error) { return f.home, nil }
	idPoll, idSettle, idOldHub = 5*time.Millisecond, 2*time.Second, 100*time.Millisecond
	t.Cleanup(func() { idHome, idPoll, idSettle, idOldHub = prevHome, prevPoll, prevSettle, prevOld })
	var err error
	if f.rs, err = newSeed(); err != nil {
		t.Fatal(err)
	}
	next, err := identity.NextDigest(f.rs, f.aid, 2)
	if err != nil {
		t.Fatal(err)
	}
	f.path = defaultRecoveryFile(f.home, f.aid)
	if protected {
		file, err := sealRecovery(f.rs, f.aid, "https://hub.example", identity.Commit{Seq: 2, Digest: next}, []byte(testPass))
		if err != nil {
			t.Fatal(err)
		}
		if err := writeFileDurable(f.path, file, 0o600); err != nil {
			t.Fatal(err)
		}
	} else if err := writeLocalRecovery(f.dir, f.rs, f.aid, "https://hub.example", identity.Commit{Seq: 2, Digest: next}); err != nil {
		t.Fatal(err)
	}
	f.d = newIDDaemon(f.aid, next[:])
	if protected {
		f.d.setRecovery("protected")
	} else {
		f.d.setRecovery("unprotected")
	}
	f.c = f.d.serve(t, f.dir)
	return f
}

func (f *idFix) cli(flags map[string]string, pos ...string) *identityCLI {
	if flags == nil {
		flags = map[string]string{}
	}
	return &identityCLI{layout: daemon.Layout{Root: f.dir}, explicit: true, pos: pos, flags: flags,
		lang: a2ashape.English, out: &f.out, err: &f.errb, c: f.c}
}

func (f *idFix) sentence(key string, args a2ashape.Args) string {
	return a2ashape.Sentence(key, a2ashape.English, args)
}

// openedFile opens the recovery file now at f.path with pass.
func (f *idFix) openedFile(pass string) (rs [32]byte, commits identity.Commit) {
	f.t.Helper()
	b, err := os.ReadFile(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	rs, _, _, commits, err = identity.OpenRecoveryFile(b, []byte(pass))
	if err != nil {
		f.t.Fatalf("recovery file does not open: %v", err)
	}
	return rs, commits
}

// seedOf is the seed rs derives for the event at seq.
func seedOf(t *testing.T, rs [32]byte, aid string, seq uint64) string {
	t.Helper()
	k, err := identity.RecoverySeed(rs, aid, seq)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k.Seed())
}

// KEL L-3: a scheduled rotation opens the file with the passphrase, reveals the key the seed derives
// and prints the result.
func TestRotateOpensTheFileAndRevealsTheNextKey(t *testing.T) {
	f := newIDFix(t, true)
	idTTY(t, testPass)
	if err := f.cli(nil).rotate(); err != nil {
		t.Fatalf("rotate: %v\n%s", err, f.errb.String())
	}
	rot := f.d.rotations()
	if len(rot) != 1 || rot[0]["mode"] != "scheduled" || rot[0]["revealed_seed"] != seedOf(t, f.rs, f.aid, 2) {
		t.Fatalf("rotations %v", rot)
	}
	if !strings.Contains(f.out.String(), f.sentence("identity.rotate.done", a2ashape.Args{"from": 1, "to": 2, "hub": "https://hub.example"})) {
		t.Fatalf("output %q", f.out.String())
	}
	// The file is written again with the commitment for the key after this rotation, same seed and
	// passphrase: `identity recover` relies on it to refuse a stale history (T-1 RT-F8).
	rs, commits := f.openedFile(testPass)
	if rs != f.rs || commits.Seq != 3 {
		t.Fatalf("the recovery file after a plain rotation: same seed %v, commits to seq %d, want 3", rs == f.rs, commits.Seq)
	}
	if _, err := os.Stat(f.path + ".new"); err == nil {
		t.Fatal(".new left behind")
	}
}

// KEL D-31a: a new recovery seed is two rotations of the same mode; the second reveals the key of the
// new seed, and the file left behind is the new seed's.
func TestNewRecoveryRotatesTwice(t *testing.T) {
	for _, emergency := range []bool{false, true} {
		f := newIDFix(t, true)
		newPass := "an entirely new passphrase"
		flags := map[string]string{"new-recovery": "true"}
		typed := []string{testPass, newPass, newPass}
		mode := "scheduled"
		if emergency {
			flags = map[string]string{"emergency": "true"}
			typed = append(typed, "yes")
			mode = "emergency"
		}
		idTTY(t, typed...)
		if err := f.cli(flags).rotate(); err != nil {
			t.Fatalf("emergency=%v: %v\n%s", emergency, err, f.errb.String())
		}
		rot := f.d.rotations()
		if len(rot) != 2 || rot[0]["mode"] != mode || rot[1]["mode"] != mode {
			t.Fatalf("emergency=%v: rotations %v", emergency, rot)
		}
		rsNew, commits := f.openedFile(newPass)
		if rsNew == f.rs {
			t.Fatal("the recovery seed did not change")
		}
		if rot[0]["revealed_seed"] != seedOf(t, f.rs, f.aid, 2) || rot[1]["revealed_seed"] != seedOf(t, rsNew, f.aid, 3) {
			t.Fatalf("emergency=%v: the second rotation must reveal the new seed's key: %v", emergency, rot)
		}
		if commits.Seq != 4 {
			t.Fatalf("emergency=%v: the file commits to seq %d, want 4", emergency, commits.Seq)
		}
		if _, err := os.Stat(f.path + ".new"); err == nil {
			t.Fatal(".new left behind")
		}
		if emergency && !strings.Contains(f.out.String(), f.sentence("rotation_emergency_done", nil)) {
			t.Fatalf("output %q", f.out.String())
		}
	}
}

// KEL L-10, D-16: protect on an identity without a passphrase rotates twice, replaces the seed and
// deletes the two local files; it never needs the old passphrase.
func TestUnprotectedRotationPublishesTwoEvents(t *testing.T) {
	f := newIDFix(t, false)
	newPass := "my new recovery passphrase"
	idTTY(t, newPass, newPass)
	if err := f.cli(nil).protect(); err != nil {
		t.Fatalf("protect: %v\n%s", err, f.errb.String())
	}
	if rot := f.d.rotations(); len(rot) != 2 {
		t.Fatalf("rotations %v", rot)
	}
	if localRecoveryPresent(f.dir) {
		t.Fatal("the unprotected files are still there")
	}
	f.openedFile(newPass)
	if !strings.Contains(f.out.String(), f.sentence("identity.protect.done", a2ashape.Args{"from": 1, "to": 3, "hub": "https://hub.example"})) {
		t.Fatalf("output %q", f.out.String())
	}
}

// KEL L-1: without a terminal nothing is asked of the daemon that changes the identity.
func TestNoTerminalIsRefusedBeforeAnything(t *testing.T) {
	for _, run := range []struct {
		name string
		do   func(*idFix) error
	}{
		{"rotate", func(f *idFix) error { return f.cli(nil).rotate() }},
		{"protect", func(f *idFix) error { return f.cli(nil).protect() }},
		{"deactivate", func(f *idFix) error { return f.cli(nil).deactivate() }},
		{"recover", func(f *idFix) error { return f.cli(nil, f.path).recover() }},
	} {
		f := newIDFix(t, true)
		withoutTTY(t)
		err := run.do(f)
		if run.name == "recover" {
			// The file is read first; it is a real file, so the terminal is what is missing.
			err = f.cli(map[string]string{"hub": "https://hub.example"}, f.path).recover()
		}
		if err != errExit1 || !strings.Contains(f.errb.String(), "terminal") {
			t.Errorf("%s: %v, said %q", run.name, err, f.errb.String())
		}
		if len(f.d.paths) != 0 {
			t.Errorf("%s asked the daemon: %v", run.name, f.d.paths)
		}
	}
}

// KEL L-5: a recovery file without a passphrase cannot be used to recover.
func TestRecoverRefusesUnprotectedFile(t *testing.T) {
	f := newIDFix(t, false)
	idTTY(t)
	err := f.cli(map[string]string{"hub": "https://hub.example"}, localRecPath(f.dir)).recover()
	if err != errExit1 || !strings.Contains(f.errb.String(), f.sentence("recovery_file_unprotected", nil)) {
		t.Fatalf("%v, said %q", err, f.errb.String())
	}
}

// KEL C-34: a passphrase that is not UTF-8 is refused where it is set and where it opens a file.
func TestPassphraseMustBeUTF8(t *testing.T) {
	f := newIDFix(t, true)
	idTTY(t, "\xc4\xe3\xba\xc3\xb7\xa2\xb0\xb2\xc8\xab")
	if err := f.cli(nil).rotate(); err != errExit1 || !strings.Contains(f.errb.String(), f.sentence("passphrase_not_utf8", nil)) {
		t.Fatalf("opening: %v, said %q", err, f.errb.String())
	}
	f = newIDFix(t, true)
	idTTY(t, testPass, "\xc4\xe3\xba\xc3\xb7\xa2\xb0\xb2\xc8\xab")
	if err := f.cli(map[string]string{"new-recovery": "true"}).rotate(); err != errExit1 || !strings.Contains(f.errb.String(), f.sentence("passphrase_not_utf8", nil)) {
		t.Fatalf("setting: %v, said %q", err, f.errb.String())
	}
	if len(f.d.rotations()) != 0 {
		t.Fatal("a rotation was asked for")
	}
}

// KEL L-3: a recovery file of an older generation is recovery_mismatch and the daemon is not asked.
func TestRotateWithAnOldFileIsAMismatch(t *testing.T) {
	f := newIDFix(t, true)
	f.d.st.Commitment = bytes.Repeat([]byte{7}, 32)
	idTTY(t, testPass)
	if err := f.cli(nil).rotate(); err != errExit1 || !strings.Contains(f.errb.String(), f.sentence("recovery_mismatch", nil)) {
		t.Fatalf("%v, said %q", err, f.errb.String())
	}
	if len(f.d.rotations()) != 0 {
		t.Fatal("a rotation was asked for")
	}
}

// KEL L-3: an interrupted two-phase write is finished when its commitment is the daemon's, deleted
// when it is not, with no passphrase.
func TestLeftoverNewFileIsSettled(t *testing.T) {
	f := newIDFix(t, true)
	good, err := sealRecovery(f.rs, f.aid, "https://hub.example", identity.Commit{Seq: 2, Digest: [32]byte(f.d.st.Commitment)}, []byte("another long passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path+".new", good, 0o600); err != nil {
		t.Fatal(err)
	}
	x := f.cli(nil)
	st, _, _ := x.pollStatus()
	x.settleLeftovers(st)
	f.openedFile("another long passphrase")
	stale, _ := sealRecovery(f.rs, f.aid, "", identity.Commit{Seq: 9, Digest: [32]byte{1}}, []byte("stale stale stale"))
	_ = os.WriteFile(f.path+".new", stale, 0o600)
	x.settleLeftovers(st)
	if _, err := os.Stat(f.path + ".new"); err == nil {
		t.Fatal("a .new that commits to another key was kept")
	}
	f.openedFile("another long passphrase")
}

// KEL L-10: protect on a protected identity says so and exits 0.
func TestProtectAlready(t *testing.T) {
	f := newIDFix(t, true)
	idTTY(t)
	if err := f.cli(nil).protect(); err != nil || !strings.Contains(f.out.String(), f.sentence("identity.protect.already", nil)) {
		t.Fatalf("%v, said %q", err, f.out.String())
	}
}

// KEL L-6: deactivating needs the last 8 characters of the AID, never yes.
func TestDeactivateNeedsThePrefix(t *testing.T) {
	f := newIDFix(t, true)
	idTTY(t, testPass, "yes")
	if err := f.cli(nil).deactivate(); err != errExit1 {
		t.Fatalf("yes was accepted: %v", err)
	}
	if len(f.d.rotations()) != 0 {
		t.Fatal("deactivated")
	}
	f = newIDFix(t, true)
	idTTY(t, testPass, f.aid[len(f.aid)-8:])
	if err := f.cli(nil).deactivate(); err != nil {
		t.Fatalf("%v, said %q", err, f.errb.String())
	}
	if !strings.Contains(f.out.String(), f.sentence("identity.deactivate.tasks", a2ashape.Args{"n": 2})) {
		t.Fatalf("output %q", f.out.String())
	}
}

// KEL L-4: the CLI waits at most idOldHub for the old witness, and exits 0 with old_hub_unacked.
func TestSwitchWaitsAtMost10sForOldHub(t *testing.T) {
	f := newIDFix(t, true)
	idTTY(t, testPass)
	var out, errb bytes.Buffer
	h := hubCLI{stdout: &out, stderr: &errb, getenv: func(string) string { return "" }, terminal: func() bool { return true }, lang: a2ashape.English}
	start := time.Now()
	if err := h.switchHub(f.c, "https://hub2.example", map[string]string{}); err != nil {
		t.Fatalf("%v, said %q", err, errb.String())
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("waited too long")
	}
	if !strings.Contains(out.String(), a2ashape.Sentence("old_hub_unacked", a2ashape.English, a2ashape.Args{"hub": "https://hub.example"})) ||
		!strings.Contains(out.String(), a2ashape.Sentence("identity.switch.done", a2ashape.English, a2ashape.Args{"hub": "https://hub2.example", "from": 1, "to": 2})) {
		t.Fatalf("output %q", out.String())
	}
	rot := f.d.rotations()
	if len(rot) != 1 || rot[0]["hub_url"] != "https://hub2.example" {
		t.Fatalf("rotations %v", rot)
	}
}

// KEL D-11: without a terminal a first join keeps the seed under a local key, and a second run uses it again.
func TestNoTTYJoinReusesLocalSeed(t *testing.T) {
	dir, aid := freshIdentityDir(t)
	var out, errb bytes.Buffer
	h := hubCLI{stdout: &out, stderr: &errb, terminal: func() bool { return false }, lang: a2ashape.English}
	d1, un1, _, err := joinRecovery(h, dir, "https://hub.example", map[string]string{})
	if err != nil || !un1 {
		t.Fatalf("%v %v", err, un1)
	}
	d2, _, _, err := joinRecovery(h, dir, "https://hub.example", map[string]string{})
	if err != nil || !bytes.Equal(d1, d2) {
		t.Fatalf("the second run committed to another seed: %v", err)
	}
	rs, got, _, commits, err := readLocalRecovery(dir)
	if err != nil || got != aid || commits.Seq != 2 {
		t.Fatalf("local file: %v %v %v", err, got, commits)
	}
	want, _ := identity.NextDigest(rs, aid, 2)
	if !bytes.Equal(d1, want[:]) {
		t.Fatal("the digest is not the seed's")
	}
	// A flag that needs a terminal is refused.
	if _, _, _, err := joinRecovery(h, dir, "https://hub.example", map[string]string{"new-recovery": "true"}); err != errExit1 {
		t.Fatalf("new-recovery without a terminal: %v", err)
	}
}

// KEL D-10: with a terminal the user sets the passphrase, the file is written first, and the
// unprotected files of an earlier join are deleted once the daemon has the digest.
func TestJoinWithTerminalWritesTheFile(t *testing.T) {
	dir, aid := freshIdentityDir(t)
	home := t.TempDir()
	prev := idHome
	idHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { idHome = prev })
	old := [32]byte{1}
	oldNext, _ := identity.NextDigest(old, aid, 2)
	if err := writeLocalRecovery(dir, old, aid, "https://hub.example", identity.Commit{Seq: 2, Digest: oldNext}); err != nil {
		t.Fatal(err)
	}
	idTTY(t, testPass, testPass)
	var out, errb bytes.Buffer
	h := hubCLI{stdout: &out, stderr: &errb, terminal: func() bool { return true }, lang: a2ashape.English}
	digest, un, done, err := joinRecovery(h, dir, "https://hub.example", map[string]string{})
	if err != nil || un {
		t.Fatalf("%v %v %q", err, un, errb.String())
	}
	b, err := os.ReadFile(defaultRecoveryFile(home, aid))
	if err != nil {
		t.Fatal(err)
	}
	rs, _, _, commits, err := identity.OpenRecoveryFile(b, []byte(testPass))
	want, _ := identity.NextDigest(rs, aid, 2)
	if err != nil || commits.Seq != 2 || !bytes.Equal(digest, want[:]) {
		t.Fatalf("the file does not match the digest: %v", err)
	}
	if !localRecoveryPresent(dir) {
		t.Fatal("the unprotected files went before the daemon had the digest")
	}
	done()
	if localRecoveryPresent(dir) {
		t.Fatal("the unprotected files were kept")
	}
}

// KEL-10: a daemon that holds the data directory without answering stops `retention set` before it writes.
func TestRetentionSetWaitsForABusyDaemon(t *testing.T) {
	dir := t.TempDir()
	layout := daemon.Layout{Root: dir}
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"content_retention":"30d"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := daemon.AcquireDataLock(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	before, _ := os.ReadFile(cfg)
	prevD, prevOut, prevErr := retentionDaemon, retentionStdout, retentionStderr
	var out, errb bytes.Buffer
	retentionDaemon = func(daemon.Layout) *client { return nil }
	retentionStdout, retentionStderr = &out, &errb
	t.Cleanup(func() { retentionDaemon, retentionStdout, retentionStderr = prevD, prevOut, prevErr })
	idTTY(t, "yes")
	if err := setRetention(layout, "7d"); err != errExit1 {
		t.Fatalf("%v, said %q", err, errb.String())
	}
	if !strings.Contains(errb.String(), "did not answer within") {
		t.Fatalf("said %q", errb.String())
	}
	after, _ := os.ReadFile(cfg)
	if !bytes.Equal(before, after) {
		t.Fatal("config.json changed")
	}
}

// KEL L-1: the passphrase has no way in but the terminal: there is no flag or variable for it.
func TestPassphraseNeverInArgvEnvOrLog(t *testing.T) {
	for _, flag := range []string{"passphrase", "recovery-passphrase", "password"} {
		if err := checkFlags("identity", []string{"rotate", "--" + flag, "x"}); err == nil {
			t.Errorf("anet identity takes --%s", flag)
		}
	}
	src, err := os.ReadFile("identity.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "Getenv(\"ANET_RECOVERY") || strings.Contains(string(src), "ANET_RECOVERY_PASSPHRASE") {
		t.Error("the passphrase is read from the environment")
	}
}

// KEL D-45: recover reads the hub's record, checks it against the file, writes a new data directory with
// the first rotation pending and a new recovery file, and starts the daemon there.
func TestRecoverWritesTheNewDataDirectory(t *testing.T) {
	hubID, err := identity.InceptHub(uint64(time.Now().UnixMilli()))
	if err != nil {
		t.Fatal(err)
	}
	now := uint64(time.Now().UnixMilli())
	agent, err := identity.InceptAgent(now)
	if err != nil {
		t.Fatal(err)
	}
	aid := agent.AID()
	rs, _ := newSeed()
	next, _ := identity.NextDigest(rs, aid, 2)
	ev, err := agent.BuildWitnessDesignation(hubID.AID(), next[:], now+10)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	raw, err := identity.MarshalKEL(agent.KEL())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/identity":
			_ = json.NewEncoder(w).Encode(map[string]any{"aid": hubID.AID(), "wire": 3})
		case "/agents/" + aid + "/kel":
			_ = json.NewEncoder(w).Encode(map[string]any{"aid": aid, "kel": base64.RawURLEncoding.EncodeToString(raw), "registered": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldPass, newPass := "the old recovery phrase", "the new recovery phrase"
	file, err := sealRecovery(rs, aid, srv.URL, identity.Commit{Seq: 2, Digest: next}, []byte(oldPass))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("ANET_HOME", home)
	prevHome, prevSettle, prevPoll := idHome, idSettle, idPoll
	idHome = func() (string, error) { return home, nil }
	idSettle, idPoll = 50*time.Millisecond, 10*time.Millisecond
	started := ""
	prevStart := idStartDaemon
	idStartDaemon = func(l daemon.Layout) error { started = l.Root; return nil }
	t.Cleanup(func() { idHome, idSettle, idPoll, idStartDaemon = prevHome, prevSettle, prevPoll, prevStart })
	src := filepath.Join(t.TempDir(), "saved.anetrec")
	if err := os.WriteFile(src, file, 0o600); err != nil {
		t.Fatal(err)
	}
	idTTY(t, oldPass, newPass, newPass)
	var out, errb bytes.Buffer
	x := &identityCLI{layout: daemon.Layout{Root: t.TempDir()}, pos: []string{src}, flags: map[string]string{"name": "back"},
		lang: a2ashape.English, out: &out, err: &errb}
	if err := x.recover(); err != nil {
		t.Fatalf("%v, said %q", err, errb.String())
	}
	layout := daemon.IdentityLayout("back")
	if started != layout.Root {
		t.Fatalf("daemon started in %q", started)
	}
	b, err := os.ReadFile(layout.IdentityPath())
	if err != nil {
		t.Fatal(err)
	}
	c, err := identity.Restore(b)
	if err != nil {
		t.Fatalf("the identity file does not restore: %v", err)
	}
	if _, ok := c.Pending(); !ok {
		t.Fatal("nothing pending")
	}
	f, err := identity.ParseIdentityFile(b)
	k, _ := identity.RecoverySeed(rs, aid, 2)
	if err != nil || f.Pending == nil || !f.Pending.Emergency || f.Pending.Kind != identity.PendingRot ||
		string(f.Pending.Revealed) != string(k.Seed()) {
		t.Fatalf("pending %+v (%v)", f.Pending, err)
	}
	cfg, err := daemon.LoadConfig(layout)
	if err != nil || cfg.HubURL != srv.URL {
		t.Fatalf("config %v %v", cfg.HubURL, err)
	}
	nb, err := os.ReadFile(defaultRecoveryFile(home, aid))
	if err != nil {
		t.Fatal(err)
	}
	rsNew, _, _, commits, err := identity.OpenRecoveryFile(nb, []byte(newPass))
	if err != nil || rsNew == rs || commits.Seq != 3 {
		t.Fatalf("new recovery file: %v %v", err, commits)
	}
	// The daemon never answered in this test: the wait ends with rotation_pending, exit 0.
	if !strings.Contains(out.String(), a2ashape.Sentence("rotation_pending", a2ashape.English, nil)) {
		t.Fatalf("output %q", out.String())
	}
	// A name that is taken is recover_target_exists.
	idTTY(t, oldPass)
	errb.Reset()
	if err := x.recover(); err != errExit1 || !strings.Contains(errb.String(), a2ashape.Sentence("recover_target_exists", a2ashape.English, a2ashape.Args{"name": "back"})) {
		t.Fatalf("%v %q", err, errb.String())
	}
}

// KEL L-3: the rotation that is still being published when the wait ends is not an error.
func TestRotationPendingExitsZero(t *testing.T) {
	f := newIDFix(t, true)
	f.d.publish = true
	idSettle = 50 * time.Millisecond
	idTTY(t, testPass)
	if err := f.cli(nil).rotate(); err != nil {
		t.Fatalf("%v, said %q", err, f.errb.String())
	}
	if !strings.Contains(f.out.String(), f.sentence("rotation_pending", nil)) {
		t.Fatalf("output %q", f.out.String())
	}
	// A new request while one is pending is refused, exit 1.
	idTTY(t, testPass)
	if err := f.cli(nil).rotate(); err != errExit1 {
		t.Fatalf("%v", err)
	}
}

// KEL L-7: status prints the five rows and the reminder of an identity without a passphrase.
func TestStatusOfAnUnprotectedIdentity(t *testing.T) {
	f := newIDFix(t, false)
	f.d.mu.Lock()
	f.d.st.Message = "coder …abcd, key confirmed."
	f.d.st.Recovery = daemonRecovery(f.sentence("identity.unprotected", nil), "anet identity protect")
	f.d.mu.Unlock()
	if err := f.cli(nil).statusCommand(); err != nil {
		t.Fatal(err)
	}
	got := f.out.String()
	for _, want := range []string{"Identity       coder ", "Recovery file  " + f.sentence("identity.status.recovery_unprotected", a2ashape.Args{"path": localRecPath(f.dir)}),
		f.sentence("identity.unprotected", nil) + "\n  → anet identity protect\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
	f.out.Reset()
	if err := f.cli(map[string]string{"json": "true"}).statusCommand(); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(f.out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	rf, _ := m["recovery_file"].(map[string]any)
	if rf["unprotected"] != true || rf["present"] != true || rf["mode_ok"] != true {
		t.Fatalf("recovery_file %v", rf)
	}
}
