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
// written in the colour it already shows, which the frame draws elsewhere. A
// GIF that would need a frame of more than 256 colours, or of 256 and a
// pixel that cannot be written so, cannot be stored one frame to a picture,
// and is refused (ErrImageTooCostly).
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
	// of its own holding want[k], after a transparent entry if clear[k].
	stores := func(name string, g rawGIF, want [][][3]byte, clear []bool) {
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

	// What a frame put back and what the next draws, between them more
	// colours than one table holds; and 256 of them around a pixel that
	// shows nothing.
	for name, g := range map[string]rawGIF{
		"more than 256 colours in a frame": {w: 300, h: 1, frames: []rawFrame{
			frame(image.Rect(0, 0, 150, 1), keepFrame, -1, shades(0, 150), indices(150)),
			frame(image.Rect(0, 0, 150, 1), restoreFrame, -1, shades(1000, 150), indices(150)),
			frame(image.Rect(150, 0, 300, 1), keepFrame, -1, shades(2000, 150), indices(150)),
		}},
		"256 colours in a frame and a pixel it cannot write": {w: 257, h: 1, frames: []rawFrame{
			frame(image.Rect(0, 0, 128, 1), keepFrame, -1, shades(0, 128), indices(128)),
			frame(image.Rect(0, 0, 128, 1), restoreFrame, -1, a, make([]uint8, 128)),
			frame(image.Rect(129, 0, 257, 1), keepFrame, -1, shades(1000, 128), indices(128)),
		}},
	} {
		if _, err := Process(context.Background(), g.bytes()); !errors.Is(err, ErrImageTooCostly) {
			t.Errorf("%s: Process returned %v, want ErrImageTooCostly", name, err)
		}
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
// written.
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
