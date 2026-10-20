// Package tlstrace records the TLS record sequence of each connection until
// the handshake completes, and logs it when the handshake fails. It parses
// only plaintext record headers and handshake message types (never keys or
// application data) and exists to diagnose client interoperability issues.
package tlstrace

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
)

// Listener wraps a raw (pre-TLS) listener.
type Listener struct {
	net.Listener
	Log  *slog.Logger
	Name string
}

func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	t := &conn{Conn: c, log: l.Log, name: l.Name}
	t.in.dir, t.out.dir = "C>S", "S>C"
	return t, nil
}

const maxEvents = 48

type conn struct {
	net.Conn
	log  *slog.Logger
	name string

	mu       sync.Mutex
	in, out  parser
	events   []string
	finished bool // handshake done or trace already logged
}

func (c *conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.mu.Lock()
	if !c.finished {
		if n > 0 {
			c.in.feed(b[:n], c.add)
		}
		if err != nil {
			c.add(fmt.Sprintf("C>S closed (%v)", err))
			c.report("connection closed during TLS handshake")
		}
		c.checkDone()
	}
	c.mu.Unlock()
	return n, err
}

func (c *conn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if !c.finished {
		c.out.feed(b, c.add)
		c.checkDone()
	}
	c.mu.Unlock()
	return c.Conn.Write(b)
}

func (c *conn) add(ev string) {
	if len(c.events) < maxEvents {
		c.events = append(c.events, ev)
	}
	if strings.Contains(ev, "Alert(") {
		c.report("TLS handshake alert")
	}
}

func (c *conn) checkDone() {
	if c.in.appData && c.out.appData {
		c.finished = true
		c.events, c.in.buf, c.out.buf = nil, nil, nil
	}
}

func (c *conn) report(msg string) {
	if c.finished {
		return
	}
	c.finished = true
	c.log.Debug(msg, "listener", c.name, "remote", c.RemoteAddr().String(), "trace", strings.Join(c.events, " | "))
}

// parser splits one direction of the byte stream into TLS records.
type parser struct {
	dir       string
	buf       []byte
	encrypted bool
	appData   bool
}

var handshakeNames = map[byte]string{
	0: "HelloRequest", 1: "ClientHello", 2: "ServerHello", 4: "NewSessionTicket", 8: "EncryptedExtensions",
	11: "Certificate", 12: "ServerKeyExchange", 13: "CertificateRequest", 14: "ServerHelloDone",
	15: "CertificateVerify", 16: "ClientKeyExchange", 20: "Finished", 24: "KeyUpdate",
}

var alertNames = map[byte]string{
	0: "close_notify", 10: "unexpected_message", 20: "bad_record_mac", 40: "handshake_failure", 42: "bad_certificate",
	43: "unsupported_certificate", 44: "certificate_revoked", 45: "certificate_expired", 46: "certificate_unknown",
	47: "illegal_parameter", 48: "unknown_ca", 50: "decode_error", 51: "decrypt_error", 70: "protocol_version",
	71: "insufficient_security", 80: "internal_error", 90: "user_canceled", 100: "no_renegotiation",
	109: "missing_extension", 110: "unsupported_extension", 112: "unrecognized_name", 116: "certificate_required",
}

func (p *parser) feed(b []byte, add func(string)) {
	if p.appData && p.encrypted {
		return
	}
	p.buf = append(p.buf, b...)
	for len(p.buf) >= 5 {
		typ, ver := p.buf[0], binary.BigEndian.Uint16(p.buf[1:3])
		n := int(binary.BigEndian.Uint16(p.buf[3:5]))
		if typ < 20 || typ > 24 || n > 18432 {
			add(fmt.Sprintf("%s not TLS (first bytes %q)", p.dir, preview(p.buf)))
			p.buf, p.appData, p.encrypted = nil, true, true
			return
		}
		if len(p.buf) < 5+n {
			return
		}
		p.record(typ, ver, p.buf[5:5+n], add)
		p.buf = p.buf[5+n:]
	}
}

func (p *parser) record(typ byte, ver uint16, body []byte, add func(string)) {
	switch typ {
	case 20:
		add(p.dir + " ChangeCipherSpec")
		p.encrypted = true
	case 21:
		if !p.encrypted && len(body) >= 2 {
			add(fmt.Sprintf("%s Alert(%s %s)", p.dir, name(map[byte]string{1: "warning", 2: "fatal"}, body[0]), name(alertNames, body[1])))
		} else {
			add(p.dir + " Alert(encrypted)")
		}
	case 22:
		if p.encrypted {
			add(p.dir + " Handshake(encrypted)")
			return
		}
		var msgs []string
		for len(body) >= 4 {
			mt := body[0]
			ml := int(body[1])<<16 | int(body[2])<<8 | int(body[3])
			if len(body) < 4+ml {
				msgs = append(msgs, name(handshakeNames, mt)+"(fragment)")
				break
			}
			msgs = append(msgs, describe(mt, body[4:4+ml]))
			body = body[4+ml:]
		}
		add(fmt.Sprintf("%s [%s] %s", p.dir, versionName(ver), strings.Join(msgs, ", ")))
	case 23:
		if !p.appData {
			add(p.dir + " ApplicationData")
		}
		p.appData = true
	case 24:
		add(p.dir + " Heartbeat")
	}
}

func describe(mt byte, m []byte) string {
	n := name(handshakeNames, mt)
	switch mt {
	case 1:
		return n + clientHello(m)
	case 2:
		if len(m) >= 35 {
			sid := int(m[34])
			s := fmt.Sprintf("(%s sid=%d", versionName(binary.BigEndian.Uint16(m)), sid)
			if len(m) >= 37+sid {
				s += fmt.Sprintf(" suite=0x%04x", binary.BigEndian.Uint16(m[35+sid:]))
			}
			return n + s + ")"
		}
	case 11:
		if len(m) >= 3 {
			l := int(m[0])<<16 | int(m[1])<<8 | int(m[2])
			if l == 0 {
				return n + "(empty)"
			}
			return n + fmt.Sprintf("(%d bytes)", l)
		}
	case 13:
		return n + fmt.Sprintf("(%d bytes)", len(m))
	}
	return n
}

// clientHello summarises version, session ID, ticket and extensions.
func clientHello(m []byte) string {
	if len(m) < 35 {
		return "(short)"
	}
	ver := binary.BigEndian.Uint16(m)
	p := 34
	sid := int(m[p])
	p += 1 + sid
	if len(m) < p+2 {
		return "(short)"
	}
	cs := int(binary.BigEndian.Uint16(m[p:]))
	p += 2 + cs
	if len(m) < p+1 {
		return "(short)"
	}
	p += 1 + int(m[p])
	out := fmt.Sprintf("(%s sid=%d suites=%d", versionName(ver), sid, cs/2)
	if len(m) >= p+2 {
		end := p + 2 + int(binary.BigEndian.Uint16(m[p:]))
		p += 2
		var exts []string
		for p+4 <= len(m) && p+4 <= end {
			et, el := binary.BigEndian.Uint16(m[p:]), int(binary.BigEndian.Uint16(m[p+2:]))
			switch et {
			case 35:
				exts = append(exts, fmt.Sprintf("ticket=%d", el))
			case 43:
				exts = append(exts, "supported_versions")
			case 0xff01:
				exts = append(exts, "renegotiation_info")
			case 23:
				exts = append(exts, "ems")
			case 0:
				exts = append(exts, "sni")
			default:
				exts = append(exts, fmt.Sprint(et))
			}
			p += 4 + el
		}
		out += " ext=" + strings.Join(exts, ",")
	}
	return out + ")"
}

func versionName(v uint16) string {
	switch v {
	case 0x0301:
		return "TLS1.0"
	case 0x0302:
		return "TLS1.1"
	case 0x0303:
		return "TLS1.2"
	case 0x0304:
		return "TLS1.3"
	}
	return fmt.Sprintf("0x%04x", v)
}

func name(m map[byte]string, b byte) string {
	if s, ok := m[b]; ok {
		return s
	}
	return fmt.Sprint(b)
}

func preview(b []byte) []byte {
	if len(b) > 12 {
		return b[:12]
	}
	return b
}
