package web

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// "Send to devices": the admin console sends a chat message or offers a
// data package to connected clients, using the same CoT formats ATAK
// produces (GeoChat b-t-f and file transfer b-f-t-r), so every TAK client
// handles them natively.

const allChatRooms = "All Chat Rooms" // ATAK's GeoChatService.DEFAULT_CHATROOM_NAME

// sendTarget selects recipients: everyone, members of groups (by their IN
// groups), or specific connected clients by id. Selections combine.
type sendTarget struct {
	All     bool     `json:"all"`
	Groups  []string `json:"groups"`
	Clients []uint64 `json:"clients"`
}

type sendRecord struct {
	Time      time.Time `json:"time"`
	Admin     string    `json:"admin"`
	Kind      string    `json:"kind"` // message or package
	Summary   string    `json:"summary"`
	To        string    `json:"to"`
	Delivered int       `json:"delivered"`
}

type sendHistory struct {
	mu      sync.Mutex
	entries []sendRecord
}

func (h *sendHistory) add(r sendRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append([]sendRecord{r}, h.entries...)
	if len(h.entries) > 100 {
		h.entries = h.entries[:100]
	}
}

func (h *sendHistory) list() []sendRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sendRecord{}, h.entries...)
}

func (s *Server) sendRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/api/send/message", s.admin(s.sendMessage))
	mux.HandleFunc("POST /admin/api/send/package", s.admin(s.sendPackage))
	mux.HandleFunc("GET /admin/api/send/history", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.history.list())
	}))
}

// resolveTargets returns the connected clients a target selects.
func (s *Server) resolveTargets(t sendTarget) []*router.Client {
	var out []*router.Client
	for _, c := range s.Router.Clients() {
		switch {
		case t.All, slices.Contains(t.Clients, c.ID):
		case slices.ContainsFunc(c.InGroups(), func(g string) bool { return slices.Contains(t.Groups, g) }):
		default:
			continue
		}
		out = append(out, c)
	}
	return out
}

func describeTarget(t sendTarget) string {
	if t.All {
		return "everyone"
	}
	var parts []string
	if len(t.Groups) > 0 {
		parts = append(parts, "groups "+strings.Join(t.Groups, ", "))
	}
	if len(t.Clients) > 0 {
		parts = append(parts, strconv.Itoa(len(t.Clients))+" device(s)")
	}
	return strings.Join(parts, "; ")
}

// senderUID identifies the server as a chat/file sender. ATAK splits event
// UIDs on dots, so it must not contain any.
func (s *Server) senderUID() string { return "EdgeTAK-" + strings.ReplaceAll(s.NodeID, ".", "") }

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct {
		From string     `json:"from"`
		Text string     `json:"text"`
		To   sendTarget `json:"to"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		jsonError(w, http.StatusBadRequest, "the message is empty")
		return
	}
	from := firstNonEmpty(strings.TrimSpace(req.From), s.Cfg.ServerName)
	targets := s.resolveTargets(req.To)
	if len(targets) == 0 {
		jsonError(w, http.StatusBadRequest, "no connected devices match the selected recipients")
		return
	}
	ev := s.chatEvent(from, req.Text)
	for _, c := range targets {
		c.Send(ev)
	}
	s.history.add(sendRecord{Time: time.Now().UTC(), Admin: id.Username, Kind: "message", Summary: from + ": " + truncate(req.Text, 80),
		To: describeTarget(req.To), Delivered: len(targets)})
	s.Log.Info("admin sent message", "admin", id.Username, "to", describeTarget(req.To), "devices", len(targets))
	writeJSON(w, http.StatusOK, map[string]int{"delivered": len(targets)})
}

// chatEvent builds a GeoChat message to "All Chat Rooms" as ATAK sends it.
func (s *Server) chatEvent(from, text string) *cot.Event {
	sender, msgID := s.senderUID(), uuid()
	now := time.Now().UTC()
	ev := cot.NewEvent("b-t-f", "GeoChat."+sender+"."+allChatRooms+"."+msgID, "h-g-i-g-o", 24*time.Hour)
	ev.Detail.XML = `<__chat parent="RootContactGroup" groupOwner="false" messageId="` + msgID + `" chatroom="` + allChatRooms +
		`" id="` + allChatRooms + `" senderCallsign="` + xmlEscape(from) + `">` +
		`<chatgrp uid0="` + sender + `" uid1="` + allChatRooms + `" id="` + allChatRooms + `"/></__chat>` +
		`<link uid="` + sender + `" type="a-f-G-U-C" relation="p-p"/>` +
		`<remarks source="BAO.F.ATAK.` + sender + `" sourceID="` + sender + `" to="` + allChatRooms + `" time="` +
		now.Format(cot.TimeLayout) + `">` + xmlEscape(text) + `</remarks>`
	return ev
}

func (s *Server) sendPackage(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct {
		From string     `json:"from"`
		Hash string     `json:"hash"`
		To   sendTarget `json:"to"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	p, ok := s.Store.Package(strings.ToLower(req.Hash))
	if !ok {
		jsonError(w, http.StatusNotFound, "data package not found")
		return
	}
	targets := s.resolveTargets(req.To)
	if len(targets) == 0 {
		jsonError(w, http.StatusBadRequest, "no connected devices match the selected recipients")
		return
	}
	// Sending a file shares it: recipients must be able to download it.
	share := slices.Clone(p.Groups)
	for _, c := range targets {
		for _, g := range c.InGroups() {
			if !slices.Contains(share, g) {
				share = append(share, g)
			}
		}
	}
	if len(share) != len(p.Groups) {
		if err := s.Store.UpdatePackage(p.Hash, func(p *store.Package) { p.Groups = share }); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	from := firstNonEmpty(strings.TrimSpace(req.From), s.Cfg.ServerName)
	events := map[string]*cot.Event{} // one per transport; the download URL differs
	delivered, skipped := 0, 0
	for _, c := range targets {
		ev, ok := events[c.Transport]
		if !ok {
			url := s.contentURL(c.Transport, p.Hash)
			if url != "" {
				ev = s.fileShareEvent(from, url, p)
			}
			events[c.Transport] = ev
		}
		if ev == nil {
			skipped++
			continue
		}
		c.Send(ev)
		delivered++
	}
	s.history.add(sendRecord{Time: time.Now().UTC(), Admin: id.Username, Kind: "package", Summary: p.Name,
		To: describeTarget(req.To), Delivered: delivered})
	s.Log.Info("admin sent data package", "admin", id.Username, "package", p.Name, "to", describeTarget(req.To), "devices", delivered, "skipped", skipped)
	writeJSON(w, http.StatusOK, map[string]int{"delivered": delivered, "skipped": skipped})
}

// contentURL is where a client on the given transport downloads a package:
// the TLS API for certificate clients, the plaintext API for TCP clients.
func (s *Server) contentURL(transport, hash string) string {
	scheme, listener := "https", "https"
	if transport == "tcp" {
		scheme, listener = "http", "http"
	}
	_, port, err := net.SplitHostPort(s.Addrs[listener])
	if err != nil || s.Cfg.PublicHost == "" {
		return ""
	}
	return fmt.Sprintf("%s://%s/Marti/sync/content?hash=%s", scheme, net.JoinHostPort(s.Cfg.PublicHost, port), hash)
}

// fileShareEvent builds a file transfer request (b-f-t-r) as ATAK sends one.
func (s *Server) fileShareEvent(from, url string, p *store.Package) *cot.Event {
	ev := cot.NewEvent("b-f-t-r", uuid(), "h-e", 5*time.Minute)
	name := strings.TrimSuffix(p.Name, ".zip")
	ev.Detail.XML = `<fileshare filename="` + xmlEscape(p.Name) + `" senderUrl="` + xmlEscape(url) +
		`" sizeInBytes="` + strconv.FormatInt(p.Size, 10) + `" sha256="` + p.Hash + `" senderUid="` + s.senderUID() +
		`" senderCallsign="` + xmlEscape(from) + `" name="` + xmlEscape(name) + `"/>`
	return ev
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
