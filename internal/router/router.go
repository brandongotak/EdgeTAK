// Package router tracks connected stream clients and decides which of them
// receive each CoT event.
package router

import (
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edgetak/edgetak/internal/cot"
)

// Hook inspects (and may modify) an event before it is routed. Returning
// false drops the event. Hooks are the extension point for plugins. src is
// nil for events injected by the server.
type Hook func(src *Client, ev *cot.Event) bool

type Router struct {
	log          *slog.Logger
	sendLatestSA bool

	mu      sync.RWMutex
	clients map[uint64]*Client
	hooks   []Hook
	nextID  atomic.Uint64
}

func New(log *slog.Logger, sendLatestSA bool) *Router {
	return &Router{log: log, sendLatestSA: sendLatestSA, clients: map[uint64]*Client{}}
}

// AddHook registers a hook; hooks run in registration order.
func (r *Router) AddHook(h Hook) {
	r.mu.Lock()
	r.hooks = append(r.hooks, h)
	r.mu.Unlock()
}

// Register adds a connected client and replays the latest SA of visible peers.
func (r *Router) Register(c *Client) {
	c.ID = r.nextID.Add(1)
	r.mu.Lock()
	r.clients[c.ID] = c
	var replay []*cot.Event
	if r.sendLatestSA {
		now := time.Now()
		for _, o := range r.clients {
			if o == c || !sharesGroup(o.Groups, c.Groups) {
				continue
			}
			if sa := o.LastSA(); sa != nil && !sa.IsStale(now) {
				replay = append(replay, sa)
			}
		}
	}
	r.mu.Unlock()
	for _, sa := range replay {
		c.Send(sa)
	}
	r.log.Info("client connected", "id", c.ID, "remote", c.Remote, "user", c.Username, "transport", c.Transport, "groups", c.Groups)
}

func (r *Router) Unregister(c *Client) {
	r.mu.Lock()
	delete(r.clients, c.ID)
	r.mu.Unlock()
	uid, cs := c.Identity()
	r.log.Info("client disconnected", "id", c.ID, "remote", c.Remote, "uid", uid, "callsign", cs,
		"format", c.Format(), "in", c.In.Load(), "out", c.Out.Load(), "dropped", c.Dropped.Load())
}

// Clients returns a snapshot of connected clients ordered by id.
func (r *Router) Clients() []*Client {
	r.mu.RLock()
	out := make([]*Client, 0, len(r.clients))
	for _, c := range r.clients {
		out = append(out, c)
	}
	r.mu.RUnlock()
	slices.SortFunc(out, func(a, b *Client) int { return int(a.ID - b.ID) })
	return out
}

func (r *Router) Client(id uint64) (*Client, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.clients[id]
	return c, ok
}

// Route delivers an event from src to every other client that shares a group
// with it, or only to the addressed clients when the event carries
// <marti><dest uid|callsign/></marti> entries.
func (r *Router) Route(src *Client, ev *cot.Event) {
	r.deliver(src, src.Groups, ev)
}

// Inject delivers a server-originated event to clients in any of groups.
func (r *Router) Inject(ev *cot.Event, groups []string) {
	r.deliver(nil, groups, ev)
}

func (r *Router) deliver(src *Client, groups []string, ev *cot.Event) {
	r.mu.RLock()
	hooks := r.hooks
	r.mu.RUnlock()
	for _, h := range hooks {
		if !h(src, ev) {
			return
		}
	}

	var uids, callsigns []string
	for _, d := range ev.Dests() {
		if d.UID != "" {
			uids = append(uids, d.UID)
		}
		if d.Callsign != "" {
			callsigns = append(callsigns, d.Callsign)
		}
	}
	directed := len(uids)+len(callsigns) > 0

	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.clients {
		if c == src || !sharesGroup(groups, c.Groups) {
			continue
		}
		if directed {
			uid, cs := c.Identity()
			if !slices.Contains(uids, uid) && !slices.Contains(callsigns, cs) {
				continue
			}
		}
		c.Send(ev)
	}
}

func sharesGroup(a, b []string) bool {
	for _, g := range a {
		if slices.Contains(b, g) {
			return true
		}
	}
	return false
}

// ---- Client ----

const queueSize = 2048

// Client is one connected streaming endpoint.
type Client struct {
	ID          uint64
	Remote      string
	Transport   string // "tcp" or "tls"
	Username    string // empty for anonymous TCP clients
	CertSerial  string // hex serial of the client certificate (TLS only)
	Groups      []string
	ConnectedAt time.Time

	In, Out, Dropped atomic.Uint64
	// Protobuf is set once the stream has negotiated TAK protocol v1.
	Protobuf atomic.Bool

	queue  chan *cot.Event
	closed chan struct{}
	once   sync.Once
	close  func()

	mu       sync.RWMutex
	uid      string
	callsign string
	lastSA   *cot.Event
	lastSeen time.Time
}

// NewClient creates a client whose Close calls closeFn (typically conn.Close).
func NewClient(remote, transport, username string, groups []string, closeFn func()) *Client {
	now := time.Now()
	return &Client{
		Remote: remote, Transport: transport, Username: username, Groups: groups,
		ConnectedAt: now, lastSeen: now,
		queue: make(chan *cot.Event, queueSize), closed: make(chan struct{}), close: closeFn,
	}
}

// Send queues ev without blocking; events are dropped for clients that
// cannot keep up so one slow link never stalls the router.
func (c *Client) Send(ev *cot.Event) {
	select {
	case c.queue <- ev:
	case <-c.closed:
	default:
		c.Dropped.Add(1)
	}
}

// Queue is consumed by the connection's writer.
func (c *Client) Queue() <-chan *cot.Event { return c.queue }

// Done is closed when the client is closed.
func (c *Client) Done() <-chan struct{} { return c.closed }

func (c *Client) Close() {
	c.once.Do(func() {
		close(c.closed)
		if c.close != nil {
			c.close()
		}
	})
}

// Observe updates the client's identity and latest SA from an inbound event.
// The first self-report (an a-* event with a callsign and device info)
// establishes the client's UID.
func (c *Client) Observe(ev *cot.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSeen = time.Now()
	if !strings.HasPrefix(ev.Type, "a-") {
		return
	}
	if c.uid == "" {
		d := ev.Detail
		if d.Contact == nil || d.Contact.Callsign == "" || (d.Takv == nil && d.Contact.Endpoint == "") {
			return
		}
		c.uid = ev.UID
	}
	if ev.UID == c.uid {
		c.callsign = ev.Callsign()
		c.lastSA = ev
	}
}

// Format reports the client's negotiated wire format: "protobuf" or "xml".
func (c *Client) Format() string { return c.wireFormat().String() }

func (c *Client) wireFormat() cot.Format {
	if c.Protobuf.Load() {
		return cot.FormatProto
	}
	return cot.FormatXML
}

func (c *Client) Touch() {
	c.mu.Lock()
	c.lastSeen = time.Now()
	c.mu.Unlock()
}

func (c *Client) Identity() (uid, callsign string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.uid, c.callsign
}

func (c *Client) LastSA() *cot.Event {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSA
}

func (c *Client) LastSeen() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSeen
}
