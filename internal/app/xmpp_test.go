package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestXMPPChat covers XMPP chat without Prosody installed: the settings and
// rooms, the files EdgeTAK writes for Prosody (configuration, users, groups,
// rooms) and the password check Prosody asks EdgeTAK for. Prosody itself
// was tested against these by hand (Prosody 13 on Debian 13).
func TestXMPPChat(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct{ name, group string }{{"alice", "Cyan"}, {"Bob", "Cyan"}, {"carol", "Red"}, {"dave@corp", "Cyan"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}
	call, _ := adminJSON(t, a)
	settings := func(change map[string]any) map[string]any {
		m := map[string]any{"enabled": false, "domain": "", "port": 5222, "upload_port": 5281, "upload_mb": 25, "upload_days": 7, "history_days": 30, "user_rooms": true}
		for k, v := range change {
			m[k] = v
		}
		return m
	}

	st, v := call("GET", "/admin/api/xmpp", nil)
	if st != 200 || v["status"].(map[string]any)["installed"] != false || v["can_install"] != false || v["install_note"] == "" {
		t.Fatalf("view = %d %v", st, v)
	}
	if skipped := mustJSON(v["skipped_users"]); skipped != `["dave@corp"]` {
		t.Errorf("skipped users = %s", skipped)
	}
	for name, c := range map[string]struct {
		body map[string]any
		want int
	}{
		"not installed": {settings(map[string]any{"enabled": true}), 400},
		"bad domain":    {settings(map[string]any{"domain": "not a domain!"}), 400},
		"same ports":    {settings(map[string]any{"upload_port": 5222}), 400},
		"local port":    {settings(map[string]any{"port": 5280}), 400},
		"huge files":    {settings(map[string]any{"upload_mb": 99999}), 400},
		"ok":            {settings(map[string]any{"domain": "Chat.Example.com", "upload_mb": 50, "history_days": 90, "user_rooms": false}), 200},
	} {
		if st, v := call("PUT", "/admin/api/xmpp/settings", c.body); st != c.want {
			t.Errorf("%s: %d %v", name, st, v)
		}
	}
	_, v = call("GET", "/admin/api/xmpp", nil)
	if s := v["settings"].(map[string]any); s["domain"] != "chat.example.com" || s["upload_mb"] != 50.0 || s["user_rooms"] != false || v["domain"] != "chat.example.com" {
		t.Errorf("settings = %v", v)
	}

	// Rooms.
	if st, v := call("POST", "/admin/api/xmpp/rooms", map[string]any{"name": "Ops Team!", "description": "Cyan only", "groups": []string{"Cyan"}}); st != 200 {
		t.Fatalf("add room = %d %v", st, v)
	}
	if st, _ := call("POST", "/admin/api/xmpp/rooms", map[string]any{"name": "ops team"}); st != 400 {
		t.Errorf("same address again = %d", st)
	}
	if st, _ := call("POST", "/admin/api/xmpp/rooms", map[string]any{"name": ""}); st != 400 {
		t.Errorf("no name = %d", st)
	}
	call("POST", "/admin/api/xmpp/rooms", map[string]any{"name": "Lobby"})
	if st, _ := call("PUT", "/admin/api/xmpp/rooms/lobby", map[string]any{"name": "Lobby", "groups": []string{"Red"}}); st != 200 {
		t.Errorf("edit room = %d", st)
	}
	if st, _ := call("PUT", "/admin/api/xmpp/rooms/nope", map[string]any{"name": "x"}); st != 404 {
		t.Errorf("edit missing room = %d", st)
	}

	// What Prosody gets.
	dir := filepath.Join(a.DataDir, "xmpp")
	var state struct {
		Users  map[string]string
		Groups map[string][]string
		Rooms  []struct {
			Node        string
			MembersOnly bool `json:"members_only"`
			Members     []string
		}
	}
	waitFor(t, "state for Prosody", func() bool {
		b, err := os.ReadFile(filepath.Join(dir, "state.json"))
		return err == nil && json.Unmarshal(b, &state) == nil && len(state.Rooms) == 2 && len(state.Rooms[1].Members) == 1
	})
	if mustJSON(state.Users) != `{"admin":"admin","alice":"alice","bob":"Bob","carol":"carol"}` || mustJSON(state.Groups["Cyan"]) != `["alice","bob"]` ||
		state.Rooms[0].Node != "ops-team" || !state.Rooms[0].MembersOnly || mustJSON(state.Rooms[0].Members) != `["alice","bob"]` ||
		state.Rooms[1].Node != "lobby" || mustJSON(state.Rooms[1].Members) != `["carol"]` {
		t.Errorf("state = %+v", state)
	}
	cfg, err := os.ReadFile(filepath.Join(dir, "prosody.cfg.lua"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`VirtualHost "chat.example.com"`, `Component "conference.chat.example.com" "muc"`, `restrict_room_creation = true`,
		`http_file_share_size_limit = 52428800`, `archive_expires_after = "90d"`, `authentication = "edgetak"`, `c2s_require_encryption = true`} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("config lacks %s", want)
		}
	}
	for _, f := range []string{"modules/mod_auth_edgetak.lua", "modules/mod_edgetak.lua", "modules/mod_edgetak_rooms.lua", "certs/server.crt", "certs/server.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}

	// The password check, as Prosody makes it.
	authURL := regexp.MustCompile(`edgetak_auth_url = "([^"]+)"`).FindStringSubmatch(string(cfg))[1]
	secret := regexp.MustCompile(`edgetak_secret = "([^"]+)"`).FindStringSubmatch(string(cfg))[1]
	check := func(secret, user, pw string) int {
		t.Helper()
		req, _ := http.NewRequest("POST", authURL, strings.NewReader(mustJSON(map[string]string{"username": user, "password": pw, "ip": "10.1.2.3"})))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for name, c := range map[string]struct {
		secret, user, pw string
		want             int
	}{
		"right":          {secret, "alice", "password-alice", 200},
		"capitals":       {secret, "bob", "password-Bob", 200},
		"wrong password": {secret, "alice", "nope", 401},
		"wrong secret":   {"guess", "alice", "password-alice", 403},
		"no such user":   {secret, "zed", "x", 401},
	} {
		if got := check(c.secret, c.user, c.pw); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	// A disabled user can't sign in, and leaves Prosody's list.
	if st, _ := call("PATCH", "/admin/api/users/carol", map[string]any{"disabled": true}); st != 200 {
		t.Fatalf("disable = %d", st)
	}
	if got := check(secret, "carol", "password-carol"); got != 401 {
		t.Errorf("disabled user = %d", got)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(dir, "state.json"))
		if !strings.Contains(string(b), `"carol"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disabled user still in Prosody's list")
		}
		time.Sleep(200 * time.Millisecond)
	}

	if st, _ := call("DELETE", "/admin/api/xmpp/rooms/lobby", nil); st != 200 {
		t.Errorf("delete room = %d", st)
	}
	if st, _ := call("DELETE", "/admin/api/xmpp/rooms/lobby", nil); st != 404 {
		t.Errorf("delete again = %d", st)
	}
}
