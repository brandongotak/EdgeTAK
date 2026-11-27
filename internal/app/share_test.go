package app

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestShareMap covers shared map links: a read-only live map of chosen
// groups' devices that anyone with the link can open without signing in,
// until it is deleted.
func TestShareMap(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct{ name, group string }{{"cyan1", "Cyan"}, {"red1", "Red"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}
	c1, r1 := enroll(t, a, "cyan1", "password-cyan1"), enroll(t, a, "red1", "password-red1")
	mustDial(t, a, &c1).send(sa("ANDROID-cyan1", "CYAN-1", "Cyan"))
	mustDial(t, a, &r1).send(sa("ANDROID-red1", "RED-1", "Red"))
	waitFor(t, "devices identified", func() bool {
		n := 0
		for _, c := range a.Router.Clients() {
			if c.LastSA() != nil {
				n++
			}
		}
		return n == 2
	})

	call, _ := adminJSON(t, a)
	if st, _ := call("POST", "/admin/api/share-links", map[string]any{"name": "", "hours": 24}); st != 400 {
		t.Errorf("no name = %d", st)
	}
	if st, _ := call("POST", "/admin/api/share-links", map[string]any{"name": "x", "hours": 999}); st != 400 {
		t.Errorf("too long = %d", st)
	}
	st, v := call("POST", "/admin/api/share-links", map[string]any{"name": "Cyan team", "hours": 8, "groups": []string{"Cyan"}, "callsigns": true})
	if st != 201 {
		t.Fatalf("make link = %d %v", st, v)
	}
	link := v["link"].(map[string]any)
	u, _ := url.Parse(link["url"].(string))
	if !strings.HasPrefix(u.Path, "/share/") || len(u.Path) < len("/share/")+20 || u.Port() != portOf(a.Addrs["enroll"]) {
		t.Fatalf("link = %v", link["url"])
	}
	_, v2 := call("POST", "/admin/api/share-links", map[string]any{"name": "Everyone, no names", "hours": 1, "callsigns": false})
	link2 := v2["link"].(map[string]any)

	// The public side: no sign-in, no client certificate.
	anon := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	base := "https://" + a.Addrs["https"]
	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := anon.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	type data struct {
		Name    string
		Devices []map[string]any
	}
	read := func(path string) (int, data) {
		t.Helper()
		resp, b := get(path + "/data")
		var d data
		json.Unmarshal(b, &d)
		return resp.StatusCode, d
	}
	resp, page := get(u.Path)
	if resp.StatusCode != 200 || !strings.Contains(string(page), "/admin/share.js") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") ||
		resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("page = %d %v", resp.StatusCode, resp.Header)
	}
	st, d := read(u.Path)
	if st != 200 || d.Name != "Cyan team" || len(d.Devices) != 1 || d.Devices[0]["callsign"] != "CYAN-1" || d.Devices[0]["team"] != "Cyan" {
		t.Fatalf("data = %d %+v", st, d)
	}
	if _, b := get(u.Path + "/data"); strings.Contains(string(b), "cyan1") || strings.Contains(string(b), "ANDROID") {
		t.Errorf("data leaks usernames or UIDs: %s", b)
	}
	u2, _ := url.Parse(link2["url"].(string))
	if st, d := read(u2.Path); st != 200 || len(d.Devices) != 2 || d.Devices[0]["callsign"] != nil {
		t.Errorf("every device without callsigns = %d %+v", st, d)
	}
	if st, _ := read("/share/not-a-real-token"); st != 410 {
		t.Errorf("unknown link = %d", st)
	}

	// Views are counted; a deleted link ends at once.
	adminGet := func(path string) []map[string]any {
		t.Helper()
		_, hc := adminJSON(t, a)
		r, err := hc.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var out []map[string]any
		json.NewDecoder(r.Body).Decode(&out)
		return out
	}
	for _, l := range adminGet("/admin/api/share-links") {
		if l["id"] == link["id"] && l["views"] != 1.0 {
			t.Errorf("views = %v", l["views"])
		}
	}
	resp2, err := func() (*http.Response, error) {
		req, _ := http.NewRequest("GET", base+"/admin/api/share-links", nil)
		return anon.Do(req)
	}()
	if err == nil {
		resp2.Body.Close()
		if resp2.StatusCode == 200 {
			t.Error("the link list is open without signing in")
		}
	}
	if st, _ := call("DELETE", "/admin/api/share-links/"+link["id"].(string), nil); st != 200 {
		t.Fatalf("delete = %d", st)
	}
	if st, _ := read(u.Path); st != 410 {
		t.Errorf("deleted link = %d", st)
	}
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return ""
}
