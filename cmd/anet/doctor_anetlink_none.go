//go:build no_anetlink

package main

// doctor_anetlink_none.go is anetlinkChecks in a -tags no_anetlink build: there is no C1 client to
// probe with. A configured modules.anetlink stops the daemon at start (module_absent.configured), and
// doctor's modules row says that once, so there is nothing more to say here.

import (
	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

func anetlinkChecks(func(id, status, detail, hint string), func(id, status, key string, args a2ashape.Args),
	daemon.Config, []daemon.PublicCapability) {
}
