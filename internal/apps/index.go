package apps

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// ATAK's update server layout, as ATAK's RemoteProductProvider reads it:
//
//	<url>/product.infz            zip of product.inf and the icons
//	<url>/<atak version>/...      the same for one ATAK version, tried first
//	                              (e.g. 5.4.0); a 404 falls back to <url>
//
// product.inf has one line per APK with 13 columns: platform, type, package,
// name, version, version code, APK path, icon path, description, SHA-256,
// minimum Android SDK, ATAK plugin API, size. Paths are relative to the
// index. ATAK ignores entries without an icon or description.

// APKPath and IconPath are an app's paths relative to the index.
func APKPath(a store.App) string  { return "apk/" + a.SHA256 + ".apk" }
func IconPath(a store.App) string { return "icons/" + a.SHA256 + ".png" }

// ATAKVersion is the ATAK version a plugin is built for: "5.4.0" from the
// plugin API "com.atakmap.app@5.4.0.CIV"; "" for apps.
func ATAKVersion(pluginAPI string) string {
	_, v, ok := strings.Cut(pluginAPI, "@")
	if !ok {
		return ""
	}
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		return v
	}
	return strings.Join(parts[:3], ".")
}

// Select picks what an index offers: for an ATAK version, the plugins built
// for it plus apps; for "" (the general index), every plugin and app. Only
// the newest build of each package is listed per index.
func Select(all []store.App, version string) []store.App {
	best := map[string]store.App{}
	for _, a := range all {
		if version != "" && a.PluginAPI != "" && ATAKVersion(a.PluginAPI) != version {
			continue
		}
		if b, ok := best[a.Package]; !ok || newer(a, b, version) {
			best[a.Package] = a
		}
	}
	out := make([]store.App, 0, len(best))
	for _, a := range best {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b store.App) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}

func newer(a, b store.App, version string) bool {
	if a.VersionCode != b.VersionCode {
		return a.VersionCode > b.VersionCode
	}
	if a.PluginAPI != b.PluginAPI {
		return a.PluginAPI > b.PluginAPI
	}
	return a.Uploaded.After(b.Uploaded)
}

// HasVersion reports whether any plugin is built for the ATAK version, so a
// version-specific index is worth serving.
func HasVersion(all []store.App, version string) bool {
	for _, a := range all {
		if a.PluginAPI != "" && ATAKVersion(a.PluginAPI) == version {
			return true
		}
	}
	return false
}

// field escapes a value the way ATAK persists it.
func field(s string) string {
	s = strings.ReplaceAll(s, ",", `\u002c`)
	s = strings.ReplaceAll(s, "\r", "")
	return strings.ReplaceAll(s, "\n", `\n`)
}

// ProductInf is the index text.
func ProductInf(list []store.App) []byte {
	var b strings.Builder
	for _, a := range list {
		typ := "app"
		if a.PluginAPI != "" {
			typ = "plugin"
		}
		desc := a.Description
		if strings.TrimSpace(desc) == "" {
			desc = a.Name
		}
		version := a.Version
		if version == "" {
			version = strconv.FormatInt(a.VersionCode, 10)
		}
		cols := []string{"Android", typ, field(a.Package), field(a.Name), field(version), strconv.FormatInt(a.VersionCode, 10),
			APKPath(a), IconPath(a), field(desc), a.SHA256, strconv.Itoa(a.MinSDK), field(a.PluginAPI), strconv.FormatInt(a.Size, 10)}
		b.WriteString(strings.Join(cols, ",") + "\n")
	}
	return []byte(b.String())
}

// ProductInfz is the zipped index with the icons; icon returns an app's
// icon (nil for the placeholder).
func ProductInfz(list []store.App, icon func(store.App) []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("product.inf")
	if err != nil {
		return nil, err
	}
	w.Write(ProductInf(list))
	for _, a := range list {
		data := icon(a)
		if len(data) == 0 {
			data = Placeholder()
		}
		w, err := zw.Create(IconPath(a))
		if err != nil {
			return nil, err
		}
		w.Write(data)
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var placeholder = sync.OnceValue(func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 96, 96))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{0x4c, 0xc3, 0x8a, 0xff}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(24, 24, 72, 72), &image.Uniform{color.RGBA{0x0e, 0x12, 0x14, 0xff}}, image.Point{}, draw.Src)
	return encodePNG(img)
})

// Placeholder is the icon for APKs whose icon EdgeTAK cannot read (vector
// and adaptive icons).
func Placeholder() []byte { return placeholder() }
