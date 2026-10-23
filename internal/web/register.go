package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Self-registration. People join through an invite link (account created at
// once, in the invite's groups) or, depending on the registration mode, by
// requesting an account (an admin approves it) or by open sign-up (default
// groups). The public page is /register on the HTTPS listeners.

func (s *Server) registerRoutes(mux *http.ServeMux) {
	page := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, staticSub(), "register.html")
	}
	mux.HandleFunc("GET /register", page)
	mux.HandleFunc("GET /register/{$}", page)
	mux.HandleFunc("GET /api/register/info", s.registerInfo)
	mux.HandleFunc("POST /api/register", s.register)
	// After registering, users fetch their own connection package with their new credentials.
	mux.HandleFunc("GET /api/register/package", s.named(s.ownPackage))

	mux.HandleFunc("GET /admin/api/registration", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, map[string]any{
			"settings": s.Store.Registration(), "invites": s.inviteViews(), "requests": s.Store.Requests(),
			"register_url": s.registerURL(""),
		})
	}))
	mux.HandleFunc("PUT /admin/api/registration", s.admin(s.setRegistration))
	mux.HandleFunc("POST /admin/api/registration/invites", s.admin(s.createInvite))
	mux.HandleFunc("DELETE /admin/api/registration/invites/{code}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if err := s.Store.RevokeInvite(r.PathValue("code")); err != nil {
			jsonError(w, http.StatusNotFound, "invite not found")
			return
		}
		s.Log.Info("invite revoked", "admin", id.Username)
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /admin/api/registration/requests/{id}/approve", s.admin(s.approveRequest))
	mux.HandleFunc("DELETE /admin/api/registration/requests/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		req, err := s.Store.TakeRequest(r.PathValue("id"))
		if err != nil {
			jsonError(w, http.StatusNotFound, "request not found")
			return
		}
		s.Log.Info("sign-up rejected", "admin", id.Username, "user", req.Username)
		w.WriteHeader(http.StatusNoContent)
	}))
}

// registerURL is the public sign-up page, on the enrollment port (which has
// the publicly trusted certificate when Let's Encrypt is on).
func (s *Server) registerURL(code string) string {
	_, port, err := net.SplitHostPort(s.Addrs["enroll"])
	if err != nil {
		if _, port, err = net.SplitHostPort(s.Addrs["https"]); err != nil {
			return ""
		}
	}
	u := "https://" + net.JoinHostPort(s.Cfg.PublicHost, port) + "/register"
	if code != "" {
		u += "?invite=" + code
	}
	return u
}

type inviteView struct {
	*store.Invite
	URL    string `json:"url"`
	Usable bool   `json:"usable"`
}

func (s *Server) inviteViews() []inviteView {
	out := []inviteView{}
	now := time.Now()
	for _, i := range s.Store.Invites() {
		out = append(out, inviteView{Invite: i, URL: s.registerURL(i.Code), Usable: i.Usable(now)})
	}
	return out
}

// ---- public ----

// signedUp is the answer to a sign-up that made an account: with a QR code
// that sets up one device, so the new user's phone connects in one scan.
func (s *Server) signedUp(r *http.Request, username string) map[string]any {
	out := map[string]any{"status": "created", "username": username}
	host := s.qrHost(r)
	if code, c, err := s.newEnrollCode(username, 24, true, false, host, "sign-up"); err == nil {
		out["qr_link"], out["qr_expires"] = s.qrLink(host, username, code), c.Expires
	}
	return out
}

func (s *Server) registerInfo(w http.ResponseWriter, r *http.Request) {
	reg := s.Store.Registration()
	info := map[string]any{
		"server_name": s.Cfg.ServerName, "host": s.Cfg.PublicHost, "mode": reg.Mode,
		"quick_connect": s.Public.Info()["mode"] != "off", "streaming_port": port(s.Addrs["tls"]),
	}
	if code := r.URL.Query().Get("invite"); code != "" {
		inv, ok := s.Store.Invite(code)
		valid := ok && inv.Usable(time.Now())
		entry := map[string]any{"valid": valid}
		if valid {
			entry["label"], entry["groups"] = inv.Label, inv.Groups
			if inv.Event != "" {
				entry["event_ends"] = inv.Expires // the account is erased then
			}
		}
		info["invite"] = entry
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !s.signupLimiter.allow(clientIP(r)) {
		jsonError(w, http.StatusTooManyRequests, "too many attempts; try again later")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Invite   string `json:"invite"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !usernameRE.MatchString(req.Username) {
		jsonError(w, http.StatusBadRequest, "usernames are 1-64 characters: letters, digits, . _ @ -")
		return
	}
	if len(req.Password) < 8 {
		jsonError(w, http.StatusBadRequest, "the password must be at least 8 characters")
		return
	}
	hash, err := store.HashPassword(req.Password)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "internal error")
		return
	}
	reg := s.Store.Registration()
	remote := clientIP(r)

	switch {
	case req.Invite != "":
		u, err := s.Store.RegisterWithInvite(req.Invite, req.Username, hash)
		if s.registrationError(w, err) {
			return
		}
		// A team signing up behind one NAT address shouldn't run into the limit.
		s.signupLimiter.refund(remote)
		s.Log.Info("user registered with invite", "user", u.Username, "groups", u.Groups, "remote", remote)
		s.notify(notice{Kind: "signup", Title: "New account", Username: u.Username, Text: u.Username + " signed up with an invite."})
		writeJSON(w, http.StatusCreated, s.signedUp(r, u.Username))
	case reg.Mode == "open":
		u, err := s.Store.CreateUserWithHash(req.Username, hash, reg.DefaultGroups)
		if s.registrationError(w, err) {
			return
		}
		s.Log.Info("user registered (open sign-up)", "user", u.Username, "groups", u.Groups, "remote", remote)
		s.notify(notice{Kind: "signup", Title: "New account", Username: u.Username, Text: u.Username + " signed up."})
		writeJSON(w, http.StatusCreated, s.signedUp(r, u.Username))
	case reg.Mode == "approval":
		err := s.Store.AddRequest(store.SignupRequest{ID: uuid(), Username: req.Username, PasswordHash: hash,
			Note: truncate(strings.TrimSpace(req.Note), 300), Remote: remote, Created: time.Now().UTC()})
		if s.registrationError(w, err) {
			return
		}
		s.Log.Info("sign-up request received", "user", req.Username, "remote", remote)
		s.notify(notice{Kind: "signup", Title: "Sign-up request", Text: req.Username + " asked for an account; approve or reject it in the console (People → Sign-ups).", Groups: []string{}})
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending", "username": req.Username})
	default:
		jsonError(w, http.StatusForbidden, "registration is closed; ask an administrator for an invite link")
	}
}

// registrationError writes the response for a failed sign-up; it reports
// whether there was an error.
func (s *Server) registrationError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrExists):
		jsonError(w, http.StatusConflict, "that username is taken")
	case errors.Is(err, store.ErrInviteUnusable):
		jsonError(w, http.StatusGone, "this invite link has expired or been used up")
	default:
		s.Log.Error("registration failed", "err", err)
		jsonError(w, http.StatusInternalServerError, "registration failed")
	}
	return true
}

// ownPackage gives a registered user their connection package.
func (s *Server) ownPackage(w http.ResponseWriter, r *http.Request, id *identity) {
	host := s.Cfg.PublicHost
	var data []byte
	var err error
	mode := r.URL.Query().Get("mode")
	switch mode {
	case "itak":
		data, err = s.buildITAKPackage(host, id.Username, id.Username)
	default:
		mode = "ssl"
		data, err = s.buildConnectionPackage("ssl", host, id.Username)
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitize(s.Cfg.ServerName)+"-"+sanitize(id.Username)+"-"+mode+`.zip"`)
	w.Write(data)
}

// ---- admin ----

func (s *Server) setRegistration(w http.ResponseWriter, r *http.Request, id *identity) {
	var req store.RegistrationSettings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Mode != "off" && req.Mode != "approval" && req.Mode != "open" {
		jsonError(w, http.StatusBadRequest, "mode must be off, approval or open")
		return
	}
	req.DefaultGroups = cleanGroups(req.DefaultGroups)
	if err := s.Store.SetRegistration(req); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("registration settings changed", "admin", id.Username, "mode", req.Mode, "default_groups", req.DefaultGroups)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct {
		Label       string   `json:"label"`
		Groups      []string `json:"groups"`
		MaxUses     int      `json:"max_uses"`
		ExpiresDays int      `json:"expires_days"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.MaxUses < 0 || req.ExpiresDays < 0 {
		jsonError(w, http.StatusBadRequest, "uses and expiry cannot be negative")
		return
	}
	inv := store.Invite{Code: randomCode(), Label: truncate(strings.TrimSpace(req.Label), 100), Groups: cleanGroups(req.Groups),
		MaxUses: req.MaxUses, Created: time.Now().UTC(), CreatedBy: id.Username}
	if req.ExpiresDays > 0 {
		inv.Expires = inv.Created.Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
	}
	created, err := s.Store.CreateInvite(inv)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("invite created", "admin", id.Username, "label", inv.Label, "groups", inv.Groups, "max_uses", inv.MaxUses)
	writeJSON(w, http.StatusCreated, inviteView{Invite: created, URL: s.registerURL(created.Code), Usable: true})
}

func (s *Server) approveRequest(w http.ResponseWriter, r *http.Request, id *identity) {
	var body struct {
		Groups *[]string `json:"groups"` // nil = registration default groups
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body)
	req, err := s.Store.TakeRequest(r.PathValue("id"))
	if err != nil {
		jsonError(w, http.StatusNotFound, "request not found")
		return
	}
	groups := s.Store.Registration().DefaultGroups
	if body.Groups != nil {
		groups = cleanGroups(*body.Groups)
	}
	u, err := s.Store.CreateUserWithHash(req.Username, req.PasswordHash, groups)
	if err != nil {
		jsonError(w, http.StatusConflict, "could not create the account: "+err.Error())
		return
	}
	s.Log.Info("sign-up approved", "admin", id.Username, "user", u.Username, "groups", u.Groups)
	writeJSON(w, http.StatusOK, newUserView(u, 0))
}

// ---- helpers ----

func randomCode() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func port(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

// rateLimiter allows a burst of attempts per key, refilling slowly.
type rateLimiter struct {
	mu      sync.Mutex
	burst   float64
	perSec  float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(burst int, per time.Duration) *rateLimiter {
	return &rateLimiter{burst: float64(burst), perSec: float64(burst) / per.Seconds(), buckets: map[string]*bucket{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) > 10000 { // bound memory under address churn
			l.buckets = map[string]*bucket{}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSec)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// refund gives back an attempt taken by allow.
func (l *rateLimiter) refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		b.tokens = min(l.burst, b.tokens+1)
	}
}
