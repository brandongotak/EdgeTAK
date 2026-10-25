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
	for _, f := range []string{"static/app.js", "static/i18n/runtime.js"} {
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Fatalf("%s does not parse:\n%s", f, out)
		}
	}
}
