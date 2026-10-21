package app

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

func TestPositionHistory(t *testing.T) {
	a := startApp(t)
	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]
	if _, err := a.Store.CreateUser("alice", "password-alice", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	cert := enroll(t, a, "alice", "password-alice")
	c := mustDial(t, a, &cert)

	tracks := func() []map[string]any {
		t.Helper()
		resp, err := hc.Get(base + "/admin/api/history?minutes=10")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out []map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	// Off by default.
	c.send(sa("ANDROID-alice", "ALICE", "Cyan"))
	time.Sleep(200 * time.Millisecond)
	if tr := tracks(); len(tr) != 0 {
		t.Fatalf("recorded while off: %v", tr)
	}
	if code, _ := call("PUT", "/admin/api/history/settings", map[string]any{"enabled": true, "retention_hours": 0}); code != http.StatusBadRequest {
		t.Fatalf("retention 0 accepted: %d", code)
	}
	if code, st := call("PUT", "/admin/api/history/settings", map[string]any{"enabled": true, "retention_hours": 48}); code != 200 || st["enabled"] != true {
		t.Fatalf("enable = %d %v", code, st)
	}

	// Two of the device's own reports, far enough apart to both be kept, and
	// a marker it shares, which is not part of its trail.
	c.send(sa("ANDROID-alice", "ALICE", "Cyan"))
	marker := cot.NewEvent("a-h-G", "marker-1", "h-g-i-g-o", time.Hour)
	marker.Lat, marker.Lon = 38.95, -77.1
	c.send(marker)
	time.Sleep(3100 * time.Millisecond)
	moved := sa("ANDROID-alice", "ALICE", "Cyan")
	moved.Lat += 0.001
	c.send(moved)
	time.Sleep(300 * time.Millisecond)

	tr := tracks()
	if len(tr) != 1 || tr[0]["uid"] != "ANDROID-alice" || tr[0]["callsign"] != "ALICE" || tr[0]["team"] != "Cyan" {
		t.Fatalf("tracks = %v", tr)
	}
	if pts := tr[0]["points"].([]any); len(pts) != 2 {
		t.Fatalf("points = %v", pts)
	}
	if _, st := call("GET", "/admin/api/history/settings", nil); st["points"] != 2.0 || st["retention_hours"] != 48.0 {
		t.Fatalf("status = %v", st)
	}

	if code, _ := call("DELETE", "/admin/api/history", nil); code != http.StatusNoContent {
		t.Fatalf("clear = %d", code)
	}
	if tr := tracks(); len(tr) != 0 {
		t.Fatalf("after clear: %v", tr)
	}
}
