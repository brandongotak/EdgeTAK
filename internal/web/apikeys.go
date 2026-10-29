package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// API keys (Server -> API keys): tokens for scripts and automations to use
// the REST API as a user, sent as "Authorization: Bearer etk_...". A key
// skips its user's password and two-factor login, so only an administrator
// signed in to the console can make one, and the key is shown only once;
// only its hash is kept. A read-only key can only make GET requests. Keys,
// like passwords, are only accepted over HTTPS (or from this machine).

const apiKeyPrefix = "etk_"

// apiKeyIdentity authenticates an API key in the Authorization header. It
// returns nil when the request carries none; ok is false when it carries a
// key that doesn't work.
func (s *Server) apiKeyIdentity(r *http.Request) (id *identity, ok bool) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok = strings.TrimSpace(tok)
	if !strings.HasPrefix(tok, apiKeyPrefix) {
		return nil, true
	}
	if r.TLS == nil && !isLoopback(r.RemoteAddr) {
		return nil, false
	}
	ip := clientIP(r)
	if s.passwordBlocked(ip) {
		return nil, false
	}
	k, found := s.Store.APIKeyByHash(hashCode(tok))
	if !found {
		s.signInFailedFrom(ip, "API key")
		return nil, false
	}
	now := time.Now()
	if !k.Expires.IsZero() && !now.Before(k.Expires) {
		return nil, false
	}
	u, found := s.Store.User(k.Username)
	if !found || u.Disabled {
		return nil, false
	}
	s.Store.TouchAPIKey(k.ID, now.UTC(), ip)
	id = s.fromUser(u)
	id.APIKey, id.ReadOnly = k.Name, k.ReadOnly
	return id, true
}

type apiKeyView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Username  string    `json:"username"`
	Prefix    string    `json:"prefix"`
	ReadOnly  bool      `json:"read_only"`
	Expires   time.Time `json:"expires,omitzero"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by"`
	LastUsed  time.Time `json:"last_used,omitzero"`
	LastIP    string    `json:"last_ip,omitempty"`
	State     string    `json:"state"` // active, expired, or no user (deleted or disabled)
}

func (s *Server) apiKeyViews() []apiKeyView {
	now := time.Now()
	out := []apiKeyView{}
	for _, k := range s.Store.APIKeys() {
		v := apiKeyView{ID: k.ID, Name: k.Name, Username: k.Username, Prefix: k.Prefix, ReadOnly: k.ReadOnly, Expires: k.Expires,
			Created: k.Created, CreatedBy: k.CreatedBy, LastUsed: k.LastUsed, LastIP: k.LastIP, State: "active"}
		if u, ok := s.Store.User(k.Username); !ok || u.Disabled {
			v.State = "no user"
		} else if !k.Expires.IsZero() && !now.Before(k.Expires) {
			v.State = "expired"
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) apiKeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/api-keys", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.apiKeyViews())
	}))
	// Make a key. The key itself is only in this answer.
	mux.HandleFunc("POST /admin/api/api-keys", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if !id.Session {
			jsonError(w, http.StatusForbidden, "make API keys in the admin console")
			return
		}
		var req struct {
			Name     string `json:"name"`
			Username string `json:"username"`
			ReadOnly bool   `json:"read_only"`
			Days     int    `json:"days"` // 0: no expiry
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || utf8.RuneCountInString(req.Name) > 60 {
			jsonError(w, http.StatusBadRequest, "give the key a name of up to 60 characters")
			return
		}
		if req.Days < 0 || req.Days > 3650 {
			jsonError(w, http.StatusBadRequest, "days must be from 0 (no expiry) to 3650")
			return
		}
		if len(s.Store.APIKeys()) >= 100 {
			jsonError(w, http.StatusBadRequest, "this server has 100 API keys; delete some first")
			return
		}
		u, ok := s.Store.User(firstNonEmpty(req.Username, id.Username))
		if !ok {
			jsonError(w, http.StatusNotFound, "no such user")
			return
		}
		if u.Disabled {
			jsonError(w, http.StatusBadRequest, u.Username+" is disabled")
			return
		}
		key := apiKeyPrefix + randomToken(24)
		now := time.Now().UTC()
		k := store.APIKey{ID: randomToken(8), Name: req.Name, Username: u.Username, Hash: hashCode(key), Prefix: key[:len(apiKeyPrefix)+6],
			ReadOnly: req.ReadOnly, Created: now, CreatedBy: id.Username}
		if req.Days > 0 {
			k.Expires = now.AddDate(0, 0, req.Days)
		}
		if err := s.Store.AddAPIKey(k); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"key": key, "id": k.ID, "keys": s.apiKeyViews()})
	}))
	mux.HandleFunc("DELETE /admin/api/api-keys/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if err := s.Store.DeleteAPIKey(r.PathValue("id")); err != nil {
			jsonError(w, http.StatusNotFound, "no such API key")
			return
		}
		writeJSON(w, http.StatusOK, s.apiKeyViews())
	}))
}
