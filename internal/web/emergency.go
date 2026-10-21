package web

import (
	"encoding/xml"
	"net/http"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Emergency alerts. ATAK's emergency button sends a b-a-* event whose UID is
// "<device uid>-9-1-1" with <emergency type="911 Alert">CALLSIGN</emergency>,
// and b-a-o-can with <emergency cancel="true"> to cancel it (see ATAK's
// EmergencyManager). The server keeps an alert active until the device
// cancels it or an admin dismisses it, so one from a phone that then drops
// off the network stays on screen. Alerts are routed to devices as usual.

// emergencyTypes are ATAK's EmergencyType values.
var emergencyTypes = map[string]string{
	"b-a-o-tbl": "911 Alert",
	"b-a-o-pan": "Ring The Bell",
	"b-a-o-opn": "In Contact",
	"b-a-g":     "Geo-fence Breached",
	"b-a-o-c":   "Custom",
}

const (
	emergencyCancel    = "b-a-o-can"
	emergencySaveEvery = 30 * time.Second // repeats update the saved position at most this often
	emergencyLogSize   = 100
)

type emergencyXML struct {
	Type   string `xml:"type,attr"`
	Cancel string `xml:"cancel,attr"`
	Text   string `xml:",chardata"`
}

// detailElement decodes the first <name ...> element of a detail.
func detailElement(detail, name string, v any) bool {
	i := strings.Index(detail, "<"+name)
	for i >= 0 {
		// Make sure it is the element itself, not one whose name starts the same.
		if j := i + 1 + len(name); j < len(detail) && strings.ContainsRune(" />\t\r\n", rune(detail[j])) {
			return xml.NewDecoder(strings.NewReader(detail[i:])).Decode(v) == nil
		}
		k := strings.Index(detail[i+1:], "<"+name)
		if k < 0 {
			break
		}
		i += 1 + k
	}
	return false
}

// emergencyHook records alerts and cancellations from devices. It never
// drops events.
func (s *Server) emergencyHook(src *router.Client, ev *cot.Event) bool {
	if src == nil || !strings.HasPrefix(ev.Type, "b-a-") {
		return true
	}
	desc, isAlert := emergencyTypes[ev.Type]
	if !isAlert && ev.Type != emergencyCancel {
		return true
	}
	var em emergencyXML
	detailElement(ev.Detail.XML, "emergency", &em)
	var link struct {
		UID string `xml:"uid,attr"`
	}
	detailElement(ev.Detail.XML, "link", &link)
	srcUID, srcCallsign := src.Identity()
	device := firstNonEmpty(link.UID, strings.TrimSuffix(ev.UID, "-9-1-1"), srcUID)
	now := time.Now().UTC()

	var active *store.Emergency
	for _, e := range s.Store.Emergencies() {
		if e.Active() && (e.EventUID == ev.UID || e.DeviceUID == device) {
			e := e
			active = &e
			break
		}
	}
	if ev.Type == emergencyCancel || strings.EqualFold(em.Cancel, "true") {
		if active == nil {
			return true
		}
		if _, err := s.Store.UpdateEmergency(active.ID, func(e *store.Emergency) {
			e.Ended, e.EndedBy, e.Updated = &now, "device", now
		}); err != nil {
			s.Log.Error("store emergency", "err", err)
		}
		s.Log.Warn("emergency cancelled by the device", "callsign", active.Callsign, "type", active.Type, "user", src.Username)
		return true
	}
	if em.Type != "" {
		desc = em.Type // custom alerts carry their own text
	}
	callsign := firstNonEmpty(strings.TrimSpace(em.Text), srcCallsign, device)
	if active != nil {
		if active.Type == desc && now.Sub(active.Updated) < emergencySaveEvery {
			return true
		}
		if _, err := s.Store.UpdateEmergency(active.ID, func(e *store.Emergency) {
			e.Type, e.CotType, e.Updated = desc, ev.Type, now
			if ev.Lat != 0 || ev.Lon != 0 {
				e.Lat, e.Lon = ev.Lat, ev.Lon
			}
		}); err != nil {
			s.Log.Error("store emergency", "err", err)
		}
		return true
	}
	e := store.Emergency{ID: uuid(), EventUID: ev.UID, DeviceUID: device, Callsign: callsign, Username: src.Username,
		Type: desc, CotType: ev.Type, Lat: ev.Lat, Lon: ev.Lon, Started: now, Updated: now}
	if err := s.Store.AddEmergency(e); err != nil {
		s.Log.Error("store emergency", "err", err)
	}
	s.Log.Warn("EMERGENCY", "type", desc, "callsign", callsign, "user", src.Username, "lat", ev.Lat, "lon", ev.Lon)
	return true
}

// activeEmergencies returns the alerts still active, newest first.
func (s *Server) activeEmergencies() []store.Emergency {
	out := []store.Emergency{}
	for _, e := range s.Store.Emergencies() {
		if e.Active() {
			out = append(out, e)
		}
	}
	return out
}

func (s *Server) emergencyRoutes(mux *http.ServeMux) {
	// ?active=1 leaves out the log: the console asks every few seconds.
	mux.HandleFunc("GET /admin/api/emergencies", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		if r.URL.Query().Get("active") != "" {
			writeJSON(w, http.StatusOK, map[string]any{"active": s.activeEmergencies()})
			return
		}
		all := s.Store.Emergencies()
		if len(all) > emergencyLogSize {
			all = all[:emergencyLogSize]
		}
		writeJSON(w, http.StatusOK, map[string]any{"active": s.activeEmergencies(), "log": all})
	}))
	// Acknowledge: the console stops sounding the alarm; the alert stays.
	mux.HandleFunc("POST /admin/api/emergencies/{id}/ack", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		now := time.Now().UTC()
		e, err := s.Store.UpdateEmergency(r.PathValue("id"), func(e *store.Emergency) {
			if e.Acked == nil {
				e.Acked, e.AckedBy = &now, id.Username
			}
		})
		if err != nil {
			jsonError(w, http.StatusNotFound, "alert not found")
			return
		}
		s.Log.Info("emergency acknowledged", "admin", id.Username, "callsign", e.Callsign, "type", e.Type)
		writeJSON(w, http.StatusOK, e)
	}))
	// Dismiss: close an alert the device can no longer cancel. Devices are
	// not told; their own alert stays until it is cancelled there.
	mux.HandleFunc("POST /admin/api/emergencies/{id}/dismiss", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		now := time.Now().UTC()
		e, err := s.Store.UpdateEmergency(r.PathValue("id"), func(e *store.Emergency) {
			if e.Ended == nil {
				e.Ended, e.EndedBy = &now, id.Username
			}
			if e.Acked == nil {
				e.Acked, e.AckedBy = &now, id.Username
			}
		})
		if err != nil {
			jsonError(w, http.StatusNotFound, "alert not found")
			return
		}
		s.Log.Warn("emergency dismissed by an administrator", "admin", id.Username, "callsign", e.Callsign, "type", e.Type)
		writeJSON(w, http.StatusOK, e)
	}))
}
