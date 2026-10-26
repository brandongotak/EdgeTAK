package extension

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	// MaxUpload is the largest zip accepted.
	MaxUpload = 200 << 20
	// maxUnpacked limits what a zip may unpack to, against zip bombs.
	maxUnpacked = 500 << 20
	maxFiles    = 20000
)

// readZip checks an uploaded zip and returns its manifest and the prefix
// its files are under: plugin.json may be at the root or in one top
// folder (a zipped folder).
func readZip(zr *zip.Reader) (Manifest, string, error) {
	var prefix string
	var mf *zip.File
	for _, f := range zr.File {
		if f.Name == ManifestFile {
			mf, prefix = f, ""
			break
		}
		if dir, base := path.Split(f.Name); base == ManifestFile && strings.Count(dir, "/") == 1 && mf == nil {
			mf, prefix = f, dir
		}
	}
	if mf == nil {
		return Manifest{}, "", errors.New("the zip has no " + ManifestFile + " (at its root or in one folder)")
	}
	rc, err := mf.Open()
	if err != nil {
		return Manifest{}, "", err
	}
	b, err := io.ReadAll(io.LimitReader(rc, 64<<10))
	rc.Close()
	if err != nil {
		return Manifest{}, "", err
	}
	m, err := ParseManifest(b)
	return m, prefix, err
}

// unpack extracts the files under prefix into dir.
func unpack(zr *zip.Reader, prefix, dir string, m Manifest) error {
	if len(zr.File) > maxFiles {
		return fmt.Errorf("the zip has more than %d files", maxFiles)
	}
	var total int64
	runFiles := map[string]bool{}
	for _, c := range m.Run {
		runFiles[path.Clean(filepath.ToSlash(c[0]))] = true
	}
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, prefix) {
			continue
		}
		name := strings.TrimPrefix(f.Name, prefix)
		if name == "" {
			continue
		}
		if !safeName(name) {
			return fmt.Errorf("the zip has an unsafe file name: %q", f.Name)
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("the zip has a symbolic link: %q", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		// Executables keep their mode; a zip made on Windows has none, so
		// the files run names are made executable.
		mode := os.FileMode(0o644)
		if f.Mode()&0o111 != 0 || runFiles[path.Clean(name)] {
			mode = 0o755
		}
		n, err := writeEntry(f, p, mode, maxUnpacked-total)
		total += n
		if err != nil {
			return err
		}
	}
	return nil
}

func writeEntry(f *zip.File, p string, mode os.FileMode, room int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	out, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(rc, room+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > room {
		err = fmt.Errorf("the extension unpacks to more than %d MB", maxUnpacked>>20)
	}
	return n, err
}

// safeName accepts relative slash paths that stay inside the folder.
func safeName(name string) bool {
	if strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
		return false
	}
	for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
