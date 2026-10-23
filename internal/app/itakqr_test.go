package app

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/config"
	"software.sslmate.com/src/go-pkcs12"
)

// TestITAKQR covers iTAK's QR codes: a package download on a local server,
// and iTAK's own quick-connect code plus a typeable code with Let's Encrypt.
func TestITAKQR(t *testing.T) {
	t.Run("local server: a package download", func(t *testing.T) {
		a := startApp(t)
		call, _ := adminJSON(t, a)
		a.Store.CreateUser("ivan", "ivans-password-1", []string{"Cyan"}, false)
		code, v := call("POST", "/admin/api/enroll-codes", map[string]any{"username": "ivan", "app": "itak", "one_device": true})
		link, _ := v["qr_link"].(string)
		if code != 201 || v["app"] != "itak" || v["password"] != nil || !strings.HasPrefix(link, "http://127.0.0.1:"+port(a.Addrs["http"])+"/qr/") || !strings.HasSuffix(link, "-ivan-itak.zip") {
			t.Fatalf("iTAK code = %d %v", code, v)
		}
		resp, err := (&http.Client{Transport: memTransport(nil)}).Get(link)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if resp.StatusCode != 200 || err != nil {
			t.Fatalf("download = %d %v", resp.StatusCode, err)
		}
		files := map[string][]byte{}
		for _, f := range zr.File {
			rc, _ := f.Open()
			files[f.Name], _ = io.ReadAll(rc)
			rc.Close()
		}
		// iTAK's flat package: config.pref, its certificate and the truststore.
		if files["config.pref"] == nil || files["ivan.p12"] == nil || files["truststore-root.p12"] == nil {
			t.Fatalf("package files = %v", len(files))
		}
		key, leaf, _, err := pkcs12.DecodeChain(files["ivan.p12"], a.Cfg.TruststorePassword)
		if err != nil || leaf.Subject.CommonName != "ivan" {
			t.Fatalf("client p12: %v", err)
		}
		cert := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
		mustDial(t, a, &cert).conn.Close()
		// The code knows the certificate it issued.
		if d := codeDevices(t, a, v["id"].(string)); len(d) != 1 || d[0]["serial"] != serialOf(cert) {
			t.Errorf("code devices = %v", d)
		}
	})

	t.Run("Let's Encrypt: a registration code with the login", func(t *testing.T) {
		const domain = "edge.example.org"
		publicRoots, certFile, keyFile := fakePublicCA(t, domain)
		a := startAppWith(t, func(c *config.Config) {
			c.PublicHost = domain
			c.PublicCert = config.PublicCert{Mode: "files", CertFile: certFile, KeyFile: keyFile}
		})
		call, _ := adminJSON(t, a)
		a.Store.CreateUser("ivy", "ivys-password-1", []string{"Cyan"}, false)
		code, v := call("POST", "/admin/api/enroll-codes", map[string]any{"username": "ivy", "app": "itak", "host": domain})
		var reg struct {
			Type   string `json:"type"`
			Server struct {
				ConnectionString string `json:"connectionString"`
			} `json:"serverCredentials"`
			User struct {
				Username, Password, RegistrationID string
			} `json:"userCredentials"`
		}
		link, _ := v["qr_link"].(string)
		if code != 201 || json.Unmarshal([]byte(link), &reg) != nil || reg.Type != "registration" ||
			reg.Server.ConnectionString != domain+":"+port(a.Addrs["tls"])+":ssl" || reg.User.Username != "ivy" || reg.User.Password == "" ||
			strings.Contains(link, "ivys-password-1") {
			t.Fatalf("iTAK code = %d %v", code, v)
		}
		pw := reg.User.Password
		// iTAK signs in with the code as the password, on the enrollment port.
		device := &http.Client{Transport: memTransport(&tls.Config{RootCAs: publicRoots, ServerName: domain})}
		req, _ := http.NewRequest("GET", "https://"+a.Addrs["enroll"]+"/Marti/api/tls/config", nil)
		req.SetBasicAuth("ivy", pw)
		resp, err := device.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("iTAK enrollment with the code = %v %v", err, resp)
		}
		resp.Body.Close()
	})
}
