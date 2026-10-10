package main

// Fuzz targets of what SS-12 reads from the control plane: the /peers/list
// answer `anet peers list` turns into a table, and the /status answer
// doctor's sender privacy checks decide from. Whatever a daemon — or
// anything holding its port — answers, neither panics, and what reaches the
// terminal is one line per row with no control or direction-changing
// character.

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// safeLine reports whether a line printed for a person carries nothing that
// could move the cursor, recolour the terminal or reorder the text.
func safeLine(s string) bool {
	for _, r := range s {
		if r == '\n' || unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

func FuzzPeersListTable(f *testing.F) {
	for _, s := range []string{
		`{"peers":[{"aid":"bafyreie6k7xot2qpitfcoseuevnuq7zcztdpdgx4coe3p2jlqjjl65hqu4","lists":["allow","trust"],"sender_hidden":true,"sender_hidden_text":"x"}]}`,
		`{"peers":[{"aid":"a\u001b[2Jb","lists":["de‮ny"],"sender_hidden_text":"one\nline two"}]}`,
		`{"peers":[{"aid":"","lists":[],"sender_hidden_text":""},{"aid":"翻译","lists":["allow","","trust"]}]}`,
		`{"peers":[]}`, `{}`, `null`, `[]`, `{"peers":7}`, `{"peers":[{"lists":"allow"}]}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var list struct {
			Peers []peerRow `json:"peers"`
		}
		if json.Unmarshal(b, &list) != nil {
			return // peersList answers the error instead
		}
		for _, lang := range []a2ashape.Language{a2ashape.English, a2ashape.Chinese} {
			var out strings.Builder
			writePeersTable(&out, list.Peers, lang)
			lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			if len(lines) != len(list.Peers)+1 {
				t.Fatalf("%d peers made %d lines:\n%s", len(list.Peers), len(lines), out.String())
			}
			for i, l := range lines {
				if !safeLine(l) {
					t.Fatalf("line %d %q is not safe for a terminal", i, l)
				}
				if i > 0 && strings.TrimSpace(l) == "" {
					t.Fatalf("row %d is empty", i)
				}
			}
		}
	})
}

func FuzzDoctorSenderPrivacy(f *testing.F) {
	for _, s := range []string{
		`{"hub":{"state":"registered"},"sender_privacy":{"mailbox_registered":true,"session_ok":true,"held":3,"issued":1}}`,
		`{"hub":{"state":"registered"},"sender_privacy":{"session_ok":false,"refusal_downgrades_24h":9,"small_buckets_24h":2,"hub_epoch_invalid":true,"unpushed_revocation_age_s":99999,"unpushed_revocations_1h":4}}`,
		`{"hub":{"state":"registered"},"sender_privacy":{"held":-1,"unpushed_revocation_age_s":-5}}`,
		`{"hub":{"state":"registered"},"sender_privacy":{"held":"x"}}`,
		`{"hub":{"state":"registered"},"sender_privacy":null}`,
		`{"hub":"registered","sender_privacy":{}}`, `{"hub":{"state":"pending_admission"}}`, `{}`, `null`,
	} {
		f.Add([]byte(s))
	}
	layout := daemon.NewLayout(f.TempDir())
	known := map[string]bool{}
	for _, id := range senderPrivacyIDs {
		known[id] = true
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var st map[string]any
		if json.Unmarshal(b, &st) != nil || st == nil {
			return // daemonStatusAt reports the error: the daemon is taken as down
		}
		env := doctorEnv{running: func(string, string) bool { return true },
			status: func(string, string) (map[string]any, error) { return st, nil }}
		rep := &doctorReport{}
		rep.Control.Running = true
		var got []doctorCheck
		say := func(id, status, key string, args a2ashape.Args) {
			got = append(got, sayCheck(id, status, key, args, a2ashape.English))
		}
		add := func(id, status, detail, hint string) {
			got = append(got, doctorCheck{ID: id, Status: status, Detail: detail, Hint: hint})
		}
		senderPrivacyChecks(say, add, rep, env, layout, daemon.Config{HubURL: "https://hub.example"})
		if len(got) != 0 && len(got) != len(senderPrivacyIDs) {
			t.Fatalf("%d findings: %+v", len(got), got)
		}
		for i, c := range got {
			if c.ID != senderPrivacyIDs[i] || !known[c.ID] {
				t.Fatalf("finding %d is %s", i, c.ID)
			}
			switch c.Status {
			case stOK, stInfo, stWarn:
			default:
				t.Fatalf("%s: status %q", c.ID, c.Status)
			}
			if c.Reason != "" {
				if _, ok := a2ashape.Lookup(c.Reason); !ok || c.Detail == "" || strings.Contains(c.Detail, "{") {
					t.Fatalf("%s: reason %q, detail %q", c.ID, c.Reason, c.Detail)
				}
			}
			if !safeLine(c.Detail) {
				t.Fatalf("%s: detail %q", c.ID, c.Detail)
			}
		}
	})
}
