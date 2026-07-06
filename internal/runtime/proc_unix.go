//go:build !windows

package runtime

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the child in its own process group and kills the whole
// group on cancel/timeout, so `sh -c "a | b &"` children don't outlive us.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
