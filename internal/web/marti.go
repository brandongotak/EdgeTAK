package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edgetak/edgetak/internal/cot"
	"github.com/edgetak/edgetak/internal/mission"
	"github.com/edgetak/edgetak/internal/pki"
	"github.com/edgetak/edgetak/internal/store"
)

func (s *Server) martiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /Marti/api/version", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "EdgeTAK %s", s.Version)
	})
	mux.HandleFunc("GET /Marti/api/version/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.martiEnvelope("ServerConfig", map[string]string{
			"version": "EdgeTAK " + s.Version, "api": "3", "hostname": s.Cfg.PublicHost,
		}))
	})

	// Certificate enrollment (ATAK/WinTAK/iTAK "enroll for client certificate").
	mux.HandleFunc("GET /Marti/api/tls/config", s.named(s.tlsConfig))
	mux.HandleFunc("POST /Marti/api/tls/signClient/v2", s.named(s.signClient))
	noContent := func(w http.ResponseWriter, r *http.Request, _ *identity) { w.WriteHeader(http.StatusNoContent) }
	mux.HandleFunc("GET /Marti/api/tls/profile/enrollment", s.named(noContent))
	mux.HandleFunc("GET /Marti/api/device/profile/connection", s.authed(noContent))

	mux.HandleFunc("GET /Marti/api/clientEndPoints", s.authed(s.clientEndpoints))
	mux.HandleFunc("GET /Marti/api/groups/all", s.authed(s.groupsAll))
	for _, p := range []string{"/Marti/api/contacts/all", "/Marti/api/contacts/all/lite", "/Marti/api/contacts/all/full"} {
		mux.HandleFunc("GET "+p, s.authed(s.contacts))
	}

	// Data packages.
	mux.HandleFunc("POST /Marti/sync/missionupload", s.authed(s.missionUpload))
	mux.HandleFunc("GET /Marti/sync/missionquery", s.authed(s.missionQuery))
	mux.HandleFunc("GET /Marti/sync/content", s.authed(s.content))
	mux.HandleFunc("HEAD /Marti/sync/content", s.authed(s.content))
	mux.HandleFunc("GET /Marti/sync/search", s.authed(s.search))
	mux.HandleFunc("POST /Marti/sync/upload", s.authed(s.rawUpload))
	mux.HandleFunc("PUT /Marti/api/sync/metadata/{hash}/tool", s.authed(s.setTool))
	mux.HandleFunc("DELETE /Marti/api/files/{hash}", s.authed(s.deleteFile))

	// Newer (Spring API) forms used by TAKX and recent clients.
	mux.HandleFunc("GET /Marti/api/sync/search", s.authed(s.apiSearch))
	mux.HandleFunc("GET /Marti/api/files/{hash}", s.authed(s.fileByHash))
	mux.HandleFunc("HEAD /Marti/api/files/{hash}", s.authed(s.fileByHash))
	mux.HandleFunc("PUT /Marti/api/sync/metadata/{hash}/keywords", s.authed(s.setKeywords))
	mux.HandleFunc("PUT /Marti/api/sync/metadata/{hash}/expiration", s.authed(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if _, ok := s.visiblePackage(r.PathValue("hash"), id); !ok {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK) // packages do not expire on EdgeTAK
	}))

	s.missionRoutes(mux)
}

// ---- enrollment ----

func (s *Server) tlsConfig(w http.ResponseWriter, r *http.Request, _ *identity) {
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`+
		`<ns2:certificateConfig xmlns="http://bbn.com/marti/xml/config" xmlns:ns2="com.bbn.marti.config">`+
		`<nameEntries><nameEntry name="O" value="%s"/><nameEntry name="OU" value="%s"/></nameEntries>`+
		`<validityDays>%d</validityDays></ns2:certificateConfig>`,
		xmlEscape(s.Cfg.CertOrg), xmlEscape(s.Cfg.CertOrgUnit), s.Cfg.CertValidityDays)
}

func (s *Server) signClient(w http.ResponseWriter, r *http.Request, id *identity) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	csr, err := pki.ParseCSR(body)
	if err != nil {
		http.Error(w, "invalid CSR", http.StatusBadRequest)
		return
	}
	cert, err := s.CA.SignCSR(csr, id.Username)
	if err != nil {
		s.Log.Warn("enrollment failed", "user", id.Username, "err", err)
		http.Error(w, "could not sign CSR", http.StatusBadRequest)
		return
	}
	clientUID := r.URL.Query().Get("clientUid")
	if err := s.Store.AddCert(store.Cert{
		Serial: pki.SerialHex(cert), CommonName: id.Username, ClientUID: clientUID,
		Issued: time.Now().UTC(), Expires: cert.NotAfter.UTC(),
	}); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	s.Log.Info("issued client certificate", "user", id.Username, "clientUid", clientUID, "serial", pki.SerialHex(cert))

	signed, ca := pki.PEMBody(cert), pki.PEMBody(s.CA.Cert)
	accept := strings.ToLower(r.Header.Get("Accept"))
	if strings.Contains(accept, "application/xml") && !strings.Contains(accept, "application/json") {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><enrollment><signedCert>%s</signedCert><ca>%s</ca></enrollment>`, signed, ca)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"signedCert": signed, "ca0": ca})
}

// ---- contacts and groups ----

func (s *Server) clientEndpoints(w http.ResponseWriter, r *http.Request, id *identity) {
	data := []map[string]string{}
	for _, c := range s.Router.Clients() {
		if !id.canSee(c.Groups) {
			continue
		}
		uid, cs := c.Identity()
		if uid == "" {
			continue
		}
		data = append(data, map[string]string{
			"callsign": cs, "uid": uid, "username": c.Username,
			"lastEventTime": c.LastSeen().UTC().Format(cot.TimeLayout), "lastStatus": "Connected",
		})
	}
	writeJSON(w, http.StatusOK, s.martiEnvelope("com.bbn.marti.remote.ClientEndpoint", data))
}

// contacts lists connected clients like the official /contacts/all APIs
// (RemoteSubscriptionLite, a bare JSON array), filtered by group.
func (s *Server) contacts(w http.ResponseWriter, r *http.Request, id *identity) {
	out := []map[string]any{}
	for _, c := range s.Router.Clients() {
		uid, cs := c.Identity()
		if uid == "" || !id.canSee(c.Groups) {
			continue
		}
		entry := map[string]any{"uid": uid, "callsign": cs, "filterGroups": c.Groups, "notes": "", "username": c.Username}
		if sa := c.LastSA(); sa != nil {
			if g := sa.Detail.Group; g != nil {
				entry["team"], entry["role"] = g.Name, g.Role
			}
			if t := sa.Detail.Takv; t != nil {
				entry["takv"] = strings.TrimSpace(t.Platform + " " + t.Version)
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) groupsAll(w http.ResponseWriter, r *http.Request, id *identity) {
	data := []map[string]any{}
	for i, g := range id.Groups {
		for _, dir := range []string{"IN", "OUT"} {
			data = append(data, map[string]any{
				"name": g, "direction": dir, "created": s.Started.Format("2006-01-02"),
				"type": "SYSTEM", "bitpos": i, "active": true,
			})
		}
	}
	writeJSON(w, http.StatusOK, s.martiEnvelope("com.bbn.marti.remote.groups.Group", data))
}

// ---- data packages ----

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validHash(h string) bool { return hashRE.MatchString(h) }

func (s *Server) missionUpload(w http.ResponseWriter, r *http.Request, id *identity) {
	q := r.URL.Query()
	r.Body = http.MaxBytesReader(w, r.Body, s.Cfg.MaxUploadMB<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "data package upload must be multipart/form-data with part 'assetfile'", http.StatusBadRequest)
		return
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			http.Error(w, "no 'assetfile' part in upload", http.StatusBadRequest)
			return
		}
		if part.FormName() != "assetfile" && part.FormName() != "resource" {
			part.Close()
			continue
		}
		name := firstNonEmpty(q.Get("filename"), part.FileName(), "package.zip")
		mimeType := firstNonEmpty(part.Header.Get("Content-Type"), "application/x-zip-compressed")
		if mimeType == "application/octet-stream" {
			mimeType = "application/x-zip-compressed"
		}
		keywords := []string{firstNonEmpty(q.Get("keyword"), "missionpackage")}
		p, err := s.savePackage(part, name, mimeType, firstNonEmpty(q.Get("creatorUid"), q.Get("CreatorUid")), keywords, firstNonEmpty(q.Get("tool"), "public"), id)
		if err != nil {
			s.uploadError(w, err)
			return
		}
		if h := q.Get("hash"); h != "" && !strings.EqualFold(h, p.Hash) {
			s.Log.Warn("upload hash mismatch", "claimed", h, "actual", p.Hash, "name", name)
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "%s/Marti/sync/content?hash=%s", baseURL(r), p.Hash)
		return
	}
}

// rawUpload handles /Marti/sync/upload, where the body is the file itself.
func (s *Server) rawUpload(w http.ResponseWriter, r *http.Request, id *identity) {
	q := r.URL.Query()
	r.Body = http.MaxBytesReader(w, r.Body, s.Cfg.MaxUploadMB<<20)
	mimeType := r.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(mimeType); err == nil && mt == "multipart/form-data" {
		s.missionUpload(w, r, id)
		return
	}
	var keywords []string
	for _, k := range q["keywords"] {
		keywords = append(keywords, strings.Split(k, ",")...)
	}
	p, err := s.savePackage(r.Body, firstNonEmpty(q.Get("name"), "upload"), firstNonEmpty(mimeType, "application/octet-stream"),
		firstNonEmpty(q.Get("CreatorUid"), q.Get("creatorUid")), keywords, firstNonEmpty(q.Get("tool"), "public"), id)
	if err != nil {
		s.uploadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, packageJSON(p))
}

func (s *Server) uploadError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		return
	}
	s.Log.Error("upload failed", "err", err)
	http.Error(w, "upload failed", http.StatusInternalServerError)
}

// savePackage streams src to disk, naming it by its SHA-256.
func (s *Server) savePackage(src io.Reader, name, mimeType, creatorUID string, keywords []string, tool string, id *identity) (*store.Package, error) {
	if err := os.MkdirAll(s.PackageDir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(s.PackageDir, "upload-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), src)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(tmp.Name(), filepath.Join(s.PackageDir, hash)); err != nil {
		return nil, err
	}
	groups := slices.Clone(id.Groups)
	if old, ok := s.Store.Package(hash); ok {
		// Re-uploading identical content must not hide it from earlier groups.
		for _, g := range old.Groups {
			if !slices.Contains(groups, g) {
				groups = append(groups, g)
			}
		}
	}
	p, err := s.Store.PutPackage(store.Package{
		Hash: hash, Name: filepath.Base(name), MIMEType: mimeType, Size: n, Keywords: keywords, Tool: tool,
		CreatorUID: creatorUID, Submitter: id.Username, Groups: groups, Submitted: time.Now().UTC(),
	})
	if err == nil {
		s.Log.Info("stored data package", "name", p.Name, "hash", hash, "size", n, "user", id.Username)
	}
	return p, err
}

// visiblePackage returns the package if it exists and id may see it, either
// through its groups or because it belongs to a mission id can read.
func (s *Server) visiblePackage(hash string, id *identity) (*store.Package, bool) {
	p, ok := s.Store.Package(strings.ToLower(hash))
	if !ok {
		return nil, false
	}
	if id.canSee(p.Groups) {
		return p, true
	}
	if s.Missions != nil && s.Missions.CanReadHash(p.Hash, mission.Caller{Username: id.Username, Groups: id.Groups, Admin: id.Admin, Anonymous: id.Anonymous}) {
		return p, true
	}
	return nil, false
}

func (s *Server) missionQuery(w http.ResponseWriter, r *http.Request, id *identity) {
	p, ok := s.visiblePackage(r.URL.Query().Get("hash"), id)
	if !ok {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "%s/Marti/sync/content?hash=%s", baseURL(r), p.Hash)
}

func (s *Server) content(w http.ResponseWriter, r *http.Request, id *identity) {
	hash := strings.ToLower(firstNonEmpty(r.URL.Query().Get("hash"), r.URL.Query().Get("uid")))
	p, ok := s.visiblePackage(hash, id)
	if !ok || !validHash(p.Hash) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	f, err := os.Open(filepath.Join(s.PackageDir, p.Hash))
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", p.MIMEType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": p.Name}))
	http.ServeContent(w, r, "", p.Submitted, f)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request, id *identity) {
	q := r.URL.Query()
	var keywords []string
	for _, k := range append(q["keywords"], q["keyword"]...) {
		keywords = append(keywords, strings.Split(k, ",")...)
	}
	tool, name := q.Get("tool"), q.Get("name")
	results := []map[string]any{}
	for _, p := range s.Store.Packages() {
		if !id.canSee(p.Groups) || (tool != "" && p.Tool != tool) || (name != "" && p.Name != name) {
			continue
		}
		match := true
		for _, k := range keywords {
			if k != "" && !slices.Contains(p.Keywords, k) {
				match = false
			}
		}
		if match {
			results = append(results, packageJSON(p))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"resultCount": len(results), "results": results})
}

// apiSearch is /Marti/api/sync/search: the same search as /Marti/sync/search
// but answered as an ApiResponse of Resource objects.
func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request, id *identity) {
	q := r.URL.Query()
	var keywords []string
	for _, k := range append(q["keyword"], q["keywords"]...) {
		keywords = append(keywords, strings.Split(k, ",")...)
	}
	exact := map[string]func(*store.Package) string{
		"tool":     func(p *store.Package) string { return p.Tool },
		"name":     func(p *store.Package) string { return p.Name },
		"filename": func(p *store.Package) string { return p.Name },
		"mimetype": func(p *store.Package) string { return p.MIMEType },
		"hash":     func(p *store.Package) string { return p.Hash },
		"uid":      func(p *store.Package) string { return p.Hash },
	}
	results := []map[string]any{}
	for _, p := range s.Store.Packages() {
		if !id.canSee(p.Groups) {
			continue
		}
		match := true
		for param, field := range exact {
			if v := q.Get(param); v != "" && !strings.EqualFold(field(p), v) {
				match = false
			}
		}
		for _, k := range keywords {
			if k != "" && !slices.Contains(p.Keywords, k) {
				match = false
			}
		}
		if match {
			results = append(results, mission.ResourceJSON(p))
		}
	}
	writeJSON(w, http.StatusOK, s.martiEnvelope("Resource", results))
}

// fileByHash serves /Marti/api/files/{hash}.
func (s *Server) fileByHash(w http.ResponseWriter, r *http.Request, id *identity) {
	q := r.URL.Query()
	q.Set("hash", r.PathValue("hash"))
	r.URL.RawQuery = q.Encode()
	s.content(w, r, id)
}

func (s *Server) setKeywords(w http.ResponseWriter, r *http.Request, id *identity) {
	p, ok := s.visiblePackage(r.PathValue("hash"), id)
	if !ok {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	if !id.Admin && p.Submitter != id.Username {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var kw []string
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&kw); err != nil {
		http.Error(w, "keywords must be a JSON array of strings", http.StatusBadRequest)
		return
	}
	if err := s.Store.UpdatePackage(p.Hash, func(p *store.Package) { p.Keywords = kw }); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// packageJSON renders metadata like the official server's Metadata.toJSONObject.
func packageJSON(p *store.Package) map[string]any {
	kw := p.Keywords
	if kw == nil {
		kw = []string{}
	}
	return map[string]any{
		"UID": p.Hash, "Hash": p.Hash, "Name": p.Name, "MIMEType": p.MIMEType,
		"Size": strconv.FormatInt(p.Size, 10), "PrimaryKey": strconv.FormatInt(p.Seq, 10),
		"SubmissionDateTime": p.Submitted.Format(cot.TimeLayout), "SubmissionUser": p.Submitter,
		"CreatorUid": p.CreatorUID, "Keywords": kw, "Tool": p.Tool, "Groups": p.Groups, "EXPIRATION": "-1",
	}
}

func (s *Server) setTool(w http.ResponseWriter, r *http.Request, id *identity) {
	p, ok := s.visiblePackage(r.PathValue("hash"), id)
	if !ok {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	if !id.Admin && p.Submitter != id.Username {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 256))
	tool := strings.TrimSpace(string(body))
	if tool == "" {
		tool = "public"
	}
	if err := s.Store.UpdatePackage(p.Hash, func(p *store.Package) { p.Tool = tool }); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request, id *identity) {
	p, ok := s.visiblePackage(r.PathValue("hash"), id)
	if !ok {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	if !id.Admin && p.Submitter != id.Username {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := s.removePackage(p.Hash); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) removePackage(hash string) error {
	if err := s.Store.DeletePackage(hash); err != nil {
		return err
	}
	if validHash(hash) {
		os.Remove(filepath.Join(s.PackageDir, hash))
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
