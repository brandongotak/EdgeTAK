// Command edgetak runs the EdgeTAK server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/brandongotak/EdgeTAK/internal/app"
	"github.com/brandongotak/EdgeTAK/internal/config"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// version is set by -ldflags in release builds; go install builds report the
// module version instead.
var version = "0.1.0-dev"

func init() {
	if bi, ok := debug.ReadBuildInfo(); ok && version == "0.1.0-dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
}

func main() {
	dataDir := flag.String("data", envOr("EDGETAK_DATA", "data"), "data directory (config, database, certificates, packages)")
	cfgPath := flag.String("config", "", "config file (default <data>/edgetak.json)")
	resetPassword := flag.String("reset-password", "", "set a new random password for `user`, print it and exit")
	reset2FA := flag.String("reset-2fa", "", "turn off two-factor login for `user` (lost phone), then exit; stop EdgeTAK first")
	initOnly := flag.Bool("init", false, "create the data directory, config, CA and admin account, then exit")
	enableLE := flag.String("enable-letsencrypt", "", "use a Let's Encrypt certificate on the enrollment port, optionally registered to `email` (-enable-letsencrypt= for none); needs public_host to be a domain pointing here and port 80 reachable")
	enableVideo := flag.Bool("enable-video", false, "connect EdgeTAK to a MediaMTX media server on this machine (the installer sets it up)")
	disableVideo := flag.Bool("disable-video", false, "turn video off (the installer's --no-video)")
	showVersion := flag.Bool("version", false, "print version and exit")
	healthCheck := flag.Bool("healthcheck", false, "exit 0 if the running EdgeTAK answers on its enrollment port (container health check)")
	flag.Parse()
	// -enable-letsencrypt= (no email) still counts: Let's Encrypt no longer
	// sends expiry emails, so the address is optional.
	leSet := false
	flag.Visit(func(f *flag.Flag) { leSet = leSet || f.Name == "enable-letsencrypt" })

	if *showVersion {
		fmt.Println("EdgeTAK", version)
		return
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		fatal(err)
	}
	if *cfgPath == "" {
		*cfgPath = filepath.Join(*dataDir, "edgetak.json")
	}

	if *resetPassword != "" {
		st, err := store.Open(filepath.Join(*dataDir, "db.json"))
		if err != nil {
			fatal(err)
		}
		pw := app.RandomPassword()
		if err := st.SetPassword(*resetPassword, pw); err != nil {
			fatal(fmt.Errorf("user %q: %w", *resetPassword, err))
		}
		fmt.Printf("new password for %s: %s\n", *resetPassword, pw)
		return
	}
	if *reset2FA != "" {
		st, err := store.Open(filepath.Join(*dataDir, "db.json"))
		if err != nil {
			fatal(err)
		}
		if err := st.DisableTOTP(*reset2FA); err != nil {
			fatal(fmt.Errorf("user %q: %w", *reset2FA, err))
		}
		fmt.Printf("two-factor login turned off for %s; start EdgeTAK again\n", *reset2FA)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}
	if leSet {
		if cfg.PublicHost == "" || net.ParseIP(cfg.PublicHost) != nil {
			fatal(fmt.Errorf("Let's Encrypt needs public_host set to a domain name (it is %q)", cfg.PublicHost))
		}
		cfg.PublicCert = cfg.PublicCert.Normalized(cfg.PublicHost)
		cfg.PublicCert.Mode, cfg.PublicCert.Email, cfg.PublicCert.Domains = "letsencrypt", *enableLE, []string{cfg.PublicHost}
		if err := config.Save(*cfgPath, cfg); err != nil {
			fatal(err)
		}
		fmt.Printf("Let's Encrypt enabled for %s on %v (restart EdgeTAK to apply)\n", cfg.PublicHost, cfg.PublicCert.Listeners)
		return
	}
	if *disableVideo {
		if !cfg.Video.Enabled {
			return
		}
		cfg.Video.Enabled = false
		if err := config.Save(*cfgPath, cfg); err != nil {
			fatal(err)
		}
		fmt.Println("Video disabled (restart EdgeTAK to apply)")
		return
	}
	if *enableVideo {
		cfg.Video = cfg.Video.Normalized()
		cfg.Video.Enabled = true
		if cfg.Video.MediaMTXBinary == "" {
			cfg.Video.MediaMTXBinary = defaultMediaMTXBinary(*dataDir)
		}
		if err := config.Save(*cfgPath, cfg); err != nil {
			fatal(err)
		}
		fmt.Printf("Video enabled: MediaMTX on RTSP %d, RTMP %d, SRT %d (restart EdgeTAK to apply)\n", cfg.Video.RTSPPort, cfg.Video.RTMPPort, cfg.Video.SRTPort)
		return
	}
	if err := applyEnv(&cfg, *dataDir); err != nil {
		fatal(err)
	}
	if *healthCheck {
		os.Exit(healthcheck(cfg))
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))

	a, err := app.New(cfg, *dataDir, version, log)
	if err != nil {
		fatal(err)
	}
	if *initOnly {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	wait, err := a.Start(ctx)
	if err != nil {
		fatal(err)
	}
	wait()
	log.Info("EdgeTAK stopped")
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "edgetak:", err)
	os.Exit(1)
}
