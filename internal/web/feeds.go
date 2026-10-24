package web

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Aircraft and ship feeds (Settings → Aircraft and ships): EdgeTAK turns
// public ADS-B and AIS data for an area into markers for devices; see
// internal/feeds.

func (s *Server) feedsView() map[string]any {
	set := s.Store.Feeds()
	hasKey := set.AIS.APIKey != ""
	set.AIS.APIKey = "" // never sent back
	v := map[string]any{"settings": set, "has_api_key": hasKey, "sources": []string{"adsb.lol", "adsb.fi"}}
	if s.Feeds != nil {
		v["status"] = s.Feeds.Status()
	}
	return v
}

func (s *Server) feedRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/feeds", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.feedsView())
	}))
	mux.HandleFunc("PUT /admin/api/feeds", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req store.FeedSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		req.AIS.APIKey = strings.TrimSpace(req.AIS.APIKey)
		if req.AIS.APIKey == "" {
			req.AIS.APIKey = s.Store.Feeds().AIS.APIKey // blank keeps the saved key
		}
		if req.Lat < -90 || req.Lat > 90 || req.Lon < -180 || req.Lon > 180 {
			jsonError(w, http.StatusBadRequest, "the area's latitude or longitude is out of range")
			return
		}
		if (req.ADSB.Enabled || req.AIS.Enabled) && req.Lat == 0 && req.Lon == 0 {
			jsonError(w, http.StatusBadRequest, "set the area's center first (pick it on the map, or enter its latitude and longitude)")
			return
		}
		if req.RadiusNM < 1 || req.RadiusNM > 250 {
			jsonError(w, http.StatusBadRequest, "the radius is 1 to 250 nautical miles")
			return
		}
		if req.ADSB.Source != "" && !slices.Contains([]string{"adsb.lol", "adsb.fi"}, req.ADSB.Source) {
			jsonError(w, http.StatusBadRequest, "the ADS-B source is adsb.lol or adsb.fi")
			return
		}
		if req.AIS.Enabled && req.AIS.APIKey == "" {
			jsonError(w, http.StatusBadRequest, "ships need an aisstream.io API key")
			return
		}
		for _, g := range append(slices.Clone(req.ADSB.Groups), req.AIS.Groups...) {
			if _, ok := s.Store.Group(g); !ok {
				jsonError(w, http.StatusBadRequest, "no such group: "+g)
				return
			}
		}
		if err := s.Store.SetFeeds(req); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if s.Feeds != nil {
			s.Feeds.Apply(s.Store.Feeds())
		}
		s.Log.Info("aircraft and ship feeds changed", "admin", id.Username, "adsb", req.ADSB.Enabled, "ais", req.AIS.Enabled,
			"lat", req.Lat, "lon", req.Lon, "radius_nm", req.RadiusNM)
		writeJSON(w, http.StatusOK, s.feedsView())
	}))
	// The current aircraft and ships, for the console map.
	mux.HandleFunc("GET /admin/api/feeds/tracks", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if s.Feeds == nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		writeJSON(w, http.StatusOK, s.Feeds.Tracks())
	}))
}
