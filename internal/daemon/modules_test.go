package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/blackboard"
	"github.com/ANetResearch/ANet/module/inv1"
	"github.com/ANetResearch/ANet/module/inv2"
	"github.com/ANetResearch/ANet/module/org"
)

// CC-6: providers.anetlink is gone. The daemon has no typed field for it,
// so nothing reads it into a module block, and a config.json that still
// has it is refused at start as a removed key (modules.anetlink is the
// block); modules.anetlink reaches the module as it is.
func TestTheProvidersKeyIsGone(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for i := range typ.NumField() {
		if name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ","); name == "providers" {
			t.Fatalf("Config still has the field %s for providers", typ.Field(i).Name)
		}
	}
	found, err := FindRemovedSettings([]byte(`{"providers":{"anetlink":{"socket":"/run/link.sock"}}}`))
	if err != nil || len(found) != 1 || found[0].Key != "providers" {
		t.Fatalf("providers is not refused as a removed key: %+v %v", found, err)
	}
	raw, err := moduleConfig(Config{Modules: ModulesConfig{"anetlink": json.RawMessage(`{"socket":"/run/link.sock"}`)}})
	if err != nil || string(raw["anetlink"]) != `{"socket":"/run/link.sock"}` {
		t.Fatalf("modules.anetlink must reach the module as its block: %s %v", raw["anetlink"], err)
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

// The other half of INV-1, at the boundary that actually exists here.
//
// GuardCommonsPublish had no call site anywhere in this repository while
// its own documentation said every publish boundary calls it. The
// boundaries it was written for — a gossip announce, the commons boards
// — belong to the previous generation and were not carried over, so the
// invariant held vacuously and the guard watched nothing.
//
// What a node registers IS read by parties it never chose: the hub syncs
// it to federation peers and serves it from a browsable directory. That
// is the commons in anet4, and this is the check that it is guarded.
func TestAnOrgObjectCannotBePublishedToTheHub(t *testing.T) {
	// No confidential module: INV-1 must not be conditional on running
	// one. The screening used to return early when nothing declared a
	// secret, which would have skipped this entirely.
	d := &Daemon{}
	cred := org.Credential{OrgID: "org-1", Subject: "did:anet:x", Role: org.RoleMember}
	if err := d.screenPublication("this node's registration", map[string]any{
		"aid": "did:anet:me", "name": "node", "card": cred,
	}); !errors.Is(err, inv1.ErrOrgScopedOnCommons) {
		t.Errorf("a membership credential reached the hub: %v", err)
	}
	// A CogUnit is org-scoped too: what a node is thinking about is not
	// something the directory gets to hold.
	if err := d.screenPublication("this node's registration", map[string]any{
		"aid": "did:anet:me", "notes": []any{&blackboard.CogUnit{TaskID: "t"}},
	}); !errors.Is(err, inv1.ErrOrgScopedOnCommons) {
		t.Errorf("a CogUnit reached the hub: %v", err)
	}
	// And an ordinary registration still goes out.
	if err := d.screenPublication("this node's registration", map[string]any{
		"aid": "did:anet:me", "name": "node", "caps": []any{"text.stats"},
	}); err != nil {
		t.Errorf("an ordinary registration was refused: %v", err)
	}
}
