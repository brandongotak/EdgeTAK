//go:build !windows

package mediamtx

import (
	"os"
	"syscall"
)

// interrupt asks MediaMTX to shut down gracefully.
func interrupt(p *os.Process) { p.Signal(syscall.SIGTERM) }
