package web

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Prometheus metrics at GET /metrics, in the text exposition format, for
// Grafana or other monitoring. Scraping needs an administrator: an API key
// (Server -> API keys; read-only is enough) sent as a bearer token, which
// Prometheus does with "authorization: { credentials: etk_... }".

type metricsWriter struct {
	w    io.Writer
	seen map[string]bool
}

// metric writes one sample, with its HELP and TYPE lines the first time.
// labels are name, value pairs.
func (m *metricsWriter) metric(name, kind, help string, value float64, labels ...string) {
	if !m.seen[name] {
		m.seen[name] = true
		fmt.Fprintf(m.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 1 {
		b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(labels[i] + `="` + promEscape(labels[i+1]) + `"`)
		}
		b.WriteByte('}')
	}
	fmt.Fprintf(m.w, "%s %s\n", b.String(), strconv.FormatFloat(value, 'g', -1, 64))
}

func promEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func (s *Server) metricsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /metrics", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.writeMetrics(w)
	}))
}

func (s *Server) writeMetrics(w io.Writer) {
	m := &metricsWriter{w: w, seen: map[string]bool{}}
	now := time.Now()
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}

	m.metric("edgetak_info", "gauge", "EdgeTAK version (always 1).", 1, "version", s.Version, "go_version", runtime.Version())
	if !s.Started.IsZero() {
		m.metric("edgetak_start_time_seconds", "gauge", "When EdgeTAK started, Unix time.", float64(s.Started.Unix()))
		m.metric("edgetak_uptime_seconds", "gauge", "Seconds since EdgeTAK started.", now.Sub(s.Started).Seconds())
	}

	// Devices and traffic.
	byTransport := map[string]int{"tls": 0, "tcp": 0}
	identified := 0
	for _, c := range s.Router.Clients() {
		byTransport[c.Transport]++
		if uid, _ := c.Identity(); uid != "" {
			identified++
		}
	}
	for _, t := range sortedKeys(byTransport) {
		m.metric("edgetak_clients_connected", "gauge", "Connected streaming clients, by transport.", float64(byTransport[t]), "transport", t)
	}
	m.metric("edgetak_devices_identified", "gauge", "Connected clients that have reported who they are.", float64(identified))
	in, out, dropped := s.Router.Totals()
	m.metric("edgetak_cot_received_total", "counter", "CoT events received from clients.", float64(in))
	m.metric("edgetak_cot_sent_total", "counter", "CoT events sent to clients.", float64(out))
	m.metric("edgetak_cot_dropped_total", "counter", "CoT events dropped because a client could not keep up.", float64(dropped))
	if s.Cfg.Video.Enabled {
		vin, vout := s.VideoBytes()
		m.metric("edgetak_video_received_bytes_total", "counter", "Bytes received by live video streams.", float64(vin))
		m.metric("edgetak_video_sent_bytes_total", "counter", "Bytes sent to video viewers.", float64(vout))
	}

	// Accounts and data.
	users, disabled, admins := 0, 0, 0
	for _, u := range s.Store.Users() {
		users++
		if u.Disabled {
			disabled++
		}
		if u.Admin {
			admins++
		}
	}
	m.metric("edgetak_users", "gauge", "User accounts.", float64(users))
	m.metric("edgetak_users_disabled", "gauge", "Disabled user accounts.", float64(disabled))
	m.metric("edgetak_admins", "gauge", "Administrator accounts.", float64(admins))
	m.metric("edgetak_groups", "gauge", "Groups.", float64(len(s.Store.Groups())))
	certs, expiring := 0, 0
	for _, c := range s.Store.Certs() {
		if c.Revoked || now.After(c.Expires) {
			continue
		}
		certs++
		if c.Expires.Sub(now) < 30*24*time.Hour {
			expiring++
		}
	}
	m.metric("edgetak_certificates_valid", "gauge", "Device certificates that are neither revoked nor expired.", float64(certs))
	m.metric("edgetak_certificates_expiring_30d", "gauge", "Valid device certificates expiring within 30 days.", float64(expiring))
	var pkgBytes int64
	pkgs := s.Store.Packages()
	for _, p := range pkgs {
		pkgBytes += p.Size
	}
	m.metric("edgetak_data_packages", "gauge", "Data packages.", float64(len(pkgs)))
	m.metric("edgetak_data_package_bytes", "gauge", "Total size of data packages.", float64(pkgBytes))
	if s.Missions != nil {
		missions, subs := 0, 0
		for _, ms := range s.Missions.Summaries() {
			missions++
			subs += len(ms.Subscribers)
		}
		m.metric("edgetak_missions", "gauge", "Data Sync missions.", float64(missions))
		m.metric("edgetak_mission_subscriptions", "gauge", "Subscriptions to Data Sync missions.", float64(subs))
	}
	m.metric("edgetak_reports", "gauge", "Reports from the ATAK Reports plugin.", float64(len(s.Store.Reports())))

	// Alerts.
	alerts := map[string]int{"emergency": 0, "lost_contact": 0, "geofence": 0}
	for _, e := range s.activeEmergencies() {
		switch e.Kind {
		case "":
			alerts["emergency"]++
		case "lost":
			alerts["lost_contact"]++
		default:
			alerts[e.Kind]++
		}
	}
	for _, k := range sortedKeys(alerts) {
		m.metric("edgetak_alerts_active", "gauge", "Open alerts, by kind.", float64(alerts[k]), "kind", k)
	}

	// Federation.
	if s.Federation != nil {
		st := s.Federation.Status()
		states := map[string]int{}
		var fin, fout, fdrop uint64
		for _, f := range st.Federates {
			states[f.State]++
			fin, fout, fdrop = fin+f.In, fout+f.Out, fdrop+f.Dropped
		}
		m.metric("edgetak_federation_listener_up", "gauge", "Whether the federation listener is running (1) or not (0).", b2f(st.Listener.Running))
		for _, k := range sortedKeys(states) {
			m.metric("edgetak_federates", "gauge", "Enabled federation links, by state.", float64(states[k]), "state", k)
		}
		m.metric("edgetak_federation_received_total", "counter", "Events received from federated servers on the current connections.", float64(fin))
		m.metric("edgetak_federation_sent_total", "counter", "Events sent to federated servers on the current connections.", float64(fout))
		m.metric("edgetak_federation_dropped_total", "counter", "Events for federated servers dropped on the current connections.", float64(fdrop))
		m.metric("edgetak_federation_pending", "gauge", "Federation requests waiting for approval.", float64(len(st.Pending)))
	}

	// Recording.
	if s.History != nil {
		tracks, points, bytes := s.History.Stats()
		m.metric("edgetak_position_history_tracks", "gauge", "Devices with recorded position history.", float64(tracks))
		m.metric("edgetak_position_history_points", "gauge", "Recorded positions.", float64(points))
		m.metric("edgetak_position_history_bytes", "gauge", "Position history on disk.", float64(bytes))
	}
	if s.CoTLog != nil {
		bytes, _ := s.CoTLog.Stats()
		m.metric("edgetak_cot_history_bytes", "gauge", "CoT history on disk.", float64(bytes))
	}

	// The process and the machine (from the Health tab's last sample; -1 = not available here).
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	m.metric("edgetak_go_goroutines", "gauge", "Goroutines.", float64(runtime.NumGoroutine()))
	m.metric("edgetak_go_heap_bytes", "gauge", "Go heap in use.", float64(ms.HeapAlloc))
	if s.Health != nil {
		if h, ok := s.Health.Latest(); ok {
			m.metric("edgetak_process_resident_memory_bytes", "gauge", "EdgeTAK's resident memory.", float64(h.ProcRSS))
			opt := func(name, help string, v float64) {
				if v >= 0 {
					m.metric(name, "gauge", help, v)
				}
			}
			opt("edgetak_process_cpu_percent", "EdgeTAK's share of all CPUs, %, over the last 10 seconds.", h.ProcCPU)
			opt("edgetak_system_cpu_percent", "System CPU use, %, over the last 10 seconds.", h.CPU)
			opt("edgetak_system_load1", "System load average over 1 minute.", h.Load1)
			opt("edgetak_system_temperature_celsius", "System temperature.", h.TempC)
			if h.MemTotal > 0 {
				m.metric("edgetak_system_memory_used_bytes", "gauge", "System memory in use.", float64(h.MemUsed))
				m.metric("edgetak_system_memory_total_bytes", "gauge", "System memory.", float64(h.MemTotal))
			}
			if h.DiskTotal > 0 {
				m.metric("edgetak_data_disk_used_bytes", "gauge", "Used space on the data directory's disk.", float64(h.DiskUsed))
				m.metric("edgetak_data_disk_total_bytes", "gauge", "Size of the data directory's disk.", float64(h.DiskTotal))
			}
		}
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
