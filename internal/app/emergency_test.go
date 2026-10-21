package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/totp"
)

// atakAlert builds an emergency event the way ATAK's EmergencyManager does.
func atakAlert(uid, callsign, cotType, desc string, cancel bool) *cot.Event {
	ev := cot.NewEvent(cotType, uid+"-9-1-1", "h-e", 10*time.Second)
	ev.Lat, ev.Lon = 38.8977, -77.0365
	if cancel {
		ev.Type = "b-a-o-can"
		ev.Detail.XML = `<emergency cancel="true">` + callsign + `</emergency>`
		return ev
	}
	ev.Detail.XML = `<link uid="` + uid + `" type="a-f-G-U-C" relation="p-p"/><contact callsign="` + callsign + `-Alert"/>` +
		`<emergency type="` + desc + `">` + callsign + `</emergency>`
	return ev
}

func TestEmergencyAlerts(t *testing.T) {
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
	call, hc := adminJSON(t, a)
	active := func() []map[string]any {
		t.Helper()
		code, r := call("GET", "/admin/api/emergencies?active=1", nil)
		if code != 200 {
			t.Fatalf("emergencies = %d", code)
		}
		var out []map[string]any
		for _, e := range r["active"].([]any) {
			out = append(out, e.(map[string]any))
		}
		return out
	}

	// alice presses 911: bob still receives it, and the console shows it.
	streams["alice"].send(atakAlert("ANDROID-alice", "ALICE", "b-a-o-tbl", "911 Alert", false))
	streams["bob"].expect(func(e *cot.Event) bool { return e.Type == "b-a-o-tbl" }, "alert at bob")
	time.Sleep(100 * time.Millisecond)
	act := active()
	if len(act) != 1 || act[0]["type"] != "911 Alert" || act[0]["callsign"] != "ALICE" || act[0]["username"] != "alice" ||
		act[0]["device_uid"] != "ANDROID-alice" || act[0]["lat"] != 38.8977 {
		t.Fatalf("active = %v", act)
	}
	// ATAK repeats the alert; it stays one alert.
	streams["alice"].send(atakAlert("ANDROID-alice", "ALICE", "b-a-o-tbl", "911 Alert", false))
	time.Sleep(100 * time.Millisecond)
	if n := len(active()); n != 1 {
		t.Errorf("after a repeat: %d alerts", n)
	}
	// The Health tab flags the device.

	// The Health tab puts the alert first among alice's device issues.
	resp, err := hc.Get("https://" + a.Addrs["https"] + "/admin/api/health/devices")
	if err != nil {
		t.Fatal(err)
	}
	var health []map[string]any
	json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()
	flagged := false
	for _, d := range health {
		if d["callsign"] == "ALICE" {
			issues := d["issues"].([]any)
			flagged = len(issues) > 0 && strings.HasPrefix(issues[0].(map[string]any)["text"].(string), "EMERGENCY: 911 Alert")
		}
	}
	if !flagged {
		t.Errorf("health devices = %v", health)
	}

	id := act[0]["id"].(string)
	if code, e := call("POST", "/admin/api/emergencies/"+id+"/ack", nil); code != 200 || e["acked_by"] != "admin" {
		t.Errorf("ack = %d %v", code, e)
	}
	if act := active(); len(act) != 1 || act[0]["acked"] == nil {
		t.Errorf("an acknowledged alert stays active: %v", act)
	}
	// alice cancels on her device.
	streams["alice"].send(atakAlert("ANDROID-alice", "ALICE", "", "", true))
	streams["bob"].expect(func(e *cot.Event) bool { return e.Type == "b-a-o-can" }, "cancel at bob")
	time.Sleep(100 * time.Millisecond)
	if n := len(active()); n != 0 {
		t.Errorf("after cancel: %d active", n)
	}

	// bob's In Contact alert; his phone dies, so an admin dismisses it.
	streams["bob"].send(atakAlert("ANDROID-bob", "BOB", "b-a-o-opn", "In Contact", false))
	time.Sleep(150 * time.Millisecond)
	act = active()
	if len(act) != 1 || act[0]["type"] != "In Contact" {
		t.Fatalf("bob's alert = %v", act)
	}
	streams["bob"].conn.Close()
	time.Sleep(100 * time.Millisecond)
	if n := len(active()); n != 1 {
		t.Errorf("a disconnected device's alert must stay: %d", n)
	}
	if code, _ := call("POST", "/admin/api/emergencies/"+act[0]["id"].(string)+"/dismiss", nil); code != 200 {
		t.Errorf("dismiss = %d", code)
	}

	// The log has both, newest first, with how each ended.
	_, all := call("GET", "/admin/api/emergencies", nil)
	log := all["log"].([]any)
	if len(log) != 2 {
		t.Fatalf("log = %v", log)
	}
	if first := log[0].(map[string]any); first["ended_by"] != "admin" || first["type"] != "In Contact" {
		t.Errorf("newest = %v", first)
	}
	if second := log[1].(map[string]any); second["ended_by"] != "device" || second["acked_by"] != "admin" {
		t.Errorf("older = %v", second)
	}
}

// Every user can turn on two-factor login from the account page.
func TestAccountTwoFactorSetup(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("erin", "password-erin", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, _ := accountClient(t, a, "erin", "password-erin")
	if code, st := call("GET", "/account/api/2fa", nil); code != 200 || st["enabled"] != false {
		t.Fatalf("status = %d %v", code, st)
	}
	code, setup := call("POST", "/account/api/2fa/setup", nil)
	if code != 200 || setup["secret"] == "" || !strings.Contains(setup["uri"].(string), "erin") {
		t.Fatalf("setup = %d %v", code, setup)
	}
	secret := setup["secret"].(string)
	if code, _ := call("POST", "/account/api/2fa/enable", map[string]string{"code": "000000"}); code != 400 {
		t.Errorf("wrong code accepted: %d", code)
	}
	now, _ := totp.Code(secret, time.Now())
	code, res := call("POST", "/account/api/2fa/enable", map[string]string{"code": now})
	if code != 200 || len(res["recovery_codes"].([]any)) != 10 {
		t.Fatalf("enable = %d %v", code, res)
	}
	// Signing in now needs a code; a recovery code works once.
	plain, _ := accountClientRaw(t, a)
	if code, r := plain("POST", "/account/api/login", map[string]string{"username": "erin", "password": "password-erin"}); code != 401 || r["need_code"] != true {
		t.Errorf("sign-in without a code = %d %v", code, r)
	}
	recovery := res["recovery_codes"].([]any)[0].(string)
	if code, _ := plain("POST", "/account/api/login", map[string]string{"username": "erin", "password": "password-erin", "code": recovery}); code != 200 {
		t.Errorf("recovery code sign-in = %d", code)
	}
	// Turning it off needs the password and a code.
	if code, _ := call("POST", "/account/api/2fa/disable", map[string]string{"password": "password-erin"}); code != 401 {
		t.Errorf("disable without a code = %d", code)
	}
	if code, _ := call("POST", "/account/api/2fa/disable", map[string]string{"password": "password-erin", "code": res["recovery_codes"].([]any)[1].(string)}); code != 204 {
		t.Errorf("disable = %d", code)
	}
	if u, _ := a.Store.User("erin"); u.TwoFactor() {
		t.Error("still on")
	}
}
