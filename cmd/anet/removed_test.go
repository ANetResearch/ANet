package main

// UX-T7, the removed half (ux §4.3): each removed command and flag prints
// cli_removed with what to do instead and exits 2, before the CLI contacts
// the daemon or writes anything.

import (
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// removedInvocation is a full command line that uses r.
func removedInvocation(r removedCLI) []string {
	args := []string{r.cmd}
	switch r.cmd {
	case "delegate":
		args = append(args, "bafyreixtplpft7v4seh2kvj72ceuvw75efr6edt4sywb5wkh7d6kx2wxyz", "--capability", "translate")
	case "profile", "payments", "autoreply", "auto-reply":
		args = append(args, "set")
	case "hub-register":
		args = append(args, "https://hub.example")
	case "accept":
		args = append(args, "on")
	case "accept-end", "evidence":
		args = append(args, "t_9f3c")
	case "install":
		args = append(args, "--agent", "claude")
	case "x402-authorize", "pay-header":
		args = append(args, "--pay-to", "bafyreix", "--amount", "3")
	}
	if r.flag != "" {
		args = append(args, "--"+r.flag, "value")
	}
	return args
}

func TestRemovedCommandsAreRefusedBeforeTheDaemon(t *testing.T) {
	// ux §4.3 has 14 removed commands and flags (api-key aside); 0.3.1 adds the two auto-reply flags whose
	// values became fixed; each auto-reply flag is also spelled under auto-reply.
	if len(removedCommands) != 19 {
		t.Fatalf("ux §4.3 has 14 removed commands and flags (api-key aside), 0.3.1 two more, and three alias spellings, the table %d", len(removedCommands))
	}
	for _, lang := range []string{"C", "zh_CN.UTF-8"} {
		for _, r := range removedCommands {
			args := removedInvocation(r)
			t.Run(lang+"/"+strings.Join(args, "_"), func(t *testing.T) {
				h := newCLIHome(t, lang)
				fc := newFakeControl(t, h)
				before := snapshotDir(t, h.root)
				stdout, stderr, code := runCLI(t, h, args)
				l := a2ashape.LangFrom(func(k string) string { return map[string]string{"LANG": lang}[k] })
				want := a2ashape.Sentence("cli_removed", l, a2ashape.Args{"what": r.what, "instead": r.instead})
				if code != 2 || stdout != "" || stderr != want+"\n" {
					t.Errorf("exit %d, stdout %q, stderr %q; want 2 and %q", code, stdout, stderr, want)
				}
				if n := fc.conns.Load(); n != 0 {
					t.Errorf("the CLI contacted the control plane %d times", n)
				}
				if !sameSnapshot(before, snapshotDir(t, h.root)) {
					t.Error("the CLI changed files")
				}
			})
		}
	}
	// The harness sees a connection when there is one.
	h := newCLIHome(t, "C")
	fc := newFakeControl(t, h)
	runCLI(t, h, []string{"status"})
	if fc.conns.Load() == 0 {
		t.Fatal("anet status did not reach the fake control plane; the check above proves nothing")
	}
}

// The forms of a removed flag, and what is not one.
func TestRefuseRemovedMatching(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		rest []string
		want string // what; "" for not refused
	}{
		{"evidence", nil, "anet evidence"},
		{"delegate", []string{"aid", "--pay"}, "anet delegate --pay"},
		{"delegate", []string{"aid", "--pay=true"}, "anet delegate --pay"},
		{"delegate", []string{"aid", "--capability", "x"}, ""},
		{"delegate", []string{"aid", "--", "--pay"}, ""}, // a goal after -- is not a flag
		{"delegate", []string{"aid", "--payload"}, ""},
		{"hub-register", []string{"u", "--token=abc"}, "anet hub-register --token"},
		{"hub-register", []string{"u", "--token-file", "f"}, "anet hub-register --token-file"},
		{"hub-register", []string{"u", "--name", "n"}, ""},
		{"payments", []string{"set", "--payees-file", "f"}, "anet payments set --payees-file"},
		{"payments", []string{"set", "auto_max=5"}, ""},
		{"profile", []string{"set", "--summary", "s"}, ""},
		{"profile", []string{"set", "--pricing", "p"}, "anet profile set --pricing"},
		{"autoreply", []string{"set", "--api-key", "k"}, ""}, // api_key_on_argv, not this table
		{"status", nil, ""},
	} {
		s, ok := refuseRemoved(c.cmd, c.rest, a2ashape.English)
		if ok != (c.want != "") || (ok && !strings.HasPrefix(s, c.want+" was removed in 0.3.0; ")) {
			t.Errorf("%s %v: %v %q, want %q", c.cmd, c.rest, ok, s, c.want)
		}
	}
}

// Every removed entry's phrase is in the reason table, and every phrase of
// cli_removed is some entry's.
func TestRemovedCommandsHaveTheirPhrases(t *testing.T) {
	used := map[string]bool{}
	for _, r := range removedCommands {
		used[r.instead] = true
		s := a2ashape.Sentence("cli_removed", a2ashape.English, a2ashape.Args{"what": r.what, "instead": r.instead})
		if strings.Contains(s, "—") {
			t.Errorf("%s: no phrase %q in the reason table", r.what, r.instead)
		}
		if !strings.HasPrefix(r.what, "anet "+r.cmd) {
			t.Errorf("%s does not name the command %s", r.what, r.cmd)
		}
	}
	for _, id := range []string{"x402_authorize", "delegate_pay", "profile_pricing", "hub_register_token",
		"hub_register_accept_delegations", "p2p_advertise", "accept", "accept_end", "install", "evidence",
		"payments_payees_file", "require_image", "fixed_auto_reply"} {
		if !used[id] {
			t.Errorf("phrase %s is no removed entry's", id)
		}
	}
}

// FuzzRefuseRemoved: any command line is refused at most with a whole
// cli_removed sentence, and only for a listed command.
func FuzzRefuseRemoved(f *testing.F) {
	f.Add("delegate", "aid --pay")
	f.Add("hub-register", "u --token=x --name n")
	f.Add("evidence", "")
	f.Add("status", "-- --pay")
	f.Fuzz(func(t *testing.T, cmd, rest string) {
		s, ok := refuseRemoved(cmd, strings.Fields(rest), a2ashape.Chinese)
		if !ok {
			if s != "" {
				t.Fatal("a sentence without a refusal")
			}
			return
		}
		listed := false
		for _, r := range removedCommands {
			if r.cmd == cmd {
				listed = true
			}
		}
		if !listed || !strings.Contains(s, "已在 0.3.0 删除") || strings.ContainsAny(s, "{}") {
			t.Fatalf("%q %q: %q", cmd, rest, s)
		}
	})
}
