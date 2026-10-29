package web

import (
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/config"
)

// Let's Encrypt (Settings → Let's Encrypt): turns the public certificate on
// or off after install, as the installer's --letsencrypt does. The choice is
// saved in the configuration file and applied by restarting EdgeTAK.

var (
	domainName = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	emailAddr  = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)
)

func (s *Server) letsEncryptView() (map[string]any, error) {
	cfg, err := config.Load(s.ConfigPath)
	if err != nil {
		return nil, err
	}
	domain := ""
	if len(cfg.PublicCert.Domains) > 0 {
		domain = cfg.PublicCert.Domains[0]
	} else if net.ParseIP(cfg.PublicHost) == nil {
		domain = cfg.PublicHost
	}
	return map[string]any{
		"mode": cfg.PublicCert.Mode, "domain": domain, "email": cfg.PublicCert.Email,
		"current": s.Public.Info(), "can_restart": s.Restart != nil,
		// Saved but not running yet: EdgeTAK wasn't restarted.
		"pending": cfg.PublicCert.Mode != s.Cfg.PublicCert.Mode,
	}, nil
}

func (s *Server) letsEncryptRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/letsencrypt", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if s.ConfigPath == "" {
			jsonError(w, http.StatusNotImplemented, "this server has no configuration file")
			return
		}
		v, err := s.letsEncryptView()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, v)
	}))
	mux.HandleFunc("PUT /admin/api/letsencrypt", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			On     bool   `json:"on"`
			Domain string `json:"domain"`
			Email  string `json:"email"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if s.ConfigPath == "" {
			jsonError(w, http.StatusNotImplemented, "this server has no configuration file")
			return
		}
		cfg, err := config.Load(s.ConfigPath)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if cfg.PublicCert.Mode == "files" {
			jsonError(w, http.StatusConflict, "this server uses certificate files (public_cert in its configuration); change them there")
			return
		}
		domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.Domain)), ".")
		email := strings.TrimSpace(req.Email)
		if req.On {
			if !domainName.MatchString(domain) || net.ParseIP(domain) != nil {
				jsonError(w, http.StatusBadRequest, "Enter a domain name such as tak.example.com, not an IP address")
				return
			}
			if email != "" && !emailAddr.MatchString(email) {
				jsonError(w, http.StatusBadRequest, "invalid email address")
				return
			}
			lookup := s.LookupHost
			if lookup == nil {
				lookup = net.DefaultResolver.LookupHost
			}
			if _, err := lookup(r.Context(), domain); err != nil {
				jsonError(w, http.StatusBadRequest, domain+" does not resolve yet. Point its DNS record at this server and try again once it does.")
				return
			}
			cfg.PublicCert = cfg.PublicCert.Normalized(domain)
			// EdgeTAK doesn't start without the challenge port, and then
			// the console is gone too: check it opens before restarting.
			if s.Public.ChallengeHandler("") == nil {
				ln, err := net.Listen("tcp", cfg.PublicCert.ACMEHTTPAddr)
				if err != nil {
					jsonError(w, http.StatusConflict, "EdgeTAK can't open port 80 for Let's Encrypt ("+err.Error()+"). Stop what uses it, or run EdgeTAK with permission to use it.")
					return
				}
				ln.Close()
			}
			cfg.PublicHost = domain
			cfg.PublicCert.Mode, cfg.PublicCert.Email, cfg.PublicCert.Domains = "letsencrypt", email, []string{domain}
		} else {
			cfg.PublicCert.Mode = ""
		}
		if err := config.Save(s.ConfigPath, cfg); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Warn("Let's Encrypt turned "+map[bool]string{true: "on", false: "off"}[req.On]+"; EdgeTAK restarts to apply it",
			"admin", id.Username, "domain", cfg.PublicHost)
		v, err := s.letsEncryptView()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		v["restarting"] = s.Restart != nil
		writeJSON(w, http.StatusOK, v)
		if s.Restart != nil {
			go func() { time.Sleep(500 * time.Millisecond); s.Restart() }()
		}
	}))
}
