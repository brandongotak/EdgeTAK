//go:build windows

package xmpp

import "os"

// interrupt stops MediaMTX; Windows has no SIGTERM for child processes.
func interrupt(p *os.Process) { p.Kill() }
