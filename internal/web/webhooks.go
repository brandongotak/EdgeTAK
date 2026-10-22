package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Webhooks (Server -> Webhooks): alerts sent to Discord, Slack, Microsoft
// Teams, ntfy or any URL, so people hear about them with the console
// closed. Each webhook picks the kinds of alert it wants and, optionally,
// the groups whose devices it cares about. Delivery is in the background,
// retried, and limited per webhook so a burst can't flood a channel.

var webhookKinds = []string{"emergency", "geofence", "lost_contact", "ended", "signup", "admin", "server", "security"}

var webhookFormats = map[string]bool{"discord": true, "slack": true, "teams": true, "ntfy": true, "json": true}

const (
	webhookPerMinute = 20 // messages per webhook per minute; more are summarised
	webhookQueue     = 1000
	webhookWorkers   = 4
)

// notice is an alert worth telling people about.
type notice struct {
	Kind     string // emergency, geofence, lost_contact, ended, signup, test
	Title    string // e.g. "911 Emergency"
	Callsign string
	Username string
	Text     string
	Lat, Lon float64
	Time     time.Time
	Groups   []string // the user's groups, for the webhooks' group filters
}

type webhookJob struct {
	hook  store.Webhook
	alert notice
}

type webhookState struct {
	queue   chan webhookJob
	mu      sync.Mutex
	buckets map[string]*webhookBucket
}

type webhookBucket struct {
	tokens     float64
	at         time.Time
	suppressed int // alerts not sent since the last one that was
}

func (s *Server) webhooksStart(ctx context.Context) {
	s.hooks.queue = make(chan webhookJob, webhookQueue)
	for range webhookWorkers {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-s.hooks.queue:
					s.deliverWebhook(ctx, j)
				}
			}
		}()
	}
}

// notify sends an alert to the webhooks that want it.
func (s *Server) notify(a notice) {
	if a.Time.IsZero() {
		a.Time = time.Now()
	}
	if a.Groups == nil && a.Username != "" {
		if u, ok := s.Store.User(a.Username); ok {
			a.Groups = u.Groups
		}
	}
	for _, h := range s.Store.Webhooks() {
		if h.Off || !slices.Contains(h.Events, a.Kind) || (len(h.Groups) > 0 && !slices.ContainsFunc(a.Groups, func(g string) bool { return slices.Contains(h.Groups, g) })) {
			continue
		}
		s.enqueueWebhook(h, a)
	}
}

func (s *Server) enqueueWebhook(h store.Webhook, a notice) bool {
	if s.hooks.queue == nil {
		return false
	}
	select {
	case s.hooks.queue <- webhookJob{h, a}:
		return true
	default:
		s.Log.Warn("webhook queue full; alert not sent", "webhook", h.Name, "alert", a.Title)
		return false
	}
}

// allow takes a token from the webhook's bucket; it returns false when the
// webhook is over its rate, and how many alerts were held back before this one.
func (st *webhookState) allow(id string, now time.Time) (bool, int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.buckets == nil {
		st.buckets = map[string]*webhookBucket{}
	}
	b := st.buckets[id]
	if b == nil {
		b = &webhookBucket{tokens: webhookPerMinute, at: now}
		st.buckets[id] = b
	}
	b.tokens = min(webhookPerMinute, b.tokens+now.Sub(b.at).Minutes()*webhookPerMinute)
	b.at = now
	if b.tokens < 1 {
		b.suppressed++
		return false, 0
	}
	b.tokens--
	held := b.suppressed
	b.suppressed = 0
	return true, held
}

func (s *Server) webhookHTTP() *http.Client {
	if s.WebhookClient != nil {
		return s.WebhookClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// deliverWebhook sends one alert, retrying failures twice.
func (s *Server) deliverWebhook(ctx context.Context, j webhookJob) {
	ok, held := s.hooks.allow(j.hook.ID, time.Now())
	if !ok && j.alert.Kind != "test" {
		return
	}
	if held > 0 {
		j.alert.Text = strings.TrimSpace(j.alert.Text + fmt.Sprintf("\n(%d more alerts weren't sent: too many at once. See the console.)", held))
	}
	var err error
	for attempt, wait := range []time.Duration{0, 2 * time.Second, 10 * time.Second} {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		var retry bool
		if retry, err = s.postWebhook(ctx, j.hook, j.alert); err == nil || !retry {
			break
		}
	}
	s.Store.UpdateWebhook(j.hook.ID, func(w *store.Webhook) {
		w.LastSent, w.LastOK, w.LastErr = time.Now().UTC(), err == nil, ""
		if err != nil {
			w.LastErr, w.Failed = err.Error(), w.Failed+1
		} else {
			w.Sent++
		}
	})
	if err != nil {
		s.Log.Warn("webhook failed", "webhook", j.hook.Name, "alert", j.alert.Title, "err", err)
	}
}

// postWebhook sends the alert once; retry says whether trying again may help.
func (s *Server) postWebhook(ctx context.Context, h store.Webhook, a notice) (retry bool, err error) {
	target, body, headers, err := s.webhookRequest(h, a)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "EdgeTAK/"+s.Version)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.webhookHTTP().Do(req)
	if err != nil {
		return true, errors.New(strings.TrimPrefix(err.Error(), "Post "))
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		if secs, _ := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); secs > 0 && secs <= 30 {
			time.Sleep(time.Duration(secs * float64(time.Second)))
		}
	}
	err = fmt.Errorf("%s answered %s", urlHost(target), resp.Status)
	if t := strings.TrimSpace(string(msg)); t != "" && len(t) < 200 {
		err = fmt.Errorf("%w: %s", err, t)
	}
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500, err
}

func urlHost(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Host
	}
	return "the server"
}

// ---- message formats ----

var webhookEmoji = map[string]string{"emergency": "🚨", "geofence": "🚧", "lost_contact": "📡", "ended": "✅", "signup": "👤", "test": "🔔", "admin": "🛠️", "server": "🖥️", "security": "🔒"}

func (a notice) headline() string {
	h := a.Title
	if a.Callsign != "" {
		h += ": " + a.Callsign
	}
	return h
}

func (a notice) hasPosition() bool { return a.Lat != 0 || a.Lon != 0 }

func (a notice) position() string { return fmt.Sprintf("%.5f, %.5f", a.Lat, a.Lon) }

func (a notice) mapURL() string {
	return fmt.Sprintf("https://www.google.com/maps/search/?api=1&query=%.6f,%.6f", a.Lat, a.Lon)
}

// facts are the alert's details as label/value pairs.
func (a notice) facts(server string) [][2]string {
	var f [][2]string
	if a.Username != "" && a.Username != a.Callsign {
		f = append(f, [2]string{"User", a.Username})
	}
	if a.hasPosition() {
		f = append(f, [2]string{"Position", a.position()})
	}
	f = append(f, [2]string{"Time", a.Time.Local().Format("2006-01-02 15:04:05 MST")}, [2]string{"Server", server})
	return f
}

// webhookRequest builds the request body (and URL and headers) for a
// webhook's format.
func (s *Server) webhookRequest(h store.Webhook, a notice) (string, []byte, map[string]string, error) {
	server := firstNonEmpty(s.Cfg.ServerName, "EdgeTAK")
	emoji := webhookEmoji[a.Kind]
	facts := a.facts(server)
	var payload any
	target, headers := h.URL, map[string]string{}
	switch h.Format {
	case "discord":
		colors := map[string]int{"emergency": 0xD32F2F, "geofence": 0xF29900, "lost_contact": 0xF29900, "ended": 0x2E7D32, "signup": 0x1E88E5, "test": 0x1E88E5, "server": 0xF29900, "security": 0xD32F2F}
		fields := []map[string]any{}
		for _, f := range facts {
			v := f[1]
			if f[0] == "Position" {
				v = "[" + v + "](" + a.mapURL() + ")"
			}
			fields = append(fields, map[string]any{"name": f[0], "value": v, "inline": true})
		}
		embed := map[string]any{"title": emoji + " " + a.headline(), "color": colors[a.Kind], "fields": fields, "timestamp": a.Time.UTC().Format(time.RFC3339)}
		if a.Text != "" {
			embed["description"] = a.Text
		}
		payload = map[string]any{"username": server, "embeds": []any{embed}}
	case "slack":
		var b strings.Builder
		b.WriteString(emoji + " *" + slackEscape(a.headline()) + "*")
		if a.Text != "" {
			b.WriteString("\n" + slackEscape(a.Text))
		}
		for _, f := range facts {
			v := slackEscape(f[1])
			if f[0] == "Position" {
				v = "<" + a.mapURL() + "|" + v + ">"
			}
			b.WriteString("\n" + f[0] + ": " + v)
		}
		payload = map[string]any{"text": b.String()}
	case "teams":
		factSet := []map[string]string{}
		for _, f := range facts {
			factSet = append(factSet, map[string]string{"title": f[0], "value": f[1]})
		}
		color := map[string]string{"emergency": "Attention", "geofence": "Warning", "lost_contact": "Warning", "ended": "Good"}[a.Kind]
		body := []any{map[string]any{"type": "TextBlock", "text": emoji + " " + a.headline(), "weight": "Bolder", "size": "Medium", "wrap": true, "color": firstNonEmpty(color, "Default")}}
		if a.Text != "" {
			body = append(body, map[string]any{"type": "TextBlock", "text": a.Text, "wrap": true})
		}
		body = append(body, map[string]any{"type": "FactSet", "facts": factSet})
		card := map[string]any{"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard", "version": "1.4", "body": body}
		if a.hasPosition() {
			card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": "Open map", "url": a.mapURL()}}
		}
		payload = map[string]any{"type": "message", "attachments": []any{map[string]any{"contentType": "application/vnd.microsoft.card.adaptive", "content": card}}}
	case "ntfy":
		// JSON publishing: the topic is the URL's path, posted to its root.
		u, err := url.Parse(h.URL)
		if err != nil || strings.Trim(u.Path, "/") == "" {
			return "", nil, nil, errors.New("an ntfy URL ends in the topic, e.g. https://ntfy.sh/my-team-alerts")
		}
		topic := strings.Trim(u.Path, "/")
		u.Path, u.RawQuery = "/", ""
		target = u.String()
		priority := map[string]int{"emergency": 5, "geofence": 4, "lost_contact": 4, "server": 4, "security": 4}[a.Kind]
		tags := map[string][]string{"emergency": {"rotating_light"}, "geofence": {"construction"}, "lost_contact": {"satellite"},
			"ended": {"white_check_mark"}, "signup": {"bust_in_silhouette"}, "test": {"bell"}, "server": {"desktop_computer"}, "admin": {"hammer_and_wrench"}, "security": {"lock"}}[a.Kind]
		var lines []string
		if a.Text != "" {
			lines = append(lines, a.Text)
		}
		for _, f := range facts {
			lines = append(lines, f[0]+": "+f[1])
		}
		msg := map[string]any{"topic": topic, "title": a.headline(), "message": strings.Join(lines, "\n"), "priority": max(priority, 3), "tags": tags}
		if a.hasPosition() {
			msg["click"] = a.mapURL()
		}
		payload = msg
		if u.User != nil { // https://user:pass@ntfy.example.org/topic
			pw, _ := u.User.Password()
			req, _ := http.NewRequest("POST", "http://x", nil)
			req.SetBasicAuth(u.User.Username(), pw)
			headers["Authorization"] = req.Header.Get("Authorization")
			u.User = nil
			target = u.String()
		}
	default: // json
		msg := map[string]any{"event": a.Kind, "title": a.Title, "callsign": a.Callsign, "username": a.Username, "text": a.Text,
			"time": a.Time.UTC().Format(time.RFC3339), "server": server}
		if a.hasPosition() {
			msg["lat"], msg["lon"], msg["map_url"] = a.Lat, a.Lon, a.mapURL()
		}
		payload = msg
		headers["X-EdgeTAK-Event"] = a.Kind
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, nil, err
	}
	if h.Format == "json" && h.Secret != "" {
		m := hmac.New(sha256.New, []byte(h.Secret))
		m.Write(body)
		headers["X-EdgeTAK-Signature"] = "sha256=" + hex.EncodeToString(m.Sum(nil))
	}
	return target, body, headers, nil
}

func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ---- admin API ----

type webhookView struct {
	store.Webhook
	URL       string `json:"url"` // shown without its secret path
	HasSecret bool   `json:"has_secret"`
}

func (s *Server) webhookViews() []webhookView {
	out := []webhookView{}
	for _, h := range s.Store.Webhooks() {
		v := webhookView{Webhook: h, URL: maskURL(h.URL), HasSecret: h.Secret != ""}
		v.Secret = ""
		out = append(out, v)
	}
	return out
}

// maskURL keeps a URL's scheme and host: chat services put the token in
// the path.
func maskURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "…"
	}
	return p.Scheme + "://" + p.Host + "/…"
}

func validWebhook(h *store.Webhook) error {
	h.Name = strings.TrimSpace(h.Name)
	h.URL = strings.TrimSpace(h.URL)
	if h.Name == "" || len(h.Name) > 80 {
		return errors.New("give the webhook a name (up to 80 characters)")
	}
	u, err := url.Parse(h.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(h.URL) > 2000 {
		return errors.New("the URL must be an http(s) address")
	}
	if !webhookFormats[h.Format] {
		return errors.New("choose Discord, Slack, Microsoft Teams, ntfy or JSON")
	}
	if h.Format == "ntfy" && strings.Trim(u.Path, "/") == "" {
		return errors.New("an ntfy URL ends in the topic, e.g. https://ntfy.sh/my-team-alerts")
	}
	var events []string
	for _, k := range webhookKinds {
		if slices.Contains(h.Events, k) {
			events = append(events, k)
		}
	}
	if len(events) == 0 {
		return errors.New("choose at least one kind of alert")
	}
	h.Events, h.Groups = events, cleanGroups(h.Groups)
	slices.Sort(h.Groups)
	if h.Format != "json" {
		h.Secret = ""
	}
	return nil
}

func (s *Server) webhookRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/webhooks", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.webhookViews())
	}))
	save := func(w http.ResponseWriter, r *http.Request, id *identity) {
		var h store.Webhook
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&h); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if hid := r.PathValue("id"); hid != "" {
			i := slices.IndexFunc(s.Store.Webhooks(), func(o store.Webhook) bool { return o.ID == hid })
			if i < 0 {
				jsonError(w, http.StatusNotFound, "no such webhook")
				return
			}
			old := s.Store.Webhooks()[i]
			// Blank URL and secret keep the stored ones (the console never sees them).
			if strings.TrimSpace(h.URL) == "" {
				h.URL = old.URL
			}
			if h.Secret == "" {
				h.Secret = old.Secret
			}
			h.ID, h.Created, h.LastSent, h.LastOK, h.LastErr, h.Sent, h.Failed = old.ID, old.Created, old.LastSent, old.LastOK, old.LastErr, old.Sent, old.Failed
		} else {
			h.ID, h.Created = uuid(), time.Now().UTC()
		}
		if err := validWebhook(&h); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.Store.PutWebhook(h); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("webhook saved", "admin", id.Username, "webhook", h.Name, "format", h.Format, "events", h.Events, "groups", h.Groups)
		writeJSON(w, http.StatusOK, s.webhookViews())
	}
	mux.HandleFunc("POST /admin/api/webhooks", s.admin(save))
	mux.HandleFunc("PUT /admin/api/webhooks/{id}", s.admin(save))
	mux.HandleFunc("DELETE /admin/api/webhooks/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if err := s.Store.DeleteWebhook(r.PathValue("id")); err != nil {
			jsonError(w, http.StatusNotFound, "no such webhook")
			return
		}
		writeJSON(w, http.StatusOK, s.webhookViews())
	}))
	// Send a test alert now and report how it went.
	mux.HandleFunc("POST /admin/api/webhooks/{id}/test", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		i := slices.IndexFunc(s.Store.Webhooks(), func(o store.Webhook) bool { return o.ID == r.PathValue("id") })
		if i < 0 {
			jsonError(w, http.StatusNotFound, "no such webhook")
			return
		}
		h := s.Store.Webhooks()[i]
		a := notice{Kind: "test", Title: "Test alert", Callsign: "EdgeTAK", Username: id.Username, Time: time.Now(),
			Text: "If you can read this, " + h.Name + " works. Sent from the EdgeTAK console by " + id.Username + "."}
		if sc := s.Store.ServerContact(); sc.Lat != 0 || sc.Lon != 0 {
			a.Lat, a.Lon = sc.Lat, sc.Lon
		}
		_, err := s.postWebhook(r.Context(), h, a)
		s.Store.UpdateWebhook(h.ID, func(w *store.Webhook) {
			w.LastSent, w.LastOK, w.LastErr = time.Now().UTC(), err == nil, ""
			if err != nil {
				w.LastErr = err.Error()
			}
		})
		if err != nil {
			jsonError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.webhookViews())
	}))
}
