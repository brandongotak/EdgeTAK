package pki

import (
	"crypto/x509"
	"testing"
	"time"
)

func TestServerChain(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 2; i++ { // second pass exercises reload from disk
		ca, err := Load(dir, "EdgeTAK", "EdgeTAK", []string{"127.0.0.1", "localhost"}, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		leaf := ca.ServerCert.Leaf
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: ca.Pool(), DNSName: "localhost"}); err != nil {
			t.Fatalf("pass %d: server cert does not chain to CA: %v (issuer %q, ca subject %q)", i, err, leaf.Issuer, ca.Cert.Subject)
		}
		client, _, err := ca.NewClient("alice")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Verify(x509.VerifyOptions{Roots: ca.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			t.Fatalf("pass %d: client cert: %v", i, err)
		}
	}
}
