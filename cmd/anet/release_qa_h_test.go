package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// dispatchCommands reads the verb names main.go actually dispatches on.
//
// Read from the source rather than kept as a second list, because a second list is the thing that
// drifts: the defect these tests guard is a hand-written string that named commands nobody can type.
// Each entry is one `case` group, so aliases stay together ("message", "msg").
func dispatchCommands(t *testing.T) [][]string {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	caseRe := regexp.MustCompile(`^\tcase (.+):$`)
	nameRe := regexp.MustCompile(`"([^"]+)"`)
	var groups [][]string
	in := false
	for _, l := range lines {
		if l == "\tswitch cmd {" {
			in = true
			continue
		}
		if !in {
			continue
		}
		if l == "\t}" { // end of this switch on cmd
			in = false
			continue
		}
		m := caseRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		var names []string
		for _, n := range nameRe.FindAllStringSubmatch(m[1], -1) {
			names = append(names, n[1])
		}
		if len(names) > 0 {
			groups = append(groups, names)
		}
	}
	if len(groups) < 25 {
		t.Fatalf("only %d dispatch cases were found — the source parse is broken", len(groups))
	}
	return groups
}

// Every word in the opening banner has to be a command the reader can type.
//
// It printed "register · find · delegate · chat · end · review". Two of those six are not commands:
// registering is `hub-register` and chatting is `message`. The banner is the first line `anet` prints
// with no arguments, so a reader following it literally met `unknown command` twice before finding the
// real names — one step to self-correct each time, on the first two things they tried.
func TestTheBannerNamesOnlyRealCommands(t *testing.T) {
	known := map[string]bool{}
	for _, g := range dispatchCommands(t) {
		for _, n := range g {
			known[n] = true
		}
	}
	banner := guideBanner()
	list, _, ok := strings.Cut(banner, " — ")
	if !ok {
		t.Fatalf("the banner must keep its `commands — sentence` shape: %q", banner)
	}
	words := strings.Split(list, " · ")
	if len(words) < 4 {
		t.Fatalf("the banner should still walk the reader through the path: %q", banner)
	}
	for _, w := range words {
		w = strings.TrimSpace(w)
		if !known[w] {
			t.Errorf("the banner shows %q, which `anet %s` answers with `unknown command`", w, w)
		}
	}
}

// A command that exists must be listed by `anet help --all`.
//
// `anet visibility` is the switch that puts an entry into the federated directory, defaults to
// hub-local (off), and was absent from the full help — an operator could not find out it existed. The
// same gap is possible for every other command, so the check is over all of them rather than that one.
func TestEveryDispatchedCommandIsInTheFullHelp(t *testing.T) {
	help := usageAllText()
	// `help` itself is how a reader got here, and the dash-spelled aliases (`--version`, `-h`) are
	// spellings of commands that ARE listed; neither is a command anyone needs the list to discover.
	exempt := map[string]bool{"help": true}
	for _, group := range dispatchCommands(t) {
		listed := false
		skip := true
		for _, name := range group {
			if strings.HasPrefix(name, "-") || exempt[name] {
				continue
			}
			skip = false
			if strings.Contains(help, "anet "+name+" ") || strings.Contains(help, "anet "+name+"\n") {
				listed = true
			}
		}
		if skip || listed {
			continue
		}
		t.Errorf("`anet %s` is dispatched but `anet help --all` never mentions it", group[0])
	}
}

// The hub's "this recipient has not collected its mail" has to reach the person who typed the command.
//
// The daemon carries it on the /delegate answer; the CLI pretty-prints that object, where one more key
// among five reads as noise. The extra line is what an operator actually sees before they start waiting
// for a reply that may be days out. The delegation is queued either way, so this is a note, not an error
// — the exit status stays 0.
func TestDelegatePrintsTheQuietNotice(t *testing.T) {
	const warning = "queued, but bafyreiabc has not collected its mail for 3 days — it may not be running"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"interaction_id":"ix-1","status":"queued","recipient_quiet":true,` +
			`"warning":"` + warning + `"}`))
	}))
	defer srv.Close()

	c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
	var err error
	out := captureStdout(t, func() { err = c.do("/delegate", map[string]any{"provider": "bafyreiabc"}) })
	if err != nil {
		t.Fatalf("a queued delegation with a quiet recipient is not a failure: %v", err)
	}
	if !strings.Contains(out, `"interaction_id": "ix-1"`) {
		t.Errorf("the answer itself must still be printed:\n%s", out)
	}
	notice := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "note:") {
			notice = line
		}
	}
	if notice == "" {
		t.Fatalf("the hub's warning was dropped instead of shown on its own line:\n%s", out)
	}
	if !strings.Contains(notice, "3 days") || !strings.Contains(notice, "not collected its mail") {
		t.Errorf("the notice must say how long the recipient has been silent: %q", notice)
	}
}

// A response with no warning prints no note.
func TestAnOrdinaryAnswerPrintsNoNotice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"interaction_id":"ix-2","status":"queued"}`))
	}))
	defer srv.Close()

	c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
	out := captureStdout(t, func() { _ = c.do("/delegate", nil) })
	if strings.Contains(out, "note:") {
		t.Errorf("nothing was warned about; there must be no note:\n%s", out)
	}
}
