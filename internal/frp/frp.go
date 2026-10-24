// Package frp runs the FRP client (frpc) for remote access: frpc connects
// out to an FRP server (frps) on a VPS or a relay service, which forwards
// its public ports back to EdgeTAK, so devices reach a server behind NAT or
// a mobile connection without opening ports or installing anything.
//
// EdgeTAK downloads frpc itself from FRP's GitHub release, a pinned version
// checked against pinned SHA-256 checksums, keeps it in <data>/frpc-bin
// (left out of backups) and its config in <data>/frp, and runs it as a
// child process, restarting it if it stops.
package frp

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Version is the frpc release EdgeTAK installs.
const Version = "0.71.1"

// checksums are the release's SHA-256 sums (frp_sha256_checksums.txt).
var checksums = map[string]string{
	"frp_0.71.1_linux_amd64.tar.gz": "1d9875210972a5d1a62b2dc4c48df45ab0af05d3ca1b5e494a014428259df803",
	"frp_0.71.1_linux_arm64.tar.gz": "8edf590b0c396fa42f9c147121a76af83cfc2002b08f5aef0ea1008c43482019",
	"frp_0.71.1_linux_arm.tar.gz":   "8eb6fe66a3b36db24ca0de44fb01e6742d0ff60e30f462c9662f41813bbf4376",
	"frp_0.71.1_windows_amd64.zip":  "b90074b2b6a1d792a7a40f085ac6d276bcab2d1bc60201f8a86ca53ce65ce821",
}

const (
	maxDownload = 64 << 20
	logLines    = 200
)

// Manager installs and supervises frpc.
type Manager struct {
	Dir    string // <data>/frp: frpc.toml
	BinDir string // <data>/frpc-bin: frpc, downloaded again when missing
	Log    *slog.Logger
	HTTP   *http.Client
	// DownloadBase replaces the GitHub release URL (tests).
	DownloadBase string
	// Checksums replaces the pinned checksums (tests).
	Checksums map[string]string

	mu         sync.Mutex
	enabled    bool
	wake       chan struct{}
	cmd        *exec.Cmd
	exited     chan struct{}
	started    time.Time
	lastExit   string
	lines      []string
	installing bool
	installErr string
}

func (m *Manager) ConfigPath() string { return filepath.Join(m.Dir, "frpc.toml") }

// Binary is where frpc is installed.
func (m *Manager) Binary() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(m.BinDir, "frpc.exe")
	}
	return filepath.Join(m.BinDir, "frpc")
}

// Run supervises frpc until ctx ends: while enabled, it installs frpc if
// needed, runs it, and restarts it when it stops.
func (m *Manager) Run(ctx context.Context, enabled bool) {
	m.mu.Lock()
	m.wake, m.enabled = make(chan struct{}, 1), enabled
	m.mu.Unlock()
	backoff := 2 * time.Second
	wait := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-m.wake:
		case <-time.After(d):
		}
		return true
	}
	for ctx.Err() == nil {
		m.mu.Lock()
		on := m.enabled
		m.mu.Unlock()
		if !on || !fileExists(m.ConfigPath()) {
			if !wait(time.Hour) {
				return
			}
			continue
		}
		if !fileExists(m.Binary()) {
			if err := m.Install(ctx); err != nil {
				m.Log.Error("installing frpc", "err", err)
				if !wait(time.Minute) {
					return
				}
				continue
			}
		}
		began := time.Now()
		if err := m.start(); err != nil {
			m.Log.Error("frpc failed to start", "err", err)
		} else {
			m.mu.Lock()
			exited := m.exited
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				m.stop()
				return
			case <-exited:
			}
		}
		if time.Since(began) > time.Minute {
			backoff = 2 * time.Second
		}
		if !wait(backoff) {
			return
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (m *Manager) kick() {
	m.mu.Lock()
	w := m.wake
	m.mu.Unlock()
	if w != nil {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// SetEnabled connects (starting frpc) or disconnects.
func (m *Manager) SetEnabled(on bool) {
	m.mu.Lock()
	m.enabled = on
	m.mu.Unlock()
	if !on {
		m.stop()
	}
	m.kick()
}

// Restart stops frpc so the supervisor starts it with the current config.
func (m *Manager) Restart() {
	m.stop()
	m.kick()
}

func (m *Manager) start() error {
	// Absolute paths: frpc runs in its config's folder.
	bin, err := filepath.Abs(m.Binary())
	if err != nil {
		return err
	}
	cfg, err := filepath.Abs(m.ConfigPath())
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "-c", cfg)
	cmd.Dir = m.Dir
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	m.mu.Lock()
	m.cmd, m.exited, m.started, m.lines = cmd, exited, time.Now(), nil
	m.mu.Unlock()
	m.Log.Info("frpc started", "pid", cmd.Process.Pid)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			m.note(sc.Text())
		}
		err := cmd.Wait()
		msg := "exited"
		if err != nil {
			msg = err.Error()
		}
		m.mu.Lock()
		m.cmd = nil
		wanted := m.enabled
		if wanted {
			m.lastExit = time.Now().UTC().Format(time.RFC3339) + ": " + msg
		}
		m.mu.Unlock()
		if wanted {
			m.Log.Warn("frpc stopped; restarting", "err", msg)
		}
		close(exited)
	}()
	return nil
}

var reANSI = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func (m *Manager) note(line string) {
	line = reANSI.ReplaceAllString(line, "") // frpc colours its log
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lines = append(m.lines, line)
	if len(m.lines) > logLines {
		m.lines = m.lines[len(m.lines)-logLines:]
	}
}

func (m *Manager) stop() {
	m.mu.Lock()
	cmd, exited := m.cmd, m.exited
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	interrupt(cmd.Process)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-exited
	}
}

// ---- status ----

// Status is the remote access card's state.
type Status struct {
	Installed  bool              `json:"installed"`
	Version    string            `json:"version"`
	Installing bool              `json:"installing,omitempty"`
	InstallErr string            `json:"install_error,omitempty"`
	HasConfig  bool              `json:"has_config"`
	Enabled    bool              `json:"enabled"`
	Running    bool              `json:"running"`
	Since      time.Time         `json:"since,omitzero"`
	Connected  bool              `json:"connected"` // logged in to the FRP server
	LoginError string            `json:"login_error,omitempty"`
	Proxies    map[string]string `json:"proxies"` // name -> "ok" or the error
	LastExit   string            `json:"last_exit,omitempty"`
	Log        []string          `json:"log"`
}

var (
	reProxyOK  = regexp.MustCompile(`\[([^\]]+)\] start proxy success`)
	reProxyErr = regexp.MustCompile(`\[([^\]]+)\] start error: (.*)`)
	reLoginErr = regexp.MustCompile(`(?i)(?:login to (?:the )?server failed|connect to server error): (.*)`)
)

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Version: Version, Installed: fileExists(m.Binary()), Installing: m.installing, InstallErr: m.installErr,
		HasConfig: fileExists(m.ConfigPath()), Enabled: m.enabled, Running: m.cmd != nil, LastExit: m.lastExit,
		Proxies: map[string]string{}, Log: append([]string{}, m.lines...)}
	if st.Running {
		st.Since = m.started.UTC()
	}
	// The log since frpc last started tells how it's doing.
	for _, l := range m.lines {
		switch {
		case strings.Contains(l, "login to server success"):
			st.Connected, st.LoginError = true, ""
		case reLoginErr.MatchString(l):
			st.Connected, st.LoginError = false, reLoginErr.FindStringSubmatch(l)[1]
		case reProxyOK.MatchString(l):
			st.Proxies[reProxyOK.FindStringSubmatch(l)[1]] = "ok"
		case reProxyErr.MatchString(l):
			mm := reProxyErr.FindStringSubmatch(l)
			st.Proxies[mm[1]] = mm[2]
		}
	}
	if !st.Running {
		st.Connected = false
	}
	return st
}

// ---- config ----

var reServerAddr = regexp.MustCompile(`(?m)^\s*serverAddr\s*=\s*"([^"]+)"`)
var reRemotePort = regexp.MustCompile(`(?m)^\s*remotePort\s*=\s*(\d+)`)

// ServerAddr is the FRP server in the saved config ("" if none).
func (m *Manager) ServerAddr() string {
	b, err := os.ReadFile(m.ConfigPath())
	if err != nil {
		return ""
	}
	if mm := reServerAddr.FindSubmatch(b); mm != nil {
		return string(mm[1])
	}
	return ""
}

// RemotePorts lists the ports the saved config opens on the FRP server.
func (m *Manager) RemotePorts() []string {
	b, err := os.ReadFile(m.ConfigPath())
	if err != nil {
		return nil
	}
	var out []string
	for _, mm := range reRemotePort.FindAllSubmatch(b, -1) {
		out = append(out, string(mm[1]))
	}
	return out
}

// SaveConfig checks a new frpc.toml (with frpc itself, once installed) and
// saves it.
func (m *Manager) SaveConfig(ctx context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("the config is empty")
	}
	if !reServerAddr.MatchString(text) {
		return errors.New(`the config needs serverAddr = "your FRP server" (frpc's TOML format)`)
	}
	if err := os.MkdirAll(m.Dir, 0o750); err != nil {
		return err
	}
	tmp := m.ConfigPath() + ".new"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	if fileExists(m.Binary()) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, m.Binary(), "verify", "-c", tmp).CombinedOutput()
		if err != nil {
			os.Remove(tmp)
			return fmt.Errorf("frpc says the config is wrong: %s", strings.TrimSpace(lastLine(string(out))))
		}
	}
	if err := os.Rename(tmp, m.ConfigPath()); err != nil {
		return err
	}
	m.Restart()
	return nil
}

// DeleteConfig disconnects and removes the config.
func (m *Manager) DeleteConfig() error {
	m.SetEnabled(false)
	err := os.Remove(m.ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// ---- install ----

func assetName() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64", "linux/arm64", "linux/arm":
		return "frp_" + Version + "_linux_" + runtime.GOARCH + ".tar.gz", nil
	case "windows/amd64":
		return "frp_" + Version + "_windows_amd64.zip", nil
	}
	return "", fmt.Errorf("EdgeTAK can't install frpc on %s/%s; install it yourself as %s", runtime.GOOS, runtime.GOARCH, "frpc")
}

// Install downloads frpc, checks it against the pinned checksum and puts it
// in place.
func (m *Manager) Install(ctx context.Context) error {
	m.mu.Lock()
	if m.installing {
		m.mu.Unlock()
		return errors.New("already installing")
	}
	m.installing, m.installErr = true, ""
	m.mu.Unlock()
	err := m.install(ctx)
	m.mu.Lock()
	m.installing = false
	if err != nil {
		m.installErr = err.Error()
	}
	m.mu.Unlock()
	return err
}

func (m *Manager) install(ctx context.Context) error {
	asset, err := assetName()
	if err != nil {
		return err
	}
	sums := m.Checksums
	if sums == nil {
		sums = checksums
	}
	want := sums[asset]
	if want == "" {
		return fmt.Errorf("no pinned checksum for %s", asset)
	}
	base := m.DownloadBase
	if base == "" {
		base = "https://github.com/fatedier/frp/releases/download/v" + Version
	}
	m.Log.Info("downloading frpc", "version", Version, "asset", asset)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/"+asset, nil)
	if err != nil {
		return err
	}
	client := m.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading frpc: %v (does the server have internet access?)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading frpc: GitHub answered %s", resp.Status)
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("the frpc download doesn't match its pinned checksum (got %s); not installing", got)
	}
	bin, err := extract(asset, archive)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.BinDir, 0o750); err != nil {
		return err
	}
	tmp := m.Binary() + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.Binary()); err != nil {
		return err
	}
	m.Log.Info("frpc installed", "version", Version)
	return nil
}

func extract(asset string, archive []byte) ([]byte, error) {
	if strings.HasSuffix(asset, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if path.Base(f.Name) == "frpc.exe" {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, maxDownload))
			}
		}
		return nil, errors.New("frpc.exe not found in the download")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, errors.New("frpc not found in the download")
		}
		if path.Base(h.Name) == "frpc" && h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
