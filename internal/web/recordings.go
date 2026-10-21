package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Video recording. MediaMTX records streams to <data>/recordings/<stream>/
// in fragmented-MP4 segments named after their start time, and deletes them
// after the kept number of days. The console plays the segment files
// directly (with byte ranges, so seeking works) and exports a whole
// recording as one MP4 through MediaMTX's playback server.

const (
	recordSegment     = 10 * time.Minute
	recordCheckEvery  = 30 * time.Second
	recordPauseAt     = 95.0 // disk % at which recording pauses
	recordResumeAt    = 90.0
	recordingGap      = 5 * time.Second // more than this between segments starts a new recording
	recordingLiveIdle = 10 * time.Second
)

// recFileRE matches MediaMTX's %Y-%m-%d_%H-%M-%S-%f segment names.
var recFileRE = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})-(\d{6})\.(mp4|ts)$`)

type recSegment struct {
	File      string    `json:"file"`
	Start     time.Time `json:"start"`
	Duration  float64   `json:"duration"` // seconds
	Bytes     int64     `json:"bytes"`
	Recording bool      `json:"recording"` // still being written
}

type recSpan struct {
	Start     time.Time    `json:"start"`
	Duration  float64      `json:"duration"`
	Bytes     int64        `json:"bytes"`
	Recording bool         `json:"recording"`
	Segments  []recSegment `json:"segments"`
}

type recStream struct {
	Path  string    `json:"path"`
	Alias string    `json:"alias,omitempty"`
	Bytes int64     `json:"bytes"`
	Spans []recSpan `json:"spans"`
}

func (s *Server) recordingsDir() string {
	dir := filepath.Join(filepath.Dir(s.PackageDir), "recordings")
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// recordingPlan is what MediaMTX should record now: every stream, or the
// listed paths. Nothing while recording is paused for a full disk.
func (s *Server) recordingPlan() (all bool, paths []string) {
	s.video.mu.Lock()
	paused := s.video.recordPaused
	s.video.mu.Unlock()
	if paused {
		return false, nil
	}
	switch s.Store.RecordingSettings().Mode {
	case "all":
		return true, nil
	case "feeds":
		for _, f := range s.Store.VideoFeeds() {
			if f.Record && f.Path != "" && videoPathRE.MatchString(f.Path) && !slices.Contains(paths, f.Path) {
				paths = append(paths, f.Path)
			}
		}
		slices.Sort(paths)
	}
	return false, paths
}

// mediamtxRecordConfig is the recording part of mediamtx.yml: the
// pathDefaults entries and the per-path overrides.
func (s *Server) mediamtxRecordConfig() (defaults, paths string) {
	all, list := s.recordingPlan()
	keep := s.Store.RecordingSettings().KeepDays
	deleteAfter := "0s" // keep forever
	if keep > 0 {
		deleteAfter = strconv.Itoa(keep*24) + "h"
	}
	recPath := filepath.ToSlash(s.recordingsDir()) + "/%path/%Y-%m-%d_%H-%M-%S-%f"
	defaults = fmt.Sprintf(`  record: %t
  recordPath: '%s'
  recordFormat: fmp4
  recordSegmentDuration: %s
  recordDeleteAfter: %s
`, all, strings.ReplaceAll(recPath, "'", "''"), recordSegment, deleteAfter)
	for _, p := range list {
		paths += fmt.Sprintf("  '%s':\n    record: true\n", p) // videoPathRE: no quotes possible
	}
	return defaults, paths
}

// applyRecording rewrites MediaMTX's config after a recording change; it
// reloads the file by itself.
func (s *Server) applyRecording() {
	if !s.Cfg.Video.Enabled {
		return
	}
	if err := s.writeMediaMTXConfig(); err != nil {
		s.Log.Error("write mediamtx config", "err", err)
	}
}

// watchRecordingDisk pauses recording when the disk is nearly full and
// resumes it once space is freed (old recordings expire, or an admin
// deletes some).
func (s *Server) watchRecordingDisk(ctx context.Context) {
	t := time.NewTicker(recordCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.Health == nil {
			continue
		}
		l, ok := s.Health.Latest()
		if !ok || l.DiskTotal == 0 {
			continue // disk figures need Linux
		}
		pct := 100 * float64(l.DiskUsed) / float64(l.DiskTotal)
		s.video.mu.Lock()
		was := s.video.recordPaused
		now := was
		if !was && pct >= recordPauseAt && s.Store.RecordingSettings().Mode != "off" {
			now = true
		} else if was && pct < recordResumeAt {
			now = false
		}
		s.video.recordPaused = now
		s.video.mu.Unlock()
		if now != was {
			if now {
				s.Log.Warn("video recording paused: the disk is nearly full", "disk_percent", int(pct))
			} else {
				s.Log.Info("video recording resumed", "disk_percent", int(pct))
			}
			s.applyRecording()
		}
	}
}

// parseRecFile reads a segment's start time from its name (MediaMTX writes
// local time, as EdgeTAK and MediaMTX share the machine's time zone).
func parseRecFile(name string) (time.Time, bool) {
	m := recFileRE.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02_15-04-05", m[1], time.Local)
	if err != nil {
		return time.Time{}, false
	}
	us, _ := strconv.Atoi(m[2])
	return t.Add(time.Duration(us) * time.Microsecond), true
}

// listRecordings scans the recordings folder. A segment ends at its last
// write; segments that follow each other without a gap form one recording.
func (s *Server) listRecordings() ([]recStream, int64) {
	root := s.recordingsDir()
	byPath := map[string][]recSegment{}
	now := time.Now()
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		start, ok := parseRecFile(d.Name())
		if !ok {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return nil
		}
		stream := filepath.ToSlash(rel)
		if !videoPathRE.MatchString(stream) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		seg := recSegment{File: d.Name(), Start: start, Bytes: fi.Size(), Recording: now.Sub(fi.ModTime()) < recordingLiveIdle}
		if end := fi.ModTime(); end.After(start) {
			seg.Duration = end.Sub(start).Seconds()
		}
		byPath[stream] = append(byPath[stream], seg)
		return nil
	})
	aliases := map[string]string{}
	for _, f := range s.Store.VideoFeeds() {
		if f.Path != "" {
			aliases[f.Path] = f.Alias
		}
	}
	var total int64
	out := []recStream{}
	for path, segs := range byPath {
		slices.SortFunc(segs, func(a, b recSegment) int { return a.Start.Compare(b.Start) })
		st := recStream{Path: path, Alias: aliases[path]}
		for _, seg := range segs {
			st.Bytes += seg.Bytes
			n := len(st.Spans)
			if n > 0 {
				last := &st.Spans[n-1]
				end := last.Start.Add(time.Duration(last.Duration * float64(time.Second)))
				if seg.Start.Sub(end) <= recordingGap {
					last.Segments = append(last.Segments, seg)
					last.Duration = seg.Start.Sub(last.Start).Seconds() + seg.Duration
					last.Bytes += seg.Bytes
					last.Recording = last.Recording || seg.Recording
					continue
				}
			}
			st.Spans = append(st.Spans, recSpan{Start: seg.Start, Duration: seg.Duration, Bytes: seg.Bytes, Recording: seg.Recording, Segments: []recSegment{seg}})
		}
		slices.Reverse(st.Spans) // newest first
		total += st.Bytes
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b recStream) int {
		return b.Spans[0].Start.Add(time.Duration(b.Spans[0].Duration * float64(time.Second))).Compare(a.Spans[0].Start.Add(time.Duration(a.Spans[0].Duration * float64(time.Second))))
	})
	return out, total
}

// segmentFile resolves a stream path and segment name to a file inside the
// recordings folder.
func (s *Server) segmentFile(stream, file string) (string, error) {
	if !videoPathRE.MatchString(stream) || !recFileRE.MatchString(file) {
		return "", errors.New("invalid recording")
	}
	root := s.recordingsDir()
	p := filepath.Join(root, filepath.FromSlash(stream), file)
	if rel, err := filepath.Rel(root, p); err != nil || strings.HasPrefix(rel, "..") {
		return "", errors.New("invalid recording")
	}
	return p, nil
}

func (s *Server) recordingStatus() map[string]any {
	streams, total := s.listRecordings()
	s.video.mu.Lock()
	paused := s.video.recordPaused
	s.video.mu.Unlock()
	out := map[string]any{"settings": s.Store.RecordingSettings(), "paused": paused, "enabled": s.Cfg.Video.Enabled,
		"streams": streams, "bytes": total, "segment_minutes": int(recordSegment.Minutes())}
	if s.Health != nil {
		if l, ok := s.Health.Latest(); ok && l.DiskTotal > 0 {
			out["disk_used"], out["disk_total"] = l.DiskUsed, l.DiskTotal
		}
	}
	return out
}

func (s *Server) recordingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/video/recordings", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.recordingStatus())
	}))
	mux.HandleFunc("PUT /admin/api/video/recording", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req store.RecordingSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if req.Mode != "off" && req.Mode != "feeds" && req.Mode != "all" {
			jsonError(w, http.StatusBadRequest, "mode must be off, feeds or all")
			return
		}
		if req.KeepDays < 0 || req.KeepDays > 3650 {
			jsonError(w, http.StatusBadRequest, "keep recordings 0 (forever) to 3650 days")
			return
		}
		if err := s.Store.SetRecordingSettings(req); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.applyRecording()
		s.Log.Info("video recording settings changed", "admin", id.Username, "mode", req.Mode, "keep_days", req.KeepDays)
		writeJSON(w, http.StatusOK, s.recordingStatus())
	}))
	// ?path=STREAM&file=SEGMENT[&download=1]: one segment, with byte ranges.
	mux.HandleFunc("GET /admin/video/recordings/file", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		q := r.URL.Query()
		p, err := s.segmentFile(q.Get("path"), q.Get("file"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, err := os.Open(p)
		if err != nil {
			http.Error(w, "recording not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if q.Get("download") != "" {
			w.Header().Set("Content-Disposition", `attachment; filename="`+sanitize(strings.ReplaceAll(q.Get("path"), "/", "_"))+"_"+q.Get("file")+`"`)
		}
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "", fi.ModTime(), f)
	}))
	// ?path=STREAM&start=RFC3339&duration=SECONDS: a whole recording as one MP4.
	mux.HandleFunc("GET /admin/video/recordings/export", s.admin(s.exportRecording))
	// ?path=STREAM[&start=RFC3339]: delete one recording, or all of a stream's.
	mux.HandleFunc("DELETE /admin/api/video/recordings", s.admin(s.deleteRecordings))
}

func (s *Server) exportRecording(w http.ResponseWriter, r *http.Request, id *identity) {
	q := r.URL.Query()
	stream := q.Get("path")
	start, err := time.Parse(time.RFC3339Nano, q.Get("start"))
	dur, derr := strconv.ParseFloat(q.Get("duration"), 64)
	if !videoPathRE.MatchString(stream) || err != nil || derr != nil || dur <= 0 {
		http.Error(w, "invalid recording", http.StatusBadRequest)
		return
	}
	if !s.Cfg.Video.Enabled {
		http.Error(w, "video is not enabled", http.StatusNotFound)
		return
	}
	target := &url.URL{Scheme: "http", Host: s.videoCfg().PlaybackAddr}
	name := sanitize(strings.ReplaceAll(stream, "/", "_")) + "_" + start.Local().Format("2006-01-02_15-04-05") + ".mp4"
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/get"
			v := url.Values{"path": {stream}, "start": {start.Format(time.RFC3339Nano)}, "duration": {strconv.FormatFloat(dur, 'f', 3, 64)}, "format": {"mp4"}}
			pr.Out.URL.RawQuery = v.Encode()
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Range")
			pr.Out.SetBasicAuth(internalVideoUser, s.internalVideoPass())
		},
		ModifyResponse: func(res *http.Response) error {
			if res.StatusCode == http.StatusOK {
				res.Header.Set("Content-Disposition", `attachment; filename="`+name+`"`)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "the media server is not reachable; download the recording's segments instead", http.StatusBadGateway)
		},
	}
	s.Log.Info("video recording exported", "admin", id.Username, "path", stream, "start", start, "seconds", int(dur))
	proxy.ServeHTTP(w, r)
}

func (s *Server) deleteRecordings(w http.ResponseWriter, r *http.Request, id *identity) {
	q := r.URL.Query()
	stream := q.Get("path")
	if !videoPathRE.MatchString(stream) {
		jsonError(w, http.StatusBadRequest, "invalid stream")
		return
	}
	var only *time.Time
	if v := q.Get("start"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid start")
			return
		}
		only = &t
	}
	streams, _ := s.listRecordings()
	deleted, skipped := 0, 0
	for _, st := range streams {
		if st.Path != stream {
			continue
		}
		for _, sp := range st.Spans {
			if only != nil && !sp.Start.Equal(*only) {
				continue
			}
			for _, seg := range sp.Segments {
				if seg.Recording {
					skipped++ // MediaMTX is still writing it
					continue
				}
				p, err := s.segmentFile(stream, seg.File)
				if err == nil {
					err = os.Remove(p)
				}
				if err != nil {
					skipped++
					continue
				}
				deleted++
			}
		}
	}
	// Remove the stream's folders once empty.
	root := s.recordingsDir()
	for dir := filepath.Join(root, filepath.FromSlash(stream)); dir != root && strings.HasPrefix(dir, root); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
	s.Log.Info("video recordings deleted", "admin", id.Username, "path", stream, "segments", deleted, "skipped", skipped)
	writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted, "skipped": skipped})
}
