package federation

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// Conversion between EdgeTAK's CoT events and federation GeoEvents, as TAK
// Server's ProtoBufHelper does (and the OTS federation plugin, which was
// checked against TAK Server 5.6).

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// toFederated converts a CoT event for federates.
func toFederated(ev *cot.Event, groups []string, prov []Provenance) *FederatedEvent {
	detail, track, status, ploc := ev.FederationDetail()
	g := &GeoEvent{
		UID: ev.UID, Type: ev.Type, CoordSource: ev.How,
		SendTime: ms(ev.Time), StartTime: ms(ev.Start), StaleTime: ms(ev.Stale),
		Lat: ev.Lat, Lon: ev.Lon, Hae: ev.Hae, CE: ev.CE, LE: ev.LE,
		Access: ev.Access, Caveat: ev.Caveat, ReleaseableTo: ev.ReleasableTo,
		Other: detail,
	}
	if track != nil {
		g.Speed, g.Course = track.Speed, track.Course
	}
	if status != nil {
		g.Battery = int32(status.Battery)
	}
	if ploc != nil {
		g.Ploc, g.Palt = ploc.GeoPointSrc, ploc.AltSrc
	}
	if c := ev.Detail.Contact; c != nil {
		g.ScreenName = c.Callsign
	}
	if gr := ev.Detail.Group; gr != nil {
		g.GroupName, g.GroupRole = gr.Name, gr.Role
	}
	for _, d := range ev.Dests() {
		if d.Callsign != "" {
			g.PtpCallsigns = append(g.PtpCallsigns, d.Callsign)
		}
		if d.UID != "" {
			g.PtpUIDs = append(g.PtpUIDs, d.UID)
		}
		if d.Mission != "" {
			g.MissionNames = append(g.MissionNames, d.Mission)
		}
	}
	return &FederatedEvent{Event: g, FederateGroups: groups, Provenance: prov}
}

func escAttr(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func timeAttr(msec, fallback int64) string {
	if msec == 0 {
		msec = fallback
	}
	if msec == 0 {
		msec = time.Now().UnixMilli()
	}
	return time.UnixMilli(msec).UTC().Format(cot.TimeLayout)
}

func floatAttr(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// fromFederated converts a federated event into a CoT event. It returns nil
// for events without a GeoEvent, uid or type.
func fromFederated(fe *FederatedEvent) (*cot.Event, error) {
	g := fe.Event
	if g == nil || g.UID == "" || g.Type == "" {
		return nil, nil
	}
	send := g.SendTime
	if send == 0 {
		send = g.StartTime
	}
	var b strings.Builder
	b.WriteString(`<event version="2.0" uid="` + escAttr(g.UID) + `" type="` + escAttr(g.Type) + `"`)
	how := g.CoordSource
	if how == "" {
		how = "h-g-i-g-o"
	}
	b.WriteString(` how="` + escAttr(how) + `" time="` + timeAttr(send, 0) + `" start="` + timeAttr(g.StartTime, send) +
		`" stale="` + timeAttr(g.StaleTime, send) + `"`)
	for _, kv := range [][2]string{{"access", g.Access}, {"caveat", g.Caveat}, {"releaseableTo", g.ReleaseableTo}} {
		if kv[1] != "" {
			b.WriteString(` ` + kv[0] + `="` + escAttr(kv[1]) + `"`)
		}
	}
	b.WriteString(`><point lat="` + floatAttr(g.Lat) + `" lon="` + floatAttr(g.Lon) + `" hae="` + floatAttr(g.Hae) +
		`" ce="` + floatAttr(g.CE) + `" le="` + floatAttr(g.LE) + `"/><detail>`)

	// Other is usually "<detail>…</detail>"; keep what is inside.
	inner := strings.TrimSpace(g.Other)
	if strings.HasPrefix(inner, "<detail") {
		if i := strings.Index(inner, ">"); i >= 0 {
			if strings.HasSuffix(inner[:i+1], "/>") {
				inner = ""
			} else {
				inner = strings.TrimSuffix(strings.TrimSpace(inner[i+1:]), "</detail>")
			}
		}
	}
	b.WriteString(inner)
	has := func(tag string) bool {
		return strings.Contains(inner, "<"+tag+" ") || strings.Contains(inner, "<"+tag+">") || strings.Contains(inner, "<"+tag+"/")
	}
	// Only add what the XML does not already have.
	if !has("contact") && g.ScreenName != "" {
		b.WriteString(`<contact callsign="` + escAttr(g.ScreenName) + `"`)
		if g.Phone != "" {
			b.WriteString(` phone="` + escAttr(g.Phone) + `"`)
		}
		b.WriteString(`/>`)
	}
	if !has("__group") && g.GroupName != "" {
		b.WriteString(`<__group name="` + escAttr(g.GroupName) + `" role="` + escAttr(g.GroupRole) + `"/>`)
	}
	if !has("status") && g.Battery != 0 {
		b.WriteString(`<status battery="` + strconv.Itoa(int(g.Battery)) + `"/>`)
	}
	if !has("track") && (g.Speed != 0 || g.Course != 0) {
		b.WriteString(`<track speed="` + floatAttr(g.Speed) + `" course="` + floatAttr(g.Course) + `"/>`)
	}
	if !has("precisionlocation") && (g.Ploc != "" || g.Palt != "") {
		b.WriteString(`<precisionlocation geopointsrc="` + escAttr(g.Ploc) + `" altsrc="` + escAttr(g.Palt) + `"/>`)
	}
	// TAK Server routes direct messages on ptpUids/ptpCallsigns; EdgeTAK's
	// router on <marti><dest>, so add them when the XML has none.
	if !has("marti") && len(g.PtpUIDs)+len(g.PtpCallsigns) > 0 {
		b.WriteString(`<marti>`)
		for _, u := range g.PtpUIDs {
			b.WriteString(`<dest uid="` + escAttr(u) + `"/>`)
		}
		for _, c := range g.PtpCallsigns {
			b.WriteString(`<dest callsign="` + escAttr(c) + `"/>`)
		}
		b.WriteString(`</marti>`)
	}
	b.WriteString(`</detail></event>`)
	return cot.ParseXML([]byte(b.String()))
}

// isContactSA reports whether ev is a device's position report, which makes
// it a contact (TAK Server only counts CoT with a <contact endpoint>).
func isContactSA(ev *cot.Event) bool {
	c := ev.Detail.Contact
	return strings.HasPrefix(ev.Type, "a-") && ev.UID != "" && c != nil && c.Callsign != "" && c.Endpoint != ""
}

// deleteEvent is the CoT servers send when a user leaves, so devices drop
// the contact.
func deleteEvent(uid, linkType string) (*cot.Event, error) {
	now := time.Now().UTC()
	x := `<event version="2.0" uid="` + escAttr(uuidString()) + `" type="t-x-d-d" how="h-g-i-g-o" time="` + now.Format(cot.TimeLayout) +
		`" start="` + now.Format(cot.TimeLayout) + `" stale="` + now.Add(20*time.Second).Format(cot.TimeLayout) + `">` +
		`<point lat="0" lon="0" hae="0" ce="9999999" le="9999999"/><detail><link relation="p-p" uid="` + escAttr(uid) +
		`" type="` + escAttr(linkType) + `"/></detail></event>`
	return cot.ParseXML([]byte(x))
}
