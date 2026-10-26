package web

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func testLogo(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestBrandingFiles(t *testing.T) {
	files, err := brandingFiles(testLogo(t, 1200, 300, color.Black))
	if err != nil {
		t.Fatal(err)
	}
	logo, err := png.Decode(bytes.NewReader(files["logo.png"]))
	if err != nil {
		t.Fatal(err)
	}
	if b := logo.Bounds(); b.Dx() != 512 || b.Dy() != 128 {
		t.Errorf("logo is %v, want 512x128 (scaled down, shape kept)", b.Size())
	}
	for name, ic := range brandIcons {
		img, err := png.Decode(bytes.NewReader(files[name]))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if b := img.Bounds(); b.Dx() != ic.size || b.Dy() != ic.size {
			t.Errorf("%s is %v, want %d square", name, b.Size(), ic.size)
		}
		// A black logo gets a white background where the icon is solid,
		// and stays transparent elsewhere.
		_, _, _, a := img.At(0, 0).RGBA()
		r, _, _, _ := img.At(0, 0).RGBA()
		if ic.solid && (a != 0xffff || r != 0xffff) {
			t.Errorf("%s corner = %v, want white", name, img.At(0, 0))
		}
		if !ic.solid && a != 0 {
			t.Errorf("%s corner = %v, want transparent", name, img.At(0, 0))
		}
	}
}

func TestBrandingRejects(t *testing.T) {
	if _, err := brandingFiles([]byte("<svg onload=alert(1)>")); err == nil || !strings.Contains(err.Error(), "PNG, JPEG or WebP") {
		t.Errorf("SVG: err = %v", err)
	}
	// A tiny PNG whose header claims 100000 x 100000 pixels.
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 100000)
	binary.BigEndian.PutUint32(ihdr[4:], 100000)
	ihdr[8], ihdr[9] = 8, 6
	binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	if _, err := brandingFiles(b.Bytes()); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("huge PNG: err = %v", err)
	}
}

func TestLogoServed(t *testing.T) {
	data := t.TempDir()
	s := &Server{PackageDir: filepath.Join(data, "packages"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	mux := http.NewServeMux()
	s.brandingRoutes(mux)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	builtin := get("/admin/icons/favicon-32.png")
	if builtin.Code != http.StatusOK || get("/admin/logo.png").Code != http.StatusNotFound || s.logoVersion() != "" {
		t.Fatalf("before upload: icon %d, logo %d, version %q", builtin.Code, get("/admin/logo.png").Code, s.logoVersion())
	}

	w := httptest.NewRecorder()
	s.setLogo(w, httptest.NewRequest("PUT", "/admin/api/logo", bytes.NewReader(testLogo(t, 64, 64, color.White))), &identity{Username: "admin"})
	if w.Code != http.StatusOK || s.logoVersion() == "" {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if r := get("/admin/logo.png"); r.Code != http.StatusOK || r.Header().Get("Content-Type") != "image/png" {
		t.Errorf("logo: %d %s", r.Code, r.Header().Get("Content-Type"))
	}
	if r := get("/admin/icons/favicon-32.png"); r.Code != http.StatusOK || bytes.Equal(r.Body.Bytes(), builtin.Body.Bytes()) {
		t.Error("favicon is not made from the logo")
	}
	// Icons a logo doesn't replace still come from the console's files.
	if r := get("/admin/icons/nope.png"); r.Code != http.StatusNotFound {
		t.Errorf("unknown icon: %d", r.Code)
	}
}
