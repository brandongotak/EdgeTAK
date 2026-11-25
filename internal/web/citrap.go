package web

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/mission"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// The ATAK Reports plugin stores its reports on TAK Server through the CITRAP
// API (TAK Server's CITrapReportAPI):
//
//	POST   /Marti/api/citrap?clientUid=         a report: a zip with the report XML and attachments
//	GET    /Marti/api/citrap?keywords=&bbox=&startTime=&endTime=&maxReportCount=&type=&callsign=&subscribe=&clientUid=
//	GET    /Marti/api/citrap/{id}?clientUid=    the zip (or the XML, Accept: application/xml)
//	PUT    /Marti/api/citrap/{id}?clientUid=    a new zip, or new XML (Content-Type: application/xml)
//	DELETE /Marti/api/citrap/{id}?clientUid=
//	POST   /Marti/api/citrap/{id}/attachment    a file (Content-Disposition: filename=...)
//
// Like TAK Server, each report's zip is a data package (tool "citrap"), and
// it is added to the "citrap" mission, which every user sees and the plugin
// subscribes to for new reports, and to a mission of its own named by the
// report ID, whose subscribers hear of changes to that report.

const citrapMission = "citrap"

var citrapIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func (s *Server) citrapRoutes(mux *http.ServeMux) {
	if s.Missions != nil {
		if err := s.Missions.EnsureEveryone(citrapMission, "citrap", "CITrapReportService"); err != nil {
			s.Log.Error("can't create the citrap mission for the Reports plugin", "err", err)
		}
	}
	mux.HandleFunc("POST /Marti/api/citrap", s.authed(s.citrapPost))
	mux.HandleFunc("GET /Marti/api/citrap", s.authed(s.citrapSearch))
	mux.HandleFunc("GET /Marti/api/citrap/{id}", s.authed(s.citrapGet))
	mux.HandleFunc("PUT /Marti/api/citrap/{id}", s.authed(s.citrapPut))
	mux.HandleFunc("DELETE /Marti/api/citrap/{id}", s.authed(s.citrapDelete))
	mux.HandleFunc("POST /Marti/api/citrap/{id}/attachment", s.authed(s.citrapAttachment))
}

// citrapReport is the root element of a report's XML.
type citrapReport struct {
	ID, Type, Title, UserCallsign, UserDescription, DateTime, DateTimeDescription string
	Location, LocationDescription, EventScale, Importance, Status                 string
}

// parseCitrapReport reads the attributes of the first <report> element.
func parseCitrapReport(x []byte) (citrapReport, error) {
	d := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(x, []byte("\xef\xbb\xbf"))))
	d.Strict = false
	for {
		tok, err := d.Token()
		if err != nil {
			return citrapReport{}, errors.New("no <report> element in the report")
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "report" {
			continue
		}
		var r citrapReport
		for _, a := range se.Attr {
			v := a.Value
			switch a.Name.Local {
			case "id":
				r.ID = v
			case "type":
				r.Type = v
			case "title":
				r.Title = v
			case "userCallsign":
				r.UserCallsign = v
			case "userDescription":
				r.UserDescription = v
			case "dateTime":
				r.DateTime = v
			case "dateTimeDescription":
				r.DateTimeDescription = v
			case "location":
				r.Location = v
			case "locationDescription":
				r.LocationDescription = v
			case "eventScale":
				r.EventScale = v
			case "importance":
				r.Importance = v
			case "status":
				r.Status = v
			}
		}
		return r, nil
	}
}

// setCitrapID gives the report element an id attribute.
func setCitrapID(x []byte, id string) []byte {
	i := bytes.Index(x, []byte("<report"))
	for i >= 0 && i+7 < len(x) && !strings.ContainsRune(" \t\r\n/>", rune(x[i+7])) { // skip <reports
		j := bytes.Index(x[i+7:], []byte("<report"))
		if j < 0 {
			return x
		}
		i += 7 + j
	}
	if i < 0 {
		return x
	}
	attr := ` id="` + xmlEscape(id) + `"`
	return append(append(append([]byte{}, x[:i+7]...), attr...), x[i+7:]...)
}

// citrapTimeLayouts are the report dateTime and search time forms.
var citrapTimeLayouts = []string{"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02T15:04:05.999999999Z", "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05Z", "2006-01-02T15:04:05"}

func parseCitrapTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	for _, l := range citrapTimeLayouts {
		if t, err := time.Parse(l, v); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// wktNumbers pulls the coordinates out of WKT (POINT (lon lat), LINESTRING,
// POLYGON), as lon/lat pairs.
var wktNumber = regexp.MustCompile(`-?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?`)

func wktPoints(wkt string) [][2]float64 {
	nums := wktNumber.FindAllString(wkt, -1)
	var out [][2]float64
	for i := 0; i+1 < len(nums); i += 2 {
		lon, err1 := strconv.ParseFloat(nums[i], 64)
		lat, err2 := strconv.ParseFloat(nums[i+1], 64)
		if err1 == nil && err2 == nil {
			out = append(out, [2]float64{lon, lat})
		}
	}
	return out
}

// reportZipXML finds the report XML in a report zip.
func reportZipXML(zr *zip.Reader) (name string, data []byte, err error) {
	for _, f := range zr.File {
		if !strings.HasSuffix(strings.ToLower(f.Name), ".xml") || f.UncompressedSize64 > 8<<20 {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(rc, 8<<20))
		rc.Close()
		if err == nil && bytes.Contains(b, []byte("<report")) {
			return f.Name, b, nil
		}
	}
	return "", nil, errors.New("no report XML in the zip")
}

// rewriteReportZip copies a report zip, replacing the report XML (when xmlData
// is set) and adding a file (when addName is set).
func rewriteReportZip(src []byte, xmlData []byte, addName string, addData []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		return nil, errors.New("the report isn't a zip")
	}
	xmlName, _, err := reportZipXML(zr)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		if (xmlData != nil && f.Name == xmlName) || (addName != "" && f.Name == addName) {
			continue
		}
		if err := zw.Copy(f); err != nil {
			return nil, err
		}
	}
	if xmlData != nil {
		w, err := zw.Create(xmlName)
		if err != nil {
			return nil, err
		}
		w.Write(xmlData)
	}
	if addName != "" {
		w, err := zw.Create(addName)
		if err != nil {
			return nil, err
		}
		w.Write(addData)
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func newReportID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (s *Server) readCitrapBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.Cfg.MaxUploadMB<<20))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "upload failed", http.StatusBadRequest)
		}
		return nil, false
	}
	return b, true
}

// visibleReport returns the report if id may see it.
func (s *Server) visibleReport(reportID string, id *identity) (*store.Report, bool) {
	rep, ok := s.Store.Report(reportID)
	if !ok || !id.canSee(rep.Groups) {
		return nil, false
	}
	return rep, true
}

func (s *Server) citrapPost(w http.ResponseWriter, r *http.Request, id *identity) {
	body, ok := s.readCitrapBody(w, r)
	if !ok {
		return
	}
	rep, status, err := s.saveReport(r, id, "", body)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Location", baseURL(r)+"/Marti/api/citrap/"+rep.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"id": rep.ID})
}

// saveReport adds a report, or updates one (wantID set, or the zip's report
// has the ID of an existing one). It returns the HTTP status to answer.
func (s *Server) saveReport(r *http.Request, id *identity, wantID string, zipData []byte) (*store.Report, int, error) {
	if id.Anonymous {
		return nil, http.StatusForbidden, errors.New("sign in to add reports")
	}
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("the report must be a zip")
	}
	_, xmlData, err := reportZipXML(zr)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	parsed, err := parseCitrapReport(xmlData)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if parsed.ID == "" {
		parsed.ID = firstNonEmpty(wantID, newReportID())
		xmlData = setCitrapID(xmlData, parsed.ID)
		if zipData, err = rewriteReportZip(zipData, xmlData, "", nil); err != nil {
			return nil, http.StatusBadRequest, err
		}
	}
	if !citrapIDPattern.MatchString(parsed.ID) || (wantID != "" && !strings.EqualFold(wantID, parsed.ID)) {
		return nil, http.StatusBadRequest, errors.New("invalid report id")
	}
	clientUID := r.URL.Query().Get("clientUid")
	old, exists := s.Store.Report(parsed.ID)
	if exists && !id.canSee(old.Groups) {
		return nil, http.StatusForbidden, errors.New("a report with this id belongs to other groups")
	}
	if wantID != "" && !exists {
		return nil, http.StatusNotFound, errors.New("no such report")
	}

	p, err := s.savePackage(bytes.NewReader(zipData), parsed.ID+".zip", "application/zip", clientUID, []string{"citrap"}, "citrap", id)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	when, ok := parseCitrapTime(parsed.DateTime)
	if !ok {
		when = time.Now().UTC()
	}
	now := time.Now().UTC()
	rep := store.Report{
		ID: parsed.ID, Type: parsed.Type, Title: parsed.Title, UserCallsign: parsed.UserCallsign, UserDescription: parsed.UserDescription,
		DateTime: when, DateTimeDescription: parsed.DateTimeDescription, Location: parsed.Location,
		LocationDescription: parsed.LocationDescription, EventScale: parsed.EventScale, Importance: parsed.Importance,
		Status: parsed.Status, XML: string(xmlData), Hash: p.Hash, Groups: p.Groups, CreatorUID: clientUID,
		Submitter: id.Username, Created: now, Updated: now,
	}
	if exists {
		rep.Groups, rep.CreatorUID, rep.Submitter, rep.Created = old.Groups, old.CreatorUID, old.Submitter, old.Created
		// Like TAK Server, earlier versions stay (the citrap mission lists
		// the first) until the report is deleted.
		rep.OldHashes = old.OldHashes
		if old.Hash != p.Hash && !slices.Contains(rep.OldHashes, old.Hash) {
			rep.OldHashes = append(rep.OldHashes, old.Hash)
		}
	}
	if err := s.Store.PutReport(rep); err != nil {
		return nil, http.StatusInternalServerError, err
	}

	if s.Missions != nil {
		sys := mission.Caller{Username: id.Username, Admin: true, ClientUID: clientUID}
		if !exists {
			if _, err := s.Missions.AddContents(citrapMission, sys, []string{p.Hash}, nil); err != nil {
				s.Log.Warn("report not added to the citrap mission", "report", rep.ID, "err", err)
			}
			user := mission.Caller{Username: id.Username, Groups: rep.Groups, OutGroups: rep.Groups, ClientUID: clientUID}
			if _, _, err := s.Missions.Create(mission.CreateParams{Name: rep.ID, CreatorUID: clientUID, Tool: "citrap", Groups: rep.Groups}, user); err != nil && !errors.Is(err, mission.ErrExists) {
				s.Log.Warn("report mission not created", "report", rep.ID, "err", err)
			}
		}
		// Subscribers of the report's mission hear of the new version.
		if _, err := s.Missions.AddContents(rep.ID, sys, []string{p.Hash}, nil); err != nil {
			s.Log.Warn("report not added to its mission", "report", rep.ID, "err", err)
		}
		if exists && clientUID != "" {
			s.Missions.Subscribe(rep.ID, missionCaller(r, id), "", time.Time{})
		}
	}
	verb, status := "added", http.StatusCreated
	if exists {
		verb, status = "updated", http.StatusOK
	}
	s.Log.Info("report "+verb, "report", rep.ID, "type", rep.Type, "title", rep.Title, "user", id.Username)
	return &rep, status, nil
}

// dropReportPackage deletes a report's old zip, unless something else uses it.
func (s *Server) dropReportPackage(hash, reportID string) {
	p, ok := s.Store.Package(hash)
	if !ok || p.Tool != "citrap" {
		return
	}
	for _, rep := range s.Store.Reports() {
		if rep.ID != reportID && (rep.Hash == hash || slices.Contains(rep.OldHashes, hash)) {
			return
		}
	}
	if s.Missions != nil {
		s.Missions.RemoveContent(citrapMission, mission.Caller{Admin: true}, hash, "")
	}
	if s.Store.DeletePackage(hash) == nil {
		os.Remove(filepath.Join(s.PackageDir, hash))
	}
}

func (s *Server) citrapSearch(w http.ResponseWriter, r *http.Request, id *identity) {
	q := r.URL.Query()
	var keywords, types, callsigns []string
	for _, k := range strings.Split(q.Get("keywords"), ",") {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			keywords = append(keywords, k)
		}
	}
	for _, t := range strings.Split(q.Get("type"), ",") {
		if t != "" {
			types = append(types, t)
		}
	}
	for _, c := range strings.Split(q.Get("callsign"), ",") {
		if c != "" {
			callsigns = append(callsigns, c)
		}
	}
	var start, end time.Time
	if v := q.Get("startTime"); v != "" {
		t, ok := parseCitrapTime(v)
		if !ok {
			http.Error(w, "invalid startTime", http.StatusBadRequest)
			return
		}
		start = t
	}
	if v := q.Get("endTime"); v != "" {
		t, ok := parseCitrapTime(v)
		if !ok {
			http.Error(w, "invalid endTime", http.StatusBadRequest)
			return
		}
		end = t
	}
	var box []float64
	if v := q.Get("bbox"); v != "" {
		for _, f := range strings.Fields(strings.ReplaceAll(v, ",", " ")) {
			n, err := strconv.ParseFloat(f, 64)
			if err != nil {
				http.Error(w, "invalid bbox", http.StatusBadRequest)
				return
			}
			box = append(box, n)
		}
		if len(box) != 4 {
			http.Error(w, "bbox must be 4 numbers", http.StatusBadRequest)
			return
		}
	}
	limit := -1
	if v := q.Get("maxReportCount"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "invalid maxReportCount", http.StatusBadRequest)
			return
		}
		limit = n
	}

	var found []*store.Report
	for _, rep := range s.Store.Reports() {
		if limit >= 0 && len(found) >= limit {
			break
		}
		if !id.canSee(rep.Groups) ||
			(len(types) > 0 && !slices.Contains(types, rep.Type)) ||
			(len(callsigns) > 0 && !slices.Contains(callsigns, rep.UserCallsign)) ||
			(!start.IsZero() && rep.DateTime.Before(start)) || (!end.IsZero() && rep.DateTime.After(end)) ||
			(box != nil && !reportInBox(rep.Location, box)) {
			continue
		}
		if len(keywords) > 0 {
			hay := strings.ToLower(strings.Join([]string{rep.Type, rep.Title, rep.UserCallsign, rep.UserDescription, rep.DateTimeDescription,
				rep.LocationDescription, rep.EventScale, rep.Importance, rep.ID, rep.Hash}, "\x00"))
			match := false
			for _, k := range keywords {
				if strings.Contains(hay, k) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		found = append(found, rep)
	}

	if strings.EqualFold(q.Get("subscribe"), "true") && q.Get("clientUid") != "" && s.Missions != nil {
		c := missionCaller(r, id)
		for _, rep := range found {
			s.Missions.Subscribe(rep.ID, c, "", time.Time{})
		}
	}

	accept := strings.ToLower(r.Header.Get("Accept"))
	if accept != "" && !strings.Contains(accept, "*/*") && !strings.Contains(accept, "application/json") {
		if !strings.Contains(accept, "application/xml") {
			http.Error(w, "unsupported Accept", http.StatusBadRequest)
			return
		}
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><reports>`)
		for _, rep := range found {
			fmt.Fprintf(&b, `<report id="%s" type="%s" title="%s" userCallsign="%s" userDescription="%s" dateTime="%s" dateTimeDescription="%s" location="%s" locationDescription="%s" eventScale="%s" importance="%s"`,
				xmlEscape(rep.ID), xmlEscape(rep.Type), xmlEscape(rep.Title), xmlEscape(rep.UserCallsign), xmlEscape(rep.UserDescription),
				citrapTime(rep.DateTime), xmlEscape(rep.DateTimeDescription), xmlEscape(rep.Location), xmlEscape(rep.LocationDescription),
				xmlEscape(rep.EventScale), xmlEscape(rep.Importance))
			if rep.Status != "" {
				fmt.Fprintf(&b, ` status="%s"`, xmlEscape(rep.Status))
			}
			b.WriteString("/>")
		}
		b.WriteString("</reports>")
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, b.String())
		return
	}
	out := make([]map[string]any, 0, len(found))
	for _, rep := range found {
		m := map[string]any{
			"id": rep.ID, "type": rep.Type, "title": rep.Title, "userCallsign": rep.UserCallsign, "userDescription": rep.UserDescription,
			"dateTime": citrapTime(rep.DateTime), "dateTimeDescription": rep.DateTimeDescription, "location": rep.Location,
			"locationDescription": rep.LocationDescription, "eventScale": rep.EventScale, "importance": rep.Importance,
		}
		if rep.Status != "" {
			m["status"] = rep.Status
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, out)
}

// citrapTime is TAK Server's CoT time form for reports.
func citrapTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// reportInBox reports whether a report's location touches the box (two
// lon/lat corners, in either order).
func reportInBox(wkt string, box []float64) bool {
	pts := wktPoints(wkt)
	if len(pts) == 0 {
		return false
	}
	minLon, maxLon := math.Min(box[0], box[2]), math.Max(box[0], box[2])
	minLat, maxLat := math.Min(box[1], box[3]), math.Max(box[1], box[3])
	lo, hi := pts[0], pts[0]
	for _, p := range pts {
		lo = [2]float64{math.Min(lo[0], p[0]), math.Min(lo[1], p[1])}
		hi = [2]float64{math.Max(hi[0], p[0]), math.Max(hi[1], p[1])}
	}
	return lo[0] <= maxLon && hi[0] >= minLon && lo[1] <= maxLat && hi[1] >= minLat
}

func (s *Server) citrapGet(w http.ResponseWriter, r *http.Request, id *identity) {
	rep, ok := s.visibleReport(r.PathValue("id"), id)
	if !ok {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	if q := r.URL.Query(); q.Get("clientUid") != "" && s.Missions != nil {
		s.Missions.Subscribe(rep.ID, missionCaller(r, id), "", time.Time{})
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	if strings.Contains(accept, "application/xml") && !strings.Contains(accept, "application/zip") && !strings.Contains(accept, "*/*") {
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": rep.ID + ".xml"}))
		io.WriteString(w, rep.XML)
		return
	}
	f, err := os.Open(filepath.Join(s.PackageDir, rep.Hash))
	if err != nil {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": rep.ID + ".zip"}))
	http.ServeContent(w, r, "", rep.Updated, f)
}

func (s *Server) citrapPut(w http.ResponseWriter, r *http.Request, id *identity) {
	rep, ok := s.visibleReport(r.PathValue("id"), id)
	if !ok {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	body, ok := s.readCitrapBody(w, r)
	if !ok {
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/xml" || mt == "text/xml" {
		old, err := os.ReadFile(filepath.Join(s.PackageDir, rep.Hash))
		if err != nil {
			http.Error(w, "report not found", http.StatusNotFound)
			return
		}
		if body, err = rewriteReportZip(old, body, "", nil); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if _, status, err := s.saveReport(r, id, rep.ID, body); err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) citrapAttachment(w http.ResponseWriter, r *http.Request, id *identity) {
	rep, ok := s.visibleReport(r.PathValue("id"), id)
	if !ok {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	cd := r.Header.Get("Content-Disposition")
	name := ""
	if _, params, err := mime.ParseMediaType(cd); err == nil {
		name = params["filename"]
	} else if _, v, ok := strings.Cut(cd, "="); ok {
		name = strings.Trim(strings.TrimSpace(v), `"`)
	}
	name = path.Clean("/" + strings.ReplaceAll(name, `\`, "/"))[1:]
	if name == "" || name == "." {
		http.Error(w, "the attachment needs a filename in Content-Disposition", http.StatusBadRequest)
		return
	}
	body, ok := s.readCitrapBody(w, r)
	if !ok {
		return
	}
	old, err := os.ReadFile(filepath.Join(s.PackageDir, rep.Hash))
	if err != nil {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	updated, err := rewriteReportZip(old, nil, name, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, status, err := s.saveReport(r, id, rep.ID, updated); err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) citrapDelete(w http.ResponseWriter, r *http.Request, id *identity) {
	rep, ok := s.visibleReport(r.PathValue("id"), id)
	if !ok {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	if id.Anonymous {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := s.Store.DeleteReport(rep.ID); err != nil {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	for _, h := range append(rep.OldHashes, rep.Hash) {
		s.dropReportPackage(h, rep.ID)
	}
	if s.Missions != nil {
		sys := mission.Caller{Username: id.Username, Admin: true, ClientUID: r.URL.Query().Get("clientUid")}
		s.Missions.Delete(rep.ID, sys)
	}
	s.Log.Info("report deleted", "report", rep.ID, "title", rep.Title, "user", id.Username)
	w.WriteHeader(http.StatusOK)
}
