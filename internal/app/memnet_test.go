package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
)

// memNet is an in-memory network for tests. Some desktop antivirus products
// (e.g. AVG Web Shield) transparently intercept local TLS connections and
// re-sign them, which breaks mutual TLS; pipes avoid the OS socket layer.
type memNet struct {
	mu        sync.Mutex
	next      int
	listeners map[string]*memListener
}

var mem = &memNet{next: 40000, listeners: map[string]*memListener{}}

func (m *memNet) Listen(_, address string) (net.Listener, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A port memnet handed out before can be listened on again (a listener
	// reopened at its old address); anything else gets a new port.
	port := 0
	if _, p, err := net.SplitHostPort(address); err == nil {
		port, _ = strconv.Atoi(p)
	}
	if port <= 40000 || port > m.next {
		m.next++
		port = m.next
	} else if _, taken := m.listeners[(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}).String()]; taken {
		return nil, fmt.Errorf("memnet: address in use: %s", address)
	}
	l := &memListener{net: m, addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}, conns: make(chan net.Conn), done: make(chan struct{})}
	m.listeners[l.addr.String()] = l
	return l, nil
}

func (m *memNet) Dial(addr string) (net.Conn, error) {
	m.mu.Lock()
	l, ok := m.listeners[addr]
	m.next++
	clientAddr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: m.next}
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("memnet: connection refused: %s", addr)
	}
	c, s := net.Pipe()
	select {
	case l.conns <- &memConn{Conn: s, local: l.addr, remote: clientAddr}:
		return &memConn{Conn: c, local: clientAddr, remote: l.addr}, nil
	case <-l.done:
		return nil, fmt.Errorf("memnet: connection refused: %s", addr)
	}
}

type memListener struct {
	net   *memNet
	addr  *net.TCPAddr
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.net.mu.Lock()
		delete(l.net.listeners, l.addr.String())
		l.net.mu.Unlock()
	})
	return nil
}

func (l *memListener) Addr() net.Addr { return l.addr }

type memConn struct {
	net.Conn
	local, remote net.Addr
}

func (c *memConn) LocalAddr() net.Addr  { return c.local }
func (c *memConn) RemoteAddr() net.Addr { return c.remote }

func tlsDial(addr string, cfg *tls.Config) (*tls.Conn, error) {
	raw, err := mem.Dial(addr)
	if err != nil {
		return nil, err
	}
	c := tls.Client(raw, cfg)
	if err := c.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	return c, nil
}

func memTransport(cfg *tls.Config) *http.Transport {
	return &http.Transport{
		TLSClientConfig: cfg,
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			return mem.Dial(addr)
		},
	}
}
