package federation

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// A fake OpenTAKServer: a TAK-style login on its port, no TAK Server admin
// API (nginx answers 404), and the federation plugin's API.
func TestTrustOnOpenTAKServer(t *testing.T) {
	var created map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"access_token": "oauth"})
	})
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var login map[string]string
		json.NewDecoder(r.Body).Decode(&login)
		if login["password"] != "secret" {
			w.WriteHeader(400)
			w.Write([]byte(`{"meta":{"code":400},"response":{"errors":["Invalid username or password"]}}`))
			return
		}
		w.Write([]byte(`{"meta":{"code":200},"response":{"csrf_token":"x","user":{"username":"admin","authentication_token":"tok"}}}`))
	})
	plugin := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authentication-Token") != "tok" {
				w.WriteHeader(401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET "+otsPluginAPI+"/federates", plugin(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	mux.HandleFunc("POST "+otsPluginAPI+"/federates", plugin(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&created)
		json.NewEncoder(w).Encode(created)
	}))
	mux.HandleFunc("GET "+otsPluginAPI+"/status", plugin(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"listener":{"enabled":false,"running":false,"port":9001}}`))
	}))
	srv := httptest.NewTLSServer(mux) // everything else: 404, like OTS's nginx
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	admin := func(pw string) *TAKAdmin {
		return &TAKAdmin{Address: host, Port: p, CAPEM: caPEM, Username: "admin", Password: pw}
	}
	ca, cert := []byte("ca"), []byte("cert")

	a := admin("secret")
	if err := a.SignIn(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.TrustEdgeTAK(ca, cert, nil); !errors.Is(err, ErrNoTAKAPI) {
		t.Fatalf("TrustEdgeTAK on OpenTAKServer: %v, want ErrNoTAKAPI", err)
	}
	res, err := a.TrustOnOpenTAKServer("EdgeTAK Alpha", ca, cert, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Server != "OpenTAKServer" || !res.Uploaded || !res.ListenerOff || res.ListenerPort != 9001 {
		t.Errorf("result = %+v", res)
	}
	if created["direction"] != "inbound" || created["fingerprint"] != Fingerprint(cert) || created["name"] != "EdgeTAK Alpha" {
		t.Errorf("created = %v", created)
	}
	// Only the plugin's own federate settings: it rejects unknown ones.
	for k := range created {
		switch k {
		case "name", "enabled", "direction", "fingerprint", "ca_pem", "inbound_groups", "outbound_groups":
		default:
			t.Errorf("unexpected setting %q", k)
		}
	}
	if _, err := admin("wrong").TrustOnOpenTAKServer("x", ca, cert, nil); err == nil || errors.Is(err, ErrNotOTS) {
		t.Errorf("wrong password: %v", err)
	}
}
