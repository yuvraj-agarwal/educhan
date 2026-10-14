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
	"image/png"
	"math"
	"math/rand"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
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

// paddedBitstream is a simple lossy WebP with n more bytes at the end of its
// VP8 chunk. Stripping keeps the chunk whole, and the decoder copies all of
// it before it decodes the picture and then ignores what the picture does
// not need, as it would a file hidden there (sanitizeWebP).
func paddedBitstream(t *testing.T, data []byte, n int) []byte {
	t.Helper()
	chunks := webpChunks(t, data)
	if len(chunks) != 1 || chunks[0].id != "VP8 " {
		t.Fatalf("chunks %v, want a simple lossy WebP", chunkIDs(chunks))
	}
	vp8 := chunks[0].payload
	chunks[0].payload = append(vp8[:len(vp8):len(vp8)], make([]byte, n)...)
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
	// A riffChunk is two headers wide, so one per chunk is chunks*40 bytes of
	// slice before any growth doubling. The bound sits well under that and well
	// over the file plus the short id string the walk makes for each chunk.
	limit := int64(len(data)) + chunks*16
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
// a colour index packed two pixels to the byte: the dearest way to decode
// either. cost-411.jpg is made with ImageMagick (-sampling-factor 4x1
// -quality 90), because Pillow does not write 4:1:1. The opaque lossy WebP
// carries 8 MiB more at the end of its bitstream (paddedBitstream), so the
// two copies of the file the model charges for are megabytes too. Those
// marked "o6" carry an EXIF orientation of 6, so they are turned on their
// side too, and "o3" one of 3, turned around.
func costFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	turned := func(jpg []byte, o uint16) []byte {
		return withSegments(jpg, exifSegment(orientationTIFF(binary.LittleEndian, o)))
	}
	sideways := func(jpg []byte) []byte { return turned(jpg, 6) }
	const w, h = 2400, 1800
	const large, tall = 3200, 2400
	const small, short = 1200, 900
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
		"PNG palette":                rawPNG(t, large, tall, 3, 8, false, false),
		"PNG RGB":                    rawPNG(t, w, h, 2, 8, false, false),
		"PNG grey with transparency": rawPNG(t, w, h, 0, 8, false, true),
		"PNG RGBA interlaced":        rawPNG(t, w, h, 6, 8, true, false),
		"PNG grey 16-bit interlaced": rawPNG(t, w, h, 0, 16, true, false),
		"PNG RGB 16-bit interlaced":  rawPNG(t, small, short, 2, 16, true, false),
		"PNG RGBA 16-bit o6": withPNGChunks(rawPNG(t, small, short, 6, 16, false, false),
			pngChunk("eXIf", orientationTIFF(binary.BigEndian, 6))),
		"GIF of four frames":           gifOfFrames(t, w, h, 4),
		"GIF of two interlaced frames": interlaced(t, gifOfFrames(t, large, tall, 2)),
		"WebP lossy":                   paddedBitstream(t, readTestdata(t, "cost-lossy.webp"), 8<<20),
		"WebP lossy with alpha":        readTestdata(t, "cost-alpha.webp"),
		"WebP lossless":                readTestdata(t, "cost-lossless.webp"),
		"WebP of many chunks":          manyChunkWebP(t, readTestdata(t, "webp-swatches.webp"), 200000),
	}
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
func peakLive(t *testing.T, name string, work func() error) int64 {
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
func leastPeak(t *testing.T, name string, want int64, work func() error) int64 {
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
// writes.
func TestProcessingCostCoversWhatIsHeld(t *testing.T) {
	for name, data := range costFixtures(t) {
		in, err := inspect(data)
		if err != nil {
			t.Fatalf("%s: inspect: %v", name, err)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: DecodeConfig: %v", name, err)
		}

		decoding := peakOf(decodingPhases(in.format, data, cfg, in.frames)...) + processingSlack
		decoded := leastPeak(t, name, decoding, func() error {
			unstored := in
			unstored.limit = 0
			if _, err := sanitize(unstored, data); !errors.Is(err, ErrStoredTooLarge) {
				return fmt.Errorf("sanitize with nowhere to store = %v, want ErrStoredTooLarge", err)
			}
			return nil
		})
		whole := leastPeak(t, name, in.cost, func() error {
			_, err := Process(context.Background(), data)
			return err
		})

		mib := func(n int64) float64 { return float64(n) / (1 << 20) }
		t.Logf("%-28s %4d×%-4d decoding %5.1f MiB of %5.1f, all of it %5.1f MiB of %5.1f",
			name, cfg.Width, cfg.Height, mib(decoded), mib(decoding), mib(whole), mib(in.cost))
		if decoded > decoding {
			t.Errorf("%s: held %d bytes live before encoding, over the estimate of its decoding phases, %d", name, decoded, decoding)
		}
		if whole > in.cost {
			t.Errorf("%s: held %d bytes live, over its estimate of %d", name, whole, in.cost)
		}
	}
}

// The thumbnail scaler's scratch is no more than thumbScratch, for a picture
// wider than it is tall and for one taller than it is wide: the scaler works
// on a picture as wide as the thumbnail and as tall as the source, so it is
// the shorter side that counts. In the cost of the whole it sits on top of a
// bound for the encoder's output that is far above what a flat picture
// needs, which would hide a scratch smaller than the scaler's.
func TestThumbScratchCoversTheScaler(t *testing.T) {
	for _, size := range []image.Point{{2400, 1800}, {1800, 2400}} {
		pic := image.NewYCbCr(image.Rect(0, 0, size.X, size.Y), image.YCbCrSubsampleRatio420)
		name := fmt.Sprintf("%dx%d", size.X, size.Y)
		want := thumbScratch(size.X, size.Y) + processingSlack
		held := leastPeak(t, name, want, func() error {
			_, err := makeThumbnail(pic, pic.Rect, FormatJPEG)
			return err
		})
		t.Logf("%s: thumbnail held %.1f MiB of %.1f", name, float64(held)/(1<<20), float64(want)/(1<<20))
		if held > want {
			t.Errorf("%s: making the thumbnail held %d bytes live, over thumbScratch and processingSlack, %d", name, held, want)
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
// past the header is read before decoding.
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
	if head[0] == 0xFF { // the start of a scan, where image/jpeg stops reading a header
		out = append(out, 0xFF, 0xDA, 0x00, 0x02)
	}
	return append(out, make([]byte, max(size-len(out), 0))...)
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

// At the largest resolution allowed, how a file is encoded decides whether it
// can be processed within the budget, and Inspect says so from the header,
// before anything is decoded or a cooldown is charged. These are the lists
// the README gives. What is accepted is accepted at its dearest: a square
// picture, whose thumbnail scaler needs the most scratch, from an upload of
// the default 8 MiB limit, whose stored file may be the largest. What is
// refused is refused at its cheapest: a wide picture from a small upload. At
// a quarter of the size, everything is accepted at its dearest.
func TestCostlyEncodingsAreRefusedFromTheirHeader(t *testing.T) {
	type kind struct {
		name string
		head func(w, h int) []byte
		o    uint16
	}
	jpg := func(marker byte, samplings ...byte) func(w, h int) []byte {
		return func(w, h int) []byte { return jpegHeader(marker, w, h, samplings...) }
	}
	rgbJPEG := func(marker byte) func(w, h int) []byte {
		return func(w, h int) []byte { // an Adobe segment with transform 0 instead of JFIF
			j := jpegHeader(marker, w, h, 0x11, 0x11, 0x11)
			return withSegments(append([]byte{0xFF, 0xD8}, j[2+18:]...), jpegSegment(0xEE, []byte("Adobe\x00\x64\x00\x00\x00\x00\x00")))
		}
	}
	pngOf := func(colour, depth byte, interlaced bool) func(w, h int) []byte {
		return func(w, h int) []byte { return pngHeader(w, h, colour, depth, interlaced) }
	}
	// A grey (0) or truecolour (2) PNG with a tRNS chunk before its pixels
	// decodes to NRGBA(64), four or eight bytes a pixel, which pngBytesPerPixel
	// reads from the chunk. headerUpload does not add one, so it goes on here.
	pngTRNS := func(colour, depth byte, interlaced bool) func(w, h int) []byte {
		return func(w, h int) []byte {
			return append(pngHeader(w, h, colour, depth, interlaced),
				pngChunk("tRNS", make([]byte, map[byte]int{0: 2, 2: 6}[colour]))...)
		}
	}
	webpOf := func(k string) func(w, h int) []byte {
		return func(w, h int) []byte { return webpHeader(k, w, h) }
	}
	var accepted, refused []kind
	for _, o := range []uint16{1, 6} {
		accepted = append(accepted,
			kind{"baseline 4:2:0 JPEG", jpg(0xC0, 0x22, 0x11, 0x11), o},
			kind{"baseline 4:2:2 JPEG", jpg(0xC0, 0x21, 0x11, 0x11), o},
			kind{"baseline 4:4:4 JPEG", jpg(0xC0, 0x11, 0x11, 0x11), o},
			kind{"baseline 4:1:1 JPEG", jpg(0xC0, 0x41, 0x11, 0x11), o},
			kind{"baseline 4:4:0 JPEG", jpg(0xC0, 0x12, 0x11, 0x11), o},
			kind{"baseline 4:1:0 JPEG", jpg(0xC0, 0x42, 0x11, 0x11), o},
			kind{"baseline grey JPEG", jpg(0xC0, 0x11), o},
			kind{"progressive 4:2:0 JPEG", jpg(0xC2, 0x22, 0x11, 0x11), o},
			kind{"progressive 4:1:1 JPEG", jpg(0xC2, 0x41, 0x11, 0x11), o},
			kind{"progressive 4:1:0 JPEG", jpg(0xC2, 0x42, 0x11, 0x11), o},
			kind{"progressive grey JPEG", jpg(0xC2, 0x11), o},
			kind{"palette PNG", pngOf(3, 8, false), o},
			kind{"interlaced palette PNG", pngOf(3, 8, true), o},
			kind{"8-bit grey PNG", pngOf(0, 8, false), o},
			kind{"16-bit grey PNG", pngOf(0, 16, false), o},
			kind{"interlaced 16-bit grey PNG", pngOf(0, 16, true), o},
			kind{"8-bit RGB PNG", pngOf(2, 8, false), o},
			kind{"8-bit RGBA PNG", pngOf(6, 8, false), o},
			kind{"8-bit grey+alpha PNG", pngOf(4, 8, false), o},
		)
		refused = append(refused,
			kind{"progressive 4:4:4 JPEG", jpg(0xC2, 0x11, 0x11, 0x11), o},
			kind{"progressive CMYK JPEG", jpg(0xC2, 0x11, 0x11, 0x11, 0x11), o},
			kind{"progressive RGB JPEG", rgbJPEG(0xC2), o},
			kind{"interlaced 16-bit RGB PNG", pngOf(2, 16, true), o},
			kind{"interlaced 16-bit RGBA PNG", pngOf(6, 16, true), o},
			kind{"interlaced 16-bit grey+tRNS PNG", pngTRNS(0, 16, true), o},
		)
	}
	accepted = append(accepted,
		kind{"baseline CMYK JPEG", jpg(0xC0, 0x11, 0x11, 0x11, 0x11), 1},
		kind{"baseline RGB JPEG", rgbJPEG(0xC0), 1},
		kind{"progressive 4:2:2 JPEG", jpg(0xC2, 0x21, 0x11, 0x11), 1},
		kind{"progressive 4:4:0 JPEG", jpg(0xC2, 0x12, 0x11, 0x11), 1},
		kind{"interlaced 8-bit RGB PNG", pngOf(2, 8, true), 1},
		kind{"interlaced 8-bit RGBA PNG", pngOf(6, 8, true), 1},
		kind{"interlaced 8-bit grey+alpha PNG", pngOf(4, 8, true), 1},
		kind{"lossy WebP", webpOf("lossy"), 1},
	)
	refused = append(refused, kind{"lossy WebP with transparency", webpOf("alpha"), 1})
	refused = append(refused,
		kind{"baseline CMYK JPEG", jpg(0xC0, 0x11, 0x11, 0x11, 0x11), 6},
		kind{"baseline RGB JPEG", rgbJPEG(0xC0), 6},
		kind{"progressive 4:2:2 JPEG", jpg(0xC2, 0x21, 0x11, 0x11), 6},
		kind{"progressive 4:4:0 JPEG", jpg(0xC2, 0x12, 0x11, 0x11), 6},
		kind{"interlaced 8-bit RGB PNG", pngOf(2, 8, true), 6},
		kind{"interlaced 8-bit RGBA PNG", pngOf(6, 8, true), 6},
		kind{"interlaced 8-bit grey+alpha PNG", pngOf(4, 8, true), 6},
		kind{"16-bit RGB PNG", pngOf(2, 16, false), 6},
		kind{"16-bit RGBA PNG", pngOf(6, 16, false), 6},
		kind{"16-bit grey+tRNS PNG", pngTRNS(0, 16, false), 6},
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
		check(k, 4*side, side/4, small, ErrImageTooLarge)
		check(k, side/2, side/2, large, nil)
	}
	// The "either" kinds: a 16-bit colour PNG that is neither interlaced nor
	// turned (eight bytes a pixel) is refused at a square, its dearest shape,
	// but stored when wide. A grey PNG with a transparent colour counts as a
	// colour PNG here: with the key applied it decodes to NRGBA64 the same way.
	for _, k := range []kind{
		{"16-bit RGB PNG", pngOf(2, 16, false), 1},
		{"16-bit RGBA PNG", pngOf(6, 16, false), 1},
		{"16-bit grey+tRNS PNG", pngTRNS(0, 16, false), 1},
	} {
		check(k, side, side, large, ErrImageTooLarge)
		check(k, 4*side, side/4, small, nil)
	}
	// One pixel more than the cap is refused whatever it is.
	check(kind{"baseline 4:2:0 JPEG", jpg(0xC0, 0x22, 0x11, 0x11), 1}, side+1, side, small, ErrImageTooLarge)

	// Lossless WebP and a VP8L-compressed alpha plane are the one case refused
	// by the size of the upload rather than its resolution: golang.org/x/image's
	// vp8l decoder sizes its Huffman groups and tree nodes from the bitstream,
	// not from any header field, so a file over about a third of a megabyte is
	// refused whatever its resolution, and a small one accepted (vp8lMemory).
	for _, k := range []string{"lossless", "calpha"} {
		check(kind{k + " WebP", webpOf(k), 1}, 256, 256, small, nil)
		check(kind{k + " WebP, large upload", webpOf(k), 1}, 256, 256, large, ErrImageTooLarge)
	}
	// An uncompressed alpha plane needs no such limit: it is not decoded
	// through vp8l, so a large upload of one is accepted at a small size.
	check(kind{"lossy WebP, uncompressed alpha", webpOf("alpha"), 1}, 256, 256, large, nil)
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

	b := newStoredBuffer(1, 10_000)
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
	if lossless, alpha := webpPicture(upload); lossless || !alpha {
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
