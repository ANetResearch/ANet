// Package procgroup runs a command as the root of a process tree that is killed as a whole when
// the command's context ends: a timeout, a cancellation or a caller's own stop.
//
// On unix the tree is a process group: the command starts with Setpgid, and cancellation sends
// SIGKILL to the whole group. On Windows the tree is a Job object: the command starts suspended,
// is assigned to a job that kills every process in it when its last handle closes, and only then
// resumes, so no descendant can exist outside the job. Cancellation terminates the job. When the
// command ends normally, Wait releases the job, so a process the command deliberately left running
// survives, as a background process survives its shell on unix.
//
// It imports only the standard library and, in its Windows file, golang.org/x/sys/windows.
package procgroup

import "os/exec"

// Group is the process tree rooted at one started command.
type Group struct {
	cmd *exec.Cmd
	sys sysGroup
}

// Start starts cmd as the root of a tree that is killed as a whole when cmd's context ends (it sets
// cmd.Cancel, overwriting it) and returns its Group. It sets cmd.SysProcAttr, creating it when nil
// and keeping fields already set: unix Setpgid = true; windows CreationFlags |= CREATE_SUSPENDED|
// CREATE_NEW_PROCESS_GROUP|CREATE_NO_WINDOW. cmd.WaitDelay is the caller's.
// windows: a Job object (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, no breakaway) is created before
// cmd.Start; after Start the process is opened (PROCESS_SET_QUOTA|PROCESS_TERMINATE|
// PROCESS_SUSPEND_RESUME), assigned, and resumed with ntdll NtResumeProcess. Any failure after Start
// kills the process, waits for it, closes the job and returns an error: nothing is left running.
// Cancel = TerminateJobObject(job, 1) and cmd.Process.Kill() (the latter covers a cancel that races
// the assignment).
// unix: Cancel = syscall.Kill(-pid, SIGKILL) — the current code, verbatim.
func Start(cmd *exec.Cmd) (*Group, error) {
	g := &Group{cmd: cmd}
	if err := g.start(); err != nil {
		return nil, err
	}
	return g, nil
}

// Wait is cmd.Wait; then, on windows, the job is released: KILL_ON_JOB_CLOSE is cleared and the
// handle closed, so a process the command left running survives, as on unix. Call exactly once.
func (g *Group) Wait() error {
	return g.wait()
}

// Run is Start followed by Wait.
func Run(cmd *exec.Cmd) error {
	g, err := Start(cmd)
	if err != nil {
		return err
	}
	return g.Wait()
}
