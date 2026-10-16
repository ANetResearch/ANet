package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// T-1 RT-F8: one recovery seed derives the key of every position, so a hub that serves a prefix of
// the real history (an old backup, or a hostile witness) would have `identity recover` reveal a
// key the real history used already, and the rotation signed with it forks the identity for good.
// The file says how long the history was when it was written; recover holds the hub to that.
func TestRecoverRefusesAHistoryThatIsNotTheFilesLength(t *testing.T) {
	hubID, _ := identity.InceptHub(uint64(time.Now().UnixMilli()))
	now := uint64(time.Now().UnixMilli())
	agent, _ := identity.InceptAgent(now)
	aid := agent.AID()
	rs, _ := newSeed()
	d2, _ := identity.NextDigest(rs, aid, 2)
	ev, err := agent.BuildWitnessDesignation(hubID.AID(), d2[:], now+10)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	stale := agent.KEL() // 2 events: what an old backup of the hub holds
	for seq := uint64(2); seq <= 3; seq++ {
		k, _ := identity.RecoverySeed(rs, aid, seq)
		nx, _ := identity.NextDigest(rs, aid, seq+1)
		e, err := agent.BuildRotation(k, nx[:], hubID.AID(), false, now+uint64(seq)*1000)
		if err != nil {
			t.Fatal(err)
		}
		if err := agent.Adopt(e); err != nil {
			t.Fatal(err)
		}
	}
	real := agent.KEL() // 4 events; the file below was written after the last rotation
	d4, _ := identity.NextDigest(rs, aid, 4)
	for _, tc := range []struct {
		name  string
		serve []identity.SignedEvent
		seq   uint64
		reply string
	}{
		{"hub behind the file", stale, 4, "recover_not_witness"},
		{"file behind the hub", real, 2, "recovery_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := identity.MarshalKEL(tc.serve)
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
			pass := "the recovery phrase of this test"
			commit := identity.Commit{Seq: tc.seq, Digest: d4}
			if tc.seq == 2 {
				commit.Digest = d2
			}
			file, err := sealRecovery(rs, aid, srv.URL, commit, []byte(pass))
			if err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			t.Setenv("ANET_HOME", home)
			prevHome, prevSettle, prevPoll, prevStart := idHome, idSettle, idPoll, idStartDaemon
			idHome = func() (string, error) { return home, nil }
			idSettle, idPoll = 50*time.Millisecond, 10*time.Millisecond
			started := false
			idStartDaemon = func(daemon.Layout) error { started = true; return nil }
			t.Cleanup(func() { idHome, idSettle, idPoll, idStartDaemon = prevHome, prevSettle, prevPoll, prevStart })
			src := filepath.Join(t.TempDir(), "saved.anetrec")
			if err := os.WriteFile(src, file, 0o600); err != nil {
				t.Fatal(err)
			}
			idTTY(t, pass, "a brand new phrase 1234", "a brand new phrase 1234")
			var out, errb bytes.Buffer
			x := &identityCLI{layout: daemon.Layout{Root: t.TempDir()}, pos: []string{src}, flags: map[string]string{"name": "back"},
				lang: a2ashape.English, out: &out, err: &errb}
			if err := x.recover(); err != errExit1 {
				t.Fatalf("recover: %v, said %q", err, errb.String())
			}
			want := a2ashape.Sentence(tc.reply, a2ashape.English, a2ashape.Args{"hub": srv.URL})
			if !strings.Contains(errb.String(), want) {
				t.Fatalf("said %q, want %q", errb.String(), want)
			}
			if started || daemon.IdentityLayout("back").Initialized() {
				t.Fatal("a data directory or a daemon was made")
			}
		})
	}
}
