package app

import (
	"strings"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// TestSimulator covers the console's simulator: simulated devices connect,
// report and see each other's reports, with the relay time measured; real
// devices outside the simulator's group don't see them; stopping removes
// the temporary account and group.
func TestSimulator(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("alpha", "password-alpha", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, _ := adminJSON(t, a)
	cert := enroll(t, a, "alpha", "password-alpha")
	alpha := mustDial(t, a, &cert)

	if code, _ := call("POST", "/admin/api/sim/start", map[string]any{"devices": 0, "center": []float64{39.7, -105}}); code != 400 {
		t.Errorf("no devices = %d", code)
	}
	if code, v := call("POST", "/admin/api/sim/start", map[string]any{"devices": 20, "ramp": 200, "interval": 0.5, "chat_every": 1,
		"center": []float64{39.7, -105}, "radius": 500}); code != 200 {
		t.Fatalf("start = %d %v", code, v)
	}
	if code, _ := call("POST", "/admin/api/sim/start", map[string]any{"devices": 5, "center": []float64{39.7, -105}}); code != 409 {
		t.Errorf("a second run = %d", code)
	}
	var stats map[string]any
	waitFor(t, "20 devices relaying, with latency measured", func() bool {
		_, v := call("GET", "/admin/api/sim", nil)
		stats, _ = v["stats"].(map[string]any)
		return stats != nil && stats["connected"] == float64(20) && stats["received"].(float64) > 100 && stats["latency_samples"].(float64) > 0
	})
	if stats["failed"] != float64(0) || stats["dropped"] != float64(0) || stats["latency_p95_ms"].(float64) <= 0 {
		t.Errorf("stats = %v", stats)
	}
	if u, ok := a.Store.User("edgetak-sim"); !ok || len(u.Groups) != 1 || u.Groups[0] != "Simulator" {
		t.Errorf("simulator account = %+v", u)
	}
	alpha.expectNone(func(e *cot.Event) bool { return strings.HasPrefix(e.UID, "EdgeTAK-SIM-") || e.Type == "b-t-f" }, "simulated traffic at a Cyan device")

	code, v := call("POST", "/admin/api/sim/stop", nil)
	if code != 200 || v["running"] != false {
		t.Fatalf("stop = %d %v", code, v)
	}
	if _, ok := a.Store.User("edgetak-sim"); ok {
		t.Error("the simulator account is still there")
	}
	if _, ok := a.Store.Group("Simulator"); ok {
		t.Error("the simulator's group is still there")
	}
	waitFor(t, "simulated devices gone", func() bool {
		for _, c := range a.Router.Clients() {
			if c.Username == "edgetak-sim" {
				return false
			}
		}
		return true
	})
	if final, _ := v["stats"].(map[string]any); final == nil || final["sent"].(float64) == 0 {
		t.Errorf("result kept = %v", v)
	}
}
