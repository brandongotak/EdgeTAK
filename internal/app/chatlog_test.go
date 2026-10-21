package app

import (
	"encoding/csv"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/chatlog"
	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// geoChat builds a GeoChat message the way ATAK sends one.
func geoChat(msgID, fromUID, fromCS, parent, room, id, text string, members ...string) *cot.Event {
	ev := cot.NewEvent("b-t-f", "GeoChat."+fromUID+"."+id+"."+msgID, "h-g-i-g-o", time.Hour)
	grp := ` uid0="` + fromUID + `"`
	for i, m := range members {
		grp += ` uid` + string(rune('1'+i)) + `="` + m + `"`
	}
	ev.Detail.XML = `<__chat parent="` + parent + `" groupOwner="false" messageId="` + msgID + `" chatroom="` + room + `" id="` + id +
		`" senderCallsign="` + fromCS + `"><chatgrp` + grp + ` id="` + id + `"/></__chat>` +
		`<link uid="` + fromUID + `" type="a-f-G-U-C" relation="p-p"/><remarks source="BAO.F.ATAK.` + fromUID + `" to="` + id + `">` + text + `</remarks>`
	ev.Lat, ev.Lon = 38.9, -77.03
	return ev
}

func TestChatLog(t *testing.T) {
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
	call, adminHTTP := adminJSON(t, a)
	isChat := func(id string) func(*cot.Event) bool {
		return func(e *cot.Event) bool { return e.Type == "b-t-f" && strings.HasSuffix(e.UID, "."+id) }
	}

	// Off by default: nothing is recorded.
	streams["alice"].send(geoChat("m0", "ANDROID-alice", "ALICE", "RootContactGroup", "All Chat Rooms", "All Chat Rooms", "before", "All Chat Rooms"))
	streams["bob"].expect(isChat("m0"), "m0 at bob")
	if code, st := call("GET", "/admin/api/chat/settings", nil); code != 200 || st["enabled"] != false || st["messages"] != 0.0 {
		t.Fatalf("settings = %d %v", code, st)
	}
	if code, _ := call("PUT", "/admin/api/chat/settings", map[string]bool{"enabled": true}); code != 200 {
		t.Fatalf("enable = %d", code)
	}

	streams["alice"].send(geoChat("m1", "ANDROID-alice", "ALICE", "RootContactGroup", "All Chat Rooms", "All Chat Rooms", "moving to checkpoint 2", "All Chat Rooms"))
	streams["alice"].send(geoChat("m2", "ANDROID-alice", "ALICE", "TeamGroups", "Cyan", "Cyan", "cyan team only", "ANDROID-bob"))
	streams["alice"].send(geoChat("m3", "ANDROID-alice", "ALICE", "RootContactGroup", "BOB", "ANDROID-bob", "=HYPERLINK(\"x\") private to bob", "ANDROID-bob"))
	streams["bob"].send(geoChat("m4", "ANDROID-bob", "BOB", "RootContactGroup", "ALICE", "ANDROID-alice", "copy, alice", "ANDROID-alice"))
	// ATAK can send the same message again; delivery receipts are not messages.
	streams["alice"].send(geoChat("m1", "ANDROID-alice", "ALICE", "RootContactGroup", "All Chat Rooms", "All Chat Rooms", "moving to checkpoint 2", "All Chat Rooms"))
	receipt := geoChat("m5", "ANDROID-bob", "BOB", "RootContactGroup", "ALICE", "ANDROID-alice", "", "ANDROID-alice")
	receipt.Type = "b-t-f-d"
	streams["bob"].send(receipt)
	streams["alice"].expect(isChat("m4"), "m4 at alice")
	// And the console's own messages.
	if code, _ := call("POST", "/admin/api/send/message", map[string]any{"text": "all units hold", "from": "Ops", "to": map[string]any{"all": true}}); code != 200 {
		t.Fatalf("console send = %d", code)
	}
	time.Sleep(200 * time.Millisecond)

	find := func(q url.Values) (int, []map[string]any) {
		t.Helper()
		code, res := call("GET", "/admin/api/chat?"+q.Encode(), nil)
		if code != 200 {
			t.Fatalf("chat = %d", code)
		}
		var out []map[string]any
		for _, m := range res["messages"].([]any) {
			out = append(out, m.(map[string]any))
		}
		return int(res["total"].(float64)), out
	}
	total, msgs := find(nil)
	if total != 5 {
		t.Fatalf("total = %d: %v", total, msgs)
	}
	if msgs[0]["text"] != "all units hold" || msgs[0]["console"] != true || msgs[0]["username"] != "admin" || msgs[0]["from_callsign"] != "Ops" {
		t.Errorf("newest = %v", msgs[0])
	}
	byText := map[string]map[string]any{}
	for _, m := range msgs {
		byText[m["text"].(string)] = m
	}
	if m := byText["cyan team only"]; m["kind"] != "team" || m["room"] != "Cyan team" || m["username"] != "alice" {
		t.Errorf("team message = %v", m)
	}
	if m := byText["moving to checkpoint 2"]; m["kind"] != "all" || m["room_id"] != "all" || m["lat"] != 38.9 {
		t.Errorf("all chat = %v", m)
	}
	// Both directions of a one-to-one chat are one conversation.
	dm, back := byText[`=HYPERLINK("x") private to bob`], byText["copy, alice"]
	if dm["kind"] != "direct" || dm["room_id"] != back["room_id"] || dm["room"] != "ALICE ↔ BOB" {
		t.Errorf("direct = %v / %v", dm, back)
	}

	if n, _ := find(url.Values{"room": {dm["room_id"].(string)}}); n != 2 {
		t.Errorf("conversation filter = %d", n)
	}
	if n, _ := find(url.Values{"from": {"ANDROID-bob"}}); n != 1 {
		t.Errorf("sender filter = %d", n)
	}
	if n, _ := find(url.Values{"q": {"CHECKPOINT"}}); n != 1 {
		t.Errorf("search = %d", n)
	}
	if n, _ := find(url.Values{"since": {time.Now().Add(time.Hour).Format(time.RFC3339)}}); n != 0 {
		t.Errorf("future since = %d", n)
	}
	if code, r := call("GET", "/admin/api/chat/rooms", nil); code != 200 || len(r["rooms"].([]any)) != 3 || len(r["senders"].([]any)) != 3 {
		t.Errorf("rooms = %v", r)
	}

	// CSV export, oldest first, with formulas defused.
	resp, err := adminHTTP.Get("https://" + a.Addrs["https"] + "/admin/api/chat/export?format=csv")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(resp.Body).ReadAll()
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || len(rows) != 6 || rows[0][0] != "time_utc" {
		t.Fatalf("csv = %d %v %v", resp.StatusCode, err, rows)
	}
	defused := false
	for _, r := range rows[1:] {
		defused = defused || r[7] == `'=HYPERLINK("x") private to bob`
	}
	if rows[1][7] != "moving to checkpoint 2" || rows[5][7] != "all units hold" || !defused {
		t.Errorf("csv rows = %v", rows) // alice's and bob's direct messages may arrive in either order
	}

	// Retention removes old messages.
	a.Chat.Record(chatlog.Message{ID: "old", Time: time.Now().Add(-10 * 24 * time.Hour), FromUID: "x", RoomID: "all", Kind: "all", Room: "All Chat Rooms", Text: "last week"})
	if code, res := call("POST", "/admin/api/retention/preview", map[string]int{"chat_days": 7}); code != 200 || res["removed"].(map[string]any)["chat"] != 1.0 {
		t.Errorf("retention preview = %v", res)
	}
	call("PUT", "/admin/api/retention", map[string]int{"chat_days": 7})
	call("POST", "/admin/api/retention/run", nil)
	if n, _ := find(nil); n != 5 {
		t.Errorf("after retention = %d", n)
	}

	// Clearing and turning it off.
	if code, _ := call("DELETE", "/admin/api/chat", nil); code != http.StatusNoContent {
		t.Fatalf("clear = %d", code)
	}
	call("PUT", "/admin/api/chat/settings", map[string]bool{"enabled": false})
	streams["alice"].send(geoChat("m9", "ANDROID-alice", "ALICE", "RootContactGroup", "All Chat Rooms", "All Chat Rooms", "after", "All Chat Rooms"))
	streams["bob"].expect(isChat("m9"), "m9 at bob")
	if n, _ := find(nil); n != 0 {
		t.Errorf("after clear and off = %d", n)
	}
}
