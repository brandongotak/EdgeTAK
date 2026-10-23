package app

import (
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/config"
)

// TestPlainTCPToggle covers turning the plain TCP port off and on from the
// console: off closes the port and drops its devices, and the choice is
// kept.
func TestPlainTCPToggle(t *testing.T) {
	a := startApp(t)
	call, _ := adminJSON(t, a)
	dev := mustDial(t, a, nil)
	dev.send(sa("ANDROID-anon", "ANON", "Cyan"))
	tcpClients := func() int {
		n := 0
		for _, c := range a.Router.Clients() {
			if c.Transport == "tcp" {
				n++
			}
		}
		return n
	}
	waitFor(t, "a TCP device", func() bool { return tcpClients() == 1 })
	if code, v := call("GET", "/admin/api/plain-tcp", nil); code != 200 || v["on"] != true || v["configured"] != true {
		t.Fatalf("status = %d %v", code, v)
	}

	if code, v := call("PUT", "/admin/api/plain-tcp", map[string]bool{"on": false}); code != 200 || v["on"] != false {
		t.Fatalf("turn off = %d %v", code, v)
	}
	waitFor(t, "the TCP device dropped", func() bool { return tcpClients() == 0 })
	if _, err := dialStream(t, a, nil); err == nil {
		t.Error("connected to plain TCP while it is off")
	}
	if !a.Store.PlainTCPOff() {
		t.Error("the setting wasn't kept")
	}
	if _, st := call("GET", "/admin/api/status", nil); st["listeners"].(map[string]any)["tcp"] != nil {
		t.Errorf("status lists tcp while off: %v", st["listeners"])
	}
	if code, _ := call("GET", "/admin/api/connection-package?mode=tcp", nil); code == 200 {
		t.Error("made a TCP package while TCP is off")
	}

	if code, v := call("PUT", "/admin/api/plain-tcp", map[string]bool{"on": true}); code != 200 || v["on"] != true {
		t.Fatalf("turn on = %d %v", code, v)
	}
	mustDial(t, a, nil).send(sa("ANDROID-anon2", "ANON2", "Cyan"))
	waitFor(t, "a TCP device again", func() bool { return tcpClients() == 1 })
	if a.Store.PlainTCPOff() {
		t.Error("the setting wasn't kept")
	}
}

// TestPlainTCPWithoutConfig covers a server installed with --no-plaintext:
// plain TCP starts off, and turning it on opens the usual port.
func TestPlainTCPWithoutConfig(t *testing.T) {
	a := startAppWith(t, func(c *config.Config) { c.TCPAddr = "" })
	call, _ := adminJSON(t, a)
	if code, v := call("GET", "/admin/api/plain-tcp", nil); code != 200 || v["on"] != false {
		t.Fatalf("status = %d %v", code, v)
	}
	if _, st := call("GET", "/admin/api/status", nil); st["listeners"].(map[string]any)["tcp"] != nil {
		t.Errorf("status lists tcp while off: %v", st["listeners"])
	}
	code, v := call("PUT", "/admin/api/plain-tcp", map[string]bool{"on": true})
	if code != 200 || v["on"] != true || v["port"] == "" {
		t.Fatalf("turn on = %d %v", code, v)
	}
	if on, _ := a.Store.PlainTCP(); !on {
		t.Error("the setting wasn't kept")
	}
	conn, err := mem.Dial("127.0.0.1:" + v["port"].(string))
	if err != nil {
		t.Fatalf("plain TCP after turning it on: %v", err)
	}
	conn.Close()
	if _, st := call("GET", "/admin/api/status", nil); st["listeners"].(map[string]any)["tcp"] == nil {
		t.Errorf("status doesn't list tcp while on: %v", st["listeners"])
	}
}
