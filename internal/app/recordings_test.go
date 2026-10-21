package app

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/config"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

func TestVideoRecording(t *testing.T) {
	a := startAppWith(t, func(c *config.Config) {
		c.Video.Enabled = true // MediaMTX itself is not run: no binary configured
		c.Video.AuthAddr = "127.0.0.1:0"
	})
	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]
	conf := func() string { return readFile(t, filepath.Join(a.DataDir, "mediamtx.yml")) }

	// Off by default; the playback server is on, on loopback.
	if c := conf(); !strings.Contains(c, "  record: false\n") || !strings.Contains(c, "playback: true\nplaybackAddress: 127.0.0.1:9996") {
		t.Fatalf("default config:\n%s", c)
	}
	if code, _ := call("PUT", "/admin/api/video/recording", map[string]any{"mode": "sometimes", "keep_days": 7}); code != 400 {
		t.Errorf("bad mode = %d", code)
	}
	// Saved feeds marked Record are recorded by name.
	if code, _ := call("POST", "/admin/api/video/feeds", map[string]any{"alias": "Drone 1", "path": "drone1", "record": true}); code != 200 {
		t.Fatalf("save feed = %d", code)
	}
	if code, _ := call("POST", "/admin/api/video/feeds", map[string]any{"alias": "Gate", "path": "gate"}); code != 200 {
		t.Fatalf("save feed = %d", code)
	}
	if code, _ := call("PUT", "/admin/api/video/recording", map[string]any{"mode": "feeds", "keep_days": 3}); code != 200 {
		t.Fatalf("save settings = %d", code)
	}
	c := conf()
	if !strings.Contains(c, "  'drone1':\n    record: true\n") || strings.Contains(c, "'gate'") || !strings.Contains(c, "recordDeleteAfter: 72h") || !strings.Contains(c, "  record: false\n") {
		t.Errorf("feeds config:\n%s", c)
	}
	wantDir := filepath.ToSlash(filepath.Join(a.DataDir, "recordings"))
	if !strings.Contains(c, "recordPath: '"+wantDir+"/%path/%Y-%m-%d_%H-%M-%S-%f'") {
		t.Errorf("record path not under the data folder (%s):\n%s", wantDir, c)
	}
	call("PUT", "/admin/api/video/recording", map[string]any{"mode": "all", "keep_days": 0})
	if c := conf(); !strings.Contains(c, "  record: true\n") || !strings.Contains(c, "recordDeleteAfter: 0s") || strings.Contains(c, "'drone1'") {
		t.Errorf("all config:\n%s", c)
	}

	// Three segments: two back to back (one recording) and one later.
	dir := filepath.Join(a.DataDir, "recordings", "drone1")
	os.MkdirAll(dir, 0o755)
	start := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	seg := func(at time.Time, length time.Duration, body string) string {
		name := at.Format("2006-01-02_15-04-05") + "-000000.mp4"
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		os.Chtimes(p, at.Add(length), at.Add(length))
		return name
	}
	first := seg(start, 10*time.Minute, "first-segment-bytes")
	seg(start.Add(10*time.Minute), 4*time.Minute, "second")
	seg(start.Add(2*time.Hour), time.Minute, "third")
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644)

	code, st := call("GET", "/admin/api/video/recordings", nil)
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	streams := st["streams"].([]any)
	if len(streams) != 1 {
		t.Fatalf("streams = %v", streams)
	}
	s0 := streams[0].(map[string]any)
	spans := s0["spans"].([]any)
	if s0["alias"] != "Drone 1" || len(spans) != 2 {
		t.Fatalf("stream = %v", s0)
	}
	newest, older := spans[0].(map[string]any), spans[1].(map[string]any)
	if len(older["segments"].([]any)) != 2 || len(newest["segments"].([]any)) != 1 {
		t.Errorf("grouping: %v / %v", older, newest)
	}
	if d := older["duration"].(float64); d < 13*60 || d > 15*60 {
		t.Errorf("recording length = %v s, want 14 min", d)
	}

	// Segments are served with byte ranges, so the browser can seek.
	req, _ := http.NewRequest("GET", base+"/admin/video/recordings/file?"+url.Values{"path": {"drone1"}, "file": {first}}.Encode(), nil)
	req.Header.Set("Range", "bytes=6-12")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(part) != "segment" || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Errorf("range = %d %q %s", resp.StatusCode, part, resp.Header.Get("Content-Type"))
	}
	for _, bad := range []url.Values{{"path": {"../x"}, "file": {first}}, {"path": {"drone1"}, "file": {"../../db.json"}}, {"path": {"drone1"}, "file": {"notes.txt"}}} {
		resp, err := hc.Get(base + "/admin/video/recordings/file?" + bad.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d", bad, resp.StatusCode)
		}
	}

	os.Remove(filepath.Join(dir, "notes.txt")) // other files keep the folder

	// Delete one recording, then the rest.
	q := url.Values{"path": {"drone1"}, "start": {older["start"].(string)}}
	if code, res := call("DELETE", "/admin/api/video/recordings?"+q.Encode(), nil); code != 200 || res["deleted"] != 2.0 {
		t.Fatalf("delete one = %d %v", code, res)
	}
	if _, err := os.Stat(filepath.Join(dir, first)); !os.IsNotExist(err) {
		t.Error("segment file kept")
	}
	if code, res := call("DELETE", "/admin/api/video/recordings?path=drone1", nil); code != 200 || res["deleted"] != 1.0 {
		t.Fatalf("delete all = %d %v", code, res)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("empty stream folder kept")
	}

	// Data retention shows the recording rule.
	if code, r := call("GET", "/admin/api/retention", nil); code != 200 || r["recording"].(map[string]any)["mode"] != "all" {
		t.Errorf("retention = %v", r["recording"])
	}
	if got := a.Store.RecordingSettings(); got != (store.RecordingSettings{Mode: "all", KeepDays: 0}) {
		t.Errorf("stored = %+v", got)
	}
}
