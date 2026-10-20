package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"

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
//   - GIF is decoded frame by frame and played, and what it shows is written
//     out by one rule (playGIF): each picture it shows, for as long as it
//     shows, and its loop count, and none of its comments, its other
//     application data, or anything of how its frames made those pictures.
//     Two GIFs a browser shows alike are stored as the same bytes.
//   - WebP is decoded and stored as a PNG, or as a JPEG when it is lossy and
//     opaque, because nothing in the standard library or x/image can encode
//     WebP (sanitizeWebP says why it is not kept as it came).
//
// So the stored file is always one Go encoded from decoded pixels, and holds
// nothing of the upload's own bytes. The thumbnail is made from those same
// pixels as they were handed to the encoder (sanitized.pic; for a GIF, the
// first picture it shows), not from the stored file decoded again, and the checksum is of the stored file, so all
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
// transparent entry onto one and remapping the pixels (cleanPaletted). A
// GIF's tables are written from the colours its pictures show, in order of
// their values (playGIF). What is left is the picture: a fully opaque pixel keeps its exact
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
// have 300 frames) and no more memory than the largest JPEG. A GIF past it is
// refused as too costly to process (ErrImageTooCostly), not as too large a
// picture: its canvas may be small, and each frame smaller.
//
// It also bounds the work of playing a GIF, which goes through each frame,
// and each frame it stores, a row at a time: what is held to it is each
// frame's charge (gifCharge), its pixels with every row charged as at least
// gifRowPixels, and playGIF holds the frames it stores and the rectangles it
// looks at to it the same way.
const maxGIFFramePixels = 4 * MaxPixels

// gifRowPixels is the least each row of a GIF's frame is charged against
// maxGIFFramePixels, in pixels (gifCharge). Decoding a frame, playing it and
// writing what it shows each pass over it a row at a time, and every pass
// sets each row up (the rows it compares, the frames drawn on them, a call
// of the LZW writer) at a cost of its own, which a row a pixel wide costs as
// much of as a wide one. Charged by its pixels alone, a GIF of the cap's
// worth of frames a pixel wide and 65,535 tall, 1,525 of them and 100
// million rows, took 21 seconds of CPU to play, where four 5000×5000 frames
// take under three. At 64 pixels a row a GIF has at most 1.56 million rows,
// whose setting up comes to a few tenths of a second whatever the path, and
// a frame at least 64 pixels wide is charged its pixels, as before.
const gifRowPixels = 64

// gifCharge is what a GIF's frame, or a rectangle playGIF stores or looks
// at, of w×h pixels is charged against maxGIFFramePixels: its pixels, with
// each row charged as at least gifRowPixels.
func gifCharge(w, h int64) int64 {
	if w <= 0 || h <= 0 {
		return 0
	}
	return max(w, gifRowPixels) * h
}

// storedBuffer collects an encoded original and refuses to hold more than
// limit bytes (storedLimit), with ErrStoredTooLarge. It grows by doubling, as a
// bytes.Buffer does, but never past the limit, so it holds at most twice the
// limit while it grows: what processingCost counts for it.
//
// It also fails a write once its context is done (ctx), so an encode of a
// costly picture stops when the request that asked for it is cancelled or has
// run out of time, rather than running to the end and only then finding
// nothing wants it. The encoders stop at the first write refused them, or
// soon after: jpegenc at the end of the row of units it is coding
// (TestEncodeStopsAtAFailedWrite), image/png and image/gif once their own
// buffers next pass a write on. While the context is live the check is the
// one comparison and the bytes written are exactly what they would be
// without it.
type storedBuffer struct {
	ctx   context.Context
	buf   []byte
	limit int64
}

func newStoredBuffer(ctx context.Context, sizeHint int, limit int64) *storedBuffer {
	return &storedBuffer{ctx: ctx, buf: make([]byte, 0, min(int64(sizeHint), limit)), limit: limit}
}

func (b *storedBuffer) Write(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
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
	format Format      // what data is, which for a WebP upload is PNG or JPEG
	data   []byte      // the bytes to store
	pic    image.Image // the picture, or the first a GIF shows, at its size
	// thumb is pic's thumbnail when it was made already, as a GIF's is,
	// before its frames are played.
	thumb *Thumbnail
}

// sanitize rewrites an upload for storage. The rewritten file may not be
// longer than in.limit (storedLimit).
//
// ctx is the request's: the decode reads through a ctxReader and the encode
// writes into a storedBuffer that both fail once it is done, each format
// checks it again once its decode is done, and playGIF checks it between
// frames, so a costly picture stops being processed soon after the post that
// asked for it is cancelled or out of time rather than running to the end
// first — which is what lets processHeld give the budget back. A decode is
// seen to stop only at its next read, though, and some read all their input
// before their costly work: image/jpeg reads a small progressive file whole
// before its passes over the blocks, and x/image's lossy WebP decoder its
// partitions before it decodes them. Such a decode runs to its end once it
// has started, a few seconds of CPU at most — what the pixel cap, the memory
// estimate and the scan revisit cap bound — while holding its share of the
// budget, and then nothing more is done. A decode or encode a cancellation
// cut off surfaces as the format's own error; sanitize reports the context's
// error instead when it is what stopped the work, so Process and Save map it
// to the right answer (a deadline to 503, a cancellation to the client having
// gone) rather than to ErrCorruptImage. While ctx is live nothing it does
// changes the stored bytes.
func sanitize(ctx context.Context, in inspection, data []byte) (sanitized, error) {
	out, err := sanitizeFormat(ctx, in, data)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return sanitized{}, ctxErr
		}
	}
	return out, err
}

func sanitizeFormat(ctx context.Context, in inspection, data []byte) (sanitized, error) {
	switch in.format.Name {
	case FormatJPEG.Name:
		return sanitizeJPEG(ctx, data, in.limit)
	case FormatPNG.Name:
		return sanitizePNG(ctx, data, in.limit)
	case FormatGIF.Name:
		return sanitizeGIF(ctx, data, in.limit, in.frames)
	case FormatWebP.Name:
		return sanitizeWebP(ctx, data, in.limit)
	}
	return sanitized{}, ErrUnsupportedFormat
}

// ctxReader makes a reader fail once ctx is done, so a decode reading an
// upload through it stops promptly when the request is cancelled or out of
// time instead of reading the whole file first. While ctx is live it is the
// reader unchanged, so what the decoder sees, and so what is stored, is byte
// for byte what it would be without it.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
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
func sanitizeJPEG(ctx context.Context, data []byte, limit int64) (sanitized, error) {
	src, err := jpeg.Decode(ctxReader{ctx, bytes.NewReader(data)})
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	if err := ctx.Err(); err != nil {
		return sanitized{}, err
	}
	if cmyk, ok := src.(*image.CMYK); ok {
		src = rgbaOfCMYK(cmyk)
	}
	o := jpegOrientation(data)
	pic := orient(src, o)
	f, framed := readJPEGFrame(data)
	buf := newStoredBuffer(ctx, len(data), limit)
	stored, err := buf.encoded(FormatJPEG, jpegenc.Encode(buf, pic, &jpegenc.Options{Quality: jpegQuality(f, framed, o)}))
	if err != nil {
		return sanitized{}, err
	}
	return sanitized{format: FormatJPEG, data: stored, pic: pic}, nil
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
func sanitizePNG(ctx context.Context, data []byte, limit int64) (sanitized, error) {
	src, err := png.Decode(ctxReader{ctx, bytes.NewReader(data)})
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	if err := ctx.Err(); err != nil {
		return sanitized{}, err
	}
	if gamma, ok := pngGamma(data); ok {
		applyPNGGamma(src, gamma)
	}
	reduceHiddenColour(src)
	pic := orient(src, pngOrientation(data))
	buf := newStoredBuffer(ctx, len(data), limit)
	stored, err := buf.encoded(FormatPNG, storedPNG.Encode(buf, pic))
	if err != nil {
		return sanitized{}, err
	}
	return sanitized{format: FormatPNG, data: stored, pic: pic}, nil
}

// storedPNG writes every stored PNG — a PNG upload's, a lossless WebP's and a
// transparent lossy WebP's — at image/png's BestSpeed, which deflates in one
// pass that tries one earlier place for each match, so that how long the
// encoding takes depends on how many bytes the picture's rows come to and
// little on what they hold. At the default level deflate tries up to 128
// earlier places for each match, and rows of a few repeated byte values make it
// try them all at almost every byte: measured in CPU time on one core, a
// 25-megapixel 16-bit PNG of 1.4 MB took 16 to 18 s to process that way, and an
// 8-bit one of 0.7 MB 8 to 9 s. At BestSpeed each takes under 3 s, and the
// slowest PNG measured, a 16-bit one of 25 megapixels, 4 to 5 s
// (processingAllowance). The cost is size, and with it some pictures the
// default level stored. The PNGs of photos and screenshots tried were stored 1%
// to 80% larger than at the default level, most of them about a tenth; of the
// 573 PNGs stored from a mixed corpus of 14,015 uploads, PNG uploads a median
// of 5% larger and at most 2.2 times, and WebPs stored as PNGs a median of 11%.
// Synthetic pictures grow more: grey 16-bit ones of 25 megapixels 1.3 times
// when flat or graded along one axis, 1.6 times graded radially and 3.8 to 4.6
// times graded diagonally, and rows crafted for it 11 times. So a picture the
// default level stored close to storedLimit can come out past it here, and is
// refused (ErrStoredTooLarge) where it was stored before: a 10000×2500 16-bit
// gradient of 1.7 MB, its rows filtered Up and deflated at zlib's highest
// level, came to 97% of its limit at the default level, in 12 s of CPU, and
// comes to 159% of it here. A thumbnail's rows are a few hundred kilobytes at
// most, and it keeps the default level.
var storedPNG = png.Encoder{CompressionLevel: png.BestSpeed}

// sanitizeGIF decodes every frame of a GIF and stores what they show, and
// nothing else (playGIF): each picture, for as long as it shows, and the loop
// count, written by one rule, so that two GIFs Chromium shows alike are
// stored as the same bytes however their frames made what they show. Comment
// extensions, every application extension (XMP, for one) and plain text are
// dropped, but for the loop count, which is written as NETSCAPE2.0 whichever
// of the two loop extensions it came in (gifFramePixels reads it). The
// thumbnail is of the first picture the GIF shows, at the size of its canvas,
// made before its frames are played. A GIF that cannot be stored one frame
// to a picture, because a stored frame would need more colours than one
// table holds, is refused, as is one whose stored frames would be charged
// more between them than maxGIFFramePixels (gifCharge; ErrImageTooCostly,
// from playGIF).
//
// A frame's colours, delay and disposal method are taken from what the walk
// read of it (gifFrameHead), as Chromium reads them, where that and
// image/gif part ways: a frame with no graphic control extension of its own
// has disposal 0, where image/gif gives it the disposal of the last frame
// that had one; its transparent index is the last extension's before it, or
// none, where image/gif keeps one an earlier extension named; and an index
// past the end of its colour table is opaque black but the transparent one,
// where image/gif makes all of them up to a transparent index past the table
// transparent. image/gif takes such an index only up to a transparent index
// that is itself past the table: a frame whose pixels index past their table
// otherwise is refused here (ErrCorruptImage), as the decoder refuses it,
// though Chromium shows those pixels black. Two kinds of file image/gif
// refuses and browsers show are read as browsers read them as well. A file
// that ends after its last block without a trailer plays in a browser, and
// is decoded with one put after it; a frame after the first that reaches
// past the canvas is drawn by a browser only where it overlaps the canvas,
// which is all playGIF plays of it, and image/gif, which refuses the whole
// file, is told the logical screen takes in every frame (gifReader). A first
// frame past the canvas is refused by gifFramePixels, because browsers
// disagree on what to show for it, and so is a file with an extension
// Chromium ends somewhere other than image/gif does.
func sanitizeGIF(ctx context.Context, data []byte, limit int64, frames gifFrames) (sanitized, error) {
	g, err := gif.DecodeAll(ctxReader{ctx, gifReader(data, frames)})
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	heads := make([]gifFrameHead, 0, frames.count)
	if _, err := walkGIF(data, math.MaxInt64, func(h gifFrameHead) { heads = append(heads, h) }); err != nil || len(heads) != len(g.Image) {
		return sanitized{}, ErrCorruptImage
	}
	canvas := image.Rect(0, 0, frames.canvas.X, frames.canvas.Y)
	// The thumbnail is of the first picture the GIF shows, its first frame
	// on the canvas, made before the frames are played.
	first := newFirstGIFPicture(g.Image[0], heads[0], canvas)
	thumb, err := makeThumbnail(first, FormatGIF)
	if err != nil {
		return sanitized{}, err
	}
	loop := -1
	if frames.looped {
		loop = frames.loop
	}
	buf := newStoredBuffer(ctx, len(data), limit)
	err = playGIF(ctx, g.Image, heads, canvas, loop, buf)
	if errors.Is(err, ErrImageTooCostly) {
		return sanitized{}, err
	}
	stored, err := buf.encoded(FormatGIF, err)
	if err != nil {
		return sanitized{}, err
	}
	return sanitized{format: FormatGIF, data: stored, pic: first, thumb: &thumb}, nil
}

// firstGIFPicture is the first picture a GIF shows, at the size of its
// canvas: its first frame, which gifFramePixels holds to the canvas, drawn
// where it is on a canvas that is otherwise transparent, each index in the
// colour Chromium reads it as (gifFrameHead.keys). It is an
// image.RGBA64Image, which the thumbnail scaler reads a pixel at a time
// without making a color.Color of each.
type firstGIFPicture struct {
	frame   *image.Paletted
	colours [256]color.RGBA64
	canvas  image.Rectangle
}

func newFirstGIFPicture(frame *image.Paletted, head gifFrameHead, canvas image.Rectangle) *firstGIFPicture {
	p := &firstGIFPicture{frame: frame, canvas: canvas}
	for v, k := range head.keys() {
		if k != 0 {
			p.colours[v] = color.RGBA64{uint16(k>>16&0xFF) * 0x101, uint16(k>>8&0xFF) * 0x101, uint16(k&0xFF) * 0x101, 0xFFFF}
		}
	}
	return p
}

func (p *firstGIFPicture) ColorModel() color.Model { return color.RGBA64Model }
func (p *firstGIFPicture) Bounds() image.Rectangle { return p.canvas }
func (p *firstGIFPicture) At(x, y int) color.Color { return p.RGBA64At(x, y) }
func (p *firstGIFPicture) RGBA64At(x, y int) color.RGBA64 {
	if !image.Pt(x, y).In(p.frame.Rect) {
		return color.RGBA64{}
	}
	return p.colours[p.frame.Pix[p.frame.PixOffset(x, y)]]
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
// animated WebP does not get here: x/image decodes still images only, so
// Inspect refuses one from its chunks (webpAnimated), and one stripWebP finds
// animated is refused here the same way.
//
// Stripping alone is not enough to store. A decoder stops reading a VP8, VP8L
// or ALPH chunk once it has its picture and ignores whatever else the chunk
// holds, so a whole second file — another image, its own EXIF and all — can
// sit inside a chunk that stripping has to keep, and would be stored and
// served byte for byte. Nobody would see it on screen, moderators and image
// matching included, which is what makes it a hiding place. Encoding the
// decoded pixels, with no colour kept under transparent ones, leaves only
// what is shown, as for the other formats.
func sanitizeWebP(ctx context.Context, data []byte, limit int64) (sanitized, error) {
	stripped, info, err := stripWebP(data)
	if err != nil {
		return sanitized{}, err
	}
	if info.Animated {
		return sanitized{}, ErrCorruptImage
	}
	pic, err := webp.Decode(ctxReader{ctx, bytes.NewReader(stripped)})
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	if err := ctx.Err(); err != nil {
		return sanitized{}, err
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
	buf := newStoredBuffer(ctx, len(data), limit)
	switch p := pic.(type) {
	case *image.YCbCr: // lossy and opaque
		format = FormatJPEG
		expandLimitedRange(p)
		err = jpegenc.Encode(buf, p, &jpegenc.Options{Quality: storedJPEGQuality})
	case *image.NYCbCrA: // lossy with transparency
		rgb := lossyWebPToNRGBA(p)
		pic = rgb
		err = storedPNG.Encode(buf, rgb)
	default: // lossless, decoded as *image.NRGBA
		reduceHiddenColour(pic)
		err = storedPNG.Encode(buf, pic)
	}
	stored, err := buf.encoded(format, err)
	if err != nil {
		return sanitized{}, err
	}
	return sanitized{format: format, data: stored, pic: pic}, nil
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
// a pixel that names an entry past its palette's end (as image/png pads a
// short table), transparent black for the one transparent entry. They are
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
// hold a whole second file in the pixel bytes.
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
// past the palette's end as opaque black, as image/png pads a short table.
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
	foldPaletted(m, firstUses(m), f) // a lone picture never overflows
	if len(f.palette) == 0 {
		f.palette = color.Palette{transparentEntry} // a picture of no pixels
	}
	m.Palette = f.palette
}

// firstUses is the palette indices m's pixels use, each once, in the order
// its pixels first use them.
func firstUses(m *image.Paletted) []uint8 {
	var seen [256]bool
	var uses []uint8
	b := m.Rect
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for _, v := range m.Pix[m.PixOffset(b.Min.X, y):m.PixOffset(b.Max.X, y)] {
			if !seen[v] {
				seen[v] = true
				uses = append(uses, v)
			}
		}
	}
	return uses
}

// foldPaletted folds the colours of the indices m's pixels use (uses, in
// first-use order) into f, and remaps the pixels to their colours' indices
// there. It leaves m's palette as it was: the caller gives it f's. Folding
// the indices in the order the pixels first use them adds their colours in
// the order the pixels first show them, so the table comes out the same as
// folding pixel by pixel would make it.
func foldPaletted(m *image.Paletted, uses []uint8, f *colourFolder) {
	var to [256]uint8
	for _, v := range uses {
		i, _ := f.fold(paletteEntryAt(m.Palette, v))
		to[v] = uint8(i)
	}
	b := m.Rect
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := m.Pix[m.PixOffset(b.Min.X, y):m.PixOffset(b.Max.X, y)]
		for j, v := range row {
			row[j] = to[v]
		}
	}
}

// gifFrames is what a GIF's frames come to: their area together, and how
// many there are, and how often they are played; and what gifReader needs
// to have image/gif decode them.
type gifFrames struct {
	pixels, count int64
	// charged is what the frames are charged against maxGIFFramePixels
	// (gifCharge): their pixels, with each row at least gifRowPixels.
	charged int64
	looped  bool // a loop extension gave a loop count,
	loop    int  // which is this, as gif.GIF's LoopCount reads
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
	// many is whether the frames' colour tables, as Chromium reads them
	// (gifFrameHead.keys), give more than 255 colours between them: each
	// table's entries but the frame's transparent one, and black for a
	// table of fewer than 256 entries, which an index past its end shows.
	// Only then can the frames draw more colours than one table holds
	// beside a transparent entry, and only then may playGIF put a stored
	// frame back (gifPlayScratch).
	many bool
	// apart is whether the walk refused the file at an extension that
	// Chromium ends somewhere other than where image/gif does (walkGIF), so
	// FuzzGIFFramePixels can tell that refusal from a disagreement with the
	// decoder.
	apart bool
}

// gifFramePixels adds up the area of every frame a GIF declares, and its
// charge (gifCharge), and counts them, reading only the block structure:
// image descriptors are read, and everything else, compressed pixels
// included, is stepped over. It allocates nothing.
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
// Reading the file as image/gif does is not enough on its own, because the
// stored file is held to what Chromium shows, and Chromium ends three forms
// of extension somewhere else: it reads a plain-text extension's block size
// byte as the length of a sub-block, which is the same thirteen bytes only
// when that byte is 12; it ends an application extension whose block size
// byte is 0 straight after it, where image/gif reads sub-blocks after it; and
// it takes the terminator straight after a NETSCAPE2.0 or ANIMEXTS1.0
// identifier for the loop sub-block and reads sub-blocks on after it, where
// image/gif ends the extension there. Past such a place the two read the
// file differently; Chromium meets a byte that starts no block and shows
// only the frames before it, while image/gif decodes on, so frames the
// upload never showed in a browser would be stored and shown. The walk
// refuses all three (ErrCorruptImage, with apart set) rather than store
// either reading: none is what the specification writes, and a file that
// shows a still in a browser must not be stored as an animation.
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
// It returns ErrImageTooCostly as soon as the charge passes limit, and
// ErrCorruptImage when the walk does not end at the trailer or at the end of
// a whole block: a file the decoder could not read to its end is refused
// rather than half counted. TestGIFFramePixelsReadsWhatTheDecoderReads and
// FuzzGIFFramePixels hold it to the decoder.
func gifFramePixels(data []byte, limit int64) (gifFrames, error) {
	return walkGIF(data, limit, nil)
}

// gifFrameHead is what a GIF says of one of its frames besides its pixels and
// where it is, read as Chromium reads it, which is not always as image/gif
// does (sanitizeGIF). The disposal method, the delay and the transparent
// index are the last graphic control extension's since the frame before it;
// a frame with none has delay 0, disposal 0 and no transparent index.
type gifFrameHead struct {
	disposal    byte // the disposal method as written, 0 to 7
	delay       int  // in hundredths of a second, as written
	transparent bool // whether it names a transparent index,
	ti          byte // which is this
	// table is the colour table the frame's pixels index, three bytes an
	// entry: its own, or the global one; nil when there is neither, which
	// image/gif refuses.
	table []byte
}

// walkGIF is gifFramePixels, also calling frame, unless it is nil, with each
// frame's head (gifFrameHead) as the walk reaches the frame.
func walkGIF(data []byte, limit int64, frame func(gifFrameHead)) (gifFrames, error) {
	const (
		headerLen     = 13 // signature, version, logical screen descriptor
		descriptorLen = 10 // separator, position, size, flags
		graphicLen    = 6  // block size (4), flags, delay, transparent index, terminator
		plainTextLen  = 13 // read whole by image/gif, whatever its block size says (but see above)
		maxSide       = 0xFFFF
	)
	if len(data) < headerLen {
		return gifFrames{}, ErrCorruptImage
	}
	var f gifFrames
	f.canvas = image.Pt(int(binary.LittleEndian.Uint16(data[6:])), int(binary.LittleEndian.Uint16(data[8:])))
	f.screen = f.canvas
	i := headerLen
	var global []byte
	if flags := data[10]; flags&0x80 != 0 {
		i += 3 << (1 + flags&0x07)
		if i <= len(data) {
			global = data[headerLen:i]
		}
	}
	var head gifFrameHead // what the extensions since the last frame said
	// colours is the colours the frames' tables give (gifFrames.many),
	// gathered until there are more than 255. The global table is gathered
	// once with each transparent index frames give it (gathered[i+1]), and
	// once with none (gathered[0]).
	var colours colourIndex
	var gathered [257]bool
	gather := func(table []byte, transparent bool, ti byte) {
		n := len(table) / 3
		for e := 0; e < n && !f.many; e++ {
			if !transparent || e != int(ti) {
				k := opaqueKey | uint32(table[3*e])<<16 | uint32(table[3*e+1])<<8 | uint32(table[3*e+2])
				f.many = colours.add(k, 0) && colours.n > 255
			}
		}
		if n < 256 && !f.many {
			f.many = colours.add(opaqueKey, 0) && colours.n > 255
		}
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
				flags := data[at+1]
				head.disposal = flags >> 2 & 0x07
				head.delay = int(binary.LittleEndian.Uint16(data[at+2:]))
				head.transparent, head.ti = flags&0x01 != 0, data[at+4]
				i = at + graphicLen
			case 0x01: // plain text
				if at < len(data) && data[at] != 12 {
					// Chromium reads the size byte as a sub-block's length,
					// which spans the thirteen bytes only when it is 12.
					f.apart = true
					return f, ErrCorruptImage
				}
				i, whole = skipBlocks(at + plainTextLen)
			case 0xFF: // application: its own length byte, then sub-blocks
				if at >= len(data) {
					return f, ErrCorruptImage
				}
				size := int(data[at])
				if size == 0 {
					// Chromium ends the extension here.
					f.apart = true
					return f, ErrCorruptImage
				}
				if j := at + 1 + size; size == 11 && j < len(data) {
					if id := string(data[at+1 : j]); id == "NETSCAPE2.0" || id == "ANIMEXTS1.0" {
						if data[j] == 0 {
							// Chromium takes the terminator for the loop
							// sub-block and reads on past it.
							f.apart = true
							return f, ErrCorruptImage
						}
						if j+4 <= len(data) && data[j] == 3 && data[j+1] == 1 {
							f.looped, f.loop = true, int(data[j+2])|int(data[j+3])<<8
						}
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
			f.charged += gifCharge(int64(w), int64(h))
			f.largest = max(f.largest, int64(w)*int64(h))
			if f.charged > limit {
				return f, ErrImageTooCostly
			}
			i += descriptorLen
			head.table = global
			if flags&0x80 != 0 {
				at := i
				i += 3 << (1 + flags&0x07)
				head.table = data[at:min(i, len(data))]
				gather(head.table, head.transparent, head.ti)
			} else if g := b2i(head.transparent) * (1 + int(head.ti)); !gathered[g] {
				gathered[g] = true
				gather(global, head.transparent, head.ti)
			}
			// The LZW minimum code size, which image/gif holds to 2–8, and
			// then the compressed pixels.
			if i >= len(data) || data[i] < 2 || data[i] > 8 {
				return f, ErrCorruptImage
			}
			if frame != nil {
				frame(head)
			}
			head = gifFrameHead{}
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
