package web

import (
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/chatlog"
	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Chat log: GeoChat messages devices send through the server, and those the
// console sends, recorded only when an admin turns it on.

const chatPageSize = 200

// chatXML is the <__chat> element of a GeoChat message.
type chatXML struct {
	Parent         string `xml:"parent,attr"`
	MessageID      string `xml:"messageId,attr"`
	Chatroom       string `xml:"chatroom,attr"`
	ID             string `xml:"id,attr"`
	SenderCallsign string `xml:"senderCallsign,attr"`
	Group          struct {
		Attrs []xml.Attr `xml:",any,attr"`
	} `xml:"chatgrp"`
}

func parseChat(ev *cot.Event) (chatXML, bool) {
	var c chatXML
	i := strings.Index(ev.Detail.XML, "<__chat")
	if i < 0 {
		return c, false
	}
	if err := xml.NewDecoder(strings.NewReader(ev.Detail.XML[i:])).Decode(&c); err != nil {
		return c, false
	}
	return c, true
}

// chatMessage describes a GeoChat event for the log. src is the device that
// sent it (nil for the console's own messages).
func chatMessage(ev *cot.Event, src *router.Client, username string) (chatlog.Message, bool) {
	if ev.Type != "b-t-f" {
		return chatlog.Message{}, false // b-t-f-d / b-t-f-r are delivery and read receipts
	}
	c, ok := parseChat(ev)
	if !ok {
		return chatlog.Message{}, false
	}
	m := chatlog.Message{ID: firstNonEmpty(c.MessageID, ev.UID), Time: time.Now().UTC(), FromCallsign: c.SenderCallsign,
		Username: username, Text: chatText(ev), Console: src == nil}
	members := []string{}
	for _, a := range c.Group.Attrs {
		if strings.HasPrefix(a.Name.Local, "uid") && a.Value != "" {
			members = append(members, a.Value)
		}
	}
	if src != nil {
		uid, cs := src.Identity()
		m.FromUID, m.FromCallsign = uid, firstNonEmpty(cs, m.FromCallsign)
	}
	if m.FromUID == "" && len(members) > 0 {
		m.FromUID = members[0] // chatgrp uid0 is the sender
	}
	for _, u := range members {
		if u != m.FromUID && u != allChatRooms {
			m.To = append(m.To, u)
		}
	}
	if ev.Lat != 0 || ev.Lon != 0 {
		if ev.Lat >= -90 && ev.Lat <= 90 && ev.Lon >= -180 && ev.Lon <= 180 {
			m.Lat, m.Lon = ev.Lat, ev.Lon
		}
	}
	id := firstNonEmpty(c.ID, c.Chatroom)
	switch {
	case id == allChatRooms || id == "":
		m.Kind, m.Room, m.RoomID = "all", allChatRooms, "all"
	case c.Parent == "TeamGroups":
		m.Kind, m.Room, m.RoomID = "team", firstNonEmpty(c.Chatroom, id)+" team", "team:"+id
	case c.Parent == "RootContactGroup":
		// One-to-one: id is the recipient's UID, chatroom their callsign. Both
		// directions share one conversation, named by both callsigns.
		m.Kind = "direct"
		pair := [][2]string{{m.FromUID, m.FromCallsign}, {id, firstNonEmpty(c.Chatroom, id)}}
		slices.SortFunc(pair, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
		m.RoomID = "direct:" + pair[0][0] + "|" + pair[1][0]
		m.Room = pair[0][1] + " ↔ " + pair[1][1]
	default:
		m.Kind, m.Room, m.RoomID = "group", firstNonEmpty(c.Chatroom, id), "group:"+id
	}
	return m, true
}

// chatHook logs chat messages from devices. It never drops events.
func (s *Server) chatHook(src *router.Client, ev *cot.Event) bool {
	if src == nil || s.Chat == nil || ev.Type != "b-t-f" || !s.Chat.Enabled() {
		return true
	}
	if m, ok := chatMessage(ev, src, src.Username); ok {
		s.Chat.Record(m)
	}
	return true
}

// logConsoleChat logs a message the console sent.
func (s *Server) logConsoleChat(ev *cot.Event, admin string) {
	if s.Chat == nil || !s.Chat.Enabled() {
		return
	}
	if m, ok := chatMessage(ev, nil, admin); ok {
		m.FromUID = s.senderUID()
		s.Chat.Record(m)
	}
}

func chatQuery(r *http.Request) chatlog.Query {
	v := r.URL.Query()
	q := chatlog.Query{RoomID: v.Get("room"), FromUID: v.Get("from"), Text: strings.TrimSpace(v.Get("q"))}
	if t, err := time.Parse(time.RFC3339, v.Get("since")); err == nil {
		q.Since = t
	}
	if t, err := time.Parse(time.RFC3339, v.Get("until")); err == nil {
		q.Until = t
	}
	return q
}

func (s *Server) chatStatus() map[string]any {
	out := map[string]any{"enabled": s.Store.ChatLogSettings().Enabled, "retention_days": s.Store.RetentionSettings().ChatDays}
	if s.Chat != nil {
		out["messages"], out["bytes"] = s.Chat.Stats()
	}
	return out
}

func (s *Server) chatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/chat/settings", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.chatStatus())
	}))
	mux.HandleFunc("PUT /admin/api/chat/settings", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req store.ChatLogSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if err := s.Store.SetChatLogSettings(req); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if s.Chat != nil {
			s.Chat.SetEnabled(req.Enabled)
		}
		s.Log.Info("chat log settings changed", "admin", id.Username, "enabled", req.Enabled)
		writeJSON(w, http.StatusOK, s.chatStatus())
	}))
	// ?room= &from= &q= &since= &until= (RFC 3339) &limit= (default 200)
	mux.HandleFunc("GET /admin/api/chat", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if s.Chat == nil {
			writeJSON(w, http.StatusOK, map[string]any{"messages": []any{}, "total": 0})
			return
		}
		q := chatQuery(r)
		q.Limit = chatPageSize
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 5000 {
			q.Limit = n
		}
		msgs, total := s.Chat.Find(q)
		writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "total": total})
	}))
	mux.HandleFunc("GET /admin/api/chat/rooms", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if s.Chat == nil {
			writeJSON(w, http.StatusOK, map[string]any{"rooms": []any{}, "senders": []any{}})
			return
		}
		rooms, senders := s.Chat.Rooms()
		writeJSON(w, http.StatusOK, map[string]any{"rooms": rooms, "senders": senders})
	}))
	// The same filters as GET /admin/api/chat, oldest first, as CSV or JSON.
	mux.HandleFunc("GET /admin/api/chat/export", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var msgs []chatlog.Message
		if s.Chat != nil {
			msgs, _ = s.Chat.Find(chatQuery(r))
		}
		slices.Reverse(msgs)
		name := "edgetak-chat-" + time.Now().UTC().Format("20060102-150405")
		s.Log.Info("chat log exported", "admin", id.Username, "messages", len(msgs))
		if r.URL.Query().Get("format") == "json" {
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.json"`)
			writeJSON(w, http.StatusOK, msgs)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
		cw := csv.NewWriter(w)
		cw.Write([]string{"time_utc", "conversation", "kind", "from_callsign", "from_uid", "username", "to_uids", "text", "lat", "lon", "from_console"})
		for _, m := range msgs {
			lat, lon := "", ""
			if m.Lat != 0 || m.Lon != 0 {
				lat, lon = strconv.FormatFloat(m.Lat, 'f', 6, 64), strconv.FormatFloat(m.Lon, 'f', 6, 64)
			}
			cw.Write([]string{m.Time.UTC().Format(time.RFC3339), csvSafe(m.Room), m.Kind, csvSafe(m.FromCallsign), csvSafe(m.FromUID), csvSafe(m.Username),
				strings.Join(m.To, " "), csvSafe(m.Text), lat, lon, strconv.FormatBool(m.Console)})
		}
		cw.Flush()
	}))
	mux.HandleFunc("DELETE /admin/api/chat", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if s.Chat != nil {
			if err := s.Chat.Clear(); err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		s.Log.Info("chat log cleared", "admin", id.Username)
		w.WriteHeader(http.StatusNoContent)
	}))
}

// csvSafe keeps spreadsheet apps from running text that looks like a
// formula (anyone can type a chat message).
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
