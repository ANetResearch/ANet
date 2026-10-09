package daemon

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// endedTask records a finished task of a content class that ended at endedAt.
func endedTask(t *testing.T, d *Daemon, id, class string, endedAt uint64) {
	t.Helper()
	role, trust, cause := interactions.RoleInbound, class, interactions.CauseProviderEnd
	if class == interactions.ClassOutbound {
		role, trust, cause = interactions.RoleOutbound, "", interactions.CausePeerEnded
	}
	d.ix.SetClock(func() int64 { return int64(endedAt) })
	defer d.ix.SetClock(nil)
	if err := d.ix.Create(interactions.New{ID: id, Role: role, PeerAID: "did:anet:peer", Goal: "the goal of " + id,
		Trust: trust}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ix.SetState(id, interactions.StateCompleted, cause); err != nil {
		t.Fatal(err)
	}
}

func stored(t *testing.T, d *Daemon, id string) bool {
	t.Helper()
	_, err := d.ix.Get(id)
	if err != nil && !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// storeHits names the files of d's store that hold needle.
func storeHits(t *testing.T, d *Daemon, needle []byte) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(d.layout.InteractionsDir(), "interactions.db*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no store files: %v", err)
	}
	var hits []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, needle) {
			hits = append(hits, filepath.Base(f))
		}
	}
	return hits
}

// The setting: absent or null is 30 days, a fresh config.json says so, `anet init` writes it into a
// config without it, and a value that is neither a number of days nor forever stops the start with
// retention_invalid.
func TestContentRetentionConfig(t *testing.T) {
	if DefaultConfig().ContentRetention != "30d" {
		t.Fatalf("the default config says %q", DefaultConfig().ContentRetention)
	}
	if r := (Config{}).contentRetention(); r != interactions.DefaultRetention() {
		t.Fatalf("no setting is %q", r)
	}
	l := NewLayout(t.TempDir())
	if _, err := LoadConfig(l); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(l.ConfigPath())
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m["content_retention"] != "30d" {
		t.Fatalf("a fresh config.json: %v %v", m["content_retention"], err)
	}
	for _, tc := range []struct {
		value  string // JSON; "" for the key left out
		refuse bool
	}{
		{"", false}, {`null`, false}, {`"30d"`, false}, {`"1d"`, false}, {`"3650d"`, false}, {`"forever"`, false},
		{`""`, true}, {`"0d"`, true}, {`"3651d"`, true}, {`"30"`, true}, {`30`, true}, {`"30D"`, true},
		{`["30d"]`, true}, {`{"days":30}`, true}, {`true`, true}, {`"never"`, true}, {`" 30d"`, true},
	} {
		l := NewLayout(t.TempDir())
		body := `{"control_addr":"127.0.0.1:0"`
		if tc.value != "" {
			body += `,"content_retention":` + tc.value
		}
		if err := os.WriteFile(l.ConfigPath(), []byte(body+"}"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := CheckConfigForStart(l)
		var cr *ConfigRefusal
		refused := errors.As(err, &cr) && len(cr.Refusals) == 1 && cr.Refusals[0].Key == "retention_invalid"
		if refused != tc.refuse || (!tc.refuse && err != nil) {
			t.Errorf("content_retention %s: %v", tc.value, err)
		}
		if tc.refuse {
			if got := cr.Error(); got != a2ashape.Sentence("retention_invalid", a2ashape.Lang(), nil) {
				t.Errorf("content_retention %s: said %q", tc.value, got)
			}
		}
		r, text, ref, err := ContentRetentionInFile(l)
		if err != nil || (ref != nil) != tc.refuse {
			t.Errorf("ContentRetentionInFile with %s: %v %v %v", tc.value, r, ref, err)
		}
		if !tc.refuse && tc.value != "" && tc.value != "null" && strconv.Quote(text) != tc.value {
			t.Errorf("ContentRetentionInFile with %s read %q", tc.value, text)
		}
	}
	// init adds the key to a config without it, and leaves one that has it.
	l = NewLayout(t.TempDir())
	if err := os.WriteFile(l.ConfigPath(), []byte(`{"control_addr":"127.0.0.1:0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := InitLayout(l)
	if err != nil {
		t.Fatal(err)
	}
	added := false
	for _, c := range rep.Changes {
		added = added || (c.Key == "content_retention" && c.Action == "added" && c.Value == "30d")
	}
	if r, _, ref, _ := ContentRetentionInFile(l); !added || ref != nil || r != interactions.DefaultRetention() {
		t.Fatalf("init: added %v, now %q", added, r)
	}
	if err := SetContentRetentionInFile(l, interactions.RetentionForever); err != nil {
		t.Fatal(err)
	}
	if rep, _ := InitLayout(l); rep.Wrote {
		t.Fatalf("init rewrote a config that has every key: %+v", rep.Changes)
	}
	if r, _, _, _ := ContentRetentionInFile(l); r != interactions.RetentionForever {
		t.Fatalf("init reset the setting to %q", r)
	}
}

// SetContentRetentionInFile changes that key alone and keeps the rest of the file as it is, keys
// this version does not know included; the file stays 0600.
func TestSetContentRetentionInFileKeepsTheRest(t *testing.T) {
	l := NewLayout(t.TempDir())
	if err := os.WriteFile(l.ConfigPath(), []byte(`{"control_addr":"127.0.0.1:0","content_retention":30,"name":"n",`+
		`"a_later_key":{"x":[1,2.5]},"inbound":{"policy":"closed"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetContentRetentionInFile(l, mustRet(t, "7d")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(l.ConfigPath())
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["content_retention"] != "7d" || m["name"] != "n" || fmt.Sprint(m["a_later_key"]) != "map[x:[1 2.5]]" ||
		fmt.Sprint(m["inbound"]) != "map[policy:closed]" {
		t.Fatalf("config after the change: %s", raw)
	}
	if fi, _ := os.Stat(l.ConfigPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if err := CheckConfigForStart(l); err != nil {
		t.Fatalf("the repaired config does not start: %v", err)
	}
	if err := SetContentRetentionInFile(l, interactions.Retention{}); err == nil {
		t.Fatal("no setting was written")
	}
	// A data directory without a config gets one, with the value.
	fresh := NewLayout(t.TempDir())
	if err := SetContentRetentionInFile(fresh, mustRet(t, "90d")); err != nil {
		t.Fatal(err)
	}
	if r, _, ref, err := ContentRetentionInFile(fresh); err != nil || ref != nil || r != mustRet(t, "90d") {
		t.Fatalf("a fresh directory: %q %v %v", r, ref, err)
	}
}

func mustRet(t testing.TB, s string) interactions.Retention {
	t.Helper()
	r, ok := interactions.ParseRetention(s)
	if !ok {
		t.Fatalf("%q", s)
	}
	return r
}

// Every class the sweep deletes is recorded, one anet.interaction.pruned each, in the registered
// payload with the class's cutoff and retention, and nothing else is written.
func TestRetentionSweepRecordsEveryClass(t *testing.T) {
	d := newBareDaemon(t)
	now := time.Date(2026, 9, 27, 15, 0, 0, 0, time.UTC).UnixMilli()
	for _, c := range interactions.ContentClasses() {
		endedTask(t, d, "ix_"+c, c, uint64(now-40*dayMS))
	}
	endedTask(t, d, "ix_kept", interactions.ClassPeer, uint64(now-2*dayMS))
	if n := d.pruneRetentionAt(now, clockFull); n != 5 {
		t.Fatalf("deleted %d", n)
	}
	evs := ledgerPayloads(t, d, EvInteractionPruned)
	if len(evs) != 5 {
		t.Fatalf("%d records: %v", len(evs), evs)
	}
	shape, _ := evtypes.PayloadOf(EvInteractionPruned)
	seen := map[string]bool{}
	for _, ev := range evs {
		if why := shape.Check(ev); why != "" {
			t.Errorf("%v: %s", ev, why)
		}
		class, _ := ev["trust"].(string)
		seen[class] = true
		cutoff, _ := interactions.RetentionCutoff(now, class, d.ContentRetention())
		days := 30
		if interactions.PublicClass(class) {
			days = 7
		}
		if fmt.Sprint(ev["before"]) != fmt.Sprint(cutoff) || fmt.Sprint(ev["retention_days"]) != strconv.Itoa(days) ||
			fmt.Sprint(ev["interactions"]) != "1" {
			t.Errorf("%s: %v", class, ev)
		}
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), "ix_") || strings.Contains(string(raw), "goal") {
			t.Errorf("a record names a task or its content: %s", raw)
		}
	}
	if len(seen) != 5 || !stored(t, d, "ix_kept") {
		t.Fatalf("classes %v; the recent task stored %v", seen, stored(t, d, "ix_kept"))
	}
}

// TestCheckpointAfterKeyDelete (FS-L8, mutation 16's checkpoint half): a key deleted from the store —
// a provider's interaction key at the task's end, a held item's data key at its expiry — stays in the
// write-ahead log until it is truncated; the next maintenance period truncates it, and the key's bytes
// are then in no file of the store.
func TestCheckpointAfterKeyDelete(t *testing.T) {
	d := newTestDaemon(t, "", true)
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k := interactions.IXKey{IX: "ix_p", PeerAID: "did:anet:peer", Suite: seal.SuiteX25519, Pub: priv.PublicKey().Bytes(), Priv: priv.Bytes()}
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		if err := tx.Create(interactions.New{ID: "ix_p", Role: interactions.RoleInbound, PeerAID: k.PeerAID, Goal: "g",
			Trust: interactions.TrustPeer}); err != nil {
			return err
		}
		if err := tx.PutIXKey(k); err != nil {
			return err
		}
		return tx.PutPending(interactions.PendingItem{IX: "ix_held", FromAID: "did:anet:peer", ArrivedAt: 1,
			Delegate: []byte("D")}, 0, 0)
	}); err != nil {
		t.Fatal(err)
	}
	dekBytes := rawDEK(t, d, "ix_held")
	if _, err := d.ix.SetState("ix_p", interactions.StateCompleted, interactions.CauseProviderEnd); err != nil {
		t.Fatal(err)
	}
	d.expirePendingAt(int64(d.nowMS()))
	if _, err := d.ix.DEK("ix_held"); !errors.Is(err, interactions.ErrContentGone) {
		t.Fatalf("the held item's key: %v", err)
	}
	for name, b := range map[string][]byte{"interaction key": priv.Bytes(), "data key": dekBytes} {
		if hits := storeHits(t, d, b); len(hits) == 0 {
			t.Fatalf("the deleted %s is in no file before the period: the test would prove nothing", name)
		}
	}
	d.keyMaintenance()
	for name, b := range map[string][]byte{"interaction key": priv.Bytes(), "data key": dekBytes} {
		if hits := storeHits(t, d, b); len(hits) != 0 {
			t.Errorf("a period after its deletion the %s is still in %v", name, hits)
		}
	}
	if fi, err := os.Stat(filepath.Join(d.layout.InteractionsDir(), "interactions.db-wal")); err == nil && fi.Size() != 0 {
		t.Errorf("the log is %d bytes after the period", fi.Size())
	}
}

// rawDEK reads task ix's data key as the store holds it, from another connection.
func rawDEK(t *testing.T, d *Daemon, ix string) []byte {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(d.layout.InteractionsDir(), "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var k []byte
	if err := db.QueryRow(`SELECT key FROM ix_dek WHERE ix=?`, ix).Scan(&k); err != nil || len(k) != interactions.DEKSize {
		t.Fatalf("the data key of %s: %d bytes, %v", ix, len(k), err)
	}
	return k
}

// A log that stays busy is counted, once every walBusyReport periods in a row (wal-checkpoint-busy),
// and a period that truncates it counts nothing and leaves it empty. (The daemon's own start sweep may
// run a checkpoint of its own meanwhile, so the busy periods are counted up to the first report.)
func TestCheckpointBusyIsCounted(t *testing.T) {
	for n, want := range map[int]bool{0: false, 1: false, 11: false, 12: true, 13: false, 24: true} {
		if walBusyDue(n, walBusyReport) != want {
			t.Errorf("busy %d periods in a row: counted %v", n, !want)
		}
	}
	d := newTestDaemon(t, "", true)
	endedTask(t, d, "ix_a", interactions.ClassPeer, d.nowMS())
	reader := holdSnapshot(t, d)
	endedTask(t, d, "ix_b", interactions.ClassPeer, d.nowMS())
	for i := 0; i < 2*walBusyReport && d.ReceiveStats()[counterWALBusy] == 0; i++ {
		d.checkpointStore()
	}
	counted := d.ReceiveStats()[counterWALBusy]
	if counted == 0 {
		t.Fatalf("a log busy for %d periods was not counted", 2*walBusyReport)
	}
	reader()
	d.checkpointStore()
	if n := d.ReceiveStats()[counterWALBusy]; n != counted {
		t.Fatalf("a period that truncated the log counted it: %d, was %d", n, counted)
	}
	if fi, err := os.Stat(filepath.Join(d.layout.InteractionsDir(), "interactions.db-wal")); err == nil && fi.Size() != 0 {
		t.Fatalf("the log is %d bytes once the reader is gone", fi.Size())
	}
}

// holdSnapshot opens a read transaction on d's store from another connection, as a reader that keeps
// an older snapshot, and returns the function that ends it.
func holdSnapshot(t *testing.T, d *Daemon) func() {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(d.layout.InteractionsDir(), "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM interaction`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	done := false
	end := func() {
		if !done {
			done = true
			_ = tx.Rollback()
			_ = db.Close()
		}
	}
	t.Cleanup(end)
	return end
}

// TestPendingExpiryDeletesDEK (FS-L9): a held item that expires goes with its data key, in the same
// transaction, whatever the clock gate says; one whose key is already gone expires too.
func TestPendingExpiryDeletesDEK(t *testing.T) {
	d := newTestDaemon(t, silentHub, true) // the hub never answers: the gate is closed
	if level := d.clockLevelNow(); level != clockClosed {
		t.Fatalf("level %v, want closed", level)
	}
	now := int64(d.nowMS())
	ttl := int64(d.config().inbound().Pending.TTLHours) * 3600 * 1000
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		for _, ix := range []string{"ix_old", "ix_keyless", "ix_new"} {
			at := now - ttl - 1
			if ix == "ix_new" {
				at = now
			}
			if err := tx.PutPending(interactions.PendingItem{IX: ix, FromAID: "did:anet:peer", ArrivedAt: at, Delegate: []byte("D")}, 0, 0); err != nil {
				return err
			}
		}
		_, err := tx.DeleteDEK("ix_keyless")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	d.expirePendingAt(now)
	for _, ix := range []string{"ix_old", "ix_keyless"} {
		if _, err := d.ix.GetPending(ix); !errors.Is(err, interactions.ErrNotFound) {
			t.Errorf("%s is still held: %v", ix, err)
		}
		if _, err := d.ix.DEK(ix); !errors.Is(err, interactions.ErrContentGone) {
			t.Errorf("%s's data key outlived it: %v", ix, err)
		}
	}
	if _, err := d.ix.GetPending("ix_new"); err != nil {
		t.Fatalf("an item within its time expired: %v", err)
	}
	if _, err := d.ix.DEK("ix_new"); err != nil {
		t.Fatalf("an item within its time lost its key: %v", err)
	}
}

// TestWorkDirsPrunedWithinS (FS-L11): a maintenance period removes the exec work directories of tasks
// that have ended — under both roots, read-only leftovers and prompt files included — and of ids no
// task has, with their agent session bindings, whether or not auto-reply is configured; an open task's
// directory, the self-check's and the operator's own work_dir stay. A task that ends has its
// directory gone by the next period.
func TestWorkDirsPrunedWithinS(t *testing.T) {
	d := newTestDaemon(t, "", true)
	now := d.nowMS()
	if err := d.ix.Create(interactions.New{ID: "ix_open", Role: interactions.RoleInbound, PeerAID: "did:anet:peer", Goal: "g",
		Trust: interactions.TrustPeer}); err != nil {
		t.Fatal(err)
	}
	endedTask(t, d, "ix_done", interactions.ClassPeer, now)
	dirOf := func(root, ix string) string { return filepath.Join(root, aidShort(d.AID()), ixDirName(ix)) }
	roots := []string{execWorkRoot(), execSandboxWorkRoot()}
	for _, root := range roots {
		for _, ix := range []string{"ix_open", "ix_done", "ix_never_a_task", ""} {
			dir := dirOf(root, ix)
			if err := os.MkdirAll(filepath.Join(dir, "ro", "deep"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".anet-prompt"), []byte("the peer's words"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(dir, "ro"), 0o500); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() {
		for _, root := range roots {
			removeWorkTree(filepath.Join(root, aidShort(d.AID())))
		}
	})
	own := filepath.Join(t.TempDir(), "operator-work")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "notes"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sess := sharedExecSessionStore(d.layout.Root)
	sess.set("ix_done", "s-done")
	sess.set("ix_open", "s-open")
	exists := func(p string) bool { _, err := os.Lstat(p); return err == nil }

	d.keyMaintenance()
	for _, root := range roots {
		for ix, want := range map[string]bool{"ix_open": true, "ix_done": false, "ix_never_a_task": false, "": true} {
			if got := exists(dirOf(root, ix)); got != want {
				t.Errorf("%s under %s: present %v, want %v", ix, root, got, want)
			}
		}
	}
	if !exists(filepath.Join(own, "notes")) {
		t.Error("the operator's own work dir was touched")
	}
	if sess.get("ix_done") != "" || sess.get("ix_open") != "s-open" {
		t.Errorf("session bindings: done %q, open %q", sess.get("ix_done"), sess.get("ix_open"))
	}
	if _, err := d.ix.SetState("ix_open", interactions.StateCompleted, interactions.CauseProviderEnd); err != nil {
		t.Fatal(err)
	}
	d.keyMaintenance()
	for _, root := range roots {
		if exists(dirOf(root, "ix_open")) {
			t.Errorf("a period after its task ended the work dir under %s stays", root)
		}
	}
	if sess.get("ix_open") != "" {
		t.Error("the ended task's session binding stays")
	}
}

// A directory made for a task that starts while the period runs is not taken for an ended one's: the
// directories are listed before the open tasks are read. Here the task's directory appears while the
// store is asked, and the answer, read from a snapshot taken just before the task was recorded, does
// not name it yet.
func TestWorkDirPruneListsBeforeItAsks(t *testing.T) {
	aid := "bafyreiexampleexampleexampleexample"
	root := execWorkRoot()
	late := filepath.Join(root, aidShort(aid), ixDirName("ix_late"))
	t.Cleanup(func() { removeWorkTree(filepath.Join(root, aidShort(aid))) })
	if err := os.MkdirAll(filepath.Join(root, aidShort(aid), ixDirName("ix_gone")), 0o700); err != nil {
		t.Fatal(err)
	}
	err := pruneExecWorkDirs(aid, func() (map[string]bool, error) {
		if err := os.MkdirAll(late, 0o700); err != nil {
			t.Fatal(err)
		}
		return map[string]bool{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(late); err != nil {
		t.Fatalf("the new task's dir went: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, aidShort(aid), ixDirName("ix_gone"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the ended task's dir stays: %v", err)
	}
}

// stallContentWorld is the close_after and retention half of TestHardBoundDeletesDespiteHubStall: a
// node whose hub never confirms its clock (Date six minutes off, or no answer) deletes an interaction
// key past close_after and tasks past their retention no earlier than they are due, and no later than
// a day of running time and a period after (FS-K14a; SI-27).
func stallContentWorld(t *testing.T, skewed bool) {
	d := newTestDaemon(t, silentHub, true)
	start := epochOf(d.nowMS()) + 30*60*1000
	clocks := newGateClocks(t, d, start)
	if _, err := d.SetContentRetention(mustRet(t, "2d")); err != nil {
		t.Fatal(err)
	}
	const step = hourMS
	type item struct {
		due     uint64
		present func() bool
	}
	items := map[string]*item{}
	kid := keyedOutboundTask(t, d, "ix_closing", start-14*dayMS-12*hourMS) // close_after = start + 12 hours
	items["close_after"] = &item{due: start + 12*hourMS, present: func() bool {
		k, err := d.ix.IXKeyByKID(kid)
		if err != nil {
			t.Fatal(err)
		}
		return k != nil
	}}
	task := func(name, class string, endedAt uint64) {
		endedTask(t, d, name, class, endedAt)
		due, _ := interactions.ContentExpiresAt(int64(endedAt), class, d.ContentRetention())
		items[name] = &item{due: uint64(due), present: func() bool { return stored(t, d, name) }}
	}
	task("private_due_at_start", interactions.ClassPeer, start-3*dayMS)
	task("private_due_later", interactions.ClassApproved, start+3*dayMS)
	task("public_due_at_start", interactions.ClassPublicCap, start-3*dayMS)
	gone := map[string]uint64{}
	for clocks.wall.Load() < start+12*dayMS {
		if skewed {
			hubSays(d, clocks.wall.Load()-6*minuteMS)
		}
		d.keyMaintenance()
		now := clocks.wall.Load()
		for name, it := range items {
			if _, done := gone[name]; done {
				continue
			}
			if !it.present() {
				gone[name] = now
				if now < it.due {
					t.Fatalf("%s went %d ms before it was due", name, it.due-now)
				}
				continue
			}
			if now > max(it.due, start)+clockFallbackAfter+step {
				t.Fatalf("%s is still there %d ms after it was due (skewed %v)", name, now-it.due, skewed)
			}
		}
		clocks.run(step)
	}
	if len(gone) != len(items) {
		t.Fatalf("only %v went", gone)
	}
}

// TestRetentionRoute (FS §2.10): a dry run says how many finished tasks the value deletes at once and
// changes nothing; the change writes config.json, sweeps, records anet.retention.changed and answers
// with retention.set; the same value again changes nothing; /status.privacy follows; a body that is not
// exactly the request, or a value that is not a setting, is retention_invalid.
func TestRetentionRoute(t *testing.T) {
	p := newPlane(t)
	d := p.d
	now := d.nowMS()
	endedTask(t, d, "ix_40", interactions.ClassPeer, now-40*dayMS)
	endedTask(t, d, "ix_10", interactions.ClassOutbound, now-10*dayMS)
	endedTask(t, d, "ix_pub", interactions.ClassPublicCap, now-5*dayMS)
	post := func(body string) (int, map[string]any) {
		t.Helper()
		resp, b := p.req(t, "POST", "/retention", body, p.bearer)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}
	if code, m := post(`{"value":"7d","dry_run":true}`); code != 200 || m["value"] != "7d" || fmt.Sprint(m["would_delete"]) != "1" {
		t.Fatalf("dry run: %d %v", code, m)
	}
	if r, _, _, _ := ContentRetentionInFile(d.layout); r != interactions.DefaultRetention() || !stored(t, d, "ix_10") {
		t.Fatal("a dry run changed something")
	}
	code, m := post(`{"value":"7d"}`)
	if code != 200 || m["value"] != "7d" || fmt.Sprint(m["deleted"]) != "2" ||
		m["message"] != a2ashape.Sentence("retention.set", a2ashape.Lang(), a2ashape.Args{"retention": "7d"}) {
		t.Fatalf("set: %d %v", code, m)
	}
	if r, _, _, _ := ContentRetentionInFile(d.layout); r != mustRet(t, "7d") || d.ContentRetention() != mustRet(t, "7d") {
		t.Fatalf("after the change config.json says %q, the daemon %q", r, d.ContentRetention())
	}
	if stored(t, d, "ix_40") || stored(t, d, "ix_10") || !stored(t, d, "ix_pub") {
		t.Fatal("the sweep after the change did not delete what was due, or deleted the public call")
	}
	evs := ledgerPayloads(t, d, EvRetentionChanged)
	shape, _ := evtypes.PayloadOf(EvRetentionChanged)
	if len(evs) != 1 || evs[0]["from"] != "30d" || evs[0]["to"] != "7d" || fmt.Sprint(evs[0]["deleted_interactions"]) != "2" ||
		shape.Check(evs[0]) != "" {
		t.Fatalf("change evidence %v", evs)
	}
	if code, m := post(`{"value":"7d"}`); code != 200 || fmt.Sprint(m["deleted"]) != "0" || chainEvents(t, d, EvRetentionChanged) != 1 {
		t.Fatalf("the same value again: %d %v", code, m)
	}
	status := func() map[string]any {
		_, b := p.req(t, "GET", "/status", "", p.bearer)
		var st struct {
			Privacy map[string]any `json:"privacy"`
		}
		_ = json.Unmarshal(b, &st)
		return st.Privacy
	}
	if pv := status(); pv["content_retention"] != "7d" || pv["public_retention"] != "7d" {
		t.Fatalf("privacy %v", pv)
	}
	if code, _ := post(`{"value":"3d"}`); code != 200 {
		t.Fatal(code)
	}
	if pv := status(); pv["content_retention"] != "3d" || pv["public_retention"] != "3d" {
		t.Fatalf("privacy %v", pv)
	}
	if code, _ := post(`{"value":"forever"}`); code != 200 {
		t.Fatal(code)
	}
	if pv := status(); pv["content_retention"] != "forever" || pv["public_retention"] != "7d" {
		t.Fatalf("privacy %v", pv)
	}
	want := a2ashape.Refuse("retention_invalid", nil).Body(a2ashape.Lang())
	for _, body := range []string{`{"value":"0d"}`, `{"value":"30"}`, `{"value":30}`, `{"value":"7d","extra":1}`,
		`{"Value":"7d"}`, `{"value":"7d","value":"8d"}`, `{"value":"7d","dry_run":"yes"}`, `{"dry_run":true}`, `{}`,
		`[]`, `{"value":"7d"} {}`, `{"value":null}`, `{"value":["7d"]}`, ``, `{"value":"` + strings.Repeat("9", 5000) + `d"}`} {
		code, m := post(body)
		if code != 400 || m["reason"] != "retention_invalid" || m["error"] != want.Error || m["next"] != "anet retention set 30d" {
			t.Errorf("%.40q: %d %v", body, code, m)
		}
	}
	if d.ContentRetention() != interactions.RetentionForever {
		t.Fatal("a refused request changed the setting")
	}
}

// At the closed clock gate a change stands but deletes nothing until the gate opens (FS §5.2 step 5).
func TestRetentionChangeAtClosedGate(t *testing.T) {
	d := newTestDaemon(t, silentHub, true)
	endedTask(t, d, "ix_old", interactions.ClassPeer, d.nowMS()-10*dayMS)
	n, err := d.SetContentRetention(mustRet(t, "1d"))
	if err != nil || n != 0 {
		t.Fatalf("set at closed: %d %v", n, err)
	}
	if d.ContentRetention() != mustRet(t, "1d") || !stored(t, d, "ix_old") {
		t.Fatal("at the closed gate the value did not stand, or a task went")
	}
	if evs := ledgerPayloads(t, d, EvRetentionChanged); len(evs) != 1 || fmt.Sprint(evs[0]["deleted_interactions"]) != "0" {
		t.Fatalf("change evidence %v", evs)
	}
	hubSays(d, d.nowMS()) // the hub confirms the clock: the next period sweeps
	d.keyMaintenance()
	if stored(t, d, "ix_old") {
		t.Fatal("the task stays once the gate opened")
	}
}

// The sweep a change runs between two periods reads the gate as a period starting then would (FS-K14a,
// P-6 item 4): after a day of running time with the hub silent the last period ran at local, but a wall
// clock that has jumped since closes the gate, and the change deletes nothing until a day of running
// time has passed again.
func TestRetentionChangeAfterWallJump(t *testing.T) {
	d := newTestDaemon(t, silentHub, true)
	clocks := newGateClocks(t, d, d.nowMS())
	d.forgetCycle()
	d.enc.mu.Lock()
	d.enc.run = clockFallbackAfter
	d.enc.mu.Unlock()
	d.keyMaintenance()
	if level := d.clockLevelNow(); level != clockLocal {
		t.Fatalf("after a day of running time with the hub silent the level is %v, want local", level)
	}
	endedTask(t, d, "ix_recent", interactions.ClassPeer, clocks.wall.Load()-2*dayMS)
	clocks.wall.Add(20 * dayMS) // the wall clock jumps; the boot clock does not
	if level := d.clockLevelNow(); level != clockClosed {
		t.Fatalf("after the wall clock jumped the level is %v, want closed", level)
	}
	if n, err := d.SetContentRetention(mustRet(t, "7d")); err != nil || n != 0 || !stored(t, d, "ix_recent") {
		t.Fatalf("a change after the wall clock jumped deleted %d (%v); the task stored %v", n, err, stored(t, d, "ix_recent"))
	}
}

// TestRetentionRouteNotInMCPOrConsole (FS-U3, UX-M2): changing the retention is the CLI's alone. The
// route is bearer only — not on the console session allowlist, and a session is refused it — and no
// MCP tool or route and nothing of the local A2A interface names it.
func TestRetentionRouteNotInMCPOrConsole(t *testing.T) {
	p := newPlane(t)
	registered := false
	for _, pat := range p.cp.api.patterns {
		registered = registered || pat == "POST /retention"
	}
	if !registered {
		t.Fatal("POST /retention is not a route")
	}
	if _, ok := sessionRoutes["POST /retention"]; ok {
		t.Fatal("POST /retention is on the console session allowlist")
	}
	if resp, b := p.req(t, "POST", "/retention", `{"value":"1d"}`, p.session(t).as(p)); !refusedForSession(resp, b) {
		t.Fatalf("a console session reached /retention: %d %s", resp.StatusCode, b)
	}
	if p.d.ContentRetention() != interactions.DefaultRetention() {
		t.Fatal("a console session changed the retention")
	}
	toolName := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for _, dir := range []string{"../mcpserv", "../../module/a2a"} {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
		if err != nil || len(pkgs) == 0 {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			ast.Inspect(pkg, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				if strings.Contains(s, "/retention") || (toolName.MatchString(s) && strings.Contains(s, "retention")) {
					t.Errorf("%s names %q", fset.Position(lit.Pos()), s)
				}
				return true
			})
		}
	}
}

// FuzzRetentionRequest: the strict decoder never panics, and what it takes is exactly one object with
// a "value" string and at most a "dry_run" boolean.
func FuzzRetentionRequest(f *testing.F) {
	for _, s := range []string{`{"value":"7d"}`, `{"value":"forever","dry_run":true}`, `{"dry_run":false,"value":"1d"}`,
		`{"value":"7d","value":"8d"}`, `{"Value":"7d"}`, `{"value":7}`, `{}`, `[]`, `{"value":"7d"} x`, ``, `{"value":"7d"}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		v, dry, err := decodeRetentionRequest(b)
		if err != nil {
			return
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%q taken but not one JSON object: %v", b, err)
		}
		for k := range m {
			if k != "value" && k != "dry_run" {
				t.Fatalf("%q taken with member %q", b, k)
			}
		}
		var got string
		if json.Unmarshal(m["value"], &got) != nil || got != v {
			t.Fatalf("%q: value %q, decoded %q", b, v, got)
		}
		if raw, ok := m["dry_run"]; ok {
			var f bool
			if json.Unmarshal(raw, &f) != nil || f != dry {
				t.Fatalf("%q: dry_run %v", b, dry)
			}
		} else if dry {
			t.Fatalf("%q: dry_run without the member", b)
		}
	})
}
