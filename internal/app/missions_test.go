package app

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/mission"
)

type apiClient struct {
	t    *testing.T
	base string
	hc   *http.Client
}

func newAPI(t *testing.T, a *App, cert tls.Certificate) *apiClient {
	return &apiClient{t: t, base: "https://" + a.Addrs["https"], hc: &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool(), Certificates: []tls.Certificate{cert}})}}
}

// do sends a request and decodes a JSON response into a generic map.
func (c *apiClient) do(method, path string, body any) (int, map[string]any, []byte) {
	c.t.Helper()
	var rd io.Reader
	if b, ok := body.([]byte); ok {
		rd = bytes.NewReader(b)
	} else if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	return resp.StatusCode, m, raw
}

func (c *apiClient) want(status int, method, path string, body any) map[string]any {
	c.t.Helper()
	got, m, raw := c.do(method, path, body)
	if got != status {
		c.t.Fatalf("%s %s = %d, want %d: %s", method, path, got, status, raw)
	}
	return m
}

// firstMission returns data[0] of a Mission envelope.
func firstMission(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	if env["type"] != "Mission" {
		t.Fatalf("envelope type = %v", env["type"])
	}
	data, _ := env["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected one mission, got %v", env["data"])
	}
	return data[0].(map[string]any)
}

func isType(typ string) func(*cot.Event) bool {
	return func(e *cot.Event) bool { return e.Type == typ }
}

func TestDataSync(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct{ name, group string }{{"alice", "Cyan"}, {"bob", "Cyan"}, {"carol", "Cyan"}, {"eve", "Red"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}
	certs := map[string]tls.Certificate{}
	streams := map[string]*streamClient{}
	for _, n := range []string{"alice", "bob", "carol", "eve"} {
		cert := enroll(t, a, n, "password-"+n)
		certs[n] = cert
		streams[n] = mustDial(t, a, &cert)
	}
	streams["bob"].negotiate() // bob receives protobuf
	for n, s := range streams {
		s.send(sa("ANDROID-"+n, strings.ToUpper(n), "Cyan"))
	}
	time.Sleep(200 * time.Millisecond) // let the router learn every client's UID
	alice, bob, eve := newAPI(t, a, certs["alice"]), newAPI(t, a, certs["bob"]), newAPI(t, a, certs["eve"])
	base := "/Marti/api/missions/"

	// Create: 201, owner token, creator subscribed, CREATE announced to the group.
	m := firstMission(t, alice.want(201, "PUT", base+"ops?creatorUid=ANDROID-alice&group=Cyan&description=Operation&tool=public", nil))
	if m["name"] != "ops" || m["token"] == "" || m["creatorUid"] != "ANDROID-alice" || m["passwordProtected"] != false {
		t.Fatalf("created mission = %v", m)
	}
	if dr := m["defaultRole"].(map[string]any); dr["type"] != mission.RoleSubscriber {
		t.Errorf("defaultRole = %v", dr)
	}
	streams["bob"].expect(isType("t-x-m-n"), "mission create announcement")
	streams["eve"].expectNone(isType("t-x-m-n"), "create announcement in other group")

	// Listing is group scoped.
	if data := bob.want(200, "GET", "/Marti/api/missions?passwordProtected=true&defaultRole=true", nil)["data"].([]any); len(data) != 1 {
		t.Errorf("bob sees %d missions", len(data))
	}
	if data := eve.want(200, "GET", "/Marti/api/missions", nil)["data"].([]any); len(data) != 0 {
		t.Errorf("eve sees %d missions", len(data))
	}
	if env := bob.want(200, "GET", "/Marti/api/pagedmissions?page=0&pagesize=10&nameFilter=OP", nil); len(env["data"].([]any)) != 1 {
		t.Errorf("pagedmissions = %v", env["data"])
	}
	if env := bob.want(200, "GET", "/Marti/api/missioncount", nil); env["data"] != float64(1) {
		t.Errorf("missioncount = %v", env["data"])
	}
	eve.want(404, "GET", base+"ops", nil)
	eve.want(404, "PUT", base+"ops/subscription?uid=ANDROID-eve", nil)

	// Subscribe.
	sub := bob.want(201, "PUT", base+"ops/subscription?uid=ANDROID-bob", nil)
	sd := sub["data"].(map[string]any)
	if sub["type"] != "com.bbn.marti.sync.model.MissionSubscription" || sd["token"] == "" || sd["role"].(map[string]any)["type"] != mission.RoleSubscriber {
		t.Fatalf("subscription = %v", sub)
	}

	// A map item sent to the mission reaches subscribers only and joins the mission.
	marker := cot.NewEvent("a-h-G", "marker-1", "h-g-i-g-o", time.Hour)
	marker.Lat, marker.Lon = 34.5, -117.25
	marker.Detail.Contact = &cot.Contact{Callsign: "Hostile 1"}
	marker.Detail.XML = `<usericon iconsetpath="COT_MAPPING_2525B/a-h/a-h-G"/><marti><dest mission="ops"/></marti>`
	streams["alice"].send(marker)
	evs := streams["bob"].expectAll("mission CoT and its ADD_CONTENT notification", isFrom("marker-1"), isType("t-x-m-c"))
	if evs[0].Callsign() != "Hostile 1" {
		t.Errorf("marker callsign = %q", evs[0].Callsign())
	}
	change := evs[1]
	for _, want := range []string{`name="ops"`, "<type>ADD_CONTENT</type>", "<contentUid>marker-1</contentUid>", `callsign="Hostile 1"`, `iconsetPath="COT_MAPPING_2525B/a-h/a-h-G"`, `<location lat="34.5" lon="-117.25"/>`} {
		if !strings.Contains(change.Detail.XML, want) {
			t.Errorf("change notification missing %s:\n%s", want, change.Detail.XML)
		}
	}
	streams["carol"].expectNone(isFrom("marker-1"), "mission CoT at non-subscriber")
	streams["eve"].expectNone(isFrom("marker-1"), "mission CoT in other group")

	m = firstMission(t, bob.want(200, "GET", base+"ops", nil))
	uids := m["uids"].([]any)
	if len(uids) != 1 || uids[0].(map[string]any)["data"] != "marker-1" || uids[0].(map[string]any)["details"].(map[string]any)["callsign"] != "Hostile 1" {
		t.Errorf("mission uids = %v", uids)
	}
	_, _, cotXML := bob.do("GET", base+"ops/cot", nil)
	if !bytes.Contains(cotXML, []byte(`uid="marker-1"`)) || bytes.Contains(cotXML, []byte("<marti>")) || !bytes.HasSuffix(cotXML, []byte("</events>")) {
		t.Errorf("mission cot = %s", cotXML)
	}

	// Clients fetch announced items by UID.
	_, _, one := bob.do("GET", "/Marti/api/cot/xml/marker-1", nil)
	if !bytes.Contains(one, []byte(`uid="marker-1"`)) || !bytes.HasPrefix(one, []byte("<?xml")) {
		t.Errorf("cot/xml = %s", one)
	}
	if _, _, all := bob.do("GET", "/Marti/api/cot/xml/marker-1/all", nil); !bytes.Contains(all, []byte(`<events><event`)) {
		t.Errorf("cot/xml/all = %s", all)
	}
	eve.want(404, "GET", "/Marti/api/cot/xml/marker-1", nil)
	if _, _, many := bob.do("POST", "/Marti/api/cot", []string{"marker-1", "ANDROID-alice", "nope"}); bytes.Count(many, []byte("<event ")) != 2 {
		t.Errorf("POST cot = %s", many)
	}
	if env := bob.want(200, "GET", "/Marti/api/groups/groupCacheEnabled", nil); env["data"] != true {
		t.Errorf("groupCacheEnabled = %v", env)
	}

	// Add a file: upload, then PUT contents by hash.
	file := []byte("route plan " + time.Now().String())
	sum := sha256.Sum256(file)
	hash := hex.EncodeToString(sum[:])
	alice.want(200, "POST", "/Marti/sync/upload?name=plan.txt&keywords=missionpackage&creatorUid=ANDROID-alice", file)
	m = firstMission(t, alice.want(200, "PUT", base+"ops/contents?creatorUid=ANDROID-alice", map[string]any{"hashes": []string{hash}}))
	contents := m["contents"].([]any)
	if len(contents) != 1 || contents[0].(map[string]any)["data"].(map[string]any)["hash"] != hash {
		t.Fatalf("mission contents = %v", contents)
	}
	change = streams["bob"].expect(func(e *cot.Event) bool {
		return e.Type == "t-x-m-c" && strings.Contains(e.Detail.XML, "<contentResource>")
	}, "file ADD_CONTENT notification")
	if !strings.Contains(change.Detail.XML, "<hash>"+hash+"</hash>") || !strings.Contains(change.Detail.XML, "<filename>plan.txt</filename>") {
		t.Errorf("file change = %s", change.Detail.XML)
	}

	// TAKX forms: /Marti/api/sync/content, per-mission contacts/children/token/invitations.
	if _, _, dl := bob.do("GET", "/Marti/api/sync/content?hash="+hash, nil); !bytes.Equal(dl, file) {
		t.Errorf("/Marti/api/sync/content returned %q", dl)
	}
	if env := bob.want(200, "GET", base+"ops/contacts", nil); len(env["data"].([]any)) != 2 {
		t.Errorf("mission contacts = %v", env["data"])
	}
	bob.want(200, "GET", base+"ops/children", nil)
	bob.want(200, "GET", base+"ops/invitations", nil)
	if env := bob.want(200, "GET", base+"ops/token", nil); env["data"] == "" {
		t.Errorf("mission token = %v", env)
	}
	bob.want(200, "PUT", "/Marti/api/groups/active", []byte(`[]`))

	// The change feed and subscriber list.
	ch := bob.want(200, "GET", base+"ops/changes?secago=3600", nil)["data"].([]any)
	if len(ch) < 2 {
		t.Errorf("changes = %v", ch)
	}
	roles := alice.want(200, "GET", base+"ops/subscriptions/roles", nil)["data"].([]any)
	if len(roles) != 2 {
		t.Errorf("subscription roles = %v", roles)
	}

	// Logs.
	alice.want(201, "POST", "/Marti/api/missions/logs/entries", map[string]any{
		"content": "Contact at grid 123", "creatorUid": "ANDROID-alice", "missionNames": []string{"ops"}, "dtg": "2026-10-20T12:00:00.000Z",
	})
	streams["bob"].expect(isType("t-x-m-c-l"), "log notification")
	logs := bob.want(200, "GET", base+"ops/log", nil)["data"].([]any)
	if len(logs) != 1 || logs[0].(map[string]any)["content"] != "Contact at grid 123" {
		t.Errorf("logs = %v", logs)
	}

	// Read-only subscribers cannot change contents.
	alice.want(200, "PUT", base+"ops/role?clientUid=ANDROID-bob&role=MISSION_READONLY_SUBSCRIBER&creatorUid=ANDROID-alice", nil)
	streams["bob"].expect(isType("t-x-m-r"), "role change notification")
	bob.want(403, "PUT", base+"ops/contents?creatorUid=ANDROID-bob", map[string]any{"uids": []string{"ANDROID-bob"}})
	bob.want(403, "DELETE", base+"ops?creatorUid=ANDROID-bob", nil)
	bob.want(200, "GET", base+"ops", nil)

	// Removing an item is announced.
	alice.want(200, "DELETE", base+"ops/contents?uid=marker-1&creatorUid=ANDROID-alice", nil)
	streams["bob"].expect(func(e *cot.Event) bool {
		return e.Type == "t-x-m-c" && strings.Contains(e.Detail.XML, "REMOVE_CONTENT")
	}, "REMOVE_CONTENT notification")

	// Password-protected missions.
	alice.want(201, "PUT", base+"secret?creatorUid=ANDROID-alice&group=Cyan&password=hunter22", nil)
	if data := bob.want(200, "GET", "/Marti/api/missions", nil)["data"].([]any); len(data) != 1 {
		t.Errorf("protected mission listed without passwordProtected=true: %d", len(data))
	}
	if data := bob.want(200, "GET", "/Marti/api/missions?passwordProtected=true", nil)["data"].([]any); len(data) != 2 {
		t.Errorf("passwordProtected=true list = %d", len(data))
	}
	bob.want(403, "GET", base+"secret", nil)
	bob.want(403, "PUT", base+"secret/subscription?uid=ANDROID-bob&password=wrong", nil)
	tok := bob.want(201, "PUT", base+"secret/subscription?uid=ANDROID-bob&password=hunter22", nil)["data"].(map[string]any)["token"].(string)
	bob.want(200, "GET", base+"secret", nil) // subscribed now
	// The token alone grants access (as ATAK sends it in the Authorization header).
	req, _ := http.NewRequest("GET", carolBase(a)+base+"secret", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := newAPI(t, a, certs["carol"]).hc.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("token access = %v %v", resp.StatusCode, err)
	}

	// Archive is an importable data package.
	_, _, zipData := bob.do("GET", base+"ops/archive", nil)
	if !bytes.HasPrefix(zipData, []byte("PK")) {
		t.Errorf("archive is not a zip: %q", zipData[:min(40, len(zipData))])
	}

	// Missions survive a restart.
	a.Missions.Flush()
	again, err := mission.Open(filepath.Join(a.DataDir, "missions"), a.Store, a.Router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]mission.Summary{}
	for _, s := range again.Summaries() {
		names[s.Name] = s
	}
	if s := names["ops"]; s.Files != 1 || len(s.Subscribers) != 2 || s.Logs != 1 || !names["secret"].Protected {
		t.Errorf("reloaded missions = %+v", names)
	}

	// GUID-addressed routes reach the same mission, including names with spaces.
	night := firstMission(t, alice.want(201, "PUT", base+"Night%20Ops?creatorUid=ANDROID-alice&group=Cyan", nil))
	byGUID := firstMission(t, bob.want(200, "GET", base+"guid/"+night["guid"].(string), nil))
	if byGUID["name"] != "Night Ops" {
		t.Errorf("guid lookup = %v", byGUID["name"])
	}
	bob.want(201, "PUT", base+"guid/"+night["guid"].(string)+"/subscription?uid=ANDROID-bob", nil)
	bob.want(200, "GET", base+"guid/"+night["guid"].(string)+"/changes", nil)
	bob.want(404, "GET", base+"guid/00000000-0000-0000-0000-000000000000", nil)

	// Delete notifies subscribers.
	alice.want(200, "DELETE", base+"ops?creatorUid=ANDROID-alice", nil)
	streams["bob"].expect(isType("t-x-m-d"), "mission delete notification")
	bob.want(404, "GET", base+"ops", nil)
}

// expectAll waits until one event matching each predicate arrived, in any order.
func (c *streamClient) expectAll(what string, preds ...func(*cot.Event) bool) []*cot.Event {
	c.t.Helper()
	got := make([]*cot.Event, len(preds))
	missing := len(preds)
	deadline := time.After(3 * time.Second)
	for missing > 0 {
		select {
		case ev, ok := <-c.events:
			if !ok {
				c.t.Fatalf("connection closed waiting for %s", what)
			}
			for i, p := range preds {
				if got[i] == nil && p(ev) {
					got[i] = ev
					missing--
					break
				}
			}
		case <-deadline:
			c.t.Fatalf("timed out waiting for %s", what)
		}
	}
	return got
}

func carolBase(a *App) string { return "https://" + a.Addrs["https"] }
