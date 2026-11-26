package xmpp

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// stopLeftover ends a Prosody that an earlier EdgeTAK started with this
// configuration and that is still running (it would hold the ports and the
// pidfile, and the new one would refuse to start).
func stopLeftover(pidfile, config string) {
	b, err := os.ReadFile(pidfile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || !strings.Contains(string(cmdline), config) {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	p.Signal(syscall.SIGTERM)
	for i := 0; i < 50; i++ {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.Kill()
}
