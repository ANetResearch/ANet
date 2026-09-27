package daemon

import (
	"sync/atomic"
	"time"
)

// Test hooks: the wire clock and the receive-transaction fault.
//
// Both are fields a test sets on a daemon that is already running. New
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

// setRxFault makes every receive transaction call f after its business
// writes, rolling back when f returns an error; nil removes it.
func (d *Daemon) setRxFault(f func(typ string) error) {
	if f == nil {
		d.rxFault.Store(nil)
		return
	}
	d.rxFault.Store(&f)
}

// testClock is the clock a test set, or nil.
func (d *Daemon) testClock() func() uint64 {
	if p := d.clock.Load(); p != nil {
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
