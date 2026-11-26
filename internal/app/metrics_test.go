package app

import (
	"crypto/tls"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestMetrics covers the Prometheus endpoint: an admin's read-only API key
// scrapes it, a user's key and no key can't, and the numbers follow what
// is connected.
func TestMetrics(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("bob", "password-bob", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, _ := adminJSON(t, a)
	mk := func(body map[string]any) string {
		t.Helper()
		st, v := call("POST", "/admin/api/api-keys", body)
		key, _ := v["key"].(string)
		if st != 200 || key == "" {
			t.Fatalf("make key = %d %v", st, v)
		}
		return key
	}
	adminKey := mk(map[string]any{"name": "prometheus", "read_only": true})
	bobKey := mk(map[string]any{"name": "bob", "username": "bob", "read_only": true})

	cert := enroll(t, a, "bob", "password-bob")
	bob := mustDial(t, a, &cert)
	bob.send(sa("ANDROID-bob", "BOB", "Cyan"))
	waitFor(t, "bob identified", func() bool {
		for _, c := range a.Router.Clients() {
			if uid, _ := c.Identity(); uid == "ANDROID-bob" {
				return true
			}
		}
		return false
	})

	hc := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})} // no client certificate: only the key counts
	scrape := func(key string) (int, string, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", "https://"+a.Addrs["https"]+"/metrics", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}
	if st, _, _ := scrape(""); st != 401 && st != 403 {
		t.Errorf("no key = %d", st)
	}
	if st, _, _ := scrape(bobKey); st != 401 && st != 403 {
		t.Errorf("a user's key = %d", st)
	}
	st, ct, body := scrape(adminKey)
	if st != 200 || !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("scrape = %d %s %s", st, ct, body)
	}
	for _, want := range []string{
		`# TYPE edgetak_cot_received_total counter`,
		`edgetak_info{version="`,
		`edgetak_clients_connected{transport="tls"} 1`,
		`edgetak_devices_identified 1`,
		`edgetak_users 2`,
		`edgetak_admins 1`,
		`edgetak_alerts_active{kind="emergency"} 0`,
		`edgetak_missions 1`, // the Reports plugin's citrap mission
		`edgetak_certificates_valid 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	// Every line is a comment or "name{labels} value".
	line := regexp.MustCompile(`^(# (HELP|TYPE) .+|[a-z_0-9]+(\{[a-z_]+="[^"]*"(,[a-z_]+="[^"]*")*\})? -?[0-9.e+]+)$`)
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		if !line.MatchString(l) {
			t.Errorf("bad line %q", l)
		}
	}
}
