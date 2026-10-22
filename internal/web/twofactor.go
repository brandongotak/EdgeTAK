package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
	"github.com/brandongotak/EdgeTAK/internal/totp"
)

// Two-factor login for the admin console, with authenticator-app codes
// (TOTP) and single-use recovery codes. It is optional per account. TAK
// clients cannot enter codes, so device logins (enrollment, basic auth,
// OAuth, certificates) are unaffected; instead, an account with two-factor
// on can only use the admin API through a console session that passed it.

const recoveryCodeCount = 10

var (
	codeLimiterOnce sync.Once
	codeLimiter     *rateLimiter
)

func verifyTOTP(secret, code string, lastStep int64) (int64, bool) {
	return totp.Verify(secret, code, time.Now(), lastStep)
}

// hashRecovery normalizes and hashes a recovery code ("ab12-cd34").
func hashRecovery(code string) string {
	c := strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	sum := sha256.Sum256([]byte("edgetak-recovery:" + c))
	return hex.EncodeToString(sum[:])
}

// newRecoveryCodes returns codes to show once and their hashes to store.
func newRecoveryCodes() (codes, hashes []string) {
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	for range recoveryCodeCount {
		b := make([]byte, 5)
		rand.Read(b)
		c := strings.ToLower(enc.EncodeToString(b)) // 8 characters
		codes = append(codes, c[:4]+"-"+c[4:])
		hashes = append(hashes, hashRecovery(c))
	}
	return codes, hashes
}

// secondFactor checks the login code; it writes the response and returns
// false when the login must not proceed.
func (s *Server) secondFactor(w http.ResponseWriter, r *http.Request, username, code string) bool {
	if strings.TrimSpace(code) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "enter the code from your authenticator app", "need_code": true})
		return false
	}
	codeLimiterOnce.Do(func() { codeLimiter = newRateLimiter(6, 10*time.Minute) })
	if !codeLimiter.allow(strings.ToLower(username)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many wrong codes; wait a few minutes", "need_code": true})
		return false
	}
	ok, recovery := s.Store.CheckSecondFactor(username, code, verifyTOTP, hashRecovery)
	if !ok {
		time.Sleep(500 * time.Millisecond)
		s.Log.Warn("sign-in: wrong two-factor code", "user", username, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong or expired code", "need_code": true})
		return false
	}
	codeLimiter.refund(strings.ToLower(username))
	if recovery {
		s.Log.Warn("sign-in with a recovery code", "user", username, "remote", r.RemoteAddr)
	}
	return true
}

// twoFactorHandler serves one of a user's own two-factor actions. The same
// handlers are on the admin console (/admin/api/me/2fa) and the account page
// (/account/api/2fa), so every user can turn it on.
type twoFactorHandler func(w http.ResponseWriter, r *http.Request, username string)

func (s *Server) twoFactorRoutes(mux *http.ServeMux) {
	own := map[string]twoFactorHandler{
		"GET ":           s.twoFactorStatus,
		"POST /setup":    s.twoFactorSetup,
		"POST /enable":   s.twoFactorEnable,
		"POST /disable":  s.twoFactorDisable,
		"POST /recovery": s.twoFactorRecovery,
	}
	for route, h := range own {
		method, sub, _ := strings.Cut(route, " ")
		h := h
		mux.HandleFunc(method+" /admin/api/me/2fa"+sub, s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
			if method == "POST" && !id.Session {
				jsonError(w, http.StatusForbidden, "manage two-factor login from the admin console")
				return
			}
			h(w, r, id.Username)
		}))
		mux.HandleFunc(method+" /account/api/2fa"+sub, s.accountAuth(func(w http.ResponseWriter, r *http.Request, u *store.User, _ string) {
			h(w, r, u.Username)
		}))
	}
	// An admin resets someone else's two-factor (lost phone).
	mux.HandleFunc("DELETE /admin/api/users/{name}/2fa", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		name := r.PathValue("name")
		if err := s.Store.DisableTOTP(name); err != nil {
			jsonError(w, http.StatusNotFound, "user not found")
			return
		}
		s.Log.Warn("two-factor login reset by an administrator", "admin", id.Username, "user", name)
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (s *Server) twoFactorStatus(w http.ResponseWriter, r *http.Request, username string) {
	u, _ := s.Store.User(username)
	left := 0
	if u != nil {
		left = len(u.RecoveryHash)
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": u != nil && u.TwoFactor(), "recovery_left": left})
}

// twoFactorSetup starts setup: a new secret to scan, which only counts once
// confirmed with a code.
func (s *Server) twoFactorSetup(w http.ResponseWriter, r *http.Request, username string) {
	secret, err := totp.NewSecret()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Store.SetTOTPPending(username, secret); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	issuer := firstNonEmpty(s.Cfg.ServerName, "EdgeTAK")
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "uri": totp.URI(issuer, username, secret)})
}

func (s *Server) twoFactorEnable(w http.ResponseWriter, r *http.Request, username string) {
	var req struct{ Code string }
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req)
	codes, hashes := newRecoveryCodes()
	if err := s.Store.EnableTOTP(username, req.Code, hashes, verifyTOTP); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Log.Info("two-factor login turned on", "user", username)
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// Turning it off or new recovery codes need the password and a current code.
func (s *Server) twoFactorDisable(w http.ResponseWriter, r *http.Request, username string) {
	if !s.reauth(w, r, username) {
		return
	}
	if err := s.Store.DisableTOTP(username); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Warn("two-factor login turned off", "user", username)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) twoFactorRecovery(w http.ResponseWriter, r *http.Request, username string) {
	if !s.reauth(w, r, username) {
		return
	}
	codes, hashes := newRecoveryCodes()
	if err := s.Store.SetRecoveryCodes(username, hashes); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("new two-factor recovery codes", "user", username)
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// reauth checks the password and a current second factor in the request
// body before a sensitive change.
func (s *Server) reauth(w http.ResponseWriter, r *http.Request, username string) bool {
	var req struct{ Password, Code string }
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&req)
	if _, ok := s.authenticate(username, req.Password); !ok {
		time.Sleep(500 * time.Millisecond)
		jsonError(w, http.StatusUnauthorized, "wrong password")
		return false
	}
	return s.secondFactor(w, r, username, req.Code)
}
