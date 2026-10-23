//go:build !linux && !darwin

package autoreply

import (
	"context"
	"os"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// RunnerOptions configures RunnerServe (runner_serve.go); on this platform there is no runner.
type RunnerOptions struct {
	Socket, AllowUser, SocketGroup string
	SocketMode                     os.FileMode
	Agents                         map[string]string
	MaxJobs                        int
	Workspace                      string
	KeepDays                       int
	Root                           string
	Ready                          func(socket string)
}

// The values of RunnerOptions.Workspace.
const (
	RunnerWorkspacePerPeer = "per-peer"
	RunnerWorkspacePerRun  = "per-run"
)

// RunnerServe is runner_unsupported here: the runner checks every connection with the kernel's peer
// credentials, which only Linux and macOS give it (design v0.3.9 §C).
func RunnerServe(context.Context, RunnerOptions) error {
	return a2ashape.Refuse("runner_unsupported", nil)
}
