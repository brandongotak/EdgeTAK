package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// TestServerHealthAlerts covers server health alerts: a federation link
// down for over 5 minutes is an issue on the Health tab and alerts the
// "server" webhooks once, again a day later if it lasts, and when resolved.
func TestServerHealthAlerts(t *testing.T) {
	a := startApp(t)
	call, _ := adminJSON(t, a)
	var mu sync.Mutex
	var titles []string
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		if title, _ := m["title"].(string); strings.Contains(title, "Federation") {
			mu.Lock()
			titles = append(titles, title)
			mu.Unlock()
		}
	}))
	defer recv.Close()
	a.Web.WebhookClient = recv.Client()
	call("POST", "/admin/api/webhooks", map[string]any{"name": "Ops", "url": recv.URL + "/x", "format": "json", "events": []string{"server"}})
	told := func(n int) []string {
		t.Helper()
		waitFor(t, "webhook messages", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(titles) >= n
		})
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), titles...)
	}

	if err := a.Store.PutFederate(store.Federate{ID: "f1", Name: "Partner HQ", Enabled: true, Direction: "outbound", Address: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a.Web.CheckHealthAlerts(now)                      // first seen down
	a.Web.CheckHealthAlerts(now.Add(2 * time.Minute)) // not yet 5 minutes
	mu.Lock()
	early := len(titles)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("alerted too soon: %v", titles)
	}
	// The issue shows on the Health tab once it counts.
	a.Web.CheckHealthAlerts(time.Now().Add(6 * time.Minute))
	if got := told(1); got[0] != "Federation with Partner HQ down" {
		t.Fatalf("alert = %v", got)
	}
	a.Web.CheckHealthAlerts(time.Now().Add(7 * time.Minute)) // still down: no repeat
	a.Web.CheckHealthAlerts(time.Now().Add(25 * time.Hour))  // a day later: a reminder
	if got := told(2); len(got) != 2 || !strings.HasPrefix(got[1], "Still: Federation with Partner HQ down") {
		t.Fatalf("reminder = %v", got)
	}
	// Turned off: resolved.
	f := a.Store.FederationSettings().Federates[0]
	f.Enabled = false
	a.Store.PutFederate(f)
	a.Web.CheckHealthAlerts(time.Now().Add(26 * time.Hour))
	if got := told(3); len(got) != 3 || got[2] != "Resolved: Federation with Partner HQ down" {
		t.Fatalf("resolved = %v", got)
	}
}
