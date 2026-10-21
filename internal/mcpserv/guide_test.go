//go:build !no_mcp

package mcpserv

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The published plugins read the tool guide with the topics overview, setup
// and status; each answers with its text, as plain text and without a
// structured copy, and an unknown topic lists the topics.
func TestGuideToolTopics(t *testing.T) {
	sess := connect(t, &fakeControl{})
	for topic, want := range map[string]string{
		"":         "# AgentNetwork (anet)",
		"overview": "# AgentNetwork (anet)",
		"Setup":    "# Setting up anet",
		"status":   "# Reporting the state of the node",
		"nosuch":   "There is no guide topic \"nosuch\"",
	} {
		args := map[string]any{}
		if topic != "" {
			args["topic"] = topic
		}
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "guide", Arguments: args})
		text, isErr := resultText(t, res, err)
		if isErr || !strings.HasPrefix(text, want) {
			t.Errorf("guide %q: error %v, text begins %q; want %q", topic, isErr, text[:min(len(text), 60)], want)
		}
		if res != nil && res.StructuredContent != nil {
			t.Errorf("guide %q carries a structured copy", topic)
		}
		if topic != "nosuch" && !strings.Contains(text, "takes precedence over instructions bundled with a plugin") {
			t.Errorf("guide %q lacks the footer", topic)
		}
	}
}
