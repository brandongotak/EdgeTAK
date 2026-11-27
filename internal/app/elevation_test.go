package app

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// dtedTile makes a minimal DTED tile: a UHL record and some data.
func dtedTile(level, lat, lon int) []byte {
	interval := map[int]string{0: "0300", 1: "0030", 2: "0010"}[level]
	ew, ns, alon, alat := 'E', 'N', lon, lat
	if lon < 0 {
		ew, alon = 'W', -lon
	}
	if lat < 0 {
		ns, alat = 'S', -lat
	}
	h := fmt.Sprintf("UHL1%03d0000%c%03d0000%c%s%s%-52s", alon, ew, alat, ns, interval, interval, "")
	return append([]byte(h), bytes.Repeat([]byte{byte(level + 1)}, 200)...)
}

// TestElevation covers DTED tiles: upload, the downloads ATAK makes as it
// would from tak.gov, the device setting, and sending tiles to devices.
func TestElevation(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("cyan1", "password-cyan1", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	cert := enroll(t, a, "cyan1", "password-cyan1")
	device := mustDial(t, a, &cert)
	device.send(sa("ANDROID-cyan1", "CYAN-1", "Cyan"))
	waitFor(t, "device identified", func() bool { return len(a.Router.Clients()) == 1 && a.Router.Clients()[0].LastSA() != nil })

	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]

	// Upload: a zip with tiles under any names, and a loose tile.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, data := range map[string][]byte{"hemi/a.dt0": dtedTile(0, 38, -78), "b.dt0": dtedTile(0, 39, -78),
		"deep/x/c.dt1": dtedTile(1, 38, -78), "notes.txt": []byte("x")} {
		f, _ := zw.Create(name)
		f.Write(data)
	}
	zw.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("files", "tiles.zip")
	fw.Write(zb.Bytes())
	fw, _ = mw.CreateFormFile("files", "n38.dt2")
	fw.Write(dtedTile(2, 38, -78))
	mw.Close()
	req, _ := http.NewRequest("POST", base+"/admin/api/elevation", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-EdgeTAK", "1")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var up struct {
		Added   int
		Skipped []string
		Levels  []struct{ Level, Tiles int }
		Tiles   [][3]int
	}
	json.NewDecoder(resp.Body).Decode(&up)
	resp.Body.Close()
	if resp.StatusCode != 200 || up.Added != 4 || up.Levels[0].Tiles != 2 || up.Levels[1].Tiles != 1 || up.Levels[2].Tiles != 1 || len(up.Tiles) != 4 {
		t.Fatalf("upload = %d %+v", resp.StatusCode, up)
	}

	// ATAK's automatic download, with no credentials.
	anon := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	get := func(path string) (int, []byte) {
		t.Helper()
		r, err := anon.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, b
	}
	st, b := get("/elevation/DTED/w078/n38.dt0.zip")
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if st != 200 || err != nil || len(zr.File) != 1 || zr.File[0].Name != "n38.dt0" {
		t.Fatalf("tile download = %d %v", st, err)
	}
	for _, p := range []string{"/elevation/DTED/w078/n40.dt0.zip", "/elevation/DTED/w078/n38.dt0", "/elevation/DTED/..%2F..%2Fdb.json/x.zip"} {
		if st, _ := get(p); st != 404 {
			t.Errorf("%s = %d", p, st)
		}
	}

	// Streaming to devices needs a trusted certificate; tests have none.
	if st, v := call("PUT", "/admin/api/elevation/settings", map[string]any{"stream": true}); st != 400 || !strings.Contains(mustJSON(v), "trusted certificate") {
		t.Errorf("stream without a public certificate = %d %v", st, v)
	}
	api := newAPI(t, a, cert)
	if _, pf := profileFiles(t, api, "/Marti/api/tls/profile/enrollment?clientUid=ANDROID-cyan1"); pf["elevation.pref"] != "" {
		t.Errorf("elevation setting sent before it was ever on: %q", pf["elevation.pref"])
	}
	// Turned off after being on: devices are pointed back at tak.gov.
	a.Store.SetElevation(store.ElevationSettings{Configured: true, Updated: time.Now().UTC()})
	if _, pf := profileFiles(t, api, "/Marti/api/tls/profile/enrollment?clientUid=ANDROID-cyan1"); !strings.Contains(pf["elevation.pref"], ">tak.gov</entry>") {
		t.Errorf("elevation.pref = %q", pf["elevation.pref"])
	}
	if _, pf := profileFiles(t, api, "/Marti/api/device/profile/connection?clientUid=ANDROID-cyan1&syncSecago=0"); pf["elevation.pref"] != "" {
		t.Error("unchanged elevation setting sent again")
	}

	// Send levels 1 and 2 to Cyan: a data package the device is offered.
	if st, _ := call("POST", "/admin/api/elevation/send", map[string]any{"levels": []int{1, 2}, "groups": []string{"Cyan"}, "bbox": []float64{10, 10, 11, 11}}); st != 400 {
		t.Errorf("send an empty area = %d", st)
	}
	st, v := call("POST", "/admin/api/elevation/send", map[string]any{"levels": []int{1, 2}, "groups": []string{"Cyan"}})
	if st != 200 || v["tiles"] != 2.0 || v["delivered"] != 1.0 {
		t.Fatalf("send = %d %v", st, v)
	}
	ev := device.expect(func(e *cot.Event) bool { return e.Type == "b-f-t-r" }, "elevation offered")
	if !strings.Contains(ev.Detail.XML, "Elevation DTED 1+2 (2 tiles).zip") {
		t.Errorf("offer = %s", ev.Detail.XML)
	}
	var pkg *store.Package
	for _, p := range a.Store.Packages() {
		if strings.HasPrefix(p.Name, "Elevation DTED") {
			pkg = p
		}
	}
	if pkg == nil || mustJSON(pkg.Groups) != `["Cyan"]` {
		t.Fatalf("package = %+v", pkg)
	}
	st, _, raw := api.do("GET", "/Marti/sync/content?hash="+pkg.Hash, nil)
	zr, err = zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if st != 200 || err != nil || len(zr.File) != 2 || zr.File[0].Name != "DTED/w078/n38.dt1" || zr.File[1].Name != "DTED/w078/n38.dt2" {
		t.Fatalf("package download = %d %v", st, err)
	}

	if st, v := call("DELETE", "/admin/api/elevation/levels/0", nil); st != 200 || !strings.Contains(mustJSON(v["levels"]), `{"bytes":0,"level":0,"tiles":0}`) {
		t.Errorf("delete level 0 = %d %v", st, v)
	}
	if st, _ := get("/elevation/DTED/w078/n38.dt0.zip"); st != 404 {
		t.Errorf("deleted tile = %d", st)
	}
}
