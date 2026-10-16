package interactions_test

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// dirState is what a directory holds: each file's size, content hash and
// modification time, by name.
type dirState map[string]string

func readDirState(t *testing.T, dir string) dirState {
	t.Helper()
	out := dirState{}
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = fmt.Sprintf("%d %x %d", len(b), sha256.Sum256(b), fi.ModTime().UnixNano())
	}
	return out
}

// writeOldStore writes, at dir/interactions.db, a store as an earlier anet
// left it: a schema and the given user_version (0 for v0.1 and v0.2, which
// set none), in WAL mode as they ran, closed cleanly.
func writeOldStore(t *testing.T, dir string, version int, wal bool) {
	t.Helper()
	dsn := filepath.Join(dir, "interactions.db")
	if wal {
		dsn += "?_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE interaction (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, role TEXT NOT NULL,
		  peer_aid TEXT NOT NULL, goal TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`INSERT INTO interaction(id, role, peer_aid, goal, status, created_at, updated_at)
		  VALUES('ix_old', 'inbound', 'did:anet:peer', 'an old task', 'queued', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		fmt.Sprintf(`PRAGMA user_version = %d`, version),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// readOnly opens dir/interactions.db for reading without changing it.
func readOnly(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "interactions.db")+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func columns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// A new store is created whole, at user_version 3 (core CORE-V4): with the
// cancel tombstones and the refused table's reason column (DLV §3.1,
// DLV-ST-10). Reopening it changes nothing.
func TestAFreshStoreIsVersion3(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = interactions.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s.Close()
	db := readOnly(t, dir)
	if interactions.SchemaVersion != 3 {
		t.Fatalf("SchemaVersion %d, want 3 (core CORE-V4)", interactions.SchemaVersion)
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 3 {
		t.Fatalf("user_version %d (%v), want 3", v, err)
	}
	if got, want := columns(t, db, "cancel_tomb"), []string{"exp", "from_aid", "ix"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cancel_tomb columns %v, want %v", got, want)
	}
	if got, want := columns(t, db, "refused"), []string{"exp", "from_aid", "mid", "reason", "ts"}; !reflect.DeepEqual(got, want) {
		t.Errorf("refused columns %v, want %v", got, want)
	}
	var decl string
	if err := db.QueryRow(`SELECT type || ' ' || "notnull" || ' ' || dflt_value FROM pragma_table_info('refused') WHERE name='reason'`).Scan(&decl); err != nil ||
		decl != "TEXT 1 ''" {
		t.Errorf("refused.reason is %q (%v), want TEXT NOT NULL DEFAULT ''", decl, err)
	}
	var idx int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_cancel_tomb_exp' AND tbl_name='cancel_tomb'`).Scan(&idx); err != nil || idx != 1 {
		t.Errorf("idx_cancel_tomb_exp: %d (%v)", idx, err)
	}
	// Nothing of the v0.1 schema is created.
	for _, c := range columns(t, db, "interaction") {
		if c == "status" {
			t.Errorf("interaction has the wire-1 status column")
		}
	}
}

// A store an earlier anet wrote — v0.1 and v0.2 set no user_version, and
// any value below 3 is not this version's — is refused, not converted
// (core CORE-V4, FS-S1, DLV-ST-10); so is one a newer anet wrote, with the
// user_version above 3, but as ErrNewerStore and never as ErrOldStore: its
// next step is to upgrade, not to move the data away. The refusal writes
// nothing: the file keeps its bytes and its time, and no -wal or -shm file
// appears.
func TestOpenRefusesAnEarlierStore(t *testing.T) {
	for _, tc := range []struct {
		version int
		wal     bool
		want    error
	}{
		{0, true, interactions.ErrOldStore}, {0, false, interactions.ErrOldStore}, {1, true, interactions.ErrOldStore},
		{2, true, interactions.ErrOldStore}, {2, false, interactions.ErrOldStore},
		{4, true, interactions.ErrNewerStore}, {4, false, interactions.ErrNewerStore}, {1 << 30, true, interactions.ErrNewerStore},
	} {
		t.Run(fmt.Sprintf("user_version %d wal %v", tc.version, tc.wal), func(t *testing.T) {
			dir := t.TempDir()
			writeOldStore(t, dir, tc.version, tc.wal)
			before := readDirState(t, dir)
			time.Sleep(10 * time.Millisecond) // a write would show in the modification time
			err := interactions.CheckStore(dir)
			if !errors.Is(err, tc.want) || errors.Is(err, interactions.ErrOldStore) && errors.Is(err, interactions.ErrNewerStore) {
				t.Fatalf("CheckStore: %v, want %v", err, tc.want)
			}
			s, err := interactions.Open(dir)
			if !errors.Is(err, tc.want) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open: %v, want %v", err, tc.want)
			}
			if after := readDirState(t, dir); !reflect.DeepEqual(after, before) {
				t.Fatalf("the refused store's directory changed:\n before %v\n after  %v", before, after)
			}
		})
	}
}

// No store, or a file with no schema in it (a creation that never
// committed), is a new store.
func TestCheckStoreTakesNoStoreAsNew(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-yet")
	if err := interactions.CheckStore(missing); err != nil {
		t.Fatalf("no directory: %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CheckStore made the directory: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "interactions.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := interactions.CheckStore(dir); err != nil {
		t.Fatalf("an empty file: %v", err)
	}
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatalf("open an empty file: %v", err)
	}
	s.Close()
	var v int
	if err := readOnly(t, dir).QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 3 {
		t.Fatalf("user_version %d (%v), want 3", v, err)
	}
	if err := interactions.CheckStore(dir); err != nil {
		t.Fatalf("a v0.3.0 store: %v", err)
	}
}

// A file that is not a database is an error, and not taken for an earlier
// anet's store.
func TestCheckStoreReportsAFileThatIsNotADatabase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "interactions.db"), []byte("this is not a database, it is a note to self"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := interactions.CheckStore(dir)
	if err == nil || errors.Is(err, interactions.ErrOldStore) {
		t.Fatalf("CheckStore: %v, want an error that is not ErrOldStore", err)
	}
}

// writeOldStoreInTheLog writes, at dir/interactions.db, the store of an
// earlier anet that stopped without closing it (killed, or the machine went
// down) before SQLite moved anything from its write-ahead log into the
// file: the schema and the row are in interactions.db-wal alone, and the
// file holds no schema yet.
func writeOldStoreInTheLog(t *testing.T, dir string) {
	t.Helper()
	src := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(src, "interactions.db")+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE interaction (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, role TEXT NOT NULL,
		  peer_aid TEXT NOT NULL, goal TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`INSERT INTO interaction(id, role, peer_aid, goal, status, created_at, updated_at)
		  VALUES('ix_old', 'inbound', 'did:anet:peer', 'an old task', 'queued', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// Copied while the store is still open, as a crash leaves it.
	for _, name := range []string{"interactions.db", "interactions.db-wal"} {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// An earlier anet's store whose schema is still only in its write-ahead
// log looks new to CheckStore, which reads the file alone. Open reads
// through the log, refuses the store all the same and converts nothing:
// the earlier table and its row are as they were, no table of this version
// is added and user_version stays 0. (Closing the refused store moves its
// log into the file; what the store holds does not change.)
func TestOpenRefusesAnEarlierStoreFoundOnlyInTheLog(t *testing.T) {
	dir := t.TempDir()
	writeOldStoreInTheLog(t, dir)
	var inFile int
	if err := readOnly(t, dir).QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&inFile); err != nil || inFile != 0 {
		t.Fatalf("setup: the file itself holds %d schema objects (%v); the schema is meant to be in the log alone", inFile, err)
	}
	s, err := interactions.Open(dir)
	if !errors.Is(err, interactions.ErrOldStore) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open: %v, want ErrOldStore", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, rows int
	var tables []string
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 0 {
		t.Errorf("user_version %d (%v), want 0", version, err)
	}
	r, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var n string
		if err := r.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	r.Close()
	if !reflect.DeepEqual(tables, []string{"interaction"}) {
		t.Errorf("tables %v, want the earlier store's [interaction] alone", tables)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM interaction WHERE id = 'ix_old' AND status = 'queued'`).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("the earlier row: %d (%v), want 1 as it was written", rows, err)
	}
}
