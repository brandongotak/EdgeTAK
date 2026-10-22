package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// TestMapSourcesToDevices covers sending console map sources to ATAK: the
// connected devices in the chosen groups are offered one at once, devices
// get it in their connection profile, and unchanged sources aren't resent.
func TestMapSourcesToDevices(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct{ name, group string }{{"alpha", "Cyan"}, {"red1", "Red"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}
	_, hc := adminJSON(t, a)
	put := func(list []map[string]any) int {
		b, _ := json.Marshal(list)
		req, _ := http.NewRequest("PUT", "https://"+a.Addrs["https"]+"/admin/api/map/sources", bytes.NewReader(b))
		req.Header.Set("X-EdgeTAK", "1")
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	alphaCert, redCert := enroll(t, a, "alpha", "password-alpha"), enroll(t, a, "red1", "password-red1")
	alpha, red := mustDial(t, a, &alphaCert), mustDial(t, a, &redCert)
	alphaAPI, redAPI := newAPI(t, a, alphaCert), newAPI(t, a, redCert)
	offer := func(e *cot.Event) bool { return e.Type == "b-f-t-r" && strings.Contains(e.Detail.XML, "Map source") }

	google := map[string]any{"name": "Google Hybrid", "url": "https://mt{s}.google.com/vt/lyrs=y&x={x}&y={y}&z={z}", "subdomains": "0123",
		"max_zoom": 20, "devices": true, "groups": []string{"Cyan"}}
	console := map[string]any{"name": "County", "url": "https://tiles.example.org/{z}/{x}/{-y}.jpg"}
	if st := put([]map[string]any{google, console}); st != 200 {
		t.Fatalf("save = %d", st)
	}
	ev := alpha.expect(offer, "the map source offered to ALPHA")
	red.expectNone(offer, "the map source offered to RED1")
	m := regexp.MustCompile(`senderUrl="https://[^/]+(/Marti/sync/content\?hash=[0-9a-f]+)"`).FindStringSubmatch(ev.Detail.XML)
	if m == nil {
		t.Fatalf("offer = %s", ev.Detail.XML)
	}
	if st, _, _ := alphaAPI.do("GET", m[1], nil); st != 200 {
		t.Errorf("ALPHA downloading = %d", st)
	}
	if st, _, _ := redAPI.do("GET", m[1], nil); st != 404 {
		t.Errorf("RED1 downloading = %d", st)
	}

	// The connection profile carries it for Cyan, as an ATAK map source.
	_, files := profileFiles(t, alphaAPI, "/Marti/api/device/profile/connection?clientUid=ANDROID-alpha&syncSecago=-1")
	xml := files["EdgeTAK-source-Google_Hybrid.xml"]
	for _, want := range []string{"<name>Google Hybrid</name>", "<maxZoom>20</maxZoom>", "<serverParts>0 1 2 3</serverParts>",
		"https://mt{$serverpart}.google.com/vt/lyrs=y&amp;x={$x}&amp;y={$y}&amp;z={$z}"} {
		if !strings.Contains(xml, want) {
			t.Errorf("map source lacks %s:\n%s", want, xml)
		}
	}
	if files["EdgeTAK-source-County.xml"] != "" {
		t.Error("a console-only source went to devices")
	}
	if _, files := profileFiles(t, redAPI, "/Marti/api/device/profile/connection?clientUid=ANDROID-red1&syncSecago=-1"); files["EdgeTAK-source-Google_Hybrid.xml"] != "" {
		t.Error("RED1's profile has Cyan's map source")
	}

	// Saving it unchanged sends nothing new; a reconnecting device that
	// synced since doesn't get it again.
	time.Sleep(1100 * time.Millisecond)
	put([]map[string]any{google, console})
	alpha.expectNone(offer, "the unchanged map source offered again")
	if _, files := profileFiles(t, alphaAPI, "/Marti/api/device/profile/connection?clientUid=ANDROID-alpha&syncSecago=1"); files["EdgeTAK-source-Google_Hybrid.xml"] != "" {
		t.Error("the unchanged source is in an incremental profile")
	}

	// Sending the console-only one to everyone reaches Red, with TMS rows.
	console["devices"] = true
	put([]map[string]any{google, console})
	red.expect(offer, "the County source offered to RED1")
	_, files = profileFiles(t, redAPI, "/Marti/api/device/profile/connection?clientUid=ANDROID-red1&syncSecago=5")
	if c := files["EdgeTAK-source-County.xml"]; !strings.Contains(c, "<invertYCoordinate>true</invertYCoordinate>") || !strings.Contains(c, "<tileType>jpg</tileType>") {
		t.Errorf("County = %s", c)
	}
}
