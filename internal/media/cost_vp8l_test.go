package media

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

// golang.org/x/image's vp8l decoder sizes two allocations from the compressed
// bitstream rather than from the header: an hGroup for every meta-image index
// (up to 65536, 2680 bytes each) and a node slice for every Huffman tree (up
// to 2*alphabet-1 nodes). A crafted 1×1 lossless WebP of 160 KiB makes it hold
// about 170 MiB (TestVP8LHuffmanMemoryIsHeldWithinTheEstimate), which no
// resolution cap catches, and a larger one more. cost.go charges that memory
// (vp8lMemory) on the bytes the decoder reads, so Inspect refuses an
// oversized bitstream from its header, and these tests pin that it does,
// where the line falls, and that the charge covers what is held.

// vp8lBitWriter writes a VP8L bitstream the way golang.org/x/image's decoder
// reads one: bits low-order first within each byte.
type vp8lBitWriter struct {
	buf  []byte
	bits uint64
	n    uint
}

func (w *vp8lBitWriter) put(v uint32, n uint) {
	w.bits |= uint64(v&(1<<n-1)) << w.n
	w.n += n
	for w.n >= 8 {
		w.buf = append(w.buf, byte(w.bits))
		w.bits >>= 8
		w.n -= 8
	}
}

func (w *vp8lBitWriter) bytes() []byte {
	out := w.buf
	if w.n > 0 {
		out = append(out, byte(w.bits))
	}
	return out
}

// simpleTree writes a one-symbol Huffman tree (the cheapest kind, one node).
func (w *vp8lBitWriter) simpleTree(symbol uint32) {
	w.put(1, 1) // useSimple
	w.put(0, 1) // nSymbols-1 = 0
	if symbol < 2 {
		w.put(0, 1)
		w.put(symbol, 1)
	} else {
		w.put(1, 1)
		w.put(symbol, 8)
	}
}

// fullTree writes a complete depth-8 tree over 256 symbols (511 nodes): the
// kind that forces the dearest node allocation per byte of bitstream.
func (w *vp8lBitWriter) fullTree() {
	w.put(0, 1)  // not simple
	w.put(15, 4) // nCodes = 19
	order := []int{17, 18, 0, 1, 2, 3, 4, 5, 16, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	lens := map[int]uint32{8: 1, 16: 1} // symbols 8 and 16 at length 1
	for _, sym := range order {
		w.put(lens[sym], 3)
	}
	w.put(0, 1) // useLength = 0 -> all 256 symbols
	w.put(0, 1) // symbol 8 (code bit 0): an explicit length 8
	for _, run := range append([]uint32{3}, repeated(6, 42)...) {
		w.put(1, 1)     // symbol 16 (code bit 1): repeat previous length
		w.put(run-3, 2) // 3 + (0..3); 3 + 42*6 = 255 more symbols
	}
}

func repeated(v uint32, n int) []uint32 {
	s := make([]uint32, n)
	for i := range s {
		s[i] = v
	}
	return s
}

// adversarialLosslessWebP builds a 1×1 lossless WebP whose meta-image names a
// single hGroup index, so the decoder allocates index+1 hGroups, each of five
// trees: simple when full is false (cheapest, maximising groups per byte), or
// three complete 256-symbol trees when true (maximising nodes per byte).
func adversarialLosslessWebP(groups int, full bool) []byte {
	w := &vp8lBitWriter{}
	w.put(0x2f, 8) // VP8L magic
	w.put(0, 14)   // width-1
	w.put(0, 14)   // height-1
	w.put(0, 1)    // alpha hint
	w.put(0, 3)    // version
	w.put(0, 1)    // no transforms
	w.put(0, 1)    // main decodePix: no colour cache
	w.put(1, 1)    // use meta-image
	w.put(0, 3)    // hBits raw -> 2; a 1×1 image has a 1×1 meta-image
	w.put(0, 1)    // meta decodePix: no colour cache
	maxIdx := uint32(groups - 1)
	// Meta pixel's green=low byte, red=high byte of the index.
	w.simpleTree(maxIdx & 0xff) // green
	w.simpleTree(maxIdx >> 8)   // red
	w.simpleTree(0)             // blue
	w.simpleTree(0)             // alpha
	w.simpleTree(0)             // distance
	for i := 0; i < groups; i++ {
		if full {
			w.simpleTree(0) // green: literal 0, so the main pixel decodes
			w.fullTree()    // red
			w.fullTree()    // blue
			w.fullTree()    // alpha
			w.simpleTree(0) // distance
		} else {
			for j := 0; j < 5; j++ {
				w.simpleTree(0)
			}
		}
	}
	if full { // the main pixel reads 8-bit red, blue, alpha from the full trees
		w.put(0, 8)
		w.put(0, 8)
		w.put(0, 8)
	}
	vp8l := w.bytes()
	out := []byte("RIFF\x00\x00\x00\x00WEBPVP8L")
	out = binary.LittleEndian.AppendUint32(out, uint32(len(vp8l)))
	out = append(out, vp8l...)
	if len(vp8l)%2 == 1 {
		out = append(out, 0)
	}
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out
}

// webpOfBitstream is a w×h WebP whose bytes for the vp8l decoder are n long
// — the VP8L chunk of a lossless picture ("lossless"), or the ALPH chunk of a
// lossy one whose alpha plane is compressed with VP8L ("calpha") — made up to
// an upload of size bytes, when that is more, by what else such a file
// carries: an EXIF chunk after the lossless picture, a larger lossy
// bitstream after the alpha plane. Nothing past the header is read before
// decoding, so the bytes are zero.
func webpOfBitstream(kind string, w, h, n, size int) []byte {
	var chunks []riffChunk
	switch kind {
	case "lossless":
		chunks = []riffChunk{vp8xChunk(vp8xFlagEXIF, w, h), {id: "VP8L", payload: make([]byte, n)}, {id: "EXIF"}}
	case "calpha":
		alph := make([]byte, n)
		alph[0] = 1 // compressed with VP8L
		chunks = []riffChunk{vp8xChunk(vp8xFlagAlpha, w, h), {id: "ALPH", payload: alph}, {id: "VP8 ", payload: make([]byte, 10)}}
	default:
		panic("webpOfBitstream: unknown kind " + kind)
	}
	if rest := size - len(webpFile(chunks...)); rest > 0 {
		last := &chunks[len(chunks)-1]
		last.payload = append(last.payload, make([]byte, rest)...)
	}
	return webpFile(chunks...)
}

// TestVP8LHuffmanMemoryIsCharged: a lossless WebP, and a VP8L-compressed
// alpha plane, carry the Huffman-group charge for the bytes the vp8l decoder
// reads — the VP8L chunk, or the ALPH chunk — and not for the rest of the
// file, which the decoder is never handed. So a bitstream up to the line the
// README gives is accepted and one past it refused from the header, whatever
// else the upload carries: the line is 224 KiB for a small picture, about
// 205 KiB at a megapixel and 130 KiB at nine, where the pixels take more of
// the budget, and a little lower in a larger upload.
func TestVP8LHuffmanMemoryIsCharged(t *testing.T) {
	const kib = 1 << 10
	for _, kind := range []string{"lossless", "calpha"} {
		small := webpOfBitstream(kind, 100, 100, 64*kib, 0)
		in, err := inspect(small)
		if err != nil {
			t.Fatalf("%s, 64 KiB of bitstream: inspect = %v, want accepted", kind, err)
		}
		if want := vp8lMemory(64 * kib); in.cost < want {
			t.Errorf("%s, 64 KiB of bitstream: cost %d does not carry the Huffman charge %d", kind, in.cost, want)
		}

		for _, c := range []struct {
			side, n int
			want    error
		}{
			{64, 224 * kib, nil},
			{64, 225 * kib, ErrImageTooLarge},
			{1000, 200 * kib, nil},
			{1000, 215 * kib, ErrImageTooLarge},
			{3000, 128 * kib, nil},
			{3000, 145 * kib, ErrImageTooLarge},
		} {
			if _, err := Inspect(webpOfBitstream(kind, c.side, c.side, c.n, 0)); !errors.Is(err, c.want) {
				t.Errorf("%s, %d×%d, %d KiB of bitstream: Inspect = %v, want %v", kind, c.side, c.side, c.n/kib, err, c.want)
			}
		}

		// A small bitstream in a large upload — a lossless picture with a
		// megabyte of EXIF after it, a cut-out whose lossy picture is large
		// beside its alpha plane — is charged for its bitstream alone.
		big := webpOfBitstream(kind, 100, 100, kib, 4<<20)
		in, err = inspect(big)
		if err != nil {
			t.Fatalf("%s, 1 KiB of bitstream in a 4 MiB upload: inspect = %v, want accepted", kind, err)
		}
		if in.cost >= vp8lGroupBacking {
			t.Errorf("%s, 1 KiB of bitstream in a 4 MiB upload: cost %d carries a charge for the whole file", kind, in.cost)
		}
	}
	// An uncompressed alpha plane carries no charge: it is not decoded through vp8l.
	if in, err := inspect(headerUpload(webpHeader("alpha", 100, 100), 1, 1<<20)); err != nil {
		t.Errorf("uncompressed alpha 1 MiB: inspect = %v, want accepted", err)
	} else if in.cost >= vp8lGroupBacking {
		t.Errorf("uncompressed alpha: cost %d carries the Huffman floor, it should not", in.cost)
	}
}

// A cut-out saved as a lossy WebP is stored however large its lossy picture
// is: its alpha plane, a few hundred bytes to a few kilobytes, is all of it
// the vp8l decoder reads. Charged for the whole file, a cut-out of a quarter
// of a megabyte was refused as too large while the same picture with no
// transparency was stored. So is a small lossless picture behind a large
// colour profile or EXIF, which stripping drops before it is decoded.
func TestLargeWebPsWithSmallVP8LBitstreamsAreStored(t *testing.T) {
	cutout := paddedBitstream(t, readTestdata(t, "cutout-alpha.webp"), 270<<10)
	lossless := webpFile(vp8xChunk(vp8xFlagEXIF, 32, 32),
		webpChunks(t, readTestdata(t, "hidden-lossless.webp"))[0],
		riffChunk{id: "EXIF", payload: make([]byte, 300<<10)})
	for name, data := range map[string][]byte{
		"a cut-out":                            cutout,
		"a lossless icon with 300 KiB of EXIF": lossless,
	} {
		name = fmt.Sprintf("%s (%d KiB)", name, len(data)>>10)
		in, err := inspect(data)
		if err != nil {
			t.Errorf("%s: inspect = %v, want it accepted", name, err)
			continue
		}
		if in.cost >= vp8lGroupBacking {
			t.Errorf("%s: cost %d carries a Huffman charge for the whole file", name, in.cost)
		}
		got, err := Process(context.Background(), data)
		if err != nil {
			t.Errorf("%s: Process = %v, want it stored", name, err)
			continue
		}
		if got.Format != FormatPNG {
			t.Errorf("%s: stored as %s, want png", name, got.Format.Name)
		}
	}
}

// TestVP8LHuffmanMemoryIsHeldWithinTheEstimate decodes crafted lossless WebPs
// whose Huffman groups dominate the heap — one that maximises the group
// backing (65536 one-symbol groups, 160 KiB of file that holds about 170 MiB),
// one that maximises the tree nodes (full 256-symbol trees, 215 KiB holding
// about 43 MiB) — and confirms each is accepted and holds no more than
// cost.go estimated. Both are under the 224 KiB the charge allows a small
// picture, so they are not refused; a larger one would be.
func TestVP8LHuffmanMemoryIsHeldWithinTheEstimate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		groups int
		full   bool
	}{
		{"65536 one-symbol groups", 65536, false},
		{"3000 full-tree groups", 3000, true},
	} {
		data := adversarialLosslessWebP(tc.groups, tc.full)
		in, err := inspect(data)
		if err != nil {
			t.Fatalf("%s (%d bytes): inspect = %v, want accepted", tc.name, len(data), err)
		}
		held := leastPeak(t, tc.name, in.cost, func() error {
			_, err := Process(context.Background(), data)
			return err
		})
		t.Logf("%-26s %6d bytes held %.1f MiB of %.1f", tc.name, len(data),
			float64(held)/(1<<20), float64(in.cost)/(1<<20))
		if held > in.cost {
			t.Errorf("%s: held %d bytes, over the estimate %d", tc.name, held, in.cost)
		}
	}
}
