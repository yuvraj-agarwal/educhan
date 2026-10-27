package media

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
	"testing"
)

// greyRampPNG is a 256×4 grey PNG whose column x has the value x.
func greyRampPNG(t *testing.T) []byte {
	t.Helper()
	m := image.NewGray(image.Rect(0, 0, 256, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 256; x++ {
			m.Pix[y*m.Stride+x] = uint8(x)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// gAMAChunk is a gAMA chunk naming gamma×100000.
func gAMAChunk(value uint32) []byte {
	return pngChunk("gAMA", binary.BigEndian.AppendUint32(nil, value))
}

// beforeChunk splices chunks in before the first chunk of type typ.
func beforeChunk(t *testing.T, p []byte, typ string, chunks ...[]byte) []byte {
	t.Helper()
	for i := 8; i+8 <= len(p); {
		if string(p[i+4:i+8]) == typ {
			out := append([]byte(nil), p[:i]...)
			for _, c := range chunks {
				out = append(out, c...)
			}
			return append(out, p[i:]...)
		}
		i += 12 + int(binary.BigEndian.Uint32(p[i:i+4]))
	}
	t.Fatalf("no %s chunk", typ)
	return nil
}

// iccTag is a tag of an ICC profile: its signature and what it holds.
type iccTag struct {
	sig  string
	data []byte
}

// iccProfile is an ICC profile of colour space space and the XYZ connection
// space with a D50 illuminant, holding tags in the order given; header edits
// what is written of its header before the tag count.
func iccProfile(space string, header func(h []byte), tags ...iccTag) []byte {
	h := make([]byte, 128)
	copy(h[4:], "none")
	binary.BigEndian.PutUint32(h[8:], 0x02100000)
	copy(h[12:], "mntr")
	copy(h[16:], space)
	copy(h[20:], "XYZ ")
	copy(h[36:], "acsp")
	for k, v := range []float64{0.9642, 1, 0.8249} {
		binary.BigEndian.PutUint32(h[68+4*k:], uint32(int32(v*65536+0.5)))
	}
	table := binary.BigEndian.AppendUint32(nil, uint32(len(tags)))
	var body []byte
	at := 132 + 12*len(tags)
	for _, tag := range tags {
		for len(body)%4 != 0 {
			body = append(body, 0)
		}
		table = append(table, tag.sig...)
		table = binary.BigEndian.AppendUint32(table, uint32(at+len(body)))
		table = binary.BigEndian.AppendUint32(table, uint32(len(tag.data)))
		body = append(body, tag.data...)
	}
	binary.BigEndian.PutUint32(h[0:], uint32(128+len(table)+len(body)))
	if header != nil {
		header(h)
	}
	return append(append(h, table...), body...)
}

// iccFixed is v as an ICC s15Fixed16Number.
func iccFixed(v float64) []byte {
	return binary.BigEndian.AppendUint32(nil, uint32(int32(math.Round(v*65536))))
}

// iccXYZ is an XYZ tag; iccPara is a parametric curve of function type kind.
func iccXYZ(x, y, z float64) []byte {
	return append(append(append([]byte("XYZ \x00\x00\x00\x00"), iccFixed(x)...), iccFixed(y)...), iccFixed(z)...)
}

func iccPara(kind uint16, params ...float64) []byte {
	b := binary.BigEndian.AppendUint16([]byte("para\x00\x00\x00\x00"), kind)
	b = append(b, 0, 0)
	for _, v := range params {
		b = append(b, iccFixed(v)...)
	}
	return b
}

// sRGBProfile is an RGB profile with the sRGB tone curve and primaries, as
// skcms reads it: Chromium shows a picture under it as it is.
func sRGBProfile(trc []byte, extra ...iccTag) []byte {
	if trc == nil {
		trc = iccPara(3, 2.4, 1/1.055, 0.055/1.055, 1/12.92, 0.04045)
	}
	tags := append([]iccTag{
		{"rTRC", trc}, {"gTRC", trc}, {"bTRC", trc},
		{"rXYZ", iccXYZ(0.4361, 0.2225, 0.0139)}, {"gXYZ", iccXYZ(0.3851, 0.7169, 0.0971)},
		{"bXYZ", iccXYZ(0.1431, 0.0606, 0.7141)}, {"wtpt", iccXYZ(0.9642, 1, 0.8249)},
	}, extra...)
	return iccProfile("RGB ", nil, tags...)
}

// deflated is b as a zlib stream; iCCPChunk is an iCCP chunk of the given
// name and compression method holding a stream.
func deflated(b []byte) []byte {
	var z bytes.Buffer
	w, _ := zlib.NewWriterLevel(&z, zlib.BestSpeed)
	w.Write(b)
	w.Close()
	return z.Bytes()
}

func iCCPChunk(name string, method byte, stream []byte) []byte {
	return pngChunk("iCCP", append(append([]byte(name), 0, method), stream...))
}

// Browsers show a PNG by its gAMA chunk, and the stored file has none, so a
// gamma browsers apply is applied to the samples instead: a picture tagged
// with the linear gamma of 1.0 would otherwise be stored with its mid-grey,
// which showed at about 188, at 128. The want rows are what Chromium 141
// drew for a grey ramp under each tag, at columns 0, 16, 32, 64, 96, 128,
// 160, 192, 224 and 255; the stored ramp is held to within a level of them.
// Chromium leaves alone a gamma within 5% of 1/2.2, an sRGB chunk or a
// profile beside it, a gAMA after the pixels and a gAMA of 0, and reads the
// first of two; so is each here. It passes over an sRGB chunk whose
// rendering intent it does not know and a profile it cannot read, and
// applies the gamma beside them, so each kind of those is here too, with a
// profile that differs from a sound one in one thing.
func TestPNGGammaIsShownAsBrowsersShowIt(t *testing.T) {
	ramp := greyRampPNG(t)
	columns := []int{0, 16, 32, 64, 96, 128, 160, 192, 224, 255}
	untouched := []uint8{0, 16, 32, 64, 96, 128, 160, 192, 224, 255}
	linear := []uint8{0, 71, 99, 137, 165, 188, 208, 225, 241, 255}
	beside := func(chunks ...[]byte) []byte {
		return withPNGChunks(ramp, append([][]byte{gAMAChunk(100000)}, chunks...)...)
	}
	profile := sRGBProfile(nil)
	sound := deflated(profile)
	// edited is a copy of b changed by edit.
	edited := func(b []byte, edit func(b []byte)) []byte {
		b = append([]byte(nil), b...)
		edit(b)
		return b
	}
	curve := iccPara(3, 2.4, 1/1.055, 0.055/1.055, 1/12.92, 0.04045)
	iccp := func(p []byte) []byte { return iCCPChunk("x", 0, deflated(p)) }
	grey := func(edit func([]byte), tags ...iccTag) []byte { return iccp(iccProfile("GRAY", edit, tags...)) }
	for _, tc := range []struct {
		name string
		data []byte
		want []uint8
	}{
		{"gamma 1.0", withPNGChunks(ramp, gAMAChunk(100000)), linear},
		{"gamma 1/1.8", withPNGChunks(ramp, gAMAChunk(55556)), []uint8{0, 20, 43, 81, 115, 146, 176, 203, 230, 255}},
		{"gamma 0.5", withPNGChunks(ramp, gAMAChunk(50000)), []uint8{0, 13, 34, 71, 105, 137, 168, 198, 227, 255}},
		{"gamma 2.0", withPNGChunks(ramp, gAMAChunk(200000)), []uint8{0, 137, 161, 188, 205, 219, 230, 240, 248, 255}},
		{"gamma 0.431, just past 5%", withPNGChunks(ramp, gAMAChunk(43100)), []uint8{0, 5, 22, 57, 91, 124, 157, 190, 223, 255}},
		{"gamma 1/2.2", withPNGChunks(ramp, gAMAChunk(45455)), untouched},
		{"gamma 0.432, within 5%", withPNGChunks(ramp, gAMAChunk(43200)), untouched},
		{"gamma 0.476, within 5%", withPNGChunks(ramp, gAMAChunk(47600)), untouched},
		{"gamma 1.0 after the pixels", beforeChunk(t, ramp, "IEND", gAMAChunk(100000)), untouched},
		{"gamma 0", withPNGChunks(ramp, gAMAChunk(0)), untouched},
		{"gamma 1.0, then 1/2.2", withPNGChunks(ramp, gAMAChunk(100000), gAMAChunk(45455)), linear},
		{"gamma 1/2.2, then 1.0", withPNGChunks(ramp, gAMAChunk(45455), gAMAChunk(100000)), untouched},

		{"gamma 1.0 beside sRGB", beside(pngChunk("sRGB", []byte{0})), untouched},
		{"gamma 1.0 beside sRGB of intent 3", beside(pngChunk("sRGB", []byte{3})), untouched},
		{"gamma 1.0 beside sRGB of two bytes", beside(pngChunk("sRGB", []byte{0, 0})), untouched},
		{"gamma 1.0 beside sRGB of intent 4", beside(pngChunk("sRGB", []byte{4})), linear},
		{"gamma 1.0 beside sRGB of intent 7", beside(pngChunk("sRGB", []byte{7})), linear},
		{"gamma 1.0 beside sRGB of no bytes", beside(pngChunk("sRGB", nil)), linear},
		{"gamma 1.0 beside sRGB of intent 7, then of 0", beside(pngChunk("sRGB", []byte{7}), pngChunk("sRGB", []byte{0})), untouched},

		{"gamma 1.0 beside a profile", beside(iCCPChunk("x", 0, sound)), untouched},
		{"gamma 1.0 beside a profile named in 80 bytes", beside(iCCPChunk(strings.Repeat("n", 80), 0, sound)), untouched},
		{"gamma 1.0 beside a profile with bytes after its stream", beside(iCCPChunk("x", 0, append(sound, "junk"...))), untouched},
		{"gamma 1.0 beside a profile after one that is not", beside(iccp([]byte("not a profile")), iccp(profile)), linear},
		{"gamma 1.0 beside a profile that does not inflate", beside(pngChunk("iCCP", []byte("x\x00\x00not a profile"))), linear},
		{"gamma 1.0 beside a profile whose checksum is wrong", beside(iCCPChunk("x", 0, edited(sound, func(z []byte) { z[len(z)-1] ^= 1 }))), linear},
		{"gamma 1.0 beside a profile cut short", beside(iCCPChunk("x", 0, sound[:len(sound)-6])), linear},
		{"gamma 1.0 beside a profile with no name", beside(iCCPChunk("", 0, sound)), linear},
		{"gamma 1.0 beside a profile named in 81 bytes", beside(iCCPChunk(strings.Repeat("n", 81), 0, sound)), linear},
		{"gamma 1.0 beside a profile of compression method 1", beside(iCCPChunk("x", 1, sound)), linear},
		{"gamma 1.0 beside a profile of more than 64 MiB", beside(iccp(append(profile, make([]byte, maxICCProfile)...))), linear},
		{"gamma 1.0 beside a profile that is a header alone", beside(grey(nil)), linear},
		{"gamma 1.0 beside a profile that is not acsp", beside(iccp(edited(profile, func(p []byte) { p[39] = 'q' }))), linear},
		{"gamma 1.0 beside a profile of version 5", beside(grey(func(h []byte) { h[8] = 5 }, iccTag{"kTRC", curve})), linear},
		{"gamma 1.0 beside a profile not lit by D50", beside(grey(func(h []byte) { copy(h[68:], iccFixed(0.95)) }, iccTag{"kTRC", curve})), linear},
		{"gamma 1.0 beside a profile in Lab", beside(grey(func(h []byte) { copy(h[20:], "Lab ") }, iccTag{"kTRC", curve})), linear},
		{"gamma 1.0 beside a profile larger than it is", beside(grey(func(h []byte) { binary.BigEndian.PutUint32(h, binary.BigEndian.Uint32(h)+4) }, iccTag{"kTRC", curve})), linear},
		{"gamma 1.0 beside a profile with a tag of 3 bytes", beside(grey(nil, iccTag{"kTRC", curve}, iccTag{"desc", []byte("abc")})), linear},
		{"gamma 1.0 beside a grey profile with no curve", beside(grey(nil, iccTag{"wtpt", iccXYZ(0.9642, 1, 0.8249)})), linear},
		{"gamma 1.0 beside a profile whose curve is cut short", beside(iccp(sRGBProfile([]byte("curv\x00\x00\x00\x00\x00\x00\x00\x03\x00\x00\x00\x80")))), linear},
		{"gamma 1.0 beside a profile of a negative gamma", beside(iccp(sRGBProfile(iccPara(0, -2.2)))), linear},
		{"gamma 1.0 beside a profile of a curve of type 5", beside(iccp(sRGBProfile(iccPara(5, 2.2)))), linear},
		{"gamma 1.0 beside a profile of a curve with a of 0", beside(iccp(sRGBProfile(iccPara(1, 2.2, 0, 0)))), linear},
		{"gamma 1.0 beside a profile of a colourant of 16 bytes", beside(iccp(iccProfile("RGB ", nil, iccTag{"rTRC", curve}, iccTag{"gTRC", curve}, iccTag{"bTRC", curve}, iccTag{"rXYZ", iccXYZ(0.4361, 0.2225, 0.0139)}, iccTag{"gXYZ", iccXYZ(0.3851, 0.7169, 0.0971)}, iccTag{"bXYZ", iccXYZ(0.1431, 0.0606, 0.7141)[:16]}))), linear},
		{"gamma 1.0 beside a profile with a cicp tag of 8 bytes", beside(iccp(sRGBProfile(nil, iccTag{"cicp", []byte("cicp\x00\x00\x00\x00")}))), linear},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := decodeStored(t, process(t, tc.data).Data)
			for k, x := range columns {
				r, _, _, _ := stored.At(x, 1).RGBA()
				if got := int(r >> 8); got-int(tc.want[k]) > 1 || int(tc.want[k])-got > 1 {
					t.Errorf("column %d stored as %d, want %d as Chromium shows it", x, got, tc.want[k])
				}
			}
		})
	}
}

// Every kind of picture image/png decodes is converted the same way, colour
// only: a palette's entries, colour with and without alpha, and 16-bit
// samples at 16 bits, with alpha and the picture's form left as they were.
func TestPNGGammaConvertsColourOnly(t *testing.T) {
	const gamma = 1.0
	t8, t16 := gammaTable(gamma, 0xFF), gammaTable(gamma, 0xFFFF)
	tag := gAMAChunk(100000)
	encode := func(m image.Image) []byte {
		var buf bytes.Buffer
		if err := png.Encode(&buf, m); err != nil {
			t.Fatalf("encode: %v", err)
		}
		return buf.Bytes()
	}

	rgb := image.NewRGBA(image.Rect(0, 0, 256, 1))
	nrgba := image.NewNRGBA(image.Rect(0, 0, 256, 1))
	grey16 := image.NewGray16(image.Rect(0, 0, 256, 1))
	pal := image.NewPaletted(image.Rect(0, 0, 256, 1), nil)
	for x := 0; x < 256; x++ {
		rgb.SetRGBA(x, 0, color.RGBA{uint8(x), uint8(255 - x), 100, 0xFF})
		nrgba.SetNRGBA(x, 0, color.NRGBA{uint8(x), 30, 200, 0xFF})
		grey16.SetGray16(x, 0, color.Gray16{uint16(x) * 257})
		pal.Palette = append(pal.Palette, color.RGBA{uint8(x), uint8(x / 2), 40, 0xFF})
		pal.Pix[x] = uint8(x)
	}
	nrgba.SetNRGBA(10, 0, color.NRGBA{200, 30, 200, 0x80}) // half transparent

	t.Run("RGB", func(t *testing.T) {
		got := decodeStored(t, process(t, withPNGChunks(encode(rgb), tag)).Data)
		for x := 0; x < 256; x++ {
			want := color.RGBA{uint8(t8[x]), uint8(t8[255-x]), uint8(t8[100]), 0xFF}
			if c := color.RGBAModel.Convert(got.At(x, 0)); c != want {
				t.Fatalf("pixel %d = %v, want %v", x, c, want)
			}
		}
	})
	t.Run("RGBA", func(t *testing.T) {
		got := decodeStored(t, process(t, withPNGChunks(encode(nrgba), tag)).Data)
		for _, x := range []int{0, 64, 128, 255} {
			want := color.NRGBA{uint8(t8[x]), uint8(t8[30]), uint8(t8[200]), 0xFF}
			if c := color.NRGBAModel.Convert(got.At(x, 0)); c != want {
				t.Errorf("pixel %d = %v, want %v", x, c, want)
			}
		}
		if _, _, _, a := got.At(10, 0).RGBA(); a>>8 != 0x80 {
			t.Errorf("the half-transparent pixel has alpha %d, want 128 as it was", a>>8)
		}
	})
	t.Run("16-bit grey", func(t *testing.T) {
		got, ok := decodeStored(t, process(t, withPNGChunks(encode(grey16), tag)).Data).(*image.Gray16)
		if !ok {
			t.Fatal("not stored as 16-bit grey")
		}
		for x := 0; x < 256; x++ {
			if v, want := got.Gray16At(x, 0).Y, t16[x*257]; v != want {
				t.Fatalf("pixel %d = %d, want %d", x, v, want)
			}
		}
	})
	t.Run("palette", func(t *testing.T) {
		data := beforeChunk(t, encode(pal), "IDAT", tag) // after PLTE, where Chromium still reads it
		got := decodeStored(t, process(t, data).Data)
		for _, x := range []int{0, 1, 77, 200, 255} {
			want := color.RGBA{uint8(t8[x]), uint8(t8[x/2]), uint8(t8[40]), 0xFF}
			if c := color.RGBAModel.Convert(got.At(x, 0)); c != want {
				t.Errorf("pixel %d = %v, want %v", x, c, want)
			}
		}
	})
}
