//go:build !windows

package agents

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes the child lead its own process group and kills the whole group on
// cancel, so agent-spawned children (shells, test runners) don't outlive a timeout.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
