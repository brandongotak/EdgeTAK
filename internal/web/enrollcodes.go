package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Expires   time.Time `json:"expires"`
	OneDevice bool      `json:"one_device"`
	Used      time.Time `json:"used,omitzero"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by"`
	State     string    `json:"state"` // active, used or expired
}

func (s *Server) enrollCodeViews() []enrollCodeView {
	now := time.Now()
	out := []enrollCodeView{}
	for _, c := range s.Store.EnrollCodes() {
		v := enrollCodeView{ID: c.ID, Username: c.Username, Expires: c.Expires, OneDevice: c.OneDevice, Used: c.Used,
			Created: c.Created, CreatedBy: c.CreatedBy, State: "active"}
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

// qrHost is the server address to put in a QR code made from r.
func (s *Server) qrHost(r *http.Request) string {
	if s.Cfg.PublicHost != "" {
		return s.Cfg.PublicHost
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	return host
}

// newEnrollCode makes and saves a code for username that works for hours.
// The code is returned only here.
func (s *Server) newEnrollCode(username string, hours int, oneDevice bool, host, by string) (string, store.EnrollCode, error) {
	if host == "" || strings.ContainsAny(host, "/?#@ ") {
		return "", store.EnrollCode{}, errNoHost
	}
	b := make([]byte, 18)
	rand.Read(b)
	code := base64.RawURLEncoding.EncodeToString(b)
	now := time.Now().UTC()
	c := store.EnrollCode{ID: uuid(), Username: username, Hash: hashCode(code), Expires: now.Add(time.Duration(hours) * time.Hour),
		OneDevice: oneDevice, Created: now, CreatedBy: by, Host: host}
	if err := s.Store.AddEnrollCode(c, now); err != nil {
		return "", store.EnrollCode{}, err
	}
	s.Log.Info("enrollment code made", "by", by, "user", username, "hours", hours, "one_device", oneDevice)
	return code, c, nil
}

var errNoHost = errors.New("set the server address devices use")

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
	var username, host, details, codeID string
	found, live := false, false
	for _, c := range s.Store.EnrollCodes() {
		if subtle.ConstantTimeCompare([]byte(c.Hash), []byte(want)) == 1 {
			found, live = true, enrollCodeLive(c, now)
			username, host, details, codeID = c.Username, c.Host, "code made by "+c.CreatedBy, c.ID
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
	data, serial, err := s.buildCertPackage(firstNonEmpty(host, s.Cfg.PublicHost), u.Username)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if codeID != "" {
		s.Store.UseEnrollCode(codeID, now.UTC())
	}
	s.Log.Info("QR connection package downloaded", "user", u.Username, "serial", serial, "remote", r.RemoteAddr)
	if s.Audit != nil {
		s.Audit.Add(audit.Entry{Actor: u.Username, IP: ip, Action: "Downloaded a QR connection package", Target: u.Username,
			Details: "certificate " + serial + ", " + details})
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitize(s.Cfg.ServerName)+"-"+sanitize(u.Username)+`.zip"`)
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
		host := firstNonEmpty(strings.TrimSpace(req.Host), s.Cfg.PublicHost)
		code, c, err := s.newEnrollCode(u.Username, req.Hours, req.OneDevice, host, id.Username)
		if errors.Is(err, errNoHost) {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		} else if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Without a public certificate on the enrollment port ATAK can't
		// trust it yet, so the QR code imports a package instead.
		writeJSON(w, http.StatusCreated, map[string]any{"id": c.ID, "username": u.Username, "expires": c.Expires, "one_device": c.OneDevice,
			"link": enrollLink(host, u.Username, code), "package_link": s.qrPackageLink(host, u.Username, code), "local": !s.Public.Uses("enroll"),
			"qr_link": s.qrLink(host, u.Username, code), "codes": s.enrollCodeViews()})
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
		host := firstNonEmpty(strings.TrimSpace(req.Host), s.Cfg.PublicHost)
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
			code, c, err := s.newEnrollCode(u.Username, req.Hours, req.OneDevice, host, id.Username)
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
	mux.HandleFunc("DELETE /admin/api/enroll-codes/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if err := s.Store.DeleteEnrollCode(r.PathValue("id")); err != nil {
			jsonError(w, http.StatusNotFound, "no such code")
			return
		}
		writeJSON(w, http.StatusOK, s.enrollCodeViews())
	}))
}
