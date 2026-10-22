package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// TestScheduledSends covers scheduled sends: a daily message reaches its
// group's devices when due (and not others), moves to the next day, can
// be sent now, and a run missed while EdgeTAK was off is skipped.
func TestScheduledSends(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct{ name, group string }{{"alpha", "Cyan"}, {"red1", "Red"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}
	call, _ := adminJSON(t, a)
	alphaCert, redCert := enroll(t, a, "alpha", "password-alpha"), enroll(t, a, "red1", "password-red1")
	alpha, red := mustDial(t, a, &alphaCert), mustDial(t, a, &redCert)
	brief := func(e *cot.Event) bool { return e.Type == "b-t-f" && strings.Contains(e.Detail.XML, "Morning brief") }

	if code, _ := call("POST", "/admin/api/schedules", map[string]any{"name": "x", "kind": "message", "text": "hi", "repeat": "once",
		"groups": []string{"Cyan"}, "start": time.Now().Add(-time.Hour)}); code != 400 {
		t.Errorf("once, in the past = %d", code)
	}
	start := time.Now().Add(time.Minute).Truncate(time.Minute)
	code, v := call("POST", "/admin/api/schedules", map[string]any{"name": "0700 brief", "kind": "message", "text": "Morning brief: weather is clear",
		"from": "Ops", "repeat": "daily", "groups": []string{"Cyan"}, "start": start})
	if code != 200 {
		t.Fatalf("create = %d %v", code, v)
	}
	c := a.Store.Schedules()[0]
	if !c.Next.Equal(start) {
		t.Fatalf("next = %v, want %v", c.Next, start)
	}

	a.Web.RunDueSchedules(start.Add(-time.Second))
	alpha.expectNone(brief, "the brief before its time")
	a.Web.RunDueSchedules(start.Add(5 * time.Second))
	if ev := alpha.expect(brief, "the brief at ALPHA"); !strings.Contains(ev.Detail.XML, `senderCallsign="Ops"`) {
		t.Errorf("brief = %s", ev.Detail.XML)
	}
	red.expectNone(brief, "the brief at RED1 (not Cyan)")
	c = a.Store.Schedules()[0]
	if c.Runs != 1 || !c.Next.Equal(start.AddDate(0, 0, 1)) || !strings.Contains(c.LastResult, "sent to 1 device") {
		t.Errorf("after the run = %+v", c)
	}

	// Send now: delivered, timetable unchanged.
	if code, v := call("POST", "/admin/api/schedules/"+c.ID+"/run", nil); code != 200 {
		t.Fatalf("run now = %d %v", code, v)
	}
	alpha.expect(brief, "the brief sent now")
	if c2 := a.Store.Schedules()[0]; !c2.Next.Equal(c.Next) || c2.Runs != 2 {
		t.Errorf("after send now = %+v", c2)
	}

	// EdgeTAK was off at the next run: skipped, not sent late.
	a.Web.RunDueSchedules(start.AddDate(0, 0, 1).Add(time.Hour))
	alpha.expectNone(brief, "a missed brief sent late")
	if c3 := a.Store.Schedules()[0]; !strings.HasPrefix(c3.LastResult, "missed") || !c3.Next.Equal(start.AddDate(0, 0, 2)) {
		t.Errorf("after a missed run = %+v", c3)
	}
}
