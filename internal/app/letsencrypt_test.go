package app

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/config"
)

// TestLetsEncryptSetting covers turning Let's Encrypt on and off from the
// console: saved to the configuration file, applied by a restart.
func TestLetsEncryptSetting(t *testing.T) {
	a := startApp(t)
	path := filepath.Join(a.DataDir, "edgetak.json")
	cfg := a.Cfg
	cfg.PublicCert.ACMEHTTPAddr = "127.0.0.1:0" // port 80 is checked to open first
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	var restarts atomic.Int32
	a.Web.ConfigPath, a.Web.Restart = path, func() { restarts.Add(1) }
	a.Web.LookupHost = func(_ context.Context, host string) ([]string, error) {
		if host == "tak.example.com" {
			return []string{"203.0.113.5"}, nil
		}
		return nil, errors.New("no such host")
	}
	call, _ := adminJSON(t, a)

	if st, v := call("GET", "/admin/api/letsencrypt", nil); st != 200 || v["mode"] != "" || v["domain"] != "" {
		t.Fatalf("initial = %d %v", st, v)
	}
	for name, body := range map[string]map[string]any{
		"ip address":   {"on": true, "domain": "203.0.113.5"},
		"no domain":    {"on": true, "domain": ""},
		"bad email":    {"on": true, "domain": "tak.example.com", "email": "nope"},
		"not resolved": {"on": true, "domain": "edgetak-test.invalid"},
	} {
		if st, v := call("PUT", "/admin/api/letsencrypt", body); st != 400 {
			t.Errorf("%s = %d %v", name, st, v)
		}
	}
	// A port that can't be opened is refused before anything is saved.
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	cfg.PublicCert.ACMEHTTPAddr = busy.Addr().String()
	config.Save(path, cfg)
	if st, v := call("PUT", "/admin/api/letsencrypt", map[string]any{"on": true, "domain": "tak.example.com"}); st != 409 {
		t.Errorf("port in use = %d %v", st, v)
	}
	cfg.PublicCert.ACMEHTTPAddr = "127.0.0.1:0"
	config.Save(path, cfg)
	if cfg, _ := config.Load(path); cfg.PublicCert.Mode != "" || restarts.Load() != 0 {
		t.Fatal("a refused request changed the configuration or restarted")
	}

	st, v := call("PUT", "/admin/api/letsencrypt", map[string]any{"on": true, "domain": " TAK.Example.com. ", "email": "ops@example.com"})
	if st != 200 || v["restarting"] != true || v["pending"] != true || v["domain"] != "tak.example.com" {
		t.Fatalf("turn on = %d %v", st, v)
	}
	cfg, _ = config.Load(path)
	if cfg.PublicHost != "tak.example.com" || cfg.PublicCert.Mode != "letsencrypt" || cfg.PublicCert.Email != "ops@example.com" ||
		len(cfg.PublicCert.Domains) != 1 || cfg.PublicCert.ACMEHTTPAddr != "127.0.0.1:0" || len(cfg.PublicCert.Listeners) == 0 {
		t.Errorf("saved config: host %q %+v", cfg.PublicHost, cfg.PublicCert)
	}

	if st, v := call("PUT", "/admin/api/letsencrypt", map[string]any{"on": false}); st != 200 || v["mode"] != "" {
		t.Fatalf("turn off = %d %v", st, v)
	}
	if cfg, _ := config.Load(path); cfg.PublicCert.Mode != "" || cfg.PublicHost != "tak.example.com" {
		t.Errorf("after turning off: host %q %+v", cfg.PublicHost, cfg.PublicCert)
	}
}
