package media

import (
	"encoding/binary"
	"image"
)

// Processing an upload holds a lot more than its pixels, and how much more
// depends on how the file is encoded rather than on its resolution alone. A
// 25-megapixel baseline JPEG decodes to 37.5 MB; the same picture saved
// progressive with full-resolution colour keeps every DCT coefficient of the
// file, four bytes each, until the last scan is read, and holds over 350 MB;
// CMYK and 16-bit pictures are larger again. A pixel cap cannot tell these
// apart, so each upload's cost is estimated from its header (processingCost)
// before anything is decoded, and that estimate is what it takes from the
// processing budget while it is decoded and re-encoded.
//
// The estimate is of live memory, taken phase by phase as the file is
// decoded, converted, turned upright, re-encoded and thumbnailed (peakOf).
// Garbage is not counted once the collector can take it back, which a memory
// limit (GOMEMLIMIT) makes it do before the process grows; what it cannot
// take back is what the estimate covers, including a phase's scratch for as
// long as a collection that began during that phase is still running. Small
// garbage a phase makes and drops as it goes (row buffers, a GIF's LZW
// tables) is left to the collector's pacing, beyond processingSlack.
//
// TestProcessingCostCoversWhatIsHeld measures a file of each kind the model
// distinguishes against the estimate for the whole of its processing and
// against its decoding phases alone (decodingPhases), at sizes where each
// term charged for decoding comes to megabytes; TestThumbScratchCoversTheScaler
// measures the thumbnail's scratch, and
// TestGIFFrameOverheadCoversWhatDecodingKeeps what image/gif keeps of each
// frame. A term dropped is caught when it is larger than processingSlack — as
// each is in some file — but a partial under-charge smaller than the slack it
// absorbs and does not: the turn term, only a couple of megabytes above the
// slack in the largest file that turns, can be cut by more than half unseen.
// So the tests guard against a term left out, not against every under-estimate.
// Even a term left out goes unnoticed where another term charged for the same
// moment is at least as large in every file they measure; a few are, the alpha
// plane and the NRGBA copy of a transparent lossy WebP among them.

// processingBudget is the memory every upload being processed at once may
// hold between them, by their estimates, from when it starts to be decoded
// until both its renditions are stored (Save). An upload whose estimate alone
// is over it is refused from its header (ErrImageTooLarge); the others wait
// for their share, for as long as their post's deadline allows
// (processingAllowance). The uploads' own bytes are not in the estimates:
// uploadBudget (budget.go) counts them, from before they are read. Between
// them the two budgets hold at most 320 MiB, whatever number of requests an
// instance is let take at once. The rest of a process is not budgeted: idle,
// about 40 MiB. Eighty 8 MiB uploads sent at once to a server run with a
// GOMEMLIMIT of 400 MiB peaked at 330 MiB resident, which fits an instance of
// 512 MiB with that limit; the limit is the deployment's to set.
const processingBudget int64 = 256 << 20

// processingSlack is what each estimate adds for what it does not itemise:
// decoders' tables and row buffers, compressors' windows, the thumbnail's own
// pixels and encoding, and what a phase allocates while a collection runs.
const processingSlack int64 = 4 << 20

// gifFrameOverhead is what image/gif keeps for each frame of a GIF besides
// its pixels: the frame's header, its palette (a local colour table, or the
// copy of the global one DecodeAll makes for a frame with a transparent
// colour: 256 interface values and the colours they hold, 5 KiB), its place
// in three slices, and the up to 8 KiB the allocator rounds its pixels up
// by. A frame can be a handful of bytes of file, so without it a GIF of a
// few megabytes could decode to gigabytes of frames of no pixels at all.
const gifFrameOverhead int64 = 16 << 10

// phase is one step of processing an upload. It allocates first as it
// begins, goes on to allocate the rest of alloc (which includes first) on
// top of what the step before it left held, and leaves after held once it is
// done.
type phase struct{ first, alloc, after int64 }

// peakOf is the most a run of phases holds live at once. Within a phase that
// is what it started with and everything it allocates. Across a boundary it
// is more: a collection treats everything that was reachable when it began as
// live until it ends, so a collection that begins during one phase keeps that
// phase's scratch alive into the next. The next phase's first allocation is
// counted on top of the earlier phase's peak for that reason; a collection
// is usually over long before the rest of a phase has been allocated, though
// on a machine short of CPU one can run on further and, until it ends, hold
// more than this counts.
func peakOf(phases ...phase) int64 {
	var worst, held, top int64
	for _, p := range phases {
		worst = max(worst, top+p.first, held+p.alloc)
		top, held = held+p.alloc, p.after
	}
	return worst
}

// processingCost estimates the most an upload holds while it is processed,
// from its header: what the decoder allocates, what converting it and turning
// it upright copy, the encoder's output (bounded by storedLimit) and the
// thumbnail scaler's scratch. cfg is the header's, already checked against
// MaxPixels; frames is a GIF's frames (gifFramePixels).
func processingCost(format Format, data []byte, cfg image.Config, frames gifFrames) int64 {
	phases := decodingPhases(format, data, cfg, frames)
	if phases == nil {
		return processingBudget + 1
	}
	held := phases[len(phases)-1].after
	upload := int64(len(data))
	limit := storedLimit(format, data, cfg.Width, cfg.Height)
	scratch := thumbScratch(cfg.Width, cfg.Height)
	phases = append(phases,
		// The encoder writes into a buffer the size of the upload, which
		// doubles up to limit, holding the old and the new while it grows
		// (storedBuffer). What it wrote is kept for the thumbnail.
		phase{first: min(upload, limit), alloc: 2 * limit, after: held + limit},
		// The scaler allocates its scratch whole as it starts. What the
		// encoder wrote is kept after it, until it has been stored (Save
		// holds the budget until then), but nothing larger is made for
		// that: the local store writes the file from it as it is, and the
		// Cloud Storage writer is given no buffer of its own (GCSStore.Put).
		phase{first: scratch, alloc: scratch, after: limit},
	)
	return peakOf(phases...) + processingSlack
}

// decodingPhases are the phases of processing an upload before it is
// encoded again: decoding it, converting it and turning it upright, which is
// everything sanitize does before it writes the first byte of the stored
// file. They are nil for a format it does not know.
func decodingPhases(format Format, data []byte, cfg image.Config, frames gifFrames) []phase {
	w, h := int64(cfg.Width), int64(cfg.Height)
	px := w * h
	switch format.Name {
	case FormatJPEG.Name:
		return jpegPhases(data, w, h)
	case FormatPNG.Name:
		d := pngBytesPerPixel(data) * px
		decode := phase{first: d, alloc: d, after: d}
		if pngInterlaced(data) {
			// Each of the seven passes is decoded into a picture of its own
			// and merged into the whole; the last pass is half of it.
			decode.alloc = 2 * d
		}
		phases := []phase{decode}
		if pngOrientation(data) >= 2 {
			// orient copies the picture into another of the same kind.
			phases = append(phases, phase{first: d, alloc: d, after: d})
		}
		return phases
	case FormatGIF.Name:
		// Every frame is decoded and kept, a byte a pixel and its overhead,
		// until all of them are encoded again. An interlaced frame is
		// decoded once more while its rows are put in order.
		kept := frames.pixels + frames.count*gifFrameOverhead
		return []phase{{alloc: kept + px, after: kept}}
	case FormatWebP.Name:
		return webpPhases(data, w, h)
	}
	return nil
}

// storedLimit is the most a stored original may take: four times the upload,
// plus half a byte a pixel. Re-encoding a JPEG at its own quality and
// subsampling (jpegQuality), a lossy WebP as a JPEG at storedJPEGQuality, or a
// PNG or a GIF in its own format, comes to well inside that. What reaches it
// is a file that compresses in a way Go's encoders cannot follow — a lossless
// WebP of a pattern repeated further apart than deflate's 32 KiB window,
// say, which as a PNG is raw pixels — and storing that would turn a small
// upload into a file many times its size. Such a file is refused
// (ErrStoredTooLarge) rather than stored.
//
// A lossy WebP with transparency is the exception: it is stored as a PNG of
// its decoded pixels, because nothing else Go writes keeps the transparency,
// and a lossy photo stored losslessly is many times the size it came in at
// by design, not by any trick. Its limit is what image/png can take for such
// a picture at worst (pngLimit), so the size it will be stored at is known
// from its header, and a picture too large to store that way is refused
// from it, as too costly to process (processingCost), before any cooldown.
func storedLimit(format Format, data []byte, w, h int) int64 {
	limit := 4*int64(len(data)) + int64(w)*int64(h)/2 + 64<<10
	if format == FormatWebP {
		if lossless, alpha, _ := webpPicture(data); !lossless && alpha {
			limit = max(limit, pngLimit(w, h, 4))
		}
	}
	return limit
}

// pngLimit is the most image/png writes for a w×h picture of bpp bytes a
// pixel, however badly it compresses: its rows with a filter byte each, which
// deflate stores with 5 bytes for every block of up to 32 KiB it cannot
// compress, in IDAT chunks of 12 bytes for every 32 KiB, a 1024th more than
// the rows in all, and its headers.
func pngLimit(w, h, bpp int) int64 {
	rows := int64(h) * (1 + int64(bpp)*int64(w))
	return rows + rows/1024 + 64<<10
}

// thumbScratch bounds the scratch buffer golang.org/x/image/draw's kernel
// scaler allocates to make a thumbnail: four float64s for every pixel of a
// picture as wide as the thumbnail and as tall as the source. That is at most
// 32 bytes × ThumbMaxDim × the source's shorter side.
func thumbScratch(w, h int) int64 {
	return 32 * ThumbMaxDim * int64(min(w, h))
}

// jpegPhases models image/jpeg and what sanitizeJPEG does after it. The
// decoder allocates the picture at its coded size (whole MCUs, 64 bytes a
// block); a progressive file also keeps every coefficient, 256 bytes a block,
// until the last scan; and a CMYK or RGB file is converted to a picture of 4
// bytes a pixel while the coded one is still held. (sanitizeJPEG turns CMYK
// into RGBA where it lies.) orient copies the picture when its EXIF
// orientation turns it.
func jpegPhases(data []byte, w, h int64) []phase {
	px := w * h
	f, ok := readJPEGFrame(data)
	if !ok {
		// image.DecodeConfig read a frame header this did not: assume the
		// most expensive kind of file there is.
		f = jpegFrame{progressive: true, comps: 4, blocks: 4 * (px/64 + 1)}
	}
	coded := 64 * f.blocks
	decode := phase{first: coded, alloc: coded, after: coded}
	if f.progressive {
		decode.alloc += 256 * f.blocks
	}
	if f.comps == 4 || (f.comps == 3 && f.rgb()) {
		decode.alloc += 4 * px
		decode.after = 4 * px
	}
	phases := []phase{decode}
	if o := jpegOrientation(data); o >= 2 {
		// orient copies grayscale and RGBA as they are, and YCbCr in its
		// planes, at the picture's size rather than the coded one, unless
		// they cannot be turned exactly that way: then it is 4:4:4.
		turn := 4 * px
		switch {
		case f.comps == 1:
			turn = px
		case f.comps == 3 && !f.rgb():
			turn = coded
			if _, ok := turnedSubsampling(f.subsampling(), o, int(w), int(h)); !ok {
				turn = max(coded, 3*px)
			}
		}
		phases = append(phases, phase{first: turn, alloc: turn, after: turn})
	}
	return phases
}

// jpegFrame is what jpegPhases needs of a JPEG's frame header and the
// application segments around it, and what jpegQuality needs of its
// quantisation tables.
type jpegFrame struct {
	progressive bool
	comps       int
	blocks      int64 // 8×8 blocks the decoder allocates, all components
	ids         [3]byte
	// hRatio and vRatio are how many luma samples across and down share a
	// chroma sample: the first component's sampling factors over the
	// second's, as image/jpeg works them out.
	hRatio, vRatio int64
	jfif           bool
	adobe          bool // an Adobe APP14 segment with a transform
	transform      byte
	// tq is the quantisation table each component names.
	tq [4]byte
	// quant is the quantisation tables, in zig-zag order, as they stand
	// when the first scan starts, and defined says which have been; tables
	// says whether the walk got that far, so they are all there are.
	quant   [4][64]uint16
	defined [4]bool
	tables  bool
}

// subsampling is the YCbCr subsampling image/jpeg decodes a three-component
// frame to, from its sampling factors, or -1 for one it refuses.
func (f jpegFrame) subsampling() image.YCbCrSubsampleRatio {
	switch f.hRatio<<4 | f.vRatio {
	case 0x11:
		return image.YCbCrSubsampleRatio444
	case 0x12:
		return image.YCbCrSubsampleRatio440
	case 0x21:
		return image.YCbCrSubsampleRatio422
	case 0x22:
		return image.YCbCrSubsampleRatio420
	case 0x41:
		return image.YCbCrSubsampleRatio411
	case 0x42:
		return image.YCbCrSubsampleRatio410
	}
	return -1
}

// rgb reports whether image/jpeg reads a three-component file as RGB rather
// than YCbCr, by the same rule it uses (decoder.isRGB).
func (f jpegFrame) rgb() bool {
	if f.jfif {
		return false
	}
	if f.adobe && f.transform == 0 {
		return true
	}
	return f.ids == [3]byte{'R', 'G', 'B'}
}

// readJPEGFrame walks a JPEG's segments up to its first scan, as
// jpegOrientation does: in place, every length checked before it is used,
// nothing allocated. It reports whether it found a frame header before
// anything stopped it. It counts the blocks image/jpeg will allocate: whole
// MCUs of the first component's sampling, each holding h×v blocks of every
// component, and a single component's samples counted 1×1 whatever it
// claims, as the decoder does. It reads the quantisation tables defined
// before the first scan; a file that redefines one between its scans is the
// uploader's own oddity, and costs only the quality its picture is stored at.
func readJPEGFrame(data []byte) (jpegFrame, bool) {
	var f jpegFrame
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return f, false
	}
	framed := false
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xFF {
			return f, framed
		}
		marker := data[i+1]
		switch {
		case marker == 0xFF:
			i++
			continue
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD8):
			i += 2
			continue
		case marker == 0xDA:
			f.tables = framed
			return f, framed
		case marker == 0xD9:
			return f, framed
		}
		length := int(data[i+2])<<8 | int(data[i+3])
		if length < 2 || length > len(data)-i-2 {
			return f, framed
		}
		segment := data[i+4 : i+2+length]
		switch marker {
		case 0xE0:
			if len(segment) >= 5 && string(segment[:5]) == "JFIF\x00" {
				f.jfif = true
			}
		case 0xEE:
			if len(segment) >= 12 && string(segment[:5]) == "Adobe" {
				f.adobe, f.transform = true, segment[11]
			}
		case 0xDB:
			if !f.readDQT(segment) {
				return f, framed
			}
		case 0xC0, 0xC1, 0xC2:
			if framed {
				// A second frame header, which image/jpeg refuses.
				return f, framed
			}
			if !f.read(segment, marker == 0xC2) {
				return f, false
			}
			framed = true
		}
		i += 2 + length
	}
	return f, framed
}

// readDQT records the tables a DQT segment defines, as image/jpeg reads them:
// one after another, each a byte holding its precision (8 or 16 bits) and its
// number (0–3), and then its 64 values in zig-zag order.
func (f *jpegFrame) readDQT(segment []byte) bool {
	for len(segment) > 0 {
		pq, tq := segment[0]>>4, segment[0]&0x0F
		if pq > 1 || tq > 3 || len(segment) < 1+64<<pq {
			return false
		}
		for k := range f.quant[tq] {
			if pq == 0 {
				f.quant[tq][k] = uint16(segment[1+k])
			} else {
				f.quant[tq][k] = binary.BigEndian.Uint16(segment[1+2*k:])
			}
		}
		f.defined[tq] = true
		segment = segment[1+64<<pq:]
	}
	return true
}

// read fills in the frame from a SOF segment's body.
func (f *jpegFrame) read(sof []byte, progressive bool) bool {
	if len(sof) < 6 {
		return false
	}
	height := int64(binary.BigEndian.Uint16(sof[1:3]))
	width := int64(binary.BigEndian.Uint16(sof[3:5]))
	n := int(sof[5])
	if n != 1 && n != 3 && n != 4 || len(sof) < 6+3*n {
		return false
	}
	f.progressive, f.comps = progressive, n
	h0, v0 := int64(1), int64(1)
	var units int64
	for k := 0; k < n; k++ {
		c := sof[6+3*k : 9+3*k]
		h, v := int64(c[1]>>4), int64(c[1]&0x0F)
		if h < 1 || v < 1 {
			return false
		}
		if n == 1 {
			h, v = 1, 1
		}
		if k == 0 {
			h0, v0 = h, v
		}
		if k < 3 {
			f.ids[k] = c[0]
		}
		f.tq[k] = c[2]
		if k == 1 {
			f.hRatio, f.vRatio = h0/h, v0/v
		}
		units += h * v
	}
	mxx := (width + 8*h0 - 1) / (8 * h0)
	myy := (height + 8*v0 - 1) / (8 * v0)
	f.blocks = mxx * myy * units
	return true
}

// pngBytesPerPixel is what image/png allocates a pixel for a PNG of the
// colour type and bit depth its IHDR gives: grey and palette pictures a byte
// (two at 16 bits), and colour or anything with transparency 4 bytes (8 at 16
// bits). A tRNS chunk turns grey and truecolour into NRGBA, so the chunks
// before the first IDAT are read for one.
func pngBytesPerPixel(data []byte) int64 {
	if len(data) < 29 {
		return 8
	}
	depth, colour := data[24], data[25]
	wide := int64(1)
	if depth == 16 {
		wide = 2
	}
	switch colour {
	case 3: // palette
		return 1
	case 0: // grey
		if pngHasTRNS(data) {
			return 4 * wide
		}
		return wide
	}
	return 4 * wide // truecolour, grey with alpha, truecolour with alpha
}

// pngHasTRNS reports whether a tRNS chunk comes before the first IDAT, or
// whether the chunk list cannot be followed that far, in which case it
// assumes one.
func pngHasTRNS(data []byte) bool {
	i := 8
	for i+8 <= len(data) {
		n := int64(binary.BigEndian.Uint32(data[i : i+4]))
		switch string(data[i+4 : i+8]) {
		case "tRNS":
			return true
		case "IDAT":
			return false
		}
		if n > int64(len(data)-i-12) {
			return true
		}
		i += 12 + int(n)
	}
	return true
}

func pngInterlaced(data []byte) bool {
	return len(data) < 29 || data[28] != 0
}

// webpPhases models golang.org/x/image/webp and what sanitizeWebP does
// after it, starting from stripWebP's copy of the file. A lossy picture
// decodes to YCbCr at its size in whole 16-pixel macroblocks, 1.5 bytes a
// pixel, from a copy the decoder makes of its compressed partitions, and is
// then converted to NRGBA, 4 bytes a pixel, to take its limited-range colours
// back to full range (lossyWebPToNRGBA) before it is encoded as a JPEG or,
// with transparency, a PNG. A lossless picture decodes to NRGBA, 4
// bytes a pixel, but one written with a colour index of 16 colours or fewer
// is decoded packed first, at least two pixels to every four bytes, and
// unpacked into a second picture: 6 bytes a pixel at most (vp8l's
// inverseColorIndexing). An alpha plane, decoded first, is a byte a pixel,
// but a compressed one is decoded as a lossless picture first and copied out
// of it, which is no more than the lossless picture's own peak.
//
// A lossless bitstream, and a VP8L-compressed alpha plane, also make the
// decoder hold its Huffman groups (vp8lMemory), alive while the pixels are
// decoded, which the pixel terms alone do not cover. They are charged on the
// bytes the vp8l decoder can read, which webpPicture finds: the decoder is
// handed one chunk, never the rest of the file.
func webpPhases(data []byte, w, h int64) []phase {
	px, stripped := w*h, int64(len(data))
	planes := 384 * ((w + 15) / 16) * ((h + 15) / 16)
	strip := phase{first: stripped, alloc: stripped, after: stripped}
	lossy := phase{first: planes + stripped, alloc: planes + stripped, after: planes}
	// The lossy picture is converted from its YCbCr planes to an NRGBA copy
	// (4 bytes a pixel), which is held while it is encoded; the planes are let
	// go once it is made.
	toNRGBA := phase{first: 4 * px, alloc: 4 * px, after: 4 * px}
	lossless, alpha, bitstream := webpPicture(data)
	v := vp8lMemory(bitstream)
	switch {
	case lossless:
		return []phase{strip, {first: v + 6*px, alloc: v + 6*px, after: 4 * px}}
	case alpha:
		lossy.after += px
		return []phase{
			strip,
			{first: v + 6*px, alloc: v + 6*px, after: stripped + px},
			lossy,
			toNRGBA,
		}
	}
	return []phase{strip, lossy, toNRGBA}
}

// vp8lGroupBacking is the largest backing array golang.org/x/image's vp8l
// decoder allocates for its Huffman groups. A lossless (VP8L) bitstream, and
// a VP8L-compressed alpha plane, may carry a meta-image that maps tiles to a
// group index, and the decoder makes one hGroup for every index up to the
// largest the meta-image holds (decodeHuffmanGroups). The index is two bytes,
// so it reaches 65535; an hGroup is five Huffman trees, each a 24-byte slice
// header and a 512-byte look-up table. None of this is in the file's header
// or its resolution: a 1×1 lossless WebP of 160 KiB forces the full
// allocation, and the only bound the file gives is the size of its bitstream.
const vp8lGroupBacking int64 = 65536 * 5 * (24 + 512) // 167.5 MiB

// vp8lBackingPerByte and vp8lNodesPerByte bound, per byte of the bitstream,
// the two allocations the vp8l decoder makes that the header cannot predict:
// the hGroup backing array (vp8lGroupBacking, capped), and the trees' node
// slices (make([]hNode, 1, 2*symbols-1), 8 bytes each). The cheapest hGroup
// is five one-symbol trees, about 2.5 bytes of bitstream for 2680 bytes of
// backing, so 1072. The most node memory a bitstream byte can force is
// bounded by the decoder's alphabets (256, plus up to a 2048-entry colour
// cache for the first tree) and the cheapest encoding of their code lengths
// (about half a bit a symbol through repeat codes, eight node bytes a
// symbol): about 256 node bytes a byte, so 384 leaves a margin.
// TestVP8LHuffmanMemoryIsHeldWithinTheEstimate measures a crafted file at
// each extreme against the estimate these give: 160 KiB of one-symbol groups,
// the most backing a byte can force, which holds about 170 MiB, and 215 KiB of
// full trees, the most nodes, which holds about 43 MiB.
const (
	vp8lBackingPerByte int64 = 1072
	vp8lNodesPerByte   int64 = 384
)

// vp8lMemory bounds the memory the vp8l decoder holds for a bitstream of n
// bytes beyond its decoded pixels: the hGroup backing (capped at the 16-bit
// index ceiling) and the trees' node slices. A file splits its bytes between
// forcing more groups and fuller trees, so charging the maximum of each on
// the whole size over-counts, which is safe. A lossless bitstream or a
// VP8L-compressed alpha plane of more than 224 KiB is refused by this from
// its header, before any cooldown (processingCost, Inspect), and less in a
// larger picture, whose pixels take more of the budget beside it: about
// 205 KiB at a megapixel and 130 KiB at nine, and a little less again in a
// larger upload (TestVP8LHuffmanMemoryIsCharged). x/image's decoder cannot be
// held to less, and no header field says a given file is benign.
func vp8lMemory(n int64) int64 {
	return min(vp8lGroupBacking, vp8lBackingPerByte*n) + vp8lNodesPerByte*n
}

// vp8lAlphaHeader is the VP8L header golang.org/x/image's decoder puts in
// front of a compressed alpha plane (readAlpha), where the ALPH chunk's own
// first byte, its compression method and filter, was.
const vp8lAlphaHeader = 5

// webpPicture reports, from a WebP's top-level chunks, whether its picture is
// lossless, whether it has an alpha plane, and how many bytes of it
// golang.org/x/image's vp8l decoder can read, which is what vp8lMemory is
// charged on: a lossless picture's VP8L chunk; an alpha plane compressed with
// VP8L (method 1 in the low bits of its first byte; method 0 is uncompressed,
// allocates only the plane, and is not read through vp8l), its ALPH chunk
// with the header the decoder puts in front of it; and otherwise nothing. The
// decoder reads each chunk through a reader that ends with the chunk, so the
// rest of the file — a lossy bitstream, a colour profile, EXIF — is never
// part of it. These are the first such chunks, which are the ones stripWebP
// keeps and the decoder takes: a file with a second is refused before it is
// decoded. A file it cannot follow is costed as lossless, the dearer kind,
// with the whole file as its bitstream; the decoder will have the last word.
func webpPicture(data []byte) (lossless, alpha bool, vp8l int64) {
	unknown := func() (bool, bool, int64) { return true, false, int64(len(data)) }
	if len(data) < 12 {
		return unknown()
	}
	b := data[12:]
	for len(b) >= 8 {
		n := uint64(binary.LittleEndian.Uint32(b[4:8]))
		payload := int64(min(n, uint64(len(b)-8)))
		switch string(b[:4]) {
		case "VP8L":
			return true, false, payload
		case "ALPH":
			if !alpha {
				alpha = true
				// A plane too short to say how it is compressed is costed
				// as compressed, the dearer kind.
				if payload == 0 || b[8]&0x03 != 0 {
					vp8l = vp8lAlphaHeader + payload
				}
			}
		case "VP8 ":
			return false, alpha, vp8l
		case "ANMF":
			return unknown()
		}
		if n > uint64(len(b)-8) {
			break
		}
		next := 8 + int(n) + int(n&1)
		if next > len(b) {
			break
		}
		b = b[next:]
	}
	return unknown()
}
