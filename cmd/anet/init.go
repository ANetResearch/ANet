package main

// init.go holds `anet init` (A2A-DESIGN §13.1): write the safe defaults of
// SI-5 into this identity's data directory, explicitly, and say what was
// added and what was found different from a fresh install. It needs no
// daemon and never overwrites an existing value; see daemon.InitLayout.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/ANetResearch/ANet/internal/daemon"
)

func runInit(layout daemon.Layout, rest []string) error {
	_, flags := splitFlags(rest)
	return initTo(os.Stdout, layout, flags["json"] == "true")
}

func initTo(w io.Writer, layout daemon.Layout, asJSON bool) error {
	rep, err := daemon.InitLayout(layout)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
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
