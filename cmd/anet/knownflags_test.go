package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A misspelled flag must be refused, not dropped.
//
// `anet find --capability text.digest` used to return the unfiltered
// directory with exit status 0 — indistinguishable from a query that
// matched everything, and the reason a caller could delegate to whoever
// happened to be first in the list.
func TestUnknownFlagsAreRefused(t *testing.T) {
	for _, tc := range []struct{ cmd, flag string }{
		{"find", "--capability"},
		{"hub-register", "--nmae"},
		{"delegate", "--cap"}, // the real one is --capability
		{"redeem", "--reff"},
		{"evidence", "--kind"}, // the real one is --type
	} {
		err := checkFlags(tc.cmd, []string{tc.flag, "value"})
		if err == nil {
			t.Errorf("%s %s: accepted a flag it does not take", tc.cmd, tc.flag)
			continue
		}
		if !strings.Contains(err.Error(), tc.flag[2:]) {
			t.Errorf("%s %s: the error must name the flag: %v", tc.cmd, tc.flag, err)
		}
	}
}

// A command that takes no flags says so, rather than listing an empty set.
func TestACommandWithNoFlagsSaysSo(t *testing.T) {
	err := checkFlags("results", []string{"--limit", "5"})
	if err == nil || !strings.Contains(err.Error(), "takes no flags") {
		t.Fatalf("expected a no-flags message, got %v", err)
	}
}

func TestKnownFlagsAreAccepted(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		args []string
	}{
		{"find", []string{"--cap", "text.digest"}},
		{"find", []string{"--cap=text.digest"}},
		{"hub-register", []string{"http://h", "--name", "n", "--caps", "a,b", "--token", "t"}},
		{"delegate", []string{"aid", "--capability", "x", "--args", `{"a":1}`, "--pay"}},
		{"message", []string{"ix", "--file", "/tmp/x", "--attach", "/tmp/y"}},
		{"autoreply", []string{"set", "--backend", "openai", "--api-base", "u", "--model", "m"}},
		{"verify", []string{"--receipt", "r", "--kel", "k", "--result", "f"}},
		{"install", []string{"--agent", "claude"}},
		{"results", nil},
	} {
		if err := checkFlags(tc.cmd, tc.args); err != nil {
			t.Errorf("%s %v: a documented flag was refused: %v", tc.cmd, tc.args, err)
		}
	}
}

// Positional arguments are not flags, and a goal or message may contain
// anything — including a leading dash.
func TestPositionalArgumentsAreNotChecked(t *testing.T) {
	if err := checkFlags("delegate", []string{"aid", "--", "-not-a-flag"}); err != nil {
		t.Fatalf("a bare -- and a dashed positional must pass: %v", err)
	}
}

// The checker must accept everything the help promises.
//
// This is the direction that actually hurts. The first version of the table
// was written by scanning `flags["…"]` in the top-level case blocks, which
// misses every other way a flag is read — `hasFlag(rest, "--print")`, and
// the map literals `runAutoReply` iterates. Seven flags were left out, and
// `anet up --all`, `anet console --print` and
// `anet autoreply set --poll-interval 10` — all documented, all implemented
// — started exiting 2. A hand-maintained allowlist drifts toward refusing
// valid input, which is worse than the silence it replaced.
//
// So the test reads the user-facing contract instead of the code: every
// `--flag` that `anet help --all` shows for a command has to be accepted for
// that command. Found by the documentation fact-check, not by this suite's
// earlier drift test.
func TestTheFlagCheckerAcceptsEverythingTheHelpPromises(t *testing.T) {
	help := usageAllText()
	cmdRe := regexp.MustCompile(`^\s+anet ([a-z0-9-]+)`)
	flagRe := regexp.MustCompile(`--([a-z0-9-]+)`)

	seen := 0
	for _, line := range strings.Split(help, "\n") {
		m := cmdRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cmd := m[1]
		if _, checked := knownFlags[cmd]; !checked {
			continue // not gated; see checkFlags
		}
		for _, f := range flagRe.FindAllStringSubmatch(line, -1) {
			flag := f[1]
			// `--all` in `logs [N|--all]` and the like are real; a bare
			// `--` never appears. Nothing else to exclude.
			if err := checkFlags(cmd, []string{"--" + flag, "v"}); err != nil {
				t.Errorf("help says `anet %s … --%s` but the checker refuses it: %v", cmd, flag, err)
			}
			seen++
		}
	}
	if seen < 15 {
		t.Fatalf("only %d documented flags were checked — the help parse is probably broken", seen)
	}
}

// Every flag the source documents anywhere must be accepted by something.
//
// The test above reads `anet help --all`, which attributes flags to commands
// precisely — and misses the ones documented in a command's own usage string.
// `--poll-interval`, `--max-history`, `--api-timeout` and `--max-auto-replies`
// live in the string `runAutoReply` prints, so leaving them out of the table
// broke `anet autoreply set` while that test stayed green.
//
// This one gives up on attribution and asks a weaker question the source can
// answer: a flag the code tells a user to type must be typeable somewhere. It
// cannot catch a flag listed under the wrong command; it does catch a flag
// listed under no command at all, which is the failure that actually happened.
func TestEveryDocumentedFlagIsAcceptedBySomeCommand(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	// Not per-command flags:
	//   version / help — dispatched as commands (`anet --version` is `anet version`)
	//   id             — a GLOBAL identity selector, pulled out by
	//                    extractGlobalID before dispatch, so checkFlags never
	//                    sees it (verified: `anet --id foo status` reaches the
	//                    daemon lookup for that identity)
	notFlags := map[string]bool{"version": true, "help": true, "id": true}

	accepted := map[string]bool{}
	for _, ks := range knownFlags {
		for _, k := range ks {
			accepted[k] = true
		}
	}
	// Comments first: one of them describes the general shape as
	// "--key value" and "--bool", which are not flags any command takes.
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAll(src, []byte(" "))
	src = regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAll(src, []byte(" "))
	strRe := regexp.MustCompile("`[^`]*`|\"(?:[^\"\\\\]|\\\\.)*\"")
	flagRe := regexp.MustCompile(`--([a-z][a-z0-9-]+)`)
	missing := map[string]bool{}
	for _, lit := range strRe.FindAllString(string(src), -1) {
		for _, m := range flagRe.FindAllStringSubmatch(lit, -1) {
			f := m[1]
			if notFlags[f] || accepted[f] {
				continue
			}
			missing[f] = true
		}
	}
	if len(missing) > 0 {
		var list []string
		for f := range missing {
			list = append(list, "--"+f)
		}
		sort.Strings(list)
		t.Fatalf("the source documents flags no command accepts: %s", strings.Join(list, " "))
	}
}

// The table has to stay level with the code.
//
// Every flag the source actually reads for a command must be listed for
// that command, or the check refuses usage the command supports. A
// hand-maintained list drifts toward rejecting valid input, which is worse
// than the silence it replaced — so the list is compared against the
// source rather than trusted.
func TestEveryFlagTheSourceReadsIsInTheTable(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	caseRe := regexp.MustCompile(`^(\t+)case (.+):$`)
	flagRe := regexp.MustCompile(`flags\["([^"]+)"\]`)

	// Only top-level command cases: one tab of indent inside the dispatch
	// switches. Deeper cases belong to sub-switches (identity verbs,
	// autoreply verbs) whose flags are attributed to the parent command.
	type blk struct {
		start, depth int
		names        []string
	}
	var blocks []blk
	nameRe := regexp.MustCompile(`"([^"]+)"`)
	for i, l := range lines {
		if m := caseRe.FindStringSubmatch(l); m != nil {
			var names []string
			for _, n := range nameRe.FindAllStringSubmatch(m[2], -1) {
				names = append(names, n[1])
			}
			blocks = append(blocks, blk{i, len(m[1]), names})
		}
	}
	missing := map[string][]string{}
	for bi, b := range blocks {
		if b.depth != 1 {
			continue
		}
		end := len(lines)
		for _, nb := range blocks[bi+1:] {
			if nb.depth <= b.depth {
				end = nb.start
				break
			}
		}
		read := map[string]bool{}
		for _, m := range flagRe.FindAllStringSubmatch(strings.Join(lines[b.start:end], "\n"), -1) {
			read[m[1]] = true
		}
		for _, name := range b.names {
			known, listed := knownFlags[name]
			if !listed {
				continue // not checked at all; see checkFlags
			}
			set := map[string]bool{}
			for _, k := range known {
				set[k] = true
			}
			for k := range read {
				if !set[k] {
					missing[name] = append(missing[name], k)
				}
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("knownFlags is missing flags the source reads: %v", missing)
	}
}
