// Package app wires EdgeTAK's components together and runs its listeners.
package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edgetak/edgetak/internal/config"
	"github.com/edgetak/edgetak/internal/pki"
	"github.com/edgetak/edgetak/internal/router"
	"github.com/edgetak/edgetak/internal/store"
	"github.com/edgetak/edgetak/internal/stream"
	"github.com/edgetak/edgetak/internal/web"
)

type App struct {
	Cfg     config.Config
	DataDir string
	Log     *slog.Logger
	Version string

	Store  *store.Store
	CA     *pki.CA
	Router *router.Router

	// Addrs holds the bound address of each listener after Start.
	Addrs map[string]string
	// Listen opens listeners; it defaults to net.Listen and exists for tests.
	Listen func(network, address string) (net.Listener, error)

	nodeID string
}

// New opens the data directory, creating the CA, server certificate and an
// initial administrator on first run.
func New(cfg config.Config, dataDir, version string, log *slog.Logger) (*App, error) {
	a := &App{Cfg: cfg, DataDir: dataDir, Log: log, Version: version, Addrs: map[string]string{}, Listen: net.Listen}
	if a.Cfg.PublicHost == "" {
		a.Cfg.PublicHost = pki.PrimaryIP()
		if a.Cfg.PublicHost == "" {
			a.Cfg.PublicHost, _ = os.Hostname()
		}
	}
	var err error
	if a.Store, err = store.Open(filepath.Join(dataDir, "db.json")); err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := a.ensureAdmin(); err != nil {
		return nil, err
	}
	hosts := append(pki.DetectHosts(), cfg.Hostnames...)
	if a.Cfg.PublicHost != "" {
		hosts = append([]string{a.Cfg.PublicHost}, hosts...)
	}
	validity := time.Duration(cfg.CertValidityDays) * 24 * time.Hour
	if a.CA, err = pki.Load(filepath.Join(dataDir, "certs"), cfg.CertOrg, cfg.CertOrgUnit, hosts, validity); err != nil {
		return nil, err
	}
	if a.nodeID, err = readOrCreate(filepath.Join(dataDir, "node-id"), func() string { return randomString(16) }); err != nil {
		return nil, err
	}
	a.Router = router.New(log, cfg.SendLatestSA)
	return a, nil
}

func (a *App) ensureAdmin() error {
	if a.Store.HasAdmin() {
		return nil
	}
	pw := randomString(16)
	if _, ok := a.Store.User("admin"); ok {
		if err := a.Store.SetPassword("admin", pw); err != nil {
			return err
		}
		if _, err := a.Store.UpdateUser("admin", func(u *store.User) error { u.Admin = true; return nil }); err != nil {
			return err
		}
	} else if _, err := a.Store.CreateUser("admin", pw, nil, true); err != nil {
		return err
	}
	path := filepath.Join(a.DataDir, "initial-admin-password.txt")
	os.WriteFile(path, []byte("username: admin\npassword: "+pw+"\n"), 0o600)
	a.Log.Warn("created administrator account", "username", "admin", "password", pw, "saved_to", path)
	return nil
}

// Start opens all configured listeners and serves until ctx is cancelled.
// It returns once every listener is bound; Wait blocks until shutdown.
func (a *App) Start(ctx context.Context) (wait func(), err error) {
	pool := a.CA.Pool()
	streamTLS := &tls.Config{
		Certificates: []tls.Certificate{a.CA.ServerCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	httpsTLS := streamTLS.Clone()
	httpsTLS.ClientAuth = tls.VerifyClientCertIfGiven

	st := &stream.Server{
		Router: a.Router, Store: a.Store, Log: a.Log, AnonymousGroup: a.Cfg.AnonymousGroup,
		IdleTimeout: time.Duration(a.Cfg.IdleTimeoutSeconds) * time.Second,
		ServerUID:   "EdgeTAK-" + a.nodeID, Version: "EdgeTAK " + a.Version,
	}
	ws := &web.Server{
		Cfg: a.Cfg, Store: a.Store, CA: a.CA, Router: a.Router, Log: a.Log,
		PackageDir: filepath.Join(a.DataDir, "packages"), NodeID: a.nodeID, Version: a.Version, Started: time.Now(),
	}

	// Bind every listener before serving any, so a port conflict fails
	// cleanly and handlers can rely on the complete set of bound addresses.
	type bound struct {
		name string
		ln   net.Listener
		web  bool
		tls  bool
	}
	specs := []struct {
		name, addr string
		tls        *tls.Config
		web        bool
	}{
		{"tcp", a.Cfg.TCPAddr, nil, false},
		{"tls", a.Cfg.TLSAddr, streamTLS, false},
		{"https", a.Cfg.HTTPSAddr, httpsTLS, true},
		{"enroll", a.Cfg.EnrollAddr, httpsTLS, true},
		{"http", a.Cfg.HTTPAddr, nil, true},
	}
	var binds []bound
	for _, sp := range specs {
		if sp.addr == "" {
			continue
		}
		ln, err := a.Listen("tcp", sp.addr)
		if err != nil {
			for _, b := range binds {
				b.ln.Close()
			}
			return nil, fmt.Errorf("%s listener %s: %w", sp.name, sp.addr, err)
		}
		a.Addrs[sp.name] = ln.Addr().String()
		if sp.tls != nil {
			ln = tls.NewListener(ln, sp.tls)
		}
		binds = append(binds, bound{sp.name, ln, sp.web, sp.tls != nil})
	}
	ws.Addrs = a.Addrs

	var wg sync.WaitGroup
	errLog := slog.NewLogLogger(a.Log.Handler(), slog.LevelDebug)
	var servers []*http.Server
	for _, b := range binds {
		wg.Add(1)
		if !b.web {
			go func() {
				defer wg.Done()
				if err := st.Serve(ctx, b.ln, b.name); err != nil {
					a.Log.Error("stream listener stopped", "listener", b.name, "err", err)
				}
			}()
			continue
		}
		srv := &http.Server{
			Handler:           ws.Handler(b.tls),
			ReadHeaderTimeout: 15 * time.Second,
			IdleTimeout:       2 * time.Minute,
			ErrorLog:          errLog,
		}
		servers = append(servers, srv)
		go func() {
			defer wg.Done()
			if err := srv.Serve(b.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.Log.Error("http listener stopped", "listener", b.name, "err", err)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, s := range servers {
			s.Shutdown(shutdownCtx)
		}
		for _, c := range a.Router.Clients() {
			c.Close()
		}
	}()

	var names []string
	for k, v := range a.Addrs {
		names = append(names, k+"="+v)
	}
	a.Log.Info("EdgeTAK started", "version", a.Version, "public_host", a.Cfg.PublicHost, "listeners", strings.Join(names, " "))
	return wg.Wait, nil
}

func readOrCreate(path string, gen func() string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	v := gen()
	return v, os.WriteFile(path, []byte(v+"\n"), 0o644)
}

const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		k, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[k.Int64()]
	}
	return string(b)
}

// RandomPassword returns a new random password.
func RandomPassword() string { return randomString(16) }
