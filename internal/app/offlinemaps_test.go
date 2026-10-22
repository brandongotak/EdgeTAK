package app

import (
	"bytes"
	"crypto/tls"
	"database/sql"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestOfflineMaps covers MBTiles maps: an admin uploads one, devices fetch
// its tiles over HTTPS with their certificate, and their device profile
// carries it as an ATAK map source.
func TestOfflineMaps(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("cyan1", "password-cyan1", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]

	// A small map: one tile at 2/1/1 (XYZ), stored TMS (row 4-1-1 = 2).
	tile := []byte("\x89PNG\r\n\x1a\n tile 2/1/1")
	path := filepath.Join(t.TempDir(), "area.mbtiles")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`CREATE TABLE metadata (name TEXT, value TEXT)`, `CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB)`,
		`INSERT INTO metadata VALUES ('name', 'Training Area'), ('format', 'png'), ('bounds', '-106,39,-105,40')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Exec(`INSERT INTO tiles VALUES (2, 1, 2, ?)`, tile)
	db.Close()
	file, _ := os.ReadFile(path)

	upload := func(name string, body []byte) int {
		req, _ := http.NewRequest("POST", base+"/admin/api/maps?name="+name, bytes.NewReader(body))
		req.Header.Set("X-EdgeTAK", "1")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := upload("notes.mbtiles", []byte("not a map")); code != 400 {
		t.Errorf("bad map upload = %d", code)
	}
	if code := upload("../escape.mbtiles", file); code != 200 {
		t.Errorf("upload = %d", code) // the path is dropped, the name kept
	}
	if _, err := os.Stat(filepath.Join(a.DataDir, "maps", "escape.mbtiles")); err != nil {
		t.Fatalf("map not stored in the maps folder: %v", err)
	}
	if code := upload("Training%20Area.mbtiles", file); code != 200 {
		t.Fatalf("upload = %d", code)
	}

	device := newAPI(t, a, enroll(t, a, "cyan1", "password-cyan1"))
	st, _, got := device.do("GET", "/tiles/Training%20Area.mbtiles/2/1/1.png", nil)
	if st != 200 || !bytes.Equal(got, tile) {
		t.Errorf("tile = %d %q", st, got)
	}
	if st, _, _ := device.do("GET", "/tiles/Training%20Area.mbtiles/2/1/2", nil); st != 404 {
		t.Errorf("missing tile = %d", st)
	}
	plain := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	resp, _ := plain.Get(base + "/tiles/Training%20Area.mbtiles/2/1/1")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("tile without credentials = %d", resp.StatusCode)
	}

	// Devices get it as a map source, with this server's tile URL.
	_, pf := profileFiles(t, device, "/Marti/api/tls/profile/enrollment?clientUid=ANDROID-cyan1")
	src := pf["EdgeTAK-Training_Area.xml"]
	if !strings.Contains(src, "<name>Training Area</name>") || !strings.Contains(src, "/tiles/Training%20Area.mbtiles/{$z}/{$x}/{$y}</url>") ||
		!strings.Contains(src, "<maxZoom>2</maxZoom>") {
		t.Errorf("map source = %q (files %v)", src, keys(pf))
	}
	// A connection sync only carries maps changed since the last one.
	if _, pf := profileFiles(t, device, "/Marti/api/tls/profile/connection?clientUid=ANDROID-cyan1&syncSecago=1"); pf["EdgeTAK-Training_Area.xml"] != "" {
		t.Error("unchanged map sent again on connect")
	}
	if code, _ := call("PUT", "/admin/api/maps/Training%20Area.mbtiles", map[string]bool{"devices": false}); code != 200 {
		t.Fatalf("turn off = %d", code)
	}
	if _, pf := profileFiles(t, device, "/Marti/api/tls/profile/enrollment?clientUid=ANDROID-cyan1"); pf["EdgeTAK-Training_Area.xml"] != "" {
		t.Error("map sent after turning it off for devices")
	}

	if code, _ := call("DELETE", "/admin/api/maps/Training%20Area.mbtiles", nil); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if st, _, _ := device.do("GET", "/tiles/Training%20Area.mbtiles/2/1/1", nil); st != 404 {
		t.Errorf("deleted map's tile = %d", st)
	}
	resp, _ = hc.Get(base + "/admin/api/maps")
	list, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(list), `"Training Area.mbtiles"`) {
		t.Errorf("deleted map still listed: %s", list)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
