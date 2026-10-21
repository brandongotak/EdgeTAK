package web

import (
	"context"
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

func TestSelfUpdate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("console updates are for the Linux installer")
	}
	dir := t.TempDir()
	oldScript, oldStatus, oldAPI := updaterScript, updateStatusFile, githubAPI
	defer func() { updaterScript, updateStatusFile, githubAPI = oldScript, oldStatus, oldAPI }()
	updaterScript = filepath.Join(dir, "edgetak-update.sh")
	updateStatusFile = filepath.Join(dir, "status")
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v0.3.0","published_at":"2026-10-21T00:00:00Z","html_url":"https://example/r","body":"Notes"}`)
	}))
	defer gh.Close()
	githubAPI = gh.URL

	data := filepath.Join(dir, "data")
	s := &Server{PackageDir: filepath.Join(data, "packages"), Version: "v0.2.15", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if ok, why := s.updaterAvailable(); ok || !strings.Contains(why, "Rerun the installer") {
		t.Fatalf("without the updater: %v %q", ok, why)
	}
	os.WriteFile(updaterScript, []byte("#!/bin/sh\n"), 0o755)
	if ok, _ := s.updaterAvailable(); ok {
		t.Fatal("available without the update folder")
	}
	os.MkdirAll(filepath.Join(data, "update"), 0o750)
	if ok, why := s.updaterAvailable(); !ok {
		t.Fatalf("not available: %s", why)
	}

	for _, v := range []string{"", "latest", "0.3.0", "v0.3.0; rm -rf /", "v0.2.14"} {
		if err := s.requestUpdate(v); err == nil {
			t.Errorf("version %q accepted", v)
		}
	}
	if err := s.requestUpdate("v0.3.0"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(s.updateRequestFile()); string(b) != "v0.3.0\n" {
		t.Fatalf("request file = %q", b)
	}

	// The updater's status line is shown, and a running update blocks another.
	os.WriteFile(updateStatusFile, []byte(time.Now().UTC().Format(time.RFC3339)+" running downloading v0.3.0\n"), 0o644)
	if st := readUpdateStatus(); st == nil || st.State != "running" || st.Message != "downloading v0.3.0" {
		t.Fatalf("status = %+v", st)
	}
	if err := s.requestUpdate("v0.3.0"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second request while running = %v", err)
	}
	os.WriteFile(updateStatusFile, []byte(time.Now().UTC().Format(time.RFC3339)+" done updated to v0.3.0\n"), 0o644)

	// Latest release and whether it is newer than this build.
	if v := s.selfUpdateView(); v["available"] != true || v["update_available"] != nil {
		t.Fatalf("view before check = %v", v)
	}
	if err := s.checkSelfUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := s.selfUpdateView(); v["update_available"] != true || v["requested"] != "v0.3.0" {
		t.Fatalf("view after check = %v", v)
	}
	s.Version = "v0.3.0-2-gabc123-dirty" // a development build of the latest release
	if v := s.selfUpdateView(); v["update_available"] != false {
		t.Fatalf("dev build of the latest = %v", v["update_available"])
	}
}
