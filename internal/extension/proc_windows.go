//go:build windows

package extension

import (
	"os/exec"
	"syscall"
)

// An extension runs without a console of its own (none pops up, and it
// doesn't depend on EdgeTAK having one, as a service doesn't), in its own
// process group.
func setProcessGroup(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow | syscall.CREATE_NEW_PROCESS_GROUP}
}

// interrupt stops the extension; Windows has no SIGTERM for child processes.
func interrupt(cmd *exec.Cmd) { cmd.Process.Kill() }

func kill(cmd *exec.Cmd) { cmd.Process.Kill() }
