package app

import (
	"io"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestHistoryExport covers downloading trails as KML or GPX for a time
// range and chosen devices, and saving them as an overlay that isn't sent
// until an admin chooses who gets it.
func TestHistoryExport(t *testing.T) {
	a := startApp(t)
	call, hc := adminJSON(t, a)
	a.History.Configure(true, 24*time.Hour)
	start := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	for i := 0; i < 5; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		a.History.Record("ANDROID-alpha", "ALPHA", "Cyan", at, 39.5+float64(i)*0.001, -106.0, 1, 90)
		a.History.Record("ANDROID-red1", "RED1", "Red", at, 39.6, -106.1+float64(i)*0.001, 1, 0)
	}
	since, until := start.Add(-time.Minute).Format(time.RFC3339), time.Now().Format(time.RFC3339)
	get := func(q url.Values) (int, string, string) {
		resp, err := hc.Get("https://" + a.Addrs["https"] + "/admin/api/history/export?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Disposition"), string(b)
	}

	code, disp, body := get(url.Values{"since": {since}, "until": {until}})
	if code != 200 || !strings.Contains(disp, ".kml") || !strings.Contains(body, "<name>ALPHA</name>") || !strings.Contains(body, "<name>RED1</name>") {
		t.Fatalf("KML = %d %s\n%s", code, disp, body)
	}
	code, disp, body = get(url.Values{"since": {since}, "until": {until}, "format": {"gpx"}, "uid": {"ANDROID-alpha"}})
	if code != 200 || !strings.Contains(disp, ".gpx") || strings.Count(body, "<trkpt") != 5 || strings.Contains(body, "RED1") {
		t.Fatalf("GPX for ALPHA = %d %s\n%s", code, disp, body)
	}
	if code, _, _ := get(url.Values{"since": {until}, "until": {since}}); code != 400 {
		t.Errorf("backwards range = %d", code)
	}
	if code, _, _ := get(url.Values{"since": {since}, "until": {until}, "uid": {"nobody"}}); code != 400 {
		t.Errorf("no data = %d", code)
	}

	// As an overlay: created, but not sent until an admin decides.
	code, v := call("POST", "/admin/api/history/overlay", map[string]any{"since": since, "until": until, "uids": []string{"ANDROID-red1"}, "name": "Red's route"})
	if code != 201 || v["devices"] != float64(1) {
		t.Fatalf("overlay = %d %v", code, v)
	}
	o, ok := a.Store.Overlay(v["id"].(string))
	if !ok || !o.Paused || o.Name != "Red's route" || o.Format != "kml" || o.Items != 3 {
		t.Errorf("overlay = %+v", o)
	}
}
