package mesh

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// fakeNet is an in-memory Transport: tests hand it datagrams "heard" and
// read what the bridge sent.
type fakeNet struct {
	mu   sync.Mutex
	recv func(Packet)
	sent []sentPacket
}

type sentPacket struct {
	ifname, addr string
	data         []byte
}

func (f *fakeNet) Open(_ context.Context, _ store.MeshSettings, recv func(Packet)) ([]Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recv = recv
	return []Interface{{Name: "radio0", Addr: "10.0.0.5"}}, nil
}

func (f *fakeNet) Send(ifname, addr string, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentPacket{ifname, addr, append([]byte(nil), b...)})
	return nil
}

func (f *fakeNet) Close() {}

func (f *fakeNet) hear(data []byte, from, port string) {
	f.mu.Lock()
	recv := f.recv
	f.mu.Unlock()
	addr, _ := net.ResolveUDPAddr("udp4", from)
	recv(Packet{Data: data, From: addr, Port: port})
}

func (f *fakeNet) take() []sentPacket {
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sent
	f.sent = nil
	return out
}

const meshSA = `<?xml version='1.0' encoding='UTF-8' standalone='yes'?>
<event version="2.0" uid="ANDROID-mesh1" type="a-f-G-U-C" how="h-e" time="2026-10-26T00:00:00Z" start="2026-10-26T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="1" lon="2" hae="0" ce="9" le="9"/><detail><contact callsign="Radio 1" endpoint="10.0.0.9:4242:tcp"/><takv device="x" platform="ATAK-CIV" os="34" version="5.6"/></detail></event>`

func serverSA(uid, callsign string) *cot.Event {
	ev, err := cot.ParseXML([]byte(`<event version="2.0" uid="` + uid + `" type="a-f-G-U-C" how="m-g" time="2026-10-26T00:00:00Z" start="2026-10-26T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="3" lon="4" hae="0" ce="9" le="9"/><detail><contact callsign="` + callsign + `" endpoint="*:-1:stcp"/><takv device="y" platform="ATAK-CIV" os="34" version="5.6"/></detail></event>`))
	if err != nil {
		panic(err)
	}
	return ev
}

type rig struct {
	b    *Bridge
	net  *fakeNet
	r    *router.Router
	now  time.Time
	user *router.Client // a server device in Cyan
}

func newRig(t *testing.T) *rig {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := &rig{net: &fakeNet{}, r: router.New(log, true), now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	g.b = &Bridge{Router: g.r, Log: log, Transport: g.net, AnonGroup: "__ANON__", Now: func() time.Time { return g.now }}
	g.user = router.NewClient("u", "tls", "alice", []string{"Cyan"}, []string{"Cyan"}, nil)
	g.r.Register(g.user)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g.b.Start(ctx, store.MeshSettings{Enabled: true, Group: "Cyan", SendPositions: true, SendChat: true})
	return g
}

func drain(c *router.Client) []*cot.Event {
	var out []*cot.Event
	for {
		select {
		case ev := <-c.Queue():
			out = append(out, ev)
		case <-time.After(30 * time.Millisecond):
			return out
		}
	}
}

func TestMeshToServer(t *testing.T) {
	g := newRig(t)
	drain(g.user)
	g.net.hear([]byte(meshSA), "10.0.0.9:6969", "sa")
	got := drain(g.user)
	if len(got) != 1 || got[0].UID != "ANDROID-mesh1" {
		t.Fatalf("server device got %v", got)
	}
	// Replayed to a device connecting later.
	late := router.NewClient("l", "tls", "bob", []string{"Cyan"}, nil, nil)
	g.r.Register(late)
	if ev := drain(late); len(ev) != 1 || ev[0].UID != "ANDROID-mesh1" {
		t.Errorf("late device got %v", ev)
	}
	// TAK Protocol works too.
	ev, _ := cot.ParseXML([]byte(strings.Replace(meshSA, "ANDROID-mesh1", "ANDROID-mesh2", 1)[strings.Index(meshSA, "<event"):]))
	g.net.hear(append([]byte{0xbf, 0x01, 0xbf}, ev.TakMessage()...), "10.0.0.10:6969", "sa")
	if got := drain(g.user); len(got) != 1 || got[0].UID != "ANDROID-mesh2" {
		t.Errorf("protobuf from the mesh: %v", got)
	}
	st := g.b.Status()
	if st.FromMesh != 2 || len(st.Peers) != 2 {
		t.Errorf("status %+v", st)
	}
}

func TestNoLoops(t *testing.T) {
	g := newRig(t)
	g.take(t)
	// What came from the mesh isn't sent back to it.
	g.net.hear([]byte(meshSA), "10.0.0.9:6969", "sa")
	if s := g.net.take(); len(s) != 0 {
		t.Errorf("mesh traffic went back to the mesh: %d", len(s))
	}
	// The bridge's own datagrams, heard back, are dropped.
	g.r.Route(g.user, serverSA("ANDROID-alice", "Alice"))
	sent := g.net.take()
	if len(sent) != 1 {
		t.Fatalf("sent %d", len(sent))
	}
	drain(g.user)
	g.net.hear(sent[0].data, "10.0.0.5:6969", "sa")
	if got := drain(g.user); len(got) != 0 {
		t.Errorf("the bridge's own datagram came back in: %v", got)
	}
	// A device on both the mesh and the server isn't doubled.
	g.user.Observe(serverSA("ANDROID-alice", "Alice"))
	dual := strings.Replace(meshSA, "ANDROID-mesh1", "ANDROID-alice", 1)
	g.net.hear([]byte(dual), "10.0.0.11:6969", "sa")
	other := router.NewClient("o", "tls", "carol", []string{"Cyan"}, nil, nil)
	g.r.Register(other)
	drain(other)
	g.net.hear([]byte(strings.Replace(dual, "time=", "how2=\"x\" time=", 1)), "10.0.0.11:6969", "sa")
	for _, ev := range drain(other) {
		if ev.UID == "ANDROID-alice" {
			t.Error("a server device's mesh copy came in too")
		}
	}
}

func (g *rig) take(t *testing.T) { g.net.take() }

func TestServerToMesh(t *testing.T) {
	g := newRig(t)
	g.net.take()
	g.r.Route(g.user, serverSA("ANDROID-alice", "Alice"))
	sent := g.net.take()
	if len(sent) != 1 || sent[0].addr != "239.2.3.1:6969" || sent[0].ifname != "radio0" {
		t.Fatalf("sent %+v", sent)
	}
	if !bytes.HasPrefix(sent[0].data, []byte{0xbf, 0x01, 0xbf}) {
		t.Errorf("not TAK Protocol: %q", sent[0].data[:8])
	}
	ev, _, err := decode(sent[0].data)
	if err != nil {
		t.Fatal(err)
	}
	// Mesh devices reach the contact through the bridge.
	if ev.Detail.Contact.Endpoint != "10.0.0.5:4242:tcp" {
		t.Errorf("endpoint %q", ev.Detail.Contact.Endpoint)
	}

	// Positions at most every 10 s per item.
	g.r.Route(g.user, serverSA("ANDROID-alice", "Alice"))
	if s := g.net.take(); len(s) != 0 {
		t.Errorf("position sent again at once")
	}
	g.now = g.now.Add(11 * time.Second)
	g.r.Route(g.user, serverSA("ANDROID-alice", "Alice"))
	if s := g.net.take(); len(s) != 1 {
		t.Errorf("position not sent after 11 s")
	}

	// Chat goes to the chat group; drawings aren't sent (not ticked).
	chat, _ := cot.ParseXML([]byte(`<event version="2.0" uid="GeoChat.x.All Chat Rooms.1" type="b-t-f" how="h-g-i-g-o" time="2026-10-26T00:00:00Z" start="2026-10-26T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="0" lon="0" hae="0" ce="9" le="9"/><detail><__chat id="All Chat Rooms" chatroom="All Chat Rooms"/></detail></event>`))
	g.r.Route(g.user, chat)
	shape, _ := cot.ParseXML([]byte(`<event version="2.0" uid="shape1" type="u-d-f" how="h-e" time="2026-10-26T00:00:00Z" start="2026-10-26T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="0" lon="0" hae="0" ce="9" le="9"/><detail/></event>`))
	g.r.Route(g.user, shape)
	sent = g.net.take()
	if len(sent) != 1 || sent[0].addr != "224.10.10.1:17012" {
		t.Errorf("chat/shape: %+v", sent)
	}

	// A device that only speaks XML turns the bridge to XML, until it's quiet for 2 minutes.
	g.net.hear([]byte(strings.Replace(meshSA, "ANDROID-mesh1", "OLD-1", 1)), "10.0.0.20:6969", "sa")
	g.now = g.now.Add(11 * time.Second)
	g.r.Route(g.user, serverSA("ANDROID-alice", "Alice"))
	sent = g.net.take()
	if len(sent) != 1 || sent[0].data[0] != '<' {
		t.Errorf("not XML with an XML-only device about")
	}
	if st := g.b.Status(); st.Format != "XML" || st.XMLPeer != "10.0.0.20" {
		t.Errorf("status %s %s", st.Format, st.XMLPeer)
	}
	g.now = g.now.Add(3 * time.Minute)
	g.r.Route(g.user, serverSA("ANDROID-alice", "Alice"))
	if sent = g.net.take(); len(sent) != 1 || sent[0].data[0] != 0xbf {
		t.Errorf("not back to TAK Protocol")
	}
}

func TestDirectChat(t *testing.T) {
	g := newRig(t)
	g.user.Observe(serverSA("ANDROID-alice", "Alice"))
	bob := router.NewClient("b", "tls", "bob", []string{"Cyan"}, nil, nil)
	g.r.Register(bob)
	drain(g.user)
	drain(bob)
	dm := `<event version="2.0" uid="GeoChat.ANDROID-mesh1.ANDROID-alice.1" type="b-t-f" how="h-g-i-g-o" time="2026-10-26T00:00:00Z" start="2026-10-26T00:00:00Z" stale="2099-01-01T00:00:00Z"><point lat="0" lon="0" hae="0" ce="9" le="9"/><detail><__chat id="ANDROID-alice" chatroom="Alice" senderCallsign="Radio 1"/><remarks>hi</remarks></detail></event>`
	g.net.hear([]byte(dm), "10.0.0.9:51000", "direct")
	if got := drain(g.user); len(got) != 1 {
		t.Errorf("Alice got %d", len(got))
	}
	if got := drain(bob); len(got) != 0 {
		t.Errorf("Bob got Alice's direct message")
	}
}

func TestOffAndRestart(t *testing.T) {
	g := newRig(t)
	g.b.Apply(store.MeshSettings{Enabled: false})
	if g.b.Status().Running || len(g.r.Taps()) != 0 {
		t.Error("still running")
	}
	g.b.Apply(store.MeshSettings{Enabled: true})
	if !g.b.Status().Running || len(g.r.Taps()) != 1 {
		t.Error("not running again")
	}
}

func TestCategory(t *testing.T) {
	for typ, want := range map[string]string{"a-f-G-U-C": "positions", "b-t-f": "chat", "b-a-o-tbl": "alerts",
		"u-d-f": "drawings", "b-m-r": "drawings", "t-x-c-t": "control", "t-x-d-d": "positions", "b-i-v": "other"} {
		if got := category(typ); got != want {
			t.Errorf("%s: %s, want %s", typ, got, want)
		}
	}
}
