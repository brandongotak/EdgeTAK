package app

import (
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// TestPackageGroups covers choosing which groups a data package is for.
func TestPackageGroups(t *testing.T) {
	a := startApp(t)
	for user, group := range map[string]string{"rita": "Red", "bob": "Blue"} {
		if _, err := a.Store.CreateUser(user, "password-"+user, []string{group}, false); err != nil {
			t.Fatal(err)
		}
	}
	hash := "ab" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"
	if _, err := a.Store.PutPackage(store.Package{Hash: hash, Name: "plan.zip", Groups: []string{"Red"}, Submitted: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rita := newAPI(t, a, enroll(t, a, "rita", "password-rita"))
	bob := newAPI(t, a, enroll(t, a, "bob", "password-bob"))
	sees := func(c *apiClient) bool {
		st, _, _ := c.do("GET", "/Marti/sync/missionquery?hash="+hash, nil)
		return st == 200
	}
	if !sees(rita) || sees(bob) {
		t.Fatal("before: only Red should see the package")
	}

	call, _ := adminJSON(t, a)
	for name, groups := range map[string][]string{"none": {}, "unknown": {"Green"}} {
		if st, _ := call("PUT", "/admin/api/packages/"+hash+"/groups", map[string]any{"groups": groups}); st != 400 {
			t.Errorf("%s groups = %d", name, st)
		}
	}
	if st, _ := call("PUT", "/admin/api/packages/"+"cd"+hash[2:]+"/groups", map[string]any{"groups": []string{"Blue"}}); st != 404 {
		t.Errorf("unknown package = %d", st)
	}
	if st, p := call("PUT", "/admin/api/packages/"+hash+"/groups", map[string]any{"groups": []string{"Blue", "Blue"}}); st != 200 || len(p["groups"].([]any)) != 1 {
		t.Fatalf("set groups = %d %v", st, p)
	}
	if sees(rita) || !sees(bob) {
		t.Error("after: only Blue should see the package")
	}
}
