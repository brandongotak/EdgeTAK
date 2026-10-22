package web

import (
	"archive/zip"
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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Device profiles, as on the official server: settings and files that reach
// the TAK clients of users in a profile's groups. ATAK downloads them as a
// mission package (auto-imported: .pref files change settings, APKs offer a
// plugin install, map source XML adds map sources) when it enrolls
// (/Marti/api/tls/profile/enrollment), when it connects
// (/Marti/api/device/profile/connection, only profiles changed since its
// last sync, from syncSecago), or when an admin sends one now.

const maxProfileFile = 512 << 20

var (
	prefClasses = map[string]string{"string": "java.lang.String", "boolean": "java.lang.Boolean", "integer": "java.lang.Integer",
		"long": "java.lang.Long", "float": "java.lang.Float"}
	prefKeyRE       = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,128}$`)
	profileFileRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()+-]{0,127}$`)
	errProfileInput = errors.New("invalid profile")
)

func (s *Server) profilesDir() string { return filepath.Join(filepath.Dir(s.PackageDir), "profiles") }

func (s *Server) profileFilePath(id, name string) (string, error) {
	if !profileFileRE.MatchString(name) || strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) {
		return "", errProfileInput
	}
	return filepath.Join(s.profilesDir(), id, name), nil
}

// profileApplies reports whether p goes to id's devices: active, and id is
// in one of its groups (receive side), or it has no groups (everyone).
func (s *Server) profileApplies(p store.DeviceProfile, id *identity) bool {
	if !p.Active || id == nil || id.Anonymous {
		return false
	}
	if len(p.Groups) == 0 {
		return true
	}
	return slices.ContainsFunc(p.Groups, func(g string) bool { return slices.Contains(id.Groups, g) })
}

// prefFileName is the name of the .pref file holding a profile's settings.
func prefFileName(p store.DeviceProfile) string {
	return strings.ReplaceAll(sanitize(p.Name), " ", "_") + ".pref"
}

func prefsXML(entries string) []byte {
	return []byte(`<?xml version='1.0' standalone='yes'?><preferences>` +
		`<preference version="1" name="com.atakmap.app.civ_preferences">` + entries + `</preference></preferences>`)
}

func profilePrefs(p store.DeviceProfile) []byte {
	var b strings.Builder
	for _, pr := range p.Prefs {
		b.WriteString(`<entry key="` + xmlEscape(pr.Key) + `" class="class ` + prefClasses[pr.Type] + `">` + xmlEscape(pr.Value) + `</entry>`)
	}
	return prefsXML(b.String())
}

// profileEntry is one file of a profile package: in memory, or on disk.
type profileEntry struct {
	name string
	data []byte
	path string
}

func (s *Server) profileEntries(p store.DeviceProfile) []profileEntry {
	var out []profileEntry
	if len(p.Prefs) > 0 {
		out = append(out, profileEntry{name: prefFileName(p), data: profilePrefs(p)})
	}
	for _, f := range p.Files {
		if path, err := s.profileFilePath(p.ID, f.Name); err == nil {
			out = append(out, profileEntry{name: f.Name, path: path})
		}
	}
	return out
}

// writeProfilePackage streams a mission package of entries, laid out as
// the official server does (one fileN/ folder per file).
func writeProfilePackage(w io.Writer, uid, name string, entries []profileEntry) error {
	var manifest strings.Builder
	manifest.WriteString(`<MissionPackageManifest version="2"><Configuration>` +
		`<Parameter name="uid" value="` + xmlEscape(uid) + `"/><Parameter name="name" value="` + xmlEscape(name) + `"/>` +
		`<Parameter name="onReceiveImport" value="true"/><Parameter name="onReceiveDelete" value="true"/></Configuration><Contents>`)
	for i, e := range entries {
		manifest.WriteString(`<Content ignore="false" zipEntry="file` + strconv.Itoa(i) + `/` + xmlEscape(e.name) + `"/>`)
	}
	manifest.WriteString(`</Contents></MissionPackageManifest>`)
	zw := zip.NewWriter(w)
	fw, err := zw.CreateHeader(&zip.FileHeader{Name: "MANIFEST/manifest.xml", Method: zip.Deflate, Modified: time.Now()})
	if err != nil {
		return err
	}
	io.WriteString(fw, manifest.String())
	for i, e := range entries {
		method := zip.Deflate
		if strings.HasSuffix(strings.ToLower(e.name), ".apk") || strings.HasSuffix(strings.ToLower(e.name), ".zip") {
			method = zip.Store // already compressed
		}
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: "file" + strconv.Itoa(i) + "/" + e.name, Method: method, Modified: time.Now()})
		if err != nil {
			return err
		}
		if e.path == "" {
			fw.Write(e.data)
			continue
		}
		f, err := os.Open(e.path)
		if err != nil {
			return err
		}
		_, err = io.Copy(fw, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	return zw.Close()
}

// deviceProfile serves ATAK's enrollment and connection profile requests:
// the Channels settings, plus the profiles meant for this user.
func (s *Server) deviceProfile(kind string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, id *identity) {
		if id.Anonymous {
			w.WriteHeader(http.StatusNoContent) // nothing to choose without an account
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		entries := []profileEntry{{name: "enable-channels.pref", data: prefsXML(channelPrefEntries(host))}}
		if prefs := s.updateServerPrefs(host); prefs != "" {
			entries = append(entries, profileEntry{name: "update-server.pref", data: prefsXML(prefs)})
		}
		// The connection profile only carries what changed since the device
		// last synced (syncSecago seconds ago; -1 or absent = everything).
		var since time.Time
		if kind == "Connection" {
			if secs, err := strconv.ParseInt(r.URL.Query().Get("syncSecago"), 10, 64); err == nil && secs >= 0 {
				since = time.Now().Add(-time.Duration(secs) * time.Second)
			}
		}
		entries = append(entries, s.mapSourceEntries(host, since)...)
		entries = append(entries, s.customMapSourceEntries(append(slices.Clone(id.Groups), id.OutGroups...), since)...)
		var sent []string
		for _, p := range s.Store.DeviceProfiles() {
			wanted := p.OnConnect
			if kind == "Enrollment" {
				wanted = p.OnEnrollment
			}
			if !wanted || !s.profileApplies(p, id) || (!since.IsZero() && !p.Updated.After(since)) {
				continue
			}
			entries = append(entries, s.profileEntries(p)...)
			sent = append(sent, p.Name)
		}
		if len(sent) > 0 {
			s.Log.Info("device profiles sent", "kind", strings.ToLower(kind), "user", id.Username, "client", r.URL.Query().Get("clientUid"), "profiles", sent)
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=profile.zip")
		if err := writeProfilePackage(w, "ProfileMissionPackage-"+uuid(), kind, entries); err != nil {
			s.Log.Warn("device profile", "err", err)
		}
	}
}

// ---- admin ----

type profileView struct {
	store.DeviceProfile
	Users     int `json:"users"`     // accounts it applies to
	Connected int `json:"connected"` // their devices connected now
}

func (s *Server) profileTargets(p store.DeviceProfile) (users int, clients []*router.Client) {
	ids := map[string]*identity{}
	for _, u := range s.Store.Users() {
		id := s.fromUser(u)
		if s.profileApplies(p, id) {
			users++
			ids[u.Username] = id
		}
	}
	for _, c := range s.Router.Clients() {
		if ids[c.Username] != nil {
			clients = append(clients, c)
		}
	}
	return users, clients
}

func (s *Server) profileViews() []profileView {
	out := []profileView{}
	for _, p := range s.Store.DeviceProfiles() {
		users, clients := s.profileTargets(p)
		out = append(out, profileView{DeviceProfile: p, Users: users, Connected: len(clients)})
	}
	return out
}

func validPrefs(prefs []store.ProfilePref) ([]store.ProfilePref, error) {
	out := []store.ProfilePref{}
	for _, p := range prefs {
		p.Key, p.Value = strings.TrimSpace(p.Key), strings.TrimSpace(p.Value)
		if p.Key == "" {
			continue
		}
		if p.Type == "" {
			p.Type = "string"
		}
		if !prefKeyRE.MatchString(p.Key) {
			return nil, fmt.Errorf("setting names are letters, digits and . _ : @ - (%q)", p.Key)
		}
		if _, ok := prefClasses[p.Type]; !ok {
			return nil, fmt.Errorf("unknown setting type %q", p.Type)
		}
		switch p.Type {
		case "boolean":
			b, err := strconv.ParseBool(p.Value)
			if err != nil {
				return nil, fmt.Errorf("%s must be true or false", p.Key)
			}
			p.Value = strconv.FormatBool(b)
		case "integer", "long":
			if _, err := strconv.ParseInt(p.Value, 10, 64); err != nil {
				return nil, fmt.Errorf("%s must be a whole number", p.Key)
			}
		case "float":
			if _, err := strconv.ParseFloat(p.Value, 64); err != nil {
				return nil, fmt.Errorf("%s must be a number", p.Key)
			}
		}
		if len(p.Value) > 4096 {
			return nil, fmt.Errorf("%s is too long", p.Key)
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Server) profileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/profiles", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.profileViews())
	}))
	mux.HandleFunc("POST /admin/api/profiles", s.admin(s.saveProfile))
	mux.HandleFunc("PUT /admin/api/profiles/{id}", s.admin(s.saveProfile))
	mux.HandleFunc("DELETE /admin/api/profiles/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		pid := r.PathValue("id")
		p, ok := s.Store.DeviceProfile(pid)
		if !ok || s.Store.DeleteDeviceProfile(pid) != nil {
			jsonError(w, http.StatusNotFound, "profile not found")
			return
		}
		if dir, err := s.profileFilePath(pid, "x"); err == nil {
			os.RemoveAll(filepath.Dir(dir))
		}
		s.Log.Info("device profile deleted", "admin", id.Username, "profile", p.Name)
		w.WriteHeader(http.StatusNoContent)
	}))
	// multipart: file
	mux.HandleFunc("POST /admin/api/profiles/{id}/files", s.admin(s.addProfileFile))
	mux.HandleFunc("DELETE /admin/api/profiles/{id}/files/{name}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		pid, name := r.PathValue("id"), r.PathValue("name")
		p, err := s.Store.UpdateDeviceProfile(pid, func(p *store.DeviceProfile) error {
			i := slices.IndexFunc(p.Files, func(f store.ProfileFile) bool { return f.Name == name })
			if i < 0 {
				return store.ErrNotFound
			}
			p.Files = slices.Delete(p.Files, i, i+1)
			return nil
		})
		if err != nil {
			jsonError(w, http.StatusNotFound, "file not found")
			return
		}
		if path, err := s.profileFilePath(pid, name); err == nil {
			os.Remove(path)
		}
		s.Log.Info("device profile file removed", "admin", id.Username, "profile", p.Name, "file", name)
		writeJSON(w, http.StatusOK, p)
	}))
	mux.HandleFunc("GET /admin/api/profiles/{id}/files/{name}", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		pid, name := r.PathValue("id"), r.PathValue("name")
		p, ok := s.Store.DeviceProfile(pid)
		path, err := s.profileFilePath(pid, name)
		if !ok || err != nil || !slices.ContainsFunc(p.Files, func(f store.ProfileFile) bool { return f.Name == name }) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		http.ServeFile(w, r, path)
	}))
	// The package devices receive, to check it.
	mux.HandleFunc("GET /admin/api/profiles/{id}/package", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		p, ok := s.Store.DeviceProfile(r.PathValue("id"))
		if !ok {
			http.Error(w, "profile not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+sanitize(p.Name)+`-profile.zip"`)
		writeProfilePackage(w, "ProfileMissionPackage-"+p.ID, p.Name, s.profileEntries(p))
	}))
	mux.HandleFunc("POST /admin/api/profiles/{id}/send", s.admin(s.sendProfile))
}

func (s *Server) saveProfile(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct {
		Name         string              `json:"name"`
		Groups       []string            `json:"groups"`
		Active       bool                `json:"active"`
		OnEnrollment bool                `json:"on_enrollment"`
		OnConnect    bool                `json:"on_connect"`
		Prefs        []store.ProfilePref `json:"prefs"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Name = truncate(strings.TrimSpace(req.Name), 100)
	if req.Name == "" {
		jsonError(w, http.StatusBadRequest, "a name is required")
		return
	}
	prefs, err := validPrefs(req.Prefs)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	set := func(p *store.DeviceProfile) error {
		p.Name, p.Groups, p.Active, p.OnEnrollment, p.OnConnect, p.Prefs = req.Name, cleanGroups(req.Groups), req.Active, req.OnEnrollment, req.OnConnect, prefs
		return nil
	}
	var p store.DeviceProfile
	if pid := r.PathValue("id"); pid != "" {
		if p, err = s.Store.UpdateDeviceProfile(pid, set); err != nil {
			jsonError(w, http.StatusNotFound, "profile not found")
			return
		}
	} else {
		now := time.Now().UTC()
		p = store.DeviceProfile{ID: uuid(), Created: now, CreatedBy: id.Username, Updated: now, Files: []store.ProfileFile{}}
		set(&p)
		if err := s.Store.PutDeviceProfile(p); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.Log.Info("device profile saved", "admin", id.Username, "profile", p.Name, "groups", p.Groups, "active", p.Active, "settings", len(p.Prefs))
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) addProfileFile(w http.ResponseWriter, r *http.Request, id *identity) {
	pid := r.PathValue("id")
	if _, ok := s.Store.DeviceProfile(pid); !ok {
		jsonError(w, http.StatusNotFound, "profile not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProfileFile+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		jsonError(w, http.StatusBadRequest, "send the file as multipart form data")
		return
	}
	var part io.ReadCloser
	name := ""
	for {
		pt, err := mr.NextPart()
		if err != nil {
			break
		}
		if pt.FormName() == "file" {
			part, name = pt, filepath.Base(strings.ReplaceAll(pt.FileName(), `\`, "/"))
			break
		}
		pt.Close()
	}
	if part == nil {
		jsonError(w, http.StatusBadRequest, "no file")
		return
	}
	defer part.Close()
	final, err := s.profileFilePath(pid, name)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "file names are letters, digits, spaces and . _ ( ) + - (up to 128 characters)")
		return
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(final), ".upload-*")
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(part, maxProfileFile+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		jsonError(w, http.StatusBadRequest, "upload failed: "+err.Error())
		return
	}
	if n > maxProfileFile {
		jsonError(w, http.StatusRequestEntityTooLarge, "files can be up to 512 MB")
		return
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	f := store.ProfileFile{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), Added: time.Now().UTC()}
	p, err := s.Store.UpdateDeviceProfile(pid, func(p *store.DeviceProfile) error {
		p.Files = slices.DeleteFunc(p.Files, func(x store.ProfileFile) bool { return x.Name == name }) // a new version replaces the old
		p.Files = append(p.Files, f)
		return nil
	})
	if err != nil {
		jsonError(w, http.StatusNotFound, "profile not found")
		return
	}
	s.Log.Info("device profile file added", "admin", id.Username, "profile", p.Name, "file", name, "bytes", n)
	writeJSON(w, http.StatusOK, p)
}

// sendProfile offers a profile to its users' connected devices now, as a
// data package, instead of waiting for them to reconnect.
func (s *Server) sendProfile(w http.ResponseWriter, r *http.Request, id *identity) {
	p, ok := s.Store.DeviceProfile(r.PathValue("id"))
	if !ok {
		jsonError(w, http.StatusNotFound, "profile not found")
		return
	}
	if !p.Active {
		jsonError(w, http.StatusBadRequest, "turn the profile on first")
		return
	}
	_, targets := s.profileTargets(p)
	if len(targets) == 0 {
		jsonError(w, http.StatusBadRequest, "none of this profile's users have a device connected")
		return
	}
	entries := s.profileEntries(p)
	if len(entries) == 0 {
		jsonError(w, http.StatusBadRequest, "the profile has no settings or files yet")
		return
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeProfilePackage(pw, "ProfileMissionPackage-"+p.ID, p.Name, entries)) }()
	// The package is only shared with the recipients' groups (offerPackage).
	owner := &identity{Username: id.Username, Groups: p.Groups, OutGroups: p.Groups}
	pkg, err := s.savePackage(pr, "Profile-"+sanitize(p.Name)+".zip", "application/x-zip-compressed", "", []string{"device profile"}, "profile", owner)
	pr.Close()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	delivered, skipped, err := s.offerPackage(pkg, targets, s.Cfg.ServerName)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.history.add(sendRecord{Time: time.Now().UTC(), Admin: id.Username, Kind: "package", Summary: "Device profile " + p.Name,
		To: fmt.Sprintf("%d connected devices", len(targets)), Delivered: delivered})
	s.Log.Info("device profile sent now", "admin", id.Username, "profile", p.Name, "devices", delivered, "skipped", skipped)
	writeJSON(w, http.StatusOK, map[string]int{"delivered": delivered, "skipped": skipped})
}
