package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Geofences: areas drawn on the console map. Each position a device reports
// is checked against them; crossing a boundary in a watched direction
// raises an alert (an emergency record of kind "geofence") that ends when
// the device crosses back. A device's first position only sets where it
// is, so drawing an area doesn't alert everyone already in it. Areas can
// also be drawn on devices' maps, as ATAK shapes.

type geofenceState struct {
	mu     sync.Mutex
	inside map[string]map[string]bool // fence ID -> device UID -> inside
	alerts map[string]string          // fence ID + "|" + device UID -> active alert
}

var hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// contains reports whether lat, lon is inside the fence.
func contains(g *store.Geofence, lat, lon float64) bool {
	if g.Shape == "circle" {
		return distanceMeters(lat, lon, g.Center[0], g.Center[1]) <= g.Radius
	}
	in := false
	pts := g.Points
	for i, j := 0, len(pts)-1; i < len(pts); j, i = i, i+1 {
		yi, xi, yj, xj := pts[i][0], pts[i][1], pts[j][0], pts[j][1]
		if (yi > lat) != (yj > lat) && lon < (xj-xi)*(lat-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

func distanceMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371008.8
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := p2-p1, (lon2-lon1)*math.Pi/180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(a)))
}

// geofenceHook checks each device's own position reports against the fences.
func (s *Server) geofenceHook(src *router.Client, ev *cot.Event) bool {
	if src == nil || !strings.HasPrefix(ev.Type, "a-") {
		return true
	}
	uid, callsign := src.Identity()
	if uid == "" || ev.UID != uid || (ev.Lat == 0 && ev.Lon == 0) {
		return true // not its own position, or no fix
	}
	fences := s.Store.Geofences()
	if len(fences) == 0 {
		return true
	}
	groups := deviceGroups(src)
	for i := range fences {
		g := &fences[i]
		if g.Off || !watched(store.LostContactSettings{Groups: g.Groups}, groups) {
			continue
		}
		s.crossing(g, uid, callsign, src.Username, ev.Lat, ev.Lon)
	}
	return true
}

// crossing records where a device is relative to a fence, and alerts when
// it crossed the boundary.
func (s *Server) crossing(g *store.Geofence, uid, callsign, username string, lat, lon float64) {
	in := contains(g, lat, lon)
	key := g.ID + "|" + uid
	s.fences.mu.Lock()
	defer s.fences.mu.Unlock()
	if s.fences.inside == nil {
		s.fences.inside, s.fences.alerts = map[string]map[string]bool{}, map[string]string{}
	}
	devices := s.fences.inside[g.ID]
	if devices == nil {
		devices = map[string]bool{}
		s.fences.inside[g.ID] = devices
	}
	was, known := devices[uid]
	devices[uid] = in
	if !known || was == in {
		return
	}
	// It crossed: the alert for the other direction is over.
	if id := s.fences.alerts[key]; id != "" {
		s.endAlert(id, "device")
		delete(s.fences.alerts, key)
	}
	if (in && g.Trigger == "exit") || (!in && g.Trigger == "enter") {
		return
	}
	what := "Left " + g.Name
	if in {
		what = "Entered " + g.Name
	}
	now := time.Now().UTC()
	id := uuid()
	if err := s.Store.AddEmergency(store.Emergency{ID: id, Kind: "geofence", EventUID: key, DeviceUID: uid, Callsign: callsign,
		Username: username, Type: what, Lat: lat, Lon: lon, Started: now, Updated: now}); err != nil {
		s.Log.Error("store geofence alert", "err", err)
		return
	}
	s.fences.alerts[key] = id
	s.Log.Warn("GEOFENCE", "what", what, "callsign", callsign, "user", username, "lat", lat, "lon", lon)
}

// endAlert closes an alert (unless an admin already did).
func (s *Server) endAlert(id, by string) {
	now := time.Now().UTC()
	s.Store.UpdateEmergency(id, func(e *store.Emergency) {
		if e.Ended == nil {
			e.Ended, e.EndedBy = &now, by
		}
	})
}

// forgetFence drops a fence's state and ends its alerts (changed or deleted).
func (s *Server) forgetFence(id, by string) {
	s.fences.mu.Lock()
	defer s.fences.mu.Unlock()
	delete(s.fences.inside, id)
	for key, alert := range s.fences.alerts {
		if strings.HasPrefix(key, id+"|") {
			s.endAlert(alert, by)
			delete(s.fences.alerts, key)
		}
	}
}

func (s *Server) geofenceStart() {
	// Alerts still open from before a restart end when the device crosses back.
	s.fences.mu.Lock()
	s.fences.inside, s.fences.alerts = map[string]map[string]bool{}, map[string]string{}
	for _, e := range s.activeEmergencies() {
		if e.Kind == "geofence" {
			fence, uid, _ := strings.Cut(e.EventUID, "|")
			s.fences.alerts[e.EventUID] = e.ID
			if s.fences.inside[fence] == nil {
				s.fences.inside[fence] = map[string]bool{}
			}
			s.fences.inside[fence][uid] = strings.HasPrefix(e.Type, "Entered ")
		}
	}
	s.fences.mu.Unlock()
	s.Router.AddHook(s.geofenceHook)
	s.Router.OnJoin(func(c *router.Client) {
		for _, g := range s.Store.Geofences() {
			if g.Devices && fenceVisible(&g, c) {
				c.Send(fenceShape(&g))
			}
		}
	})
}

// ---- shapes on devices' maps ----

const fenceShapeStale = 365 * 24 * time.Hour

func fenceUID(g *store.Geofence) string { return "edgetak-geofence-" + g.ID }

func fenceVisible(g *store.Geofence, c *router.Client) bool {
	return watched(store.LostContactSettings{Groups: g.Groups}, deviceGroups(c))
}

func fenceShape(g *store.Geofence) *cot.Event {
	return shapeEvent(fenceUID(g), g.Name, "Geofence (EdgeTAK)", g.Shape == "circle", g.Points, g.Center, g.Radius, g.Color)
}

// atakColor turns #RRGGBB into ATAK's signed ARGB integer.
func atakColor(hex string, alpha uint32) int32 {
	rgb, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	return int32(alpha<<24 | uint32(rgb))
}

// shapeEvent is an area or circle as ATAK draws shapes: a closed freeform
// polygon (u-d-f) or a circle (u-d-c-c).
func shapeEvent(uid, name, remarks string, circle bool, points [][2]float64, center [2]float64, radius float64, color string) *cot.Event {
	stroke, fill := atakColor(color, 0xFF), atakColor(color, 0x40)
	style := fmt.Sprintf(`<strokeColor value="%d"/><strokeWeight value="4.0"/><fillColor value="%d"/>`, stroke, fill) +
		`<contact callsign="` + xmlEscape(name) + `"/><remarks>` + xmlEscape(remarks) + `</remarks><labels_on value="true"/><archive/>`
	if circle {
		ev := cot.NewEvent("u-d-c-c", uid, "h-e", fenceShapeStale)
		ev.Lat, ev.Lon, ev.Hae = center[0], center[1], 9999999
		kml := func(argb uint32) string { // KML colors are aabbggrr
			return fmt.Sprintf("%02x%02x%02x%02x", argb>>24, argb&0xff, argb>>8&0xff, argb>>16&0xff)
		}
		ev.Detail.XML = fmt.Sprintf(`<shape><ellipse major="%g" minor="%g" angle="360"/>`, radius, radius) +
			`<link uid="` + xmlEscape(uid) + `.Style" type="b-x-KmlStyle" relation="p-c"><Style><LineStyle><color>` + kml(uint32(stroke)) +
			`</color><width>4.0</width></LineStyle><PolyStyle><color>` + kml(uint32(fill)) + `</color></PolyStyle></Style></link></shape>` + style
		return ev
	}
	ev := cot.NewEvent("u-d-f", uid, "h-e", fenceShapeStale)
	var lat, lon float64
	var links strings.Builder
	for _, p := range points {
		lat, lon = lat+p[0], lon+p[1]
		fmt.Fprintf(&links, `<link point="%.7f,%.7f"/>`, p[0], p[1])
	}
	fmt.Fprintf(&links, `<link point="%.7f,%.7f"/>`, points[0][0], points[0][1]) // closed
	ev.Lat, ev.Lon, ev.Hae = lat/float64(len(points)), lon/float64(len(points)), 9999999
	ev.Detail.XML = links.String() + style
	return ev
}

func fenceDelete(g *store.Geofence) *cot.Event {
	ev := cot.NewEvent(cot.TypeDelete, uuid(), "h-g-i-g-o", 20*time.Second)
	ev.Detail.XML = `<link uid="` + xmlEscape(fenceUID(g)) + `" relation="none" type="none"/><__forcedelete/>`
	return ev
}

// sendFence draws (or removes) a fence on the devices that should see it.
func (s *Server) sendFence(g *store.Geofence, remove bool) {
	for _, c := range s.Router.Clients() {
		if remove || !g.Devices || !fenceVisible(g, c) {
			c.Send(fenceDelete(g))
			continue
		}
		c.Send(fenceShape(g))
	}
}

// ---- API ----

func validGeofence(g store.Geofence) (store.Geofence, error) {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" || len(g.Name) > 80 {
		return g, fmt.Errorf("give the area a name (up to 80 characters)")
	}
	switch g.Shape {
	case "circle":
		if g.Radius < 5 || g.Radius > 500000 || !validLatLon(g.Center) {
			return g, fmt.Errorf("a circle needs a center and a radius from 5 m to 500 km")
		}
		g.Points = nil
	case "polygon":
		if len(g.Points) < 3 || len(g.Points) > 500 {
			return g, fmt.Errorf("an area needs 3 to 500 points")
		}
		for _, p := range g.Points {
			if !validLatLon(p) {
				return g, fmt.Errorf("a point is outside the map")
			}
		}
		g.Radius = 0
	default:
		return g, fmt.Errorf("the shape is a polygon or a circle")
	}
	if !slices.Contains([]string{"enter", "exit", "both"}, g.Trigger) {
		return g, fmt.Errorf("alert on enter, exit or both")
	}
	if !hexColor.MatchString(g.Color) {
		g.Color = "#F29900"
	}
	if g.Groups == nil {
		g.Groups = []string{}
	}
	return g, nil
}

func validLatLon(p [2]float64) bool {
	return p[0] >= -90 && p[0] <= 90 && p[1] >= -180 && p[1] <= 180 && !math.IsNaN(p[0]) && !math.IsNaN(p[1])
}

type geofenceView struct {
	store.Geofence
	Inside int `json:"inside"` // devices known to be inside now
}

func (s *Server) geofencesView() []geofenceView {
	out := []geofenceView{}
	s.fences.mu.Lock()
	defer s.fences.mu.Unlock()
	for _, g := range s.Store.Geofences() {
		v := geofenceView{Geofence: g}
		for _, in := range s.fences.inside[g.ID] {
			if in {
				v.Inside++
			}
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) geofenceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/geofences", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.geofencesView())
	}))
	save := func(w http.ResponseWriter, r *http.Request, id *identity) {
		var g store.Geofence
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&g); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		var old *store.Geofence
		if fid := r.PathValue("id"); fid != "" {
			for _, f := range s.Store.Geofences() {
				if f.ID == fid {
					f := f
					old = &f
				}
			}
			if old == nil {
				jsonError(w, http.StatusNotFound, "no such area")
				return
			}
			g.ID, g.Created = old.ID, old.Created
		} else {
			g.ID, g.Created = uuid(), time.Now().UTC()
		}
		g.Updated = time.Now().UTC()
		g, err := validGeofence(g)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.Store.PutGeofence(g); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if old != nil {
			s.forgetFence(g.ID, id.Username) // the area changed: start over
		}
		s.sendFence(&g, false)
		s.Log.Info("geofence saved", "admin", id.Username, "name", g.Name, "shape", g.Shape, "trigger", g.Trigger, "groups", g.Groups)
		writeJSON(w, http.StatusOK, s.geofencesView())
	}
	mux.HandleFunc("POST /admin/api/geofences", s.admin(save))
	mux.HandleFunc("PUT /admin/api/geofences/{id}", s.admin(save))
	mux.HandleFunc("DELETE /admin/api/geofences/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		g, err := s.Store.DeleteGeofence(r.PathValue("id"))
		if err != nil {
			jsonError(w, http.StatusNotFound, "no such area")
			return
		}
		s.forgetFence(g.ID, id.Username)
		s.sendFence(&g, true)
		s.Log.Info("geofence deleted", "admin", id.Username, "name", g.Name)
		writeJSON(w, http.StatusOK, s.geofencesView())
	}))
}
