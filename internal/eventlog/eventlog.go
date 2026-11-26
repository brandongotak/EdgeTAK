// Package eventlog records the CoT events that pass through the server, for
// searching by type, area and time, and exporting the results.
//
// Recording is off unless enabled. Events are appended to one JSON-lines
// file per UTC day and searched from disk, newest first; nothing is held in
// memory. A device's own position reports are thinned (one when it has
// moved, changed, or once a minute) and kept without their XML, since they
// make up most of the traffic. Days older than the keep period are deleted.
package eventlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
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
	filePrefix  = "events-"
	fileSuffix  = ".jsonl"
	dayLayout   = "2006-01-02"
	minMove     = 5.0 // metres moved before another position report is kept
	keepAlive   = time.Minute
	maxXML      = 64 << 10 // larger events are kept without their XML
	maxRemarks  = 1000
	pruneEvery  = time.Hour
	DefaultDays = 7
	MaxDays     = 365
)

// Entry is one recorded event. The short JSON names keep the files small.
type Entry struct {
	T            int64   `json:"t"` // Unix milliseconds, when the server saw it
	UID          string  `json:"u"`
	Type         string  `json:"y"`
	How          string  `json:"w,omitempty"`
	Callsign     string  `json:"c,omitempty"`
	FromUID      string  `json:"f,omitempty"`  // the device that sent it
	FromCallsign string  `json:"fc,omitempty"` // its callsign
	Username     string  `json:"n,omitempty"`  // its EdgeTAK account
	Source       string  `json:"s,omitempty"`  // device, federation, mesh, extension or server
	HasPos       bool    `json:"p,omitempty"`
	Lat          float64 `json:"la,omitempty"`
	Lon          float64 `json:"lo,omitempty"`
	Remarks      string  `json:"r,omitempty"`
	SA           bool    `json:"a,omitempty"` // the sending device's own position report
	XML          string  `json:"x,omitempty"`
}

// Query selects events. Zero values match everything.
type Query struct {
	Since, Until time.Time
	Types        []string    // type prefixes, any of them ("a-h", "b-t-f")
	Text         string      // case-insensitive, in the UID, callsigns, username, type or remarks
	BBox         *[4]float64 // west, south, east, north; only events with a position inside
	Positions    bool        // include devices' own position reports
	Limit        int         // newest first; 0 = all
}

func (q *Query) match(e *Entry) bool {
	if (!q.Since.IsZero() && e.T < q.Since.UnixMilli()) || (!q.Until.IsZero() && e.T > q.Until.UnixMilli()) {
		return false
	}
	if e.SA && !q.Positions {
		return false
	}
	if len(q.Types) > 0 && !slices.ContainsFunc(q.Types, func(p string) bool { return strings.HasPrefix(e.Type, p) }) {
		return false
	}
	if b := q.BBox; b != nil {
		if !e.HasPos || e.Lat < b[1] || e.Lat > b[3] {
			return false
		}
		if b[0] <= b[2] && (e.Lon < b[0] || e.Lon > b[2]) || b[0] > b[2] && e.Lon < b[0] && e.Lon > b[2] { // across the antimeridian
			return false
		}
	}
	if q.Text != "" {
		t := strings.ToLower(q.Text)
		for _, v := range []string{e.UID, e.Callsign, e.FromCallsign, e.FromUID, e.Username, e.Type, e.Remarks} {
			if strings.Contains(strings.ToLower(v), t) {
				return true
			}
		}
		return false
	}
	return true
}

type lastPos struct {
	t        int64
	lat, lon float64
	sig      uint64 // type, callsign and remarks
}

type Log struct {
	dir string
	log *slog.Logger

	mu       sync.Mutex
	enabled  bool
	keepDays int
	file     *os.File
	fileDay  string
	last     map[string]lastPos // by UID, for thinning position reports
	lastT    int64              // each entry gets its own time, so t identifies it
}

// Open opens the event log in dir. Recording starts disabled; call Configure
// with the saved settings.
func Open(dir string, log *slog.Logger) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Log{dir: dir, log: log, keepDays: DefaultDays, last: map[string]lastPos{}}, nil
}

// Configure turns recording on or off and sets how many days are kept.
func (l *Log) Configure(enabled bool, keepDays int) {
	if keepDays <= 0 {
		keepDays = DefaultDays
	}
	l.mu.Lock()
	l.enabled, l.keepDays = enabled, min(keepDays, MaxDays)
	if !enabled {
		l.closeFileLocked()
		l.last = map[string]lastPos{}
	}
	l.mu.Unlock()
	l.Prune(time.Now())
}

func (l *Log) Enabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enabled
}

// Record saves e if recording is on. Position reports (e.SA) are thinned.
func (l *Log) Record(e Entry) {
	if e.UID == "" || e.Type == "" {
		return
	}
	if e.HasPos && (math.IsNaN(e.Lat) || math.IsNaN(e.Lon) || math.Abs(e.Lat) > 90 || math.Abs(e.Lon) > 180 || e.Lat == 0 && e.Lon == 0) {
		e.HasPos, e.Lat, e.Lon = false, 0, 0
	}
	if len(e.Remarks) > maxRemarks {
		e.Remarks = e.Remarks[:maxRemarks]
	}
	if len(e.XML) > maxXML {
		e.XML = ""
	}
	if e.HasPos {
		e.Lat, e.Lon = math.Round(e.Lat*1e7)/1e7, math.Round(e.Lon*1e7)/1e7
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.enabled {
		return
	}
	if e.SA {
		h := fnv.New64a()
		h.Write([]byte(e.Type + "\x00" + e.Callsign + "\x00" + e.Remarks))
		sig := h.Sum64()
		if p, ok := l.last[e.UID]; ok && p.sig == sig && time.Duration(e.T-p.t)*time.Millisecond < keepAlive &&
			(!e.HasPos || distance(p.lat, p.lon, e.Lat, e.Lon) < minMove) {
			return
		}
		l.last[e.UID] = lastPos{t: e.T, lat: e.Lat, lon: e.Lon, sig: sig}
		e.XML = "" // the type, position and callsign say it all
	}
	if e.T <= l.lastT {
		e.T = l.lastT + 1
	}
	l.lastT = e.T
	if err := l.writeLocked(e); err != nil {
		l.log.Warn("event log write failed", "err", err)
	}
}

func (l *Log) writeLocked(e Entry) error {
	day := time.UnixMilli(e.T).UTC().Format(dayLayout)
	if l.file == nil || l.fileDay != day {
		l.closeFileLocked()
		f, err := os.OpenFile(filepath.Join(l.dir, filePrefix+day+fileSuffix), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		l.file, l.fileDay = f, day
	}
	b, _ := json.Marshal(e)
	_, err := l.file.Write(append(b, '\n'))
	return err
}

func (l *Log) closeFileLocked() {
	if l.file != nil {
		l.file.Close()
		l.file, l.fileDay = nil, ""
	}
}

// days lists the day files, oldest first.
func (l *Log) days() ([]string, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasPrefix(n, filePrefix) && strings.HasSuffix(n, fileSuffix) {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(n, filePrefix), fileSuffix))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Find returns the matching events, newest first, and how many matched in
// all (more than returned when Limit cut the list). withXML keeps each
// event's XML.
func (l *Log) Find(q Query, withXML bool) (out []Entry, total int, err error) {
	days, err := l.days()
	if err != nil {
		return nil, 0, err
	}
	out = []Entry{}
	saMarker := []byte(`"a":true`)
	for i := len(days) - 1; i >= 0; i-- {
		day, err := time.Parse(dayLayout, days[i])
		if err != nil || (!q.Until.IsZero() && day.After(q.Until)) || (!q.Since.IsZero() && !day.Add(24*time.Hour).After(q.Since)) {
			continue
		}
		var found []Entry
		err = l.scan(days[i], func(line []byte) {
			if !q.Positions && bytes.Contains(line, saMarker) {
				return
			}
			var e Entry
			if json.Unmarshal(line, &e) != nil || !q.match(&e) {
				return
			}
			if !withXML {
				e.XML = ""
			}
			found = append(found, e)
		})
		if err != nil {
			l.log.Warn("event log file skipped", "day", days[i], "err", err)
		}
		// Lines are in arrival order; newest first, stable for equal times.
		slices.Reverse(found)
		slices.SortStableFunc(found, func(a, b Entry) int { return int(b.T - a.T) })
		total += len(found)
		if q.Limit == 0 || len(out) < q.Limit {
			n := len(found)
			if q.Limit > 0 {
				n = min(n, q.Limit-len(out))
			}
			out = append(out, found[:n]...)
		}
	}
	return out, total, nil
}

// Event returns the event recorded at t (Unix ms) for uid, with its XML.
func (l *Log) Event(t int64, uid string) (Entry, bool) {
	day := time.UnixMilli(t).UTC().Format(dayLayout)
	var found Entry
	ok := false
	l.scan(day, func(line []byte) {
		if ok || !bytes.Contains(line, []byte(fmt.Sprintf(`"t":%d,`, t))) {
			return
		}
		var e Entry
		if json.Unmarshal(line, &e) == nil && e.T == t && e.UID == uid {
			found, ok = e, true
		}
	})
	return found, ok
}

func (l *Log) scan(day string, fn func(line []byte)) error {
	f, err := os.Open(filepath.Join(l.dir, filePrefix+day+fileSuffix))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 2*maxXML+64<<10)
	for sc.Scan() {
		fn(sc.Bytes()) // a torn last line after a crash fails to decode and is skipped
	}
	return sc.Err()
}

// Stats reports the size on disk and the first day held.
func (l *Log) Stats() (bytes int64, oldest string) {
	days, _ := l.days()
	for _, d := range days {
		if fi, err := os.Stat(filepath.Join(l.dir, filePrefix+d+fileSuffix)); err == nil {
			bytes += fi.Size()
		}
	}
	if len(days) > 0 {
		oldest = days[0]
	}
	return
}

// Prune deletes the days older than the keep period (whole UTC days).
func (l *Log) Prune(now time.Time) {
	l.mu.Lock()
	keep := l.keepDays
	l.mu.Unlock()
	cutoff := now.UTC().AddDate(0, 0, -keep).Format(dayLayout)
	days, _ := l.days()
	for _, d := range days {
		if d >= cutoff {
			continue
		}
		l.mu.Lock()
		if l.fileDay == d {
			l.closeFileLocked()
		}
		l.mu.Unlock()
		if err := os.Remove(filepath.Join(l.dir, filePrefix+d+fileSuffix)); err != nil {
			l.log.Warn("event log prune failed", "day", d, "err", err)
		}
	}
}

// Clear deletes every recorded event.
func (l *Log) Clear() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeFileLocked()
	l.last = map[string]lastPos{}
	days, err := l.days()
	if err != nil {
		return err
	}
	for _, d := range days {
		if err := os.Remove(filepath.Join(l.dir, filePrefix+d+fileSuffix)); err != nil {
			return fmt.Errorf("remove %s: %w", d, err)
		}
	}
	return nil
}

// Run prunes periodically until ctx is done, then closes the open file.
func (l *Log) Run(ctx context.Context) {
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			l.mu.Lock()
			l.closeFileLocked()
			l.mu.Unlock()
			return
		case now := <-t.C:
			l.Prune(now)
		}
	}
}

// distance is the great-circle distance in metres.
func distance(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := p2-p1, (lon2-lon1)*math.Pi/180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Sqrt(math.Min(1, a)))
}
