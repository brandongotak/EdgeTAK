package federation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Helpers that set up a TAK Server for federation from EdgeTAK's console,
// so nothing has to be copied between the servers by hand (as in the OTS
// federation plugin):
//   - fetch the CA a server presents (trust on first use: the admin confirms
//     the fingerprint);
//   - sign in as a TAK Server admin (OAuth2 password flow on /oauth/token, as
//     its web login does; it turns basic auth off by default) and generate a
//     federation token, or upload EdgeTAK's CA as a federate CA with groups.
// The admin's username and password are only used for that request.

// FetchedCA is a CA certificate a server presented.
type FetchedCA struct {
	PEM         string `json:"pem"`
	Subject     string `json:"subject"`
	Fingerprint string `json:"fingerprint"`
	Ports       []int  `json:"ports"`
}

// FetchResult is what FetchCAs found.
type FetchResult struct {
	CAPEM               string            `json:"ca_pem"`
	CAs                 []FetchedCA       `json:"cas"`
	ServerNames         []string          `json:"server_names"`
	SuggestedServerName string            `json:"suggested_tls_server_name,omitempty"`
	Errors              map[string]string `json:"errors,omitempty"`
}

func certNames(c *x509.Certificate) []string {
	names := append([]string{}, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	if c.Subject.CommonName != "" && !slices.Contains(names, c.Subject.CommonName) {
		names = append([]string{c.Subject.CommonName}, names...)
	}
	return names
}

func pemOf(c *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
}

// presentedChain connects without verifying and returns the chain the
// server sends. Ports that need a client certificate fail the handshake
// after that, which does not matter here.
func presentedChain(dial func(string) (net.Conn, error), address string, port int) ([]*x509.Certificate, error) {
	conn, err := dial(net.JoinHostPort(address, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	var chain []*x509.Certificate
	cfg := &tls.Config{InsecureSkipVerify: true, ServerName: address, VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
		for _, r := range raw {
			if c, err := x509.ParseCertificate(r); err == nil {
				chain = append(chain, c)
			}
		}
		return nil
	}}
	if net.ParseIP(address) != nil {
		cfg.ServerName = ""
	}
	hsErr := tls.Client(conn, cfg).Handshake()
	if len(chain) == 0 {
		if hsErr != nil {
			return nil, hsErr
		}
		return nil, errors.New("no certificate was presented")
	}
	return chain, nil
}

// FetchCAs gets the CA certificates a server presents on its ports (the
// federation port and the web port may use different certificates).
func FetchCAs(dial func(string) (net.Conn, error), address string, ports []int) (*FetchResult, error) {
	res := &FetchResult{Errors: map[string]string{}}
	seen := map[string]int{}
	first := true
	for _, port := range ports {
		if port <= 0 {
			continue
		}
		chain, err := presentedChain(dial, address, port)
		if err != nil {
			res.Errors[strconv.Itoa(port)] = err.Error()
			continue
		}
		if first {
			first = false
			res.ServerNames = certNames(chain[0])
			if !slices.Contains(res.ServerNames, address) && len(res.ServerNames) > 0 {
				res.SuggestedServerName = res.ServerNames[0]
			}
		}
		found := false
		for _, c := range chain[1:] {
			if !c.IsCA && !bytes.Equal(c.RawSubject, c.RawIssuer) {
				continue
			}
			found = true
			fp := Fingerprint(c.Raw)
			if i, ok := seen[fp]; ok {
				res.CAs[i].Ports = append(res.CAs[i].Ports, port)
				continue
			}
			seen[fp] = len(res.CAs)
			res.CAs = append(res.CAs, FetchedCA{PEM: pemOf(c), Subject: c.Subject.String(), Fingerprint: fp, Ports: []int{port}})
		}
		if !found {
			res.Errors[strconv.Itoa(port)] = "the server did not send its CA certificate; copy it from the server instead (TAK Server: certs/files/ca.pem)"
		}
	}
	if len(res.CAs) == 0 {
		var msgs []string
		for p, e := range res.Errors {
			msgs = append(msgs, "port "+p+": "+e)
		}
		slices.Sort(msgs)
		return nil, errors.New(strings.Join(msgs, "; "))
	}
	for _, ca := range res.CAs {
		res.CAPEM += ca.PEM
	}
	return res, nil
}

// TAKAdmin talks to a TAK Server's web API as an admin.
type TAKAdmin struct {
	Address  string
	Port     int    // the web port with the login page, 8446 by default
	CAPEM    string // the TAK Server's CA (fetched or pasted)
	Username string
	Password string
	Dial     func(string) (net.Conn, error)

	client *http.Client
	token  string
}

func (t *TAKAdmin) base() string {
	return "https://" + net.JoinHostPort(t.Address, strconv.Itoa(t.Port))
}

// httpClient verifies the web port against the TAK CA (hostname not checked:
// TAK certificates often do not name the address), or against the system's
// CAs for ports with a public certificate such as Let's Encrypt.
func (t *TAKAdmin) httpClient() *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(t.CAPEM))
	cfg := &tls.Config{InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("no server certificate")
		}
		inter := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			inter.AddCert(c)
		}
		leaf := cs.PeerCertificates[0]
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter}); err == nil {
			return nil
		}
		_, err := leaf.Verify(x509.VerifyOptions{Intermediates: inter, DNSName: t.Address})
		if err != nil {
			return fmt.Errorf("the TAK Server's web certificate is not from its CA or a public CA: %w", err)
		}
		return nil
	}}
	tr := &http.Transport{TLSClientConfig: cfg}
	if t.Dial != nil {
		tr.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) { return t.Dial(addr) }
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// SignIn logs in like TAK Server's web login page.
func (t *TAKAdmin) SignIn() error {
	if t.CAPEM == "" {
		return errors.New("the TAK Server's CA certificate is needed first")
	}
	t.client = t.httpClient()
	form := url.Values{"grant_type": {"password"}, "username": {t.Username}, "password": {t.Password}}
	req, _ := http.NewRequest("POST", t.base()+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", t.base(), err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403:
		return errors.New("the TAK Server rejected the username or password")
	case resp.StatusCode == 404:
		return &statusError{code: 404, msg: fmt.Sprintf("port %d has no TAK Server login (/oauth/token); use the port with its web login page, 8446 by default", t.Port)}
	case resp.StatusCode != 200:
		return fmt.Errorf("the TAK Server returned HTTP %d while signing in", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.AccessToken == "" {
		return errors.New("the TAK Server did not return an access token")
	}
	t.token = body.AccessToken
	return nil
}

var errNotAdmin = errors.New("the TAK Server accepted the login but refused the request: the account needs the TAK Server admin role (ROLE_ADMIN)")

// ErrNoTAKAPI matches errors from a server that answers but has no TAK
// Server admin API (OpenTAKServer, for one).
var ErrNoTAKAPI = errors.New("no TAK Server admin API")

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string        { return e.msg }
func (e *statusError) Is(target error) bool { return target == ErrNoTAKAPI && e.code == 404 }

func (t *TAKAdmin) call(method, path string, body io.Reader, contentType string, out any) error {
	req, _ := http.NewRequest(method, t.base()+path, body)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the TAK Server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		return errNotAdmin
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &statusError{code: resp.StatusCode,
			msg: fmt.Sprintf("the TAK Server returned HTTP %d for %s: %s", resp.StatusCode, path, strings.TrimSpace(string(data[:min(len(data), 300)])))}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errNotAdmin // an HTML page: usually a login redirect
	}
	return nil
}

// GenerateToken creates a federation token, as Federation > Generate Token
// does (TAK Server 5.6 token federation). Expiration is in milliseconds
// since the epoch, -1 for none.
func (t *TAKAdmin) GenerateToken(name string, days int) (token string, expiration int64, err error) {
	expiration = -1
	if days > 0 {
		expiration = time.Now().Add(time.Duration(days) * 24 * time.Hour).UnixMilli()
	}
	body, _ := json.Marshal(map[string]any{"expiration": expiration, "attributes": map[string]string{"name": name}})
	var out struct {
		Data struct {
			Token      string `json:"token"`
			Expiration int64  `json:"expiration"`
		} `json:"data"`
	}
	if err := t.call("POST", "/Marti/api/generateAndSaveFederationJwtToken", bytes.NewReader(body), "application/json", &out); err != nil {
		return "", 0, err
	}
	if out.Data.Token == "" {
		return "", 0, errors.New("the TAK Server did not return a federation token")
	}
	if out.Data.Expiration != 0 {
		expiration = out.Data.Expiration
	}
	return out.Data.Token, expiration, nil
}

func normalizeFP(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune("0123456789ABCDEF", r) {
			return r
		}
		return -1
	}, strings.ToUpper(s))
}

// TrustResult reports what TrustEdgeTAK did.
type TrustResult struct {
	Server         string   `json:"server,omitempty"` // "OpenTAKServer" when its federation plugin was set up
	Uploaded       bool     `json:"uploaded"`
	Groups         []string `json:"groups"`
	FederateFound  bool     `json:"federate_found"`
	FederateGroups []string `json:"federate_groups,omitempty"`
	ListenerOff    bool     `json:"listener_off,omitempty"` // OpenTAKServer: its federation listener is not running
	ListenerPort   int      `json:"listener_port,omitempty"`
}

// TrustEdgeTAK makes the TAK Server trust EdgeTAK's CA for certificate
// federation (Federation > Manage Federate Certificate Authorities), and
// gives federates from it groups in both directions. TAK Server only copies
// a CA's groups into a federate when it first creates it, so an existing
// EdgeTAK federate gets them directly too.
func (t *TAKAdmin) TrustEdgeTAK(caDER, certDER []byte, groups []string) (*TrustResult, error) {
	fp := Fingerprint(caDER)
	caID := func() (string, error) {
		var out struct {
			Data []struct {
				FingerPrint string `json:"fingerPrint"`
			} `json:"data"`
		}
		if err := t.call("GET", "/Marti/api/federatecertificates", nil, "", &out); err != nil {
			return "", err
		}
		for _, c := range out.Data {
			if normalizeFP(c.FingerPrint) == fp {
				return c.FingerPrint, nil
			}
		}
		return "", nil
	}
	id, err := caID()
	if err != nil {
		return nil, err
	}
	res := &TrustResult{Groups: groups}
	if id == "" {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", "edgetak-ca.pem")
		fw.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
		mw.Close()
		if err := t.call("POST", "/Marti/api/federatecertificates", &body, mw.FormDataContentType(), nil); err != nil {
			return nil, err
		}
		res.Uploaded = true
		if id, err = caID(); err != nil {
			return nil, err
		}
		if id == "" {
			return nil, errors.New("the TAK Server accepted EdgeTAK's CA but does not list it as a federate CA")
		}
	}
	for _, g := range groups {
		body, _ := json.Marshal(map[string]string{"caId": id, "group": g, "direction": "BOTH"})
		if err := t.call("POST", "/Marti/api/federatecagroups", bytes.NewReader(body), "application/json", nil); err != nil {
			return nil, err
		}
	}
	// The EdgeTAK federate, if it has connected before.
	sum := sha256.Sum256(certDER)
	certFP := strings.ToUpper(hex.EncodeToString(sum[:]))
	var feds struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := t.call("GET", "/Marti/api/federates", nil, "", &feds); err != nil {
		// Some TAK Servers fail this call (HTTP 500); connected federates are
		// also listed under active connections.
		var active struct {
			Data []struct {
				FederateID string `json:"federateId"`
				Name       string `json:"federateName"`
			} `json:"data"`
		}
		if t.call("GET", "/Marti/api/activeconnections", nil, "", &active) != nil {
			return res, nil // trust is in place; the federate gets the CA's groups when it is created
		}
		feds.Data = feds.Data[:0]
		for _, a := range active.Data {
			feds.Data = append(feds.Data, struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}{a.FederateID, a.Name})
		}
	}
	for _, f := range feds.Data {
		if normalizeFP(f.ID) != certFP {
			continue
		}
		res.FederateFound = true
		for _, g := range groups {
			body, _ := json.Marshal(map[string]string{"federateId": f.ID, "group": g, "direction": "BOTH"})
			t.call("POST", "/Marti/api/federategroups", bytes.NewReader(body), "application/json", nil)
		}
		var assoc struct {
			Data []struct {
				Group     string `json:"group"`
				Direction string `json:"direction"`
			} `json:"data"`
		}
		if t.call("GET", "/Marti/api/federategroups/"+url.PathEscape(f.ID), nil, "", &assoc) == nil {
			for _, a := range assoc.Data {
				if !slices.Contains(res.FederateGroups, a.Group) {
					res.FederateGroups = append(res.FederateGroups, a.Group)
				}
			}
			slices.Sort(res.FederateGroups)
		}
	}
	return res, nil
}
