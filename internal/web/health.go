package web

import (
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/health"
)

// Server and device health for the admin console's Health tab.

type issue struct {
	Level string `json:"level"` // info, warning, critical
	Text  string `json:"text"`
}

var levelRank = map[string]int{"critical": 3, "warning": 2, "info": 1}

func (s *Server) healthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/health", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		minutes, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
		if minutes <= 0 || minutes > 24*60 {
			minutes = 60
		}
		out := map[string]any{"info": health.SystemInfo(), "version": s.Version,
			"edgetak_uptime": int(time.Since(s.Started).Seconds()), "interval": health.Interval.Seconds(),
			"samples": []health.Sample{}, "issues": s.serverIssues()}
		if s.Health != nil {
			out["samples"] = s.Health.Samples(time.Now().Add(-time.Duration(minutes)*time.Minute), 240)
			if l, ok := s.Health.Latest(); ok {
				out["latest"] = l
			}
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("GET /admin/api/health/devices", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.deviceHealth())
	}))
}

// serverIssues are warnings about the server itself.
func (s *Server) serverIssues() []issue {
	out := s.certIssues(s.certReport())
	if s.Health == nil {
		return out
	}
	recent := s.Health.Samples(time.Now().Add(-5*time.Minute), 0)
	if len(recent) == 0 {
		return out
	}
	l := recent[len(recent)-1]
	pct := func(used, total uint64) float64 {
		if total == 0 {
			return 0
		}
		return 100 * float64(used) / float64(total)
	}
	switch d := pct(l.DiskUsed, l.DiskTotal); {
	case d >= 95:
		out = append(out, issue{"critical", fmt.Sprintf("Disk %.0f%% full: uploads, history and recordings will start failing", d)})
	case d >= 85:
		out = append(out, issue{"warning", fmt.Sprintf("Disk %.0f%% full", d)})
	}
	if m := pct(l.MemUsed, l.MemTotal); m >= 90 {
		out = append(out, issue{"warning", fmt.Sprintf("Memory %.0f%% used", m)})
	}
	switch {
	case l.TempC >= 80:
		out = append(out, issue{"critical", fmt.Sprintf("%.0f °C: the CPU is throttling; improve cooling", l.TempC)})
	case l.TempC >= 70:
		out = append(out, issue{"warning", fmt.Sprintf("%.0f °C: running hot", l.TempC)})
	}
	var cpu, dropped float64
	n := 0
	for _, sm := range recent {
		if sm.CPU >= 0 {
			cpu += sm.CPU
			n++
		}
		dropped += sm.Dropped * health.Interval.Seconds()
	}
	if n > 0 && cpu/float64(n) >= 90 {
		out = append(out, issue{"warning", fmt.Sprintf("CPU busy: %.0f%% over the last 5 minutes", cpu/float64(n))})
	}
	if dropped >= 1 {
		out = append(out, issue{"warning", fmt.Sprintf("%.0f messages dropped in the last 5 minutes: devices on slow links could not keep up", dropped)})
	}
	if s.Cfg.Video.Enabled {
		s.video.mu.Lock()
		apiErr := s.video.apiErr
		s.video.mu.Unlock()
		if apiErr != "" {
			out = append(out, issue{"critical", "MediaMTX is not reachable: video is down"})
		}
	}
	return out
}

type deviceHealthView struct {
	clientView
	OS          string                `json:"os,omitempty"`
	Version     string                `json:"version,omitempty"`
	GeoSource   string                `json:"geo_source,omitempty"`
	LastSeenAgo int                   `json:"last_seen_ago"` // seconds
	BatteryHist []health.BatteryPoint `json:"battery_history,omitempty"`
	DrainPerHr  float64               `json:"drain_per_hour,omitempty"`
	Issues      []issue               `json:"issues"`
	Worst       string                `json:"worst"`
}

func (s *Server) deviceHealth() []deviceHealthView {
	out := []deviceHealthView{}
	now := time.Now()
	for _, c := range s.Router.Clients() {
		uid, cs := c.Identity()
		v := deviceHealthView{clientView: clientView{
			ID: c.ID, UID: uid, Callsign: cs, Username: c.Username, Remote: c.Remote, Transport: c.Transport, Format: c.Format(),
			GroupsIn: c.InGroups(), GroupsOut: c.OutGroups(), ConnectedAt: c.ConnectedAt, LastSeen: c.LastSeen(),
			In: c.In.Load(), Out: c.Out.Load(), Dropped: c.Dropped.Load(),
		}}
		v.LastSeenAgo = int(now.Sub(v.LastSeen).Seconds())
		sa := c.LastSA()
		if sa != nil {
			fillSA(&v.clientView, sa)
			if t := sa.Detail.Takv; t != nil {
				v.OS, v.Version = t.OS, t.Version
			}
			if p := sa.Detail.PrecisionLocation; p != nil {
				v.GeoSource = p.GeoPointSrc
			}
		}
		if s.Health != nil && uid != "" {
			hist, drain := s.Health.Battery(uid)
			v.BatteryHist, v.DrainPerHr = thinBattery(hist, 48), math.Round(drain*10)/10
		}
		v.Issues = deviceIssues(v, sa != nil)
		v.Worst = "ok" // notes ("info") are shown but are not problems
		for _, is := range v.Issues {
			if is.Level != "info" && levelRank[is.Level] > levelRank[v.Worst] {
				v.Worst = is.Level
			}
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b deviceHealthView) int {
		if d := levelRank[b.Worst] - levelRank[a.Worst]; d != 0 {
			return d
		}
		return strings.Compare(strings.ToLower(a.Callsign), strings.ToLower(b.Callsign))
	})
	return out
}

func deviceIssues(v deviceHealthView, haveSA bool) []issue {
	out := []issue{}
	if !haveSA {
		out = append(out, issue{"warning", "Has not reported its position yet"})
	}
	if v.Battery != nil {
		b := int(*v.Battery)
		switch {
		case b <= 15:
			out = append(out, issue{"critical", fmt.Sprintf("Battery %d%%", b)})
		case b <= 30:
			out = append(out, issue{"warning", fmt.Sprintf("Battery %d%%", b)})
		}
		if v.DrainPerHr > 0.5 && b > 0 {
			if left := float64(b) / v.DrainPerHr; left < 1.5 {
				out = append(out, issue{"warning", fmt.Sprintf("About %s of battery left at the current rate", fmtHours(left))})
			}
		}
	}
	switch ago := v.LastSeenAgo; {
	case ago >= 300:
		out = append(out, issue{"critical", fmt.Sprintf("No reports for %s", fmtHours(float64(ago)/3600))})
	case ago >= 120:
		out = append(out, issue{"warning", fmt.Sprintf("No reports for %s", fmtHours(float64(ago)/3600))})
	}
	if haveSA && (v.Lat == nil || (*v.Lat == 0 && *v.Lon == 0)) {
		out = append(out, issue{"warning", "No GPS position"})
	} else if strings.EqualFold(v.GeoSource, "USER") {
		out = append(out, issue{"info", "Position set by hand, not GPS"})
	}
	if v.Dropped > 0 {
		out = append(out, issue{"warning", fmt.Sprintf("%d messages dropped: slow or congested connection", v.Dropped)})
	}
	if v.Transport == "tcp" {
		out = append(out, issue{"info", "Unencrypted connection (TCP)"})
	}
	return out
}

func fmtHours(h float64) string {
	m := int(math.Round(h * 60))
	switch {
	case m < 1:
		return "under a minute"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	default:
		return fmt.Sprintf("%dh %02dm", m/60, m%60)
	}
}

// thinBattery keeps at most n points, always including the latest.
func thinBattery(p []health.BatteryPoint, n int) []health.BatteryPoint {
	if len(p) <= n {
		return p
	}
	out := make([]health.BatteryPoint, 0, n)
	step := float64(len(p)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		out = append(out, p[int(math.Round(float64(i)*step))])
	}
	return out
}
