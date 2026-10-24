package frp

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// As a stand-in for frpc, the test binary prints what frpc logs.
func TestMain(m *testing.M) {
	if os.Getenv("EDGETAK_FAKE_FRPC") == "1" {
		fakeFRPC()
		return
	}
	os.Exit(m.Run())
}

func fakeFRPC() {
	if len(os.Args) > 1 && os.Args[1] == "verify" {
		b, _ := os.ReadFile(os.Args[3])
		if strings.Contains(string(b), "bad") {
			fmt.Println("[E] parse config error: line 3: expected '='")
			os.Exit(1)
		}
		fmt.Println("frpc: the configuration file is syntactically ok")
		return
	}
	fmt.Println("2026-10-24 06:00:00.000 [I] [client/service.go:295] try to connect to server...")
	fmt.Println("\x1b[1;34m2026-10-24 06:00:00.100 [I] [client/service.go:287] [abc123] login to server success, get run id [abc123]\x1b[0m")
	fmt.Println("2026-10-24 06:00:00.200 [I] [client/control.go:168] [abc123] [edge-streaming] start proxy success")
	fmt.Println("2026-10-24 06:00:00.300 [W] [client/control.go:170] [abc123] [edge-api] start error: port already used")
	time.Sleep(time.Minute)
}

func fakeRelease(t *testing.T, bin []byte) (asset string, archive []byte) {
	t.Helper()
	asset, err := assetName()
	if err != nil {
		t.Skip(err)
	}
	var buf bytes.Buffer
	dir := strings.TrimSuffix(strings.TrimSuffix(asset, ".tar.gz"), ".zip")
	if strings.HasSuffix(asset, ".zip") {
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create(dir + "/frpc.exe")
		w.Write(bin)
		zw.Close()
	} else {
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: dir + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		tw.WriteHeader(&tar.Header{Name: dir + "/frpc", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(bin))})
		tw.Write(bin)
		tw.Close()
		gz.Close()
	}
	return asset, buf.Bytes()
}

func newManager(t *testing.T) *Manager {
	dir := t.TempDir()
	return &Manager{Dir: filepath.Join(dir, "frp"), BinDir: filepath.Join(dir, "frpc-bin"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestInstallChecksPinnedChecksum(t *testing.T) {
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	asset, archive := fakeRelease(t, self)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+asset {
			http.NotFound(w, r)
			return
		}
		w.Write(archive)
	}))
	defer srv.Close()
	m := newManager(t)
	m.DownloadBase = srv.URL

	m.Checksums = map[string]string{asset: strings.Repeat("0", 64)}
	if err := m.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum") || fileExists(m.Binary()) {
		t.Fatalf("tampered download installed: %v", err)
	}
	sum := sha256.Sum256(archive)
	m.Checksums = map[string]string{asset: hex.EncodeToString(sum[:])}
	if err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(m.Binary()); !bytes.Equal(got, self) {
		t.Error("installed frpc differs from the release's")
	}
	if !strings.HasPrefix(m.Binary(), m.BinDir) {
		t.Error("frpc isn't kept apart from the config (backups leave it out)")
	}
}

func TestRunAndStatus(t *testing.T) {
	m := newManager(t)
	t.Setenv("EDGETAK_FAKE_FRPC", "1")
	os.MkdirAll(m.BinDir, 0o750)
	self, _ := os.ReadFile(os.Args[0])
	os.WriteFile(m.Binary(), self, 0o755)

	// Saving checks the config with frpc itself.
	if err := m.SaveConfig(context.Background(), "serverAddr = \"frp.example.com\"\nbad\n"); err == nil || !strings.Contains(err.Error(), "parse config error") {
		t.Errorf("bad config saved: %v", err)
	}
	if err := m.SaveConfig(context.Background(), "nothing here"); err == nil {
		t.Error("a config without serverAddr was saved")
	}
	cfg := "serverAddr = \"frp.example.com\"\nserverPort = 7000\n[[proxies]]\nname = \"edge-streaming\"\nremotePort = 8089\n[[proxies]]\nname = \"edge-api\"\nremotePort = 8443\n"
	if err := m.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if m.ServerAddr() != "frp.example.com" || strings.Join(m.RemotePorts(), ",") != "8089,8443" {
		t.Errorf("parsed %q %v", m.ServerAddr(), m.RemotePorts())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx, true); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	var st Status
	for time.Now().Before(deadline) {
		if st = m.Status(); st.Connected && len(st.Proxies) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !st.Running || !st.Connected || st.Proxies["edge-streaming"] != "ok" || !strings.Contains(st.Proxies["edge-api"], "port already used") {
		t.Fatalf("status = %+v", st)
	}
	for _, l := range st.Log {
		if strings.Contains(l, "\x1b") {
			t.Errorf("colour codes in the log: %q", l)
		}
	}

	m.SetEnabled(false)
	for time.Now().Before(deadline) && m.Status().Running {
		time.Sleep(50 * time.Millisecond)
	}
	if st := m.Status(); st.Running || st.Connected || st.LastExit != "" {
		t.Errorf("after disconnecting: %+v", st)
	}
	if runtime.GOOS == "windows" {
		time.Sleep(200 * time.Millisecond) // let Windows release the file
	}
}

// TestRelativeDataDir: frpc starts with a relative data directory too.
func TestRelativeDataDir(t *testing.T) {
	t.Chdir(t.TempDir())
	m := &Manager{Dir: filepath.Join("data", "frp"), BinDir: filepath.Join("data", "frpc-bin"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t.Setenv("EDGETAK_FAKE_FRPC", "1")
	os.MkdirAll(m.BinDir, 0o750)
	self, _ := os.ReadFile(os.Args[0])
	os.WriteFile(m.Binary(), self, 0o755)
	if err := m.SaveConfig(context.Background(), "serverAddr = \"frp.example.com\"\n"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx, true); close(done) }()
	defer func() { cancel(); <-done }()
	for i := 0; i < 100 && !m.Status().Connected; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if !m.Status().Connected {
		t.Fatalf("frpc didn't start: %+v", m.Status())
	}
	m.SetEnabled(false)
}
