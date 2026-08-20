package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/inv2"
)

// The typed provider config keeps working: an existing config file must not
// need rewriting because the wiring moved behind a seam.
func TestLegacyProviderConfigBecomesAModuleBlock(t *testing.T) {
	raw, err := moduleConfig(Config{
		Providers: &ProvidersConfig{ANetLink: &ANetLinkProviderConfig{Socket: "/run/link.sock"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, ok := raw["anetlink"]
	if !ok {
		t.Fatal("the anetlink provider config must reach the module as its block")
	}
	var got struct {
		Socket string `json:"socket"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Socket != "/run/link.sock" {
		t.Fatalf("socket = %q, want the configured one", got.Socket)
	}
}

// A module block for a subsystem that lands later needs no daemon field.
func TestUnknownModuleBlocksPassThrough(t *testing.T) {
	raw, err := moduleConfig(Config{
		Modules: ModulesConfig{"p2p": json.RawMessage(`{"listen":":4001"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw["p2p"]), "4001") {
		t.Fatalf("a future module's block must reach module.Build untouched: %s", raw["p2p"])
	}
}

// A node must not publish its org id.
//
// INV-1 keeps org data off the p2p fabric. This is its other half: which
// org a node belongs to is not public, and a node that publishes the id
// has disclosed its membership to everyone whether or not it published a
// credential. The realistic leak is not a field someone added on purpose
// — it is prose an agent wrote about itself.
func TestPublishingIsRefusedWhenItWouldLeakAnOrgID(t *testing.T) {
	d := &Daemon{modules: []module.Module{confidentialModule{"org-secret-token"}}}

	if err := d.screenPublication("profile", map[string]any{
		"summary": "I coordinate work for org-secret-token",
	}); !errors.Is(err, inv2.ErrLeak) {
		t.Errorf("the leak must be refused at the chokepoint, got %v", err)
	}
	if err := d.screenPublication("profile", map[string]any{
		"summary": "I translate documents",
	}); err != nil {
		t.Errorf("an ordinary profile must publish: %v", err)
	}
	// A node with no confidential module publishes freely; the guard must
	// not become a cost every node pays for a feature most do not run.
	plain := &Daemon{}
	if err := plain.screenPublication("profile", map[string]any{
		"summary": "org-secret-token",
	}); err != nil {
		t.Errorf("a node in no org must not be screened: %v", err)
	}
}

type confidentialModule struct{ token string }

func (confidentialModule) Name() string                             { return "fake" }
func (confidentialModule) Start(context.Context, module.Host) error { return nil }
func (confidentialModule) Stop(context.Context) error               { return nil }
func (m confidentialModule) ForbiddenTokens() []string              { return []string{m.token} }
