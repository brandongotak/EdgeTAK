package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/config"
	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Video.
//
// Feeds are listed for ATAK/WinTAK's Video Player at /Marti/vcm (the
// official server's XML) and /Marti/api/video (its v2 JSON). Feeds with a
// position are sent to devices as sensor markers with a <__video> link.
//
// With the MediaMTX companion (video.enabled), streams published to it
// (RTSP/RTMP/SRT) become feeds automatically. MediaMTX asks EdgeTAK to
// authorize every publish and read: publishers use their EdgeTAK username
// and password; viewers get URLs carrying a per-user read token, so their
// password never appears in a URL and deleting the user revokes it. The
// admin console plays streams through EdgeTAK's HLS relay.

const (
	internalVideoUser = "edgetak-internal"
	videoPollEvery    = 5 * time.Second
	videoMarkerStale  = 10 * time.Minute
	videoMarkerEvery  = 5 * time.Minute
	authCacheFor      = time.Minute
)

var videoPathRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,63}(/[A-Za-z0-9._~-]{1,64}){0,3}$`)

type videoState struct {
	mu         sync.Mutex
	secret     string
	live       map[string]liveStream // MediaMTX path -> stream
	publishers map[string]publisher  // path -> who published it
	authCache  map[string]time.Time  // hash(user, pass) -> expiry
	apiErr     string                // last MediaMTX API error ("" = reachable)
	bytesIn    uint64                // bytes received/sent by MediaMTX's current streams
	bytesOut   uint64
	polled     time.Time
	warned     bool            // "unreachable" was logged
	markers    map[string]bool // feed UIDs with a marker on devices
}

type liveStream struct {
	Name    string    `json:"name"`
	Tracks  []string  `json:"tracks"`
	Readers int       `json:"readers"`
	Since   time.Time `json:"since"`
	Source  string    `json:"source"`
}

type publisher struct {
	User   string
	Groups []string // the publisher's OUT groups: who may see the stream
}

// feedView is a feed as one viewer sees it.
type feedView struct {
	UID         string   `json:"uid"`
	Alias       string   `json:"alias"`
	URL         string   `json:"url"` // with the viewer's credentials for server streams
	Protocol    string   `json:"protocol"`
	Address     string   `json:"-"`
	Port        int      `json:"-"`
	Path        string   `json:"path,omitempty"` // MediaMTX path for server streams
	Hosted      bool     `json:"hosted"`
	Live        bool     `json:"live"`
	Saved       bool     `json:"saved"`
	Groups      []string `json:"groups"`
	Lat         *float64 `json:"lat,omitempty"`
	Lon         *float64 `json:"lon,omitempty"`
	Readers     int      `json:"readers"`
	Tracks      []string `json:"tracks,omitempty"`
	Publisher   string   `json:"publisher,omitempty"`
	OpenPublish bool     `json:"open_publish,omitempty"`
}

func (s *Server) videoCfg() config.Video { return s.Cfg.Video.Normalized() }

// videoStart wires up video once the server starts.
func (s *Server) videoStart(ctx context.Context) {
	secret, err := s.Store.VideoSecret()
	if err != nil {
		s.Log.Error("video secret", "err", err)
	}
	s.video.mu.Lock()
	s.video.secret = secret
	s.video.live = map[string]liveStream{}
	s.video.publishers = map[string]publisher{}
	s.video.authCache = map[string]time.Time{}
	s.video.markers = map[string]bool{}
	s.video.mu.Unlock()

	s.Router.OnJoin(func(c *router.Client) { s.sendVideoMarkers(c) })
	go func() {
		t := time.NewTicker(videoMarkerEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshVideoMarkers()
			}
		}
	}()

	if !s.Cfg.Video.Enabled {
		return
	}
	if err := s.writeMediaMTXConfig(); err != nil {
		s.Log.Error("write mediamtx config", "err", err)
	}
	go s.pollMediaMTX(ctx)
}

// ---- tokens and internal credentials ----

func (s *Server) videoMAC(msg string) []byte {
	s.video.mu.Lock()
	secret := s.video.secret
	s.video.mu.Unlock()
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// readToken lets username watch server streams; it is used as the password
// in the stream URLs that user's devices receive.
func (s *Server) readToken(username string) string {
	return "rt-" + base64.RawURLEncoding.EncodeToString(s.videoMAC("read:" + username)[:18])
}

func (s *Server) internalVideoPass() string {
	return hex.EncodeToString(s.videoMAC("internal")[:20])
}

// hlsCDNSecret identifies EdgeTAK's HLS relay to MediaMTX.
func (s *Server) hlsCDNSecret() string {
	return hex.EncodeToString(s.videoMAC("hls-cdn")[:20])
}

// ---- feeds ----

// viewerName is the identity stream URLs are issued for.
func viewerName(id *identity) string {
	if id.Anonymous {
		return "anonymous"
	}
	return id.Username
}

// visibleFeeds lists the saved feeds and live server streams id may see.
func (s *Server) visibleFeeds(id *identity) []feedView {
	s.video.mu.Lock()
	live := make(map[string]liveStream, len(s.video.live))
	for k, v := range s.video.live {
		live[k] = v
	}
	pubs := make(map[string]publisher, len(s.video.publishers))
	for k, v := range s.video.publishers {
		pubs[k] = v
	}
	s.video.mu.Unlock()

	out := []feedView{}
	saved := map[string]bool{}
	for _, f := range s.Store.VideoFeeds() {
		if f.Path != "" {
			saved[f.Path] = true
		}
		if !id.canSee(feedGroups(f.Groups, s.Cfg.AnonymousGroup)) {
			continue
		}
		v := feedView{UID: f.UID, Alias: f.Alias, Saved: true, Groups: f.Groups, Lat: f.Lat, Lon: f.Lon, Path: f.Path, OpenPublish: f.OpenPublish}
		if f.Path != "" {
			s.hostedURL(&v, id)
			ls, ok := live[f.Path]
			v.Live, v.Readers, v.Tracks = ok, ls.Readers, ls.Tracks
			v.Publisher = pubs[f.Path].User
		} else {
			v.URL = f.URL
			splitFeedURL(&v)
		}
		out = append(out, v)
	}
	// Live streams nobody has saved yet, visible to the publisher's groups.
	for name, ls := range live {
		if saved[name] {
			continue
		}
		p := pubs[name]
		groups := p.Groups
		if !id.Admin && !(p.User != "" && p.User == id.Username) && (len(groups) == 0 || !id.canSee(groups)) {
			continue
		}
		v := feedView{UID: "edgetak-live-" + name, Alias: name, Path: name, Live: true, Readers: ls.Readers, Tracks: ls.Tracks,
			Groups: groups, Publisher: p.User}
		s.hostedURL(&v, id)
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b feedView) int { return strings.Compare(strings.ToLower(a.Alias), strings.ToLower(b.Alias)) })
	return out
}

func feedGroups(groups []string, anon string) []string {
	if len(groups) == 0 {
		return []string{anon}
	}
	return groups
}

// hostedURL fills the RTSP URL of a server stream for viewer id.
func (s *Server) hostedURL(v *feedView, id *identity) {
	vc := s.videoCfg()
	user := viewerName(id)
	v.Hosted = true
	v.Protocol = "rtsp"
	v.Address = url.UserPassword(user, s.readToken(user)).String() + "@" + s.Cfg.PublicHost
	v.Port = vc.RTSPPort
	v.URL = "rtsp://" + v.Address + ":" + strconv.Itoa(vc.RTSPPort) + "/" + v.Path
}

// splitFeedURL fills protocol/address/port/path from an external URL the
// way ATAK's ConnectionEntry splits one (address = [userinfo@]host).
func splitFeedURL(v *feedView) {
	u, err := url.Parse(v.URL)
	if err != nil || u.Scheme == "" {
		return
	}
	v.Protocol = strings.ToLower(u.Scheme)
	v.Address = u.Hostname()
	if u.User != nil {
		v.Address = u.User.String() + "@" + v.Address
	}
	v.Port, _ = strconv.Atoi(u.Port())
	if v.Port == 0 {
		v.Port = map[string]int{"rtsp": 554, "rtmp": 1935, "rtmps": 443, "http": 80, "https": 443}[v.Protocol]
	}
	v.Path = u.EscapedPath()
	if u.RawQuery != "" {
		v.Path += "?" + u.RawQuery
	}
}

// ---- client APIs ----

type vcmFeed struct {
	XMLName           xml.Name `xml:"feed"`
	Active            bool     `xml:"active"`
	UID               string   `xml:"uid"`
	Alias             string   `xml:"alias"`
	Type              string   `xml:"type"`
	Protocol          string   `xml:"protocol"`
	Address           string   `xml:"address"`
	Port              string   `xml:"port"`
	Path              string   `xml:"path"`
	RoverPort         string   `xml:"roverPort"`
	IgnoreEmbeddedKLV string   `xml:"ignoreEmbeddedKLV"`
	PreferredMac      string   `xml:"preferredMacAddress"`
	Buffer            string   `xml:"buffer"`
	Timeout           string   `xml:"timeout"`
	RTSPReliable      string   `xml:"rtspReliable"`
	Latitude          string   `xml:"latitude,omitempty"`
	Longitude         string   `xml:"longitude,omitempty"`
}

func (s *Server) videoRoutes(mux *http.ServeMux) {
	// Legacy feed list, used by ATAK's and WinTAK's Video Player.
	mux.HandleFunc("GET /Marti/vcm", s.authed(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var b bytes.Buffer
		b.WriteString(xml.Header + "<videoConnections>")
		enc := xml.NewEncoder(&b)
		for _, f := range s.visibleFeeds(id) {
			if f.Protocol == "" {
				continue
			}
			vf := vcmFeed{Active: true, UID: f.UID, Alias: f.Alias, Type: "VIDEO", Protocol: f.Protocol, Address: f.Address,
				Port: strconv.Itoa(f.Port), Path: f.vcmPath(), RoverPort: "-1", IgnoreEmbeddedKLV: "false", Buffer: "-1", Timeout: "5000",
				RTSPReliable: "1"}
			if f.Lat != nil && f.Lon != nil {
				vf.Latitude, vf.Longitude = fmtCoord(*f.Lat), fmtCoord(*f.Lon)
			}
			enc.Encode(vf)
		}
		enc.Flush()
		b.WriteString("</videoConnections>")
		w.Header().Set("Content-Type", "application/xml")
		w.Write(b.Bytes())
	}))
	// ATAK can share its own video aliases to the server.
	mux.HandleFunc("POST /Marti/vcm", s.named(s.uploadVCM))
	// v2 feed list (TAK Server 4.x).
	mux.HandleFunc("GET /Marti/api/video", s.authed(func(w http.ResponseWriter, r *http.Request, id *identity) {
		type feedV2 struct {
			Active            bool   `json:"active"`
			UUID              string `json:"uuid"`
			Alias             string `json:"alias"`
			URL               string `json:"url"`
			Order             *int   `json:"order"`
			MacAddress        string `json:"macAddress"`
			RoverPort         string `json:"roverPort"`
			IgnoreEmbeddedKLV string `json:"ignoreEmbeddedKLV"`
			Source            string `json:"source"`
			NetworkTimeout    string `json:"networkTimeout"`
			BufferTime        string `json:"bufferTime"`
			RTSPReliable      string `json:"rtspReliable"`
			Thumbnail         string `json:"thumbnail"`
			Classification    string `json:"classification"`
			Latitude          string `json:"latitude"`
			Longitude         string `json:"longitude"`
		}
		type conn struct {
			UUID           string   `json:"uuid"`
			Active         bool     `json:"active"`
			Alias          string   `json:"alias"`
			Thumbnail      string   `json:"thumbnail"`
			Classification string   `json:"classification"`
			Feeds          []feedV2 `json:"feeds"`
		}
		out := []conn{}
		for _, f := range s.visibleFeeds(id) {
			fv := feedV2{Active: true, UUID: f.UID, Alias: f.Alias, URL: f.URL, RoverPort: "-1", IgnoreEmbeddedKLV: "false",
				NetworkTimeout: "5000", BufferTime: "-1", RTSPReliable: "1"}
			if f.Lat != nil && f.Lon != nil {
				fv.Latitude, fv.Longitude = fmtCoord(*f.Lat), fmtCoord(*f.Lon)
			}
			out = append(out, conn{UUID: f.UID, Active: true, Alias: f.Alias, Feeds: []feedV2{fv}})
		}
		writeJSON(w, http.StatusOK, map[string]any{"videoConnections": out})
	}))

	// Admin console.
	mux.HandleFunc("GET /admin/api/video", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		writeJSON(w, http.StatusOK, s.videoStatus(id))
	}))
	mux.HandleFunc("PUT /admin/api/video/settings", s.admin(s.setVideoSettings))
	mux.HandleFunc("POST /admin/api/video/feeds", s.admin(s.saveFeed))
	mux.HandleFunc("PUT /admin/api/video/feeds/{uid}", s.admin(s.saveFeed))
	mux.HandleFunc("DELETE /admin/api/video/feeds/{uid}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		uid := r.PathValue("uid")
		if err := s.Store.DeleteVideoFeed(uid); err != nil {
			jsonError(w, http.StatusNotFound, "feed not found")
			return
		}
		s.removeVideoMarker(uid)
		s.Log.Info("video feed deleted", "admin", id.Username, "uid", uid)
		w.WriteHeader(http.StatusNoContent)
	}))
	// HLS relay for the console player: /admin/video/hls/<path>/index.m3u8
	mux.HandleFunc("GET /admin/video/hls/{rest...}", s.admin(s.hlsRelay))

	// Managing the MediaMTX process: status, update, rollback, restart.
	mux.HandleFunc("GET /admin/api/video/mediamtx", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.mediamtxStatus(r.Context()))
	}))
	mux.HandleFunc("POST /admin/api/video/mediamtx/check", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if !s.managedMediaMTX(w) {
			return
		}
		if _, err := s.MediaMTX.CheckLatest(r.Context()); err != nil {
			jsonError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.mediamtxStatus(r.Context()))
	}))
	mux.HandleFunc("POST /admin/api/video/mediamtx/update", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if !s.managedMediaMTX(w) {
			return
		}
		var req struct {
			Version string `json:"version"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req)
		if req.Version == "" {
			rel, err := s.MediaMTX.CheckLatest(r.Context())
			if err != nil {
				jsonError(w, http.StatusBadGateway, err.Error())
				return
			}
			req.Version = rel.Version
		}
		if err := s.MediaMTX.StartUpdate(req.Version); err != nil {
			jsonError(w, http.StatusConflict, err.Error())
			return
		}
		s.Log.Info("MediaMTX update started", "admin", id.Username, "version", req.Version)
		writeJSON(w, http.StatusAccepted, s.mediamtxStatus(r.Context()))
	}))
	mux.HandleFunc("POST /admin/api/video/mediamtx/rollback", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if !s.managedMediaMTX(w) {
			return
		}
		res, err := s.MediaMTX.Rollback(r.Context())
		if err != nil {
			jsonError(w, http.StatusConflict, err.Error())
			return
		}
		s.Log.Info("MediaMTX rolled back", "admin", id.Username, "result", res)
		writeJSON(w, http.StatusOK, s.mediamtxStatus(r.Context()))
	}))
	mux.HandleFunc("POST /admin/api/video/mediamtx/restart", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if !s.managedMediaMTX(w) {
			return
		}
		s.MediaMTX.Restart()
		s.Log.Info("MediaMTX restarted", "admin", id.Username)
		writeJSON(w, http.StatusOK, s.mediamtxStatus(r.Context()))
	}))
}

func (s *Server) managedMediaMTX(w http.ResponseWriter) bool {
	if s.MediaMTX == nil {
		jsonError(w, http.StatusNotFound, "MediaMTX is not managed by EdgeTAK on this server")
		return false
	}
	return true
}

func (s *Server) mediamtxStatus(ctx context.Context) any {
	if s.MediaMTX == nil {
		return map[string]any{"managed": false}
	}
	return s.MediaMTX.Status(ctx)
}

// VideoBytes returns the bytes MediaMTX's current streams have received and
// sent, for the health graphs.
func (s *Server) VideoBytes() (in, out uint64) {
	s.video.mu.Lock()
	defer s.video.mu.Unlock()
	return s.video.bytesIn, s.video.bytesOut
}

// MediaMTXAPIAuth adds EdgeTAK's internal credentials to MediaMTX API requests.
func (s *Server) MediaMTXAPIAuth(r *http.Request) {
	r.SetBasicAuth(internalVideoUser, s.internalVideoPass())
}

// vcmPath is the path element as ATAK expects it (leading slash).
func (f feedView) vcmPath() string {
	if f.Hosted {
		return "/" + f.Path
	}
	return f.Path
}

func fmtCoord(v float64) string { return strconv.FormatFloat(v, 'f', 7, 64) }

func (s *Server) uploadVCM(w http.ResponseWriter, r *http.Request, id *identity) {
	var doc struct {
		Feeds []struct {
			UID      string `xml:"uid"`
			Alias    string `xml:"alias"`
			Protocol string `xml:"protocol"`
			Address  string `xml:"address"`
			Port     string `xml:"port"`
			Path     string `xml:"path"`
		} `xml:"feed"`
	}
	if err := xml.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&doc); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid videoConnections XML")
		return
	}
	for _, f := range doc.Feeds {
		proto := strings.ToLower(strings.TrimSpace(f.Protocol))
		u := proto + "://" + strings.TrimSpace(f.Address)
		if p := strings.TrimSpace(f.Port); p != "" && p != "-1" {
			u += ":" + p
		}
		if p := strings.TrimSpace(f.Path); p != "" {
			if !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "?") {
				u += "/"
			}
			u += p
		}
		feed := store.VideoFeed{UID: strings.TrimSpace(f.UID), Alias: strings.TrimSpace(f.Alias), URL: u, Groups: id.OutGroups,
			Created: time.Now().UTC(), CreatedBy: id.Username}
		if feed.UID == "" {
			feed.UID = uuid()
		}
		if old, ok := s.Store.VideoFeed(feed.UID); ok {
			if !id.Admin && old.CreatedBy != id.Username {
				jsonError(w, http.StatusForbidden, "that feed belongs to someone else")
				return
			}
			feed.Lat, feed.Lon, feed.Created = old.Lat, old.Lon, old.Created
		}
		if err := validFeed(&feed); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.Store.PutVideoFeed(feed); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("video feed shared", "user", id.Username, "alias", feed.Alias)
	}
	w.WriteHeader(http.StatusOK)
}

func validFeed(f *store.VideoFeed) error {
	f.Alias = truncate(strings.TrimSpace(f.Alias), 100)
	if f.Alias == "" {
		return errors.New("a name is required")
	}
	if f.Path != "" {
		f.URL = ""
		if !videoPathRE.MatchString(f.Path) {
			return errors.New("stream names are letters, digits and . _ ~ - (up to 4 parts separated by /)")
		}
	} else {
		u, err := url.Parse(f.URL)
		if err != nil || u.Host == "" {
			return errors.New("enter a stream URL such as rtsp://camera:554/live, or a server stream name")
		}
		switch strings.ToLower(u.Scheme) {
		case "rtsp", "rtsps", "rtmp", "rtmps", "srt", "udp", "rtp", "http", "https":
		default:
			return errors.New("supported protocols: rtsp, rtmp, srt, udp, rtp, http(s)")
		}
	}
	if (f.Lat == nil) != (f.Lon == nil) || (f.Lat != nil && (*f.Lat < -90 || *f.Lat > 90 || *f.Lon < -180 || *f.Lon > 180)) {
		return errors.New("invalid position")
	}
	return nil
}

// ---- admin ----

func (s *Server) videoStatus(id *identity) map[string]any {
	vc := s.videoCfg()
	s.video.mu.Lock()
	apiErr, polled := s.video.apiErr, s.video.polled
	s.video.mu.Unlock()
	host := s.Cfg.PublicHost
	return map[string]any{
		"enabled": s.Cfg.Video.Enabled, "mediamtx_error": apiErr, "polled": polled,
		"host": host, "rtsp_port": vc.RTSPPort, "rtmp_port": vc.RTMPPort, "srt_port": vc.SRTPort,
		"feeds": s.visibleFeeds(id), "settings": s.Store.VideoSettings(),
	}
}

func (s *Server) setVideoSettings(w http.ResponseWriter, r *http.Request, id *identity) {
	var req store.VideoSettings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.AnonPublish != "off" && req.AnonPublish != "feeds" && req.AnonPublish != "any" {
		jsonError(w, http.StatusBadRequest, "anon_publish must be off, feeds or any")
		return
	}
	req.AnonGroups = cleanGroups(req.AnonGroups)
	if err := s.Store.SetVideoSettings(req); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("video settings changed", "admin", id.Username, "anon_publish", req.AnonPublish, "anon_groups", req.AnonGroups)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) saveFeed(w http.ResponseWriter, r *http.Request, id *identity) {
	var req struct {
		Alias       string   `json:"alias"`
		URL         string   `json:"url"`
		Path        string   `json:"path"`
		Groups      []string `json:"groups"`
		Lat         *float64 `json:"lat"`
		Lon         *float64 `json:"lon"`
		OpenPublish bool     `json:"open_publish"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	f := store.VideoFeed{UID: r.PathValue("uid"), Alias: req.Alias, URL: strings.TrimSpace(req.URL), Path: strings.Trim(strings.TrimSpace(req.Path), "/"),
		Groups: cleanGroups(req.Groups), Lat: req.Lat, Lon: req.Lon, Created: time.Now().UTC(), CreatedBy: id.Username}
	f.OpenPublish = req.OpenPublish && f.Path != ""
	if f.UID == "" {
		f.UID = uuid()
	} else if old, ok := s.Store.VideoFeed(f.UID); ok {
		f.Created, f.CreatedBy = old.Created, old.CreatedBy
	} else {
		jsonError(w, http.StatusNotFound, "feed not found")
		return
	}
	if err := validFeed(&f); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, g := range f.Groups {
		if _, ok := s.Store.Group(g); !ok {
			if _, err := s.Store.CreateGroup(g, ""); err != nil && !errors.Is(err, store.ErrExists) {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	if err := s.Store.PutVideoFeed(f); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("video feed saved", "admin", id.Username, "alias", f.Alias, "path", f.Path, "groups", f.Groups)
	if f.Lat == nil {
		s.removeVideoMarker(f.UID)
	}
	go s.refreshVideoMarkers()
	writeJSON(w, http.StatusOK, f)
}

// ---- MediaMTX authentication callback ----

type mediamtxAuth struct {
	IP       string `json:"ip"`
	User     string `json:"user"`
	Password string `json:"password"`
	Token    string `json:"token"`
	Action   string `json:"action"`
	Path     string `json:"path"`
	Protocol string `json:"protocol"`
	Query    string `json:"query"`
}

// MediaMTXAuthHandler answers MediaMTX's HTTP authentication requests. It is
// served on a loopback-only listener.
func (s *Server) MediaMTXAuthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !isLoopback(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var req mediamtxAuth
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Password == "" && req.Token != "" {
			req.Password = req.Token
		}
		if ok, why := s.authorizeMedia(req); !ok {
			s.Log.Debug("video access denied", "user", req.User, "action", req.Action, "path", req.Path, "protocol", req.Protocol, "ip", req.IP, "why", why)
			http.Error(w, why, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func (s *Server) authorizeMedia(req mediamtxAuth) (bool, string) {
	// EdgeTAK itself (API polling, console HLS relay).
	if req.User == internalVideoUser {
		return subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.internalVideoPass())) == 1, "bad internal credentials"
	}
	if req.User == "" && req.Action == "publish" {
		return s.anonPublish(req)
	}
	if req.User == "" {
		return false, "credentials required" // MediaMTX then asks the client to authenticate
	}
	switch req.Action {
	case "publish":
		u, ok := s.checkVideoPassword(req.User, req.Password)
		if !ok {
			return false, "invalid username or password"
		}
		if !videoPathRE.MatchString(req.Path) {
			return false, "invalid stream name"
		}
		// The stream's saved feed, if any, decides who may publish to it.
		for _, f := range s.Store.VideoFeeds() {
			if f.Path == req.Path && !u.Admin && f.CreatedBy != u.Username && !slices.ContainsFunc(f.Groups, func(g string) bool {
				_, out := u.Effective(s.Cfg.AnonymousGroup)
				return slices.Contains(out, g)
			}) {
				return false, "not allowed to publish to this stream"
			}
		}
		_, out := u.Effective(s.Cfg.AnonymousGroup)
		s.video.mu.Lock()
		s.video.publishers[req.Path] = publisher{User: u.Username, Groups: out}
		s.video.mu.Unlock()
		s.Log.Info("video stream publishing", "user", u.Username, "path", req.Path, "protocol", req.Protocol, "ip", req.IP)
		return true, ""
	case "read", "playback":
		var id *identity
		if req.User == "anonymous" && subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.readToken("anonymous"))) == 1 {
			anon := []string{s.Cfg.AnonymousGroup}
			id = &identity{Username: "anonymous", Groups: anon, OutGroups: anon, Anonymous: true}
		} else if u, ok := s.Store.User(req.User); ok && subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.readToken(u.Username))) == 1 {
			id = s.fromUser(u)
		} else if u, ok := s.checkVideoPassword(req.User, req.Password); ok {
			id = s.fromUser(u)
		} else {
			return false, "invalid credentials"
		}
		if !s.mayWatch(id, req.Path) {
			return false, "not allowed to watch this stream"
		}
		return true, ""
	default:
		return false, "not allowed"
	}
}

// anonPublish decides a publish without credentials, for apps such as TAK ICU
// that cannot send a login.
func (s *Server) anonPublish(req mediamtxAuth) (bool, string) {
	vs := s.Store.VideoSettings()
	if vs.AnonPublish != "feeds" && vs.AnonPublish != "any" {
		return false, "credentials required"
	}
	if !videoPathRE.MatchString(req.Path) {
		return false, "invalid stream name"
	}
	var groups []string
	found := false
	for _, f := range s.Store.VideoFeeds() {
		if f.Path == req.Path {
			if !f.OpenPublish {
				return false, "credentials required" // a saved feed that needs a login
			}
			groups, found = feedGroups(f.Groups, s.Cfg.AnonymousGroup), true
		}
	}
	if !found {
		if vs.AnonPublish != "any" {
			return false, "credentials required"
		}
		groups = feedGroups(vs.AnonGroups, s.Cfg.AnonymousGroup)
	}
	s.video.mu.Lock()
	s.video.publishers[req.Path] = publisher{Groups: groups}
	s.video.mu.Unlock()
	s.Log.Info("video stream publishing without a login", "path", req.Path, "protocol", req.Protocol, "ip", req.IP)
	return true, ""
}

// checkVideoPassword verifies a password, caching successes briefly since
// some clients reconnect often and bcrypt is deliberately slow.
func (s *Server) checkVideoPassword(user, pass string) (*store.User, bool) {
	sum := sha256.Sum256([]byte(user + "\x00" + pass))
	key := hex.EncodeToString(sum[:])
	s.video.mu.Lock()
	exp, cached := s.video.authCache[key]
	s.video.mu.Unlock()
	if cached && time.Now().Before(exp) {
		if u, ok := s.Store.User(user); ok {
			return u, true
		}
	}
	u, ok := s.Store.Authenticate(user, pass)
	if !ok {
		return nil, false
	}
	s.video.mu.Lock()
	if len(s.video.authCache) > 1000 {
		s.video.authCache = map[string]time.Time{}
	}
	s.video.authCache[key] = time.Now().Add(authCacheFor)
	s.video.mu.Unlock()
	return u, true
}

// mayWatch reports whether id may read path: a saved feed's groups decide;
// otherwise the publisher's groups; streams with neither are open to any
// signed-in user.
func (s *Server) mayWatch(id *identity, path string) bool {
	if id.Admin {
		return true
	}
	for _, f := range s.Store.VideoFeeds() {
		if f.Path == path {
			return id.canSee(feedGroups(f.Groups, s.Cfg.AnonymousGroup))
		}
	}
	s.video.mu.Lock()
	p, ok := s.video.publishers[path]
	s.video.mu.Unlock()
	if ok && len(p.Groups) > 0 {
		return p.User == id.Username || id.canSee(p.Groups)
	}
	return !id.Anonymous
}

// ---- MediaMTX API polling ----

func (s *Server) pollMediaMTX(ctx context.Context) {
	client := &http.Client{Timeout: 4 * time.Second}
	t := time.NewTicker(videoPollEvery)
	defer t.Stop()
	for {
		s.pollOnce(ctx, client)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) pollOnce(ctx context.Context, client *http.Client) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.videoCfg().APIAddr+"/v3/paths/list?itemsPerPage=1000", nil)
	req.SetBasicAuth(internalVideoUser, s.internalVideoPass())
	var list struct {
		Items []struct {
			Name          string     `json:"name"`
			Ready         bool       `json:"ready"`
			Available     bool       `json:"available"`
			AvailableTime *time.Time `json:"availableTime"`
			ReadyTime     *time.Time `json:"readyTime"`
			Tracks        []string   `json:"tracks"`
			Source        *struct {
				Type string `json:"type"`
			} `json:"source"`
			Readers       []json.RawMessage `json:"readers"`
			InboundBytes  uint64            `json:"inboundBytes"`
			OutboundBytes uint64            `json:"outboundBytes"`
			BytesReceived uint64            `json:"bytesReceived"` // older MediaMTX
			BytesSent     uint64            `json:"bytesSent"`
		} `json:"items"`
	}
	errText := ""
	res, err := client.Do(req)
	if err == nil {
		if res.StatusCode != http.StatusOK {
			err = fmt.Errorf("MediaMTX API replied %s", res.Status)
		} else {
			err = json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&list)
		}
		res.Body.Close()
	}
	if err != nil {
		errText = err.Error()
		if ctx.Err() != nil {
			return
		}
	}
	live := map[string]liveStream{}
	var bytesIn, bytesOut uint64
	for _, it := range list.Items {
		bytesIn += max(it.InboundBytes, it.BytesReceived)
		bytesOut += max(it.OutboundBytes, it.BytesSent)
		if !it.Ready && !it.Available {
			continue
		}
		ls := liveStream{Name: it.Name, Tracks: it.Tracks, Readers: len(it.Readers)}
		if it.AvailableTime != nil {
			ls.Since = *it.AvailableTime
		} else if it.ReadyTime != nil {
			ls.Since = *it.ReadyTime
		}
		if it.Source != nil {
			ls.Source = it.Source.Type
		}
		live[it.Name] = ls
	}
	s.video.mu.Lock()
	prevErr := s.video.apiErr
	s.video.apiErr, s.video.polled = errText, time.Now()
	if err == nil {
		s.video.live = live
		s.video.bytesIn, s.video.bytesOut = bytesIn, bytesOut
	}
	// MediaMTX takes a few seconds to come up after EdgeTAK starts; only
	// warn when it stays unreachable.
	warn := errText != "" && !s.video.warned && time.Since(s.Started) > 20*time.Second
	if warn {
		s.video.warned = true
	}
	connected := errText == "" && (prevErr != "" || s.video.warned)
	if connected {
		s.video.warned = false
	}
	s.video.mu.Unlock()
	if warn {
		s.Log.Warn("MediaMTX unreachable: video is down (see the MediaMTX log on the Video tab)", "err", errText)
	}
	if connected {
		s.Log.Info("MediaMTX connected", "api", s.videoCfg().APIAddr)
	}
}

// ---- camera markers on devices ----

// videoMarker is a sensor marker at the feed's position whose <__video>
// detail opens the stream in ATAK, with the recipient's credentials.
func (s *Server) videoMarker(f feedView) *cot.Event {
	ev := cot.NewEvent("a-f-G-E-S", "edgetak-video-"+f.UID, "h-g-i-g-o", videoMarkerStale)
	ev.Lat, ev.Lon = *f.Lat, *f.Lon
	ce := fmt.Sprintf(`<ConnectionEntry address="%s" uid="%s" alias="%s" port="%d" roverPort="-1" rtspReliable="1" ignoreEmbeddedKLV="false" path="%s" protocol="%s" networkTimeout="5000" bufferTime="-1"/>`,
		xmlEscape(f.Address), xmlEscape(f.UID), xmlEscape(f.Alias), f.Port, xmlEscape(f.vcmPath()), xmlEscape(f.Protocol))
	ev.Detail.Contact = &cot.Contact{Callsign: f.Alias}
	ev.Detail.XML = `<__video uid="` + xmlEscape(f.UID) + `" url="` + xmlEscape(f.URL) + `">` + ce + `</__video>` +
		`<remarks>Video: ` + xmlEscape(f.Alias) + `</remarks><archive/>`
	return ev
}

// sendVideoMarkers sends c the markers of the positioned feeds it may see.
func (s *Server) sendVideoMarkers(c *router.Client) {
	id := s.clientIdentity(c)
	for _, f := range s.visibleFeeds(id) {
		if f.Lat == nil || f.Lon == nil || f.Protocol == "" {
			continue
		}
		c.Send(s.videoMarker(f))
		s.video.mu.Lock()
		s.video.markers[f.UID] = true
		s.video.mu.Unlock()
	}
}

func (s *Server) refreshVideoMarkers() {
	for _, c := range s.Router.Clients() {
		s.sendVideoMarkers(c)
	}
}

func (s *Server) removeVideoMarker(uid string) {
	s.video.mu.Lock()
	had := s.video.markers[uid]
	delete(s.video.markers, uid)
	s.video.mu.Unlock()
	if !had {
		return
	}
	ev := cot.NewEvent(cot.TypeDelete, uuid(), "h-g-i-g-o", 20*time.Second)
	ev.Detail.XML = `<link uid="edgetak-video-` + xmlEscape(uid) + `" relation="none" type="none"/><__forcedelete/>`
	s.Router.Broadcast(ev)
}

// clientIdentity is the identity a streaming client acts as.
func (s *Server) clientIdentity(c *router.Client) *identity {
	if c.Username != "" {
		if u, ok := s.Store.User(c.Username); ok {
			return s.fromUser(u)
		}
	}
	in := c.InGroups()
	return &identity{Username: "anonymous", Groups: in, OutGroups: c.OutGroups(), Anonymous: c.Username == ""}
}

// ---- HLS relay for the console player ----

func (s *Server) hlsRelay(w http.ResponseWriter, r *http.Request, id *identity) {
	if !s.Cfg.Video.Enabled {
		http.Error(w, "video is not enabled", http.StatusNotFound)
		return
	}
	rest := r.PathValue("rest")
	if strings.Contains(rest, "..") {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	target := &url.URL{Scheme: "http", Host: s.videoCfg().HLSAddr}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/" + rest
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = r.URL.RawQuery
			// The relay acts as MediaMTX's "CDN": one shared session per
			// stream, no per-browser cookies. Access was checked above.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("Authorization", "Bearer "+s.hlsCDNSecret())
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie")
			if loc := res.Header.Get("Location"); strings.HasPrefix(loc, "/") {
				res.Header.Set("Location", "/admin/video/hls"+loc)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "the media server is not reachable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// ---- MediaMTX configuration ----

func (s *Server) mediamtxConfigPath() string {
	if p := s.Cfg.Video.MediaMTXConfig; p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(s.PackageDir), "mediamtx.yml")
}

// writeMediaMTXConfig writes MediaMTX's configuration: every action is
// authorized by EdgeTAK; the API and HLS servers only listen on loopback.
// MediaMTX reloads the file when it changes.
func (s *Server) writeMediaMTXConfig() error {
	vc := s.videoCfg()
	_, authPort, _ := net.SplitHostPort(vc.AuthAddr)
	bind := func(port int) string { return net.JoinHostPort(vc.BindHost, strconv.Itoa(port)) }
	cfg := fmt.Sprintf(`# Written by EdgeTAK; changes are overwritten when EdgeTAK starts.
# Edit the "video" section of EdgeTAK's config.json instead.
logLevel: info
logDestinations: [stdout]
writeQueueSize: 1024

authMethod: http
authHTTPAddress: http://127.0.0.1:%s/mediamtx/auth
authHTTPExclude: []

api: true
apiAddress: %s

# RTSP over TCP only: works through NAT and needs no UDP ports.
rtsp: true
rtspAddress: %s
rtspTransports: [tcp]
rtmp: true
rtmpAddress: %s
srt: true
srtAddress: %s

# HLS feeds the admin console through EdgeTAK's relay. Segments suit
# internet links; always remuxing lets the player start quickly.
hls: true
hlsAddress: %s
hlsAllowOrigins: []
hlsTrustedProxies: ['127.0.0.1']
hlsAlwaysRemux: true
hlsVariant: mpegts
hlsSegmentCount: 7
hlsSegmentDuration: 2s
hlsCDNSecret: %s

webrtc: false
playback: false
metrics: false
pprof: false
# MoQ (on by default since v1.20) writes auto.key/auto.crt into the working
# directory and opens 8892-8893; not needed here.
moq: false

pathDefaults:
  # ATAK's UAS tool, TAKICU and ISR cameras send MPEG-TS (with KLV) inside
  # RTSP; unpack it so HLS and other readers get the video tracks.
  rtspDemuxMpegts: true

paths:
  all_others:
`, authPort, vc.APIAddr, bind(vc.RTSPPort), bind(vc.RTMPPort), bind(vc.SRTPort), vc.HLSAddr, s.hlsCDNSecret())
	path := s.mediamtxConfigPath()
	if old, err := os.ReadFile(path); err == nil && string(old) == cfg {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(cfg), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
