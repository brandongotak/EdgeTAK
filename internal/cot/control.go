package cot

import (
	"strings"
	"time"
)

// CoT types used for stream control rather than situational awareness.
const (
	TypePing          = "t-x-c-t"
	TypePong          = "t-x-c-t-r"
	TypeProtoAnnounce = "t-x-takp-v"
	TypeProtoRequest  = "t-x-takp-q"
	TypeProtoResponse = "t-x-takp-r"
	TypeDelete        = "t-x-d-d"
)

// ProtocolVersion is the TAK streaming protobuf version this server speaks.
const ProtocolVersion = "1"

// IsControl reports whether typ is consumed by the server instead of routed.
func IsControl(typ string) bool {
	return typ == TypePing || strings.HasPrefix(typ, "t-x-takp")
}

// ProtocolAnnouncement builds the t-x-takp-v event sent to new stream clients.
func ProtocolAnnouncement(uid, serverVersion string) *Event {
	e := NewEvent(TypeProtoAnnounce, uid, "m-g", time.Minute)
	e.Detail.XML = `<TakControl><TakProtocolSupport version="` + ProtocolVersion + `"/>` +
		`<TakServerVersionInfo serverVersion="` + escape(serverVersion) + `" apiVersion="3"/></TakControl>`
	return e
}

// ProtocolResponse builds the t-x-takp-r reply to a protocol request.
func ProtocolResponse(uid string, ok bool) *Event {
	e := NewEvent(TypeProtoResponse, uid, "m-g", time.Minute)
	status := "false"
	if ok {
		status = "true"
	}
	e.Detail.XML = `<TakControl><TakResponse status="` + status + `"/></TakControl>`
	return e
}

// Pong builds the reply to a client ping.
func Pong() *Event {
	return NewEvent(TypePong, "takPong", "h-g-i-g-o", 20*time.Second)
}

func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("&quot;")
		case '<':
			b.WriteString("&lt;")
		case '&':
			b.WriteString("&amp;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
