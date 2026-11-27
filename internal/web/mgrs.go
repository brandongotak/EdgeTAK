package web

import (
	"fmt"
	"math"
)

// mgrs gives a position as an MGRS grid reference with 1 m precision
// ("18SUJ2348706483"), as 9-line MEDEVAC requests read out line 1. It is ""
// outside the UTM bands (south of 80°S, north of 84°N).
func mgrs(lat, lon float64) string {
	if lat < -80 || lat > 84 || math.IsNaN(lat) || math.IsNaN(lon) {
		return ""
	}
	if lon >= 180 {
		lon -= 360
	}
	zone := int((lon+180)/6) + 1
	// Norway and Svalbard exceptions.
	if lat >= 56 && lat < 64 && lon >= 3 && lon < 12 {
		zone = 32
	}
	if lat >= 72 && lat < 84 {
		switch {
		case lon >= 0 && lon < 9:
			zone = 31
		case lon >= 9 && lon < 21:
			zone = 33
		case lon >= 21 && lon < 33:
			zone = 35
		case lon >= 33 && lon < 42:
			zone = 37
		}
	}
	easting, northing := utm(lat, lon, zone)

	const bands = "CDEFGHJKLMNPQRSTUVWX"
	bi := int((lat + 80) / 8)
	if bi > 19 {
		bi = 19 // X runs to 84°N
	}
	band := bands[bi]

	// 100 km square letters: columns repeat every 3 zones, rows every 2.
	colSets := []string{"ABCDEFGH", "JKLMNPQR", "STUVWXYZ"}
	col := colSets[(zone-1)%3][int(easting/100000)-1]
	const rowLetters = "ABCDEFGHJKLMNPQRSTUV"
	rowOffset := 0
	if zone%2 == 0 {
		rowOffset = 5
	}
	row := rowLetters[(int(math.Floor(northing/100000))+rowOffset)%20]
	e := int(math.Floor(easting)) % 100000
	n := int(math.Floor(northing)) % 100000
	return fmt.Sprintf("%d%c%c%c%05d%05d", zone, band, col, row, e, n)
}

// utm converts WGS84 to UTM easting and northing in a zone (northing with
// the 10,000 km false northing in the south).
func utm(lat, lon float64, zone int) (easting, northing float64) {
	const (
		a  = 6378137.0
		f  = 1 / 298.257223563
		k0 = 0.9996
	)
	e2 := f * (2 - f)
	ep2 := e2 / (1 - e2)
	phi := lat * math.Pi / 180
	lambda := lon * math.Pi / 180
	lambda0 := (float64(zone-1)*6 - 180 + 3) * math.Pi / 180

	n := a / math.Sqrt(1-e2*math.Sin(phi)*math.Sin(phi))
	t := math.Tan(phi) * math.Tan(phi)
	c := ep2 * math.Cos(phi) * math.Cos(phi)
	aa := math.Cos(phi) * (lambda - lambda0)
	m := a * ((1-e2/4-3*e2*e2/64-5*e2*e2*e2/256)*phi -
		(3*e2/8+3*e2*e2/32+45*e2*e2*e2/1024)*math.Sin(2*phi) +
		(15*e2*e2/256+45*e2*e2*e2/1024)*math.Sin(4*phi) -
		(35*e2*e2*e2/3072)*math.Sin(6*phi))

	easting = k0*n*(aa+(1-t+c)*aa*aa*aa/6+(5-18*t+t*t+72*c-58*ep2)*aa*aa*aa*aa*aa/120) + 500000
	northing = k0 * (m + n*math.Tan(phi)*(aa*aa/2+(5-t+9*c+4*c*c)*aa*aa*aa*aa/24+(61-58*t+t*t+600*c-330*ep2)*aa*aa*aa*aa*aa*aa/720))
	if lat < 0 {
		northing += 10000000
	}
	return easting, northing
}
