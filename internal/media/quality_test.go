package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"testing"

	"github.com/yuvraj-agarwal/educhan/internal/media/jpegenc"
)

// photo is a w×h picture with a photograph's mix of smooth areas, soft edges
// and grain, the same every time for the same size: what decides how large a
// JPEG of it is, which quadrants and gradients do not have.
func photo(w, h int) *image.RGBA {
	rng := rand.New(rand.NewSource(int64(w)*7919 + int64(h)))
	// noise is value noise over a grid of cells of the given size, smoothly
	// interpolated between random values at the cells' corners.
	noise := func(cell int) func(x, y int) float64 {
		gw := w/cell + 2
		v := make([]float64, gw*(h/cell+2))
		for i := range v {
			v[i] = rng.Float64()
		}
		return func(x, y int) float64 {
			fx, fy := float64(x)/float64(cell), float64(y)/float64(cell)
			x0, y0 := int(fx), int(fy)
			tx, ty := fx-float64(x0), fy-float64(y0)
			top := v[y0*gw+x0]*(1-tx) + v[y0*gw+x0+1]*tx
			bottom := v[(y0+1)*gw+x0]*(1-tx) + v[(y0+1)*gw+x0+1]*tx
			return top*(1-ty) + bottom*ty
		}
	}
	type layers struct{ coarse, mid, fine func(x, y int) float64 }
	var ch [3]layers
	for i := range ch {
		ch[i] = layers{noise(80), noise(14), noise(3)}
	}
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var px [3]uint8
			for i, l := range ch {
				v := 0.5*l.coarse(x, y) + 0.3*l.mid(x, y) + 0.15*l.fine(x, y) + 0.05*rng.Float64()
				px[i] = uint8(255 * v)
			}
			m.SetRGBA(x, y, color.RGBA{px[0], px[1], px[2], 255})
		}
	}
	return m
}

// zigzag is the zig-zag order of the JPEG specification's Figure A.6, written
// out rather than worked out: zigzag[k] is the natural position, row × 8 +
// column, of the coefficient at zig-zag position k.
var zigzag = [64]int{
	0, 1, 8, 16, 9, 2, 3, 10,
	17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34,
	27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36,
	29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46,
	53, 60, 61, 54, 47, 55, 62, 63,
}

func TestTransposedZigTransposesTheBlock(t *testing.T) {
	for k, n := range zigzag {
		turned := (n%8)*8 + n/8
		got := transposedZig(k)
		if got < 0 || got > 63 || zigzag[got] != turned {
			t.Errorf("transposedZig(%d) = %d, want the position of natural %d", k, got, turned)
		}
	}
}

// fineEnough reports whether jpegenc's tables at quality q quantise no
// coefficient of any component more coarsely than tables did, with the
// coefficients of a transposed picture compared with the ones they came
// from. It is the rule jpegQuality applies, worked out in natural order.
func fineEnough(q int, comps int, tq [4]byte, tables *[4][64]uint16, transposed bool) bool {
	ours := jpegenc.QuantTables(q)
	for c := 0; c < comps; c++ {
		for k, n := range zigzag {
			from := n
			if transposed {
				from = (n%8)*8 + n/8
			}
			at := 0
			for zigzag[at] != from {
				at++
			}
			if int(ours[min(c, 1)][k]) > int(tables[tq[c]][at]) {
				return false
			}
		}
	}
	return true
}

// tablesHeader is the segments of a JPEG up to its first scan: the given DQT
// segments and a baseline frame header of comps components naming tables
// tq, in the order given by dqtAfterSOF. No picture follows; readJPEGFrame
// stops at the scan's marker.
func tablesHeader(comps int, tq [4]byte, dqtAfterSOF bool, dqts ...[]byte) []byte {
	sof := []byte{8, 0, 16, 0, 16, byte(comps)}
	for c := 0; c < comps; c++ {
		sampling := byte(0x11)
		if c == 0 && comps == 3 {
			sampling = 0x22
		}
		sof = append(sof, byte(c+1), sampling, tq[c])
	}
	out := []byte{0xFF, 0xD8}
	if !dqtAfterSOF {
		for _, d := range dqts {
			out = append(out, jpegSegment(0xDB, d)...)
		}
	}
	out = append(out, jpegSegment(0xC0, sof)...)
	if dqtAfterSOF {
		for _, d := range dqts {
			out = append(out, jpegSegment(0xDB, d)...)
		}
	}
	return append(out, 0xFF, 0xDA, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3F, 0x00)
}

// dqtPayload is one table of a DQT segment, at 8 or 16 bits a value.
func dqtPayload(id byte, wide bool, table []uint16) []byte {
	if !wide {
		out := []byte{id}
		for _, v := range table {
			out = append(out, byte(v))
		}
		return out
	}
	out := []byte{0x10 | id}
	for _, v := range table {
		out = binary.BigEndian.AppendUint16(out, v)
	}
	return out
}

func qualityOf(data []byte, orientation int) int {
	f, framed := readJPEGFrame(data)
	return jpegQuality(f, framed, orientation)
}

// TestStoredQualityIsTheUploadsOwn: a JPEG saved with the standard tables, as
// image/jpeg, libjpeg and most software save one, is stored with the very
// tables it was saved with, up to storedJPEGQuality's.
func TestStoredQualityIsTheUploadsOwn(t *testing.T) {
	pic := photo(48, 32)
	grey := image.NewGray(pic.Rect)
	for i := range grey.Pix {
		grey.Pix[i] = pic.Pix[4*i+1]
	}
	encoders := map[string]func(q int) []byte{
		"image/jpeg 4:2:0": func(q int) []byte {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, pic, &jpeg.Options{Quality: q}); err != nil {
				t.Fatal(err)
			}
			return buf.Bytes()
		},
		"jpegenc 4:4:4": func(q int) []byte {
			var buf bytes.Buffer
			if err := jpegenc.Encode(&buf, pic, &jpegenc.Options{Quality: q}); err != nil {
				t.Fatal(err)
			}
			return buf.Bytes()
		},
		"grey": func(q int) []byte {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, grey, &jpeg.Options{Quality: q}); err != nil {
				t.Fatal(err)
			}
			return buf.Bytes()
		},
	}
	for name, encode := range encoders {
		for q := 1; q <= 100; q++ {
			got := qualityOf(encode(q), 1)
			if q >= storedJPEGQuality {
				if got != storedJPEGQuality {
					t.Errorf("%s at quality %d: stored at %d, want %d", name, q, got, storedJPEGQuality)
				}
				continue
			}
			if jpegenc.QuantTables(got) != jpegenc.QuantTables(q) {
				t.Errorf("%s at quality %d: stored at %d, with other tables", name, q, got)
			}
		}
	}
}

// TestStoredQualityHoldsToAnyTables checks the rule against tables no
// encoder's quality setting makes: random ones, at 8 and 16 bits, for grey
// and colour, upright and on their side. The quality chosen quantises nothing
// more coarsely than they did, and the one below it would.
func TestStoredQualityHoldsToAnyTables(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 500; i++ {
		comps := 3
		if i%4 == 0 {
			comps = 1
		}
		wide := i%3 == 0
		limit := 256
		if wide {
			limit = 400
		}
		var tables [4][64]uint16
		var dqt []byte
		for id := byte(0); id < 3; id++ {
			// A scale, and then each value somewhere around it, so that
			// some tables come out near a quality's and some nowhere near.
			base := 1 + rng.Intn(limit-1)
			for k := range tables[id] {
				tables[id][k] = uint16(min(limit-1, max(1, base/2+rng.Intn(base+1))))
			}
			dqt = append(dqt, dqtPayload(id, wide, tables[id][:])...)
		}
		tq := [4]byte{0, 1, 2}
		if i%5 == 0 {
			tq = [4]byte{1, 0, 0}
		}
		o := 1
		if i%2 == 1 {
			o = 6
		}
		data := tablesHeader(comps, tq, i%7 == 0, dqt)
		got := qualityOf(data, o)
		if got < 1 || got > storedJPEGQuality {
			t.Fatalf("case %d: quality %d", i, got)
		}
		if got < storedJPEGQuality && !fineEnough(got, comps, tq, &tables, o >= 5) {
			t.Errorf("case %d: quality %d quantises more coarsely than the upload did", i, got)
		}
		if got > 1 && fineEnough(got-1, comps, tq, &tables, o >= 5) {
			t.Errorf("case %d: quality %d chosen where %d would do", i, got, got-1)
		}
	}
}

// TestStoredQualityIsNinetyWhenTheTablesCannotBeRead covers every way
// jpegQuality has of not knowing, and the tables it still reads.
func TestStoredQualityIsNinetyWhenTheTablesCannotBeRead(t *testing.T) {
	q30 := jpegenc.QuantTables(30)
	luma, chroma := make([]uint16, 64), make([]uint16, 64)
	for k := range luma {
		luma[k], chroma[k] = uint16(q30[0][k]), uint16(q30[1][k])
	}
	both := append(dqtPayload(0, false, luma), dqtPayload(1, false, chroma)...)
	colour := [4]byte{0, 1, 1}

	cases := []struct {
		name string
		data []byte
		want int
	}{
		{"tables before the frame", tablesHeader(3, colour, false, both), 30},
		{"tables after the frame", tablesHeader(3, colour, true, both), 30},
		{"tables in two segments", tablesHeader(3, colour, false, dqtPayload(0, false, luma), dqtPayload(1, false, chroma)), 30},
		{"16-bit tables", tablesHeader(3, colour, false, append(dqtPayload(0, true, luma), dqtPayload(1, true, chroma)...)), 30},
		{"a table redefined: the last one counts", tablesHeader(3, colour, false, dqtPayload(0, false, chroma), both), 30},
		{"no tables", tablesHeader(3, colour, false), storedJPEGQuality},
		{"a component's table missing", tablesHeader(3, [4]byte{0, 1, 2}, false, both), storedJPEGQuality},
		{"a table cut short", tablesHeader(3, colour, false, both[:100]), storedJPEGQuality},
		{"a precision that does not exist", tablesHeader(3, colour, false, append([]byte{0x20}, both[1:]...)), storedJPEGQuality},
		{"a table number that does not exist", tablesHeader(3, colour, false, append(both, 0x04)), storedJPEGQuality},
		{"CMYK", readTestdata(t, "cost-cmyk.jpg"), storedJPEGQuality},
		{"RGB", readTestdata(t, "cost-rgb.jpg"), storedJPEGQuality},
	}
	header := tablesHeader(3, colour, false, both)
	cases = append(cases,
		struct {
			name string
			data []byte
			want int
		}{"no scan", header[:len(header)-10], storedJPEGQuality},
		struct {
			name string
			data []byte
			want int
		}{"a second frame header", withSegments(header, jpegSegment(0xC0, []byte{8, 0, 16, 0, 16, 1, 1, 0x11, 0})), storedJPEGQuality},
	)
	for _, c := range cases {
		if got := qualityOf(c.data, 1); got != c.want {
			t.Errorf("%s: quality %d, want %d", c.name, got, c.want)
		}
	}
}

// meanDifference is the mean absolute difference between two pictures of the
// same size, over every colour channel, in 8-bit levels.
func meanDifference(t *testing.T, a, b image.Image) float64 {
	t.Helper()
	if a.Bounds() != b.Bounds() {
		t.Fatalf("pictures of %v and %v", a.Bounds(), b.Bounds())
	}
	var sum, n float64
	r := a.Bounds()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			r1, g1, b1, _ := a.At(x, y).RGBA()
			r2, g2, b2, _ := b.At(x, y).RGBA()
			for _, d := range []int{int(r1>>8) - int(r2>>8), int(g1>>8) - int(g2>>8), int(b1>>8) - int(b2>>8)} {
				sum += float64(max(d, -d))
				n++
			}
		}
	}
	return sum / n
}

// TestLowQualityJPEGsAreNotStoredLarger: a photo saved at a low quality is
// stored at that quality, so at about the size it came in at and looking as
// it did. Stored at 90, it came out at up to twice its size, holding its own
// blocking in more detail. Re-encoded by the encoder that made it, at its own
// tables, a file comes back as it was; one from libjpeg with Huffman tables
// fitted to the picture, as an editor's "optimise" makes, comes back a little
// larger, because Go writes the standard ones.
func TestLowQualityJPEGsAreNotStoredLarger(t *testing.T) {
	type upload struct {
		name   string
		data   []byte
		growth float64
	}
	var uploads []upload
	pic := photo(640, 480)
	for _, q := range []int{10, 30, 50, 70, 85} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, pic, &jpeg.Options{Quality: q}); err != nil {
			t.Fatal(err)
		}
		uploads = append(uploads, upload{fmt.Sprintf("image/jpeg at quality %d", q), buf.Bytes(), 1.02})
	}
	uploads = append(uploads, upload{"libjpeg at quality 30, optimised", readTestdata(t, "photo-q30.jpg"), 1.25})
	for _, u := range uploads {
		p := process(t, u.data)
		if growth := float64(len(p.Data)) / float64(len(u.data)); growth > u.growth {
			t.Errorf("%s: %d bytes stored as %d, %.2f times", u.name, len(u.data), len(p.Data), growth)
		}
		before, err := jpeg.Decode(bytes.NewReader(u.data))
		if err != nil {
			t.Fatal(err)
		}
		if d := meanDifference(t, before, decodeStored(t, p.Data)); d > 0.5 {
			t.Errorf("%s: stored picture differs from the upload by %.2f levels on average", u.name, d)
		}
	}
}

// TestJPEGKeepsItsChromaSubsampling: every subsampling image/jpeg decodes is
// stored at that subsampling, upright or turned, as a turned picture's
// planes have it; one whose chroma cannot be turned in its planes is stored
// at 4:4:4. Two of the uploads come from libjpeg rather than jpegenc.
func TestJPEGKeepsItsChromaSubsampling(t *testing.T) {
	type upload struct {
		name  string
		data  []byte
		ratio image.YCbCrSubsampleRatio
	}
	var uploads []upload
	// ycbcrJPEG encodes photo(w, h) at ratio with jpegenc, the subsampling
	// kept exactly.
	ycbcrJPEG := func(w, h int, ratio image.YCbCrSubsampleRatio) []byte {
		pic := photo(w, h)
		m := image.NewYCbCr(pic.Rect, ratio)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				c := pic.RGBAAt(x, y)
				yy, cb, cr := color.RGBToYCbCr(c.R, c.G, c.B)
				m.Y[m.YOffset(x, y)] = yy
				m.Cb[m.COffset(x, y)], m.Cr[m.COffset(x, y)] = cb, cr
			}
		}
		var buf bytes.Buffer
		if err := jpegenc.Encode(&buf, m, &jpegenc.Options{Quality: 95}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	ratios := []image.YCbCrSubsampleRatio{
		image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio422, image.YCbCrSubsampleRatio420,
		image.YCbCrSubsampleRatio440, image.YCbCrSubsampleRatio411, image.YCbCrSubsampleRatio410,
	}
	for _, ratio := range ratios {
		uploads = append(uploads, upload{ratio.String(), ycbcrJPEG(64, 48, ratio), ratio})
	}
	// An odd-sided 4:2:0 cannot keep its chroma grid through a turn that mirrors
	// the odd side: its short last chroma sample would land first and shift the
	// colour along that side, so the turned picture is stored at 4:4:4 instead
	// (turnedSubsampling, orientYCbCr). Odd height, so the 180° and 90° turns
	// here both promote it, while upright it stays 4:2:0.
	uploads = append(uploads, upload{"4:2:0 odd side", ycbcrJPEG(64, 49, image.YCbCrSubsampleRatio420), image.YCbCrSubsampleRatio420})
	uploads = append(uploads,
		upload{"progressive 4:4:4 from libjpeg", readTestdata(t, "cost-prog444.jpg"), image.YCbCrSubsampleRatio444},
		upload{"4:1:1 from libjpeg", readTestdata(t, "cost-411.jpg"), image.YCbCrSubsampleRatio411},
	)
	for _, u := range uploads {
		// The size is the file's own, and so is the subsampling, which has
		// to be the one the upload is listed at for the test to mean anything.
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(u.data))
		if err != nil {
			t.Fatalf("%s: %v", u.name, err)
		}
		if f, _ := readJPEGFrame(u.data); f.subsampling() != u.ratio {
			t.Fatalf("%s is %v, not %v", u.name, f.subsampling(), u.ratio)
		}
		for _, o := range []int{1, 3, 6} {
			// Each orientation is given in an EXIF segment in front of any
			// the file has, which is the one read: cost-prog444.jpg carries
			// an orientation of 6 of its own, which would otherwise turn it
			// at "1".
			data := withSegments(u.data, exifSegment(orientationTIFF(binary.BigEndian, uint16(o))))
			if got := jpegOrientation(data); got != o {
				t.Fatalf("%s: orientation %d read as %d", u.name, o, got)
			}
			want, ok := turnedSubsampling(u.ratio, o, cfg.Width, cfg.Height)
			if !ok {
				want = image.YCbCrSubsampleRatio444
			}
			p := process(t, data)
			f, framed := readJPEGFrame(p.Data)
			if !framed || f.comps != 3 {
				t.Fatalf("%s, orientation %d: stored file has no colour frame", u.name, o)
			}
			if got := f.subsampling(); got != want {
				t.Errorf("%s, orientation %d: stored at %v, want %v", u.name, o, got, want)
			}
		}
	}
}

// TestColourDetailIsKept is what keeping the subsampling is for: coloured
// strokes a pixel wide, saved at 4:4:4 the way an editor saves a screenshot
// or a diagram at high quality, come back in their own colours. Stored at
// 4:2:0, each stroke was averaged with the background beside it.
func TestColourDetailIsKept(t *testing.T) {
	m := image.NewRGBA(image.Rect(0, 0, 96, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 96; x++ {
			c := color.RGBA{10, 50, 190, 255}
			if x%5 == 2 || y%7 == 3 {
				c = color.RGBA{235, 25, 25, 255}
			}
			m.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpegenc.Encode(&buf, m, &jpegenc.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	upload := buf.Bytes()
	if f, _ := readJPEGFrame(upload); f.subsampling() != image.YCbCrSubsampleRatio444 {
		t.Fatalf("fixture is %v, not 4:4:4", f.subsampling())
	}
	before, err := jpeg.Decode(bytes.NewReader(upload))
	if err != nil {
		t.Fatal(err)
	}
	after := decodeStored(t, process(t, upload).Data)
	worst := 0
	for y := 0; y < 64; y++ {
		for x := 0; x < 96; x++ {
			r1, _, b1, _ := before.At(x, y).RGBA()
			r2, _, b2, _ := after.At(x, y).RGBA()
			for _, d := range []int{int(r1>>8) - int(r2>>8), int(b1>>8) - int(b2>>8)} {
				worst = max(worst, d, -d)
			}
		}
	}
	if worst > 48 {
		t.Errorf("a stroke's red or blue moved by %d levels", worst)
	}
}

// The header walk reads an upload's bytes in place, and choosing a quality
// from what it read allocates nothing either.
func TestJPEGQualityAllocatesNothing(t *testing.T) {
	data := readTestdata(t, "photo-q30.jpg")
	if allocs := testing.AllocsPerRun(10, func() { qualityOf(data, 6) }); allocs != 0 {
		t.Errorf("reading the tables and choosing a quality made %v allocations", allocs)
	}
}

// Fuzzing the walk and the choice with headers as seeds: whatever the bytes,
// a quality from 1 to storedJPEGQuality comes back, and no panic.
func FuzzJPEGQuality(f *testing.F) {
	q50 := jpegenc.QuantTables(50)
	luma, chroma := make([]uint16, 64), make([]uint16, 64)
	for k := range luma {
		luma[k], chroma[k] = uint16(q50[0][k]), uint16(q50[1][k])
	}
	f.Add(tablesHeader(3, [4]byte{0, 1, 1}, false, append(dqtPayload(0, false, luma), dqtPayload(1, true, chroma)...)), 6)
	f.Add(tablesHeader(1, [4]byte{}, true, dqtPayload(0, false, luma)), 1)
	photo := readTestdata(f, "photo-q30.jpg")
	f.Add(photo[:min(len(photo), 1024)], 1)
	f.Fuzz(func(t *testing.T, data []byte, orientation int) {
		if q := qualityOf(data, orientation); q < 1 || q > storedJPEGQuality {
			t.Fatalf("quality %d, outside 1–%d", q, storedJPEGQuality)
		}
	})
}
