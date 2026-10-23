package release

import "testing"

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v0.22.1", "v0.21.2", true},
		{"v0.21.2", "v0.22.1", false},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.0", "v1.0.0-rc.1", true},          // the release after its candidate
		{"v1.0.0", "v1.0.0-rc.2-3-gabc123", true}, // after a development build of a candidate
		{"v1.0.0", "v1.0.0-beta", true},
		{"v0.3.0", "v0.3.0-2-gabc123-dirty", false}, // a development build after its tag
		{"v0.22.1", "v1.0.0-rc.1", false},
		{"v1.0.0-rc.2", "v1.0.0-rc.1", false}, // candidates aren't offered as updates
		{"v1.0.1", "v1.0.0-rc.1", true},
	} {
		if got := Newer(tc.a, tc.b); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
