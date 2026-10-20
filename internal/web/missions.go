package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/edgetak/edgetak/internal/mission"
)

// missionRoutes serves the Data Sync API (/Marti/api/missions/...).
func (s *Server) missionRoutes(mux *http.ServeMux) {
	const p = "/Marti/api/missions"
	h := func(pattern string, fn func(w http.ResponseWriter, r *http.Request, c mission.Caller)) {
		mux.HandleFunc(pattern, s.authed(func(w http.ResponseWriter, r *http.Request, id *identity) {
			fn(w, r, missionCaller(r, id))
		}))
	}

	h("GET "+p, s.listMissions)
	h("GET "+p+"/{name}", s.getMission)
	h("PUT "+p+"/{name}", s.createMission)
	h("POST "+p+"/{name}", s.createMission)
	h("DELETE "+p+"/{name}", s.deleteMission)
	h("GET "+p+"/{name}/archive", s.missionArchive)
	h("GET "+p+"/{name}/cot", s.missionCoT)
	h("GET "+p+"/{name}/changes", s.missionChanges)
	h("PUT "+p+"/{name}/contents", s.addMissionContents)
	h("DELETE "+p+"/{name}/contents", s.removeMissionContent)
	h("PUT "+p+"/{name}/keywords", s.setMissionKeywords)
	h("DELETE "+p+"/{name}/keywords", s.clearMissionKeywords)
	h("DELETE "+p+"/{name}/keywords/{keyword}", s.removeMissionKeyword)

	h("GET "+p+"/{name}/subscription", s.getMissionSubscription)
	h("PUT "+p+"/{name}/subscription", s.subscribeMission)
	h("POST "+p+"/{name}/subscription", s.subscribeMission)
	h("DELETE "+p+"/{name}/subscription", s.unsubscribeMission)
	h("GET "+p+"/{name}/subscriptions", s.missionSubscribers(false))
	h("GET "+p+"/{name}/subscriptions/roles", s.missionSubscribers(true))
	h("GET "+p+"/all/subscriptions", s.allMissionSubscriptions)
	h("GET "+p+"/{name}/role", s.missionRole)
	h("PUT "+p+"/{name}/role", s.setMissionRole)
	h("PUT "+p+"/{name}/password", s.setMissionPassword)
	h("DELETE "+p+"/{name}/password", s.clearMissionPassword)

	h("GET "+p+"/all/invitations", s.missionInvitationNames)
	h("GET "+p+"/invitations", s.missionInvitations)
	h("PUT "+p+"/{name}/invite/{type}/{invitee}", s.inviteMission)
	h("DELETE "+p+"/{name}/invite/{type}/{invitee}", s.uninviteMission)

	h("POST "+p+"/logs/entries", s.addMissionLog)
	h("PUT "+p+"/logs/entries", s.updateMissionLog)
	h("DELETE "+p+"/logs/entries/{id}", s.deleteMissionLog)
	h("GET "+p+"/{name}/log", s.missionLogs)
	h("GET "+p+"/all/logs", s.allMissionLogs)

	// Newer Data Sync features EdgeTAK does not implement yet; empty answers
	// keep clients from treating the mission as broken.
	empty := func(w http.ResponseWriter, r *http.Request, c mission.Caller) {
		if _, err := s.Missions.Get(r.PathValue("name"), c, r.URL.Query().Get("password"), mission.View{}); err != nil {
			s.missionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.martiEnvelope("MissionLayer", []any{}))
	}
	h("GET "+p+"/{name}/layers", empty)

	// Latest CoT by UID (official CotApi), used to fetch announced mission items.
	h("GET /Marti/api/cot/xml/{uid}", s.cotXML(false))
	h("GET /Marti/api/cot/xml/{uid}/all", s.cotXML(true))
	h("GET /Marti/api/cot", s.cotEvents)
	h("POST /Marti/api/cot", s.cotEvents)
	mux.HandleFunc("GET /Marti/api/groups/groupCacheEnabled", s.authed(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.martiEnvelope("java.lang.Boolean", false))
	}))
}

const xmlHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`

// cotXML serves one item's latest event; wrapped in <events> for /all,
// since EdgeTAK keeps only the latest version of each item.
func (s *Server) cotXML(all bool) func(http.ResponseWriter, *http.Request, mission.Caller) {
	return func(w http.ResponseWriter, r *http.Request, c mission.Caller) {
		x, ok := s.Missions.LatestCoT(r.PathValue("uid"), c)
		if !ok {
			http.Error(w, "no CoT for uid", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		if all {
			io.WriteString(w, xmlHeader+"<events>"+x+"</events>")
			return
		}
		io.WriteString(w, xmlHeader+x)
	}
}

// cotEvents returns the latest event for each UID in the JSON body.
func (s *Server) cotEvents(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	var uids []string
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&uids); err != nil {
		http.Error(w, "body must be a JSON array of UIDs", http.StatusBadRequest)
		return
	}
	var b strings.Builder
	b.WriteString(xmlHeader + "<events>")
	for _, uid := range uids {
		if x, ok := s.Missions.LatestCoT(uid, c); ok {
			b.WriteString(x)
		}
	}
	b.WriteString("</events>")
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, b.String())
}

// missionCaller combines the HTTP identity with the device UID and mission
// token a Data Sync request carries.
func missionCaller(r *http.Request, id *identity) mission.Caller {
	q := r.URL.Query()
	token := r.Header.Get("MissionAuthorization")
	if token == "" {
		token = r.Header.Get("Authorization")
	}
	token, _ = strings.CutPrefix(token, "Bearer ")
	return mission.Caller{
		Username: id.Username, Groups: id.Groups, Admin: id.Admin, Anonymous: id.Anonymous,
		ClientUID: firstNonEmpty(q.Get("uid"), q.Get("clientUid"), q.Get("creatorUid")),
		Token:     strings.TrimSpace(token),
	}
}

func (s *Server) missionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mission.ErrNotFound):
		http.Error(w, "mission not found", http.StatusNotFound)
	case errors.Is(err, mission.ErrForbidden), errors.Is(err, mission.ErrBadPassword):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, mission.ErrExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, mission.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		s.Log.Error("mission request failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (s *Server) missionsResponse(w http.ResponseWriter, status int, data ...map[string]any) {
	if data == nil {
		data = []map[string]any{}
	}
	writeJSON(w, status, s.martiEnvelope("Mission", data))
}

// since interprets the secago / start query parameters.
func since(r *http.Request) time.Time {
	q := r.URL.Query()
	if v, err := strconv.ParseInt(q.Get("secago"), 10, 64); err == nil && v > 0 {
		return time.Now().Add(-time.Duration(v) * time.Second)
	}
	if t, err := time.Parse(time.RFC3339Nano, q.Get("start")); err == nil {
		return t
	}
	return time.Time{}
}

func boolParam(r *http.Request, name string) bool {
	v, _ := strconv.ParseBool(r.URL.Query().Get(name))
	return v
}

// ---- missions ----

func (s *Server) listMissions(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	s.missionsResponse(w, http.StatusOK, s.Missions.List(c, boolParam(r, "passwordProtected"), r.URL.Query().Get("tool"))...)
}

func (s *Server) getMission(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	v := mission.View{Changes: boolParam(r, "changes"), Logs: boolParam(r, "logs"), Since: since(r)}
	m, err := s.Missions.Get(r.PathValue("name"), c, r.URL.Query().Get("password"), v)
	if err != nil {
		s.missionError(w, err)
		return
	}
	s.missionsResponse(w, http.StatusOK, m)
}

func (s *Server) createMission(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	q := r.URL.Query()
	exp, _ := strconv.ParseInt(q.Get("expiration"), 10, 64)
	p := mission.CreateParams{
		Name: r.PathValue("name"), CreatorUID: q.Get("creatorUid"), Description: q.Get("description"),
		ChatRoom: q.Get("chatRoom"), BaseLayer: q.Get("baseLayer"), BBox: q.Get("bbox"), Path: q.Get("path"),
		Classification: q.Get("classification"), Tool: q.Get("tool"), Password: q.Get("password"),
		DefaultRole: q.Get("defaultRole"), Groups: q["group"], InviteOnly: boolParam(r, "inviteOnly"), Expiration: exp,
	}
	m, created, err := s.Missions.Create(p, c)
	if err != nil {
		s.missionError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.missionsResponse(w, status, m)
}

func (s *Server) deleteMission(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	m, err := s.Missions.Delete(r.PathValue("name"), c)
	if err != nil {
		s.missionError(w, err)
		return
	}
	s.missionsResponse(w, http.StatusOK, m)
}

func (s *Server) missionArchive(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	data, name, err := s.Missions.Archive(r.PathValue("name"), c)
	if err != nil {
		s.missionError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(data)
}

func (s *Server) missionCoT(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	x, err := s.Missions.CoT(r.PathValue("name"), c)
	if err != nil {
		s.missionError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, x)
}

func (s *Server) missionChanges(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	squashed := r.URL.Query().Get("squashed") != "false"
	ch, err := s.Missions.Changes(r.PathValue("name"), c, since(r), squashed)
	if err != nil {
		s.missionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.martiEnvelope("MissionChange", ch))
}

func (s *Server) addMissionContents(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	var body struct {
		Hashes []string `json:"hashes"`
		UIDs   []string `json:"uids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid content body", http.StatusBadRequest)
		return
	}
	m, err := s.Missions.AddContents(r.PathValue("name"), c, body.Hashes, body.UIDs)
	if err != nil {
		s.missionError(w, err)
		return
	}
	s.missionsResponse(w, http.StatusOK, m)
}

func (s *Server) removeMissionContent(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	q := r.URL.Query()
	m, err := s.Missions.RemoveContent(r.PathValue("name"), c, q.Get("hash"), q.Get("uid"))
	if err != nil {
		s.missionError(w, err)
		return
	}
	s.missionsResponse(w, http.StatusOK, m)
}

func (s *Server) setMissionKeywords(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	var kw []string
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&kw); err != nil {
		http.Error(w, "keywords must be a JSON array of strings", http.StatusBadRequest)
		return
	}
	m, err := s.Missions.SetKeywords(r.PathValue("name"), c, kw)
	if err != nil {
		s.missionError(w, err)
		return
	}
	s.missionsResponse(w, http.StatusOK, m)
}

func (s *Server) clearMissionKeywords(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	m, err := s.Missions.RemoveKeyword(r.PathValue("name"), c, "")
	if err != nil {
		s.missionError(w, err)
		return
	}
	s.missionsResponse(w, http.StatusOK, m)
}

func (s *Server) removeMissionKeyword(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	m, err := s.Missions.RemoveKeyword(r.PathValue("name"), c, r.PathValue("keyword"))
	if err != nil {
		s.missionError(w, err)
		return
	}
	s.missionsResponse(w, http.StatusOK, m)
}

// ---- subscriptions and roles ----

func (s *Server) subscribeMission(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	sub, err := s.Missions.Subscribe(r.PathValue("name"), c, r.URL.Query().Get("password"), since(r))
	if err != nil {
		s.missionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.martiEnvelope("com.bbn.marti.sync.model.MissionSubscription", sub))
}

func (s *Server) unsubscribeMission(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	if err := s.Missions.Unsubscribe(r.PathValue("name"), c); err != nil {
		s.missionError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getMissionSubscription(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	sub, err := s.Missions.Subscription(r.PathValue("name"), c, c.ClientUID)
	if err != nil {
		s.missionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.martiEnvelope("com.bbn.marti.sync.model.MissionSubscription", sub))
}

func (s *Server) missionSubscribers(withRoles bool) func(http.ResponseWriter, *http.Request, mission.Caller) {
	return func(w http.ResponseWriter, r *http.Request, c mission.Caller) {
		data, err := s.Missions.Subscribers(r.PathValue("name"), c, withRoles)
		if err != nil {
			s.missionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.martiEnvelope("MissionSubscription", data))
	}
}

func (s *Server) allMissionSubscriptions(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	writeJSON(w, http.StatusOK, s.martiEnvelope("MissionSubscription", s.Missions.AllSubscriptions(c)))
}

func (s *Server) missionRole(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	role, err := s.Missions.Role(r.PathValue("name"), c)
	if err != nil {
		s.missionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.martiEnvelope("MissionRole", role))
}

func (s *Server) setMissionRole(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	q := r.URL.Query()
	// clientUid here names the subscriber being changed, not the caller.
	c.ClientUID = q.Get("creatorUid")
	if err := s.Missions.SetRole(r.PathValue("name"), c, q.Get("clientUid"), q.Get("role")); err != nil {
		s.missionError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) setMissionPassword(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	if err := s.Missions.SetPassword(r.PathValue("name"), c, r.URL.Query().Get("password")); err != nil {
		s.missionError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) clearMissionPassword(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	if err := s.Missions.SetPassword(r.PathValue("name"), c, ""); err != nil {
		s.missionError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- invitations ----

func (s *Server) missionInvitationNames(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	names := []string{}
	for _, inv := range s.Missions.Invitations(c) {
		names = append(names, inv["missionName"].(string))
	}
	writeJSON(w, http.StatusOK, s.martiEnvelope("MissionInvitation", names))
}

func (s *Server) missionInvitations(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	writeJSON(w, http.StatusOK, s.martiEnvelope("MissionInvitation", s.Missions.Invitations(c)))
}

func (s *Server) inviteMission(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	if err := s.Missions.Invite(r.PathValue("name"), c, r.PathValue("type"), r.PathValue("invitee"), r.URL.Query().Get("role")); err != nil {
		s.missionError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) uninviteMission(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	if err := s.Missions.Uninvite(r.PathValue("name"), c, r.PathValue("type"), r.PathValue("invitee")); err != nil {
		s.missionError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- logs ----

// logInput accepts log entries with loosely formatted timestamps.
type logInput struct {
	ID            string   `json:"id"`
	Content       string   `json:"content"`
	CreatorUID    string   `json:"creatorUid"`
	EntryUID      string   `json:"entryUid"`
	MissionNames  []string `json:"missionNames"`
	DTG           string   `json:"dtg"`
	Created       string   `json:"created"`
	ContentHashes []string `json:"contentHashes"`
	Keywords      []string `json:"keywords"`
}

func (in logInput) entry() mission.LogEntry {
	parse := func(v string) time.Time { t, _ := time.Parse(time.RFC3339Nano, v); return t.UTC() }
	return mission.LogEntry{
		ID: in.ID, Content: in.Content, CreatorUID: in.CreatorUID, EntryUID: in.EntryUID,
		MissionNames: in.MissionNames, DTG: parse(in.DTG), Created: parse(in.Created),
		ContentHashes: in.ContentHashes, Keywords: in.Keywords,
	}
}

func decodeLog(w http.ResponseWriter, r *http.Request) (mission.LogEntry, bool) {
	var in logInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, "invalid log entry", http.StatusBadRequest)
		return mission.LogEntry{}, false
	}
	return in.entry(), true
}

func (s *Server) addMissionLog(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	e, ok := decodeLog(w, r)
	if !ok {
		return
	}
	if c.ClientUID == "" {
		c.ClientUID = e.CreatorUID
	}
	saved, err := s.Missions.AddLog(e, c)
	if err != nil {
		s.missionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.martiEnvelope("com.bbn.marti.sync.model.LogEntry", mission.LogJSON(saved)))
}

func (s *Server) updateMissionLog(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	e, ok := decodeLog(w, r)
	if !ok {
		return
	}
	if c.ClientUID == "" {
		c.ClientUID = e.CreatorUID
	}
	saved, err := s.Missions.UpdateLog(e, c)
	if err != nil {
		s.missionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.martiEnvelope("com.bbn.marti.sync.model.LogEntry", mission.LogJSON(saved)))
}

func (s *Server) deleteMissionLog(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	if err := s.Missions.DeleteLog(r.PathValue("id"), c); err != nil {
		s.missionError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) missionLogs(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	logs, err := s.Missions.Logs(r.PathValue("name"), c, since(r))
	if err != nil {
		s.missionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.martiEnvelope("com.bbn.marti.sync.model.LogEntry", mission.LogsJSON(logs)))
}

func (s *Server) allMissionLogs(w http.ResponseWriter, r *http.Request, c mission.Caller) {
	writeJSON(w, http.StatusOK, s.martiEnvelope("com.bbn.marti.sync.model.LogEntry", mission.LogsJSON(s.Missions.AllLogs(c))))
}
