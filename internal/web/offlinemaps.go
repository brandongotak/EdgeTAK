package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/maps"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Offline maps (Maps tab): MBTiles files in <data>/maps, uploaded or copied
// there. Their tiles are served at /tiles/<file>/<z>/<x>/<y> to the console
// and to devices, which get each map as an ATAK map source in their device
// profile; ATAK fetches the tiles over HTTPS with its certificate.

const maxMapUpload = 64 << 30

var mapFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()+-]{0,120}\.mbtiles$`)

func (s *Server) mapsDir() string { return filepath.Join(s.DataDir, "maps") }

type offlineMapView struct {
	*maps.Map
	Devices bool   `json:"devices"`
	URL     string `json:"url"` // tile URL template for the console
}

func (s *Server) offlineMapsView() map[string]any {
	list := []offlineMapView{}
	if s.Maps != nil {
		for _, m := range s.Maps.Maps() {
			list = append(list, offlineMapView{Map: m, Devices: !s.Store.OfflineMap(m.File).NoDevices,
				URL: "/tiles/" + url.PathEscape(m.File) + "/{z}/{x}/{y}"})
		}
	}
	folder, err := filepath.Abs(s.mapsDir())
	if err != nil {
		folder = s.mapsDir()
	}
	return map[string]any{"maps": list, "folder": folder}
}

func (s *Server) offlineMapRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /tiles/{file}/{z}/{x}/{y}", s.authed(s.tile))

	mux.HandleFunc("GET /admin/api/maps", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.offlineMapsView())
	}))
	mux.HandleFunc("POST /admin/api/maps/rescan", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if err := s.Maps.Scan(); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.offlineMapsView())
	}))
	// Upload: the file is the body, its name in ?name=.
	mux.HandleFunc("POST /admin/api/maps", s.admin(s.uploadMap))
	mux.HandleFunc("PUT /admin/api/maps/{file}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		file := r.PathValue("file")
		if _, ok := s.Maps.Get(file); !ok {
			jsonError(w, http.StatusNotFound, "no such map")
			return
		}
		var req struct {
			Devices bool `json:"devices"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if err := s.Store.SetOfflineMap(file, store.OfflineMap{NoDevices: !req.Devices}); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("offline map changed", "admin", id.Username, "map", file, "devices", req.Devices)
		writeJSON(w, http.StatusOK, s.offlineMapsView())
	}))
	mux.HandleFunc("DELETE /admin/api/maps/{file}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		file := r.PathValue("file")
		if err := s.Maps.Remove(file); err != nil {
			jsonError(w, http.StatusNotFound, "no such map")
			return
		}
		s.Store.DeleteOfflineMap(file)
		s.Log.Info("offline map deleted", "admin", id.Username, "map", file)
		writeJSON(w, http.StatusOK, s.offlineMapsView())
	}))
}

func (s *Server) tile(w http.ResponseWriter, r *http.Request, _ *identity) {
	m, ok := s.Maps.Get(r.PathValue("file"))
	if !ok || m.Error != "" {
		http.NotFound(w, r)
		return
	}
	z, err1 := strconv.Atoi(r.PathValue("z"))
	x, err2 := strconv.Atoi(r.PathValue("x"))
	ys := r.PathValue("y")
	if i := strings.IndexByte(ys, '.'); i >= 0 {
		ys = ys[:i] // ATAK and Leaflet may add an extension
	}
	y, err3 := strconv.Atoi(ys)
	if err1 != nil || err2 != nil || err3 != nil {
		http.NotFound(w, r)
		return
	}
	data, err := m.Tile(z, x, y)
	if errors.Is(err, maps.ErrNoTile) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", m.ContentType())
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(data)
}

func (s *Server) uploadMap(w http.ResponseWriter, r *http.Request, id *identity) {
	name := filepath.Base(strings.ReplaceAll(r.URL.Query().Get("name"), `\`, "/"))
	if !strings.EqualFold(filepath.Ext(name), ".mbtiles") {
		jsonError(w, http.StatusBadRequest, "upload an .mbtiles file")
		return
	}
	name = strings.TrimSuffix(name, filepath.Ext(name)) + ".mbtiles"
	if !mapFileName.MatchString(name) {
		jsonError(w, http.StatusBadRequest, "map file names are letters, digits, spaces and . _ ( ) + - (up to 120 characters)")
		return
	}
	if err := os.MkdirAll(s.mapsDir(), 0o755); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmp, err := os.CreateTemp(s.mapsDir(), ".upload-*")
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, maxMapUpload))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		jsonError(w, http.StatusBadRequest, "upload failed: "+err.Error())
		return
	}
	// Check it before it replaces anything.
	if m := maps.Open(tmp.Name()); m.Error != "" {
		jsonError(w, http.StatusBadRequest, name+": "+m.Error)
		return
	} else {
		m.Close()
	}
	final := filepath.Join(s.mapsDir(), name)
	if _, ok := s.Maps.Get(name); ok {
		s.Maps.Remove(name) // replaced by the new upload
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Maps.Scan(); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Store.SetOfflineMap(name, s.Store.OfflineMap(name)) // marks it updated for devices
	s.Log.Info("offline map added", "admin", id.Username, "map", name, "bytes", n)
	writeJSON(w, http.StatusOK, s.offlineMapsView())
}

// mapSourceEntries are the ATAK map sources for the offline maps sent to
// devices, for device profiles; since limits them to maps changed after it.
func (s *Server) mapSourceEntries(host string, since time.Time) []profileEntry {
	if s.Maps == nil {
		return nil
	}
	_, port, err := net.SplitHostPort(s.Addrs["https"])
	if err != nil || host == "" {
		return nil
	}
	var out []profileEntry
	for _, m := range s.Maps.Maps() {
		set := s.Store.OfflineMap(m.File)
		if m.Error != "" || set.NoDevices {
			continue
		}
		if !since.IsZero() && !m.Modified.After(since) && !set.Updated.After(since) {
			continue
		}
		u := "https://" + net.JoinHostPort(host, port) + "/tiles/" + url.PathEscape(m.File) + "/{$z}/{$x}/{$y}"
		xml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<customMapSource>
  <name>%s</name>
  <minZoom>%d</minZoom>
  <maxZoom>%d</maxZoom>
  <tileType>%s</tileType>
  <tileUpdate>None</tileUpdate>
  <url>%s</url>
  <backgroundColor>#000000</backgroundColor>
</customMapSource>
`, xmlEscape(m.Name), m.MinZoom, m.MaxZoom, m.Format, xmlEscape(u))
		out = append(out, profileEntry{name: "EdgeTAK-" + strings.TrimSuffix(sanitize(m.File), ".mbtiles") + ".xml", data: []byte(xml)})
	}
	return out
}
