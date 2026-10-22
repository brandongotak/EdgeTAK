package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Lost contact alerts. A device that signs off (turns the connection off,
// quits ATAK, shuts the phone down) closes its stream cleanly and is only
// listed as signed off. One whose stream goes silent or is cut raises an
// alert once it has been quiet for the grace period, unless it comes back
// first (a phone switching networks reconnects within seconds); so does one
// that stays connected without sending a position. Alerts are emergency
// records of kind "lost": the console shows them like emergencies, in
// amber, and they end by themselves when the device is back.

const (
	lostCheckEvery = 15 * time.Second
	offlineKeep    = 24 * time.Hour
)

// offlineDevice is a device whose stream ended.
type offlineDevice struct {
	UID      string    `json:"uid"`
	Callsign string    `json:"callsign"`
	Username string    `json:"username,omitempty"`
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	LastSeen time.Time `json:"last_seen"` // last heard from
	Ended    time.Time `json:"ended"`     // when the stream ended
	Clean    bool      `json:"signed_off"`

	groups []string
}

type lostState struct {
	mu      sync.Mutex
	offline map[string]*offlineDevice // by device UID
	alerts  map[string]string         // device UID -> its lost contact alert (kept after a dismissal until the device is back)
}

// ClientEnded is told by the stream servers when a client's stream ends.
func (s *Server) ClientEnded(c *router.Client, clean bool) {
	uid, callsign := c.Identity()
	if uid == "" {
		return // never reported who it is
	}
	for _, o := range s.Router.Clients() {
		if o != c {
			if u, _ := o.Identity(); u == uid {
				return // already back on another connection
			}
		}
	}
	d := &offlineDevice{UID: uid, Callsign: callsign, Username: c.Username, LastSeen: c.LastSeen(), Ended: time.Now(), Clean: clean,
		groups: deviceGroups(c)}
	if sa := c.LastSA(); sa != nil {
		d.Lat, d.Lon = sa.Lat, sa.Lon
	}
	s.lost.mu.Lock()
	if s.lost.offline == nil {
		s.lost.offline = map[string]*offlineDevice{}
	}
	s.lost.offline[uid] = d
	s.lost.mu.Unlock()
	if clean {
		s.Log.Info("device signed off", "callsign", callsign, "user", c.Username)
	} else {
		s.Log.Info("device connection lost", "callsign", callsign, "user", c.Username, "quiet_since", d.LastSeen.Format(time.TimeOnly))
	}
}

func deviceGroups(c *router.Client) []string {
	g := append(slices.Clone(c.InGroups()), c.OutGroups()...)
	slices.Sort(g)
	return slices.Compact(g)
}

func watched(set store.LostContactSettings, groups []string) bool {
	if len(set.Groups) == 0 {
		return true
	}
	for _, g := range groups {
		if slices.Contains(set.Groups, g) {
			return true
		}
	}
	return false
}

func (s *Server) lostStart(ctx context.Context) {
	// Alerts still open from before a restart end when their device is back.
	s.lost.mu.Lock()
	s.lost.alerts = map[string]string{}
	for _, e := range s.activeEmergencies() {
		if e.Kind == "lost" {
			s.lost.alerts[e.DeviceUID] = e.ID
		}
	}
	s.lost.mu.Unlock()
	go func() {
		t := time.NewTicker(lostCheckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.CheckLostContact(time.Now())
			}
		}
	}()
}

// CheckLostContact raises and ends lost contact alerts, as of now (run every
// 15 seconds; tests pass a later time instead of waiting).
func (s *Server) CheckLostContact(now time.Time) {
	set := s.Store.LostContact()
	grace := time.Duration(set.GraceMinutes) * time.Minute
	noPos := time.Duration(set.NoPositionMinutes) * time.Minute
	connected := map[string]*router.Client{}
	for _, c := range s.Router.Clients() {
		if uid, _ := c.Identity(); uid != "" {
			connected[uid] = c
		}
	}

	type alert struct {
		uid, callsign, username, what string
		lat, lon                      float64
		since                         time.Time
	}
	var raise []alert
	var back []string

	s.lost.mu.Lock()
	if s.lost.alerts == nil {
		s.lost.alerts = map[string]string{}
	}
	for uid, d := range s.lost.offline {
		switch {
		case connected[uid] != nil:
			delete(s.lost.offline, uid)
		case now.Sub(d.Ended) > offlineKeep:
			delete(s.lost.offline, uid)
		case !d.Clean && !set.Off && s.lost.alerts[uid] == "" && now.Sub(d.LastSeen) >= grace && watched(set, d.groups):
			raise = append(raise, alert{uid, d.Callsign, d.Username, "Lost contact", d.Lat, d.Lon, d.LastSeen})
		}
	}
	for uid, c := range connected {
		_, callsign := c.Identity()
		quiet := now.Sub(c.LastSeen()) >= grace
		lastPos := c.LastSAAt()
		stuck := noPos > 0 && !lastPos.IsZero() && now.Sub(lastPos) >= noPos
		if !quiet && !stuck {
			if s.lost.alerts[uid] != "" {
				back = append(back, uid)
			}
			continue
		}
		if set.Off || s.lost.alerts[uid] != "" || !watched(set, deviceGroups(c)) {
			continue
		}
		a := alert{uid: uid, callsign: callsign, username: c.Username, what: "Lost contact", since: c.LastSeen()}
		if !quiet {
			a.what, a.since = "No position", lastPos
		}
		if sa := c.LastSA(); sa != nil {
			a.lat, a.lon = sa.Lat, sa.Lon
		}
		raise = append(raise, a)
	}
	// Devices back in contact end their alerts.
	for _, uid := range back {
		s.endLostAlert(s.lost.alerts[uid])
		delete(s.lost.alerts, uid)
	}
	for _, a := range raise {
		id := uuid()
		e := store.Emergency{ID: id, Kind: "lost", EventUID: a.uid + "-lost", DeviceUID: a.uid, Callsign: a.callsign, Username: a.username,
			Type: a.what, Lat: a.lat, Lon: a.lon, Started: a.since.UTC(), Updated: now.UTC()}
		if err := s.Store.AddEmergency(e); err != nil {
			s.Log.Error("store lost contact alert", "err", err)
			continue
		}
		s.lost.alerts[a.uid] = id
		s.Log.Warn("LOST CONTACT", "what", a.what, "callsign", a.callsign, "user", a.username, "since", a.since.Format(time.TimeOnly))
		go s.notify(notice{Kind: "lost_contact", Title: a.what, Callsign: a.callsign, Username: a.username, Lat: a.lat, Lon: a.lon,
			Text: "Last heard from at " + a.since.Local().Format("15:04:05 MST") + "."})
	}
	s.lost.mu.Unlock()
}

// endLostAlert closes an alert because its device is back (unless an admin
// already closed it).
func (s *Server) endLostAlert(id string) {
	now := time.Now().UTC()
	e, err := s.Store.UpdateEmergency(id, func(e *store.Emergency) {
		if e.Ended == nil {
			e.Ended, e.EndedBy = &now, "device"
		}
	})
	if err == nil && e.EndedBy == "device" {
		s.Log.Info("back in contact", "callsign", e.Callsign)
		go s.notify(notice{Kind: "ended", Title: "Back in contact", Callsign: e.Callsign, Username: e.Username})
	}
}

func (s *Server) lostContactRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/lost-contact", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.lostContactView())
	}))
	mux.HandleFunc("PUT /admin/api/lost-contact", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var l store.LostContactSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&l); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if l.GraceMinutes < 1 || l.GraceMinutes > 24*60 || l.NoPositionMinutes < -1 || l.NoPositionMinutes > 24*60 {
			jsonError(w, http.StatusBadRequest, "the times are minutes, from 1 to 1440")
			return
		}
		if l.NoPositionMinutes == 0 {
			l.NoPositionMinutes = -1 // 0 in the form means off
		}
		if err := s.Store.SetLostContact(l); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("lost contact settings changed", "admin", id.Username, "off", l.Off, "grace", l.GraceMinutes, "no_position", l.NoPositionMinutes, "groups", l.Groups)
		writeJSON(w, http.StatusOK, s.lostContactView())
	}))
}

// LostContactOffline lists the devices whose stream ended in the last day.
func (s *Server) LostContactOffline() []offlineDevice {
	return s.lostContactView()["offline"].([]offlineDevice)
}

func (s *Server) lostContactView() map[string]any {
	s.lost.mu.Lock()
	list := []offlineDevice{}
	for _, d := range s.lost.offline {
		list = append(list, *d)
	}
	s.lost.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Ended.After(list[j].Ended) })
	return map[string]any{"settings": s.Store.LostContact(), "offline": list}
}
