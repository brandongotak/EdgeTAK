package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestWebhooks covers alerts sent to webhooks: an emergency reaches the
// Discord and ntfy webhooks as their formats, a JSON webhook limited to
// another group doesn't get it, the device cancelling sends "ended", the
// test button reports failures, and URLs and secrets aren't shown.
func TestWebhooks(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("alice", "password-alice", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, _ := adminJSON(t, a)

	type hit struct {
		path string
		body map[string]any
		head http.Header
		raw  []byte
	}
	var mu sync.Mutex
	var hits []hit
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		mu.Lock()
		hits = append(hits, hit{r.URL.Path, m, r.Header.Clone(), b})
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/broken") {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer recv.Close()
	a.Web.WebhookClient = recv.Client()
	got := func(path string) []hit {
		mu.Lock()
		defer mu.Unlock()
		var out []hit
		for _, h := range hits {
			if h.path == path {
				out = append(out, h)
			}
		}
		return out
	}

	for _, bad := range []map[string]any{
		{"name": "x", "url": "ftp://x", "format": "discord", "events": []string{"emergency"}},
		{"name": "x", "url": recv.URL + "/a", "format": "carrier-pigeon", "events": []string{"emergency"}},
		{"name": "x", "url": recv.URL + "/a", "format": "discord", "events": []string{}},
		{"name": "x", "url": recv.URL, "format": "ntfy", "events": []string{"emergency"}},
	} {
		if code, _ := call("POST", "/admin/api/webhooks", bad); code != 400 {
			t.Errorf("%v accepted (%d)", bad, code)
		}
	}
	add := func(h map[string]any) {
		resp, v := call("POST", "/admin/api/webhooks", h)
		if resp != 200 {
			t.Fatalf("add %v = %d %v", h["name"], resp, v)
		}
	}
	add(map[string]any{"name": "Discord", "url": recv.URL + "/discord/123/token", "format": "discord", "events": []string{"emergency", "ended"}})
	add(map[string]any{"name": "Phones", "url": recv.URL + "/team-alerts", "format": "ntfy", "events": []string{"emergency"}})
	add(map[string]any{"name": "Red ops", "url": recv.URL + "/json", "format": "json", "events": []string{"emergency"}, "groups": []string{"Red"}, "secret": "s3cret"})
	add(map[string]any{"name": "Broken", "url": recv.URL + "/broken", "format": "slack", "events": []string{"signup"}})

	var list []map[string]any
	_, hc := adminJSON(t, a)
	r2, err := hc.Get("https://" + a.Addrs["https"] + "/admin/api/webhooks")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	json.Unmarshal(raw, &list)
	if len(list) != 4 || strings.Contains(string(raw), "token") || strings.Contains(string(raw), "s3cret") || list[2]["has_secret"] != true {
		t.Fatalf("list shows secrets or is wrong: %s", raw)
	}

	// An emergency from a Cyan device.
	cert := enroll(t, a, "alice", "password-alice")
	dev := mustDial(t, a, &cert)
	dev.send(sa("ANDROID-alice", "ALICE", "Cyan"))
	dev.send(atakAlert("ANDROID-alice", "ALICE", "b-a-o-tbl", "911 Alert", false))
	waitFor(t, "Discord and ntfy alerted", func() bool { return len(got("/discord/123/token")) == 1 && len(got("/")) == 1 })
	d := got("/discord/123/token")[0].body
	embed := d["embeds"].([]any)[0].(map[string]any)
	if !strings.Contains(embed["title"].(string), "911 Alert: ALICE") || embed["color"] != float64(0xD32F2F) {
		t.Errorf("discord = %v", d)
	}
	n := got("/")[0].body
	if n["topic"] != "team-alerts" || n["priority"] != float64(5) || !strings.Contains(n["title"].(string), "ALICE") {
		t.Errorf("ntfy = %v", n)
	}
	if len(got("/json")) != 0 {
		t.Error("the Red-only webhook got a Cyan device's alert")
	}

	// Cancelling sends "ended" to the webhook that wants it.
	dev.send(atakAlert("ANDROID-alice", "ALICE", "b-a-o-tbl", "911 Alert", true))
	waitFor(t, "ended at Discord", func() bool { return len(got("/discord/123/token")) == 2 })
	if e := got("/discord/123/token")[1].body["embeds"].([]any)[0].(map[string]any); !strings.Contains(e["title"].(string), "cancelled") {
		t.Errorf("ended = %v", e)
	}

	// The test button: a JSON webhook signs its body; a failing one reports why.
	id := list[2]["id"].(string)
	if code, v := call("POST", "/admin/api/webhooks/"+id+"/test", nil); code != 200 {
		t.Fatalf("test = %d %v", code, v)
	}
	j := got("/json")[0]
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(j.raw)
	if j.head.Get("X-EdgeTAK-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) || j.body["event"] != "test" {
		t.Errorf("signed test = %v %v", j.head, j.body)
	}
	if code, v := call("POST", "/admin/api/webhooks/"+list[3]["id"].(string)+"/test", nil); code != 502 || !strings.Contains(v["error"].(string), "403") {
		t.Errorf("broken test = %d %v", code, v)
	}
}
