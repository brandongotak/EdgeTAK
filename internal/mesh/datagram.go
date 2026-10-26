package mesh

import (
	"bytes"
	"errors"
	"regexp"
	"strings"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// ATAK's mesh datagrams (commoncommo protocol.txt): one CoT event each,
// either XML ("version 0") or TAK Protocol: 0xbf, the version as a varint,
// 0xbf, then a protobuf TakMessage. There is no length prefix.

var takProtoHeader = []byte{0xbf, 0x01, 0xbf}

// decode reads a datagram. ev is nil for a TakMessage with only control
// data (an ATAK's version announcement); proto reports the format.
func decode(b []byte) (ev *cot.Event, proto bool, err error) {
	if bytes.HasPrefix(b, takProtoHeader) {
		ev, _, err = cot.DecodeTakMessage(b[len(takProtoHeader):])
		return ev, true, err
	}
	if len(b) > 0 && b[0] == 0xbf {
		return nil, true, errors.New("TAK Protocol version other than 1")
	}
	ev, err = cot.ParseXML(bytes.TrimSpace(b))
	return ev, false, err
}

// encode writes ev as a datagram.
func encode(ev *cot.Event, proto bool) []byte {
	if !proto {
		return ev.XML()
	}
	msg := ev.TakMessage()
	out := make([]byte, 0, len(takProtoHeader)+len(msg))
	return append(append(out, takProtoHeader...), msg...)
}

var reContactEndpoint = regexp.MustCompile(`(<contact\b[^>]*\bendpoint=")[^"]*(")`)

// withEndpoint is ev with its contact endpoint replaced, as ATAK does for
// its own announcements on each interface: mesh devices then reach the
// contact through the bridge.
func withEndpoint(ev *cot.Event, endpoint string) *cot.Event {
	if ev.Detail.Contact == nil || ev.Detail.Contact.Endpoint == "" {
		return ev
	}
	xml := reContactEndpoint.ReplaceAll(ev.XML(), []byte("${1}"+endpoint+"${2}"))
	out, err := cot.ParseXML(xml)
	if err != nil {
		return ev
	}
	return out
}

var reChatID = regexp.MustCompile(`<__chat\b[^>]*\bid="([^"]*)"`)

// chatRecipient is the device a direct chat message is for, "" for a
// chat room (All Chat Rooms, a group) or anything else.
func chatRecipient(ev *cot.Event) string {
	if ev.Type != "b-t-f" {
		return ""
	}
	m := reChatID.FindStringSubmatch(ev.Detail.XML)
	if m == nil || m[1] == "" || m[1] == "All Chat Rooms" || strings.Contains(m[1], " ") {
		return ""
	}
	return m[1]
}

// addressedTo is ev addressed to the device uid (<marti><dest uid/>), so
// the server delivers it only there.
func addressedTo(ev *cot.Event, uid string) *cot.Event {
	if len(ev.Dests()) > 0 {
		return ev
	}
	xml := ev.XML()
	i := bytes.LastIndex(xml, []byte("</detail>"))
	if i < 0 {
		return ev
	}
	dest := []byte(`<marti><dest uid="` + escapeAttr(uid) + `"/></marti>`)
	out, err := cot.ParseXML(append(append(append([]byte{}, xml[:i]...), dest...), xml[i:]...))
	if err != nil {
		return ev
	}
	return out
}

func escapeAttr(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// category is which "send to the mesh" option covers an event type.
func category(typ string) string {
	switch {
	case strings.HasPrefix(typ, "a-"), typ == "t-x-d-d": // a delete removes a marker
		return "positions"
	case typ == "b-t-f" || strings.HasPrefix(typ, "b-t-f-"):
		return "chat"
	case strings.HasPrefix(typ, "b-a-"):
		return "alerts"
	case strings.HasPrefix(typ, "u-"), strings.HasPrefix(typ, "b-m-r"), strings.HasPrefix(typ, "b-m-p"):
		return "drawings"
	case strings.HasPrefix(typ, "t-x-c-"), strings.HasPrefix(typ, "t-x-takp"):
		return "control" // pings and protocol negotiation: never bridged
	}
	return "other"
}
