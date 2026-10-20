// Package publiccert provides a publicly trusted server certificate, either
// obtained and renewed from Let's Encrypt (ACME HTTP-01) or loaded from files
// such as certbot's, so clients can enroll with only an address, username and
// password: their first connection (enrollment on 8446) is then trusted by
// the device's built-in roots, and enrollment hands them the EdgeTAK CA for
// everything after that.
package publiccert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/brandongotak/EdgeTAK/internal/config"
)

const letsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"

// Provider serves the public certificate for TLS handshakes.
type Provider struct {
	cfg      config.PublicCert
	log      *slog.Logger
	fallback *tls.Certificate // EdgeTAK CA server cert, used if the public one is unavailable

	acme *autocert.Manager

	mu       sync.Mutex
	files    *tls.Certificate
	modTime  time.Time
	checked  time.Time
	lastWarn time.Time
}

// New returns nil when no public certificate is configured.
func New(cfg config.PublicCert, dataDir string, fallback *tls.Certificate, log *slog.Logger) (*Provider, error) {
	p := &Provider{cfg: cfg, log: log, fallback: fallback}
	switch cfg.Mode {
	case "":
		return nil, nil
	case "letsencrypt":
		if len(cfg.Domains) == 0 {
			return nil, errors.New("public_cert: letsencrypt needs a domain (set public_host or public_cert.domains)")
		}
		p.acme = &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(filepath.Join(dataDir, "acme")),
			HostPolicy: autocert.HostWhitelist(cfg.Domains...),
			Email:      cfg.Email,
		}
		if cfg.Staging {
			p.acme.Client = &acme.Client{DirectoryURL: letsEncryptStaging}
		}
	case "files":
		if cfg.CertFile == "" || cfg.KeyFile == "" {
			return nil, errors.New("public_cert: files mode needs cert_file and key_file")
		}
		if err := p.reloadFiles(true); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("public_cert: unknown mode %q (use letsencrypt or files)", cfg.Mode)
	}
	return p, nil
}

// Uses reports whether listener should present the public certificate.
func (p *Provider) Uses(listener string) bool {
	if p == nil {
		return false
	}
	for _, l := range p.cfg.Listeners {
		if l == listener {
			return true
		}
	}
	return false
}

// GetCertificate implements tls.Config.GetCertificate. Clients that connect
// by IP send no SNI; they get the first configured domain. If the public
// certificate cannot be obtained, the EdgeTAK certificate is served instead
// so the port keeps working for clients that trust the EdgeTAK CA.
func (p *Provider) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert, err := p.get(hello)
	if err == nil {
		return cert, nil
	}
	p.mu.Lock()
	if time.Since(p.lastWarn) > time.Minute {
		p.lastWarn = time.Now()
		p.log.Warn("public certificate unavailable; serving the EdgeTAK certificate", "mode", p.cfg.Mode, "err", err)
	}
	p.mu.Unlock()
	return p.fallback, nil
}

func (p *Provider) get(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if p.acme != nil {
		h := *hello
		if !p.known(h.ServerName) {
			h.ServerName = p.cfg.Domains[0]
		}
		return p.acme.GetCertificate(&h)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.checked) > time.Minute {
		p.checked = time.Now()
		if err := p.reloadFiles(false); err != nil {
			p.log.Warn("could not reload public certificate files; keeping the current one", "err", err)
		}
	}
	if p.files == nil {
		return nil, errors.New("no certificate loaded")
	}
	return p.files, nil
}

func (p *Provider) known(name string) bool {
	for _, d := range p.cfg.Domains {
		if strings.EqualFold(d, name) {
			return true
		}
	}
	return false
}

// reloadFiles loads cert_file/key_file when they changed. Callers hold p.mu
// (or own p exclusively).
func (p *Provider) reloadFiles(force bool) error {
	st, err := os.Stat(p.cfg.CertFile)
	if err != nil {
		return fmt.Errorf("public_cert: %w", err)
	}
	if !force && !st.ModTime().After(p.modTime) {
		return nil
	}
	pair, err := tls.LoadX509KeyPair(p.cfg.CertFile, p.cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("public_cert: %w (the edgetak user must be able to read both files)", err)
	}
	if pair.Leaf == nil {
		pair.Leaf, _ = x509.ParseCertificate(pair.Certificate[0])
	}
	p.files, p.modTime = &pair, st.ModTime()
	p.log.Info("loaded public certificate", "file", p.cfg.CertFile, "subject", pair.Leaf.Subject.CommonName, "expires", pair.Leaf.NotAfter)
	return nil
}

// ChallengeHandler serves ACME HTTP-01 challenges and redirects everything
// else to the admin console on the enrollment port. Nil outside letsencrypt mode.
func (p *Provider) ChallengeHandler(enrollPort string) http.Handler {
	if p == nil || p.acme == nil {
		return nil
	}
	return p.acme.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.Split(r.Host, ":")[0]
		http.Redirect(w, r, "https://"+host+":"+enrollPort+"/admin/", http.StatusFound)
	}))
}

// Warm obtains the Let's Encrypt certificate in the background at startup,
// so the first client does not wait for issuance.
func (p *Provider) Warm(ctx context.Context) {
	if p == nil || p.acme == nil {
		return
	}
	go func() {
		if ctx.Err() != nil {
			return
		}
		_, err := p.acme.GetCertificate(&tls.ClientHelloInfo{ServerName: p.cfg.Domains[0],
			CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}})
		if err != nil {
			p.log.Warn("Let's Encrypt certificate not obtained yet; check DNS for the domain and that port 80 is reachable",
				"domain", p.cfg.Domains[0], "err", err)
			return
		}
		p.log.Info("Let's Encrypt certificate ready", "domain", p.cfg.Domains[0])
	}()
}

// Info describes the current certificate for the admin console.
func (p *Provider) Info() map[string]any {
	if p == nil {
		return map[string]any{"mode": "off"}
	}
	info := map[string]any{"mode": p.cfg.Mode, "domains": p.cfg.Domains, "listeners": p.cfg.Listeners}
	if p.cfg.Staging {
		info["staging"] = true
	}
	var leaf *x509.Certificate
	if p.acme != nil {
		// Only report what is already cached; never trigger issuance here.
		if data, err := p.acme.Cache.Get(context.Background(), p.cfg.Domains[0]); err == nil {
			if cert, err := tls.X509KeyPair(data, data); err == nil {
				leaf, _ = x509.ParseCertificate(cert.Certificate[0])
			}
		}
	} else {
		p.mu.Lock()
		if p.files != nil {
			leaf = p.files.Leaf
		}
		p.mu.Unlock()
	}
	if leaf != nil {
		info["issuer"] = leaf.Issuer.CommonName
		info["not_after"] = leaf.NotAfter
	} else {
		info["status"] = "pending"
	}
	return info
}
