package web

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/brandongotak/EdgeTAK/internal/mesh"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// The mesh bridge (Settings → Mesh bridge): ATAK's multicast network on
// one side, an EdgeTAK group on the other; see internal/mesh.

func (s *Server) meshView() map[string]any {
	v := map[string]any{"settings": s.Store.Mesh().Normalized(s.Cfg.AnonymousGroup), "available": mesh.Interfaces()}
	if s.Mesh != nil {
		v["status"] = s.Mesh.Status()
	}
	return v
}

func (s *Server) meshRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/mesh", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.meshView())
	}))
	mux.HandleFunc("PUT /admin/api/mesh", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req store.MeshSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if req.Group == "" {
			req.Group = s.Cfg.AnonymousGroup
		}
		for _, g := range append([]string{req.Group}, req.ToMesh...) {
			if _, ok := s.Store.Group(g); !ok && g != s.Cfg.AnonymousGroup {
				jsonError(w, http.StatusBadRequest, "no such group: "+g)
				return
			}
		}
		if req.ToMesh == nil {
			req.ToMesh = []string{}
		}
		for _, a := range []*string{&req.SAAddr, &req.ChatAddr} {
			if *a = strings.TrimSpace(*a); *a == "" {
				continue
			}
			addr, err := net.ResolveUDPAddr("udp4", *a)
			if err != nil || !addr.IP.IsMulticast() || addr.Port == 0 {
				jsonError(w, http.StatusBadRequest, *a+" is not a multicast address and port, e.g. 239.2.3.1:6969")
				return
			}
		}
		switch {
		case req.PositionSeconds < 0 || req.PositionSeconds > 3600:
			jsonError(w, http.StatusBadRequest, "positions can be sent every 1 to 3600 seconds")
			return
		case req.TTL < 0 || req.TTL > 255:
			jsonError(w, http.StatusBadRequest, "the TTL is 1 to 255")
			return
		case req.DirectPort < -1 || req.DirectPort > 65535:
			jsonError(w, http.StatusBadRequest, "the direct message port is 1 to 65535, or -1 for none")
			return
		}
		if err := s.Store.SetMesh(req); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if s.Mesh != nil {
			s.Mesh.Apply(s.Store.Mesh())
		}
		s.Log.Info("mesh bridge settings changed", "admin", id.Username, "enabled", req.Enabled, "group", req.Group, "to_mesh", req.ToMesh)
		writeJSON(w, http.StatusOK, s.meshView())
	}))
}

// applyMesh restarts the bridge after a group it uses was renamed or deleted.
func (s *Server) applyMesh() {
	if s.Mesh != nil {
		s.Mesh.Apply(s.Store.Mesh())
	}
}
