package daemon

// bootclock.go: the monotonic clock of the clock gate (FS-K13a). The gate
// and unverified_run read it through monoMS and nothing else
// (TestGateUsesBootClock): Go's own monotonic reading (time.Since and the
// like) stops while a Linux machine sleeps, so every suspend of a laptop
// would look like a wall clock jump and reset the day of running time the
// local gate waits for (FS-K14a) — on a laptop it would never open.

import (
	"log"
	"sync"
)

// monoMS is the boot clock in milliseconds (bootClock), or the clock a
// test set (setMonoClock).
func (d *Daemon) monoMS() uint64 {
	if p := d.monoClock.Load(); p != nil {
		return (*p)()
	}
	return bootClock()
}

// bootClockFailed logs, once per process, that the boot clock could not be
// read. bootClock then answers 0: the gate sees a clock that does not move
// while the wall clock does, takes it for a jump, and deletes nothing it
// would need the local clock for, which is the safe side.
var bootClockFailed sync.Once

func noteBootClockFailure(err error) {
	bootClockFailed.Do(func() {
		log.Printf("anet: cannot read the boot clock (%v); expired keys and content are kept until it can be read", err)
	})
}
