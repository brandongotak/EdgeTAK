package web

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/apps"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// tak.gov (Plugins tab): an admin links EdgeTAK to their tak.gov account
// with the OAuth device flow (open a link, enter a code, sign in), then
// browses the plugins tak.gov publishes for an ATAK version and installs
// them on the update server in one click. EdgeTAK keeps only the refresh
// token, which tak.gov replaces at each use; access tokens live in memory.

const (
	takGovAuthURL = "https://auth.tak.gov/auth/realms/TPC/protocol/openid-connect"
	takGovAPIURL  = "https://tak.gov/eud_api"
	takGovClient  = "tak-gov-eud"
)

type takGovState struct {
	mu      sync.Mutex
	pending *takGovPending
	access  string
	expires time.Time
}

// takGovPending is a device sign-in waiting for the admin to finish it.
type takGovPending struct {
	DeviceCode string    `json:"-"`
	UserCode   string    `json:"user_code"`
	URL        string    `json:"verification_uri"`
	URLWith    string    `json:"verification_uri_complete"`
	Expires    time.Time `json:"expires"`
	Interval   int       `json:"interval"`
	By         string    `json:"-"`
}

// takGovPlugin is a plugin as tak.gov lists it.
type takGovPlugin struct {
	Name            string `json:"display_name"`
	Package         string `json:"package_name"`
	Version         string `json:"version"`
	Revision        any    `json:"revision_code,omitempty"`
	Description     string `json:"description"`
	Platform        string `json:"platform,omitempty"`
	Type            string `json:"apk_type,omitempty"`
	OSRequirement   any    `json:"os_requirement,omitempty"`
	TakPrerequisite any    `json:"tak_prerequisite,omitempty"`
	ATAKVersion     string `json:"atak_version,omitempty"`
	APKURL          string `json:"apk_url"`
	APKHash         string `json:"apk_hash,omitempty"`
	APKSize         int64  `json:"apk_size_bytes,omitempty"`
	IconURL         string `json:"icon_url,omitempty"`
}

func (s *Server) takGovAuth() string { return firstNonEmpty(s.TakGovAuthURL, takGovAuthURL) }
func (s *Server) takGovAPI() string  { return firstNonEmpty(s.TakGovAPIURL, takGovAPIURL) }

func (s *Server) takGovHTTP() *http.Client {
	if s.TakGovClient != nil {
		return s.TakGovClient
	}
	return &http.Client{Timeout: 10 * time.Minute} // tak.gov needs HTTP/2, which Go's client speaks
}

// takGovURL reports whether u is on tak.gov's plugin API: the only place
// the account's token is sent.
func (s *Server) takGovURL(u string) bool {
	base := s.takGovAPI() + "/"
	p, err := url.Parse(u)
	return err == nil && strings.HasPrefix(u, base) && !strings.Contains(p.Path, "..")
}

func (s *Server) takGovRequest(method, u string, form url.Values) (*http.Request, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("User-Agent", "EdgeTAK/"+s.Version)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// takGovTokens posts to the token endpoint. A non-empty oauthErr is
// tak.gov's error code (authorization_pending, invalid_grant, ...).
func (s *Server) takGovTokens(form url.Values) (tok struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	ID        string `json:"id_token"`
	ExpiresIn int    `json:"expires_in"`
}, oauthErr string, err error) {
	form.Set("client_id", takGovClient)
	req, err := s.takGovRequest("POST", s.takGovAuth()+"/token", form)
	if err != nil {
		return tok, "", err
	}
	resp, err := s.takGovHTTP().Do(req)
	if err != nil {
		return tok, "", fmt.Errorf("can't reach tak.gov: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		json.Unmarshal(b, &e)
		if e.Error != "" {
			return tok, e.Error, fmt.Errorf("tak.gov: %s", firstNonEmpty(e.Description, e.Error))
		}
		return tok, "", fmt.Errorf("tak.gov answered %s", resp.Status)
	}
	if err := json.Unmarshal(b, &tok); err != nil || tok.Access == "" {
		return tok, "", errors.New("tak.gov sent no token")
	}
	return tok, "", nil
}

// takGovAccount is the account name in a token, for display (the token
// came straight from tak.gov over TLS, so its claims aren't verified).
func takGovAccount(tokens ...string) string {
	for _, t := range tokens {
		parts := strings.Split(t, ".")
		if len(parts) != 3 {
			continue
		}
		b, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var c struct {
			Email string `json:"email"`
			User  string `json:"preferred_username"`
			Name  string `json:"name"`
		}
		if json.Unmarshal(b, &c) == nil {
			if v := firstNonEmpty(c.Email, c.User, c.Name); v != "" {
				return v
			}
		}
	}
	return ""
}

// takGovToken is an access token for the linked account, refreshing it
// (and saving tak.gov's new refresh token) when it is about to expire.
func (s *Server) takGovToken() (string, error) {
	s.takgov.mu.Lock()
	defer s.takgov.mu.Unlock()
	if s.takgov.access != "" && time.Now().Before(s.takgov.expires) {
		return s.takgov.access, nil
	}
	link := s.Store.TakGov()
	if link.RefreshToken == "" {
		return "", errors.New("EdgeTAK isn't linked to a tak.gov account")
	}
	tok, oauthErr, err := s.takGovTokens(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {link.RefreshToken}})
	if err != nil {
		if oauthErr == "invalid_grant" {
			// Signed out at tak.gov, or unused for too long: link again.
			s.Store.UpdateTakGov(func(t *store.TakGovLink) { t.RefreshToken = "" })
			s.Log.Warn("tak.gov link expired", "account", link.Account)
			return "", errors.New("the tak.gov link has expired; link the account again")
		}
		return "", err
	}
	s.takgov.access, s.takgov.expires = tok.Access, takGovExpiry(tok.ExpiresIn)
	if tok.Refresh != "" && tok.Refresh != link.RefreshToken {
		s.Store.UpdateTakGov(func(t *store.TakGovLink) { t.RefreshToken = tok.Refresh })
	}
	return tok.Access, nil
}

// takGovExpiry is when to stop using an access token: 30 s early.
func takGovExpiry(seconds int) time.Time {
	return time.Now().Add(time.Duration(seconds-30) * time.Second)
}

// takGovGet fetches a tak.gov plugin API URL with the account's token.
func (s *Server) takGovGet(u string) (*http.Response, error) {
	if !s.takGovURL(u) {
		return nil, errors.New("not a tak.gov plugin address")
	}
	for try := 0; ; try++ {
		token, err := s.takGovToken()
		if err != nil {
			return nil, err
		}
		req, err := s.takGovRequest("GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "*/*")
		// Redirects (to file storage) don't carry the token.
		resp, err := s.takGovHTTP().Do(req)
		if err != nil {
			return nil, fmt.Errorf("can't reach tak.gov: %w", err)
		}
		if resp.StatusCode != http.StatusUnauthorized || try > 0 {
			return resp, nil
		}
		resp.Body.Close()
		s.takgov.mu.Lock()
		if s.takgov.access == token {
			s.takgov.access = "" // refused: get a fresh one
		}
		s.takgov.mu.Unlock()
	}
}

func (s *Server) takGovView() map[string]any {
	link := s.Store.TakGov()
	v := map[string]any{"linked": link.RefreshToken != "", "account": link.Account, "linked_by": link.LinkedBy,
		"product": firstNonEmpty(link.Product, "ATAK-CIV"), "product_version": firstNonEmpty(link.Version, s.takGovDefaultVersion())}
	if !link.Linked.IsZero() {
		v["linked_at"] = link.Linked
	}
	s.takgov.mu.Lock()
	if p := s.takgov.pending; p != nil && time.Now().Before(p.Expires) {
		v["pending"] = p
	}
	s.takgov.mu.Unlock()
	return v
}

// takGovDefaultVersion is the newest ATAK version the update server
// already has plugins for, or 5.6.0.
func (s *Server) takGovDefaultVersion() string {
	best := ""
	for _, a := range s.Store.Apps() {
		if _, v, ok := strings.Cut(a.PluginAPI, "@"); ok {
			v = strings.TrimSuffix(strings.TrimSuffix(v, ".CIV"), ".MIL")
			if best == "" || apps.CompareVersions(v, best) > 0 {
				best = v
			}
		}
	}
	return firstNonEmpty(best, "5.6.0")
}

var takGovFilenameRE = regexp.MustCompile(`^[A-Za-z0-9._ ()+-]{1,200}$`)

func (s *Server) takGovRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/takgov", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.takGovView())
	}))

	// Start linking: tak.gov gives a code for the admin to enter.
	mux.HandleFunc("POST /admin/api/takgov/link", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		req, err := s.takGovRequest("POST", s.takGovAuth()+"/auth/device",
			url.Values{"client_id": {takGovClient}, "scope": {"openid offline_access email profile"}})
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp, err := s.takGovHTTP().Do(req)
		if err != nil {
			jsonError(w, http.StatusBadGateway, "can't reach tak.gov: "+err.Error())
			return
		}
		defer resp.Body.Close()
		var d struct {
			DeviceCode string `json:"device_code"`
			UserCode   string `json:"user_code"`
			URL        string `json:"verification_uri"`
			URLWith    string `json:"verification_uri_complete"`
			ExpiresIn  int    `json:"expires_in"`
			Interval   int    `json:"interval"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d) != nil || d.DeviceCode == "" {
			jsonError(w, http.StatusBadGateway, "tak.gov didn't start the sign-in ("+resp.Status+")")
			return
		}
		p := &takGovPending{DeviceCode: d.DeviceCode, UserCode: d.UserCode, URL: d.URL, URLWith: firstNonEmpty(d.URLWith, d.URL),
			Expires: time.Now().Add(time.Duration(max(d.ExpiresIn, 60)) * time.Second), Interval: max(d.Interval, 5), By: id.Username}
		s.takgov.mu.Lock()
		s.takgov.pending = p
		s.takgov.mu.Unlock()
		writeJSON(w, http.StatusOK, s.takGovView())
	}))

	// Check whether the admin has finished signing in at tak.gov.
	mux.HandleFunc("POST /admin/api/takgov/poll", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		s.takgov.mu.Lock()
		p := s.takgov.pending
		s.takgov.mu.Unlock()
		view := func(state string) {
			v := s.takGovView()
			v["state"] = state
			writeJSON(w, http.StatusOK, v)
		}
		if p == nil || time.Now().After(p.Expires) {
			view("expired")
			return
		}
		tok, oauthErr, err := s.takGovTokens(url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {p.DeviceCode}})
		switch {
		case oauthErr == "authorization_pending" || oauthErr == "slow_down":
			view("pending")
			return
		case err != nil:
			s.takgov.mu.Lock()
			if s.takgov.pending == p {
				s.takgov.pending = nil
			}
			s.takgov.mu.Unlock()
			if oauthErr == "access_denied" {
				view("denied")
			} else if oauthErr == "expired_token" {
				view("expired")
			} else {
				jsonError(w, http.StatusBadGateway, err.Error())
			}
			return
		case tok.Refresh == "":
			jsonError(w, http.StatusBadGateway, "tak.gov sent no refresh token")
			return
		}
		account := takGovAccount(tok.ID, tok.Access)
		if err := s.Store.UpdateTakGov(func(t *store.TakGovLink) {
			t.RefreshToken, t.Account, t.Linked, t.LinkedBy = tok.Refresh, account, time.Now().UTC(), p.By
		}); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.takgov.mu.Lock()
		s.takgov.pending = nil
		s.takgov.access, s.takgov.expires = tok.Access, takGovExpiry(tok.ExpiresIn)
		s.takgov.mu.Unlock()
		s.Log.Info("tak.gov account linked", "admin", p.By, "account", account)
		view("linked")
	}))

	mux.HandleFunc("DELETE /admin/api/takgov", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		link := s.Store.TakGov()
		// Tell tak.gov too, so the token is dead there (best effort).
		if link.RefreshToken != "" {
			if req, err := s.takGovRequest("POST", s.takGovAuth()+"/logout",
				url.Values{"client_id": {takGovClient}, "refresh_token": {link.RefreshToken}}); err == nil {
				if resp, err := s.takGovHTTP().Do(req); err == nil {
					resp.Body.Close()
				}
			}
		}
		s.Store.UpdateTakGov(func(t *store.TakGovLink) { *t = store.TakGovLink{Product: t.Product, Version: t.Version} })
		s.takgov.mu.Lock()
		s.takgov.access, s.takgov.pending = "", nil
		s.takgov.mu.Unlock()
		s.Log.Info("tak.gov account unlinked", "admin", id.Username, "account", link.Account)
		writeJSON(w, http.StatusOK, s.takGovView())
	}))

	// The plugins tak.gov offers for a product and version, each marked
	// with whether the update server already has it.
	mux.HandleFunc("GET /admin/api/takgov/plugins", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		product := strings.TrimSpace(r.URL.Query().Get("product"))
		version := strings.TrimSpace(r.URL.Query().Get("product_version"))
		if product == "" || version == "" {
			jsonError(w, http.StatusBadRequest, "product and product_version are required")
			return
		}
		q := url.Values{"product": {product}, "product_version": {version}}
		resp, err := s.takGovGet(s.takGovAPI() + "/software/v1/plugins?" + q.Encode())
		if err != nil {
			jsonError(w, http.StatusBadGateway, err.Error())
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if resp.StatusCode != http.StatusOK {
			var e struct {
				Description string `json:"error_description"`
			}
			json.Unmarshal(b, &e)
			jsonError(w, http.StatusBadGateway, "tak.gov: "+firstNonEmpty(e.Description, resp.Status))
			return
		}
		plugins, err := decodeTakGovPlugins(b)
		if err != nil {
			jsonError(w, http.StatusBadGateway, "tak.gov sent a plugin list EdgeTAK can't read")
			return
		}
		s.Store.UpdateTakGov(func(t *store.TakGovLink) { t.Product, t.Version = product, version })
		have := map[string]bool{}
		for _, a := range s.Store.Apps() {
			have[a.Package+"\x00"+a.Version] = true
			have["sha\x00"+a.SHA256] = true
		}
		type view struct {
			takGovPlugin
			Installed bool `json:"installed"`
		}
		out := []view{}
		for _, p := range plugins {
			out = append(out, view{p, have[p.Package+"\x00"+p.Version] || (p.APKHash != "" && have["sha\x00"+strings.ToLower(p.APKHash)])})
		}
		writeJSON(w, http.StatusOK, map[string]any{"product": product, "product_version": version, "plugins": out})
	}))

	mux.HandleFunc("GET /admin/api/takgov/icon", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		resp, err := s.takGovGet(r.URL.Query().Get("url"))
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxIcon+1))
		ct := http.DetectContentType(b)
		if resp.StatusCode != http.StatusOK || len(b) > maxIcon || !strings.HasPrefix(ct, "image/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Write(b)
	}))

	mux.HandleFunc("POST /admin/api/takgov/install", s.admin(s.takGovInstall))
}

// decodeTakGovPlugins reads tak.gov's list: an array, or an object holding one.
func decodeTakGovPlugins(b []byte) ([]takGovPlugin, error) {
	var list []takGovPlugin
	if err := json.Unmarshal(b, &list); err == nil {
		return list, nil
	}
	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(b, &wrapped); err != nil {
		return nil, err
	}
	for _, k := range []string{"plugins", "results", "data", "items"} {
		if raw, ok := wrapped[k]; ok && json.Unmarshal(raw, &list) == nil {
			return list, nil
		}
	}
	return nil, errors.New("no plugin list")
}

// takGovInstall downloads a plugin from tak.gov onto the update server.
func (s *Server) takGovInstall(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct {
		APKURL  string `json:"apk_url"`
		APKHash string `json:"apk_hash"`
		IconURL string `json:"icon_url"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || req.APKURL == "" {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !s.takGovURL(req.APKURL) {
		jsonError(w, http.StatusBadRequest, "not a tak.gov plugin address")
		return
	}
	resp, err := s.takGovGet(req.APKURL)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		jsonError(w, http.StatusBadGateway, "tak.gov answered "+resp.Status+" for the APK")
		return
	}
	name := path.Base(resp.Request.URL.Path)
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = filepath.Base(strings.ReplaceAll(params["filename"], `\`, "/"))
	}
	if !takGovFilenameRE.MatchString(name) || !strings.HasSuffix(strings.ToLower(name), ".apk") {
		name = firstNonEmpty(strings.Map(func(r rune) rune {
			if strings.ContainsRune("/\\:*?\"<>|", r) {
				return '_'
			}
			return r
		}, req.Name), "plugin") + ".apk"
	}
	if err := os.MkdirAll(s.appsDir(), 0o750); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Check the download against tak.gov's hash before adding it.
	tmp, err := os.CreateTemp(s.appsDir(), ".takgov-*")
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxAPK+1)); err != nil {
		jsonError(w, http.StatusBadGateway, "download from tak.gov failed: "+err.Error())
		return
	}
	if want := strings.ToLower(strings.TrimSpace(req.APKHash)); len(want) == 64 && want != hex.EncodeToString(h.Sum(nil)) {
		s.Log.Warn("tak.gov download doesn't match its hash", "admin", id.Username, "url", req.APKURL)
		jsonError(w, http.StatusBadGateway, "the download doesn't match the hash tak.gov lists; not added")
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a, err := s.addAPK(tmp, name, id)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	// tak.gov's icon, when the APK has none EdgeTAK can draw.
	if !a.HasIcon && req.IconURL != "" {
		if ir, err := s.takGovGet(req.IconURL); err == nil {
			b, _ := io.ReadAll(io.LimitReader(ir.Body, maxIcon+1))
			ir.Body.Close()
			if ir.StatusCode == http.StatusOK && len(b) <= maxIcon && http.DetectContentType(b) == "image/png" &&
				os.WriteFile(s.iconFile(*a), b, 0o640) == nil {
				a.HasIcon = true
				s.Store.SetAppIcon(a.SHA256, true)
			}
		}
	}
	s.Log.Info("plugin installed from tak.gov", "admin", id.Username, "package", a.Package, "version", a.Version)
	view := s.appsView(r)
	view["added"] = []string{a.Name + " " + a.Version}
	writeJSON(w, http.StatusOK, view)
}
