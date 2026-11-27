package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Shared map links (Data → Share map): a read-only live map of chosen
// groups' devices for someone without a TAK app, at /share/<token> on the
// enrollment port (which has the trusted certificate when Let's Encrypt is
// on). No sign-in: the link is the key, so it expires, can be deleted, and
// shows only callsigns and positions, never usernames or anything else.

const maxShareHours = 30 * 24

type shareLinkView struct {
	store.ShareLink
	URL   string `json:"url"`
	State string `json:"state"` // active or expired
}

func (s *Server) shareURL(r *http.Request, token string) string {
	_, port, err := net.SplitHostPort(s.Addrs["enroll"])
	if err != nil {
		if _, port, err = net.SplitHostPort(s.Addrs["https"]); err != nil {
			return "/share/" + token
		}
	}
	return "https://" + net.JoinHostPort(s.deviceHost(r), port) + "/share/" + token
}

func (s *Server) shareLinkViews(r *http.Request) []shareLinkView {
	now := time.Now()
	out := []shareLinkView{}
	for _, l := range s.Store.ShareLinks() {
		v := shareLinkView{ShareLink: l, URL: s.shareURL(r, l.Token), State: "active"}
		if !now.Before(l.Expires) {
			v.State = "expired"
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b shareLinkView) int { return b.Created.Compare(a.Created) })
	return out
}

func newShareToken() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) shareRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/share-links", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.shareLinkViews(r))
	}))
	mux.HandleFunc("POST /admin/api/share-links", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			Name      string   `json:"name"`
			Groups    []string `json:"groups"`
			Hours     int      `json:"hours"`
			Callsigns bool     `json:"callsigns"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || utf8.RuneCountInString(req.Name) > 80 {
			jsonError(w, http.StatusBadRequest, "give the map a name of up to 80 characters; whoever opens it sees it")
			return
		}
		if req.Hours < 1 || req.Hours > maxShareHours {
			jsonError(w, http.StatusBadRequest, "a link lasts from 1 hour to 30 days")
			return
		}
		if len(s.Store.ShareLinks()) >= 100 {
			jsonError(w, http.StatusBadRequest, "this server has 100 shared map links; delete some first")
			return
		}
		now := time.Now().UTC()
		l := store.ShareLink{ID: uuid(), Token: newShareToken(), Name: req.Name, Groups: cleanGroups(req.Groups), Callsigns: req.Callsigns,
			Expires: now.Add(time.Duration(req.Hours) * time.Hour), Created: now, CreatedBy: id.Username}
		if err := s.Store.AddShareLink(l); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("shared map link made", "admin", id.Username, "name", l.Name, "groups", l.Groups, "hours", req.Hours)
		writeJSON(w, http.StatusCreated, map[string]any{"link": shareLinkView{ShareLink: l, URL: s.shareURL(r, l.Token), State: "active"},
			"links": s.shareLinkViews(r)})
	}))
	mux.HandleFunc("DELETE /admin/api/share-links/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if err := s.Store.DeleteShareLink(r.PathValue("id")); err != nil {
			jsonError(w, http.StatusNotFound, "no such link")
			return
		}
		s.Log.Info("shared map link deleted", "admin", id.Username, "link", r.PathValue("id"))
		writeJSON(w, http.StatusOK, s.shareLinkViews(r))
	}))

	// The public map and its data.
	mux.HandleFunc("GET /share/{token}", func(w http.ResponseWriter, r *http.Request) {
		if l, ok := s.liveShareLink(r.PathValue("token")); ok {
			s.Store.ViewShareLink(l.ID, time.Now().UTC())
		}
		shareHeaders(w)
		http.ServeFileFS(w, r, staticSub(), "share.html")
	})
	mux.HandleFunc("GET /share/{token}/data", func(w http.ResponseWriter, r *http.Request) {
		shareHeaders(w)
		l, ok := s.liveShareLink(r.PathValue("token"))
		if !ok {
			jsonError(w, http.StatusGone, "This map link has ended or was removed.")
			return
		}
		writeJSON(w, http.StatusOK, s.shareData(l))
	})
}

func shareHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer") // the link is the key
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; "+
		"script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
}

func (s *Server) liveShareLink(token string) (store.ShareLink, bool) {
	if token == "" || len(token) > 64 {
		return store.ShareLink{}, false
	}
	l, ok := s.Store.ShareLinkByToken(token)
	if !ok || !time.Now().Before(l.Expires) {
		return store.ShareLink{}, false
	}
	return l, true
}

type shareDevice struct {
	Callsign string  `json:"callsign,omitempty"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Team     string  `json:"team,omitempty"`
	Type     string  `json:"type,omitempty"`
	Speed    float64 `json:"speed,omitempty"`  // m/s
	Course   float64 `json:"course,omitempty"` // degrees
	Seen     int64   `json:"seen"`             // Unix ms of the last report
}

// shareData is what a shared map shows: devices in its groups that have
// reported a position.
func (s *Server) shareData(l store.ShareLink) map[string]any {
	devices := []shareDevice{}
	for _, c := range s.Router.Clients() {
		uid, callsign := c.Identity()
		sa := c.LastSA()
		if uid == "" || sa == nil || (sa.Lat == 0 && sa.Lon == 0) {
			continue
		}
		if len(l.Groups) > 0 && !slices.ContainsFunc(deviceGroups(c), func(g string) bool { return slices.Contains(l.Groups, g) }) {
			continue
		}
		d := shareDevice{Lat: sa.Lat, Lon: sa.Lon, Type: sa.Type, Seen: c.LastSAAt().UnixMilli()}
		if l.Callsigns {
			d.Callsign = callsign
		}
		if g := sa.Detail.Group; g != nil {
			d.Team = g.Name
		}
		if t := sa.Detail.Track; t != nil {
			d.Speed, d.Course = t.Speed, t.Course
		}
		devices = append(devices, d)
	}
	slices.SortFunc(devices, func(a, b shareDevice) int { return strings.Compare(a.Callsign, b.Callsign) })
	return map[string]any{"name": l.Name, "server": s.Cfg.ServerName, "expires": l.Expires, "devices": devices, "now": time.Now().UnixMilli()}
}
