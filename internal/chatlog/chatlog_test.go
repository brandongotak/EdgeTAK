package chatlog

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistAndPrune(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	l, err := Open(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	l.Record(Message{ID: "ignored", Time: time.Now()}) // recording is off
	l.SetEnabled(true)
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	for i, at := range []time.Time{day.Add(-30 * time.Hour), day.Add(2 * time.Hour), day.Add(20 * time.Hour), day.Add(30 * time.Hour)} {
		l.Record(Message{ID: string(rune('a' + i)), Time: at, RoomID: "all", Text: "m"})
	}
	l.Record(Message{ID: "b", Time: day}) // duplicate ID
	if msgs, total := l.Find(Query{}); total != 4 || msgs[0].ID != "d" {
		t.Fatalf("find = %d %v", total, msgs)
	}

	// Remove everything before 10:00 on the 10th: the 9th's file goes, the
	// 10th's is rewritten without its early message.
	cut := day.Add(10 * time.Hour)
	if n, _ := l.Prune(cut, true); n != 2 {
		t.Fatalf("dry prune = %d", n)
	}
	if n, err := l.Prune(cut, false); n != 2 || err != nil {
		t.Fatalf("prune = %d %v", n, err)
	}
	l.Close()
	if _, err := os.Stat(filepath.Join(dir, "chat-2026-03-09.jsonl")); !os.IsNotExist(err) {
		t.Error("old day file kept")
	}

	l2, err := Open(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	msgs, total := l2.Find(Query{})
	if total != 2 || msgs[0].ID != "d" || msgs[1].ID != "c" {
		t.Fatalf("after reopen = %v", msgs)
	}
	if _, total := l2.Find(Query{Since: day.Add(25 * time.Hour)}); total != 1 {
		t.Errorf("since = %d", total)
	}
	if err := l2.Clear(); err != nil {
		t.Fatal(err)
	}
	if n, _ := l2.Stats(); n != 0 {
		t.Errorf("after clear = %d", n)
	}
}
