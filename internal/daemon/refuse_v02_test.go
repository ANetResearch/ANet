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

	"github.com/ANetResearch/ANetCore/coredet"
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
// RC-4 covers interactions.db (user_version 0, 1 and 2); FS-4 adds the
// enc_keys.cbor cases: the key ring 0.2 wrote (v 1), and one with no v.
func TestRefuseV02DataDir(t *testing.T) {
	for _, v := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("interactions.db user_version %d", v), func(t *testing.T) {
			layout := v02DataDir(t, true, func(l Layout) { writeV02Store(t, l, v) })
			refuseV02(t, layout)
		})
	}
	// Without an identity file this version reads, the start would make
	// one: the refusal comes first, and the directory gets no new identity.
	t.Run("interactions.db user_version 0, no identity file", func(t *testing.T) {
		layout := v02DataDir(t, false, func(l Layout) { writeV02Store(t, l, 0) })
		refuseV02(t, layout)
	})
	for name, ring := range map[string]map[uint64]any{
		"enc_keys.cbor v 1":    v02KeyRing(1),
		"enc_keys.cbor v 0":    v02KeyRing(0),
		"enc_keys.cbor no v":   v02KeyRing(-1),
		"enc_keys.cbor v 1, a": {1: uint64(1), 2: "did:anet:someone", 3: uint64(1)},
	} {
		t.Run(name, func(t *testing.T) {
			layout := v02DataDir(t, true, func(l Layout) { writeRingFile(t, l, ring) })
			refuseV02(t, layout)
		})
	}
	t.Run("enc_keys.cbor v 1, no identity file", func(t *testing.T) {
		layout := v02DataDir(t, false, func(l Layout) { writeRingFile(t, l, v02KeyRing(1)) })
		refuseV02(t, layout)
	})
}

// v02KeyRing is enc_keys.cbor as anet 0.2 wrote it — {1: v, 2: aid, 3:
// last_seq, 4: key pairs, 5: signed set} — with v (absent when v < 0).
func v02KeyRing(v int) map[uint64]any {
	m := map[uint64]any{2: "did:anet:old", 3: uint64(1_700_000_000_000),
		4: []any{map[uint64]any{1: map[uint64]any{1: make([]byte, 16), 2: uint64(1), 3: make([]byte, 32),
			4: uint64(1_700_000_000_000), 5: uint64(1_700_604_800_000)}, 2: make([]byte, 32)}},
		5: []byte{0xa0}}
	if v >= 0 {
		m[1] = uint64(v)
	}
	return m
}

// writeRingFile writes ring as enc_keys.cbor.
func writeRingFile(t *testing.T, l Layout, ring map[uint64]any) {
	t.Helper()
	b, err := coredet.Marshal(ring)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.EncKeysPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRefuseNewerDataDir (core CORE-V4, revision 5): a data directory a
// newer anet wrote — interactions.db with a user_version above 3, a key
// ring with a v above 2, an identity file whose type tag names a version
// above 2 — is refused at start with datadir_too_new, whose next step is to
// upgrade and keep the directory, never with identity_v02, whose next step
// is to move it away. Nothing in it changes. When one part is newer and
// another older, the newer wins: the directory holds data this version
// cannot judge.
func TestRefuseNewerDataDir(t *testing.T) {
	t.Run("interactions.db user_version 4", func(t *testing.T) {
		layout := v02DataDir(t, true, func(l Layout) { writeV02Store(t, l, 4) })
		refuseDataDir(t, layout, "datadir_too_new")
	})
	t.Run("enc_keys.cbor v 3", func(t *testing.T) {
		layout := v02DataDir(t, true, func(l Layout) {
			ring := v02KeyRing(3)
			ring[10] = "a member this version does not know"
			writeRingFile(t, l, ring)
		})
		refuseDataDir(t, layout, "datadir_too_new")
	})
	t.Run("identity file anet.identity/3", func(t *testing.T) {
		layout := v02DataDir(t, false, func(l Layout) { writeTaggedIdentity(t, l, 3) })
		refuseDataDir(t, layout, "datadir_too_new")
		_, err := LoadOrGenerateIdentity(layout)
		assertDataDirRefusal(t, "LoadOrGenerateIdentity", err, "datadir_too_new")
		if _, err := ReadIdentityStatus(layout, a2ashape.Lang()); !isRefusal(err, "datadir_too_new") {
			t.Fatalf("ReadIdentityStatus: %v, want datadir_too_new", err)
		}
	})
	t.Run("a newer key ring beside a 0.2 store", func(t *testing.T) {
		layout := v02DataDir(t, true, func(l Layout) {
			writeV02Store(t, l, 0)
			writeRingFile(t, l, v02KeyRing(3))
		})
		refuseDataDir(t, layout, "datadir_too_new")
	})
	// An older tagged identity file is 0.2's case, and an identity file
	// whose tag is not of that form is damaged, which neither says.
	t.Run("identity file anet.identity/1", func(t *testing.T) {
		layout := v02DataDir(t, false, func(l Layout) { writeTaggedIdentity(t, l, 1) })
		refuseDataDir(t, layout, "identity_v02")
	})
	for name, tag := range map[string]string{"no number": identityFileTagPrefix, "leading zero": identityFileTagPrefix + "03",
		"sign": identityFileTagPrefix + "+3", "other prefix": "anet-identity/3"} {
		t.Run("identity file tag "+name, func(t *testing.T) {
			layout := v02DataDir(t, false, func(l Layout) { writeIdentityTag(t, l, tag) })
			if err := CheckDataDirForStart(layout); err != nil {
				t.Fatalf("a damaged identity file was refused as old or new: %v", err)
			}
		})
	}
	if identity.IdentityFileType != identityFileTagPrefix+fmt.Sprint(identityFileVersion) {
		t.Fatalf("this version's identity tag is %q, the check takes %q%d", identity.IdentityFileType, identityFileTagPrefix, identityFileVersion)
	}
}

// writeTaggedIdentity writes an identity file whose type tag names version n.
func writeTaggedIdentity(t *testing.T, l Layout, n int) {
	t.Helper()
	writeIdentityTag(t, l, fmt.Sprintf("%s%d", identityFileTagPrefix, n))
}

// writeIdentityTag writes an identity file of this version's shape with the
// type tag tag.
func writeIdentityTag(t *testing.T, l Layout, tag string) {
	t.Helper()
	b, err := coredet.Marshal(map[uint64]any{0: tag, 1: []byte{0x80}, 6: "ok", 20: "a member a newer version added"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.IdentityPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// v02DataDir is a data directory with a config.json, an identity when
// withIdentity, and whatever old writes into it.
func v02DataDir(t *testing.T, withIdentity bool, old func(Layout)) Layout {
	t.Helper()
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0"})
	layout := NewLayout(root)
	if withIdentity {
		self, err := identity.InceptAgent(uint64(time.Now().Add(-time.Hour).UnixMilli()))
		if err != nil {
			t.Fatal(err)
		}
		if err := saveIdentity(layout, self); err != nil {
			t.Fatal(err)
		}
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
	refuseDataDir(t, layout, "identity_v02")
}

// refuseDataDir checks that layout is refused with code alone, by the check
// before the start and by New, and that the directory is left as it was.
func refuseDataDir(t *testing.T, layout Layout, code string) {
	t.Helper()
	before := dataDirState(t, layout.Root)
	time.Sleep(10 * time.Millisecond) // a write would show in a modification time
	assertDataDirRefusal(t, "CheckDataDirForStart", CheckDataDirForStart(layout), code)
	d, err := New(layout)
	if d != nil {
		d.Close()
	}
	assertDataDirRefusal(t, "New", err, code)
	if after := dataDirState(t, layout.Root); !reflect.DeepEqual(after, before) {
		t.Fatalf("the refused data directory changed:\n before %v\n after  %v", before, after)
	}
}

func assertV02Refusal(t *testing.T, who string, err error) {
	t.Helper()
	assertDataDirRefusal(t, who, err, "identity_v02")
}

// assertDataDirRefusal checks that err is the one refusal code, saying its
// sentence in both languages.
func assertDataDirRefusal(t *testing.T, who string, err error, code string) {
	t.Helper()
	var cr *ConfigRefusal
	if !errors.As(err, &cr) || len(cr.Refusals) != 1 || cr.Refusals[0].Code() != code {
		t.Fatalf("%s: %v, want the one refusal %s", who, err, code)
	}
	for _, lang := range []a2ashape.Language{a2ashape.English, a2ashape.Chinese} {
		want := a2ashape.Sentence(code, lang, nil)
		if got := cr.Sentences(lang); len(got) != 1 || got[0] != want {
			t.Errorf("%s (%v): %q, want %q", who, lang, got, want)
		}
	}
	if err.Error() != a2ashape.Sentence(code, a2ashape.Lang(), nil) {
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

// An earlier anet's store whose schema is still only in its write-ahead
// log (that anet stopped without closing it) passes the check before the
// start, which reads the file alone. New finds it when it opens the store
// and refuses it the same way, with identity_v02, and converts nothing.
func TestRefuseV02DataDirFoundOnlyInTheLog(t *testing.T) {
	layout := v02DataDir(t, true, func(l Layout) {
		if err := os.MkdirAll(l.InteractionsDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		src := t.TempDir()
		db, err := sql.Open("sqlite", filepath.Join(src, "interactions.db")+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(`CREATE TABLE refused (from_aid TEXT NOT NULL, mid BLOB NOT NULL, ts INTEGER NOT NULL,
		  exp INTEGER NOT NULL, PRIMARY KEY (from_aid, mid))`); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"interactions.db", "interactions.db-wal"} { // as a crash leaves them
			b, err := os.ReadFile(filepath.Join(src, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(l.InteractionsDir(), name), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	d, err := New(layout)
	if d != nil {
		d.Close()
	}
	assertV02Refusal(t, "New", err)
	db, err := sql.Open("sqlite", filepath.Join(layout.InteractionsDir(), "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, objects, reason int
	if err := db.QueryRow(`SELECT (SELECT user_version FROM pragma_user_version), (SELECT count(*) FROM sqlite_master),
	  (SELECT count(*) FROM pragma_table_info('refused') WHERE name = 'reason')`).Scan(&version, &objects, &reason); err != nil {
		t.Fatal(err)
	}
	if version != 0 || objects != 2 || reason != 0 { // the table and its primary key index, as written
		t.Fatalf("the refused store was converted: user_version %d, %d schema objects, reason column %d", version, objects, reason)
	}
}

// FuzzDataDirVersions: the start check's readers of the key ring's v and
// the identity file's type tag never panic, and agree with how the start
// judges the file: a v the ring decoder takes is this version's, and a
// version tag read from an identity file is the tag's own number.
func FuzzDataDirVersions(f *testing.F) {
	for _, m := range []map[uint64]any{v02KeyRing(1), v02KeyRing(3), v02KeyRing(-1), {0: "anet.identity/3"},
		{0: "anet.identity/2", 1: []byte{0x80}}, {0: "anet.identity/03"}, {0: uint64(2)}} {
		b, err := coredet.Marshal(m)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte("not cbor"))
	f.Fuzz(func(t *testing.T, b []byte) {
		v, ok := keyRingFileVersion(b)
		if fk, err := decodeEncKeyFile(b); err == nil && (!ok || v != encKeyFileVersion || fk.V != v) {
			t.Fatalf("the ring decoder took v %d that the start reads as %d (%v)", fk.V, v, ok)
		}
		n, tagged := identityFileTagVersion(b)
		if tagged && identityFileIsV02(b) {
			t.Fatal("a tagged identity file is also 0.2's untagged one")
		}
		if tagged && (identityFileIsNewer(b) != (n > identityFileVersion) || identityFileTagIsOlder(b) != (n < identityFileVersion)) {
			t.Fatalf("tag version %d judged inconsistently", n)
		}
	})
}
