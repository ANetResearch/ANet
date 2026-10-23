//go:build !no_autoreply

package main

// runner_cmd.go is `anet runner serve`, the runner of the auto-reply module's direct runs (design v0.3.9
// §C, module/autoreply runner_serve.go). It runs as the account the agent is to run as, needs no data
// directory and no identity, and talks to nothing but the node on its socket.

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module/autoreply"
)

const runnerUsage = "runner serve --socket PATH --allow-user NAME|UID [--socket-group NAME|GID] [--agent ID[=/abs/path]]... " +
	"[--max-jobs N] [--workspace per-peer|per-run] [--keep-days N]"

// runRunner is `anet runner <subcommand>`.
func runRunner(rest []string) error {
	if len(rest) == 0 || rest[0] != "serve" {
		return usageError{runnerUsage}
	}
	o, err := parseRunnerServe(rest[1:])
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := autoreply.RunnerServe(ctx, o); err != nil {
		var ref *a2ashape.Refusal
		if errors.As(err, &ref) {
			sayRefusal(os.Stderr, ref.Key, a2ashape.Lang(), ref.Args)
			return errExit1
		}
		return err
	}
	return nil
}

// parseRunnerServe reads the flags of `anet runner serve`. --agent may be given more than once; each one
// names an agent the runner runs, with its binary or, without one, the binary found on PATH.
func parseRunnerServe(args []string) (autoreply.RunnerOptions, error) {
	o := autoreply.RunnerOptions{Agents: map[string]string{}}
	bad := usageError{runnerUsage}
	for i := 0; i < len(args); i++ {
		a := args[i]
		key, val, inline := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !strings.HasPrefix(a, "--") {
			return o, bad
		}
		if !inline {
			if i+1 >= len(args) {
				return o, bad
			}
			i++
			val = args[i]
		}
		switch key {
		case "socket":
			o.Socket = val
		case "allow-user":
			o.AllowUser = val
		case "socket-group":
			o.SocketGroup = val
		case "agent":
			id, bin, _ := strings.Cut(val, "=")
			o.Agents[strings.TrimSpace(id)] = strings.TrimSpace(bin)
		case "max-jobs":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return o, bad
			}
			o.MaxJobs = n
		case "workspace":
			o.Workspace = val
		case "keep-days":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return o, bad
			}
			o.KeepDays = n
			if n == 0 {
				o.KeepDays = -1 // 0 on the command line: kept until removed by hand
			}
		default:
			return o, bad
		}
	}
	if o.Socket == "" || o.AllowUser == "" {
		return o, bad
	}
	return o, nil
}
