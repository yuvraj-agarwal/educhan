package media

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"image/png"
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
// start of its last row appear once it is viewed correctly.
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

	for name, stored := range pictures {
		for o := 1; o <= 8; o++ {
			order := tiffOrder(binary.LittleEndian)
			if o%2 == 0 {
				order = binary.BigEndian
			}
			upload := withSegments(encodeJPEGImage(t, stored), exifSegment(orientationTIFF(order, uint16(o))))
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

// Turning a YCbCr picture keeps every pixel's colour exactly, whatever its
// subsampling and size. Where the chroma samples line up with the turned
// picture, the planes are turned as they are and keep their subsampling; an
// odd side mirrored at 4:2:0, a side that is not a multiple of four mirrored
// at 4:1:1, and 4:1:1 or 4:1:0 on its side do not, and are turned a pixel at
// a time into 4:4:4, where before the colours along the side shifted a pixel.
func TestOrientKeepsEverySubsamplingExact(t *testing.T) {
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
	for _, size := range []image.Point{{8, 12}, {7, 5}, {5, 7}, {9, 6}, {6, 9}, {10, 13}, {1, 1}, {3, 2}} {
		w, h := size.X, size.Y
		for _, ratio := range ratios {
			src := image.NewYCbCr(image.Rect(0, 0, w, h), ratio)
			rng.Read(src.Y)
			rng.Read(src.Cb)
			rng.Read(src.Cr)
			fx, fy := subsampling(ratio)
			for o := 1; o <= 8; o++ {
				dst, ok := orient(src, o).(*image.YCbCr)
				if !ok {
					t.Fatalf("%dx%d %v orientation %d: turned into %T, want YCbCr", w, h, ratio, o, orient(src, o))
				}
				for y := 0; y < h; y++ {
					for x := 0; x < w; x++ {
						dx, dy := orientedPoint(o, x, y, w, h)
						if got, want := dst.YCbCrAt(dx, dy), src.YCbCrAt(x, y); got != want {
							t.Fatalf("%dx%d %v orientation %d: (%d,%d) landed as %v, want %v", w, h, ratio, o, x, y, got, want)
						}
					}
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
					t.Errorf("%dx%d %v orientation %d: turned at %v, want %v", w, h, ratio, o, dst.SubsampleRatio, want)
				}
			}
		}
	}
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
// read as upright, never as a panic, a hang or an allocation, and the decoder
// alone decides whether the file is an image: one whose segment runs off the
// end is refused as corrupt, one whose EXIF is merely nonsense is stored
// upright, with the nonsense gone.
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
		{"huge entry count", withSegments(base, exifSegment(hugeCount)), 1, false},
		{"IFD offsets in a loop", withSegments(base, exifSegment(loop)), 6, false},
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

// Fuzzing the reader with the fixtures as seeds: whatever the bytes, it
// answers with an orientation and does not panic.
func FuzzJPEGOrientation(f *testing.F) {
	f.Add(withSegments([]byte{0xFF, 0xD8, 0xFF, 0xD9}, exifSegment(gpsTIFF(binary.BigEndian, 6))))
	f.Add(withSegments([]byte{0xFF, 0xD8, 0xFF, 0xD9}, exifSegment(orientationTIFF(binary.LittleEndian, 8))))
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0, 0})
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

// Re-encoding keeps a 16-bit PNG at 16 bits, transparency and all.
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
	requireSamePixels(t, src, out)
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

// A PNG's EXIF orientation, in an eXIf chunk before its pixels, is applied
// as a JPEG's is, because browsers show the picture turned by it: every kind
// of PNG is stored turned, pixel for pixel, in the form image/png decodes it
// to, and its dimensions are the turned ones. An eXIf chunk after the pixels, which
// browsers do not read, or one holding anything but a TIFF structure, turns
// nothing. None of them is stored.
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
					if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
						t.Fatalf("%s orientation %d: (%d,%d) landed as %v, want %v", name, o, x, y, stored.At(dx, dy), original.At(x, y))
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
		requireSamePixels(t, nrgba, decodeStored(t, got.Data))
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

/* GIF ---------------------------------------------------------------------- */

// animatedGIF is three frames on a 60×40 canvas, the second covering only part
// of it, with distinct delays and disposals and the given loop count.
func animatedGIF(t *testing.T, loop int) *gif.GIF {
	t.Helper()
	pal := color.Palette(palette.WebSafe[:32])
	g := &gif.GIF{
		LoopCount:       loop,
		Delay:           []int{10, 20, 30},
		Disposal:        []byte{gif.DisposalNone, gif.DisposalBackground, gif.DisposalPrevious},
		Config:          image.Config{ColorModel: pal, Width: 60, Height: 40},
		BackgroundIndex: 2,
	}
	for i, r := range []image.Rectangle{image.Rect(0, 0, 60, 40), image.Rect(10, 5, 30, 25), image.Rect(0, 0, 60, 40)} {
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

// An animated GIF stays animated: frames, positions, delays, disposals and the
// loop count survive, and the comments and the XMP do not.
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
		stored, err := gif.DecodeAll(bytes.NewReader(got.Data))
		if err != nil {
			t.Fatalf("decode stored gif: %v", err)
		}
		if stored.LoopCount != loop {
			t.Errorf("loop count = %d, want %d", stored.LoopCount, loop)
		}
		if len(stored.Image) != 3 {
			t.Fatalf("stored gif has %d frames, want 3", len(stored.Image))
		}
		for i := range original.Image {
			if stored.Delay[i] != original.Delay[i] || stored.Disposal[i] != original.Disposal[i] {
				t.Errorf("frame %d: delay %d disposal %d, want %d and %d", i,
					stored.Delay[i], stored.Disposal[i], original.Delay[i], original.Disposal[i])
			}
			if stored.Image[i].Bounds() != original.Image[i].Bounds() {
				t.Errorf("frame %d: bounds %v, want %v", i, stored.Image[i].Bounds(), original.Image[i].Bounds())
			}
			requireSamePixels(t, original.Image[i], stored.Image[i])
		}
		if stored.BackgroundIndex != original.BackgroundIndex {
			t.Errorf("background index = %d, want %d", stored.BackgroundIndex, original.BackgroundIndex)
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
	if got, err := gifFramePixels(within, maxGIFFramePixels); got != (gifFrames{pixels: 4 * MaxPixels, count: 4}) || err != nil {
		t.Errorf("gifFramePixels = %+v, %v; want four frames of %d pixels and no error", got, err, 4*MaxPixels)
	}
	if _, err := Inspect(within); err != nil {
		t.Errorf("Inspect(four canvases of frames) = %v, want it within the budget", err)
	}

	over := gifOf(5)
	if _, err := Inspect(over); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("Inspect(five canvases of frames) = %v, want ErrImageTooLarge", err)
	}
	if _, err := Process(context.Background(), over); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("Process(five canvases of frames) = %v, want ErrImageTooLarge", err)
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
	if _, err := Inspect(tinyFrames(20_000)); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("Inspect(20,000 one-pixel frames) = %v, want ErrImageTooLarge", err)
	}
	if _, err := Inspect(tinyFrames(2_000)); err != nil {
		t.Errorf("Inspect(2,000 one-pixel frames) = %v, want it within the budget", err)
	}
}

// gifExtensions are extensions image/gif reads past, each spelled the way the
// decoder reads it rather than the way the specification lays it out where
// the two differ. A walk that read every extension as a run of sub-blocks
// lost its place at the first two and counted none of the frames after them.
var gifExtensions = map[string][]byte{
	// An application extension whose identifier is zero bytes long: the
	// decoder reads the zero as that length, then reads sub-blocks after it.
	"application with an empty identifier": {0x21, 0xFF, 0x00, 0x02, 'x', 'x', 0x00},
	// A plain-text extension is read as a fixed thirteen bytes, whatever its
	// block size byte says, then sub-blocks.
	"plain text with a zero block size": append(append([]byte{0x21, 0x01, 0x00}, make([]byte, 12)...), 0x00),
	"plain text as specified":           append(append([]byte{0x21, 0x01, 0x0C}, make([]byte, 12)...), 0x03, 'a', 'b', 'c', 0x00),
	"application as specified":          append(append([]byte{0x21, 0xFF, 0x0B}, "XMP DataXMP"...), 0x01, 'x', 0x00),
	"comment":                           {0x21, 0xFE, 0x02, 'h', 'i', 0x00},
	"a loop count as ANIMEXTS1.0":       loopExtension("ANIMEXTS1.0", 7),
	"graphic control":                   {0x21, 0xF9, 0x04, 0x00, 0x0A, 0x00, 0x00, 0x00},
}

// framesDecoded is what gifFramePixels should make of a GIF image/gif
// decoded as g: the area and number of its frames, and its loop count.
func framesDecoded(g *gif.GIF) gifFrames {
	f := gifFrames{count: int64(len(g.Image)), looped: g.LoopCount != -1}
	for _, frame := range g.Image {
		f.pixels += int64(frame.Bounds().Dx()) * int64(frame.Bounds().Dy())
	}
	if f.looped {
		f.loop = g.LoopCount
	}
	return f
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

// The frame count reads a GIF block for block as image/gif does, so no
// extension can hide the frames after it from the budget, and a file the
// decoder could not read to its trailer is refused before anything is decoded.
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
		if got, err := gifFramePixels(data, math.MaxInt64); got != framesDecoded(g) || err != nil {
			t.Errorf("%s: gifFramePixels = %+v, %v; the decoder read %+v", name, got, err, framesDecoded(g))
		}
		if _, err := Inspect(spliceAfterHeader(huge, ext)); !errors.Is(err, ErrImageTooLarge) {
			t.Errorf("%s: Inspect(five canvases of frames after it) = %v, want ErrImageTooLarge", name, err)
		}
	}

	whole := small.Bytes()
	refused := map[string][]byte{
		"no trailer":           whole[:len(whole)-1],
		"cut inside a frame":   whole[:len(whole)/2],
		"an unknown block":     append(append([]byte(nil), whole[:len(whole)-1]...), 0x00, 0x3B),
		"an unknown extension": spliceAfterHeader(whole, []byte{0x21, 0x02, 0x00}),
		"a bad graphic control": spliceAfterHeader(whole,
			[]byte{0x21, 0xF9, 0x05, 0x00, 0x0A, 0x00, 0x00, 0x00, 0x00}),
		"no frames":         spliceAfterHeader(whole[:13+3*(2<<(whole[10]&7))], []byte{0x3B}),
		"a bad LZW minimum": spliceAfterHeader(whole, []byte{0x2C, 0, 0, 0, 0, 1, 0, 1, 0, 0, 9, 0}),
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
	f.Add([]byte("GIF89a\x01\x00\x01\x00\xff\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := gifFramePixels(data, math.MaxInt64)
		if n.pixels < 0 || n.count < 0 {
			t.Fatalf("gifFramePixels = %+v", n)
		}
		// Bound what the decoder may allocate here, as Inspect does.
		if cfg, err := gif.DecodeConfig(bytes.NewReader(data)); err != nil || int64(cfg.Width)*int64(cfg.Height) > 1<<20 || n.pixels > 1<<24 || n.count > 1<<12 {
			return
		}
		g, decErr := gif.DecodeAll(bytes.NewReader(data))
		if decErr != nil {
			return
		}
		want := framesDecoded(g)
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

// readTestdata reads a fixture from testdata. Of the WebPs, sample.webp is lossless
// (VP8L, 320×200). lossy.webp and lossy-alpha.webp are 64×48, four flat
// quadrants (red, green / blue, white) made with Pillow at quality 90, the
// second with its left 16 columns transparent: a simple-format VP8 file, and
// VP8X, ALPH and VP8. hidden-lossless.webp and hidden-lossy-alpha.webp are
// 32×32, made with Pillow with exact=True so the colour under transparent
// pixels is kept (the second lossy, at quality 90): the left half fully
// transparent over random colours (seed 7), the right half opaque
// (200, 30, 30). cutout-alpha.webp is a cut-out photo as a lossy WebP with
// transparency, made with Pillow at quality 30: 640×480 of a colour ramp with
// noise (±60, seed 11) softened by a 0.8-pixel blur, inside an opaque ellipse
// on a transparent ground. Go cannot encode WebP, so these are files rather
// than built in code. photo-q30.jpg is a JPEG as libjpeg saves one for the
// web, from Pillow at quality 30 with optimize=True (Huffman tables fitted
// to the picture), 4:2:0: 640×480 of the same ramp and noise as the cut-out,
// softened the same way, opaque.
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
// dropping everything else inside and around them. Process still refuses an
// animated WebP, as it always has: x/image cannot decode one.
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

	huge := webpFile(vp8xChunk(0, 10000, 10000), bitstream)
	if _, _, err := stripWebP(huge); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("stripWebP(10000x10000 canvas) = %v, want ErrImageTooLarge", err)
	}
}

// Whatever the bytes, the rewrite does not panic, and what it accepts it
// would accept again unchanged: stripping is idempotent.
func FuzzStripWebP(f *testing.F) {
	sample, err := os.ReadFile(filepath.Join("testdata", "sample.webp"))
	if err != nil {
		f.Fatalf("read fixture: %v", err)
	}
	f.Add(sample)
	f.Add(webpFile(vp8xChunk(vp8xFlagEXIF, 320, 200), riffChunk{id: "VP8L", payload: sample[20:]},
		riffChunk{id: "EXIF", payload: []byte("Exif")}))
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
