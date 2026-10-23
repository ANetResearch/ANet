//go:build no_autoreply

package main

import "github.com/ANetResearch/ANet/internal/daemon"

// The build has no auto-reply module, whose runs the runner serves: `anet runner` answers module_absent
// (MOD-F2), with the sentence every entrance of an absent module gives.
func runRunner([]string) error { return requireModule(daemon.Layout{}, "autoreply") }
