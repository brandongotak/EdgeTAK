package web

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/federation"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Federation tab: TAK Server Federation v2 with TAK Servers and other
// EdgeTAKs (see internal/federation).

var fingerprintRE = regexp.MustCompile(`^[0-9A-F]{64}$`)

type federateView struct {
	store.Federate
	Token    string                    `json:"token,omitempty"` // never sent
	HasToken bool                      `json:"has_token"`
	Status   *federation.SessionStatus `json:"status,omitempty"`
}

func (s *Server) federationStatus() map[string]any {
	settings := s.Store.FederationSettings()
	st := s.Federation.Status()
	feds := []federateView{}
	for _, f := range settings.Federates {
		v := federateView{Federate: f, HasToken: f.Token != ""}
		v.Federate.Token = ""
		if ss, ok := st.Federates[f.ID]; ok {
			v.Status = &ss
		}
		feds = append(feds, v)
	}
	cert := s.CA.ServerCert.Leaf
	identity := map[string]any{"server_name": s.Cfg.ServerName, "node_id": s.NodeID,
		"ca_subject": s.CA.Cert.Subject.String(), "ca_fingerprint": federation.Fingerprint(s.CA.Cert.Raw)}
	if cert != nil {
		identity["cert_subject"], identity["cert_fingerprint"] = cert.Subject.String(), federation.Fingerprint(cert.Raw)
	}
	return map[string]any{"identity": identity, "listener": st.Listener, "federates": feds, "pending": st.Pending,
		"contacts": st.Contacts, "public_host": s.Cfg.PublicHost}
}

// validFederate checks and normalizes a federate; old is the saved one when
// editing (its token is kept when none is sent).
func validFederate(f store.Federate, old *store.Federate) (store.Federate, error) {
	f.Name = truncate(strings.TrimSpace(f.Name), 100)
	if f.Name == "" {
		return f, errors.New("a name is required")
	}
	f.Address, f.TLSServerName = strings.TrimSpace(f.Address), strings.TrimSpace(f.TLSServerName)
	f.CAPEM, f.Token, f.ClientCN = strings.TrimSpace(f.CAPEM), strings.TrimSpace(f.Token), strings.TrimSpace(f.ClientCN)
	f.OutboundGroups, f.InboundGroups = cleanGroups(f.OutboundGroups), cleanGroups(f.InboundGroups)
	if f.SendMode != "groups" {
		f.SendMode = "all"
	} else if len(f.OutboundGroups) == 0 {
		return f, errors.New("choose the groups to share, or share everything")
	}
	caOK := func() bool {
		pool := x509.NewCertPool()
		return strings.Contains(f.CAPEM, "BEGIN CERTIFICATE") && pool.AppendCertsFromPEM([]byte(f.CAPEM))
	}
	switch f.Direction {
	case federation.Outbound:
		f.Fingerprint, f.ClientCN = "", ""
		if f.Address == "" {
			return f, errors.New("enter the other server's address")
		}
		if f.AuthMode != "token" {
			f.AuthMode, f.UseTLS, f.Token, f.TokenExpiration = "certificate", true, "", 0
		} else if f.Token == "" && old != nil {
			f.Token = old.Token
		}
		if f.AuthMode == "token" && f.Token == "" {
			return f, errors.New("a federation token is needed: paste one, or get one with a TAK Server admin login")
		}
		if f.Port == 0 {
			f.Port = 9001
			if f.AuthMode == "token" {
				f.Port = 9002
			}
		}
		if f.Port < 1 || f.Port > 65535 {
			return f, errors.New("the port must be 1 to 65535")
		}
		if f.UseTLS && !caOK() {
			return f, errors.New("the other server's CA certificate is needed (PEM): get it from the server, or paste it")
		}
	case federation.Inbound:
		f.Address, f.Port, f.AuthMode, f.Token, f.TokenExpiration, f.TLSServerName, f.UseTLS = "", 0, "", "", 0, "", true
		f.Fingerprint = strings.Map(func(r rune) rune {
			if strings.ContainsRune("0123456789ABCDEF", r) {
				return r
			}
			return -1
		}, strings.ToUpper(f.Fingerprint))
		if f.Fingerprint != "" && !fingerprintRE.MatchString(f.Fingerprint) {
			return f, errors.New("the fingerprint must be a SHA-256 certificate fingerprint (64 hex digits)")
		}
		if f.Fingerprint == "" && (f.ClientCN == "" || !caOK()) {
			return f, errors.New("an inbound federate needs its certificate's fingerprint, or its common name and CA certificate")
		}
		if f.CAPEM != "" && !caOK() {
			return f, errors.New("the CA certificate is not valid PEM")
		}
	default:
		return f, errors.New("direction must be outbound or inbound")
	}
	return f, nil
}

func (s *Server) federationRoutes(mux *http.ServeMux) {
	if s.Federation == nil {
		return
	}
	mux.HandleFunc("GET /admin/api/federation", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.federationStatus())
	}))
	mux.HandleFunc("PUT /admin/api/federation/listener", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			Enabled bool `json:"enabled"`
			Port    int  `json:"port"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || req.Port < 0 || req.Port > 65535 {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if req.Port == 0 {
			req.Port = 9001
		}
		if err := s.Store.SetFederationListener(req.Enabled, req.Port); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Federation.Reload()
		s.Log.Info("federation listener changed", "admin", id.Username, "enabled", req.Enabled, "port", req.Port)
		time.Sleep(200 * time.Millisecond) // let it bind, so the status is current
		writeJSON(w, http.StatusOK, s.federationStatus())
	}))
	mux.HandleFunc("POST /admin/api/federation/federates", s.admin(s.saveFederate))
	mux.HandleFunc("PUT /admin/api/federation/federates/{id}", s.admin(s.saveFederate))
	mux.HandleFunc("DELETE /admin/api/federation/federates/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if err := s.Store.DeleteFederate(r.PathValue("id")); err != nil {
			jsonError(w, http.StatusNotFound, "federate not found")
			return
		}
		s.Federation.Reload()
		s.Log.Info("federate deleted", "admin", id.Username, "id", r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	}))
	// Approve a server that tried to connect: it becomes an inbound federate
	// pinned to its certificate's fingerprint.
	mux.HandleFunc("POST /admin/api/federation/pending/{fp}/approve", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req store.Federate
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		p, ok := s.Federation.TakePending(r.PathValue("fp"))
		if !ok {
			jsonError(w, http.StatusNotFound, "that server is no longer waiting; it may have stopped trying")
			return
		}
		req.ID, req.Direction, req.Enabled, req.Fingerprint, req.Created = uuid(), federation.Inbound, true, p.Fingerprint, time.Now().UTC()
		if req.Name == "" {
			req.Name = firstNonEmpty(p.CommonName, "Federate")
		}
		f, err := validFederate(req, nil)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.Store.PutFederate(f); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Federation.Reload()
		s.Log.Info("federate approved", "admin", id.Username, "name", f.Name, "fingerprint", f.Fingerprint)
		writeJSON(w, http.StatusOK, s.federationStatus())
	}))
	mux.HandleFunc("DELETE /admin/api/federation/pending/{fp}", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		s.Federation.TakePending(r.PathValue("fp"))
		w.WriteHeader(http.StatusNoContent)
	}))
	// The CA a server presents, to trust it (the admin checks the fingerprint).
	mux.HandleFunc("POST /admin/api/federation/fetch-ca", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		var req struct {
			Address string `json:"address"`
			Ports   []int  `json:"ports"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || strings.TrimSpace(req.Address) == "" {
			jsonError(w, http.StatusBadRequest, "enter the server's address")
			return
		}
		res, err := federation.FetchCAs(s.dialer(), strings.TrimSpace(req.Address), req.Ports)
		if err != nil {
			jsonError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	}))
	mux.HandleFunc("POST /admin/api/federation/tak/token", s.admin(s.takToken))
	mux.HandleFunc("POST /admin/api/federation/tak/trust", s.admin(s.takTrust))
}

func (s *Server) dialer() func(string) (net.Conn, error) {
	if s.FederationDial != nil {
		return s.FederationDial
	}
	return func(addr string) (net.Conn, error) { return net.DialTimeout("tcp", addr, 10*time.Second) }
}

func (s *Server) saveFederate(w http.ResponseWriter, r *http.Request, id *identity) {
	var req store.Federate
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	var old *store.Federate
	if fid := r.PathValue("id"); fid != "" {
		for _, f := range s.Store.FederationSettings().Federates {
			if f.ID == fid {
				f := f
				old = &f
			}
		}
		if old == nil {
			jsonError(w, http.StatusNotFound, "federate not found")
			return
		}
		req.ID, req.Created = old.ID, old.Created
	} else {
		req.ID, req.Created = uuid(), time.Now().UTC()
	}
	f, err := validFederate(req, old)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.PutFederate(f); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Federation.Reload()
	s.Log.Info("federate saved", "admin", id.Username, "name", f.Name, "direction", f.Direction, "enabled", f.Enabled)
	writeJSON(w, http.StatusOK, s.federationStatus())
}

type takLogin struct {
	Address  string   `json:"address"`
	Port     int      `json:"port"`
	CAPEM    string   `json:"ca_pem"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	Name     string   `json:"name"`
	Days     int      `json:"days"`
	Groups   []string `json:"groups"`
}

func (s *Server) takAdmin(w http.ResponseWriter, r *http.Request) (*federation.TAKAdmin, takLogin, bool) {
	var req takLogin
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.Address == "" || req.Username == "" {
		jsonError(w, http.StatusBadRequest, "the TAK Server's address and an admin username and password are needed")
		return nil, req, false
	}
	if req.Port == 0 {
		req.Port = 8446
	}
	t := &federation.TAKAdmin{Address: strings.TrimSpace(req.Address), Port: req.Port, CAPEM: req.CAPEM, Username: req.Username,
		Password: req.Password, Dial: s.FederationDial}
	if err := t.SignIn(); err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return nil, req, false
	}
	return t, req, true
}

// takToken generates a federation token with a TAK Server admin login.
func (s *Server) takToken(w http.ResponseWriter, r *http.Request, id *identity) {
	t, req, ok := s.takAdmin(w, r)
	if !ok {
		return
	}
	token, exp, err := t.GenerateToken(firstNonEmpty(req.Name, s.Cfg.ServerName), req.Days)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.Log.Info("federation token generated on a TAK Server", "admin", id.Username, "server", req.Address)
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expiration": exp})
}

// takTrust makes a TAK Server trust EdgeTAK's CA, with groups.
func (s *Server) takTrust(w http.ResponseWriter, r *http.Request, id *identity) {
	t, req, ok := s.takAdmin(w, r)
	if !ok {
		return
	}
	res, err := t.TrustEdgeTAK(s.CA.Cert.Raw, s.CA.ServerCert.Certificate[0], cleanGroups(req.Groups))
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.Log.Info("TAK Server set up to trust EdgeTAK", "admin", id.Username, "server", req.Address, "uploaded", res.Uploaded, "groups", res.Groups)
	writeJSON(w, http.StatusOK, res)
}
