package app

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/config"
	"github.com/brandongotak/EdgeTAK/internal/cot"
)

func TestVideo(t *testing.T) {
	mtxConf := filepath.Join(t.TempDir(), "mediamtx.yml")
	a := startAppWith(t, func(c *config.Config) {
		c.Video = config.Video{Enabled: true, MediaMTXConfig: mtxConf, AuthAddr: "127.0.0.1:0", APIAddr: "127.0.0.1:1", HLSAddr: "127.0.0.1:1", RTSPPort: 8554}
	})
	call, _ := adminJSON(t, a)
	for _, u := range []struct{ name, group string }{{"alice", "Cyan"}, {"bob", "Red"}} {
		if _, err := a.Store.CreateUser(u.name, "password-"+u.name, []string{u.group}, false); err != nil {
			t.Fatal(err)
		}
	}

	// EdgeTAK writes MediaMTX's config pointing auth back at itself.
	conf, err := os.ReadFile(mtxConf)
	if err != nil || !strings.Contains(string(conf), "authMethod: http") || !strings.Contains(string(conf), "/mediamtx/auth") {
		t.Fatalf("mediamtx.yml: %v\n%s", err, conf)
	}

	// A positioned external camera and a server stream, both for Cyan.
	code, gate := call("POST", "/admin/api/video/feeds", map[string]any{"alias": "Gate", "url": "rtsp://10.0.0.5:554/live", "groups": []string{"Cyan"}, "lat": 38.9, "lon": -77.0})
	if code != 200 {
		t.Fatalf("save feed = %d %v", code, gate)
	}
	if code, _ := call("POST", "/admin/api/video/feeds", map[string]any{"alias": "Drone", "path": "drone1", "groups": []string{"Cyan"}}); code != 200 {
		t.Fatalf("save hosted feed = %d", code)
	}
	if code, _ := call("POST", "/admin/api/video/feeds", map[string]any{"alias": "Bad", "url": "file:///etc/passwd"}); code != 400 {
		t.Fatalf("file URL accepted: %d", code)
	}

	// Devices in Cyan get the camera marker with a playable <__video> link.
	aliceCert := enroll(t, a, "alice", "password-alice")
	alice := mustDial(t, a, &aliceCert)
	marker := alice.expect(func(e *cot.Event) bool { return e.UID == "edgetak-video-"+gate["uid"].(string) }, "camera marker")
	if marker.Type != "a-f-G-E-S" || !strings.Contains(marker.Detail.XML, `url="rtsp://10.0.0.5:554/live"`) || !strings.Contains(marker.Detail.XML, "<ConnectionEntry") {
		t.Fatalf("marker = %s %s", marker.Type, marker.Detail.XML)
	}
	bobCert := enroll(t, a, "bob", "password-bob")
	bob := mustDial(t, a, &bobCert)
	bob.expectNone(func(e *cot.Event) bool { return strings.HasPrefix(e.UID, "edgetak-video-") }, "camera marker for another group")

	// /Marti/vcm: alice sees both, with a read token for the server stream.
	hc := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	vcm := func(user string) string {
		req, _ := http.NewRequest("GET", "https://"+a.Addrs["https"]+"/Marti/vcm", nil)
		req.SetBasicAuth(user, "password-"+user)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	ax := vcm("alice")
	if !strings.Contains(ax, "<alias>Gate</alias>") || !strings.Contains(ax, "<address>10.0.0.5</address>") || !strings.Contains(ax, "<path>/live</path>") {
		t.Fatalf("alice vcm:\n%s", ax)
	}
	tok := regexp.MustCompile(`<address>alice:(rt-[A-Za-z0-9_-]+)@`).FindStringSubmatch(ax)
	if tok == nil || !strings.Contains(ax, "<path>/drone1</path>") || strings.Contains(ax, "password-alice") {
		t.Fatalf("hosted feed in alice vcm:\n%s", ax)
	}
	if bx := vcm("bob"); strings.Contains(bx, "Gate") || strings.Contains(bx, "drone1") {
		t.Fatalf("bob sees Cyan feeds:\n%s", bx)
	}

	// ATAK shares a video alias; it lands in the sharer's groups.
	req, _ := http.NewRequest("POST", "https://"+a.Addrs["https"]+"/Marti/vcm", strings.NewReader(
		`<videoConnections><feed><uid>atak-1</uid><alias>Body cam</alias><protocol>rtsp</protocol><address>192.168.1.20</address><port>8554</port><path>/cam</path></feed></videoConnections>`))
	req.SetBasicAuth("bob", "password-bob")
	if resp, err := hc.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("POST vcm: %v %v", resp, err)
	}
	if f, ok := a.Store.VideoFeed("atak-1"); !ok || f.URL != "rtsp://192.168.1.20:8554/cam" || f.Groups[0] != "Red" {
		t.Fatalf("shared feed = %+v", f)
	}

	// MediaMTX's auth callback.
	auth := func(body map[string]string) int {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := hc.Post("http://"+a.Addrs["video_auth"]+"/mediamtx/auth", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	cases := []struct {
		name string
		req  map[string]string
		want int
	}{
		{"publish with password", map[string]string{"user": "alice", "password": "password-alice", "action": "publish", "path": "drone1", "protocol": "rtsp"}, 200},
		{"publish with wrong password", map[string]string{"user": "alice", "password": "nope", "action": "publish", "path": "drone1"}, 401},
		{"publish to another group's stream", map[string]string{"user": "bob", "password": "password-bob", "action": "publish", "path": "drone1"}, 401},
		{"read with token", map[string]string{"user": "alice", "password": tok[1], "action": "read", "path": "drone1"}, 200},
		{"read with password", map[string]string{"user": "alice", "password": "password-alice", "action": "read", "path": "drone1"}, 200},
		{"token is per user", map[string]string{"user": "bob", "password": tok[1], "action": "read", "path": "drone1"}, 401},
		{"other group may not read", map[string]string{"user": "bob", "password": "password-bob", "action": "read", "path": "drone1"}, 401},
		{"no credentials", map[string]string{"action": "read", "path": "drone1"}, 401},
		{"bad internal credentials", map[string]string{"user": "edgetak-internal", "password": "guess", "action": "api"}, 401},
		{"api for users", map[string]string{"user": "alice", "password": "password-alice", "action": "api"}, 401},
	}
	for _, c := range cases {
		if got := auth(c.req); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}

	// Deleting a positioned feed removes its marker from devices.
	if code, _ := call("DELETE", "/admin/api/video/feeds/"+gate["uid"].(string), nil); code != 204 {
		t.Fatalf("delete = %d", code)
	}
	alice.expect(func(e *cot.Event) bool {
		return e.Type == cot.TypeDelete && strings.Contains(e.Detail.XML, "edgetak-video-"+gate["uid"].(string))
	}, "marker removal")
}
