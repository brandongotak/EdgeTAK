package app

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestLDAPSignIn signs in against a real directory. It runs when
// EDGETAK_LDAP_URL points at one seeded as in the README's test setup
// (osixia/openldap with users alice, bob, carol and groups TAK-Cyan,
// TAK-Red, TAK-Admins, Staff under dc=example,dc=org).
func TestLDAPSignIn(t *testing.T) {
	url := os.Getenv("EDGETAK_LDAP_URL")
	if url == "" {
		t.Skip("set EDGETAK_LDAP_URL to run against a test directory")
	}
	a := startApp(t) // the test network is in memory; the directory is real
	call, _ := adminJSON(t, a)
	settings := map[string]any{"enabled": true, "url": url, "bind_dn": "cn=admin,dc=example,dc=org", "bind_password": "admin-secret",
		"base_dn": "ou=people,dc=example,dc=org", "group_base_dn": "ou=groups,dc=example,dc=org",
		"groups": []string{"TAK-Cyan", "TAK-Red"}, "admin_group": "TAK-Admins", "required_group": "Staff"}
	if ca := os.Getenv("EDGETAK_LDAP_CA"); ca != "" { // a file with the directory's CA, for ldaps:// or StartTLS
		pem, err := os.ReadFile(ca)
		if err != nil {
			t.Fatal(err)
		}
		settings["ca_pem"] = string(pem)
	}
	settings["start_tls"] = os.Getenv("EDGETAK_LDAP_STARTTLS") != ""

	// The Test button: wrong password, then a real sign-in.
	if code, _ := call("POST", "/admin/api/ldap/test", map[string]any{"settings": settings, "username": "alice", "password": "nope"}); code != 502 {
		t.Errorf("test with a wrong password = %d", code)
	}
	code, r := call("POST", "/admin/api/ldap/test", map[string]any{"settings": settings, "username": "alice", "password": "alice-pass-1"})
	user, _ := r["user"].(map[string]any)
	if code != 200 || user["admin"] != true || user["allowed"] != true || !strings.Contains(fmt.Sprint(user["groups"]), "TAK-Cyan") {
		t.Fatalf("test = %d %v", code, r)
	}
	if code, _ := call("PUT", "/admin/api/ldap", settings); code != 200 {
		t.Fatalf("save = %d", code)
	}

	// ATAK enrollment signs in with the directory password.
	signIn := func(user, pw string) int {
		hc := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
		req, _ := http.NewRequest("GET", "https://"+a.Addrs["https"]+"/Marti/api/tls/config", nil)
		req.SetBasicAuth(user, pw)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := signIn("alice", "alice-pass-1"); c != 200 {
		t.Fatalf("alice = %d", c)
	}
	u, ok := a.Store.User("alice")
	if !ok || u.Source != "ldap" || !u.Admin || strings.Join(u.Groups, ",") != "TAK-Cyan" || u.PasswordHash != "" {
		t.Errorf("alice's record = %+v", u)
	}
	if c := signIn("ALICE", "alice-pass-1"); c != 200 {
		t.Errorf("ALICE (any case) = %d", c)
	}
	if c := signIn("alice", "wrong"); c != 401 {
		t.Errorf("alice with a wrong password = %d", c)
	}
	if c := signIn("bob", "bob-pass-1"); c != 200 {
		t.Errorf("bob = %d", c)
	}
	if u, _ := a.Store.User("bob"); u.Admin || strings.Join(u.Groups, ",") != "TAK-Red" {
		t.Errorf("bob's record = %+v", u)
	}
	// Only Staff may sign in: everyone here is; drop the requirement to a
	// group carol isn't in and she is refused.
	settings["required_group"] = "TAK-Red"
	call("PUT", "/admin/api/ldap", settings)
	if c := signIn("carol", "carol-pass-1"); c != 401 {
		t.Errorf("carol (not in the required group) = %d", c)
	}
	if _, ok := a.Store.User("carol"); ok {
		t.Error("a refused user got a record")
	}
	// A local account keeps working, and directory users can't get a local password.
	a.Store.CreateUser("local1", "local-pass-1", nil, false)
	if c := signIn("local1", "local-pass-1"); c != 200 {
		t.Errorf("local user = %d", c)
	}
	if code, _ := call("PATCH", "/admin/api/users/alice", map[string]any{"password": "something-new"}); code != 400 {
		t.Errorf("setting a directory user's password = %d", code)
	}

	// Sync: a group change in the directory reaches the user without a sign-in.
	a.Store.UpsertDirectoryUser("bob", "uid=bob,ou=people,dc=example,dc=org", []string{"stale"}, nil)
	settings["required_group"] = ""
	call("PUT", "/admin/api/ldap", settings)
	a.Web.SyncDirectoryUsers()
	if u, _ := a.Store.User("bob"); strings.Join(u.Groups, ",") != "TAK-Red" {
		t.Errorf("bob after sync = %+v", u.Groups)
	}

	// Directory off: directory users can't sign in, local ones can.
	settings["enabled"] = false
	call("PUT", "/admin/api/ldap", settings)
	if c := signIn("bob", "bob-pass-1"); c != 401 {
		t.Errorf("bob with the directory off = %d", c)
	}
}
