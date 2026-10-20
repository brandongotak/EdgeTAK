// Command edgetak runs the EdgeTAK server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/edgetak/edgetak/internal/app"
	"github.com/edgetak/edgetak/internal/config"
	"github.com/edgetak/edgetak/internal/store"
)

var version = "0.1.0-dev"

func main() {
	dataDir := flag.String("data", envOr("EDGETAK_DATA", "data"), "data directory (config, database, certificates, packages)")
	cfgPath := flag.String("config", "", "config file (default <data>/edgetak.json)")
	resetPassword := flag.String("reset-password", "", "set a new random password for `user`, print it and exit")
	initOnly := flag.Bool("init", false, "create the data directory, config, CA and admin account, then exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

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

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
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
