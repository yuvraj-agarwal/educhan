package app

import (
	"math/rand"
	"reflect"
	"regexp/syntax"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// The tests here hold the filter's searches (filtersearch.go) to what Go's
// regexp finds with the same patterns, since a search is only a faster way
// of finding exactly that, and the filter's decisions are made on it.

func TestFilterSearchesAreAllPlanned(t *testing.T) {
	prepareSearches()
	if len(searches.all) < 50 {
		t.Fatalf("only %d searches recorded", len(searches.all))
	}
	for _, s := range searches.all {
		if s.branches == nil {
			t.Errorf("not planned, so run whole by Go's regexp: %.100q", s.String())
		}
	}
}

// searchSampler writes text a pattern was written to match, chosen at random
// from what it allows.
type searchSampler struct {
	rnd *rand.Rand
	b   strings.Builder
}

func (g *searchSampler) sample(re *syntax.Regexp) {
	switch re.Op {
	case syntax.OpLiteral:
		for _, c := range re.Rune {
			if re.Flags&syntax.FoldCase != 0 && g.rnd.Intn(4) == 0 {
				c = unicode.SimpleFold(c)
			}
			g.b.WriteRune(c)
		}
	case syntax.OpCharClass:
		g.b.WriteRune(g.classRune(re.Rune))
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		g.b.WriteByte("ab .-1"[g.rnd.Intn(6)])
	case syntax.OpCapture:
		g.sample(re.Sub[0])
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat:
		lo, hi := 0, -1
		switch re.Op {
		case syntax.OpPlus:
			lo = 1
		case syntax.OpQuest:
			hi = 1
		case syntax.OpRepeat:
			lo, hi = re.Min, re.Max
		}
		n := lo + g.rnd.Intn(3)
		if hi >= 0 {
			n = min(n, hi)
		}
		for range n {
			g.sample(re.Sub[0])
		}
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			g.sample(sub)
		}
	case syntax.OpAlternate:
		g.sample(re.Sub[g.rnd.Intn(len(re.Sub))])
	}
	// Assertions and empty matches write nothing.
}

// classRune picks a character of a class, mostly from its ASCII part, as
// posts are mostly written.
func (g *searchSampler) classRune(ranges []rune) rune {
	n := len(ranges) / 2
	i := g.rnd.Intn(n)
	if ranges[0] < utf8.RuneSelf && g.rnd.Intn(4) != 0 {
		i = 0
		for i+1 < n && ranges[2*i+2] < utf8.RuneSelf && g.rnd.Intn(2) == 0 {
			i++
		}
	}
	lo, hi := ranges[2*i], ranges[2*i+1]
	if lo < utf8.RuneSelf {
		hi = min(hi, utf8.RuneSelf-1)
	}
	r := lo + rune(g.rnd.Int63n(int64(hi-lo)+1))
	if !utf8.ValidRune(r) {
		r = lo
	}
	return r
}

// filterSearchTexts is text made from every search's own pattern: for each,
// strings it was written to match, drawn with a fixed seed from what it
// allows, set among words and punctuation and strung together, a few
// searches to a text, and a window search's also alone.
func filterSearchTexts(t testing.TB, perSearch int) []string {
	prepareSearches()
	rnd := rand.New(rand.NewSource(20261015))
	fillers := []string{"", " ", ". ", "\n", ", ", "ok ", "I ", "x", "1", "@", "-", " the ", "!! "}
	var samples, alone []string
	for _, s := range searches.all {
		re, err := syntax.Parse(s.src, syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		for range perSearch {
			g := &searchSampler{rnd: rnd}
			g.sample(re)
			samples = append(samples, g.b.String())
			// A window search is read on a few words cut out of a view, and
			// some are anchored to where those end, so its samples are read
			// alone as well.
			if s.family >= numFamilies {
				alone = append(alone, g.b.String())
			}
		}
	}
	texts := alone
	for i := 0; i < len(samples); i += 3 {
		var b strings.Builder
		for _, s := range samples[i:min(len(samples), i+4)] {
			b.WriteString(fillers[rnd.Intn(len(fillers))])
			b.WriteString(s)
		}
		b.WriteString(fillers[rnd.Intn(len(fillers))])
		texts = append(texts, b.String())
	}
	return texts
}

// searchWindows are stretches of a text of the lengths the filter reads
// round a match.
func searchWindows(text string) []string {
	out := []string{text}
	for i := 0; i < len(text); i += 29 {
		for _, n := range []int{24, 64, 140} {
			out = append(out, text[i:min(len(text), i+n)])
		}
	}
	return out
}

// compareSearches runs every search over both views of text and reports
// where it finds anything other than its regexp does. It returns how many
// matches each search found.
func compareSearches(t testing.TB, text string, found map[*search]int) {
	f := scanField(fieldBody, text, markupReadings(text))
	for _, v := range []*view{&f.plain, &f.loose} {
		for _, s := range searches.all {
			if s.family >= numFamilies {
				for _, w := range searchWindows(v.text) {
					want := s.regexp().FindStringSubmatchIndex(w)
					if got := s.findIn(v, w); !reflect.DeepEqual(got, want) {
						t.Errorf("window search %.60q in %q: got %v, want %v", s.String(), w, got, want)
					}
					if want != nil {
						found[s]++
					}
				}
				continue
			}
			want := s.regexp().FindAllStringSubmatchIndex(v.text, -1)
			var got [][]int
			s.each(v, func(m []int) bool {
				got = append(got, m)
				return true
			})
			if len(got) != len(want) || len(want) > 0 && !reflect.DeepEqual(got, want) {
				t.Errorf("search %.60q (%s) in %q:\n got  %v\n want %v", s.String(), s.planString(), v.text, got, want)
			}
			found[s] += len(want)
		}
	}
}

func TestFilterSearchesMatchTheirRegexps(t *testing.T) {
	found := map[*search]int{}
	for _, text := range filterSearchTexts(t, 4) {
		compareSearches(t, text, found)
		if t.Failed() {
			return
		}
	}
	// Each search found something somewhere, so none was compared only
	// where it finds nothing.
	for _, s := range searches.all {
		if found[s] == 0 {
			t.Errorf("search %.80q matched none of its own samples", s.String())
		}
	}
}

func FuzzFilterSearches(f *testing.F) {
	for _, text := range filterSearchTexts(f, 1) {
		f.Add(text)
	}
	for _, text := range []string{
		"", "a", "@", "i will kill you at school tomorrow", "my snap is ab_cd", "how many pills to die",
		"\u33c2@\u33d8", "u'll die if u come to school", "attherateofa.a", "ab at cd dot com",
	} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 2000 {
			return
		}
		compareSearches(t, text, map[*search]int{})
	})
}
