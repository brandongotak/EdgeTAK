package web

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// MEDEVAC board (Overview → MEDEVAC): the 9-line MEDEVAC requests ATAK
// users send (a b-r-f-h-c marker carrying a _medevac_ detail, from ATAK's
// MedLine tool), with what each line says, where, and a status the console
// tracks (open, acknowledged, en route, complete, cancelled). A new request
// raises a "medevac" webhook alert.

const medevacType = "b-r-f-h-c"

var medevacStatuses = []string{"open", "acknowledged", "en_route", "complete", "cancelled"}

// parseMedevac reads the _medevac_ element of a detail.
func parseMedevac(detail string) (fields map[string]string, zmists []store.ZMist, ok bool) {
	i := strings.Index(detail, "<_medevac_")
	if i < 0 {
		return nil, nil, false
	}
	d := xml.NewDecoder(strings.NewReader(detail[i:]))
	d.Strict = false
	fields = map[string]string{}
	depth := 0
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 1 && t.Name.Local == "_medevac_":
				for _, a := range t.Attr {
					if v := strings.TrimSpace(a.Value); v != "" && len(fields) < 100 {
						fields[a.Name.Local] = truncate(v, 500)
					}
				}
			case t.Name.Local == "zMist" && len(zmists) < 50:
				z := store.ZMist{}
				for _, a := range t.Attr {
					v := truncate(strings.TrimSpace(a.Value), 500)
					switch a.Name.Local {
					case "title":
						z.Title = v
					case "z":
						z.Z = v
					case "m":
						z.M = v
					case "i":
						z.I = v
					case "s":
						z.S = v
					case "t":
						z.T = v
					}
				}
				zmists = append(zmists, z)
			}
		case xml.EndElement:
			depth--
			if depth == 0 {
				return fields, zmists, true
			}
		}
	}
	return fields, zmists, len(fields) > 0
}

// medevacHook records MEDEVAC requests and their removal. It never drops
// events.
func (s *Server) medevacHook(src *router.Client, ev *cot.Event) bool {
	switch {
	case ev.Type == medevacType || strings.Contains(ev.Detail.XML, "<_medevac_"):
		s.recordMedevac(src, ev)
	case ev.Type == cot.TypeDelete:
		if m := reLinkUID.FindStringSubmatch(ev.Detail.XML); m != nil {
			if _, ok := s.Store.Medevac(m[1]); ok {
				s.Store.UpsertMedevac(m[1], func(r *store.Medevac) {
					if !r.Removed {
						now := time.Now().UTC()
						r.Removed, r.Updated = true, now
						r.History = append(r.History, store.MedevacEntry{At: now, What: "removed on the device"})
					}
				})
			}
		}
	}
	return true
}

var reLinkUID = regexp.MustCompile(`<link[\s][^>]*?uid=["']([^"']+)["']`)

func (s *Server) recordMedevac(src *router.Client, ev *cot.Event) {
	fields, zmists, ok := parseMedevac(ev.Detail.XML)
	if !ok || ev.UID == "" {
		return
	}
	var fromUID, fromCallsign, username string
	var groups []string
	if src != nil {
		fromUID, fromCallsign = src.Identity()
		username, groups = src.Username, deviceGroups(src)
	}
	now := time.Now().UTC()
	m, created, err := s.Store.UpsertMedevac(ev.UID, func(r *store.Medevac) {
		r.Title = firstNonEmpty(fields["title"], ev.Callsign(), r.Title, ev.UID)
		r.Lat, r.Lon, r.Hae = ev.Lat, ev.Lon, ev.Hae
		r.Fields, r.ZMists = fields, zmists
		if r.Created.IsZero() {
			r.Created, r.Status = now, "open"
			r.FromUID, r.FromCallsign, r.Username, r.Groups = fromUID, fromCallsign, username, groups
			r.History = append(r.History, store.MedevacEntry{At: now, What: "open", By: firstNonEmpty(fromCallsign, username)})
		} else if r.Updated.Before(now.Add(-2 * time.Second)) { // ATAK resends; note real edits only
			r.History = append(r.History, store.MedevacEntry{At: now, What: "updated", By: firstNonEmpty(fromCallsign, username)})
		}
		r.Removed = false
		r.Updated = now
	})
	if err != nil {
		s.Log.Error("store MEDEVAC request", "err", err)
		return
	}
	if created {
		s.Log.Warn("MEDEVAC REQUEST", "title", m.Title, "from", fromCallsign, "user", username, "summary", medevacSummary(m))
		go s.notify(notice{Kind: "medevac", Title: "MEDEVAC request: " + m.Title, Callsign: fromCallsign, Username: username,
			Text: medevacSummary(m), Lat: m.Lat, Lon: m.Lon, Groups: m.Groups})
	}
}

// medevacSummary is the request in one line: patients by precedence and
// type, and security at the pick-up site.
func medevacSummary(m *store.Medevac) string {
	var parts []string
	var prec []string
	for _, p := range [][2]string{{"urgent", "urgent"}, {"urgent_surgical", "urgent surgical"}, {"priority", "priority"}, {"routine", "routine"}, {"convenience", "convenience"}} {
		if n, _ := strconv.Atoi(m.Fields[p[0]]); n > 0 {
			prec = append(prec, fmt.Sprintf("%d %s", n, p[1]))
		}
	}
	if len(prec) > 0 {
		parts = append(parts, strings.Join(prec, ", "))
	}
	var kind []string
	for _, p := range [][2]string{{"litter", "litter"}, {"ambulatory", "ambulatory"}} {
		if n, _ := strconv.Atoi(m.Fields[p[0]]); n > 0 {
			kind = append(kind, fmt.Sprintf("%d %s", n, p[1]))
		}
	}
	if len(kind) > 0 {
		parts = append(parts, strings.Join(kind, ", "))
	}
	if sec, ok := map[string]string{"0": "N - no enemy", "1": "P - possible enemy", "2": "E - enemy, approach with caution", "3": "X - enemy, armed escort required"}[m.Fields["security"]]; ok {
		parts = append(parts, "security "+sec)
	}
	if g := mgrs(m.Lat, m.Lon); g != "" {
		parts = append(parts, "MGRS "+g)
	}
	return strings.Join(parts, " · ")
}

type medevacView struct {
	*store.Medevac
	MGRS    string `json:"mgrs,omitempty"`
	Summary string `json:"summary"`
}

func (s *Server) medevacViews() []medevacView {
	out := []medevacView{}
	for _, m := range s.Store.Medevacs() {
		out = append(out, medevacView{Medevac: m, MGRS: mgrs(m.Lat, m.Lon), Summary: medevacSummary(m)})
	}
	return out
}

func medevacActive(m *store.Medevac) bool { return m.Status != "complete" && m.Status != "cancelled" }

func (s *Server) medevacRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/medevac", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if r.URL.Query().Get("summary") == "1" { // the console's badge
			open, newest := 0, time.Time{}
			for _, m := range s.Store.Medevacs() {
				if medevacActive(m) {
					open++
				}
				if m.Created.After(newest) {
					newest = m.Created
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"active": open, "newest": newest})
			return
		}
		writeJSON(w, http.StatusOK, s.medevacViews())
	}))
	mux.HandleFunc("PUT /admin/api/medevac/{id}/status", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || !slices.Contains(medevacStatuses, req.Status) {
			jsonError(w, http.StatusBadRequest, "the status is open, acknowledged, en_route, complete or cancelled")
			return
		}
		if _, ok := s.Store.Medevac(r.PathValue("id")); !ok {
			jsonError(w, http.StatusNotFound, "no such request")
			return
		}
		now := time.Now().UTC()
		m, _, err := s.Store.UpsertMedevac(r.PathValue("id"), func(m *store.Medevac) {
			if m.Status != req.Status {
				m.Status, m.StatusBy, m.StatusAt = req.Status, id.Username, now
				m.History = append(m.History, store.MedevacEntry{At: now, What: req.Status, By: id.Username})
			}
		})
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("MEDEVAC status", "admin", id.Username, "request", m.Title, "status", req.Status)
		writeJSON(w, http.StatusOK, s.medevacViews())
	}))
	mux.HandleFunc("DELETE /admin/api/medevac/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if err := s.Store.DeleteMedevac(r.PathValue("id")); err != nil {
			jsonError(w, http.StatusNotFound, "no such request")
			return
		}
		s.Log.Info("MEDEVAC request removed from the board", "admin", id.Username, "request", r.PathValue("id"))
		writeJSON(w, http.StatusOK, s.medevacViews())
	}))
}
