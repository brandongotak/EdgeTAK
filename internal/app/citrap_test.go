package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestCITrapReports covers the ATAK Reports plugin's server API (TAK Server's
// CITRAP API): upload, search, download, update, attachments and delete,
// limited to the uploader's groups.
func TestCITrapReports(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct{ name, group string }{{"cyan1", "Cyan"}, {"cyan2", "Cyan"}, {"red1", "Red"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}
	cyan1 := newAPI(t, a, enroll(t, a, "cyan1", "password-cyan1"))
	cyan2 := newAPI(t, a, enroll(t, a, "cyan2", "password-cyan2"))
	red1 := newAPI(t, a, enroll(t, a, "red1", "password-red1"))
	send := func(c *apiClient, method, path, contentType, accept string, body []byte, hdr ...string) (int, http.Header, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, c.base+path, bytes.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, raw
	}
	search := func(c *apiClient, query string) []map[string]any {
		t.Helper()
		st, _, raw := send(c, "GET", "/Marti/api/citrap?"+query, "", "*/*", nil)
		var out []map[string]any
		if st != 200 || json.Unmarshal(raw, &out) != nil {
			t.Fatalf("search %q = %d %s", query, st, raw)
		}
		return out
	}

	// Every user can subscribe to the citrap mission, as the plugin does.
	for _, c := range []*apiClient{cyan2, red1} {
		if st, _, raw := c.do("PUT", "/Marti/api/missions/citrap/subscription?uid=ANDROID-x", nil); st != 201 && st != 200 {
			t.Fatalf("subscribe to citrap = %d %s", st, raw)
		}
	}

	// The plugin uploads a zip: the report XML and an attachment.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("SUAS-DHS.xml")
	io.WriteString(w, "ï»¿"+`<?xml version="1.0" encoding="UTF-8"?><report type="SUAS-DHS" title="Drone over the fence" userCallsign="CYAN1" userDescription="seen twice" dateTime="2026-11-25T14:30:00.000" location="POINT (-77.03 38.89)" locationDescription="north gate" eventScale="Small" importance="High" status="Open"><section title="Activity"><option title="Time" type="dateTime"/></section></report>`)
	w, _ = zw.Create("photo.jpg")
	io.WriteString(w, "jpeg")
	zw.Close()
	st, hdr, raw := send(cyan1, "POST", "/Marti/api/citrap?clientUid=ANDROID-cyan1", "application/x-zip-compressed", "", zb.Bytes())
	var created struct{ ID string }
	if st != 201 || json.Unmarshal(raw, &created) != nil || created.ID == "" || !strings.HasSuffix(hdr.Get("Location"), "/Marti/api/citrap/"+created.ID) {
		t.Fatalf("upload = %d %s %v", st, raw, hdr)
	}
	id := created.ID

	// Search: the same groups see it; filters narrow it down.
	got := search(cyan2, "clientUid=ANDROID-cyan2")
	if len(got) != 1 || got[0]["id"] != id || got[0]["title"] != "Drone over the fence" || got[0]["dateTime"] != "2026-11-25T14:30:00.000Z" ||
		got[0]["location"] != "POINT (-77.03 38.89)" || got[0]["status"] != "Open" || got[0]["importance"] != "High" {
		t.Fatalf("search = %v", got)
	}
	for q, want := range map[string]int{
		"keywords=fence":                     1,
		"keywords=nothing,NORTH":             1,
		"keywords=nothing":                   0,
		"type=SUAS-DHS":                      1,
		"type=Other":                         0,
		"callsign=CYAN1":                     1,
		"bbox=-78,38,-76,39":                 1,
		"bbox=10,10,11,11":                   0,
		"startTime=2026-11-25T00:00:00.000Z": 1,
		"endTime=2026-11-24T00:00:00.000Z":   0,
		"maxReportCount=0":                   0,
	} {
		if n := len(search(cyan2, q)); n != want {
			t.Errorf("search %s = %d reports, want %d", q, n, want)
		}
	}
	if n := len(search(red1, "")); n != 0 {
		t.Errorf("another group's search found %d reports", n)
	}
	if st, _, _ := send(red1, "GET", "/Marti/api/citrap/"+id+"?clientUid=ANDROID-red1", "", "", nil); st != 404 {
		t.Errorf("another group's download = %d, want 404", st)
	}
	if st, _, raw := send(cyan2, "GET", "/Marti/api/citrap?keywords=fence", "", "application/xml", nil); st != 200 || !strings.Contains(string(raw), `<reports><report id="`+id+`"`) {
		t.Errorf("XML search = %d %s", st, raw)
	}

	// Download: the zip, with the report XML now carrying its id.
	zipFiles := func(b []byte) map[string]string {
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatalf("not a zip: %v", err)
		}
		out := map[string]string{}
		for _, f := range zr.File {
			rc, _ := f.Open()
			d, _ := io.ReadAll(rc)
			rc.Close()
			out[f.Name] = string(d)
		}
		return out
	}
	st, _, raw = send(cyan2, "GET", "/Marti/api/citrap/"+id+"?clientUid=ANDROID-cyan2", "", "*/*", nil)
	files := zipFiles(raw)
	if st != 200 || files["photo.jpg"] != "jpeg" || !strings.Contains(files["SUAS-DHS.xml"], `<report id="`+id+`" type="SUAS-DHS"`) {
		t.Fatalf("download = %d %v", st, files)
	}
	if st, _, raw := send(cyan2, "GET", "/Marti/api/citrap/"+id+"?clientUid=ANDROID-cyan2", "", "application/xml", nil); st != 200 || !strings.Contains(string(raw), "Drone over the fence") {
		t.Errorf("XML download = %d %s", st, raw)
	}

	// The report is in the citrap mission, and has a mission of its own.
	_, m, _ := red1.do("GET", "/Marti/api/missions/citrap", nil)
	if !strings.Contains(mustJSON(m), `"tool":"citrap"`) || !strings.Contains(mustJSON(m), `"contents":[{`) {
		t.Errorf("citrap mission = %s", mustJSON(m))
	}
	if st, _, _ := cyan2.do("GET", "/Marti/api/missions/"+id, nil); st != 200 {
		t.Errorf("report mission = %d", st)
	}
	if st, _, _ := red1.do("GET", "/Marti/api/missions/"+id, nil); st != 404 {
		t.Errorf("another group's report mission = %d, want 404", st)
	}

	// Update with new XML, then add an attachment.
	newXML := strings.Replace(files["SUAS-DHS.xml"], "Drone over the fence", "Two drones", 1)
	if st, _, raw := send(cyan2, "PUT", "/Marti/api/citrap/"+id+"?clientUid=ANDROID-cyan2", "application/xml", "", []byte(newXML)); st != 200 {
		t.Fatalf("update = %d %s", st, raw)
	}
	if got := search(cyan1, "keywords=two"); len(got) != 1 || got[0]["title"] != "Two drones" {
		t.Errorf("after update = %v", got)
	}
	if st, _, raw := send(cyan1, "POST", "/Marti/api/citrap/"+id+"/attachment?clientUid=ANDROID-cyan1", "application/octet-stream", "", []byte("notes"), "Content-Disposition", "attachment; filename=notes.txt"); st != 200 {
		t.Fatalf("attachment = %d %s", st, raw)
	}
	_, _, raw = send(cyan2, "GET", "/Marti/api/citrap/"+id+"?clientUid=ANDROID-cyan2", "", "", nil)
	if files := zipFiles(raw); files["notes.txt"] != "notes" || files["photo.jpg"] != "jpeg" || !strings.Contains(files["SUAS-DHS.xml"], "Two drones") {
		t.Errorf("after attachment = %v", files)
	}

	// Delete: gone from search, download and missions.
	if st, _, _ := send(red1, "DELETE", "/Marti/api/citrap/"+id+"?clientUid=ANDROID-red1", "", "", nil); st != 404 {
		t.Errorf("another group's delete = %d, want 404", st)
	}
	if st, _, raw := send(cyan2, "DELETE", "/Marti/api/citrap/"+id+"?clientUid=ANDROID-cyan2", "", "", nil); st != 200 {
		t.Fatalf("delete = %d %s", st, raw)
	}
	if n := len(search(cyan1, "")); n != 0 {
		t.Errorf("after delete, search found %d", n)
	}
	if st, _, _ := cyan1.do("GET", "/Marti/api/missions/"+id, nil); st != 404 {
		t.Errorf("report mission after delete = %d", st)
	}
	if len(a.Store.Packages()) != 0 {
		t.Errorf("report files left after delete: %d", len(a.Store.Packages()))
	}
}
