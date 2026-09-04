package daemon

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/provider"
)

// slowProvider serves one capability that blocks until released, and
// declares a bound far past the daemon's default.
type slowProvider struct {
	started  atomic.Int32
	release  chan struct{}
	declared time.Duration
}

func newSlowProvider(d time.Duration) *slowProvider {
	return &slowProvider{release: make(chan struct{}), declared: d}
}

func (s *slowProvider) ID() string { return "slow" }
func (s *slowProvider) Capabilities(context.Context) ([]string, error) {
	return []string{"work.slow"}, nil
}
func (s *slowProvider) Describe(context.Context) (string, error) { return "", nil }
func (s *slowProvider) Health(context.Context) error             { return nil }
func (s *slowProvider) InvokeTimeout(string) (time.Duration, bool) {
	return s.declared, s.declared > 0
}
func (s *slowProvider) Invoke(ctx context.Context, _ provider.Call) (effect.Effect, error) {
	s.started.Add(1)
	select {
	case <-s.release:
		return effect.Effect{Status: effect.OK, Message: "done"}, nil
	case <-ctx.Done():
		return effect.Effect{Status: effect.Failed, Message: "cut off"}, nil
	}
}

// The daemon must take the provider's word for how long its work takes.
// Two layers each held a timeout and only the shorter one was ever
// visible: the shell module validated timeout_s up to its ceiling, an
// operator set twenty minutes, and this constant killed the command at
// sixty seconds.
func TestTheProvidersOwnBoundIsTheOneThatApplies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared time.Duration
		want     time.Duration
		long     bool
	}{
		{"declares nothing", 0, capabilityInvokeTimeout, false},
		{"declares less than the default", 5 * time.Second, capabilityInvokeTimeout, false},
		{"declares exactly the default", capabilityInvokeTimeout, capabilityInvokeTimeout, false},
		{"declares an hour", time.Hour, time.Hour, true},
		{"declares a day", 24 * time.Hour, 24 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, long := invokeBound(newSlowProvider(tc.declared), "work.slow")
			if got != tc.want {
				t.Errorf("bound = %s, want %s", got, tc.want)
			}
			if long != tc.long {
				t.Errorf("long = %v, want %v", long, tc.long)
			}
		})
	}
}

// A provider that does not implement the interface at all keeps the
// default — the interface is optional and its absence is not an error.
func TestAProviderThatDeclaresNothingKeepsTheDefault(t *testing.T) {
	got, long := invokeBound(&lampProvider{}, "light.onoff@sim/lamp-1")
	if got != capabilityInvokeTimeout || long {
		t.Fatalf("bound = %s long = %v, want %s false", got, long, capabilityInvokeTimeout)
	}
}

// The property the whole change exists for: dispatching a long call
// must not hold the mailbox poll.
//
// pollOnce dispatches synchronously, so before this an hour-long command
// was an hour in which the node acked nothing and answered nobody —
// which is why the bound was sixty seconds in the first place. Raising
// the bound without moving the work off the loop would only have made
// the freeze longer.
//
// Timed directly against pollOnce rather than inferred from a second
// call coming back, because that is the thing that must not block.
func TestALongCallDoesNotHoldTheMailboxPoll(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	slow := newSlowProvider(time.Hour)
	lamp := &lampProvider{}
	for _, p := range []provider.CapabilityProvider{slow, lamp} {
		if err := prov.Providers().Register(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Board", []string{"work.slow"}, GuestDefaultMessages, ""); err != nil {
		t.Fatal(err)
	}
	defer close(slow.release)

	if _, err := req.DelegateCapability(ctx, prov.AID(), "work.slow", nil); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the poll took %s — it waited for the long call to finish", elapsed)
	}
	// It really is running, not skipped.
	deadline := time.Now().Add(10 * time.Second)
	for slow.started.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the poll returned promptly because the call never started")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And the node goes on serving while it runs.
	quickID, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1", map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := req.Results(ctx)
	if err != nil {
		t.Fatal(err)
	}
	answered := false
	for _, r := range results {
		if r.InteractionID == quickID && strings.Contains(r.Result, `"status":"OK"`) {
			answered = true
		}
	}
	if !answered {
		t.Error("the node did not answer other work while a long call was in flight")
	}
	if got := slow.started.Load(); got != 1 {
		t.Errorf("the long call ran %d times, want 1", got)
	}
}

// Running long calls off the poll loop is what makes concurrency
// possible here, so it needs its own limit — otherwise the fix trades
// "one long task freezes the node" for "twenty exhaust it", which on a
// small board is the worse of the two. Over the limit the caller is told,
// not queued behind work it cannot see.
func TestOverTheConcurrencyLimitTheCallerIsToldRatherThanQueued(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	slow := newSlowProvider(time.Hour)
	if err := prov.Providers().Register(ctx, slow); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Board", []string{"work.slow"}, GuestDefaultMessages, ""); err != nil {
		t.Fatal(err)
	}
	defer close(slow.release)

	for i := 0; i < maxConcurrentLongCalls; i++ {
		if _, err := req.DelegateCapability(ctx, prov.AID(), "work.slow", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for int(slow.started.Load()) < maxConcurrentLongCalls {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d long calls started", slow.started.Load(), maxConcurrentLongCalls)
		}
		time.Sleep(20 * time.Millisecond)
	}

	overID, err := req.DelegateCapability(ctx, prov.AID(), "work.slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := req.Results(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.InteractionID != overID {
			continue
		}
		if !strings.Contains(r.Result, "UNAVAILABLE") {
			t.Errorf("over the limit the answer was %q, want UNAVAILABLE", r.Result)
		}
		if !strings.Contains(r.Result, "long tasks") {
			t.Errorf("the refusal does not say why: %q", r.Result)
		}
		if got := int(slow.started.Load()); got != maxConcurrentLongCalls {
			t.Errorf("%d calls started, want %d — the limit did not hold", got, maxConcurrentLongCalls)
		}
		return
	}
	t.Fatal("the call over the limit was neither run nor refused — it was silently swallowed")
}
