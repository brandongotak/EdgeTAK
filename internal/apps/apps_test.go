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
	if !HasVersion(all, "5.3.0") || HasVersion(all, "5.5.0") {
		t.Error("HasVersion")
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
