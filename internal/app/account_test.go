package app

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/pki"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// accountClient signs in to the account page as user.
func accountClient(t *testing.T, a *App, user, pw string) (func(method, path string, body any) (int, map[string]any), *http.Client) {
	t.Helper()
	call, hc := accountClientRaw(t, a)
	if code, res := call("POST", "/account/api/login", map[string]string{"username": user, "password": pw}); code != 200 {
		t.Fatalf("account sign-in as %s = %d %v", user, code, res)
	}
	return call, hc
}

// accountClientRaw is a client for the account page that has not signed in.
func accountClientRaw(t *testing.T, a *App) (func(method, path string, body any) (int, map[string]any), *http.Client) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	base := "https://" + a.Addrs["enroll"]
	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+path, rd)
		req.Header.Set("X-EdgeTAK", "1")
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	return call, hc
}

func TestAccountSelfService(t *testing.T) {
	a := startApp(t)
	for _, u := range []string{"bob", "eve"} {
		if _, err := a.Store.CreateUser(u, "password-"+u, []string{"Cyan"}, false); err != nil {
			t.Fatal(err)
		}
	}
	base := "https://" + a.Addrs["enroll"]
	plain := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	status := func(resp *http.Response, err error) int {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() // an open body keeps the connection busy and delays shutdown
		return resp.StatusCode
	}

	// The page is public; the API needs a sign-in.
	if code := status(plain.Get(base + "/account")); code != 200 {
		t.Fatalf("page = %d", code)
	}
	if code := status(plain.Get(base + "/account/api/me")); code != 401 {
		t.Errorf("me without sign-in = %d", code)
	}
	if code := status(plain.Post(base+"/account/api/login", "application/json", strings.NewReader(`{"username":"bob","password":"wrong"}`))); code != 401 {
		t.Errorf("wrong password = %d", code)
	}

	// bob enrolls two devices and connects with one.
	phone := enroll(t, a, "bob", "password-bob")
	tablet := enroll(t, a, "bob", "password-bob")
	stream := mustDial(t, a, &phone)
	stream.send(sa("ANDROID-bob", "BOB", "Cyan"))
	eveCert := enroll(t, a, "eve", "password-eve")
	time.Sleep(200 * time.Millisecond)
	serial := func(c tls.Certificate) string {
		leaf, err := x509.ParseCertificate(c.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return pki.SerialHex(leaf)
	}

	call, hc := accountClient(t, a, "bob", "password-bob")
	code, me := call("GET", "/account/api/me", nil)
	if code != 200 || me["username"] != "bob" || me["admin"] != false {
		t.Fatalf("me = %d %v", code, me)
	}
	if devs := me["devices"].([]any); len(devs) != 1 || devs[0].(map[string]any)["callsign"] != "BOB" {
		t.Errorf("devices = %v", devs)
	}
	certs := me["certs"].([]any)
	if len(certs) != 2 {
		t.Fatalf("certs = %v", certs)
	}
	inUse := 0
	for _, c := range certs {
		if u, _ := c.(map[string]any)["in_use"].([]any); len(u) > 0 {
			inUse++
		}
	}
	if inUse != 1 {
		t.Errorf("certs in use = %d", inUse)
	}

	// The account session is not an admin or TAK API login.
	var cookie *http.Cookie
	for _, c := range hc.Jar.Cookies(&url.URL{Scheme: "https", Host: a.Addrs["enroll"], Path: "/account/"}) {
		if c.Name == "edgetak_account" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no account cookie")
	}
	for _, path := range []string{"/admin/api/me", "/Marti/api/groups/all"} {
		req, _ := http.NewRequest("GET", "https://"+a.Addrs["https"]+path, nil)
		req.AddCookie(cookie)
		req.Header.Set("Cookie", "edgetak_account="+cookie.Value+"; edgetak_session="+cookie.Value)
		if code := status(plain.Do(req)); code < 400 {
			t.Errorf("%s with the account cookie = %d", path, code)
		}
	}
	// State changes need the custom header.
	req, _ := http.NewRequest("POST", base+"/account/api/password", strings.NewReader(`{}`))
	if code := status(hc.Do(req)); code != http.StatusForbidden {
		t.Errorf("password change without header = %d", code)
	}

	// Connection package for bob.
	resp, err := hc.Get(base + "/account/api/package?mode=ssl")
	if err != nil {
		t.Fatal(err)
	}
	zip, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.HasPrefix(zip, []byte("PK")) || !strings.Contains(resp.Header.Get("Content-Disposition"), "bob") {
		t.Errorf("package = %d %q", resp.StatusCode, resp.Header.Get("Content-Disposition"))
	}

	// bob can revoke his own certificates but not eve's.
	if code, _ := call("POST", "/account/api/certs/"+serial(eveCert)+"/revoke", nil); code != 404 {
		t.Errorf("revoking eve's certificate = %d", code)
	}
	if a.Store.IsRevoked(serial(eveCert)) {
		t.Fatal("eve's certificate was revoked")
	}
	if code, _ := call("POST", "/account/api/certs/"+serial(phone)+"/revoke", nil); code != 204 {
		t.Fatalf("revoke = %d", code)
	}
	if !a.Store.IsRevoked(serial(phone)) || a.Store.IsRevoked(serial(tablet)) {
		t.Error("wrong certificate revoked")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(a.Router.Clients()) > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(a.Router.Clients()); n != 0 {
		t.Errorf("the revoked device is still connected (%d clients)", n)
	}

	// Changing the password signs out bob's other sessions.
	other, _ := accountClient(t, a, "bob", "password-bob")
	if code, _ := call("POST", "/account/api/password", map[string]string{"current": "nope", "new": "new-password-1"}); code != 403 {
		t.Errorf("wrong current password = %d", code)
	}
	if code, _ := call("POST", "/account/api/password", map[string]string{"current": "password-bob", "new": "short"}); code != 400 {
		t.Errorf("short password = %d", code)
	}
	if code, _ := call("POST", "/account/api/password", map[string]string{"current": "password-bob", "new": "new-password-1"}); code != 204 {
		t.Fatalf("password change = %d", code)
	}
	if _, ok := a.Store.Authenticate("bob", "new-password-1"); !ok {
		t.Error("new password does not work")
	}
	if code, _ := call("GET", "/account/api/me", nil); code != 200 {
		t.Errorf("this session after the change = %d", code)
	}
	if code, _ := other("GET", "/account/api/me", nil); code != 401 {
		t.Errorf("the other session after the change = %d", code)
	}

	// Deleting the account ends its sessions.
	if err := a.Store.DeleteUser("bob"); err != nil {
		t.Fatal(err)
	}
	if code, _ := call("GET", "/account/api/me", nil); code != 401 {
		t.Errorf("after deletion = %d", code)
	}
}

// One sign-in for everyone: admins get the console and their account page,
// users only their account page, and signing out ends both.
func TestSharedSignIn(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("dave", "password-dave", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	raw := readFile(t, a.DataDir+"/initial-admin-password.txt")
	adminPW := strings.TrimPrefix(strings.Split(raw, "\n")[1], "password: ")
	base := "https://" + a.Addrs["https"]
	newClient := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	}
	do := func(hc *http.Client, method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("X-EdgeTAK", "1")
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	// The sign-in page is the root.
	hc := newClient()
	resp, err := hc.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "signin-form") {
		t.Fatalf("root = %d", resp.StatusCode)
	}

	// A user signing in through the console's endpoint gets only their account.
	if code, res := do(hc, "POST", "/admin/api/login", `{"username":"dave","password":"password-dave"}`); code != 200 || res["admin"] != false {
		t.Fatalf("user sign-in = %d %v", code, res)
	}
	if code, _ := do(hc, "GET", "/account/api/me", ""); code != 200 {
		t.Errorf("user account page = %d", code)
	}
	if code, _ := do(hc, "GET", "/admin/api/me", ""); code != 401 {
		t.Errorf("user console = %d", code)
	}

	// An admin gets both, from either endpoint.
	adm := newClient()
	if code, res := do(adm, "POST", "/account/api/login", `{"username":"admin","password":"`+adminPW+`"}`); code != 200 || res["admin"] != true {
		t.Fatalf("admin sign-in = %d %v", code, res)
	}
	if code, _ := do(adm, "GET", "/admin/api/me", ""); code != 200 {
		t.Errorf("admin console = %d", code)
	}
	if code, me := do(adm, "GET", "/account/api/me", ""); code != 200 || me["admin"] != true {
		t.Errorf("admin account page = %d %v", code, me)
	}

	// Signing out of either ends both.
	if code, _ := do(adm, "POST", "/admin/api/logout", ""); code != 204 {
		t.Fatalf("sign out = %d", code)
	}
	for _, path := range []string{"/admin/api/me", "/account/api/me"} {
		if code, _ := do(adm, "GET", path, ""); code != 401 {
			t.Errorf("%s after sign out = %d", path, code)
		}
	}
}

func TestAccountTwoFactor(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("carol", "password-carol", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.UpdateUser("carol", func(u *store.User) error {
		u.TOTPSecret = "JBSWY3DPEHPK3PXP"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	plain := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	resp, err := plain.Post("https://"+a.Addrs["enroll"]+"/account/api/login", "application/json", strings.NewReader(`{"username":"carol","password":"password-carol"}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 401 || body["need_code"] != true {
		t.Errorf("sign-in without a code = %d %v", resp.StatusCode, body)
	}
}
