//go:build no_autoreply

package main

// doctor_sandbox_none.go is doctor's side of a build without the auto-reply module: the auto_reply item is
// an info item and no sandbox item exists (ux §5.2, UX-T9b).

import (
	"encoding/json"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// sandboxSelfCheck is nil: there is no sandbox to check in this build.
var sandboxSelfCheck func() sandboxReport

func autoReplyReport(rep *doctorReport, cfg daemon.Config) {}

func autoReplyChecks(add func(id, status, detail, hint string), say func(id, status, key string, args a2ashape.Args),
	rep *doctorReport, env doctorEnv, layout daemon.Layout, cfg daemon.Config, lang a2ashape.Language) {
	rep.Checks = append(rep.Checks, sayCheck("auto_reply", stInfo, "autoreply.off", nil, lang))
}

func autoReplyBackend(raw json.RawMessage) (backend string, configured bool) { return "", false }
