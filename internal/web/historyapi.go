package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Position history (breadcrumb trails), recorded only when an admin turns it on.

const maxRetentionHours = 30 * 24

func (s *Server) historyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/history/settings", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.historyStatus())
	}))
	mux.HandleFunc("PUT /admin/api/history/settings", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req store.HistorySettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if req.RetentionHours < 1 || req.RetentionHours > maxRetentionHours {
			jsonError(w, http.StatusBadRequest, "keep history between 1 hour and 30 days")
			return
		}
		if err := s.Store.SetHistorySettings(req); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if s.History != nil {
			s.History.Configure(req.Enabled, time.Duration(req.RetentionHours)*time.Hour)
		}
		s.Log.Info("history settings changed", "admin", id.Username, "enabled", req.Enabled, "retention_hours", req.RetentionHours)
		writeJSON(w, http.StatusOK, s.historyStatus())
	}))
	// ?minutes=N (default 60) or ?since=&until= (RFC 3339); optional ?uid=.
	mux.HandleFunc("GET /admin/api/history", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if s.History == nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		q := r.URL.Query()
		until := time.Now()
		since := until.Add(-time.Hour)
		if m, err := strconv.Atoi(q.Get("minutes")); err == nil && m > 0 {
			since = until.Add(-time.Duration(m) * time.Minute)
		}
		if t, err := time.Parse(time.RFC3339, q.Get("since")); err == nil {
			since = t
		}
		if t, err := time.Parse(time.RFC3339, q.Get("until")); err == nil {
			until = t
		}
		writeJSON(w, http.StatusOK, s.History.Tracks(since, until, q.Get("uid")))
	}))
	mux.HandleFunc("DELETE /admin/api/history", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if s.History != nil {
			if err := s.History.Clear(); err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		s.Log.Info("history cleared", "admin", id.Username)
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (s *Server) historyStatus() map[string]any {
	h := s.Store.HistorySettings()
	out := map[string]any{"enabled": h.Enabled, "retention_hours": h.RetentionHours}
	if s.History != nil {
		tracks, points, bytes := s.History.Stats()
		out["tracks"], out["points"], out["bytes"] = tracks, points, bytes
	}
	return out
}
