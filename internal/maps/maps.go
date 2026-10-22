// Package maps serves offline map tiles from MBTiles files, so the console
// and ATAK have maps without internet access.
//
// An MBTiles file is a SQLite database: a metadata table (name, format,
// bounds, zoom levels...) and a tiles table addressed by zoom, column and
// row, with rows counted from the south (TMS), so y is flipped for the
// usual XYZ scheme. Only raster tiles (PNG, JPEG, WebP) are served: vector
// tiles need a renderer the console and ATAK map sources don't have.
package maps

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go: builds without a C compiler
)

// Map is one MBTiles file.
type Map struct {
	File        string     `json:"file"` // file name in the maps folder, also its ID
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Attribution string     `json:"attribution,omitempty"`
	Format      string     `json:"format"`  // png, jpg or webp
	Overlay     bool       `json:"overlay"` // metadata type "overlay"
	MinZoom     int        `json:"min_zoom"`
	MaxZoom     int        `json:"max_zoom"`
	Bounds      [4]float64 `json:"bounds"` // west, south, east, north
	Size        int64      `json:"size"`
	Modified    time.Time  `json:"modified"`
	Error       string     `json:"error,omitempty"` // why it can't be served

	db *sql.DB
}

// ContentType is the tiles' MIME type.
func (m *Map) ContentType() string {
	switch m.Format {
	case "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	}
	return "image/png"
}

// ErrNoTile: the map has no tile there.
var ErrNoTile = errors.New("no tile")

// Tile returns the tile at z/x/y (XYZ, as web maps and ATAK count).
func (m *Map) Tile(z, x, y int) ([]byte, error) {
	if m.db == nil || z < 0 || z > 30 || x < 0 || y < 0 || x >= 1<<z || y >= 1<<z {
		return nil, ErrNoTile
	}
	var data []byte
	err := m.db.QueryRow(`SELECT tile_data FROM tiles WHERE zoom_level = ? AND tile_column = ? AND tile_row = ?`,
		z, x, (1<<z)-1-y).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoTile
	}
	return data, err
}

// Open reads an MBTiles file's metadata and keeps it open for tiles. A file
// that can't be served comes back with Error set.
func Open(path string) *Map {
	m := &Map{File: filepath.Base(path), Name: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))}
	if fi, err := os.Stat(path); err == nil {
		m.Size, m.Modified = fi.Size(), fi.ModTime()
	}
	abs, _ := filepath.Abs(path)
	// Read-only and immutable: no journal or lock files beside the map.
	dsn := "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?mode=ro&immutable=1"
	if filepath.VolumeName(abs) != "" {
		dsn = "file:///" + strings.TrimPrefix((&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath(), "/") + "?mode=ro&immutable=1"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		m.Error = "not a readable MBTiles file: " + err.Error()
		return m
	}
	db.SetMaxOpenConns(4)
	meta := map[string]string{}
	rows, err := db.Query(`SELECT name, value FROM metadata`)
	if err != nil {
		db.Close()
		m.Error = "not an MBTiles file (no metadata table)"
		return m
	}
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			meta[strings.ToLower(k)] = v
		}
	}
	rows.Close()
	if n := strings.TrimSpace(meta["name"]); n != "" {
		m.Name = n
	}
	m.Description, m.Attribution = meta["description"], meta["attribution"]
	m.Overlay = strings.EqualFold(meta["type"], "overlay")
	m.Format = strings.ToLower(strings.TrimSpace(meta["format"]))
	if m.Format == "jpeg" {
		m.Format = "jpg"
	}
	var zmin, zmax sql.NullInt64
	if err := db.QueryRow(`SELECT MIN(zoom_level), MAX(zoom_level) FROM tiles`).Scan(&zmin, &zmax); err != nil {
		db.Close()
		m.Error = "not an MBTiles file (no tiles table)"
		return m
	}
	if !zmin.Valid {
		db.Close()
		m.Error = "the file has no tiles"
		return m
	}
	m.MinZoom, m.MaxZoom = int(zmin.Int64), int(zmax.Int64)
	if m.Format == "" {
		m.Format = sniff(db)
	}
	switch m.Format {
	case "png", "jpg", "webp":
	case "pbf", "mvt":
		db.Close()
		m.Error = "vector tiles (" + m.Format + "): only raster maps (PNG, JPEG, WebP) can be shown"
		return m
	default:
		db.Close()
		m.Error = "unknown tile format " + strconv.Quote(m.Format)
		return m
	}
	m.Bounds = [4]float64{-180, -85.0511, 180, 85.0511}
	if b := strings.Split(meta["bounds"], ","); len(b) == 4 {
		var v [4]float64
		ok := true
		for i := range b {
			f, err := strconv.ParseFloat(strings.TrimSpace(b[i]), 64)
			ok = ok && err == nil
			v[i] = f
		}
		if ok {
			m.Bounds = v
		}
	}
	m.db = db
	return m
}

// sniff guesses the format from a tile's first bytes, for files whose
// metadata doesn't say.
func sniff(db *sql.DB) string {
	var b []byte
	if db.QueryRow(`SELECT tile_data FROM tiles LIMIT 1`).Scan(&b) != nil || len(b) < 12 {
		return ""
	}
	switch {
	case b[0] == 0x89 && string(b[1:4]) == "PNG":
		return "png"
	case b[0] == 0xff && b[1] == 0xd8:
		return "jpg"
	case string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp"
	case b[0] == 0x1f && b[1] == 0x8b:
		return "pbf" // gzipped vector tiles
	}
	return ""
}

// Close closes a map opened with Open.
func (m *Map) Close() { m.close() }

func (m *Map) close() {
	if m.db != nil {
		m.db.Close()
		m.db = nil
	}
}

// Library is the maps folder.
type Library struct {
	Dir string

	mu   sync.RWMutex
	maps map[string]*Map
}

// Scan (re)reads the folder: new files are opened, removed ones closed,
// changed ones reopened.
func (l *Library) Scan() error {
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".mbtiles") {
			continue
		}
		seen[e.Name()] = true
		fi, err := e.Info()
		if err != nil {
			continue
		}
		l.mu.RLock()
		old := l.maps[e.Name()]
		l.mu.RUnlock()
		if old != nil && old.Size == fi.Size() && old.Modified.Equal(fi.ModTime()) {
			continue
		}
		m := Open(filepath.Join(l.Dir, e.Name()))
		l.mu.Lock()
		if l.maps == nil {
			l.maps = map[string]*Map{}
		}
		if old := l.maps[e.Name()]; old != nil {
			old.close()
		}
		l.maps[e.Name()] = m
		l.mu.Unlock()
	}
	l.mu.Lock()
	for name, m := range l.maps {
		if !seen[name] {
			m.close()
			delete(l.maps, name)
		}
	}
	l.mu.Unlock()
	return nil
}

// Maps lists the maps by name.
func (l *Library) Maps() []*Map {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*Map, 0, len(l.maps))
	for _, m := range l.maps {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b *Map) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}

// Get returns a map by file name.
func (l *Library) Get(file string) (*Map, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	m, ok := l.maps[file]
	return m, ok
}

// Remove closes and deletes a map's file.
func (l *Library) Remove(file string) error {
	l.mu.Lock()
	m, ok := l.maps[file]
	if ok {
		m.close()
		delete(l.maps, file)
	}
	l.mu.Unlock()
	if !ok {
		return fmt.Errorf("no map %q", file)
	}
	return os.Remove(filepath.Join(l.Dir, file))
}

// Close closes every map.
func (l *Library) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.maps {
		m.close()
	}
	l.maps = nil
}
