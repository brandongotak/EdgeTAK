// Package edgetak is a small client for EdgeTAK's extension API. Copy it
// into your extension as it is; it needs only the standard library. See
// docs/extensions.md in the EdgeTAK repository for the API it wraps.
package edgetak

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is the extension's connection to EdgeTAK.
type Client struct {
	URL, Token string
	ID         string // the manifest's id
	DataDir    string // the extension's own folder, kept across updates
	ServerName string
	PagePort   int // where to serve the page (0: no page)
	HTTP       *http.Client
}

// FromEnv sets up the client from the environment EdgeTAK gives the extension.
func FromEnv() (*Client, error) {
	c := &Client{URL: os.Getenv("EDGETAK_URL"), Token: os.Getenv("EDGETAK_TOKEN"), ID: os.Getenv("EDGETAK_EXTENSION_ID"),
		DataDir: os.Getenv("EDGETAK_DATA_DIR"), ServerName: os.Getenv("EDGETAK_SERVER_NAME"), HTTP: &http.Client{}}
	if c.URL == "" || c.Token == "" {
		return nil, fmt.Errorf("EDGETAK_URL and EDGETAK_TOKEN are not set: EdgeTAK runs extensions, install it on the Extensions page")
	}
	if p := os.Getenv("EDGETAK_PAGE_PORT"); p != "" {
		c.PagePort, _ = strconv.Atoi(p)
	}
	return c, nil
}

// APIError is an error answer from EdgeTAK.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e struct{ Error string }
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, &APIError{resp.StatusCode, e.Error}
	}
	return resp, nil
}

// Get reads a JSON API path into v, e.g. Get(ctx, "/api/v1/clients", nil, &devices).
func (c *Client) Get(ctx context.Context, path string, query url.Values, v any) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	resp, err := c.do(ctx, "GET", path, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// Info is the extension's settings and the server's, from /api/v1/info.
type Info struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Version        string   `json:"version"`
	Permissions    []string `json:"permissions"`
	ServerName     string   `json:"server_name"`
	EdgeTAKVersion string   `json:"edgetak_version"`
	GroupsIn       []string `json:"groups_in"`
	GroupsOut      []string `json:"groups_out"`
	PagePath       string   `json:"page_path"`
}

func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	err := c.Get(ctx, "/api/v1/info", nil, &i)
	return i, err
}

// Send sends CoT events (XML, e.g. from Marker) to the extension's out groups.
func (c *Client) Send(ctx context.Context, events ...string) error {
	return c.SendTo(ctx, nil, events...)
}

// SendTo sends CoT events to some of the extension's out groups (all of
// them if groups is empty).
func (c *Client) SendTo(ctx context.Context, groups []string, events ...string) error {
	path := "/api/v1/cot"
	if len(groups) > 0 {
		path += "?groups=" + url.QueryEscape(strings.Join(groups, ","))
	}
	resp, err := c.do(ctx, "POST", path, strings.NewReader(strings.Join(events, "")), "application/xml")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Delete is a CoT delete event: it removes the item uid from devices' maps
// (in the groups it is sent to).
func Delete(uid, cotType string) string {
	now := time.Now().UTC()
	ts := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z") }
	return fmt.Sprintf(`<event version="2.0" uid="delete-%s-%d" type="t-x-d-d" how="h-g-i-g-o" time="%s" start="%s" stale="%s">`+
		`<point lat="0" lon="0" hae="0" ce="9999999" le="9999999"/><detail><link relation="p-p" uid="%s" type="%s"/></detail></event>`,
		escape(uid), now.UnixNano(), ts(now), ts(now), ts(now.Add(20*time.Second)), escape(uid), escape(cotType))
}

// Event is one CoT event from the stream.
type Event struct {
	UID      string    `json:"uid"`
	Type     string    `json:"type"`
	How      string    `json:"how"`
	Time     time.Time `json:"time"`
	Stale    time.Time `json:"stale"`
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	Hae      float64   `json:"hae"`
	Callsign string    `json:"callsign"`
	XML      string    `json:"xml"`
}

// Stream calls fn for each CoT event of the extension's in groups until ctx
// ends, reconnecting whenever the connection drops (onErr hears why; it may
// be nil). types limits it to types starting with these prefixes.
func (c *Client) Stream(ctx context.Context, types []string, fn func(Event), onErr func(error)) {
	path := "/api/v1/cot"
	if len(types) > 0 {
		path += "?types=" + url.QueryEscape(strings.Join(types, ","))
	}
	for ctx.Err() == nil {
		err := c.stream(ctx, path, fn)
		if ctx.Err() != nil {
			return
		}
		if onErr != nil && err != nil {
			onErr(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *Client) stream(ctx context.Context, path string, fn func(Event)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := c.do(ctx, "GET", path, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// A blank line comes every 30 s, so a longer silence means the connection is gone.
	alive := time.AfterFunc(90*time.Second, cancel)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		alive.Reset(90 * time.Second)
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err == nil {
			fn(ev)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

// MarkerOptions are a marker's optional parts.
type MarkerOptions struct {
	Type    string        // default a-f-G-U-C (a friendly ground unit)
	Stale   time.Duration // default 2 minutes: send it again before then
	Hae     float64
	Remarks string
}

// Marker is a CoT position event.
func Marker(uid string, lat, lon float64, callsign string, o MarkerOptions) string {
	if o.Type == "" {
		o.Type = "a-f-G-U-C"
	}
	if o.Stale == 0 {
		o.Stale = 2 * time.Minute
	}
	now := time.Now().UTC()
	ts := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z") }
	detail := `<contact callsign="` + escape(callsign) + `"/>`
	if o.Remarks != "" {
		detail += "<remarks>" + escape(o.Remarks) + "</remarks>"
	}
	return fmt.Sprintf(`<event version="2.0" uid="%s" type="%s" how="h-e" time="%s" start="%s" stale="%s">`+
		`<point lat="%.7f" lon="%.7f" hae="%.1f" ce="10" le="10"/><detail>%s</detail></event>`,
		escape(uid), escape(o.Type), ts(now), ts(now), ts(now.Add(o.Stale)), lat, lon, o.Hae, detail)
}

func escape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Settings keeps a settings value as JSON in the extension's data folder.
type Settings[T any] struct {
	path string
	mu   sync.Mutex
	v    T
}

// LoadSettings reads settings.json, or starts from defaults.
func LoadSettings[T any](dataDir string, defaults T) *Settings[T] {
	s := &Settings[T]{path: filepath.Join(dataDir, "settings.json"), v: defaults}
	if b, err := os.ReadFile(s.path); err == nil {
		json.Unmarshal(b, &s.v)
	}
	return s
}

func (s *Settings[T]) Get() T {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v
}

func (s *Settings[T]) Set(v T) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.path+".tmp", b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(s.path+".tmp", s.path); err != nil {
		return err
	}
	s.v = v
	return nil
}
