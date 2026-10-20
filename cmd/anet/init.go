package main

// init.go holds `anet init` (A2A-DESIGN §13.1): write the safe defaults of
// SI-5 into this identity's data directory, explicitly, and say what was
// added and what was found different from a fresh install. It needs no
// daemon and never overwrites an existing value; see daemon.InitLayout.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/module"
)

const initUsage = "anet init [--without <module>[,<module>...]] [--with <module>] [--shell-controller <aid> [--yes]] [--json]"

// shellInBinary reports whether this binary contains the shell module, asked of an Availability value
// with nothing turned off (MOD-1: the registry is read through the value); tests replace it.
var shellInBinary = func() bool { return module.Availability{}.Available("shell") }

func runInit(layout daemon.Layout, rest []string) error {
	pos, flags := splitFlags(rest)
	if len(pos) > 0 {
		return usageError{initUsage}
	}
	if v, ok := flags["shell-controller"]; ok {
		aid := strings.TrimSpace(v)
		if v == "true" || aid == "" {
			return usageError{initUsage}
		}
		if _, ok := flags["without"]; ok {
			return usageError{initUsage}
		}
		if _, ok := flags["with"]; ok {
			return usageError{initUsage}
		}
		yes, given := flags["yes"]
		if given && yes != "true" {
			return usageError{initUsage}
		}
		return initShellControllerTo(os.Stdout, layout, flags["json"] == "true", aid, given)
	}
	// --yes confirms the controller grant and nothing else.
	if _, ok := flags["yes"]; ok {
		return usageError{initUsage}
	}
	var without, with []string
	for _, f := range []struct {
		name string
		out  *[]string
	}{{"without", &without}, {"with", &with}} {
		v, ok := flags[f.name]
		if !ok {
			continue
		}
		names := splitNames(v)
		if v == "true" || len(names) == 0 {
			return usageError{initUsage}
		}
		*f.out = names
	}
	return initTo(os.Stdout, layout, flags["json"] == "true", without, with)
}

// splitNames splits a comma-separated module list, dropping empty items.
func splitNames(v string) []string {
	var out []string
	for _, n := range strings.Split(v, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// initRefusal turns the refusals of daemon.InitWithout and daemon.InitShellController into what the command
// prints: the sentence and its next step, exit status 2 for input that has
// to change and 1 for a running daemon (02-ux, module sentences).
func initRefusal(err error) error {
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) {
		return err
	}
	lang := a2ashape.Lang()
	if ref.Key == "daemon_running" {
		sayRefusal(os.Stderr, ref.Key, lang, ref.Args)
		return errExit1
	}
	text := a2ashape.Sentence(ref.Key, lang, ref.Args)
	if n := a2ashape.Next(ref.Key, lang, ref.Args); n != "" {
		text += "\n" + nextPrefix + n
	}
	return absentError{text}
}

func initTo(w io.Writer, layout daemon.Layout, asJSON bool, without, with []string) error {
	var rep daemon.InitReport
	var wc daemon.WithoutChange
	var err error
	if len(without) > 0 || len(with) > 0 {
		rep, wc, err = daemon.InitWithout(layout, without, with)
		err = initRefusal(err)
	} else {
		rep, err = daemon.InitLayout(layout)
	}
	if err != nil {
		return err
	}
	if asJSON {
		out := struct {
			daemon.InitReport
			Without *daemon.WithoutChange `json:"without,omitempty"`
		}{rep, nil}
		if len(without) > 0 || len(with) > 0 {
			out.Without = &wc
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Fprintf(w, "anet init — %s\n", rep.DataDir)
	switch {
	case rep.Created:
		fmt.Fprintf(w, "  created %s with the safe defaults written out\n", rep.ConfigPath)
	case rep.Wrote:
		fmt.Fprintf(w, "  added the missing keys to %s (existing values are never changed)\n", rep.ConfigPath)
	default:
		fmt.Fprintf(w, "  %s already has every key; not changed\n", rep.ConfigPath)
	}
	for _, c := range rep.Changes {
		switch c.Action {
		case "added":
			fmt.Fprintf(w, "  + %-34s %s\n", c.Key, compactJSON(c.Value))
		case "removed":
			fmt.Fprintf(w, "  - %-34s %s  (%s)\n", c.Key, compactJSON(c.Value), c.Note)
		case "created":
			if c.Key == "config.json" {
				fmt.Fprintf(w, "  + %-34s %s\n", c.Key, c.Note)
			} else {
				fmt.Fprintf(w, "  + %-34s empty file %s\n", c.Key, c.Note)
			}
		case "mode":
			fmt.Fprintf(w, "  ~ %-34s mode %s\n", c.Key, c.Note)
		case "kept":
			fmt.Fprintf(w, "  = %-34s exists, %v entries (%s)\n", c.Key, c.Value, c.Note)
		}
	}
	if len(rep.Kept) > 0 {
		fmt.Fprintln(w, "\nSettings that differ from a fresh install, left as they are (init does not reset them):")
		for _, k := range rep.Kept {
			fmt.Fprintf(w, "  ! %-34s %s   (%s)\n", k.Key, compactJSON(k.Value), k.Note)
		}
	}
	if len(rep.Problems) > 0 {
		fmt.Fprintln(w, "\nProblems (not fixed by init):")
		for _, p := range rep.Problems {
			fmt.Fprintf(w, "  ✗ %s\n", p)
		}
	}
	if len(without) > 0 || len(with) > 0 {
		switch {
		case len(wc.Added) == 0 && len(wc.Removed) == 0:
			fmt.Fprintf(w, "  without is already %s; not changed\n", listOrNone(wc.Without))
		default:
			fmt.Fprintf(w, "  without: %s\n", listOrNone(wc.Without))
		}
	}
	if localDaemonUp(layout) && rep.Wrote {
		fmt.Fprintln(w, "\nThe daemon is running; restart it to read the completed config: "+restartNodeCmds)
	}
	fmt.Fprintln(w, "\nNext: anet doctor")
	return nil
}

// shellControllerInterp is the interpreter written into the controller's modules.shell block: PowerShell
// on Windows, the module's default (/bin/sh) elsewhere.
func shellControllerInterp() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return ""
}

// initShellControllerTo is `anet init --shell-controller <aid>` (D9): it makes aid the one caller of the
// shell module and puts it on peers.allow, under one confirmation on the terminal, while no daemon runs
// (daemon.InitShellController). Exit status 2 for input that has to change (an AID that is not whole, a
// binary without the shell module, a configuration that conflicts); 1 for a running daemon, no terminal
// or a declined confirmation; 0 when written or already in place.
//
// With yes (`--yes`), the command line is the confirmation: nothing is asked and no terminal is opened,
// so the installer runs the grant without a console (install.ps1 -Controller, an agent's tool call, a
// CI job). It writes exactly what the confirmed grant writes, under the same refusals. It opens nothing
// that the caller could not open already: the grant is offline (no daemon may run) and writes two files
// of the data directory, which a process running as this user can write itself.
func initShellControllerTo(w io.Writer, layout daemon.Layout, asJSON bool, aid string, yes bool) error {
	lang := a2ashape.Lang()
	if !identity.ValidAID(aid) {
		args := a2ashape.Args{"value": printable(aid, 64)}
		if args["value"] == "" {
			args["value"] = `""`
		}
		text := a2ashape.Sentence("aid_invalid", lang, args)
		if n := a2ashape.Next("aid_invalid", lang, args); n != "" {
			text += "\n" + nextPrefix + n
		}
		return absentError{text}
	}
	if !shellInBinary() {
		return absentError{"this anet binary does not contain the shell module; install the shell build " +
			"(install.ps1 -Controller or -Shell on Windows, install.sh --shell elsewhere)"}
	}
	confirm := func(p daemon.ShellControllerPlan) error {
		if yes {
			return nil
		}
		return ttyConfirm("shell_controller", func() (string, error) {
			return a2ashape.Sentence("shell.controller_confirm", a2ashape.Lang(), a2ashape.Args{"peer": p.AID}), nil
		})
	}
	plan, err := daemon.InitShellController(layout, aid, shellControllerInterp(), confirm)
	if err != nil {
		return initRefusal(err)
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(plan)
	}
	fmt.Fprintf(w, "anet init --shell-controller — %s\n", layout.Root)
	if !plan.Changed {
		fmt.Fprintf(w, "  modules.shell and peers.allow already name %s; nothing changed\n", plan.AID)
	}
	if plan.ModuleBlock != nil {
		fmt.Fprintf(w, "  + modules.shell %s\n", plan.ModuleBlock)
	}
	if plan.AddAllow {
		fmt.Fprintf(w, "  + peers.allow %s\n", plan.AID)
	}
	if yes && plan.Changed {
		fmt.Fprintln(w, "  confirmed by --yes on the command line")
	}
	fmt.Fprintf(w, "\nOnly %s can run commands through the shell module. Every command and every refusal is recorded "+
		"on this node's evidence chain (anet audit). Start the node with: anet up\n", plan.AID)
	return nil
}

// compactJSON renders a value on one line for a report.
func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "(none)"
	}
	return strings.Join(l, ", ")
}
