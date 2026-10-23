package app

import (
	"context"
	"net"
	"sync"
)

// plainTCP opens and closes the plain TCP streaming port while EdgeTAK
// runs (Settings → Plain TCP), always on the same address.
type plainTCP struct {
	mu     sync.Mutex
	parent context.Context
	addr   string
	listen func(addr string) (net.Listener, error)
	serve  func(ctx context.Context, ln net.Listener)
	// dropClients disconnects the devices connected over plain TCP.
	dropClients func()
	cancel      context.CancelFunc // nil while off
}

// start serves ln (already bound) until the port is turned off.
func (p *plainTCP) start(ln net.Listener) {
	ctx, cancel := context.WithCancel(p.parent)
	p.cancel, p.addr = cancel, ln.Addr().String()
	go p.serve(ctx, ln)
}

// Addr is where it listens (or will, once turned on).
func (p *plainTCP) Addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addr
}

func (p *plainTCP) On() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancel != nil
}

// Set opens or closes the port.
func (p *plainTCP) Set(on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if on == (p.cancel != nil) {
		return nil
	}
	if !on {
		p.cancel() // closes the listener
		p.cancel = nil
		p.dropClients()
		return nil
	}
	ln, err := p.listen(p.addr)
	if err != nil {
		return err
	}
	p.start(ln)
	return nil
}
