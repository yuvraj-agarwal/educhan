package app

import (
	"cmp"
	"math"
	"regexp"
	"regexp/syntax"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// How the filter runs its patterns.
//
// A rule's pattern is one Go regexp, and some of them are very large: the
// threat and contact patterns are alternations of hundreds of phrasings. Go's
// regexp has no DFA, so an unanchored search pays at every character for every
// way a match could start there, and a post as long as a post can be, made of
// characters that each read as four, cost hundreds of milliseconds of one core.
//
// So the filter never runs a whole pattern over a whole view. Each pattern is
// split, once, into branches: the alternatives of its first alternation, each
// with the rest of the pattern after it, which together match exactly what the
// pattern matches, in the same order of preference. Each branch is compiled
// as Go's regexp compiles a pattern. For every branch the planner works out
// what any match must start with: the words it opens with, read as literally
// as the pattern allows, with a run of separators or spaces between them read
// as "skip what follows of these", and a bounded run of them as a gap tried
// at every length (one gap to an opening). All of those openings, for every
// pattern a view is searched with, go into one trie, and one walk of the view
// (searchIndex.walk) finds every place each branch could start.
//
// A branch is then run only from those places, in the order the whole
// pattern would have found them, so what is found is what the whole
// pattern's FindAllStringSubmatchIndex finds: the leftmost match, the
// alternative the pattern prefers there, then the next match after it. It
// is run by a copy of Go's own regexp machine (regexp/exec.go) that starts
// threads only at those places: a finder, which keeps only where each
// thread started, finds where the branch's first match starts, and the
// machine, from there alone, finds the match again with its groups. Both
// look one character ahead, as Go's machine does not: a thread is not put
// at an instruction from which nothing can take the next character or
// match (viability), since it could only die there.
// TestFilterSearchesMatchTheirRegexps holds every pattern to that on every
// view of the filter's test inputs, and FuzzFilterSearches on any text.
//
// Two shapes of branch start with something too common to look for, and are
// found from what comes after it instead:
//
//   - delimited: a sentence boundary ("^" or one of a few punctuation
//     characters), any run of separators, then the phrase. The phrase is
//     found first, and a finder checks that it matches there; a match then
//     starts at the boundary before the run of separators in front of it.
//   - run: an address or a handle, a bounded run of word characters before an
//     "@", an "at", "on snap" and the like. What follows the run is found
//     first, and checked; a match then starts a bounded number of
//     characters before it, within the same run.
//
// Either way the place found is where the leftmost match starts, so the
// machine is run from there alone.
//
// Some patterns are read not on a view but on the few words round each
// match of a rule, once a match: the sentence round a threat, say. They are
// window searches (newWindowSearch), searched the same way over the stretch
// of text they are given, each with a trie of its own.
//
// The trie and the two shapes only say where a branch is worth running;
// the branch's program decides. A pattern the planner cannot read this way
// is run as before, by Go's regexp, and TestFilterSearchesAreAllPlanned
// fails if any is.
//
// What a search costs is counted, in steps that do not depend on the
// computer (searchIndex.work), so that TestFilterCostIsBounded can hold it
// to a number of steps for each character a post has.

// The families of searches: the plain view is searched with the plain form of
// every rule, the loose view with the loose form of the rules that have one.
// A rule with no loose form has a family of its own, because its plain form
// is still searched when the loose view stands in for the plain one. Each
// window search (newWindowSearch) is a family of its own after these.
const (
	famPlain = iota
	famPlainOnly
	famLoose
	numFamilies
)

// search is one regexp of a rule, with the plan for running it.
type search struct {
	src    string
	family int
	// branches are the pattern's branches in order of preference; nil when
	// the pattern is run whole, by Go's regexp.
	branches []*branch
	// numSubexp is how many groups the pattern has.
	numSubexp int
	whole     struct {
		once sync.Once
		re   *regexp.Regexp
	}
}

// String returns the pattern.
func (s *search) String() string { return s.src }

// NumSubexp returns how many groups the pattern has.
func (s *search) NumSubexp() int { return s.regexp().NumSubexp() }

// regexp is the whole pattern, compiled the first time it is needed: only a
// search that could not be planned, and the tests, need it.
func (s *search) regexp() *regexp.Regexp {
	s.whole.once.Do(func() { s.whole.re = regexp.MustCompile(s.src) })
	return s.whole.re
}

// searches holds every search the rules make, and what planning them built.
var searches struct {
	once    sync.Once
	all     []*search
	windows int // how many window searches there are
	// Per family: its trie, and how many groups and runs of a class it has,
	// which size the state of a search over a text.
	tries  []*trie
	groups []int
	slots  []int
	// classList is every class, by id; maxInst and maxCap size the
	// machines that run branches.
	classList       []*runeClass
	maxInst, maxCap int
	// viables are the distinct viabilities of the branches' instructions,
	// which each branch's via names by instruction.
	viables []viable
}

// newSearch records a rule's regexp. Rules are package variables, so every
// search exists before the first post is read, and is planned then.
func newSearch(src string, family int) *search {
	s := &search{src: src, family: family}
	searches.all = append(searches.all, s)
	return s
}

// newWindowSearch records a regexp the filter runs on a short stretch of a
// view, such as the sentence round a match, rather than on the view: once
// for every match of a rule, so it is searched as a rule is, from where it
// can start, with a trie of its own.
func newWindowSearch(src string) *search {
	searches.windows++
	return newSearch(src, numFamilies+searches.windows-1)
}

// prepareSearches plans every search, once. A server starts it as it
// starts serving (App.Routes), so that the first post need not wait for all
// of it; anything else that reads a post plans them on first use.
func prepareSearches() { searches.once.Do(planSearches) }

func planSearches() {
	p := newPlanner(numFamilies + searches.windows)
	for _, s := range searches.all {
		re, err := syntax.Parse(s.src, syntax.Perl)
		if err != nil {
			panic("filter pattern: " + err.Error())
		}
		s.numSubexp = re.MaxCap()
		s.branches = p.plan(re, s.family)
		p.forget()
	}
	searches.classList = p.list
	searches.viables = p.viables
	for _, tb := range p.tries {
		searches.tries = append(searches.tries, tb.build())
		searches.slots = append(searches.slots, len(tb.slots))
	}
	searches.groups = p.groups
	searches.maxInst, searches.maxCap = p.maxInst, p.maxCap
}

/* Character classes ----------------------------------------------------------- */

// runeClass is a set of characters, as a regexp character class has it.
type runeClass struct {
	id     int
	ranges []rune // sorted, merged [lo, hi] pairs
	ascii  [2]uint64
}

func (c *runeClass) has(r rune) bool {
	if uint32(r) < utf8.RuneSelf {
		return c.ascii[r>>6]&(1<<(uint(r)&63)) != 0
	}
	rs := c.ranges
	lo, hi := 0, len(rs)/2
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		switch {
		case r < rs[2*m]:
			hi = m
		case r > rs[2*m+1]:
			lo = m + 1
		default:
			return true
		}
	}
	return false
}

// subsetOf reports whether every character of c is in d.
func (c *runeClass) subsetOf(d *runeClass) bool {
	j := 0
	for i := 0; i < len(c.ranges); i += 2 {
		lo, hi := c.ranges[i], c.ranges[i+1]
		for j < len(d.ranges) && d.ranges[j+1] < lo {
			j += 2
		}
		if j >= len(d.ranges) || d.ranges[j] > lo || d.ranges[j+1] < hi {
			return false
		}
	}
	return true
}

// disjoint reports whether c and d have no character in common.
func (c *runeClass) disjoint(d *runeClass) bool {
	i, j := 0, 0
	for i < len(c.ranges) && j < len(d.ranges) {
		switch {
		case c.ranges[i+1] < d.ranges[j]:
			i += 2
		case d.ranges[j+1] < c.ranges[i]:
			j += 2
		default:
			return false
		}
	}
	return true
}

func mergeRanges(in []rune) []rune {
	pairs := make([][2]rune, 0, len(in)/2)
	for i := 0; i+1 < len(in); i += 2 {
		pairs = append(pairs, [2]rune{in[i], in[i+1]})
	}
	slices.SortFunc(pairs, func(a, b [2]rune) int { return int(a[0] - b[0]) })
	var out []rune
	for _, p := range pairs {
		if n := len(out); n > 0 && p[0] <= out[n-1]+1 {
			out[n-1] = max(out[n-1], p[1])
			continue
		}
		out = append(out, p[0], p[1])
	}
	return out
}

/* Planning -------------------------------------------------------------------- */

type planner struct {
	classes map[string]*runeClass
	list    []*runeClass
	tries   []*trieBuilder
	groups  []int
	// Branches share most of their syntax trees, so what is worked out
	// about a node is kept by node.
	singles map[*syntax.Regexp]*runeClass
	covers  map[*syntax.Regexp][]entry
	firsts  map[*syntax.Regexp]firstSet
	sizes   map[*syntax.Regexp]int
	// runeSets are the ranges of the programs' character classes, one
	// copy of each (shareRunes), found by a hash of them or by the slice
	// the parser made.
	runeSets map[uint64][][]rune
	runesAt  map[*rune][]rune
	// maxInst and maxCap are the largest program and the most groups of
	// any branch.
	maxInst, maxCap int
	// viables are the distinct viabilities, and viableAt where each is;
	// viability works them out in scratch, one program at a time.
	viables  []viable
	viableAt map[viable]uint16
	scratch  []viable
}

type firstSet struct {
	ranges []rune
	empty  bool
}

func newPlanner(families int) *planner {
	p := &planner{
		classes:  map[string]*runeClass{},
		tries:    make([]*trieBuilder, families),
		groups:   make([]int, families),
		runeSets: map[uint64][][]rune{},
		viableAt: map[viable]uint16{},
	}
	for f := range p.tries {
		p.tries[f] = &trieBuilder{nodes: []buildNode{{}}, slots: map[int]int32{}}
	}
	p.forget()
	return p
}

// forget lets go of what was worked out about the nodes of one pattern,
// once it is planned. No two patterns share a node, and holding on to every
// pattern's tree until the last was planned more than doubled the heap a
// server needs as it starts: a "[^\pL\pN]" alone is some 11 kB of ranges,
// and the patterns have thousands.
func (p *planner) forget() {
	p.singles = map[*syntax.Regexp]*runeClass{}
	p.covers = map[*syntax.Regexp][]entry{}
	p.firsts = map[*syntax.Regexp]firstSet{}
	p.sizes = map[*syntax.Regexp]int{}
	p.runesAt = map[*rune][]rune{}
}

// class interns a character class.
func (p *planner) class(ranges []rune) *runeClass {
	rs := mergeRanges(ranges)
	var key []byte
	for _, r := range rs {
		key = strconv.AppendInt(key, int64(r), 16)
		key = append(key, ',')
	}
	if c, ok := p.classes[string(key)]; ok {
		return c
	}
	c := &runeClass{id: len(p.list), ranges: rs}
	for i := 0; i < len(rs); i += 2 {
		for r := rs[i]; r <= rs[i+1] && r < utf8.RuneSelf; r++ {
			c.ascii[r>>6] |= 1 << (uint(r) & 63)
		}
	}
	p.classes[string(key)] = c
	p.list = append(p.list, c)
	return c
}

// singleClass is the class of what re matches when re matches exactly one
// character, and nil otherwise.
func (p *planner) singleClass(re *syntax.Regexp) *runeClass {
	if c, ok := p.singles[re]; ok {
		return c
	}
	c := p.singleClassOf(re)
	p.singles[re] = c
	return c
}

func (p *planner) singleClassOf(re *syntax.Regexp) *runeClass {
	switch re.Op {
	case syntax.OpLiteral:
		if len(re.Rune) != 1 {
			return nil
		}
		r := re.Rune[0]
		ranges := []rune{r, r}
		if re.Flags&syntax.FoldCase != 0 {
			for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
				ranges = append(ranges, f, f)
			}
		}
		return p.class(ranges)
	case syntax.OpCharClass:
		return p.class(re.Rune)
	case syntax.OpAnyCharNotNL:
		return p.class([]rune{0, '\n' - 1, '\n' + 1, unicode.MaxRune})
	case syntax.OpAnyChar:
		return p.class([]rune{0, unicode.MaxRune})
	case syntax.OpCapture:
		return p.singleClass(re.Sub[0])
	}
	return nil
}

func emptyOpOf(re *syntax.Regexp) (syntax.EmptyOp, bool) {
	switch re.Op {
	case syntax.OpBeginLine:
		return syntax.EmptyBeginLine, true
	case syntax.OpEndLine:
		return syntax.EmptyEndLine, true
	case syntax.OpBeginText:
		return syntax.EmptyBeginText, true
	case syntax.OpEndText:
		return syntax.EmptyEndText, true
	case syntax.OpWordBoundary:
		return syntax.EmptyWordBoundary, true
	case syntax.OpNoWordBoundary:
		return syntax.EmptyNoWordBoundary, true
	}
	return 0, false
}

func hasCapture(re *syntax.Regexp) bool {
	if re.Op == syntax.OpCapture {
		return true
	}
	for _, s := range re.Sub {
		if hasCapture(s) {
			return true
		}
	}
	return false
}

// minRunes is the fewest characters re can match.
func minRunes(re *syntax.Regexp) int {
	switch re.Op {
	case syntax.OpLiteral:
		return len(re.Rune)
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return 1
	case syntax.OpCapture, syntax.OpPlus:
		return minRunes(re.Sub[0])
	case syntax.OpRepeat:
		return re.Min * minRunes(re.Sub[0])
	case syntax.OpConcat:
		n := 0
		for _, s := range re.Sub {
			n += minRunes(s)
		}
		return n
	case syntax.OpAlternate:
		n := -1
		for _, s := range re.Sub {
			if m := minRunes(s); n < 0 || m < n {
				n = m
			}
		}
		return max(n, 0)
	}
	return 0
}

// maxBranches bounds how many branches one pattern is split into.
const maxBranches = 256

// maxSplitTail bounds what splitting copies: an alternation is split only
// when what follows it in the pattern compiles to at most this many
// instructions (as size estimates them), since every branch compiles its
// own copy. Splitting the subjects of "i will" and "someone should" in
// front of the threat patterns' long list of acts would compile the list
// once for every subject, two dozen times over, and find no place worth
// running a branch from that the unsplit branch does not find.
const maxSplitTail = 4096

// split splits a pattern into branches: at an alternation, into the
// alternatives, each with what comes before and after it in the pattern, as
// often as maxBranches allows. At any one place the pattern matches what its
// first alternative that matches there matches, so the branches tried in
// order find the same match. That holds when what comes before the
// alternation can match only one way, which forced checks, and when the
// alternatives have no groups in them, so that every branch has the
// pattern's groups, numbered as the pattern numbers them.
func (p *planner) split(re *syntax.Regexp) []*syntax.Regexp {
	out := []*syntax.Regexp{re}
	for changed := true; changed; {
		changed = false
		var next []*syntax.Regexp
		for i, t := range out {
			parts := p.splitOnce(t)
			if len(parts) > 1 && len(next)+len(parts)+len(out)-i-1 <= maxBranches {
				next = append(next, parts...)
				changed = true
			} else {
				next = append(next, t)
			}
		}
		out = next
	}
	return out
}

func (p *planner) splitOnce(re *syntax.Regexp) []*syntax.Regexp {
	splittable := func(a *syntax.Regexp) bool {
		// An alternative that can match nothing, such as the "^" of
		// "(?:^|[.?!])", starts no branch of its own: what follows it
		// starts it, and that is the delimited shape.
		return a.Op == syntax.OpAlternate && !hasCapture(a) &&
			!slices.ContainsFunc(a.Sub, func(x *syntax.Regexp) bool { return minRunes(x) == 0 })
	}
	switch {
	case splittable(re):
		return re.Sub
	case re.Op == syntax.OpConcat:
		for h, sub := range re.Sub {
			if splittable(sub) && p.forced(re.Sub, h) {
				tail := 0
				for _, t := range re.Sub[h+1:] {
					tail += p.size(t)
				}
				if tail > maxSplitTail {
					break
				}
				var out []*syntax.Regexp
				for _, a := range sub.Sub {
					subs := slices.Clone(re.Sub[:h])
					if a.Op == syntax.OpConcat {
						subs = append(subs, a.Sub...)
					} else {
						subs = append(subs, a)
					}
					subs = append(subs, re.Sub[h+1:]...)
					out = append(out, &syntax.Regexp{Op: syntax.OpConcat, Flags: re.Flags, Sub: subs})
				}
				return out
			}
		}
	}
	return []*syntax.Regexp{re}
}

// size estimates how many instructions re compiles to, as syntax.Compile
// compiles it once simplified: a bounded repetition is that many copies.
func (p *planner) size(re *syntax.Regexp) int {
	if n, ok := p.sizes[re]; ok {
		return n
	}
	n := 1
	switch re.Op {
	case syntax.OpLiteral:
		n = len(re.Rune)
	case syntax.OpRepeat:
		n = (p.size(re.Sub[0]) + 1) * max(re.Max, re.Min, 1)
	default:
		for _, sub := range re.Sub {
			n += p.size(sub)
		}
	}
	p.sizes[re] = n
	return n
}

// forced reports whether subs[:h] can match only one way in front of
// subs[h:]: it is assertions, single characters, and runs of a class that
// what follows them cannot start with, so that a run is always the longest.
func (p *planner) forced(subs []*syntax.Regexp, h int) bool {
	for i := 0; i < h; i++ {
		c := subs[i]
		for c.Op == syntax.OpCapture {
			c = c.Sub[0]
		}
		if _, ok := emptyOpOf(c); ok || (c.Op == syntax.OpLiteral && c.Flags&syntax.FoldCase == 0) ||
			c.Op != syntax.OpLiteral && p.singleClass(c) != nil {
			continue
		}
		if c.Op != syntax.OpStar && c.Op != syntax.OpPlus {
			return false
		}
		run := p.singleClass(c.Sub[0])
		if run == nil {
			return false
		}
		first, empty := p.firstRunes(concatOf(subs[i+1 : h+1]))
		if empty || len(intersectRanges(first, run.ranges)) > 0 {
			return false
		}
	}
	return true
}

// firstRunes is the characters a match of re can start with, as ranges, and
// whether re can match nothing, when what follows it matters too.
func (p *planner) firstRunes(re *syntax.Regexp) ([]rune, bool) {
	if f, ok := p.firsts[re]; ok {
		return f.ranges, f.empty
	}
	r, e := p.firstRunesOf(re)
	p.firsts[re] = firstSet{r, e}
	return r, e
}

func (p *planner) firstRunesOf(re *syntax.Regexp) ([]rune, bool) {
	switch re.Op {
	case syntax.OpLiteral:
		if len(re.Rune) == 0 {
			return nil, true
		}
		return p.singleClass(&syntax.Regexp{Op: syntax.OpLiteral, Rune: re.Rune[:1], Flags: re.Flags}).ranges, false
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return p.singleClass(re).ranges, false
	case syntax.OpCapture, syntax.OpPlus:
		return p.firstRunes(re.Sub[0])
	case syntax.OpStar, syntax.OpQuest:
		r, _ := p.firstRunes(re.Sub[0])
		return r, true
	case syntax.OpRepeat:
		r, e := p.firstRunes(re.Sub[0])
		return r, e || re.Min == 0
	case syntax.OpConcat:
		var acc []rune
		for _, sub := range re.Sub {
			r, e := p.firstRunes(sub)
			acc = mergeRanges(append(acc, r...))
			if !e {
				return acc, false
			}
		}
		return acc, true
	case syntax.OpAlternate:
		var acc []rune
		empty := false
		for _, sub := range re.Sub {
			r, e := p.firstRunes(sub)
			acc = mergeRanges(append(acc, r...))
			empty = empty || e
		}
		return acc, empty
	case syntax.OpNoMatch:
		return nil, false
	}
	return nil, true // empty-width
}

func concatOf(subs []*syntax.Regexp) *syntax.Regexp {
	if len(subs) == 1 {
		return subs[0]
	}
	return &syntax.Regexp{Op: syntax.OpConcat, Sub: subs}
}

// plan splits a pattern into branches and plans each; nil when some branch
// cannot be planned, and the pattern is run whole.
func (p *planner) plan(re *syntax.Regexp, family int) []*branch {
	var out []*branch
	for _, t := range p.split(re) {
		// A branch that can match nothing would need FindAll's rule for
		// empty matches, which the search loop does not repeat.
		if minRunes(t) == 0 {
			return nil
		}
		b := p.branch(t, family)
		if b == nil {
			return nil
		}
		out = append(out, b)
	}
	return out
}

type branchKind uint8

const (
	literalBranch branchKind = iota
	delimitedBranch
	runBranch
)

// branch is one branch of a pattern: its program, and what says where it
// can start.
type branch struct {
	kind branchKind
	// prog is the branch compiled as Go's regexp compiles a pattern, and
	// cond the empty-width conditions it starts with.
	prog *syntax.Prog
	cond syntax.EmptyOp
	// via is the viability of each instruction of prog, as an index into
	// searches.viables.
	via []uint16
	// trigger, for a literal branch, names its openings in the trie.
	trigger int32
	// rest is what follows the boundary and its separators (delimited), or
	// the run (run).
	rest *group

	// delimited: (?:^|delims) skip* rest
	delims *runeClass
	begin  bool
	skip   *runeClass
	// slot is where a search keeps the last run it measured of skip, or
	// of runMore.
	slot int32

	// run: lead runFirst runMore{min,max} rest, where the lead is an
	// assertion, or leadRune or an assertion.
	hasLeadRune       bool
	leadRune          rune
	leadOp            syntax.EmptyOp
	runFirst, runMore *runeClass
	runMin, runMax    int
}

func (p *planner) branch(t *syntax.Regexp, family int) *branch {
	b := &branch{}
	if !p.delimited(b, t, family) && !p.run(b, t, family) {
		id, ok := p.trigger(t, family, openAlone, nil)
		if !ok {
			return nil
		}
		b.kind, b.trigger = literalBranch, id
	}
	return p.compile(b, t)
}

// compile compiles t into b as regexp.Compile does: simplify, then compile.
// Simplify shares what it does not change, and changes nothing it is given.
func (p *planner) compile(b *branch, t *syntax.Regexp) *branch {
	prog, err := syntax.Compile(t.Simplify())
	if err != nil {
		return nil
	}
	for i := range prog.Inst {
		if r := prog.Inst[i].Rune; len(r) > 2 {
			prog.Inst[i].Rune = p.shareRunes(r)
		}
	}
	b.prog, b.cond = prog, prog.StartCond()
	b.via = p.viability(prog)
	p.maxInst = max(p.maxInst, len(prog.Inst))
	p.maxCap = max(p.maxCap, prog.NumCap)
	return b
}

// shareRunes is one copy of the ranges r, for every program whose
// instructions match them. A compiled instruction keeps the slice the parser
// made, and the parser makes one for every "[^\pL\pN]" in every pattern:
// thousands of copies of the same thousands of ranges, most of what the
// programs would otherwise keep alive.
func (p *planner) shareRunes(r []rune) []rune {
	if s, ok := p.runesAt[&r[0]]; ok && len(s) == len(r) {
		return s
	}
	h := uint64(14695981039346656037)
	for _, x := range r {
		h = (h ^ uint64(x)) * 1099511628211
	}
	for _, s := range p.runeSets[h] {
		if slices.Equal(s, r) {
			p.runesAt[&r[0]] = s
			return s
		}
	}
	s := slices.Clone(r)
	p.runeSets[h] = append(p.runeSets[h], s)
	p.runesAt[&r[0]] = s
	return s
}

// delimited plans (?:^|[D])[S]*rest.
func (p *planner) delimited(b *branch, t *syntax.Regexp, family int) bool {
	if t.Op != syntax.OpConcat || len(t.Sub) < 3 {
		return false
	}
	lead, star := t.Sub[0], t.Sub[1]
	begin := false
	if lead.Op == syntax.OpAlternate && len(lead.Sub) == 2 {
		x, y := lead.Sub[0], lead.Sub[1]
		if y.Op == syntax.OpBeginText {
			x, y = y, x
		}
		if x.Op != syntax.OpBeginText {
			return false
		}
		lead, begin = y, true
	}
	delims := p.singleClass(lead)
	if delims == nil || star.Op != syntax.OpStar {
		return false
	}
	skip := p.singleClass(star.Sub[0])
	if skip == nil {
		return false
	}
	// rest must start outside the separators, so that the run of them after
	// the boundary ends exactly where rest starts.
	g := p.group(concatOf(t.Sub[2:]), family, openAfterSkip, skip)
	if g == nil {
		return false
	}
	b.kind, b.rest, b.delims, b.begin, b.skip = delimitedBranch, g, delims, begin, skip
	b.slot = p.tries[family].slot(skip)
	return true
}

// run plans lead([C1][C2]{min,max})rest, the lead an assertion, or a literal
// character or an assertion.
func (p *planner) run(b *branch, t *syntax.Regexp, family int) bool {
	if t.Op != syntax.OpConcat || len(t.Sub) < 3 {
		return false
	}
	i := 0
	if op, ok := emptyOpOf(t.Sub[0]); ok {
		b.leadOp, i = op, 1
	} else if a := t.Sub[0]; a.Op == syntax.OpAlternate && len(a.Sub) == 2 {
		x := a.Sub[0]
		if x.Op == syntax.OpCapture {
			x = x.Sub[0]
		}
		op, ok := emptyOpOf(a.Sub[1])
		if !ok || x.Op != syntax.OpLiteral || len(x.Rune) != 1 || x.Flags&syntax.FoldCase != 0 {
			return false
		}
		b.hasLeadRune, b.leadRune, b.leadOp, i = true, x.Rune[0], op, 1
	}
	if i+1 >= len(t.Sub) {
		return false
	}
	run := t.Sub[i]
	if run.Op == syntax.OpCapture {
		run = run.Sub[0]
	}
	if run.Op != syntax.OpConcat || len(run.Sub) != 2 || run.Sub[1].Op != syntax.OpRepeat || run.Sub[1].Max < 0 {
		return false
	}
	first, more := p.singleClass(run.Sub[0]), p.singleClass(run.Sub[1].Sub[0])
	if first == nil || more == nil {
		return false
	}
	g := p.group(concatOf(t.Sub[i+1:]), family, openAfterRun, nil)
	if g == nil {
		return false
	}
	b.kind, b.rest = runBranch, g
	b.runFirst, b.runMore, b.runMin, b.runMax = first, more, run.Sub[1].Min, run.Sub[1].Max
	b.slot = p.tries[family].slot(more)
	return true
}

// group is what follows a delimited branch's separators or a run branch's
// run: the branches it splits into, found by their openings, which say
// where it can start.
type group struct {
	id       int
	triggers []int32
	// prog is what follows, compiled, to tell where it matches before the
	// branch is run.
	prog *branch
}

func (p *planner) group(t *syntax.Regexp, family int, mode openingMode, outside *runeClass) *group {
	g := &group{id: p.groups[family]}
	for _, bt := range p.split(t) {
		id, ok := p.trigger(bt, family, mode, outside)
		if !ok {
			return nil
		}
		g.triggers = append(g.triggers, id)
	}
	g.prog = p.compile(&branch{}, t)
	if g.prog == nil {
		return nil
	}
	p.groups[family]++
	return g
}

// openingMode says where a branch's openings are looked for.
type openingMode uint8

const (
	// openAlone: the branch starts a match, so it is run wherever its
	// opening is found.
	openAlone openingMode = iota
	// openAfterSkip: the branch follows a delimited branch's run of
	// separators, so it must start outside them.
	openAfterSkip
	// openAfterRun: the branch follows a run branch's run, and is only run
	// where that run can end, so it may start with a run of its own.
	openAfterRun
)

// trigger puts the openings of t into its family's trie under a new trigger.
func (p *planner) trigger(t *syntax.Regexp, family int, mode openingMode, outside *runeClass) (int32, bool) {
	var all [][]item
	for _, e := range p.cover(t) {
		items := normalize(e.items)
		if !usableOpening(items, mode, outside) {
			return 0, false
		}
		all = append(all, items)
	}
	tb := p.tries[family]
	id := tb.triggers
	tb.triggers++
	for _, items := range all {
		tb.insert(items, id)
	}
	return id, true
}

// usableOpening reports whether a normalised opening says where a match
// starts: it reaches a character it names (or, after a run, a run and then
// one), outside the class outside when that is given. A branch run wherever
// its opening is found also must not open with a character the run of
// skipped characters after it could repeat, as "-+>" does: the opening would
// be found at every character of a long run, and the branch run over the
// rest of the run from each. A \b in front rules that out when the
// characters the two share are all word characters or all not, since then
// there is no word boundary inside the run.
func usableOpening(items []item, mode openingMode, outside *runeClass) bool {
	i := 0
	bounded := false
	for i < len(items) && items[i].kind == itemAssert {
		bounded = bounded || items[i].op&syntax.EmptyWordBoundary != 0
		i++
	}
	if i == len(items) {
		return false
	}
	if items[i].kind == itemSkip || items[i].kind == itemGap {
		return mode == openAfterRun && i+1 < len(items)
	}
	first := items[i]
	if outside != nil && first.overlaps(outside) {
		return false
	}
	if mode != openAlone {
		return true
	}
	for j := i + 1; j < len(items); j++ {
		switch items[j].kind {
		case itemAssert:
			continue
		case itemSkip:
			shared := first.shared(items[j].class)
			return shared == nil || bounded && (allWord(shared) || noWord(shared))
		}
		// A gap is bounded, so the branch is run over at most that much
		// of a run from each place.
		break
	}
	return true
}

func intersectRanges(a, b []rune) []rune {
	var out []rune
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		lo, hi := max(a[i], b[j]), min(a[i+1], b[j+1])
		if lo <= hi {
			out = append(out, lo, hi)
		}
		if a[i+1] < b[j+1] {
			i += 2
		} else {
			j += 2
		}
	}
	return out
}

// shared is the characters an opening item has in common with c, as
// ranges, or nil.
func (it item) shared(c *runeClass) []rune {
	if it.kind == itemRune {
		if c.has(it.r) {
			return []rune{it.r, it.r}
		}
		return nil
	}
	return intersectRanges(it.class.ranges, c.ranges)
}

var wordRanges = []rune{'0', '9', 'A', 'Z', '_', '_', 'a', 'z'}

func allWord(rs []rune) bool { return rangeSize(intersectRanges(rs, wordRanges)) == rangeSize(rs) }

func rangeSize(rs []rune) int {
	n := 0
	for i := 0; i+1 < len(rs); i += 2 {
		n += int(rs[i+1]-rs[i]) + 1
	}
	return n
}

func noWord(rs []rune) bool { return len(intersectRanges(rs, wordRanges)) == 0 }

/* Openings -------------------------------------------------------------------- */

type itemKind uint8

const (
	itemRune   itemKind = iota // one given character
	itemClass                  // one character of a class
	itemSkip                   // the longest run of characters of a class, perhaps none
	itemGap                    // up to n characters of a class, every length tried
	itemAssert                 // an empty-width assertion
)

type item struct {
	kind  itemKind
	r     rune
	class *runeClass
	op    syntax.EmptyOp
	n     int
}

func (it item) overlaps(c *runeClass) bool {
	if it.kind == itemRune {
		return c.has(it.r)
	}
	return !it.class.disjoint(c)
}

// entry is one opening: a sequence every match of some part of the pattern
// starts with. An open entry is the whole of such a match, so what follows
// it in the pattern can be appended.
type entry struct {
	items []item
	open  bool
}

const (
	// maxCoverEntries bounds the openings one part of a pattern is read as;
	// past it the openings stop where they are.
	maxCoverEntries = 128
	// maxCoverItems is how many characters of an opening are read: enough
	// to tell a phrase from the words it shares a start with.
	maxCoverItems = 12
	// maxGap is the longest bounded repetition read as a gap rather than a
	// skip.
	maxGap = 100
)

// weight is how many characters an opening names after its last gap: a gap
// is only worth walking for what comes after it.
func weight(items []item) int {
	n := 0
	for _, it := range items {
		switch it.kind {
		case itemRune, itemClass:
			n++
		case itemGap:
			n = 0
		}
	}
	return n
}

// cover reads what every match of re starts with. Repetitions of one
// character become skips, which may claim more than the pattern allows (a
// skip has no upper bound) but never less, so the openings are a necessary
// condition for a match and never a sufficient one.
func (p *planner) cover(re *syntax.Regexp) []entry {
	if c, ok := p.covers[re]; ok {
		return c
	}
	c := p.coverOf(re)
	p.covers[re] = c
	return c
}

// coverOf is cover, worked out. What it returns is shared, and never
// changed afterwards.
func (p *planner) coverOf(re *syntax.Regexp) []entry {
	one := func(it item) []entry { return []entry{{items: []item{it}, open: true}} }
	switch re.Op {
	case syntax.OpNoMatch:
		return nil
	case syntax.OpEmptyMatch:
		return []entry{{open: true}}
	case syntax.OpLiteral:
		items := make([]item, len(re.Rune))
		for i, r := range re.Rune {
			if re.Flags&syntax.FoldCase != 0 {
				items[i] = item{kind: itemClass, class: p.singleClass(&syntax.Regexp{Op: syntax.OpLiteral, Rune: []rune{r}, Flags: re.Flags})}
			} else {
				items[i] = item{kind: itemRune, r: r}
			}
		}
		return []entry{{items: items, open: true}}
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		c := p.singleClass(re)
		if len(c.ranges) == 2 && c.ranges[0] == c.ranges[1] {
			return one(item{kind: itemRune, r: c.ranges[0]})
		}
		return one(item{kind: itemClass, class: c})
	case syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		op, _ := emptyOpOf(re)
		return one(item{kind: itemAssert, op: op})
	case syntax.OpCapture:
		return p.cover(re.Sub[0])
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
		if c := p.singleClass(re.Sub[0]); c != nil && hi != 1 {
			if lo > maxCoverItems {
				return []entry{{}}
			}
			var items []item
			for range lo {
				items = append(items, item{kind: itemClass, class: c})
			}
			switch {
			case hi >= 0 && hi-lo <= maxGap && hi != lo:
				items = append(items, item{kind: itemGap, class: c, n: hi - lo})
			case hi != lo:
				items = append(items, item{kind: itemSkip, class: c})
			}
			return []entry{{items: items, open: true}}
		}
		sub := p.cover(re.Sub[0])
		var out []entry
		for _, e := range sub {
			// Once is the whole match only for X? and X{1}.
			out = append(out, entry{items: e.items, open: e.open && hi == 1})
		}
		if lo == 0 {
			out = append(out, entry{open: true})
		}
		return out
	case syntax.OpConcat:
		cur := []entry{{open: true}}
		for _, sub := range re.Sub {
			if !slices.ContainsFunc(cur, func(e entry) bool { return e.open }) {
				break
			}
			sc := p.cover(sub)
			next := make([]entry, 0, len(cur))
			for _, a := range cur {
				if !a.open || weight(a.items) >= maxCoverItems {
					next = append(next, entry{items: a.items})
					continue
				}
				for _, s := range sc {
					items := append(slices.Clip(a.items), s.items...)
					next = append(next, entry{items: items, open: s.open})
				}
			}
			cur = shrink(next)
		}
		return cur
	case syntax.OpAlternate:
		var out []entry
		for _, sub := range re.Sub {
			out = append(out, p.cover(sub)...)
		}
		return shrink(out)
	}
	return []entry{{}}
}

// shrink keeps the number of openings within maxCoverEntries by cutting them
// all short, as little as will do: the start of an opening still says how
// every match it covers starts.
func shrink(entries []entry) []entry {
	if len(entries) <= maxCoverEntries {
		return entries
	}
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b entry) int { return compareItems(a.items, b.items) })
	// common[i] is how many items sorted[i] shares with sorted[i-1].
	common := make([]int, len(sorted))
	longest := len(sorted[0].items)
	for i := 1; i < len(sorted); i++ {
		a, b := sorted[i-1].items, sorted[i].items
		n := 0
		for n < len(a) && n < len(b) && a[n] == b[n] {
			n++
		}
		common[i] = n
		longest = max(longest, len(b))
	}
	// Cut to k items, the openings that start alike are one; sorted, they
	// are neighbours.
	same := func(i, k int) bool {
		a, b := min(len(sorted[i-1].items), k), min(len(sorted[i].items), k)
		return a == b && common[i] >= a
	}
	for k := longest - 1; k > 0; k-- {
		n := 1
		for i := 1; i < len(sorted); i++ {
			if !same(i, k) {
				n++
			}
		}
		if n > maxCoverEntries {
			continue
		}
		out := make([]entry, 0, n)
		for i, e := range sorted {
			if len(e.items) > k {
				e = entry{items: e.items[:k]}
			}
			if i > 0 && same(i, k) {
				out[len(out)-1].open = out[len(out)-1].open && e.open
				continue
			}
			out = append(out, e)
		}
		return out
	}
	return []entry{{}}
}

// compareItems orders openings, so that those that start alike sort
// together.
func compareItems(a, b []item) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		if c := cmp.Compare(x.kind, y.kind); c != 0 {
			return c
		}
		if c := cmp.Compare(x.r, y.r); c != 0 {
			return c
		}
		var cx, cy int
		if x.class != nil {
			cx = x.class.id + 1
		}
		if y.class != nil {
			cy = y.class.id + 1
		}
		if c := cmp.Compare(cx, cy); c != 0 {
			return c
		}
		if c := cmp.Compare(x.op, y.op); c != 0 {
			return c
		}
		if c := cmp.Compare(x.n, y.n); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

// normalize makes an opening walkable. After a skip, characters the skip
// would take are dropped, because the walk takes the longest run; a class
// that only partly overlaps the skip, or a skip after one it does not fit
// in, ends the opening, before the skip, since where that run ends cannot be
// told. A gap followed by a character it cannot take is a skip, since the
// longest run is then the only one that can be followed by that character;
// any other gap is walked at every length, and the opening ends before a
// second one. A gap may follow a skip it
// overlaps when what follows the gap is outside the skip, or a skip that
// takes everything it does: wherever the longest run ends, the gap's
// shorter lengths and what follows it then cover where the pattern's run
// could have ended.
func normalize(items []item) []item {
	out := make([]item, 0, len(items))
	var skip *runeClass
	// earlier are the skips before skip that it follows directly, each
	// with nothing in common with those after it, so that each run ends
	// where the next begins; what follows must be outside all of them.
	var earlier []*runeClass
	pending := 0
	// gapped is whether a gap has been kept: the opening ends before a
	// second, since the walk would try every length of the second after
	// every length of the first.
	gapped := false
	outside := func(it item) bool {
		for _, c := range earlier {
			if it.overlaps(c) {
				return false
			}
		}
		return true
	}
	for i, it := range items {
		if it.kind == itemGap {
			j := i + 1
			for j < len(items) && items[j].kind == itemAssert {
				j++
			}
			if j == len(items) {
				break
			}
			next := items[j]
			if (next.kind == itemRune || next.kind == itemClass) && !next.overlaps(it.class) {
				it = item{kind: itemSkip, class: it.class}
			} else if skip != nil && !it.class.subsetOf(skip) {
				if len(out) > pending+1 || !((next.kind == itemRune || next.kind == itemClass) && !next.overlaps(skip) ||
					next.kind == itemSkip && skip.subsetOf(next.class)) {
					return out[:pending]
				}
				skip = nil
			}
		}
		if skip != nil {
			switch it.kind {
			case itemRune:
				if skip.has(it.r) {
					continue
				}
			case itemClass:
				if it.class.subsetOf(skip) {
					continue
				}
				if !it.class.disjoint(skip) {
					return out[:pending]
				}
			case itemSkip:
				if it.class.subsetOf(skip) {
					continue
				}
				// A run of characters the run before cannot take
				// starts where that run ends.
				if it.class.disjoint(skip) && outside(it) {
					earlier = append(earlier, skip)
					skip = it.class
					out = append(out, it)
					continue
				}
				return out[:pending]
			case itemGap:
				if it.class.subsetOf(skip) {
					continue
				}
				return out[:pending]
			}
			if it.kind != itemAssert {
				if !outside(it) {
					return out[:pending]
				}
				skip, earlier = nil, earlier[:0]
			}
		}
		if it.kind == itemSkip {
			skip, pending = it.class, len(out)
		}
		if it.kind == itemGap {
			if gapped {
				break
			}
			gapped = true
		}
		out = append(out, it)
	}
	if skip != nil {
		return out[:pending]
	}
	// Nothing after a trailing gap says anything.
	for len(out) > 0 && (out[len(out)-1].kind == itemGap || out[len(out)-1].kind == itemAssert) {
		out = out[:len(out)-1]
	}
	return out
}

/* The trie --------------------------------------------------------------------- */

type trieBuilder struct {
	nodes    []buildNode
	triggers int32
	// slots numbers the classes whose runs a search over a text measures:
	// a skip's in the trie, a delimited branch's separators, a run
	// branch's run.
	slots map[int]int32
}

// slot is where a search keeps the last run it measured of c.
func (t *trieBuilder) slot(c *runeClass) int32 {
	n, ok := t.slots[c.id]
	if !ok {
		n = int32(len(t.slots))
		t.slots[c.id] = n
	}
	return n
}

type buildNode struct {
	bytes map[byte]int32
	edges []trieEdge
	ends  []int32
}

// trieEdge is an edge that reads something other than one byte: a
// character of a class, a run of one, a gap, or an assertion.
type trieEdge struct {
	kind  itemKind
	op    syntax.EmptyOp
	class int32 // the class's id, or -1
	n     int32 // a gap's length; a skip's slot
	child int32
	// first, for a gap, is where in the trie's firsts the bytes are that
	// its child can go on with, or -1 for any.
	first int32
}

func (t *trieBuilder) insert(items []item, trigger int32) {
	n := int32(0)
	var buf [utf8.UTFMax]byte
	for _, it := range items {
		if it.kind == itemRune {
			k := utf8.EncodeRune(buf[:], it.r)
			for _, c := range buf[:k] {
				next, ok := t.nodes[n].bytes[c]
				if !ok {
					next = int32(len(t.nodes))
					t.nodes = append(t.nodes, buildNode{})
					if t.nodes[n].bytes == nil {
						t.nodes[n].bytes = map[byte]int32{}
					}
					t.nodes[n].bytes[c] = next
				}
				n = next
			}
			continue
		}
		class := int32(-1)
		if it.class != nil {
			class = int32(it.class.id)
		}
		en := int32(it.n)
		if it.kind == itemSkip {
			en = t.slot(it.class)
		}
		found := int32(-1)
		for _, e := range t.nodes[n].edges {
			if e.kind == it.kind && e.class == class && e.op == it.op && e.n == en {
				found = e.child
				break
			}
		}
		if found < 0 {
			found = int32(len(t.nodes))
			t.nodes = append(t.nodes, buildNode{})
			t.nodes[n].edges = append(t.nodes[n].edges, trieEdge{kind: it.kind, op: it.op, class: class, n: en, child: found, first: -1})
		}
		n = found
	}
	if !slices.Contains(t.nodes[n].ends, trigger) {
		t.nodes[n].ends = append(t.nodes[n].ends, trigger)
	}
}

// trie is every opening of a family's branches, in flat arrays the garbage
// collector has no reason to look into. Node 0 is the root, which is no
// node's child, so 0 in kids means no edge.
type trie struct {
	nodes  []trieNode
	kids   []int32 // kids[node.kids+c-node.lo] is the child for byte c
	edges  []trieEdge
	ends   []int32 // the triggers whose openings end at a node
	firsts []byteSet
	// start is where in firsts the bytes are that an opening can start
	// with, or -1 for any.
	start    int32
	triggers int
}

// byteSet is a set of bytes.
type byteSet [4]uint64

func (b *byteSet) has(c byte) bool { return b[c>>6]&(1<<(c&63)) != 0 }
func (b *byteSet) add(c byte)      { b[c>>6] |= 1 << (c & 63) }

// addClass adds every byte a character of c can start with.
func (b *byteSet) addClass(c *runeClass) {
	for i := range 2 {
		b[i] |= c.ascii[i]
	}
	if n := len(c.ranges); n > 0 && c.ranges[n-1] >= utf8.RuneSelf {
		for x := 0xc0; x <= 0xff; x++ {
			b.add(byte(x))
		}
	}
}

// trieNode is where a node's children, edges and ends are in its trie.
type trieNode struct {
	kids, edges, ends    int32
	nkids, nedges, nends uint16
	lo                   byte
}

func (t *trieBuilder) build() *trie {
	out := &trie{nodes: make([]trieNode, len(t.nodes)), kids: []int32{}, triggers: int(t.triggers)}
	for i, n := range t.nodes {
		if len(n.edges) > 0xffff || len(n.ends) > 0xffff {
			panic("filter trie: too many edges at one node")
		}
		tn := trieNode{
			kids: int32(len(out.kids)), edges: int32(len(out.edges)), ends: int32(len(out.ends)),
			nedges: uint16(len(n.edges)), nends: uint16(len(n.ends)),
		}
		out.edges = append(out.edges, n.edges...)
		out.ends = append(out.ends, n.ends...)
		if len(n.bytes) > 0 {
			lo, hi := byte(255), byte(0)
			for c := range n.bytes {
				lo, hi = min(lo, c), max(hi, c)
			}
			tn.lo, tn.nkids = lo, uint16(int(hi-lo)+1)
			out.kids = append(out.kids, make([]int32, tn.nkids)...)
			for c, k := range n.bytes {
				out.kids[int(tn.kids)+int(c-lo)] = k
			}
		}
		out.nodes[i] = tn
	}
	// A gap is walked at every length, and its child only where the text
	// goes on with a byte the child can read first; the walk starts only
	// where the text has a byte the root can.
	memo := map[int32]int32{}
	for i := range out.edges {
		if e := &out.edges[i]; e.kind == itemGap {
			e.first = out.firstOf(e.child, memo)
		}
	}
	out.start = out.firstOf(0, memo)
	return out
}

// firstOf finds the bytes that a walk from node n at a place in the text
// can go on with towards an opening's end, and returns where it put them
// in t.firsts, or -1 when any place can lead to one: n ends an opening.
// At the end of the text there is no byte, and n is always walked.
func (t *trie) firstOf(n int32, memo map[int32]int32) int32 {
	if f, ok := memo[n]; ok {
		return f
	}
	node := &t.nodes[n]
	f := int32(-1)
	if node.nends == 0 {
		var set byteSet
		all := false
		for i := range int(node.nkids) {
			if t.kids[int(node.kids)+i] != 0 {
				set.add(node.lo + byte(i))
			}
		}
		for _, e := range t.edges[node.edges : node.edges+int32(node.nedges)] {
			switch e.kind {
			case itemClass:
				set.addClass(searches.classList[e.class])
				continue
			case itemSkip, itemGap:
				set.addClass(searches.classList[e.class])
			}
			// What follows a skip, a gap or an assertion can start where
			// they do.
			g := t.firstOf(e.child, memo)
			if g < 0 {
				all = true
				break
			}
			for k := range set {
				set[k] |= t.firsts[g][k]
			}
		}
		if !all {
			f = int32(len(t.firsts))
			t.firsts = append(t.firsts, set)
		}
	}
	memo[n] = f
	return f
}

/* Searching a view ------------------------------------------------------------- */

// searchIndex is what one view knows about one family: where each trigger's
// openings are, and what has been learned while searching.
type searchIndex struct {
	text string
	trie *trie
	// hitAt holds, per trigger, the places its openings start, in order:
	// trigger i's are hitAt[hitOff[i]:hitOff[i+1]].
	hitAt, hitOff []int32
	// groups holds, per group, the places its branches can start, in
	// order, and the next place it matches.
	groups []groupState
	// runs holds, per class slot, the runs of the class measured so far,
	// each as long as it goes both ways, as sorted [from, to) pairs that do
	// not touch (runOf).
	runs [][]int
	// work counts what searching costs, in steps that do not depend on the
	// computer: trie nodes visited, characters scanned, and the threads a
	// machine ran at each character (machine.work).
	work int
}

// searchIndex returns the view's index for a family, walking the view for
// it the first time.
func (v *view) searchIndex(family int) *searchIndex {
	if ix := v.index[family]; ix != nil {
		return ix
	}
	ix := newSearchIndex(v.text, family)
	v.index[family] = ix
	return ix
}

// newSearchIndex walks a text for a family.
func newSearchIndex(text string, family int) *searchIndex {
	n := searches.slots[family]
	ix := &searchIndex{
		text:   text,
		trie:   searches.tries[family],
		groups: make([]groupState, searches.groups[family]),
		runs:   make([][]int, n),
	}
	ix.walk()
	return ix
}

func decodeRune(s string, i int) (rune, int) {
	if c := s[i]; c < utf8.RuneSelf {
		return rune(c), 1
	}
	return utf8.DecodeRuneInString(s[i:])
}

// holds evaluates an empty-width assertion at text[at], as Go's regexp does:
// \b and \B by ASCII word characters.
func holds(op syntax.EmptyOp, text string, at int) bool {
	if op&syntax.EmptyBeginText != 0 && at != 0 ||
		op&syntax.EmptyEndText != 0 && at != len(text) ||
		op&syntax.EmptyBeginLine != 0 && at != 0 && text[at-1] != '\n' ||
		op&syntax.EmptyEndLine != 0 && at != len(text) && text[at] != '\n' {
		return false
	}
	if op&(syntax.EmptyWordBoundary|syntax.EmptyNoWordBoundary) != 0 {
		b := (at > 0 && isWordByte(text[at-1])) != (at < len(text) && isWordByte(text[at]))
		if op&syntax.EmptyWordBoundary != 0 && !b || op&syntax.EmptyNoWordBoundary != 0 && b {
			return false
		}
	}
	return true
}

func isWordByte(c byte) bool { return c < utf8.RuneSelf && syntax.IsWordChar(rune(c)) }

// walk finds every opening in the text: from every character, every path of
// the trie the text follows. It notes each as trigger<<32 | place, in the
// order of the places, and then sorts them by trigger, which keeps that
// order within each.
func (ix *searchIndex) walk() {
	t := ix.trie
	n := t.triggers
	w := walker{
		ix: ix, text: ix.text, nodes: t.nodes, kids: t.kids, edges: t.edges, ends: t.ends, firsts: t.firsts,
		classes: searches.classList, last: make([]int32, n),
	}
	var first *byteSet
	if t.start >= 0 {
		first = &t.firsts[t.start]
	}
	for w.start < len(w.text) {
		if first == nil || first.has(w.text[w.start]) {
			w.visit(0, w.start)
		}
		if w.text[w.start] < utf8.RuneSelf {
			w.start++
		} else {
			_, k := utf8.DecodeRuneInString(w.text[w.start:])
			w.start += k
		}
	}
	ix.work += w.work
	ix.hitOff = make([]int32, n+1)
	for _, f := range w.found {
		ix.hitOff[f>>32+1]++
	}
	for i := range n {
		ix.hitOff[i+1] += ix.hitOff[i]
	}
	ix.hitAt = make([]int32, len(w.found))
	fill := w.last // reused: how many of each trigger are placed
	clear(fill)
	for _, f := range w.found {
		id := f >> 32
		ix.hitAt[ix.hitOff[id]+fill[id]] = int32(uint32(f))
		fill[id]++
	}
}

// hits is where the openings of a trigger start, in order.
func (ix *searchIndex) hits(id int32) []int32 {
	return ix.hitAt[ix.hitOff[id]:ix.hitOff[id+1]]
}

// walker is a walk of a trie over a text, from one place at a time.
type walker struct {
	ix      *searchIndex
	text    string
	nodes   []trieNode
	kids    []int32
	edges   []trieEdge
	ends    []int32
	firsts  []byteSet
	classes []*runeClass
	start   int
	last    []int32  // per trigger, the last place noted, plus one
	found   []uint64 // trigger<<32 | place
	work    int
}

// visit follows the trie from node n at text[at].
func (w *walker) visit(n int32, at int) {
	w.work++
	node := &w.nodes[n]
	if node.nends > 0 {
		for _, id := range w.ends[node.ends : node.ends+int32(node.nends)] {
			if w.last[id] != int32(w.start)+1 {
				w.found = append(w.found, uint64(id)<<32|uint64(w.start))
				w.last[id] = int32(w.start) + 1
			}
		}
	}
	text := w.text
	if node.nkids > 0 && at < len(text) {
		if i := int(text[at]) - int(node.lo); i >= 0 && i < int(node.nkids) {
			if k := w.kids[int(node.kids)+i]; k != 0 {
				w.visit(k, at+1)
			}
		}
	}
	if node.nedges == 0 {
		return
	}
	for i := node.edges; i < node.edges+int32(node.nedges); i++ {
		e := &w.edges[i]
		switch e.kind {
		case itemClass:
			if at < len(text) {
				if r, k := decodeRune(text, at); w.classes[e.class].has(r) {
					w.visit(e.child, at+k)
				}
			}
		case itemSkip:
			w.visit(e.child, w.ix.skipEnd(w.classes[e.class], e.n, at))
		case itemGap:
			c := w.classes[e.class]
			var first *byteSet
			if e.first >= 0 {
				first = &w.firsts[e.first]
			}
			for p, k := at, int32(0); ; k++ {
				w.work++
				if first == nil || p >= len(text) || first.has(text[p]) {
					w.visit(e.child, p)
				}
				if k == e.n || p >= len(text) {
					break
				}
				r, rw := decodeRune(text, p)
				if !c.has(r) {
					break
				}
				p += rw
			}
		case itemAssert:
			if holds(e.op, text, at) {
				w.visit(e.child, at)
			}
		}
	}
}

// skipEnd is where the run of c starting at at ends.
func (ix *searchIndex) skipEnd(c *runeClass, slot int32, at int) int {
	if at == len(ix.text) {
		return at
	}
	if r, _ := decodeRune(ix.text, at); !c.has(r) {
		return at
	}
	_, to := ix.runOf(c, slot, at)
	return to
}

// runStart is where the run of c that ends at q, or holds it, starts.
func (ix *searchIndex) runStart(c *runeClass, slot int32, q int) int {
	from, _ := ix.runOf(c, slot, q)
	return from
}

// runOf is the run of c that holds at or ends there, as long as it goes
// both ways: at, at, when there is none. Each run is measured once, and
// kept, whatever order it is asked about in. The walk asks about places
// further and further on from each start, so it can ask about one run, then
// one after it, then the first again from the next start: a search that kept
// only the last run it measured measured the first one again from every
// start in it, and two long runs of spaces with an arrow after each cost the
// square of their length (TestFilterCostIsBounded, "arrows far apart").
func (ix *searchIndex) runOf(c *runeClass, slot int32, at int) (int, int) {
	rs := ix.runs[slot]
	n := len(rs) / 2
	i := sort.Search(n, func(k int) bool { return rs[2*k+1] >= at })
	if i < n && rs[2*i] <= at {
		return rs[2*i], rs[2*i+1]
	}
	text := ix.text
	from := at
	for from > 0 {
		r, w := utf8.DecodeLastRuneInString(text[:from])
		if !c.has(r) {
			break
		}
		from -= w
	}
	to := at
	for to < len(text) {
		r, w := decodeRune(text, to)
		if !c.has(r) {
			break
		}
		to += w
	}
	ix.work += to - from
	if to > from {
		rs = slices.Insert(rs, 2*i, from, to)
		ix.runs[slot] = rs
	}
	return from, to
}

// groupState is what a view knows about a group: the places, in order,
// where one of its branches can start, and the first place at or after lo
// where it matches, q (-1 for none), once looked for.
type groupState struct {
	qs     []int32
	cur    int
	lo, q  int
	looked bool
}

func (ix *searchIndex) group(g *group) *groupState {
	st := &ix.groups[g.id]
	if st.qs == nil {
		for _, id := range g.triggers {
			st.qs = append(st.qs, ix.hits(id)...)
		}
		slices.Sort(st.qs)
		st.qs = slices.Compact(st.qs)
		if st.qs == nil {
			st.qs = []int32{}
		}
	}
	return st
}

// matchAfter returns the first place at or after from where what a group
// stands for matches, or -1.
func (ix *searchIndex) matchAfter(g *group, from int) int {
	st := ix.group(g)
	if st.looked && from >= st.lo && (st.q < 0 || from <= st.q) {
		return st.q
	}
	f := getFinder()
	q, _ := f.run(g.prog, ix.text, from, func(x int) int {
		i := st.cur
		for i > 0 && int(st.qs[i-1]) >= x {
			i--
		}
		for i < len(st.qs) && int(st.qs[i]) < x {
			i++
		}
		st.cur = i
		if i < len(st.qs) {
			return int(st.qs[i])
		}
		return -1
	}, true)
	ix.work += f.work
	putFinder(f)
	st.lo, st.q, st.looked = from, q, true
	return q
}

// next returns the first place at or after from where b can start, or -1.
// cur is the caller's cursor for b. It moves forwards as from does, and back
// when from goes back: a machine looks for places to start until a match
// ends, so the next search, from that end, can begin before them.
func (ix *searchIndex) next(b *branch, cur *int, from int) int {
	switch b.kind {
	case delimitedBranch:
		return ix.nextDelimited(b, from)
	case runBranch:
		return ix.nextRun(b, from)
	}
	h := ix.hits(b.trigger)
	i := *cur
	for i > 0 && int(h[i-1]) >= from {
		i--
	}
	for i < len(h) && int(h[i]) < from {
		i++
	}
	*cur = i
	if i < len(h) {
		return int(h[i])
	}
	return -1
}

// nextDelimited finds the next place where rest can start, and the boundary
// before the separators in front of it: the start of the text, or the first
// delimiter among them or just before them, at or after from. The places
// rest can start are in order, and so are the stretches of separators in
// front of them, so the first that has a boundary gives the leftmost start.
func (ix *searchIndex) nextDelimited(b *branch, from int) int {
	text := ix.text
	for at := from; ; {
		q := ix.matchAfter(b.rest, at)
		if q < 0 {
			return -1
		}
		lo := ix.runStart(b.skip, b.slot, q)
		if b.begin && lo == 0 && from == 0 {
			return 0
		}
		x := lo
		if x > 0 {
			_, w := utf8.DecodeLastRuneInString(text[:x])
			x -= w
		}
		x = max(x, from)
		start := x
		for x < q {
			r, w := decodeRune(text, x)
			if b.delims.has(r) {
				break
			}
			x += w
		}
		ix.work += x - start
		if x < q {
			return x
		}
		_, w := decodeRune(text, q)
		at = q + w
	}
}

// nextRun finds the next place where rest can start, and the leftmost start
// at or after from of a run that can end there: within the run of runMore
// characters before it, at least runMin and at most runMax of them back,
// after a runFirst character and the lead. The places rest can start are in
// order, and the earliest start each allows never goes backwards, so the
// first place that allows one gives the leftmost.
func (ix *searchIndex) nextRun(b *branch, from int) int {
	text := ix.text
	for at := from + 1; ; {
		q := ix.matchAfter(b.rest, at)
		if q < 0 {
			return -1
		}
		if best := ix.runEndingAt(b, from, q); best >= 0 {
			return best
		}
		// None of the runs that end at q starts a match, and the next place
		// a run can end is just after the next character a run is made of:
		// what follows the run can match at every place of a run of spaces
		// in front of an "@", and looking from each of them again for where
		// it matches read the rest of the run from each.
		for at = q; at < len(text); {
			r, w := decodeRune(text, at)
			at += w
			if b.runFirst.has(r) || b.runMore.has(r) {
				break
			}
		}
		if at == q {
			return -1
		}
		ix.work += at - q
	}
}

// runEndingAt is the leftmost start at or after from of a run of b that ends
// at q, with its lead, or -1.
func (ix *searchIndex) runEndingAt(b *branch, from, q int) int {
	text := ix.text
	lo := ix.runStart(b.runMore, b.slot, q)
	// p is where the runMore characters start: from runMax of them
	// before q to runMin of them before q, inside the run.
	hi := q
	for range b.runMin {
		if hi <= lo {
			return -1
		}
		_, w := utf8.DecodeLastRuneInString(text[:hi])
		hi -= w
	}
	p := hi
	for k := b.runMin; k < b.runMax && p > lo; k++ {
		_, w := utf8.DecodeLastRuneInString(text[:p])
		p -= w
	}
	ix.work += q - p
	best := -1
	for ; p <= hi; p++ {
		if p == 0 || p < len(text) && !utf8.RuneStart(text[p]) {
			continue
		}
		r, w := utf8.DecodeLastRuneInString(text[:p])
		rp := p - w
		if !b.runFirst.has(r) {
			continue
		}
		if b.hasLeadRune && rp > 0 {
			if x, wx := utf8.DecodeLastRuneInString(text[:rp]); x == b.leadRune && rp-wx >= from && (best < 0 || rp-wx < best) {
				best = rp - wx
			}
		}
		if rp >= from && holds(b.leadOp, text, rp) && (best < 0 || rp < best) {
			best = rp
		}
	}
	return best
}

// run returns the first match of b at or after from, as indexes into the
// text, or nil. cur is b's cursor, as for next.
//
// A delimited or a run branch is only ever found where what follows its
// boundary or its run has been seen to match, so the place next gives is
// where its leftmost match starts. A literal branch's places are only where
// its openings are, so a finder runs it from each in turn to find where its
// first match starts. Either way the machine then runs it from that place
// alone, for the groups.
func (ix *searchIndex) run(b *branch, cur *int, from int) []int {
	s := ix.next(b, cur, from)
	if s < 0 {
		return nil
	}
	if b.kind == literalBranch {
		f := getFinder()
		s, _ = f.run(b, ix.text, s, func(x int) int { return ix.next(b, cur, x) }, false)
		ix.work += f.work
		putFinder(f)
		if s < 0 {
			return nil
		}
	}
	m := getMachine()
	match := m.run(b, ix.text, s)
	ix.work += m.work
	putMachine(m)
	return match
}

// each calls fn with each match of s in v, in order, exactly as
// FindAllStringSubmatchIndex would return them, until fn returns false.
//
// Each branch has a candidate: the next place it can start, or, once it has
// been run from there, the place its next match starts. The leftmost
// candidate is where the pattern's next match can start, and the branches
// whose candidate it is are run from there in order of preference: the
// first whose match starts there gives the pattern's match, as the pattern
// would have preferred it.
func (s *search) each(v *view, fn func(m []int) bool) {
	prepareSearches()
	if s.branches == nil {
		v.work += len(v.text)
		for _, m := range s.regexp().FindAllStringSubmatchIndex(v.text, -1) {
			if !fn(m) {
				return
			}
		}
		return
	}
	s.eachIn(v.searchIndex(s.family), fn)
}

// matchIn reports whether a window search matches text, a stretch of v
// (or of it rewritten), as the pattern's MatchString would, and counts
// what that cost in v's work.
func (s *search) matchIn(v *view, text string) bool {
	return s.findIn(v, text) != nil
}

// findIn returns a window search's first match in text, a stretch of v, as
// the pattern's FindStringSubmatchIndex would, or nil.
func (s *search) findIn(v *view, text string) []int {
	prepareSearches()
	if s.branches == nil {
		v.work += len(text)
		return s.regexp().FindStringSubmatchIndex(text)
	}
	ix := newSearchIndex(text, s.family)
	var first []int
	s.eachIn(ix, func(m []int) bool {
		first = m
		return false
	})
	v.work += ix.work
	return first
}

// eachIn is each, over the text ix has walked.
func (s *search) eachIn(ix *searchIndex, fn func(m []int) bool) {
	n := len(s.branches)
	cand := make([]int, n)
	cur := make([]int, n)
	found := make([][]int, n) // the match at cand, once the branch has run
	for i := range cand {
		cand[i] = -2 // not yet looked for
	}
	for from := 0; ; {
		best := -1
		for i, b := range s.branches {
			if c := cand[i]; c == -2 || c >= 0 && c < from {
				cand[i], found[i] = ix.next(b, &cur[i], from), nil
			}
			if c := cand[i]; c >= 0 && (best < 0 || c < best) {
				best = c
			}
		}
		if best < 0 {
			return
		}
		var m []int
		for i, b := range s.branches {
			if cand[i] != best {
				continue
			}
			if found[i] == nil {
				found[i] = ix.run(b, &cur[i], best)
				if found[i] == nil {
					cand[i] = -1
					continue
				}
				cand[i] = found[i][0]
			}
			if cand[i] == best {
				m = found[i]
				break
			}
		}
		if m == nil {
			continue
		}
		m = pad(m, s.numSubexp)
		if !fn(m) {
			return
		}
		from = m[1]
	}
}

// pad fills out a match to every group of the pattern, as Go's regexp does.
func pad(m []int, numSubexp int) []int {
	for len(m) < 2*(1+numSubexp) {
		m = append(m, -1)
	}
	return m
}

/* Viability ------------------------------------------------------------------- */

// viable is what an instruction can go on to do next: the characters that it,
// or an instruction it reaches without taking a character, can take, and
// whether it can reach the end of the program, a match, without taking one.
// Empty-width assertions are read as holding, so it may say more than the
// instruction can do in a given place, never less.
type viable struct {
	ascii [2]uint64 // the ASCII characters
	other bool      // some character beyond ASCII
	match bool
}

// takes reports whether a thread at the instruction could go on with c next,
// the character at the place it is put on a queue, or endOfText. One that
// cannot dies at the next step, having matched nothing, so the machine and
// the finder do not put it on the queue at all: what they find is the same,
// and the steps they count are fewer.
func (v *viable) takes(c rune) bool {
	switch {
	case v.match:
		return true
	case c < 0:
		return false
	case c < utf8.RuneSelf:
		return v.ascii[c>>6]&(1<<(uint(c)&63)) != 0
	}
	return v.other
}

// viability works out what each instruction of prog can go on to do: what
// it takes itself, and everything an instruction after it without a
// character between can, until nothing changes. It returns their indexes
// into p.viables, of which there are a few thousand.
func (p *planner) viability(prog *syntax.Prog) []uint16 {
	if cap(p.scratch) < len(prog.Inst) {
		p.scratch = make([]viable, len(prog.Inst))
	}
	v := p.scratch[:len(prog.Inst)]
	clear(v)
	for pc := range prog.Inst {
		i := &prog.Inst[pc]
		w := &v[pc]
		switch i.Op {
		case syntax.InstMatch:
			w.match = true
		case syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			w.ascii = [2]uint64{^uint64(0), ^uint64(0)}
			if i.Op == syntax.InstRuneAnyNotNL {
				w.ascii[0] &^= 1 << '\n'
			}
			w.other = true
		case syntax.InstRune1:
			w.add(i.Rune[0], i.Rune[0])
		case syntax.InstRune:
			if len(i.Rune) == 1 || syntax.Flags(i.Arg)&syntax.FoldCase != 0 {
				// One character, or a class matched without regard to
				// case: ask the instruction about every ASCII character,
				// and allow for the others.
				for c := rune(0); c < utf8.RuneSelf; c++ {
					if i.MatchRune(c) {
						w.add(c, c)
					}
				}
				w.other = true
				break
			}
			// The ranges are sorted: those that start in ASCII, and
			// whether the last ends beyond it.
			for k := 0; k+1 < len(i.Rune) && i.Rune[k] < utf8.RuneSelf; k += 2 {
				w.add(i.Rune[k], i.Rune[k+1])
			}
			w.other = w.other || i.Rune[len(i.Rune)-1] >= utf8.RuneSelf
		}
	}
	for changed := true; changed; {
		changed = false
		for pc := len(prog.Inst) - 1; pc >= 0; pc-- {
			i := &prog.Inst[pc]
			switch i.Op {
			case syntax.InstAlt, syntax.InstAltMatch:
				changed = v[pc].merge(&v[i.Out]) || changed
				changed = v[pc].merge(&v[i.Arg]) || changed
			case syntax.InstEmptyWidth, syntax.InstNop, syntax.InstCapture:
				changed = v[pc].merge(&v[i.Out]) || changed
			}
		}
	}
	via := make([]uint16, len(v))
	for pc, w := range v {
		id, ok := p.viableAt[w]
		if !ok {
			if len(p.viables) > math.MaxUint16 {
				panic("filter patterns: more viabilities than an index holds")
			}
			id = uint16(len(p.viables))
			p.viables = append(p.viables, w)
			p.viableAt[w] = id
		}
		via[pc] = id
	}
	return via
}

// add allows the characters lo to hi.
func (v *viable) add(lo, hi rune) {
	if hi >= utf8.RuneSelf {
		v.other = true
	}
	for c := lo; c <= min(hi, utf8.RuneSelf-1); c++ {
		v.ascii[c>>6] |= 1 << (uint(c) & 63)
	}
}

// merge adds what w allows to v, and reports whether that changed v.
func (v *viable) merge(w *viable) bool {
	n := viable{[2]uint64{v.ascii[0] | w.ascii[0], v.ascii[1] | w.ascii[1]}, v.other || w.other, v.match || w.match}
	if n == *v {
		return false
	}
	*v = n
	return true
}

// next is the character an instruction put on a queue at a place would take
// next: the one after the place, in the context cond gives it.
func (cond lazyFlag) next() rune { return rune(int32(uint32(cond))) }

/* The machine ------------------------------------------------------------------- */

// machine runs a branch's program from one place as Go's regexp runs a
// pattern (regexp/exec.go, which this follows step for step, but for not
// putting a thread where it cannot take the next character): a thread per
// place in the program, in order of preference, at most one at each
// instruction, each with the groups as far as it has come, and the first
// thread to reach the end cutting off every thread after it.
type machine struct {
	prog     *syntax.Prog
	via      []uint16
	q0, q1   queue
	pool     []*thread
	matched  bool
	matchcap []int
	ncap     int
	// work counts the threads run at each character, plus one for each.
	work int
}

type queue struct {
	sparse []uint32
	dense  []queueEntry
}

type queueEntry struct {
	pc uint32
	t  *thread
}

type thread struct {
	inst *syntax.Inst
	cap  []int
}

var machines sync.Pool

func getMachine() *machine {
	if m, ok := machines.Get().(*machine); ok {
		m.work = 0
		return m
	}
	n := searches.maxInst
	return &machine{
		q0:       queue{sparse: make([]uint32, n), dense: make([]queueEntry, 0, n)},
		q1:       queue{sparse: make([]uint32, n), dense: make([]queueEntry, 0, n)},
		matchcap: make([]int, searches.maxCap),
	}
}

func putMachine(m *machine) {
	m.prog, m.via = nil, nil
	machines.Put(m)
}

const endOfText rune = -1

// stepRune is the character at text[pos] and its width, or endOfText.
func stepRune(text string, pos int) (rune, int) {
	if pos < len(text) {
		if c := text[pos]; c < utf8.RuneSelf {
			return rune(c), 1
		}
		return utf8.DecodeRuneInString(text[pos:])
	}
	return endOfText, 0
}

// contextAt is the empty-width context at pos: the characters either side.
func contextAt(text string, pos int) lazyFlag {
	r1, r2 := endOfText, endOfText
	if uint(pos-1) < uint(len(text)) {
		r1 = rune(text[pos-1])
		if r1 >= utf8.RuneSelf {
			r1, _ = utf8.DecodeLastRuneInString(text[:pos])
		}
	}
	if uint(pos) < uint(len(text)) {
		r2 = rune(text[pos])
		if r2 >= utf8.RuneSelf {
			r2, _ = utf8.DecodeRuneInString(text[pos:])
		}
	}
	return newLazyFlag(r1, r2)
}

// lazyFlag is the pair of characters an empty-width assertion looks at.
type lazyFlag uint64

func newLazyFlag(r1, r2 rune) lazyFlag {
	return lazyFlag(uint64(r1)<<32 | uint64(uint32(r2)))
}

func (f lazyFlag) match(op syntax.EmptyOp) bool {
	if op == 0 {
		return true
	}
	r1 := rune(f >> 32)
	if op&syntax.EmptyBeginLine != 0 {
		if r1 != '\n' && r1 >= 0 {
			return false
		}
		op &^= syntax.EmptyBeginLine
	}
	if op&syntax.EmptyBeginText != 0 {
		if r1 >= 0 {
			return false
		}
		op &^= syntax.EmptyBeginText
	}
	if op == 0 {
		return true
	}
	r2 := rune(f)
	if op&syntax.EmptyEndLine != 0 {
		if r2 != '\n' && r2 >= 0 {
			return false
		}
		op &^= syntax.EmptyEndLine
	}
	if op&syntax.EmptyEndText != 0 {
		if r2 >= 0 {
			return false
		}
		op &^= syntax.EmptyEndText
	}
	if op == 0 {
		return true
	}
	if syntax.IsWordChar(r1) != syntax.IsWordChar(r2) {
		op &^= syntax.EmptyWordBoundary
	} else {
		op &^= syntax.EmptyNoWordBoundary
	}
	return op == 0
}

// run returns the match of b's program in text that starts at start, the
// one the program prefers there, or nil when none does.
func (m *machine) run(b *branch, text string, start int) []int {
	if b.cond == ^syntax.EmptyOp(0) {
		return nil
	}
	m.prog, m.ncap, m.via = b.prog, b.prog.NumCap, b.via
	m.matched = false
	m.matchcap = m.matchcap[:m.ncap]
	for i := range m.matchcap {
		m.matchcap[i] = -1
	}
	runq, nextq := &m.q0, &m.q1
	pos := start
	r, width := stepRune(text, pos)
	r1, width1 := endOfText, 0
	if r != endOfText {
		r1, width1 = stepRune(text, pos+width)
	}
	flag := contextAt(text, pos)
	m.matchcap[0] = pos
	m.add(runq, uint32(m.prog.Start), pos, m.matchcap, &flag, nil)
	for len(runq.dense) > 0 {
		m.work += 1 + len(runq.dense)
		flag = newLazyFlag(r, r1)
		m.step(runq, nextq, pos, pos+width, r, &flag)
		if width == 0 {
			break
		}
		pos += width
		r, width = r1, width1
		if r != endOfText {
			r1, width1 = stepRune(text, pos+width)
		}
		runq, nextq = nextq, runq
	}
	m.clear(nextq)
	m.clear(runq)
	if !m.matched {
		return nil
	}
	return slices.Clone(m.matchcap)
}

func (m *machine) alloc(i *syntax.Inst) *thread {
	var t *thread
	if n := len(m.pool); n > 0 {
		t = m.pool[n-1]
		m.pool = m.pool[:n-1]
	} else {
		t = &thread{cap: make([]int, searches.maxCap)}
	}
	t.cap = t.cap[:m.ncap]
	t.inst = i
	return t
}

func (m *machine) clear(q *queue) {
	for _, d := range q.dense {
		if d.t != nil {
			m.pool = append(m.pool, d.t)
		}
	}
	q.dense = q.dense[:0]
}

// step runs each thread on runq over the character c, which starts at pos
// and ends at nextPos, and puts the threads that go on onto nextq.
func (m *machine) step(runq, nextq *queue, pos, nextPos int, c rune, nextCond *lazyFlag) {
	for j := 0; j < len(runq.dense); j++ {
		d := &runq.dense[j]
		t := d.t
		if t == nil {
			continue
		}
		i := t.inst
		add := false
		switch i.Op {
		default:
			panic("bad inst")
		case syntax.InstMatch:
			t.cap[1] = pos
			copy(m.matchcap, t.cap)
			// The first thread to match cuts off the threads after it.
			for _, d := range runq.dense[j+1:] {
				if d.t != nil {
					m.pool = append(m.pool, d.t)
				}
			}
			runq.dense = runq.dense[:0]
			m.matched = true
		case syntax.InstRune:
			add = i.MatchRune(c)
		case syntax.InstRune1:
			add = c == i.Rune[0]
		case syntax.InstRuneAny:
			add = true
		case syntax.InstRuneAnyNotNL:
			add = c != '\n'
		}
		if add {
			t = m.add(nextq, i.Out, nextPos, t.cap, nextCond, t)
		}
		if t != nil {
			m.pool = append(m.pool, t)
		}
	}
	runq.dense = runq.dense[:0]
}

// add puts a thread for pc on q, unless q has one, and threads for every
// instruction reachable from pc by empty-width steps that cond allows.
func (m *machine) add(q *queue, pc uint32, pos int, cap []int, cond *lazyFlag, t *thread) *thread {
Again:
	if pc == 0 || !searches.viables[m.via[pc]].takes(cond.next()) {
		return t
	}
	if j := q.sparse[pc]; j < uint32(len(q.dense)) && q.dense[j].pc == pc {
		return t
	}
	j := len(q.dense)
	q.dense = q.dense[:j+1]
	d := &q.dense[j]
	d.t = nil
	d.pc = pc
	q.sparse[pc] = uint32(j)

	i := &m.prog.Inst[pc]
	switch i.Op {
	default:
		panic("unhandled")
	case syntax.InstFail:
	case syntax.InstAlt, syntax.InstAltMatch:
		t = m.add(q, i.Out, pos, cap, cond, t)
		pc = i.Arg
		goto Again
	case syntax.InstEmptyWidth:
		if cond.match(syntax.EmptyOp(i.Arg)) {
			pc = i.Out
			goto Again
		}
	case syntax.InstNop:
		pc = i.Out
		goto Again
	case syntax.InstCapture:
		if int(i.Arg) < len(cap) {
			opos := cap[i.Arg]
			cap[i.Arg] = pos
			m.add(q, i.Out, pos, cap, cond, nil)
			cap[i.Arg] = opos
		} else {
			pc = i.Out
			goto Again
		}
	case syntax.InstMatch, syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
		if t == nil {
			t = m.alloc(i)
		} else {
			t.inst = i
		}
		if len(cap) > 0 && &t.cap[0] != &cap[0] {
			copy(t.cap, cap)
		}
		d.t = t
		t = nil
	}
	return t
}

/* The finder ------------------------------------------------------------------ */

// finder runs a branch's program as machine does, but from many places and
// keeping only where each thread started: it finds where the first match
// starts and ends, not its groups, which is all a search needs to know
// until it has a match.
//
// Go's regexp starts a thread at every character of the text; a finder
// starts one only where the branch can start. That finds the same match:
// a thread started anywhere else never matches, and, since two threads at
// one instruction go on alike, it never takes the place of a thread that
// would. Between the places, when no thread is left, the finder goes
// straight to the next one.
//
// The machine, run from where the finder's match starts, finds that match
// again, groups and all: a thread from an earlier place that took an
// instruction a thread from there wanted could not have gone on to match,
// or the match would have started earlier, and a thread from a later place
// never comes before it.
type finder struct {
	prog   *syntax.Prog
	via    []uint16
	q0, q1 finderQueue
	// work counts the threads run at each character, plus one for each.
	work int
}

type finderQueue struct {
	sparse []uint32
	dense  []finderEntry
}

// finderEntry is an instruction a thread has reached, and where the thread
// started, or -1 when no thread waits there (it only passed through).
type finderEntry struct {
	pc    uint32
	start int32
}

var finders sync.Pool

func getFinder() *finder {
	if f, ok := finders.Get().(*finder); ok {
		f.work = 0
		return f
	}
	n := searches.maxInst
	return &finder{
		q0: finderQueue{sparse: make([]uint32, n), dense: make([]finderEntry, 0, n)},
		q1: finderQueue{sparse: make([]uint32, n), dense: make([]finderEntry, 0, n)},
	}
}

func putFinder(f *finder) {
	f.prog, f.via = nil, nil
	finders.Put(f)
}

// run returns where the first match of b's program in text starts and
// ends, starting at one of the places next gives, from from on: next(x) is
// the first place at or after x where b can start, or -1. It returns -1, -1
// when there is none. With startOnly, only where the match starts is
// wanted: the finder stops once it has a match and no thread that started
// before it is left, and the end it returns is not the match's.
func (f *finder) run(b *branch, text string, from int, next func(int) int, startOnly bool) (int, int) {
	mstart, mend := -1, -1
	if b.cond == ^syntax.EmptyOp(0) {
		return mstart, mend
	}
	f.prog, f.via = b.prog, b.via
	start := next(from)
	if start < 0 {
		return mstart, mend
	}
	runq, nextq := &f.q0, &f.q1
	pos := start
	r, width := stepRune(text, pos)
	r1, width1 := endOfText, 0
	if r != endOfText {
		r1, width1 = stepRune(text, pos+width)
	}
	flag := contextAt(text, pos)
	for {
		for start >= 0 && start < pos {
			start = next(pos)
		}
		if len(runq.dense) == 0 {
			if mstart >= 0 || start < 0 {
				break
			}
			if start > pos {
				pos = start
				r, width = stepRune(text, pos)
				r1, width1 = endOfText, 0
				if r != endOfText {
					r1, width1 = stepRune(text, pos+width)
				}
				flag = contextAt(text, pos)
			}
		}
		if mstart < 0 && pos == start {
			f.add(runq, uint32(f.prog.Start), int32(pos), &flag)
			start = next(pos + max(width, 1))
		}
		f.work += 1 + len(runq.dense)
		flag = newLazyFlag(r, r1)
		for _, d := range runq.dense {
			if d.start < 0 {
				continue
			}
			i := &f.prog.Inst[d.pc]
			ok := false
			switch i.Op {
			default:
				panic("bad inst")
			case syntax.InstMatch:
				mstart, mend = int(d.start), pos
			case syntax.InstRune:
				ok = i.MatchRune(r)
			case syntax.InstRune1:
				ok = r == i.Rune[0]
			case syntax.InstRuneAny:
				ok = true
			case syntax.InstRuneAnyNotNL:
				ok = r != '\n'
			}
			if i.Op == syntax.InstMatch {
				// The first thread to match cuts off the threads after it.
				break
			}
			if ok {
				f.add(nextq, i.Out, d.start, &flag)
			}
		}
		runq.dense = runq.dense[:0]
		if width == 0 || startOnly && mstart >= 0 && !nextq.before(int32(mstart)) {
			break
		}
		pos += width
		r, width = r1, width1
		if r != endOfText {
			r1, width1 = stepRune(text, pos+width)
		}
		runq, nextq = nextq, runq
	}
	runq.dense = runq.dense[:0]
	nextq.dense = nextq.dense[:0]
	return mstart, mend
}

// before reports whether a thread on q started before at.
func (q *finderQueue) before(at int32) bool {
	for _, d := range q.dense {
		if d.start >= 0 && d.start < at {
			return true
		}
	}
	return false
}

// add puts a thread for pc on q, unless q has one, and threads for every
// instruction reachable from pc by empty-width steps that cond allows, as
// machine.add does.
func (f *finder) add(q *finderQueue, pc uint32, start int32, cond *lazyFlag) {
Again:
	if pc == 0 || !searches.viables[f.via[pc]].takes(cond.next()) {
		return
	}
	if j := q.sparse[pc]; j < uint32(len(q.dense)) && q.dense[j].pc == pc {
		return
	}
	j := len(q.dense)
	q.dense = q.dense[:j+1]
	d := &q.dense[j]
	d.pc, d.start = pc, -1
	q.sparse[pc] = uint32(j)

	i := &f.prog.Inst[pc]
	switch i.Op {
	default:
		panic("unhandled")
	case syntax.InstFail:
	case syntax.InstAlt, syntax.InstAltMatch:
		f.add(q, i.Out, start, cond)
		pc = i.Arg
		goto Again
	case syntax.InstEmptyWidth:
		if cond.match(syntax.EmptyOp(i.Arg)) {
			pc = i.Out
			goto Again
		}
	case syntax.InstNop, syntax.InstCapture:
		pc = i.Out
		goto Again
	case syntax.InstMatch, syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
		d.start = start
	}
}

// planString describes the plan, for tests.
func (s *search) planString() string {
	if s.branches == nil {
		return "whole"
	}
	var parts []string
	for _, b := range s.branches {
		parts = append(parts, [...]string{"literal", "delimited", "run"}[b.kind])
	}
	return strings.Join(parts, " ")
}
