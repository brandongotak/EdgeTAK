package app

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/config"
)

// TestTerraTAK covers the TerraTAK companion against a stand-in TerraTAK:
// its web app only for signed-in administrators on its own port, with
// TerraTAK's http:// addresses rewritten and EdgeTAK's credentials kept from
// it; cross-origin changes refused; and tilesets sent to devices as map
// sources whose tiles devices fetch from the HTTPS port.
func TestTerraTAK(t *testing.T) {
	var mu sync.Mutex
	var gotCookie, gotAuth, gotPost string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotCookie, gotAuth = r.Header.Get("Cookie"), r.Header.Get("Authorization")
		if r.Method == "POST" {
			gotPost = r.URL.Path
		}
		mu.Unlock()
		switch {
		case r.URL.Path == "/":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<!doctype html><title>TerraTAK</title><script src="/assets/index.js"></script>`)
		case r.URL.Path == "/openapi":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"openapi":"3.0.4","info":{"title":"TerraTAK API","version":"9.9.9"},"x":[!!not json]}`)
		case r.URL.Path == "/tilesets" && r.Method == "GET":
			io.WriteString(w, `{"data":[{"id":"abc-123","name":"Ortho","contentType":"imagery","minZoom":2,"maxZoom":17,"footprint":[-106,39,-105,39,-105,40,-106,40]},{"id":"vec-1","name":"Roads","contentType":"vector","minZoom":0,"maxZoom":14}]}`)
		case r.URL.Path == "/tilesets" && r.Method == "POST":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"new"}`)
		case r.URL.Path == "/tilesets/abc-123":
			io.WriteString(w, `{"id":"abc-123","name":"Ortho","contentType":"imagery","minZoom":2,"maxZoom":17}`)
		case r.URL.Path == "/tilesets/vec-1":
			io.WriteString(w, `{"id":"vec-1","name":"Roads","contentType":"vector","minZoom":0,"maxZoom":14}`)
		case r.URL.Path == "/tilesets/abc-123.json":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			io.WriteString(w, `{"mimeType":"image/jpeg","url":"http://`+r.Host+`/tilesets/abc-123/tiles/{$z}/{$x}/{$y}"}`)
		case r.URL.Path == "/tilesets/abc-123/tiles/3/1/2":
			w.Header().Set("Content-Type", "image/jpeg")
			io.WriteString(w, "JPEGDATA")
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	a := startAppWith(t, func(c *config.Config) {
		c.TerraTAK = config.TerraTAK{Enabled: true, URL: fake.URL, Addr: "127.0.0.1:0"}
	})
	if a.Addrs["terratak"] == "" {
		t.Fatal("no TerraTAK listener")
	}
	if _, err := a.Store.CreateUser("bob", "password-bob", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, hc := adminJSON(t, a)
	app := "https://" + a.Addrs["terratak"]
	do := func(c *http.Client, method, url string, hdr map[string]string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader("{}"))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	anon := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}

	// Only administrators reach the app.
	if st, body := do(anon, "GET", app+"/", map[string]string{"Accept": "text/html"}); st != 401 || !strings.Contains(body, "Sign in to EdgeTAK first") {
		t.Errorf("no login, html = %d %s", st, body)
	}
	if st, _ := do(anon, "GET", app+"/tilesets", nil); st != 401 {
		t.Errorf("no login = %d", st)
	}
	bob := map[string]string{"Authorization": basic("bob", "password-bob")}
	if st, _ := do(anon, "GET", app+"/tilesets", bob); st != 403 {
		t.Errorf("non-admin = %d", st)
	}
	// The console session works on the app's port too.
	if st, body := do(hc, "GET", app+"/", nil); st != 200 || !strings.Contains(body, "<title>TerraTAK</title>") {
		t.Errorf("admin app = %d %s", st, body)
	}
	mu.Lock()
	if gotCookie != "" || gotAuth != "" {
		t.Errorf("credentials passed on to TerraTAK: cookie %q, auth %q", gotCookie, gotAuth)
	}
	mu.Unlock()
	// Pointers name the app's TLS address.
	if st, body := do(hc, "GET", app+"/tilesets/abc-123.json?format=takjson", nil); st != 200 || !strings.Contains(body, `"url":"https://`+a.Addrs["terratak"]+`/tilesets/abc-123/tiles/`) {
		t.Errorf("pointer = %d %s", st, body)
	}
	// Changes: same origin only.
	if st, _ := do(hc, "POST", app+"/tilesets", map[string]string{"Origin": "https://evil.example"}); st != 403 {
		t.Errorf("cross-origin POST = %d", st)
	}
	if st, _ := do(hc, "POST", app+"/tilesets", map[string]string{"Origin": app}); st != 201 {
		t.Errorf("same-origin POST = %d", st)
	}
	mu.Lock()
	if gotPost != "/tilesets" {
		t.Errorf("POST reached TerraTAK as %q", gotPost)
	}
	mu.Unlock()

	// The console page.
	st, v := call("GET", "/admin/api/terratak", nil)
	sets, _ := v["tilesets"].([]any)
	if st != 200 || v["version"] != "9.9.9" || v["reachable"] != true || len(sets) != 2 {
		t.Fatalf("terratak view = %d %v", st, v)
	}
	if b, _ := sets[0].(map[string]any)["bounds"].([]any); len(b) != 4 || b[0] != -106.0 || b[3] != 40.0 {
		t.Errorf("bounds = %v", sets[0])
	}
	if st, _ := call("PUT", "/admin/api/terratak/tilesets/vec-1", map[string]any{"devices": true}); st != 400 {
		t.Errorf("vector tileset to devices = %d", st)
	}
	if st, _ := call("PUT", "/admin/api/terratak/tilesets/abc-123", map[string]any{"devices": true}); st != 200 {
		t.Errorf("imagery tileset to devices = %d", st)
	}

	// Devices: the map source in their profile, tiles from the HTTPS port.
	https := "https://" + a.Addrs["https"]
	st, body := do(anon, "GET", https+"/Marti/api/device/profile/connection?syncSecago=-1&clientUid=ANDROID-bob", bob)
	if st != 200 {
		t.Fatalf("profile = %d", st)
	}
	zr, err := zip.NewReader(bytes.NewReader([]byte(body)), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var source string
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "TerraTAK-abc-123.xml") {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			source = string(b)
		}
	}
	if !strings.Contains(source, "<url>"+https+"/terratak/tiles/abc-123/{$z}/{$x}/{$y}</url>") || !strings.Contains(source, "<tileType>jpg</tileType>") ||
		!strings.Contains(source, "<minZoom>2</minZoom>") {
		t.Errorf("map source = %q", source)
	}
	if st, body := do(anon, "GET", https+"/terratak/tiles/abc-123/3/1/2", bob); st != 200 || body != "JPEGDATA" {
		t.Errorf("device tile = %d %q", st, body)
	}
	if st, _ := do(anon, "GET", https+"/terratak/tiles/abc-123/3/1/2", nil); st != 401 {
		t.Errorf("tile without a login = %d", st)
	}
	if st, _ := do(anon, "GET", https+"/terratak/tiles/..%2f..%2fusers/3/1/2", bob); st != 404 {
		t.Errorf("odd tileset ID = %d", st)
	}
	if st, _ := call("PUT", "/admin/api/terratak/tilesets/abc-123", map[string]any{"devices": false}); st != 200 || len(a.Store.TerraTAKTilesets()) != 0 {
		t.Errorf("stop sending = %d", st)
	}
}

func basic(user, pw string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pw))
}
