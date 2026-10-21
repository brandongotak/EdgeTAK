package app

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/mission"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

func TestDataRetention(t *testing.T) {
	a := startApp(t)
	call, _ := adminJSON(t, a)
	old, recent := time.Now().Add(-40*24*time.Hour), time.Now().Add(-2*time.Hour)

	pkgDir := filepath.Join(a.DataDir, "packages")
	os.MkdirAll(pkgDir, 0o750)
	addPkg := func(hash string, at time.Time) {
		t.Helper()
		if _, err := a.Store.PutPackage(store.Package{Hash: hash, Name: hash[:6] + ".zip", Submitted: at, Groups: []string{"__ANON__"}}); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(pkgDir, hash), []byte("zip"), 0o640)
	}
	oldPkg := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	keptByMission := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	newPkg := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	addPkg(oldPkg, old)
	addPkg(keptByMission, old)
	addPkg(newPkg, recent)
	admin := mission.Caller{Username: "admin", Admin: true}
	if _, _, err := a.Missions.Create(mission.CreateParams{Name: "Active op"}, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Missions.AddContents("Active op", admin, []string{keptByMission}, nil); err != nil {
		t.Fatal(err)
	}

	a.Store.AddInbox(store.InboxMessage{ID: "1", Time: old, Text: "old"})
	a.Store.AddInbox(store.InboxMessage{ID: "2", Time: recent, Text: "new"})
	a.Store.AddRequest(store.SignupRequest{ID: "r1", Username: "late", Created: old})
	a.Store.CreateInvite(store.Invite{Code: "expired", Created: old, Expires: old.Add(time.Hour)})
	a.Store.CreateInvite(store.Invite{Code: "usable", Created: old})
	a.Store.AddCert(store.Cert{Serial: "01", CommonName: "gone", Expires: old})
	a.Store.AddCert(store.Cert{Serial: "02", CommonName: "revoked-still-valid", Expires: time.Now().Add(300 * 24 * time.Hour), Revoked: true})

	rules := map[string]int{"packages_days": 30, "inbox_days": 30, "requests_days": 14, "invites_days": 7, "certs_days": 7}
	code, prev := call("POST", "/admin/api/retention/preview", rules)
	removed := prev["removed"].(map[string]any)
	want := map[string]float64{"packages": 1, "inbox": 1, "requests": 1, "invites": 1, "certs": 1}
	for k, n := range want {
		if code != 200 || removed[k] != n {
			t.Fatalf("preview %s = %v (all: %v)", k, removed[k], removed)
		}
	}
	// A preview changes nothing.
	if len(a.Store.Packages()) != 3 {
		t.Fatal("preview removed packages")
	}

	if code, _ := call("PUT", "/admin/api/retention", rules); code != http.StatusNoContent {
		t.Fatalf("save = %d", code)
	}
	if code, res := call("POST", "/admin/api/retention/run", nil); code != 200 || res["removed"].(map[string]any)["packages"] != 1.0 {
		t.Fatalf("run = %d %v", code, res)
	}

	hashes := map[string]bool{}
	for _, p := range a.Store.Packages() {
		hashes[p.Hash] = true
	}
	if hashes[oldPkg] || !hashes[keptByMission] || !hashes[newPkg] {
		t.Fatalf("packages left = %v", hashes)
	}
	if _, err := os.Stat(filepath.Join(pkgDir, oldPkg)); !os.IsNotExist(err) {
		t.Fatal("the old package's file was not deleted")
	}
	if in := a.Store.Inbox(); len(in) != 1 || in[0].ID != "2" {
		t.Fatalf("inbox = %+v", in)
	}
	if len(a.Store.Requests()) != 0 {
		t.Fatal("old sign-up request kept")
	}
	if _, ok := a.Store.Invite("expired"); ok {
		t.Fatal("expired invite kept")
	}
	if _, ok := a.Store.Invite("usable"); !ok {
		t.Fatal("a usable invite was removed")
	}
	if !a.Store.IsRevoked("02") {
		t.Fatal("a revoked but unexpired certificate record was removed: it would work again")
	}
	if len(a.Store.Certs()) != 1 {
		t.Fatalf("certs = %+v", a.Store.Certs())
	}

	// Idle missions.
	if code, res := call("POST", "/admin/api/retention/preview", map[string]int{"missions_idle_days": 1}); code != 200 || res["removed"].(map[string]any)["missions"] != 0.0 {
		t.Fatalf("a mission edited just now would be removed: %v", res)
	}
	// Status shows settings, the history rule and what is pending.
	if code, st := call("GET", "/admin/api/retention", nil); code != 200 || st["settings"].(map[string]any)["packages_days"] != 30.0 || st["history"] == nil || st["last"] == nil {
		t.Fatalf("status = %v", st)
	}
}
