package app

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

// TestDeviceHostFromAddressBar: on a local server, packages and QR codes
// use the address the console was opened at (here 192.168.7.7), not the
// configured public_host.
func TestDeviceHostFromAddressBar(t *testing.T) {
	a := startApp(t)
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	do := func(method, path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, "https://"+a.Addrs["https"]+path, strings.NewReader(body))
		req.Host = "192.168.7.7:8443"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-EdgeTAK", "1")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	a.Store.CreateUser("kim", "kims-password-1", []string{"Cyan"}, true)
	if resp := do("POST", "/admin/api/login", `{"username":"kim","password":"kims-password-1"}`); resp.StatusCode != 200 {
		t.Fatalf("login = %d", resp.StatusCode)
	}

	resp := do("GET", "/admin/api/status", "")
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st["device_host"] != "192.168.7.7" {
		t.Errorf("device_host = %v", st["device_host"])
	}

	resp = do("POST", "/admin/api/enroll-codes", `{"username":"kim"}`)
	var v map[string]any
	json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
	if l, _ := v["qr_link"].(string); !strings.Contains(l, "192.168.7.7") {
		t.Errorf("QR link = %v", v["qr_link"])
	}

	resp = do("GET", "/admin/api/connection-package?mode=ssl&username=kim", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("package: %d %v", resp.StatusCode, err)
	}
	found := false
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".pref") {
			rc, _ := f.Open()
			pref, _ := io.ReadAll(rc)
			rc.Close()
			found = bytes.Contains(pref, []byte("192.168.7.7:"))
		}
	}
	if !found {
		t.Error("the package doesn't point at 192.168.7.7")
	}
}
