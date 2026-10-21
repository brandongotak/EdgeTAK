// Package history records where devices have been, for breadcrumb trails.
//
// Recording is off unless enabled. Positions are thinned (a point when a
// device has moved, or once a minute while it stands still), kept in memory
// for queries, and appended to one JSON-lines file per UTC day so trails
// survive restarts. Days older than the retention period are deleted.
package history

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	minMove      = 5.0 // metres moved before another point is kept
	minInterval  = 3 * time.Second
	keepAlive    = time.Minute // a point at least this often while stationary
	maxPerTrack  = 200_000
	filePrefix   = "history-"
	fileSuffix   = ".jsonl"
	pruneEvery   = 10 * time.Minute
	defaultHours = 24
)

// Point is one recorded position; T is Unix milliseconds.
type Point struct {
	T      int64   `json:"t"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Speed  float64 `json:"speed,omitempty"`  // m/s
	Course float64 `json:"course,omitempty"` // degrees
}

// Track is the recorded path of one device.
type Track struct {
	UID      string  `json:"uid"`
	Callsign string  `json:"callsign"`
	Team     string  `json:"team,omitempty"`
	Points   []Point `json:"points"`
}

// line is the on-disk form of a point.
type line struct {
	UID      string  `json:"u"`
	Callsign string  `json:"c,omitempty"`
	Team     string  `json:"g,omitempty"`
	T        int64   `json:"t"`
	Lat      float64 `json:"la"`
	Lon      float64 `json:"lo"`
	Speed    float64 `json:"s,omitempty"`
	Course   float64 `json:"h,omitempty"`
}

type Recorder struct {
	dir string
	log *slog.Logger

	mu        sync.Mutex
	enabled   bool
	retention time.Duration
	tracks    map[string]*Track
	file      *os.File
	fileDay   string
}

// Open loads the recorded history in dir. Recording starts disabled; call
// Configure with the saved settings.
func Open(dir string, log *slog.Logger) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := &Recorder{dir: dir, log: log, retention: defaultHours * time.Hour, tracks: map[string]*Track{}}
	files, err := r.dayFiles()
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := r.load(filepath.Join(dir, f)); err != nil {
			log.Warn("history file skipped", "file", f, "err", err)
		}
	}
	return r, nil
}

func (r *Recorder) dayFiles() ([]string, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasPrefix(n, filePrefix) && strings.HasSuffix(n, fileSuffix) {
			out = append(out, n)
		}
	}
	sort.Strings(out) // names sort by date
	return out, nil
}

func (r *Recorder) load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.UID == "" {
			continue // a torn last line after a crash
		}
		r.appendLocked(l)
	}
	return sc.Err()
}

func (r *Recorder) appendLocked(l line) {
	t := r.tracks[l.UID]
	if t == nil {
		t = &Track{UID: l.UID}
		r.tracks[l.UID] = t
	}
	if l.Callsign != "" {
		t.Callsign = l.Callsign
	}
	if l.Team != "" {
		t.Team = l.Team
	}
	if len(t.Points) >= maxPerTrack {
		t.Points = slices.Delete(t.Points, 0, maxPerTrack/10)
	}
	t.Points = append(t.Points, Point{T: l.T, Lat: l.Lat, Lon: l.Lon, Speed: l.Speed, Course: l.Course})
}

// Configure turns recording on or off and sets how long history is kept.
func (r *Recorder) Configure(enabled bool, retention time.Duration) {
	if retention <= 0 {
		retention = defaultHours * time.Hour
	}
	r.mu.Lock()
	r.enabled, r.retention = enabled, retention
	if !enabled {
		r.closeFileLocked()
	}
	r.mu.Unlock()
	r.Prune(time.Now())
}

func (r *Recorder) Enabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enabled
}

// Record adds a position report if recording is on and the device moved
// enough (or enough time passed) since its last point.
func (r *Recorder) Record(uid, callsign, team string, at time.Time, lat, lon, speed, course float64) {
	if uid == "" || (lat == 0 && lon == 0) || math.IsNaN(lat) || math.IsNaN(lon) || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
		return
	}
	if math.IsNaN(speed) || speed < 0 {
		speed = 0
	}
	if math.IsNaN(course) || course < 0 {
		course = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled {
		return
	}
	ms := at.UnixMilli()
	if t := r.tracks[uid]; t != nil && len(t.Points) > 0 {
		last := t.Points[len(t.Points)-1]
		dt := time.Duration(ms-last.T) * time.Millisecond
		if dt < minInterval || (dt < keepAlive && distance(last.Lat, last.Lon, lat, lon) < minMove) {
			return
		}
	}
	l := line{UID: uid, Callsign: callsign, Team: team, T: ms, Lat: round(lat, 7), Lon: round(lon, 7), Speed: round(speed, 2), Course: round(course, 1)}
	r.appendLocked(l)
	if err := r.writeLocked(l, at); err != nil {
		r.log.Warn("history write failed", "err", err)
	}
}

func (r *Recorder) writeLocked(l line, at time.Time) error {
	day := at.UTC().Format("2006-01-02")
	if r.file == nil || r.fileDay != day {
		r.closeFileLocked()
		f, err := os.OpenFile(filepath.Join(r.dir, filePrefix+day+fileSuffix), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		r.file, r.fileDay = f, day
	}
	b, _ := json.Marshal(l)
	_, err := r.file.Write(append(b, '\n'))
	return err
}

func (r *Recorder) closeFileLocked() {
	if r.file != nil {
		r.file.Close()
		r.file, r.fileDay = nil, ""
	}
}

// Tracks returns each device's points between since and until (inclusive),
// optionally for one UID. Tracks without points in the window are left out.
func (r *Recorder) Tracks(since, until time.Time, uid string) []Track {
	from, to := since.UnixMilli(), until.UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Track{}
	for id, t := range r.tracks {
		if uid != "" && id != uid {
			continue
		}
		i := sort.Search(len(t.Points), func(i int) bool { return t.Points[i].T >= from })
		j := sort.Search(len(t.Points), func(j int) bool { return t.Points[j].T > to })
		if i >= j {
			continue
		}
		out = append(out, Track{UID: t.UID, Callsign: t.Callsign, Team: t.Team, Points: slices.Clone(t.Points[i:j])})
	}
	slices.SortFunc(out, func(a, b Track) int { return strings.Compare(a.Callsign+a.UID, b.Callsign+b.UID) })
	return out
}

// Stats reports how much history is held.
func (r *Recorder) Stats() (tracks, points int, bytes int64) {
	r.mu.Lock()
	for _, t := range r.tracks {
		if len(t.Points) > 0 {
			tracks++
			points += len(t.Points)
		}
	}
	r.mu.Unlock()
	files, _ := r.dayFiles()
	for _, f := range files {
		if fi, err := os.Stat(filepath.Join(r.dir, f)); err == nil {
			bytes += fi.Size()
		}
	}
	return
}

// Prune drops points older than the retention period and deletes day files
// that only hold such points.
func (r *Recorder) Prune(now time.Time) {
	r.mu.Lock()
	cutoff := now.Add(-r.retention)
	from := cutoff.UnixMilli()
	for id, t := range r.tracks {
		i := sort.Search(len(t.Points), func(i int) bool { return t.Points[i].T >= from })
		t.Points = slices.Delete(t.Points, 0, i)
		if len(t.Points) == 0 {
			delete(r.tracks, id)
		}
	}
	r.mu.Unlock()
	// A file for day D holds points up to the end of D.
	files, _ := r.dayFiles()
	for _, f := range files {
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(strings.TrimPrefix(f, filePrefix), fileSuffix))
		if err != nil || !day.Add(24*time.Hour).Before(cutoff) {
			continue
		}
		r.mu.Lock()
		if r.fileDay == day.Format("2006-01-02") {
			r.closeFileLocked()
		}
		r.mu.Unlock()
		if err := os.Remove(filepath.Join(r.dir, f)); err != nil {
			r.log.Warn("history prune failed", "file", f, "err", err)
		}
	}
}

// Clear deletes all recorded history.
func (r *Recorder) Clear() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeFileLocked()
	r.tracks = map[string]*Track{}
	files, err := r.dayFiles()
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := os.Remove(filepath.Join(r.dir, f)); err != nil {
			return fmt.Errorf("remove %s: %w", f, err)
		}
	}
	return nil
}

// Run prunes periodically until ctx is done, then closes the open file.
func (r *Recorder) Run(ctx context.Context) {
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.closeFileLocked()
			r.mu.Unlock()
			return
		case now := <-t.C:
			r.Prune(now)
		}
	}
}

// distance is the great-circle distance in metres.
func distance(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := (lat2-lat1)*math.Pi/180, (lon2-lon1)*math.Pi/180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * R * math.Asin(math.Sqrt(a))
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
