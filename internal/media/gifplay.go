package media

import (
	"compress/lzw"
	"encoding/binary"
	"errors"
	"image"
	"image/gif"
	"io"
	"slices"
)

// What a GIF shows is a run of pictures on its canvas, each for a time,
// played once or a number of times over; everything else in the file is free
// for whoever made it to fill in. The same run can be made of frames in any
// number of ways — a frame that draws the picture again, or draws nothing,
// time split between frames, a disposal method the next frame paints over,
// a pixel drawn in the colour already under it rather than left transparent,
// colour tables in any order — and every one of those choices is bits nobody
// sees. So a GIF is not stored as its frames but as what it shows (playGIF):
// two GIFs Chromium shows alike, the same pictures in the same order, each
// for the same time, on a canvas of the same size, played as many times, are
// stored as the same bytes. Nothing of how the upload made its pictures is
// left to carry anything.
//
// The stored file is written by one rule from the run of pictures:
//
//   - Each picture is one frame. A picture that shows for longer than one
//     delay can say is that frame and then frames that draw nothing, holding
//     the rest of its time.
//   - A frame draws exactly the pixels in which its picture differs from what
//     the frame before it left, and leaves every other pixel transparent; it
//     is cut to the smallest rectangle that holds those pixels and the
//     pixels the next picture has transparent where this one does not. It is
//     cleared once its time is over when there are such pixels, since only
//     clearing makes a pixel transparent again, and left in place otherwise.
//   - The colours are the ones the pictures show, in order of their values.
//     Where there are 255 or fewer, they are one global table after a
//     transparent entry at index 0, which every frame uses. Where there are
//     more, each frame has a table of its own, of the colours it draws, after
//     a transparent entry if any pixel of its rectangle needs one.
//   - Delays are the times a browser shows (shownDelay), the last picture of
//     a GIF played once has none, and a GIF of one picture is a still: one
//     frame, with no delay, no disposal and no loop count.

// The disposal methods browsers carry out: keepFrame leaves a frame where it
// is, clearFrame clears its rectangle to transparent, and restoreFrame puts
// back what was there before it was drawn. playGIF writes only the first two.
const (
	keepFrame    = gif.DisposalNone
	clearFrame   = gif.DisposalBackground
	restoreFrame = gif.DisposalPrevious
)

// maxGIFDelay is the longest delay a GIF can give a frame, in hundredths of a
// second.
const maxGIFDelay = 0xFFFF

// browserDisposal is what Chromium and Firefox do with a frame once its delay
// is over, for the disposal method its file gives: 2 clears its rectangle and
// 3 puts back what was under it, as the specification says; 4, which the
// specification leaves undefined, is read as 3; and 0, 1 and the other values
// it leaves undefined leave the frame in place.
func browserDisposal(d byte) byte {
	switch d {
	case clearFrame:
		return clearFrame
	case restoreFrame, 4:
		return restoreFrame
	}
	return keepFrame
}

// shownDelay is how long Chromium and Firefox show a frame of the delay
// given, in hundredths of a second: as written, but for a delay of 0 or 1,
// which both show for a tenth of a second.
func shownDelay(d int) int {
	if d <= 1 {
		return 10
	}
	return d
}

// A colour key is a colour a GIF's pixel shows, as one number: 0 for
// transparent, and otherwise opaqueKey with the red, green and blue bytes
// below it.
const opaqueKey uint32 = 0xFF << 24

// keys is the colour key each index of a frame's pixels shows, as Chromium
// reads the frame: none for its transparent index, the colour its table
// gives an index within it, and opaque black past the table's end.
func (h gifFrameHead) keys() (keys [256]uint32) {
	for v := range keys {
		switch {
		case h.transparent && v == int(h.ti):
		case 3*v+3 <= len(h.table):
			keys[v] = opaqueKey | uint32(h.table[3*v])<<16 | uint32(h.table[3*v+1])<<8 | uint32(h.table[3*v+2])
		default:
			keys[v] = opaqueKey
		}
	}
	return keys
}

// gifFrame is a decoded frame as it is played: the part of it on the canvas,
// how it is disposed of, and the colour key of each of its indices.
type gifFrame struct {
	m        *image.Paletted
	r        image.Rectangle
	disposal byte
	keys     [256]uint32
}

func (f *gifFrame) load(m *image.Paletted, h gifFrameHead, canvas image.Rectangle) {
	f.m, f.r, f.disposal, f.keys = m, m.Rect.Intersect(canvas), browserDisposal(h.disposal), h.keys()
}

// overlay draws the frame's pixels of canvas row y on row, which holds that
// row's colour keys from x0 on: every pixel of the frame there but its
// transparent ones.
func (f *gifFrame) overlay(row []uint32, y, x0 int) {
	r := f.r
	if y < r.Min.Y || y >= r.Max.Y {
		return
	}
	lo, hi := max(x0, r.Min.X), min(x0+len(row), r.Max.X)
	if lo >= hi {
		return
	}
	src := f.m.Pix[f.m.PixOffset(lo, y):][:hi-lo]
	dst := row[lo-x0:][:len(src)]
	for i, v := range src {
		if k := f.keys[v]; k != 0 {
			dst[i] = k
		}
	}
}

// clearRun makes the pixels of row, which holds canvas row y from x0 on,
// transparent where they are in r.
func clearRun(row []uint32, r image.Rectangle, y, x0 int) {
	if y < r.Min.Y || y >= r.Max.Y {
		return
	}
	if lo, hi := max(x0, r.Min.X), min(x0+len(row), r.Max.X); lo < hi {
		clear(row[lo-x0 : hi-x0])
	}
}

// spans calls f for each row of the union of three rectangles, in runs
// [x0, x1) that do not overlap, so that each pixel of the union is visited
// once.
func spans(a, b, c image.Rectangle, f func(y, x0, x1 int)) {
	rects := [3]image.Rectangle{a, b, c}
	bound := a.Union(b).Union(c)
	for y := bound.Min.Y; y < bound.Max.Y; y++ {
		var runs [3][2]int
		n := 0
		for _, r := range rects {
			if r.Empty() || y < r.Min.Y || y >= r.Max.Y {
				continue
			}
			i := n
			for ; i > 0 && runs[i-1][0] > r.Min.X; i-- {
				runs[i] = runs[i-1]
			}
			runs[i] = [2]int{r.Min.X, r.Max.X}
			n++
		}
		if n == 0 {
			continue
		}
		x0, x1 := runs[0][0], runs[0][1]
		for _, run := range runs[1:n] {
			if run[0] <= x1 {
				x1 = max(x1, run[1])
				continue
			}
			f(y, x0, x1)
			x0, x1 = run[0], run[1]
		}
		f(y, x0, x1)
	}
}

// clearBitRange clears bits lo to hi of a bit set.
func clearBitRange(bits []uint64, lo, hi int) {
	for lo < hi {
		n := min(64-lo&63, hi-lo)
		bits[lo>>6] &^= ^uint64(0) >> (64 - n) << (lo & 63)
		lo += n
	}
}

// gifPlayer plays a GIF's frames and writes the run of pictures they show.
//
// Frame j is drawn on what the frames before it left: base, which is the
// picture before it (prev) if frame j-1 is left in place, that picture with
// frame j-1's rectangle cleared if it is cleared, and what was there before
// frame j-1 was drawn if that is put back. The player keeps that last one,
// under, as colour keys, which is all it needs: prev is under with frame
// j-1 drawn on it, base is one of the three, and the picture frame j makes,
// now, is base with frame j drawn on it. All three can differ from under
// only where frame j-1 or frame j is, so each is worked out a row at a time
// where it is needed and never kept.
//
// A picture ends where the next frame changes what shows. Until then the
// player knows the picture being shown and the pixels its stored frame
// draws (changed, a bit a canvas pixel, with box around them), and adds up
// the time it shows; when it ends, it knows what the next picture needs
// cleared, and writes the frame.
type gifPlayer struct {
	w       int
	under   []uint32
	a, b    *gifFrame // frames j-1 and j
	changed []uint64  // what the stored frame of the picture showing draws
	next    []uint64  // what the stored frame of the next picture draws
	box     image.Rectangle
	time    int // how long the picture showing has shown, in hundredths
	loop    int // the loop count, or -1 for a GIF played once
	emitted int // the pictures written
	area    int64
	global  bool
	index   colourIndex
	colours []uint32 // a frame's colours, for its own table
	table   []uint32 // a frame's own table
	prev    []uint32 // row scratch
	base    []uint32
	now     []uint32
	idx     []uint8
	out     gifWriter
}

// rows is prev, base and now for canvas row y from x0 to x1. base and prev
// are the same slice when frame j-1 is left in place.
func (p *gifPlayer) rows(y, x0, x1 int) (prev, base, now []uint32) {
	n := x1 - x0
	under := p.under[y*p.w+x0 : y*p.w+x1]
	prev = p.prev[:n]
	copy(prev, under)
	p.a.overlay(prev, y, x0)
	switch p.a.disposal {
	case keepFrame:
		base = prev
	case clearFrame:
		base = p.base[:n]
		copy(base, prev)
		clearRun(base, p.a.r, y, x0)
	default:
		base = under
	}
	now = p.now[:n]
	copy(now, base)
	p.b.overlay(now, y, x0)
	return prev, base, now
}

// prevRow is prev alone, for canvas row y from x0 to x1.
func (p *gifPlayer) prevRow(y, x0, x1 int) []uint32 {
	prev := p.prev[:x1-x0]
	copy(prev, p.under[y*p.w+x0:y*p.w+x1])
	p.a.overlay(prev, y, x0)
	return prev
}

func (p *gifPlayer) isChanged(i int) bool { return p.changed[i>>6]>>(i&63)&1 != 0 }

// differs compares what frame j shows with what showed before it. It
// returns box, around every pixel that changes, which is empty when frame j
// shows the same picture; and toClear, around every pixel that was opaque
// and is now transparent. It marks the pixels that change in next: what the
// stored frame of the new picture draws, if the stored frame before it is
// left in place.
func (p *gifPlayer) differs() (box, toClear image.Rectangle) {
	spans(p.a.r, p.b.r, image.Rectangle{}, func(y, x0, x1 int) {
		prev, _, now := p.rows(y, x0, x1)
		prev = prev[:len(now)]
		at := y*p.w + x0
		lo, hi, clo, chi := -1, -1, -1, -1
		for i, k := range now {
			if k == prev[i] {
				continue
			}
			p.next[(at+i)>>6] |= 1 << ((at + i) & 63)
			if lo < 0 {
				lo = i
			}
			hi = i
			if k == 0 {
				if clo < 0 {
					clo = i
				}
				chi = i
			}
		}
		if lo >= 0 {
			box = box.Union(image.Rect(x0+lo, y, x0+hi+1, y+1))
		}
		if clo >= 0 {
			toClear = toClear.Union(image.Rect(x0+clo, y, x0+chi+1, y+1))
		}
	})
	return box, toClear
}

// mark marks in next what the stored frame of the new picture draws when
// the stored frame before it clears cleared: every pixel in which the new
// picture differs from the one before it with cleared made transparent. It
// returns the rectangle around them.
func (p *gifPlayer) mark(cleared image.Rectangle) (box image.Rectangle) {
	spans(p.a.r, p.b.r, cleared, func(y, x0, x1 int) {
		prev, _, now := p.rows(y, x0, x1)
		prev = prev[:len(now)]
		c0, c1 := x0, x0 // the part of the run cleared
		if y >= cleared.Min.Y && y < cleared.Max.Y {
			c0, c1 = max(x0, cleared.Min.X), max(x0, min(x1, cleared.Max.X))
		}
		at := y*p.w + x0
		lo, hi := -1, -1
		for i, k := range now {
			was := prev[i]
			if x := x0 + i; x >= c0 && x < c1 {
				was = 0
			}
			bit := uint64(1) << ((at + i) & 63)
			if k == was {
				p.next[(at+i)>>6] &^= bit
				continue
			}
			p.next[(at+i)>>6] |= bit
			if lo < 0 {
				lo = i
			}
			hi = i
		}
		if lo >= 0 {
			box = box.Union(image.Rect(x0+lo, y, x0+hi+1, y+1))
		}
	})
	return box
}

// advance moves on to the next frame: under becomes what frame j-1's
// disposal left, and frame j becomes frame j-1.
func (p *gifPlayer) advance() {
	a := p.a
	for y := a.r.Min.Y; y < a.r.Max.Y; y++ {
		row := p.under[y*p.w+a.r.Min.X : y*p.w+a.r.Max.X]
		switch a.disposal {
		case keepFrame:
			a.overlay(row, y, a.r.Min.X)
		case clearFrame:
			clear(row)
		}
	}
	p.a, p.b = p.b, p.a
	p.b.m = nil
}

// emit writes the stored frame of the picture showing, once it has ended,
// or, with last, once the GIF has: cut to the pixels it changes and the
// pixels cleared, which the next picture needs cleared, and cleared after
// its time if they are any. It returns the frame's rectangle.
func (p *gifPlayer) emit(cleared image.Rectangle, last bool) (image.Rectangle, error) {
	r := p.box.Union(cleared)
	if last && p.emitted == 0 {
		return r, p.frame(r, 0, 0, true, false)
	}
	disposal := byte(keepFrame)
	if !cleared.Empty() {
		disposal = clearFrame
	}
	if p.emitted == 0 && p.loop >= 0 {
		p.out.loopCount(p.loop)
	}
	p.emitted++
	t := p.time
	if last && p.loop < 0 {
		t = 0 // it stays on screen once it is drawn
	}
	// The time is split into delays of at most maxGIFDelay, none of them
	// 1, which shows for 10.
	piece := func(t int) int {
		d := min(t, maxGIFDelay)
		if t-d == 1 {
			d--
		}
		return d
	}
	d := piece(t)
	t -= d
	if t > 0 {
		if err := p.frame(r, d, keepFrame, false, false); err != nil {
			return r, err
		}
		for t > 0 {
			d = piece(t)
			t -= d
			at, how := image.Rectangle{}, byte(keepFrame)
			if t == 0 && disposal == clearFrame {
				at, how = r, clearFrame
			}
			if err := p.frame(at, d, how, false, true); err != nil {
				return r, err
			}
		}
		return r, nil
	}
	return r, p.frame(r, d, disposal, false, false)
}

// errGIFColour is a pixel whose colour is not in the table it is written
// with, which playGIF never makes.
var errGIFColour = errors.New("media: a GIF pixel's colour is not in its table")

// frame writes one stored frame: of rectangle r (one transparent pixel at
// the corner if it is empty), drawing the pixels marked changed in the
// colours prev shows, or nothing at all if blank. A still has no control
// extension unless it has transparent pixels.
func (p *gifPlayer) frame(r image.Rectangle, delay int, disposal byte, still, blank bool) error {
	if r.Empty() {
		r, blank = image.Rect(0, 0, 1, 1), true
	}
	p.area += int64(r.Dx()) * int64(r.Dy())
	if p.area > maxGIFFramePixels {
		return ErrImageTooCostly
	}
	transparent := true
	var own []uint32
	switch {
	case !p.global:
		colours, needed, err := p.frameColours(r, blank)
		if err != nil {
			return err
		}
		transparent = needed
		own = p.table[:0]
		if transparent {
			own = append(own, 0)
		}
		own = append(own, colours...)
		p.index.reset()
		for i, k := range own[b2i(transparent):] {
			p.index.add(k, uint8(i+b2i(transparent)))
		}
	case still:
		transparent = blank || !p.allChanged(r)
	}
	if !still || transparent {
		p.out.control(disposal, delay, transparent)
	}
	p.out.image(r, own)
	missing := false
	idx := p.idx[:r.Dx()]
	for y := r.Min.Y; y < r.Max.Y; y++ {
		if blank {
			clear(idx)
		} else {
			prev := p.prevRow(y, r.Min.X, r.Max.X)
			at := y*p.w + r.Min.X
			for i, k := range prev[:len(idx)] {
				if transparent && !p.isChanged(at+i) {
					idx[i] = 0
					continue
				}
				v, ok := p.index.lookup(k)
				missing = missing || !ok
				idx[i] = v
			}
		}
		p.out.pixels(idx)
	}
	p.out.endImage()
	if missing {
		return errGIFColour
	}
	return p.out.err
}

// allChanged reports whether every pixel of r is marked changed.
func (p *gifPlayer) allChanged(r image.Rectangle) bool {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for i := y*p.w + r.Min.X; i < y*p.w+r.Max.X; i++ {
			if !p.isChanged(i) {
				return false
			}
		}
	}
	return true
}

// frameColours is the colours a stored frame of its own table draws, in
// order of their values, and whether it needs a transparent entry: for every
// pixel of its rectangle it does not draw. 256 colours leave no room for
// one, and then each pixel it does not draw is written in the colour it
// already shows, which the frame has to draw elsewhere; where that cannot be
// done, or the frame draws more than 256 colours, the GIF is refused
// (ErrImageTooCostly): it cannot be stored one frame to a picture.
func (p *gifPlayer) frameColours(r image.Rectangle, blank bool) (colours []uint32, transparent bool, err error) {
	if blank {
		return nil, true, nil
	}
	p.index.reset()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		at := y*p.w + r.Min.X
		for i, k := range p.prevRow(y, r.Min.X, r.Max.X) {
			if !p.isChanged(at + i) {
				transparent = true
			} else if p.index.add(k, 0) && p.index.n > 256 {
				return nil, false, ErrImageTooCostly
			}
		}
	}
	colours = p.index.sorted(p.colours[:0])
	if transparent && len(colours) == 256 {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			at := y*p.w + r.Min.X
			for i, k := range p.prevRow(y, r.Min.X, r.Max.X) {
				if _, ok := p.index.lookup(k); !p.isChanged(at+i) && !ok {
					return nil, false, ErrImageTooCostly
				}
			}
		}
		transparent = false
	}
	return colours, transparent, nil
}

// playGIF writes to out the run of pictures a GIF's frames show on a canvas,
// played as Chromium plays them, as described at the top of this file: on a
// canvas that starts transparent, each frame's pixels but its transparent
// ones are drawn, the canvas is shown for the frame's delay, and the frame is
// disposed of (browserDisposal). frames are the frames image/gif decoded, and
// heads what the walk read of each (gifFrameHead), from which their colours,
// delays and disposal methods are taken, as Chromium reads them. loop is the
// loop count, or -1 for a GIF played once.
//
// It refuses a GIF it cannot store one frame to a picture (frameColours), or
// whose stored frames would have more pixels between them than
// maxGIFFramePixels, with ErrImageTooCostly. The header is written before
// anything is allocated for play, so a limit too small for it costs nothing.
// Each frame is let go once it has been played, its place in frames set to
// nil.
func playGIF(frames []*image.Paletted, heads []gifFrameHead, canvas image.Rectangle, loop int, out io.Writer) error {
	w, h := canvas.Dx(), canvas.Dy()
	colours, global := gifColours(frames, heads, canvas)
	p := &gifPlayer{w: w, loop: loop, global: global}
	p.out.w = out
	var table []uint32
	if global {
		table = append([]uint32{0}, colours...)
	}
	p.out.header(w, h, table)
	if p.out.err != nil {
		return p.out.err
	}
	if global {
		p.index.reset()
		for i, k := range colours {
			p.index.add(k, uint8(i+1))
		}
	} else {
		p.colours, p.table = make([]uint32, 0, 256), make([]uint32, 0, 257)
	}
	words := (w*h + 63) / 64
	p.under, p.changed, p.next = make([]uint32, w*h), make([]uint64, words), make([]uint64, words)
	p.prev, p.base, p.now, p.idx = make([]uint32, w), make([]uint32, w), make([]uint32, w), make([]uint8, w)
	p.a, p.b = &gifFrame{disposal: keepFrame}, &gifFrame{}

	for j, m := range frames {
		p.b.load(m, heads[j], canvas)
		box, toClear := p.differs()
		if j == 0 || !box.Empty() {
			// Frame j shows a new picture: the one before it has ended.
			if j > 0 {
				r, err := p.emit(toClear, false)
				if err != nil {
					return err
				}
				if !toClear.Empty() {
					box = p.mark(r)
				}
				for y := p.box.Min.Y; y < p.box.Max.Y; y++ {
					clearBitRange(p.changed, y*w+p.box.Min.X, y*w+p.box.Max.X)
				}
				p.time = 0
			}
			p.changed, p.next, p.box = p.next, p.changed, box
		}
		p.time += shownDelay(heads[j].delay)
		p.advance()
		if j > 0 {
			frames[j-1] = nil
		}
	}
	if _, err := p.emit(image.Rectangle{}, true); err != nil {
		return err
	}
	p.out.trailer()
	return p.out.err
}

// gifColours is every colour a GIF's frames draw on its canvas, in order of
// their values, and whether there are no more than 255 of them, which one
// table holds with a transparent entry. It stops counting past 255.
//
// Every pixel a frame draws on the canvas shows in the picture it makes, and
// every pixel a picture shows was drawn by a frame, so these are the colours
// of the pictures, whatever the frames that made them.
func gifColours(frames []*image.Paletted, heads []gifFrameHead, canvas image.Rectangle) ([]uint32, bool) {
	var set colourIndex
	for k, m := range frames {
		keys := heads[k].keys()
		r := m.Rect.Intersect(canvas)
		var used [256]bool
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for _, v := range m.Pix[m.PixOffset(r.Min.X, y):][:r.Dx()] {
				used[v] = true
			}
		}
		for v, u := range used {
			if u && keys[v] != 0 && set.add(keys[v], 0) && set.n > 255 {
				return nil, false
			}
		}
	}
	return set.sorted(nil), true
}

// colourIndex is a set of up to 256 opaque colour keys and the index of
// each in a colour table, in a table of 512 slots an empty slot of which is
// 0, which no opaque key is.
type colourIndex struct {
	keys    [512]uint32
	at      [512]uint8
	n       int
	lastKey uint32 // the last key looked up, and its index
	lastAt  uint8
}

func colourSlot(k uint32) uint32 { return (k * 0x9E3779B1) >> 23 }

func (c *colourIndex) reset() {
	clear(c.keys[:])
	c.n, c.lastKey = 0, 0
}

// add puts opaque key k in the set at index i, and reports whether it was
// not there already. The set must have room: fewer than 511 keys.
func (c *colourIndex) add(k uint32, i uint8) bool {
	s := colourSlot(k)
	for ; c.keys[s] != 0; s = (s + 1) & 511 {
		if c.keys[s] == k {
			return false
		}
	}
	c.keys[s], c.at[s] = k, i
	c.n++
	return true
}

// lookup is k's index, and whether it is in the set.
func (c *colourIndex) lookup(k uint32) (uint8, bool) {
	if k == c.lastKey && k != 0 {
		return c.lastAt, true
	}
	for s := colourSlot(k); ; s = (s + 1) & 511 {
		switch c.keys[s] {
		case 0:
			return 0, false
		case k:
			c.lastKey, c.lastAt = k, c.at[s]
			return c.at[s], true
		}
	}
}

// sorted appends the set's keys to dst in order of their values.
func (c *colourIndex) sorted(dst []uint32) []uint32 {
	for _, k := range c.keys {
		if k != 0 {
			dst = append(dst, k)
		}
	}
	slices.Sort(dst)
	return dst
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// gifWriter writes a GIF's blocks, keeping the first error its writer
// returns and writing nothing after it.
type gifWriter struct {
	w      io.Writer
	err    error
	buf    []byte
	field  byte // the global table's size field, which frames without their own use
	blocks subBlocks
	lzw    *lzw.Writer
}

func (g *gifWriter) write(b []byte) {
	if g.err == nil {
		_, g.err = g.w.Write(b)
	}
}

// tableField is the size field of a colour table of n entries, which holds
// 2<<field of them.
func tableField(n int) byte {
	f := byte(0)
	for 2<<f < n {
		f++
	}
	return f
}

// appendTable appends a colour table of colour keys, padded with black to
// the size its field says.
func appendTable(b []byte, table []uint32) []byte {
	for i := 0; i < 2<<tableField(len(table)); i++ {
		var k uint32
		if i < len(table) {
			k = table[i]
		}
		b = append(b, byte(k>>16), byte(k>>8), byte(k))
	}
	return b
}

// header writes the signature, the logical screen and the global colour
// table, if there is one: no background colour, no aspect ratio, and the
// colour resolution and sort flag the same whatever the table.
func (g *gifWriter) header(w, h int, table []uint32) {
	b := append(g.buf[:0], "GIF89a"...)
	b = binary.LittleEndian.AppendUint16(b, uint16(w))
	b = binary.LittleEndian.AppendUint16(b, uint16(h))
	if table == nil {
		b = append(b, 0, 0, 0)
	} else {
		g.field = tableField(len(table))
		b = appendTable(append(b, 0xF0|g.field, 0, 0), table)
	}
	g.buf = b
	g.write(b)
}

// loopCount writes a NETSCAPE2.0 extension that plays the GIF n times over
// (0 for ever).
func (g *gifWriter) loopCount(n int) {
	b := append(g.buf[:0], 0x21, 0xFF, 11)
	b = append(b, "NETSCAPE2.0"...)
	b = append(b, 3, 1, byte(n), byte(n>>8), 0)
	g.buf = b
	g.write(b)
}

// control writes a graphic control extension: the disposal method, the
// delay, and index 0 as the transparent one if transparent.
func (g *gifWriter) control(disposal byte, delay int, transparent bool) {
	g.buf = append(g.buf[:0], 0x21, 0xF9, 4, disposal<<2|byte(b2i(transparent)), byte(delay), byte(delay>>8), 0, 0)
	g.write(g.buf)
}

// image writes an image descriptor for rectangle r, not interlaced, with
// its own colour table if table is not nil, and starts its pixels.
func (g *gifWriter) image(r image.Rectangle, table []uint32) {
	b := append(g.buf[:0], 0x2C)
	for _, v := range []int{r.Min.X, r.Min.Y, r.Dx(), r.Dy()} {
		b = binary.LittleEndian.AppendUint16(b, uint16(v))
	}
	field := g.field
	if table == nil {
		b = append(b, 0)
	} else {
		field = tableField(len(table))
		b = appendTable(append(b, 0x80|field), table)
	}
	lit := max(2, int(field)+1)
	b = append(b, byte(lit))
	g.buf = b
	g.write(b)
	g.blocks = subBlocks{w: g.w}
	if g.lzw == nil {
		g.lzw = lzw.NewWriter(&g.blocks, lzw.LSB, lit).(*lzw.Writer)
	} else {
		g.lzw.Reset(&g.blocks, lzw.LSB, lit)
	}
}

// pixels writes a row of the image's pixels.
func (g *gifWriter) pixels(row []uint8) {
	if g.err == nil {
		_, g.err = g.lzw.Write(row)
	}
}

// endImage ends the image's pixels.
func (g *gifWriter) endImage() {
	if g.err == nil {
		g.err = g.lzw.Close()
	}
	if g.err == nil {
		g.err = g.blocks.close()
	}
}

func (g *gifWriter) trailer() { g.write(gifTrailer) }

// subBlocks writes a stream as GIF data sub-blocks of up to 255 bytes each,
// and ends it with an empty one.
type subBlocks struct {
	w   io.Writer
	buf [256]byte
	n   int
	err error
}

func (s *subBlocks) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 && s.err == nil {
		c := copy(s.buf[1+s.n:], b)
		s.n += c
		b = b[c:]
		written += c
		if s.n == 255 {
			s.flush()
		}
	}
	return written, s.err
}

// WriteByte and Flush make subBlocks a writer compress/lzw writes to as it
// is, rather than through a buffer of its own for each frame.
func (s *subBlocks) WriteByte(c byte) error {
	if s.err == nil {
		s.n++
		s.buf[s.n] = c
		if s.n == 255 {
			s.flush()
		}
	}
	return s.err
}

func (s *subBlocks) Flush() error { return s.err }

func (s *subBlocks) flush() {
	if s.n > 0 && s.err == nil {
		s.buf[0] = byte(s.n)
		_, s.err = s.w.Write(s.buf[:1+s.n])
	}
	s.n = 0
}

func (s *subBlocks) close() error {
	s.flush()
	if s.err == nil {
		_, s.err = s.w.Write([]byte{0})
	}
	return s.err
}
