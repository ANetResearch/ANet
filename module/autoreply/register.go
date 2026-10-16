// Package autoreply is the auto-reply module: this node answers the tasks of its peers by itself, with a
// model service reached through the node's own injection route, or with a local coding agent that runs in
// a sandbox (Linux bubblewrap and seccomp, macOS Seatbelt) behind a CONNECT allowlist, under a daily
// budget. The kernel decides who may be answered and sends every reply; this package only runs the
// answer, through the TurnHost, OwnEvidenceHost, SensitivePathsHost and ModuleConfigHost seams.
//
// The package is compiled into every build except -tags no_autoreply (cmd/anet/module_autoreply.go).
package autoreply

import "github.com/ANetResearch/ANet/module"

func init() { module.Register("autoreply", newAutoReplyModule) }
