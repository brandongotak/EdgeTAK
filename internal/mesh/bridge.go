// Package mesh bridges ATAK's mesh network to EdgeTAK. Without a server,
// ATAK and WinTAK devices on one network (Wi-Fi, or MANET radios) share
// positions over UDP multicast (239.2.3.1:6969) and chat (224.10.10.1:17012).
// The bridge joins those groups on chosen network interfaces: what it hears
// goes into an EdgeTAK group, and the traffic of chosen groups goes out to
// the mesh, so radio teams and server users see each other.
//
// It joins the router as a tap (see router.AddTap): what it brings in has
// it as the source, so it is never sent back out, and the latest position
// of each mesh device is replayed to devices connecting to the server. Its
// own datagrams, heard back, are dropped by content; so is mesh traffic
// from devices also connected to the server. It sends TAK Protocol unless
// a device heard lately speaks only XML, as ATAK does.
package mesh

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

const (
	peerTimeout = 120 * time.Second // ATAK forgets a mesh contact's protocol after this (PROTOINF_TIMEOUT_SEC)
	echoWindow  = time.Minute
	maxRate     = 20 // datagrams a second to the mesh, at most
	maxBurst    = 40
)

// Packet is a datagram heard, or one to send.
type Packet struct {
	Data []byte
	From net.Addr // where it came from (heard)
	Port string   // "sa", "chat" or "direct": which socket heard it
}

// Interface is a network interface the bridge works on.
type Interface struct {
	Name  string `json:"name"`
	Addr  string `json:"addr"`            // its IPv4 address
	Error string `json:"error,omitempty"` // joining the groups failed
}

// Transport is the network: UDP multicast for real, in memory for tests.
type Transport interface {
	// Open joins the groups on the interfaces (none: every interface that
	// can multicast) and starts delivering what is heard to recv.
	Open(ctx context.Context, set store.MeshSettings, recv func(Packet)) ([]Interface, error)
	// Send sends a datagram to addr out of the interface named ifname.
	Send(ifname, addr string, b []byte) error
	Close()
}

// Status is what the console shows.
type Status struct {
	Running    bool        `json:"running"`
	Error      string      `json:"error,omitempty"`
	Interfaces []Interface `json:"interfaces"`
	FromMesh   uint64      `json:"from_mesh"`
	ToMesh     uint64      `json:"to_mesh"`
	Limited    uint64      `json:"limited"` // not sent: rate limits
	Format     string      `json:"format"`  // what it sends: "TAK Protocol" or "XML"
	XMLPeer    string      `json:"xml_peer,omitempty"`
	Peers      []PeerView  `json:"peers"`
	Since      time.Time   `json:"since,omitzero"`
}

// PeerView is a device heard on the mesh.
type PeerView struct {
	Addr     string    `json:"addr"`
	Callsign string    `json:"callsign,omitempty"`
	UID      string    `json:"uid,omitempty"`
	Format   string    `json:"format"`
	Last     time.Time `json:"last"`
}

type peer struct {
	uid, callsign string
	last          time.Time
	lastProto     bool      // its last message was TAK Protocol
	protoSeen     time.Time // when it last sent TAK Protocol: it can read it until peerTimeout after
}

// Bridge is the mesh bridge.
type Bridge struct {
	Router    *router.Router
	Log       *slog.Logger
	Transport Transport
	AnonGroup string
	Now       func() time.Time // tests

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	set     store.MeshSettings
	tap     *router.Client
	st      Status
	ifaces  []Interface
	peers   map[string]*peer // by source IP
	echoes  map[[32]byte]time.Time
	lastPos map[string]time.Time // uid -> last position sent to the mesh
	tokens  float64
	tokenAt time.Time
}

func (b *Bridge) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Start runs the bridge with set (if enabled) until ctx ends.
func (b *Bridge) Start(ctx context.Context, set store.MeshSettings) {
	b.mu.Lock()
	b.ctx = ctx
	b.mu.Unlock()
	b.Apply(set)
}

// Apply stops the bridge and starts it again with set.
func (b *Bridge) Apply(set store.MeshSettings) {
	b.stop()
	set = set.Normalized(b.AnonGroup)
	b.mu.Lock()
	b.set = set
	parent := b.ctx
	b.mu.Unlock()
	if !set.Enabled || parent == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	tap := router.NewClient("mesh", "mesh", "mesh", set.ToMesh, []string{set.Group}, nil)
	b.mu.Lock()
	b.cancel, b.tap = cancel, tap
	b.st = Status{Since: b.now()}
	b.peers, b.echoes, b.lastPos = map[string]*peer{}, map[[32]byte]time.Time{}, map[string]time.Time{}
	b.tokens, b.tokenAt = maxBurst, b.now()
	b.mu.Unlock()

	ifaces, err := b.Transport.Open(ctx, set, b.heard)
	b.mu.Lock()
	b.ifaces = ifaces
	if err != nil {
		b.st.Error = err.Error()
		b.mu.Unlock()
		b.Log.Error("mesh bridge", "err", err)
		cancel()
		return
	}
	b.st.Running = true
	b.mu.Unlock()
	b.Router.AddTap(tap)
	b.Log.Info("mesh bridge on", "interfaces", len(ifaces), "group", set.Group, "to_mesh", set.ToMesh)
	go b.sendLoop(ctx, tap)
	b.Router.Replay(tap) // the server's devices, out to the mesh
}

func (b *Bridge) stop() {
	b.mu.Lock()
	cancel, tap := b.cancel, b.tap
	b.cancel, b.tap = nil, nil
	b.st.Running = false
	b.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	b.Router.RemoveTap(tap)
	tap.Close()
	b.Transport.Close()
}

// ---- from the mesh ----

func (b *Bridge) heard(p Packet) {
	now := b.now()
	sum := sha256.Sum256(p.Data)
	b.mu.Lock()
	tap := b.tap
	_, echo := b.echoes[sum]
	b.mu.Unlock()
	if tap == nil || echo {
		return // our own datagram, heard back
	}
	ev, proto, err := decode(p.Data)
	if err != nil {
		b.Log.Debug("mesh: unreadable datagram", "from", p.From, "err", err)
		return
	}
	host := hostOf(p.From)
	b.mu.Lock()
	pe := b.peers[host]
	if pe == nil {
		pe = &peer{}
		b.peers[host] = pe
	}
	pe.last, pe.lastProto = now, proto
	if proto {
		pe.protoSeen = now
	}
	if ev != nil && ev.Detail.Contact != nil && ev.Detail.Contact.Callsign != "" && len(ev.Type) > 2 && ev.Type[:2] == "a-" {
		pe.uid, pe.callsign = ev.UID, ev.Detail.Contact.Callsign
	}
	b.mu.Unlock()
	if ev == nil || category(ev.Type) == "control" || ev.IsStale(now) {
		return
	}
	// A device also connected to the server: it reaches the server itself.
	for _, c := range b.Router.Clients() {
		if uid, _ := c.Identity(); uid != "" && uid == ev.UID {
			return
		}
	}
	if to := chatRecipient(ev); to != "" && p.Port == "direct" {
		ev = addressedTo(ev, to) // a direct message to a server user
	}
	b.Router.Route(tap, ev)
	b.mu.Lock()
	b.st.FromMesh++
	b.mu.Unlock()
}

func hostOf(a net.Addr) string {
	if a == nil {
		return ""
	}
	if h, _, err := net.SplitHostPort(a.String()); err == nil {
		return h
	}
	return a.String()
}

// ---- to the mesh ----

func (b *Bridge) sendLoop(ctx context.Context, tap *router.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tap.Done():
			return
		case ev := <-tap.Queue():
			b.toMesh(ev)
		}
	}
}

func (b *Bridge) wanted(typ string, set store.MeshSettings) bool {
	switch category(typ) {
	case "positions":
		return set.SendPositions
	case "chat":
		return set.SendChat
	case "alerts":
		return set.SendAlerts
	case "drawings":
		return set.SendDrawings
	case "other":
		return set.SendOther
	}
	return false
}

func (b *Bridge) toMesh(ev *cot.Event) {
	now := b.now()
	b.mu.Lock()
	set, ifaces := b.set, b.ifaces
	b.mu.Unlock()
	if ev.IsStale(now) || !b.wanted(ev.Type, set) {
		return
	}
	b.mu.Lock()
	// Each item's position at most every PositionSeconds: radios are slow.
	if ev.Type[0] == 'a' {
		if last, ok := b.lastPos[ev.UID]; ok && now.Sub(last) < time.Duration(set.PositionSeconds)*time.Second {
			b.st.Limited++
			b.mu.Unlock()
			return
		}
	}
	// And at most maxRate datagrams a second in all.
	b.tokens = min(maxBurst, b.tokens+now.Sub(b.tokenAt).Seconds()*maxRate)
	b.tokenAt = now
	if b.tokens < float64(len(ifaces)) {
		b.st.Limited++
		b.mu.Unlock()
		return
	}
	b.tokens -= float64(len(ifaces))
	if ev.Type[0] == 'a' {
		b.lastPos[ev.UID] = now
	}
	proto, _ := b.formatLocked(now)
	b.mu.Unlock()

	addr := set.SAAddr
	if category(ev.Type) == "chat" {
		addr = set.ChatAddr
	}
	sent := false
	for _, ifc := range ifaces {
		if ifc.Error != "" {
			continue
		}
		out := ev
		if set.DirectPort > 0 && ifc.Addr != "" {
			out = withEndpoint(ev, ifc.Addr+":"+strconv.Itoa(set.DirectPort)+":tcp")
		}
		data := encode(out, proto)
		b.mu.Lock()
		b.echoes[sha256.Sum256(data)] = now
		b.mu.Unlock()
		if err := b.Transport.Send(ifc.Name, addr, data); err != nil {
			b.Log.Debug("mesh: send failed", "interface", ifc.Name, "err", err)
			continue
		}
		sent = true
	}
	if sent {
		b.mu.Lock()
		b.st.ToMesh++
		b.mu.Unlock()
	}
}

// formatLocked is whether to send TAK Protocol: unless a device heard
// lately can only read XML (it sent XML, and no TAK Protocol in
// peerTimeout), as ATAK decides. It also prunes old state.
func (b *Bridge) formatLocked(now time.Time) (proto bool, xmlPeer string) {
	proto = true
	for host, p := range b.peers {
		if now.Sub(p.last) > peerTimeout {
			delete(b.peers, host)
			continue
		}
		if !p.lastProto && now.Sub(p.protoSeen) > peerTimeout {
			proto, xmlPeer = false, host
		}
	}
	for sum, t := range b.echoes {
		if now.Sub(t) > echoWindow {
			delete(b.echoes, sum)
		}
	}
	for uid, t := range b.lastPos {
		if now.Sub(t) > time.Hour {
			delete(b.lastPos, uid)
		}
	}
	return proto, xmlPeer
}

// Status is the bridge's state for the console.
func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.st
	st.Interfaces = slices.Clone(b.ifaces)
	if st.Interfaces == nil {
		st.Interfaces = []Interface{}
	}
	proto, xmlPeer := b.formatLocked(b.now())
	st.Format, st.XMLPeer = "TAK Protocol", xmlPeer
	if !proto {
		st.Format = "XML"
	}
	st.Peers = []PeerView{}
	for host, p := range b.peers {
		f := "XML"
		if p.lastProto {
			f = "TAK Protocol"
		}
		st.Peers = append(st.Peers, PeerView{Addr: host, Callsign: p.callsign, UID: p.uid, Format: f, Last: p.last})
	}
	slices.SortFunc(st.Peers, func(a, c PeerView) int { return c.Last.Compare(a.Last) })
	return st
}

// Interfaces lists the network interfaces that can multicast, for the console.
func Interfaces() []Interface {
	out := []Interface{}
	all, _ := net.Interfaces()
	for _, ifc := range all {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if a := ipv4Of(&ifc); a != "" {
			out = append(out, Interface{Name: ifc.Name, Addr: a})
		}
	}
	return out
}

func ipv4Of(ifc *net.Interface) string {
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.To4().String()
		}
	}
	return ""
}
