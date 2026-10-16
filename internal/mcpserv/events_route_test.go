//go:build !no_mcp

package mcpserv

import "testing"

// CUX §4.8: MCP does not expose previews, so /tasks/events is not a route
// it may call.
func TestTasksEventsIsNotAnMCPRoute(t *testing.T) {
	if _, ok := allowedPaths["/tasks/events"]; ok {
		t.Fatal("MCP may call /tasks/events")
	}
}
