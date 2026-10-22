package web

import (
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

func TestNextRun(t *testing.T) {
	saved := time.Local
	time.Local = time.UTC
	defer func() { time.Local = saved }()
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// 2026-10-23 is a Friday.
	start := at("2026-10-23 07:00")
	for _, tc := range []struct {
		c     store.Schedule
		after string
		want  string // "" = none
	}{
		{store.Schedule{Start: start, Repeat: "once"}, "2026-10-22 12:00", "2026-10-23 07:00"},
		{store.Schedule{Start: start, Repeat: "once"}, "2026-10-23 07:00", ""},
		{store.Schedule{Start: start, Repeat: "daily"}, "2026-10-23 07:00", "2026-10-24 07:00"},
		{store.Schedule{Start: start, Repeat: "daily"}, "2026-10-25 06:59", "2026-10-25 07:00"},
		{store.Schedule{Start: start, Repeat: "weekdays"}, "2026-10-23 07:00", "2026-10-26 07:00"}, // Friday -> Monday
		{store.Schedule{Start: start, Repeat: "weekly", Days: []int{2, 4}}, "2026-10-23 07:00", "2026-10-27 07:00"}, // Tuesday
		{store.Schedule{Start: start, Repeat: "hourly", Every: 4}, "2026-10-23 07:00", "2026-10-23 11:00"},
		{store.Schedule{Start: start, Repeat: "hourly", Every: 4}, "2026-10-23 12:30", "2026-10-23 15:00"},
		{store.Schedule{Start: start, Repeat: "daily", Until: at("2026-10-24 12:00")}, "2026-10-24 07:00", ""},
	} {
		got := nextRun(tc.c, at(tc.after))
		want := time.Time{}
		if tc.want != "" {
			want = at(tc.want)
		}
		if !got.Equal(want) {
			t.Errorf("%s every %d %v after %s = %v, want %v", tc.c.Repeat, tc.c.Every, tc.c.Days, tc.after, got, want)
		}
	}
}
