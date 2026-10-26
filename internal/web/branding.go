package web

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"net/http"
	"os"
	"path/filepath"

	_ "image/jpeg" // logos may be PNG, JPEG or WebP

	_ "golang.org/x/image/webp"

	"github.com/brandongotak/EdgeTAK/internal/apps"
)

// The server's logo: an uploaded image shown in place of the ◆ mark on the
// console, account, sign-in and sign-up pages and on printed QR sheets, and
// used for the browser and installed-app icons. It is stored as PNG (so
// what is served is never a script, whatever was uploaded) in the data
// folder's branding directory, which backups keep.

const (
	maxLogo       = 5 << 20
	maxLogoPixels = 40 << 20 // decoded size limit, against tiny files that unpack huge
	logoSize      = 512
)

// brandIcons are the console's icon files a logo replaces, with their size
// and whether they need a solid background (iOS and Android masks).
var brandIcons = map[string]struct {
	size  int
	solid bool
}{
	"favicon-32.png":        {32, false},
	"apple-touch-icon.png":  {180, true},
	"icon-192.png":          {192, false},
	"icon-512.png":          {512, false},
	"icon-maskable-512.png": {512, true},
}

func (s *Server) brandingDir() string { return filepath.Join(filepath.Dir(s.PackageDir), "branding") }
func (s *Server) logoFile() string    { return filepath.Join(s.brandingDir(), "logo.png") }

// logoVersion changes whenever the logo does, so pages can link to it past
// browser caches; "" means there is no logo.
func (s *Server) logoVersion() string {
	st, err := os.Stat(s.logoFile())
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", st.ModTime().UnixNano())
}

func (s *Server) brandingRoutes(mux *http.ServeMux) {
	// Public: the sign-in pages show it too.
	mux.HandleFunc("GET /admin/logo.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, s.logoFile())
	})
	mux.HandleFunc("GET /admin/icons/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		name := r.PathValue("name")
		if _, ok := brandIcons[name]; ok && s.logoVersion() != "" {
			if p := filepath.Join(s.brandingDir(), name); fileExists(p) {
				http.ServeFile(w, r, p)
				return
			}
		}
		http.ServeFileFS(w, r, staticSub(), "icons/"+name)
	})
	mux.HandleFunc("GET /admin/api/logo", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, map[string]any{"logo": s.logoVersion()})
	}))
	mux.HandleFunc("PUT /admin/api/logo", s.admin(s.setLogo))
	mux.HandleFunc("DELETE /admin/api/logo", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		os.Remove(s.logoFile())
		for name := range brandIcons {
			os.Remove(filepath.Join(s.brandingDir(), name))
		}
		s.Log.Info("server logo removed", "admin", id.Username)
		writeJSON(w, http.StatusOK, map[string]any{"logo": ""})
	}))
}

// setLogo stores an uploaded image (the request body) as the logo, and
// makes the icons from it.
func (s *Server) setLogo(w http.ResponseWriter, r *http.Request, id *identity) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLogo))
	if err != nil {
		jsonError(w, http.StatusRequestEntityTooLarge, "the logo can be up to 5 MB")
		return
	}
	files, err := brandingFiles(data)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(s.brandingDir(), 0o750); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Icons first, the logo last: its presence is what turns them on.
	for name, b := range files {
		if name == "logo.png" {
			continue
		}
		if err := writeAtomic(filepath.Join(s.brandingDir(), name), b); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := writeAtomic(s.logoFile(), files["logo.png"]); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("server logo changed", "admin", id.Username)
	writeJSON(w, http.StatusOK, map[string]any{"logo": s.logoVersion()})
}

// brandingFiles turns an uploaded PNG, JPEG or WebP image into logo.png
// and the icons, by file name.
func brandingFiles(data []byte) (map[string][]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("the logo must be a PNG, JPEG or WebP image")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width*cfg.Height > maxLogoPixels {
		return nil, errors.New("the logo image is too large (40 megapixels at most)")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("the logo must be a PNG, JPEG or WebP image")
	}
	logo := img
	if b := img.Bounds(); b.Dx() > logoSize || b.Dy() > logoSize {
		logo = apps.Scale(img, logoSize)
	}
	files := map[string][]byte{"logo.png": apps.EncodePNG(logo)}
	bg := iconBackground(logo)
	for name, ic := range brandIcons {
		files[name] = apps.EncodePNG(squareIcon(logo, ic.size, ic.solid, bg))
	}
	for name, b := range files {
		if b == nil {
			return nil, fmt.Errorf("the logo could not be converted (%s)", name)
		}
	}
	return files, nil
}

// squareIcon centres img on a size x size square. A solid icon gets a
// background and more margin, since iOS and Android crop its edges.
func squareIcon(img image.Image, size int, solid bool, bg color.Color) image.Image {
	inner := size
	if solid {
		inner = size * 7 / 10
	} else if size >= 64 {
		inner = size * 9 / 10
	}
	fit := apps.Scale(img, inner)
	out := image.NewNRGBA(image.Rect(0, 0, size, size))
	if solid {
		draw.Draw(out, out.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
	}
	fb := fit.Bounds()
	at := image.Pt((size-fb.Dx())/2, (size-fb.Dy())/2)
	draw.Draw(out, image.Rectangle{at, at.Add(fb.Size())}, fit, fb.Min, draw.Over)
	return out
}

// iconBackground picks white behind a dark logo and the console's dark
// colour behind a light one, so the logo stays visible on a solid icon.
func iconBackground(img image.Image) color.Color {
	b := img.Bounds()
	step := max(1, max(b.Dx(), b.Dy())/64)
	var sum, n float64
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if c.A < 128 {
				continue
			}
			sum += 0.299*float64(c.R) + 0.587*float64(c.G) + 0.114*float64(c.B)
			n++
		}
	}
	if n > 0 && sum/n > 140 {
		return color.NRGBA{0x16, 0x1c, 0x1f, 0xff}
	}
	return color.White
}

func writeAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
