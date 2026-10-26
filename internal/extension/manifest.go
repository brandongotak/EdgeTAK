// Package extension installs and runs EdgeTAK extensions: server plugins
// that add features without changing EdgeTAK. An extension is a program in
// any language, uploaded as a zip with a plugin.json manifest. EdgeTAK
// unpacks it into <data>/extensions/<id>, runs it as a child process
// (restarting it if it stops) and gives it, in environment variables, the
// address of a local API and a token for it. Through the API it receives
// and sends CoT and reads server data, as far as its permissions allow, and
// it may serve a page that the console shows behind the admin login.
//
// An extension is code running on the server: installing one means
// trusting it like any other program. Its permissions limit what the API
// lets it do, not what the program can do.
package extension

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

// ManifestFile is the manifest's name at the root of the zip.
const ManifestFile = "plugin.json"

// The permissions an extension can ask for.
const (
	PermCoTRead  = "cot:read"  // receive the CoT of its in groups
	PermCoTWrite = "cot:write" // send CoT to its out groups
	PermDataRead = "data:read" // read groups, connected devices, missions, position history
)

var permissions = []string{PermCoTRead, PermCoTWrite, PermDataRead}

// Manifest is plugin.json.
type Manifest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Author      string `json:"author,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
	// Run is the command to start, by platform: "linux/arm64", "linux",
	// "windows/amd64", "windows" or "*", the most specific match winning.
	// A first word naming a file in the extension is run from there;
	// otherwise it is looked up on the PATH (python3, node...).
	Run         map[string][]string `json:"run"`
	Permissions []string            `json:"permissions,omitempty"`
	// Page: the extension serves a web page on EDGETAK_PAGE_PORT, shown in
	// the console.
	Page bool `json:"page,omitempty"`
}

var reID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ParseManifest reads and checks a manifest.
func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s is not valid JSON: %v", ManifestFile, err)
	}
	m.Name, m.Version = strings.TrimSpace(m.Name), strings.TrimSpace(m.Version)
	switch {
	case !reID.MatchString(m.ID):
		return m, fmt.Errorf("%s: id must be 1-40 lowercase letters, digits and dashes", ManifestFile)
	case m.Name == "" || len(m.Name) > 80:
		return m, fmt.Errorf("%s: name must be 1-80 characters", ManifestFile)
	case m.Version == "" || len(m.Version) > 40:
		return m, fmt.Errorf("%s: version must be 1-40 characters", ManifestFile)
	case len(m.Description) > 500:
		return m, fmt.Errorf("%s: description can be up to 500 characters", ManifestFile)
	case len(m.Run) == 0:
		return m, fmt.Errorf("%s: run must give the command to start", ManifestFile)
	}
	for k, cmd := range m.Run {
		if len(cmd) == 0 || cmd[0] == "" {
			return m, fmt.Errorf("%s: run[%q] is empty", ManifestFile, k)
		}
	}
	for _, p := range m.Permissions {
		if !slices.Contains(permissions, p) {
			return m, fmt.Errorf("%s: unknown permission %q (use %s)", ManifestFile, p, strings.Join(permissions, ", "))
		}
	}
	return m, nil
}

// Command returns the command for this platform.
func (m Manifest) Command() ([]string, error) {
	for _, k := range []string{runtime.GOOS + "/" + runtime.GOARCH, runtime.GOOS, "*"} {
		if c, ok := m.Run[k]; ok {
			return c, nil
		}
	}
	return nil, errors.New("this extension has no command for " + runtime.GOOS + "/" + runtime.GOARCH)
}

// Can reports whether the extension has permission p.
func (m Manifest) Can(p string) bool { return slices.Contains(m.Permissions, p) }
