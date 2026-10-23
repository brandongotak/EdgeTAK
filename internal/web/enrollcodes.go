package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
// enrolls), never for signing in anywhere else, and only until it
// expires; a one-device code stops working shortly after its first use.
// Only a hash of each code is kept.

// enrollCodeGrace is how long a one-device code keeps working after its
// first use: one enrollment makes several requests.
const enrollCodeGrace = 10 * time.Minute

func enrollPath(p string) bool { return strings.HasPrefix(p, "/Marti/api/tls/") }

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
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
		if !now.Before(c.Expires) || (c.OneDevice && !c.Used.IsZero() && now.After(c.Used.Add(enrollCodeGrace))) {
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

func (s *Server) enrollCodeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/enroll-codes", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.enrollCodeViews())
	}))
	// Make a code for a user and the ATAK quick-connect link holding it. The
	// code is only in this answer.
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
		if host == "" || strings.ContainsAny(host, "/?#@ ") {
			jsonError(w, http.StatusBadRequest, "set the server address devices use")
			return
		}
		b := make([]byte, 18)
		rand.Read(b)
		code := base64.RawURLEncoding.EncodeToString(b)
		now := time.Now().UTC()
		c := store.EnrollCode{ID: uuid(), Username: u.Username, Hash: hashCode(code), Expires: now.Add(time.Duration(req.Hours) * time.Hour),
			OneDevice: req.OneDevice, Created: now, CreatedBy: id.Username}
		if err := s.Store.AddEnrollCode(c, now); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("enrollment code made", "admin", id.Username, "user", u.Username, "hours", req.Hours, "one_device", req.OneDevice)
		link := "tak://com.atakmap.app/enroll?host=" + url.QueryEscape(host) + "&username=" + url.QueryEscape(u.Username) + "&token=" + url.QueryEscape(code)
		writeJSON(w, http.StatusCreated, map[string]any{"id": c.ID, "username": u.Username, "expires": c.Expires, "one_device": c.OneDevice,
			"link": link, "codes": s.enrollCodeViews()})
	}))
	mux.HandleFunc("DELETE /admin/api/enroll-codes/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if err := s.Store.DeleteEnrollCode(r.PathValue("id")); err != nil {
			jsonError(w, http.StatusNotFound, "no such code")
			return
		}
		writeJSON(w, http.StatusOK, s.enrollCodeViews())
	}))
}
