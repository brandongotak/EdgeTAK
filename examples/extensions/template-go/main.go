// My Extension: a starting point for an EdgeTAK extension in Go.
//
// As it comes, it counts the CoT it receives, keeps a marker on the map
// where an admin puts it on its page, and shows its status there. Replace
// the parts marked TODO with your own. See README.md.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"example.com/my-extension/edgetak"
)

//go:embed page
var pageFiles embed.FS

// Settings are what an admin sets on the page. TODO: your own.
type Settings struct {
	MarkerOn bool     `json:"marker_on"`
	Callsign string   `json:"callsign"`
	Lat      *float64 `json:"lat"`
	Lon      *float64 `json:"lon"`
	Interval int      `json:"interval"` // seconds
}

var defaults = Settings{Callsign: "My marker", Interval: 30}

type recent struct {
	Time     time.Time `json:"time"`
	Type     string    `json:"type"`
	UID      string    `json:"uid"`
	Callsign string    `json:"callsign"`
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
}

var (
	tak      *edgetak.Client
	settings *edgetak.Settings[Settings]

	mu       sync.Mutex
	started  = time.Now()
	received int
	sent     int
	lastErr  string
	latest   []recent
)

func fail(where string, err error) {
	mu.Lock()
	lastErr = fmt.Sprintf("%s: %v (%s)", where, err, time.Now().Format("15:04:05"))
	mu.Unlock()
	log.Printf("%s: %v", where, err) // shows under Log on the Extensions page
}

// onEvent is called for each CoT event of the extension's in groups.
// TODO: your logic: forward it somewhere, record it, react to it.
func onEvent(ev edgetak.Event) {
	mu.Lock()
	defer mu.Unlock()
	received++
	latest = append([]recent{{ev.Time, ev.Type, ev.UID, ev.Callsign, ev.Lat, ev.Lon}}, latest...)
	if len(latest) > 20 {
		latest = latest[:20]
	}
}

// sendMarker sends the marker. Positions stay on the map until their stale
// time, and devices connecting later get the latest of each uid.
// TODO: send your own markers.
func sendMarker(ctx context.Context, s Settings) error {
	if s.Lat == nil || s.Lon == nil {
		return nil
	}
	ev := edgetak.Marker(tak.ID+"-marker", *s.Lat, *s.Lon, s.Callsign, edgetak.MarkerOptions{
		Stale: max(time.Minute, 3*time.Duration(s.Interval)*time.Second), Remarks: "Placed by " + tak.ID})
	if err := tak.Send(ctx, ev); err != nil {
		return err
	}
	mu.Lock()
	sent++
	mu.Unlock()
	return nil
}

// worker runs every few seconds. TODO: poll an outside service, send what it reports.
func worker(ctx context.Context) {
	for {
		s := settings.Get()
		if s.MarkerOn {
			if err := sendMarker(ctx, s); err != nil {
				fail("send", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(max(5, s.Interval)) * time.Second):
		}
	}
}

// ---- the page ----

// The console serves the page at /admin/ext/<id>/ and strips that prefix,
// so the handlers see "/", "/api/status"... Only signed-in admins get
// here; X-EdgeTAK-User says which.
func pageHandler(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(pageFiles, "page")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		st := map[string]any{"started": started.Unix(), "received": received, "sent": sent, "error": lastErr,
			"recent": append([]recent{}, latest...)}
		mu.Unlock()
		st["settings"] = settings.Get()
		st["user"] = r.Header.Get("X-EdgeTAK-User")
		var devices []json.RawMessage // needs data:read
		if err := tak.Get(r.Context(), "/api/v1/clients", nil, &devices); err == nil {
			st["devices"] = len(devices)
		}
		reply(w, http.StatusOK, st)
	})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		s, err := checkSettings(req)
		if err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := settings.Set(s); err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("settings changed by %s", r.Header.Get("X-EdgeTAK-User"))
		if s.MarkerOn {
			if err := sendMarker(ctx, s); err != nil {
				fail("send", err)
			}
		}
		reply(w, http.StatusOK, s)
	})
	return mux
}

// checkSettings checks settings from the page (its form sends text).
func checkSettings(req map[string]any) (Settings, error) {
	s := settings.Get()
	s.MarkerOn, _ = req["marker_on"].(bool)
	s.Callsign = strings.TrimSpace(fmt.Sprint(req["callsign"]))
	if s.Callsign == "" || s.Callsign == "<nil>" {
		s.Callsign = defaults.Callsign
	}
	if len(s.Callsign) > 40 {
		s.Callsign = s.Callsign[:40]
	}
	lat, latOK, err1 := number(req["lat"])
	lon, lonOK, err2 := number(req["lon"])
	switch {
	case err1 != nil || err2 != nil:
		return s, errors.New("latitude and longitude are numbers")
	case !latOK || !lonOK:
		s.Lat, s.Lon = nil, nil
	case lat < -90 || lat > 90 || lon < -180 || lon > 180:
		return s, errors.New("latitude is -90 to 90 and longitude -180 to 180")
	default:
		s.Lat, s.Lon = &lat, &lon
	}
	iv, ok, err := number(req["interval"])
	if err != nil || (ok && (iv < 5 || iv > 3600)) {
		return s, errors.New("the interval is 5 to 3600 seconds")
	}
	if ok {
		s.Interval = int(iv)
	}
	if s.MarkerOn && s.Lat == nil {
		return s, errors.New("set a position for the marker")
	}
	return s, nil
}

// number reads a JSON number or a numeric string; ok is false when empty.
func number(v any) (n float64, ok bool, err error) {
	switch v := v.(type) {
	case nil:
		return 0, false, nil
	case float64:
		return v, true, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return 0, false, nil
		}
		n, err = strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n, err == nil, err
	}
	return 0, false, errors.New("not a number")
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func main() {
	log.SetFlags(0)
	var err error
	if tak, err = edgetak.FromEnv(); err != nil {
		log.Fatal(err)
	}
	// EdgeTAK stops an extension with SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	settings = edgetak.LoadSettings(tak.DataDir, defaults)

	info, err := tak.Info(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%s %s on %s (EdgeTAK %s): in %v, out %v", info.Name, info.Version, info.ServerName, info.EdgeTAKVersion, info.GroupsIn, info.GroupsOut)
	for _, p := range info.Permissions {
		if p == "cot:read" {
			go tak.Stream(ctx, nil, onEvent, func(err error) { fail("stream", err) })
		}
	}
	go worker(ctx)
	if tak.PagePort != 0 {
		srv := &http.Server{Addr: "127.0.0.1:" + strconv.Itoa(tak.PagePort), Handler: pageHandler(ctx), ReadHeaderTimeout: 10 * time.Second}
		go func() { <-ctx.Done(); srv.Close() }()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
		return
	}
	<-ctx.Done()
}
