package app

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/totp"
)

func TestTwoFactorLogin(t *testing.T) {
	a := startApp(t)
	raw, _ := os.ReadFile(filepath.Join(a.DataDir, "initial-admin-password.txt"))
	pw := strings.TrimPrefix(strings.Split(string(raw), "\n")[1], "password: ")
	base := "https://" + a.Addrs["https"]
	newClient := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	}
	login := func(hc *http.Client, user, pass, code string) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"username": user, "password": pass, "code": code})
		resp, err := hc.Post(base+"/admin/api/login", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	call := func(hc *http.Client, method, path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(b))
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

	admin := newClient()
	if code, _ := login(admin, "admin", pw, ""); code != 200 {
		t.Fatalf("login without 2FA = %d", code)
	}
	if _, st := call(admin, "GET", "/admin/api/me/2fa", nil); st["enabled"] != false {
		t.Fatalf("2fa status = %v", st)
	}

	// Set up: scan, then confirm with a code.
	_, setup := call(admin, "POST", "/admin/api/me/2fa/setup", nil)
	secret := setup["secret"].(string)
	if !strings.HasPrefix(setup["uri"].(string), "otpauth://totp/") {
		t.Fatalf("setup = %v", setup)
	}
	if code, _ := call(admin, "POST", "/admin/api/me/2fa/enable", map[string]string{"code": "000000"}); code != 400 {
		t.Fatalf("enable with a wrong code = %d", code)
	}
	now, _ := totp.Code(secret, time.Now())
	code, en := call(admin, "POST", "/admin/api/me/2fa/enable", map[string]string{"code": now})
	if code != 200 || len(en["recovery_codes"].([]any)) != 10 {
		t.Fatalf("enable = %d %v", code, en)
	}
	recovery := en["recovery_codes"].([]any)[0].(string)

	// Signing in now needs a code.
	c2 := newClient()
	if code, body := login(c2, "admin", pw, ""); code != 401 || body["need_code"] != true {
		t.Fatalf("login without code = %d %v", code, body)
	}
	if code, _ := login(c2, "admin", pw, "123456"); code != 401 {
		t.Fatalf("login with a wrong code = %d", code)
	}
	next, _ := totp.Code(secret, time.Now().Add(30*time.Second)) // the enable used the current step
	if code, _ := login(c2, "admin", pw, next); code != 200 {
		t.Fatalf("login with the right code = %d", code)
	}
	if code, _ := login(newClient(), "admin", pw, next); code != 401 {
		t.Fatalf("the same code worked twice: %d", code)
	}
	// Recovery codes work once.
	if code, _ := login(newClient(), "admin", pw, strings.ToUpper(recovery)); code != 200 {
		t.Fatalf("recovery code login = %d", code)
	}
	if code, _ := login(newClient(), "admin", pw, recovery); code != 401 {
		t.Fatal("a recovery code worked twice")
	}
	if _, st := call(c2, "GET", "/admin/api/me/2fa", nil); st["enabled"] != true || st["recovery_left"] != 9.0 {
		t.Fatalf("2fa status = %v", st)
	}

	// The password alone cannot reach the admin API any more...
	req, _ := http.NewRequest("GET", base+"/admin/api/users", nil)
	req.SetBasicAuth("admin", pw)
	resp, err := newClient().Do(req)
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("basic auth to the admin API with 2FA on: %v %v", resp, err)
	}
	resp.Body.Close()
	// ...but devices still enroll with it.
	enroll(t, a, "admin", pw)

	// Turning it off needs the password and a code.
	if code, _ := call(c2, "POST", "/admin/api/me/2fa/disable", map[string]string{"password": "wrong", "code": recovery}); code != 401 {
		t.Fatalf("disable with a wrong password = %d", code)
	}
	second := en["recovery_codes"].([]any)[1].(string)
	if code, _ := call(c2, "POST", "/admin/api/me/2fa/disable", map[string]string{"password": pw, "code": second}); code != 204 {
		t.Fatalf("disable = %d", code)
	}
	if code, _ := login(newClient(), "admin", pw, ""); code != 200 {
		t.Fatalf("login after turning it off = %d", code)
	}

	// Another admin with two-factor on.
	if _, err := a.Store.CreateUser("ops", "password-ops", nil, true); err != nil {
		t.Fatal(err)
	}
	a.Store.SetTOTPPending("ops", secret)
	opsCode, _ := totp.Code(secret, time.Now().Add(60*time.Second))
	if err := a.Store.EnableTOTP("ops", opsCode, nil, func(s, c string, l int64) (int64, bool) { return totp.Verify(s, c, time.Now().Add(60*time.Second), l) }); err != nil {
		t.Fatal(err)
	}
	// Guessing codes is slowed down.
	limited := false
	for i := 0; i < 8; i++ {
		if code, _ := login(newClient(), "ops", "password-ops", "999999"); code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("wrong codes are not rate limited")
	}
	// An admin can reset it (lost phone).
	if code, _ := call(c2, "DELETE", "/admin/api/users/ops/2fa", nil); code != 204 {
		t.Fatalf("reset = %d", code)
	}
	if u, _ := a.Store.User("ops"); u.TwoFactor() {
		t.Fatal("reset did not turn it off")
	}
}
