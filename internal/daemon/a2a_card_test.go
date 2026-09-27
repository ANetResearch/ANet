package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/module"
)

// newCardDaemon is a daemon with no hub in its config, so nothing
// registers in the background: each test registers explicitly with
// RegisterWithHub and builds cards with networkCard(hubURL).
func newCardDaemon(t *testing.T, name, summary string) *Daemon {
	t.Helper()
	root := t.TempDir()
	b, _ := json.Marshal(map[string]any{"control_addr": "127.0.0.1:0", "name": name, "summary": summary})
	if err := os.WriteFile(filepath.Join(root, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := New(NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// setPublic sets inbound.public_capabilities without the background
// republish SetPublicCapabilities starts.
func setPublic(d *Daemon, ids ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	in := d.cfg.inbound()
	in.PublicCapabilities = nil
	for _, id := range ids {
		in.PublicCapabilities = append(in.PublicCapabilities, PublicCapability{ID: id})
	}
	d.cfg.Inbound = &in
}

func lampNode(t *testing.T) *Daemon {
	t.Helper()
	d := newCardDaemon(t, "Lamp Agent", "Turns the lamp on and off.")
	if err := d.Providers().Register(context.Background(), &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	setPublic(d, lampCap)
	return d
}

func decodeCard(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("card is not JSON: %v", err)
	}
	return m
}

func verifyOwn(t *testing.T, d *Daemon, raw []byte) *a2acard.Verified {
	t.Helper()
	v, err := a2acard.Verify(raw, func(aid string) ([]identity.SignedEvent, error) {
		return d.self.KEL(), nil
	}, uint64(time.Now().UnixMilli()))
	if err != nil {
		t.Fatalf("a2acard.Verify refused the card: %v\n%s", err, raw)
	}
	return v
}

// assertPublishForm walks a card and fails on anything the publish form
// (A2A §8.4.1) leaves out: a default value ("", false, [] or {}) anywhere
// except streaming and pushNotifications, which are proto3 optional and
// set explicitly; and a JSON number anywhere, since every number on the
// card is a decimal string.
func assertPublishForm(t *testing.T, v any, path string) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 && path != "" {
			t.Errorf("%s: empty object", path)
		}
		for k, e := range x {
			p := path + "." + k
			if b, ok := e.(bool); ok && !b && p != ".capabilities.streaming" && p != ".capabilities.pushNotifications" {
				t.Errorf("%s: false is a default value and must be left out", p)
			}
			assertPublishForm(t, e, p)
		}
	case []any:
		if len(x) == 0 {
			t.Errorf("%s: empty array", path)
		}
		for i, e := range x {
			assertPublishForm(t, e, path+"["+strconv.Itoa(i)+"]")
		}
	case string:
		if x == "" {
			t.Errorf("%s: empty string", path)
		}
	case json.Number:
		t.Errorf("%s: number %s; numbers on the card are strings", path, x)
	}
}

// The card, field by field, with the values that vary from run to run
// (AID, seq, times, hub port, signature) replaced by placeholders. The
// skill is free, so the payment module (when compiled in) adds nothing:
// this is also the card of a -tags no_x402 build, with no payment URI.
func TestTheNetworkCardGolden(t *testing.T) {
	h := newFakeHub(t)
	d := lampNode(t)
	// Served but not public: it must not appear, and neither may its price.
	if err := d.Providers().Register(context.Background(), &cardPricedWork{}); err != nil {
		t.Fatal(err)
	}
	raw, seq, err := d.networkCard(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := a2acard.CheckPublishForm(raw); err != nil {
		t.Fatalf("signed card is not in publish form: %v", err)
	}
	v := verifyOwn(t, d, raw)
	if v.AID != d.AID() || v.Seq != seq || v.Name != "Lamp Agent" {
		t.Fatalf("verified %+v, want aid %s seq %d", v, d.AID(), seq)
	}
	// The signed bytes are the RFC 8785 form: what is stored and relayed is
	// exactly what the signature covers.
	if canon, err := a2acard.Canonicalize(raw); err != nil || !bytes.Equal(canon, raw) {
		t.Fatalf("card is not in canonical form (%v)", err)
	}

	card := decodeCard(t, raw)
	assertPublishForm(t, card, "")

	sigs, _ := card["signatures"].([]any)
	if len(sigs) != 1 {
		t.Fatalf("want one signature, got %v", card["signatures"])
	}
	card["signatures"] = "<sig>"
	ext := card["capabilities"].(map[string]any)["extensions"].([]any)
	params := ext[0].(map[string]any)["params"].(map[string]any)
	for _, k := range []string{"seq", "issuedAt", "notBefore"} {
		s, ok := params[k].(string)
		if !ok || s == "" || strings.TrimLeft(s, "0123456789") != "" {
			t.Fatalf("params.%s = %#v, want a decimal string", k, params[k])
		}
		params[k] = "<" + k + ">"
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(card); err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(strings.TrimSpace(buf.String()), d.AID(), "<aid>")
	s = strings.ReplaceAll(s, h.URL, "<hub>")
	s = strings.ReplaceAll(s, `"version":"`+Version+`"`, `"version":"<version>"`)

	const golden = `{"capabilities":{"extensions":[` +
		`{"description":"anet identity (AID) and card sequence; the card is signed under the AID's current KEL key",` +
		`"params":{"aid":"<aid>","issuedAt":"<issuedAt>","notBefore":"<notBefore>","seq":"<seq>"},` +
		`"uri":"https://agentnetwork.org.cn/a2a/ext/anet-card/v1"},` +
		`{"description":"completed tasks carry a provider-signed receipt and anet.* evidence metadata",` +
		`"uri":"https://agentnetwork.org.cn/a2a/ext/anet-evidence/v1"}],` +
		`"pushNotifications":false,"streaming":false},` +
		`"defaultInputModes":["application/json"],"defaultOutputModes":["application/json"],` +
		`"description":"Turns the lamp on and off.","name":"Lamp Agent","signatures":"<sig>",` +
		`"skills":[{"description":"anet capability light.onoff@sim/lamp-1 (no description published by its provider)",` +
		`"id":"light.onoff@sim/lamp-1","name":"light.onoff@sim/lamp-1","tags":["light","onoff"]}],` +
		`"supportedInterfaces":[{"protocolBinding":"https://agentnetwork.org.cn/a2a/bindings/anet-relay/v1",` +
		`"protocolVersion":"1.0","tenant":"<aid>","url":"<hub>/relay"}],"version":"<version>"}`
	if s != golden {
		t.Fatalf("network card changed\n got  %s\n want %s", s, golden)
	}
	for _, uri := range []string{module.ExtX402URI, module.ExtPricingURI} {
		if bytes.Contains(raw, []byte(uri)) {
			t.Fatalf("a card with no payment module carries %s", uri)
		}
	}
	// The protected header names the current key and the hub's JWKS.
	hdr := decodeProtected(t, sigs[0].(map[string]any)["protected"].(string))
	wantHdr := map[string]any{"alg": "EdDSA", "typ": "JOSE",
		"kid": a2acard.KID(d.AID(), d.self.CurrentSeq()), "jku": h.URL + "/agents/" + d.AID() + "/jwks.json"}
	if !reflect.DeepEqual(hdr, wantHdr) {
		t.Fatalf("protected header %v, want %v", hdr, wantHdr)
	}
}

func decodeProtected(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// No public skill, no card: a fresh install serves nobody, and A2A
// requires skills to be non-empty. A public capability nobody serves does
// not count.
func TestNoPublicSkillMeansNoCardAndARegistrationWithoutOne(t *testing.T) {
	h := newFakeHub(t)
	d := newCardDaemon(t, "Nobody", "")
	if _, _, err := d.networkCard(h.URL); err != errNoPublicSkill {
		t.Fatalf("got %v, want errNoPublicSkill", err)
	}
	setPublic(d, "not.served")
	if _, _, err := d.networkCard(h.URL); err != errNoPublicSkill {
		t.Fatalf("an unserved public capability built a card: %v", err)
	}
	if err := d.RegisterWithHub(context.Background(), h.URL, "Nobody", nil, ""); err != nil {
		t.Fatal(err)
	}
	fh := fakeHubAt(t, h.URL)
	fh.mu.Lock()
	sends, status := fh.a2aCardSends, fh.registerCards[d.AID()]
	fh.mu.Unlock()
	if sends != 0 || status != hubapi.CardStatusAbsent {
		t.Fatalf("registration carried a card: sends %d, card_status %q", sends, status)
	}
	if pub := d.CardPublicationStatus(); pub.Sent {
		t.Fatalf("publication status says a card was sent: %+v", pub)
	}
}

// A registration carries the card, the hub admits it, and a second
// registration that changes nothing resends the same bytes (unchanged),
// so the seq does not creep upward. A change mints a higher seq.
func TestRegistrationPublishesTheCardAndRepeatsItUnchanged(t *testing.T) {
	h := newFakeHub(t)
	d := lampNode(t)
	fh := fakeHubAt(t, h.URL)
	ctx := context.Background()
	stored := func() ([]byte, string, a2acard.Mark) {
		fh.mu.Lock()
		defer fh.mu.Unlock()
		return fh.a2aCards[d.AID()], fh.registerCards[d.AID()], fh.a2aMarks[d.AID()]
	}

	if err := d.RegisterWithHub(ctx, h.URL, "Lamp Agent", nil, ""); err != nil {
		t.Fatal(err)
	}
	first, status, mark1 := stored()
	if status != hubapi.CardStatusOK || len(first) == 0 {
		t.Fatalf("first registration: card_status %q", status)
	}
	if pub := d.CardPublicationStatus(); !pub.Sent || pub.Status != hubapi.CardStatusOK || pub.Seq != mark1.Seq {
		t.Fatalf("publication status %+v", pub)
	}

	if err := d.RegisterWithHub(ctx, h.URL, "Lamp Agent", nil, ""); err != nil {
		t.Fatal(err)
	}
	second, status, mark2 := stored()
	if status != hubapi.CardStatusUnchanged || !bytes.Equal(first, second) || mark2 != mark1 {
		t.Fatalf("an unchanged node re-registered with a different card: status %q", status)
	}

	// A restart keeps the issued card, so it too is unchanged.
	d2, err := New(d.layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d2.Close() })
	if err := d2.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	setPublic(d2, lampCap)
	if err := d2.RegisterWithHub(ctx, h.URL, "Lamp Agent", nil, ""); err != nil {
		t.Fatal(err)
	}
	if third, status, _ := stored(); status != hubapi.CardStatusUnchanged || !bytes.Equal(first, third) {
		t.Fatalf("after a restart: status %q, same bytes %v", status, bytes.Equal(first, third))
	}

	d2.mu.Lock()
	d2.cfg.Summary = "Now also dims."
	d2.mu.Unlock()
	if err := d2.RegisterWithHub(ctx, h.URL, "Lamp Agent", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, status, mark3 := stored(); status != hubapi.CardStatusOK || mark3.Seq <= mark1.Seq {
		t.Fatalf("a changed card: status %q, seq %d after %d", status, mark3.Seq, mark1.Seq)
	}
}

// The hub holds this seq with other content (a restored data directory):
// the node re-issues under a new seq and the second attempt is admitted.
func TestAConflictIsResolvedWithAFreshSeq(t *testing.T) {
	h := newFakeHub(t)
	d := lampNode(t)
	fh := fakeHubAt(t, h.URL)
	ctx := context.Background()
	if err := d.RegisterWithHub(ctx, h.URL, "Lamp Agent", nil, ""); err != nil {
		t.Fatal(err)
	}
	fh.mu.Lock()
	m := fh.a2aMarks[d.AID()]
	before := m.Seq
	m.PayloadHash[0] ^= 1 // the hub's record now disagrees with this node's at the same seq
	fh.a2aMarks[d.AID()] = m
	fh.mu.Unlock()

	if err := d.RegisterWithHub(ctx, h.URL, "Lamp Agent", nil, ""); err != nil {
		t.Fatal(err)
	}
	fh.mu.Lock()
	status, after := fh.registerCards[d.AID()], fh.a2aMarks[d.AID()].Seq
	fh.mu.Unlock()
	if status != hubapi.CardStatusOK || after <= before {
		t.Fatalf("conflict not resolved: status %q, seq %d after %d", status, after, before)
	}
}

// cardModule stands in for p2p and x402: a module that contributes to the
// card, written carelessly on purpose.
type cardModule struct {
	ext    []map[string]any
	ifaces []map[string]any
}

func (c *cardModule) Name() string                                       { return "cardtest" }
func (c *cardModule) Start(context.Context, module.Host) error           { return nil }
func (c *cardModule) Stop(context.Context) error                         { return nil }
func (c *cardModule) CardExtensions(module.CardContext) []map[string]any { return c.ext }
func (c *cardModule) CardInterfaces(cc module.CardContext) []map[string]any {
	for _, it := range c.ifaces {
		if it["tenant"] == "<aid>" {
			it["tenant"] = cc.AID
		}
	}
	return c.ifaces
}

// A p2p node's card carries its direct interface after the relay one; a
// loopback address never enters it. Contributions are put in publish form:
// "required": false and an empty description disappear, and an extension
// carrying a number, or naming a kernel URI, is dropped.
func TestContributionsEnterTheCardInPublishForm(t *testing.T) {
	h := newFakeHub(t)
	d := lampNode(t)
	d.modules = append(d.modules, &cardModule{
		ifaces: []map[string]any{
			{"url": "tcp://203.0.113.7:4001", "protocolBinding": module.BindingP2PURI, "protocolVersion": "1.0", "tenant": "<aid>"},
			{"url": "tcp://127.0.0.1:4001", "protocolBinding": module.BindingP2PURI, "protocolVersion": "1.0", "tenant": "<aid>"},
			{"url": "tcp://[::]:4001", "protocolBinding": module.BindingP2PURI, "protocolVersion": "1.0"},
			{"url": "https://evil.example/relay", "protocolBinding": a2acard.BindingRelayURI, "protocolVersion": "1.0", "tenant": "someone"},
		},
		ext: []map[string]any{
			{"uri": module.ExtX402URI, "required": false, "description": ""},
			{"uri": module.ExtPricingURI, "params": map[string]any{"network": "hub:x", "prices": []any{map[string]any{"skillId": lampCap, "amount": 5}}}},
			{"uri": a2acard.ExtCardURI, "params": map[string]any{"aid": "someone"}},
			{"uri": "https://example.org/ext", "params": map[string]any{}, "required": true},
		},
	})
	raw, _, err := d.networkCard(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	verifyOwn(t, d, raw)
	card := decodeCard(t, raw)
	assertPublishForm(t, card, "")

	ifaces := card["supportedInterfaces"].([]any)
	if len(ifaces) != 2 {
		t.Fatalf("want relay + direct, got %v", ifaces)
	}
	if ifaces[0].(map[string]any)["protocolBinding"] != a2acard.BindingRelayURI ||
		ifaces[0].(map[string]any)["tenant"] != d.AID() {
		t.Fatalf("relay interface first, for this AID: %v", ifaces[0])
	}
	want := map[string]any{"url": "tcp://203.0.113.7:4001", "protocolBinding": module.BindingP2PURI,
		"protocolVersion": "1.0", "tenant": d.AID()}
	if !reflect.DeepEqual(ifaces[1], want) {
		t.Fatalf("direct interface %v, want %v", ifaces[1], want)
	}
	if bytes.Contains(raw, []byte("127.0.0.1:4001")) || bytes.Contains(raw, []byte("evil.example")) {
		t.Fatalf("a loopback or foreign relay interface entered the card:\n%s", raw)
	}

	var uris []string
	for _, e := range card["capabilities"].(map[string]any)["extensions"].([]any) {
		em := e.(map[string]any)
		uris = append(uris, em["uri"].(string))
		if em["uri"] == module.ExtX402URI && len(em) != 1 {
			t.Fatalf("x402 declaration not reduced to its uri: %v", em)
		}
		if em["uri"] == "https://example.org/ext" {
			if _, ok := em["params"]; ok || em["required"] != true {
				t.Fatalf("empty params kept or required lost: %v", em)
			}
		}
	}
	// Kernel entries first, then contributions by uri.
	wantURIs := []string{a2acard.ExtCardURI, module.ExtEvidenceURI, "https://example.org/ext", module.ExtX402URI}
	if !reflect.DeepEqual(uris, wantURIs) {
		t.Fatalf("extensions %v, want %v", uris, wantURIs)
	}
}

// list_agents free text stays on this node: the hub is asked for the
// listing, and the match is made here.
func TestFindMatchesLocallyAndDoesNotSendTheQuery(t *testing.T) {
	h := newFakeHub(t)
	a := newCardDaemon(t, "Translator", "")
	b := newCardDaemon(t, "Painter", "")
	ctx := context.Background()
	for _, x := range []struct {
		d    *Daemon
		name string
		caps []string
	}{{a, "Translator", []string{"text.translate"}}, {b, "Painter", []string{"image.paint"}}} {
		setPublic(x.d, x.caps...) // only public capabilities are published (0017 Q14)
		if err := x.d.RegisterWithHub(ctx, h.URL, x.name, x.caps, ""); err != nil {
			t.Fatal(err)
		}
	}
	a.mu.Lock()
	a.cfg.HubURL = h.URL
	a.mu.Unlock()
	fh := fakeHubAt(t, h.URL)
	fh.mu.Lock()
	fh.agentsQueries = nil
	fh.mu.Unlock()

	got, err := a.Find(ctx, "TRANSLATE")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AID != a.AID() {
		t.Fatalf("got %v", got)
	}
	fh.mu.Lock()
	queries := fh.agentsQueries
	fh.mu.Unlock()
	for _, q := range queries {
		if strings.Contains(strings.ToLower(q), "translate") {
			t.Fatalf("the free text reached the hub: %q", q)
		}
	}
	if len(queries) != 1 {
		t.Fatalf("want one listing request, got %v", queries)
	}
	all, err := a.Find(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("empty query: %v %v", all, err)
	}
}

// cardPricedWork is a public capability with a price. Declared here rather
// than reusing pay_test.go's, which a -tags no_x402 build does not compile.
type cardPricedWork struct{ lampProvider }

func (*cardPricedWork) ID() string { return "cardpriced" }
func (*cardPricedWork) Capabilities(context.Context) ([]string, error) {
	return []string{"work.priced"}, nil
}
func (*cardPricedWork) Price(c string) (uint64, bool) { return 5, c == "work.priced" }

// A priced public skill: with the payment module in the build, the card
// declares a2a-x402 (required, since every skill is priced) and the signed
// price list; a -tags no_x402 build refuses every call to a priced
// capability, so it lists no such skill and carries neither URI. Run under
// both.
func TestPaymentOnTheCardFollowsTheBuild(t *testing.T) {
	h := newFakeHub(t)
	d := newCardDaemon(t, "Worker", "")
	if err := d.Providers().Register(context.Background(), &cardPricedWork{}); err != nil {
		t.Fatal(err)
	}
	setPublic(d, "work.priced")
	d.mu.Lock()
	d.cfg.HubURL = h.URL // the payment seam reads the hub from config
	d.mu.Unlock()

	compiled := false
	for _, n := range module.Compiled() {
		compiled = compiled || n == "x402"
	}
	if !compiled {
		if _, _, err := d.networkCard(h.URL); err != errNoPublicSkill {
			t.Fatalf("a no_x402 build published a skill it refuses: %v", err)
		}
		if err := d.Providers().Register(context.Background(), &lampProvider{}); err != nil {
			t.Fatal(err)
		}
		setPublic(d, "work.priced", lampCap)
		raw, _, err := d.networkCard(h.URL)
		if err != nil {
			t.Fatal(err)
		}
		v := verifyOwn(t, d, raw)
		if len(v.Skills) != 1 || v.Skills[0].ID != lampCap {
			t.Fatalf("skills %+v, want only the free %s", v.Skills, lampCap)
		}
		if bytes.Contains(raw, []byte("x402")) || bytes.Contains(raw, []byte(module.ExtPricingURI)) {
			t.Fatalf("a no_x402 build published a payment URI:\n%s", raw)
		}
		return
	}

	raw, _, err := d.networkCard(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	verifyOwn(t, d, raw)
	card := decodeCard(t, raw)
	assertPublishForm(t, card, "")

	exts := map[string]map[string]any{}
	for _, e := range card["capabilities"].(map[string]any)["extensions"].([]any) {
		em := e.(map[string]any)
		exts[em["uri"].(string)] = em
	}
	x, ok := exts[module.ExtX402URI]
	if !ok || x["required"] != true {
		t.Fatalf("a2a-x402 declaration %v; want required true", x)
	}
	hubAID, _, err := d.hubIdentity(context.Background(), h.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"network": "hub:" + hubAID,
		"prices": []any{map[string]any{"skillId": "work.priced", "amount": "5"}}}
	if p := exts[module.ExtPricingURI]; p == nil || !reflect.DeepEqual(p["params"], want) {
		t.Fatalf("pricing %v, want params %v", p, want)
	}
}

// A node that cannot take payment (no payment module, as in a -tags
// no_x402 build) refuses every call to a priced capability
// (capability.go), so its card does not list one: a directory entry for it
// would invite calls that can only fail. Build-independent: the payer is
// removed by hand.
func TestAPricedSkillIsLeftOffWhenThisNodeCannotCharge(t *testing.T) {
	h := newFakeHub(t)
	d := lampNode(t)
	if err := d.Providers().Register(context.Background(), &cardPricedWork{}); err != nil {
		t.Fatal(err)
	}
	setPublic(d, lampCap, "work.priced")
	d.mu.Lock()
	d.pay = nil
	d.mu.Unlock()
	raw, _, err := d.networkCard(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	v := verifyOwn(t, d, raw)
	if len(v.Skills) != 1 || v.Skills[0].ID != lampCap {
		t.Fatalf("skills %+v, want only the free %s", v.Skills, lampCap)
	}
	if bytes.Contains(raw, []byte("work.priced")) {
		t.Fatalf("the priced skill is on the card:\n%s", raw)
	}
}

// The card sent with a registration carries the name registered under,
// not the one in the config: an explicit hub-register writes its name to
// the config only after the hub has accepted it.
func TestTheCardCarriesTheRegisteredName(t *testing.T) {
	h := newFakeHub(t)
	d := lampNode(t) // configured name "Lamp Agent"
	if err := d.RegisterWithHub(context.Background(), h.URL, "Desk Lamp", nil, ""); err != nil {
		t.Fatal(err)
	}
	fh := fakeHubAt(t, h.URL)
	fh.mu.Lock()
	raw := fh.a2aCards[d.AID()]
	fh.mu.Unlock()
	if v := verifyOwn(t, d, raw); v.Name != "Desk Lamp" {
		t.Fatalf("card name %q, want the registered %q", v.Name, "Desk Lamp")
	}
}

// The first hub-register of a node with a priced public skill: the payment
// module reads the hub from the config, which HubRegister writes after the
// hub answered, so the node publishes again and the hub ends up with the
// price list.
func TestAFirstHubRegisterEndsWithThePriceListPublished(t *testing.T) {
	compiled := false
	for _, n := range module.Compiled() {
		compiled = compiled || n == "x402"
	}
	if !compiled {
		t.Skip("no payment module in this build")
	}
	h := newFakeHub(t)
	d := newCardDaemon(t, "Worker", "")
	if err := d.Providers().Register(context.Background(), &cardPricedWork{}); err != nil {
		t.Fatal(err)
	}
	setPublic(d, "work.priced")
	if err := d.HubRegister(context.Background(), h.URL, "Worker", nil, ""); err != nil {
		t.Fatal(err)
	}
	fh := fakeHubAt(t, h.URL)
	deadline := time.Now().Add(5 * time.Second)
	for {
		fh.mu.Lock()
		raw := fh.a2aCards[d.AID()]
		fh.mu.Unlock()
		if bytes.Contains(raw, []byte(module.ExtPricingURI)) {
			verifyOwn(t, d, raw)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the hub never received the price list:\n%s", raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Making a capability public republishes the registration, and with it the
// card, without an explicit hub-register.
func TestSettingPublicCapabilitiesRepublishesTheCard(t *testing.T) {
	h := newFakeHub(t)
	d := newCardDaemon(t, "Lamp Agent", "")
	ctx := context.Background()
	if err := d.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	if err := d.RegisterWithHub(ctx, h.URL, "Lamp Agent", nil, ""); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.cfg.HubURL = h.URL
	d.mu.Unlock()
	fh := fakeHubAt(t, h.URL)
	fh.mu.Lock()
	if fh.registerCards[d.AID()] != hubapi.CardStatusAbsent {
		t.Fatalf("a node with nothing public registered with a card: %q", fh.registerCards[d.AID()])
	}
	fh.mu.Unlock()

	if err := d.SetPublicCapabilities([]PublicCapability{{ID: lampCap}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		fh.mu.Lock()
		status := fh.registerCards[d.AID()]
		fh.mu.Unlock()
		if status == hubapi.CardStatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no card was published after the capability became public (card_status %q)", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
