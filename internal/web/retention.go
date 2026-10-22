package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/mission"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Data retention: how long each kind of data is kept. Rules run every hour
// and on demand; the console previews what each rule would remove.

const retentionEvery = time.Hour

type retentionResult struct {
	At      time.Time      `json:"at"`
	Removed map[string]int `json:"removed"`
	Errors  []string       `json:"errors,omitempty"`
	DryRun  bool           `json:"dry_run"`
}

type retentionState struct {
	mu   sync.Mutex
	last *retentionResult
}

func (s *Server) retentionStart(ctx context.Context) {
	go func() {
		t := time.NewTicker(retentionEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.applyRetention(s.Store.RetentionSettings(), false)
			}
		}
	}()
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// applyRetention removes (or with dry, counts) what the rules say is too old.
func (s *Server) applyRetention(r store.RetentionSettings, dry bool) retentionResult {
	now := time.Now()
	res := retentionResult{At: now, Removed: map[string]int{}, DryRun: dry}
	note := func(kind string, n int, err error) {
		res.Removed[kind] = n
		if err != nil {
			res.Errors = append(res.Errors, kind+": "+err.Error())
		}
	}
	// Missions first: packages a deleted mission used can then age out too.
	if r.MissionsIdleDays > 0 && s.Missions != nil {
		cut := now.Add(-days(r.MissionsIdleDays))
		n := 0
		var err error
		for _, m := range s.Missions.Summaries() {
			last := m.LastEdited
			if last.IsZero() {
				last = m.Created
			}
			if last.Before(cut) {
				n++
				if !dry {
					if _, e := s.Missions.Delete(m.Name, mission.Caller{Username: "retention", Admin: true}); e != nil {
						err = e
					}
				}
			}
		}
		note("missions", n, err)
	}
	if r.PackagesDays > 0 {
		cut := now.Add(-days(r.PackagesDays))
		inUse := map[string]bool{}
		if s.Missions != nil {
			inUse = s.Missions.ReferencedHashes()
		}
		n := 0
		var err error
		for _, p := range s.Store.Packages() {
			if p.Submitted.Before(cut) && !inUse[p.Hash] {
				n++
				if !dry {
					if e := s.removePackage(p.Hash); e != nil {
						err = e
					}
				}
			}
		}
		note("packages", n, err)
	}
	if r.InboxDays > 0 {
		n, err := s.Store.PruneInbox(now.Add(-days(r.InboxDays)), dry)
		note("inbox", n, err)
	}
	if r.RequestsDays > 0 {
		n, err := s.Store.PruneRequests(now.Add(-days(r.RequestsDays)), dry)
		note("requests", n, err)
	}
	if r.InvitesDays > 0 {
		n, err := s.Store.PruneInvites(now.Add(-days(r.InvitesDays)), now, dry)
		note("invites", n, err)
	}
	if r.CertsDays > 0 {
		n, err := s.Store.PruneCerts(now.Add(-days(r.CertsDays)), dry)
		note("certs", n, err)
	}
	if r.ChatDays > 0 && s.Chat != nil {
		n, err := s.Chat.Prune(now.Add(-days(r.ChatDays)), dry)
		note("chat", n, err)
	}
	if r.AuditDays > 0 && s.Audit != nil {
		n, err := s.Audit.Prune(now.Add(-days(r.AuditDays)), dry)
		note("audit", n, err)
	}
	if !dry {
		total := 0
		for _, n := range res.Removed {
			total += n
		}
		if total > 0 || len(res.Errors) > 0 {
			s.Log.Info("data retention applied", "removed", res.Removed, "errors", res.Errors)
		}
		s.retention.mu.Lock()
		s.retention.last = &res
		s.retention.mu.Unlock()
	}
	return res
}

func (s *Server) retentionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/retention", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		set := s.Store.RetentionSettings()
		s.retention.mu.Lock()
		last := s.retention.last
		s.retention.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"settings": set, "history": s.Store.HistorySettings(), "pending": s.applyRetention(set, true), "last": last,
			"recording": s.Store.RecordingSettings(), "video": s.Cfg.Video.Enabled,
		})
	}))
	// Preview: what the given settings would remove now, without saving.
	mux.HandleFunc("POST /admin/api/retention/preview", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		var set store.RetentionSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&set); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		writeJSON(w, http.StatusOK, s.applyRetention(clampRetention(set), true))
	}))
	mux.HandleFunc("PUT /admin/api/retention", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var set store.RetentionSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&set); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		set = clampRetention(set)
		if err := s.Store.SetRetentionSettings(set); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("data retention settings changed", "admin", id.Username, "settings", set)
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /admin/api/retention/run", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		s.Log.Info("data retention run requested", "admin", id.Username)
		writeJSON(w, http.StatusOK, s.applyRetention(s.Store.RetentionSettings(), false))
	}))
}

// clampRetention keeps day counts between 0 (keep forever) and ten years.
func clampRetention(r store.RetentionSettings) store.RetentionSettings {
	c := func(n int) int { return max(0, min(n, 3650)) }
	return store.RetentionSettings{PackagesDays: c(r.PackagesDays), MissionsIdleDays: c(r.MissionsIdleDays), InboxDays: c(r.InboxDays),
		RequestsDays: c(r.RequestsDays), InvitesDays: c(r.InvitesDays), CertsDays: c(r.CertsDays), ChatDays: c(r.ChatDays), AuditDays: c(r.AuditDays)}
}
