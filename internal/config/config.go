// Package config loads EdgeTAK's JSON configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the on-disk server configuration. An empty listener address
// disables that listener.
type Config struct {
	ServerName string `json:"server_name"`

	// PublicHost is the address clients use to reach this server; it is
	// written into connection data packages. Defaults to the first detected IP.
	PublicHost string `json:"public_host"`
	// Hostnames are extra DNS names / IPs added to the server certificate.
	Hostnames []string `json:"hostnames"`

	TCPAddr    string `json:"tcp_addr"`    // plaintext CoT streaming (8087)
	TLSAddr    string `json:"tls_addr"`    // mutual-TLS CoT streaming (8089)
	HTTPSAddr  string `json:"https_addr"`  // Marti API + admin UI over TLS (8443)
	EnrollAddr string `json:"enroll_addr"` // certificate enrollment (8446)
	HTTPAddr   string `json:"http_addr"`   // plaintext Marti API for TCP clients (8080)

	// AnonymousGroup is assigned to plaintext clients and users without groups.
	AnonymousGroup string `json:"anonymous_group"`
	// AllowAnonymousHTTP lets unauthenticated clients use the plaintext Marti API.
	AllowAnonymousHTTP bool `json:"allow_anonymous_http"`

	CertOrg            string `json:"cert_org"`
	CertOrgUnit        string `json:"cert_org_unit"`
	CertValidityDays   int    `json:"cert_validity_days"`
	TruststorePassword string `json:"truststore_password"`

	// SendLatestSA replays connected clients' latest position to new clients.
	SendLatestSA bool `json:"send_latest_sa"`
	// IdleTimeoutSeconds disconnects stream clients that send nothing for this long.
	IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
	// MaxUploadMB bounds data package uploads.
	MaxUploadMB int64 `json:"max_upload_mb"`

	LogLevel string `json:"log_level"` // debug, info, warn, error

	// PublicCert serves a publicly trusted certificate (e.g. Let's Encrypt)
	// on selected listeners instead of the EdgeTAK CA's server certificate.
	PublicCert PublicCert `json:"public_cert"`

	// Video connects EdgeTAK to a MediaMTX media server (installer --video).
	Video Video `json:"video"`
}

// Video configures the MediaMTX companion. EdgeTAK writes MediaMTX's config
// file, answers its authentication callbacks, lists its live streams as
// feeds and relays its HLS output to the admin console.
type Video struct {
	Enabled bool `json:"enabled"`
	// MediaMTXBinary is the MediaMTX executable EdgeTAK runs, restarts and
	// updates ("" = MediaMTX is run some other way, e.g. its own service).
	MediaMTXBinary string `json:"mediamtx_binary"`
	// MediaMTXConfig is where EdgeTAK writes mediamtx.yml ("" = <data>/mediamtx.yml).
	MediaMTXConfig string `json:"mediamtx_config"`
	// AuthAddr is the loopback listener for MediaMTX's auth callbacks.
	AuthAddr string `json:"auth_addr"`
	APIAddr  string `json:"api_addr"` // MediaMTX control API (loopback)
	HLSAddr  string `json:"hls_addr"` // MediaMTX HLS server (loopback; relayed by EdgeTAK)
	// PlaybackAddr is MediaMTX's recording playback server (loopback; used to
	// export recordings as one MP4).
	PlaybackAddr string `json:"playback_addr"`
	// BindHost limits MediaMTX's stream ports to one address ("" = all).
	BindHost string `json:"bind_host"`
	// Ports MediaMTX accepts streams on, advertised to clients.
	RTSPPort int `json:"rtsp_port"`
	RTMPPort int `json:"rtmp_port"`
	SRTPort  int `json:"srt_port"`
}

// Normalized fills defaults for unset video settings.
func (v Video) Normalized() Video {
	if v.AuthAddr == "" {
		v.AuthAddr = "127.0.0.1:8099"
	}
	if v.APIAddr == "" {
		v.APIAddr = "127.0.0.1:9997"
	}
	if v.HLSAddr == "" {
		v.HLSAddr = "127.0.0.1:8888"
	}
	if v.PlaybackAddr == "" {
		v.PlaybackAddr = "127.0.0.1:9996"
	}
	if v.RTSPPort == 0 {
		v.RTSPPort = 8554
	}
	if v.RTMPPort == 0 {
		v.RTMPPort = 1935
	}
	if v.SRTPort == 0 {
		v.SRTPort = 8890
	}
	return v
}

// PublicCert configures a publicly trusted server certificate. Like
// installTAK, it is meant for the enrollment port: clients enroll trusting
// the public CA, then receive the EdgeTAK CA for streaming and the API.
type PublicCert struct {
	// Mode is "" (off), "letsencrypt" (EdgeTAK obtains and renews the
	// certificate via ACME HTTP-01) or "files" (CertFile/KeyFile, e.g. from
	// certbot, reloaded when they change).
	Mode string `json:"mode"`
	// Domains to certify; defaults to public_host. Must resolve to this server.
	Domains []string `json:"domains"`
	// Email for the Let's Encrypt account (expiry notices).
	Email string `json:"email"`
	// ACMEHTTPAddr serves HTTP-01 challenges; Let's Encrypt connects to port 80.
	ACMEHTTPAddr string `json:"acme_http_addr"`
	// Staging uses the Let's Encrypt staging CA (untrusted, high rate limits).
	Staging  bool   `json:"staging"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	// Listeners that present the public certificate (tcp, tls, https, enroll).
	Listeners []string `json:"listeners"`
}

// Normalized fills defaults that older config files lack.
func (p PublicCert) Normalized(publicHost string) PublicCert {
	if len(p.Domains) == 0 && publicHost != "" {
		p.Domains = []string{publicHost}
	}
	if len(p.Listeners) == 0 {
		p.Listeners = []string{"enroll"}
	}
	if p.ACMEHTTPAddr == "" {
		p.ACMEHTTPAddr = ":80"
	}
	return p
}

// Default returns the configuration used when no file exists.
func Default() Config {
	return Config{
		ServerName:         "EdgeTAK",
		TCPAddr:            ":8087",
		TLSAddr:            ":8089",
		HTTPSAddr:          ":8443",
		EnrollAddr:         ":8446",
		HTTPAddr:           ":8080",
		AnonymousGroup:     "__ANON__",
		AllowAnonymousHTTP: true,
		CertOrg:            "EdgeTAK",
		CertOrgUnit:        "EdgeTAK",
		CertValidityDays:   365,
		TruststorePassword: "atakatak",
		SendLatestSA:       true,
		IdleTimeoutSeconds: 180,
		MaxUploadMB:        500,
		LogLevel:           "info",
		PublicCert:         PublicCert{Domains: []string{}, ACMEHTTPAddr: ":80", Listeners: []string{"enroll"}},
	}
}

// Load reads path, writing a default config there first if it does not exist.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, Save(path, cfg)
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes cfg to path as indented JSON.
func Save(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
