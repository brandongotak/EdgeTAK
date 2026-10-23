package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/audit"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Enrollment codes (Server -> Connect -> Quick-connect QR): an admin picks a
// user and gets an ATAK quick-connect QR code without knowing their
// password. The code in the QR stands in for the password, but only for
// enrolling a device (the /Marti/api/tls/ requests ATAK makes when it
// enrolls) or downloading a connection package for the user (/qr/), never
// for signing in anywhere else, and only until it expires; a one-device
// code stops working shortly after its first use. Only a hash of each code
// is kept.
//
// The same code goes in one of two QR codes:
//   - with a public certificate (Let's Encrypt) on the enrollment port, the
//     quick-connect link (tak://com.atakmap.app/enroll), which ATAK only
//     follows to a server it already trusts;
//   - otherwise, an import link (tak://com.atakmap.app/import) to a
//     connection package carrying the EdgeTAK CA and a client certificate,
//     so ATAK connects without enrolling. ATAK can't trust EdgeTAK's
//     certificate before it has the package, so the package comes over
//     plain HTTP.

// enrollCodeGrace is how long a one-device code keeps working after its
// first use: one enrollment makes several requests.
const enrollCodeGrace = 10 * time.Minute

func enrollPath(p string) bool { return strings.HasPrefix(p, "/Marti/api/tls/") }

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// enrollCodeLive reports whether c can still be used.
func enrollCodeLive(c store.EnrollCode, now time.Time) bool {
	return now.Before(c.Expires) && !(c.OneDevice && !c.Used.IsZero() && now.After(c.Used.Add(enrollCodeGrace)))
}

// checkEnrollCode reports whether code is a usable enrollment code for
// name, marking it used.
func (s *Server) checkEnrollCode(r *http.Request, name, code string) (*store.User, bool) {
	if code == "" || !enrollPath(r.URL.Path) {
		return nil, false
	}
	u, ok := s.Store.User(name)
	if !ok || u.Disabled {
		return nil, false
	}
	now := time.Now()
	want := hashCode(code)
	for _, c := range s.Store.EnrollCodes() {
		if c.Username != u.Username || subtle.ConstantTimeCompare([]byte(c.Hash), []byte(want)) != 1 {
			continue
		}
		if !enrollCodeLive(c, now) {
			return nil, false
		}
		if c.Used.IsZero() {
			s.Store.UseEnrollCode(c.ID, now.UTC())
			s.Log.Info("enrollment code used", "user", u.Username, "remote", r.RemoteAddr)
			if s.Audit != nil {
				s.Audit.Add(audit.Entry{Actor: u.Username, IP: clientIP(r), Action: "Enrolled with a QR code", Target: u.Username,
					Details: "code made by " + c.CreatedBy})
			}
		}
		return u, true
	}
	return nil, false
}

type enrollCodeView struct {
	ID        string       `json:"id"`
	Username  string       `json:"username"`
	Expires   time.Time    `json:"expires"`
	OneDevice bool         `json:"one_device"`
	Used      time.Time    `json:"used,omitzero"`
	Created   time.Time    `json:"created"`
	CreatedBy string       `json:"created_by"`
	State     string       `json:"state"` // active, used or expired
	Replace   bool         `json:"replace,omitempty"`
	App       string       `json:"app,omitempty"`
	Devices   []codeDevice `json:"devices"` // what it set up
}

// codeDevice is a certificate a code issued, and the device using it.
type codeDevice struct {
	Serial    string    `json:"serial"`
	Revoked   bool      `json:"revoked,omitempty"`
	Connected time.Time `json:"connected,omitzero"` // first connection; zero if it never connected
	Online    bool      `json:"online"`
	Callsign  string    `json:"callsign,omitempty"`
}

func (s *Server) enrollCodeViews() []enrollCodeView {
	now := time.Now()
	certs := map[string]store.Cert{}
	for _, c := range s.Store.Certs() {
		certs[c.Serial] = c
	}
	online := map[string]string{} // serial -> callsign
	if s.Router != nil {
		for _, c := range s.Router.Clients() {
			if c.CertSerial != "" {
				_, online[c.CertSerial] = c.Identity()
			}
		}
	}
	out := []enrollCodeView{}
	for _, c := range s.Store.EnrollCodes() {
		v := enrollCodeView{ID: c.ID, Username: c.Username, Expires: c.Expires, OneDevice: c.OneDevice, Used: c.Used,
			Created: c.Created, CreatedBy: c.CreatedBy, State: "active", Replace: c.Replace, App: c.App, Devices: []codeDevice{}}
		for _, serial := range c.Certs {
			callsign, on := online[serial]
			v.Devices = append(v.Devices, codeDevice{Serial: serial, Revoked: certs[serial].Revoked, Connected: certs[serial].Connected,
				Online: on, Callsign: callsign})
		}
		switch {
		case !now.Before(c.Expires):
			v.State = "expired"
		case c.OneDevice && !c.Used.IsZero() && now.After(c.Used.Add(enrollCodeGrace)):
			v.State = "used"
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b enrollCodeView) int { return b.Created.Compare(a.Created) })
	return out
}

// codeIssued records that code c issued the certificate serial and, for a
// replacement code, revokes the user's other devices' certificates and
// disconnects them.
func (s *Server) codeIssued(r *http.Request, c store.EnrollCode, serial string) {
	s.Store.AddEnrollCodeCert(c.ID, serial)
	if !c.Replace {
		return
	}
	revoked, err := s.Store.RevokeUserCerts(c.Username, append(slices.Clone(c.Certs), serial))
	if err != nil {
		s.Log.Error("replacing devices", "user", c.Username, "err", err)
		return
	}
	s.cutOffCerts(revoked)
	s.Log.Warn("replaced a user's devices", "user", c.Username, "revoked", len(revoked), "new_serial", serial)
	if s.Audit != nil {
		s.Audit.Add(audit.Entry{Actor: c.Username, IP: clientIP(r), Action: "Replaced devices with a QR code", Target: c.Username,
			Details: fmt.Sprintf("%d certificate(s) revoked, code made by %s", len(revoked), c.CreatedBy)})
	}
}

// cutOffCerts disconnects the devices using these certificates.
func (s *Server) cutOffCerts(serials []string) {
	if len(serials) == 0 || s.Router == nil {
		return
	}
	for _, c := range s.Router.Clients() {
		if c.CertSerial != "" && slices.Contains(serials, c.CertSerial) {
			c.Close()
		}
	}
}

// enrollCodeCertIssued links a certificate issued by enrollment to the
// code the device enrolled with, if it used one.
func (s *Server) enrollCodeCertIssued(r *http.Request, username, serial string) {
	name, pw, ok := r.BasicAuth()
	if !ok || pw == "" || name != username {
		return
	}
	want := hashCode(pw)
	for _, c := range s.Store.EnrollCodes() {
		if c.Username == username && subtle.ConstantTimeCompare([]byte(c.Hash), []byte(want)) == 1 {
			s.codeIssued(r, c, serial)
			return
		}
	}
}

// sharedEventAccount reports whether username is a shared event account,
// whose devices all use one account (and so can't be replaced one by one).
func (s *Server) sharedEventAccount(username string) bool {
	for _, e := range s.Store.Events() {
		if e.Mode == "shared" && e.Account == username {
			return true
		}
	}
	return false
}

// qrPackageLink is the ATAK import link to the connection package for code,
// or "" when the plain HTTP port is off.
func (s *Server) qrPackageLink(host, username, code string) string {
	if s.Addrs["http"] == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(s.Addrs["http"])
	if err != nil {
		return ""
	}
	// ATAK names the download after the end of the path.
	u := "http://" + net.JoinHostPort(host, port) + "/qr/" + url.PathEscape(code) + "/" + sanitize(s.Cfg.ServerName) + "-" + sanitize(username) + ".zip"
	return "tak://com.atakmap.app/import?url=" + url.QueryEscape(u)
}

// enrollLink is the ATAK quick-connect link for a code (or password).
func enrollLink(host, username, code string) string {
	return "tak://com.atakmap.app/enroll?host=" + url.QueryEscape(host) + "&username=" + url.QueryEscape(username) + "&token=" + url.QueryEscape(code)
}

// qrLink is the link to put in a user's QR code: quick connect when the
// enrollment port has a public certificate, else the package import link
// (or quick connect after all when plain HTTP is off, which only works on
// a phone that already trusts this server).
func (s *Server) qrLink(host, username, code string) string {
	if host != "" && !s.Public.Uses("enroll") {
		if l := s.qrPackageLink(host, username, code); l != "" {
			return l
		}
	}
	return enrollLink(host, username, code)
}

// deviceHost is the server address to give devices in the connection
// packages, QR codes and links made for request r. Without a public
// certificate (a local server), it is the address r came in on, as on
// OpenTAKServer: the one in the admin's or user's address bar, so a server
// whose IP address changes needs no setting. That isn't used when it is
// this machine itself (localhost), and with Let's Encrypt the certificate
// only covers public_host, so then it's always public_host.
func (s *Server) deviceHost(r *http.Request) string {
	host := ""
	if r != nil {
		host = r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
	}
	usable := host != "" && !strings.ContainsAny(host, "/?#@ ") && host != "localhost" && !isLoopbackIP(host)
	if usable && !s.Public.Uses("enroll") {
		return host
	}
	if s.Cfg.PublicHost != "" || !usable {
		return s.Cfg.PublicHost
	}
	return host
}

// newEnrollCode makes and saves a code for username that works for hours.
// The code is returned only here.
func (s *Server) newEnrollCode(username string, hours int, oneDevice, replace bool, app, host, by string) (string, store.EnrollCode, error) {
	if host == "" || strings.ContainsAny(host, "/?#@ ") {
		return "", store.EnrollCode{}, errNoHost
	}
	b := make([]byte, 18)
	rand.Read(b)
	code := base64.RawURLEncoding.EncodeToString(b)
	now := time.Now().UTC()
	c := store.EnrollCode{ID: uuid(), Username: username, Hash: hashCode(code), Expires: now.Add(time.Duration(hours) * time.Hour),
		OneDevice: oneDevice, Created: now, CreatedBy: by, Host: host, Replace: replace, App: app}
	if err := s.Store.AddEnrollCode(c, now); err != nil {
		return "", store.EnrollCode{}, err
	}
	s.Log.Info("enrollment code made", "by", by, "user", username, "hours", hours, "one_device", oneDevice, "replace", replace)
	return code, c, nil
}

var errNoHost = errors.New("set the server address devices use")

// itakRegistration is iTAK's registration QR code: JSON with the server's
// connection string and a login, here the one-time code as the password.
// iTAK then enrolls on its own, so it needs the enrollment port to have a
// certificate it trusts (Let's Encrypt).
func (s *Server) itakRegistration(host, username, code, id string) string {
	type server struct {
		ConnectionString string `json:"connectionString"`
	}
	type user struct {
		Username       string `json:"username"`
		Password       string `json:"password"`
		RegistrationID string `json:"registrationId"`
	}
	b, _ := json.Marshal(struct {
		Passphrase string `json:"passphrase"`
		Type       string `json:"type"`
		Server     server `json:"serverCredentials"`
		User       user   `json:"userCredentials"`
	}{"false", "registration", server{host + ":" + firstNonEmpty(port(s.Addrs["tls"]), "8089") + ":ssl"}, user{username, code, id}})
	return string(b)
}

// itakPackageLink is a plain link to the iTAK connection package for code
// (opened with the phone's camera, then imported into iTAK), or "" when the
// plain HTTP port is off.
func (s *Server) itakPackageLink(host, username, code string) string {
	if s.Addrs["http"] == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(host, port(s.Addrs["http"])) + "/qr/" + url.PathEscape(code) + "/" + sanitize(s.Cfg.ServerName) + "-" + sanitize(username) + "-itak.zip"
}

// qrPackage serves the connection package for a code: anyone with the code
// gets it, as anyone with it could enroll. An active shared event's
// password works too, for the event's shared account.
func (s *Server) qrPackage(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.passwordBlocked(ip) {
		http.Error(w, "Too many failed attempts; try again later.", http.StatusTooManyRequests)
		return
	}
	now := time.Now()
	given := r.PathValue("code")
	want := hashCode(given)
	var username, host, details string
	var code *store.EnrollCode
	found, live := false, false
	for _, c := range s.Store.EnrollCodes() {
		if subtle.ConstantTimeCompare([]byte(c.Hash), []byte(want)) == 1 {
			found, live = true, enrollCodeLive(c, now)
			username, host, details, code = c.Username, c.Host, "code made by "+c.CreatedBy, &c
		}
	}
	if !found {
		for _, e := range s.Store.Events() {
			if e.Mode == "shared" && e.Password != "" && subtle.ConstantTimeCompare([]byte(e.Password), []byte(given)) == 1 {
				found, live = true, e.Ended.IsZero() && now.Before(e.Expires)
				username, details = e.Account, "event "+e.Name
			}
		}
	}
	u, ok := s.Store.User(username)
	if !found || !live || !ok || u.Disabled {
		if !found {
			s.signInFailedFrom(ip, "")
		}
		http.Error(w, "This QR code has expired or was revoked. Ask for a new one.", http.StatusNotFound)
		return
	}
	var data []byte
	var serial string
	var err error
	if code != nil && code.App == "itak" {
		data, serial, err = s.buildITAKPackage(firstNonEmpty(host, s.Cfg.PublicHost), u.Username, "QR code")
	} else {
		data, serial, err = s.buildCertPackage(firstNonEmpty(host, s.Cfg.PublicHost), u.Username, "qr-package")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if code != nil {
		s.Store.UseEnrollCode(code.ID, now.UTC())
		s.codeIssued(r, *code, serial)
	}
	s.Log.Info("QR connection package downloaded", "user", u.Username, "serial", serial, "remote", r.RemoteAddr)
	if s.Audit != nil {
		s.Audit.Add(audit.Entry{Actor: u.Username, IP: ip, Action: "Downloaded a QR connection package", Target: u.Username,
			Details: "certificate " + serial + ", " + details})
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitize(r.PathValue("file"))+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func (s *Server) enrollCodeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /qr/{code}/{file}", s.qrPackage)
	mux.HandleFunc("GET /admin/api/enroll-codes", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.enrollCodeViews())
	}))
	// Make a code for a user and the ATAK links holding it. The code is only
	// in this answer.
	mux.HandleFunc("POST /admin/api/enroll-codes", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			Username  string `json:"username"`
			Hours     int    `json:"hours"`
			OneDevice bool   `json:"one_device"`
			Replace   bool   `json:"replace"`
			App       string `json:"app"` // "itak", or "" for ATAK
			Host      string `json:"host"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		u, ok := s.Store.User(req.Username)
		if !ok {
			jsonError(w, http.StatusNotFound, "no such user")
			return
		}
		if u.Disabled {
			jsonError(w, http.StatusBadRequest, u.Username+" is disabled")
			return
		}
		if req.Hours <= 0 {
			req.Hours = 24
		}
		req.Hours = min(req.Hours, 30*24)
		if req.Replace && s.sharedEventAccount(u.Username) {
			jsonError(w, http.StatusBadRequest, u.Username+" is an event's shared account: every device uses it, so they can't be replaced one by one")
			return
		}
		if req.App != "" && req.App != "itak" {
			jsonError(w, http.StatusBadRequest, "app must be itak or empty (ATAK)")
			return
		}
		// A replacement code sets up one phone.
		req.OneDevice = req.OneDevice || req.Replace
		host := firstNonEmpty(strings.TrimSpace(req.Host), s.deviceHost(r))
		local := !s.Public.Uses("enroll")
		if req.App == "itak" && local && s.Addrs["http"] == "" {
			jsonError(w, http.StatusBadRequest, "an iTAK QR code on a server without Let's Encrypt downloads a package over plain HTTP (port 8080), which is turned off here; use the iTAK package instead")
			return
		}
		code, c, err := s.newEnrollCode(u.Username, req.Hours, req.OneDevice, req.Replace, req.App, host, id.Username)
		if errors.Is(err, errNoHost) {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		} else if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Without a public certificate on the enrollment port ATAK can't
		// trust it yet, so the QR code imports a package instead.
		out := map[string]any{"id": c.ID, "username": u.Username, "expires": c.Expires, "one_device": c.OneDevice, "replace": c.Replace,
			"local": local, "codes": s.enrollCodeViews()}
		if req.App == "itak" {
			// iTAK: with Let's Encrypt, its registration QR code (it enrolls
			// with the code); otherwise a link to its package.
			out["app"] = "itak"
			if local {
				out["qr_link"] = s.itakPackageLink(host, u.Username, code)
			} else {
				out["qr_link"] = s.itakRegistration(host, u.Username, code, c.ID)
			}
		} else {
			out["link"], out["package_link"], out["qr_link"] = enrollLink(host, u.Username, code), s.qrPackageLink(host, u.Username, code), s.qrLink(host, u.Username, code)
		}
		writeJSON(w, http.StatusCreated, out)
	}))
	// A code each for several users, for a printed sheet of QR codes.
	mux.HandleFunc("POST /admin/api/enroll-codes/batch", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			Usernames []string `json:"usernames"`
			Hours     int      `json:"hours"`
			OneDevice bool     `json:"one_device"`
			Host      string   `json:"host"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if len(req.Usernames) == 0 || len(req.Usernames) > 200 {
			jsonError(w, http.StatusBadRequest, "choose 1 to 200 users")
			return
		}
		if req.Hours <= 0 {
			req.Hours = 24
		}
		req.Hours = min(req.Hours, 30*24)
		host := firstNonEmpty(strings.TrimSpace(req.Host), s.deviceHost(r))
		type card struct {
			Username string    `json:"username"`
			Expires  time.Time `json:"expires"`
			Link     string    `json:"qr_link"`
		}
		cards, skipped := []card{}, []string{}
		for _, name := range slices.Compact(slices.Sorted(slices.Values(req.Usernames))) {
			u, ok := s.Store.User(name)
			if !ok || u.Disabled {
				skipped = append(skipped, name)
				continue
			}
			code, c, err := s.newEnrollCode(u.Username, req.Hours, req.OneDevice, false, "", host, id.Username)
			if errors.Is(err, errNoHost) {
				jsonError(w, http.StatusBadRequest, err.Error())
				return
			} else if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			cards = append(cards, card{Username: u.Username, Expires: c.Expires, Link: s.qrLink(host, u.Username, code)})
		}
		writeJSON(w, http.StatusCreated, map[string]any{"cards": cards, "skipped": skipped, "codes": s.enrollCodeViews()})
	}))
	// ?certs=1 also revokes the certificates the code issued, disconnecting
	// the devices it set up.
	mux.HandleFunc("DELETE /admin/api/enroll-codes/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if boolParam(r, "certs") {
			for _, c := range s.Store.EnrollCodes() {
				if c.ID == r.PathValue("id") {
					for _, serial := range c.Certs {
						s.Store.RevokeCert(serial)
					}
					s.cutOffCerts(c.Certs)
					s.Log.Warn("revoked a QR code's certificates", "admin", id.Username, "user", c.Username, "certs", len(c.Certs))
				}
			}
		}
		if err := s.Store.DeleteEnrollCode(r.PathValue("id")); err != nil {
			jsonError(w, http.StatusNotFound, "no such code")
			return
		}
		writeJSON(w, http.StatusOK, s.enrollCodeViews())
	}))
}
