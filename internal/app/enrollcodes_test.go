package app

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestEnrollCodes covers quick-connect QR codes made by an admin: the code
// in the link enrolls the user's device in place of their password, but
// signs in nowhere else, and revoking it stops it.
func TestEnrollCodes(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("alice", "alices-real-password", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, _ := adminJSON(t, a)
	code, v := call("POST", "/admin/api/enroll-codes", map[string]any{"username": "alice", "hours": 24})
	if code != 201 {
		t.Fatalf("make code = %d %v", code, v)
	}
	link, _ := url.Parse(v["link"].(string))
	q := link.Query()
	token := q.Get("token")
	if link.Scheme != "tak" || q.Get("username") != "alice" || q.Get("host") != "127.0.0.1" || len(token) < 20 || strings.Contains(v["link"].(string), "alices-real-password") {
		t.Fatalf("link = %s", link)
	}

	// The device enrolls with the code (the same requests ATAK makes) and
	// connects with the certificate it gets.
	cert := enroll(t, a, "alice", token)
	dev := mustDial(t, a, &cert)
	dev.send(sa("ANDROID-alice", "ALICE", "Cyan"))
	waitFor(t, "alice connected", func() bool {
		for _, c := range a.Router.Clients() {
			if c.Username == "alice" {
				return true
			}
		}
		return false
	})

	// The code is no password anywhere else.
	plain := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	base := "https://" + a.Addrs["https"]
	for _, path := range []string{"/admin/api/login", "/account/api/login"} {
		resp, err := plain.Post(base+path, "application/json", strings.NewReader(`{"username":"alice","password":"`+token+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("signed in to %s with the enrollment code", path)
		}
	}
	req, _ := http.NewRequest("GET", base+"/Marti/api/missions", nil)
	req.SetBasicAuth("alice", token)
	if resp, err := plain.Do(req); err != nil || resp.StatusCode != 401 {
		t.Errorf("the code on another API = %v %v", err, resp.Status)
	}
	// Alice's real password is unchanged.
	enroll(t, a, "alice", "alices-real-password")

	// Revoked: it no longer enrolls.
	codes := v["codes"].([]any)
	id := codes[0].(map[string]any)["id"].(string)
	if code, _ := call("DELETE", "/admin/api/enroll-codes/"+id, nil); code != 200 {
		t.Fatalf("revoke = %d", code)
	}
	req, _ = http.NewRequest("GET", "https://"+a.Addrs["enroll"]+"/Marti/api/tls/config", nil)
	req.SetBasicAuth("alice", token)
	if resp, err := plain.Do(req); err != nil || resp.StatusCode != 401 {
		t.Errorf("revoked code = %v %v", err, resp.Status)
	}

	// No codes for unknown or disabled users.
	if code, _ := call("POST", "/admin/api/enroll-codes", map[string]any{"username": "nobody"}); code != 404 {
		t.Errorf("unknown user = %d", code)
	}
	call("PATCH", "/admin/api/users/alice", map[string]bool{"disabled": true})
	if code, _ := call("POST", "/admin/api/enroll-codes", map[string]any{"username": "alice"}); code != 400 {
		t.Errorf("disabled user = %d", code)
	}
}
