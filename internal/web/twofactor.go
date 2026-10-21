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
		s.Log.Warn("admin login: wrong two-factor code", "user", username, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong or expired code", "need_code": true})
		return false
	}
	codeLimiter.refund(strings.ToLower(username))
	if recovery {
		s.Log.Warn("admin login with a recovery code", "user", username, "remote", r.RemoteAddr)
	}
	return true
}

func (s *Server) twoFactorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/me/2fa", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		u, _ := s.Store.User(id.Username)
		left := 0
		if u != nil {
			left = len(u.RecoveryHash)
		}
		writeJSON(w, http.StatusOK, map[string]any{"enabled": u != nil && u.TwoFactor(), "recovery_left": left})
	}))
	// Start setup: a new secret to scan; it only counts once confirmed.
	mux.HandleFunc("POST /admin/api/me/2fa/setup", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if !id.Session {
			jsonError(w, http.StatusForbidden, "set up two-factor login from the admin console")
			return
		}
		secret, err := totp.NewSecret()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.Store.SetTOTPPending(id.Username, secret); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		issuer := firstNonEmpty(s.Cfg.ServerName, "EdgeTAK")
		writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "uri": totp.URI(issuer, id.Username, secret)})
	}))
	mux.HandleFunc("POST /admin/api/me/2fa/enable", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct{ Code string }
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req)
		codes, hashes := newRecoveryCodes()
		if err := s.Store.EnableTOTP(id.Username, req.Code, hashes, verifyTOTP); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.Log.Info("two-factor login turned on", "user", id.Username)
		writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
	}))
	// Turning it off or new recovery codes need the password and a current code.
	mux.HandleFunc("POST /admin/api/me/2fa/disable", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if !s.reauth(w, r, id) {
			return
		}
		if err := s.Store.DisableTOTP(id.Username); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Warn("two-factor login turned off", "user", id.Username)
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /admin/api/me/2fa/recovery", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if !s.reauth(w, r, id) {
			return
		}
		codes, hashes := newRecoveryCodes()
		if err := s.Store.SetRecoveryCodes(id.Username, hashes); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("new two-factor recovery codes", "user", id.Username)
		writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
	}))
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

// reauth checks the password and a current second factor in the request
// body before a sensitive change.
func (s *Server) reauth(w http.ResponseWriter, r *http.Request, id *identity) bool {
	var req struct{ Password, Code string }
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&req)
	if _, ok := s.Store.Authenticate(id.Username, req.Password); !ok {
		time.Sleep(500 * time.Millisecond)
		jsonError(w, http.StatusUnauthorized, "wrong password")
		return false
	}
	return s.secondFactor(w, r, id.Username, req.Code)
}
