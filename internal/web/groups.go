package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Group management for the admin console. Memberships have a direction like
// the official server's: IN (receive what the group shares), OUT (send to the
// group) or both. Every change is pushed to connected clients immediately.

var groupNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9 ._@()-]{0,63}$`)

type groupView struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Created     string         `json:"created"`
	Builtin     bool           `json:"builtin"` // the anonymous group
	Members     []store.Member `json:"members"`
	Online      int            `json:"online"` // connected clients that receive this group
}

func (s *Server) groupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/groups", s.admin(s.listGroups))
	mux.HandleFunc("POST /admin/api/groups", s.admin(s.createGroup))
	mux.HandleFunc("PATCH /admin/api/groups/{name}", s.admin(s.updateGroup))
	mux.HandleFunc("DELETE /admin/api/groups/{name}", s.admin(s.deleteGroup))
	mux.HandleFunc("PUT /admin/api/groups/{name}/members/{user}", s.admin(s.setMember))
	mux.HandleFunc("DELETE /admin/api/groups/{name}/members/{user}", s.admin(s.removeMember))
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request, _ *identity) {
	online := map[string]int{}
	for _, c := range s.Router.Clients() {
		for _, g := range c.InGroups() {
			online[g]++
		}
	}
	out := []groupView{}
	for _, g := range s.Store.Groups() {
		out = append(out, groupView{
			Name: g.Name, Description: g.Description, Created: g.Created.Format("2006-01-02"),
			Builtin: g.Name == s.Cfg.AnonymousGroup, Members: g.Members, Online: online[g.Name],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct{ Name, Description string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !groupNameRE.MatchString(req.Name) || strings.Contains(req.Name, ",") {
		jsonError(w, http.StatusBadRequest, "group names are 1-64 characters: letters, digits, spaces and . _ @ ( ) -")
		return
	}
	if _, err := s.Store.CreateGroup(req.Name, strings.TrimSpace(req.Description)); errors.Is(err, store.ErrExists) {
		jsonError(w, http.StatusConflict, "group already exists")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("group created", "admin", id.Username, "group", req.Name)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) updateGroup(w http.ResponseWriter, r *http.Request, id *identity) {
	name := r.PathValue("name")
	g, ok := s.Store.Group(name)
	if !ok {
		jsonError(w, http.StatusNotFound, "group not found")
		return
	}
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	newName, desc := "", g.Description
	if req.Description != nil {
		desc = strings.TrimSpace(*req.Description)
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) != name {
		newName = strings.TrimSpace(*req.Name)
		if name == s.Cfg.AnonymousGroup {
			jsonError(w, http.StatusBadRequest, "the anonymous group cannot be renamed (see anonymous_group in the config)")
			return
		}
		if !groupNameRE.MatchString(newName) || strings.Contains(newName, ",") {
			jsonError(w, http.StatusBadRequest, "invalid group name")
			return
		}
	}
	affected, err := s.Store.UpdateGroup(name, newName, desc)
	if errors.Is(err, store.ErrExists) {
		jsonError(w, http.StatusConflict, "a group with that name already exists")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if newName != "" {
		s.Missions.RenameGroup(name, newName)
		for _, u := range affected {
			s.applyUserGroups(u)
		}
		s.applyExtensionGroups()
		s.applyMesh()
		s.Log.Info("group renamed", "admin", id.Username, "from", name, "to", newName)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request, id *identity) {
	name := r.PathValue("name")
	if name == s.Cfg.AnonymousGroup {
		jsonError(w, http.StatusBadRequest, "the anonymous group cannot be deleted")
		return
	}
	affected, err := s.Store.DeleteGroup(name)
	if err != nil {
		jsonError(w, http.StatusNotFound, "group not found")
		return
	}
	for _, u := range affected {
		s.applyUserGroups(u)
	}
	s.applyExtensionGroups()
	s.applyMesh()
	s.Log.Info("group deleted", "admin", id.Username, "group", name, "members_removed", len(affected))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setMember(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct{ Direction string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	dir := strings.ToLower(req.Direction)
	if dir == "" {
		dir = "both"
	}
	if dir != "both" && dir != "in" && dir != "out" {
		jsonError(w, http.StatusBadRequest, "direction must be both, in or out")
		return
	}
	s.membership(w, r, id, dir)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, id *identity) {
	s.membership(w, r, id, "")
}

func (s *Server) membership(w http.ResponseWriter, r *http.Request, id *identity, dir string) {
	group, user := r.PathValue("name"), r.PathValue("user")
	if err := s.Store.SetMembership(group, user, dir); err != nil {
		jsonError(w, http.StatusNotFound, "group or user not found")
		return
	}
	s.applyUserGroups(user)
	s.Log.Info("group membership changed", "admin", id.Username, "group", group, "user", user, "direction", firstNonEmpty(dir, "removed"))
	w.WriteHeader(http.StatusNoContent)
}
