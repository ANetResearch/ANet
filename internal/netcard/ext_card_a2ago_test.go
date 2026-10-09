//go:build !no_a2a

package netcard

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"github.com/ANetResearch/ANetCore/identity"
)

// a2a-go verifies every card of G-PAY-EXT-1 with the key of the kid the
// vector names, reads the extended card flag, and refuses a card whose
// price was changed after signing. The cards are tests' imports only: the
// kernel links no A2A library (SI-8).
func TestExtCard1VerifiesInA2AGo(t *testing.T) {
	vec := loadExtCard1(t)
	kel := identity.SuiteAgentController().KEL()
	pub := ed25519.PublicKey(kel[len(kel)-1].Event.Keys[0])
	v := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: a2acrypto.KeyResolverFunc(
		func(_ context.Context, kid string) (crypto.PublicKey, error) {
			if kid != vec.Kid {
				return nil, errors.New("unknown kid " + kid)
			}
			return pub, nil
		})})
	n := 0
	for _, node := range vec.Nodes {
		for _, c := range node.Cards {
			var card a2a.AgentCard
			if err := json.Unmarshal([]byte(c.Card), &card); err != nil {
				t.Fatalf("%s/%s: a2a-go does not read the card: %v", node.Name, c.Variant, err)
			}
			if !card.Capabilities.ExtendedAgentCard || len(card.Signatures) != 1 {
				t.Fatalf("%s/%s: extendedAgentCard %v, %d signatures", node.Name, c.Variant,
					card.Capabilities.ExtendedAgentCard, len(card.Signatures))
			}
			if err := v.Verify(context.Background(), json.RawMessage(c.Card), &card.Signatures[0]); err != nil {
				t.Errorf("%s/%s: a2a-go: %v", node.Name, c.Variant, err)
				continue
			}
			n++
		}
	}
	if n != 5 {
		t.Fatalf("a2a-go verified %d cards, want 5", n)
	}
	// A price lowered after signing does not verify.
	for _, node := range vec.Nodes {
		for _, c := range node.Cards {
			if c.Variant != "list" {
				continue
			}
			var m map[string]any
			_ = json.Unmarshal([]byte(c.Card), &m)
			for _, e := range m["capabilities"].(map[string]any)["extensions"].([]any) {
				if params, ok := e.(map[string]any)["params"].(map[string]any); ok {
					if prices, ok := params["prices"].([]any); ok {
						prices[0].(map[string]any)["amount"] = "1"
					}
				}
			}
			tampered, _ := json.Marshal(m)
			var card a2a.AgentCard
			_ = json.Unmarshal(tampered, &card)
			if err := v.Verify(context.Background(), tampered, &card.Signatures[0]); err == nil {
				t.Errorf("%s/list: a2a-go verified a card whose price was changed", node.Name)
			}
		}
	}
}
