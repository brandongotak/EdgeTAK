package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/sim"
)

// runSim is `edgetak sim`: simulated devices against a running EdgeTAK (or
// any TAK server) from this computer, for load testing.
func runSim(args []string) int {
	fs := flag.NewFlagSet("edgetak sim", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `Usage: edgetak sim -server HOST -user NAME -lat LAT -lon LON [options]

Simulated TAK devices connect to the server's streaming port, walk around
LAT,LON sending position reports (and chat, with -chat), and report what
they send and receive and how long the server takes to relay positions.
They sign in as -user (its password from -password or EDGETAK_SIM_PASSWORD),
so they are in that user's groups: use an account in a test group.

Options:`)
		fs.PrintDefaults()
	}
	server := fs.String("server", "", "the server's address (`host`)")
	port := fs.Int("port", 8089, "its TLS streaming port")
	enrollPort := fs.Int("enroll-port", 8446, "its enrollment port (TLS certificates)")
	user := fs.String("user", "", "account the devices sign in as")
	password := fs.String("password", os.Getenv("EDGETAK_SIM_PASSWORD"), "its password (or set EDGETAK_SIM_PASSWORD)")
	plain := fs.Bool("tcp", false, "use the plain TCP streaming port instead (default port 8087, no account)")
	insecure := fs.Bool("insecure", false, "don't check the server's certificates (a self-signed enrollment port, or -server is an address its certificate doesn't name)")
	devices := fs.Int("devices", 100, "how many devices")
	ramp := fs.Float64("ramp", 50, "devices connecting per second")
	interval := fs.Duration("interval", 5*time.Second, "between each device's position reports")
	chat := fs.Duration("chat", 0, "between each device's chat messages (0 = none)")
	duration := fs.Duration("duration", 0, "how long to run (0 = until Ctrl-C)")
	lat := fs.Float64("lat", 0, "latitude the devices walk around")
	lon := fs.Float64("lon", 0, "longitude the devices walk around")
	radius := fs.Float64("radius", 2000, "how far they walk, in metres")
	every := fs.Duration("every", 5*time.Second, "how often to print statistics")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "edgetak sim: "+format+"\n", a...)
		return 1
	}
	switch {
	case *server == "":
		return fail("-server is required")
	case *lat == 0 && *lon == 0:
		return fail("-lat and -lon are required (where the devices are)")
	case *devices < 1:
		return fail("-devices must be at least 1")
	}
	cfg := sim.Config{Devices: *devices, Ramp: *ramp, Interval: *interval, ChatEvery: *chat, Duration: *duration,
		Center: [2]float64{*lat, *lon}, Radius: *radius, Version: version}
	if *plain {
		p := *port
		if p == 8089 {
			p = 8087
		}
		cfg.Addr = net.JoinHostPort(*server, strconv.Itoa(p))
	} else {
		if *user == "" || *password == "" {
			return fail("-user and -password (or EDGETAK_SIM_PASSWORD) are required for TLS; or use -tcp")
		}
		tc, err := simEnroll(*server, *enrollPort, *user, *password, *insecure)
		if err != nil {
			return fail("%v", err)
		}
		cfg.Addr, cfg.TLS = net.JoinHostPort(*server, strconv.Itoa(*port)), tc
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	run, err := sim.Start(ctx, cfg)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("Simulating %d devices against %s (a report every %s", *devices, cfg.Addr, *interval)
	if *chat > 0 {
		fmt.Printf(", chat every %s", *chat)
	}
	fmt.Println("). Ctrl-C to stop.")
	t := time.NewTicker(*every)
	defer t.Stop()
	for {
		select {
		case <-run.Done():
			st := run.Stats()
			fmt.Println("\nFinished.")
			printSimStats(st)
			fmt.Printf("  totals    sent %d events (%s), received %d (%s), %d connections, %d failed, %d dropped\n",
				st.Sent, simBytes(st.BytesOut), st.Received, simBytes(st.BytesIn), st.Connects, st.Failed, st.Dropped)
			if st.Failed > 0 || st.Dropped > 0 {
				return 1
			}
			return 0
		case <-t.C:
			printSimStats(run.Stats())
		}
	}
}

func printSimStats(st sim.Stats) {
	line := fmt.Sprintf("%6.0fs  connected %d/%d  sent %.0f/s  received %.0f/s", st.Elapsed, st.Connected, st.Devices, st.SentPerSec, st.ReceivedPerSec)
	if st.Samples > 0 {
		line += fmt.Sprintf("  relay p50 %.0f ms  p95 %.0f ms  max %.0f ms", st.LatencyP50, st.LatencyP95, st.LatencyMax)
	}
	if st.Failed > 0 || st.Dropped > 0 {
		line += fmt.Sprintf("  failed %d  dropped %d", st.Failed, st.Dropped)
	}
	if st.LastError != "" {
		line += "  (last error: " + st.LastError + ")"
	}
	fmt.Println(line)
}

func simBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
}

// simEnroll gets a client certificate for user from the enrollment port,
// as ATAK does, and returns the TLS settings for the streaming port.
func simEnroll(host string, port int, user, password string, insecure bool) (*tls.Config, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: user}}, key)
	if err != nil {
		return nil, err
	}
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}}
	u := "https://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/Marti/api/tls/signClient/v2?" +
		url.Values{"clientUid": {"edgetak-sim"}, "version": {version}}.Encode()
	req, err := http.NewRequest("POST", u, bytes.NewReader(csr))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(user, password)
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		var unknown x509.UnknownAuthorityError
		if errors.As(err, &unknown) || strings.Contains(err.Error(), "certificate") {
			return nil, fmt.Errorf("enrollment: %v (add -insecure for a server whose enrollment port uses its own certificate)", err)
		}
		return nil, fmt.Errorf("enrollment: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, errors.New("enrollment: wrong username or password")
		}
		return nil, fmt.Errorf("enrollment: the server answered %s", resp.Status)
	}
	var out map[string]string
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.New("enrollment: unexpected answer from the server")
	}
	decode := func(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), "")) }
	certDER, err := decode(out["signedCert"])
	if err != nil {
		return nil, errors.New("enrollment: no certificate in the answer")
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if caDER, err := decode(out["ca0"]); err == nil {
		if ca, err := x509.ParseCertificate(caDER); err == nil {
			roots.AddCert(ca)
		}
	}
	fmt.Printf("Enrolled as %s (certificate valid until %s).\n", user, leaf.NotAfter.Format("2006-01-02"))
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{certDER}, PrivateKey: key, Leaf: leaf}},
		RootCAs: roots, ServerName: host, InsecureSkipVerify: insecure}, nil
}
