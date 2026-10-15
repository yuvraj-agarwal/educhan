package media

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"math/rand"
	"strings"
	"testing"
)

// What a GIF shows is the frames drawn one on another, each for its delay,
// and that is all a stored GIF keeps of its frames: a frame that changes
// nothing on screen is folded into the time of the one before it, and the
// fields browsers read past are written the same whatever the upload said.
// These tests hold the stored file to what Chromium and Firefox show of the
// upload, through a model of their compositing written here independently of
// playGIF (browserPlay).

/* A browser, as far as a GIF goes ------------------------------------------ */

// onScreen is a picture a browser shows while a GIF plays, and for how long.
type onScreen struct {
	pic *image.NRGBA
	ms  int
}

// browserPlay plays a decoded GIF as Chromium and Firefox do: on a canvas that
// starts transparent, each frame's pixels other than its transparent ones are
// drawn, the canvas is shown for the frame's delay (one of 10 ms or less shown
// for 100 ms), and the frame is then disposed of by its method — 2 clears its
// rectangle to transparent, 3 and 4 put back what was there before it, and
// anything else leaves it. Consecutive frames that show the same picture are
// one picture shown for their time together.
func browserPlay(g *gif.GIF) []onScreen {
	canvas := image.NewNRGBA(image.Rect(0, 0, g.Config.Width, g.Config.Height))
	var shows []onScreen
	for k, m := range g.Image {
		var disposal byte
		if g.Disposal != nil {
			disposal = g.Disposal[k]
		}
		var saved []byte
		if disposal == 3 || disposal == 4 {
			saved = append([]byte(nil), canvas.Pix...)
		}
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
		ms := 10 * g.Delay[k]
		if ms <= 10 {
			ms = 100
		}
		if n := len(shows); n > 0 && bytes.Equal(shows[n-1].pic.Pix, canvas.Pix) {
			shows[n-1].ms += ms
		} else {
			pic := image.NewNRGBA(canvas.Rect)
			copy(pic.Pix, canvas.Pix)
			shows = append(shows, onScreen{pic: pic, ms: ms})
		}
		switch disposal {
		case 2:
			for y := r.Min.Y; y < r.Max.Y; y++ {
				clear(canvas.Pix[canvas.PixOffset(r.Min.X, y):canvas.PixOffset(r.Max.X, y)])
			}
		case 3, 4:
			copy(canvas.Pix, saved)
		}
	}
	return shows
}

// requireSamePlay checks that a stored GIF shows what its upload showed: the
// same pictures, in the same order, each for the same time, and played as
// many times. The time the last picture of an animation played once is shown
// for is not compared, because it stays on screen once the animation ends;
// nor is anything about the timing of a GIF that only ever shows one picture.
func requireSamePlay(t *testing.T, name string, upload, stored []byte) {
	t.Helper()
	up, err := gif.DecodeAll(bytes.NewReader(upload))
	if err != nil {
		t.Fatalf("%s: the upload does not decode: %v", name, err)
	}
	st, err := gif.DecodeAll(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("%s: the stored GIF does not decode: %v", name, err)
	}
	if st.Config.Width != up.Config.Width || st.Config.Height != up.Config.Height {
		t.Fatalf("%s: stored on a %dx%d canvas, want %dx%d", name, st.Config.Width, st.Config.Height, up.Config.Width, up.Config.Height)
	}
	want, got := browserPlay(up), browserPlay(st)
	if len(got) != len(want) {
		t.Fatalf("%s: the stored GIF shows %d pictures in turn, the upload %d", name, len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i].pic.Pix, want[i].pic.Pix) {
			t.Fatalf("%s: picture %d differs from the upload's", name, i)
		}
		if len(want) > 1 && (i < len(want)-1 || up.LoopCount != -1) && got[i].ms != want[i].ms {
			t.Errorf("%s: picture %d shown for %d ms, want %d", name, i, got[i].ms, want[i].ms)
		}
	}
	if len(want) > 1 && st.LoopCount != up.LoopCount {
		t.Errorf("%s: stored loop count %d, want %d", name, st.LoopCount, up.LoopCount)
	}
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

// A frame that changes nothing on screen is folded into the one before it, so
// what a browser shows is the same picture for the same time; but where the
// one before it disposes of itself, what shows during it is what that left,
// and it is kept as one transparent pixel at the corner, which shows that for
// its time. What a frame draws over the canvas is cut to the part it draws on,
// and a frame that clears its rectangle afterwards to the part that clearing
// changes. Each case is held to what a browser shows of the upload.
func TestGIFFramesThatChangeNothingAreFolded(t *testing.T) {
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
			name: "a frame that draws nothing and clears what is under it",
			loop: 0,
			frames: []playFrame{
				red(rect(0, 0, 4, 4), gif.DisposalNone, 10),
				empty(rect(2, 2, 6, 6), gif.DisposalBackground, 5),
				green,
			},
			want: []stored{
				{rect(0, 0, 4, 4), 10, gif.DisposalNone, false},
				{rect(2, 2, 4, 4), 5, gif.DisposalBackground, true},
				{rect(7, 7, 8, 8), 30, gif.DisposalNone, false},
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
			name: "a frame that draws nothing after one that clears itself",
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
				{rect(0, 0, 2, 2), 10, gif.DisposalPrevious, false},
				{rect(0, 0, 1, 1), 20, gif.DisposalNone, true},
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
				{rect(0, 0, 2, 2), 10, gif.DisposalPrevious, false},
				{rect(2, 2, 4, 4), 10, gif.DisposalNone, false},
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

// Whatever its frames, a GIF is stored as it plays, and as nothing more: each
// stored frame changes what is on screen or disposes of what one did, and
// processing a stored GIF again stores the same bytes, so nothing a frame
// carries that does not show was left behind to be folded the second time.
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
			case len(g.Image) > 1 && (d < gif.DisposalNone || d > gif.DisposalPrevious || last && d != gif.DisposalNone):
				t.Errorf("%s: frame %d of %d stored with disposal %d", name, k, len(g.Image), d)
			case len(g.Image) > 1 && g.Delay[k] < 2 && !(last && g.LoopCount == -1 && g.Delay[k] == 0):
				t.Errorf("%s: frame %d stored with delay %d, which browsers do not show as written", name, k, g.Delay[k])
			}
		}
	}
}
