//go:build netbsd

package daemon

import "golang.org/x/sys/unix"

// clockMonotonicNetBSD is NetBSD's CLOCK_MONOTONIC, which x/sys/unix does
// not name.
const clockMonotonicNetBSD = 3

// bootClock is CLOCK_MONOTONIC in milliseconds; see bootclock_other.go.
func bootClock() uint64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(clockMonotonicNetBSD, &ts); err != nil {
		noteBootClockFailure(err)
		return 0
	}
	return uint64(ts.Sec)*1000 + uint64(ts.Nsec)/1_000_000
}
