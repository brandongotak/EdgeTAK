package feeds

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

func f(v float64) *float64 { return &v }

func TestAircraftTrack(t *testing.T) {
	tr, ok := aircraftTrack(aircraft{Hex: "A3A8EF", Flight: "FFT3134 ", Reg: "N335FR", ICAOType: "A20N", AltBaro: 3300.0, AltGeom: f(3575),
		GS: f(184.5), Track: f(31.7), Squawk: "1760", Emergency: "none", Category: "A3", Lat: f(40.65), Lon: f(-73.97), SeenPos: f(0.2)}, "adsb.lol")
	if !ok || tr.UID != "ICAO-a3a8ef" || tr.Callsign != "FFT3134" || tr.Type != "a-n-A-C-F" || tr.Course != 31.7 {
		t.Fatalf("airliner = %+v", tr)
	}
	if int(tr.AltM) != 1089 || tr.SpeedMS < 94.8 || tr.SpeedMS > 95 || !strings.Contains(tr.Remarks, "3575 ft") || !strings.Contains(tr.Remarks, "Squawk 1760") {
		t.Errorf("airliner numbers = %+v", tr)
	}
	// A military helicopter on the ground with an emergency, known only by registration.
	tr, _ = aircraftTrack(aircraft{Hex: "ae1234", Reg: "12-345", Category: "A7", DBFlags: 1, AltBaro: "ground", Emergency: "general",
		Lat: f(1), Lon: f(2)}, "adsb.fi")
	if tr.Type != "a-n-A-M-H" || tr.Callsign != "12-345" || !strings.Contains(tr.Remarks, "on the ground") || !strings.Contains(tr.Remarks, "EMERGENCY: general") {
		t.Errorf("helicopter = %+v", tr)
	}
	if _, ok := aircraftTrack(aircraft{Hex: "abc", Lat: f(1), Lon: f(2), SeenPos: f(120)}, "x"); ok {
		t.Error("an old position was used")
	}
	if _, ok := aircraftTrack(aircraft{Hex: "abc"}, "x"); ok {
		t.Error("an aircraft without a position was used")
	}
}

func TestAISMessages(t *testing.T) {
	s := &Service{}
	ships := map[int64]shipInfo{}
	msg := func(js string) aisMessage {
		var m aisMessage
		if err := json.Unmarshal([]byte(js), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Static data first: no marker, but remembered.
	if _, ok := s.aisMessageTrack(msg(`{"MessageType":"ShipStaticData","MetaData":{"MMSI":366999712,"ShipName":"EVER GIVEN@@@"},
		"Message":{"ShipStaticData":{"Name":"EVER GIVEN@@@","CallSign":"H3RC ","Type":71,"Destination":"NEW YORK",
		"Dimension":{"A":300,"B":100,"C":30,"D":29}}}}`), ships); ok {
		t.Error("static data made a marker")
	}
	tr, ok := s.aisMessageTrack(msg(`{"MessageType":"PositionReport","MetaData":{"MMSI":366999712,"ShipName":"EVER GIVEN","latitude":40.6,"longitude":-74.0},
		"Message":{"PositionReport":{"Cog":215.3,"Sog":12.4,"TrueHeading":214,"Latitude":40.61,"Longitude":-74.02,"NavigationalStatus":0}}}`), ships)
	if !ok || tr.UID != "MMSI-366999712" || tr.Callsign != "EVER GIVEN" || tr.Type != "a-n-S-X-M-C" || tr.Course != 214 || tr.Lat != 40.61 {
		t.Fatalf("ship = %+v", tr)
	}
	for _, want := range []string{"Cargo", "12.4 kt", "to NEW YORK", "400 m long", "Call sign H3RC"} {
		if !strings.Contains(tr.Remarks, want) {
			t.Errorf("remarks %q lack %q", tr.Remarks, want)
		}
	}
	// A class B boat never sends static data: its MMSI names it; no heading
	// (511) falls back to the course.
	tr, _ = s.aisMessageTrack(msg(`{"MessageType":"StandardClassBPositionReport","MetaData":{"MMSI":338123456},
		"Message":{"StandardClassBPositionReport":{"Cog":90,"Sog":102.3,"TrueHeading":511,"Latitude":41,"Longitude":-71}}}`), ships)
	if tr.Callsign != "MMSI 338123456" || tr.Type != "a-n-S-X" || tr.Course != 90 || tr.SpeedMS != 0 {
		t.Errorf("class B = %+v", tr)
	}
}

// TestCustomADSBKey covers sending a custom source's API key: in x-api-key
// by default, in a named header, or in the URL with {key}; and keeping it
// out of error messages.
func TestCustomADSBKey(t *testing.T) {
	var gotHeader, gotNamed, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader, gotNamed, gotQuery = r.Header.Get("X-Api-Key"), r.Header.Get("Api-Auth"), r.URL.Query().Get("apikey")
		w.Write([]byte(`{"ac":[]}`))
	}))
	defer srv.Close()
	s := &Service{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	set := store.FeedSettings{Lat: 1, Lon: 2}
	set.ADSB.Source, set.ADSB.RadiusNM, set.ADSB.HeaderValue = "custom", 10, "k3y/+secret"

	set.ADSB.CustomURL = srv.URL + "/a"
	if _, err := s.fetchAircraft(context.Background(), set); err != nil || gotHeader != "k3y/+secret" {
		t.Errorf("default header: %v %q", err, gotHeader)
	}
	set.ADSB.HeaderName = "api-auth"
	if _, err := s.fetchAircraft(context.Background(), set); err != nil || gotNamed != "k3y/+secret" || gotHeader != "" {
		t.Errorf("named header: %v %q %q", err, gotNamed, gotHeader)
	}
	set.ADSB.CustomURL = srv.URL + "/a?apikey={key}"
	if _, err := s.fetchAircraft(context.Background(), set); err != nil || gotQuery != "k3y/+secret" || gotNamed != "" {
		t.Errorf("key in the URL: %v %q %q", err, gotQuery, gotNamed)
	}
	// A failed request names the URL, but not the key.
	set.ADSB.CustomURL = "http://127.0.0.1:1/a?apikey={key}"
	_, err := s.fetchAircraft(context.Background(), set)
	if err == nil || strings.Contains(err.Error(), "k3y") || !strings.Contains(err.Error(), "[API key]") {
		t.Errorf("error = %v", err)
	}
}
