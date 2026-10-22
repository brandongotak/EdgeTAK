package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/mission"
	"github.com/brandongotak/EdgeTAK/internal/pki"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

func (s *Server) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/api/login", s.signIn)
	mux.HandleFunc("POST /admin/api/logout", s.signOut)
	mux.HandleFunc("GET /admin/api/me", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		writeJSON(w, http.StatusOK, map[string]any{"username": id.Username, "groups": id.Groups})
	}))
	mux.HandleFunc("GET /admin/api/status", s.admin(s.status))
	mux.HandleFunc("GET /admin/api/clients", s.admin(s.listClients))
	mux.HandleFunc("POST /admin/api/clients/{id}/disconnect", s.admin(s.disconnectClient))

	mux.HandleFunc("GET /admin/api/users", s.admin(s.listUsers))
	mux.HandleFunc("POST /admin/api/users", s.admin(s.createUser))
	mux.HandleFunc("PATCH /admin/api/users/{name}", s.admin(s.updateUser))
	mux.HandleFunc("DELETE /admin/api/users/{name}", s.admin(s.deleteUser))
	mux.HandleFunc("POST /admin/api/users/{name}/certificate", s.admin(s.userCertificate))

	mux.HandleFunc("GET /admin/api/certs", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.Store.Certs())
	}))
	mux.HandleFunc("POST /admin/api/certs/{serial}/revoke", s.admin(s.revokeCert))

	mux.HandleFunc("GET /admin/api/packages", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.Store.Packages())
	}))
	mux.HandleFunc("DELETE /admin/api/packages/{hash}", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if err := s.removePackage(r.PathValue("hash")); err != nil {
			jsonError(w, http.StatusNotFound, "package not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.HandleFunc("GET /admin/api/missions", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		writeJSON(w, http.StatusOK, s.Missions.Summaries())
	}))
	mux.HandleFunc("DELETE /admin/api/missions/{name}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if _, err := s.Missions.Delete(r.PathValue("name"), mission.Caller{Username: id.Username, Groups: id.Groups, Admin: true}); err != nil {
			s.missionError(w, err)
			return
		}
		s.Log.Info("admin deleted mission", "admin", id.Username, "mission", r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.HandleFunc("GET /admin/api/connection-package", s.admin(s.connectionPackage))
	mux.HandleFunc("GET /admin/api/ca.pem", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="edgetak-ca.pem"`)
		w.Write([]byte("-----BEGIN CERTIFICATE-----\n" + pki.PEMBody(s.CA.Cert) + "\n-----END CERTIFICATE-----\n"))
	}))
}

// ---- status & clients ----

func (s *Server) status(w http.ResponseWriter, r *http.Request, _ *identity) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	writeJSON(w, http.StatusOK, map[string]any{
		"server_name": s.Cfg.ServerName, "version": s.Version, "public_host": s.Cfg.PublicHost,
		"started": s.Started, "uptime_seconds": int(time.Since(s.Started).Seconds()),
		"clients": len(s.Router.Clients()), "users": len(s.Store.Users()), "packages": len(s.Store.Packages()),
		"missions":  len(s.Missions.Summaries()),
		"memory_mb": mem.Alloc >> 20, "goroutines": runtime.NumGoroutine(),
		"listeners":  s.Addrs,
		"ca_subject": s.CA.Cert.Subject.CommonName, "ca_expires": s.CA.Cert.NotAfter,
		"public_cert": s.Public.Info(), "signin_url": strings.TrimSuffix(s.registerURL(""), "register"),
	})
}

type clientView struct {
	ID          uint64    `json:"id"`
	UID         string    `json:"uid"`
	Callsign    string    `json:"callsign"`
	Username    string    `json:"username"`
	Remote      string    `json:"remote"`
	Transport   string    `json:"transport"`
	Format      string    `json:"format"`
	GroupsIn    []string  `json:"groups_in"`
	GroupsOut   []string  `json:"groups_out"`
	ConnectedAt time.Time `json:"connected_at"`
	LastSeen    time.Time `json:"last_seen"`
	In          uint64    `json:"in"`
	Out         uint64    `json:"out"`
	Dropped     uint64    `json:"dropped"`
	Type        string    `json:"type,omitempty"`
	Lat         *float64  `json:"lat,omitempty"` // both set once the client has sent SA
	Lon         *float64  `json:"lon,omitempty"`
	Team        string    `json:"team,omitempty"`
	Role        string    `json:"role,omitempty"`
	Battery     *uint32   `json:"battery,omitempty"`
	Speed       float64   `json:"speed,omitempty"`
	Course      float64   `json:"course,omitempty"`
	Device      string    `json:"device,omitempty"`
	Platform    string    `json:"platform,omitempty"`
}

func (s *Server) listClients(w http.ResponseWriter, r *http.Request, _ *identity) {
	out := []clientView{}
	for _, c := range s.Router.Clients() {
		uid, cs := c.Identity()
		v := clientView{
			ID: c.ID, UID: uid, Callsign: cs, Username: c.Username, Remote: c.Remote, Transport: c.Transport, Format: c.Format(),
			GroupsIn: c.InGroups(), GroupsOut: c.OutGroups(), ConnectedAt: c.ConnectedAt, LastSeen: c.LastSeen(),
			In: c.In.Load(), Out: c.Out.Load(), Dropped: c.Dropped.Load(),
		}
		if sa := c.LastSA(); sa != nil {
			fillSA(&v, sa)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func fillSA(v *clientView, sa *cot.Event) {
	lat, lon := sa.Lat, sa.Lon
	v.Type, v.Lat, v.Lon = sa.Type, &lat, &lon
	d := sa.Detail
	if d.Group != nil {
		v.Team, v.Role = d.Group.Name, d.Group.Role
	}
	if d.Status != nil {
		b := d.Status.Battery
		v.Battery = &b
	}
	if d.Track != nil {
		v.Speed, v.Course = d.Track.Speed, d.Track.Course
	}
	if d.Takv != nil {
		v.Device, v.Platform = d.Takv.Device, d.Takv.Platform
	}
}

func (s *Server) disconnectClient(w http.ResponseWriter, r *http.Request, id *identity) {
	n, _ := strconv.ParseUint(r.PathValue("id"), 10, 64)
	c, ok := s.Router.Client(n)
	if !ok {
		jsonError(w, http.StatusNotFound, "client not connected")
		return
	}
	s.Log.Info("admin disconnected client", "admin", id.Username, "client", c.ID, "remote", c.Remote)
	c.Close()
	w.WriteHeader(http.StatusNoContent)
}

// ---- users ----

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

type userView struct {
	Username  string    `json:"username"`
	Groups    []string  `json:"groups"`     // both directions
	GroupsIn  []string  `json:"groups_in"`  // receive only
	GroupsOut []string  `json:"groups_out"` // send only
	Inactive  []string  `json:"inactive"`   // channels the user switched off ("group:IN")
	Admin     bool      `json:"admin"`
	Created   time.Time `json:"created"`
	Online    int       `json:"online"`
	TwoFactor bool      `json:"two_factor"`
	Source    string    `json:"source,omitempty"`            // "ldap" for directory users
	Missing   bool      `json:"directory_missing,omitempty"` // not found at the last directory sync
}

func newUserView(u *store.User, online int) userView {
	nn := func(l []string) []string {
		if l == nil {
			return []string{}
		}
		return l
	}
	return userView{u.Username, nn(u.Groups), nn(u.GroupsIn), nn(u.GroupsOut), nn(u.Inactive), u.Admin, u.Created, online, u.TwoFactor(), u.Source, u.DirectoryOff}
}

// applyUserGroups pushes a user's current memberships to their live connections.
func (s *Server) applyUserGroups(username string) {
	if u, ok := s.Store.User(username); ok {
		in, out := u.Effective(s.Cfg.AnonymousGroup)
		s.Router.SetUserGroups(username, in, out)
	}
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, _ *identity) {
	online := map[string]int{}
	for _, c := range s.Router.Clients() {
		online[c.Username]++
	}
	out := []userView{}
	for _, u := range s.Store.Users() {
		out = append(out, newUserView(u, online[u.Username]))
	}
	writeJSON(w, http.StatusOK, out)
}

func cleanGroups(in []string) []string {
	out := []string{}
	for _, g := range in {
		g = strings.TrimSpace(g)
		if g != "" && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct {
		Username string   `json:"username"`
		Password string   `json:"password"`
		Groups   []string `json:"groups"`
		Admin    bool     `json:"admin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !usernameRE.MatchString(req.Username) {
		jsonError(w, http.StatusBadRequest, "username must be 1-64 characters: letters, digits, . _ @ -")
		return
	}
	if len(req.Password) < 8 {
		jsonError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	u, err := s.Store.CreateUser(req.Username, req.Password, cleanGroups(req.Groups), req.Admin)
	if errors.Is(err, store.ErrExists) {
		jsonError(w, http.StatusConflict, "user already exists")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("user created", "admin", id.Username, "user", u.Username, "groups", u.Groups)
	writeJSON(w, http.StatusCreated, newUserView(u, 0))
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, id *identity) {
	name := r.PathValue("name")
	var req struct {
		Password  *string   `json:"password"`
		Groups    *[]string `json:"groups"`
		GroupsIn  *[]string `json:"groups_in"`
		GroupsOut *[]string `json:"groups_out"`
		Admin     *bool     `json:"admin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Admin != nil && !*req.Admin && name == id.Username {
		jsonError(w, http.StatusBadRequest, "you cannot remove your own administrator role")
		return
	}
	var hash string
	if req.Password != nil {
		if old, ok := s.Store.User(name); ok && old.Source == "ldap" {
			jsonError(w, http.StatusBadRequest, "this user signs in with their directory password; change it in the directory")
			return
		}
		if len(*req.Password) < 8 {
			jsonError(w, http.StatusBadRequest, "password must be at least 8 characters")
			return
		}
		var err error
		if hash, err = store.HashPassword(*req.Password); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	u, err := s.Store.UpdateUser(name, func(u *store.User) error {
		if hash != "" {
			u.PasswordHash = hash
		}
		if req.Groups != nil {
			u.Groups = cleanGroups(*req.Groups)
		}
		if req.GroupsIn != nil {
			u.GroupsIn = cleanGroups(*req.GroupsIn)
		}
		if req.GroupsOut != nil {
			u.GroupsOut = cleanGroups(*req.GroupsOut)
		}
		if req.Admin != nil {
			u.Admin = *req.Admin
		}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		jsonError(w, http.StatusNotFound, "user not found")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.applyUserGroups(name)
	s.Log.Info("user updated", "admin", id.Username, "user", name)
	writeJSON(w, http.StatusOK, newUserView(u, 0))
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, id *identity) {
	name := r.PathValue("name")
	if name == id.Username {
		jsonError(w, http.StatusBadRequest, "you cannot delete yourself")
		return
	}
	if err := s.Store.DeleteUser(name); err != nil {
		jsonError(w, http.StatusNotFound, "user not found")
		return
	}
	// Drop live connections authenticated as this user; their certs are now revoked.
	for _, c := range s.Router.Clients() {
		if c.Username == name {
			c.Close()
		}
	}
	s.Log.Info("user deleted", "admin", id.Username, "user", name)
	w.WriteHeader(http.StatusNoContent)
}

// userCertificate issues a client certificate and returns it as a .p12, for
// clients that are provisioned manually rather than by enrollment.
func (s *Server) userCertificate(w http.ResponseWriter, r *http.Request, id *identity) {
	name := r.PathValue("name")
	if _, ok := s.Store.User(name); !ok {
		jsonError(w, http.StatusNotFound, "user not found")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	if req.Password == "" {
		req.Password = s.Cfg.TruststorePassword
	}
	cert, key, err := s.CA.NewClient(name)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p12, err := s.CA.ClientP12(cert, key, req.Password)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Store.AddCert(store.Cert{Serial: pki.SerialHex(cert), CommonName: name, ClientUID: "admin-issued",
		Issued: time.Now().UTC(), Expires: cert.NotAfter.UTC()}); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("admin issued client certificate", "admin", id.Username, "user", name, "serial", pki.SerialHex(cert))
	w.Header().Set("Content-Type", "application/x-pkcs12")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.p12"`)
	w.Write(p12)
}

func (s *Server) revokeCert(w http.ResponseWriter, r *http.Request, id *identity) {
	serial := r.PathValue("serial")
	if err := s.Store.RevokeCert(serial); err != nil {
		jsonError(w, http.StatusNotFound, "certificate not found")
		return
	}
	for _, c := range s.Router.Clients() {
		if c.CertSerial == serial {
			c.Close()
		}
	}
	s.Log.Info("certificate revoked", "admin", id.Username, "serial", serial)
	w.WriteHeader(http.StatusNoContent)
}
