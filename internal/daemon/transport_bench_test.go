package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
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
		if err := d.relaySend(ctx, "aid-peer", "message", "ix", benchPayload); err != nil {
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
	d.RegisterTransport(&benchTransport{name: "fallback", reachable: true}) // stands in for the hub
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := d.relaySend(ctx, "aid-peer", "message", "ix", benchPayload); err != nil {
			b.Fatal(err)
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
		_ = d.transports()
		_ = ctx
	}
}

var benchPayload = make([]byte, 4096)

func quietLog(b *testing.B) {
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
func (t *benchTransport) Send(context.Context, string, string, string, []byte) error {
	return t.fail
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
	b.Cleanup(func() { d.Close() })
	return d
}
