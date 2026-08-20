//go:build !no_anetlink

package main

// Device capabilities, via the ANetLink provider.
//
// The blank import carries the same tag as the module it pulls in, which is
// what makes the tag the actual switch: with `-tags no_anetlink` this file
// is not compiled, the module's init() never runs, nothing registers, and
// the subsystem is absent from the binary rather than disabled inside it.
//
// The binary is where composition is decided — that is what "one codebase,
// N distributions" means in practice.
import _ "github.com/ANetResearch/ANet/module/anetlink"
