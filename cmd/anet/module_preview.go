//go:build !no_preview

package main

// Streaming previews of a reply while it is being written.
//
// `-tags no_preview` leaves the module out: the binary neither declares nor
// requests previews, and a preview frame that arrives is dropped and
// counted. The tag on this file is the one that governs the module, so the
// module's init() does not run in a build that carries it.
import _ "github.com/ANetResearch/ANet/module/preview"
