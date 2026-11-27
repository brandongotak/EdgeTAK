package web

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/brandongotak/EdgeTAK/internal/mission"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Mission templates (Data → Missions): a mission's settings, files and map
// items saved to make new missions from, for operations that start the
// same way each time (a search pattern, rally points, reference files).

type missionTemplateView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Groups      []string  `json:"groups"`
	DefaultRole string    `json:"default_role"`
	InviteOnly  bool      `json:"invite_only"`
	Files       int       `json:"files"`
	Missing     int       `json:"missing_files"` // data packages deleted since
	Items       int       `json:"items"`
	From        string    `json:"from"`
	Created     time.Time `json:"created"`
	CreatedBy   string    `json:"created_by"`
}

func (s *Server) missionTemplateViews() []missionTemplateView {
	out := []missionTemplateView{}
	for _, t := range s.Store.MissionTemplates() {
		v := missionTemplateView{ID: t.ID, Name: t.Name, Description: t.Description, Groups: t.Groups, DefaultRole: t.DefaultRole,
			InviteOnly: t.InviteOnly, Files: len(t.Files), Items: len(t.Items), From: t.From, Created: t.Created, CreatedBy: t.CreatedBy}
		if v.Groups == nil {
			v.Groups = []string{}
		}
		for _, h := range t.Files {
			if _, ok := s.Store.Package(h); !ok {
				v.Missing++
			}
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b missionTemplateView) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
}

// templateFiles are the data packages templates use, so retention keeps them.
func (s *Server) templateFiles() map[string]bool {
	out := map[string]bool{}
	for _, t := range s.Store.MissionTemplates() {
		for _, h := range t.Files {
			out[h] = true
		}
	}
	return out
}

func (s *Server) missionTemplateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/mission-templates", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.missionTemplateViews())
	}))
	// Save a mission as a template: {mission, name}.
	mux.HandleFunc("POST /admin/api/mission-templates", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			Mission string `json:"mission"`
			Name    string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || utf8.RuneCountInString(req.Name) > 80 {
			jsonError(w, http.StatusBadRequest, "give the template a name of up to 80 characters")
			return
		}
		if len(s.Store.MissionTemplates()) >= 100 {
			jsonError(w, http.StatusBadRequest, "this server has 100 mission templates; delete some first")
			return
		}
		snap, err := s.Missions.TakeSnapshot(req.Mission, mission.Caller{Username: id.Username, Admin: true})
		if err != nil {
			s.missionError(w, err)
			return
		}
		now := time.Now().UTC()
		t := store.MissionTemplate{ID: uuid(), Name: req.Name, Description: snap.Description, Groups: snap.Groups, DefaultRole: snap.DefaultRole,
			InviteOnly: snap.InviteOnly, Tool: snap.Tool, Keywords: snap.Keywords, ChatRoom: snap.ChatRoom, BaseLayer: snap.BaseLayer,
			BBox: snap.BBox, Files: snap.Files, Items: snap.Items, From: req.Mission, Created: now, CreatedBy: id.Username, Updated: now}
		if err := s.Store.PutMissionTemplate(t); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("mission template saved", "admin", id.Username, "template", t.Name, "from", req.Mission, "files", len(t.Files), "items", len(t.Items))
		writeJSON(w, http.StatusCreated, s.missionTemplateViews())
	}))
	mux.HandleFunc("PUT /admin/api/mission-templates/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Groups      []string `json:"groups"`
			DefaultRole string   `json:"default_role"`
			InviteOnly  bool     `json:"invite_only"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		req.Name, req.Description = strings.TrimSpace(req.Name), strings.TrimSpace(req.Description)
		if req.Name == "" || utf8.RuneCountInString(req.Name) > 80 || utf8.RuneCountInString(req.Description) > 500 {
			jsonError(w, http.StatusBadRequest, "give the template a name of up to 80 characters (description up to 500)")
			return
		}
		if !mission.ValidRole(req.DefaultRole) {
			jsonError(w, http.StatusBadRequest, "unknown role")
			return
		}
		t, ok := s.Store.MissionTemplate(r.PathValue("id"))
		if !ok {
			jsonError(w, http.StatusNotFound, "no such template")
			return
		}
		t.Name, t.Description, t.Groups, t.DefaultRole, t.InviteOnly = req.Name, req.Description, cleanGroups(req.Groups), req.DefaultRole, req.InviteOnly
		t.Updated = time.Now().UTC()
		if err := s.Store.PutMissionTemplate(t); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("mission template changed", "admin", id.Username, "template", t.Name)
		writeJSON(w, http.StatusOK, s.missionTemplateViews())
	}))
	mux.HandleFunc("DELETE /admin/api/mission-templates/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if err := s.Store.DeleteMissionTemplate(r.PathValue("id")); err != nil {
			jsonError(w, http.StatusNotFound, "no such template")
			return
		}
		s.Log.Info("mission template deleted", "admin", id.Username, "template", r.PathValue("id"))
		writeJSON(w, http.StatusOK, s.missionTemplateViews())
	}))
	mux.HandleFunc("POST /admin/api/mission-templates/{id}/create", s.admin(s.missionFromTemplate))
}

// missionFromTemplate makes a mission: {name, password, invite}. With
// invite, connected devices in its groups are invited to it.
func (s *Server) missionFromTemplate(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Invite   bool   `json:"invite"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	t, ok := s.Store.MissionTemplate(r.PathValue("id"))
	if !ok {
		jsonError(w, http.StatusNotFound, "no such template")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 256 || strings.ContainsAny(req.Name, "/\\") {
		jsonError(w, http.StatusBadRequest, "give the mission a name (no slashes)")
		return
	}
	if s.Missions.Exists(req.Name) {
		jsonError(w, http.StatusConflict, "a mission with this name exists")
		return
	}
	c := mission.Caller{Username: id.Username, Admin: true, Groups: t.Groups, OutGroups: t.Groups}
	p := mission.CreateParams{Name: req.Name, Description: t.Description, Groups: t.Groups, DefaultRole: t.DefaultRole, InviteOnly: t.InviteOnly,
		Tool: t.Tool, ChatRoom: t.ChatRoom, BaseLayer: t.BaseLayer, BBox: t.BBox, Password: req.Password}
	if _, _, err := s.Missions.Create(p, c); err != nil {
		s.missionError(w, err)
		return
	}
	if len(t.Keywords) > 0 {
		s.Missions.SetKeywords(req.Name, c, t.Keywords)
	}
	var files []string
	for _, h := range t.Files {
		if _, ok := s.Store.Package(h); ok {
			files = append(files, h)
		}
	}
	if len(files) > 0 {
		if _, err := s.Missions.AddContents(req.Name, c, files, nil); err != nil {
			s.Log.Warn("template files not added", "mission", req.Name, "err", err)
		}
	}
	items, err := s.Missions.AddTemplateItems(req.Name, c, t.Items)
	if err != nil {
		s.Log.Warn("template items not added", "mission", req.Name, "err", err)
	}
	invited := 0
	if req.Invite {
		for _, cl := range s.Router.Clients() {
			uid, _ := cl.Identity()
			if uid == "" || !(len(t.Groups) == 0 || slices.ContainsFunc(deviceGroups(cl), func(g string) bool { return slices.Contains(t.Groups, g) })) {
				continue
			}
			if err := s.Missions.Invite(req.Name, c, "clientUid", uid, ""); err == nil {
				invited++
			}
		}
	}
	s.Log.Info("mission made from template", "admin", id.Username, "template", t.Name, "mission", req.Name,
		"files", len(files), "items", items, "invited", invited)
	writeJSON(w, http.StatusCreated, map[string]any{"mission": req.Name, "files": len(files), "missing_files": len(t.Files) - len(files),
		"items": items, "invited": invited})
}
