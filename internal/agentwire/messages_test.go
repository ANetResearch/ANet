//go:build !no_mcp

package agentwire

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// The package holds no sentence of its own: every key it names is a row of
// the reason table, every error key is a wire_ code of class C with a next
// step, and no source file carries a Chinese character.
func TestEveryKeyIsARowAndNoSentenceIsHardCoded(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	argKey := regexp.MustCompile(`"(wire[._][a-z0-9_.]+)"`)
	keys := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if strings.IndexFunc(src, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
			t.Errorf("%s holds a Chinese character; sentences come from the reason table", f)
		}
		for _, m := range argKey.FindAllStringSubmatch(src, -1) {
			keys[m[1]] = true
		}
	}
	if len(keys) < 40 {
		t.Fatalf("found only %d keys in the sources; the pattern is stale", len(keys))
	}
	for k := range keys {
		r, ok := a2ashape.Lookup(k)
		if !ok || r.Key() != k {
			t.Errorf("%s is used in the package and is not a row of the reason table", k)
			continue
		}
		if strings.HasPrefix(k, "wire_") {
			if r.Class&a2ashape.ClassC == 0 || r.NextKind == a2ashape.NextUnset {
				t.Errorf("%s: an error of the package is class C with a next step", k)
			}
			if a2ashape.ReasonCode(k) == "" || len(a2ashape.ReasonCode(k)) > 30 {
				t.Errorf("%s: not a reason code", k)
			}
		}
	}
}

// A real refusal reads in both languages with its values in it, and the
// reason code is the key without its variant.
func TestRefusalsRenderInBothLanguages(t *testing.T) {
	h := newHost(t)
	cases := map[string]string{
		"toml": "[mcp_servers.anet]\ncommand = \"x\"\n",
	}
	h.write(".codex/config.toml", cases["toml"], 0o644)
	r := one(t, h.wire(ToolCodex), Conflict)
	m, ok := AsMsg(r.Err)
	if !ok || m.Key != "wire_conflict.toml" || m.Code() != "wire_conflict" || argLine(m) != 1 {
		t.Fatalf("conflict: %+v", r.Err)
	}
	for lang, want := range map[a2ashape.Language]string{
		a2ashape.English: "already defines an entry mcp_servers.anet that anet did not write",
		a2ashape.Chinese: "已定义了一个不是 anet 写的 mcp_servers.anet",
	} {
		got := m.Text(lang)
		if !strings.Contains(got, want) || !strings.Contains(got, ".codex/config.toml") || strings.ContainsAny(got, "{}") {
			t.Errorf("language %d: %q", lang, got)
		}
	}
	// The Hermes entries need the local A2A interface: a command to run.
	e := fail("wire_a2a_not_started", "path", "/d/a2a_addr.txt")
	if e.Next(a2ashape.English) != "anet up" || e.Code() != "wire_a2a_not_started" || !strings.Contains(e.Error(), "/d/a2a_addr.txt") {
		t.Errorf("a2a not started: %q next %q", e.Error(), e.Next(a2ashape.English))
	}
}

// Text of a user's file is never a placeholder value: a config whose lines
// hold markup, a Chinese sentence or control characters shows up in no
// sentence, only its path and line.
func TestUserFileTextIsNotInterpolated(t *testing.T) {
	h := newHost(t)
	cfg := "# {path} 你好 \x1b[31m\n[mcp_servers.\"anet\"]\ncommand = \"x\"\n"
	h.write(".codex/config.toml", cfg, 0o644)
	r := one(t, h.wire(ToolCodex), Conflict)
	m, _ := AsMsg(r.Err)
	for _, lang := range []a2ashape.Language{a2ashape.English, a2ashape.Chinese} {
		got := m.Text(lang)
		if strings.Contains(got, "\x1b") || strings.Contains(got, "你") || strings.Contains(got, "# {path}") {
			t.Errorf("a line of the file reached the sentence: %q", got)
		}
	}
}
