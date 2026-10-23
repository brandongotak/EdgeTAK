package web

import (
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/audit"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

func guardServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("alice", "right-password-1", nil, false); err != nil {
		t.Fatal(err)
	}
	al, _ := audit.Open(t.TempDir())
	t.Cleanup(func() { al.Close() })
	return &Server{Store: st, Audit: al, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// reqFrom is a request from ip.
func reqFrom(ip string) *http.Request {
	r := httptest.NewRequest("POST", "/admin/api/login", nil)
	r.RemoteAddr = net.JoinHostPort(ip, "40000")
	return r
}

func TestLoginProtection(t *testing.T) {
	s := guardServer(t)
	s.Store.SetLoginProtection(store.LoginProtection{MaxFailures: 3, Allow: []string{"10.0.0.0/8"}})
	try := func(ip, pw string) error {
		_, err := s.passwordSignIn(reqFrom(ip), "alice", pw)
		return err
	}

	// Two failures, then a success: the count starts again.
	try("203.0.113.5", "wrong")
	try("203.0.113.5", "wrong")
	if err := try("203.0.113.5", "right-password-1"); err != nil {
		t.Fatalf("right password = %v", err)
	}
	// Three failures block the address, even for the right password.
	for range 3 {
		try("203.0.113.5", "wrong")
	}
	if err := try("203.0.113.5", "right-password-1"); !errors.Is(err, errBlocked) {
		t.Fatalf("after 3 failures = %v", err)
	}
	// Other addresses are unaffected; the allow list and this machine never block.
	if err := try("198.51.100.7", "right-password-1"); err != nil {
		t.Errorf("another address = %v", err)
	}
	for range 5 {
		try("10.1.2.3", "wrong")
		try("127.0.0.1", "wrong")
	}
	if err := try("10.1.2.3", "right-password-1"); err != nil {
		t.Errorf("allow-listed address = %v", err)
	}
	if err := try("127.0.0.1", "right-password-1"); err != nil {
		t.Errorf("loopback = %v", err)
	}
	// The block is recorded, shown, and can be lifted.
	if list, _ := s.Audit.Query(audit.Filter{Text: "Address blocked"}); len(list) != 1 || list[0].Actor != "203.0.113.5" {
		t.Errorf("audit = %+v", list)
	}
	v := s.loginProtectionView()
	if b := v["blocked"].([]blockedView); len(b) != 1 || b[0].Address != "203.0.113.5" || b[0].LastUser != "alice" {
		t.Errorf("blocked = %+v", v["blocked"])
	}
	if !s.unblock("203.0.113.5") {
		t.Fatal("unblock")
	}
	if err := try("203.0.113.5", "right-password-1"); err != nil {
		t.Errorf("after unblocking = %v", err)
	}
	// Blocks expire.
	for range 3 {
		try("203.0.113.9", "wrong")
	}
	if u := s.blockedUntil("203.0.113.9", time.Now().Add(31*time.Minute)); !u.IsZero() {
		t.Errorf("still blocked after 31 minutes: %v", u)
	}
	// Video sign-ins (RTSP/RTMP/SRT, via MediaMTX) count and are refused too.
	s.video.authCache = map[string]time.Time{}
	for range 3 {
		s.checkVideoPassword("192.0.2.80", "alice", "wrong")
	}
	if _, ok := s.checkVideoPassword("192.0.2.80", "alice", "right-password-1"); ok {
		t.Error("a blocked address signed in to video")
	}
	if _, ok := s.checkVideoPassword("192.0.2.81", "alice", "right-password-1"); !ok {
		t.Error("video sign-in from another address failed")
	}
	// Turned off: nothing is counted.
	s.Store.SetLoginProtection(store.LoginProtection{Off: true, MaxFailures: 1})
	try("192.0.2.1", "wrong")
	if err := try("192.0.2.1", "right-password-1"); err != nil {
		t.Errorf("with protection off = %v", err)
	}
}

func TestDenyList(t *testing.T) {
	s := guardServer(t)
	s.Store.SetLoginProtection(store.LoginProtection{Deny: []string{"203.0.113.0/24", "2001:db8::1"}})
	for remote, want := range map[string]bool{"203.0.113.77:5000": true, "198.51.100.1:5000": false, "[2001:db8::1]:8089": true,
		"127.0.0.1:1": false, "[::ffff:203.0.113.2]:9": true} {
		if got := s.AddressRefused(remote); got != want {
			t.Errorf("%s refused = %v, want %v", remote, got, want)
		}
	}
	if _, err := cleanAddresses([]string{"10.0.0.0/8", " 192.168.1.5 ", "nonsense"}); err == nil || !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("bad entry accepted: %v", err)
	}
}

// TestPasswordsNeedTLS: a username and password sent over the plain HTTP
// port from another machine are ignored; over TLS, or from this machine,
// they work.
func TestPasswordsNeedTLS(t *testing.T) {
	s := guardServer(t)
	r := reqFrom("203.0.113.5")
	r.SetBasicAuth("alice", "right-password-1")
	if id := s.identify(r); id != nil {
		t.Errorf("plain HTTP from another machine signed in as %s", id.Username)
	}
	r.TLS = &tls.ConnectionState{}
	if id := s.identify(r); id == nil || id.Username != "alice" {
		t.Errorf("over TLS = %+v", id)
	}
	r = reqFrom("127.0.0.1")
	r.SetBasicAuth("alice", "right-password-1")
	if id := s.identify(r); id == nil {
		t.Error("from this machine over plain HTTP was refused")
	}
}

func TestSecurityHeadersAndInitialPassword(t *testing.T) {
	s := guardServer(t)
	s.DataDir = t.TempDir()
	h := s.Handler(true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	for k, want := range map[string]string{"X-Frame-Options": "DENY", "Content-Security-Policy": "frame-ancestors 'none'", "Referrer-Policy": "same-origin"} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	p := filepath.Join(s.DataDir, "initial-admin-password.txt")
	os.WriteFile(p, []byte("username: admin\npassword: x\n"), 0o600)
	s.forgetInitialPassword("alice")
	if _, err := os.Stat(p); err != nil {
		t.Fatal("removed for another user's password change")
	}
	s.forgetInitialPassword("admin")
	if _, err := os.Stat(p); err == nil {
		t.Error("initial-admin-password.txt kept after the admin password changed")
	}
}
