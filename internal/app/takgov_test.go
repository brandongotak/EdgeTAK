package app

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestTakGov covers linking to tak.gov (device sign-in), browsing its
// plugins and installing one on the update server, against a fake tak.gov.
func TestTakGov(t *testing.T) {
	a := startApp(t)
	call, _ := adminJSON(t, a)
	apk, err := os.ReadFile("../apps/testdata/helloworld.apk")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(apk)
	hash := hex.EncodeToString(sum[:])

	var mu sync.Mutex
	approved := false
	refresh := "refresh-1"
	refreshes := 0
	idToken := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"admin@example.org"}`)) + ".y"
	var fake *httptest.Server
	fake = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		oauthErr := func(code string) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": code})
		}
		switch {
		case r.URL.Path == "/auth/auth/device":
			if r.FormValue("client_id") != "tak-gov-eud" || !strings.Contains(r.FormValue("scope"), "offline_access") {
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"device_code": "dev-1", "user_code": "ABCD-EFGH",
				"verification_uri": "https://tak.gov/device", "verification_uri_complete": "https://tak.gov/device?user_code=ABCD-EFGH",
				"expires_in": 180, "interval": 5})
		case r.URL.Path == "/auth/token":
			switch r.FormValue("grant_type") {
			case "urn:ietf:params:oauth:grant-type:device_code":
				if !approved {
					oauthErr("authorization_pending")
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"access_token": "access-0", "refresh_token": refresh, "id_token": idToken, "expires_in": 0})
			case "refresh_token":
				if r.FormValue("refresh_token") != refresh {
					oauthErr("invalid_grant")
					return
				}
				refreshes++
				refresh = "refresh-" + string(rune('1'+refreshes))
				json.NewEncoder(w).Encode(map[string]any{"access_token": "access-ok", "refresh_token": refresh, "expires_in": 180})
			}
		case r.URL.Path == "/auth/logout":
			refresh = ""
		case strings.HasPrefix(r.URL.Path, "/eud_api/"):
			if r.Header.Get("Authorization") != "Bearer access-ok" {
				w.WriteHeader(401)
				return
			}
			switch r.URL.Path {
			case "/eud_api/software/v1/plugins":
				if r.URL.Query().Get("product") != "ATAK-CIV" || r.URL.Query().Get("product_version") != "5.6.0" {
					json.NewEncoder(w).Encode([]any{})
					return
				}
				json.NewEncoder(w).Encode([]map[string]any{
					{"display_name": "Hello World", "package_name": "com.example.helloworld", "version": "1.0", "apk_hash": hash,
						"apk_url": fake.URL + "/eud_api/software/v1/plugins/hello/apk", "icon_url": fake.URL + "/eud_api/software/v1/plugins/hello/icon"},
					{"display_name": "Tampered", "package_name": "com.example.bad", "version": "2.0", "apk_hash": strings.Repeat("0", 64),
						"apk_url": fake.URL + "/eud_api/software/v1/plugins/bad/apk"},
				})
			case "/eud_api/software/v1/plugins/hello/apk", "/eud_api/software/v1/plugins/bad/apk":
				w.Header().Set("Content-Disposition", `attachment; filename="HelloWorld-1.0.apk"`)
				w.Write(apk)
			default:
				w.WriteHeader(404)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()
	a.Web.TakGovAuthURL, a.Web.TakGovAPIURL, a.Web.TakGovClient = fake.URL+"/auth", fake.URL+"/eud_api", fake.Client()

	if _, v := call("GET", "/admin/api/takgov", nil); v["linked"] != false {
		t.Fatalf("before linking = %v", v)
	}
	if code, _ := call("GET", "/admin/api/takgov/plugins?product=ATAK-CIV&product_version=5.6.0", nil); code != 502 {
		t.Errorf("plugins before linking = %d", code)
	}

	// Link: a code to enter at tak.gov, pending until the admin signs in.
	code, v := call("POST", "/admin/api/takgov/link", nil)
	if p, _ := v["pending"].(map[string]any); code != 200 || p["user_code"] != "ABCD-EFGH" || p["verification_uri_complete"] == "" {
		t.Fatalf("link = %d %v", code, v)
	}
	if _, v := call("POST", "/admin/api/takgov/poll", nil); v["state"] != "pending" {
		t.Fatalf("poll before sign-in = %v", v)
	}
	mu.Lock()
	approved = true
	mu.Unlock()
	if _, v := call("POST", "/admin/api/takgov/poll", nil); v["state"] != "linked" || v["account"] != "admin@example.org" || v["linked"] != true {
		t.Fatalf("poll after sign-in = %v", v)
	}
	if l := a.Store.TakGov(); l.RefreshToken != "refresh-1" {
		t.Errorf("stored link = %+v", l)
	}

	// Browsing refreshes the expired access token; tak.gov's new refresh
	// token replaces the old one.
	code, v = call("GET", "/admin/api/takgov/plugins?product=ATAK-CIV&product_version=5.6.0", nil)
	plugins, _ := v["plugins"].([]any)
	if code != 200 || len(plugins) != 2 || plugins[0].(map[string]any)["installed"] != false {
		t.Fatalf("plugins = %d %v", code, v)
	}
	if l := a.Store.TakGov(); l.RefreshToken != "refresh-2" || l.Version != "5.6.0" {
		t.Errorf("after refresh = %+v", l)
	}

	// Only tak.gov addresses get the token.
	if code, _ := call("POST", "/admin/api/takgov/install", map[string]string{"apk_url": "https://evil.example/eud_api/x.apk"}); code != 400 {
		t.Errorf("install from elsewhere = %d", code)
	}
	if code, _ := call("GET", "/admin/api/takgov/icon?url=https://evil.example/x.png", nil); code != 400 {
		t.Errorf("icon from elsewhere = %d", code)
	}

	// A download that doesn't match tak.gov's hash isn't added.
	bad := plugins[1].(map[string]any)
	if code, v := call("POST", "/admin/api/takgov/install", map[string]any{"apk_url": bad["apk_url"], "apk_hash": bad["apk_hash"]}); code != 502 {
		t.Errorf("tampered install = %d %v", code, v)
	}
	if len(a.Store.Apps()) != 0 {
		t.Fatal("the tampered APK was added")
	}

	hello := plugins[0].(map[string]any)
	code, v = call("POST", "/admin/api/takgov/install", map[string]any{"apk_url": hello["apk_url"], "apk_hash": hello["apk_hash"], "icon_url": hello["icon_url"], "name": hello["display_name"]})
	if code != 200 {
		t.Fatalf("install = %d %v", code, v)
	}
	list := a.Store.Apps()
	if len(list) != 1 || list[0].SHA256 != hash || list[0].FileName != "HelloWorld-1.0.apk" || list[0].Uploader != "admin" {
		t.Fatalf("apps = %+v", list)
	}
	if _, v := call("GET", "/admin/api/takgov/plugins?product=ATAK-CIV&product_version=5.6.0", nil); v["plugins"].([]any)[0].(map[string]any)["installed"] != true {
		t.Errorf("not marked installed: %v", v)
	}

	// Unlink: the token is gone here and at tak.gov.
	if _, v := call("DELETE", "/admin/api/takgov", nil); v["linked"] != false {
		t.Errorf("unlink = %v", v)
	}
	if l := a.Store.TakGov(); l.RefreshToken != "" || l.Product != "ATAK-CIV" {
		t.Errorf("after unlink = %+v", l)
	}
	mu.Lock()
	if refresh != "" {
		t.Error("tak.gov wasn't told")
	}
	mu.Unlock()
	if code, _ := call("GET", "/admin/api/takgov/plugins?product=ATAK-CIV&product_version=5.6.0", nil); code != 502 {
		t.Errorf("plugins after unlinking = %d", code)
	}
}
