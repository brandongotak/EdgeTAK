package federation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

const (
	Outbound = "outbound"
	Inbound  = "inbound"

	queueSize      = 5000
	recentMessages = 25
	maxMessage     = 64 << 20
	contactSweep   = 5 * time.Second
	pendingFor     = time.Hour
)

// Config is what the manager needs from the rest of EdgeTAK.
type Config struct {
	Router     *router.Router
	Store      *store.Store
	Cert       func() *tls.Certificate // the certificate EdgeTAK presents (its server certificate)
	ServerName string
	NodeID     string
	Version    string
	AnonGroup  string
	Log        *slog.Logger
	// Listen and Dial default to TCP; tests replace them.
	Listen func(addr string) (net.Listener, error)
	Dial   func(addr string) (net.Conn, error)
}

// Manager runs EdgeTAK's federation: outbound connections, the inbound
// listener, routing between local devices and federates, and contacts.
type Manager struct {
	cfg Config

	mu       sync.Mutex
	sessions map[string]*session // by federate ID
	pending  map[string]Pending  // rejected inbound servers, by fingerprint
	listener *listener
	stopped  bool

	cmu      sync.Mutex
	contacts map[string]*contact // local devices announced to federates, by UID
}

// Pending is a server that tried to connect without being approved.
type Pending struct {
	Fingerprint string    `json:"fingerprint"`
	CommonName  string    `json:"common_name"`
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	Remote      string    `json:"remote"`
	LastSeen    time.Time `json:"last_seen"`
	certDER     []byte
}

type contact struct {
	uid, callsign string
	groups        []string // the device's OUT groups
	stale         time.Time
}

// New creates the manager; Start connects.
func New(cfg Config) *Manager {
	if cfg.Listen == nil {
		cfg.Listen = func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
	}
	if cfg.Dial == nil {
		cfg.Dial = func(addr string) (net.Conn, error) { return (&net.Dialer{Timeout: 20 * time.Second}).Dial("tcp", addr) }
	}
	return &Manager{cfg: cfg, sessions: map[string]*session{}, pending: map[string]Pending{}, contacts: map[string]*contact{}}
}

// Start hooks into the router, starts the sessions and the listener, and
// sweeps contacts until stop is closed.
func (m *Manager) Start(stop <-chan struct{}) {
	m.cfg.Router.AddHook(m.hook)
	m.Reload()
	go func() {
		t := time.NewTicker(contactSweep)
		defer t.Stop()
		for {
			select {
			case <-stop:
				m.shutdown()
				return
			case <-t.C:
				m.sweepContacts()
			}
		}
	}()
}

func (m *Manager) shutdown() {
	m.mu.Lock()
	m.stopped = true
	sessions := m.sessions
	m.sessions = map[string]*session{}
	l := m.listener
	m.listener = nil
	m.mu.Unlock()
	for _, s := range sessions {
		s.stop()
	}
	if l != nil {
		l.stop()
	}
}

// Reload brings the sessions and listener in line with the stored settings.
func (m *Manager) Reload() {
	settings := m.cfg.Store.FederationSettings()
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	want := map[string]store.Federate{}
	for _, f := range settings.Federates {
		if f.Enabled {
			want[f.ID] = f
		}
	}
	var stop []*session
	for id, s := range m.sessions {
		if f, ok := want[id]; !ok || !sameFederate(f, s.fed) {
			stop = append(stop, s)
			delete(m.sessions, id)
		}
	}
	var start []*session
	for id, f := range want {
		if _, ok := m.sessions[id]; ok {
			continue
		}
		s := newSession(m, f)
		m.sessions[id] = s
		if f.Direction == Outbound {
			start = append(start, s)
		}
	}
	var oldListener *listener
	if l := m.listener; l != nil && (!settings.ListenEnabled || l.port != settings.ListenPort) {
		oldListener, m.listener = l, nil
	}
	needListener := settings.ListenEnabled && m.listener == nil
	m.mu.Unlock()

	for _, s := range stop {
		s.stop()
	}
	for _, s := range start {
		go s.runOutbound()
	}
	if oldListener != nil {
		oldListener.stop()
	}
	if needListener {
		l := m.startListener(settings.ListenPort)
		m.mu.Lock()
		if m.stopped || m.listener != nil {
			m.mu.Unlock()
			l.stop()
			return
		}
		m.listener = l
		m.mu.Unlock()
	}
}

func sameFederate(a, b store.Federate) bool {
	return fmt.Sprintf("%#v", a) == fmt.Sprintf("%#v", b)
}

func (m *Manager) provenance() []Provenance {
	return []Provenance{{ServerID: m.cfg.NodeID, ServerName: m.cfg.ServerName}}
}

func (m *Manager) identity(typ int64, uid string) Identity {
	return Identity{Name: m.cfg.ServerName, UID: uid, ServerID: m.cfg.NodeID, Description: "EdgeTAK", Type: typ}
}

// version is the TAK Server version EdgeTAK reports. It must look like a
// real TAK Server: TAK Server only completes its HealthCheck reply for
// clients whose major version is above 0 (older ones broke when it did), so
// a 0.x EdgeTAK version would leave every health check hanging. EdgeTAK's
// own version goes in the variant.
func (m *Manager) version() *Version {
	return &Version{Major: 5, Minor: 6, Patch: 0, Branch: "EdgeTAK", Variant: m.cfg.Version}
}

// subscription is how EdgeTAK identifies itself on streams it answers.
func (m *Manager) subscription() *Subscription {
	return &Subscription{Identity: m.identity(identityTakServer, m.cfg.NodeID), Version: m.version()}
}

// groupsMessage lists what EdgeTAK shares with a federate.
func (m *Manager) groupsMessage(f store.Federate) *FederateGroups {
	return &FederateGroups{StreamUpdate: &Health{Status: statusServing}, Groups: slices.Clone(f.OutboundGroups), Provenance: m.provenance()}
}

// ---- local devices -> federates ----

// skippedType reports CoT that stays local: pings and TAK protocol control.
func skippedType(t string) bool {
	return strings.HasPrefix(t, "t-x-c-t") || strings.HasPrefix(t, "t-x-takp")
}

// sharedGroups is what of a device's groups a federate gets; ok is false
// when it gets nothing.
func sharedGroups(f store.Federate, groups []string) (shared []string, ok bool) {
	if f.SendMode != "groups" {
		return nil, true
	}
	for _, g := range groups {
		if slices.Contains(f.OutboundGroups, g) {
			shared = append(shared, g)
		}
	}
	return shared, len(shared) > 0
}

// hook sends what local devices send to the federates that should get it.
// Events from federates and from the server itself (src nil) stay local.
func (m *Manager) hook(src *router.Client, ev *cot.Event) bool {
	if src == nil || skippedType(ev.Type) {
		return true
	}
	sessions := m.connected()
	groups := src.OutGroups()
	if isContactSA(ev) {
		m.trackContact(ev, groups, sessions)
	}
	if len(sessions) == 0 {
		return true
	}
	var fe *FederatedEvent
	dests := ev.Dests()
	for _, s := range sessions {
		shared, ok := sharedGroups(s.fed, groups)
		if !ok {
			continue
		}
		// Direct messages only go to a federate one of their recipients is on.
		if len(dests) > 0 && !s.knowsAny(dests) {
			continue
		}
		if fe == nil {
			fe = toFederated(ev, nil, m.provenance())
		}
		out := *fe
		out.FederateGroups = shared
		s.enqueue(&out)
	}
	return true
}

func (m *Manager) connected() []*session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s.isConnected() {
			out = append(out, s)
		}
	}
	return out
}

// ---- contacts ----
//
// TAK Server only delivers CoT addressed to someone (direct messages, items
// sent to a contact) to a federate whose contact list has that user, so
// local devices are announced as ContactListEntry messages, as TAK Server
// announces its own users.

func (m *Manager) trackContact(ev *cot.Event, groups []string, sessions []*session) {
	c := &contact{uid: ev.UID, callsign: ev.Detail.Contact.Callsign, groups: groups, stale: ev.Stale}
	if c.stale.IsZero() {
		c.stale = time.Now().Add(10 * time.Minute)
	}
	m.cmu.Lock()
	old := m.contacts[c.uid]
	m.contacts[c.uid] = c
	m.cmu.Unlock()
	if old == nil || old.callsign != c.callsign || !slices.Equal(old.groups, c.groups) {
		m.announce(c, opCreate, sessions)
	}
}

func (m *Manager) announce(c *contact, op int64, sessions []*session) {
	for _, s := range sessions {
		shared, ok := sharedGroups(s.fed, c.groups)
		if !ok {
			continue
		}
		s.enqueue(&FederatedEvent{Contact: &ContactListEntry{Operation: op, UID: c.uid, Callsign: c.callsign},
			FederateGroups: shared, Provenance: m.provenance()})
	}
}

// replayContacts sends every current local contact to a federate that just
// connected.
func (m *Manager) replayContacts(s *session) {
	now := time.Now()
	m.cmu.Lock()
	var list []*contact
	for _, c := range m.contacts {
		if c.stale.After(now) {
			list = append(list, c)
		}
	}
	m.cmu.Unlock()
	for _, c := range list {
		m.announce(c, opCreate, []*session{s})
	}
}

// sweepContacts removes contacts that went stale or disconnected.
func (m *Manager) sweepContacts() {
	online := map[string]bool{}
	for _, c := range m.cfg.Router.Clients() {
		if uid, _ := c.Identity(); uid != "" {
			online[uid] = true
		}
	}
	now := time.Now()
	m.cmu.Lock()
	var gone []*contact
	for uid, c := range m.contacts {
		if !c.stale.After(now) || !online[uid] {
			gone = append(gone, c)
			delete(m.contacts, uid)
		}
	}
	m.cmu.Unlock()
	if len(gone) == 0 {
		return
	}
	sessions := m.connected()
	for _, c := range gone {
		m.announce(c, opDelete, sessions)
	}
}

// ---- federates -> local devices ----

// deliveryGroups is where a federate's events go: its inbound groups (none
// = every group), plus groups named like the remote federate groups.
func (m *Manager) deliveryGroups(f store.Federate, remote []string) []string {
	groups := slices.Clone(f.InboundGroups)
	if len(groups) == 0 {
		groups = append(groups, m.cfg.AnonGroup)
		for _, g := range m.cfg.Store.Groups() {
			groups = append(groups, g.Name)
		}
	}
	if f.MapRemoteGroups {
		groups = append(groups, remote...)
	}
	slices.Sort(groups)
	return slices.Compact(groups)
}

// handleInbound delivers an event from a federate to local devices.
func (m *Manager) handleInbound(s *session, fe *FederatedEvent) {
	if m.cfg.Log.Enabled(context.Background(), slog.LevelDebug) {
		what := "nothing"
		if fe.Event != nil {
			what = fe.Event.Type + " " + fe.Event.UID
		} else if fe.Contact != nil {
			what = "contact " + opName(fe.Contact.Operation) + " " + fe.Contact.UID
		}
		m.cfg.Log.Debug("federation: received", "federate", s.fed.Name, "what", what, "groups", fe.FederateGroups, "provenance", len(fe.Provenance))
	}
	if fe.HasProvenance(m.cfg.NodeID) {
		return // ours, come back around
	}
	if fe.Event == nil {
		if c := fe.Contact; c != nil {
			s.record("in", "contact "+opName(c.Operation), c.UID, c.Callsign, "")
			s.noteContact(c)
			if c.Operation == opDelete && c.UID != "" {
				// TAK Server puts the deleted item's CoT type in the callsign field.
				linkType := "a-f-G-U-C"
				if t := c.Callsign; len(t) > 2 && strings.Contains("abtu", t[:1]) && t[1] == '-' {
					linkType = t
				}
				if ev, err := deleteEvent(c.UID, linkType); err == nil {
					m.cfg.Router.Inject(ev, m.deliveryGroups(s.fed, fe.FederateGroups))
				}
			}
		}
		return
	}
	ev, err := fromFederated(fe)
	if err != nil || ev == nil {
		if err != nil {
			s.setWarning("an event could not be read: " + err.Error())
		}
		return
	}
	s.countIn()
	if isContactSA(ev) {
		s.noteRemote(ev.UID, ev.Detail.Contact.Callsign)
	}
	groups := m.deliveryGroups(s.fed, fe.FederateGroups)
	m.cfg.Router.Inject(ev, groups)
	if !isContactSA(ev) {
		route := "to groups " + strings.Join(groups, ", ")
		if d := ev.Dests(); len(d) > 0 {
			route = "to " + destNames(d)
		}
		name := ""
		if ev.Detail.Contact != nil {
			name = ev.Detail.Contact.Callsign
		}
		s.record("in", ev.Type, ev.UID, name, route)
	}
}

func destNames(ds []cot.Dest) string {
	var out []string
	for _, d := range ds {
		out = append(out, firstNonEmpty(d.Callsign, d.UID, d.Mission))
	}
	return strings.Join(out, ", ")
}

func opName(op int64) string {
	switch op {
	case opCreate:
		return "CREATE"
	case opUpdate:
		return "UPDATE"
	case opDelete:
		return "DELETE"
	}
	return "READ"
}

// ---- inbound authorization ----

// Fingerprint is the SHA-256 fingerprint of a DER certificate, upper case hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// inboundSession finds the approved inbound federate a client certificate
// belongs to: by fingerprint, or by common name when issued by the
// federate's CA.
func (m *Manager) inboundSession(cert *x509.Certificate) *session {
	fp := Fingerprint(cert.Raw)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		f := s.fed
		if f.Direction != Inbound {
			continue
		}
		if f.Fingerprint != "" {
			if f.Fingerprint == fp {
				return s
			}
			continue
		}
		if f.ClientCN == "" || cert.Subject.CommonName != f.ClientCN {
			continue
		}
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM([]byte(f.CAPEM)) {
			if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err == nil {
				return s
			}
		}
	}
	return nil
}

func (m *Manager) recordPending(cert *x509.Certificate, remote string) {
	fp := Fingerprint(cert.Raw)
	m.mu.Lock()
	_, seen := m.pending[fp]
	m.pending[fp] = Pending{Fingerprint: fp, CommonName: cert.Subject.CommonName, Subject: cert.Subject.String(),
		Issuer: cert.Issuer.String(), Remote: remote, LastSeen: time.Now(), certDER: cert.Raw}
	m.mu.Unlock()
	if !seen {
		m.cfg.Log.Warn("federation: rejected a server that is not approved; approve it on the Federation tab",
			"remote", remote, "common_name", cert.Subject.CommonName, "fingerprint", fp)
	}
}

// TakePending removes a pending server, for approval or dismissal.
func (m *Manager) TakePending(fp string) (Pending, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[fp]
	delete(m.pending, fp)
	return p, ok
}

// ---- status ----

// Status is the federation state for the admin console.
type Status struct {
	Listener  ListenerStatus           `json:"listener"`
	Federates map[string]SessionStatus `json:"federates"` // by federate ID, enabled ones
	Pending   []Pending                `json:"pending"`
	Contacts  int                      `json:"contacts"`
}

type ListenerStatus struct {
	Enabled bool   `json:"enabled"`
	Port    int    `json:"port"`
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`
}

func (m *Manager) Status() Status {
	settings := m.cfg.Store.FederationSettings()
	st := Status{Federates: map[string]SessionStatus{}, Pending: []Pending{},
		Listener: ListenerStatus{Enabled: settings.ListenEnabled, Port: settings.ListenPort}}
	m.mu.Lock()
	if l := m.listener; l != nil {
		st.Listener.Running, st.Listener.Error = l.err == "", l.err
	}
	for id, s := range m.sessions {
		st.Federates[id] = s.status()
	}
	for fp, p := range m.pending {
		if time.Since(p.LastSeen) > pendingFor {
			delete(m.pending, fp)
			continue
		}
		st.Pending = append(st.Pending, p)
	}
	m.mu.Unlock()
	slices.SortFunc(st.Pending, func(a, b Pending) int { return b.LastSeen.Compare(a.LastSeen) })
	m.cmu.Lock()
	st.Contacts = len(m.contacts)
	m.cmu.Unlock()
	return st
}

// ---- helpers ----

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func uuidString() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// ListenerAddr is the address the inbound listener is bound to ("" when it
// is not running).
func (m *Manager) ListenerAddr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listener == nil || m.listener.err != "" {
		return ""
	}
	return m.listener.addr
}
