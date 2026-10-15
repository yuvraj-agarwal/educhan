package media

import (
	"bytes"
	"compress/lzw"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"math/rand"
	"testing"
)

// Fixtures and tests for what a picture holds that nobody sees: colour under
// fully transparent pixels, and palette entries no pixel uses.

const (
	needlePalette = "SECRET-NEEDLE: Priya Sharma lives at 51.5N 0.12W"
	needleTRNS    = "alpha-needle: in the tRNS of unused entries"
	needleGlobal  = "global-table-needle: nobody draws this colour"
	needleLocal   = "local-table-needle: nor this one"
)

func pngOf(t *testing.T, m image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// hiddenUnderAlpha counts a decoded picture's fully transparent pixels, and
// those of them that hold a colour. It reads the values as stored, not the
// premultiplied colour At reports, which is zero there whatever is stored.
func hiddenUnderAlpha(t *testing.T, m image.Image) (coloured, transparent int) {
	t.Helper()
	count := func(isClear, hasColour bool) {
		if isClear {
			transparent++
			if hasColour {
				coloured++
			}
		}
	}
	switch p := m.(type) {
	case *image.NRGBA:
		for i := 0; i < len(p.Pix); i += 4 {
			count(p.Pix[i+3] == 0, p.Pix[i]|p.Pix[i+1]|p.Pix[i+2] != 0)
		}
	case *image.NRGBA64:
		for i := 0; i < len(p.Pix); i += 8 {
			count(p.Pix[i+6]|p.Pix[i+7] == 0, !bytes.Equal(p.Pix[i:i+6], make([]byte, 6)))
		}
	case *image.NYCbCrA:
		b := p.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				c := p.NYCbCrAAt(x, y)
				r, g, bl, _ := c.YCbCr.RGBA()
				count(c.A == 0, r|g|bl != 0)
			}
		}
	case *image.Paletted:
		for _, v := range p.Pix {
			c, ok := p.Palette[v].(color.NRGBA)
			if !ok {
				c = color.NRGBAModel.Convert(p.Palette[v]).(color.NRGBA)
			}
			count(c.A == 0, c.R|c.G|c.B != 0)
		}
	default:
		t.Fatalf("hiddenUnderAlpha cannot read %T", m)
	}
	return coloured, transparent
}

// An editor that erases part of a picture by clearing its alpha can leave the
// erased colour behind, readable by anyone who drops the alpha channel. None
// of it is stored, in any format that keeps transparency, and every pixel that
// can be seen is kept exactly where the format is lossless.
func TestColourUnderTransparentPixelsIsNotStored(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	eight := image.NewNRGBA(image.Rect(0, 0, 32, 24))
	sixteen := image.NewNRGBA64(image.Rect(0, 0, 32, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			if x < 16 { // transparent, holding a colour
				eight.SetNRGBA(x, y, color.NRGBA{uint8(1 + rng.Intn(255)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 0})
				sixteen.SetNRGBA64(x, y, color.NRGBA64{uint16(1 + rng.Intn(65535)), uint16(rng.Intn(65536)), uint16(rng.Intn(65536)), 0})
				continue
			}
			// Seen, some of it partly transparent, which keeps its colour.
			eight.SetNRGBA(x, y, color.NRGBA{200, uint8(8 * y), 30, uint8(100 + 4*x)})
			sixteen.SetNRGBA64(x, y, color.NRGBA64{50000, uint16(2000 * y), 3000, uint16(30000 + 1000*x)})
		}
	}
	for _, tc := range []struct {
		name     string
		upload   []byte
		lossless bool
	}{
		{"PNG, 8 bits a channel", pngOf(t, eight), true},
		{"PNG, 16 bits a channel", pngOf(t, sixteen), true},
		{"lossless WebP", readTestdata(t, "hidden-lossless.webp"), true},
		{"lossy WebP with alpha", readTestdata(t, "hidden-lossy-alpha.webp"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, _, err := image.Decode(bytes.NewReader(tc.upload))
			if err != nil {
				t.Fatalf("the fixture does not decode: %v", err)
			}
			if coloured, transparent := hiddenUnderAlpha(t, src); coloured == 0 || coloured != transparent {
				t.Fatalf("%d of the fixture's %d transparent pixels hold a colour; want all, or the test proves nothing", coloured, transparent)
			}
			stored := decodeStored(t, process(t, tc.upload).Data)
			coloured, transparent := hiddenUnderAlpha(t, stored)
			if transparent == 0 {
				t.Fatal("the stored file has no transparent pixels; it should keep the upload's")
			}
			if coloured != 0 {
				t.Errorf("%d of the stored file's %d transparent pixels still hold a colour", coloured, transparent)
			}
			if tc.lossless {
				requireSameAppearance(t, src, stored)
			}
		})
	}
}

// A partly transparent pixel barely shows, yet a re-encode that keeps the
// picture exact carries its full colour through untouched: a photo laid under
// an alpha of 1 composites to one faint tint but keeps every bit of its three
// channels, there to read for anyone who drops the alpha. So each pixel's
// colour is cut to the precision its alpha can show before it is stored
// (reduceHiddenColour): what composites to the screen stays the same, but the
// colour hidden beneath it does not survive.
func TestPartlyTransparentColourIsReducedToItsAlpha(t *testing.T) {
	// All a re-encode shows of a pixel is its premultiplied colour, so
	// premultiplying and reading back at the same alpha is exactly what
	// survives; a low alpha reads back only a few of the 256 (or 65536) values.
	keptAt8 := func(c, a uint8) uint8 {
		if a == 0 {
			return 0
		}
		p := (int(c)*int(a) + 127) / 255
		return uint8((p*255 + int(a)/2) / int(a))
	}
	keptAt16 := func(c, a uint16) uint16 {
		if a == 0 {
			return 0
		}
		p := (uint32(c)*uint32(a) + 0x7FFF) / 0xFFFF
		return uint16((p*0xFFFF + uint32(a)/2) / uint32(a))
	}

	rng := rand.New(rand.NewSource(7))
	alphas := []uint8{1, 3, 7, 64, 200, 255}
	eight := image.NewNRGBA(image.Rect(0, 0, len(alphas), 1))
	origEight := make([]color.NRGBA, len(alphas))
	for x, a := range alphas {
		c := color.NRGBA{uint8(1 + rng.Intn(255)), uint8(1 + rng.Intn(255)), uint8(1 + rng.Intn(255)), a}
		origEight[x] = c
		eight.SetNRGBA(x, 0, c)
	}
	origSixteen := color.NRGBA64{40000, 55000, 12345, 3}
	sixteen := image.NewNRGBA64(image.Rect(0, 0, 1, 1))
	sixteen.SetNRGBA64(0, 0, origSixteen)
	uploadEight, uploadSixteen := pngOf(t, eight), pngOf(t, sixteen)

	// Positive control: the fixtures really keep the full colour under each
	// alpha, so a stored colour that differs proves the reduction happened.
	up8, ok := decodeStored(t, uploadEight).(*image.NRGBA)
	if !ok {
		t.Fatal("the 8-bit fixture does not decode to NRGBA")
	}
	for x := range alphas {
		if up8.NRGBAAt(x, 0) != origEight[x] {
			t.Fatalf("the 8-bit fixture lost colour at x=%d before storing; the test would prove nothing", x)
		}
	}
	up16, ok := decodeStored(t, uploadSixteen).(*image.NRGBA64)
	if !ok {
		t.Fatal("the 16-bit fixture does not decode to NRGBA64")
	}
	if up16.NRGBA64At(0, 0) != origSixteen {
		t.Fatal("the 16-bit fixture lost colour before storing; the test would prove nothing")
	}

	stored8, ok := decodeStored(t, process(t, uploadEight).Data).(*image.NRGBA)
	if !ok {
		t.Fatal("the stored 8-bit file is not NRGBA")
	}
	reduced := false
	for x, a := range alphas {
		want := color.NRGBA{keptAt8(origEight[x].R, a), keptAt8(origEight[x].G, a), keptAt8(origEight[x].B, a), a}
		if got := stored8.NRGBAAt(x, 0); got != want {
			t.Errorf("stored 8-bit pixel at alpha %d = %v, want %v (colour cut to what the alpha shows)", a, got, want)
		}
		if a != 0xFF && want != origEight[x] {
			reduced = true
		}
	}
	if !reduced {
		t.Fatal("no partly transparent pixel's colour was reduced; the fixture proves nothing")
	}

	stored16, ok := decodeStored(t, process(t, uploadSixteen).Data).(*image.NRGBA64)
	if !ok {
		t.Fatal("the stored 16-bit file is not NRGBA64")
	}
	want16 := color.NRGBA64{keptAt16(origSixteen.R, 3), keptAt16(origSixteen.G, 3), keptAt16(origSixteen.B, 3), 3}
	if want16 == origSixteen {
		t.Fatal("the 16-bit fixture was not reduced; it proves nothing")
	}
	if got := stored16.NRGBA64At(0, 0); got != want16 {
		t.Errorf("stored 16-bit pixel = %v, want %v (colour cut to what the alpha shows)", got, want16)
	}
}

// A paletted picture can hide a file not in its palette but in its pixels: with
// 256 identical entries the palette shows one flat colour and gives nothing
// away, yet which of the 256 equal entries each pixel points at is a free byte
// a reader recovers by looking at the indices alone. Rebuilding the palette
// from the colours the pixels show collapses the equal entries to one and
// points every pixel at it, so the indices carry nothing (cleanPaletted,
// cleanGIF).
func TestFlatPaletteCannotHideAFileInItsPixels(t *testing.T) {
	hidden := cameraJPEG(t, 16, 16) // a whole JPEG, EXIF and GPS and all
	side := 1
	for side*side < len(hidden) {
		side++
	}
	pal := make(color.Palette, 256)
	for i := range pal {
		pal[i] = color.RGBA{0x80, 0x80, 0x80, 0xFF} // every entry the same grey
	}
	base := image.NewPaletted(image.Rect(0, 0, side, side), pal)
	copy(base.Pix, hidden) // the pixel indices are the file's bytes

	distinctIndices := func(m *image.Paletted) int {
		var seen [256]bool
		n := 0
		for _, v := range m.Pix {
			if !seen[v] {
				seen[v], n = true, n+1
			}
		}
		return n
	}
	needles := []string{"Exif", needleCamera, needleGPS, needleDatum}

	for _, tc := range []struct {
		name   string
		encode func() []byte
		decode func([]byte) *image.Paletted
	}{
		{"PNG", func() []byte { return pngOf(t, base) }, func(b []byte) *image.Paletted {
			p, ok := decodeStored(t, b).(*image.Paletted)
			if !ok {
				t.Fatal("stored PNG is not paletted")
			}
			return p
		}},
		{"GIF", func() []byte {
			var buf bytes.Buffer
			if err := gif.Encode(&buf, base, nil); err != nil {
				t.Fatalf("encode gif: %v", err)
			}
			return buf.Bytes()
		}, func(b []byte) *image.Paletted {
			g, err := gif.DecodeAll(bytes.NewReader(b))
			if err != nil {
				t.Fatalf("decode stored gif: %v", err)
			}
			return g.Image[0]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upload := tc.encode()
			// Positive control: the file is recoverable from the upload's
			// pixel indices, read as bytes.
			up := tc.decode(upload)
			if !bytes.Equal(up.Pix[:len(hidden)], hidden) {
				t.Fatal("the upload's indices are not the hidden file; the test would prove nothing")
			}
			requireCarries(t, up.Pix, needles...)

			stored := tc.decode(process(t, upload).Data)
			if n := distinctIndices(stored); n != 1 {
				t.Errorf("stored picture uses %d distinct palette indices, want 1", n)
			}
			requireFree(t, stored.Pix, needles...)
		})
	}
}

// A palette entry no pixel uses is not shown, so whatever it holds — here a
// sentence in the colours of unused PLTE entries and another in their tRNS
// alphas — is not stored. Two transparent entries become one, holding no
// colour, and the palette ends at its last used entry.
func TestPNGPaletteKeepsOnlyWhatIsShown(t *testing.T) {
	pal := color.Palette{
		color.NRGBA{10, 20, 30, 255},     // 0: seen
		color.NRGBA{'h', 'i', 'd', 0},    // 1: transparent, holding a colour
		color.NRGBA{200, 100, 50, 128},   // 2: partly transparent, seen
		color.NRGBA{'e', 'n', '!', 0},    // 3: transparent too
		color.NRGBA{250, 250, 250, 255},  // 4: unused
		color.NRGBA{'x', 'y', 'z', 0x80}, // 5: unused
	}
	colours, alphas := []byte(needlePalette), []byte(needleTRNS)
	for i := 0; i+3 <= len(colours); i += 3 {
		pal = append(pal, color.NRGBA{colours[i], colours[i+1], colours[i+2], alphas[(i/3)%len(alphas)]})
	}
	m := image.NewPaletted(image.Rect(0, 0, 12, 8), pal)
	for i := range m.Pix {
		m.Pix[i] = []uint8{0, 1, 2, 3}[i%4]
	}
	upload := pngOf(t, m)
	requireCarries(t, upload, needlePalette, needleTRNS[:len(colours)/3])

	got := process(t, upload)
	requireFree(t, got.Data, needlePalette, needleTRNS[:8], "hid", "en!", "xyz")
	stored, ok := decodeStored(t, got.Data).(*image.Paletted)
	if !ok {
		t.Fatal("the stored PNG is not paletted any more")
	}
	if len(stored.Palette) != 3 {
		t.Errorf("stored palette has %d entries, want the 3 its pixels show", len(stored.Palette))
	}
	if coloured, transparent := hiddenUnderAlpha(t, stored); transparent == 0 || coloured != 0 {
		t.Errorf("%d of %d transparent pixels hold a colour", coloured, transparent)
	}
	requireSameAppearance(t, m, stored)
}

// gifTables reads back a GIF that image/gif wrote: its global colour table,
// and whether each frame carries a table of its own.
func gifTables(t *testing.T, data []byte) (global []byte, local []bool) {
	t.Helper()
	i := 13
	if flags := data[10]; flags&0x80 != 0 {
		n := 3 << (1 + flags&0x07)
		global, i = data[i:i+n], i+n
	}
	subBlocks := func(i int) int {
		for data[i] != 0 {
			i += 1 + int(data[i])
		}
		return i + 1
	}
	for {
		switch data[i] {
		case 0x21: // every extension image/gif writes is a run of sub-blocks
			i = subBlocks(i + 2)
		case 0x2C:
			flags := data[i+9]
			local = append(local, flags&0x80 != 0)
			i += 10
			if flags&0x80 != 0 {
				i += 3 << (1 + flags&0x07)
			}
			i = subBlocks(i + 1)
		case 0x3B:
			return global, local
		default:
			t.Fatalf("unexpected block %#x at %d", data[i], i)
		}
	}
}

// A GIF's colour tables keep only the colours its frames show. Here the
// unused entries of the global table and of a frame's own table each carry a
// sentence, the global table's entry for a frame's transparent colour holds a
// colour nobody draws, and two one-pixel frames carry a camera's JPEG, EXIF
// and GPS position and all, in tables they use one entry of. None of it is
// stored; every frame shows the same pixels, and the frames that used the
// global table still do, rather than each getting a table of its own.
func TestGIFColourTablesKeepOnlyWhatIsShown(t *testing.T) {
	seen := []color.Color{
		color.RGBA{220, 30, 30, 255}, color.RGBA{30, 200, 30, 255},
		color.RGBA{'T', 'R', 'N', 255}, // only ever the transparent colour
		color.RGBA{30, 30, 220, 255},
	}
	tableOf := func(seen []color.Color, needle []byte) color.Palette {
		p := append(color.Palette(nil), seen...)
		for i := 0; len(p) < 256; i += 3 {
			p = append(p, color.RGBA{needle[i%len(needle)], needle[(i+1)%len(needle)], needle[(i+2)%len(needle)], 255})
		}
		return p
	}
	global := tableOf(seen, []byte(needleGlobal))
	frame := func(p color.Palette, w, h int, pixels ...uint8) *image.Paletted {
		m := image.NewPaletted(image.Rect(0, 0, w, h), p)
		for i := range m.Pix {
			m.Pix[i] = pixels[i%len(pixels)]
		}
		return m
	}
	clearAt2 := append(color.Palette(nil), global...)
	clearAt2[2] = color.RGBA{}
	g := &gif.GIF{
		Config: image.Config{ColorModel: global, Width: 16, Height: 8},
		Image: []*image.Paletted{
			frame(global, 16, 8, 0, 1),                                 // the global table itself
			frame(clearAt2, 16, 8, 2, 3, 0),                            // a copy, with entry 2 transparent
			frame(tableOf(seen[:2], []byte(needleLocal)), 16, 8, 1, 0), // a table of its own
		},
	}
	hidden := cameraJPEG(t, 16, 16)
	for at := 0; at < len(hidden); at += 768 {
		chunk := hidden[at:min(at+768, len(hidden))]
		p := make(color.Palette, 256)
		for i := range p {
			p[i] = color.RGBA{A: 255}
			if 3*i+2 < len(chunk) {
				p[i] = color.RGBA{chunk[3*i], chunk[3*i+1], chunk[3*i+2], 255}
			}
		}
		g.Image = append(g.Image, frame(p, 1, 1, 0))
	}
	for range g.Image {
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("encode: %v", err)
	}
	upload := buf.Bytes()
	needles := []string{needleGlobal[:20], needleLocal[:20], "TRN", "Exif", needleCamera, needleGPS, needleDatum, needleXMP, needleComment}
	requireCarries(t, upload, needles...)
	original, err := gif.DecodeAll(bytes.NewReader(upload))
	if err != nil {
		t.Fatalf("the fixture does not decode: %v", err)
	}
	if _, local := gifTables(t, upload); local[0] || local[1] || !local[2] {
		t.Fatalf("the fixture's frames carry their own tables %v; want the first two on the global one", local)
	}

	got := process(t, upload)
	requireFree(t, got.Data, needles...)
	stored, err := gif.DecodeAll(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatalf("decode stored gif: %v", err)
	}
	if len(stored.Image) != len(original.Image) {
		t.Fatalf("stored %d frames, want %d", len(stored.Image), len(original.Image))
	}
	for i := range original.Image {
		requireSamePixels(t, original.Image[i], stored.Image[i])
	}
	table, local := gifTables(t, got.Data)
	if local[0] || local[1] {
		t.Errorf("frames that used the global table carry their own now: %v", local)
	}
	if want := []byte{220, 30, 30, 30, 200, 30, 0, 0, 0, 30, 30, 220}; !bytes.Equal(table, want) {
		t.Errorf("stored global table = % x, want the colours shown and black for the transparent one: % x", table, want)
	}
	if len(got.Data) >= len(upload)/2 {
		t.Errorf("stored %d bytes of a %d-byte upload; the unused tables should be gone", len(got.Data), len(upload))
	}
}

// gifWithPixels is a w×1 GIF89a with a two-colour global table, red and
// green, whose one frame marks transparent an index past the end of that
// table, as some encoders do: browsers draw it transparent, and image/gif
// pads the palette with transparent entries up to it (golang.org/issue/15059).
func gifWithPixels(t *testing.T, transparent byte, pixels ...uint8) []byte {
	t.Helper()
	b := []byte("GIF89a")
	b = append(b, byte(len(pixels)), 0, 1, 0, 0x80, 0, 0) // a 2-entry global table
	b = append(b, 255, 0, 0, 0, 255, 0)
	b = append(b, 0x21, 0xF9, 4, 1, 0, 0, transparent, 0)
	b = append(b, 0x2C, 0, 0, 0, 0, byte(len(pixels)), 0, 1, 0, 0)
	var lzwData bytes.Buffer
	w := lzw.NewWriter(&lzwData, lzw.LSB, 2)
	if _, err := w.Write(pixels); err != nil {
		t.Fatalf("lzw: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("lzw: %v", err)
	}
	b = append(b, 2, byte(lzwData.Len()))
	b = append(b, lzwData.Bytes()...)
	return append(b, 0, 0x3B)
}

// Pixels on a transparent index past the end of a frame's colour table stay
// transparent: image/gif's encoder marks only the first of the entries
// image/gif padded the palette with as transparent, and the pixels on the
// others were stored opaque black.
func TestGIFTransparentIndexPastItsTable(t *testing.T) {
	for _, pixels := range [][]uint8{{0, 1, 3, 3}, {0, 1, 2, 3}, {3, 1, 0, 2}} {
		upload := gifWithPixels(t, 3, pixels...)
		original, err := gif.DecodeAll(bytes.NewReader(upload))
		if err != nil {
			t.Fatalf("%v: the fixture does not decode: %v", pixels, err)
		}
		clearPixels := 0
		for x := range pixels {
			if _, _, _, a := original.Image[0].At(x, 0).RGBA(); a == 0 {
				clearPixels++
			}
		}
		if clearPixels != 2 {
			t.Fatalf("%v: the fixture has %d transparent pixels, want 2", pixels, clearPixels)
		}
		got := process(t, upload)
		stored, err := gif.DecodeAll(bytes.NewReader(got.Data))
		if err != nil {
			t.Fatalf("%v: decode stored gif: %v", pixels, err)
		}
		requireSamePixels(t, original.Image[0], stored.Image[0])
	}
}
