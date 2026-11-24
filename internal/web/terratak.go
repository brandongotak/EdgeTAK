package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/config"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// TerraTAK (Data → TerraTAK): TAK Product Center's tile server, which makes
// tilesets from imagery, terrain and vector data. It ships as a container
// image from tak.gov, which EdgeTAK doesn't redistribute: the administrator
// loads the image they downloaded. TerraTAK has no login of its own, so it
// listens on loopback only, and EdgeTAK
//   - serves its web app over TLS on its own port (8447, as the app uses
//     absolute paths), to signed-in administrators only;
//   - serves its tiles to devices on the HTTPS port at
//     /terratak/tiles/<tileset>/<z>/<x>/<y>, authenticated like every other
//     device request, and sends chosen tilesets to devices as ATAK map
//     sources in their device profiles.
//
// Running containers needs root, so as with ZeroTier the installer adds a
// root-owned helper (edgetak-terratak.service, started when
// <data>/terratak/request changes). EdgeTAK only writes a request word
// there: install-docker, load (the image uploaded to
// <data>/terratak/image.tar), restart or remove.

var (
	terratakHelper     = "/usr/local/lib/edgetak/edgetak-terratak.sh"
	terratakStatusFile = "/var/lib/edgetak-terratak/status"
	// Container runtimes the helper can use: Docker needs its daemon too
	// (Debian can leave the docker command without it).
	terratakDocker = []string{"/usr/bin/dockerd", "/usr/sbin/dockerd", "/usr/local/bin/dockerd"}
	terratakPodman = []string{"/usr/bin/podman", "/usr/local/bin/podman"}
)

// TerraTAK tileset IDs are UUIDs; z/x/y are tile numbers.
var (
	terratakID   = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	terratakTile = regexp.MustCompile(`^[0-9]{1,8}$`)
)

var terratakClient = &http.Client{Timeout: 30 * time.Second}

func (s *Server) terratakCfg() config.TerraTAK { return s.Cfg.TerraTAK.Normalized() }

func (s *Server) terratakDir() string { return filepath.Join(filepath.Dir(s.PackageDir), "terratak") }

// terratakGet calls TerraTAK's API and decodes its JSON answer into out.
func (s *Server) terratakGet(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(s.terratakCfg().URL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := terratakClient.Do(req)
	if err != nil {
		return errors.New("TerraTAK isn't answering; it may still be starting (it takes about 10 seconds)")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("TerraTAK answered %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

var terratakVersionRe = regexp.MustCompile(`"version"\s*:\s*"([^"]{1,40})"`)

// terratakVersion is the running TerraTAK's version, from the start of its
// OpenAPI document (info.version; the whole document isn't strict JSON).
func (s *Server) terratakVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(s.terratakCfg().URL, "/")+"/openapi", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := terratakClient.Do(req)
	if err != nil {
		return "", errors.New("TerraTAK isn't answering; it may still be starting (it takes about 10 seconds)")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if m := terratakVersionRe.FindSubmatch(b); m != nil {
		return string(m[1]), nil
	}
	return "", nil
}

// terratakRuntime is the container runtime installed here ("" = none).
func terratakRuntime() string {
	for _, rt := range []struct {
		name  string
		paths []string
	}{{"docker", terratakDocker}, {"podman", terratakPodman}} {
		for _, p := range rt.paths {
			if _, err := os.Stat(p); err == nil {
				return rt.name
			}
		}
	}
	return ""
}

// terratakCanManage reports whether the console can install Docker and load
// TerraTAK here, and why not.
func (s *Server) terratakCanManage() (bool, string) {
	if os.Getenv("EDGETAK_CONTAINER") != "" {
		return false, "EdgeTAK runs in a container: run TerraTAK as a container next to it and set EDGETAK_TERRATAK_URL (see docker-compose.yml)."
	}
	if runtime.GOOS != "linux" {
		return false, "Run TerraTAK with Docker yourself (docker run -p 127.0.0.1:8098:8080 terratak:<version>), then turn it on in edgetak.json (\"terratak\": {\"enabled\": true})."
	}
	if _, err := os.Stat(terratakHelper); err != nil {
		return false, "Update EdgeTAK (or rerun the installer) to set up TerraTAK from here."
	}
	return true, ""
}

type terratakTileset struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ContentType string    `json:"contentType"` // imagery, terrain, vector
	MinZoom     int       `json:"minZoom"`
	MaxZoom     int       `json:"maxZoom"`
	Status      string    `json:"status"`
	DateAdded   string    `json:"dateAdded"`
	Footprint   []float64 `json:"footprint"` // lon, lat pairs
}

// bounds is the footprint's [west, south, east, north], or nil.
func (t terratakTileset) bounds() []float64 {
	if len(t.Footprint) < 4 || len(t.Footprint)%2 != 0 {
		return nil
	}
	b := []float64{t.Footprint[0], t.Footprint[1], t.Footprint[0], t.Footprint[1]}
	for i := 0; i < len(t.Footprint); i += 2 {
		b[0], b[1] = min(b[0], t.Footprint[i]), min(b[1], t.Footprint[i+1])
		b[2], b[3] = max(b[2], t.Footprint[i]), max(b[3], t.Footprint[i+1])
	}
	return b
}

// terratakView is the TerraTAK page's state.
func (s *Server) terratakView(ctx context.Context) map[string]any {
	cfg := s.terratakCfg()
	can, why := s.terratakCanManage()
	v := map[string]any{"enabled": cfg.Enabled, "can_manage": can, "manage_note": why, "runtime": terratakRuntime(),
		"reachable": false, "tilesets": []map[string]any{}}
	if _, p, err := net.SplitHostPort(s.Addrs["terratak"]); err == nil {
		v["app_port"] = p
	}
	if st := readStatusFile(terratakStatusFile); st != nil {
		v["status"] = st
	}
	if !cfg.Enabled {
		return v
	}
	version, err := s.terratakVersion(ctx)
	if err != nil {
		v["error"] = err.Error()
		return v
	}
	v["reachable"], v["version"] = true, version
	sent := s.Store.TerraTAKTilesets()
	var list []map[string]any
	for page := 1; page <= 20; page++ {
		var res struct {
			Data []terratakTileset `json:"data"`
		}
		if err := s.terratakGet(ctx, "/tilesets?limit=100&page="+strconv.Itoa(page), &res); err != nil {
			v["error"] = err.Error()
			break
		}
		for _, t := range res.Data {
			_, devices := sent[t.ID]
			list = append(list, map[string]any{"id": t.ID, "name": t.Name, "content_type": t.ContentType,
				"min_zoom": t.MinZoom, "max_zoom": t.MaxZoom, "status": t.Status, "added": t.DateAdded, "devices": devices, "bounds": t.bounds()})
		}
		if len(res.Data) < 100 {
			break
		}
	}
	if list != nil {
		v["tilesets"] = list
	}
	return v
}

func (s *Server) terratakRequest(word string) error {
	if err := os.MkdirAll(s.terratakDir(), 0o750); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.terratakDir(), "request"), []byte(word+"\n"), 0o640)
}

func (s *Server) terratakRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/terratak", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.terratakView(r.Context()))
	}))
	// Requests for the root-owned helper.
	helper := func(word, what string) handlerFunc {
		return func(w http.ResponseWriter, r *http.Request, id *identity) {
			if ok, why := s.terratakCanManage(); !ok {
				jsonError(w, http.StatusBadRequest, why)
				return
			}
			if err := s.terratakRequest(word); err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			s.Log.Info(what, "admin", id.Username)
			writeJSON(w, http.StatusAccepted, s.terratakView(r.Context()))
		}
	}
	mux.HandleFunc("POST /admin/api/terratak/docker", s.admin(helper("install-docker", "Docker install requested for TerraTAK")))
	mux.HandleFunc("POST /admin/api/terratak/restart", s.admin(helper("restart", "TerraTAK restart requested")))
	mux.HandleFunc("POST /admin/api/terratak/remove", s.admin(helper("remove", "TerraTAK removal requested")))
	// The image archive downloaded from tak.gov (docker save format, about
	// 650 MB): saved for the helper, which loads and starts it.
	mux.HandleFunc("POST /admin/api/terratak/image", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if ok, why := s.terratakCanManage(); !ok {
			jsonError(w, http.StatusBadRequest, why)
			return
		}
		if terratakRuntime() == "" {
			jsonError(w, http.StatusBadRequest, "install Docker first")
			return
		}
		if err := os.MkdirAll(s.terratakDir(), 0o750); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		part := filepath.Join(s.terratakDir(), "image.tar.part")
		f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, 8<<30))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && !isDockerArchive(part) {
			err = errors.New("that isn't a container image archive: upload the terratak-<version>-<amd64|arm64>.tar from tak.gov")
		}
		if err != nil {
			os.Remove(part)
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := os.Rename(part, filepath.Join(s.terratakDir(), "image.tar")); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.terratakRequest("load"); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("TerraTAK image uploaded", "admin", id.Username, "bytes", n)
		writeJSON(w, http.StatusAccepted, s.terratakView(r.Context()))
	}))
	// Send a tileset to devices as an ATAK map source, or stop.
	mux.HandleFunc("PUT /admin/api/terratak/tilesets/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		tid := r.PathValue("id")
		var req struct {
			Devices bool `json:"devices"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req)
		if !terratakID.MatchString(tid) {
			jsonError(w, http.StatusBadRequest, "invalid tileset")
			return
		}
		if !req.Devices {
			s.Store.SetTerraTAKTileset(tid, nil)
			writeJSON(w, http.StatusOK, s.terratakView(r.Context()))
			return
		}
		var t terratakTileset
		if err := s.terratakGet(r.Context(), "/tilesets/"+tid, &t); err != nil {
			jsonError(w, http.StatusBadGateway, err.Error())
			return
		}
		if t.ContentType != "" && t.ContentType != "imagery" {
			jsonError(w, http.StatusBadRequest, "only imagery tilesets can be ATAK map sources")
			return
		}
		var ptr struct {
			MimeType string `json:"mimeType"`
		}
		s.terratakGet(r.Context(), "/tilesets/"+tid+".json?format=takjson&overlay=false", &ptr)
		format := "png"
		if strings.Contains(ptr.MimeType, "jpeg") || strings.Contains(ptr.MimeType, "jpg") {
			format = "jpg"
		}
		if err := s.Store.SetTerraTAKTileset(tid, &store.TerraTAKTileset{Name: t.Name, MinZoom: t.MinZoom, MaxZoom: t.MaxZoom, Format: format}); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("TerraTAK tileset sent to devices", "admin", id.Username, "tileset", t.Name)
		writeJSON(w, http.StatusOK, s.terratakView(r.Context()))
	}))
	// Tiles for devices and the console map.
	mux.HandleFunc("GET /terratak/tiles/{id}/{z}/{x}/{y}", s.authed(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		tid, z, x, y := r.PathValue("id"), r.PathValue("z"), r.PathValue("x"), r.PathValue("y")
		y, _, _ = strings.Cut(y, ".") // {y}.png
		if !s.terratakCfg().Enabled || !terratakID.MatchString(tid) || !terratakTile.MatchString(z) || !terratakTile.MatchString(x) || !terratakTile.MatchString(y) {
			http.NotFound(w, r)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), "GET", strings.TrimRight(s.terratakCfg().URL, "/")+"/tilesets/"+tid+"/tiles/"+z+"/"+x+"/"+y, nil)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		resp, err := terratakClient.Do(req)
		if err != nil {
			http.Error(w, "TerraTAK isn't answering", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for _, h := range []string{"Content-Type", "Content-Length", "Cache-Control", "ETag", "Last-Modified"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
}

// isDockerArchive reports whether path looks like a `docker save` archive
// (a tar with a manifest.json).
func isDockerArchive(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	// The first tar header: a 512-byte block with "ustar" at offset 257.
	hdr := make([]byte, 512)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return false
	}
	return bytes.HasPrefix(hdr[257:], []byte("ustar"))
}

// terratakEntries are the ATAK map sources for the TerraTAK tilesets sent to
// devices, for device profiles; since limits them to ones changed after it.
func (s *Server) terratakEntries(host string, since time.Time) []profileEntry {
	if !s.terratakCfg().Enabled {
		return nil
	}
	_, port, err := net.SplitHostPort(s.Addrs["https"])
	if err != nil || host == "" {
		return nil
	}
	var out []profileEntry
	for id, t := range s.Store.TerraTAKTilesets() {
		if !since.IsZero() && !t.Updated.After(since) {
			continue
		}
		u := "https://" + net.JoinHostPort(host, port) + "/terratak/tiles/" + url.PathEscape(id) + "/{$z}/{$x}/{$y}"
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
`, xmlEscape(t.Name), t.MinZoom, t.MaxZoom, t.Format, xmlEscape(u))
		out = append(out, profileEntry{name: "TerraTAK-" + sanitize(id) + ".xml", data: []byte(xml)})
	}
	return out
}

// ---- the TerraTAK web app ----

// TerraTAKHandler serves TerraTAK's web app and API on its own TLS port to
// signed-in administrators (console session or API key).
func (s *Server) TerraTAKHandler() http.Handler {
	target, err := url.Parse(s.terratakCfg().URL)
	if err != nil {
		target = &url.URL{Scheme: "http", Host: "127.0.0.1:8098"}
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// TerraTAK writes this host into tile URLs and pointers.
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del(csrfHeader)
			pr.SetXForwarded()
			if terratakRewritten(pr.In.URL.Path) {
				pr.Out.Header.Del("Accept-Encoding") // so the answer can be rewritten
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			host := resp.Request.Host
			// TerraTAK only knows plain HTTP; clients reach it over TLS.
			if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "http://"+host) {
				resp.Header.Set("Location", "https://"+strings.TrimPrefix(loc, "http://"))
			}
			if !terratakRewritten(resp.Request.URL.Path) || resp.Header.Get("Content-Encoding") != "" || resp.ContentLength > 8<<20 {
				return nil
			}
			b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			if err != nil {
				return err
			}
			b = bytes.ReplaceAll(b, []byte("http://"+host), []byte("https://"+host))
			resp.Body = io.NopCloser(bytes.NewReader(b))
			resp.ContentLength = int64(len(b))
			resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.Log.Warn("TerraTAK isn't answering", "url", target.String(), "err", err)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, terratakPage("TerraTAK isn't answering", "It may still be starting (about 10 seconds), or not be installed yet. See Data → TerraTAK in the EdgeTAK console."))
		},
		FlushInterval: 100 * time.Millisecond,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() { s.logRequest(r, rec, time.Since(start)) }()
		rec.Header().Set("X-Content-Type-Options", "nosniff")
		rec.Header().Set("X-Frame-Options", "DENY")
		rec.Header().Set("Referrer-Policy", "same-origin")
		if s.AddressRefused(r.RemoteAddr) {
			http.Error(rec, "refused", http.StatusForbidden)
			return
		}
		id, status, msg := s.terratakAuth(r)
		if id == nil {
			if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
				rec.Header().Set("Content-Type", "text/html; charset=utf-8")
				rec.WriteHeader(status)
				fmt.Fprint(rec, terratakPage("Sign in to EdgeTAK first", msg+` <a href="`+html.EscapeString(s.consoleURL(r))+`">Open the EdgeTAK console</a>, sign in as an administrator, then reload this page.`))
				return
			}
			jsonError(rec, status, msg)
			return
		}
		if !safeMethod(r.Method) {
			s.Log.Info("TerraTAK change", "admin", id.Username, "method", r.Method, "path", r.URL.Path)
		}
		rp.ServeHTTP(rec, r)
	})
}

// terratakRewritten: answers that carry TerraTAK's own address (pointers,
// TileJSON, styles), which EdgeTAK rewrites from http to https.
func terratakRewritten(path string) bool {
	return strings.HasSuffix(path, ".json") || strings.HasSuffix(path, "/pointer")
}

// terratakAuth returns the administrator a request to the TerraTAK app comes
// from, or the status and reason to refuse it.
func (s *Server) terratakAuth(r *http.Request) (*identity, int, string) {
	if !safeMethod(r.Method) {
		// TerraTAK's app can't send the console's anti-forgery header; a
		// same-origin check stands in for it. Browsers send Origin on every
		// cross-origin and every state-changing request.
		if o := r.Header.Get("Origin"); o != "" && o != "https://"+r.Host {
			return nil, http.StatusForbidden, "cross-origin request refused."
		}
		r = r.Clone(r.Context())
		r.Header.Set(csrfHeader, "1")
	}
	id := s.identify(r)
	switch {
	case id == nil || id.Anonymous:
		return nil, http.StatusUnauthorized, "TerraTAK is for EdgeTAK administrators."
	case !id.Admin:
		return nil, http.StatusForbidden, "TerraTAK is for EdgeTAK administrators."
	case id.ReadOnly && !safeMethod(r.Method):
		return nil, http.StatusForbidden, readOnlyKey
	}
	if !id.Session && id.APIKey == "" {
		if u, ok := s.Store.User(id.Username); ok && u.TwoFactor() {
			return nil, http.StatusUnauthorized, "This account uses two-factor login; sign in to the console."
		}
	}
	return id, 0, ""
}

// consoleURL is the admin console on the host the request came to.
func (s *Server) consoleURL(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	name := "https"
	if s.Public != nil && s.Public.Uses("enroll") {
		name = "enroll" // the port with the trusted certificate
	}
	_, port, err := net.SplitHostPort(s.Addrs[name])
	if err != nil {
		port = "8443"
	}
	return "https://" + net.JoinHostPort(host, port) + "/admin/#terratak"
}

func terratakPage(title, body string) string {
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + html.EscapeString(title) + `</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:560px;margin:12vh auto;padding:0 16px;color:#1f2328;background:#f6f7f9}h1{font-size:20px}a{color:#1f7a4d}@media(prefers-color-scheme:dark){body{color:#e6e6e6;background:#15171a}a{color:#5fc08f}}</style></head>
<body><h1>` + html.EscapeString(title) + `</h1><p>` + body + `</p></body></html>`
}
