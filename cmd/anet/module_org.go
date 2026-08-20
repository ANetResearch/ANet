//go:build !no_org

package main

// Organisation membership.
//
// The subsystem that taught the lesson the seam exists for: in anet3 it had
// no boundary at all and removing it took an axe. `-tags no_org` and it is
// simply not in the binary.
import _ "github.com/ANetResearch/ANet/module/org"
