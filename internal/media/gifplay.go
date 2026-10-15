package media

import (
	"image"
	"image/color"
	"image/gif"
)

// What a GIF shows is its frames drawn one on another, each for its delay,
// and everything else a frame says is free for whoever made the file to fill
// in. A frame of one transparent pixel draws nothing, so its position, its
// delay and its disposal method are bytes nobody sees: fifteen such frames
// after a picture carry sixty bytes of anything, under the same picture shown
// for the same time. So a GIF is stored as it plays (playGIF): every frame
// that changes nothing on screen is folded into the time of the frame before
// it, every frame kept is cut to the part of it that changes something, and
// every disposal method is written as the one browsers carry out. What a
// frame shows and for how long is all of it that is kept.
//
// What still comes from the upload is how the pictures it shows are made of
// frames: a frame may clear or put back what the frame before it drew and the
// next one draw it again, and how long the picture shows is then shared
// between the two as the upload shared it; and a frame that changes some of
// the pixels under it keeps the others it repaints in the colour they already
// had. Both are the picture as the poster drew it, frame by frame, as the low
// bits of an opaque pixel are; nothing about them is metadata.

// The disposal methods browsers carry out, as playGIF writes them:
// keepFrame leaves a frame where it is, clearFrame clears its rectangle to
// transparent, and restoreFrame puts back what was there before it was drawn.
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

// gifShown is a frame of a GIF as it is stored: the decoded frame it is cut
// from (or none, for a frame that draws nothing and only holds what is on
// screen), where it is, how long what it shows is shown for, and how it is
// disposed of.
type gifShown struct {
	src      int // the decoded frame, or -1
	at       image.Rectangle
	hold     int // hundredths of a second, before it is split (playGIF)
	disposal byte
	blank    bool // draws nothing: its pixels are all made transparent
}

// gifCanvas is the canvas a GIF is played on, which records for each of its
// pixels which frame's pixel shows there rather than the colour, so that it
// takes two bytes a pixel whatever colours the frames have between them.
type gifCanvas struct {
	frames []*image.Paletted
	// keys holds 256 entries for each frame: the colour each index of its
	// palette shows (colourKey), with an index past the palette's end opaque
	// black, as image/gif pads a short table.
	keys  []uint32
	w     int
	owner []uint16 // 1 + the frame whose pixel shows, or 0 where none does
}

// colourKey is a colour as one number, which is 0 for every fully
// transparent colour and only for those.
func colourKey(c color.Color) uint32 {
	v := visibleNRGBA(c)
	return uint32(v.A)<<24 | uint32(v.R)<<16 | uint32(v.G)<<8 | uint32(v.B)
}

func newGIFCanvas(frames []*image.Paletted, w, h int) *gifCanvas {
	c := &gifCanvas{frames: frames, keys: make([]uint32, 256*len(frames)), w: w, owner: make([]uint16, w*h)}
	black := colourKey(unusedEntry)
	for k, m := range frames {
		keys := c.keys[256*k : 256*(k+1)]
		for i := range keys {
			keys[i] = black
			if i < len(m.Palette) {
				keys[i] = colourKey(m.Palette[i])
			}
		}
	}
	return c
}

// showing is the colour showing at canvas pixel (x, y), whose offset is i.
func (c *gifCanvas) showing(i, x, y int) uint32 {
	o := int(c.owner[i])
	if o == 0 {
		return 0
	}
	m := c.frames[o-1]
	return c.keys[256*(o-1)+int(m.Pix[m.PixOffset(x, y)])]
}

// each calls f for every pixel of r, which is on the canvas, with its offset.
func (c *gifCanvas) each(r image.Rectangle, f func(i, x, y int)) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		i := y*c.w + r.Min.X
		for x := r.Min.X; x < r.Max.X; x++ {
			f(i, x, y)
			i++
		}
	}
}

// grow widens r to take in the pixel (x, y).
func grow(r *image.Rectangle, x, y int) {
	if r.Empty() {
		*r = image.Rect(x, y, x+1, y+1)
		return
	}
	r.Min.X, r.Min.Y = min(r.Min.X, x), min(r.Min.Y, y)
	r.Max.X, r.Max.Y = max(r.Max.X, x+1), max(r.Max.Y, y+1)
}

// changes is the smallest rectangle holding every pixel frame k would change
// if it were drawn now: a pixel it draws, which is any pixel that is not
// transparent, in a colour other than the one showing there.
func (c *gifCanvas) changes(k int) image.Rectangle {
	m, keys := c.frames[k], c.keys[256*k:256*(k+1)]
	var r image.Rectangle
	c.each(m.Rect, func(i, x, y int) {
		if key := keys[m.Pix[m.PixOffset(x, y)]]; key != 0 && key != c.showing(i, x, y) {
			grow(&r, x, y)
		}
	})
	return r
}

// draw draws frame k's pixels in r on the canvas.
func (c *gifCanvas) draw(k int, r image.Rectangle) {
	m, keys := c.frames[k], c.keys[256*k:256*(k+1)]
	c.each(r, func(i, x, y int) {
		if key := keys[m.Pix[m.PixOffset(x, y)]]; key != 0 && key != c.showing(i, x, y) {
			c.owner[i] = uint16(k + 1)
		}
	})
}

// covered is the smallest rectangle holding every pixel of r that is not
// transparent.
func (c *gifCanvas) covered(r image.Rectangle) image.Rectangle {
	var on image.Rectangle
	c.each(r, func(i, x, y int) {
		if c.owner[i] != 0 {
			grow(&on, x, y)
		}
	})
	return on
}

// clear makes every pixel of r transparent.
func (c *gifCanvas) clear(r image.Rectangle) {
	c.each(r, func(i, _, _ int) { c.owner[i] = 0 })
}

// playGIF rewrites a decoded GIF, whose frames are all on its canvas
// (clipGIF), as the frames that make what it shows, played the way Chromium
// and Firefox play it: on a canvas that starts transparent, each frame's
// pixels but its transparent ones are drawn, the canvas is shown for the
// frame's delay, and the frame is disposed of (browserDisposal).
//
//   - A frame that changes nothing on screen, and whose disposal changes
//     nothing either, is not kept: its time is added to the frame's before
//     it. Where that one is disposed of when its delay is over, what shows
//     during the frame not kept is what the disposal left, and it is kept as
//     a frame of one transparent pixel at the corner, which shows that for
//     the time. So is a first frame that draws nothing, which shows the empty
//     canvas.
//   - A frame kept is cut to the smallest rectangle holding every pixel it
//     changes, and, for one cleared once its delay is over, every pixel the
//     clearing changes. A frame that changes nothing when it is drawn, and is
//     kept for its clearing, has its pixels made transparent.
//   - Each frame is given the disposal that does what its own did: one left
//     in place if its disposal changes nothing, one cleared if clearing does
//     what putting back what was under it does, and otherwise its own. The
//     last frame is left in place: a browser shows a GIF played to its end
//     with its last frame drawn, and one that loops starts again on an empty
//     canvas, so how the last frame is disposed of never shows.
//   - Each frame is given the delay a browser shows it for (shownDelay), so a
//     delay of 0 or 1 is written as 10. A frame given more time than a delay
//     can say is followed by frames of one transparent pixel holding the
//     rest, none of them under 2. The last frame of a GIF played once is
//     given no delay, because it stays on screen once it is drawn.
//   - A GIF whose frames come down to one is a still: that frame, with no
//     delay, no disposal and no loop count, none of which a still shows.
//
// The canvas records which frame shows at each pixel in 16 bits, which holds
// every frame a GIF Inspect admits can have: processingCost charges each
// gifFrameOverhead, so the budget holds 16,384. playGIF refuses more with
// ErrImageTooLarge.
func playGIF(g *gif.GIF) error {
	frames := g.Image
	if len(frames) >= 1<<16 {
		return ErrImageTooLarge
	}
	canvas := newGIFCanvas(frames, g.Config.Width, g.Config.Height)
	var shown []gifShown
	// hold adds time to what is on screen: to the last frame kept if it is
	// left in place, or else to a frame that draws nothing after it.
	hold := func(t int) {
		if n := len(shown); n > 0 && shown[n-1].disposal == keepFrame {
			shown[n-1].hold += t
			return
		}
		shown = append(shown, gifShown{src: -1, at: image.Rect(0, 0, 1, 1), hold: t, disposal: keepFrame, blank: true})
	}
	for k, m := range frames {
		t := shownDelay(g.Delay[k])
		disposal := byte(keepFrame)
		if k < len(frames)-1 && g.Disposal != nil {
			disposal = browserDisposal(g.Disposal[k])
		}
		drawn := canvas.changes(k)
		blank := false
		switch disposal {
		case keepFrame:
			if drawn.Empty() {
				hold(t)
				continue
			}
			canvas.draw(k, drawn)
		case restoreFrame:
			if drawn.Empty() {
				hold(t)
				continue
			}
			// Putting back what was under it, where nothing was, is
			// clearing it.
			if canvas.covered(drawn).Empty() {
				disposal = clearFrame
			}
		case clearFrame:
			// Clearing its rectangle once it has been shown leaves the
			// canvas as it was with what was under the frame cleared.
			under := canvas.covered(m.Rect)
			if drawn.Empty() && under.Empty() {
				hold(t)
				continue
			}
			canvas.clear(under)
			blank = drawn.Empty()
			drawn = drawn.Union(under)
		}
		shown = append(shown, gifShown{src: k, at: drawn, hold: t, disposal: disposal, blank: blank})
	}

	g.Image, g.Delay, g.Disposal = nil, nil, nil
	add := func(m *image.Paletted, delay int, disposal byte) {
		g.Image = append(g.Image, m)
		g.Delay = append(g.Delay, delay)
		g.Disposal = append(g.Disposal, disposal)
	}
	if len(shown) == 1 {
		add(shown[0].frame(frames), 0, 0)
		g.LoopCount = -1
		return nil
	}
	for n, s := range shown {
		m, t := s.frame(frames), s.hold
		if n == len(shown)-1 && g.LoopCount == -1 {
			t = 0
		}
		// Only a frame left in place is given more than one delay can
		// say: any other holds its own delay alone.
		for t > maxGIFDelay {
			piece := maxGIFDelay
			if t-piece == 1 {
				piece-- // a delay of 1 shows for 10
			}
			add(m, piece, keepFrame)
			m, t = emptyGIFFrame(), t-piece
		}
		add(m, t, s.disposal)
	}
	return nil
}

// frame is the stored frame s is, cut from the decoded frames.
func (s gifShown) frame(frames []*image.Paletted) *image.Paletted {
	if s.src < 0 {
		return emptyGIFFrame()
	}
	m := frames[s.src]
	if s.at != m.Rect {
		m = m.SubImage(s.at).(*image.Paletted)
	}
	if s.blank {
		for y := s.at.Min.Y; y < s.at.Max.Y; y++ {
			clear(m.Pix[m.PixOffset(s.at.Min.X, y):m.PixOffset(s.at.Max.X, y)])
		}
		m.Palette = color.Palette{transparentEntry}
	}
	return m
}

// emptyGIFFrame is a frame of one transparent pixel at the canvas's corner,
// which draws nothing.
func emptyGIFFrame() *image.Paletted {
	return image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{transparentEntry})
}
