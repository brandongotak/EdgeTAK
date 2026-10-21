package mediamtx

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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildFake compiles the fake MediaMTX with the given version.
func buildFake(t *testing.T, dir, version string, broken bool) []byte {
	t.Helper()
	// Prebuilt fakes, for running a cross-compiled test where there is no go tool.
	if pre := os.Getenv("MEDIAMTX_FAKES"); pre != "" {
		b, err := os.ReadFile(filepath.Join(pre, "fake-"+version))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	out := filepath.Join(dir, "fake-"+version)
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	ld := "-X main.version=" + version
	if broken {
		ld += " -X main.broken=1"
	}
	cmd := exec.Command("go", "build", "-o", out, "-ldflags", ld, "./testdata/fakemediamtx")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// archiveFor packs bin the way MediaMTX releases do for this platform.
func archiveFor(t *testing.T, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("mediamtx.exe")
		w.Write(bin)
		zw.Create("LICENSE")
		zw.Close()
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "mediamtx", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg})
	tw.Write(bin)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestUpdateAndRollback(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil && os.Getenv("MEDIAMTX_FAKES") == "" {
		t.Skip("needs the go tool to build the fake MediaMTX")
	}
	work := t.TempDir()
	releases := map[string][]byte{} // version -> archive
	sums := map[string]string{}     // version -> checksums.sha256 body
	for _, r := range []struct {
		v      string
		broken bool
		badSum bool
	}{{"v1.0.0", false, false}, {"v2.0.0", false, false}, {"v3.0.0", true, false}, {"v4.0.0", false, true}} {
		arc := archiveFor(t, buildFake(t, work, r.v, r.broken))
		asset, _ := assetName(r.v)
		sum := sha256.Sum256(arc)
		hexSum := hex.EncodeToString(sum[:])
		if r.badSum {
			hexSum = strings.Repeat("0", 64)
		}
		releases[r.v] = arc
		sums[r.v] = hexSum + " *" + asset + "\n" + strings.Repeat("a", 64) + " *other.tar.gz\n"
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/bluenviron/mediamtx/releases/latest":
			fmt.Fprint(w, `{"tag_name":"v2.0.0","published_at":"2026-09-01T00:00:00Z","html_url":"https://example/v2","body":"Fixes."}`)
		case strings.HasPrefix(r.URL.Path, "/bluenviron/mediamtx/releases/download/"):
			parts := strings.Split(r.URL.Path, "/")
			v, file := parts[5], parts[6]
			if file == "checksums.sha256" {
				io.WriteString(w, sums[v])
			} else if want, _ := assetName(v); file == want {
				w.Write(releases[v])
			} else {
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()

	dir := filepath.Join(work, "mediamtx")
	os.MkdirAll(dir, 0o755)
	binName := "mediamtx"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	bin := filepath.Join(dir, binName)
	v1, _ := extractBinary(must(assetName("v1.0.0")), releases["v1.0.0"])
	os.WriteFile(bin, v1, 0o755)
	os.WriteFile(bin+".version", []byte("v1.0.0\n"), 0o644)
	api := freeAddr(t)
	conf := filepath.Join(work, "mediamtx.yml")
	os.WriteFile(conf, []byte("apiAddress: "+api+"\n"), 0o644)

	m := &Manager{Binary: bin, Config: conf, APIAddr: api, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		APIBase: gh.URL, DownloadBase: gh.URL}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	if !m.waitVersion(ctx, "v1.0.0", 20*time.Second) {
		t.Fatalf("v1 did not start; log: %v", m.Status(ctx).Log)
	}
	if _, err := m.CheckLatest(ctx); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(ctx); !st.UpdateAvailable || st.Latest.Version != "v2.0.0" || st.Version != "v1.0.0" {
		t.Fatalf("status before update = %+v", st)
	}

	update := func(v string) UpdateState {
		t.Helper()
		if err := m.StartUpdate(v); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if u := m.Status(ctx).Update; !u.Running {
				return u
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("update to %s did not finish", v)
		return UpdateState{}
	}

	// A good release is installed; the old one is kept for rollback.
	if u := update("v2.0.0"); u.Error != "" || !strings.Contains(u.Result, "v1.0.0 → v2.0.0") {
		t.Fatalf("update to v2 = %+v", u)
	}
	if st := m.Status(ctx); st.Version != "v2.0.0" || st.Previous != "v1.0.0" || st.UpdateAvailable {
		t.Fatalf("after update = %+v", st)
	}

	// A release that does not start is rolled back automatically.
	if u := update("v3.0.0"); !strings.Contains(u.Error, "did not start") {
		t.Fatalf("broken update = %+v", u)
	}
	if v := m.RunningVersion(ctx); v != "v2.0.0" {
		t.Fatalf("after failed update running %q, want v2.0.0", v)
	}

	// A checksum mismatch never touches the installed binary.
	if u := update("v4.0.0"); !strings.Contains(u.Error, "checksum mismatch") {
		t.Fatalf("bad checksum update = %+v", u)
	}
	if v := readVersionFile(bin); v != "v2.0.0" {
		t.Fatalf("version file after refused update = %q", v)
	}

	// Manual rollback to the previous version.
	if res, err := m.Rollback(ctx); err != nil || !strings.Contains(res, "v2.0.0 → v1.0.0") {
		t.Fatalf("rollback = %q, %v", res, err)
	}
	if v := m.RunningVersion(ctx); v != "v1.0.0" {
		t.Fatalf("after rollback running %q", v)
	}

	// Restart and crash recovery.
	before := m.Status(ctx).Restarts
	m.Restart()
	if !m.waitVersion(ctx, "v1.0.0", 20*time.Second) {
		t.Fatal("not running after restart")
	}
	m.mu.Lock()
	proc := m.cmd.Process
	m.mu.Unlock()
	proc.Kill() // a crash
	time.Sleep(300 * time.Millisecond)
	if !m.waitVersion(ctx, "v1.0.0", 20*time.Second) {
		t.Fatal("not restarted after a crash")
	}
	if st := m.Status(ctx); st.Restarts <= before || st.LastExit == "" {
		t.Fatalf("restart accounting = %+v", st)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"v1.21.2", "v1.21.1", true}, {"v1.21.1", "v1.21.1", false}, {"v1.9.0", "v1.10.0", false}, {"v2.0.0", "v1.99.99", true}, {"junk", "v1.0.0", false}} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestCheckArch(t *testing.T) {
	if err := checkArch([]byte("#!/bin/sh\necho hi\n")); runtime.GOOS != "darwin" && err == nil {
		t.Fatal("a shell script was accepted as MediaMTX")
	}
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}
