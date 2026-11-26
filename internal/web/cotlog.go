package web

import (
	"archive/zip"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/eventlog"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// CoT history: every event that passes through the server, recorded only
// when an admin turns it on, searched by type, area, time and text, and
// exported as CSV, KML, JSON or an ATAK data package.

const (
	cotPageSize  = 500
	cotMaxPage   = 5000
	cotMaxExport = 100_000
)

// cotLogHook records events. ADS-B and AIS feed tracks are left out: there
// are a great many, and they come from public sources anyway.
func (s *Server) cotLogHook(src *router.Client, ev *cot.Event) bool {
	if s.CoTLog == nil || cot.IsControl(ev.Type) || !s.CoTLog.Enabled() {
		return true
	}
	if src == nil && (strings.HasPrefix(ev.UID, "ICAO-") || strings.HasPrefix(ev.UID, "MMSI-")) {
		return true
	}
	e := eventlog.Entry{T: time.Now().UnixMilli(), UID: ev.UID, Type: ev.Type, How: ev.How, Callsign: ev.Callsign(),
		HasPos: true, Lat: ev.Lat, Lon: ev.Lon, Remarks: chatText(ev), Source: "server"}
	if src != nil {
		e.FromUID, e.FromCallsign = src.Identity()
		e.Username = src.Username
		switch src.Transport {
		case "tcp", "tls":
			e.Source = "device"
		default:
			e.Source = src.Transport // mesh, extension
		}
		e.SA = e.FromUID != "" && e.FromUID == ev.UID && strings.HasPrefix(ev.Type, "a-")
		if strings.HasPrefix(e.Username, "extension:") {
			e.Username = ""
		}
	}
	if !e.SA {
		e.XML = string(ev.XML())
	}
	s.CoTLog.Record(e)
	return true
}

// cotView is an event as the console lists it.
type cotView struct {
	T            int64    `json:"t"`
	UID          string   `json:"uid"`
	Type         string   `json:"type"`
	How          string   `json:"how,omitempty"`
	Callsign     string   `json:"callsign,omitempty"`
	FromUID      string   `json:"from_uid,omitempty"`
	FromCallsign string   `json:"from_callsign,omitempty"`
	Username     string   `json:"username,omitempty"`
	Source       string   `json:"source"`
	Lat          *float64 `json:"lat,omitempty"`
	Lon          *float64 `json:"lon,omitempty"`
	Remarks      string   `json:"remarks,omitempty"`
	Position     bool     `json:"position_report,omitempty"`
	XML          string   `json:"xml,omitempty"`
}

func viewCoT(e eventlog.Entry) cotView {
	v := cotView{T: e.T, UID: e.UID, Type: e.Type, How: e.How, Callsign: e.Callsign, FromUID: e.FromUID, FromCallsign: e.FromCallsign,
		Username: e.Username, Source: e.Source, Remarks: e.Remarks, Position: e.SA, XML: e.XML}
	if e.HasPos {
		lat, lon := e.Lat, e.Lon
		v.Lat, v.Lon = &lat, &lon
	}
	return v
}

func cotQuery(r *http.Request) (eventlog.Query, error) {
	v := r.URL.Query()
	q := eventlog.Query{Text: strings.TrimSpace(v.Get("q")), Positions: v.Get("positions") == "1"}
	for k, dst := range map[string]*time.Time{"since": &q.Since, "until": &q.Until} {
		if s := v.Get(k); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return q, fmt.Errorf("%s must be a time like 2026-11-25T14:00:00Z", k)
			}
			*dst = t
		}
	}
	for _, t := range strings.Split(v.Get("types"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			q.Types = append(q.Types, t)
		}
	}
	if b := v.Get("bbox"); b != "" {
		parts := strings.Split(b, ",")
		var box [4]float64
		ok := len(parts) == 4
		for i := 0; ok && i < 4; i++ {
			f, err := strconv.ParseFloat(strings.TrimSpace(parts[i]), 64)
			ok = err == nil && (i%2 == 0 && f >= -180 && f <= 180 || i%2 == 1 && f >= -90 && f <= 90)
			box[i] = f
		}
		if !ok || box[1] > box[3] {
			return q, fmt.Errorf("bbox must be west,south,east,north")
		}
		q.BBox = &box
	}
	return q, nil
}

func (s *Server) cotLogStatus() map[string]any {
	set := s.Store.CoTLogSettings()
	if set.KeepDays == 0 {
		set.KeepDays = eventlog.DefaultDays
	}
	out := map[string]any{"enabled": set.Enabled, "keep_days": set.KeepDays}
	if s.CoTLog != nil {
		out["bytes"], out["oldest"] = s.CoTLog.Stats()
	}
	return out
}

func (s *Server) cotLogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/cot-history/settings", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.cotLogStatus())
	}))
	mux.HandleFunc("PUT /admin/api/cot-history/settings", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req store.CoTLogSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if req.KeepDays < 1 || req.KeepDays > eventlog.MaxDays {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("keep events between 1 and %d days", eventlog.MaxDays))
			return
		}
		if err := s.Store.SetCoTLogSettings(req); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if s.CoTLog != nil {
			s.CoTLog.Configure(req.Enabled, req.KeepDays)
		}
		s.Log.Info("CoT history settings changed", "admin", id.Username, "enabled", req.Enabled, "keep_days", req.KeepDays)
		writeJSON(w, http.StatusOK, s.cotLogStatus())
	}))
	// ?since= &until= (RFC 3339) &types=a-h,b-t-f &q= &bbox=w,s,e,n &positions=1 &limit=
	mux.HandleFunc("GET /admin/api/cot-history", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		q, err := cotQuery(r)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		q.Limit = cotPageSize
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
			q.Limit = min(n, cotMaxPage)
		}
		views := []cotView{}
		total := 0
		if s.CoTLog != nil {
			found, n, err := s.CoTLog.Find(q, false)
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			total = n
			for _, e := range found {
				views = append(views, viewCoT(e))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": views, "total": total})
	}))
	// One event with its XML: ?t= (Unix ms) &uid=
	mux.HandleFunc("GET /admin/api/cot-history/event", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		t, err := strconv.ParseInt(r.URL.Query().Get("t"), 10, 64)
		if err != nil || s.CoTLog == nil {
			jsonError(w, http.StatusNotFound, "event not found")
			return
		}
		e, ok := s.CoTLog.Event(t, r.URL.Query().Get("uid"))
		if !ok {
			jsonError(w, http.StatusNotFound, "event not found")
			return
		}
		writeJSON(w, http.StatusOK, viewCoT(e))
	}))
	// The same filters, oldest first: ?format=csv|kml|json|zip (an ATAK data
	// package with the last version of each item).
	mux.HandleFunc("GET /admin/api/cot-history/export", s.admin(s.exportCoT))
	mux.HandleFunc("DELETE /admin/api/cot-history", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if s.CoTLog != nil {
			if err := s.CoTLog.Clear(); err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		s.Log.Info("CoT history cleared", "admin", id.Username)
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (s *Server) exportCoT(w http.ResponseWriter, r *http.Request, id *identity) {
	q, err := cotQuery(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	q.Limit = cotMaxExport
	var found []eventlog.Entry
	if s.CoTLog != nil {
		if found, _, err = s.CoTLog.Find(q, true); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	slices.Reverse(found) // oldest first
	format := r.URL.Query().Get("format")
	name := "edgetak-cot-" + time.Now().UTC().Format("20060102-150405")
	s.Log.Info("CoT history exported", "admin", id.Username, "format", format, "events", len(found))
	attach := func(ext, ctype string) {
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+ext+`"`)
	}
	ts := func(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z") }
	switch format {
	case "json":
		views := make([]cotView, 0, len(found))
		for _, e := range found {
			views = append(views, viewCoT(e))
		}
		attach(".json", "application/json")
		json.NewEncoder(w).Encode(views)
	case "kml":
		attach(".kml", "application/vnd.google-earth.kml+xml")
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<kml xmlns="http://www.opengis.net/kml/2.2"><Document><name>` + xmlEscape(name) + "</name>\n")
		for _, e := range found {
			if !e.HasPos {
				continue
			}
			desc := e.Type + " · " + ts(e.T)
			if e.FromCallsign != "" && e.FromCallsign != e.Callsign {
				desc += " · from " + e.FromCallsign
			}
			if e.Remarks != "" {
				desc += "\n" + e.Remarks
			}
			fmt.Fprintf(&b, "<Placemark><name>%s</name><description>%s</description><TimeStamp><when>%s</when></TimeStamp><Point><coordinates>%s,%s</coordinates></Point></Placemark>\n",
				xmlEscape(firstNonEmpty(e.Callsign, e.UID)), xmlEscape(desc), ts(e.T),
				strconv.FormatFloat(e.Lon, 'f', -1, 64), strconv.FormatFloat(e.Lat, 'f', -1, 64))
		}
		b.WriteString("</Document></kml>\n")
		w.Write([]byte(b.String()))
	case "zip":
		attach(".zip", "application/zip")
		writeCoTPackage(w, name, found)
	default:
		attach(".csv", "text/csv; charset=utf-8")
		cw := csv.NewWriter(w)
		cw.Write([]string{"time_utc", "type", "how", "uid", "callsign", "from_callsign", "from_uid", "username", "source", "lat", "lon", "remarks"})
		for _, e := range found {
			lat, lon := "", ""
			if e.HasPos {
				lat, lon = strconv.FormatFloat(e.Lat, 'f', 7, 64), strconv.FormatFloat(e.Lon, 'f', 7, 64)
			}
			cw.Write([]string{ts(e.T), csvSafe(e.Type), csvSafe(e.How), csvSafe(e.UID), csvSafe(e.Callsign), csvSafe(e.FromCallsign), csvSafe(e.FromUID),
				csvSafe(e.Username), e.Source, lat, lon, csvSafe(e.Remarks)})
		}
		cw.Flush()
	}
}

// writeCoTPackage writes an ATAK data package with the last recorded version
// of each map item in found (oldest first): markers, shapes, routes and the
// like, not chat, deletes or devices' own positions.
func writeCoTPackage(w http.ResponseWriter, name string, found []eventlog.Entry) {
	last := map[string]eventlog.Entry{}
	var order []string
	for _, e := range found {
		if e.XML == "" || e.SA || strings.HasPrefix(e.Type, "t-") || strings.HasPrefix(e.Type, "b-t-f") {
			continue
		}
		if _, seen := last[e.UID]; !seen {
			order = append(order, e.UID)
		}
		last[e.UID] = e
	}
	zw := zip.NewWriter(w)
	var m strings.Builder
	m.WriteString(`<MissionPackageManifest version="2"><Configuration><Parameter name="uid" value="` + xmlEscape(newReportID()) +
		`"/><Parameter name="name" value="` + xmlEscape(name) + `"/></Configuration><Contents>`)
	for i, uid := range order {
		entry := fmt.Sprintf("%d/%s.cot", i+1, safeFileName(uid))
		fmt.Fprintf(&m, `<Content ignore="false" zipEntry="%s"><Parameter name="uid" value="%s"/></Content>`, xmlEscape(entry), xmlEscape(uid))
		if f, err := zw.Create(entry); err == nil {
			f.Write([]byte(last[uid].XML))
		}
	}
	m.WriteString(`</Contents></MissionPackageManifest>`)
	if f, err := zw.Create("MANIFEST/manifest.xml"); err == nil {
		f.Write([]byte(m.String()))
	}
	zw.Close()
}

// safeFileName keeps a UID usable as a file name.
func safeFileName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < ' ' {
			return '_'
		}
		return r
	}, s)
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}
