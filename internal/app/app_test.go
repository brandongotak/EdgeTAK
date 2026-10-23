package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"

	"github.com/brandongotak/EdgeTAK/internal/config"
	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/pki"
)

func startApp(t *testing.T) *App { return startAppWith(t, nil) }

// startAppWith starts a server on the in-memory network after applying edit
// to the test configuration.
func startAppWith(t *testing.T, edit func(*config.Config)) *App {
	t.Helper()
	cfg := config.Default()
	for _, p := range []*string{&cfg.TCPAddr, &cfg.TLSAddr, &cfg.HTTPSAddr, &cfg.EnrollAddr, &cfg.HTTPAddr} {
		*p = "127.0.0.1:0"
	}
	cfg.PublicHost = "127.0.0.1"
	if edit != nil {
		edit(&cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	a, err := New(cfg, t.TempDir(), "test", log)
	if err != nil {
		t.Fatal(err)
	}
	a.Listen = mem.Listen
	a.Dial = mem.Dial
	ctx, cancel := context.WithCancel(context.Background())
	wait, err := a.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); wait() })
	return a
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// enroll performs the ATAK enrollment flow and returns a client certificate.
func enroll(t *testing.T, a *App, user, pw string) tls.Certificate {
	t.Helper()
	hc := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	base := "https://" + a.Addrs["enroll"]

	req, _ := http.NewRequest("GET", base+"/Marti/api/tls/config", nil)
	req.SetBasicAuth(user, pw)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte(`nameEntry name="O"`)) {
		t.Fatalf("tls/config: %d %s", resp.StatusCode, body)
	}

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: user, Organization: []string{"EdgeTAK"}}}, key)
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	req, _ = http.NewRequest("POST", base+"/Marti/api/tls/signClient/v2?clientUid=ANDROID-"+user+"&version=5.5", bytes.NewReader(csrPEM))
	req.SetBasicAuth(user, pw)
	req.Header.Set("Accept", "application/json")
	resp, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("signClient: %d %s", resp.StatusCode, b)
	}
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	der, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out["signedCert"], "\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != user {
		t.Errorf("CN = %q", cert.Subject.CommonName)
	}
	caDER, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(out["ca0"], "\n", ""))
	if !bytes.Equal(caDER, a.CA.Cert.Raw) {
		t.Error("ca0 is not the server CA")
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}
}

type streamClient struct {
	t      *testing.T
	conn   net.Conn
	events chan *cot.Event
	format cot.Format
}

func dialStream(t *testing.T, a *App, cert *tls.Certificate) (*streamClient, error) {
	t.Helper()
	var conn net.Conn
	var err error
	if cert != nil {
		conn, err = tlsDial(a.Addrs["tls"], &tls.Config{RootCAs: a.CA.Pool(), Certificates: []tls.Certificate{*cert}, ServerName: "127.0.0.1"})
	} else {
		conn, err = mem.Dial(a.Addrs["tcp"])
	}
	if err != nil {
		return nil, err
	}
	c := &streamClient{t: t, conn: conn, events: make(chan *cot.Event, 64)}
	t.Cleanup(func() { conn.Close() })
	go func() {
		defer close(c.events)
		r := cot.NewReader(conn)
		for {
			ev, _, err := r.Next()
			if err != nil {
				return
			}
			c.events <- ev
		}
	}()
	return c, nil
}

func mustDial(t *testing.T, a *App, cert *tls.Certificate) *streamClient {
	t.Helper()
	c, err := dialStream(t, a, cert)
	if err != nil {
		t.Fatal(err)
	}
	c.expect(func(e *cot.Event) bool { return e.Type == cot.TypeProtoAnnounce }, "protocol announcement")
	return c
}

// negotiate upgrades the stream to TAK protobuf like ATAK does.
func (c *streamClient) negotiate() {
	req := cot.NewEvent(cot.TypeProtoRequest, "req-"+fmt.Sprint(time.Now().UnixNano()), "m-g", time.Minute)
	req.Detail.XML = `<TakControl><TakRequest version="1"/></TakControl>`
	c.send(req)
	c.expect(func(e *cot.Event) bool {
		return e.Type == cot.TypeProtoResponse && strings.Contains(e.Detail.XML, `status="true"`)
	}, "protocol response")
	c.format = cot.FormatProto
}

func (c *streamClient) send(ev *cot.Event) {
	c.t.Helper()
	if err := cot.WriteFrame(c.conn, ev, c.format); err != nil {
		c.t.Fatal(err)
	}
}

func (c *streamClient) expect(pred func(*cot.Event) bool, what string) *cot.Event {
	c.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-c.events:
			if !ok {
				c.t.Fatalf("connection closed waiting for %s", what)
			}
			if pred(ev) {
				return ev
			}
		case <-deadline:
			c.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (c *streamClient) expectNone(pred func(*cot.Event) bool, what string) {
	c.t.Helper()
	deadline := time.After(400 * time.Millisecond)
	for {
		select {
		case ev, ok := <-c.events:
			if !ok {
				return
			}
			if pred(ev) {
				c.t.Fatalf("unexpectedly received %s", what)
			}
		case <-deadline:
			return
		}
	}
}

func sa(uid, callsign, team string) *cot.Event {
	e := cot.NewEvent("a-f-G-U-C", uid, "m-g", 2*time.Minute)
	e.Lat, e.Lon, e.Hae = 38.8977, -77.0365, 10
	e.Detail.Contact = &cot.Contact{Endpoint: "*:-1:stcp", Callsign: callsign}
	e.Detail.Group = &cot.Group{Name: team, Role: "Team Member"}
	e.Detail.Takv = &cot.Takv{Device: "TEST", Platform: "ATAK-CIV", OS: "34", Version: "5.5"}
	e.Detail.Status = &cot.Status{Battery: 77}
	e.Detail.XML = `<uid Droid="` + callsign + `"/>`
	return e
}

func chat(from, to string) *cot.Event {
	e := cot.NewEvent("b-t-f", "GeoChat."+from+"."+to+".1", "h-g-i-g-o", time.Hour)
	e.Detail.XML = `<__chat chatroom="` + to + `" senderCallsign="` + from + `"/><marti><dest callsign="` + to + `"/></marti><remarks>hello</remarks>`
	return e
}

func isFrom(uid string) func(*cot.Event) bool {
	return func(e *cot.Event) bool { return e.UID == uid }
}

func TestEndToEnd(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct{ name, group string }{{"alice", "Cyan"}, {"bob", "Cyan"}, {"carol", "Cyan"}, {"eve", "Red"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}
	aliceCert := enroll(t, a, "alice", "password-alice")
	bobCert := enroll(t, a, "bob", "password-bob")
	carolCert := enroll(t, a, "carol", "password-carol")
	eveCert := enroll(t, a, "eve", "password-eve")

	// Bad password must not enroll.
	hc := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	req, _ := http.NewRequest("GET", "https://"+a.Addrs["enroll"]+"/Marti/api/tls/config", nil)
	req.SetBasicAuth("alice", "wrong")
	if resp, err := hc.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("bad password: %v %v", resp.StatusCode, err)
	}

	// A TLS client without a certificate is refused.
	if c, err := tlsDial(a.Addrs["tls"], &tls.Config{RootCAs: a.CA.Pool(), ServerName: "127.0.0.1"}); err == nil {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("TLS stream accepted a client without a certificate")
		}
		c.Close()
	}

	alice := mustDial(t, a, &aliceCert)
	alice.negotiate() // alice speaks protobuf, everyone else XML
	bob := mustDial(t, a, &bobCert)
	carol := mustDial(t, a, &carolCert)
	eve := mustDial(t, a, &eveCert)
	anon := mustDial(t, a, nil)

	t.Run("ping", func(t *testing.T) {
		bob.send(cot.NewEvent(cot.TypePing, "bob-ping", "m-g", 10*time.Second))
		bob.expect(func(e *cot.Event) bool { return e.Type == cot.TypePong }, "pong")
	})

	t.Run("group SA routing across encodings", func(t *testing.T) {
		alice.send(sa("ANDROID-alice", "ALICE", "Cyan"))
		got := bob.expect(isFrom("ANDROID-alice"), "alice SA at bob")
		if got.Callsign() != "ALICE" || got.Detail.Status == nil || got.Detail.Status.Battery != 77 || !strings.Contains(got.Detail.XML, `Droid="ALICE"`) {
			t.Errorf("SA lost data through protobuf->XML: %s", got.XML())
		}
		carol.expect(isFrom("ANDROID-alice"), "alice SA at carol")
		eve.expectNone(isFrom("ANDROID-alice"), "alice SA at eve (other group)")
		anon.expectNone(isFrom("ANDROID-alice"), "alice SA at anonymous client")

		bob.send(sa("ANDROID-bob", "BOB", "Cyan"))
		got = alice.expect(isFrom("ANDROID-bob"), "bob SA at alice (protobuf)")
		if got.Callsign() != "BOB" || got.Lat != 38.8977 {
			t.Errorf("XML->protobuf lost data: %+v", got)
		}
		carol.send(sa("ANDROID-carol", "CAROL", "Cyan"))
		alice.expect(isFrom("ANDROID-carol"), "carol SA at alice")
	})

	t.Run("directed chat", func(t *testing.T) {
		bob.send(chat("BOB", "ALICE"))
		alice.expect(func(e *cot.Event) bool { return e.Type == "b-t-f" }, "chat at alice")
		carol.expectNone(func(e *cot.Event) bool { return e.Type == "b-t-f" }, "chat at carol (not addressed)")
	})

	t.Run("latest SA replay", func(t *testing.T) {
		late := mustDial(t, a, &carolCert)
		late.expectAll("replayed alice and bob SA", isFrom("ANDROID-alice"), isFrom("ANDROID-bob"))
		late.conn.Close()
	})

	t.Run("data packages", func(t *testing.T) {
		https := func(cert tls.Certificate) *http.Client {
			return &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool(), Certificates: []tls.Certificate{cert}})}
		}
		base := "https://" + a.Addrs["https"]
		content := []byte("PK fake mission package " + time.Now().String())
		sum := sha256.Sum256(content)
		hash := hex.EncodeToString(sum[:])

		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("assetfile", "ops.zip")
		fw.Write(content)
		mw.Close()
		resp, err := https(aliceCert).Post(base+"/Marti/sync/missionupload?hash="+hash+"&filename=ops.zip&creatorUid=ANDROID-alice", mw.FormDataContentType(), &body)
		if err != nil {
			t.Fatal(err)
		}
		url, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasSuffix(string(url), "/Marti/sync/content?hash="+hash) {
			t.Fatalf("upload: %d %s", resp.StatusCode, url)
		}

		resp, _ = https(bobCert).Get(base + "/Marti/sync/content?hash=" + hash)
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Equal(got, content) {
			t.Errorf("bob download: %d", resp.StatusCode)
		}
		resp, _ = https(eveCert).Get(base + "/Marti/sync/content?hash=" + hash)
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("eve (other group) download = %d, want 404", resp.StatusCode)
		}

		resp, _ = https(bobCert).Get(base + "/Marti/sync/search?keywords=missionpackage&tool=public")
		var sr struct {
			ResultCount int              `json:"resultCount"`
			Results     []map[string]any `json:"results"`
		}
		json.NewDecoder(resp.Body).Decode(&sr)
		resp.Body.Close()
		if sr.ResultCount != 1 || sr.Results[0]["Hash"] != hash || sr.Results[0]["Size"] != fmt.Sprint(len(content)) {
			t.Errorf("search = %+v", sr)
		}
		resp, _ = https(bobCert).Get(base + "/Marti/sync/missionquery?hash=" + hash)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("missionquery = %d", resp.StatusCode)
		}

		// The newer API forms (TAKX): /Marti/api/sync/search and /Marti/api/files/{hash}.
		apiSearch := func(cert tls.Certificate) []any {
			resp, err := https(cert).Get(base + "/Marti/api/sync/search?keyword=missionpackage")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var env map[string]any
			json.NewDecoder(resp.Body).Decode(&env)
			if resp.StatusCode != 200 || env["type"] != "Resource" {
				t.Fatalf("api search = %d %v", resp.StatusCode, env)
			}
			data, _ := env["data"].([]any)
			return data
		}
		if res := apiSearch(bobCert); len(res) != 1 || res[0].(map[string]any)["hash"] != hash || res[0].(map[string]any)["size"] != float64(len(content)) {
			t.Errorf("api search for bob = %v", res)
		}
		if res := apiSearch(eveCert); len(res) != 0 {
			t.Errorf("api search leaked across groups: %v", res)
		}
		resp, _ = https(bobCert).Get(base + "/Marti/api/files/" + hash)
		got, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Equal(got, content) {
			t.Errorf("api files download = %d", resp.StatusCode)
		}
	})

	t.Run("oauth password grant and bearer access (TAKX)", func(t *testing.T) {
		hc := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})} // no client cert
		base := "https://" + a.Addrs["https"]
		token := func(form string) (int, map[string]any) {
			resp, err := hc.Post(base+"/oauth/token?"+form, "application/x-www-form-urlencoded", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var m map[string]any
			json.NewDecoder(resp.Body).Decode(&m)
			return resp.StatusCode, m
		}
		if st, m := token("grant_type=password&username=alice&password=wrong"); st != 400 || m["error"] != "invalid_grant" {
			t.Errorf("bad password = %d %v", st, m)
		}
		st, m := token("grant_type=password&username=alice&password=password-alice")
		access, _ := m["access_token"].(string)
		if st != 200 || access == "" || m["token_type"] != "Bearer" || m["scope"] != "openid profile" || m["expires_in"] == nil {
			t.Fatalf("token = %d %v", st, m)
		}
		// Like the official server, the token is an RS256 JWT signed by the
		// server's TLS key, verifiable against the certificate the client sees.
		parts := strings.Split(access, ".")
		if len(parts) != 3 {
			t.Fatalf("not a JWT: %s", access)
		}
		hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
		payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(a.CA.ServerCert.Leaf.PublicKey.(*rsa.PublicKey), crypto.SHA256, digest[:], sig); err != nil {
			t.Errorf("token signature does not verify with the server certificate: %v (header %s)", err, hdr)
		}
		var claims map[string]any
		json.Unmarshal(payload, &claims)
		if claims["sub"] != "alice" || claims["exp"] == nil || claims["iat"] == nil || claims["jti"] == nil {
			t.Errorf("claims = %v", claims)
		}
		if scope, _ := claims["scope"].([]any); len(scope) != 2 {
			t.Errorf("scope claim = %v", claims["scope"])
		}
		get := func(path, tok string) (int, []byte) {
			req, _ := http.NewRequest("GET", base+path, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := hc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, b
		}
		st, body := get("/Marti/api/contacts/all", access)
		var contacts []map[string]any
		json.Unmarshal(body, &contacts)
		found := false
		for _, c := range contacts {
			if c["callsign"] == "BOB" && c["team"] == "Cyan" {
				found = true
			}
		}
		if st != 200 || !found {
			t.Errorf("contacts with bearer = %d %s", st, body)
		}
		// Flip a character in the middle of the signature (the last ones may only hold padding bits).
		mid := strings.LastIndex(access, ".") + 20
		flipped := byte('A')
		if access[mid] == 'A' {
			flipped = 'B'
		}
		if st, _ := get("/Marti/api/clientEndPoints", access[:mid]+string(flipped)+access[mid+1:]); st != 401 {
			t.Errorf("tampered token = %d, want 401", st)
		}
		if st, m := token("grant_type=client_credentials"); st != 400 || m["error"] != "unsupported_grant_type" {
			t.Errorf("unsupported grant = %d %v", st, m)
		}
	})

	t.Run("https never resumes sessions and sends the full chain", func(t *testing.T) {
		tr := memTransport(&tls.Config{RootCAs: a.CA.Pool(), Certificates: []tls.Certificate{aliceCert}, ClientSessionCache: tls.NewLRUClientSessionCache(8)})
		tr.DisableKeepAlives = true
		hc := &http.Client{Transport: tr}
		for i := 0; i < 2; i++ {
			resp, err := hc.Get("https://" + a.Addrs["https"] + "/Marti/api/version")
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.TLS.DidResume {
				t.Errorf("request %d resumed a TLS session; WinTAK (SChannel) cannot handle that", i)
			}
			if n := len(resp.TLS.PeerCertificates); n != 2 {
				t.Errorf("server sent %d certificates, want leaf + CA", n)
			}
		}
	})

	t.Run("revocation", func(t *testing.T) {
		if err := a.Store.RevokeCert(pki.SerialHex(eveCert.Leaf)); err != nil {
			t.Fatal(err)
		}
		c, err := dialStream(t, a, &eveCert)
		if err != nil {
			return // handshake-level rejection is fine too
		}
		for range c.events {
			// drain until the server closes the connection
		}
	})

	t.Run("admin console", func(t *testing.T) {
		pw := strings.TrimPrefix(strings.Split(readFile(t, a.DataDir+"/initial-admin-password.txt"), "\n")[1], "password: ")
		jar, _ := cookiejar.New(nil)
		hc := &http.Client{Jar: jar, Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
		base := "https://" + a.Addrs["https"]

		resp, _ := hc.Get(base + "/admin/api/clients")
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("unauthenticated clients list = %d", resp.StatusCode)
		}
		resp, _ = hc.Post(base+"/admin/api/login", "application/json", strings.NewReader(`{"username":"admin","password":"`+pw+`"}`))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("login = %d", resp.StatusCode)
		}
		resp, _ = hc.Get(base + "/admin/api/clients")
		var clients []map[string]any
		json.NewDecoder(resp.Body).Decode(&clients)
		resp.Body.Close()
		found := false
		for _, c := range clients {
			if c["callsign"] == "ALICE" && c["team"] == "Cyan" {
				found = true
				if _, ok := c["lat"].(float64); !ok {
					t.Errorf("ALICE lat missing: %v", c)
				}
				if _, ok := c["lon"].(float64); !ok {
					t.Errorf("ALICE lon missing: %v", c)
				}
				if c["format"] != "protobuf" {
					t.Errorf("ALICE format = %v, want protobuf", c["format"])
				}
			}
			if c["callsign"] == "BOB" && c["format"] != "xml" {
				t.Errorf("BOB format = %v, want xml", c["format"])
			}
		}
		if !found {
			t.Errorf("ALICE missing from admin client list: %v", clients)
		}

		// State-changing requests without the CSRF header are rejected.
		resp, _ = hc.Post(base+"/admin/api/users", "application/json", strings.NewReader(`{"username":"mallory","password":"12345678"}`))
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("create user without CSRF header = %d, want 401", resp.StatusCode)
		}
		req, _ := http.NewRequest("POST", base+"/admin/api/users", strings.NewReader(`{"username":"dave","password":"password-dave","groups":["Cyan"]}`))
		req.Header.Set("X-EdgeTAK", "1")
		resp, _ = hc.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Errorf("create user = %d", resp.StatusCode)
		}

		resp, _ = hc.Get(base + "/admin/api/connection-package?username=dave")
		zipData, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "-dave-ssl.zip") {
			t.Errorf("package file name = %q, want the username in it", cd)
		}
		zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
		if err != nil {
			t.Fatalf("connection package: %d %v", resp.StatusCode, err)
		}
		files := map[string][]byte{}
		for _, f := range zr.File {
			rc, _ := f.Open()
			files[f.Name], _ = io.ReadAll(rc)
			rc.Close()
		}
		if !bytes.Contains(files["certs/server.pref"], []byte("127.0.0.1:"+port(a.Addrs["tls"])+":ssl")) {
			t.Errorf("pref missing connect string:\n%s", files["certs/server.pref"])
		}
		if !bytes.Contains(files["certs/server.pref"], []byte(`key="prefs_enable_channels_host-127.0.0.1"`)) ||
			!bytes.Contains(files["certs/server.pref"], []byte(`key="deviceProfileEnableOnConnect" class="class java.lang.Boolean"`)) {
			t.Errorf("connection package lacks the channel preferences:\n%s", files["certs/server.pref"])
		}
		dec := xml.NewDecoder(bytes.NewReader(files["certs/server.pref"]))
		dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil } // ATAK prefs declare ASCII
		if err := dec.Decode(new(struct{ XMLName xml.Name })); err != nil {
			t.Errorf("connection package pref is not well-formed XML: %v", err)
		}
		var ts []byte
		for name, data := range files {
			if strings.HasPrefix(name, "certs/truststore") {
				ts = data
			}
		}
		certs, err := pkcs12.DecodeTrustStore(ts, a.Cfg.TruststorePassword)
		if err != nil || len(certs) != 1 || !certs[0].Equal(a.CA.Cert) {
			t.Errorf("truststore: %v %d", err, len(certs))
		}
		// For a user it carries their certificate instead of enrolling.
		if _, leaf, _, err := pkcs12.DecodeChain(files["certs/dave.p12"], a.Cfg.TruststorePassword); err != nil || leaf.Subject.CommonName != "dave" ||
			bytes.Contains(files["certs/server.pref"], []byte("enrollForCertificateWithTrust")) {
			t.Errorf("user package: client certificate %v; pref:\n%s", err, files["certs/server.pref"])
		}

		// iTAK: flat zip with config.pref, a client .p12 and the truststore.
		resp, _ = hc.Get(base + "/admin/api/connection-package?mode=itak&username=nobody")
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("itak package for unknown user = %d", resp.StatusCode)
		}
		resp, _ = hc.Get(base + "/admin/api/connection-package?mode=itak&username=dave")
		zipData, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		zr, err = zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
		if err != nil {
			t.Fatalf("itak package: %d %v", resp.StatusCode, err)
		}
		files = map[string][]byte{}
		for _, f := range zr.File {
			rc, _ := f.Open()
			files[f.Name], _ = io.ReadAll(rc)
			rc.Close()
		}
		if len(files) != 3 || files["config.pref"] == nil || files["dave.p12"] == nil || files["truststore-root.p12"] == nil {
			t.Fatalf("itak package entries = %v", maps.Keys(files))
		}
		for _, want := range []string{"cert/dave.p12", "cert/truststore-root.p12", "127.0.0.1:" + port(a.Addrs["tls"]) + ":ssl"} {
			if !bytes.Contains(files["config.pref"], []byte(want)) {
				t.Errorf("itak config.pref missing %q:\n%s", want, files["config.pref"])
			}
		}
		key, leaf, _, err := pkcs12.DecodeChain(files["dave.p12"], a.Cfg.TruststorePassword)
		if err != nil || leaf.Subject.CommonName != "dave" {
			t.Fatalf("itak client p12: %v", err)
		}
		// The bundled certificate is accepted by the TLS stream.
		itakCert := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
		mustDial(t, a, &itakCert).conn.Close()
	})

	t.Run("plaintext admin is loopback only and marti allows anonymous", func(t *testing.T) {
		plain := &http.Client{Transport: memTransport(nil)}
		resp, err := plain.Get("http://" + a.Addrs["http"] + "/Marti/api/version")
		if err != nil {
			t.Fatal(err)
		}
		v, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.HasPrefix(string(v), "EdgeTAK") {
			t.Errorf("version = %q", v)
		}
		resp, _ = plain.Get("http://" + a.Addrs["http"] + "/admin/")
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("admin from loopback over http = %d", resp.StatusCode)
		}
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
