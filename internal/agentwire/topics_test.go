//go:build !no_mcp

package agentwire

import (
	"strings"
	"testing"
)

// The topics the published plugins name are permanent; the overview is the
// skill wire installs, without its front matter.
func TestGuidePermanentTopics(t *testing.T) {
	for _, topic := range []string{"overview", "setup", "status"} {
		text, ok := Guide(topic)
		if !ok || text == "" {
			t.Fatalf("topic %s is missing", topic)
		}
	}
	text, _ := Guide("")
	if !strings.HasPrefix(text, "# AgentNetwork (anet)\n") || strings.Contains(text, "description: ") {
		t.Errorf("overview begins %q", text[:80])
	}
	if !strings.Contains(skillMarkdown, strings.TrimSuffix(strings.SplitN(text, "\n---\n", 2)[0], "\n")) {
		t.Error("the overview is not the text of the skill")
	}
	if _, ok := Guide("nosuch"); ok {
		t.Error("an unknown topic was found")
	}
}
