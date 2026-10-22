package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Drawings: markers, routes, areas and circles drawn on the console map
// (Maps tab) and kept on devices' maps. EdgeTAK holds them and sends them
// to the devices that should see them when they are saved and again to
// each device as it connects, so phones that were off get them too.
// Changing one replaces it on devices (same UID); deleting removes it.
// EdgeTAK's copy is the reference: a device's own edits are replaced the
// next time it connects.

// markerTypes are the marker kinds the console offers.
var markerTypes = map[string]bool{
	"a-f-G": true, "a-h-G": true, "a-n-G": true, "a-u-G": true, // friendly, hostile, neutral, unknown
	"b-m-p-s-m": true, // spot map marker (a colored dot)
}

func drawingUID(d *store.Drawing) string { return "edgetak-drawing-" + d.ID }

func drawingVisible(d *store.Drawing, c *router.Client) bool {
	return watched(store.LostContactSettings{Groups: d.Groups}, deviceGroups(c))
}

// drawingEvent is the drawing as ATAK sends it.
func drawingEvent(d *store.Drawing) *cot.Event {
	uid := drawingUID(d)
	switch d.Kind {
	case "area", "circle":
		return shapeEvent(uid, d.Name, d.Remarks, d.Kind == "circle", d.Points, d.Center, d.Radius, d.Color)
	case "route":
		ev := cot.NewEvent("b-m-r", uid, "h-e", fenceShapeStale)
		ev.Lat, ev.Lon, ev.Hae = d.Points[0][0], d.Points[0][1], 9999999
		var b strings.Builder
		for i, p := range d.Points {
			// The ends are waypoints, the points between them checkpoints.
			typ, cs := "b-m-p-c", ""
			if i == 0 || i == len(d.Points)-1 {
				typ = "b-m-p-w"
			}
			if i == 0 {
				cs = d.Name + " start"
			} else if i == len(d.Points)-1 {
				cs = d.Name + " end"
			}
			fmt.Fprintf(&b, `<link uid="%s.%d" callsign="%s" type="%s" point="%.7f,%.7f" remarks="" relation="c"/>`,
				xmlEscape(uid), i, xmlEscape(cs), typ, p[0], p[1])
		}
		color := atakColor(d.Color, 0xFF)
		fmt.Fprintf(&b, `<link_attr color="%d" stroke="3" style="0" type="Vehicle" method="Driving" direction="Infil" routetype="Primary" order="Ascending Check Points" planningmethod="Infil" prefix="CP"/>`, color)
		fmt.Fprintf(&b, `<strokeColor value="%d"/><strokeWeight value="3.0"/><__routeinfo><__navcues/></__routeinfo><color value="%d"/>`, color, color)
		b.WriteString(`<contact callsign="` + xmlEscape(d.Name) + `"/><remarks>` + xmlEscape(d.Remarks) + `</remarks><labels_on value="false"/><archive/>`)
		ev.Detail.XML = b.String()
		return ev
	}
	ev := cot.NewEvent(d.Type, uid, "h-g-i-g-o", fenceShapeStale)
	ev.Lat, ev.Lon, ev.Hae = d.Center[0], d.Center[1], 9999999
	ev.Detail.Contact = &cot.Contact{Callsign: d.Name}
	xml := `<remarks>` + xmlEscape(d.Remarks) + `</remarks><archive/>`
	if d.Type == "b-m-p-s-m" {
		c := atakColor(d.Color, 0xFF)
		xml += fmt.Sprintf(`<usericon iconsetpath="COT_MAPPING_SPOTMAP/b-m-p-s-m/%d"/><color argb="%d"/>`, c, c)
	}
	ev.Detail.XML = xml
	return ev
}

func drawingDelete(d *store.Drawing) *cot.Event {
	ev := cot.NewEvent(cot.TypeDelete, uuid(), "h-g-i-g-o", 20*time.Second)
	ev.Detail.XML = `<link uid="` + xmlEscape(drawingUID(d)) + `" relation="none" type="none"/><__forcedelete/>`
	return ev
}

// sendDrawing puts a drawing on (or takes it off) devices' maps. A device
// that may no longer see it gets the delete.
func (s *Server) sendDrawing(d *store.Drawing, remove bool) {
	for _, c := range s.Router.Clients() {
		if remove || !drawingVisible(d, c) {
			c.Send(drawingDelete(d))
			continue
		}
		c.Send(drawingEvent(d))
	}
}

func (s *Server) drawingsStart() {
	s.Router.OnJoin(func(c *router.Client) {
		for _, d := range s.Store.Drawings() {
			if drawingVisible(&d, c) {
				c.Send(drawingEvent(&d))
			}
		}
	})
}

func validDrawing(d store.Drawing) (store.Drawing, error) {
	d.Name, d.Remarks = strings.TrimSpace(d.Name), strings.TrimSpace(d.Remarks)
	if d.Name == "" || len(d.Name) > 80 || len(d.Remarks) > 2000 {
		return d, fmt.Errorf("give it a name (up to 80 characters; remarks up to 2000)")
	}
	switch d.Kind {
	case "marker":
		if !markerTypes[d.Type] {
			return d, fmt.Errorf("unknown marker type")
		}
		if !validLatLon(d.Center) {
			return d, fmt.Errorf("the marker is outside the map")
		}
		d.Points, d.Radius = nil, 0
	case "circle":
		if d.Radius < 1 || d.Radius > 500000 || !validLatLon(d.Center) {
			return d, fmt.Errorf("a circle needs a center and a radius from 1 m to 500 km")
		}
		d.Points, d.Type = nil, ""
	case "area", "route":
		least := 3
		if d.Kind == "route" {
			least = 2
		}
		if len(d.Points) < least || len(d.Points) > 500 {
			return d, fmt.Errorf("a %s needs %d to 500 points", d.Kind, least)
		}
		for _, p := range d.Points {
			if !validLatLon(p) {
				return d, fmt.Errorf("a point is outside the map")
			}
		}
		d.Radius, d.Type = 0, ""
	default:
		return d, fmt.Errorf("draw a marker, route, area or circle")
	}
	if !hexColor.MatchString(d.Color) {
		d.Color = "#F29900"
	}
	if d.Groups == nil {
		d.Groups = []string{}
	}
	slices.Sort(d.Groups)
	return d, nil
}

func (s *Server) drawingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/drawings", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.Store.Drawings())
	}))
	save := func(w http.ResponseWriter, r *http.Request, id *identity) {
		var d store.Drawing
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&d); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		var old *store.Drawing
		if did := r.PathValue("id"); did != "" {
			for _, o := range s.Store.Drawings() {
				if o.ID == did {
					o := o
					old = &o
				}
			}
			if old == nil {
				jsonError(w, http.StatusNotFound, "no such drawing")
				return
			}
			d.ID, d.Created, d.Author = old.ID, old.Created, old.Author
		} else {
			d.ID, d.Created, d.Author = uuid(), time.Now().UTC(), id.Username
		}
		d.Updated = time.Now().UTC()
		d, err := validDrawing(d)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.Store.PutDrawing(d); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.sendDrawing(&d, false)
		s.Log.Info("drawing saved", "admin", id.Username, "kind", d.Kind, "name", d.Name, "groups", d.Groups)
		writeJSON(w, http.StatusOK, s.Store.Drawings())
	}
	mux.HandleFunc("POST /admin/api/drawings", s.admin(save))
	mux.HandleFunc("PUT /admin/api/drawings/{id}", s.admin(save))
	mux.HandleFunc("DELETE /admin/api/drawings/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		d, err := s.Store.DeleteDrawing(r.PathValue("id"))
		if err != nil {
			jsonError(w, http.StatusNotFound, "no such drawing")
			return
		}
		s.sendDrawing(&d, true)
		s.Log.Info("drawing deleted", "admin", id.Username, "kind", d.Kind, "name", d.Name)
		writeJSON(w, http.StatusOK, s.Store.Drawings())
	}))
}
