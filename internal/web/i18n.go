package web

import (
	"io/fs"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Translations: /admin/i18n.js sends the chosen language's dictionary
// (static/i18n/<lang>.json, English text -> translation) and the runtime
// that applies it (static/i18n/runtime.js). Every page loads it in <head>.
// The language is the edgetak_lang cookie (the language picker), else the
// browser's Accept-Language, else English.

var languages = []string{"en", "es", "fr"}

func pickLanguage(r *http.Request) string {
	if c, err := r.Cookie("edgetak_lang"); err == nil && slices.Contains(languages, c.Value) {
		return c.Value
	}
	type pref struct {
		lang string
		q    float64
	}
	var prefs []pref
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		base, _, _ := strings.Cut(strings.ToLower(tag), "-")
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				q = f
			}
		}
		prefs = append(prefs, pref{base, q})
	}
	slices.SortStableFunc(prefs, func(a, b pref) int {
		switch {
		case a.q > b.q:
			return -1
		case a.q < b.q:
			return 1
		}
		return 0
	})
	for _, p := range prefs {
		if slices.Contains(languages, p.lang) {
			return p.lang
		}
	}
	return "en"
}

func (s *Server) i18nScript(w http.ResponseWriter, r *http.Request) {
	lang := pickLanguage(r)
	dict := []byte("{}")
	if lang != "en" {
		if b, err := fs.ReadFile(staticSub(), "i18n/"+lang+".json"); err == nil {
			dict = b
		}
	}
	runtime, _ := fs.ReadFile(staticSub(), "i18n/runtime.js")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Vary", "Cookie, Accept-Language")
	w.Write([]byte("window.EDGETAK_LANG = " + strconv.Quote(lang) + ";\nwindow.EDGETAK_I18N = "))
	w.Write(dict)
	w.Write([]byte(";\n"))
	w.Write(runtime)
}
