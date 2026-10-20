// Package pki manages EdgeTAK's certificate authority: it creates the CA and
// server certificate on first start, signs client CSRs for enrollment and
// produces PKCS#12 trust/key stores for ATAK.
package pki

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

const keyBits = 2048

// CA is the server's certificate authority plus its TLS server certificate.
type CA struct {
	Cert       *x509.Certificate
	key        crypto.Signer
	ServerCert tls.Certificate
	org, ou    string
	validity   time.Duration
}

// Load opens (or creates) the CA in dir and ensures the server certificate
// covers hosts, reissuing it when the host list changes.
func Load(dir, org, ou string, hosts []string, validity time.Duration) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ca := &CA{org: org, ou: ou, validity: validity}
	caCert, caKey := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	if _, err := os.Stat(caCert); errors.Is(err, os.ErrNotExist) {
		if err := ca.create(caCert, caKey); err != nil {
			return nil, fmt.Errorf("create CA: %w", err)
		}
	} else {
		if ca.Cert, ca.key, err = loadPair(caCert, caKey); err != nil {
			return nil, fmt.Errorf("load CA: %w", err)
		}
	}
	if err := ca.ensureServer(dir, hosts); err != nil {
		return nil, fmt.Errorf("server cert: %w", err)
	}
	return ca, nil
}

func (ca *CA) create(certPath, keyPath string) error {
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return err
	}
	serial, err := newSerial()
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: fmt.Sprintf("%s CA %s", ca.org, serial.Text(16)[:6]), Organization: []string{ca.org}, OrganizationalUnit: []string{ca.ou}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	if ca.Cert, err = x509.ParseCertificate(der); err != nil {
		return err
	}
	ca.key = key
	return writePair(certPath, keyPath, der, key)
}

func (ca *CA) ensureServer(dir string, hosts []string) error {
	certPath, keyPath, hostsPath := filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"), filepath.Join(dir, "server.hosts")
	hosts = slices.Compact(slices.Sorted(slices.Values(hosts)))
	want := strings.Join(hosts, "\n")
	if old, err := os.ReadFile(hostsPath); err == nil && string(old) == want {
		if pair, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			if leaf, err := x509.ParseCertificate(pair.Certificate[0]); err == nil && time.Until(leaf.NotAfter) > 30*24*time.Hour {
				pair.Leaf = leaf
				if len(pair.Certificate) == 1 {
					// Older installs stored only the leaf; always present the full chain.
					pair.Certificate = append(pair.Certificate, ca.Cert.Raw)
				}
				ca.ServerCert = pair
				return nil
			}
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return err
	}
	serial, err := newSerial()
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hosts[0], Organization: []string{ca.org}, OrganizationalUnit: []string{ca.ou}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.key)
	if err != nil {
		return err
	}
	if err := writePair(certPath, keyPath, der, key); err != nil {
		return err
	}
	if err := os.WriteFile(hostsPath, []byte(want), 0o600); err != nil {
		return err
	}
	leaf, _ := x509.ParseCertificate(der)
	ca.ServerCert = tls.Certificate{Certificate: [][]byte{der, ca.Cert.Raw}, PrivateKey: key, Leaf: leaf}
	return nil
}

// Pool returns a cert pool containing only this CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// SignCSR issues a client certificate for cn using the CSR's public key. The
// subject is set by the server, not taken from the CSR.
func (ca *CA) SignCSR(csr *x509.CertificateRequest, cn string) (*x509.Certificate, error) {
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr signature: %w", err)
	}
	return ca.issueClient(csr.PublicKey, cn)
}

// NewClient generates a key pair and client certificate for cn.
func (ca *CA) NewClient(cn string) (*x509.Certificate, *rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, nil, err
	}
	cert, err := ca.issueClient(&key.PublicKey, cn)
	return cert, key, err
}

func (ca *CA) issueClient(pub any, cn string) (*x509.Certificate, error) {
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{ca.org}, OrganizationalUnit: []string{ca.ou}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(ca.validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// TrustStoreP12 returns the CA as a password-protected PKCS#12 truststore.
// Legacy (3DES) encryption is used because older Android releases cannot
// read the modern AES variant.
func (ca *CA) TrustStoreP12(password string) ([]byte, error) {
	return pkcs12.LegacyDES.EncodeTrustStore([]*x509.Certificate{ca.Cert}, password)
}

// ClientP12 bundles a client key and certificate (plus the CA) as PKCS#12.
func (ca *CA) ClientP12(cert *x509.Certificate, key *rsa.PrivateKey, password string) ([]byte, error) {
	return pkcs12.LegacyDES.Encode(key, cert, []*x509.Certificate{ca.Cert}, password)
}

// ParseCSR accepts a PEM CSR, or bare base64 DER as some clients send.
func ParseCSR(body []byte) (*x509.CertificateRequest, error) {
	if block, _ := pem.Decode(body); block != nil {
		return x509.ParseCertificateRequest(block.Bytes)
	}
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, string(body))
	der, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("csr: not PEM or base64: %w", err)
	}
	return x509.ParseCertificateRequest(der)
}

// PEMBody encodes a certificate as PEM without header lines, matching the
// official server's enrollment responses.
func PEMBody(cert *x509.Certificate) string {
	enc := base64.StdEncoding.EncodeToString(cert.Raw)
	var b strings.Builder
	for len(enc) > 64 {
		b.WriteString(enc[:64])
		b.WriteByte('\n')
		enc = enc[64:]
	}
	b.WriteString(enc)
	return b.String()
}

// SerialHex formats a certificate serial the way the store keys it.
func SerialHex(c *x509.Certificate) string { return c.SerialNumber.Text(16) }

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func writePair(certPath, keyPath string, der []byte, key *rsa.PrivateKey) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func loadPair(certPath, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("CA key is not a signer")
	}
	return cert, signer, nil
}

// DetectHosts returns this machine's hostname, non-loopback IPs and localhost.
func DetectHosts() []string {
	var hosts []string
	if h, err := os.Hostname(); err == nil && h != "" {
		hosts = append(hosts, h)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				hosts = append(hosts, ipn.IP.String())
			}
		}
	}
	return append(hosts, "localhost", "127.0.0.1")
}

// PrimaryIP returns the first non-loopback IPv4 address, or "".
func PrimaryIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	return ""
}
