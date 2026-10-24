// Package feeds turns public aircraft (ADS-B) and ship (AIS) data into CoT
// markers for TAK devices: aircraft from a free community ADS-B API
// (adsb.lol or adsb.fi), polled around a point, and ships from aisstream.io's
// live feed for the same area.
package feeds

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Track is an aircraft or ship as last reported.
type Track struct {
	UID      string    `json:"uid"`
	Kind     string    `json:"kind"` // aircraft or ship
	Callsign string    `json:"callsign"`
	Type     string    `json:"type"` // CoT type
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	AltM     float64   `json:"alt_m,omitempty"`
	Course   float64   `json:"course"`
	SpeedMS  float64   `json:"speed_ms"`
	Remarks  string    `json:"remarks"`
	Seen     time.Time `json:"seen"`
}

// FeedStatus is how a feed is doing.
type FeedStatus struct {
	Running bool      `json:"running"`
	Tracks  int       `json:"tracks"`
	Last    time.Time `json:"last,omitzero"` // last data received
	Error   string    `json:"error,omitempty"`
}

// Service runs the feeds.
type Service struct {
	Router  *router.Router
	Log     *slog.Logger
	HTTP    *http.Client // nil = a default client
	Version string
	// Endpoints, overridable in tests.
	ADSBURL func(source string, lat, lon float64, radiusNM int) string
	AISURL  string

	mu       sync.Mutex
	ctx      context.Context
	set      store.FeedSettings
	stopADSB context.CancelFunc
	stopAIS  context.CancelFunc
	tracks   map[string]Track
	adsb     FeedStatus
	ais      FeedStatus
}

const (
	shipStale  = 10 * time.Minute
	maxAirAge  = 60 * time.Second // positions older than this aren't sent
	userAgentF = "EdgeTAK/%s (+https://github.com/brandongotak/EdgeTAK)"
)

// Start runs the feeds in settings until ctx ends.
func (s *Service) Start(ctx context.Context, set store.FeedSettings) {
	s.mu.Lock()
	s.ctx, s.tracks = ctx, map[string]Track{}
	s.mu.Unlock()
	s.Apply(set)
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.prune()
			}
		}
	}()
}

// Apply starts, stops or restarts the feeds for new settings.
func (s *Service) Apply(set store.FeedSettings) {
	set = set.WithDefaults()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil {
		s.set = set
		return
	}
	old := s.set
	s.set = set
	areaSame := old.Lat == set.Lat && old.Lon == set.Lon // each feed's radius is in its own settings
	adsbOn := set.ADSB.Enabled && validArea(set)
	if s.stopADSB == nil || !adsbOn || !areaSame || !reflect.DeepEqual(old.ADSB, set.ADSB) {
		if s.stopADSB != nil {
			s.stopADSB()
			s.stopADSB = nil
		}
		s.adsb = FeedStatus{}
		if adsbOn {
			ctx, cancel := context.WithCancel(s.ctx)
			s.stopADSB = cancel
			go s.runADSB(ctx, set)
		}
		s.dropKind("aircraft")
	}
	aisOn := set.AIS.Enabled && set.AIS.APIKey != "" && validArea(set)
	if s.stopAIS == nil || !aisOn || !areaSame || !reflect.DeepEqual(old.AIS, set.AIS) {
		if s.stopAIS != nil {
			s.stopAIS()
			s.stopAIS = nil
		}
		s.ais = FeedStatus{}
		if aisOn {
			ctx, cancel := context.WithCancel(s.ctx)
			s.stopAIS = cancel
			go s.runAIS(ctx, set)
		}
		s.dropKind("ship")
	}
}

func validArea(set store.FeedSettings) bool {
	return set.Lat >= -90 && set.Lat <= 90 && set.Lon >= -180 && set.Lon <= 180 && !(set.Lat == 0 && set.Lon == 0)
}

// dropKind forgets one kind of track (mu held).
func (s *Service) dropKind(kind string) {
	for k, t := range s.tracks {
		if t.Kind == kind {
			delete(s.tracks, k)
		}
	}
}

// Status reports both feeds.
func (s *Service) Status() map[string]FeedStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, b := s.adsb, s.ais
	for _, t := range s.tracks {
		if t.Kind == "aircraft" {
			a.Tracks++
		} else {
			b.Tracks++
		}
	}
	return map[string]FeedStatus{"adsb": a, "ais": b}
}

// Tracks lists the current aircraft and ships, for the console map.
func (s *Service) Tracks() []Track {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Track, 0, len(s.tracks))
	for _, t := range s.tracks {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b Track) int { return strings.Compare(a.UID, b.UID) })
	return out
}

func (s *Service) prune() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.tracks {
		max := 2 * time.Minute
		if t.Kind == "ship" {
			max = shipStale
		}
		if now.Sub(t.Seen) > max {
			delete(s.tracks, k)
		}
	}
}

func (s *Service) setStatus(kind string, f func(*FeedStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "aircraft" {
		f(&s.adsb)
	} else {
		f(&s.ais)
	}
}

// send records a track and delivers its marker to the feed's groups.
func (s *Service) send(t Track, stale time.Duration, groups []string) {
	t.Seen = time.Now().UTC()
	s.mu.Lock()
	s.tracks[t.UID] = t
	s.mu.Unlock()
	ev := cot.NewEvent(t.Type, t.UID, "m-g", stale)
	ev.Lat, ev.Lon, ev.CE = t.Lat, t.Lon, 30
	if t.AltM != 0 {
		ev.Hae = t.AltM
	}
	ev.Detail.Contact = &cot.Contact{Callsign: t.Callsign}
	ev.Detail.Track = &cot.Track{Course: t.Course, Speed: t.SpeedMS}
	var b strings.Builder
	b.WriteString("<remarks>")
	xml.EscapeText(&b, []byte(t.Remarks))
	b.WriteString("</remarks>")
	ev.Detail.XML = b.String()
	ev.Changed()
	if s.Router == nil {
		return
	}
	if len(groups) == 0 {
		s.Router.Broadcast(ev)
	} else {
		s.Router.Inject(ev, groups)
	}
}

func (s *Service) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (s *Service) userAgent() string { return fmt.Sprintf(userAgentF, firstNonEmpty(s.Version, "dev")) }

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// knots to metres per second, feet to metres.
const (
	knot = 0.514444
	foot = 0.3048
)

// bbox is the square around the ships' area, as aisstream.io wants it.
func bbox(set store.FeedSettings) [2][2]float64 {
	dLat := float64(set.AIS.RadiusNM) / 60
	dLon := dLat / math.Max(math.Cos(set.Lat*math.Pi/180), 0.01)
	return [2][2]float64{{set.Lat - dLat, set.Lon - dLon}, {set.Lat + dLat, set.Lon + dLon}}
}
