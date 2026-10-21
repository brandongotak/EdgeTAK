package app

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"io"
	"mime/multipart"
	"net"
	"net/http"
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

	// Data packages: alice's upload is copied to B, and when she sends it to
	// bob he is offered B's copy; the same works from bob to alice.
	sendPackage := func(from *App, cert tls.Certificate, sender *streamClient, senderCallsign, to string) (string, []byte) {
		t.Helper()
		content := []byte("PK federated package from " + senderCallsign + " " + time.Now().String())
		sum := sha256.Sum256(content)
		hash := hex.EncodeToString(sum[:])
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("assetfile", "ops.zip")
		fw.Write(content)
		mw.Close()
		client := &http.Client{Transport: memTransport(&tls.Config{RootCAs: from.CA.Pool(), Certificates: []tls.Certificate{cert}})}
		resp, err := client.Post("https://"+from.Addrs["https"]+"/Marti/sync/missionupload?hash="+hash+"&filename=ops.zip&creatorUid=ANDROID-"+strings.ToLower(senderCallsign), mw.FormDataContentType(), &body)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("upload at %s: %d", senderCallsign, resp.StatusCode)
		}
		ev := cot.NewEvent("b-f-t-r", "fileshare-"+hash[:8], "h-e", 5*time.Minute)
		ev.Detail.XML = `<fileshare filename="ops.zip" senderUrl="https://` + from.Addrs["https"] + `/Marti/sync/content?hash=` + hash + `" sizeInBytes="` + strconv.Itoa(len(content)) +
			`" sha256="` + hash + `" senderUid="ANDROID-` + strings.ToLower(senderCallsign) + `" senderCallsign="` + senderCallsign + `" name="ops"/><marti><dest callsign="` + to + `"/></marti>`
		sender.send(ev)
		return hash, content
	}
	receivePackage := func(at *App, cert tls.Certificate, c *streamClient, hash string, content []byte, who string) {
		t.Helper()
		offer := c.expect(func(e *cot.Event) bool { return e.Type == "b-f-t-r" && strings.Contains(e.Detail.XML, hash) }, "the package offer at "+who)
		_, port, _ := net.SplitHostPort(at.Addrs["https"])
		if !strings.Contains(offer.Detail.XML, ":"+port+"/Marti/sync/content?hash="+hash) {
			t.Errorf("%s's offer does not point at their own server: %s", who, offer.Detail.XML)
		}
		client := &http.Client{Transport: memTransport(&tls.Config{RootCAs: at.CA.Pool(), Certificates: []tls.Certificate{cert}})}
		resp, err := client.Get("https://" + at.Addrs["https"] + "/Marti/sync/content?hash=" + hash)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Equal(got, content) {
			t.Errorf("%s's download: %d %q", who, resp.StatusCode, got)
		}
	}
	hash, content := sendPackage(a, aliceCert, alice, "ALICE", "BOB")
	waitFor(t, "alice's package copied to B", func() bool { _, ok := b.Store.Package(hash); return ok })
	if p, _ := b.Store.Package(hash); p.Name != "ops.zip" || !strings.Contains(p.Submitter, "alice") {
		t.Errorf("B's copy = %+v", p)
	}
	receivePackage(b, bobCert, bob, hash, content, "bob")
	carol.expectNone(func(e *cot.Event) bool { return e.Type == "b-f-t-r" }, "the package offer at carol")
	hash, content = sendPackage(b, bobCert, bob, "BOB", "ALICE")
	receivePackage(a, aliceCert, alice, hash, content, "alice")

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
