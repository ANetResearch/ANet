//go:build unix

package procgroup

import "syscall"

// sysGroup holds nothing on unix: the group is named by the root's pid.
type sysGroup struct{}

func (g *Group) start() error {
	cmd := g.cmd
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // negative pid: the whole group
	}
	return cmd.Start()
}

func (g *Group) wait() error {
	return g.cmd.Wait()
}
