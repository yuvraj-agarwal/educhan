package media

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// testImage builds a deterministic gradient so decoders have something valid to
// chew on.
func testImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	return img
}

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, testImage(w, h)); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, testImage(w, h), nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func encodeGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.Encode(&buf, testImage(w, h), nil); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

func TestDetectFormat(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want Format
		ok   bool
	}{
		{"jpeg", encodeJPEG(t, 4, 4), FormatJPEG, true},
		{"png", encodePNG(t, 4, 4), FormatPNG, true},
		{"gif", encodeGIF(t, 4, 4), FormatGIF, true},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 8)...), FormatWebP, true},
		{"text", []byte("just some words, definitely not an image"), Format{}, false},
		{"empty", nil, Format{}, false},
		// A PNG extension cannot smuggle a non-image past magic-byte checks.
		{"disguised", []byte("GIF but not really"), Format{}, false},
		{"truncated riff", []byte("RIFF1234"), Format{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DetectFormat(tc.data)
			if ok != tc.ok {
				t.Fatalf("DetectFormat ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("DetectFormat = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestThumbSize(t *testing.T) {
	cases := []struct {
		w, h, max      int
		wantW, wantH   int
		describeAspect string
	}{
		{1000, 500, 250, 250, 125, "landscape"},
		{500, 1000, 250, 125, 250, "portrait"},
		{800, 800, 250, 250, 250, "square"},
		{100, 60, 250, 100, 60, "already smaller, no upscale"},
		{5000, 3, 250, 250, 1, "extreme ratio clamps to one pixel"},
	}
	for _, tc := range cases {
		t.Run(tc.describeAspect, func(t *testing.T) {
			gotW, gotH := ThumbSize(tc.w, tc.h, tc.max)
			if gotW != tc.wantW || gotH != tc.wantH {
				t.Fatalf("ThumbSize(%d,%d,%d) = %dx%d, want %dx%d",
					tc.w, tc.h, tc.max, gotW, gotH, tc.wantW, tc.wantH)
			}
		})
	}
}

func TestProcessGeneratesThumbnail(t *testing.T) {
	cases := []struct {
		name       string
		data       []byte
		wantFormat Format
		wantThumb  Format
	}{
		{"png stays png", encodePNG(t, 900, 300), FormatPNG, FormatPNG},
		{"jpeg stays jpeg", encodeJPEG(t, 300, 900), FormatJPEG, FormatJPEG},
		{"gif thumbnails as png", encodeGIF(t, 600, 600), FormatGIF, FormatPNG},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Process(tc.data)
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			if got.Format != tc.wantFormat {
				t.Errorf("format = %+v, want %+v", got.Format, tc.wantFormat)
			}
			if got.Thumb.Format != tc.wantThumb {
				t.Errorf("thumb format = %+v, want %+v", got.Thumb.Format, tc.wantThumb)
			}
			if got.Thumb.Width > ThumbMaxDim || got.Thumb.Height > ThumbMaxDim {
				t.Errorf("thumb %dx%d exceeds %d", got.Thumb.Width, got.Thumb.Height, ThumbMaxDim)
			}
			if got.Thumb.Width != ThumbMaxDim && got.Thumb.Height != ThumbMaxDim {
				t.Errorf("thumb %dx%d should touch the %dpx bound", got.Thumb.Width, got.Thumb.Height, ThumbMaxDim)
			}

			// The encoded thumbnail must decode to exactly the reported size.
			decoded, format, err := image.Decode(bytes.NewReader(got.Thumb.Data))
			if err != nil {
				t.Fatalf("decode thumbnail: %v", err)
			}
			if format != tc.wantThumb.Name {
				t.Errorf("decoded thumbnail format = %q, want %q", format, tc.wantThumb.Name)
			}
			if b := decoded.Bounds(); b.Dx() != got.Thumb.Width || b.Dy() != got.Thumb.Height {
				t.Errorf("decoded thumbnail is %dx%d, want %dx%d",
					b.Dx(), b.Dy(), got.Thumb.Width, got.Thumb.Height)
			}
		})
	}
}

func TestProcessPreservesAspectRatio(t *testing.T) {
	got, err := Process(encodePNG(t, 1000, 400))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got.Width != 1000 || got.Height != 400 {
		t.Fatalf("original recorded as %dx%d, want 1000x400", got.Width, got.Height)
	}
	srcRatio := 1000.0 / 400.0
	thumbRatio := float64(got.Thumb.Width) / float64(got.Thumb.Height)
	if diff := srcRatio - thumbRatio; diff > 0.02 || diff < -0.02 {
		t.Fatalf("thumb ratio %.3f differs from source ratio %.3f", thumbRatio, srcRatio)
	}
}

func TestProcessSmallImageIsNotUpscaled(t *testing.T) {
	got, err := Process(encodePNG(t, 40, 20))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got.Thumb.Width != 40 || got.Thumb.Height != 20 {
		t.Fatalf("thumb = %dx%d, want 40x20", got.Thumb.Width, got.Thumb.Height)
	}
}

// The standard library cannot decode WebP, so the fixture guards the
// golang.org/x/image/webp decoder actually being wired in.
func TestProcessWebP(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "sample.webp"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if format, ok := DetectFormat(data); !ok || format != FormatWebP {
		t.Fatalf("DetectFormat = %+v, %v; want WebP", format, ok)
	}
	got, err := Process(data)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got.Width != 320 || got.Height != 200 {
		t.Errorf("dimensions = %dx%d, want 320x200", got.Width, got.Height)
	}
	// WebP may carry alpha, so its thumbnail is PNG.
	if got.Thumb.Format != FormatPNG {
		t.Errorf("thumb format = %+v, want PNG", got.Thumb.Format)
	}
	if got.Thumb.Width != 250 || got.Thumb.Height != 156 {
		t.Errorf("thumb = %dx%d, want 250x156", got.Thumb.Width, got.Thumb.Height)
	}
}

func TestProcessAnimatedGIFUsesFirstFrame(t *testing.T) {
	frames := &gif.GIF{}
	for i := 0; i < 3; i++ {
		palette := image.NewPaletted(image.Rect(0, 0, 400, 200), palette.Plan9)
		frames.Image = append(frames.Image, palette)
		frames.Delay = append(frames.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, frames); err != nil {
		t.Fatalf("encode animated gif: %v", err)
	}
	got, err := Process(buf.Bytes())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got.Width != 400 || got.Height != 200 {
		t.Errorf("dimensions = %dx%d, want 400x200", got.Width, got.Height)
	}
	if got.Thumb.Width != 250 || got.Thumb.Height != 125 {
		t.Errorf("thumb = %dx%d, want 250x125", got.Thumb.Width, got.Thumb.Height)
	}
}

func TestProcessRejectsBadInput(t *testing.T) {
	if _, err := Process([]byte("#!/bin/sh\necho hello\n")); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("Process(script) error = %v, want ErrUnsupportedFormat", err)
	}
	// Correct magic bytes, garbage payload.
	corrupt := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, []byte("nonsense")...)
	if _, err := Process(corrupt); !errors.Is(err, ErrCorruptImage) {
		t.Fatalf("Process(corrupt) error = %v, want ErrCorruptImage", err)
	}
}

func TestContentTypeForExt(t *testing.T) {
	cases := map[string]string{
		".jpg":  "image/jpeg",
		".png":  "image/png",
		".gif":  "image/gif",
		".webp": "image/webp",
		".txt":  "application/octet-stream",
	}
	for ext, want := range cases {
		if got := ContentTypeForExt(ext); got != want {
			t.Errorf("ContentTypeForExt(%q) = %q, want %q", ext, got, want)
		}
	}
}
