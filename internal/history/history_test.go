package history

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecorder(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := Open(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour)

	// Off by default: nothing is recorded.
	r.Record("u1", "ALPHA", "Cyan", t0, 38.9, -77.0, 0, 0)
	if _, points, _ := r.Stats(); points != 0 {
		t.Fatalf("recorded while disabled: %d", points)
	}

	r.Configure(true, 24*time.Hour)
	r.Record("u1", "ALPHA", "Cyan", t0, 38.9, -77.0, 1.5, 90)
	r.Record("u1", "ALPHA", "Cyan", t0.Add(time.Second), 38.9001, -77.0, 0, 0)     // too soon
	r.Record("u1", "ALPHA", "Cyan", t0.Add(10*time.Second), 38.90001, -77.0, 0, 0) // moved ~1 m
	r.Record("u1", "ALPHA", "Cyan", t0.Add(20*time.Second), 38.9010, -77.0, 0, 0)  // moved ~111 m
	r.Record("u1", "ALPHA", "Cyan", t0.Add(90*time.Second), 38.9010, -77.0, 0, 0)  // stationary keep-alive
	r.Record("u2", "BRAVO", "Red", t0, 0, 0, 0, 0)                                 // no fix
	r.Record("u2", "BRAVO", "Red", t0, 38.8, -77.1, 0, 0)

	tracks := r.Tracks(t0.Add(-time.Minute), time.Now(), "")
	if len(tracks) != 2 || tracks[0].Callsign != "ALPHA" || len(tracks[0].Points) != 3 || tracks[1].Team != "Red" {
		t.Fatalf("tracks = %+v", tracks)
	}
	if p := tracks[0].Points[0]; p.Speed != 1.5 || p.Course != 90 {
		t.Fatalf("first point = %+v", p)
	}
	if got := r.Tracks(t0.Add(15*time.Second), time.Now(), "u1"); len(got) != 1 || len(got[0].Points) != 2 {
		t.Fatalf("windowed track = %+v", got)
	}

	// History survives a restart.
	r.Configure(false, 24*time.Hour)
	r2, err := Open(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	r2.Configure(true, 24*time.Hour)
	if _, points, bytes := r2.Stats(); points != 4 || bytes == 0 {
		t.Fatalf("after reload: %d points, %d bytes", points, bytes)
	}

	// Pruning drops old points and old day files.
	old := filepath.Join(dir, filePrefix+"2000-01-01"+fileSuffix)
	os.WriteFile(old, []byte(`{"u":"u9","t":946684800000,"la":1,"lo":1}`+"\n"), 0o600)
	r2.Prune(time.Now().Add(23*time.Hour + 30*time.Minute)) // cutoff just after t0
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old day file was not deleted")
	}
	if got := r2.Tracks(time.Time{}, time.Now(), "u2"); len(got) != 0 {
		t.Fatalf("u2 should be pruned: %+v", got)
	}

	if err := r2.Clear(); err != nil {
		t.Fatal(err)
	}
	if tracks, points, bytes := r2.Stats(); tracks+points != 0 || bytes != 0 {
		t.Fatalf("after clear: %d tracks, %d points, %d bytes", tracks, points, bytes)
	}
}
