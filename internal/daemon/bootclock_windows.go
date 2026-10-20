//go:build windows

package daemon

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// procQueryInterruptTime is QueryInterruptTime (Windows 10 and Windows Server 2016 or later), looked up
// when it is first called. It is exported by KernelBase.dll; kernel32.dll does not forward it.
var procQueryInterruptTime = windows.NewLazySystemDLL("kernelbase.dll").NewProc("QueryInterruptTime")

// bootClock is the interrupt time in milliseconds: the time since boot in 100 ns units, which keeps
// counting while the machine sleeps or hibernates (it is the biased interrupt time; the unbiased one
// excludes sleep), as FS-K13a needs. When the function cannot be found or called, the failure is noted
// once and bootClock answers 0, which the gate takes as a clock that does not move: it deletes nothing
// that depends on the local clock.
func bootClock() uint64 {
	if err := procQueryInterruptTime.Find(); err != nil {
		noteBootClockFailure(err)
		return 0
	}
	var t uint64
	// QueryInterruptTime returns nothing; it writes the 100 ns count through the pointer.
	_, _, _ = procQueryInterruptTime.Call(uintptr(unsafe.Pointer(&t)))
	return t / 10_000
}
