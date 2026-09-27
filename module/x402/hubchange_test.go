//go:build !no_x402

package x402

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
)

// hubSwitchingHost is a testHost whose hub can be changed, as hub-register
// to another hub changes it: HubURL and HubIdentity answer for the hub the
// node is on now, and identities counts the lookups.
type hubSwitchingHost struct {
	*testHost
	identities int
}

func (h *hubSwitchingHost) PaymentSeam() (module.PaymentSeam, bool) { return h, true }
func (h *hubSwitchingHost) HubIdentity() (string, []identity.SignedEvent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.identities++
	return h.hub.AID(), h.hub.KEL(), true
}

func (h *hubSwitchingHost) moveTo(url string) *identity.Controller {
	next, err := identity.Incept()
	if err != nil {
		panic(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.url, h.hub = url, next
	return next
}

// The hub's identity is learned once per hub URL: asked again it is not
// fetched again, and after the node moves to another hub the next payment
// is signed to that hub — not to the one it left, which a cache kept for
// the life of the process went on naming. A redemption confirmed for the
// old hub is refused before anything is signed; one confirmed for the new
// hub goes to the spending policy as a payment to it.
func TestTheHubIdentityFollowsTheHub(t *testing.T) {
	h := &hubSwitchingHost{testHost: newHost(t)}
	h.url = "http://hub-one.invalid"
	mod, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	m := mod.(*Module)
	first := h.hub.AID()
	if got := m.hubAID(); got != first {
		t.Fatalf("hub AID %s, want %s", got, first)
	}
	if got := m.hubAID(); got != first || h.identities != 1 {
		t.Fatalf("asked again: %s after %d lookups, want one lookup", got, h.identities)
	}
	if kel, err := m.hubKEL(); err != nil || len(kel) == 0 {
		t.Fatalf("hub KEL: %v", err)
	}

	second := h.moveTo("http://hub-two.invalid/").AID()
	if got := m.hubAID(); got != second {
		t.Fatalf("after moving hubs the module names %s, want the new hub %s", got, second)
	}
	if got := m.HomeNetwork(); got == "" || strings.Contains(got, first) || !strings.Contains(got, second) {
		t.Fatalf("home network %q after moving hubs", got)
	}

	h.spendRefusal = errors.New("stop at the policy")
	_, err = m.Redeem(context.Background(), 5, "ref", first)
	if !errors.Is(err, module.ErrRedeemPayee) {
		t.Fatalf("a redemption confirmed for the old hub: %v, want ErrRedeemPayee", err)
	}
	if len(h.spends) != 0 || len(h.eventsOf(EvPaymentAuthorized)) != 0 {
		t.Fatalf("a redemption to the wrong hub reached the policy or was signed: %+v", h.spends)
	}
	if _, err := m.Redeem(context.Background(), 5, "ref", ""); !errors.Is(err, module.ErrRedeemPayee) {
		t.Fatalf("a redemption without a payee: %v", err)
	}
	if _, err := m.Redeem(context.Background(), 5, "ref", second); err == nil || errors.Is(err, module.ErrRedeemPayee) {
		t.Fatalf("a redemption confirmed for the current hub: %v, want the policy's refusal", err)
	}
	if len(h.spends) != 1 || h.spends[0].payTo != second || h.spends[0].purpose != module.PurposeRedeem {
		t.Fatalf("the policy was asked %+v, want one redemption to %s", h.spends, second)
	}
}
