package maps

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

var pngHeader = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}

// MakeMBTiles writes an MBTiles file with the given metadata and tiles
// (keyed "z/x/y" in XYZ numbering).
func makeMBTiles(t *testing.T, path string, meta map[string]string, tiles map[[3]int][]byte) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE metadata (name TEXT, value TEXT)`,
		`CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range meta {
		db.Exec(`INSERT INTO metadata VALUES (?, ?)`, k, v)
	}
	for k, v := range tiles {
		z, x, y := k[0], k[1], k[2]
		if _, err := db.Exec(`INSERT INTO tiles VALUES (?, ?, ?, ?)`, z, x, (1<<z)-1-y, v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLibrary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "maps with spaces")
	os.MkdirAll(dir, 0o755)
	tile := append(append([]byte{}, pngHeader...), 'A')
	makeMBTiles(t, filepath.Join(dir, "area.mbtiles"), map[string]string{"name": "Training area", "format": "png", "bounds": "-105.5,39.5,-105.0,40.0", "type": "overlay"},
		map[[3]int][]byte{{3, 1, 2}: tile, {5, 6, 12}: tile})
	makeMBTiles(t, filepath.Join(dir, "noformat.mbtiles"), nil, map[[3]int][]byte{{1, 0, 0}: tile})
	makeMBTiles(t, filepath.Join(dir, "vector.mbtiles"), map[string]string{"format": "pbf"}, map[[3]int][]byte{{1, 0, 0}: {0x1f, 0x8b}})
	os.WriteFile(filepath.Join(dir, "broken.mbtiles"), []byte("not sqlite"), 0o644)
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("ignored"), 0o644)

	l := &Library{Dir: dir}
	if err := l.Scan(); err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if n := len(l.Maps()); n != 4 {
		t.Fatalf("%d maps, want 4", n)
	}
	m, _ := l.Get("area.mbtiles")
	if m.Error != "" || m.Name != "Training area" || !m.Overlay || m.MinZoom != 3 || m.MaxZoom != 5 || m.Bounds[0] != -105.5 || m.ContentType() != "image/png" {
		t.Errorf("area = %+v", m)
	}
	if got, err := m.Tile(3, 1, 2); err != nil || string(got) != string(tile) {
		t.Errorf("tile 3/1/2 = %q %v", got, err)
	}
	if _, err := m.Tile(3, 1, 5); !errors.Is(err, ErrNoTile) {
		t.Errorf("missing tile: %v", err)
	}
	if _, err := m.Tile(3, 9, 0); !errors.Is(err, ErrNoTile) {
		t.Errorf("out of range tile: %v", err)
	}
	if m, _ := l.Get("noformat.mbtiles"); m.Error != "" || m.Format != "png" || m.Name != "noformat" {
		t.Errorf("format from the tiles: %+v", m)
	}
	if m, _ := l.Get("vector.mbtiles"); m.Error == "" {
		t.Error("vector tiles accepted")
	}
	if m, _ := l.Get("broken.mbtiles"); m.Error == "" {
		t.Error("broken file accepted")
	}
	// No journal or lock files are left beside the maps.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 5 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("files in the maps folder: %v", names)
	}

	if err := l.Remove("vector.mbtiles"); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "broken.mbtiles"))
	l.Scan()
	if n := len(l.Maps()); n != 2 {
		t.Errorf("after removing: %d maps", n)
	}
}
