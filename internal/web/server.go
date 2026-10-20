// Package web serves the Marti HTTP API used by TAK clients (enrollment, data
// packages, contacts) and the browser admin console.
package web

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/config"
	"github.com/brandongotak/EdgeTAK/internal/mission"
	"github.com/brandongotak/EdgeTAK/internal/pki"
	"github.com/brandongotak/EdgeTAK/internal/publiccert"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	Cfg        config.Config
	Store      *store.Store
	CA         *pki.CA
	Router     *router.Router
	Missions   *mission.Service
	Public     *publiccert.Provider // nil unless a public certificate is configured
	Log        *slog.Logger
	PackageDir string
	NodeID     string
	Version    string
	Started    time.Time
	// Addrs maps listener names (tcp, tls, https, enroll, http) to their
	// bound addresses. It is set before serving starts and not modified.
	Addrs map[string]string

	history sendHistory // admin "send to devices" log

	sessMu   sync.Mutex
	sessions map[string]session
}

type session struct {
	username string
	expires  time.Time
}

const (
	sessionCookie = "edgetak_session"
	sessionTTL    = 12 * time.Hour
	csrfHeader    = "X-EdgeTAK"
)

// identity is the authenticated principal of an HTTP request. Groups are its
// IN groups (what it may see); OutGroups are where its uploads and new
// missions go.
type identity struct {
	Username  string
	Groups    []string
	OutGroups []string
	Admin     bool
	Anonymous bool
	Session   bool // authenticated via admin console cookie
}

// Handler returns the HTTP handler. tlsListener reports whether it is served
// over TLS; over plaintext the admin console is only reachable from loopback.
func (s *Server) Handler(tlsListener bool) http.Handler {
	if s.sessions == nil {
		s.sessions = map[string]session{}
	}
	mux := http.NewServeMux()
	s.martiRoutes(mux)
	s.oauthRoutes(mux)
	s.adminRoutes(mux)
	s.groupRoutes(mux)
	s.channelRoutes(mux)
	s.sendRoutes(mux)

	sub, _ := fs.Sub(staticFS, "static")
	files := http.StripPrefix("/admin/", http.FileServerFS(sub))
	mux.HandleFunc("GET /admin/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() { s.logRequest(r, rec, time.Since(start)) }()

		rec.Header().Set("X-Content-Type-Options", "nosniff")
		if !tlsListener && isAdminPath(r.URL.Path) && !isLoopback(r.RemoteAddr) {
			http.Error(rec, "the admin console is only available over HTTPS (port 8443) or from localhost", http.StatusForbidden)
			return
		}
		if !s.rewriteMissionGUID(r) {
			http.Error(rec, "mission not found", http.StatusNotFound)
			return
		}
		mux.ServeHTTP(rec, r)
	})
}

// rewriteMissionGUID maps /Marti/api/missions/guid/{guid}/... onto the
// name-based routes, which newer clients use interchangeably. It returns
// false for an unknown GUID.
func (s *Server) rewriteMissionGUID(r *http.Request) bool {
	const prefix = "/Marti/api/missions/guid/"
	if !strings.HasPrefix(r.URL.Path, prefix) || s.Missions == nil {
		return true
	}
	guid, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, prefix), "/")
	name, ok := s.Missions.NameByGUID(guid)
	if !ok {
		return false
	}
	if rest != "" {
		rest = "/" + rest
	}
	r.URL.Path = "/Marti/api/missions/" + name + rest
	r.URL.RawPath = "/Marti/api/missions/" + url.PathEscape(name) + rest
	return true
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// redactedQuery hides secrets that clients pass as query parameters.
func redactedQuery(q url.Values) string {
	for _, k := range []string{"password", "token"} {
		if q.Has(k) {
			q.Set(k, "REDACTED")
		}
	}
	return q.Encode()
}

// logRequest records every HTTP request at debug level; this is the main
// tool for diagnosing client compatibility (log_level: debug).
func (s *Server) logRequest(r *http.Request, rec *statusRecorder, d time.Duration) {
	if !s.Log.Enabled(r.Context(), slog.LevelDebug) {
		return
	}
	who := ""
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		who = "cert:" + r.TLS.PeerCertificates[0].Subject.CommonName
	} else if u, _, ok := r.BasicAuth(); ok {
		who = "basic:" + u
	} else if bearerToken(r) != "" {
		who = "bearer"
	}
	s.Log.Debug("http", "method", r.Method, "path", r.URL.Path, "query", redactedQuery(r.URL.Query()),
		"status", rec.status, "bytes", rec.bytes, "ms", d.Milliseconds(), "remote", r.RemoteAddr,
		"who", who, "agent", r.UserAgent(), "accept", r.Header.Get("Accept"))
}

func isAdminPath(p string) bool { return p == "/" || len(p) >= 6 && p[:6] == "/admin" }

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// identify resolves the caller from (in order) a verified client certificate,
// HTTP basic auth, or an admin session cookie. It returns nil when the request
// is unauthenticated and anonymous access is not permitted.
func (s *Server) identify(r *http.Request) *identity {
	if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
		leaf := r.TLS.PeerCertificates[0]
		if s.Store.IsRevoked(pki.SerialHex(leaf)) {
			return nil
		}
		return s.userIdentity(leaf.Subject.CommonName)
	}
	if id := s.bearerIdentity(r); id != nil {
		return id
	}
	if name, pw, ok := r.BasicAuth(); ok {
		u, ok := s.Store.Authenticate(name, pw)
		if !ok {
			return nil
		}
		return s.fromUser(u)
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessMu.Lock()
		sess, ok := s.sessions[c.Value]
		if ok && time.Now().After(sess.expires) {
			delete(s.sessions, c.Value)
			ok = false
		}
		s.sessMu.Unlock()
		// Cookie auth on state-changing requests needs the custom header,
		// which cross-site pages cannot send without a CORS preflight.
		if ok && (safeMethod(r.Method) || r.Header.Get(csrfHeader) != "") {
			if id := s.userIdentity(sess.username); id != nil {
				id.Session = true
				return id
			}
		}
	}
	if r.TLS == nil && s.Cfg.AllowAnonymousHTTP {
		anon := []string{s.Cfg.AnonymousGroup}
		return &identity{Username: "anonymous", Groups: anon, OutGroups: anon, Anonymous: true}
	}
	return nil
}

func safeMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead }

// userIdentity builds an identity for a certificate CN or session user. CNs
// signed by our CA that are not in the user table get the anonymous group.
func (s *Server) userIdentity(name string) *identity {
	if u, ok := s.Store.User(name); ok {
		return s.fromUser(u)
	}
	anon := []string{s.Cfg.AnonymousGroup}
	return &identity{Username: name, Groups: anon, OutGroups: anon}
}

func (s *Server) fromUser(u *store.User) *identity {
	in, out := u.Effective(s.Cfg.AnonymousGroup)
	return &identity{Username: u.Username, Groups: in, OutGroups: out, Admin: u.Admin}
}

func (id *identity) canSee(groups []string) bool {
	if id.Admin {
		return true
	}
	for _, g := range groups {
		if slices.Contains(id.Groups, g) {
			return true
		}
	}
	return false
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, id *identity)

// authed requires an identity (anonymous allowed where configured).
func (s *Server) authed(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := s.identify(r)
		if id == nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="EdgeTAK"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		h(w, r, id)
	}
}

// named requires a non-anonymous identity.
func (s *Server) named(h handlerFunc) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if id.Anonymous {
			w.Header().Set("WWW-Authenticate", `Basic realm="EdgeTAK"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		h(w, r, id)
	})
}

// admin requires an administrator.
func (s *Server) admin(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := s.identify(r)
		if id == nil || id.Anonymous {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		if !id.Admin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "administrator required"})
			return
		}
		h(w, r, id)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	enc.Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// baseURL is the scheme://host:port the client used to reach us.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// martiEnvelope wraps data in the official server's API response envelope.
func (s *Server) martiEnvelope(typ string, data any) map[string]any {
	return map[string]any{"version": "3", "type": typ, "data": data, "nodeId": s.NodeID}
}
