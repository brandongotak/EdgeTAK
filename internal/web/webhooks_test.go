package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

func TestWebhookRateLimit(t *testing.T) {
	var st webhookState
	now := time.Now()
	for i := 0; i < webhookPerMinute; i++ {
		if ok, _ := st.allow("a", now); !ok {
			t.Fatalf("message %d held back", i+1)
		}
	}
	for i := 0; i < 3; i++ {
		if ok, _ := st.allow("a", now); ok {
			t.Fatal("over the limit but sent")
		}
	}
	if ok, _ := st.allow("b", now); !ok {
		t.Error("another webhook is limited too")
	}
	// A few seconds later there's room again, and the held-back count is reported.
	if ok, held := st.allow("a", now.Add(4*time.Second)); !ok || held != 3 {
		t.Errorf("after a pause: ok %v, held %d", ok, held)
	}
}

func TestWebhookFormats(t *testing.T) {
	s := &Server{}
	a := notice{Kind: "geofence", Title: "Left Base", Callsign: "ALPHA <1>", Username: "alpha", Lat: 39.5, Lon: -106, Time: time.Now()}
	_, body, _, err := s.webhookRequest(store.Webhook{Format: "slack", URL: "https://hooks.slack.com/x"}, a)
	var slack struct{ Text string }
	if err != nil || json.Unmarshal(body, &slack) != nil || !strings.Contains(slack.Text, "*Left Base: ALPHA &lt;1&gt;*") ||
		!strings.Contains(slack.Text, "&query=39.500000,-106.000000|39.50000, -106.00000>") {
		t.Errorf("slack = %s %v", body, err)
	}
	_, body, _, err = s.webhookRequest(store.Webhook{Format: "teams", URL: "https://example.org/x"}, a)
	var card map[string]any
	if err != nil || json.Unmarshal(body, &card) != nil || card["type"] != "message" || !strings.Contains(string(body), `"Open map"`) || !strings.Contains(string(body), `"Warning"`) {
		t.Errorf("teams = %s %v", body, err)
	}
	target, body, head, err := s.webhookRequest(store.Webhook{Format: "ntfy", URL: "https://me:pw@ntfy.example.org/ops"}, a)
	if err != nil || target != "https://ntfy.example.org/" || !strings.HasPrefix(head["Authorization"], "Basic ") || !strings.Contains(string(body), `"topic":"ops"`) {
		t.Errorf("ntfy = %s %s %v %v", target, body, head, err)
	}
}

func TestIssueKey(t *testing.T) {
	if a, b := issueKey("Disk 86% full"), issueKey("Disk 96% full: uploads, history and recordings will start failing"); a != b || a != "Disk full" {
		t.Errorf("disk keys %q, %q", a, b)
	}
	if issueKey("Federation with HQ down: not connected since Oct 2 10:00") == issueKey("Federation with Lab down: not connected since Oct 2 10:00") {
		t.Error("two links share a key")
	}
}
