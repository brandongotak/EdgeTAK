//go:build windows

package frp

import "os"

// interrupt stops frpc; Windows has no SIGTERM for child processes.
func interrupt(p *os.Process) { p.Kill() }
