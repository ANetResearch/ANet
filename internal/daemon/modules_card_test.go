package daemon

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/identity"
)

func proxyCardJSON(ifaceURL, binding string, exts ...string) []byte {
	var ext []any
	for _, u := range exts {
		ext = append(ext, map[string]any{"uri": u})
	}
	b, _ := json.Marshal(map[string]any{
		"name": "n", "description": "d", "version": "1",
		"supportedInterfaces": []any{map[string]any{"url": ifaceURL, "protocolBinding": binding, "protocolVersion": "1.0"}},
		"capabilities":        map[string]any{"streaming": true, "pushNotifications": false, "extensions": ext},
		"defaultInputModes":   []any{"text/plain"}, "defaultOutputModes": []any{"text/plain"},
		"skills": []any{map[string]any{"id": "chat", "name": "chat", "description": "d", "tags": []any{"chat"}}},
	})
	return b
}

// The proxy-card grant signs cards about a local interface, under this
// node's current key without a jku, and nothing a hub would take for this
// node's network card (A2A-DESIGN §11.3).
func TestSignProxyCard(t *testing.T) {
	c, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	h := moduleHost{&Daemon{self: c}}
	signed, err := h.SignProxyCard(proxyCardJSON("http://127.0.0.1:41811/a2a/v1/agents/x/jsonrpc", "JSONRPC", "https://agentnetwork.org.cn/a2a/ext/anet-origin/v1"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	var out struct {
		Signatures []a2acard.Signature `json:"signatures"`
	}
	if err := json.Unmarshal(signed, &out); err != nil || len(out.Signatures) != 1 {
		t.Fatalf("signed card: %s %v", signed, err)
	}
	pub := c.CurrentPrivateKey().Public().(ed25519.PublicKey)
	hdr, err := a2acard.VerifySignature(signed, out.Signatures[0], pub)
	if err != nil || hdr.Kid != a2acard.KID(c.AID(), c.CurrentSeq()) || hdr.Jku != "" {
		t.Fatalf("header %+v %v", hdr, err)
	}

	for name, card := range map[string][]byte{
		"anet-card extension": proxyCardJSON("http://127.0.0.1:1/x", "JSONRPC", a2acard.ExtCardURI),
		"relay interface":     proxyCardJSON("https://hub.example/relay", a2acard.BindingRelayURI),
		"remote interface":    proxyCardJSON("http://192.0.2.1:41811/x", "JSONRPC"),
		"https interface":     proxyCardJSON("https://127.0.0.1:41811/x", "JSONRPC"),
		"not JSON":            []byte("{"),
	} {
		if _, err := h.SignProxyCard(card); err == nil || !strings.Contains(err.Error(), "proxy card") {
			t.Errorf("%s: signed (%v)", name, err)
		}
	}
}
