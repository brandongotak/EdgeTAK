// Package sim simulates TAK devices for load testing: each one connects to
// a server's streaming port (TLS or plain TCP) like ATAK, walks around a
// centre point sending position reports, optionally sends chat, and reads
// everything the server relays. One device's reports carry a timestamp so
// receivers can measure how long the server takes to relay them.
package sim

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Config describes a simulation run.
type Config struct {
	Addr      string        // the server's streaming port, host:port
	TLS       *tls.Config   // nil for plain TCP
	Devices   int           // how many devices
	Ramp      float64       // devices connecting per second (default 50)
	Interval  time.Duration // between position reports (default 5 s)
	ChatEvery time.Duration // between each device's chat messages (0 = none)
	Duration  time.Duration // how long to run (0 = until stopped)
	Center    [2]float64    // lat, lon the devices walk around
	Radius    float64       // metres (default 2000)
	Prefix    string        // callsign prefix (default SIM)
	Version   string        // reported in the devices' takv
	// Dial connects (tests use an in-memory network); default net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Stats is a snapshot of a run.
type Stats struct {
	Running        bool      `json:"running"`
	Started        time.Time `json:"started"`
	Ended          time.Time `json:"ended,omitzero"`
	Elapsed        float64   `json:"elapsed"` // seconds
	Devices        int       `json:"devices"` // target
	Connected      int       `json:"connected"`
	Connects       uint64    `json:"connects"` // successful connections, reconnects included
	Failed         uint64    `json:"failed"`   // connection attempts that failed
	Dropped        uint64    `json:"dropped"`  // connections the server closed
	Sent           uint64    `json:"sent"`     // events
	Received       uint64    `json:"received"` // events
	BytesOut       uint64    `json:"bytes_out"`
	BytesIn        uint64    `json:"bytes_in"`
	SentPerSec     float64   `json:"sent_per_sec"`
	ReceivedPerSec float64   `json:"received_per_sec"`
	LatencyP50     float64   `json:"latency_p50_ms"`
	LatencyP95     float64   `json:"latency_p95_ms"`
	LatencyMax     float64   `json:"latency_max_ms"`
	Samples        int       `json:"latency_samples"`
	LastError      string    `json:"last_error,omitempty"`
}

// Run is a simulation in progress.
type Run struct {
	cfg     Config
	id      string
	cancel  context.CancelFunc
	done    chan struct{}
	started time.Time

	connected                    atomic.Int64
	connects, failed, dropped    atomic.Uint64
	sent, received, bytesOut, in atomic.Uint64

	mu      sync.Mutex
	ended   time.Time
	lastErr string
	latency []float64 // ms, the most recent ones
	rates   [][2]uint64
}

const (
	probeMarker  = "edgetak-sim-probe:"
	maxSamples   = 2000
	probeReaders = 5 // devices that time the probe's reports
	rateWindow   = 10
)

var teams = []string{"Cyan", "Yellow", "Green", "Red", "Blue", "Orange", "Magenta", "White"}

// Start begins a run; it stops when ctx ends, Stop is called or the
// duration is up.
func Start(ctx context.Context, cfg Config) (*Run, error) {
	if cfg.Addr == "" || cfg.Devices < 1 {
		return nil, errors.New("a server address and at least one device are needed")
	}
	if cfg.Ramp <= 0 {
		cfg.Ramp = 50
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Radius <= 0 {
		cfg.Radius = 2000
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "SIM"
	}
	if cfg.Dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		cfg.Dial = d.DialContext
	}
	ctx, cancel := context.WithCancel(ctx)
	if cfg.Duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, cfg.Duration)
		cancelRun := cancel
		cancel = func() { stop(); cancelRun() }
	}
	r := &Run{cfg: cfg, id: strconv.FormatInt(time.Now().Unix()%100000, 36), cancel: cancel, done: make(chan struct{}), started: time.Now()}
	go r.run(ctx)
	return r, nil
}

// Stop ends the run and waits for its devices to disconnect.
func (r *Run) Stop() {
	r.cancel()
	<-r.done
}

// Done is closed when the run has ended.
func (r *Run) Done() <-chan struct{} { return r.done }

func (r *Run) run(ctx context.Context) {
	defer close(r.done)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.measure(ctx) }()
	// Devices join at the ramp rate.
	gap := time.Duration(float64(time.Second) / r.cfg.Ramp)
	for i := 0; i < r.cfg.Devices && ctx.Err() == nil; i++ {
		wg.Add(1)
		go func(n int) { defer wg.Done(); r.device(ctx, n) }(i)
		select {
		case <-ctx.Done():
		case <-time.After(gap):
		}
	}
	<-ctx.Done()
	wg.Wait()
	r.mu.Lock()
	r.ended = time.Now()
	r.mu.Unlock()
}

// measure keeps the counters of the last seconds, for rates.
func (r *Run) measure(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		r.mu.Lock()
		r.rates = append(r.rates, [2]uint64{r.sent.Load(), r.received.Load()})
		if len(r.rates) > rateWindow+1 {
			r.rates = r.rates[1:]
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Run) fail(err error) {
	r.mu.Lock()
	r.lastErr = err.Error()
	r.mu.Unlock()
}

// device keeps one simulated device connected until the run ends,
// reconnecting after a drop.
func (r *Run) device(ctx context.Context, n int) {
	d := &walker{
		uid:      fmt.Sprintf("EdgeTAK-SIM-%s-%04d", r.id, n+1),
		callsign: fmt.Sprintf("%s-%04d", r.cfg.Prefix, n+1),
		team:     teams[n%len(teams)],
		rng:      rand.New(rand.NewPCG(uint64(n)+1, uint64(time.Now().UnixNano()))),
	}
	d.place(r.cfg.Center, r.cfg.Radius)
	for ctx.Err() == nil {
		err := r.session(ctx, d, n)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			r.fail(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2*time.Second + time.Duration(d.rng.IntN(1000))*time.Millisecond):
		}
	}
}

// session is one connection: it reports until the run ends or the
// connection fails.
func (r *Run) session(ctx context.Context, d *walker, n int) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, err := r.cfg.Dial(dialCtx, "tcp", r.cfg.Addr)
	if err == nil && r.cfg.TLS != nil {
		tc := tls.Client(conn, r.cfg.TLS)
		if err = tc.HandshakeContext(dialCtx); err != nil {
			conn.Close()
		}
		conn = tc
	}
	cancel()
	if err != nil {
		r.failed.Add(1)
		return fmt.Errorf("connect: %w", err)
	}
	r.connects.Add(1)
	r.connected.Add(1)
	defer r.connected.Add(-1)
	readErr := make(chan error, 1)
	go func() { readErr <- r.read(conn, n > 0 && n <= probeReaders) }()
	defer conn.Close()

	send := func(b []byte) error {
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := conn.Write(b); err != nil {
			return err
		}
		r.sent.Add(1)
		r.bytesOut.Add(uint64(len(b)))
		return nil
	}
	// Spread the devices' reports over the interval.
	pos := time.NewTimer(time.Duration(d.rng.Int64N(int64(r.cfg.Interval))))
	defer pos.Stop()
	var chat <-chan time.Time
	if r.cfg.ChatEvery > 0 {
		t := time.NewTicker(r.cfg.ChatEvery + time.Duration(d.rng.Int64N(int64(r.cfg.ChatEvery))))
		defer t.Stop()
		chat = t.C
	}
	if err := send(d.sa(r.cfg, n == 0)); err != nil {
		r.dropped.Add(1)
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			r.dropped.Add(1)
			return fmt.Errorf("server closed the connection: %w", err)
		case <-pos.C:
			d.step(r.cfg.Center, r.cfg.Radius, r.cfg.Interval)
			if err := send(d.sa(r.cfg, n == 0)); err != nil {
				r.dropped.Add(1)
				return err
			}
			pos.Reset(r.cfg.Interval)
		case <-chat:
			if err := send(d.chat()); err != nil {
				r.dropped.Add(1)
				return err
			}
		}
	}
}

// read counts what the server sends; probe readers also time the probe
// device's reports.
func (r *Run) read(conn net.Conn, probe bool) error {
	buf := make([]byte, 64<<10)
	var tail []byte // the end of the last read, for markers split across reads
	end := []byte("</event>")
	marker := []byte(probeMarker)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			chunk := append(tail, buf[:n]...)
			r.in.Add(uint64(n))
			r.received.Add(uint64(bytes.Count(chunk, end) - bytes.Count(tail, end)))
			if probe {
				r.probe(chunk, marker)
			}
			keep := min(len(chunk), 48)
			tail = append(tail[:0], chunk[len(chunk)-keep:]...)
		}
		if err != nil {
			return err
		}
	}
}

func (r *Run) probe(chunk, marker []byte) {
	now := time.Now().UnixNano()
	for {
		i := bytes.Index(chunk, marker)
		if i < 0 {
			return
		}
		chunk = chunk[i+len(marker):]
		j := bytes.IndexByte(chunk, '<')
		if j < 0 {
			return
		}
		sent, err := strconv.ParseInt(string(chunk[:j]), 10, 64)
		chunk = chunk[j:]
		if err != nil || sent > now {
			continue
		}
		r.mu.Lock()
		r.latency = append(r.latency, float64(now-sent)/1e6)
		if len(r.latency) > maxSamples {
			r.latency = r.latency[len(r.latency)-maxSamples:]
		}
		r.mu.Unlock()
	}
}

// Stats is a snapshot of the run so far.
func (r *Run) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Stats{Running: r.ended.IsZero(), Started: r.started, Ended: r.ended, Devices: r.cfg.Devices,
		Connected: int(r.connected.Load()), Connects: r.connects.Load(), Failed: r.failed.Load(), Dropped: r.dropped.Load(),
		Sent: r.sent.Load(), Received: r.received.Load(), BytesOut: r.bytesOut.Load(), BytesIn: r.in.Load(), LastError: r.lastErr}
	end := r.ended
	if end.IsZero() {
		end = time.Now()
	}
	st.Elapsed = math.Round(end.Sub(r.started).Seconds()*10) / 10
	if st.Running && len(r.rates) > 1 {
		first, last := r.rates[0], r.rates[len(r.rates)-1]
		secs := float64(len(r.rates) - 1)
		st.SentPerSec = math.Round(float64(last[0]-first[0])/secs*10) / 10
		st.ReceivedPerSec = math.Round(float64(last[1]-first[1])/secs*10) / 10
	}
	if n := len(r.latency); n > 0 {
		s := slices.Clone(r.latency)
		slices.Sort(s)
		pct := func(p float64) float64 { return math.Round(s[min(n-1, int(p*float64(n)))]*10) / 10 }
		st.LatencyP50, st.LatencyP95, st.LatencyMax, st.Samples = pct(0.5), pct(0.95), math.Round(s[n-1]*10)/10, n
	}
	return st
}

// ---- a walking device ----

type walker struct {
	uid, callsign, team string
	lat, lon            float64
	heading, speed      float64 // degrees, m/s
	rng                 *rand.Rand
	seq                 int
}

const metresPerDegree = 111_320.0

func (d *walker) place(center [2]float64, radius float64) {
	r, a := radius*math.Sqrt(d.rng.Float64()), d.rng.Float64()*2*math.Pi
	d.lat = center[0] + r*math.Cos(a)/metresPerDegree
	d.lon = center[1] + r*math.Sin(a)/(metresPerDegree*math.Cos(center[0]*math.Pi/180))
	d.heading = d.rng.Float64() * 360
	d.speed = 0.5 + d.rng.Float64()*4 // walking to jogging; some faster below
	if d.rng.IntN(5) == 0 {
		d.speed = 5 + d.rng.Float64()*20 // vehicles
	}
}

// step moves the device along its heading, turning a little, and back
// towards the centre when it strays outside the radius.
func (d *walker) step(center [2]float64, radius float64, dt time.Duration) {
	d.heading = math.Mod(d.heading+d.rng.NormFloat64()*20+360, 360)
	dy := (d.lat - center[0]) * metresPerDegree
	dx := (d.lon - center[1]) * metresPerDegree * math.Cos(center[0]*math.Pi/180)
	if math.Hypot(dx, dy) > radius {
		d.heading = math.Mod(math.Atan2(-dx, -dy)*180/math.Pi+360, 360)
	}
	dist := d.speed * dt.Seconds()
	rad := d.heading * math.Pi / 180
	d.lat += dist * math.Cos(rad) / metresPerDegree
	d.lon += dist * math.Sin(rad) / (metresPerDegree * math.Cos(d.lat*math.Pi/180))
}

func cotTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// sa is the device's position report, as ATAK sends it.
func (d *walker) sa(cfg Config, probe bool) []byte {
	now := time.Now()
	remarks := ""
	if probe {
		remarks = "<remarks>" + probeMarker + strconv.FormatInt(now.UnixNano(), 10) + "</remarks>"
	}
	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><event version="2.0" uid="%s" type="a-f-G-U-C" how="m-g" time="%s" start="%s" stale="%s">`+
		`<point lat="%.7f" lon="%.7f" hae="1600" ce="9.9" le="9999999"/><detail><contact callsign="%s" endpoint="*:-1:stcp"/>`+
		`<__group name="%s" role="Team Member"/><takv device="EdgeTAK simulator" platform="EdgeTAK-Sim" os="sim" version="%s"/>`+
		`<track course="%.1f" speed="%.1f"/><status battery="%d"/><uid Droid="%s"/>%s</detail></event>`,
		d.uid, cotTime(now), cotTime(now), cotTime(now.Add(3*cfg.Interval+30*time.Second)), d.lat, d.lon, d.callsign,
		d.team, cfg.Version, d.heading, d.speed, 40+d.rng.IntN(60), d.callsign, remarks)
}

// chat is a message to All Chat Rooms.
func (d *walker) chat() []byte {
	d.seq++
	now := time.Now()
	id := fmt.Sprintf("%s-chat-%d", d.uid, d.seq)
	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><event version="2.0" uid="GeoChat.%s.All Chat Rooms.%s" type="b-t-f" how="h-g-i-g-o" time="%s" start="%s" stale="%s">`+
		`<point lat="%.7f" lon="%.7f" hae="0" ce="9999999" le="9999999"/><detail><__chat parent="RootContactGroup" groupOwner="false" messageId="%s" chatroom="All Chat Rooms" id="All Chat Rooms" senderCallsign="%s">`+
		`<chatgrp uid0="%s" uid1="All Chat Rooms" id="All Chat Rooms"/></__chat><link uid="%s" type="a-f-G-U-C" relation="p-p"/>`+
		`<remarks source="BAO.F.ATAK.%s" to="All Chat Rooms" time="%s">Simulated message %d from %s</remarks></detail></event>`,
		d.uid, id, cotTime(now), cotTime(now), cotTime(now.Add(24*time.Hour)), d.lat, d.lon, id, d.callsign, d.uid, d.uid, d.uid, cotTime(now), d.seq, d.callsign)
}
