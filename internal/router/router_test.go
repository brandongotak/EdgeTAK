package router

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

func event(t *testing.T, xml string) *cot.Event {
	t.Helper()
	ev, err := cot.ParseXML([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

const tracker = `<event version="2.0" uid="LIGHTBUG.1" type="a-f-G-U-C" how="h-e" time="2026-10-26T00:00:00Z" start="2026-10-26T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="1" lon="2" hae="0" ce="9" le="9"/><detail><contact callsign="T1"/></detail></event>`
const deleteTracker = `<event version="2.0" uid="del-1" type="t-x-d-d" how="h-g-i-g-o" time="2026-10-26T00:00:00Z" start="2026-10-26T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="0" lon="0" hae="0" ce="9" le="9"/><detail><link relation="p-p" uid="LIGHTBUG.1" type="a-f-G-U-C"/></detail></event>`

// got returns the uids queued for c without waiting.
func got(c *Client) []string {
	var uids []string
	for {
		select {
		case ev := <-c.Queue():
			uids = append(uids, ev.UID)
		case <-time.After(20 * time.Millisecond):
			return uids
		}
	}
}

func TestTapGroupsAndReplay(t *testing.T) {
	r := New(slog.New(slog.NewTextHandler(io.Discard, nil)), true)
	tap := NewClient("extension", "extension", "extension:trackers", nil, []string{"A", "B"}, nil)
	r.AddTap(tap)
	a := NewClient("a", "tls", "a", []string{"A"}, []string{"A"}, nil)
	b := NewClient("b", "tls", "b", []string{"B"}, []string{"B"}, nil)
	r.Register(a)
	r.Register(b)

	r.RouteTo(tap, []string{"A"}, event(t, tracker))
	if g := got(a); len(g) != 1 {
		t.Errorf("A got %v", g)
	}
	if g := got(b); len(g) != 0 {
		t.Errorf("B got %v, but it was sent to A", g)
	}
	// Replayed to devices connecting later, in its groups only.
	a2 := NewClient("a2", "tls", "a2", []string{"A"}, nil, nil)
	b2 := NewClient("b2", "tls", "b2", []string{"B"}, nil, nil)
	r.Register(a2)
	r.Register(b2)
	if g := got(a2); len(g) != 1 || g[0] != "LIGHTBUG.1" {
		t.Errorf("late A got %v", g)
	}
	if g := got(b2); len(g) != 0 {
		t.Errorf("late B got %v", g)
	}

	// Moved to B: sent there, deleted from A.
	r.RouteTo(tap, []string{"B"}, event(t, tracker))
	r.RouteTo(tap, []string{"A"}, event(t, deleteTracker))
	got(a)
	got(b)
	a3 := NewClient("a3", "tls", "a3", []string{"A"}, nil, nil)
	b3 := NewClient("b3", "tls", "b3", []string{"B"}, nil, nil)
	r.Register(a3)
	r.Register(b3)
	if g := got(a3); len(g) != 0 {
		t.Errorf("A still replays a tracker deleted there: %v", g)
	}
	if g := got(b3); len(g) != 1 {
		t.Errorf("B doesn't replay the tracker moved there: %v", g)
	}

	// Groups the tap may not send to are ignored.
	r.RouteTo(tap, []string{"C"}, event(t, tracker))
	c := NewClient("c", "tls", "c", []string{"C"}, nil, nil)
	r.Register(c)
	if g := got(c); len(g) != 0 {
		t.Errorf("C got %v from a tap that can't send there", g)
	}

	// Taps aren't devices.
	if len(r.Clients()) != 7 {
		t.Errorf("%d clients", len(r.Clients()))
	}
	r.RemoveTap(tap)
	d := NewClient("d", "tls", "d", []string{"A", "B"}, nil, nil)
	r.Register(d)
	if g := got(d); len(g) != 0 {
		t.Errorf("a removed tap's trackers are still replayed: %v", g)
	}
}
