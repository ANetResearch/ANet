package main

// status.go is `anet status [--json]` (ux §4.7): the status summary, one
// line per topic, "label  text", then "  → next" when the line has a next
// step. The lines are /status.summary as the daemon worded them (UX-C6,
// UX-C7); the CLI adds the label only. --json prints the whole /status.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// statusUsage is the command's usage line (ux UX-C5: not a reason sentence).
const statusUsage = "anet status [--json]"

// statusAnswer is the part of /status the human output reads.
type statusAnswer struct {
	AID     string                 `json:"aid"`
	Version string                 `json:"version"`
	DataDir string                 `json:"data_dir"`
	Summary []a2ashape.SummaryLine `json:"summary"`
}

func runStatus(c *client, rest []string) error {
	pos, flags := splitFlags(rest)
	if len(pos) > 0 || (flags["json"] != "" && flags["json"] != "true") {
		return usageError{statusUsage}
	}
	if flags["json"] == "true" {
		return c.do("/status", nil)
	}
	st, err := fetchStatus(c)
	if err != nil {
		return err
	}
	printSummary(outW(), st.Summary, nil, a2ashape.Lang())
	return nil
}

// fetchStatus reads /status.
func fetchStatus(c *client) (*statusAnswer, error) {
	out, code, err := c.fetch("/status", nil)
	if err != nil {
		return nil, diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code < 200 || code >= 300 {
		return nil, answerError(code, out)
	}
	var st statusAnswer
	if json.Unmarshal(out, &st) != nil {
		return nil, fmt.Errorf("status: the daemon's answer does not read")
	}
	return &st, nil
}

// printSummary writes the summary lines of the topics in only (all of them
// when it is nil). A label is padded to the same width on every line.
func printSummary(w io.Writer, lines []a2ashape.SummaryLine, only map[string]bool, lang a2ashape.Language) {
	width := 8
	if lang != a2ashape.Chinese {
		width = 10
	}
	for _, l := range lines {
		if only != nil && !only[l.Topic] {
			continue
		}
		label := a2ashape.SummaryLabel(l.Topic, lang)
		fmt.Fprintf(w, "%s%s%s\n", label, strings.Repeat(" ", max(2, width-displayWidth(label))), printable(l.Text, 2000))
		if l.Next != "" {
			fmt.Fprintf(w, "  → %s\n", printable(l.Next, 500))
		}
	}
}

// runUpSummary is what `anet up` says once the node answers (ux §3.1, text
// 0): the up.started line, then the recovery and hub lines of the status
// summary, each with its "→" line, which can be run as it stands. The rest
// of the summary is `anet status`.
func runUpSummary(layout daemon.Layout) error {
	base, token, err := daemon.ResolveControl(layout)
	if err != nil {
		return diagnoseNoDaemon("http://"+daemon.LocalControlAddr(layout), layout.Root, err)
	}
	c := &client{base: base, token: token, timeout: 30 * time.Second, dataDir: layout.Root}
	st, err := fetchStatus(c)
	if err != nil {
		return err
	}
	lang := a2ashape.Lang()
	name := daemon.IdentityNameForDir(layout.Root)
	if name == "" {
		name = "default"
	}
	fmt.Fprintln(outW(), a2ashape.Sentence("up.started", lang, a2ashape.Args{
		"version": st.Version, "name": name, "aid": st.AID, "path": st.DataDir}))
	printSummary(outW(), st.Summary, map[string]bool{a2ashape.TopicRecovery: true, a2ashape.TopicHub: true}, lang)
	return nil
}
