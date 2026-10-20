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
// distinguishes against the estimate for the whole of its processing, decoded
// to the end (completedCost), and against its decoding phases alone
// (decodingPhases), at sizes where each term charged for decoding comes to
// megabytes; TestThumbScratchCoversTheScaler measures the thumbnail's scratch,
// and TestGIFFrameOverheadCoversWhatDecodingKeeps what image/gif keeps of each
// frame. A term dropped is caught when it is larger than processingSlack — as
// each is in some file — but a partial under-charge smaller than the slack it
// absorbs and does not: the turn term, only a couple of megabytes above the
// slack in the largest file that turns, can be cut by more than half unseen. So
// the tests guard against a term left out, not against every under-estimate.
// Even a term left out goes unnoticed where another term charged for the same
// moment is at least as large in every file they measure; a few are, the alpha
// plane and the NRGBA copy of a transparent lossy WebP among them.

// processingBudget is the memory every upload being processed at once may
// hold between them, by their estimates, from when it starts to be decoded
// until both its renditions are stored (Save). An upload whose estimate alone
// is over it is refused from its header (ErrImageTooCostly); the others wait
// for their share, for as long as their post's deadline allows
// (processingAllowance). The uploads' own bytes are not in the estimates:
// UploadBudget (budget.go) counts them, from before they are read. Between
// them the two budgets hold at most 320 MiB, whatever number of requests an
// instance is let take at once. The rest of a process is not budgeted: idle,
// about 40 MiB. Eighty 8 MiB uploads sent at once to a server run with a
// GOMEMLIMIT of 400 MiB peaked at 330 MiB resident, which fits an instance of
// 512 MiB with that limit; the limit is the deployment's to set.
const processingBudget int64 = 256 << 20

// MaxUploadBytes is the most an upload size limit may be, 31 MiB: the
// largest size, in whole MiB, at which the cheapest file of every format, a
// picture of one pixel and bytes its decoder skips, is still processed within
// the budget. From about 31.5 MiB every file is refused as too costly to
// process (ErrImageTooCostly), whatever picture it holds, so the server
// refuses to start with a limit over this one. What an upload is estimated to
// hold grows with its size however little its picture is, since its stored
// file may come to storedPerByte times its size (storedLimit) and the
// encoder's buffer holds two of those as it grows (completedCost): eight bytes
// for every byte of the upload. What is left of the budget once
// processingSlack, storedLimit's floor twice over and cheapestRest are set
// aside, at eight bytes a byte, comes to 31.36 MiB, and is rounded down to a
// whole MiB. TestLargestAttachmentLimitIsReachable (internal/app) posts the
// cheapest file of each format at that size, and holds the same file a MiB
// larger to a refusal.
const MaxUploadBytes = ((processingBudget - processingSlack - 2*storedFloor - cheapestRest) /
	(2 * storedPerByte)) &^ (1<<20 - 1)

// cheapestRest bounds what the cheapest upload of any size holds besides its
// encoder's buffer: its picture of a pixel decoded, the thumbnail scaler's
// scratch for it, a GIF's overhead for its one frame (gifFrameOverhead), a
// few dozen kilobytes in all. It is set aside for MaxUploadBytes.
const cheapestRest int64 = 1 << 20

// processingSlack is what each estimate adds for what it does not itemise:
// decoders' tables and row buffers, compressors' windows, a PNG's gamma tables
// (applyPNGGamma, 128 KiB at most), the thumbnail's own pixels and encoding,
// and what a phase allocates while a collection runs.
const processingSlack int64 = 4 << 20

// gifFrameOverhead is what image/gif keeps for each frame of a GIF besides
// its pixels: the frame's header, its palette (a local colour table, or the
// copy of the global one DecodeAll makes for a frame with a transparent
// colour: 256 interface values and the colours they hold, 5 KiB), its place
// in three slices, and the up to 8 KiB the allocator rounds its pixels up
// by. A frame can be a handful of bytes of file, so without it a GIF of a
// few megabytes could decode to gigabytes of frames of no pixels at all.
const gifFrameOverhead int64 = 16 << 10

// gifFramePlay is what a GIF holds for each of its frames besides what
// image/gif keeps (gifFrameOverhead) while it is played and written:
// what the walk read of the frame (gifFrameHead), 48 bytes, kept from before
// the frames are played until they all have been. playGIF keeps nothing
// else of a frame: what it writes of each goes into the stored file, which
// storedLimit bounds, and it lets each frame go once it has played it.
const gifFramePlay int64 = 64

// gifPlayScratch is what playGIF allocates to play a GIF of the frames given
// on a w×h canvas: the colour key of every pixel of what the frames before
// the one being played left (under), four bytes each; two bit sets over the
// canvas, of the pixels the stored frames of the picture showing and of the
// next one draw; and four rows of scratch, three of colour keys and one of
// the indices written. An animation whose tables give more than 255 colours
// (gifFrames.many) may be stored with a table a frame, and some of its
// frames put back, for which the player keeps what was under the stored
// frame of the picture showing (saved), four bytes a pixel more. Its colour
// index and its LZW compressor's tables, under 80 KiB together, are left to
// processingSlack.
func gifPlayScratch(w, h int64, frames gifFrames) int64 {
	px := w * h
	scratch := 4*px + 2*8*((px+63)/64) + 13*w
	if frames.count > 1 && frames.many {
		scratch += 4 * px
	}
	return scratch
}

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
// from its header. That is the more of two ways it can go: decoded to the
// end (completedCost), which bounds a decoder that fails part of the way
// through as well, but for one: a WebP whose vp8l bitstream names more
// Huffman groups than it gives and then ends (vp8lStopCost), which makes
// golang.org/x/image's decoder set aside every group it named however few
// bytes it took to name them. cfg is the header's, already checked against
// MaxPixels; frames is a GIF's frames (gifFramePixels).
func processingCost(format Format, data []byte, cfg image.Config, frames gifFrames) int64 {
	return max(completedCost(format, data, cfg, frames), vp8lStopCost(format, data, cfg))
}

// completedCost is what an upload holds when it is decoded to the end: what
// the decoder allocates, what converting it and turning it upright copy, the
// encoder's output (bounded by storedLimit) and the thumbnail scaler's
// scratch.
func completedCost(format Format, data []byte, cfg image.Config, frames gifFrames) int64 {
	phases := decodingPhases(format, data, cfg, frames)
	if phases == nil {
		return processingBudget + 1
	}
	held := phases[len(phases)-1].after
	upload := int64(len(data))
	limit := storedLimit(format, data, cfg.Width, cfg.Height)
	if format == FormatGIF {
		// A GIF is written as its frames are played (playGIF), into a
		// buffer the size of the upload that doubles up to limit, holding
		// the old and the new while it grows (storedBuffer); the buffer
		// and the player's scratch are allocated as it starts. Its
		// thumbnail was made among its decoding phases, and its frames are
		// let go once they are written.
		play := gifPlayScratch(int64(cfg.Width), int64(cfg.Height), frames)
		phases = append(phases, phase{first: min(upload, limit) + play, alloc: play + 2*limit, after: limit})
		return peakOf(phases...) + processingSlack
	}
	// The thumbnail is made of the picture as it is stored, turned upright.
	tw, th := cfg.Width, cfg.Height
	if storedOnItsSide(format, data) {
		tw, th = th, tw
	}
	scratch := thumbScratch(tw, th)
	// The encoder writes into a buffer the size of the upload, which
	// doubles up to limit, holding the old and the new while it grows
	// (storedBuffer). What it wrote is kept for the thumbnail.
	phases = append(phases, phase{first: min(upload, limit), alloc: 2 * limit, after: held + limit})
	// The scaler allocates its scratch whole as it starts. What the
	// encoder wrote is kept after it, until it has been stored (Save
	// holds the budget until then), but nothing larger is made for
	// that: the local store writes the file from it as it is, and the
	// Cloud Storage writer is given no buffer of its own (GCSStore.Put).
	phases = append(phases, phase{first: scratch, alloc: scratch, after: limit})
	return peakOf(phases...) + processingSlack
}

// storedOnItsSide reports whether an upload's EXIF orientation turns it on
// its side (5 to 8), so that it is stored with its width and height swapped:
// a JPEG's or a PNG's, which sanitize turns upright, and never a GIF's or a
// WebP's, which it does not.
func storedOnItsSide(format Format, data []byte) bool {
	switch format.Name {
	case FormatJPEG.Name:
		return jpegOrientation(data) >= 5
	case FormatPNG.Name:
		return pngOrientation(data) >= 5
	}
	return false
}

// decodingPhases are the phases of processing an upload before it is
// encoded again: decoding it, converting it and turning it upright (and for a
// GIF, thumbnailing its first picture), which is everything sanitize does
// before it writes the first byte of the stored file. They are nil for a
// format it does not know.
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
		// with what the walk read of it, until it has been played. An
		// interlaced frame is decoded once more while its rows are put in
		// order: at most the canvas, or a frame reaching past it
		// (gifReader) if that is larger. The first picture is then
		// thumbnailed, at the size of the canvas. Playing the frames
		// writes the stored file as it goes, from its first byte
		// (completedCost).
		kept := frames.pixels + frames.count*(gifFrameOverhead+gifFramePlay)
		scratch := thumbScratch(cfg.Width, cfg.Height)
		return []phase{
			{alloc: kept + max(px, frames.largest), after: kept},
			{first: scratch, alloc: scratch, after: kept},
		}
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
	limit := storedPerByte*int64(len(data)) + int64(w)*int64(h)/2 + storedFloor
	if format == FormatWebP {
		if lossless, alpha, _ := webpPicture(data); !lossless && alpha {
			limit = max(limit, pngLimit(w, h, 4))
		}
	}
	return limit
}

// storedPerByte and storedFloor are storedLimit's terms besides the pixels:
// four bytes for every byte of the upload, and 64 KiB whatever its size.
// MaxUploadBytes is derived from them.
const (
	storedPerByte int64 = 4
	storedFloor   int64 = 64 << 10
)

// pngLimit is the most image/png writes for a w×h picture of bpp bytes a
// pixel, however badly it compresses: its rows with a filter byte each, which
// deflate stores with 5 bytes for every block of up to 32 KiB it cannot
// compress, in IDAT chunks of 12 bytes for every 32 KiB, a 1024th more than
// the rows in all, and its headers.
func pngLimit(w, h, bpp int) int64 {
	rows := int64(h) * (1 + int64(bpp)*int64(w))
	return rows + rows/1024 + 64<<10
}

// thumbScratch bounds what golang.org/x/image/draw's kernel scaler allocates
// to make the thumbnail of a w×h picture. It works through a buffer of four
// float64s for every pixel of a picture as wide as the thumbnail and as tall
// as the source: 32 bytes × ThumbMaxDim × the shorter side, but for a picture
// more than ThumbMaxDim times taller than it is wide, whose thumbnail is still
// a pixel wide, so that its buffer is 32 bytes for every row. And it holds a
// table of weights for each axis (newDistrib): 24 bytes for every thumbnail
// pixel along it, and a 16-byte weight for each source pixel the kernel
// spreads over each of them, at most four for every source pixel and six
// for every thumbnail pixel. The tables come to 64 bytes for every pixel of
// the two sides, which a square barely notices and is most of the scratch
// for a long, thin strip: a PNG of 25 megapixels in one row would take
// 1.6 GB of them, from an upload of kilobytes.
func thumbScratch(w, h int) int64 {
	tw, th := ThumbSize(w, h, ThumbMaxDim)
	weights := func(dst, src int) int64 { return 24*int64(dst) + 16*(4*int64(src)+6*int64(dst)) }
	return 32*int64(tw)*int64(h) + weights(tw, w) + weights(th, h)
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

// jpegComp is one of a JPEG frame's components: the id a scan names it by and
// the sampling factors that say how many 8×8 blocks of it an MCU holds.
type jpegComp struct {
	id   byte
	h, v int64
}

// jpegFrame is what jpegPhases needs of a JPEG's frame header and the
// application segments around it, and what jpegQuality needs of its
// quantisation tables.
type jpegFrame struct {
	progressive bool
	comps       int
	blocks      int64 // 8×8 blocks the decoder allocates, all components
	ids         [3]byte
	// width and height are the frame's, and comp its first ncomp components'
	// ids and sampling factors, which jpegScanBlockVisits walks the scans
	// with.
	width, height int64
	comp          [4]jpegComp
	ncomp         int
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

// readJPEGFrame walks a JPEG's segments up to its first scan as image/jpeg's
// decoder does (jpegNextSegment): in place, every length checked before it is
// used, nothing allocated. It reports whether it found a frame header before
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
	for i := 2; ; {
		marker, segment, next, ok := jpegNextSegment(data, i)
		if !ok {
			return f, framed
		}
		switch marker {
		case 0xDA:
			f.tables = framed
			return f, framed
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
		i = next
	}
}

// jpegNextSegment finds the segment image/jpeg's decoder reads next when it
// looks for one at offset i, and returns its marker, its body and the offset
// after it, the way the decoder's own segment loop goes. Whatever comes before
// an 0xFF is extraneous data, which the decoder skips a byte at a time rather
// than refusing, as libjpeg does; an 0xFF00 pair is skipped whole; a run of
// 0xFF fill bytes stands for the marker after it; and a restart marker
// (0xFFD0..0xFFD7), which has no length, is passed over. A scan's
// entropy-coded data is nothing but such bytes up to the next marker (stuffed
// 0xFF00 pairs and restart markers), and the decoder's bit reader never reads
// past a marker that is not one of those, so whatever of a scan's data the
// decoder takes, it next finds the segment this finds, read from right after
// the scan's header. ok is false where the decoder stops: at the end of the
// image (0xFFD9), at the end of the data, at a segment whose length does not
// fit, and at a marker it does not read past but refuses — anything but a
// frame header it decodes (0xFFC0..0xFFC2), a table (0xFFC4, 0xFFDB), a scan
// (0xFFDA), a restart interval (0xFFDD), an application segment
// (0xFFE0..0xFFEF) or a comment (0xFFFE).
func jpegNextSegment(data []byte, i int) (marker byte, body []byte, next int, ok bool) {
	for i+1 < len(data) {
		switch {
		case data[i] != 0xFF:
			i++
			continue
		case data[i+1] == 0x00:
			i += 2
			continue
		}
		j := i + 1
		for j < len(data) && data[j] == 0xFF {
			j++
		}
		if j >= len(data) {
			return 0, nil, len(data), false
		}
		marker, i = data[j], j+1
		switch {
		case marker >= 0xD0 && marker <= 0xD7:
			continue
		case marker == 0xC0, marker == 0xC1, marker == 0xC2, marker == 0xC4,
			marker == 0xDA, marker == 0xDB, marker == 0xDD, marker == 0xFE,
			marker >= 0xE0 && marker <= 0xEF:
		default:
			return marker, nil, i, false
		}
		if i+2 > len(data) {
			return marker, nil, len(data), false
		}
		length := int(data[i])<<8 | int(data[i+1])
		if length < 2 || length > len(data)-i {
			return marker, nil, len(data), false
		}
		return marker, data[i+2 : i+length], i + length, true
	}
	return 0, nil, len(data), false
}

// maxJPEGScanRevisits caps the work a JPEG's scans may ask of image/jpeg,
// averaged over the picture's 8×8 blocks and counted in passes of a progressive
// first scan over them (jpegScanBlockVisits). A progressive file may repeat a
// band's scan any number of times, each a few dozen bytes and holding no more
// memory, so that its decoding visits orders of magnitude more blocks than the
// picture has; a baseline file may repeat its scans too, at a couple of bits a
// block each. Either is a loop the pixel cap (MaxPixels) and the memory
// estimate (processingCost) do not see, because it is bounded by neither the
// resolution nor what is held but by the scan count. A file whose scans would
// come to more than this many passes is refused as too costly to process
// (ErrImageTooCostly), from its header, before any cooldown (inspect). What
// encoders write is far below it: a baseline file's one scan counts
// jpegBaselineVisitWork (6) passes, and the progressive files here, libjpeg's
// ten-scan default as Pillow writes it, 6 to 8 with their refinement scans
// weighed in. The ceiling bounds the decoder's work: the largest picture it
// admits is one the memory estimate still fits with the most blocks, about 0.8
// million — a 25-megapixel 4:2:2 picture, or about 17 megapixels at 4:4:4 — and
// at the cap its scans take about 3 to 4 s of CPU on one core, and the whole
// of Process about 4 to 6 s, whichever kind of scan fills it: 64 first passes,
// about 32 refinement passes, or 10 baseline scans. Those are the dearest of
// each kind — first passes in which every block codes an end-of-band symbol of
// its own rather than sharing a run, and baseline scans with detail in every
// block, where scans of empty blocks take a little less — measured in CPU time
// on one core of a shared machine, whose load makes most of the spread. A grey
// or 4:2:0 picture of 25 megapixels has fewer blocks, about 0.4 and 0.6
// million, and its scans take at most about 2 s and 3 s. All of it is well
// inside processingAllowance.
const maxJPEGScanRevisits = 64

// jpegBaselineVisitWork and jpegRefineVisitWork weigh a block visit by what
// image/jpeg does on it, in visits of a progressive first scan (which loads the
// block's coefficients, decodes a symbol or counts down an end-of-band run, and
// stores them back), as measured on one core. A baseline scan also dequantises
// the block, inverse-transforms it and writes it into the picture
// (reconstructBlock), about six times that work; a progressive AC refinement
// scan walks every coefficient of the band in every block (refineNonZeroes),
// inside an end-of-band run too, about twice it. Unweighed, the cap admitted a
// 25-megapixel 4:2:2 file of 64 baseline scans (12 MB) that decoded for about
// 14 s, and one of refinement scans (100 KB) for about 5.5 s, where 64 first
// passes take about 3 to 4.
const (
	jpegBaselineVisitWork = 6
	jpegRefineVisitWork   = 2
)

// jpegScanBlockVisits adds up the 8×8 blocks image/jpeg visits decoding every
// scan of a JPEG whose frame header is f (readJPEGFrame), each weighed by the
// work the decoder does on it (jpegBaselineVisitWork, jpegRefineVisitWork). The
// decoder walks the MCU grid once per scan (processSOS), and for each MCU
// visits every 8×8 block of the scan's components; so a scan's visits are the
// grid, mxx×myy from the first component's sampling, times the sum of h×v over
// the components the scan lists. A baseline file's one scan visits each block
// once; a progressive file revisits a component's blocks on each of its scans.
// It walks the segments as the decoder reads them (jpegNextSegment), its
// extraneous bytes and each scan's entropy-coded data stepped over to the next
// marker exactly as the decoder steps over them, so it counts every scan the
// decoder processes and stops where the decoder would: a byte the decoder skips
// cannot hide a scan from it, and a scan header of the wrong length, which the
// decoder refuses, ends the count. The count, not the file's size, is what
// maxJPEGScanRevisits bounds.
func jpegScanBlockVisits(data []byte, f jpegFrame) int64 {
	if f.ncomp == 0 || f.comp[0].h < 1 || f.comp[0].v < 1 {
		return 0
	}
	h0, v0 := f.comp[0].h, f.comp[0].v
	mxx := (f.width + 8*h0 - 1) / (8 * h0)
	myy := (f.height + 8*v0 - 1) / (8 * v0)
	mcus := mxx * myy

	var visits int64
	for i := 2; ; {
		marker, body, next, ok := jpegNextSegment(data, i)
		if !ok {
			return visits
		}
		i = next
		if marker != 0xDA {
			continue
		}
		// The SOS header lists the scan's components by selector; sum h×v
		// over the frame components they name. Its entropy-coded data is
		// stepped over by the next call, from right after the header.
		ns := 0
		if len(body) >= 1 {
			ns = int(body[0])
		}
		if ns < 1 || len(body) != 4+2*ns {
			return visits
		}
		var hv int64
		for k := 0; k < ns; k++ {
			cs := body[1+2*k]
			for c := 0; c < f.ncomp; c++ {
				if f.comp[c].id == cs {
					hv += f.comp[c].h * f.comp[c].v
					break
				}
			}
		}
		// The spectral start and the successive-approximation high bit,
		// which the decoder reads only in a progressive file.
		ss, ah := body[1+2*ns], body[3+2*ns]>>4
		switch {
		case !f.progressive:
			hv *= jpegBaselineVisitWork
		case ss > 0 && ah > 0:
			hv *= jpegRefineVisitWork
		}
		visits += mcus * hv
	}
}

// jpegScansTooCostly reports whether a JPEG's scans would make image/jpeg
// revisit its blocks more than maxJPEGScanRevisits times over
// (jpegScanBlockVisits). A frame readJPEGFrame could not parse (framed false)
// has no sampling to walk the scans with, and image/jpeg will not decode it
// either, so it is left to the memory estimate and the decoder's own refusal.
func jpegScansTooCostly(data []byte) bool {
	f, framed := readJPEGFrame(data)
	if !framed {
		return false
	}
	return jpegScanBlockVisits(data, f) > maxJPEGScanRevisits*f.blocks
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
	f.width, f.height, f.ncomp = width, height, n
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
		f.comp[k] = jpegComp{id: c[0], h: h, v: v}
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

// webpPhases models golang.org/x/image/webp and what sanitizeWebP does after
// it, starting from stripWebP's copy of the file. A lossy picture decodes to
// YCbCr at its size in whole 16-pixel macroblocks, 1.5 bytes a pixel, from a
// copy the decoder makes of its compressed partitions. An opaque one is taken
// to full range in those planes (expandLimitedRange) and encoded from them as
// a JPEG; one with an alpha plane is converted to NRGBA, 4 bytes a pixel, for
// its PNG (lossyWebPToNRGBA), which the header cannot tell apart from one
// whose alpha turns out opaque throughout, so it is charged either way. A
// lossless picture decodes to NRGBA, 4 bytes a pixel, and holds more than that
// on the way (vp8lPixels). An alpha plane, decoded first, is a byte a pixel,
// but a compressed one is decoded as a lossless picture first and copied out
// of it, which is no more than the lossless picture's own peak.
//
// A lossless bitstream, and a VP8L-compressed alpha plane, also make the
// decoder hold its Huffman groups (vp8lMemory), alive while the pixels are
// decoded, which the pixel terms alone do not cover. They are charged on the
// bytes the vp8l decoder can read, which webpPicture finds: the decoder is
// handed one chunk, never the rest of the file. These are the phases of a
// bitstream the decoder reads to its end; one that ends after naming its
// groups can hold more of them, and none of the pixels (vp8lStopCost).
func webpPhases(data []byte, w, h int64) []phase {
	px, stripped := w*h, int64(len(data))
	planes := 384 * ((w + 15) / 16) * ((h + 15) / 16)
	strip := phase{first: stripped, alloc: stripped, after: stripped}
	lossy := phase{first: planes + stripped, alloc: planes + stripped, after: planes}
	lossless, alpha, bitstream := webpPicture(data)
	decode := vp8lMemory(bitstream) + vp8lPixels(w, h)
	switch {
	case lossless:
		return []phase{strip, {first: decode, alloc: decode, after: 4 * px}}
	case alpha:
		lossy.after += px
		return []phase{
			strip,
			{first: decode, alloc: decode, after: stripped + px},
			lossy,
			// The picture is converted from its YCbCr planes and alpha
			// plane to an NRGBA copy (4 bytes a pixel), which is held while
			// it is encoded; the planes are let go once it is made.
			{first: 4 * px, alloc: 4 * px, after: 4 * px},
		}
	}
	// The planes are taken to full range where they lie, and encoded from.
	return []phase{strip, lossy}
}

// vp8lPixels is the most golang.org/x/image's vp8l decoder holds for a w×h
// picture it decodes to its end, besides its Huffman groups (vp8lMemory):
// 6.625 bytes a pixel, and a little more where a side is not a multiple of 8.
// The picture decodes to NRGBA, 4 bytes a pixel, but one written with a
// colour index of 16 colours or fewer is decoded packed first, and unpacked
// into a second picture while the packed one is still held
// (inverseColorIndexing): at 5 to 16 colours two pixels to every four bytes,
// 2 bytes a pixel, which is the most; at fewer, four or eight. With more
// colours there is no packed picture, and the indices are replaced where they
// lie. Beside both pictures are the images of a predictor and a cross-colour
// transform read before the colour index (decodeTransform), held until the
// picture is returned: a quarter of a byte a pixel each (vp8lTileImage), or
// less after the colour index, where they are of the packed picture's width.
// And the meta-image that maps tiles to Huffman groups (decodeHuffmanGroups)
// is of the picture decodePix decodes, the packed one, an eighth of a byte a
// pixel of the whole at 5 to 16 colours. It and the groups are let go once
// the packed picture is done, just before the unpacked one is made, but a
// collection that began before then keeps them live until it ends (peakOf),
// so they are charged beside both pictures. That is 4 + 2 + 2/4 + 1/8 bytes
// a pixel. A picture that is not packed holds less: its meta-image is a
// quarter of a byte a pixel, but there is no second picture. The colour
// index's palette (1 KiB) and the colour cache (8 KiB at most) are inside
// processingSlack. A crafted file of 42 bytes that has all of these holds
// 6.5 bytes a pixel as it decodes 25 megapixels, 155 MiB, and 158 MiB when a
// collection runs on past the moment its meta-image is let go; 147 MiB had
// been charged for it (TestProcessingCostCoversWhatIsHeld). The meta-image's
// eighth of a byte a pixel, 3 MiB there, is less than processingSlack, so
// that test would not see it left out.
func vp8lPixels(w, h int64) int64 {
	packed := (w + 1) / 2
	return 4*w*h + 4*packed*h + 2*vp8lTileImage(w, h) + vp8lTileImage(packed, h)
}

// vp8lTileImage is the most the vp8l decoder holds for an image of tiles of a
// w×h picture — a predictor or a cross-colour transform's, or the meta-image
// — which has a pixel, 4 bytes, for every tile, and a tile is 4×4 pixels at
// the least (decodeTransform, decodeHuffmanGroups). One of a single colour
// costs the bitstream no bits at all, so it is held whatever the file's size.
func vp8lTileImage(w, h int64) int64 {
	return 4 * ((w + 3) / 4) * ((h + 3) / 4)
}

// vp8lGroupBacking is the largest backing array golang.org/x/image's vp8l
// decoder allocates for its Huffman groups. A lossless (VP8L) bitstream, and
// a VP8L-compressed alpha plane, may carry a meta-image that maps tiles to a
// group index, and the decoder makes one hGroup for every index up to the
// largest the meta-image holds (decodeHuffmanGroups), all of them at once,
// before it reads a single group's trees. The index is two bytes, so it
// reaches 65535; an hGroup is five Huffman trees, each a 24-byte slice header
// and a 512-byte look-up table. None of this is in the file's header or its
// resolution, nor bounded by its size: the meta-image can name index 65535
// in a few bits, so a bitstream of a few dozen bytes that names it and ends
// forces the whole allocation (vp8lStopCost). Only a bitstream the decoder
// reads to its end has given the trees of every group it named, which bounds
// them by its size (vp8lBackingPerByte).
const vp8lGroupBacking int64 = 65536 * 5 * (24 + 512) // 167.5 MiB

// vp8lBackingPerByte and vp8lNodesPerByte bound, per byte of a bitstream the
// decoder reads to its end, the two allocations the vp8l decoder makes that
// the header cannot predict: the hGroup backing array (vp8lGroupBacking,
// capped), and the trees' node slices (make([]hNode, 1, 2*symbols-1), 8
// bytes each). The cheapest hGroup is five one-symbol trees, about 2.5 bytes
// of bitstream for 2680 bytes of backing, so 1072. The most node memory a
// bitstream byte can force is bounded by the decoder's alphabets (256, plus
// up to a 2048-entry colour cache for the first tree) and the cheapest
// encoding of their code lengths (about half a bit a symbol through repeat
// codes, eight node bytes a symbol): about 256 node bytes a byte, so 384
// leaves a margin. Nodes are made as each tree is read, so vp8lNodesPerByte
// bounds a bitstream that ends early too.
// TestVP8LHuffmanMemoryIsHeldWithinTheEstimate measures a crafted file at
// each extreme against the estimate these give: 160 KiB of one-symbol
// groups, the most backing a byte can force, which holds about 170 MiB, and
// 215 KiB of full trees, the most nodes, which holds about 43 MiB.
const (
	vp8lBackingPerByte int64 = 1072
	vp8lNodesPerByte   int64 = 384
)

// vp8lMemory bounds the memory the vp8l decoder holds, beyond its decoded
// pixels, for a bitstream of n bytes that it reads to its end: the hGroup
// backing (capped at the 16-bit index ceiling) and the trees' node slices. A
// file splits its bytes between forcing more groups and fuller trees, so
// charging the maximum of each on the whole size over-counts, which is safe.
// A lossless bitstream or a VP8L-compressed alpha plane of more than 224 KiB
// is refused by this from its header, before any cooldown (processingCost,
// Inspect), and less in a larger picture, whose pixels take more of the
// budget beside it: about 203 KiB at a megapixel and 128 KiB at nine, and a
// little less again in a larger upload (TestVP8LHuffmanMemoryIsCharged).
// x/image's decoder cannot be held to less, and no header field says a given
// file is benign.
func vp8lMemory(n int64) int64 {
	return min(vp8lGroupBacking, vp8lBackingPerByte*n) + vp8lNodesPerByte*n
}

// vp8lStopCost is what a WebP holds when its vp8l bitstream — n bytes, as
// webpPicture finds them — names Huffman groups and ends before it has given
// their trees, which golang.org/x/image's decoder finds out only after it has
// made all of them (vp8lGroupBacking). It then fails, before it makes the
// pixels, so the picture's size counts only for the images it decoded before
// the groups and still holds beside them: a predictor and a cross-colour
// transform's, and the meta-image, each at most vp8lTileImage of the whole
// picture, which are megabytes in a large picture and cost the bitstream no
// bits at all when each is one colour; a colour-indexing transform's palette
// and the colour cache are a few kilobytes, inside processingSlack. With the
// copy stripWebP made, the nodes of the trees it did read, and
// processingSlack, that is at least 167.5 MiB for every lossless WebP and
// every one whose alpha plane is compressed, however small: a file of a few
// dozen bytes makes the decoder set that much aside
// (TestVP8LGroupsNamedButNeverGivenAreCharged), and no header field tells such
// a file from a benign one. So two are never processed at once. It is zero for
// anything else, which the vp8l decoder does not read.
func vp8lStopCost(format Format, data []byte, cfg image.Config) int64 {
	if format != FormatWebP {
		return 0
	}
	_, _, n := webpPicture(data)
	if n == 0 {
		return 0
	}
	images := 3 * vp8lTileImage(int64(cfg.Width), int64(cfg.Height))
	return int64(len(data)) + vp8lGroupBacking + vp8lNodesPerByte*n + images + processingSlack
}

// vp8lAlphaHeader is the VP8L header golang.org/x/image's decoder puts in
// front of a compressed alpha plane (readAlpha), where the ALPH chunk's own
// first byte, its compression method and filter, was.
const vp8lAlphaHeader = 5

// webpPicture reports, from a WebP's top-level chunks, whether its picture is
// lossless, whether it has an alpha plane, and how many bytes of it
// golang.org/x/image's vp8l decoder can read, which is what vp8lMemory and
// vp8lStopCost are charged on: a lossless picture's VP8L chunk; an alpha plane
// compressed with VP8L (method 1 in the low bits of its first byte; method 0
// is uncompressed, allocates only the plane, and is not read through vp8l),
// its ALPH chunk with the header the decoder puts in front of it; and
// otherwise nothing. The decoder reads each chunk through a reader that ends
// with the chunk, so the rest of the file — a lossy bitstream, a colour
// profile, EXIF — is never part of it. These are the first such chunks,
// which are the ones stripWebP keeps and the decoder takes: a file with a
// second is refused before it is decoded. A file it cannot follow is costed as
// lossless, the dearer kind, with the whole file as its bitstream; the decoder
// will have the last word. An animated WebP, which has frames (ANMF) where a
// still has its picture, never gets here: Inspect refuses it from its chunks
// first (webpAnimated), so it is not charged as one long lossless picture.
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
