//go:build linux

package daemon

import "golang.org/x/sys/unix"

// bootClock is CLOCK_BOOTTIME in milliseconds: monotonic, and counting
// while the machine is suspended (FS-K13a).
func bootClock() uint64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		noteBootClockFailure(err)
		return 0
	}
	return uint64(ts.Sec)*1000 + uint64(ts.Nsec)/1_000_000
}
