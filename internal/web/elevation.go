package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/elevation"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Elevation (Data → Elevation): DTED tiles for ATAK. Admins upload tiles;
// ATAK's automatic elevation downloads (normally from tak.gov) can come from
// this server instead, which needs the trusted (Let's Encrypt) certificate
// on the enrollment port, since ATAK checks it against the phone's own CAs;
// and higher-resolution tiles go to devices as a data package ATAK imports.

const maxElevationPackage = 2 << 30

func (s *Server) elevation() *elevation.Store {
	s.elevOnce.Do(func() {
		s.elev = &elevation.Store{Dir: filepath.Join(filepath.Dir(s.PackageDir), "elevation", "DTED")}
	})
	return s.elev
}

// elevationStreamHost is what devices put in ATAK's elevation server
// setting, or "" with why it can't be used.
func (s *Server) elevationStreamHost() (string, string) {
	if !s.Public.Uses("enroll") {
		return "", "needs a trusted certificate on the enrollment port (Let's Encrypt, on the Settings page): ATAK only downloads elevation from servers the phone itself trusts"
	}
	host := s.Cfg.PublicHost
	if host == "" {
		return "", "set the server's public host first"
	}
	if p := port(s.Addrs["enroll"]); p != "" && p != "443" {
		host += ":" + p
	}
	return host, ""
}

// elevationPrefs is the setting device profiles carry when it changed since
// since: this server for automatic elevation downloads, or tak.gov again.
func (s *Server) elevationPrefs(since time.Time) string {
	set := s.Store.Elevation()
	if !set.Configured || (!since.IsZero() && !set.Updated.After(since)) {
		return ""
	}
	server := "tak.gov"
	if host, _ := s.elevationStreamHost(); set.Stream && host != "" {
		server = host
	}
	return `<entry key="prefs_dted_stream" class="class java.lang.Boolean">true</entry>` +
		`<entry key="prefs_dted_stream_server" class="class java.lang.String">` + xmlEscape(server) + `</entry>`
}

type elevationLevel struct {
	Level int   `json:"level"`
	Tiles int   `json:"tiles"`
	Bytes int64 `json:"bytes"`
}

func (s *Server) elevationView() map[string]any {
	tiles := s.elevation().Tiles()
	levels := []elevationLevel{{Level: 0}, {Level: 1}, {Level: 2}}
	cells := make([][3]int, 0, len(tiles)) // level, lat, lon: compact for big sets
	for _, t := range tiles {
		levels[t.Level].Tiles++
		levels[t.Level].Bytes += t.Size
		cells = append(cells, [3]int{t.Level, t.Lat, t.Lon})
	}
	host, note := s.elevationStreamHost()
	return map[string]any{"levels": levels, "tiles": cells, "stream": s.Store.Elevation().Stream,
		"stream_host": host, "stream_note": note}
}

func (s *Server) elevationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/elevation", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.elevationView())
	}))
	mux.HandleFunc("POST /admin/api/elevation", s.admin(s.uploadElevation))
	mux.HandleFunc("DELETE /admin/api/elevation/levels/{level}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		level, err := strconv.Atoi(r.PathValue("level"))
		if err != nil || level < 0 || level > 2 {
			jsonError(w, http.StatusBadRequest, "the level is 0, 1 or 2")
			return
		}
		n := s.elevation().DeleteLevel(level)
		s.Log.Info("elevation tiles deleted", "admin", id.Username, "level", level, "tiles", n)
		writeJSON(w, http.StatusOK, s.elevationView())
	}))
	mux.HandleFunc("PUT /admin/api/elevation/settings", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if _, why := s.elevationStreamHost(); req.Stream && why != "" {
			jsonError(w, http.StatusBadRequest, "Streaming elevation "+why)
			return
		}
		set := s.Store.Elevation()
		set.Stream, set.Updated = req.Stream, time.Now().UTC()
		set.Configured = set.Configured || req.Stream
		if err := s.Store.SetElevation(set); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("elevation streaming changed", "admin", id.Username, "stream", req.Stream)
		writeJSON(w, http.StatusOK, s.elevationView())
	}))
	mux.HandleFunc("POST /admin/api/elevation/send", s.admin(s.sendElevation))

	// What ATAK downloads, as from tak.gov: /elevation/DTED/w078/n38.dt0.zip.
	// Elevation isn't private (tak.gov serves it to anyone), and ATAK sends
	// no credentials for it.
	mux.HandleFunc("GET /elevation/DTED/{lon}/{file}", func(w http.ResponseWriter, r *http.Request) {
		file := r.PathValue("file")
		if !strings.HasSuffix(file, ".zip") {
			http.NotFound(w, r)
			return
		}
		var buf bytes.Buffer
		if err := s.elevation().StreamZip(&buf, r.PathValue("lon")+"/"+strings.TrimSuffix(file, ".zip")); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(buf.Bytes())
	})
}

// uploadElevation imports .dt0/.dt1/.dt2 files and zips of them. Each file
// goes to a temporary file first, so a large zip needn't fit in memory.
func (s *Server) uploadElevation(w http.ResponseWriter, r *http.Request, id *identity) {
	r.Body = http.MaxBytesReader(w, r.Body, s.Cfg.MaxUploadMB<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		jsonError(w, http.StatusBadRequest, "upload DTED files (.dt0, .dt1, .dt2) or zips of them")
		return
	}
	tmpDir := filepath.Dir(s.elevation().Dir)
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total := elevation.ImportResult{Skipped: []string{}}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				jsonError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the upload is over %d MB; upload fewer files at a time", s.Cfg.MaxUploadMB))
				return
			}
			jsonError(w, http.StatusBadRequest, "upload failed")
			return
		}
		if part.FormName() != "files" || part.FileName() == "" {
			part.Close()
			continue
		}
		res, err := s.importElevationPart(tmpDir, part)
		part.Close()
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				jsonError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the upload is over %d MB; upload fewer files at a time", s.Cfg.MaxUploadMB))
				return
			}
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		total.Added += res.Added
		total.Skipped = append(total.Skipped, res.Skipped...)
	}
	s.Log.Info("elevation tiles uploaded", "admin", id.Username, "added", total.Added, "skipped", len(total.Skipped))
	view := s.elevationView()
	view["added"], view["skipped"] = total.Added, total.Skipped
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) importElevationPart(dir string, part *multipart.Part) (elevation.ImportResult, error) {
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return elevation.ImportResult{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(tmp, part)
	if err != nil {
		return elevation.ImportResult{}, err
	}
	return s.elevation().Import(filepath.Base(part.FileName()), tmp, n), nil
}

// sendElevation packs tiles into the zip ATAK imports as elevation data,
// keeps it as a data package, and offers it to devices in the chosen groups
// (connected now; others can download it from the data package list).
func (s *Server) sendElevation(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct {
		Levels []int       `json:"levels"`
		Groups []string    `json:"groups"`
		BBox   *[4]float64 `json:"bbox"` // west, south, east, north: tiles touching it
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	var tiles []elevation.Tile
	var size int64
	for _, t := range s.elevation().Tiles() {
		if !slices.Contains(req.Levels, t.Level) {
			continue
		}
		if b := req.BBox; b != nil {
			if float64(t.Lat+1) <= b[1] || float64(t.Lat) >= b[3] || float64(t.Lon+1) <= b[0] || float64(t.Lon) >= b[2] {
				continue
			}
		}
		tiles = append(tiles, t)
		size += t.Size
	}
	if len(tiles) == 0 {
		jsonError(w, http.StatusBadRequest, "no tiles of those levels in that area")
		return
	}
	if size > maxElevationPackage {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("that's %d MB of tiles; send a smaller area (up to %d MB)", size>>20, maxElevationPackage>>20))
		return
	}
	targets := s.resolveTargets(sendTarget{All: len(req.Groups) == 0, Groups: cleanGroups(req.Groups)})
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(s.elevation().Package(pw, tiles)) }()
	levels := make([]string, 0, len(req.Levels))
	for _, l := range []int{0, 1, 2} {
		if slices.Contains(req.Levels, l) {
			levels = append(levels, strconv.Itoa(l))
		}
	}
	name := fmt.Sprintf("Elevation DTED %s (%d tiles).zip", strings.Join(levels, "+"), len(tiles))
	p, err := s.savePackage(pr, name, "application/zip", "", []string{"elevation", "DTED"}, "", id)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if groups := cleanGroups(req.Groups); len(groups) > 0 {
		s.Store.UpdatePackage(p.Hash, func(p *store.Package) { p.Groups = groups })
		p.Groups = groups
	}
	delivered, skipped, err := s.offerPackage(p, targets, s.Cfg.ServerName)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	to := "everyone"
	if len(req.Groups) > 0 {
		to = "groups " + strings.Join(req.Groups, ", ")
	}
	s.history.add(sendRecord{Time: time.Now().UTC(), Admin: id.Username, Kind: "package", Summary: p.Name, To: to, Delivered: delivered})
	s.Log.Info("elevation sent to devices", "admin", id.Username, "tiles", len(tiles), "bytes", size, "to", to, "devices", delivered, "skipped", skipped)
	writeJSON(w, http.StatusOK, map[string]any{"tiles": len(tiles), "bytes": p.Size, "delivered": delivered, "package": p.Name})
}
