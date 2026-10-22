package overlay

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
)

const kml = `<?xml version="1.0"?><kml xmlns="http://www.opengis.net/kml/2.2"><Document>
<Placemark><name>A</name><Point><coordinates>-104.9,39.7,0</coordinates></Point></Placemark>
<Placemark><name>B</name><LineString><coordinates>-105.1,39.5 -104.8,39.9</coordinates></LineString></Placemark>
</Document></kml>`

func TestKML(t *testing.T) {
	f, err := Read([]byte(kml), "sectors.kml")
	if err != nil {
		t.Fatal(err)
	}
	if f.Format != "kml" || f.Ext != ".kml" || f.Items != 2 || f.Bounds != [4]float64{39.5, -105.1, 39.9, -104.8} {
		t.Errorf("kml = %+v", f.Info)
	}
	for _, bad := range []string{"hello", "<html><body/></html>", `{"type":"Nope"}`, `{"type":"FeatureCollection","features":[]}`} {
		if _, err := Read([]byte(bad), "x"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestKMZ(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("files/icon.png")
	w.Write([]byte("png"))
	w, _ = zw.Create("doc.kml")
	io.WriteString(w, kml)
	zw.Close()
	f, err := Read(buf.Bytes(), "area.kmz")
	if err != nil {
		t.Fatal(err)
	}
	if f.Format != "kmz" || f.Ext != ".kmz" || f.Items != 2 || !bytes.Equal(f.Data, buf.Bytes()) {
		t.Errorf("kmz = %+v", f.Info)
	}
}

func TestGeoJSON(t *testing.T) {
	src := `{"type":"FeatureCollection","features":[
	 {"type":"Feature","properties":{"name":"Sector <1>","stroke":"#ff0000","fill":"#00f","fill-opacity":0.25,"team":"Cyan"},
	  "geometry":{"type":"Polygon","coordinates":[[[-105,39],[-104,39],[-104,40],[-105,39]],[[-104.6,39.4],[-104.5,39.4],[-104.5,39.5],[-104.6,39.4]]]}},
	 {"type":"Feature","properties":{"title":"CP"},"geometry":{"type":"MultiPoint","coordinates":[[-104.2,39.2],[-104.3,39.3]]}},
	 {"type":"Feature","properties":{},"geometry":null}]}`
	f, err := Read([]byte(src), "sectors.geojson")
	if err != nil {
		t.Fatal(err)
	}
	out := string(f.Data)
	for _, want := range []string{"<name>sectors</name>", "<name>Sector &lt;1&gt;</name>", "<description>team: Cyan</description>",
		"<LineStyle><color>ff0000ff</color>", "<PolyStyle><color>40ff0000</color>", "<innerBoundaryIs>", "<MultiGeometry><Point>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if f.Format != "geojson" || f.Ext != ".kml" || f.Items != 2 || f.Bounds != [4]float64{39, -105, 40, -104} {
		t.Errorf("geojson = %+v", f.Info)
	}
	// The result is KML that reads back the same.
	if again, err := Read(f.Data, "x.kml"); err != nil || again.Items != 2 {
		t.Errorf("round trip = %+v %v", again.Info, err)
	}
}

func TestPackage(t *testing.T) {
	f, _ := Read([]byte(kml), "a.kml")
	a, err := Package("edgetak-overlay-1", "Sectors", SafeName("Sec/tors", f.Ext), f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Package("edgetak-overlay-1", "Sectors", SafeName("Sec/tors", f.Ext), f)
	if !bytes.Equal(a, b) {
		t.Error("the same overlay packs differently")
	}
	zr, err := zip.NewReader(bytes.NewReader(a), int64(len(a)))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range zr.File {
		names = append(names, e.Name)
		if e.Name == "MANIFEST/manifest.xml" {
			rc, _ := e.Open()
			m, _ := io.ReadAll(rc)
			if !strings.Contains(string(m), `zipEntry="edgetak-overlay-1/Sec_tors.kml"`) || !strings.Contains(string(m), `name="uid" value="edgetak-overlay-1"`) {
				t.Errorf("manifest = %s", m)
			}
		}
	}
	if strings.Join(names, " ") != "MANIFEST/manifest.xml edgetak-overlay-1/Sec_tors.kml" {
		t.Errorf("entries = %v", names)
	}
}
