//go:build windows

package procgroup

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Process creation flags and process access rights (winbase.h, winnt.h).
const (
	createSuspended       = 0x00000004
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000

	processTerminate     = 0x0001
	processSetQuota      = 0x0100
	processSuspendResume = 0x0800
)

// ntResumeProcess resumes every thread of a process. The root starts suspended because
// os.StartProcess does not return its main thread's handle.
var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// openJobs counts the job handles this package holds; the tests check that none leaks.
var openJobs atomic.Int64

// sysGroup is the Job object holding the tree.
type sysGroup struct {
	mu sync.Mutex
	// job is the job handle; 0 once closed.
	job windows.Handle
	// cancelled records that Cancel ran: the job is then never released.
	cancelled bool
}

func (g *Group) start() error {
	cmd := g.cmd
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createSuspended | createNewProcessGroup | createNoWindow
	job, err := newKillJob()
	if err != nil {
		return err
	}
	g.sys.job = job
	cmd.Cancel = g.cancel
	if err := cmd.Start(); err != nil {
		g.closeJob(false)
		return err
	}
	if err := g.adopt(); err != nil {
		// Nothing may be left running: the root is still suspended, or already in the job.
		cmd.Process.Kill()
		cmd.Wait()
		g.closeJob(false)
		return err
	}
	return nil
}

// newKillJob creates a job object whose processes are killed when its last handle closes.
func newKillJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		if job != 0 {
			windows.CloseHandle(job)
		}
		return 0, fmt.Errorf("procgroup: create a job object: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("procgroup: set the limits of a job object: %w", err)
	}
	openJobs.Add(1)
	return job, nil
}

// adopt assigns the suspended root to the job and resumes it.
func (g *Group) adopt() error {
	pid := g.cmd.Process.Pid
	if err := ntResumeProcess.Find(); err != nil {
		return fmt.Errorf("procgroup: find NtResumeProcess: %w", err)
	}
	h, err := windows.OpenProcess(processSetQuota|processTerminate|processSuspendResume, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("procgroup: open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	g.sys.mu.Lock()
	job := g.sys.job
	g.sys.mu.Unlock()
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return fmt.Errorf("procgroup: assign process %d to its job object: %w", pid, err)
	}
	if r, _, _ := ntResumeProcess.Call(uintptr(h)); r != 0 {
		return fmt.Errorf("procgroup: resume process %d: %w", pid, windows.NTStatus(r))
	}
	return nil
}

var errJobClosed = errors.New("procgroup: the job object is closed")

// cancel is cmd.Cancel: it terminates every process in the job, and the root itself in case the
// cancellation came before the root was assigned to the job.
func (g *Group) cancel() error {
	g.sys.mu.Lock()
	g.sys.cancelled = true
	jerr := errJobClosed
	if g.sys.job != 0 {
		jerr = windows.TerminateJobObject(g.sys.job, 1)
	}
	g.sys.mu.Unlock()
	var kerr error = errJobClosed
	if g.cmd.Process != nil {
		kerr = g.cmd.Process.Kill()
	}
	if jerr == nil {
		return nil
	}
	return kerr
}

// closeJob closes the job handle once. With release, after a command that was not cancelled,
// KILL_ON_JOB_CLOSE is cleared first, so processes the command left running survive. Otherwise
// the remaining processes are terminated and the handle close kills anything that is left.
func (g *Group) closeJob(release bool) {
	g.sys.mu.Lock()
	defer g.sys.mu.Unlock()
	job := g.sys.job
	if job == 0 {
		return
	}
	if release && !g.sys.cancelled {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	} else if g.sys.cancelled {
		windows.TerminateJobObject(job, 1)
	}
	windows.CloseHandle(job)
	g.sys.job = 0
	openJobs.Add(-1)
}

func (g *Group) wait() error {
	err := g.cmd.Wait()
	g.closeJob(true)
	return err
}
