package release

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v0.22.1", "v0.21.2", true},
		{"v0.21.2", "v0.22.1", false},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.0", "v1.0.0-rc.1", true},           // the release after its candidate
		{"v1.0.0", "v1.0.0-rc.2-3-gabc123", true}, // after a development build of a candidate
		{"v1.0.0", "v1.0.0-beta", true},
		{"v0.3.0", "v0.3.0-2-gabc123-dirty", false}, // a development build after its tag
		{"v0.22.1", "v1.0.0-rc.1", false},
		{"v1.0.0-rc.1", "v0.22.1", true}, // a candidate for a later version
		{"v1.0.0-rc.2", "v1.0.0-rc.1", true},
		{"v1.0.0-rc.1", "v1.0.0-rc.2", false},
		{"v1.0.0-rc.1", "v1.0.0-rc.1-4-gabc123", false},
		{"v1.0.0-rc.1", "v1.0.0-beta.3", true},
		{"v1.0.0-rc.1", "v1.0.0", false}, // never back to a candidate
		{"v1.0.1", "v1.0.0-rc.1", true},
		{"nonsense", "v1.0.0", false},
	} {
		if got := Newer(tc.a, tc.b); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestLatestAny(t *testing.T) {
	releases := `[
	 {"tag_name":"v1.0.0-rc.3","draft":true,"prerelease":true},
	 {"tag_name":"v1.0.0-rc.2","prerelease":true,"body":"rc2 notes"},
	 {"tag_name":"v0.22.1","body":"stable"},
	 {"tag_name":"v1.0.0-rc.1","prerelease":true},
	 {"tag_name":"something-else"}]`
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/releases" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, releases)
	}))
	defer gh.Close()
	rel, err := LatestAny(context.Background(), gh.URL, "o/r")
	if err != nil || rel.Version != "v1.0.0-rc.2" || !rel.Pre || rel.Notes != "rc2 notes" {
		t.Fatalf("latest with pre-releases = %+v %v (the draft rc.3 must be skipped)", rel, err)
	}
	// Once the release is out it wins over its candidates.
	releases = `[{"tag_name":"v1.0.0"},{"tag_name":"v1.0.0-rc.2","prerelease":true}]`
	if rel, err := LatestAny(context.Background(), gh.URL, "o/r"); err != nil || rel.Version != "v1.0.0" || rel.Pre {
		t.Fatalf("after the release = %+v %v", rel, err)
	}
}
