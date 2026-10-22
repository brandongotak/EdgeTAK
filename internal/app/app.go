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

	"github.com/brandongotak/EdgeTAK/internal/chatlog"
	"github.com/brandongotak/EdgeTAK/internal/config"
	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/federation"
	"github.com/brandongotak/EdgeTAK/internal/health"
	"github.com/brandongotak/EdgeTAK/internal/history"
	"github.com/brandongotak/EdgeTAK/internal/maps"
	"github.com/brandongotak/EdgeTAK/internal/mediamtx"
	"github.com/brandongotak/EdgeTAK/internal/mission"
	"github.com/brandongotak/EdgeTAK/internal/pki"
	"github.com/brandongotak/EdgeTAK/internal/publiccert"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
	"github.com/brandongotak/EdgeTAK/internal/stream"
	"github.com/brandongotak/EdgeTAK/internal/tlstrace"
	"github.com/brandongotak/EdgeTAK/internal/web"
)

type App struct {
	Cfg     config.Config
	DataDir string
	Log     *slog.Logger
	Version string

	Store    *store.Store
	CA       *pki.CA
	Router   *router.Router
	Missions *mission.Service
	Public   *publiccert.Provider // nil unless public_cert is configured
	History  *history.Recorder
	Chat     *chatlog.Log

	// Addrs holds the bound address of each listener after Start.
	Addrs map[string]string
	// Listen opens listeners; it defaults to net.Listen and exists for tests.
	Listen func(network, address string) (net.Listener, error)
	// Dial opens outbound federation connections; it defaults to TCP and
	// exists for tests.
	Dial func(address string) (net.Conn, error)
	// Federation runs TAK Server federation (set by Start).
	Federation *federation.Manager
	// Web is the HTTP side (set by Start).
	Web *web.Server
	// ConfigPath is the configuration file (for backups); Restart stops
	// EdgeTAK for its service manager to start again (after a restore).
	ConfigPath string
	Restart    func()

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
	a.checkPublicHost()
	var err error
	if a.Store, err = store.Open(filepath.Join(dataDir, "db.json")); err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := a.ensureAdmin(); err != nil {
		return nil, err
	}
	if _, ok := a.Store.Group(cfg.AnonymousGroup); !ok {
		if _, err := a.Store.CreateGroup(cfg.AnonymousGroup, "Users without groups and anonymous (plaintext) clients"); err != nil {
			return nil, err
		}
	}
	hosts := append(pki.DetectHosts(), cfg.Hostnames...)
	if a.Cfg.PublicHost != "" {
		hosts = append([]string{a.Cfg.PublicHost}, hosts...)
	}
	validity := time.Duration(cfg.CertValidityDays) * 24 * time.Hour
	if d := a.Store.CertValidityDays(); d > 0 { // set by an admin on the Certificates tab
		validity = time.Duration(d) * 24 * time.Hour
	}
	if a.CA, err = pki.Load(filepath.Join(dataDir, "certs"), cfg.CertOrg, cfg.CertOrgUnit, hosts, validity); err != nil {
		return nil, err
	}
	if a.nodeID, err = readOrCreate(filepath.Join(dataDir, "node-id"), func() string { return randomString(16) }); err != nil {
		return nil, err
	}
	a.Router = router.New(log, cfg.SendLatestSA)
	if a.Missions, err = mission.Open(filepath.Join(dataDir, "missions"), a.Store, a.Router, log); err != nil {
		return nil, fmt.Errorf("open missions: %w", err)
	}
	a.Missions.PackageDir = filepath.Join(dataDir, "packages")
	if a.History, err = history.Open(filepath.Join(dataDir, "history"), log); err != nil {
		return nil, fmt.Errorf("open history: %w", err)
	}
	hs := a.Store.HistorySettings()
	a.History.Configure(hs.Enabled, time.Duration(hs.RetentionHours)*time.Hour)
	a.Router.AddHook(a.recordHistory)
	if a.Chat, err = chatlog.Open(filepath.Join(dataDir, "chat"), log); err != nil {
		return nil, fmt.Errorf("open chat log: %w", err)
	}
	a.Chat.SetEnabled(a.Store.ChatLogSettings().Enabled)
	if a.Public, err = publiccert.New(cfg.PublicCert.Normalized(a.Cfg.PublicHost), dataDir, &a.CA.ServerCert, log); err != nil {
		return nil, err
	}
	return a, nil
}

// recordHistory saves each device's own position reports (not markers it
// shares) for breadcrumb trails. It never drops events.
func (a *App) recordHistory(src *router.Client, ev *cot.Event) bool {
	if src == nil || !strings.HasPrefix(ev.Type, "a-") || !a.History.Enabled() {
		return true
	}
	if uid, callsign := src.Identity(); uid != "" && uid == ev.UID {
		team, speed, course := "", 0.0, 0.0
		if g := ev.Detail.Group; g != nil {
			team = g.Name
		}
		if t := ev.Detail.Track; t != nil {
			speed, course = t.Speed, t.Course
		}
		a.History.Record(uid, callsign, team, time.Now(), ev.Lat, ev.Lon, speed, course)
	}
	return true
}

// checkPublicHost warns when public_host is neither one of this machine's
// addresses nor a resolvable name: connection packages would point clients
// at an unreachable server.
func (a *App) checkPublicHost() {
	h := a.Cfg.PublicHost
	if h == "" {
		return
	}
	if ip := net.ParseIP(h); ip != nil {
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, ad := range addrs {
				if ipn, ok := ad.(*net.IPNet); ok && ipn.IP.Equal(ip) {
					return
				}
			}
		}
		a.Log.Warn("public_host is not an address of this machine; clients need it to reach the server (fine if behind NAT/port forwarding)",
			"public_host", h, "detected", pki.PrimaryIP())
		return
	}
	if _, err := net.LookupHost(h); err != nil {
		a.Log.Warn("public_host does not resolve; connection packages will point clients at an unreachable server",
			"public_host", h, "detected", pki.PrimaryIP(), "err", err)
	}
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
	// Windows SChannel (WinTAK's HTTPS stack) aborts both full handshakes
	// that issue a session ticket and ticket resumptions when it presents a
	// client certificate. Full handshakes every time match what TAK clients
	// see from the official server and cost little at edge scale.
	httpsTLS.SessionTicketsDisabled = true

	st := &stream.Server{
		Router: a.Router, Store: a.Store, Log: a.Log, AnonymousGroup: a.Cfg.AnonymousGroup,
		IdleTimeout: time.Duration(a.Cfg.IdleTimeoutSeconds) * time.Second,
		ServerUID:   "EdgeTAK-" + a.nodeID, Version: "EdgeTAK " + a.Version,
	}
	ws := &web.Server{
		Cfg: a.Cfg, Store: a.Store, CA: a.CA, Router: a.Router, Missions: a.Missions, Public: a.Public, History: a.History, Chat: a.Chat, Log: a.Log,
		PackageDir: filepath.Join(a.DataDir, "packages"), NodeID: a.nodeID, Version: a.Version, Started: time.Now(),
		DataDir: a.DataDir, ConfigPath: a.ConfigPath, Restart: a.Restart, Maps: &maps.Library{Dir: filepath.Join(a.DataDir, "maps")},
	}
	st.Ended = ws.ClientEnded
	a.Web = ws
	if err := ws.Maps.Scan(); err != nil {
		a.Log.Warn("offline maps", "err", err)
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
		if sp.tls != nil && a.Public.Uses(sp.name) {
			// Present the publicly trusted certificate; client certificates
			// are still verified against the EdgeTAK CA.
			sp.tls = sp.tls.Clone()
			sp.tls.Certificates = nil
			sp.tls.GetCertificate = a.Public.GetCertificate
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
			if a.Log.Enabled(ctx, slog.LevelDebug) {
				ln = &tlstrace.Listener{Listener: ln, Log: a.Log, Name: sp.name}
			}
			ln = tls.NewListener(ln, sp.tls)
		}
		binds = append(binds, bound{sp.name, ln, sp.web, sp.tls != nil})
	}
	var acmeLn net.Listener
	if h := a.Public.ChallengeHandler(port(a.Addrs["enroll"])); h != nil {
		addr := a.Cfg.PublicCert.Normalized(a.Cfg.PublicHost).ACMEHTTPAddr
		if acmeLn, err = a.Listen("tcp", addr); err != nil {
			for _, b := range binds {
				b.ln.Close()
			}
			return nil, fmt.Errorf("Let's Encrypt challenge listener %s (needs port 80 reachable from the internet): %w", addr, err)
		}
		a.Addrs["acme"] = acmeLn.Addr().String()
		a.Public.Warm(ctx)
	}
	// MediaMTX authenticates every publish and read through EdgeTAK.
	var videoLn net.Listener
	if a.Cfg.Video.Enabled {
		addr := a.Cfg.Video.Normalized().AuthAddr
		if videoLn, err = a.Listen("tcp", addr); err != nil {
			for _, b := range binds {
				b.ln.Close()
			}
			if acmeLn != nil {
				acmeLn.Close()
			}
			return nil, fmt.Errorf("video auth listener %s: %w", addr, err)
		}
		a.Addrs["video_auth"] = videoLn.Addr().String()
	}
	ws.Addrs = a.Addrs
	ws.Health = &health.Sampler{DataDir: a.DataDir, Src: health.Sources{
		Clients: func() int { return len(a.Router.Clients()) },
		CoT:     a.Router.Totals,
		Video:   ws.VideoBytes,
	}}
	a.Router.AddHook(func(src *router.Client, ev *cot.Event) bool {
		if src != nil && ev.Detail.Status != nil && strings.HasPrefix(ev.Type, "a-") {
			if uid, _ := src.Identity(); uid != "" && uid == ev.UID {
				ws.Health.RecordBattery(uid, int(ev.Detail.Status.Battery), time.Now())
			}
		}
		return true
	})
	if vc := a.Cfg.Video.Normalized(); vc.Enabled && vc.MediaMTXBinary != "" {
		conf := vc.MediaMTXConfig
		if conf == "" {
			conf = filepath.Join(a.DataDir, "mediamtx.yml")
		}
		ws.MediaMTX = &mediamtx.Manager{Binary: vc.MediaMTXBinary, Config: conf, APIAddr: vc.APIAddr,
			Log: a.Log.With("component", "mediamtx"), APIAuth: ws.MediaMTXAPIAuth}
	}
	fedCfg := federation.Config{Router: a.Router, Store: a.Store, Cert: func() *tls.Certificate { return &a.CA.ServerCert },
		ServerName: a.Cfg.ServerName, NodeID: a.nodeID, Version: a.Version, AnonGroup: a.Cfg.AnonymousGroup,
		PackageDir: ws.PackageDir, OfferPackage: ws.OfferFederatedPackage,
		Log:    a.Log.With("component", "federation"),
		Listen: func(addr string) (net.Listener, error) { return a.Listen("tcp", addr) }, Dial: a.Dial}
	a.Federation = federation.New(fedCfg)
	ws.Federation = a.Federation
	ws.FederationDial = a.Dial
	ws.Start(ctx) // server contact, Inbox and video (writes mediamtx.yml)
	a.Federation.Start(ctx.Done())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a.Missions.FlushLoop(ctx.Done()) }()
	wg.Add(1)
	go func() { defer wg.Done(); a.History.Run(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); <-ctx.Done(); a.Chat.Close() }()
	wg.Add(1)
	go func() { defer wg.Done(); <-ctx.Done(); ws.Maps.Close() }()
	wg.Add(1)
	go func() { defer wg.Done(); ws.Health.Run(ctx) }()
	if ws.MediaMTX != nil {
		wg.Add(1)
		go func() { defer wg.Done(); ws.MediaMTX.Run(ctx) }()
	}
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
	if acmeLn != nil {
		srv := &http.Server{Handler: a.Public.ChallengeHandler(port(a.Addrs["enroll"])), ReadHeaderTimeout: 15 * time.Second, ErrorLog: errLog}
		servers = append(servers, srv)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Serve(acmeLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.Log.Error("Let's Encrypt challenge listener stopped", "err", err)
			}
		}()
	}

	if videoLn != nil {
		mux := http.NewServeMux()
		mux.Handle("POST /mediamtx/auth", ws.MediaMTXAuthHandler())
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ErrorLog: errLog}
		servers = append(servers, srv)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Serve(videoLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.Log.Error("video auth listener stopped", "err", err)
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

// port returns the port of a host:port address ("" if none).
func port(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
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
