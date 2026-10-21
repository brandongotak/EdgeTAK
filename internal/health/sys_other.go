//go:build !linux

package health

// On systems other than Linux only EdgeTAK's own figures are measured.

type sysReading struct {
	ok                  bool
	cpuTotal, cpuIdle   uint64
	memUsed, memTotal   uint64
	diskUsed, diskTotal uint64
	netRx, netTx        uint64
	load1, tempC        float64
	procTicks           uint64
	procRSS             uint64
}

func readSystem(string) sysReading { return sysReading{load1: -1, tempC: -1} }

func systemUptime() float64 { return 0 }
