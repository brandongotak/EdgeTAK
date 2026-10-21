//go:build linux

package health

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

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

func readSystem(dataDir string) sysReading {
	r := sysReading{load1: -1, tempC: -1}
	// CPU: the "cpu" line of /proc/stat, in ticks.
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		line, _, _ := strings.Cut(string(b), "\n")
		f := strings.Fields(line)
		if len(f) > 5 && f[0] == "cpu" {
			for i, v := range f[1:] {
				n, _ := strconv.ParseUint(v, 10, 64)
				if i < 8 { // user nice system idle iowait irq softirq steal (guest is included in user)
					r.cpuTotal += n
				}
				if i == 3 || i == 4 {
					r.cpuIdle += n
				}
			}
			r.ok = true
		}
	}
	// Memory: used = total - available.
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		var avail uint64
		for sc.Scan() {
			k, v, _ := strings.Cut(sc.Text(), ":")
			n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			switch k {
			case "MemTotal":
				r.memTotal = n * 1024
			case "MemAvailable":
				avail = n * 1024
			}
		}
		f.Close()
		if r.memTotal >= avail {
			r.memUsed = r.memTotal - avail
		}
	}
	// Disk holding the data directory.
	var st syscall.Statfs_t
	if dataDir != "" && syscall.Statfs(dataDir, &st) == nil {
		bs := uint64(st.Bsize)
		r.diskTotal = st.Blocks * bs
		r.diskUsed = (st.Blocks - st.Bfree) * bs
	}
	// Network: all interfaces except loopback.
	if b, err := os.ReadFile("/proc/net/dev"); err == nil {
		for _, line := range strings.Split(string(b), "\n")[2:] {
			name, rest, ok := strings.Cut(line, ":")
			if !ok || strings.TrimSpace(name) == "lo" {
				continue
			}
			f := strings.Fields(rest)
			if len(f) >= 9 {
				rx, _ := strconv.ParseUint(f[0], 10, 64)
				tx, _ := strconv.ParseUint(f[8], 10, 64)
				r.netRx += rx
				r.netTx += tx
			}
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			r.load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	// Temperature: the warmest thermal zone (the SoC on a Raspberry Pi).
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	for _, z := range zones {
		if b, err := os.ReadFile(z); err == nil {
			if milli, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil && milli > 0 {
				if c := milli / 1000; c > r.tempC {
					r.tempC = c
				}
			}
		}
	}
	// This process: CPU ticks (utime + stime) and resident memory.
	if b, err := os.ReadFile("/proc/self/stat"); err == nil {
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i > 0 {
			f := strings.Fields(s[i+1:])
			if len(f) > 12 {
				u, _ := strconv.ParseUint(f[11], 10, 64)
				k, _ := strconv.ParseUint(f[12], 10, 64)
				r.procTicks = u + k
			}
		}
	}
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
				n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
				r.procRSS = n * 1024
			}
		}
	}
	return r
}

func systemUptime() float64 {
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			v, _ := strconv.ParseFloat(f[0], 64)
			return v
		}
	}
	return 0
}
