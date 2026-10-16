//go:build !no_autoreply

// The auto-reply module: this node answers tasks of its peers by itself, with a model service or with
// a local coding agent in a sandbox. Removed by -tags no_autoreply.
package main

import _ "github.com/ANetResearch/ANet/module/autoreply"
