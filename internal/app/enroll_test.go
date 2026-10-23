package app

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// TestEnrollmentForms covers the original signClient API (PKCS#12 reply)
// and the CSR encodings clients send.
func TestEnrollmentForms(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("ios1", "ios1-password", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "ios1"}}, key)

	post := func(path string, body []byte) (int, []byte) {
		req, _ := http.NewRequest("POST", "https://"+a.Addrs["enroll"]+path, bytes.NewReader(body))
		req.SetBasicAuth("ios1", "ios1-password")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	// v1: base64 CSR in, PKCS#12 (signedCert + ca0, password atakatak) out.
	st, p12 := post("/Marti/api/tls/signClient?clientUid=IOS-1&version=2", []byte(base64.StdEncoding.EncodeToString(der)))
	if st != 200 {
		t.Fatalf("signClient v1 = %d %s", st, p12)
	}
	certs, err := pkcs12.DecodeTrustStore(p12, "atakatak")
	if err != nil || len(certs) != 2 {
		t.Fatalf("v1 keystore: %v (%d certs)", err, len(certs))
	}
	var signed, ca *x509.Certificate
	for _, c := range certs {
		if c.Subject.CommonName == "ios1" {
			signed = c
		} else if c.Equal(a.CA.Cert) {
			ca = c
		}
	}
	if signed == nil || ca == nil {
		t.Fatalf("v1 keystore lacks the signed cert or the CA: %v", certs)
	}
	if _, err := signed.Verify(x509.VerifyOptions{Roots: a.CA.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("v1 cert does not chain: %v", err)
	}

	// v2 accepts raw DER and URL-safe base64 as well as PEM.
	for name, body := range map[string][]byte{
		"der":           der,
		"base64url":     []byte(base64.RawURLEncoding.EncodeToString(der)),
		"base64 broken": []byte(base64.StdEncoding.EncodeToString(der)[:40] + "\n" + base64.StdEncoding.EncodeToString(der)[40:]),
	} {
		if st, b := post("/Marti/api/tls/signClient/v2?clientUid=IOS-1", body); st != 200 {
			t.Errorf("v2 with %s CSR = %d %s", name, st, b)
		}
	}
	if st, _ := post("/Marti/api/tls/signClient/v2", []byte("not a csr")); st != 400 {
		t.Errorf("garbage CSR = %d", st)
	}

	// iTAK sends Accept: text/plain and only accepts JSON labelled text/plain.
	req, _ := http.NewRequest("POST", "https://"+a.Addrs["enroll"]+"/Marti/api/tls/signClient/v2?clientUid=DE178FC4&version=2.12.3",
		bytes.NewReader([]byte(base64.StdEncoding.EncodeToString(der))))
	req.SetBasicAuth("ios1", "ios1-password")
	req.Header.Set("Accept", "text/plain")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	err = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != 200 || !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("iTAK enrollment: %d %q", resp.StatusCode, ct)
	}
	if err != nil || out["signedCert"] == "" || out["ca0"] == "" || out["ca1"] != out["ca0"] {
		t.Errorf("iTAK enrollment body: %v %v", err, out)
	}

	// iTAK as it actually calls it: Accept */* with its User-Agent. It gets
	// ca0 and ca1 as JSON; other clients keep the official ca0-only reply.
	enrollAs := func(agent string) (string, map[string]string) {
		req, _ := http.NewRequest("POST", "https://"+a.Addrs["enroll"]+"/Marti/api/tls/signClient/v2?clientUid=X",
			bytes.NewReader([]byte(base64.StdEncoding.EncodeToString(der))))
		req.SetBasicAuth("ios1", "ios1-password")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("User-Agent", agent)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]string
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.Header.Get("Content-Type"), m
	}
	if ct, m := enrollAs("iTAK%20App%20Store/651 CFNetwork/3896.100.1.2.1 Darwin/27.0.0"); ct != "application/json" || m["ca1"] == "" || m["ca1"] != m["ca0"] {
		t.Errorf("iTAK */* enrollment: %q %v", ct, m)
	}
	// TROP sends no User-Agent and needs ca1 too, and, like any iOS
	// decoder, base64 without line breaks.
	ct, m := enrollAs("")
	if ct != "application/json" || m["ca1"] == "" || m["ca1"] != m["ca0"] {
		t.Errorf("TROP enrollment: %q %v", ct, m)
	}
	for _, k := range []string{"signedCert", "ca0"} {
		raw, err := base64.StdEncoding.Strict().DecodeString(m[k])
		if err != nil || strings.Contains(m[k], "\n") {
			t.Errorf("TROP %s isn't one-line base64: %v", k, err)
		} else if _, err := x509.ParseCertificate(raw); err != nil {
			t.Errorf("TROP %s: %v", k, err)
		}
	}
	if _, m := enrollAs("TAK/5.5 (Android)"); m["ca0"] == "" || m["ca1"] != "" || !strings.Contains(m["signedCert"], "\n") {
		t.Errorf("non-iTAK enrollment changed: %v", m)
	}
}
