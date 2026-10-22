package history

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestExport(t *testing.T) {
	base := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC).UnixMilli()
	tracks := []Track{{UID: "ANDROID-1", Callsign: "ALPHA <1>", Team: "Cyan", Points: []Point{
		{T: base, Lat: 39.5, Lon: -106.0},
		{T: base + 60_000, Lat: 39.501, Lon: -106.001},
		{T: base + 20*60_000, Lat: 39.6, Lon: -106.1}, // after a 19-minute gap
	}}, {UID: "ANDROID-2", Points: nil}}

	k := string(KML(tracks, "Search & rescue"))
	for _, want := range []string{"<name>Search &amp; rescue</name>", "<name>ALPHA &lt;1&gt;</name>",
		"<LineStyle><color>ffffff00</color>", "<begin>2026-10-22T14:00:00Z</begin><end>2026-10-22T14:01:00Z</end>",
		"-106.0000000,39.5000000,0 -106.0010000,39.5010000,0", "<name>ALPHA &lt;1&gt; (2)</name>", "<Point><coordinates>-106.1000000,39.6000000,0",
		"<name>ALPHA &lt;1&gt; end</name><TimeStamp><when>2026-10-22T14:20:00Z</when>"} {
		if !strings.Contains(k, want) {
			t.Errorf("KML lacks %s:\n%s", want, k)
		}
	}
	if strings.Contains(k, "ANDROID-2") {
		t.Error("an empty track was written")
	}
	g := GPX(tracks, "Search & rescue")
	var doc struct {
		Trk []struct {
			Name string `xml:"name"`
			Type string `xml:"type"`
			Seg  []struct {
				Pt []struct {
					Lat  float64 `xml:"lat,attr"`
					Time string  `xml:"time"`
				} `xml:"trkpt"`
			} `xml:"trkseg"`
		} `xml:"trk"`
	}
	if err := xml.Unmarshal(g, &doc); err != nil {
		t.Fatalf("GPX: %v\n%s", err, g)
	}
	if len(doc.Trk) != 1 || doc.Trk[0].Name != "ALPHA <1>" || doc.Trk[0].Type != "Cyan" || len(doc.Trk[0].Seg) != 2 ||
		len(doc.Trk[0].Seg[0].Pt) != 2 || doc.Trk[0].Seg[1].Pt[0].Time != "2026-10-22T14:20:00Z" {
		t.Errorf("GPX = %+v", doc)
	}
	if err := xml.Unmarshal(KML(tracks, "x"), new(struct{})); err != nil {
		t.Errorf("KML isn't well formed: %v", err)
	}
}
