package eventlog

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordFindPrune(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	ms := func(d time.Duration) int64 { return base.Add(d).UnixMilli() }

	l.Record(Entry{T: ms(0), UID: "x", Type: "a-h-G"}) // off: not kept
	l.Configure(true, 3)

	// A device's own reports: thinned while it stands still.
	sa := func(d time.Duration, lat float64) Entry {
		return Entry{T: ms(d), UID: "ANDROID-1", Type: "a-f-G-U-C", Callsign: "ALPHA", FromUID: "ANDROID-1", SA: true, HasPos: true, Lat: lat, Lon: -77, XML: "<event/>"}
	}
	l.Record(sa(0, 38))
	l.Record(sa(10*time.Second, 38))     // same place: dropped
	l.Record(sa(20*time.Second, 38.001)) // moved ~110 m: kept
	l.Record(Entry{T: ms(time.Minute), UID: "hostile-1", Type: "a-h-G", Callsign: "Enemy truck", HasPos: true, Lat: 38.5, Lon: -77.5, Remarks: "Moving north", XML: "<event uid='hostile-1'/>"})
	l.Record(sa(90*time.Second, 38.001)) // a minute on: kept
	l.Record(Entry{T: ms(2 * time.Minute), UID: "GeoChat.1", Type: "b-t-f", Remarks: "hello", XML: "<event/>"})
	l.Record(Entry{T: ms(25 * time.Hour), UID: "spot-1", Type: "b-m-p-s-m", HasPos: true, Lat: 10, Lon: 179.5})

	find := func(q Query) []Entry {
		t.Helper()
		out, total, err := l.Find(q, false)
		if err != nil {
			t.Fatal(err)
		}
		if q.Limit == 0 && total != len(out) {
			t.Fatalf("total %d, returned %d", total, len(out))
		}
		return out
	}
	if got := find(Query{}); len(got) != 3 || got[0].UID != "spot-1" || got[2].UID != "hostile-1" {
		t.Fatalf("without positions = %+v", got)
	}
	if got := find(Query{Positions: true}); len(got) != 6 {
		t.Fatalf("with positions = %d", len(got))
	}
	for name, c := range map[string]struct {
		q    Query
		want int
	}{
		"type":         {Query{Types: []string{"a-h"}}, 1},
		"two types":    {Query{Types: []string{"a-h", "b-t-f"}}, 2},
		"text":         {Query{Text: "NORTH"}, 1},
		"callsign":     {Query{Text: "alpha", Positions: true}, 3},
		"since":        {Query{Since: base.Add(90 * time.Second)}, 2},
		"until":        {Query{Until: base.Add(90 * time.Second)}, 1},
		"bbox":         {Query{BBox: &[4]float64{-78, 38.4, -77, 38.6}}, 1},
		"bbox no pos":  {Query{BBox: &[4]float64{-180, -90, 180, 90}, Types: []string{"b-t-f"}}, 0},
		"antimeridian": {Query{BBox: &[4]float64{179, 9, -179, 11}}, 1},
		"limit":        {Query{Limit: 2}, 2},
	} {
		if got := find(c.q); len(got) != c.want {
			t.Errorf("%s: %d events, want %d", name, len(got), c.want)
		}
	}
	if _, total, _ := l.Find(Query{Limit: 1}, false); total != 3 {
		t.Errorf("total with a limit = %d", total)
	}
	// XML is kept for events, not for position reports, and only when asked.
	if got := find(Query{Types: []string{"a-h"}}); got[0].XML != "" {
		t.Error("XML returned without asking")
	}
	if got, _, _ := l.Find(Query{Positions: true, Text: "ALPHA"}, true); got[0].XML != "" {
		t.Error("a position report kept its XML")
	}
	if e, ok := l.Event(ms(time.Minute), "hostile-1"); !ok || e.XML != "<event uid='hostile-1'/>" {
		t.Errorf("event = %+v %v", e, ok)
	}

	// Pruning removes whole days older than the keep period.
	l.Prune(base.AddDate(0, 0, 4))
	if got := find(Query{Positions: true}); len(got) != 1 || got[0].UID != "spot-1" {
		t.Errorf("after prune = %+v", got)
	}
	if err := l.Clear(); err != nil {
		t.Fatal(err)
	}
	if files, _ := os.ReadDir(dir); len(files) != 0 {
		t.Errorf("files left after clear: %v", files)
	}
	// A torn line is skipped.
	os.WriteFile(filepath.Join(dir, "events-2026-11-25.jsonl"), []byte(`{"t":1,"u":"a","y":"a-f"}`+"\n"+`{"t":2,"u":`), 0o600)
	if got := find(Query{}); len(got) != 1 {
		t.Errorf("with a torn line = %d", len(got))
	}
}
