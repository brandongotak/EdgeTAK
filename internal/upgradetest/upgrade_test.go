//go:build upgradetest

// Package upgradetest checks that upgrading keeps a server's data working:
// it runs old EdgeTAK releases (downloaded from GitHub), fills them with
// data, then runs this source tree's EdgeTAK on the same data folder (and
// restores an old backup into it) and checks everything still works:
// accounts, groups, passwords, an enrolled device's certificate on the API
// and the TLS stream, data packages, missions and the CA.
//
// It needs the network, so it only builds with the tag:
//
//	go test -tags upgradetest -count=1 ./internal/upgradetest
//
// The release workflow runs it before publishing.
package upgradetest

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const repo = "brandongotak/EdgeTAK"

// The oldest release, and the first with backups.
const (
	oldestRelease = "v0.2.0"
	backupRelease = "v0.9.0"
)

const kml = `<?xml version="1.0"?><kml xmlns="http://www.opengis.net/kml/2.2"><Document><Placemark><name>Rally</name><Point><coordinates>-104.99,39.74</coordinates></Point></Placemark></Document></kml>`

const mission = "Op Upgrade"

// ---- running servers ----

type server struct {
	t                     *testing.T
	bin, dataDir, cfgPath string
	ports                 map[string]int
	cmd                   *exec.Cmd
	mu                    sync.Mutex
	log                   bytes.Buffer
	started               chan struct{}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newServer(t *testing.T, bin, dataDir string) *server {
	s := &server{t: t, bin: bin, dataDir: dataDir, cfgPath: filepath.Join(filepath.Dir(dataDir), filepath.Base(dataDir)+".json"), ports: map[string]int{}}
	for _, k := range []string{"tcp", "tls", "https", "enroll", "http"} {
		s.ports[k] = freePort(t)
	}
	cfg := map[string]any{"server_name": "Upgrade Test", "public_host": "127.0.0.1", "log_level": "info"}
	for k, p := range s.ports {
		cfg[k+"_addr"] = fmt.Sprintf("127.0.0.1:%d", p)
	}
	b, _ := json.Marshal(cfg)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *server) withBinary(bin string) *server {
	s.bin = bin
	return s
}

func (s *server) start() {
	s.t.Helper()
	s.mu.Lock()
	s.log.Reset()
	s.mu.Unlock()
	s.started = make(chan struct{})
	s.cmd = exec.Command(s.bin, "-config", s.cfgPath, "-data", s.dataDir)
	out, _ := s.cmd.StdoutPipe()
	s.cmd.Stderr = s.cmd.Stdout
	if err := s.cmd.Start(); err != nil {
		s.t.Fatalf("start %s: %v", s.bin, err)
	}
	once := sync.Once{}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			s.mu.Lock()
			s.log.WriteString(sc.Text() + "\n")
			s.mu.Unlock()
			if strings.Contains(sc.Text(), "EdgeTAK started") {
				once.Do(func() { close(s.started) })
			}
		}
	}()
	select {
	case <-s.started:
	case <-time.After(60 * time.Second):
		s.stop()
		s.t.Fatalf("%s didn't start:\n%s", filepath.Base(s.bin), s.logText())
	}
}

func (s *server) stop() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	if runtime.GOOS != "windows" {
		s.cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { s.cmd.Wait(); close(done) }()
		select {
		case <-done:
			s.cmd = nil
			return
		case <-time.After(20 * time.Second):
		}
	}
	s.cmd.Process.Kill()
	s.cmd.Wait()
	s.cmd = nil
}

func (s *server) logText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.log.String()
}

func (s *server) url(port string) string { return fmt.Sprintf("https://127.0.0.1:%d", s.ports[port]) }

// ---- clients ----

func insecure(certs ...tls.Certificate) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, Certificates: certs}}}
}

type adminAPI func(method, path string, body any) (int, []byte)

// admin signs in with the first-run password from passwordDir.
func (s *server) admin(passwordDir string) adminAPI {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(passwordDir, "initial-admin-password.txt"))
	if err != nil {
		s.t.Fatal(err)
	}
	pw := strings.TrimSpace(strings.TrimPrefix(strings.Split(string(raw), "\n")[1], "password: "))
	jar, _ := cookiejar.New(nil)
	c := insecure()
	c.Jar = jar
	b, _ := json.Marshal(map[string]string{"username": "admin", "password": pw})
	resp, err := c.Post(s.url("https")+"/admin/api/login", "application/json", bytes.NewReader(b))
	if err != nil {
		s.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		s.t.Fatalf("admin sign-in = %d", resp.StatusCode)
	}
	return func(method, path string, body any) (int, []byte) {
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, s.url("https")+path, r)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-EdgeTAK", "1")
		resp, err := c.Do(req)
		if err != nil {
			s.t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
}

func caFingerprint(call adminAPI) string {
	_, b := call("GET", "/admin/api/ca.pem", nil)
	blk, _ := pem.Decode(b)
	if blk == nil {
		return ""
	}
	sum := sha256.Sum256(blk.Bytes)
	return hex.EncodeToString(sum[:])
}

func sa() string {
	now := time.Now().UTC()
	ts := func(d time.Duration) string { return now.Add(d).Format("2006-01-02T15:04:05.000Z") }
	return fmt.Sprintf(`<event version="2.0" uid="ANDROID-alice" type="a-f-G-U-C" how="m-g" time="%s" start="%s" stale="%s"><point lat="39.74" lon="-104.99" hae="0" ce="9" le="9"/><detail><contact callsign="ALICE" endpoint="*:-1:stcp"/><__group name="Cyan" role="Team Member"/><takv device="Test" platform="ATAK-CIV" os="34" version="5.6.0"/></detail></event>`, ts(0), ts(0), ts(5*time.Minute))
}

func (s *server) connectStream(cert tls.Certificate) error {
	conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.ports["tls"]), &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{cert}})
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(sa())); err != nil {
		return err
	}
	// The server closes a connection it refuses; one it accepts stays open.
	conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	buf := make([]byte, 4096)
	for {
		if _, err := conn.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil
			}
			return fmt.Errorf("the server closed the stream: %v", err)
		}
	}
}

// ---- seed and verify ----

type seeded struct {
	cert          tls.Certificate
	hash, caPrint string
}

// seed fills a running server: groups, users, an enrolled device, a data
// package and a mission holding it.
func (s *server) seed() seeded {
	t := s.t
	call := s.admin(s.dataDir)
	for _, g := range []string{"Cyan", "Red"} {
		if code, b := call("POST", "/admin/api/groups", map[string]string{"name": g, "description": g + " team"}); code >= 300 {
			t.Fatalf("create group %s = %d %s", g, code, b)
		}
	}
	for _, u := range []struct{ name, group string }{{"alice", "Cyan"}, {"bob", "Red"}, {"carol", ""}} {
		groups := []string{}
		if u.group != "" {
			groups = append(groups, u.group)
		}
		if code, b := call("POST", "/admin/api/users", map[string]any{"username": u.name, "password": "password-" + u.name, "groups": groups}); code >= 300 {
			t.Fatalf("create user %s = %d %s", u.name, code, b)
		}
	}

	// Enroll alice's device as ATAK does.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "alice"}}, key)
	req, _ := http.NewRequest("POST", s.url("enroll")+"/Marti/api/tls/signClient/v2?clientUid=ANDROID-alice&version=5.5",
		bytes.NewReader(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})))
	req.SetBasicAuth("alice", "password-alice")
	req.Header.Set("Accept", "application/json")
	resp, err := insecure().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(out["signedCert"]), ""))
	if resp.StatusCode != 200 || err != nil {
		t.Fatalf("enroll = %d %v", resp.StatusCode, err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	if err := s.connectStream(cert); err != nil {
		t.Fatalf("device stream: %v", err)
	}

	dev := insecure(cert)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("assetfile", "rally.kml")
	io.WriteString(fw, kml)
	mw.Close()
	resp, err = dev.Post(s.url("https")+"/Marti/sync/missionupload?filename=rally.kml&creatorUid=ANDROID-alice", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := regexp.MustCompile(`hash=([0-9a-f]{64})`).FindStringSubmatch(string(b))
	if resp.StatusCode != 200 || m == nil {
		t.Fatalf("upload = %d %s", resp.StatusCode, b)
	}
	hash := m[1]
	mreq, _ := http.NewRequest("PUT", s.url("https")+"/Marti/api/missions/"+url.PathEscape(mission)+"?creatorUid=ANDROID-alice&group=Cyan", nil)
	if resp, err = dev.Do(mreq); err != nil || resp.StatusCode >= 300 {
		t.Fatalf("create mission: %v %v", err, resp.Status)
	}
	resp.Body.Close()
	cb, _ := json.Marshal(map[string][]string{"hashes": {hash}})
	creq, _ := http.NewRequest("PUT", s.url("https")+"/Marti/api/missions/"+url.PathEscape(mission)+"/contents?creatorUid=ANDROID-alice", bytes.NewReader(cb))
	creq.Header.Set("Content-Type", "application/json")
	if resp, err = dev.Do(creq); err != nil || resp.StatusCode >= 300 {
		t.Fatalf("add mission contents: %v %v", err, resp.Status)
	}
	resp.Body.Close()
	return seeded{cert: cert, hash: hash, caPrint: caFingerprint(call)}
}

// verify checks a running server still has what seed put in it, signing
// in with the first-run password kept in passwordDir.
func (s *server) verify(d seeded, passwordDir string) {
	t := s.t
	t.Helper()
	call := s.admin(passwordDir)
	code, b := call("GET", "/admin/api/users", nil)
	var users []map[string]any
	json.Unmarshal(b, &users)
	by := map[string]map[string]any{}
	for _, u := range users {
		by[fmt.Sprint(u["username"])] = u
	}
	if code != 200 || by["alice"] == nil || by["bob"] == nil || by["carol"] == nil {
		t.Errorf("users = %d %s", code, b)
	} else {
		if !strings.Contains(fmt.Sprint(by["alice"]["groups"]), "Cyan") || !strings.Contains(fmt.Sprint(by["bob"]["groups"]), "Red") {
			t.Errorf("group membership lost: alice %v, bob %v", by["alice"]["groups"], by["bob"]["groups"])
		}
	}
	if code, b := call("GET", "/admin/api/groups", nil); code != 200 || !strings.Contains(string(b), `"Cyan"`) || !strings.Contains(string(b), `"Red"`) {
		t.Errorf("groups = %d %s", code, b)
	}

	req, _ := http.NewRequest("GET", s.url("enroll")+"/Marti/api/tls/config", nil)
	req.SetBasicAuth("bob", "password-bob")
	if resp, err := insecure().Do(req); err != nil || resp.StatusCode != 200 {
		t.Errorf("bob's password: %v %v", err, resp)
	} else {
		resp.Body.Close()
	}

	dev := insecure(d.cert)
	resp, err := dev.Get(s.url("https") + "/Marti/api/missions/" + url.PathEscape(mission))
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(mb), d.hash) {
		t.Errorf("mission with the old certificate = %d %s", resp.StatusCode, mb)
	}
	resp, err = dev.Get(s.url("https") + "/Marti/sync/content?hash=" + d.hash)
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(pb) != kml {
		t.Errorf("package download = %d (%d bytes)", resp.StatusCode, len(pb))
	}
	if err := s.connectStream(d.cert); err != nil {
		t.Errorf("old certificate on the TLS stream: %v", err)
	}
	if code, b := call("GET", "/admin/api/packages", nil); code != 200 || !strings.Contains(string(b), d.hash) {
		t.Errorf("packages = %d %s", code, b)
	}
	if code, b := call("GET", "/admin/api/missions", nil); code != 200 || !strings.Contains(string(b), mission) {
		t.Errorf("missions = %d %s", code, b)
	}
	if got := caFingerprint(call); got != d.caPrint {
		t.Errorf("the CA changed: %s, was %s", got, d.caPrint)
	}
}

// consoleLoads checks every console page's API answers on the upgraded data.
func (s *server) consoleLoads(passwordDir string) {
	call := s.admin(passwordDir)
	for _, p := range []string{"status", "users", "groups", "certs", "packages", "missions", "clients", "overlays", "webhooks",
		"schedules", "audit", "login-protection", "events", "drawings", "geofences", "apps", "maps", "takgov", "sim",
		"history/settings", "lost-contact", "backup", "federation", "profiles", "video", "registration", "retention",
		"emergencies", "map/sources", "health", "send/history", "chat/rooms"} {
		if code, b := call("GET", "/admin/api/"+p, nil); code != 200 {
			s.t.Errorf("console API %s = %d %s", p, code, b)
		}
	}
}

// noErrors fails on errors the server logged.
func (s *server) noErrors() {
	for _, line := range strings.Split(s.logText(), "\n") {
		if strings.Contains(line, "level=ERROR") {
			s.t.Errorf("logged an error: %s", line)
		}
	}
}

// ---- binaries ----

// release downloads an old release's binary for this system.
func release(t *testing.T, dir, version string) string {
	t.Helper()
	name := fmt.Sprintf("edgetak-%s-%s-%s", version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	resp, err := http.Get(fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, version, name))
	if err != nil {
		t.Fatalf("download %s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download %s: %s", name, resp.Status)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

// current builds this source tree's EdgeTAK.
func current(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "edgetak-current")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", path, "../../cmd/edgetak")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return path
}

// ---- the tests ----

func TestUpgradeInPlace(t *testing.T) {
	dir := t.TempDir()
	old, cur := release(t, dir, oldestRelease), current(t, dir)
	data := filepath.Join(dir, "data")
	s := newServer(t, old, data)
	s.start()
	d := s.seed()
	s.verify(d, data) // the checks hold before upgrading
	s.stop()
	if t.Failed() {
		t.Fatalf("%s:\n%s", oldestRelease, s.logText())
	}

	s.withBinary(cur).start()
	defer s.stop()
	s.verify(d, data)
	s.consoleLoads(data)
	s.noErrors()
	if t.Failed() {
		t.Logf("upgraded server's log:\n%s", s.logText())
	}
}

func TestRestoreOldBackup(t *testing.T) {
	dir := t.TempDir()
	old, cur := release(t, dir, backupRelease), current(t, dir)
	oldData := filepath.Join(dir, "old")
	s := newServer(t, old, oldData)
	s.start()
	d := s.seed()
	call := s.admin(oldData)
	code, backup := call("GET", "/admin/api/backup/download", nil)
	s.stop()
	if code != 200 || len(backup) < 1000 {
		t.Fatalf("backup download = %d (%d bytes)", code, len(backup))
	}
	file := filepath.Join(dir, "backup.tar.gz")
	os.WriteFile(file, backup, 0o600)

	// Restore it into this EdgeTAK on an empty data folder.
	newData := filepath.Join(dir, "new")
	r := newServer(t, cur, newData)
	if out, err := exec.Command(cur, "-config", r.cfgPath, "-data", newData, "-restore", file).CombinedOutput(); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	// The backup carries the old configuration; give it this test's ports.
	cfg, _ := json.Marshal(map[string]any{"server_name": "Upgrade Test", "public_host": "127.0.0.1", "log_level": "info",
		"tcp_addr": fmt.Sprintf("127.0.0.1:%d", r.ports["tcp"]), "tls_addr": fmt.Sprintf("127.0.0.1:%d", r.ports["tls"]),
		"https_addr": fmt.Sprintf("127.0.0.1:%d", r.ports["https"]), "enroll_addr": fmt.Sprintf("127.0.0.1:%d", r.ports["enroll"]),
		"http_addr": fmt.Sprintf("127.0.0.1:%d", r.ports["http"])})
	os.WriteFile(r.cfgPath, cfg, 0o600)
	r.start()
	defer r.stop()
	if _, err := os.Stat(filepath.Join(newData, "initial-admin-password.txt")); err == nil {
		t.Error("a new admin account was made though the backup has one")
	}
	r.verify(d, oldData) // the admin password is the old server's
	r.consoleLoads(oldData)
	r.noErrors()
	if t.Failed() {
		t.Logf("restored server's log:\n%s", r.logText())
	}
}
