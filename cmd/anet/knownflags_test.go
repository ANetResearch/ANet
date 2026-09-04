package main

import (
	"os"
	"regexp"
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
