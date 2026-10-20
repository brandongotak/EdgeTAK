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
	"slices"
	"sync"
	"time"

	"github.com/edgetak/edgetak/internal/config"
	"github.com/edgetak/edgetak/internal/pki"
	"github.com/edgetak/edgetak/internal/router"
	"github.com/edgetak/edgetak/internal/store"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	Cfg        config.Config
	Store      *store.Store
	CA         *pki.CA
	Router     *router.Router
	Log        *slog.Logger
	PackageDir string
	NodeID     string
	Version    string
	Started    time.Time
	// Addrs maps listener names (tcp, tls, https, enroll, http) to their
	// bound addresses. It is set before serving starts and not modified.
	Addrs map[string]string

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

// identity is the authenticated principal of an HTTP request.
type identity struct {
	Username  string
	Groups    []string
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
	s.adminRoutes(mux)

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
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !tlsListener && isAdminPath(r.URL.Path) && !isLoopback(r.RemoteAddr) {
			http.Error(w, "the admin console is only available over HTTPS (port 8443) or from localhost", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
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
		return &identity{Username: "anonymous", Groups: []string{s.Cfg.AnonymousGroup}, Anonymous: true}
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
	return &identity{Username: name, Groups: []string{s.Cfg.AnonymousGroup}}
}

func (s *Server) fromUser(u *store.User) *identity {
	groups := u.Groups
	if len(groups) == 0 {
		groups = []string{s.Cfg.AnonymousGroup}
	}
	return &identity{Username: u.Username, Groups: groups, Admin: u.Admin}
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
