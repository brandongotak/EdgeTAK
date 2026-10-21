package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/config"
)

func TestApplyEnv(t *testing.T) {
	data := t.TempDir()
	seed := filepath.Join(t.TempDir(), "mediamtx")
	os.WriteFile(seed, []byte("binary"), 0o755)
	os.WriteFile(seed+".version", []byte("v1.21.1\n"), 0o644)
	t.Setenv("EDGETAK_PUBLIC_HOST", "tak.example.com")
	t.Setenv("EDGETAK_LETSENCRYPT", "1")
	t.Setenv("EDGETAK_NO_PLAINTEXT", "true")
	t.Setenv("EDGETAK_VIDEO", "1")
	t.Setenv("EDGETAK_MEDIAMTX_SEED", seed)

	cfg := config.Default()
	if err := applyEnv(&cfg, data); err != nil {
		t.Fatal(err)
	}
	if cfg.PublicHost != "tak.example.com" || cfg.PublicCert.Mode != "letsencrypt" || cfg.PublicCert.Email != "" ||
		cfg.TCPAddr != "" || cfg.HTTPAddr != "" || !cfg.Video.Enabled {
		t.Fatalf("cfg = %+v", cfg)
	}
	// The bundled MediaMTX is copied into the data volume once.
	if b, err := os.ReadFile(cfg.Video.MediaMTXBinary); err != nil || string(b) != "binary" {
		t.Fatalf("seeded binary: %q %v", b, err)
	}
	if v, _ := os.ReadFile(cfg.Video.MediaMTXBinary + ".version"); string(v) != "v1.21.1\n" {
		t.Fatalf("seeded version = %q", v)
	}
	os.WriteFile(cfg.Video.MediaMTXBinary, []byte("updated from the console"), 0o755)
	cfg2 := config.Default()
	if err := applyEnv(&cfg2, data); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfg2.Video.MediaMTXBinary); string(b) != "updated from the console" {
		t.Fatal("a console-updated MediaMTX was overwritten by the image's copy")
	}

	// Let's Encrypt with an email, and refusing an IP address.
	t.Setenv("EDGETAK_LETSENCRYPT", "ops@example.com")
	cfg3 := config.Default()
	applyEnv(&cfg3, data)
	if cfg3.PublicCert.Email != "ops@example.com" {
		t.Fatalf("email = %q", cfg3.PublicCert.Email)
	}
	t.Setenv("EDGETAK_PUBLIC_HOST", "203.0.113.5")
	if err := applyEnv(new(config.Config), data); err == nil {
		t.Fatal("Let's Encrypt accepted an IP address")
	}
	// Off values leave things alone.
	t.Setenv("EDGETAK_PUBLIC_HOST", "")
	t.Setenv("EDGETAK_LETSENCRYPT", "0")
	t.Setenv("EDGETAK_NO_PLAINTEXT", "")
	t.Setenv("EDGETAK_VIDEO", "no")
	cfg4 := config.Default()
	if err := applyEnv(&cfg4, data); err != nil || cfg4.PublicCert.Mode != "" || cfg4.TCPAddr == "" || cfg4.Video.Enabled {
		t.Fatalf("off values changed the config: %+v %v", cfg4, err)
	}
}
