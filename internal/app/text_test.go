package app

import (
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestNormalizeBody(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "trims surrounding whitespace", in: "  hello  ", want: "hello"},
		{name: "normalises CRLF", in: "a\r\nb", want: "a\nb"},
		{name: "normalises lone CR", in: "a\rb", want: "a\nb"},
		{name: "trims trailing spaces per line", in: "a   \nb\t", want: "a\nb"},
		{name: "collapses blank line runs", in: "a\n\n\n\n\nb", want: "a\n\nb"},
		{name: "keeps a single blank line", in: "a\n\nb", want: "a\n\nb"},
		{name: "drops control characters", in: "a\x00\x07b", want: "ab"},
		{name: "keeps tabs", in: "a\tb", want: "a\tb"},
		{name: "whitespace only becomes empty", in: " \n\t\n ", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeBody(tc.in); got != tc.want {
				t.Errorf("normalizeBody(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtractQuotes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []int64
	}{
		{name: "no quotes", in: "just text", want: nil},
		{name: "single quote", in: ">>12 sure", want: []int64{12}},
		{name: "keeps first-seen order and dedupes", in: ">>9\n>>3\n>>9", want: []int64{9, 3}},
		{name: "ignores single angle bracket", in: ">12", want: nil},
		{name: "quote inside a sentence", in: "as >>4 said", want: []int64{4}},
		{name: "ignores a quote inside a code block", in: "[code]y = x >>2;[/code]", want: nil},
		{name: "code block spanning lines", in: "[code]\nint a = b>>3;\n>>4\n[/code]", want: nil},
		{name: "keeps quotes around a code block", in: ">>3 [code]>>4[/code] >>5", want: []int64{3, 5}},
		{name: "the first close ends the block", in: "[code]a[/code]>>6[/code]", want: []int64{6}},
		{name: "an open tag inside code is code", in: "[code][code]>>1[/code]>>2", want: []int64{2}},
		{name: "unclosed code is text", in: "[code] >>6", want: []int64{6}},
		{name: "empty code pair is text", in: "[code][/code]>>7", want: []int64{7}},
		{name: "code inside a spoiler is still code", in: "[spoiler][code]>>8[/code][/spoiler]", want: nil},
		{name: "a spoiler's quotes count", in: "[spoiler]>>9[/spoiler]", want: []int64{9}},
		{name: "a removed block does not join its neighbours", in: ">>[code]x[/code]12", want: nil},
		{name: "cross-board links are not quotes", in: ">>>/math/12 >>>/u/iitb/13", want: nil},
		{name: "ignores a shift inside inline maths", in: "[math]a >>2[/math]", want: nil},
		{name: "ignores a quote inside display maths", in: "[eqn]\n>>3\n[/eqn] >>4", want: []int64{4}},
		{name: "unclosed maths is text", in: "[math]>>5 [/eqn]", want: []int64{5}},
		{name: "maths inside a spoiler is still maths", in: "[spoiler][math]>>6[/math] >>7[/spoiler]", want: []int64{7}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractQuotes(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("extractQuotes(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtractQuotesCap(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= maxQuotes+15; i++ {
		sb.WriteString(">>")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString("\n")
	}
	if got := extractQuotes(sb.String()); len(got) != maxQuotes {
		t.Errorf("extracted %d quotes, want the cap of %d", len(got), maxQuotes)
	}
}

// TestMarkupBlocks pins the pairing rules, which parseMarkup in web/app.js
// follows too: the server and the page have to agree on what is code or maths.
func TestMarkupBlocks(t *testing.T) {
	type block struct {
		kind  blockKind
		inner string
	}
	const (
		spoiler = blockSpoiler
		code    = blockCode
		math    = blockMath
		eqn     = blockEqn
	)
	tags := map[blockKind][2]string{spoiler: {spoilerOpenTag, spoilerCloseTag}}
	for _, t := range opaqueTags {
		tags[t.kind] = [2]string{t.open, t.close}
	}
	tests := []struct {
		name string
		in   string
		want []block
	}{
		{name: "none", in: "plain [b] text", want: nil},
		{name: "code", in: "a [code]x[/code] b", want: []block{{code, "x"}}},
		{name: "spoiler", in: "a [spoiler]x[/spoiler] b", want: []block{{spoiler, "x"}}},
		{name: "spoiler across lines", in: "[spoiler]a\nb[/spoiler]", want: []block{{spoiler, "a\nb"}}},
		{name: "code inside a spoiler", in: "[spoiler]a[code]x[/code][/spoiler]",
			want: []block{{spoiler, "a[code]x[/code]"}, {code, "x"}}},
		{name: "tags inside code are code", in: "[code][spoiler]x[/spoiler][/code]", want: []block{{code, "[spoiler]x[/spoiler]"}}},
		{name: "unclosed code", in: "[code]x", want: nil},
		{name: "unclosed spoiler", in: "[spoiler]x", want: nil},
		{name: "stray closes", in: "x[/code][/spoiler][/math][/eqn]", want: nil},
		{name: "empty pairs", in: "[code][/code][spoiler][/spoiler][math][/math][eqn][/eqn]", want: nil},
		{name: "nested spoiler is text inside the first", in: "[spoiler]a[spoiler]b[/spoiler]c[/spoiler]",
			want: []block{{spoiler, "a[spoiler]b"}}},
		{name: "a close inside code does not close a spoiler", in: "[spoiler]a[code][/spoiler][/code]",
			want: []block{{code, "[/spoiler]"}}},
		{name: "a spoiler cannot close inside code", in: "[spoiler]a[code][/spoiler][/code]b[/spoiler]",
			want: []block{{spoiler, "a[code][/spoiler][/code]b"}, {code, "[/spoiler]"}}},
		{name: "unclosed code inside a spoiler", in: "[spoiler]a[code]b[/spoiler]", want: []block{{spoiler, "a[code]b"}}},
		{name: "two in a row", in: "[spoiler]a[/spoiler][spoiler]b[/spoiler]", want: []block{{spoiler, "a"}, {spoiler, "b"}}},

		{name: "inline maths", in: "so [math]x^2[/math] grows", want: []block{{math, "x^2"}}},
		{name: "display maths", in: "[eqn]\n\\int_0^1 f\n[/eqn]", want: []block{{eqn, "\n\\int_0^1 f\n"}}},
		{name: "maths closes on its own tag only", in: "[math]a[/eqn]b[/code]c[/math]", want: []block{{math, "a[/eqn]b[/code]c"}}},
		{name: "unclosed maths", in: "[math]x [eqn]y", want: nil},
		{name: "tags inside maths are TeX", in: "[math][spoiler]x[/spoiler][code]y[/code][/math]",
			want: []block{{math, "[spoiler]x[/spoiler][code]y[/code]"}}},
		{name: "maths inside code is code", in: "[code][math]x[/math][/code]", want: []block{{code, "[math]x[/math]"}}},
		{name: "code inside maths is TeX", in: "[eqn][code]x[/code][/eqn]", want: []block{{eqn, "[code]x[/code]"}}},
		{name: "maths inside a spoiler", in: "[spoiler]so [eqn]x[/eqn][/spoiler]",
			want: []block{{spoiler, "so [eqn]x[/eqn]"}, {eqn, "x"}}},
		{name: "a spoiler cannot close inside maths", in: "[spoiler]a[math][/spoiler][/math]b[/spoiler]",
			want: []block{{spoiler, "a[math][/spoiler][/math]b"}, {math, "[/spoiler]"}}},
		{name: "the first opening claims", in: "[math]a[eqn]b[/math]c[/eqn]", want: []block{{math, "a[eqn]b"}}},
		{name: "one after another", in: "[math]a[/math][eqn]b[/eqn][code]c[/code]",
			want: []block{{math, "a"}, {eqn, "b"}, {code, "c"}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []block
			for _, blk := range markupBlocks(tc.in) {
				got = append(got, block{blk.kind, tc.in[blk.inner[0]:blk.inner[1]]})
				want := tags[blk.kind]
				if tag := tc.in[blk.start:blk.inner[0]]; tag != want[0] {
					t.Errorf("block opens with %q, want %q", tag, want[0])
				}
				if tag := tc.in[blk.inner[1]:blk.end]; tag != want[1] {
					t.Errorf("block closes with %q, want %q", tag, want[1])
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("markupBlocks(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestPlainText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "hello >>12", want: "hello >>12"},
		{name: "spoiler hidden", in: "the ending: [spoiler]they all die[/spoiler]!", want: "the ending: [spoiler]!"},
		{name: "code unwrapped", in: "try [code]x := 1[/code] instead", want: "try x := 1 instead"},
		{name: "code inside a spoiler stays hidden", in: "[spoiler]answer: [code]42[/code][/spoiler]", want: "[spoiler]"},
		{name: "unpaired tags are text", in: "[spoiler]x [code]y", want: "[spoiler]x [code]y"},
		{name: "maths unwrapped to its TeX", in: "so [math]x^2[/math] and [eqn]E = mc^2[/eqn]", want: "so x^2 and E = mc^2"},
		{name: "maths inside a spoiler stays hidden", in: "[spoiler][math]42[/math][/spoiler]", want: "[spoiler]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := plainText(tc.in); got != tc.want {
				t.Errorf("plainText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExcerptHidesSpoilers(t *testing.T) {
	got := excerpt("[spoiler]"+strings.Repeat("secret ", 60)+"[/spoiler] after", excerptLen)
	if strings.Contains(got, "secret") {
		t.Errorf("excerpt %q shows a spoiler's text", got)
	}
	if got != "[spoiler] after" {
		t.Errorf("excerpt = %q, want %q", got, "[spoiler] after")
	}
}

func TestExcerpt(t *testing.T) {
	if got := excerpt("short", 10); got != "short" {
		t.Errorf("excerpt = %q, want %q", got, "short")
	}
	long := strings.Repeat("é", 30)
	got := excerpt(long, 10)
	if !strings.HasSuffix(got, "\u2026") {
		t.Errorf("excerpt %q should end with an ellipsis", got)
	}
	if runes := len([]rune(got)); runes != 11 {
		t.Errorf("excerpt has %d runes, want 11 (10 + ellipsis)", runes)
	}
}

func TestSettledStart(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "no tags at all", want: "no tags at all"},
		{name: "closed blocks stay", in: "a [spoiler]b[/spoiler] [code]c[/code] d", want: "a [spoiler]b[/spoiler] [code]c[/code] d"},
		{name: "a spoiler closed further on", in: "a [spoiler]secret", want: "a "},
		{name: "code closed further on", in: "a [code]x := 1", want: "a "},
		{name: "maths closed further on", in: "a [math]x^2 [eqn]y", want: "a "},
		{name: "a tag cut in half", in: "a [spoi", want: "a "},
		{name: "a close cut in half", in: "a [spoiler]b[/spoiler] c [/spo", want: "a [spoiler]b[/spoiler] c "},
		{name: "a bracket that is no tag", in: "a [b] [1, 2]", want: "a [b] [1, 2]"},
		{name: "an empty pair is text", in: "a [spoiler][/spoiler] b", want: "a [spoiler][/spoiler] b"},
		{name: "a spoiler inside a spoiler is text", in: "[spoiler]a [spoiler] b[/spoiler] c", want: "[spoiler]a [spoiler] b[/spoiler] c"},
		{name: "tags inside closed code are code", in: "[code][spoiler]x[/code] y", want: "[code][spoiler]x[/code] y"},
		// The rest of the body could close the [code], which would take the
		// [/spoiler] with it and leave the spoiler to close later; so the
		// [code] goes, and then the spoiler it unpaired.
		{name: "a spoiler around code closed further on", in: "a [spoiler]b [code] c [/spoiler] d", want: "a "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := settledStart(tc.in); got != tc.want {
				t.Errorf("settledStart(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A settled start pairs exactly as the whole body does: every block the body
// opens within it closes within it, and nothing else pairs there. Checked over
// tag-heavy bodies cut everywhere, since that is where the rules can disagree.
func TestSettledStartPairsAsTheWholeBody(t *testing.T) {
	pieces := []string{
		"[spoiler]", "[/spoiler]", "[code]", "[/code]", "[math]", "[/math]", "[eqn]", "[/eqn]",
		"x", "y ", "[", "]", "[spo", "[/co",
	}
	rng := rand.New(rand.NewSource(1))
	for n := 0; n < 3000; n++ {
		var b strings.Builder
		for range 4 + rng.Intn(10) {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		body := b.String()
		whole := markupBlocks(body)
		for cut := 0; cut <= len(body); cut++ {
			start, cuts := settle(body[:cut])
			if cuts > 3 {
				t.Fatalf("body %q cut at %d took %d cuts to settle, want at most 3", body, cut, cuts)
			}
			var want []markupBlock
			for _, blk := range whole {
				if blk.start < len(start) {
					want = append(want, blk)
				}
			}
			got := markupBlocks(start)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("body %q cut at %d settles to %q, which pairs as %+v; the whole body pairs there as %+v",
					body, cut, start, got, want)
			}
		}
	}
}

// A start the archive reads settles in a few passes however it is written.
// Runs of what look like tags cut in half used to settle one character per
// pass, rescanning the whole start each time: quadratic in the window, and so
// milliseconds of CPU per such thread on every anonymous archive listing.
func TestSettlingTakesAFewPassesWhateverTheStart(t *testing.T) {
	for _, run := range []string{
		"[", "[s", "[sp", "[spoiler", "[/spoiler", "[math", "[/c", "[e",
		"[spoiler]", "[code]", "[spoiler][code]", "[spoiler]x[/spoiler][code]", "[[spoiler]",
	} {
		start := strings.Repeat(run, archiveBodyWindow/len(run)+1)[:archiveBodyWindow]
		if _, cuts := settle(start); cuts > 3 {
			t.Errorf("a run of %q took %d cuts to settle, want at most 3", run, cuts)
		}
	}
	// What precedes the last "[" was followed by another "[" in the body, so
	// it is text, and the excerpt shows it.
	if got, want := excerptOfStart(strings.Repeat("[", archiveBodyWindow), true, archiveExcerptLen),
		strings.Repeat("[", archiveExcerptLen)+"\u2026"; got != want {
		t.Errorf("a run of brackets excerpts to %q, want %q", got, want)
	}
}

func TestExcerptOfStart(t *testing.T) {
	if got, want := excerptOfStart("whole [spoiler]body[/spoiler]", false, 120), "whole [spoiler]"; got != want {
		t.Errorf("uncut: %q, want %q", got, want)
	}
	if got, want := excerptOfStart("the twist: [spoiler]the narrator", true, 120), "the twist:\u2026"; got != want {
		t.Errorf("a cut spoiler: %q, want %q", got, want)
	}
	if got, want := excerptOfStart(strings.Repeat("é", 30), true, 10), strings.Repeat("é", 10)+"\u2026"; got != want {
		t.Errorf("a long cut start: %q, want %q", got, want)
	}
}

func TestCountLines(t *testing.T) {
	if got := countLines("a"); got != 1 {
		t.Errorf("countLines = %d, want 1", got)
	}
	if got := countLines("a\nb\nc"); got != 3 {
		t.Errorf("countLines = %d, want 3", got)
	}
}
