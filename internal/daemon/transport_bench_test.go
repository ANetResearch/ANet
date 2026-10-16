package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"
)

// The transport list is walked on every delegation, message and result, so
// its own cost has to be negligible next to the network call it precedes.
// These measure the dispatch, not the delivery: a fake transport returns
// immediately, so what is left is the seam.

func BenchmarkTransportDispatchDirect(b *testing.B) {
	d := benchDaemon(b)
	d.RegisterTransport(&benchTransport{name: "direct", reachable: true})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := d.deliverEnvelope(ctx, "aid-peer", benchPayload); err != nil {
			b.Fatal(err)
		}
	}
}

// The fall-through path costs one extra Reachable call plus the failed
// Send. It runs whenever a direct transport is present but cannot help,
// which on a node behind NAT is most of the time.
func BenchmarkTransportDispatchFallThrough(b *testing.B) {
	// The seam logs a transport's first failure; go test folds that into
	// stdout and splices it through the result line. Silencing it here
	// measures dispatch rather than the logger.
	quietLog(b)

	d := benchDaemon(b)
	d.RegisterTransport(&benchTransport{name: "direct", reachable: true, fail: errors.New("unreachable")})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// No hub behind it: the direct path fails, and the fall-through ends
		// at the missing hub without a network call.
		if err := d.deliverEnvelope(ctx, "aid-peer", benchPayload); err == nil {
			b.Fatal("no path took it, yet it was delivered")
		}
	}
}

// A build with no transport module: the list is the hub alone. This is the
// baseline the seam must not regress against, because it is what most
// deployments run.
func BenchmarkTransportDispatchHubOnly(b *testing.B) {
	d := benchDaemon(b)
	d.RegisterTransport(&benchTransport{name: "direct", reachable: true})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = d.directTransports()
		_ = ctx
	}
}

var benchPayload = make([]byte, 4096)

// deliverEnvelope hands one envelope to its recipient once: a direct path
// first, then the hub, with the same bytes. Nothing in the daemon calls it
// any more (a message that is not queued goes through oneShotSend); the
// benchmarks below keep it to measure the dispatch seam, and it obeys the
// outbound gate like every send (ENG-K13): while it is closed nothing goes.
func (d *Daemon) deliverEnvelope(ctx context.Context, toAID string, envelope []byte) error {
	if g := d.identityGate(); !g.Open {
		return &gateClosedError{state: g.State}
	}
	if t := d.p2pPath(ctx, toAID, d.nowMS()); t != nil {
		_, _, err := t.Send(ctx, toAID, envelope)
		d.noteTransport(t.Name(), err)
		if err == nil {
			return nil
		}
	}
	hub := d.configuredHub()
	if hub == "" {
		return fmt.Errorf("anet: no hub configured")
	}
	choice := AccessChoice{Send: true, Kind: seal.AccessKindSession}
	if c, err := d.accessChoiceFor(toAID, "", false, accessOrdinary, d.nowMS()); err == nil && c.Kind == seal.AccessKindSession {
		choice.HintHome = c.HintHome
	}
	_, _, err := d.relaySendV3(ctx, envelope, choice)
	return err
}

func quietLog(b testing.TB) {
	b.Helper()
	prev := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(prev) })
}

type benchTransport struct {
	// name matters: transport health is tracked per name, so two transports
	// sharing one flap between failing and recovered. Real transports have
	// distinct names; a benchmark that did not was measuring its own bug.
	name      string
	reachable bool
	fail      error
}

func (t *benchTransport) Name() string                           { return t.name }
func (t *benchTransport) Reachable(context.Context, string) bool { return t.reachable }
func (t *benchTransport) Send(context.Context, string, []byte) ([]byte, bool, error) {
	return nil, true, t.fail
}

// benchDaemon builds a daemon with no hub, so the fake transports carry
// everything and the benchmark measures dispatch rather than HTTP.
func benchDaemon(b *testing.B) *Daemon {
	b.Helper()
	root := b.TempDir()
	cfg := map[string]any{"control_addr": "127.0.0.1:0", "hub_url": "", "accept_delegations": false}
	j, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(root, "config.json"), j, 0o600); err != nil {
		b.Fatal(err)
	}
	d, err := New(NewLayout(root))
	if err != nil {
		b.Fatal(err)
	}
	d.mu.Lock()
	if d.relayStop != nil {
		d.relayStop()
		d.relayStop = nil
	}
	d.mu.Unlock()
	// What is measured is the dispatch: the gate (engine ENG-K13) is held
	// open, as for a node its witness confirmed.
	d.setIdentityGate(func() GateState { return GateState{Open: true, State: IdentityOK} })
	b.Cleanup(func() { d.Close() })
	return d
}
