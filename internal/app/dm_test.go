package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

func TestDirectMessages(t *testing.T) {
	a := startApp(t)
	for _, u := range []string{"alice", "bob"} {
		if _, err := a.Store.CreateUser(u, "password-"+u, []string{"Cyan"}, false); err != nil {
			t.Fatal(err)
		}
	}
	streams := map[string]*streamClient{}
	for _, u := range []string{"alice", "bob"} {
		c := enroll(t, a, u, "password-"+u)
		streams[u] = mustDial(t, a, &c)
		streams[u].send(sa("ANDROID-"+u, strings.ToUpper(u), "Cyan"))
	}
	time.Sleep(200 * time.Millisecond)
	ids := map[string]uint64{}
	for _, c := range a.Router.Clients() {
		ids[c.Username] = c.ID
	}
	send, adminHTTP := adminJSON(t, a)
	serverUID := "EdgeTAK-" + a.nodeID
	isPresence := func(e *cot.Event) bool { return e.UID == serverUID && strings.HasPrefix(e.Type, "a-f-") }

	// Direct messages need the server contact.
	if st, _ := send("POST", "/admin/api/send/message", map[string]any{"text": "hi", "direct": true, "to": map[string]any{"all": true}}); st != 400 {
		t.Errorf("direct message without server contact = %d, want 400", st)
	}
	if st, _ := send("PUT", "/admin/api/send/contact", map[string]any{"enabled": true, "callsign": "Ops Center"}); st != 400 {
		t.Errorf("enabling without a position = %d, want 400", st)
	}
	if st, _ := send("PUT", "/admin/api/send/contact", map[string]any{"enabled": true, "callsign": "Ops Center", "lat": 38.9, "lon": -77.03}); st != 204 {
		t.Fatalf("set server contact = %d", st)
	}
	// Connected devices learn the contact: a marker with a server-routed endpoint.
	p := streams["alice"].expect(isPresence, "server contact at alice")
	if p.Callsign() != "Ops Center" || p.Detail.Contact.Endpoint != "*:-1:stcp" || p.Detail.Group == nil || p.Detail.Group.Role != "HQ" || p.Lat != 38.9 {
		t.Errorf("presence = %+v %+v", p, p.Detail)
	}
	// New devices get it when they connect.
	late := mustDial(t, a, ptr(enroll(t, a, "alice", "password-alice")))
	late.expect(isPresence, "server contact on connect")
	late.conn.Close()

	// A direct message to bob opens a conversation with the server contact.
	if st, m := send("POST", "/admin/api/send/message", map[string]any{"text": "report status", "direct": true, "to": map[string]any{"clients": []uint64{ids["bob"]}}}); st != 200 || m["delivered"] != float64(1) {
		t.Fatalf("direct message = %d %v", st, m)
	}
	dm := streams["bob"].expect(func(e *cot.Event) bool { return e.Type == "b-t-f" }, "direct message at bob")
	parts := strings.Split(dm.UID, ".")
	if len(parts) != 4 || parts[1] != serverUID || parts[2] != "ANDROID-bob" {
		t.Errorf("direct message uid = %q, want GeoChat.<server>.<bob uid>.<id>", dm.UID)
	}
	for _, want := range []string{`id="ANDROID-bob"`, `chatroom="BOB"`, `senderCallsign="Ops Center"`, `uid1="ANDROID-bob"`, `>report status</remarks>`} {
		if !strings.Contains(dm.Detail.XML, want) {
			t.Errorf("direct message missing %s:\n%s", want, dm.Detail.XML)
		}
	}
	streams["alice"].expectNone(func(e *cot.Event) bool { return e.Type == "b-t-f" }, "bob's direct message at alice")

	// bob replies to the contact the way ATAK does; it lands in the Inbox.
	reply := cot.NewEvent("b-t-f", "GeoChat.ANDROID-bob."+serverUID+".m1", "h-g-i-g-o", time.Hour)
	reply.Detail.XML = `<__chat parent="RootContactGroup" groupOwner="false" messageId="m1" chatroom="Ops Center" id="` + serverUID +
		`" senderCallsign="BOB"><chatgrp uid0="ANDROID-bob" uid1="` + serverUID + `" id="` + serverUID + `"/></__chat>` +
		`<link uid="ANDROID-bob" type="a-f-G-U-C" relation="p-p"/><remarks source="BAO.F.ATAK.ANDROID-bob" to="` + serverUID +
		`">all good &amp; moving</remarks><marti><dest callsign="Ops Center"/></marti>`
	streams["bob"].send(reply)
	streams["alice"].expectNone(func(e *cot.Event) bool { return e.UID == reply.UID }, "bob's reply leaking to alice")
	resp, _ := adminHTTP.Get("https://" + a.Addrs["https"] + "/admin/api/send/inbox")
	var inbox []map[string]any
	json.NewDecoder(resp.Body).Decode(&inbox)
	resp.Body.Close()
	if len(inbox) != 1 || inbox[0]["from_callsign"] != "BOB" || inbox[0]["text"] != "all good & moving" || inbox[0]["username"] != "bob" {
		t.Fatalf("inbox = %v", inbox)
	}

	// Replying from the Inbox sends another direct message to bob.
	if st, _ := send("POST", "/admin/api/send/reply", map[string]any{"uid": "ANDROID-bob", "text": "copy"}); st != 200 {
		t.Errorf("reply = %d", st)
	}
	streams["bob"].expect(func(e *cot.Event) bool { return e.Type == "b-t-f" && strings.Contains(e.Detail.XML, ">copy</remarks>") }, "reply at bob")
	if st, _ := send("POST", "/admin/api/send/reply", map[string]any{"uid": "ANDROID-nobody", "text": "x"}); st != 404 {
		t.Errorf("reply to unknown device = %d", st)
	}

	// Turning the contact off removes it from devices.
	send("PUT", "/admin/api/send/contact", map[string]any{"enabled": false, "callsign": "Ops Center", "lat": 38.9, "lon": -77.03})
	del := streams["alice"].expect(func(e *cot.Event) bool { return e.Type == cot.TypeDelete }, "server contact removal")
	if !strings.Contains(del.Detail.XML, `uid="`+serverUID+`"`) {
		t.Errorf("removal = %s", del.Detail.XML)
	}
}

func ptr[T any](v T) *T { return &v }
