// Package release looks up GitHub releases and compares versions.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// Release is a published GitHub release.
type Release struct {
	Version   string    `json:"version"`
	Published time.Time `json:"published"`
	URL       string    `json:"url"`
	Notes     string    `json:"notes"`
	Pre       bool      `json:"prerelease,omitempty"`
}

// VersionRE matches a release tag such as v1.2.3.
var VersionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// TagRE matches a release tag, a pre-release included: v1.2.3, v1.2.3-rc.1,
// v1.2.3-beta.2, v1.2.3-alpha.
var TagRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-(alpha|beta|rc)(?:\.(\d+))?)?$`)

// prefixRE matches the version at the start of a build string such as
// "v0.2.14-3-gabc123-dirty".
var prefixRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// Latest fetches the latest stable release of repo ("owner/name") from the
// GitHub API at apiBase ("" = https://api.github.com).
func Latest(ctx context.Context, apiBase, repo string) (*Release, error) {
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "EdgeTAK")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub replied %s", res.Status)
	}
	var r ghRelease
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&r); err != nil {
		return nil, err
	}
	if !VersionRE.MatchString(r.Tag) {
		return nil, fmt.Errorf("unexpected release tag %q", r.Tag)
	}
	return r.release(), nil
}

type ghRelease struct {
	Tag       string    `json:"tag_name"`
	Published time.Time `json:"published_at"`
	URL       string    `json:"html_url"`
	Body      string    `json:"body"`
	Draft     bool      `json:"draft"`
	Pre       bool      `json:"prerelease"`
}

func (r ghRelease) release() *Release {
	notes := r.Body
	if len(notes) > 8000 {
		notes = notes[:8000] + "…"
	}
	return &Release{Version: r.Tag, Published: r.Published, URL: r.URL, Notes: notes, Pre: r.Pre}
}

// LatestAny is the newest published release of repo, pre-releases
// included (from its most recent releases).
func LatestAny(ctx context.Context, apiBase, repo string) (*Release, error) {
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/repos/"+repo+"/releases?per_page=30", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "EdgeTAK")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub replied %s", res.Status)
	}
	var list []ghRelease
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&list); err != nil {
		return nil, err
	}
	var best *ghRelease
	for i, r := range list {
		if r.Draft || !TagRE.MatchString(r.Tag) {
			continue
		}
		if best == nil || Newer(r.Tag, best.Tag) {
			best = &list[i]
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no releases found")
	}
	return best.release(), nil
}

// Newer reports whether release a (a tag, pre-releases included) is later
// than version b. b may be a build string with a suffix
// ("v1.2.3-4-gabc-dirty", which comes after v1.2.3); an unparsable b is
// never older than anything. A release comes after its pre-releases, and
// rc after beta after alpha.
func Newer(a, b string) bool {
	pa, pb := TagRE.FindStringSubmatch(a), prefixRE.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	ra, na := stage(pa[4], pa[5])
	rb, nb := 3, 0 // a final release, or a development build after one
	if m := preReleaseRE.FindStringSubmatch(b); m != nil {
		rb, nb = stage(m[1], m[2])
	}
	if ra != rb {
		return ra > rb
	}
	return na > nb
}

// stage ranks a pre-release: alpha 0, beta 1, rc 2, the release itself 3.
func stage(kind, n string) (rank, num int) {
	num, _ = strconv.Atoi(n)
	switch kind {
	case "alpha":
		return 0, num
	case "beta":
		return 1, num
	case "rc":
		return 2, num
	}
	return 3, 0
}

var preReleaseRE = regexp.MustCompile(`^v?\d+\.\d+\.\d+-(alpha|beta|rc)(?:\.(\d+))?`)
