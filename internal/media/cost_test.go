package media

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"math/rand"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

/* Fixtures ----------------------------------------------------------------- */

// rawPNG writes a w×h PNG of the given colour type and bit depth, Adam7
// interlaced or not, with a tRNS chunk if asked: the kinds image/png cannot
// write itself. The pixels are four flat quadrants, the top left one black,
// which is the colour a tRNS chunk written here makes transparent.
func rawPNG(t *testing.T, w, h int, colour, depth byte, interlaced, trns bool) []byte {
	t.Helper()
	channels := map[byte]int{0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[colour]
	bpp := channels * int(depth) / 8
	chunk := func(out []byte, typ string, data []byte) []byte {
		out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
		out = append(out, typ...)
		out = append(out, data...)
		return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	}
	ihdr := binary.BigEndian.AppendUint32(nil, uint32(w))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(h))
	method := byte(0)
	if interlaced {
		method = 1
	}
	out := chunk([]byte("\x89PNG\r\n\x1a\n"), "IHDR", append(ihdr, depth, colour, 0, 0, method))
	if colour == 3 {
		out = chunk(out, "PLTE", []byte{0, 0, 0, 255, 255, 255})
	}
	if trns {
		out = chunk(out, "tRNS", make([]byte, map[byte]int{0: 2, 2: 6, 3: 1}[colour]))
	}
	passes := [][4]int{{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4}, {0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2}}
	if !interlaced {
		passes = [][4]int{{0, 0, 1, 1}}
	}
	var idat bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&idat, zlib.BestSpeed)
	for _, p := range passes {
		if p[0] >= w {
			continue
		}
		for y := p[1]; y < h; y += p[3] {
			row := []byte{0}
			for x := p[0]; x < w; x += p[2] {
				quadrant := 0
				if x >= w/2 {
					quadrant++
				}
				if y >= h/2 {
					quadrant += 2
				}
				v := byte(0x40 * quadrant)
				if colour == 3 {
					v = byte(quadrant & 1)
				}
				for k := 0; k < bpp; k++ {
					row = append(row, v)
				}
			}
			if _, err := zw.Write(row); err != nil {
				t.Fatalf("compress: %v", err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("compress: %v", err)
	}
	out = chunk(out, "IDAT", idat.Bytes())
	return chunk(out, "IEND", nil)
}

// gifOfFrames is a GIF of n flat frames covering a w×h canvas.
func gifOfFrames(t *testing.T, w, h, n int) []byte {
	t.Helper()
	pal := color.Palette{color.Black, color.White}
	g := &gif.GIF{}
	for i := 0; i < n; i++ {
		g.Image = append(g.Image, image.NewPaletted(image.Rect(0, 0, w, h), pal))
		g.Delay = append(g.Delay, 1)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

// gifPutBack is a GIF of five frames on a w×h canvas that is stored with a
// table a frame and frames put back (playGIF): a background of 256 bands of
// colour, then a spark of one colour over its left half, put back, then 256
// bands of other colours over its right half, and the spark and another 256
// after that. Left in place or cleared, the spark would leave the frame
// after it to draw the background's left half as well, 512 colours.
func gifPutBack(t *testing.T, w, h int) []byte {
	t.Helper()
	bands := func(r image.Rectangle, red uint8) *image.Paletted {
		pal := make(color.Palette, 256)
		for i := range pal {
			pal[i] = color.RGBA{red, uint8(i), uint8(255 - i), 0xFF}
		}
		m := image.NewPaletted(r, pal)
		for y := r.Min.Y; y < r.Max.Y; y++ {
			row := m.Pix[m.PixOffset(r.Min.X, y):][:r.Dx()]
			for x := range row {
				row[x] = uint8((y - r.Min.Y) * 256 / r.Dy())
			}
		}
		return m
	}
	left, right := image.Rect(0, 0, w/2, h), image.Rect(w/2, 0, w, h)
	spark := image.NewPaletted(left, color.Palette{color.RGBA{0xF0, 0xF0, 0x10, 0xFF}})
	g := &gif.GIF{
		Image:    []*image.Paletted{bands(image.Rect(0, 0, w, h), 0), spark, bands(right, 1), spark, bands(right, 2)},
		Delay:    []int{10, 10, 10, 10, 10},
		Disposal: []byte{gif.DisposalNone, gif.DisposalPrevious, gif.DisposalNone, gif.DisposalPrevious, gif.DisposalNone},
		Config:   image.Config{Width: w, Height: h},
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

// gifPastCanvas is a GIF whose 64×64 canvas holds its first frame, and whose
// second, large×tall, reaches far past it, as browsers draw one cut to the
// canvas. image/gif decodes it whole (gifReader), so the canvas says nothing
// of what decoding it takes.
func gifPastCanvas(t *testing.T, large, tall int) []byte {
	t.Helper()
	pal := color.Palette{color.Black, color.White}
	g := &gif.GIF{
		Image: []*image.Paletted{
			image.NewPaletted(image.Rect(0, 0, 64, 64), pal),
			image.NewPaletted(image.Rect(32, 32, 32+large, 32+tall), pal),
		},
		Delay:  []int{1, 1},
		Config: image.Config{Width: 32 + large, Height: 32 + tall},
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	out := buf.Bytes()
	binary.LittleEndian.PutUint16(out[6:], 64)
	binary.LittleEndian.PutUint16(out[8:], 64)
	return out
}

// interlaced marks every frame of a GIF of flat frames interlaced, which
// image/gif never writes itself. A flat frame is the same picture whatever
// order its rows come in, so the flag is all that has to change.
func interlaced(t *testing.T, data []byte) []byte {
	t.Helper()
	out := append([]byte(nil), data...)
	table := func(flags byte) int {
		if flags&0x80 == 0 {
			return 0
		}
		return 3 << (1 + flags&0x07)
	}
	subBlocks := func(i int) int {
		for i < len(out) && out[i] != 0 {
			i += 1 + int(out[i])
		}
		return i + 1
	}
	frames := 0
	for i := 13 + table(out[10]); i < len(out); {
		switch out[i] {
		case 0x21: // an extension: its label, then its sub-blocks
			i = subBlocks(i + 2)
		case 0x2C: // a frame: its descriptor, colour table, code size and data
			flags := out[i+9]
			out[i+9] = flags | 0x40
			frames++
			i = subBlocks(i + 10 + table(flags) + 1)
		case 0x3B:
			if frames == 0 {
				t.Fatal("no frame to mark interlaced")
			}
			return out
		default:
			t.Fatalf("unexpected GIF block %#x at %d", out[i], i)
		}
	}
	t.Fatal("no GIF trailer")
	return nil
}

// paddedBitstream is a lossy WebP with n more bytes at the end of its VP8
// chunk. Stripping keeps the chunk whole, and the decoder copies all of it
// before it decodes the picture and then ignores what the picture does not
// need, as it would a file hidden there (sanitizeWebP). For the cost model it
// is a larger lossy bitstream, which is what a larger picture or a higher
// quality would make.
func paddedBitstream(t *testing.T, data []byte, n int) []byte {
	t.Helper()
	chunks := webpChunks(t, data)
	padded := 0
	for i, c := range chunks {
		if c.id == "VP8 " {
			chunks[i].payload = append(c.payload[:len(c.payload):len(c.payload)], make([]byte, n)...)
			padded++
		}
	}
	if padded != 1 {
		t.Fatalf("chunks %v, want a lossy WebP", chunkIDs(chunks))
	}
	return webpFile(chunks...)
}

// manyChunkWebP is a simple lossy WebP with n empty unknown chunks after its
// bitstream. The picture is unchanged and stripping drops the empties, but a
// walker that built a struct for every chunk would hold n of them, far more
// than the file's size the cost model charges the strip phase. The base is
// small, so the slice, not the picture, is what the model has to cover.
func manyChunkWebP(t *testing.T, data []byte, n int) []byte {
	t.Helper()
	chunks := webpChunks(t, data)
	if len(chunks) != 1 || chunks[0].id != "VP8 " {
		t.Fatalf("chunks %v, want a simple lossy WebP", chunkIDs(chunks))
	}
	all := make([]riffChunk, 1, 1+n)
	all[0] = chunks[0]
	for i := 0; i < n; i++ {
		all = append(all, riffChunk{id: "JUNK"})
	}
	return webpFile(all...)
}

// The chunk walk reads one chunk at a time instead of collecting them into a
// list, so a WebP of a great many empty chunks costs the walk a bounded amount
// of memory rather than one riffChunk struct for every chunk in the file. What
// is kept is the picture; the chunks around its bytes are walked and dropped.
// The file here is under two megabytes, so a walker holding a struct per chunk
// would stand out at once.
func TestWebPChunkWalkDoesNotHoldAStructPerChunk(t *testing.T) {
	const chunks = 200000
	data := manyChunkWebP(t, readTestdata(t, "webp-swatches.webp"), chunks)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	out, _, err := stripWebP(data)
	if err != nil {
		t.Fatalf("stripWebP: %v", err)
	}
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(out)

	got := int64(after.TotalAlloc - before.TotalAlloc)
	// A riffChunk is forty bytes, so a slice of one per chunk is chunks*40
	// before the doubling append does as it grows, which lands it at several
	// times this; the walk instead holds the picture and a short id string per
	// chunk as it goes. The bound is the file plus one struct's width a chunk,
	// 9.2 MiB: well over what the walk takes (about 2.3 MiB, or 4.6 MiB under
	// the race detector's own allocation) and well under the slice.
	limit := int64(len(data)) + chunks*40
	if got > limit {
		t.Errorf("stripWebP of a %d-chunk file allocated %d bytes, over %d: it is holding a struct per chunk", chunks, got, limit)
	}
}

// greyQuadrants is quadrants as a grey picture.
func greyQuadrants(w, h int) *image.Gray {
	m := image.NewGray(image.Rect(0, 0, w, h))
	draw.Draw(m, m.Rect, quadrants(w, h), image.Point{}, draw.Src)
	return m
}

// costFixtures are one file of each kind processingCost models, pictures of
// four flat quadrants unless they say otherwise. They are large enough for
// each term the model charges for decoding to come to megabytes more than
// processingSlack in one of them at least, so that a term left out is seen (a
// partial cut smaller than the slack it hides instead): 2400×1800, four times
// a phone screenshot's pixels, for a
// term of a byte and a half a pixel; 3200×2400 for the three kinds that turn
// on a term of a byte a pixel; and 1200×900 for the 16-bit colour PNGs,
// whose least term is eight bytes a pixel, and which take the longest to
// encode.
//
// The cost-*.jpg and cost-*.webp files in testdata are 2400×1800, made with
// Pillow, because Go encodes neither progressive nor CMYK JPEGs nor any WebP:
// the JPEGs at quality 90, the progressive ones with an EXIF orientation of 6
// and cost-rgb.jpg with keep_rgb (an Adobe segment of transform 0, and
// components named R, G and B), and the WebPs at Pillow's defaults. The
// lossless WebP is a grid of nine colours and the transparent one has eight
// levels of alpha in vertical bands, the first clear, which libwebp writes as
// a colour index packed two pixels to the byte. cost-411.jpg is made with
// ImageMagick (-sampling-factor 4x1 -quality 90), because Pillow does not
// write 4:1:1. The dearest lossless picture to decode adds what libwebp did
// not need for those two, a predictor and a cross-colour transform of 4×4
// tiles before the colour index, whose images are held beside the packed
// picture and the unpacked one: "WebP lossless, unpacked" (unpackedWebP) is
// 42 bytes that decode to 25 megapixels and hold 6.5 bytes a pixel, 12 MiB
// of it the transforms' images. It is four times as wide as it is tall
// because the thumbnail scaler's scratch grows with the shorter side: as a
// square, the scratch and the encoder's output beneath it would cost more
// than decoding it does, and hide a term of its decoding from the cost of the
// whole. The GIF past its canvas has a 3200×2400 interlaced frame on a canvas
// of 64×64, so that the copy its rows are put in order in is charged on the
// frame, not the canvas. The GIF of frames put back is stored with frames
// put back, for which what was under them is kept (gifPlayScratch). An
// opaque lossy WebP is charged for its planes and
// two copies of its file (the strip's and the decoder's), so it comes three
// ways, each term megabytes over the others somewhere: 2400×1800 as Pillow
// made it, whose planes are megabytes and whose file is not; 144×48 with 8 MiB
// more at the end of its bitstream (paddedBitstream), whose two copies are
// megabytes and whose planes are not; and 2400×1800 padded, where both are.
// The PNG strip is a row of 524288 grey pixels four deep, whose thumbnail
// scaler holds tables of weights of 64 bytes for each of them (thumbScratch);
// turned on its side, it is stored four pixels wide, so its thumbnail is a
// pixel wide and the scaler's buffer is 32 bytes for every one of its rows as
// well, which only reading the orientation charges. The 16-bit RGBA strip, a
// row of 1048576 pixels, is decoded through two rows of the file of 8 MiB each
// beside its picture (pngRowBytes). Two more JPEGs of YCbCr samples, both
// turned on their side, are converted to RGB, a copy of 4 bytes a pixel that
// the turn copies again whole, by segments image/jpeg reads where a walk that
// stopped at the first scan would not see them: the progressive 4:2:0 file,
// without its JFIF APP0, with an Adobe segment of transform 0 after its last
// scan, and a baseline 4:2:0 one whose JFIF APP0 is followed by another APP0,
// which clears what the first said, so that the Adobe segment after it counts.
// Those marked "o6" carry an EXIF orientation of 6, so they are turned on
// their side too, and "o3" one of 3, turned around.
func costFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	turned := func(jpg []byte, o uint16) []byte {
		return withSegments(jpg, exifSegment(orientationTIFF(binary.LittleEndian, o)))
	}
	sideways := func(jpg []byte) []byte { return turned(jpg, 6) }
	const w, h = 2400, 1800
	const large, tall = 3200, 2400
	const small, short = 1200, 900
	notJFIF := jpegSegment(0xE0, []byte("AVI1\x00\x00\x00\x00"))
	return map[string][]byte{
		"baseline 4:2:0 o6":          sideways(encodeJPEGImage(t, quadrants(w, h))),
		"baseline 4:2:0 odd o3":      turned(encodeJPEGImage(t, quadrants(w+1, h+1)), 3),
		"baseline 4:1:1 o6":          sideways(readTestdata(t, "cost-411.jpg")),
		"baseline grey o6":           sideways(encodeJPEGImage(t, greyQuadrants(large, tall))),
		"progressive 4:2:0 o6":       readTestdata(t, "cost-prog420.jpg"),
		"progressive 4:4:4 o6":       readTestdata(t, "cost-prog444.jpg"),
		"progressive grey o6":        readTestdata(t, "cost-proggray.jpg"),
		"progressive CMYK o6":        readTestdata(t, "cost-progcmyk.jpg"),
		"baseline CMYK":              readTestdata(t, "cost-cmyk.jpg"),
		"RGB":                        readTestdata(t, "cost-rgb.jpg"),
		"RGB, Adobe after the scans": beforeEOI(withoutJFIF(t, readTestdata(t, "cost-prog420.jpg")), adobeSegment(0)),
		"RGB, JFIF undone by APP0":   sideways(withSegments(encodeJPEGImage(t, quadrants(w, h)), jfifSegment(), notJFIF, adobeSegment(0))),
		"PNG palette":                rawPNG(t, large, tall, 3, 8, false, false),
		"PNG RGB":                    rawPNG(t, w, h, 2, 8, false, false),
		"PNG grey with transparency": rawPNG(t, w, h, 0, 8, false, true),
		"PNG RGBA interlaced":        rawPNG(t, w, h, 6, 8, true, false),
		"PNG grey 16-bit interlaced": rawPNG(t, w, h, 0, 16, true, false),
		"PNG RGB 16-bit interlaced":  rawPNG(t, small, short, 2, 16, true, false),
		"PNG RGBA 16-bit o6": withPNGChunks(rawPNG(t, small, short, 6, 16, false, false),
			pngChunk("eXIf", orientationTIFF(binary.BigEndian, 6))),
		"PNG strip o6": withPNGChunks(rawPNG(t, 1<<19, 4, 0, 8, false, false),
			pngChunk("eXIf", orientationTIFF(binary.BigEndian, 6))),
		"PNG RGBA 16-bit strip":        rawPNG(t, 1<<20, 1, 6, 16, false, false),
		"GIF of four frames":           gifOfFrames(t, w, h, 4),
		"GIF of two interlaced frames": interlaced(t, gifOfFrames(t, large, tall, 2)),
		"GIF past its canvas":          interlaced(t, gifPastCanvas(t, large, tall)),
		"GIF of frames put back":       gifPutBack(t, w, h),
		"WebP lossy":                   readTestdata(t, "cost-lossy.webp"),
		"WebP lossy, small, padded":    paddedBitstream(t, readTestdata(t, "webp-swatches.webp"), 8<<20),
		"WebP lossy, padded":           paddedBitstream(t, readTestdata(t, "cost-lossy.webp"), 8<<20),
		"WebP lossy with alpha":        readTestdata(t, "cost-alpha.webp"),
		"WebP lossless":                readTestdata(t, "cost-lossless.webp"),
		"WebP lossless, unpacked":      unpackedWebP(10000, 2500),
		"WebP of many chunks":          manyChunkWebP(t, readTestdata(t, "webp-swatches.webp"), 200000),
	}
}

// withoutJFIF is a JPEG without the JFIF APP0 segment it starts with, as
// Pillow and libjpeg write one.
func withoutJFIF(t *testing.T, jpg []byte) []byte {
	t.Helper()
	jfif := jfifSegment()
	if len(jpg) < 2+len(jfif) || !bytes.Equal(jpg[2:6], jfif[:4]) || string(jpg[6:11]) != "JFIF\x00" {
		t.Fatal("withoutJFIF: the JPEG does not start with a JFIF APP0 of the usual length")
	}
	return append([]byte{0xFF, 0xD8}, jpg[2+len(jfif):]...)
}

/* Measurement -------------------------------------------------------------- */

// liveHeap is the heap the collector last marked live.
func liveHeap() int64 {
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	return int64(s[0].Value.Uint64())
}

// peakLive runs work with the collector set to start again whenever the heap
// grows by a hundredth, and returns the most the heap held live, as the
// collector marked it, above what it held before. That is the quantity
// processingCost estimates: garbage a phase leaves behind is not in it,
// because a memory limit makes the collector take it back.
//
// A collection marks live everything reachable when it begins and everything
// allocated while it runs, so what it marks is no less than what was held at
// any moment while it ran; and with one started at every hundredth of growth,
// and at once by any allocation large enough to matter, what comes and goes
// between two of them is small. peakLive reads what each collection marked
// after it ends, and runs work again if two ended between its reads, so none
// is missed. What it measures can be more than was held, though: what work
// allocates and drops while one collection runs is marked live too, and how
// much of that a run measures depends on how the collections fall, which
// depends on what else the machine is doing (leastPeak). A GIF of thousands
// of tiny frames is the extreme case, mostly such churn: image/gif allocates
// tens of kilobytes of LZW tables for each frame and keeps a few. What it
// keeps is measured exactly instead, by
// TestGIFFrameOverheadCoversWhatDecodingKeeps.
//
// The sampler goroutine has to run alongside work to read every collection as
// it ends; on a single core it cannot, because work is CPU-bound and does not
// yield to it often enough, so it misses collections and the measurement never
// settles. peakLive therefore runs on at least two cores, raising GOMAXPROCS
// for its own length when it has to (a plain `go test` already has more, so
// only `-cpu=1`, GOMAXPROCS=1, or one allotted core needs this).
func peakLive(t testing.TB, name string, work func() error) int64 {
	t.Helper()
	if runtime.GOMAXPROCS(0) < 2 {
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	}
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	for attempt := 0; attempt < 3; attempt++ {
		runtime.GC()
		s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}, {Name: "/gc/cycles/total:gc-cycles"}}
		metrics.Read(s)
		base, cycles := int64(s[0].Value.Uint64()), s[1].Value.Uint64()

		var (
			peak   int64
			missed bool
			wg     sync.WaitGroup
			stop   = make(chan struct{})
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				metrics.Read(s)
				peak = max(peak, int64(s[0].Value.Uint64()))
				n := s[1].Value.Uint64()
				missed = missed || n > cycles+1
				cycles = n
				select {
				case <-stop:
					return
				default:
					time.Sleep(50 * time.Microsecond)
				}
			}
		}()
		err := work()
		runtime.GC() // marks whatever work still held
		close(stop)
		wg.Wait()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !missed {
			return peak - base
		}
	}
	t.Fatalf("%s: every run ended two collections between reads of what they marked", name)
	return 0
}

// leastPeak is the least peakLive measures of work in up to three runs,
// stopping at the first that is within want. A run can measure more than
// work held but not less (peakLive), so the least is the closest to what it
// held, and work that held more than want measures more in every run. One run
// alone can fail on a busy machine: a collection that runs on across a phase
// boundary marks the garbage of the phase before it live alongside the next
// phase's allocations, and it does not fall that way three times running.
func leastPeak(t testing.TB, name string, want int64, work func() error) int64 {
	t.Helper()
	least := int64(math.MaxInt64)
	for run := 0; run < 3 && least > want; run++ {
		least = min(least, peakLive(t, name, work))
	}
	return least
}

/* Tests -------------------------------------------------------------------- */

// What an upload holds while it is processed stays inside what its header
// said it would, for every kind of file the estimate models: progressive
// coefficients, CMYK and RGB conversions, turning upright exactly and at
// 4:4:4, interlaced and 16-bit PNGs, transparency, every frame of a GIF and
// an interlaced one, and WebP's three kinds. So does what it holds before it
// is encoded again, against the decoding phases alone (decodingPhases): in
// the cost of the whole, the thumbnail's scratch and a bound on the encoder's
// output far above what flat quadrants need are often the larger part, and
// would hide a decoding term the model left out. The encoder is stopped
// there by a stored limit of nothing, so it is refused the first byte it
// writes. Every fixture decodes to the end, so the whole is held to the cost
// of that (completedCost), not to what Inspect charges: for a lossless WebP
// and a compressed alpha plane that is the full Huffman-group charge of a
// bitstream that stops after naming its groups (vp8lStopCost), which would
// hide any term of theirs left out.
//
// The whole is measured first. The limit-0 run makes exactly the same
// allocations as the whole up to the byte the encoder is refused, and after
// it does no more than part of what the whole goes on to do — jpegenc stops
// at the end of the row of units it is coding, image/png and image/gif once
// their own buffers next pass a write on, and the stored file never grows —
// so its peak can never exceed the whole's; when the whole is already within
// the decoding estimate, the limit-0 run must be too, so it is not run.
// That halves the slowest fixtures, whose peak is all in the decode (an
// unpacked lossless WebP). It hides nothing: a decoding term left out shrinks
// `decoding`, and where that lets the real decode peak exceed it, the whole,
// which includes the decode, exceeds it too, so the whole is not within
// `decoding`, the limit-0 run is not skipped, and the decoded check still
// fails (TestDroppedDecodingTermIsStillCaught drops one and watches
// checkProcessingCost catch it).
func TestProcessingCostCoversWhatIsHeld(t *testing.T) {
	for name, data := range costFixtures(t) {
		checkProcessingCost(t, name, data, nil)
	}
}

// checkProcessingCost holds one fixture to both bounds, measuring the whole
// Process run first and the limit-0 decoding run only when the whole is not
// already within the decoding estimate (see TestProcessingCostCoversWhatIsHeld).
// The decoding estimate is of decodingPhases' phases, passed through model
// when it is not nil, which is how TestDroppedDecodingTermIsStillCaught
// leaves a term out. It returns what the whole held, and whether it measured
// the decoding run.
func checkProcessingCost(t testing.TB, name string, data []byte, model func([]phase) []phase) (whole int64, ranDecoding bool) {
	t.Helper()
	in, err := inspect(data)
	if err != nil {
		t.Fatalf("%s: inspect: %v", name, err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("%s: DecodeConfig: %v", name, err)
	}

	phases := decodingPhases(in.format, data, cfg, in.frames)
	if model != nil {
		phases = model(phases)
	}
	decoding := peakOf(phases...) + processingSlack
	completed := completedCost(in.format, data, cfg, in.frames)
	whole = leastPeak(t, name, completed, func() error {
		_, err := Process(context.Background(), data)
		return err
	})
	mib := func(n int64) float64 { return float64(n) / (1 << 20) }
	if whole > completed {
		t.Errorf("%s: held %d bytes live, over the estimate of decoding it to the end, %d", name, whole, completed)
	}

	if whole <= decoding {
		t.Logf("%-28s %4d×%-4d all of it %5.1f MiB of %5.1f (within decoding %5.1f, decoding run skipped)",
			name, cfg.Width, cfg.Height, mib(whole), mib(completed), mib(decoding))
		return whole, false
	}

	decoded := leastPeak(t, name, decoding, func() error {
		unstored := in
		unstored.limit = 0
		if _, err := sanitize(context.Background(), unstored, data); !errors.Is(err, ErrStoredTooLarge) {
			return fmt.Errorf("sanitize with nowhere to store = %v, want ErrStoredTooLarge", err)
		}
		return nil
	})
	t.Logf("%-28s %4d×%-4d decoding %5.1f MiB of %5.1f, all of it %5.1f MiB of %5.1f",
		name, cfg.Width, cfg.Height, mib(decoded), mib(decoding), mib(whole), mib(completed))
	if decoded > decoding {
		t.Errorf("%s: held %d bytes live before encoding, over the estimate of its decoding phases, %d", name, decoded, decoding)
	}
	return whole, true
}

// errorsKept is a testing.TB that keeps the errors reported to it instead
// of failing the test, and passes everything else on: a Fatalf, which only
// a broken measurement reports, still fails it.
type errorsKept struct {
	testing.TB
	errs []string
}

func (e *errorsKept) Errorf(format string, args ...any) {
	e.errs = append(e.errs, fmt.Sprintf(format, args...))
}

// TestDroppedDecodingTermIsStillCaught proves that the skip in
// checkProcessingCost — not measuring the limit-0 decoding run when the whole
// is already within the decoding estimate — cannot hide a decoding term left
// out of the model. The fixture, a lossless WebP, is one the skip is taken
// for: its full decoding estimate (about 32.5 MiB) covers the whole process's
// peak (about 30.7 MiB), so with the model unchanged the decoding run is
// skipped.
//
// The term dropped is a realistic one, not a gross slice of the estimate: the
// colour-indexed decode's packed picture, 4·⌈w/2⌉·h, about 8 MiB here, which a
// model that forgot it would omit. That lands the decoding estimate at about
// 24.3 MiB, just under what the decode actually holds (the limit-0 run
// measures about 24.7 MiB), which is the point — the skip must not be taken
// once the estimate drops below the measured whole, however little.
// checkProcessingCost has to find the whole over the shrunk estimate, run the
// decoding run it would otherwise skip, and report the decode over the
// estimate. That holds the skip near the committed condition (whole <=
// decoding), within the margin the drop leaves: the shrunk estimate is about
// 6.4 MiB under the whole (24.3 against 30.7 MiB), so a condition widened by
// more than that — whole <= decoding + 7 MiB, say, or whole <= 2·decoding —
// would skip the run here and hide the dropped term, and this test fails; one
// widened by less, + 2 MiB or + 6 MiB, still runs it, and this test does not
// tell it from the committed one. It does not pin the exact byte boundary; it
// pins that a realistic forgotten term is caught, which dropping the whole
// largest phase (a 7× gap) would not establish.
func TestDroppedDecodingTermIsStillCaught(t *testing.T) {
	const name = "WebP lossless"
	data := readTestdata(t, "cost-lossless.webp")
	in, err := inspect(data)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	full := peakOf(decodingPhases(in.format, data, cfg, in.frames)...) + processingSlack

	// Drop only the vp8l colour-index packed picture (vp8lPixels' 4·⌈w/2⌉·h
	// term) from the decode phase, the one with the most allocated: a term a
	// model could plausibly forget, not the whole phase.
	packed := 4 * ((int64(cfg.Width) + 1) / 2) * int64(cfg.Height)
	dropPacked := func(phases []phase) []phase {
		out := append([]phase{}, phases...)
		largest := 0
		for i, p := range out {
			if p.alloc > out[largest].alloc {
				largest = i
			}
		}
		out[largest].first -= packed
		out[largest].alloc -= packed
		return out
	}
	shrunk := peakOf(dropPacked(decodingPhases(in.format, data, cfg, in.frames))...) + processingSlack

	kept := &errorsKept{TB: t}
	whole, ran := checkProcessingCost(kept, name+", packed term dropped", data, dropPacked)
	// The dropped term must leave the estimate under the whole (so the run is
	// not skipped) while the full estimate still covers it (so without the
	// drop the run would be skipped): that the drop crosses the boundary is
	// what the test turns on.
	if !(shrunk < whole && whole <= full) {
		t.Fatalf("whole %d not between the shrunk estimate %d and the full one %d: the drop does not cross the skip boundary, so the test proves nothing", whole, shrunk, full)
	}
	if !ran {
		t.Fatal("checkProcessingCost skipped the decoding run with the packed term left out: a realistic forgotten term is hidden")
	}
	caught := false
	for _, e := range kept.errs {
		if strings.Contains(e, "over the estimate of its decoding phases") {
			caught = true
		} else {
			t.Errorf("unexpected error: %s", e)
		}
	}
	if !caught {
		t.Errorf("checkProcessingCost reported %q with the packed term left out, want the decode over the estimate of its decoding phases", kept.errs)
	}
}

// What playGIF allocates is no more than gifPlayScratch: for a GIF of few
// colours, what the frames left and the two bit sets; and for an animation
// of many, which may be stored with frames put back and here is
// (gifPutBack), what was under the stored frame as well. The frames are
// decoded before it is measured and held throughout. In the cost of the
// whole the scratch sits on top of a bound for the stored file that is far
// above what these need, which would hide a scratch smaller than the
// player's.
func TestGIFPlayScratchCoversThePlayer(t *testing.T) {
	const w, h = 2400, 1800
	for _, c := range []struct {
		name string
		data []byte
		many bool
	}{
		{"few colours", gifOfFrames(t, w, h, 4), false},
		{"frames put back", gifPutBack(t, w, h), true},
	} {
		frames, err := gifFramePixels(c.data, maxGIFFramePixels)
		if err != nil || frames.many != c.many {
			t.Fatalf("%s: read as %+v, %v; want many colours %v", c.name, frames, err, c.many)
		}
		g, err := gif.DecodeAll(bytes.NewReader(c.data))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var heads []gifFrameHead
		walkGIF(c.data, math.MaxInt64, func(head gifFrameHead) { heads = append(heads, head) })
		want := gifPlayScratch(w, h, frames) + processingSlack
		held := leastPeak(t, c.name, want, func() error {
			// playGIF lets go of the frames in the slice it is given, and
			// the frames stay held in g.
			return playGIF(context.Background(), slices.Clone(g.Image), heads, image.Rect(0, 0, w, h), 0, io.Discard)
		})
		t.Logf("%s: playing held %.1f MiB of %.1f", c.name, float64(held)/(1<<20), float64(want)/(1<<20))
		if held > want {
			t.Errorf("%s: playing the GIF held %d bytes live, over gifPlayScratch and processingSlack, %d", c.name, held, want)
		}
	}
}

// An animation is charged for what putting frames back keeps
// (gifPlayScratch) when its frames' tables, as Chromium reads them, give more
// than 255 colours between them (gifFrames.many): a frame's transparent
// entry shows no colour, and an index past the end of a table of fewer than
// 256 entries shows black. Only such tables can draw more colours than one
// table holds beside a transparent entry, which is when playGIF gives each
// frame a table of its own and may put frames back; so every GIF of random
// frames or of sprites that is stored so was charged.
func TestGIFOfManyColoursIsReadFromItsTables(t *testing.T) {
	distinct := func(first, n int) [][3]byte {
		c := make([][3]byte, n)
		for i := range c {
			c[i] = [3]byte{byte(first + i), byte((first + i) >> 8), 0x77}
		}
		return c
	}
	frame := func(local [][3]byte, ti int) rawFrame {
		return rawFrame{controls: []rawControl{{keepFrame, 10, ti}}, at: image.Rect(0, 0, 2, 1), local: local, pix: []uint8{0, 1}}
	}
	for _, c := range []struct {
		name string
		g    rawGIF
		many bool
	}{
		{"a global table of 256 colours", rawGIF{w: 2, h: 1, global: distinct(0, 256),
			frames: []rawFrame{frame(nil, -1), frame(nil, -1)}}, true},
		{"a global table of 256 colours, one of them transparent in every frame", rawGIF{w: 2, h: 1, global: distinct(0, 256),
			frames: []rawFrame{frame(nil, 7), frame(nil, 7)}}, false},
		{"a global table of 256 colours, a different one transparent in each frame", rawGIF{w: 2, h: 1, global: distinct(0, 256),
			frames: []rawFrame{frame(nil, 7), frame(nil, 9)}}, true},
		{"tables of 128 colours and the black past them", rawGIF{w: 2, h: 1, global: distinct(0, 128),
			frames: []rawFrame{frame(nil, -1), frame(distinct(0, 127), -1)}}, false},
		{"two tables of 128 colours and the black past them", rawGIF{w: 2, h: 1,
			frames: []rawFrame{frame(distinct(0, 128), -1), frame(distinct(128, 128), -1)}}, true},
	} {
		frames, err := gifFramePixels(c.g.bytes(), maxGIFFramePixels)
		if err != nil || frames.many != c.many {
			t.Errorf("%s: read as many colours %v (%v), want %v", c.name, frames.many, err, c.many)
		}
	}
	// Reading the tables allocates nothing, however many there are: here
	// 2,000 of 128 colours each, the same ones, so that it reads them all.
	g := rawGIF{w: 2, h: 1}
	for len(g.frames) < 2000 {
		g.frames = append(g.frames, frame(distinct(0, 128), 5))
	}
	tables := g.bytes()
	if frames, err := gifFramePixels(tables, maxGIFFramePixels); err != nil || frames.many {
		t.Fatalf("2,000 tables of the same 128 colours read as many colours %v (%v)", frames.many, err)
	}
	if allocs := testing.AllocsPerRun(10, func() { gifFramePixels(tables, maxGIFFramePixels) }); allocs != 0 {
		t.Errorf("gifFramePixels allocated %v times reading 2,000 tables, want none", allocs)
	}

	rng := rand.New(rand.NewSource(3))
	own := 0
	for i := 0; i < 600; i++ {
		g := randomSpritesGIF(rng)
		if i%2 == 0 {
			g = randomRawGIF(rng, 600)
		}
		data := g.bytes()
		got, err := Process(context.Background(), data)
		if err != nil {
			continue
		}
		if _, local := gifTables(t, got.Data); slices.Contains(local, true) && len(local) > 1 {
			own++
			if frames, _ := gifFramePixels(data, maxGIFFramePixels); !frames.many {
				t.Fatalf("GIF %d is stored with a table a frame, but was not read as many colours", i)
			}
		}
	}
	if own < 300 {
		t.Errorf("%d GIFs stored with a table a frame; the test shows little", own)
	}
}

// The thumbnail scaler's scratch is no more than thumbScratch, for a picture
// wider than it is tall and for one taller than it is wide: the scaler works
// on a picture as wide as the thumbnail and as tall as the source, so it is
// the shorter side that counts. Its tables of weights, which grow with the
// longer side, are most of it for a long, thin strip, and a strip taller
// than ThumbMaxDim times its width has a thumbnail a pixel wide, so its
// buffer grows with the longer side too. In the cost of the whole the
// scratch sits on top of a bound for the encoder's output that is far above
// what a flat picture needs, which would hide a scratch smaller than the
// scaler's.
func TestThumbScratchCoversTheScaler(t *testing.T) {
	for _, size := range []image.Point{{2400, 1800}, {1800, 2400}, {1 << 19, 4}, {4, 1 << 19}} {
		pic := image.NewYCbCr(image.Rect(0, 0, size.X, size.Y), image.YCbCrSubsampleRatio420)
		name := fmt.Sprintf("%dx%d", size.X, size.Y)
		want := thumbScratch(size.X, size.Y) + processingSlack
		held := leastPeak(t, name, want, func() error {
			_, err := makeThumbnail(pic, FormatJPEG)
			return err
		})
		t.Logf("%s: thumbnail held %.1f MiB of %.1f", name, float64(held)/(1<<20), float64(want)/(1<<20))
		if held > want {
			t.Errorf("%s: making the thumbnail held %d bytes live, over thumbScratch and processingSlack, %d", name, held, want)
		}
	}
}

// image/png's encoder writes a picture through six rows of it, the row under
// each of its five filters and the row before it, each as wide as the
// picture; in a strip a million pixels wide they are more than
// processingSlack, so they are charged (pngEncoderRows), and what the encoder
// holds is no more than that and the slack. That is so for every kind of PNG
// as sanitizePNG hands it to the encoder, and for the NRGBA picture a lossless
// WebP or a lossy one with an alpha plane is stored as. They are measured
// here, in the encoder alone, because measured over the whole of Process
// they would not show left out: the scaler's scratch for a strip's
// thumbnail, made once the encoder is done with them, is larger.
func TestPNGEncoderRowsAreCharged(t *testing.T) {
	const w = 1 << 20
	type strip struct {
		name   string
		format Format
		data   []byte // the upload, which pngEncoderRows reads the kind from
		pic    image.Image
	}
	var strips []strip
	for _, k := range []struct {
		name          string
		colour, depth byte
		trns          bool
	}{
		{"palette PNG", 3, 8, false},
		{"8-bit grey PNG", 0, 8, false},
		{"16-bit grey PNG", 0, 16, false},
		{"8-bit grey+tRNS PNG", 0, 8, true},
		{"16-bit grey+tRNS PNG", 0, 16, true},
		{"8-bit grey+alpha PNG", 4, 8, false},
		{"8-bit RGB PNG", 2, 8, false},
		{"16-bit RGB PNG", 2, 16, false},
		{"8-bit RGBA PNG", 6, 8, false},
		{"16-bit RGBA PNG", 6, 16, false},
	} {
		data := rawPNG(t, w, 1, k.colour, k.depth, false, k.trns)
		pic, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", k.name, err)
		}
		reduceHiddenColour(pic)
		strips = append(strips, strip{k.name, FormatPNG, data, pic})
	}
	nrgba := image.NewNRGBA(image.Rect(0, 0, w, 1))
	for i := 3; i < len(nrgba.Pix); i += 8 {
		nrgba.Pix[i] = 0xFF // every other pixel opaque, so it is stored with alpha
	}
	strips = append(strips,
		strip{"lossless WebP", FormatWebP, webpHeader("lossless", w, 1), nrgba},
		strip{"lossy WebP with transparency", FormatWebP, webpHeader("alpha", w, 1), nrgba})

	for _, s := range strips {
		rows := pngEncoderRows(s.format, s.data, w)
		want := rows + processingSlack
		out := bytes.NewBuffer(make([]byte, 0, 4<<20))
		held := leastPeak(t, s.name, want, func() error {
			out.Reset()
			return storedPNG.Encode(out, s.pic)
		})
		t.Logf("%s: encoding held %.1f MiB of %.1f", s.name, float64(held)/(1<<20), float64(want)/(1<<20))
		if held > want {
			t.Errorf("%s: encoding a %dx1 strip held %d bytes live, over pngEncoderRows and processingSlack, %d", s.name, w, held, want)
		}
		// The palette here has two colours, which are written at a bit a
		// pixel, so its rows are within the slack; every other kind's are not.
		if _, paletted := s.pic.(*image.Paletted); !paletted && held <= processingSlack {
			t.Errorf("%s: encoding a %dx1 strip held %d bytes live, within processingSlack: the strip is too narrow to show the rows", s.name, w, held)
		}
	}
}

// What image/gif keeps of each frame besides its pixels fits in
// gifFrameOverhead, for the frames that keep the most for the fewest bytes
// of file (each with its own copy of a 256-colour palette) and for frames
// just past the size the allocator starts rounding up to whole pages.
func TestGIFFrameOverheadCoversWhatDecodingKeeps(t *testing.T) {
	pal := append(color.Palette{color.RGBA{}}, palette.Plan9[1:]...) // colour 0 is transparent
	var large bytes.Buffer
	g := &gif.GIF{Config: image.Config{ColorModel: pal, Width: 32<<10 + 1, Height: 1}}
	for i := 0; i < 100; i++ {
		g.Image = append(g.Image, image.NewPaletted(image.Rect(0, 0, 32<<10+1, 1), pal))
		g.Delay = append(g.Delay, 0)
	}
	if err := gif.EncodeAll(&large, g); err != nil {
		t.Fatalf("encode: %v", err)
	}

	for name, data := range map[string][]byte{
		"one-pixel frames":       tinyFrames(2000),
		"frames of 32 KiB and 1": large.Bytes(),
	} {
		frames, err := gifFramePixels(data, math.MaxInt64)
		if err != nil {
			t.Fatalf("%s: gifFramePixels: %v", name, err)
		}
		runtime.GC()
		before := liveHeap()
		g, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		runtime.GC()
		kept := liveHeap() - before
		runtime.KeepAlive(g)
		if &g.Image[0].Palette[0] == &g.Image[1].Palette[0] {
			t.Fatalf("%s: the frames share a palette, so the test would prove little", name)
		}
		per := (kept - frames.pixels) / frames.count
		t.Logf("%-24s %d frames, %d bytes kept besides the pixels of each", name, frames.count, per)
		if per > gifFrameOverhead {
			t.Errorf("%s: %d bytes kept for each frame besides its pixels, over gifFrameOverhead (%d)", name, per, gifFrameOverhead)
		}
	}
}

// jpegHeader is the start of a JPEG as far as its frame header: JFIF, then a
// SOF of the given kind with three components of the given samplings, or one.
func jpegHeader(marker byte, w, h int, samplings ...byte) []byte {
	b := []byte{0xFF, 0xD8}
	b = append(b, jpegSegment(0xE0, []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00"))...)
	sof := []byte{8}
	sof = binary.BigEndian.AppendUint16(sof, uint16(h))
	sof = binary.BigEndian.AppendUint16(sof, uint16(w))
	sof = append(sof, byte(len(samplings)))
	for i, s := range samplings {
		sof = append(sof, byte(i+1), s, 0)
	}
	return append(b, jpegSegment(marker, sof)...)
}

// pngHeader is a PNG signature and an IHDR, which is all Inspect reads.
func pngHeader(w, h int, colour, depth byte, interlaced bool) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, uint32(w))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(h))
	method := byte(0)
	if interlaced {
		method = 1
	}
	ihdr = append(ihdr, depth, colour, 0, 0, method)
	out := []byte("\x89PNG\r\n\x1a\n")
	out = binary.BigEndian.AppendUint32(out, uint32(len(ihdr)))
	out = append(out, "IHDR"...)
	out = append(out, ihdr...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte("IHDR"), ihdr...)))
}

// headerUpload is a header from jpegHeader, pngHeader or webpHeader turned
// by EXIF orientation o (none for 1) and made up to an upload of size bytes,
// which is what the stored file's limit, and so the cost, grows with. Nothing
// it is made up with is a picture's data.
func headerUpload(head []byte, o uint16, size int) []byte {
	png := head[0] == 0x89
	switch {
	case o == 1:
	case png:
		head = withPNGChunks(head, pngChunk("eXIf", orientationTIFF(binary.BigEndian, o)))
	default:
		head = withSegments(head, exifSegment(orientationTIFF(binary.LittleEndian, o)))
	}
	out := append([]byte(nil), head...)
	if png {
		if head[25] == 3 { // a palette, which image/png reads for the header
			out = append(out, pngChunk("PLTE", []byte{0, 0, 0, 255, 255, 255})...)
		}
		// A chunk image/png skips, then the pixels: no tRNS for the cost to
		// assume.
		out = append(out, pngChunk("zzzz", make([]byte, max(size-len(out)-24, 0)))...)
		return append(out, pngChunk("IDAT", nil)...)
	}
	if head[0] == 0xFF {
		// Comments make a JPEG up to size, then the start of a scan, where
		// image/jpeg stops reading a header.
		out = append(out, jpegComments(size-len(out)-4)...)
		out = append(out, 0xFF, 0xDA, 0x00, 0x02)
	}
	return append(out, make([]byte, max(size-len(out), 0))...)
}

// jpegComments is n bytes of JPEG comment segments, or none for an n under
// the four bytes of the shortest. The walks that read a JPEG to its end
// (readJPEGFrame, jpegScanBlockVisits) step over a comment whole, so a file
// made up to a size with them is read at the cost of its header and a hop a
// comment, which is what lets TestEncodingsAreJudgedAtEveryShape judge
// millions of shapes of an 8 MiB upload.
func jpegComments(n int) []byte {
	var out []byte
	for pad := n; pad >= 4; {
		// Each comment is 0xFFFE and a length counting itself, at most
		// 0xFFFF, and leaves either nothing or room for another.
		k := min(pad, 2+0xFFFF)
		if rest := pad - k; rest > 0 && rest < 4 {
			k -= 4 - rest
		}
		out = append(out, jpegSegment(0xFE, make([]byte, k-4))...)
		pad -= k
	}
	return out
}

// gifHeaderUpload is a GIF of the given number of frames, each as wide as
// its w×h canvas and frameH tall, made up to an upload of about size bytes
// with a comment. The frames are drawn from a two-colour global table, or,
// with many, each from a table of its own of 256 greys, more than 255
// colours (gifFrames.many). Each frame's pixel data is one sub-block of
// nonsense: Inspect's walk reads where the frames are and how large, and
// their tables, never their pixels.
func gifHeaderUpload(w, h, frameH, frames, size int, many bool) []byte {
	b := []byte("GIF89a")
	b = binary.LittleEndian.AppendUint16(b, uint16(w))
	b = binary.LittleEndian.AppendUint16(b, uint16(h))
	if many {
		b = append(b, 0, 0, 0)
	} else {
		b = append(b, 0x80, 0, 0, 0, 0, 0, 255, 255, 255) // a two-colour global table
	}
	for i := 0; i < frames; i++ {
		b = append(b, 0x21, 0xF9, 4, 0, 10, 0, 0, 0) // a graphic control extension
		b = append(b, 0x2C, 0, 0, 0, 0)
		b = binary.LittleEndian.AppendUint16(b, uint16(w))
		b = binary.LittleEndian.AppendUint16(b, uint16(frameH))
		if many {
			b = append(b, 0x87) // a table of its own of 256 entries
			for e := 0; e < 256; e++ {
				b = append(b, byte(e), byte(e), byte(e))
			}
			b = append(b, 8, 1, 0x44, 0)
			continue
		}
		b = append(b, 0, 2, 1, 0x44, 0)
	}
	if pad := size - len(b) - 4; pad > 0 {
		b = append(b, 0x21, 0xFE) // a comment, in sub-blocks of up to 255 bytes
		for ; pad > 1; pad -= 256 {
			k := min(255, pad-1)
			b = append(b, byte(k))
			b = append(b, make([]byte, k)...)
		}
		b = append(b, 0)
	}
	return append(b, 0x3B)
}

// webpHeader is a VP8X WebP of the given size with a stub bitstream: lossy,
// lossy with an alpha plane (uncompressed, or VP8L-compressed with "calpha"),
// or lossless.
func webpHeader(kind string, w, h int) []byte {
	switch kind {
	case "lossy":
		return webpFile(vp8xChunk(0, w, h), riffChunk{id: "VP8 ", payload: make([]byte, 10)})
	case "alpha":
		return webpFile(vp8xChunk(vp8xFlagAlpha, w, h), riffChunk{id: "ALPH", payload: make([]byte, 10)},
			riffChunk{id: "VP8 ", payload: make([]byte, 10)})
	case "calpha": // an alpha plane compressed with VP8L (method 1 in the low bits)
		return webpFile(vp8xChunk(vp8xFlagAlpha, w, h), riffChunk{id: "ALPH", payload: []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
			riffChunk{id: "VP8 ", payload: make([]byte, 10)})
	}
	return webpFile(vp8xChunk(0, w, h), riffChunk{id: "VP8L", payload: make([]byte, 10)})
}

// headJPEG, headRGBJPEG, headPNG, headPNGWithTRNS and headWebP make the
// header of a file of one kind at any size, for headerUpload.
func headJPEG(marker byte, samplings ...byte) func(w, h int) []byte {
	return func(w, h int) []byte { return jpegHeader(marker, w, h, samplings...) }
}

// headRGBJPEG has an Adobe segment with transform 0 instead of JFIF.
func headRGBJPEG(marker byte) func(w, h int) []byte {
	return func(w, h int) []byte {
		j := jpegHeader(marker, w, h, 0x11, 0x11, 0x11)
		return withSegments(append([]byte{0xFF, 0xD8}, j[2+18:]...), jpegSegment(0xEE, []byte("Adobe\x00\x64\x00\x00\x00\x00\x00")))
	}
}

func headPNG(colour, depth byte, interlaced bool) func(w, h int) []byte {
	return func(w, h int) []byte { return pngHeader(w, h, colour, depth, interlaced) }
}

// headPNGWithTRNS is a grey (0) or truecolour (2) PNG with a tRNS chunk
// before its pixels, which decodes to NRGBA(64), four or eight bytes a pixel,
// as pngBytesPerPixel reads from the chunk. headerUpload does not add one.
func headPNGWithTRNS(colour, depth byte, interlaced bool) func(w, h int) []byte {
	return func(w, h int) []byte {
		return append(pngHeader(w, h, colour, depth, interlaced),
			pngChunk("tRNS", make([]byte, map[byte]int{0: 2, 2: 6}[colour]))...)
	}
}

func headWebP(kind string) func(w, h int) []byte {
	return func(w, h int) []byte { return webpHeader(kind, w, h) }
}

// At the largest resolution allowed, how a file is encoded decides whether it
// can be processed within the budget, and Inspect says so from the header,
// before anything is decoded or a cooldown is charged. These are the lists
// the README gives. What is accepted is accepted as a square, whose
// thumbnail scaler needs the most scratch for its pixels, from an upload of
// the default 8 MiB limit, whose stored file may be the largest; a square is
// not the dearest shape of every kind, and TestEncodingsAreJudgedAtEveryShape
// takes all the others. What is refused is refused at its cheapest: a wide
// picture from a small upload. At a quarter of the size, everything is
// accepted as a square.
func TestCostlyEncodingsAreRefusedFromTheirHeader(t *testing.T) {
	type kind struct {
		name string
		head func(w, h int) []byte
		o    uint16
	}
	var accepted, refused []kind
	for _, o := range []uint16{1, 6} {
		accepted = append(accepted,
			kind{"baseline 4:2:0 JPEG", headJPEG(0xC0, 0x22, 0x11, 0x11), o},
			kind{"baseline 4:2:2 JPEG", headJPEG(0xC0, 0x21, 0x11, 0x11), o},
			kind{"baseline 4:4:4 JPEG", headJPEG(0xC0, 0x11, 0x11, 0x11), o},
			kind{"baseline 4:1:1 JPEG", headJPEG(0xC0, 0x41, 0x11, 0x11), o},
			kind{"baseline 4:4:0 JPEG", headJPEG(0xC0, 0x12, 0x11, 0x11), o},
			kind{"baseline 4:1:0 JPEG", headJPEG(0xC0, 0x42, 0x11, 0x11), o},
			kind{"baseline grey JPEG", headJPEG(0xC0, 0x11), o},
			kind{"progressive 4:2:0 JPEG", headJPEG(0xC2, 0x22, 0x11, 0x11), o},
			kind{"progressive 4:1:1 JPEG", headJPEG(0xC2, 0x41, 0x11, 0x11), o},
			kind{"progressive 4:1:0 JPEG", headJPEG(0xC2, 0x42, 0x11, 0x11), o},
			kind{"progressive grey JPEG", headJPEG(0xC2, 0x11), o},
			kind{"palette PNG", headPNG(3, 8, false), o},
			kind{"interlaced palette PNG", headPNG(3, 8, true), o},
			kind{"8-bit grey PNG", headPNG(0, 8, false), o},
			kind{"16-bit grey PNG", headPNG(0, 16, false), o},
			kind{"interlaced 16-bit grey PNG", headPNG(0, 16, true), o},
			kind{"8-bit RGB PNG", headPNG(2, 8, false), o},
			kind{"8-bit RGBA PNG", headPNG(6, 8, false), o},
			kind{"8-bit grey+alpha PNG", headPNG(4, 8, false), o},
		)
		refused = append(refused,
			kind{"progressive 4:4:4 JPEG", headJPEG(0xC2, 0x11, 0x11, 0x11), o},
			kind{"progressive CMYK JPEG", headJPEG(0xC2, 0x11, 0x11, 0x11, 0x11), o},
			kind{"progressive RGB JPEG", headRGBJPEG(0xC2), o},
			kind{"interlaced 16-bit RGB PNG", headPNG(2, 16, true), o},
			kind{"interlaced 16-bit RGBA PNG", headPNG(6, 16, true), o},
			kind{"interlaced 16-bit grey+tRNS PNG", headPNGWithTRNS(0, 16, true), o},
		)
	}
	accepted = append(accepted,
		kind{"baseline CMYK JPEG", headJPEG(0xC0, 0x11, 0x11, 0x11, 0x11), 1},
		kind{"baseline RGB JPEG", headRGBJPEG(0xC0), 1},
		kind{"progressive 4:2:2 JPEG", headJPEG(0xC2, 0x21, 0x11, 0x11), 1},
		kind{"progressive 4:4:0 JPEG", headJPEG(0xC2, 0x12, 0x11, 0x11), 1},
		kind{"interlaced 8-bit RGB PNG", headPNG(2, 8, true), 1},
		kind{"interlaced 8-bit RGBA PNG", headPNG(6, 8, true), 1},
		kind{"interlaced 8-bit grey+alpha PNG", headPNG(4, 8, true), 1},
		kind{"lossy WebP", headWebP("lossy"), 1},
	)
	refused = append(refused, kind{"lossy WebP with transparency", headWebP("alpha"), 1})
	refused = append(refused,
		kind{"baseline CMYK JPEG", headJPEG(0xC0, 0x11, 0x11, 0x11, 0x11), 6},
		kind{"baseline RGB JPEG", headRGBJPEG(0xC0), 6},
		kind{"progressive 4:2:2 JPEG", headJPEG(0xC2, 0x21, 0x11, 0x11), 6},
		kind{"progressive 4:4:0 JPEG", headJPEG(0xC2, 0x12, 0x11, 0x11), 6},
		kind{"interlaced 8-bit RGB PNG", headPNG(2, 8, true), 6},
		kind{"interlaced 8-bit RGBA PNG", headPNG(6, 8, true), 6},
		kind{"interlaced 8-bit grey+alpha PNG", headPNG(4, 8, true), 6},
		kind{"16-bit RGB PNG", headPNG(2, 16, false), 6},
		kind{"16-bit RGBA PNG", headPNG(6, 16, false), 6},
		kind{"16-bit grey+tRNS PNG", headPNGWithTRNS(0, 16, false), 6},
	)

	const side = 5000 // a square of MaxPixels
	if side*side != MaxPixels {
		t.Fatalf("%d² is not MaxPixels", side)
	}
	const small, large = 64 << 10, 8 << 20
	check := func(k kind, w, h, size int, want error) {
		t.Helper()
		data := headerUpload(k.head(w, h), k.o, size)
		if _, err := Inspect(data); !errors.Is(err, want) {
			t.Errorf("Inspect(%s, o%d, %dx%d, %d KiB) = %v, want %v", k.name, k.o, w, h, size>>10, err, want)
		}
	}
	for _, k := range accepted {
		check(k, side, side, large, nil)
	}
	for _, k := range refused {
		check(k, 4*side, side/4, small, ErrImageTooCostly)
		check(k, side/2, side/2, large, nil)
	}
	// The "either" kinds: a 16-bit colour PNG that is neither interlaced nor
	// turned (eight bytes a pixel) is refused at a square from a large upload,
	// but stored when wide from a small one. A grey PNG with a transparent
	// colour counts as a colour PNG here: with the key applied it decodes to
	// NRGBA64 the same way. The JPEGs that are either by their shape are
	// TestEncodingsAreJudgedAtEveryShape's.
	for _, k := range []kind{
		{"16-bit RGB PNG", headPNG(2, 16, false), 1},
		{"16-bit RGBA PNG", headPNG(6, 16, false), 1},
		{"16-bit grey+tRNS PNG", headPNGWithTRNS(0, 16, false), 1},
	} {
		check(k, side, side, large, ErrImageTooCostly)
		check(k, 4*side, side/4, small, nil)
	}
	// A GIF is either by the pixels its frames come to and the upload's size,
	// at any shape: it is played on four bytes for every pixel of its canvas
	// (gifPlayScratch) while all its frames are held, into a stored file that
	// can come to four times the upload. Frames of two canvases are stored
	// from a large upload, of three from one of up to about 6.9 MiB, and of
	// four, the most there can be (maxGIFFramePixels), from one of up to
	// about 3.9 MiB. An animation whose tables give more than 255 colours is
	// played on eight, for the frames it may put back: at this size only one
	// whose frames come to less than two thirds of a canvas, half of one
	// from an upload of up to about 2.4 MiB; from a large upload, two whole
	// frames up to about 17 megapixels and four up to about 15. One frame of
	// many colours is a still, which puts nothing back, and is charged as
	// any other. At a quarter of the size, the most there can be is stored
	// from a large upload, of many colours or few.
	for _, c := range []struct {
		w, h, frameH, frames, size int
		many                       bool
		want                       error
	}{
		{side, side, side, 2, large, false, nil},
		{side, side, side, 3, 66 << 20 / 10, false, nil},
		{side, side, side, 3, 72 << 20 / 10, false, ErrImageTooCostly},
		{side, side, side, 4, 36 << 20 / 10, false, nil},
		{side, side, side, 4, 42 << 20 / 10, false, ErrImageTooCostly},
		{4 * side, side / 4, side / 4, 2, large, false, nil},
		{4 * side, side / 4, side / 4, 3, 66 << 20 / 10, false, nil},
		{4 * side, side / 4, side / 4, 3, 72 << 20 / 10, false, ErrImageTooCostly},
		{4 * side, side / 4, side / 4, 4, 36 << 20 / 10, false, nil},
		{4 * side, side / 4, side / 4, 4, 42 << 20 / 10, false, ErrImageTooCostly},
		{side / 2, side / 2, side / 2, 16, large, false, nil},
		{side, side, side, 1, large, true, nil},
		{side, side, side / 3, 2, small, true, nil},
		{side, side, side * 7 / 20, 2, small, true, ErrImageTooCostly},
		{side, side, side / 4, 2, 22 << 20 / 10, true, nil},
		{side, side, side / 4, 2, 26 << 20 / 10, true, ErrImageTooCostly},
		{4100, 4100, 4100, 2, large, true, nil},
		{4250, 4250, 4250, 2, large, true, ErrImageTooCostly},
		{3800, 3800, 3800, 4, large, true, nil},
		{3900, 3900, 3900, 4, large, true, ErrImageTooCostly},
		{side / 2, side / 2, side / 2, 16, large, true, nil},
	} {
		data := gifHeaderUpload(c.w, c.h, c.frameH, c.frames, c.size, c.many)
		if _, err := Inspect(data); !errors.Is(err, c.want) {
			t.Errorf("Inspect(GIF of %d %dx%d frames on %dx%d, many colours %v, %d KiB) = %v, want %v", c.frames, c.w, c.frameH, c.w, c.h, c.many, len(data)>>10, err, c.want)
		}
	}
	// One pixel more than the cap is refused whatever it is.
	check(kind{"baseline 4:2:0 JPEG", headJPEG(0xC0, 0x22, 0x11, 0x11), 1}, side+1, side, small, ErrImageTooLarge)

	// Lossless WebP and a VP8L-compressed alpha plane are the one case refused
	// by the size of a bitstream rather than the picture's resolution:
	// golang.org/x/image's vp8l decoder sizes its Huffman groups and tree nodes
	// from the bitstream it reads, not from any header field, so one over
	// about 224 KiB is refused whatever its resolution, and a small one
	// accepted however large the rest of the upload (vp8lMemory,
	// TestVP8LHuffmanMemoryIsCharged).
	for _, k := range []string{"lossless", "calpha"} {
		for _, c := range []struct {
			n, size int
			want    error
		}{
			{small, small, nil},
			{1 << 20, 1 << 20, ErrImageTooCostly},
			{small, large, nil},
		} {
			data := webpOfBitstream(k, 256, 256, c.n, c.size)
			if _, err := Inspect(data); !errors.Is(err, c.want) {
				t.Errorf("Inspect(%s WebP, 256x256, %d KiB of bitstream in %d KiB) = %v, want %v", k, c.n>>10, len(data)>>10, err, c.want)
			}
		}
	}
	// An uncompressed alpha plane needs no such limit: it is not decoded
	// through vp8l, so a large upload of one is accepted at a small size.
	check(kind{"lossy WebP, uncompressed alpha", headWebP("alpha"), 1}, 256, 256, large, nil)
}

// Inspect reads what an uploader wrote with walks of its own before any
// decoder sees it, besides image.DecodeConfig's: the JPEG segment walk for the
// frame header, the quantisation tables and the orientation (readJPEGFrame,
// jpegOrientation), the PNG chunk walks (pngBytesPerPixel, pngHasTRNS,
// pngOrientation), the GIF block walk (gifFramePixels, which
// FuzzGIFFramePixels holds to the decoder) and webpPicture's walk over a
// WebP's chunks. Whatever the bytes, each must read in bounds and answer
// within its range; Inspect must refuse with one of its three errors, each a
// 400; and what it admits must come with an estimate within the budget that
// covers at least a byte for every pixel of its canvas, and a limit on the
// stored file above four times the upload, so a walk that misreads a header
// cannot let a large picture through as a cheap one. The seeds are a file of
// every format and kind the cost model tells apart.
func FuzzInspect(f *testing.F) {
	var buf bytes.Buffer
	_ = gif.EncodeAll(&buf, &gif.GIF{
		Image: []*image.Paletted{image.NewPaletted(image.Rect(0, 0, 6, 4), palette.Plan9), image.NewPaletted(image.Rect(1, 1, 3, 3), palette.Plan9)},
		Delay: []int{1, 2},
	})
	seeds := [][]byte{
		headerUpload(jpegHeader(0xC0, 64, 48, 0x22, 0x11, 0x11), 6, 0),
		headerUpload(jpegHeader(0xC0, 63, 47, 0x41, 0x11, 0x11), 3, 0),
		headerUpload(jpegHeader(0xC2, 64, 48, 0x11, 0x11, 0x11), 1, 0),
		headerUpload(jpegHeader(0xC2, 64, 48, 0x11, 0x11, 0x11, 0x11), 8, 0),
		headerUpload(jpegHeader(0xC0, 64, 48, 0x11), 1, 0),
		headerUpload(headRGBJPEG(0xC2)(64, 48), 1, 0),
		readTestdata(f, "photo-q30.jpg")[:1024],
		headerUpload(pngHeader(64, 48, 3, 8, false), 1, 0),
		headerUpload(pngHeader(64, 48, 0, 16, true), 6, 0),
		headerUpload(pngHeader(64, 48, 6, 16, true), 1, 0),
		headerUpload(headPNGWithTRNS(0, 8, false)(64, 48), 5, 0),
		buf.Bytes(),
		readTestdata(f, "lossy.webp"),
		readTestdata(f, "lossy-alpha.webp"),
		readTestdata(f, "sample.webp"),
		webpHeader("calpha", 64, 48),
		webpFile(vp8xChunk(vp8xFlagAnimation, 64, 48), riffChunk{id: "ANIM", payload: make([]byte, 6)},
			anmfChunk(0, 0, 64, 48, riffChunk{id: "VP8L", payload: make([]byte, 10)})),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if fr, framed := readJPEGFrame(data); framed && (fr.comps != 1 && fr.comps != 3 && fr.comps != 4 || fr.blocks < 0) {
			t.Fatalf("readJPEGFrame read %d components in %d blocks", fr.comps, fr.blocks)
		}
		if o := jpegOrientation(data); o < 1 || o > 8 {
			t.Fatalf("jpegOrientation = %d", o)
		}
		if o := pngOrientation(data); o < 1 || o > 8 {
			t.Fatalf("pngOrientation = %d", o)
		}
		if bpp := pngBytesPerPixel(data); bpp != 1 && bpp != 2 && bpp != 4 && bpp != 8 {
			t.Fatalf("pngBytesPerPixel = %d", bpp)
		}
		if _, _, n := webpPicture(data); n < 0 || n > int64(len(data))+vp8lAlphaHeader {
			t.Fatalf("webpPicture charges %d bytes of bitstream in a %d-byte file", n, len(data))
		}

		in, err := inspect(data)
		if err != nil {
			if !errors.Is(err, ErrUnsupportedFormat) && !errors.Is(err, ErrCorruptImage) && !errors.Is(err, ErrImageTooLarge) && !errors.Is(err, ErrImageTooCostly) {
				t.Fatalf("Inspect refused with %v", err)
			}
			return
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("Inspect admitted a file whose header does not decode: %v", err)
		}
		if format, _ := DetectFormat(data); in.format != format {
			t.Fatalf("Inspect read %v from a %v file", in.format, format)
		}
		if px := int64(cfg.Width) * int64(cfg.Height); in.cost < px+processingSlack || in.cost > processingBudget {
			t.Fatalf("Inspect admitted %dx%d at a cost of %d", cfg.Width, cfg.Height, in.cost)
		}
		if in.limit < 4*int64(len(data)) {
			t.Fatalf("stored-file limit %d for a %d-byte upload", in.limit, len(data))
		}
	})
}

// shapeClaim is what the README says of one kind of file at the full size,
// from an upload of the default 8 MiB, at each orientation in turns.
type shapeClaim struct {
	name   string
	head   func(w, h int) []byte
	format Format
	turns  []uint16
	// upTo, when it is not 0, is the ratio of the longer side to the shorter
	// below which every shape is accepted, and some shape within a tenth past
	// it is refused; at444, when it is set, narrows that to the shapes a turn
	// stores at 4:4:4 at that subsampling (turnedSubsampling), and the rest
	// are accepted at every shape.
	upTo  float64
	at444 image.YCbCrSubsampleRatio
	// longest, for a PNG, is the longer side below which every shape is
	// accepted, and some shape within a tenth past it is refused. A JPEG's and
	// a WebP's sides stop far short of it.
	longest int
}

// everyTurn is every EXIF orientation.
var everyTurn = []uint16{1, 2, 3, 4, 5, 6, 7, 8}

// largestSide is the longest side a file of format can have: a JPEG's is a
// 16-bit field, and a lossy WebP's a 14-bit one in its bitstream, which has
// to be the size the VP8X chunk says (stillImage). A PNG's is limited only
// by MaxPixels.
func largestSide(format Format) int {
	switch format.Name {
	case FormatJPEG.Name:
		return 0xFFFF
	case FormatWebP.Name:
		return 0x3FFF
	}
	return MaxPixels
}

// eachShape calls f with the largest pictures of every shape that fit in
// area pixels and sides of max: for each shorter side, the longest side
// that fits and the three below it, which between them take every remainder
// the chroma samples of a turned JPEG depend on, one way round and the
// other.
func eachShape(area int64, max int, f func(w, h int)) {
	for s := 1; int64(s)*int64(s) <= area; s++ {
		top := min(int(area/int64(s)), max)
		for l := top; l > top-4 && l >= s; l-- {
			f(l, s)
			if l != s {
				f(s, l)
			}
		}
	}
}

// keepsSubsampling reports whether turning a w×h YCbCr picture at ratio by
// orientation o keeps it in its planes, rather than at 4:4:4.
func keepsSubsampling(ratio image.YCbCrSubsampleRatio, o uint16, w, h int) bool {
	_, ok := turnedSubsampling(ratio, int(o), w, h)
	return ok
}

// jpegFrameAt is where a JPEG's frame header starts, or -1.
func jpegFrameAt(data []byte) int {
	for i := 2; i+4 <= len(data) && data[i] == 0xFF; i += 2 + int(binary.BigEndian.Uint16(data[i+2:])) {
		if m := data[i+1]; m >= 0xC0 && m <= 0xC2 {
			return i
		}
	}
	return -1
}

// The lists above hold at every shape, not only as a square: a picture of
// the same size costs more the longer and thinner it is, because a JPEG's
// sides are rounded up to whole blocks, a turned one whose chroma samples do
// not line up is copied at 4:4:4 (turnedSubsampling), and the thumbnail's
// scaler holds 64 bytes and more for every pixel of the longer side
// (thumbScratch). So every kind the README lists as accepted, or accepted up
// to a shape, is judged at every shape of the full size its format can have
// (eachShape) and at every orientation it is listed at, from an upload of
// the default 8 MiB: a progressive 4:2:0 or 4:1:1 JPEG that a turn stores at
// 4:4:4 is accepted up to about 8:1 or 2:1, a progressive 4:2:2 or 4:4:0 one
// that is not turned up to about 60:1, and a PNG until its longer side passes
// about 750,000 pixels in colour or 1.3 to 1.5 million otherwise; everything
// else in the accepted list at every shape. At a quarter of the size, every
// kind is accepted at every shape up to a thousand to one, and a PNG of any
// size is refused once its longer side passes 2.2 to 3.7 million.
//
// That is millions of shapes, so this reads the estimate Inspect compares
// with the budget (processingCost), with a JPEG's frame header rewritten in
// place for each, in as many goroutines as there are cores; Inspect itself is
// asked about the dearest shape of each kind and orientation, and the least
// elongated it refuses, and has to agree.
func TestEncodingsAreJudgedAtEveryShape(t *testing.T) {
	const upload = 8 << 20
	jpg, png, webp := FormatJPEG, FormatPNG, FormatWebP
	claims := []shapeClaim{
		{name: "baseline 4:2:0 JPEG", head: headJPEG(0xC0, 0x22, 0x11, 0x11), format: jpg, turns: everyTurn},
		{name: "baseline 4:2:2 JPEG", head: headJPEG(0xC0, 0x21, 0x11, 0x11), format: jpg, turns: everyTurn},
		{name: "baseline 4:4:4 JPEG", head: headJPEG(0xC0, 0x11, 0x11, 0x11), format: jpg, turns: everyTurn},
		{name: "baseline 4:1:1 JPEG", head: headJPEG(0xC0, 0x41, 0x11, 0x11), format: jpg, turns: everyTurn},
		{name: "baseline 4:4:0 JPEG", head: headJPEG(0xC0, 0x12, 0x11, 0x11), format: jpg, turns: everyTurn},
		{name: "baseline 4:1:0 JPEG", head: headJPEG(0xC0, 0x42, 0x11, 0x11), format: jpg, turns: everyTurn},
		{name: "baseline grey JPEG", head: headJPEG(0xC0, 0x11), format: jpg, turns: everyTurn},
		{name: "baseline CMYK JPEG", head: headJPEG(0xC0, 0x11, 0x11, 0x11, 0x11), format: jpg, turns: []uint16{1}},
		{name: "baseline RGB JPEG", head: headRGBJPEG(0xC0), format: jpg, turns: []uint16{1}},
		{name: "progressive 4:1:0 JPEG", head: headJPEG(0xC2, 0x42, 0x11, 0x11), format: jpg, turns: everyTurn},
		{name: "progressive grey JPEG", head: headJPEG(0xC2, 0x11), format: jpg, turns: everyTurn},
		{name: "progressive 4:2:0 JPEG", head: headJPEG(0xC2, 0x22, 0x11, 0x11), format: jpg, turns: everyTurn,
			upTo: 8, at444: image.YCbCrSubsampleRatio420},
		{name: "progressive 4:1:1 JPEG", head: headJPEG(0xC2, 0x41, 0x11, 0x11), format: jpg, turns: everyTurn,
			upTo: 2, at444: image.YCbCrSubsampleRatio411},
		{name: "progressive 4:2:2 JPEG", head: headJPEG(0xC2, 0x21, 0x11, 0x11), format: jpg, turns: []uint16{1}, upTo: 60},
		{name: "progressive 4:4:0 JPEG", head: headJPEG(0xC2, 0x12, 0x11, 0x11), format: jpg, turns: []uint16{1}, upTo: 60},
		{name: "palette PNG", head: headPNG(3, 8, false), format: png, turns: everyTurn, longest: 1_500_000},
		{name: "interlaced palette PNG", head: headPNG(3, 8, true), format: png, turns: everyTurn, longest: 1_500_000},
		{name: "8-bit grey PNG", head: headPNG(0, 8, false), format: png, turns: everyTurn, longest: 1_500_000},
		{name: "16-bit grey PNG", head: headPNG(0, 16, false), format: png, turns: everyTurn, longest: 1_300_000},
		{name: "interlaced 16-bit grey PNG", head: headPNG(0, 16, true), format: png, turns: everyTurn, longest: 1_300_000},
		{name: "8-bit RGB PNG", head: headPNG(2, 8, false), format: png, turns: everyTurn, longest: 750_000},
		{name: "8-bit RGBA PNG", head: headPNG(6, 8, false), format: png, turns: everyTurn, longest: 750_000},
		{name: "8-bit grey+alpha PNG", head: headPNG(4, 8, false), format: png, turns: everyTurn, longest: 750_000},
		{name: "8-bit grey+tRNS PNG", head: headPNGWithTRNS(0, 8, false), format: png, turns: everyTurn, longest: 750_000},
		{name: "interlaced 8-bit RGB PNG", head: headPNG(2, 8, true), format: png, turns: []uint16{1}, longest: 750_000},
		{name: "interlaced 8-bit RGBA PNG", head: headPNG(6, 8, true), format: png, turns: []uint16{1}, longest: 750_000},
		{name: "interlaced 8-bit grey+alpha PNG", head: headPNG(4, 8, true), format: png, turns: []uint16{1}, longest: 750_000},
		{name: "lossy WebP", head: headWebP("lossy"), format: webp, turns: []uint16{1}},
	}
	// The refused kinds, and the 16-bit colour PNGs that are refused or
	// accepted by their shape and the upload's size, are judged at a quarter
	// of the size along with the rest.
	quarter := append(append([]shapeClaim(nil), claims...), []shapeClaim{
		{name: "progressive 4:4:4 JPEG", head: headJPEG(0xC2, 0x11, 0x11, 0x11), format: jpg},
		{name: "progressive CMYK JPEG", head: headJPEG(0xC2, 0x11, 0x11, 0x11, 0x11), format: jpg},
		{name: "progressive RGB JPEG", head: headRGBJPEG(0xC2), format: jpg},
		{name: "16-bit RGB PNG", head: headPNG(2, 16, false), format: png},
		{name: "16-bit RGBA PNG", head: headPNG(6, 16, false), format: png},
		{name: "16-bit grey+tRNS PNG", head: headPNGWithTRNS(0, 16, false), format: png},
		{name: "interlaced 16-bit RGB PNG", head: headPNG(2, 16, true), format: png},
		{name: "interlaced 16-bit RGBA PNG", head: headPNG(6, 16, true), format: png},
		{name: "interlaced 16-bit grey+tRNS PNG", head: headPNGWithTRNS(0, 16, true), format: png},
		{name: "lossy WebP with transparency", head: headWebP("alpha"), format: webp},
	}...)
	for i := range quarter {
		quarter[i].turns = everyTurn
		if quarter[i].format == webp {
			quarter[i].turns = []uint16{1}
		}
	}

	// judge runs one kind at one orientation, at every shape of area, and
	// returns what it found wrong.
	judge := func(data []byte, k shapeClaim, o uint16, area int64, ratio float64) []string {
		// data is the upload, of the default 8 MiB. Its header is the
		// file's, ending in its last chunk or the start of its scan, and
		// the rest is nothing a picture is made of: zeros to 4 KiB and
		// comments after them (jpegComments), as headerUpload makes it up
		// with comments and zeros, but for a PNG's filler chunk. What the
		// estimate reads of a file is its header and its length, and of a
		// JPEG the application segments after its scans as well, of which
		// comments are none.
		head := headerUpload(k.head(1000, 1000), o, 0)
		if len(head) > 4<<10 {
			return []string{"a header over 4 KiB"}
		}
		clear(data[:4<<10])
		copy(data, head)
		frame := -1
		if k.format == jpg {
			if frame = jpegFrameAt(data); frame < 0 {
				return []string{"no frame header"}
			}
		}
		var (
			wrong       []string
			wrongCount  int
			gated       bool
			least       = math.Inf(1) // the least ratio, or longer side, that is refused where a limit applies
			leastShape  image.Point
			leastCost   int64
			dearest     image.Point
			dearestCost int64
		)
		eachShape(area, largestSide(k.format), func(w, h int) {
			if frame >= 0 {
				binary.BigEndian.PutUint16(data[frame+5:], uint16(h))
				binary.BigEndian.PutUint16(data[frame+7:], uint16(w))
			}
			cost := processingCost(k.format, data, image.Config{Width: w, Height: h}, gifFrames{})
			if cost > dearestCost {
				dearest, dearestCost = image.Pt(w, h), cost
			}
			refused := cost > processingBudget
			long, short := max(w, h), min(w, h)
			shape := float64(long) / float64(short)
			limit, at := math.Inf(1), 0.0
			switch {
			case ratio > 0: // at a quarter of the size
				if shape > ratio {
					return
				}
			case k.upTo > 0:
				if k.at444 == 0 || !keepsSubsampling(k.at444, o, w, h) {
					gated, limit, at = true, k.upTo, shape
				}
			case k.longest > 0:
				gated, limit, at = true, float64(k.longest), float64(long)
			}
			if refused && at < least && gated {
				least, leastShape, leastCost = at, image.Pt(w, h), cost
			}
			if refused && at < limit {
				if wrongCount++; wrongCount <= 3 {
					wrong = append(wrong, fmt.Sprintf("%dx%d refused (%.0f MiB)", w, h, float64(cost)/(1<<20)))
				}
			}
		})
		if wrongCount > 3 {
			wrong = append(wrong, fmt.Sprintf("and %d more shapes refused", wrongCount-3))
		}
		if ratio == 0 && gated {
			limit := k.upTo
			if limit == 0 {
				limit = float64(k.longest)
			}
			switch {
			case math.IsInf(least, 1):
				wrong = append(wrong, fmt.Sprintf("accepted at every shape, where the README has it refused from about %g", limit))
			case least >= 1.1*limit:
				wrong = append(wrong, fmt.Sprintf("refused from %.1f, more than a tenth past the %g the README gives", least, limit))
			}
		}
		// The estimate of the shapes above, read with the frame header
		// rewritten, is the estimate of a header made at that size, and
		// Inspect refuses by it, at the dearest shape and the least
		// elongated refused. Once for each kind, the header comes with the
		// rest of the upload as headerUpload makes it, too.
		for p, cost := range map[image.Point]int64{dearest: dearestCost, leastShape: leastCost} {
			if p == (image.Point{}) {
				continue
			}
			head := headerUpload(k.head(p.X, p.Y), o, 0)
			clear(data[:4<<10])
			copy(data, head)
			files := [][]byte{data}
			if ratio == 0 && o == k.turns[0] && p == dearest {
				files = append(files, headerUpload(k.head(p.X, p.Y), o, upload))
			}
			for _, file := range files {
				if c := processingCost(k.format, file, image.Config{Width: p.X, Height: p.Y}, gifFrames{}); c != cost {
					wrong = append(wrong, fmt.Sprintf("%dx%d estimated at %d as a file of its own, %d in the scan", p.X, p.Y, c, cost))
				}
				_, err := Inspect(file)
				if refused := cost > processingBudget; refused != errors.Is(err, ErrImageTooCostly) || !refused && err != nil {
					wrong = append(wrong, fmt.Sprintf("Inspect(%dx%d) = %v where the estimate is %d", p.X, p.Y, err, cost))
				}
			}
		}
		return wrong
	}

	type job struct {
		k     shapeClaim
		o     uint16
		area  int64
		ratio float64
	}
	var jobs []job
	for _, k := range claims {
		for _, o := range k.turns {
			jobs = append(jobs, job{k, o, MaxPixels, 0})
		}
	}
	for _, k := range quarter {
		for _, o := range k.turns {
			jobs = append(jobs, job{k, o, MaxPixels / 4, 1000})
		}
	}
	work := make(chan job)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := make([]byte, upload)
			copy(data[4<<10:], jpegComments(upload-4<<10))
			for j := range work {
				size := "full size"
				if j.ratio > 0 {
					size = "a quarter of the size"
				}
				for _, w := range judge(data, j.k, j.o, j.area, j.ratio) {
					t.Errorf("%s, o%d, at %s: %s", j.k.name, j.o, size, w)
				}
			}
		}()
	}
	for _, j := range jobs {
		work <- j
	}
	close(work)
	wg.Wait()

	// However narrow a PNG is, it is refused once its longer side passes
	// 2.2 to 3.7 million pixels, by its kind and which way it lies: every
	// kind at every orientation, from an upload of a kilobyte, as a row and
	// as a column, is accepted at 2.1 million and refused at 3.7 million.
	// The thumbnail's scaler sets the limit for a column, and for a row the
	// scaler and image/png's rows of the picture together (pngRowBytes,
	// pngEncoderRows), which is why a row of more bytes a pixel stops sooner.
	for _, k := range quarter {
		if k.format != png {
			continue
		}
		for _, o := range k.turns {
			for _, size := range []image.Point{{2_100_000, 1}, {1, 2_100_000}} {
				if _, err := Inspect(headerUpload(k.head(size.X, size.Y), o, 1<<10)); err != nil {
					t.Errorf("Inspect(%s, o%d, %dx%d, 1 KiB) = %v, want it accepted", k.name, o, size.X, size.Y, err)
				}
			}
			for _, size := range []image.Point{{3_700_000, 1}, {1, 3_700_000}} {
				if _, err := Inspect(headerUpload(k.head(size.X, size.Y), o, 1<<10)); !errors.Is(err, ErrImageTooCostly) {
					t.Errorf("Inspect(%s, o%d, %dx%d, 1 KiB) = %v, want it refused", k.name, o, size.X, size.Y, err)
				}
			}
		}
	}
	if _, err := Inspect(headerUpload(headPNG(3, 8, false)(3_600_000, 1), 1, 1<<10)); err != nil {
		t.Errorf("Inspect(palette PNG, 3600000x1, 1 KiB) = %v, want it accepted: the limit is about 3.7 million", err)
	}
}

// The photos phones and cameras save at 24 megapixels are taken at their full
// size, turned on their side or not, from an upload as large as the default
// limit: they are not all 24 million pixels.
func TestTwentyFourMegapixelPhotosAreAccepted(t *testing.T) {
	for _, size := range []image.Point{
		{5712, 4284}, // an iPhone 15 Pro's or later's 24-megapixel photo
		{6048, 4024}, // a 24.5-megapixel full-frame camera's
		{6000, 4000}, // a 24-megapixel camera's
	} {
		for _, wh := range []image.Point{size, {size.Y, size.X}} {
			for _, o := range []uint16{1, 6} {
				for _, samplings := range [][]byte{{0x22, 0x11, 0x11}, {0x21, 0x11, 0x11}} {
					data := headerUpload(jpegHeader(0xC0, wh.X, wh.Y, samplings...), o, 8<<20)
					if _, err := Inspect(data); err != nil {
						t.Errorf("Inspect(%dx%d baseline JPEG %#x, o%d) = %v, want it accepted", wh.X, wh.Y, samplings[0], o, err)
					}
				}
			}
		}
	}
}

// The stored file may not grow past four times the upload and half a byte a
// pixel. A lossless WebP of two rows of noise taking turns compresses to
// those two rows, because WebP can refer back any distance; as a PNG every
// row is new to deflate's 32 KiB window, and it would be stored at 30 times
// the size it came in at. It is refused instead.
func TestStoredFileCannotOutgrowItsUpload(t *testing.T) {
	upload := readTestdata(t, "repeat.webp")
	if _, err := Inspect(upload); err != nil {
		t.Fatalf("Inspect = %v; the header is sound", err)
	}
	if _, err := Process(context.Background(), upload); !errors.Is(err, ErrStoredTooLarge) {
		t.Errorf("Process = %v, want ErrStoredTooLarge", err)
	}
	if n := processing.held(); n != 0 {
		t.Errorf("%d bytes of the budget still held after the refusal", n)
	}

	b := newStoredBuffer(context.Background(), 1, 10_000)
	for i := 0; i < 9; i++ {
		if _, err := b.Write(make([]byte, 1000)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if cap(b.buf) > 10_000 {
		t.Errorf("the buffer grew to %d, past its limit", cap(b.buf))
	}
	if _, err := b.Write(make([]byte, 1001)); !errors.Is(err, ErrStoredTooLarge) {
		t.Errorf("a write past the limit = %v, want ErrStoredTooLarge", err)
	}
	if _, err := b.Write(make([]byte, 1000)); err != nil || len(b.buf) != 10_000 {
		t.Errorf("a write up to the limit = %v with %d bytes, want it taken", err, len(b.buf))
	}
}

// A cut-out photo saved as a lossy WebP with transparency is stored as a PNG
// of its pixels, which is many times the size it came in at: that is what a
// lossy picture stored losslessly comes to, and it is stored, not refused
// after decoding as a file made to outgrow its upload. Its limit is what a
// PNG of its size can come to, which its cost counts from the header.
func TestCutOutWebPIsStoredAsTheLargerPNGItMakes(t *testing.T) {
	upload := readTestdata(t, "cutout-alpha.webp")
	if lossless, alpha, _ := webpPicture(upload); lossless || !alpha {
		t.Fatalf("the fixture is not a lossy WebP with an alpha plane")
	}
	got, err := Process(context.Background(), upload)
	if err != nil {
		t.Fatalf("Process = %v, want it stored", err)
	}
	if got.Format != FormatPNG {
		t.Errorf("stored as %s, want png", got.Format.Name)
	}
	if ordinary := 4*int64(len(upload)) + int64(got.Width*got.Height)/2 + 64<<10; int64(len(got.Data)) <= ordinary {
		t.Fatalf("stored %d bytes of a %d-byte upload, within the ordinary limit of %d; the fixture proves nothing", len(got.Data), len(upload), ordinary)
	}
	in, err := inspect(upload)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if in.limit != pngLimit(got.Width, got.Height, 4) {
		t.Errorf("limit = %d, want a PNG's of its size, %d", in.limit, pngLimit(got.Width, got.Height, 4))
	}
	if in.cost < 2*in.limit {
		t.Errorf("cost = %d, which does not cover a stored buffer growing to %d", in.cost, in.limit)
	}
	clearPixels := 0
	stored := decodeStored(t, got.Data).(*image.NRGBA)
	for i := 3; i < len(stored.Pix); i += 4 {
		if stored.Pix[i] == 0 {
			clearPixels++
		}
	}
	if clearPixels == 0 {
		t.Error("the stored PNG lost the cut-out's transparency")
	}
}

// pngLimit holds for what image/png makes of pictures that do not compress
// at all, rows of noise, and is not loose by much.
func TestPNGLimitHoldsForNoise(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	for _, size := range []image.Point{{1, 1}, {257, 129}, {1500, 700}} {
		m := image.NewNRGBA(image.Rect(0, 0, size.X, size.Y))
		rng.Read(m.Pix)
		var buf bytes.Buffer
		if err := png.Encode(&buf, m); err != nil {
			t.Fatalf("encode: %v", err)
		}
		rows := int64(size.Y) * (1 + 4*int64(size.X))
		if n := int64(buf.Len()); n > pngLimit(size.X, size.Y, 4) || n < rows {
			t.Errorf("%v: %d bytes of PNG, want between its %d bytes of rows and pngLimit %d", size, n, rows, pngLimit(size.X, size.Y, 4))
		}
	}
}

// The budget hands out room in the order it was asked for, so a large upload
// is not starved by small ones slipping past, and a waiter that gives up
// lets the ones behind it through.
func TestBudgetServesInOrder(t *testing.T) {
	b := newBudget(10)
	ctx := context.Background()
	hold, err := b.acquire(ctx, 6)
	if err != nil {
		t.Fatalf("acquire 6: %v", err)
	}

	order := make(chan string, 3)
	wait := func(name string, ctx context.Context, n int64) chan func() {
		got := make(chan func(), 1)
		go func() {
			release, err := b.acquire(ctx, n)
			if err != nil {
				order <- name + " gave up"
				got <- nil
				return
			}
			order <- name
			got <- release
		}()
		return got
	}
	waitQueued := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			b.mu.Lock()
			queued := b.waiters.Len()
			b.mu.Unlock()
			if queued == n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d waiting, want %d", queued, n)
			}
			time.Sleep(time.Millisecond)
		}
	}

	// 8 does not fit beside 6; 1 would, but queues behind 8.
	large := wait("large", ctx, 8)
	waitQueued(1)
	small := wait("small", ctx, 1)
	waitQueued(2)
	select {
	case name := <-order:
		t.Fatalf("%s was served while the head of the queue could not be", name)
	case <-time.After(20 * time.Millisecond):
	}
	hold()
	if got := map[string]bool{<-order: true, <-order: true}; !got["large"] || !got["small"] {
		t.Fatalf("served %v once there was room, want both", got)
	}
	(<-large)()
	(<-small)()

	// A head that gives up lets the next through.
	hold, _ = b.acquire(ctx, 6)
	cancelled, cancel := context.WithCancel(ctx)
	wait("doomed", cancelled, 8)
	waitQueued(1)
	behind := wait("behind", ctx, 4)
	waitQueued(2)
	cancel()
	got := map[string]bool{<-order: true, <-order: true}
	if !got["doomed gave up"] || !got["behind"] {
		t.Fatalf("got %v, want the cancelled head to give up and the one behind it served", got)
	}
	(<-behind)()
	hold()
	if n := b.held(); n != 0 {
		t.Errorf("%d still held after everything was given back", n)
	}

	// A cost over the whole budget waits for it to be empty, not forever.
	release, err := b.acquire(ctx, 1_000)
	if err != nil || b.held() != 10 {
		t.Fatalf("acquire over the budget = %v holding %d, want the whole budget", err, b.held())
	}
	release()
}

// A phase's first allocation is counted on top of everything the phase before
// it held at its peak, scratch included, and the rest of its allocation on top
// of what that phase kept.
func TestPeakOfCountsTheEarlierPhaseAsTheNextBegins(t *testing.T) {
	decode := phase{first: 4, alloc: 10, after: 2} // 8 of scratch, gone at its end
	turn := phase{first: 5, alloc: 6, after: 6}
	if got := peakOf(decode, turn); got != 15 {
		t.Errorf("peakOf = %d, want 15: the decoder's 10 and the turn's first 5", got)
	}
	grow := phase{first: 1, alloc: 20, after: 20}
	if got := peakOf(decode, grow); got != 22 {
		t.Errorf("peakOf = %d, want 22: the 2 kept and all 20 grown on top", got)
	}
}

// scanBombBits accumulates entropy-coded bits for a progressive JPEG, stuffing
// 0xFF with a following 0x00 as the format requires.
type scanBombBits struct {
	out  []byte
	acc  uint32
	nacc uint
}

func (b *scanBombBits) put(v uint32, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		b.acc = b.acc<<1 | (v>>uint(i))&1
		b.nacc++
		if b.nacc == 8 {
			c := byte(b.acc)
			b.out = append(b.out, c)
			if c == 0xFF {
				b.out = append(b.out, 0)
			}
			b.acc, b.nacc = 0, 0
		}
	}
}

func (b *scanBombBits) flush() []byte {
	for b.nacc != 0 {
		b.put(1, 1)
	}
	return b.out
}

// scanBombJPEG builds a w×h greyscale progressive JPEG with one DC scan and
// acScans AC scans (Ss=1..63) that each code every block as one long EOB run,
// so the picture is flat grey and every scan is a few dozen bytes yet makes
// image/jpeg revisit all the picture's blocks. It is how a progressive file
// hides unbounded decoding work behind a small upload — the memory it holds is
// a single grey progressive picture's whatever the scan count — which
// maxJPEGScanRevisits bounds.
func scanBombJPEG(w, h, acScans int) []byte {
	return progressiveScanBomb(w, h, acScans, 0x00)
}

// refinementScanBombJPEG is scanBombJPEG with AC refinement scans (Ah=1,
// Al=0), each of which image/jpeg walks every coefficient of every block for
// even inside the EOB run (refineNonZeroes).
func refinementScanBombJPEG(w, h, acScans int) []byte {
	return progressiveScanBomb(w, h, acScans, 0x10)
}

// progressiveScanBomb builds scanBombJPEG's file with ahal as each AC scan's
// successive-approximation byte.
func progressiveScanBomb(w, h, acScans int, ahal byte) []byte {
	seg := func(b []byte, marker byte, body []byte) []byte {
		b = append(b, 0xFF, marker)
		b = binary.BigEndian.AppendUint16(b, uint16(len(body)+2))
		return append(b, body...)
	}
	b := []byte{0xFF, 0xD8}
	dqt := []byte{0}
	for i := 0; i < 64; i++ {
		dqt = append(dqt, 1)
	}
	b = seg(b, 0xDB, dqt)
	sof := []byte{8}
	sof = binary.BigEndian.AppendUint16(sof, uint16(h))
	sof = binary.BigEndian.AppendUint16(sof, uint16(w))
	sof = append(sof, 1, 1, 0x11, 0) // one component, 1×1 sampling
	b = seg(b, 0xC2, sof)
	counts := make([]byte, 16)
	counts[0] = 1
	b = seg(b, 0xC4, append(append([]byte{0x00}, counts...), 0x00)) // DC: '0' → category 0
	b = seg(b, 0xC4, append(append([]byte{0x10}, counts...), 0xE0)) // AC: '0' → EOB run r=14
	blocks := ((w + 7) / 8) * ((h + 7) / 8)
	b = seg(b, 0xDA, []byte{1, 1, 0x00, 0, 0, 0}) // DC scan, Ss=Se=0
	var dc scanBombBits
	for i := 0; i < blocks; i++ {
		dc.put(0, 1)
	}
	b = append(b, dc.flush()...)
	var ac scanBombBits
	for left := blocks; left > 0; {
		run := min(left, 32767)
		if left-run > 0 && left-run < 16384 {
			run = left - 16384
		}
		ac.put(0, 1)
		ac.put(uint32(run-16384), 14)
		left -= run
	}
	acData := ac.flush()
	for s := 0; s < acScans; s++ {
		b = seg(b, 0xDA, []byte{1, 1, 0x00, 1, 63, ahal}) // AC scan, Ss=1 Se=63
		b = append(b, acData...)
	}
	return append(b, 0xFF, 0xD9)
}

// baselineScanBombJPEG builds a w×h greyscale baseline JPEG of scans scans,
// each coding every block as a zero DC difference and an immediate end of
// block, two bits a block: a flat grey picture that image/jpeg decodes whole
// once per scan, dequantising, inverse-transforming and writing out every
// block each time (reconstructBlock).
func baselineScanBombJPEG(w, h, scans int) []byte {
	seg := func(b []byte, marker byte, body []byte) []byte {
		b = append(b, 0xFF, marker)
		b = binary.BigEndian.AppendUint16(b, uint16(len(body)+2))
		return append(b, body...)
	}
	b := []byte{0xFF, 0xD8}
	dqt := []byte{0}
	for i := 0; i < 64; i++ {
		dqt = append(dqt, 1)
	}
	b = seg(b, 0xDB, dqt)
	sof := []byte{8}
	sof = binary.BigEndian.AppendUint16(sof, uint16(h))
	sof = binary.BigEndian.AppendUint16(sof, uint16(w))
	sof = append(sof, 1, 1, 0x11, 0) // one component, 1×1 sampling
	b = seg(b, 0xC0, sof)
	counts := make([]byte, 16)
	counts[0] = 1
	b = seg(b, 0xC4, append(append([]byte{0x00}, counts...), 0x00)) // DC: '0' → category 0
	b = seg(b, 0xC4, append(append([]byte{0x10}, counts...), 0x00)) // AC: '0' → end of block
	var bits scanBombBits
	for i := 0; i < 2*((w+7)/8)*((h+7)/8); i++ {
		bits.put(0, 1)
	}
	data := bits.flush()
	for s := 0; s < scans; s++ {
		b = seg(b, 0xDA, []byte{1, 1, 0x00, 0, 63, 0})
		b = append(b, data...)
	}
	return append(b, 0xFF, 0xD9)
}

// A progressive JPEG can repeat a short, cheap scan so that image/jpeg
// revisits every block of the picture thousands of times — decoding work the
// pixel cap and the memory estimate do not see, since the file is small and
// holds no more than one progressive picture. jpegScanBlockVisits counts that
// work from the header, and Inspect refuses a file past maxJPEGScanRevisits
// passes over its blocks as too costly to process, before any cooldown, while
// the progressive files encoders actually write (a handful of passes) are
// admitted.
func TestJPEGScanBombIsRefusedFromHeader(t *testing.T) {
	// The fixtures, encoders' own output, baseline and progressive, are well
	// under the cap and admitted.
	for _, name := range []string{"cost-prog420.jpg", "cost-prog444.jpg", "cost-progcmyk.jpg", "cost-proggray.jpg",
		"cost-411.jpg", "cost-cmyk.jpg", "cost-rgb.jpg", "photo-q30.jpg"} {
		data := readTestdata(t, name)
		f, framed := readJPEGFrame(data)
		if !framed {
			t.Fatalf("%s: readJPEGFrame did not read a frame", name)
		}
		if visits := jpegScanBlockVisits(data, f); visits > maxJPEGScanRevisits*f.blocks {
			t.Errorf("%s: %d scan visits over the cap of %d×%d blocks", name, visits, maxJPEGScanRevisits, f.blocks)
		}
		if _, err := Inspect(data); err != nil {
			t.Errorf("Inspect(%s) = %v, want it admitted", name, err)
		}
	}

	// A grey progressive file: each AC scan and the DC scan visit every block
	// once (one component), so acScans AC scans plus the DC scan come to
	// (acScans+1) passes. The cap admits exactly maxJPEGScanRevisits passes.
	const w, h = 1000, 1000
	blocks := int64(((w + 7) / 8) * ((h + 7) / 8))
	atCap := scanBombJPEG(w, h, maxJPEGScanRevisits-1) // (maxJPEGScanRevisits-1)+1 passes
	if _, err := Inspect(atCap); err != nil {
		t.Errorf("Inspect(%d passes) = %v, want admitted at the cap", maxJPEGScanRevisits, err)
	}
	overCap := scanBombJPEG(w, h, maxJPEGScanRevisits) // one pass over the cap
	if _, err := Inspect(overCap); !errors.Is(err, ErrImageTooCostly) {
		t.Errorf("Inspect(%d passes) = %v, want ErrImageTooCostly", maxJPEGScanRevisits+1, err)
	}
	// The walk's count is exactly the passes, and nothing smaller admits the
	// over-cap file by miscounting.
	f, _ := readJPEGFrame(overCap)
	if got, want := jpegScanBlockVisits(overCap, f), int64(maxJPEGScanRevisits+1)*blocks; got != want {
		t.Errorf("jpegScanBlockVisits = %d, want %d", got, want)
	}
}

// image/jpeg does more on some block visits than on others: a baseline scan
// dequantises, inverse-transforms and writes out every block it visits, about
// six times the work of a progressive first pass, and an AC refinement scan
// walks every coefficient of its band in every block, even inside an EOB
// run, about twice it. jpegScanBlockVisits weighs each scan by that work, so
// neither kind buys more decoding under the cap than first passes do: a grey
// file of repeated baseline scans is admitted up to 10 of them (60 passes'
// work) and refused at 11 (66), and one of refinement scans after its DC scan
// up to 31 (63) and refused at 32 (65). Counted as plain visits, both were
// admitted up to 64 scans — a 25-megapixel 4:2:2 picture's 64 baseline scans
// decoded for about 14 s of CPU, and its refinement scans for about 5.5 s,
// where 64 first passes take about 3 to 4. Every file here decodes whole in
// image/jpeg, so the count is of work it really does.
func TestJPEGScanCountWeighsEachScanByItsWork(t *testing.T) {
	const w, h = 1000, 1000
	blocks := int64(((w + 7) / 8) * ((h + 7) / 8))
	for _, c := range []struct {
		name string
		make func(scans int) []byte
		// fixed and perScan are the file's passes of work: its DC scan,
		// and each of the scans it repeats.
		fixed, perScan int64
		admitted       int
	}{
		{"baseline", func(n int) []byte { return baselineScanBombJPEG(w, h, n) },
			0, jpegBaselineVisitWork, 10},
		{"refinement", func(n int) []byte { return refinementScanBombJPEG(w, h, n) },
			1, jpegRefineVisitWork, 31},
	} {
		for _, n := range []int{c.admitted, c.admitted + 1} {
			data := c.make(n)
			if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
				t.Fatalf("%s, %d scans: image/jpeg does not decode it: %v", c.name, n, err)
			}
			f, framed := readJPEGFrame(data)
			if !framed {
				t.Fatalf("%s, %d scans: readJPEGFrame did not read a frame", c.name, n)
			}
			if got, want := jpegScanBlockVisits(data, f), (c.fixed+c.perScan*int64(n))*blocks; got != want {
				t.Errorf("%s, %d scans: jpegScanBlockVisits = %d, want %d", c.name, n, got, want)
			}
			_, err := Inspect(data)
			switch {
			case n == c.admitted && err != nil:
				t.Errorf("Inspect(%s, %d scans) = %v, want it admitted", c.name, n, err)
			case n > c.admitted && !errors.Is(err, ErrImageTooCostly):
				t.Errorf("Inspect(%s, %d scans) = %v, want ErrImageTooCostly", c.name, n, err)
			}
		}
	}
}

// image/jpeg reads past extraneous bytes between a JPEG's segments, as libjpeg
// does — anything before an 0xFF, an 0xFF00 pair, fill bytes, a restart marker
// where no scan expects one — and decodes every scan after them. The walks
// that count the scans read past them the same way (jpegNextSegment), so no
// such byte hides a scan from the cap: the over-cap file with them before its
// frame header, before its Huffman tables, before its first scan or before
// every scan is decoded whole by image/jpeg and still refused from its header,
// with the same count as the plain file. (Straight after the start of the
// image they would not get that far: Inspect takes a file for a JPEG only when
// an 0xFF follows it.) One zero byte before the first scan once let the walk
// stop there, count nothing, and admit a file that then decoded for the whole
// of its deadline.
func TestJPEGScanCountReadsPastExtraneousBytes(t *testing.T) {
	const w, h = 1000, 1000
	plain := scanBombJPEG(w, h, maxJPEGScanRevisits)
	f, _ := readJPEGFrame(plain)
	want := jpegScanBlockVisits(plain, f)
	if want <= maxJPEGScanRevisits*f.blocks {
		t.Fatalf("the plain file makes %d visits, within the cap of %d: the test would prove nothing", want, maxJPEGScanRevisits*f.blocks)
	}
	// starts are where segments with marker begin in the plain file. None
	// of its bytes but markers is 0xFF: the tables are ones, the sizes are
	// under 0xFF00 and the scans' data is single EOB runs.
	starts := func(marker byte) []int {
		var at []int
		for i := 0; i+1 < len(plain); i++ {
			if plain[i] == 0xFF && plain[i+1] == marker {
				at = append(at, i)
			}
		}
		if len(at) == 0 {
			t.Fatalf("no 0xFF%02X in the plain file", marker)
		}
		return at
	}
	scans := starts(0xDA)
	places := []struct {
		name string
		at   []int
	}{
		{"before the frame header", starts(0xC2)},
		{"before the Huffman tables", starts(0xC4)[:1]},
		{"before the first scan", scans[:1]},
		{"before every scan", scans},
	}
	junks := map[string][]byte{
		"a zero byte":              {0x00},
		"three bytes":              {0x12, 0x34, 0x56},
		"a stuffed 0xFF00":         {0xFF, 0x00},
		"a restart marker":         {0xFF, 0xD3},
		"bytes, 0xFF00 and a 0x7F": {0x00, 0xFF, 0x00, 0x7F},
	}
	for _, place := range places {
		for jname, junk := range junks {
			name := jname + " " + place.name
			var data []byte
			last := 0
			for _, at := range place.at {
				data = append(append(data, plain[last:at]...), junk...)
				last = at
			}
			data = append(data, plain[last:]...)
			// The decoder reads through to the end of the image, every scan
			// on the way: it returns a picture only once it reaches 0xFFD9,
			// which comes after the last of them.
			if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
				t.Fatalf("%s: image/jpeg refuses the file (%v): the bytes are not ones it reads past", name, err)
			}
			g, framed := readJPEGFrame(data)
			if !framed || g.blocks != f.blocks {
				t.Errorf("%s: readJPEGFrame = %d blocks, framed %v; want the plain file's %d", name, g.blocks, framed, f.blocks)
			}
			if got := jpegScanBlockVisits(data, g); got != want {
				t.Errorf("%s: jpegScanBlockVisits = %d, want the plain file's %d", name, got, want)
			}
			if _, err := Inspect(data); !errors.Is(err, ErrImageTooCostly) {
				t.Errorf("%s: Inspect = %v, want ErrImageTooCostly", name, err)
			}
		}
	}
}

// Whether a three-component JPEG is read as RGB, and so converted to a
// picture of four bytes a pixel, is decided the way image/jpeg decides it:
// from its APP0 and Adobe APP14 segments wherever they come before the end
// of the image, after the scans too, the last of each kind counting, and an
// APP0 of five bytes or more that is not JFIF clearing what a JFIF one said.
// Each file here is decoded and the picture it decodes to is the answer.
func TestJPEGColourModelIsReadAsTheDecoderReadsIt(t *testing.T) {
	plain := encodeJPEGImage(t, quadrants(16, 16))
	app0 := func(body string) []byte { return jpegSegment(0xE0, []byte(body)) }
	notJFIF := app0("AVI1\x00\x00\x00\x00")
	cases := []struct {
		name string
		data []byte
	}{
		{"no segments", plain},
		{"Adobe RGB", withSegments(plain, adobeSegment(0))},
		{"Adobe YCbCr", withSegments(plain, adobeSegment(1))},
		{"Adobe RGB after the scans", beforeEOI(plain, adobeSegment(0))},
		{"Adobe RGB, then YCbCr after the scans", beforeEOI(withSegments(plain, adobeSegment(0)), adobeSegment(1))},
		{"Adobe YCbCr, then RGB after the scans", beforeEOI(withSegments(plain, adobeSegment(1)), adobeSegment(0))},
		{"JFIF and Adobe RGB", withSegments(plain, jfifSegment(), adobeSegment(0))},
		{"JFIF, another APP0 and Adobe RGB", withSegments(plain, jfifSegment(), notJFIF, adobeSegment(0))},
		{"JFIF, a short APP0 and Adobe RGB", withSegments(plain, jfifSegment(), app0("AV"), adobeSegment(0))},
		{"another APP0 and Adobe RGB, JFIF after the scans", beforeEOI(withSegments(plain, notJFIF, adobeSegment(0)), jfifSegment())},
		{"JFIF and Adobe RGB, another APP0 after the scans", beforeEOI(withSegments(plain, jfifSegment(), adobeSegment(0)), notJFIF)},
		{"Adobe RGB past the end of the image", append(append([]byte(nil), plain...), adobeSegment(0)...)},
	}
	for _, c := range cases {
		pic, err := jpeg.Decode(bytes.NewReader(c.data))
		if err != nil {
			t.Fatalf("%s: image/jpeg refuses the file: %v", c.name, err)
		}
		_, decodedRGB := pic.(*image.RGBA)
		f, framed := readJPEGFrame(c.data)
		if !framed || f.comps != 3 {
			t.Fatalf("%s: readJPEGFrame = %d components, framed %v", c.name, f.comps, framed)
		}
		if f.rgb() != decodedRGB {
			t.Errorf("%s: read as RGB %v, but image/jpeg decodes it to %T", c.name, f.rgb(), pic)
		}
	}
}
