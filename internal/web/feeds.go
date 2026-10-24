package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Aircraft and ship feeds (Settings → Aircraft and ships): EdgeTAK turns
// public ADS-B and AIS data for an area into markers for devices; see
// internal/feeds.

var httpHeaderName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,64}$`)

func (s *Server) feedsView() map[string]any {
	set := s.Store.Feeds()
	hasKey, hasHeader := set.AIS.APIKey != "", set.ADSB.HeaderValue != ""
	set.AIS.APIKey, set.ADSB.HeaderValue = "", "" // never sent back
	v := map[string]any{"settings": set, "has_api_key": hasKey, "has_header_value": hasHeader, "sources": []string{"adsb.lol", "adsb.fi", "custom"}}
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
		saved := s.Store.Feeds()
		req.AIS.APIKey = strings.TrimSpace(req.AIS.APIKey)
		if req.AIS.APIKey == "" {
			req.AIS.APIKey = saved.AIS.APIKey // blank keeps the saved key
		}
		req.ADSB.CustomURL, req.ADSB.HeaderName = strings.TrimSpace(req.ADSB.CustomURL), strings.TrimSpace(req.ADSB.HeaderName)
		if req.ADSB.HeaderValue == "" && req.ADSB.HeaderName == saved.ADSB.HeaderName {
			req.ADSB.HeaderValue = saved.ADSB.HeaderValue // blank keeps the saved value
		}
		if req.Lat < -90 || req.Lat > 90 || req.Lon < -180 || req.Lon > 180 {
			jsonError(w, http.StatusBadRequest, "the area's latitude or longitude is out of range")
			return
		}
		if (req.ADSB.Enabled || req.AIS.Enabled) && req.Lat == 0 && req.Lon == 0 {
			jsonError(w, http.StatusBadRequest, "set the area's center first (pick it on the map, or enter its latitude and longitude)")
			return
		}
		for _, r := range []int{req.ADSB.RadiusNM, req.AIS.RadiusNM} {
			if r < 1 || r > 250 {
				jsonError(w, http.StatusBadRequest, "each radius is 1 to 250 nautical miles")
				return
			}
		}
		req.RadiusNM = 0 // the old shared radius
		if req.ADSB.Source != "" && !slices.Contains([]string{"adsb.lol", "adsb.fi", "custom"}, req.ADSB.Source) {
			jsonError(w, http.StatusBadRequest, "the ADS-B source is adsb.lol, adsb.fi or custom")
			return
		}
		if req.ADSB.Source == "custom" {
			u, err := url.Parse(strings.NewReplacer("{lat}", "0", "{lon}", "0", "{radius}", "1").Replace(req.ADSB.CustomURL))
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				jsonError(w, http.StatusBadRequest, "the custom ADS-B source needs an http:// or https:// URL")
				return
			}
			if req.ADSB.HeaderName != "" && !httpHeaderName.MatchString(req.ADSB.HeaderName) {
				jsonError(w, http.StatusBadRequest, "invalid header name")
				return
			}
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
			"lat", req.Lat, "lon", req.Lon, "adsb_radius_nm", req.ADSB.RadiusNM, "ais_radius_nm", req.AIS.RadiusNM)
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
