package apps

import (
	"math"
	"testing"
)

func TestParsePathData(t *testing.T) {
	// Compact numbers ("6.8-1.2", ".5.5"), relative commands, H/V, closing.
	sp := parsePathData("M6.5,14.5 l1-1 h2 v.5.5 z m10,0 L20,14.5")
	if len(sp) != 2 {
		t.Fatalf("subpaths = %v", sp)
	}
	want := [][2]float64{{6.5, 14.5}, {7.5, 13.5}, {9.5, 13.5}, {9.5, 14}, {9.5, 14.5}, {6.5, 14.5}}
	if len(sp[0]) != len(want) {
		t.Fatalf("first = %v", sp[0])
	}
	for i := range want {
		if math.Abs(sp[0][i][0]-want[i][0]) > 1e-9 || math.Abs(sp[0][i][1]-want[i][1]) > 1e-9 {
			t.Errorf("point %d = %v, want %v", i, sp[0][i], want[i])
		}
	}
	// After z, a relative move starts from the subpath's start.
	if sp[1][0] != [2]float64{16.5, 14.5} || sp[1][len(sp[1])-1] != [2]float64{20, 14.5} {
		t.Errorf("second = %v", sp[1])
	}
	// Arcs, with flags written together ("0 1,1"), end where they say.
	sp = parsePathData("M50.0,2.38 A47.62,47.62 0 1,1 49.99,2.38 M0,0 a4,4 0 0 1 8,0")
	if len(sp) != 2 {
		t.Fatalf("arcs = %d subpaths", len(sp))
	}
	end := sp[0][len(sp[0])-1]
	if math.Abs(end[0]-49.99) > 1e-9 || math.Abs(end[1]-2.38) > 1e-9 || len(sp[0]) < 20 {
		t.Errorf("full circle ends at %v after %d points", end, len(sp[0]))
	}
	top := 0.0
	for _, p := range sp[1] {
		top = math.Min(top, p[1])
	}
	if math.Abs(top+4) > 0.05 || sp[1][len(sp[1])-1] != [2]float64{8, 0} {
		t.Errorf("half circle: top %v, end %v", top, sp[1][len(sp[1])-1])
	}
	// Garbage stops the parser instead of looping.
	if sp := parsePathData("M1,1 L . x 4"); len(sp) > 1 {
		t.Errorf("garbage = %v", sp)
	}
}

func TestTypedValues(t *testing.T) {
	if v := typedFloat("@0x41C00000", 0); v != 24 {
		t.Errorf("float = %v", v)
	}
	if c, ok := parseColor("@0xFFFFC107"); !ok || c.R != 0xFF || c.G != 0xC1 || c.B != 0x07 || c.A != 0xFF {
		t.Errorf("color = %v", c)
	}
	if f := insetFraction("@0x228F5C30"); math.Abs(f-0.27) > 0.001 {
		t.Errorf("27%% inset = %v", f)
	}
	if f := insetFraction("@0x00001801"); math.Abs(f-24.0/108) > 0.001 { // 24dp
		t.Errorf("24dp inset = %v", f)
	}
}
