package web

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/brandongotak/EdgeTAK/internal/release"
)

// Server health alerts: every few minutes the server's own issues (the
// ones on the Health tab: disk, memory, temperature, certificates, plus
// federation links down, automatic backups failing and updates available)
// are compared with the last check, and webhooks with "Server health"
// ticked hear when one starts, once a day while it lasts, and when it is
// resolved.

const (
	healthCheckEvery = 5 * time.Minute
	healthRemind     = 24 * time.Hour
	fedDownAlert     = 5 * time.Minute // a link down this long is an issue
	updateCheckEvery = 24 * time.Hour
)

type healthWatch struct {
	mu        sync.Mutex
	active    map[string]healthAlert // by issueKey
	fedDown   map[string]time.Time   // federate ID -> first seen down
	backupErr string                 // the last automatic backup's error
	backupAt  time.Time
}

type healthAlert struct {
	issue
	since, told time.Time
}

// issueKey identifies an issue across checks whose numbers change ("Disk
// 86% full" and "Disk 96% full: ..." are the same issue).
func issueKey(text string) string {
	if i := strings.IndexAny(text, ":."); i > 0 {
		text = text[:i]
	}
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) || r == '%' || r == '°' {
			return -1
		}
		return r
	}, text)), " ")
}

// healthAlertIssues are the issues alerts are sent for: warnings and worse,
// and an update being available.
func (s *Server) healthAlertIssues(now time.Time) []issue {
	var out []issue
	for _, i := range s.serverIssuesAt(now) {
		if i.Level != "info" || strings.HasPrefix(i.Text, "EdgeTAK v") {
			out = append(out, i)
		}
	}
	return out
}

// moreServerIssues are the issues added to the Health tab's own checks.
func (s *Server) moreServerIssues(now time.Time) []issue {
	var out []issue
	s.healthWatch.mu.Lock()
	if s.healthWatch.backupErr != "" {
		out = append(out, issue{"warning", "Automatic backup failed: " + s.healthWatch.backupErr})
	}
	down := map[string]time.Time{}
	for id, t := range s.healthWatch.fedDown {
		down[id] = t
	}
	s.healthWatch.mu.Unlock()
	for _, f := range s.Store.FederationSettings().Federates {
		if t, ok := down[f.ID]; ok && f.Enabled && now.Sub(t) >= fedDownAlert {
			out = append(out, issue{"warning", fmt.Sprintf("Federation with %s down: not connected since %s", f.Name, t.Local().Format("Jan 2 15:04"))})
		}
	}
	s.selfUpdate.mu.Lock()
	latest := s.selfUpdate.latest
	s.selfUpdate.mu.Unlock()
	if latest != nil && release.Newer(latest.Version, s.Version) {
		out = append(out, issue{"info", fmt.Sprintf("EdgeTAK %s is available (this is %s): update from the Dashboard", latest.Version, s.Version)})
	}
	return out
}

// trackFederation notes which enabled federation links are down, and since when.
func (s *Server) trackFederation(now time.Time) {
	if s.Federation == nil {
		return
	}
	st := s.Federation.Status()
	s.healthWatch.mu.Lock()
	defer s.healthWatch.mu.Unlock()
	if s.healthWatch.fedDown == nil {
		s.healthWatch.fedDown = map[string]time.Time{}
	}
	for _, f := range s.Store.FederationSettings().Federates {
		ss, ok := st.Federates[f.ID]
		if !f.Enabled || (ok && ss.State == "connected") {
			delete(s.healthWatch.fedDown, f.ID)
			continue
		}
		if _, seen := s.healthWatch.fedDown[f.ID]; !seen {
			s.healthWatch.fedDown[f.ID] = now
		}
	}
}

// backupResult records how the last automatic backup went.
func (s *Server) backupResult(err error) {
	s.healthWatch.mu.Lock()
	defer s.healthWatch.mu.Unlock()
	s.healthWatch.backupErr, s.healthWatch.backupAt = "", time.Now()
	if err != nil {
		s.healthWatch.backupErr = err.Error()
	}
}

// CheckHealthAlerts compares the server's issues with the last check and
// sends alerts for what changed (every 5 minutes; tests call it).
func (s *Server) CheckHealthAlerts(now time.Time) {
	s.trackFederation(now)
	current := map[string]issue{}
	for _, i := range s.healthAlertIssues(now) {
		current[issueKey(i.Text)] = i
	}
	s.healthWatch.mu.Lock()
	if s.healthWatch.active == nil {
		s.healthWatch.active = map[string]healthAlert{}
	}
	var tell []notice
	for key, i := range current {
		a, was := s.healthWatch.active[key]
		switch {
		case !was:
			a = healthAlert{issue: i, since: now, told: now}
			tell = append(tell, notice{Kind: "server", Title: healthTitle(i), Text: i.Text})
		case i.Level != a.Level && levelRank[i.Level] > levelRank[a.Level]:
			a.issue, a.told = i, now // it got worse
			tell = append(tell, notice{Kind: "server", Title: healthTitle(i), Text: i.Text})
		case now.Sub(a.told) >= healthRemind:
			a.issue, a.told = i, now
			tell = append(tell, notice{Kind: "server", Title: "Still: " + healthTitle(i), Text: i.Text + fmt.Sprintf(" (since %s)", a.since.Local().Format("Jan 2 15:04"))})
		default:
			a.issue = i
		}
		s.healthWatch.active[key] = a
	}
	for key, a := range s.healthWatch.active {
		if _, still := current[key]; !still {
			delete(s.healthWatch.active, key)
			if !strings.HasPrefix(a.Text, "EdgeTAK v") { // an update installed needs no news
				tell = append(tell, notice{Kind: "server", Title: "Resolved: " + healthTitle(a.issue), Text: a.Text})
			}
		}
	}
	s.healthWatch.mu.Unlock()
	for _, n := range tell {
		n.Time, n.Groups = now, []string{}
		s.Log.Warn("server health: " + n.Title)
		s.notify(n)
	}
}

// healthTitle is an issue's headline: its text up to the first explanation.
func healthTitle(i issue) string {
	t := i.Text
	if j := strings.Index(t, ": "); j > 0 {
		t = t[:j]
	}
	if i.Level == "critical" {
		return "Critical: " + t
	}
	return t
}

// healthAlertsStart checks every few minutes, and for EdgeTAK updates once
// a day (when the server can reach GitHub).
func (s *Server) healthAlertsStart(ctx context.Context) {
	go func() {
		t := time.NewTicker(healthCheckEvery)
		defer t.Stop()
		lastUpdateCheck := time.Time{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if time.Since(lastUpdateCheck) >= updateCheckEvery {
				lastUpdateCheck = time.Now()
				cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				s.checkSelfUpdate(cctx) // offline servers just don't hear about updates
				cancel()
			}
			s.CheckHealthAlerts(time.Now())
		}
	}()
}
