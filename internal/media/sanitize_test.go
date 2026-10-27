package media

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/webp"
)

/* Helpers ------------------------------------------------------------------ */

func process(t *testing.T, data []byte) Processed {
	t.Helper()
	got, err := Process(context.Background(), data)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return got
}

// requireCarries is the positive control: the fixture really does hold every
// needle, so their absence afterwards means something.
func requireCarries(t *testing.T, data []byte, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !bytes.Contains(data, []byte(n)) {
			t.Fatalf("fixture does not carry %q, so the test would prove nothing", n)
		}
	}
}

func requireFree(t *testing.T, data []byte, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if bytes.Contains(data, []byte(n)) {
			t.Errorf("stored file still carries %q", n)
		}
	}
}

// jpegMarkers lists the markers of a JPEG up to and including its first start
// of scan.
func jpegMarkers(t *testing.T, data []byte) []byte {
	t.Helper()
	var markers []byte
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			t.Fatalf("no marker at offset %d", i)
		}
		m := data[i+1]
		markers = append(markers, m)
		if m == 0xDA {
			return markers
		}
		i += 2 + (int(data[i+2])<<8 | int(data[i+3]))
	}
	return markers
}

func near(a, b color.Color) bool {
	r1, g1, b1, _ := a.RGBA()
	r2, g2, b2, _ := b.RGBA()
	d := func(x, y uint32) bool {
		x, y = x>>8, y>>8
		return x+24 >= y && y+24 >= x
	}
	return d(r1, r2) && d(g1, g2) && d(b1, b2)
}

// cornerColour samples a picture just inside one of its corners.
func cornerColour(img image.Image, corner string) color.Color {
	b := img.Bounds()
	const inset = 3
	x, y := b.Min.X+inset, b.Min.Y+inset
	if strings.Contains(corner, "R") {
		x = b.Max.X - 1 - inset
	}
	if strings.HasPrefix(corner, "B") {
		y = b.Max.Y - 1 - inset
	}
	return img.At(x, y)
}

func decodeStored(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode stored file: %v", err)
	}
	return img
}

// narrowBudget replaces the processing budget with one of n bytes for the
// rest of the test.
func narrowBudget(t *testing.T, n int64) *budget {
	t.Helper()
	old := processing
	processing = newBudget(n)
	t.Cleanup(func() { processing = old })
	return processing
}

/* JPEG --------------------------------------------------------------------- */

// The fixture is a phone photo carrying everything a phone and an editor put
// in one. What is stored has none of it, is upright, and says so in its
// dimensions.
func TestJPEGIsStoredWithoutMetadata(t *testing.T) {
	upload := cameraJPEG(t, 64, 32)
	needles := []string{"Exif", needleGPS, needleDatum, needleCamera, needleXMP, needleXMPGPS,
		needlePS, "8BIM", needleIPTC, "ICC_PROFILE", needleICC, needleComment}
	requireCarries(t, upload, needles...)

	got := process(t, upload)
	requireFree(t, got.Data, needles...)
	for _, m := range jpegMarkers(t, got.Data) {
		if (m >= 0xE0 && m <= 0xEF) || m == 0xFE {
			t.Errorf("stored JPEG has an APP%d/COM segment (marker %#x)", m-0xE0, m)
		}
	}

	// Stored on its side with orientation 6; kept upright, 64×32.
	if got.Width != 64 || got.Height != 32 {
		t.Errorf("dimensions = %dx%d, want the upright 64x32", got.Width, got.Height)
	}
	stored := decodeStored(t, got.Data)
	if b := stored.Bounds(); b.Dx() != 64 || b.Dy() != 32 {
		t.Fatalf("stored file is %dx%d, want 64x32", b.Dx(), b.Dy())
	}
	thumb := decodeStored(t, got.Thumb.Data)
	if got.Thumb.Width != 64 || got.Thumb.Height != 32 {
		t.Errorf("thumbnail = %dx%d, want 64x32", got.Thumb.Width, got.Thumb.Height)
	}
	for corner, want := range map[string]color.RGBA{"TL": quadTL, "TR": quadTR, "BL": quadBL, "BR": quadBR} {
		if c := cornerColour(stored, corner); !near(c, want) {
			t.Errorf("stored %s corner = %v, want %v: the picture is not upright", corner, c, want)
		}
		if c := cornerColour(thumb, corner); !near(c, want) {
			t.Errorf("thumbnail %s corner = %v, want %v: the thumbnail is not upright", corner, c, want)
		}
	}
}

// Every orientation, read from EXIF in either byte order, turns the stored
// pixels upright. The table is the TIFF definition, not this package's code:
// where the stored picture's first pixel, the end of its first row, and the
// start of its last row appear once it is viewed correctly. The Exif segment
// is read wherever the decoder and a browser find it: straight after the SOI
// marker, and after bytes between segments that both skip — an extraneous
// byte, or an 0xFF00 pair — where a browser turns the picture as well.
func TestEveryOrientationIsApplied(t *testing.T) {
	const w, h = 64, 32
	visual := map[int][3]string{
		1: {"TL", "TR", "BL"},
		2: {"TR", "TL", "BR"},
		3: {"BR", "BL", "TR"},
		4: {"BL", "BR", "TL"},
		5: {"TL", "BL", "TR"},
		6: {"TR", "BR", "TL"},
		7: {"BR", "TR", "BL"},
		8: {"BL", "TL", "BR"},
	}
	colour := quadrants(w, h)
	gray := image.NewGray(colour.Bounds())
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			gray.Set(x, y, colour.At(x, y))
		}
	}
	pictures := map[string]image.Image{"colour": colour, "grayscale": gray}
	// What comes before the Exif segment. A comment segment first keeps the
	// file starting as a JPEG does (DetectFormat), so the skipped bytes sit
	// between two segments.
	comment := jpegSegment(0xFE, []byte("before"))
	placements := []struct {
		name   string
		before [][]byte
	}{
		{"after SOI", nil},
		{"after an extraneous byte", [][]byte{comment, {0x00}}},
		{"after an 0xFF00 pair", [][]byte{comment, {0xFF, 0x00}}},
	}

	for _, place := range placements {
		for name, stored := range pictures {
			name := name + " " + place.name
			for o := 1; o <= 8; o++ {
				order := tiffOrder(binary.LittleEndian)
				if o%2 == 0 {
					order = binary.BigEndian
				}
				segments := append(append([][]byte(nil), place.before...), exifSegment(orientationTIFF(order, uint16(o))))
				upload := withSegments(encodeJPEGImage(t, stored), segments...)
				if got := jpegOrientation(upload); got != o {
					t.Fatalf("%s: jpegOrientation = %d, want %d", name, got, o)
				}
				got := process(t, upload)
				wantW, wantH := w, h
				if o >= 5 {
					wantW, wantH = h, w
				}
				if got.Width != wantW || got.Height != wantH {
					t.Errorf("%s orientation %d: dimensions = %dx%d, want %dx%d", name, o, got.Width, got.Height, wantW, wantH)
				}
				out := decodeStored(t, got.Data)
				if b := out.Bounds(); b.Dx() != wantW || b.Dy() != wantH {
					t.Fatalf("%s orientation %d: stored file is %dx%d, want %dx%d", name, o, b.Dx(), b.Dy(), wantW, wantH)
				}
				for i, want := range []color.Color{
					stored.At(0, 0), stored.At(w-1, 0), stored.At(0, h-1),
				} {
					if c := cornerColour(out, visual[o][i]); !near(c, want) {
						t.Errorf("%s orientation %d: %s corner = %v, want %v", name, o, visual[o][i], c, want)
					}
				}
			}
		}
	}
}

// Turning a YCbCr picture keeps every pixel's colour as a browser shows it,
// whatever its subsampling and size. Where the chroma samples line up with
// the turned picture, the planes are turned as they are and keep their
// subsampling, every sample exactly. An odd side mirrored at 4:2:0, a side
// that is not a multiple of four mirrored at 4:1:1, and 4:1:1 or 4:1:0 on its
// side do not line up, and are turned a pixel at a time into 4:4:4, where
// before the colours along the side shifted a pixel: each pixel's luma
// exactly, and its chroma widened as libjpeg-turbo widens it for display
// (libjpegUpsampled), not the sample it lies in, which at a sharp colour edge
// is far from what the poster saw.
func TestOrientTurnsEverySubsamplingAsShown(t *testing.T) {
	ratios := []image.YCbCrSubsampleRatio{
		image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio422, image.YCbCrSubsampleRatio420,
		image.YCbCrSubsampleRatio440, image.YCbCrSubsampleRatio411, image.YCbCrSubsampleRatio410,
	}
	turned := map[image.YCbCrSubsampleRatio]image.YCbCrSubsampleRatio{
		image.YCbCrSubsampleRatio444: image.YCbCrSubsampleRatio444,
		image.YCbCrSubsampleRatio422: image.YCbCrSubsampleRatio440,
		image.YCbCrSubsampleRatio420: image.YCbCrSubsampleRatio420,
		image.YCbCrSubsampleRatio440: image.YCbCrSubsampleRatio422,
	}
	rng := rand.New(rand.NewSource(1))
	blended := 0 // widened pixels whose chroma is not the sample they lie in
	sizes := []image.Point{{8, 12}, {7, 5}, {5, 7}, {9, 6}, {6, 9}, {10, 13}, {1, 1}, {3, 2}, {4, 3}, {17, 9}, {2, 7}}
	for _, size := range sizes {
		w, h := size.X, size.Y
		for _, ratio := range ratios {
			src := image.NewYCbCr(image.Rect(0, 0, w, h), ratio)
			rng.Read(src.Y)
			rng.Read(src.Cb)
			rng.Read(src.Cr)
			shownCb := libjpegUpsampled(src.Cb, src.CStride, ratio, w, h)
			shownCr := libjpegUpsampled(src.Cr, src.CStride, ratio, w, h)
			fx, fy := subsampling(ratio)
			for o := 1; o <= 8; o++ {
				dst, ok := orient(src, o).(*image.YCbCr)
				if !ok {
					t.Fatalf("%dx%d %v orientation %d: turned into %T, want YCbCr", w, h, ratio, o, orient(src, o))
				}
				mirrorsX := o == 2 || o == 3 || o == 7 || o == 8
				mirrorsY := o == 3 || o == 4 || o == 6 || o == 7
				want := ratio
				switch {
				case o == 1:
				case mirrorsX && w%fx != 0, mirrorsY && h%fy != 0:
					want = image.YCbCrSubsampleRatio444
				case o >= 5:
					var kept bool
					if want, kept = turned[ratio]; !kept {
						want = image.YCbCrSubsampleRatio444
					}
				}
				if dst.SubsampleRatio != want {
					t.Fatalf("%dx%d %v orientation %d: turned at %v, want %v", w, h, ratio, o, dst.SubsampleRatio, want)
				}
				widened := want != ratio && want == image.YCbCrSubsampleRatio444
				for y := 0; y < h; y++ {
					for x := 0; x < w; x++ {
						dx, dy := orientedPoint(o, x, y, w, h)
						want := src.YCbCrAt(x, y)
						if widened {
							if shownCb[y][x] != want.Cb || shownCr[y][x] != want.Cr {
								blended++
							}
							want.Cb, want.Cr = shownCb[y][x], shownCr[y][x]
						}
						if got := dst.YCbCrAt(dx, dy); got != want {
							t.Fatalf("%dx%d %v orientation %d: (%d,%d) landed as %v, want %v", w, h, ratio, o, x, y, got, want)
						}
					}
				}
			}
		}
	}
	if blended == 0 {
		t.Fatal("no widened pixel's chroma differed from the sample it lies in: the cases test nothing")
	}
}

// libjpegUpsampled is one chroma plane of a w×h JPEG at ratio, held with
// stride stride, widened to a sample a pixel as libjpeg-turbo widens it with
// do_fancy_upsampling on, its default and Chromium's: jdsample.c's C methods
// transcribed (the SIMD ones compute the same), h2v2_fancy_upsample and
// h2v1_fancy_upsample when the plane is more than two samples wide and the
// box filters h2v2_upsample and h2v1_upsample when not, h1v2_fancy_upsample,
// and int_upsample's replication for 4:1:1 and 4:1:0, with the context rows
// jdmainct.c supplies around the picture: the first sample row again above
// it and the last again below.
func libjpegUpsampled(plane []uint8, stride int, ratio image.YCbCrSubsampleRatio, w, h int) [][]uint8 {
	fx, fy := subsampling(ratio)
	cw, ch := chromaSize(ratio, w, h)
	row := func(i int) []uint8 {
		i = max(0, min(i, ch-1))
		return plane[i*stride : i*stride+cw]
	}
	var shown [][]uint8
	for in := 0; in < ch; in++ {
		for v := 0; v < fy; v++ {
			near, far, bias := row(in), row(in-1), 1 // the next nearest row is the one above
			if v == 1 {
				far, bias = row(in+1), 2 // and for the second output row, the one below
			}
			var out []uint8
			switch {
			case fx == 2 && fy == 2 && cw > 2: // h2v2_fancy_upsample
				this := 3*int(near[0]) + int(far[0])
				next := 3*int(near[1]) + int(far[1])
				out = append(out, uint8((this*4+8)>>4), uint8((this*3+next+7)>>4))
				last := this
				this = next
				for c := 2; c < cw; c++ {
					next = 3*int(near[c]) + int(far[c])
					out = append(out, uint8((this*3+last+8)>>4), uint8((this*3+next+7)>>4))
					last, this = this, next
				}
				out = append(out, uint8((this*3+last+8)>>4), uint8((this*4+7)>>4))
			case fx == 2 && fy == 1 && cw > 2: // h2v1_fancy_upsample
				out = append(out, near[0], uint8((3*int(near[0])+int(near[1])+2)>>2))
				for c := 1; c < cw-1; c++ {
					out = append(out, uint8((3*int(near[c])+int(near[c-1])+1)>>2), uint8((3*int(near[c])+int(near[c+1])+2)>>2))
				}
				out = append(out, uint8((3*int(near[cw-1])+int(near[cw-2])+1)>>2), near[cw-1])
			case fx == 1 && fy == 2: // h1v2_fancy_upsample
				for c := 0; c < cw; c++ {
					out = append(out, uint8((3*int(near[c])+int(far[c])+bias)>>2))
				}
			default: // h2v2_upsample, h2v1_upsample, int_upsample: each sample repeated
				for c := 0; c < cw; c++ {
					for i := 0; i < fx; i++ {
						out = append(out, near[c])
					}
				}
			}
			shown = append(shown, out[:w])
		}
	}
	return shown[:h]
}

// Every other kind of picture a JPEG or a PNG decodes to is turned into a
// picture of the same kind with every pixel's bytes exactly as they were, a
// palette's included, wherever the picture starts.
func TestOrientKeepsEveryPixelKindExact(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	r := image.Rect(0, 0, 9, 6)
	pal := color.Palette{color.NRGBA{1, 2, 3, 4}, color.NRGBA{200, 100, 50, 255}, color.RGBA{}}
	pictures := map[string]image.Image{
		"Gray": image.NewGray(r), "Gray16": image.NewGray16(r), "RGBA": image.NewRGBA(r),
		"NRGBA": image.NewNRGBA(r), "RGBA64": image.NewRGBA64(r), "NRGBA64": image.NewNRGBA64(r),
		"Paletted": image.NewPaletted(r, pal),
	}
	for name, m := range pictures {
		pix := reflect.ValueOf(m).Elem().FieldByName("Pix").Bytes()
		rng.Read(pix)
		if p, ok := m.(*image.Paletted); ok {
			for i := range p.Pix {
				p.Pix[i] %= uint8(len(pal))
			}
		}
		sub := m.(interface {
			SubImage(image.Rectangle) image.Image
		}).SubImage(image.Rect(2, 1, 9, 6))
		for _, src := range []image.Image{m, sub} {
			b := src.Bounds()
			w, h := b.Dx(), b.Dy()
			for o := 2; o <= 8; o++ {
				dst := orient(src, o)
				if reflect.TypeOf(dst) != reflect.TypeOf(src) {
					t.Fatalf("%s orientation %d: turned into %T", name, o, dst)
				}
				for y := 0; y < h; y++ {
					for x := 0; x < w; x++ {
						dx, dy := orientedPoint(o, x, y, w, h)
						if got, want := dst.At(dx, dy), src.At(b.Min.X+x, b.Min.Y+y); got != want {
							t.Fatalf("%s %v orientation %d: (%d,%d) landed as %v, want %v", name, b, o, x, y, got, want)
						}
					}
				}
			}
		}
	}
}

// The orientation reader works on whatever an upload holds. Broken EXIF is
// read for what Chromium reads of it (TestOrientationIsReadAsChromiumReadsIt)
// and as upright where it cannot be read, never as a panic, a hang or an
// allocation, and the decoder alone decides whether the file is an image: one
// whose segment runs off the end is refused as corrupt, one whose EXIF is
// merely nonsense is stored upright, with the nonsense gone.
func TestHostileEXIF(t *testing.T) {
	base := encodeJPEGImage(t, quadrants(64, 32))
	le := binary.LittleEndian

	header := tiffHeader(le)
	hugeCount := append(append([]byte(nil), header...), 0xFF, 0xFF) // 65535 entries claimed
	hugeCount = append(hugeCount, appendIFD(nil, le, []tiffEntry{shortEntry(le, 0x0112, 6)}, 0)[2:]...)

	// IFD0 says it is followed by itself, and that the Exif sub-IFD is
	// itself too. The tag is still read, and nothing is followed.
	loop := appendIFD(append([]byte(nil), header...), le, []tiffEntry{
		shortEntry(le, 0x0112, 6),
		longEntry(le, 0x8769, 8),
	}, 8)

	outside := append([]byte(nil), header...)
	le.PutUint32(outside[4:8], 0xFFFFFFF0)

	// IFD0 names itself as its Exif sub-IFD, and the sub-IFD, read once,
	// names itself again; or names a sub-IFD past the end of the segment.
	exifLoop := appendIFD(append([]byte(nil), header...), le, []tiffEntry{longEntry(le, 0x8769, 8)}, 8)
	exifOutside := appendIFD(append([]byte(nil), header...), le, []tiffEntry{longEntry(le, 0x8769, 0xFFFFFFF0)}, 0)

	wrongType := appendIFD(append([]byte(nil), header...), le, []tiffEntry{longEntry(le, 0x0112, 6)}, 0)
	outOfRange := orientationTIFF(le, 9)

	cases := []struct {
		name    string
		upload  []byte
		want    int  // orientation read
		corrupt bool // whether the decoder refuses the file
	}{
		{"truncated TIFF", withSegments(base, exifSegment(header[:6])), 1, false},
		{"IFD with no room for its count", withSegments(base, exifSegment(header)), 1, false},
		{"IFD offset outside the segment", withSegments(base, exifSegment(outside)), 1, false},
		// Read for the one entry it holds, as Chromium reads it.
		{"huge entry count", withSegments(base, exifSegment(hugeCount)), 6, false},
		{"IFD offsets in a loop", withSegments(base, exifSegment(loop)), 6, false},
		{"Exif sub-IFD offsets in a loop", withSegments(base, exifSegment(exifLoop)), 1, false},
		{"Exif sub-IFD offset outside the segment", withSegments(base, exifSegment(exifOutside)), 1, false},
		{"orientation of the wrong type", withSegments(base, exifSegment(wrongType)), 1, false},
		{"orientation out of range", withSegments(base, exifSegment(outOfRange)), 1, false},
		{"bad byte order", withSegments(base, exifSegment([]byte("XX\x00\x2a\x00\x00\x00\x08"))), 1, false},
		{"Exif and nothing else", withSegments(base, jpegSegment(0xE1, []byte("Exif\x00\x00"))), 1, false},
		// The segment claims 200 bytes and the file ends 50 bytes in.
		{"APP1 running off the end", append(append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0xC8}, "Exif\x00\x00II*\x00"...), make([]byte, 40)...), 1, true},
		{"segment length below two", withSegments(base, []byte{0xFF, 0xE1, 0x00, 0x01}), 1, true},
		{"not a JPEG", []byte("Exif"), 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jpegOrientation(tc.upload); got != tc.want {
				t.Errorf("jpegOrientation = %d, want %d", got, tc.want)
			}
			if allocs := testing.AllocsPerRun(10, func() { jpegOrientation(tc.upload) }); allocs != 0 {
				t.Errorf("jpegOrientation allocated %v times, want none", allocs)
			}
			got, err := Process(context.Background(), tc.upload)
			switch {
			case tc.corrupt && !errors.Is(err, ErrCorruptImage) && !errors.Is(err, ErrUnsupportedFormat):
				t.Fatalf("Process error = %v, want the file refused", err)
			case tc.corrupt:
				return
			case err != nil:
				t.Fatalf("Process: %v; broken EXIF should not cost the picture", err)
			}
			wantW, wantH := 64, 32
			if tc.want >= 5 {
				wantW, wantH = 32, 64
			}
			if got.Width != wantW || got.Height != wantH {
				t.Errorf("dimensions = %dx%d, want %dx%d", got.Width, got.Height, wantW, wantH)
			}
			requireFree(t, got.Data, "Exif")
		})
	}
}

// The orientation is read from EXIF as Chromium reads it, so that the stored
// picture is turned as the poster's browser showed it, in a JPEG's Exif
// segment and a PNG's eXIf chunk alike: IFD0's entries are read in order, and
// the Exif sub-IFD's where IFD0's entry naming it, a single LONG, stands; the
// first Orientation entry that is a single SHORT from 1 to 8 counts, and one
// that is not is passed over rather than read as upright. An IFD that claims
// more entries than it holds is read for the ones it holds, IFD0 is read at
// any offset, overlapping the header too, every Exif sub-IFD IFD0 names is
// followed, in turn, and nothing else is: not a sub-IFD named by a SHORT or by
// the sub-IFD, nor the next IFD. Each want is what Chromium 141 did with both
// files, loaded as images: turned them by it, or, for 1, left them as they
// were. Past maxEXIFEntries entries the reader stops where Chromium does not
// (TestEXIFReadIsBounded).
func TestOrientationIsReadAsChromiumReadsIt(t *testing.T) {
	le := binary.LittleEndian
	o := func(v uint16) tiffEntry { return shortEntry(le, 0x0112, v) }
	ifd0 := func(entries ...tiffEntry) []byte { return appendIFD(tiffHeader(le), le, entries, 0) }
	// claiming says an IFD0 holds count entries, whatever it holds.
	claiming := func(count uint16, tiff []byte) []byte {
		out := append([]byte(nil), tiff...)
		le.PutUint16(out[8:], count)
		return out
	}
	twoShorts := tiffEntry{tag: 0x0112, typ: tiffTypeShort, count: 2, value: le.AppendUint16(le.AppendUint16(nil, 6), 6)}
	// ifds lays IFDs out one after another from offset 8, and after is where
	// the one after IFDs of so many entries each starts. exif names the Exif
	// sub-IFD at an offset.
	ifds := func(lists ...[]tiffEntry) []byte {
		b := tiffHeader(le)
		for _, l := range lists {
			b = appendIFD(b, le, l, 0)
		}
		return b
	}
	after := func(entries ...int) uint32 {
		at := 8
		for _, n := range entries {
			at += 2 + 12*n + 4
		}
		return uint32(at)
	}
	exif := func(at uint32) tiffEntry { return longEntry(le, 0x8769, at) }
	// other is an entry of no interest to the reader: the image width.
	other := shortEntry(le, 0x0100, 64)
	// nextIFD is IFD0 holding the camera's make, followed by IFD1 holding
	// the orientation, as the next IFD in the chain.
	nextIFD := appendIFD(appendIFD(tiffHeader(le), le, []tiffEntry{asciiEntry(0x010F, "A")}, after(1)), le, []tiffEntry{o(6)}, 0)
	// at4 and at0 put IFD0 at offsets 4 and 0, so that its count is read from
	// the header (4, from the offset itself; 0x4949, from "II") and its
	// entries start inside it; the orientation is the second entry.
	entry := appendIFD(nil, le, []tiffEntry{o(6)}, 0)[2:14]
	at4 := append(append(append([]byte("II*\x00\x04\x00\x00\x00"), make([]byte, 10)...), entry...), 0, 0, 0, 0)
	at0 := append(append([]byte("II*\x00\x00\x00\x00\x00"), make([]byte, 6)...), entry...)
	cases := []struct {
		name string
		tiff []byte
		want int
	}{
		{"one orientation", ifd0(o(6)), 6},
		{"five entries claimed, one held and no next offset", claiming(5, ifd0(o(6))[:8+2+12]), 6},
		{"65535 entries claimed, one held", claiming(65535, ifd0(o(6))), 6},
		{"orientation 9, then 6", ifd0(o(9), o(6)), 6},
		{"orientation 0, then 6", ifd0(o(0), o(6)), 6},
		{"orientation as a LONG, then as a SHORT", ifd0(longEntry(le, 0x0112, 6), o(6)), 6},
		{"orientation of two SHORTs, then of one", ifd0(twoShorts, o(6)), 6},
		{"orientation 1, then 6", ifd0(o(1), o(6)), 1},
		{"orientation 9 alone", ifd0(o(9)), 1},
		{"orientation as a LONG alone", ifd0(longEntry(le, 0x0112, 6)), 1},
		{"orientation of two SHORTs alone", ifd0(twoShorts), 1},
		{"IFD0 at offset 4", at4, 6},
		{"IFD0 at offset 0", at0, 6},
		{"orientation in the Exif sub-IFD alone", ifds([]tiffEntry{exif(after(1))}, []tiffEntry{o(6)}), 6},
		{"the Exif sub-IFD's orientation, then IFD0's", ifds([]tiffEntry{exif(after(2)), o(1)}, []tiffEntry{o(6)}), 6},
		{"IFD0's orientation, then the Exif sub-IFD's", ifds([]tiffEntry{o(1), exif(after(2))}, []tiffEntry{o(6)}), 1},
		{"orientation 9, then the Exif sub-IFD's", ifds([]tiffEntry{o(9), exif(after(2))}, []tiffEntry{o(6)}), 6},
		{"the Exif sub-IFD's orientation 9, then its 6", ifds([]tiffEntry{exif(after(1))}, []tiffEntry{o(9), o(6)}), 6},
		{"an Exif sub-IFD named by a SHORT", ifds([]tiffEntry{shortEntry(le, 0x8769, uint16(after(1)))}, []tiffEntry{o(6)}), 1},
		{"an Exif sub-IFD named by the sub-IFD", ifds([]tiffEntry{exif(after(1))}, []tiffEntry{exif(after(1, 1))}, []tiffEntry{o(6)}), 1},
		{"orientation in the next IFD", nextIFD, 1},
		{"an Exif sub-IFD that is IFD0, then IFD0's orientation", ifd0(exif(8), o(6)), 6},
		{"two Exif sub-IFDs, the second's orientation", ifds([]tiffEntry{exif(after(2)), exif(after(2, 1))}, []tiffEntry{other}, []tiffEntry{o(6)}), 6},
		{"two Exif sub-IFDs, the first's orientation 9, the second's 6", ifds([]tiffEntry{exif(after(2)), exif(after(2, 1))}, []tiffEntry{o(9)}, []tiffEntry{o(6)}), 6},
		{"an empty Exif sub-IFD, then one with an orientation", ifds([]tiffEntry{exif(after(2)), exif(after(2, 0))}, []tiffEntry{}, []tiffEntry{o(6)}), 6},
		{"an Exif sub-IFD past the segment, then one inside it", ifds([]tiffEntry{exif(0xFFFFFFF0), exif(after(2))}, []tiffEntry{o(6)}), 6},
		{"an Exif sub-IFD that is IFD0, then another", ifds([]tiffEntry{exif(8), exif(after(2))}, []tiffEntry{o(6)}), 6},
		{"an Exif sub-IFD named by a SHORT, then by a LONG", ifds([]tiffEntry{shortEntry(le, 0x8769, uint16(after(2))), exif(after(2))}, []tiffEntry{o(6)}), 6},
		{"three Exif sub-IFDs, the third's orientation", ifds([]tiffEntry{exif(after(3)), exif(after(3, 1)), exif(after(3, 1, 1))}, []tiffEntry{other}, []tiffEntry{other}, []tiffEntry{o(6)}), 6},
	}
	pic := quadrants(64, 32)
	jpg, pngFile := encodeJPEGImage(t, pic), pngOf(t, pic)
	for _, c := range cases {
		jpgUpload := withSegments(jpg, exifSegment(c.tiff))
		pngUpload := withPNGChunks(pngFile, pngChunk("eXIf", c.tiff))
		if got := jpegOrientation(jpgUpload); got != c.want {
			t.Errorf("%s: jpegOrientation = %d, want %d", c.name, got, c.want)
		}
		if got := pngOrientation(pngUpload); got != c.want {
			t.Errorf("%s: pngOrientation = %d, want %d", c.name, got, c.want)
		}
		wantW, wantH := 64, 32
		if c.want >= 5 {
			wantW, wantH = 32, 64
		}
		for format, upload := range map[string][]byte{"JPEG": jpgUpload, "PNG": pngUpload} {
			if got := process(t, upload); got.Width != wantW || got.Height != wantH {
				t.Errorf("%s: the %s is stored %dx%d, want %dx%d", c.name, format, got.Width, got.Height, wantW, wantH)
			}
		}
	}
}

// Chromium reads the orientation from the first EXIF block in a file that
// holds anything: it passes over a JPEG's Exif segment of the six-byte header
// alone, whatever its sixth byte, and a PNG's empty eXIf chunk, for a later
// one, but takes the first that holds a byte more, even one too short to read,
// and reads no other. Each want is what Chromium 141 did with the file.
func TestEmptyEXIFBlocksArePassedOver(t *testing.T) {
	le := binary.LittleEndian
	o6, o3 := orientationTIFF(le, 6), orientationTIFF(le, 3)
	app1 := func(payload string) []byte { return jpegSegment(0xE1, []byte(payload)) }
	exif := func(tiff []byte) []byte { return pngChunk("eXIf", tiff) }
	pic := quadrants(64, 32)
	jpg, pngFile := encodeJPEGImage(t, pic), pngOf(t, pic)
	cases := []struct {
		name   string
		upload []byte
		want   int
	}{
		{"JPEG: an Exif header alone, then orientation 6", withSegments(jpg, app1("Exif\x00\x00"), exifSegment(o6)), 6},
		{"JPEG: two Exif headers alone, then orientation 6", withSegments(jpg, app1("Exif\x00\x00"), app1("Exif\x00\x00"), exifSegment(o6)), 6},
		{"JPEG: an Exif header ending in X alone, then orientation 6", withSegments(jpg, app1("Exif\x00X"), exifSegment(o6)), 6},
		{"JPEG: orientation 6 after an Exif header ending in X", withSegments(jpg, app1("Exif\x00X"+string(o6))), 6},
		{"JPEG: an Exif block of one byte, then orientation 6", withSegments(jpg, app1("Exif\x00\x00I"), exifSegment(o6)), 1},
		{"JPEG: an Exif block of one byte after an X, then orientation 6", withSegments(jpg, app1("Exif\x00XI"), exifSegment(o6)), 1},
		{"JPEG: a TIFF header alone after an X, then orientation 6", withSegments(jpg, app1("Exif\x00XII*\x00"), exifSegment(o6)), 1},
		{"PNG: an empty eXIf, then orientation 6", withPNGChunks(pngFile, exif(nil), exif(o6)), 6},
		{"PNG: two empty eXIfs, then orientation 6", withPNGChunks(pngFile, exif(nil), exif(nil), exif(o6)), 6},
		{"PNG: an eXIf of one byte, then orientation 6", withPNGChunks(pngFile, exif([]byte("I")), exif(o6)), 1},
		{"PNG: orientation 6, then 3", withPNGChunks(pngFile, exif(o6), exif(o3)), 6},
	}
	for _, c := range cases {
		read := jpegOrientation
		if strings.HasPrefix(c.name, "PNG") {
			read = pngOrientation
		}
		if got := read(c.upload); got != c.want {
			t.Errorf("%s: orientation read = %d, want %d", c.name, got, c.want)
		}
		wantW, wantH := 64, 32
		if c.want >= 5 {
			wantW, wantH = 32, 64
		}
		if got := process(t, c.upload); got.Width != wantW || got.Height != wantH {
			t.Errorf("%s: stored %dx%d, want %dx%d", c.name, got.Width, got.Height, wantW, wantH)
		}
	}
}

// Chromium follows every Exif sub-IFD entry in IFD0, each into a scan of
// the sub-IFD it names, so an IFD0 of 65535 entries that each name IFD0 is
// 4.3 billion entries to read: a PNG of 768 KB that took ~26-27 s of CPU
// before the reader was bounded, and Inspect reads its orientation more than
// once, all before any cooldown. The reader stops after maxEXIFEntries
// entries in all, the 131070th read and the 131071st not, which leaves the
// last three files here upright where Chromium 141 turned each of them; and
// it still reads every entry of IFD0 and of one sub-IFD however many each
// holds.
func TestEXIFReadIsBounded(t *testing.T) {
	le := binary.LittleEndian
	o6 := shortEntry(le, 0x0112, 6)
	other := shortEntry(le, 0x0100, 64)
	exif := func(at uint32) tiffEntry { return longEntry(le, 0x8769, at) }
	repeat := func(e tiffEntry, n int) []tiffEntry {
		out := make([]tiffEntry, n)
		for i := range out {
			out[i] = e
		}
		return out
	}
	// subIFDTwice is IFD0 holding lead entries of no interest, then two
	// entries naming one Exif sub-IFD of n such entries, then orientation 6:
	// lead + n + 1 + n + 1 entries to read to reach the orientation.
	subIFDTwice := func(lead, n int) []byte {
		sub := uint32(8 + 2 + 12*(lead+3) + 4)
		entries := append(repeat(other, lead), exif(sub), exif(sub), o6)
		b := appendIFD(tiffHeader(le), le, entries, 0)
		return appendIFD(b, le, repeat(other, n), 0)
	}
	// selfNaming is IFD0 of n entries naming IFD0 as the Exif sub-IFD, then,
	// if tail, one naming a sub-IFD that holds orientation 6.
	selfNaming := func(n int, tail bool) []byte {
		entries := repeat(exif(8), n)
		if !tail {
			return appendIFD(tiffHeader(le), le, entries, 0)
		}
		entries = append(entries, exif(uint32(8+2+12*(n+1)+4)))
		return appendIFD(appendIFD(tiffHeader(le), le, entries, 0), le, []tiffEntry{o6}, 0)
	}
	if maxEXIFEntries != 131070 {
		t.Fatalf("maxEXIFEntries = %d; the cases below are laid out for 131070", maxEXIFEntries)
	}
	pngFile := pngOf(t, quadrants(64, 32))
	cases := []struct {
		name string
		tiff []byte
		want int
	}{
		// The bound is checked first, so that a reader without it fails here
		// rather than spending minutes on the files below.
		{"the orientation as the 131070th entry read", subIFDTwice(1, 65533), 6},
		{"the orientation as the 131071st entry read", subIFDTwice(2, 65533), 1},
		{"IFD0 of 65535 entries naming IFD0", selfNaming(65535, false), 1},
		{"IFD0 of 65534 entries naming IFD0, then one naming an orientation", selfNaming(65534, true), 1},
	}
	for _, c := range cases {
		upload := withPNGChunks(pngFile, pngChunk("eXIf", c.tiff))
		done := make(chan int, 1)
		go func() { done <- pngOrientation(upload) }()
		select {
		case got := <-done:
			if got != c.want {
				t.Fatalf("%s: pngOrientation = %d, want %d", c.name, got, c.want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: pngOrientation still reading after 10 s", c.name)
		}
		if allocs := testing.AllocsPerRun(3, func() { pngOrientation(upload) }); allocs != 0 {
			t.Errorf("%s: pngOrientation allocated %v times, want none", c.name, allocs)
		}
		wantW, wantH := 64, 32
		if c.want >= 5 {
			wantW, wantH = 32, 64
		}
		if got := process(t, upload); got.Width != wantW || got.Height != wantH {
			t.Errorf("%s: stored %dx%d, want %dx%d", c.name, got.Width, got.Height, wantW, wantH)
		}
	}
}

// Fuzzing the reader with the fixtures as seeds: whatever the bytes, it
// answers with an orientation and does not panic.
func FuzzJPEGOrientation(f *testing.F) {
	f.Add(withSegments([]byte{0xFF, 0xD8, 0xFF, 0xD9}, exifSegment(gpsTIFF(binary.BigEndian, 6))))
	f.Add(withSegments([]byte{0xFF, 0xD8, 0xFF, 0xD9}, exifSegment(orientationTIFF(binary.LittleEndian, 8))))
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0, 0})
	// Exif after bytes the decoder skips between segments.
	comment := jpegSegment(0xFE, []byte("before"))
	f.Add(withSegments([]byte{0xFF, 0xD8, 0xFF, 0xD9}, comment, []byte{0x00}, exifSegment(orientationTIFF(binary.BigEndian, 6))))
	f.Add(withSegments([]byte{0xFF, 0xD8, 0xFF, 0xD9}, comment, []byte{0xFF, 0x00}, exifSegment(orientationTIFF(binary.LittleEndian, 5))))
	f.Fuzz(func(t *testing.T, data []byte) {
		if o := jpegOrientation(data); o < 1 || o > 8 {
			t.Fatalf("jpegOrientation = %d, outside 1–8", o)
		}
	})
}

// A CMYK JPEG is stored as the colours color.CMYK itself reports, converted
// where the decoder left them rather than into a second picture.
func TestCMYKIsConvertedAsTheColourModelDoes(t *testing.T) {
	src, err := jpeg.Decode(bytes.NewReader(readTestdata(t, "cost-cmyk.jpg")))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	cmyk, ok := src.(*image.CMYK)
	if !ok {
		t.Fatalf("the fixture decodes as %T, not CMYK", src)
	}
	b := cmyk.Bounds()
	want := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			want.Set(x, y, cmyk.At(x, y))
		}
	}
	got := rgbaOfCMYK(cmyk)
	if got.Bounds() != b || !bytes.Equal(got.Pix, want.Pix) {
		t.Error("rgbaOfCMYK differs from color.CMYK's own conversion")
	}
	if &got.Pix[0] != &cmyk.Pix[0] {
		t.Error("rgbaOfCMYK allocated a second picture")
	}
}

/* PNG ---------------------------------------------------------------------- */

// A PNG's ancillary chunks are where its text, XMP, EXIF and timestamps live.
// None is stored, and the pixels are kept exactly.
func TestPNGIsStoredWithoutAncillaryChunks(t *testing.T) {
	clean := encodePNG(t, 40, 30)
	upload := withPNGChunks(clean,
		pngChunk("tEXt", []byte("Comment\x00"+needleComment)),
		pngChunk("zTXt", []byte("Software\x00\x00"+needleCamera)), // not real deflate; decoders skip it
		pngChunk("iTXt", []byte("XML:com.adobe.xmp\x00\x00\x00\x00\x00"+needleXMP+" "+xmpPacket())),
		pngChunk("eXIf", gpsTIFF(binary.BigEndian, 1)),
		pngChunk("tIME", []byte{0x07, 0xEA, 10, 13, 9, 30, 0}),
		pngChunk("iCCP", []byte(needleICC+"\x00\x00")),
		pngChunk("prVt", []byte(needleIPTC)), // a private chunk nobody has heard of
	)
	needles := []string{"tEXt", "zTXt", "iTXt", "eXIf", "tIME", "iCCP", "prVt", needleComment,
		needleCamera, needleXMP, needleXMPGPS, needleGPS, needleDatum, needleICC, needleIPTC}
	requireCarries(t, upload, needles...)

	got := process(t, upload)
	requireFree(t, got.Data, needles...)
	for _, typ := range pngChunkTypes(t, got.Data) {
		switch typ {
		case "IHDR", "PLTE", "tRNS", "IDAT", "IEND":
		default:
			t.Errorf("stored PNG has a %s chunk", typ)
		}
	}
	requireSamePixels(t, decodeStored(t, clean), decodeStored(t, got.Data))
}

// Re-encoding keeps a 16-bit PNG at 16 bits, transparency and all, and shows
// the same picture — a partly transparent pixel to the precision its alpha can
// show (reduceHiddenColour), an opaque one to the bit.
func TestPNGKeepsSixteenBitPixels(t *testing.T) {
	src := image.NewNRGBA64(image.Rect(0, 0, 20, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 20; x++ {
			src.SetNRGBA64(x, y, color.NRGBA64{R: uint16(x * 3001), G: uint16(y * 6007), B: 0x1234, A: uint16(0xFFFF - x*1000)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := decodeStored(t, process(t, buf.Bytes()).Data)
	if _, ok := out.(*image.NRGBA64); !ok {
		t.Errorf("stored PNG decodes to %T, want 16 bits a channel with alpha", out)
	}
	requireSameAppearance(t, src, out)
}

// A PNG is stored in the form image/png decodes it to, as sanitizePNG and the
// README say, not always the one it came in: grey of 1, 2 or 4 bits comes
// back at 8, and grey or colour with a transparent colour key (tRNS) comes
// back as colour with an alpha channel, or with none when no pixel matches
// the key, and without the tRNS chunk. 16 bits stay 16. Every pixel keeps
// its value.
func TestPNGIsStoredInTheFormItDecodesTo(t *testing.T) {
	const w, h = 16, 8
	// lowBitGrey packs pixels of value (x+y) mod 2^depth, with a tRNS chunk
	// making 0 transparent if asked: image/png writes no grey below 8 bits.
	lowBitGrey := func(depth byte, key bool) []byte {
		var raw bytes.Buffer
		zw := zlib.NewWriter(&raw)
		perByte := 8 / int(depth)
		for y := 0; y < h; y++ {
			row := make([]byte, 1+(w+perByte-1)/perByte)
			for x := 0; x < w; x++ {
				v := byte((x + y) % (1 << depth))
				row[1+x/perByte] |= v << (8 - int(depth)*(x%perByte+1))
			}
			if _, err := zw.Write(row); err != nil {
				t.Fatalf("compress: %v", err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("compress: %v", err)
		}
		ihdr := binary.BigEndian.AppendUint32(nil, w)
		ihdr = binary.BigEndian.AppendUint32(ihdr, h)
		out := append([]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", append(ihdr, depth, 0, 0, 0, 0))...)
		if key {
			out = append(out, pngChunk("tRNS", []byte{0, 0})...)
		}
		out = append(out, pngChunk("IDAT", raw.Bytes())...)
		return append(out, pngChunk("IEND", nil)...)
	}
	// unmatchedKey makes a colour no pixel of rawPNG's has transparent.
	unmatchedKey := func(p []byte, key []byte) []byte {
		return withPNGChunks(p, pngChunk("tRNS", key))
	}
	for _, c := range []struct {
		name          string
		upload        []byte
		colour, depth byte
	}{
		{"1-bit grey", lowBitGrey(1, false), 0, 8},
		{"2-bit grey", lowBitGrey(2, false), 0, 8},
		{"4-bit grey", lowBitGrey(4, false), 0, 8},
		{"8-bit grey", rawPNG(t, w, h, 0, 8, false, false), 0, 8},
		{"16-bit grey", rawPNG(t, w, h, 0, 16, false, false), 0, 16},
		{"1-bit grey with a colour key", lowBitGrey(1, true), 6, 8},
		{"4-bit grey with a colour key", lowBitGrey(4, true), 6, 8},
		{"8-bit grey with a colour key", rawPNG(t, w, h, 0, 8, false, true), 6, 8},
		{"8-bit RGB with a colour key", rawPNG(t, w, h, 2, 8, false, true), 6, 8},
		{"16-bit grey with a colour key", rawPNG(t, w, h, 0, 16, false, true), 6, 16},
		{"16-bit RGB with a colour key", rawPNG(t, w, h, 2, 16, false, true), 6, 16},
		{"8-bit grey with a key no pixel has", unmatchedKey(rawPNG(t, w, h, 0, 8, false, false), []byte{0, 0x99}), 2, 8},
		{"16-bit grey with a key no pixel has", unmatchedKey(rawPNG(t, w, h, 0, 16, false, false), []byte{0x99, 0x99}), 2, 16},
	} {
		got := process(t, c.upload)
		if colour, depth := got.Data[25], got.Data[24]; colour != c.colour || depth != c.depth {
			t.Errorf("%s: stored as colour type %d at %d bits, want %d at %d", c.name, colour, depth, c.colour, c.depth)
		}
		for _, typ := range pngChunkTypes(t, got.Data) {
			if typ == "tRNS" {
				t.Errorf("%s: stored with a tRNS chunk", c.name)
			}
		}
		requireSamePixels(t, decodeStored(t, c.upload), decodeStored(t, got.Data))
	}
}

// A PNG's EXIF orientation, in an eXIf chunk before its pixels, is applied as
// a JPEG's is, because browsers show the picture turned by it: every kind of
// PNG is stored turned, pixel for pixel, in the form image/png decodes it to,
// and its dimensions are the turned ones. An eXIf chunk after the pixels,
// which browsers do not read, or one holding anything but a TIFF structure,
// turns nothing. None of them is stored.
func TestPNGOrientationIsApplied(t *testing.T) {
	const w, h = 12, 7
	rng := rand.New(rand.NewSource(4))
	nrgba := image.NewNRGBA(image.Rect(0, 0, w, h))
	gray16 := image.NewGray16(image.Rect(0, 0, w, h))
	paletted := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{
		color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}, color.RGBA{0, 0, 255, 255}, color.NRGBA{9, 9, 9, 128},
	})
	rng.Read(gray16.Pix)
	for i := range nrgba.Pix {
		nrgba.Pix[i] = byte(1 + rng.Intn(255)) // no fully transparent pixels, whose colour is cleared
	}
	for i := range paletted.Pix {
		paletted.Pix[i] = uint8(rng.Intn(4))
	}
	pictures := map[string]image.Image{"NRGBA": nrgba, "Gray16": gray16, "Paletted": paletted}

	for name, pic := range pictures {
		clean := pngOf(t, pic)
		original, err := png.Decode(bytes.NewReader(clean))
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		for o := 1; o <= 8; o++ {
			order := tiffOrder(binary.LittleEndian)
			if o%2 == 0 {
				order = binary.BigEndian
			}
			upload := withPNGChunks(clean, pngChunk("eXIf", orientationTIFF(order, uint16(o))))
			if got := pngOrientation(upload); got != o {
				t.Fatalf("%s: pngOrientation = %d, want %d", name, got, o)
			}
			got := process(t, upload)
			requireFree(t, got.Data, "eXIf")
			stored := decodeStored(t, got.Data)
			if reflect.TypeOf(stored) != reflect.TypeOf(original) {
				t.Errorf("%s orientation %d: stored as %T, want %T", name, o, stored, original)
			}
			wantW, wantH := w, h
			if o >= 5 {
				wantW, wantH = h, w
			}
			if got.Width != wantW || got.Height != wantH || stored.Bounds() != image.Rect(0, 0, wantW, wantH) {
				t.Fatalf("%s orientation %d: dimensions %dx%d, stored %v, want %dx%d", name, o, got.Width, got.Height, stored.Bounds(), wantW, wantH)
			}
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					dx, dy := orientedPoint(o, x, y, w, h)
					r1, g1, b1, a1 := original.At(x, y).RGBA()
					r2, g2, b2, a2 := stored.At(dx, dy).RGBA()
					// A partly transparent pixel's colour is cut to what its
					// alpha shows (reduceHiddenColour), so the look lands, to a
					// step, where the orientation puts it, and the alpha exactly.
					if !nearPremul(r1, r2) || !nearPremul(g1, g2) || !nearPremul(b1, b2) || a1 != a2 {
						t.Fatalf("%s orientation %d: (%d,%d) landed as %v, want about %v", name, o, x, y, stored.At(dx, dy), original.At(x, y))
					}
				}
			}
		}
	}

	clean := pngOf(t, nrgba)
	iend := len(clean) - 12
	notRead := map[string][]byte{
		"eXIf after the pixels": append(append(append([]byte(nil), clean[:iend]...),
			pngChunk("eXIf", orientationTIFF(binary.BigEndian, 6))...), clean[iend:]...),
		"eXIf with an Exif prefix": withPNGChunks(clean,
			pngChunk("eXIf", append([]byte("Exif\x00\x00"), orientationTIFF(binary.BigEndian, 6)...))),
		"eXIf of nonsense": withPNGChunks(clean, pngChunk("eXIf", []byte(needleComment))),
	}
	for name, upload := range notRead {
		if _, err := png.Decode(bytes.NewReader(upload)); err != nil {
			t.Fatalf("%s: the fixture does not decode: %v", name, err)
		}
		if o := pngOrientation(upload); o != 1 {
			t.Errorf("%s: pngOrientation = %d, want 1", name, o)
		}
		got := process(t, upload)
		requireFree(t, got.Data, "eXIf")
		requireSameAppearance(t, nrgba, decodeStored(t, got.Data))
	}
	if allocs := testing.AllocsPerRun(10, func() { pngOrientation(notRead["eXIf after the pixels"]) }); allocs != 0 {
		t.Errorf("pngOrientation allocated %v times, want none", allocs)
	}
}

// Fuzzing the PNG reader as the JPEG one: whatever the bytes, it answers with
// an orientation and does not panic.
func FuzzPNGOrientation(f *testing.F) {
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewGray(image.Rect(0, 0, 3, 2)))
	f.Add(withPNGChunks(buf.Bytes(), pngChunk("eXIf", orientationTIFF(binary.BigEndian, 6))))
	f.Add(withPNGChunks(buf.Bytes(), pngChunk("eXIf", gpsTIFF(binary.LittleEndian, 8))))
	f.Add([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x08eXIfII*\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if o := pngOrientation(data); o < 1 || o > 8 {
			t.Fatalf("pngOrientation = %d, outside 1–8", o)
		}
	})
}

func requireSamePixels(t *testing.T, want, got image.Image) {
	t.Helper()
	if want.Bounds() != got.Bounds() {
		t.Fatalf("bounds = %v, want %v", got.Bounds(), want.Bounds())
	}
	b := want.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r1, g1, b1, a1 := want.At(x, y).RGBA()
			r2, g2, b2, a2 := got.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got.At(x, y), want.At(x, y))
			}
		}
	}
}

// requireSameAppearance is requireSamePixels relaxed to what a picture
// composites to. Cutting a partly transparent pixel's colour to the precision
// its alpha can show (reduceHiddenColour) leaves what shows unchanged but for
// rounding: the premultiplied RGBA that At reports moves by at most one 8-bit
// step, while the alpha is kept exactly. So an opaque picture still has to
// match to the bit, and a partly transparent one keeps its look.
func requireSameAppearance(t *testing.T, want, got image.Image) {
	t.Helper()
	if want.Bounds() != got.Bounds() {
		t.Fatalf("bounds = %v, want %v", got.Bounds(), want.Bounds())
	}
	b := want.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r1, g1, b1, a1 := want.At(x, y).RGBA()
			r2, g2, b2, a2 := got.At(x, y).RGBA()
			if !nearPremul(r1, r2) || !nearPremul(g1, g2) || !nearPremul(b1, b2) || a1 != a2 {
				t.Fatalf("pixel (%d,%d) = %v, want about %v (same colour to the alpha's precision)", x, y, got.At(x, y), want.At(x, y))
			}
		}
	}
}

// nearPremul reports whether two premultiplied 16-bit channel values are the
// same 8-bit value give or take one step.
func nearPremul(u, v uint32) bool {
	return (u>>8) <= (v>>8)+1 && (v>>8) <= (u>>8)+1
}

/* GIF ---------------------------------------------------------------------- */

// animatedGIF is four frames on a 60×40 canvas, the second covering only part
// of it, with distinct delays and disposals and the given loop count, each
// showing a picture of its own.
func animatedGIF(t *testing.T, loop int) *gif.GIF {
	t.Helper()
	pal := color.Palette(palette.WebSafe[:32])
	g := &gif.GIF{
		LoopCount:       loop,
		Delay:           []int{10, 20, 30, 40},
		Disposal:        []byte{gif.DisposalNone, gif.DisposalBackground, gif.DisposalPrevious, gif.DisposalNone},
		Config:          image.Config{ColorModel: pal, Width: 60, Height: 40},
		BackgroundIndex: 2,
	}
	for i, r := range []image.Rectangle{image.Rect(0, 0, 60, 40), image.Rect(10, 5, 30, 25), image.Rect(0, 0, 60, 40), image.Rect(0, 0, 60, 40)} {
		frame := image.NewPaletted(r, pal)
		for j := range frame.Pix {
			frame.Pix[j] = uint8((j/7 + i*5) % len(pal))
		}
		g.Image = append(g.Image, frame)
	}
	return g
}

// withGIFExtras splices a comment and an XMP application extension in after a
// GIF's header and global colour table, and another comment before its
// trailer.
func withGIFExtras(t *testing.T, data []byte) []byte {
	t.Helper()
	at := 13
	if flags := data[10]; flags&0x80 != 0 {
		at += 3 << (1 + flags&0x07)
	}
	subBlocks := func(s string) []byte {
		var out []byte
		for len(s) > 0 {
			n := min(len(s), 255)
			out = append(append(out, byte(n)), s[:n]...)
			s = s[n:]
		}
		return append(out, 0)
	}
	comment := append([]byte{0x21, 0xFE}, subBlocks(needleComment)...)
	xmp := append(append([]byte{0x21, 0xFF, 0x0B}, "XMP DataXMP"...), subBlocks(xmpPacket())...)

	out := append([]byte(nil), data[:at]...)
	out = append(out, comment...)
	out = append(out, xmp...)
	out = append(out, data[at:len(data)-1]...)
	out = append(out, comment...)
	return append(out, 0x3B)
}

// An animated GIF stays animated: what it shows survives, each picture for
// as long as it shows, and the loop count, and the comments and the XMP do
// not, nor does the background colour, which browsers never draw: it is
// written as entry 0.
func TestGIFKeepsItsAnimationAndDropsItsExtensions(t *testing.T) {
	for _, loop := range []int{0, 3} {
		original := animatedGIF(t, loop)
		var buf bytes.Buffer
		if err := gif.EncodeAll(&buf, original); err != nil {
			t.Fatalf("encode: %v", err)
		}
		upload := withGIFExtras(t, buf.Bytes())
		needles := []string{needleComment, "XMP DataXMP", needleXMPGPS}
		requireCarries(t, upload, needles...)
		if _, err := gif.DecodeAll(bytes.NewReader(upload)); err != nil {
			t.Fatalf("the fixture does not decode: %v", err)
		}

		got := process(t, upload)
		requireFree(t, got.Data, needles...)
		if got.Width != 60 || got.Height != 40 {
			t.Errorf("dimensions = %dx%d, want the 60x40 canvas", got.Width, got.Height)
		}
		requireSamePlay(t, "with its extensions", upload, got.Data)
		stored, err := gif.DecodeAll(bytes.NewReader(got.Data))
		if err != nil {
			t.Fatalf("decode stored gif: %v", err)
		}
		if stored.LoopCount != loop {
			t.Errorf("loop count = %d, want %d", stored.LoopCount, loop)
		}
		if len(stored.Image) != 4 {
			t.Errorf("stored gif has %d frames, want one for each of its 4 pictures", len(stored.Image))
		}
		if stored.BackgroundIndex != 0 {
			t.Errorf("background index = %d, want 0 whatever the upload's (%d)", stored.BackgroundIndex, original.BackgroundIndex)
		}
	}
}

// loopExtension is an application extension giving a loop count, under the
// identifier given: NETSCAPE2.0, or ANIMEXTS1.0, which browsers read the same.
func loopExtension(id string, loop int) []byte {
	ext := append([]byte{0x21, 0xFF, 0x0B}, id...)
	return append(ext, 0x03, 0x01, byte(loop), byte(loop>>8), 0x00)
}

// An animation keeps the loop count a browser would have played it with,
// whichever of the two loop extensions it came in, the last one winning as in
// a browser; it is stored as NETSCAPE2.0, the one image/gif writes and reads.
// An extension that gives no loop count is not one.
func TestGIFLoopCountIsReadFromEitherLoopExtension(t *testing.T) {
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, animatedGIF(t, -1)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte("NETSCAPE2.0")) {
		t.Fatal("the base fixture has a loop extension of its own")
	}
	buffering := append(append([]byte{0x21, 0xFF, 0x0B}, "ANIMEXTS1.0"...), 0x05, 0x02, 0, 0, 1, 0, 0x00)
	for name, tc := range map[string]struct {
		exts [][]byte
		want int
	}{
		"ANIMEXTS1.0, forever":           {[][]byte{loopExtension("ANIMEXTS1.0", 0)}, 0},
		"ANIMEXTS1.0, four times":        {[][]byte{loopExtension("ANIMEXTS1.0", 4)}, 4},
		"NETSCAPE2.0, then ANIMEXTS1.0":  {[][]byte{loopExtension("NETSCAPE2.0", 5), loopExtension("ANIMEXTS1.0", 2)}, 2},
		"ANIMEXTS1.0, then NETSCAPE2.0":  {[][]byte{loopExtension("ANIMEXTS1.0", 2), loopExtension("NETSCAPE2.0", 5)}, 5},
		"ANIMEXTS1.0 buffering, no loop": {[][]byte{buffering}, -1},
		"no loop extension":              {nil, -1},
	} {
		upload := buf.Bytes()
		for i := len(tc.exts) - 1; i >= 0; i-- {
			upload = spliceAfterHeader(upload, tc.exts[i])
		}
		if _, err := gif.DecodeAll(bytes.NewReader(upload)); err != nil {
			t.Fatalf("%s: the fixture does not decode: %v", name, err)
		}
		got := process(t, upload)
		stored, err := gif.DecodeAll(bytes.NewReader(got.Data))
		if err != nil {
			t.Fatalf("%s: decode stored gif: %v", name, err)
		}
		if stored.LoopCount != tc.want {
			t.Errorf("%s: stored loop count = %d, want %d", name, stored.LoopCount, tc.want)
		}
		if bytes.Contains(got.Data, []byte("ANIMEXTS1.0")) {
			t.Errorf("%s: the stored file carries ANIMEXTS1.0; image/gif writes only NETSCAPE2.0", name)
		}
	}
}

// A GIF's first frame need not cover its canvas. The dimensions are the
// canvas's, and the thumbnail shows the frame where it sits, with the rest of
// the canvas left transparent.
func TestGIFThumbnailPlacesTheFirstFrameOnTheCanvas(t *testing.T) {
	red := color.RGBA{255, 0, 0, 255}
	frame := image.NewPaletted(image.Rect(100, 50, 300, 150), color.Palette{red})
	g := &gif.GIF{
		Image:  []*image.Paletted{frame},
		Delay:  []int{0},
		Config: image.Config{ColorModel: color.Palette{red}, Width: 400, Height: 200},
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := process(t, buf.Bytes())
	if got.Width != 400 || got.Height != 200 {
		t.Errorf("dimensions = %dx%d, want the 400x200 canvas", got.Width, got.Height)
	}
	if got.Thumb.Width != 250 || got.Thumb.Height != 125 {
		t.Fatalf("thumbnail = %dx%d, want 250x125", got.Thumb.Width, got.Thumb.Height)
	}
	thumb := decodeStored(t, got.Thumb.Data)
	if c := thumb.At(125, 62); !near(c, red) {
		t.Errorf("thumbnail centre = %v, want the frame's red", c)
	}
	if _, _, _, a := thumb.At(5, 5).RGBA(); a != 0 {
		t.Errorf("thumbnail corner has alpha %d, want the uncovered canvas transparent", a)
	}
}

// A GIF's thumbnail is the first picture it shows, at the size of its canvas:
// its first frame drawn where it is on a transparent canvas, scaled down as a
// whole, as a still picture of the same pixels would be. A frame that covers
// the canvas but draws only a line two pixels wide, or a square in its middle,
// is thumbnailed with the line, a half pixel wide, and with the square's edges
// where they fall, a quarter of the way into a pixel; neither is placed by
// the part of the frame that draws.
func TestGIFThumbnailIsTheFirstPictureAtTheSizeOfItsCanvas(t *testing.T) {
	pal := color.Palette{color.RGBA{}, color.RGBA{20, 20, 20, 255}, color.RGBA{230, 40, 40, 255}, color.RGBA{40, 40, 230, 255}}
	const side = 1000
	for _, tc := range []struct {
		name  string
		drawn func(x, y int) uint8
	}{
		{"a line two pixels wide", func(x, y int) uint8 {
			if x == 501 || x == 502 {
				return 1
			}
			return 0
		}},
		{"a square in the middle", func(x, y int) uint8 {
			if x >= 349 && x < 650 && y >= 349 && y < 650 {
				return uint8(1 + (x/7+y/7)%2)
			}
			return 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := image.NewPaletted(image.Rect(0, 0, side, side), pal)
			want := image.NewNRGBA(first.Rect)
			for y := 0; y < side; y++ {
				for x := 0; x < side; x++ {
					v := tc.drawn(x, y)
					first.SetColorIndex(x, y, v)
					want.Set(x, y, pal[v])
				}
			}
			second := image.NewPaletted(image.Rect(0, 0, 10, 10), pal)
			for i := range second.Pix {
				second.Pix[i] = 3
			}
			g := &gif.GIF{
				Image:    []*image.Paletted{first, second},
				Delay:    []int{50, 50},
				Disposal: []byte{gif.DisposalNone, gif.DisposalNone},
				Config:   image.Config{ColorModel: pal, Width: side, Height: side},
			}
			var buf bytes.Buffer
			if err := gif.EncodeAll(&buf, g); err != nil {
				t.Fatalf("encode: %v", err)
			}
			got := decodeStored(t, process(t, buf.Bytes()).Thumb.Data)
			still, err := makeThumbnail(want, FormatGIF)
			if err != nil {
				t.Fatalf("thumbnail of the picture: %v", err)
			}
			expected := decodeStored(t, still.Data)
			if got.Bounds() != expected.Bounds() {
				t.Fatalf("thumbnail is %v, want %v", got.Bounds(), expected.Bounds())
			}
			// The scaler takes a different path for a picture of its own
			// kind than for an NRGBA one, which can round a channel apart.
			worst, shown := 0, 0
			b := got.Bounds()
			for y := b.Min.Y; y < b.Max.Y; y++ {
				for x := b.Min.X; x < b.Max.X; x++ {
					gr, gg, gb, ga := got.At(x, y).RGBA()
					er, eg, eb, ea := expected.At(x, y).RGBA()
					if ga != 0 {
						shown++
					}
					for _, d := range []int{int(gr>>8) - int(er>>8), int(gg>>8) - int(eg>>8), int(gb>>8) - int(eb>>8), int(ga>>8) - int(ea>>8)} {
						worst = max(worst, d, -d)
					}
				}
			}
			if shown == 0 {
				t.Fatal("the thumbnail is transparent all over")
			}
			if worst > 2 {
				t.Errorf("the thumbnail differs from that of the first picture by up to %d in a channel", worst)
			}
		})
	}
}

// tinyFrames is a GIF of n one-pixel frames, each marking a colour of its
// 256-colour global table transparent, which makes image/gif keep a copy of
// the table for every frame: the most a frame can cost for the fewest bytes
// of file, 25.
func tinyFrames(n int) []byte {
	b := []byte("GIF89a\x01\x00\x01\x00\xF7\x00\x00")
	b = append(b, make([]byte, 3*256)...)
	for i := 0; i < n; i++ {
		b = append(b, 0x21, 0xF9, 4, 1, 0, 0, 0, 0)       // colour 0 is transparent
		b = append(b, 0x2C, 0, 0, 0, 0, 1, 0, 1, 0, 0)    // one pixel at the origin
		b = append(b, 8, 4, 0x00, 0x01, 0x04, 0x04, 0x00) // LZW: clear, 0, end
	}
	return append(b, 0x3B)
}

// A GIF's frames are all decoded, so their area together is bounded as well
// as each frame's. The count reads only the block structure, before any
// decoding, and allocates nothing however many frames there are.
func TestGIFFrameBudget(t *testing.T) {
	const side = 5000
	if side*side != MaxPixels {
		t.Fatalf("%d² is not MaxPixels", side)
	}
	gifOf := func(frames int) []byte {
		// Each frame is a whole canvas of MaxPixels, whose pixel data is
		// one sub-block of nonsense the count never looks at.
		b := []byte("GIF89a")
		b = binary.LittleEndian.AppendUint16(b, side)
		b = binary.LittleEndian.AppendUint16(b, side)
		b = append(b, 0x80, 0, 0, 0, 0, 0, 255, 255, 255) // a two-colour global table
		for i := 0; i < frames; i++ {
			b = append(b, 0x21, 0xF9, 4, 0, 10, 0, 0, 0) // a graphic control extension
			b = append(b, 0x2C, 0, 0, 0, 0)
			b = binary.LittleEndian.AppendUint16(b, side)
			b = binary.LittleEndian.AppendUint16(b, side)
			b = append(b, 0, 2, 1, 0x44, 0)
		}
		return append(b, 0x3B)
	}

	within := gifOf(4)
	if got, err := gifFramePixels(within, maxGIFFramePixels); got.pixels != 4*MaxPixels || got.count != 4 || err != nil {
		t.Errorf("gifFramePixels = %+v, %v; want four frames of %d pixels and no error", got, err, 4*MaxPixels)
	}
	if _, err := Inspect(within); err != nil {
		t.Errorf("Inspect(four canvases of frames) = %v, want it within the budget", err)
	}

	over := gifOf(5)
	if _, err := Inspect(over); !errors.Is(err, ErrImageTooCostly) {
		t.Errorf("Inspect(five canvases of frames) = %v, want ErrImageTooCostly", err)
	}
	if _, err := Process(context.Background(), over); !errors.Is(err, ErrImageTooCostly) {
		t.Errorf("Process(five canvases of frames) = %v, want ErrImageTooCostly", err)
	}
	many := gifOf(2000)
	if allocs := testing.AllocsPerRun(10, func() { gifFramePixels(many, maxGIFFramePixels) }); allocs != 0 {
		t.Errorf("gifFramePixels allocated %v times, want none", allocs)
	}

	// A frame of no size to speak of is not free: image/gif keeps a header
	// and a palette for every one. Twenty thousand of them, half a megabyte
	// of file, would hold more than the whole processing budget, and are
	// refused from the count; two thousand are not.
	g, err := gif.DecodeAll(bytes.NewReader(tinyFrames(3)))
	if err != nil || len(g.Image) != 3 || &g.Image[0].Palette[0] == &g.Image[1].Palette[0] {
		t.Fatalf("the fixture does not decode to three frames with palettes of their own (%v)", err)
	}
	if _, err := Inspect(tinyFrames(20_000)); !errors.Is(err, ErrImageTooCostly) {
		t.Errorf("Inspect(20,000 one-pixel frames) = %v, want ErrImageTooCostly", err)
	}
	if _, err := Inspect(tinyFrames(2_000)); err != nil {
		t.Errorf("Inspect(2,000 one-pixel frames) = %v, want it within the budget", err)
	}
}

// The walk charges a GIF's frames against maxGIFFramePixels by their pixels,
// but every row as at least gifRowPixels (gifCharge): decoding, playing and
// writing a frame go through it a row at a time, and a row a pixel wide costs
// about what a wide one does. Frames a pixel wide and 65,535 tall are charged
// 64 pixels a row, so 23 of them are admitted and 24 refused from the header,
// where by their pixels alone 1,525 were admitted, and took 21 seconds of
// CPU on one core to play. A frame 63 pixels wide is charged as one of 64,
// and one 64 wide or wider by its pixels, as before (four canvases' worth of
// 5000×5000 frames still come to the cap exactly, TestGIFFrameBudget).
func TestGIFFrameRowsAreChargedAsAtLeastGIFRowPixels(t *testing.T) {
	const h = 65535
	for _, c := range []struct {
		w, n            int
		pixels, charged int64
		admitted        bool
	}{
		{1, 23, 23 * h, 23 * 64 * h, true},
		{1, 24, 0, 0, false},
		{63, 23, 23 * 63 * h, 23 * 64 * h, true},
		{64, 23, 23 * 64 * h, 23 * 64 * h, true},
		{64, 24, 0, 0, false},
	} {
		name := fmt.Sprintf("%d frames %d×%d", c.n, c.w, h)
		// Each frame's pixel data is one sub-block of nonsense the walk
		// never looks at, as in TestGIFFrameBudget.
		data := []byte("GIF89a")
		data = binary.LittleEndian.AppendUint16(data, uint16(c.w))
		data = binary.LittleEndian.AppendUint16(data, h)
		data = append(data, 0x80, 0, 0, 0, 0, 0, 255, 255, 255)
		for i := 0; i < c.n; i++ {
			data = append(data, 0x21, 0xF9, 4, 0, 10, 0, 0, 0, 0x2C, 0, 0, 0, 0)
			data = binary.LittleEndian.AppendUint16(data, uint16(c.w))
			data = binary.LittleEndian.AppendUint16(data, h)
			data = append(data, 0, 2, 1, 0x44, 0)
		}
		data = append(data, 0x3B)
		frames, err := gifFramePixels(data, maxGIFFramePixels)
		_, inspected := Inspect(data)
		if !c.admitted {
			if !errors.Is(err, ErrImageTooCostly) || !errors.Is(inspected, ErrImageTooCostly) {
				t.Errorf("%s: gifFramePixels = %v, Inspect = %v; want both ErrImageTooCostly", name, err, inspected)
			}
			continue
		}
		if err != nil || inspected != nil {
			t.Errorf("%s: gifFramePixels = %v, Inspect = %v; want both admitted", name, err, inspected)
		}
		if frames.pixels != c.pixels || frames.charged != c.charged {
			t.Errorf("%s: pixels %d, charged %d; want %d and %d", name, frames.pixels, frames.charged, c.pixels, c.charged)
		}
	}
}

// gifExtensions are extensions image/gif and Chromium both read past, ending
// each in the same place.
var gifExtensions = map[string][]byte{
	"plain text as specified":     append(append([]byte{0x21, 0x01, 0x0C}, make([]byte, 12)...), 0x03, 'a', 'b', 'c', 0x00),
	"application as specified":    append(append([]byte{0x21, 0xFF, 0x0B}, "XMP DataXMP"...), 0x01, 'x', 0x00),
	"comment":                     {0x21, 0xFE, 0x02, 'h', 'i', 0x00},
	"a loop count as ANIMEXTS1.0": loopExtension("ANIMEXTS1.0", 7),
	"graphic control":             {0x21, 0xF9, 0x04, 0x00, 0x0A, 0x00, 0x00, 0x00},
}

// gifExtensionsReadApart are extensions image/gif reads past that Chromium
// ends somewhere else, so that the two read the rest of the file differently
// and Chromium stops showing frames there (walkGIF). image/gif reads a
// plain-text extension as a fixed thirteen bytes whatever its block size byte
// says, where Chromium reads that byte as a sub-block's length; it reads
// sub-blocks after an application extension whose block size byte is 0,
// where Chromium ends it at once; and it ends a loop extension at a
// terminator straight after its identifier, which Chromium takes for the
// loop sub-block before reading on. A walk that read the first two as a run
// of sub-blocks once lost its place in image/gif's reading and counted none
// of the frames after them; it now refuses all of these.
var gifExtensionsReadApart = map[string][]byte{
	"application with an empty identifier": {0x21, 0xFF, 0x00, 0x02, 'x', 'x', 0x00},
	"application of no bytes":              {0x21, 0xFF, 0x00, 0x00},
	"plain text with a zero block size":    append(append([]byte{0x21, 0x01, 0x00}, make([]byte, 12)...), 0x00),
	"plain text with a block size of 0x61": append(append([]byte{0x21, 0x01, 0x61}, make([]byte, 12)...), 0x00),
	"NETSCAPE2.0 with no sub-block":        append(append([]byte{0x21, 0xFF, 0x0B}, "NETSCAPE2.0"...), 0x00),
	"ANIMEXTS1.0 with no sub-block":        append(append([]byte{0x21, 0xFF, 0x0B}, "ANIMEXTS1.0"...), 0x00),
}

// framesDecoded is what gifFramePixels should make of a GIF image/gif
// decoded as g: the area and number of its frames, the largest of them, and
// its loop count.
func framesDecoded(g *gif.GIF) gifFrames {
	f := gifFrames{count: int64(len(g.Image)), looped: g.LoopCount != -1}
	for _, frame := range g.Image {
		area := int64(frame.Bounds().Dx()) * int64(frame.Bounds().Dy())
		f.pixels += area
		f.largest = max(f.largest, area)
	}
	if f.looped {
		f.loop = g.LoopCount
	}
	return f
}

// counted is what of a walk's gifFrames a decoder can be held to: what
// framesDecoded reads off the frames it made.
func counted(f gifFrames) gifFrames {
	return gifFrames{pixels: f.pixels, count: f.count, looped: f.looped, loop: f.loop, largest: f.largest}
}

// decodeAsStored decodes a GIF the way sanitizeGIF does, from what the walk
// made of it (gifReader).
func decodeAsStored(t *testing.T, data []byte) (*gif.GIF, gifFrames) {
	t.Helper()
	f, err := gifFramePixels(data, math.MaxInt64)
	if err != nil {
		t.Fatalf("gifFramePixels: %v", err)
	}
	g, err := gif.DecodeAll(gifReader(data, f))
	if err != nil {
		t.Fatalf("the decoder refuses what the walk took: %v", err)
	}
	return g, f
}

// spliceAfterHeader puts ext into a GIF after its header and global colour
// table, before the first frame.
func spliceAfterHeader(data, ext []byte) []byte {
	at := 13
	if flags := data[10]; flags&0x80 != 0 {
		at += 3 << (1 + flags&0x07)
	}
	out := append([]byte(nil), data[:at]...)
	out = append(out, ext...)
	return append(out, data[at:]...)
}

// withFrameAt moves frame k of a GIF that image/gif wrote to (left, top),
// keeping its size: the position fields of its image descriptor, and nothing
// else, change.
func withFrameAt(t *testing.T, data []byte, k, left, top int) []byte {
	t.Helper()
	out := append([]byte(nil), data...)
	table := func(flags byte) int {
		if flags&0x80 == 0 {
			return 0
		}
		return 3 << (1 + flags&0x07)
	}
	subBlocks := func(i int) int {
		for i < len(out) && out[i] != 0 {
			i += 1 + int(out[i])
		}
		return i + 1
	}
	for i, frame := 13+table(out[10]), 0; i < len(out); {
		switch out[i] {
		case 0x21: // image/gif writes graphic control and application extensions
			switch out[i+1] {
			case 0xF9:
				i += 8
			case 0xFF:
				i = subBlocks(i + 3 + int(out[i+2]))
			default:
				t.Fatalf("an extension image/gif does not write, %#x", out[i+1])
			}
		case 0x2C:
			if frame == k {
				binary.LittleEndian.PutUint16(out[i+1:], uint16(left))
				binary.LittleEndian.PutUint16(out[i+3:], uint16(top))
				return out
			}
			frame++
			i = subBlocks(i + 10 + table(out[i+9]) + 1)
		default:
			t.Fatalf("no frame %d in the fixture", k)
		}
	}
	t.Fatalf("no frame %d in the fixture", k)
	return nil
}

// A GIF is stored as browsers show it where image/gif alone would refuse it.
// A file that ends after its last frame without the trailer plays in a
// browser, and is stored as if it had one. A frame after the first that
// reaches past the canvas is drawn by a browser only where it is on the
// canvas, and is stored as the same GIF with that frame cut to it would be.
// One wholly off the canvas draws nothing, so what shows for its delay is
// what the frame before it left once disposed of: after a frame that is kept
// (disposal 0 or 1), that frame's picture, which shows on for both delays,
// as the GIF without the frame off the canvas would show it; after one
// restored to the background (2), the canvas with that frame's area cleared;
// after one restored to the previous picture (3), the canvas as it was before
// that frame. Either of the last two is stored as the GIF whose frame off the
// canvas is replaced by one drawing nothing on it would be. A first frame
// past the canvas is refused before any cooldown: Chromium widens the canvas
// to show it, Firefox moves it to the corner, and there is no one picture to
// store.
func TestGIFIsStoredAsBrowsersShowIt(t *testing.T) {
	src := animatedGIF(t, 0) // 60×40; frame 1 is 20×20 at (10,5)
	encode := func(t *testing.T, g *gif.GIF) []byte {
		t.Helper()
		var buf bytes.Buffer
		if err := gif.EncodeAll(&buf, g); err != nil {
			t.Fatalf("encode: %v", err)
		}
		return buf.Bytes()
	}
	whole := encode(t, src)
	want := process(t, whole)

	t.Run("no trailer", func(t *testing.T) {
		got := process(t, whole[:len(whole)-1])
		if !bytes.Equal(got.Data, want.Data) || !bytes.Equal(got.Thumb.Data, want.Thumb.Data) {
			t.Error("stored differently from the same GIF with its trailer")
		}
	})

	// requireStoredAs checks that an upload past the canvas is stored as
	// the GIF a browser shows of it, and that that shows what the upload
	// does, decoded as sanitizeGIF decodes it (gifReader) and played on the
	// canvas.
	requireStoredAs := func(t *testing.T, upload []byte, shown *gif.GIF) {
		t.Helper()
		got, as := process(t, upload).Data, process(t, encode(t, shown)).Data
		if !bytes.Equal(got, as) {
			t.Error("stored differently from the GIF a browser shows of it")
		}
		up, _ := decodeAsStored(t, upload)
		up.Config.Width, up.Config.Height = 60, 40
		requirePlays(t, "past the canvas", up, got)
	}
	// without is src with frame k replaced, or left out if m is nil.
	without := func(k int, m *image.Paletted) *gif.GIF {
		g := *src
		g.Image = append([]*image.Paletted(nil), src.Image...)
		g.Delay = append([]int(nil), src.Delay...)
		g.Disposal = append([]byte(nil), src.Disposal...)
		if m != nil {
			g.Image[k] = m
			return &g
		}
		g.Delay[k-1] += g.Delay[k]
		g.Image = append(g.Image[:k], g.Image[k+1:]...)
		g.Delay = append(g.Delay[:k], g.Delay[k+1:]...)
		g.Disposal = append(g.Disposal[:k], g.Disposal[k+1:]...)
		return &g
	}

	t.Run("a later frame past the canvas", func(t *testing.T) {
		// Moved to (50,30), frame 1's pixel (x, y) is the source's
		// (x-40, y-25), and the part on the canvas is (50,30)-(60,40).
		cut := image.NewPaletted(image.Rect(50, 30, 60, 40), src.Image[1].Palette)
		for y := 30; y < 40; y++ {
			for x := 50; x < 60; x++ {
				cut.SetColorIndex(x, y, src.Image[1].ColorIndexAt(x-40, y-25))
			}
		}
		requireStoredAs(t, withFrameAt(t, whole, 1, 50, 30), without(1, cut))
	})

	t.Run("a later frame wholly off the canvas", func(t *testing.T) {
		requireStoredAs(t, withFrameAt(t, whole, 1, 61, 41), without(1, nil))
	})

	// Frame 1 is restored to the background and frame 2 to the previous
	// picture. A frame that draws nothing on the canvas is a pixel of the
	// one transparent colour of its own palette.
	nothing := image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{color.RGBA{}})
	t.Run("a frame wholly off the canvas after one restored to the background", func(t *testing.T) {
		requireStoredAs(t, withFrameAt(t, whole, 2, 61, 41), without(2, nothing))
	})
	t.Run("a frame wholly off the canvas after one restored to the previous picture", func(t *testing.T) {
		requireStoredAs(t, withFrameAt(t, whole, 3, 61, 41), without(3, nothing))
	})

	t.Run("a first frame past the canvas", func(t *testing.T) {
		data := withFrameAt(t, whole, 0, 1, 0)
		if _, err := Inspect(data); !errors.Is(err, ErrCorruptImage) {
			t.Errorf("Inspect = %v, want ErrCorruptImage before any cooldown", err)
		}
	})
}

// The frame count reads a GIF block for block as image/gif does, so no
// extension can hide the frames after it from the budget, and a file the
// decoder could not read to its end is refused before anything is decoded.
// Where the walk reads a file browsers show and image/gif refuses as is, it
// is held to what the decoder reads from gifReader.
func TestGIFFramePixelsReadsWhatTheDecoderReads(t *testing.T) {
	var small bytes.Buffer
	if err := gif.EncodeAll(&small, animatedGIF(t, 0)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Five canvases of 4000×6000 frames, over the budget, whose pixels the
	// count never looks at.
	huge := []byte("GIF89a")
	huge = binary.LittleEndian.AppendUint16(huge, 4000)
	huge = binary.LittleEndian.AppendUint16(huge, 6000)
	huge = append(huge, 0x80, 0, 0, 0, 0, 0, 255, 255, 255)
	for i := 0; i < 5; i++ {
		huge = append(huge, 0x2C, 0, 0, 0, 0)
		huge = binary.LittleEndian.AppendUint16(huge, 4000)
		huge = binary.LittleEndian.AppendUint16(huge, 6000)
		huge = append(huge, 0, 2, 1, 0x44, 0)
	}
	huge = append(huge, 0x3B)

	for name, ext := range gifExtensions {
		data := spliceAfterHeader(small.Bytes(), ext)
		g, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: the fixture does not decode: %v", name, err)
		}
		if got, err := gifFramePixels(data, math.MaxInt64); counted(got) != framesDecoded(g) || !got.trailed || err != nil {
			t.Errorf("%s: gifFramePixels = %+v, %v; the decoder read %+v", name, got, err, framesDecoded(g))
		}
		if _, err := Inspect(spliceAfterHeader(huge, ext)); !errors.Is(err, ErrImageTooCostly) {
			t.Errorf("%s: Inspect(five canvases of frames after it) = %v, want ErrImageTooCostly", name, err)
		}
	}
	// image/gif decodes every frame after each of these, and Chromium none:
	// the walk refuses the file there, before anything is decoded.
	for name, ext := range gifExtensionsReadApart {
		data := spliceAfterHeader(small.Bytes(), ext)
		if _, err := gif.DecodeAll(bytes.NewReader(data)); err != nil {
			t.Fatalf("%s: the fixture does not decode: %v", name, err)
		}
		if got, err := gifFramePixels(data, math.MaxInt64); !errors.Is(err, ErrCorruptImage) || !got.apart || got.count != 0 {
			t.Errorf("%s: gifFramePixels = %+v, %v; want ErrCorruptImage at the extension, with apart set", name, got, err)
		}
		for _, data := range [][]byte{data, spliceAfterHeader(huge, ext)} {
			if _, err := Inspect(data); !errors.Is(err, ErrCorruptImage) {
				t.Errorf("%s: Inspect = %v, want ErrCorruptImage", name, err)
			}
		}
	}

	whole := small.Bytes()
	noTrailer := whole[:len(whole)-1]
	header := 13 + 3*(2<<(whole[10]&7))
	// Read as browsers read them: image/gif refuses each as it is, and
	// decodes it from gifReader.
	widened := map[string][]byte{
		"no trailer":                       noTrailer,
		"no trailer, after an extension":   append(append([]byte(nil), noTrailer...), gifExtensions["comment"]...),
		"a later frame past the canvas":    withFrameAt(t, whole, 1, 50, 30),
		"a later frame wholly off it":      withFrameAt(t, whole, 1, 61, 41),
		"a later frame past it, untrailed": withFrameAt(t, noTrailer, 2, 1, 1),
	}
	for name, data := range widened {
		if _, err := gif.DecodeAll(bytes.NewReader(data)); err == nil {
			t.Fatalf("%s: image/gif decodes the fixture as it is; it should not", name)
		}
		g, got := decodeAsStored(t, data)
		if counted(got) != framesDecoded(g) {
			t.Errorf("%s: gifFramePixels = %+v; the decoder read %+v", name, got, framesDecoded(g))
		}
		if _, err := Inspect(data); err != nil {
			t.Errorf("%s: Inspect = %v, want it taken", name, err)
		}
	}

	refused := map[string][]byte{
		"cut inside a frame":             whole[:len(whole)/2],
		"cut inside a comment":           append(append([]byte(nil), noTrailer...), 0x21, 0xFE, 0x05, 'h', 'i'),
		"cut after a frame's descriptor": append(append([]byte(nil), noTrailer...), 0x2C, 0, 0, 0, 0, 1, 0, 1, 0, 0),
		"an unknown block":               append(append([]byte(nil), noTrailer...), 0x00, 0x3B),
		"an unknown extension":           spliceAfterHeader(whole, []byte{0x21, 0x02, 0x00}),
		"a bad graphic control": spliceAfterHeader(whole,
			[]byte{0x21, 0xF9, 0x05, 0x00, 0x0A, 0x00, 0x00, 0x00, 0x00}),
		"no frames":                          spliceAfterHeader(whole[:header], []byte{0x3B}),
		"no frames and no trailer":           whole[:header],
		"a bad LZW minimum":                  spliceAfterHeader(whole, []byte{0x2C, 0, 0, 0, 0, 1, 0, 1, 0, 0, 9, 0}),
		"a first frame past the canvas":      withFrameAt(t, whole, 0, 1, 0),
		"a frame past what a header can say": withFrameAt(t, whole, 1, 0xFFFF-10, 0),
	}
	for name, data := range refused {
		if _, err := gif.DecodeAll(bytes.NewReader(data)); err == nil {
			t.Fatalf("%s: the fixture decodes; it should not", name)
		}
		if _, err := gifFramePixels(data, math.MaxInt64); !errors.Is(err, ErrCorruptImage) {
			t.Errorf("%s: gifFramePixels error = %v, want ErrCorruptImage", name, err)
		}
		if _, err := Inspect(data); !errors.Is(err, ErrCorruptImage) {
			t.Errorf("%s: Inspect = %v, want ErrCorruptImage", name, err)
		}
	}
}

// FuzzGIFFramePixels holds the count to the decoder: whatever image/gif
// decodes, the count reaches the trailer and makes the frames exactly as large
// as the decoder did, and reads the loop count the decoder read, when there is
// no ANIMEXTS1.0 loop extension for the walk to read and the decoder not.
func FuzzGIFFramePixels(f *testing.F) {
	var buf bytes.Buffer
	_ = gif.Encode(&buf, image.NewPaletted(image.Rect(0, 0, 4, 4), palette.Plan9), nil)
	f.Add(buf.Bytes())
	buf.Reset()
	_ = gif.EncodeAll(&buf, &gif.GIF{
		Image:     []*image.Paletted{image.NewPaletted(image.Rect(0, 0, 3, 2), palette.Plan9), image.NewPaletted(image.Rect(1, 1, 2, 2), palette.Plan9)},
		Delay:     []int{1, 2},
		LoopCount: 2,
	})
	f.Add(buf.Bytes())
	for _, ext := range gifExtensions {
		f.Add(spliceAfterHeader(buf.Bytes(), ext))
	}
	for _, ext := range gifExtensionsReadApart {
		f.Add(spliceAfterHeader(buf.Bytes(), ext))
	}
	f.Add([]byte("GIF89a\x01\x00\x01\x00\xff\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := gifFramePixels(data, math.MaxInt64)
		if n.pixels < 0 || n.count < 0 || n.largest > n.pixels {
			t.Fatalf("gifFramePixels = %+v", n)
		}
		// Bound what the decoder may allocate here, as Inspect does.
		if cfg, err := gif.DecodeConfig(bytes.NewReader(data)); err != nil || int64(cfg.Width)*int64(cfg.Height) > 1<<20 || n.pixels > 1<<24 || n.count > 1<<12 {
			return
		}
		// A file the walk takes is decoded as sanitizeGIF decodes it; one it
		// refuses, as it is, which the decoder must refuse too — but for a
		// file refused at an extension Chromium ends elsewhere (apart),
		// which image/gif reads past.
		if err != nil && n.apart {
			return
		}
		r := io.Reader(bytes.NewReader(data))
		if err == nil {
			r = gifReader(data, n)
		}
		g, decErr := gif.DecodeAll(r)
		if decErr != nil {
			return
		}
		want := framesDecoded(g)
		n = counted(n)
		if bytes.Contains(data, []byte("ANIMEXTS1.0")) {
			// image/gif does not read this loop extension; the walk does.
			n.looped, n.loop = false, 0
			want.looped, want.loop = false, 0
		}
		if err != nil || n != want {
			t.Fatalf("gifFramePixels = %+v, %v; the decoder read %+v", n, err, want)
		}
	})
}

/* WebP --------------------------------------------------------------------- */

func sampleWebP(t *testing.T) []byte {
	t.Helper()
	return readTestdata(t, "sample.webp")
}

// sampleBitstream is the lossless VP8L chunk of the 320×200 fixture.
func sampleBitstream(t *testing.T) riffChunk {
	t.Helper()
	chunks := webpChunks(t, sampleWebP(t))
	if len(chunks) != 1 || chunks[0].id != "VP8L" {
		t.Fatalf("sample.webp chunks = %v, want one VP8L", chunkIDs(chunks))
	}
	return chunks[0]
}

// readTestdata reads a fixture from testdata. Of the WebPs, sample.webp is
// lossless (VP8L, 320×200). lossy.webp and lossy-alpha.webp are 64×48, four
// flat quadrants (red, green / blue, white) made with Pillow at quality 90,
// the second with its left 16 columns transparent: a simple-format VP8 file,
// and VP8X, ALPH and VP8. hidden-lossless.webp and hidden-lossy-alpha.webp
// are 32×32, made with Pillow with exact=True so the colour under transparent
// pixels is kept (the second lossy, at quality 90): the left half fully
// transparent over random colours (seed 7), the right half opaque
// (200, 30, 30). cutout-alpha.webp is a cut-out photo as a lossy WebP with
// transparency, made with Pillow at quality 30: 640×480 of a colour ramp with
// noise (±60, seed 11) softened by a 0.8-pixel blur, inside an opaque ellipse
// on a transparent ground. Go cannot encode WebP, so these are files rather
// than built in code. photo-q30.jpg is a JPEG as libjpeg saves one for the
// web, from Pillow at quality 30 with optimize=True (Huffman tables fitted
// to the picture), 4:2:0: 640×480 of the same ramp and noise as the cut-out,
// softened the same way, opaque. lossy-alpha.libwebp.png is lossy-alpha.webp
// as libwebp 1.6.0 decodes it to RGBA, through Pillow 12.3.0: what a browser
// shows of it.
func readTestdata(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// An extended WebP carrying a profile, EXIF, XMP and a chunk nobody knows is
// stripped to the VP8X header and the untouched bitstream, with the flags
// that announced the metadata and every reserved bit cleared and the RIFF
// size right; that is what is decoded. What is stored is a PNG of the same
// pixels, with none of it.
func TestWebPIsStrippedChunkByChunk(t *testing.T) {
	bitstream := sampleBitstream(t)
	head := vp8xChunk(vp8xFlagICC|vp8xFlagEXIF|vp8xFlagXMP|0xC1, 320, 200)
	copy(head.payload[1:4], "SEC") // the 24 reserved bits after the flags
	upload := webpFile(
		head,
		riffChunk{id: "ICCP", payload: []byte(needleICC)},
		bitstream,
		riffChunk{id: "EXIF", payload: gpsTIFF(binary.LittleEndian, 1)},
		riffChunk{id: "XMP ", payload: []byte(needleXMP + " " + xmpPacket())},
		riffChunk{id: "ZZZZ", payload: []byte(needleComment + "!")}, // odd length, so padded
	)
	upload = append(upload, "after the end: "+needleIPTC...) // outside the RIFF
	needles := []string{"ICCP", needleICC, "EXIF", needleGPS, needleDatum, needleCamera, "XMP ",
		needleXMP, needleXMPGPS, "ZZZZ", needleComment, needleIPTC, "SEC"}
	requireCarries(t, upload, needles...)

	stripped, info, err := stripWebP(upload)
	if err != nil {
		t.Fatalf("stripWebP: %v", err)
	}
	if info.Width != 320 || info.Height != 200 || info.Animated {
		t.Errorf("info = %+v, want a still 320x200 canvas", info)
	}
	requireFree(t, stripped, needles...)
	chunks := webpChunks(t, stripped)
	if ids := strings.Join(chunkIDs(chunks), ","); ids != "VP8X,VP8L" {
		t.Fatalf("stripped chunks = %s, want VP8X,VP8L", ids)
	}
	if !bytes.Equal(chunks[0].payload[:4], []byte{0, 0, 0, 0}) {
		t.Errorf("VP8X flags and reserved bytes = % x, want all clear", chunks[0].payload[:4])
	}
	if !bytes.Equal(chunks[0].payload[4:], vp8xChunk(0, 320, 200).payload[4:]) {
		t.Errorf("VP8X canvas = % x, want 320x200 unchanged", chunks[0].payload[4:])
	}
	if !bytes.Equal(chunks[1].payload, bitstream.payload) {
		t.Error("the bitstream changed; stripping must leave the image data as it was")
	}

	got := process(t, upload)
	requireFree(t, got.Data, needles...)
	requireFree(t, got.Thumb.Data, needles...)
	if got.Format != FormatPNG {
		t.Errorf("stored format = %s, want png", got.Format.Name)
	}
	if got.Width != 320 || got.Height != 200 {
		t.Errorf("dimensions = %dx%d, want 320x200", got.Width, got.Height)
	}
	want, err := webp.Decode(bytes.NewReader(sampleWebP(t)))
	if err != nil {
		t.Fatalf("decode sample: %v", err)
	}
	requireSamePixels(t, want, decodeStored(t, got.Data))
}

// A VP8X's alpha flag decides whether Chromium shows a lossy picture's alpha
// plane: without the flag the picture shows opaque, the plane ignored, and so
// it is stored. Otherwise the flag is a hint, and Chromium shows what the
// chunks hold whatever it says. libwebp writes a lossless picture with
// transparency and metadata under it, though the lossless bitstream carries
// its own alpha and golang.org/x/image refuses the flag beside one, as it
// refuses the flag over a lossy picture with no alpha plane. Each is stored as
// the picture Chromium 141 showed, and costed as it is decoded.
func TestWebPAlphaFlagFollowsTheChunks(t *testing.T) {
	lossless := webpChunks(t, readTestdata(t, "hidden-lossless.webp")) // VP8L, half transparent
	lossy := webpChunks(t, readTestdata(t, "lossy.webp"))              // VP8, opaque
	lossyAlpha := webpChunks(t, readTestdata(t, "lossy-alpha.webp"))   // VP8X, ALPH, VP8
	if len(lossless) != 1 || lossless[0].id != "VP8L" || len(lossy) != 1 || lossy[0].id != "VP8 " ||
		len(lossyAlpha) != 3 || lossyAlpha[1].id != "ALPH" {
		t.Fatal("the fixtures are not the chunks this test expects")
	}
	exif := riffChunk{id: "EXIF", payload: gpsTIFF(binary.LittleEndian, 1)}
	icc := riffChunk{id: "ICCP", payload: []byte(needleICC)}
	for _, tc := range []struct {
		name       string
		upload, as []byte
	}{
		{"lossless under the alpha flag, with EXIF",
			webpFile(vp8xChunk(vp8xFlagAlpha|vp8xFlagEXIF, 32, 32), lossless[0], exif),
			webpFile(lossless[0])},
		{"lossless under the alpha flag, with a profile",
			webpFile(vp8xChunk(vp8xFlagAlpha|vp8xFlagICC, 32, 32), icc, lossless[0]),
			webpFile(lossless[0])},
		{"lossy under the alpha flag with no plane",
			webpFile(vp8xChunk(vp8xFlagAlpha, 64, 48), lossy[0]),
			webpFile(lossy[0])},
		{"an alpha plane without the flag",
			webpFile(vp8xChunk(0, 64, 48), lossyAlpha[1], lossyAlpha[2]),
			webpFile(lossyAlpha[2])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, gotAlpha, _ := webpPicture(tc.upload)
			if _, wantAlpha, _ := webpPicture(tc.as); gotAlpha != wantAlpha {
				t.Errorf("costed with an alpha plane: %v, want %v", gotAlpha, wantAlpha)
			}
			want := process(t, tc.as)
			got, err := Process(context.Background(), tc.upload)
			if err != nil {
				t.Fatalf("Process = %v, want it stored as the picture it shows", err)
			}
			if !bytes.Equal(got.Data, want.Data) || !bytes.Equal(got.Thumb.Data, want.Thumb.Data) {
				t.Errorf("stored as a different %s from the same picture without the flag's mismatch", got.Format.Name)
			}
			requireFree(t, got.Data, needleGPS, needleICC)
		})
	}
}

// A WebP is stored as a file Go encoded from its pixels: a lossless one as a
// PNG with exactly the same pixels, a lossy one with transparency as a PNG
// that keeps it, and a lossy opaque one as a JPEG. The thumbnail follows the
// stored format.
func TestWebPIsStoredAsPNGOrJPEG(t *testing.T) {
	red, green := color.RGBA{220, 30, 30, 255}, color.RGBA{30, 200, 30, 255}
	blue, white := color.RGBA{30, 30, 220, 255}, color.RGBA{250, 250, 250, 255}
	for _, tc := range []struct {
		file  string
		want  Format
		thumb Format
	}{
		{"sample.webp", FormatPNG, FormatPNG},
		{"lossy.webp", FormatJPEG, FormatJPEG},
		{"lossy-alpha.webp", FormatPNG, FormatPNG},
	} {
		t.Run(tc.file, func(t *testing.T) {
			upload := readTestdata(t, tc.file)
			src, err := webp.Decode(bytes.NewReader(upload))
			if err != nil {
				t.Fatalf("the fixture does not decode: %v", err)
			}
			got := process(t, upload)
			if got.Format != tc.want || got.Thumb.Format != tc.thumb {
				t.Fatalf("stored as %s with a %s thumbnail, want %s and %s",
					got.Format.Name, got.Thumb.Format.Name, tc.want.Name, tc.thumb.Name)
			}
			if f, ok := DetectFormat(got.Data); !ok || f != tc.want {
				t.Fatalf("the stored bytes are %v (%v), not %s", f, ok, tc.want.Name)
			}
			stored := decodeStored(t, got.Data)
			if stored.Bounds() != src.Bounds() || got.Width != src.Bounds().Dx() || got.Height != src.Bounds().Dy() {
				t.Fatalf("stored %v reported %dx%d, want %v", stored.Bounds(), got.Width, got.Height, src.Bounds())
			}
			switch tc.file {
			case "sample.webp":
				requireSamePixels(t, src, stored)
			case "lossy.webp", "lossy-alpha.webp":
				for _, p := range []struct {
					x, y int
					c    color.Color
				}{{24, 4, red}, {60, 4, green}, {24, 44, blue}, {60, 44, white}} {
					if c := stored.At(p.x, p.y); !near(c, p.c) {
						t.Errorf("pixel (%d,%d) = %v, want about %v", p.x, p.y, c, p.c)
					}
				}
				if tc.file == "lossy-alpha.webp" {
					for _, p := range []image.Point{{2, 4}, {10, 40}} {
						if _, _, _, a := stored.At(p.X, p.Y).RGBA(); a != 0 {
							t.Errorf("pixel %v has alpha %d, want it transparent as uploaded", p, a)
						}
					}
				}
			}
		})
	}
}

// Every stored PNG is deflated at BestSpeed (storedPNG), whatever it was
// made from: at the default level, deflate's search for matches makes the
// encoding's CPU depend on what the rows hold, many times over for rows of a
// few repeated byte values. compress/zlib's header says which: its second
// byte is 0x01 at BestSpeed and 0x9C at the default level.
func TestStoredPNGsAreDeflatedAtBestSpeed(t *testing.T) {
	encode := func(m image.Image) []byte {
		var buf bytes.Buffer
		if err := png.Encode(&buf, m); err != nil {
			t.Fatalf("encode: %v", err)
		}
		return buf.Bytes()
	}
	rgba := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	deep := image.NewNRGBA64(image.Rect(0, 0, 64, 48))
	pal := image.NewPaletted(image.Rect(0, 0, 64, 48), palette.Plan9)
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			rgba.SetNRGBA(x, y, color.NRGBA{uint8(x * 4), uint8(y * 5), 0x40, uint8(0xFF - x)})
			deep.SetNRGBA64(x, y, color.NRGBA64{uint16(x * 1001), uint16(y * 1303), 0x1234, uint16(0xFFFF - x*500)})
			pal.SetColorIndex(x, y, uint8(x+y))
		}
	}
	for _, tc := range []struct {
		name   string
		upload []byte
	}{
		{"8-bit PNG with transparency", encode(rgba)},
		{"16-bit PNG", encode(deep)},
		{"palette PNG", encode(pal)},
		{"lossless WebP", readTestdata(t, "sample.webp")},
		{"lossy WebP with transparency", readTestdata(t, "lossy-alpha.webp")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := process(t, tc.upload)
			if got.Format != FormatPNG {
				t.Fatalf("stored as %s, want PNG", got.Format.Name)
			}
			var idat []byte
			for i := 8; i+8 <= len(got.Data); {
				n := int(binary.BigEndian.Uint32(got.Data[i : i+4]))
				if string(got.Data[i+4:i+8]) == "IDAT" {
					idat = got.Data[i+8 : i+8+n]
					break
				}
				i += 12 + n
			}
			if len(idat) < 2 {
				t.Fatalf("the stored PNG has no image data: chunks %v", pngChunkTypes(t, got.Data))
			}
			if idat[0] != 0x78 || idat[1] != 0x01 {
				t.Errorf("the stored PNG's zlib header is % x, want 78 01 (BestSpeed)", idat[:2])
			}
		})
	}
}

// A lossy WebP is stored with the colours it showed, not the washed-out ones
// golang.org/x/image/webp's limited-range YCbCr gives when it is read as full
// range. webp-swatches.webp is three flat 48×48 swatches — black, white and
// pure red — libwebp wrote from RGB (0,0,0), (255,255,255) and (255,0,0); the
// decoder hands those back as Y=16/235/82, which color.YCbCr would store as
// (16,16,16), (235,235,235) and a muddy red. The stored JPEG, and the
// thumbnail drawn from the same pixels, must instead show the true colours,
// well inside the ±24 that re-encoding a flat colour at quality 90 can shift.
func TestLossyWebPKeepsItsColours(t *testing.T) {
	want := []struct {
		x int
		c color.RGBA
	}{
		{24, color.RGBA{0, 0, 0, 255}},
		{72, color.RGBA{255, 255, 255, 255}},
		{120, color.RGBA{255, 0, 0, 255}},
	}
	near12 := func(a color.Color, b color.RGBA) bool {
		r, g, bl, _ := a.RGBA()
		d := func(x uint32, y uint8) bool { v := int(x >> 8); return v-int(y) <= 12 && int(y)-v <= 12 }
		return d(r, b.R) && d(g, b.G) && d(bl, b.B)
	}
	got := process(t, readTestdata(t, "webp-swatches.webp"))
	if got.Format != FormatJPEG {
		t.Fatalf("stored as %s, want a JPEG", got.Format.Name)
	}
	stored := decodeStored(t, got.Data)
	for _, w := range want {
		if c := stored.At(w.x, 24); !near12(c, w.c) {
			t.Errorf("stored swatch at x=%d = %v, want about %v (washed-out range not expanded)", w.x, c, w.c)
		}
	}
	thumb, err := jpeg.Decode(bytes.NewReader(got.Thumb.Data))
	if err != nil {
		t.Fatalf("decode thumbnail: %v", err)
	}
	tb := thumb.Bounds()
	for _, w := range want {
		tx := tb.Min.X + w.x*tb.Dx()/144
		if c := thumb.At(tx, tb.Min.Y+tb.Dy()/2); !near12(c, w.c) {
			t.Errorf("thumbnail swatch near x=%d = %v, want about %v", w.x, c, w.c)
		}
	}
}

// A lossy WebP carries its colour at half resolution each way (4:2:0), and an
// opaque one is stored as a JPEG at that same resolution: encoded from RGB at
// 4:4:4, the JPEG spent bytes on colour detail the WebP never had and was
// stored up to a quarter larger for it. An alpha plane that is opaque
// throughout changes nothing, and the colours are the ones the WebP showed
// (bt601LimitedToRGB), on average to within what re-encoding at quality 90
// costs.
func TestLossyWebPIsStoredAtItsOwnColourResolution(t *testing.T) {
	plain := readTestdata(t, "lossy.webp")
	chunks := webpChunks(t, plain)
	if len(chunks) != 1 || chunks[0].id != "VP8 " {
		t.Fatalf("lossy.webp chunks = %v, want one VP8", chunkIDs(chunks))
	}
	opaque := append([]byte{0}, bytes.Repeat([]byte{0xFF}, 64*48)...) // uncompressed, all opaque
	withAlpha := webpFile(vp8xChunk(vp8xFlagAlpha, 64, 48), riffChunk{id: "ALPH", payload: opaque}, chunks[0])

	src, err := webp.Decode(bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("decode lossy.webp: %v", err)
	}
	ycc, ok := src.(*image.YCbCr)
	if !ok || ycc.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		t.Fatalf("lossy.webp decodes as %T, want 4:2:0 YCbCr", src)
	}
	want := image.NewNRGBA(ycc.Rect)
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			r, g, b := bt601LimitedToRGB(ycc.Y[ycc.YOffset(x, y)], ycc.Cb[ycc.COffset(x, y)], ycc.Cr[ycc.COffset(x, y)])
			want.SetNRGBA(x, y, color.NRGBA{r, g, b, 0xFF})
		}
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{{"simple", plain}, {"with an opaque alpha plane", withAlpha}} {
		t.Run(tc.name, func(t *testing.T) {
			got := process(t, tc.data)
			if got.Format != FormatJPEG {
				t.Fatalf("stored as %s, want a JPEG", got.Format.Name)
			}
			f, framed := readJPEGFrame(got.Data)
			if !framed || f.comps != 3 {
				t.Fatal("the stored file has no colour frame")
			}
			if s := f.subsampling(); s != image.YCbCrSubsampleRatio420 {
				t.Errorf("stored at %v, want the WebP's own 4:2:0", s)
			}
			if d := meanDifference(t, decodeStored(t, got.Data), want); d > 2 {
				t.Errorf("stored picture differs from the colours the WebP showed by %.2f levels on average", d)
			}
		})
	}
}

// A lossy WebP with transparency is stored as RGBA, so its half-resolution
// chroma has to be widened to a sample a pixel, and it is widened as libwebp's
// fancy upsampler widens it, which is what Chromium shows: a blend of the
// four nearest samples rather than the nearest alone, which at the colour
// edges of lossy-alpha.webp's quadrants was 91 levels off what the browser
// showed, and 151 on a sticker. lossy-alpha.libwebp.png is the fixture as
// libwebp decodes it; the stored picture is held to it within a level, the
// rounding of the conversion to RGB (bt601LimitedToRGB, where libwebp's has
// 14 bits). webpChromaRow is held, bit for bit, to libwebp's own loops
// (libwebpUpsampled) on planes of every parity, down to a pixel.
func TestLossyWebPIsUpsampledAsLibwebpDoes(t *testing.T) {
	got := decodeStored(t, process(t, readTestdata(t, "lossy-alpha.webp")).Data)
	ref, err := png.Decode(bytes.NewReader(readTestdata(t, "lossy-alpha.libwebp.png")))
	if err != nil {
		t.Fatalf("decode the reference: %v", err)
	}
	if got.Bounds() != ref.Bounds() {
		t.Fatalf("stored %v, the reference is %v", got.Bounds(), ref.Bounds())
	}
	worst := 0
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			g, r := color.NRGBAModel.Convert(got.At(x, y)).(color.NRGBA), color.NRGBAModel.Convert(ref.At(x, y)).(color.NRGBA)
			if g.A != r.A {
				t.Fatalf("(%d,%d): alpha %d, libwebp's %d", x, y, g.A, r.A)
			}
			if r.A == 0 {
				continue // libwebp keeps a colour under a clear pixel; educhan does not
			}
			for _, d := range [3]int{int(g.R) - int(r.R), int(g.G) - int(r.G), int(g.B) - int(r.B)} {
				worst = max(worst, d, -d)
			}
			if worst > 1 {
				t.Fatalf("(%d,%d) stored as %v, libwebp shows %v", x, y, g, r)
			}
		}
	}

	rng := rand.New(rand.NewSource(3))
	for _, size := range []image.Point{{1, 1}, {1, 6}, {6, 1}, {2, 2}, {3, 3}, {7, 5}, {8, 6}, {9, 10}, {16, 9}} {
		w, h := size.X, size.Y
		m := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
		rng.Read(m.Cb)
		rng.Read(m.Cr)
		cw, ch := (w+1)/2, (h+1)/2
		want := [2][][]uint8{libwebpUpsampled(m.Cb, m.CStride, cw, ch, w, h), libwebpUpsampled(m.Cr, m.CStride, cw, ch, w, h)}
		cb, cr := make([]uint8, w), make([]uint8, w)
		for y := 0; y < h; y++ {
			webpChromaRow(cb, cr, m, y)
			for x := 0; x < w; x++ {
				if cb[x] != want[0][y][x] || cr[x] != want[1][y][x] {
					t.Fatalf("%dx%d (%d,%d): chroma %d,%d, libwebp's %d,%d", w, h, x, y, cb[x], cr[x], want[0][y][x], want[1][y][x])
				}
			}
		}
	}
}

// libwebpUpsampled is a 4:2:0 chroma plane of cw×ch samples widened to w×h
// by libwebp's fancy upsampler, transcribed from src/dsp/upsampling.c
// (UPSAMPLE_FUNC, here for one plane rather than U and V packed together)
// and the order src/dec/io_dec.c's EmitFancyRGB runs it in: the first row on
// its own, against its own chroma row, then pairs of rows between two chroma
// rows, then, for an even height, the last row on its own.
func libwebpUpsampled(plane []uint8, stride, cw, ch, w, h int) [][]uint8 {
	out := make([][]uint8, h)
	for y := range out {
		out[y] = make([]uint8, w)
	}
	row := func(j int) []uint8 { return plane[j*stride : j*stride+cw] }
	// linePair is UPSAMPLE_FUNC: top and bottom are output rows (bottom may
	// be nil), between chroma rows topUV and curUV.
	linePair := func(top, bottom []uint8, topUV, curUV []uint8) {
		lastPair := (w - 1) >> 1
		tl, l := int(topUV[0]), int(curUV[0])
		top[0] = uint8((3*tl + l + 2) >> 2)
		if bottom != nil {
			bottom[0] = uint8((3*l + tl + 2) >> 2)
		}
		for x := 1; x <= lastPair; x++ {
			t, uv := int(topUV[x]), int(curUV[x])
			avg := tl + t + l + uv + 8
			diag12 := (avg + 2*(t+l)) >> 3
			diag03 := (avg + 2*(tl+uv)) >> 3
			top[2*x-1] = uint8((diag12 + tl) >> 1)
			top[2*x] = uint8((diag03 + t) >> 1)
			if bottom != nil {
				bottom[2*x-1] = uint8((diag03 + l) >> 1)
				bottom[2*x] = uint8((diag12 + uv) >> 1)
			}
			tl, l = t, uv
		}
		if w&1 == 0 {
			top[w-1] = uint8((3*tl + l + 2) >> 2)
			if bottom != nil {
				bottom[w-1] = uint8((3*l + tl + 2) >> 2)
			}
		}
	}
	linePair(out[0], nil, row(0), row(0))
	y := 0
	for ; y+2 < h; y += 2 {
		linePair(out[y+1], out[y+2], row(y/2), row(y/2+1))
	}
	if h&1 == 0 {
		linePair(out[h-1], nil, row(ch-1), row(ch-1))
	}
	return out
}

// A decoder stops reading a picture chunk once it has its picture, so a whole
// second file can ride inside one: here a JPEG carrying a camera's EXIF and GPS
// position, inside a lossless bitstream, a lossy one, and an alpha plane. The
// upload still decodes, so stripping has to keep the chunk; none of it is in
// what is stored.
func TestFilesHiddenInsideWebPChunksAreNotStored(t *testing.T) {
	hidden := cameraJPEG(t, 16, 16)
	needles := []string{"Exif", needleGPS, needleDatum, needleCamera, needleXMP, needleComment}
	inside := func(t *testing.T, file, id string) []byte {
		t.Helper()
		var out []riffChunk
		found := false
		for _, c := range webpChunks(t, readTestdata(t, file)) {
			if c.id == id {
				c.payload = append(append([]byte(nil), c.payload...), hidden...)
				found = true
			}
			out = append(out, c)
		}
		if !found {
			t.Fatalf("%s has no %s chunk", file, id)
		}
		return webpFile(out...)
	}
	for _, tc := range []struct{ file, id string }{
		{"sample.webp", "VP8L"},
		{"lossy.webp", "VP8 "},
		{"lossy-alpha.webp", "VP8 "},
		{"lossy-alpha.webp", "ALPH"},
	} {
		t.Run(tc.file+" "+tc.id, func(t *testing.T) {
			upload := inside(t, tc.file, tc.id)
			requireCarries(t, upload, needles...)
			want, err := webp.Decode(bytes.NewReader(readTestdata(t, tc.file)))
			if err != nil {
				t.Fatalf("decode the fixture: %v", err)
			}
			if got, err := webp.Decode(bytes.NewReader(upload)); err != nil {
				t.Fatalf("the upload does not decode, so it proves nothing: %v", err)
			} else {
				requireSamePixels(t, want, got)
			}
			stripped, _, err := stripWebP(upload)
			if err != nil {
				t.Fatalf("stripWebP: %v", err)
			}
			requireCarries(t, stripped, needles...) // what stripping alone would have stored

			got := process(t, upload)
			requireFree(t, got.Data, needles...)
			requireFree(t, got.Thumb.Data, needles...)
		})
	}
}

// The rewrite understands animation, keeping ANIM and each frame's picture and
// dropping everything else inside and around them. An animated WebP is still
// refused, as it always has been, since x/image cannot decode one: by Inspect
// from its header (TestAnimatedWebPIsRefusedFromItsHeader), and by
// sanitizeWebP should one get that far.
func TestAnimatedWebP(t *testing.T) {
	bitstream := sampleBitstream(t)
	unknown := riffChunk{id: "ZZZZ", payload: []byte(needleComment)}
	upload := webpFile(
		vp8xChunk(vp8xFlagAnimation|vp8xFlagEXIF, 320, 200),
		riffChunk{id: "ANIM", payload: []byte{0, 0, 0, 0, 0, 0}},
		anmfChunk(0, 0, 320, 200, bitstream, unknown),
		anmfChunk(0, 0, 320, 200, unknown, bitstream),
		riffChunk{id: "EXIF", payload: gpsTIFF(binary.BigEndian, 1)},
	)
	requireCarries(t, upload, needleComment, needleGPS)

	stripped, info, err := stripWebP(upload)
	if err != nil {
		t.Fatalf("stripWebP: %v", err)
	}
	if !info.Animated || info.Width != 320 || info.Height != 200 {
		t.Errorf("info = %+v, want an animated 320x200 canvas", info)
	}
	requireFree(t, stripped, needleComment, needleGPS)
	chunks := webpChunks(t, stripped)
	if ids := strings.Join(chunkIDs(chunks), ","); ids != "VP8X,ANIM,ANMF,ANMF" {
		t.Fatalf("stored chunks = %s, want VP8X,ANIM,ANMF,ANMF", ids)
	}
	if flags := chunks[0].payload[0]; flags != vp8xFlagAnimation {
		t.Errorf("VP8X flags = %#x, want only the animation flag", flags)
	}
	want := anmfChunk(0, 0, 320, 200, bitstream).payload
	for _, frame := range chunks[2:] {
		if !bytes.Equal(frame.payload, want) {
			t.Error("a frame was not reduced to its header and its picture")
		}
	}

	if _, err := Process(context.Background(), upload); !errors.Is(err, ErrCorruptImage) {
		t.Errorf("Process(animated WebP) = %v, want ErrCorruptImage, as before stripping", err)
	}
	if _, err := sanitizeWebP(context.Background(), upload, 1<<20); !errors.Is(err, ErrCorruptImage) {
		t.Errorf("sanitizeWebP(animated WebP) = %v, want ErrCorruptImage", err)
	}
}

// An animated WebP is refused from its header, before anything is decoded
// and before any cooldown, and with the same answer whatever its size: the
// animation flag in its VP8X, or, in an extended file, a frame among its
// chunks, is enough to know that nothing here can decode it. A long one used
// to be charged as a lossless picture of its whole length and refused as too
// large, a short one let through to fail as it was decoded.
func TestAnimatedWebPIsRefusedFromItsHeader(t *testing.T) {
	bitstream := sampleBitstream(t) // 320×200
	frames := func(n, pad int) []riffChunk {
		var out []riffChunk
		for i := 0; i < n; i++ {
			out = append(out, anmfChunk(0, 0, 320, 200, bitstream, riffChunk{id: "ZZZZ", payload: make([]byte, pad)}))
		}
		return out
	}
	animated := func(flags byte, w, h int, chunks ...riffChunk) []byte {
		return webpFile(append([]riffChunk{vp8xChunk(flags, w, h), {id: "ANIM", payload: make([]byte, 6)}}, chunks...)...)
	}
	twoFrames := animated(vp8xFlagAnimation, 320, 200, frames(2, 0)...)
	sixFrames := animated(vp8xFlagAnimation, 320, 200, frames(6, 50<<10)...)
	for name, data := range map[string][]byte{
		"two frames":                   twoFrames,
		"six frames of 300 KiB in all": sixFrames,
		"a canvas past MaxPixels":      animated(vp8xFlagAnimation, 10000, 10000, frames(2, 0)...),
		"the flag and no frames":       animated(vp8xFlagAnimation, 320, 200),
		"frames without the flag":      animated(0, 320, 200, frames(2, 0)...),
		"a picture, then a frame":      webpFile(vp8xChunk(0, 320, 200), bitstream, frames(1, 0)[0]),
		// Cut short, as a partly sent file is: the RIFF size runs past the
		// end, and the first chunk still says what the file is.
		"two frames, cut short":              twoFrames[:len(twoFrames)-100],
		"six frames, cut short":              sixFrames[:len(sixFrames)-100],
		"frames without the flag, cut short": animated(0, 320, 200, frames(2, 0)...)[:len(twoFrames)-100],
	} {
		if !webpAnimated(data) {
			t.Errorf("%s: webpAnimated = false", name)
		}
		if _, err := Inspect(data); !errors.Is(err, ErrCorruptImage) {
			t.Errorf("%s: Inspect = %v, want ErrCorruptImage before anything is decoded", name, err)
		}
	}
}

// A simple-format WebP (its first chunk the picture) is a still whatever
// follows its picture: stripWebP keeps the picture and drops the rest, and
// browsers show the picture alone. A frame after it is not an animation.
func TestWebPStillWithAFrameAfterItsPictureIsStoredAsAStill(t *testing.T) {
	bitstream := sampleBitstream(t) // 320×200, lossless
	data := webpFile(bitstream, riffChunk{id: "ANIM", payload: make([]byte, 6)}, anmfChunk(0, 0, 320, 200, bitstream))
	if webpAnimated(data) {
		t.Error("webpAnimated = true for a simple-format still")
	}
	if _, err := Inspect(data); err != nil {
		t.Fatalf("Inspect = %v, want nil", err)
	}
	out, err := Process(context.Background(), data)
	if err != nil {
		t.Fatalf("Process = %v", err)
	}
	if out.Format != FormatPNG || out.Width != 320 || out.Height != 200 {
		t.Errorf("stored %s %d×%d, want a 320×200 PNG", out.Format.Name, out.Width, out.Height)
	}
}

// A WebP whose structure does not hold together is refused, whatever a
// lenient decoder would have made of it.
func TestMalformedWebPIsRefused(t *testing.T) {
	bitstream := sampleBitstream(t)
	clean := sampleWebP(t)
	longer := append([]byte(nil), clean...)
	binary.LittleEndian.PutUint32(longer[4:8], uint32(len(clean)))
	chunkPastEnd := append([]byte(nil), clean...)
	binary.LittleEndian.PutUint32(chunkPastEnd[16:20], uint32(len(clean)))

	cases := map[string][]byte{
		"RIFF size past the end": longer,
		"chunk past the end":     chunkPastEnd,
		// The canvas says 10×10 and the bitstream 320×200; decoders size
		// their buffers from one or the other.
		"canvas and bitstream disagree": webpFile(vp8xChunk(0, 10, 10), bitstream),
		"VP8X of the wrong length":      webpFile(riffChunk{id: "VP8X", payload: make([]byte, 9)}, bitstream),
		"two bitstreams":                webpFile(vp8xChunk(0, 320, 200), bitstream, bitstream),
		"no bitstream":                  webpFile(riffChunk{id: "EXIF", payload: []byte("x")}),
		"alpha in the simple format":    webpFile(riffChunk{id: "ALPH", payload: []byte{0}}, bitstream),
		"frame outside the canvas": webpFile(vp8xChunk(vp8xFlagAnimation, 320, 200),
			riffChunk{id: "ANIM", payload: make([]byte, 6)}, anmfChunk(2, 0, 320, 200, bitstream)),
	}
	for name, upload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := stripWebP(upload); !errors.Is(err, ErrCorruptImage) {
				t.Errorf("stripWebP = %v, want ErrCorruptImage", err)
			}
			if _, err := Process(context.Background(), upload); err == nil {
				t.Error("Process accepted it")
			}
		})
	}
	// A RIFF that runs past the end is known from the header, so it is
	// refused before any cooldown, as it would be once processed.
	if _, err := Inspect(longer); !errors.Is(err, ErrCorruptImage) {
		t.Errorf("Inspect(RIFF size past the end) = %v, want ErrCorruptImage", err)
	}

	huge := webpFile(vp8xChunk(0, 10000, 10000), bitstream)
	if _, _, err := stripWebP(huge); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("stripWebP(10000x10000 canvas) = %v, want ErrImageTooLarge", err)
	}
}

// Whatever the bytes, the rewrite does not panic, and what it accepts it
// would accept again unchanged: stripping is idempotent. The seeds are
// lossless and lossy, simple and extended, with and without an alpha plane,
// metadata or animation.
func FuzzStripWebP(f *testing.F) {
	sample, err := os.ReadFile(filepath.Join("testdata", "sample.webp"))
	if err != nil {
		f.Fatalf("read fixture: %v", err)
	}
	f.Add(sample)
	f.Add(webpFile(vp8xChunk(vp8xFlagEXIF, 320, 200), riffChunk{id: "VP8L", payload: sample[20:]},
		riffChunk{id: "EXIF", payload: []byte("Exif")}))
	// lossy.webp is a VP8 chunk alone, and lossy-alpha.webp VP8X, ALPH and
	// VP8, both 64×48.
	lossy, lossyAlpha := readTestdata(f, "lossy.webp"), readTestdata(f, "lossy-alpha.webp")
	f.Add(lossy)
	f.Add(lossyAlpha)
	var vp8, alph riffChunk
	walkChunks(lossyAlpha[12:], func(c riffChunk) {
		switch c.id {
		case "VP8 ":
			vp8 = c
		case "ALPH":
			alph = c
		}
	})
	f.Add(webpFile(vp8, riffChunk{id: "EXIF", payload: []byte("Exif")}))
	f.Add(webpFile(vp8xChunk(vp8xFlagICC|vp8xFlagAlpha|vp8xFlagEXIF|vp8xFlagXMP, 64, 48),
		riffChunk{id: "ICCP", payload: []byte("profile")}, alph, vp8,
		riffChunk{id: "EXIF", payload: []byte("Exif")}, riffChunk{id: "XMP ", payload: []byte("<x/>")},
		riffChunk{id: "ZZZZ", payload: []byte("unknown")}))
	f.Add(webpFile(vp8xChunk(vp8xFlagAnimation|vp8xFlagAlpha, 64, 48), riffChunk{id: "ANIM", payload: make([]byte, 6)},
		anmfChunk(0, 0, 64, 48, alph, vp8, riffChunk{id: "ZZZZ", payload: []byte("unknown")})))
	f.Fuzz(func(t *testing.T, data []byte) {
		out, info, err := stripWebP(data)
		if err != nil {
			return
		}
		if int64(info.Width)*int64(info.Height) > MaxPixels {
			t.Fatalf("accepted a %dx%d canvas", info.Width, info.Height)
		}
		again, _, err := stripWebP(out)
		if err != nil || !bytes.Equal(again, out) {
			t.Fatalf("stripping the stripped file changed it (err %v)", err)
		}
	})
}

/* Checksum ----------------------------------------------------------------- */

// The checksum is of what is stored, so the same picture uploaded with
// different metadata on it has the same one.
func TestChecksumIsOfTheStoredFile(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	plain := encodeJPEGImage(t, quadrants(64, 32))
	tagged := withSegments(plain, exifSegment(gpsTIFF(binary.LittleEndian, 1)), commentSegment())
	ctx := context.Background()
	a, err := Save(ctx, store, "general", plain, time.Now())
	if err != nil {
		t.Fatalf("Save(plain): %v", err)
	}
	b, err := Save(ctx, store, "general", tagged, time.Now())
	if err != nil {
		t.Fatalf("Save(tagged): %v", err)
	}
	if a.Checksum != b.Checksum || a.Size != b.Size {
		t.Errorf("the same picture stored as %s (%d bytes) and %s (%d bytes); want one checksum",
			a.Checksum, a.Size, b.Checksum, b.Size)
	}
}

/* Processing budget -------------------------------------------------------- */

// An upload waits until its estimated cost fits in the processing budget, and
// gives up when its context does, or at once when its deadline leaves no time
// to wait. Given back, the budget serves the next.
func TestProcessWaitsForItsShareAndGivesUpWithItsContext(t *testing.T) {
	upload := encodePNG(t, 32, 32)
	in, err := inspect(upload)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	cost := in.cost
	b := narrowBudget(t, cost)

	release, err := acquireProcessing(context.Background(), 1)
	if err != nil {
		t.Fatalf("take a byte of the budget: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Process(ctx, upload)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Process returned (%v) while its cost did not fit", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Process error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Process kept waiting after its context was cancelled")
	}

	// A deadline too near to leave time for the work is not waited out.
	short, stop := context.WithTimeout(context.Background(), processingAllowance/2)
	defer stop()
	if _, err := Process(short, upload); !errors.Is(err, ErrBusy) {
		t.Errorf("Process error = %v, want ErrBusy", err)
	}

	release()
	for i := 0; i < 2; i++ {
		if _, err := Process(context.Background(), upload); err != nil {
			t.Fatalf("Process with the budget free: %v", err)
		}
	}
	if n := b.held(); n != 0 {
		t.Errorf("%d bytes of the budget still held after every upload finished", n)
	}
}

// A file refused from its header never waits for the budget: there is
// nothing to decode.
func TestHeaderRefusalsDoNotWaitForTheBudget(t *testing.T) {
	b := narrowBudget(t, 1)
	release, err := b.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("take the whole budget: %v", err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Process(ctx, []byte("not an image")); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("Process(text) = %v, want ErrUnsupportedFormat at once", err)
	}
}
