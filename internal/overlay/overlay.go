// Package overlay turns a KML, KMZ or GeoJSON file into a data package
// (mission package) that ATAK imports as a map overlay. GeoJSON, which ATAK
// does not read, is converted to KML.
package overlay

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MaxSize is the largest file accepted.
const MaxSize = 64 << 20

// Info describes an overlay file.
type Info struct {
	Format string     // kml, kmz or geojson
	Items  int        // placemarks (features)
	Bounds [4]float64 // south, west, north, east; zero when there are no coordinates
}

// File is an overlay ready for ATAK: a KML or KMZ file.
type File struct {
	Info
	Ext  string // .kml or .kmz
	Data []byte
}

// Read checks an uploaded file and returns it as KML or KMZ.
func Read(data []byte, name string) (File, error) {
	switch {
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		kml, err := kmzDoc(data)
		if err != nil {
			return File{}, err
		}
		info, err := scanKML(kml)
		if err != nil {
			return File{}, err
		}
		info.Format = "kmz"
		return File{Info: info, Ext: ".kmz", Data: data}, nil
	case looksJSON(data):
		kml, info, err := geoJSONToKML(data, strings.TrimSuffix(path.Base(name), path.Ext(name)))
		if err != nil {
			return File{}, err
		}
		info.Format = "geojson"
		return File{Info: info, Ext: ".kml", Data: kml}, nil
	default:
		info, err := scanKML(data)
		if err != nil {
			return File{}, err
		}
		info.Format = "kml"
		return File{Info: info, Ext: ".kml", Data: data}, nil
	}
}

func looksJSON(b []byte) bool {
	b = bytes.TrimLeft(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), " \t\r\n")
	return len(b) > 0 && b[0] == '{'
}

// kmzDoc is a KMZ's main KML: doc.kml, else the first KML at the top level,
// else the first anywhere.
func kmzDoc(data []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("not a readable KMZ (zip) file")
	}
	var pick *zip.File
	for _, f := range zr.File {
		lower := strings.ToLower(f.Name)
		if !strings.HasSuffix(lower, ".kml") {
			continue
		}
		switch {
		case lower == "doc.kml":
			pick = f
		case pick == nil, !strings.Contains(lower, "/") && strings.Contains(pick.Name, "/"):
			pick = f
		}
		if lower == "doc.kml" {
			break
		}
	}
	if pick == nil {
		return nil, errors.New("the KMZ has no KML file in it")
	}
	rc, err := pick.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, MaxSize+1))
	if err != nil || len(b) > MaxSize {
		return nil, errors.New("the KMZ's KML can't be read")
	}
	return b, nil
}

// scanKML checks that b is KML and counts its placemarks and extent.
func scanKML(b []byte) (Info, error) {
	info := Info{}
	bounds := newBounds()
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	root, inCoords := true, false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if root {
				return info, errors.New("not a KML, KMZ or GeoJSON file")
			}
			return info, fmt.Errorf("the KML can't be read: %v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if root {
				if t.Name.Local != "kml" {
					return info, errors.New("not a KML, KMZ or GeoJSON file")
				}
				root = false
			}
			switch t.Name.Local {
			case "Placemark", "GroundOverlay":
				info.Items++
			case "coordinates":
				inCoords = true
			}
		case xml.EndElement:
			if t.Name.Local == "coordinates" {
				inCoords = false
			}
		case xml.CharData:
			if inCoords {
				for _, tuple := range strings.Fields(string(t)) {
					p := strings.Split(tuple, ",")
					if len(p) < 2 {
						continue
					}
					lon, err1 := strconv.ParseFloat(p[0], 64)
					lat, err2 := strconv.ParseFloat(p[1], 64)
					if err1 == nil && err2 == nil {
						bounds.add(lon, lat)
					}
				}
			}
		}
	}
	if root {
		return info, errors.New("not a KML, KMZ or GeoJSON file")
	}
	info.Bounds = bounds.result()
	return info, nil
}

type extent struct{ s, w, n, e float64 }

func newBounds() *extent { return &extent{90, 180, -90, -180} }

func (x *extent) add(lon, lat float64) {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 || math.IsNaN(lat) || math.IsNaN(lon) {
		return
	}
	x.s, x.n = min(x.s, lat), max(x.n, lat)
	x.w, x.e = min(x.w, lon), max(x.e, lon)
}

func (x *extent) result() [4]float64 {
	if x.s > x.n {
		return [4]float64{}
	}
	return [4]float64{x.s, x.w, x.n, x.e}
}

// ---- GeoJSON ----

type geometry struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
	Geometries  []geometry      `json:"geometries"`
}

type feature struct {
	Type       string         `json:"type"`
	Geometry   *geometry      `json:"geometry"`
	Properties map[string]any `json:"properties"`
	Features   []feature      `json:"features"`
	geometry
}

func geoJSONToKML(data []byte, docName string) ([]byte, Info, error) {
	var root feature
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, Info{}, errors.New("the GeoJSON can't be read: " + err.Error())
	}
	var features []feature
	switch root.Type {
	case "FeatureCollection":
		features = root.Features
	case "Feature":
		features = []feature{root}
	case "Point", "MultiPoint", "LineString", "MultiLineString", "Polygon", "MultiPolygon", "GeometryCollection":
		g := root.geometry
		features = []feature{{Type: "Feature", Geometry: &g}}
	default:
		return nil, Info{}, errors.New("not a KML, KMZ or GeoJSON file")
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<kml xmlns="http://www.opengis.net/kml/2.2"><Document><name>`)
	xmlText(&b, docName)
	b.WriteString("</name>\n")
	info := Info{}
	bounds := newBounds()
	for i, f := range features {
		if f.Geometry == nil {
			continue
		}
		var geom strings.Builder
		if err := writeGeometry(&geom, *f.Geometry, bounds); err != nil {
			return nil, Info{}, fmt.Errorf("feature %d: %v", i+1, err)
		}
		if geom.Len() == 0 {
			continue
		}
		info.Items++
		b.WriteString("<Placemark>")
		if name := propString(f.Properties, "name", "title", "Name", "NAME", "label"); name != "" {
			b.WriteString("<name>")
			xmlText(&b, name)
			b.WriteString("</name>")
		}
		if desc := describe(f.Properties); desc != "" {
			b.WriteString("<description>")
			xmlText(&b, desc)
			b.WriteString("</description>")
		}
		b.WriteString(style(f.Properties))
		b.WriteString(geom.String())
		b.WriteString("</Placemark>\n")
	}
	b.WriteString("</Document></kml>\n")
	if info.Items == 0 {
		return nil, Info{}, errors.New("the GeoJSON has no features with a geometry")
	}
	info.Bounds = bounds.result()
	return []byte(b.String()), info, nil
}

func writeGeometry(b *strings.Builder, g geometry, x *extent) error {
	var err error
	switch g.Type {
	case "Point":
		var p []float64
		if err = json.Unmarshal(g.Coordinates, &p); err == nil {
			b.WriteString("<Point><coordinates>" + coords([][]float64{p}, x) + "</coordinates></Point>")
		}
	case "LineString":
		var l [][]float64
		if err = json.Unmarshal(g.Coordinates, &l); err == nil {
			b.WriteString("<LineString><tessellate>1</tessellate><coordinates>" + coords(l, x) + "</coordinates></LineString>")
		}
	case "Polygon":
		var rings [][][]float64
		if err = json.Unmarshal(g.Coordinates, &rings); err == nil {
			polygon(b, rings, x)
		}
	case "MultiPoint", "MultiLineString", "MultiPolygon":
		var parts []json.RawMessage
		if err = json.Unmarshal(g.Coordinates, &parts); err == nil {
			b.WriteString("<MultiGeometry>")
			for _, p := range parts {
				if err = writeGeometry(b, geometry{Type: strings.TrimPrefix(g.Type, "Multi"), Coordinates: p}, x); err != nil {
					break
				}
			}
			b.WriteString("</MultiGeometry>")
		}
	case "GeometryCollection":
		b.WriteString("<MultiGeometry>")
		for _, sub := range g.Geometries {
			if err = writeGeometry(b, sub, x); err != nil {
				break
			}
		}
		b.WriteString("</MultiGeometry>")
	default:
		err = fmt.Errorf("unknown geometry %q", g.Type)
	}
	if err != nil {
		return fmt.Errorf("bad %s coordinates", g.Type)
	}
	return nil
}

func polygon(b *strings.Builder, rings [][][]float64, x *extent) {
	if len(rings) == 0 {
		return
	}
	b.WriteString("<Polygon><outerBoundaryIs><LinearRing><coordinates>" + coords(rings[0], x) + "</coordinates></LinearRing></outerBoundaryIs>")
	for _, hole := range rings[1:] {
		b.WriteString("<innerBoundaryIs><LinearRing><coordinates>" + coords(hole, x) + "</coordinates></LinearRing></innerBoundaryIs>")
	}
	b.WriteString("</Polygon>")
}

func coords(points [][]float64, x *extent) string {
	var b strings.Builder
	for i, p := range points {
		if len(p) < 2 {
			continue
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		x.add(p[0], p[1])
		b.WriteString(strconv.FormatFloat(p[0], 'f', -1, 64) + "," + strconv.FormatFloat(p[1], 'f', -1, 64))
		if len(p) > 2 {
			b.WriteString("," + strconv.FormatFloat(p[2], 'f', -1, 64))
		}
	}
	return b.String()
}

func propString(props map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := props[k]; ok && v != nil {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return s
			}
		}
	}
	return ""
}

// describe is a feature's description, or its other properties as lines.
func describe(props map[string]any) string {
	if d := propString(props, "description", "desc", "Description"); d != "" {
		return d
	}
	var lines []string
	for _, k := range slices.Sorted(maps.Keys(props)) {
		switch k {
		case "name", "title", "Name", "NAME", "label", "stroke", "stroke-width", "stroke-opacity", "fill", "fill-opacity",
			"marker-color", "marker-size", "marker-symbol":
			continue
		}
		v := props[k]
		if v == nil {
			continue
		}
		if _, nested := v.(map[string]any); nested {
			continue
		}
		lines = append(lines, k+": "+fmt.Sprint(v))
	}
	return strings.Join(lines, "\n")
}

// style turns simplestyle properties (stroke, fill, marker-color, ...) into
// an inline KML style.
func style(props map[string]any) string {
	stroke, fill, marker := propString(props, "stroke"), propString(props, "fill"), propString(props, "marker-color")
	if stroke == "" && fill == "" && marker == "" {
		return ""
	}
	num := func(k string, def float64) float64 {
		switch v := props[k].(type) {
		case float64:
			return v
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
		return def
	}
	var b strings.Builder
	b.WriteString("<Style>")
	if c, ok := kmlColor(marker, 1); ok {
		b.WriteString("<IconStyle><color>" + c + "</color></IconStyle>")
	}
	if c, ok := kmlColor(stroke, num("stroke-opacity", 1)); ok {
		b.WriteString("<LineStyle><color>" + c + "</color><width>" + strconv.FormatFloat(num("stroke-width", 2), 'f', -1, 64) + "</width></LineStyle>")
	}
	if c, ok := kmlColor(fill, num("fill-opacity", 0.6)); ok {
		b.WriteString("<PolyStyle><color>" + c + "</color></PolyStyle>")
	}
	b.WriteString("</Style>")
	return b.String()
}

// kmlColor turns #rrggbb (or #rgb) and an opacity into KML's aabbggrr.
func kmlColor(css string, opacity float64) (string, bool) {
	h := strings.TrimPrefix(strings.TrimSpace(css), "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return "", false
	}
	if _, err := strconv.ParseUint(h, 16, 32); err != nil {
		return "", false
	}
	a := int(math.Round(max(0, min(1, opacity)) * 255))
	return strings.ToLower(fmt.Sprintf("%02x%s%s%s", a, h[4:6], h[2:4], h[0:2])), true
}

func xmlText(b *strings.Builder, s string) {
	xml.EscapeText(b, []byte(s))
}

// ---- data package ----

// Package wraps an overlay in a data package ATAK imports: uid stays the
// same across versions, so a new version replaces the old one on devices.
func Package(uid, name, fileName string, f File) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entry := uid + "/" + fileName
	var m strings.Builder
	m.WriteString(`<MissionPackageManifest version="2"><Configuration><Parameter name="uid" value="`)
	xmlText(&m, uid)
	m.WriteString(`"/><Parameter name="name" value="`)
	xmlText(&m, name)
	m.WriteString(`"/><Parameter name="onReceiveDelete" value="false"/></Configuration><Contents><Content ignore="false" zipEntry="`)
	xmlText(&m, entry)
	m.WriteString(`"/></Contents></MissionPackageManifest>`)
	modified := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // the same bytes for the same overlay
	for _, part := range []struct {
		name string
		data []byte
	}{{"MANIFEST/manifest.xml", []byte(m.String())}, {entry, f.Data}} {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: part.name, Method: zip.Deflate, Modified: modified})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(part.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SafeName is a file name for an overlay called name.
func SafeName(name, ext string) string {
	s := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < ' ' {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	if s == "" || s == "." || s == ".." {
		s = "overlay"
	}
	return s + ext
}
