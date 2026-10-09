//go:build unix && !darwin

package daemon

import "syscall"

// sandboxAgentSharesGroup: init starts the agent in a process group of its own and forwards SIGTERM and
// SIGINT to that group (LEC SB-5). On Linux nothing outlives the run anyway: bwrap's PID namespace ends
// with init. macOS has no such namespace and keeps the agent in init's group (sandbox_darwin.go).
const sandboxAgentSharesGroup = false

// forwardSandboxSignal passes a signal init received on to the agent's process group.
func forwardSandboxSignal(agent int, sig syscall.Signal) { _ = syscall.Kill(-agent, sig) }

// endSandboxAgentGroup: nothing to end here once the agent has exited.
func endSandboxAgentGroup() {}
