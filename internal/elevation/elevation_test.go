package elevation

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"testing"
)

// Tile makes a minimal DTED tile: a UHL record and some data.
func Tile0(level, lat, lon int) []byte {
	interval := map[int]string{0: "0300", 1: "0030", 2: "0010"}[level]
	ew, ns := 'E', 'N'
	if lon < 0 {
		ew = 'W'
	}
	if lat < 0 {
		ns = 'S'
	}
	h := fmt.Sprintf("UHL1%03d0000%c%03d0000%c%s%s", abs(lon), ew, abs(lat), ns, interval, interval)
	h += fmt.Sprintf("%-52s", "")
	return append([]byte(h), bytes.Repeat([]byte{7}, 100*(level+1))...)
}

func TestImportServePackage(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	if got := RelPath(2, 30, -73); got != "w073/n30.dt2" {
		t.Errorf("RelPath = %s", got)
	}
	if got := RelPath(0, -1, 5); got != "e005/s01.dt0" {
		t.Errorf("RelPath south/east = %s", got)
	}

	// A zip with tiles under the wrong names and folders: placed by header.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, data := range map[string][]byte{
		"stuff/a.dt0":      Tile0(0, 38, -78),
		"x/y/whatever.DT1": Tile0(1, 38, -78),
		"z.dt2":            Tile0(2, -34, 151),
		"readme.txt":       []byte("hi"),
		"bad.dt0":          []byte("not dted at all, but long enough to pass the length check of eighty bytes......."),
	} {
		f, _ := zw.Create(name)
		f.Write(data)
	}
	zw.Close()
	res := s.Import("tiles.zip", bytes.NewReader(zb.Bytes()), int64(zb.Len()))
	if res.Added != 3 || len(res.Skipped) != 1 {
		t.Fatalf("import = %+v", res)
	}
	if b := Tile0(0, 39, -78); s.Import("n38.dt0", bytes.NewReader(b), int64(len(b))).Added != 1 {
		t.Fatal("single tile not added")
	}
	if res := s.Import("photo.jpg", bytes.NewReader([]byte("x")), 1); res.Added != 0 || len(res.Skipped) != 1 {
		t.Errorf("not a tile = %+v", res)
	}
	tiles := s.Tiles()
	var names []string
	for _, tl := range tiles {
		names = append(names, tl.Path)
	}
	if fmt.Sprint(names) != "[e151/s34.dt2 w078/n38.dt0 w078/n38.dt1 w078/n39.dt0]" {
		t.Fatalf("tiles = %v", names)
	}

	// Streaming: the tile alone, named as ATAK expects.
	var sb bytes.Buffer
	if err := s.StreamZip(&sb, "w078/n38.dt0"); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(sb.Bytes()), int64(sb.Len()))
	if len(zr.File) != 1 || zr.File[0].Name != "n38.dt0" {
		t.Errorf("stream zip = %v", zr.File)
	}
	if err := s.StreamZip(&sb, "../../etc/passwd"); err == nil {
		t.Error("a path outside the tiles was served")
	}
	if err := s.StreamZip(&sb, "w010/n10.dt0"); err == nil {
		t.Error("a missing tile was served")
	}

	// The package for devices: DTED/ at the root.
	var pb bytes.Buffer
	s.Package(&pb, []Tile{tiles[0], tiles[2]})
	zr, _ = zip.NewReader(bytes.NewReader(pb.Bytes()), int64(pb.Len()))
	if len(zr.File) != 2 || zr.File[0].Name != "DTED/e151/s34.dt2" || zr.File[1].Name != "DTED/w078/n38.dt1" {
		t.Errorf("package = %v", zr.File)
	}
	rc, _ := zr.File[1].Open()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, Tile0(1, 38, -78)) {
		t.Error("package tile changed")
	}

	if n := s.DeleteLevel(0); n != 2 || len(s.Tiles()) != 2 {
		t.Errorf("delete level 0 = %d, left %d", n, len(s.Tiles()))
	}
}
