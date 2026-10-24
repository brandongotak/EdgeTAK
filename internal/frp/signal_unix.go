//go:build !windows

package frp

import (
	"os"
	"syscall"
)

// interrupt asks frpc to shut down gracefully.
func interrupt(p *os.Process) { p.Signal(syscall.SIGTERM) }
