// Package federation speaks TAK Server Federation v2: the gRPC service
// FederatedChannel from TAK Server's fig.proto, over mutual TLS (port 9001)
// or with a federation token (port 9002). EdgeTAK can connect to a TAK
// Server (or another EdgeTAK) and accept their connections, and exchanges
// CoT and contact lists with them. To the other side it looks like a TAK
// Server.
//
// The messages are encoded by hand with protowire, like EdgeTAK's CoT
// protobuf, so no generated code is needed. Field numbers follow fig.proto.
package federation

import (
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

// message is implemented by every fig.proto message EdgeTAK sends or reads.
type message interface {
	marshal(b []byte) []byte
	unmarshal(b []byte) error
}

// CRUD operations of a ContactListEntry.
const (
	opCreate = 1
	opUpdate = 3
	opDelete = 4
)

// Identity.ConnectionType
const (
	identityTakServer = 2 // FEDERATION_TAK_SERVER
	identityTakClient = 3 // FEDERATION_TAK_CLIENT
)

// ServingStatus of ClientHealth and ServerHealth.
const (
	statusServing      = 1
	statusNotConnected = 3
)

// ---- encoding helpers ----

func appendString(b []byte, num protowire.Number, s string) []byte {
	if s == "" {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func appendBytes(b []byte, num protowire.Number, v []byte) []byte {
	if len(v) == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func appendVarint(b []byte, num protowire.Number, v int64) []byte {
	if v == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, uint64(v))
}

func appendDouble(b []byte, num protowire.Number, v float64) []byte {
	if v == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.Fixed64Type)
	return protowire.AppendFixed64(b, math.Float64bits(v))
}

func appendMessage(b []byte, num protowire.Number, m message) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, m.marshal(nil))
}

// field is one decoded field: Value for varint and fixed fields, Bytes for
// length-delimited ones.
type field struct {
	Num   protowire.Number
	Type  protowire.Type
	Value uint64
	Bytes []byte
}

// eachField calls fn for every field of a message.
func eachField(b []byte, fn func(f field) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		f := field{Num: num, Type: typ}
		switch typ {
		case protowire.VarintType:
			f.Value, n = protowire.ConsumeVarint(b)
		case protowire.Fixed64Type:
			f.Value, n = protowire.ConsumeFixed64(b)
		case protowire.Fixed32Type:
			var v uint32
			v, n = protowire.ConsumeFixed32(b)
			f.Value = uint64(v)
		case protowire.BytesType:
			f.Bytes, n = protowire.ConsumeBytes(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

func (f field) str() string       { return string(f.Bytes) }
func (f field) double() float64   { return math.Float64frombits(f.Value) }
func (f field) int64() int64      { return int64(f.Value) }
func (f field) copyBytes() []byte { return append([]byte(nil), f.Bytes...) }

// ---- messages ----

// Empty is google's empty message, used by several RPCs.
type Empty struct{}

func (*Empty) marshal(b []byte) []byte  { return b }
func (*Empty) unmarshal(b []byte) error { return nil }

// Raw carries a message EdgeTAK does not read (binary blobs), so the
// stream that carries them can be kept open and drained.
type Raw struct{ Data []byte }

func (m *Raw) marshal(b []byte) []byte  { return append(b, m.Data...) }
func (m *Raw) unmarshal(b []byte) error { m.Data = append([]byte(nil), b...); return nil }

// Provenance records a server an event has passed through (loop prevention).
type Provenance struct {
	ServerID   string
	ServerName string
}

func (m *Provenance) marshal(b []byte) []byte {
	b = appendString(b, 1, m.ServerID)
	return appendString(b, 2, m.ServerName)
}

func (m *Provenance) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		switch f.Num {
		case 1:
			m.ServerID = f.str()
		case 2:
			m.ServerName = f.str()
		}
		return nil
	})
}

// GeoEvent is a CoT event as TAK Server federates it: the common fields
// broken out, the rest of the <detail> as XML in Other.
type GeoEvent struct {
	SendTime, StartTime, StaleTime int64 // milliseconds since the epoch
	Lat, Lon, Hae, CE, LE          float64
	UID, Type, CoordSource, Other  string
	Battery                        int32
	Ploc, Palt                     string
	ScreenName, GroupName          string
	GroupRole, Phone               string
	Speed, Course                  float64
	PtpUIDs, PtpCallsigns          []string
	FeedUID                        string
	MissionNames                   []string
	Access, Caveat, ReleaseableTo  string
	MissionGUIDs                   []string
	extra                          []byte // binary (22) and bloads (30), passed through unread
}

func (m *GeoEvent) marshal(b []byte) []byte {
	b = appendVarint(b, 1, m.SendTime)
	b = appendVarint(b, 2, m.StartTime)
	b = appendVarint(b, 3, m.StaleTime)
	b = appendDouble(b, 4, m.Lat)
	b = appendDouble(b, 5, m.Lon)
	b = appendDouble(b, 6, m.Hae)
	b = appendDouble(b, 7, m.CE)
	b = appendDouble(b, 8, m.LE)
	b = appendString(b, 9, m.UID)
	b = appendString(b, 10, m.Type)
	b = appendString(b, 11, m.CoordSource)
	b = appendString(b, 12, m.Other)
	b = appendVarint(b, 13, int64(m.Battery))
	b = appendString(b, 14, m.Ploc)
	b = appendString(b, 15, m.Palt)
	b = appendString(b, 16, m.ScreenName)
	b = appendString(b, 17, m.GroupName)
	b = appendString(b, 18, m.GroupRole)
	b = appendString(b, 19, m.Phone)
	b = appendDouble(b, 20, m.Speed)
	b = appendDouble(b, 21, m.Course)
	for _, s := range m.PtpUIDs {
		b = protowire.AppendTag(b, 23, protowire.BytesType)
		b = protowire.AppendString(b, s)
	}
	for _, s := range m.PtpCallsigns {
		b = protowire.AppendTag(b, 24, protowire.BytesType)
		b = protowire.AppendString(b, s)
	}
	b = appendString(b, 25, m.FeedUID)
	for _, s := range m.MissionNames {
		b = protowire.AppendTag(b, 26, protowire.BytesType)
		b = protowire.AppendString(b, s)
	}
	b = appendString(b, 27, m.Access)
	b = appendString(b, 28, m.Caveat)
	b = appendString(b, 29, m.ReleaseableTo)
	for _, s := range m.MissionGUIDs {
		b = protowire.AppendTag(b, 31, protowire.BytesType)
		b = protowire.AppendString(b, s)
	}
	return append(b, m.extra...)
}

func (m *GeoEvent) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		switch f.Num {
		case 1:
			m.SendTime = f.int64()
		case 2:
			m.StartTime = f.int64()
		case 3:
			m.StaleTime = f.int64()
		case 4:
			m.Lat = f.double()
		case 5:
			m.Lon = f.double()
		case 6:
			m.Hae = f.double()
		case 7:
			m.CE = f.double()
		case 8:
			m.LE = f.double()
		case 9:
			m.UID = f.str()
		case 10:
			m.Type = f.str()
		case 11:
			m.CoordSource = f.str()
		case 12:
			m.Other = f.str()
		case 13:
			m.Battery = int32(f.Value)
		case 14:
			m.Ploc = f.str()
		case 15:
			m.Palt = f.str()
		case 16:
			m.ScreenName = f.str()
		case 17:
			m.GroupName = f.str()
		case 18:
			m.GroupRole = f.str()
		case 19:
			m.Phone = f.str()
		case 20:
			m.Speed = f.double()
		case 21:
			m.Course = f.double()
		case 22, 30:
			m.extra = protowire.AppendTag(m.extra, f.Num, protowire.BytesType)
			m.extra = protowire.AppendBytes(m.extra, f.Bytes)
		case 23:
			m.PtpUIDs = append(m.PtpUIDs, f.str())
		case 24:
			m.PtpCallsigns = append(m.PtpCallsigns, f.str())
		case 25:
			m.FeedUID = f.str()
		case 26:
			m.MissionNames = append(m.MissionNames, f.str())
		case 27:
			m.Access = f.str()
		case 28:
			m.Caveat = f.str()
		case 29:
			m.ReleaseableTo = f.str()
		case 31:
			m.MissionGUIDs = append(m.MissionGUIDs, f.str())
		}
		return nil
	})
}

// ContactListEntry tells a federate about a user it can address.
type ContactListEntry struct {
	Operation int64
	UID       string
	Callsign  string
	Phone     string
}

func (m *ContactListEntry) marshal(b []byte) []byte {
	b = appendVarint(b, 1, m.Operation)
	b = appendString(b, 2, m.UID)
	b = appendString(b, 3, m.Callsign)
	return appendString(b, 4, m.Phone)
}

func (m *ContactListEntry) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		switch f.Num {
		case 1:
			m.Operation = f.int64()
		case 2:
			m.UID = f.str()
		case 3:
			m.Callsign = f.str()
		case 4:
			m.Phone = f.str()
		}
		return nil
	})
}

// Hops limits how far an event travels between federates.
type Hops struct{ Max, Current int64 }

func (m *Hops) marshal(b []byte) []byte {
	b = appendVarint(b, 1, m.Max)
	return appendVarint(b, 2, m.Current)
}

func (m *Hops) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		switch f.Num {
		case 1:
			m.Max = f.int64()
		case 2:
			m.Current = f.int64()
		}
		return nil
	})
}

// FederatedEvent is one CoT event or contact list change.
type FederatedEvent struct {
	Event          *GeoEvent
	Contact        *ContactListEntry
	FederateGroups []string
	Provenance     []Provenance
	Hops           *Hops
	groupHopLimits []byte // passed through unread
}

func (m *FederatedEvent) marshal(b []byte) []byte {
	if m.Event != nil {
		b = appendMessage(b, 1, m.Event)
	}
	if m.Contact != nil {
		b = appendMessage(b, 2, m.Contact)
	}
	for _, g := range m.FederateGroups {
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendString(b, g)
	}
	for i := range m.Provenance {
		b = appendMessage(b, 4, &m.Provenance[i])
	}
	if m.Hops != nil {
		b = appendMessage(b, 5, m.Hops)
	}
	return appendBytes(b, 6, m.groupHopLimits)
}

func (m *FederatedEvent) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		if f.Type != protowire.BytesType {
			return nil
		}
		switch f.Num {
		case 1:
			m.Event = &GeoEvent{}
			return m.Event.unmarshal(f.Bytes)
		case 2:
			m.Contact = &ContactListEntry{}
			return m.Contact.unmarshal(f.Bytes)
		case 3:
			m.FederateGroups = append(m.FederateGroups, f.str())
		case 4:
			var p Provenance
			if err := p.unmarshal(f.Bytes); err != nil {
				return err
			}
			m.Provenance = append(m.Provenance, p)
		case 5:
			m.Hops = &Hops{}
			return m.Hops.unmarshal(f.Bytes)
		case 6:
			m.groupHopLimits = f.copyBytes()
		}
		return nil
	})
}

// HasProvenance reports whether the event already passed through serverID.
func (m *FederatedEvent) HasProvenance(serverID string) bool {
	return hasProvenance(m.Provenance, serverID)
}

func hasProvenance(list []Provenance, serverID string) bool {
	for _, p := range list {
		if p.ServerID == serverID {
			return true
		}
	}
	return false
}

// Identity names a federation endpoint.
type Identity struct {
	Name, UID, Description string
	Type                   int64
	ServerID               string
}

func (m *Identity) marshal(b []byte) []byte {
	b = appendString(b, 1, m.Name)
	b = appendString(b, 2, m.UID)
	b = appendString(b, 3, m.Description)
	b = appendVarint(b, 4, m.Type)
	return appendString(b, 5, m.ServerID)
}

func (m *Identity) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		switch f.Num {
		case 1:
			m.Name = f.str()
		case 2:
			m.UID = f.str()
		case 3:
			m.Description = f.str()
		case 4:
			m.Type = f.int64()
		case 5:
			m.ServerID = f.str()
		}
		return nil
	})
}

// Version is TakServerVersion.
type Version struct {
	Major, Minor, Patch int64
	Branch, Variant     string
}

func (m *Version) marshal(b []byte) []byte {
	b = appendVarint(b, 1, m.Major)
	b = appendVarint(b, 2, m.Minor)
	b = appendVarint(b, 3, m.Patch)
	b = appendString(b, 4, m.Branch)
	return appendString(b, 5, m.Variant)
}

func (m *Version) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		switch f.Num {
		case 1:
			m.Major = f.int64()
		case 2:
			m.Minor = f.int64()
		case 3:
			m.Patch = f.int64()
		case 4:
			m.Branch = f.str()
		case 5:
			m.Variant = f.str()
		}
		return nil
	})
}

// Subscription opens and answers streams.
type Subscription struct {
	Identity Identity
	Filter   string
	Version  *Version
}

func (m *Subscription) marshal(b []byte) []byte {
	b = appendMessage(b, 1, &m.Identity)
	b = appendString(b, 2, m.Filter)
	if m.Version != nil {
		b = appendMessage(b, 3, m.Version)
	}
	return b
}

func (m *Subscription) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		switch f.Num {
		case 1:
			return m.Identity.unmarshal(f.Bytes)
		case 2:
			m.Filter = f.str()
		case 3:
			m.Version = &Version{}
			return m.Version.unmarshal(f.Bytes)
		}
		return nil
	})
}

// Health is ClientHealth and ServerHealth (one status field).
type Health struct{ Status int64 }

func (m *Health) marshal(b []byte) []byte { return appendVarint(b, 1, m.Status) }

func (m *Health) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		if f.Num == 1 {
			m.Status = f.int64()
		}
		return nil
	})
}

// FederateGroups lists the groups a federate shares.
type FederateGroups struct {
	StreamUpdate *Health
	Groups       []string
	Provenance   []Provenance
	rest         []byte // hops, nested groups, hop limits: passed through unread
}

func (m *FederateGroups) marshal(b []byte) []byte {
	if m.StreamUpdate != nil {
		b = appendMessage(b, 1, m.StreamUpdate)
	}
	for _, g := range m.Groups {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendString(b, g)
	}
	for i := range m.Provenance {
		b = appendMessage(b, 3, &m.Provenance[i])
	}
	return append(b, m.rest...)
}

func (m *FederateGroups) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		if f.Type != protowire.BytesType {
			return nil
		}
		switch f.Num {
		case 1:
			m.StreamUpdate = &Health{}
			return m.StreamUpdate.unmarshal(f.Bytes)
		case 2:
			m.Groups = append(m.Groups, f.str())
		case 3:
			var p Provenance
			if err := p.unmarshal(f.Bytes); err != nil {
				return err
			}
			m.Provenance = append(m.Provenance, p)
		default:
			m.rest = protowire.AppendTag(m.rest, f.Num, protowire.BytesType)
			m.rest = protowire.AppendBytes(m.rest, f.Bytes)
		}
		return nil
	})
}

// BinaryBlob carries a file (ROL payloads) or this server's certificate
// (Getx509Identity, as DER) in its data field.
type BinaryBlob struct{ Data []byte }

func (m *BinaryBlob) marshal(b []byte) []byte { return appendBytes(b, 2, m.Data) }

func (m *BinaryBlob) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		if f.Num == 2 && f.Type == protowire.BytesType {
			m.Data = f.copyBytes()
		}
		return nil
	})
}

// TokenResponse is FederateTokenResponse.
type TokenResponse struct{ Token string }

func (m *TokenResponse) marshal(b []byte) []byte { return appendString(b, 1, m.Token) }

func (m *TokenResponse) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		if f.Num == 1 {
			m.Token = f.str()
		}
		return nil
	})
}

// ROL is a Resource Operation Language command. TAK Server federates files
// and missions with these: a program such as "create resource\n{json};",
// with any file in a BinaryBlob payload.
type ROL struct {
	Program        string
	Payload        [][]byte // the BinaryBlobs' data
	FederateGroups []string
	Provenance     []Provenance
}

func (m *ROL) marshal(b []byte) []byte {
	b = appendString(b, 1, m.Program)
	for _, p := range m.Payload {
		b = appendMessage(b, 2, &BinaryBlob{Data: p})
	}
	for _, g := range m.FederateGroups {
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendString(b, g)
	}
	for i := range m.Provenance {
		b = appendMessage(b, 4, &m.Provenance[i])
	}
	return b
}

func (m *ROL) unmarshal(b []byte) error {
	return eachField(b, func(f field) error {
		if f.Type != protowire.BytesType {
			return nil
		}
		switch f.Num {
		case 1:
			m.Program = f.str()
		case 2:
			var blob BinaryBlob
			if err := blob.unmarshal(f.Bytes); err != nil {
				return err
			}
			m.Payload = append(m.Payload, blob.Data)
		case 3:
			m.FederateGroups = append(m.FederateGroups, f.str())
		case 4:
			var p Provenance
			if err := p.unmarshal(f.Bytes); err != nil {
				return err
			}
			m.Provenance = append(m.Provenance, p)
		}
		return nil
	})
}
