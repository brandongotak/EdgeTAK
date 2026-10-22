package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/overlay"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Overlays (Data -> Overlays): an admin imports a KML, KMZ or GeoJSON file
// once and every device in the chosen groups gets it as a data package
// ATAK imports as a map overlay (one entry it can show or hide). EdgeTAK
// sends it when it is added or replaced, and to each device as it connects
// until that device has the current version, so phones that were off get
// it too. A new version replaces the old one on devices (same package UID).
// ATAK has no way to be told to delete an overlay, so deleting one here
// only stops it being sent.

func (s *Server) overlaysDir() string { return filepath.Join(filepath.Dir(s.PackageDir), "overlays") }

func (s *Server) overlayPackage(o store.Overlay) string {
	return filepath.Join(s.overlaysDir(), o.ID+".zip")
}

func overlayUID(o store.Overlay) string { return "edgetak-overlay-" + o.ID }

func overlayVisible(o store.Overlay, c *router.Client) bool {
	return watched(store.LostContactSettings{Groups: o.Groups}, deviceGroups(c))
}

// overlayByHash is the overlay whose data package has this hash, if id
// may download it.
func (s *Server) overlayByHash(hash string, id *identity) (store.Overlay, bool) {
	for _, o := range s.Store.Overlays() {
		if o.Hash == hash && (len(o.Groups) == 0 || id.canSee(o.Groups)) {
			return o, true
		}
	}
	return store.Overlay{}, false
}

// serveOverlay sends an overlay's data package (Marti content downloads).
func (s *Server) serveOverlay(w http.ResponseWriter, r *http.Request, o store.Overlay) {
	f, err := os.Open(s.overlayPackage(o))
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": overlay.SafeName(o.Name, ".zip")}))
	http.ServeContent(w, r, "", o.Updated, f)
}

// deliverOverlay offers an overlay to the connected devices that should
// have it and don't have its current version (all of them with force).
func (s *Server) deliverOverlay(o store.Overlay, clients []*router.Client, force bool) (sent int) {
	if o.Paused {
		return 0
	}
	p := &store.Package{Name: overlay.SafeName(o.Name, ".zip"), Hash: o.Hash, Size: o.Size}
	for _, c := range clients {
		uid, _ := c.Identity()
		if uid == "" || !overlayVisible(o, c) || (!force && o.Delivered[uid] == o.Version) {
			continue
		}
		url := s.contentURL(c.Transport, o.Hash)
		if url == "" {
			continue
		}
		c.Send(s.fileShareEvent(s.Cfg.ServerName, url, p))
		s.Store.OverlayDelivered(o.ID, uid, o.Version)
		sent++
	}
	if sent > 0 {
		s.Log.Info("overlay sent", "overlay", o.Name, "version", o.Version, "devices", sent)
	}
	return sent
}

// overlaysStart sends each connecting device the overlays it lacks, once
// it has said who it is.
func (s *Server) overlaysStart() {
	s.Router.OnJoin(func(c *router.Client) {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); {
			if uid, _ := c.Identity(); uid != "" {
				for _, o := range s.Store.Overlays() {
					s.deliverOverlay(o, []*router.Client{c}, false)
				}
				return
			}
			select {
			case <-c.Done():
				return
			case <-t.C:
			}
		}
	})
}

type overlayView struct {
	store.Overlay
	Delivered int `json:"delivered"` // devices sent the current version
	Waiting   int `json:"waiting"`   // connected devices that should have it and don't
}

func (s *Server) overlayViews() []overlayView {
	clients := s.Router.Clients()
	out := []overlayView{}
	for _, o := range s.Store.Overlays() {
		v := overlayView{Overlay: o}
		for _, version := range o.Delivered {
			if version == o.Version {
				v.Delivered++
			}
		}
		for _, c := range clients {
			if uid, _ := c.Identity(); uid != "" && overlayVisible(o, c) && o.Delivered[uid] != o.Version {
				v.Waiting++
			}
		}
		v.Overlay.Delivered = nil
		out = append(out, v)
	}
	return out
}

// readOverlayUpload reads the "file" part of a multipart upload, and the
// name and groups fields when given.
func readOverlayUpload(w http.ResponseWriter, r *http.Request) (data []byte, fileName, name string, groups []string, err error) {
	r.Body = http.MaxBytesReader(w, r.Body, overlay.MaxSize+64<<10)
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, "", "", nil, err
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", "", nil, err
		}
		switch part.FormName() {
		case "file":
			fileName = filepath.Base(strings.ReplaceAll(part.FileName(), `\`, "/"))
			data, err = io.ReadAll(io.LimitReader(part, overlay.MaxSize+1))
		case "name":
			var b []byte
			b, err = io.ReadAll(io.LimitReader(part, 1<<10))
			name = strings.TrimSpace(string(b))
		case "groups":
			var b []byte
			if b, err = io.ReadAll(io.LimitReader(part, 64<<10)); err == nil {
				err = json.Unmarshal(b, &groups)
			}
		}
		part.Close()
		if err != nil {
			return nil, "", "", nil, err
		}
	}
	return data, fileName, name, groups, nil
}

// buildOverlay checks the file and writes the overlay's data package,
// filling in its format, size, hash and extent.
func (s *Server) buildOverlay(o *store.Overlay, data []byte, fileName string) error {
	if len(data) > overlay.MaxSize {
		return errors.New("overlay files can be up to 64 MB")
	}
	f, err := overlay.Read(data, fileName)
	if err != nil {
		return err
	}
	pkg, err := overlay.Package(overlayUID(*o), o.Name, overlay.SafeName(o.Name, f.Ext), f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.overlaysDir(), 0o750); err != nil {
		return err
	}
	// Keep the KML/KMZ too, for downloading and rebuilding on rename.
	src := filepath.Join(s.overlaysDir(), o.ID+".src")
	if err := os.WriteFile(src+".tmp", f.Data, 0o640); err != nil {
		return err
	}
	if err := os.Rename(src+".tmp", src); err != nil {
		return err
	}
	tmp := s.overlayPackage(*o) + ".tmp"
	if err := os.WriteFile(tmp, pkg, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.overlayPackage(*o)); err != nil {
		return err
	}
	sum := sha256.Sum256(pkg)
	o.Format, o.Items, o.Bounds, o.Size, o.Hash = f.Format, f.Items, f.Bounds, int64(len(pkg)), hex.EncodeToString(sum[:])
	if fileName != "" {
		o.FileName = fileName
	}
	return nil
}

func validOverlayName(name string) bool {
	return name != "" && len(name) <= 80 && !strings.ContainsAny(name, "\x00\r\n")
}

func (s *Server) overlayRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/overlays", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.overlayViews())
	}))

	// Import: multipart with file, name (default: the file's) and groups (JSON).
	mux.HandleFunc("POST /admin/api/overlays", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		data, fileName, name, groups, err := readOverlayUpload(w, r)
		if err != nil || data == nil {
			jsonError(w, http.StatusBadRequest, "send the file as multipart form data (field \"file\")")
			return
		}
		name = firstNonEmpty(name, strings.TrimSuffix(fileName, filepath.Ext(fileName)))
		if !validOverlayName(name) {
			jsonError(w, http.StatusBadRequest, "give the overlay a name (up to 80 characters)")
			return
		}
		now := time.Now().UTC()
		o := store.Overlay{ID: uuid(), Name: name, Groups: cleanGroups(groups), Version: 1, Author: id.Username, Created: now, Updated: now}
		if err := s.buildOverlay(&o, data, fileName); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.Store.PutOverlay(o); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("overlay added", "admin", id.Username, "overlay", o.Name, "format", o.Format, "items", o.Items, "groups", o.Groups)
		s.deliverOverlay(o, s.Router.Clients(), false)
		writeJSON(w, http.StatusCreated, s.overlayViews())
	}))

	// Replace the file: a new version, sent to every device again.
	mux.HandleFunc("PUT /admin/api/overlays/{id}/file", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		o, ok := s.Store.Overlay(r.PathValue("id"))
		if !ok {
			jsonError(w, http.StatusNotFound, "no such overlay")
			return
		}
		data, fileName, _, _, err := readOverlayUpload(w, r)
		if err != nil || data == nil {
			jsonError(w, http.StatusBadRequest, "send the file as multipart form data (field \"file\")")
			return
		}
		o.Version++
		o.Updated = time.Now().UTC()
		if err := s.buildOverlay(&o, data, fileName); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.Store.PutOverlay(o); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		o, _ = s.Store.Overlay(o.ID)
		s.Log.Info("overlay replaced", "admin", id.Username, "overlay", o.Name, "version", o.Version, "items", o.Items)
		s.deliverOverlay(o, s.Router.Clients(), false)
		writeJSON(w, http.StatusOK, s.overlayViews())
	}))

	// Rename (a new version), change groups, pause or resume sending.
	mux.HandleFunc("PATCH /admin/api/overlays/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		o, ok := s.Store.Overlay(r.PathValue("id"))
		if !ok {
			jsonError(w, http.StatusNotFound, "no such overlay")
			return
		}
		var req struct {
			Name   *string   `json:"name"`
			Groups *[]string `json:"groups"`
			Paused *bool     `json:"paused"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if req.Groups != nil {
			o.Groups = cleanGroups(*req.Groups)
			slices.Sort(o.Groups)
		}
		if req.Paused != nil {
			o.Paused = *req.Paused
		}
		if req.Name != nil && strings.TrimSpace(*req.Name) != o.Name {
			name := strings.TrimSpace(*req.Name)
			if !validOverlayName(name) {
				jsonError(w, http.StatusBadRequest, "give the overlay a name (up to 80 characters)")
				return
			}
			src, err := os.ReadFile(filepath.Join(s.overlaysDir(), o.ID+".src"))
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			o.Name, o.Version = name, o.Version+1
			if err := s.buildOverlay(&o, src, ""); err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		o.Updated = time.Now().UTC()
		if err := s.Store.PutOverlay(o); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		o, _ = s.Store.Overlay(o.ID)
		s.Log.Info("overlay changed", "admin", id.Username, "overlay", o.Name, "groups", o.Groups, "paused", o.Paused)
		s.deliverOverlay(o, s.Router.Clients(), false)
		writeJSON(w, http.StatusOK, s.overlayViews())
	}))

	// Send again to every connected device that should have it.
	mux.HandleFunc("POST /admin/api/overlays/{id}/send", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		o, ok := s.Store.Overlay(r.PathValue("id"))
		if !ok {
			jsonError(w, http.StatusNotFound, "no such overlay")
			return
		}
		if o.Paused {
			jsonError(w, http.StatusBadRequest, "resume sending first")
			return
		}
		n := s.deliverOverlay(o, s.Router.Clients(), true)
		writeJSON(w, http.StatusOK, map[string]any{"sent": n, "overlays": s.overlayViews()})
	}))

	// The overlay's KML or KMZ (GeoJSON as converted).
	mux.HandleFunc("GET /admin/api/overlays/{id}/file", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		o, ok := s.Store.Overlay(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		ext, ct := ".kml", "application/vnd.google-earth.kml+xml"
		if o.Format == "kmz" {
			ext, ct = ".kmz", "application/vnd.google-earth.kmz"
		}
		f, err := os.Open(filepath.Join(s.overlaysDir(), o.ID+".src"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": overlay.SafeName(o.Name, ext)}))
		http.ServeContent(w, r, "", o.Updated, f)
	}))

	mux.HandleFunc("DELETE /admin/api/overlays/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		o, err := s.Store.DeleteOverlay(r.PathValue("id"))
		if err != nil {
			jsonError(w, http.StatusNotFound, "no such overlay")
			return
		}
		os.Remove(s.overlayPackage(o))
		os.Remove(filepath.Join(s.overlaysDir(), o.ID+".src"))
		s.Log.Info("overlay deleted", "admin", id.Username, "overlay", o.Name)
		writeJSON(w, http.StatusOK, s.overlayViews())
	}))
}
