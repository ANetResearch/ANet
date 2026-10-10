//go:build !no_p2p

package p2p

import (
	"github.com/ANetResearch/ANet/module"
)

// CardInterfaces adds nothing to the network card (sealed-sender SSR-P1).
// The card is public: an address in it would tell anyone who reads it where
// this node's peer process listens and that the node behind it takes direct
// deliveries, and would let anyone dial it. The address goes only to
// contacts, inside the contact-class peer capability this node gives them
// (modules.p2p.advertise, SSR-D8), and only a peer process holding one
// dials it.
func (m *Module) CardInterfaces(module.CardContext) []map[string]any { return nil }

// CardExtensions adds no extension.
func (m *Module) CardExtensions(module.CardContext) []map[string]any { return nil }

var _ module.CardContributor = (*Module)(nil)
