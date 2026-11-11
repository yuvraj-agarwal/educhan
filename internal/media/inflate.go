package media

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/bits"
)

// Inflating a PNG's image data makes garbage that a collection can keep: for
// every dynamic block whose literal/length or distance code runs past nine
// bits, compress/flate makes a table for each of the first nine bits that
// lead to a longer code, and a slice to hold them, and lets them go at the
// block's end. A block that holds nothing but such codes (costlyStream) is a
// few dozen to 168 bytes and makes up to 41 KiB of them, hundreds of times
// its bytes, and a collection marks live everything made while it runs, for
// as long as it runs, which grows with the rest of the heap: beside 128 MiB
// of live objects, a picture of one pixel and 8 MiB of such blocks held 10.9
// to 16.7 MiB as it decoded, where 8 MiB was charged for it. So before a
// PNG's image data is inflated it is walked (pngImageDataTables), counting
// what compress/flate would make of it without making any of it, and an
// upload whose image data would make more than the decode is charged for
// (pngTablesAllowed) is refused, as is one whose ICC profile, inflated after
// the decode, would make more than what the image data left of that
// (pngProfileUsable). What a decode holds of the tables is then never more
// than were made, whatever the collector does.
//
// The walk reads every block's codes and symbols, as the inflating after it
// does, at about as much CPU or less (TestAWalkCostsNoMoreThanInflating):
// zlib's streams of photographs and of text at 100 to 120 MB/s on one core,
// where compress/flate inflated them at 50 to 90, and a photo's rows as Go's
// encoder deflates them a little faster than compress/flate. A stream made to
// cost CPU costs most where its blocks are shortest: blocks of a dozen bytes
// that hold nothing but their end, under codes of a bit each that make no
// tables (cheapCodes), which compress/flate inflated at about 0.14 s of CPU a
// MiB on one core and the walk walked at about 0.05 s a MiB, reading their few
// codes a bit at a time rather than building tables for them (buildAfter).

// errTooManyTables is a zlib stream whose dynamic blocks would have
// compress/flate make more tables than a walk allows.
var errTooManyTables = errors.New("zlib stream makes too many tables")

// errStreamCorrupt is a zlib stream compress/zlib or compress/flate would
// stop at as corrupt: a header either refuses, or a deflate block it cannot
// read.
var errStreamCorrupt = errors.New("corrupt zlib stream")

// pngImageDataTables walks a PNG's image data as image/png reads it, the
// data of the IDAT chunks that follow one another from the first, as one
// zlib stream (pngFirstIDAT, pngIDATAt), and returns the bytes of the tables
// compress/flate would make inflating it (inflateWalk). It stops with
// errTooManyTables as soon as they would come to more than limit, before the
// block that takes them past it, and with ctx's error once ctx has ended,
// and otherwise returns nil, whether the stream ends, breaks or stops short:
// those are for image/png to find as it reads it. Chunks' checksums are not
// read, so a stream image/png gives up on at a checksum is walked on past it.
func pngImageDataTables(ctx context.Context, data []byte, limit int64) (int64, error) {
	w := &inflateWalk{ctx: ctx, png: data, next: pngFirstIDAT(data)}
	return w.tables, w.outcome(w.walk(limit))
}

// zlibTables is pngImageDataTables for a zlib stream on its own: an iCCP
// chunk's, which compress/zlib inflates from what pngProfileUsable cuts it
// to.
func zlibTables(ctx context.Context, stream []byte, limit int64) (int64, error) {
	w := &inflateWalk{ctx: ctx, cur: stream, next: -1}
	return w.tables, w.outcome(w.walk(limit))
}

// outcome is what a walk that ended with err returns: the error for running
// past the limit or ctx's, and nil for the rest.
func (w *inflateWalk) outcome(err error) error {
	if err == errTooManyTables {
		return err
	}
	return w.halt
}

// pngFirstIDAT is where a PNG's first IDAT chunk starts, its length's first
// byte, or -1 when its chunks cannot be followed that far.
func pngFirstIDAT(data []byte) int {
	i := 8 // the signature
	for i+8 <= len(data) {
		if string(data[i+4:i+8]) == "IDAT" {
			return i
		}
		n := int64(binary.BigEndian.Uint32(data[i : i+4]))
		if n > int64(len(data)-i-12) {
			return -1
		}
		i += 12 + int(n)
	}
	return -1
}

// pngIDATAt reads the chunk at i, when it is an IDAT chunk, as image/png's
// decoder reads one as it inflates: its data, as much of it as the file
// holds, and where the chunk after it starts, past its checksum. ok is false
// when there is no IDAT chunk at i, which ends the image data.
func pngIDATAt(data []byte, i int) (piece []byte, next int, ok bool) {
	if i < 0 || i+8 > len(data) || string(data[i+4:i+8]) != "IDAT" {
		return nil, -1, false
	}
	n := int64(binary.BigEndian.Uint32(data[i : i+4]))
	if n > int64(len(data)-i-8) {
		return data[i+8:], -1, true
	}
	return data[i+8 : i+8+int(n)], i + 12 + int(n), true
}

// pngImageDataBytes is how many bytes a PNG's image data is, as image/png
// reads it (pngIDATAt).
func pngImageDataBytes(data []byte) int64 {
	var n int64
	for i := pngFirstIDAT(data); ; {
		piece, next, ok := pngIDATAt(data, i)
		if !ok {
			return n
		}
		n += int64(len(piece))
		i = next
	}
}

// inflateWalk is a walk over a zlib stream as compress/zlib and compress/flate
// (Go 1.22 to 1.25) read it: the header as compress/zlib checks it, and each
// block's codes, symbols, lengths and distances as compress/flate decodes
// them, failing where it would — but nothing written out, and nothing of the
// stream's checksum read. It counts the tables compress/flate makes for each
// dynamic block's codes (flateCode.init) and stops before the block that takes
// them past the walk's limit. It allocates nothing as it goes, and asks ctx
// whether to go on before its first block and every so often after (halted).
// walk returns io.EOF at the end of the last block, io.ErrUnexpectedEOF where
// the input ends before the stream does, errStreamCorrupt where compress/zlib
// or compress/flate would stop at any other error, errTooManyTables past the
// limit, and ctx's error once it has ended, having counted what compress/flate
// would have made by then.
type inflateWalk struct {
	ctx  context.Context
	cur  []byte // what is left of the piece of the stream being read
	png  []byte // for a PNG's image data, the file, whose IDAT chunks
	next int    // go on from here (pngIDATAt), or -1 for no more pieces

	b      uint64 // bits read and not yet used, the next lowest
	nb     uint   // how many bits b holds
	out    int64  // bytes the stream has inflated to so far
	tables int64  // bytes of tables compress/flate would have made so far
	work   int    // symbols read since ctx was last asked, and blocks, weighted
	halt   error  // ctx's error, once it has given one

	lit, dist, clen flateCode
	lens            [maxLitCodes + maxDistCodes]uint8
}

const (
	maxLitCodes  = 286 // literal/length codes a dynamic block may give (RFC 1951 3.2.7)
	maxDistCodes = 30  // distance codes, likewise
)

// haltEveryWork is how much work a walk does between asks of its context:
// 65536 symbols read, each block's header weighing as much as 256 of them.
// That came to half a millisecond of CPU at most, on one core, between asks
// of walks over every kind of stream made to cost CPU, and to a third of one
// over a photo's.
const (
	haltEveryWork = 1 << 16
	blockWork     = 256
)

// fixedLit is the literal/length code of a block of fixed Huffman codes (RFC
// 1951 3.2.6), for which compress/flate makes its tables once, when it
// first reads one.
var fixedLit = func() *flateCode {
	var lens [288]uint8
	for i := range lens {
		switch {
		case i < 144:
			lens[i] = 8
		case i < 256:
			lens[i] = 9
		case i < 280:
			lens[i] = 7
		default:
			lens[i] = 8
		}
	}
	c := new(flateCode)
	c.init(lens[:])
	c.build() // so that walks only ever read it
	return c
}()

// halted asks ctx whether to go on (ask) once work's worth more has been
// done since it last asked, and returns its error once it has ended.
func (w *inflateWalk) halted(work int) error {
	if w.work += work; w.work < haltEveryWork {
		return nil
	}
	return w.ask()
}

// ask asks ctx whether to go on, and returns its error once it has ended.
func (w *inflateWalk) ask() error {
	w.work = 0
	w.halt = w.ctx.Err()
	return w.halt
}

// walk reads the stream's header and its blocks, to the end of the last.
func (w *inflateWalk) walk(limit int64) error {
	// RFC 1950 2.2, as compress/zlib checks it: deflate, a window of 32 KiB
	// at most, and a check that holds. A preset dictionary is taken only if
	// it is the empty one, which is the one compress/zlib is given.
	var head [2]byte
	for i := range head {
		c, err := w.byte()
		if err != nil {
			return err
		}
		head[i] = c
	}
	if head[0]&0x0F != 8 || head[0]>>4 > 7 || binary.BigEndian.Uint16(head[:])%31 != 0 {
		return errStreamCorrupt
	}
	if head[1]&0x20 != 0 {
		var id uint32
		for i := 0; i < 4; i++ {
			c, err := w.byte()
			if err != nil {
				return err
			}
			id = id<<8 | uint32(c)
		}
		if id != 1 { // adler32 of nothing
			return errStreamCorrupt
		}
	}
	w.work = haltEveryWork // to ask before the first block
	for {
		if err := w.halted(blockWork); err != nil {
			return err
		}
		if err := w.need(3); err != nil {
			return err
		}
		final := w.b&1 == 1
		typ := w.b >> 1 & 3
		w.use(3)
		var err error
		switch typ {
		case 0:
			err = w.storedBlock()
		case 1:
			err = w.codedBlock(fixedLit, nil)
		case 2:
			if err = w.codes(limit); err == nil {
				err = w.codedBlock(&w.lit, &w.dist)
			}
		default:
			err = errStreamCorrupt
		}
		if err != nil {
			return err
		}
		if final {
			return io.EOF
		}
	}
}

// codes reads a dynamic block's codes (RFC 1951 3.2.7) and counts the tables
// compress/flate makes for them, failing where it would.
func (w *inflateWalk) codes(limit int64) error {
	if err := w.need(14); err != nil {
		return err
	}
	nlit := int(w.b&0x1F) + 257
	ndist := int(w.b>>5&0x1F) + 1
	nclen := int(w.b>>10&0xF) + 4
	if nlit > maxLitCodes || ndist > maxDistCodes {
		return errStreamCorrupt
	}
	w.use(14)
	order := [19]uint8{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}
	var clens [19]uint8
	for i := 0; i < nclen; i++ {
		if err := w.need(3); err != nil {
			return err
		}
		clens[order[i]] = uint8(w.b & 7)
		w.use(3)
	}
	if !w.clen.init(clens[:]) {
		return errStreamCorrupt
	}
	lens := w.lens[:nlit+ndist]
	for i := 0; i < len(lens); {
		x, err := w.sym(&w.clen)
		if err != nil {
			return err
		}
		if x < 16 {
			lens[i] = uint8(x)
			i++
			continue
		}
		var rep int
		var nb uint
		var v uint8
		switch x {
		case 16:
			if i == 0 {
				return errStreamCorrupt
			}
			rep, nb, v = 3, 2, lens[i-1]
		case 17:
			rep, nb = 3, 3
		default: // 18
			rep, nb = 11, 7
		}
		if err := w.need(nb); err != nil {
			return err
		}
		rep += int(w.b & (1<<nb - 1))
		w.use(nb)
		if i+rep > len(lens) {
			return errStreamCorrupt
		}
		for ; rep > 0; rep-- {
			lens[i] = v
			i++
		}
	}
	// compress/flate makes the literal/length code's tables before it
	// tries the distance code, and does not try that if the first fails.
	if !w.lit.init(lens[:nlit]) {
		return errStreamCorrupt
	}
	// Every block ends with its end-of-block code, so compress/flate reads
	// at least as many bits as that before it looks up a literal or length.
	w.lit.min = max(w.lit.min, uint(lens[256]))
	if w.tables += w.lit.tables; w.tables > limit {
		return errTooManyTables
	}
	if !w.dist.init(lens[nlit:]) {
		return errStreamCorrupt
	}
	if w.tables += w.dist.tables; w.tables > limit {
		return errTooManyTables
	}
	return nil
}

// codedBlock reads the symbols of a block of Huffman codes to its end, with
// dist nil for fixed codes, failing where compress/flate would.
func (w *inflateWalk) codedBlock(lit, dist *flateCode) error {
	for {
		v, err := w.sym(lit)
		if err != nil {
			return err
		}
		if v < 256 {
			w.out++
			continue
		}
		if v == 256 {
			return nil
		}
		// A length (RFC 1951 3.2.5), from 3 to 258, and extra bits for
		// it; 286 and 287 are not lengths.
		if v >= maxLitCodes {
			return errStreamCorrupt
		}
		length := 258
		if v < 285 {
			n := uint(0)
			if v >= 265 {
				n = uint(v-261) / 4
			}
			length = 3 + int(lengthBase[v-257])
			if err := w.need(n); err != nil {
				return err
			}
			length += int(w.b & (1<<n - 1))
			w.use(n)
		}
		var d int
		if dist == nil {
			if err := w.need(5); err != nil {
				return err
			}
			d = int(bits.Reverse8(uint8(w.b&0x1F) << 3))
			w.use(5)
		} else if d, err = w.sym(dist); err != nil {
			return err
		}
		switch {
		case d < 4:
			d++
		case d < maxDistCodes:
			n := uint(d-2) >> 1
			extra := (d & 1) << n
			if err := w.need(n); err != nil {
				return err
			}
			extra |= int(w.b & (1<<n - 1))
			w.use(n)
			d = 1<<(n+1) + 1 + extra
		default:
			return errStreamCorrupt
		}
		// compress/flate refuses a distance back past what it has
		// written, or past its window of 32 KiB.
		if int64(d) > min(w.out, 1<<15) {
			return errStreamCorrupt
		}
		w.out += int64(length)
	}
}

// lengthBase is what each length code from 257 to 284 adds to 3 before its
// extra bits (RFC 1951 3.2.5).
var lengthBase = [28]uint8{
	0, 1, 2, 3, 4, 5, 6, 7, 8, 10, 12, 14, 16, 20, 24, 28,
	32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224,
}

// storedBlock skips a stored block (RFC 1951 3.2.4), from the next byte.
func (w *inflateWalk) storedBlock() error {
	w.use(w.nb % 8)
	var lens [4]byte
	for i := range lens {
		c, err := w.byte()
		if err != nil {
			return err
		}
		lens[i] = c
	}
	n := binary.LittleEndian.Uint16(lens[:2])
	if n != ^binary.LittleEndian.Uint16(lens[2:]) {
		return errStreamCorrupt
	}
	w.out += int64(n)
	for ; n > 0 && w.nb > 0; n-- {
		w.use(8)
	}
	for n > 0 {
		if len(w.cur) == 0 && !w.piece() {
			return io.ErrUnexpectedEOF
		}
		k := min(int(n), len(w.cur))
		w.cur = w.cur[k:]
		n -= uint16(k)
	}
	return nil
}

// piece moves on to the stream's next piece that is not empty, and reports
// whether there is one.
func (w *inflateWalk) piece() bool {
	for len(w.cur) == 0 {
		var ok bool
		if w.cur, w.next, ok = pngIDATAt(w.png, w.next); !ok {
			return false
		}
	}
	return true
}

// byte reads the next whole byte, from the bits held first.
func (w *inflateWalk) byte() (byte, error) {
	if err := w.need(8); err != nil {
		return 0, err
	}
	c := byte(w.b)
	w.use(8)
	return c, nil
}

// fill holds as many more of the stream's bytes in b as it has room for.
func (w *inflateWalk) fill() {
	for w.nb <= 56 {
		if len(w.cur) == 0 && !w.piece() {
			return
		}
		if k := (64 - w.nb) / 8; len(w.cur) >= 8 {
			v := binary.LittleEndian.Uint64(w.cur)
			if k < 8 {
				v &= 1<<(8*k) - 1
			}
			w.b |= v << w.nb
			w.nb += 8 * k
			w.cur = w.cur[k:]
			continue
		}
		w.b |= uint64(w.cur[0]) << w.nb
		w.nb += 8
		w.cur = w.cur[1:]
	}
}

// need holds at least n bits in b, or fails at the end of the input.
func (w *inflateWalk) need(n uint) error {
	if w.nb < n {
		w.fill()
		if w.nb < n {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

// use drops the next n bits.
func (w *inflateWalk) use(n uint) {
	w.b >>= n
	w.nb -= n
}

// sym reads the next symbol of code c, as compress/flate's huffSym does: it
// needs the code's shortest length in bits before it looks (min), and fails
// as corrupt on bits no code begins with, which only a code of one symbol of
// one bit, or of none, has.
func (w *inflateWalk) sym(c *flateCode) (int, error) {
	if w.work++; w.work >= haltEveryWork { // halted(1), without a call
		if err := w.ask(); err != nil {
			return 0, err
		}
	}
	if w.nb < 15 {
		w.fill()
	}
	if w.nb < c.min {
		return 0, io.ErrUnexpectedEOF
	}
	if !c.built {
		if c.read < buildAfter {
			c.read++
			return w.canonical(c, 1, 0, 0, 0)
		}
		c.build()
	}
	e := c.table[w.b&0x1FF]
	n := uint(e & 0xF)
	if n == 0 {
		return 0, errStreamCorrupt
	}
	if n <= 9 {
		if n > w.nb {
			return 0, io.ErrUnexpectedEOF
		}
		w.use(n)
		return int(e >> 4), nil
	}
	// A code longer than nine bits, read on from its tenth.
	return w.canonical(c, 10, int(bits.Reverse16(uint16(w.b&0x1FF))>>7), c.first10, c.index10)
}

// canonical reads the next symbol of code c a bit at a time, in canonical
// order (RFC 1951 3.2.2), from its bit n, given its first n-1 bits as code,
// first, the first code of n bits, and index, where those begin in sorted.
// Bits no code begins with, which only a code of one symbol of one bit, or
// of none, has, are corrupt; bits past the input's end read as zeros, and a
// code that runs into them is cut short, as sym's table reads them.
func (w *inflateWalk) canonical(c *flateCode, n uint, code, first, index int) (int, error) {
	for ; n <= c.max; n++ {
		code = code<<1 | int(w.b>>(n-1)&1)
		count := c.count[n]
		if code-first < count {
			if n > w.nb {
				return 0, io.ErrUnexpectedEOF
			}
			w.use(n)
			return int(c.sorted[index+code-first]), nil
		}
		index += count
		first = (first + count) << 1
	}
	return 0, errStreamCorrupt
}

// buildAfter is how many symbols of a code sym reads a bit at a time
// (canonical) before it builds the code's table (build): reading a symbol so
// takes a few nanoseconds for every bit of its code, and building the table
// writes its 512 entries, which took about a microsecond, as long as reading
// all the codes of a block that holds no more than its end. So a block of
// few symbols, as a stream made to cost CPU holds, has no table built, and
// one of many builds it after a few dozen.
const buildAfter = 64

// flateCode is a Huffman code as a walk reads it: its symbols in canonical
// order, which sym reads its first buildAfter symbols by, and then a table of
// the symbols of codes of up to nine bits, by those bits as they come
// (symbol<<4 | length), with 10 for the first nine bits of a longer code,
// which sym reads on from there in canonical order.
type flateCode struct {
	table   [512]uint16
	built   bool        // whether table holds the code (build)
	read    int         // symbols read since init, while it did not
	count   [16]int     // codes of each length
	sorted  [288]uint16 // the symbols in canonical order
	first10 int         // the first ten-bit code
	index10 int         // where the codes of ten bits and more begin in sorted
	min     uint        // the bits sym needs before it looks
	max     uint        // the longest code's length
	tables  int64       // what compress/flate allocates for the code
}

// init sets c to the code of the given lengths and reports whether
// compress/flate's huffmanDecoder.init takes it: a code that is complete, or
// of one symbol of one bit, or of none. It counts what init allocates for a
// code longer than nine bits (tables): for each of the first nine bits from
// those the first ten-bit code begins with, a table of 1<<(max-9) entries of
// four bytes, and a slice of as many slices to hold them, each as the
// allocator sizes it (allocated).
func (c *flateCode) init(lengths []uint8) bool {
	var count [16]int
	lo, hi := 0, 0
	for _, n := range lengths {
		if n == 0 {
			continue
		}
		if lo == 0 || int(n) < lo {
			lo = int(n)
		}
		hi = max(hi, int(n))
		count[n]++
	}
	c.built, c.read = false, 0
	c.count = count
	c.min, c.max, c.tables = uint(lo), uint(hi), 0
	if hi == 0 {
		return true
	}
	code := 0
	var next [16]int
	for i := lo; i <= hi; i++ {
		code <<= 1
		next[i] = code
		code += count[i]
	}
	if code != 1<<hi && !(code == 1 && hi == 1) {
		return false
	}
	if hi > 9 {
		n := int64(512 - next[10]>>1)
		c.tables = n*allocated(4<<(hi-9), false) + allocated(n*sliceHeader, true)
	}
	var at [16]int
	for i := 1; i < 16; i++ {
		at[i] = at[i-1] + count[i-1]
	}
	c.first10, c.index10 = next[10], at[10]
	for sym, n := range lengths {
		if n != 0 {
			c.sorted[at[n]] = uint16(sym)
			at[n]++
		}
	}
	return true
}

// build fills in c's table from its symbols in canonical order.
func (c *flateCode) build() {
	c.table = [512]uint16{}
	code, index := 0, 0
	for n := uint(1); n <= c.max; n++ {
		for _, sym := range c.sorted[index : index+c.count[n]] {
			rev := int(bits.Reverse16(uint16(code)) >> (16 - n))
			code++
			if n <= 9 {
				for off := rev; off < 512; off += 1 << n {
					c.table[off] = uint16(sym)<<4 | uint16(n)
				}
			} else {
				c.table[rev&0x1FF] = 10
			}
		}
		index += c.count[n]
		code <<= 1
	}
	c.built = true
}

// sliceHeader is the bytes of a slice's header, which is what each entry of
// a slice of slices takes.
const sliceHeader = 24

// allocated is what Go's allocator (1.22 to 1.25, on 64-bit platforms) takes
// for an object of n bytes, 12 KiB or less, with pointers in it or not: an
// object of fewer than 16 bytes and no pointers, a block of 16, which it
// shares with others but under the race detector, which gives each its own;
// an object of more than 512 bytes with pointers, eight bytes more for a
// header of its own; and either rounded up to the size class it falls in.
func allocated(n int64, pointers bool) int64 {
	if !pointers && n < 16 {
		return 16
	}
	if pointers && n > 512 {
		n += 8
	}
	for _, size := range sizeClasses {
		if size >= n {
			return size
		}
	}
	return n
}

// sizeClasses are the allocator's size classes up to 16 KiB
// (runtime/sizeclasses.go, the same from Go 1.22 to 1.25).
var sizeClasses = [...]int64{
	8, 16, 24, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208, 224, 240,
	256, 288, 320, 352, 384, 416, 448, 480, 512, 576, 640, 704, 768, 896, 1024,
	1152, 1280, 1408, 1536, 1792, 2048, 2304, 2688, 3072, 3200, 3456, 4096,
	4864, 5376, 6144, 6528, 6784, 6912, 8192, 9472, 9728, 10240, 10880, 12288,
	13568, 14336, 16384,
}
