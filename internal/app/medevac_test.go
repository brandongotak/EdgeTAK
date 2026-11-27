package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// medevacEvent is a 9-line request as ATAK's MedLine tool sends it.
func medevacEvent(uid string) *cot.Event {
	ev := cot.NewEvent("b-r-f-h-c", uid, "h-g-i-g-o", time.Hour)
	ev.Lat, ev.Lon = 38.8895, -77.0353
	ev.Detail.XML = `<contact callsign="MED.1"/><_medevac_ title="MED.1" freq="34.50" urgent="2" priority="1" routine="0" hoist="true" ` +
		`equipment_other="true" equipment_detail="vacuum splint" litter="2" ambulatory="1" security="1" hlz_marking="2" us_military="3" ` +
		`terrain_slope="true" terrain_slope_dir="N" obstacles="power lines east" medline_remarks="two with leg wounds" casevac="false">` +
		`<zMistsMap><zMist title="ZMIST1" z="A1" m="IED" i="leg" s="stable" t="tourniquet"/></zMistsMap></_medevac_>`
	return ev
}

// TestMedevac covers the MEDEVAC board: requests from ATAK recorded with
// every line, a webhook alert for new ones only, statuses, removal on the
// device, and removal from the board.
func TestMedevac(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("medic", "password-medic", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]
	var mu sync.Mutex
	var hooked []map[string]any
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		hooked = append(hooked, m)
		mu.Unlock()
	}))
	defer recv.Close()
	a.Web.WebhookClient = recv.Client()
	call("POST", "/admin/api/webhooks", map[string]any{"name": "Medics", "url": recv.URL + "/x", "format": "json", "events": []string{"medevac"}})

	cert := enroll(t, a, "medic", "password-medic")
	dev := mustDial(t, a, &cert)
	dev.send(sa("ANDROID-medic", "MEDIC", "Cyan"))
	time.Sleep(200 * time.Millisecond)
	dev.send(medevacEvent("med-1"))

	type board []struct {
		ID, Title, Status, MGRS, Summary string
		Fields                           map[string]string
		ZMists                           []map[string]string
		FromCallsign                     string `json:"from_callsign"`
		Username                         string
		Removed                          bool
		History                          []map[string]any
	}
	get := func() board {
		t.Helper()
		r, err := hc.Get(base + "/admin/api/medevac")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		var b board
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("board: %v %s", err, raw)
		}
		return b
	}
	waitFor(t, "request on the board", func() bool { return len(get()) == 1 })
	b := get()
	m := b[0]
	if m.ID != "med-1" || m.Title != "MED.1" || m.Status != "open" || m.FromCallsign != "MEDIC" || m.Username != "medic" ||
		m.Fields["urgent"] != "2" || m.Fields["equipment_detail"] != "vacuum splint" || m.Fields["obstacles"] != "power lines east" ||
		len(m.ZMists) != 1 || m.ZMists[0]["m"] != "IED" || !strings.HasPrefix(m.MGRS, "18SUJ23") {
		t.Fatalf("request = %+v", m)
	}
	if want := "2 urgent, 1 priority · 2 litter, 1 ambulatory · security P - possible enemy · MGRS 18SUJ"; !strings.HasPrefix(m.Summary, want) {
		t.Errorf("summary = %q", m.Summary)
	}
	waitFor(t, "webhook alert", func() bool { mu.Lock(); defer mu.Unlock(); return len(hooked) == 1 })
	mu.Lock()
	if h := hooked[0]; h["event"] != "medevac" || h["title"] != "MEDEVAC request: MED.1" || !strings.Contains(h["text"].(string), "2 urgent") {
		t.Errorf("webhook = %v", h)
	}
	mu.Unlock()

	// The device resends it (ATAK does): no second alert.
	dev.send(medevacEvent("med-1"))
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	if len(hooked) != 1 {
		t.Errorf("alerts after a resend = %d", len(hooked))
	}
	mu.Unlock()

	// Statuses.
	if st, _ := call("PUT", "/admin/api/medevac/med-1/status", map[string]any{"status": "flying"}); st != 400 {
		t.Errorf("bad status = %d", st)
	}
	if st, _ := call("PUT", "/admin/api/medevac/nope/status", map[string]any{"status": "complete"}); st != 404 {
		t.Errorf("missing request = %d", st)
	}
	call("PUT", "/admin/api/medevac/med-1/status", map[string]any{"status": "en_route"})
	if m := get()[0]; m.Status != "en_route" || len(m.History) < 2 || m.History[len(m.History)-1]["by"] != "admin" {
		t.Errorf("after status = %+v", m)
	}
	r, _ := hc.Get(base + "/admin/api/medevac?summary=1")
	var sum struct{ Active int }
	json.NewDecoder(r.Body).Decode(&sum)
	r.Body.Close()
	if sum.Active != 1 {
		t.Errorf("active = %d", sum.Active)
	}

	// Deleted on the device.
	del := cot.NewEvent("t-x-d-d", "del-1", "h-g-i-g-o", time.Minute)
	del.Detail.XML = `<link uid="med-1" relation="none" type="b-r-f-h-c"/>`
	dev.send(del)
	waitFor(t, "removed on the device", func() bool { return get()[0].Removed })

	if st, _ := call("DELETE", "/admin/api/medevac/med-1", nil); st != 200 || len(get()) != 0 {
		t.Errorf("remove from board = %d", st)
	}
}
