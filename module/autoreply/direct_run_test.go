package autoreply

// direct_run_test.go — direct runs beside the scan (startDirect): one peer's long run holds up neither
// another peer's turn nor the scans after it, one run per peer at once, at most directRunsAtOnce in all,
// no turn answered twice, and an admitted peer's task refused before its run starts once the day's
// admitted budget is used up.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/module"
)

// heldReplier answers a turn once the test lets it go, and records which turns it started and how many
// ran at once.
type heldReplier struct {
	mu      sync.Mutex
	release map[string]chan struct{} // interaction id -> closed when the turn may answer; absent: at once
	started []string
	now     int
	max     int
}

func newHeldReplier() *heldReplier { return &heldReplier{release: map[string]chan struct{}{}} }

// hold makes ix wait until let(ix).
func (r *heldReplier) hold(ix string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.release[ix] = make(chan struct{})
}

func (r *heldReplier) let(ix string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.release[ix]; c != nil {
		close(c)
		delete(r.release, ix)
	}
}

func (r *heldReplier) letAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ix, c := range r.release {
		close(c)
		delete(r.release, ix)
	}
}

func (r *heldReplier) startedTurns() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.started...)
}

func (r *heldReplier) maxAtOnce() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.max
}

func (r *heldReplier) Reply(ctx context.Context, rc replyContext, _ []chatTurn) (string, error) {
	r.mu.Lock()
	r.started = append(r.started, rc.InteractionID)
	r.now++
	r.max = max(r.max, r.now)
	c := r.release[rc.InteractionID]
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.now--
		r.mu.Unlock()
	}()
	if c != nil {
		select {
		case <-c:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "answered " + rc.InteractionID, nil
}

// directFx is the module with the exec backend and auto_reply.admitted=direct, scanned with r.
type directFx struct {
	*turnFx
	r *heldReplier
}

func newDirectFx(t *testing.T, cfg AutoReplyConfig) *directFx {
	t.Helper()
	fx := &directFx{turnFx: newTurnFx(t, cfg, nil), r: newHeldReplier()}
	t.Cleanup(func() {
		fx.r.letAll()
		fx.m.direct.runs.Wait()
	})
	return fx
}

// scan is one scan with the held replier; unlike tick it does not wait for the direct runs.
func (fx *directFx) scan() {
	fx.m.autoReplyOnce(fx.ctx, fx.cfg, fx.r)
}

// tick is one scan with the held replier and the end of the direct runs it started.
func (fx *directFx) tick() {
	fx.scan()
	fx.m.direct.runs.Wait()
}

// deliverAdmitted is a task of a peer that inbound.admit admitted.
func (fx *directFx) deliverAdmitted(peer, text string) string {
	ix := fx.deliver(peer, false, text)
	fx.h.mu.Lock()
	fx.h.ixs[ix].turn.Admitted = true
	fx.h.mu.Unlock()
	return ix
}

// waitSent waits until the module sent a message in ix.
func (fx *directFx) waitSent(ix string) {
	fx.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := fx.lastSent(ix); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	fx.t.Fatalf("%s: nothing sent", ix)
}

// waitStarted waits until the replier started n turns.
func (fx *directFx) waitStarted(n int) {
	fx.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(fx.r.startedTurns()) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	fx.t.Fatalf("started %v, want %d turns", fx.r.startedTurns(), n)
}

var directCfg = AutoReplyConfig{Backend: "exec", Agent: agentHermes, Admitted: AdmittedDirect}

// A member's long run holds up neither the scan nor another member's turn, nor a trusted peer's: the
// scan returns at once, and the turns that arrive while the long run is under way are answered.
func TestDirectRunsDoNotHoldUpOtherPeers(t *testing.T) {
	fx := newDirectFx(t, directCfg)
	long := fx.deliverAdmitted("peerA", "a task that takes hours")
	fx.r.hold(long)
	done := make(chan struct{})
	go func() {
		fx.scan()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the scan waited for the long run")
	}
	fx.waitStarted(1)

	other := fx.deliverAdmitted("peerB", "a short question")
	trusted := fx.deliver("peerC", true, "the operator's own task")
	fx.scan()
	fx.waitSent(other)
	fx.waitSent(trusted)
	if _, ok := fx.lastSent(long); ok {
		t.Fatal("the long run answered before it was let go")
	}
	fx.r.let(long)
	fx.waitSent(long)
	fx.m.direct.runs.Wait()
	for _, ix := range []string{long, other, trusted} {
		if x := fx.ix(ix); len(x.sent) != 1 {
			t.Errorf("%s answered %d times", ix, len(x.sent))
		}
	}
}

// One run per peer at once: the peer's second task waits for the next scan after its first run ended,
// and no turn is answered twice, also by a scan whose list was made before a run's answer was sent.
func TestDirectRunsOnePerPeer(t *testing.T) {
	fx := newDirectFx(t, directCfg)
	first := fx.deliverAdmitted("peerA", "first")
	second := fx.deliverAdmitted("peerA", "second")
	fx.r.hold(first)
	fx.scan()
	fx.waitStarted(1)
	fx.scan()
	fx.scan()
	time.Sleep(100 * time.Millisecond)
	if got := fx.r.startedTurns(); len(got) != 1 || got[0] != first {
		t.Fatalf("started %v while the peer's first run is under way, want only %s", got, first)
	}
	fx.r.let(first)
	fx.waitSent(first)
	fx.m.direct.runs.Wait()
	fx.tick()
	if got := fx.r.startedTurns(); len(got) != 2 || got[1] != second {
		t.Fatalf("started %v, want %s next", got, second)
	}
	fx.tick()
	if got := fx.r.startedTurns(); len(got) != 2 {
		t.Fatalf("a turn already answered was started again: %v", got)
	}
	if fx.r.maxAtOnce() != 1 {
		t.Errorf("%d runs of one peer at once", fx.r.maxAtOnce())
	}
}

// A scan that listed a turn before the run that answered it ended does not start it again; a scan that
// began before a later scan starts nothing more.
func TestDirectRunsOrder(t *testing.T) {
	var d directRuns
	s1 := d.beginScan()
	if !d.take(s1, "peerA", "ix1", 4) {
		t.Fatal("the first run was not started")
	}
	if d.take(s1, "peerA", "ix2", 4) {
		t.Fatal("a second run of the peer was started")
	}
	s2 := d.beginScan() // lists ix1 as owed: its answer is not sent yet
	d.done("peerA", "ix1")
	if d.take(s2, "peerA", "ix1", 4) {
		t.Fatal("a scan listed before the answer started the turn again")
	}
	if d.take(s1, "peerB", "ix3", 4) {
		t.Fatal("an earlier scan started a run after a later one began")
	}
	s3 := d.beginScan()
	if !d.take(s3, "peerA", "ix1", 4) {
		t.Fatal("a scan listed after the answer may start the turn's next run")
	}
	d.done("peerA", "ix1")
	if len(d.ended) != 1 || d.running != 0 || len(d.peers) != 0 {
		t.Errorf("left over: ended %v running %d peers %v", d.ended, d.running, d.peers)
	}
	d.beginScan()
	if len(d.ended) != 0 {
		t.Errorf("the ends before the latest scan are not forgotten: %v", d.ended)
	}
}

// At most directRunsAtOnce direct runs at once in all: the default without a runner, and
// auto_reply.runner.max_jobs with one.
func TestDirectRunsAtOnce(t *testing.T) {
	fx := newDirectFx(t, directCfg)
	var ixs []string
	for _, p := range []string{"p1", "p2", "p3", "p4", "p5", "p6"} {
		ix := fx.deliverAdmitted(p, "work")
		fx.r.hold(ix)
		ixs = append(ixs, ix)
	}
	fx.scan()
	fx.waitStarted(directRunsAtOnceDefault)
	fx.scan()
	time.Sleep(100 * time.Millisecond)
	if n := len(fx.r.startedTurns()); n != directRunsAtOnceDefault {
		t.Fatalf("%d runs started, want %d", n, directRunsAtOnceDefault)
	}
	fx.r.let(ixs[0])
	fx.waitSent(ixs[0])
	fx.r.letAll()
	fx.m.direct.runs.Wait()
	fx.tick()
	fx.tick()
	for _, ix := range ixs {
		if x := fx.ix(ix); len(x.sent) != 1 {
			t.Errorf("%s answered %d times", ix, len(x.sent))
		}
	}
	if m := fx.r.maxAtOnce(); m > directRunsAtOnceDefault {
		t.Errorf("%d runs at once", m)
	}

	withRunner := AutoReplyConfig{Backend: "exec", Agent: agentHermes, Runner: &AutoReplyRunnerConfig{Socket: "/run/r.sock", MaxJobs: 2}}
	if n := withRunner.directRunsAtOnce(); n != 2 {
		t.Errorf("with runner.max_jobs 2: %d at once", n)
	}
	withRunner.Runner.MaxJobs = 0
	if n := withRunner.directRunsAtOnce(); n != directRunsAtOnceDefault {
		t.Errorf("with runner.max_jobs unset: %d at once", n)
	}
	for _, bad := range []int{-1, runnerMaxJobs + 1} {
		c := withRunner
		r := *c.Runner
		r.MaxJobs = bad
		c.Runner = &r
		if ref := checkRunnerConfig(&c); ref == nil || ref.Args["key"] != "auto_reply.runner.max_jobs" {
			t.Errorf("runner.max_jobs %d: %v", bad, ref)
		}
	}
}

// Once the admitted budget of the day is used up, an admitted peer's task is refused with the budget's
// reason and the time until the next local midnight, and its agent never starts; a budget of 0 leaves the
// task in the inbox; a trusted peer is not counted.
func TestAdmittedBudgetRefusesBeforeTheRun(t *testing.T) {
	up := newFakeUpstream(t, "x")
	cfg := directCfg
	cfg.AdmittedBudget = &AutoReplyAdmittedBudget{RunsPerDay: Budget(5), RunsPerPeerPerDay: Budget(1)}
	fx := newDirectFx(t, cfg)
	fx.m.proxy = newTestProxy(t, up, "k", "")

	first := fx.deliverAdmitted("peerA", "first")
	fx.tick()
	if x := fx.ix(first); len(x.sent) != 1 {
		t.Fatalf("the first task of the day: %+v", x)
	}
	second := fx.deliverAdmitted("peerA", "second")
	fx.tick()
	x := fx.ix(second)
	if len(x.sent) != 0 || x.reason != module.ReasonBudget || x.retryAfter <= 0 || x.retryAfter > 25*time.Hour {
		t.Fatalf("the peer's second task of the day: sent %d, refused %q after %s", len(x.sent), x.reason, x.retryAfter)
	}
	if got := fx.r.startedTurns(); len(got) != 1 {
		t.Fatalf("the agent started for %v, want the first task only", got)
	}
	recs := withExit(fx.invoked(), exitBudget)
	if len(recs) != 1 || recs[0]["mode"] != modeDirect || recs[0]["trusted"] != false {
		t.Errorf("the refusal's record: %v", recs)
	}
	trusted := fx.deliver("peerA", true, "the same peer, now trusted")
	fx.tick()
	if x := fx.ix(trusted); len(x.sent) != 1 {
		t.Errorf("a trusted peer's task was counted: %+v", x)
	}

	// A budget of 0: nothing runs and nothing is refused; the task stays for the operator.
	off := cfg
	off.AdmittedBudget = &AutoReplyAdmittedBudget{RunsPerDay: Budget(0)}
	fx.setConfig(off)
	idle := fx.deliverAdmitted("peerB", "with no budget")
	fx.tick()
	if x := fx.ix(idle); len(x.sent) != 0 || x.refusals != 0 {
		t.Errorf("with a budget of 0: %+v", x)
	}
	if got := fx.r.startedTurns(); len(got) != 2 {
		t.Errorf("the agent started for %v", got)
	}
}
