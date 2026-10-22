package apps

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"image"
	"image/color"
	"image/draw"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/shogo82148/androidbinary"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/vector"
)

// Icons drawn by Android from XML: vector drawables (paths), adaptive icons
// (a background and a foreground layer) and layer lists. Many plugins have
// only these, so EdgeTAK draws them itself: fills and strokes of path data
// (lines, curves, arcs), groups with transforms, and colors and drawables
// referenced from the app's resource table. Gradients, clip paths and theme
// attributes are not drawn.

const iconSize = 192

// drawableRenderer draws drawables from one APK.
type drawableRenderer struct {
	files map[string]*zip.File
	table *androidbinary.TableFile
	cfg   *androidbinary.ResTableConfig
}

func newDrawableRenderer(zr *zip.Reader) *drawableRenderer {
	r := &drawableRenderer{files: map[string]*zip.File{}, cfg: &androidbinary.ResTableConfig{Density: 480}}
	for _, f := range zr.File {
		r.files[f.Name] = f
	}
	if b := r.read("resources.arsc"); b != nil {
		r.table, _ = androidbinary.NewTableFile(bytes.NewReader(b))
	}
	return r
}

func (r *drawableRenderer) read(name string) []byte {
	f, ok := r.files[name]
	if !ok || f.UncompressedSize64 > 8<<20 {
		return nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil
	}
	return b
}

// resource resolves a reference ("@0x7F070006") to a file path (string) or
// a color (uint32); ok is false for values that are not references.
func (r *drawableRenderer) resource(v string) (any, bool) {
	if r.table == nil || !strings.HasPrefix(v, "@0x") {
		return nil, false
	}
	n, err := strconv.ParseUint(v[3:], 16, 32)
	if err != nil || n>>24 != 0x7F { // the app's own package
		return nil, false
	}
	res, err := r.table.GetResource(androidbinary.ResID(n), r.cfg)
	if err != nil || res == nil {
		return nil, false
	}
	return res, true
}

// render draws the drawable at path into a size x size image.
func (r *drawableRenderer) render(path string, size, depth int) image.Image {
	if depth > 4 {
		return nil
	}
	data := r.read(path)
	if data == nil {
		return nil
	}
	if !strings.HasSuffix(path, ".xml") {
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return nil
		}
		return scale(img, size, size)
	}
	xf, err := androidbinary.NewXMLFile(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	var root element
	if err := xml.NewDecoder(xf.Reader()).Decode(&root); err != nil {
		return nil
	}
	switch root.XMLName.Local {
	case "vector":
		return r.vector(&root, size)
	case "adaptive-icon":
		// Layers are 108 dp, of which the launcher shows the middle 72.
		full := size * 108 / 72
		dst := image.NewNRGBA(image.Rect(0, 0, full, full))
		for _, name := range []string{"background", "foreground"} {
			if c := root.child(name); c != nil {
				r.layer(dst, c.attr("drawable"), full, depth)
			}
		}
		off := (full - size) / 2
		out := image.NewNRGBA(image.Rect(0, 0, size, size))
		draw.Draw(out, out.Bounds(), dst, image.Pt(off, off), draw.Src)
		return out
	case "shape": // a filled rectangle or oval
		c, ok := r.color(childAttr(&root, "solid", "color"))
		if !ok {
			return nil
		}
		dst := image.NewNRGBA(image.Rect(0, 0, size, size))
		if root.attr("shape") == "1" || root.attr("shape") == "0x00000001" {
			ras := vector.NewRasterizer(size, size)
			disc(ras, float64(size)/2, float64(size)/2, float64(size)/2)
			paint(dst, ras, c, 1)
		} else {
			draw.Draw(dst, dst.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
		}
		return dst
	case "inset": // another drawable, shrunk by its insets
		res, ok := r.resource(root.attr("drawable"))
		p, isPath := res.(string)
		if !ok || !isPath {
			return nil
		}
		inset := func(name string) int { return int(insetFraction(root.attr(name)) * float64(size)) }
		in := image.Rect(inset("insetLeft"), inset("insetTop"), size-inset("insetRight"), size-inset("insetBottom"))
		if in.Dx() <= 0 || in.Dy() <= 0 {
			return nil
		}
		img := r.render(p, max(in.Dx(), in.Dy()), depth+1)
		if img == nil {
			return nil
		}
		dst := image.NewNRGBA(image.Rect(0, 0, size, size))
		draw.Draw(dst, in, scale(img, in.Dx(), in.Dy()), image.Point{}, draw.Over)
		return dst
	case "bitmap":
		if res, ok := r.resource(root.attr("src")); ok {
			if p, ok := res.(string); ok {
				return r.render(p, size, depth+1)
			}
		}
		return nil
	case "layer-list":
		dst := image.NewNRGBA(image.Rect(0, 0, size, size))
		for i := range root.Children {
			if c := &root.Children[i]; c.XMLName.Local == "item" {
				r.layer(dst, c.attr("drawable"), size, depth)
			}
		}
		return dst
	}
	return nil
}

// layer draws a referenced drawable or color over dst.
func (r *drawableRenderer) layer(dst *image.NRGBA, ref string, size, depth int) {
	res, ok := r.resource(ref)
	if !ok {
		if c, ok := parseColor(ref); ok {
			draw.Draw(dst, dst.Bounds(), image.NewUniform(c), image.Point{}, draw.Over)
		}
		return
	}
	switch v := res.(type) {
	case string:
		if img := r.render(v, size, depth+1); img != nil {
			draw.Draw(dst, dst.Bounds(), img, image.Point{}, draw.Over)
		}
	case uint32:
		draw.Draw(dst, dst.Bounds(), image.NewUniform(argb(v)), image.Point{}, draw.Over)
	}
}

// element is a decoded XML element with its attributes (android: names
// without the prefix).
type element struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Children []element  `xml:",any"`
}

func (e *element) attr(name string) string {
	for _, a := range e.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func (e *element) child(name string) *element {
	for i := range e.Children {
		if e.Children[i].XMLName.Local == name {
			return &e.Children[i]
		}
	}
	return nil
}

func childAttr(e *element, child, name string) string {
	if c := e.child(child); c != nil {
		return c.attr(name)
	}
	return ""
}

// insetFraction reads an inset as a fraction of the drawable's size. Android
// stores fractions ("27%") and dimensions in the same packed form: the low
// nibble is the unit, bits 4-5 the radix, the rest the mantissa. Dimensions
// are taken as dp of a 108 dp adaptive icon layer.
func insetFraction(v string) float64 {
	if !strings.HasPrefix(v, "@0x") {
		return 0
	}
	n, err := strconv.ParseUint(v[3:], 16, 32)
	if err != nil {
		return 0
	}
	mult := [4]float64{1.0 / (1 << 8), 1.0 / (1 << 15), 1.0 / (1 << 23), 1.0 / (1 << 31)}
	value := float64(int32(uint32(n)&0xffffff00)) * mult[(n>>4)&3]
	if value > 1 { // a dimension
		value /= 108
	}
	return math.Max(0, math.Min(0.45, value))
}

// typedFloat reads a float attribute, stored as "@0x" and its bits.
func typedFloat(v string, def float64) float64 {
	if strings.HasPrefix(v, "@0x") {
		if n, err := strconv.ParseUint(v[3:], 16, 32); err == nil {
			return float64(math.Float32frombits(uint32(n)))
		}
		return def
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return def
}

func argb(v uint32) color.NRGBA {
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: uint8(v >> 24)}
}

// parseColor reads "@0xAARRGGBB" or "#RRGGBB" / "#AARRGGBB".
func parseColor(v string) (color.NRGBA, bool) {
	switch {
	case strings.HasPrefix(v, "@0x"):
		n, err := strconv.ParseUint(v[3:], 16, 32)
		return argb(uint32(n)), err == nil
	case strings.HasPrefix(v, "#"):
		h := v[1:]
		if len(h) == 6 {
			h = "FF" + h
		}
		n, err := strconv.ParseUint(h, 16, 32)
		return argb(uint32(n)), err == nil && len(h) == 8
	}
	return color.NRGBA{}, false
}

// color resolves a color attribute: a value, or a color resource.
func (r *drawableRenderer) color(v string) (color.NRGBA, bool) {
	if res, ok := r.resource(v); ok {
		if n, ok := res.(uint32); ok {
			return argb(n), true
		}
		return color.NRGBA{}, false // a color state list or gradient
	}
	return parseColor(v)
}

// affine is a 2-D transform: x' = a*x + c*y + e, y' = b*x + d*y + f.
type affine struct{ a, b, c, d, e, f float64 }

func (m affine) apply(x, y float64) (float64, float64) {
	return m.a*x + m.c*y + m.e, m.b*x + m.d*y + m.f
}

func (m affine) then(n affine) affine { // n after m
	return affine{
		a: n.a*m.a + n.c*m.b, b: n.b*m.a + n.d*m.b,
		c: n.a*m.c + n.c*m.d, d: n.b*m.c + n.d*m.d,
		e: n.a*m.e + n.c*m.f + n.e, f: n.b*m.e + n.d*m.f + n.f,
	}
}

func (m affine) scale() float64 { return math.Sqrt(math.Abs(m.a*m.d - m.b*m.c)) }

func (r *drawableRenderer) vector(root *element, size int) image.Image {
	vw, vh := typedFloat(root.attr("viewportWidth"), 24), typedFloat(root.attr("viewportHeight"), 24)
	if vw <= 0 || vh <= 0 {
		return nil
	}
	w, h := size, size
	if vw > vh {
		h = int(float64(size) * vh / vw)
	} else if vh > vw {
		w = int(float64(size) * vw / vh)
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	alpha := typedFloat(root.attr("alpha"), 1)
	r.group(dst, root, affine{a: float64(w) / vw, d: float64(h) / vh}, alpha)
	return dst
}

func (r *drawableRenderer) group(dst *image.NRGBA, g *element, m affine, alpha float64) {
	for i := range g.Children {
		c := &g.Children[i]
		switch c.XMLName.Local {
		case "group":
			px, py := typedFloat(c.attr("pivotX"), 0), typedFloat(c.attr("pivotY"), 0)
			sx, sy := typedFloat(c.attr("scaleX"), 1), typedFloat(c.attr("scaleY"), 1)
			rot := typedFloat(c.attr("rotation"), 0) * math.Pi / 180
			tx, ty := typedFloat(c.attr("translateX"), 0), typedFloat(c.attr("translateY"), 0)
			cos, sin := math.Cos(rot), math.Sin(rot)
			// Android: translate(-pivot), scale, rotate, translate(pivot + translate).
			local := affine{a: 1, d: 1, e: -px, f: -py}.
				then(affine{a: sx, d: sy}).
				then(affine{a: cos, b: sin, c: -sin, d: cos}).
				then(affine{a: 1, d: 1, e: px + tx, f: py + ty})
			r.group(dst, c, local.then(m), alpha)
		case "path":
			r.path(dst, c, m, alpha)
		}
	}
}

func (r *drawableRenderer) path(dst *image.NRGBA, p *element, m affine, alpha float64) {
	subpaths := parsePathData(p.attr("pathData"))
	if len(subpaths) == 0 {
		return
	}
	b := dst.Bounds()
	if fill, ok := r.color(p.attr("fillColor")); ok && fill.A > 0 {
		ras := vector.NewRasterizer(b.Dx(), b.Dy())
		for _, sp := range subpaths {
			for i, pt := range sp {
				x, y := m.apply(pt[0], pt[1])
				if i == 0 {
					ras.MoveTo(float32(x), float32(y))
				} else {
					ras.LineTo(float32(x), float32(y))
				}
			}
			ras.ClosePath()
		}
		paint(dst, ras, fill, alpha*typedFloat(p.attr("fillAlpha"), 1))
	}
	stroke, ok := r.color(p.attr("strokeColor"))
	width := typedFloat(p.attr("strokeWidth"), 0) * m.scale()
	if !ok || stroke.A == 0 || width <= 0 {
		return
	}
	// Each segment as its own quad, plus a disc at each joint, merged into one
	// mask (so overlapping pieces don't cancel out).
	mask := image.NewAlpha(b)
	piece := func(build func(*vector.Rasterizer)) {
		ras := vector.NewRasterizer(b.Dx(), b.Dy())
		build(ras)
		ras.DrawOp = draw.Over
		ras.Draw(mask, b, image.Opaque, image.Point{})
	}
	half := width / 2
	for _, sp := range subpaths {
		for i := range sp {
			x, y := m.apply(sp[i][0], sp[i][1])
			piece(func(ras *vector.Rasterizer) { disc(ras, x, y, half) })
			if i == 0 {
				continue
			}
			x0, y0 := m.apply(sp[i-1][0], sp[i-1][1])
			dx, dy := x-x0, y-y0
			l := math.Hypot(dx, dy)
			if l == 0 {
				continue
			}
			nx, ny := -dy/l*half, dx/l*half
			piece(func(ras *vector.Rasterizer) {
				ras.MoveTo(float32(x0+nx), float32(y0+ny))
				ras.LineTo(float32(x+nx), float32(y+ny))
				ras.LineTo(float32(x-nx), float32(y-ny))
				ras.LineTo(float32(x0-nx), float32(y0-ny))
				ras.ClosePath()
			})
		}
	}
	a := alpha * typedFloat(p.attr("strokeAlpha"), 1) * float64(stroke.A) / 255
	src := image.NewUniform(color.NRGBA{stroke.R, stroke.G, stroke.B, 255})
	draw.DrawMask(dst, b, src, image.Point{}, &scaledAlpha{mask, a}, b.Min, draw.Over)
}

func disc(ras *vector.Rasterizer, x, y, r float64) {
	const n = 16
	for i := 0; i <= n; i++ {
		t := float64(i) * 2 * math.Pi / n
		px, py := float32(x+r*math.Cos(t)), float32(y+r*math.Sin(t))
		if i == 0 {
			ras.MoveTo(px, py)
		} else {
			ras.LineTo(px, py)
		}
	}
	ras.ClosePath()
}

// paint fills ras's coverage with c at the given opacity.
func paint(dst *image.NRGBA, ras *vector.Rasterizer, c color.NRGBA, alpha float64) {
	mask := image.NewAlpha(dst.Bounds())
	ras.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
	src := image.NewUniform(color.NRGBA{c.R, c.G, c.B, 255})
	draw.DrawMask(dst, dst.Bounds(), src, image.Point{}, &scaledAlpha{mask, alpha * float64(c.A) / 255}, image.Point{}, draw.Over)
}

// scaledAlpha is a mask scaled by a constant opacity.
type scaledAlpha struct {
	m *image.Alpha
	k float64
}

func (s *scaledAlpha) ColorModel() color.Model { return color.AlphaModel }
func (s *scaledAlpha) Bounds() image.Rectangle { return s.m.Bounds() }
func (s *scaledAlpha) At(x, y int) color.Color {
	return color.Alpha{A: uint8(float64(s.m.AlphaAt(x, y).A) * math.Max(0, math.Min(1, s.k)))}
}

func scale(img image.Image, w, h int) image.Image {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
	return dst
}

// ---- path data ----

// parsePathData turns SVG/Android path data into subpaths of points, with
// curves and arcs flattened into short line segments.
func parsePathData(d string) [][][2]float64 {
	p := &pathParser{s: d}
	var out [][][2]float64
	var cur [][2]float64
	var x, y, sx, sy float64 // current point and subpath start
	var cx, cy float64       // last control point, for S and T
	var prev byte            // previous command
	flush := func() {
		if len(cur) > 1 {
			out = append(out, cur)
		}
		cur = nil
	}
	lineTo := func(nx, ny float64) {
		if len(cur) == 0 {
			cur = append(cur, [2]float64{x, y})
		}
		cur = append(cur, [2]float64{nx, ny})
		x, y = nx, ny
	}
	cubic := func(x1, y1, x2, y2, x3, y3 float64) {
		x0, y0 := x, y
		const n = 12
		for i := 1; i <= n; i++ {
			t := float64(i) / n
			u := 1 - t
			lineTo(u*u*u*x0+3*u*u*t*x1+3*u*t*t*x2+t*t*t*x3, u*u*u*y0+3*u*u*t*y1+3*u*t*t*y2+t*t*t*y3)
		}
		cx, cy = x2, y2
	}
	quad := func(x1, y1, x2, y2 float64) {
		x0, y0 := x, y
		const n = 10
		for i := 1; i <= n; i++ {
			t := float64(i) / n
			u := 1 - t
			lineTo(u*u*x0+2*u*t*x1+t*t*x2, u*u*y0+2*u*t*y1+t*t*y2)
		}
		cx, cy = x1, y1
	}
	for guard := 0; guard < 1<<20; guard++ {
		cmd, ok := p.command()
		if !ok {
			break
		}
		rel := cmd >= 'a'
		up := cmd &^ 0x20
		ox, oy := 0.0, 0.0
		if rel {
			ox, oy = x, y
		}
		first := true
		for first || (up != 'Z' && p.hasNumber()) {
			if !first && rel {
				ox, oy = x, y
			}
			switch up {
			case 'M':
				nx, ny, ok := p.pair()
				if !ok {
					flush()
					return out
				}
				if first {
					flush()
					x, y = nx+ox, ny+oy
					sx, sy = x, y
					cur = [][2]float64{{x, y}}
				} else {
					lineTo(nx+ox, ny+oy) // extra pairs after M are lines
				}
			case 'L':
				nx, ny, ok := p.pair()
				if !ok {
					flush()
					return out
				}
				lineTo(nx+ox, ny+oy)
			case 'H':
				nx, ok := p.number()
				if !ok {
					flush()
					return out
				}
				lineTo(nx+ox, y)
			case 'V':
				ny, ok := p.number()
				if !ok {
					flush()
					return out
				}
				lineTo(x, ny+oy)
			case 'C':
				v, ok := p.numbers(6)
				if !ok {
					flush()
					return out
				}
				cubic(v[0]+ox, v[1]+oy, v[2]+ox, v[3]+oy, v[4]+ox, v[5]+oy)
			case 'S':
				v, ok := p.numbers(4)
				if !ok {
					flush()
					return out
				}
				x1, y1 := x, y
				if prev == 'C' || prev == 'S' {
					x1, y1 = 2*x-cx, 2*y-cy
				}
				cubic(x1, y1, v[0]+ox, v[1]+oy, v[2]+ox, v[3]+oy)
			case 'Q':
				v, ok := p.numbers(4)
				if !ok {
					flush()
					return out
				}
				quad(v[0]+ox, v[1]+oy, v[2]+ox, v[3]+oy)
			case 'T':
				v, ok := p.numbers(2)
				if !ok {
					flush()
					return out
				}
				x1, y1 := x, y
				if prev == 'Q' || prev == 'T' {
					x1, y1 = 2*x-cx, 2*y-cy
				}
				quad(x1, y1, v[0]+ox, v[1]+oy)
			case 'A':
				rx, ok1 := p.number()
				ry, ok2 := p.number()
				rot, ok3 := p.number()
				large, ok4 := p.flag()
				sweep, ok5 := p.flag()
				ex, ey, ok6 := p.pair()
				if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6) {
					flush()
					return out
				}
				for _, pt := range arc(x, y, rx, ry, rot, large, sweep, ex+ox, ey+oy) {
					lineTo(pt[0], pt[1])
				}
			case 'Z':
				if len(cur) > 0 {
					lineTo(sx, sy)
				}
				flush()
				x, y = sx, sy
			default:
				flush()
				return out
			}
			prev = up
			first = false
		}
	}
	flush()
	return out
}

// arc flattens an elliptical arc (SVG endpoint form) into points.
func arc(x1, y1, rx, ry, phiDeg float64, large, sweep bool, x2, y2 float64) [][2]float64 {
	if rx == 0 || ry == 0 || (x1 == x2 && y1 == y2) {
		return [][2]float64{{x2, y2}}
	}
	rx, ry = math.Abs(rx), math.Abs(ry)
	phi := phiDeg * math.Pi / 180
	cos, sin := math.Cos(phi), math.Sin(phi)
	dx, dy := (x1-x2)/2, (y1-y2)/2
	x1p, y1p := cos*dx+sin*dy, -sin*dx+cos*dy
	if l := x1p*x1p/(rx*rx) + y1p*y1p/(ry*ry); l > 1 {
		rx, ry = rx*math.Sqrt(l), ry*math.Sqrt(l)
	}
	num := rx*rx*ry*ry - rx*rx*y1p*y1p - ry*ry*x1p*x1p
	den := rx*rx*y1p*y1p + ry*ry*x1p*x1p
	k := math.Sqrt(math.Max(0, num/den))
	if large == sweep {
		k = -k
	}
	cxp, cyp := k*rx*y1p/ry, -k*ry*x1p/rx
	ccx, ccy := cos*cxp-sin*cyp+(x1+x2)/2, sin*cxp+cos*cyp+(y1+y2)/2
	angle := func(ux, uy, vx, vy float64) float64 { return math.Atan2(ux*vy-uy*vx, ux*vx+uy*vy) }
	t1 := angle(1, 0, (x1p-cxp)/rx, (y1p-cyp)/ry)
	dt := angle((x1p-cxp)/rx, (y1p-cyp)/ry, (-x1p-cxp)/rx, (-y1p-cyp)/ry)
	if !sweep && dt > 0 {
		dt -= 2 * math.Pi
	} else if sweep && dt < 0 {
		dt += 2 * math.Pi
	}
	n := max(4, int(math.Ceil(math.Abs(dt)/(math.Pi/16))))
	pts := make([][2]float64, 0, n)
	for i := 1; i <= n; i++ {
		t := t1 + dt*float64(i)/float64(n)
		ex, ey := rx*math.Cos(t), ry*math.Sin(t)
		pts = append(pts, [2]float64{cos*ex - sin*ey + ccx, sin*ex + cos*ey + ccy})
	}
	pts[len(pts)-1] = [2]float64{x2, y2}
	return pts
}

type pathParser struct {
	s string
	i int
}

func (p *pathParser) skip() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == ',' || p.s[p.i] == '\n' || p.s[p.i] == '\t' || p.s[p.i] == '\r') {
		p.i++
	}
}

func (p *pathParser) command() (byte, bool) {
	p.skip()
	if p.i >= len(p.s) {
		return 0, false
	}
	c := p.s[p.i]
	if strings.IndexByte("MmLlHhVvCcSsQqTtAaZz", c) < 0 {
		return 0, false
	}
	p.i++
	return c, true
}

func (p *pathParser) hasNumber() bool {
	p.skip()
	return p.i < len(p.s) && strings.IndexByte("+-.0123456789", p.s[p.i]) >= 0
}

// number reads one number; "1.2.3" is 1.2 then .3, "1-2" is 1 then -2.
func (p *pathParser) number() (float64, bool) {
	p.skip()
	start := p.i
	if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
		p.i++
	}
	dot, digits := false, false
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c >= '0' && c <= '9':
			digits = true
		case c == '.' && !dot:
			dot = true
		case (c == 'e' || c == 'E') && digits:
			p.i++
			if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
				p.i++
			}
			continue
		default:
			goto done
		}
		p.i++
	}
done:
	f, err := strconv.ParseFloat(p.s[start:p.i], 64)
	if err != nil {
		p.i = start
		return 0, false
	}
	return f, true
}

// flag reads an arc flag, which may be written without a separator ("11").
func (p *pathParser) flag() (bool, bool) {
	p.skip()
	if p.i < len(p.s) && (p.s[p.i] == '0' || p.s[p.i] == '1') {
		p.i++
		return p.s[p.i-1] == '1', true
	}
	return false, false
}

func (p *pathParser) pair() (float64, float64, bool) {
	a, ok1 := p.number()
	b, ok2 := p.number()
	return a, b, ok1 && ok2
}

func (p *pathParser) numbers(n int) ([]float64, bool) {
	out := make([]float64, n)
	for i := range out {
		v, ok := p.number()
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}
