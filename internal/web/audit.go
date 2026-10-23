package web

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/audit"
)

// The audit log (Server -> Audit log): every change an administrator makes
// through the console or admin API, recorded by the admin wrapper (see
// auditRequest), plus sign-ins and certificate enrollments. Entries say who,
// from where, what, to what and whether it worked; secrets in a request
// (passwords, tokens, webhook URLs) are never recorded.

// auditActions describes each admin route that changes something.
var auditActions = map[string]string{
	"DELETE /admin/api/apps/{id}":                        "Deleted plugin",
	"DELETE /admin/api/backups/{name}":                   "Deleted backup",
	"DELETE /admin/api/chat":                             "Cleared the chat log",
	"DELETE /admin/api/drawings/{id}":                    "Deleted drawing",
	"DELETE /admin/api/enroll-codes/{id}":                "Revoked a quick-connect QR code",
	"DELETE /admin/api/events/{id}":                      "Deleted event record",
	"DELETE /admin/api/federation/federates/{id}":        "Removed federation link",
	"DELETE /admin/api/federation/pending/{fp}":          "Rejected federation request",
	"DELETE /admin/api/geofences/{id}":                   "Deleted geofence",
	"DELETE /admin/api/groups/{name}":                    "Deleted group",
	"DELETE /admin/api/groups/{name}/members/{user}":     "Removed group member",
	"DELETE /admin/api/history":                          "Deleted all position history",
	"DELETE /admin/api/login-protection/blocked/{ip}":    "Unblocked an address",
	"DELETE /admin/api/maps/{file}":                      "Deleted offline map",
	"DELETE /admin/api/missions/{name}":                  "Deleted mission",
	"DELETE /admin/api/overlays/{id}":                    "Deleted overlay",
	"DELETE /admin/api/packages/{hash}":                  "Deleted data package",
	"DELETE /admin/api/profiles/{id}":                    "Deleted device profile",
	"DELETE /admin/api/profiles/{id}/files/{name}":       "Removed device profile file",
	"DELETE /admin/api/registration/invites/{code}":      "Revoked invite",
	"DELETE /admin/api/registration/requests/{id}":       "Rejected sign-up request",
	"DELETE /admin/api/schedules/{id}":                   "Deleted scheduled send",
	"PATCH /admin/api/schedules/{id}":                    "Paused or resumed scheduled send",
	"POST /admin/api/schedules":                          "Created scheduled send",
	"POST /admin/api/schedules/{id}/run":                 "Sent a scheduled send now",
	"PUT /admin/api/schedules/{id}":                      "Changed scheduled send",
	"DELETE /admin/api/takgov":                           "Unlinked tak.gov",
	"DELETE /admin/api/users/{name}":                     "Deleted user",
	"DELETE /admin/api/users/{name}/2fa":                 "Turned off two-factor login",
	"DELETE /admin/api/video/feeds/{uid}":                "Deleted video feed",
	"DELETE /admin/api/video/recordings":                 "Deleted video recordings",
	"DELETE /admin/api/webhooks/{id}":                    "Deleted webhook",
	"PATCH /admin/api/events/{id}":                       "Changed event",
	"PATCH /admin/api/groups/{name}":                     "Changed group",
	"PATCH /admin/api/overlays/{id}":                     "Changed overlay",
	"PATCH /admin/api/users/{name}":                      "Changed user",
	"POST /admin/api/apps":                               "Uploaded plugins",
	"POST /admin/api/backup/restore":                     "Restored a backup (uploaded)",
	"POST /admin/api/backups":                            "Made a backup",
	"POST /admin/api/backups/{name}/restore":             "Restored a backup",
	"POST /admin/api/certs/{serial}/revoke":              "Revoked certificate",
	"POST /admin/api/clients/{id}/disconnect":            "Disconnected a device",
	"POST /admin/api/drawings":                           "Added drawing",
	"POST /admin/api/emergencies/{id}/ack":               "Acknowledged alert",
	"POST /admin/api/emergencies/{id}/dismiss":           "Dismissed alert",
	"POST /admin/api/enroll-codes":                       "Made a quick-connect QR code",
	"PUT /admin/api/plain-tcp":                           "Turned plain TCP on or off",
	"POST /admin/api/enroll-codes/batch":                 "Made a QR code sheet",
	"POST /admin/api/events":                             "Created event",
	"POST /admin/api/events/{id}/end":                    "Ended event",
	"POST /admin/api/federation/federates":               "Added federation link",
	"POST /admin/api/federation/fetch-ca":                "Fetched a federation CA",
	"POST /admin/api/federation/pending/{fp}/approve":    "Approved federation request",
	"POST /admin/api/federation/tak/token":               "Made a federation token",
	"POST /admin/api/federation/tak/trust":               "Trusted a TAK Server",
	"POST /admin/api/geofences":                          "Added geofence",
	"POST /admin/api/groups":                             "Created group",
	"POST /admin/api/history/overlay":                    "Saved trails as an overlay",
	"POST /admin/api/ldap/sync":                          "Synced directory users",
	"POST /admin/api/map/sources/import":                 "Imported a map source",
	"POST /admin/api/maps":                               "Uploaded an offline map",
	"POST /admin/api/maps/rescan":                        "Rescanned offline maps",
	"POST /admin/api/overlays":                           "Imported overlay",
	"POST /admin/api/overlays/{id}/send":                 "Sent overlay again",
	"POST /admin/api/profiles":                           "Created device profile",
	"POST /admin/api/profiles/{id}/files":                "Added device profile file",
	"POST /admin/api/profiles/{id}/send":                 "Sent device profile",
	"POST /admin/api/registration/invites":               "Created invite",
	"POST /admin/api/registration/requests/{id}/approve": "Approved sign-up request",
	"POST /admin/api/retention/run":                      "Applied data retention now",
	"POST /admin/api/send/message":                       "Sent a message to devices",
	"POST /admin/api/send/package":                       "Sent a data package to devices",
	"POST /admin/api/send/reply":                         "Replied as the server contact",
	"POST /admin/api/server/update":                      "Updated EdgeTAK",
	"PUT /admin/api/server/update/settings":              "Changed whether pre-releases are offered",
	"POST /admin/api/sim/start":                          "Started the simulator",
	"POST /admin/api/sim/stop":                           "Stopped the simulator",
	"POST /admin/api/takgov/install":                     "Added a plugin from tak.gov",
	"POST /admin/api/takgov/link":                        "Started linking tak.gov",
	"POST /admin/api/users":                              "Created user",
	"POST /admin/api/users/{name}/certificate":           "Issued a certificate file",
	"POST /admin/api/video/feeds":                        "Added video feed",
	"POST /admin/api/video/mediamtx/restart":             "Restarted MediaMTX",
	"POST /admin/api/video/mediamtx/rollback":            "Rolled back MediaMTX",
	"POST /admin/api/video/mediamtx/update":              "Updated MediaMTX",
	"POST /admin/api/webhooks":                           "Added webhook",
	"PUT /admin/api/apps/{id}/icon":                      "Changed plugin icon",
	"PUT /admin/api/apps/{id}/kind":                      "Changed whether an APK is an app or a plugin",
	"PUT /admin/api/apps/settings":                       "Changed update server settings",
	"PUT /admin/api/backup/settings":                     "Changed backup settings",
	"PUT /admin/api/certs/settings":                      "Changed certificate settings",
	"PUT /admin/api/chat/settings":                       "Changed chat log settings",
	"PUT /admin/api/drawings/{id}":                       "Changed drawing",
	"PUT /admin/api/federation/federates/{id}":           "Changed federation link",
	"PUT /admin/api/federation/listener":                 "Changed federation listener",
	"PUT /admin/api/geofences/{id}":                      "Changed geofence",
	"PUT /admin/api/groups/{name}/members/{user}":        "Added group member",
	"PUT /admin/api/history/settings":                    "Changed position history settings",
	"PUT /admin/api/ldap":                                "Changed directory sign-in",
	"PUT /admin/api/login-protection":                    "Changed login protection",
	"PUT /admin/api/lost-contact":                        "Changed lost contact alerts",
	"PUT /admin/api/map/sources":                         "Changed map sources",
	"PUT /admin/api/maps/{file}":                         "Changed offline map",
	"PUT /admin/api/overlays/{id}/file":                  "Replaced overlay file",
	"PUT /admin/api/profiles/{id}":                       "Changed device profile",
	"PUT /admin/api/registration":                        "Changed sign-up settings",
	"PUT /admin/api/retention":                           "Changed data retention",
	"PUT /admin/api/send/contact":                        "Changed the server contact",
	"PUT /admin/api/video/feeds/{uid}":                   "Changed video feed",
	"PUT /admin/api/video/recording":                     "Changed video recording",
	"PUT /admin/api/video/settings":                      "Changed video settings",
	"PUT /admin/api/webhooks/{id}":                       "Changed webhook",
}

// auditQuiet are admin requests that change nothing worth recording.
var auditQuiet = map[string]bool{
	"POST /admin/api/retention/preview":    true,
	"POST /admin/api/takgov/poll":          true,
	"POST /admin/api/server/update/check":  true,
	"POST /admin/api/video/mediamtx/check": true,
	"POST /admin/api/ldap/test":            true,
	"POST /admin/api/webhooks/{id}/test":   true,
	"DELETE /admin/api/audit":              true, // records its own entry, with the count
}

// auditSecret are request fields never recorded.
func auditSecret(key string) bool {
	k := strings.ToLower(key)
	for _, s := range []string{"password", "secret", "token", "key", "url", "code", "totp", "pin", "csr", "p12"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// auditDetails summarises a JSON request body: its fields and simple
// values, secrets left out.
func auditDetails(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	var parts []string
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}) {
		if auditSecret(k) {
			continue
		}
		var v string
		switch x := m[k].(type) {
		case nil:
			continue
		case string:
			v = truncate(x, 60)
		case bool, float64:
			v = fmt.Sprint(x)
		case []any:
			var items []string
			for _, it := range x {
				switch s := it.(type) {
				case string:
					items = append(items, s)
				case float64, bool:
					items = append(items, fmt.Sprint(s))
				}
			}
			if len(items) != len(x) {
				v = fmt.Sprintf("%d items", len(x))
			} else {
				v = "[" + truncate(strings.Join(items, ", "), 80) + "]"
			}
		default:
			continue
		}
		parts = append(parts, k+": "+v)
	}
	return truncate(strings.Join(parts, "; "), 400)
}

// auditTarget names what a request acts on: the record its path names
// (looked up before the handler can delete it), else the name in its body.
func (s *Server) auditTarget(r *http.Request, body []byte) string {
	if id := r.PathValue("id"); id != "" {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/overlays/"):
			if o, ok := s.Store.Overlay(id); ok {
				return o.Name
			}
		case strings.Contains(path, "/webhooks/"):
			for _, h := range s.Store.Webhooks() {
				if h.ID == id {
					return h.Name
				}
			}
		case strings.Contains(path, "/geofences/"):
			for _, g := range s.Store.Geofences() {
				if g.ID == id {
					return g.Name
				}
			}
		case strings.Contains(path, "/drawings/"):
			for _, d := range s.Store.Drawings() {
				if d.ID == id {
					return d.Name
				}
			}
		case strings.Contains(path, "/events/"):
			if e, ok := s.Store.Event(id); ok {
				return e.Name
			}
		case strings.Contains(path, "/profiles/"):
			for _, p := range s.Store.DeviceProfiles() {
				if p.ID == id {
					if f := r.PathValue("name"); f != "" {
						return p.Name + ": " + f
					}
					return p.Name
				}
			}
		case strings.Contains(path, "/emergencies/"):
			for _, e := range s.Store.Emergencies() {
				if e.ID == id {
					return e.Type + ": " + e.Callsign
				}
			}
		case strings.Contains(path, "/enroll-codes/"):
			for _, c := range s.Store.EnrollCodes() {
				if c.ID == id {
					return c.Username
				}
			}
		case strings.Contains(path, "/schedules/"):
			for _, c := range s.Store.Schedules() {
				if c.ID == id {
					return c.Name
				}
			}
		case strings.Contains(path, "/apps/"):
			if a, ok := s.app(id); ok {
				return a.Name + " " + a.Version
			}
		}
		return id
	}
	var parts []string
	for _, k := range []string{"name", "user", "file", "serial", "uid", "fp", "code", "hash", "ip"} {
		if v := r.PathValue(k); v != "" {
			if k == "code" || k == "hash" {
				v = truncate(v, 12)
			}
			parts = append(parts, v)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " → ")
	}
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		for _, k := range []string{"name", "username", "label", "title"} {
			if v, ok := m[k].(string); ok && v != "" {
				return truncate(v, 80)
			}
		}
	}
	return ""
}

// statusWriter remembers the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// auditRequest runs an admin handler and records what it changed.
func (s *Server) auditRequest(h handlerFunc, w http.ResponseWriter, r *http.Request, id *identity) {
	action, ok := auditActions[r.Pattern]
	if r.Method == http.MethodGet || r.Method == http.MethodHead || auditQuiet[r.Pattern] || s.Audit == nil {
		h(w, r, id)
		return
	}
	if !ok {
		action = r.Method + " " + strings.TrimPrefix(r.URL.Path, "/admin/api/")
	}
	// A small JSON body is read for its details, then handed on unchanged.
	var body []byte
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") && r.ContentLength >= 0 && r.ContentLength <= 64<<10 {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 64<<10))
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	target := s.auditTarget(r, body)
	sw := &statusWriter{ResponseWriter: w}
	h(sw, r, id)
	status := sw.status
	if status == 0 {
		status = http.StatusOK
	}
	e := audit.Entry{Actor: id.Username, IP: clientIP(r), Action: action, Target: target, Details: auditDetails(body),
		Status: status, Failed: status >= 400}
	s.Audit.Add(e)
	if !e.Failed {
		go s.notify(notice{Kind: "admin", Title: e.Action, Callsign: e.Target, Username: e.Actor, Text: strings.TrimSpace("By " + e.Actor + ". " + e.Details), Groups: []string{}})
	}
}

// auditEvent records something outside the admin API (sign-ins, enrollment).
func (s *Server) auditEvent(r *http.Request, actor, action, target string, failed bool) {
	if s.Audit == nil {
		return
	}
	e := audit.Entry{Actor: actor, Action: action, Target: target, Failed: failed}
	if r != nil {
		e.IP = clientIP(r)
	}
	s.Audit.Add(e)
}

func auditFilter(r *http.Request) audit.Filter {
	q := r.URL.Query()
	f := audit.Filter{Text: q.Get("q"), Actor: q.Get("actor"), Failed: q.Get("failed") == "1"}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if t, err := time.Parse(time.RFC3339, q.Get("since")); err == nil {
		f.Since = t
	}
	if t, err := time.Parse(time.RFC3339Nano, q.Get("before")); err == nil {
		f.Before = t
	}
	return f
}

func (s *Server) auditRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/audit", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if s.Audit == nil {
			writeJSON(w, http.StatusOK, []audit.Entry{})
			return
		}
		list, err := s.Audit.Query(auditFilter(r))
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	// Clear the log. The clearing is the first entry of the new log, so it
	// can't be hidden.
	mux.HandleFunc("DELETE /admin/api/audit", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if s.Audit == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		n, err := s.Audit.Clear()
		e := audit.Entry{Actor: id.Username, IP: clientIP(r), Action: "Cleared the audit log", Details: fmt.Sprintf("%d entries removed", n)}
		if err != nil {
			e.Failed, e.Status, e.Details = true, http.StatusInternalServerError, e.Details+"; "+err.Error()
		}
		s.Audit.Add(e)
		s.Log.Warn("audit log cleared", "admin", id.Username, "entries", n, "err", err)
		go s.notify(notice{Kind: "admin", Title: e.Action, Username: e.Actor, Text: "By " + e.Actor + ". " + e.Details, Groups: []string{}})
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"removed": n})
	}))
	// The same search as a spreadsheet (up to 5000 entries).
	mux.HandleFunc("GET /admin/api/audit.csv", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		f := auditFilter(r)
		f.Limit = 5000
		var list []audit.Entry
		if s.Audit != nil {
			list, _ = s.Audit.Query(f)
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "edgetak-audit-" + time.Now().Format("2006-01-02") + ".csv"}))
		cw := csv.NewWriter(w)
		cw.Write([]string{"time (UTC)", "who", "from", "action", "target", "details", "result"})
		for _, e := range list {
			result := "ok"
			if e.Failed {
				result = "failed (" + strconv.Itoa(e.Status) + ")"
				if e.Status == 0 {
					result = "failed"
				}
			}
			cw.Write([]string{e.Time.Format(time.RFC3339), e.Actor, e.IP, e.Action, e.Target, e.Details, result})
		}
		cw.Flush()
	}))
}
