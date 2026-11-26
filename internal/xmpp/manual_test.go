package xmpp

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestWriteManual writes a configuration for trying Prosody by hand:
// EDGETAK_XMPP_OUT=dir go test -run WriteManual ./internal/xmpp
func TestWriteManual(t *testing.T) {
	dir := os.Getenv("EDGETAK_XMPP_OUT")
	if dir == "" {
		t.Skip("EDGETAK_XMPP_OUT not set")
	}
	m := &Manager{Dir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := m.Configure(Config{Domain: "tak.test", PublicHost: "127.0.0.1", Port: 15222, HTTPSPort: 15281, HTTPPort: 15280,
		UploadMB: 5, UploadDays: 2, HistoryDays: 3, UserRooms: true, AuthURL: "http://127.0.0.1:18999/auth", Secret: "s3cret",
		CertFile: dir + "/cert.pem", KeyFile: dir + "/key.pem"})
	if err != nil {
		t.Fatal(err)
	}
	err = m.WriteState(State{Users: map[string]string{"alice": "Alice", "bob": "Bob", "carol": "Carol"},
		Groups: map[string][]string{"Cyan": {"alice", "bob"}, "Red": {"carol"}},
		Rooms:  []Room{{Node: "ops", Name: "Operations", Description: "Cyan only", MembersOnly: true, Members: []string{"alice", "bob"}}, {Node: "lobby", Name: "Lobby"}}})
	if err != nil {
		t.Fatal(err)
	}
}
