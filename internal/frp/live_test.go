package frp

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveFRP runs the real frpc (through Manager) against a real frps:
// FRP_LIVE_DIR holds both from the release.
func TestLiveFRP(t *testing.T) {
	dir := os.Getenv("FRP_LIVE_DIR")
	if dir == "" {
		t.Skip("FRP_LIVE_DIR not set")
	}
	// A local service to reach through the tunnel.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("hello from edgetak"))
			c.Close()
		}
	}()
	local := ln.Addr().(*net.TCPAddr).Port

	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "frps.toml"), []byte("bindPort = 17000\nauth.method = \"token\"\nauth.token = \"t0ken\"\n"), 0o600)
	frps := exec.Command(filepath.Join(dir, "frps"), "-c", filepath.Join(work, "frps.toml"))
	frps.Stdout, frps.Stderr = os.Stdout, os.Stderr
	if err := frps.Start(); err != nil {
		t.Fatal(err)
	}
	defer frps.Process.Kill()
	time.Sleep(500 * time.Millisecond)

	m := newManager(t)
	os.MkdirAll(m.BinDir, 0o750)
	bin, _ := os.ReadFile(filepath.Join(dir, "frpc"))
	os.WriteFile(m.Binary(), bin, 0o755)
	cfg := fmt.Sprintf(`serverAddr = "127.0.0.1"
serverPort = 17000
auth.method = "token"
auth.token = "t0ken"

[[proxies]]
name = "edge-streaming"
type = "tcp"
localIP = "127.0.0.1"
localPort = %d
remotePort = 18089

[[proxies]]
name = "edge-api"
type = "tcp"
localIP = "127.0.0.1"
localPort = %d
remotePort = 22
`, local, local)
	if err := m.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx, true)
	var st Status
	for i := 0; i < 100; i++ {
		if st = m.Status(); st.Connected && len(st.Proxies) == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("status: connected=%v proxies=%v", st.Connected, st.Proxies)
	for _, l := range st.Log {
		t.Log("frpc: " + l)
	}
	if !st.Connected || st.Proxies["edge-streaming"] != "ok" || st.Proxies["edge-api"] == "ok" {
		t.Fatalf("status = %+v", st)
	}
	c, err := net.DialTimeout("tcp", "127.0.0.1:18089", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(c)
	c.Close()
	if string(got) != "hello from edgetak" {
		t.Fatalf("through the tunnel: %q", got)
	}
	// A wrong token is reported.
	m.SaveConfig(context.Background(), cfg[:0]+"serverAddr = \"127.0.0.1\"\nserverPort = 17000\nauth.method = \"token\"\nauth.token = \"wrong\"\n")
	for i := 0; i < 100; i++ {
		if st = m.Status(); st.LoginError != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("wrong token: %q", st.LoginError)
	if st.LoginError == "" || st.Connected {
		t.Errorf("wrong token not reported: %+v", st)
	}
}
