package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/backup"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Backup and restore from the Settings tab: download a backup, keep
// automatic daily ones on the server, and restore either kind. A restore is
// staged and applied when EdgeTAK restarts (see package backup).

const maxRestore = 64 << 30

var (
	backupName = regexp.MustCompile(`^edgetak-backup-[A-Za-z0-9._-]+\.tar\.gz$`)
	backupBusy sync.Mutex // one backup or restore at a time
)

func (s *Server) backupsDir() string { return filepath.Join(s.DataDir, backup.BackupsDir) }

func (s *Server) backupSource() backup.Source {
	src := backup.Source{DataDir: s.DataDir, ConfigPath: s.ConfigPath, DB: s.Store.Snapshot,
		Version: s.Version, ServerName: s.Cfg.ServerName, NodeID: s.NodeID}
	if s.Missions != nil {
		src.Flush = s.Missions.Flush
	}
	return src
}

func (s *Server) backupFileName(t time.Time) string {
	name := strings.Trim(regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(s.Cfg.ServerName, "-"), "-.")
	if name == "" {
		name = "server"
	}
	return "edgetak-backup-" + name + "-" + t.Local().Format("20060102-150405") + ".tar.gz"
}

type backupFile struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

// savedBackups lists the backups kept on the server, newest first.
func (s *Server) savedBackups() []backupFile {
	entries, _ := os.ReadDir(s.backupsDir())
	out := []backupFile{}
	for _, e := range entries {
		if !backupName.MatchString(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, backupFile{Name: e.Name(), Size: fi.Size(), Created: fi.ModTime()})
		}
	}
	slices.SortFunc(out, func(a, b backupFile) int { return b.Created.Compare(a.Created) })
	return out
}

func (s *Server) backupView() map[string]any {
	view := map[string]any{"settings": s.Store.BackupSettings(), "backups": s.savedBackups(),
		"pending": backup.Pending(s.DataDir), "can_restart": s.Restart != nil}
	if fi, err := os.Stat(filepath.Join(s.DataDir, ".pre-restore")); err == nil {
		view["previous"] = fi.ModTime()
	}
	return view
}

// saveBackup writes a backup into the backups folder and keeps the newest
// keep of them.
func (s *Server) saveBackup(opt backup.Options, keep int) (backupFile, error) {
	if !backupBusy.TryLock() {
		return backupFile{}, errors.New("a backup or restore is already running")
	}
	defer backupBusy.Unlock()
	if err := os.MkdirAll(s.backupsDir(), 0o700); err != nil {
		return backupFile{}, err
	}
	now := time.Now()
	name := s.backupFileName(now)
	tmp, err := os.CreateTemp(s.backupsDir(), ".backup-*")
	if err != nil {
		return backupFile{}, err
	}
	defer os.Remove(tmp.Name())
	err = backup.Write(tmp, s.backupSource(), opt)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return backupFile{}, err
	}
	final := filepath.Join(s.backupsDir(), name)
	if err := os.Rename(tmp.Name(), final); err != nil {
		return backupFile{}, err
	}
	for _, old := range s.savedBackups()[min(keep, len(s.savedBackups())):] {
		os.Remove(filepath.Join(s.backupsDir(), old.Name))
	}
	fi, _ := os.Stat(final)
	size := int64(0)
	if fi != nil {
		size = fi.Size()
	}
	return backupFile{Name: name, Size: size, Created: now}, nil
}

// backupStart makes the automatic daily backups.
func (s *Server) backupStart(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		check := func() {
			set := s.Store.BackupSettings()
			if !set.Auto {
				return
			}
			if list := s.savedBackups(); len(list) > 0 && time.Since(list[0].Created) < 23*time.Hour {
				return
			}
			b, err := s.saveBackup(backup.Options{History: set.History}, set.Keep)
			s.backupResult(err)
			if err != nil {
				s.Log.Error("automatic backup failed", "err", err)
				return
			}
			s.Log.Info("automatic backup saved", "file", b.Name, "bytes", b.Size)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
			check()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				check()
			}
		}
	}()
}

func (s *Server) backupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/backup", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.backupView())
	}))
	mux.HandleFunc("PUT /admin/api/backup/settings", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var b store.BackupSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil || b.Keep < 1 || b.Keep > 90 {
			jsonError(w, http.StatusBadRequest, "keep between 1 and 90 backups")
			return
		}
		if err := s.Store.SetBackupSettings(b); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("backup settings changed", "admin", id.Username, "auto", b.Auto, "keep", b.Keep, "history", b.History)
		writeJSON(w, http.StatusOK, s.backupView())
	}))
	// Download a backup made now.
	mux.HandleFunc("GET /admin/api/backup/download", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if !backupBusy.TryLock() {
			jsonError(w, http.StatusConflict, "a backup or restore is already running")
			return
		}
		defer backupBusy.Unlock()
		q := r.URL.Query()
		opt := backup.Options{History: q.Get("history") == "1", Recordings: q.Get("recordings") == "1", Maps: q.Get("maps") == "1"}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+s.backupFileName(time.Now())+`"`)
		w.Header().Set("Cache-Control", "no-store")
		if err := backup.Write(w, s.backupSource(), opt); err != nil {
			s.Log.Error("backup download failed", "admin", id.Username, "err", err)
			return
		}
		s.Log.Info("backup downloaded", "admin", id.Username, "history", opt.History, "recordings", opt.Recordings)
	}))
	// Save one on the server now.
	mux.HandleFunc("POST /admin/api/backups", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		set := s.Store.BackupSettings()
		b, err := s.saveBackup(backup.Options{History: set.History}, max(set.Keep, len(s.savedBackups())+1))
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("backup saved", "admin", id.Username, "file", b.Name, "bytes", b.Size)
		writeJSON(w, http.StatusOK, s.backupView())
	}))
	mux.HandleFunc("GET /admin/api/backups/{name}", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		name := r.PathValue("name")
		if !backupName.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		http.ServeFile(w, r, filepath.Join(s.backupsDir(), name))
	}))
	mux.HandleFunc("DELETE /admin/api/backups/{name}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		name := r.PathValue("name")
		if !backupName.MatchString(name) || os.Remove(filepath.Join(s.backupsDir(), name)) != nil {
			jsonError(w, http.StatusNotFound, "not found")
			return
		}
		s.Log.Info("backup deleted", "admin", id.Username, "file", name)
		writeJSON(w, http.StatusOK, s.backupView())
	}))
	// Restore an uploaded backup, or one kept on the server.
	mux.HandleFunc("POST /admin/api/backup/restore", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		s.restore(w, http.MaxBytesReader(w, r.Body, maxRestore), "upload", id)
	}))
	mux.HandleFunc("POST /admin/api/backups/{name}/restore", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		name := r.PathValue("name")
		f, err := os.Open(filepath.Join(s.backupsDir(), name))
		if !backupName.MatchString(name) || err != nil {
			jsonError(w, http.StatusNotFound, "not found")
			return
		}
		defer f.Close()
		s.restore(w, f, name, id)
	}))
}

// restore stages a backup and restarts EdgeTAK to apply it.
func (s *Server) restore(w http.ResponseWriter, src io.Reader, from string, id *identity) {
	if !backupBusy.TryLock() {
		jsonError(w, http.StatusConflict, "a backup or restore is already running")
		return
	}
	defer backupBusy.Unlock()
	m, err := backup.Stage(src, s.DataDir)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Log.Warn("backup restore staged; EdgeTAK restarts to apply it", "admin", id.Username, "from", from,
		"backup_of", m.ServerName, "made", m.Created, "version", m.Version)
	writeJSON(w, http.StatusOK, map[string]any{"manifest": m, "restarting": s.Restart != nil})
	if s.Restart != nil {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go func() {
			time.Sleep(time.Second)
			s.Restart()
		}()
	}
}
