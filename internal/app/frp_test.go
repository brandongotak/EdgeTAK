package app

import (
	"strings"
	"testing"
)

// TestRemoteAccessFRP covers the console side of remote access: saving an
// frpc config (never sent back, as it holds the FRP server's token),
// connecting only with one, and noticing the certificate doesn't name the
// FRP server until EdgeTAK restarts.
func TestRemoteAccessFRP(t *testing.T) {
	a := startApp(t)
	call, _ := adminJSON(t, a)
	if code, _ := call("PUT", "/admin/api/frp/enabled", map[string]bool{"on": true}); code != 400 {
		t.Errorf("connect without a config = %d", code)
	}
	if code, _ := call("PUT", "/admin/api/frp/config", map[string]string{"config": "no server here"}); code != 400 {
		t.Errorf("config without serverAddr = %d", code)
	}
	cfg := "serverAddr = \"relay.example.org\"\nserverPort = 7000\nauth.token = \"super-secret-token\"\n\n[[proxies]]\nname = \"x\"\ntype = \"tcp\"\nlocalPort = 8089\nremotePort = 8089\n"
	code, v := call("PUT", "/admin/api/frp/config", map[string]string{"config": cfg})
	if code != 200 || v["server_addr"] != "relay.example.org" || v["cert_missing"] != true || strings.Contains(mustJSON(v), "super-secret-token") {
		t.Fatalf("save = %d %v", code, v)
	}
	if ports, _ := v["remote_ports"].([]any); len(ports) != 1 || ports[0] != "8089" {
		t.Errorf("remote ports = %v", v["remote_ports"])
	}
	// Connecting is kept; the FRP server joins the certificate's names at
	// the next start.
	if code, v := call("PUT", "/admin/api/frp/enabled", map[string]bool{"on": true}); code != 200 || v["status"].(map[string]any)["enabled"] != true {
		t.Errorf("connect = %d %v", code, v)
	}
	if !a.Store.FRPEnabled() {
		t.Error("connecting wasn't kept")
	}
	if code, _ := call("DELETE", "/admin/api/frp/config", nil); code != 200 || a.Store.FRPEnabled() {
		t.Errorf("remove = %d, still enabled %v", code, a.Store.FRPEnabled())
	}
}
