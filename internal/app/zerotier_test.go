package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeZeroTier is the local ZeroTier service's API, as far as EdgeTAK uses it.
type fakeZeroTier struct {
	mu   sync.Mutex
	nets map[string]map[string]any
}

func (z *fakeZeroTier) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-ZT1-Auth") != "zt-token-123" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	switch {
	case r.Method == "GET" && r.URL.Path == "/status":
		json.NewEncoder(w).Encode(map[string]any{"address": "a1b2c3d4e5", "version": "1.14.2", "online": true})
	case r.Method == "GET" && r.URL.Path == "/network":
		list := []map[string]any{}
		for _, n := range z.nets {
			list = append(list, n)
		}
		json.NewEncoder(w).Encode(list)
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/network/"):
		id := strings.TrimPrefix(r.URL.Path, "/network/")
		z.nets[id] = map[string]any{"id": id, "name": "field-net", "status": "OK", "type": "PRIVATE",
			"assignedAddresses": []string{"10.147.17.5/24"}, "portDeviceName": "ztabc"}
		json.NewEncoder(w).Encode(z.nets[id])
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/network/"):
		delete(z.nets, strings.TrimPrefix(r.URL.Path, "/network/"))
		w.Write([]byte("{}"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// TestZeroTier covers the Settings card: connecting to the local ZeroTier
// service with its token, joining and leaving networks, and noticing an
// address the server certificate doesn't cover yet.
func TestZeroTier(t *testing.T) {
	a := startApp(t)
	call, _ := adminJSON(t, a)
	zt := &fakeZeroTier{nets: map[string]map[string]any{}}
	srv := httptest.NewServer(zt)
	defer srv.Close()
	a.Web.ZeroTierAPI = srv.URL

	// No token yet: not connected, and (no installer helper here) no install.
	if code, v := call("GET", "/admin/api/zerotier", nil); code != 200 || v["connected"] != false || v["can_install"] != false {
		t.Fatalf("before = %d %v", code, v)
	}
	if code, _ := call("POST", "/admin/api/zerotier/install", nil); code != 400 {
		t.Errorf("install without the helper = %d", code)
	}

	// The helper gives EdgeTAK the token.
	dir := filepath.Join(a.DataDir, "zerotier")
	os.MkdirAll(dir, 0o750)
	os.WriteFile(filepath.Join(dir, "authtoken"), []byte("zt-token-123\n"), 0o600)
	code, v := call("GET", "/admin/api/zerotier", nil)
	node, _ := v["node"].(map[string]any)
	if code != 200 || v["connected"] != true || node["address"] != "a1b2c3d4e5" || node["online"] != true {
		t.Fatalf("connected = %d %v", code, v)
	}

	if code, _ := call("POST", "/admin/api/zerotier/networks", map[string]string{"network_id": "not-a-network"}); code != 400 {
		t.Errorf("bad network ID = %d", code)
	}
	code, v = call("POST", "/admin/api/zerotier/networks", map[string]string{"network_id": "8056C2E21C000001"})
	nets, _ := v["networks"].([]any)
	if code != 200 || len(nets) != 1 || nets[0].(map[string]any)["id"] != "8056c2e21c000001" {
		t.Fatalf("join = %d %v", code, v)
	}
	// 10.147.17.5 is new to this machine: the certificate doesn't cover it.
	missing, _ := v["cert_missing"].([]any)
	if !slices.Contains(missing, any("10.147.17.5")) {
		t.Errorf("cert_missing = %v", v["cert_missing"])
	}
	code, v = call("DELETE", "/admin/api/zerotier/networks/8056c2e21c000001", nil)
	if nets, _ := v["networks"].([]any); code != 200 || len(nets) != 0 {
		t.Errorf("leave = %d %v", code, v)
	}

	// A stale token is reported, not hidden.
	os.WriteFile(filepath.Join(dir, "authtoken"), []byte("old-token"), 0o600)
	if _, v := call("GET", "/admin/api/zerotier", nil); v["connected"] != false || !strings.Contains(v["error"].(string), "refused") {
		t.Errorf("stale token = %v", v)
	}
}
