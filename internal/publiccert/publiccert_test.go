package publiccert

import (
	"crypto/tls"
	"io"
	"log/slog"
	"testing"

	"golang.org/x/crypto/acme"

	"github.com/brandongotak/EdgeTAK/internal/config"
)

// When Let's Encrypt is unreachable, handshakes fall back to the EdgeTAK
// certificate instead of failing.
func TestFallbackWhenACMEUnavailable(t *testing.T) {
	fallback := &tls.Certificate{Certificate: [][]byte{[]byte("edgetak")}}
	p, err := New(config.PublicCert{Mode: "letsencrypt", Domains: []string{"edge.example.org"}, Listeners: []string{"enroll"}},
		t.TempDir(), fallback, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	p.acme.Client = &acme.Client{DirectoryURL: "http://127.0.0.1:1/directory"} // nothing listens here
	got, err := p.GetCertificate(&tls.ClientHelloInfo{ServerName: "edge.example.org",
		CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}})
	if err != nil || got != fallback {
		t.Errorf("GetCertificate = %v, %v; want fallback", got, err)
	}
	if !p.Uses("enroll") || p.Uses("tls") {
		t.Error("listener selection wrong")
	}
	if info := p.Info(); info["status"] != "pending" {
		t.Errorf("info = %v", info)
	}
}

func TestOffAndBadConfig(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if p, err := New(config.PublicCert{}, t.TempDir(), nil, log); p != nil || err != nil {
		t.Errorf("mode off = %v, %v", p, err)
	}
	if _, err := New(config.PublicCert{Mode: "letsencrypt"}, t.TempDir(), nil, log); err == nil {
		t.Error("letsencrypt without a domain accepted")
	}
	if _, err := New(config.PublicCert{Mode: "files"}, t.TempDir(), nil, log); err == nil {
		t.Error("files mode without paths accepted")
	}
	var nilP *Provider
	if nilP.Uses("enroll") || nilP.Info()["mode"] != "off" {
		t.Error("nil provider should be inert")
	}
}
