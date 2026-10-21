package federation

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenTAKServer has no TAK Server admin API; its federation plugin
// (ots_federation_plugin) has its own. Through it, TrustOnOpenTAKServer
// makes EdgeTAK an approved inbound federate there, matched by its
// certificate's fingerprint, as TrustEdgeTAK does on a TAK Server.

const otsPluginAPI = "/api/plugins/ots_federation_plugin"

// ErrNotOTS: no OpenTAKServer login on the ports tried.
var ErrNotOTS = errors.New("not an OpenTAKServer")

// otsSignIn logs in to OpenTAKServer's web API (Flask-Security) for a token.
// The API is usually on the web UI's port (443) rather than 8446.
func (t *TAKAdmin) otsSignIn() error {
	if t.client == nil {
		t.client = t.httpClient()
	}
	ports := []int{t.Port}
	if t.Port != 443 {
		ports = append(ports, 443)
	}
	login, _ := json.Marshal(map[string]string{"username": t.Username, "password": t.Password})
	for _, port := range ports {
		t.Port = port
		req, _ := http.NewRequest("POST", t.base()+"/api/login?include_auth_token", bytes.NewReader(login))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := t.client.Do(req)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		var out struct {
			Response *struct {
				User struct {
					Token string `json:"authentication_token"`
				} `json:"user"`
				TwoFactor bool `json:"tf_required"`
			} `json:"response"`
		}
		if resp.StatusCode == 404 || json.Unmarshal(data, &out) != nil || out.Response == nil {
			continue // not OpenTAKServer's API
		}
		switch {
		case out.Response.TwoFactor:
			return errors.New("the OpenTAKServer account uses two-factor login, which EdgeTAK can't do here: " +
				"add EdgeTAK on the federation plugin's page instead (an inbound federate with EdgeTAK's certificate fingerprint and CA, shown under This server)")
		case resp.StatusCode == 400 || resp.StatusCode == 401:
			return errors.New("OpenTAKServer rejected the username or password")
		case resp.StatusCode != 200 || out.Response.User.Token == "":
			return fmt.Errorf("OpenTAKServer returned HTTP %d while signing in", resp.StatusCode)
		}
		t.token = out.Response.User.Token
		return nil
	}
	return ErrNotOTS
}

func (t *TAKAdmin) otsCall(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, t.base()+otsPluginAPI+path, body)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authentication-Token", t.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach OpenTAKServer: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == 404:
		return errors.New("OpenTAKServer has no federation plugin: install and enable OTS-Federation-Plugin (OpenTAKServer > Plugins)")
	case resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode >= 300 && resp.StatusCode < 400:
		return errors.New("OpenTAKServer accepted the login but refused the request: the account needs the administrator role")
	case resp.StatusCode >= 400:
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &e)
		return fmt.Errorf("OpenTAKServer's federation plugin returned HTTP %d: %s", resp.StatusCode,
			firstNonEmpty(e.Error, strings.TrimSpace(string(data[:min(len(data), 300)]))))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// TrustOnOpenTAKServer adds EdgeTAK to OpenTAKServer's federation plugin as
// an inbound federate (or updates the one there), delivering to groups.
func (t *TAKAdmin) TrustOnOpenTAKServer(name string, caDER, certDER []byte, groups []string) (*TrustResult, error) {
	if err := t.otsSignIn(); err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		groups = []string{"__ANON__"}
	}
	var feds []struct {
		ID          string `json:"id"`
		Direction   string `json:"direction"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := t.otsCall("GET", "/federates", nil, &feds); err != nil {
		return nil, err
	}
	fp := Fingerprint(certDER)
	res := &TrustResult{Server: "OpenTAKServer", Groups: groups, FederateGroups: groups}
	settings := map[string]any{"enabled": true, "inbound_groups": groups, "outbound_groups": groups}
	for _, f := range feds {
		if f.Direction == Inbound && normalizeFP(f.Fingerprint) == fp {
			res.FederateFound = true
			if err := t.otsCall("PATCH", "/federates/"+f.ID, settings, nil); err != nil {
				return nil, err
			}
		}
	}
	if !res.FederateFound {
		settings["name"], settings["direction"], settings["fingerprint"] = name, Inbound, fp
		settings["ca_pem"] = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
		if err := t.otsCall("POST", "/federates", settings, nil); err != nil {
			return nil, err
		}
		res.Uploaded = true
	}
	var st struct {
		Listener struct {
			Running bool `json:"running"`
			Port    int  `json:"port"`
		} `json:"listener"`
	}
	if t.otsCall("GET", "/status", nil, &st) == nil {
		res.ListenerOff, res.ListenerPort = !st.Listener.Running, st.Listener.Port
	}
	return res, nil
}
