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
