package web

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Custom map sources for the console map, shared by all administrators.
// ATAK map source files (<customMapSource>) can be imported as they are.

const maxMapSources = 50

func (s *Server) mapRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/map/sources", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.Store.MapSources())
	}))
	mux.HandleFunc("PUT /admin/api/map/sources", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var in []store.MapSource
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&in); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		s.saveMapSources(w, id, in)
	}))
	// The body is an ATAK map source XML file; the source is appended.
	mux.HandleFunc("POST /admin/api/map/sources/import", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			jsonError(w, http.StatusBadRequest, "file too large")
			return
		}
		src, err := parseATAKMapSource(raw)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.saveMapSources(w, id, append(s.Store.MapSources(), src))
	}))
}

func (s *Server) saveMapSources(w http.ResponseWriter, id *identity, in []store.MapSource) {
	if len(in) > maxMapSources {
		jsonError(w, http.StatusBadRequest, "too many map sources")
		return
	}
	for i := range in {
		if err := cleanMapSource(&in[i]); err != nil {
			jsonError(w, http.StatusBadRequest, in[i].Name+": "+err.Error())
			return
		}
	}
	if err := s.Store.SetMapSources(in); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("map sources changed", "admin", id.Username, "count", len(in))
	writeJSON(w, http.StatusOK, in)
}

func cleanMapSource(m *store.MapSource) error {
	m.Name = truncate(strings.TrimSpace(m.Name), 80)
	m.URL = strings.TrimSpace(m.URL)
	m.Attribution = truncate(strings.TrimSpace(m.Attribution), 200)
	m.Subdomains = strings.TrimSpace(m.Subdomains)
	if m.Name == "" {
		return errors.New("a name is required")
	}
	u, err := url.Parse(strings.NewReplacer("{", "", "}", "").Replace(m.URL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("the URL must be an http(s) address")
	}
	if !strings.Contains(m.URL, "{z}") || !strings.Contains(m.URL, "{x}") ||
		!strings.Contains(m.URL, "{y}") && !strings.Contains(m.URL, "{-y}") {
		return errors.New("the URL needs {z}, {x} and {y} placeholders")
	}
	if strings.Contains(m.URL, "{s}") && m.Subdomains == "" {
		m.Subdomains = "abc"
	}
	if m.MaxZoom <= 0 || m.MaxZoom > 24 {
		m.MaxZoom = 19
	}
	m.MinZoom = max(0, min(m.MinZoom, m.MaxZoom))
	return nil
}

// parseATAKMapSource converts an ATAK/WinTAK custom map source file.
func parseATAKMapSource(raw []byte) (store.MapSource, error) {
	var x struct {
		XMLName     xml.Name `xml:"customMapSource"`
		Name        string   `xml:"name"`
		MinZoom     int      `xml:"minZoom"`
		MaxZoom     int      `xml:"maxZoom"`
		URL         string   `xml:"url"`
		ServerParts string   `xml:"serverParts"`
		InvertY     bool     `xml:"invertYCoordinate"`
	}
	if err := xml.Unmarshal(raw, &x); err != nil {
		return store.MapSource{}, errors.New("not an ATAK map source file (<customMapSource>)")
	}
	y := "{y}"
	if x.InvertY {
		y = "{-y}"
	}
	u := strings.NewReplacer("{$z}", "{z}", "{$x}", "{x}", "{$y}", y, "{$serverpart}", "{s}", "{$s}", "{s}").Replace(strings.TrimSpace(x.URL))
	src := store.MapSource{Name: x.Name, URL: u, MinZoom: x.MinZoom, MaxZoom: x.MaxZoom,
		Subdomains: strings.Join(strings.Fields(x.ServerParts), ",")}
	return src, cleanMapSource(&src)
}
