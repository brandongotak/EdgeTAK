package app

import (
	"encoding/pem"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/federation"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

func caPEM(a *App) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.CA.Cert.Raw}))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// federateTo makes from connect to to's listener.
func federateTo(t *testing.T, from, to *App, id string) {
	t.Helper()
	host, port, _ := net.SplitHostPort(to.Federation.ListenerAddr())
	p, _ := strconv.Atoi(port)
	if err := from.Store.PutFederate(store.Federate{ID: id, Name: "to " + id, Enabled: true, Direction: federation.Outbound,
		Address: host, Port: p, AuthMode: "certificate", UseTLS: true, CAPEM: caPEM(to), SendMode: "all"}); err != nil {
		t.Fatal(err)
	}
	from.Federation.Reload()
}

func TestFederationBetweenEdgeTAKs(t *testing.T) {
	a, b := startApp(t), startApp(t)
	for _, app := range []*App{a, b} {
		for _, u := range []string{"alice", "bob", "carol"} {
			app.Store.CreateUser(u, "password-"+u, nil, false)
		}
	}
	// B accepts federates; it approves A by its certificate's fingerprint.
	b.Store.SetFederationListener(true, 0)
	b.Federation.Reload()
	waitFor(t, "B's listener", func() bool { return b.Federation.ListenerAddr() != "" })
	b.Store.PutFederate(store.Federate{ID: "from-a", Name: "Server A", Enabled: true, Direction: federation.Inbound,
		Fingerprint: federation.Fingerprint(a.CA.ServerCert.Certificate[0]), SendMode: "all"})
	b.Federation.Reload()
	federateTo(t, a, b, "b")
	waitFor(t, "A connected to B", func() bool { return a.Federation.Status().Federates["b"].State == "connected" })
	waitFor(t, "B sees A", func() bool { return b.Federation.Status().Federates["from-a"].State == "connected" })
	waitFor(t, "A learns B's name", func() bool { return a.Federation.Status().Federates["b"].RemoteName == b.Cfg.ServerName })

	aliceCert := enroll(t, a, "alice", "password-alice")
	bobCert := enroll(t, b, "bob", "password-bob")
	carolCert := enroll(t, b, "carol", "password-carol")
	alice, bob, carol := mustDial(t, a, &aliceCert), mustDial(t, b, &bobCert), mustDial(t, b, &carolCert)

	// Positions cross in both directions, with their details.
	alice.send(sa("ANDROID-alice", "ALICE", "Cyan"))
	got := bob.expect(func(e *cot.Event) bool { return e.UID == "ANDROID-alice" }, "alice's position at bob")
	if got.Callsign() != "ALICE" || got.Detail.Group == nil || got.Detail.Group.Name != "Cyan" || got.Detail.Status == nil || got.Detail.Status.Battery != 77 ||
		got.Detail.Contact.Endpoint == "" || got.Lat != 38.8977 {
		t.Errorf("alice at bob = %+v %+v", got, got.Detail)
	}
	bob.send(sa("ANDROID-bob", "BOB", "Red"))
	alice.expect(func(e *cot.Event) bool { return e.UID == "ANDROID-bob" && e.Callsign() == "BOB" }, "bob's position at alice")
	carol.send(sa("ANDROID-carol", "CAROL", "Red"))
	alice.expect(func(e *cot.Event) bool { return e.UID == "ANDROID-carol" }, "carol's position at alice")

	// B knows alice as a contact, so it can route to her.
	waitFor(t, "alice as B's contact", func() bool { return b.Federation.Status().Federates["from-a"].RemoteContacts >= 1 })

	// A direct message from alice reaches bob only.
	chat := cot.NewEvent("b-t-f", "GeoChat.ANDROID-alice.ANDROID-bob.m1", "h-g-i-g-o", time.Hour)
	chat.Detail.XML = `<__chat parent="RootContactGroup" messageId="m1" chatroom="BOB" id="ANDROID-bob" senderCallsign="ALICE"><chatgrp uid0="ANDROID-alice" uid1="ANDROID-bob" id="ANDROID-bob"/></__chat>` +
		`<remarks source="BAO.F.ATAK.ANDROID-alice" to="ANDROID-bob">over the wire</remarks><marti><dest callsign="BOB"/></marti>`
	alice.send(chat)
	dm := bob.expect(func(e *cot.Event) bool { return e.Type == "b-t-f" }, "the direct message at bob")
	if !strings.Contains(dm.Detail.XML, "over the wire") {
		t.Errorf("dm = %s", dm.Detail.XML)
	}
	carol.expectNone(func(e *cot.Event) bool { return e.Type == "b-t-f" }, "the direct message at carol")

	// An unapproved server is turned away and listed for approval.
	c := startApp(t)
	federateTo(t, c, b, "b")
	cfp := federation.Fingerprint(c.CA.ServerCert.Certificate[0])
	waitFor(t, "C pending on B", func() bool {
		for _, p := range b.Federation.Status().Pending {
			if p.Fingerprint == cfp {
				return true
			}
		}
		return false
	})
	waitFor(t, "C told why", func() bool {
		return strings.Contains(c.Federation.Status().Federates["b"].LastError, "PermissionDenied")
	})

	// When alice leaves, B's devices are told to remove her.
	alice.conn.Close()
	bob.expect(func(e *cot.Event) bool {
		return e.Type == "t-x-d-d" && strings.Contains(e.Detail.XML, `uid="ANDROID-alice"`)
	}, "alice removed at bob")

	// Messages are counted on both sides.
	if st := a.Federation.Status().Federates["b"]; st.Out == 0 || st.In == 0 {
		t.Errorf("A's counters = %+v", st)
	}
}
