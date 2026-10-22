package app

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/mission"
)

// TestEvents covers events: each one gets its group and mission; people
// join through its invite (or its shared account); when it expires every
// event account is erased, the group and invite removed, the mission kept.
func TestEvents(t *testing.T) {
	a := startApp(t)
	call, hc := adminJSON(t, a)
	plain := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	base := "https://" + a.Addrs["https"]
	signUp := func(invite, user string) int {
		b, _ := json.Marshal(map[string]string{"username": user, "password": "password-" + user, "invite": invite})
		resp, err := plain.Post(base+"/api/register", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	byName := func(name string) map[string]any {
		resp, err := hc.Get("https://" + a.Addrs["https"] + "/admin/api/events")
		if err != nil {
			t.Fatal(err)
		}
		var list []map[string]any
		json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		for _, e := range list {
			if e["name"] == name {
				return e
			}
		}
		return nil
	}

	ends := time.Now().Add(2 * time.Hour)
	if code, _ := call("POST", "/admin/api/events", map[string]any{"name": "Search 1", "mode": "person", "expires": time.Now().Add(-time.Hour)}); code != 400 {
		t.Errorf("event ending in the past = %d", code)
	}
	if code, _ := call("POST", "/admin/api/events", map[string]any{"name": "Search 1", "mode": "person", "expires": ends, "max_people": 2, "mission": true}); code != 201 {
		t.Fatalf("person event = %d", code)
	}
	if code, _ := call("POST", "/admin/api/events", map[string]any{"name": "Walk Up", "mode": "shared", "expires": ends}); code != 201 {
		t.Fatalf("shared event = %d", code)
	}
	search := byName("Search 1")
	invite, _ := search["invite"].(string)
	if search["group"] != "Search 1" || search["mission"] != "Search 1" || invite == "" || search["join_url"] == "" {
		t.Fatalf("person event = %v", search)
	}
	if _, ok := a.Store.Group("Search 1"); !ok {
		t.Error("no group")
	}
	if _, err := a.Missions.Get("Search 1", mission.Caller{Username: "admin", Admin: true}, "", mission.View{}); err != nil {
		t.Errorf("no mission: %v", err)
	}

	// People join, up to the limit; their accounts are in the event's group.
	if c := signUp(invite, "ann"); c != 201 {
		t.Fatalf("ann = %d", c)
	}
	if c := signUp(invite, "ben"); c != 201 {
		t.Fatalf("ben = %d", c)
	}
	if c := signUp(invite, "cat"); c != 410 {
		t.Errorf("third person over the limit = %d", c)
	}
	if u, _ := a.Store.User("ann"); u.Event != search["id"] || len(u.Groups) != 1 || u.Groups[0] != "Search 1" {
		t.Errorf("ann = %+v", u)
	}
	annCert := enroll(t, a, "ann", "password-ann")
	ann := newAPI(t, a, annCert)

	// The shared account works with its password.
	walk := byName("Walk Up")
	account, _ := walk["account"].(string)
	password, _ := walk["password"].(string)
	if account != "event-walk-up" || password == "" || walk["enroll_link"] == "" {
		t.Fatalf("shared event = %v", walk)
	}
	enroll(t, a, account, password)

	// A non-event user keeps everything.
	a.Store.CreateUser("staff", "password-staff", []string{"Search 1"}, false)

	// Extending moves the invite's end too.
	later := ends.Add(time.Hour)
	if code, _ := call("PATCH", "/admin/api/events/"+search["id"].(string), map[string]any{"expires": later}); code != 200 {
		t.Errorf("extend = %d", code)
	}
	if inv, _ := a.Store.Invite(invite); !inv.Expires.Equal(later.UTC().Truncate(time.Second)) && inv.Expires.Sub(later) > time.Second {
		t.Errorf("invite end = %v, want %v", inv.Expires, later)
	}

	// Time's up for both.
	a.Web.EndExpiredEvents(later.Add(time.Minute))
	for _, name := range []string{"ann", "ben", account} {
		if _, ok := a.Store.User(name); ok {
			t.Errorf("%s still exists", name)
		}
	}
	if st, _, _ := ann.do("GET", "/Marti/api/tls/config", nil); st != 401 {
		t.Errorf("ann's certificate after the event = %d", st)
	}
	if _, ok := a.Store.Group("Search 1"); ok {
		t.Error("the event's group is still there")
	}
	if u, ok := a.Store.User("staff"); !ok || len(u.Groups) != 0 {
		t.Errorf("staff = %+v", u)
	}
	if c := signUp(invite, "dan"); c != 410 {
		t.Errorf("sign-up after the event = %d", c)
	}
	if _, err := a.Missions.Get("Search 1", mission.Caller{Username: "admin", Admin: true}, "", mission.View{}); err != nil {
		t.Errorf("the mission was not kept: %v", err)
	}
	if e := byName("Search 1"); e["ended_by"] != "expired" || e["removed"] != float64(2) {
		t.Errorf("ended event = %v", e)
	}
}
