package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Account self-service: every user (not only admins) can sign in at
// /account to change their password, get connection packages, and see and
// revoke their own devices' certificates.
//
// Account sessions are separate from admin console sessions: their cookie
// is scoped to /account and only accepted by the /account/api routes, so
// signing in here never grants the admin API or the TAK (Marti) API.

const (
	accountCookie = "edgetak_account"
	accountTTL    = 12 * time.Hour
)

type accountState struct {
	mu       sync.Mutex
	sessions map[string]session
	limiter  *rateLimiter // failed sign-ins per address
}

func isAccountPath(p string) bool { return p == "/account" || strings.HasPrefix(p, "/account/") }

func (s *Server) accountRoutes(mux *http.ServeMux) {
	s.account.mu.Lock()
	if s.account.sessions == nil {
		s.account.sessions = map[string]session{}
		s.account.limiter = newRateLimiter(10, 15*time.Minute)
	}
	s.account.mu.Unlock()

	serve := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFileFS(w, r, staticSub(), name)
		}
	}
	// One sign-in page for everyone, at the root of the HTTPS ports.
	mux.HandleFunc("GET /{$}", serve("login.html"))
	mux.HandleFunc("GET /account", serve("account.html"))
	mux.HandleFunc("GET /account/{$}", serve("account.html"))
	mux.HandleFunc("GET /account/api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"server_name": s.Cfg.ServerName, "host": s.Cfg.PublicHost,
			"quick_connect": s.Public.Info()["mode"] != "off", "streaming_port": port(s.Addrs["tls"]),
			"signup": s.Store.Registration().Mode != "off"})
	})
	mux.HandleFunc("POST /account/api/login", s.signIn)
	mux.HandleFunc("POST /account/api/logout", s.signOut)
	mux.HandleFunc("GET /account/api/me", s.accountAuth(s.accountMe))
	mux.HandleFunc("POST /account/api/password", s.accountAuth(s.accountPassword))
	mux.HandleFunc("GET /account/api/package", s.accountAuth(func(w http.ResponseWriter, r *http.Request, u *store.User, _ string) {
		s.ownPackage(w, r, s.fromUser(u))
	}))
	mux.HandleFunc("POST /account/api/certs/{serial}/revoke", s.accountAuth(s.accountRevoke))
}

// accountAuth requires an account session. State-changing requests also need
// the custom header, which cross-site pages cannot send.
func (s *Server) accountAuth(h func(w http.ResponseWriter, r *http.Request, u *store.User, token string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(accountCookie)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in first"})
			return
		}
		s.account.mu.Lock()
		sess, ok := s.account.sessions[c.Value]
		if ok && time.Now().After(sess.expires) {
			delete(s.account.sessions, c.Value)
			ok = false
		}
		s.account.mu.Unlock()
		u, exists := s.Store.User(sess.username)
		if !ok || !exists {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in first"})
			return
		}
		if !safeMethod(r.Method) && r.Header.Get(csrfHeader) == "" {
			jsonError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
			return
		}
		h(w, r, u, c.Value)
	}
}

// signIn is the one sign-in for everyone (the page at /, the console and
// the account page). Every user gets an account session; administrators
// also get an admin console session.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password, Code string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	ip := clientIP(r)
	if !s.account.limiter.allow(ip) {
		jsonError(w, http.StatusTooManyRequests, "too many failed sign-ins; wait a few minutes")
		return
	}
	u, ok := s.Store.Authenticate(req.Username, req.Password)
	if !ok {
		time.Sleep(500 * time.Millisecond)
		s.Log.Warn("sign-in failed", "user", req.Username, "remote", r.RemoteAddr)
		jsonError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	if u.TwoFactor() && !s.secondFactor(w, r, u.Username, req.Code) {
		return
	}
	s.account.limiter.refund(ip)
	now := time.Now()
	token := randomToken(32)
	s.account.mu.Lock()
	for k, v := range s.account.sessions {
		if now.After(v.expires) {
			delete(s.account.sessions, k)
		}
	}
	s.account.sessions[token] = session{username: u.Username, expires: now.Add(accountTTL)}
	s.account.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: accountCookie, Value: token, Path: "/account", HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode, MaxAge: int(accountTTL.Seconds()),
	})
	if u.Admin {
		token := randomToken(32)
		s.sessMu.Lock()
		for k, v := range s.sessions {
			if now.After(v.expires) {
				delete(s.sessions, k)
			}
		}
		s.sessions[token] = session{username: u.Username, expires: now.Add(sessionTTL)}
		s.sessMu.Unlock()
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil,
			SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
		})
	}
	s.Log.Info("sign-in", "user", u.Username, "admin", u.Admin, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{"username": u.Username, "admin": u.Admin})
}

// signOut ends both the account and the admin console session.
func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(accountCookie); err == nil {
		s.account.mu.Lock()
		delete(s.account.sessions, c.Value)
		s.account.mu.Unlock()
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessMu.Lock()
		delete(s.sessions, c.Value)
		s.sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: accountCookie, Value: "", Path: "/account", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) accountMe(w http.ResponseWriter, r *http.Request, u *store.User, _ string) {
	in, out := u.Effective(s.Cfg.AnonymousGroup)
	devices := []clientView{}
	for _, c := range s.Router.Clients() {
		if c.Username != u.Username {
			continue
		}
		uid, cs := c.Identity()
		v := clientView{ID: c.ID, UID: uid, Callsign: cs, Transport: c.Transport, ConnectedAt: c.ConnectedAt, LastSeen: c.LastSeen()}
		if sa := c.LastSA(); sa != nil {
			fillSA(&v, sa)
		}
		devices = append(devices, v)
	}
	certs := []clientCertView{}
	rep := s.certReport()
	for _, c := range rep.Certs {
		if c.CommonName == u.Username {
			certs = append(certs, c)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username": u.Username, "admin": u.Admin, "two_factor": u.TwoFactor(), "groups_in": in, "groups_out": out,
		"devices": devices, "certs": certs, "warn_days": rep.WarnDays,
	})
}

func (s *Server) accountPassword(w http.ResponseWriter, r *http.Request, u *store.User, token string) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if _, ok := s.Store.Authenticate(u.Username, req.Current); !ok {
		time.Sleep(500 * time.Millisecond)
		jsonError(w, http.StatusForbidden, "your current password is wrong")
		return
	}
	if len(req.New) < 8 {
		jsonError(w, http.StatusBadRequest, "the new password must be at least 8 characters")
		return
	}
	if req.New == req.Current {
		jsonError(w, http.StatusBadRequest, "choose a password different from your current one")
		return
	}
	if err := s.Store.SetPassword(u.Username, req.New); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Sign out everywhere else: other account and admin console sessions.
	s.account.mu.Lock()
	for k, v := range s.account.sessions {
		if v.username == u.Username && k != token {
			delete(s.account.sessions, k)
		}
	}
	s.account.mu.Unlock()
	s.sessMu.Lock()
	for k, v := range s.sessions {
		if v.username == u.Username {
			delete(s.sessions, k)
		}
	}
	s.sessMu.Unlock()
	s.Log.Info("user changed their password", "user", u.Username, "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}

// accountRevoke revokes one of the user's own certificates, for a lost or
// replaced device.
func (s *Server) accountRevoke(w http.ResponseWriter, r *http.Request, u *store.User, _ string) {
	serial := r.PathValue("serial")
	own := false
	for _, c := range s.Store.Certs() {
		if c.Serial == serial && c.CommonName == u.Username {
			own = true
		}
	}
	if !own {
		jsonError(w, http.StatusNotFound, "certificate not found")
		return
	}
	if err := s.Store.RevokeCert(serial); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, c := range s.Router.Clients() {
		if c.CertSerial == serial {
			c.Close()
		}
	}
	s.Log.Info("user revoked their certificate", "user", u.Username, "serial", serial)
	w.WriteHeader(http.StatusNoContent)
}
