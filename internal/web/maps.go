package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Custom map sources for the console map, shared by all administrators.
// ATAK map source files (<customMapSource>) can be imported as they are.
// A source marked for devices is also sent to ATAK as a map source: in the
// connection profile of the devices in its groups (so devices that were off
// get it when they connect), and straight away as a data package to those
// connected when it is added or changed.

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
	old := map[string]store.MapSource{}
	for _, m := range s.Store.MapSources() {
		old[m.Name] = m
	}
	now := time.Now().UTC()
	var changed []store.MapSource
	for i := range in {
		if err := cleanMapSource(&in[i]); err != nil {
			jsonError(w, http.StatusBadRequest, in[i].Name+": "+err.Error())
			return
		}
		if o, ok := old[in[i].Name]; ok && sameForDevices(o, in[i]) {
			in[i].Updated = o.Updated
			continue
		}
		in[i].Updated = now
		if in[i].Devices {
			changed = append(changed, in[i])
		}
	}
	if err := s.Store.SetMapSources(in); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("map sources changed", "admin", id.Username, "count", len(in))
	for _, m := range changed {
		s.pushMapSource(m)
	}
	writeJSON(w, http.StatusOK, in)
}

// sameForDevices reports whether devices would get the same map source.
func sameForDevices(a, b store.MapSource) bool {
	return a.URL == b.URL && a.MinZoom == b.MinZoom && a.MaxZoom == b.MaxZoom && a.Subdomains == b.Subdomains &&
		a.Devices == b.Devices && slices.Equal(a.Groups, b.Groups)
}

// atakMapSource is a map source as an ATAK <customMapSource> file.
func atakMapSource(m store.MapSource) []byte {
	u := strings.NewReplacer("{z}", "{$z}", "{x}", "{$x}", "{y}", "{$y}", "{-y}", "{$y}", "{s}", "{$serverpart}", "{r}", "").Replace(m.URL)
	tileType := "png"
	if p, err := url.Parse(strings.NewReplacer("{", "", "}", "", "$", "").Replace(m.URL)); err == nil {
		if ext := strings.ToLower(path.Ext(p.Path)); ext == ".jpg" || ext == ".jpeg" {
			tileType = "jpg"
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<customMapSource>
  <name>%s</name>
  <minZoom>%d</minZoom>
  <maxZoom>%d</maxZoom>
  <tileType>%s</tileType>
  <tileUpdate>false</tileUpdate>
  <url>%s</url>
`, xmlEscape(m.Name), m.MinZoom, m.MaxZoom, tileType, xmlEscape(u))
	if strings.Contains(m.URL, "{s}") {
		parts := strings.Split(m.Subdomains, ",")
		if !strings.Contains(m.Subdomains, ",") {
			parts = strings.Split(m.Subdomains, "")
		}
		fmt.Fprintf(&b, "  <serverParts>%s</serverParts>\n", xmlEscape(strings.Join(parts, " ")))
	}
	if strings.Contains(m.URL, "{-y}") {
		b.WriteString("  <invertYCoordinate>true</invertYCoordinate>\n")
	}
	b.WriteString("  <backgroundColor>#000000</backgroundColor>\n</customMapSource>\n")
	return []byte(b.String())
}

func mapSourceFile(m store.MapSource) string { return "EdgeTAK-source-" + sanitize(m.Name) + ".xml" }

// customMapSourceEntries are the map sources meant for a user with these
// groups, for device profiles; since limits them to ones changed after it.
func (s *Server) customMapSourceEntries(groups []string, since time.Time) []profileEntry {
	var out []profileEntry
	for _, m := range s.Store.MapSources() {
		if !m.Devices || !watched(store.LostContactSettings{Groups: m.Groups}, groups) || (!since.IsZero() && !m.Updated.After(since)) {
			continue
		}
		out = append(out, profileEntry{name: mapSourceFile(m), data: atakMapSource(m)})
	}
	return out
}

// pushMapSource offers a map source to the connected devices it is for.
func (s *Server) pushMapSource(m store.MapSource) {
	var targets []*router.Client
	for _, c := range s.Router.Clients() {
		if watched(store.LostContactSettings{Groups: m.Groups}, deviceGroups(c)) {
			targets = append(targets, c)
		}
	}
	if len(targets) == 0 {
		return
	}
	var buf bytes.Buffer
	if err := writeProfilePackage(&buf, "edgetak-mapsource-"+sanitize(m.Name), "Map source "+m.Name,
		[]profileEntry{{name: mapSourceFile(m), data: atakMapSource(m)}}); err != nil {
		return
	}
	f := s.pushed.add(buf.Bytes(), "Map source "+sanitize(m.Name)+".zip", m.Groups)
	p := &store.Package{Name: f.name, Hash: f.hash, Size: int64(len(f.data))}
	n := 0
	for _, c := range targets {
		if url := s.contentURL(c.Transport, f.hash); url != "" {
			c.Send(s.fileShareEvent(s.Cfg.ServerName, url, p))
			n++
		}
	}
	s.Log.Info("map source sent", "source", m.Name, "devices", n)
}

// pushedFiles are small generated data packages offered to connected
// devices, kept for an hour so they can download them.
type pushedFiles struct {
	mu    sync.Mutex
	files map[string]pushedFile
}

type pushedFile struct {
	hash, name string
	data       []byte
	groups     []string
	at         time.Time
}

func (p *pushedFiles) add(data []byte, name string, groups []string) pushedFile {
	sum := sha256.Sum256(data)
	f := pushedFile{hash: hex.EncodeToString(sum[:]), name: name, data: data, groups: groups, at: time.Now()}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.files == nil {
		p.files = map[string]pushedFile{}
	}
	for h, old := range p.files {
		if time.Since(old.at) > time.Hour {
			delete(p.files, h)
		}
	}
	p.files[f.hash] = f
	return f
}

// get is the file with this hash, if id may download it.
func (p *pushedFiles) get(hash string, id *identity) (pushedFile, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f, ok := p.files[hash]
	if !ok || time.Since(f.at) > time.Hour || (len(f.groups) > 0 && !id.canSee(f.groups)) {
		return pushedFile{}, false
	}
	return f, true
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
	m.Groups = cleanGroups(m.Groups)
	slices.Sort(m.Groups)
	if !m.Devices {
		m.Groups = nil
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
