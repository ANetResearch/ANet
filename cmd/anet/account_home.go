package main

import (
	"os"
	"os/user"
	"path/filepath"
)

// accountHome is the home directory of the account that runs anet, as the
// user database records it (os/user: the passwd entry on Unix, the profile
// directory that Windows reports for the account), not as the environment
// says: HOME, like ANET_HOME, is a variable that a host may pass on from a
// project. os.UserHomeDir is used only when the lookup fails. An anet home
// that HOME spells differently is therefore outside, and is not started
// implicitly (the safe direction). A variable so that the tests of this
// package can name a throwaway home (cli_subprocess_test.go).
var accountHome = func() string {
	if u, err := user.Current(); err == nil && filepath.IsAbs(u.HomeDir) {
		return u.HomeDir
	}
	if h, err := os.UserHomeDir(); err == nil && filepath.IsAbs(h) {
		return h
	}
	return ""
}
