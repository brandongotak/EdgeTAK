package app

import (
	"crypto/tls"
	"net/http"
	"testing"
	"time"
)

// TestDisableUser covers disabling a user: password sign-in, the
// certificate, console sessions and device streams all stop, and enabling
// brings everything back.
func TestDisableUser(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("alice", "password-alice", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, _ := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]
	cert := enroll(t, a, "alice", "password-alice")
	dev := mustDial(t, a, &cert)
	dev.send(sa("ANDROID-alice", "ALICE", "Cyan"))
	api := newAPI(t, a, cert)
	password := func(pw string) int {
		hc := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
		req, _ := http.NewRequest("GET", base+"/Marti/api/tls/config", nil)
		req.SetBasicAuth("alice", pw)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st, _, _ := api.do("GET", "/Marti/api/tls/config", nil); st != 200 || password("password-alice") != 200 {
		t.Fatalf("before: cert %d, password %d", st, password("password-alice"))
	}

	// Admins can't disable themselves.
	if code, _ := call("PATCH", "/admin/api/users/admin", map[string]bool{"disabled": true}); code != 400 {
		t.Errorf("disabling yourself = %d", code)
	}
	if code, v := call("PATCH", "/admin/api/users/alice", map[string]bool{"disabled": true}); code != 200 || v["disabled"] != true || v["disabled_by"] != "admin" {
		t.Fatalf("disable = %d %v", code, v)
	}

	// The open stream is closed, and a new one is refused.
	waitFor(t, "alice's device disconnected", func() bool {
		for _, c := range a.Router.Clients() {
			if c.Username == "alice" {
				return false
			}
		}
		return true
	})
	if c, err := dialStream(t, a, &cert); err == nil {
		c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 1)
		for {
			if _, err := c.conn.Read(buf); err != nil {
				break // closed by the server
			}
		}
		time.Sleep(100 * time.Millisecond)
		for _, cl := range a.Router.Clients() {
			if cl.Username == "alice" {
				t.Error("a disabled user's device was let in")
			}
		}
	}
	if st, _, _ := api.do("GET", "/Marti/api/tls/config", nil); st != 401 {
		t.Errorf("certificate after disabling = %d", st)
	}
	if c := password("password-alice"); c != 401 {
		t.Errorf("password after disabling = %d", c)
	}
	if u, _ := a.Store.User("alice"); len(u.Groups) != 1 {
		t.Errorf("groups not kept: %+v", u)
	}

	// Another admin can be disabled (one who signs in always remains: you).
	a.Store.CreateUser("boss", "password-boss", nil, true)
	if code, _ := call("PATCH", "/admin/api/users/boss", map[string]bool{"disabled": true}); code != 200 {
		t.Errorf("disabling a second admin = %d", code)
	}

	// Enabling brings everything back.
	if code, v := call("PATCH", "/admin/api/users/alice", map[string]bool{"disabled": false}); code != 200 || v["disabled"] == true {
		t.Fatalf("enable = %d %v", code, v)
	}
	if st, _, _ := api.do("GET", "/Marti/api/tls/config", nil); st != 200 || password("password-alice") != 200 {
		t.Errorf("after enabling: cert %d, password %d", st, password("password-alice"))
	}
	mustDial(t, a, &cert).send(sa("ANDROID-alice", "ALICE", "Cyan"))
	waitFor(t, "alice's device back", func() bool {
		for _, c := range a.Router.Clients() {
			if c.Username == "alice" {
				return true
			}
		}
		return false
	})
}
