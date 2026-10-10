package main

// `anet peers list` (SS-12; sealed-sender §5.2, ux UX-C1): a table for a
// person, the sender identity column in the control plane's words.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// TestPeersListTable: one line per listed peer under the column titles of
// the process's language — the whole AID first, the lists, and the sender
// identity sentence exactly as /peers/list gave it (UX-C5, UX-T1: the CLI
// words none of it, so a sentence the daemon did not send never shows);
// --json is the daemon's answer as it came, on stdout only (UX-C1).
func TestPeersListTable(t *testing.T) {
	const a2 = "bafyreiezxp2ekudenn7w2fa4eigj3vrgi2imeroc2e5v6l2cwliogscawa"
	// The daemon's words: whatever it says is what the column shows.
	hidden := "DAEMON-SAYS-HIDDEN"
	answer := `{"policy":"closed","allow":["` + wholeAID + `","` + a2 + `"],"trust":["` + wholeAID + `"],"deny":[],` +
		`"peers":[` +
		`{"aid":"` + wholeAID + `","lists":["allow","trust"],"sender_hidden":true,"sender_hidden_text":"` + hidden + `"},` +
		`{"aid":"` + a2 + `","lists":["allow"],"sender_hidden":false,"sender_hidden_text":"` +
		a2ashape.Sentence("peers.sender_visible.paid", a2ashape.Chinese, nil) + `"},` +
		`{"aid":"bad\u001b[31maid","lists":["deny"],"sender_hidden":false,"sender_hidden_text":"line\nbreak"}` +
		`]}`
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}

	jsonOut, errOut := captureCLI(t, func() {
		if err := runPeers(c, []string{"list", "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var back struct {
		Peers []peerRow `json:"peers"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &back); err != nil || len(back.Peers) != 3 || errOut != "" {
		t.Fatalf("--json: %v, %q, stderr %q", err, jsonOut, errOut)
	}

	for _, lang := range []string{"C", "zh_CN.UTF-8"} {
		t.Setenv("LC_ALL", "")
		t.Setenv("LC_MESSAGES", "")
		t.Setenv("LANG", lang)
		l := a2ashape.Lang()
		for _, args := range [][]string{nil, {"list"}, {"ls"}} {
			table, errOut := captureCLI(t, func() {
				if err := runPeers(c, args); err != nil {
					t.Fatal(err)
				}
			})
			lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
			if len(lines) != 4 || errOut != "" {
				t.Fatalf("%s %v: table\n%s\nstderr %q", lang, args, table, errOut)
			}
			head := a2ashape.Label("peers_list.aid", l)
			if !strings.HasPrefix(lines[0], head) || !strings.Contains(lines[0], a2ashape.Label("peers_list.lists", l)) ||
				!strings.HasSuffix(lines[0], a2ashape.Label("peers_list.sender", l)) {
				t.Errorf("%s: header %q", lang, lines[0])
			}
			f := strings.Fields(lines[1])
			if f[0] != wholeAID || f[1] != "allow,trust" || !strings.HasSuffix(lines[1], "  "+hidden) {
				t.Errorf("%s: row %q", lang, lines[1])
			}
			if f := strings.Fields(lines[2]); f[0] != a2 || f[1] != "allow" ||
				!strings.HasSuffix(lines[2], "  "+a2ashape.Sentence("peers.sender_visible.paid", a2ashape.Chinese, nil)) {
				t.Errorf("%s: row %q", lang, lines[2])
			}
			if strings.ContainsAny(lines[3], "\x1b\n") || !strings.HasPrefix(lines[3], "bad[31maid") ||
				!strings.HasSuffix(lines[3], "line break") {
				t.Errorf("%s: a line the daemon's values could break: %q", lang, lines[3])
			}
			// The columns line up: the sentence starts at the same terminal
			// column on every line.
			col := -1
			for i, ln := range lines {
				var at int
				if i == 0 {
					at = displayWidth(strings.TrimSuffix(ln, a2ashape.Label("peers_list.sender", l)))
				} else {
					text := []string{hidden, a2ashape.Sentence("peers.sender_visible.paid", a2ashape.Chinese, nil), "line break"}[i-1]
					at = displayWidth(strings.TrimSuffix(ln, text))
				}
				if col >= 0 && at != col {
					t.Errorf("%s: the last column is not aligned:\n%s", lang, table)
					break
				}
				col = at
			}
		}
	}
	for _, p := range paths {
		if p != "POST /peers/list" {
			t.Errorf("anet peers list asked %s", p)
		}
	}

	// An empty list is the header alone.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"policy":"closed","allow":[],"trust":[],"deny":[],"peers":[]}`))
	}))
	t.Cleanup(empty.Close)
	table, _ := captureCLI(t, func() {
		if err := runPeers(&client{base: empty.URL, token: "t", timeout: 5 * time.Second}, []string{"list"}); err != nil {
			t.Fatal(err)
		}
	})
	if n := strings.Count(table, "\n"); n != 1 {
		t.Errorf("an empty list: %q", table)
	}
}
