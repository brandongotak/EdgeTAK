package web

import (
	"os/exec"
	"testing"
)

// TestConsoleScriptParses catches syntax errors in the embedded admin
// console, which would otherwise only show up as a blank page in a browser.
func TestConsoleScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	if out, err := exec.Command(node, "--check", "static/app.js").CombinedOutput(); err != nil {
		t.Fatalf("static/app.js does not parse:\n%s", out)
	}
}
