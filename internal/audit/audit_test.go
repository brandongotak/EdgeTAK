package audit

import (
	"testing"
	"time"
)

func TestLog(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	sept := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	l.Add(Entry{Time: sept, Actor: "admin", Action: "Created user", Target: "alice"})
	l.Add(Entry{Time: oct, Actor: "boss", Action: "Deleted user", Target: "bob"})
	l.Add(Entry{Time: oct.Add(time.Hour), Actor: "mallory", Action: "Sign-in failed", Failed: true})

	all, err := l.Query(Filter{})
	if err != nil || len(all) != 3 || all[0].Actor != "mallory" || all[2].Target != "alice" {
		t.Fatalf("all = %+v %v", all, err)
	}
	if got, _ := l.Query(Filter{Text: "ALICE"}); len(got) != 1 {
		t.Errorf("text = %+v", got)
	}
	if got, _ := l.Query(Filter{Actor: "boss"}); len(got) != 1 || got[0].Target != "bob" {
		t.Errorf("actor = %+v", got)
	}
	if got, _ := l.Query(Filter{Failed: true}); len(got) != 1 {
		t.Errorf("failed = %+v", got)
	}
	if got, _ := l.Query(Filter{Since: oct}); len(got) != 2 {
		t.Errorf("since = %+v", got)
	}
	page, _ := l.Query(Filter{Limit: 2})
	if more, _ := l.Query(Filter{Before: page[1].Time}); len(page) != 2 || len(more) != 1 || more[0].Target != "alice" {
		t.Errorf("paging = %+v then %+v", page, more)
	}
	if n, _ := l.Prune(oct, true); n != 1 {
		t.Errorf("dry run counted %d months", n)
	}
	if n, _ := l.Prune(oct, false); n != 1 {
		t.Errorf("pruned %d months", n)
	}
	if got, _ := l.Query(Filter{}); len(got) != 2 {
		t.Errorf("after prune = %+v", got)
	}
}

func TestClear(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.Add(Entry{Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Actor: "a", Action: "x"})
	l.Add(Entry{Actor: "b", Action: "y"})
	l.Add(Entry{Actor: "c", Action: "z"})
	if n, err := l.Clear(); n != 3 || err != nil {
		t.Fatalf("clear = %d %v", n, err)
	}
	if got, _ := l.Query(Filter{}); len(got) != 0 {
		t.Fatalf("after clear = %+v", got)
	}
	// It keeps working afterwards.
	l.Add(Entry{Actor: "d", Action: "Cleared the audit log"})
	if got, _ := l.Query(Filter{}); len(got) != 1 {
		t.Errorf("after adding = %+v", got)
	}
}
