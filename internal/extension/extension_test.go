package extension

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseManifest(t *testing.T) {
	ok := `{"id":"trackers","name":"Trackers","version":"1.2.0","run":{"*":["python3","main.py"]},"permissions":["cot:write"]}`
	if _, err := ParseManifest([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"id":"Trackers","name":"T","version":"1","run":{"*":["x"]}}`,
		`{"id":"../x","name":"T","version":"1","run":{"*":["x"]}}`,
		`{"id":"t","name":"","version":"1","run":{"*":["x"]}}`,
		`{"id":"t","name":"T","version":"1"}`,
		`{"id":"t","name":"T","version":"1","run":{"*":[]}}`,
		`{"id":"t","name":"T","version":"1","run":{"*":["x"]},"permissions":["root"]}`,
		`not json`,
	} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestCommandForPlatform(t *testing.T) {
	m := Manifest{Run: map[string][]string{"*": {"any"}, runtime.GOOS: {"os"}, runtime.GOOS + "/" + runtime.GOARCH: {"exact"}}}
	if c, _ := m.Command(); c[0] != "exact" {
		t.Errorf("got %v", c)
	}
	delete(m.Run, runtime.GOOS+"/"+runtime.GOARCH)
	if c, _ := m.Command(); c[0] != "os" {
		t.Errorf("got %v", c)
	}
	if _, err := (Manifest{Run: map[string][]string{"plan9": {"x"}}}).Command(); err == nil {
		t.Error("no command for this platform, but no error")
	}
}

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	zw.Close()
	return b.Bytes()
}

const manifest = `{"id":"demo","name":"Demo","version":"1.0","run":{"*":["bin/demo"]}}`

func newManager(t *testing.T) *Manager {
	dir := t.TempDir()
	m := &Manager{Dir: filepath.Join(dir, "ext"), DataDir: filepath.Join(dir, "data"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := m.Load(ctx); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInstall(t *testing.T) {
	m := newManager(t)
	// A zipped folder: plugin.json one level down.
	man, updated, err := m.Install(makeZip(t, map[string]string{"demo/plugin.json": manifest, "demo/bin/demo": "#!/bin/sh\n", "demo/README.md": "hi"}))
	if err != nil || updated || man.ID != "demo" {
		t.Fatalf("install: %v %v %+v", err, updated, man)
	}
	st, err := os.Stat(filepath.Join(m.Dir, "demo", "bin", "demo"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
		t.Error("the run file is not executable")
	}
	os.MkdirAll(filepath.Join(m.DataDir, "demo"), 0o755)
	os.WriteFile(filepath.Join(m.DataDir, "demo", "settings.json"), []byte("{}"), 0o600)

	// An update replaces the files and keeps the extension's data.
	if _, updated, err = m.Install(makeZip(t, map[string]string{"plugin.json": strings.Replace(manifest, "1.0", "1.1", 1), "bin/demo": "x"})); err != nil || !updated {
		t.Fatalf("update: %v %v", err, updated)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "demo", "README.md")); err == nil {
		t.Error("files of the old version are left")
	}
	if man, _ := m.Manifest("demo"); man.Version != "1.1" {
		t.Errorf("version %s after update", man.Version)
	}
	if _, err := os.Stat(filepath.Join(m.DataDir, "demo", "settings.json")); err != nil {
		t.Error("the update lost the extension's data")
	}

	// Found again after a restart.
	m2 := &Manager{Dir: m.Dir, DataDir: m.DataDir, Log: m.Log}
	m2.Load(context.Background())
	if _, ok := m2.Manifest("demo"); !ok {
		t.Error("not found after a restart")
	}

	if err := m.Remove("demo", true); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(m.Dir, "demo"), filepath.Join(m.DataDir, "demo")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s left after Remove", p)
		}
	}
}

func TestInstallRejects(t *testing.T) {
	m := newManager(t)
	for name, files := range map[string]map[string]string{
		"no manifest": {"main.py": ""},
		"zip slip":    {"plugin.json": manifest, "../evil": "x"},
		"absolute":    {"plugin.json": manifest, "/etc/evil": "x"},
		"backslash":   {"plugin.json": manifest, `..\evil`: "x"},
		"deep":        {"a/b/plugin.json": manifest},
	} {
		if _, _, err := m.Install(makeZip(t, files)); err == nil {
			t.Errorf("%s: installed", name)
		}
	}
	if _, _, err := m.Install([]byte("not a zip")); err == nil {
		t.Error("installed a non-zip")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(m.Dir), "evil")); err == nil {
		t.Error("a file was written outside the extension")
	}
	if len(m.List()) != 0 {
		t.Errorf("rejected zips left extensions: %v", m.List())
	}
}
