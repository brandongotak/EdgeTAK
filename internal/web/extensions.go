package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/extension"
	"github.com/brandongotak/EdgeTAK/internal/router"
)

// Extensions (server plugins, see package extension) reach EdgeTAK through
// a local API on a loopback port, with a token made for each run. Each
// extension joins the router as a tap with the groups an admin gave it:
// it receives its in groups' CoT and sends to its out groups. Its page is
// served through the console at /admin/ext/<id>/, behind the admin login.

type extState struct {
	mu       sync.Mutex
	apiURL   string
	sessions map[string]*extSession // by token
}

// extSession is one run of an extension.
type extSession struct {
	m         extension.Manifest
	tap       *router.Client
	streaming atomic.Bool
}

const maxExtCoT = 1 << 20

func (s *Server) extensionsStart(ctx context.Context) {
	if s.Extensions == nil {
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.Log.Error("extension API: can't listen on loopback; extensions are not started", "err", err)
		return
	}
	srv := &http.Server{Handler: s.extensionAPI(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	go func() { <-ctx.Done(); srv.Close() }()
	s.ext.mu.Lock()
	s.ext.apiURL = "http://" + ln.Addr().String()
	s.ext.sessions = map[string]*extSession{}
	s.ext.mu.Unlock()
	s.Extensions.Prepare = s.prepareExtension
	for _, st := range s.Extensions.List() {
		if s.Store.Extension(st.ID).Enabled {
			if err := s.Extensions.Enable(st.ID); err != nil {
				s.Log.Warn("extension not started", "extension", st.ID, "err", err)
			}
		}
	}
}

// prepareExtension makes a run's token and tap.
func (s *Server) prepareExtension(m extension.Manifest) ([]string, func()) {
	token := randomToken(32)
	sess := &extSession{m: m}
	if m.Can(extension.PermCoTRead) || m.Can(extension.PermCoTWrite) {
		in, out := s.extensionGroups(m)
		sess.tap = router.NewClient("extension", "extension", "extension:"+m.ID, in, out, nil)
		s.Router.AddTap(sess.tap)
	}
	s.ext.mu.Lock()
	s.ext.sessions[token] = sess
	apiURL := s.ext.apiURL
	s.ext.mu.Unlock()
	env := []string{"EDGETAK_URL=" + apiURL, "EDGETAK_TOKEN=" + token, "EDGETAK_SERVER_NAME=" + s.Cfg.ServerName}
	if m.Page {
		env = append(env, "EDGETAK_PAGE_PATH=/admin/ext/"+m.ID+"/")
	}
	return env, func() {
		s.ext.mu.Lock()
		delete(s.ext.sessions, token)
		s.ext.mu.Unlock()
		if sess.tap != nil {
			s.Router.RemoveTap(sess.tap)
			sess.tap.Close()
		}
	}
}

// extensionGroups are the groups an extension's tap uses: what an admin
// chose, as far as its permissions reach.
func (s *Server) extensionGroups(m extension.Manifest) (in, out []string) {
	set := s.Store.Extension(m.ID)
	if m.Can(extension.PermCoTRead) {
		in = set.GroupsIn
	}
	if m.Can(extension.PermCoTWrite) {
		out = set.GroupsOut
	}
	return in, out
}

// applyExtensionGroups gives running extensions their current groups.
func (s *Server) applyExtensionGroups() {
	s.ext.mu.Lock()
	defer s.ext.mu.Unlock()
	for _, sess := range s.ext.sessions {
		if sess.tap != nil {
			sess.tap.SetGroups(s.extensionGroups(sess.m))
		}
	}
}

func (s *Server) extSessionFor(id string) *extSession {
	s.ext.mu.Lock()
	defer s.ext.mu.Unlock()
	for _, sess := range s.ext.sessions {
		if sess.m.ID == id {
			return sess
		}
	}
	return nil
}

// ---- admin API ----

type extensionView struct {
	extension.Status
	GroupsIn    []string  `json:"groups_in"`
	GroupsOut   []string  `json:"groups_out"`
	InstalledBy string    `json:"installed_by,omitempty"`
	InstalledAt time.Time `json:"installed_at,omitzero"`
	PagePath    string    `json:"page_path,omitempty"` // while its page can be opened
	Received    uint64    `json:"received"`            // CoT sent to it this run
	Sent        uint64    `json:"sent"`                // CoT it sent this run
	Dropped     uint64    `json:"dropped,omitempty"`   // CoT it didn't read in time
	Streaming   bool      `json:"streaming,omitempty"`
}

func (s *Server) extensionView(st extension.Status) extensionView {
	set := s.Store.Extension(st.ID)
	v := extensionView{Status: st, GroupsIn: set.GroupsIn, GroupsOut: set.GroupsOut, InstalledBy: set.InstalledBy, InstalledAt: set.InstalledAt}
	if v.GroupsIn == nil {
		v.GroupsIn = []string{}
	}
	if v.GroupsOut == nil {
		v.GroupsOut = []string{}
	}
	if _, ok := s.Extensions.PagePort(st.ID); ok {
		v.PagePath = "/admin/ext/" + st.ID + "/"
	}
	if sess := s.extSessionFor(st.ID); sess != nil && sess.tap != nil {
		v.Received, v.Sent, v.Dropped = sess.tap.Out.Load(), sess.tap.In.Load(), sess.tap.Dropped.Load()
		v.Streaming = sess.streaming.Load()
	}
	return v
}

func (s *Server) extensionList(w http.ResponseWriter) {
	out := []extensionView{}
	for _, st := range s.Extensions.List() {
		out = append(out, s.extensionView(st))
	}
	writeJSON(w, http.StatusOK, map[string]any{"extensions": out, "platform": platform()})
}

func (s *Server) extensionRoutes(mux *http.ServeMux) {
	if s.Extensions == nil {
		return
	}
	mux.HandleFunc("GET /admin/api/extensions", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		s.extensionList(w)
	}))
	mux.HandleFunc("POST /admin/api/extensions", s.admin(s.installExtension))
	mux.HandleFunc("PUT /admin/api/extensions/{id}", s.admin(s.setExtension))
	mux.HandleFunc("POST /admin/api/extensions/{id}/restart", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		s.Extensions.Restart(r.PathValue("id"))
		s.extensionList(w)
	}))
	mux.HandleFunc("DELETE /admin/api/extensions/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		ext := r.PathValue("id")
		if err := s.Extensions.Remove(ext, r.URL.Query().Get("data") == "1"); errors.Is(err, os.ErrNotExist) {
			jsonError(w, http.StatusNotFound, "no such extension")
			return
		} else if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Store.DeleteExtension(ext)
		s.Log.Info("extension removed", "admin", id.Username, "extension", ext)
		s.extensionList(w)
	}))
	mux.HandleFunc("GET /admin/api/extensions/{id}/log", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		lines := s.Extensions.Logs(r.PathValue("id"))
		if lines == nil {
			lines = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
	}))
	// The extension's page, behind the admin login. Each method separately,
	// so these are more specific than the console's GET /admin/.
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		mux.HandleFunc(method+" /admin/ext/{id}/{path...}", s.extensionPage)
	}
	mux.HandleFunc("GET /admin/ext/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/ext/"+url.PathEscape(r.PathValue("id"))+"/", http.StatusFound)
	})
}

// installExtension installs or updates an extension from the zip in the
// request body. A new extension starts stopped, without groups, for an
// admin to review its permissions first.
func (s *Server) installExtension(w http.ResponseWriter, r *http.Request, id *identity) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, extension.MaxUpload))
	if err != nil {
		jsonError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("an extension can be up to %d MB", extension.MaxUpload>>20))
		return
	}
	m, updated, err := s.Extensions.Install(data)
	if err != nil && m.ID == "" {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	set := s.Store.Extension(m.ID)
	set.InstalledBy, set.InstalledAt = id.Username, time.Now().UTC()
	if serr := s.Store.SetExtension(m.ID, set); serr != nil && err == nil {
		err = serr
	}
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Log.Info("extension installed", "admin", id.Username, "extension", m.ID, "version", m.Version, "update", updated)
	st, _ := s.Extensions.Status(m.ID)
	writeJSON(w, http.StatusOK, map[string]any{"extension": s.extensionView(st), "updated": updated})
}

func (s *Server) setExtension(w http.ResponseWriter, r *http.Request, id *identity) {
	ext := r.PathValue("id")
	if _, ok := s.Extensions.Manifest(ext); !ok {
		jsonError(w, http.StatusNotFound, "no such extension")
		return
	}
	var req struct {
		Enabled   *bool     `json:"enabled"`
		GroupsIn  *[]string `json:"groups_in"`
		GroupsOut *[]string `json:"groups_out"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	set := s.Store.Extension(ext)
	for _, gs := range []*[]string{req.GroupsIn, req.GroupsOut} {
		if gs == nil {
			continue
		}
		for _, g := range *gs {
			if _, ok := s.Store.Group(g); !ok {
				jsonError(w, http.StatusBadRequest, "no such group: "+g)
				return
			}
		}
	}
	if req.GroupsIn != nil {
		set.GroupsIn = slices.Compact(slices.Sorted(slices.Values(*req.GroupsIn)))
	}
	if req.GroupsOut != nil {
		set.GroupsOut = slices.Compact(slices.Sorted(slices.Values(*req.GroupsOut)))
	}
	if req.Enabled != nil {
		set.Enabled = *req.Enabled
	}
	if err := s.Store.SetExtension(ext, set); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.applyExtensionGroups()
	if req.Enabled != nil {
		if *req.Enabled {
			if err := s.Extensions.Enable(ext); err != nil {
				jsonError(w, http.StatusBadRequest, err.Error())
				return
			}
		} else {
			s.Extensions.Disable(ext)
		}
	}
	s.Log.Info("extension settings changed", "admin", id.Username, "extension", ext, "enabled", set.Enabled,
		"groups_in", set.GroupsIn, "groups_out", set.GroupsOut)
	s.extensionList(w)
}

// extensionPage passes a console request on to the extension's own page.
// The extension learns who is signed in (X-EdgeTAK-User) but never gets
// the console's cookies, and can't set any.
func (s *Server) extensionPage(w http.ResponseWriter, r *http.Request) {
	id := s.adminIdentity(w, r)
	if id == nil {
		return
	}
	ext := r.PathValue("id")
	port, ok := s.Extensions.PagePort(ext)
	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><p style="font:14px system-ui,sans-serif;color:#666">This extension isn't running, or has no page.</p>`)
		return
	}
	prefix := "/admin/ext/" + ext + "/"
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port)}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path, pr.Out.URL.RawPath = "/"+r.PathValue("path"), ""
			for _, h := range []string{"Cookie", "Authorization", csrfHeader} {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set("X-EdgeTAK-User", id.Username)
			pr.Out.Header.Set("X-Forwarded-Prefix", prefix)
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			resp.Header.Set("X-Frame-Options", "SAMEORIGIN") // the console shows it in a frame
			resp.Header.Set("Content-Security-Policy", "frame-ancestors 'self'")
			if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, prefix) {
				resp.Header.Set("Location", prefix+strings.TrimPrefix(loc, "/"))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "the extension's page did not answer: "+err.Error(), http.StatusBadGateway)
		},
	}
	w.Header().Del("X-Frame-Options")
	w.Header().Del("Content-Security-Policy")
	proxy.ServeHTTP(w, r)
}

// ---- the extensions' API ----

func (s *Server) extensionAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/info", s.extAuth("", s.extInfo))
	mux.HandleFunc("GET /api/v1/cot", s.extAuth(extension.PermCoTRead, s.extStream))
	mux.HandleFunc("POST /api/v1/cot", s.extAuth(extension.PermCoTWrite, s.extSend))
	mux.HandleFunc("GET /api/v1/groups", s.extAuth(extension.PermDataRead, func(w http.ResponseWriter, r *http.Request, _ *extSession) {
		writeJSON(w, http.StatusOK, s.Store.Groups())
	}))
	mux.HandleFunc("GET /api/v1/clients", s.extAuth(extension.PermDataRead, s.extClients))
	mux.HandleFunc("GET /api/v1/missions", s.extAuth(extension.PermDataRead, func(w http.ResponseWriter, r *http.Request, _ *extSession) {
		writeJSON(w, http.StatusOK, s.Missions.Summaries())
	}))
	mux.HandleFunc("GET /api/v1/history", s.extAuth(extension.PermDataRead, s.extHistory))
	return mux
}

func (s *Server) extAuth(perm string, h func(http.ResponseWriter, *http.Request, *extSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.ext.mu.Lock()
		sess := s.ext.sessions[token]
		s.ext.mu.Unlock()
		if token == "" || sess == nil {
			jsonError(w, http.StatusUnauthorized, "missing or wrong token (EDGETAK_TOKEN)")
			return
		}
		if perm != "" && !sess.m.Can(perm) {
			jsonError(w, http.StatusForbidden, "this needs the "+perm+" permission in "+extension.ManifestFile)
			return
		}
		h(w, r, sess)
	}
}

func (s *Server) extInfo(w http.ResponseWriter, r *http.Request, sess *extSession) {
	in, out := s.extensionGroups(sess.m)
	info := map[string]any{"id": sess.m.ID, "name": sess.m.Name, "version": sess.m.Version, "permissions": sess.m.Permissions,
		"server_name": s.Cfg.ServerName, "edgetak_version": s.Version, "groups_in": nonNil(in), "groups_out": nonNil(out)}
	if sess.m.Page {
		info["page_path"] = "/admin/ext/" + sess.m.ID + "/"
	}
	writeJSON(w, http.StatusOK, info)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// extEvent is one line of the CoT stream.
type extEvent struct {
	UID      string    `json:"uid"`
	Type     string    `json:"type"`
	How      string    `json:"how,omitempty"`
	Time     time.Time `json:"time"`
	Stale    time.Time `json:"stale"`
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	Hae      float64   `json:"hae"`
	Callsign string    `json:"callsign,omitempty"`
	XML      string    `json:"xml"`
}

// extStream sends the extension the CoT of its in groups, one JSON object
// per line (and an empty line every 30 s, to keep the connection open),
// starting with the latest position of everything it may see.
func (s *Server) extStream(w http.ResponseWriter, r *http.Request, sess *extSession) {
	if !sess.streaming.CompareAndSwap(false, true) {
		jsonError(w, http.StatusConflict, "this extension is already reading the stream")
		return
	}
	defer sess.streaming.Store(false)
	var types []string
	for _, t := range strings.Split(r.URL.Query().Get("types"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			types = append(types, t)
		}
	}
	match := func(ev *cot.Event) bool {
		return len(types) == 0 || slices.ContainsFunc(types, func(t string) bool { return strings.HasPrefix(ev.Type, t) })
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	tap := sess.tap
	// What queued while nothing was reading is old: start afresh.
	for drained := false; !drained; {
		select {
		case <-tap.Queue():
		default:
			drained = true
		}
	}
	s.Router.Replay(tap)
	if flusher != nil {
		flusher.Flush()
	}
	enc := json.NewEncoder(w)
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case ev := <-tap.Queue():
			if match(ev) {
				if enc.Encode(extEvent{UID: ev.UID, Type: ev.Type, How: ev.How, Time: ev.Time, Stale: ev.Stale,
					Lat: ev.Lat, Lon: ev.Lon, Hae: ev.Hae, Callsign: ev.Callsign(), XML: string(ev.XML())}) != nil {
					return
				}
				tap.Out.Add(1)
			}
			if len(tap.Queue()) == 0 && flusher != nil {
				flusher.Flush()
			}
		case <-keepalive.C:
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-tap.Done():
			return
		case <-r.Context().Done():
			return
		}
	}
}

// extSend routes the CoT events in the body (one or more <event>
// elements) to the extension's out groups, or with ?groups=A,B to some of
// them.
func (s *Server) extSend(w http.ResponseWriter, r *http.Request, sess *extSession) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxExtCoT))
	if err != nil {
		jsonError(w, http.StatusRequestEntityTooLarge, "send up to 1 MB of CoT at a time")
		return
	}
	out := sess.tap.OutGroups()
	if len(out) == 0 {
		jsonError(w, http.StatusConflict, "this extension has no groups to send to; choose its out groups on the Extensions page")
		return
	}
	var groups []string
	if q := r.URL.Query().Get("groups"); q != "" {
		for _, g := range strings.Split(q, ",") {
			if g = strings.TrimSpace(g); g == "" {
				continue
			}
			if !slices.Contains(out, g) {
				jsonError(w, http.StatusBadRequest, fmt.Sprintf("%q is not one of this extension's out groups (%s)", g, strings.Join(out, ", ")))
				return
			}
			groups = append(groups, g)
		}
	}
	var events []*cot.Event
	for i, raw := range splitEvents(body) {
		ev, err := cot.ParseXML(raw)
		if err != nil {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("event %d: %v", i+1, err))
			return
		}
		if ev.UID == "" || ev.Type == "" {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("event %d: uid and type are required", i+1))
			return
		}
		events = append(events, ev)
	}
	if len(events) == 0 {
		jsonError(w, http.StatusBadRequest, "no <event> in the body")
		return
	}
	for _, ev := range events {
		if groups != nil {
			s.Router.RouteTo(sess.tap, groups, ev)
		} else {
			s.Router.Route(sess.tap, ev)
		}
		sess.tap.In.Add(1)
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": len(events)})
}

// splitEvents finds each <event>...</event> in b.
func splitEvents(b []byte) [][]byte {
	var out [][]byte
	for {
		i := bytes.Index(b, []byte("<event"))
		if i < 0 {
			return out
		}
		j := bytes.Index(b[i:], []byte("</event>"))
		if j < 0 {
			// A self-closing <event .../> has no detail; ParseXML says what's wrong.
			return append(out, b[i:])
		}
		out = append(out, b[i:i+j+len("</event>")])
		b = b[i+j+len("</event>"):]
	}
}

func (s *Server) extClients(w http.ResponseWriter, r *http.Request, _ *extSession) {
	type device struct {
		UID       string    `json:"uid"`
		Callsign  string    `json:"callsign"`
		Username  string    `json:"username,omitempty"`
		Transport string    `json:"transport"`
		GroupsIn  []string  `json:"groups_in"`
		GroupsOut []string  `json:"groups_out"`
		Connected time.Time `json:"connected"`
		LastSeen  time.Time `json:"last_seen"`
		Lat       *float64  `json:"lat,omitempty"`
		Lon       *float64  `json:"lon,omitempty"`
		Position  time.Time `json:"position_time,omitzero"`
	}
	out := []device{}
	for _, c := range s.Router.Clients() {
		uid, cs := c.Identity()
		d := device{UID: uid, Callsign: cs, Username: c.Username, Transport: c.Transport, GroupsIn: nonNil(c.InGroups()),
			GroupsOut: nonNil(c.OutGroups()), Connected: c.ConnectedAt, LastSeen: c.LastSeen()}
		if sa := c.LastSA(); sa != nil {
			d.Lat, d.Lon, d.Position = &sa.Lat, &sa.Lon, c.LastSAAt()
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

// extHistory returns recorded tracks: ?since= and ?until= (RFC 3339,
// default the last 24 hours) and optionally ?uid=.
func (s *Server) extHistory(w http.ResponseWriter, r *http.Request, _ *extSession) {
	if s.History == nil {
		jsonError(w, http.StatusNotFound, "position history is off")
		return
	}
	until, since := time.Now(), time.Now().Add(-24*time.Hour)
	for name, t := range map[string]*time.Time{"since": &since, "until": &until} {
		if v := r.URL.Query().Get(name); v != "" {
			p, err := time.Parse(time.RFC3339, v)
			if err != nil {
				jsonError(w, http.StatusBadRequest, name+" must be an RFC 3339 time, e.g. 2026-10-26T08:00:00Z")
				return
			}
			*t = p
		}
	}
	writeJSON(w, http.StatusOK, s.History.Tracks(since, until, r.URL.Query().Get("uid")))
}

// platform is what a manifest's run is matched against here.
func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }
