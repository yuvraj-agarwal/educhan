package media

import (
	"bytes"
	"compress/lzw"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// What a GIF shows is the frames drawn one on another, each for its delay,
// and that is all a stored GIF keeps of its frames: it is written from the
// run of pictures they show, one frame to a picture, by one rule, so that
// GIFs a browser shows alike are stored alike. These tests hold the stored
// file to what Chromium shows of the upload, through a model of its
// compositing written here independently of playGIF (browserPlay), and to
// the rule.

/* A browser, as far as a GIF goes ------------------------------------------ */

// onScreen is a picture a browser shows while a GIF plays, and for how long.
type onScreen struct {
	pic *image.NRGBA
	ms  int
}

// composite plays a decoded GIF as Chromium and Firefox do: on a canvas that
// starts transparent, each frame's pixels other than its transparent ones
// are drawn, the canvas is shown for the frame's delay, and the frame is then
// disposed of by its method — 2 clears its rectangle to transparent, 3 and 4
// put back what was there before it, and anything else leaves it. It returns
// what each frame is drawn on, and what shows while it does.
func composite(g *gif.GIF) (before, shown []*image.NRGBA) {
	canvas := image.NewNRGBA(image.Rect(0, 0, g.Config.Width, g.Config.Height))
	snapshot := func() *image.NRGBA {
		pic := image.NewNRGBA(canvas.Rect)
		copy(pic.Pix, canvas.Pix)
		return pic
	}
	for k, m := range g.Image {
		var disposal byte
		if g.Disposal != nil {
			disposal = g.Disposal[k]
		}
		before = append(before, snapshot())
		r := m.Rect.Intersect(canvas.Rect)
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				c := color.Color(color.RGBA{A: 0xFF})
				if v := m.Pix[m.PixOffset(x, y)]; int(v) < len(m.Palette) {
					c = m.Palette[v]
				}
				if nc := color.NRGBAModel.Convert(c).(color.NRGBA); nc.A != 0 {
					canvas.SetNRGBA(x, y, nc)
				}
			}
		}
		shown = append(shown, snapshot())
		switch disposal {
		case 2:
			for y := r.Min.Y; y < r.Max.Y; y++ {
				clear(canvas.Pix[canvas.PixOffset(r.Min.X, y):canvas.PixOffset(r.Max.X, y)])
			}
		case 3, 4:
			copy(canvas.Pix, before[k].Pix)
		}
	}
	return before, shown
}

// browserPlay is what a browser shows of a decoded GIF (composite): each
// picture, and for how long, a delay of 10 ms or less shown for 100 ms.
// Consecutive frames that show the same picture are one picture shown for
// their time together.
func browserPlay(g *gif.GIF) []onScreen {
	_, pictures := composite(g)
	var shows []onScreen
	for k, pic := range pictures {
		ms := 10 * g.Delay[k]
		if ms <= 10 {
			ms = 100
		}
		if n := len(shows); n > 0 && bytes.Equal(shows[n-1].pic.Pix, pic.Pix) {
			shows[n-1].ms += ms
			continue
		}
		shows = append(shows, onScreen{pic: pic, ms: ms})
	}
	return shows
}

// showDifference is how what a browser shows of one GIF differs from what it
// shows of another, or "" if it does not: the pictures, in order, each for
// its time, but for the time of the last of a GIF played once, which stays
// on screen once it is drawn, and how many times they are played, for more
// than one picture.
func showDifference(a, b *gif.GIF) string {
	if a.Config.Width != b.Config.Width || a.Config.Height != b.Config.Height {
		return fmt.Sprintf("a %dx%d canvas against %dx%d", a.Config.Width, a.Config.Height, b.Config.Width, b.Config.Height)
	}
	pa, pb := browserPlay(a), browserPlay(b)
	if len(pa) != len(pb) {
		return fmt.Sprintf("%d pictures in turn against %d", len(pa), len(pb))
	}
	for i := range pa {
		if !bytes.Equal(pa[i].pic.Pix, pb[i].pic.Pix) {
			return fmt.Sprintf("picture %d is %q against %q", i, pictureRows(pa[i].pic), pictureRows(pb[i].pic))
		}
		if len(pa) > 1 && (i < len(pa)-1 || a.LoopCount != -1) && pa[i].ms != pb[i].ms {
			return fmt.Sprintf("picture %d shown for %d ms against %d", i, pa[i].ms, pb[i].ms)
		}
	}
	if len(pa) > 1 && a.LoopCount != b.LoopCount {
		return fmt.Sprintf("loop count %d against %d", a.LoopCount, b.LoopCount)
	}
	return ""
}

// requireSamePlay checks that a stored GIF shows what its upload showed
// (showDifference): the same pictures, in the same order, each for the same
// time, and played as many times.
func requireSamePlay(t *testing.T, name string, upload, stored []byte) {
	t.Helper()
	up, err := gif.DecodeAll(bytes.NewReader(upload))
	if err != nil {
		t.Fatalf("%s: the upload does not decode: %v", name, err)
	}
	requirePlays(t, name, up, stored)
}

// requirePlays checks that a stored GIF shows what a browser shows of a
// decoded upload, read as the browser reads it.
func requirePlays(t *testing.T, name string, up *gif.GIF, stored []byte) {
	t.Helper()
	st, err := gif.DecodeAll(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("%s: the stored GIF does not decode: %v", name, err)
	}
	if d := showDifference(up, st); d != "" {
		t.Fatalf("%s: the stored GIF does not show what the upload does: %s (the upload's first)", name, d)
	}
}

// pictureRows is a picture as rows of its pixels' colours, each six hex
// digits, or "......" for a transparent one: the form the Chromium
// observations below are written in.
func pictureRows(pic *image.NRGBA) []string {
	var rows []string
	b := pic.Rect
	for y := b.Min.Y; y < b.Max.Y; y++ {
		var cells []string
		for x := b.Min.X; x < b.Max.X; x++ {
			c := pic.NRGBAAt(x, y)
			if c.A == 0 {
				cells = append(cells, "......")
				continue
			}
			cells = append(cells, fmt.Sprintf("%02x%02x%02x", c.R, c.G, c.B))
		}
		rows = append(rows, strings.Join(cells, " "))
	}
	return rows
}

/* A GIF written field by field --------------------------------------------- */

// rawGIF is a GIF written field by field, for files image/gif's encoder
// cannot make: frames with no graphic control extension or with two, a
// transparent index past the colour table, a table with its colours in any
// order or repeated, extensions between a frame's control and its pixels.
type rawGIF struct {
	w, h      int
	global    [][3]byte // the global colour table, or none
	bg        byte      // the background colour index
	loops     []rawLoop // loop extensions, after the global table
	frames    []rawFrame
	noTrailer bool
}

// rawLoop is a loop extension: NETSCAPE2.0 or ANIMEXTS1.0, and its count.
type rawLoop struct {
	id    string
	count int
}

// rawControl is a graphic control extension: a disposal method, a delay and a
// transparent index, or -1 for none.
type rawControl struct {
	disposal byte
	delay    int
	ti       int
}

// rawFrame is a frame and the extensions before it.
type rawFrame struct {
	controls  []rawControl // graphic control extensions before it, in order
	plainText bool         // a plain-text extension after them
	comment   string       // a comment extension after them, if not empty
	at        image.Rectangle
	local     [][3]byte // its own colour table, or none
	interlace bool
	pix       []uint8 // at.Dx()×at.Dy() indices, row by row
}

// rawTable appends a colour table, padded with black to a power of two, and
// returns the size field that says how long it is.
func rawTable(out []byte, t [][3]byte) ([]byte, byte) {
	bits := byte(0)
	for 2<<bits < len(t) {
		bits++
	}
	for i := 0; i < 2<<bits; i++ {
		var c [3]byte
		if i < len(t) {
			c = t[i]
		}
		out = append(out, c[:]...)
	}
	return out, bits
}

// bytes is the file.
func (g rawGIF) bytes() []byte {
	le := binary.LittleEndian
	out := le.AppendUint16(le.AppendUint16([]byte("GIF89a"), uint16(g.w)), uint16(g.h))
	if g.global != nil {
		out = append(out, 0, g.bg, 0)
		var bits byte
		out, bits = rawTable(out, g.global)
		out[10] = 0x80 | 0x70 | bits
	} else {
		out = append(out, 0, g.bg, 0)
	}
	for _, l := range g.loops {
		out = append(out, 0x21, 0xFF, 11)
		out = append(out, l.id...)
		out = append(out, 3, 1, byte(l.count), byte(l.count>>8), 0)
	}
	for _, f := range g.frames {
		for _, c := range f.controls {
			flags, ti := c.disposal<<2, byte(0)
			if c.ti >= 0 {
				flags, ti = flags|1, byte(c.ti)
			}
			out = append(out, 0x21, 0xF9, 4, flags, byte(c.delay), byte(c.delay>>8), ti, 0)
		}
		if f.plainText {
			out = append(out, 0x21, 0x01, 12, 0, 0, 0, 0, 8, 0, 8, 0, 8, 8, 1, 0, 3, 'a', 'b', 'c', 0)
		}
		if f.comment != "" {
			out = append(append(append(out, 0x21, 0xFE, byte(len(f.comment))), f.comment...), 0)
		}
		out = append(out, 0x2C)
		for _, v := range []int{f.at.Min.X, f.at.Min.Y, f.at.Dx(), f.at.Dy()} {
			out = le.AppendUint16(out, uint16(v))
		}
		table, flags := g.global, byte(0)
		if f.local != nil {
			table, flags = f.local, 0x80
		}
		if f.interlace {
			flags |= 0x40
		}
		out = append(out, flags)
		at := len(out) - 1
		if f.local != nil {
			var bits byte
			out, bits = rawTable(out, f.local)
			out[at] |= bits
		}
		lit := 2
		for 1<<lit < len(table) {
			lit++
		}
		for _, v := range f.pix {
			for int(v) >= 1<<lit {
				lit++
			}
		}
		pix := f.pix
		if f.interlace {
			w, h := f.at.Dx(), f.at.Dy()
			pix = nil
			for _, pass := range [][2]int{{0, 8}, {4, 8}, {2, 4}, {1, 2}} {
				for y := pass[0]; y < h; y += pass[1] {
					pix = append(pix, f.pix[y*w:(y+1)*w]...)
				}
			}
		}
		var lzwData bytes.Buffer
		lw := lzw.NewWriter(&lzwData, lzw.LSB, lit)
		lw.Write(pix)
		lw.Close()
		out = append(out, byte(lit))
		for d := lzwData.Bytes(); len(d) > 0; {
			n := min(255, len(d))
			out = append(append(out, byte(n)), d[:n]...)
			d = d[n:]
		}
		out = append(out, 0)
	}
	if !g.noTrailer {
		out = append(out, 0x3B)
	}
	return out
}

// chromiumReads is a rawGIF as Chromium reads its fields, decoded without
// image/gif, for browserPlay to play. Each frame's control is the last
// graphic control extension before it, and a frame with none has delay 0,
// disposal 0 and no transparent index; extensions between the control and
// the frame change nothing. An index past the end of the frame's colour
// table is opaque black, but for the transparent index, wherever that is.
// The loop count is the last loop extension's, NETSCAPE2.0 or ANIMEXTS1.0.
// Each was seen in Chromium (TestGIFIsReadAsChromiumReadsIt).
func chromiumReads(g rawGIF) *gif.GIF {
	out := &gif.GIF{LoopCount: -1, Config: image.Config{Width: g.w, Height: g.h}}
	for _, l := range g.loops {
		out.LoopCount = l.count
	}
	for _, f := range g.frames {
		c := rawControl{ti: -1}
		if n := len(f.controls); n > 0 {
			c = f.controls[n-1]
		}
		table := g.global
		if f.local != nil {
			table = f.local
		}
		pal := make(color.Palette, 256)
		for v := range pal {
			switch {
			case v == c.ti:
				pal[v] = color.RGBA{}
			case v < len(table):
				pal[v] = color.RGBA{table[v][0], table[v][1], table[v][2], 0xFF}
			default:
				pal[v] = color.RGBA{A: 0xFF}
			}
		}
		m := image.NewPaletted(f.at, pal)
		copy(m.Pix, f.pix)
		out.Image = append(out.Image, m)
		out.Delay = append(out.Delay, c.delay)
		out.Disposal = append(out.Disposal, c.disposal)
	}
	return out
}

// requireShowsAs checks that a stored GIF shows what Chromium shows of an
// upload written field by field (chromiumReads), as requireSamePlay does for
// one image/gif reads as Chromium does.
func requireShowsAs(t *testing.T, name string, upload rawGIF, stored []byte) {
	t.Helper()
	requirePlays(t, name, chromiumReads(upload), stored)
}

/* Fixtures ----------------------------------------------------------------- */

// playPalette is the colour table the frames below share: entry 0
// transparent, then red, green, blue and white.
var playPalette = color.Palette{
	color.RGBA{},
	color.RGBA{220, 30, 30, 255},
	color.RGBA{30, 200, 30, 255},
	color.RGBA{30, 30, 220, 255},
	color.RGBA{240, 240, 240, 255},
}

// Indices into playPalette.
const (
	clearPx = iota
	redPx
	greenPx
	bluePx
	whitePx
)

// playFrame is one frame of a test GIF: where it is, its pixels (repeated to
// fill it), its disposal method as the file gives it and its delay.
type playFrame struct {
	at       image.Rectangle
	pixels   []uint8
	disposal byte
	delay    int
	palette  color.Palette // playPalette when nil
}

// gifPlaying encodes a GIF of the frames on a w×h canvas with playPalette as
// its global table, and the loop count given (-1 for none).
func gifPlaying(t *testing.T, w, h, loop int, frames ...playFrame) []byte {
	t.Helper()
	g := &gif.GIF{LoopCount: loop, Config: image.Config{ColorModel: playPalette, Width: w, Height: h}}
	for _, f := range frames {
		pal := f.palette
		if pal == nil {
			pal = playPalette
		}
		m := image.NewPaletted(f.at, pal)
		for i := range m.Pix {
			m.Pix[i] = f.pixels[i%len(f.pixels)]
		}
		g.Image = append(g.Image, m)
		g.Delay = append(g.Delay, f.delay)
		g.Disposal = append(g.Disposal, f.disposal)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// storedGIF processes a GIF upload and decodes what is stored.
func storedGIF(t *testing.T, upload []byte) ([]byte, *gif.GIF) {
	t.Helper()
	got := process(t, upload)
	g, err := gif.DecodeAll(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatalf("the stored GIF does not decode: %v", err)
	}
	return got.Data, g
}

/* Tests -------------------------------------------------------------------- */

// Where image/gif and Chromium read a GIF's fields differently, the GIF is
// stored as Chromium shows it. Each case is a file Chromium was shown (through
// WebCodecs' ImageDecoder, which <img> shares its GIF decoder with) and the
// pictures it showed, each for how long; the stored file must show those, and
// chromiumReads, the model the other tests hold stored files to, must read
// the upload that way too. Pictures are rows of colours, "......" a
// transparent pixel; green, red and blue are 00ff00, ff0000 and 0000ff, and
// the table's first colour is black.
func TestGIFIsReadAsChromiumReadsIt(t *testing.T) {
	table := [][3]byte{{0, 0, 0}, {255, 0, 0}, {0, 255, 0}, {0, 0, 255}}
	control := func(disposal byte, delay, ti int) rawControl { return rawControl{disposal, delay, ti} }
	frame := func(x, w int, pix []uint8, controls ...rawControl) rawFrame {
		return rawFrame{controls: controls, at: image.Rect(x, 0, x+w, 1), pix: pix}
	}
	type shown struct {
		ms  int
		row string
	}
	for _, tc := range []struct {
		name   string
		w      int
		frames []rawFrame
		want   []shown
	}{{
		// image/gif keeps the first extension's transparent index, and
		// draws the red pixel transparent.
		name: "two controls, the last naming no transparent index",
		w:    2,
		frames: []rawFrame{
			frame(0, 2, []uint8{2, 2}, control(1, 50, -1)),
			frame(0, 2, []uint8{1, 3}, control(1, 60, 1), control(1, 70, -1)),
		},
		want: []shown{{500, "00ff00 00ff00"}, {700, "ff0000 0000ff"}},
	}, {
		name: "two controls, the last leaving the frame in place",
		w:    2,
		frames: []rawFrame{
			frame(0, 2, []uint8{2, 2}, control(1, 50, -1)),
			frame(0, 1, []uint8{1}, control(2, 60, -1), control(0, 70, -1)),
			frame(1, 1, []uint8{3}, control(1, 80, -1)),
		},
		want: []shown{{500, "00ff00 00ff00"}, {700, "ff0000 00ff00"}, {800, "ff0000 0000ff"}},
	}, {
		name: "a control, then a plain-text extension, then the frame",
		w:    2,
		frames: []rawFrame{
			frame(0, 2, []uint8{2, 2}, control(1, 50, -1)),
			func() rawFrame {
				f := frame(0, 1, []uint8{1}, control(2, 60, 1))
				f.plainText = true
				return f
			}(),
			frame(1, 1, []uint8{3}, control(1, 80, -1)),
		},
		want: []shown{{1100, "00ff00 00ff00"}, {800, "...... 0000ff"}},
	}, {
		// image/gif gives the frame with no control the disposal of the
		// one before it, and clears it once it has shown.
		name: "a frame with no control, after one that clears itself",
		w:    2,
		frames: []rawFrame{
			frame(0, 2, []uint8{2, 2}, control(1, 50, -1)),
			frame(0, 1, []uint8{1}, control(2, 60, 3)),
			frame(0, 2, []uint8{3, 1}),
			frame(1, 1, []uint8{0}, control(1, 80, -1)),
		},
		want: []shown{{500, "00ff00 00ff00"}, {600, "ff0000 00ff00"}, {100, "0000ff ff0000"}, {800, "0000ff 000000"}},
	}, {
		// The table has four entries and the transparent index is 6:
		// image/gif makes 4, 5 and 6 transparent, Chromium only 6.
		name: "a transparent index past the table",
		w:    4,
		frames: []rawFrame{
			frame(0, 4, []uint8{2, 2, 2, 2}, control(1, 50, -1)),
			frame(0, 4, []uint8{1, 4, 6, 5}, control(1, 60, 6)),
		},
		want: []shown{{500, "00ff00 00ff00 00ff00 00ff00"}, {600, "ff0000 000000 00ff00 000000"}},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			upload := rawGIF{w: tc.w, h: 1, global: table, loops: []rawLoop{{"NETSCAPE2.0", 0}}, frames: tc.frames}
			var modelled []shown
			for _, s := range browserPlay(chromiumReads(upload)) {
				modelled = append(modelled, shown{s.ms, pictureRows(s.pic)[0]})
			}
			if fmt.Sprint(modelled) != fmt.Sprint(tc.want) {
				t.Fatalf("chromiumReads shows %v, Chromium showed %v", modelled, tc.want)
			}
			requireShowsAs(t, tc.name, upload, process(t, upload.bytes()).Data)
		})
	}
}

// The reviewer's GIF: a 256×256 canvas with one opaque pixel, and then
// fifteen frames of one fully transparent pixel each, which draw nothing,
// whose positions and delays spell out a message, under a disposal method the
// specification reserves. A browser shows one red pixel for the whole of it.
// None of the fifteen is stored, so neither is the message, and the time they
// took is kept as the red pixel's; followed by a second picture, the stored
// GIF is still an animation, shown for exactly as long.
func TestGIFFramesThatDrawNothingCarryNothing(t *testing.T) {
	const secret = "the meeting is at 51.5N 0.12W; the code: 4492"
	if len(secret) != 45 {
		t.Fatalf("the secret is %d bytes, want 45, three to each of fifteen frames", len(secret))
	}
	// spelled is what a reader recovers from the frames after the first:
	// each one's left and top, and its delay.
	spelled := func(g *gif.GIF) string {
		var b strings.Builder
		for k := 1; k < len(g.Image); k++ {
			r := g.Image[k].Rect
			b.WriteByte(byte(r.Min.X))
			b.WriteByte(byte(r.Min.Y))
			b.WriteByte(byte(g.Delay[k]))
		}
		return b.String()
	}
	frames := []playFrame{{at: image.Rect(0, 0, 1, 1), pixels: []uint8{redPx}, disposal: gif.DisposalNone, delay: 50}}
	for i := 0; i < len(secret); i += 3 {
		x, y := int(secret[i]), int(secret[i+1])
		frames = append(frames, playFrame{
			at:       image.Rect(x, y, x+1, y+1),
			pixels:   []uint8{clearPx},
			disposal: 5,
			delay:    int(secret[i+2]),
		})
	}

	for _, tc := range []struct {
		name  string
		then  []playFrame
		count int
	}{
		{"alone", nil, 1},
		{"followed by a picture", []playFrame{{at: image.Rect(1, 1, 2, 2), pixels: []uint8{greenPx}, delay: 20}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upload := gifPlaying(t, 256, 256, 0, append(append([]playFrame(nil), frames...), tc.then...)...)
			up, err := gif.DecodeAll(bytes.NewReader(upload))
			if err != nil {
				t.Fatalf("the fixture does not decode: %v", err)
			}
			if !strings.Contains(spelled(up), secret) {
				t.Fatal("the upload's frames do not spell the secret; the test would prove nothing")
			}
			for k := 1; k <= 15; k++ {
				if up.Disposal[k] != 5 {
					t.Fatalf("frame %d of the fixture has disposal %d, want the reserved 5", k, up.Disposal[k])
				}
			}

			data, stored := storedGIF(t, upload)
			requireSamePlay(t, tc.name, upload, data)
			if s := spelled(stored); strings.Contains(s, secret[:3]) || strings.Contains(s, secret[3:6]) {
				t.Errorf("the stored frames still spell part of the secret: %q", s)
			}
			if len(stored.Image) != tc.count {
				t.Errorf("stored %d frames, want %d: the red pixel, and whatever shows after it", len(stored.Image), tc.count)
			}
			for k, d := range stored.Disposal {
				if d > gif.DisposalPrevious {
					t.Errorf("frame %d stored with disposal %d, which no browser reads as written", k, d)
				}
			}
		})
	}
}

// A GIF is stored one frame to a picture: frames that show the same picture
// one after another are one frame, shown for their time together. Each frame
// draws exactly the pixels in which its picture differs from what the frame
// before it left, and is cut to the smallest rectangle around them and the
// pixels the next picture has transparent where this one does not, which it
// is cleared of once its time is over; a frame clears nothing otherwise, and
// a picture that differs from what was left in nothing draws one transparent
// pixel at the corner. Each case is held to what a browser shows of the
// upload, and to the frames that rule makes of it.
func TestGIFIsStoredOneFrameToAPicture(t *testing.T) {
	rect := image.Rect
	base := playFrame{at: rect(0, 0, 8, 8), pixels: []uint8{whitePx}, disposal: gif.DisposalNone, delay: 40}
	red := func(r image.Rectangle, disposal byte, delay int) playFrame {
		return playFrame{at: r, pixels: []uint8{redPx}, disposal: disposal, delay: delay}
	}
	empty := func(r image.Rectangle, disposal byte, delay int) playFrame {
		return playFrame{at: r, pixels: []uint8{clearPx}, disposal: disposal, delay: delay}
	}
	green := playFrame{at: rect(7, 7, 8, 8), pixels: []uint8{greenPx}, disposal: gif.DisposalNone, delay: 30}

	// stored is what each stored frame should be: where it is, its delay and
	// its disposal, and whether it draws nothing at all.
	type stored struct {
		at       image.Rectangle
		delay    int
		disposal byte
		empty    bool
	}
	for _, tc := range []struct {
		name   string
		loop   int
		frames []playFrame
		want   []stored
	}{
		{
			name: "a first frame that draws nothing",
			loop: 0,
			frames: []playFrame{
				empty(rect(5, 5, 6, 6), gif.DisposalNone, 7),
				empty(rect(3, 3, 5, 5), gif.DisposalPrevious, 3),
				red(rect(1, 1, 3, 3), gif.DisposalNone, 20),
				green,
			},
			want: []stored{
				{rect(0, 0, 1, 1), 10, gif.DisposalNone, true},
				{rect(1, 1, 3, 3), 20, gif.DisposalNone, false},
				{rect(7, 7, 8, 8), 30, gif.DisposalNone, false},
			},
		},
		{
			// The red square shows for both frames' time, and is cleared
			// where the next picture is transparent; the next frame draws
			// what is left of it again.
			name: "a frame that draws nothing and clears what is under it",
			loop: 0,
			frames: []playFrame{
				red(rect(0, 0, 4, 4), gif.DisposalNone, 10),
				empty(rect(2, 2, 6, 6), gif.DisposalBackground, 5),
				green,
			},
			want: []stored{
				{rect(0, 0, 4, 4), 15, gif.DisposalBackground, false},
				{rect(0, 0, 8, 8), 30, gif.DisposalNone, false},
			},
		},
		{
			name: "a frame that draws nothing and clears nothing",
			loop: 0,
			frames: []playFrame{
				red(rect(0, 0, 2, 2), gif.DisposalNone, 10),
				empty(rect(4, 4, 6, 6), gif.DisposalBackground, 5),
				green,
			},
			want: []stored{
				{rect(0, 0, 2, 2), 15, gif.DisposalNone, false},
				{rect(7, 7, 8, 8), 30, gif.DisposalNone, false},
			},
		},
		{
			name: "frames that draw nothing after one that clears itself",
			loop: 0,
			frames: []playFrame{
				red(rect(0, 0, 2, 2), gif.DisposalBackground, 10),
				empty(rect(4, 4, 6, 6), gif.DisposalNone, 20),
				empty(rect(1, 4, 2, 6), 6, 5),
				green,
			},
			want: []stored{
				{rect(0, 0, 2, 2), 10, gif.DisposalBackground, false},
				{rect(0, 0, 1, 1), 25, gif.DisposalNone, true},
				{rect(7, 7, 8, 8), 30, gif.DisposalNone, false},
			},
		},
		{
			// What the red square was drawn over shows again, and is drawn
			// again: no frame is stored to put anything back.
			name: "a frame that draws nothing after one that puts back what was under it",
			loop: 0,
			frames: []playFrame{
				base,
				red(rect(0, 0, 2, 2), gif.DisposalPrevious, 10),
				empty(rect(4, 4, 6, 6), gif.DisposalNone, 20),
				green,
			},
			want: []stored{
				{rect(0, 0, 8, 8), 40, gif.DisposalNone, false},
				{rect(0, 0, 2, 2), 10, gif.DisposalNone, false},
				{rect(0, 0, 2, 2), 20, gif.DisposalNone, false},
				{rect(7, 7, 8, 8), 30, gif.DisposalNone, false},
			},
		},
		{
			name: "frames that paint what is already there",
			loop: 0,
			frames: []playFrame{
				red(rect(0, 0, 4, 4), gif.DisposalNone, 10),
				red(rect(1, 1, 3, 3), gif.DisposalNone, 5),
				red(rect(2, 2, 4, 4), gif.DisposalPrevious, 6),
				red(rect(0, 0, 1, 1), 7, 7),
				green,
			},
			want: []stored{
				{rect(0, 0, 4, 4), 28, gif.DisposalNone, false},
				{rect(7, 7, 8, 8), 30, gif.DisposalNone, false},
			},
		},
		{
			name: "pictures made through a clear and a put-back",
			loop: 0,
			frames: []playFrame{
				base,
				red(rect(0, 0, 2, 2), gif.DisposalBackground, 10),
				{at: rect(0, 0, 2, 2), pixels: []uint8{redPx, whitePx, redPx, redPx}, disposal: gif.DisposalPrevious, delay: 20},
				red(rect(0, 0, 2, 2), gif.DisposalNone, 30),
				green,
			},
			want: []stored{
				{rect(0, 0, 8, 8), 40, gif.DisposalNone, false},
				{rect(0, 0, 2, 2), 10, gif.DisposalNone, false},
				{rect(1, 0, 2, 1), 20, gif.DisposalNone, false},
				{rect(1, 0, 2, 1), 30, gif.DisposalNone, false},
				{rect(7, 7, 8, 8), 30, gif.DisposalNone, false},
			},
		},
		{
			name: "a frame drawn over transparent margins",
			loop: 0,
			frames: []playFrame{
				{at: rect(0, 0, 6, 6), pixels: []uint8{
					clearPx, clearPx, clearPx, clearPx, clearPx, clearPx,
					clearPx, clearPx, clearPx, clearPx, clearPx, clearPx,
					clearPx, clearPx, redPx, bluePx, clearPx, clearPx,
					clearPx, clearPx, clearPx, redPx, clearPx, clearPx,
					clearPx, clearPx, clearPx, clearPx, clearPx, clearPx,
					clearPx, clearPx, clearPx, clearPx, clearPx, clearPx,
				}, disposal: gif.DisposalNone, delay: 10},
				green,
			},
			want: []stored{
				{rect(2, 2, 4, 4), 10, gif.DisposalNone, false},
				{rect(7, 7, 8, 8), 30, gif.DisposalNone, false},
			},
		},
		{
			name: "a first frame that puts back what was under it",
			loop: 0,
			frames: []playFrame{
				red(rect(0, 0, 2, 2), gif.DisposalPrevious, 10),
				green,
			},
			want: []stored{
				{rect(0, 0, 2, 2), 10, gif.DisposalBackground, false},
				{rect(7, 7, 8, 8), 30, gif.DisposalNone, false},
			},
		},
		{
			// Frame 2 puts back the white under frame 1 and draws a red
			// square of its own, so the stored frame of its picture draws
			// both.
			name: "reserved disposal methods, short delays and the last frame's disposal",
			loop: 2,
			frames: []playFrame{
				base,
				red(rect(0, 0, 2, 2), 4, 0),
				red(rect(2, 2, 4, 4), 5, 1),
				red(rect(4, 4, 6, 6), 0, 2),
				red(rect(6, 6, 8, 8), gif.DisposalBackground, 3),
			},
			want: []stored{
				{rect(0, 0, 8, 8), 40, gif.DisposalNone, false},
				{rect(0, 0, 2, 2), 10, gif.DisposalNone, false},
				{rect(0, 0, 4, 4), 10, gif.DisposalNone, false},
				{rect(4, 4, 6, 6), 2, gif.DisposalNone, false},
				{rect(6, 6, 8, 8), 3, gif.DisposalNone, false},
			},
		},
		{
			name: "played once",
			loop: -1,
			frames: []playFrame{
				red(rect(0, 0, 2, 2), gif.DisposalNone, 30),
				{at: rect(7, 7, 8, 8), pixels: []uint8{greenPx}, disposal: gif.DisposalNone, delay: 500},
				empty(rect(1, 1, 2, 2), gif.DisposalBackground, 900),
			},
			want: []stored{
				{rect(0, 0, 2, 2), 30, gif.DisposalNone, false},
				{rect(7, 7, 8, 8), 0, gif.DisposalNone, false},
			},
		},
		{
			name: "played once, ending on what a frame cleared",
			loop: -1,
			frames: []playFrame{
				red(rect(0, 0, 2, 2), gif.DisposalNone, 30),
				{at: rect(7, 7, 8, 8), pixels: []uint8{greenPx}, disposal: gif.DisposalBackground, delay: 500},
				empty(rect(1, 1, 2, 2), gif.DisposalNone, 900),
			},
			want: []stored{
				{rect(0, 0, 2, 2), 30, gif.DisposalNone, false},
				{rect(7, 7, 8, 8), 500, gif.DisposalBackground, false},
				{rect(0, 0, 1, 1), 0, gif.DisposalNone, true},
			},
		},
		{
			name: "played forever",
			loop: 0,
			frames: []playFrame{
				red(rect(0, 0, 2, 2), gif.DisposalNone, 30),
				{at: rect(7, 7, 8, 8), pixels: []uint8{greenPx}, disposal: gif.DisposalNone, delay: 500},
				empty(rect(1, 1, 2, 2), gif.DisposalNone, 900),
			},
			want: []stored{
				{rect(0, 0, 2, 2), 30, gif.DisposalNone, false},
				{rect(7, 7, 8, 8), 1400, gif.DisposalNone, false},
			},
		},
		{
			name: "more time than one delay can say",
			loop: 0,
			frames: []playFrame{
				red(rect(0, 0, 2, 2), gif.DisposalNone, 65535),
				empty(rect(1, 1, 2, 2), gif.DisposalNone, 65535),
				empty(rect(1, 1, 2, 2), gif.DisposalNone, 2),
				{at: rect(2, 2, 3, 3), pixels: []uint8{bluePx}, disposal: gif.DisposalNone, delay: 65534},
				empty(rect(1, 1, 2, 2), gif.DisposalNone, 2),
				{at: rect(3, 3, 4, 4), pixels: []uint8{whitePx}, disposal: gif.DisposalNone, delay: 65535},
				empty(rect(1, 1, 2, 2), gif.DisposalNone, 1),
				green,
			},
			want: []stored{
				{rect(0, 0, 2, 2), 65535, gif.DisposalNone, false},
				{rect(0, 0, 1, 1), 65535, gif.DisposalNone, true},
				{rect(0, 0, 1, 1), 2, gif.DisposalNone, true},
				{rect(2, 2, 3, 3), 65534, gif.DisposalNone, false},
				{rect(0, 0, 1, 1), 2, gif.DisposalNone, true},
				{rect(3, 3, 4, 4), 65535, gif.DisposalNone, false},
				{rect(0, 0, 1, 1), 10, gif.DisposalNone, true},
				{rect(7, 7, 8, 8), 30, gif.DisposalNone, false},
			},
		},
		{
			// The frame of the red square is left in place for the first
			// of its time; what draws nothing and holds the rest is
			// cleared once it is over, of the square's rectangle.
			name: "more time than one delay can say, then cleared",
			loop: 0,
			frames: []playFrame{
				red(rect(0, 0, 2, 2), gif.DisposalNone, 65535),
				empty(rect(0, 0, 2, 2), gif.DisposalBackground, 100),
				green,
			},
			want: []stored{
				{rect(0, 0, 2, 2), 65535, gif.DisposalNone, false},
				{rect(0, 0, 2, 2), 100, gif.DisposalBackground, true},
				{rect(7, 7, 8, 8), 30, gif.DisposalNone, false},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upload := gifPlaying(t, 8, 8, tc.loop, tc.frames...)
			data, g := storedGIF(t, upload)
			requireSamePlay(t, tc.name, upload, data)
			if len(g.Image) != len(tc.want) {
				t.Fatalf("stored %d frames, want %d", len(g.Image), len(tc.want))
			}
			for k, want := range tc.want {
				m := g.Image[k]
				empty := true
				for _, v := range m.Pix {
					if _, _, _, a := m.Palette[v].RGBA(); a != 0 {
						empty = false
					}
				}
				if m.Rect != want.at || g.Delay[k] != want.delay || g.Disposal[k] != want.disposal || empty != want.empty {
					t.Errorf("frame %d stored at %v, delay %d, disposal %d, drawing nothing %v; want %v, %d, %d, %v",
						k, m.Rect, g.Delay[k], g.Disposal[k], empty, want.at, want.delay, want.disposal, want.empty)
				}
			}
		})
	}
}

// A GIF none of whose frames after the first changes anything on screen is
// stored as a still: one frame, with no delay, no disposal and no loop count,
// none of which a still picture shows.
func TestGIFOfOnePictureIsStoredAsAStill(t *testing.T) {
	upload := gifPlaying(t, 8, 8, 3,
		playFrame{at: image.Rect(1, 1, 3, 3), pixels: []uint8{redPx, bluePx}, disposal: gif.DisposalNone, delay: 70},
		playFrame{at: image.Rect(1, 1, 3, 3), pixels: []uint8{redPx, bluePx}, disposal: gif.DisposalPrevious, delay: 70},
		playFrame{at: image.Rect(0, 0, 8, 8), pixels: []uint8{clearPx}, disposal: 7, delay: 9},
	)
	if !bytes.Contains(upload, []byte("NETSCAPE2.0")) {
		t.Fatal("the fixture has no loop count")
	}
	data, g := storedGIF(t, upload)
	requireSamePlay(t, "one picture", upload, data)
	if len(g.Image) != 1 || g.Delay[0] != 0 || g.Disposal[0] != 0 {
		t.Errorf("stored %d frames, the first with delay %d and disposal %d; want one, with neither", len(g.Image), g.Delay[0], g.Disposal[0])
	}
	if bytes.Contains(data, []byte("NETSCAPE2.0")) {
		t.Error("the stored still carries a loop count")
	}
}

// The fields of a GIF browsers read past, or read alike whatever they hold,
// are written the same whatever the upload had in them: the background
// colour, which browsers never draw, and whether a frame carries a colour
// table of its own or uses the global one, which they show alike. Two
// uploads that differ only there are stored as the same bytes.
func TestGIFFieldsBrowsersDoNotShowAreWrittenTheSame(t *testing.T) {
	frames := []playFrame{
		{at: image.Rect(0, 0, 8, 8), pixels: []uint8{whitePx, redPx}, disposal: gif.DisposalNone, delay: 20},
		{at: image.Rect(2, 2, 6, 6), pixels: []uint8{bluePx, clearPx, greenPx}, disposal: gif.DisposalBackground, delay: 30},
		{at: image.Rect(0, 0, 4, 4), pixels: []uint8{greenPx, redPx}, disposal: gif.DisposalNone, delay: 40},
	}
	plain := gifPlaying(t, 8, 8, 0, frames...)

	// The same frames, with frame 1 carrying a table of its own: the shared
	// colours in another order, its pixels pointing at them there.
	own := color.Palette{playPalette[greenPx], playPalette[clearPx], playPalette[bluePx], playPalette[whitePx], playPalette[redPx]}
	at := map[uint8]uint8{greenPx: 0, clearPx: 1, bluePx: 2, whitePx: 3, redPx: 4}
	local := append([]playFrame(nil), frames...)
	local[1].palette = own
	local[1].pixels = []uint8{at[bluePx], at[clearPx], at[greenPx]}
	withOwnTable := gifPlaying(t, 8, 8, 0, local...)
	if _, tables := gifTables(t, withOwnTable); !tables[1] || tables[0] || tables[2] {
		t.Fatalf("the fixture's frames carry tables of their own %v; want only frame 1's", tables)
	}

	// The same again with a background colour set in the header.
	withBackground := append([]byte(nil), plain...)
	withBackground[11] = 3

	want := process(t, plain).Data
	requireSamePlay(t, "plain", plain, want)
	for name, upload := range map[string][]byte{"a table of its own": withOwnTable, "a background colour": withBackground} {
		got := process(t, upload).Data
		requireSamePlay(t, name, upload, got)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: stored differently from the same frames without it", name)
		}
	}
	if want[11] != 0 {
		t.Errorf("stored background colour %d, want 0", want[11])
	}
}

// shades is n colours no other shade is, the first of them the one given.
func shades(first, n int) [][3]byte {
	t := make([][3]byte, n)
	for i := range t {
		t[i] = [3]byte{byte(first + i), byte((first + i) >> 8), 0x55}
	}
	return t
}

// indices is the indices 0 to n-1, in order.
func indices(n int) []uint8 {
	pix := make([]uint8, n)
	for i := range pix {
		pix[i] = uint8(i)
	}
	return pix
}

// A GIF of more than 255 colours is stored with a colour table a frame: the
// colours the frame draws, in order of their values, after a transparent
// entry where it has a pixel it does not draw. A frame that draws 256
// colours has no room for that entry, so each pixel it does not draw is
// written in the colour it already shows, which the frame draws elsewhere.
// Where leaving a frame in place or clearing it would have the next frame
// draw more colours than that, the frame is put back instead, if that
// leaves the next one few enough. A GIF that no way of disposing of a frame
// leaves few enough cannot be stored one frame to a picture, and is refused
// (ErrImageTooCostly).
func TestGIFOfManyColoursHasATableAFrame(t *testing.T) {
	frame := func(at image.Rectangle, disposal byte, ti int, table [][3]byte, pix []uint8) rawFrame {
		return rawFrame{controls: []rawControl{{disposal, 10, ti}}, at: at, local: table, pix: pix}
	}
	sortedOf := func(tables ...[][3]byte) [][3]byte {
		var all [][3]byte
		for _, t := range tables {
			all = append(all, t...)
		}
		slices.SortFunc(all, func(a, b [3]byte) int { return bytes.Compare(a[:], b[:]) })
		return slices.Compact(all)
	}
	// stores requires the GIF stored as it shows, each frame with a table
	// of its own holding want[k], after a transparent entry if clear[k],
	// and returns it decoded.
	stores := func(name string, g rawGIF, want [][][3]byte, clear []bool) *gif.GIF {
		t.Helper()
		data := process(t, g.bytes()).Data
		requireShowsAs(t, name, g, data)
		st, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: the stored GIF does not decode: %v", name, err)
		}
		if global, local := gifTables(t, data); global != nil || slices.Contains(local, false) || len(local) != len(want) {
			t.Fatalf("%s: stored a global table of %d bytes and frames with tables of their own %v; want %d frames, each with one", name, len(global), local, len(want))
		}
		for k, m := range st.Image {
			var table [][3]byte
			first := 0
			if clear[k] {
				if _, _, _, a := m.Palette[0].RGBA(); a != 0 {
					t.Errorf("%s: frame %d's entry 0 is not transparent", name, k)
				}
				first = 1
			}
			for _, c := range m.Palette[first:][:len(want[k])] {
				r, g, b, a := c.RGBA()
				if a == 0 {
					t.Errorf("%s: frame %d has a transparent entry it should not", name, k)
				}
				table = append(table, [3]byte{byte(r >> 8), byte(g >> 8), byte(b >> 8)})
			}
			if !slices.Equal(table, want[k]) {
				t.Errorf("%s: frame %d's table is not the colours it draws, in order", name, k)
			}
		}
		return st
	}

	// 200 colours, then 100 more on every other pixel of the rows below.
	checks := make([]uint8, 200)
	for i := range checks {
		checks[i] = 100
		if (i+i/20)%2 == 0 {
			checks[i] = uint8(i / 2)
		}
	}
	stores("more than 255 colours", rawGIF{w: 20, h: 20, loops: []rawLoop{{"NETSCAPE2.0", 0}}, frames: []rawFrame{
		frame(image.Rect(0, 0, 20, 10), keepFrame, -1, shades(0, 200), indices(200)),
		frame(image.Rect(0, 10, 20, 20), keepFrame, 100, shades(200, 100), checks),
	}}, [][][3]byte{sortedOf(shades(0, 200)), sortedOf(shades(200, 100))}, []bool{false, true})

	// A row of one colour with a sixth shade in the middle, then 256 shades
	// over it that leave that one as it is.
	a := [][3]byte{{1, 2, 3}}
	under := make([]uint8, 257)
	under[128] = 1
	over := make([]uint8, 257)
	for x := range over {
		over[x] = uint8(x)
		if x > 128 {
			over[x] = uint8(x - 1)
		}
	}
	over[128] = 5
	stores("256 colours and a pixel left as it is", rawGIF{w: 257, h: 1, loops: []rawLoop{{"NETSCAPE2.0", 0}}, frames: []rawFrame{
		frame(image.Rect(0, 0, 257, 1), keepFrame, -1, append(a, shades(5, 1)...), under),
		frame(image.Rect(0, 0, 257, 1), keepFrame, -1, shades(0, 256), over),
	}}, [][][3]byte{sortedOf(a, shades(5, 1)), sortedOf(shades(0, 256))}, []bool{false, false})

	// A frame put back where what was under it and what the next frame
	// draws come to more colours than one table holds; and to 256 of them
	// around a pixel that shows nothing. Left in place or cleared, the
	// frame would leave the next one to draw both.
	for _, c := range []struct {
		name  string
		g     rawGIF
		want  [][][3]byte
		clear []bool
	}{
		{"more than 256 colours", rawGIF{w: 300, h: 1, frames: []rawFrame{
			frame(image.Rect(0, 0, 150, 1), keepFrame, -1, shades(0, 150), indices(150)),
			frame(image.Rect(0, 0, 150, 1), restoreFrame, -1, shades(1000, 150), indices(150)),
			frame(image.Rect(150, 0, 300, 1), keepFrame, -1, shades(2000, 150), indices(150)),
		}}, [][][3]byte{sortedOf(shades(0, 150)), sortedOf(shades(1000, 150)), sortedOf(shades(2000, 150))}, []bool{false, false, false}},
		{"256 colours and a pixel that shows nothing", rawGIF{w: 257, h: 1, frames: []rawFrame{
			frame(image.Rect(0, 0, 128, 1), keepFrame, -1, shades(0, 128), indices(128)),
			frame(image.Rect(0, 0, 128, 1), restoreFrame, -1, a, make([]uint8, 128)),
			frame(image.Rect(129, 0, 257, 1), keepFrame, -1, shades(1000, 128), indices(128)),
		}}, [][][3]byte{sortedOf(shades(0, 128)), a, sortedOf(shades(1000, 128))}, []bool{false, false, false}},
	} {
		name := "a frame put back, rather than its next drawing " + c.name
		st := stores(name, c.g, c.want, c.clear)
		if want := []byte{keepFrame, restoreFrame, keepFrame}; !bytes.Equal(st.Disposal, want) {
			t.Errorf("%s: stored with disposals %v, want %v", name, st.Disposal, want)
		}
	}

	// Two frames put back in turn, the second to the picture the first
	// showed: once the second is over, whichever way it is disposed of, the
	// next frame has to draw what the first frame covered and what it draws
	// itself, 300 colours.
	g := rawGIF{w: 300, h: 1, frames: []rawFrame{
		frame(image.Rect(0, 0, 150, 1), keepFrame, -1, shades(0, 150), indices(150)),
		frame(image.Rect(0, 0, 150, 1), restoreFrame, -1, shades(1000, 150), indices(150)),
		frame(image.Rect(0, 0, 150, 1), restoreFrame, -1, shades(2000, 150), indices(150)),
		frame(image.Rect(150, 0, 300, 1), keepFrame, -1, shades(3000, 150), indices(150)),
	}}
	if _, err := Process(context.Background(), g.bytes()); !errors.Is(err, ErrImageTooCostly) {
		t.Errorf("frames put back to a picture no frame can: Process returned %v, want ErrImageTooCostly", err)
	}
}

// An animation as ImageMagick optimises one, with a table of up to 256
// colours to each frame, can put a frame back where nothing else would do:
// ImageMagick's -layers OptimizeFrame did, for a sprite over a background
// of many colours, and the GIF was refused until frames were put back in
// the stored file too. Here a background of 256 colours has a spark shown
// over its left half and put back, and then a figure of 256 colours drawn
// over its right half: left in place or cleared, the spark would leave the
// figure's frame to draw the background's left half again as well, 384
// colours. A spark the upload draws twice, to show it for longer than one
// delay can say, is stored drawn for as much of that time as one delay
// holds and again for the rest, and put back each time.
func TestGIFOfManyColoursPutsAFrameBack(t *testing.T) {
	background, spark, figure := make([][3]byte, 256), make([][3]byte, 128), make([][3]byte, 256)
	for i := range background {
		background[i] = [3]byte{byte(i), 0x40, byte(255 - i)}
		figure[i] = [3]byte{byte(i), 0xC0, byte(i / 2)}
	}
	for i := range spark {
		spark[i] = [3]byte{0xF0, byte(i), byte(2 * i)}
	}
	// Each of the background's colours is three pixels of a row, and each
	// of the spark's six; the figure's are pixels in turn.
	pixels := func(at image.Rectangle, index func(x, y int) int) []uint8 {
		var pix []uint8
		for y := at.Min.Y; y < at.Max.Y; y++ {
			for x := at.Min.X; x < at.Max.X; x++ {
				pix = append(pix, uint8(index(x, y)))
			}
		}
		return pix
	}
	whole, left, right := image.Rect(0, 0, 48, 16), image.Rect(0, 0, 24, 16), image.Rect(24, 0, 48, 16)
	// The spark is drawn once for each of the delays given, and put back
	// after each.
	animation := func(sparkTimes ...int) rawGIF {
		g := rawGIF{w: 48, h: 16, loops: []rawLoop{{"NETSCAPE2.0", 0}}, frames: []rawFrame{
			{controls: []rawControl{{keepFrame, 8, -1}}, at: whole, local: background,
				pix: pixels(whole, func(x, y int) int { return x/3 + 16*y })},
		}}
		for _, d := range sparkTimes {
			g.frames = append(g.frames, rawFrame{controls: []rawControl{{restoreFrame, d, -1}}, at: left, local: spark,
				pix: pixels(left, func(x, y int) int { return x/6 + 4*y })})
		}
		g.frames = append(g.frames, rawFrame{controls: []rawControl{{keepFrame, 8, -1}}, at: right, local: figure,
			pix: pixels(right, func(x, y int) int { return (x - 24 + 24*y) % 256 })})
		return g
	}
	for _, c := range []struct {
		name      string
		g         rawGIF
		disposals []byte
		delays    []int
	}{
		{"a spark put back", animation(8), []byte{keepFrame, restoreFrame, keepFrame}, []int{8, 8, 8}},
		{"a spark shown for longer than a delay can say, put back", animation(40000, 30000),
			[]byte{keepFrame, restoreFrame, restoreFrame, keepFrame}, []int{8, maxGIFDelay, 70000 - maxGIFDelay, 8}},
	} {
		g := c.g
		got, err := Process(context.Background(), g.bytes())
		if err != nil {
			t.Fatalf("%s: Process returned %v, want it stored", c.name, err)
		}
		requireShowsAs(t, c.name, g, got.Data)
		st, err := gif.DecodeAll(bytes.NewReader(got.Data))
		if err != nil {
			t.Fatalf("%s: the stored GIF does not decode: %v", c.name, err)
		}
		if !bytes.Equal(st.Disposal, c.disposals) || !slices.Equal(st.Delay, c.delays) {
			t.Errorf("%s: stored with disposals %v and delays %v, want %v and %v", c.name, st.Disposal, st.Delay, c.disposals, c.delays)
		}
		if global, local := gifTables(t, got.Data); global != nil || slices.Contains(local, false) {
			t.Errorf("%s: stored with a global table of %d bytes and tables of their own %v; want a table a frame", c.name, len(global), local)
		}
		if again := process(t, got.Data).Data; !bytes.Equal(again, got.Data) {
			t.Errorf("%s: processing the stored GIF again stores it differently", c.name)
		}
	}
}

// What choose looks at for a way it does not take is charged to the stored
// frames' pixels, as if a frame of it had been written, so the work a GIF of
// a table a frame takes stays within maxGIFFramePixels. Here frames of 256
// colours, 16 pixels square, at opposite corners of a canvas of a megapixel,
// are put back by turns: after each over the top left, the next picture
// shows the colours it covered again and the frame at the bottom right as
// well. Left in place or cleared, it would leave the next frame to draw
// both, 512 colours, in a rectangle of the whole canvas, which choose looks
// at twice before putting it back. Uncharged, every such picture had choose
// look at two canvases of which nothing is written, and an upload of a few
// megabytes of these frames on a 25-megapixel canvas would have taken
// minutes; charged, fifty of them come to the limit.
func TestGIFWaysNotTakenAreCharged(t *testing.T) {
	const side = 1000
	topLeft, bottomRight := image.Rect(0, 0, 16, 16), image.Rect(side-16, side-16, side, side)
	animation := func(n int) rawGIF {
		g := rawGIF{w: side, h: side, frames: []rawFrame{
			{controls: []rawControl{{keepFrame, 10, -1}}, at: topLeft, local: shades(0, 256), pix: indices(256)},
		}}
		for k := 1; k < n; k++ {
			at, table := topLeft, shades(1000, 256)
			if k%2 == 0 {
				at, table = bottomRight, shades(2000, 256)
			}
			g.frames = append(g.frames, rawFrame{controls: []rawControl{{restoreFrame, 10, -1}}, at: at, local: table, pix: indices(256)})
		}
		return g
	}

	g := animation(21)
	got, err := Process(context.Background(), g.bytes())
	if err != nil {
		t.Fatalf("21 frames: Process returned %v, want it stored", err)
	}
	requireShowsAs(t, "21 frames", g, got.Data)
	want := []byte{keepFrame}
	for k := 1; k < 20; k++ {
		want = append(want, []byte{clearFrame, restoreFrame}[k%2])
	}
	if want = append(want, keepFrame); !bytes.Equal(gifDisposals(t, got.Data), want) {
		t.Errorf("21 frames: stored with disposals %v, want %v", gifDisposals(t, got.Data), want)
	}

	if _, err := Process(context.Background(), animation(101).bytes()); !errors.Is(err, ErrImageTooCostly) {
		t.Errorf("101 frames, with 50 pictures after which choose looks at two canvases for ways it does not take: Process returned %v, want ErrImageTooCostly", err)
	}
}

// randomGIF is a GIF of up to eight frames on a small canvas, drawn at
// random from what a crafted file can say about its frames: where they are,
// pixels that are mostly transparent or repeat what is under them, any of the
// eight disposal values, delays from none to the most a delay can say, and
// any loop count.
func randomGIF(t *testing.T, rng *rand.Rand) []byte {
	t.Helper()
	w, h := 1+rng.Intn(6), 1+rng.Intn(6)
	delays := []int{0, 1, 2, 3, 10, 65534, 65535}
	var frames []playFrame
	for n := 1 + rng.Intn(8); len(frames) < n; {
		x0, y0 := rng.Intn(w), rng.Intn(h)
		r := image.Rect(x0, y0, x0+1+rng.Intn(w-x0), y0+1+rng.Intn(h-y0))
		pixels := make([]uint8, r.Dx()*r.Dy())
		colours := []uint8{clearPx, clearPx, redPx, redPx, greenPx}[:2+rng.Intn(4)]
		for i := range pixels {
			pixels[i] = colours[rng.Intn(len(colours))]
		}
		frames = append(frames, playFrame{at: r, pixels: pixels, disposal: byte(rng.Intn(8)), delay: delays[rng.Intn(len(delays))]})
	}
	return gifPlaying(t, w, h, []int{-1, 0, 1, 65535}[rng.Intn(4)], frames...)
}

// Whatever its frames, a GIF is stored as it plays, and as nothing more:
// processing a stored GIF again stores the same bytes, every frame of an
// animation is left in place or cleared, the last left in place, and no
// delay but the last of a GIF played once is one a browser does not show as
// written. These GIFs are of a few colours, in one table, whose frames are
// never put back; a GIF of a table a frame may have frames put back
// (TestGIFOfManyColoursPutsAFrameBack, TestGIFsShownAlikeAreStoredAlike).
func TestGIFIsStoredAsItPlays(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		upload := randomGIF(t, rng)
		name := fmt.Sprintf("GIF %d", i)
		data, g := storedGIF(t, upload)
		requireSamePlay(t, name, upload, data)
		if again := process(t, data).Data; !bytes.Equal(again, data) {
			t.Fatalf("%s: processing the stored GIF again stores it differently", name)
		}
		if data[11] != 0 {
			t.Errorf("%s: stored background colour %d, want 0", name, data[11])
		}
		for k, d := range g.Disposal {
			last := k == len(g.Image)-1
			switch {
			case len(g.Image) == 1 && (d != 0 || g.Delay[k] != 0):
				t.Errorf("%s: a still stored with disposal %d and delay %d, want neither", name, d, g.Delay[k])
			case len(g.Image) > 1 && (d != keepFrame && d != clearFrame || last && d != keepFrame):
				t.Errorf("%s: frame %d of %d stored with disposal %d", name, k, len(g.Image), d)
			case len(g.Image) > 1 && g.Delay[k] < 2 && !(last && g.LoopCount == -1 && g.Delay[k] == 0):
				t.Errorf("%s: frame %d stored with delay %d, which browsers do not show as written", name, k, g.Delay[k])
			}
		}
	}
}

/* GIFs shown alike ---------------------------------------------------------- */

// clone is a copy of g that shares nothing with it.
func (g rawGIF) clone() rawGIF {
	c := g
	c.global, c.loops = slices.Clone(g.global), slices.Clone(g.loops)
	c.frames = make([]rawFrame, len(g.frames))
	for k, f := range g.frames {
		f.controls, f.local, f.pix = slices.Clone(f.controls), slices.Clone(f.local), slices.Clone(f.pix)
		c.frames[k] = f
	}
	return c
}

// table is the colour table frame k's pixels index: its own, or the global
// one.
func (g rawGIF) table(k int) [][3]byte {
	if g.frames[k].local != nil {
		return g.frames[k].local
	}
	return g.global
}

// control is the graphic control extension Chromium reads a frame by: its
// last, or none, which is disposal 0, delay 0 and no transparent index.
func (f rawFrame) control() rawControl {
	if n := len(f.controls); n > 0 {
		return f.controls[n-1]
	}
	return rawControl{ti: -1}
}

// padded is a colour table with the black entries rawTable pads it with
// written out, so that it is as long as the file says it is.
func padded(t [][3]byte) [][3]byte {
	out, _ := rawTable(nil, t)
	p := make([][3]byte, len(out)/3)
	for i := range p {
		copy(p[i][:], out[3*i:])
	}
	return p
}

// randomRawGIF is a GIF of up to seven frames on a small canvas, written
// field by field and drawn at random from what a file can say: colour tables
// global and local, of colours from a pool of the size given (so that frames
// share them, or, from a pool of many, overflow one table between them),
// frames on and off the canvas, opaque, transparent or both, transparent
// indices within the table and past it, any disposal value and delay, no
// control, one or two, and loop counts or none.
func randomRawGIF(rng *rand.Rand, colours int) rawGIF {
	pool := make([][3]byte, colours)
	for i := range pool {
		pool[i] = [3]byte{byte(rng.Intn(256)), byte(rng.Intn(256)), byte(rng.Intn(256))}
	}
	least, most, low, side, whole, own := 1, 6, 1, 9, 4, 4
	if colours > 256 {
		least, most, low, side, whole, own = 128, 256, 16, 32, 2, 2
	}
	table := func() [][3]byte {
		t := make([][3]byte, least+rng.Intn(most-least+1))
		for i := range t {
			t[i] = pool[rng.Intn(len(pool))]
		}
		return t
	}
	g := rawGIF{w: low + rng.Intn(side-low+1), h: low + rng.Intn(side-low+1), bg: byte(rng.Intn(4))}
	canvas := image.Rect(0, 0, g.w, g.h)
	if rng.Intn(5) != 0 {
		g.global = table()
	}
	if rng.Intn(3) != 0 {
		g.loops = []rawLoop{{[]string{"NETSCAPE2.0", "ANIMEXTS1.0"}[rng.Intn(2)], []int{0, 1, 3, 65535}[rng.Intn(4)]}}
	}
	delays := []int{0, 1, 2, 3, 5, 10, 100, 65534, 65535}
	for n := 1 + rng.Intn(7); len(g.frames) < n; {
		var f rawFrame
		switch {
		case rng.Intn(whole) == 0:
			f.at = canvas
		case len(g.frames) == 0:
			x, y := rng.Intn(g.w), rng.Intn(g.h)
			f.at = image.Rect(x, y, x+rng.Intn(g.w-x+1), y+rng.Intn(g.h-y+1))
		default:
			x, y := rng.Intn(g.w+3), rng.Intn(g.h+3)
			f.at = image.Rect(x, y, x+rng.Intn(g.w+2), y+rng.Intn(g.h+2))
		}
		if g.global == nil || rng.Intn(own) == 0 {
			f.local = table()
		}
		t := f.local
		if t == nil {
			t = g.global
		}
		size := len(padded(t))
		c := rawControl{disposal: byte(rng.Intn(8)), delay: delays[rng.Intn(len(delays))], ti: -1}
		if rng.Intn(3) != 0 {
			c.ti = rng.Intn(min(size+1, 256)) // the size itself is past the table
		}
		switch rng.Intn(20) {
		case 0:
			c = rawControl{ti: -1}
		case 1:
			f.controls = []rawControl{{byte(rng.Intn(8)), delays[rng.Intn(len(delays))], rng.Intn(size+1) - 1}, c}
		default:
			f.controls = []rawControl{c}
		}
		// Opaque pixels, or any pixels, or mostly transparent ones; every
		// index is in the table as the file pads it, or the transparent one.
		opaque := func() uint8 {
			for {
				if v := rng.Intn(len(t)); v != c.ti {
					return uint8(v)
				}
				if len(t) == 1 {
					return uint8(min(size-1, 1))
				}
			}
		}
		mode := rng.Intn(3)
		f.pix = make([]uint8, f.at.Dx()*f.at.Dy())
		for i := range f.pix {
			switch {
			case mode == 0 || c.ti < 0:
				f.pix[i] = opaque()
			case mode == 1 && rng.Intn(3) != 0:
				f.pix[i] = uint8(rng.Intn(size))
			default:
				f.pix[i] = uint8(c.ti)
			}
		}
		f.interlace = rng.Intn(4) == 0
		g.frames = append(g.frames, f)
	}
	return g
}

// randomSpritesGIF is an animation drawn at random as ImageMagick draws
// one: a background over the whole canvas in up to 256 colours, then up to
// five sprites over large parts of it, each with a table of its own of 192
// to 256 colours from a pool of 1,000, most of them put back once shown and
// the rest cleared or left in place, some with a transparent index, and
// some put back and drawn again to show for longer than one delay can say.
// Put back over a background of many colours, a sprite leaves the next
// frame to draw the background again as well as its own; so these are the
// GIFs whose stored files have frames put back.
func randomSpritesGIF(rng *rand.Rand) rawGIF {
	pool := make([][3]byte, 1000)
	for i := range pool {
		pool[i] = [3]byte{byte(rng.Intn(256)), byte(rng.Intn(256)), byte(rng.Intn(256))}
	}
	table := func(n int) [][3]byte {
		t := make([][3]byte, n)
		for i := range t {
			t[i] = pool[rng.Intn(len(pool))]
		}
		return t
	}
	g := rawGIF{w: 24 + rng.Intn(25), h: 16 + rng.Intn(17)}
	if rng.Intn(2) == 0 {
		g.loops = []rawLoop{{"NETSCAPE2.0", []int{0, 2}[rng.Intn(2)]}}
	}
	delays := []int{2, 5, 10, 50}
	background := rawFrame{
		controls: []rawControl{{keepFrame, delays[rng.Intn(len(delays))], -1}},
		at:       image.Rect(0, 0, g.w, g.h),
		local:    table(256),
		pix:      make([]uint8, g.w*g.h),
	}
	for i := range background.pix {
		background.pix[i] = uint8(rng.Intn(256))
	}
	g.frames = append(g.frames, background)
	for n := 2 + rng.Intn(5); len(g.frames) < n; {
		w, h := g.w/3+rng.Intn(g.w-g.w/3+1), g.h/3+rng.Intn(g.h-g.h/3+1)
		x, y := rng.Intn(g.w-w+1), rng.Intn(g.h-h+1)
		f := rawFrame{at: image.Rect(x, y, x+w, y+h), local: table(192 + rng.Intn(65)), pix: make([]uint8, w*h)}
		c := rawControl{[]byte{keepFrame, restoreFrame, restoreFrame, 4, clearFrame}[rng.Intn(5)], delays[rng.Intn(len(delays))], -1}
		if rng.Intn(3) == 0 {
			c.ti = rng.Intn(len(f.local))
		}
		f.controls = []rawControl{c}
		for i := range f.pix {
			f.pix[i] = uint8(rng.Intn(len(f.local)))
		}
		g.frames = append(g.frames, f)
		if browserDisposal(c.disposal) == restoreFrame && rng.Intn(6) == 0 {
			again := f
			again.controls = []rawControl{{c.disposal, maxGIFDelay, c.ti}}
			g.frames[len(g.frames)-1].controls[0].delay = 40000
			g.frames = append(g.frames, again)
		}
	}
	return g
}

// gifRewrite rewrites a GIF as one a browser shows exactly as it shows the
// original, or reports that it cannot rewrite this one so.
type gifRewrite struct {
	name    string
	rewrite func(rng *rand.Rand, g rawGIF) (rawGIF, bool)
}

// keepDisposals are the disposal values that leave a frame where it is.
var keepDisposals = []byte{0, 1, 5, 6, 7}

// splitDelay splits the time a frame of delay d shows for into two delays a
// browser shows as written, if it is long enough.
func splitDelay(rng *rand.Rand, d int) (int, int, bool) {
	total := shownDelay(d)
	if total < 4 {
		return 0, 0, false
	}
	first := 2 + rng.Intn(total-3)
	return first, total - first, true
}

var gifRewrites = []gifRewrite{
	{"a colour table in another order", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		k := rng.Intn(len(g.frames)+1) - 1 // -1 is the global table
		if k < 0 && g.global == nil || k >= 0 && g.frames[k].local == nil {
			return g, false
		}
		g = g.clone()
		table := &g.global
		if k >= 0 {
			table = &g.frames[k].local
		}
		old := padded(*table)
		perm := rng.Perm(len(old))
		*table = make([][3]byte, len(old))
		for i, c := range old {
			(*table)[perm[i]] = c
		}
		for j := range g.frames {
			f := &g.frames[j]
			if j != k && (k >= 0 || f.local != nil) {
				continue
			}
			for i, v := range f.pix {
				if int(v) < len(old) {
					f.pix[i] = uint8(perm[v])
				}
			}
			for i, c := range f.controls {
				if c.ti >= 0 && c.ti < len(old) {
					f.controls[i].ti = perm[c.ti]
				}
			}
		}
		return g, true
	}},
	{"colours repeated in a table", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		k := rng.Intn(len(g.frames)+1) - 1
		if k < 0 && g.global == nil || k >= 0 && g.frames[k].local == nil {
			return g, false
		}
		g = g.clone()
		table := &g.global
		if k >= 0 {
			table = &g.frames[k].local
		}
		old := padded(*table)
		users := []int{k}
		if k < 0 {
			users = nil
			for j, f := range g.frames {
				if f.local == nil {
					users = append(users, j)
				}
			}
		}
		// A pixel past the table shows black, and would show a colour of
		// a longer one.
		for _, j := range users {
			ti := g.frames[j].control().ti
			for _, v := range g.frames[j].pix {
				if int(v) >= len(old) && int(v) != ti {
					return g, false
				}
			}
		}
		if len(old) >= 256 {
			return g, false
		}
		*table = old
		copies := map[int][]int{}
		for n := 1 + rng.Intn(256-len(old)); n > 0; n-- {
			i := rng.Intn(len(old))
			copies[i] = append(copies[i], len(*table))
			*table = append(*table, old[i])
		}
		for _, j := range users {
			f := &g.frames[j]
			ti := f.control().ti
			for i, v := range f.pix {
				// A copy written where the transparent index was past the
				// table is transparent, whatever colour it holds.
				c := slices.DeleteFunc(slices.Clone(copies[int(v)]), func(u int) bool { return u == ti })
				if int(v) != ti && len(c) > 0 && rng.Intn(2) == 0 {
					f.pix[i] = uint8(c[rng.Intn(len(c))])
				}
			}
		}
		return g, true
	}},
	{"the global table copied into the frames", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		if g.global == nil {
			return g, false
		}
		g = g.clone()
		for j := range g.frames {
			if g.frames[j].local == nil {
				g.frames[j].local = padded(g.global)
			}
		}
		if rng.Intn(2) == 0 {
			g.global = nil
		}
		return g, true
	}},
	{"a frame's table made the global one", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		k := rng.Intn(len(g.frames))
		if g.global != nil || g.frames[k].local == nil {
			return g, false
		}
		g = g.clone()
		g.global = padded(g.frames[k].local)
		for j := range g.frames {
			if f := &g.frames[j]; slices.Equal(padded(f.local), g.global) && rng.Intn(2) == 0 {
				f.local = nil
			}
		}
		return g, true
	}},
	{"pixels that draw the colour under them made transparent, and back", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		k := rng.Intn(len(g.frames))
		before, _ := composite(chromiumReads(g))
		g = g.clone()
		f := &g.frames[k]
		table, ti := padded(g.table(k)), f.control().ti
		changed := false
		for y := f.at.Min.Y; y < f.at.Max.Y; y++ {
			for x := f.at.Min.X; x < f.at.Max.X; x++ {
				i := (y-f.at.Min.Y)*f.at.Dx() + x - f.at.Min.X
				if !image.Pt(x, y).In(before[k].Rect) {
					f.pix[i], changed = uint8(rng.Intn(len(table))), true // off the canvas, nothing shows
					continue
				}
				under := before[k].NRGBAAt(x, y)
				if rng.Intn(2) == 0 || under.A == 0 {
					continue
				}
				colour := [3]byte{under.R, under.G, under.B}
				if v := int(f.pix[i]); v == ti {
					var same []int
					for u, c := range table {
						if u != ti && c == colour {
							same = append(same, u)
						}
					}
					if len(same) > 0 {
						f.pix[i], changed = uint8(same[rng.Intn(len(same))]), true
					}
				} else if ti >= 0 && v < len(table) && table[v] == colour {
					f.pix[i], changed = uint8(ti), true
				}
			}
		}
		return g, changed
	}},
	{"a frame's time split with a frame that draws nothing", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		k := rng.Intn(len(g.frames))
		c := g.frames[k].control()
		disposal := browserDisposal(c.disposal)
		first, rest, ok := splitDelay(rng, c.delay)
		if !ok || disposal == restoreFrame {
			return g, false
		}
		g = g.clone()
		f := &g.frames[k]
		f.controls = []rawControl{{keepDisposals[rng.Intn(len(keepDisposals))], first, c.ti}}
		blank := rawFrame{
			controls: []rawControl{{keepDisposals[rng.Intn(len(keepDisposals))], rest, 0}},
			local:    [][3]byte{{byte(rng.Intn(256)), 0, 0}},
		}
		if disposal == clearFrame {
			blank.at, blank.controls[0].disposal = f.at, clearFrame
		} else {
			x, y := rng.Intn(g.w+4), rng.Intn(g.h+4)
			blank.at = image.Rect(x, y, x+rng.Intn(3), y+rng.Intn(3))
		}
		blank.pix = make([]uint8, blank.at.Dx()*blank.at.Dy())
		g.frames = slices.Insert(g.frames, k+1, blank)
		return g, true
	}},
	{"a picture's time moved between its frames", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		if len(g.frames) < 2 {
			return g, false
		}
		k := rng.Intn(len(g.frames) - 1)
		if _, shown := composite(chromiumReads(g)); !bytes.Equal(shown[k].Pix, shown[k+1].Pix) {
			return g, false
		}
		// Each delay written is one a browser shows as written, and the two
		// may say more time between them than one delay can.
		total := shownDelay(g.frames[k].control().delay) + shownDelay(g.frames[k+1].control().delay)
		least, most := max(2, total-maxGIFDelay), min(maxGIFDelay, total-2)
		if least > most {
			return g, false
		}
		first := least + rng.Intn(most-least+1)
		g = g.clone()
		for j, d := range []int{first, total - first} {
			f := &g.frames[k+j]
			if len(f.controls) == 0 {
				f.controls = []rawControl{{ti: -1}}
			}
			f.controls[len(f.controls)-1].delay = d
		}
		return g, true
	}},
	{"a picture cleared or put back and drawn again", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		k := rng.Intn(len(g.frames))
		c := g.frames[k].control()
		first, rest, ok := splitDelay(rng, c.delay)
		at := g.frames[k].at.Intersect(image.Rect(0, 0, g.w, g.h))
		if !ok || browserDisposal(c.disposal) == restoreFrame || at.Empty() {
			return g, false
		}
		_, shown := composite(chromiumReads(g))
		again := rawFrame{controls: []rawControl{{c.disposal, rest, 0}}, at: at, local: [][3]byte{{}}}
		index := map[[3]byte]int{}
		for y := at.Min.Y; y < at.Max.Y; y++ {
			for x := at.Min.X; x < at.Max.X; x++ {
				p := shown[k].NRGBAAt(x, y)
				if p.A == 0 {
					again.pix = append(again.pix, 0)
					continue
				}
				colour := [3]byte{p.R, p.G, p.B}
				if _, ok := index[colour]; !ok {
					index[colour] = len(again.local)
					again.local = append(again.local, colour)
				}
				again.pix = append(again.pix, uint8(index[colour]))
			}
		}
		if len(again.local) > 256 {
			return g, false
		}
		g = g.clone()
		g.frames[k].controls = []rawControl{{[]byte{2, 3, 4}[rng.Intn(3)], first, c.ti}}
		g.frames = slices.Insert(g.frames, k+1, again)
		return g, true
	}},
	{"a frame put back, drawn again for part of its time", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		k := rng.Intn(len(g.frames))
		c := g.frames[k].control()
		first, rest, ok := splitDelay(rng, c.delay)
		if !ok || browserDisposal(c.disposal) != restoreFrame {
			return g, false
		}
		g = g.clone()
		again := g.frames[k]
		again.local, again.pix = slices.Clone(again.local), slices.Clone(again.pix)
		again.controls = []rawControl{{[]byte{3, 4}[rng.Intn(2)], rest, c.ti}}
		g.frames[k].controls = []rawControl{{c.disposal, first, c.ti}}
		g.frames = slices.Insert(g.frames, k+1, again)
		return g, true
	}},
	{"a disposal method the next frame paints over", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		k := rng.Intn(len(g.frames))
		if k == len(g.frames)-1 {
			return g, false
		}
		next := g.frames[k+1]
		if k+1 < len(g.frames)-1 && browserDisposal(next.control().disposal) == restoreFrame {
			return g, false // what it puts back is what the disposal left
		}
		at := g.frames[k].at.Intersect(image.Rect(0, 0, g.w, g.h))
		if !at.In(next.at) {
			return g, false
		}
		ti := next.control().ti
		for y := at.Min.Y; y < at.Max.Y; y++ {
			for x := at.Min.X; x < at.Max.X; x++ {
				if int(next.pix[(y-next.at.Min.Y)*next.at.Dx()+x-next.at.Min.X]) == ti {
					return g, false
				}
			}
		}
		g = g.clone()
		f := &g.frames[k]
		if len(f.controls) == 0 {
			f.controls = []rawControl{{ti: -1}}
		}
		f.controls[len(f.controls)-1].disposal = byte(rng.Intn(8))
		return g, true
	}},
	{"a frame widened with pixels that draw nothing", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		k := rng.Intn(len(g.frames))
		f := g.frames[k]
		c := f.control()
		if c.ti < 0 || c.ti > 255 {
			return g, false
		}
		at := image.Rect(max(0, f.at.Min.X-rng.Intn(3)), max(0, f.at.Min.Y-rng.Intn(3)), f.at.Max.X+rng.Intn(3), f.at.Max.Y+rng.Intn(3))
		if k == 0 {
			at = at.Intersect(image.Rect(0, 0, g.w, g.h)).Union(f.at)
		}
		if at == f.at {
			return g, false
		}
		if browserDisposal(c.disposal) == clearFrame {
			// Clearing the wider frame clears more, which shows unless
			// there is nothing there.
			_, shown := composite(chromiumReads(g))
			for y := at.Min.Y; y < at.Max.Y; y++ {
				for x := at.Min.X; x < at.Max.X; x++ {
					if p := image.Pt(x, y); !p.In(f.at) && p.In(shown[k].Rect) && shown[k].NRGBAAt(x, y).A != 0 {
						return g, false
					}
				}
			}
		}
		g = g.clone()
		pix := make([]uint8, at.Dx()*at.Dy())
		for y := at.Min.Y; y < at.Max.Y; y++ {
			for x := at.Min.X; x < at.Max.X; x++ {
				v := uint8(c.ti)
				if image.Pt(x, y).In(f.at) {
					v = f.pix[(y-f.at.Min.Y)*f.at.Dx()+x-f.at.Min.X]
				}
				pix[(y-at.Min.Y)*at.Dx()+x-at.Min.X] = v
			}
		}
		g.frames[k].at, g.frames[k].pix = at, pix
		return g, true
	}},
	{"fields a browser reads past", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		g = g.clone()
		f := &g.frames[rng.Intn(len(g.frames))]
		switch rng.Intn(7) {
		case 0:
			f.comment = fmt.Sprintf("comment %d", rng.Intn(1000))
		case 1:
			f.plainText = true
		case 2:
			if len(f.controls) == 0 {
				return g, false
			}
			f.controls = append([]rawControl{{byte(rng.Intn(8)), rng.Intn(70000) % 0x10000, rng.Intn(257) - 1}}, f.controls...)
		case 3:
			f.interlace = !f.interlace
		case 4:
			g.bg = byte(rng.Intn(256))
		case 5:
			if len(g.loops) == 0 {
				return g, false
			}
			g.loops = append([]rawLoop{{"NETSCAPE2.0", rng.Intn(5)}}, g.loops...)
			last := &g.loops[len(g.loops)-1]
			last.id = []string{"NETSCAPE2.0", "ANIMEXTS1.0"}[rng.Intn(2)]
		case 6:
			g.noTrailer = !g.noTrailer
		}
		return g, true
	}},
	{"a loop count for a GIF of one picture", func(rng *rand.Rand, g rawGIF) (rawGIF, bool) {
		if len(browserPlay(chromiumReads(g))) != 1 {
			return g, false
		}
		g = g.clone()
		g.loops = nil
		if rng.Intn(3) != 0 {
			g.loops = []rawLoop{{"NETSCAPE2.0", rng.Intn(4)}}
		}
		return g, true
	}},
}

// GIFs a browser shows alike are stored as the same bytes, however their
// frames make what they show. Random GIFs (randomRawGIF) are rewritten at
// random, a few times over, in ways that change none of what Chromium shows
// (gifRewrites): colour tables in another order, with colours repeated, or
// copied between the global table and the frames; pixels that draw the
// colour already under them made transparent, and transparent ones made to
// draw it; a frame's time split with a frame that draws nothing, or with the
// same picture drawn again after a clear or a put-back; a disposal method
// the next frame paints over; a frame widened with pixels that draw nothing;
// fields a browser reads past; and the loop count of a GIF of one picture.
// The model (chromiumReads, composite) has to agree that each rewrite shows
// what the original does, which holds the test to rewrites that change
// nothing; the stored files then have to be the same. Each original is also
// held to what it shows, and to being stored again as the same bytes. A
// quarter of the first 800 draw from many colours, so that their frames'
// colours overflow one table between them; the rest are animations of
// sprites over a background of many colours (randomSpritesGIF), so that
// their stored files have frames put back, which only a GIF of tables of
// their own may have.
func TestGIFsShownAlikeAreStoredAlike(t *testing.T) {
	rng := rand.New(rand.NewSource(20261015))
	applied := map[string]int{}
	stored, refused, own, putBack, putBackForms := 0, 0, 0, 0, 0
	for i := 0; i < 1100; i++ {
		var g rawGIF
		switch {
		case i >= 800:
			g = randomSpritesGIF(rng)
		case i%4 == 3:
			g = randomRawGIF(rng, 600)
		default:
			g = randomRawGIF(rng, 6)
		}
		name := fmt.Sprintf("GIF %d", i)
		want, wantErr := Process(context.Background(), g.bytes())
		switch {
		case errors.Is(wantErr, ErrImageTooCostly):
			refused++
		case wantErr != nil:
			t.Fatalf("%s: %v", name, wantErr)
		default:
			stored++
			requireShowsAs(t, name, g, want.Data)
			if again := process(t, want.Data).Data; !bytes.Equal(again, want.Data) {
				t.Fatalf("%s: processing the stored GIF again stores it differently", name)
			}
			_, local := gifTables(t, want.Data)
			if slices.Contains(local, true) {
				own++
			}
			if bytes.Contains(gifDisposals(t, want.Data), []byte{restoreFrame}) {
				putBack++
				if slices.Contains(local, false) {
					t.Fatalf("%s: stored with a frame put back and a frame of the global table", name)
				}
			}
		}
		for form := 0; form < 6; form++ {
			rewritten, done := g, []string(nil)
			for n := 1 + rng.Intn(4); n > 0; n-- {
				r := gifRewrites[rng.Intn(len(gifRewrites))]
				if next, ok := r.rewrite(rng, rewritten); ok {
					rewritten, done = next, append(done, r.name)
				}
			}
			if len(done) == 0 {
				continue
			}
			if d := showDifference(chromiumReads(g), chromiumReads(rewritten)); d != "" {
				t.Fatalf("%s rewritten (%s) does not show what it did: %s", name, strings.Join(done, "; "), d)
			}
			for _, r := range done {
				applied[r]++
			}
			got, err := Process(context.Background(), rewritten.bytes())
			if !errors.Is(err, wantErr) || err == nil && !bytes.Equal(got.Data, want.Data) {
				t.Fatalf("%s rewritten (%s) is stored differently (%v, against %v for the original)", name, strings.Join(done, "; "), err, wantErr)
			}
			if err == nil && bytes.Contains(gifDisposals(t, got.Data), []byte{restoreFrame}) {
				putBackForms++
			}
		}
	}
	t.Logf("%d GIFs stored (%d with tables of their own, %d with a frame put back, rewritten %d times), %d refused; rewrites applied: %v", stored, own, putBack, putBackForms, refused, applied)
	for _, r := range gifRewrites {
		if applied[r.name] < 50 {
			t.Errorf("%q was applied %d times; the test shows little of it", r.name, applied[r.name])
		}
	}
	if own < 300 || putBack < 50 || putBackForms < 200 || stored < 900 {
		t.Errorf("%d GIFs stored, %d with tables of their own and %d with a frame put back, rewritten %d times; the test shows little", stored, own, putBack, putBackForms)
	}
}

// gifDisposals is the disposal method of each frame of a GIF, as written.
func gifDisposals(t *testing.T, data []byte) []byte {
	t.Helper()
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the stored GIF does not decode: %v", err)
	}
	return g.Disposal
}

/* spans walks only the rows the rectangles cover --------------------------- */

// spansBoundingBox is spans as it was before it walked only covered rows: it
// iterates every row of the bounding box of the three rectangles, so two
// one-pixel rectangles at the top and the bottom of a tall canvas made it
// iterate every row of the canvas though only two held a run. It is kept here
// as the reference spans must still match call for call, and as what the
// row-count test is measured against.
func spansBoundingBox(a, b, c image.Rectangle, f func(y, x0, x1 int)) {
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

// TestSpansVisitsEachPixelOnce holds spans to spansBoundingBox call for call,
// over random rectangles overlapping, apart, nested and empty, so that
// walking only covered rows did not change which runs it reports.
func TestSpansVisitsEachPixelOnce(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	rect := func() image.Rectangle {
		if rng.Intn(5) == 0 {
			return image.Rectangle{}
		}
		x, y := rng.Intn(40), rng.Intn(40)
		return image.Rect(x, y, x+rng.Intn(12), y+rng.Intn(12))
	}
	for i := 0; i < 100000; i++ {
		a, b, c := rect(), rect(), rect()
		var want, got []string
		spansBoundingBox(a, b, c, func(y, x0, x1 int) { want = append(want, fmt.Sprint(y, x0, x1)) })
		spans(a, b, c, func(y, x0, x1 int) { got = append(got, fmt.Sprint(y, x0, x1)) })
		if fmt.Sprint(want) != fmt.Sprint(got) {
			t.Fatalf("%v %v %v:\n want %v\n got  %v", a, b, c, want, got)
		}
	}
}

// walkedRows is how many rows spans iterates for the rectangles: the rows
// coveredRows reports, which is its whole outer loop. It is a structural
// count, not a time, so it holds on CI whatever the load.
func walkedRows(a, b, c image.Rectangle) int {
	rects := [3]image.Rectangle{a, b, c}
	ys, m := coveredRows(&rects)
	rows := 0
	for _, iv := range ys[:m] {
		rows += iv[1] - iv[0]
	}
	return rows
}

// TestSpansWalksOnlyCoveredRows pins that spans iterates only the rows a
// rectangle covers, not every row between them. The CPU finding was a GIF of
// a one-pixel frame at the top and another at the bottom of a 1×65535 canvas:
// spans walked all 65,535 rows per frame though only two held a pixel, and
// nothing charged the gap (the walk charges the frames' pixels, playGIF's
// area the stored frames'). Before the fix walkedRows returned the bounding
// box's height; it now returns the covered rows. The count is structural, so
// a revert to the bounding-box walk (spansBoundingBox) fails it without any
// timing.
func TestSpansWalksOnlyCoveredRows(t *testing.T) {
	top := image.Rect(0, 0, 1, 1)
	bottom := image.Rect(0, 65534, 1, 65535)
	if got := walkedRows(top, bottom, image.Rectangle{}); got != 2 {
		t.Errorf("spans walks %d rows for two one-pixel rectangles 65,534 apart, want 2 (the bounding box is 65,535 rows)", got)
	}

	// Over random rectangles, spans never walks more rows than the rectangles
	// cover between them, which is at most their heights added up — never the
	// gaps between them, which nothing charges.
	rng := rand.New(rand.NewSource(2))
	rect := func() image.Rectangle {
		if rng.Intn(5) == 0 {
			return image.Rectangle{}
		}
		x, y := rng.Intn(1000), rng.Intn(100000)
		return image.Rect(x, y, x+rng.Intn(50), y+rng.Intn(50))
	}
	for i := 0; i < 20000; i++ {
		a, b, c := rect(), rect(), rect()
		covered := map[int]bool{}
		for _, r := range []image.Rectangle{a, b, c} {
			if r.Empty() {
				continue
			}
			for y := r.Min.Y; y < r.Max.Y; y++ {
				covered[y] = true
			}
		}
		if got, want := walkedRows(a, b, c), len(covered); got != want {
			t.Fatalf("%v %v %v: spans walks %d rows, want %d (the rows a rectangle covers)", a, b, c, got, want)
		}
	}
}
