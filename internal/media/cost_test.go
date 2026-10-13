package media

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"math"
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
// write itself. The pixels are a smooth ramp, so the file stays small.
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
				for k := 0; k < bpp; k++ {
					v := byte((x + y) >> 4)
					if colour == 3 {
						v &= 1
					}
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

// costFixtures are one file of each kind processingCost models, 1200×900
// pictures of four flat quadrants unless they say otherwise. The cost-*.jpg
// and cost-*.webp files in testdata are made with Pillow, because Go encodes
// neither progressive nor CMYK JPEGs nor any WebP, except cost-411.jpg, which
// is made with ImageMagick (-sampling-factor 4x1), because Pillow does not
// write 4:1:1 either. Those marked "o6" carry an EXIF orientation of 6, so
// they are turned on their side too, and "o3" one of 3, turned around.
func costFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	turned := func(jpg []byte, o uint16) []byte {
		return withSegments(jpg, exifSegment(orientationTIFF(binary.LittleEndian, o)))
	}
	sideways := func(jpg []byte) []byte { return turned(jpg, 6) }
	const w, h = 1200, 900
	return map[string][]byte{
		"baseline 4:2:0 o6":          sideways(encodeJPEGImage(t, quadrants(w, h))),
		"baseline 4:2:0 odd o3":      turned(encodeJPEGImage(t, quadrants(w+1, h+1)), 3),
		"baseline 4:1:1 o6":          sideways(readTestdata(t, "cost-411.jpg")),
		"progressive 4:2:0 o6":       readTestdata(t, "cost-prog420.jpg"),
		"progressive 4:4:4 o6":       readTestdata(t, "cost-prog444.jpg"),
		"progressive grey o6":        readTestdata(t, "cost-proggray.jpg"),
		"progressive CMYK o6":        readTestdata(t, "cost-progcmyk.jpg"),
		"baseline CMYK":              readTestdata(t, "cost-cmyk.jpg"),
		"RGB o6":                     readTestdata(t, "cost-rgb.jpg"),
		"PNG palette":                rawPNG(t, w, h, 3, 8, false, false),
		"PNG RGB":                    rawPNG(t, w, h, 2, 8, false, false),
		"PNG grey with transparency": rawPNG(t, w, h, 0, 8, false, true),
		"PNG RGBA interlaced":        rawPNG(t, w, h, 6, 8, true, false),
		"PNG grey 16-bit interlaced": rawPNG(t, w, h, 0, 16, true, false),
		"PNG RGB 16-bit interlaced":  rawPNG(t, w, h, 2, 16, true, false),
		"PNG RGBA 16-bit o6": withPNGChunks(rawPNG(t, w, h, 6, 16, false, false),
			pngChunk("eXIf", orientationTIFF(binary.BigEndian, 6))),
		"GIF of four frames":    gifOfFrames(t, w, h, 4),
		"WebP lossy":            readTestdata(t, "cost-lossy.webp"),
		"WebP lossy with alpha": readTestdata(t, "cost-alpha.webp"),
		"WebP lossless":         readTestdata(t, "cost-lossless.webp"),
	}
}

/* Measurement -------------------------------------------------------------- */

// liveHeap is the heap the collector last marked live.
func liveHeap() int64 {
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	return int64(s[0].Value.Uint64())
}

// peakLive processes data with the collector running continually and returns
// the most the heap held live, as the collector marked it, above what it held
// before. That is the quantity processingCost estimates: garbage a phase
// leaves behind is not in it, because a memory limit makes the collector take
// it back.
//
// What a phase allocates and drops while a collection runs is marked live
// too, so a file whose processing is mostly such churn measures as much of
// it as the collector happened to overlap. A GIF of thousands of tiny frames
// is one: image/gif allocates tens of kilobytes of LZW tables for each frame
// and keeps a few. What it keeps is measured exactly instead, by
// TestGIFFrameOverheadCoversWhatDecodingKeeps.
func peakLive(t *testing.T, name string, data []byte) int64 {
	t.Helper()
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	runtime.GC()
	base := liveHeap()

	var (
		peak int64
		wg   sync.WaitGroup
		stop = make(chan struct{})
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			peak = max(peak, liveHeap())
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Microsecond):
			}
		}
	}()
	_, err := Process(context.Background(), data)
	runtime.GC() // marks whatever the last phase still held
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("%s: Process: %v", name, err)
	}
	return peak - base
}

/* Tests -------------------------------------------------------------------- */

// What an upload holds while it is processed stays inside what its header
// said it would, for every kind of file the estimate models: progressive
// coefficients, CMYK and RGB conversions, turning upright, interlaced and
// 16-bit PNGs, transparency, every frame of a GIF, and WebP's three kinds.
func TestProcessingCostCoversWhatIsHeld(t *testing.T) {
	for name, data := range costFixtures(t) {
		in, err := inspect(data)
		if err != nil {
			t.Fatalf("%s: inspect: %v", name, err)
		}
		cost := in.cost
		live := peakLive(t, name, data)
		t.Logf("%-28s estimate %6.1f MiB, held %6.1f MiB", name, float64(cost)/(1<<20), float64(live)/(1<<20))
		if live > cost {
			t.Errorf("%s: held %d bytes live, over its estimate of %d", name, live, cost)
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

// At the largest resolution allowed, how a file is encoded decides whether
// it can be processed within the budget, and Inspect says so from the header,
// before anything is decoded or a cooldown is charged.
func TestCostlyEncodingsAreRefusedFromTheirHeader(t *testing.T) {
	const w, h = 6000, 4000 // MaxPixels
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"baseline 4:2:0 JPEG", jpegHeader(0xC0, w, h, 0x22, 0x11, 0x11), nil},
		{"baseline 4:4:4 JPEG", jpegHeader(0xC0, w, h, 0x11, 0x11, 0x11), nil},
		{"progressive 4:2:0 JPEG", jpegHeader(0xC2, w, h, 0x22, 0x11, 0x11), nil},
		{"progressive 4:4:4 JPEG", jpegHeader(0xC2, w, h, 0x11, 0x11, 0x11), ErrImageTooLarge},
		{"progressive CMYK JPEG", jpegHeader(0xC2, w, h, 0x11, 0x11, 0x11, 0x11), ErrImageTooLarge},
		{"progressive 4:4:4 JPEG at 12 MP", jpegHeader(0xC2, 4000, 3000, 0x11, 0x11, 0x11), nil},
		{"8-bit RGB PNG", pngHeader(w, h, 2, 8, false), nil},
		{"8-bit RGBA interlaced PNG", pngHeader(w, h, 6, 8, true), nil},
		{"16-bit RGB interlaced PNG", pngHeader(w, h, 2, 16, true), ErrImageTooLarge},
		{"16-bit RGB interlaced PNG at 12 MP", pngHeader(4000, 3000, 2, 16, true), nil},
	}
	for _, tc := range cases {
		if _, err := Inspect(tc.data); !errors.Is(err, tc.want) {
			t.Errorf("Inspect(%s) = %v, want %v", tc.name, err, tc.want)
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
	if _, err := Process(context.Background(), upload); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("Process = %v, want ErrImageTooLarge", err)
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
	if _, err := b.Write(make([]byte, 1001)); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("a write past the limit = %v, want ErrImageTooLarge", err)
	}
	if _, err := b.Write(make([]byte, 1000)); err != nil || len(b.buf) != 10_000 {
		t.Errorf("a write up to the limit = %v with %d bytes, want it taken", err, len(b.buf))
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
