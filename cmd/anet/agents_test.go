//go:build !no_mcp

package main

import (
	"reflect"
	"strings"
	"testing"
)

// --a2a takes AIDs in every way the help's "<aid>…" suggests: after the
// flag up to the next flag or tool name, comma-separated, or repeated.
func TestAgentsArgs(t *testing.T) {
	for _, tc := range []struct {
		in    []string
		tools []string
		a2a   []string
		all   bool
		fresh bool
	}{
		{in: []string{"claude", "codex"}, tools: []string{"claude", "codex"}},
		{in: []string{"--all", "--refresh"}, all: true, fresh: true},
		{in: []string{"hermes", "--a2a", "aid1", "aid2"}, tools: []string{"hermes"}, a2a: []string{"aid1", "aid2"}},
		{in: []string{"--a2a", "aid1", "hermes"}, tools: []string{"hermes"}, a2a: []string{"aid1"}},
		{in: []string{"hermes", "--a2a=aid1,aid2", "--a2a", "aid3"}, tools: []string{"hermes"}, a2a: []string{"aid1", "aid2", "aid3"}},
		{in: []string{"--a2a", "aid1", "--refresh", "hermes"}, tools: []string{"hermes"}, a2a: []string{"aid1"}, fresh: true},
		{in: []string{"--a2a", "aid1", "claude-code"}, tools: []string{"claude-code"}, a2a: []string{"aid1"}},
	} {
		got, err := parseAgentsArgs(tc.in)
		if err != nil {
			t.Errorf("%v: %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got.tools, tc.tools) || !reflect.DeepEqual(got.a2a, tc.a2a) ||
			got.all != tc.all || got.refresh != tc.fresh {
			t.Errorf("%v: got %+v", tc.in, got)
		}
	}
	// `unwire hermes --a2a` with the AID forgotten must not fall back to a
	// plain unwire, which would remove the MCP entry and every token.
	for _, bad := range [][]string{{"--all", "claude"}, {"--force"}, {"hermes", "--a2a"}, {"--a2a", "hermes"},
		{"hermes", "--a2a="}} {
		if _, err := parseAgentsArgs(bad); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
}

// The help documents `anet agents` and its flags, and the flag checker
// accepts them.
func TestAgentsIsDocumentedAndItsFlagsAccepted(t *testing.T) {
	help := usageAllText()
	for _, want := range []string{"anet agents wire", "anet agents unwire", "--refresh", "--a2a"} {
		if !strings.Contains(help, want) {
			t.Errorf("help --all does not mention %q", want)
		}
	}
	if err := checkFlags("agents", []string{"wire", "--all", "--refresh", "--a2a", "x"}); err != nil {
		t.Fatal(err)
	}
}
