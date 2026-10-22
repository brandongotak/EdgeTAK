package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// TestGeofences covers geofences: crossing a boundary alerts as set, the
// first position doesn't, crossing back ends the alert, the group filter
// applies, and devices get the area as an ATAK shape.
func TestGeofences(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct{ name, group string }{{"alpha", "Cyan"}, {"red1", "Red"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}
	call, _ := adminJSON(t, a)
	alphaCert, redCert := enroll(t, a, "alpha", "password-alpha"), enroll(t, a, "red1", "password-red1")
	alpha, red := mustDial(t, a, &alphaCert), mustDial(t, a, &redCert)
	at := func(c *streamClient, uid, cs string, lat, lon float64) {
		ev := sa(uid, cs, "Cyan")
		ev.Lat, ev.Lon = lat, lon
		c.send(ev)
	}
	alerts := func() []store.Emergency {
		var out []store.Emergency
		for _, e := range a.Store.Emergencies() {
			if e.Kind == "geofence" {
				out = append(out, e)
			}
		}
		return out
	}
	active := func(what string) bool {
		for _, e := range alerts() {
			if e.Type == what && e.Ended == nil {
				return true
			}
		}
		return false
	}

	// Bad areas are refused.
	for _, bad := range []map[string]any{
		{"name": "", "shape": "circle", "center": []float64{39, -106}, "radius": 100, "trigger": "both"},
		{"name": "x", "shape": "polygon", "points": [][]float64{{39, -106}, {39.1, -106}}, "trigger": "both"},
		{"name": "x", "shape": "circle", "center": []float64{39, -106}, "radius": 100, "trigger": "sometimes"},
	} {
		if code, _ := call("POST", "/admin/api/geofences", bad); code != 400 {
			t.Errorf("%v accepted (%d)", bad, code)
		}
	}

	// ALPHA is outside before the area exists: its first position after only
	// notes where it is.
	at(alpha, "ANDROID-alpha", "ALPHA", 39.60, -106.00)
	at(red, "ANDROID-red", "RED1", 39.60, -106.00)
	// A 2 km square around 39.50, -106.05, watching Cyan, on devices' maps.
	if code, _ := call("POST", "/admin/api/geofences", map[string]any{"name": "Training Area", "shape": "polygon",
		"points": [][]float64{{39.49, -106.06}, {39.49, -106.04}, {39.51, -106.04}, {39.51, -106.06}}, "trigger": "both",
		"groups": []string{"Cyan"}, "color": "#1E88E5", "devices": true}); code != 200 {
		t.Fatalf("create = %d", code)
	}
	shape := alpha.expect(func(e *cot.Event) bool { return e.Type == "u-d-f" }, "the area on ALPHA's map")
	if !strings.Contains(shape.Detail.XML, `<link point="39.4900000,-106.0600000"/>`) || strings.Count(shape.Detail.XML, "<link point") != 5 ||
		shape.Callsign() != "Training Area" {
		t.Errorf("shape = %s", shape.Detail.XML)
	}
	red.expectNone(func(e *cot.Event) bool { return e.Type == "u-d-f" }, "the area on RED1's map (not watched)")

	at(alpha, "ANDROID-alpha", "ALPHA", 39.61, -106.00) // first position since the area: outside
	time.Sleep(100 * time.Millisecond)
	if n := len(alerts()); n != 0 {
		t.Fatalf("%d alerts from a first position", n)
	}
	at(alpha, "ANDROID-alpha", "ALPHA", 39.50, -106.05) // in
	waitFor(t, "the enter alert", func() bool { return active("Entered Training Area") })
	at(red, "ANDROID-red", "RED1", 39.61, -106.00)
	at(red, "ANDROID-red", "RED1", 39.50, -106.05)      // RED1 is not watched
	at(alpha, "ANDROID-alpha", "ALPHA", 39.52, -106.05) // out again
	waitFor(t, "the exit alert", func() bool { return active("Left Training Area") })
	if active("Entered Training Area") {
		t.Error("the enter alert is still open after leaving")
	}
	for _, e := range alerts() {
		if e.Callsign != "ALPHA" {
			t.Errorf("alert for %s", e.Callsign)
		}
	}

	// A circle that only alerts on leaving.
	call("POST", "/admin/api/geofences", map[string]any{"name": "Camp", "shape": "circle", "center": []float64{39.52, -106.05}, "radius": 300, "trigger": "exit", "devices": true})
	at(alpha, "ANDROID-alpha", "ALPHA", 39.5201, -106.05) // inside the circle: first position, then no enter alert
	at(alpha, "ANDROID-alpha", "ALPHA", 39.5202, -106.0501)
	at(alpha, "ANDROID-alpha", "ALPHA", 39.53, -106.05) // 1.1 km away: left
	waitFor(t, "the circle's exit alert", func() bool { return active("Left Camp") })
	if active("Entered Camp") {
		t.Error("an exit-only circle alerted on entering")
	}

	// Deleting the area ends its alerts and removes it from devices.
	var fenceID string
	for _, g := range a.Store.Geofences() {
		if g.Name == "Training Area" {
			fenceID = g.ID
		}
	}
	if code, _ := call("DELETE", "/admin/api/geofences/"+fenceID, nil); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	alpha.expect(func(e *cot.Event) bool {
		return e.Type == "t-x-d-d" && strings.Contains(e.Detail.XML, "edgetak-geofence-"+fenceID)
	}, "the area removed from ALPHA's map")
	if active("Left Training Area") {
		t.Error("the deleted area's alert is still open")
	}

	// A device that connects later gets the areas it should see.
	alpha.conn.Close()
	alpha = mustDial(t, a, &alphaCert)
	alpha.expect(func(e *cot.Event) bool {
		return e.Type == "u-d-c-c" && strings.Contains(e.Detail.XML, `<ellipse major="300"`)
	}, "the circle on connect")
}
