package web

import (
	"encoding/json"
	"net/http"
)

// Plain TCP (Settings → Plain TCP): the unencrypted, anonymous streaming
// port (8087), for local networks. Admins can turn it off and on without
// a restart, also on servers installed without it (--no-plaintext); the
// choice is kept.

func (s *Server) plainTCPOn() bool { return s.PlainTCP != nil && s.PlainTCP.On() }

// tcpAddr is the plain TCP port's address.
func (s *Server) tcpAddr() string {
	if s.PlainTCP != nil {
		return s.PlainTCP.Addr()
	}
	return s.Addrs["tcp"]
}

func (s *Server) plainTCPView() map[string]any {
	return map[string]any{"configured": s.PlainTCP != nil, "on": s.plainTCPOn(), "port": port(s.tcpAddr())}
}

func (s *Server) plainTCPRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/plain-tcp", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.plainTCPView())
	}))
	mux.HandleFunc("PUT /admin/api/plain-tcp", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			On bool `json:"on"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if s.PlainTCP == nil {
			jsonError(w, http.StatusBadRequest, "plain TCP is turned off in the server's configuration (tcp_addr, or installed with --no-plaintext)")
			return
		}
		if err := s.PlainTCP.Set(req.On); err != nil {
			jsonError(w, http.StatusInternalServerError, "could not open the port: "+err.Error())
			return
		}
		if err := s.Store.SetPlainTCP(req.On); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Warn("plain TCP turned "+map[bool]string{true: "on", false: "off"}[req.On], "admin", id.Username, "addr", s.tcpAddr())
		writeJSON(w, http.StatusOK, s.plainTCPView())
	}))
}
