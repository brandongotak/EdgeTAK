// Package cot models Cursor-on-Target events and converts them between the
// CoT XML encoding and the TAK protocol v1 protobuf encoding.
package cot

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TimeLayout is the CoT timestamp layout used when emitting XML.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// Unknown is the CoT sentinel for unknown hae/ce/le values.
const Unknown = 9999999.0

type Contact struct{ Endpoint, Callsign string }
type Group struct{ Name, Role string }
type PrecisionLocation struct{ GeoPointSrc, AltSrc string }
type Status struct{ Battery uint32 }
type Takv struct{ Device, Platform, OS, Version string }
type Track struct{ Speed, Course float64 }

// Dest is a <marti><dest/></marti> entry used for directed delivery.
type Dest struct{ UID, Callsign, Mission string }

// Detail mirrors the TAK protobuf Detail message. The typed fields are always
// populated when the element exists (for convenient reading); elements that
// carry data the typed form cannot represent stay in XML and are flagged so
// they are not emitted twice.
type Detail struct {
	XML               string // detail child elements not carried by typed fields
	Contact           *Contact
	Group             *Group
	PrecisionLocation *PrecisionLocation
	Status            *Status
	Takv              *Takv
	Track             *Track

	inXML uint8 // bitmask of typed elements that live in XML instead
	dests []Dest
}

const (
	bitContact uint8 = 1 << iota
	bitGroup
	bitPrecisionLocation
	bitStatus
	bitTakv
	bitTrack
)

// Event is a single CoT event.
type Event struct {
	UID, Type, How                          string
	Access, QoS, Opex, Caveat, ReleasableTo string
	Time, Start, Stale                      time.Time
	Lat, Lon, Hae, CE, LE                   float64
	Detail                                  Detail

	mu       sync.Mutex
	raw      []byte // original XML, forwarded verbatim to XML clients
	xmlBuf   []byte
	protoBuf []byte
}

// NewEvent creates a server-originated event stamped with the current time.
func NewEvent(typ, uid, how string, staleAfter time.Duration) *Event {
	now := time.Now().UTC()
	return &Event{
		UID: uid, Type: typ, How: how,
		Time: now, Start: now, Stale: now.Add(staleAfter),
		Hae: Unknown, CE: Unknown, LE: Unknown,
	}
}

// Changed must be called after mutating an event so cached encodings are rebuilt.
func (e *Event) Changed() {
	e.mu.Lock()
	e.raw, e.xmlBuf, e.protoBuf = nil, nil, nil
	e.mu.Unlock()
	e.Detail.scanXML()
}

// Callsign returns the contact callsign, if any.
func (e *Event) Callsign() string {
	if e.Detail.Contact != nil {
		return e.Detail.Contact.Callsign
	}
	return ""
}

// Dests returns the <marti><dest/> entries of the event.
func (e *Event) Dests() []Dest { return e.Detail.dests }

// IsStale reports whether the event's stale time has passed.
func (e *Event) IsStale(now time.Time) bool { return !e.Stale.IsZero() && now.After(e.Stale) }

// XML returns the event encoded as CoT XML (without an XML declaration).
func (e *Event) XML() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.raw != nil {
		return e.raw
	}
	if e.xmlBuf == nil {
		e.xmlBuf = e.marshalXML()
	}
	return e.xmlBuf
}

// TakMessage returns the event encoded as a TAK protobuf TakMessage.
func (e *Event) TakMessage() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.protoBuf == nil {
		e.protoBuf = encodeTakMessage(e)
	}
	return e.protoBuf
}

// ---- XML decoding ----

type xmlEvent struct {
	XMLName      xml.Name `xml:"event"`
	UID          string   `xml:"uid,attr"`
	Type         string   `xml:"type,attr"`
	How          string   `xml:"how,attr"`
	Time         string   `xml:"time,attr"`
	Start        string   `xml:"start,attr"`
	Stale        string   `xml:"stale,attr"`
	Access       string   `xml:"access,attr"`
	QoS          string   `xml:"qos,attr"`
	Opex         string   `xml:"opex,attr"`
	Caveat       string   `xml:"caveat,attr"`
	ReleasableTo string   `xml:"releaseableTo,attr"`
	Point        *struct {
		Lat string `xml:"lat,attr"`
		Lon string `xml:"lon,attr"`
		Hae string `xml:"hae,attr"`
		CE  string `xml:"ce,attr"`
		LE  string `xml:"le,attr"`
	} `xml:"point"`
	Detail *struct {
		Inner []byte `xml:",innerxml"`
	} `xml:"detail"`
}

// ErrNotEvent is returned when XML does not contain a CoT <event>.
var ErrNotEvent = errors.New("cot: not an event")

// ParseXML parses a single CoT XML event. The input is retained as the
// event's raw encoding, so callers must not reuse the slice.
func ParseXML(data []byte) (*Event, error) {
	var x xmlEvent
	if err := xml.Unmarshal(data, &x); err != nil {
		return nil, fmt.Errorf("cot: %w", err)
	}
	if x.UID == "" || x.Type == "" {
		return nil, ErrNotEvent
	}
	e := &Event{
		UID: x.UID, Type: x.Type, How: x.How,
		Access: x.Access, QoS: x.QoS, Opex: x.Opex, Caveat: x.Caveat, ReleasableTo: x.ReleasableTo,
		Time: parseTime(x.Time), Start: parseTime(x.Start), Stale: parseTime(x.Stale),
		Hae: Unknown, CE: Unknown, LE: Unknown,
	}
	if p := x.Point; p != nil {
		e.Lat = parseFloat(p.Lat, 0)
		e.Lon = parseFloat(p.Lon, 0)
		e.Hae = parseFloat(p.Hae, Unknown)
		e.CE = parseFloat(p.CE, Unknown)
		e.LE = parseFloat(p.LE, Unknown)
	}
	if x.Detail != nil {
		if err := e.Detail.splitXML(x.Detail.Inner); err != nil {
			return nil, err
		}
	}
	if i := bytes.Index(data, []byte("<event")); i > 0 {
		data = data[i:] // drop any XML declaration
	}
	e.raw = data
	return e, nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func parseFloat(s string, def float64) float64 {
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return def
	}
	return f
}

// splitXML separates the detail's top-level elements into typed fields (when
// they can be represented losslessly) and the residual XML.
func (d *Detail) splitXML(inner []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(inner))
	var rest bytes.Buffer
	for {
		start := dec.InputOffset()
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("cot: detail: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue // whitespace, comments, etc. between elements
		}
		se = se.Copy()
		hasChildren, err := consumeElement(dec)
		if err != nil {
			return fmt.Errorf("cot: detail: %w", err)
		}
		elem := inner[start:dec.InputOffset()]
		if hasChildren || !d.setTyped(se) {
			rest.Write(elem)
		}
	}
	d.XML = rest.String()
	d.scanXML()
	return nil
}

// consumeElement reads to the end of the element just started and reports
// whether it had child elements or non-whitespace text.
func consumeElement(dec *xml.Decoder) (bool, error) {
	depth, children := 1, false
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return false, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			children = true
		case xml.EndElement:
			depth--
		case xml.CharData:
			if len(bytes.TrimSpace(t)) > 0 {
				children = true
			}
		}
	}
	return children, nil
}

// setTyped stores se in a typed field if every attribute is representable.
// It returns false if the element must remain in the XML part.
func (d *Detail) setTyped(se xml.StartElement) bool {
	attrs := map[string]string{}
	for _, a := range se.Attr {
		attrs[a.Name.Local] = a.Value
	}
	only := func(allowed ...string) bool {
		if len(attrs) > len(allowed) {
			return false
		}
		for k := range attrs {
			found := false
			for _, a := range allowed {
				if k == a {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	switch se.Name.Local {
	case "contact":
		if d.Contact != nil || !only("endpoint", "callsign") {
			return false
		}
		d.Contact = &Contact{Endpoint: attrs["endpoint"], Callsign: attrs["callsign"]}
	case "__group":
		if d.Group != nil || !only("name", "role") {
			return false
		}
		d.Group = &Group{Name: attrs["name"], Role: attrs["role"]}
	case "precisionlocation":
		if d.PrecisionLocation != nil || !only("geopointsrc", "altsrc") {
			return false
		}
		d.PrecisionLocation = &PrecisionLocation{GeoPointSrc: attrs["geopointsrc"], AltSrc: attrs["altsrc"]}
	case "status":
		if d.Status != nil || !only("battery") {
			return false
		}
		b, err := strconv.ParseUint(attrs["battery"], 10, 32)
		if err != nil {
			return false
		}
		d.Status = &Status{Battery: uint32(b)}
	case "takv":
		if d.Takv != nil || !only("device", "platform", "os", "version") {
			return false
		}
		d.Takv = &Takv{Device: attrs["device"], Platform: attrs["platform"], OS: attrs["os"], Version: attrs["version"]}
	case "track":
		if d.Track != nil || !only("speed", "course") {
			return false
		}
		sp, err1 := strconv.ParseFloat(attrs["speed"], 64)
		co, err2 := strconv.ParseFloat(attrs["course"], 64)
		if err1 != nil || err2 != nil {
			return false
		}
		d.Track = &Track{Speed: sp, Course: co}
	default:
		return false
	}
	return true
}

// scanXML fills typed fields for read access from elements that live in the
// XML part, and extracts <marti> destinations.
func (d *Detail) scanXML() {
	d.dests = nil
	if d.XML == "" {
		return
	}
	dec := xml.NewDecoder(strings.NewReader(d.XML))
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			attr := func(name string) string {
				for _, a := range t.Attr {
					if a.Name.Local == name {
						return a.Value
					}
				}
				return ""
			}
			if depth == 1 {
				switch t.Name.Local {
				case "contact":
					if d.Contact == nil {
						d.Contact = &Contact{Endpoint: attr("endpoint"), Callsign: attr("callsign")}
						d.inXML |= bitContact
					}
				case "__group":
					if d.Group == nil {
						d.Group = &Group{Name: attr("name"), Role: attr("role")}
						d.inXML |= bitGroup
					}
				case "takv":
					if d.Takv == nil {
						d.Takv = &Takv{Device: attr("device"), Platform: attr("platform"), OS: attr("os"), Version: attr("version")}
						d.inXML |= bitTakv
					}
				case "status":
					if d.Status == nil {
						if b, err := strconv.ParseUint(attr("battery"), 10, 32); err == nil {
							d.Status = &Status{Battery: uint32(b)}
							d.inXML |= bitStatus
						}
					}
				}
			} else if depth == 2 && t.Name.Local == "dest" {
				d.dests = append(d.dests, Dest{UID: attr("uid"), Callsign: attr("callsign"), Mission: attr("mission")})
			}
		case xml.EndElement:
			depth--
		}
	}
}

// TakRequestVersion returns the version from a t-x-takp-q TakRequest, or "".
func (e *Event) TakRequestVersion() string {
	var tc struct {
		TakRequest struct {
			Version string `xml:"version,attr"`
		} `xml:"TakRequest"`
	}
	i := strings.Index(e.Detail.XML, "<TakControl")
	if i < 0 {
		return ""
	}
	if err := xml.Unmarshal([]byte(e.Detail.XML[i:]), &tc); err != nil {
		return ""
	}
	return tc.TakRequest.Version
}

// ---- XML encoding ----

func (e *Event) marshalXML() []byte {
	var b bytes.Buffer
	b.WriteString(`<event version="2.0"`)
	attr(&b, "uid", e.UID)
	attr(&b, "type", e.Type)
	for _, kv := range [][2]string{{"access", e.Access}, {"qos", e.QoS}, {"opex", e.Opex}, {"caveat", e.Caveat}, {"releaseableTo", e.ReleasableTo}} {
		if kv[1] != "" {
			attr(&b, kv[0], kv[1])
		}
	}
	attr(&b, "time", fmtTime(e.Time))
	attr(&b, "start", fmtTime(e.Start))
	attr(&b, "stale", fmtTime(e.Stale))
	attr(&b, "how", e.How)
	b.WriteString(`><point`)
	attr(&b, "lat", fmtFloat(e.Lat))
	attr(&b, "lon", fmtFloat(e.Lon))
	attr(&b, "hae", fmtFloat(e.Hae))
	attr(&b, "ce", fmtFloat(e.CE))
	attr(&b, "le", fmtFloat(e.LE))
	b.WriteString(`/><detail>`)
	d := &e.Detail
	if c := d.Contact; c != nil && d.inXML&bitContact == 0 {
		b.WriteString(`<contact`)
		if c.Endpoint != "" {
			attr(&b, "endpoint", c.Endpoint)
		}
		attr(&b, "callsign", c.Callsign)
		b.WriteString(`/>`)
	}
	if g := d.Group; g != nil && d.inXML&bitGroup == 0 {
		b.WriteString(`<__group`)
		attr(&b, "name", g.Name)
		attr(&b, "role", g.Role)
		b.WriteString(`/>`)
	}
	if p := d.PrecisionLocation; p != nil && d.inXML&bitPrecisionLocation == 0 {
		b.WriteString(`<precisionlocation`)
		if p.GeoPointSrc != "" {
			attr(&b, "geopointsrc", p.GeoPointSrc)
		}
		if p.AltSrc != "" {
			attr(&b, "altsrc", p.AltSrc)
		}
		b.WriteString(`/>`)
	}
	if s := d.Status; s != nil && d.inXML&bitStatus == 0 {
		b.WriteString(`<status`)
		attr(&b, "battery", strconv.FormatUint(uint64(s.Battery), 10))
		b.WriteString(`/>`)
	}
	if t := d.Takv; t != nil && d.inXML&bitTakv == 0 {
		b.WriteString(`<takv`)
		attr(&b, "device", t.Device)
		attr(&b, "platform", t.Platform)
		attr(&b, "os", t.OS)
		attr(&b, "version", t.Version)
		b.WriteString(`/>`)
	}
	if t := d.Track; t != nil && d.inXML&bitTrack == 0 {
		b.WriteString(`<track`)
		attr(&b, "speed", fmtFloat(t.Speed))
		attr(&b, "course", fmtFloat(t.Course))
		b.WriteString(`/>`)
	}
	b.WriteString(d.XML)
	b.WriteString(`</detail></event>`)
	return b.Bytes()
}

func attr(b *bytes.Buffer, name, value string) {
	b.WriteByte(' ')
	b.WriteString(name)
	b.WriteString(`="`)
	xml.EscapeText(b, []byte(value))
	b.WriteByte('"')
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format(TimeLayout)
}

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
