//go:build !windows

package extension

import (
	"os/exec"
	"syscall"
)

// An extension runs in its own process group, so stopping it also stops
// the processes it started (a Python program's workers, say).
func setProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func interrupt(cmd *exec.Cmd) { syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }

func kill(cmd *exec.Cmd) { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
