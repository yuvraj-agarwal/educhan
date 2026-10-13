package media

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/webp"
)

// What is stored as an upload's original is never the file as uploaded. A
// photo carries more than its pixels: EXIF (the camera, the time to the
// second, and often the GPS position it was taken at), XMP (the same again,
// plus an editor's history), IPTC captions and credits, comments, and colour
// profiles that can name the device. Anyone who opened the full image could
// read all of it. So every upload is rewritten here, in Process, before either
// storage backend sees a byte of it:
//
//   - JPEG is decoded, turned upright by its EXIF orientation, and re-encoded.
//   - PNG is decoded and re-encoded, which keeps none of its ancillary chunks.
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

// storedJPEGQuality is the quality a JPEG original is re-encoded at: high
// enough that a second generation is hard to tell from the first, which is
// what a poster sees of their own photo.
const storedJPEGQuality = 90

// maxGIFFramePixels bounds the pixels of all a GIF's frames together. The
// header check holds each frame to the canvas, and the canvas to MaxPixels,
// but every frame is decoded and kept at a byte a pixel until it is
// re-encoded, and a frame of one flat colour compresses to almost nothing: a
// few megabytes of GIF could otherwise ask for gigabytes. Four
// canvases' worth is a long animation at any ordinary size (a 640×480 GIF can
// have 300 frames) and no more memory than the largest JPEG.
const maxGIFFramePixels = 4 * MaxPixels

// processingSlots is how many uploads are decoded and re-encoded at once.
// An image at MaxPixels can take a few hundred megabytes while it is turned
// into its stored form, on an instance with 512 MiB in all and many requests
// in flight, so uploads queue for a slot instead of each taking that much at
// the same time. A waiting upload gives up when its request does. It is a
// variable so tests can narrow it; nothing else changes it.
var processingSlots = make(chan struct{}, 2)

// acquireProcessing waits for a processing slot and returns the function that
// gives it back, or the context's error if the context ends first.
func acquireProcessing(ctx context.Context) (func(), error) {
	slots := processingSlots
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// sanitized is an upload rewritten for storage.
type sanitized struct {
	format Format          // what data is, which for a WebP upload is PNG or JPEG
	data   []byte          // the bytes to store
	pic    image.Image     // what to thumbnail: the picture, or a GIF's first frame
	canvas image.Rectangle // the whole picture, which pic may only partly cover
}

func sanitize(format Format, data []byte) (sanitized, error) {
	switch format.Name {
	case FormatJPEG.Name:
		return sanitizeJPEG(data)
	case FormatPNG.Name:
		return sanitizePNG(data)
	case FormatGIF.Name:
		return sanitizeGIF(data)
	case FormatWebP.Name:
		return sanitizeWebP(data)
	}
	return sanitized{}, ErrUnsupportedFormat
}

// sanitizeJPEG decodes a JPEG, applies its EXIF orientation to the pixels, and
// re-encodes it. image/jpeg writes no APP or COM segments at all, so EXIF,
// XMP, IPTC and Photoshop data, comments, and the ICC profile are all gone
// from the result.
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
func sanitizeJPEG(data []byte) (sanitized, error) {
	src, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	pic := orient(src, jpegOrientation(data))
	buf := bytes.NewBuffer(make([]byte, 0, len(data)))
	if err := jpeg.Encode(buf, pic, &jpeg.Options{Quality: storedJPEGQuality}); err != nil {
		return sanitized{}, fmt.Errorf("media: encode jpeg: %w", err)
	}
	return sanitized{format: FormatJPEG, data: buf.Bytes(), pic: pic, canvas: pic.Bounds()}, nil
}

// sanitizePNG decodes and re-encodes a PNG. image/png writes only the chunks
// the pixels need (IHDR, PLTE and tRNS when there is a palette or a
// transparent colour, IDAT, IEND), so text of every kind (tEXt, zTXt, and iTXt,
// which is where XMP goes), eXIf, tIME, colour chunks and anything private are
// left behind. The pixels are kept exactly, at the bit depth they came in. An
// animated PNG keeps its still image, which is what a viewer without APNG
// support has always shown.
func sanitizePNG(data []byte) (sanitized, error) {
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	buf := bytes.NewBuffer(make([]byte, 0, len(data)))
	if err := png.Encode(buf, src); err != nil {
		return sanitized{}, fmt.Errorf("media: encode png: %w", err)
	}
	return sanitized{format: FormatPNG, data: buf.Bytes(), pic: src, canvas: src.Bounds()}, nil
}

// sanitizeGIF decodes every frame of a GIF and encodes them again. The frames,
// their positions, palettes and transparency, their delays and disposal, the
// background colour and the loop count all survive; comment extensions and
// every application extension but the NETSCAPE2.0 one carrying the loop count
// (XMP, for one) do not, because image/gif reads past them and never writes
// them. The thumbnail is of the first frame.
func sanitizeGIF(data []byte) (sanitized, error) {
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return sanitized{}, ErrCorruptImage
	}
	buf := bytes.NewBuffer(make([]byte, 0, len(data)))
	if err := gif.EncodeAll(buf, g); err != nil {
		return sanitized{}, fmt.Errorf("media: encode gif: %w", err)
	}
	return sanitized{
		format: FormatGIF,
		data:   buf.Bytes(),
		pic:    g.Image[0],
		canvas: image.Rect(0, 0, g.Config.Width, g.Config.Height),
	}, nil
}

// sanitizeWebP checks a WebP's structure and strips it to the chunks that
// make the picture (stripWebP), decodes what is left, and stores the pixels as
// a new file: a JPEG when the picture is lossy and opaque, which is what a
// lossy WebP is closest to, and otherwise a PNG, which keeps a lossless
// picture exactly and a transparent one's alpha. An animated WebP is refused
// as it always has been; x/image decodes still images only.
//
// Stripping alone is not enough to store. A decoder stops reading a VP8, VP8L
// or ALPH chunk once it has its picture and ignores whatever else the chunk
// holds, so a whole second file — another image, its own EXIF and all — can
// sit inside a chunk that stripping has to keep, and would be stored and
// served byte for byte. Nobody would see it on screen, moderators and image
// matching included, which is what makes it a hiding place. Encoding the
// decoded pixels leaves nothing of the upload, as for the other formats.
func sanitizeWebP(data []byte) (sanitized, error) {
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
	buf := bytes.NewBuffer(make([]byte, 0, len(data)))
	switch m := pic.(type) {
	case *image.YCbCr: // lossy, with no alpha
		format = FormatJPEG
		err = jpeg.Encode(buf, m, &jpeg.Options{Quality: storedJPEGQuality})
	case *image.NYCbCrA: // lossy, with an alpha plane
		if m.Opaque() {
			format = FormatJPEG
			pic = &m.YCbCr
			err = jpeg.Encode(buf, pic, &jpeg.Options{Quality: storedJPEGQuality})
			break
		}
		pic = nrgbaOf(m)
		err = png.Encode(buf, pic)
	default: // lossless, decoded as *image.NRGBA
		err = png.Encode(buf, pic)
	}
	if err != nil {
		return sanitized{}, fmt.Errorf("media: encode webp as %s: %w", format.Name, err)
	}
	return sanitized{format: format, data: buf.Bytes(), pic: pic, canvas: pic.Bounds()}, nil
}

// nrgbaOf converts a lossy picture with alpha to plain non-premultiplied RGBA,
// which image/png encodes directly rather than a pixel at a time through the
// colour interfaces. Each pixel's colour is converted exactly as
// color.YCbCr does, and its alpha is copied.
func nrgbaOf(m *image.NYCbCrA) *image.NRGBA {
	b := m.Bounds()
	out := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := out.Pix[(y-b.Min.Y)*out.Stride:]
		for x := b.Min.X; x < b.Max.X; x++ {
			c := m.NYCbCrAAt(x, y)
			r, g, bl, _ := c.YCbCr.RGBA()
			i := (x - b.Min.X) * 4
			row[i+0] = uint8(r >> 8)
			row[i+1] = uint8(g >> 8)
			row[i+2] = uint8(bl >> 8)
			row[i+3] = c.A
		}
	}
	return out
}

// gifFramePixels adds up the area of every frame a GIF declares, reading only
// the block structure: image descriptors are read, and everything else,
// compressed pixels included, is stepped over. It allocates nothing.
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
// It returns ErrImageTooLarge as soon as the total passes limit, and
// ErrCorruptImage when the walk does not end at the trailer: a file the
// decoder could not read to its end is refused rather than half counted.
// TestGIFFramePixelsReadsWhatTheDecoderReads and FuzzGIFFramePixels hold it to
// the decoder.
func gifFramePixels(data []byte, limit int64) (int64, error) {
	const (
		headerLen     = 13 // signature, version, logical screen descriptor
		descriptorLen = 10 // separator, position, size, flags
		graphicLen    = 6  // block size (4), flags, delay, transparent index, terminator
		plainTextLen  = 13 // read whole by image/gif, whatever its block size says
	)
	if len(data) < headerLen {
		return 0, ErrCorruptImage
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

	var total, frames int64
	for i < len(data) {
		switch data[i] {
		case 0x21: // extension: introducer, label, then as the label says
			if i+2 > len(data) {
				return total, ErrCorruptImage
			}
			at := i + 2
			switch data[i+1] {
			case 0xF9: // graphic control
				if at+graphicLen > len(data) || data[at] != 4 || data[at+5] != 0 {
					return total, ErrCorruptImage
				}
				i = at + graphicLen
			case 0x01: // plain text
				i = skipBlocks(at + plainTextLen)
			case 0xFF: // application: its own length byte, then sub-blocks
				if at >= len(data) {
					return total, ErrCorruptImage
				}
				i = skipBlocks(at + 1 + int(data[at]))
			case 0xFE: // comment
				i = skipBlocks(at)
			default: // image/gif refuses an extension it does not know
				return total, ErrCorruptImage
			}
		case 0x2C: // image descriptor
			if i+descriptorLen > len(data) {
				return total, ErrCorruptImage
			}
			w := int64(data[i+5]) | int64(data[i+6])<<8
			h := int64(data[i+7]) | int64(data[i+8])<<8
			flags := data[i+9]
			frames++
			total += w * h
			if total > limit {
				return total, ErrImageTooLarge
			}
			i += descriptorLen
			if flags&0x80 != 0 {
				i += 3 << (1 + flags&0x07)
			}
			// The LZW minimum code size, which image/gif holds to 2–8, and
			// then the compressed pixels.
			if i >= len(data) || data[i] < 2 || data[i] > 8 {
				return total, ErrCorruptImage
			}
			i = skipBlocks(i + 1)
		case 0x3B: // the trailer, where the decoder stops reading
			if frames == 0 { // which it refuses before any frame
				return total, ErrCorruptImage
			}
			return total, nil
		default: // a block image/gif refuses
			return total, ErrCorruptImage
		}
	}
	return total, ErrCorruptImage
}
