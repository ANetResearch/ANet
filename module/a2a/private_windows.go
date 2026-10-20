//go:build !no_a2a && windows

package a2a

import "github.com/ANetResearch/ANet/internal/fsperm"

// keepPrivate gives an existing state file the private access control list (this user, SYSTEM,
// Administrators) on every start, as the unix build narrows it to 0600.
func keepPrivate(path string) error { return fsperm.Protect(path) }

// protectTemp gives a temporary file the private access control list before it is renamed into place.
func protectTemp(path string) error { return fsperm.Protect(path) }
