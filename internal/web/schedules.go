package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/audit"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Scheduled sends (Messages -> Send -> Scheduled): a chat message or data
// package sent to everyone or chosen groups at a set time, once or on a
// repeat (every few hours, daily, weekdays, chosen days of the week).
// Times follow the server's time zone. A run missed by more than
// scheduleGrace (EdgeTAK was off) is skipped, not sent late.

const (
	scheduleCheckEvery = 15 * time.Second
	scheduleGrace      = 15 * time.Minute
)

var scheduleRepeats = map[string]bool{"once": true, "hourly": true, "daily": true, "weekdays": true, "weekly": true}

// nextRun is the schedule's first run after after (zero when there is none).
func nextRun(c store.Schedule, after time.Time) time.Time {
	start := c.Start.In(time.Local)
	ok := func(t time.Time) bool { return c.Until.IsZero() || !t.After(c.Until) }
	if !start.After(after) && c.Repeat == "once" {
		return time.Time{}
	}
	if start.After(after) {
		if ok(start) {
			return start
		}
		return time.Time{}
	}
	var t time.Time
	switch c.Repeat {
	case "hourly":
		step := time.Duration(max(c.Every, 1)) * time.Hour
		n := after.Sub(start)/step + 1
		t = start.Add(n * step)
	default: // daily, weekdays, weekly: the start's clock time on the next suitable day
		a := after.In(time.Local)
		for d := 0; d <= 8; d++ {
			day := time.Date(a.Year(), a.Month(), a.Day()+d, start.Hour(), start.Minute(), 0, 0, time.Local)
			if !day.After(after) {
				continue
			}
			wd := int(day.Weekday())
			if (c.Repeat == "weekdays" && (wd == 0 || wd == 6)) || (c.Repeat == "weekly" && !slices.Contains(c.Days, wd)) {
				continue
			}
			t = day
			break
		}
	}
	if t.IsZero() || !ok(t) {
		return time.Time{}
	}
	return t
}

func validSchedule(c *store.Schedule, s *Server) error {
	c.Name, c.Text, c.From = strings.TrimSpace(c.Name), strings.TrimSpace(c.Text), strings.TrimSpace(c.From)
	switch {
	case c.Name == "" || len(c.Name) > 80:
		return errors.New("give it a name (up to 80 characters)")
	case c.Kind == "message" && (c.Text == "" || len(c.Text) > 4000):
		return errors.New("write the message (up to 4000 characters)")
	case c.Kind == "package":
		if _, ok := s.Store.Package(strings.ToLower(c.PackageHash)); !ok {
			return errors.New("choose a data package")
		}
		c.PackageHash, c.Text = strings.ToLower(c.PackageHash), ""
	case c.Kind != "message":
		return errors.New("send a message or a data package")
	}
	if !scheduleRepeats[c.Repeat] {
		return errors.New("choose when it repeats")
	}
	c.Groups = cleanGroups(c.Groups)
	if !c.All && len(c.Groups) == 0 {
		return errors.New("send it to everyone or to some groups")
	}
	if c.All {
		c.Groups = []string{}
	}
	var days []int
	for d := range 7 {
		if slices.Contains(c.Days, d) {
			days = append(days, d)
		}
	}
	c.Days = days
	if c.Repeat == "weekly" && len(c.Days) == 0 {
		return errors.New("choose the days of the week")
	}
	if c.Repeat == "hourly" && (c.Every < 1 || c.Every > 24) {
		c.Every = max(1, min(c.Every, 24))
	}
	if c.Start.IsZero() {
		return errors.New("choose when to send it")
	}
	return nil
}

// runSchedule sends a schedule now and records the result.
func (s *Server) runSchedule(c store.Schedule, by string) (string, error) {
	t := sendTarget{All: c.All, Groups: c.Groups}
	targets := s.resolveTargets(t)
	from := firstNonEmpty(c.From, s.Cfg.ServerName)
	result := ""
	switch c.Kind {
	case "message":
		if len(targets) > 0 {
			ev := s.chatEvent(from, c.Text)
			for _, cl := range targets {
				cl.Send(ev)
			}
			s.logConsoleChat(ev, "schedule: "+c.Name)
		}
		result = fmt.Sprintf("message sent to %d device%s", len(targets), plural(len(targets)))
		s.history.add(sendRecord{Time: time.Now().UTC(), Admin: "schedule: " + c.Name, Kind: "message", Summary: from + ": " + truncate(c.Text, 80),
			To: describeTarget(t), Delivered: len(targets)})
	case "package":
		p, ok := s.Store.Package(c.PackageHash)
		if !ok {
			return "", errors.New("its data package was deleted")
		}
		delivered := 0
		if len(targets) > 0 {
			var err error
			if delivered, _, err = s.offerPackage(p, targets, from); err != nil {
				return "", err
			}
		}
		result = fmt.Sprintf("%s offered to %d device%s", p.Name, delivered, plural(delivered))
		s.history.add(sendRecord{Time: time.Now().UTC(), Admin: "schedule: " + c.Name, Kind: "package", Summary: p.Name,
			To: describeTarget(t), Delivered: delivered})
	}
	s.Log.Info("scheduled send", "schedule", c.Name, "by", by, "result", result)
	if s.Audit != nil {
		s.Audit.Add(audit.Entry{Actor: by, Action: "Scheduled send ran", Target: c.Name, Details: result})
	}
	return result, nil
}

// RunDueSchedules sends the schedules whose time has come (every 15 s;
// tests pass the time).
func (s *Server) RunDueSchedules(now time.Time) {
	for _, c := range s.Store.Schedules() {
		if c.Off || c.Next.IsZero() || c.Next.After(now) {
			continue
		}
		if now.Sub(c.Next) > scheduleGrace {
			c.LastResult = "missed at " + c.Next.Local().Format("Jan 2 15:04") + " (EdgeTAK was off)"
		} else {
			res, err := s.runSchedule(c, "schedule")
			c.LastRun, c.Runs = now.UTC(), c.Runs+1
			c.LastResult = res
			if err != nil {
				c.LastResult = "failed: " + err.Error()
			}
		}
		c.Next = nextRun(c, now)
		s.Store.PutSchedule(c)
	}
}

func (s *Server) schedulesStart(ctx context.Context) {
	go func() {
		t := time.NewTicker(scheduleCheckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.RunDueSchedules(time.Now())
			}
		}
	}()
}

type scheduleView struct {
	store.Schedule
	PackageName string `json:"package_name,omitempty"`
}

func (s *Server) scheduleViews() []scheduleView {
	out := []scheduleView{}
	for _, c := range s.Store.Schedules() {
		v := scheduleView{Schedule: c}
		if p, ok := s.Store.Package(c.PackageHash); ok {
			v.PackageName = p.Name
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b scheduleView) int {
		switch { // upcoming first, soonest first; finished last
		case a.Next.IsZero() != b.Next.IsZero():
			if a.Next.IsZero() {
				return 1
			}
			return -1
		}
		return a.Next.Compare(b.Next)
	})
	return out
}

func (s *Server) scheduleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/schedules", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.scheduleViews())
	}))
	save := func(w http.ResponseWriter, r *http.Request, id *identity) {
		var c store.Schedule
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&c); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if sid := r.PathValue("id"); sid != "" {
			i := slices.IndexFunc(s.Store.Schedules(), func(o store.Schedule) bool { return o.ID == sid })
			if i < 0 {
				jsonError(w, http.StatusNotFound, "no such schedule")
				return
			}
			old := s.Store.Schedules()[i]
			c.ID, c.Author, c.Created, c.LastRun, c.LastResult, c.Runs = old.ID, old.Author, old.Created, old.LastRun, old.LastResult, old.Runs
		} else {
			c.ID, c.Author, c.Created = uuid(), id.Username, time.Now().UTC()
		}
		if err := validSchedule(&c, s); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		now := time.Now()
		c.Next = nextRun(c, now.Add(-time.Second))
		if c.Next.IsZero() && !c.Off {
			jsonError(w, http.StatusBadRequest, "that time has passed; choose a time in the future")
			return
		}
		if err := s.Store.PutSchedule(c); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.scheduleViews())
	}
	mux.HandleFunc("POST /admin/api/schedules", s.admin(save))
	mux.HandleFunc("PUT /admin/api/schedules/{id}", s.admin(save))
	mux.HandleFunc("DELETE /admin/api/schedules/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if err := s.Store.DeleteSchedule(r.PathValue("id")); err != nil {
			jsonError(w, http.StatusNotFound, "no such schedule")
			return
		}
		writeJSON(w, http.StatusOK, s.scheduleViews())
	}))
	// Pause or resume.
	mux.HandleFunc("PATCH /admin/api/schedules/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		var req struct {
			Off bool `json:"off"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req)
		i := slices.IndexFunc(s.Store.Schedules(), func(o store.Schedule) bool { return o.ID == r.PathValue("id") })
		if i < 0 {
			jsonError(w, http.StatusNotFound, "no such schedule")
			return
		}
		c := s.Store.Schedules()[i]
		c.Off = req.Off
		if !c.Off {
			c.Next = nextRun(c, time.Now()) // resumed: from now on, without catching up
		}
		s.Store.PutSchedule(c)
		writeJSON(w, http.StatusOK, s.scheduleViews())
	}))
	// Send it now (its timetable is unchanged).
	mux.HandleFunc("POST /admin/api/schedules/{id}/run", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		i := slices.IndexFunc(s.Store.Schedules(), func(o store.Schedule) bool { return o.ID == r.PathValue("id") })
		if i < 0 {
			jsonError(w, http.StatusNotFound, "no such schedule")
			return
		}
		c := s.Store.Schedules()[i]
		res, err := s.runSchedule(c, id.Username)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		c.LastRun, c.LastResult, c.Runs = time.Now().UTC(), res+" (sent now by "+id.Username+")", c.Runs+1
		s.Store.PutSchedule(c)
		writeJSON(w, http.StatusOK, map[string]any{"result": res, "schedules": s.scheduleViews()})
	}))
}
