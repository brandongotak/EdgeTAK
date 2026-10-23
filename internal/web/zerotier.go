package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// ZeroTier (Settings → ZeroTier): EdgeTAK manages the ZeroTier service on
// the same machine through ZeroTier's local API (127.0.0.1:9993), so devices
// on a ZeroTier network reach the server at its ZeroTier address. Installing
// ZeroTier needs root, so as with updates the installer adds a root-owned
// helper (edgetak-zerotier.service, started when <data>/zerotier/request
// changes): EdgeTAK only writes "install" there. The helper installs
// zerotier-one from ZeroTier's signed apt repository and gives EdgeTAK a copy
// of the service's API token in <data>/zerotier/authtoken.

var (
	zerotierHelper     = "/usr/local/lib/edgetak/edgetak-zerotier.sh"
	zerotierStatusFile = "/var/lib/edgetak-zerotier/status"
	// Where ZeroTier keeps its token when EdgeTAK may read it directly (it
	// runs as an administrator, e.g. on Windows).
	zerotierTokenPaths = []string{"/var/lib/zerotier-one/authtoken.secret", `C:\ProgramData\ZeroTier\One\authtoken.secret`,
		"/Library/Application Support/ZeroTier/One/authtoken.secret"}
)

var zerotierNetworkID = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (s *Server) zerotierDir() string { return filepath.Join(filepath.Dir(s.PackageDir), "zerotier") }

// zerotierToken is the ZeroTier service's API token, if EdgeTAK has one.
func (s *Server) zerotierToken() string {
	for _, p := range append([]string{filepath.Join(s.zerotierDir(), "authtoken")}, zerotierTokenPaths...) {
		if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// zerotierCall makes a request to the local ZeroTier API and decodes its
// JSON answer into out (if not nil).
func (s *Server) zerotierCall(ctx context.Context, method, path string, body any, out any) error {
	token := s.zerotierToken()
	if token == "" {
		return errZeroTierNoToken
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, firstNonEmpty(s.ZeroTierAPI, "http://127.0.0.1:9993")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-ZT1-Auth", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach the ZeroTier service: is zerotier-one running? (%v)", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errors.New("ZeroTier refused EdgeTAK's API token; click Install to give EdgeTAK the current one")
	case resp.StatusCode >= 300:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("ZeroTier answered %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	return nil
}

var errZeroTierNoToken = errors.New("EdgeTAK isn't connected to ZeroTier yet")

// zerotierCanInstall reports whether the console can install ZeroTier here,
// and why not.
func (s *Server) zerotierCanInstall() (bool, string) {
	if os.Getenv("EDGETAK_CONTAINER") != "" {
		return false, "EdgeTAK runs in a container: install ZeroTier on the host, then give the container its network (network_mode: host)."
	}
	if runtime.GOOS != "linux" {
		return false, "Install ZeroTier yourself (zerotier.com/download); EdgeTAK connects to it when it can read its API token."
	}
	if _, err := os.Stat(zerotierHelper); err != nil {
		return false, "Update EdgeTAK (or rerun the installer) to install ZeroTier from here."
	}
	return true, ""
}

type zerotierNetwork struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Status    string   `json:"status"` // OK, REQUESTING_CONFIGURATION, ACCESS_DENIED, NOT_FOUND, ...
	Type      string   `json:"type"`
	Addresses []string `json:"assignedAddresses"`
	Device    string   `json:"portDeviceName"`
}

// zerotierView is the Settings card's state.
func (s *Server) zerotierView(ctx context.Context) map[string]any {
	can, why := s.zerotierCanInstall()
	v := map[string]any{"can_install": can, "install_note": why, "connected": false, "networks": []zerotierNetwork{}}
	if st := readStatusFile(zerotierStatusFile); st != nil {
		v["install"] = st
	}
	var node struct {
		Address string `json:"address"`
		Version string `json:"version"`
		Online  bool   `json:"online"`
	}
	if err := s.zerotierCall(ctx, "GET", "/status", nil, &node); err != nil {
		if !errors.Is(err, errZeroTierNoToken) {
			v["error"] = err.Error()
		}
		return v
	}
	v["connected"], v["node"] = true, node
	var nets []zerotierNetwork
	if err := s.zerotierCall(ctx, "GET", "/network", nil, &nets); err != nil {
		v["error"] = err.Error()
		return v
	}
	if nets == nil {
		nets = []zerotierNetwork{}
	}
	v["networks"] = nets
	// Addresses the server certificate doesn't cover yet: EdgeTAK adds the
	// machine's addresses to it when it starts.
	var covered []string
	if leaf := s.CA.ServerCert.Leaf; leaf != nil {
		for _, ip := range leaf.IPAddresses {
			covered = append(covered, ip.String())
		}
	}
	missing := []string{}
	for _, n := range nets {
		for _, a := range n.Addresses {
			if ip, _, err := net.ParseCIDR(a); err == nil && !slices.Contains(covered, ip.String()) {
				missing = append(missing, ip.String())
			}
		}
	}
	v["cert_missing"] = missing
	return v
}

func (s *Server) zerotierRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/zerotier", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.zerotierView(r.Context()))
	}))
	// Install ZeroTier, or connect EdgeTAK to one installed by hand.
	mux.HandleFunc("POST /admin/api/zerotier/install", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if ok, why := s.zerotierCanInstall(); !ok {
			jsonError(w, http.StatusBadRequest, why)
			return
		}
		os.MkdirAll(s.zerotierDir(), 0o750)
		if err := os.WriteFile(filepath.Join(s.zerotierDir(), "request"), []byte("install\n"), 0o640); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("ZeroTier install requested", "admin", id.Username)
		writeJSON(w, http.StatusAccepted, s.zerotierView(r.Context()))
	}))
	mux.HandleFunc("POST /admin/api/zerotier/networks", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			NetworkID string `json:"network_id"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req)
		nwid := strings.ToLower(strings.TrimSpace(req.NetworkID))
		if !zerotierNetworkID.MatchString(nwid) {
			jsonError(w, http.StatusBadRequest, "a ZeroTier network ID is 16 characters, 0-9 and a-f")
			return
		}
		if err := s.zerotierCall(r.Context(), "POST", "/network/"+nwid, map[string]any{}, nil); err != nil {
			jsonError(w, http.StatusBadGateway, err.Error())
			return
		}
		s.Log.Info("joined ZeroTier network", "admin", id.Username, "network", nwid)
		writeJSON(w, http.StatusOK, s.zerotierView(r.Context()))
	}))
	mux.HandleFunc("DELETE /admin/api/zerotier/networks/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		nwid := r.PathValue("id")
		if !zerotierNetworkID.MatchString(nwid) {
			jsonError(w, http.StatusBadRequest, "invalid network ID")
			return
		}
		if err := s.zerotierCall(r.Context(), "DELETE", "/network/"+nwid, nil, nil); err != nil {
			jsonError(w, http.StatusBadGateway, err.Error())
			return
		}
		s.Log.Info("left ZeroTier network", "admin", id.Username, "network", nwid)
		writeJSON(w, http.StatusOK, s.zerotierView(r.Context()))
	}))
	// Restart EdgeTAK, so its certificate covers new addresses.
	mux.HandleFunc("POST /admin/api/server/restart", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if s.Restart == nil {
			jsonError(w, http.StatusNotImplemented, "restart EdgeTAK yourself (sudo systemctl restart edgetak)")
			return
		}
		s.Log.Warn("restart requested", "admin", id.Username)
		w.WriteHeader(http.StatusAccepted)
		go func() { time.Sleep(500 * time.Millisecond); s.Restart() }()
	}))
}
