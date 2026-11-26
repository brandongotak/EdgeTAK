package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// TestCoTHistory covers recording every event, searching it from the
// console and exporting the results.
func TestCoTHistory(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("alpha", "password-alpha", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]
	get := func(path string) (int, []byte) {
		t.Helper()
		resp, err := hc.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	type result struct {
		Events []struct {
			T                   int64
			UID, Type, Callsign string
			FromCallsign        string `json:"from_callsign"`
			Username            string
			Source, Remarks     string
			Lat, Lon            *float64
			PositionReport      bool `json:"position_report"`
		}
		Total int
	}
	search := func(q string) result {
		t.Helper()
		st, b := get("/admin/api/cot-history?" + q)
		var r result
		if st != 200 || json.Unmarshal(b, &r) != nil {
			t.Fatalf("search %q = %d %s", q, st, b)
		}
		return r
	}

	cert := enroll(t, a, "alpha", "password-alpha")
	alpha := mustDial(t, a, &cert)
	alpha.send(sa("ANDROID-alpha", "ALPHA", "Cyan")) // before recording: not kept

	if st, _ := call("PUT", "/admin/api/cot-history/settings", map[string]any{"enabled": true, "keep_days": 0}); st != 400 {
		t.Errorf("keep 0 days = %d", st)
	}
	if st, m := call("PUT", "/admin/api/cot-history/settings", map[string]any{"enabled": true, "keep_days": 14}); st != 200 || m["enabled"] != true || m["keep_days"] != 14.0 {
		t.Fatalf("turn on = %d %v", st, m)
	}

	alpha.send(sa("ANDROID-alpha", "ALPHA", "Cyan"))
	hostile := cot.NewEvent("a-h-G-U-C-V", "marker-1", "h-g-i-g-o", time.Hour)
	hostile.Lat, hostile.Lon = 38.95, -77.1
	hostile.Detail.XML = `<contact callsign="Truck"/><remarks>Two trucks heading north</remarks>`
	alpha.send(hostile)
	hostile2 := cot.NewEvent("a-h-G-U-C-V", "marker-1", "h-g-i-g-o", time.Hour) // moved
	hostile2.Lat, hostile2.Lon = 38.96, -77.1
	hostile2.Detail.XML = `<contact callsign="Truck"/><remarks>Now at the bridge</remarks>`
	alpha.send(hostile2)
	alpha.send(chat("ALPHA", "All Chat Rooms"))
	waitFor(t, "events recorded", func() bool { return search("").Total == 3 })

	r := search("")
	if r.Events[0].Type != "b-t-f" || r.Events[1].Remarks != "Now at the bridge" || r.Events[1].FromCallsign != "ALPHA" ||
		r.Events[1].Username != "alpha" || r.Events[1].Source != "device" || r.Events[1].Callsign != "Truck" || *r.Events[1].Lat != 38.96 {
		t.Fatalf("events = %+v", r.Events)
	}
	if r := search("positions=1"); r.Total != 4 || !r.Events[3].PositionReport {
		t.Errorf("with position reports = %+v", r)
	}
	for q, want := range map[string]int{
		"types=a-h":                2,
		"types=a-h,b-t-f":          3,
		"types=b-m-p":              0,
		"q=bridge":                 1,
		"q=truck":                  2,
		"bbox=-77.2,38.955,-77,39": 1,
		"bbox=0,0,1,1":             0,
		"limit=1":                  1,
		"since=" + url.QueryEscape(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)): 0,
	} {
		if n := len(search(q).Events); n != want {
			t.Errorf("%s: %d events, want %d", q, n, want)
		}
	}
	if st, _ := get("/admin/api/cot-history?bbox=1,2,3"); st != 400 {
		t.Errorf("bad bbox = %d", st)
	}
	if st, _ := get("/admin/api/cot-history?since=yesterday"); st != 400 {
		t.Errorf("bad since = %d", st)
	}

	// One event with its XML.
	ev := r.Events[1]
	st, b := get("/admin/api/cot-history/event?t=" + strconv.FormatInt(ev.T, 10) + "&uid=marker-1")
	var one struct{ Remarks, XML string }
	json.Unmarshal(b, &one)
	if st != 200 || one.Remarks != "Now at the bridge" || !strings.HasPrefix(one.XML, "<event ") || !strings.Contains(one.XML, "Now at the bridge") {
		t.Errorf("event = %d %s", st, b)
	}

	// Exports.
	if st, b := get("/admin/api/cot-history/export?format=csv&types=a-h"); st != 200 || strings.Count(string(b), "\n") != 3 || !strings.Contains(string(b), "Two trucks heading north") {
		t.Errorf("CSV = %d %s", st, b)
	}
	if st, b := get("/admin/api/cot-history/export?format=kml"); st != 200 || strings.Count(string(b), "<Placemark>") != 2 {
		t.Errorf("KML = %d %s", st, b)
	}
	st, b = get("/admin/api/cot-history/export?format=zip")
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if st != 200 || err != nil {
		t.Fatalf("package = %d %v", st, err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		d, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(d)
	}
	// The last version of the marker only; no chat.
	if len(files) != 2 || !strings.Contains(files["1/marker-1.cot"], "Now at the bridge") || !strings.Contains(files["MANIFEST/manifest.xml"], `zipEntry="1/marker-1.cot"`) {
		t.Errorf("package files = %v", files)
	}

	// Users can't search.
	if st, _, _ := newAPI(t, a, cert).do("GET", "/admin/api/cot-history", nil); st == http.StatusOK {
		t.Error("a user searched CoT history")
	}
	// Turning it off stops recording; clearing deletes it.
	call("PUT", "/admin/api/cot-history/settings", map[string]any{"enabled": false, "keep_days": 14})
	alpha.send(chat("ALPHA", "All Chat Rooms"))
	time.Sleep(200 * time.Millisecond)
	if n := search("").Total; n != 3 {
		t.Errorf("recorded while off: %d", n)
	}
	if st, _ := call("DELETE", "/admin/api/cot-history", nil); st != 204 {
		t.Errorf("clear = %d", st)
	}
	if n := search("positions=1").Total; n != 0 {
		t.Errorf("after clear: %d", n)
	}
}
