package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/config"
)

// Settings from the environment, for containers: a compose file describes
// the server and they apply on every start, overriding the config file
// without being written to it.
//
//	EDGETAK_PUBLIC_HOST   address devices use to reach the server
//	EDGETAK_SERVER_NAME   name shown in the console and on devices
//	EDGETAK_LETSENCRYPT   1 (or an email address) for a Let's Encrypt certificate on 8446
//	EDGETAK_NO_PLAINTEXT  1 to turn off the plain TCP (8087) and HTTP (8080) ports
//	EDGETAK_VIDEO         1 to run the bundled MediaMTX media server (the image's default), 0 for no video
//	EDGETAK_LOG_LEVEL     debug, info, warn or error
func applyEnv(cfg *config.Config, dataDir string) error {
	if v := strings.TrimSpace(os.Getenv("EDGETAK_PUBLIC_HOST")); v != "" {
		cfg.PublicHost = v
	}
	if v := strings.TrimSpace(os.Getenv("EDGETAK_SERVER_NAME")); v != "" {
		cfg.ServerName = v
	}
	if v := strings.TrimSpace(os.Getenv("EDGETAK_LOG_LEVEL")); v != "" {
		cfg.LogLevel = v
	}
	if v := strings.TrimSpace(os.Getenv("EDGETAK_LETSENCRYPT")); v != "" && !isOff(v) {
		if cfg.PublicHost == "" || net.ParseIP(cfg.PublicHost) != nil {
			return fmt.Errorf("EDGETAK_LETSENCRYPT needs EDGETAK_PUBLIC_HOST set to a domain name (it is %q)", cfg.PublicHost)
		}
		cfg.PublicCert = cfg.PublicCert.Normalized(cfg.PublicHost)
		cfg.PublicCert.Mode, cfg.PublicCert.Domains = "letsencrypt", []string{cfg.PublicHost}
		if strings.Contains(v, "@") {
			cfg.PublicCert.Email = v
		}
	}
	if isOn(os.Getenv("EDGETAK_NO_PLAINTEXT")) {
		cfg.TCPAddr, cfg.HTTPAddr = "", ""
	}
	if isOff(os.Getenv("EDGETAK_VIDEO")) {
		cfg.Video.Enabled = false
	} else if isOn(os.Getenv("EDGETAK_VIDEO")) {
		cfg.Video = cfg.Video.Normalized()
		cfg.Video.Enabled = true
		if cfg.Video.MediaMTXBinary == "" {
			cfg.Video.MediaMTXBinary = defaultMediaMTXBinary(dataDir)
		}
		if err := seedMediaMTX(cfg.Video.MediaMTXBinary); err != nil {
			return err
		}
	}
	return nil
}

func isOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func isOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

func defaultMediaMTXBinary(dataDir string) string {
	exe := "mediamtx"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	p := filepath.Join(dataDir, "mediamtx", exe)
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// seedMediaMTX copies the MediaMTX that ships in the container image into
// the data volume the first time video is turned on. From then on the
// console's MediaMTX updates replace the copy in the volume.
func seedMediaMTX(dst string) error {
	src := os.Getenv("EDGETAK_MEDIAMTX_SEED")
	if src == "" {
		return nil
	}
	if _, err := os.Stat(dst); err == nil {
		return nil // already there (possibly updated from the console)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("video: no MediaMTX to install (%w)", err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o750)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if v, err := os.ReadFile(src + ".version"); err == nil {
		os.WriteFile(dst+".version", v, 0o640)
	}
	return os.Rename(tmp, dst)
}

// healthcheck reports whether EdgeTAK answers on its enrollment port (for
// Docker's HEALTHCHECK; the image has no curl).
func healthcheck(cfg config.Config) int {
	addr := cfg.EnrollAddr
	if addr == "" {
		addr = cfg.HTTPSAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: no HTTPS port configured")
		return 1
	}
	client := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{
		// Loopback only; the certificate is for the public name, not 127.0.0.1.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	res, err := client.Get("https://127.0.0.1:" + port + "/Marti/api/version")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", res.Status)
		return 1
	}
	return 0
}
