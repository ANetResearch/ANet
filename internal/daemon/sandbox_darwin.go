//go:build darwin

package daemon

// sandbox_darwin.go puts the Seatbelt sandbox (sandbox_seatbelt.go, LEC §4.3.8) in place on macOS, and
// keeps the agent in init's process group there (MS-5).

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// seatbeltExecPath is sandbox-exec (MS-1): by its absolute path, never looked up on PATH. Tests point it
// elsewhere to simulate a Mac without it.
var seatbeltExecPath = "/usr/bin/sandbox-exec"

// sandboxKind is the sandbox a run on this platform is in: the sandbox member of anet.autoreply.invoked
// (LEC §2.5).
const sandboxKind = "seatbelt"

// sandboxLimits is /status.auto_reply.sandbox.limits (LEC §2.3.3, MS-6): init's rlimits, no memory or
// process cap.
func sandboxLimits() string { return "rlimit_only" }

// prepare checks that sandbox-exec is there and that the probe passes on this Mac.
func (p *sandboxPlan) prepare() error { return seatbeltPrepare(p, seatbeltExecPath) }

// command prepares one agent process for the sandbox (seatbeltCommand).
func (p *sandboxPlan) command(workDir string, argv, env []string, stdin []byte) (*sandboxLaunch, error) {
	return seatbeltCommand(p, workDir, argv, env, stdin)
}

// usable: the macOS probe's spec (seatbeltSpecUsable).
func (s sandboxCheckSpec) usable() bool { return seatbeltSpecUsable(s) }

// runSandboxChecks runs the five checks of MS-4 inside the sandbox (seatbeltChecks).
func runSandboxChecks(spec sandboxCheckSpec) sandboxCheckReport { return seatbeltChecks(spec) }

// --- the agent's process group (MS-5) ---

// sandboxAgentSharesGroup: init starts the agent in its own process group, the one the daemon started
// sandbox-exec in, so that the daemon's kill of that group on a timeout reaches the agent and what it
// started. There is no PID namespace to end them otherwise.
const sandboxAgentSharesGroup = true

// forwardSandboxSignal passes a signal init received on to every other process of its group, or to the
// agent alone when init does not lead its group (started by hand or by a test, its group is someone
// else's).
func forwardSandboxSignal(agent int, sig syscall.Signal) {
	if !sandboxInitLeadsGroup() {
		_ = syscall.Kill(agent, sig)
		return
	}
	for _, pid := range sandboxGroupOthers() {
		_ = syscall.Kill(pid, sig)
	}
}

// endSandboxAgentGroup kills what the agent left in init's group once it has exited (MS-5: at the end of
// a run as on a timeout): background processes it started and did not wait for. Only a group init leads
// is touched.
func endSandboxAgentGroup() {
	if !sandboxInitLeadsGroup() {
		return
	}
	for i := 0; i < 50; i++ {
		others := sandboxGroupOthers()
		if len(others) == 0 {
			return
		}
		for _, pid := range others {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sandboxInitLeadsGroup reports whether this process leads its process group.
func sandboxInitLeadsGroup() bool { return syscall.Getpgrp() == os.Getpid() }

// darwinSZOMB is p_stat of a process that has exited and not been waited for (sys/proc.h).
const darwinSZOMB = 5

// sandboxGroupOthers are the live processes of this process's group other than itself.
func sandboxGroupOthers() []int {
	self := os.Getpid()
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", syscall.Getpgrp())
	if err != nil {
		return nil
	}
	var out []int
	for _, kp := range procs {
		pid := int(kp.Proc.P_pid)
		if pid != self && pid > 0 && kp.Proc.P_stat != darwinSZOMB {
			out = append(out, pid)
		}
	}
	return out
}

// underSystemDir reports a path the profile already lets the agent read and execute from as part of the
// system (seatbeltExecSystem, each also in seatbeltReadSystem), for pythonRuntime: a Python installed
// anywhere else — a framework build under /Library/Frameworks, Homebrew's, uv's or pyenv's, or one in a
// system directory the agent may only read — is listed with its prefix, which it reads its standard
// library from and where a framework build's python re-executes the interpreter inside its app bundle.
func underSystemDir(p string) bool {
	for _, d := range seatbeltExecSystem {
		if pathWithin(p, d) {
			return true
		}
	}
	return false
}
