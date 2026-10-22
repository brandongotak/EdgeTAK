package apps

import (
	"archive/zip"
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

func TestReadAPK(t *testing.T) {
	info, err := ReadAPK("testdata/helloworld.apk")
	if err != nil {
		t.Fatal(err)
	}
	if info.Package != "com.example.helloworld" || info.Name == "" || info.VersionCode != 1 || info.PluginAPI != "" {
		t.Errorf("info = %+v", info)
	}
	if _, err := ReadAPK("apps_test.go"); err == nil {
		t.Error("a Go file read as an APK")
	}
}

func TestIconPNG(t *testing.T) {
	big := image.NewRGBA(image.Rect(0, 0, 512, 400))
	var src bytes.Buffer
	jpeg.Encode(&src, big, nil)
	out, err := IconPNG(src.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil || img.Bounds().Dx() != 192 || img.Bounds().Dy() != 150 {
		t.Errorf("scaled icon = %v %v", img.Bounds(), err)
	}
	if _, err := IconPNG([]byte("not an image")); err == nil {
		t.Error("text accepted as an icon")
	}
}

// ATAK loads plugins built for its version or an older one (from 4.10.0):
// a 5.6 plugin runs on 5.7.
func TestSelectForATAKVersion(t *testing.T) {
	now := time.Now()
	plugin := func(id, pkg, atak string, code int64) store.App {
		return store.App{SHA256: id, Package: pkg, Name: pkg, VersionCode: code, PluginAPI: "com.atakmap.app@" + atak + ".CIV", Uploaded: now}
	}
	all := []store.App{
		plugin("p56", "com.p", "5.6.0", 5),
		plugin("q56", "com.q", "5.6.0", 1),
		plugin("q57", "com.q", "5.7.0", 1), // built for 5.7: preferred on 5.7, though not a newer version code
		plugin("r58", "com.r", "5.8.0", 9), // too new for 5.7
		plugin("s49", "com.s", "4.9.0", 1), // before 4.10: only on 4.9.0
		plugin("t510", "com.t", "5.10.0", 1),
		{SHA256: "app", Package: "com.app", Name: "app", VersionCode: 1, Uploaded: now},
	}
	ids := func(l []store.App) map[string]bool {
		out := map[string]bool{}
		for _, a := range l {
			out[a.SHA256] = true
		}
		return out
	}
	for version, want := range map[string][]string{
		"5.7.0":  {"p56", "q57", "app"},
		"5.6.0":  {"p56", "q56", "app"},
		"5.10.0": {"p56", "q57", "r58", "t510", "app"},
		"4.9.0":  {"s49", "app"},
	} {
		got := ids(Select(all, version))
		if len(got) != len(want) {
			t.Errorf("%s: got %v, want %v", version, got, want)
			continue
		}
		for _, id := range want {
			if !got[id] {
				t.Errorf("%s: got %v, want %v", version, got, want)
			}
		}
	}
	if CompareVersions("5.10.0", "5.9.0") <= 0 || CompareVersions("5.6.0", "5.6.0") != 0 {
		t.Error("CompareVersions")
	}
}

func TestIndex(t *testing.T) {
	now := time.Now()
	all := []store.App{
		{SHA256: "a1", Package: "com.x.plugin", Name: "X, the plugin", Version: "1.0", VersionCode: 10, PluginAPI: "com.atakmap.app@5.4.0.CIV", Description: "line one\nline two", MinSDK: 21, Size: 5, Uploaded: now},
		{SHA256: "a2", Package: "com.x.plugin", Name: "X", Version: "0.9", VersionCode: 9, PluginAPI: "com.atakmap.app@5.3.0.CIV", Uploaded: now},
		{SHA256: "b1", Package: "com.y.app", Name: "Y", VersionCode: 3, Uploaded: now},
	}
	if v := ATAKVersion("com.atakmap.app@5.4.0.CIV"); v != "5.4.0" {
		t.Errorf("ATAKVersion = %q", v)
	}
	if !IsVersion("5.7.0") || IsVersion("apk") || IsVersion("5.7") {
		t.Error("IsVersion")
	}
	// The 5.3.0 index has the 5.3.0 build and the app; the general one the newest build.
	if l := Select(all, "5.3.0"); len(l) != 2 || l[0].SHA256 != "a2" || l[1].SHA256 != "b1" {
		t.Errorf("5.3.0 = %+v", l)
	}
	l := Select(all, "")
	if len(l) != 2 || l[0].SHA256 != "a1" {
		t.Errorf("general = %+v", l)
	}
	lines := strings.Split(strings.TrimSpace(string(ProductInf(l))), "\n")
	cols := strings.Split(lines[0], ",")
	if len(cols) != 13 || cols[0] != "Android" || cols[1] != "plugin" || cols[3] != `X\u002c the plugin` || cols[6] != "apk/a1.apk" ||
		cols[7] != "icons/a1.png" || cols[8] != `line one\nline two` || cols[9] != "a1" || cols[11] != "com.atakmap.app@5.4.0.CIV" {
		t.Errorf("plugin line = %q", lines[0])
	}
	if c := strings.Split(lines[1], ","); c[1] != "app" || c[4] != "3" || c[8] != "Y" {
		t.Errorf("app line (version and description filled in) = %q", lines[1])
	}
	z, err := ProductInfz(l, func(store.App) []byte { return nil })
	if err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(z), int64(len(z)))
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if strings.Join(names, " ") != "product.inf icons/a1.png icons/b1.png" {
		t.Errorf("infz = %v", names)
	}
}
