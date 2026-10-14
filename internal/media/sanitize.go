package media

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/webp"

	"github.com/yuvraj-agarwal/educhan/internal/media/jpegenc"
)

// What is stored as an upload's original is never the file as uploaded. A
// photo carries more than its pixels: EXIF (the camera, the time to the
// second, and often the GPS position it was taken at), XMP (the same again,
// plus an editor's history), IPTC captions and credits, comments, and colour
// profiles that can name the device. Anyone who opened the full image could
// read all of it. So every upload is rewritten here, in Process, before either
// storage backend sees a byte of it:
//
//   - JPEG is decoded, turned upright by its EXIF orientation, and re-encoded
//     at its own chroma subsampling, and at its own quality up to
//     storedJPEGQuality.
//   - PNG is decoded, turned upright by the EXIF orientation of an eXIf chunk,
//     and re-encoded, which keeps none of its ancillary chunks.
//   - GIF is decoded frame by frame and re-encoded with its frames, delays,
//     disposal and loop count, and none of its comments or other application
//     data.
//   - WebP is decoded and stored as a PNG, or as a JPEG when it is lossy and
//     opaque, because nothing in the standard library or x/image can encode
//     WebP (sanitizeWebP says why it is not kept as it came).
//
// So the stored file is always one Go encoded from decoded pixels, and holds
// nothing of the upload's own bytes. It is the one decoded for the thumbnail
// and hashed for the checksum, so all three describe the same picture.
//
// A decoded picture can still hold what nobody sees: the colour of a fully
// transparent pixel, which an editor that erases part of a picture by
// clearing its alpha often leaves behind, and palette entries no pixel uses.
// Either could carry the erased part of a photo, or a whole second file, EXIF
// and all, through to the stored original. So the colour of every fully
// transparent pixel is cleared (clearTransparentColour), and a palette keeps
// only the entries its pixels use, with one transparent entry at most
// (cleanPaletted, cleanGIF). What is left is the picture, kept exactly in a
// lossless format -- which is not the same as keeping only what the eye picks
// out. A pixel at alpha 1 keeps its full colour, a 16-bit channel keeps its
// low byte, and which palette entry a pixel points at is kept even where two
// entries are the same colour; so data put into the pixels on purpose, as
// low-bit steganography does, survives. Removing it would alter the picture,
// which no rewrite that keeps the picture exact can do: the job here is to
// drop metadata, not to launder the pixels.

// storedJPEGQuality is the highest quality an original is stored at as a
// JPEG, and the quality of one made from a lossy WebP. A JPEG upload saved at
// a higher quality is stored at this one, which keeps its detail while
// shrinking the file; one saved at a lower quality is stored at its own
// (jpegQuality). Either way it keeps its chroma subsampling (jpegenc), so
// colour that was saved at full resolution is stored at it.
const storedJPEGQuality = 90

// jpegQuality is the quality sanitizeJPEG stores a JPEG at: the lowest at
// which jpegenc quantises no coefficient of any component more coarsely than
// the upload's own tables did, and storedJPEGQuality at most. A picture
// re-encoded at finer steps than it was saved with stores its compression's
// own blocks and ringing in more detail and shows nothing more, at about
// twice the size; re-encoded at the same steps, its coefficients
// mostly come back as they were, and the file at about its own size
// (TestLowQualityJPEGsAreNotStoredLarger). An upload saved with the standard
// tables, as most software saves, is stored at exactly its own quality.
//
// The tables are read, never copied: all the stored file takes from them is
// a quality from 1 to 90, the same tables at it that any upload would get,
// so they cannot carry anything through. A file whose tables are not all
// there before its first scan, or whose components are not YCbCr or grey
// (CMYK, RGB), is stored at storedJPEGQuality. A picture turned on its side
// has its blocks transposed, so it is held to the tables transposed.
func jpegQuality(f jpegFrame, framed bool, orientation int) int {
	if !framed || !f.tables || (f.comps != 1 && f.comps != 3) || (f.comps == 3 && f.rgb()) {
		return storedJPEGQuality
	}
	for c := 0; c < f.comps; c++ {
		if f.tq[c] > 3 || !f.defined[f.tq[c]] {
			return storedJPEGQuality
		}
	}
	transposed := orientation >= 5
	for q := 1; q < storedJPEGQuality; q++ {
		ours := jpegenc.QuantTables(q)
		fine := true
		for c := 0; c < f.comps && fine; c++ {
			theirs, table := &f.quant[f.tq[c]], &ours[min(c, 1)]
			for k, step := range table {
				at := k
				if transposed {
					at = transposedZig(k)
				}
				if uint16(step) > theirs[at] {
					fine = false
					break
				}
			}
		}
		if fine {
			return q
		}
	}
	return storedJPEGQuality
}

// transposedZig is the zig-zag position a block's coefficient at zig-zag
// position k moves to when the block is transposed. Zig-zag order runs along
// the block's diagonals, the first one holding one coefficient, the next
// two, and so on to eight and back down to one, and transposing a block
// keeps each coefficient on its diagonal and runs the diagonal the other way.
func transposedZig(k int) int {
	start := 0
	for d := 0; d < 15; d++ {
		n := min(d, 14-d) + 1
		if k < start+n {
			return 2*start + n - 1 - k
		}
		start += n
	}
	return k
}

// maxGIFFramePixels bounds the pixels of all a GIF's frames together. The
// header check holds each frame to the canvas, and the canvas to MaxPixels,
// but every frame is decoded and kept at a byte a pixel until it is
// re-encoded, and a frame of one flat colour compresses to almost nothing: a
// few megabytes of GIF could otherwise ask for gigabytes. Four
// canvases' worth is a long animation at any ordinary size (a 640×480 GIF can
// have 300 frames) and no more memory than the largest JPEG.
const maxGIFFramePixels = 4 * MaxPixels

// storedBuffer collects an encoded original and refuses to hold more than
// limit bytes (storedLimit), with ErrStoredTooLarge. It grows by doubling, as a
// bytes.Buffer does, but never past the limit, so it holds at most twice the
// limit while it grows: what processingCost counts for it.
type storedBuffer struct {
	buf   []byte
	limit int64
}

func newStoredBuffer(sizeHint int, limit int64) *storedBuffer {
	return &storedBuffer{buf: make([]byte, 0, min(int64(sizeHint), limit)), limit: limit}
}

func (b *storedBuffer) Write(p []byte) (int, error) {
	need := int64(len(b.buf)) + int64(len(p))
	if need > b.limit {
		return 0, ErrStoredTooLarge
	}
	if need > int64(cap(b.buf)) {
		grown := make([]byte, len(b.buf), min(max(2*int64(cap(b.buf)), need, 4096), b.limit))
		copy(grown, b.buf)
		b.buf = grown
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// encoded is the result of encoding into a storedBuffer: its bytes, or
// ErrStoredTooLarge if the limit stopped it (image/jpeg, image/png and
// image/gif all hand back their writer's error as it was), or the encoder's
// own error.
func (b *storedBuffer) encoded(format Format, err error) ([]byte, error) {
	switch {
	case err == nil:
		return b.buf, nil
	case errors.Is(err, ErrStoredTooLarge):
		return nil, ErrStoredTooLarge
	}
	return nil, fmt.Errorf("media: encode %s: %w", format.Name, err)
}

// sanitized is an upload rewritten for storage.
type sanitized struct {
	format Format          // what data is, which for a WebP upload is PNG or JPEG
	data   []byte          // the bytes to store
	pic    image.Image     // what to thumbnail: the picture, or a GIF's first frame
	canvas image.Rectangle // the whole picture, which pic may only partly cover
}

// sanitize rewrites an upload for storage. The rewritten file may not be
// longer than in.limit (storedLimit).
func sanitize(in inspection, data []byte) (sanitized, error) {
	switch in.format.Name {
	case FormatJPEG.Name:
		return sanitizeJPEG(data, in.limit)
	case FormatPNG.Name:
		return sanitizePNG(data, in.limit)
	case FormatGIF.Name:
		return sanitizeGIF(data, in.limit, in.frames)
	case FormatWebP.Name:
		return sanitizeWebP(data, in.limit)
	}
	return sanitized{}, ErrUnsupportedFormat
}

// sanitizeJPEG decodes a JPEG, applies its EXIF orientation to the pixels, and
// re-encodes it at its own chroma subsampling (jpegenc) and its own quality
// up to storedJPEGQuality (jpegQuality). jpegenc, like the image/jpeg encoder
// it is a copy of, writes no APP or COM segments at all, so EXIF, XMP, IPTC
// and Photoshop data, comments, and the ICC profile are all gone from the
// result.
//
// Dropping the ICC profile is a trade-off. Most photos are sRGB, which is what
// a browser assumes of a JPEG with no profile, and they look the same. A
// wide-gamut photo (Display P3 from a recent phone, Adobe RGB from a camera)
// loses its tag and its colours shift slightly, usually towards duller. That
// is accepted because a profile is not just colour: a calibrated one is
// specific to one screen or one camera, and editors write their own names into
// it. Converting the pixels to sRGB properly would need a colour engine the
// standard library does not have. The same goes for the colour chunks of a PNG
// and the ICCP chunk of a WebP.
//
// A CMYK JPEG is converted to RGB where it lies as soon as it is decoded,
// which is what the encoder would do a pixel at a time, through an interface
// value each, and what turning it upright would do the same way.
func sanitizeJPEG(data []byte, limit int64) (sanitized, error) {
	src, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	if cmyk, ok := src.(*image.CMYK); ok {
		src = rgbaOfCMYK(cmyk)
	}
	o := jpegOrientation(data)
	pic := orient(src, o)
	f, framed := readJPEGFrame(data)
	buf := newStoredBuffer(len(data), limit)
	stored, err := buf.encoded(FormatJPEG, jpegenc.Encode(buf, pic, &jpegenc.Options{Quality: jpegQuality(f, framed, o)}))
	if err != nil {
		return sanitized{}, err
	}
	return sanitized{format: FormatJPEG, data: stored, pic: pic, canvas: pic.Bounds()}, nil
}

// rgbaOfCMYK converts a CMYK picture to RGBA with color.CMYKToRGB, the
// conversion color.CMYK's own RGBA method makes. Both are four bytes a pixel,
// so each pixel is converted where it lies and the CMYK picture is used up.
func rgbaOfCMYK(m *image.CMYK) *image.RGBA {
	b := m.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := m.Pix[m.PixOffset(b.Min.X, y):]
		for i := 0; i < 4*b.Dx(); i += 4 {
			row[i], row[i+1], row[i+2] = color.CMYKToRGB(row[i], row[i+1], row[i+2], row[i+3])
			row[i+3] = 0xFF
		}
	}
	return &image.RGBA{Pix: m.Pix, Stride: m.Stride, Rect: m.Rect}
}

// sanitizePNG decodes and re-encodes a PNG. image/png writes only IHDR, PLTE
// and tRNS for a palette, IDAT and IEND, so text of every kind (tEXt, zTXt,
// and iTXt, which is where XMP goes), eXIf, tIME, colour chunks and anything
// private are left behind. The pixel values are kept exactly as image/png
// decodes them, which is not always the form they came in: 1-, 2- and 4-bit
// grey is stored at 8 bits, and grey or colour with a transparent colour key
// (tRNS) is stored as colour with an alpha channel, or with none when no pixel
// has the key's colour, while 16-bit pictures stay 16-bit
// (TestPNGIsStoredInTheFormItDecodesTo). A palette keeps only the entries its
// pixels use, so it may be stored at a lower bit depth, and fully transparent
// pixels hold no colour. A picture with an EXIF orientation in an eXIf chunk
// is turned upright first, as for a JPEG, because browsers show it turned.
// An animated PNG keeps its still image, which is what a viewer without APNG
// support has always shown.
func sanitizePNG(data []byte, limit int64) (sanitized, error) {
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	clearTransparentColour(src)
	pic := orient(src, pngOrientation(data))
	buf := newStoredBuffer(len(data), limit)
	stored, err := buf.encoded(FormatPNG, png.Encode(buf, pic))
	if err != nil {
		return sanitized{}, err
	}
	return sanitized{format: FormatPNG, data: stored, pic: pic, canvas: pic.Bounds()}, nil
}

// sanitizeGIF decodes every frame of a GIF and encodes them again. The frames
// and their pixels, positions, transparency, delays and disposal, and the loop
// count all survive; comment extensions and every application extension (XMP,
// for one) do not, because image/gif reads past them and never writes them,
// but for the loop count, which it writes as NETSCAPE2.0 whichever of the two
// loop extensions it came in (gifFramePixels reads it). The colour tables keep
// only the colours the pixels use (cleanGIF). The thumbnail is of the first
// frame.
func sanitizeGIF(data []byte, limit int64, frames gifFrames) (sanitized, error) {
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	if frames.looped {
		g.LoopCount = frames.loop
	}
	cleanGIF(g)
	buf := newStoredBuffer(len(data), limit)
	stored, err := buf.encoded(FormatGIF, gif.EncodeAll(buf, g))
	if err != nil {
		return sanitized{}, err
	}
	return sanitized{
		format: FormatGIF,
		data:   stored,
		pic:    g.Image[0],
		canvas: image.Rect(0, 0, g.Config.Width, g.Config.Height),
	}, nil
}

// sanitizeWebP checks a WebP's structure and strips it to the chunks that
// make the picture (stripWebP), decodes what is left, and stores the pixels as
// a new file: a JPEG when the picture is lossy and opaque, which is what a
// lossy WebP is closest to, and otherwise a PNG, which keeps a lossless
// picture exactly and a transparent one's alpha. A lossy picture's colours are
// taken from the limited-range YCbCr x/image decodes to back to full-range RGB
// first (lossyWebPToNRGBA), or black and white would be stored as grey. The
// JPEG is at
// storedJPEGQuality whatever the WebP was saved at: a WebP's quality setting
// is not a JPEG's, so its own is no guide, and a WebP saved at a low quality
// can be stored at two to four times its size. An animated WebP is refused
// as it always has been; x/image decodes still images only.
//
// Stripping alone is not enough to store. A decoder stops reading a VP8, VP8L
// or ALPH chunk once it has its picture and ignores whatever else the chunk
// holds, so a whole second file — another image, its own EXIF and all — can
// sit inside a chunk that stripping has to keep, and would be stored and
// served byte for byte. Nobody would see it on screen, moderators and image
// matching included, which is what makes it a hiding place. Encoding the
// decoded pixels, with no colour kept under transparent ones, leaves only
// what is shown, as for the other formats.
func sanitizeWebP(data []byte, limit int64) (sanitized, error) {
	stripped, info, err := stripWebP(data)
	if err != nil {
		return sanitized{}, err
	}
	if info.Animated {
		return sanitized{}, ErrCorruptImage
	}
	pic, err := webp.Decode(bytes.NewReader(stripped))
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	if b := pic.Bounds(); b.Dx() != info.Width || b.Dy() != info.Height {
		return sanitized{}, ErrCorruptImage
	}

	format := FormatPNG
	buf := newStoredBuffer(len(data), limit)
	switch pic.(type) {
	case *image.YCbCr, *image.NYCbCrA: // lossy: YCbCr, with an alpha plane or not
		rgb, opaque := lossyWebPToNRGBA(pic)
		pic = rgb
		if opaque {
			format = FormatJPEG
			err = jpegenc.Encode(buf, rgb, &jpegenc.Options{Quality: storedJPEGQuality})
		} else {
			err = png.Encode(buf, rgb)
		}
	default: // lossless, decoded as *image.NRGBA
		clearTransparentColour(pic)
		err = png.Encode(buf, pic)
	}
	stored, err := buf.encoded(format, err)
	if err != nil {
		return sanitized{}, err
	}
	return sanitized{format: format, data: stored, pic: pic, canvas: pic.Bounds()}, nil
}

// clip8 clamps an integer to a uint8.
func clip8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// bt601LimitedToRGB converts one limited-range ("studio swing") BT.601 Y'CbCr
// sample to full-range 8-bit RGB. golang.org/x/image/webp decodes a lossy
// WebP to an image.YCbCr whose samples libwebp wrote with the limited-range
// matrix — black is Y=16, white Y=235, and the chroma span is 224, not 255 —
// and image.YCbCr reads them back with the JFIF full-range formula, which
// leaves black at (16,16,16) and white at (235,235,235) and washes out every
// colour. These are the standard BT.601 limited-range coefficients (298/256 ≈
// 1.164, 409 ≈ 1.596, 100 ≈ 0.391, 208 ≈ 0.813, 516 ≈ 2.017), so black
// returns to 0, white to 255 and a primary to its full value.
func bt601LimitedToRGB(y, cb, cr uint8) (r, g, b uint8) {
	yy := (int(y) - 16) * 298
	u := int(cb) - 128
	v := int(cr) - 128
	return clip8((yy + 409*v + 128) >> 8),
		clip8((yy - 100*u - 208*v + 128) >> 8),
		clip8((yy + 516*u + 128) >> 8)
}

// lossyWebPToNRGBA converts a lossy WebP — decoded as image.YCbCr when it is
// opaque, image.NYCbCrA when it carries an alpha plane — to non-premultiplied
// RGBA, which image/png and jpegenc encode directly. Its limited-range YCbCr
// is taken to full-range RGB (bt601LimitedToRGB) so the stored picture keeps
// the colours the WebP showed rather than the washed-out ones color.YCbCr
// would give. A fully transparent pixel keeps no colour. It reports whether
// every pixel is opaque, so an opaque picture is stored as a JPEG and one with
// transparency as a PNG, which keeps the alpha.
func lossyWebPToNRGBA(m image.Image) (out *image.NRGBA, opaque bool) {
	b := m.Bounds()
	out = image.NewNRGBA(b)
	opaque = true
	switch p := m.(type) {
	case *image.YCbCr:
		for y := b.Min.Y; y < b.Max.Y; y++ {
			row := out.Pix[(y-b.Min.Y)*out.Stride:]
			for x := b.Min.X; x < b.Max.X; x++ {
				r, g, bl := bt601LimitedToRGB(p.Y[p.YOffset(x, y)], p.Cb[p.COffset(x, y)], p.Cr[p.COffset(x, y)])
				i := (x - b.Min.X) * 4
				row[i+0], row[i+1], row[i+2], row[i+3] = r, g, bl, 0xFF
			}
		}
	case *image.NYCbCrA:
		for y := b.Min.Y; y < b.Max.Y; y++ {
			row := out.Pix[(y-b.Min.Y)*out.Stride:]
			for x := b.Min.X; x < b.Max.X; x++ {
				a := p.A[p.AOffset(x, y)]
				if a != 0xFF {
					opaque = false
				}
				if a == 0 {
					continue // NewNRGBA is all zero: no colour under a clear pixel
				}
				r, g, bl := bt601LimitedToRGB(p.Y[p.YOffset(x, y)], p.Cb[p.COffset(x, y)], p.Cr[p.COffset(x, y)])
				i := (x - b.Min.X) * 4
				row[i+0], row[i+1], row[i+2], row[i+3] = r, g, bl, a
			}
		}
	}
	return out, opaque
}

/* What nobody sees --------------------------------------------------------- */

// The entries a cleaned palette gives what it does not show: opaque black for
// an entry no pixel uses, transparent black for the one transparent entry.
// They are values of the color.Color interface made once, so filling a
// palette with them allocates nothing.
var (
	unusedEntry      color.Color = color.RGBA{A: 0xFF}
	transparentEntry color.Color = color.RGBA{}
)

// clearTransparentColour sets the colour of every fully transparent pixel of
// a decoded picture to zero, in place, and cleans a paletted one
// (cleanPaletted). These are the kinds image/png and x/image/webp decode
// anything with transparency to; the others (*image.RGBA and *image.RGBA64,
// which are premultiplied, grey, YCbCr) cannot hold colour under a
// transparent pixel.
func clearTransparentColour(m image.Image) {
	switch p := m.(type) {
	case *image.NRGBA:
		b := p.Rect
		for y := b.Min.Y; y < b.Max.Y; y++ {
			row := p.Pix[p.PixOffset(b.Min.X, y):p.PixOffset(b.Max.X, y)]
			for i := 0; i < len(row); i += 4 {
				if row[i+3] == 0 {
					row[i], row[i+1], row[i+2] = 0, 0, 0
				}
			}
		}
	case *image.NRGBA64:
		b := p.Rect
		for y := b.Min.Y; y < b.Max.Y; y++ {
			row := p.Pix[p.PixOffset(b.Min.X, y):p.PixOffset(b.Max.X, y)]
			for i := 0; i < len(row); i += 8 {
				if row[i+6] == 0 && row[i+7] == 0 {
					clear(row[i : i+6])
				}
			}
		}
	case *image.Paletted:
		cleanPaletted(p)
	}
}

// isTransparent reports whether a palette entry is fully transparent.
func isTransparent(c color.Color) bool {
	_, _, _, a := c.RGBA()
	return a == 0
}

// paletteUse is what a paletted picture's pixels use of its palette, once
// every pixel on a fully transparent entry has been moved onto the first such
// entry any pixel is on (remapTransparent).
type paletteUse struct {
	used  [256]bool
	last  int // the highest entry used, or -1 for a picture of no pixels
	clear int // the one transparent entry used, or -1
}

// remapTransparent moves every pixel of m on a fully transparent palette
// entry onto the first such entry a pixel is on, so a picture shows the same
// with one transparent entry where it had several, and reports which entries
// the pixels then use. image/gif gives a frame several transparent entries
// when its transparent index lies past the end of its colour table, and
// image/gif's encoder marks only the first of them transparent, which left
// pixels on the others opaque black.
func remapTransparent(m *image.Paletted) paletteUse {
	u := paletteUse{last: -1, clear: -1}
	b := m.Rect
	pixelRows := func(visit func(row []uint8)) {
		for y := b.Min.Y; y < b.Max.Y; y++ {
			visit(m.Pix[m.PixOffset(b.Min.X, y):m.PixOffset(b.Max.X, y)])
		}
	}
	pixelRows(func(row []uint8) {
		for _, v := range row {
			u.used[v] = true
		}
	})
	// The decoders keep every pixel inside its palette; a palette shorter
	// than a pixel's entry is lengthened as image/png does for one that is
	// not, with opaque black.
	for i := len(m.Palette); i < 256; i++ {
		if u.used[i] {
			for len(m.Palette) <= i {
				m.Palette = append(m.Palette, unusedEntry)
			}
		}
	}
	var transparent [256]bool
	extra := false
	for i, c := range m.Palette {
		if !u.used[i] || !isTransparent(c) {
			continue
		}
		if u.clear < 0 {
			u.clear = i
		} else {
			transparent[i], extra = true, true
			u.used[i] = false
		}
	}
	if extra {
		clearAt := uint8(u.clear)
		pixelRows(func(row []uint8) {
			for j, v := range row {
				if transparent[v] {
					row[j] = clearAt
				}
			}
		})
	}
	for i := 255; i >= 0; i-- {
		if u.used[i] {
			u.last = i
			break
		}
	}
	return u
}

// cleanPaletted rewrites m's palette in place to what its pixels show: one
// transparent entry at most (remapTransparent), holding transparent black,
// every entry no pixel uses opaque black, and nothing after the last entry
// used. A partly transparent entry keeps its colour, which shows.
func cleanPaletted(m *image.Paletted) {
	u := remapTransparent(m)
	for i := range m.Palette {
		switch {
		case !u.used[i]:
			m.Palette[i] = unusedEntry
		case i == u.clear:
			m.Palette[i] = transparentEntry
		}
	}
	m.Palette = m.Palette[:max(u.last+1, 1)]
}

// cleanGIF cleans the colour tables of a decoded GIF as cleanPaletted cleans
// one palette, and keeps the frames that shared the global table sharing it.
//
// image/gif gives a frame with a colour table of its own that table, and a
// frame without one the global table itself, or a copy of it with the
// frame's transparent entry made transparent (and lengthened with transparent
// entries when that lies past its end). Its encoder writes the global table
// from Config.ColorModel and lets a frame use it whenever the frame's palette
// matches it but for the transparent entry. So the frames are sorted the same
// way: a palette that is the global table, entry for entry, but for
// transparent entries is cleaned into a new global table holding every colour
// any of those frames shows, and given that table, with its own transparent
// entry where it has one; any other palette is the frame's own and is cleaned
// by itself. A GIF none of whose frames uses its global table is stored
// without one. The background colour, which browsers do not draw, keeps its
// index when that is still in the table, and is otherwise the first entry.
func cleanGIF(g *gif.GIF) {
	global, _ := g.Config.ColorModel.(color.Palette)
	var (
		shows      [256]bool // the global table's entries a frame shows
		globalLen  int       // how much of it the frames use
		sharers    []int     // the frames that use it, by index
		sharerUses []paletteUse
	)
	for k, frame := range g.Image {
		if !copiesPalette(frame.Palette, global) {
			cleanPaletted(frame)
			continue
		}
		u := remapTransparent(frame)
		for i := 0; i <= u.last; i++ {
			if u.used[i] && i != u.clear {
				shows[i] = true
			}
		}
		globalLen = max(globalLen, u.last+1, 1)
		sharers = append(sharers, k)
		sharerUses = append(sharerUses, u)
	}
	if len(sharers) == 0 {
		g.Config.ColorModel = nil
		g.BackgroundIndex = 0
		return
	}

	// An entry past the global table's end that a frame shows can only be
	// one remapTransparent lengthened its palette with, which is opaque black.
	table := make(color.Palette, globalLen)
	for i := range table {
		table[i] = unusedEntry
		if shows[i] && i < len(global) {
			table[i] = global[i]
		}
	}
	withClear := map[int]color.Palette{}
	for j, k := range sharers {
		ti := sharerUses[j].clear
		if ti < 0 {
			g.Image[k].Palette = table
			continue
		}
		p, ok := withClear[ti]
		if !ok {
			p = append(color.Palette(nil), table...)
			p[ti] = transparentEntry
			withClear[ti] = p
		}
		g.Image[k].Palette = p
	}
	g.Config.ColorModel = table
	if int(g.BackgroundIndex) >= len(table) {
		g.BackgroundIndex = 0
	}
}

// copiesPalette reports whether p is global, or image/gif's copy of it for a
// frame with a transparent entry: the same colours, but for transparent
// entries, which may run past global's end.
func copiesPalette(p, global color.Palette) bool {
	if len(global) == 0 || len(p) < len(global) {
		return false
	}
	if &p[0] == &global[0] {
		return true
	}
	for i, c := range p {
		if i < len(global) && c == global[i] {
			continue
		}
		if !isTransparent(c) {
			return false
		}
	}
	return true
}

// gifFrames is what a GIF's frames come to: their area together, and how
// many there are, and how often they are played.
type gifFrames struct {
	pixels, count int64
	looped        bool // a loop extension gave a loop count,
	loop          int  // which is this, as gif.GIF's LoopCount reads
}

// gifFramePixels adds up the area of every frame a GIF declares, and counts
// them, reading only the block structure: image descriptors are read, and
// everything else, compressed pixels included, is stepped over. It allocates
// nothing.
//
// It also reads the loop count, which image/gif reads only from a NETSCAPE2.0
// application extension: from its first sub-block, when that is three bytes
// starting with 1, the last such extension winning. Browsers read an
// ANIMEXTS1.0 extension exactly the same way, and some encoders write that
// one instead, so the walk reads both, and an animation that looped in a
// browser still loops once it is stored.
//
// The walk has to read the file exactly as image/gif does, block for block,
// or a file could steer the two apart: an extension the walk read one way and
// the decoder another would leave the walk lost in the middle of the file,
// counting none of the frames the decoder goes on to decode. So each
// extension is stepped over the way readExtension reads it — a graphic
// control extension is a fixed six bytes, a plain-text one a fixed thirteen
// and then sub-blocks, an application one a length byte, that many bytes and
// then sub-blocks, and a comment only sub-blocks — and the walk stops on a
// block, an extension or a field the decoder would stop on. It does not judge
// the compressed pixels; where they make the decoder stop with an error, the
// frames it had allocated by then have already been counted.
//
// It returns ErrImageTooLarge as soon as the area passes limit, and
// ErrCorruptImage when the walk does not end at the trailer: a file the
// decoder could not read to its end is refused rather than half counted.
// TestGIFFramePixelsReadsWhatTheDecoderReads and FuzzGIFFramePixels hold it to
// the decoder.
func gifFramePixels(data []byte, limit int64) (gifFrames, error) {
	const (
		headerLen     = 13 // signature, version, logical screen descriptor
		descriptorLen = 10 // separator, position, size, flags
		graphicLen    = 6  // block size (4), flags, delay, transparent index, terminator
		plainTextLen  = 13 // read whole by image/gif, whatever its block size says
	)
	if len(data) < headerLen {
		return gifFrames{}, ErrCorruptImage
	}
	i := headerLen
	if flags := data[10]; flags&0x80 != 0 {
		i += 3 << (1 + flags&0x07)
	}
	// skipBlocks steps over a run of data sub-blocks and its terminator. It
	// returns len(data) when the run is cut short, which ends the walk
	// without a trailer.
	skipBlocks := func(i int) int {
		for i < len(data) {
			n := int(data[i])
			i++
			if n == 0 {
				return i
			}
			i += n
		}
		return len(data)
	}

	var f gifFrames
	for i < len(data) {
		switch data[i] {
		case 0x21: // extension: introducer, label, then as the label says
			if i+2 > len(data) {
				return f, ErrCorruptImage
			}
			at := i + 2
			switch data[i+1] {
			case 0xF9: // graphic control
				if at+graphicLen > len(data) || data[at] != 4 || data[at+5] != 0 {
					return f, ErrCorruptImage
				}
				i = at + graphicLen
			case 0x01: // plain text
				i = skipBlocks(at + plainTextLen)
			case 0xFF: // application: its own length byte, then sub-blocks
				if at >= len(data) {
					return f, ErrCorruptImage
				}
				size := int(data[at])
				if j := at + 1 + size; size == 11 && j+4 <= len(data) && data[j] == 3 && data[j+1] == 1 {
					if id := string(data[at+1 : j]); id == "NETSCAPE2.0" || id == "ANIMEXTS1.0" {
						f.looped, f.loop = true, int(data[j+2])|int(data[j+3])<<8
					}
				}
				i = skipBlocks(at + 1 + size)
			case 0xFE: // comment
				i = skipBlocks(at)
			default: // image/gif refuses an extension it does not know
				return f, ErrCorruptImage
			}
		case 0x2C: // image descriptor
			if i+descriptorLen > len(data) {
				return f, ErrCorruptImage
			}
			w := int64(data[i+5]) | int64(data[i+6])<<8
			h := int64(data[i+7]) | int64(data[i+8])<<8
			flags := data[i+9]
			f.count++
			f.pixels += w * h
			if f.pixels > limit {
				return f, ErrImageTooLarge
			}
			i += descriptorLen
			if flags&0x80 != 0 {
				i += 3 << (1 + flags&0x07)
			}
			// The LZW minimum code size, which image/gif holds to 2–8, and
			// then the compressed pixels.
			if i >= len(data) || data[i] < 2 || data[i] > 8 {
				return f, ErrCorruptImage
			}
			i = skipBlocks(i + 1)
		case 0x3B: // the trailer, where the decoder stops reading
			if f.count == 0 { // which it refuses before any frame
				return f, ErrCorruptImage
			}
			return f, nil
		default: // a block image/gif refuses
			return f, ErrCorruptImage
		}
	}
	return f, ErrCorruptImage
}
