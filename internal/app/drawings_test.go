package app

import (
	"strings"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// TestDrawings covers drawings: they reach the devices they are meant for
// as ATAK items, again on connect, change in place, and are removed.
func TestDrawings(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct{ name, group string }{{"alpha", "Cyan"}, {"red1", "Red"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}
	call, _ := adminJSON(t, a)
	alphaCert, redCert := enroll(t, a, "alpha", "password-alpha"), enroll(t, a, "red1", "password-red1")
	alpha, red := mustDial(t, a, &alphaCert), mustDial(t, a, &redCert)
	idOf := func(name string) string {
		for _, d := range a.Store.Drawings() {
			if d.Name == name {
				return d.ID
			}
		}
		return ""
	}

	for _, bad := range []map[string]any{
		{"kind": "marker", "name": "x", "type": "a-x-Z", "center": []float64{39, -106}},
		{"kind": "route", "name": "x", "points": [][]float64{{39, -106}}},
		{"kind": "blob", "name": "x"},
		{"kind": "marker", "name": "", "type": "a-h-G", "center": []float64{39, -106}},
	} {
		if code, _ := call("POST", "/admin/api/drawings", bad); code != 400 {
			t.Errorf("%v accepted (%d)", bad, code)
		}
	}

	// A hostile marker for everyone.
	if code, _ := call("POST", "/admin/api/drawings", map[string]any{"kind": "marker", "name": "Enemy OP", "type": "a-h-G",
		"center": []float64{39.5, -106.05}, "remarks": "seen 0600"}); code != 200 {
		t.Fatalf("marker = %d", code)
	}
	for who, c := range map[string]*streamClient{"ALPHA": alpha, "RED1": red} {
		m := c.expect(func(e *cot.Event) bool { return e.Type == "a-h-G" }, "the marker at "+who)
		if m.Callsign() != "Enemy OP" || m.Lat != 39.5 || !strings.Contains(m.Detail.XML, "seen 0600") || !strings.Contains(m.Detail.XML, "<archive") {
			t.Errorf("marker at %s = %+v %s", who, m, m.Detail.XML)
		}
	}
	// A route for Cyan only.
	if code, _ := call("POST", "/admin/api/drawings", map[string]any{"kind": "route", "name": "Route Blue",
		"points": [][]float64{{39.50, -106.05}, {39.51, -106.04}, {39.52, -106.03}}, "groups": []string{"Cyan"}, "color": "#1E88E5"}); code != 200 {
		t.Fatalf("route = %d", code)
	}
	route := alpha.expect(func(e *cot.Event) bool { return e.Type == "b-m-r" }, "the route at ALPHA")
	if strings.Count(route.Detail.XML, `relation="c"`) != 3 || !strings.Contains(route.Detail.XML, `type="b-m-p-w"`) ||
		!strings.Contains(route.Detail.XML, `type="b-m-p-c"`) || !strings.Contains(route.Detail.XML, "<link_attr") {
		t.Errorf("route = %s", route.Detail.XML)
	}
	red.expectNone(func(e *cot.Event) bool { return e.Type == "b-m-r" }, "the route at RED1 (not Cyan)")

	// Changing the marker sends it again with the same UID.
	markerID := idOf("Enemy OP")
	call("PUT", "/admin/api/drawings/"+markerID, map[string]any{"kind": "marker", "name": "Enemy OP", "type": "a-h-G",
		"center": []float64{39.6, -106.05}, "remarks": "moved"})
	moved := alpha.expect(func(e *cot.Event) bool { return e.Type == "a-h-G" && e.Lat == 39.6 }, "the moved marker")
	if moved.UID != "edgetak-drawing-"+markerID {
		t.Errorf("moved marker UID = %s", moved.UID)
	}

	// Deleting removes it from devices.
	call("DELETE", "/admin/api/drawings/"+markerID, nil)
	alpha.expect(func(e *cot.Event) bool {
		return e.Type == "t-x-d-d" && strings.Contains(e.Detail.XML, "edgetak-drawing-"+markerID)
	}, "the marker removed at ALPHA")

	// A device that connects later gets what it should see.
	alpha.conn.Close()
	alpha = mustDial(t, a, &alphaCert)
	alpha.expect(func(e *cot.Event) bool { return e.Type == "b-m-r" && e.UID == "edgetak-drawing-"+idOf("Route Blue") }, "the route on connect")
	alpha.expectNone(func(e *cot.Event) bool { return e.Type == "a-h-G" }, "the deleted marker on connect")
}
