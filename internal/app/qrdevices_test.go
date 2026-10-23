package app

import (
	"crypto/tls"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/pki"
	"software.sslmate.com/src/go-pkcs12"
)

// qrPackageCert downloads a QR package and returns its client certificate.
func qrPackageCert(t *testing.T, a *App, importLink string) tls.Certificate {
	t.Helper()
	for name, data := range qrDownload(t, a, importLink) {
		if name != "certs/server.pref" && name != "MANIFEST/manifest.xml" && !strings.HasPrefix(name, "certs/truststore") {
			key, leaf, _, err := pkcs12.DecodeChain(data, a.Cfg.TruststorePassword)
			if err != nil {
				t.Fatal(err)
			}
			return tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
		}
	}
	t.Fatal("no client certificate in the package")
	return tls.Certificate{}
}

func serialOf(c tls.Certificate) string { return pki.SerialHex(c.Leaf) }

// connected reports whether a device is connected with the certificate.
func connected(a *App, serial string) bool {
	for _, c := range a.Router.Clients() {
		if c.CertSerial == serial {
			return true
		}
	}
	return false
}

// codeDevices is what the code list shows a code set up.
func codeDevices(t *testing.T, a *App, id string) []map[string]any {
	t.Helper()
	_, hc := adminJSON(t, a)
	resp, err := hc.Get("https://" + a.Addrs["https"] + "/admin/api/enroll-codes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	for _, c := range list {
		if c["id"] == id {
			var out []map[string]any
			for _, d := range c["devices"].([]any) {
				out = append(out, d.(map[string]any))
			}
			return out
		}
	}
	t.Fatalf("no code %s", id)
	return nil
}

// TestQRCodeDevices covers what the code list shows a code set up, cutting
// those devices off, and replacement ("new phone") codes.
func TestQRCodeDevices(t *testing.T) {
	a := startApp(t)
	call, _ := adminJSON(t, a)
	a.Store.CreateUser("ivy", "ivys-password-1", []string{"Cyan"}, false)
	mk := func(body map[string]any) (string, map[string]any) {
		t.Helper()
		code, v := call("POST", "/admin/api/enroll-codes", body)
		if code != 201 {
			t.Fatalf("make code = %d %v", code, v)
		}
		return v["id"].(string), v
	}

	// A code's certificate shows as never connected, then online.
	id, v := mk(map[string]any{"username": "ivy", "one_device": true})
	certB := qrPackageCert(t, a, v["package_link"].(string))
	if d := codeDevices(t, a, id); len(d) != 1 || d[0]["serial"] != serialOf(certB) || d[0]["online"] != false || d[0]["connected"] != nil {
		t.Fatalf("before connecting: %v", d)
	}
	devB := mustDial(t, a, &certB)
	devB.send(sa("ANDROID-ivy-b", "IVY-B", "Cyan"))
	waitFor(t, "IVY-B online in the code list", func() bool {
		d := codeDevices(t, a, id)
		return len(d) == 1 && d[0]["online"] == true && d[0]["callsign"] == "IVY-B" && d[0]["connected"] != nil
	})

	// Another device, enrolled with her password.
	certA := enroll(t, a, "ivy", "ivys-password-1")
	mustDial(t, a, &certA)

	// A replacement code: nothing happens until it is scanned...
	rid, rv := mk(map[string]any{"username": "ivy", "replace": true})
	if rv["replace"] != true || rv["one_device"] != true {
		t.Fatalf("replace code = %v", rv)
	}
	if !connected(a, serialOf(certA)) || !connected(a, serialOf(certB)) {
		t.Fatal("devices cut off before the code was scanned")
	}
	// ...then the new phone's package cuts off and revokes the others.
	certC := qrPackageCert(t, a, rv["package_link"].(string))
	waitFor(t, "old devices disconnected", func() bool { return !connected(a, serialOf(certA)) && !connected(a, serialOf(certB)) })
	for _, c := range []tls.Certificate{certA, certB} {
		if !a.Store.IsRevoked(serialOf(c)) {
			t.Errorf("%s not revoked", serialOf(c))
		}
	}
	mustDial(t, a, &certC).send(sa("ANDROID-ivy-c", "IVY-C", "Cyan"))
	if d := codeDevices(t, a, rid); len(d) != 1 || d[0]["serial"] != serialOf(certC) {
		t.Errorf("replace code devices = %v", d)
	}

	// Replacing by enrolling with the code works the same way, and links
	// the enrolled certificate to the code.
	eid, ev := mk(map[string]any{"username": "ivy", "replace": true})
	link, _ := url.Parse(ev["link"].(string))
	certD := enroll(t, a, "ivy", link.Query().Get("token"))
	waitFor(t, "IVY-C disconnected", func() bool { return !connected(a, serialOf(certC)) })
	if !a.Store.IsRevoked(serialOf(certC)) || a.Store.IsRevoked(serialOf(certD)) {
		t.Errorf("after enrolling: C revoked %v, D revoked %v", a.Store.IsRevoked(serialOf(certC)), a.Store.IsRevoked(serialOf(certD)))
	}
	if d := codeDevices(t, a, eid); len(d) != 1 || d[0]["serial"] != serialOf(certD) {
		t.Errorf("enrolled code devices = %v", d)
	}

	// Disconnect: revoking a code with its certificates cuts its device off.
	mustDial(t, a, &certD)
	if code, _ := call("DELETE", "/admin/api/enroll-codes/"+eid+"?certs=1", nil); code != 200 {
		t.Fatalf("disconnect = %d", code)
	}
	waitFor(t, "D disconnected", func() bool { return !connected(a, serialOf(certD)) })
	if !a.Store.IsRevoked(serialOf(certD)) {
		t.Error("D not revoked")
	}

	// A shared event account can't be replaced device by device.
	call("POST", "/admin/api/events", map[string]any{"name": "Walk In", "mode": "shared", "expires": "2099-01-01T00:00:00Z"})
	acct := eventByName(t, a, "Walk In")["account"].(string)
	if code, _ := call("POST", "/admin/api/enroll-codes", map[string]any{"username": acct, "replace": true}); code != 400 {
		t.Errorf("replace on a shared account = %d", code)
	}
}
