package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"

	"github.com/ANetResearch/ANetCore/a2acard"

	"github.com/ANetResearch/ANet/module"
)

// SignProxyCard implements module.ProxyCardSigner: it signs a proxy card
// of the local A2A interface with this node's current key (kid
// did:anet:<AID>#<seq>, no jku; A2A-DESIGN §11.3).
//
// It signs cards about a local interface and nothing else. A card with the
// anet-card extension or a relay interface is what a hub admits as this
// node's network card (§10.3), and one with an interface off this machine
// is a claim about somewhere else; both are refused, so the grant cannot
// be used to speak for this node's identity on the network.
func (h moduleHost) SignProxyCard(card []byte) ([]byte, error) {
	if err := checkProxyCard(card); err != nil {
		return nil, err
	}
	return a2acard.SignWithController(card, h.d.self, "")
}

var _ module.ProxyCardSigner = moduleHost{}

// checkProxyCard refuses a card that is not a proxy card.
func checkProxyCard(card []byte) error {
	var c struct {
		SupportedInterfaces []struct {
			URL             string `json:"url"`
			ProtocolBinding string `json:"protocolBinding"`
		} `json:"supportedInterfaces"`
		Capabilities struct {
			Extensions []struct {
				URI string `json:"uri"`
			} `json:"extensions"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(card, &c); err != nil {
		return fmt.Errorf("anet: proxy card: %w", err)
	}
	if len(c.SupportedInterfaces) == 0 {
		return errors.New("anet: proxy card: no interfaces")
	}
	for _, e := range c.Capabilities.Extensions {
		if e.URI == a2acard.ExtCardURI {
			return errors.New("anet: proxy card: the anet-card extension belongs to this node's network card")
		}
	}
	for _, i := range c.SupportedInterfaces {
		if i.ProtocolBinding == a2acard.BindingRelayURI {
			return errors.New("anet: proxy card: a relay interface belongs to this node's network card")
		}
		u, err := url.Parse(i.URL)
		if err != nil || u.Scheme != "http" || u.User != nil {
			return fmt.Errorf("anet: proxy card: interface %q is not a local http URL", i.URL)
		}
		if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("anet: proxy card: interface %q is not on this machine's loopback", i.URL)
		}
	}
	return nil
}
