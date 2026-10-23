package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/release"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// TestPreReleaseUpdates: release candidates are only offered as updates
// when an admin turns pre-releases on.
func TestPreReleaseUpdates(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + edgetakRepo + "/releases/latest":
			fmt.Fprint(w, `{"tag_name":"v0.22.1"}`)
		case "/repos/" + edgetakRepo + "/releases":
			fmt.Fprint(w, `[{"tag_name":"v1.0.0-rc.1","prerelease":true},{"tag_name":"v0.22.1"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	old := githubAPI
	githubAPI = gh.URL
	defer func() { githubAPI = old }()

	s := guardServer(t)
	s.Version = "v0.22.1"
	if err := s.checkSelfUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := s.selfUpdateView(); v["update_available"] != false || v["prereleases"] != false {
		t.Errorf("pre-releases off = %v", v)
	}
	s.Store.SetSelfUpdate(store.SelfUpdateSettings{PreReleases: true})
	if err := s.checkSelfUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := s.selfUpdateView()
	if l, _ := v["latest"].(*release.Release); v["update_available"] != true || v["prereleases"] != true || l == nil || l.Version != "v1.0.0-rc.1" {
		t.Errorf("pre-releases on = %v", v)
	}
}
