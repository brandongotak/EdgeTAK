package app

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

func TestSelfRegistration(t *testing.T) {
	a := startApp(t)
	call, _ := adminJSON(t, a)
	hc := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	base := "https://" + a.Addrs["enroll"]

	public := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+path, rd)
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
	signup := func(user, pw, invite string) int {
		t.Helper()
		code, _ := public("POST", "/api/register", map[string]string{"username": user, "password": pw, "invite": invite})
		return code
	}
	groupsOf := func(name string) []string {
		t.Helper()
		u, ok := a.Store.User(name)
		if !ok {
			t.Fatalf("user %s does not exist", name)
		}
		return u.Groups
	}

	// The page is served and registration starts closed.
	resp, err := hc.Get(base + "/register?invite=x")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("register page: %v %v", resp, err)
	}
	resp.Body.Close()
	if _, info := public("GET", "/api/register/info", nil); info["mode"] != "off" {
		t.Fatalf("info = %v", info)
	}
	if c := signup("walkin", "password-1", ""); c != http.StatusForbidden {
		t.Fatalf("closed sign-up = %d", c)
	}

	// Single-use invite into Cyan.
	code, inv := call("POST", "/admin/api/registration/invites", map[string]any{"label": "team", "groups": []string{"Cyan"}, "max_uses": 1})
	if code != http.StatusCreated || !strings.Contains(inv["url"].(string), "/register?invite=") {
		t.Fatalf("create invite = %d %v", code, inv)
	}
	ic := inv["code"].(string)
	if _, info := public("GET", "/api/register/info?invite="+ic, nil); info["invite"].(map[string]any)["valid"] != true {
		t.Fatalf("invite info = %v", info)
	}
	if c := signup("alice", "password-alice", ic); c != http.StatusCreated {
		t.Fatalf("invite sign-up = %d", c)
	}
	if g := groupsOf("alice"); !slices.Equal(g, []string{"Cyan"}) {
		t.Fatalf("alice groups = %v", g)
	}
	if c := signup("alice2", "password-alice", ic); c != http.StatusGone {
		t.Fatalf("used-up invite = %d", c)
	}
	// The new account works for certificate enrollment.
	enroll(t, a, "alice", "password-alice")

	// Expired and revoked invites.
	exp, _ := a.Store.CreateInvite(store.Invite{Code: "expired-code", Created: time.Now().Add(-48 * time.Hour), Expires: time.Now().Add(-time.Hour)})
	if c := signup("late", "password-late", exp.Code); c != http.StatusGone {
		t.Fatalf("expired invite = %d", c)
	}
	_, inv2 := call("POST", "/admin/api/registration/invites", map[string]any{})
	if c, _ := call("DELETE", "/admin/api/registration/invites/"+inv2["code"].(string), nil); c != http.StatusNoContent {
		t.Fatalf("revoke = %d", c)
	}
	if c := signup("revoked", "password-rev", inv2["code"].(string)); c != http.StatusGone {
		t.Fatalf("revoked invite = %d", c)
	}

	// Approval mode with default groups.
	if c, _ := call("PUT", "/admin/api/registration", map[string]any{"mode": "approval", "default_groups": []string{"Red"}}); c != http.StatusNoContent {
		t.Fatalf("set registration = %d", c)
	}
	if c := signup("bob", "password-bob", ""); c != http.StatusAccepted {
		t.Fatalf("request = %d", c)
	}
	if c := signup("BOB", "password-bob", ""); c != http.StatusConflict {
		t.Fatalf("duplicate pending username = %d", c)
	}
	if c := signup("carol", "password-carol", ""); c != http.StatusAccepted {
		t.Fatalf("request = %d", c)
	}
	if _, ok := a.Store.User("bob"); ok {
		t.Fatal("bob exists before approval")
	}
	_, reg := call("GET", "/admin/api/registration", nil)
	reqs := reg["requests"].([]any)
	if len(reqs) != 2 {
		t.Fatalf("requests = %v", reqs)
	}
	ids := map[string]string{}
	for _, r := range reqs {
		m := r.(map[string]any)
		if _, leaked := m["password_hash"]; leaked {
			t.Fatal("request listing includes the password hash")
		}
		ids[m["username"].(string)] = m["id"].(string)
	}
	if c, _ := call("POST", "/admin/api/registration/requests/"+ids["bob"]+"/approve", map[string]any{}); c != http.StatusOK {
		t.Fatalf("approve = %d", c)
	}
	if c, _ := call("DELETE", "/admin/api/registration/requests/"+ids["carol"], nil); c != http.StatusNoContent {
		t.Fatalf("reject = %d", c)
	}
	if g := groupsOf("bob"); !slices.Equal(g, []string{"Red"}) {
		t.Fatalf("bob groups = %v", g)
	}
	if _, ok := a.Store.User("carol"); ok {
		t.Fatal("rejected request created a user")
	}
	enroll(t, a, "bob", "password-bob") // the password chosen at sign-up

	// Open mode.
	call("PUT", "/admin/api/registration", map[string]any{"mode": "open", "default_groups": []string{"Green"}})
	if c := signup("dave", "short", ""); c != http.StatusBadRequest {
		t.Fatalf("short password = %d", c)
	}
	if c := signup("dave", "password-dave", ""); c != http.StatusCreated {
		t.Fatalf("open sign-up = %d", c)
	}
	if c := signup("dave", "password-dave", ""); c != http.StatusConflict {
		t.Fatalf("duplicate username = %d", c)
	}
	if g := groupsOf("dave"); !slices.Equal(g, []string{"Green"}) {
		t.Fatalf("dave groups = %v", g)
	}

	// The new user downloads their connection package with their credentials.
	req, _ := http.NewRequest("GET", base+"/api/register/package?mode=ssl", nil)
	req.SetBasicAuth("dave", "password-dave")
	resp, err = hc.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("package: %v %v", resp, err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if _, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("package is not a zip: %v", err)
	}
	req, _ = http.NewRequest("GET", base+"/api/register/package", nil)
	req.SetBasicAuth("dave", "wrong-password")
	if resp, _ = hc.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("package with wrong password = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The admin endpoints need an administrator.
	if c, _ := public("GET", "/admin/api/registration", nil); c != http.StatusUnauthorized {
		t.Fatalf("anonymous admin access = %d", c)
	}
}
