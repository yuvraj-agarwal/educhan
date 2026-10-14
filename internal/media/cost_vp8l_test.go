package media

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

// golang.org/x/image's vp8l decoder sizes two allocations from the compressed
// bitstream rather than from the header: an hGroup for every meta-image index
// (up to 65536, 2680 bytes each) and a node slice for every Huffman tree (up
// to 2*alphabet-1 nodes). A crafted lossless WebP of a few hundred kilobytes
// makes it hold over a gigabyte, which no resolution cap catches. cost.go
// charges that memory (vp8lMemory) so Inspect refuses an oversized file from
// its header, and these tests pin that it does and that the charge covers
// what is held. See the probe in the branch history for the full 1 GiB file.

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

// TestVP8LHuffmanMemoryIsCharged: a lossless WebP, and a VP8L-compressed
// alpha plane, carry the Huffman-group floor in their estimate, so a small
// one is accepted and a large one refused from its header, whatever its
// resolution.
func TestVP8LHuffmanMemoryIsCharged(t *testing.T) {
	for _, kind := range []string{"lossless", "calpha"} {
		small := headerUpload(webpHeader(kind, 100, 100), 1, 64<<10)
		in, err := inspect(small)
		if err != nil {
			t.Fatalf("%s 64 KiB: inspect = %v, want accepted", kind, err)
		}
		if want := vp8lMemory(int64(len(small))); in.cost < want {
			t.Errorf("%s 64 KiB: cost %d does not carry the Huffman charge %d", kind, in.cost, want)
		}
		big := headerUpload(webpHeader(kind, 100, 100), 1, 1<<20)
		if _, err := Inspect(big); !errors.Is(err, ErrImageTooLarge) {
			t.Errorf("%s 1 MiB: Inspect = %v, want ErrImageTooLarge", kind, err)
		}
	}
	// An uncompressed alpha plane carries no floor: it is not decoded through vp8l.
	if in, err := inspect(headerUpload(webpHeader("alpha", 100, 100), 1, 1<<20)); err != nil {
		t.Errorf("uncompressed alpha 1 MiB: inspect = %v, want accepted", err)
	} else if in.cost >= vp8lGroupBacking {
		t.Errorf("uncompressed alpha: cost %d carries the Huffman floor, it should not", in.cost)
	}
}

// TestVP8LHuffmanMemoryIsHeldWithinTheEstimate decodes crafted lossless WebPs
// whose Huffman groups dominate the heap — one that maximises the group
// backing (65536 one-symbol groups), one that maximises the tree nodes (full
// 256-symbol trees) — and confirms each is accepted and holds no more than
// cost.go estimated. Both are under the third-of-a-megabyte the charge
// allows, so they are not refused; a larger one would be.
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
