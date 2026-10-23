package app

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestAuditLog covers the audit log: admin changes are recorded with who,
// what and the target (named even when it was deleted), without secrets;
// refused changes, sign-ins and failed sign-ins are recorded; the log can
// be searched and exported; an "admin" webhook hears about changes.
func TestAuditLog(t *testing.T) {
	a := startApp(t)
	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]

	var mu sync.Mutex
	var hooked []string
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		hooked = append(hooked, m["title"].(string))
		mu.Unlock()
	}))
	defer recv.Close()
	a.Web.WebhookClient = recv.Client()
	call("POST", "/admin/api/webhooks", map[string]any{"name": "Admins", "url": recv.URL + "/x", "format": "json", "events": []string{"admin"}})

	call("POST", "/admin/api/users", map[string]any{"username": "alice", "password": "very-secret-pw-1", "groups": []string{"Cyan"}})
	call("PATCH", "/admin/api/users/alice", map[string]any{"disabled": true})
	call("POST", "/admin/api/overlays/nope/send", nil) // refused: no such overlay
	call("DELETE", "/admin/api/users/alice", nil)
	call("PUT", "/admin/api/history/settings", map[string]any{"enabled": true, "retention_hours": 24})
	// A failed sign-in.
	plain := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	if resp, err := plain.Post(base+"/admin/api/login", "application/json", strings.NewReader(`{"username":"mallory","password":"guess"}`)); err == nil {
		resp.Body.Close()
	}

	get := func(q string) []map[string]any {
		r, err := hc.Get(base + "/admin/api/audit" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var list []map[string]any
		json.NewDecoder(r.Body).Decode(&list)
		return list
	}
	list := get("")
	byAction := map[string]map[string]any{}
	for _, e := range list {
		if _, seen := byAction[e["action"].(string)]; !seen {
			byAction[e["action"].(string)] = e
		}
	}
	if e := byAction["Created user"]; e == nil || e["actor"] != "admin" || e["target"] != "alice" || !strings.Contains(e["details"].(string), "groups: [Cyan]") {
		t.Errorf("created = %v", e)
	}
	if e := byAction["Changed user"]; e == nil || e["target"] != "alice" || e["details"] != "disabled: true" {
		t.Errorf("changed = %v", e)
	}
	if e := byAction["Deleted user"]; e == nil || e["target"] != "alice" {
		t.Errorf("deleted = %v", e)
	}
	if e := byAction["Sent overlay again"]; e == nil || e["failed"] != true || e["status"] != float64(404) {
		t.Errorf("refused = %v", e)
	}
	if e := byAction["Signed in (administrator)"]; e == nil || e["actor"] != "admin" {
		t.Errorf("sign-in = %v", e)
	}
	if e := byAction["Sign-in failed"]; e == nil || e["actor"] != "mallory" {
		t.Errorf("failed sign-in = %v (have %v)", e, byAction)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "very-secret-pw-1") || strings.Contains(string(raw), recv.URL) {
		t.Error("a secret was recorded")
	}

	// Search, failures only, CSV.
	if l := get("?q=alice"); len(l) != 3 {
		t.Errorf("search for alice = %d entries", len(l))
	}
	if l := get("?failed=1"); len(l) != 2 {
		t.Errorf("failures = %v", l)
	}
	r, err := hc.Get(base + "/admin/api/audit.csv?q=alice")
	if err != nil {
		t.Fatal(err)
	}
	csv, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.HasPrefix(string(csv), "time (UTC),who,from,action,target,details,result") || strings.Count(string(csv), "\n") != 4 {
		t.Errorf("csv = %s", csv)
	}

	waitFor(t, "admin webhook", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.Join(hooked, "|"), "Deleted user")
	})
	mu.Lock()
	if strings.Contains(strings.Join(hooked, "|"), "Sent overlay again") {
		t.Error("a refused change went to the webhook")
	}
	mu.Unlock()

	// Clearing leaves only the record of the clearing, and tells the webhook.
	before := len(get("?limit=1000"))
	code, v := call("DELETE", "/admin/api/audit", nil)
	if code != 200 || int(v["removed"].(float64)) < before {
		t.Fatalf("clear = %d %v (had %d)", code, v, before)
	}
	if l := get(""); len(l) != 1 || l[0]["action"] != "Cleared the audit log" || l[0]["actor"] != "admin" {
		t.Errorf("after clearing = %v", l)
	}
	waitFor(t, "clear webhook", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.Join(hooked, "|"), "Cleared the audit log")
	})
}
