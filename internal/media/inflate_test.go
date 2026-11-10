package media

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/adler32"
	"image/png"
	"io"
	"math"
	"math/bits"
	"math/rand"
	"runtime"
	"testing"
	"time"
)

// flateOutcome is what compress/zlib and compress/flate make of a zlib
// stream, classed as a walk classes it (inflateWalk), and how many bytes it
// inflated to: io.EOF at the end of its last block, io.ErrUnexpectedEOF where
// the input ends before it does, and errStreamCorrupt where compress/zlib
// refuses its header or compress/flate a block. The header is checked as
// compress/zlib checks it, here, so that its checksum, which the walk does
// not read, is not.
func flateOutcome(stream []byte) (error, int64) {
	if len(stream) < 2 {
		return io.ErrUnexpectedEOF, 0
	}
	if stream[0]&0x0F != 8 || stream[0]>>4 > 7 || binary.BigEndian.Uint16(stream)%31 != 0 {
		return errStreamCorrupt, 0
	}
	head := 2
	if stream[1]&0x20 != 0 {
		if len(stream) < 6 {
			return io.ErrUnexpectedEOF, 0
		}
		if binary.BigEndian.Uint32(stream[2:]) != 1 {
			return errStreamCorrupt, 0
		}
		head = 6
	}
	n, err := io.Copy(io.Discard, flate.NewReader(bytes.NewReader(stream[head:])))
	var corrupt flate.CorruptInputError
	switch {
	case err == nil:
		return io.EOF, n
	case errors.As(err, &corrupt):
		return errStreamCorrupt, n
	}
	return err, n
}

// walked is a walk over stream with no limit: the tables it counted, the
// bytes the stream inflated to, and how it ended.
func walked(stream []byte) (tables, out int64, err error) {
	w := &inflateWalk{ctx: context.Background(), cur: stream, next: -1}
	err = w.walk(math.MaxInt64)
	return w.tables, w.out, err
}

// deflatedAt is data deflated by compress/zlib at level.
func deflatedAt(t testing.TB, data []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	z, err := zlib.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := z.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// endBlocks is a zlib stream of data in its last block, a stored one, after
// as many dynamic blocks as make it at least size bytes long that each hold
// no more than their end, under a literal/length code and a distance code of
// the given lengths, run-length coded as the format allows (RFC 1951 3.2.7),
// under a code-length code as short as covers the symbols that takes.
func endBlocks(size int, lit, dist []int, data []byte) []byte {
	var (
		out  = []byte{0x78, 0x01}
		acc  uint64
		have uint
	)
	put := func(v uint64, n uint) { // least significant bit first
		acc |= v << have
		for have += n; have >= 8; have -= 8 {
			out, acc = append(out, byte(acc)), acc>>8
		}
	}
	// codes are the canonical codes of the given lengths (RFC 1951 3.2.2),
	// and code writes one, most significant bit first.
	codes := func(lens []int) []int {
		var count, next [16]int
		for _, l := range lens {
			if l > 0 {
				count[l]++
			}
		}
		for l, c := 2, 0; l < 16; l++ {
			c = (c + count[l-1]) << 1
			next[l] = c
		}
		out := make([]int, len(lens))
		for sym, l := range lens {
			if l > 0 {
				out[sym] = next[l]
				next[l]++
			}
		}
		return out
	}
	code := func(c, n int) { put(uint64(bits.Reverse16(uint16(c)))>>(16-n), uint(n)) }
	// The lengths, run-length coded: runs of zeros as 17 or 18, and of
	// another length as it and 16s, and what is left over one by one.
	type rl struct{ sym, extra, bits int }
	var seq []rl
	all := append(append([]int(nil), lit...), dist...)
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j] == all[i] {
			j++
		}
		l, run := all[i], j-i
		if l != 0 {
			seq = append(seq, rl{l, 0, 0})
			run--
			for ; run >= 3; run -= min(run, 6) {
				seq = append(seq, rl{16, min(run, 6) - 3, 2})
			}
		} else {
			for ; run >= 11; run -= min(run, 138) {
				seq = append(seq, rl{18, min(run, 138) - 11, 7})
			}
			for ; run >= 3; run -= min(run, 10) {
				seq = append(seq, rl{17, min(run, 10) - 3, 3})
			}
		}
		for ; run > 0; run-- {
			seq = append(seq, rl{l, 0, 0})
		}
		i = j
	}
	// The code-length code: of the k symbols seq takes, 2^d-k of d-1 bits
	// and the rest of d, which is complete, or one of one bit.
	var used []int
	for sym := 0; sym < 19; sym++ {
		for _, x := range seq {
			if x.sym == sym {
				used = append(used, sym)
				break
			}
		}
	}
	clen := make([]int, 19)
	d := bits.Len(uint(len(used) - 1))
	for i, sym := range used {
		clen[sym] = d
		if i < 1<<d-len(used) {
			clen[sym] = d - 1
		}
	}
	if len(used) == 1 {
		clen[used[0]] = 1
	}
	order := []int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}
	nclen := 4
	for i, sym := range order {
		if clen[sym] > 0 {
			nclen = max(nclen, i+1)
		}
	}
	clenCodes, litCodes := codes(clen), codes(lit)
	for len(out) < size {
		put(0b100, 3) // not the last; dynamic codes
		put(uint64(len(lit)-257), 5)
		put(uint64(len(dist)-1), 5)
		put(uint64(nclen-4), 4)
		for _, sym := range order[:nclen] {
			put(uint64(clen[sym]), 3)
		}
		for _, x := range seq {
			code(clenCodes[x.sym], clen[x.sym])
			put(uint64(x.extra), uint(x.bits))
		}
		code(litCodes[256], lit[256])
	}
	put(1, 3) // the last block; stored
	if have > 0 {
		put(0, 8-have)
	}
	out = binary.LittleEndian.AppendUint16(out, uint16(len(data)))
	out = binary.LittleEndian.AppendUint16(out, ^uint16(len(data)))
	return binary.BigEndian.AppendUint32(append(out, data...), adler32.Checksum(data))
}

// cheapCodes are the lengths of codes that compress/flate makes no tables
// for, and endBlocks under them: blocks of codes of one bit, an end of
// block and two distances, a dozen bytes each, the shortest dynamic blocks
// there are but for a few bits; and blocks of a literal/length code whose
// literals are of nine bits and whose end is of one, about 30 bytes each.
// Neither makes tables, so a walk lets through as many as an upload can
// hold, and they cost more CPU for each of their bytes than any other
// blocks found that it lets through.
func cheapCodes() map[string]func(size int, data []byte) []byte {
	oneBit := make([]int, 257)
	oneBit[256] = 1
	nineBits := make([]int, 257)
	for i := range nineBits {
		nineBits[i] = 9
	}
	nineBits[256] = 1
	return map[string]func(int, []byte) []byte{
		"blocks of one-bit codes": func(size int, data []byte) []byte {
			return endBlocks(size, oneBit, []int{1, 1}, data)
		},
		"blocks of nine-bit codes": func(size int, data []byte) []byte {
			return endBlocks(size, nineBits, []int{0}, data)
		},
	}
}

// walkStreams are zlib streams of the kinds a walk has to read as
// compress/flate does: Go's encoder at each of its levels, Huffman only
// among them, on a photo-like picture's rows, on smooth rows and on noise;
// dynamic blocks of nothing but costly codes (costlyStream), and of nothing
// but codes that make no tables (cheapCodes), which a walk reads a bit at a
// time; and stored blocks, a block of fixed codes, and a header with the
// empty preset dictionary.
func walkStreams(t testing.TB) map[string][]byte {
	t.Helper()
	streams := map[string][]byte{}
	pic := photo(320, 240)
	var rows, smooth []byte
	for y := 0; y < 240; y++ {
		rows = append(rows, 0)
		smooth = append(smooth, 2)
		for x := 0; x < 320; x++ {
			i := pic.PixOffset(x, y)
			rows = append(rows, pic.Pix[i:i+3]...)
			smooth = append(smooth, byte(x/3), byte(y/2), byte((x+y)/5))
		}
	}
	noise := make([]byte, 64<<10)
	rand.New(rand.NewSource(1)).Read(noise)
	for name, data := range map[string][]byte{"photo": rows, "smooth": smooth, "noise": noise} {
		for _, level := range []int{zlib.HuffmanOnly, 1, 2, 3, 4, 5, 6, 7, 8, 9, zlib.NoCompression} {
			streams[fmt.Sprintf("%s at level %d", name, level)] = deflatedAt(t, data, level)
		}
	}
	streams["costly blocks"] = costlyStream(64<<10, []byte("a picture's rows"))
	for name, blocks := range cheapCodes() {
		streams[name] = blocks(16<<10, []byte("a picture's rows"))
	}
	streams["stored blocks"] = storedStream(100, []byte("a picture's rows"), 100)
	// One block of fixed codes holding "abc" and a length back to it, and
	// the same with a preset dictionary, the empty one.
	fixed := []byte{0x78, 0x9C, 0x4B, 0x4C, 0x4A, 0x4E, 0x04, 0x22, 0x00}
	streams["fixed codes"] = fixed
	streams["empty preset dictionary"] = append([]byte{0x78, 0xBB, 0, 0, 0, 1}, fixed[2:]...)
	return streams
}

// A walk reads a zlib stream as compress/zlib and compress/flate do: it ends
// at the same place, the end of the last block, the end of the input or the
// first thing either refuses, having inflated it to as many bytes, whatever
// encoder made it, and whatever it is cut short or broken by. It is what
// tells whether a PNG's image data would make more tables than its decode is
// charged for (pngImageDataTables), so a stream it read otherwise would be
// counted for blocks compress/flate never reads, or not for blocks it does.
// Every stream is walked whole, then broken at random 5,000 times: a bit
// turned, a few bytes changed, the stream cut, or a bit turned in its first
// 64 bytes, where its header and its first codes are.
func TestTheWalkReadsStreamsAsCompressFlateDoes(t *testing.T) {
	streams := walkStreams(t)
	var seeds [][]byte
	for name, s := range streams {
		_, out, err := walked(s)
		want, n := flateOutcome(s)
		if err != want || out != n {
			t.Errorf("%s: the walk ended with %v at %d bytes, compress/flate with %v at %d", name, err, out, want, n)
		}
		if err != io.EOF {
			t.Errorf("%s: the walk ended with %v, want the stream's end", name, err)
		}
		seeds = append(seeds, s[:min(len(s), 6000)])
	}
	rng := rand.New(rand.NewSource(7))
	outcomes := map[error]int{}
	for i := 0; i < 5000; i++ {
		s := append([]byte(nil), seeds[rng.Intn(len(seeds))]...)
		switch rng.Intn(4) {
		case 0:
			s[rng.Intn(len(s))] ^= 1 << rng.Intn(8)
		case 1:
			for k := rng.Intn(4) + 1; k > 0; k-- {
				s[rng.Intn(len(s))] = byte(rng.Intn(256))
			}
		case 2:
			s = s[:rng.Intn(len(s)+1)]
		case 3:
			s[rng.Intn(min(len(s), 64))] ^= 1 << rng.Intn(8)
		}
		_, out, err := walked(s)
		want, n := flateOutcome(s)
		outcomes[want]++
		if err != want || want == io.EOF && out != n {
			t.Fatalf("broken stream %d (%x): the walk ended with %v at %d bytes, compress/flate with %v at %d", i, s, err, out, want, n)
		}
	}
	t.Logf("compress/flate's outcomes over the broken streams: %v", outcomes)
	for _, want := range []error{io.EOF, io.ErrUnexpectedEOF, errStreamCorrupt} {
		if outcomes[want] == 0 {
			t.Errorf("no broken stream ended with %v: the streams do not try that ending", want)
		}
	}
}

// What a walk counts of the tables compress/flate makes for a stream is what
// inflating it allocates for them, size class and all, and never less: it is
// the bound on what a collection can find of them (pngTablesAllowed). What
// inflating allocates otherwise, the inflater and its window, is the same
// for every stream, and is taken off as what inflating a stream of one
// stored block allocates; the 16 KiB allowed either way is for what that
// measure does not hold still, about 8 KiB under -race. The count may run
// over by more for codes of ten bits, whose tables of two entries the
// allocator gives a block of 16 bytes it shares with another but under the
// race detector, which the count takes as 16 bytes each: by a quarter at
// most.
func TestTheWalkCountsTheTablesCompressFlateMakes(t *testing.T) {
	inflating := func(s []byte) int64 {
		runtime.GC()
		before := heapAllocs()
		z, err := zlib.NewReader(bytes.NewReader(s))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, z); err != nil {
			t.Fatal(err)
		}
		runtime.GC() // counts what the allocator's caches still held
		return heapAllocs() - before
	}
	base := inflating(storedStream(0, []byte("x"), 0))
	const noise = 16 << 10
	pic := photo(640, 480)
	var rows []byte
	for y := 0; y < 480; y++ {
		i := pic.PixOffset(0, y)
		rows = append(append(rows, 1), pic.Pix[i:i+640*4]...)
	}
	for _, c := range []struct {
		name   string
		stream []byte
		exact  bool
	}{
		{"1 MiB of costly blocks", costlyStream(1<<20, nil), true},
		{"a photo at level 1", deflatedAt(t, rows, 1), false},
		{"a photo at level 6", deflatedAt(t, rows, 6), false},
		{"a photo, Huffman only", deflatedAt(t, rows, zlib.HuffmanOnly), false},
	} {
		counted, _, err := walked(c.stream)
		if err != io.EOF {
			t.Fatalf("%s: the walk ended with %v", c.name, err)
		}
		made := inflating(c.stream) - base
		t.Logf("%-24s counted %9d bytes of tables, inflating allocated %9d more", c.name, counted, made)
		if counted < 64<<10 {
			t.Errorf("%s: counted %d bytes of tables, too few for the measure to tell", c.name, counted)
		}
		if counted < made-noise {
			t.Errorf("%s: counted %d bytes of tables, under the %d inflating allocated", c.name, counted, made)
		}
		over := made / 4
		if c.exact {
			over = 0
		}
		if counted > made+over+noise {
			t.Errorf("%s: counted %d bytes of tables, over the %d inflating allocated", c.name, counted, made)
		}
	}
}

// A walk makes nothing of the stream as it reads it: what it allocates is
// its own state, the same however many blocks it reads.
func TestTheWalkAllocatesNothingAsItGoes(t *testing.T) {
	short, long := costlyStream(1<<10, nil), costlyStream(256<<10, nil)
	for name, s := range map[string][]byte{"a short stream": short, "a long one": long} {
		if n := testing.AllocsPerRun(5, func() { walked(s) }); n > 1 {
			t.Errorf("walking %s allocated %v times, want once, for its state", name, n)
		}
	}
}

// A PNG's image data is walked as image/png reads it: the data of the IDAT
// chunks that follow one another from the first, empty ones among them, as
// one stream, up to the first chunk of another kind, and no further than the
// file holds. A walk over a PNG split that way counts what a walk over its
// stream whole does, and what image/png decodes from it is the picture.
func TestPNGImageDataIsReadAsImagePNGReadsIt(t *testing.T) {
	// A 64×48 RGBA picture's rows, unfiltered, after costly blocks, so that
	// there are tables to count, in four chunks, one of them empty, then a
	// tEXt chunk, then an IDAT chunk image/png does not read.
	const w, h = 64, 48
	pic := photo(w, h)
	var rows []byte
	for y := 0; y < h; y++ {
		i := pic.PixOffset(0, y)
		rows = append(append(rows, 0), pic.Pix[i:i+4*w]...)
	}
	stream := costlyStream(4<<10, rows)
	head := pngHeader(w, h, 6, 8, false)
	var split []byte
	split = append(split, head...)
	for _, p := range [][]byte{stream[:1000], nil, stream[1000:3000], stream[3000:]} {
		split = append(split, pngChunk("IDAT", p)...)
	}
	split = append(split, pngChunk("tEXt", []byte("k\x00v"))...)
	split = append(split, pngChunk("IDAT", costlyStream(64<<10, nil))...)
	split = append(split, pngChunk("IEND", nil)...)

	if n, want := pngImageDataBytes(split), int64(len(stream)); n != want {
		t.Errorf("pngImageDataBytes = %d, want the %d bytes of the four chunks before the tEXt", n, want)
	}
	counted, _, err := walked(stream)
	if err != io.EOF || counted == 0 {
		t.Fatalf("the stream whole: the walk counted %d and ended with %v, want tables and its end", counted, err)
	}
	if got, err := pngImageDataTables(context.Background(), split, math.MaxInt64); got != counted || err != nil {
		t.Errorf("the PNG's image data: the walk counted %d (%v), want %d, as for its stream whole", got, err, counted)
	}
	if _, err := png.Decode(bytes.NewReader(split)); err != nil {
		t.Errorf("image/png does not decode the PNG as split: %v", err)
	}
	// Cut short in its last IDAT chunk before the tEXt, the file holds what
	// it holds of that chunk, and the walk stops there without error,
	// having counted no more than the whole.
	cut := split[:len(head)+3*12+8+3000+500]
	if n, want := pngImageDataBytes(cut), int64(3500); n != want {
		t.Errorf("cut short: pngImageDataBytes = %d, want %d", n, want)
	}
	if got, err := pngImageDataTables(context.Background(), cut, math.MaxInt64); got > counted || err != nil {
		t.Errorf("cut short: the walk counted %d (%v), want no more than %d and no error", got, err, counted)
	}
	// With no IDAT chunk, there is nothing to walk.
	if n := pngImageDataBytes(head); n != 0 {
		t.Errorf("a header alone: pngImageDataBytes = %d, want 0", n)
	}
}

// A walk costs about as much CPU as the inflating it comes before, or less,
// over a stream of any kind, so that walking first adds no more than about
// as much again to what inflating a PNG's image data costs: no more than a
// quarter over it is allowed here, timed on the walk's thread, the best of
// three, over a photo's rows as Go's encoder deflates them, and over a
// quarter of a MiB of blocks under costly codes (costlyStream), of which a
// walk lets through only a few, and of blocks that hold nothing but their
// end under codes that make no tables (cheapCodes), which cost the most for
// their bytes of what it lets through. Over those a walk reads every code a
// bit at a time and builds no table for it (buildAfter); building them cost
// it a fifth more than inflating did over blocks of one-bit codes, where it
// now costs a third as much.
func TestAWalkCostsNoMoreThanInflating(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pic := photo(640, 480)
	var rows []byte
	for y := 0; y < 480; y++ {
		i := pic.PixOffset(0, y)
		rows = append(append(rows, 1), pic.Pix[i:i+640*4]...)
	}
	streams := map[string][]byte{
		"costly blocks":  costlyStream(256<<10, nil),
		"a photo's rows": deflatedAt(t, rows, zlib.DefaultCompression),
	}
	for name, blocks := range cheapCodes() {
		streams[name] = blocks(256<<10, nil)
	}
	best := func(f func()) time.Duration {
		d := time.Duration(math.MaxInt64)
		for i := 0; i < 3; i++ {
			runtime.GC()
			began := threadCPU()
			f()
			d = min(d, threadCPU()-began)
		}
		return d
	}
	for name, s := range streams {
		if len(s) < 256<<10 {
			t.Fatalf("%s: %d bytes, want a quarter of a MiB or more", name, len(s))
		}
		walk := best(func() {
			if _, _, err := walked(s); err != io.EOF {
				t.Fatalf("%s: the walk ended with %v", name, err)
			}
		})
		inflate := best(func() {
			if err, _ := flateOutcome(s); err != io.EOF {
				t.Fatalf("%s: inflating ended with %v", name, err)
			}
		})
		t.Logf("%-26s %8d bytes: walked in %v, inflated in %v", name, len(s), walk, inflate)
		if walk > inflate+inflate/4 {
			t.Errorf("%s: walked in %v of CPU, inflated in %v; want the walk to cost no more than a quarter over", name, walk, inflate)
		}
	}
}

// A walk reads the codes of a block a bit at a time until it has read
// buildAfter symbols of them, and only then builds a table for them: over
// blocks that hold nothing but their end, as a stream made to cost CPU
// does, it builds none for their literal/length codes, nor for the codes
// their lengths are given in where those are run-length coded (cheapCodes),
// and over a photo's blocks it builds one for each literal/length code.
func TestAWalkBuildsTablesOnlyForCodesOfManySymbols(t *testing.T) {
	pic := photo(320, 240)
	var rows []byte
	for y := 0; y < 240; y++ {
		i := pic.PixOffset(0, y)
		rows = append(append(rows, 1), pic.Pix[i:i+320*4]...)
	}
	type want struct{ lit, clen bool } // whether each has a table built
	streams := map[string]struct {
		stream []byte
		want   *want // nil for not looked at
	}{
		"a photo's rows": {deflatedAt(t, rows, zlib.DefaultCompression), nil},
		// Their lengths are given one by one, 316 symbols.
		"costly blocks": {costlyStream(16<<10, nil), &want{false, true}},
	}
	for name, blocks := range cheapCodes() {
		streams[name] = struct {
			stream []byte
			want   *want
		}{blocks(16<<10, nil), &want{false, false}}
	}
	for name, c := range streams {
		w := &inflateWalk{ctx: context.Background(), cur: c.stream, next: -1}
		if err := w.walk(math.MaxInt64); err != io.EOF {
			t.Fatalf("%s: the walk ended with %v", name, err)
		}
		// The codes left are those of the last dynamic block.
		got := want{w.lit.built, w.clen.built}
		if c.want == nil {
			if !got.lit {
				t.Errorf("%s: the last dynamic block's literal/length code has no table built", name)
			}
		} else if got != *c.want {
			t.Errorf("%s: the last dynamic block's literal/length and code-length codes have tables built: %v, want %v", name, got, *c.want)
		}
	}
}
