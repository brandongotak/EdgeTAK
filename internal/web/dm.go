package web

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Direct messages and the server contact.
//
// ATAK only routes a conversation through the server when the other party
// is a contact learned from a marker with <contact endpoint="*:-1:stcp"/>
// (ContactListDetailHandler). So the server publishes itself as an HQ
// contact at an admin-chosen position; direct messages come from that
// contact, and messages users send back to it land in the admin Inbox.

const (
	presenceStale   = 15 * time.Minute
	presenceRefresh = 5 * time.Minute
)

// Start registers the Inbox hook and the server contact. Call once.
func (s *Server) Start(ctx context.Context) {
	s.Router.AddHook(s.inboxHook)
	s.Router.AddHook(s.chatHook)
	s.Router.AddHook(s.emergencyHook)
	s.Router.SetPresence(s.presenceEvent)
	s.videoStart(ctx)
	s.retentionStart(ctx)
	s.backupStart(ctx)
	go s.redrawAppIcons()
	s.lostStart(ctx)
	s.geofenceStart()
	s.drawingsStart()
	s.overlaysStart()
	s.simStart(ctx)
	s.webhooksStart(ctx)
	s.healthAlertsStart(ctx)
	s.extensionsStart(ctx)
	s.schedulesStart(ctx)
	s.ldapSyncStart(ctx)
	s.eventsStart(ctx)
	s.certWatchStart(ctx)
	go func() {
		t := time.NewTicker(presenceRefresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if ev := s.presenceEvent(); ev != nil {
					s.Router.Broadcast(ev)
				}
			}
		}
	}()
}

// presenceEvent is the server contact's SA, or nil when it is disabled.
func (s *Server) presenceEvent() *cot.Event {
	sc := s.Store.ServerContact()
	if !sc.Enabled {
		return nil
	}
	ev := cot.NewEvent("a-f-G-U-C", s.senderUID(), "h-g-i-g-o", presenceStale)
	ev.Lat, ev.Lon = sc.Lat, sc.Lon
	ev.Detail.Contact = &cot.Contact{Endpoint: "*:-1:stcp", Callsign: s.contactCallsign(sc)}
	ev.Detail.Group = &cot.Group{Name: "White", Role: "HQ"}
	ev.Detail.Takv = &cot.Takv{Device: "EdgeTAK", Platform: "EdgeTAK", OS: "server", Version: s.Version}
	ev.Detail.XML = `<uid Droid="` + xmlEscape(s.contactCallsign(sc)) + `"/>`
	return ev
}

func (s *Server) contactCallsign(sc store.ServerContact) string {
	return firstNonEmpty(strings.TrimSpace(sc.Callsign), s.Cfg.ServerName)
}

// removeEvent tells clients to delete the server contact's marker.
func (s *Server) removeEvent() *cot.Event {
	ev := cot.NewEvent(cot.TypeDelete, uuid(), "h-g-i-g-o", 20*time.Second)
	ev.Detail.XML = `<link uid="` + s.senderUID() + `" relation="none" type="none"/><__forcedelete/>`
	return ev
}

// directEvent is a one-to-one GeoChat message from the server contact to a
// device, as ATAK sends one: the conversation id is the recipient's UID,
// which ATAK files under the sender's (known) contact.
func (s *Server) directEvent(toUID, toCallsign, from, text string) *cot.Event {
	sender, msgID := s.senderUID(), uuid()
	ev := cot.NewEvent("b-t-f", "GeoChat."+sender+"."+toUID+"."+msgID, "h-g-i-g-o", 24*time.Hour)
	ev.Detail.XML = `<__chat parent="RootContactGroup" groupOwner="false" messageId="` + msgID + `" chatroom="` + xmlEscape(toCallsign) +
		`" id="` + xmlEscape(toUID) + `" senderCallsign="` + xmlEscape(from) + `">` +
		`<chatgrp uid0="` + sender + `" uid1="` + xmlEscape(toUID) + `" id="` + xmlEscape(toUID) + `"/></__chat>` +
		`<link uid="` + sender + `" type="a-f-G-U-C" relation="p-p"/>` +
		`<remarks source="BAO.F.ATAK.` + sender + `" sourceID="` + sender + `" to="` + xmlEscape(toUID) + `" time="` +
		time.Now().UTC().Format(cot.TimeLayout) + `">` + xmlEscape(text) + `</remarks>` +
		`<marti><dest uid="` + xmlEscape(toUID) + `"/></marti>`
	return ev
}

// sendDirect sends text privately to each target that has identified itself.
func (s *Server) sendDirect(targets []*router.Client, text, admin string) (delivered, skipped int) {
	sc := s.Store.ServerContact()
	presence, from := s.presenceEvent(), s.contactCallsign(sc)
	for _, c := range targets {
		uid, cs := c.Identity()
		if uid == "" {
			skipped++ // no SA yet, so no UID to address
			continue
		}
		if presence != nil {
			c.Send(presence) // make sure the sender is a known contact first
		}
		ev := s.directEvent(uid, firstNonEmpty(cs, uid), from, text)
		c.Send(ev)
		s.logConsoleChat(ev, admin)
		delivered++
	}
	return delivered, skipped
}

// inboxHook records chat messages that devices address to the server contact.
func (s *Server) inboxHook(src *router.Client, ev *cot.Event) bool {
	if src == nil || ev.Type != "b-t-f" {
		return true
	}
	me, sc := s.senderUID(), s.Store.ServerContact()
	toMe := strings.Contains(ev.Detail.XML, `"`+me+`"`) // chatgrp uidN / __chat id
	for _, d := range ev.Dests() {
		if d.UID == me || (sc.Enabled && d.Callsign != "" && d.Callsign == s.contactCallsign(sc)) {
			toMe = true
		}
	}
	if !toMe || strings.HasPrefix(ev.UID, "GeoChat."+me+".") {
		return true
	}
	uid, callsign := src.Identity()
	msg := store.InboxMessage{ID: uuid(), Time: time.Now().UTC(), FromUID: uid, FromCallsign: callsign,
		Username: src.Username, Text: chatText(ev)}
	if err := s.Store.AddInbox(msg); err != nil {
		s.Log.Error("store inbox message", "err", err)
	}
	s.Log.Info("message to server contact", "from", callsign, "user", src.Username)
	return true
}

// chatText extracts the <remarks> text of a chat event.
func chatText(ev *cot.Event) string {
	i := strings.Index(ev.Detail.XML, "<remarks")
	if i < 0 {
		return ""
	}
	var r struct {
		Text string `xml:",chardata"`
	}
	if err := xml.NewDecoder(strings.NewReader(ev.Detail.XML[i:])).Decode(&r); err != nil {
		return ""
	}
	return strings.TrimSpace(r.Text)
}

func (s *Server) dmRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/send/contact", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		sc := s.Store.ServerContact()
		writeJSON(w, http.StatusOK, map[string]any{"enabled": sc.Enabled, "callsign": s.contactCallsign(sc), "lat": sc.Lat, "lon": sc.Lon, "uid": s.senderUID()})
	}))
	mux.HandleFunc("PUT /admin/api/send/contact", s.admin(s.setServerContact))
	mux.HandleFunc("GET /admin/api/send/inbox", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.Store.Inbox())
	}))
	mux.HandleFunc("POST /admin/api/send/reply", s.admin(s.replyDirect))
}

func (s *Server) setServerContact(w http.ResponseWriter, r *http.Request, id *identity) {
	var req store.ServerContact
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Lat < -90 || req.Lat > 90 || req.Lon < -180 || req.Lon > 180 {
		jsonError(w, http.StatusBadRequest, "latitude must be within ±90 and longitude within ±180")
		return
	}
	if req.Enabled && req.Lat == 0 && req.Lon == 0 {
		jsonError(w, http.StatusBadRequest, "set the contact's position (it appears as a marker on devices)")
		return
	}
	req.Callsign = strings.TrimSpace(req.Callsign)
	was := s.Store.ServerContact().Enabled
	if err := s.Store.SetServerContact(req); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ev := s.presenceEvent(); ev != nil {
		s.Router.Broadcast(ev)
	} else if was {
		s.Router.Broadcast(s.removeEvent())
	}
	s.Log.Info("server contact updated", "admin", id.Username, "enabled", req.Enabled, "callsign", s.contactCallsign(req),
		"position", strconv.FormatFloat(req.Lat, 'f', 5, 64)+","+strconv.FormatFloat(req.Lon, 'f', 5, 64))
	w.WriteHeader(http.StatusNoContent)
}

// replyDirect answers an Inbox message: a direct message to that device.
func (s *Server) replyDirect(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct {
		UID  string `json:"uid"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		jsonError(w, http.StatusBadRequest, "a device and a message are required")
		return
	}
	if !s.Store.ServerContact().Enabled {
		jsonError(w, http.StatusBadRequest, "turn on the server contact first")
		return
	}
	var target []*router.Client
	for _, c := range s.Router.Clients() {
		if uid, _ := c.Identity(); uid == req.UID {
			target = append(target, c)
		}
	}
	if len(target) == 0 {
		jsonError(w, http.StatusNotFound, "that device is not connected")
		return
	}
	delivered, _ := s.sendDirect(target, strings.TrimSpace(req.Text), id.Username)
	_, cs := target[0].Identity()
	s.history.add(sendRecord{Time: time.Now().UTC(), Admin: id.Username, Kind: "direct", Summary: truncate(strings.TrimSpace(req.Text), 80),
		To: "reply to " + cs, Delivered: delivered})
	writeJSON(w, http.StatusOK, map[string]int{"delivered": delivered})
}
