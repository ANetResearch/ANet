package daemon

import (
	"fmt"
	"os"

	"github.com/ANetResearch/ANet/internal/protocol/identity"
)

// LoadOrGenerateIdentity loads the daemon's persisted identity (identity.kel), or, on first run,
// incepts a fresh one and persists it (0600 — it holds private keys). The returned Controller is the
// node's single AID/KEL for its lifetime (one runtime = one agent = one AID).
func LoadOrGenerateIdentity(l Layout) (*identity.Controller, error) {
	b, err := os.ReadFile(l.IdentityPath())
	if err == nil {
		c, rerr := identity.Restore(b)
		if rerr != nil {
			return nil, fmt.Errorf("anet: restore identity %s: %w", l.IdentityPath(), rerr)
		}
		return c, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("anet: read identity: %w", err)
	}
	c, err := identity.Incept()
	if err != nil {
		return nil, err
	}
	if err := saveIdentity(l, c); err != nil {
		return nil, err
	}
	return c, nil
}

// saveIdentity persists a Controller (private-key seeds + KEL) to identity.kel, 0600.
func saveIdentity(l Layout, c *identity.Controller) error {
	if err := l.EnsureRoot(); err != nil {
		return err
	}
	blob, err := c.Export()
	if err != nil {
		return err
	}
	return writeFileAtomic(l.IdentityPath(), blob, 0o600)
}
