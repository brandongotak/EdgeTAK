package app

import (
	"archive/zip"
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// profileFiles fetches a profile package with a device certificate and
// returns its files by name (fileN/ folders dropped).
func profileFiles(t *testing.T, c *apiClient, path string) (int, map[string]string) {
	t.Helper()
	st, _, raw := c.do("GET", path, nil)
	if st != 200 {
		return st, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		name := f.Name
		if i := strings.Index(name, "/"); i >= 0 && strings.HasPrefix(name, "file") {
			name = name[i+1:]
		}
		files[name] = string(b)
	}
	return st, files
}

func TestDeviceProfiles(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct{ name, group string }{{"cyan1", "Cyan"}, {"red1", "Red"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}
	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]

	// Bad settings are refused.
	for _, bad := range []map[string]any{
		{"name": "x", "prefs": []map[string]string{{"key": "bad key", "value": "1", "type": "string"}}},
		{"name": "x", "prefs": []map[string]string{{"key": "displayRed", "value": "maybe", "type": "boolean"}}},
		{"name": "x", "prefs": []map[string]string{{"key": "n", "value": "1", "type": "object"}}},
		{"name": " "},
	} {
		if code, _ := call("POST", "/admin/api/profiles", bad); code != 400 {
			t.Errorf("%v accepted (%d)", bad, code)
		}
	}
	code, p := call("POST", "/admin/api/profiles", map[string]any{
		"name": "Cyan team", "groups": []string{"Cyan"}, "active": true, "on_enrollment": true, "on_connect": true,
		"prefs": []map[string]string{{"key": "locationTeam", "value": "Cyan", "type": "string"}, {"key": "atakRoleType", "value": "Team Lead"},
			{"key": "displayServerConnectionWidget", "value": "TRUE", "type": "boolean"}},
	})
	if code != 200 {
		t.Fatalf("create = %d %v", code, p)
	}
	id := p["id"].(string)

	// A file, as the console uploads it.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "maps.xml")
	io.WriteString(fw, "<customMapSource><name>Test</name></customMapSource>")
	mw.Close()
	req, _ := http.NewRequest("POST", base+"/admin/api/profiles/"+id+"/files", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-EdgeTAK", "1")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(a.DataDir, "profiles", id, "maps.xml")); err != nil {
		t.Fatalf("file not stored: %v", err)
	}

	cyan := enroll(t, a, "cyan1", "password-cyan1")
	red := enroll(t, a, "red1", "password-red1")
	cyanAPI, redAPI := newAPI(t, a, cyan), newAPI(t, a, red)

	// Members get it on enrollment: channel settings, the profile's settings, its file.
	_, files := profileFiles(t, cyanAPI, "/Marti/api/tls/profile/enrollment?clientUid=ANDROID-cyan1")
	pref := files["Cyan_team.pref"]
	if files["enable-channels.pref"] == "" || files["maps.xml"] == "" ||
		!strings.Contains(pref, `<entry key="locationTeam" class="class java.lang.String">Cyan</entry>`) ||
		!strings.Contains(pref, `<entry key="displayServerConnectionWidget" class="class java.lang.Boolean">true</entry>`) ||
		!strings.Contains(pref, `name="com.atakmap.app.civ_preferences"`) {
		t.Fatalf("enrollment profile = %v", files)
	}
	if !strings.Contains(files["MANIFEST/manifest.xml"], `zipEntry="file2/maps.xml"`) {
		t.Errorf("manifest = %s", files["MANIFEST/manifest.xml"])
	}
	// Others don't.
	if _, files := profileFiles(t, redAPI, "/Marti/api/tls/profile/enrollment?clientUid=ANDROID-red1"); files["maps.xml"] != "" || files["Cyan_team.pref"] != "" {
		t.Errorf("red got the Cyan profile: %v", files)
	}

	// On connect: only when changed since the device's last sync.
	if _, files := profileFiles(t, cyanAPI, "/Marti/api/device/profile/connection?clientUid=ANDROID-cyan1&syncSecago=-1"); files["maps.xml"] == "" {
		t.Error("first sync (syncSecago=-1) without the profile")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, files := profileFiles(t, cyanAPI, "/Marti/api/device/profile/connection?clientUid=ANDROID-cyan1&syncSecago=0"); files["maps.xml"] != "" {
		t.Error("an unchanged profile was sent again")
	}
	// An edit counts as a change.
	call("PUT", "/admin/api/profiles/"+id, map[string]any{"name": "Cyan team", "groups": []string{"Cyan"}, "active": true, "on_enrollment": true, "on_connect": true})
	if _, files := profileFiles(t, cyanAPI, "/Marti/api/device/profile/connection?clientUid=ANDROID-cyan1&syncSecago=1"); files["maps.xml"] == "" {
		t.Error("an updated profile was not sent")
	}

	// Send now reaches a connected member as a file offer.
	stream := mustDial(t, a, &cyan)
	stream.send(sa("ANDROID-cyan1", "CYAN1", "Cyan"))
	time.Sleep(200 * time.Millisecond)
	if code, r := call("POST", "/admin/api/profiles/"+id+"/send", nil); code != 200 || r["delivered"] != 1.0 {
		t.Fatalf("send now = %d %v", code, r)
	}
	stream.expect(func(e *cot.Event) bool {
		return e.Type == "b-f-t-r" && strings.Contains(e.Detail.XML, "Profile-Cyan_team.zip")
	}, "profile offer")

	// Renaming the group keeps the profile pointed at it.
	if code, _ := call("PATCH", "/admin/api/groups/Cyan", map[string]any{"name": "Cyan-1"}); code != 200 && code != 204 {
		t.Fatalf("rename group = %d", code)
	}
	if prof, _ := a.Store.DeviceProfile(id); len(prof.Groups) != 1 || prof.Groups[0] != "Cyan-1" {
		t.Errorf("profile groups after rename = %v", prof.Groups)
	}

	// Turned off, nobody gets it; deleted, its files go too.
	call("PUT", "/admin/api/profiles/"+id, map[string]any{"name": "Cyan team", "groups": []string{"Cyan-1"}, "active": false, "on_enrollment": true, "on_connect": true})
	if _, files := profileFiles(t, cyanAPI, "/Marti/api/tls/profile/enrollment?clientUid=ANDROID-cyan1"); files["maps.xml"] != "" {
		t.Error("an inactive profile was sent")
	}
	if code, _ := call("DELETE", "/admin/api/profiles/"+id, nil); code != 204 {
		t.Fatalf("delete = %d", code)
	}
	if _, err := os.Stat(filepath.Join(a.DataDir, "profiles", id)); !os.IsNotExist(err) {
		t.Error("profile files kept after delete")
	}
}
