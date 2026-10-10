package daemon

import (
	"crypto/x509"
	"net/http"
	"sync/atomic"
	"time"
)

// Test hooks: the wire clock, the boot clock, the receive-transaction fault,
// the outbound gate and the join loop's wait.
//
// All are fields a test sets on a daemon that is already running. New
// starts the outbox loop and the receive maintenance before it returns,
// and those read the clock, so a plain function field written by the
// test and read by a loop is a data race — `go test -race` said so, in
// TestTheKeyRingRotatesAndRetires. The fields are atomic pointers and a
// test goes through the setters below; TestNoTestAssignsAHookDirectly
// keeps it that way.

// setClock replaces the wire clock (nowMS) with f; nil restores the wall
// clock. It does not touch the interaction store's own clock
// (ix.SetClock), which is a separate one.
func (d *Daemon) setClock(f func() uint64) {
	if f == nil {
		d.clock.Store(nil)
		return
	}
	d.clock.Store(&f)
}

// setMonoClock replaces the boot clock the clock gate counts running time
// on (monoMS) with f; nil restores the boot clock. The next maintenance
// period starts the count afresh, as the first period of a process does,
// so a reading of one clock is never subtracted from one of the other.
func (d *Daemon) setMonoClock(f func() uint64) {
	if f == nil {
		d.monoClock.Store(nil)
	} else {
		d.monoClock.Store(&f)
	}
	d.forgetCycle()
}

// setKeyHold makes keysHeld answer f(): a test stands in for the emergency
// rotation between its E1 and E2. nil restores the default.
func (d *Daemon) setKeyHold(f func() bool) {
	if f == nil {
		d.keyHold.Store(nil)
		return
	}
	d.keyHold.Store(&f)
}

// testKeyHold is the key hold a test set, or nil.
func (d *Daemon) testKeyHold() func() bool {
	if p := d.keyHold.Load(); p != nil {
		return *p
	}
	return nil
}

// setRxFault makes every receive transaction call f after its business
// writes, rolling back when f returns an error; nil removes it.
func (d *Daemon) setRxFault(f func(typ string) error) {
	if f == nil {
		d.rxFault.Store(nil)
		return
	}
	d.rxFault.Store(&f)
}

// setIdentityGate makes identityGate answer f(); nil restores the gate the
// identity's state decides. Only tests call it (TestOnlyTestsReplaceTheGate):
// the engine's tests hold the gate open or closed without building a
// witnessed identity, a pending event or a notice.
func (d *Daemon) setIdentityGate(f func() GateState) {
	if f == nil {
		d.idGate.gateHook.Store(nil)
		return
	}
	d.idGate.gateHook.Store(&f)
}

// setJoinWait makes the join loop call f each time it starts to wait, with
// the time (unix ms) of the attempt its timer is set for (0: none); nil
// removes it. A test learns from it that the loop took every wake-up it
// was given and armed its timer, before it changes the record under it.
func (d *Daemon) setJoinWait(f func(nextAttemptAt int64)) {
	if f == nil {
		d.join.waitHook.Store(nil)
		return
	}
	d.join.waitHook.Store(&f)
}

// testJoinWait is the join loop's wait hook a test set, or nil.
func (d *Daemon) testJoinWait() func(int64) {
	if p := d.join.waitHook.Load(); p != nil {
		return *p
	}
	return nil
}

// setHubRoots makes both hub transports trust pool instead of the system's
// roots (a test hub's certificate), from the next request on.
func (d *Daemon) setHubRoots(pool *x509.CertPool) {
	c := &d.hubc
	c.mu.Lock()
	defer c.mu.Unlock()
	c.roots = pool
	c.sides = [3]*http.Client{}
}

// testClock is the clock a test set, or nil.
func (d *Daemon) testClock() func() uint64 {
	if p := d.clock.Load(); p != nil {
		return *p
	}
	return nil
}

// testMonoClock is the boot clock a test set, or nil.
func (d *Daemon) testMonoClock() func() uint64 {
	if p := d.monoClock.Load(); p != nil {
		return *p
	}
	return nil
}

// testRxFault is the receive fault a test set, or nil.
func (d *Daemon) testRxFault() func(typ string) error {
	if p := d.rxFault.Load(); p != nil {
		return *p
	}
	return nil
}

// testIdentityGate is the gate a test set, or nil.
func (d *Daemon) testIdentityGate() func() GateState {
	if p := d.idGate.gateHook.Load(); p != nil {
		return *p
	}
	return nil
}

// testJoinHooks, when set, gives a new daemon its join hooks in place of
// the kernel's, before its join loop makes its first attempt (initJoin):
// the package's tests set it once, in an init function, so that a test
// node designates its hub its witness when it joins (KEL D-13) whichever
// attempt — the loop's first or the test's — registers it. A nil result
// keeps the kernel's. Nothing else sets it.
var testJoinHooks func(d *Daemon) JoinHooks

// setOutboxHold holds the outbox loop and the no-response sweep while on:
// their passes are skipped, so a test that drives every attempt, status
// query and sweep itself (the engine's model test) is the only one that
// makes them. What a call queues is still attempted by the call.
func (d *Daemon) setOutboxHold(on bool) { d.outboxHold.Store(on) }

// knob is a package-level duration the tests shorten (settleRetryBase).
// A plain variable raced the same way the hooks did: a test sets it while
// the loop of a daemon it started earlier — or reopened — still reads it.
type knob struct{ ns atomic.Int64 }

func newKnob(d time.Duration) *knob {
	k := &knob{}
	k.ns.Store(int64(d))
	return k
}

func (k *knob) get() time.Duration { return time.Duration(k.ns.Load()) }

// set replaces the value and returns the one it replaced.
func (k *knob) set(d time.Duration) time.Duration { return time.Duration(k.ns.Swap(int64(d))) }
