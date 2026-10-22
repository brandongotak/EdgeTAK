package web

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/history"
	"github.com/brandongotak/EdgeTAK/internal/overlay"
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
	// Download trails: ?format=kml|gpx&since=&until= (RFC 3339), optional
	// uid=... (repeated) for chosen devices.
	mux.HandleFunc("GET /admin/api/history/export", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		q := r.URL.Query()
		tracks, since, until, err := s.exportTracks(q.Get("since"), q.Get("until"), q["uid"])
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		title := historyTitle(since, until)
		data, ext, ct := history.KML(tracks, title), ".kml", "application/vnd.google-earth.kml+xml"
		if q.Get("format") == "gpx" {
			data, ext, ct = history.GPX(tracks, title), ".gpx", "application/gpx+xml"
		}
		s.Log.Info("history exported", "admin", id.Username, "format", strings.TrimPrefix(ext, "."), "devices", len(tracks), "since", since, "until", until)
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": overlay.SafeName(title, ext)}))
		w.Write(data)
	}))
	// Turn trails into an overlay (not sent until an admin chooses its
	// groups and resumes it on the Overlays page).
	mux.HandleFunc("POST /admin/api/history/overlay", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			Name  string   `json:"name"`
			Since string   `json:"since"`
			Until string   `json:"until"`
			UIDs  []string `json:"uids"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		tracks, since, until, err := s.exportTracks(req.Since, req.Until, req.UIDs)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		name := firstNonEmpty(strings.TrimSpace(req.Name), historyTitle(since, until))
		if !validOverlayName(name) {
			jsonError(w, http.StatusBadRequest, "give the overlay a name (up to 80 characters)")
			return
		}
		now := time.Now().UTC()
		o := store.Overlay{ID: uuid(), Name: name, Groups: []string{}, Version: 1, Author: id.Username, Created: now, Updated: now, Paused: true}
		if err := s.buildOverlay(&o, history.KML(tracks, name), overlay.SafeName(name, ".kml")); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.Store.PutOverlay(o); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("history saved as an overlay", "admin", id.Username, "overlay", o.Name, "devices", len(tracks))
		writeJSON(w, http.StatusCreated, map[string]any{"id": o.ID, "name": o.Name, "devices": len(tracks)})
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

// exportTracks are the recorded trails between since and until (RFC 3339),
// of the given devices (all when none are given).
func (s *Server) exportTracks(sinceStr, untilStr string, uids []string) ([]history.Track, time.Time, time.Time, error) {
	since, err1 := time.Parse(time.RFC3339, sinceStr)
	until, err2 := time.Parse(time.RFC3339, untilStr)
	if err1 != nil || err2 != nil || !until.After(since) {
		return nil, since, until, errors.New("choose a time range (since before until)")
	}
	if s.History == nil {
		return nil, since, until, errors.New("no position history is recorded")
	}
	var out []history.Track
	for _, t := range s.History.Tracks(since, until, "") {
		if len(uids) == 0 || slices.Contains(uids, t.UID) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, since, until, errors.New("no recorded positions for those devices in that time range")
	}
	return out, since, until, nil
}

// historyTitle names an export by its time range (server time).
func historyTitle(since, until time.Time) string {
	since, until = since.Local(), until.Local()
	if since.Format("2006-01-02") == until.Format("2006-01-02") {
		return "Trails " + since.Format("2006-01-02 1504") + "-" + until.Format("1504")
	}
	return "Trails " + since.Format("2006-01-02 1504") + " to " + until.Format("2006-01-02 1504")
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
