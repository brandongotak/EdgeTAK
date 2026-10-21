package federation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

const (
	reconnectMin    = 5 * time.Second
	reconnectMax    = 2 * time.Minute
	duplicateWait   = 30 * time.Second // TAK Server keeps a dropped federate for 15 s and rejects it as a duplicate until then
	connectTimeout  = 20 * time.Second
	healthEvery     = 5 * time.Second // TAK Server drops streams after 15 s without a health check
	firstHealthWait = time.Second
	remoteForget    = 30 * time.Minute
)

// session is the connection to one federate, in either direction.
type session struct {
	m   *Manager
	fed store.Federate

	queue chan *FederatedEvent
	done  chan struct{} // closed by stop
	once  sync.Once

	mu         sync.Mutex
	state      string // disconnected, connecting, connected
	lastErr    string
	warning    string
	since      time.Time
	lastHealth time.Time
	in, out    uint64
	dropped    uint64
	recent     []Recent
	identity   *Identity
	groups     []string             // the groups the other side shares
	remote     map[string]time.Time // UIDs and callsigns of the other side's users
	gen        int                  // inbound: the active ClientEventStream
}

// Recent is a message shown on the Federation tab (not position reports).
type Recent struct {
	Time      time.Time `json:"time"`
	Direction string    `json:"direction"`
	Type      string    `json:"type"`
	UID       string    `json:"uid"`
	Name      string    `json:"name,omitempty"`
	Route     string    `json:"route,omitempty"`
}

// SessionStatus is one federate's state for the admin console.
type SessionStatus struct {
	State          string    `json:"state"`
	LastError      string    `json:"last_error,omitempty"`
	Warning        string    `json:"warning,omitempty"`
	Since          time.Time `json:"since,omitzero"`
	LastHealth     time.Time `json:"last_health,omitzero"`
	In             uint64    `json:"in"`
	Out            uint64    `json:"out"`
	Dropped        uint64    `json:"dropped"`
	Queued         int       `json:"queued"`
	Recent         []Recent  `json:"recent"`
	RemoteName     string    `json:"remote_name,omitempty"`
	RemoteServerID string    `json:"remote_server_id,omitempty"`
	RemoteGroups   []string  `json:"remote_groups"`
	RemoteContacts int       `json:"remote_contacts"`
}

func newSession(m *Manager, f store.Federate) *session {
	return &session{m: m, fed: f, queue: make(chan *FederatedEvent, queueSize), done: make(chan struct{}),
		state: "disconnected", remote: map[string]time.Time{}}
}

func (s *session) stop() { s.once.Do(func() { close(s.done) }) }

func (s *session) stopped() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *session) isConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == "connected"
}

// enqueue queues an event; when the queue is full the oldest is dropped,
// since fresh positions matter more.
func (s *session) enqueue(fe *FederatedEvent) {
	for {
		select {
		case s.queue <- fe:
			return
		default:
		}
		select {
		case <-s.queue:
			s.mu.Lock()
			s.dropped++
			s.mu.Unlock()
		default:
		}
	}
}

func (s *session) clearQueue() {
	for {
		select {
		case <-s.queue:
		default:
			return
		}
	}
}

func (s *session) setState(state, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.state
	s.state = state
	switch state {
	case "connected":
		s.lastErr, s.warning, s.since = "", "", time.Now()
	case "disconnected":
		s.since = time.Time{}
		if err != "" {
			s.lastErr = err
		}
	}
	if was == "connected" && state != "connected" {
		s.m.cfg.Log.Info("federate disconnected", "federate", s.fed.Name, "err", err)
	} else if state == "connected" && was != "connected" {
		s.m.cfg.Log.Info("federate connected", "federate", s.fed.Name)
	}
}

func (s *session) setWarning(w string) {
	s.mu.Lock()
	s.warning = w
	s.mu.Unlock()
}

func (s *session) countIn() {
	s.mu.Lock()
	s.in++
	s.mu.Unlock()
}

func (s *session) record(dir, typ, uid, name, route string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent = append([]Recent{{Time: time.Now(), Direction: dir, Type: typ, UID: uid, Name: name, Route: route}}, s.recent...)
	if len(s.recent) > recentMessages {
		s.recent = s.recent[:recentMessages]
	}
}

// recordSent counts an event going out, and lists it unless it is a
// position report.
func (s *session) recordSent(fe *FederatedEvent) {
	s.mu.Lock()
	s.out++
	s.mu.Unlock()
	if c := fe.Contact; c != nil {
		s.record("out", "contact "+opName(c.Operation), c.UID, c.Callsign, "")
		return
	}
	g := fe.Event
	if g == nil || (strings.HasPrefix(g.Type, "a-") && strings.Contains(g.Other, "endpoint=")) {
		return
	}
	route := ""
	if len(g.PtpCallsigns)+len(g.PtpUIDs) > 0 {
		route = "to " + strings.Join(append(append([]string{}, g.PtpCallsigns...), g.PtpUIDs...), ", ")
	} else if len(fe.FederateGroups) > 0 {
		route = "groups " + strings.Join(fe.FederateGroups, ", ")
	}
	s.record("out", g.Type, g.UID, g.ScreenName, route)
}

// noteRemote remembers a user on the other side, so direct messages to them
// are sent there.
func (s *session) noteRemote(uid, callsign string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if uid != "" {
		s.remote["uid:"+uid] = now
	}
	if callsign != "" {
		s.remote["cs:"+callsign] = now
	}
}

func (s *session) noteContact(c *ContactListEntry) {
	if c.Operation == opDelete {
		s.mu.Lock()
		delete(s.remote, "uid:"+c.UID)
		s.mu.Unlock()
		return
	}
	s.noteRemote(c.UID, c.Callsign)
}

func (s *session) knowsAny(dests []cot.Dest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-remoteForget)
	for k, t := range s.remote {
		if t.Before(cutoff) {
			delete(s.remote, k)
		}
	}
	for _, d := range dests {
		if (d.UID != "" && !s.remote["uid:"+d.UID].IsZero()) || (d.Callsign != "" && !s.remote["cs:"+d.Callsign].IsZero()) {
			return true
		}
	}
	return false
}

func (s *session) status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SessionStatus{State: s.state, LastError: s.lastErr, Warning: s.warning, Since: s.since, LastHealth: s.lastHealth,
		In: s.in, Out: s.out, Dropped: s.dropped, Queued: len(s.queue), Recent: append([]Recent{}, s.recent...),
		RemoteGroups: append([]string{}, s.groups...)}
	if s.identity != nil {
		st.RemoteName, st.RemoteServerID = s.identity.Name, s.identity.ServerID
	}
	for k := range s.remote {
		if strings.HasPrefix(k, "uid:") {
			st.RemoteContacts++
		}
	}
	return st
}

// ---- outbound ----

// runOutbound keeps connecting to the federate until the session stops.
func (s *session) runOutbound() {
	backoff := reconnectMin
	for !s.stopped() {
		s.setState("connecting", "")
		started := time.Now()
		err := s.connect()
		if s.stopped() {
			s.setState("disconnected", "")
			return
		}
		msg := describe(err)
		s.setState("disconnected", msg)
		s.m.cfg.Log.Warn("federate connection ended", "federate", s.fed.Name, "err", msg)
		wait := backoff
		if time.Since(started) > time.Minute { // it was up for a while: start the backoff over
			backoff, wait = reconnectMin, reconnectMin
		} else {
			backoff = min(backoff*2, reconnectMax)
		}
		if strings.Contains(strings.ToLower(msg), "duplicate") {
			wait = max(wait, duplicateWait)
		}
		select {
		case <-s.done:
			s.setState("disconnected", "")
			return
		case <-time.After(wait):
		}
	}
}

// describe makes gRPC errors readable, with hints for the usual causes.
func describe(err error) string {
	if err == nil {
		return "the other server closed the connection"
	}
	st, ok := status.FromError(err)
	if !ok {
		return err.Error()
	}
	msg := st.Code().String() + ": " + st.Message()
	switch st.Code() {
	case codes.PermissionDenied:
		msg += " (has the other server approved this federate and given it groups?)"
	case codes.Unauthenticated:
		msg += " (the federation token was rejected: it may have expired or been deleted)"
	case codes.Unavailable:
		if strings.Contains(st.Message(), "certificate") || strings.Contains(st.Message(), "tls") {
			msg += " (check that each server trusts the other's CA)"
		}
	}
	return msg
}

// bearer sends a federation token with every call.
type bearer struct {
	token  string
	secure bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearer) RequireTransportSecurity() bool { return b.secure }

// clientTLS trusts only the federate's CA. The hostname is checked when a
// TLS server name is set (TAK Server certificates often don't name the
// address used to reach them).
func (s *session) clientTLS(withCert bool) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(s.fed.CAPEM)) {
		return nil, errors.New("the federate's CA certificate is missing or not PEM")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, // verified below, against the pinned CA
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the server sent no certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			opts := x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			if s.fed.TLSServerName != "" {
				opts.DNSName = s.fed.TLSServerName
			}
			if _, err := cs.PeerCertificates[0].Verify(opts); err != nil {
				return fmt.Errorf("the server's certificate is not issued by the CA saved for this federate: %w", err)
			}
			return nil
		}}
	if withCert {
		cert := s.m.cfg.Cert()
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert, nil }
	}
	return cfg, nil
}

func (s *session) dialOptions() ([]grpc.DialOption, error) {
	opts := []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) { return s.m.cfg.Dial(addr) }),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessage), grpc.MaxCallSendMsgSize(maxMessage)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 20 * time.Second, PermitWithoutStream: true}),
	}
	token := s.fed.AuthMode == "token"
	if token {
		opts = append(opts, grpc.WithPerRPCCredentials(bearer{token: s.fed.Token, secure: s.fed.UseTLS}))
	}
	if token && !s.fed.UseTLS {
		return append(opts, grpc.WithTransportCredentials(insecure.NewCredentials())), nil
	}
	cfg, err := s.clientTLS(!token)
	if err != nil {
		return nil, err
	}
	return append(opts, grpc.WithTransportCredentials(credentials.NewTLS(cfg))), nil
}

// connect runs one connection, in the order TAK Server's own federation
// client uses (TakFigClient): our group stream; the server's group stream
// and the event stream from it; the event stream to it once its group
// stream answers (TAK Server registers the federate then); health checks.
func (s *session) connect() error {
	if s.fed.AuthMode == "token" && s.fed.TokenExpiration > 0 && s.fed.TokenExpiration < time.Now().UnixMilli() {
		return errors.New("the federation token has expired; generate a new one")
	}
	opts, err := s.dialOptions()
	if err != nil {
		return err
	}
	target := net.JoinHostPort(s.fed.Address, strconv.Itoa(s.fed.Port))
	cc, err := grpc.NewClient("passthrough:///"+target, opts...)
	if err != nil {
		return err
	}
	defer cc.Close()
	if err := waitReady(cc, s.done); err != nil {
		if why := s.diagnose(target); why != "" {
			return errors.New(why)
		}
		return err
	}
	debug := func(msg string, args ...any) {
		s.m.cfg.Log.Debug("federation: "+msg, append([]any{"federate", s.fed.Name}, args...)...)
	}
	debug("connected", "target", target)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.done:
		case <-ctx.Done():
		}
		cancel()
	}()

	fail := make(chan error, 4)
	failed := func(err error) {
		select {
		case fail <- err:
		default:
		}
	}
	// A new UID for every connection, like TAK Server's own client.
	sub := &Subscription{Identity: s.m.identity(identityTakClient, uuidString()), Version: s.m.version()}

	ourGroups, err := openClientStream(ctx, cc, "ClientFederateGroupsStream")
	if err != nil {
		return err
	}
	if err := ourGroups.SendMsg(s.m.groupsMessage(s.fed)); err != nil {
		return err
	}
	theirGroups, err := openServerStream(ctx, cc, "ServerFederateGroupsStream", sub)
	if err != nil {
		return err
	}
	events, err := openServerStream(ctx, cc, "ClientEventStream", sub)
	if err != nil {
		return err
	}
	groupsReady := make(chan struct{})
	var readyOnce sync.Once
	go func() {
		defer readyOnce.Do(func() { close(groupsReady) })
		for {
			var fg FederateGroups
			if err := theirGroups.RecvMsg(&fg); err != nil {
				debug("group stream from the other server ended", "err", err)
				return // TAK Server's client goes on without it
			}
			debug("groups from the other server", "groups", fg.Groups)
			readyOnce.Do(func() { close(groupsReady) })
			if len(fg.Groups) > 0 {
				s.mu.Lock()
				s.groups = fg.Groups
				s.mu.Unlock()
			}
		}
	}()
	go func() {
		for {
			var fe FederatedEvent
			if err := events.RecvMsg(&fe); err != nil {
				debug("event stream from the other server ended", "err", err)
				failed(err)
				return
			}
			s.m.handleInbound(s, &fe)
		}
	}()
	select {
	case <-groupsReady:
	case <-time.After(connectTimeout):
		s.setWarning("no answer on the group stream; opening the event stream anyway")
	case err := <-fail:
		return err
	case <-ctx.Done():
		return nil
	}
	send, err := openClientStream(ctx, cc, "ServerEventStream")
	if err != nil {
		return err
	}
	debug("event stream to the other server opened")
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case fe := <-s.queue:
				if err := send.SendMsg(fe); err != nil {
					failed(err)
					return
				}
				s.recordSent(fe)
			}
		}
	}()

	// Health checks: the first after a second, then regularly; the
	// connection counts once the other side answers SERVING.
	started := time.Now()
	healthy := false
	wait := firstHealthWait
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-fail:
			return err
		case <-time.After(wait):
		}
		hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
		var h Health
		err := invoke(hctx, cc, "HealthCheck", &Health{Status: statusServing}, &h)
		hcancel()
		debug("health check", "status", healthName(h.Status), "err", err)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.lastHealth = time.Now()
		s.mu.Unlock()
		if h.Status == statusServing {
			if !healthy {
				healthy = true
				s.clearQueue()
				s.setState("connected", "")
				s.m.replayContacts(s)
				var id Identity // the other server's name, for the console
				ictx, icancel := context.WithTimeout(ctx, 10*time.Second)
				err := invoke(ictx, cc, "getIdentity", &Empty{}, &id)
				icancel()
				if err == nil && id.Name != "" {
					s.mu.Lock()
					s.identity = &id
					s.mu.Unlock()
				}
				// Missions and files (ROL) are not federated yet; TAK Server's
				// clients keep this stream open, so EdgeTAK does too.
				if rol, err := openServerStream(ctx, cc, "ClientROLStream", sub); err == nil {
					go func() {
						for {
							if rol.RecvMsg(&Raw{}) != nil {
								return
							}
						}
					}()
				}
			}
			s.setWarning("")
			wait = healthEvery
			continue
		}
		if healthy || time.Since(started) > connectTimeout {
			return fmt.Errorf("the other server reports this federation connection as %s", healthName(h.Status))
		}
		s.setWarning("waiting for the other server to accept the connection (" + healthName(h.Status) + ")")
		wait = firstHealthWait
	}
}

func healthName(st int64) string {
	switch st {
	case statusServing:
		return "SERVING"
	case 2:
		return "NOT_SERVING"
	case statusNotConnected:
		return "NOT_CONNECTED"
	}
	return "UNKNOWN"
}

// waitReady waits for the connection to come up (TLS included).
func waitReady(cc *grpc.ClientConn, done <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	cc.Connect()
	for {
		st := cc.GetState()
		if st == connectivity.Ready {
			return nil
		}
		if !cc.WaitForStateChange(ctx, st) {
			if st == connectivity.TransientFailure {
				return errors.New("could not connect: check the address, port and firewall, and that each server trusts the other's CA")
			}
			return errors.New("timed out connecting: check the address, port and firewall")
		}
	}
}

// diagnose repeats the TLS handshake on its own to find out why gRPC could
// not connect, since gRPC reports TLS failures as a plain timeout.
func (s *session) diagnose(target string) string {
	conn, err := s.m.cfg.Dial(target)
	if err != nil {
		return "could not connect to " + target + ": " + err.Error() + " (check the address, port and firewall)"
	}
	defer conn.Close()
	if s.fed.AuthMode == "token" && !s.fed.UseTLS {
		return ""
	}
	cfg, err := s.clientTLS(s.fed.AuthMode != "token")
	if err != nil {
		return err.Error()
	}
	cfg.NextProtos = []string{"h2"}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	tc := tls.Client(conn, cfg)
	if err := tc.Handshake(); err != nil {
		msg := "TLS handshake with " + target + " failed: " + err.Error()
		if strings.Contains(err.Error(), "bad certificate") || strings.Contains(err.Error(), "unknown certificate") {
			msg += " (the other server does not trust EdgeTAK's CA yet)"
		}
		return msg
	}
	// TLS 1.3 servers reject a client certificate after the handshake; a
	// short read shows that.
	tc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := tc.Read(make([]byte, 1)); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		var ne net.Error
		if !(errors.As(err, &ne) && ne.Timeout()) && err != io.EOF {
			return "the other server closed the TLS connection: " + err.Error() + " (it may not trust EdgeTAK's CA)"
		}
	}
	return ""
}
