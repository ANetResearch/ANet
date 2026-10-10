package daemon

// P-PAY-10 (HPP §7.2): charge is a total function of the a2a-x402
// declaration of the card it is read from; a list always reads the public
// card; /agents/card reads the extended card when one was fetched and
// verified; a card that is not VERIFIED has no charge.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/identity/identitytest"

	"github.com/ANetResearch/ANet/internal/netcard"
	"github.com/ANetResearch/ANet/module"
)

func TestChargeIsTheDeclaration(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range []struct {
		declared, required bool
		want               string
	}{
		{false, false, module.ChargeFree},
		{false, true, module.ChargeFree}, // no declaration says nothing of required
		{true, false, module.ChargePartial},
		{true, true, module.ChargePaid},
	} {
		if got := chargeOfDeclaration(c.declared, c.required); got != c.want {
			t.Errorf("declared %t required %t: %s, want %s", c.declared, c.required, got, c.want)
		}
		if !c.declared && c.required {
			continue
		}
		for _, extended := range []bool{true, false} {
			card := cardWithDeclaration(t, extended, c.declared, c.required)
			if got := chargeOf(card); got != c.want {
				t.Errorf("a card declaring %t/%t (extended %t): %s, want %s", c.declared, c.required, extended, got, c.want)
			}
		}
		seen[c.want] = true
	}
	if len(seen) != 3 {
		t.Fatalf("three declarations, %d charges", len(seen))
	}
	// Other extensions say nothing about it.
	card := cardWithDeclaration(t, true, false, false)
	more := bytes.Replace(card, []byte(`"extensions":[`), []byte(`"extensions":[{"uri":"https://example.org/x","required":true},`), 1)
	if !bytes.Equal(more, card) && chargeOf(more) != module.ChargeFree {
		t.Error("another required extension made a free card charge")
	}
	// A card that declares a2a-x402 twice has no charge.
	if chargeOf([]byte(`{"capabilities":{"extensions":[{"uri":"`+module.ExtX402URI+`"},{"uri":"`+module.ExtX402URI+`"}]}}`)) != "" {
		t.Error("a card that declares a2a-x402 twice has a charge")
	}
}

// The three places a charge comes from, on one agent whose public card
// charges nothing and whose list variant charges for one skill: the list
// says free, the card shown to a listed caller says some skills paid, and
// an agent whose card does not verify has no charge at all.
func TestChargeFollowsTheCardShown(t *testing.T) {
	srv := newFakeHub(t)
	withRegistry(t, srv.URL)
	prov := cardLabProvider(t, srv, "Echo", []string{"echo"}, []string{"y"}, map[string]uint64{"y": 2})
	req := cardLabRequester(t, srv, "Friend")
	allowPeers(t, prov, req.AID())
	heldCapFrom(t, prov, req)
	if e := listRouteEntry(t, req, prov.AID()); e.Charge != module.ChargeFree {
		t.Fatalf("the list: %+v", e)
	}
	if ra := cardRoute(t, req, prov.AID(), prov); ra.Charge != module.ChargePartial {
		t.Fatalf("the card: %+v", ra)
	}
	// The list still reads the public card once the extended card is kept.
	if e := listRouteEntry(t, req, prov.AID()); e.Charge != module.ChargeFree {
		t.Fatalf("the list after the card: %+v", e)
	}
}

// A card that does not verify is shown without its bytes, its name or a
// charge: what it says could be anyone's words (an internal note item).
func TestAnUnverifiedCardHasNoCharge(t *testing.T) {
	ctl, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	other, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	unsigned, err := netcard.Build(netcard.Input{AID: ctl.AID(), Name: "x", HubURL: "https://hub.example", Seq: 1, IssuedAtMs: 1,
		NotBeforeMs: 1, Skills: []netcard.Skill{{ID: "a", Name: "a", Description: "a"}},
		Extensions: []map[string]any{a2acard.ExtensionDecl(module.ExtX402URI, "x402", true, nil)}})
	if err != nil {
		t.Fatal(err)
	}
	card, err := a2acard.SignWithSnapshot(unsigned, ctl, "")
	if err != nil {
		t.Fatal(err)
	}
	d := newCardDaemon(t, "Req", "")
	ra := module.RemoteAgent{AID: ctl.AID(), Card: card}
	// Checked against another agent's key history: it does not verify.
	if v := d.verifyCardInto(&ra, singleKEL(ctl.AID(), other.KEL())); v != nil || ra.Verification != cardUnverified {
		t.Fatalf("%+v", ra)
	}
	b, _ := json.Marshal(ra)
	if bytes.Contains(b, []byte(`"charge"`)) || bytes.Contains(b, []byte(`"prices"`)) {
		t.Fatalf("an unverified entry: %s", b)
	}
	// And a list entry of the same card from a hub's registry.
	srv := newFakeHub(t)
	req := cardLabRequester(t, srv, "Req")
	w := httptest.NewRecorder()
	req.hAgentsCard(w, httptest.NewRequest(http.MethodPost, "/agents/card", bytes.NewReader([]byte(`{"aid":"`+ctl.AID()+`"}`))))
	if bytes.Contains(w.Body.Bytes(), []byte(`"charge"`)) {
		t.Fatalf("an agent no hub knows: %s", w.Body)
	}
}
