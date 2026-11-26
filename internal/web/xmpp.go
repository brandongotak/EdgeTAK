package web

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
	"github.com/brandongotak/EdgeTAK/internal/xmpp"
)

// XMPP chat (Messages → XMPP chat): the Prosody XMPP server, for TAK Chat
// and other XMPP apps. Installing Prosody needs root, so the installer adds
// a root-owned helper (edgetak-xmpp.service, started when
// <data>/xmpp/request changes) that installs the system package; EdgeTAK
// then runs Prosody itself (see internal/xmpp), with EdgeTAK's server
// certificate, its users and passwords, and its groups as contact lists.

const (
	xmppDefaultPort        = 5222
	xmppDefaultUploadPort  = 5281
	xmppDefaultLocalPort   = 5280
	xmppDefaultUploadMB    = 25
	xmppDefaultUploadDays  = 7
	xmppDefaultHistoryDays = 30
	xmppSyncEvery          = 10 * time.Second
)

var (
	xmppHelper     = "/usr/local/lib/edgetak/edgetak-xmpp.sh"
	xmppStatusFile = "/var/lib/edgetak-xmpp/status"
	xmppRoomNode   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,47}$`)
	xmppDomainRE   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

type xmppState struct {
	mu       sync.Mutex
	authAddr string
	applied  string // the configuration Prosody runs with
}

// xmppEffective fills in the defaults.
func (s *Server) xmppEffective() store.XMPPSettings {
	x := s.Store.XMPP()
	if x.Port == 0 {
		x.Port = xmppDefaultPort
	}
	if x.UploadPort == 0 {
		x.UploadPort = xmppDefaultUploadPort
	}
	if x.UploadMB == 0 {
		x.UploadMB = xmppDefaultUploadMB
	}
	if x.UploadDays == 0 {
		x.UploadDays = xmppDefaultUploadDays
	}
	if x.HistoryDays == 0 {
		x.HistoryDays = xmppDefaultHistoryDays
	}
	return x
}

// xmppDomain is the XMPP domain: users are name@domain.
func (s *Server) xmppDomain(x store.XMPPSettings) string {
	return strings.ToLower(firstNonEmpty(x.Domain, s.Cfg.PublicHost, "localhost"))
}

// xmppLocalpart is a user's XMPP name, or "" if their EdgeTAK name can't be
// one (it has an @).
func xmppLocalpart(username string) string {
	if strings.Contains(username, "@") {
		return ""
	}
	return strings.ToLower(username)
}

func (s *Server) xmppStart(ctx context.Context) {
	if s.XMPP == nil {
		return
	}
	if s.Store.XMPP().Secret == "" {
		s.Store.UpdateXMPP(func(x *store.XMPPSettings) error {
			if x.Secret == "" {
				x.Secret = randomCode() + randomCode()
			}
			return nil
		})
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.Log.Error("XMPP chat: can't open the sign-in listener", "err", err)
		return
	}
	s.xmpp.mu.Lock()
	s.xmpp.authAddr = ln.Addr().String()
	s.xmpp.mu.Unlock()
	srv := &http.Server{Handler: http.HandlerFunc(s.xmppAuth), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	go func() { <-ctx.Done(); srv.Close() }()

	if err := s.xmppApply(false); err != nil {
		s.Log.Error("XMPP chat: can't write Prosody's configuration", "err", err)
	}
	go s.XMPP.Run(ctx)
	go func() {
		t := time.NewTicker(xmppSyncEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.xmppSync()
			}
		}
	}()
}

// xmppApply writes Prosody's configuration from the settings and starts,
// stops or (with restart, when the configuration changed) restarts it.
func (s *Server) xmppApply(restart bool) error {
	x := s.xmppEffective()
	dir := s.XMPP.Dir
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o750); err != nil {
		return err
	}
	certFile, keyFile := filepath.Join(dir, "certs", "server.crt"), filepath.Join(dir, "certs", "server.key")
	if err := s.xmppWriteCert(certFile, keyFile); err != nil {
		return err
	}
	s.xmpp.mu.Lock()
	authAddr := s.xmpp.authAddr
	s.xmpp.mu.Unlock()
	c := xmpp.Config{Domain: s.xmppDomain(x), PublicHost: firstNonEmpty(s.Cfg.PublicHost, s.xmppDomain(x)), Port: x.Port,
		HTTPSPort: x.UploadPort, HTTPPort: xmppDefaultLocalPort, UploadMB: x.UploadMB, UploadDays: x.UploadDays,
		HistoryDays: x.HistoryDays, UserRooms: !x.NoUserRooms, AuthURL: "http://" + authAddr + "/auth", Secret: x.Secret,
		CertFile: certFile, KeyFile: keyFile}
	if err := s.XMPP.Configure(c); err != nil {
		return err
	}
	s.xmppSync()
	key := fmt.Sprintf("%+v", c)
	s.xmpp.mu.Lock()
	changed := s.xmpp.applied != key
	s.xmpp.applied = key
	s.xmpp.mu.Unlock()
	s.XMPP.SetEnabled(x.Enabled)
	if restart && changed && x.Enabled {
		s.XMPP.Restart()
	}
	return nil
}

// xmppWriteCert gives Prosody EdgeTAK's server certificate (signed by the
// CA that devices already trust).
func (s *Server) xmppWriteCert(certFile, keyFile string) error {
	var chain []byte
	for _, der := range s.CA.ServerCert.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(s.CA.ServerCert.PrivateKey)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(certFile, chain, 0o640); err != nil {
		return err
	}
	return writeFileAtomic(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600)
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(b) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// xmppSync gives Prosody the current users, groups and rooms.
func (s *Server) xmppSync() {
	if s.XMPP == nil {
		return
	}
	st := xmpp.State{Users: map[string]string{}, Groups: map[string][]string{}, Rooms: []xmpp.Room{}}
	inGroup := map[string][]string{} // group -> localparts
	for _, u := range s.Store.Users() {
		lp := xmppLocalpart(u.Username)
		if lp == "" || u.Disabled {
			continue
		}
		if _, dup := st.Users[lp]; dup {
			continue // two EdgeTAK names differing only in case: the first keeps it
		}
		st.Users[lp] = u.Username
		in, out := u.Effective(s.Cfg.AnonymousGroup)
		for _, g := range append(in, out...) {
			if g != s.Cfg.AnonymousGroup && !slices.Contains(inGroup[g], lp) {
				inGroup[g] = append(inGroup[g], lp)
			}
		}
	}
	for g, list := range inGroup {
		st.Groups[g] = list
	}
	for _, r := range s.Store.XMPP().Rooms {
		room := xmpp.Room{Node: r.Node, Name: r.Name, Description: r.Description, MembersOnly: len(r.Groups) > 0, Members: []string{}}
		for _, g := range r.Groups {
			for _, lp := range inGroup[g] {
				if !slices.Contains(room.Members, lp) {
					room.Members = append(room.Members, lp)
				}
			}
		}
		slices.Sort(room.Members)
		st.Rooms = append(st.Rooms, room)
	}
	if err := s.XMPP.WriteState(st); err != nil {
		s.Log.Warn("XMPP chat: can't write the user list for Prosody", "err", err)
	}
}

// xmppAuth answers Prosody's password checks.
func (s *Server) xmppAuth(w http.ResponseWriter, r *http.Request) {
	secret := s.Store.XMPP().Secret
	got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.Method != http.MethodPost || r.URL.Path != "/auth" || !isLoopback(r.RemoteAddr) || secret == "" ||
		subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		IP       string `json:"ip"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Prosody lowercases names: find the EdgeTAK user it means.
	name := req.Username
	for _, u := range s.Store.Users() {
		if xmppLocalpart(u.Username) == req.Username {
			name = u.Username
			break
		}
	}
	if net.ParseIP(req.IP) == nil {
		req.IP = ""
	}
	if _, err := s.passwordSignInFrom(req.IP, name, req.Password); err != nil {
		s.Log.Info("XMPP chat sign-in refused", "user", req.Username, "ip", req.IP, "err", err)
		http.Error(w, "not authorized", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// xmppCanInstall reports whether the console can install Prosody here.
func (s *Server) xmppCanInstall() (bool, string) {
	if os.Getenv("EDGETAK_CONTAINER") != "" {
		return false, "EdgeTAK runs in a container, which has no Prosody. Run EdgeTAK with the Linux installer to use XMPP chat."
	}
	if runtime.GOOS != "linux" {
		return false, "Install Prosody yourself (prosody.im/download); EdgeTAK runs it when it finds the prosody program."
	}
	if _, err := os.Stat(xmppHelper); err != nil {
		return false, "Update EdgeTAK (or rerun the installer) to install Prosody from here."
	}
	return true, ""
}

func (s *Server) xmppView() map[string]any {
	x := s.xmppEffective()
	st := s.XMPP.Status()
	can, why := s.xmppCanInstall()
	v := map[string]any{
		"settings": map[string]any{"enabled": x.Enabled, "domain": x.Domain, "port": x.Port, "upload_port": x.UploadPort,
			"upload_mb": x.UploadMB, "upload_days": x.UploadDays, "history_days": x.HistoryDays, "user_rooms": !x.NoUserRooms},
		"rooms": x.Rooms, "status": st, "domain": s.xmppDomain(x), "public_host": s.Cfg.PublicHost,
		"can_install": can, "install_note": why,
	}
	if x.Rooms == nil {
		v["rooms"] = []store.XMPPRoom{}
	}
	if ist := readStatusFile(xmppStatusFile); ist != nil {
		v["install"] = ist
	}
	// Names that can't sign in to XMPP.
	skipped := []string{}
	seen := map[string]bool{}
	for _, u := range s.Store.Users() {
		lp := xmppLocalpart(u.Username)
		if lp == "" || seen[lp] {
			skipped = append(skipped, u.Username)
		}
		seen[lp] = true
	}
	v["skipped_users"] = skipped
	// Does the server certificate cover the domain?
	if leaf := s.CA.ServerCert.Leaf; leaf != nil {
		v["cert_covers_domain"] = leaf.VerifyHostname(s.xmppDomain(x)) == nil
	}
	return v
}

func (s *Server) xmppRoutes(mux *http.ServeMux) {
	if s.XMPP == nil {
		return
	}
	mux.HandleFunc("GET /admin/api/xmpp", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.xmppView())
	}))
	mux.HandleFunc("PUT /admin/api/xmpp/settings", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			Enabled     bool   `json:"enabled"`
			Domain      string `json:"domain"`
			Port        int    `json:"port"`
			UploadPort  int    `json:"upload_port"`
			UploadMB    int    `json:"upload_mb"`
			UploadDays  int    `json:"upload_days"`
			HistoryDays int    `json:"history_days"`
			UserRooms   bool   `json:"user_rooms"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))
		switch {
		case req.Domain != "" && !xmppDomainRE.MatchString(req.Domain):
			jsonError(w, http.StatusBadRequest, "the domain is a host name like chat.example.com, or empty for this server's address")
			return
		case req.Port < 1 || req.Port > 65535 || req.UploadPort < 1 || req.UploadPort > 65535 || req.Port == req.UploadPort ||
			req.Port == xmppDefaultLocalPort || req.UploadPort == xmppDefaultLocalPort:
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("choose two different ports from 1 to 65535 (not %d, which Prosody uses inside this machine)", xmppDefaultLocalPort))
			return
		case req.UploadMB < 1 || req.UploadMB > 2048:
			jsonError(w, http.StatusBadRequest, "the file size limit is from 1 to 2048 MB")
			return
		case req.UploadDays < 1 || req.UploadDays > 3650 || req.HistoryDays < 1 || req.HistoryDays > 3650:
			jsonError(w, http.StatusBadRequest, "keep files and messages from 1 to 3650 days")
			return
		}
		if req.Enabled && s.XMPP.FindBinary() == "" {
			jsonError(w, http.StatusBadRequest, "install Prosody first")
			return
		}
		_, err := s.Store.UpdateXMPP(func(x *store.XMPPSettings) error {
			x.Enabled, x.Domain, x.NoUserRooms = req.Enabled, req.Domain, !req.UserRooms
			x.Port, x.UploadPort = zeroIf(req.Port, xmppDefaultPort), zeroIf(req.UploadPort, xmppDefaultUploadPort)
			x.UploadMB, x.UploadDays = zeroIf(req.UploadMB, xmppDefaultUploadMB), zeroIf(req.UploadDays, xmppDefaultUploadDays)
			x.HistoryDays = zeroIf(req.HistoryDays, xmppDefaultHistoryDays)
			return nil
		})
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.xmppApply(true); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("XMPP chat settings changed", "admin", id.Username, "enabled", req.Enabled, "domain", req.Domain, "port", req.Port)
		writeJSON(w, http.StatusOK, s.xmppView())
	}))
	mux.HandleFunc("POST /admin/api/xmpp/install", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if ok, why := s.xmppCanInstall(); !ok {
			jsonError(w, http.StatusBadRequest, why)
			return
		}
		os.MkdirAll(s.XMPP.Dir, 0o750)
		if err := os.WriteFile(filepath.Join(s.XMPP.Dir, "request"), []byte("install\n"), 0o640); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("Prosody install requested", "admin", id.Username)
		writeJSON(w, http.StatusAccepted, s.xmppView())
	}))
	mux.HandleFunc("POST /admin/api/xmpp/restart", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if !s.xmppEffective().Enabled {
			jsonError(w, http.StatusBadRequest, "XMPP chat is off")
			return
		}
		s.XMPP.Restart()
		s.Log.Info("Prosody restarted", "admin", id.Username)
		writeJSON(w, http.StatusOK, s.xmppView())
	}))
	saveRoom := func(w http.ResponseWriter, r *http.Request, id *identity, node string) {
		var req store.XMPPRoom
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		req.Name, req.Description = strings.TrimSpace(req.Name), strings.TrimSpace(req.Description)
		if node == "" {
			req.Node = strings.ToLower(strings.TrimSpace(req.Node))
			if req.Node == "" {
				req.Node = roomNode(req.Name)
			}
		} else {
			req.Node = node
		}
		if !xmppRoomNode.MatchString(req.Node) {
			jsonError(w, http.StatusBadRequest, "the room address is up to 48 characters: a-z, 0-9, . _ -")
			return
		}
		if req.Name == "" || len(req.Name) > 80 || len(req.Description) > 300 {
			jsonError(w, http.StatusBadRequest, "give the room a name (up to 80 characters) and a description of up to 300")
			return
		}
		req.Groups = cleanGroups(req.Groups)
		_, err := s.Store.UpdateXMPP(func(x *store.XMPPSettings) error {
			i := slices.IndexFunc(x.Rooms, func(o store.XMPPRoom) bool { return o.Node == req.Node })
			switch {
			case node == "" && i >= 0:
				return errors.New("a room with this address exists")
			case node != "" && i < 0:
				return errNotFound
			case i >= 0:
				x.Rooms[i] = req
			default:
				if len(x.Rooms) >= 200 {
					return errors.New("200 rooms is the most")
				}
				x.Rooms = append(x.Rooms, req)
			}
			return nil
		})
		if errors.Is(err, errNotFound) {
			jsonError(w, http.StatusNotFound, "no such room")
			return
		} else if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.xmppSync()
		s.Log.Info("XMPP chat room saved", "admin", id.Username, "room", req.Node, "groups", req.Groups)
		writeJSON(w, http.StatusOK, s.xmppView())
	}
	mux.HandleFunc("POST /admin/api/xmpp/rooms", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) { saveRoom(w, r, id, "") }))
	mux.HandleFunc("PUT /admin/api/xmpp/rooms/{node}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		saveRoom(w, r, id, r.PathValue("node"))
	}))
	mux.HandleFunc("DELETE /admin/api/xmpp/rooms/{node}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		node := r.PathValue("node")
		_, err := s.Store.UpdateXMPP(func(x *store.XMPPSettings) error {
			i := slices.IndexFunc(x.Rooms, func(o store.XMPPRoom) bool { return o.Node == node })
			if i < 0 {
				return errNotFound
			}
			x.Rooms = slices.Delete(x.Rooms, i, i+1)
			return nil
		})
		if err != nil {
			jsonError(w, http.StatusNotFound, "no such room")
			return
		}
		s.xmppSync()
		s.Log.Info("XMPP chat room deleted", "admin", id.Username, "room", node)
		writeJSON(w, http.StatusOK, s.xmppView())
	}))
}

var errNotFound = errors.New("not found")

func zeroIf(v, def int) int {
	if v == def {
		return 0
	}
	return v
}

// roomNode makes a room address from its name.
func roomNode(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
