// Package apps serves ATAK plugins and apps to devices the way ATAK's
// update server expects (Plugins > Update Server): a product.infz index
// (a zip of product.inf and the icons) with the APKs beside it.
package apps

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"strings"

	_ "image/jpeg" // icons may be JPEG or WebP

	_ "golang.org/x/image/webp"

	"github.com/shogo82148/androidbinary"
	"github.com/shogo82148/androidbinary/apk"
)

// Info is what EdgeTAK reads from an APK.
type Info struct {
	Package     string
	Name        string // the app's label
	Version     string // versionName
	VersionCode int64
	MinSDK      int
	PluginAPI   string // "plugin-api" meta-data: set for ATAK plugins
	Description string // "app_desc" meta-data
	Icon        []byte // PNG, when the APK has a bitmap icon
}

// ReadAPK reads an APK's manifest, label, plugin metadata and icon.
func ReadAPK(path string) (*Info, error) {
	pkg, err := apk.OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("not an APK: %w", err)
	}
	defer pkg.Close()
	m := pkg.Manifest()
	info := &Info{Package: m.Package.MustString(), Version: m.VersionName.MustString()}
	if info.Package == "" {
		return nil, errors.New("the APK has no package name")
	}
	if v, err := m.VersionCode.Int32(); err == nil {
		info.VersionCode = int64(v)
	}
	if v, err := m.SDK.Min.Int32(); err == nil {
		info.MinSDK = int(v)
	}
	cfg := &androidbinary.ResTableConfig{}
	info.Name, _ = pkg.Label(cfg)
	if info.Name == "" || androidbinary.IsResID(info.Name) {
		info.Name = info.Package
	}
	for _, md := range m.App.MetaData {
		name, _ := md.Name.String()
		value, err := md.Value.WithResTableConfig(cfg).String()
		if err != nil || androidbinary.IsResID(value) {
			continue
		}
		switch name {
		case "plugin-api":
			info.PluginAPI = strings.TrimSpace(value)
		case "app_desc":
			info.Description = strings.TrimSpace(value)
		}
	}
	info.Icon = icon(pkg)
	return info, nil
}

// icon is the APK's launcher icon as a PNG, at the highest density that is a
// bitmap (adaptive icons are XML and are skipped).
func icon(pkg *apk.Apk) []byte {
	for _, density := range []uint16{640, 480, 320, 240, 160, 0} {
		for _, sdk := range []uint16{25, 0} {
			img, err := pkg.Icon(&androidbinary.ResTableConfig{Density: density, SDKVersion: sdk})
			if err != nil || img == nil {
				continue
			}
			return encodePNG(img)
		}
	}
	return nil
}

func encodePNG(img image.Image) []byte {
	var b bytes.Buffer
	if png.Encode(&b, img) != nil {
		return nil
	}
	return b.Bytes()
}
