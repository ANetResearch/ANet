// Package apparmor holds the AppArmor profile for bubblewrap that `anet doctor --apparmor-profile` prints.
package apparmor

import _ "embed"

// Profile is the text of anet-bwrap, byte for byte.
//
//go:embed anet-bwrap
var Profile string
