package web

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// OAuth2 password grant at /oauth/token, as served by the official server
// (Spring Authorization Server) and used by TAKX. Like the official server,
// access tokens are RS256 JWTs signed with the server's TLS key, so clients
// can verify them against the certificate of the server they talk to. They
// are accepted as "Authorization: Bearer" (or the access_token cookie).

const accessTokenTTL = 24 * time.Hour

type jwtClaims struct {
	Sub   string   `json:"sub"`
	Aud   []string `json:"aud"`
	Nbf   int64    `json:"nbf"`
	Scope []string `json:"scope"`
	Iss   string   `json:"iss"`
	Exp   int64    `json:"exp"`
	Iat   int64    `json:"iat"`
	Jti   string   `json:"jti"`
}

// jwtKeyID identifies the signing key (the server certificate).
func (s *Server) jwtKeyID() string {
	sum := sha256.Sum256(s.CA.ServerCert.Certificate[0])
	return hex.EncodeToString(sum[:8])
}

func (s *Server) signJWT(c jwtClaims) (string, error) {
	signer, ok := s.CA.ServerCert.PrivateKey.(crypto.Signer)
	if !ok {
		return "", errors.New("server key cannot sign")
	}
	header, _ := json.Marshal(map[string]string{"kid": s.jwtKeyID(), "alg": "RS256"})
	body, _ := json.Marshal(c)
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := signer.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

var errBadToken = errors.New("invalid token")

// parseJWT verifies a token issued by this server and returns its claims.
func (s *Server) parseJWT(tok string) (*jwtClaims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errBadToken
	}
	var header struct{ Alg string }
	if raw, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil || json.Unmarshal(raw, &header) != nil || header.Alg != "RS256" {
		return nil, errBadToken
	}
	pub, ok := s.CA.ServerCert.Leaf.PublicKey.(*rsa.PublicKey)
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[2]) // reject altered padding bits
	if !ok || err != nil {
		return nil, errBadToken
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) != nil {
		return nil, errBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errBadToken
	}
	var c jwtClaims
	now := time.Now().Unix()
	if json.Unmarshal(raw, &c) != nil || now >= c.Exp || now < c.Nbf-60 {
		return nil, errBadToken
	}
	return &c, nil
}

// bearerToken returns the access token a request carries, if any.
func bearerToken(r *http.Request) string {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(tok)
	}
	if c, err := r.Cookie("access_token"); err == nil {
		return c.Value
	}
	return ""
}

// bearerIdentity authenticates an OAuth access token. Tokens that are not
// ours (such as mission tokens, which share the header) yield nil.
func (s *Server) bearerIdentity(r *http.Request) *identity {
	tok := bearerToken(r)
	if tok == "" {
		return nil
	}
	c, err := s.parseJWT(tok)
	if err != nil {
		return nil
	}
	u, ok := s.Store.User(c.Sub)
	if !ok || u.Disabled {
		return nil // user deleted or disabled since the token was issued
	}
	return s.fromUser(u)
}

func (s *Server) oauthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /oauth/token", s.oauthToken)
}

func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "malformed request")
		return
	}
	if r.Form.Get("grant_type") != "password" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use grant_type=password")
		return
	}
	u, ok := s.authenticate(r.Form.Get("username"), r.Form.Get("password"))
	if !ok {
		s.Log.Warn("oauth login failed", "user", r.Form.Get("username"), "remote", r.RemoteAddr)
		oauthError(w, http.StatusBadRequest, "invalid_grant", "Bad credentials")
		return
	}
	now := time.Now()
	tok, err := s.signJWT(jwtClaims{
		Sub: u.Username, Aud: []string{u.Username}, Nbf: now.Unix(), Scope: []string{"openid", "profile"},
		Iss: baseURL(r), Exp: now.Add(accessTokenTTL).Unix(), Iat: now.Unix(), Jti: uuid(),
	})
	if err != nil {
		s.Log.Error("oauth signing failed", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not issue token")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok,
		"scope":        "openid profile",
		"token_type":   "Bearer",
		"expires_in":   int(accessTokenTTL.Seconds()) - 1,
	})
	s.Log.Info("oauth token issued", "user", u.Username, "remote", r.RemoteAddr)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}
