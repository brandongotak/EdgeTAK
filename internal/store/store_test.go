package store

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestEffectiveGroups(t *testing.T) {
	u := &User{}
	if in, out := u.Effective("__ANON__"); !slices.Equal(in, []string{"__ANON__"}) || !slices.Equal(out, []string{"__ANON__"}) {
		t.Errorf("no groups = %v %v", in, out)
	}
	u.SetDirection("Ops", "both")
	u.SetDirection("Intel", "in")
	u.SetDirection("Cmd", "out")
	in, out := u.Effective("__ANON__")
	if !slices.Equal(in, []string{"Ops", "Intel"}) || !slices.Equal(out, []string{"Ops", "Cmd"}) {
		t.Errorf("effective = in %v out %v", in, out)
	}
	u.SetDirection("Ops", "in") // changing direction replaces the old one
	if u.Direction("Ops") != "in" || slices.Contains(u.Groups, "Ops") {
		t.Errorf("direction change: %+v", u)
	}
	u.SetDirection("Ops", "")
	if u.Direction("Ops") != "" {
		t.Error("removal failed")
	}
	// Receive-only users send nowhere (they are not put in __ANON__).
	ro := &User{GroupsIn: []string{"Ops"}}
	if _, out := ro.Effective("__ANON__"); len(out) != 0 {
		t.Errorf("receive-only out = %v", out)
	}
}

func TestGroupLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.CreateUser("a", "password-a", []string{"Ops"}, false) // creates the Ops record implicitly
	s.CreateUser("b", "password-b", nil, false)
	if _, ok := s.Group("Ops"); !ok {
		t.Fatal("group not created from user membership")
	}
	if err := s.SetMembership("Ops", "b", "in"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMembership("Nope", "b", "in"); err == nil {
		t.Error("membership in unknown group accepted")
	}
	members := func(name string) []Member {
		for _, g := range s.Groups() {
			if g.Name == name {
				return g.Members
			}
		}
		return nil
	}
	if m := members("Ops"); len(m) != 2 || m[0] != (Member{"a", "both"}) || m[1] != (Member{"b", "in"}) {
		t.Errorf("members = %v", m)
	}

	s.PutPackage(Package{Hash: "h1", Groups: []string{"Ops"}})
	affected, err := s.UpdateGroup("Ops", "Operations", "main team")
	if err != nil || len(affected) != 2 {
		t.Fatalf("rename: %v %v", affected, err)
	}
	if u, _ := s.User("b"); u.Direction("Operations") != "in" {
		t.Errorf("rename lost direction: %+v", u)
	}
	if p, _ := s.Package("h1"); p.Groups[0] != "Operations" {
		t.Errorf("package groups not renamed: %v", p.Groups)
	}
	if _, err := s.UpdateGroup("Operations", "Operations", "desc only"); err != nil {
		t.Errorf("description update: %v", err)
	}

	// Reopen: memberships and group records persist.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := s2.Group("Operations"); !ok || g.Description != "desc only" {
		t.Errorf("reloaded group = %+v", g)
	}
	affected, err = s2.DeleteGroup("Operations")
	if err != nil || len(affected) != 2 {
		t.Fatalf("delete: %v %v", affected, err)
	}
	if u, _ := s2.User("a"); len(u.Groups) != 0 {
		t.Errorf("membership survived delete: %+v", u)
	}
}
