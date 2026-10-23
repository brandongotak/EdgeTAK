package app

import (
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/config"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// TestLostContact covers lost contact alerts: a device that signs off is
// only listed; one whose connection goes silent raises an alert after the
// grace period, which ends when it is back; one that stays connected
// without a position raises its own; and the group filter applies.
func TestLostContact(t *testing.T) {
	a := startAppWith(t, func(c *config.Config) { c.IdleTimeoutSeconds = 1 })
	for _, u := range []string{"alpha", "bravo", "charlie"} {
		if _, err := a.Store.CreateUser(u, "password-"+u, []string{"Cyan"}, false); err != nil {
			t.Fatal(err)
		}
	}
	lostAlerts := func() map[string]store.Emergency {
		out := map[string]store.Emergency{}
		for _, e := range a.Store.Emergencies() {
			if e.Kind == "lost" {
				out[e.Callsign] = e
			}
		}
		return out
	}
	offline := func(callsign string) (signedOff, found bool) {
		for _, d := range a.Web.LostContactOffline() {
			if d.Callsign == callsign {
				return d.Clean, true
			}
		}
		return false, false
	}

	alphaCert, bravoCert := enroll(t, a, "alpha", "password-alpha"), enroll(t, a, "bravo", "password-bravo")
	alpha, bravo := mustDial(t, a, &alphaCert), mustDial(t, a, &bravoCert)
	alpha.send(sa("ANDROID-alpha", "ALPHA", "Cyan"))
	bravo.send(sa("ANDROID-bravo", "BRAVO", "Cyan"))
	waitFor(t, "both identified", func() bool {
		n := 0
		for _, c := range a.Router.Clients() {
			if uid, _ := c.Identity(); uid != "" {
				n++
			}
		}
		return n == 2
	})

	// ALPHA signs off: its connection closes cleanly.
	alpha.conn.Close()
	waitFor(t, "ALPHA listed as signed off", func() bool { s, ok := offline("ALPHA"); return ok && s })
	// BRAVO goes silent: EdgeTAK's idle timeout (1 s here) cuts it.
	waitFor(t, "BRAVO's connection lost", func() bool { s, ok := offline("BRAVO"); return ok && !s })

	a.Web.CheckLostContact(time.Now()) // within the grace period: nothing yet
	if n := len(lostAlerts()); n != 0 {
		t.Fatalf("%d alerts within the grace period", n)
	}
	later := time.Now().Add(6 * time.Minute)
	a.Web.CheckLostContact(later)
	alerts := lostAlerts()
	if _, ok := alerts["ALPHA"]; ok {
		t.Error("ALPHA signed off but raised an alert")
	}
	b, ok := alerts["BRAVO"]
	if !ok || b.Type != "Lost contact" || b.Ended != nil || b.Lat != 38.8977 || b.Username != "bravo" {
		t.Fatalf("BRAVO's alert = %+v", b)
	}
	a.Web.CheckLostContact(later.Add(time.Minute))
	if n := len(lostAlerts()); n != 1 {
		t.Errorf("%d alerts after checking again, want 1", n)
	}

	// BRAVO is back: its alert ends by itself.
	bravo = mustDial(t, a, &bravoCert)
	bravo.send(sa("ANDROID-bravo", "BRAVO", "Cyan"))
	waitFor(t, "BRAVO back", func() bool {
		for _, c := range a.Router.Clients() {
			if uid, _ := c.Identity(); uid == "ANDROID-bravo" {
				return true
			}
		}
		return false
	})
	a.Web.CheckLostContact(time.Now())
	if _, off := offline("BRAVO"); off {
		t.Error("BRAVO still listed offline after coming back")
	}
	if b := lostAlerts()["BRAVO"]; b.Ended == nil || b.EndedBy != "device" {
		t.Errorf("BRAVO's alert after coming back = %+v", b)
	}

	// Connected without a position: with a long grace period, only the
	// position check fires.
	if err := a.Store.SetLostContact(store.LostContactSettings{GraceMinutes: 60, NoPositionMinutes: 10}); err != nil {
		t.Fatal(err)
	}
	charlieCert := enroll(t, a, "charlie", "password-charlie")
	charlie := mustDial(t, a, &charlieCert)
	charlie.send(sa("ANDROID-charlie", "CHARLIE", "Cyan"))
	waitFor(t, "CHARLIE identified", func() bool {
		for _, c := range a.Router.Clients() {
			if uid, _ := c.Identity(); uid == "ANDROID-charlie" {
				return true
			}
		}
		return false
	})
	// Only alerts for the Red group: CHARLIE (Cyan) is not watched.
	a.Store.SetLostContact(store.LostContactSettings{GraceMinutes: 60, NoPositionMinutes: 10, Groups: []string{"Red"}})
	a.Web.CheckLostContact(time.Now().Add(11 * time.Minute))
	if _, ok := lostAlerts()["CHARLIE"]; ok {
		t.Error("CHARLIE alerted though only Red is watched")
	}
	a.Store.SetLostContact(store.LostContactSettings{GraceMinutes: 60, NoPositionMinutes: 10})
	a.Web.CheckLostContact(time.Now().Add(11 * time.Minute))
	if c := lostAlerts()["CHARLIE"]; c.Type != "No position" || c.Ended != nil {
		t.Errorf("CHARLIE's alert = %+v", c)
	}
	charlie.send(sa("ANDROID-charlie", "CHARLIE", "Cyan"))
	time.Sleep(100 * time.Millisecond)
	a.Web.CheckLostContact(time.Now())
	if c := lostAlerts()["CHARLIE"]; c.Ended == nil {
		t.Errorf("CHARLIE's alert did not end with a new position: %+v", c)
	}

	// Turning lost contact alerts off ends the open ones and raises no more.
	openLost := func() []store.Emergency {
		var out []store.Emergency
		for _, e := range a.Store.Emergencies() {
			if e.Kind == "lost" && e.Ended == nil {
				out = append(out, e)
			}
		}
		return out
	}
	a.Web.CheckLostContact(time.Now().Add(11 * time.Minute))
	open := openLost()
	if len(open) == 0 {
		t.Fatal("no open alert before turning alerts off")
	}
	call, _ := adminJSON(t, a)
	if code, v := call("PUT", "/admin/api/lost-contact", map[string]any{"off": true, "grace_minutes": 60, "no_position_minutes": 10}); code != 200 {
		t.Fatalf("turn off = %d %v", code, v)
	}
	if len(openLost()) != 0 {
		t.Errorf("alerts still open after turning alerts off: %+v", openLost())
	}
	for _, e := range a.Store.Emergencies() {
		for _, o := range open {
			if e.ID == o.ID && e.EndedBy != "admin" {
				t.Errorf("%s ended by %q, want admin", e.Callsign, e.EndedBy)
			}
		}
	}
	a.Web.CheckLostContact(time.Now().Add(30 * time.Minute))
	if len(openLost()) != 0 {
		t.Errorf("an alert was raised while alerts are off: %+v", openLost())
	}
}
