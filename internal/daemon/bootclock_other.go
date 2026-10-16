//go:build unix && !linux && !darwin && !netbsd

package daemon

import "golang.org/x/sys/unix"

// bootClock is CLOCK_MONOTONIC in milliseconds. anet supports Linux and
// macOS (bootclock_linux.go, bootclock_darwin.go); on the other systems it
// builds for, this clock may stop while the machine sleeps, which the gate
// takes for a wall clock jump: deletions wait longer, never happen early.
func bootClock() uint64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		noteBootClockFailure(err)
		return 0
	}
	return uint64(ts.Sec)*1000 + uint64(ts.Nsec)/1_000_000
}
