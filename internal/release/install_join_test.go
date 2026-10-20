package release

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// installFuncs are install.sh's functions named, as the script defines
// them, for a test that runs a part of it.
func installFuncs(t *testing.T, names ...string) string {
	t.Helper()
	script := repoFile(t, "deploy/release/install.sh")
	// The output helpers that say, warn and die call come with them.
	if slices.Contains(names, "say") || slices.Contains(names, "die") {
		for _, h := range []string{"show", "m", "tilde", "logto", "finish_log"} {
			if !slices.Contains(names, h) {
				names = append(names, h)
			}
		}
	}
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
	script := "set -eu\n" + installFuncs(t, "say", "warn", "die", "usage", "removed_flag", "parse_args") +
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
			_, stderr, code, calls := runInstallPart(t, []string{lang.env}, c.args, "true", "", 0)
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

// Spec B: by default the node is started and joins
// https://hub.agentnetwork.org.cn; --hub names another hub, --no-join keeps
// the start and skips the join, --no-start skips both. The join passes
// --passphrase-later and never a terminal (start_node), and the summary is
// the last step of main.
func TestInstallShJoinsByDefault(t *testing.T) {
	for _, c := range []struct{ args, want string }{
		{"", "https://hub.agentnetwork.org.cn 1 1 0"},
		{"--hub https://h.example", "https://h.example 1 1 1"},
		{"--hub=https://h.example --no-join", "https://h.example 1 0 1"},
		{"--no-start", "https://hub.agentnetwork.org.cn 0 0 0"},
		{"--no-join --no-start", "https://hub.agentnetwork.org.cn 0 0 0"},
	} {
		stdout, stderr, code, _ := runInstallPart(t, []string{"ANET_LANG=en"}, c.args, `echo "$HUB $START $JOIN $HUB_EXPLICIT"`, "", 0)
		if code != 0 || strings.TrimSpace(stdout) != c.want {
			t.Errorf("%q: exit %d, %q (stderr %q), want %q", c.args, code, stdout, stderr, c.want)
		}
	}
	script := repoFile(t, "deploy/release/install.sh")
	start := regexp.MustCompile(`(?ms)^start_node\(\)\s*\{.*?^\}\n`).FindString(script)
	if !strings.Contains(start, `hub-register "$HUB" --name "$NODE_NAME" --passphrase-later </dev/null`) {
		t.Errorf("start_node does not run hub-register with --passphrase-later and no input:\n%s", start)
	}
	main := regexp.MustCompile(`(?ms)^main\(\)\s*\{.*?^\}\n`).FindString(script)
	steps := regexp.MustCompile(`(?m)^\s+([a-z_]+)$`).FindAllStringSubmatch(main, -1)
	if len(steps) < 2 || steps[len(steps)-1][1] != "summary" || steps[len(steps)-2][1] != "start_node" {
		t.Errorf("install.sh does not end with start_node and summary: %v", steps)
	}
}
