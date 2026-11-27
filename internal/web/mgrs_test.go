package web

import (
	"strings"
	"testing"
)

func TestMGRS(t *testing.T) {
	for _, c := range []struct {
		lat, lon float64
		prefix   string // zone, band and 100 km square, plus digits where well known
	}{
		{0, 0, "31NAA6602100000"},
		{38.8895, -77.0353, "18SUJ23"}, // Washington Monument
		{40.7128, -74.0060, "18TWL8"},  // New York City Hall
		{-33.8688, 151.2093, "56HLH"},  // Sydney
		{60.0, 5.0, "32V"},             // Norway exception
	} {
		got := mgrs(c.lat, c.lon)
		if !strings.HasPrefix(got, c.prefix) || len(got) != len(c.prefix[:3])+12 && len(got) != 15 {
			t.Errorf("mgrs(%v, %v) = %q, want %s…", c.lat, c.lon, got, c.prefix)
		}
	}
	if mgrs(85, 0) != "" || mgrs(-81, 0) != "" {
		t.Error("polar positions should have no MGRS")
	}
}
