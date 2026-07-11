//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// configureBashCancel makes cancellation kill the entire process group, so
// grandchildren (compilers, test binaries) die with the shell.
func configureBashCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
}
