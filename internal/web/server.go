// Package web serves the Marti HTTP API used by TAK clients (enrollment, data
// packages, contacts) and the browser admin console.
package web

import (
	"context"
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

	"github.com/brandongotak/EdgeTAK/internal/audit"
	"github.com/brandongotak/EdgeTAK/internal/chatlog"
	"github.com/brandongotak/EdgeTAK/internal/config"
	"github.com/brandongotak/EdgeTAK/internal/federation"
	"github.com/brandongotak/EdgeTAK/internal/feeds"
	"github.com/brandongotak/EdgeTAK/internal/frp"
	"github.com/brandongotak/EdgeTAK/internal/health"
	"github.com/brandongotak/EdgeTAK/internal/history"
	"github.com/brandongotak/EdgeTAK/internal/maps"
	"github.com/brandongotak/EdgeTAK/internal/mediamtx"
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
	History    *history.Recorder
	Chat       *chatlog.Log
	Federation *federation.Manager
	// PlainTCP turns the plain TCP streaming port on and off; nil when it
	// is turned off in the configuration (no tcp_addr, --no-plaintext).
	PlainTCP interface {
		On() bool
		Set(on bool) error
		Addr() string
	}
	// Feeds turns ADS-B and AIS data into markers (set by the app).
	Feeds *feeds.Service
	// FRP runs the FRP client for remote access (set by the app).
	FRP *frp.Manager
	// ZeroTierAPI is the local ZeroTier service's API ("" =
	// http://127.0.0.1:9993; tests use a fake).
	ZeroTierAPI string
	// FederationDial reaches other servers for federation setup (tests).
	FederationDial func(addr string) (net.Conn, error)
	MediaMTX       *mediamtx.Manager // nil unless EdgeTAK runs MediaMTX itself
	Health         *health.Sampler
	Log            *slog.Logger
	PackageDir     string
	NodeID         string
	Version        string
	Started        time.Time
	// DataDir and ConfigPath are what a backup saves; Restart stops EdgeTAK
	// so its service manager starts it again (after a restore).
	DataDir    string
	ConfigPath string
	Restart    func()
	// Maps are the offline maps (MBTiles) in <data>/maps.
	Maps *maps.Library
	// Addrs maps listener names (tcp, tls, https, enroll, http) to their
	// bound addresses. It is set before serving starts and not modified.
	Addrs map[string]string
	// TakGovAuthURL, TakGovAPIURL and TakGovClient reach tak.gov (tests
	// point them at a fake one).
	TakGovAuthURL, TakGovAPIURL string
	TakGovClient                *http.Client
	// Audit is the audit log (nil: nothing is recorded).
	Audit *audit.Log
	// WebhookClient sends webhooks (tests); default a 15 s client.
	WebhookClient *http.Client
	// SimDial connects simulated devices (tests use an in-memory network).
	SimDial func(ctx context.Context, network, addr string) (net.Conn, error)

	history       sendHistory  // admin "send to devices" log
	signupLimiter *rateLimiter // public sign-up attempts per address
	video         videoState
	selfUpdate    selfUpdateState
	retention     retentionState
	account       accountState
	lost          lostState
	fences        geofenceState
	ldap          ldapState
	takgov        takGovState
	pushed        pushedFiles
	sim           simState
	hooks         webhookState
	healthWatch   healthWatch
	guard         loginGuard

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
	s.sessMu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]session{}
	}
	if s.signupLimiter == nil {
		s.signupLimiter = newRateLimiter(10, time.Hour)
	}
	s.sessMu.Unlock()
	mux := http.NewServeMux()
	s.martiRoutes(mux)
	s.oauthRoutes(mux)
	s.adminRoutes(mux)
	s.groupRoutes(mux)
	s.channelRoutes(mux)
	s.sendRoutes(mux)
	s.registerRoutes(mux)
	s.mapRoutes(mux)
	s.historyRoutes(mux)
	s.videoRoutes(mux)
	s.updateRoutes(mux)
	s.twoFactorRoutes(mux)
	s.healthRoutes(mux)
	s.retentionRoutes(mux)
	s.certStatusRoutes(mux)
	s.chatRoutes(mux)
	s.recordingRoutes(mux)
	s.emergencyRoutes(mux)
	s.profileRoutes(mux)
	s.appRoutes(mux)
	s.takGovRoutes(mux)
	s.overlayRoutes(mux)
	s.simRoutes(mux)
	s.webhookRoutes(mux)
	s.auditRoutes(mux)
	s.scheduleRoutes(mux)
	s.loginGuardRoutes(mux)
	s.enrollCodeRoutes(mux)
	s.plainTCPRoutes(mux)
	s.zerotierRoutes(mux)
	s.feedRoutes(mux)
	s.frpRoutes(mux)
	s.brandingRoutes(mux)
	s.backupRoutes(mux)
	s.offlineMapRoutes(mux)
	s.lostContactRoutes(mux)
	s.geofenceRoutes(mux)
	s.drawingRoutes(mux)
	s.ldapRoutes(mux)
	s.eventRoutes(mux)
	s.federationRoutes(mux)
	s.accountRoutes(mux)

	mux.HandleFunc("GET /admin/i18n.js", s.i18nScript)
	files := http.StripPrefix("/admin/", http.FileServerFS(staticSub()))
	mux.HandleFunc("GET /admin/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() { s.logRequest(r, rec, time.Since(start)) }()

		rec.Header().Set("X-Content-Type-Options", "nosniff")
		// No other site may frame these pages (clickjacking), or learn console
		// addresses from links followed out of it.
		rec.Header().Set("X-Frame-Options", "DENY")
		rec.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		rec.Header().Set("Referrer-Policy", "same-origin")
		if s.AddressRefused(r.RemoteAddr) {
			http.Error(rec, "refused", http.StatusForbidden)
			return
		}
		if !tlsListener && isAdminPath(r.URL.Path) && !isLoopback(r.RemoteAddr) {
			http.Error(rec, "the admin console is only available over HTTPS (port 8443) or from localhost", http.StatusForbidden)
			return
		}
		if !tlsListener && isRegisterPath(r.URL.Path) && !isLoopback(r.RemoteAddr) {
			http.Error(rec, "sign-up is only available over HTTPS", http.StatusForbidden)
			return
		}
		if !tlsListener && isAccountPath(r.URL.Path) && !isLoopback(r.RemoteAddr) {
			http.Error(rec, "your account page is only available over HTTPS", http.StatusForbidden)
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

func isRegisterPath(p string) bool {
	return strings.HasPrefix(p, "/register") || strings.HasPrefix(p, "/api/register")
}

func staticSub() fs.FS {
	sub, _ := fs.Sub(staticFS, "static")
	return sub
}

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
	// Passwords are only accepted over TLS (or from this machine): on the
	// plain HTTP port they would cross the network in the clear.
	if name, pw, ok := r.BasicAuth(); ok && (r.TLS != nil || isLoopback(r.RemoteAddr)) {
		u, err := s.passwordSignIn(r, name, pw)
		if err != nil {
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
		if u.Disabled {
			return nil
		}
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
		// A password alone (basic auth, OAuth token, device certificate) must
		// not get around an account's two-factor login.
		if !id.Session {
			if u, ok := s.Store.User(id.Username); ok && u.TwoFactor() {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "this account uses two-factor login; sign in to the admin console"})
				return
			}
		}
		s.auditRequest(h, w, r, id)
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
