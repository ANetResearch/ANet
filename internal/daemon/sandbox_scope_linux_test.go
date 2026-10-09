//go:build linux

package daemon

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// stubSystemd answers systemctl --user is-system-running with state, counting the questions, and
// finds systemctl and systemd-run unless missing names one of them.
func stubSystemd(t *testing.T, state, missing string) *int {
	t.Helper()
	asked := 0
	oldLook, oldState := systemdLookPath, userSystemdState
	systemdLookPath = func(name string) (string, error) {
		if name == missing {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + name, nil
	}
	userSystemdState = func(string) string { asked++; return state }
	userSystemdCache.Lock()
	userSystemdCache.at = time.Time{}
	userSystemdCache.Unlock()
	t.Cleanup(func() {
		systemdLookPath, userSystemdState = oldLook, oldState
		userSystemdCache.Lock()
		userSystemdCache.at = time.Time{}
		userSystemdCache.Unlock()
	})
	return &asked
}

// SB-7: with the user's systemd running or degraded, each sandboxed run starts in a user scope with
// the memory and task limits — systemd-run around bwrap, with only the manager's runtime directory and
// bus in its environment, and still no agent argument or key on any command line — and limits is
// "full"; otherwise bwrap runs alone and limits is "no_cgroup" (a warning, not a refusal: core N4).
// The answer is kept for five minutes.
func TestRunsGetAUserScopeWhenTheUserSystemdRuns(t *testing.T) {
	for _, tc := range []struct {
		state, missing string
		scope          bool
	}{
		{"running", "", true}, {"degraded", "", true}, {"starting", "", false}, {"offline", "", false},
		{"", "", false}, {"running", "systemd-run", false}, {"running", "systemctl", false},
	} {
		asked := stubSystemd(t, tc.state, tc.missing)
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/4242")
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
		run := sandboxSystemdRun()
		if (run == "/usr/bin/systemd-run") != tc.scope {
			t.Fatalf("%q (missing %q): systemd-run %q", tc.state, tc.missing, run)
		}
		if want := map[bool]string{true: "full", false: "no_cgroup"}[tc.scope]; sandboxLimits() != want {
			t.Fatalf("%q: limits %q, want %q", tc.state, sandboxLimits(), want)
		}
		if tc.missing == "" && *asked != 1 {
			t.Fatalf("%q: the user's systemd was asked %d times within five minutes", tc.state, *asked)
		}

		agent := writeStub(t, "#!/bin/sh\necho hi\n")
		plan := &sandboxPlan{Bwrap: "/usr/bin/bwrap", AID: "EaidSystemdScope0001", Home: filepath.Join(t.TempDir(), "home"),
			SystemdRun: run}
		const key = "CANARY-KEY-5e1d"
		l, err := plan.command(t.TempDir(), []string{agent, "the peer's words"}, []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=" + key}, nil)
		if err != nil {
			t.Fatal(err)
		}
		line := strings.Join(append([]string{l.Path}, l.Args...), " ")
		if strings.Contains(line, key) || strings.Contains(line, "the peer's words") || strings.Contains(line, agent) {
			t.Errorf("the command line carries the agent's command or key: %s", line)
		}
		if tc.scope {
			want := []string{"--user", "--scope", "--quiet", "-p", "MemoryMax=2G", "-p", "TasksMax=256", "--", "/usr/bin/bwrap"}
			if l.Path != run || !slices.Equal(l.Args[:len(want)], want) {
				t.Errorf("%q: %s", tc.state, line)
			}
			if !slices.Equal(l.Env, []string{"XDG_RUNTIME_DIR=/run/user/4242"}) {
				t.Errorf("systemd-run's environment: %v", l.Env)
			}
		} else if l.Path != "/usr/bin/bwrap" || len(l.Env) != 0 {
			t.Errorf("%q: %s with environment %v", tc.state, line, l.Env)
		}
		l.Close()
	}
}

// The wrapper works on this host when its user systemd runs: systemd-run with the limits and only
// the environment the runs give it starts a program in a user scope. Skipped where the user has no
// systemd manager (containers, CI runners without a session).
func TestTheUserScopeOnThisHost(t *testing.T) {
	userSystemdCache.Lock()
	userSystemdCache.at = time.Time{}
	userSystemdCache.Unlock()
	run := sandboxSystemdRun()
	if run == "" {
		t.Skip("no user systemd manager running here")
	}
	cmd := exec.Command(run, append(append([]string{}, sandboxSystemdRunArgs...), "/bin/sh", "-c", "cat /proc/self/cgroup")...)
	cmd.Env = systemdEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), ".scope") {
		t.Fatalf("the program did not run in a scope: %s", out)
	}
}
