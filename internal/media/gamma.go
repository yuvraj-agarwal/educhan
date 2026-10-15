package media

import (
	"encoding/binary"
	"image"
	"image/color"
	"math"
)

// A PNG can say in a gAMA chunk what gamma its samples were encoded with,
// and browsers show it accordingly: a picture tagged 1.0, the linear light a
// renderer or ImageMagick writes, shows its mid-grey of 128 at about 188.
// image/png ignores the chunk and the stored file has none, so a picture
// stored with its samples as they were would show far darker than it did.
// The samples are converted instead, to the sRGB values that show the same
// light, which is what a browser takes an untagged PNG's samples to be.
//
// Chromium is the reference: it takes a sample s of a picture tagged with
// gamma g to linear light s^(1/g), and that to sRGB with the sRGB curve,
// which TestPNGGammaIsShownAsBrowsersShowIt holds to within a level of what
// Chromium 141 drew; and it leaves alone a gamma within 5% of 1/2.2, which
// is libpng's threshold, an sRGB chunk, which says the samples are sRGB
// whatever gAMA says, and an ICC profile, which it reads instead. A profile
// is dropped here, as from a JPEG, with no conversion (sanitizeJPEG says
// why), and so are the primaries of a cHRM chunk, which change colours but
// not greys and are rarer still.

// pngGamma reads the gamma a PNG's samples are to be converted from: the
// first gAMA chunk before the first IDAT, as Chromium reads it, when it
// names a gamma a browser applies and no sRGB or iCCP chunk takes its place.
func pngGamma(data []byte) (gamma float64, ok bool) {
	var (
		value   uint32
		tagged  bool // a gAMA chunk has been read
		managed bool // an sRGB or iCCP chunk says what the samples are instead
	)
	i := 8 // the signature
	for i+8 <= len(data) {
		n := int64(binary.BigEndian.Uint32(data[i : i+4]))
		if n > int64(len(data)-i-12) {
			break
		}
		typ := string(data[i+4 : i+8])
		if typ == "IDAT" {
			break
		}
		switch typ {
		case "gAMA":
			if !tagged && n == 4 {
				tagged, value = true, binary.BigEndian.Uint32(data[i+8:i+12])
			}
		case "sRGB", "iCCP":
			managed = true
		}
		i += 12 + int(n)
	}
	if !tagged || managed || value == 0 {
		return 0, false
	}
	gamma = float64(value) / 100000
	if math.Abs(gamma*2.2-1) <= 0.05 {
		return 0, false
	}
	return gamma, true
}

// sRGBEncode takes linear light, from 0 to 1, to the sRGB value that shows
// it, from 0 to 1.
func sRGBEncode(l float64) float64 {
	if l <= 0.0031308 {
		return 12.92 * l
	}
	return 1.055*math.Pow(l, 1/2.4) - 0.055
}

// gammaTable maps every sample value from 0 to top of a picture encoded with
// gamma to the sRGB value, at the same depth, that shows the same light.
func gammaTable(gamma float64, top int) []uint16 {
	t := make([]uint16, top+1)
	for v := range t {
		l := math.Pow(float64(v)/float64(top), 1/gamma)
		t[v] = uint16(math.Round(float64(top) * sRGBEncode(l)))
	}
	return t
}

// applyPNGGamma converts, in place, the colour samples of a picture as
// image/png decodes one from a file encoded with gamma to sRGB (gammaTable).
// Alpha is not a colour and is left as it is. image/png decodes an opaque
// truecolour picture as image.RGBA and image.RGBA64, whose alpha is always
// full, so their premultiplied samples are the colour itself.
func applyPNGGamma(m image.Image, gamma float64) {
	var t8 []uint16
	eight := func() []uint16 {
		if t8 == nil {
			t8 = gammaTable(gamma, 0xFF)
		}
		return t8
	}
	// colour8 and colour16 convert the first three samples of every pixel
	// of size bytes in pix, at 8 and 16 bits.
	colour8 := func(pix []uint8, size int) {
		t := eight()
		for i := 0; i+size <= len(pix); i += size {
			pix[i+0], pix[i+1], pix[i+2] = uint8(t[pix[i+0]]), uint8(t[pix[i+1]]), uint8(t[pix[i+2]])
		}
	}
	sixteen := func(pix []uint8, size, samples int) {
		t := gammaTable(gamma, 0xFFFF)
		for i := 0; i+size <= len(pix); i += size {
			for s := i; s < i+2*samples; s += 2 {
				binary.BigEndian.PutUint16(pix[s:], t[binary.BigEndian.Uint16(pix[s:])])
			}
		}
	}
	switch p := m.(type) {
	case *image.Gray:
		t := eight()
		for i, v := range p.Pix {
			p.Pix[i] = uint8(t[v])
		}
	case *image.RGBA:
		colour8(p.Pix, 4)
	case *image.NRGBA:
		colour8(p.Pix, 4)
	case *image.Gray16:
		sixteen(p.Pix, 2, 1)
	case *image.RGBA64:
		sixteen(p.Pix, 8, 3)
	case *image.NRGBA64:
		sixteen(p.Pix, 8, 3)
	case *image.Paletted:
		t := eight()
		for k, c := range p.Palette {
			n := color.NRGBAModel.Convert(c).(color.NRGBA)
			p.Palette[k] = color.NRGBA{uint8(t[n.R]), uint8(t[n.G]), uint8(t[n.B]), n.A}
		}
	}
}
