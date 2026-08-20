//go:build !no_p2p

package main

// Peer-to-peer delivery, via an out-of-process peer stack.
//
// The tag carries the same name as the module, so `-tags no_p2p` leaves it
// out of the binary entirely — which is the build for a node distributed
// alongside a hub and never expected to talk to anyone directly.
import _ "github.com/ANetResearch/ANet/module/p2p"
