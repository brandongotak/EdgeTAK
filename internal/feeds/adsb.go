package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Aircraft come from an ADS-B API in the readsb format (adsb.lol answers
// with "ac", adsb.fi and readsb's own aircraft.json with "aircraft"), polled
// around the area: a free community one, or a custom URL (a receiver of
// your own, or another API in that format).

type aircraft struct {
	Hex       string   `json:"hex"`
	Flight    string   `json:"flight"`
	Reg       string   `json:"r"`
	ICAOType  string   `json:"t"`
	Desc      string   `json:"desc"`
	AltBaro   any      `json:"alt_baro"` // feet, or "ground"
	AltGeom   *float64 `json:"alt_geom"` // feet
	GS        *float64 `json:"gs"`       // knots
	Altitude  any      `json:"altitude"` // older dump1090: feet, or "ground"
	Speed     *float64 `json:"speed"`    // older dump1090: knots
	Track     *float64 `json:"track"`
	Squawk    string   `json:"squawk"`
	Emergency string   `json:"emergency"`
	Category  string   `json:"category"`
	Lat       *float64 `json:"lat"`
	Lon       *float64 `json:"lon"`
	SeenPos   *float64 `json:"seen_pos"` // seconds since the position
	DBFlags   int      `json:"dbFlags"`  // bit 1: military
}

func (s *Service) adsbURL(set store.FeedSettings) string {
	if s.ADSBURL != nil {
		return s.ADSBURL(set.ADSB.Source, set.Lat, set.Lon, set.ADSB.RadiusNM)
	}
	if set.ADSB.Source == "custom" {
		return strings.NewReplacer("{lat}", fmt.Sprintf("%.4f", set.Lat), "{lon}", fmt.Sprintf("%.4f", set.Lon),
			"{radius}", fmt.Sprint(set.ADSB.RadiusNM)).Replace(set.ADSB.CustomURL)
	}
	if set.ADSB.Source == "adsb.fi" {
		return fmt.Sprintf("https://opendata.adsb.fi/api/v2/lat/%.4f/lon/%.4f/dist/%d", set.Lat, set.Lon, set.ADSB.RadiusNM)
	}
	return fmt.Sprintf("https://api.adsb.lol/v2/point/%.4f/%.4f/%d", set.Lat, set.Lon, set.ADSB.RadiusNM)
}

func (s *Service) runADSB(ctx context.Context, set store.FeedSettings) {
	every := time.Duration(set.ADSB.IntervalSec) * time.Second
	stale := max(time.Minute, 6*every)
	s.setStatus("aircraft", func(f *FeedStatus) { f.Running = true })
	for {
		wait := every
		if list, err := s.fetchAircraft(ctx, set); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.setStatus("aircraft", func(f *FeedStatus) { f.Error = err.Error() })
			s.Log.Warn("ADS-B feed", "source", set.ADSB.Source, "err", err)
			if strings.Contains(err.Error(), "429") {
				wait = 3 * every // asked to slow down
			}
		} else {
			for _, a := range list {
				// A receiver or API that ignores the area sends everything it has.
				if a.Lat == nil || a.Lon == nil || distanceNM(set.Lat, set.Lon, *a.Lat, *a.Lon) > float64(set.ADSB.RadiusNM) {
					continue
				}
				if t, ok := aircraftTrack(a, sourceName(set)); ok {
					s.send(t, stale, set.ADSB.Groups)
				}
			}
			s.setStatus("aircraft", func(f *FeedStatus) { f.Last, f.Error = time.Now().UTC(), "" })
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (s *Service) fetchAircraft(ctx context.Context, set store.FeedSettings) ([]aircraft, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", s.adsbURL(set), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.userAgent())
	req.Header.Set("Accept", "application/json")
	if set.ADSB.Source == "custom" && set.ADSB.HeaderName != "" {
		req.Header.Set(set.ADSB.HeaderName, set.ADSB.HeaderValue)
	}
	name := sourceName(set)
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't reach %s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("%s answered %d %s", name, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var body struct {
		AC       []aircraft `json:"ac"`
		Aircraft []aircraft `json:"aircraft"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s: not ADS-B JSON in the readsb/dump1090 format: %v", name, err)
	}
	return append(body.AC, body.Aircraft...), nil
}

// sourceName names the ADS-B source in remarks and errors: a custom URL by
// its host.
func sourceName(set store.FeedSettings) string {
	if set.ADSB.Source != "custom" {
		return set.ADSB.Source
	}
	if u, err := url.Parse(set.ADSB.CustomURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "custom source"
}

// distanceNM is the great-circle distance in nautical miles.
func distanceNM(lat1, lon1, lat2, lon2 float64) float64 {
	const rad = math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * 3440.065 * math.Asin(math.Sqrt(h))
}

// aircraftTrack makes a marker from an aircraft with a recent position.
func aircraftTrack(a aircraft, source string) (Track, bool) {
	if a.Lat == nil || a.Lon == nil || a.Hex == "" || (a.SeenPos != nil && *a.SeenPos > maxAirAge.Seconds()) {
		return Track{}, false
	}
	hex := strings.ToLower(strings.TrimPrefix(a.Hex, "~"))
	t := Track{UID: "ICAO-" + hex, Kind: "aircraft", Lat: *a.Lat, Lon: *a.Lon}
	t.Callsign = firstNonEmpty(strings.TrimSpace(a.Flight), strings.TrimSpace(a.Reg), strings.ToUpper(hex))

	rotor, military := a.Category == "A7", a.DBFlags&1 != 0
	switch {
	case military && rotor:
		t.Type = "a-n-A-M-H"
	case military:
		t.Type = "a-n-A-M-F"
	case rotor:
		t.Type = "a-n-A-C-H"
	default:
		t.Type = "a-n-A-C-F"
	}

	var parts []string
	if d := firstNonEmpty(a.Desc, a.ICAOType); d != "" {
		parts = append(parts, d)
	}
	if a.Reg != "" {
		parts = append(parts, "Reg "+a.Reg)
	}
	onGround := false
	if a.AltBaro == nil {
		a.AltBaro = a.Altitude
	}
	if a.GS == nil {
		a.GS = a.Speed
	}
	switch v := a.AltBaro.(type) {
	case string:
		onGround = v == "ground"
	case float64:
		t.AltM = v * foot
	}
	if a.AltGeom != nil {
		t.AltM = *a.AltGeom * foot
	}
	if onGround {
		parts = append(parts, "on the ground")
	} else if t.AltM != 0 {
		parts = append(parts, fmt.Sprintf("%.0f ft", t.AltM/foot))
	}
	if a.GS != nil {
		t.SpeedMS = *a.GS * knot
		parts = append(parts, fmt.Sprintf("%.0f kt", *a.GS))
	}
	if a.Track != nil {
		t.Course = *a.Track
	}
	if a.Squawk != "" {
		parts = append(parts, "Squawk "+a.Squawk)
	}
	if a.Emergency != "" && a.Emergency != "none" {
		parts = append(parts, "EMERGENCY: "+a.Emergency)
	}
	if military {
		parts = append(parts, "military")
	}
	parts = append(parts, "ICAO "+hex, "ADS-B via "+source)
	t.Remarks = strings.Join(parts, " · ")
	return t, true
}
