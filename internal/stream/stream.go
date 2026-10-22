// Package stream serves CoT streaming connections over plain TCP and mutual TLS.
package stream

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/pki"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

type Server struct {
	Router         *router.Router
	Store          *store.Store
	Log            *slog.Logger
	AnonymousGroup string
	IdleTimeout    time.Duration
	ServerUID      string
	Version        string
	// Ended is told when a client's stream ends, and whether the client closed
	// it (clean): turning the connection off, quitting ATAK or shutting the
	// phone down. A stream that times out or is reset was lost.
	Ended func(c *router.Client, clean bool)
}

// Serve accepts connections on ln until ctx is done. Pass a tls.Listener for
// the TLS port; client identity is then taken from the verified certificate.
func (s *Server) Serve(ctx context.Context, ln net.Listener, transport string) error {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		go s.handle(ctx, conn, transport)
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn, transport string) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}

	username, serial := "", ""
	in, out := []string{s.AnonymousGroup}, []string{s.AnonymousGroup}
	if tlsConn, ok := conn.(*tls.Conn); ok {
		conn.SetDeadline(time.Now().Add(15 * time.Second))
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			s.Log.Debug("tls handshake failed", "remote", remote, "err", err)
			return
		}
		conn.SetDeadline(time.Time{})
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) == 0 {
			return
		}
		leaf := state.PeerCertificates[0]
		if s.Store.IsRevoked(pki.SerialHex(leaf)) {
			s.Log.Warn("rejected revoked certificate", "remote", remote, "cn", leaf.Subject.CommonName)
			return
		}
		username, serial = leaf.Subject.CommonName, pki.SerialHex(leaf)
		if u, ok := s.Store.User(username); ok {
			in, out = u.Effective(s.AnonymousGroup)
		}
	}

	c := router.NewClient(remote, transport, username, in, out, func() { conn.Close() })
	c.CertSerial = serial
	defer c.Close()

	// The announcement goes out before anything else on the stream.
	conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err := cot.WriteFrame(conn, cot.ProtocolAnnouncement(s.ServerUID, s.Version), cot.FormatXML); err != nil {
		return
	}

	ctrl := make(chan control, 4)
	writerDone := make(chan struct{})
	go s.writeLoop(conn, c, ctrl, writerDone)

	s.Router.Register(c)
	defer s.Router.Unregister(c)

	sendCtrl := func(m control) {
		select {
		case ctrl <- m:
		case <-c.Done():
		}
	}

	r := cot.NewReader(conn)
	var readErr error
	defer func() {
		if s.Ended != nil {
			s.Ended(c, cleanEnd(readErr))
		}
	}()
	for {
		if s.IdleTimeout > 0 {
			conn.SetReadDeadline(time.Now().Add(s.IdleTimeout))
		}
		ev, _, err := r.Next()
		if err != nil {
			var me *cot.MalformedError
			if errors.As(err, &me) {
				s.Log.Debug("dropping malformed message", "remote", remote, "err", err)
				continue
			}
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.Log.Debug("read ended", "remote", remote, "err", err)
			}
			readErr = err
			break
		}
		c.In.Add(1)
		switch ev.Type {
		case cot.TypePing:
			c.Touch()
			sendCtrl(control{ev: cot.Pong()})
			continue
		case cot.TypeProtoRequest:
			v := ev.TakRequestVersion()
			ok := v == cot.ProtocolVersion
			if !ok {
				s.Log.Warn("client requested unsupported TAK protocol version; staying on XML", "id", c.ID, "user", username, "version", v)
			}
			sendCtrl(control{ev: cot.ProtocolResponse(s.ServerUID, ok), switchToProto: ok})
			continue
		}
		if cot.IsControl(ev.Type) {
			continue
		}
		c.Observe(ev)
		s.Router.Route(c, ev)
	}
	c.Close()
	<-writerDone
}

// control is a message the writer must never drop. If switchToProto is set,
// everything written after ev uses TAK protobuf framing.
type control struct {
	ev            *cot.Event
	switchToProto bool
}

func (s *Server) writeLoop(conn net.Conn, c *router.Client, ctrl <-chan control, done chan<- struct{}) {
	defer close(done)
	defer c.Close()
	w := bufio.NewWriterSize(conn, 64<<10)
	format := cot.FormatXML
	var buf []byte
	for {
		var ev *cot.Event
		var next cot.Format = format
		select {
		case m := <-ctrl:
			ev = m.ev
			if m.switchToProto {
				next = cot.FormatProto
			}
		case ev = <-c.Queue():
		case <-c.Done():
			return
		}
		buf = cot.AppendFrame(buf[:0], ev, format)
		if next != format {
			format = next
			c.Protobuf.Store(true)
			uid, cs := c.Identity()
			s.Log.Info("client switched to TAK protobuf", "id", c.ID, "remote", c.Remote, "user", c.Username, "uid", uid, "callsign", cs)
		}
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := w.Write(buf); err != nil {
			return
		}
		c.Out.Add(1)
		if len(c.Queue()) == 0 && len(ctrl) == 0 {
			if err := w.Flush(); err != nil {
				return
			}
		}
	}
}

// cleanEnd reports whether a stream ended because the client closed it (or
// EdgeTAK did), rather than going silent (timeout) or being reset.
func cleanEnd(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed)
}
