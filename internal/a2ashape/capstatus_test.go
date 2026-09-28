package a2ashape_test

import (
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The effect status a capability task states is the one its deliverable
// says as written (docs/notes/0033, FuzzProject). Go decodes the
// deliverable ignoring the case of member names and takes the last match,
// so {"status":"FAILED","Status":"OK"} — FAILED to the client, to a2a-go,
// to anyone checking the receipt over those bytes — was stated OK. A
// deliverable that reads two ways states no effect of its own: the task
// falls back to what is known without it (UNVERIFIED once it has ended).
func TestADeliverableThatReadsTwoWaysDoesNotStateItsEffect(t *testing.T) {
	for name, result := range map[string]string{
		"two spellings":    `{"status":"FAILED","message":"did not run","Status":"OK"}`,
		"other case only":  `{"STATUS":"OK"}`,
		"message two ways": `{"status":"OK","message":"done","MESSAGE":"refund issued"}`,
	} {
		ix := &interactions.Interaction{ID: "ix_1", Role: interactions.RoleOutbound, PeerAID: peer,
			Goal: "invoke capability text.echo", State: interactions.StateCompleted, StateAt: now, StateSeq: 3,
			IsCapability: true, Result: []byte(result), ReceiptVerified: interactions.VerificationVerified}
		task := a2ashape.Project(a2ashape.Source{Interaction: ix}, a2ashape.Options{Artifacts: true})
		if es := task.Metadata[a2ashape.KeyEffectStatus]; es != "UNVERIFIED" {
			t.Errorf("%s: effect_status %v, want UNVERIFIED", name, es)
		}
	}
	ix := &interactions.Interaction{ID: "ix_2", Role: interactions.RoleOutbound, PeerAID: peer,
		Goal: "invoke capability text.echo", State: interactions.StateCompleted, StateAt: now, StateSeq: 3,
		IsCapability: true, Result: []byte(`{"status":"OK","message":"done","output":{"Status":"x"}}`)}
	if es := a2ashape.Project(a2ashape.Source{Interaction: ix}, a2ashape.Options{}).Metadata[a2ashape.KeyEffectStatus]; es != "OK" {
		t.Errorf("a deliverable that reads one way: effect_status %v, want OK", es)
	}
}
