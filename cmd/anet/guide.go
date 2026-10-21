//go:build !no_mcp

package main

import (
	"fmt"
	"strings"

	"github.com/ANetResearch/ANet/internal/agentwire"
)

// runGuide is `anet guide [<topic>] [--list]`: the guide of this release
// for coding agents, the text of the MCP tool guide (internal/agentwire
// topics.go). It reads nothing and needs no daemon, so that a plugin can
// show the setup before the node runs. The command, its argument and the
// topics overview, setup and status are permanent: the published plugins
// call them. An unknown topic lists the topics and exits with status 2.
func runGuide(rest []string) error {
	var topic string
	for _, a := range rest {
		switch {
		case a == "--list":
			fmt.Fprintln(outW(), strings.Join(agentwire.GuideTopics(), "\n"))
			return nil
		case strings.HasPrefix(a, "-"):
			return usageError{"anet guide [<topic>] [--list]"}
		case topic != "":
			return usageError{"anet guide [<topic>] [--list]  (one topic)"}
		default:
			topic = a
		}
	}
	text, found := agentwire.Guide(topic)
	if !found {
		return absentError{strings.TrimRight(text, "\n")}
	}
	fmt.Fprint(outW(), text)
	return nil
}
