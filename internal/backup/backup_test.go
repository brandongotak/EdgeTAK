package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func TestBackupAndRestore(t *testing.T) {
	dir := t.TempDir()
	data, cfg := filepath.Join(dir, "data"), filepath.Join(dir, "data", "edgetak.json")
	write(t, filepath.Join(data, "db.json"), `{"users":{"old":{}}}`)
	write(t, cfg, `{"server_name":"Alpha"}`)
	write(t, filepath.Join(data, "certs", "ca.pem"), "CA")
	write(t, filepath.Join(data, "certs", "ca-key.pem"), "CA KEY")
	write(t, filepath.Join(data, "packages", "abc"), "package")
	write(t, filepath.Join(data, "history", "h-2026-10-21.jsonl"), "history")
	write(t, filepath.Join(data, "recordings", "cam.mp4"), "video")
	write(t, filepath.Join(data, "mediamtx", "mediamtx"), "binary")
	write(t, filepath.Join(data, "packages", ".upload-123"), "partial")
	write(t, filepath.Join(data, "initial-admin-password.txt"), "secret")

	var buf bytes.Buffer
	db := `{"users":{"alice":{}}}` // the snapshot, not the file
	src := Source{DataDir: data, ConfigPath: cfg, DB: func() ([]byte, error) { return []byte(db), nil }, Version: "v1", ServerName: "Alpha"}
	if err := Write(&buf, src, Options{History: true}); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	tr := tar.NewReader(mustGzip(t, buf.Bytes()))
	for h, err := tr.Next(); err == nil; h, err = tr.Next() {
		names[h.Name] = true
	}
	for _, want := range []string{manifestName, "data/db.json", "config/edgetak.json", "data/certs/ca-key.pem", "data/packages/abc", "data/history/h-2026-10-21.jsonl"} {
		if !names[want] {
			t.Errorf("backup lacks %s", want)
		}
	}
	for _, not := range []string{"data/recordings/cam.mp4", "data/mediamtx/mediamtx", "data/packages/.upload-123", "data/initial-admin-password.txt", "data/edgetak.json"} {
		if names[not] {
			t.Errorf("backup has %s", not)
		}
	}
	if m, err := Check(bytes.NewReader(buf.Bytes())); err != nil || m.ServerName != "Alpha" || !m.History || m.Recordings {
		t.Errorf("Check = %+v %v", m, err)
	}

	// The server changes after the backup.
	write(t, filepath.Join(data, "db.json"), `{"users":{"mallory":{}}}`)
	write(t, cfg, `{"server_name":"Changed"}`)
	write(t, filepath.Join(data, "packages", "new"), "new package")
	os.Remove(filepath.Join(data, "packages", "abc"))
	write(t, filepath.Join(data, "recordings", "later.mp4"), "video 2")

	m, err := Stage(bytes.NewReader(buf.Bytes()), data)
	if err != nil || !Pending(data) || m.Version != "v1" {
		t.Fatalf("Stage = %+v %v", m, err)
	}
	if read(filepath.Join(data, "db.json")) != `{"users":{"mallory":{}}}` {
		t.Error("staging changed the live data")
	}
	applied, err := ApplyPending(data, cfg)
	if err != nil || !applied || Pending(data) {
		t.Fatalf("ApplyPending = %v %v", applied, err)
	}
	for p, want := range map[string]string{
		"db.json":                       db,
		"edgetak.json":                  `{"server_name":"Alpha"}`,
		"packages/abc":                  "package",
		"packages/new":                  "<missing>",
		"certs/ca-key.pem":              "CA KEY",
		"recordings/later.mp4":          "video 2", // not in the backup: kept
		"mediamtx/mediamtx":             "binary",
		".pre-restore/db.json":          `{"users":{"mallory":{}}}`,
		".pre-restore/packages/new":     "new package",
		".pre-restore/edgetak.json":     `{"server_name":"Changed"}`,
		".restore-staging/data/db.json": "<missing>",
	} {
		if got := read(filepath.Join(data, filepath.FromSlash(p))); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	if applied, err := ApplyPending(data, cfg); applied || err != nil {
		t.Errorf("second ApplyPending = %v %v", applied, err)
	}
}

func mustGzip(t *testing.T, b []byte) *gzip.Reader {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return gz
}

func makeTar(t *testing.T, files map[string]string, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range order {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(files[name])), Typeflag: tar.TypeReg})
		tw.Write([]byte(files[name]))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestStageRefusesBadBackups(t *testing.T) {
	manifest := `{"format":1,"version":"v1"}`
	ok := map[string]string{manifestName: manifest, "data/db.json": `{}`, "data/certs/ca.pem": "x", "data/certs/ca-key.pem": "x"}
	base := []string{manifestName, "data/db.json", "data/certs/ca.pem", "data/certs/ca-key.pem"}
	cases := map[string][]byte{
		"not gzip":       []byte("hello"),
		"no manifest":    makeTar(t, ok, base[1:]),
		"escaping path":  makeTar(t, map[string]string{manifestName: manifest, "data/../../evil": "x"}, []string{manifestName, "data/../../evil"}),
		"outside data":   makeTar(t, map[string]string{manifestName: manifest, "etc/passwd": "x"}, []string{manifestName, "etc/passwd"}),
		"missing CA key": makeTar(t, ok, base[:3]),
		"bad database":   makeTar(t, map[string]string{manifestName: manifest, "data/db.json": "{", "data/certs/ca.pem": "x", "data/certs/ca-key.pem": "x"}, base),
		"newer format":   makeTar(t, map[string]string{manifestName: `{"format":99,"version":"v9"}`}, []string{manifestName}),
	}
	for name, b := range cases {
		dir := t.TempDir()
		if _, err := Stage(bytes.NewReader(b), dir); err == nil {
			t.Errorf("%s: accepted", name)
		} else if Pending(dir) {
			t.Errorf("%s: left a pending restore", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "..", "evil")); err == nil {
			t.Errorf("%s: wrote outside the data directory", name)
		}
	}
	if _, err := Stage(bytes.NewReader([]byte("hello")), t.TempDir()); !errors.Is(err, ErrNotBackup) {
		t.Errorf("not gzip: %v", err)
	}
	if _, err := Stage(bytes.NewReader(makeTar(t, ok, base)), t.TempDir()); err != nil {
		t.Errorf("minimal backup refused: %v", err)
	}
	if !strings.Contains(func() string {
		_, err := Stage(bytes.NewReader(cases["newer format"]), t.TempDir())
		return err.Error()
	}(), "newer EdgeTAK") {
		t.Error("newer format: no hint to update")
	}
}
