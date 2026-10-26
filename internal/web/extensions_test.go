package web

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/config"
	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/extension"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// TestExtensionHelper is the extension TestExtensionEndToEnd runs: this
// test binary started again with EDGETAK_TEST_EXTENSION set.
func TestExtensionHelper(t *testing.T) {
	if os.Getenv("EDGETAK_TEST_EXTENSION") == "" {
		t.Skip("run by TestExtensionEndToEnd")
	}
	base, token := os.Getenv("EDGETAK_URL"), os.Getenv("EDGETAK_TOKEN")
	call := func(method, path string, body io.Reader) *http.Response {
		req, _ := http.NewRequest(method, base+path, body)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println("error:", err)
			os.Exit(1)
		}
		return resp
	}
	// Its page answers with the user the console says is signed in.
	go http.ListenAndServe("127.0.0.1:"+os.Getenv("EDGETAK_PAGE_PORT"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "page %s for %s cookie=%q", r.URL.Path, r.Header.Get("X-EdgeTAK-User"), r.Header.Get("Cookie"))
	}))
	if resp := call("GET", "/api/v1/missions", nil); resp.StatusCode != http.StatusForbidden {
		fmt.Println("data:read was not refused:", resp.Status)
	}
	stream := call("GET", "/api/v1/cot?types=a-f", nil)
	call("POST", "/api/v1/cot", strings.NewReader(`<event version="2.0" uid="tracker-1" type="a-f-G-U-C" how="m-g" time="2026-10-26T00:00:00Z" start="2026-10-26T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="1" lon="2" hae="0" ce="9" le="9"/><detail><contact callsign="Tracker 1"/></detail></event>`))
	sc := bufio.NewScanner(stream.Body)
	for sc.Scan() {
		var ev struct{ UID, Callsign string }
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.UID != "" {
			os.WriteFile(filepath.Join(os.Getenv("EDGETAK_DATA_DIR"), "got.txt"), []byte(ev.UID+" "+ev.Callsign), 0o600)
		}
	}
	select {}
}

func TestExtensionEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a process")
	}
	data := t.TempDir()
	st, err := store.Open(filepath.Join(data, "db.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.CreateGroup("Cyan", "")
	st.SetExtension("helper", store.ExtensionSettings{GroupsIn: []string{"Cyan"}, GroupsOut: []string{"Cyan"}})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{Store: st, Router: router.New(log, true), Log: log, Cfg: config.Config{ServerName: "Test"},
		Extensions: &extension.Manager{Dir: filepath.Join(data, "extensions"), DataDir: filepath.Join(data, "extension-data"), Log: log},
		sessions:   map[string]session{"admin-session": {username: "admin", expires: time.Now().Add(time.Hour)}}}
	st.CreateUser("admin", "password123", nil, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Extensions.Load(ctx); err != nil {
		t.Fatal(err)
	}
	s.extensionsStart(ctx)

	exe, _ := os.Executable()
	manifest, _ := json.Marshal(map[string]any{"id": "helper", "name": "Helper", "version": "1.0",
		"run": map[string][]string{"*": {exe, "-test.run=^TestExtensionHelper$"}}, "permissions": []string{"cot:read", "cot:write"}, "page": true})
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	f, _ := zw.Create("helper/plugin.json")
	f.Write(manifest)
	zw.Close()
	if _, _, err := s.Extensions.Install(zb.Bytes()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDGETAK_TEST_EXTENSION", "1")

	// A device in Cyan gets the extension's marker and sends it a position.
	dev := router.NewClient("192.0.2.1:1", "tls", "alice", []string{"Cyan"}, []string{"Cyan"}, nil)
	s.Router.Register(dev)
	if err := s.Extensions.Enable("helper"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-dev.Queue():
		if ev.UID != "tracker-1" {
			t.Fatalf("device got %s", ev.UID)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the extension's CoT never arrived; log: %q", s.Extensions.Logs("helper"))
	}
	// A device connecting later gets the marker replayed.
	late := router.NewClient("192.0.2.2:1", "tls", "bob", []string{"Cyan"}, []string{"Cyan"}, nil)
	s.Router.Register(late)
	select {
	case ev := <-late.Queue():
		if ev.UID != "tracker-1" {
			t.Errorf("late device got %s", ev.UID)
		}
	case <-time.After(5 * time.Second):
		t.Error("the marker was not replayed to a device connecting later")
	}

	got := filepath.Join(data, "extension-data", "helper", "got.txt")
	pos, _ := cot.ParseXML([]byte(`<event version="2.0" uid="alice-uid" type="a-f-G-U-C" how="m-g" time="2026-10-26T00:00:00Z" start="2026-10-26T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="1" lon="2" hae="0" ce="9" le="9"/><detail><contact callsign="Alice"/></detail></event>`))
	deadline := time.Now().Add(20 * time.Second)
	for {
		s.Router.Route(dev, pos)
		if b, _ := os.ReadFile(got); string(b) == "alice-uid Alice" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the extension never received the device's position; log: %q", s.Extensions.Logs("helper"))
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Its page, through the console: the admin's name, not their cookie.
	mux := http.NewServeMux()
	s.extensionRoutes(mux)
	req := httptest.NewRequest("GET", "/admin/ext/helper/settings?x=1", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "admin-session"})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if body := w.Body.String(); w.Code != http.StatusOK || body != `page /settings for admin cookie=""` {
		t.Errorf("page: %d %q", w.Code, body)
	}
	if w.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q", w.Header().Get("X-Frame-Options"))
	}
	// Without signing in, nothing reaches the extension.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/admin/ext/helper/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("page without a session: %d", w.Code)
	}

	s.Extensions.Disable("helper")
	if st, _ := s.Extensions.Status("helper"); st.Running {
		t.Error("still running after Disable")
	}
	if len(s.Router.Taps()) != 0 {
		t.Error("the tap outlived the extension")
	}
}

func TestExtensionAPIAuth(t *testing.T) {
	s := &Server{}
	s.ext.sessions = map[string]*extSession{"tok": {m: extension.Manifest{ID: "x", Permissions: []string{"cot:read"}}}}
	h := s.extensionAPI()
	for _, tc := range []struct {
		auth, path string
		want       int
	}{
		{"", "/api/v1/info", http.StatusUnauthorized},
		{"Bearer nope", "/api/v1/info", http.StatusUnauthorized},
		{"Bearer tok", "/api/v1/groups", http.StatusForbidden},
		{"Bearer tok", "/api/v1/history", http.StatusForbidden},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%q %s: %d, want %d", tc.auth, tc.path, w.Code, tc.want)
		}
	}
	req := httptest.NewRequest("POST", "/api/v1/cot", strings.NewReader("<event/>"))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("sending without cot:write: %d", w.Code)
	}
}

func TestSplitEvents(t *testing.T) {
	got := splitEvents([]byte(`<?xml version="1.0"?><event uid="a"><detail/></event>
<event uid="b"></event>`))
	if len(got) != 2 || !strings.HasSuffix(string(got[1]), `uid="b"></event>`) {
		t.Errorf("got %q", got)
	}
}
