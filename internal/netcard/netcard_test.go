package netcard

import (
	"bytes"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/a2acard"

	"github.com/ANetResearch/ANet/module"
)

func goldenInput() Input {
	return Input{
		AID: "bafyaid", Version: "0.2.0", HubURL: "https://hub.example/",
		Seq: 1790000000, IssuedAtMs: 1790000000123, NotBeforeMs: 1789999940123,
		Skills: []Skill{
			{ID: "ptz.absolute@onvif/cam-1", Name: "PTZ", Description: "Moves the camera.", Tags: []string{"camera"},
				Examples: []string{"pan left", ""}, InputModes: []string{"application/json"}, OutputModes: []string{"image/jpeg"}},
			{ID: "text.digest"},
		},
		Extensions: []map[string]any{
			{"uri": module.ExtPricingURI, "params": map[string]any{"network": "hub:bafyhub",
				"prices": []any{map[string]any{"skillId": "text.digest", "amount": "5"}}}},
			{"uri": module.ExtX402URI, "required": true},
		},
		Interfaces: []map[string]any{
			{"url": "tcp://203.0.113.7:4001", "protocolBinding": module.BindingP2PURI, "protocolVersion": "1.0", "tenant": "bafyaid"},
		},
	}
}

// The card, byte for byte. Defaults filled (name, description, a skill's
// description and tags), a skill mode equal to the card default left out,
// an empty example dropped, contributions after the kernel's own entries,
// every number a string.
func TestBuildGolden(t *testing.T) {
	got, err := Build(goldenInput())
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"name":"anet agent bafyaid","description":"` + DefaultDescription + `",` +
		`"supportedInterfaces":[` +
		`{"protocolBinding":"https://agentnetwork.org.cn/a2a/bindings/anet-relay/v1","protocolVersion":"1.0","tenant":"bafyaid","url":"https://hub.example/relay"},` +
		`{"protocolBinding":"https://agentnetwork.org.cn/a2a/bindings/anet-p2p/v1","protocolVersion":"1.0","tenant":"bafyaid","url":"tcp://203.0.113.7:4001"}],` +
		`"version":"0.2.0",` +
		`"capabilities":{"streaming":false,"pushNotifications":false,"extensions":[` +
		`{"description":"anet identity (AID) and card sequence; the card is signed under the AID's current KEL key",` +
		`"params":{"aid":"bafyaid","issuedAt":"1790000000123","notBefore":"1789999940123","seq":"1790000000"},` +
		`"uri":"https://agentnetwork.org.cn/a2a/ext/anet-card/v1"},` +
		`{"description":"completed tasks carry a provider-signed receipt and anet.* evidence metadata",` +
		`"uri":"https://agentnetwork.org.cn/a2a/ext/anet-evidence/v1"},` +
		`{"params":{"network":"hub:bafyhub","prices":[{"amount":"5","skillId":"text.digest"}]},` +
		`"uri":"https://agentnetwork.org.cn/a2a/ext/anet-pricing/v1"},` +
		`{"required":true,"uri":"https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2"}]},` +
		`"defaultInputModes":["application/json"],"defaultOutputModes":["application/json"],` +
		`"skills":[` +
		`{"id":"ptz.absolute@onvif/cam-1","name":"PTZ","description":"Moves the camera.","tags":["camera"],` +
		`"examples":["pan left"],"outputModes":["image/jpeg"]},` +
		`{"id":"text.digest","name":"text.digest","description":"anet capability text.digest (the provider gave no description)",` +
		`"tags":["text","digest"]}]}`
	if string(got) != want {
		t.Fatalf("card changed\n got  %s\n want %s", got, want)
	}
	// Publish form means one payload for every verifier: the
	// specification's proto-stripped form equals the raw form a2a-go signs.
	stripped, err := a2acard.SigningPayload(got)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a2acard.RawSigningPayload(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stripped, raw) {
		t.Fatalf("payload forms differ\n stripped %s\n raw      %s", stripped, raw)
	}
}

func TestBuildRefusesWhatCannotBePublished(t *testing.T) {
	in := goldenInput()
	in.Skills = nil
	if _, err := Build(in); !errors.Is(err, ErrNoSkill) {
		t.Fatalf("no skill: %v", err)
	}
	in = goldenInput()
	in.Skills = append(in.Skills, Skill{ID: "text.digest"})
	if _, err := Build(in); err == nil {
		t.Fatal("a repeated skill id was accepted")
	}
	in = goldenInput()
	in.Skills = nil
	for i := 0; i <= a2acard.MaxSkills; i++ {
		in.Skills = append(in.Skills, Skill{ID: "s" + strconv.Itoa(i)})
	}
	if _, err := Build(in); err == nil {
		t.Fatal("more skills than a card may hold")
	}
	// A contribution that skipped Extension is still caught before
	// anything is signed.
	in = goldenInput()
	in.Extensions = append(in.Extensions, map[string]any{"uri": "https://example.org/x", "description": ""})
	if _, err := Build(in); !a2acard.IsCode(err, a2acard.CodeNotPublishForm) {
		t.Fatalf("an empty description reached the card: %v", err)
	}
}

func TestExtensionIsPutInPublishForm(t *testing.T) {
	got, err := Extension(map[string]any{"uri": module.ExtX402URI, "required": false, "description": "", "params": map[string]any{}})
	if err != nil || !reflect.DeepEqual(got, map[string]any{"uri": module.ExtX402URI}) {
		t.Fatalf("got %v, %v", got, err)
	}
	got, err = Extension(map[string]any{"uri": "u", "required": true, "description": "d", "params": map[string]any{"a": []any{"x"}}})
	if err != nil || got["required"] != true || got["description"] != "d" || got["params"] == nil {
		t.Fatalf("got %v, %v", got, err)
	}
	for name, bad := range map[string]map[string]any{
		"no uri":        {"description": "x"},
		"kernel card":   {"uri": a2acard.ExtCardURI},
		"kernel ev":     {"uri": module.ExtEvidenceURI},
		"unknown":       {"uri": "u", "extra": "x"},
		"number":        {"uri": "u", "params": map[string]any{"amount": 5}},
		"nested number": {"uri": "u", "params": map[string]any{"prices": []any{map[string]any{"amount": 5.0}}}},
		"empty string":  {"uri": "u", "params": map[string]any{"network": ""}},
		"empty array":   {"uri": "u", "params": map[string]any{"prices": []any{}}},
		"null":          {"uri": "u", "params": map[string]any{"x": nil}},
		"required type": {"uri": "u", "required": "yes"},
	} {
		if got, err := Extension(bad); err == nil {
			t.Errorf("%s: accepted as %v", name, got)
		}
	}
}

func TestInterfaceKeepsLoopbackAndForeignRoutesOff(t *testing.T) {
	ok := map[string]any{"url": "tcp://203.0.113.7:4001", "protocolBinding": module.BindingP2PURI, "protocolVersion": "1.0", "tenant": "me"}
	if got, err := Interface(ok, "me"); err != nil || !reflect.DeepEqual(got, ok) {
		t.Fatalf("got %v, %v", got, err)
	}
	other := map[string]any{"url": "https://agent.example/a2a", "protocolBinding": "JSONRPC", "protocolVersion": "1.0", "tenant": ""}
	if got, err := Interface(other, "me"); err != nil || len(got) != 3 {
		t.Fatalf("empty tenant not removed: %v, %v", got, err)
	}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range ok {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	for name, bad := range map[string]map[string]any{
		"loopback v4":     with("url", "tcp://127.0.0.1:4001"),
		"loopback v6":     with("url", "tcp://[::1]:4001"),
		"localhost":       with("url", "http://localhost:8080/a2a"),
		"unspecified":     with("url", "tcp://0.0.0.0:4001"),
		"unix":            with("url", "unix:///run/a.sock"),
		"relative":        with("url", "/run/a.sock"),
		"relay binding":   with("protocolBinding", a2acard.BindingRelayURI),
		"foreign tenant":  with("tenant", "someone-else"),
		"no version":      with("protocolVersion", ""),
		"unknown member":  with("extra", "x"),
		"tenant not text": with("tenant", 1),
	} {
		if got, err := Interface(bad, "me"); err == nil {
			t.Errorf("%s: accepted as %v", name, got)
		}
	}
}

func TestRelayURLAndJKU(t *testing.T) {
	if got := RelayURL(" https://hub.example// "); got != "https://hub.example/relay" {
		t.Fatalf("RelayURL = %q", got)
	}
	if got := JKU("https://hub.example/", "bafyaid"); got != "https://hub.example/agents/bafyaid/jwks.json" {
		t.Fatalf("JKU = %q", got)
	}
	if !strings.HasPrefix(DefaultName("bafyreiabcdefghijklmn"), "anet agent bafyreiabcde") {
		t.Fatalf("DefaultName = %q", DefaultName("bafyreiabcdefghijklmn"))
	}
}
