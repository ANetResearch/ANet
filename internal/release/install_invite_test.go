package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// F41 (red team, supply): install.sh took the hub invite as `--token INVITE` and passed it to
// `anet hub-register --token`, so it sat in the argv of the installer's sh for the whole install and in
// anet's afterwards, where any local user reads it from /proc/<pid>/cmdline and registers with it first.
// Run install.sh's own argument parsing and hub join against a stand-in anet that records its argv and
// environment: the invite must reach hub-register through its environment only, reach no other process,
// and a --token on the command line must stop the install.
func TestInstallShKeepsTheInviteOffCommandLines(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	script := repoFile(t, "deploy/release/install.sh")
	var funcs []string
	for _, name := range []string{"say", "warn", "die", "usage", "parse_args", "join_hub"} {
		re := regexp.MustCompile(`(?ms)^` + name + `\(\)\s*\{(?:[^\n]*\}\n|[^\n]*\n.*?^\}\n)`)
		f := re.FindString(script)
		if f == "" {
			t.Fatalf("install.sh has no function %s", name)
		}
		funcs = append(funcs, f)
	}
	const secret = "anetinv_SECRET_F41"
	run := func(t *testing.T, env []string, args string) (string, string, error) {
		t.Helper()
		prefix := t.TempDir()
		log := filepath.Join(t.TempDir(), "calls")
		fake := "#!/bin/sh\nprintf 'ARGV %s | ENV %s\\n' \"$*\" \"${ANET_INVITE:-none}\" >> '" + log + "'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(prefix, "anet"), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "set -eu\n" + strings.Join(funcs, "\n") +
			"\nparse_args --prefix '" + prefix + "' " + args +
			"\nsh -c 'printf \"child sees %s\\n\" \"${ANET_INVITE:-none}\"' >> '" + log + "'\njoin_hub\n"
		cmd := exec.Command(sh, "-c", body)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		calls, _ := os.ReadFile(log)
		return string(out), string(calls), err
	}

	t.Run("on the command line: refused before anything runs", func(t *testing.T) {
		for _, a := range []string{"--hub https://h --name n --token " + secret, "--hub https://h --token=" + secret} {
			out, calls, err := run(t, nil, a)
			if err == nil || !strings.Contains(out, "ANET_INVITE") {
				t.Fatalf("%s: err %v\n%s", a, err, out)
			}
			if calls != "" {
				t.Fatalf("%s: ran %q", a, calls)
			}
		}
	})

	check := func(t *testing.T, calls string) {
		t.Helper()
		var reg int
		for _, line := range strings.Split(strings.TrimSpace(calls), "\n") {
			argv, env, _ := strings.Cut(line, " | ")
			if strings.Contains(argv, secret) {
				t.Fatalf("the invite is on a command line: %q", line)
			}
			switch {
			case strings.HasPrefix(line, "child sees"):
				if strings.Contains(line, secret) {
					t.Fatalf("a child of the installer inherited the invite: %q", line)
				}
			case strings.Contains(argv, "hub-register"):
				reg++
				if env != "ENV "+secret {
					t.Fatalf("hub-register did not get the invite in its environment: %q", line)
				}
			default:
				if strings.Contains(env, secret) {
					t.Fatalf("%q got the invite", line)
				}
			}
		}
		if reg != 1 {
			t.Fatalf("hub-register ran %d times:\n%s", reg, calls)
		}
	}
	t.Run("from ANET_INVITE", func(t *testing.T) {
		out, calls, err := run(t, []string{"ANET_INVITE=" + secret}, "--hub https://h --name n")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		check(t, calls)
	})
	t.Run("from --token-file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "invite")
		if err := os.WriteFile(f, []byte(secret+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, calls, err := run(t, nil, "--hub https://h --name n --token-file '"+f+"'")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		check(t, calls)
	})
}
