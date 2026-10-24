package web

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
)

// Remote access (Settings → Remote access): EdgeTAK runs the FRP client,
// which connects out to an FRP server that forwards its ports back here;
// see internal/frp.

func (s *Server) frpView() map[string]any {
	st := s.FRP.Status()
	v := map[string]any{"status": st, "server_addr": s.FRP.ServerAddr(), "remote_ports": s.FRP.RemotePorts(),
		"ports":       map[string]string{"tls": port(s.Addrs["tls"]), "https": port(s.Addrs["https"]), "enroll": port(s.Addrs["enroll"])},
		"server_name": s.Cfg.ServerName}
	// The certificate should name the FRP server: devices connect to it.
	if addr := s.FRP.ServerAddr(); addr != "" {
		covered := false
		if leaf := s.CA.ServerCert.Leaf; leaf != nil {
			if ip := net.ParseIP(addr); ip != nil {
				covered = slices.ContainsFunc(leaf.IPAddresses, ip.Equal)
			} else {
				covered = slices.Contains(leaf.DNSNames, addr)
			}
		}
		v["cert_missing"] = !covered
	}
	return v
}

func (s *Server) frpRoutes(mux *http.ServeMux) {
	if s.FRP == nil {
		return
	}
	mux.HandleFunc("GET /admin/api/frp", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.frpView())
	}))
	// Save a new frpc.toml (the body, as text). It's never sent back: it
	// holds the FRP server's token.
	mux.HandleFunc("PUT /admin/api/frp/config", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			jsonError(w, http.StatusBadRequest, "the config is too large")
			return
		}
		var req struct {
			Config string `json:"config"`
		}
		if json.Unmarshal(b, &req) != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if err := s.FRP.SaveConfig(r.Context(), req.Config); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.Log.Info("FRP config saved", "admin", id.Username, "server", s.FRP.ServerAddr())
		writeJSON(w, http.StatusOK, s.frpView())
	}))
	mux.HandleFunc("DELETE /admin/api/frp/config", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if err := s.FRP.DeleteConfig(); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Store.SetFRPEnabled(false)
		writeJSON(w, http.StatusOK, s.frpView())
	}))
	mux.HandleFunc("PUT /admin/api/frp/enabled", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			On bool `json:"on"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req) != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if req.On && s.FRP.ServerAddr() == "" {
			jsonError(w, http.StatusBadRequest, "save an frpc config first")
			return
		}
		if err := s.Store.SetFRPEnabled(req.On); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.FRP.SetEnabled(req.On)
		s.Log.Warn("FRP remote access "+map[bool]string{true: "connected", false: "disconnected"}[req.On], "admin", id.Username,
			"server", s.FRP.ServerAddr())
		writeJSON(w, http.StatusOK, s.frpView())
	}))
}
