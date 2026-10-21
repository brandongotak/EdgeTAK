package health

import (
	"math"
	"testing"
	"time"
)

func TestRatesAndWindow(t *testing.T) {
	var in, out uint64
	s := &Sampler{DataDir: t.TempDir(), Src: Sources{
		Clients: func() int { return 3 },
		CoT:     func() (uint64, uint64, uint64) { return in, out, 0 },
	}}
	s.sample() // primes the counters; no sample yet
	if _, ok := s.Latest(); ok {
		t.Fatal("first call should only prime the counters")
	}
	// Pretend 10 s passed and 50 events came in, 200 went out.
	s.mu.Lock()
	s.prev.at = s.prev.at.Add(-10 * time.Second)
	s.mu.Unlock()
	in, out = 50, 200
	s.sample()
	l, ok := s.Latest()
	if !ok || l.Clients != 3 || math.Abs(l.CoTIn-5) > 0.1 || math.Abs(l.CoTOut-20) > 0.5 {
		t.Fatalf("latest = %+v", l)
	}
	// A counter going backwards (a stream ended) is a zero rate, not negative.
	s.mu.Lock()
	s.prev.at = s.prev.at.Add(-10 * time.Second)
	s.mu.Unlock()
	in = 10
	s.sample()
	if l, _ := s.Latest(); l.CoTIn != 0 {
		t.Fatalf("rate after reset = %v", l.CoTIn)
	}
	// Downsampling averages rates into buckets.
	s.mu.Lock()
	s.samples = nil
	for i := 0; i < 100; i++ {
		s.samples = append(s.samples, Sample{T: int64(i) * 10_000, CoTIn: float64(i % 2 * 10), CPU: -1})
	}
	s.mu.Unlock()
	got := s.Samples(time.UnixMilli(0), 10)
	if len(got) != 10 || got[0].CoTIn != 5 || got[0].CPU != -1 {
		t.Fatalf("downsampled = %d points, first %+v", len(got), got[0])
	}
}

func TestBatteryDrain(t *testing.T) {
	s := &Sampler{}
	start := time.Now().Add(-time.Hour)
	for i := 0; i <= 12; i++ { // 80% → 68% over an hour
		s.RecordBattery("dev", 80-i, start.Add(time.Duration(i)*5*time.Minute))
	}
	s.RecordBattery("dev", 68, start.Add(61*time.Minute)) // unchanged within 5 min: not stored
	pts, drain := s.Battery("dev")
	if len(pts) != 13 || math.Abs(drain-12) > 0.5 {
		t.Fatalf("points %d, drain %.2f %%/h", len(pts), drain)
	}
	if _, d := s.Battery("unknown"); d != 0 {
		t.Fatal("drain for an unknown device")
	}
	s.RecordBattery("dev2", 50, time.Now())
	if _, d := s.Battery("dev2"); d != 0 {
		t.Fatal("drain from a single reading")
	}
}
