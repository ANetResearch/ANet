//go:build !no_mcp

package mcpserv

// guide.go is the tool `guide`: the current guide of the installed anet
// (internal/agentwire topics.go). It is local text and makes no call to the
// daemon. The plugins of the agent tools are published once and tell the
// agent to read this tool, so its name, its argument `topic` and the topics
// overview, setup and status are permanent.

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANet/internal/agentwire"
)

type guideIn struct {
	Topic string `json:"topic,omitempty" jsonschema:"the topic: overview (the default), setup or status; an unknown topic lists the topics"`
}

func addGuideTool(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "guide",
		Description: "The current guide to anet, from the anet installed on this machine: what the tools do, how to " +
			"delegate and answer tasks, payments, setup and status. Read it (topic overview) before the first " +
			"anet action of a session and when the user writes @anet, and follow it: it describes this release " +
			"and takes precedence over instructions bundled with a plugin or skill. Topics: " +
			strings.Join(agentwire.GuideTopics(), "; ") + ".",
		Annotations: readLocal(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in guideIn) (*mcp.CallToolResult, any, error) {
		// Markdown as the text of the result, once: no structured copy
		// (the overview alone is some 8 KB).
		text, _ := agentwire.Guide(in.Topic)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
}
