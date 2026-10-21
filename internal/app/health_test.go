package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHealthEndpoints(t *testing.T) {
	a := startApp(t)
	call, hc := adminJSON(t, a)
	if _, err := a.Store.CreateUser("alice", "password-alice", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	cert := enroll(t, a, "alice", "password-alice")
	c := mustDial(t, a, &cert)
	ev := sa("ANDROID-alice", "ALICE", "Cyan")
	ev.Detail.Status.Battery = 12
	c.send(ev)
	time.Sleep(300 * time.Millisecond)

	code, h := call("GET", "/admin/api/health?minutes=60", nil)
	if code != 200 || h["info"] == nil || h["version"] == nil {
		t.Fatalf("health = %d %v", code, h)
	}

	resp, err := hc.Get("https://" + a.Addrs["https"] + "/admin/api/health/devices")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var devs []map[string]any
	json.NewDecoder(resp.Body).Decode(&devs)
	if len(devs) != 1 || devs[0]["callsign"] != "ALICE" || devs[0]["worst"] != "critical" {
		t.Fatalf("devices = %v", devs)
	}
	issues, _ := json.Marshal(devs[0]["issues"])
	if !strings.Contains(string(issues), "Battery 12%") {
		t.Fatalf("issues = %s", issues)
	}
	if hist, _ := devs[0]["battery_history"].([]any); len(hist) != 1 {
		t.Fatalf("battery history = %v", devs[0]["battery_history"])
	}
	// CoT totals count this device's traffic.
	if in, _, _ := a.Router.Totals(); in < 1 {
		t.Fatalf("router totals in = %d", in)
	}
}
