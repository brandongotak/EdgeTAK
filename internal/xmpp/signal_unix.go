//go:build !windows

package xmpp

import (
	"os"
	"syscall"
)

// interrupt asks Prosody to shut down gracefully.
func interrupt(p *os.Process) { p.Signal(syscall.SIGTERM) }
