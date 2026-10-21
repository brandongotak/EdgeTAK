// Package health samples server and device health for the admin console:
// system CPU, memory, disk, network and temperature (on Linux, from /proc),
// EdgeTAK's own load, CoT and video traffic, and each device's battery.
// Samples are kept in memory for 24 hours.
package health

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	Interval   = 10 * time.Second
	keep       = 24 * time.Hour
	maxSamples = int(keep / Interval)
)

// Sample is one measurement. Rates are per second over the last interval;
// -1 means "not available on this system".
type Sample struct {
	T          int64   `json:"t"` // Unix ms
	CPU        float64 `json:"cpu"`
	MemUsed    uint64  `json:"mem_used"`
	MemTotal   uint64  `json:"mem_total"`
	DiskUsed   uint64  `json:"disk_used"`
	DiskTotal  uint64  `json:"disk_total"`
	NetRx      float64 `json:"net_rx"` // bytes/s
	NetTx      float64 `json:"net_tx"`
	Load1      float64 `json:"load1"`
	TempC      float64 `json:"temp_c"`
	ProcCPU    float64 `json:"proc_cpu"` // EdgeTAK's share of all CPUs, %
	ProcRSS    uint64  `json:"proc_rss"`
	Goroutines int     `json:"goroutines"`
	Clients    int     `json:"clients"`
	CoTIn      float64 `json:"cot_in"` // events/s
	CoTOut     float64 `json:"cot_out"`
	Dropped    float64 `json:"dropped"`
	VideoIn    float64 `json:"video_in"` // bytes/s
	VideoOut   float64 `json:"video_out"`
}

// Sources are the EdgeTAK counters the sampler reads.
type Sources struct {
	Clients func() int
	CoT     func() (in, out, dropped uint64) // totals since start
	Video   func() (in, out uint64)          // byte totals of live streams
}

// Sampler collects samples every Interval.
type Sampler struct {
	DataDir string
	Src     Sources

	mu      sync.Mutex
	samples []Sample
	prev    counters
	havePrv bool
	battery map[string][]BatteryPoint
}

type counters struct {
	at                    time.Time
	cpuTotal, cpuIdle     uint64
	netRx, netTx          uint64
	procTicks             uint64
	cotIn, cotOut, cotDrp uint64
	vidIn, vidOut         uint64
}

// Run samples until ctx is done.
func (s *Sampler) Run(ctx context.Context) {
	s.sample()
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sample()
		}
	}
}

func (s *Sampler) sample() {
	now := time.Now()
	c := counters{at: now}
	sm := Sample{T: now.UnixMilli(), CPU: -1, TempC: -1, Load1: -1, NetRx: -1, NetTx: -1, ProcCPU: -1,
		Goroutines: runtime.NumGoroutine()}

	sys := readSystem(s.DataDir)
	c.cpuTotal, c.cpuIdle, c.netRx, c.netTx, c.procTicks = sys.cpuTotal, sys.cpuIdle, sys.netRx, sys.netTx, sys.procTicks
	sm.MemUsed, sm.MemTotal, sm.DiskUsed, sm.DiskTotal = sys.memUsed, sys.memTotal, sys.diskUsed, sys.diskTotal
	sm.Load1, sm.TempC, sm.ProcRSS = sys.load1, sys.tempC, sys.procRSS
	if sm.ProcRSS == 0 {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		sm.ProcRSS = ms.Sys
	}
	if s.Src.Clients != nil {
		sm.Clients = s.Src.Clients()
	}
	if s.Src.CoT != nil {
		c.cotIn, c.cotOut, c.cotDrp = s.Src.CoT()
	}
	if s.Src.Video != nil {
		c.vidIn, c.vidOut = s.Src.Video()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.havePrv {
		p := s.prev
		dt := now.Sub(p.at).Seconds()
		rate := func(cur, old uint64) float64 {
			if dt <= 0 || cur < old {
				return 0 // a counter reset (stream ended, interface changed)
			}
			return float64(cur-old) / dt
		}
		if sys.ok {
			if d := c.cpuTotal - p.cpuTotal; c.cpuTotal > p.cpuTotal {
				sm.CPU = 100 * (1 - float64(c.cpuIdle-p.cpuIdle)/float64(d))
				// /proc/stat counts in USER_HZ ticks over all CPUs, like the process times.
				sm.ProcCPU = 100 * float64(c.procTicks-p.procTicks) / float64(d)
			}
			sm.NetRx, sm.NetTx = rate(c.netRx, p.netRx), rate(c.netTx, p.netTx)
		}
		sm.CoTIn, sm.CoTOut, sm.Dropped = rate(c.cotIn, p.cotIn), rate(c.cotOut, p.cotOut), rate(c.cotDrp, p.cotDrp)
		sm.VideoIn, sm.VideoOut = rate(c.vidIn, p.vidIn), rate(c.vidOut, p.vidOut)
		s.samples = append(s.samples, sm)
		if len(s.samples) > maxSamples {
			s.samples = s.samples[len(s.samples)-maxSamples:]
		}
	}
	s.prev, s.havePrv = c, true
}

// Samples returns the samples since the given time, averaged into at most
// points buckets.
func (s *Sampler) Samples(since time.Time, points int) []Sample {
	s.mu.Lock()
	var in []Sample
	from := since.UnixMilli()
	for _, sm := range s.samples {
		if sm.T >= from {
			in = append(in, sm)
		}
	}
	s.mu.Unlock()
	if points <= 0 || len(in) <= points {
		return in
	}
	out := make([]Sample, 0, points)
	per := float64(len(in)) / float64(points)
	for i := 0; i < points; i++ {
		lo, hi := int(float64(i)*per), int(float64(i+1)*per)
		if hi > len(in) {
			hi = len(in)
		}
		if lo >= hi {
			continue
		}
		out = append(out, average(in[lo:hi]))
	}
	return out
}

func average(b []Sample) Sample {
	n := float64(len(b))
	last := b[len(b)-1] // levels (memory, disk, clients) use the latest value
	avg := last
	var cpu, rx, tx, load, temp, pcpu, ci, co, dr, vi, vo float64
	for _, sm := range b {
		cpu += sm.CPU
		rx += sm.NetRx
		tx += sm.NetTx
		load += sm.Load1
		temp += sm.TempC
		pcpu += sm.ProcCPU
		ci += sm.CoTIn
		co += sm.CoTOut
		dr += sm.Dropped
		vi += sm.VideoIn
		vo += sm.VideoOut
	}
	avg.T = b[0].T + (last.T-b[0].T)/2
	keepUnknown := func(sum float64, v float64) float64 {
		if v < 0 {
			return -1
		}
		return sum / n
	}
	avg.CPU, avg.NetRx, avg.NetTx = keepUnknown(cpu, last.CPU), keepUnknown(rx, last.NetRx), keepUnknown(tx, last.NetTx)
	avg.Load1, avg.TempC, avg.ProcCPU = keepUnknown(load, last.Load1), keepUnknown(temp, last.TempC), keepUnknown(pcpu, last.ProcCPU)
	avg.CoTIn, avg.CoTOut, avg.Dropped = ci/n, co/n, dr/n
	avg.VideoIn, avg.VideoOut = vi/n, vo/n
	return avg
}

// Latest is the most recent sample, if any.
func (s *Sampler) Latest() (Sample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.samples) == 0 {
		return Sample{}, false
	}
	return s.samples[len(s.samples)-1], true
}

// ---- battery history per device ----

// BatteryPoint is a device's battery level at a time.
type BatteryPoint struct {
	T   int64 `json:"t"`
	Pct int   `json:"pct"`
}

// RecordBattery notes a device's battery level (kept when it changes, or
// every few minutes, for 24 hours).
func (s *Sampler) RecordBattery(uid string, pct int, at time.Time) {
	if uid == "" || pct < 0 || pct > 100 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.battery == nil {
		s.battery = map[string][]BatteryPoint{}
	}
	pts := s.battery[uid]
	ms := at.UnixMilli()
	if n := len(pts); n > 0 && pts[n-1].Pct == pct && ms-pts[n-1].T < (5*time.Minute).Milliseconds() {
		return
	}
	cut := ms - keep.Milliseconds()
	i := 0
	for i < len(pts) && pts[i].T < cut {
		i++
	}
	pts = append(pts[i:], BatteryPoint{T: ms, Pct: pct})
	s.battery[uid] = pts
	if len(s.battery) > 5000 { // bound memory if many short-lived UIDs appear
		for k, v := range s.battery {
			if len(v) == 0 || v[len(v)-1].T < cut {
				delete(s.battery, k)
			}
		}
	}
}

// Battery returns a device's battery history and its drain in percentage
// points per hour over the last hour (positive = draining; 0 if unknown).
func (s *Sampler) Battery(uid string) ([]BatteryPoint, float64) {
	s.mu.Lock()
	pts := append([]BatteryPoint(nil), s.battery[uid]...)
	s.mu.Unlock()
	if len(pts) < 2 {
		return pts, 0
	}
	last := pts[len(pts)-1]
	first := last
	for i := len(pts) - 1; i >= 0 && last.T-pts[i].T <= time.Hour.Milliseconds(); i-- {
		first = pts[i]
	}
	hours := float64(last.T-first.T) / float64(time.Hour.Milliseconds())
	if hours < 0.1 { // too short a span to estimate
		return pts, 0
	}
	return pts, float64(first.Pct-last.Pct) / hours
}

// ---- system information ----

// Info describes the machine.
type Info struct {
	Hostname string  `json:"hostname"`
	OS       string  `json:"os"`
	Arch     string  `json:"arch"`
	CPUs     int     `json:"cpus"`
	Model    string  `json:"model,omitempty"`  // e.g. Raspberry Pi 4 Model B
	Uptime   float64 `json:"uptime,omitempty"` // system uptime, seconds
}

func SystemInfo() Info {
	h, _ := os.Hostname()
	in := Info{Hostname: h, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU()}
	if b, err := os.ReadFile("/proc/device-tree/model"); err == nil {
		in.Model = strings.TrimRight(string(b), "\x00\n")
	}
	in.Uptime = systemUptime()
	return in
}
