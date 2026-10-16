//go:build mod4_pending

package daemon

// autoreply_testhelpers_test.go — the tests of the execution face reach the auto-reply module through
// these methods of *Daemon, which exist in test builds only. They keep the tests written before the
// module had its own seams (MOD-3) reading as they did; nothing here is part of the kernel.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ANetResearch/ANet/internal/anethome"
)

// ar is the daemon's auto-reply module (nil when it is turned off).
func (d *Daemon) ar() *autoReplyModule {
	for _, m := range d.modules {
		if a, ok := m.(*autoReplyModule); ok {
			return a
		}
	}
	return nil
}

// SetAutoReply writes cfg as the auto-reply configuration (nil: off) as POST /autoreply does.
func (d *Daemon) SetAutoReply(cfg *AutoReplyConfig) error {
	_, err := d.ar().apply(func(*AutoReplyConfig) (*AutoReplyConfig, error) { return cfg, nil })
	return err
}

// haltLoop ends the running loop and leaves the configuration in force.
func (a *autoReplyModule) haltLoop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop != nil {
		a.stop()
		a.stop = nil
	}
}

func (d *Daemon) hAutoReply(w http.ResponseWriter, r *http.Request) {
	d.moduleRoute("autoreply", "POST /autoreply")(w, r)
}

func (d *Daemon) hAutoReplyTest(w http.ResponseWriter, r *http.Request) {
	d.moduleRoute("autoreply", "POST /autoreply-test")(w, r)
}

func (d *Daemon) TestAutoReply(ctx context.Context, prompt string) (string, error) {
	return d.ar().TestAutoReply(ctx, prompt)
}

func (d *Daemon) TestAutoReplyPaths(ctx context.Context, prompt string) ([]AutoReplyTestResult, error) {
	return d.ar().TestAutoReplyPaths(ctx, prompt)
}

func (d *Daemon) SandboxBudget() SandboxBudget { return d.ar().SandboxBudget() }

func (d *Daemon) autoReplyOnce(ctx context.Context, cfg AutoReplyConfig, replier autoReplier) {
	d.ar().autoReplyOnce(ctx, cfg, replier)
}

func (d *Daemon) runSandboxed(ctx context.Context, req sandboxRequest) (sandboxResult, error) {
	return d.ar().runSandboxed(ctx, req)
}

func (d *Daemon) newSandboxPlan() (*sandboxPlan, error) { return d.ar().newSandboxPlan() }

func (d *Daemon) sandboxNeverBind() []string { return d.ar().sandboxNeverBind() }

func (d *Daemon) newExecOutbox(ix string, sandboxed bool) (string, func(), error) {
	return d.ar().newExecOutbox(ix, sandboxed)
}

func (d *Daemon) sweepSandboxLeftovers() { d.ar().sweepSandboxLeftovers() }

func (d *Daemon) reserveRun(cfg AutoReplyConfig, peer string) (*budgetTicket, budgetVerdict, time.Duration) {
	return d.ar().reserveRun(cfg, peer)
}

func (d *Daemon) budgetRunsOnChain(since time.Time) []budgetRun {
	return d.ar().budgetRunsOnChain(since)
}

// testEnv is where the module of a test keeps its files under layout.
func testEnv(l Layout) arEnv {
	return arEnv{DataDir: l.Root, StateDir: anethome.ModuleDir(l.Root, "autoreply")}
}

// rawAR is cfg as the auto_reply block.
func rawAR(cfg *AutoReplyConfig) json.RawMessage { return marshalAutoReply(cfg) }

// AutoReplyOf is the auto-reply configuration c holds.
func AutoReplyOf(c Config) *AutoReplyConfig { return ParseAutoReply(c.AutoReply) }
