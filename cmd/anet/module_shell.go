//go:build shell

package main

// Operator-approved commands on the machine this daemon runs on.
//
// ADDITIVE, unlike every other module here: absent unless the build asks
// for it with `-tags shell`. The others are in by default and subtracted
// with `no_<name>`, because leaving out a blackboard costs a feature. The
// cost of shipping command execution to somebody who did not ask for it
// is not a feature, so the default is the safe one and the operator opts
// in — which is also what makes "this binary cannot execute anything on
// its host" a property a distribution can state and `go tool nm` can
// check.
import _ "github.com/ANetResearch/ANet/module/shell"
