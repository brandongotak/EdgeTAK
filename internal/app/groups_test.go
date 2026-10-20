package app

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// adminSession logs in to the admin console and returns a request helper.
func adminSession(t *testing.T, a *App) func(method, path string, body any) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(a.DataDir, "initial-admin-password.txt"))
	if err != nil {
		t.Fatal(err)
	}
	pw := strings.TrimPrefix(strings.Split(string(raw), "\n")[1], "password: ")
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	base := "https://" + a.Addrs["https"]
	resp, err := hc.Post(base+"/admin/api/login", "application/json", strings.NewReader(`{"username":"admin","password":"`+pw+`"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("admin login: %v %v", resp, err)
	}
	resp.Body.Close()
	return func(method, path string, body any) int {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+path, rd)
		req.Header.Set("X-EdgeTAK", "1")
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
}

// TestChannels covers ATAK "Channels": a user switches memberships on and off
// from the client, effective immediately and remembered across reconnects.
func TestChannels(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct {
		name   string
		groups []string
	}{{"alpha", []string{"Ops", "Intel"}}, {"peer1", []string{"Ops"}}, {"peer2", []string{"Intel"}}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, u.groups, false); err != nil {
			t.Fatal(err)
		}
	}
	certs := map[string]tls.Certificate{}
	streams := map[string]*streamClient{}
	for _, u := range []string{"alpha", "peer1", "peer2"} {
		certs[u] = enroll(t, a, u, "password-"+u)
		c := certs[u]
		streams[u] = mustDial(t, a, &c)
		streams[u].send(sa("ANDROID-"+u, strings.ToUpper(u), "Cyan"))
	}
	time.Sleep(200 * time.Millisecond)
	alpha := newAPI(t, a, certs["alpha"])

	if env := alpha.want(200, "GET", "/Marti/api/groups/groupCacheEnabled", nil); env["data"] != true {
		t.Fatalf("groupCacheEnabled = %v (ATAK hides Channels unless true)", env["data"])
	}
	// ATAK only shows Channels when these device preferences arrive in the
	// connection (and enrollment) profile.
	for _, path := range []string{"/Marti/api/device/profile/connection?clientUid=ANDROID-alpha&syncSecago=30", "/Marti/api/tls/profile/enrollment?clientUid=ANDROID-alpha"} {
		st, _, raw := alpha.do("GET", path, nil)
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if st != 200 || err != nil {
			t.Fatalf("%s = %d %v", path, st, err)
		}
		files := map[string]string{}
		for _, f := range zr.File {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			files[f.Name] = string(b)
		}
		pref, manifest := files["file0/enable-channels.pref"], files["MANIFEST/manifest.xml"]
		if !strings.Contains(pref, `key="prefs_enable_channels"`) || !strings.Contains(pref, `key="prefs_enable_channels_host-127.0.0.1"`) ||
			!strings.Contains(pref, `<entry key="deviceProfileEnableOnConnect" class="class java.lang.Boolean">true</entry>`) {
			t.Errorf("%s pref = %q", path, pref)
		}
		if !strings.Contains(manifest, `name="onReceiveImport" value="true"`) || !strings.Contains(manifest, `zipEntry="file0/enable-channels.pref"`) {
			t.Errorf("%s manifest = %q", path, manifest)
		}
	}
	channels := func() map[string]bool {
		out := map[string]bool{}
		for _, g := range alpha.want(200, "GET", "/Marti/api/groups/all?useCache=true", nil)["data"].([]any) {
			m := g.(map[string]any)
			out[m["name"].(string)+":"+m["direction"].(string)] = m["active"].(bool)
		}
		return out
	}
	if ch := channels(); len(ch) != 4 || !ch["Intel:IN"] || !ch["Ops:OUT"] {
		t.Fatalf("channels = %v", ch)
	}

	// Switch Intel off: peer2 no longer hears alpha and vice versa; Ops still works.
	alpha.want(200, "PUT", "/Marti/api/groups/active?clientUid=ANDROID-alpha", []map[string]any{
		{"name": "Ops", "direction": "IN", "active": true}, {"name": "Ops", "direction": "OUT", "active": true},
		{"name": "Intel", "direction": "IN", "active": false}, {"name": "Intel", "direction": "OUT", "active": false},
	})
	if ch := channels(); ch["Intel:IN"] || ch["Intel:OUT"] || !ch["Ops:IN"] || len(ch) != 4 {
		t.Errorf("channels after switching Intel off = %v", ch)
	}
	for _, s := range streams { // discard what was delivered before the switch
		s.expectNone(func(*cot.Event) bool { return false }, "")
	}
	streams["alpha"].send(sa("ANDROID-alpha", "ALPHA", "Cyan"))
	streams["peer1"].expect(isFrom("ANDROID-alpha"), "alpha SA on Ops")
	streams["peer2"].expectNone(isFrom("ANDROID-alpha"), "alpha SA on switched-off Intel")
	streams["peer2"].send(sa("ANDROID-peer2", "PEER2", "Cyan"))
	streams["alpha"].expectNone(isFrom("ANDROID-peer2"), "peer2 SA on switched-off Intel")

	// Remembered: a new connection starts with Intel off.
	if u, _ := a.Store.User("alpha"); u.IsActive("Intel", "IN") {
		t.Error("inactive channel not persisted")
	}

	// activebits switches Intel back on (both bits listed) and replays peer2's SA.
	bits := map[string]float64{}
	for _, g := range alpha.want(200, "GET", "/Marti/api/groups/all", nil)["data"].([]any) {
		m := g.(map[string]any)
		bits[m["name"].(string)] = m["bitpos"].(float64)
	}
	alpha.want(200, "PUT", "/Marti/api/groups/activebits", []float64{bits["Ops"], bits["Intel"]})
	streams["alpha"].expect(isFrom("ANDROID-peer2"), "peer2 SA replayed after Intel switched back on")
	if ch := channels(); !ch["Intel:IN"] || !ch["Intel:OUT"] {
		t.Errorf("channels after activebits = %v", ch)
	}
}

// TestGroupDirectionsAndLiveChanges covers IN/OUT membership semantics and
// that admin changes apply to connected clients without reconnecting.
func TestGroupDirectionsAndLiveChanges(t *testing.T) {
	a := startApp(t)
	admin := adminSession(t, a)
	if st := admin("POST", "/admin/api/groups", map[string]string{"name": "Ops", "description": "Operations"}); st != 201 {
		t.Fatalf("create group = %d", st)
	}
	if st := admin("POST", "/admin/api/groups", map[string]string{"name": "Ops"}); st != 409 {
		t.Errorf("duplicate group = %d", st)
	}
	for _, u := range []string{"alpha", "watch", "bravo"} {
		if _, err := a.Store.CreateUser(u, "password-"+u, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	admin("PUT", "/admin/api/groups/Ops/members/alpha", map[string]string{"direction": "both"})
	admin("PUT", "/admin/api/groups/Ops/members/watch", map[string]string{"direction": "in"}) // receive only
	if st := admin("PUT", "/admin/api/groups/Ops/members/nobody", map[string]string{"direction": "in"}); st != 404 {
		t.Errorf("unknown member = %d", st)
	}
	if st := admin("DELETE", "/admin/api/groups/__ANON__", nil); st != 400 {
		t.Errorf("deleting the anonymous group = %d", st)
	}

	streams := map[string]*streamClient{}
	for _, u := range []string{"alpha", "watch", "bravo"} {
		cert := enroll(t, a, u, "password-"+u)
		streams[u] = mustDial(t, a, &cert)
		streams[u].send(sa("ANDROID-"+u, strings.ToUpper(u), "Cyan"))
	}
	time.Sleep(200 * time.Millisecond)

	// alpha (Ops both) -> watch (Ops in): delivered. bravo (__ANON__) sees nothing from Ops.
	streams["alpha"].send(sa("ANDROID-alpha", "ALPHA", "Cyan"))
	streams["watch"].expect(isFrom("ANDROID-alpha"), "alpha SA at receive-only watch")
	streams["bravo"].expectNone(isFrom("ANDROID-alpha"), "alpha SA at bravo (other group)")
	// watch has no OUT group, so its own traffic reaches nobody.
	streams["watch"].send(sa("ANDROID-watch", "WATCH", "Cyan"))
	streams["alpha"].expectNone(isFrom("ANDROID-watch"), "receive-only watch SA at alpha")

	// Live change: add bravo to Ops; it gets alpha's latest SA without reconnecting.
	admin("PUT", "/admin/api/groups/Ops/members/bravo", map[string]string{"direction": "both"})
	streams["bravo"].expect(isFrom("ANDROID-alpha"), "alpha SA replayed to bravo after joining Ops")
	streams["alpha"].send(sa("ANDROID-alpha", "ALPHA", "Cyan"))
	streams["bravo"].expect(isFrom("ANDROID-alpha"), "alpha SA at bravo after joining Ops")

	// The client-facing group list reports directions.
	watch := newAPI(t, a, enroll(t, a, "watch", "password-watch"))
	env := watch.want(200, "GET", "/Marti/api/groups/all", nil)
	dirs := map[string]bool{}
	for _, g := range env["data"].([]any) {
		m := g.(map[string]any)
		dirs[m["name"].(string)+"/"+m["direction"].(string)] = true
		if m["name"] == "Ops" && m["description"] != "Operations" {
			t.Errorf("group description = %v", m["description"])
		}
	}
	if !dirs["Ops/IN"] || dirs["Ops/OUT"] {
		t.Errorf("watch groups/all = %v", dirs)
	}

	// The Users tab's groups dialog saves all three directions in one PATCH.
	if st := admin("PATCH", "/admin/api/users/bravo", map[string]any{"groups": []string{"Ops"}, "groups_in": []string{"Intel"}, "groups_out": []string{}}); st != 200 {
		t.Fatalf("patch user groups = %d", st)
	}
	for _, c := range a.Router.Clients() {
		if c.Username == "bravo" && (strings.Join(c.InGroups(), ",") != "Ops,Intel" || strings.Join(c.OutGroups(), ",") != "Ops") {
			t.Errorf("bravo live groups = in %v out %v", c.InGroups(), c.OutGroups())
		}
	}
	if _, ok := a.Store.Group("Intel"); !ok {
		t.Error("group record not created for a new membership")
	}

	// Rename keeps memberships and live routing.
	if st := admin("PATCH", "/admin/api/groups/Ops", map[string]string{"name": "Operations"}); st != 204 {
		t.Fatalf("rename = %d", st)
	}
	streams["alpha"].send(sa("ANDROID-alpha", "ALPHA", "Cyan"))
	streams["watch"].expect(isFrom("ANDROID-alpha"), "alpha SA at watch after rename")

	// Delete removes memberships from live clients at once; users left
	// without groups fall back to the anonymous group.
	if st := admin("DELETE", "/admin/api/groups/Operations", nil); st != 204 {
		t.Fatalf("delete = %d", st)
	}
	for _, c := range a.Router.Clients() {
		if c.Username == "alpha" && (len(c.InGroups()) != 1 || c.InGroups()[0] != a.Cfg.AnonymousGroup) {
			t.Errorf("alpha groups after delete = %v", c.InGroups())
		}
	}
}
