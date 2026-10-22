package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/mission"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Events (People -> Events): temporary operations with their own group and
// Data Sync mission. People join by scanning a QR code: in "person" mode it
// opens the sign-up page with the event's invite, so each person gets their
// own account; in "shared" mode it is an ATAK enrollment link for one
// account everyone uses. Accounts are tagged with the event. When the event
// expires (or an admin ends it), its accounts are erased (their
// certificates revoked, their devices disconnected), its group and invite
// removed; its mission is kept for review.

const eventCheckEvery = 30 * time.Second

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// eventSlug is a username-friendly form of the event name.
func eventSlug(name string) string {
	s := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "event"
	}
	return s
}

type eventView struct {
	store.Event
	Joined     int    `json:"joined"` // accounts now
	Online     int    `json:"online"` // their connected devices
	JoinURL    string `json:"join_url,omitempty"`
	EnrollLink string `json:"enroll_link,omitempty"`
	Host       string `json:"host"`
}

func (s *Server) eventViews() []eventView {
	counts := map[string]int{}
	users := map[string]string{} // username -> event
	for _, u := range s.Store.Users() {
		if u.Event != "" {
			counts[u.Event]++
			users[u.Username] = u.Event
		}
	}
	online := map[string]int{}
	for _, c := range s.Router.Clients() {
		if ev := users[c.Username]; ev != "" {
			online[ev]++
		}
	}
	out := []eventView{}
	for _, e := range s.Store.Events() {
		v := eventView{Event: e, Joined: counts[e.ID], Online: online[e.ID], Host: s.Cfg.PublicHost}
		if e.Ended.IsZero() {
			switch e.Mode {
			case "person":
				v.JoinURL = s.registerURL(e.Invite)
			case "shared":
				v.EnrollLink = "tak://com.atakmap.app/enroll?host=" + url.QueryEscape(s.Cfg.PublicHost) +
					"&username=" + url.QueryEscape(e.Account) + "&token=" + url.QueryEscape(e.Password)
			}
		}
		if !e.Ended.IsZero() || e.Mode != "shared" {
			v.Password = "" // only an active shared event needs it, for its QR code
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) eventRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/events", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.eventViews())
	}))
	mux.HandleFunc("POST /admin/api/events", s.admin(s.createEvent))
	// Change when it ends (extend or shorten) and how many may join.
	mux.HandleFunc("PATCH /admin/api/events/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		e, ok := s.Store.Event(r.PathValue("id"))
		if !ok || !e.Ended.IsZero() {
			jsonError(w, http.StatusNotFound, "no such active event")
			return
		}
		var req struct {
			Expires   *time.Time `json:"expires"`
			MaxPeople *int       `json:"max_people"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if req.Expires != nil {
			if !req.Expires.After(time.Now()) {
				jsonError(w, http.StatusBadRequest, "the end must be in the future; use End now to end it")
				return
			}
			e.Expires = req.Expires.UTC()
		}
		if req.MaxPeople != nil && *req.MaxPeople >= 0 {
			e.MaxPeople = *req.MaxPeople
		}
		if e.Invite != "" {
			s.Store.UpdateInvite(e.Invite, func(i *store.Invite) { i.Expires, i.MaxUses = e.Expires, e.MaxPeople })
		}
		if err := s.Store.PutEvent(e); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("event changed", "admin", id.Username, "event", e.Name, "expires", e.Expires)
		writeJSON(w, http.StatusOK, s.eventViews())
	}))
	mux.HandleFunc("POST /admin/api/events/{id}/end", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		e, ok := s.Store.Event(r.PathValue("id"))
		if !ok || !e.Ended.IsZero() {
			jsonError(w, http.StatusNotFound, "no such active event")
			return
		}
		s.endEvent(e, id.Username)
		writeJSON(w, http.StatusOK, s.eventViews())
	}))
	// Delete an ended event's record (its mission stays).
	mux.HandleFunc("DELETE /admin/api/events/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		e, ok := s.Store.Event(r.PathValue("id"))
		if !ok {
			jsonError(w, http.StatusNotFound, "no such event")
			return
		}
		if e.Ended.IsZero() {
			jsonError(w, http.StatusBadRequest, "end the event first")
			return
		}
		s.Store.DeleteEvent(e.ID)
		writeJSON(w, http.StatusOK, s.eventViews())
	}))
}

func (s *Server) createEvent(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct {
		Name      string    `json:"name"`
		Mode      string    `json:"mode"`
		Expires   time.Time `json:"expires"`
		MaxPeople int       `json:"max_people"`
		Mission   bool      `json:"mission"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	switch {
	case !groupNameRE.MatchString(req.Name) || strings.Contains(req.Name, ","):
		jsonError(w, http.StatusBadRequest, "event names are letters, digits, spaces and . _ @ ( ) - (up to 64 characters)")
		return
	case req.Mode != "person" && req.Mode != "shared":
		jsonError(w, http.StatusBadRequest, "people join one by one (person) or with one shared account (shared)")
		return
	case !req.Expires.After(time.Now().Add(time.Minute)):
		jsonError(w, http.StatusBadRequest, "the event must end in the future")
		return
	case req.MaxPeople < 0:
		req.MaxPeople = 0
	}
	// Its group: the event's name, unless a group of that name exists.
	group := req.Name
	if _, exists := s.Store.Group(group); exists {
		group = truncate(req.Name, 56) + " (event)"
	}
	if _, err := s.Store.CreateGroup(group, "Event "+req.Name+" (removed when the event ends)"); err != nil {
		jsonError(w, http.StatusConflict, "a group named "+group+" already exists; choose another event name")
		return
	}
	e := store.Event{ID: uuid(), Name: req.Name, Mode: req.Mode, Group: group, MaxPeople: req.MaxPeople,
		Expires: req.Expires.UTC(), Created: time.Now().UTC(), CreatedBy: id.Username}
	if req.Mission {
		_, _, err := s.Missions.Create(mission.CreateParams{Name: req.Name, Description: "Event " + req.Name, Groups: []string{group}},
			mission.Caller{Username: id.Username, Admin: true, Groups: []string{group}, OutGroups: []string{group}})
		if err == nil {
			e.Mission = req.Name
		} else {
			s.Log.Warn("event mission not created", "event", req.Name, "err", err)
		}
	}
	switch req.Mode {
	case "person":
		inv, err := s.Store.CreateInvite(store.Invite{Code: randomCode(), Label: "Event: " + req.Name, Groups: []string{group},
			MaxUses: req.MaxPeople, Expires: e.Expires, Created: e.Created, CreatedBy: id.Username, Event: e.ID})
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		e.Invite = inv.Code
	case "shared":
		e.Account, e.Password = "event-"+eventSlug(req.Name), randomToken(8)
		for i := 2; ; i++ {
			if _, taken := s.Store.User(e.Account); !taken {
				break
			}
			e.Account = fmt.Sprintf("event-%s-%d", eventSlug(req.Name), i)
		}
		hash, err := store.HashPassword(e.Password)
		if err == nil {
			_, err = s.Store.CreateUserWithHash(e.Account, hash, []string{group})
		}
		if err == nil {
			_, err = s.Store.UpdateUser(e.Account, func(u *store.User) error { u.Event = e.ID; return nil })
		}
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := s.Store.PutEvent(e); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("event created", "admin", id.Username, "event", e.Name, "mode", e.Mode, "group", e.Group, "mission", e.Mission, "expires", e.Expires)
	writeJSON(w, http.StatusCreated, s.eventViews())
}

// endEvent erases an event's accounts and removes its group and invite.
func (s *Server) endEvent(e store.Event, by string) {
	removed := 0
	for _, u := range s.Store.Users() {
		if u.Event == e.ID && s.removeUser(u.Username) == nil {
			removed++
		}
	}
	if e.Invite != "" {
		s.Store.RevokeInvite(e.Invite)
	}
	if affected, err := s.Store.DeleteGroup(e.Group); err == nil {
		for _, name := range affected {
			s.applyUserGroups(name) // others an admin had put in the group
		}
	}
	e.Ended, e.EndedBy, e.Removed, e.Password = time.Now().UTC(), by, removed, ""
	s.Store.PutEvent(e)
	s.Log.Warn("event ended", "event", e.Name, "by", by, "accounts_removed", removed, "mission_kept", e.Mission)
}

// eventsStart ends events as they expire.
func (s *Server) eventsStart(ctx context.Context) {
	go func() {
		t := time.NewTicker(eventCheckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.EndExpiredEvents(time.Now())
			}
		}
	}()
}

// EndExpiredEvents ends the events whose time is up (run every 30 s;
// tests pass a later time).
func (s *Server) EndExpiredEvents(now time.Time) {
	for _, e := range s.Store.Events() {
		if e.Ended.IsZero() && !now.Before(e.Expires) {
			s.endEvent(e, "expired")
		}
	}
}
