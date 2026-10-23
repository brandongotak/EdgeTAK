package app

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

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
