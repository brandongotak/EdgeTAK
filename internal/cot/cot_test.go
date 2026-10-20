package cot

import (
	"bytes"
	"strings"
	"testing"
)

const atakSA = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><event version="2.0" uid="ANDROID-abc123" type="a-f-G-U-C" time="2026-10-20T12:00:00.000Z" start="2026-10-20T12:00:00.000Z" stale="2026-10-20T12:06:00.000Z" how="m-g"><point lat="38.8977" lon="-77.0365" hae="12.5" ce="9.9" le="9999999.0"/><detail><takv os="34" version="5.5.1.8 (civ)" device="GOOGLE PIXEL 8" platform="ATAK-CIV"/><contact endpoint="*:-1:stcp" callsign="VIPER"/><uid Droid="VIPER"/><precisionlocation altsrc="GPS" geopointsrc="GPS"/><__group role="Team Lead" name="Cyan"/><status battery="87"/><track course="271.5" speed="1.25"/></detail></event>`

func TestParseXMLTypedFields(t *testing.T) {
	ev, err := ParseXML([]byte(atakSA))
	if err != nil {
		t.Fatal(err)
	}
	if ev.UID != "ANDROID-abc123" || ev.Type != "a-f-G-U-C" || ev.Lat != 38.8977 || ev.Hae != 12.5 {
		t.Fatalf("bad header: %+v", ev)
	}
	d := ev.Detail
	if ev.Callsign() != "VIPER" || d.Contact.Endpoint != "*:-1:stcp" {
		t.Errorf("contact = %+v", d.Contact)
	}
	if d.Group == nil || d.Group.Name != "Cyan" || d.Group.Role != "Team Lead" {
		t.Errorf("group = %+v", d.Group)
	}
	if d.Status == nil || d.Status.Battery != 87 || d.Track == nil || d.Track.Course != 271.5 {
		t.Errorf("status/track = %+v %+v", d.Status, d.Track)
	}
	if d.inXML != 0 {
		t.Errorf("inXML = %b, want 0", d.inXML)
	}
	if d.XML != `<uid Droid="VIPER"/>` {
		t.Errorf("residual XML = %q", d.XML)
	}
	if bytes.HasPrefix(ev.XML(), []byte("<?xml")) {
		t.Error("raw XML should not include declaration")
	}
}

func TestContactWithExtraAttrsStaysXML(t *testing.T) {
	ev, err := ParseXML([]byte(`<event version="2.0" uid="u1" type="a-f-G" how="h-e" time="2026-10-20T12:00:00Z" start="2026-10-20T12:00:00Z" stale="2026-10-20T12:01:00Z"><point lat="1" lon="2" hae="0" ce="1" le="1"/><detail><contact callsign="X" phone="555"/></detail></event>`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Callsign() != "X" {
		t.Errorf("callsign = %q", ev.Callsign())
	}
	if ev.Detail.inXML&bitContact == 0 || !strings.Contains(ev.Detail.XML, `phone="555"`) {
		t.Errorf("contact should remain in XML: %q", ev.Detail.XML)
	}
	// Must not be duplicated when re-encoded.
	back, _, err := DecodeTakMessage(ev.TakMessage())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(back.XML()), "<contact") != 1 {
		t.Errorf("contact duplicated: %s", back.XML())
	}
}

func TestProtoRoundTrip(t *testing.T) {
	ev, err := ParseXML([]byte(atakSA))
	if err != nil {
		t.Fatal(err)
	}
	back, _, err := DecodeTakMessage(ev.TakMessage())
	if err != nil {
		t.Fatal(err)
	}
	if back.UID != ev.UID || back.Type != ev.Type || back.Lat != ev.Lat || back.Lon != ev.Lon || back.LE != ev.LE {
		t.Fatalf("header mismatch: %+v", back)
	}
	if !back.Time.Equal(ev.Time) || !back.Stale.Equal(ev.Stale) {
		t.Errorf("times: %v %v", back.Time, back.Stale)
	}
	if back.Callsign() != "VIPER" || back.Detail.Takv.Device != "GOOGLE PIXEL 8" || back.Detail.Status.Battery != 87 {
		t.Errorf("detail mismatch: %+v", back.Detail)
	}
	if back.Detail.XML != ev.Detail.XML {
		t.Errorf("xmlDetail = %q", back.Detail.XML)
	}
	// Proto-sourced event must render as parseable XML equivalent to the source.
	again, err := ParseXML(back.XML())
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, back.XML())
	}
	if again.Callsign() != "VIPER" || again.Detail.Group.Name != "Cyan" || again.Detail.Track.Speed != 1.25 {
		t.Errorf("xml re-render lost data: %s", back.XML())
	}
}

func TestDests(t *testing.T) {
	ev, err := ParseXML([]byte(`<event version="2.0" uid="GeoChat.a.b.c" type="b-t-f" how="h-g-i-g-o" time="2026-10-20T12:00:00Z" start="2026-10-20T12:00:00Z" stale="2026-10-21T12:00:00Z"><point lat="0" lon="0" hae="0" ce="0" le="0"/><detail><__chat id="x" chatroom="BRAVO"/><marti><dest callsign="BRAVO"/><dest uid="ANDROID-2"/></marti><remarks>hi &lt;/event&gt;</remarks></detail></event>`))
	if err != nil {
		t.Fatal(err)
	}
	d := ev.Dests()
	if len(d) != 2 || d[0].Callsign != "BRAVO" || d[1].UID != "ANDROID-2" {
		t.Errorf("dests = %+v", d)
	}
}

func TestReaderMixedStream(t *testing.T) {
	ping := `<event version="2.0" uid="x-ping" type="t-x-c-t" how="h-g-i-g-o" time="2026-10-20T12:00:00Z" start="2026-10-20T12:00:00Z" stale="2026-10-20T12:00:10Z"><point lat="0" lon="0" hae="0" ce="0" le="0"/><detail/></event>`
	sa, _ := ParseXML([]byte(atakSA))
	var stream []byte
	stream = append(stream, "\n"+atakSA+"\r\n"...)
	stream = append(stream, ping...)
	stream = AppendProtoFrame(stream, EncodeTakControl(TakControl{1, 1})) // control-only: skipped
	stream = AppendFrame(stream, sa, FormatProto)
	stream = append(stream, "<event garbage</event>"...)
	stream = append(stream, ping...)

	r := NewReader(bytes.NewReader(stream))
	want := []struct {
		typ string
		f   Format
		bad bool
	}{{"a-f-G-U-C", FormatXML, false}, {"t-x-c-t", FormatXML, false}, {"a-f-G-U-C", FormatProto, false}, {"", FormatXML, true}, {"t-x-c-t", FormatXML, false}}
	for i, w := range want {
		ev, f, err := r.Next()
		if w.bad {
			if _, ok := err.(*MalformedError); !ok {
				t.Fatalf("msg %d: want MalformedError, got %v", i, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
		if ev.Type != w.typ || f != w.f {
			t.Errorf("msg %d: got %s/%v want %s/%v", i, ev.Type, f, w.typ, w.f)
		}
	}
	if _, _, err := r.Next(); err == nil {
		t.Error("expected EOF")
	}
}

func TestTakRequestVersion(t *testing.T) {
	ev, err := ParseXML([]byte(`<event version="2.0" uid="u" type="t-x-takp-q" how="m-g" time="2026-10-20T12:00:00Z" start="2026-10-20T12:00:00Z" stale="2026-10-20T12:01:00Z"><point lat="0" lon="0" hae="0" ce="0" le="0"/><detail><TakControl><TakRequest version="1"/></TakControl></detail></event>`))
	if err != nil {
		t.Fatal(err)
	}
	if v := ev.TakRequestVersion(); v != "1" {
		t.Errorf("version = %q", v)
	}
	ann := ProtocolAnnouncement("srv", `Edge"TAK`)
	if _, err := ParseXML(ann.XML()); err != nil {
		t.Errorf("announcement not valid XML: %v\n%s", err, ann.XML())
	}
}
