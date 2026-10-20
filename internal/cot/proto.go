package cot

import (
	"errors"
	"math"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// Field numbers follow atakmap.commoncommo.protobuf.v1 (takmessage.proto et al).

// TakControl is the protobuf TakControl message.
type TakControl struct{ MinProtoVersion, MaxProtoVersion uint32 }

var errProto = errors.New("cot: malformed protobuf")

// DecodeTakMessage decodes a TakMessage. The event is nil if the message
// carried only control data.
func DecodeTakMessage(b []byte) (*Event, *TakControl, error) {
	var ev *Event
	var ctl *TakControl
	err := walk(b, func(num protowire.Number, typ protowire.Type, v []byte, n uint64) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			ctl = &TakControl{}
			return walk(v, func(num protowire.Number, typ protowire.Type, _ []byte, n uint64) error {
				if typ == protowire.VarintType {
					switch num {
					case 1:
						ctl.MinProtoVersion = uint32(n)
					case 2:
						ctl.MaxProtoVersion = uint32(n)
					}
				}
				return nil
			})
		case num == 2 && typ == protowire.BytesType:
			var err error
			ev, err = decodeCotEvent(v)
			return err
		}
		return nil
	})
	return ev, ctl, err
}

func decodeCotEvent(b []byte) (*Event, error) {
	e := &Event{}
	err := walk(b, func(num protowire.Number, typ protowire.Type, v []byte, n uint64) error {
		switch typ {
		case protowire.BytesType:
			s := string(v)
			switch num {
			case 1:
				e.Type = s
			case 2:
				e.Access = s
			case 3:
				e.QoS = s
			case 4:
				e.Opex = s
			case 5:
				e.UID = s
			case 9:
				e.How = s
			case 15:
				return decodeDetail(v, &e.Detail)
			case 16:
				e.Caveat = s
			case 17:
				e.ReleasableTo = s
			}
		case protowire.VarintType:
			switch num {
			case 6:
				e.Time = msTime(n)
			case 7:
				e.Start = msTime(n)
			case 8:
				e.Stale = msTime(n)
			}
		case protowire.Fixed64Type:
			f := math.Float64frombits(n)
			switch num {
			case 10:
				e.Lat = f
			case 11:
				e.Lon = f
			case 12:
				e.Hae = f
			case 13:
				e.CE = f
			case 14:
				e.LE = f
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if e.UID == "" || e.Type == "" {
		return nil, ErrNotEvent
	}
	e.Detail.scanXML()
	return e, nil
}

func decodeDetail(b []byte, d *Detail) error {
	return walk(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if typ != protowire.BytesType {
			return nil
		}
		switch num {
		case 1:
			d.XML = string(v)
		case 2:
			c := &Contact{}
			d.Contact = c
			return strings2(v, &c.Endpoint, &c.Callsign)
		case 3:
			g := &Group{}
			d.Group = g
			return strings2(v, &g.Name, &g.Role)
		case 4:
			p := &PrecisionLocation{}
			d.PrecisionLocation = p
			return strings2(v, &p.GeoPointSrc, &p.AltSrc)
		case 5:
			s := &Status{}
			d.Status = s
			return walk(v, func(num protowire.Number, typ protowire.Type, _ []byte, n uint64) error {
				if num == 1 && typ == protowire.VarintType {
					s.Battery = uint32(n)
				}
				return nil
			})
		case 6:
			t := &Takv{}
			d.Takv = t
			return walk(v, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
				if typ == protowire.BytesType {
					switch num {
					case 1:
						t.Device = string(v)
					case 2:
						t.Platform = string(v)
					case 3:
						t.OS = string(v)
					case 4:
						t.Version = string(v)
					}
				}
				return nil
			})
		case 7:
			t := &Track{}
			d.Track = t
			return walk(v, func(num protowire.Number, typ protowire.Type, _ []byte, n uint64) error {
				if typ == protowire.Fixed64Type {
					switch num {
					case 1:
						t.Speed = math.Float64frombits(n)
					case 2:
						t.Course = math.Float64frombits(n)
					}
				}
				return nil
			})
		}
		return nil
	})
}

// strings2 decodes a message whose fields 1 and 2 are strings.
func strings2(b []byte, f1, f2 *string) error {
	return walk(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if typ == protowire.BytesType {
			switch num {
			case 1:
				*f1 = string(v)
			case 2:
				*f2 = string(v)
			}
		}
		return nil
	})
}

// walk iterates over the fields of a protobuf message. For bytes fields v is
// set; for varint and fixed fields n holds the raw value.
func walk(b []byte, fn func(num protowire.Number, typ protowire.Type, v []byte, n uint64) error) error {
	for len(b) > 0 {
		num, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return errProto
		}
		b = b[l:]
		var v []byte
		var n uint64
		switch typ {
		case protowire.BytesType:
			v, l = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			n, l = protowire.ConsumeVarint(b)
		case protowire.Fixed64Type:
			n, l = protowire.ConsumeFixed64(b)
		case protowire.Fixed32Type:
			var n32 uint32
			n32, l = protowire.ConsumeFixed32(b)
			n = uint64(n32)
		default:
			l = protowire.ConsumeFieldValue(num, typ, b)
		}
		if l < 0 {
			return errProto
		}
		b = b[l:]
		if err := fn(num, typ, v, n); err != nil {
			return err
		}
	}
	return nil
}

func msTime(ms uint64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(ms)).UTC()
}

func timeMs(t time.Time) uint64 {
	if t.IsZero() {
		t = time.Now()
	}
	return uint64(t.UnixMilli())
}

// ---- encoding ----

func encodeTakMessage(e *Event) []byte {
	ev := encodeCotEvent(e)
	b := protowire.AppendTag(nil, 2, protowire.BytesType)
	return protowire.AppendBytes(b, ev)
}

// EncodeTakControl encodes a TakMessage carrying only TakControl.
func EncodeTakControl(c TakControl) []byte {
	var inner []byte
	inner = appendVarint(inner, 1, uint64(c.MinProtoVersion))
	inner = appendVarint(inner, 2, uint64(c.MaxProtoVersion))
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(b, inner)
}

func encodeCotEvent(e *Event) []byte {
	var b []byte
	b = appendString(b, 1, e.Type)
	b = appendString(b, 2, e.Access)
	b = appendString(b, 3, e.QoS)
	b = appendString(b, 4, e.Opex)
	b = appendString(b, 5, e.UID)
	b = appendVarint(b, 6, timeMs(e.Time))
	b = appendVarint(b, 7, timeMs(e.Start))
	b = appendVarint(b, 8, timeMs(e.Stale))
	b = appendString(b, 9, e.How)
	b = appendDouble(b, 10, e.Lat)
	b = appendDouble(b, 11, e.Lon)
	b = appendDouble(b, 12, e.Hae)
	b = appendDouble(b, 13, e.CE)
	b = appendDouble(b, 14, e.LE)
	if d := encodeDetail(&e.Detail); len(d) > 0 {
		b = appendMessage(b, 15, d)
	}
	b = appendString(b, 16, e.Caveat)
	b = appendString(b, 17, e.ReleasableTo)
	return b
}

func encodeDetail(d *Detail) []byte {
	var b []byte
	b = appendString(b, 1, d.XML)
	if c := d.Contact; c != nil && d.inXML&bitContact == 0 {
		b = appendMessage(b, 2, appendString(appendString(nil, 1, c.Endpoint), 2, c.Callsign))
	}
	if g := d.Group; g != nil && d.inXML&bitGroup == 0 {
		b = appendMessage(b, 3, appendString(appendString(nil, 1, g.Name), 2, g.Role))
	}
	if p := d.PrecisionLocation; p != nil && d.inXML&bitPrecisionLocation == 0 {
		b = appendMessage(b, 4, appendString(appendString(nil, 1, p.GeoPointSrc), 2, p.AltSrc))
	}
	if s := d.Status; s != nil && d.inXML&bitStatus == 0 {
		b = appendMessage(b, 5, appendVarint(nil, 1, uint64(s.Battery)))
	}
	if t := d.Takv; t != nil && d.inXML&bitTakv == 0 {
		var m []byte
		m = appendString(m, 1, t.Device)
		m = appendString(m, 2, t.Platform)
		m = appendString(m, 3, t.OS)
		m = appendString(m, 4, t.Version)
		b = appendMessage(b, 6, m)
	}
	if t := d.Track; t != nil && d.inXML&bitTrack == 0 {
		b = appendMessage(b, 7, appendDouble(appendDouble(nil, 1, t.Speed), 2, t.Course))
	}
	return b
}

func appendString(b []byte, num protowire.Number, s string) []byte {
	if s == "" {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func appendMessage(b []byte, num protowire.Number, m []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, m)
}

func appendVarint(b []byte, num protowire.Number, v uint64) []byte {
	if v == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func appendDouble(b []byte, num protowire.Number, f float64) []byte {
	if f == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.Fixed64Type)
	return protowire.AppendFixed64(b, math.Float64bits(f))
}
