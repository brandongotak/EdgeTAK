package app

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

func TestCertificateStatus(t *testing.T) {
	a := startApp(t)
	call, _ := adminJSON(t, a)
	now := time.Now()
	day := 24 * time.Hour
	add := func(serial, cn, uid string, issued, expires time.Duration, revoked bool) {
		t.Helper()
		if err := a.Store.AddCert(store.Cert{Serial: serial, CommonName: cn, ClientUID: uid, Issued: now.Add(issued), Expires: now.Add(expires), Revoked: revoked}); err != nil {
			t.Fatal(err)
		}
	}
	add("a1", "alice", "phone", -300*day, 65*day, false) // valid
	add("b1", "bob", "phone", -350*day, 10*day, false)   // expiring
	add("c1", "carol", "phone", -400*day, -5*day, false) // expired, not replaced
	add("d1", "dave", "phone", -400*day, -30*day, false) // expired, replaced by d2
	add("d2", "dave", "phone", -30*day, 335*day, false)  // valid
	add("e1", "erin", "tablet", -350*day, 5*day, true)   // revoked
	add("f1", "frank", "phone", -350*day, 20*day, false) // expiring, replaced by f2
	add("f2", "frank", "phone", -2*day, 363*day, false)  // valid

	status := func() (map[string]any, []any) {
		t.Helper()
		code, res := call("GET", "/admin/api/certs/status", nil)
		if code != 200 {
			t.Fatalf("status = %d", code)
		}
		return res["report"].(map[string]any), res["issues"].([]any)
	}
	rep, issues := status()
	if rep["warn_days"] != 30.0 {
		t.Fatalf("default warning period = %v", rep["warn_days"])
	}
	byserial := map[string]map[string]any{}
	for _, c := range rep["certs"].([]any) {
		m := c.(map[string]any)
		byserial[m["serial"].(string)] = m
	}
	want := map[string]struct {
		state               string
		replaced, attention bool
	}{
		"a1": {"valid", false, false}, "b1": {"expiring", false, true}, "c1": {"expired", false, true},
		"d1": {"expired", true, false}, "d2": {"valid", false, false}, "e1": {"revoked", false, false},
		"f1": {"expiring", true, false}, "f2": {"valid", false, false},
	}
	for serial, w := range want {
		c := byserial[serial]
		if c["state"] != w.state || c["replaced"] != w.replaced || c["attention"] != w.attention {
			t.Errorf("%s = %v %v %v, want %+v", serial, c["state"], c["replaced"], c["attention"], w)
		}
	}
	if d := byserial["b1"]["days_left"]; d != 9.0 && d != 10.0 {
		t.Errorf("days left = %v", d)
	}
	sum := rep["summary"].(map[string]any)
	if sum["attention"] != 2.0 || sum["expiring"] != 2.0 || sum["replaced"] != 2.0 {
		t.Errorf("summary = %v", sum)
	}

	// Server certificates: the CA (ten years) and the server certificate (two years) are fine.
	server := rep["server"].([]any)
	if len(server) != 2 {
		t.Fatalf("server certificates = %v", server)
	}
	for _, c := range server {
		if m := c.(map[string]any); m["state"] != "valid" {
			t.Errorf("%v is %v", m["name"], m["state"])
		}
	}

	text := ""
	for _, i := range issues {
		text += i.(map[string]any)["text"].(string) + "\n"
	}
	if !strings.Contains(text, "1 device certificate expires within 30 days") || !strings.Contains(text, "1 device certificate expired and not replaced") {
		t.Errorf("issues = %s", text)
	}
	// The Health tab carries the same warnings.
	if code, h := call("GET", "/admin/api/health", nil); code != 200 || !strings.Contains(stringify(h["issues"]), "device certificate expires") {
		t.Errorf("health issues = %v", h["issues"])
	}

	// A longer warning period flags alice too.
	if code, _ := call("PUT", "/admin/api/certs/settings", map[string]int{"warn_days": 90}); code != http.StatusNoContent {
		t.Fatalf("save = %d", code)
	}
	if rep, _ = status(); rep["summary"].(map[string]any)["attention"] != 3.0 {
		t.Errorf("with 90 days: %v", rep["summary"])
	}
	for _, bad := range []int{0, 400} {
		if code, _ := call("PUT", "/admin/api/certs/settings", map[string]int{"warn_days": bad}); code != http.StatusBadRequest {
			t.Errorf("warn_days %d accepted", bad)
		}
	}
}

func stringify(v any) string {
	var b strings.Builder
	for _, i := range v.([]any) {
		b.WriteString(i.(map[string]any)["text"].(string) + "\n")
	}
	return b.String()
}
