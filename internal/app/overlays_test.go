package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// TestOverlays covers overlays: a GeoJSON file becomes a data package the
// devices in its group are offered and can download; devices that connect
// later get it once; a new version is offered again; others never see it.
func TestOverlays(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct{ name, group string }{{"alpha", "Cyan"}, {"bravo", "Cyan"}, {"red1", "Red"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}
	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]
	upload := func(method, path, fileName, content string, fields map[string]string) (int, []map[string]any) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, v := range fields {
			mw.WriteField(k, v)
		}
		fw, _ := mw.CreateFormFile("file", fileName)
		io.WriteString(fw, content)
		mw.Close()
		req, _ := http.NewRequest(method, base+path, &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("X-EdgeTAK", "1")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var list []map[string]any
		json.NewDecoder(resp.Body).Decode(&list)
		return resp.StatusCode, list
	}
	offer := func(e *cot.Event) bool { return e.Type == "b-f-t-r" && strings.Contains(e.Detail.XML, "Search Sectors") }
	urlRE := regexp.MustCompile(`senderUrl="https://[^/]+(/Marti/sync/content\?hash=[0-9a-f]+)"`)

	alphaCert, redCert := enroll(t, a, "alpha", "password-alpha"), enroll(t, a, "red1", "password-red1")
	alpha, red := mustDial(t, a, &alphaCert), mustDial(t, a, &redCert)
	alpha.send(sa("ANDROID-alpha", "ALPHA", "Cyan"))
	red.send(sa("ANDROID-red1", "RED1", "Red"))
	waitFor(t, "devices identified", func() bool {
		n := 0
		for _, c := range a.Router.Clients() {
			if uid, _ := c.Identity(); uid != "" {
				n++
			}
		}
		return n == 2
	})

	if code, _ := upload("POST", "/admin/api/overlays", "notes.txt", "hello", nil); code != 400 {
		t.Errorf("a text file = %d", code)
	}
	geo := `{"type":"FeatureCollection","features":[{"type":"Feature","properties":{"name":"Sector 1","stroke":"#ff0000"},
	  "geometry":{"type":"Polygon","coordinates":[[[-106.1,39.5],[-106.0,39.5],[-106.0,39.6],[-106.1,39.5]]]}}]}`
	code, list := upload("POST", "/admin/api/overlays", "sectors.geojson", geo, map[string]string{"name": "Search Sectors", "groups": `["Cyan"]`})
	if code != 201 || len(list) != 1 || list[0]["format"] != "geojson" || list[0]["items"] != float64(1) {
		t.Fatalf("import = %d %v", code, list)
	}
	id := list[0]["id"].(string)

	// Cyan's device is offered it and can download it; Red's isn't and can't.
	ev := alpha.expect(offer, "the overlay offered to ALPHA")
	m := urlRE.FindStringSubmatch(ev.Detail.XML)
	if m == nil {
		t.Fatalf("offer = %s", ev.Detail.XML)
	}
	st, _, pkg := newAPI(t, a, alphaCert).do("GET", m[1], nil)
	if st != 200 {
		t.Fatalf("download = %d", st)
	}
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	var kml string
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/Search Sectors.kml") {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			kml = string(b)
		}
	}
	if !strings.Contains(kml, "<name>Sector 1</name>") || !strings.Contains(kml, "<Polygon>") {
		t.Errorf("package = %v, kml %q", zr.File, kml)
	}
	red.expectNone(offer, "the overlay offered to RED1")
	if st, _, _ := newAPI(t, a, redCert).do("GET", m[1], nil); st != 404 {
		t.Errorf("RED1 downloading = %d", st)
	}
	if len(a.Store.Packages()) != 0 {
		t.Error("the overlay shows up as a data package")
	}

	// A device that connects later gets it once; reconnecting doesn't resend it.
	bravoCert := enroll(t, a, "bravo", "password-bravo")
	bravo := mustDial(t, a, &bravoCert)
	bravo.send(sa("ANDROID-bravo", "BRAVO", "Cyan"))
	bravo.expect(offer, "the overlay offered to BRAVO on connect")
	bravo.conn.Close()
	bravo = mustDial(t, a, &bravoCert)
	bravo.send(sa("ANDROID-bravo", "BRAVO", "Cyan"))
	bravo.expectNone(offer, "the overlay offered again on reconnect")

	// A new version is offered again; the list counts deliveries.
	geo2 := strings.Replace(geo, "Sector 1", "Sector 1A", 1)
	if code, list := upload("PUT", "/admin/api/overlays/"+id+"/file", "sectors.geojson", geo2, nil); code != 200 || list[0]["version"] != float64(2) {
		t.Fatalf("replace = %d %v", code, list)
	}
	alpha.expect(offer, "version 2 at ALPHA")
	bravo.expect(offer, "version 2 at BRAVO")
	if _, v := call("GET", "/admin/api/overlays", nil); v != nil {
		t.Errorf("list is not an array: %v", v)
	}
	resp, _ := hc.Get(base + "/admin/api/overlays")
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if list[0]["delivered"] != float64(2) || list[0]["waiting"] != float64(0) {
		t.Errorf("counts = %v", list[0])
	}

	// Opening it to every group reaches Red.
	if code, _ := call("PATCH", "/admin/api/overlays/"+id, map[string]any{"groups": []string{}}); code != 200 {
		t.Fatalf("groups = %d", code)
	}
	red.expect(offer, "the overlay offered to RED1 once it is for everyone")

	// Deleting stops it being served.
	call("DELETE", "/admin/api/overlays/"+id, nil)
	if st, _, _ := newAPI(t, a, alphaCert).do("GET", m[1], nil); st != 404 {
		t.Errorf("download after delete = %d", st)
	}
}
