package daemon

// profile.go is the daemon-side write path for an agent's self-authored description (summary/readme/
// pricing). It is persisted to config and, when the agent is registered with a Hub, published there.
// The description is meant to be written BY the operator's agent (via `anet profile set`), not by a
// human web form; pricing is display-only text in v0.1 (no settlement).

import "context"

// Profile is the current self-description snapshot.
type Profile struct {
	Summary string `json:"summary"`
	Readme  string `json:"readme"`
	Pricing string `json:"pricing"`
}

// CurrentProfile returns the persisted profile snapshot.
func (d *Daemon) CurrentProfile() Profile {
	cfg := d.config()
	return Profile{Summary: cfg.Summary, Readme: cfg.Readme, Pricing: cfg.Pricing}
}

// SetProfile stores the full desired profile (summary/readme/pricing), persists it to config, and — if
// this agent is registered with a Hub — publishes it (signed). Callers pass the complete desired state
// (the CLI merges partial flags with the current values before calling).
func (d *Daemon) SetProfile(ctx context.Context, p Profile) error {
	d.mu.Lock()
	d.cfg.Summary = p.Summary
	d.cfg.Readme = p.Readme
	d.cfg.Pricing = p.Pricing
	cfg := d.cfg
	d.mu.Unlock()
	if err := SaveConfig(d.layout, cfg); err != nil {
		return err
	}
	if cfg.HubURL != "" {
		return d.PublishProfile(ctx, cfg.HubURL, p.Summary, p.Readme, p.Pricing)
	}
	return nil
}
