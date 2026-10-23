package app

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"software.sslmate.com/src/go-pkcs12"
)

// TestQRPackage covers the QR code for servers without Let's Encrypt: it
// imports a connection package with a client certificate, so ATAK
// connects without enrolling.
func TestQRPackage(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("erin", "erins-real-password", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, _ := adminJSON(t, a)
	code, v := call("POST", "/admin/api/enroll-codes", map[string]any{"username": "erin", "hours": 1, "one_device": true})
	if code != 201 || v["local"] != true {
		t.Fatalf("make code = %d %v", code, v)
	}
	link, _ := url.Parse(v["package_link"].(string))
	if link.Scheme != "tak" || link.Host != "com.atakmap.app" || link.Path != "/import" {
		t.Fatalf("package link = %s", link)
	}
	pkgURL := link.Query().Get("url")
	if !strings.HasPrefix(pkgURL, "http://127.0.0.1:"+port(a.Addrs["http"])+"/qr/") || !strings.HasSuffix(pkgURL, "-erin.zip") {
		t.Fatalf("package url = %s", pkgURL)
	}

	plain := &http.Client{Transport: memTransport(nil)}
	get := func(u string) (*http.Response, []byte) {
		t.Helper()
		// The test server listens on the in-memory network; 127.0.0.1:port is its address.
		resp, err := plain.Get(strings.Replace(u, "127.0.0.1:"+port(a.Addrs["http"]), a.Addrs["http"], 1))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, body
	}
	resp, body := get(pkgURL)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("download = %d %s", resp.StatusCode, body)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		files[f.Name], _ = io.ReadAll(rc)
		rc.Close()
	}
	pref := files["certs/server.pref"]
	for _, want := range []string{"cert/erin.p12", "certificateLocation0", "caLocation0", "127.0.0.1:" + port(a.Addrs["tls"]) + ":ssl"} {
		if !bytes.Contains(pref, []byte(want)) {
			t.Errorf("server.pref missing %q:\n%s", want, pref)
		}
	}
	// No enrollment (it would ask for a password), and the device's
	// default truststore is left alone.
	for _, unwanted := range []string{"enrollForCertificateWithTrust", `key="caLocation"`, "erins-real-password"} {
		if bytes.Contains(pref, []byte(unwanted)) {
			t.Errorf("server.pref has %q", unwanted)
		}
	}
	if !bytes.Contains(files["MANIFEST/manifest.xml"], []byte(`zipEntry="certs/erin.p12"`)) {
		t.Errorf("manifest:\n%s", files["MANIFEST/manifest.xml"])
	}
	key, leaf, _, err := pkcs12.DecodeChain(files["certs/erin.p12"], a.Cfg.TruststorePassword)
	if err != nil || leaf.Subject.CommonName != "erin" {
		t.Fatalf("client p12: %v", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
	dev := mustDial(t, a, &cert)
	dev.send(sa("ANDROID-erin", "ERIN", "Cyan"))
	waitFor(t, "erin connected", func() bool {
		for _, c := range a.Router.Clients() {
			if c.Username == "erin" {
				return true
			}
		}
		return false
	})

	// The certificate is listed (so it can be revoked) and the download is audited.
	found := false
	for _, c := range a.Store.Certs() {
		found = found || c.ClientUID == "qr-package" && c.CommonName == "erin"
	}
	if !found {
		t.Error("package certificate not recorded")
	}

	// A wrong code gets nothing; a revoked one neither.
	if resp, _ := get(strings.Replace(pkgURL, "/qr/", "/qr/x", 1)); resp.StatusCode != 404 {
		t.Errorf("wrong code = %d", resp.StatusCode)
	}
	codes := v["codes"].([]any)
	call("DELETE", "/admin/api/enroll-codes/"+codes[0].(map[string]any)["id"].(string), nil)
	if resp, _ := get(pkgURL); resp.StatusCode != 404 {
		t.Errorf("revoked code = %d", resp.StatusCode)
	}
}

// qrDownload follows an ATAK import link (as ATAK does) and returns the
// package's files, failing unless it downloads.
func qrDownload(t *testing.T, a *App, importLink string) map[string][]byte {
	t.Helper()
	link, err := url.Parse(importLink)
	if err != nil || link.Scheme != "tak" || link.Path != "/import" {
		t.Fatalf("not an import link: %s", importLink)
	}
	resp, err := (&http.Client{Transport: memTransport(nil)}).Get(link.Query().Get("url"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download = %d %s", resp.StatusCode, body)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		files[f.Name], _ = io.ReadAll(rc)
		rc.Close()
	}
	return files
}

// qrPackageUser is the user a downloaded QR package's certificate is for.
func qrPackageUser(t *testing.T, a *App, files map[string][]byte) string {
	t.Helper()
	for name, data := range files {
		if strings.HasPrefix(name, "certs/") && strings.HasSuffix(name, ".p12") && !strings.Contains(name, "truststore") {
			_, leaf, _, err := pkcs12.DecodeChain(data, a.Cfg.TruststorePassword)
			if err != nil {
				t.Fatal(err)
			}
			return leaf.Subject.CommonName
		}
	}
	t.Fatalf("no client certificate in %v", files)
	return ""
}

// TestQRCodesEverywhere covers the other QR codes on a server without
// Let's Encrypt: the account page's, the one after signing up, a shared
// event's, and a printed sheet's all set up ATAK with a package.
func TestQRCodesEverywhere(t *testing.T) {
	a := startApp(t)
	call, _ := adminJSON(t, a)
	a.Store.CreateUser("fay", "fays-password-1", []string{"Cyan"}, false)
	a.Store.CreateUser("gus", "guss-password-1", []string{"Cyan"}, false)
	call("PATCH", "/admin/api/users/gus", map[string]bool{"disabled": true})

	t.Run("account page", func(t *testing.T) {
		acct, _ := accountClient(t, a, "fay", "fays-password-1")
		code, v := acct("POST", "/account/api/qr", nil)
		if code != 200 {
			t.Fatalf("account qr = %d %v", code, v)
		}
		if u := qrPackageUser(t, a, qrDownload(t, a, v["link"].(string))); u != "fay" {
			t.Errorf("certificate for %q", u)
		}
	})

	t.Run("sign-up", func(t *testing.T) {
		call("PUT", "/admin/api/registration", map[string]any{"mode": "open", "default_groups": []string{"Cyan"}})
		hc := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
		resp, err := hc.Post("https://"+a.Addrs["enroll"]+"/api/register", "application/json",
			strings.NewReader(`{"username":"hal","password":"hals-password-1"}`))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if resp.StatusCode != 201 || v["qr_link"] == nil {
			t.Fatalf("sign-up = %d %v", resp.StatusCode, v)
		}
		if u := qrPackageUser(t, a, qrDownload(t, a, v["qr_link"].(string))); u != "hal" {
			t.Errorf("certificate for %q", u)
		}
	})

	t.Run("shared event", func(t *testing.T) {
		if code, v := call("POST", "/admin/api/events", map[string]any{"name": "Walk Up", "mode": "shared", "expires": time.Now().Add(time.Hour)}); code != 201 {
			t.Fatalf("event = %d %v", code, v)
		}
		ev := eventByName(t, a, "Walk Up")
		files := qrDownload(t, a, ev["enroll_link"].(string))
		if u := qrPackageUser(t, a, files); u != ev["account"] {
			t.Errorf("certificate for %q, want %v", u, ev["account"])
		}
		// Many devices share it; it stops when the event ends.
		qrDownload(t, a, ev["enroll_link"].(string))
		call("POST", "/admin/api/events/"+ev["id"].(string)+"/end", nil)
		link, _ := url.Parse(ev["enroll_link"].(string))
		resp, err := (&http.Client{Transport: memTransport(nil)}).Get(link.Query().Get("url"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("after the event = %d", resp.StatusCode)
		}
	})

	t.Run("sheet", func(t *testing.T) {
		code, v := call("POST", "/admin/api/enroll-codes/batch", map[string]any{"usernames": []string{"fay", "gus", "nobody", "fay"}, "hours": 24, "one_device": true})
		if code != 201 {
			t.Fatalf("batch = %d %v", code, v)
		}
		cards, skipped := v["cards"].([]any), v["skipped"].([]any)
		if len(cards) != 1 || len(skipped) != 2 {
			t.Fatalf("cards %v, skipped %v", cards, skipped)
		}
		card := cards[0].(map[string]any)
		if u := qrPackageUser(t, a, qrDownload(t, a, card["qr_link"].(string))); card["username"] != "fay" || u != "fay" {
			t.Errorf("card %v, certificate for %q", card, u)
		}
		if code, _ := call("POST", "/admin/api/enroll-codes/batch", map[string]any{"usernames": []string{}}); code != 400 {
			t.Errorf("empty batch = %d", code)
		}
	})
}

// eventByName is the admin view of an event.
func eventByName(t *testing.T, a *App, name string) map[string]any {
	t.Helper()
	_, hc := adminJSON(t, a)
	resp, err := hc.Get("https://" + a.Addrs["https"] + "/admin/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	for _, e := range list {
		if e["name"] == name {
			return e
		}
	}
	t.Fatalf("no event %q in %v", name, list)
	return nil
}
