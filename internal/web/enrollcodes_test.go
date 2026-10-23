package web

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

func TestEnrollCodeRules(t *testing.T) {
	s := guardServer(t)
	s.Store.CreateUser("bob", "bobs-password-1", nil, false)
	now := time.Now().UTC()
	add := func(id, user, code string, expires time.Time, oneDevice bool, used time.Time) {
		s.Store.AddEnrollCode(store.EnrollCode{ID: id, Username: user, Hash: hashCode(code), Expires: expires, OneDevice: oneDevice,
			Used: used, Created: now, CreatedBy: "admin"}, now)
	}
	add("fresh", "alice", "code-fresh", now.Add(time.Hour), true, time.Time{})
	add("grace", "alice", "code-grace", now.Add(time.Hour), true, now.Add(-5*time.Minute))
	add("spent", "alice", "code-spent", now.Add(time.Hour), true, now.Add(-11*time.Minute))
	add("many", "alice", "code-many", now.Add(time.Hour), false, now.Add(-time.Hour))
	add("old", "alice", "code-old", now.Add(-time.Minute), false, time.Time{})
	enrollReq := httptest.NewRequest("GET", "/Marti/api/tls/config", nil)
	for code, want := range map[string]bool{"code-fresh": true, "code-grace": true, "code-spent": false, "code-many": true, "code-old": false, "nope": false} {
		if _, ok := s.checkEnrollCode(enrollReq, "alice", code); ok != want {
			t.Errorf("%s: %v, want %v", code, ok, want)
		}
	}
	if _, ok := s.checkEnrollCode(enrollReq, "bob", "code-many"); ok {
		t.Error("alice's code worked for bob")
	}
	if _, ok := s.checkEnrollCode(httptest.NewRequest("GET", "/Marti/api/missions", nil), "alice", "code-many"); ok {
		t.Error("a code worked outside enrollment")
	}
	states := map[string]string{}
	for _, v := range s.enrollCodeViews() {
		states[v.ID] = v.State
	}
	if states["fresh"] != "active" || states["spent"] != "used" || states["old"] != "expired" || states["many"] != "active" {
		t.Errorf("states = %v", states)
	}
}

// TestDeviceHost covers the address packages and QR codes use: on a local
// server, the one the request came in on, unless that is localhost.
func TestDeviceHost(t *testing.T) {
	s := guardServer(t)
	s.Cfg.PublicHost = "10.0.0.5"
	for host, want := range map[string]string{
		"192.168.1.50:8443":  "192.168.1.50", // the Pi's new address
		"edgetak.local:8443": "edgetak.local",
		"[fd00::7]:8443":     "fd00::7",
		"localhost:8443":     "10.0.0.5", // on the server itself
		"127.0.0.1:8443":     "10.0.0.5",
		"[::1]:8443":         "10.0.0.5",
		"bad/host":           "10.0.0.5",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		if got := s.deviceHost(r); got != want {
			t.Errorf("%s: %q, want %q", host, got, want)
		}
	}
	s.Cfg.PublicHost = ""
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "localhost:8443"
	if got := s.deviceHost(r); got != "" {
		t.Errorf("localhost without public_host: %q", got)
	}
}
