package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/apps"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// The update server: ATAK plugins and apps that devices install and update
// from ATAK (Plugins > Update Server). Admins upload APKs; devices read
// /update/product.infz over the HTTPS API with their certificate (or a
// username and password, which ATAK asks for), and device profiles point
// ATAK at it.

const maxAPK = 512 << 20

func (s *Server) appsDir() string { return filepath.Join(filepath.Dir(s.PackageDir), "apps") }

func (s *Server) apkFile(a store.App) string  { return filepath.Join(s.appsDir(), a.SHA256+".apk") }
func (s *Server) iconFile(a store.App) string { return filepath.Join(s.appsDir(), a.SHA256+".png") }

// updateServerURL is where devices find the update server: the HTTPS API on
// the host they reached EdgeTAK by.
func (s *Server) updateServerURL(host string) string {
	_, port, err := net.SplitHostPort(s.Addrs["https"])
	if err != nil || host == "" {
		return ""
	}
	return "https://" + net.JoinHostPort(host, port) + "/update"
}

// updateServerPrefs are the ATAK settings that turn the update server on,
// for device profiles; empty when there is nothing to offer.
func (s *Server) updateServerPrefs(host string) string {
	url := s.updateServerURL(host)
	if url == "" || s.Store.UpdateServer().DontConfigureDevices || len(s.Store.Apps()) == 0 {
		return ""
	}
	return `<entry key="appMgmtEnableUpdateServer" class="class java.lang.Boolean">true</entry>` +
		`<entry key="atakUpdateServerUrl" class="class java.lang.String">` + xmlEscape(url) + `</entry>`
}

func (s *Server) appRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /update/{path...}", s.authed(s.updateServer))

	mux.HandleFunc("GET /admin/api/apps", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.appsView(r))
	}))
	mux.HandleFunc("POST /admin/api/apps", s.admin(s.uploadAPKs))
	mux.HandleFunc("DELETE /admin/api/apps/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		a, err := s.Store.DeleteApp(r.PathValue("id"))
		if err != nil {
			jsonError(w, http.StatusNotFound, "not found")
			return
		}
		os.Remove(s.apkFile(a))
		os.Remove(s.iconFile(a))
		s.Log.Info("ATAK app removed", "admin", id.Username, "package", a.Package, "version", a.Version)
		writeJSON(w, http.StatusOK, s.appsView(r))
	}))
	mux.HandleFunc("GET /admin/api/apps/{id}/icon", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		a, ok := s.app(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Write(s.appIcon(a))
	}))
	mux.HandleFunc("GET /admin/api/apps/{id}/apk", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		a, ok := s.app(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.serveAPK(w, r, a, firstNonEmpty(a.FileName, a.Package+".apk"))
	}))
	mux.HandleFunc("PUT /admin/api/apps/settings", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			ConfigureDevices bool `json:"configure_devices"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if err := s.Store.SetUpdateServer(store.UpdateServerSettings{DontConfigureDevices: !req.ConfigureDevices}); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("update server settings changed", "admin", id.Username, "configure_devices", req.ConfigureDevices)
		writeJSON(w, http.StatusOK, s.appsView(r))
	}))
}

func (s *Server) app(id string) (store.App, bool) {
	for _, a := range s.Store.Apps() {
		if a.SHA256 == id {
			return a, true
		}
	}
	return store.App{}, false
}

func (s *Server) appIcon(a store.App) []byte {
	if a.HasIcon {
		if b, err := os.ReadFile(s.iconFile(a)); err == nil {
			return b
		}
	}
	return apps.Placeholder()
}

type appView struct {
	store.App
	ATAKVersion string `json:"atak_version,omitempty"`
}

func (s *Server) appsView(r *http.Request) map[string]any {
	list := []appView{}
	for _, a := range s.Store.Apps() {
		list = append(list, appView{App: a, ATAKVersion: apps.ATAKVersion(a.PluginAPI)})
	}
	host := s.Cfg.PublicHost
	if host == "" {
		host, _, _ = net.SplitHostPort(r.Host)
	}
	return map[string]any{"apps": list, "url": s.updateServerURL(host), "configure_devices": !s.Store.UpdateServer().DontConfigureDevices}
}

// updateServer answers ATAK: the index (general, or for one ATAK version)
// and the APKs it lists.
func (s *Server) updateServer(w http.ResponseWriter, r *http.Request, id *identity) {
	path := strings.Trim(r.PathValue("path"), "/")
	all := s.Store.Apps()
	version := ""
	if first, rest, ok := strings.Cut(path, "/"); ok && first != "apk" {
		// ATAK first asks for <url>/<its version>/product.infz; without
		// plugins for that version, a 404 makes it use the general index.
		if !apps.HasVersion(all, first) {
			http.NotFound(w, r)
			return
		}
		version, path = first, rest
	}
	list := apps.Select(all, version)
	switch {
	case path == "product.infz":
		data, err := apps.ProductInfz(list, s.appIcon)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Write(data)
	case path == "product.inf":
		data := apps.ProductInf(list)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Write(data)
	case strings.HasPrefix(path, "apk/") && strings.HasSuffix(path, ".apk"):
		a, ok := s.app(strings.TrimSuffix(strings.TrimPrefix(path, "apk/"), ".apk"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet {
			s.Log.Info("ATAK app downloaded", "user", id.Username, "package", a.Package, "version", a.Version)
		}
		s.serveAPK(w, r, a, a.Package+"-"+firstNonEmpty(a.Version, "apk")+".apk")
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveAPK(w http.ResponseWriter, r *http.Request, a store.App, name string) {
	f, err := os.Open(s.apkFile(a))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitize(name)+`"`)
	http.ServeContent(w, r, "", a.Uploaded, f)
}

// uploadAPKs adds the APKs in a multipart upload (field "apk", one or more).
func (s *Server) uploadAPKs(w http.ResponseWriter, r *http.Request, id *identity) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*maxAPK)
	mr, err := r.MultipartReader()
	if err != nil {
		jsonError(w, http.StatusBadRequest, "send the APKs as multipart form data")
		return
	}
	if err := os.MkdirAll(s.appsDir(), 0o750); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var added []string
	var problems []string
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() != "apk" {
			part.Close()
			continue
		}
		name := filepath.Base(strings.ReplaceAll(part.FileName(), `\`, "/"))
		a, err := s.addAPK(part, name, id)
		part.Close()
		if err != nil {
			problems = append(problems, name+": "+err.Error())
			continue
		}
		added = append(added, a.Name+" "+a.Version)
	}
	if len(added) == 0 && len(problems) == 0 {
		jsonError(w, http.StatusBadRequest, "no APK in the upload")
		return
	}
	view := s.appsView(r)
	view["added"], view["problems"] = added, problems
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) addAPK(src io.Reader, name string, id *identity) (*store.App, error) {
	tmp, err := os.CreateTemp(s.appsDir(), ".upload-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(src, maxAPK+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	if n > maxAPK {
		return nil, errors.New("APKs can be up to 512 MB")
	}
	info, err := apps.ReadAPK(tmp.Name())
	if err != nil {
		return nil, errors.New("not an Android app (APK) EdgeTAK can read")
	}
	a := store.App{SHA256: hex.EncodeToString(h.Sum(nil)), Package: info.Package, Name: info.Name, Version: info.Version,
		VersionCode: info.VersionCode, MinSDK: info.MinSDK, PluginAPI: info.PluginAPI, Description: info.Description,
		Size: n, HasIcon: len(info.Icon) > 0, FileName: name, Uploader: id.Username, Uploaded: time.Now().UTC()}
	if err := os.Rename(tmp.Name(), s.apkFile(a)); err != nil {
		return nil, err
	}
	if a.HasIcon {
		if err := os.WriteFile(s.iconFile(a), info.Icon, 0o640); err != nil {
			a.HasIcon = false
		}
	}
	replaced, err := s.Store.PutApp(a)
	if err != nil {
		return nil, err
	}
	for _, old := range replaced {
		os.Remove(s.apkFile(old))
		os.Remove(s.iconFile(old))
	}
	s.Log.Info("ATAK app added", "admin", id.Username, "package", a.Package, "name", a.Name, "version", a.Version,
		"plugin_api", a.PluginAPI, "replaced", len(replaced))
	return &a, nil
}
