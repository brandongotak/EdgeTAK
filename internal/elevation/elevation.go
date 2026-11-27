// Package elevation keeps DTED elevation tiles for ATAK: it files uploaded
// tiles under ATAK's own names (w078/n38.dt1), serves level 0 tiles the way
// ATAK streams them from tak.gov (/elevation/DTED/w078/n38.dt0.zip), and
// packs tiles into the zip ATAK imports as elevation data (a DTED folder at
// the root, no manifest).
//
// A tile's place and level come from its own header (the UHL record), not
// its file name, so tiles in any folder layout import correctly.
package elevation

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	maxTile   = 64 << 20 // a DTED level 2 tile is about 25 MB
	headerLen = 80
)

// Tile is one stored tile; Lat and Lon are its south-west corner.
type Tile struct {
	Level int    `json:"level"`
	Lat   int    `json:"lat"`
	Lon   int    `json:"lon"`
	Size  int64  `json:"size"`
	Path  string `json:"path"` // w078/n38.dt1
}

// Store is the tile folder (<data>/elevation/DTED).
type Store struct {
	Dir string
	mu  sync.Mutex
}

// RelPath is ATAK's name for a tile: RelPath(2, 30, -73) = "w073/n30.dt2".
func RelPath(level, lat, lon int) string {
	ew, ns := "e", "n"
	if lon < 0 {
		ew = "w"
	}
	if lat < 0 {
		ns = "s"
	}
	return fmt.Sprintf("%s%03d/%s%02d.dt%d", ew, abs(lon), ns, abs(lat), level)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

var relPathRE = regexp.MustCompile(`^([ew])(\d{3})/([ns])(\d{2})\.dt([0-2])$`)

// ParsePath reads a tile name; ok is false for anything else.
func ParsePath(rel string) (level, lat, lon int, ok bool) {
	m := relPathRE.FindStringSubmatch(rel)
	if m == nil {
		return 0, 0, 0, false
	}
	lon, _ = strconv.Atoi(m[2])
	lat, _ = strconv.Atoi(m[4])
	level, _ = strconv.Atoi(m[5])
	if m[1] == "w" {
		lon = -lon
	}
	if m[3] == "s" {
		lat = -lat
	}
	if lon < -180 || lon > 179 || lat < -90 || lat > 89 {
		return 0, 0, 0, false
	}
	return level, lat, lon, true
}

// parseAngle reads a DTED DDDMMSSH angle into whole degrees of its origin.
func parseAngle(s string) (int, error) {
	if len(s) != 8 {
		return 0, errors.New("bad angle")
	}
	deg, err1 := strconv.Atoi(strings.TrimSpace(s[0:3]))
	min, err2 := strconv.Atoi(s[3:5])
	sec, err3 := strconv.Atoi(s[5:7])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, errors.New("bad angle")
	}
	v := float64(deg) + float64(min)/60 + float64(sec)/3600
	switch s[7] {
	case 'S', 's', 'W', 'w':
		v = -v
	case 'N', 'n', 'E', 'e':
	default:
		return 0, errors.New("bad hemisphere")
	}
	// Origins are whole degrees; round off any seconds of slop.
	if v < 0 {
		return -int(-v + 0.5), nil
	}
	return int(v + 0.5), nil
}

// ParseHeader reads where a tile is and its level from its UHL record.
func ParseHeader(b []byte) (level, lat, lon int, err error) {
	if len(b) < headerLen || string(b[0:3]) != "UHL" {
		return 0, 0, 0, errors.New("not a DTED file (no UHL record)")
	}
	if lon, err = parseAngle(string(b[4:12])); err != nil {
		return 0, 0, 0, fmt.Errorf("DTED longitude: %w", err)
	}
	if lat, err = parseAngle(string(b[12:20])); err != nil {
		return 0, 0, 0, fmt.Errorf("DTED latitude: %w", err)
	}
	// The latitude interval, in tenths of an arc second, gives the level.
	switch strings.TrimSpace(string(b[24:28])) {
	case "0300":
		level = 0
	case "0030":
		level = 1
	case "0010":
		level = 2
	default:
		return 0, 0, 0, fmt.Errorf("unsupported DTED spacing %q", string(b[24:28]))
	}
	if lon < -180 || lon > 179 || lat < -90 || lat > 89 {
		return 0, 0, 0, errors.New("DTED origin out of range")
	}
	return level, lat, lon, nil
}

// Add stores one tile and returns its name.
func (s *Store) Add(data []byte) (string, error) {
	if len(data) > maxTile {
		return "", errors.New("too large for a DTED tile")
	}
	level, lat, lon, err := ParseHeader(data)
	if err != nil {
		return "", err
	}
	rel := RelPath(level, lat, lon)
	dst := filepath.Join(s.Dir, filepath.FromSlash(rel))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return "", err
	}
	return rel, os.Rename(tmp, dst)
}

// ImportResult says what an upload added.
type ImportResult struct {
	Added   int      `json:"added"`
	Skipped []string `json:"skipped"` // files that weren't DTED tiles, with why
}

func isTileName(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".dt0" || ext == ".dt1" || ext == ".dt2"
}

// Import adds a tile, or every tile in a zip, read from r (size bytes), so
// a large zip needn't fit in memory.
func (s *Store) Import(name string, r io.ReaderAt, size int64) ImportResult {
	res := ImportResult{Skipped: []string{}}
	add := func(n string, b []byte) {
		if _, err := s.Add(b); err != nil {
			res.Skipped = append(res.Skipped, path.Base(n)+": "+err.Error())
			return
		}
		res.Added++
	}
	if strings.EqualFold(path.Ext(name), ".zip") {
		zr, err := zip.NewReader(r, size)
		if err != nil {
			res.Skipped = append(res.Skipped, name+": not a zip")
			return res
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() || !isTileName(f.Name) {
				continue
			}
			if f.UncompressedSize64 > maxTile {
				res.Skipped = append(res.Skipped, path.Base(f.Name)+": too large")
				continue
			}
			rc, err := f.Open()
			if err != nil {
				res.Skipped = append(res.Skipped, path.Base(f.Name)+": "+err.Error())
				continue
			}
			b, err := io.ReadAll(io.LimitReader(rc, maxTile+1))
			rc.Close()
			if err != nil {
				res.Skipped = append(res.Skipped, path.Base(f.Name)+": "+err.Error())
				continue
			}
			add(f.Name, b)
		}
		if res.Added == 0 && len(res.Skipped) == 0 {
			res.Skipped = append(res.Skipped, name+": no .dt0, .dt1 or .dt2 files inside")
		}
		return res
	}
	if !isTileName(name) {
		res.Skipped = append(res.Skipped, name+": not a .dt0, .dt1, .dt2 or .zip file")
		return res
	}
	if size > maxTile {
		res.Skipped = append(res.Skipped, name+": too large for a DTED tile")
		return res
	}
	data := make([]byte, size)
	if _, err := r.ReadAt(data, 0); err != nil && err != io.EOF {
		res.Skipped = append(res.Skipped, name+": "+err.Error())
		return res
	}
	add(name, data)
	return res
}

// Tiles lists the stored tiles.
func (s *Store) Tiles() []Tile {
	out := []Tile{}
	fs.WalkDir(os.DirFS(s.Dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		level, lat, lon, ok := ParsePath(p)
		if !ok {
			return nil
		}
		t := Tile{Level: level, Lat: lat, Lon: lon, Path: p}
		if fi, err := d.Info(); err == nil {
			t.Size = fi.Size()
		}
		out = append(out, t)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Delete removes one tile.
func (s *Store) Delete(rel string) error {
	if _, _, _, ok := ParsePath(rel); !ok {
		return errors.New("not a tile name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(filepath.Join(s.Dir, filepath.FromSlash(rel)))
	os.Remove(filepath.Dir(filepath.Join(s.Dir, filepath.FromSlash(rel)))) // the folder, once empty
	return err
}

// DeleteLevel removes every tile of a level and reports how many.
func (s *Store) DeleteLevel(level int) int {
	n := 0
	for _, t := range s.Tiles() {
		if t.Level == level && s.Delete(t.Path) == nil {
			n++
		}
	}
	return n
}

// StreamZip writes the zip ATAK downloads for one tile: the tile alone,
// which ATAK unzips next to the zip (w078/n38.dt0.zip holds n38.dt0).
func (s *Store) StreamZip(w io.Writer, rel string) error {
	if _, _, _, ok := ParsePath(rel); !ok {
		return fs.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	f, err := zw.Create(path.Base(rel))
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return zw.Close()
}

// Package writes the zip ATAK imports as elevation data: DTED/<tile> for
// each of tiles.
func (s *Store) Package(w io.Writer, tiles []Tile) error {
	zw := zip.NewWriter(w)
	for _, t := range tiles {
		data, err := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(t.Path)))
		if err != nil {
			continue
		}
		f, err := zw.CreateHeader(&zip.FileHeader{Name: "DTED/" + t.Path, Method: zip.Deflate})
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			return err
		}
	}
	return zw.Close()
}
