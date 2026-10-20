package tlstrace

import (
	"bytes"
	"crypto/tls"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/edgetak/edgetak/internal/pki"
)

// handshake runs a mutual-TLS handshake through a traced server conn and
// returns what was logged. withCert controls whether the client presents a
// certificate issued by the server's CA.
func handshake(t *testing.T, withCert bool) string {
	t.Helper()
	ca, err := pki.Load(t.TempDir(), "T", "T", []string{"localhost"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var clientCerts []tls.Certificate
	if withCert {
		cert, key, err := ca.NewClient("u")
		if err != nil {
			t.Fatal(err)
		}
		clientCerts = []tls.Certificate{{Certificate: [][]byte{cert.Raw}, PrivateKey: key}}
	}

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srvRaw, cliRaw := net.Pipe()
	traced := &conn{Conn: srvRaw, log: log, name: "test"}
	traced.in.dir, traced.out.dir = "C>S", "S>C"

	srv := tls.Server(traced, &tls.Config{Certificates: []tls.Certificate{ca.ServerCert}, ClientCAs: ca.Pool(),
		ClientAuth: tls.RequireAndVerifyClientCert, MaxVersion: tls.VersionTLS12})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if srv.Handshake() == nil {
			srv.Read(make([]byte, 1)) // first application data completes the trace
			srv.Write([]byte("y"))
		}
		srv.Close()
	}()
	cli := tls.Client(cliRaw, &tls.Config{RootCAs: ca.Pool(), ServerName: "localhost", Certificates: clientCerts})
	if cli.Handshake() == nil {
		cli.Write([]byte("x"))
		cli.Read(make([]byte, 1))
	}
	cli.Close()
	<-done
	return logs.String()
}

func TestTraceFailedHandshake(t *testing.T) {
	out := handshake(t, false)
	for _, want := range []string{
		"ClientHello(TLS1.2 sid=", "S>C [TLS1.2] ServerHello(TLS1.2", "CertificateRequest", "ServerHelloDone",
		"C>S [TLS1.2] Certificate(empty)", "Alert(fatal",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("trace missing %q:\n%s", want, out)
		}
	}
}

func TestNoTraceOnSuccess(t *testing.T) {
	if out := handshake(t, true); out != "" {
		t.Errorf("successful handshake was logged:\n%s", out)
	}
}
