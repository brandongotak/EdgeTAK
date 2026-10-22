// Package audit keeps the audit log: who changed what on the server, and
// when, plus sign-ins. Entries are appended to one JSON-lines file per month
// (audit-2026-10.jsonl) and are never edited.
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Entry is one recorded action.
type Entry struct {
	Time    time.Time `json:"time"`
	Actor   string    `json:"actor"`             // the account, or the name tried for a failed sign-in
	IP      string    `json:"ip,omitempty"`      // where the request came from
	Action  string    `json:"action"`            // e.g. "Deleted user"
	Target  string    `json:"target,omitempty"`  // what it was done to
	Details string    `json:"details,omitempty"` // settings changed, without secrets
	Status  int       `json:"status,omitempty"`  // HTTP status of the request
	Failed  bool      `json:"failed,omitempty"`  // the action was refused or went wrong
}

// Filter narrows a query. Entries match when every set field does.
type Filter struct {
	Text   string    // in the actor, action, target or details (any case)
	Actor  string    // exactly
	Since  time.Time // inclusive
	Before time.Time // exclusive; for paging back
	Failed bool      // only failures
	Limit  int       // default 200
}

// Log appends entries and searches them.
type Log struct {
	dir   string
	mu    sync.Mutex
	f     *os.File
	month string
}

// Open uses dir for the log files, creating it.
func Open(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Log{dir: dir}, nil
}

func fileFor(month string) string { return "audit-" + month + ".jsonl" }

// Add records an entry (time defaults to now).
func (l *Log) Add(e Entry) error {
	if l == nil {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Time = e.Time.UTC().Truncate(time.Millisecond)
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	month := e.Time.Format("2006-01")
	if l.f == nil || month != l.month {
		if l.f != nil {
			l.f.Close()
		}
		f, err := os.OpenFile(filepath.Join(l.dir, fileFor(month)), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			l.f = nil
			return err
		}
		l.f, l.month = f, month
	}
	_, err = l.f.Write(append(b, '\n'))
	return err
}

// Close closes the current file.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Query returns matching entries, newest first.
func (l *Log) Query(f Filter) ([]Entry, error) {
	if f.Limit <= 0 || f.Limit > 5000 {
		f.Limit = 200
	}
	names, err := filepath.Glob(filepath.Join(l.dir, "audit-*.jsonl"))
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	text := strings.ToLower(strings.TrimSpace(f.Text))
	out := []Entry{}
	for i := len(names) - 1; i >= 0 && len(out) < f.Limit; i-- {
		month := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(names[i]), "audit-"), ".jsonl")
		if m, err := time.Parse("2006-01", month); err == nil && !f.Since.IsZero() && m.AddDate(0, 1, 0).Before(f.Since) {
			break // older months can't match
		}
		entries, err := l.readFile(names[i])
		if err != nil {
			return nil, err
		}
		for j := len(entries) - 1; j >= 0 && len(out) < f.Limit; j-- {
			e := entries[j]
			switch {
			case !f.Since.IsZero() && e.Time.Before(f.Since),
				!f.Before.IsZero() && !e.Time.Before(f.Before),
				f.Actor != "" && e.Actor != f.Actor,
				f.Failed && !e.Failed,
				text != "" && !strings.Contains(strings.ToLower(e.Actor+" "+e.Action+" "+e.Target+" "+e.Details+" "+e.IP), text):
				continue
			}
			out = append(out, e)
		}
	}
	return out, nil
}

func (l *Log) readFile(name string) ([]Entry, error) {
	l.mu.Lock() // a line being appended is never read half-written
	defer l.mu.Unlock()
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// Prune deletes the months entirely before cut (dry: only counts them).
func (l *Log) Prune(cut time.Time, dry bool) (int, error) {
	names, err := filepath.Glob(filepath.Join(l.dir, "audit-*.jsonl"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, name := range names {
		month := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(name), "audit-"), ".jsonl")
		m, err := time.Parse("2006-01", month)
		if err != nil || !m.AddDate(0, 1, 0).Before(cut) {
			continue
		}
		if dry {
			n++
			continue
		}
		l.mu.Lock()
		if month == l.month && l.f != nil {
			l.f.Close()
			l.f = nil
		}
		err = os.Remove(name)
		l.mu.Unlock()
		if err == nil {
			n++
		}
	}
	return n, nil
}
