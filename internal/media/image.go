package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // GIF decoding
	"image/jpeg"
	"image/png"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // WebP decoding (the standard library has none)
)

// ThumbMaxDim is the longest-side bound, in pixels, of a generated thumbnail.
const ThumbMaxDim = 250

// MaxPixels caps the decoded resolution of an upload. Compressed formats can
// describe enormous canvases in a handful of bytes, so the dimensions are
// checked before the image is materialised in memory. 24 megapixels covers
// what most phone cameras save by default. Decoding a JPEG that size, turning
// it upright and re-encoding it allocates about 160 MB along the way, where the
// 50 megapixels allowed before could need 275 MB live at once, on an instance
// with 512 MiB in all.
const MaxPixels = 24_000_000

// thumbJPEGQuality is used when a thumbnail is encoded as JPEG.
const thumbJPEGQuality = 82

// Errors reported for rejected uploads. Handlers translate these into 4xx
// responses, so their text is user-facing.
var (
	ErrUnsupportedFormat = errors.New("unsupported image format: use JPEG, PNG, WebP, or GIF")
	ErrImageTooLarge     = errors.New("image resolution is too large")
	ErrCorruptImage      = errors.New("image could not be decoded")
)

// Format is one of the image types educhan accepts.
type Format struct {
	Name        string // canonical short name, e.g. "jpeg"
	ContentType string
	Ext         string
}

// Supported formats, in the order magic bytes are tested.
var (
	FormatJPEG = Format{Name: "jpeg", ContentType: "image/jpeg", Ext: ".jpg"}
	FormatPNG  = Format{Name: "png", ContentType: "image/png", Ext: ".png"}
	FormatGIF  = Format{Name: "gif", ContentType: "image/gif", Ext: ".gif"}
	FormatWebP = Format{Name: "webp", ContentType: "image/webp", Ext: ".webp"}
)

// DetectFormat identifies an upload from its leading bytes. Extensions and
// client-supplied content types are never trusted.
func DetectFormat(data []byte) (Format, bool) {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return FormatJPEG, true
	case bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return FormatPNG, true
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return FormatGIF, true
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return FormatWebP, true
	}
	return Format{}, false
}

// ContentTypeForExt maps a stored file extension back to its media type. Only
// extensions produced by this package are recognised.
func ContentTypeForExt(ext string) string {
	switch ext {
	case FormatJPEG.Ext:
		return FormatJPEG.ContentType
	case FormatPNG.Ext:
		return FormatPNG.ContentType
	case FormatGIF.Ext:
		return FormatGIF.ContentType
	case FormatWebP.Ext:
		return FormatWebP.ContentType
	}
	return "application/octet-stream"
}

// Thumbnail is a downscaled rendition of an upload.
type Thumbnail struct {
	Data   []byte
	Format Format
	Width  int
	Height int
}

// Processed is an upload validated, rewritten for storage, and thumbnailed.
type Processed struct {
	Format Format
	// Data is the original as it is to be stored: re-encoded, or for WebP
	// stripped chunk by chunk, so that it carries no metadata (sanitize.go).
	Data []byte
	// Width and Height are the stored picture's, which for a JPEG is the
	// picture after its EXIF orientation has been applied.
	Width  int
	Height int
	Thumb  Thumbnail
}

// Inspect checks what an upload says about itself without decoding it: its
// format, by magic bytes, and its dimensions, from its header. That is cheap
// enough to run before flood control, so a file that is not an accepted image,
// or one too large to decode, can be refused without costing its poster a
// cooldown. It cannot tell whether the pixels decode; only Process can, and
// Process runs it first.
func Inspect(data []byte) (Format, error) {
	format, ok := DetectFormat(data)
	if !ok {
		return Format{}, ErrUnsupportedFormat
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Format{}, ErrCorruptImage
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Format{}, ErrCorruptImage
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return Format{}, ErrImageTooLarge
	}
	// Every frame of a GIF is decoded, so their sizes are added up as well.
	// Skipping over the blocks is cheap enough to do here, and an animation
	// too long to decode is refused like an image too large to.
	if format == FormatGIF && gifFramePixels(data, maxGIFFramePixels) > maxGIFFramePixels {
		return Format{}, ErrImageTooLarge
	}
	return format, nil
}

// Process validates the raw upload, rewrites it without its metadata, and
// derives its thumbnail from the rewritten picture. Animated GIFs are
// thumbnailed from their first frame.
//
// The decoding and encoding wait for a processing slot (processingSlots), and
// the wait ends with the context's error if ctx is done first.
func Process(ctx context.Context, data []byte) (Processed, error) {
	format, err := Inspect(data)
	if err != nil {
		return Processed{}, err
	}

	release, err := acquireProcessing(ctx)
	if err != nil {
		return Processed{}, err
	}
	defer release()

	out, err := sanitize(format, data)
	if err != nil {
		return Processed{}, err
	}
	thumb, err := makeThumbnail(out.pic, out.canvas, format)
	if err != nil {
		return Processed{}, err
	}
	return Processed{
		Format: format,
		Data:   out.data,
		Width:  out.canvas.Dx(),
		Height: out.canvas.Dy(),
		Thumb:  thumb,
	}, nil
}

// ThumbSize scales w×h to fit within max on the longest side, preserving the
// aspect ratio. Images already within the bound are left alone rather than
// upscaled.
func ThumbSize(w, h, max int) (int, int) {
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	if w <= max && h <= max {
		return w, h
	}
	if w >= h {
		return max, atLeastOne(int(float64(h) * float64(max) / float64(w)))
	}
	return atLeastOne(int(float64(w) * float64(max) / float64(h))), max
}

func atLeastOne(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

// thumbFormat picks the encoding of a thumbnail. JPEG sources stay JPEG; the
// formats that may carry transparency become PNG so it is not lost.
func thumbFormat(src Format) Format {
	if src.Name == FormatJPEG.Name {
		return FormatJPEG
	}
	return FormatPNG
}

// makeThumbnail scales the canvas down to a thumbnail. src is drawn where it
// sits on the canvas: for everything but a GIF it is the whole canvas, and a
// GIF's first frame may cover only part of it, leaving the rest transparent.
func makeThumbnail(src image.Image, canvas image.Rectangle, format Format) (Thumbnail, error) {
	cw, ch := canvas.Dx(), canvas.Dy()
	tw, th := ThumbSize(cw, ch, ThumbMaxDim)

	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	b := src.Bounds()
	at := image.Rect(
		(b.Min.X-canvas.Min.X)*tw/cw, (b.Min.Y-canvas.Min.Y)*th/ch,
		(b.Max.X-canvas.Min.X)*tw/cw, (b.Max.Y-canvas.Min.Y)*th/ch,
	)
	// CatmullRom is slower than the nearest-neighbour kernels but keeps small
	// thumbnails of dense screenshots readable, which is what they are for.
	xdraw.CatmullRom.Scale(dst, at, src, b, xdraw.Src, nil)

	out := thumbFormat(format)
	var buf bytes.Buffer
	var err error
	switch out.Name {
	case FormatJPEG.Name:
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: thumbJPEGQuality})
	default:
		err = png.Encode(&buf, dst)
	}
	if err != nil {
		return Thumbnail{}, fmt.Errorf("media: encode thumbnail: %w", err)
	}
	return Thumbnail{Data: buf.Bytes(), Format: out, Width: tw, Height: th}, nil
}
