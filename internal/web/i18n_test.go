package web

import (
	"encoding/json"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestPickLanguage(t *testing.T) {
	for _, tc := range []struct{ cookie, accept, want string }{
		{"", "", "en"},
		{"", "fr-CA,fr;q=0.9,en;q=0.8", "fr"},
		{"", "de-DE,es;q=0.5,fr;q=0.7", "fr"},
		{"", "de, ja", "en"},
		{"es", "fr", "es"},
		{"xx", "fr", "fr"},
		{"en", "es", "en"},
	} {
		r := httptest.NewRequest("GET", "/admin/i18n.js", nil)
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: "edgetak_lang", Value: tc.cookie})
		}
		if tc.accept != "" {
			r.Header.Set("Accept-Language", tc.accept)
		}
		if got := pickLanguage(r); got != tc.want {
			t.Errorf("cookie %q, Accept-Language %q: got %s, want %s", tc.cookie, tc.accept, got, tc.want)
		}
	}
}

// TestDictionaries checks every translation keeps its English text's
// {0}, {1}... placeholders, and that each language has a Help page.
func TestDictionaries(t *testing.T) {
	ph := regexp.MustCompile(`\{\d+\}`)
	count := func(s string) map[string]int {
		m := map[string]int{}
		for _, p := range ph.FindAllString(s, -1) {
			m[p]++
		}
		return m
	}
	for _, lang := range languages[1:] {
		b, err := fs.ReadFile(staticSub(), "i18n/"+lang+".json")
		if err != nil {
			t.Fatal(err)
		}
		var dict map[string]string
		if err := json.Unmarshal(b, &dict); err != nil {
			t.Fatalf("%s.json: %v", lang, err)
		}
		if len(dict) < 1000 {
			t.Errorf("%s.json has only %d entries", lang, len(dict))
		}
		for _, k := range slices.Sorted(maps.Keys(dict)) {
			if !maps.Equal(count(k), count(dict[k])) {
				t.Errorf("%s.json: placeholders differ: %q -> %q", lang, k, dict[k])
			}
			if strings.TrimSpace(dict[k]) == "" {
				t.Errorf("%s.json: empty translation for %q", lang, k)
			}
		}
		if _, err := fs.Stat(staticSub(), "help."+lang+".html"); err != nil {
			t.Errorf("no Help page for %s: %v", lang, err)
		}
	}
}

func TestI18nScript(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/admin/i18n.js", nil)
	r.Header.Set("Accept-Language", "es")
	w := httptest.NewRecorder()
	s.i18nScript(w, r)
	body := w.Body.String()
	if !strings.HasPrefix(body, "window.EDGETAK_LANG = \"es\";\nwindow.EDGETAK_I18N = {") {
		t.Fatalf("unexpected start: %.80q", body)
	}
	if !strings.Contains(body, "window.translateDOM") {
		t.Error("runtime missing")
	}
	if v := w.Header().Get("Vary"); !strings.Contains(v, "Accept-Language") {
		t.Errorf("Vary = %q", v)
	}
}
