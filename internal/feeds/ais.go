package feeds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/websocket"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Ships come from aisstream.io's live WebSocket feed: a subscription with
// the API key and the area's bounding box, then position and static-data
// messages.

const aisStreamURL = "wss://stream.aisstream.io/v0/stream"

type aisMessage struct {
	Error       string `json:"error"`
	MessageType string
	MetaData    struct {
		MMSI      int64
		ShipName  string
		Latitude  float64
		Longitude float64
	}
	Message map[string]json.RawMessage
}

type aisPosition struct {
	Cog, Sog           float64
	TrueHeading        int
	Latitude           float64
	Longitude          float64
	NavigationalStatus int
	Name               string // extended class B
	Type               int
}

type aisStatic struct {
	Name, CallSign, Destination string
	Type                        int
	Dimension                   struct{ A, B, C, D int }
	ReportA                     struct{ Name string } // class B static data
	ReportB                     struct {
		ShipType int
		CallSign string
	} // class B static data
}

type shipInfo struct {
	Name, CallSign, Destination string
	Type, Length                int
}

func (s *Service) runAIS(ctx context.Context, set store.FeedSettings) {
	s.setStatus("ship", func(f *FeedStatus) { f.Running = true })
	ships := map[int64]shipInfo{}
	backoff := 5 * time.Second
	for {
		err := s.streamAIS(ctx, set, ships)
		if ctx.Err() != nil {
			return
		}
		wait := backoff
		var bad errBadKey
		if errors.As(err, &bad) {
			wait = 5 * time.Minute // don't hammer the service with a wrong key
		} else {
			backoff = min(2*backoff, time.Minute)
		}
		s.setStatus("ship", func(f *FeedStatus) { f.Error = err.Error() })
		s.Log.Warn("AIS feed", "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

type errBadKey struct{ msg string }

func (e errBadKey) Error() string { return "aisstream.io refused the API key: " + e.msg }

// streamAIS reads the feed until it fails or ctx ends.
func (s *Service) streamAIS(ctx context.Context, set store.FeedSettings, ships map[int64]shipInfo) error {
	cfg, err := websocket.NewConfig(firstNonEmpty(s.AISURL, aisStreamURL), "https://edgetak.local/")
	if err != nil {
		return err
	}
	cfg.Header.Set("User-Agent", s.userAgent())
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		return fmt.Errorf("can't reach aisstream.io: %v", err)
	}
	defer ws.Close()
	go func() { <-ctx.Done(); ws.Close() }()
	bb := bbox(set)
	sub := map[string]any{
		"APIKey":        set.AIS.APIKey,
		"BoundingBoxes": [][2][2]float64{bb},
		"FilterMessageTypes": []string{"PositionReport", "StandardClassBPositionReport", "ExtendedClassBPositionReport",
			"ShipStaticData", "StaticDataReport"},
	}
	ws.SetWriteDeadline(time.Now().Add(3 * time.Second)) // aisstream.io wants it within 3 s
	if err := websocket.JSON.Send(ws, sub); err != nil {
		return fmt.Errorf("aisstream.io: sending the subscription: %v", err)
	}
	ws.SetWriteDeadline(time.Time{})
	s.Log.Info("AIS feed connected", "bbox", bb)
	for {
		ws.SetReadDeadline(time.Now().Add(5 * time.Minute)) // a quiet area still gets something now and then
		var m aisMessage
		if err := websocket.JSON.Receive(ws, &m); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("aisstream.io connection ended: %v", err)
		}
		if m.Error != "" {
			if strings.Contains(strings.ToLower(m.Error), "key") {
				return errBadKey{m.Error}
			}
			return fmt.Errorf("aisstream.io: %s", m.Error)
		}
		s.setStatus("ship", func(f *FeedStatus) { f.Last, f.Error = time.Now().UTC(), "" })
		if t, ok := s.aisMessageTrack(m, ships); ok {
			s.send(t, shipStale, set.AIS.Groups)
		}
	}
}

// aisMessageTrack remembers static data and makes a marker from a position.
func (s *Service) aisMessageTrack(m aisMessage, ships map[int64]shipInfo) (Track, bool) {
	mmsi := m.MetaData.MMSI
	if mmsi == 0 {
		return Track{}, false
	}
	raw := m.Message[m.MessageType]
	info := ships[mmsi]
	switch m.MessageType {
	case "ShipStaticData", "StaticDataReport":
		var st aisStatic
		if json.Unmarshal(raw, &st) == nil {
			info.Name = firstNonEmpty(clean(st.Name), clean(st.ReportA.Name), info.Name)
			info.CallSign = firstNonEmpty(clean(st.CallSign), clean(st.ReportB.CallSign), info.CallSign)
			info.Destination = firstNonEmpty(clean(st.Destination), info.Destination)
			if t := max(st.Type, st.ReportB.ShipType); t > 0 {
				info.Type = t
			}
			if l := st.Dimension.A + st.Dimension.B; l > 0 {
				info.Length = l
			}
			ships[mmsi] = info
		}
		return Track{}, false
	case "PositionReport", "StandardClassBPositionReport", "ExtendedClassBPositionReport":
	default:
		return Track{}, false
	}
	var p aisPosition
	if json.Unmarshal(raw, &p) != nil {
		return Track{}, false
	}
	if p.Name != "" && info.Name == "" {
		info.Name = clean(p.Name)
	}
	if p.Type > 0 && info.Type == 0 {
		info.Type = p.Type
	}
	ships[mmsi] = info
	lat, lon := firstNonZero(p.Latitude, m.MetaData.Latitude), firstNonZero(p.Longitude, m.MetaData.Longitude)
	if lat == 0 && lon == 0 || lat > 90 || lat < -90 || lon > 180 || lon < -180 {
		return Track{}, false
	}
	return shipTrack(mmsi, firstNonEmpty(info.Name, clean(m.MetaData.ShipName)), info, p, lat, lon), true
}

func shipTrack(mmsi int64, name string, info shipInfo, p aisPosition, lat, lon float64) Track {
	id := strconv.FormatInt(mmsi, 10)
	t := Track{UID: "MMSI-" + id, Kind: "ship", Lat: lat, Lon: lon, Callsign: firstNonEmpty(name, "MMSI "+id), Type: shipCoT(info.Type)}
	var parts []string
	if kind := shipKind(info.Type); kind != "" {
		parts = append(parts, kind)
	}
	if p.Sog < 102.2 { // 102.3 = not available
		t.SpeedMS = p.Sog * knot
		parts = append(parts, fmt.Sprintf("%.1f kt", p.Sog))
	}
	switch {
	case p.TrueHeading >= 0 && p.TrueHeading < 360:
		t.Course = float64(p.TrueHeading)
	case p.Cog < 360:
		t.Course = p.Cog
	}
	if st := navStatus(p.NavigationalStatus); st != "" {
		parts = append(parts, st)
	}
	if info.Destination != "" {
		parts = append(parts, "to "+info.Destination)
	}
	if info.Length > 0 {
		parts = append(parts, fmt.Sprintf("%d m long", info.Length))
	}
	if info.CallSign != "" {
		parts = append(parts, "Call sign "+info.CallSign)
	}
	parts = append(parts, "MMSI "+id, "AIS via aisstream.io")
	t.Remarks = strings.Join(parts, " · ")
	return t
}

// shipCoT is the CoT type for an AIS ship type: neutral sea surface, by kind.
func shipCoT(t int) string {
	switch {
	case t == 30:
		return "a-n-S-X-F" // fishing
	case t == 36 || t == 37:
		return "a-n-S-X-R" // sailing, pleasure
	case t == 55:
		return "a-n-S-X-L" // law enforcement
	case t >= 60 && t <= 69:
		return "a-n-S-X-M-P" // passenger
	case t >= 70 && t <= 79:
		return "a-n-S-X-M-C" // cargo
	case t >= 80 && t <= 89:
		return "a-n-S-X-M-O" // tanker
	default:
		return "a-n-S-X"
	}
}

func shipKind(t int) string {
	switch {
	case t == 0:
		return ""
	case t >= 20 && t <= 29:
		return "Wing in ground"
	case t == 30:
		return "Fishing"
	case t == 31 || t == 32 || t == 52:
		return "Tug"
	case t == 33:
		return "Dredger"
	case t == 34:
		return "Diving"
	case t == 35:
		return "Military"
	case t == 36:
		return "Sailing"
	case t == 37:
		return "Pleasure craft"
	case t >= 40 && t <= 49:
		return "High-speed craft"
	case t == 50:
		return "Pilot"
	case t == 51:
		return "Search and rescue"
	case t == 53:
		return "Port tender"
	case t == 55:
		return "Law enforcement"
	case t == 58:
		return "Medical"
	case t >= 60 && t <= 69:
		return "Passenger"
	case t >= 70 && t <= 79:
		return "Cargo"
	case t >= 80 && t <= 89:
		return "Tanker"
	default:
		return "Ship type " + strconv.Itoa(t)
	}
}

func navStatus(n int) string {
	return map[int]string{1: "at anchor", 2: "not under command", 3: "restricted manoeuvrability", 5: "moored",
		6: "aground", 7: "fishing", 8: "under way sailing", 14: "AIS-SART active"}[n]
}

// clean trims AIS text (padded with spaces and @).
func clean(s string) string { return strings.TrimSpace(strings.Trim(s, "@ ")) }

func firstNonZero(v ...float64) float64 {
	for _, f := range v {
		if f != 0 {
			return f
		}
	}
	return 0
}
