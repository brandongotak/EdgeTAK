package mesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// UDP is the real network: it joins ATAK's multicast groups on each
// interface, and listens on the direct port (UDP and TCP) for messages
// mesh devices send straight to a contact.
type UDP struct {
	Log *slog.Logger

	mu      sync.Mutex
	closers []io.Closer
	send    *ipv4.PacketConn
	ifaces  map[string]*net.Interface
}

func (u *UDP) Open(ctx context.Context, set store.MeshSettings, recv func(Packet)) ([]Interface, error) {
	u.Close()
	var chosen []Interface
	byName := map[string]*net.Interface{}
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range all {
		ifc := &all[i]
		usable := ifc.Flags&net.FlagUp != 0 && ifc.Flags&net.FlagMulticast != 0 && ifc.Flags&net.FlagLoopback == 0
		if len(set.Interfaces) > 0 {
			usable = containsString(set.Interfaces, ifc.Name)
		}
		if !usable {
			continue
		}
		in := Interface{Name: ifc.Name, Addr: ipv4Of(ifc)}
		if in.Addr == "" {
			in.Error = "no IPv4 address"
		} else if ifc.Flags&net.FlagUp == 0 {
			in.Error = "down"
		}
		chosen = append(chosen, in)
		byName[ifc.Name] = ifc
	}
	for _, name := range set.Interfaces {
		if byName[name] == nil {
			chosen = append(chosen, Interface{Name: name, Error: "not found on this machine"})
		}
	}
	if len(chosen) == 0 {
		return nil, errors.New("no network interface here can multicast")
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	u.ifaces = byName
	for _, g := range []struct{ addr, port string }{{set.SAAddr, "sa"}, {set.ChatAddr, "chat"}} {
		group, err := net.ResolveUDPAddr("udp4", g.addr)
		if err != nil || !group.IP.IsMulticast() {
			u.closeLocked()
			return chosen, fmt.Errorf("%s is not a multicast address and port", g.addr)
		}
		c, err := listenReuse(ctx, "udp4", ":"+strconv.Itoa(group.Port))
		if err != nil {
			u.closeLocked()
			return chosen, fmt.Errorf("listening on port %d: %w", group.Port, err)
		}
		u.closers = append(u.closers, c)
		p := ipv4.NewPacketConn(c)
		for i := range chosen {
			if ifc := byName[chosen[i].Name]; ifc != nil && chosen[i].Error == "" {
				if err := p.JoinGroup(ifc, &net.UDPAddr{IP: group.IP}); err != nil {
					chosen[i].Error = "joining " + group.IP.String() + ": " + err.Error()
				}
			}
		}
		go u.read(c, g.port, recv)
	}
	if set.DirectPort > 0 {
		port := ":" + strconv.Itoa(set.DirectPort)
		if c, err := listenReuse(ctx, "udp4", port); err == nil {
			u.closers = append(u.closers, c)
			go u.read(c, "direct", recv)
		} else {
			u.Log.Warn("mesh bridge: can't receive direct messages over UDP", "port", set.DirectPort, "err", err)
		}
		if ln, err := net.Listen("tcp4", port); err == nil {
			u.closers = append(u.closers, ln)
			go u.accept(ln, recv)
		} else {
			u.Log.Warn("mesh bridge: can't receive direct messages over TCP", "port", set.DirectPort, "err", err)
		}
	}
	sc, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		u.closeLocked()
		return chosen, err
	}
	u.closers = append(u.closers, sc)
	u.send = ipv4.NewPacketConn(sc)
	u.send.SetMulticastTTL(set.TTL)
	// Loopback on, so ATAK or WinTAK on this machine hears the bridge; the
	// bridge drops its own datagrams when it hears them back.
	u.send.SetMulticastLoopback(true)
	go func() { <-ctx.Done(); u.Close() }()
	return chosen, nil
}

func (u *UDP) read(c net.PacketConn, port string, recv func(Packet)) {
	buf := make([]byte, 65536) // ATAK's largest datagram
	for {
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		recv(Packet{Data: append([]byte(nil), buf[:n]...), From: from, Port: port})
	}
}

// accept takes direct messages over TCP: a device connects and sends CoT
// (usually one XML event), then closes.
func (u *UDP) accept(ln net.Listener, recv func(Packet)) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(20 * time.Second))
			r := cot.NewReader(io.LimitReader(conn, 1<<20))
			for {
				ev, _, err := r.Next()
				if err != nil {
					return
				}
				recv(Packet{Data: ev.XML(), From: conn.RemoteAddr(), Port: "direct"})
			}
		}()
	}
}

func (u *UDP) Send(ifname, addr string, b []byte) error {
	dst, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.send == nil {
		return net.ErrClosed
	}
	ifc := u.ifaces[ifname]
	if ifc == nil {
		return fmt.Errorf("no interface %s", ifname)
	}
	if err := u.send.SetMulticastInterface(ifc); err != nil {
		return err
	}
	_, err = u.send.WriteTo(b, nil, dst)
	return err
}

func (u *UDP) Close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.closeLocked()
}

func (u *UDP) closeLocked() {
	for _, c := range u.closers {
		c.Close()
	}
	u.closers, u.send = nil, nil
}

// listenReuse binds a UDP port others (ATAK or WinTAK on this machine) may
// also have bound.
func listenReuse(ctx context.Context, network, addr string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = reuseAddr(fd) }); err != nil {
			return err
		}
		return serr
	}}
	return lc.ListenPacket(ctx, network, addr)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
