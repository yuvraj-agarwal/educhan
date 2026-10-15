package media

import (
	"encoding/binary"
)

// WebP is a RIFF file: a list of tagged chunks. golang.org/x/image/webp only
// decodes, so a WebP upload is stored as a PNG or a JPEG made from its pixels
// (sanitizeWebP). Before it is decoded it is stripped here at the chunk level,
// so the decoder is handed only the chunks that make the picture, and only
// once their sizes have been checked to agree.
//
// The rewrite keeps the chunks that make the picture and drops every other one:
// EXIF and XMP (where a camera's GPS position and an editor's history live),
// ICCP (the colour profile; sanitizeJPEG says why losing it is acceptable),
// and any chunk this code does not know, since an unknown chunk is exactly where
// a tool would put metadata nobody asked for. Readers are required to ignore
// unknown chunks, so dropping them changes nothing on screen. The VP8X flags
// that announce ICC, EXIF and XMP are cleared to match, and its reserved bits
// with them, a still picture's alpha flag is made to say whether an alpha
// plane was kept, the RIFF size is recomputed, and anything after the RIFF's
// declared end is dropped with the rest. What it keeps of the picture chunks
// (VP8, VP8L, ALPH) it keeps byte for byte, and a decoder ignores whatever
// follows the picture inside them, which is why the result is decoded rather
// than stored.
//
// Animation chunks are kept, and the unknown chunks inside each frame dropped,
// so the rewrite is complete for the whole format. An animated WebP is still
// refused, as it was before stripping existed: x/image cannot decode one, so
// there would be no thumbnail and no check that the frames decode at all.
// Inspect refuses it from its chunks (webpAnimated), before any cooldown and
// whatever its size, and sanitizeWebP again should one get past that.

const (
	vp8xFlagICC       = 0x20
	vp8xFlagAlpha     = 0x10
	vp8xFlagEXIF      = 0x08
	vp8xFlagXMP       = 0x04
	vp8xFlagAnimation = 0x02

	// anmfHeaderLen is the frame header at the start of an ANMF payload:
	// offsets, size, duration and flags, before the frame's own chunks.
	anmfHeaderLen = 16
)

// webpInfo is what the rewrite learned about the picture.
type webpInfo struct {
	Width, Height int
	Animated      bool
}

type riffChunk struct {
	id      string
	payload []byte
}

// stripWebP rewrites a WebP file with its metadata chunks removed. Its errors
// are ErrCorruptImage for a file whose structure does not hold together and
// ErrImageTooLarge for a canvas over MaxPixels; the structure has to be sound
// enough that what is decoded is what was checked.
func stripWebP(data []byte) ([]byte, webpInfo, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil, webpInfo{}, ErrCorruptImage
	}
	size := uint64(binary.LittleEndian.Uint32(data[4:8]))
	if size < 4 || size > uint64(len(data)-8) {
		return nil, webpInfo{}, ErrCorruptImage
	}
	region := data[12 : 8+size]

	// The chunks are walked in place rather than read into a list, so a file of
	// a million empty chunks costs one chunk's memory, not a million structs
	// (what is kept is bounded by the picture). The first chunk chooses the
	// format; the strip functions walk the whole region and keep only what makes
	// the picture.
	first, rest, ok := nextChunk(region)
	if !ok {
		return nil, webpInfo{}, ErrCorruptImage
	}
	var (
		kept []riffChunk
		info webpInfo
		err  error
	)
	if first.id == "VP8X" {
		kept, info, err = stripExtended(first, rest)
	} else {
		kept, info, err = stripSimple(region)
	}
	if err != nil {
		return nil, webpInfo{}, err
	}

	out := make([]byte, 12, len(data))
	copy(out, "RIFF\x00\x00\x00\x00WEBP")
	for _, c := range kept {
		out = appendChunk(out, c)
	}
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out, info, nil
}

// stripSimple handles the simple format: one lossy or lossless bitstream and,
// by the specification, nothing else. As a decoder does, it takes the first
// bitstream and drops what surrounds it; an ALPH chunk, which only the
// extended format may carry, makes the file corrupt. It walks the whole region
// so a malformed chunk anywhere is caught, as reading a list of them was.
func stripSimple(region []byte) ([]riffChunk, webpInfo, error) {
	var (
		kept []riffChunk
		info webpInfo
		fail error
	)
	if !walkChunks(region, func(c riffChunk) {
		if fail != nil || kept != nil {
			return // decided; keep walking only to validate the structure
		}
		switch c.id {
		case "ALPH":
			fail = ErrCorruptImage
		case "VP8 ", "VP8L":
			w, h, ok := bitstreamSize(c)
			if !ok {
				fail = ErrCorruptImage
				return
			}
			if int64(w)*int64(h) > MaxPixels {
				fail = ErrImageTooLarge
				return
			}
			kept, info = []riffChunk{c}, webpInfo{Width: w, Height: h}
		}
	}) {
		return nil, webpInfo{}, ErrCorruptImage
	}
	if fail != nil {
		return nil, webpInfo{}, fail
	}
	if kept == nil {
		return nil, webpInfo{}, ErrCorruptImage
	}
	return kept, info, nil
}

// stripExtended handles the extended format, which starts with a VP8X chunk
// (head) carrying the canvas size and the feature flags, with the rest of the
// region (rest) after it.
func stripExtended(head riffChunk, rest []byte) ([]riffChunk, webpInfo, error) {
	if len(head.payload) != 10 {
		return nil, webpInfo{}, ErrCorruptImage
	}
	flags := head.payload[0]
	info := webpInfo{
		Width:    1 + int(uint24(head.payload[4:7])),
		Height:   1 + int(uint24(head.payload[7:10])),
		Animated: flags&vp8xFlagAnimation != 0,
	}
	if int64(info.Width)*int64(info.Height) > MaxPixels {
		return nil, webpInfo{}, ErrImageTooLarge
	}

	// Only the flags describing the picture and the canvas size survive; the
	// metadata flags and every reserved bit, in the flags byte and the three
	// bytes after it, are left zero.
	vp8x := make([]byte, 10)
	vp8x[0] = flags & (vp8xFlagAlpha | vp8xFlagAnimation)
	copy(vp8x[4:], head.payload[4:])
	kept := []riffChunk{{id: "VP8X", payload: vp8x}}

	if info.Animated {
		var (
			anim, frames int
			fail         error
		)
		if !walkChunks(rest, func(c riffChunk) {
			if fail != nil {
				return
			}
			switch c.id {
			case "ANIM":
				if len(c.payload) != 6 {
					fail = ErrCorruptImage
					return
				}
				anim++
				kept = append(kept, c)
			case "ANMF":
				frame, ok := stripFrame(c.payload, info)
				if !ok {
					fail = ErrCorruptImage
					return
				}
				frames++
				kept = append(kept, riffChunk{id: "ANMF", payload: frame})
			case "VP8X":
				fail = ErrCorruptImage
			}
			// Everything else, including a stray still bitstream that a
			// still-image decoder would show instead of the animation, is
			// dropped.
		}) {
			return nil, webpInfo{}, ErrCorruptImage
		}
		if fail != nil {
			return nil, webpInfo{}, fail
		}
		if anim != 1 || frames == 0 {
			return nil, webpInfo{}, ErrCorruptImage
		}
		return kept, info, nil
	}

	picture, ok := stillImage(rest, info.Width, info.Height)
	if !ok {
		return nil, webpInfo{}, ErrCorruptImage
	}
	// The alpha flag is set when an alpha plane is kept, and only then.
	// libwebp, and with it every browser, takes the flag as a hint and shows
	// what the chunks hold, but golang.org/x/image refuses a file where the
	// two disagree: a lossless picture, which carries its own alpha, under the
	// flag, as libwebp itself writes one with transparency and metadata; a
	// lossy one under it with no plane; or a plane without it.
	vp8x[0] &^= vp8xFlagAlpha
	if picture[0].id == "ALPH" {
		vp8x[0] |= vp8xFlagAlpha
	}
	return append(kept, picture...), info, nil
}

// stillImage picks the chunks of one picture out of a region — an optional
// ALPH before a lossy bitstream, then the bitstream — checking that the
// bitstream is the size it is meant to be. Anything else is dropped. A decoder
// sizes its buffers from the bitstream and the canvas separately, so a file
// where the two disagree is refused here rather than trusted to either. It
// walks in place, holding no list of the region's chunks.
func stillImage(region []byte, w, h int) ([]riffChunk, bool) {
	var (
		alpha, bitstream              riffChunk
		haveAlpha, haveBitstream, bad bool
	)
	if !walkChunks(region, func(c riffChunk) {
		switch c.id {
		case "ALPH":
			if haveAlpha || haveBitstream {
				bad = true
				return
			}
			alpha, haveAlpha = c, true
		case "VP8 ", "VP8L":
			if haveBitstream {
				bad = true
				return
			}
			bitstream, haveBitstream = c, true
		case "VP8X", "ANIM", "ANMF":
			bad = true
		}
	}) {
		return nil, false
	}
	if bad || !haveBitstream || (haveAlpha && bitstream.id != "VP8 ") {
		return nil, false
	}
	if bw, bh, ok := bitstreamSize(bitstream); !ok || bw != w || bh != h {
		return nil, false
	}
	if haveAlpha {
		return []riffChunk{alpha, bitstream}, true
	}
	return []riffChunk{bitstream}, true
}

// stripFrame rewrites the payload of one ANMF chunk: its 16-byte header, then
// the frame's picture with every other chunk in it dropped. The frame has to
// lie inside the canvas.
func stripFrame(payload []byte, canvas webpInfo) ([]byte, bool) {
	if len(payload) < anmfHeaderLen {
		return nil, false
	}
	x := 2 * int(uint24(payload[0:3]))
	y := 2 * int(uint24(payload[3:6]))
	w := 1 + int(uint24(payload[6:9]))
	h := 1 + int(uint24(payload[9:12]))
	if x+w > canvas.Width || y+h > canvas.Height {
		return nil, false
	}
	picture, ok := stillImage(payload[anmfHeaderLen:], w, h)
	if !ok {
		return nil, false
	}
	out := append(make([]byte, 0, len(payload)), payload[:anmfHeaderLen]...)
	for _, c := range picture {
		out = appendChunk(out, c)
	}
	return out, true
}

// webpAnimated reports whether a WebP is an animation: its first chunk is a
// VP8X with the animation flag, or a frame (ANMF) is among the chunks inside
// its RIFF, as stripWebP reads them. Neither can be decoded here — a frame
// outside an animation fails stillImage — so Inspect refuses such a file
// as it would fail once processed, but from its header. The walk stops at the
// first chunk it cannot follow, which stripWebP refuses anyway.
func webpAnimated(data []byte) bool {
	if len(data) < 12 {
		return false
	}
	size := uint64(binary.LittleEndian.Uint32(data[4:8]))
	if size < 4 || size > uint64(len(data)-8) {
		return false
	}
	b := data[12 : 8+size]
	for first := true; len(b) > 0; first = false {
		c, rest, ok := nextChunk(b)
		if !ok {
			return false
		}
		switch {
		case first && c.id == "VP8X" && len(c.payload) > 0 && c.payload[0]&vp8xFlagAnimation != 0:
			return true
		case c.id == "ANMF":
			return true
		}
		b = rest
	}
	return false
}

// nextChunk reads the first RIFF chunk of b: a four-character code, a
// little-endian length, the payload, and a pad byte when the length is odd. It
// returns the chunk, the bytes after it, and whether b began with a well-formed
// chunk; a missing final pad byte is tolerated, a length running past the end
// is not. The payload aliases b.
func nextChunk(b []byte) (riffChunk, []byte, bool) {
	if len(b) < 8 {
		return riffChunk{}, nil, false
	}
	size := uint64(binary.LittleEndian.Uint32(b[4:8]))
	if size > uint64(len(b)-8) {
		return riffChunk{}, nil, false
	}
	n := int(size)
	next := 8 + n + n&1
	if next > len(b) {
		next = len(b)
	}
	return riffChunk{id: string(b[:4]), payload: b[8 : 8+n]}, b[next:], true
}

// walkChunks visits the RIFF chunks in b in order and reports whether the whole
// run is well-formed. It walks in place and builds no list, so a file of a
// million empty chunks costs it one chunk's memory, not a million structs: only
// what the caller keeps is held. The chunk passed to visit aliases b and must
// be copied, not retained, to outlive the walk.
func walkChunks(b []byte, visit func(riffChunk)) bool {
	for len(b) > 0 {
		c, rest, ok := nextChunk(b)
		if !ok {
			return false
		}
		visit(c)
		b = rest
	}
	return true
}

func appendChunk(out []byte, c riffChunk) []byte {
	out = append(out, c.id...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(c.payload)))
	out = append(out, c.payload...)
	if len(c.payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// bitstreamSize reads the picture size from the header of a VP8 (lossy) or
// VP8L (lossless) bitstream.
func bitstreamSize(c riffChunk) (int, int, bool) {
	p := c.payload
	switch c.id {
	case "VP8 ":
		// A frame tag whose low bit is clear marks a key frame, the only kind a
		// still image has; the start code and the 14-bit sizes follow.
		if len(p) < 10 || p[0]&1 != 0 || p[3] != 0x9d || p[4] != 0x01 || p[5] != 0x2a {
			return 0, 0, false
		}
		w := int(binary.LittleEndian.Uint16(p[6:8]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(p[8:10]) & 0x3fff)
		return w, h, w > 0 && h > 0
	case "VP8L":
		if len(p) < 5 || p[0] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(p[1:5])
		if bits>>29 != 0 { // the version, which is 0
			return 0, 0, false
		}
		return 1 + int(bits&0x3fff), 1 + int(bits>>14&0x3fff), true
	}
	return 0, 0, false
}

func uint24(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
}
