package daemon

import (
	"encoding/json"
	"strings"
	"testing"
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
