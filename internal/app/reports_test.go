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

// TestReportsConsole covers the console's Reports page: every report with
// its answers and attachments, attachment downloads and deleting.
func TestReportsConsole(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("cyan1", "password-cyan1", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	cyan1 := newAPI(t, a, enroll(t, a, "cyan1", "password-cyan1"))
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("SUAS-DHS.xml")
	io.WriteString(w, `<report type="SUAS-DHS" title="Drone" userCallsign="CYAN1" dateTime="2026-11-25T14:30:00.000" location="POINT (-77.03 38.89)" status="Open">`+
		`<section title="Aircraft"><option title="Altitude" value="120" unitValue="m"/><option title="Empty" value=""/>`+
		`<list title="Colour"><option title="Red"/><option title="White" selected="true"/><option title="Black" selected="true"/></list>`+
		`<section title="Pilot"><option title="Seen" type="checkbox" selected="true"/></section></section></report>`)
	w, _ = zw.Create("photo.jpg")
	io.WriteString(w, "jpeg")
	w, _ = zw.Create("notes.html")
	io.WriteString(w, "<script>alert(1)</script>")
	zw.Close()
	req, _ := http.NewRequest("POST", cyan1.base+"/Marti/api/citrap?clientUid=ANDROID-cyan1", bytes.NewReader(zb.Bytes()))
	resp, err := cyan1.hc.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("upload: %v %v", err, resp)
	}
	var created struct{ ID string }
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]
	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := hc.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	resp, raw := get("/admin/api/reports")
	var list []struct {
		ID          string
		Title       string
		Lat, Lon    *float64
		Fields      []struct{ Section, Title, Value string }
		Attachments []struct{ Name string }
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &list) != nil || len(list) != 1 {
		t.Fatalf("list = %d %s", resp.StatusCode, raw)
	}
	r := list[0]
	if r.ID != created.ID || r.Lat == nil || *r.Lat != 38.89 || *r.Lon != -77.03 || len(r.Attachments) != 2 {
		t.Fatalf("report = %s", raw)
	}
	fields := map[string]string{}
	for _, f := range r.Fields {
		fields[f.Section+"/"+f.Title] = f.Value
	}
	if len(fields) != 3 || fields["Aircraft/Altitude"] != "120 m" || fields["Aircraft/Colour"] != "White, Black" || fields["Aircraft › Pilot/Seen"] != "✓" {
		t.Errorf("fields = %v", fields)
	}

	// Images show in the browser; anything else only downloads.
	if resp, b := get("/admin/api/reports/" + created.ID + "/files/photo.jpg"); resp.StatusCode != 200 || string(b) != "jpeg" ||
		resp.Header.Get("Content-Type") != "image/jpeg" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") {
		t.Errorf("photo = %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := get("/admin/api/reports/" + created.ID + "/files/notes.html"); resp.Header.Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("html attachment served as %v", resp.Header)
	}
	if resp, _ := get("/admin/api/reports/" + created.ID + "/files/missing.jpg"); resp.StatusCode != 404 {
		t.Errorf("missing attachment = %d", resp.StatusCode)
	}
	// The console's Download button.
	if resp, b := get("/Marti/api/citrap/" + created.ID); resp.StatusCode != 200 || !bytes.HasPrefix(b, []byte("PK")) {
		t.Errorf("download = %d", resp.StatusCode)
	}
	// Users can't use the admin API.
	if st, _, _ := cyan1.do("GET", "/admin/api/reports", nil); st == 200 {
		t.Error("a user listed reports through the admin API")
	}

	if st, _ := call("DELETE", "/admin/api/reports/"+created.ID, nil); st != 204 {
		t.Fatalf("delete = %d", st)
	}
	if st, _ := call("DELETE", "/admin/api/reports/"+created.ID, nil); st != 404 {
		t.Errorf("second delete = %d", st)
	}
	if _, raw := get("/admin/api/reports"); string(bytes.TrimSpace(raw)) != "[]" {
		t.Errorf("after delete = %s", raw)
	}
	if len(a.Store.Packages()) != 0 {
		t.Errorf("report files left: %d", len(a.Store.Packages()))
	}
	if st, _, _ := cyan1.do("GET", "/Marti/api/missions/"+created.ID, nil); st != 404 {
		t.Errorf("report mission after delete = %d", st)
	}
}
