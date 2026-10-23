//go:build linux || darwin || freebsd || openbsd || netbsd

package runner

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func runProcess(ctx context.Context, cmd *exec.Cmd, mode string) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	err := cmd.Wait()
	// Drain descendants that kept inherited output pipes or outlived the command.
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return err
}
