package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/release"
)

// Updating EdgeTAK from the admin console.
//
// EdgeTAK runs unprivileged and cannot replace itself. The installer adds a
// root-owned updater (edgetak-update.service, started by edgetak-update.path
// when <data>/update/request changes). EdgeTAK only writes the wanted version
// into that file; the updater checks it, downloads that release's installer
// from GitHub, verifies it against the release's SHA256SUMS and reruns it
// with the options the server was installed with. It reports progress in a
// root-owned status file.

const (
	edgetakRepo = "brandongotak/EdgeTAK"
	// The first release whose installer contains the updater; older ones
	// would remove the ability to update from the console.
	minUpdateVersion = "v0.2.15"
)

var (
	updaterScript    = "/usr/local/lib/edgetak/edgetak-update.sh"
	updateStatusFile = "/var/lib/edgetak-update/status"
	githubAPI        = "" // "" = api.github.com; overridden in tests
)

type selfUpdateState struct {
	mu        sync.Mutex
	latest    *release.Release
	checkedAt time.Time
	requested string
	at        time.Time
}

func (s *Server) updateRequestFile() string {
	return filepath.Join(filepath.Dir(s.PackageDir), "update", "request")
}

// updaterAvailable reports whether this server can update itself, and why not.
func (s *Server) updaterAvailable() (bool, string) {
	if runtime.GOOS != "linux" {
		return false, "Updating from the console works on servers installed with the Linux installer."
	}
	if _, err := os.Stat(updaterScript); err != nil {
		return false, "Rerun the installer once (the one-line install command) to turn on updates from the console."
	}
	if fi, err := os.Stat(filepath.Dir(s.updateRequestFile())); err != nil || !fi.IsDir() {
		return false, "The update folder is missing; rerun the installer."
	}
	return true, ""
}

type updateStatus struct {
	State   string    `json:"state"` // running, done, error
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// readUpdateStatus parses the updater's "<time> <state> <message>" line.
func readUpdateStatus() *updateStatus {
	b, err := os.ReadFile(updateStatusFile)
	if err != nil {
		return nil
	}
	f := strings.SplitN(strings.TrimSpace(string(b)), " ", 3)
	if len(f) < 2 {
		return nil
	}
	st := &updateStatus{State: f[1]}
	st.At, _ = time.Parse(time.RFC3339, f[0])
	if len(f) == 3 {
		st.Message = f[2]
	}
	return st
}

func (s *Server) selfUpdateView() map[string]any {
	ok, why := s.updaterAvailable()
	s.selfUpdate.mu.Lock()
	latest, requested, at := s.selfUpdate.latest, s.selfUpdate.requested, s.selfUpdate.at
	s.selfUpdate.mu.Unlock()
	v := map[string]any{"version": s.Version, "available": ok, "reason": why, "latest": latest, "status": readUpdateStatus()}
	if latest != nil {
		v["update_available"] = release.Newer(latest.Version, s.Version)
	}
	if requested != "" {
		v["requested"], v["requested_at"] = requested, at
	}
	return v
}

func (s *Server) updateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/server/update", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.selfUpdateView())
	}))
	mux.HandleFunc("POST /admin/api/server/update/check", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if err := s.checkSelfUpdate(r.Context()); err != nil {
			jsonError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.selfUpdateView())
	}))
	mux.HandleFunc("POST /admin/api/server/update", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			Version string `json:"version"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req)
		if err := s.requestUpdate(req.Version); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.Log.Info("EdgeTAK update requested", "admin", id.Username, "from", s.Version, "to", req.Version)
		writeJSON(w, http.StatusAccepted, s.selfUpdateView())
	}))
}

// checkSelfUpdate looks up the latest EdgeTAK release.
func (s *Server) checkSelfUpdate(ctx context.Context) error {
	rel, err := release.Latest(ctx, githubAPI, edgetakRepo)
	if err != nil {
		return err
	}
	s.selfUpdate.mu.Lock()
	s.selfUpdate.latest, s.selfUpdate.checkedAt = rel, time.Now()
	s.selfUpdate.mu.Unlock()
	return nil
}

// requestUpdate asks the root updater to install version.
func (s *Server) requestUpdate(version string) error {
	if ok, why := s.updaterAvailable(); !ok {
		return errors.New(why)
	}
	if !release.VersionRE.MatchString(version) || !strings.HasPrefix(version, "v") {
		return errors.New("invalid version")
	}
	if release.Newer(minUpdateVersion, version) {
		return errors.New("versions before " + minUpdateVersion + " cannot be installed from the console")
	}
	if st := readUpdateStatus(); st != nil && st.State == "running" && time.Since(st.At) < 15*time.Minute {
		return errors.New("an update is already running")
	}
	// Written in place (not renamed) so the path unit sees the file close.
	if err := os.WriteFile(s.updateRequestFile(), []byte(version+"\n"), 0o640); err != nil {
		return err
	}
	s.selfUpdate.mu.Lock()
	s.selfUpdate.requested, s.selfUpdate.at = version, time.Now()
	s.selfUpdate.mu.Unlock()
	return nil
}
