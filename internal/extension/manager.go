package extension

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const logLines = 300

// Manager installs extensions and supervises their processes.
type Manager struct {
	Dir     string // <data>/extensions: each extension's files, replaced on update
	DataDir string // <data>/extension-data: each extension's own files, kept
	Log     *slog.Logger
	// Prepare runs before each start. It returns the environment the
	// extension gets besides EDGETAK_DATA_DIR and EDGETAK_PAGE_PORT (the
	// API's address and a new token), and a function to run when the
	// process ends.
	Prepare func(m Manifest) (env []string, done func())

	mu   sync.Mutex
	ctx  context.Context
	exts map[string]*ext
}

type ext struct {
	m        Manifest
	enabled  bool
	quit     chan struct{} // closed to stop supervising
	stopped  chan struct{} // closed when supervising has stopped
	cmd      *exec.Cmd
	exited   chan struct{}
	started  time.Time
	lastExit string
	restarts int
	now      bool // restart at once (Restart), not after a pause
	pagePort int
	lines    []string
}

// Status is an extension's state for the console.
type Status struct {
	Manifest
	Enabled  bool      `json:"enabled"`
	Running  bool      `json:"running"`
	Started  time.Time `json:"started,omitzero"`
	LastExit string    `json:"last_exit,omitempty"`
	Restarts int       `json:"restarts,omitempty"`
	Problem  string    `json:"problem,omitempty"` // why it can't run here
}

// Load finds the installed extensions. ctx ends them all when it ends.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	m.ctx, m.exts = ctx, map[string]*ext{}
	m.mu.Unlock()
	go func() {
		<-ctx.Done()
		for _, id := range m.ids() {
			m.Disable(id)
		}
	}()
	entries, err := os.ReadDir(m.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.Dir, e.Name(), ManifestFile))
		if err != nil {
			continue
		}
		man, err := ParseManifest(b)
		if err != nil || man.ID != e.Name() {
			m.Log.Warn("extension skipped", "dir", e.Name(), "err", err)
			continue
		}
		m.mu.Lock()
		m.exts[man.ID] = &ext{m: man}
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) ids() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.exts))
	for id := range m.exts {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Install adds an extension from a zip, or updates the installed one with
// the same id (stopping it while its files are replaced, then starting it
// again if it was running). It returns the manifest and whether the
// extension was already installed.
func (m *Manager) Install(data []byte) (Manifest, bool, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Manifest{}, false, errors.New("the file is not a zip")
	}
	man, prefix, err := readZip(zr)
	if err != nil {
		return man, false, err
	}
	tmp := filepath.Join(m.Dir, ".install-"+man.ID)
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return man, false, err
	}
	defer os.RemoveAll(tmp)
	if err := unpack(zr, prefix, tmp, man); err != nil {
		return man, false, err
	}
	m.mu.Lock()
	e, existed := m.exts[man.ID]
	wasOn := existed && e.enabled
	m.mu.Unlock()
	if wasOn {
		m.Disable(man.ID)
	}
	dir := filepath.Join(m.Dir, man.ID)
	if err := os.RemoveAll(dir); err != nil {
		return man, existed, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return man, existed, err
	}
	m.mu.Lock()
	if e == nil {
		e = &ext{}
		m.exts[man.ID] = e
	}
	e.m, e.lastExit, e.restarts, e.lines = man, "", 0, nil
	m.mu.Unlock()
	if wasOn {
		err = m.Enable(man.ID)
	}
	return man, existed, err
}

// Remove stops and deletes an extension, and its own files if data is set.
func (m *Manager) Remove(id string, data bool) error {
	if _, ok := m.Manifest(id); !ok {
		return os.ErrNotExist
	}
	m.Disable(id)
	m.mu.Lock()
	delete(m.exts, id)
	m.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(m.Dir, id)); err != nil {
		return err
	}
	if data {
		return os.RemoveAll(filepath.Join(m.DataDir, id))
	}
	return nil
}

// Manifest returns an installed extension's manifest.
func (m *Manager) Manifest(id string) (Manifest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.exts[id]; ok {
		return e.m, true
	}
	return Manifest{}, false
}

// Enable runs the extension, restarting it whenever it stops.
func (m *Manager) Enable(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.exts[id]
	if !ok {
		return os.ErrNotExist
	}
	if _, err := e.m.Command(); err != nil {
		return err
	}
	if e.enabled || m.ctx == nil || m.ctx.Err() != nil {
		return nil
	}
	e.enabled, e.quit, e.stopped = true, make(chan struct{}), make(chan struct{})
	go m.supervise(e, e.quit, e.stopped)
	return nil
}

// Disable stops the extension and waits until it has ended.
func (m *Manager) Disable(id string) {
	m.mu.Lock()
	e, ok := m.exts[id]
	if !ok || !e.enabled {
		m.mu.Unlock()
		return
	}
	e.enabled = false
	close(e.quit)
	stopped := e.stopped
	m.mu.Unlock()
	<-stopped
}

// Restart stops the running process; it starts again at once.
func (m *Manager) Restart(id string) {
	m.mu.Lock()
	e, ok := m.exts[id]
	if !ok || e.cmd == nil {
		m.mu.Unlock()
		return
	}
	e.now = true
	cmd, exited := e.cmd, e.exited
	m.mu.Unlock()
	stopProcess(cmd, exited)
}

func (m *Manager) supervise(e *ext, quit, stopped chan struct{}) {
	defer close(stopped)
	backoff := 2 * time.Second
	for {
		start := time.Now()
		exited, err := m.start(e)
		if err != nil {
			m.mu.Lock()
			e.lastExit = time.Now().UTC().Format(time.RFC3339) + ": " + err.Error()
			m.mu.Unlock()
			m.Log.Warn("extension did not start", "extension", e.m.ID, "err", err)
		} else {
			select {
			case <-quit:
				m.mu.Lock()
				cmd := e.cmd
				m.mu.Unlock()
				stopProcess(cmd, exited)
				return
			case <-exited:
			}
		}
		m.mu.Lock()
		now := e.now
		e.now = false
		e.restarts++
		m.mu.Unlock()
		if time.Since(start) > time.Minute || now {
			backoff = 2 * time.Second
		}
		wait := backoff
		if now {
			wait = 0
		}
		select {
		case <-quit:
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (m *Manager) start(e *ext) (chan struct{}, error) {
	m.mu.Lock()
	man := e.m
	m.mu.Unlock()
	cmdline, err := man.Command()
	if err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(filepath.Join(m.Dir, man.ID))
	if err != nil {
		return nil, err
	}
	bin := cmdline[0]
	if p := filepath.Join(dir, filepath.FromSlash(bin)); fileExists(p) {
		bin = p
	} else if bin, err = exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("can't find %s: %v", cmdline[0], err)
	}
	data, err := filepath.Abs(filepath.Join(m.DataDir, man.ID))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(data, 0o750); err != nil {
		return nil, err
	}
	env := []string{"EDGETAK_EXTENSION_ID=" + man.ID, "EDGETAK_DATA_DIR=" + data}
	port := 0
	if man.Page {
		if port, err = freePort(); err != nil {
			return nil, err
		}
		env = append(env, "EDGETAK_PAGE_PORT="+strconv.Itoa(port))
	}
	done := func() {}
	if m.Prepare != nil {
		more, d := m.Prepare(man)
		env, done = append(env, more...), d
	}
	cmd := exec.Command(bin, cmdline[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	setProcessGroup(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		done()
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		done()
		return nil, err
	}
	exited := make(chan struct{})
	m.mu.Lock()
	e.cmd, e.exited, e.started, e.pagePort = cmd, exited, time.Now(), port
	m.mu.Unlock()
	m.Log.Info("extension started", "extension", man.ID, "version", man.Version, "pid", cmd.Process.Pid)
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			m.note(e, sc.Text())
		}
		err := cmd.Wait()
		done()
		msg := "exited"
		if err != nil {
			msg = err.Error()
		}
		m.mu.Lock()
		e.cmd, e.pagePort = nil, 0
		wanted := e.enabled && !e.now
		e.lastExit = time.Now().UTC().Format(time.RFC3339) + ": " + msg
		m.mu.Unlock()
		if wanted {
			m.Log.Warn("extension stopped; restarting", "extension", man.ID, "err", msg)
		}
		close(exited)
	}()
	return exited, nil
}

func (m *Manager) note(e *ext, line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.lines = append(e.lines, line)
	if len(e.lines) > logLines {
		e.lines = e.lines[len(e.lines)-logLines:]
	}
}

// stopProcess asks the process to end, and ends it if it hasn't in 10 s.
func stopProcess(cmd *exec.Cmd, exited chan struct{}) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	interrupt(cmd)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		kill(cmd)
		<-exited
	}
}

// List returns every installed extension's state.
func (m *Manager) List() []Status {
	out := []Status{}
	for _, id := range m.ids() {
		if st, ok := m.Status(id); ok {
			out = append(out, st)
		}
	}
	return out
}

// Status returns an extension's state.
func (m *Manager) Status(id string) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.exts[id]
	if !ok {
		return Status{}, false
	}
	st := Status{Manifest: e.m, Enabled: e.enabled, Running: e.cmd != nil, LastExit: e.lastExit, Restarts: e.restarts}
	if st.Running {
		st.Started = e.started
	}
	if _, err := e.m.Command(); err != nil {
		st.Problem = err.Error()
	}
	return st, true
}

// Logs returns the latest lines the extension printed.
func (m *Manager) Logs(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.exts[id]; ok {
		return slices.Clone(e.lines)
	}
	return nil
}

// PagePort is where a running extension serves its page.
func (m *Manager) PagePort(id string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.exts[id]; ok && e.cmd != nil && e.m.Page && e.pagePort != 0 {
		return e.pagePort, true
	}
	return 0, false
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
