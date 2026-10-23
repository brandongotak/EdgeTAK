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
}

// VersionRE matches a release tag such as v1.2.3.
var VersionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

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
	var r struct {
		Tag       string    `json:"tag_name"`
		Published time.Time `json:"published_at"`
		URL       string    `json:"html_url"`
		Body      string    `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&r); err != nil {
		return nil, err
	}
	if !VersionRE.MatchString(r.Tag) {
		return nil, fmt.Errorf("unexpected release tag %q", r.Tag)
	}
	notes := r.Body
	if len(notes) > 8000 {
		notes = notes[:8000] + "…"
	}
	return &Release{Version: r.Tag, Published: r.Published, URL: r.URL, Notes: notes}, nil
}

// Newer reports whether release a is later than version b. b may be a build
// string with a suffix ("v1.2.3-4-gabc-dirty"); an unparsable b is never
// older than anything.
func Newer(a, b string) bool {
	pa, pb := VersionRE.FindStringSubmatch(a), prefixRE.FindStringSubmatch(b)
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
	// Same numbers: a release candidate (v1.0.0-rc.1) comes before its
	// release; a development build (v0.3.0-2-gabc123) comes after its tag.
	return preReleaseRE.MatchString(b)
}

var preReleaseRE = regexp.MustCompile(`^v?\d+\.\d+\.\d+-(rc|beta|alpha)([.0-9]|$)`)
