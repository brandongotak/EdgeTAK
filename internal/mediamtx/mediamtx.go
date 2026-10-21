// Package mediamtx runs the MediaMTX media server as a child process of
// EdgeTAK and updates it from GitHub releases.
//
// Running it as a child (rather than its own service) lets the unprivileged
// EdgeTAK process restart, update and roll back MediaMTX from the admin
// console: the binary lives in EdgeTAK's data directory and runs as the same
// user, so no root grant is involved.
package mediamtx

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	repo        = "bluenviron/mediamtx"
	logLines    = 200
	maxDownload = 200 << 20
)

// Manager supervises one MediaMTX process.
type Manager struct {
	Binary  string // path to the mediamtx executable
	Config  string // path to mediamtx.yml
	APIAddr string // MediaMTX control API (host:port)
	Log     *slog.Logger
	// APIAuth adds credentials to requests to MediaMTX's API.
	APIAuth func(*http.Request)
	// GitHub API and download base URLs; overridable in tests.
	APIBase, DownloadBase string

	mu         sync.Mutex
	cmd        *exec.Cmd
	exited     chan struct{}
	started    time.Time
	lastExit   string
	restarts   int
	lines      []string
	stopped    bool // admin-requested stop or update in progress
	restarting bool // the current process is being restarted on purpose
	update     UpdateState
	latest     *Release
	checkedAt  time.Time
	wake       chan struct{}
}

// Release is a MediaMTX release on GitHub.
type Release struct {
	Version   string    `json:"version"`
	Published time.Time `json:"published"`
	URL       string    `json:"url"`
	Notes     string    `json:"notes"`
}

// UpdateState reports a running or finished update.
type UpdateState struct {
	Running bool      `json:"running"`
	Step    string    `json:"step,omitempty"`
	Error   string    `json:"error,omitempty"`
	Result  string    `json:"result,omitempty"`
	At      time.Time `json:"at,omitempty"`
}

// Status is what the admin console shows.
type Status struct {
	Managed         bool        `json:"managed"`
	Running         bool        `json:"running"`
	Version         string      `json:"version"`
	Started         time.Time   `json:"started,omitempty"`
	Restarts        int         `json:"restarts"`
	LastExit        string      `json:"last_exit,omitempty"`
	Previous        string      `json:"previous,omitempty"` // version a rollback would restore
	Latest          *Release    `json:"latest,omitempty"`
	UpdateAvailable bool        `json:"update_available"`
	Update          UpdateState `json:"update"`
	Log             []string    `json:"log"`
	Asset           string      `json:"asset"`
}

func (m *Manager) apiBase() string {
	if m.APIBase != "" {
		return m.APIBase
	}
	return "https://api.github.com"
}

func (m *Manager) downloadBase() string {
	if m.DownloadBase != "" {
		return m.DownloadBase
	}
	return "https://github.com"
}

// ---- process supervision ----

// Run starts MediaMTX and restarts it if it exits, until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.wake = make(chan struct{}, 1)
	m.mu.Unlock()
	backoff := time.Second
	for ctx.Err() == nil {
		m.mu.Lock()
		stopped := m.stopped
		m.mu.Unlock()
		if stopped {
			select {
			case <-ctx.Done():
			case <-m.wake:
			}
			continue
		}
		if _, err := os.Stat(m.Binary); err != nil {
			m.note("MediaMTX binary missing: " + err.Error())
			select {
			case <-ctx.Done():
			case <-m.wake:
			case <-time.After(30 * time.Second):
			}
			continue
		}
		began := time.Now()
		if err := m.start(); err != nil {
			m.Log.Error("MediaMTX failed to start", "err", err)
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
			backoff = time.Second
		}
		m.mu.Lock()
		stopped = m.stopped
		if !stopped {
			m.restarts++
		}
		m.mu.Unlock()
		if !stopped {
			select {
			case <-ctx.Done():
				return
			case <-m.wake:
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

func (m *Manager) start() error {
	cmd := exec.Command(m.Binary, m.Config)
	cmd.Dir = filepath.Dir(m.Binary)
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
	m.cmd, m.exited, m.started = cmd, exited, time.Now()
	m.mu.Unlock()
	m.Log.Info("MediaMTX started", "pid", cmd.Process.Pid, "binary", m.Binary)
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
		stopped := m.stopped || m.restarting
		m.restarting = false
		if !stopped { // only unexpected exits are worth showing
			m.lastExit = time.Now().Format(time.RFC3339) + ": " + msg
		}
		m.mu.Unlock()
		if !stopped {
			m.Log.Warn("MediaMTX stopped; restarting", "err", msg)
		}
		close(exited)
	}()
	return nil
}

// note keeps the last lines of MediaMTX's output for the console.
func (m *Manager) note(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lines = append(m.lines, line)
	if len(m.lines) > logLines {
		m.lines = m.lines[len(m.lines)-logLines:]
	}
}

// stop ends the running process and waits for it.
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

// Restart stops MediaMTX; the supervisor starts it again right away.
func (m *Manager) Restart() {
	m.mu.Lock()
	m.stopped = false
	m.restarting = m.cmd != nil
	m.mu.Unlock()
	m.stop()
	m.kick()
}

func (m *Manager) pause() {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	m.stop()
}

func (m *Manager) resume() {
	m.mu.Lock()
	m.stopped = false
	m.mu.Unlock()
	m.kick()
}

// ---- status ----

// RunningVersion asks the MediaMTX API for its version ("" if unreachable).
func (m *Manager) RunningVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+m.APIAddr+"/v3/info", nil)
	if m.APIAuth != nil {
		m.APIAuth(req)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	var info struct {
		Version string `json:"version"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&info) != nil {
		return ""
	}
	return info.Version
}

func (m *Manager) Status(ctx context.Context) Status {
	v := m.RunningVersion(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Managed: true, Running: m.cmd != nil, Version: v, Restarts: m.restarts, LastExit: m.lastExit,
		Latest: m.latest, Update: m.update, Log: append([]string{}, m.lines...)}
	if a, err := assetName("vX"); err == nil {
		st.Asset = a
	}
	if st.Running {
		st.Started = m.started
	}
	if st.Version == "" {
		st.Version = readVersionFile(m.Binary)
	}
	st.Previous = readVersionFile(m.Binary + ".prev")
	if st.Latest != nil && st.Version != "" {
		st.UpdateAvailable = newer(st.Latest.Version, st.Version)
	}
	return st
}

// versionFile records the version of a binary next to it.
func readVersionFile(bin string) string {
	b, err := os.ReadFile(bin + ".version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

var versionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// newer reports whether version a is later than b.
func newer(a, b string) bool {
	pa, pb := versionRE.FindStringSubmatch(a), versionRE.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	return false
}

// CheckLatest fetches the latest stable release from GitHub.
func (m *Manager) CheckLatest(ctx context.Context) (*Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, m.apiBase()+"/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "EdgeTAK")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub replied %s", res.Status)
	}
	var r struct {
		Tag       string    `json:"tag_name"`
		Published time.Time `json:"published_at"`
		URL       string    `json:"html_url"`
		Body      string    `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&r); err != nil {
		return nil, err
	}
	if !versionRE.MatchString(r.Tag) {
		return nil, fmt.Errorf("unexpected release tag %q", r.Tag)
	}
	rel := &Release{Version: r.Tag, Published: r.Published, URL: r.URL, Notes: truncate(r.Body, 8000)}
	m.mu.Lock()
	m.latest, m.checkedAt = rel, time.Now()
	m.mu.Unlock()
	return rel, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- updates ----

// StartUpdate installs version ("v1.2.3") in the background. Progress is
// reported in Status().Update.
func (m *Manager) StartUpdate(version string) error {
	if !versionRE.MatchString(version) {
		return errors.New("invalid version")
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	m.mu.Lock()
	if m.update.Running {
		m.mu.Unlock()
		return errors.New("an update is already running")
	}
	m.update = UpdateState{Running: true, Step: "starting", At: time.Now()}
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		res, err := m.update1(ctx, version)
		m.mu.Lock()
		m.update = UpdateState{At: time.Now(), Result: res}
		if err != nil {
			m.update.Error = err.Error()
			m.Log.Error("MediaMTX update failed", "version", version, "err", err)
		} else {
			m.Log.Info("MediaMTX updated", "result", res)
		}
		m.mu.Unlock()
	}()
	return nil
}

func (m *Manager) step(s string) {
	m.mu.Lock()
	m.update.Step = s
	m.mu.Unlock()
}

func (m *Manager) update1(ctx context.Context, version string) (string, error) {
	asset, err := assetName(version)
	if err != nil {
		return "", err
	}
	base := m.downloadBase() + "/" + repo + "/releases/download/" + version + "/"
	m.step("downloading checksums")
	sums, err := m.fetch(ctx, base+"checksums.sha256", 1<<20)
	if err != nil {
		return "", err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return "", fmt.Errorf("%s is not in the release's checksums", asset)
	}
	m.step("downloading " + asset)
	archive, err := m.fetch(ctx, base+asset, maxDownload)
	if err != nil {
		return "", err
	}
	m.step("verifying")
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return "", fmt.Errorf("checksum mismatch for %s (got %s, want %s)", asset, got, want)
	}
	bin, err := extractBinary(asset, archive)
	if err != nil {
		return "", err
	}
	if err := checkArch(bin); err != nil {
		return "", err
	}
	m.step("installing")
	newPath := m.Binary + ".new"
	if err := os.WriteFile(newPath, bin, 0o750); err != nil {
		return "", err
	}
	old := readVersionFile(m.Binary)
	if old == "" {
		old = m.RunningVersion(ctx)
	}
	if err := m.swapIn(ctx, newPath, version, old); err != nil {
		return "", err
	}
	return "updated " + firstNonEmpty(old, "unknown") + " → " + version, nil
}

// swapIn replaces the binary with newPath, keeping the old one as .prev,
// and rolls back if the new one does not come up.
func (m *Manager) swapIn(ctx context.Context, newPath, newVersion, oldVersion string) error {
	m.pause()
	defer m.resume()
	prev := m.Binary + ".prev"
	// Set the existing backup aside, so a failed update leaves everything
	// exactly as it was.
	keep := prev + ".keep"
	os.Remove(keep)
	os.Remove(keep + ".version")
	hadKeep := os.Rename(prev, keep) == nil
	if hadKeep {
		os.Rename(prev+".version", keep+".version")
	}
	restoreKeep := func() {
		if hadKeep {
			os.Remove(prev)
			os.Rename(keep, prev)
			os.Rename(keep+".version", prev+".version")
		}
	}
	hadOld := fileExists(m.Binary)
	if hadOld {
		if err := os.Rename(m.Binary, prev); err != nil {
			restoreKeep()
			return err
		}
		os.WriteFile(prev+".version", []byte(oldVersion+"\n"), 0o640)
	}
	if err := os.Rename(newPath, m.Binary); err != nil {
		if hadOld {
			os.Rename(prev, m.Binary)
		}
		restoreKeep()
		return err
	}
	os.WriteFile(m.Binary+".version", []byte(newVersion+"\n"), 0o640)
	m.step("starting " + newVersion)
	m.resume()
	if m.waitVersion(ctx, newVersion, 30*time.Second) {
		os.Remove(keep)
		os.Remove(keep + ".version")
		return nil
	}
	// Roll back.
	m.step("new version did not start; rolling back")
	m.pause()
	failed := m.Binary + ".failed"
	os.Remove(failed)
	os.Rename(m.Binary, failed)
	if hadOld {
		os.Rename(prev, m.Binary)
		os.WriteFile(m.Binary+".version", []byte(oldVersion+"\n"), 0o640)
		os.Remove(prev + ".version")
	}
	restoreKeep()
	m.resume()
	if oldVersion != "" {
		m.waitVersion(ctx, oldVersion, 30*time.Second)
	}
	return fmt.Errorf("MediaMTX %s did not start (see the MediaMTX log); kept %s", newVersion, firstNonEmpty(oldVersion, "the previous version"))
}

// Rollback restores the binary that the last update replaced.
func (m *Manager) Rollback(ctx context.Context) (string, error) {
	prev := m.Binary + ".prev"
	if !fileExists(prev) {
		return "", errors.New("no previous version to roll back to")
	}
	m.mu.Lock()
	if m.update.Running {
		m.mu.Unlock()
		return "", errors.New("an update is running")
	}
	m.update = UpdateState{Running: true, Step: "rolling back", At: time.Now()}
	m.mu.Unlock()
	prevVersion, curVersion := readVersionFile(prev), readVersionFile(m.Binary)
	tmp := m.Binary + ".rollback"
	os.Remove(tmp)
	if err := os.Rename(prev, tmp); err != nil {
		m.finish("", err)
		return "", err
	}
	os.Remove(prev + ".version")
	err := m.swapIn(ctx, tmp, prevVersion, curVersion)
	res := "rolled back " + firstNonEmpty(curVersion, "?") + " → " + firstNonEmpty(prevVersion, "?")
	if err != nil {
		res = ""
	}
	m.finish(res, err)
	return res, err
}

func (m *Manager) finish(res string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.update = UpdateState{At: time.Now(), Result: res}
	if err != nil {
		m.update.Error = err.Error()
	}
}

func (m *Manager) waitVersion(ctx context.Context, version string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if v := m.RunningVersion(ctx); v == version {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

func (m *Manager) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "EdgeTAK")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download of %s failed: %s", url, res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("download too large")
	}
	return b, nil
}

// ---- release assets ----

// assetName is the release file for this machine.
func assetName(version string) (string, error) {
	arch := ""
	switch runtime.GOARCH {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	case "arm":
		arch = armVariant()
	default:
		return "", fmt.Errorf("MediaMTX has no build for %s", runtime.GOARCH)
	}
	switch runtime.GOOS {
	case "linux":
		return "mediamtx_" + version + "_linux_" + arch + ".tar.gz", nil
	case "windows":
		if arch != "amd64" {
			return "", errors.New("MediaMTX for Windows is amd64 only")
		}
		return "mediamtx_" + version + "_windows_amd64.zip", nil
	case "darwin":
		return "mediamtx_" + version + "_darwin_" + arch + ".tar.gz", nil
	}
	return "", fmt.Errorf("MediaMTX has no build for %s", runtime.GOOS)
}

// armVariant picks armv7 or armv6 for 32-bit ARM (EdgeTAK's armv6 build
// also runs on ARMv7 Pis, which should get MediaMTX's faster armv7 build).
func armVariant() string {
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		s := string(b)
		if strings.Contains(s, "ARMv7") || strings.Contains(s, "ARMv8") || strings.Contains(s, "CPU architecture: 7") || strings.Contains(s, "CPU architecture: 8") {
			return "armv7"
		}
	}
	return "armv6"
}

func extractBinary(asset string, archive []byte) ([]byte, error) {
	want := "mediamtx"
	if strings.HasSuffix(asset, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if f.Name == want+".exe" {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, maxDownload))
			}
		}
		return nil, errors.New("mediamtx.exe not found in the archive")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, errors.New("mediamtx not found in the archive")
		}
		if h.Name == want && h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

// checkArch reads the executable header (without running it) to make sure
// it is for this machine.
func checkArch(bin []byte) error {
	switch runtime.GOOS {
	case "windows":
		if len(bin) < 2 || string(bin[:2]) != "MZ" {
			return errors.New("the downloaded file is not a Windows program")
		}
		return nil
	case "linux":
		if len(bin) < 20 || string(bin[:4]) != "\x7fELF" {
			return errors.New("the downloaded file is not a Linux program")
		}
		machine := uint16(bin[18]) | uint16(bin[19])<<8
		want := map[string]uint16{"amd64": 0x3E, "arm64": 0xB7, "arm": 0x28}[runtime.GOARCH]
		if want != 0 && machine != want {
			return fmt.Errorf("the downloaded program is for a different processor (ELF machine %#x)", machine)
		}
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
