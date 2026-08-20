//go:build !no_cas

package main

// Content-addressed storage.
//
// `-tags no_cas` leaves it out — the build for a node that delegates and
// receives work but stores no content of its own.
import _ "github.com/ANetResearch/ANet/module/cas"
