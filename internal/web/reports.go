package web

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// The console's Reports page: the ATAK Reports plugin's reports (see
// citrap.go), with what was filled in, their attachments and their place on
// the map.

// reportView is a report as the console shows it.
type reportView struct {
	ID                  string             `json:"id"`
	Type                string             `json:"type"`
	Title               string             `json:"title"`
	Callsign            string             `json:"callsign"`
	Description         string             `json:"description"`
	DateTime            time.Time          `json:"date_time"`
	DateTimeDescription string             `json:"date_time_description"`
	Location            string             `json:"location"`
	Lat                 *float64           `json:"lat,omitempty"`
	Lon                 *float64           `json:"lon,omitempty"`
	LocationDescription string             `json:"location_description"`
	EventScale          string             `json:"event_scale"`
	Importance          string             `json:"importance"`
	Status              string             `json:"status"`
	Groups              []string           `json:"groups"`
	Submitter           string             `json:"submitter"`
	CreatorUID          string             `json:"creator_uid"`
	Created             time.Time          `json:"created"`
	Updated             time.Time          `json:"updated"`
	Versions            int                `json:"versions"`
	Fields              []reportField      `json:"fields"`
	Attachments         []reportAttachment `json:"attachments"`
}

// reportField is one answer on the report's form.
type reportField struct {
	Section string `json:"section,omitempty"`
	Title   string `json:"title"`
	Value   string `json:"value"`
}

type reportAttachment struct {
	Name string `json:"name"`
	Size uint64 `json:"size"`
}

func (s *Server) reportAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/reports", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		out := []reportView{}
		for _, rep := range s.Store.Reports() {
			out = append(out, s.reportView(rep))
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("GET /admin/api/reports/{id}/files/{name...}", s.admin(s.reportFile))
	mux.HandleFunc("DELETE /admin/api/reports/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		rep, ok := s.Store.Report(r.PathValue("id"))
		if !ok || s.deleteReport(rep, id.Username, "") != nil {
			jsonError(w, http.StatusNotFound, "report not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (s *Server) reportView(rep *store.Report) reportView {
	v := reportView{ID: rep.ID, Type: rep.Type, Title: rep.Title, Callsign: rep.UserCallsign, Description: rep.UserDescription,
		DateTime: rep.DateTime, DateTimeDescription: rep.DateTimeDescription, Location: rep.Location,
		LocationDescription: rep.LocationDescription, EventScale: rep.EventScale, Importance: rep.Importance, Status: rep.Status,
		Groups: rep.Groups, Submitter: rep.Submitter, CreatorUID: rep.CreatorUID, Created: rep.Created, Updated: rep.Updated,
		Versions: len(rep.OldHashes) + 1, Fields: reportFields([]byte(rep.XML)), Attachments: []reportAttachment{}}
	if pts := wktPoints(rep.Location); len(pts) > 0 {
		lon, lat := pts[0][0], pts[0][1]
		if lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 {
			v.Lat, v.Lon = &lat, &lon
		}
	}
	if zr, xmlName, err := s.openReportZip(rep); err == nil {
		for _, f := range zr.File {
			if f.Name != xmlName && !strings.HasSuffix(f.Name, "/") {
				v.Attachments = append(v.Attachments, reportAttachment{Name: f.Name, Size: f.UncompressedSize64})
			}
		}
	}
	return v
}

func (s *Server) openReportZip(rep *store.Report) (*zip.Reader, string, error) {
	f, err := os.Open(filepath.Join(s.PackageDir, rep.Hash))
	if err != nil {
		return nil, "", err
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return nil, "", err
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, "", err
	}
	name, _, err := reportZipXML(zr)
	return zr, name, err
}

// reportFile serves one of a report's attachments. Only images are shown in
// the browser (not SVG, which can carry script); anything else downloads.
func (s *Server) reportFile(w http.ResponseWriter, r *http.Request, _ *identity) {
	rep, ok := s.Store.Report(r.PathValue("id"))
	if !ok {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	zr, _, err := s.openReportZip(rep)
	if err != nil {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	name := r.PathValue("name")
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			http.Error(w, "can't read the attachment", http.StatusInternalServerError)
			return
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			http.Error(w, "can't read the attachment", http.StatusInternalServerError)
			return
		}
		ct := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
		disp := "attachment"
		if strings.HasPrefix(ct, "image/") && !strings.Contains(ct, "svg") {
			disp = "inline"
		} else {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": path.Base(name)}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox")
		http.ServeContent(w, r, "", rep.Updated, bytes.NewReader(data))
		return
	}
	http.Error(w, "attachment not found", http.StatusNotFound)
}

// reportFields lists what was filled in on a report's form: options with a
// value or ticked, and the choices picked in lists, under their section.
func reportFields(x []byte) []reportField {
	d := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(x, []byte("\xef\xbb\xbf"))))
	d.Strict = false
	attr := func(se xml.StartElement, name string) string {
		for _, a := range se.Attr {
			if a.Name.Local == name {
				return strings.TrimSpace(a.Value)
			}
		}
		return ""
	}
	type list struct {
		title  string
		picked []string
	}
	var sections []string
	var lists []*list
	var open []string // element names, to know what an end tag closes
	out := []reportField{}
	for {
		tok, err := d.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			open = append(open, t.Name.Local)
			switch t.Name.Local {
			case "section":
				sections = append(sections, attr(t, "title"))
			case "list":
				lists = append(lists, &list{title: attr(t, "title")})
			case "option":
				title, val := attr(t, "title"), attr(t, "value")
				if val == "" {
					val = attr(t, "spinnerValue")
				}
				selected := strings.EqualFold(attr(t, "selected"), "true")
				if len(lists) > 0 {
					if selected {
						l := lists[len(lists)-1]
						l.picked = append(l.picked, firstNonEmpty(val, title))
					}
					continue
				}
				if val == "" && selected {
					val = "✓"
				}
				if val == "" {
					continue
				}
				if u := attr(t, "unitValue"); u != "" {
					val += " " + u
				}
				out = append(out, reportField{Section: sectionPath(sections), Title: title, Value: val})
			}
		case xml.EndElement:
			if len(open) == 0 {
				continue
			}
			name := open[len(open)-1]
			open = open[:len(open)-1]
			switch {
			case name == "section" && len(sections) > 0:
				sections = sections[:len(sections)-1]
			case name == "list" && len(lists) > 0:
				l := lists[len(lists)-1]
				lists = lists[:len(lists)-1]
				if len(l.picked) > 0 {
					out = append(out, reportField{Section: sectionPath(sections), Title: l.title, Value: strings.Join(l.picked, ", ")})
				}
			}
		}
	}
}

func sectionPath(sections []string) string {
	var parts []string
	for _, s := range sections {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " › ")
}
