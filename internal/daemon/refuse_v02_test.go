package daemon

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// TestRefuseV02DataDir (FS-S1, core CORE-V4; DLV-ST-10): a data directory
// an earlier anet wrote is refused at start with the one sentence of
// identity_v02 — this is anet 0.2's data directory, 0.3.0 does not migrate
// it, move it away and run anet up — and nothing in it changes: no file is
// written, created or touched. The check before the start
// (CheckDataDirForStart, what anet daemon and anet up print) and New say
// the same.
//
// RC-4 covers interactions.db (user_version 0, 1 and 2); FS-4 adds its
// enc_keys.cbor cases here.
func TestRefuseV02DataDir(t *testing.T) {
	for _, v := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("interactions.db user_version %d", v), func(t *testing.T) {
			layout := v02DataDir(t, func(l Layout) { writeV02Store(t, l, v) })
			refuseV02(t, layout)
		})
	}
}

// v02DataDir is a data directory with a config.json and an identity, and
// whatever old writes into it.
func v02DataDir(t *testing.T, old func(Layout)) Layout {
	t.Helper()
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0"})
	self, err := identity.InceptAgent(uint64(time.Now().Add(-time.Hour).UnixMilli()))
	if err != nil {
		t.Fatal(err)
	}
	layout := NewLayout(root)
	if err := saveIdentity(layout, self); err != nil {
		t.Fatal(err)
	}
	old(layout)
	return layout
}

// writeV02Store writes interactions.db as an earlier anet left it: its
// tables, in WAL mode, and user_version v (0 for v0.1 and v0.2, which set
// none).
func writeV02Store(t *testing.T, l Layout, v int) {
	t.Helper()
	if err := os.MkdirAll(l.InteractionsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(l.InteractionsDir(), "interactions.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE interaction (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, role TEXT NOT NULL,
		  peer_aid TEXT NOT NULL, goal BLOB NOT NULL, state TEXT NOT NULL DEFAULT 'submitted',
		  created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE refused (from_aid TEXT NOT NULL, mid BLOB NOT NULL, ts INTEGER NOT NULL, exp INTEGER NOT NULL,
		  PRIMARY KEY (from_aid, mid))`,
		`INSERT INTO interaction(id, role, peer_aid, goal, created_at, updated_at)
		  VALUES('ix_0123456789abcdef0123456789abcdef', 'inbound', 'did:anet:peer', 'an old task', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		fmt.Sprintf(`PRAGMA user_version = %d`, v),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// refuseV02 checks that layout is refused with identity_v02 alone, by the
// check before the start and by New, and that the directory is left as it
// was.
func refuseV02(t *testing.T, layout Layout) {
	t.Helper()
	before := dataDirState(t, layout.Root)
	time.Sleep(10 * time.Millisecond) // a write would show in a modification time
	assertV02Refusal(t, "CheckDataDirForStart", CheckDataDirForStart(layout))
	d, err := New(layout)
	if d != nil {
		d.Close()
	}
	assertV02Refusal(t, "New", err)
	if after := dataDirState(t, layout.Root); !reflect.DeepEqual(after, before) {
		t.Fatalf("the refused data directory changed:\n before %v\n after  %v", before, after)
	}
}

func assertV02Refusal(t *testing.T, who string, err error) {
	t.Helper()
	var cr *ConfigRefusal
	if !errors.As(err, &cr) || len(cr.Refusals) != 1 || cr.Refusals[0].Code() != "identity_v02" {
		t.Fatalf("%s: %v, want the one refusal identity_v02", who, err)
	}
	for _, lang := range []a2ashape.Language{a2ashape.English, a2ashape.Chinese} {
		want := a2ashape.Sentence("identity_v02", lang, nil)
		if got := cr.Sentences(lang); len(got) != 1 || got[0] != want {
			t.Errorf("%s (%v): %q, want %q", who, lang, got, want)
		}
	}
	if err.Error() != a2ashape.Sentence("identity_v02", a2ashape.Lang(), nil) {
		t.Errorf("%s: the error reads %q, not the sentence", who, err.Error())
	}
}

// dataDirState is every file under root: its size, content hash and
// modification time, by path.
func dataDirState(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if e.IsDir() {
			out[rel+"/"] = fi.Mode().String()
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = fmt.Sprintf("%s %d %x %d", fi.Mode(), len(b), sha256.Sum256(b), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
