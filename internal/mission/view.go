package mission

import (
	"encoding/xml"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edgetak/edgetak/internal/cot"
	"github.com/edgetak/edgetak/internal/store"
)

// JSON and XML renderings follow the official server's Jackson/JAXB output
// (Mission, MissionChange, Resource, MissionRole, MissionSubscription).

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(cot.TimeLayout)
}

func roleJSON(role string) map[string]any {
	perms := rolePermissions[role]
	if perms == nil {
		perms = []string{}
	}
	return map[string]any{"type": role, "permissions": perms}
}

func roleXML(role string) string {
	var b strings.Builder
	b.WriteString(`<role type="` + role + `"><permissions>`)
	for _, p := range rolePermissions[role] {
		b.WriteString(`<permission type="` + p + `"/>`)
	}
	b.WriteString(`</permissions></role>`)
	return b.String()
}

// ResourceJSON renders a stored file as the official Resource JSON.
func ResourceJSON(p *store.Package) map[string]any {
	kw := p.Keywords
	if kw == nil {
		kw = []string{}
	}
	return map[string]any{
		"filename": p.Name, "name": p.Name, "keywords": kw, "mimeType": p.MIMEType, "contentType": p.MIMEType,
		"submissionTime": fmtTime(p.Submitted), "submitter": p.Submitter, "uid": p.Hash, "hash": p.Hash,
		"size": p.Size, "creatorUid": p.CreatorUID, "tool": p.Tool, "expiration": -1, "groups": p.Groups,
	}
}

func detailsJSON(d *UIDDetails) map[string]any {
	if d == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range map[string]string{"type": d.Type, "callsign": d.Callsign, "title": d.Title, "iconsetPath": d.IconsetPath, "color": d.Color} {
		if v != "" {
			out[k] = v
		}
	}
	if d.Location != nil {
		out["location"] = map[string]float64{"lat": d.Location.Lat, "lon": d.Location.Lon}
	}
	return out
}

// render produces the official Mission JSON. Callers hold s.mu.
func (s *Service) render(m *Mission, v View) map[string]any {
	uids := []map[string]any{}
	for _, u := range m.UIDs {
		item := map[string]any{"data": u.UID, "timestamp": fmtTime(u.Timestamp), "creatorUid": u.CreatorUID}
		if d := detailsJSON(u.Details); d != nil {
			item["details"] = d
		}
		uids = append(uids, item)
	}
	contents := []map[string]any{}
	for _, c := range m.Contents {
		p, ok := s.store.Package(c.Hash)
		if !ok {
			continue // file was deleted from the server
		}
		contents = append(contents, map[string]any{"data": ResourceJSON(p), "timestamp": fmtTime(c.Timestamp), "creatorUid": c.CreatorUID})
	}
	out := map[string]any{
		"name": m.Name, "guid": m.GUID, "description": m.Description, "chatRoom": m.ChatRoom,
		"baseLayer": m.BaseLayer, "bbox": m.BBox, "path": m.Path, "classification": m.Classification,
		"tool": m.Tool, "keywords": nonNil(m.Keywords), "creatorUid": m.CreatorUID,
		"createTime": fmtTime(m.Created), "lastEdited": fmtTime(m.LastEdited),
		"groups": nonNil(m.Groups), "externalData": []any{}, "feeds": []any{}, "mapLayers": []any{},
		"defaultRole": roleJSON(m.DefaultRole), "ownerRole": roleJSON(RoleOwner),
		"inviteOnly": m.InviteOnly, "expiration": m.Expiration, "passwordProtected": m.PasswordHash != "",
		"uids": uids, "contents": contents,
	}
	if v.Token != "" {
		out["token"] = v.Token
	}
	if v.Changes {
		out["missionChanges"] = s.changesJSON(m, v.Since, false)
	}
	if v.Logs {
		out["logs"] = LogsJSON(filterLogs(m.Logs, v.Since))
	}
	return out
}

// LogsJSON renders log entries with the official date format.
func LogsJSON(logs []LogEntry) []map[string]any {
	out := []map[string]any{}
	for _, l := range logs {
		out = append(out, LogJSON(l))
	}
	return out
}

func LogJSON(l LogEntry) map[string]any {
	return map[string]any{
		"id": l.ID, "content": l.Content, "creatorUid": l.CreatorUID, "entryUid": l.EntryUID,
		"missionNames": nonNil(l.MissionNames), "servertime": fmtTime(l.ServerTime), "dtg": fmtTime(l.DTG),
		"created": fmtTime(l.Created), "contentHashes": nonNil(l.ContentHashes), "keywords": nonNil(l.Keywords),
	}
}

func subscriptionJSON(m *Mission, sub *Subscription, token string) map[string]any {
	out := map[string]any{
		"clientUid": sub.ClientUID, "username": sub.Username, "createTime": fmtTime(sub.Created),
		"role": roleJSON(sub.Role),
	}
	if token != "" {
		out["token"] = token
	}
	return out
}

func (s *Service) changeJSON(m *Mission, ch Change) map[string]any {
	out := map[string]any{
		"type": ch.Type, "missionName": m.Name, "missionGuid": m.GUID, "timestamp": fmtTime(ch.Timestamp),
		"serverTime": fmtTime(ch.Timestamp), "creatorUid": ch.CreatorUID, "isFederatedChange": false,
	}
	if ch.ContentUID != "" {
		out["contentUid"] = ch.ContentUID
		if d := detailsJSON(ch.Details); d != nil {
			out["details"] = d
		}
	}
	if ch.ContentHash != "" {
		if p, ok := s.store.Package(ch.ContentHash); ok {
			out["contentResource"] = ResourceJSON(p)
		} else {
			out["contentResource"] = map[string]any{"hash": ch.ContentHash, "uid": ch.ContentHash}
		}
	}
	return out
}

func (s *Service) changesJSON(m *Mission, since time.Time, squashed bool) []map[string]any {
	changes := m.Changes
	if squashed {
		// Keep only the last change for each item, in log order.
		last := map[string]int{}
		for i, ch := range changes {
			last[changeKey(ch)] = i
		}
		var kept []Change
		for i, ch := range changes {
			if last[changeKey(ch)] == i {
				kept = append(kept, ch)
			}
		}
		changes = kept
	}
	out := []map[string]any{}
	for _, ch := range changes {
		if !since.IsZero() && !ch.Timestamp.After(since) {
			continue
		}
		out = append(out, s.changeJSON(m, ch))
	}
	return out
}

func changeKey(ch Change) string {
	switch {
	case ch.ContentUID != "":
		return "u:" + ch.ContentUID
	case ch.ContentHash != "":
		return "h:" + ch.ContentHash
	}
	return "t:" + ch.Type + ch.Timestamp.String()
}

// changesXML renders <MissionChanges> for a t-x-m-c notification.
func (s *Service) changesXML(m *Mission, ch Change) string {
	var b strings.Builder
	el := func(name, val string) {
		if val != "" {
			b.WriteString("<" + name + ">" + xmlEsc(val) + "</" + name + ">")
		}
	}
	b.WriteString("<MissionChanges><MissionChange>")
	if ch.ContentHash != "" {
		b.WriteString("<contentResource>")
		if p, ok := s.store.Package(ch.ContentHash); ok {
			el("creatorUid", p.CreatorUID)
			el("expiration", "-1")
			el("filename", p.Name)
			el("hash", p.Hash)
			for _, k := range p.Keywords {
				el("keywords", k)
			}
			el("mimeType", p.MIMEType)
			el("name", p.Name)
			el("size", strconv.FormatInt(p.Size, 10))
			el("submissionTime", fmtTime(p.Submitted))
			el("submitter", p.Submitter)
			el("tool", p.Tool)
			el("uid", p.Hash)
		} else {
			el("hash", ch.ContentHash)
			el("uid", ch.ContentHash)
		}
		b.WriteString("</contentResource>")
	}
	el("contentUid", ch.ContentUID)
	el("creatorUid", ch.CreatorUID)
	el("isFederatedChange", "false")
	el("missionName", m.Name)
	el("missionGuid", m.GUID)
	el("timestamp", fmtTime(ch.Timestamp))
	el("type", ch.Type)
	if d := ch.Details; d != nil {
		b.WriteString("<details")
		for _, kv := range [][2]string{{"type", d.Type}, {"callsign", d.Callsign}, {"title", d.Title}, {"iconsetPath", d.IconsetPath}, {"color", d.Color}} {
			if kv[1] != "" {
				b.WriteString(" " + kv[0] + `="` + xmlEsc(kv[1]) + `"`)
			}
		}
		b.WriteString(">")
		if d.Location != nil {
			b.WriteString(`<location lat="` + strconv.FormatFloat(d.Location.Lat, 'f', -1, 64) + `" lon="` + strconv.FormatFloat(d.Location.Lon, 'f', -1, 64) + `"/>`)
		}
		b.WriteString("</details>")
	}
	b.WriteString("</MissionChange></MissionChanges>")
	return b.String()
}

// missionEvent builds a t-x-m-* notification event.
func missionEvent(m *Mission, cotType, msgType, authorUID, inner string) *cot.Event {
	ev := cot.NewEvent(cotType, newGUID(), "h-g-i-g-o", 20*time.Second)
	ev.Hae = 0
	var b strings.Builder
	b.WriteString(`<mission type="` + msgType + `" tool="` + xmlEsc(m.Tool) + `" name="` + xmlEsc(m.Name) + `" guid="` + m.GUID + `"`)
	if authorUID != "" {
		b.WriteString(` authorUid="` + xmlEsc(authorUID) + `"`)
	}
	b.WriteString(">" + inner + "</mission>")
	ev.Detail.XML = b.String()
	return ev
}

// uidDetails summarises a map item from its CoT.
func uidDetails(ev *cot.Event) *UIDDetails {
	d := &UIDDetails{Type: ev.Type, Callsign: ev.Callsign(), Location: &Location{Lat: ev.Lat, Lon: ev.Lon}}
	dec := xml.NewDecoder(strings.NewReader(ev.Detail.XML))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		for _, a := range se.Attr {
			switch {
			case se.Name.Local == "usericon" && a.Name.Local == "iconsetpath":
				d.IconsetPath = a.Value
			case se.Name.Local == "color" && a.Name.Local == "argb" && d.Color == "":
				d.Color = a.Value
			case se.Name.Local == "strokeColor" && a.Name.Local == "value" && d.Color == "":
				d.Color = a.Value
			case se.Name.Local == "contact" && a.Name.Local == "callsign" && d.Callsign == "":
				d.Callsign = a.Value
			}
		}
		if se.Name.Local == "title" {
			var t string
			if dec.DecodeElement(&t, &se) == nil {
				d.Title = strings.TrimSpace(t)
			}
		}
	}
	return d
}

var martiRE = regexp.MustCompile(`(?s)<marti>.*?</marti>|<marti/>`)

func stripMarti(x string) string { return martiRE.ReplaceAllString(x, "") }

func xmlEsc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
