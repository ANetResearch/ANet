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
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

const initUsage = "anet init [--without <module>[,<module>...]] [--with <module>] [--json]"

func runInit(layout daemon.Layout, rest []string) error {
	pos, flags := splitFlags(rest)
	if len(pos) > 0 {
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

// initRefusal turns the refusals of daemon.InitWithout into what the command
// prints: the sentence and its next step, exit status 2 for input that has
// to change and 1 for a running daemon (02-ux, module sentences).
func initRefusal(err error) error {
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) {
		return err
	}
	lang := a2ashape.Lang()
	if ref.Key == "daemon_running" {
		say(os.Stderr, a2ashape.Sentence(ref.Key, lang, ref.Args), a2ashape.Next(ref.Key, lang, ref.Args))
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
		fmt.Fprintln(w, "\nThe daemon is running; restart it to read the completed config: anet stop && anet up")
	}
	fmt.Fprintln(w, "\nNext: anet doctor")
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
