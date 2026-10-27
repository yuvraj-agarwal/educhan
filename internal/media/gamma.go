package media

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"image"
	"image/color"
	"io"
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
// not greys and are rarer still. An sRGB chunk or a profile Chromium cannot
// read takes nothing's place, though, and it applies the gAMA beside it, so
// they are read as it reads them: an sRGB chunk by its rendering intent
// (pngGamma), and a profile by whether it inflates and skcms, Chromium's
// colour engine, would take it (pngProfileUsable). A cICP chunk, which
// Chromium reads before all of these, is not read here, so a picture tagged
// with one Chromium reads and with a gAMA is converted by the gAMA, where
// Chromium shows it by the cICP.

// pngGamma reads the gamma a PNG's samples are to be converted from: the
// first gAMA chunk before the first IDAT, as Chromium reads it, when it
// names a gamma a browser applies and nothing takes its place. An sRGB chunk
// does when its rendering intent, its first byte, is one of the four there
// are, whatever follows it, and an iCCP chunk when it holds a profile
// Chromium uses (pngProfileUsable). Chromium reads every sRGB chunk until
// one will do but only the first iCCP that is not empty, so a profile that
// will not do keeps a later one from being read.
func pngGamma(data []byte) (gamma float64, ok bool) {
	var (
		value   uint32
		tagged  bool   // a gAMA chunk has been read
		srgb    bool   // an sRGB chunk says the samples are sRGB
		profile []byte // the first iCCP chunk's data that is not empty
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
		case "sRGB":
			if n >= 1 && data[i+8] <= 3 {
				srgb = true
			}
		case "iCCP":
			if profile == nil && n > 0 {
				profile = data[i+8 : i+8+int(n)]
			}
		}
		i += 12 + int(n)
	}
	if !tagged || srgb || value == 0 {
		return 0, false
	}
	gamma = float64(value) / 100000
	if math.Abs(gamma*2.2-1) <= 0.05 {
		return 0, false
	}
	if profile != nil && pngProfileUsable(profile) {
		return 0, false
	}
	return gamma, true
}

// maxICCProfile is the most a PNG's ICC profile may inflate to here for
// Chromium to be taken to read it. Chromium's PNG decoder takes a profile
// from a 64 MiB budget that it takes its own buffers from too, and passes
// over a profile that does not fit, as it does one that does not inflate; a
// picture whose buffers then no longer fit beside the profile it does not
// show at all. What its buffers left fell short of 64 MiB by more than the
// compressed chunk in both files Chromium 141 was shown: from a 256×4
// picture whose iCCP chunk was 291 KB it passed over a profile 472 KiB short
// of 64 MiB and used one 488 KiB short, and from a 6.5 MB upload whose
// profile held 6 MiB of random bytes it passed over one of 57 MiB and used
// one of 56 MiB. The cut here is at 64 MiB, so a profile between Chromium's
// cut and it, a band of megabytes for a large upload, is taken as used here,
// where Chromium passes it over and applies the gAMA, or does not show the
// picture.
const maxICCProfile = 64 << 20

// iccProfileRead is how much of a PNG's ICC profile pngProfileUsable keeps
// to read: a profile is a few hundred bytes to a few dozen kilobytes, and
// one larger than this is read only as far as its header.
const iccProfileRead = 256 << 10

// pngProfileUsable reports whether the data of an iCCP chunk holds a profile
// Chromium uses, as its PNG decoder and skcms read one: a name of one to 80
// bytes and its NUL, compression method 0, and a zlib stream that inflates
// whole, checksum and all, to no more than maxICCProfile, holding a profile
// iccProfileUsable takes. Chromium's inflater refuses a stream whose header
// asks for a preset dictionary, which compress/zlib takes when the
// dictionary it names is the empty one, so that is looked for first; one
// that asks for a window over 32 KiB, which both refuse, is left to
// compress/zlib. The stream is inflated to its end, so whatever it holds
// past what is kept is counted and let go.
func pngProfileUsable(chunk []byte) bool {
	name := bytes.IndexByte(chunk[:min(len(chunk), 81)], 0)
	if name < 1 || name+3 >= len(chunk) || chunk[name+1] != 0 {
		return false
	}
	if chunk[name+3]&0x20 != 0 { // FDICT, in the stream's second byte
		return false
	}
	z, err := zlib.NewReader(bytes.NewReader(chunk[name+2:]))
	if err != nil {
		return false
	}
	p, k := make([]byte, iccProfileRead), 0
	for k < len(p) && err == nil {
		var m int
		m, err = z.Read(p[k:])
		k += m
	}
	n := int64(k)
	if err == nil {
		// p is full: the rest is counted, as far as the limit and a byte.
		n, err = io.Copy(io.Discard, io.LimitReader(z, maxICCProfile-n+1))
		n += int64(k)
	}
	if err != nil && err != io.EOF || n > maxICCProfile {
		return false
	}
	return iccProfileUsable(p[:k], n)
}

// iccProfileUsable reports whether skcms_Parse, which is how Chromium reads
// an ICC profile, takes the profile of n bytes that p begins: a header it
// can read, with the signature acsp, a size no larger than the profile and
// room for its tag table, a major version of 4 or less, a D50 illuminant and
// an XYZ or Lab connection space; tags that each lie inside the size and are
// four bytes or more; tone curves (kTRC for grey, or rTRC, gTRC and bTRC)
// and colourant tags (rXYZ, gXYZ and bXYZ) of the types skcms reads,
// wherever a profile has them, and a cicp tag of the type and size it reads,
// if it has one; and either a tone curve for every channel and the
// colourants, or a curve for grey in the XYZ space, or a lookup table (A2B0
// or A2B1). The lookup tables themselves are not read, nor is a profile past
// the header when it is longer than iccProfileRead, which p then stops at: a
// table or a tag skcms finds malformed is taken as sound here, where
// Chromium would pass the profile over and apply the gAMA.
func iccProfileUsable(p []byte, n int64) bool {
	be := binary.BigEndian
	if len(p) < 132 {
		return false
	}
	fixed := func(b []byte) float32 { return float32(int32(be.Uint32(b))) * (1.0 / 65536) }
	size, tags := int64(be.Uint32(p[0:])), int64(be.Uint32(p[128:]))
	if string(p[36:40]) != "acsp" || size > n || size < 132+12*tags || p[8] > 4 {
		return false
	}
	for k, white := range [3]float32{0.9642, 1, 0.8249} {
		if d := fixed(p[68+4*k:]) - white; d > 0.01 || d < -0.01 {
			return false
		}
	}
	pcs := string(p[20:24])
	if pcs != "XYZ " && pcs != "Lab " {
		return false
	}
	if size > int64(len(p)) {
		return true
	}
	p = p[:size]
	table := p[132 : 132+12*tags]
	for e := table; len(e) > 0; e = e[12:] {
		if off, n := int64(be.Uint32(e[4:])), int64(be.Uint32(e[8:])); n < 4 || off+n > size {
			return false
		}
	}
	tag := func(sig string) []byte {
		for e := table; len(e) > 0; e = e[12:] {
			if string(e[:4]) == sig {
				off := be.Uint32(e[4:])
				return p[off : off+be.Uint32(e[8:])]
			}
		}
		return nil
	}
	var curves, colourants bool
	if k := tag("kTRC"); string(p[16:20]) == "GRAY" && k != nil {
		if !iccCurve(k) {
			return false
		}
		curves, colourants = true, pcs == "XYZ "
	} else {
		if r, g, b := tag("rTRC"), tag("gTRC"), tag("bTRC"); r != nil && g != nil && b != nil {
			if !iccCurve(r) || !iccCurve(g) || !iccCurve(b) {
				return false
			}
			curves = true
		}
		if r, g, b := tag("rXYZ"), tag("gXYZ"), tag("bXYZ"); r != nil && g != nil && b != nil {
			for _, c := range [][]byte{r, g, b} {
				if string(c[:4]) != "XYZ " || len(c) < 20 {
					return false
				}
			}
			colourants = true
		}
	}
	if c := tag("cicp"); c != nil && (string(c[:4]) != "cicp" || len(c) < 12) {
		return false
	}
	return tag("A2B0") != nil || tag("A2B1") != nil || curves && colourants
}

// iccCurve reports whether skcms reads a profile's tone curve: a curv of as
// many 16-bit entries as it says, or a para of one of the five function
// types whose parameters make a curve it can use, with g, a, c and d not
// negative and a·d + b not negative, d being -b/a for the types that do not
// give it.
func iccCurve(c []byte) bool {
	be := binary.BigEndian
	if len(c) < 12 {
		return false
	}
	switch string(c[:4]) {
	case "curv":
		return int64(len(c)) >= 12+2*int64(be.Uint32(c[8:]))
	case "para":
		kind := be.Uint16(c[8:])
		if kind > 4 || len(c) < 12+[...]int{4, 12, 16, 20, 28}[kind] {
			return false
		}
		fixed := func(o int) float32 { return float32(int32(be.Uint32(c[o:]))) * (1.0 / 65536) }
		g, a, b, cc, d := fixed(12), float32(1), float32(0), float32(0), float32(0)
		switch kind {
		case 1, 2:
			a, b = fixed(16), fixed(20)
			if a == 0 {
				return false
			}
			d = -b / a
		case 3, 4:
			a, b, cc, d = fixed(16), fixed(20), fixed(24), fixed(28)
		}
		return g >= 0 && a >= 0 && cc >= 0 && d >= 0 && float32(a*d)+b >= 0
	}
	return false
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
