package history

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Exports: trails as KML (for Google Earth, ATAK, or EdgeTAK's overlays)
// and GPX (for GIS and GPS tools). A trail is split where a device went
// quiet for longer than SegmentGap, so gaps aren't drawn as straight lines.

// SegmentGap is the longest pause within one trail segment.
const SegmentGap = 5 * time.Minute

// segments splits a track's points where they are far apart in time.
func segments(points []Point) [][]Point {
	var out [][]Point
	start := 0
	for i := 1; i <= len(points); i++ {
		if i == len(points) || points[i].T-points[i-1].T > SegmentGap.Milliseconds() {
			out = append(out, points[start:i])
			start = i
		}
	}
	return out
}

// teamColors are ATAK's team colors as #RRGGBB.
var teamColors = map[string]string{
	"White": "#FFFFFF", "Yellow": "#FFFF00", "Orange": "#FF7700", "Magenta": "#FF00FF", "Red": "#FF0000",
	"Maroon": "#7F0000", "Purple": "#7F007F", "Dark Blue": "#00007F", "Blue": "#0000FF", "Cyan": "#00FFFF",
	"Teal": "#007F7F", "Green": "#00FF00", "Dark Green": "#007F00", "Brown": "#A0714F",
}

// kmlColor is a team's color in KML's aabbggrr (orange without a team).
func kmlColor(team string) string {
	c, ok := teamColors[team]
	if !ok {
		c = "#F29900"
	}
	return "ff" + strings.ToLower(c[5:7]+c[3:5]+c[1:3])
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func iso(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

func coord(p Point) string {
	return strconv.FormatFloat(p.Lon, 'f', 7, 64) + "," + strconv.FormatFloat(p.Lat, 'f', 7, 64) + ",0"
}

func trackName(t Track) string {
	if t.Callsign != "" {
		return t.Callsign
	}
	return t.UID
}

// KML writes the tracks as a KML document: per device, a folder with its
// trail segments (lines with their time span) and start and end points.
func KML(tracks []Track, title string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<kml xmlns="http://www.opengis.net/kml/2.2"><Document>`)
	fmt.Fprintf(&b, "<name>%s</name>\n", esc(title))
	for _, t := range tracks {
		if len(t.Points) == 0 {
			continue
		}
		name, color := trackName(t), kmlColor(t.Team)
		first, last := t.Points[0], t.Points[len(t.Points)-1]
		fmt.Fprintf(&b, "<Folder><name>%s</name><description>%s</description>\n", esc(name),
			esc(fmt.Sprintf("%s · %s to %s · %d positions", t.UID, iso(first.T), iso(last.T), len(t.Points))))
		for i, seg := range segments(t.Points) {
			label := name
			if i > 0 {
				label = fmt.Sprintf("%s (%d)", name, i+1)
			}
			fmt.Fprintf(&b, "<Placemark><name>%s</name><TimeSpan><begin>%s</begin><end>%s</end></TimeSpan>"+
				"<Style><LineStyle><color>%s</color><width>3</width></LineStyle></Style>", esc(label), iso(seg[0].T), iso(seg[len(seg)-1].T), color)
			if len(seg) == 1 {
				fmt.Fprintf(&b, "<Point><coordinates>%s</coordinates></Point></Placemark>\n", coord(seg[0]))
				continue
			}
			b.WriteString("<LineString><tessellate>1</tessellate><coordinates>")
			for j, p := range seg {
				if j > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(coord(p))
			}
			b.WriteString("</coordinates></LineString></Placemark>\n")
		}
		for _, end := range []struct {
			what string
			p    Point
		}{{"start", first}, {"end", last}} {
			fmt.Fprintf(&b, "<Placemark><name>%s %s</name><TimeStamp><when>%s</when></TimeStamp>"+
				"<Style><IconStyle><color>%s</color></IconStyle></Style><Point><coordinates>%s</coordinates></Point></Placemark>\n",
				esc(name), end.what, iso(end.p.T), color, coord(end.p))
		}
		b.WriteString("</Folder>\n")
	}
	b.WriteString("</Document></kml>\n")
	return []byte(b.String())
}

// GPX writes the tracks as GPX 1.1: a track per device, a segment per
// stretch of movement, each point with its time.
func GPX(tracks []Track, title string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<gpx version="1.1" creator="EdgeTAK" xmlns="http://www.topografix.com/GPX/1/1">`)
	fmt.Fprintf(&b, "<metadata><name>%s</name></metadata>\n", esc(title))
	for _, t := range tracks {
		if len(t.Points) == 0 {
			continue
		}
		fmt.Fprintf(&b, "<trk><name>%s</name><desc>%s</desc>", esc(trackName(t)), esc(t.UID))
		if t.Team != "" {
			fmt.Fprintf(&b, "<type>%s</type>", esc(t.Team))
		}
		b.WriteString("\n")
		for _, seg := range segments(t.Points) {
			b.WriteString("<trkseg>")
			for _, p := range seg {
				fmt.Fprintf(&b, `<trkpt lat="%s" lon="%s"><time>%s</time></trkpt>`,
					strconv.FormatFloat(p.Lat, 'f', 7, 64), strconv.FormatFloat(p.Lon, 'f', 7, 64), iso(p.T))
			}
			b.WriteString("</trkseg>\n")
		}
		b.WriteString("</trk>\n")
	}
	b.WriteString("</gpx>\n")
	return []byte(b.String())
}
