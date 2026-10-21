// Package chatlog records GeoChat messages for search, export and
// after-action reviews.
//
// Recording is off unless enabled. Messages are kept in memory for queries
// and appended to one JSON-lines file per UTC day so they survive restarts.
package chatlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	filePrefix = "chat-"
	fileSuffix = ".jsonl"
	dayLayout  = "2006-01-02"
	maxKept    = 1_000_000 // oldest messages are dropped from memory beyond this
)

// Message is one chat message as it passed through the server.
type Message struct {
	ID           string    `json:"id"` // GeoChat messageId (or the event UID)
	Time         time.Time `json:"time"`
	FromUID      string    `json:"from_uid"`
	FromCallsign string    `json:"from_callsign"`
	Username     string    `json:"username,omitempty"` // the device's EdgeTAK account, or the admin for console messages
	Kind         string    `json:"kind"`               // all, team, group, direct
	Room         string    `json:"room"`               // conversation name
	RoomID       string    `json:"room_id"`            // stable conversation key
	To           []string  `json:"to,omitempty"`       // other members' UIDs, as the sender listed them
	Text         string    `json:"text"`
	Lat          float64   `json:"lat,omitempty"`
	Lon          float64   `json:"lon,omitempty"`
	Console      bool      `json:"console,omitempty"` // sent from the admin console
}

// Query selects messages. Zero values match everything.
type Query struct {
	Since, Until time.Time
	RoomID       string
	FromUID      string
	Text         string // case-insensitive, in the text, sender or room
	Limit        int    // newest first; 0 = all
}

// Room summarises one conversation.
type Room struct {
	RoomID   string    `json:"room_id"`
	Room     string    `json:"room"`
	Kind     string    `json:"kind"`
	Messages int       `json:"messages"`
	Last     time.Time `json:"last"`
}

// Sender is someone who has sent messages.
type Sender struct {
	UID      string `json:"uid"`
	Callsign string `json:"callsign"`
	Messages int    `json:"messages"`
}

type Log struct {
	dir string
	log *slog.Logger

	mu      sync.Mutex
	enabled bool
	msgs    []Message // by time
	seen    map[string]bool
	file    *os.File
	fileDay string
}

// Open loads the chat log in dir. Recording starts disabled; call
// SetEnabled with the saved setting.
func Open(dir string, log *slog.Logger) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Log{dir: dir, log: log, seen: map[string]bool{}}
	files, err := l.dayFiles()
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := l.load(filepath.Join(dir, f)); err != nil {
			log.Warn("chat log file skipped", "file", f, "err", err)
		}
	}
	sort.SliceStable(l.msgs, func(i, j int) bool { return l.msgs[i].Time.Before(l.msgs[j].Time) })
	return l, nil
}

func (l *Log) dayFiles() ([]string, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasPrefix(n, filePrefix) && strings.HasSuffix(n, fileSuffix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (l *Log) load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == "" || l.seen[m.ID] {
			continue // a torn last line after a crash, or a duplicate
		}
		l.seen[m.ID] = true
		l.msgs = append(l.msgs, m)
	}
	return sc.Err()
}

func (l *Log) SetEnabled(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enabled = on
	if !on {
		l.closeFileLocked()
	}
}

func (l *Log) Enabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enabled
}

// Record saves m if recording is on. A message seen before (the same
// GeoChat message ID) is ignored.
func (l *Log) Record(m Message) {
	if m.ID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.enabled || l.seen[m.ID] {
		return
	}
	l.seen[m.ID] = true
	if len(l.msgs) >= maxKept {
		for _, old := range l.msgs[:maxKept/10] {
			delete(l.seen, old.ID)
		}
		l.msgs = slices.Delete(l.msgs, 0, maxKept/10)
	}
	// Messages arrive in order; a clock step back keeps the slice sorted anyway.
	i := len(l.msgs)
	for i > 0 && l.msgs[i-1].Time.After(m.Time) {
		i--
	}
	l.msgs = slices.Insert(l.msgs, i, m)
	if err := l.writeLocked(m); err != nil {
		l.log.Warn("chat log write failed", "err", err)
	}
}

func (l *Log) writeLocked(m Message) error {
	day := m.Time.UTC().Format(dayLayout)
	if l.file == nil || l.fileDay != day {
		l.closeFileLocked()
		f, err := os.OpenFile(filepath.Join(l.dir, filePrefix+day+fileSuffix), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		l.file, l.fileDay = f, day
	}
	b, _ := json.Marshal(m)
	_, err := l.file.Write(append(b, '\n'))
	return err
}

func (l *Log) closeFileLocked() {
	if l.file != nil {
		l.file.Close()
		l.file, l.fileDay = nil, ""
	}
}

func (q Query) match(m *Message) bool {
	if (!q.Since.IsZero() && m.Time.Before(q.Since)) || (!q.Until.IsZero() && m.Time.After(q.Until)) {
		return false
	}
	if (q.RoomID != "" && m.RoomID != q.RoomID) || (q.FromUID != "" && m.FromUID != q.FromUID) {
		return false
	}
	if q.Text != "" {
		t := strings.ToLower(q.Text)
		return strings.Contains(strings.ToLower(m.Text), t) || strings.Contains(strings.ToLower(m.FromCallsign), t) ||
			strings.Contains(strings.ToLower(m.Room), t) || strings.Contains(strings.ToLower(m.Username), t)
	}
	return true
}

// Find returns the matching messages, newest first, and how many matched
// in all (more than returned when Limit cut the list).
func (l *Log) Find(q Query) (msgs []Message, total int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msgs = []Message{}
	for i := len(l.msgs) - 1; i >= 0; i-- {
		if !q.match(&l.msgs[i]) {
			continue
		}
		total++
		if q.Limit == 0 || len(msgs) < q.Limit {
			m := l.msgs[i]
			m.To = slices.Clone(m.To)
			msgs = append(msgs, m)
		}
	}
	return msgs, total
}

// Rooms lists conversations, most recent first, and senders by name.
func (l *Log) Rooms() ([]Room, []Sender) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rooms, senders := map[string]*Room{}, map[string]*Sender{}
	for _, m := range l.msgs {
		r := rooms[m.RoomID]
		if r == nil {
			r = &Room{RoomID: m.RoomID, Kind: m.Kind}
			rooms[m.RoomID] = r
		}
		r.Room, r.Last = m.Room, m.Time // the latest name wins
		r.Messages++
		s := senders[m.FromUID]
		if s == nil {
			s = &Sender{UID: m.FromUID}
			senders[m.FromUID] = s
		}
		s.Callsign = m.FromCallsign
		s.Messages++
	}
	outR, outS := make([]Room, 0, len(rooms)), make([]Sender, 0, len(senders))
	for _, r := range rooms {
		outR = append(outR, *r)
	}
	for _, s := range senders {
		outS = append(outS, *s)
	}
	slices.SortFunc(outR, func(a, b Room) int { return b.Last.Compare(a.Last) })
	slices.SortFunc(outS, func(a, b Sender) int {
		return strings.Compare(strings.ToLower(a.Callsign), strings.ToLower(b.Callsign))
	})
	return outR, outS
}

// Stats reports how many messages are held and their size on disk.
func (l *Log) Stats() (messages int, bytes int64) {
	l.mu.Lock()
	messages = len(l.msgs)
	l.mu.Unlock()
	files, _ := l.dayFiles()
	for _, f := range files {
		if fi, err := os.Stat(filepath.Join(l.dir, f)); err == nil {
			bytes += fi.Size()
		}
	}
	return
}

// Prune removes messages older than before (with dry, only counts them).
// Day files holding only older messages are deleted; the day that straddles
// before is rewritten.
func (l *Log) Prune(before time.Time, dry bool) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := sort.Search(len(l.msgs), func(i int) bool { return !l.msgs[i].Time.Before(before) })
	if dry || n == 0 {
		return n, nil
	}
	for _, m := range l.msgs[:n] {
		delete(l.seen, m.ID)
	}
	l.msgs = slices.Delete(l.msgs, 0, n)
	files, err := l.dayFiles()
	if err != nil {
		return n, err
	}
	for _, f := range files {
		day, err := time.Parse(dayLayout, strings.TrimSuffix(strings.TrimPrefix(f, filePrefix), fileSuffix))
		if err != nil || !day.Before(before) {
			continue
		}
		if l.fileDay == day.Format(dayLayout) {
			l.closeFileLocked()
		}
		path := filepath.Join(l.dir, f)
		if !day.Add(24 * time.Hour).After(before) {
			if err := os.Remove(path); err != nil {
				return n, err
			}
			continue
		}
		if err := l.rewriteDayLocked(path, day); err != nil {
			return n, err
		}
	}
	return n, nil
}

// rewriteDayLocked replaces a day file with the messages still held for it.
func (l *Log) rewriteDayLocked(path string, day time.Time) error {
	var b []byte
	end := day.Add(24 * time.Hour)
	for _, m := range l.msgs {
		if t := m.Time.UTC(); !t.Before(day) && t.Before(end) {
			line, _ := json.Marshal(m)
			b = append(append(b, line...), '\n')
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Clear deletes the whole chat log.
func (l *Log) Clear() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeFileLocked()
	l.msgs, l.seen = nil, map[string]bool{}
	files, err := l.dayFiles()
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := os.Remove(filepath.Join(l.dir, f)); err != nil {
			return fmt.Errorf("remove %s: %w", f, err)
		}
	}
	return nil
}

// Close closes the open day file.
func (l *Log) Close() {
	l.mu.Lock()
	l.closeFileLocked()
	l.mu.Unlock()
}
