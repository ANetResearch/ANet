package release

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// installFuncs are install.sh's functions named, as the script defines
// them, for a test that runs a part of it.
func installFuncs(t *testing.T, names ...string) string {
	t.Helper()
	script := repoFile(t, "deploy/release/install.sh")
	var funcs []string
	for _, name := range names {
		re := regexp.MustCompile(`(?ms)^` + name + `\(\)\s*\{(?:[^\n]*\}\n|[^\n]*\n.*?^\}\n)`)
		f := re.FindString(script)
		if f == "" {
			t.Fatalf("install.sh has no function %s", name)
		}
		funcs = append(funcs, f)
	}
	return strings.Join(funcs, "\n")
}

// runInstallPart runs install.sh's parse_args with args and then body,
// against a stand-in anet that prints anetOut and exits anetExit, and
// returns stdout, stderr, the exit status and the stand-in's calls.
func runInstallPart(t *testing.T, env []string, args, body, anetOut string, anetExit int) (string, string, int, string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	prefix := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	fake := "#!/bin/sh\nprintf 'ARGV %s\\n' \"$*\" >> '" + log + "'\n" +
		"case \"$1\" in hub-register) printf '%s' '" + strings.ReplaceAll(anetOut, "'", `'\''`) + "'; exit " +
		string(rune('0'+anetExit)) + " ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(prefix, "anet"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "set -eu\n" + installFuncs(t, "say", "warn", "die", "usage", "removed_flag", "parse_args", "join_hub") +
		"\nparse_args --prefix '" + prefix + "' " + args + "\n" + body + "\n"
	cmd := exec.Command(sh, "-c", script)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	return out.String(), errb.String(), code, string(calls)
}

// LEC AV-5, §4.10: install.sh has no invitation any more. --token and
// --token-file stop it before anything runs, with exit status 2 and anet's
// cli_removed sentence word for word, in the language anet would choose.
func TestInstallShRefusesTheRemovedInviteFlags(t *testing.T) {
	for _, lang := range []struct {
		env  string
		lang a2ashape.Language
	}{{"ANET_LANG=en", a2ashape.English}, {"ANET_LANG=zh_CN.UTF-8", a2ashape.Chinese}, {"ANET_LANG=ZH", a2ashape.Chinese}, {"LC_ALL=zh_CN.UTF-8", a2ashape.English}} {
		for _, c := range []struct{ args, flag string }{
			{"--hub https://h --token x", "--token"},
			{"--token=x", "--token"},
			{"--hub https://h --token-file f", "--token-file"},
			{"--token-file=f --hub https://h", "--token-file"},
		} {
			_, stderr, code, calls := runInstallPart(t, []string{lang.env}, c.args, "join_hub", "", 0)
			want := a2ashape.Sentence("cli_removed", lang.lang, a2ashape.Args{"what": "install.sh " + c.flag, "instead": "hub_register_token"}) + "\n"
			if code != 2 || stderr != want || calls != "" {
				t.Errorf("%s %s: exit %d, stderr %q, calls %q; want 2 and %q", lang.env, c.args, code, stderr, calls, want)
			}
		}
	}
	script := repoFile(t, "deploy/release/install.sh")
	for _, gone := range []string{"ANET_INVITE", "anetinv_", "invite_file", "INVITE"} {
		if strings.Contains(script, gone) {
			t.Errorf("install.sh still mentions %s", gone)
		}
	}
}

// LEC §4.10: with --hub, install.sh runs hub-register last and prints its
// lines as they are — the admit line last when the hub has to admit the
// AID — adding none of its own after them, and exits with its status.
func TestInstallShPassesHubRegisterThrough(t *testing.T) {
	const aid = "bafyreiujzdegxdncf32epf3dhodzdocis2jhtlgmxgedn73u557hq2abcd"
	lines := a2ashape.Sentence("identity.unprotected", a2ashape.English, nil) + "\n  → anet identity protect\n" +
		a2ashape.Sentence("hub_pending_admission", a2ashape.English, a2ashape.Args{"hub": "https://h", "aid": aid}) +
		"\n  → anet-hub admit " + aid + "\n"
	for _, exit := range []int{0, 1} {
		stdout, _, code, calls := runInstallPart(t, []string{"ANET_LANG=en", "ANET_INVITE=anetinv_SECRET"}, "--hub https://h --name n", "join_hub", lines, exit)
		if code != exit {
			t.Errorf("exit %d, want hub-register's %d", code, exit)
		}
		if !strings.HasSuffix(stdout, lines) {
			t.Errorf("stdout does not end with hub-register's lines:\n%s", stdout)
		}
		if !strings.Contains(calls, "ARGV hub-register https://h --name n") || strings.Contains(calls, "SECRET") {
			t.Errorf("calls %q", calls)
		}
	}
	// The order of main: join_hub is the last step.
	script := repoFile(t, "deploy/release/install.sh")
	main := regexp.MustCompile(`(?ms)^main\(\)\s*\{.*?^\}\n`).FindString(script)
	steps := regexp.MustCompile(`(?m)^\s+([a-z_]+)$`).FindAllStringSubmatch(main, -1)
	if len(steps) == 0 || steps[len(steps)-1][1] != "join_hub" {
		t.Errorf("join_hub is not install.sh's last step: %v", steps)
	}
}
