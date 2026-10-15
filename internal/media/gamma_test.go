package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
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

// Browsers show a PNG by its gAMA chunk, and the stored file has none, so a
// gamma browsers apply is applied to the samples instead: a picture tagged
// with the linear gamma of 1.0 would otherwise be stored with its mid-grey,
// which showed at about 188, at 128. The want rows are what Chromium 141
// drew for a grey ramp under each tag, at columns 0, 16, 32, 64, 96, 128,
// 160, 192, 224 and 255; the stored ramp is held to within a level of them.
// Chromium leaves alone a gamma within 5% of 1/2.2, an sRGB or iCCP chunk
// beside it, a gAMA after the pixels and a gAMA of 0, and reads the first of
// two; so is each here.
func TestPNGGammaIsShownAsBrowsersShowIt(t *testing.T) {
	ramp := greyRampPNG(t)
	columns := []int{0, 16, 32, 64, 96, 128, 160, 192, 224, 255}
	untouched := []uint8{0, 16, 32, 64, 96, 128, 160, 192, 224, 255}
	iccp := pngChunk("iCCP", []byte("x\x00\x00not a profile"))
	for _, tc := range []struct {
		name string
		data []byte
		want []uint8
	}{
		{"gamma 1.0", withPNGChunks(ramp, gAMAChunk(100000)), []uint8{0, 71, 99, 137, 165, 188, 208, 225, 241, 255}},
		{"gamma 1/1.8", withPNGChunks(ramp, gAMAChunk(55556)), []uint8{0, 20, 43, 81, 115, 146, 176, 203, 230, 255}},
		{"gamma 0.5", withPNGChunks(ramp, gAMAChunk(50000)), []uint8{0, 13, 34, 71, 105, 137, 168, 198, 227, 255}},
		{"gamma 2.0", withPNGChunks(ramp, gAMAChunk(200000)), []uint8{0, 137, 161, 188, 205, 219, 230, 240, 248, 255}},
		{"gamma 0.431, just past 5%", withPNGChunks(ramp, gAMAChunk(43100)), []uint8{0, 5, 22, 57, 91, 124, 157, 190, 223, 255}},
		{"gamma 1/2.2", withPNGChunks(ramp, gAMAChunk(45455)), untouched},
		{"gamma 0.432, within 5%", withPNGChunks(ramp, gAMAChunk(43200)), untouched},
		{"gamma 0.476, within 5%", withPNGChunks(ramp, gAMAChunk(47600)), untouched},
		{"gamma 1.0 beside sRGB", withPNGChunks(ramp, gAMAChunk(100000), pngChunk("sRGB", []byte{0})), untouched},
		{"gamma 1.0 beside a profile", withPNGChunks(ramp, gAMAChunk(100000), iccp), untouched},
		{"gamma 1.0 after the pixels", beforeChunk(t, ramp, "IEND", gAMAChunk(100000)), untouched},
		{"gamma 0", withPNGChunks(ramp, gAMAChunk(0)), untouched},
		{"gamma 1.0, then 1/2.2", withPNGChunks(ramp, gAMAChunk(100000), gAMAChunk(45455)), []uint8{0, 71, 99, 137, 165, 188, 208, 225, 241, 255}},
		{"gamma 1/2.2, then 1.0", withPNGChunks(ramp, gAMAChunk(45455), gAMAChunk(100000)), untouched},
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
