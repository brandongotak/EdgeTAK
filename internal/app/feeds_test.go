package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/websocket"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// TestFeeds covers aircraft and ships: EdgeTAK polls an ADS-B API and reads
// an AIS stream for the area, and devices in the chosen groups get markers.
func TestFeeds(t *testing.T) {
	a := startApp(t)
	call, _ := adminJSON(t, a)
	a.Store.CreateUser("pilot", "pilots-password-1", []string{"Cyan"}, false)

	adsb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ac":[{"hex":"a3a8ef","flight":"FFT3134 ","t":"A20N","alt_baro":3300,"alt_geom":3575,"gs":184.5,"track":31.7,
			"category":"A3","lat":40.65,"lon":-73.97,"seen_pos":0.2}],"now":1}`))
	}))
	defer adsb.Close()
	var sub map[string]any
	gotSub := make(chan struct{}, 1)
	ais := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		if websocket.JSON.Receive(ws, &sub) != nil {
			return
		}
		gotSub <- struct{}{}
		if sub["APIKey"] != "ais-key-1" {
			websocket.Message.Send(ws, `{"error":"Api Key Is Not Valid"}`)
			return
		}
		websocket.Message.Send(ws, `{"MessageType":"ShipStaticData","MetaData":{"MMSI":366999712},"Message":{"ShipStaticData":{"Name":"EVER GIVEN","Type":71}}}`)
		websocket.Message.Send(ws, `{"MessageType":"PositionReport","MetaData":{"MMSI":366999712},"Message":{"PositionReport":{"Cog":215,"Sog":12,"TrueHeading":214,"Latitude":40.61,"Longitude":-74.02}}}`)
		var x string
		websocket.Message.Receive(ws, &x) // stay open
	}))
	defer ais.Close()
	a.Web.Feeds.ADSBURL = func(string, float64, float64, int) string { return adsb.URL }
	a.Web.Feeds.AISURL = "ws" + strings.TrimPrefix(ais.URL, "http")

	cert := enroll(t, a, "pilot", "pilots-password-1")
	dev := mustDial(t, a, &cert)
	dev.send(sa("ANDROID-pilot", "PILOT", "Cyan"))

	// Ships need a key; the area needs a center.
	if code, _ := call("PUT", "/admin/api/feeds", map[string]any{"lat": 0, "lon": 0, "adsb": map[string]any{"enabled": true, "radius_nm": 50}, "ais": map[string]any{"radius_nm": 20}}); code != 400 {
		t.Errorf("no center = %d", code)
	}
	if code, _ := call("PUT", "/admin/api/feeds", map[string]any{"lat": 40.6, "lon": -73.9, "adsb": map[string]any{"radius_nm": 50}, "ais": map[string]any{"enabled": true, "radius_nm": 20}}); code != 400 {
		t.Errorf("no key = %d", code)
	}
	var adsbRadius string
	a.Web.Feeds.ADSBURL = func(_ string, _, _ float64, r int) string { adsbRadius = fmt.Sprint(r); return adsb.URL }
	code, v := call("PUT", "/admin/api/feeds", map[string]any{"lat": 40.6, "lon": -73.9,
		"adsb": map[string]any{"enabled": true, "source": "adsb.lol", "radius_nm": 120, "interval_sec": 5, "groups": []string{"Cyan"}},
		"ais":  map[string]any{"enabled": true, "api_key": "ais-key-1", "radius_nm": 50, "groups": []string{"Cyan"}}})
	if code != 200 || v["has_api_key"] != true || strings.Contains(mustJSON(v), "ais-key-1") {
		t.Fatalf("save = %d %v", code, v)
	}

	// Either may arrive first.
	got := dev.expectAll("the aircraft and the ship",
		func(e *cot.Event) bool { return e.UID == "ICAO-a3a8ef" }, func(e *cot.Event) bool { return e.UID == "MMSI-366999712" })
	plane, ship := got[0], got[1]
	if plane.Type != "a-n-A-C-F" || plane.Callsign() != "FFT3134" || plane.Lat != 40.65 {
		t.Errorf("aircraft event = %s", plane.XML())
	}
	if ship.Type != "a-n-S-X-M-C" || ship.Callsign() != "EVER GIVEN" || !strings.Contains(string(ship.XML()), "Cargo") {
		t.Errorf("ship event = %s", ship.XML())
	}
	// Each feed has its own radius: 120 nm for aircraft, 50 nm (0.83 degrees
	// of latitude) for ships.
	<-gotSub
	if bb, _ := json.Marshal(sub["BoundingBoxes"]); !strings.HasPrefix(string(bb), "[[[39.76") {
		t.Errorf("ships' bounding box = %s", bb)
	}
	if adsbRadius != "120" {
		t.Errorf("aircraft radius = %s", adsbRadius)
	}

	// The console map lists both.
	_, hc := adminJSON(t, a)
	resp, err := hc.Get("https://" + a.Addrs["https"] + "/admin/api/feeds/tracks")
	if err != nil {
		t.Fatal(err)
	}
	var tracks []map[string]any
	json.NewDecoder(resp.Body).Decode(&tracks)
	resp.Body.Close()
	if len(tracks) != 2 {
		t.Errorf("tracks = %v", tracks)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestFeedsCustomADSB covers a custom ADS-B source: an older dump1090
// aircraft.json behind an API-key header, with the area filled into the URL
// and aircraft outside the radius left out.
func TestFeedsCustomADSB(t *testing.T) {
	a := startApp(t)
	call, _ := adminJSON(t, a)
	a.Store.CreateUser("pilot", "pilots-password-1", []string{"Cyan"}, false)
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("X-Api-Key")
		if gotKey != "secret-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"now":1,"aircraft":[
			{"hex":"abc123","flight":"N12AB","lat":40.70,"lon":-73.95,"altitude":4500,"speed":120,"track":90,"seen_pos":1},
			{"hex":"def456","flight":"FARAWAY","lat":51.5,"lon":-0.1,"altitude":30000,"speed":450}]}`))
	}))
	defer srv.Close()

	cert := enroll(t, a, "pilot", "pilots-password-1")
	dev := mustDial(t, a, &cert)
	dev.send(sa("ANDROID-pilot", "PILOT", "Cyan"))

	if code, _ := call("PUT", "/admin/api/feeds", map[string]any{"lat": 40.6, "lon": -73.9,
		"adsb": map[string]any{"enabled": true, "source": "custom", "custom_url": "ftp://x", "radius_nm": 50}, "ais": map[string]any{"radius_nm": 20}}); code != 400 {
		t.Errorf("ftp URL = %d", code)
	}
	code, v := call("PUT", "/admin/api/feeds", map[string]any{"lat": 40.6, "lon": -73.9,
		"adsb": map[string]any{"enabled": true, "source": "custom", "custom_url": srv.URL + "/v2/point/{lat}/{lon}/{radius}",
			"header_name": "X-Api-Key", "header_value": "secret-1", "radius_nm": 50, "interval_sec": 5},
		"ais": map[string]any{"radius_nm": 20}})
	if code != 200 || v["has_header_value"] != true || strings.Contains(mustJSON(v), "secret-1") {
		t.Fatalf("save = %d %v", code, v)
	}
	plane := dev.expect(func(e *cot.Event) bool { return strings.HasPrefix(e.UID, "ICAO-") }, "the aircraft")
	if plane.UID != "ICAO-abc123" || plane.Callsign() != "N12AB" || !strings.Contains(string(plane.XML()), "4500 ft") {
		t.Errorf("aircraft = %s", plane.XML())
	}
	if gotPath != "/v2/point/40.6000/-73.9000/50" {
		t.Errorf("URL path = %q", gotPath)
	}
	dev.expectNone(func(e *cot.Event) bool { return e.UID == "ICAO-def456" }, "the aircraft outside the radius")

	// Saving again without the value keeps it.
	call("PUT", "/admin/api/feeds", map[string]any{"lat": 40.6, "lon": -73.9,
		"adsb": map[string]any{"enabled": true, "source": "custom", "custom_url": srv.URL + "/x", "header_name": "X-Api-Key", "radius_nm": 50},
		"ais":  map[string]any{"radius_nm": 20}})
	if a.Store.Feeds().ADSB.HeaderValue != "secret-1" {
		t.Error("the header value was lost")
	}
}
