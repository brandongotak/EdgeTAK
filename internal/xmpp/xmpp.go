// Package xmpp runs the Prosody XMPP server for TAK Chat as a child process
// of EdgeTAK, with a configuration EdgeTAK writes.
//
// Prosody is installed from the system's packages (the admin console asks a
// root helper to do it), but EdgeTAK runs it from its own data directory as
// its own user, so the console can change its settings and restart it
// without root. Three Lua modules EdgeTAK writes tie it to EdgeTAK: users
// sign in with their EdgeTAK password (Prosody asks EdgeTAK), they see the
// members of their groups as contacts, and the chat rooms made in the
// console are kept in Prosody. EdgeTAK and Prosody exchange state through
// files in the data directory: state.json (users, groups, rooms) one way,
// status.json and rooms-status.json (who is online, room occupants) back.
package xmpp

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed modules/*.lua
var modules embed.FS

const logLines = 300

// Config is what EdgeTAK's settings make of Prosody's configuration.
type Config struct {
	Domain      string // the XMPP domain (users are name@Domain)
	PublicHost  string // address devices use (for file upload links)
	Port        int    // client connections (5222)
	HTTPSPort   int    // file uploads (5281)
	HTTPPort    int    // local only (5280)
	UploadMB    int
	UploadDays  int
	HistoryDays int
	UserRooms   bool // signed-in users may make rooms
	AuthURL     string
	Secret      string
	CertFile    string
	KeyFile     string
	CAFile      string // devices' certificates are signed by it (certificate sign-in)
}

// State is what Prosody needs to know about EdgeTAK's users.
type State struct {
	Users  map[string]string   `json:"users"`  // username -> display name
	Groups map[string][]string `json:"groups"` // group -> usernames
	Rooms  []Room              `json:"rooms"`
}

// Room is a chat room the console manages.
type Room struct {
	Node        string   `json:"node"` // the room's address is Node@conference.<domain>
	Name        string   `json:"name"`
	Description string   `json:"description"`
	MembersOnly bool     `json:"members_only"`
	Members     []string `json:"members"`
}

// Session is someone connected.
type Session struct {
	User     string  `json:"user"`
	Resource string  `json:"resource"`
	IP       string  `json:"ip"`
	Since    float64 `json:"since"` // Unix seconds
	Show     string  `json:"show"`
	Secure   bool    `json:"secure"`
}

// RoomStatus is a room as Prosody reports it.
type RoomStatus struct {
	JID        string `json:"jid"`
	Name       string `json:"name"`
	Occupants  int    `json:"occupants"`
	Persistent bool   `json:"persistent"`
}

// Status is what the admin console shows.
type Status struct {
	Installed bool         `json:"installed"`
	Binary    string       `json:"binary,omitempty"`
	Running   bool         `json:"running"`
	Version   string       `json:"version,omitempty"`
	Started   time.Time    `json:"started,omitzero"`
	Restarts  int          `json:"restarts"`
	LastExit  string       `json:"last_exit,omitempty"`
	Sessions  []Session    `json:"sessions"`
	Rooms     []RoomStatus `json:"rooms"`
	Log       []string     `json:"log"`
}

// Manager supervises one Prosody process.
type Manager struct {
	Dir    string // <data>/xmpp
	Binary string // prosody executable; "" = look for it
	Log    *slog.Logger

	mu         sync.Mutex
	enabled    bool
	cmd        *exec.Cmd
	exited     chan struct{}
	started    time.Time
	lastExit   string
	restarts   int
	lines      []string
	restarting bool
	wake       chan struct{}
	lastState  []byte
}

// FindBinary returns the prosody executable, if installed.
func (m *Manager) FindBinary() string {
	if m.Binary != "" {
		if _, err := os.Stat(m.Binary); err == nil {
			return m.Binary
		}
		return ""
	}
	for _, p := range []string{"/usr/bin/prosody", "/usr/local/bin/prosody"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("prosody"); err == nil {
		return p
	}
	return ""
}

func (m *Manager) path(name string) string { return filepath.Join(m.Dir, name) }

// Configure writes Prosody's configuration and modules. Restart applies them.
func (m *Manager) Configure(c Config) error {
	if err := os.MkdirAll(m.path("modules"), 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(m.path("data"), 0o750); err != nil {
		return err
	}
	err := fs.WalkDir(modules, "modules", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := modules.ReadFile(p)
		return writeAtomic(m.path(p), b, 0o640)
	})
	if err != nil {
		return err
	}
	return writeAtomic(m.path("prosody.cfg.lua"), []byte(m.config(c)), 0o600)
}

// Lua string literal.
func lua(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(s) + `"`
}

func (m *Manager) config(c Config) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	restrict := "true"
	if c.UserRooms {
		restrict = lua("local")
	}
	w("-- Written by EdgeTAK, which overwrites it: change these settings in the")
	w("-- admin console (Messages -> XMPP chat), not here.")
	w("pidfile = %s", lua(m.path("prosody.pid")))
	w("data_path = %s", lua(m.path("data")))
	w("plugin_paths = { %s }", lua(m.path("modules")))
	w(`log = { { levels = { min = "info" }, to = "console" } }`)
	w("daemonize = false")
	w("admins = { }")
	w(`modules_enabled = {
	"roster"; "saslauth"; "tls"; "disco"; "carbons"; "pep"; "private";
	"blocklist"; "vcard4"; "vcard_legacy"; "bookmarks"; "version"; "uptime";
	"time"; "ping"; "mam"; "csi_simple"; "smacks"; "offline"; "posix"; "edgetak";
}`)
	w(`modules_disabled = { "s2s"; "register" }`)
	w("allow_registration = false")
	w("c2s_require_encryption = true")
	w("c2s_ports = { %d }", c.Port)
	w("c2s_interfaces = { \"*\" }")
	w("s2s_ports = { }")
	w("http_ports = { %d }", c.HTTPPort)
	w(`http_interfaces = { "127.0.0.1" }`)
	w("https_ports = { %d }", c.HTTPSPort)
	w(`https_interfaces = { "*" }`)
	w(`authentication = "edgetak"`)
	w(`storage = "internal"`)
	w("archive_expires_after = %s", lua(fmt.Sprintf("%dd", max(c.HistoryDays, 1))))
	w(`default_archive_policy = true`)
	// Devices may present their EdgeTAK certificate to sign in with it
	// (SASL EXTERNAL); it is optional, and EdgeTAK checks it.
	if c.CAFile != "" {
		w(`ssl = { certificate = %s; key = %s; cafile = %s; verify = { "peer", "client_once" }; verifyext = { "lsec_continue", "lsec_ignore_purpose" } }`,
			lua(c.CertFile), lua(c.KeyFile), lua(c.CAFile))
	} else {
		w("ssl = { certificate = %s; key = %s }", lua(c.CertFile), lua(c.KeyFile))
	}
	w("https_ssl = { certificate = %s; key = %s }", lua(c.CertFile), lua(c.KeyFile))
	w("edgetak_auth_url = %s", lua(c.AuthURL))
	w("edgetak_secret = %s", lua(c.Secret))
	w("edgetak_state_file = %s", lua(m.path("state.json")))
	w("edgetak_status_file = %s", lua(m.path("status.json")))
	w("edgetak_rooms_status_file = %s", lua(m.path("rooms-status.json")))
	w("edgetak_user_host = %s", lua(c.Domain))
	w("")
	w("VirtualHost %s", lua(c.Domain))
	w("")
	w("Component %s \"muc\"", lua("conference."+c.Domain))
	w(`	name = "Chat rooms"`)
	w(`	modules_enabled = { "muc_mam"; "edgetak_rooms" }`)
	w("	restrict_room_creation = %s", restrict)
	w("	muc_room_default_persistent = true")
	w("	muc_room_default_history_length = 50")
	w("")
	w("Component %s \"http_file_share\"", lua("upload."+c.Domain))
	w("	http_file_share_size_limit = %d", int64(max(c.UploadMB, 1))<<20)
	w("	http_file_share_expires_after = %d", max(c.UploadDays, 1)*86400)
	w("	http_host = %s", lua(c.PublicHost))
	w("	http_external_url = %s", lua(fmt.Sprintf("https://%s/", hostPort(c.PublicHost, c.HTTPSPort))))
	return b.String()
}

func hostPort(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// WriteState gives Prosody EdgeTAK's users, groups and rooms (only when they
// changed: Prosody re-reads the file when it does).
func (m *Manager) WriteState(st State) error {
	for g := range st.Groups {
		sort.Strings(st.Groups[g])
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	m.mu.Lock()
	same := bytes.Equal(b, m.lastState)
	m.mu.Unlock()
	if same {
		return nil
	}
	if err := os.MkdirAll(m.Dir, 0o750); err != nil {
		return err
	}
	if err := writeAtomic(m.path("state.json"), b, 0o640); err != nil {
		return err
	}
	m.mu.Lock()
	m.lastState = b
	m.mu.Unlock()
	return nil
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- process supervision ----

// SetEnabled starts or stops Prosody.
func (m *Manager) SetEnabled(on bool) {
	m.mu.Lock()
	was := m.enabled
	m.enabled = on
	m.mu.Unlock()
	if was && !on {
		m.stop()
	}
	m.kick()
}

// Run keeps Prosody running while enabled, until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	if m.wake == nil {
		m.wake = make(chan struct{}, 1)
	}
	wake := m.wake
	m.mu.Unlock()
	backoff := time.Second
	for ctx.Err() == nil {
		m.mu.Lock()
		on := m.enabled
		m.mu.Unlock()
		bin := m.FindBinary()
		if !on || bin == "" {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-time.After(30 * time.Second): // installed meanwhile?
			}
			continue
		}
		began := time.Now()
		if err := m.start(bin); err != nil {
			m.note("can't start Prosody: " + err.Error())
			m.Log.Error("Prosody failed to start", "err", err)
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
		on = m.enabled
		if on {
			m.restarts++
		}
		m.mu.Unlock()
		if on {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

func (m *Manager) start(bin string) error {
	os.Remove(m.path("status.json")) // stale until Prosody writes it
	stopLeftover(m.path("prosody.pid"), m.path("prosody.cfg.lua"))
	cmd := exec.Command(bin, "--config", m.path("prosody.cfg.lua"), "-F")
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
	m.cmd, m.exited, m.started = cmd, exited, time.Now()
	m.mu.Unlock()
	m.Log.Info("Prosody started", "pid", cmd.Process.Pid)
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
		expected := !m.enabled || m.restarting
		m.restarting = false
		if !expected {
			m.lastExit = time.Now().Format(time.RFC3339) + ": " + msg
		}
		m.mu.Unlock()
		if !expected {
			m.Log.Warn("Prosody stopped; restarting", "err", msg)
		}
		close(exited)
	}()
	return nil
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func (m *Manager) note(line string) {
	line = ansi.ReplaceAllString(line, "")
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

func (m *Manager) kick() {
	m.mu.Lock()
	w := m.wake
	if w == nil {
		w = make(chan struct{}, 1)
		m.wake = w
	}
	m.mu.Unlock()
	select {
	case w <- struct{}{}:
	default:
	}
}

// Restart stops Prosody; Run starts it again with the current files.
func (m *Manager) Restart() {
	m.mu.Lock()
	m.restarting = m.cmd != nil
	m.mu.Unlock()
	m.stop()
	m.kick()
}

// Status reports the process and what Prosody last wrote about itself.
func (m *Manager) Status() Status {
	st := Status{Sessions: []Session{}, Rooms: []RoomStatus{}}
	st.Binary = m.FindBinary()
	st.Installed = st.Binary != ""
	m.mu.Lock()
	st.Running = m.cmd != nil
	if st.Running {
		st.Started = m.started
	}
	st.Restarts, st.LastExit = m.restarts, m.lastExit
	st.Log = append([]string{}, m.lines...)
	m.mu.Unlock()
	if !st.Running {
		return st
	}
	// Lua writes an empty list as {}.
	var ps struct {
		Version  string          `json:"version"`
		Sessions json.RawMessage `json:"sessions"`
	}
	if b, err := os.ReadFile(m.path("status.json")); err == nil && json.Unmarshal(b, &ps) == nil {
		st.Version = ps.Version
		var list []Session
		if json.Unmarshal(ps.Sessions, &list) == nil && list != nil {
			st.Sessions = list
		}
		sort.Slice(st.Sessions, func(i, j int) bool {
			return st.Sessions[i].User+st.Sessions[i].Resource < st.Sessions[j].User+st.Sessions[j].Resource
		})
	}
	var rs struct {
		Rooms json.RawMessage `json:"rooms"`
	}
	if b, err := os.ReadFile(m.path("rooms-status.json")); err == nil && json.Unmarshal(b, &rs) == nil {
		var list []RoomStatus
		if json.Unmarshal(rs.Rooms, &list) == nil && list != nil {
			sort.Slice(list, func(i, j int) bool { return list[i].JID < list[j].JID })
			st.Rooms = list
		}
	}
	return st
}
