package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAuditActionsCoverRoutes checks that every admin route that changes
// something has a description in the audit log (or is listed as quiet).
func TestAuditActionsCoverRoutes(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	route := regexp.MustCompile(`HandleFunc\("((?:POST|PUT|PATCH|DELETE) /admin/api/[^"]+)", s\.admin`)
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range route.FindAllStringSubmatch(string(src), -1) {
			seen++
			if _, ok := auditActions[m[1]]; !ok && !auditQuiet[m[1]] {
				t.Errorf("%s: %q has no audit description", f, m[1])
			}
		}
	}
	if seen < 50 {
		t.Errorf("found only %d admin routes; is the pattern still right?", seen)
	}
}

func TestAuditDetails(t *testing.T) {
	got := auditDetails([]byte(`{"username":"bob","password":"x","groups":["A","B"],"admin":false,"url":"https://h/t","bind_password":"y","max":3,"nested":{"a":1}}`))
	if got != "admin: false; groups: [A, B]; max: 3; username: bob" {
		t.Errorf("details = %q", got)
	}
}
