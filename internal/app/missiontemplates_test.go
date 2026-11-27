package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// TestMissionTemplates covers saving a mission as a template and making new
// missions from it: settings, files and map items (with fresh UIDs and the
// route's link to its point renamed to match), and invitations.
func TestMissionTemplates(t *testing.T) {
	a := startApp(t)
	for _, u := range []string{"alice", "bob"} {
		if _, err := a.Store.CreateUser(u, "password-"+u, []string{"Cyan"}, false); err != nil {
			t.Fatal(err)
		}
	}
	ac, bc := enroll(t, a, "alice", "password-alice"), enroll(t, a, "bob", "password-bob")
	as, bs := mustDial(t, a, &ac), mustDial(t, a, &bc)
	as.send(sa("ANDROID-alice", "ALICE", "Cyan"))
	bs.send(sa("ANDROID-bob", "BOB", "Cyan"))
	time.Sleep(200 * time.Millisecond)
	alice := newAPI(t, a, ac)
	base := "/Marti/api/missions/"

	// Alice's mission: a file, a waypoint and a route through it.
	alice.want(201, "PUT", base+"sar?creatorUid=ANDROID-alice&group=Cyan&description=Search+pattern&tool=public", nil)
	file := []byte("search plan " + time.Now().String())
	sum := sha256.Sum256(file)
	hash := hex.EncodeToString(sum[:])
	alice.want(200, "POST", "/Marti/sync/upload?name=plan.txt&keywords=missionpackage&creatorUid=ANDROID-alice", file)
	alice.want(200, "PUT", base+"sar/contents?creatorUid=ANDROID-alice", map[string]any{"hashes": []string{hash}})
	pt := cot.NewEvent("b-m-p-w", "pt-1", "h-g-i-g-o", time.Minute) // stale soon: the template mustn't keep that
	pt.Lat, pt.Lon = 38.9, -77.0
	pt.Detail.XML = `<contact callsign="Rally"/><marti><dest mission="sar"/></marti>`
	as.send(pt)
	rt := cot.NewEvent("b-m-r", "rt-1", "h-e", 24*time.Hour)
	rt.Lat, rt.Lon = 38.9, -77.0
	rt.Detail.XML = `<link uid="pt-1" type="b-m-p-w" relation="c" callsign="Rally" point="38.9,-77.0"/><contact callsign="Route 1"/><marti><dest mission="sar"/></marti>`
	as.send(rt)
	waitFor(t, "items in the mission", func() bool {
		m := firstMission(t, alice.want(200, "GET", base+"sar", nil))
		return len(m["uids"].([]any)) == 2
	})

	call, _ := adminJSON(t, a)
	if st, _ := call("POST", "/admin/api/mission-templates", map[string]any{"mission": "nope", "name": "x"}); st != 404 {
		t.Errorf("template from a missing mission = %d", st)
	}
	st, v := call("POST", "/admin/api/mission-templates", map[string]any{"mission": "sar", "name": "Search and rescue"})
	if st != 201 {
		t.Fatalf("save template = %d %v", st, v)
	}
	if st, _ := call("GET", "/admin/api/mission-templates", nil); st != 200 {
		t.Errorf("template list = %d", st)
	}
	tpls := a.Store.MissionTemplates()
	if len(tpls) != 1 || len(tpls[0].Files) != 1 || len(tpls[0].Items) != 2 || tpls[0].Description != "Search pattern" || mustJSON(tpls[0].Groups) != `["Cyan"]` {
		t.Fatalf("template = %+v", tpls)
	}
	id := tpls[0].ID

	// Rename it and make it read-only for subscribers.
	if st, _ := call("PUT", "/admin/api/mission-templates/"+id, map[string]any{"name": "SAR", "description": "Search pattern", "groups": []string{"Cyan"},
		"default_role": "MISSION_READONLY_SUBSCRIBER"}); st != 200 {
		t.Errorf("edit template = %d", st)
	}

	// A new mission from it, inviting Cyan's devices.
	if st, _ := call("POST", "/admin/api/mission-templates/"+id+"/create", map[string]any{"name": "sar"}); st != 409 {
		t.Errorf("an existing name = %d", st)
	}
	st, v = call("POST", "/admin/api/mission-templates/"+id+"/create", map[string]any{"name": "SAR 2026-11-27", "invite": true})
	if st != 201 || v["files"] != 1.0 || v["items"] != 2.0 || v["invited"].(float64) < 2 {
		t.Fatalf("create = %d %v", st, v)
	}
	bs.expect(func(e *cot.Event) bool { return e.Type == "t-x-m-i" && strings.Contains(e.Detail.XML, `name="SAR 2026-11-27"`) }, "invitation")

	m := firstMission(t, alice.want(200, "GET", base+"SAR%202026-11-27", nil))
	if role, _ := m["defaultRole"].(map[string]any); m["description"] != "Search pattern" || role["type"] != "MISSION_READONLY_SUBSCRIBER" {
		t.Errorf("mission settings = %v %v", m["description"], m["defaultRole"])
	}
	if c := m["contents"].([]any); len(c) != 1 || c[0].(map[string]any)["data"].(map[string]any)["hash"] != hash {
		t.Errorf("mission files = %v", c)
	}
	var newUIDs []string
	for _, u := range m["uids"].([]any) {
		newUIDs = append(newUIDs, u.(map[string]any)["data"].(string))
	}
	if len(newUIDs) != 2 || newUIDs[0] == "pt-1" || newUIDs[0] == "rt-1" || newUIDs[1] == "pt-1" || newUIDs[1] == "rt-1" {
		t.Fatalf("item UIDs = %v", newUIDs)
	}
	_, _, cotXML := alice.do("GET", base+"SAR%202026-11-27/cot", nil)
	if bytes.Contains(cotXML, []byte(`"pt-1"`)) || bytes.Contains(cotXML, []byte(`"rt-1"`)) {
		t.Errorf("old UIDs left in the new mission's CoT: %s", cotXML)
	}
	// The route links to the new point.
	var pointUID string
	for _, ev := range strings.Split(string(cotXML), "<event ")[1:] {
		if strings.Contains(ev, `type="b-m-p-w" time=`) { // the event's own type, not a link's
			pointUID = between(ev, `uid="`, `"`)
		}
	}
	if pointUID == "" || !bytes.Contains(cotXML, []byte(`<link uid="`+pointUID+`"`)) {
		t.Errorf("route doesn't link to the new point %q: %s", pointUID, cotXML)
	}
	// Fresh times: the waypoint, stale in a minute originally, lasts a year.
	if !bytes.Contains(cotXML, []byte(`stale="`+time.Now().UTC().AddDate(1, 0, 0).Format("2006-01-"))) {
		t.Errorf("items not given a long life: %s", cotXML)
	}
	// The original mission is untouched.
	if orig := firstMission(t, alice.want(200, "GET", base+"sar", nil)); len(orig["uids"].([]any)) != 2 {
		t.Errorf("original mission changed")
	}

	if st, _ := call("DELETE", "/admin/api/mission-templates/"+id, nil); st != 200 || len(a.Store.MissionTemplates()) != 0 {
		t.Errorf("delete = %d", st)
	}
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	if j := strings.Index(s, end); j >= 0 {
		return s[:j]
	}
	return ""
}
