// Package apps serves ATAK plugins and apps to devices the way ATAK's
// update server expects (Plugins > Update Server): a product.infz index
// (a zip of product.inf and the icons) with the APKs beside it.
package apps

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
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
	if info.Icon == nil {
		info.Icon = drawnIcon(path, m)
	}
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

// IconPNG turns an uploaded icon (PNG, JPEG or WebP) into a PNG, scaled down
// to at most 192 pixels square, ATAK's largest launcher icon size.
func IconPNG(data []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("the icon must be a PNG, JPEG or WebP image")
	}
	if b := img.Bounds(); b.Dx() > iconMax || b.Dy() > iconMax {
		img = shrink(img, iconMax)
	}
	out := encodePNG(img)
	if out == nil {
		return nil, errors.New("the icon could not be converted")
	}
	return out, nil
}

const iconMax = 192

// Scale scales img, up or down, to fit in size x size.
func Scale(img image.Image, size int) image.Image { return shrink(img, size) }

// EncodePNG encodes img as a PNG, or returns nil if it can't.
func EncodePNG(img image.Image) []byte { return encodePNG(img) }

// shrink scales img to fit in size x size, averaging the pixels each output
// pixel covers.
func shrink(img image.Image, size int) image.Image {
	b := img.Bounds()
	scale := float64(size) / float64(b.Dx())
	if s := float64(size) / float64(b.Dy()); s < scale {
		scale = s
	}
	w, h := int(float64(b.Dx())*scale), int(float64(b.Dy())*scale)
	w, h = max(w, 1), max(h, 1)
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := b.Min.Y+y*b.Dy()/h, b.Min.Y+(y+1)*b.Dy()/h
		for x := 0; x < w; x++ {
			x0, x1 := b.Min.X+x*b.Dx()/w, b.Min.X+(x+1)*b.Dx()/w
			var r, g, bl, a, n uint64
			for sy := y0; sy < max(y1, y0+1); sy++ {
				for sx := x0; sx < max(x1, x0+1); sx++ {
					c := color.NRGBAModel.Convert(img.At(sx, sy)).(color.NRGBA)
					r, g, bl, a, n = r+uint64(c.R), g+uint64(c.G), bl+uint64(c.B), a+uint64(c.A), n+1
				}
			}
			out.SetNRGBA(x, y, color.NRGBA{uint8(r / n), uint8(g / n), uint8(bl / n), uint8(a / n)})
		}
	}
	return out
}

// drawnIcon draws an icon that Android draws from XML (vector drawables,
// adaptive icons, layer lists); nil when there is nothing to draw.
func drawnIcon(path string, m apk.Manifest) []byte {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil
	}
	defer zr.Close()
	r := newDrawableRenderer(&zr.Reader)
	for _, sdk := range []uint16{0, 26} {
		p, err := m.App.Icon.WithResTableConfig(&androidbinary.ResTableConfig{Density: 480, SDKVersion: sdk}).String()
		if err != nil || !strings.HasSuffix(p, ".xml") {
			continue
		}
		if img := r.render(p, iconSize, 0); img != nil && !blank(img) {
			return encodePNG(img)
		}
	}
	return nil
}

// blank reports whether an image is fully transparent.
func blank(img image.Image) bool {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a > 0 {
				return false
			}
		}
	}
	return true
}

func encodePNG(img image.Image) []byte {
	var b bytes.Buffer
	if png.Encode(&b, img) != nil {
		return nil
	}
	return b.Bytes()
}
