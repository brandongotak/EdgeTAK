package app

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/config"
)

// fakePublicCA stands in for Let's Encrypt: a root the "device" trusts and a
// leaf for the server's domain, written to files like certbot's.
func fakePublicCA(t *testing.T, domain string) (root *x509.CertPool, certFile, keyFile string) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fake Public Root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafDER, _ := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: domain},
		DNSNames: []string{domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, caCert, &leafKey.PublicKey, caKey)
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(leafKey)
	os.WriteFile(certFile, chain, 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
	root = x509.NewCertPool()
	root.AddCert(caCert)
	return root, certFile, keyFile
}

// TestEnrollWithOnlyAddressUserPassword is the "quick connect" flow: a device
// with no truststore, only its built-in roots, enrolls on 8446 using the
// public certificate, then trusts 8089 via the CA returned by enrollment.
func TestEnrollWithOnlyAddressUserPassword(t *testing.T) {
	const domain = "edge.example.org"
	publicRoots, certFile, keyFile := fakePublicCA(t, domain)
	a := startAppWith(t, func(c *config.Config) {
		c.PublicHost = domain
		c.PublicCert = config.PublicCert{Mode: "files", CertFile: certFile, KeyFile: keyFile}
	})
	if _, err := a.Store.CreateUser("field1", "field1-password", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}

	// 1. Enrollment trusts only the public roots.
	device := &http.Client{Transport: memTransport(&tls.Config{RootCAs: publicRoots, ServerName: domain})}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "field1"}}, key)
	req, _ := http.NewRequest("POST", "https://"+a.Addrs["enroll"]+"/Marti/api/tls/signClient/v2?clientUid=ANDROID-field1",
		bytes.NewReader(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})))
	req.SetBasicAuth("field1", "field1-password")
	resp, err := device.Do(req)
	if err != nil {
		t.Fatalf("enrollment with public trust failed: %v", err)
	}
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("enroll status %d", resp.StatusCode)
	}
	if issuer := resp.TLS.PeerCertificates[0].Issuer.CommonName; issuer != "Fake Public Root" {
		t.Errorf("enrollment port served cert issued by %q", issuer)
	}
	der := func(s string) []byte {
		b, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(s, "\n", ""))
		return b
	}
	signed, _ := x509.ParseCertificate(der(out["signedCert"]))
	ca, err := x509.ParseCertificate(der(out["ca0"]))
	if err != nil || signed == nil {
		t.Fatalf("bad enrollment response: %v", out)
	}

	// 2. Streaming trusts only the CA handed out by enrollment.
	enrolledTrust := x509.NewCertPool()
	enrolledTrust.AddCert(ca)
	clientCert := tls.Certificate{Certificate: [][]byte{signed.Raw}, PrivateKey: key}
	conn, err := tlsDial(a.Addrs["tls"], &tls.Config{RootCAs: enrolledTrust, ServerName: domain, Certificates: []tls.Certificate{clientCert}})
	if err != nil {
		t.Fatalf("streaming with the enrolled CA failed: %v", err)
	}
	conn.Close()

	// 3. The API port keeps the EdgeTAK certificate (packages trust only the EdgeTAK CA).
	api := &http.Client{Transport: memTransport(&tls.Config{RootCAs: enrolledTrust, ServerName: domain, Certificates: []tls.Certificate{clientCert}})}
	resp, err = api.Get("https://" + a.Addrs["https"] + "/Marti/api/version")
	if err != nil {
		t.Fatalf("API with the enrolled CA failed: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// 4. The admin console reports the certificate.
	if info := a.Public.Info(); info["mode"] != "files" || info["issuer"] != "Fake Public Root" {
		t.Errorf("public cert info = %v", info)
	}
}
