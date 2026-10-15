package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

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
// nothing of the upload's own bytes. The thumbnail is made from those same
// pixels as they were handed to the encoder (sanitized.pic), not from the
// stored file decoded again, and the checksum is of the stored file, so all
// three describe the same picture: exactly when it is stored as a PNG or a
// GIF, and within what the re-encoding loses when it is stored as a JPEG.
//
// A decoded picture can still hold what nobody sees, in channels a re-encode
// would carry through untouched: the colour of a fully transparent pixel,
// which an editor that erases part of a picture by clearing its alpha often
// leaves behind; the colour a barely-visible pixel holds beyond what its alpha
// can show (a photo stored under an alpha of 1 shows almost nothing yet keeps
// every bit of its colour); and, in a paletted picture, entries no pixel uses,
// duplicate entries of one colour, the order of the palette, and the
// particular entry a pixel points at among equal ones — any of which can carry
// a whole second file, EXIF and all, in the pixel bytes of a flat picture. So
// reduceHiddenColour cuts each pixel's colour to the precision its alpha can
// show (reduce8/reduce16), zero under a fully transparent one, and rebuilds a
// palette from the colours its pixels show, in first-use order, folding every
// transparent entry onto one and remapping the pixels (cleanPaletted,
// cleanGIF). What is left is the picture: a fully opaque pixel keeps its exact
// value, including the low byte of a 16-bit channel, because that is the
// picture and no rewrite that keeps the picture exact can drop it, so data in
// the low bits of a visible picture (classic steganography) still survives —
// the job here is to strip metadata and the channels that do not show, not to
// alter the picture. The one exception is a PNG's gamma, which is applied to
// its samples so that the picture shows as it did (applyPNGGamma).

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
// header check holds the canvas to MaxPixels, but not the frames: a frame
// after the first may reach past the canvas (gifReader), every frame is
// decoded and kept at a byte a pixel until it is re-encoded, and a frame of
// one flat colour compresses to almost nothing: a few megabytes of GIF could
// otherwise ask for gigabytes. Four
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
// standard library does not have. The same goes for a PNG's iCCP chunk and
// its cHRM primaries and a WebP's ICCP chunk; a PNG's gAMA chunk is the
// exception, and is applied (pngGamma).
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
// private are left behind. Of the colour chunks, a gAMA that browsers apply
// is applied to the samples first, so the picture shows as it did
// (applyPNGGamma). An opaque pixel is otherwise kept exactly as image/png
// decodes it, which is not always the form it came in: 1-, 2- and 4-bit grey
// is stored at 8 bits, and grey or colour with a transparent colour key (tRNS)
// is stored as colour with an alpha channel, or with none when no pixel has the
// key's colour, while 16-bit pictures stay 16-bit
// (TestPNGIsStoredInTheFormItDecodesTo). A pixel's colour is first cut to what
// its alpha shows and a palette rebuilt to one entry per colour its pixels show
// (reduceHiddenColour), so a palette may be stored at a lower bit depth. A
// picture with an EXIF orientation in an eXIf chunk
// is turned upright first, as for a JPEG, because browsers show it turned.
// An animated PNG keeps its still image, which is what a viewer without APNG
// support has always shown.
func sanitizePNG(data []byte, limit int64) (sanitized, error) {
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	if gamma, ok := pngGamma(data); ok {
		applyPNGGamma(src, gamma)
	}
	reduceHiddenColour(src)
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
//
// It is stored as browsers show it where that and image/gif part ways. A
// file that ends after its last block without a trailer plays in a browser,
// and is decoded with one put after it. A frame after the first that reaches
// past the canvas is drawn by a browser only where it overlaps the canvas,
// and is cut to that (clipGIF); image/gif refuses the whole file, so it is
// told the logical screen takes in every frame (gifReader). A first frame
// past the canvas is refused by gifFramePixels: browsers disagree on what to
// show for it.
func sanitizeGIF(data []byte, limit int64, frames gifFrames) (sanitized, error) {
	g, err := gif.DecodeAll(gifReader(data, frames))
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	if frames.looped {
		g.LoopCount = frames.loop
	}
	clipGIF(g, frames.canvas)
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

// gifTrailer is the block that ends a GIF.
var gifTrailer = []byte{0x3B}

// gifReader is what image/gif decodes a GIF from, given what gifFramePixels
// read of it: the file, with the logical screen its header gives widened to
// take in every frame when one reaches past it, because image/gif refuses a
// frame outside the screen, and with a trailer after it when it had none.
// Nothing is copied but the header.
func gifReader(data []byte, frames gifFrames) io.Reader {
	head := data[:10] // signature, version, logical screen width and height
	if frames.screen != frames.canvas {
		head = append([]byte(nil), head...)
		binary.LittleEndian.PutUint16(head[6:], uint16(frames.screen.X))
		binary.LittleEndian.PutUint16(head[8:], uint16(frames.screen.Y))
	}
	parts := []io.Reader{bytes.NewReader(head), bytes.NewReader(data[10:])}
	if !frames.trailed {
		parts = append(parts, bytes.NewReader(gifTrailer))
	}
	return io.MultiReader(parts...)
}

// clipGIF holds a decoded GIF to its canvas, as browsers draw one: a frame
// reaching past the canvas is cut to the part on it, and a frame wholly off
// it, which shows nothing for its delay, becomes a single transparent pixel
// that is left in place, which shows the same nothing for the same time.
func clipGIF(g *gif.GIF, canvas image.Point) {
	g.Config.Width, g.Config.Height = canvas.X, canvas.Y
	r := image.Rectangle{Max: canvas}
	for k, m := range g.Image {
		if m.Rect.In(r) {
			continue
		}
		if on := m.Rect.Intersect(r); !on.Empty() {
			g.Image[k] = m.SubImage(on).(*image.Paletted)
			continue
		}
		g.Image[k] = image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{transparentEntry})
		g.Disposal[k] = gif.DisposalNone
	}
}

// sanitizeWebP checks a WebP's structure and strips it to the chunks that
// make the picture (stripWebP), decodes what is left, and stores the pixels as
// a new file: a JPEG when the picture is lossy and opaque, which is what a
// lossy WebP is closest to, and otherwise a PNG, which keeps a lossless
// picture exactly and a transparent one's alpha. x/image decodes a lossy
// picture to the limited-range YCbCr libwebp wrote, which read as full range
// would store black and white as greys, so its range is expanded first: an
// opaque picture's in its own YCbCr planes, where they lie
// (expandLimitedRange), so the JPEG keeps the WebP's 4:2:0 colour rather than
// spending bytes on colour detail the WebP never had; a transparent one's on
// its way to RGB for the PNG (lossyWebPToNRGBA). The JPEG is at
// storedJPEGQuality whatever the WebP was saved at: a WebP's quality setting
// is not a JPEG's, so its own is no guide, and the lower it was the larger the
// JPEG: measured on photos, up to 1.4 times the WebP's size from quality 90,
// two to three times from 75, 3.5 to 5.5 from 25 and up to 8.5 from 5. An
// animated WebP is refused as it always has been; x/image decodes still
// images only.
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

	// A lossy picture with an alpha plane that is opaque throughout is
	// stored as one without.
	if p, ok := pic.(*image.NYCbCrA); ok && opaqueAlpha(p) {
		pic = &p.YCbCr
	}
	format := FormatPNG
	buf := newStoredBuffer(len(data), limit)
	switch p := pic.(type) {
	case *image.YCbCr: // lossy and opaque
		format = FormatJPEG
		expandLimitedRange(p)
		err = jpegenc.Encode(buf, p, &jpegenc.Options{Quality: storedJPEGQuality})
	case *image.NYCbCrA: // lossy with transparency
		rgb := lossyWebPToNRGBA(p)
		pic = rgb
		err = png.Encode(buf, rgb)
	default: // lossless, decoded as *image.NRGBA
		reduceHiddenColour(pic)
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

// fullRangeLuma and fullRangeChroma take a limited-range BT.601 sample to the
// full-range one the JFIF formula reads as the same colour: luma from 16–235
// to 0–255, chroma from 16–240 to 0–255 about 128, rounded and clamped. Read
// back as JFIF YCbCr, as a JPEG decoder and image.YCbCr do, the result is
// what bt601LimitedToRGB gives the original, to within rounding.
var (
	fullRangeLuma   = rangeTable(16, 219)
	fullRangeChroma = rangeTable(128, 224)
)

// rangeTable maps every sample v to (v-mid)·255/span, plus 128 when mid is
// the chroma midpoint, rounded half away from zero and clamped to a byte.
func rangeTable(mid, span int) (t [256]uint8) {
	base := 0
	if mid == 128 {
		base = 128
	}
	for v := range t {
		n := (v - mid) * 255
		q := (2*n + span) / (2 * span) // rounds half up for n >= 0
		if n < 0 {
			q = -((-2*n + span) / (2 * span))
		}
		t[v] = clip8(base + q)
	}
	return t
}

// expandLimitedRange rewrites a decoded lossy WebP's planes, in place, from
// limited to full range (fullRangeLuma, fullRangeChroma), so that what they
// hold is what the JFIF formula — the stored JPEG's, and the thumbnail
// scaler's — reads as the colours the WebP showed. The planes keep their
// subsampling, so the stored JPEG is at the WebP's own colour resolution.
// Every sample of the planes is the decoder's own, the macroblock padding
// past the picture's edge included, so each is rewritten once.
func expandLimitedRange(m *image.YCbCr) {
	for i, v := range m.Y {
		m.Y[i] = fullRangeLuma[v]
	}
	for i, v := range m.Cb {
		m.Cb[i] = fullRangeChroma[v]
	}
	for i, v := range m.Cr {
		m.Cr[i] = fullRangeChroma[v]
	}
}

// opaqueAlpha reports whether every pixel of a picture with an alpha plane
// is fully opaque.
func opaqueAlpha(m *image.NYCbCrA) bool {
	b := m.Rect
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for _, a := range m.A[m.AOffset(b.Min.X, y):m.AOffset(b.Max.X, y)] {
			if a != 0xFF {
				return false
			}
		}
	}
	return true
}

// lossyWebPToNRGBA converts a lossy WebP with transparency, which x/image
// decodes as image.NYCbCrA, to non-premultiplied RGBA for image/png. Its
// limited-range YCbCr is taken to full-range RGB (bt601LimitedToRGB) so the
// stored picture keeps the colours the WebP showed rather than the washed-out
// ones color.YCbCr would give. A fully transparent pixel keeps no colour, and
// a partly transparent one only what its alpha shows (reduce8).
func lossyWebPToNRGBA(p *image.NYCbCrA) *image.NRGBA {
	b := p.Bounds()
	out := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := out.Pix[(y-b.Min.Y)*out.Stride:]
		for x := b.Min.X; x < b.Max.X; x++ {
			a := p.A[p.AOffset(x, y)]
			if a == 0 {
				continue // NewNRGBA is all zero: no colour under a clear pixel
			}
			r, g, bl := bt601LimitedToRGB(p.Y[p.YOffset(x, y)], p.Cb[p.COffset(x, y)], p.Cr[p.COffset(x, y)])
			r, g, bl = reduce8(r, g, bl, a) // keep only the colour this alpha shows
			i := (x - b.Min.X) * 4
			row[i+0], row[i+1], row[i+2], row[i+3] = r, g, bl, a
		}
	}
	return out
}

/* What nobody sees --------------------------------------------------------- */

// The entries a cleaned palette gives what it does not show: opaque black for
// a pixel that names an entry past its palette's end (image/png and
// image/gif), transparent black for the one transparent entry. They are
// values of the color.Color interface made once.
var (
	unusedEntry      color.Color = color.RGBA{A: 0xFF}
	transparentEntry color.Color = color.RGBA{}
)

// unpremul8 reduces one 8-bit colour channel to the precision its alpha can
// show: it premultiplies (rounding) and then unpremultiplies (rounding). At a
// full alpha it returns the channel unchanged; at a low alpha it returns one
// of the few values that alpha can represent, so a colour held at full
// precision under a near-zero alpha — which barely shows, or does not show at
// all — does not survive the round trip. a must not be zero.
func unpremul8(c, a uint8) uint8 {
	p := (int(c)*int(a) + 127) / 255
	return uint8((p*255 + int(a)/2) / int(a))
}

// unpremul16 is unpremul8 for a 16-bit channel.
func unpremul16(c, a uint16) uint16 {
	p := (uint32(c)*uint32(a) + 0x7FFF) / 0xFFFF
	return uint16((p*0xFFFF + uint32(a)/2) / uint32(a))
}

// reduce8 reduces an 8-bit non-premultiplied colour to what its alpha shows:
// an opaque pixel is unchanged, a fully transparent one keeps no colour, and a
// partly transparent one keeps only the colour its alpha can carry (unpremul8).
func reduce8(r, g, b, a uint8) (uint8, uint8, uint8) {
	switch a {
	case 0xFF:
		return r, g, b
	case 0:
		return 0, 0, 0
	}
	return unpremul8(r, a), unpremul8(g, a), unpremul8(b, a)
}

// visibleNRGBA is a palette entry reduced to what it shows: transparent black
// for a fully transparent entry, otherwise its colour to the precision its
// alpha can show (reduce8). It is the canonical form two entries that show the
// same thing share, so folding a palette by it drops duplicates.
func visibleNRGBA(c color.Color) color.NRGBA {
	nc := color.NRGBAModel.Convert(c).(color.NRGBA)
	if nc.A == 0 {
		return color.NRGBA{}
	}
	nc.R, nc.G, nc.B = reduce8(nc.R, nc.G, nc.B, nc.A)
	return nc
}

// reduceHiddenColour drops from a decoded picture, in place, every value it
// holds that it does not show: the colour under a fully transparent pixel
// (which an editor that erases by clearing alpha often leaves behind), the
// colour a partly transparent pixel holds beyond what its alpha can show
// (reduce8/reduce16), and, for a paletted picture, its duplicate and unused
// palette entries and the order of its palette (cleanPaletted). These are the
// kinds image/png and x/image/webp decode anything with transparency to; the
// others (*image.RGBA and *image.RGBA64, which are premultiplied, grey, YCbCr)
// cannot hold colour a pixel does not show.
func reduceHiddenColour(m image.Image) {
	switch p := m.(type) {
	case *image.NRGBA:
		b := p.Rect
		for y := b.Min.Y; y < b.Max.Y; y++ {
			row := p.Pix[p.PixOffset(b.Min.X, y):p.PixOffset(b.Max.X, y)]
			for i := 0; i < len(row); i += 4 {
				row[i], row[i+1], row[i+2] = reduce8(row[i], row[i+1], row[i+2], row[i+3])
			}
		}
	case *image.NRGBA64:
		b := p.Rect
		for y := b.Min.Y; y < b.Max.Y; y++ {
			row := p.Pix[p.PixOffset(b.Min.X, y):p.PixOffset(b.Max.X, y)]
			for i := 0; i < len(row); i += 8 {
				a := uint16(row[i+6])<<8 | uint16(row[i+7])
				switch a {
				case 0xFFFF:
				case 0:
					clear(row[i : i+6])
				default:
					for c := 0; c < 6; c += 2 {
						v := unpremul16(uint16(row[i+c])<<8|uint16(row[i+c+1]), a)
						row[i+c], row[i+c+1] = uint8(v>>8), uint8(v)
					}
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

// colourFolder assigns a new, compact palette index to each distinct colour a
// picture shows, in the order its pixels first name one, folding every fully
// transparent entry onto one slot (visibleNRGBA). Rebuilding a palette through
// it drops the three things a flat or near-flat picture can carry that it does
// not show: duplicate entries of one colour, the order of the palette, and the
// particular index a pixel points at among equal entries — each of which can
// hold a whole second file in the pixel bytes. One folder serves a GIF's
// frames that share a table, so they keep sharing one.
type colourFolder struct {
	palette color.Palette
	index   map[color.NRGBA]int
}

func newColourFolder() *colourFolder {
	return &colourFolder{index: map[color.NRGBA]int{}}
}

// fold returns the new index of a palette entry's visible colour, adding it to
// the rebuilt palette if it is the first pixel to show it. ok is false once
// 256 colours are used and a new one cannot be added, which only a crafted
// picture reaches.
func (f *colourFolder) fold(c color.Color) (idx int, ok bool) {
	vc := visibleNRGBA(c)
	if i, seen := f.index[vc]; seen {
		return i, true
	}
	if len(f.palette) >= 256 {
		return 0, false
	}
	i := len(f.palette)
	var entry color.Color = vc
	if vc.A == 0 {
		entry = transparentEntry
	}
	f.palette = append(f.palette, entry)
	f.index[vc] = i
	return i, true
}

// paletteEntryAt is the colour a pixel of value v shows, treating an index
// past the palette's end as opaque black, as image/png and image/gif pad a
// short table.
func paletteEntryAt(pal color.Palette, v uint8) color.Color {
	if int(v) < len(pal) {
		return pal[v]
	}
	return unusedEntry
}

// cleanPaletted rebuilds m's palette in place from the colours its pixels
// show, through a colourFolder, and remaps the pixels to match: one entry per
// distinct colour in first-use order, a single transparent entry, and nothing
// else. So a flat picture whose 256 identical entries, palette order, or pixel
// indices hid a second file keeps one entry and pixels that all point at it.
func cleanPaletted(m *image.Paletted) {
	f := newColourFolder()
	b := m.Rect
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := m.Pix[m.PixOffset(b.Min.X, y):m.PixOffset(b.Max.X, y)]
		for j, v := range row {
			i, _ := f.fold(paletteEntryAt(m.Palette, v)) // a lone picture never overflows
			row[j] = uint8(i)
		}
	}
	if len(f.palette) == 0 {
		f.palette = color.Palette{transparentEntry} // a picture of no pixels
	}
	m.Palette = f.palette
}

// cleanGIF cleans the colour tables of a decoded GIF as cleanPaletted cleans
// one palette, and keeps the frames that shared the global table sharing it.
//
// image/gif gives a frame with a colour table of its own that table, and a
// frame without one the global table itself, or a copy of it with the frame's
// transparent entry made transparent (and lengthened with transparent entries
// when that lies past its end). Its encoder writes the global table from
// Config.ColorModel and lets a frame use it whenever the frame's palette is
// that same slice, or a copy differing only at the transparent index. So the
// frames that copy the global table are rebuilt together through one
// colourFolder into a single union table — one entry per distinct colour any
// of them shows, in first-use order, with every transparent entry folded onto
// one — and each is given that table and has its pixels remapped to it, so
// image/gif writes it once as the global table and nothing of a flat frame's
// duplicate entries, palette order or pixel indices survives. Any other frame
// carries its own table and is cleaned by itself (cleanPaletted). A GIF none
// of whose frames copies the global table, or whose shared colours do not fit
// one 256-entry table, is stored with every frame's table its own. The
// background colour, which browsers do not draw, is remapped to its colour's
// new index, or the first entry.
func cleanGIF(g *gif.GIF) {
	global, _ := g.Config.ColorModel.(color.Palette)
	background := int(g.BackgroundIndex)
	var sharers []int
	for k, frame := range g.Image {
		if copiesPalette(frame.Palette, global) {
			sharers = append(sharers, k)
		} else {
			cleanPaletted(frame)
		}
	}
	noGlobalTable := func() {
		for _, k := range sharers {
			cleanPaletted(g.Image[k])
		}
		g.Config.ColorModel = nil
		g.BackgroundIndex = 0
	}
	if len(sharers) == 0 {
		g.Config.ColorModel = nil
		g.BackgroundIndex = 0
		return
	}

	// Fold every sharer frame's colours into one table first, so an overflow
	// (more distinct colours than one table holds, which only a crafted GIF
	// reaches) is found before any frame's pixels are touched.
	f := newColourFolder()
	framePixels := func(k int, visit func(off int)) {
		m := g.Image[k]
		b := m.Rect
		for y := b.Min.Y; y < b.Max.Y; y++ {
			off := m.PixOffset(b.Min.X, y)
			for x := b.Min.X; x < b.Max.X; x++ {
				visit(off)
				off++
			}
		}
	}
	for _, k := range sharers {
		frame := g.Image[k]
		overflow := false
		framePixels(k, func(off int) {
			if _, ok := f.fold(paletteEntryAt(frame.Palette, frame.Pix[off])); !ok {
				overflow = true
			}
		})
		if overflow {
			noGlobalTable()
			return
		}
	}
	for _, k := range sharers {
		frame := g.Image[k]
		framePixels(k, func(off int) {
			i, _ := f.fold(paletteEntryAt(frame.Palette, frame.Pix[off]))
			frame.Pix[off] = uint8(i)
		})
	}
	for _, k := range sharers {
		g.Image[k].Palette = f.palette
	}
	g.Config.ColorModel = f.palette
	g.BackgroundIndex = 0
	if background < len(global) {
		if i, ok := f.index[visibleNRGBA(global[background])]; ok {
			g.BackgroundIndex = uint8(i)
		}
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
// many there are, and how often they are played; and what gifReader needs
// to have image/gif decode them.
type gifFrames struct {
	pixels, count int64
	looped        bool // a loop extension gave a loop count,
	loop          int  // which is this, as gif.GIF's LoopCount reads
	// largest is the area of the largest frame. image/gif puts an
	// interlaced frame's rows in order in a copy of it, and a frame may be
	// larger than the canvas.
	largest int64
	// canvas is the logical screen the header gives, which is the picture
	// browsers show; screen takes in every frame as well.
	canvas, screen image.Point
	// trailed is whether the file ends with a trailer rather than straight
	// after its last block.
	trailed bool
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
// Where browsers show a file image/gif refuses, the walk reads it as they
// do, and sanitizeGIF has the decoder read it that way too (gifReader): a
// file that ends straight after a whole block, without the trailer, ends
// there; and a frame after the first may reach past the canvas, which the
// screen is widened to take in. A first frame past the canvas is refused,
// because browsers disagree on what to show for it (Chromium widens the
// canvas to hold it, Firefox moves it to the corner and makes the canvas its
// size), and so is a frame reaching past the 65,535 pixels a header can say.
//
// It returns ErrImageTooLarge as soon as the area passes limit, and
// ErrCorruptImage when the walk does not end at the trailer or at the end of
// a whole block: a file the decoder could not read to its end is refused
// rather than half counted. TestGIFFramePixelsReadsWhatTheDecoderReads and
// FuzzGIFFramePixels hold it to the decoder.
func gifFramePixels(data []byte, limit int64) (gifFrames, error) {
	const (
		headerLen     = 13 // signature, version, logical screen descriptor
		descriptorLen = 10 // separator, position, size, flags
		graphicLen    = 6  // block size (4), flags, delay, transparent index, terminator
		plainTextLen  = 13 // read whole by image/gif, whatever its block size says
		maxSide       = 0xFFFF
	)
	if len(data) < headerLen {
		return gifFrames{}, ErrCorruptImage
	}
	var f gifFrames
	f.canvas = image.Pt(int(binary.LittleEndian.Uint16(data[6:])), int(binary.LittleEndian.Uint16(data[8:])))
	f.screen = f.canvas
	i := headerLen
	if flags := data[10]; flags&0x80 != 0 {
		i += 3 << (1 + flags&0x07)
	}
	// skipBlocks steps over a run of data sub-blocks and its terminator, and
	// reports whether the file held all of it.
	skipBlocks := func(i int) (int, bool) {
		for i < len(data) {
			n := int(data[i])
			i++
			if n == 0 {
				return i, true
			}
			i += n
		}
		return len(data), false
	}

	whole := true
	for i < len(data) && whole {
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
				i, whole = skipBlocks(at + plainTextLen)
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
				i, whole = skipBlocks(at + 1 + size)
			case 0xFE: // comment
				i, whole = skipBlocks(at)
			default: // image/gif refuses an extension it does not know
				return f, ErrCorruptImage
			}
		case 0x2C: // image descriptor
			if i+descriptorLen > len(data) {
				return f, ErrCorruptImage
			}
			left := int(binary.LittleEndian.Uint16(data[i+1:]))
			top := int(binary.LittleEndian.Uint16(data[i+3:]))
			w := int(binary.LittleEndian.Uint16(data[i+5:]))
			h := int(binary.LittleEndian.Uint16(data[i+7:]))
			flags := data[i+9]
			right, bottom := left+w, top+h
			if f.count == 0 && (right > f.canvas.X || bottom > f.canvas.Y) || right > maxSide || bottom > maxSide {
				return f, ErrCorruptImage
			}
			f.screen = image.Pt(max(f.screen.X, right), max(f.screen.Y, bottom))
			f.count++
			f.pixels += int64(w) * int64(h)
			f.largest = max(f.largest, int64(w)*int64(h))
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
			i, whole = skipBlocks(i + 1)
		case 0x3B: // the trailer, where the decoder stops reading
			if f.count == 0 { // which it refuses before any frame
				return f, ErrCorruptImage
			}
			f.trailed = true
			return f, nil
		default: // a block image/gif refuses
			return f, ErrCorruptImage
		}
	}
	// The end of the file, with no trailer: where a whole block ended after
	// a frame, it ends there, as it does in a browser.
	if !whole || i != len(data) || f.count == 0 {
		return f, ErrCorruptImage
	}
	return f, nil
}
