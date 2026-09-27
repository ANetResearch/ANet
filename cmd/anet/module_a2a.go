//go:build !no_a2a

// The a2a module: the local A2A interface on 127.0.0.1, through which any
// A2A client reaches the agents of the network. On without configuration.
// Removed by -tags no_a2a, which takes the A2A SDK out of the binary with
// it.
package main

import _ "github.com/ANetResearch/ANet/module/a2a"
