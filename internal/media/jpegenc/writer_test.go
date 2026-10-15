package jpegenc

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// ratios is every chroma subsampling image/jpeg decodes, with the luma
// sampling factors it is written with.
var ratios = []struct {
	ratio image.YCbCrSubsampleRatio
	h, v  int
}{
	{image.YCbCrSubsampleRatio444, 1, 1},
	{image.YCbCrSubsampleRatio422, 2, 1},
	{image.YCbCrSubsampleRatio420, 2, 2},
	{image.YCbCrSubsampleRatio440, 1, 2},
	{image.YCbCrSubsampleRatio411, 4, 1},
	{image.YCbCrSubsampleRatio410, 4, 2},
}

// sizes are pictures that fill their units exactly, and pictures whose last
// unit, and last chroma sample, hang over the edge.
var sizes = []image.Point{{64, 32}, {37, 29}, {1, 1}, {9, 17}}

// pattern is a YCbCr picture of the ratio with detail in every plane: each
// sample its own value, so a plane written at the wrong resolution, or read
// back from the wrong place, cannot come out close.
func pattern(w, h int, ratio image.YCbCrSubsampleRatio) *image.YCbCr {
	m := image.NewYCbCr(image.Rect(0, 0, w, h), ratio)
	for i := range m.Y {
		x, y := i%m.YStride, i/m.YStride
		m.Y[i] = uint8(40 + (x*7+y*13)%170)
	}
	for i := range m.Cb {
		x, y := i%m.CStride, i/m.CStride
		m.Cb[i] = uint8(60 + (x*11+y*5)%130)
		m.Cr[i] = uint8(70 + (x*3+y*17)%120)
	}
	return m
}

// frameSampling reads the sampling factors of each component from the frame
// header Encode writes.
func frameSampling(t *testing.T, data []byte) []byte {
	t.Helper()
	i := 2
	for i+4 <= len(data) {
		length := int(binary.BigEndian.Uint16(data[i+2:]))
		if data[i+1] == sof0Marker {
			sof := data[i+4 : i+2+length]
			var out []byte
			for k := 0; k < int(sof[5]); k++ {
				out = append(out, sof[7+3*k])
			}
			return out
		}
		i += 2 + length
	}
	t.Fatal("no frame header")
	return nil
}

// TestFourTwoZeroIsImageJPEGsOwn holds the copy to the encoder it came from:
// a 4:2:0 picture and a grey one are written byte for byte as image/jpeg
// writes them, at every quality from 1 to 100, whatever their size. Those are
// the paths it kept as they were, so this also covers what both share: the
// tables, the transform, the Huffman coding and the headers.
func TestFourTwoZeroIsImageJPEGsOwn(t *testing.T) {
	for _, size := range sizes {
		ycc := pattern(size.X, size.Y, image.YCbCrSubsampleRatio420)
		grey := image.NewGray(ycc.Rect)
		copy(grey.Pix, ycc.Y)
		for _, m := range []image.Image{ycc, grey} {
			for q := 1; q <= 100; q++ {
				var ours, theirs bytes.Buffer
				if err := Encode(&ours, m, &Options{Quality: q}); err != nil {
					t.Fatal(err)
				}
				if err := jpeg.Encode(&theirs, m, &jpeg.Options{Quality: q}); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(ours.Bytes(), theirs.Bytes()) {
					t.Errorf("%T %v at quality %d: %d bytes, image/jpeg wrote %d different ones",
						m, size, q, ours.Len(), theirs.Len())
				}
			}
		}
	}
}

// TestEverySubsamplingIsKept encodes a picture of every subsampling and
// decodes it with image/jpeg: it comes back at the subsampling it went in at,
// every plane close to what was written.
func TestEverySubsamplingIsKept(t *testing.T) {
	for _, r := range ratios {
		for _, size := range sizes {
			src := pattern(size.X, size.Y, r.ratio)
			var buf bytes.Buffer
			if err := Encode(&buf, src, &Options{Quality: 100}); err != nil {
				t.Fatal(err)
			}
			if got, want := frameSampling(t, buf.Bytes()), []byte{byte(r.h<<4 | r.v), 0x11, 0x11}; !bytes.Equal(got, want) {
				t.Errorf("%v %v: sampling factors %x, want %x", r.ratio, size, got, want)
			}
			dec, err := jpeg.Decode(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("%v %v: %v", r.ratio, size, err)
			}
			got, ok := dec.(*image.YCbCr)
			if !ok || got.SubsampleRatio != r.ratio || got.Rect != src.Rect {
				t.Fatalf("%v %v: decoded as %T %v", r.ratio, size, dec, dec.Bounds())
			}
			// Quality 100 quantises every coefficient by 1, so what is left
			// is the rounding of the transforms. The decoder's planes are
			// padded to whole units, so each sample is found by the pixels
			// it covers.
			bad := 0
			for y := 0; y < size.Y; y++ {
				for x := 0; x < size.X; x++ {
					gy, wy := got.YOffset(x, y), src.YOffset(x, y)
					gc, wc := got.COffset(x, y), src.COffset(x, y)
					for _, d := range []int{
						int(got.Y[gy]) - int(src.Y[wy]),
						int(got.Cb[gc]) - int(src.Cb[wc]),
						int(got.Cr[gc]) - int(src.Cr[wc]),
					} {
						if d < -2 || d > 2 {
							bad++
						}
					}
				}
			}
			if bad > 0 {
				t.Errorf("%v %v: %d samples off by more than 2", r.ratio, size, bad)
			}
		}
	}
}

// TestColourIsKeptAtFullResolution is the reason for the package. Red text
// on blue, one-pixel strokes, written from an RGBA picture: image/jpeg's
// 4:2:0 averages each stroke with the blue beside it, and this encoder's
// 4:4:4 keeps it.
func TestColourIsKeptAtFullResolution(t *testing.T) {
	m := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			c := color.RGBA{0, 40, 200, 255}
			if x%4 == 1 || y%6 == 2 {
				c = color.RGBA{230, 20, 20, 255}
			}
			m.SetRGBA(x, y, c)
		}
	}
	worst := func(data []byte) int {
		dec, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		most := 0
		for y := 0; y < 64; y++ {
			for x := 0; x < 64; x++ {
				r, _, b, _ := dec.At(x, y).RGBA()
				w := m.RGBAAt(x, y)
				most = max(most, abs(int(r>>8)-int(w.R)), abs(int(b>>8)-int(w.B)))
			}
		}
		return most
	}
	var ours, theirs bytes.Buffer
	if err := Encode(&ours, m, &Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&theirs, m, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	if got := frameSampling(t, ours.Bytes()); !bytes.Equal(got, []byte{0x11, 0x11, 0x11}) {
		t.Fatalf("RGBA written with sampling factors %x, want 4:4:4", got)
	}
	o, th := worst(ours.Bytes()), worst(theirs.Bytes())
	if th < 100 {
		t.Fatalf("image/jpeg's 4:2:0 is off by %d at worst; the picture no longer shows what it loses", th)
	}
	if o > 40 {
		t.Errorf("4:4:4 is off by %d at worst (image/jpeg's 4:2:0 by %d)", o, th)
	}
}

// TestQuantTablesAreTheOnesWritten reads the tables Encode writes, at every
// quality, back out of its file.
func TestQuantTablesAreTheOnesWritten(t *testing.T) {
	m := image.NewGray(image.Rect(0, 0, 8, 8))
	for q := -5; q <= 105; q++ {
		var buf bytes.Buffer
		if err := Encode(&buf, m, &Options{Quality: q}); err != nil {
			t.Fatal(err)
		}
		data := buf.Bytes()
		// SOI, then the DQT: marker, length, and two tables of an id byte
		// and 64 values each.
		if data[2] != 0xff || data[3] != dqtMarker {
			t.Fatalf("quality %d: no DQT after SOI", q)
		}
		tables := QuantTables(q)
		for i := range tables {
			at := 6 + i*(1+blockSize)
			if data[at] != byte(i) || !bytes.Equal(data[at+1:at+1+blockSize], tables[i][:]) {
				t.Errorf("quality %d: table %d written is not QuantTables'", q, i)
			}
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
