package federation

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// The inbound listener: other servers connect here with mutual TLS. Any
// client certificate completes the handshake (proving the server holds its
// key); each call is then allowed only for an approved federate, matched by
// fingerprint or by common name and CA. Unknown servers are listed as
// pending so an admin can approve them.

type listener struct {
	port int
	addr string // as bound
	srv  *grpc.Server
	err  string
}

func (l *listener) stop() {
	if l.srv != nil {
		l.srv.Stop()
	}
}

func (m *Manager) startListener(port int) *listener {
	l := &listener{port: port}
	addr := net.JoinHostPort("", strconv.Itoa(port))
	ln, err := m.cfg.Listen(addr)
	if err != nil {
		l.err = err.Error()
		m.cfg.Log.Error("federation listener", "addr", addr, "err", err)
		return l
	}
	l.addr = ln.Addr().String()
	tlsCfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		ClientAuth:     tls.RequireAnyClientCert,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return m.cfg.Cert(), nil },
	}
	l.srv = grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ForceServerCodec(codec{}),
		grpc.MaxRecvMsgSize(maxMessage), grpc.MaxSendMsgSize(maxMessage),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 20 * time.Second}),
	)
	l.srv.RegisterService(&serviceDesc, &inbound{m: m})
	go func() {
		if err := l.srv.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			m.cfg.Log.Error("federation listener stopped", "err", err)
		}
	}()
	m.cfg.Log.Info("federation listener started", "port", port)
	return l
}

// inbound implements the FederatedChannel service.
type inbound struct{ m *Manager }

// session authorizes a call: the peer's certificate must belong to an
// approved inbound federate.
func (h *inbound) session(ctx context.Context) (*session, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return nil, status.Error(codes.Unauthenticated, "a client certificate is required")
	}
	cert := info.State.PeerCertificates[0]
	s := h.m.inboundSession(cert)
	if s == nil {
		h.m.recordPending(cert, p.Addr.String())
		return nil, status.Error(codes.PermissionDenied, "this server has not been approved on the EdgeTAK Federation tab")
	}
	return s, nil
}

func (h *inbound) sendOneEvent(ctx context.Context, ev *FederatedEvent) error {
	s, err := h.session(ctx)
	if err != nil {
		return err
	}
	h.m.handleInbound(s, ev)
	return nil
}

func (h *inbound) getIdentity(ctx context.Context) (*Identity, error) {
	if _, err := h.session(ctx); err != nil {
		return nil, err
	}
	id := h.m.subscription().Identity
	return &id, nil
}

func (h *inbound) healthCheck(ctx context.Context) (*Health, error) {
	s, err := h.session(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.lastHealth = time.Now()
	s.mu.Unlock()
	return &Health{Status: statusServing}, nil
}

// getAuthToken: TAK Server 5.6 clients may trade their certificate for a
// token and send it later. The certificate already authenticates them here,
// so the token is a formality.
func (h *inbound) getAuthToken(ctx context.Context) (*TokenResponse, error) {
	if _, err := h.session(ctx); err != nil {
		return nil, err
	}
	b := make([]byte, 32)
	rand.Read(b)
	return &TokenResponse{Token: base64.RawURLEncoding.EncodeToString(b)}, nil
}

func (h *inbound) getX509Identity(ctx context.Context) (*BinaryBlob, error) {
	if _, err := h.session(ctx); err != nil {
		return nil, err
	}
	cert := h.m.cfg.Cert()
	if cert == nil || len(cert.Certificate) == 0 {
		return &BinaryBlob{}, nil
	}
	return &BinaryBlob{Data: cert.Certificate[0]}, nil
}

// clientEventStream sends this server's events to the federate.
func (h *inbound) clientEventStream(sub *Subscription, stream grpc.ServerStream) error {
	s, err := h.session(stream.Context())
	if err != nil {
		return err
	}
	s.mu.Lock()
	id := sub.Identity
	s.identity = &id
	s.gen++
	gen := s.gen
	s.mu.Unlock()
	s.clearQueue()
	s.setState("connected", "")
	h.m.replayContacts(s)
	defer func() {
		s.mu.Lock()
		current := s.gen == gen
		s.mu.Unlock()
		if current {
			s.setState("disconnected", "")
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-s.done:
			return status.Error(codes.Unavailable, "the federate was changed or removed on EdgeTAK")
		case <-ticker.C:
			s.mu.Lock()
			replaced := s.gen != gen
			s.mu.Unlock()
			if replaced {
				return nil // a newer stream took over
			}
		case fe := <-s.queue:
			if err := stream.SendMsg(fe); err != nil {
				return err
			}
			s.recordSent(fe)
		}
	}
}

// serverEventStream receives the federate's events.
func (h *inbound) serverEventStream(stream grpc.ServerStream) error {
	s, err := h.session(stream.Context())
	if err != nil {
		return err
	}
	for {
		var fe FederatedEvent
		err := stream.RecvMsg(&fe)
		if err == io.EOF {
			return stream.SendMsg(h.m.subscription())
		}
		if err != nil {
			return err
		}
		h.m.handleInbound(s, &fe)
	}
}

// serverGroupsStream tells the federate what this server shares with it.
func (h *inbound) serverGroupsStream(_ *Subscription, stream grpc.ServerStream) error {
	s, err := h.session(stream.Context())
	if err != nil {
		return err
	}
	if err := stream.SendMsg(h.m.groupsMessage(s.fed)); err != nil {
		return err
	}
	select {
	case <-stream.Context().Done():
	case <-s.done:
	}
	return nil
}

// clientGroupsStream receives what the federate shares.
func (h *inbound) clientGroupsStream(stream grpc.ServerStream) error {
	s, err := h.session(stream.Context())
	if err != nil {
		return err
	}
	for {
		var fg FederateGroups
		err := stream.RecvMsg(&fg)
		if err == io.EOF {
			return stream.SendMsg(h.m.subscription())
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.groups = fg.Groups
		s.mu.Unlock()
	}
}

// clientROLStream sends files and their announcements to the federate.
func (h *inbound) clientROLStream(stream grpc.ServerStream) error {
	s, err := h.session(stream.Context())
	if err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-s.done:
			return status.Error(codes.Unavailable, "the federate was changed or removed on EdgeTAK")
		case r := <-s.rols:
			if err := stream.SendMsg(r); err != nil {
				return err
			}
		}
	}
}

// serverROLStream receives the federate's files and announcements.
func (h *inbound) serverROLStream(stream grpc.ServerStream) error {
	s, err := h.session(stream.Context())
	if err != nil {
		return err
	}
	for {
		var rol ROL
		err := stream.RecvMsg(&rol)
		if err == io.EOF {
			return stream.SendMsg(h.m.subscription())
		}
		if err != nil {
			return err
		}
		h.m.handleROL(s, &rol)
	}
}

// drainStream accepts binary streams, which EdgeTAK does not use, so the
// other side does not treat the federate as broken.
func (h *inbound) drainStream(stream grpc.ServerStream, reply message) error {
	if _, err := h.session(stream.Context()); err != nil {
		return err
	}
	for {
		if err := stream.RecvMsg(&Raw{}); err != nil {
			if err == io.EOF {
				return stream.SendMsg(reply)
			}
			return err
		}
	}
}

// PeerCertificate returns the certificate of a pending server, PEM encoded,
// so it can be approved by fingerprint.
func (p Pending) Certificate() *x509.Certificate {
	c, _ := x509.ParseCertificate(p.certDER)
	return c
}
