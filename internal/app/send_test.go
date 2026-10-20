package app

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// adminJSON logs in to the admin console and returns a helper that sends a
// request and decodes the JSON reply.
func adminJSON(t *testing.T, a *App) (func(method, path string, body any) (int, map[string]any), *http.Client) {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(a.DataDir, "initial-admin-password.txt"))
	pw := strings.TrimPrefix(strings.Split(string(raw), "\n")[1], "password: ")
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	base := "https://" + a.Addrs["https"]
	resp, err := hc.Post(base+"/admin/api/login", "application/json", strings.NewReader(`{"username":"admin","password":"`+pw+`"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("admin login: %v", err)
	}
	resp.Body.Close()
	return func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(b))
		req.Header.Set("X-EdgeTAK", "1")
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}, hc
}

func TestSendToDevices(t *testing.T) {
	a := startApp(t)
	for _, u := range []struct{ name, group string }{{"alice", "Cyan"}, {"bob", "Cyan"}, {"eve", "Red"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}
	certs := map[string]tls.Certificate{}
	streams := map[string]*streamClient{}
	for _, u := range []string{"alice", "bob", "eve"} {
		c := enroll(t, a, u, "password-"+u)
		certs[u] = c
		streams[u] = mustDial(t, a, &c)
		streams[u].send(sa("ANDROID-"+u, strings.ToUpper(u), "Cyan"))
	}
	time.Sleep(200 * time.Millisecond)
	ids := map[string]uint64{}
	for _, c := range a.Router.Clients() {
		ids[c.Username] = c.ID
	}
	send, adminHTTP := adminJSON(t, a)
	isChat := func(e *cot.Event) bool { return e.Type == "b-t-f" }

	// To a group: GeoChat in "All Chat Rooms" from the server.
	if st, m := send("POST", "/admin/api/send/message", map[string]any{"from": "Ops Center", "text": "Rally at <grid 12>", "to": map[string]any{"groups": []string{"Cyan"}}}); st != 200 || m["delivered"] != float64(2) {
		t.Fatalf("message to Cyan = %d %v", st, m)
	}
	msg := streams["alice"].expect(isChat, "group message at alice")
	streams["bob"].expect(isChat, "group message at bob")
	streams["eve"].expectNone(isChat, "group message at eve (Red)")
	for _, want := range []string{`chatroom="All Chat Rooms"`, `senderCallsign="Ops Center"`, `<chatgrp uid0="EdgeTAK-`, `Rally at &lt;grid 12&gt;</remarks>`} {
		if !strings.Contains(msg.Detail.XML, want) {
			t.Errorf("chat detail missing %s:\n%s", want, msg.Detail.XML)
		}
	}
	if parts := strings.Split(msg.UID, "."); len(parts) != 4 || parts[0] != "GeoChat" || parts[2] != "All Chat Rooms" {
		t.Errorf("chat uid = %q (ATAK parses GeoChat.<sender>.<room>.<id>)", msg.UID)
	}

	// To everyone, and to one device.
	send("POST", "/admin/api/send/message", map[string]any{"text": "all hands", "to": map[string]any{"all": true}})
	for _, u := range []string{"alice", "bob", "eve"} {
		streams[u].expect(isChat, "broadcast at "+u)
	}
	send("POST", "/admin/api/send/message", map[string]any{"text": "just bob", "to": map[string]any{"clients": []uint64{ids["bob"]}}})
	streams["bob"].expect(isChat, "direct message at bob")
	streams["alice"].expectNone(isChat, "direct message at alice")
	if st, _ := send("POST", "/admin/api/send/message", map[string]any{"text": "nobody", "to": map[string]any{"groups": []string{"Ghost"}}}); st != 400 {
		t.Errorf("message with no recipients = %d", st)
	}

	// A data package uploaded by the admin, pushed to eve (Red).
	content := []byte("PK route package " + time.Now().String())
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("assetfile", "routes.zip")
	fw.Write(content)
	mw.Close()
	req, _ := http.NewRequest("POST", "https://"+a.Addrs["https"]+"/Marti/sync/missionupload?filename=routes.zip&creatorUid=admin-console", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-EdgeTAK", "1")
	if resp, err := adminHTTP.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("admin upload: %v", err)
	}
	if st, m := send("POST", "/admin/api/send/package", map[string]any{"hash": hash, "to": map[string]any{"groups": []string{"Red"}}}); st != 200 || m["delivered"] != float64(1) {
		t.Fatalf("package to Red = %d %v", st, m)
	}
	offer := streams["eve"].expect(func(e *cot.Event) bool { return e.Type == "b-f-t-r" }, "file transfer request at eve")
	for _, want := range []string{`filename="routes.zip"`, `sha256="` + hash + `"`, `sizeInBytes="`, `senderUid="EdgeTAK-`, `name="routes"`} {
		if !strings.Contains(offer.Detail.XML, want) {
			t.Errorf("fileshare missing %s:\n%s", want, offer.Detail.XML)
		}
	}
	// eve can download from the URL in the offer with her own certificate.
	i := strings.Index(offer.Detail.XML, `senderUrl="`)
	raw := offer.Detail.XML[i+len(`senderUrl="`):]
	u, err := url.Parse(strings.ReplaceAll(raw[:strings.Index(raw, `"`)], "&amp;", "&"))
	if err != nil || u.Scheme != "https" || u.Path != "/Marti/sync/content" {
		t.Fatalf("senderUrl = %v %v", u, err)
	}
	eve := newAPI(t, a, certs["eve"])
	if st, _, got := eve.do("GET", u.Path+"?"+u.RawQuery, nil); st != 200 || !bytes.Equal(got, content) {
		t.Errorf("eve download from offer = %d", st)
	}

	// History lists what was sent.
	resp, _ := adminHTTP.Get("https://" + a.Addrs["https"] + "/admin/api/send/history")
	var hist []map[string]any
	json.NewDecoder(resp.Body).Decode(&hist)
	resp.Body.Close()
	if len(hist) != 4 || hist[0]["kind"] != "package" || hist[0]["summary"] != "routes.zip" {
		t.Errorf("history = %v", hist)
	}
}
