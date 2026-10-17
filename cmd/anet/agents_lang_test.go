//go:build !no_mcp

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/agentwire"
)

var hanRe = regexp.MustCompile(`\p{Han}`)

// agentsEnv is the extra environment of an `anet agents` run: no tool on
// PATH, so nothing of the machine running the test is found or started.
func agentsEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	return append([]string{"PATH=" + t.TempDir(), "ANET_LANG="}, extra...)
}

// agentsBin is a copy of the test binary under a plain name, the same one for
// every run of a test: the entries wire writes name this path, and a second
// path would make them out of date.
func agentsBin(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	agentsBins.Lock()
	defer agentsBins.Unlock()
	if p, ok := agentsBins.m[t.Name()]; ok {
		return p
	}
	bin := filepath.Join(t.TempDir(), "anet")
	if err := os.Link(exe, bin); err != nil {
		b, rerr := os.ReadFile(exe)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if err := os.WriteFile(bin, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if agentsBins.m == nil {
		agentsBins.m = map[string]string{}
	}
	agentsBins.m[t.Name()] = bin
	return bin
}

var agentsBins struct {
	sync.Mutex
	m map[string]string
}

// runAgentsCLI is runCLI on a copy of the test binary under a plain name: wire
// refuses a binary that lives in a go build directory, as a `go run` build does.
func runAgentsCLI(t *testing.T, h *cliHome, args []string, extra ...string) (stdout, stderr string, code int) {
	t.Helper()
	bin := agentsBin(t)
	cmd := exec.Command(bin, args...)
	cmd.Env = h.env(extra...)
	cmd.Dir = h.root
	detachFromTerminal(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// skipUnlessSelectorIsANETLANG skips a test that depends on the language
// coming from ANET_LANG alone (core G13) while the selector still reads the
// locale variables.
func skipUnlessSelectorIsANETLANG(t *testing.T) {
	t.Helper()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if a2ashape.LangFrom(env(map[string]string{"LANG": "zh_CN.UTF-8"})) == a2ashape.Chinese {
		t.Skip("the language selector still reads LANG")
	}
}

// The output of `anet agents wire` is English when ANET_LANG is unset, even
// under a Chinese locale, and every line carries no Chinese.
func TestAgentsWireIsEnglishUnderChineseLocale(t *testing.T) {
	skipUnlessSelectorIsANETLANG(t)
	h := newCLIHome(t, "zh_CN.UTF-8")
	out, errs, code := runAgentsCLI(t, h, []string{"agents", "wire", "cursor", "workbuddy"}, agentsEnv(t)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if hanRe.MatchString(out + errs) {
		t.Fatalf("Chinese in the default output:\n%s%s", out, errs)
	}
	for _, want := range []string{"anet agents wire (anet ", "cursor", "written", "the tool was not detected; written as requested",
		"Restart these tools", "To undo: anet agents unwire <tool> (or --all)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	out, _, _ = runAgentsCLI(t, h, []string{"agents"}, agentsEnv(t)...)
	if hanRe.MatchString(out) || !strings.Contains(out, "wired, up to date") {
		t.Errorf("status:\n%s", out)
	}
}

// ANET_LANG=zh-CN switches the same lines to Chinese.
func TestAgentsWireIsChineseWithANETLANG(t *testing.T) {
	skipUnlessSelectorIsANETLANG(t)
	h := newCLIHome(t, "en_US.UTF-8")
	out, errs, code := runAgentsCLI(t, h, []string{"agents", "wire", "cursor"}, agentsEnv(t, "ANET_LANG=zh-CN")...)
	if code != 0 || !strings.Contains(out, "已写入") || !strings.Contains(out, "重启这些工具") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
}

// A refusal prints its sentence and reason code, then its next step after
// the arrow, and ends with exit status 1 (ux UX-R2, UX-C4).
func TestAgentsRefusalsCarryCodeAndNextStep(t *testing.T) {
	h := newCLIHome(t, "en_US.UTF-8")

	// A tool name wire does not know.
	out, errs, code := runAgentsCLI(t, h, []string{"agents", "wire", "nosuchtool"}, agentsEnv(t)...)
	if code != 1 || out != "" || !strings.Contains(errs, "nosuchtool is not a supported tool") || !strings.Contains(errs, "[wire_unknown_tool]") {
		t.Errorf("unknown tool: exit %d\n%s\n%s", code, out, errs)
	}

	// An entry anet did not write.
	cursor := filepath.Join(h.home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cursor), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := `{"mcpServers": {"anet": {"command": "other"}}}` + "\n"
	if err := os.WriteFile(cursor, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errs, code = runAgentsCLI(t, h, []string{"agents", "wire", "cursor"}, agentsEnv(t)...)
	if code != 1 || !strings.Contains(out, "conflict") || !strings.Contains(out, "[wire_conflict]") ||
		!strings.Contains(errs, "[wire_tools_failed]") {
		t.Errorf("conflict: exit %d\n%s\n%s", code, out, errs)
	}
	if b, _ := os.ReadFile(cursor); string(b) != foreign {
		t.Errorf("the conflicting file was changed: %s", b)
	}
	out, _, _ = runAgentsCLI(t, h, []string{"agents"}, agentsEnv(t)...)
	if !strings.Contains(out, "conflict: ") {
		t.Errorf("status does not show the conflict:\n%s", out)
	}

	// Hermes entries need the local A2A interface to have been started.
	out, errs, code = runAgentsCLI(t, h, []string{"agents", "wire", "hermes", "--a2a", "bafyreiaaaaaaaaaaaaaa"}, agentsEnv(t)...)
	if code != 1 || !strings.Contains(out, "[wire_a2a_not_started]") || !strings.Contains(out, nextPrefix[2:]+"anet up") {
		t.Errorf("a2a not started: exit %d\n%s\n%s", code, out, errs)
	}
}

// Each of the two languages renders a real result without a placeholder or a
// dash left in it, and the Chinese one has the Chinese status words.
func TestPrintWireResultsInBothLanguages(t *testing.T) {
	home := t.TempDir()
	opts := agentwire.Options{Bin: "/opt/anet/anet", DataDir: filepath.Join(home, ".anet"), Home: home,
		Getenv:   func(string) string { return "" },
		LookPath: func(string) (string, error) { return "", errors.New("not on PATH") },
		Run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("no tool CLI is run in this test")
		},
		Now: func() time.Time { return time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC) },
	}
	res, err := agentwire.Wire(opts, []string{"codex", "opencode"}, false)
	if err != nil {
		t.Fatal(err)
	}
	for lang, want := range map[a2ashape.Language][]string{
		a2ashape.English: {"anet agents wire (anet /opt/anet/anet, data directory " + opts.DataDir + ")", "codex", "written",
			"[mcp_servers.anet] → ~/.codex/config.toml", "mcp.anet → ~/.config/opencode/opencode.json",
			"note: the tool was not detected; written as requested"},
		a2ashape.Chinese: {"anet agents wire（anet /opt/anet/anet，数据目录 " + opts.DataDir + "）", "已写入",
			"注意：未检测到该工具，按要求照写", "重启这些工具（或新开会话）后生效", "撤销：anet agents unwire <工具>（或 --all）"},
	} {
		var out, errw bytes.Buffer
		if err := printWireResults(&out, &errw, lang, "wire", res, opts); err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("language %d: output lacks %q:\n%s", lang, w, out.String())
			}
		}
		if strings.ContainsAny(out.String(), "{}") || strings.Contains(out.String(), " — ") {
			t.Errorf("language %d: a placeholder is left:\n%s", lang, out.String())
		}
		if lang == a2ashape.English && hanRe.MatchString(out.String()) {
			t.Errorf("Chinese in the English output:\n%s", out.String())
		}
	}
}
