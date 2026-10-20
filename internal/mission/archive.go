package mission

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Archive packages a mission's files and map items as a data package that
// ATAK can import.
func (s *Service) Archive(name string, c Caller) ([]byte, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, _, err := s.require(name, c, PermRead)
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, r io.Reader) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			return err
		}
		_, err = io.Copy(w, r)
		return err
	}
	var entries []string
	for _, ci := range m.Contents {
		p, ok := s.store.Package(ci.Hash)
		if !ok {
			continue
		}
		f, err := os.Open(filepath.Join(s.PackageDir, p.Hash))
		if err != nil {
			continue
		}
		entry := p.Hash + "/" + filepath.Base(p.Name)
		err = add(entry, f)
		f.Close()
		if err != nil {
			return nil, "", err
		}
		entries = append(entries, entry)
	}
	for _, u := range m.UIDs {
		x, ok := m.CoT[u.UID]
		if !ok {
			continue
		}
		entry := safeName(u.UID) + "/" + safeName(u.UID) + ".cot"
		if err := add(entry, strings.NewReader(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`+stripMarti(x))); err != nil {
			return nil, "", err
		}
		entries = append(entries, entry)
	}
	var man strings.Builder
	man.WriteString(`<MissionPackageManifest version="2"><Configuration>`)
	man.WriteString(`<Parameter name="uid" value="` + m.GUID + `"/>`)
	man.WriteString(`<Parameter name="name" value="` + xmlEsc(m.Name) + `"/>`)
	man.WriteString(`<Parameter name="mission_name" value="` + xmlEsc(m.Name) + `"/>`)
	man.WriteString(`<Parameter name="mission_guid" value="` + m.GUID + `"/>`)
	man.WriteString(`</Configuration><Contents>`)
	for _, e := range entries {
		man.WriteString(`<Content ignore="false" zipEntry="` + xmlEsc(e) + `"/>`)
	}
	man.WriteString(`</Contents></MissionPackageManifest>`)
	if err := add("MANIFEST/manifest.xml", strings.NewReader(man.String())); err != nil {
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), safeName(m.Name) + ".zip", nil
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' {
			return r
		}
		return '_'
	}, s)
}
