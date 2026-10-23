package app

import "testing"

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
