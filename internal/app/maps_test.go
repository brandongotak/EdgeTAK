package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMapSources(t *testing.T) {
	a := startApp(t)
	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]

	list := func() []map[string]any {
		t.Helper()
		req, _ := http.NewRequest("GET", base+"/admin/api/map/sources", nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out []map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	if l := list(); len(l) != 0 {
		t.Fatalf("initial sources = %v", l)
	}

	if c, _ := call("PUT", "/admin/api/map/sources", []map[string]any{{"name": "Bad", "url": "javascript:alert(1)/{z}/{x}/{y}"}}); c != http.StatusBadRequest {
		t.Fatalf("bad URL accepted: %d", c)
	}
	if c, _ := call("PUT", "/admin/api/map/sources", []map[string]any{{"name": "No placeholders", "url": "https://tiles.example.com/a.png"}}); c != http.StatusBadRequest {
		t.Fatalf("URL without placeholders accepted: %d", c)
	}
	if c, _ := call("PUT", "/admin/api/map/sources", []map[string]any{{"name": "County", "url": "https://{s}.tiles.example.com/{z}/{x}/{y}.png", "overlay": true}}); c != http.StatusOK {
		t.Fatalf("save = %d", c)
	}

	// An ATAK map source file is converted to Leaflet placeholders.
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<customMapSource>
  <name>ESRI Topo</name>
  <minZoom>0</minZoom>
  <maxZoom>18</maxZoom>
  <tileType>image/jpeg</tileType>
  <url>https://{$serverpart}.example.com/tile/{$z}/{$y}/{$x}?a=1&amp;b=2</url>
  <serverParts>t1 t2</serverParts>
</customMapSource>`
	req, _ := http.NewRequest("POST", base+"/admin/api/map/sources/import", strings.NewReader(xml))
	req.Header.Set("X-EdgeTAK", "1")
	resp, err := hc.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("import: %v %v", resp, err)
	}
	resp.Body.Close()
	l := list()
	if len(l) != 2 || l[0]["subdomains"] != "abc" {
		t.Fatalf("sources = %v", l)
	}
	if l[1]["name"] != "ESRI Topo" || l[1]["url"] != "https://{s}.example.com/tile/{z}/{y}/{x}?a=1&b=2" ||
		l[1]["subdomains"] != "t1,t2" || l[1]["max_zoom"] != 18.0 || l[1]["overlay"] != false {
		t.Fatalf("imported = %v", l[1])
	}

	req, _ = http.NewRequest("POST", base+"/admin/api/map/sources/import", strings.NewReader("<kml/>"))
	req.Header.Set("X-EdgeTAK", "1")
	if resp, _ = hc.Do(req); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non map source import = %d", resp.StatusCode)
	}
	resp.Body.Close()
}
