// Package backup saves EdgeTAK's data directory and configuration to one
// file and restores it.
//
// A backup is a gzipped tar: edgetak-backup.json (what it is), data/... (the
// data directory) and config/edgetak.json. It holds the CA and its private
// key, so a restored server keeps its identity and devices reconnect without
// re-enrolling; it also holds password hashes and federation tokens, so it
// must be kept as safely as the server itself.
//
// Restoring happens in two steps, because files cannot be swapped under a
// running server: Stage unpacks and checks a backup beside the data, and
// ApplyPending, run before anything opens the data directory, moves the
// current data aside (kept in .pre-restore) and the backup into place.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	format       = 1
	manifestName = "edgetak-backup.json"
	stagingDir   = ".restore-staging"
	previousDir  = ".pre-restore"
	pendingFile  = "restore-pending"

	// BackupsDir holds automatic backups (never itself backed up).
	BackupsDir = "backups"
)

// Manifest describes a backup.
type Manifest struct {
	Format     int       `json:"format"`
	Version    string    `json:"version"` // EdgeTAK version that made it
	ServerName string    `json:"server_name"`
	NodeID     string    `json:"node_id"`
	Created    time.Time `json:"created"`
	History    bool      `json:"history"`    // position history included
	Recordings bool      `json:"recordings"` // video recordings included
	Maps       bool      `json:"maps"`       // offline maps included
}

// Source is the running server to back up.
type Source struct {
	DataDir    string
	ConfigPath string
	// DB returns a consistent copy of db.json, which may be written while
	// the backup runs.
	DB func() ([]byte, error)
	// Flush writes out pending changes (missions) before files are read.
	Flush func()

	Version, ServerName, NodeID string
}

// Options choose the optional, larger parts.
type Options struct {
	History    bool
	Recordings bool
	Maps       bool
}

// skipped reports whether a top-level data entry stays out of backups:
// downloaded programs, generated files, the updater's folder, temporary
// files, and backups and restores themselves.
func skipped(name string, opt Options) bool {
	switch name {
	case "db.json", "mediamtx", "mediamtx.yml", "frpc-bin", "update", "initial-admin-password.txt", BackupsDir, stagingDir, previousDir, pendingFile:
		return true
	case "history":
		return !opt.History
	case "recordings":
		return !opt.Recordings
	case "maps":
		return !opt.Maps
	}
	return strings.HasSuffix(name, ".tmp")
}

func temporary(name string) bool {
	return strings.HasPrefix(name, ".upload-") || strings.HasPrefix(name, "upload-") ||
		strings.HasPrefix(name, "federated-") || strings.HasSuffix(name, ".tmp")
}

// Write streams a backup of src to w.
func Write(w io.Writer, src Source, opt Options) error {
	if src.Flush != nil {
		src.Flush()
	}
	db, err := src.DB()
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := time.Now().UTC()
	add := func(name string, data []byte, mode int64) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: now, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	m, _ := json.MarshalIndent(Manifest{Format: format, Version: src.Version, ServerName: src.ServerName, NodeID: src.NodeID,
		Created: now, History: opt.History, Recordings: opt.Recordings, Maps: opt.Maps}, "", " ")
	if err := add(manifestName, m, 0o644); err != nil {
		return err
	}
	if err := add("data/db.json", db, 0o600); err != nil {
		return err
	}
	if cfg, err := os.ReadFile(src.ConfigPath); err == nil {
		if err := add("config/edgetak.json", cfg, 0o600); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(src.DataDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if skipped(e.Name(), opt) || isConfig(filepath.Join(src.DataDir, e.Name()), src.ConfigPath) {
			continue
		}
		root := filepath.Join(src.DataDir, e.Name())
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil // removed while walking
				}
				return err
			}
			if !d.Type().IsRegular() || temporary(d.Name()) {
				return nil // directories are implied; links and temporary files skipped
			}
			rel, err := filepath.Rel(src.DataDir, p)
			if err != nil {
				return err
			}
			return addFile(tw, p, "data/"+filepath.ToSlash(rel))
		})
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// isConfig reports whether p is the configuration file (which usually lives
// in the data directory, and is saved and restored on its own).
func isConfig(p, configPath string) bool {
	if configPath == "" {
		return false
	}
	a, err1 := filepath.Abs(p)
	b, err2 := filepath.Abs(configPath)
	return err1 == nil && err2 == nil && a == b
}

func addFile(tw *tar.Writer, p, name string) error {
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(fi.Mode().Perm()), Size: fi.Size(), ModTime: fi.ModTime(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	// A file that grows while it is copied (a log) is cut at its size then.
	n, err := io.Copy(tw, io.LimitReader(f, fi.Size()))
	if err == nil && n < fi.Size() {
		err = fmt.Errorf("%s shrank while being backed up", name)
	}
	return err
}

// ErrNotBackup is returned for files that are not EdgeTAK backups.
var ErrNotBackup = errors.New("this is not an EdgeTAK backup")

// Stage unpacks a backup into the data directory's staging folder and
// checks it; ApplyPending puts it in place at the next start.
func Stage(r io.Reader, dataDir string) (*Manifest, error) {
	staging := filepath.Join(dataDir, stagingDir)
	os.RemoveAll(staging)
	os.Remove(filepath.Join(dataDir, pendingFile))
	m, err := unpack(r, staging)
	if err != nil {
		os.RemoveAll(staging)
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dataDir, pendingFile), []byte(m.Created.Format(time.RFC3339)+"\n"), 0o600); err != nil {
		os.RemoveAll(staging)
		return nil, err
	}
	return m, nil
}

// Check reads a backup's manifest and verifies it is complete, without
// unpacking it.
func Check(r io.Reader) (*Manifest, error) {
	return unpack(r, "")
}

// unpack reads a backup, writing its files under dir unless dir is "".
func unpack(r io.Reader, dir string) (*Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, ErrNotBackup
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var m *Manifest
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if m == nil {
				return nil, ErrNotBackup
			}
			return nil, fmt.Errorf("the backup is damaged or incomplete: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue // only plain files are restored
		}
		name := path.Clean(h.Name)
		if name == manifestName {
			m = &Manifest{}
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(m); err != nil || m.Format == 0 {
				return nil, ErrNotBackup
			}
			if m.Format > format {
				return nil, fmt.Errorf("this backup was made by a newer EdgeTAK (%s); update EdgeTAK first", m.Version)
			}
			continue
		}
		if m == nil {
			return nil, ErrNotBackup // the manifest comes first
		}
		if !safeName(name) {
			return nil, fmt.Errorf("the backup contains an unsafe path: %q", h.Name)
		}
		seen[name] = true
		if name == "data/db.json" {
			var probe map[string]json.RawMessage
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			if json.Unmarshal(data, &probe) != nil {
				return nil, errors.New("the backup's database is damaged")
			}
			if dir != "" {
				if err := writeFile(filepath.Join(dir, filepath.FromSlash(name)), strings.NewReader(string(data)), 0o600); err != nil {
					return nil, err
				}
			}
			continue
		}
		if dir == "" {
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return nil, fmt.Errorf("the backup is damaged or incomplete: %w", err)
			}
			continue
		}
		if err := writeFile(filepath.Join(dir, filepath.FromSlash(name)), tr, fs.FileMode(h.Mode).Perm()|0o600); err != nil {
			return nil, fmt.Errorf("the backup is damaged or incomplete: %w", err)
		}
	}
	if m == nil {
		return nil, ErrNotBackup
	}
	for _, need := range []string{"data/db.json", "data/certs/ca.pem", "data/certs/ca-key.pem"} {
		if !seen[need] {
			return nil, fmt.Errorf("the backup is missing %s", strings.TrimPrefix(need, "data/"))
		}
	}
	return m, nil
}

// safeName accepts data/... and config/edgetak.json, nothing outside them.
func safeName(name string) bool {
	if name == "config/edgetak.json" {
		return true
	}
	if !strings.HasPrefix(name, "data/") || strings.Contains(name, "..") || strings.Contains(name, `\`) || strings.Contains(name, ":") {
		return false
	}
	top := strings.SplitN(strings.TrimPrefix(name, "data/"), "/", 2)[0]
	return top != "" && !slices.Contains([]string{BackupsDir, stagingDir, previousDir, pendingFile, "update", "mediamtx", "frpc-bin"}, top)
}

func writeFile(p string, r io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Pending reports whether a staged restore waits for the next start.
func Pending(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, pendingFile))
	return err == nil
}

// ApplyPending puts a staged backup in place. It must run before the data
// directory is opened. The data it replaces is kept in .pre-restore (the
// previous one there is removed). It reports whether a backup was applied.
func ApplyPending(dataDir, configPath string) (bool, error) {
	if !Pending(dataDir) {
		return false, nil
	}
	staging := filepath.Join(dataDir, stagingDir)
	if _, err := os.Stat(filepath.Join(staging, "data", "db.json")); err != nil {
		os.Remove(filepath.Join(dataDir, pendingFile))
		return false, errors.New("a restore was requested but its files are missing; nothing was changed")
	}
	newEntries, err := os.ReadDir(filepath.Join(staging, "data"))
	if err != nil {
		return false, err
	}
	incoming := map[string]bool{}
	for _, e := range newEntries {
		incoming[e.Name()] = true
	}
	prev := filepath.Join(dataDir, previousDir)
	if err := os.RemoveAll(prev); err != nil {
		return false, err
	}
	if err := os.MkdirAll(prev, 0o700); err != nil {
		return false, err
	}
	current, err := os.ReadDir(dataDir)
	if err != nil {
		return false, err
	}
	for _, e := range current {
		name := e.Name()
		switch name {
		case stagingDir, previousDir, pendingFile, BackupsDir, "mediamtx", "frpc-bin", "update":
			continue // never replaced
		case "history", "recordings", "maps":
			if !incoming[name] {
				continue // not in this backup: keep the current ones
			}
		}
		if isConfig(filepath.Join(dataDir, name), configPath) {
			continue // replaced below, or kept when the backup has none
		}
		if err := os.Rename(filepath.Join(dataDir, name), filepath.Join(prev, name)); err != nil {
			return false, fmt.Errorf("moving %s aside: %w", name, err)
		}
	}
	for _, e := range newEntries {
		if err := os.Rename(filepath.Join(staging, "data", e.Name()), filepath.Join(dataDir, e.Name())); err != nil {
			return false, fmt.Errorf("restoring %s: %w (the previous data is in %s)", e.Name(), err, prev)
		}
	}
	if cfg, err := os.ReadFile(filepath.Join(staging, "config", "edgetak.json")); err == nil && configPath != "" {
		if old, err := os.ReadFile(configPath); err == nil {
			os.WriteFile(filepath.Join(prev, "edgetak.json"), old, 0o600)
		}
		if err := os.WriteFile(configPath, cfg, 0o600); err != nil {
			return false, fmt.Errorf("restoring the configuration: %w", err)
		}
	}
	os.RemoveAll(staging)
	os.Remove(filepath.Join(dataDir, pendingFile))
	return true, nil
}
