package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The text agentnetwork.org.cn serves as /SKILL.md, /llms-full.txt and
// /llms.txt (deploy/release/apex, docs/notes/0038) may only teach commands
// this binary has.
//
// The website's own copy went on teaching `anet whoami`, `anet daemon &` and
// `anet board` for months after 0.2.0 removed them, and an agent that reads
// it runs what it says. Every `anet …` inside code in those files must be a
// command `anet help --all` lists, a subcommand listed for it, and flags the
// flag checker accepts. The 0.1.x commands the files name in order to say
// they are gone are listed below, and must stay gone.
func TestApexDocsTeachOnlyRealCommands(t *testing.T) {
	gone := map[string]bool{"whoami": true, "board": true, "brain": true, "task publish": true}

	// What the help promises: commands, and for commands with literal
	// subcommands (agents wire, peers allow|trust, …) the set of them.
	cmds := map[string]bool{}
	subs := map[string]map[string]bool{}
	literal := regexp.MustCompile(`^[a-z][a-z-]*$`)
	for _, line := range strings.Split(usageAllText(), "\n") {
		syntax := regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(line), 2)[0]
		f := strings.Fields(syntax)
		if len(f) < 2 || f[0] != "anet" || !literal.MatchString(f[1]) {
			continue
		}
		cmds[f[1]] = true
		if len(f) > 2 {
			for _, s := range strings.Split(strings.Trim(f[2], "[]"), "|") {
				if literal.MatchString(s) {
					if subs[f[1]] == nil {
						subs[f[1]] = map[string]bool{}
					}
					subs[f[1]][s] = true
				}
			}
		}
	}
	if len(cmds) < 30 {
		t.Fatalf("only %d commands read from the help — the parse is probably broken", len(cmds))
	}
	for g := range gone {
		f := strings.Fields(g)
		if (len(f) == 1 && cmds[f[0]]) || (len(f) == 2 && subs[f[0]][f[1]]) {
			t.Errorf("the apex docs say `anet %s` is gone, but the help lists it", g)
		}
	}

	fence := regexp.MustCompile("(?s)```[a-z]*\n(.*?)```")
	inline := regexp.MustCompile("`([^`\n]+)`")
	seen := 0
	for _, name := range []string{"SKILL.md", "llms.txt"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "release", "apex", name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		// A removed command may be named only inline, in a paragraph that
		// says which release it belonged to.
		type snippet struct {
			text      string
			namesGone bool
		}
		var code []snippet
		for _, m := range fence.FindAllStringSubmatch(text, -1) {
			for _, l := range strings.Split(m[1], "\n") {
				code = append(code, snippet{l, false})
			}
		}
		for _, para := range strings.Split(fence.ReplaceAllString(text, ""), "\n\n") {
			for _, line := range strings.Split(para, "\n") {
				if strings.HasPrefix(line, "    ") { // an indented code block
					code = append(code, snippet{line, false})
					continue
				}
				for _, m := range inline.FindAllStringSubmatch(line, -1) {
					code = append(code, snippet{m[1], strings.Contains(para, "0.1.x")})
				}
			}
		}

		for _, sn := range code {
			line := sn.text
			if i := strings.Index(line, "#"); i >= 0 {
				line = line[:i]
			}
			for _, seg := range regexp.MustCompile(`&&|\|\||[|;]`).Split(line, -1) {
				f := strings.Fields(seg)
				for len(f) > 0 && f[0] != "anet" {
					f = f[1:]
				}
				if len(f) < 2 {
					continue
				}
				f = f[1:]
				if f[0] == "--id" && len(f) > 2 {
					f = f[2:]
				}
				cmd := f[0]
				if !literal.MatchString(cmd) {
					continue
				}
				seen++
				if gone[cmd] || (len(f) > 1 && gone[cmd+" "+f[1]]) {
					if !sn.namesGone {
						t.Errorf("%s: %q teaches a command 0.2 removed", name, strings.TrimSpace(seg))
					}
					continue
				}
				if !cmds[cmd] {
					t.Errorf("%s: `anet %s` is not a command (%q)", name, cmd, strings.TrimSpace(seg))
					continue
				}
				if len(f) > 1 && subs[cmd] != nil && literal.MatchString(f[1]) && !subs[cmd][f[1]] {
					t.Errorf("%s: `anet %s %s` is not a subcommand the help lists (%q)", name, cmd, f[1], strings.TrimSpace(seg))
				}
				for _, a := range f[1:] {
					if strings.HasPrefix(a, "--") {
						if err := checkFlags(cmd, []string{a, "v"}); err != nil {
							t.Errorf("%s: %q: %v", name, strings.TrimSpace(seg), err)
						}
					}
				}
			}
		}
	}
	if seen < 30 {
		t.Fatalf("only %d anet invocations found in the apex docs — the parse is probably broken", seen)
	}
}
