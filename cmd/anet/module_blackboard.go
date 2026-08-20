//go:build !no_blackboard

package main

// Shared cognition: the org blackboard, offered as capabilities.
//
// `-tags no_blackboard` leaves it out entirely — the build for a node that
// delegates and receives work but takes no part in a group's shared
// reasoning.
import _ "github.com/ANetResearch/ANet/module/blackboard"
