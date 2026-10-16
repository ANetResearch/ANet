//go:build no_anetlink

package main

// doctor_anetlink_none.go is anetlinkChecks in a -tags no_anetlink build: there is no C1 client to
// probe with, and a configured modules.anetlink stops the daemon at start (the module is not in this
// build), which is what doctor says.

import (
	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

func anetlinkChecks(add func(id, status, detail, hint string), _ func(id, status, key string, args a2ashape.Args),
	cfg daemon.Config, _ []daemon.PublicCapability) {
	if _, ok := cfg.Modules["anetlink"]; ok {
		add("anetlink.socket", stFail, "modules.anetlink is set, but this anet was built without the anetlink module "+
			"(-tags no_anetlink); the daemon does not start with it", "remove modules.anetlink, or install a build with the module")
	}
}
