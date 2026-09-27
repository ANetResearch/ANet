//go:build taskboard

package main

// The hub's shared task board, as a client.
//
// ADDITIVE, like shell: absent unless the build asks for it with
// `-tags taskboard`. The board keeps caller-supplied titles and notes in
// the clear and serves them to anyone, so a hub builds it only when asked
// (A2A-DESIGN §9, §16). A default daemon carrying the client would be
// carrying a module whose every call ends in the default hub's 404.
import _ "github.com/ANetResearch/ANet/module/taskboard"
