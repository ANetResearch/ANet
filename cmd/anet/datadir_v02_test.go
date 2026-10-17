package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// FS-S1 (core CORE-V4) through the binary: `anet daemon` and `anet up` in a
// data directory an earlier anet wrote — its interactions.db does not have
// user_version 3 — print the one sentence identity_v02 and exit 1, and
// write nothing there first: no config.json, no identity, no daemon log.
// The directory has no identity file this version reads, so a start that
// checked it only when it opened the store would already have made one.
func TestAnetDaemonRefusesAnEarlierDataDir(t *testing.T) {
	for _, lang := range []string{"C", "zh_CN.UTF-8"} {
		t.Run(lang, func(t *testing.T) {
			h := newCLIHome(t, lang)
			layout := daemon.NewLayout(h.data)
			writeEarlierStore(t, layout)
			before := snapshotDir(t, h.data)
			l := a2ashape.LangFrom(func(k string) string { return map[string]string{"ANET_LANG": lang}[k] })
			want := a2ashape.Sentence("identity_v02", l, nil)
			for _, args := range [][]string{{"daemon"}, {"up"}} {
				_, stderr, code := runCLIWithin(t, h, 20*time.Second, args)
				if code != 1 || stderr != want+"\n" {
					t.Fatalf("anet %s: exit %d, stderr %q; want 1 and %q", args[0], code, stderr, want)
				}
				if !sameSnapshot(before, snapshotDir(t, h.data)) {
					t.Fatalf("anet %s wrote to the refused data directory", args[0])
				}
			}
		})
	}
}

// writeEarlierStore writes interactions.db as anet 0.2 left it: a table
// and no user_version.
func writeEarlierStore(t *testing.T, l daemon.Layout) {
	t.Helper()
	if err := os.MkdirAll(l.InteractionsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(l.InteractionsDir(), "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE interaction (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// core CORE-V4 (revision 5) through the binary: in a data directory a newer
// anet wrote — its interactions.db has a user_version above 3 — `anet
// daemon` and `anet up` print datadir_too_new, whose next step is to
// upgrade and keep the directory, exit 1, and write nothing there.
func TestAnetDaemonRefusesANewerDataDir(t *testing.T) {
	h := newCLIHome(t, "C")
	layout := daemon.NewLayout(h.data)
	writeEarlierStore(t, layout)
	db, err := sql.Open("sqlite", filepath.Join(layout.InteractionsDir(), "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotDir(t, h.data)
	want := a2ashape.Sentence("datadir_too_new", a2ashape.English, nil)
	for _, args := range [][]string{{"daemon"}, {"up"}} {
		_, stderr, code := runCLIWithin(t, h, 20*time.Second, args)
		if code != 1 || stderr != want+"\n" {
			t.Fatalf("anet %s: exit %d, stderr %q; want 1 and %q", args[0], code, stderr, want)
		}
		if !sameSnapshot(before, snapshotDir(t, h.data)) {
			t.Fatalf("anet %s wrote to the refused data directory", args[0])
		}
	}
}
