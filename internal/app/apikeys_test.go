package app

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/audit"
)

// TestAPIKeys covers API keys: made in the console, used as a Bearer token
// with their user's rights, read-only keys refused changes, and keys that
// stop working when deleted or their user is disabled.
func TestAPIKeys(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("bob", "password-bob", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, _ := adminJSON(t, a)
	mk := func(body map[string]any) (string, string) {
		t.Helper()
		st, v := call("POST", "/admin/api/api-keys", body)
		key, _ := v["key"].(string)
		if st != 200 || !strings.HasPrefix(key, "etk_") {
			t.Fatalf("make %v = %d %v", body, st, v)
		}
		return key, v["id"].(string)
	}
	full, fullID := mk(map[string]any{"name": "script"})
	ro, roID := mk(map[string]any{"name": "grafana", "read_only": true, "days": 30})
	bobKey, _ := mk(map[string]any{"name": "bob's", "username": "bob"})
	if st, _ := call("POST", "/admin/api/api-keys", map[string]any{"name": " "}); st != 400 {
		t.Errorf("no name = %d", st)
	}
	if st, _ := call("POST", "/admin/api/api-keys", map[string]any{"name": "x", "username": "nobody"}); st != 404 {
		t.Errorf("no such user = %d", st)
	}

	// A client with no session: only the key.
	hc := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	use := func(key, method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, "https://"+a.Addrs["https"]+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, _ := use(full, "GET", "/admin/api/status", ""); st != 200 {
		t.Errorf("full key status = %d", st)
	}
	if st, _ := use(ro, "GET", "/admin/api/status", ""); st != 200 {
		t.Errorf("read-only key status = %d", st)
	}
	if st, _ := use(bobKey, "GET", "/admin/api/status", ""); st != 403 {
		t.Errorf("non-admin key on the admin API = %d", st)
	}
	if st, _ := use("etk_wrong", "GET", "/admin/api/status", ""); st != 401 {
		t.Errorf("wrong key = %d", st)
	}
	// Keys can't make keys, and read-only keys can't change anything.
	if st, _ := use(full, "POST", "/admin/api/api-keys", `{"name":"more"}`); st != 403 {
		t.Errorf("key making a key = %d", st)
	}
	if st, body := use(ro, "DELETE", "/admin/api/api-keys/"+fullID, ""); st != 403 || !strings.Contains(body, "read-only") {
		t.Errorf("read-only key deleting = %d %s", st, body)
	}
	if st, _ := use(full, "DELETE", "/admin/api/api-keys/"+roID, ""); st != 200 {
		t.Errorf("full key deleting = %d", st)
	}
	if st, _ := use(ro, "GET", "/admin/api/status", ""); st != 401 {
		t.Errorf("deleted key = %d", st)
	}

	// The audit log names the key.
	_, raw := use(full, "GET", "/admin/api/audit", "")
	var entries []audit.Entry
	json.Unmarshal([]byte(raw), &entries)
	found := false
	for _, e := range entries {
		if e.Actor == "admin (API key script)" && e.Action == "Deleted an API key" && e.Target == "grafana (admin)" {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit entry for the key's change: %s", raw)
	}

	// The list shows when keys were used, never the key or its hash.
	_, listRaw := use(full, "GET", "/admin/api/api-keys", "")
	if strings.Contains(listRaw, full) || strings.Contains(listRaw, "hash") || !strings.Contains(listRaw, "last_used") {
		t.Errorf("key list: %s", listRaw)
	}

	// A disabled user's keys stop working.
	if st, _ := use(bobKey, "GET", "/Marti/api/groups/all", ""); st != 200 {
		t.Errorf("bob's key on the Marti API = %d", st)
	}
	if st, _ := call("PATCH", "/admin/api/users/bob", map[string]any{"disabled": true}); st != 200 {
		t.Fatalf("disable bob = %d", st)
	}
	if st, _ := use(bobKey, "GET", "/Marti/api/groups/all", ""); st != 401 {
		t.Errorf("disabled user's key = %d", st)
	}
}
