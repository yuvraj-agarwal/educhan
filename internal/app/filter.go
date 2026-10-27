package app

import (
	"math/bits"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// The content filter: build plan 2.9 (content rules), the hold half of 2.11
// (threats) and 2.12 (self-harm).
//
// Every post is read here before anything about it is stored: its body, its
// subject, and the part of its name that is shown — never the tripcode secret
// after "#", which the caller does not pass in. It runs in-process and offline.
// Nothing is sent anywhere, and nothing it reads or matches is logged or kept:
// what a decision leaves is the status on the request's log line (422, 202),
// a count in the refusal budget for a refusal, and held_reason on a held post.
//
// Every rule has exactly one outcome:
//
//   - block: the post is refused with 422, naming the field and the text that
//     tripped it, so the poster can take it out and post again. Contact
//     details (phone numbers, email addresses, links to messaging and social
//     apps, asking to talk on one) and ID numbers.
//   - support: crisis language in the poster's own voice. The post is refused
//     once with a kind sentence, for a client to show with where to get help;
//     sent again with support_seen it goes on to the rules after this one. It
//     never blocks.
//   - hold: the post is stored with held_at set and appears nowhere until a
//     moderator approves it. Threats with a place or a time, self-harm
//     encouragement or method, and grooming cues.
//
// Blocks come first, so a poster fixes what they can before anything else;
// then support, unless the poster has seen it; then holds; then publication.
// The one exception is contact details beside a question that places the
// reader, which are held as grooming rather than sent back (placesTheReader).
//
// Be honest about what this is. These are word and number patterns. They are
// easy to get round — a number in words other than one digit at a time, a
// username with no app named beside it, contact details written inside [math], which is skipped
// because TeX has a use for every character — and they catch posts that are
// fine, which is why a block says exactly what it matched and a hold waits
// for a person instead of deleting anything. They raise the cost of moving a
// child off the site; they do not prevent it.
//
// The cost is bounded and linear in the length of the post. Each field is
// normalised once into two views; every pattern is a Go regexp, run only from
// the places a match can start by a copy of Go's own machine, which takes
// linear time and never backtracks (filtersearch.go); the number scanner is
// one pass, and looks for its cues in a few words either side of a number;
// the checks a match is put to read a few words either side of it;
// and the checks that pair matches up (a threat with a time, an age question
// with a contact cue) work on the sorted match lists rather than rescanning.

// contentOutcome is what the filter decided about a post.
type contentOutcome int

const (
	contentPublish contentOutcome = iota
	contentBlock
	contentSupport
	contentHold
)

// Block rules, as the API names them.
const (
	ruleContact  = "contact"
	ruleIDNumber = "id_number"
)

// Hold reasons, as posts.held_reason stores them. The migration's CHECK lists
// the same values.
const (
	holdThreat   = "threat"
	holdGrooming = "grooming"
	holdSelfHarm = "self_harm"
)

// The fields a block can name.
const (
	fieldSubject = "subject"
	fieldName    = "name"
	fieldBody    = "body"
)

// maxMatchLen bounds the text a block quotes back to the poster.
const maxMatchLen = 80

// supportMessage is the whole of what a support refusal says. Where to get
// help is the client's to show beside it (HELPLINES_HTML), which keeps the
// list in one place.
const supportMessage = "It sounds like things might be really hard for you right now, and you don't have to go through it alone."

// contentInput is the text of a post as it will be stored and shown.
type contentInput struct {
	Subject string
	// Name is the displayed part of the name field only: never the tripcode
	// secret, which is not text anyone will read.
	Name string
	Body string
}

// contentVerdict is the filter's decision.
type contentVerdict struct {
	Outcome contentOutcome
	// Rule is the block rule (ruleContact or ruleIDNumber).
	Rule string
	// Field and Match say where a block was found and what it was, as the
	// poster wrote it. Occurrence says which copy of Match in the field the
	// block is about (matchOccurrence).
	Field      string
	Match      string
	Occurrence int
	// Message is the sentence the poster reads, for a block or support.
	Message string
	// Hold is the reason a held post is stored with.
	Hold string
}

// findingKind is what a block found, which decides the sentence the poster
// reads.
type findingKind int

const (
	kindPhone findingKind = iota
	kindEmail
	kindLink
	kindHandle
	kindAsk
	kindAadhaar
	kindSSN
	kindNI
)

func (k findingKind) rule() string {
	switch k {
	case kindAadhaar, kindSSN, kindNI:
		return ruleIDNumber
	}
	return ruleContact
}

// blockMessage is one sentence a 13-year-old can act on: what to take out,
// from where, and why.
func blockMessage(k findingKind, field string) string {
	where := "your post"
	switch field {
	case fieldSubject:
		where = "the subject"
	case fieldName:
		where = "the Name field"
	}
	const contact = ", because contact details aren't allowed on educhan."
	const elsewhere = ", because educhan doesn't allow ways to talk somewhere else."
	const ids = ", because ID numbers aren't allowed on educhan."
	switch k {
	case kindPhone:
		return "Please take the phone number out of " + where + contact
	case kindEmail:
		return "Please take the email address out of " + where + contact
	case kindLink:
		return "Please take the link to another app out of " + where + elsewhere
	case kindHandle:
		return "Please take the username for another app out of " + where + elsewhere
	case kindAsk:
		return "Please take out the part of " + where + " that asks people to talk on another app" + elsewhere
	case kindAadhaar:
		return "Please take the Aadhaar number out of " + where + ids
	case kindSSN:
		return "Please take the Social Security number out of " + where + ids
	case kindNI:
		return "Please take the National Insurance number out of " + where + ids
	}
	return "Please take the contact details out of " + where + contact
}

// checkContent applies every rule to a post. supportSeen is the poster saying
// they have been shown the support message for this post, which lets it go on
// to the holds.
func checkContent(in contentInput, supportSeen bool) contentVerdict {
	v, _ := checkContentCost(in, supportSeen)
	return v
}

// checkContentCost is checkContent, and what searching the post cost, in the
// steps searchIndex.work counts.
func checkContentCost(in contentInput, supportSeen bool) (contentVerdict, int) {
	fields := make([]*scannedField, 0, 3)
	cost := func() int {
		n := 0
		for _, f := range fields {
			n += f.work()
		}
		return n
	}
	// Form order, so a poster is told about the first problem they would
	// come to.
	if in.Subject != "" {
		fields = append(fields, scanField(fieldSubject, in.Subject, nil))
	}
	if in.Name != "" {
		fields = append(fields, scanField(fieldName, in.Name, nil))
	}
	if in.Body != "" {
		fields = append(fields, scanField(fieldBody, in.Body, markupReadings(in.Body)))
	}

	for _, f := range fields {
		f.nameShown, f.subjectShown = in.Name != "", in.Subject != ""
	}
	// Contact details in a post that also asks where the reader is, how old
	// they are or which school they go to are the private-contact half of a
	// grooming cue. Sending the post back would say which half to take out,
	// so it is held for a moderator instead.
	places, placed := false, false
	contactCue := false
	for _, f := range fields {
		if found, ok := f.firstBlock(); ok {
			if found.kind.rule() == ruleContact {
				if !placed {
					places, placed = placesTheReader(fields), true
				}
				if places {
					contactCue = true
					continue
				}
			}
			match, searched := quoteSource(f.src, found.from, found.to)
			return contentVerdict{
				Outcome:    contentBlock,
				Rule:       found.kind.rule(),
				Field:      f.field,
				Match:      match,
				Occurrence: matchOccurrence(f.src, found.from, searched),
				Message:    blockMessage(found.kind, f.field),
			}, cost()
		}
	}
	if !supportSeen {
		for _, f := range fields {
			if f.anyMatch(supportRules, f.supportCheck) || f.asksForAMethod() {
				return contentVerdict{Outcome: contentSupport, Message: supportMessage}, cost()
			}
		}
	}
	if reason := holdReason(fields, contactCue); reason != "" {
		return contentVerdict{Outcome: contentHold, Hold: reason}, cost()
	}
	return contentVerdict{Outcome: contentPublish}, cost()
}

// placesTheReader reports a question in the post that places the reader: an
// age, a school year, a school or where they live.
func placesTheReader(fields []*scannedField) bool {
	for _, f := range fields {
		if f.anyMatch(ageQuestionRules, ageCheck) || f.anyMatch(aslRules, aslCheck) {
			return true
		}
	}
	return false
}

// holdReason picks the first hold that applies, most urgent first: a threat
// is what a moderator most needs to see quickly. contactCue says the post
// holds contact details, which are a private-contact cue.
func holdReason(fields []*scannedField, contactCue bool) string {
	if threatens(fields) {
		return holdThreat
	}
	for _, f := range fields {
		if f.anyMatch(selfHarmRules, selfHarmCheck) || f.anyMatch(selfHarmUrgeRules, urgeCheck) || f.asksForAMethod() ||
			f.goads() {
			return holdSelfHarm
		}
	}
	// A grooming cue is the pair: an age, school or location question and a
	// private-contact or meet-up cue, anywhere in the same post.
	cues := contactCue
	for _, f := range fields {
		cues = cues || f.anyMatch(privateCueRules, cueCheck)
	}
	if cues && placesTheReader(fields) {
		return holdGrooming
	}
	// A meeting in the open, with a group or with an errand is still a cue
	// beside the reader's age asked: "how old are you? meet me at the
	// library".
	if !cues && asksTheAge(fields) {
		for _, f := range fields {
			if f.anyMatch(privateCueRules, cueBesideAnAge) {
				return holdGrooming
			}
		}
	}
	return ""
}

// asksTheAge reports a question in the post about the reader's age itself,
// not their year, school or place: "how old are you", "what's your age", "u
// 14?", "any 14yo girls".
func asksTheAge(fields []*scannedField) bool {
	for _, f := range fields {
		if f.anyMatch(ageQuestionRules, ageAsked) {
			return true
		}
	}
	return false
}

// ageAsked is ageCheck for a match that names an age: "old", "age" or a
// number that is not a school year ("grade 11", "9th").
func ageAsked(v *view, m []int) bool {
	ws := wordsIn(v.text[m[0]:m[1]])
	for i, w := range ws {
		if is(w, "old") || is(w, "age") || isASCIIDigit(w[0]) && !ordinalWord.MatchString(w) &&
			!(i > 0 && schoolYearNouns[ws[i-1]]) {
			return ageCheck(v, m)
		}
	}
	return false
}

// quoteSource is the text a block quotes back: the poster's own, from the
// source string, trimmed, and cut to maxMatchLen characters. A cut quote ends
// in "…", so the page knows to select from where it starts to the end of the
// run it finds, not just the characters quoted. searched is the text the page
// looks for: the quote, without the "…" when it was cut.
func quoteSource(src string, from, to int) (quote, searched string) {
	q := strings.TrimSpace(src[from:to])
	if utf8.RuneCountInString(q) <= maxMatchLen {
		return q, q
	}
	cut := strings.TrimRight(truncateRunes(q, maxMatchLen-1), " \t\r\n")
	return cut + "…", cut
}

// jsSpaceRun is a run of what \s matches in the page's JavaScript, which is
// more than Go's \s matches; isJSSpace is the same set.
const jsSpaceRun = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+`

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// matchOccurrence says which copy of a block's quote the block is about, so
// the page selects that one (pointAtMatch) and not an earlier copy the rules
// allowed: a number in [math], say, before the same number after "call". It
// counts the places the page's own search finds searched before the copy at
// from: the text's words in order with any run of whitespace between them,
// each search starting one character on from the last. The page's field
// differs from src only in what normalizeBody tidies, which is whitespace at
// the ends of lines and of the post, and in a name's tripcode secret, which
// comes after the displayed part; neither adds or takes away a copy before
// the one the block is about.
func matchOccurrence(src string, from int, searched string) int {
	words := strings.FieldsFunc(searched, isJSSpace)
	if len(words) == 0 {
		return 0
	}
	for i, word := range words {
		words[i] = regexp.QuoteMeta(word)
	}
	search, err := regexp.Compile(strings.Join(words, jsSpaceRun))
	if err != nil {
		return 0
	}
	start := len(src) - len(strings.TrimLeftFunc(src[from:], unicode.IsSpace))
	n := 0
	for at := 0; at < start; {
		loc := search.FindStringIndex(src[at:])
		if loc == nil || at+loc[0] >= start {
			break
		}
		n++
		_, size := utf8.DecodeRuneInString(src[at+loc[0]:])
		at += loc[0] + size
	}
	return n
}

// reading says what a stretch of the source reads as in the plain view, in
// place of its own characters.
type reading struct {
	from, to int
	as       string
}

// markupReadings lists the markup tags of a body and what each reads as,
// following markupBlocks' pairing so that what the filter reads is what the
// reader sees.
//
// A [math] or [eqn] block, tags included, reads as one blank: the filter skips
// maths, because TeX has a use for every character and long numbers in
// equations are the commonest false positive a phone rule has. A [code] tag
// reads as a line break, since a code block is set apart from the text around
// it; the code itself is read like any other text. A [spoiler] tag reads as
// nothing, because a spoiler is drawn inline and the reader who opens it sees
// the text on either side run on: "98765[spoiler]43210[/spoiler]" is one
// number, "sn[spoiler]ap[/spoiler]" is "snap", and "madeup[spoiler]2009[/spoiler]"
// is "madeup2009". A number hidden in a spoiler straight after a word ("call
// me[spoiler]98765…") still stands on its own, because firstNumber reads the
// place where a tag was as the edge of a word.
func markupReadings(body string) []reading {
	var out []reading
	tag := func(from, to int, as string) {
		if to > from {
			out = append(out, reading{from: from, to: to, as: as})
		}
	}
	for _, blk := range markupBlocks(body) {
		switch blk.kind {
		case blockMath, blockEqn:
			tag(blk.start, blk.end, " ")
		case blockCode:
			tag(blk.start, blk.inner[0], "\n")
			tag(blk.inner[1], blk.end, "\n")
		case blockSpoiler:
			tag(blk.start, blk.inner[0], "")
			tag(blk.inner[1], blk.end, "")
		}
	}
	slices.SortFunc(out, func(a, b reading) int { return a.from - b.from })
	return out
}

/* Normalising ---------------------------------------------------------------- */

// view is one normalised rendering of a field, with every byte mapped back to
// the source character it came from, so that a match is reported as the
// poster wrote it.
type view struct {
	text  string
	start []int // start[i]: where the source character behind text[i] starts
	end   []int // end[i]: where it ends
	// index is what searching the view has found, per family of searches
	// (filtersearch.go); work counts what reading it cost besides: the
	// searches run over a few words of it (the number scanner's cues among
	// them), and the checks matches are put to.
	index [numFamilies]*searchIndex
	work  int
	// link caches where the first reply link (">>123") starts, plus one:
	// 0 when not yet looked for, -1 when there is none. lineAt and line
	// cache lineStart: the line holding offset lineAt starts at line.
	link         int
	lineAt, line int
	// runLine and runFirst cache greentextRun: the run of greentext lines
	// holding the line that starts at runLine starts at runFirst (both 0
	// when not yet asked, which is the answer for the first line).
	runLine, runFirst int
	// loose is set for the loose view, which a window search is read on in
	// the loose form of its pattern.
	loose bool
}

// span maps text[a:b] back to the source.
func (v *view) span(a, b int) (int, int) { return v.start[a], v.end[b-1] }

// apart reports that the view left something of the source out between
// text[i-1] and text[i]: a markup tag that reads as nothing, or an invisible
// character.
func (v *view) apart(i int) bool { return i > 0 && i < len(v.text) && v.end[i-1] < v.start[i] }

type viewBuilder struct {
	buf        []byte
	start, end []int
}

func newViewBuilder(n int) *viewBuilder {
	return &viewBuilder{buf: make([]byte, 0, n), start: make([]int, 0, n), end: make([]int, 0, n)}
}

func (b *viewBuilder) add(r rune, from, to int) {
	n := len(b.buf)
	b.buf = utf8.AppendRune(b.buf, r)
	for i := n; i < len(b.buf); i++ {
		b.start = append(b.start, from)
		b.end = append(b.end, to)
	}
}

func (b *viewBuilder) view() view { return view{text: string(b.buf), start: b.start, end: b.end} }

// scannedField is one field of a post with its two views.
//
// plain is the text as a reader sees it, made comparable: compatibility forms
// folded (NFKC's mapping, so full-width digits and styled letters are plain
// ones), lower case, accents, keycaps and other combining marks dropped, zero-width
// and other invisible characters removed, every script's digits read as
// 0-9, every dash as "-", and the commonest Cyrillic and Greek lookalikes read
// as the Latin letters they imitate.
//
// loose is plain read the way an evader writes: letters spaced or dotted out
// ("s n a p", "s.n.a.p") closed up, leetspeak beside letters read as letters
// (0 o, 1 i, 3 e, 4 a, 5 s, 7 t, @ a, $ s), and l read as i, so that 1 can
// stand for either. Patterns are compiled for it through canon, which makes
// the same l-to-i change in them.
type scannedField struct {
	field string
	src   string
	plain view
	loose view
	// looseOnly says the loose view is the plain one with only l read as i,
	// which is true of most posts. A rule's plain form then finds nothing its
	// loose form does not — canon changes letters to letters, which every
	// pattern treats alike — so the plain view is not searched again.
	looseOnly bool
	// offset is where the field starts when the post's fields are read as one
	// text, in readingPositions: the lengths of the fields before it, plus
	// one for the gap after each. threatens sets it.
	offset int
	// reading caches readingPositions, and words the plain view's words.
	reading []int32
	words   *wordReading
	// method caches asksForAMethod, which both the support step and the
	// holds ask: 0 not yet known, 1 no, 2 yes.
	method int8
	// nameShown and subjectShown say the post has a name or a subject, for
	// a handle given as one of them ("my insta is my name").
	nameShown, subjectShown bool
	// frameBefore and frameAfter hold the words appFrame reads round an
	// app's name, kept from one name to the next so a post that names an
	// app in every other word does not build them anew each time.
	frameBefore, frameAfter []aWord
	frameNext               []string
}

// asksForAMethod reports a selfHarmMethodRules match.
func (f *scannedField) asksForAMethod() bool {
	if f.method == 0 {
		f.method = 1
		if f.anyMatch(selfHarmMethodRules, methodCheck) || f.anyMatch(pillQuestionRules, pillQuestionCheck) {
			f.method = 2
		}
	}
	return f.method == 2
}

func scanField(field, src string, readings []reading) *scannedField {
	f := &scannedField{field: field, src: src}
	f.plain = plainView(src, readings)
	f.loose = looseView(&f.plain, src)
	f.looseOnly = f.loose.text == canon(f.plain.text)
	return f
}

// plainView builds the plain view of src. readings, sorted and apart, are the
// stretches that read as something other than their characters: markup tags,
// and maths, whose blank keeps the words either side of it apart.
func plainView(src string, readings []reading) view {
	b := newViewBuilder(len(src))
	// What a character reads as is worked out once for each post: a post of
	// one character over and over would otherwise decompose it every time.
	var folds [64]foldMemo
	k := 0
	for i := 0; i < len(src); {
		for k < len(readings) && readings[k].to <= i {
			k++
		}
		if k < len(readings) && readings[k].from <= i {
			rd := readings[k]
			for _, r := range rd.as {
				b.add(r, rd.from, rd.to)
			}
			i = rd.to
			continue
		}
		r, size := utf8.DecodeRuneInString(src[i:])
		from, to := i, i+size
		if r < utf8.RuneSelf {
			foldRune(r, func(out rune) { b.add(out, from, to) })
		} else {
			for _, out := range folds[uint32(r)*2654435761>>26].fold(r) {
				b.add(out, from, to)
			}
		}
		i = to
	}
	return b.view()
}

// foldMemo remembers what foldRune made of one character.
type foldMemo struct {
	r   rune
	n   int
	out [maxFoldRunes]rune
}

func (m *foldMemo) fold(r rune) []rune {
	if m.n > 0 && m.r == r {
		return m.out[:m.n-1]
	}
	var out []rune
	foldRune(r, func(d rune) { out = append(out, d) })
	if len(out) <= maxFoldRunes {
		m.r, m.n = r, len(out)+1
		copy(m.out[:], out)
	}
	return out
}

// foldRune emits what one source character reads as in the plain view: zero,
// one, or (for a compatibility form such as a ligature) several characters.
func foldRune(r rune, emit func(rune)) {
	if r < utf8.RuneSelf {
		switch {
		case 'A' <= r && r <= 'Z':
			r += 'a' - 'A'
		case r == '\t' || r == '\r':
			r = ' '
		case r == '`':
			r = '\''
		}
		emit(r)
		return
	}
	if invisibleRune(r) {
		return
	}
	if fancy, ok := fancyRune(r); ok {
		for _, d := range fancy {
			emit(d)
		}
		return
	}
	folded := norm.NFKD.String(string(r))
	if utf8.RuneCountInString(folded) > maxFoldRunes {
		// A whole phrase in one character, such as the Arabic ligature
		// U+FDFA, which folds to eighteen: no way of writing a digit or a
		// letter of contact details needs that many, and reading them all
		// would let a post cost the filter many times its length.
		emit(unicode.ToLower(r))
		return
	}
	for _, d := range folded {
		switch {
		case unicode.In(d, unicode.Mn, unicode.Me) || invisibleRune(d):
			// Me is the enclosing marks: the keycap of 9️⃣ and the circle of
			// 9⃝, which leave the digit inside them as it was.
			continue
		case unicode.IsDigit(d):
			d = '0' + decimalValue(d)
		case unicode.Is(unicode.Pd, d) || d == '−':
			d = '-'
		case unicode.IsSpace(d) || d == '⠀':
			d = ' '
		case d == '‘' || d == '’' || d == 'ʼ' || d == '′':
			d = '\''
		default:
			d = unicode.ToLower(d)
			if c, ok := lookalikes[d]; ok {
				d = c
			}
		}
		emit(d)
	}
}

// maxFoldRunes is the most characters one source character is read as. Every
// compatibility form of digits or Latin letters folds to no more: "㎉" is
// "kcal", "ﬃ" is "ffi".
const maxFoldRunes = 4

// invisibleRune reports characters that render as nothing themselves: format
// characters (zero-width spaces and joiners, the soft hyphen, tags), the
// other default-ignorable code points (Hangul fillers and the like) and
// variation selectors. The bidirectional controls are format characters too,
// but they reorder what is around them on screen, so they never get this far:
// collapseControl takes them out of every field before anything reads it or
// stores it.
func invisibleRune(r rune) bool {
	return unicode.In(r, unicode.Cf, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector)
}

// decimalValue is the value of a decimal digit in any script. Unicode encodes
// every set of decimal digits as a contiguous 0-9 run, so the value is how far
// the digit sits into its run.
func decimalValue(d rune) rune {
	n := rune(0)
	for n < 64 && unicode.IsDigit(d-n-1) {
		n++
	}
	return n % 10
}

// lookalikes maps the letters most often swapped in for Latin ones — Cyrillic,
// Greek, the small capitals of "fancy text" generators, and Cherokee — in
// lower case, to the Latin letter they imitate. It is a short list on purpose:
// it only has to cover what a poster can type to dodge a word, and it never
// changes what is stored.
var lookalikes = func() map[rune]rune {
	m := map[rune]rune{
		'а': 'a', 'в': 'b', 'е': 'e', 'і': 'i', 'ј': 'j', 'к': 'k', 'м': 'm', 'н': 'h',
		'о': 'o', 'п': 'n', 'р': 'p', 'с': 'c', 'т': 't', 'у': 'y', 'х': 'x', 'ѕ': 's',
		'ԁ': 'd', 'ԛ': 'q', 'ԝ': 'w', 'һ': 'h', 'ӏ': 'l', 'ɡ': 'g', 'ı': 'i', 'ɩ': 'i',
		'α': 'a', 'β': 'b', 'ε': 'e', 'ι': 'i', 'κ': 'k', 'ο': 'o', 'ρ': 'p', 'τ': 't',
		'χ': 'x', 'ζ': 'z', 'μ': 'm',
		// Small capitals, which have no compatibility form to fold through.
		'ᴀ': 'a', 'ʙ': 'b', 'ᴄ': 'c', 'ᴅ': 'd', 'ᴇ': 'e', 'ꜰ': 'f', 'ɢ': 'g', 'ʜ': 'h',
		'ɪ': 'i', 'ᴊ': 'j', 'ᴋ': 'k', 'ʟ': 'l', 'ᴍ': 'm', 'ɴ': 'n', 'ᴏ': 'o', 'ᴘ': 'p',
		'ꞯ': 'q', 'ʀ': 'r', 'ꜱ': 's', 'ᴛ': 't', 'ᴜ': 'u', 'ᴠ': 'v', 'ᴡ': 'w', 'ʏ': 'y',
		'ᴢ': 'z',
	}
	// Cherokee capitals that look like Latin ones. foldRune lowers a letter
	// before it looks it up, and Cherokee has lower-case forms, so both go in.
	for c, l := range map[rune]rune{
		'Ꭰ': 'd', 'Ꭱ': 'r', 'Ꭲ': 't', 'Ꭵ': 'i', 'Ꭹ': 'y', 'Ꭺ': 'a', 'Ꭻ': 'j', 'Ꭼ': 'e',
		'Ꮃ': 'w', 'Ꮇ': 'm', 'Ꮋ': 'h', 'Ꮐ': 'g', 'Ꮓ': 'z', 'Ꮪ': 's', 'Ꮮ': 'l', 'Ꮯ': 'c',
		'Ꮲ': 'p', 'Ꮶ': 'k', 'Ꮩ': 'v', 'Ᏼ': 'b', 'Ꮤ': 'w',
	} {
		m[c] = l
		m[unicode.ToLower(c)] = l
	}
	return m
}()

// fancyRune reads the letters and digits that "fancy text" is made of and
// that have no compatibility form for NFKD to fold: the dingbat and double-
// circled digits, the negative circled and squared letters, and the regional
// indicators flags are written with. A parenthesised letter or number, which
// NFKD would read as "(a)", reads as what is inside the brackets.
func fancyRune(r rune) (string, bool) {
	digits := func(n rune) string { return strconv.Itoa(int(n)) }
	switch {
	case r >= 0x2776 && r <= 0x277F: // ❶ to ❿
		return digits(r - 0x2776 + 1), true
	case r >= 0x2780 && r <= 0x2789: // ➀ to ➉
		return digits(r - 0x2780 + 1), true
	case r >= 0x278A && r <= 0x2793: // ➊ to ➓
		return digits(r - 0x278A + 1), true
	case r >= 0x24EB && r <= 0x24F4: // ⓫ to ⓴
		return digits(r - 0x24EB + 11), true
	case r >= 0x24F5 && r <= 0x24FE: // ⓵ to ⓾
		return digits(r - 0x24F5 + 1), true
	case r == 0x24FF || r == 0x1F10B || r == 0x1F10C: // ⓿, 🄋, 🄌
		return "0", true
	case r >= 0x2474 && r <= 0x2487: // ⑴ to ⒇
		return digits(r - 0x2474 + 1), true
	case r >= 0x249C && r <= 0x24B5: // ⒜ to ⒵
		return string('a' + r - 0x249C), true
	case r >= 0x1F110 && r <= 0x1F129: // 🄐 to 🄩
		return string('a' + r - 0x1F110), true
	case r >= 0x1F150 && r <= 0x1F169: // 🅐 to 🅩
		return string('a' + r - 0x1F150), true
	case r >= 0x1F170 && r <= 0x1F189: // 🅰 to 🆉
		return string('a' + r - 0x1F170), true
	case r >= 0x1F1E6 && r <= 0x1F1FF: // 🇦 to 🇿
		return string('a' + r - 0x1F1E6), true
	}
	return "", false
}

// leetLetters is what each leetspeak character stands for in the loose view.
var leetLetters = map[rune]rune{
	'0': 'o', '1': 'i', '3': 'e', '4': 'a', '5': 's', '7': 't', '@': 'a', '$': 's',
}

// closingBracket pairs each opening bracket with its closing one, for the
// loose view's reading of a bracketed letter.
var closingBracket = map[rune]rune{'[': ']', '(': ')', '{': '}'}

// spacingRunes are what a poster puts between the letters of a spaced-out
// word: "s n a p", "s.n.a.p", "s-n-a-p", "s_n_a_p", "s*n*a*p", "s/n/a/p".
func spacingRune(r rune) bool {
	switch r {
	case ' ', '.', '-', '_', '*', '|', '~', '+', '·', '•', '/':
		return true
	}
	return false
}

type sourceRune struct {
	r        rune
	from, to int
}

func looseView(p *view, src string) view {
	runes := make([]sourceRune, 0, len(p.text))
	for i, r := range p.text {
		runes = append(runes, sourceRune{r: r, from: p.start[i], to: p.end[i]})
	}
	runes = closeUpSpacedLetters(runes)

	b := newViewBuilder(len(p.text))
	// letterAt reports a letter at i, looking through an apostrophe, so that
	// the 1 of "1'm" is read as a letter too.
	letterAt := func(i, step int) bool {
		if i >= 0 && i < len(runes) && runes[i].r == '\'' {
			i += step
		}
		return i >= 0 && i < len(runes) && unicode.IsLetter(runes[i].r)
	}
	// written is a letter as the triple-letter rule compares it: the plain
	// view's, in which case is gone, except that a capital I stays apart from
	// a small i, because a capital I stands for an l as often as for an i and
	// looks like one: "kiII" is "kill", not "kiii" read twice.
	written := func(sr sourceRune) rune {
		if sr.r == 'i' {
			if c, _ := utf8.DecodeRuneInString(src[sr.from:sr.to]); unicode.IsUpper(c) {
				return 'I'
			}
		}
		return sr.r
	}
	// A leetspeak character is read as a letter beside a letter, or beside
	// another that is, so that a run of them reads whole: "ki11", "k1ll".
	leet := make([]bool, len(runes))
	isLeet := func(i int) bool { _, ok := leetLetters[runes[i].r]; return ok }
	for i := range runes {
		leet[i] = isLeet(i) && (letterAt(i-1, -1) || letterAt(i+1, 1))
	}
	for i := 1; i < len(runes); i++ {
		leet[i] = leet[i] || leet[i-1] && isLeet(i)
	}
	for i := len(runes) - 2; i >= 0; i-- {
		leet[i] = leet[i] || leet[i+1] && isLeet(i)
	}
	// A letter bracketed inside a word is read as part of it: "i[n]sta",
	// "sn(a)p", "disc{o}rd".
	bracketed := make([]bool, len(runes))
	for i := 1; i+3 < len(runes); i++ {
		if c := closingBracket[runes[i].r]; c != 0 && runes[i+2].r == c && unicode.IsLetter(runes[i+1].r) &&
			unicode.IsLetter(runes[i-1].r) && unicode.IsLetter(runes[i+3].r) {
			bracketed[i], bracketed[i+2] = true, true
		}
	}
	for i, sr := range runes {
		r := sr.r
		if bracketed[i] {
			continue
		}
		// A letter written three or more times running is read twice:
		// "shooot", "schoool", "killll". Twice, not once, because the words
		// looked for have double letters of their own. The rune compared is
		// the one written, before l is read as i, so "kill" stays "kiii".
		if unicode.IsLetter(r) && i >= 2 && written(runes[i-1]) == written(sr) && written(runes[i-2]) == written(sr) {
			continue
		}
		if leet[i] {
			r = leetLetters[r]
		}
		// "!" for i only between two letters ("k!ll"): beside one it ends a
		// sentence.
		if r == '!' && letterAt(i-1, -1) && letterAt(i+1, 1) {
			r = 'i'
		}
		if r == 'l' {
			r = 'i'
		}
		b.add(r, sr.from, sr.to)
	}
	v := b.view()
	v.loose = true
	return v
}

// closeUpSpacedLetters removes the gaps from runs of three or more one-
// character tokens with at least two letters among them — "s n a p",
// "d.i.s.c.o.r.d", "5 n 4 p" — so the word they spell can be matched. Two
// one-letter words in a row ("a b") are common enough in ordinary text to be
// left alone; three rarely are.
func closeUpSpacedLetters(runes []sourceRune) []sourceRune {
	wordish := func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '@' || r == '$'
	}
	single := func(i int) bool {
		if i < 0 || i >= len(runes) {
			return false
		}
		r := runes[i].r
		if !unicode.IsLetter(r) && leetLetters[r] == 0 {
			return false
		}
		return (i == 0 || !wordish(runes[i-1].r)) && (i == len(runes)-1 || !wordish(runes[i+1].r))
	}

	out := make([]sourceRune, 0, len(runes))
	for i := 0; i < len(runes); {
		if !single(i) {
			out = append(out, runes[i])
			i++
			continue
		}
		tokens := []int{i}
		j := i + 1
		for {
			k := j
			for k < len(runes) && k-j < 3 && spacingRune(runes[k].r) {
				k++
			}
			if k == j || !single(k) {
				break
			}
			tokens = append(tokens, k)
			j = k + 1
		}
		letters := 0
		for _, t := range tokens {
			if unicode.IsLetter(runes[t].r) {
				letters++
			}
		}
		last := tokens[len(tokens)-1]
		if len(tokens) >= 3 && letters >= 2 {
			for _, t := range tokens {
				out = append(out, runes[t])
			}
		} else {
			// Every run that starts later inside this one ends where it does,
			// with no more tokens or letters, so none of them closes up
			// either: copy it through rather than look again.
			out = append(out, runes[i:last+1]...)
		}
		i = last + 1
	}
	return out
}

/* Patterns -------------------------------------------------------------------- */

// rule is one regular expression compiled for both views: as written for the
// plain view, and through canon for the loose one. A rule with no loose form
// runs on the plain view only, which is right for anything whose punctuation
// matters (an "@", an email's dots, a link's slashes) because the loose view
// reads some of that as letters.
type rule struct {
	plain, loose *search
}

// sep is what may separate two words of a phrase: any run of spaces and
// punctuation, or nothing, so "add me on snap", "add-me-on-snap" and the
// closed-up "addmeonsnap" are one phrase. Pattern sources write it as "~".
const sep = `[^\pL\pN]*`

func phrase(src string) string { return strings.ReplaceAll(src, "~", sep) }

// canon is the loose view's l-to-i reading, applied to a pattern. Pattern
// sources use no POSIX classes ("[:alpha:]"), the only regexp syntax with a
// lower-case l in it, so the replacement only ever touches literal letters
// (TestFilterPatternsSurviveCanon).
func canon(src string) string { return strings.ReplaceAll(src, "l", "i") }

func bothViews(src string) rule {
	src = phrase(src)
	return rule{plain: newSearch(src, famPlain), loose: newSearch(canon(src), famLoose)}
}

func plainOnly(src string) rule {
	return rule{plain: newSearch(phrase(src), famPlainOnly)}
}

// words joins alternatives into one non-capturing group.
func words(alternatives ...string) string {
	return "(?:" + strings.Join(alternatives, "|") + ")"
}

// Apps a conversation can be moved to. contactApps are named often enough as
// the thing a handle belongs to ("my snap is", "insta:") to be matched on their
// own. otherApps are apps too, but only after "add me on" and the like, where
// placeApps (the two together) may also follow "call", "find" or "move to".
// ambiguousApps are as often a place, an exam or a subject ("DC", "WA",
// "signal processing", "Epic Universe"), so they count only after a verb of
// talking: "add me on steam", "hmu on dc". anyApps is all three.
// Words that are an app only some of the time stay out: "line" ("find me in
// line at the canteen"), "threads", "teams" and "zoom" (school uses them),
// "imo" (in my opinion, and the maths olympiad), "snp" (a genetics term), and
// "phone" or "number", which name no place to talk without the number itself,
// and the number is caught on its own.
var (
	whatsApp    = words(`wh?ats?~?ap+`, `wh?ts~?ap+`, `wtsp`)
	contactApps = words(`snap+~chat`, `snap+`, `insta~gram`, `insta`, `ig`, `discord`, `telegram`, `tele`,
		`tg`, whatsApp, `kik`, `wechat`, `skype`, `viber`, `tik~tok`, `twitter`, `facebook`, `fb`, `reddit`,
		`roblox`, `yubo`, `wizz`)
	otherApps = words(`messenger`, `bereal`, `hangouts`, `gmail`, `e~?mail`, `xbox`, `psn`, `playstation`,
		`fortnite`, `valorant`, `minecraft`, `omegle`, `the~gram`)
	ambiguousApps = words(`signal`, `sc`, `dc`, `wa`, `wp`, `steam`, `epic`, `monkey`)
	placeApps     = words(contactApps, otherApps)
	anyApps       = words(placeApps, ambiguousApps)
	// handleApps are the apps a handle is written beside: contactApps, and
	// "sc", which next to a handle can only be Snapchat.
	handleApps = words(contactApps, `sc`)
	// mailApps are the ways an address's provider is named in place of it.
	mailApps = words(`g~?mail`, `e~?mail`, `mail`) + `(?:~(?:id|address))?`
)

var (
	yours     = words(`your`, `ur`, `yo`, `ya`, `u'?r`)
	whatsYour = words(`what'?s`, `whats`, `wats`, `wat'?s`, `wht'?s`, `what~is`, `wuts?`, `send`, `give`, `drop`,
		`share`, `post`, `tell`, `dm`, `gimme`, `lemme~`+words(`get`, `have`, `add`, `see`), `comment`, `leave`,
		`write`, `type`, `put`)
	asksTo = words(`on`, `at`, `in`, `via`, `thru`, `through`, `over`, `@`)
	// hindiGive is "give" or "send" or "tell", in Hinglish.
	hindiGive = words(`do`, `de`, `dedo`, `de~do`, `dena`, `bhejo`, `bhej`, `bhej~do`, `batao`, `bata`, `bta`,
		`share~`+karo, `comment~`+karo, `daalo`, `dalo`, `likho`)
	// karo and its spellings: "do it", in Hinglish.
	karo = words(`karo`, `kar`, `kr`, `kro`, `karna`, `krna`, `karlo`, `kar~lo`, `kr~lo`, `kar~do`, `kardo`,
		`krdo`, `kr~do`)
)

// askRules ask someone to talk on another app, or for their details there.
// askCheck turns down the ones followed by a word about the app instead of a
// place to talk ("what's your snap score", "check out my roblox game").
var askRules = []rule{bothViews(`\b` + words(
	// "add me on snap", "dm me on insta", "hit me up on discord", "add me on steam"
	words(`add`, `dm`, `pm`, `msg`, `message`, `text`, `txt`, `ping`, `inbox`, `snap`, `chat~with`, `talk~to`,
		`hit`)+`~(?:me|us)~(?:up~)?`+asksTo+`~(?:my~)?`+anyApps,
	// "call me on whatsapp", "find me on insta": not "find me in dc"
	words(`call`, `ring`, `follow`, `contact`, `reach`, `find`)+`~(?:me|us)~(?:up~)?`+asksTo+`~(?:my~)?`+placeApps,
	// "reach out to me on telegram", "get in touch on whatsapp"
	words(`reach~out`, `get~in~touch`, `keep~in~touch`, `stay~in~touch`)+`~(?:`+words(`to`, `with`)+`~(?:me|us)~)?`+
		asksTo+`~(?:my~)?`+placeApps,
	// "hmu on snap", "hmu snap"
	`hmu~`+words(`on`, `at`, `in`, `via`, `over`, `@`)+`~(?:my~)?`+anyApps,
	`hmu[ \t]+(?:my~)?`+contactApps,
	// "add my snap", "follow my insta"
	words(`add`, `follow`, `dm`, `message`, `msg`, `text`, `check~out`, `sub~to`, `subscribe~to`)+`~my~`+contactApps,
	// "what's your snap", "drop your insta"
	whatsYour+`~(?:me~)?`+yours+`~`+contactApps,
	// "what's your number", "give me your number"
	words(`what'?s`, `whats`, `wats`, `wat'?s`, `wht'?s`, `what~is`, `wuts?`, `send`, `give`, `drop`, `dm`, `text`,
		`comment`, `leave`, `post`, `reply~with`)+
		`~(?:me~)?`+yours+`~(?:(?:phone|mobile|cell|whatsapp|contact|personal)~)?`+
		words(`number`, `no`, `num`, `digits`),
	// "give me your email", "drop ur email": askCheck wants nothing after
	// it but the end of the ask, so "send me your email draft" is a draft.
	words(`what'?s`, `whats`, `wats`, `wat'?s`, `what~is`, `give~me`, `gimme`, `drop`, `share`, `send~me`, `dm~me`,
		`tell~me`, `leave`, `post`, `comment`)+`~`+yours+`~(?:`+words(`personal`, `school`, `college`, `uni`)+`~)?`+
		words(`e~?mail`, `g~?mail`)+`(?:~`+words(`id`, `address`, `addy`)+`)?`,
	// "follow me on X @name": X is a letter everywhere else, so only with
	// the handle.
	words(`follow`, `add`, `dm`, `message`, `msg`, `find`)+`~(?:me|us)~on~x[ \t]*@[\pL\pN_]{2,}`,
	// "can i get your snap"
	words(`can`, `could`, `may`)+`~i~`+words(`get`, `have`, `add`, `grab`)+`~`+words(`your`, `ur`, `ya`)+`~`+
		words(contactApps, `number`, `no`, `num`, `digits`),
	// "ur snap pls"
	words(`your`, `ur`)+`~`+words(contactApps, `number`)+`~`+words(`pls`, `plz`, `please`, `plox`, `plis`),
	// "join my discord"
	`join~(?:my|our)~`+words(`discord`, `telegram`, `tele`, `tg`, whatsApp, `snap`, `insta`, `signal`),
	// "let's talk on discord", "can we chat on steam"
	words(`let'?s`, `lets`, `we~can`, `can~we`, `wanna`, `want~to`)+`~`+
		words(`talk`, `chat`, `text`, `talk~more`, `speak`)+`~`+words(`on`, `in`, `over~on`, `over`, `to`, `via`)+
		`~`+anyApps,
	// "can we move to snap", "let's continue on insta": not "let's move on"
	words(`let'?s`, `lets`, `we~can`, `can~we`, `wanna`, `want~to`, `should~we`, `shall~we`)+`~`+
		words(`move`, `go`, `continue`, `take`)+`~(?:`+words(`this`, `it`, `things`, `the~chat`, `the~convo`,
		`the~conversation`, `this~chat`, `this~convo`, `this~conversation`, `this~thread`)+`~)?`+
		words(`to`, `over~to`, `on~to`, `onto`)+`~`+placeApps,
	// "move this to discord", "take it to whatsapp"
	words(`move`, `take`)+`~`+words(`this`, `it`, `the~chat`, `the~convo`, `the~conversation`, `this~chat`,
		`this~convo`, `this~conversation`)+`~`+words(`to`, `over~to`, `onto`)+`~`+placeApps,
	// "exchange snaps", "swap instas"
	words(`exchange`, `swap`, `trade`)+`~(?:our~)?`+words(`snaps?`, `snapchats?`, `instas?`, `igs`, `socials`,
		`discords?`, `handles`, `usernames`, `contacts`, `@s`),
	`exchange~(?:`+words(`our`, `phone`)+`~)+numbers`,
	`continue~(?:this~)?`+words(`on`, `over~on`, `in`)+`~`+placeApps,
	// "are you on snap?": an app to talk on, not a game or a console ("are you
	// on xbox or ps5?"), and with only a space after "on", so that "what
	// version are you on? minecraft 1.20" is two sentences.
	words(`are`, `r`)+`~`+words(`you`, `u`, `ya`)+`~on[ \t]+`+contactApps,
	// Hinglish: "insta pe add karo", "whatsapp pe baat karte hain"
	placeApps+`~`+words(`pe`, `par`, `pr`)+`~(?:mujhe~)?`+words(`add`, `follow`, `message`, `msg`, `dm`, `text`, `ping`)+
		`~`+karo,
	placeApps+`~`+words(`pe`, `par`, `pr`)+`~baat~`+words(karo, `karte`, `krte`, `karenge`, `krenge`, `karein`, `karen`),
	// "apna number do", "tera number de", "apni insta id bhejo", "tera snap kya hai"
	words(`apna`, `apni`, `tera`, `teri`, `tumhara`, `tumhari`, `aapka`, `aapki`, `apka`, `apki`)+`~`+
		`(?:`+contactApps+`~)?`+words(`number`, `no`, `num`, `nmbr`, `id`, contactApps)+`~`+
		words(hindiGive, `kya~`+words(`hai`, `h`, `he`, `hain`)),
	// "insta id do", "number bhej": askCheck turns down a roll or a page
	// number.
	contactApps+`~id~`+hindiGive,
	words(`number`, `nmbr`, `num`)+`~`+words(`bhej`, `bhejo`, `bhejna`, `dedo`, `de~do`, `de`, `share~`+karo,
		`send~`+karo),
) + `\b(?:[ \t]*@[ \t]*[\pL\pN._]+)?`),
	// "whatsapp me", "kik me", "facetime me", "snap me": only a space
	// between, so "WhatsApp, me and my friends" is a list. askCheck wants it
	// asked, and "snap" an app: not "oh snap me too".
	bothViews(`\b` + words(whatsApp, `kik`, `telegram`, `snapchat`, `snap`, `skype`, `insta`, `discord`, `face~?time`) +
		`[ \t]+me\b`),
	// "lets facetime", "can we facetime?": askCheck wants nothing after it
	// but the end of the ask, so "can we facetime grandma" is a family's.
	bothViews(`\b` + words(`let'?s`, `lets`, `can~we`, `could~we`, `wanna`, `want~to`, `shall~we`, `should~we`,
		`we~should`, `we~can`) + `~face~?time\b`),
	// "ur snap?", "u on snap?"
	bothViews(`\b` + words(`your`, `ur`) + `~` + contactApps + `[ \t]*\?`),
	bothViews(`\b` + words(`you`, `u`, `ya`) + `[ \t]+on[ \t]+` + contactApps + `[ \t]*\?`),
}

// numbersAskRule asks everyone for their numbers: "drop your numbers", "send
// your numbers in the comments", "drop numbers for the group". Scores,
// marks and readings are numbers too, so numbersCheck wants a way to reach
// someone round the ask, or the ask to end there in a post about nothing
// else.
var numbersAskRule = bothViews(`\b` + words(`send`, `give`, `drop`, `dm`, `text`, `comment`, `leave`, `post`, `share`,
	`reply~with`) + `~(?:me~)?(?:` + yours + `~)?(?:` + words(`phone`, `mobile`, `cell`, `whatsapp`, `contact`) +
	`~)?` + words(`numbers`, `nos`, `nums`) + `\b`)

// numbersCheck reads what follows the numbers asked for, and round them. A
// way to reach someone makes them phone numbers whatever else the post is
// about: the ask names one ("drop your phone numbers", "dm me your
// numbers") or the post sets up a group or a call ("anyone wanna make a
// whatsapp group? drop your numbers", "drop ur numbers i'll add you"). Without
// one, an ask that ends there ("drop your numbers below", "send your numbers
// in the comments") is for phone numbers unless the post is about results,
// where the commonest post after one asks for everyone's ("jee mains result
// is out, drop your numbers", "what did everyone get? drop your numbers
// below"); and numbers about something are figures: "post your numbers from
// the mock", "drop your numbers in the poll".
func numbersCheck(v *view, m []int) bool {
	text := v.text
	if !notNegated(v, m) || v.reported(m) {
		return false
	}
	after := nextWords(clauseAfter(text, m[1]), 4)
	if len(after) > 0 && numbersTopics[after[0]] {
		return false
	}
	lo, hi := max(0, m[0]-160), min(len(text), m[1]+160)
	v.work += hi - lo
	around := wordsIn(text[lo:hi])
	// Not the ends of words the window cuts: "addition" is not "add".
	if hi < len(text) && len(around) > 0 {
		around = around[:len(around)-1]
	}
	if lo > 0 && len(around) > 0 {
		around = around[1:]
	}
	if anyIn(wordsIn(text[m[0]:m[1]]), numbersReach) {
		return true
	}
	if aboutResults(around) {
		// After a result, adding them to a sheet, a close call or the text
		// section are about the scores: only a way to reach people decides
		// it.
		return reachNamed(around)
	}
	return anyIn(around, gatheringWords) || allIn(after, numbersTails)
}

// reachNamed reports a way to reach people named round an ask for
// everyone's numbers in a post about results: an app, someone added, called
// or texted ("i'll add you", "add everyone to the gc", "call me", "on a
// call"), or a group set up ("so we can make a group"); not the scores added
// to a sheet, a close call, the text section or a study group.
func reachNamed(ws []string) bool {
	person := func(k int) bool { return k < len(ws) && reachedPeople[ws[k]] }
	for k, w := range ws {
		switch {
		case reachApps[w] || is(w, "contact") || is(w, "contacts"):
			return true
		case addWords[w]:
			for j := k + 1; j < min(len(ws), k+6); j++ {
				if sheetWords[ws[j]] {
					break
				}
				if numbersGroups[ws[j]] || j == k+1 && (is(ws[j], "you") || is(ws[j], "u") || is(ws[j], "ya")) {
					return true
				}
			}
		case callWords[w]:
			if person(k+1) || k > 0 && (is(ws[k-1], "on") || numbersCallKinds[ws[k-1]]) ||
				k > 1 && is(ws[k-2], "on") && is(ws[k-1], "a") {
				return true
			}
		case is(w, "text") || is(w, "texting") || is(w, "txt"):
			if person(k + 1) {
				return true
			}
		case numbersGroups[w]:
			for _, x := range ws[max(0, k-3):k] {
				if groupMakers[x] {
					return true
				}
			}
		}
	}
	return false
}

// aboutResults reports that the words round an ask for everyone's numbers
// are about an exam's results: a word for them ("result", "marks", "the
// mock's", "topped"), "how many" or "how did", or what someone got or scored
// ("what did everyone get?").
func aboutResults(ws []string) bool {
	for i, w := range ws {
		switch {
		case scoreWords[strings.TrimSuffix(w, "'s")]:
			return true
		case is(w, "how") && i+1 < len(ws) && (is(ws[i+1], "many") || is(ws[i+1], "much") || is(ws[i+1], "did")):
			return true
		case is(w, "did") || is(w, "do"):
			for _, x := range ws[i+1 : min(len(ws), i+3)] {
				if resultVerbs[x] {
					return true
				}
			}
		}
	}
	return false
}

var (
	// numbersTails end an ask for numbers: "below", "here", "in the
	// comments".
	numbersTails = canonSetOf(`below here pls plz please guys everyone all in the comments comment section chat thread
		rn asap fast quick quickly bro yaar fam`)
	// numbersTopics after numbers make them scores or figures: "from the
	// mock", "of the lab", "as they are", "on the test".
	numbersTopics = canonSetOf(`from of as on per vs versus off up down out`)
	// numbersReach in the ask itself make the numbers a way to reach
	// someone: "drop your phone numbers", "dm me your numbers", "text me ur
	// numbers".
	numbersReach = canonSetOf(`phone mobile cell whatsapp contact dm text`)
	// gatheringWords round an ask for numbers say what they are for: a
	// group, a call, being added or texted, or an app.
	gatheringWords = canonSetOf(`group groups gc grp whatsapp telegram signal discord insta instagram snap snapchat add
		adding added call calls calling text texting txt contact contacts`)
	// scoreWords round an ask for everyone's numbers make them an exam's
	// results: "jee mains result is out, drop your numbers", "mock test 5
	// done", "let's see who topped".
	scoreWords = canonSetOf(`result results marks mark score scores scored scoring mock mocks test tests exam exams
		paper papers rank ranks ranking percentile percentiles topped topper toppers cutoff cutoffs grade grades gpa
		cgpa sgpa percentage marksheet attempt attempts attempted questions hours accuracy spreadsheet leaderboard`)
	// resultVerbs after "did" or "do" ask what everyone got: "what did
	// everyone get?", "how did u score".
	resultVerbs = canonSetOf(`get got score scored`)
	// The words reachNamed reads round an ask for numbers after a result.
	reachApps        = canonSetOf(`whatsapp telegram signal discord insta instagram snap snapchat`)
	reachedPeople    = canonSetOf(`you u ya me us everyone everybody yall`)
	addWords         = canonSetOf(`add adding added`)
	callWords        = canonSetOf(`call calls calling`)
	numbersCallKinds = canonSetOf(`group video voice conference`)
	numbersGroups    = canonSetOf(`group groups gc grp chat`)
	groupMakers      = canonSetOf(`make making made create creating created start starting started form forming join
		joining new`)
	sheetWords = canonSetOf(`sheet sheets spreadsheet spreadsheets leaderboard graph graphs list table doc excel
		average averages tally up predictor poll`)
)

// atAskRule asks for a handle by its "@" alone: "drop ur @", "what's your @".
// Its check reads what follows, so that the "@" is not the start of one.
var atAskRule = bothViews(`\b` + whatsYour + `~(?:me~)?` + yours + `[ \t]*@`)

// topicWords, straight after an app's name, make it the app's score, game,
// look or posts that are being talked about, not a place to talk ("check out
// my reddit post"). The exams are Telangana's, whose "TG" is also Telegram's:
// "what's your TG EAPCET rank?"; the results are an exam's, SNAP's among them,
// which is Symbiosis's MBA entrance as well as Snapchat: "what's your SNAP
// percentile?".
var topicWords = canonSetOf(`score scores karma avatar streak streaks level levels rank elo rating bio pfp
	aesthetic filter filters theme wallpaper game games build builds map maps obby clip clips song songs edit
	edits skin skins outfit emoji emojis stats playlist screen bot bots story stories feed algorithm
	post posts reel reels video videos comment comments
	eapcet eamcet icet ecet pgecet polycet lawcet edcet
	percentile percentiles result results cutoff cutoffs marks mark exam exams slot slots attempt attempts
	paper papers prep mock mocks`)

func askCheck(v *view, m []int) bool {
	text := v.text
	if numberAsks[lastWord(text[m[0]:m[1]])] && rankedNumber(text[m[1]:]) {
		return false
	}
	// "never send your number to anyone", and the "'add me on snap' guys":
	// a warning, or the ask quoted as a kind of post.
	if !notNegated(v, m) || v.reported(m) {
		return false
	}
	if ws := wordsIn(text[m[0]:m[1]]); len(ws) > 0 && numberWords[ws[0]] {
		// "roll number bhej", "page number de": a reference, not a phone.
		if prev := wordsBefore(text, m[0], 1); len(prev) > 0 && numberRefs[prev[0]] {
			return false
		}
	}
	if mailAsked(text, m) {
		// An address asked for ends the ask: "drop ur email and I'll share
		// the folder", not "send me your email draft".
		if next := nextWords(clauseAfter(text, m[1]), 1); len(next) > 0 && !mailTails[next[0]] {
			return false
		}
	}
	if is(lastWord(text[m[0]:m[1]]), "me") {
		// "whatsapp me" asked, not "my mum will whatsapp me tonight".
		if !askedOfReaderHere(v, m) {
			return false
		}
		// "snap me back to reality", "oh snap me too".
		if ws := wordsIn(text[m[0]:m[1]]); is(ws[0], "snap") && snapNotAsked(nextWords(clauseAfter(text, m[1]), 2)) {
			return false
		}
	}
	if ws := wordsIn(text[m[0]:m[1]]); len(ws) > 2 && leaveVerbs[ws[0]] && !mailAsked(text, m) &&
		!numberAsks[ws[len(ws)-1]] {
		// "comment your insta", "leave your insta below", not "leave your
		// insta out of this".
		next := nextWords(clauseAfter(text, m[1]), 1)
		return len(next) == 0 || leaveTails[next[0]]
	}
	if is(lastWord(text[m[0]:m[1]]), "facetime") || is(lastWord(text[m[0]:m[1]]), "time") {
		// "lets facetime later", not "can we facetime grandma".
		return allIn(nextWords(clauseAfter(text, m[1]), 4), callTails)
	}
	return !topicWords[firstWordAfter(text, m[1])]
}

// snapNotAsked reports words after "snap me" that make "snap" a word, not
// the app: "snap me out of it", "snap me back to reality", "oh snap me too".
func snapNotAsked(ws []string) bool {
	if len(ws) == 0 {
		return false
	}
	switch {
	case snapNotApp[ws[0]]:
		return true
	case len(ws) > 1 && (is(ws[0], "back") || is(ws[0], "in")) && snapBackTo[ws[1]]:
		return true
	}
	return false
}

var (
	// leaveVerbs ask for an account written somewhere, and leaveTails may
	// follow it: "leave your insta below", "write your snap in the replies".
	leaveVerbs = canonSetOf(`comment leave write type put`)
	leaveTails = canonSetOf(`below here down in pls plz please ill i'll i and so if too guys bro everyone n u`)
	snapNotApp = canonSetOf(`too neither either out outta`)
	snapBackTo = canonSetOf(`to into half two`)
	// callTails may follow a call suggested: "lets facetime later", "can
	// we facetime rn?".
	callTails = canonSetOf(`later tonight today tmrw tmr tomorrow rn now sometime soon pls plz please then bro yaar
		na guys instead after school`)
)

// mailAsked reports an ask that ends in an address's name: "drop your
// email", "what's your email address".
func mailAsked(text string, m []int) bool {
	ws := wordsIn(text[m[0]:m[1]])
	for k := len(ws) - 1; k >= 0; k-- {
		if mailWords[ws[k]] {
			return true
		}
		if !mailLabels[ws[k]] {
			break
		}
	}
	return false
}

// askedOfReaderHere reports a private ask at m said as one: an imperative or
// a "can you", "feel free to", "let's" (askMood), and not asked why or how
// ("why would you text me at 3am").
func askedOfReaderHere(v *view, m []int) bool {
	text := v.text
	ws, lo := readBack(text, m[1], 10, 96)
	v.work += m[1] - lo
	k := 0
	for k < len(ws) && ws[k].to <= m[0] {
		k++
	}
	if k >= len(ws) {
		return false
	}
	return askLead(text, ws, k) && !habitAfter(text[m[1]:])
}

// askLead reports that the words before ws[k] in its clause leave the verb
// there an ask of the reader (see askedOfReaderHere).
func askLead(text string, ws []aWord, k int) bool {
	start := clauseOpening(text, ws, k)
	given, opening := start < k, true
	for _, w := range ws[start:k] {
		// Asked why or how, not "anyone who wants the notes".
		if whWords[w.w] && opening {
			return false
		}
		opening = opening && askFillers[w.w]
		given = given && (strings.ContainsAny(w.w, "0123456789") || askFillers[w.w])
	}
	// A number or a handle before it is what to use: "082 541 7703
	// whatsapp me".
	if given || askMood(text, ws, k, false) {
		return true
	}
	// Anything else before it is an ask too, unless it is a subject and its
	// verb: a condition ("if ur bored kik me"), a relative ("anyone who
	// wants the notes whatsapp me"), a purpose ("for more pics snapchat
	// me") or whom it asks ("lonely girls whatsapp me", "ur cute kik me").
	lead := make([]string, 0, k-start)
	for _, w := range ws[start:k] {
		lead = append(lead, w.w)
	}
	for len(lead) > 0 && askFillers[lead[len(lead)-1]] {
		lead = lead[:len(lead)-1]
	}
	return !statesAnAct(lead)
}

// statesAnAct reports a lead that ends in a subject, its auxiliary or an
// adverb of how often, which makes what follows a statement of what someone
// does: "my mum will whatsapp me", "she keeps trying to kik me", "he never
// facetime me", "i hope you whatsapp me", "if he kik me again".
func statesAnAct(lead []string) bool {
	n := len(lead)
	if n == 0 {
		return false
	}
	last := lead[n-1]
	before := ""
	if n > 1 {
		before = lead[n-2]
	}
	switch {
	case canModals[last] || modalsPast[last]:
		// "anyone can whatsapp me" offers.
		return !askers[before]
	case actSubjects[last] || actAdverbs[last] || is(last, "to") || is(last, "gonna") || is(last, "gotta"):
		return true
	case thirdOwners[before]:
		return true // "my mum", "her friends"
	}
	return false
}

// habitAfter reports words after "<app> me" that say how often it happens:
// "random guys kik me all the time".
func habitAfter(after string) bool {
	ws := nextWords(clauseAfter(after, 0), 3)
	if len(ws) == 0 {
		return false
	}
	switch {
	case habitWords[ws[0]]:
		return true
	case len(ws) > 2 && is(ws[0], "all") && is(ws[1], "the") && is(ws[2], "time"):
		return true
	case len(ws) > 1 && is(ws[0], "a") && is(ws[1], "lot"):
		return true
	}
	return false
}

var (
	modalsPast  = canonSetOf(`might must i'll he'll she'll they'll we'll you'll u'll i'd he'd she'd they'd we'd you'd`)
	askers      = canonSetOf(`you u ya we anyone anybody someone somebody everyone everybody whoever`)
	actSubjects = canonSetOf(`i he she they we it you u people who im i'm he's she's they're theyre`)
	actAdverbs  = canonSetOf(`always never often sometimes usually still even already rarely constantly keeps kept
		don't dont didn't didnt doesn't doesnt won't wont can't cant wouldn't wouldnt shouldn't shouldnt did does`)
	thirdOwners = canonSetOf(`my his her their our`)
	habitWords  = canonSetOf(`every everyday daily constantly nonstop sometimes often always`)
)

var (
	mailLabels = canonSetOf(`id address addy`)
	// mailTails may follow an address asked for: "drop ur email and I'll
	// share it", "your email pls", "email below".
	mailTails = canonSetOf(`and so below here if i'll ill i we then n pls plz please rn now asap too also bro yaar
		na anon op guys quickly`)
	whWords = canonSetOf(`why how who when what whom whose`)
)

var (
	numberWords = canonSetOf(`number nmbr num`)
	numberRefs  = canonSetOf(`roll order page question q seat ticket account acc registration reg application
		form enrollment enrolment admit hall invoice bill serial chapter ques sawal`)
)

// numberAsks end an ask for a phone number: "what's your number".
var numberAsks = setOf(`number no num digits`)

// rankedNumber reports a ranking or a count after "number" where a phone
// number would be asked for: "what's your number one tip", "ur no. 1
// priority", "what's your number of attempts". "One sec" and "one more time"
// are not a ranking.
func rankedNumber(after string) bool {
	s := strings.TrimLeft(after, " ")
	s = strings.TrimLeft(strings.TrimPrefix(s, "."), " #")
	if w := firstWordAfter(s, 0); w != "" {
		return w == "of" || w == "one" && !notARank[firstWordAfter(s, len(w))]
	}
	n := 0
	for n < len(s) && isASCIIDigit(s[n]) {
		n++
	}
	if n < 1 || n > 2 {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[n:])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !notARank[firstWordAfter(s, n)]
}

var notARank = canonSetOf(`sec second seconds min minute minutes moment more last time`)

// myAppRule is "my snap is ...", whose last word decides it (myAppCheck): a
// username, or a word about the app ("my discord is lagging").
// The gap after "my" may be empty, for "mysnap" closed up from "m y s n a p",
// but not an underscore: "my_snap = snap(x)" is code.
var myAppRule = bothViews(`\b` + words(`my`, `mera`, `meri`, `mere`, `apna`, `apni`) + `[^\pL\pN_]*` + contactApps +
	`\b(?:~` + words(`id`, `username`, `user~name`, `handle`, `name`, `acc`, `account`, `tag`, `@`) +
	`)?\s*(?:` + words(`is`, `hai`, `h`) + `\b|[=:\-])\s*(@?[\pL\pN._]+)`)

// myAmbiguousAppRule is "my signal is ...", for the apps that are also words:
// only a username's marks make what follows a handle ("my signal is
// quietfox.27", not "my signal is weak" or "my line is busy").
var myAmbiguousAppRule = bothViews(`\b` + words(`my`, `mera`, `meri`) + `~` + words(`signal`, `line`, `steam`,
	`epic`) + `\b(?:~` + words(`id`, `username`, `user~name`, `handle`, `tag`) + `)?\s*(?:` +
	words(`is`, `hai`, `h`) + `\b|[=:\-])\s*(@?[\pL\pN._]+)`)

// notAHandle lists words that say something about the app rather than name
// an account on it, for myAppCheck.
var notAHandle = func() map[string]bool {
	list := strings.Fields(`is not so too very really kinda kind sort literally still also just now
		down broken full private public deleted banned hacked dead gone slow lagging laggy glitching
		glitchy bugged buggy crashing acting being getting going weird empty boring bad terrible
		awful good great fine ok okay off on in at the a an my mine your ur our their his her locked
		suspended disabled deactivated blocked back working closed open filled flooded spammed spam
		messed cooked cursed toxic dry quiet inactive active different new old same like and but
		for with all always never only probably basically honestly actually definitely better worse
		fun nice cool amazing lit trash garbage mid annoying addictive useless pointless done over
		gonna going about mostly mainly where what when how why who it its this that there here
		no yes lol lmao ngl tbh idk rly lowkey highkey pretty super fire cringe cringey peak goated
		dope sick ugly embarrassing aesthetic dying hai h nahi`)
	m := make(map[string]bool, len(list))
	for _, w := range list {
		m[canon(w)] = true
	}
	return m
}()

// myMailRule and mailColonHandle name an address by its provider and the
// part before the "@": "my gmail is madeup12", "gmail: madeup12", "email ->
// madeup12". They need the same evidence as a handle. Without "my", "=",
// "=>" and ">" are code ("email = user.email", "email => email.length",
// "echo $email >> emails.txt"), not a separator.
var (
	myMailRule = bothViews(`\b` + words(`my`, `mera`, `meri`) + `[^\pL\pN_]*` + mailApps + `\b\s*(?:` +
		words(`is`, `hai`) + `\b|` + handleSeparator + `)\s*(@?)([\pL\pN._]{2,30})()`)
	mailColonHandle = bothViews(`\b` + mailApps + `\s*(?:-+>|→|:|-\s)\s*(@?)([\pL\pN._]{2,30})()`)
)

// handleSeparator is what is written between an app and a handle: a colon,
// an "=", a dash with a space after it, or an arrow ("->", "=>", "→", ">",
// ">>"), or a bar.
const handleSeparator = `(?:-+>|=>|→|>{1,2}|\||[:=]|-\s)`

// handleRules name an account on an app directly: "insta: x_y_z", "snap
// @ my_name", "insta id x_y_z", "discord: name#1234", "snap -> x_y_z",
// "@x_y_z on snap". The "@", the underscores and the digits are the
// evidence, which the caller reads from the source. Each has the same
// groups: an "@" before the handle, the handle, and a Discord discriminator
// after it.
var (
	appColonHandle = bothViews(`\b` + handleApps + `\b(?:~` +
		words(`id`, `username`, `user~name`, `handle`, `acc`, `account`, `tag`) +
		`)?\s*` + handleSeparator + `\s*(@?)([\pL\pN._]{2,30})(#\d{4})?`)
	appAtHandle = bothViews(`\b` + handleApps + `\s*(?:is\s*)?(@)\s*([\pL\pN._]{2,30})(#\d{4})?`)
	// "insta id x_y_z", "snap me x_y_z", "snap x_y_z", "insta id hai x_y_z",
	// "discord tag name#1234": a label, "me" or a copula, or nothing, and no
	// colon.
	appLabelHandle = bothViews(`\b` + handleApps + `(?:~` +
		words(`id`, `username`, `user~name`, `handle`, `tag`, `me`) + `)?(?:[ \t]+` + words(`is`, `hai`, `h`) +
		`)?\s+(@?)([\pL\pN._]{2,30})(#\d{4})?`)
	// "mera insta madeup_12 hai": the handle before the copula.
	hindiHandle = bothViews(`\b` + words(`mera`, `meri`, `mere`, `apna`, `apni`) + `~` + handleApps +
		`(?:~id)?[ \t]+(@?)([\pL\pN._]{2,30})(#\d{4})?[ \t]+` + words(`hai`, `h`) + `\b`)
	// "@x_y_z on snap", "x_y_z is my snap", "x_y_z (snap)": the handle first.
	handleBeforeApp = bothViews(`(?:(@)|\b)([\pL\pN][\pL\pN._]{1,29})(#\d{4})?[ \t]*(?:` +
		words(`on`, `is~my`, `on~my`, `in~my`, `-+>`, `=>`, `→`, `\(`) + `)[ \t]*` + handleApps + `\b`)
)

// More ways a handle is written beside its app: a Discord tag after "disc"
// or "dc" ("disc: name#1234"); an app that is also a word with a label
// before the handle ("line id: x_y", "steam username x_y"); and Snapchat's
// ghost ("👻 = x_y"). An "@" handle a few short words after the app
// ("telegram pe aao: @x_y") is appFrame's (handleAfterApp). The same three
// groups.
var (
	discTagHandle = bothViews(`\b` + words(`disc`, `dsc`, `dc`) + `\s*` + handleSeparator +
		`\s*(@?)([\pL\pN._]{2,30})(#\d{4})`)
	// A Riot ID's tag is letters and digits: "riot id: name#IND1".
	labelledAppHandle = bothViews(`\b` + words(`line`, `signal`, `steam`, `epic`, `kik`, `riot`) + `~` +
		words(`id`, `username`, `user~name`, `handle`, `tag`) + `\s*(?:` + handleSeparator + `|` +
		words(`is`, `hai`) + `\b)?\s*(@?)([\pL\pN._]{2,30})(#[\pL\pN]{3,5})?`)
	// A Riot ID after Valorant's or League's name, which needs its tag:
	// "valo id madeup#1234", "my valorant is madeup#1234", "riot:
	// madeup#1234", not "val: 3/10" or "valo is in unreal".
	riotTagHandle = bothViews(`\b` + words(`valorant`, `valo`, `val`, `riot`, `lol`, `league`) + `(?:~` +
		words(`id`, `tag`, `username`, `ign`) + `)?\s*(?:` + handleSeparator + `|` + words(`is`, `hai`) +
		`\b)?\s*(@?)([\pL\pN._]{2,30})(#[\pL\pN]{3,5})`)
	// The separator before the spaces: a single character and then a run
	// of spaces would be planned as a sentence boundary (filtersearch.go,
	// "delimited"), found from the handle after it, and so tried at every
	// word.
	ghostHandle = plainOnly(`👻(?:\s*` + handleSeparator + `)?\s*(@?)([\pL\pN._]{2,30})(#\d{4})?`)
)

// handleRules are the rules above that end at the handle.
var handleRules = []rule{appColonHandle, appAtHandle, appLabelHandle, hindiHandle, handleBeforeApp,
	discTagHandle, labelledAppHandle, riotTagHandle, ghostHandle}

// meetingIDRule is a call's meeting ID given without its link, which is as
// good as the link to anyone reading: "zoom id 845 2219 6630 pass: physics",
// "join zoom 845 2219 6630", "meeting ID: 845 2219 6630", Webex's "Meeting
// number (access code): 2634 123 4567", or a Meet code in its own shape
// after Meet's name or a label, "meet code: qvx-trmk-hpd", "gmeet
// qvx-trmk-hpd". "Zoom" alone, and a meeting's number in minutes, are not
// one.
var meetingIDRule = plainOnly(`\b(?:` + words(`zoom`, `webex`) + `~(?:meeting~)?(?:` +
	words(`id`, `code`, `no`, `number`, `link`, `access~code`) + `~)?(?:is~)?` + meetingNumber +
	`|` + words(`zoom`, `webex`, `teams`, `meet`, `g~?meet`, `google~meet`) + `~(?:meeting~)?` +
	words(`id`, `code`, `no`, `number`) + `(?:~is)?~(?:` + meetingNumber + `|` + meetCodeShape + `)` +
	`|meeting~` + words(`id`, `code`, `no`, `number`) + `(?:~access~code)?(?:~is)?~(?:` + meetingNumber + `|` +
	meetCodeShape + `)` +
	`|` + words(`meet`, `g~?meet`, `google~meet`) + `~(?:link~)?(?:is~)?` + meetCodeShape +
	`)\b`)

const (
	// meetingNumber is a meeting's number, nine to eleven digits.
	meetingNumber = `\d(?:~?\d){8,10}`
	// meetCodeShape is a Meet code: xxx-xxxx-xxx.
	meetCodeShape = `[a-z]{3}-[a-z]{4}-[a-z]{3}`
)

// accountNumberRules are accounts written as numbers no other rule reads: a
// Session messenger ID, 66 hexadecimal digits starting "05", which nothing
// else is written as; and a game's friend code after its name ("steam
// friend code 4410877305", "friend code SW-0000-0000-0000"), or its ID
// after a game's name ("bgmi id 0000000000", "free fire uid 000000000",
// "steam id 4410877305"): each has a friend list with chat.
var accountNumberRules = []rule{
	plainOnly(`\b05[0-9a-f]{64}\b`),
	plainOnly(`\bfriend~?codes?(?:~is)?~(?:sw~?)?\d(?:~?\d){7,11}\b`),
	plainOnly(`\b` + words(`steam`, `bgmi`, `pubg`, `free~?fire`, `ff`, `genshin`, `cod`, `codm`, `cod~mobile`,
		`clash~of~clans`, `coc`, `brawl~stars`, `mlbb`) + `~(?:` + words(`player`, `game`, `friend`) + `~)?` +
		words(`id`, `uid`, `code`) + `(?:~is)?~\d(?:~?\d){7,16}\b`),
}

/* Apps named in a sentence ---------------------------------------------------- */

// appNameRule finds every app's name, and Snapchat's ghost, for appFrame to
// read the words round it. The rules above are phrasings; appFrame reads the
// grammar they are instances of, from the app outwards:
//
//   - a verb of moving or talking, with its object, a particle and a
//     preposition, said as an ask: an imperative or a "let's", a "can we",
//     a "wanna" ("come to tg", "follow me back on insta pls", "can we switch
//     to signal?", "i'll dm you on insta");
//   - a question about the reader's account ("u got insta?", "do you have
//     signal?", "anyone here on discord?");
//   - messages on an app ("slide into my dms on insta", "my insta dms are
//     open");
//   - the Hinglish forms, either way round ("insta pe aaja baat karte",
//     "telegram pe join karo", "dm kar insta pe", "WhatsApp number do",
//     "apna gmail id bata");
//   - a handle a few lead words after the app ("snap pe aaja: madeup.0p",
//     "my 👻 is madeup.sprinkles", "telegram: madeup underscore jee"), or
//     quoted after "search" where an account is meant;
//   - talking "there" once an app, or a link to it, has been named
//     ("instagram.com/p/… comment there so we can talk");
//   - a group or a call set up on one ("can someone make a discord vc");
//   - a handle that is another field of the post ("my insta is my name",
//     "username is the subject of this post"), when that field is filled in.
//
// The apps that are also words ("signal", "dc", "steam") count only after a
// verb of talking, or in a "we" frame with nothing after them, or asked
// about where the post says it wants to leave the board. Threads, whose name
// is the board's word for a conversation, counts only with a handle and its
// "@" after it ("my threads is @x.y"); Bluesky everywhere.
var appNameRule = bothViews(`(?:\b` + words(anyApps, `threema`, `blue~?sky`, `bsky`, `threads`) + `\b|👻)`)

// offBoardRule is talking off the board with no app named: "anyone want to
// text instead of posting here?", "can u text me instead?", "let's dm
// somewhere else", "is there somewhere more private we could talk?".
// offBoardCheck wants a verb of talking said as an ask beside it.
var offBoardRule = bothViews(`\b` + words(
	words(
		words(`instead~of`, `rather~than`)+`~`+words(`posting`, `commenting`, `replying`, `here`, `on~here`,
			`this~board`, `the~board`, `this~thread`, `this~site`, `educhan`),
		`off~`+words(`here`, `this~board`, `the~board`, `this~site`, `educhan`),
		`outside~(?:of~)?`+words(`here`, `this~board`, `the~board`, `this~site`, `educhan`),
		`somewhere~else`, `elsewhere`, `somewhere~(?:more~)?private`,
		// "can we talk privately?", "lets talk in private", "can we talk
		// 1 on 1?", "lets go private"
		`privately`, `in~private`, `go~private`, `1~on~1`, `one~on~one`,
	)+`\b`,
	`instead[ \t]*(?:[.!?\n,;:)]|$)`,
))

// aWord is a word of a view and where it is: letters, digits, apostrophes
// and underscores, an "@" before them, and a dot only inside it, so that
// "madeup.0p" is one word and "instead." is "instead". quoted says a
// quotation mark opened just before it and closed just after it.
type aWord struct {
	w        string
	from, to int
	quoted   bool
}

// readWords reads up to n words of text from offset i on, within limit
// bytes and its sentence, and returns them and how far it read. A sentence
// ends at a "!", "?" or line break, or a full stop with a space or nothing
// after it.
func readWords(text string, i, n, limit int) ([]aWord, int) {
	ws, j, _ := readOn(text, i, n, limit, 0)
	return ws, j
}

// readOn is readWords going on past as many as crossings sentence ends. It
// also returns where the first sentence ended, or -1.
func readOn(text string, i, n, limit, crossings int) ([]aWord, int, int) {
	return readOnInto(make([]aWord, 0, min(n, 16)), text, i, n, limit, crossings)
}

// readOnInto is readOn, appending the words to out.
func readOnInto(out []aWord, text string, i, n, limit, crossings int) ([]aWord, int, int) {
	hi := min(len(text), i+limit)
	out = out[:0]
	j, firstEnd := i, -1
	for j < hi && len(out) < n {
		r, size := decodeRuneBefore(text, j, hi)
		if sentenceEnd(text, j, r) {
			if firstEnd < 0 {
				firstEnd = j
			}
			if crossings == 0 {
				break
			}
			crossings--
			j += size
			continue
		}
		if !inWord(r) && r != '@' {
			j += size
			continue
		}
		k := j + size
		for k < hi {
			r, size := decodeRuneBefore(text, k, hi)
			if inWord(r) {
				k += size
				continue
			}
			if r == '.' && k+size < hi {
				if n, _ := decodeRuneBefore(text, k+size, hi); inWord(n) && n != '\'' {
					k += size
					continue
				}
			}
			break
		}
		if w, ok := makeWord(text, j, k); ok {
			out = append(out, w)
		}
		j = k
	}
	return out, j, firstEnd
}

// readBack reads up to n words before offset i, within limit bytes and its
// sentence, in order, and returns them and where it stopped.
func readBack(text string, i, n, limit int) ([]aWord, int) {
	return readBackInto(make([]aWord, 0, min(n, 16)), text, i, n, limit)
}

// readBackInto is readBack, appending the words to out.
func readBackInto(out []aWord, text string, i, n, limit int) ([]aWord, int) {
	lo := max(0, i-limit)
	out = out[:0]
	end := -1
	j := i
	for j > lo && len(out) < n {
		r, size := decodeRuneAfter(text, lo, j)
		if inWord(r) || r == '.' && end >= 0 && j-size > lo && inWord(lastRune(text[lo:j-size])) {
			if end < 0 {
				end = j
			}
			j -= size
			continue
		}
		if end >= 0 {
			from := j
			if r == '@' {
				from = j - size
			}
			if w, ok := makeWord(text, from, end); ok {
				out = append(out, w)
			}
			end = -1
			if len(out) == n {
				break
			}
		}
		if sentenceEnd(text, j-size, r) {
			break
		}
		j -= size
	}
	if end >= 0 && len(out) < n {
		if w, ok := makeWord(text, j, end); ok {
			out = append(out, w)
		}
	}
	slices.Reverse(out)
	return out, j
}

func inWord(r rune) bool { return alnum(r) || r == '_' || r == '\'' }

// decodeRuneBefore is utf8.DecodeRuneInString(text[i:hi]), and
// decodeRuneAfter utf8.DecodeLastRuneInString(text[lo:j]), with a byte of
// ASCII read as itself first: the word readers decode every character of
// the frames round an app's name, and most of them are ASCII.
func decodeRuneBefore(text string, i, hi int) (rune, int) {
	if c := text[i]; c < utf8.RuneSelf {
		return rune(c), 1
	}
	return utf8.DecodeRuneInString(text[i:hi])
}

func decodeRuneAfter(text string, lo, j int) (rune, int) {
	if c := text[j-1]; c < utf8.RuneSelf {
		return rune(c), 1
	}
	return utf8.DecodeLastRuneInString(text[lo:j])
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

// sentenceEnd reports that the character r at offset j of text ends a
// sentence.
func sentenceEnd(text string, j int, r rune) bool {
	switch r {
	case '!', '?', '\n':
		return true
	case '.':
		return j+1 >= len(text) || text[j+1] == ' '
	}
	return false
}

// makeWord is the word at text[from:to], with quotation marks round it
// taken off and noted.
func makeWord(text string, from, to int) (aWord, bool) {
	w := aWord{w: text[from:to], from: from, to: to}
	if len(w.w) > 2 && strings.HasPrefix(w.w, "'") && strings.HasSuffix(w.w, "'") {
		w.w, w.from, w.to, w.quoted = w.w[1:len(w.w)-1], w.from+1, w.to-1, true
	} else {
		for strings.HasPrefix(w.w, "'") {
			w.w, w.from = w.w[1:], w.from+1
		}
		for strings.HasSuffix(w.w, "'") {
			w.w, w.to = w.w[:len(w.w)-1], w.to-1
		}
		// A straight or curly quotation mark each side, compared as bytes
		// rather than decoded: the word readers make every word of the
		// frames round an app's name.
		if w.from > 0 && w.to < len(text) {
			w.quoted = (text[w.from-1] == '"' || strings.HasSuffix(text[:w.from], "“")) &&
				(text[w.to] == '"' || strings.HasPrefix(text[w.to:], "”"))
		}
	}
	return w, w.w != ""
}

// clauseOpening is the index of the first of ws in the clause that ends
// at ws[k]: after the last comma, semicolon, colon or sentence end between
// them.
func clauseOpening(text string, ws []aWord, k int) int {
	for i := k; i > 0; i-- {
		if strings.ContainsAny(text[ws[i-1].to:ws[i].from], ",;:.!?\n") {
			return i
		}
	}
	return 0
}

// appFrame reads the words round the app's name at m (see appNameRule) and
// returns what to quote and what it is.
func (f *scannedField) appFrame(v *view, m []int) (int, int, findingKind, bool) {
	text := v.text
	app := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, text[m[0]:m[1]])
	if app == "" {
		app = "snap" // the ghost
	}
	mail := mailWords[app]
	ambiguous := ambiguousAppWords[app]
	threads := is(app, "threads")
	// Part of an abbreviation, as in "M.Sc", "B.Sc".
	if m[0] >= 2 && text[m[0]-1] == '.' && unicode.IsLetter(rune(text[m[0]-2])) {
		return 0, 0, 0, false
	}
	// Seven words before it and twelve after, in its sentence: enough for
	// the longest frame, and no more, since an app may be named in every
	// other word ("ig ig ig").
	before, lo := readBackInto(f.frameBefore[:0], text, m[0], 7, 64)
	following, end, firstEnd := readOnInto(f.frameAfter[:0], text, m[1], 12, 96, 0)
	f.frameBefore, f.frameAfter = before, following
	v.work += end - lo
	hi := end
	after := following
	if firstEnd >= 0 {
		hi = firstEnd
		for k, w := range following {
			if w.from > firstEnd {
				after = following[:k]
				break
			}
		}
	}
	if len(after) > 8 {
		after = after[:8]
	}
	ws := func(list []aWord) []string {
		out := make([]string, len(list))
		for i, w := range list {
			out[i] = w.w
		}
		return out
	}
	next := f.frameNext[:0]
	for _, w := range after {
		next = append(next, w.w)
	}
	f.frameNext = next
	question := strings.HasPrefix(text[hi:], "?")
	// What comes after the app makes it a topic: "my snap score", "come to
	// insta live", "tg eamcet counselling". A streak kept with someone is
	// an ask: "who wants to be my snap streak buddy".
	if len(next) > 0 && topicWords[next[0]] {
		if len(after) > 1 && streakWords[next[0]] && streakPartners[next[1]] && anyIn(ws(before), partnerAsks) &&
			!v.reported([]int{m[0], after[1].to}) {
			return m[0], after[1].to, kindAsk, true
		}
		return 0, 0, 0, false
	}
	// The app ends its clause, but for a word or two that do not change
	// it: "come to tg, the board is too slow", "can we switch to signal
	// instead?".
	clause := after
	for k := range after {
		if k > 0 && strings.ContainsAny(text[after[k-1].to:after[k].from], ",;:") {
			clause = after[:k]
			break
		}
	}
	if len(after) > 0 && strings.ContainsAny(text[m[1]:after[0].from], ",;:") {
		clause = nil
	}
	ends := allIn(next[:len(clause)], askTails) // clause is the start of after
	// An ask quoted from someone else, or answered with a no, is not the
	// poster's: ">>123 >dm me on insta lol no", "my mum said 'add me on
	// whatsapp'".
	ask := func(a, b int) (int, int, findingKind, bool) {
		if v.reported([]int{a, b}) {
			return 0, 0, 0, false
		}
		return a, b, kindAsk, true
	}

	handleKind := kindHandle
	if mail {
		handleKind = kindEmail
	}
	if threads {
		// Only "threads: @x", "my threads is @x".
		if b, ok := f.handleAfterApp(v, after, m, false); ok {
			if a, z := v.span(m[1], b); strings.Contains(f.src[a:z], "@") {
				return m[0], b, kindHandle, true
			}
		}
		return 0, 0, 0, false
	}
	// A handle after an ask is part of what to take out, and the whole of
	// it is named: "telegram pe aao: @x_y".
	withHandle := func(a, b int, k findingKind, ok bool) (int, int, findingKind, bool) {
		if hb, hok := f.handleAfterApp(v, after, m, mail); ok && hok && hb > b {
			return min(a, m[0]), hb, handleKind, true
		}
		return a, b, k, ok
	}

	if a, ok := f.asksToMove(v, before, next, app, ambiguous, ends); ok {
		return withHandle(ask(a, m[1]))
	}
	if a, ok := asksForAccount(text, before, ambiguous, gameAppWords[app], ends, question, m); ok {
		return ask(a, m[1])
	}
	if a, b, ok := messagesThere(text, before, after, m); ok {
		return withHandle(ask(a, b))
	}
	if a, b, ok := hinglishAsk(before, after, m); ok {
		return withHandle(ask(a, b))
	}
	if a, b, ok := appKaro(before, clause, ambiguous, m); ok {
		return ask(a, b)
	}
	if a, b, ok := setsUpAGroup(before, after, m); ok {
		return ask(a, b)
	}
	if b, ok := f.handleAfterApp(v, after, m, mail); ok {
		return m[0], b, handleKind, true
	}
	if mail {
		if a, b, ok := splitAddress(text, before, following, m); ok {
			return a, b, kindEmail, true
		}
	}
	if b, ok := f.otherFieldNamed(text, after, m); ok {
		return m[0], b, handleKind, true
	}
	if a, b, ok := talksThere(text, following); ok {
		return ask(a, b)
	}
	if a, ok := announcedOn(before, following); ok {
		return ask(a, m[1])
	}
	return 0, 0, 0, false
}

func allIn(ws []string, set map[string]bool) bool {
	for _, w := range ws {
		if !set[w] {
			return false
		}
	}
	return true
}

var (
	streakWords    = canonSetOf(`streak streaks`)
	streakPartners = canonSetOf(`buddy buddies partner partners pal pals bestie besties friend friends mate mates`)
	// partnerAsks before an app's streak partner ask for one: "who wants to
	// be my snap streak buddy", "need a snap streak partner".
	partnerAsks = canonSetOf(`be want wants wanna need needs looking anyone anybody someone somebody who any`)
)

var (
	mailWords         = canonSetOf(`gmail email mail`)
	ambiguousAppWords = canonSetOf(`signal sc dc wa wp steam epic monkey`)
	// shortApps are as often a place as an app ("tg" is Telangana, "wa"
	// Western Australia): moving to one is an ask only with nothing after it.
	shortApps = canonSetOf(`tg ig fb dc sc wa wp tele`)
	// askTails may end an ask after the app: "come to tg pls", "can we
	// switch to signal instead?".
	askTails = canonSetOf(`pls plz please instead then now rn tho though bro yaar na naa bhai anon op guys too also
		asap lol tonight today later there quickly`)
	moveVerbs = canonSetOf(`come move switch shift hop head migrate jump go continue take bring get`)
	// onVerbs move onto an app with "on": "hop on discord", "continue on
	// insta".
	onVerbs = canonSetOf(`hop jump get continue`)
	// appProducts after the app make it the software: "switch to whatsapp
	// web".
	appProducts = canonSetOf(`web desktop app apk business beta update version premium plus lite`)
	talkVerbs   = canonSetOf(`talk chat text txt dm pm msg message ping call ring add follow find reach hmu contact
		inbox snap hit join speak facetime vc holla holler knock`)
	// hollaVerbs take "at" before their object: "holla at me on IG".
	hollaVerbs = canonSetOf(`holla holler`)
	// movedTalk are what a move verb moves when it is not "this" or "it":
	// "move the study group to telegram", "take our chat to discord".
	movedTalk = canonSetOf(`group gc grp chat chats convo conversation conversations discussion discussions thread
		talk server squad crew gang batch`)
	movePreps = canonSetOf(`to onto into`)
	talkPreps = canonSetOf(`on at in via over thru through`)
	appDets   = canonSetOf(`my our the your ur a this that`)
	objects   = canonSetOf(`me us you u ya`)
	particles = canonSetOf(`back up over`)
	// askFillers come before an imperative without changing it: "pls just
	// come to tg", "bro add me on snap".
	askFillers = canonSetOf(`just pls plz please bro bhai yaar so ok okay then yo lol and also kindly do maybe guys
		everyone all sab anon op hey btw fr ngl now quickly abeg biko oya`)
)

// askMood reports that the words ws[:k], in the clause before a verb, make
// it an ask: nothing but fillers (an imperative), or a "let's", "we can",
// "can we", "wanna", "anyone want to", "you can", "feel free to"; or, when
// the poster offers ("i'll dm you on insta"), an "I" with "you" after the
// verb, which offer says is there.
func askMood(text string, ws []aWord, k int, offer bool) bool {
	start := clauseOpening(text, ws, k)
	lead := make([]string, 0, k-start)
	for _, w := range ws[start:k] {
		lead = append(lead, w.w)
	}
	for len(lead) > 0 && askFillers[lead[len(lead)-1]] {
		lead = lead[:len(lead)-1]
	}
	n := len(lead)
	last := func(i int) string {
		if n-i >= 0 {
			return lead[n-i]
		}
		return ""
	}
	switch {
	case n == 0:
		return true
	case is(last(1), "let's") || is(last(1), "lets") || is(last(1), "us") && is(last(2), "let"):
		return true
	case is(last(1), "wanna") || is(last(1), "to") && (is(last(2), "want") || is(last(2), "free")):
		return true
	case canModals[last(1)] && (is(last(2), "we") || is(last(2), "you") || is(last(2), "u")):
		return true // "we can", "you could"
	case (is(last(1), "we") || is(last(1), "you") || is(last(1), "u")) && canModals[last(2)]:
		return true // "can we", "could you"
	case anyoneWords[last(1)] && canModals[last(2)]:
		return true // "can someone add me to the whatsapp group"
	case offer && (firstPersonSubjects[last(1)] || canModals[last(1)] && is(last(2), "i") || is(last(1), "imma") ||
		is(last(1), "gonna")):
		return true // "i'll dm you", "i can add you"
	}
	return false
}

var (
	canModals   = canonSetOf(`can could should shall will would may`)
	anyoneWords = canonSetOf(`someone somebody anyone anybody any1`)
)

// asksToMove reads a verb of moving or talking before the app: "come to
// tg", "follow me back on insta", "can we switch to signal", "join our
// discord". It returns where the verb starts.
func (f *scannedField) asksToMove(v *view, before []aWord, next []string, app string, ambiguous, ends bool) (int, bool) {
	i := len(before) - 1
	det := i >= 0 && appDets[before[i].w]
	if det {
		i--
	}
	prep := ""
	if i >= 0 && (movePreps[before[i].w] || talkPreps[before[i].w]) {
		prep = before[i].w
		i--
		if i >= 0 && (is(before[i].w, "over") || is(before[i].w, "on")) && movePreps[prep] {
			i-- // "over to", "on to"
		}
	}
	for i >= 0 && particles[before[i].w] {
		i--
	}
	object := ""
	if i >= 0 && objects[before[i].w] {
		object = before[i].w
		i--
		if i >= 1 && is(before[i].w, "at") && hollaVerbs[before[i-1].w] {
			i-- // "holla at me"
		}
	} else if movePreps[prep] {
		// What is moved, when it is the talk itself: "move the study
		// group to telegram", "take our chat to discord".
		for span := 1; span <= 2; span++ {
			j := i - span
			if i >= 0 && movedTalk[before[i].w] && j >= 1 && appDets[before[j].w] && moveVerbs[before[j-1].w] {
				i = j - 1
				break
			}
		}
	}
	if i < 0 {
		return 0, false
	}
	verb := before[i].w
	switch {
	case moveVerbs[verb]:
		// "come to tg", "hop on discord", not "come on insta is so boring",
		// "go to tg for counselling" or "switch to whatsapp web".
		if !movePreps[prep] && !(onVerbs[verb] && talkPreps[prep]) {
			return 0, false
		}
		if shortApps[app] && !ends || len(next) > 0 && appProducts[next[0]] {
			return 0, false
		}
	case talkVerbs[verb]:
		if prep == "" && !(is(verb, "join") && det) {
			return 0, false
		}
	default:
		return 0, false
	}
	if ambiguous && (!ends || !talkVerbs[verb] && !weFrame(before, i)) {
		return 0, false
	}
	// "can we video call on whatsapp": the kind of call goes with it.
	if i > 0 && callKinds[before[i-1].w] && (is(verb, "call") || is(verb, "chat")) {
		i--
	}
	if !askMood(v.text, before, i, object != "" && !is(object, "me") && !is(object, "us")) {
		return 0, false
	}
	if !notNegated(v, []int{before[i].from, before[len(before)-1].to}) {
		return 0, false
	}
	return before[i].from, true
}

var callKinds = canonSetOf(`video voice audio`)

// weFrame reports a suggestion made to the reader before the verb at ws[k]:
// a "let's", or a "we" with a modal beside it ("can we switch to signal",
// "we should move to steam", "let's move to steam"). "We want to move to SC"
// is news, not a suggestion.
func weFrame(ws []aWord, k int) bool {
	for i := k - 1; i >= max(0, k-3); i-- {
		switch {
		case is(ws[i].w, "let's") || is(ws[i].w, "lets"):
			return true
		case is(ws[i].w, "we"):
			return i > 0 && canModals[ws[i-1].w] || i+1 < k && canModals[ws[i+1].w]
		}
	}
	return false
}

// asksForAccount reads a question about the reader's account on the app:
// "u got insta?", "do you have signal?", "anyone here on discord?", "got
// snap?". An app that is also a word ("do you have signal?" is asked of a
// phone too) needs the post to say it wants off the board as well.
func asksForAccount(text string, before []aWord, ambiguous, game, ends, question bool, m []int) (int, bool) {
	if !ends {
		return 0, false
	}
	i := len(before) - 1
	if i >= 0 && (is(before[i].w, "a") || is(before[i].w, "an")) {
		i--
	}
	if i < 0 || !accountVerbs[before[i].w] {
		return 0, false
	}
	verb := i
	i--
	for i >= 0 && (is(before[i].w, "still") || is(before[i].w, "even") || is(before[i].w, "also") ||
		is(before[i].w, "here")) {
		i--
	}
	switch {
	case i >= 0 && askedOfReader[before[i].w]:
		i--
	case i < 0 && is(before[verb].w, "got"):
	default:
		return 0, false
	}
	// Asked with a question mark, or with the question's word order:
	// "do u have telegram", "is anyone here on discord". A game is asked
	// about with the mark: "r u on minecraft rn" is about playing.
	inverted := false
	for i >= 0 && (askFillers[before[i].w] || accountAux[before[i].w]) {
		inverted = inverted || accountAux[before[i].w]
		i--
	}
	if i >= 0 && clauseOpening(text, before, len(before)-1) <= i {
		return 0, false
	}
	if !question && (!inverted || game) {
		return 0, false
	}
	if ambiguous && !leavingTheBoard(text, m) {
		return 0, false
	}
	return before[max(0, i+1)].from, true
}

var (
	accountVerbs = canonSetOf(`got have has on use`)
	accountAux   = canonSetOf(`do does d did have has is are r`)
	// gameAppWords are played as well as talked on, so being on one is
	// asked about with a question mark: "r u on minecraft rn" is a game.
	gameAppWords  = canonSetOf(`xbox psn playstation fortnite valorant minecraft`)
	askedOfReader = canonSetOf(`you u ya yu anyone anybody someone somebody any1 ppl people`)
)

// leavingTheBoard reports a post that says it wants to talk somewhere other
// than here, within 200 bytes of m: "would rather not do this on a public
// board", "not here", "instead".
func leavingTheBoard(text string, m []int) bool {
	around := text[max(0, m[0]-200):min(len(text), m[1]+200)]
	for _, cue := range offBoardCues {
		if strings.Contains(around, cue) || strings.Contains(around, canon(cue)) {
			return true
		}
	}
	return false
}

var offBoardCues = []string{"public board", "on here", "this board", "not here", "instead", "rather not",
	"in private", "privately", "off here", "off the board", "somewhere else", "elsewhere"}

// messagesThere reads messages on the app: "slide into my dms on insta",
// "my insta dms are open". The poster's or the reader's messages, or a verb
// sending someone to them; "insta dms are toxic" is about the app.
func messagesThere(text string, before, after []aWord, m []int) (int, int, bool) {
	n := len(before)
	i := n - 1
	if i >= 0 && talkPreps[before[i].w] {
		i--
		// "send me a dm on insta", "drop us a message on telegram".
		if j := i - 2; j >= 1 && sentMessages[before[i].w] && (is(before[i-1].w, "a") || is(before[i-1].w, "an")) {
			if objects[before[j].w] && !is(before[j].w, "you") && !is(before[j].w, "u") {
				j--
			}
			if j >= 0 && sendVerbs[before[j].w] && askLead(text, before, j) {
				return before[j].from, m[1], true
			}
		}
		if i >= 1 && is(before[i].w, "open") && (is(before[i-1].w, "are") || is(before[i-1].w, "r")) {
			i -= 2
		} else if i >= 1 && is(before[i].w, "open") && dmWords[before[i-1].w] &&
			(i == 1 || clauseOpening(text, before, i-1) == i-1) {
			// "dms open on discord", said as a notice.
			return before[i-1].from, m[1], true
		}
		if i >= 0 && dmWords[before[i].w] && i < n-1 {
			if i >= 1 && (is(before[i-1].w, "my") || is(before[i-1].w, "your") || is(before[i-1].w, "ur")) ||
				i >= 1 && dmVerbs[before[i-1].w] || i >= 2 && dmVerbs[before[i-2].w] || takenToDMs(before, i) {
				if len(after) > 0 && dmStatements[after[0].w] {
					return 0, 0, false
				}
				return before[max(0, i-2)].from, m[1], true
			}
		}
	}
	if len(after) > 0 && dmWords[after[0].w] && n > 0 && (is(before[n-1].w, "my") || is(before[n-1].w, "your") ||
		is(before[n-1].w, "ur")) {
		rest := after[1:]
		if len(rest) > 0 && dmStatements[rest[0].w] && !(len(rest) > 1 && is(rest[1].w, "open")) {
			return 0, 0, false
		}
		return before[n-1].from, after[0].to, true
	}
	return 0, 0, false
}

// takenToDMs reports the talk moved to the messages at before[i]: "take
// this to dms on insta", "let's move it to pms on discord".
func takenToDMs(before []aWord, i int) bool {
	if i < 2 || !movePreps[before[i-1].w] {
		return false
	}
	k := i - 2
	for n := 0; n < 2 && k >= 1 && (is(before[k].w, "this") || is(before[k].w, "it") || appDets[before[k].w] ||
		movedTalk[before[k].w]); n++ {
		k--
	}
	return moveVerbs[before[k].w]
}

var (
	dmWords      = canonSetOf(`dms dm inbox pms messages msgs`)
	dmVerbs      = canonSetOf(`slide into check send hit drop come`)
	dmStatements = canonSetOf(`are is r were was have has get got keep`)
	// sendVerbs ask for a message sent: "send me a dm on insta", "shoot me
	// a text on whatsapp".
	sendVerbs    = canonSetOf(`send drop shoot leave slide`)
	sentMessages = canonSetOf(`dm pm msg message text txt snap line`)
)

// hinglishAsk reads the Hinglish asks: the app, "pe", and a verb of coming
// or talking ("insta pe aaja baat karte", "telegram pe join karo",
// "discord pe aao na"); a verb, "kar" and the app ("dm kar insta pe"); the
// app's number or id asked for ("WhatsApp number do sab"); and "apna"
// before it ("apna gmail id bata").
func hinglishAsk(before, after []aWord, m []int) (int, int, bool) {
	n := len(before)
	if len(after) >= 2 && hindiOn[after[0].w] {
		for j := 1; j < min(len(after), 3); j++ {
			if hindiComeTalk[after[j].w] {
				if j+1 < len(after) && hindiPast[after[j+1].w] {
					return 0, 0, false
				}
				return m[0], after[j].to, true
			}
			// A bare "aa" is "come" only as an order: "whatsapp pe aa
			// jaldi", not "result telegram pe aa jayega".
			if is(after[j].w, "aa") && (j+1 == len(after) || hindiComeTails[after[j+1].w]) {
				return m[0], after[j].to, true
			}
		}
	}
	if len(after) > 0 && hindiOn[after[0].w] && n > 0 {
		i := n - 1
		if karWords[before[i].w] && i > 0 {
			i--
		}
		if hindiTalkVerbs[before[i].w] && (i < n-1 || n == 1 || !is(before[i].w, "baat")) {
			return before[i].from, after[0].to, true
		}
	}
	if len(after) >= 2 && idWords[after[0].w] && hindiGiveWords[after[1].w] {
		if is(after[1].w, "do") || is(after[1].w, "de") {
			if len(after) > 2 && englishAfterDo[after[2].w] {
				return 0, 0, false
			}
		}
		from := m[0]
		if n > 0 && hindiYour[before[n-1].w] {
			from = before[n-1].from
		}
		return from, after[1].to, true
	}
	if n > 0 && hindiYour[before[n-1].w] && len(after) > 0 {
		j := 0
		if idWords[after[0].w] {
			j++
		}
		if j < len(after) && hindiGiveWords[after[j].w] {
			return before[n-1].from, after[j].to, true
		}
	}
	return 0, 0, false
}

// appKaro reads the app's name made a verb with "karo", the Hinglish of
// "whatsapp me": "mujhe whatsapp karo", "doubt ho toh snap kar". Without
// "mujhe" it has to end its clause, so "whatsapp karo ya call karo, sab
// same hai" is not one; "maine whatsapp kar diya" is done already.
func appKaro(before, clause []aWord, ambiguous bool, m []int) (int, int, bool) {
	if ambiguous || len(clause) == 0 || !karWords[clause[0].w] {
		return 0, 0, false
	}
	j := 1
	for j < len(clause) && hindiKarTails[clause[j].w] {
		j++
	}
	n := len(before)
	me := n > 0 && hindiMe[before[n-1].w]
	if !me && j < len(clause) {
		return 0, 0, false
	}
	for _, w := range clause {
		if hindiPast[w.w] {
			return 0, 0, false
		}
	}
	from := m[0]
	if me {
		from = before[n-1].from
	}
	return from, clause[0].to, true
}

var (
	hindiMe       = canonSetOf(`mujhe muje mujhko mujko mereko mereku humein hume humko hamein`)
	hindiKarTails = canonSetOf(`do dena dijiye dijie lo lena na naa pls plz please bhai yaar jaldi abhi`)
)

var (
	hindiOn        = canonSetOf(`pe par pr pa`)
	hindiComeTalk  = canonSetOf(`aaja aaj aajao aao aana aaiye aajaiye join mil milo milte milna baat chat add follow dm msg message text ping connect judo judiye`)
	hindiPast      = canonSetOf(`kiya kia kiye diya diye gaya gayi gaye hua hui tha thi the`)
	karWords       = canonSetOf(`kar karo kr kro karna krna karlo karle krlo krle`)
	hindiTalkVerbs = canonSetOf(`dm msg message text ping add follow call baat contact connect`)
	idWords        = canonSetOf(`number no num nmbr id account username`)
	hindiGiveWords = canonSetOf(`do de dedo bhejo bhej batao bata bta dena share comment daalo dalo likho`)
	hindiComeTails = canonSetOf(`ja jaa jao jaldi na naa bhai yaar pls plz abhi`)
	hindiYour      = canonSetOf(`apna apni tera teri tumhara tumhari aapka aapki apka apki`)
	englishAfterDo = canonSetOf(`you u i we they it not this that the a your ur my he she`)
)

// setsUpAGroup reads a group or a call on the app asked for or offered:
// "can someone make a discord vc", "let's start a whatsapp group", "i'll
// create a telegram channel". "Our school made a whatsapp group" reports
// one.
func setsUpAGroup(before, after []aWord, m []int) (int, int, bool) {
	next := make([]string, len(after))
	for k, w := range after {
		next[k] = w.w
	}
	i := len(before) - 1
	end, rest := m[1], next
	switch {
	case len(next) > 0 && appGroups[next[0]]:
		// "make a whatsapp group"
		end, rest = after[0].to, next[1:]
	case i >= 1 && groupPreps[before[i].w] && appGroups[before[i-1].w]:
		// "make a gc on snap", "a group chat on whatsapp"
		i -= 2
		if i >= 0 && appGroups[before[i].w] && is(before[i+1].w, "chat") {
			i--
		}
	default:
		return 0, 0, false
	}
	if i >= 0 && (appDets[before[i].w] || is(before[i].w, "one") || is(before[i].w, "ek")) {
		i--
	}
	if i >= 1 && is(before[i].w, "up") && (is(before[i-1].w, "set") || is(before[i-1].w, "setting")) {
		i--
	}
	if i < 0 || !makeVerbs[before[i].w] && !madeVerbs[before[i].w] && !settingUp[before[i].w] {
		return 0, 0, false
	}
	k := i
	for k > 0 && askFillers[before[k-1].w] {
		k--
	}
	lead := func(j int) string {
		if j >= 0 {
			return before[j].w
		}
		return ""
	}
	verb := before[i].w
	switch {
	case madeVerbs[verb]:
		// Made already, and the reader asked to join: "made a discord
		// server for jee 2027, join", "i made a whatsapp group, dm to
		// join"; not "my mom made a whatsapp group".
		if !(k == 0 || is(lead(k-1), "i") || is(lead(k-1), "just") && k == 1) || !anyIn(next, joinWords) {
			return 0, 0, false
		}
	case settingUp[verb]:
		// Being made by the poster: "creating a whatsapp group for neet
		// aspirants", "i'm starting a discord server"; not "my school is
		// creating one".
		if !(k == 0 || is(lead(k-1), "i'm") || is(lead(k-1), "im") || is(lead(k-1), "am") && is(lead(k-2), "i")) {
			return 0, 0, false
		}
		// The subject of a sentence about it: "creating a discord server
		// is easy".
		if len(rest) > 0 && gerundVerbs[rest[0]] {
			return 0, 0, false
		}
	case k == 0:
	case groupAskers[lead(k-1)] || is(lead(k-1), "let's") || is(lead(k-1), "lets"):
	case canModals[lead(k-2)] && groupAskers[lead(k-1)],
		(is(lead(k-2), "can") || is(lead(k-2), "could")) && is(lead(k-1), "i"):
	case is(lead(k-1), "should") && is(lead(k-2), "we"):
	case is(lead(k-1), "i'll") || is(lead(k-1), "imma"):
	case is(lead(k-1), "wanna") && (k == 1 || groupAskers[lead(k-2)] || is(lead(k-2), "who")):
	case is(lead(k-1), "to") && (is(lead(k-2), "want") || is(lead(k-2), "wants")) &&
		(k == 2 || groupAskers[lead(k-3)] || is(lead(k-3), "who")):
		// "anyone want to start a telegram group", "who wants to make a
		// group chat on whatsapp"; not "my mom wants to make one".
	default:
		return 0, 0, false
	}
	return before[i].from, end, true
}

var (
	appGroups   = canonSetOf(`vc voice call group gc grp server chat channel room meet meeting community`)
	makeVerbs   = canonSetOf(`make create start set setup open host form`)
	madeVerbs   = canonSetOf(`made created started opened`)
	settingUp   = canonSetOf(`making creating starting setting opening hosting`)
	groupPreps  = canonSetOf(`on in over`)
	gerundVerbs = canonSetOf(`is was isn't wasn't takes took needs costs means seems sounds feels can could would
		will has`)
	// joinWords after a group made invite the reader in: "made a discord
	// server, join", "who wants in".
	joinWords   = canonSetOf(`join joining link dm hmu wants want in`)
	groupAskers = canonSetOf(`someone somebody anyone anybody you u we`)
)

// announcedOn reports the poster saying they are on the app, with an
// invitation to talk round it in the sentence: "i'm on discord if u wanna
// chat", "im on telegram, dm me", "dm me, im on insta", "i'm on snap every
// night if u wanna chat", not "i'm on discord too much" or "i'm on insta all
// day and nobody wants to talk". An offer to talk ("if anyone wants to talk")
// is turned down where the sentence keeps the talk here ("reply here", "this
// thread") or where the app is followed by how much the poster is on it and
// the talk by what it is about ("i'm on insta 6 hours a day, anyone else
// wanna talk about phone addiction?"): that is a post about the app, not an
// address on it. An invitation to message ("dm me", "hmu", "add me") is
// never turned down. It returns where the "i'm" starts.
func announcedOn(before, following []aWord) (int, bool) {
	n := len(before)
	if n < 2 || !is(before[n-1].w, "on") {
		return 0, false
	}
	k := n - 2
	if onAdverbs[before[k].w] {
		k--
	}
	switch {
	case k >= 0 && (is(before[k].w, "i'm") || is(before[k].w, "im")):
	case k >= 1 && is(before[k].w, "am") && is(before[k-1].w, "i"):
		k--
	default:
		return 0, false
	}
	direct, offer, topic := invitesTalk(before[:k])
	direct2, offer2, topic2 := invitesTalk(following)
	switch {
	case direct || direct2:
	case !offer && !offer2:
		return 0, false
	case talksHere(before) || talksHere(following):
		return 0, false
	case (topic || topic2) && appUsage(following):
		return 0, false
	}
	return before[k].from, true
}

// invitesTalk reads ws for an invitation to talk: direct, "dm me", "hmu",
// "add me"; or an offer, "if u wanna chat", "anyone wanna talk", and whether
// what it is about follows the offer ("anyone wanna talk about the syllabus").
func invitesTalk(ws []aWord) (direct, offer, topic bool) {
	asked := false
	for i, w := range ws {
		switch {
		case is(w.w, "hmu"):
			return true, false, false
		case inviteVerbs[w.w] && i+1 < len(ws) && (is(ws[i+1].w, "me") || is(ws[i+1].w, "us")):
			return true, false, false
		case talkInvites[w.w] && i > 0 && talkLeads[ws[i-1].w] && asked:
			offer = true
			// "talk about", "chat more about"
			topic = topic || i+1 < len(ws) && is(ws[i+1].w, "about") ||
				i+2 < len(ws) && is(ws[i+2].w, "about")
		}
		asked = asked || talkAskers[w.w]
	}
	return false, offer, topic
}

// talksHere reports ws keeping a talk on the board: "reply here", "this
// thread", "on here".
func talksHere(ws []aWord) bool {
	for _, w := range ws {
		if is(w.w, "here") || is(w.w, "thread") {
			return true
		}
	}
	return false
}

// appUsage reports how much the poster is on an app, in the words straight
// after its name: "a lot", "all day", "6 hours a day", "every night", "way too
// much", "less now", "only for the class channel".
func appUsage(ws []aWord) bool {
	i := 0
	for i < len(ws) && i < 2 && usageLeads[ws[i].w] {
		i++
	}
	if i >= len(ws) {
		return false
	}
	w, next := ws[i].w, ""
	if i+1 < len(ws) {
		next = ws[i+1].w
	}
	switch {
	case usageWords[w]:
		return true
	case is(w, "all") || is(w, "every") || is(w, "each"):
		return next != ""
	case is(w, "too") || is(w, "so"):
		return is(next, "much") || is(next, "often") || is(next, "long")
	case is(w, "only") || is(w, "just") || is(w, "mostly"):
		return is(next, "for") || is(next, "to")
	case w[0] >= '0' && w[0] <= '9':
		// "6 hours", "5hrs a day", "24/7"
		return usageUnits[next] || usageUnits[strings.TrimLeft(w, "0123456789+")]
	}
	return false
}

var (
	// usageLeads, usageWords and usageUnits say how much the poster is on an
	// app (appUsage).
	usageLeads = canonSetOf(`way like literally basically honestly still`)
	usageWords = canonSetOf(`lot alot lots constantly daily nightly everyday nonstop less more much often barely
		rarely hardly`)
	usageUnits  = canonSetOf(`hours hour hrs hr h mins min minutes 7 x7`)
	onAdverbs   = canonSetOf(`also always usually mostly active available online mainly`)
	inviteVerbs = canonSetOf(`dm pm msg message text ping add hit snap`)
	talkInvites = canonSetOf(`chat talk vc call`)
	talkLeads   = canonSetOf(`wanna to can lets let's`)
	talkAskers  = canonSetOf(`if anyone anybody someone somebody u you ya who`)
)

// handleAfterApp reads a handle a few lead words after the app: "snap pe
// aaja: madeup.0p", "follow karo insta pe: madeup_ki_kahani", "my 👻 is
// madeup.sprinkles", "threema id MADEUP42"; one spelled with "underscore"
// or "dot" after a separator ("telegram: madeup underscore jee"); and a
// quoted word after "search" where the sentence says an account is meant
// ("telegram id same as here, search 'madeupkotafactory'"). It returns where
// the handle ends.
func (f *scannedField) handleAfterApp(v *view, after []aWord, m []int, mail bool) (int, bool) {
	text := v.text
	j := 0
	for j < len(after) && j < 3 && (leadWords[isContracted(after[j].w)] || contractedIs(text, after, j, m[1])) {
		j++
	}
	// "=", "=>" and ">" after a mail word are code ("email = user.email",
	// "email => email.length"), as they are to mailColonHandle.
	// A pointing emoji or a "~" points at it too: "insta 👉 x.y", "snap ~
	// x_y".
	marks := ":=>|→~👉➡⬇👇"
	if mail {
		marks = ":|→"
	}
	separated := len(after) > j && strings.ContainsAny(text[m[1]:after[j].from], marks)
	// Reddit's "u/name": "my reddit is u/x_y", "reddit u/x_y".
	if j+1 < len(after) && is(after[j].w, "u") && strings.HasPrefix(text[after[j].to:], "/") &&
		after[j+1].from == after[j].to+1 {
		h := after[j+1]
		if a, b := v.span(h.from, h.to); plausibleHandle(f.src[a:b], false) {
			return h.to, true
		}
	}
	labelled := separated
	for _, w := range after[:j] {
		labelled = labelled || handleLabels[isContracted(w.w)]
	}
	if j < len(after) && (j > 0 || separated) {
		h := after[j]
		a, b := v.span(h.from, h.to)
		handle := f.src[a:b]
		if strings.Contains(handle[1:], "@") {
			return 0, false // an address, which the address rules judge
		}
		if strings.HasPrefix(text[h.to:], "(") {
			return 0, false // a call in code: "ig = ig_client(token)"
		}
		at := strings.HasPrefix(handle, "@")
		word := strings.ToLower(strings.TrimPrefix(handle, "@"))
		label, _, dotted := strings.Cut(word, ".")
		switch {
		case at && notHandles[word]:
		case at && mail && (dotted || mailProviders[label]):
			// A domain named with its "@", not a mailbox: "if your email
			// is @hotmail.com".
		case plausibleHandle(strings.TrimPrefix(handle, "@"), at) &&
			(labelled || at || !toolNames.MatchString(word) && !dottedAbbreviation(word)):
			if h.quoted && h.to < len(text) && strings.IndexByte(quoteMarks, text[h.to]) >= 0 {
				return h.to + 1, true // and the closing quote
			}
			return h.to, true
		case separated && j+2 < len(after) && spelledMarks[after[j+1].w]:
			return after[j+2].to, true
		}
	}
	for k, w := range after {
		if !searchWords[w.w] {
			continue
		}
		for _, q := range after[k+1 : min(len(after), k+3)] {
			if q.quoted && anyIn(wordsIn(text[m[0]:q.from]), accountWords) {
				if q.to < len(text) && strings.IndexByte(quoteMarks, text[q.to]) >= 0 {
					return q.to + 1, true // and the closing quote
				}
				return q.to, true
			}
		}
	}
	return 0, false
}

// isContracted is w without the "'s" of "is" on it: "handle's" is
// "handle".
func isContracted(w string) string {
	if s, ok := strings.CutSuffix(w, "'s"); ok {
		return s
	}
	if s, ok := strings.CutSuffix(w, "’s"); ok {
		return s
	}
	return w
}

// contractedIs reports after[j] as the "'s" of "is" on the word before it
// (or on the app, which ends at end): "my insta's @x", "ig handle's x".
func contractedIs(text string, after []aWord, j, end int) bool {
	if !is(after[j].w, "s") {
		return false
	}
	if j > 0 {
		end = after[j-1].to
	}
	gap := text[end:after[j].from]
	return gap == "'" || gap == "’"
}

var (
	leadWords = canonSetOf(`pe par pr pa aa aaja aao aajao join karo kar kr search me mujhe is hai h id username
		user userid handle naam name on as under follow add dm msg hmu mera meri my its it's at se`)
	spelledMarks = canonSetOf(`underscore dot period dash`)
	searchWords  = canonSetOf(`search find dhundo dhoondo lookup`)
	accountWords = canonSetOf(`id username handle account acc page profile naam`)
)

// otherFieldNamed reads a handle given as another field of the post: "my
// insta is my name", "snapchat = same as my name", "username is the subject
// of this post", "my email is my name here + @gmail.com". The field has to
// be filled in, and something has to say the app's account is it: a "is",
// an "=" or a ":", or "same as".
func (f *scannedField) otherFieldNamed(text string, after []aWord, m []int) (int, bool) {
	for k, w := range after {
		if k > 12 {
			break
		}
		// "my name", or "my username here", is the Name field.
		named := is(w.w, "name") || is(w.w, "naam") ||
			is(w.w, "username") && k+1 < len(after) && is(after[k+1].w, "here")
		if !named && !is(w.w, "subject") && !is(w.w, "title") || k == 0 {
			continue
		}
		if !pointerWords[after[k-1].w] {
			continue
		}
		if named && !f.nameShown || !named && !f.subjectShown {
			continue
		}
		between := text[m[1]:after[k-1].from]
		if strings.ContainsAny(between, "=:") || anyIn(wordsIn(between), linkWords) {
			return w.to, true
		}
	}
	return 0, false
}

// splitAddress reads an address given in two halves: the provider with an
// "@" and no name before it, and the name labelled apart, after it or before
// it: "[at] gmail [dot] com — prefix is madeup.quietstorm2007", "username:
// madeup.quietstorm2007, then @gmail.com". The name has to look like one
// (nameLike), since "prefix is your name" gives nothing away.
func splitAddress(text string, before, following []aWord, m []int) (int, int, bool) {
	at := addressAt(text, m[0])
	if at < 0 {
		return 0, 0, false
	}
	for k := 0; k+1 < len(following); k++ {
		if b, ok := labelledLocal(text, following, k); ok {
			return at, b, true
		}
	}
	var ws []aWord
	for _, w := range before {
		if w.to <= at {
			ws = append(ws, w)
		}
	}
	for k := 0; k+1 < len(ws); k++ {
		if _, ok := labelledLocal(text, ws, k); ok {
			return ws[k].from, m[1], true
		}
	}
	return 0, 0, false
}

// addressAt returns where the "@" before a provider's name at text[i] starts,
// when it has no name glued before it: "@gmail", "[at] gmail", "(@) gmail".
// It returns -1 otherwise, and for a bare "at", which is English.
func addressAt(text string, i int) int {
	back := func(j int) int {
		for j > 0 && text[j-1] == ' ' {
			j--
		}
		return j
	}
	j := back(i)
	switch {
	case j > 0 && text[j-1] == '@':
		j--
		if j > 0 && (inWord(rune(text[j-1])) || text[j-1] == '.') {
			return -1
		}
		return j
	case j > 0 && strings.IndexByte(")]}>", text[j-1]) >= 0:
		j = back(j - 1)
		switch {
		case j > 0 && text[j-1] == '@':
			j--
		case j > 1 && text[j-2:j] == "at" && (j == 2 || !inWord(rune(text[j-3]))):
			j -= 2
		default:
			return -1
		}
		if j = back(j); j > 0 && strings.IndexByte("([{<", text[j-1]) >= 0 {
			return j - 1
		}
	}
	return -1
}

// labelledLocal reads a name labelled as an address's first half from ws[k]
// on: a label, perhaps "is", "hai", ":" or "=", and the name.
func labelledLocal(text string, ws []aWord, k int) (int, bool) {
	if !localLabels[ws[k].w] {
		return 0, false
	}
	n := k + 1
	if n < len(ws) && (is(ws[n].w, "is") || is(ws[n].w, "hai")) {
		n++
	}
	if n >= len(ws) || n == k+1 && !strings.ContainsAny(text[ws[k].to:ws[n].from], ":=") && !is(ws[k].w, "prefix") {
		return 0, false
	}
	name := ws[n].w
	return ws[n].to, nameLike(name) && !versionShape.MatchString(name)
}

// localLabels name the half of an address before the "@".
var localLabels = canonSetOf(`prefix username user userid id handle`)

var (
	pointerWords = canonSetOf(`my the this mera meri`)
	linkWords    = canonSetOf(`is hai h same as`)
)

// talksThere reads talking "there" after an app or a link to it has been
// named, in the words ws after it, said as an ask or an offer: "instagram.com
// /p/… — this is my post, comment there so we can talk", "check my snap,
// i'll explain there".
func talksThere(text string, ws []aWord) (int, int, bool) {
	for t, w := range ws {
		if !is(w.w, "there") {
			continue
		}
		purpose := t+3 < len(ws) && is(ws[t+1].w, "so") && is(ws[t+2].w, "we") || t+1 < len(ws) &&
			(is(ws[t+1].w, "talk") || is(ws[t+1].w, "chat"))
		for k := t - 1; k >= max(0, t-3); k-- {
			if thereVerbs[ws[k].w] && (purpose || askMood(text, ws, k, true)) {
				return ws[k].from, w.to, true
			}
		}
	}
	return 0, 0, false
}

var thereVerbs = canonSetOf(`comment reply dm pm message msg text talk chat explain tell continue discuss reach find
	hmu meet ask`)

// offBoardCheck wants a verb of talking said as an ask before talking off
// the board: "anyone want to text instead of posting here", "let's chat
// off here". Talking "somewhere else" or "elsewhere" is as often another
// thread, so there the verb has to be a private one ("text", "dm"). The
// talk has to be with the poster or the reader, or about this talk ("dm me
// instead of posting here", "let's take this convo off here"): sending the
// reader to someone else ("talk to a counsellor instead of posting here",
// "call 1098 instead of posting here", "message the mods instead") is advice
// the rules want people to give.
func offBoardCheck(v *view, m []int) bool {
	text := v.text
	matched := wordsIn(text[m[0]:m[1]])
	ws, lo := readBack(text, m[0], 6, 64)
	v.work += m[0] - lo
	// "instead" on its own ends the ask: "can u text me instead?". Only a
	// private message to the poster or the reader, so "let's call the
	// office instead" and "talk to me instead" are not one.
	bare := len(matched) == 1 && is(matched[0], "instead")
	elsewhere := is(matched[0], "somewhere") || is(matched[0], "elsewhere")
	private := elsewhere || bare
	// Somewhere private is the point of the talk, so any verb of talking
	// will do: "let's talk somewhere private", "take this convo somewhere
	// more private".
	spot := is(matched[len(matched)-1], "private") && !is(matched[0], "go")
	after := nextWords(clauseAfter(text, m[1]), 3)
	withOther := len(after) > 1 && (is(after[0], "with") || is(after[0], "to")) && !objects[after[1]]
	switch {
	case is(matched[0], "go"):
		// "lets go private", said as a suggestion and ending there: not
		// "lets go private colleges" or "should i go private for the mri".
		with, lo := readBack(text, m[1], 8, 96)
		v.work += m[1] - lo
		k := len(with) - 2
		return k >= 0 && allIn(after, askTails) && askMood(text, with, k, false) && notNegated(v, m)
	case !elsewhere && !bare && withOther:
		// "privately", "in private", "1 on 1", but with no one else: not
		// "talk privately with my parents" or "we should talk in private
		// with the teacher".
		return false
	case elsewhere && !spot:
		// Somewhere else, as the end of the ask and not another part of
		// the board: "lets talk somewhere else", not "lets talk elsewhere
		// on the board" or "can we talk elsewhere? this thread is for
		// physics".
		private = !allIn(after, askTails) || anyIn(wordsIn(lineAround(text, m[0], m[1])), boardWords)
	}
	for k := len(ws) - 1; k >= max(0, len(ws)-4); k-- {
		w := ws[k].w
		if privateVerbs[w] || (!private || spot) && (talkVerbs[w] || is(w, "continue") || is(w, "move") ||
			is(w, "take")) || spot && is(w, "go") && weFrame(ws, k) {
			person := false
			for _, o := range ws[k+1:] {
				if !offBoardObjects[o.w] {
					return false
				}
				person = person || objects[o.w]
			}
			if bare && (!person || numberGiven(v, m)) {
				return false
			}
			return askMood(text, ws, k, true) && notNegated(v, []int{ws[k].from, m[1]})
		}
	}
	// Or the talk after it: "is there somewhere more private we could
	// talk?", "somewhere private where we can chat".
	if spot {
		next := nextWords(clauseAfter(text, m[1]), 4)
		if len(next) > 0 && is(next[0], "where") {
			next = next[1:]
		}
		if len(next) > 1 && (is(next[0], "we") || is(next[0], "you") || is(next[0], "u")) {
			next = next[1:]
			if canModals[next[0]] {
				next = next[1:]
			}
			return len(next) > 0 && (talkVerbs[next[0]] || privateVerbs[next[0]]) && notNegated(v, m)
		}
	}
	return false
}

// lineAround is the line round text[a:b], within 64 bytes either side: a
// sentence's dots may be a number's ("07.88.42.61.39.0").
func lineAround(text string, a, b int) string {
	before := text[max(0, a-64):a]
	after := text[b:min(len(text), b+64)]
	if j := strings.IndexByte(after, '\n'); j >= 0 {
		after = after[:j]
	}
	return before[strings.LastIndexByte(before, '\n')+1:] + text[a:b] + after
}

// numberGiven reports a number on the line round m: given with a private
// ask, it is the contact detail, which the number rules judge ("text me
// instead, 07700 900xxx" gives one of Ofcom's drama numbers, which no one can
// be reached on).
func numberGiven(v *view, m []int) bool {
	digits := 0
	for _, r := range lineAround(v.text, m[0], m[1]) {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits >= 7
}

// offBoardObjects may come between the verb and talking off the board: the
// poster or the reader, this talk, and the words an ask ends with. Anything
// else is someone or something else to talk to.
var offBoardObjects = canonSetOf(`me us you u ya yu each other to with this it that the our convo conversation
	chat chats discussion talk privately directly personally pls plz please instead now then rn later`)

var privateVerbs = canonSetOf(`text txt dm pm msg message call whatsapp snap`)

// boardWords name another part of the board, where talking somewhere else
// stays on it: "lets talk elsewhere, this thread is for physics".
var boardWords = canonSetOf(`thread threads board boards post posts subject topic op`)

// toolNames, with a version or a number after them, name a thing talked about
// on an app before it, not an account: "python3 on discord", "ps5 on reddit",
// "cs50 on discord", "jee2025 on reddit", "python3.12 on discord", "cs50x on
// reddit". handleBeforeApp reads them, where "x on <app>" is as often a topic
// as an account, and so does appLabelHandle when nothing but a space comes
// between the app and the word, where "ig" is as often "I guess" and "TG"
// Telangana ("ig python3 is better"); after "insta:" or "insta id" a handle is
// a handle. A spelledEmail reads them too: "Python3.12 at python.org".
var toolNames = regexp.MustCompile(`^(?:` + words(`python`, `py`, `java`, `node`, `php`, `perl`, `ruby`, `html`, `css`,
	`es`, `web`, `gpt`, `llama`, `ios`, `android`, `windows`, `win`, `macos`, `ubuntu`, `debian`, `fedora`, `ps`,
	`xbox`, `gta`, `fifa`, `fc`, `cod`, `mw`, `mp`, `cs`, `jee`, `neet`, `cuet`, `gate`, `cbse`, `icse`, `ipl`,
	`iphone`, `pixel`, `galaxy`, `redmi`, `vue`, `angular`, `react`, `dotnet`, `cpp`, `swift`, `kotlin`, `ue`,
	`unreal`, `excel`, `office`, `photoshop`, `overwatch`, `tekken`) +
	`)[-.]?\d{1,4}(?:\.\d{1,3})*[a-z]?$`)

// dottedAbbreviation reports a degree written with its points ("B.Tech",
// "M.Sc", "Ph.D", "LL.B"), or a numbered part of a text ("vol.1", "no.1",
// "ch.3", "Q.5"): words with a dot in them that are not someone's name.
func dottedAbbreviation(word string) bool {
	w := strings.ToLower(word)
	return strings.Contains(w, ".") && degreeNames[strings.ReplaceAll(strings.TrimSuffix(w, "."), ".", "")] ||
		numberedPart.MatchString(w)
}

var (
	degreeNames = setOf(`btech mtech be me bsc msc phd bed med bcom mcom ba ma bba mba bca mca barch march bpharm
		mpharm pharmd mbbs bds mds bams bhms llb llm mphil dphil bdes mdes bfa mfa bvoc beng meng bs ms bpt mpt
		psyd edd scd dsc`)
	numberedPart = regexp.MustCompile(`^(?:vol|no|nos|ch|chap|sec|q|ques|pg|p|pp|fig|eq|ex|art|para|pt|unit|lec|ed)` +
		`\.\d{1,4}[a-z]?$`)
	// ordinalWord is an ordinal and what it counts: "2nd-year", "3rd_sem".
	ordinalWord = regexp.MustCompile(`^\d{1,2}(?:st|nd|rd|th)(?:[-._][a-z]*)?$`)
)

// notHandles are words written with an "@" that address a group, not a
// person: "@everyone on discord".
var notHandles = setOf(`everyone here channel all mods mod admin admins op anon team`)

// Email addresses: the usual form, allowing spaces round the "@" and before
// ".com", a doubled "@@", a bracketed "[@]" or "[.]", and the Indian "at the
// rate"; the form with no top-level domain at a mail provider ("name@gmail",
// "name @ gmail", "name12 at gmail"); and the spelled-out form ("name at gmail
// dot com", "name [at] gmail [dot] com", "name@gmail_dot_com"). "at" and a
// site are ordinary English too ("look at youtube dot com", "Problem 4 at
// leetcode.com"), so a bare "at" needs more than the shape: spelledEmail and
// providerEmail ask for it.
var (
	emailRule = plainOnly(`\b([\pL\pN][\pL\pN._%+\-]{0,63})` + emailAt +
		`([\pL\pN][\pL\pN\-]{0,62}(?:` + emailInnerDot + `[\pL\pN][\pL\pN\-]{0,62})*)(\s?\.\s?|` + bracketedDot + `)` +
		`([a-z0-9]{2,24})\b`)
	providerEmailRule = plainOnly(`\b([\pL\pN][\pL\pN._%+\-]{0,63})(\s?@+\s?|\s*[(\[{<]\s*@\s*[)\]}>]\s?|\s+at\s+|` +
		atTheRate + `)` + words(`gmail`, `googlemail`, `yahoo`, `ymail`, `outlook`, `hotmail`, `icloud`, `protonmail`,
		`proton`, `rediffmail`, `yandex`, `aol`) + `\b`)
	spelledEmailRule = plainOnly(`\b([\pL\pN][\pL\pN._%+\-]{0,63})` + spelledEmailTail)
	// A name with its dots spelled too: "madeup dot riya at gmail dot
	// com". spelledEmailRule's match after "email me at" takes that "at"
	// for the address's, and goes on to "riya", and so misses it. The
	// name's first piece is a group of its own, a run before the rest
	// (filtersearch.go, "run"), so that a match is looked for from a
	// spelled "dot" after a name, not tried with every length of a name at
	// every letter: a post of "㏂", which reads as "a.m." over and over, all
	// one name, cost four times the steps and half as much time again
	// that way.
	spelledLocalEmailRule = plainOnly(`\b([\pL\pN][\pL\pN._%+\-]{0,63})((?:[\s_]+d[o0]t[\s_]+[\pL\pN][\pL\pN._%+\-]{0,63})+)` +
		spelledEmailTail)
)

// spelledEmailTail is a spelled-out address from its "at" on.
const spelledEmailTail = `\s*([(\[{<]\s*(?:at|@)\s*[)\]}>]|\s+at\s+|@+|` + atTheRate + `)\s*([\pL\pN][\pL\pN\-]{0,62}(?:` +
	spelledDot + `[\pL\pN][\pL\pN\-]{0,62})*)(` + spelledDot + `)([a-z]{2,24})\b`

const (
	atTheRate     = `\s*at~the~rate(?:~of)?\s*|attherate`
	emailAt       = `(?:\s?@+\s?|\s*[(\[{<]\s*@\s*[)\]}>]\s*|` + atTheRate + `)`
	bracketedDot  = `\s*[(\[{<]\s*\.\s*[)\]}>]\s*`
	emailInnerDot = `(?:\.|` + bracketedDot + `)`
	spelledDot    = `(?:\.|\s*[(\[{<]\s*(?:d[o0]t|\.)\s*[)\]}>]\s*|[\s_]+d[o0]t[\s_]+)`
)

var bracketedDotPattern = regexp.MustCompile(bracketedDot)

// bracketedDots turns "gmail[.]co[.]in" back into "gmail.co.in".
func bracketedDots(domain string) string {
	return bracketedDotPattern.ReplaceAllString(domain, ".")
}

// readTLD reads digits written for letters in a top-level domain ("c0m") as
// the letters, when that makes one the rules know.
func readTLD(tld string) string {
	if emailTLDs[tld] || strings.IndexFunc(tld, unicode.IsLetter) < 0 {
		return tld
	}
	for _, one := range []string{"i", "l"} {
		if read := strings.NewReplacer("0", "o", "1", one, "3", "e", "4", "a", "5", "s", "7", "t").Replace(tld); emailTLDs[read] {
			return read
		}
	}
	return tld
}

// emailTLDs are the top-level domains an address is recognised under, with
// academicSecondLevel's. An address anywhere else gets through, a deliberate
// gap: code is scanned, and "x@self.weight", "x @ w.to(dev)" or "user@host.sh"
// in a code block is not an email address.
var emailTLDs = setOf(`com net org edu gov mil int info biz co io ai me app dev xyz online site
	tech live email mail cloud in uk us ca au nz de fr es it nl be ch at se no dk fi ie pt ru ua cn jp
	kr sg my id ph th vn pk bd lk np ae sa qa za ng ke br mx ar cl tv cc ac ly gg fm`)

// academicSecondLevel reports a university's or a school's domain under a
// country's ("ogrenci.edu.tr", "mail.tau.ac.il"), which is recognised whatever
// the country.
func academicSecondLevel(domain, tld string) bool {
	if len(tld) != 2 || strings.IndexFunc(tld, func(r rune) bool { return r < 'a' || r > 'z' }) >= 0 {
		return false
	}
	_, sld, ok := strings.Cut(domain, ".")
	for ok {
		if !strings.Contains(sld, ".") {
			break
		}
		_, sld, ok = strings.Cut(sld, ".")
	}
	return ok && (sld == "edu" || sld == "ac" || sld == "sch" || sld == "k12")
}

// exampleDomain reports the domains RFC 2606 reserves for documentation and
// testing, which are no one's address.
func exampleDomain(domain string) bool {
	for _, d := range []string{"example.com", "example.net", "example.org"} {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	for _, tld := range []string{".test", ".example", ".invalid", ".localhost"} {
		if strings.HasSuffix(domain, tld) {
			return true
		}
	}
	return domain == "localhost"
}

// linkRule finds links to the hosts in contactLink. A dot may be bracketed
// ("discord(.)gg") or have a space either side of it. Hosts long enough not to
// turn up inside other words may also be written with "dot" ("discord dot
// gg/abc"), and on any host a slash may be written as a word ("t.me slash
// abc", "discord dot gg forward slash abc"). The path may start with a "#",
// as every link Signal hands out does ("signal.me/#eu/…", "signal.group/#…").
var linkRule = func() rule {
	short := func(host string) string {
		return strings.ReplaceAll(regexp.QuoteMeta(host), `\.`, `\s?(?:\.|[(\[]\.[)\]])\s?`)
	}
	long := func(host string) string {
		return strings.ReplaceAll(regexp.QuoteMeta(host), `\.`, `\s?(?:\.|[(\[]\.[)\]]|\(?dot\)?)\s?`)
	}
	hosts := words(
		long("discordapp.com"), long("discord.gg"), long("discord.com"), long("discord.me"), long("discord.io"),
		short("dsc.gg"), short("t.me"), long("telegram.me"), long("telegram.dog"), short("wa.me"),
		short("wa.link"), long("chat.whatsapp.com"), long("api.whatsapp.com"), long("web.whatsapp.com"),
		long("snapchat.com"), short("kik.me"),
		short("ig.me"), short("instagr.am"), long("instagram.com"), short("m.me"), short("fb.me"),
		long("facebook.com"), short("fb.com"), long("messenger.com"), long("signal.me"), long("signal.group"),
		short("line.me"), long("tiktok.com"), long("twitter.com"), short("x.com"), long("reddit.com"),
		long("join.skype.com"), long("linktr.ee"),
		// Calls and profiles: a meeting link is a call anyone reading can
		// join, and a LinkedIn profile is the person. A Zoom or Webex link's
		// own subdomain ("us05web.zoom.us", "acme.webex.com") needs no
		// reading: the host starts a word after its dot.
		short("zoom.us"), long("meet.google.com"), long("teams.microsoft.com"),
		long("teams.live.com"), long("meet.jit.si"), long("webex.com"), long("linkedin.com"))
	// Compiled as written: a path may contain "~", which phrase would read
	// as a separator.
	src := `\b(?:https?\W{0,3})?(?:www\s?\.\s?)?(` + hosts + `)` + linkSlash +
		`(#?[\pL\pN_@~%+.\-]+(?:` + linkSlash + `[\pL\pN_@~%+.\-]*)*)`
	return rule{plain: newSearch(src, famPlainOnly)}
}()

// linkSlash is a slash in a link, written or spelled: "/", " / ", " slash ",
// " forward slash ", "(/)".
const linkSlash = `(?:\s?/\s?|\s+(?:forward\s?)?slash\s+|\s?[(\[]\s?(?:/|slash)\s?[)\]]\s?)`

// linkPath reads a path found by linkRule with its slashes written as "/".
func linkPath(path string) string { return linkSlashPattern.ReplaceAllString(path, "/") }

var linkSlashPattern = regexp.MustCompile(linkSlash)

// linkedInPathRule is a LinkedIn profile's path given after the name in
// place of the host, "linkedin /in/x-y", "LinkedIn - in/x-y", or the
// profile's name in the shape LinkedIn gives it, a run of hyphened words
// ending in five or more letters and digits, "linkedin: x-y-1a2b3c". A vanity
// name alone ("my linkedin: x-y") is not read, since "linkedin:
// self-promotion" has the same shape, and nor is lnkd.in, whose links are to
// posts and jobs as often as to people.
var linkedInPathRule = plainOnly(`\blinked~?in\b(?:[^\pL\pN]{0,8}in[ \t]?/[ \t]?[\pL\pN][\pL\pN_\-]{2,}|` +
	`(?:~(?:profile|id|url))?(?:~is~|[ \t]*[:\-–=][ \t]*)\pL+(?:-\pL+)*-[\pL\pN]*\pN[\pL\pN]*\b)`)

// linkedInCheck wants a profile name's last part to be LinkedIn's, five or
// more letters and digits with a digit among them: "x-y-1a2b3c", not
// "covid-19".
func linkedInCheck(v *view, m []int) bool {
	match := v.text[m[0]:m[1]]
	if !strings.Contains(match, "/") {
		last := match[strings.LastIndexByte(match, '-')+1:]
		if len(last) < 5 || !strings.ContainsAny(last, "0123456789") {
			return false
		}
	}
	return !v.reported(m)
}

// spacedInviteRule is a Discord invite with its host spelled out letter by
// letter ("d i s c o r d . g g / abc"), which only the loose view closes up.
// Every Discord invite is a way to talk somewhere else, so it needs no
// contactLink check.
var spacedInviteRule = bothViews(`\bdiscord~(?:dot~)?gg[^\pL\pN/]*(?:/|(?:forward~?)?slash)[^\pL\pN]*[\pL\pN]{2,}`)

var (
	instagramPages = setOf(`p reel reels tv explore accounts about developer legal web`)
	facebookPages  = setOf(`groups watch share events photo photos story.php permalink.php reel marketplace
		help policies privacy login gaming pages hashtag search`)
	twitterPages = setOf(`home explore search i intent hashtag settings notifications login signup tos privacy`)
)

// contactLink decides whether a link opens a profile or an invite. A link to
// a post, a video or a page of the app's own is content, and passes, and so
// does a link to a page's anchor ("instagram.com/#about"), except on Signal,
// whose every link is an anchor.
func contactLink(host, path string) bool {
	host = strings.NewReplacer(" ", "", "(", "", ")", "", "[", "", "]", "", "dot", ".").Replace(host)
	host = strings.TrimPrefix(host, "www.")
	path = linkPath(path)
	if strings.HasPrefix(path, "#") {
		return host == "signal.me" || host == "signal.group"
	}
	var segs []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return false
	}
	first := segs[0]
	switch host {
	case "discord.gg", "dsc.gg", "discord.me", "discord.io", "t.me", "telegram.me", "telegram.dog",
		"wa.me", "wa.link", "chat.whatsapp.com", "kik.me", "ig.me", "m.me", "fb.me", "signal.me", "signal.group",
		"line.me", "join.skype.com", "linktr.ee":
		return true
	case "discord.com", "discordapp.com":
		return first == "invite" || first == "users"
	case "api.whatsapp.com", "web.whatsapp.com":
		return first == "send"
	case "snapchat.com":
		return first == "add" || first == "t" || strings.HasPrefix(first, "@")
	case "instagram.com", "instagr.am":
		return first == "direct" || !instagramPages[first]
	case "facebook.com", "fb.com":
		return first == "messages" || !facebookPages[first]
	case "messenger.com":
		return first == "t"
	case "tiktok.com":
		return strings.HasPrefix(first, "@") && len(segs) == 1
	case "twitter.com", "x.com":
		return len(segs) == 1 && !twitterPages[first]
	case "reddit.com":
		return (first == "u" || first == "user") && len(segs) == 2
	case "zoom.us":
		// A meeting ("/j/"), a webinar ("/w/") or a meeting started ("/s/")
		// by its number, or a personal room ("/my/") by its name, and not
		// "zoom.us/download" or a "zoom.us/j/" with no meeting after it.
		// The web client's join page names it too: "/wc/join/<number>"
		// or "/wc/<number>/join".
		switch {
		case len(segs) < 2:
			return false
		case first == "j" || first == "w" || first == "s":
			return segs[1][0] >= '0' && segs[1][0] <= '9'
		case first == "wc" && len(segs) > 2 && segs[1] == "join":
			return segs[2][0] >= '0' && segs[2][0] <= '9'
		case first == "wc" && len(segs) > 2 && segs[2] == "join":
			return segs[1][0] >= '0' && segs[1][0] <= '9'
		}
		return first == "my"
	case "meet.google.com":
		// A meeting's code, xxx-xxxx-xxx or its ten letters without the
		// dashes, or a meeting looked up by its nickname ("/lookup/…",
		// Classroom's links), not "meet.google.com/new" or "/landing".
		return meetCode.MatchString(first) || meetBareCode.MatchString(first) && !meetPages[first] ||
			first == "lookup" && len(segs) > 1
	case "teams.microsoft.com":
		// The old "/l/meetup-join/…" link, or the "/meet/<number>" Teams
		// shares a meeting by now.
		if first == "meet" && len(segs) > 1 {
			return segs[1][0] >= '0' && segs[1][0] <= '9'
		}
		return first == "l" && len(segs) > 1 && teamsJoins[segs[1]]
	case "teams.live.com":
		return first == "meet" && len(segs) > 1
	case "meet.jit.si":
		return true
	case "webex.com":
		// A personal room ("/meet/" or "/join/" and its name), a meeting or an
		// event by its site's "j.php" or "e.php" anywhere after the site's
		// name ("/acme/j.php", Events' and Training's "/acme/k2/j.php"; the
		// path stops before "?MTID=…"), or the join service's page for a
		// meeting; not "webex.com/downloads" or a help page.
		switch {
		case first == "meet" || first == "join":
			return len(segs) > 1
		case len(segs) > 1 && (slices.Contains(segs[1:], "j.php") || slices.Contains(segs[1:], "e.php")):
			return true
		}
		return (first == "wbxmjs" || first == "webappng") && slices.Contains(segs, "meeting")
	case "linkedin.com":
		return (first == "in" || first == "pub") && len(segs) > 1
	}
	return false
}

var (
	meetCode     = regexp.MustCompile(`^[a-z]{3}-[a-z]{4}-[a-z]{3}$`)
	meetBareCode = regexp.MustCompile(`^[a-z]{10}$`)
	meetPages    = setOf(`whiteboard`)
	teamsJoins   = setOf(`meetup-join meet chat`)
)

// niRule finds UK National Insurance numbers: two prefix letters, three pairs
// of digits and a suffix A-D, spaced or not.
var niRule = plainOnly(`\b([a-z])([a-z])[ \-]?(\d\d)[ \-]?(\d\d)[ \-]?(\d\d)[ \-]?([a-d])\b`)

// validNIPrefix applies HMRC's rules for the two prefix letters, which rule
// out, among others, the QQ of the number HMRC prints in its own examples.
func validNIPrefix(a, b byte) bool {
	if strings.IndexByte("dfiquv", a) >= 0 || strings.IndexByte("dfiquvo", b) >= 0 {
		return false
	}
	switch string([]byte{a, b}) {
	case "bg", "gb", "kn", "nk", "nt", "tn", "zz":
		return false
	}
	return true
}

// selfHurt is hurting oneself, as the poster says it of themselves, and
// jumpOff is one way of dying that is said of oneself and of others alike.
// crisisActs are what a crisis statement wants, plans or wishes, in the
// base form ("i used to want to die", "i wish i could disappear forever");
// crisisGerunds what it feels like or thinks about ("i feel like ending it
// all", "thinking about not existing"); and gone what "if i …" imagines
// ("if i wasn't alive", "if i killed myself"), which counterfactualCheck
// wants other people's reaction to.
var (
	selfHurt = words(`hurt~myself`, `cut~myself`, `harm~myself`, `self~harm`, `starve~myself`, `burn~myself`)
	// methodActs name a way of dying, or an overdose: "hang myself", "slit
	// my wrists", "take all my pills", "jump in front of a train".
	// supportCheck reads them for idioms ("drown myself in coffee", "shoot
	// myself a reminder", "overdose on caffeine") and routines ("take all my
	// pills before breakfast").
	methodActs = words(words(`hang`, `drown`, `poison`, `shoot`, `stab`, `electrocute`)+`~my~self`, `slit~my~wrists?`,
		`overdose`, `od`, `suicide`, `take~all~(?:of~)?my~`+words(`pills`, `meds`, `tablets`, `medicines?`, `medications?`,
			`sleeping~pills`), `jump~in~front~of~(?:a|the)~`+words(`train`, `bus`, `car`, `truck`, `lorry`, `metro`))
	crisisActs = words(`die`, `be~dead`, `not~be~alive`, `not~exist`, `not~wake~up`, `never~wake~up`,
		`(?:go~to~)?sleep~and~`+words(`never`, `not`)+`~wake~up`, `not~be~here`, `disappear`, `end~it~all`, `end~my~life`, `end~it`, `end~myself`,
		`end~everything`, `kill~my~self`, `stop~existing`, `commit~suicide`, `take~my~(?:own~)?life`, selfHurt, methodActs,
		`(?:go~to~)?sleep~forever`, `cut`)
	crisisGerunds = words(words(`hurting`, `cutting`, `harming`, `killing`, `starving`, `burning`, `hanging`, `drowning`,
		`poisoning`, `shooting`, `stabbing`)+`~my~self`,
		`ending~`+words(`it~all`, `it`, `my~life`, `everything`, `things`, `myself`), `taking~my~(?:own~)?life`,
		`suicide`, `dying`, `not~existing`, `not~being~alive`, `not~waking~up`, `overdosing`, `slitting~my~wrists?`,
		`taking~all~my~`+words(`pills`, `meds`, `tablets`))
	gone = words(`died`, `was~dead`, `were~dead`, `was~gone`, `were~gone`, words(`wasn'?t`, `weren'?t`, `was~not`,
		`were~not`)+`~`+words(`alive`, `here`, `around`, `born`), words(`didn'?t`, `did~not`)+`~`+words(`exist`,
		`wake~up`), `never~existed`, `never~woke~up`, `stopped~existing`, `disappeared`, `killed~my~self`,
		`ended~it(?:~all)?`, `ended~my~life`, `took~my~(?:own~)?life`, `was~never~born`, `had~never~been~born`)
	// plannedActs are what the poster says they are going to, planning to
	// or have decided to do: "i'm going to kill myself", "planning to end
	// my life", "decided to end it tonight".
	plannedActs = words(`kill(?:ing)?~my~self`, `end(?:ing)?~it~all`,
		`end(?:ing)?~my~life`, `end(?:ing)?~myself`, `end(?:ing)?~it`, `commit(?:ting)?~suicide`, `suicide`,
		`tak(?:e|ing)~my~(?:own~)?life`, `hurt(?:ing)?~myself`, `cut(?:ting)?~myself`, `overdos(?:e|ing)`,
		`jump(?:ing)?~off~(?:a|the)~`+words(`bridge`, `building`, `roof`, `cliff`),
		words(`hang(?:ing)?`, `drown(?:ing)?`, `poison(?:ing)?`, `shoot(?:ing)?`, `stab(?:bing)?`)+`~my~self`,
		`slit(?:ting)?~my~wrists?`, `tak(?:e|ing)~all~my~`+words(`pills`, `meds`, `tablets`), `od`)
	// hinglishActs are ending one's life, and hinglishIntents wanting, meaning
	// or being about to do it: "apni life end kar dunga", "life khatam kar
	// dunga", "zindagi khatam karna chahta hu", "suicide kar lu", "khudkushi
	// karne ka mann hai".
	hinglishActs = words(`(?:apni~)?life~`+words(`end`, `khatam`, `khatm`, `khtm`), `(?:apni~)?`+words(`zindagi`,
		`zindgi`, `jindagi`)+`~`+words(`khatam`, `khatm`, `khtm`, `end`), `suicide`, `khud~?kushi`, `khud~?khushi`)
	hinglishIntents = words(words(`kar`, `kr`)+`~`+words(`dunga`, `dungi`, `doonga`, `doongi`, `lunga`, `lungi`,
		`loonga`, `loongi`, `lu`, `loon`, `lun`, `lena~hai`), words(`karna`, `krna`)+`~`+words(`chahta`, `chahti`,
		`chahte`, `hai`, `h`), words(`karne`, `krne`)+`~`+words(`ka`, `ki`)+`~`+words(`mann`, `man`, `dil`,
		`ichha`, `iccha`, `irada`))
	// drinkBleach is "drink bleach", "drink some bleach", "drink a glass of
	// bleach".
	drinkBleach = `drink~(?:` + words(`some`, `a~`+words(`cup`, `glass`, `bottle`, `mug`)+`~of`) + `~)?bleach`
	// fatInsults, after "stop eating", make it an insult rather than advice.
	fatInsults = []string{`fatty`, `fatso`, `fatass`, `fat~ass`, `piggy`, `pig`, `lardass`, `you~fat~` +
		words(`pig`, `cow`, `slob`, `fuck`)}
	// jumpOff is jumping, or a dive or a flip, off somewhere high, with up
	// to two words naming it: "the hostel roof", "the 4th floor".
	jumpOff = words(`jump`, `leap`, `dive`, `flip`, `backflip`, `do~a~flip`, `do~a~backflip`, `swan~dive`) +
		`~off~(?:of~)?` + words(`a`, `the`, `your`, `ur`, `that`) + `~(?:[\pL\pN]+~){0,2}?` + words(`bridge`,
		`building`, `roof`, `rooftop`, `cliff`, `terrace`, `balcony`, `tower`, `floor`, `ledge`)
)

// supportRules are crisis language in the poster's own voice, in English and
// in some Hinglish. Second-person versions ("kill yourself") are not here:
// those are encouragement, a hold. supportCheck turns down the matches that
// are something else in context: a distance in "kms", a hostel in "I don't
// want to live in the hostel", an idiom in "die of embarrassment".
var supportRules = []rule{bothViews(`\b` + words(
	`i~(?:(?:really|just|honestly|kinda|actually|literally|seriously|so)~)*`+
		words(`want`, `wanna`, `wana`, `wish`, `need`)+`~(?:(?:to|2)~)?`+words(crisisActs, jumpOff),
	// Wherever the "i" is, in any tense, which ownSubject reads: "I hate
	// myself and want to self harm", "i used to want to die and now i do
	// again", "still want to die tbh". Others wanting it are aboutSomeoneElse.
	words(`want`, `wanted`, `wanting`, `wanna`, `wana`, `need`, `wish`, `wished`, `wishing`)+`~(?:(?:to|2)~)?`+crisisActs,
	`i~wish~i~`+words(`could`, `would`, `can`, `'?d`)+`~(?:just~)?`+words(crisisActs, `never~been~born`),
	`i~`+words(`'?m`, `am`, `m`)+`~(?:`+words(`so`, `just`, `honestly`, `really`)+`~)?ready~to~die`,
	`i~(?:'?m|am|m)?~`+words(`going~to`, `gonna`, `gunna`, `about~to`, `ready~to`, `thinking~about`,
		`thinking~of`, `planning~to`, `planning~on`)+`~`+plannedActs,
	// The same plan with the "I" left out, which ownSubject reads: "planning
	// to end my life", "i've decided to end my life", "decided to end it
	// tonight"; and the plan named: "i'm planning my suicide".
	words(`planning~to`, `planning~on`, `planned~to`, `decided~to`, `deciding~to`, `decided~i'?m~going~to`,
		`made~up~my~mind~to`)+`~`+plannedActs,
	words(`planning`, `planned`, `plotting`)+`~my~(?:own~)?suicide`,
	// "i should just die", "i deserve to die", "i don't deserve to live".
	`i~(?:`+words(`really`, `honestly`, `literally`, `seriously`)+`~)?`+words(`should`, `shud`, `shld`, `deserve~to`)+
		`~(?:just~)?die`,
	`i~`+words(`don'?t`, `dont`, `do~not`)+`~(?:even~)?deserve~to~`+words(`live`, `be~alive`, `exist`),
	// "i just want it all to end", "i want my life to end": supportCheck
	// turns down a sentence about exams or an essay ending. A bare "want
	// it to stop" is as often a headache, the rain or the drilling next
	// door, and is not read.
	words(`want`, `wanted`, `wanna`, `need`, `wish`)+`~`+words(`it~all`, `everything`, `my~life`, `this~life`,
		`all~of~(?:this|it)`)+`~to~`+words(`end`, `stop`, `be~over`),
	// "having thoughts of suicide", "suicide is the only option for me".
	words(`having`, `have`, `had`, `get`, `getting`, `got`, `keep~having`)+`~(?:`+words(`these`, `the`, `some`,
		`serious`, `constant`, `bad`, `dark`)+`~)?thoughts~`+words(`of`, `about`)+`~`+words(`suicide`,
		`killing~my~self`, `ending~my~life`, `ending~it(?:~all)?`, `dying`),
	`suicide~`+words(`is`, `'?s`, `seems~like`, `feels~like`, `looks~like`)+`~(?:`+words(`the`, `my`)+`~)?only~`+
		words(`option`, `way~out`, `way`, `answer`, `choice`, `solution`, `escape`),
	// "i give up on life", "giving up on living".
	words(`give`, `gave`, `given`, `giving`)+`~up~on~`+words(`life`, `living`),
	`i~`+words(`'?ll`, `will`, `wil`, `shall`, `might`, `may`, `could`)+`~(?:just~)?`+
		words(`kill~my~self`, `end~it~all`, `end~my~life`, `commit~suicide`, `take~my~(?:own~)?life`, methodActs),
	// Hoping or praying not to live: "i hope i die in my sleep", "i pray i
	// don't wake up", "hope i get hit by a bus", "can't wait to die", "i'd
	// rather be dead", "i want death", "praying for death", "life isn't
	// worth living". supportCheck reads them for an exam or a joke round
	// them, a comparison ("rather be dead than wear that") and a death that
	// names something ("Death Note").
	`(?:i~)?`+words(`hope`, `pray`, `praying`, `hoping`)+`~(?:that~)?i~`+words(`die`, `don'?t~wake~up`,
		`do~not~wake~up`, `never~wake~up`, `get~hit~by~(?:a|the)~`+words(`bus`, `car`, `truck`, `train`, `lorry`)),
	words(`can'?t`, `cannot`)+`~wait~to~die`,
	`i~(?:'?d|would)~rather~`+words(`be~dead`, `die`, `not~exist`, `not~be~alive`, `not~be~here`),
	`i~`+words(`want`, `wanna`, `need`, `crave`)+`~death`,
	words(`praying`, `pray`, `wishing`, `begging`)+`~for~(?:my~)?death`,
	`(?:my~)?life~`+words(`isn'?t`, `is~not`, `ain'?t`, `'?s~not`)+`~worth~`+words(`living`, `livin`, `it`),
	// An overdose just taken: "just took 20 paracetamol", "i swallowed a
	// whole strip of pills", "took too many pills". supportCheck wants the
	// poster as its subject, ten or more if a number is given, and no
	// headache, doctor or meal round it.
	words(`took`, `taken`, `swallowed`, `downed`, `popped`)+`~(?:like~)?`+words(`all~(?:of~)?my`, `all~the`,
		`too~many`, `so~many`, `a~(?:whole~)?`+words(`bunch`, `lot`, `handful`, `strip`, `bottle`, `box`, `pack`,
			`packet`)+`~of(?:~my)?`, `(?:an?~)?`+words(`whole`, `entire`)+`~`+words(`strip`, `bottle`, `box`, `pack`,
			`packet`)+`~of(?:~my)?`, `\d+`)+`~(?:`+words(`sleeping`, `prescription`)+`~)?`+medicines,
	// "i'm ending it tonight", "gonna end it soon": supportCheck wants the
	// poster as its subject and a time or nothing after it, and no essay
	// or series being ended.
	`ending~it(?:~all)?`, `gonna~`+words(`end~it(?:~all)?`, `kill~my~self`, methodActs),
	`i~`+words(`'?ll`, `will`)+`~(?:just~)?`+words(`end~it(?:~all)?`, `end~my~life`),
	// A suicide note, and "by the time you read this i'll be gone".
	words(`written`, `wrote`, `writing`, `write`, `finished`, `left`, `leaving`)+`~my~suicide~`+words(`note`, `letter`),
	`my~suicide~`+words(`note`, `letter`)+`~`+words(`is`, `'?s`)+`~`+words(`ready`, `done`, `written`, `finished`),
	`by~the~time~`+words(`you`, `u`, `anyone`, `someone`, `somebody`, `people`)+`~`+words(`read`, `reads`, `see`,
		`sees`, `find`, `finds`)+`~this~i~`+words(`'?ll`, `will`)+`~be~`+words(`gone`, `dead`),
	// A relapse: "i relapsed on self harm last night", "3 months clean from
	// sh and relapsed" (supportCheck wants a relapse in the post).
	`relapsed~`+words(`on`, `into`, `with`, `back~into`)+`~`+words(`self~harm(?:ing)?`, `sh`, `cutting`),
	`clean~`+words(`from`, `of`, `off`)+`~`+words(`sh`, `self~harm(?:ing)?`, `cutting`),
	// Cutting and an attempt told with a time: "i cut last night", "been
	// cutting for 2 years", "i attempted last month".
	`i~cut~`+words(`last~night`, `yesterday`, `this~morning`, `earlier`),
	`been~cutting~`+words(`for`, `since`),
	`i~(?:`+words(`'?ve`, `have`)+`~)?attempted~`+words(`last`, `yesterday`, `twice`, `once`, `before`, `again`,
		`\d+~times`),
	words(`kill(?:ing)?`, `unaliv(?:e|ing)`, `top(?:ping)?`)+`~my~self`,
	`kms`,
	// A past attempt: "I tried to end it last weekend", "i overdosed last
	// month". supportCheck reads "end it" with a past time after it, and
	// "overdosed on" only with medicine.
	`i~`+words(`tried`, `attempted`)+`~to~`+words(`end~it(?:~all)?`, `end~my~life`, `kill~my~self`,
		`take~my~(?:own~)?life`, `overdose`, `hang~my~self`),
	`i~(?:`+words(`'?ve`, `have`, `had`)+`~)?`+words(`attempted~suicide`, `overdosed`),
	// "i don't think i'll make it to 18": supportCheck wants an age or a
	// year after it, and no school work.
	`(?:i~)?`+words(`don'?t`, `dont`, `do~not`)+`~think~i~`+words(`'?ll`, `will`, `wil`)+`~`+words(`make~it`, `live`,
		`survive`, `last`)+`~`+words(`to`, `till`, `until`, `past`),
	// "this is my last post", with goodbyes round it (supportCheck).
	`this~`+words(`is`, `will~be`)+`~my~last~`+words(`post`, `message`, `thread`),
	// "I have a plan and a date", with nothing after it.
	`i~(?:`+words(`'?ve`, `have`)+`~)?(?:got~)?(?:have~)?a~plan~and~a~date`,
	// "i'm on the roof of my building rn", in a post about jumping
	// (supportCheck).
	`i~`+words(`'?m`, `am`, `m`)+`~(?:`+words(`standing`, `sitting`)+`~)?on~`+words(`the`, `a`, `my`)+
		`~(?:\pL+~){0,2}?`+words(`roof`, `rooftop`, `terrace`, `ledge`, `bridge`, `edge`),
	// Nigerian Pidgin: "I don tire for this life", "I wan die", "make I just
	// die", "I no wan live again", "life don tire me", "I dey cut myself".
	`i~(?:don~)?tire~for~(?:this~)?`+words(`life`, `world`),
	`i~(?:just~)?wan~die`, `make~i~(?:just~)?die`, `i~no~`+words(`wan`, `want`)+`~live`, `life~don~tire~me`,
	`i~dey~`+words(`cut`, `hurt`, `harm`, `kill`)+`~my~self`,
	// "the urge to cut is so strong tonight": supportCheck turns down an
	// object after "cut" ("the urge to cut my hair").
	words(`urge`, `urges`)+`~to~`+words(`cut`, `self~harm`, `sh`, `hurt~myself`, `relapse`),
	// "i'm feeling suicidal", "been feeling so suicidal", "feeling
	// suicidal tonight": with the "I" left out, ownSubject reads it.
	`(?:i~)?`+words(`'?m`, `am`, `m`, `was`, `feel`, `feeling`, `felt`, `been`, `'?ve~been`, `have~been`, `get`, `got`,
		`getting`, `keep~feeling`, `still~feel`, `still~feeling`)+`~(?:`+words(`feeling`, `getting`, `been~feeling`)+
		`~)?(?:(?:so|really|very|kinda|pretty|a~bit|super|extremely|quite|lowkey)~)?suicidal`,
	`suicidal~`+words(`thoughts`, `feelings`, `again`, `ideation`),
	`(?:i~)?`+words(`don'?t`, `do~not`, `dont`, `dnt`)+`~(?:really~)?`+words(`want~to`, `wanna`, `wana`)+`~`+
		words(`be~alive`, `live`, `exist`, `be~here~anymore`, `wake~up`),
	`i~(?:'?m~|am~|m~|'?ve~|have~)?(?:`+words(`been`, `keep`, `kept`, `still`, `started`, `start`)+`~)?`+
		words(`cutting`, `cut`, `burning`, `burned`, `burnt`, `hurting`, `harming`, `starving`)+`~myself`,
	// "I started cutting again", "I've been cutting again".
	`i~(?:'?m~|am~|m~|'?ve~|have~)?(?:`+words(`been`, `keep`, `kept`, `still`, `started`, `start`, `back~to`)+
		`~)?`+words(`cutting`, `hurting~myself`, `harming~myself`)+`~again`,
	`i~`+words(`'?m`, `am`, `m`)+`~(?:`+words(`so`, `really`, `just`, `honestly`, `literally`)+`~){0,2}`+
		words(`tired`, `sick`, `exhausted`)+`~of~`+words(`being~alive`, `living`, `life`, `existing`),
	`i~(?:`+words(`'?ve`, `have`)+`~)?(?:`+words(`always`, `often`, `really`, `just`)+`~)?`+
		words(`wanted`, `wished`)+`~(?:to~)?`+words(`die`, `be~dead`, `not~be~alive`, `not~exist`, `disappear`),
	// "I stand at the balcony and think about jumping": supportCheck wants
	// somewhere high in the sentence, or "off" after it.
	words(`think`, `thinking`, `thought`)+`~`+words(`about`, `of`)+`~jumping`,
	`i~(?:'?ve~been~|have~been~|keep~|still~)?self~harm(?:ing)?`,
	words(`feel`, `feeling`, `felt`)+`~like~`+crisisGerunds,
	words(`thinking`, `think`, `thought`)+`~`+words(`about`, `of`)+`~`+crisisGerunds,
	words(`considering`, `contemplating`)+`~`+crisisGerunds,
	words(`done`, `finished`)+`~with~`+words(`life`, `living`),
	// "no reason to live", "i don't see any reason to keep living", "what's
	// the point of going on": with "any", "a" or "the", only after a "not"
	// or a "what's" (futile).
	words(`no`, `nothing`, `any`, `a`, `the`, `zero`)+`~(?:`+words(`real`, `good`, `single`)+`~)?`+
		words(`reason`, `reasons`, `point`, `purpose`)+`~(?:left~)?(?:for~me~)?(?:`+words(`to`, `in`, `of`)+`~)?(?:`+
		words(`keep`, `keep~on`, `continue`, `go~on`, `carry~on`, `even`)+`~)?`+words(`live`, `living`, `go~on`,
		`going~on`, `carry~on`, `carrying~on`, `be~alive`, `being~alive`, `stay~alive`, `staying~alive`,
		`exist`, `existing`, `wake~up`, `waking~up`, `keep~going`),
	// "if i died nobody would care", "everyone would be happier if i just
	// wasn't alive anymore": counterfactualCheck wants the others' reaction.
	`if~i~(?:`+words(`just`, `simply`, `ever`, `suddenly`, `really`)+`~)?`+gone,
	// "the easiest way out is to just not be alive anymore", "the only way
	// out for me is to end it": the poster's, unless the sentence names
	// someone else and not the poster.
	`way~out~(?:for~me~)?`+words(`is`, `was`, `would~be`, `'?s`, `seems~to~be`, `might~be`)+`~(?:to~)?(?:just~)?`+
		words(crisisActs, `death`, `dying`, `suicide`),
	// "I've been saving up pills", "been hoarding paracetamol for weeks":
	// ownSubject wants the poster to be the one doing it.
	words(`saving`, `saved`, `stockpiling`, `stockpiled`, `hoarding`, `hoarded`, `stashing`, `stashed`,
		`collecting`, `collected`)+`~(?:up~)?(?:`+words(`my`, `all~my`, `some`, `a~few`, `enough`, `extra`, `the`,
		`these`, `\d+`)+`~)?(?:`+words(`old`, `leftover`, `spare`, `prescription`)+`~)?`+medicines,
	// Hurting oneself in any tense, with or without the "I": "hurt myself
	// last night", "I'm self harming again", "relapsed and cut again". The
	// matches with an "I" before them are found from it by the rules above.
	words(`hurt`, `hurting`, `harmed`, `harming`, `harm`, `cut`, `cutting`, `burned`, `burnt`, `burning`, `burn`,
		`starved`, `starving`, `starve`)+`~my~self`,
	`self~harm(?:ing|ed)`,
	words(`cutting`, `cut`)+`~again`,
	// "i've been cutting my wrists again", "i cut my arm last night":
	// supportCheck wants the poster doing it, and no accident.
	words(`cut`, `cutting`)+`~my~`+words(`wrists?`, `arms?`, `thighs?`, `legs?`,
		`skin`, `veins?`, `stomach`, `hips?`, `forearms?`, `ankles?`),
	// "i scratch myself till i bleed", "i bang my head against the wall till
	// it bleeds", "i hit myself when i'm stressed", "i scratch my arms till
	// they bleed": supportCheck wants bleeding, bruises or distress in the
	// sentence (bleeding or bruises for a part of the body, and no itch) and
	// no accident.
	`i~(?:`+words(`'?ve~been`, `have~been`, `been`, `keep`, `kept`, `still`, `sometimes`, `always`, `often`, `just`,
		`started`, `start`)+`~)?`+words(`scratch`, `scratched`, `scratching`, `hit`, `hitting`, `punch`, `punched`,
		`punching`, `bang`, `banged`, `banging`, `slap`, `slapped`, `slapping`)+`~`+words(`my~self`, `myself`,
		`my~head`, `my~`+words(`arms?`, `wrists?`, `legs?`, `thighs?`, `skin`, `stomach`, `face`)),
	// "i make myself throw up after meals".
	words(`make`, `made`, `making`)+`~my~self~`+words(`throw~up`, `vomit`, `puke`, `sick`)+`(?:~after~`+
		words(`meals?`, `eating`, `food`, `dinner`, `lunch`, `breakfast`, `i~eat`)+`)?`,
	`nothing~(?:left~)?to~live~for`,
	`better~off~dead`,
	`i~wish~i~`+words(`was`, `were`)+`~dead`,
	`i~wish~i~`+words(`wasn'?t`, `weren'?t`, `was~never`, `were~never`, `had~never~been`, `hadn'?t~been`)+
		`~`+words(`born`, `alive`),
	words(`everyone`, `everybody`, `everyone~else`, `the~world`, `this~world`, `my~family`, `my~parents`,
		`my~friends`, `my~mom`, `my~mum`, `my~dad`, `my~class`)+`~`+words(`would`, `will`, `'?d`, `'?ll`, `is`, `are`,
		`'?s`)+`~(?:all~)?(?:be~)?(?:`+words(`so~much`, `much`, `a~lot`, `way`)+`~)?`+words(`better~off`, `better`,
		`happier`)+`~without~me`,
	words(`nobody`, `no~one`, `noone`)+`~would~`+words(`miss`, `care~if`)+`~me`,
	// Hinglish: "mujhe marna hai", "mujhe sach mein mar jaana hai",
	// "suicide karna chahta hoon", "jeene ka mann nahi karta", "khud ko
	// khatam karne ka soch raha hoon", "khud ko hurt karta hoon".
	`mujhe~(?:`+words(`sach~(?:mein|me|mai)`, `bas`, `ab`, `really`, `seriously`, `bhi`, `toh`, `to`)+`~){0,2}`+
		words(`marna`, `mar~jana`, `mar~jaana`)+`~(?:hai|h)`,
	// "ab jeene ka mann nahi", with or without its verb (with none,
	// supportSaid wants nothing after it but a time or a filler).
	words(`jeene`, `jine`)+`~ka~`+words(`mann`, `man`, `dil`)+`~`+words(`nahi`, `nhi`)+`(?:~`+
		words(`karta`, `karti`, `krta`, `krti`, `hota`, `hai`, `h`)+`)?`,
	// "khud ko khatam karna chahta hu", "suicide karne wala hoon", "mujhe mar
	// jana chahiye", "fansi laga lunga", "apni jaan de dunga" (not "tere
	// liye", supportCheck), "neend ki goliyan kha lungi", "mann karta hai
	// mar jaun", "suicide ke thoughts", "zindagi khatam kar dunga" (not
	// "teri"), "jeene ka koi matlab nahi".
	`khud~ko~`+words(`khatam`, `khatm`, `khtm`, `maarna`, `marna`)+`~(?:`+words(`karna`, `krna`)+`~)?`+
		words(`chahta`, `chahti`, `chahte`),
	words(`suicide`, `khud~?kushi`)+`~`+words(`karne`, `krne`)+`~`+words(`wala`, `wali`, `vala`, `vali`),
	`mujhe~`+words(`mar~jana`, `mar~jaana`, `marna`)+`~`+words(`chahiye`, `chaiye`, `chahie`),
	words(`fanda`, `phanda`, `fansi`, `phansi`, `faansi`, `phaansi`)+`~`+words(`laga`, `lga`)+`~`+words(`lunga`,
		`lungi`, `loonga`, `loongi`),
	`(?:apni~)?jaan~de~`+words(`dunga`, `dungi`, `doonga`, `doongi`),
	`(?:neend~ki~)?`+words(`goliyan`, `goliya`, `golia`)+`~`+words(`kha`, `khaa`)+`~`+words(`lunga`, `lungi`,
		`loonga`, `loongi`, `li`, `liya`, `li~hai`, `li~hain`),
	words(`mann`, `man`, `dil`)+`~`+words(`karta`, `krta`, `karti`)+`~`+words(`hai`, `h`)+`~(?:ki~)?`+
		words(`mar~jaun`, `mar~jau`, `mar~jaaun`, `marjaun`, `mar~jaaon`),
	`suicide~`+words(`ke`, `ki`)+`~`+words(`thoughts`, `khayal`, `khyal`, `vichar`),
	words(`zindagi`, `zindgi`, `jindagi`)+`~`+words(`khatam`, `khatm`, `khtm`)+`~`+words(`kar`, `kr`)+`~`+
		words(`dunga`, `dungi`, `lunga`, `lungi`, `doonga`, `doongi`),
	words(`jeene`, `jine`)+`~ka~(?:koi~)?`+words(`matlab`, `wajah`, `vajah`, `reason`, `point`)+`~`+
		words(`nahi`, `nhi`),
	`khud~ko~`+words(`khatam`, `khatm`, `khtm`, `maarne`, `marne`, `maar`, `hurt`)+`~(?:`+words(`karne`, `krne`)+
		`~)?ka~`+words(`soch`, `sochta`, `sochti`, `vichar`),
	`khud~ko~`+words(`hurt`, `cut`, `kaat`, `kaatta`, `kaatti`, `maarta`, `maarti`, `jalata`, `jalati`)+`~(?:`+
		words(`karta`, `karti`, `krta`, `krti`)+`~)?`+words(`hoon`, `hu`, `hun`, `hai`, `h`),
	words(`mar~jana`, `mar~jaana`, `marna`)+`~(?:hai|h)~mujhe`,
	// "marna chahta hoon", "marne ke baare mein sochta hoon", "khud ko
	// khatam kar lunga", "zinda nahi rehna".
	words(`marna`, `mar~jana`, `mar~jaana`, `marne`)+`~`+words(`chahta`, `chahti`, `chahte`, `chahata`, `chahu`,
		`chahun`),
	words(`marne`, `suicide~karne`, `suicide~krne`, `khud~?kushi~karne`, `khud~?kushi`, `khud~?khushi`,
		`suicide`)+`~`+words(`ka`, `ki`,
		`ke~(?:baare|bare)~(?:mein|me|mai|main)`)+`~(?:\pL+~){0,2}?`+words(`soch`, `sochta`, `sochti`, `sochte`,
		`vichar`, `khayal`, `khyal`, `irada`, `plan`),
	words(`khud`, `apne~aap`)+`~ko~`+words(`khatam`, `khatm`, `khtm`, `maar`, `mar`, `hurt`, `kaat`)+`~(?:`+
		words(`kar`, `kr`)+`~)?`+words(`lunga`, `lungi`, `loonga`, `loongi`, `dunga`, `dungi`, `doonga`, `doongi`),
	words(`zinda`, `jinda`)+`~`+words(`nahi`, `nhi`, `nai`)+`~`+words(`rehna`, `rahna`),
	// "apne aap ko hurt kiya", "khud ko kaat liya": hurting oneself, past.
	words(`khud`, `apne~aap`)+`~ko~`+words(`hurt`, `cut`)+`~`+words(`kiya`, `kia`, `kar~liya`, `kr~liya`),
	words(`khud`, `apne~aap`)+`~ko~`+words(`kaat`, `kaata`, `kaati`, `jalaya`, `jalaaya`)+`~`+words(`liya`, `diya`),
	// "terrace se kood jaungi".
	words(`chhat`, `chhath`, `terrace`, `building`, `bridge`, `pul`, `balcony`, `roof`)+`~se~`+
		words(`kood`, `kud`, `koodh`)+`~`+words(`jaunga`, `jaungi`, `jaaunga`, `jaaungi`, `jaoonga`, `jaoongi`),
	// "aaj raat sab khatam kar dunga", with a goodbye in the post
	// (supportCheck).
	words(`sab`, `sabkuch`, `sab~kuch`, `sb`)+`~`+words(`khatam`, `khatm`, `khtm`)+`~`+words(`kar`, `kr`)+`~`+
		words(`dunga`, `dungi`, `doonga`, `doongi`, `lunga`, `lungi`),
	// "na is duniya mein", with not staying in the post (supportCheck).
	`is~duniya~`+words(`mein`, `me`, `mai`, `se`),
	words(`suicide`, `khud~?kushi`, `khud~?khushi`)+`~`+words(`karna`, `krna`, `kar~lunga`, `kar~lungi`,
		`karunga`, `karungi`)+
		`(?:~`+words(`chahta`, `chahti`, `hai`, `h`)+`)?`,
	words(`jeena`, `jina`)+`~`+words(`nahi`, `nhi`)+`~`+words(`chahta`, `chahti`, `hai`),
	// "mujhe nahi jeena", "main ab nahi jeena chahti": supportCheck wants
	// nothing after it but a time or a filler.
	words(`nahi`, `nhi`)+`~`+words(`jeena`, `jina`)+`(?:~`+words(`chahta`, `chahti`, `chahte`, `hai`, `h`)+`)?`,
	words(`jeene`, `jine`)+`~ki~`+words(`ichha`, `iccha`, `icchha`, `ichchha`, `khwahish`)+`~`+words(`nahi`, `nhi`),
	hinglishActs+`~`+hinglishIntents,
	words(`marne`, `suicide~karne`, `suicide~krne`, `khud~?kushi~karne`, `khud~?khushi~karne`)+`~ka~(?:mann|man)~`+words(`hai`, `h`, `karta`, `karti`,
		`kar~raha`, `kar~rha`),
) + `\b`),
	// With no "I", where a sentence, a line or a greentext line starts:
	// ">honestly want to end it", "want to die. not a meme".
	bothViews(`(?:^|[.!?;:\n>,])[^\pL\pN]*(?:` + words(`honestly`, `really`, `just`, `literally`, `seriously`,
		`kinda`, `lowkey`, `genuinely`, `still`, `sometimes`) + `~){0,3}` + words(
		words(`want`, `wanna`, `wana`)+`~(?:(?:to|2)~)?`+words(`die`, `end~it~all`, `end~my~life`, `end~it`,
			`kill~my~self`, `not~exist`, `not~be~alive`, `disappear`),
		// "been cutting again since the results came out"
		words(`been`, `started`, `keep`, `kept`, `back~to`)+`~`+words(`cutting`, `hurting~myself`, `harming~myself`)+
			`~again`,
	) + `\b`),
}

// selfHarmRules are encouragement to hurt or kill oneself, and pro-eating-
// disorder content, in English and some Hinglish. They are held, through
// selfHarmCheck.
var selfHarmRules = []rule{bothViews(`\b` + words(
	`k+y+s+`,
	// "kill", and "kil" with one l, which the loose view reads as "kii".
	`kil+~`+words(`your`, `ur`, `yo`)+`~sel(?:f|ves)`, `kil+~urself`, `kil+~yourselves`,
	`unalive~`+words(`your`, `ur`)+`~self`,
	words(`go`, `pls`, `please`, `plz`, `why~don'?t~you`, `why~dont~you`, `you~should`, `u~should`,
		`you~shud`, `u~shud`, `you~need~to`, `u~need~to`, `you~deserve~to`, `u~deserve~to`, `you~can~go`,
		`u~can~go`, `you~ought~to`, `you~must`)+`~(?:just~)?`+
		words(`die`, `kil+~(?:your|ur)~self`, `hang~(?:your|ur)~self`, `cut~(?:your|ur)~self`,
			`hurt~(?:your|ur)~self`,
			`end~your~life`, `starve~(?:your|ur)~self`, jumpOff, `cut~deeper`, drinkBleach, `commit~suicide`,
			`an~hero`, `off~(?:your|ur)~self`, `delete~(?:your|ur)~self`,
			// selfHarmCheck wants nothing but a shrug or an insult after
			// these: "you should end it", not "end it with him"; "go
			// starve", not "starve the fire"; "you should stop eating", not
			// "so much sugar".
			`end~it(?:~all)?`, `starve`, `stop~eating`),
	words(`cut`, `hang`, `starve`, `neck`, `rope`, `end`)+`~`+words(`your`, `ur`)+`~sel(?:f|ves)`,
	`end~`+words(`your`, `ur`)+`~life`,
	words(`slit`, `cut`)+`~`+words(`your`, `ur`)+`~wrists?`,
	`why~`+words(`are`, `r`)+`~`+words(`you`, `u`)+`~`+words(`still`, `even`)+`~alive`,
	// Imperatives with no "you" before them, which selfHarmCheck takes only
	// at the start of what is said: "just die", not "i just die every time";
	// "off yourself", not "take the pressure off yourself".
	jumpOff, drinkBleach, `just~die`, `cut~deeper`, `an~hero`,
	`just~stop~eating`, `stop~eating~`+words(fatInsults...),
	words(`off`, `delete`)+`~`+words(`your`, `ur`)+`~sel(?:f|ves)`,
	words(`you`, `u`)+`~`+words(`don'?t`, `dont`, `do~not`)+`~deserve~to~`+words(`live`, `be~alive`, `exist`),
	words(`nobody`, `no~one`, `noone`)+`~`+words(`would`, `will`, `'?d`, `is~gonna`, `is~going~to`)+
		`~(?:even~)?`+words(`miss`, `care~if`)+`~`+words(`you`, `u`),
	`(?:the~)?world~`+words(`would`, `will`)+`~be~better~(?:off~)?without~`+words(`you`, `u`),
	// "you're better off dead", "you'd be better off dead" (selfHarmCheck
	// reads the "you" before it), "why do you even exist", "you shouldn't
	// exist", "nobody would cry if you died".
	`better~off~dead`,
	`why~`+words(`do`, `does`, `did`)+`~`+words(`you`, `u`, `ya`)+`~(?:`+words(`even`, `still`)+`~)?`+
		words(`exist`, `live`, `breathe`),
	words(`you`, `u`)+`~(?:`+words(`really`, `just`, `honestly`, `seriously`)+`~)?`+words(`should~not`, `shouldn'?t`,
		`shouldnt`, `shudnt`, `should~never`)+`~(?:`+words(`even`, `have`)+`~)?`+words(`exist`, `existed`,
		`been~born`, `be~alive`),
	words(`nobody`, `no~one`, `noone`)+`~`+words(`would`, `will`, `'?d`)+`~(?:even~)?`+words(`cry`, `notice`)+`~`+
		words(`if`, `when`)+`~`+words(`you`, `u`),
	// Said of someone else: "OP should neck himself", "everyone crying about
	// results should just kill themselves".
	words(`should`, `shud`, `need~to`, `needs~to`, `ought~to`, `deserves?~to`, `can~go`, `must`, `gotta`, `better`)+
		`~(?:`+words(`just`, `all`, `go`)+`~){0,2}`+words(`kil+`, `unalive`, `hang`, `off`, `neck`, `end`, `rope`, `cut`)+
		`~`+words(`themselves`, `themself`, `himself`, `herself`),
	// "just end it", with nothing after it; "take all your pills".
	`just~end~it(?:~all)?`,
	`take~all~(?:of~)?`+words(`your`, `ur`)+`~`+words(`pills`, `meds`, `medication`, `medicines?`, `tablets`,
		`sleeping~pills`),
	// Hinglish: "tu mar ja", "suicide kar le", "chhat se kood ja", "phansi
	// laga le", "khud ko khatam kar le".
	words(`tu`, `tum`, `aap`)+`~(?:`+words(`jaake`, `ja~ke`)+`~)?mar~`+words(`ja`, `jaa`, `jao`, `jaiye`),
	words(`chhat`, `chat`, `chhath`, `terrace`, `building`, `bridge`, `pul`, `balcony`, `roof`)+`~se~`+
		words(`kood`, `kud`, `koodh`)+`~`+words(`ja`, `jaa`, `jao`),
	words(`phansi`, `fansi`, `phaansi`, `faansi`, `fanda`, `phanda`, `fandaa`, `phandaa`)+`~`+words(`laga`, `lga`)+
		`~`+words(`le`, `lo`),
	`khud~ko~`+words(`khatam`, `khatm`, `khtm`, `maar`, `mar`, `hurt`, `kaat`)+`~`+words(`kar~le`, `kar~lo`, `karle`,
		`karlo`, `kr~le`, `krle`, `le`, `lo`),
	`mar~`+words(`ja`, `jaa`, `jao`),
	words(`suicide`, `khud~?kushi`, `khud~?khushi`)+`~`+words(`kar~le`, `kar~lo`, `karle`, `karlo`, `kr~le`,
		`kr~lo`, `krle`, `krlo`),
	words(`jaake`, `ja~ke`, `jaa~ke`)+`~mar`,
	// "jaa mar", "ja mar ja": selfHarmCheck turns down "ja mar gaya".
	words(`ja`, `jaa`, `jao`)+`~mar+(?:~`+words(`ja`, `jaa`, `jao`)+`)?`,
	// Pro-eating-disorder content.
	`thinspo`, `thinspiration`, `meanspo`, `bonespo`, `skinnyspo`, `pro~?ana`, `pro~?mia`,
	words(`ana`, `mia`)+`~`+words(`coach`, `buddy`, `tips?`),
) + `\b`)}

// selfHarmUrgeRules read encouragement as two parts, the act and what
// frames it as said to someone, rather than as phrasings: the act is found
// here ("overdose", "stop breathing", "rope", "jump in front of a train",
// "kysing"), and urgeCheck reads the words before it for the frame: an
// imperative opening its clause ("bro just rope"), "and" after a request
// ("do us all a favour and overdose", "do the world a favour and stop
// breathing"), or a modal said to someone ("you should", "why don't you",
// "time to", "maybe you should"). The other rules judge the act itself:
// "ending yourself would be the best thing you did", "suicide is the only
// solution for someone with your marks", "the world would be better if you
// ended your life", and the Hinglish imperatives ("zeher kha le", "apni jaan
// le le", "latak ja", "mar kyun nahi jaata").
var selfHarmUrgeRules = []rule{
	bothViews(`\b` + words(
		`jump~in~front~of~`+words(`a`, `the`)+`~`+words(`train`, `bus`, `truck`, `car`, `metro`, `lorry`, `tram`),
		`overdose`, `od`, `rope`, `die`, `jump`, `do~it`,
		words(`stop`, `quit`)+`~`+words(`breathing`, `existing`, `living`), `cease~to~exist`,
		`self~?harm`, words(`harm`, `drown`, `kill`, `hang`, `end`, `off`, `cut`, `neck`, `starve`, `unalive`, `top`)+`~`+
			words(`your`, `ur`)+`~sel(?:f|ves)`, `set~`+words(`your`, `ur`)+`~self~on~fire`,
		`k+y+s+(?:ing|in|ed)`, `commit~`+words(`sudoku`, `seppuku`, `toaster~bath`, `die`),
	) + `\b`),
	// "ending yourself would be the best thing you did all year".
	bothViews(`\b` + words(`ending`, `killing`, `offing`, `hanging`, `roping`, `unaliving`, `necking`, `drowning`) +
		`~` + words(`your`, `ur`) + `~sel(?:f|ves)~` + words(`would`, `will`, `is`, `'?d`, `might`) +
		`~(?:be~)?(?:` + words(`the`, `a`, `your`, `ur`) + `~)?` + words(`best`, `right`, `only`, `smartest`,
		`kindest`, `nicest`, `greatest`, `good`, `favou?r`, `gift`, `win`, `service`, `improvement`) + `(?:~` +
		words(`thing`, `move`, `decision`, `choice`, `idea`, `option`) + `)?\b`),
	// "suicide is the only solution for someone with your marks".
	bothViews(`\b` + words(`suicide`, `kys`, `death`, `dying`, `rope`, `killing~(?:your|ur)~self`, `ending~it`,
		`ending~(?:your|ur)~life`) + `~` + words(`is`, `'?s`, `would~be`, `will~be`, `seems~like`, `sounds~like`) +
		`~(?:` + words(`the`, `your`, `ur`, `a`) + `~)?(?:` + words(`only`, `best`, `right`, `real`, `easiest`,
		`obvious`) + `~)?` + words(`solution`, `option`, `answer`, `way~out`, `choice`, `cure`, `fix`) + `\b`),
	// "the world would be better if you ended your life tonight".
	bothViews(`\b` + words(`world`, `everyone`, `everybody`, `we`, `parents`, `family`, `mom`, `mum`, `dad`, `class`,
		`board`, `school`, `earth`, `planet`, `society`, `everything`) + `~(?:` + words(`would`, `will`, `'?d`) +
		`~(?:all~)?be|` + words(`is`, `are`, `'?s`) + `)~(?:all~)?(?:` + words(`so~much`, `much`, `way`) +
		`~)?better~(?:off~)?` + words(`if~`+words(`you`, `u`)+`~`+words(`died`, `were~dead`, `was~dead`,
		`ended~(?:your|ur)~life`, `ended~it`, `killed~(?:your|ur)~self`, `weren'?t~`+words(`here`, `alive`, `around`,
			`born`), `didn'?t~exist`, `never~existed`, `disappeared`, `jumped`, `hanged~(?:your|ur)~self`,
		`were~gone`), `without~`+words(`you`, `u`)) + `\b`),
	// Hinglish: "zeher kha le", "apni jaan le le", "latak ja", "mar kyun
	// nahi jaata".
	bothViews(`\b` + words(
		words(`zeher`, `zehar`, `zahar`, `jeher`, `zaher`)+`~`+words(`kha`, `khaa`, `pi`, `pee`)+`~`+
			words(`le`, `lo`, `ja`, `jaa`, `jao`, `ke~mar~ja`, `ke~mar~jaa`, `ke~mar~jao`),
		`apni~jaan~`+words(`le`, `de`)+`~`+words(`le`, `de`, `lo`, `do`),
		words(`latak`, `lattak`)+`~`+words(`ja`, `jaa`, `jao`),
		`mar+~`+words(`kyun`, `kyu`, `kyon`, `q`)+`~`+words(`nahi`, `nhi`, `nai`)+`~`+words(`jaata`, `jata`, `jaati`,
			`jati`, `jaate`, `jate`),
		// "rope time for OP": the act as the time for it.
		words(`rope`, `kys`, `neck`)+`~time`,
	) + `\b`),
}

// selfHarmMethodRules ask or tell how to hurt or kill oneself: how, how many,
// what dose. Someone asking may be asking for themselves, so a post with one
// is answered with the support message first, as crisis language is, and is
// held once the poster has seen it. methodCheck turns down the questions
// about the world rather than oneself: "how much caffeine is lethal", "the
// lethal dose for half the test animals", "fastest way to die in minecraft".
var selfHarmMethodRules = []rule{bothViews(`\b` + words(
	`how~`+words(`do`, `can`, `could`, `should`, `would`, `does`)+`~`+
		words(`i`, `you`, `u`, `one`, `someone`, `a~person`, `people`)+`~`+methods,
	`how~to~`+words(methods, `hide~(?:my~)?`+words(`cuts`, `scars`, `self~harm`), hiddenMethods),
	`how~`+words(`many`, `much`)+`~`+window(100)+`~`+words(`to~die`, `to~kill~`+words(`my`, `your`)+`~self`,
		`to~kill~`+words(`me`, `you`, `someone`, `a~person`),
		`to~overdose`, `to~od`, `overdose`, `be~fatal`, `be~lethal`, `be~deadly`, `is~fatal`, `is~lethal`,
		`is~deadly`, words(`would`, `will`, `can`, `could`)+`~kill~`+words(`me`, `you`, `someone`, `a~person`),
		`to~end~it`, `to~end~my~life`,
		// "how many paracetamol is a lethal dose", "would be a fatal dose"
		words(`is`, `would~be`, `will~be`)+`~a~`+words(`lethal`, `fatal`, `deadly`)+`~`+words(`dose`, `amount`,
			`dosage`, `quantity`)),
	// "will 20 pills kill me"
	words(`will`, `would`, `could`, `can`, `does`, `do`)+`~(?:\d+~)?`+medicines+`~`+
		words(`kill~me`, `kill~you`, `kill~someone`, `be~fatal`, `be~lethal`, `be~enough`),
	// "is 20 paracetamol enough to die", "are 20 pills enough to kill me"
	`(?:\d+~)?`+medicines+`~(?:`+words(`be`, `is`, `are`)+`~)?enough~to~`+
		words(`die`, `kill~`+words(`me`, `you`, `someone`), `kill~`+words(`my`, `your`)+`~self`, `od`, `overdose`,
			`end~it`),
	// "can i die from 20 paracetamol", "will i die if i take all my pills",
	// "can you die from taking 20 paracetamol"
	words(`can`, `will`, `would`, `could`)+`~`+words(`i`, `you`, `u`, `someone`, `a~person`)+`~die~`+
		words(`from(?:~`+words(`taking`, `eating`, `swallowing`, `having`)+`)?`, `of`, `if~`+words(`i`, `you`, `u`)+
			`~`+words(`take`, `took`, `eat`, `swallow`, `have`))+
		`~(?:`+words(`\d+`, `a~few`, `all~(?:my|the)`, `too~many`, `some`)+`~)?`+medicines,
	// The rest open a question methodCheck reads the end of in Go, since
	// spelling each out here would copy it into every branch planned: "what
	// pills can kill you" (with "what" or "which" before), "where to cut
	// to bleed out" and "which vein to cut", "how high to jump to die" and
	// "what height is fatal" (with a death in the sentence), "how long
	// does it take to die from hanging", and "eat less than 500 calories".
	words(`pills`, `meds`, `medicines?`, `tablets`, `drugs`, `chemicals`, `poisons?`)+`~`+words(`can`, `will`,
		`could`, `would`)+`~kill~`+words(`you`, `u`, `me`, `someone`),
	words(`where`, `vein`, `veins`, `artery`, `arteries`)+`~`+words(`to`, `should~i`, `do~i`)+`~cut`,
	words(`how~high`, `how~tall`, words(`which`, `what`)+`~`+words(`floor`, `height`, `storey`)),
	`how~long~`+words(`does`, `would`, `will`, `do`)+`~`+words(`it`, `you`, `u`, `i`)+`~`+words(`take`, `need`)+
		`~to~`+words(`bleed~out`, `die`),
	`eat~`+words(`less`, `under`, `below`, `fewer`),
	// "best way to cut without getting caught", "how to cut without my
	// parents knowing": methodCheck wants a way asked for before it.
	cutUnseen,
	`what~`+words(`dose`, `amount`, `quantity`, `dosage`)+`~`+window(40)+`~`+
		words(`lethal`, `fatal`, `deadly`, `kill~`+words(`me`, `you`, `myself`, `yourself`), `to~die`,
			`overdose`, `od`),
	words(`lethal`, `fatal`, `deadly`)+`~`+words(`dose`, `doses`, `dosage`, `amount`, `quantity`),
	words(`painless`, `easiest`, `easy`, `quickest`, `quick`, `best`, `fastest`, `surest`, `least~painful`)+
		`~`+words(`way`, `ways`, `method`, `methods`)+`~(?:for~`+words(`me`, `someone`, `a~person`)+`~)?(?:to~)?`+
		words(`die`, `kill~`+words(`my`, `your`, `ur`, `one`)+`~self`, `commit~suicide`,
			`end~`+words(`my`, `your`, `one'?s`)+`~life`, `od`, `overdose`, `end~it(?:~all)?`, `go`,
			// "easiest way to purge", which methodCheck wants with nothing
			// after it, or unseen; "best way to purge after a binge".
			`purge(?:~after~(?:a~)?`+words(`eating`, `meals?`, `food`, `dinner`, `lunch`, `breakfast`, `binge`,
				`binging`, `bingeing`)+`)?`),
	`suicide~`+words(`method`, `methods`, `guide`, `instructions`, `tips`),
	// Asked the way a search is: "ways to commit suicide", "methods of
	// suicide", "easiest ways to die", "tips for purging". methodCheck
	// wants nothing after a bare "die", "suicide" or "purging", or a way
	// of dying ("painlessly"), so "ways to die in minecraft", "methods of
	// suicide prevention" and "ways to die with dignity" in a novel are not.
	words(`ways`, `methods`, `tips`, `techniques`, `tricks`)+`~`+words(`to`, `of`, `for`)+`~`+searchedMethods,
	// "can you die from cutting your wrists": methodCheck wants "can you",
	// "will i" or the like just before it.
	`die~`+words(`from~`+words(`cutting`, `slitting`), `if~`+words(`i`, `you`, `u`)+`~`+words(`cut`, `slit`))+
		`~(?:`+words(`my`, `your`, `ur`, `one'?s`, `the`, `their`)+`~)?wrists?`,
	// "how deep should i cut"
	`how~deep~`+words(`should`, `do`, `can`, `would`, `to`, `must`)+`~(?:`+words(`i`, `you`, `u`, `one`)+`~)?`+
		`(?:`+words(`have`, `need`)+`~to~)?cut`,
	// The poster's own death as what a question is about: "is there a way
	// for me to overdose", "how high does a building have to be for me to
	// not survive the fall", "how many pills would i need to not wake up";
	// methodCheck wants a question round it.
	`for~me~to~(?:just~)?`+words(`die`, `overdose`, `od`, `end~it(?:~all)?`, `end~my~life`, `kill~my~self`,
		words(`not`, `never`)+`~`+words(`survive`, `wake~up`)),
	words(`i`, `it`)+`~(?:`+words(`would`, `will`, `'?d`, `does`, `do`)+`~)?`+words(`need`, `take`, `takes`)+
		`~to~`+words(`not`, `never`)+`~`+words(`survive`, `wake~up`),
) + `\b`)}

// pillQuestionRules are how-many questions about medicine, read only as far
// as the medicine: "how many sleeping pills are lethal", "how many of my
// mum's sleeping pills would it take" (only with a medicine named, since "how
// many snakes are deadly" is a biology question), and one about pills with a
// disclaimer after it, "how many sleeping pills do you take? asking for a
// friend", "kitni neend ki goliyan leni padti hain? bas pooch raha hoon".
// pillQuestionCheck reads what makes it a question about dying after the
// medicine, in its sentence, or the disclaimer, on its line. Written as one
// pattern the three were read 60 characters on from every medicine named
// after every "how many" in reach, so a post of "howmany@pills_(snap " over
// and over, where no medicine ends a word, cost about 220 steps a byte
// (TestFilterCostIsBounded, "pills and a handle"). Now the medicine has to
// start a word, so the names are tried only where a word starts, and they
// are tried in the order of the alphabet, so that names with the same first
// letters are tried as one until they part; the order changes nothing that
// matches, since no name is the start of another that also ends a word.
var pillQuestionRules = []rule{bothViews(`\b` + words(
	`how~many~`+window(60)+`\b`+sortedMedicines,
	words(`kitni`, `kitne`)+`~(?:\pL+~){0,3}?`+words(`goliyan`, `golian`, `goliya`, `pills`, `tablets`),
) + `\b`)}

var (
	pillDeath = phrase(`\b` + words(`are~`+words(`lethal`, `fatal`, `deadly`, `enough~to~`+words(`die`,
		`kill~`+words(`me`, `you`, `someone`), `od`, `overdose`)), words(`would`, `will`, `does`, `do`)+`~it~take`) +
		`\b`)
	pillDeathPlain = newWindowSearch(pillDeath)
	pillDeathLoose = newWindowSearch(canon(pillDeath))
	disclaimer     = phrase(`\b` + words(`just~asking`, `asking~for~a~friend`, `bas~pooch`, `bas~puch`, `sirf~pooch`) +
		`\b`)
	disclaimerPlain = newWindowSearch(disclaimer)
	disclaimerLoose = newWindowSearch(canon(disclaimer))
	// disclaimedPills are what a disclaimed question may ask about.
	disclaimedPills = canonSetOf(`goliyan golian goliya pills tablets`)
)

// pillQuestionCheck reads on from a pillQuestionRules match, to a death named
// in the medicine's sentence within 60 characters, or, for pills with at most
// three words before them, a disclaimer on its line within 60, and puts the
// question as far as that to methodCheck.
func pillQuestionCheck(v *view, m []int) bool {
	text := v.text
	after := text[m[1]:min(len(text), m[1]+64)]
	find := func(plain, loose *search, s string) int {
		w := plain
		if v.loose {
			w = loose
		}
		if x := w.findIn(v, s); x != nil && x[0] <= 60 {
			return m[1] + x[1]
		}
		return -1
	}
	sentence := after
	if j := strings.IndexAny(after, ".?!\n"); j >= 0 {
		sentence = after[:j]
	}
	end := find(pillDeathPlain, pillDeathLoose, sentence)
	if ws := wordsIn(text[m[0]:m[1]]); end < 0 && disclaimedPills[ws[len(ws)-1]] &&
		(!is(ws[0], "how") || len(ws) <= 6) {
		line, _, _ := strings.Cut(after, "\n")
		end = find(disclaimerPlain, disclaimerLoose, line)
	}
	return end >= 0 && methodCheck(v, []int{m[0], end})
}

// medicines are what an overdose question names; sortedMedicines are the
// same names in the order of the alphabet.
var (
	overdoseMedicines = []string{`pills?`, `tablets?`, `capsules?`, `paracetamol`, `tylenol`, `acetaminophen`, `ibuprofen`,
		`aspirin`, `advil`, `panadol`, `crocin`, `dolo`, `meds`, `medicines?`, `medications?`, `antidepressants?`,
		`sleeping~pills`, `insulin`, `xanax`, `benadryl`, `melatonin`}
	medicines       = words(overdoseMedicines...)
	sortedMedicines = func() string {
		names := slices.Clone(overdoseMedicines)
		slices.Sort(names)
		return words(names...)
	}()
)

// window is up to n characters of one sentence, read lazily, written to
// follow a "~": "how many~" + window(60) + "pills". Written as
// "~[^.?!\n]{0,60}?" the two would overlap, since a space or a mark is both a
// separator and part of the sentence, and every character of a run of them
// would start the window afresh, keeping a copy of it alive for each space
// of the run, up to n, at every character it reads: "how many", a hundred
// spaces and "pills", over and over, cost about 250 steps a byte. Starting
// the window at a letter or a digit leaves the separator every space and
// mark before it, matches the same text, and keeps one copy.
func window(n int) string { return `(?:[\pL\pN][^.?!\n]{0,` + strconv.Itoa(n-1) + `}?)??` }

// methods are the ways of hurting or killing oneself a how-question asks for,
// purging among them: "how to end it all", "how to make myself throw up".
var methods = words(
	`end~it~all`,
	`make~`+words(`my`, `your`)+`~self~`+words(`throw~up`, `vomit`, `puke`),
	`purge~(?:after~)?`+words(`eating`, `meals?`, `food`, `dinner`, `lunch`, `breakfast`),
	// "how to purge", with nothing after it or unseen ("without anyone
	// knowing", "quietly"), which methodCheck reads.
	`purge`,
	`kill~`+words(`my`, `your`, `ur`, `them`, `one`)+`~sel(?:f|ves)`,
	`commit~suicide`,
	`end~`+words(`my`, `your`, `their`, `a`, `one'?s`)+`~life`,
	`overdose`, `od`,
	`hang~`+words(`my`, `your`, `ur`, `them`, `one`)+`~sel(?:f|ves)`,
	`die~`+words(`painlessly`, `quickly`, `fast`, `peacefully`, `without~pain`, `in~`+words(`my`, `your`)+`~sleep`),
	`cut~`+words(`deeper`, words(`my`, `your`)+`~wrists?`),
	`starve~`+words(`my`, `your`)+`~self`,
	words(`tie`, `make`)+`~(?:a~)?noose`,
)

// searchedMethods open what a search for ways, methods or tips asks after:
// "ways to commit suicide", "methods of suicide", "ways to die painlessly",
// "tips for purging". Only the first word is here, since every branch
// planned copies it; searchedMethod reads the rest.
var searchedMethods = words(`commit(?:ting)?`, `suicide`, `die`, `dying`, `kill(?:ing)?`, `end(?:ing)?`,
	`overdos(?:e|ing)`, `od`, `purg(?:e|ing)`, `hang(?:ing)?`, `starv(?:e|ing)`, `self~harm(?:ing)?`)

// hiddenMethods are asked after "how to" only, which is how they are asked:
// slitting wrists, bleeding out, purging ("how to throw up after eating"),
// starving ("how to stop eating", which methodCheck wants with nothing, or
// "completely", after it: not "junk food at night") and hiding it ("how to
// hide not eating").
var hiddenMethods = words(`slit~(?:`+words(`my`, `your`, `ur`, `one'?s`)+`~)?wrists?`, `cut~wrists?`, `bleed~out`,
	words(`throw~up`, `vomit`, `puke`)+`~after~`+words(`eating`, `meals?`, `food`, `dinner`, `lunch`, `breakfast`),
	`stop~eating`,
	`hide~`+words(`not~eating`, `that~i'?m~not~eating`, `that~i~don'?t~eat`, `my~eating~disorder`, `starving`))

// cutUnseen is cutting hidden from others: "cut without getting caught",
// "without my parents noticing".
var cutUnseen = `cut~(?:my~self~)?without~` + words(`getting~caught`, `leaving~(?:any~)?`+words(`scars?`, `marks?`),
	`(?:my~)?`+words(`anyone`, `anybody`, `people`, `parents`, `mom`, `mum`, `dad`, `family`)+`~`+words(`knowing`,
		`noticing`, `seeing`, `finding~out`))

// Threats: an intent to hurt people or attack a place, held only when a place
// or a time comes with it ("at school", "tomorrow", "in 3rd period") — which an
// intent to attack a school always does, since it names one; the warning
// "don't come to school tomorrow" with a cue that it is one ("you've been
// warned"); and "school shooting" with a time still to come. Only the first
// person and the future count, and a call for someone to do it ("someone
// should shoot up the school"): a history question, or news that reports an
// attack ("he threatened to shoot up his school"), names no intent of the
// poster's, so it passes. Some Hinglish is read too ("sabko goli maar dunga",
// "tujhe kal maar dunga", "kal school uda dunga").
var (
	// peopleNouns are who a threat names by what they are: "the boys", "my
	// maths teacher", "the 12th graders", "the warden".
	peopleNouns = words(`batch(?:mates?)?`, `boys?`, `bullies`, `bully`, `captain`, `class`, `classmates?`,
		`coach(?:es)?`, `drivers?`, `everyone`, `faculty`, `freshers?`, `girls?`, `graders?`, `guards?`, `guys?`,
		`hostell?ers`, `invigilators?`, `juniors?`, `kids?`, `lecturers?`, `ma'?am`, `madam`, `miss`, `monitors?`,
		`people`, `ppl`, `prefects?`, `principal`, `professors?`, `profs?`, `room~?mates?`, `school`, `seniors?`, `sir`,
		`staff`, `students?`, `teachers?`, `toppers?`, `wardens?`)
	// threatPeople are who a threat is aimed at. One word may come between a
	// determiner and the noun: "my maths teacher", "the 12th graders". "As
	// many people as I can" names no one, and threatens most of all.
	threatPeople = words(`everyone`, `everybody`, `every~one`, `people`, `you~all`, `y'?all`, `yall`,
		`all~of~`+words(`you`, `u`, `them`), `you`, `u`, `them~all`, `them`, `him`, `her`,
		words(`the`, `my`, `our`, `all~the`, `some`, `those`, `these`, `that`, `this`, `every`, `his`, `their`,
			`your`, `ur`)+`~(?:[\pL\pN]+[^\pL\pN]+)?`+peopleNouns,
		`teachers`, `students`, `kids`, `classmates`, `some(?:one|body)`, `the~whole~`+words(`school`, `class`),
		`as~many~(?:[\pL\pN]+~)?as~`+words(`i`, `we`, `possible`), `as~many~`+peopleNouns)
	// strongThreatPeople add the people a strong act may be aimed at without
	// naming them: "I'm gonna stab whoever sits next to me". "Kill whoever
	// ate my maggi" is how people talk; "stab whoever" is not.
	strongThreatPeople = words(threatPeople, `whoever`, `whomever`, `anyone`, `anybody`, `any~one`)
	// threatPlaces are where a threat says it will happen, gatherings and
	// the parts of a school or college among them: "the faculty meeting",
	// "the exam centre", "the mess", "the seminar hall". They are in
	// alphabetical order, as peopleNouns are, so that words starting alike
	// share their start when the pattern is compiled, and are read once.
	threatPlaces = words(`airport`, `annual~day`, `assembly`, `auditorium`, `bathroom`, `bus`, `cafeteria`, `campus`,
		`canteen`, `center`, `centre`, `ceremony`, `church`, `class`, `classroom`, `coaching`, `college`, `columbine`,
		`concert`, `convocation`, `corridor`, `dorm`, `exam~cell`, `exam~hall`, `farewell`, `ghar`, `graduation`,
		`gurdwara`, `gurudwara`, `gym`, `hall`, `hallway`, `hospital`, `hostel`, `lab`, `library`, `mall`, `meeting`,
		`mess`, `metro`, `mosque`, `office`, `parking`, `parking~lot`, `playground`, `prom`, `restroom`, `school`,
		`school~bus`, `seminar`, `sports~day`, `stadium`, `staffroom`, `staff~room`, `station`, `synagogue`, `temple`,
		`toilets?`, `tuition`, `uni`, `university`, `washroom`, `lockers?`)
	weekdays    = words(`mon`, `tues`, `wednes`, `thurs`, `fri`, `satur`, `sun`) + `day`
	threatTimes = words(`today`, `tonight`, `tomorrow`, `tmrw`, `tmr`, `tmrrw`, `tomoz`, `2moro`, `2morrow`, `kal`,
		`aaj`, words(`on`, `this`, `next`, `coming`)+`~`+weekdays, weekdays,
		`next~week`, `this~week(?:end)?`, words(`after`, `before`)+`~`+words(`school`, `class`),
		`during~`+words(`lunch`, `recess`, `break`, `assembly`, `class`, `school`, `the~exam`, `exams`, `prayer`),
		`at~`+words(`lunch`, `recess`, `break`, `noon`, `midnight`, `assembly`),
		`(?:(?:in|during)~)?`+words(`\d+(?:st|nd|rd|th)`, `first`, `second`, `third`, `fourth`, `fifth`, `sixth`,
			`seventh`, `eighth`, `last`)+`~period`,
		`at~\d{1,2}(?::\d\d)?~?(?:am|pm)`, `at~\d{1,2}:\d\d`,
		`this~`+words(`morning`, `afternoon`, `evening`), `on~the~\d{1,2}(?:st|nd|rd|th)`,
		// Hinglish: "6 baje", "15 tareekh ko", "parso", "raat ko".
		`\d{1,2}~`+words(`baje`, `bje`, `tareekh`, `tarikh`, `tarik`), `parso`, `parson`, `subah`, `shaam`, `sham`,
		`raat`, `dopahar`)
	// futureTimes are the times that can only be to come, which is what
	// tells "school shooting tomorrow" from news of one "today".
	futureTimes = words(`tomorrow`, `tmrw`, `tmr`, `tmrrw`, `tomoz`, `2moro`, `2morrow`, `tonight`, `next~week`,
		words(`next`, `this`, `coming`)+`~`+weekdays,
		`in~\d+~`+words(`mins?`, `minutes`, `hours?`, `hrs?`, `days?`))

	// Hinglish: the future of "maar" (hit, kill) and of the verbs that
	// finish an act ("maar dunga", "phod dalunga"), blades and guns, and
	// what is brought.
	hindiWill = words(`dunga`, `dungi`, `denge`, `doonga`, `doongi`, `daalunga`, `dalunga`, `daalungi`, `dalungi`,
		`daalenge`, `dalenge`)
	// hindiKill is the future of the verbs of beating and killing: "maar
	// dunga", "peet denge", "thok dunga", "tapka dunga".
	hindiKill = words(words(`maar`, `peet`, `pit`, `thok`, `thokk`, `tapka`)+`~`+hindiWill, `maarunga`,
		`marunga`, `maroonga`, `maaroonga`, `maarungi`, `marungi`, `maarenge`, `marenge`, `peetunga`, `pitunga`,
		`peetenge`, `pitenge`, `thokunga`, `thokenge`, `tapkaunga`, `tapkaenge`, `tapkayenge`)
	// hindiCome is the future of coming and going: "aaunga", "jayenge".
	hindiCome = words(`aaunga`, `aunga`, `aaoonga`, `aaonga`, `aaungi`, `aungi`, `aaenge`, `aayenge`, `aa~raha~h(?:u|oo)n?`,
		`aa~rahi~h(?:u|oo)n?`, `jaunga`, `jaaunga`, `jaungi`, `jaenge`, `jayenge`, `jaayenge`)
	// hindiBody are the parts of someone a threat breaks: "haath pair tod
	// denge", "sar phod dunga".
	hindiBody = words(`sar`, `sir`, `sarr`, `muh`, `munh`, `mooh`, `daant`, `dant`, `haddi`, `haddiyan`, `haddiyaan`,
		`taang`, `taange`, `tange`, `taangein`, `haath`, `hath`, `pair`, `pairr`, `paer`, `paon`, `paanv`, `khopdi`,
		`khopadi`, `jabda`, `pasli`, `pasliyan`, `gardan`, `kamar`, `naak`, `nak`)
	hindiBlades = words(`goli`, `chaku`, `chakku`, `chaaku`, `chakoo`, `chhura`, `chhuri`, `chura`, `churi`, `knife`,
		`talwar`, `talvar`)
	hindiArms = words(`bandook`, `banduk`, `bandooq`, `katta`, `kattaa`, `tamancha`, `tamanchaa`, `pistol`, `gun`,
		`revolver`, `chaku`, `chakku`, `chaaku`, `chhura`, `chhuri`, `talwar`, `talvar`, `knife`, `bomb`, `rod`,
		`danda`, `tezaab`, `tezab`, `acid`)
	// hindiGuns are the arms that are only arms: not a rod, and not
	// "bomb", which is praise as often.
	hindiGuns = words(`bandook`, `banduk`, `bandooq`, `katta`, `kattaa`, `tamancha`, `tamanchaa`, `pistol`, `gun`,
		`revolver`, `chaku`, `chakku`, `chaaku`, `chhura`, `chhuri`, `talwar`, `talvar`, `knife`, `tezaab`, `tezab`,
		`acid`)
	// hindiPronouns are who a Hinglish threat is aimed at.
	hindiPronouns = words(`sabko`, `sab~ko`, `sabhi~ko`, `tujhe`, `tumhe`, `tumko`, `usko`, `unko`, `isko`, `tereko`)
	// harmEnding is how a threat says an event will end: "with bodies on the
	// floor", "with their captain in hospital".
	harmEnding = `(?:` + words(`the`, `their`, `his`, `her`, `your`, `ur`, `some`, `a~few`, `lots~of`, `many`) +
		`~)?(?:[\pL\pN']+~){0,2}?(?:in~(?:` + words(`the`, `a`) + `~)?)?` + words(`bodies`, `body~bags?`,
		`blood~everywhere`, `hospital`, `icu`, `morgue`, `coffins?`, `ambulances?`)

	threatModals = words(`'?ll`, `about~to`, `bout~to`, `could`, `finna`, `going~to`, `gonna`, `gotta`, `gunna`,
		`intend~to`, `may`, `might`, `planning~on`, `planning~to`, `plan~on`, `plan~to`, `ready~to`, `shall`,
		`should`, `wanna`, `want~to`, `wil`, `will`)
	// threatAdverbs are up to two words that may come before the act: an
	// adverb, a swear word, or "go": "i'm going to fucking shoot", "gonna go
	// shoot up the school".
	threatAdverbs = `(?:` + words(`actually`, `bloody`, `come~and`, `definitely`, `effing`, `fckin`, `fcking`,
		`finally`, `fkin`, `fking`, `fkn`, `freakin`, `freaking`, `frickin`, `fricking`, `fuckin`, `fucking`, `go`,
		`go~and`, `just`, `literally`, `really`, `seriously`, `totally`) + `~){0,2}`
	firstPersonOn  = words(`(?:i|we)~(?:(?:'?m|am|are|'?re|r)~)?`+threatAdverbs+threatModals, `imma`, `ima`, `i~'?ma`)
	firstPersonIng = `(?:i|we)~` + words(`'?m`, `am`, `are`, `'?re`, `r`, words(`'?ll`, `will`, `wil`)+`~be`)

	threatIntentRules = []rule{
		bothViews(`\b` + words(
			// "i will", "we're gonna", "imma": then what. And "someone
			// should shoot up the school": a call for it.
			words(firstPersonOn, words(`someone`, `somebody`, `some~one`)+`~`+words(`should`, `needs~to`, `has~to`,
				`ought~to`, `gotta`))+`~`+threatAdverbs+threatActs(false),
			// "i'm bringing", "we're shooting up", "i'll be shooting".
			firstPersonIng+`~`+threatAdverbs+threatActs(true),
			// "tomorrow i shoot up the school": the present, with the strongest
			// acts only, since "we kill it at the fest" is how people talk.
			`(?:i|we)~`+threatAdverbs+threatStrongActs,
			words(`you`, `u`, `y'?all`, `yall`, `you~all`, `everyone`, `everybody`, `all~of~you`)+
				`~(?:`+words(`'?re`, `are`, `r`, `is`, `will`, `'?ll`)+`~)?(?:all~)?`+words(`going~to`, `gonna`, `gunna`, `will`, `'?ll`)+
				`~`+words(`die`, `get~`+words(`shot`, `stabbed`, `hurt`, `killed`, `blown~up`)),
			// "you're dead after school"
			words(`you`, `u`)+`~`+words(`'?re`, `are`, `r`)+`~dead(?:~meat)?`,
			`there~`+words(`will`, `'?ll`, `is~going~to`, `'?s~going~to`, `s~going~to`, `'?s~gonna`, `is~gonna`,
				`s~gonna`, `might`)+`~be~(?:a~)?`+
				words(`shooting`, `bombing`, `massacre`, `bloodbath`, `blood~bath`, `bomb`, `attack`, `school~shooting`),
			// "there's a bomb in the school"
			`there~`+words(`'?s`, `is`, `are`)+`~(?:`+words(`a`, `an`)+`~)?`+
				words(`bombs?`, `guns?`, `shooter`, `active~shooter`)+`~`+words(`in`, `at`, `inside`, `on`),
			// "i planted a bomb in the school": done already.
			`(?:i|we)~(?:`+words(`'?ve`, `have`, `just`, `already`)+`~){0,2}`+
				words(`planted`, `placed`, `hid`, `hidden`, `put`, `left`)+`~(?:`+words(`a`, `the`, `my`, `some`)+`~)?`+
				words(`bombs?`, `explosives?`, `device`),
			// Hinglish: "sabko goli maar dunga", "tujhe maar dalunga", with up
			// to four words between, where Hindi puts a time or a place:
			// "tujhe kal maar dunga", "tujhe kal school ke baad maar dunga",
			// "Verma ma'am ko kal chaku maar dunga", "usko maarunga"; "tujhe
			// dho dalunga", thrashing, which threatExcused takes only of a
			// person, since "kapde ko dho dunga" is the washing; and "uss
			// ladki ko college ke bahar utha lenge", carrying someone off.
			// One run of words between serves all three.
			words(hindiPronouns, `ko`)+`~(?:\pL+~){0,4}?`+words(`(?:`+hindiBlades+`~)?`+hindiKill, `dho~`+hindiWill,
				`utha~`+words(`lenge`, `le~jayenge`, `le~jaenge`, `le~jaunga`, `ke~le~jayenge`, `ke~le~jaenge`,
					`ke~le~jaunga`)),
			// "itna maarenge ki uth nahi paoge", "jamke maarenge": beat someone
			// so hard that.
			words(`itna`, `itni`, `itne`, `aisa`, `aise`)+`~`+hindiKill+`~ki`, words(`jamke`, `jam~ke`)+`~`+hindiKill,
			hindiBlades+`~`+hindiKill,
			// "kal school uda dunga": blow a place up.
			threatPlaces+`~(?:ko~)?(?:bomb~se~)?uda~`+hindiWill,
			// "chaku leke aaunga", "papa ki bandook le aaunga": bring a weapon.
			// "bomb rakh ke aaunga": leave one behind.
			hindiArms+`~`+words(`le~ke`, `leke`, `lekar`, `le~kar`, `le`, `rakh~ke`, `rakhke`, `rakh~kar`, `rakhkar`,
				`chhod~ke`, `chhodke`, `chod~ke`, `chodke`, `laga~ke`, `lagake`)+`~`+hindiCome,
			// "bomb rakh dunga", "bomb laga denge": plant one.
			words(`bomb`, `bum`)+`~`+words(`rakh`, `laga`, `fit~kar`, `plant~kar`, `chhod`, `chod`)+`~`+hindiWill,
			// "chaku le liya hai", "bandook ready hai": one got ready.
			hindiGuns+`~`+words(`le~liya`, `le~li`, `khareed~liya`, `kharid~liya`, `khareed~li`, `kharid~li`,
				`rakh~liya`, `rakh~li`, `ready`, `tayyar`, `taiyar`, `taiyaar`, `load`, `loaded`, `mil~gaya`, `mil~gayi`,
				`mil~gya`, `mil~gyi`),
			// "school mein dhamaka hoga": something will go off. threatCheck
			// wants a place in the sentence, and no party.
			words(`dhamaka`, `dhamakaa`, `dhmaka`, `blast`)+`~`+words(`hoga`, `karunga`, `karenge`, `kar~dunga`,
				`kar~denge`, `karwaunga`, `karwa~dunga`),
			hindiArms+`~`+words(`laaunga`, `launga`, `laoonga`, `laaungi`, `laungi`, `laaenge`, `laayenge`, `layenge`),
			// "pitwa dunga": have someone beaten; "sar phod dunga": break a
			// head; "aag laga dunga", "jala dunga": set on fire. threatCheck
			// turns down a stage, a lamp and a pest.
			`pitwa~`+hindiWill, words(`pitwaunga`, `pitwaoonga`, `pitwaenge`, `pitwaungi`),
			hindiBody+`~`+words(`phod`, `fod`, `tod`, `tor`, `todd`, `kaat`, `kat`)+`~`+hindiWill,
			`aag~`+words(`laga`, `lga`, `lagaa`, `laga~ke`, `lagake`)+`~`+hindiWill,
			words(`jala`, `jalaa`, `jla`)+`~`+hindiWill,
		) + `\b`),
		// Nobody named as doing it: "the next person who calls me fat gets
		// stabbed", "the principal's office gets blown up next tuesday",
		// "every teacher in the staff room gets what's coming", "they'll
		// finally pay", "the farewell is going to end with bodies on the
		// floor", "everyone in the lab at 2pm tomorrow is going to die",
		// "you won't walk again". threatCheck reads who it is said of.
		bothViews(`\b` + words(
			words(`gets`, `getting`, `will~get`, `'?ll~get`, `gonna~get`, `going~to~get`, `will~be`, `'?ll~be`,
				`gonna~be`, `going~to~be`)+`~(?:`+words(`finally`, `definitely`, `literally`, `actually`, `all`,
				`really`)+`~)?`+words(`stabbed`, `shot`, `knifed`, `jumped`, `killed`, `murdered`, `gunned~down`,
				`slaughtered`, `massacred`, `beaten~to~death`, `blown~up`, `bombed`, `burn(?:ed|t)~down`, `torched`, `set~on~fire`,
				`on~fire`),
			words(`gets`, `get`, `will~get`, `'?ll~get`, `gonna~get`, `going~to~get`)+`~what'?s~coming`,
			words(`will`, `'?ll`, `going~to`, `gonna`)+`~end~with~`+harmEnding, `ends~with~`+harmEnding,
			words(`nobody`, `no~one`, `noone`, `none~of~`+words(`you`, `u`, `them`))+`~(?:`+words(`will`, `'?ll`,
				`is~going~to`, `'?s~gonna`, `s~gonna`, `gonna`)+`~)?`+words(`gets?~out`, `makes?~it~out`, `leaves?`,
				`walks?~out`, `comes?~out`)+`~alive`,
			words(`everyone`, `everybody`, `every~one`, `all~of~you`, `y'?all`, `yall`, `you~all`)+`~`+
				words(`in`, `at`, `on`, `inside`, `who`)+`~(?:[\pL\pN']+~){1,10}?(?:`+words(`is`, `are`, `r`)+
				`~)?(?:all~)?`+words(`going~to`, `gonna`, `will`, `'?ll`)+`~(?:all~)?die`,
			words(`you`, `u`)+`~`+words(`won'?t`, `wont`, `will~not`, `will~never`, `'?ll~never`)+`~`+
				words(`walk~again`, `see~tomorrow`, `see~another~day`, `live~to~see~`+words(`tomorrow`, `another~day`),
					words(`make~it`, `get`)+`~out~(?:of~here~)?alive`, `leave~(?:here~)?alive`),
		) + `\b`),
		// "they'll finally pay", "you'll pay for this": with "finally" only
		// where the sentence ends, since "they'll pay for the trip" is not
		// a threat.
		bothViews(`\b` + words(`you`, `u`, `they`, `he`, `she`, `y'?all`, `yall`, `you~all`, `everyone`, `everybody`,
			`them~all`) + `~` + words(`will`, `'?ll`, `are~gonna`, `r~gonna`, `is~gonna`, `'?re~gonna`, `gonna`,
			`are~going~to`, `is~going~to`, `'?re~going~to`) + `~(?:` + words(`all`, `definitely`, `really`) + `~)?` +
			words(`finally~pay[ \t]*(?:[.!?\n]|$)`, `pay~for~`+words(`this`, `that`, `what`, `everything`)+`\b`,
				`pay~with~`+words(`their`, `your`, `ur`, `his`, `her`)+`~`+words(`lives`, `life`, `blood`)+`\b`)),
		// With no subject, at the start of a sentence or line: "gonna shoot up
		// the school tmrw", "shooting up the school tomorrow, don't come".
		bothViews(`(?:^|[.!?;:\n])[^\pL\pN]*` + words(
			words(`gonna`, `gunna`, `going~to`, `finna`, `bout~to`, `about~to`)+`~`+threatAdverbs+threatActs(false),
			threatStrongProgressive,
		) + `\b`),
	}
	// namedTargetRules threaten someone by name: "I'm going to shoot Rahul
	// tomorrow", "i'm going to shoot mr sharma tomorrow". namedTargetCheck
	// takes the name only when it is written with a capital or after a title,
	// and is not a day, a subject or a game. The groups are the title and the
	// name.
	namedTargetRules = []rule{bothViews(`\b` + words(
		firstPersonOn+`~`+threatAdverbs+words(`shoot`, `stab`, `kill`, `murder`),
		firstPersonIng+`~`+threatAdverbs+words(`shooting`, `stabbing`, `killing`, `murdering`),
	) + `[^\pL\pN]+(?:(` + words(`mr`, `mrs`, `ms`, `miss`, `dr`, `sir`, `madam`, `ma'?am`, `mam`) +
		`)\.?[^\pL\pN]+)?(\pL{2,20})\b`)}
	threatPlaceOrTimeRules = []rule{bothViews(`\b` + words(threatPlaces, threatTimes) + `\b`)}

	// "school shooting tomorrow at 10", with only a time still to come or a
	// day close, and so "a mass shooting", "the next school shooter", "the
	// next columbine" and "a massacre at our school": schoolShootingCheck
	// turns down the ones the words round them say are past or a topic.
	schoolShootingRules = []rule{bothViews(`\b` + words(words(`school`, `mass`)+`~shooting`,
		`next~`+words(`school~shooter`, `columbine`),
		words(`massacre`, `bloodbath`, `blood~bath`)+`~`+words(`at`, `in`)+`~`+words(`our`, `the`, `this`, `my`,
			`ur`, `your`)+`~`+words(`school`, `college`, `class`, `campus`)) + `\b`)}
	shootingTimeRules = []rule{bothViews(`\b` + words(futureTimes, weekdays) + `\b`)}

	// "Don't come to school tomorrow" is far more often about a holiday than a
	// warning, so it counts only beside a cue that it is one.
	threatWarningRules = []rule{bothViews(`\b` + words(`don'?t`, `dont`, `do~not`) + `~` + words(`come`, `go`) +
		`~(?:in~)?(?:to~)?` + words(`school`, `class`, `college`, `campus`, `uni`) + `~` + threatTimes + `\b`)}
	threatWarningCues = []rule{
		bothViews(`\b` + words(
			words(`you'?ve`, `u'?ve`, `you~have`, `y'?all~have`, `yall~have`, `you~all~have`)+`~been~warned`,
			`consider~`+words(`this`, `yourself`, `yourselves`, `urself`, `yourselfs`)+`~warned`,
			`i'?m~warning~`+words(`you`, `u`, `everyone`, `everybody`, `y'?all`, `yall`),
			`this~is~(?:`+words(`a`, `my`, `your`, `ur`)+`~)?(?:`+words(`last`, `final`, `only`, `first`)+`~)?warning`,
			`if~`+words(`you`, `u`)+`~`+words(`want~to`, `wanna`, `wish~to`)+`~`+words(`live`, `stay~alive`, `survive`),
			words(`you'?ll`, `u'?ll`, `you~will`, `y'?all~will`)+`~see~what~happens`,
			`something~`+words(`big`, `bad`, `terrible`, `crazy`, `huge`)+`~`+
				words(`is~going~to`, `will`, `is~gonna`, `gonna`, `'?s~gonna`, `'?s~going~to`)+`~happen`,
		) + `\b`),
		// "you'll see" where the sentence ends on it, not "you'll see the new
		// timetable".
		bothViews(`\b` + words(`you'?ll`, `u'?ll`, `you~will`, `y'?all~will`) + `~see[ \t]*(?:[.!?\n]|$)`),
	}
)

// threatActs is what a threat says will be done, with the verbs in the form
// ing picks: the base form after "will" or "gonna", the -ing form after
// "i'm".
func threatActs(ing bool) string { return joinActs(threatActList(ing)) }

// threatActList is threatActs act by act.
func threatActList(ing bool) []threatAct {
	v := func(base, progressive string) string {
		if ing {
			return progressive
		}
		return base
	}
	var acts []threatAct
	// act adds what follows each of verbs, given as base and -ing pairs.
	act := func(rest string, verbs ...string) {
		for k := 0; k < len(verbs); k += 2 {
			acts = append(acts, threatAct{v(verbs[k], verbs[k+1]), rest})
		}
	}
	det := `(?:` + words(`the`, `my`, `our`, `this`, `that`, `ur`, `your`) + `~)?`
	owner := threatOwner
	act(`~`+threatPeople, `shoot~up`, `shooting~up`, `kill`, `killing`, `massacre`, `massacring`,
		`slaughter`, `slaughtering`, `attack`, `attacking`, `hurt`, `hurting`, `beat~up`, `beating~up`)
	// The acts that are never a figure of speech may be aimed at anyone
	// as well: "stab whoever sits next to me".
	act(`~`+strongThreatPeople, `shoot`, `shooting`, `stab`, `stabbing`, `murder`, `murdering`, `knife`,
		`knifing`, `shank`, `shanking`, `behead`, `beheading`, `gun~down`, `gunning~down`)
	// "beat you up"
	act(`~`+threatPeople+`~up`, `beat`, `beating`)
	act(`~`+det+threatPlaces, `shoot~up`, `shooting~up`, `shoot~(?:at|in|inside)`, `shooting~(?:at|in|inside)`,
		`bomb`, `bombing`, `blow~up`, `blowing~up`, `burn~down`, `burning~down`, `torch`, `torching`,
		`set~fire~to`, `setting~fire~to`, `attack`, `attacking`, `open~fire~(?:on|in|at|inside)`,
		`opening~fire~(?:on|in|at|inside)`)
	// "shooting up st. mary's": a place by its name.
	act(`~(?:`+words(`st`, `saint`)+`~)?[\pL\pN]+'s`, `shoot~up`, `shooting~up`)
	act(`~`+det+threatPlaces+`~on~fire`, `set`, `setting`)
	act(`~`+det+threatPlaces+`~down`, `burn`, `burning`)
	// "set it on fire", "burning the exam cell down": a place named
	// before, or one not on the list. threatCheck turns down a stage, a
	// dance floor or the internet.
	act(`~`+words(`it`, `this`, `that`, `them`, `everything`, `everyone`, `the~place`, `this~place`,
		`the~whole~place`, `the~building`)+`~on~fire`, `set`, `setting`)
	act(`~`+words(`it`, `this~place`, `the~place`, `the~whole~place`, `everything`, `the~building`),
		`set~fire~to`, `setting~fire~to`)
	act(`~`+owner+`~(?:[\pL\pN]+~){1,2}?down`, `burn`, `burning`)
	act(`~(?:the~)?`+words(`place`, `school`, `it`)+`~up`, `shoot`, `shooting`)
	act(``, `shoot~up`, `shooting~up`)
	act(`~`+weapon+`~`+words(`to`, `into`, `in`)+`~(?:`+words(`the`, `my`, `our`)+`~)?`+threatPlaces,
		`bring`, `bringing`, `take`, `taking`)
	// "bringing knives for the 12th graders"; not "a knife for the cake".
	act(`~`+weapon+`~for~`+threatPeople, `bring`, `bringing`, `take`, `taking`)
	// "I'm bringing a gun": a firearm or a bomb needs no place after it.
	act(`~`+firearm, `bring`, `bringing`)
	// "I'll have a knife", "I'll be carrying a gun": threatCheck turns
	// down the kitchen, a game and a play.
	act(`~`+weapon, `have`, `having`, `carry`, `carrying`, `pack`, `packing`, `hide`, `hiding`, `keep`, `keeping`,
		`be~carrying`, `be~carrying`, `be~packing`, `be~packing`, `be~armed~with`, `be~armed~with`)
	// "I'll be waiting at the stop with a knife".
	act(`~(?:[\pL\pN']+~){0,6}?with~`+weapon, `be~waiting`, `be~waiting`, `be~standing`, `be~standing`,
		`be~there`, `be~there`, `be~outside`, `be~outside`, `be~around`, `be~around`, `be~hiding`, `be~hiding`,
		`be~sitting`, `be~sitting`)
	act(`~`+words(`to`, `into`)+`~(?:`+words(`the`, `my`, `our`)+`~)?`+threatPlaces+`~with~`+weapon,
		`come`, `coming`)
	act(`~with~`+weapon+`~`+words(`to`, `into`)+`~(?:`+words(`the`, `my`, `our`)+`~)?`+threatPlaces,
		`come`, `coming`)
	act(`~(?:a~)?columbine`, `pull`, `pulling`)
	act(`~(?:`+words(`a`, `the`)+`~)?`+words(`bomb`, `explosives?`, `device`), `plant`, `planting`,
		`set~off`, `setting~off`, `detonate`, `detonating`)
	// Other ways of hurting people: "open fire in the auditorium", "poison
	// the mess food", "throw acid on her", "drive my car into the crowd",
	// "break your legs", "take a bat to his head", "beat the warden half to
	// death".
	act(`~`+words(`on`, `in`, `at`, `inside`), `open~fire`, `opening~fire`)
	act(`~(?:`+owner+`~)?(?:[\pL\pN]+~){0,2}?`+words(`food`, `water`, `drinks?`, `tea`, `coffee`, `lunch`, `dinner`,
		`breakfast`, `mess`, `canteen`, `tiffin`, `bottles?`, `juice`, `cake`, `sweets`), `poison`, `poisoning`)
	act(`~`+threatPeople, `poison`, `poisoning`)
	act(`~acid~`+words(`on`, `at`, `in`, `into`), `throw`, `throwing`)
	act(`~`+owner+`~(?:[\pL\pN]+~)?`+words(`car`, `truck`, `bike`, `scooter`, `van`, `jeep`, `suv`)+`~`+
		words(`into`, `through`)+`~(?:the~)?`+words(`crowd`, `crowds`, `people`, `students`, `kids`, `everyone`,
		`them`, `queue`, `line`, `assembly`), `drive`, `driving`)
	act(`~`+words(`your`, `ur`, `his`, `her`, `their`, `ya`, `everyone'?s`, `some(?:one|body)'?s`, `all~your`)+`~`+
		words(`legs?`, `arms?`, `bones?`, `neck`, `nose`, `jaw`, `face`, `skull`, `teeth`, `fingers?`, `ribs?`, `knees?`,
			`kneecaps?`, `spine`), `break`, `breaking`)
	acts = append(acts, threatAct{verb: takeAWeaponTo(v(`take`, `taking`))})
	act(`~(?:[\pL\pN']+~){1,3}?(?:half~)?to~death`, `beat`, `beating`)
	return acts
}

// threatAct is a verb and what follows it.
type threatAct struct{ verb, rest string }

// joinActs makes acts one alternation that matches what they would listed
// one by one, sorted so that the longest comes first where one act starts
// another: "shoot up the school" is read whole before "shoot up". Two acts
// can match at the same place only when one's first word starts the
// other's ("shoot" and "shoot", "be" and "beat"), so only those are listed
// one by one, in that order. Every other verb shares one alternative with
// the others followed by the same words, so that each list of people or
// places is spelled out once rather than once for every verb: listed one
// by one, the acts are 40% longer, and planning the filter's searches takes
// a fifth longer and more memory.
// TestJoinActsMatchesTheActsOneByOne holds the two to the same matches.
func joinActs(acts []threatAct) string {
	full := func(a threatAct) string { return a.verb + a.rest }
	slices.SortFunc(acts, func(a, b threatAct) int { return strings.Compare(full(b), full(a)) })
	acts = slices.CompactFunc(acts, func(a, b threatAct) bool { return full(a) == full(b) })
	// first is the word every match of an act starts with: its letters up
	// to the first that is not plain, less one the pattern makes optional.
	first := func(a threatAct) string {
		f := full(a)
		i := strings.IndexFunc(f, func(r rune) bool { return (r < 'a' || r > 'z') && r != '\'' })
		if i < 0 {
			return f
		}
		if strings.IndexByte("?*{", f[i]) >= 0 {
			i--
		}
		return f[:max(i, 0)]
	}
	related := func(x, y string) bool {
		return strings.HasPrefix(x, y) || strings.HasPrefix(y, x) ||
			strings.HasPrefix(canon(x), canon(y)) || strings.HasPrefix(canon(y), canon(x))
	}
	// alts are the alternatives in order; an act on its own is one, and the
	// verbs before a rest one between them, filled in at the end.
	var alts []string
	var verbs [][]string
	var rests []string
	group := map[string]int{} // which of alts holds the verbs before a rest
	for i, a := range acts {
		alone := true
		for j, b := range acts {
			if i != j && related(first(a), first(b)) {
				alone = false
				break
			}
		}
		if !alone {
			alts, verbs, rests = append(alts, full(a)), append(verbs, nil), append(rests, "")
			continue
		}
		k, ok := group[a.rest]
		if !ok {
			k = len(alts)
			group[a.rest] = k
			alts, verbs, rests = append(alts, ""), append(verbs, nil), append(rests, a.rest)
		}
		verbs[k] = append(verbs[k], a.verb)
	}
	for k, vs := range verbs {
		switch {
		case len(vs) == 1:
			alts[k] = vs[0] + rests[k]
		case len(vs) > 1:
			alts[k] = words(vs...) + rests[k]
		}
	}
	return words(alts...)
}

// weapon is what a threat says will be brought: "a gun", "my dad's gun".
// firearm leaves out the knife, which also cuts cake. threatOwner is whose a
// thing is: "the", "his", "coach's".
var (
	threatOwner = words(`the`, `my`, `our`, `this`, `that`, `ur`, `your`, `his`, `her`, `their`, `everyone'?s`,
		`[\pL\pN]+'s`)
	weaponOwner = `(?:` + words(`a`, `my`, `the`, `an`, `some`, `my~dad'?s`, `my~brother'?s`, `his`) + `~)?`
	weapon      = weaponOwner + words(`guns?`, `knife`, `knives`, `bombs?`, `weapons?`, `rifles?`, `pistols?`,
		`explosives?`, `ak`, `ar`, `shotguns?`, `revolvers?`, `machetes?`, `swords?`, `axe`, `daggers?`, `blades?`,
		`iron~rods?`, `rods?`, `acid`, `tasers?`)
	firearm = weaponOwner + words(`guns?`, `bombs?`, `rifles?`, `pistols?`, `explosives?`, `weapons?`, `ak(?:~?47)?`,
		`ar(?:~?15)?`, `shotguns?`, `revolvers?`)
)

// takeAWeaponTo is "take a bat to his head", with verb for "take".
func takeAWeaponTo(verb string) string {
	return verb + `~(?:` + words(`a`, `my`, `the`) + `~)?` + words(`bat`, `rod`, `stick`, `hammer`, `brick`, `chair`,
		`pipe`, `hockey~stick`, `cricket~bat`, `baseball~bat`, `crowbar`, `knife`, `machete`, `axe`) +
		`~to~(?:` + threatOwner + `~)?(?:[\pL\pN]+~)?` + words(`head`, `face`, `skull`, `knees?`, `legs?`)
}

var (
	// threatStrongActs are the acts read even in the plain present, where
	// "i kill it" and "we attack the problem" are ordinary.
	threatStrongActs = words(
		words(`shoot(?:~up)?`, `stab`, `gun~down`, `massacre`, `murder`, `kill`)+`~`+threatPeople,
		words(`shoot~up`, `bomb`, `blow~up`)+`~(?:`+words(`the`, `my`, `our`, `this`, `that`)+`~)?`+threatPlaces,
		`shoot~(?:the~)?`+words(`place`, `school`)+`~up`,
		`bring~`+weapon+`~`+words(`to`, `into`)+`~(?:`+words(`the`, `my`, `our`)+`~)?`+threatPlaces,
	)
	// threatStrongProgressive are the acts read with no subject at all.
	threatStrongProgressive = words(
		`shooting~up~(?:`+words(`the`, `my`, `our`, `this`, `that`)+`~)?`+threatPlaces,
		`shooting~up~(?:`+words(`st`, `saint`)+`~)?[\pL\pN]+'s`,
		words(`bringing`, `taking`)+`~`+weapon+`~`+words(`to`, `into`)+`~(?:`+words(`the`, `my`, `our`)+`~)?`+
			threatPlaces,
		words(`bringing`, `taking`)+`~`+weapon+`~for~`+threatPeople,
		takeAWeaponTo(`taking`),
		`pulling~(?:a~)?columbine`,
	)
)

// eventWords, among the first words after a place, make it an event rather
// than a place: "I will bomb the class maths test" is about failing it.
// messageWords, after "shoot you", make it a message: "I'll shoot you the
// notes tonight".
var (
	eventWords = canonSetOf(`presentation presentations test tests exam exams quiz play photo photos photoshoot project
		assignment report trip reunion party fest festival function shoot paper essay homework notes match game
		performance debate competition interview interviews midterm midterms viva vivas practical practicals boards
		olympiad assessment semester sem`)
	messageWords = canonSetOf(`message msg text txt email mail dm pm note notes line pic photo picture video link links
		reply invite snap call ping pdf pdfs slides slide doc docs file files answers solutions screenshot
		screenshots ss list details reminder update invite info sheet form`)
	// noEventExcuse is what no event makes harmless: an event straight
	// after "shoot up the school" or "bring a gun to school" is when it
	// happens, not what is meant.
	noEventExcuse = regexp.MustCompile(canon(`shoot(?:ing)?[^\pL\pN]*up|stab|knif|shank|behead|gun[^\pL\pN]*down|` +
		`bring|coming|come|plant|detonat|set[^\pL\pN]*off|burn|torch|fire|columbine`))
	// eventStops end the words read for an event: "bomb the school tomorrow
	// during the play" names a place and then a time.
	eventStops = canonSetOf(`tomorrow today tonight tmrw at in on during after before for with and`)
	// nextTarget words are what may sit between "shoot" and what it sends:
	// "shoot you guys a quick message".
	shootFillers = canonSetOf(`you u guys all both a an some my the quick short few couple of`)
	// threatThere is "there will be a ..." and "there's a bomb in ...", whose
	// next word may make it something else: "a shooting star", "a shooting
	// practice", "a bomb in the game".
	threatThere    = regexp.MustCompile(canon(`^there\b`))
	thereNotThreat = canonSetOf(`star stars range guard practice competition session schedule script scene day match
		event club contest game lesson tournament drill drills cyclone promo sale offer`)
	metaphorAttack = regexp.MustCompile(canon(`blood[^\pL\pN]*bath|massacre`))
	// marketWords make a bloodbath or a massacre one in the markets or at the
	// polls: "a bloodbath on Dalal Street", "a massacre at the polls".
	marketWords = canonSetOf(`market markets stock stocks shares sensex nifty dalal crypto bitcoin ipo polls poll
		election elections ballot votes voting`)
	// deadNotThreat, after "you're dead", is the idiom: "dead to me", "dead
	// tired", "dead wrong", "dead serious".
	deadNotThreat = canonSetOf(`to tired wrong right serious on inside set lucky funny cute gorgeous`)
	dieNotThreat  = canonSetOf(`someday eventually old age`)
	// conditionalPlace is a condition about being somewhere: "if you come to
	// school tomorrow you will die" threatens; "you will die if you eat that
	// canteen food" warns. It is read in the sentence round each threat.
	conditionalPlace = newWindowSearch(`\b(?:come|coming|go|going|show[^\pL\pN]*up|turn[^\pL\pN]*up|step|walk|enter|be)` +
		`[^\pL\pN]+(?:(?:to|into|in|at|inside)[^\pL\pN]+)?(?:(?:the|my|our)[^\pL\pN]+)?` + phrase(threatPlaces) + `\b`)
	conditionalPlaceLoose = newWindowSearch(canon(conditionalPlace.String()))
	// peopleShootPurpose is what makes a shoot of people a photo shoot.
	peopleShootPurpose = canonSetOf(`photos photo pictures pics portraits headshots yearbook photoshoot film movie video
		documentary photography`)
)

// threatCheck turns down the commonest harmless readings of an intent match:
// an event straight after it ("bomb the class presentation"), unless the act
// is one no event excuses; a message after "shoot" ("shoot you the notes"); a
// shoot that is for a film, the yearbook or photography club; a game named
// after "at", "in" or "on" ("we're gonna kill them at the match"), though not
// after "there will be a shooting" or "a bomb"; "a shooting star" and "a
// bloodbath in the maths exam"; "hurt her feelings" and "kill them with
// kindness"; "you're dead to me"; "you will die" in a game, in an exam, of
// shock, someday, when the results come out, or on a condition that is
// neither being somewhere nor a demand ("if you tell anyone", "unless you
// pay"); and a threat quoted in a report of one ("'I will shoot up the
// school', court hears").
func (f *scannedField) threatCheck(v *view, m []int) bool {
	text := v.text
	matched := text[m[0]:m[1]]
	cm := canon(matched)
	ws := wordsIn(matched)
	if len(ws) == 0 {
		return true
	}
	last := ws[len(ws)-1]
	after := clauseAfter(text, m[1])
	next := nextWords(after, 6)
	sentence := sentenceAround(text, m[0], m[1])
	if threatExcused(text, m, cm, ws, next, sentence) {
		return false
	}
	switch {
	case threatThere.MatchString(cm):
		// No game excuses a shooting or a bomb: "there will be a shooting at
		// the football game" is how a threat to a school's match is written.
		if len(next) > 0 && thereNotThreat[next[0]] {
			return false
		}
		if len(next) > 1 && is(next[0], "on") && is(next[1], "titan") {
			return false // "there will be attack on titan marathon tonight"
		}
		if ws := wordsIn(sentence); metaphorAttack.MatchString(cm) &&
			(anyIn(ws, eventWords) || anyIn(ws, gameWords) || anyIn(ws, marketWords)) {
			return false
		}
	case is(last, "dead") || is(last, "meat"):
		return !(len(next) > 0 && deadNotThreat[next[0]])
	case is(last, "hurt") && harmlessCondition(sentence) && !conditionalPlace.matchIn(v, sentence) &&
		!conditionalPlaceLoose.matchIn(v, sentence):
		return false // "you'll get hurt if you climb that wall"
	case is(last, "die"):
		if gameDeath(next) || anyIn(next, dieNotThreat) || strings.Contains(canon(after), canon("one day")) ||
			dieIdiom(next) || whenYouSee(next) || whenResults(next) || eventAfterPreposition(next) {
			return false
		}
		if harmlessCondition(sentence) && !conditionalPlace.matchIn(v, sentence) &&
			!conditionalPlaceLoose.matchIn(v, sentence) {
			return false
		}
	}
	if harmlessObject(next) {
		return false
	}
	if !noEventExcuse.MatchString(cm) {
		for _, w := range nextWords(after, 3) {
			if eventStops[w] {
				break
			}
			if eventWords[w] {
				return false
			}
		}
	}
	if strings.Contains(cm, "shoot") && !shootUp.MatchString(cm) {
		i := 0
		for i < len(next) && i < 4 && shootFillers[next[i]] {
			i++
		}
		if i < len(next) && i < 4 && messageWords[next[i]] {
			return false
		}
		if filmShoot(sentenceAfter(text, m[1]), peopleTarget(cm)) {
			return false
		}
		// "shooting at the gym tonight for basketball practice".
		if shootAtPlace.MatchString(cm) {
			for _, w := range next {
				if gameWords[w] || shootingSports[w] {
					return false
				}
			}
		}
	}
	// A game named nearby excuses a kill only where the sentence names no
	// place: "i will kill everyone at school tomorrow then play valo" holds.
	if gameVerb.MatchString(cm) && (gameAfterAPreposition(next) ||
		sportKill.MatchString(cm) && sportAfterAPreposition(next, wordsIn(sentence)) &&
			sportBanter(slices.Concat(ws, next), wordsIn(sentence)) ||
		gameNamed(text, m) && !placeNamed(wordsIn(sentence))) {
		return false
	}
	// "I'm shooting all of you on ascent": a shot in a game, where no place
	// is named.
	if (strings.Contains(cm, "shoot") || strings.Contains(cm, "stab")) && gameNamed(text, m) &&
		!placeNamed(wordsIn(sentence)) {
		return false
	}
	return true
}

// threatExcused reads the threats that name no one doing them, the Hinglish
// ones, and the acts that are as often a kitchen, a game or a stage, for
// what makes them something else. ws are the match's words, next the words
// after it in its clause, and sentence the sentence round it.
func threatExcused(text string, m []int, cm string, ws, next []string, sentence string) bool {
	first, last := strings.TrimLeft(ws[0], "'"), ws[len(ws)-1]
	sws := wordsIn(sentence)
	before := wordsIn(clauseBefore(text, m[0]))
	prev := ""
	if p := wordsBefore(text, m[0], 1); len(p) > 0 {
		prev = p[0]
	}
	// relative is a subject that names whoever does something: "the next
	// person who", "anyone who", "every teacher".
	relative := anyIn(before, threatRelatives)
	// mine is a clause about the poster or their things: "I'll get killed",
	// "my phone is getting blown up".
	nearest := ""
	if len(before) > 0 {
		nearest = before[len(before)-1]
	}
	mine := firstPerson[nearest] || len(before) > 0 && firstPerson[before[0]] && !relative
	// A passive is said of its own subject, not the clause's first word: "i
	// swear he's getting stabbed" is said of him, "you're getting stabbed"
	// of the reader, and only "I'll get killed" or "my phone is getting
	// blown up" of the poster.
	passiveMine, passiveYou := passiveSubject(before)
	// "tujhe kal cricket mein dho dunga", "match mein peet dunga": beating
	// someone in a game, with no blade or gun in it.
	if anyIn(ws, hindiBeatWords) && !anyIn(ws, hindiBladeWords) && hindiGameNamed(sws) {
		return true
	}
	switch {
	case passiveHarm.MatchString(cm):
		destroyed := passiveRuin.MatchString(cm)
		switch {
		case is(first, "getting") && !progressiveBe[prev] && !secondPerson[prev],
			passiveMine && !relative, passiveYou && harmlessCondition(sentence),
			// A fear, a belief or a rumour about it: "i'm scared he's getting
			// jumped", "i heard he's getting jumped", not "i swear he's
			// getting stabbed".
			anyIn(before, worryWords),
			len(next) > 1 && is(next[0], "by") && !is(next[1], "me") && !is(next[1], "us") && !soonWords[next[1]] &&
				!byTime[next[1]],
			is(last, "shot") && len(next) > 0 && shotNotThreat[next[0]],
			is(last, "jumped") && len(next) > 0 && jumpedNotHarm[next[0]],
			is(last, "stabbed") && len(next) > 2 && is(next[0], "in") && is(next[2], "back"),
			is(last, "killed") && len(next) > 0 && is(next[0], "off"),
			habitual(sws), anyIn(sws, storyWords), gamesIn(sws),
			destroyed && !anyIn(before, threatPlaceWords),
			destroyed && is(last, "fire") && anyIn(sws, stageWords),
			is(first, "gets") && !relative && !anyIn(next[:min(len(next), 4)], soonWords):
			return true
		}
	case is(last, "coming"), hasWord(ws, "pay"):
		return mine || anyIn(sws, karmaWords)
	case hasWord(ws, "with") && (hasWord(ws, "end") || hasWord(ws, "ends")):
		return anyIn(ws, firstPerson) || anyIn(sws, storyWords) || len(next) > 0 && endsWithErrand[next[0]]
	case is(last, "alive"):
		return anyIn(sws, storyWords) || gamesIn(sws)
	case is(last, "die") && everyoneIn.MatchString(cm):
		return anyIn(ws, eventWords) || anyIn(ws, foodWords)
	case hinglishCarry.MatchString(cm):
		// "station se utha lunga", "school se utha lenge" are a lift.
		for i, w := range ws {
			if is(w, "se") && i > 0 && (pickupWords[ws[i-1]] || threatPlaceWords[ws[i-1]]) {
				return true
			}
		}
		return anyIn(ws, pickupWords) || is(first, "ko") && hinglishNotTarget[prev]
	case hinglishBlast.MatchString(cm):
		return !anyIn(sws, threatPlaceWords) || anyIn(sws, stageWords) || anyIn(sws, eventWords) ||
			anyIn(sws, festivalWords) || anyIn(sws, hypeWords) || gamesIn(sws)
	case hinglishReady.MatchString(cm):
		return unnegatedIn(sws, propWords, gameWords, toyWeapon, festivalWords) || gameNamed(text, m)
	case is(first, "ko") && hasWord(ws, "dho"):
		// "gaadi ko dho dunga" is the washing: only "tujhe dho dalunga",
		// said to a person, is the thrashing.
		return true
	case is(first, "ko"):
		return hinglishNotTarget[prev]
	case hinglishFire.MatchString(cm):
		// "aag laga denge" in a performance or an event is the idiom for
		// doing it brilliantly: "presentation mein aag laga denge".
		if anyIn(sws, stageWords) || anyIn(sws, lampWords) || anyIn(sws, examWords) ||
			anyIn(sws, eventWords) && !anyIn(sws, fuelWords) {
			return true
		}
		// "jala" is lighting a lamp as often as burning: only a person or
		// a place before it makes it a threat.
		if strings.HasPrefix(cm, "j") {
			bw := wordsBefore(text, m[0], 8)
			return !hasWord(bw, "ko") && !anyIn(bw, threatPlaceWords)
		}
	case hinglishBring.MatchString(cm):
		return toyWeapon[first] || toyWeapon[prev] || anyIn(sws, festivalWords)
	case weaponHeld.MatchString(cm):
		return unnegatedIn(sws, propWords, gameWords)
	case setOnFire.MatchString(cm):
		return anyIn(sws, stageWords) && !anyIn(sws, fuelWords)
	case strings.HasSuffix(cm, "death"):
		// "beat this joke to death": what is beaten is between the verb and
		// "to".
		for i, w := range ws {
			if !is(w, "beat") && !is(w, "beating") {
				continue
			}
			between := ws[i+1:]
			if j := slices.Index(between, "to"); j >= 0 {
				between = between[:j]
			}
			return anyIn(between, deadHorse) || len(between) == 1 && deadPronouns[between[0]]
		}
	}
	return false
}

// hindiGameNamed reports a game in a sentence with the Hinglish "in" or "at"
// after it: "cricket mein", "match pe", "carrom mein". "match ke baad",
// after the match, is only when.
func hindiGameNamed(ws []string) bool {
	for i, w := range ws[:max(0, len(ws)-1)] {
		if gameWords[w] && hindiInGame[ws[i+1]] && !anyIn(ws[max(0, i-3):i], excuseNegators) {
			return true
		}
	}
	return false
}

var (
	// hindiBeatWords are the Hinglish verbs of beating, killing and
	// thrashing, which a game uses of its players too; hindiBladeWords are
	// the arms none does.
	hindiBeatWords = canonSetOf(`maar peet pit thok thokk tapka dho maarunga marunga maroonga maaroonga maarungi
		marungi maarenge marenge peetunga pitunga peetenge pitenge thokunga thokenge tapkaunga tapkaenge tapkayenge`)
	hindiBladeWords = canonSetOf(`goli chaku chakku chaaku chakoo chhura chhuri chura churi knife talwar talvar`)
	hindiInGame     = canonSetOf(`mein me main mai mei pe par`)
)

// habitual reports a sentence about what happens again and again: "kids get
// shot there every week", "he always gets killed first".
func habitual(ws []string) bool {
	for i, w := range ws {
		if habitualWords[w] || is(w, "every") && i+1 < len(ws) && habitualUnits[ws[i+1]] ||
			is(w, "all") && i+2 < len(ws) && is(ws[i+1], "the") && is(ws[i+2], "time") {
			return true
		}
	}
	return false
}

var (
	// passiveHarm is a threat with nobody doing it: "gets stabbed", "is
	// getting shot", "will be blown up", and passiveRuin the ones done to a
	// place.
	passiveHarm = regexp.MustCompile(canon(`^'?(?:gets|getting|will|ll|gonna|going)` +
		`(?:[^\pL\pN]+(?:get|be|to|finally|definitely|literally|actually|all|really))*[^\pL\pN]+` +
		`(?:stabbed|shot|knifed|jumped|killed|murdered|gunned[^\pL\pN]+down|slaughtered|massacred|` +
		`beaten[^\pL\pN]+to[^\pL\pN]+death|blown[^\pL\pN]+up|bombed|burn(?:ed|t)[^\pL\pN]+down|torched|` +
		`set[^\pL\pN]+on[^\pL\pN]+fire|on[^\pL\pN]+fire)$`))
	passiveRuin = regexp.MustCompile(canon(`(?:blown|bombed|burn|torched|fire)`))
	// setOnFire is setting something on fire or burning it down, which a
	// stage or a dance floor may be: "set the stage on fire".
	setOnFire = regexp.MustCompile(canon(`\b(?:set|setting)\b.*\bfire$|\bfire[^\pL\pN]+to\b|\bburn(?:ing)?\b`))
	// everyoneIn is "everyone in the lab ... is going to die".
	everyoneIn = regexp.MustCompile(canon(`^(?:everyone|everybody|every|all|y'?all|yall|you)\b`))
	// hinglishCarry is "utha lenge", hinglishBlast "dhamaka hoga", and
	// hinglishReady "chaku le liya hai".
	hinglishCarry = regexp.MustCompile(canon(`\butha\b`))
	hinglishBlast = regexp.MustCompile(canon(`^(?:dhamaka|dhamakaa|dhmaka|blast)\b`))
	hinglishReady = regexp.MustCompile(canon(`\b(?:liya|li|ready|tayyar|taiyar|taiyaar|load|loaded|gaya|gayi|gya|gyi)$`))
	// pickupWords make carrying someone off a lift: "station se utha
	// lunga".
	pickupWords = canonSetOf(`station stop stand airport railway metro bus`)
	// hypeWords make "dhamaka hoga" a party: "kal college mein dhamaka hoga,
	// DJ aa raha hai".
	hypeWords = canonSetOf(`party dj sale offer launch news announcement result results reveal trailer song album
		movie film release show concert performance entry`)
	// hinglishFire and hinglishBring are "aag laga dunga" or "jala dunga",
	// and "chaku leke aaunga".
	hinglishFire  = regexp.MustCompile(canon(`^(?:aag|jala|jalaa|jla)\b`))
	hinglishBring = regexp.MustCompile(canon(`^[\pL\pN]+[^\pL\pN]+(?:le|leke|lekar|laaunga|launga|laoonga|` +
		`laaungi|laungi|laaenge|laayenge|layenge)\b`))
	// weaponHeld is "have", "carry" or "keep" a weapon, or wait with one.
	weaponHeld = regexp.MustCompile(canon(`\b(?:have|having|carry|carrying|pack|packing|hide|hiding|keep|keeping|` +
		`with)[^\pL\pN]+(?:(?:a|my|the|an|some|his)[^\pL\pN]+)?(?:my[^\pL\pN]+(?:dad|brother)'?s[^\pL\pN]+)?` +
		`[\pL\pN]+(?:[^\pL\pN]+rods?)?$`))

	threatRelatives = canonSetOf(`who whoever whomever anyone anybody next first whichever every each`)
	secondPerson    = canonSetOf(`you u ya yall y'all you're youre ur`)
	progressiveBe   = canonSetOf(`is s are re am m`)
	shotNotThreat   = canonSetOf(`down on for`)
	// jumpedNotHarm, after "getting jumped", is a leap or a place in a
	// queue: "jumped into the crew", "jumped over", "jumped ahead".
	jumpedNotHarm = canonSetOf(`into over through off ahead across up`)
	// worryWords, in the clause before a passive harm, make it a fear or a
	// rumour told rather than a threat: "i'm worried you're getting jumped".
	worryWords = canonSetOf(`scared afraid worried worry worrying fear feared terrified nervous concerned think thought
		heard hear hearing rumour rumor rumours rumors apparently hope hoping pray praying guess suspect wonder`)
	habitualWords = canonSetOf(`everyday daily always often usually sometimes regularly constantly whenever`)
	habitualUnits = canonSetOf(`day night week month year time single`)
	// storyWords make a death one in a story: "he gets killed in season 3".
	storyWords = canonSetOf(`episode episodes season seasons movie movies film films show shows series book books
		novel novels chapter chapters anime manga scene scenes character characters ending finale spoiler spoilers
		trailer plot netflix drama serial webseries cartoon comic comics story fanfic fiction`)
	// threatPlaceWords are threatPlaces and the other places a fire or a bomb
	// may be aimed at, word by word.
	threatPlaceWords = canonSetOf(strings.NewReplacer("`", " ", ",", " ", "~", " ", "?", "").Replace(
		`airport, assembly, auditorium, bathroom, bus, cafeteria, campus, canteen, center, centre, church, class,
		classroom, coaching, college, concert, corridor, dorm, farewell, ghar, gurdwara, gurudwara, gym, hall,
		hallway, hospital, hostel, lab, library, mall, mess, metro, mosque, office, parking, playground, restroom,
		school, seminar, stadium, staffroom, station, synagogue, temple, toilet, toilets, tuition, uni, university,
		washroom, building, house, home, room, block, flat, car, scooter, bike, shop, store, ute, kamra, kamre,
		locker, lockers`))
	byTime    = canonSetOf(`then now noon midnight morning evening afternoon night end lunch break`)
	soonWords = canonSetOf(`tomorrow tmrw tmr tonight next kal parso aaj today monday tuesday wednesday thursday
		friday saturday sunday mon tue tues wed thu thurs fri sat sun`)
	// stageWords make setting something on fire a performance: "set the
	// stage on fire", "insta pe aag laga dunga".
	stageWords = canonSetOf(`stage floor dancefloor mic internet twitter insta instagram reel reels timeline charts
		world dance performance perform performing song songs track tracks rap verse beat beats show fest festival
		party competition match game crowd audience runway ramp gaana gana mehfil function concert`)
	// fuelWords make it a fire after all: "set the stage on fire with petrol".
	fuelWords = canonSetOf(`petrol kerosene diesel gasoline fuel lighter matchbox zinda alive`)
	lampWords = canonSetOf(`diya diye diyas candle candles mombatti lamp lamps light lights bulb gas chulha stove
		agarbatti dhoop patakhe patake cracker crackers phuljhadi holika lohri bonfire kachra kooda garbage raavan
		ravan ravana effigy putla dil`)
	// propWords make carrying a knife or a sword cooking, a game or a play.
	propWords = canonSetOf(`cake cakes fruit fruits onion onions vegetables veggies butter bread kitchen cooking cook
		chopping chop apple apples mango mangoes vegetable sandwich pizza pencil pencils sharpener craft crafts art
		prop props toy toys nerf water airsoft paintball cosplay costume costumes play drama skit halloween fancy
		dress rehearsal scene stage character safety defence defense protection`)
	foodWords = canonSetOf(`eat eats ate eating food foods lunch dinner breakfast biryani maggi mess`)
	// karmaWords make what is coming to someone fate, not the poster's doing.
	karmaWords = canonSetOf(`karma`)
	// endsWithErrand, after "will end with a", makes the ending a trip's or
	// a day's: "end with a hospital visit".
	endsWithErrand = canonSetOf(`visit visits duty shift shifts internship rounds tour trip staff appointment
		checkup check bill bills food canteen`)
	// examWords make burning something an exam's: "exam paper jala dunga".
	examWords = canonSetOf(`exam paper`)
	// hinglishNotTarget, before "ko ... maar dunga", is a pest or a test,
	// not a person: "machhar ko maar dunga", "exam ko maar dunga".
	hinglishNotTarget = canonSetOf(`machhar macchar machar mosquito mosquitoes makkhi cockroach cockroaches chuha
		chuhe chuhon keeda keede kide exam exams paper test boredom bore neend bhook bhookh time mood game level boss
		sawal question questions saman samaan bag bags phone call kachra kooda`)
	toyWeapon     = canonSetOf(`water toy nerf glue nakli khilona paani pani pichkari airsoft paintball bubble spray`)
	festivalWords = canonSetOf(`diwali deepawali patakhe patake cracker crackers holi pichkari`)
	// deadHorse, beaten to death, is a topic: "beat this joke to death".
	deadHorse = canonSetOf(`topic subject horse question joke jokes meme memes trend argument point issue
		discussion concept idea song`)
	deadPronouns = canonSetOf(`it this that me myself us`)
)

// harmlessObject reports what makes hurting or killing someone harmless,
// straight after the match: their feelings ("hurt her feelings"), or kindness
// as the weapon ("kill them with kindness", "kill you with my puns").
func harmlessObject(next []string) bool {
	switch {
	case len(next) > 0 && hurtNotThreat[next[0]]:
		return true
	case len(next) > 1 && is(next[0], "with"):
		w := next[1]
		if len(next) > 2 && (is(w, "my") || is(w, "our") || is(w, "his") || is(w, "her")) {
			w = next[2]
		}
		return killWithKindness[w]
	}
	return false
}

var (
	hurtNotThreat    = canonSetOf(`feelings feeling ego pride sentiments chances vibe vibes mood`)
	killWithKindness = canonSetOf(`kindness love compliments compliment jokes joke puns pun cuteness questions facts
		logic laughter memes sarcasm smiles smile hugs sweetness niceness`)
)

// eventAfterPreposition reports an event after "in", "at", "on" or "during",
// a word or two of determiner between: "you're going to die in the chemistry
// exam tomorrow".
func eventAfterPreposition(next []string) bool {
	if len(next) < 2 || !is(next[0], "in") && !is(next[0], "at") && !is(next[0], "on") && !is(next[0], "during") {
		return false
	}
	for _, w := range next[1:min(len(next), 4)] {
		if eventWords[w] {
			return true
		}
		if is(w, "school") || is(w, "class") {
			return false
		}
	}
	return false
}

// whenResults reports the hyperbole "you're gonna die when the results come
// out": "when" and a result within the three words after it, with no "me",
// "I" or "we" in what follows.
func whenResults(next []string) bool {
	if len(next) < 2 || !is(next[0], "when") {
		return false
	}
	return anyIn(next[1:min(len(next), 4)], resultWords) && !anyIn(next[1:], firstPerson)
}

var resultWords = canonSetOf(`result results marks mark score scores grades grade rank ranks cutoff cutoffs
	percentile percentiles paper papers answer report card`)

// whenYouSee reports the hyperbole "you're gonna die when you see the
// results", "when you hear what happened": "when you" and a verb of finding
// out, with no "me", "I" or "we" after it ("when you see me at school" is a
// threat).
func whenYouSee(next []string) bool {
	if len(next) < 3 || !is(next[0], "when") || !is(next[1], "you") && !is(next[1], "u") || !findOutWords[next[2]] {
		return false
	}
	return !anyIn(next[3:], firstPerson)
}

var findOutWords = canonSetOf(`see hear read find watch look learn realise realize get open check`)

// gameNamed reports a video game, or the words of one, in the sentence round
// a match or the 64 bytes before it: "valo 5 stack tomorrow 8pm, we're gonna
// slaughter them", "Among Us after class? I'm killing all of you".
func gameNamed(text string, m []int) bool {
	ws := wordsIn(text[max(0, m[0]-64):m[0]] + " " + sentenceAround(text, m[0], m[1]))
	for i, w := range ws {
		if gameTitles[w] || i+1 < len(ws) && gamePairs[w] != "" && is(ws[i+1], gamePairs[w]) {
			return true
		}
	}
	return false
}

var (
	// gameTitles are games and words only a game has: Valorant's maps
	// among them, but not those that are also words ("split", "bind"), nor
	// "lobby", which is also a place.
	gameTitles = canonSetOf(`valo valorant fortnite minecraft roblox pubg bgmi freefire warzone amogus csgo cs2 apex
		overwatch gta fifa coc dota r6 tekken ranked respawn smp bedwars skywars ascent icebox`)
	// gamePairs are games named in two words, by the first.
	gamePairs = canonPairs(map[string]string{"among": "us", "free": "fire", "counter": "strike",
		"rocket": "league", "brawl": "stars", "rainbow": "six", "5": "stack", "five": "stack", "clash": "royale"})
	// playedGames and playedPairs are games that excuse a kill only named
	// straight after "at", "in" or "on" ("murder you in Smash", "kill you at
	// ping pong"), since their words are also verbs, seasons and the like
	// elsewhere ("smash his face in", "kill you in the fall").
	playedGames = canonSetOf(`smash paintball scrabble monopoly kahoot splatoon pokemon darts bowling foosball
		airsoft jenga pictionary trivia mariokart tetris`)
	playedPairs = canonPairs(map[string]string{"mario": "kart", "laser": "tag", "ping": "pong",
		"spelling": "bee", "fall": "guys", "just": "dance", "table": "tennis", "air": "hockey",
		"street": "fighter", "mortal": "kombat", "smash": "bros", "super": "smash", "pokemon": "go"})
	// timeBefore makes a place a time: "after school", "before class".
	timeBefore = canonSetOf(`after before till until til`)
)

// canonPairs adds to pairs of words their loose view's spelling of the first,
// which is how the loose view's text has it ("ciash" for "clash"); the
// second is compared with is, which reads either.
func canonPairs(pairs map[string]string) map[string]string {
	for k, v := range pairs {
		pairs[canon(k)] = v
	}
	return pairs
}

// placeNamed reports a place a threat may be aimed at among ws, where it is
// not a time: "at school", not "after school" or "before class".
func placeNamed(ws []string) bool {
	for i, w := range ws {
		if threatPlaceWords[w] && !(i > 0 && timeBefore[ws[i-1]]) {
			return true
		}
	}
	return false
}

// gameAfterAPreposition reports a game named after "at", "in" or "on" in the
// words after a match: "kill them at the match", "murder you on Mario Kart",
// not "kill my teacher after the game". A school or a class first ends the
// look, unless it is a time: "murder you at Tekken after school".
func gameAfterAPreposition(next []string) bool {
	for i, w := range next {
		if (is(w, "school") || is(w, "class")) && !(i > 0 && timeBefore[next[i-1]]) {
			return false
		}
		if w != "at" && w != "in" && w != "on" {
			continue
		}
		for k := i + 1; k < min(len(next), i+4); k++ {
			g := next[k]
			if is(g, "school") || is(g, "class") {
				return false // "at the school football game"
			}
			if gameWords[g] || gameTitles[g] || playedGames[g] ||
				k+1 < len(next) && (gamePairs[g] != "" && is(next[k+1], gamePairs[g]) ||
					playedPairs[g] != "" && is(next[k+1], playedPairs[g])) {
				return true
			}
		}
	}
	return false
}

// passiveSubject reads who a passive harm is said of from the clause
// before it: the word in front of its auxiliaries ("he's", "you will",
// "my phone is"), with its contraction cut ("he's" is "he"). It is the
// poster's for a first-person subject or one owned by the poster ("my
// phone", "our new car"), and the reader's for a second-person one.
func passiveSubject(before []string) (mine, yours bool) {
	k := len(before) - 1
	for k >= 0 && (auxWords[before[k]] || auxWords[strings.TrimLeft(before[k], "'")]) {
		k--
	}
	if k < 0 {
		return false, false
	}
	head := before[k]
	if i := strings.IndexByte(head, '\''); i > 0 {
		head = head[:i]
	}
	switch {
	case firstPerson[before[k]] || firstPerson[head]:
		return true, false
	case secondPerson[before[k]] || secondPerson[head]:
		return false, true
	}
	for j := k - 1; j >= max(0, k-2); j-- {
		if is(before[j], "my") || is(before[j], "our") {
			return true, false
		}
	}
	return false, false
}

// harmlessCondition reports an "if" in a sentence whose condition is not a
// demand: "you will die if you eat that canteen food" warns, but "you will
// die tomorrow if you tell anyone" and "unless you pay" coerce, and so does
// any "unless".
func harmlessCondition(sentence string) bool {
	ws := wordsIn(sentence)
	harmless := false
	for i, w := range ws {
		switch {
		case w == "unless" || w == canon("unless"):
			return false
		case w == "if":
			if anyIn(ws[i+1:min(len(ws), i+7)], coercedActs) {
				return false
			}
			harmless = true
		}
	}
	return harmless
}

// coercedActs, in a condition, make a threat a demand: telling, reporting,
// paying, sending.
var coercedActs = canonSetOf(`tell tells telling told snitch snitches snitching report reports reporting rat
	pay pays paying send sends sending show complain complains complaining inform leak talk talks say says
	call delete expose`)

// peopleTarget reports a shoot aimed at people rather than at a place.
func peopleTarget(cm string) bool { return !shootAtPlace.MatchString(cm) && !shootUp.MatchString(cm) }

// filmShoot reads what a shoot is for in the rest of its sentence: "for our
// film project", "for the yearbook", "for photography club". "For real, not a
// video" says the opposite, and a shoot of people is a photo shoot only for
// photos or a film, not "for my channel".
func filmShoot(rest string, people bool) bool {
	m := filmPurpose.FindStringSubmatchIndex(canon(rest))
	if m == nil {
		return false
	}
	between := wordsIn(canon(rest)[m[0]:m[2]])
	if anyIn(between, canonSetOf(`not no never real really`)) {
		return false
	}
	return !people || peopleShootPurpose[canon(rest)[m[2]:m[3]]]
}

// namedTargetCheck takes "shoot Rahul" when the name is written with a
// capital in the source, or comes after a title ("mr sharma", "sir"), or in
// small letters where lowerName takes it and the phrase ends there ("kill
// kabir after school"), and is not a day, a month, an exam, a subject, a
// team, a country or a game, and then reads it as threatCheck reads any
// threat. A word of up to four capitals in a sentence that is not all
// capitals is an acronym — "kill JEE", "kill CSK" — and "Rahul's" followed by
// an occasion makes the occasion the target: "kill Rahul's wedding".
func (f *scannedField) namedTargetCheck(v *view, m []int) bool {
	titled := m[2] >= 0 && m[3] > m[2]
	a, b := v.span(m[4], m[5])
	name := f.src[a:b]
	r, _ := utf8.DecodeRuneInString(name)
	lower := strings.ToLower(name)
	if notANameTarget[lower] || gameWords[lower] || sportWords[lower] {
		return false
	}
	// A name in small letters is read as one only where lowerName takes it
	// and the phrase ends after it: "kill kabir after school", not "kill
	// brain cells".
	if next := nextWords(v.text[m[5]:], 1); !titled && !unicode.IsUpper(r) &&
		!(lowerName(name) && (len(next) == 0 || nounEnds[next[0]])) {
		return false
	}
	if !titled && utf8.RuneCountInString(name) <= 4 && strings.ToUpper(name) == name {
		if lead, _ := v.span(m[0], m[4]); strings.ToUpper(f.src[lead:a]) != f.src[lead:a] {
			return false
		}
	}
	if rest, ok := possessive(v.text[m[5]:]); ok {
		if next := nextWords(rest, 1); len(next) == 1 && (eventWords[next[0]] || possessedOccasions[next[0]]) {
			return false
		}
	}
	return f.threatCheck(v, m)
}

// possessive reads the "'s" (or, after a name ending in s, the "'") that
// makes the words before it an owner, and returns what follows it.
func possessive(rest string) (string, bool) {
	switch {
	case strings.HasPrefix(rest, "'s"):
		return rest[2:], true
	case strings.HasPrefix(rest, "'"):
		return rest[1:], true
	}
	return rest, false
}

// notANameTarget are the capitalised words after "kill" that are not a
// person: days and months, exams, subjects, teams, countries and cities, and
// words that are capitalised at the start of a sentence.
var notANameTarget = setOf(`monday tuesday wednesday thursday friday saturday sunday january february march april
	may june july august september october november december
	physics chemistry chem maths math mathematics biology bio english hindi history geography economics eco
	accounts accountancy commerce sanskrit french german spanish science it cs computer computers programming
	coding python java calculus calc algebra geometry trigonometry trig statistics stats probability organic
	inorganic physical thermodynamics thermo kinematics mechanics optics electrostatics electronics
	psychology sociology polity politics philosophy literature civics zoology botany anatomy physiology
	jee neet boards board cbse icse isc gate cat sat act gre gmat ielts toefl upsc ssc nda cuet clat bitsat
	viteee comedk mains advanced kvpy olympiad cet mht eapcet eamcet mock mocks finals midterms semester
	australia england pakistan india lanka bangladesh afghanistan zealand africa windies nepal ireland
	scotland netherlands brazil argentina france germany spain italy portugal japan china korea america
	arsenal barcelona barca madrid real chelsea liverpool united city spurs tottenham bayern juventus juve
	milan inter dortmund ajax celtic everton villa newcastle lakers warriors celtics bulls
	chennai mumbai kolkata bangalore bengaluru delhi hyderabad punjab rajasthan lucknow gujarat pune
	the this that everyone everybody time god you him her them me myself himself herself all`)

// possessedOccasions, after "Rahul's", make an occasion or a thing of his
// the target, as eventWords do: "kill Rahul's wedding", "kill Rahul's vibe".
var possessedOccasions = canonSetOf(`wedding birthday bday vibe vibes mood chances chance record records score streak
	plan plans joke jokes idea ideas dream dreams speech song songs playlist outfit look hairstyle recital
	reception sangeet farewell debut launch startup channel career`)

// schoolShootingCheck turns down "school shooting" in a drill, a lesson or
// the news, and one the words round it put in the past: "a school shooting
// drill tomorrow", "an essay on school shooting prevention", "there was a
// school shooting in texas on monday". A massacre or a bloodbath at school is
// a day of exams as often, or a match: "monday's going to be a massacre at
// our school, three exams back to back".
func schoolShootingCheck(v *view, m []int) bool {
	before := wordsIn(clauseBefore(v.text, m[0]))
	near := append(before[max(0, len(before)-4):], nextWords(clauseAfter(v.text, m[1]), 4)...)
	if anyIn(near, schoolShootingTopics) || anyIn(near, shootingPast) {
		return false
	}
	if metaphorAttack.MatchString(canon(v.text[m[0]:m[1]])) {
		ws := wordsIn(sentenceAround(v.text, m[0], m[1]))
		return !anyIn(ws, eventWords) && !anyIn(ws, gameWords) && !anyIn(ws, marketWords)
	}
	return true
}

var (
	schoolShootingTopics = canonSetOf(`drill drills prevention essay essays debate debates discussion discuss discussing
		discussed news article articles report reports documentary awareness lockdown training survivors survivor
		victims memorial statistics stats project presentation research policy laws law safety protest protests trial
		court verdict hearing vigil podcast lecture seminar talk webinar book movie film studying studied watched
		watching`)
	// shootingPast put a shooting in the past: "there was", "last year".
	shootingPast = canonSetOf(`was were wasn't happened had yesterday ago last remember anniversary since survived`)
)

var (
	// shootUp is "shoot up" and "shooting up", which is never a film shoot.
	shootUp = regexp.MustCompile(`shoot(?:ing)?[^\pL\pN]*up\b`)
	// shootAtPlace is shooting at or in a place, rather than at people,
	// which a sport does as well.
	shootAtPlace   = regexp.MustCompile(`shoot(?:ing)?[^\pL\pN]*(?:at|in|inside)\b`)
	shootingSports = canonSetOf(`practice training hoops baskets goals penalties archery arrows range nets`)
	// filmPurpose says what a shoot is for: "for our film project", "for the
	// yearbook", "for photography club", with the purpose as its group.
	// Compiled through canon, and matched against canon'd text, so it reads
	// either view.
	filmPurpose = regexp.MustCompile(canon(`\bfor[^\pL\pN]+(?:\pL+[^\pL\pN]+){0,3}?(` + words(`film`, `films`,
		`movie`, `video`, `videos`, `vlog`, `reel`, `reels`, `yearbook`, `documentary`, `photoshoot`, `photos?`,
		`pictures`, `pics`, `portraits`, `headshots`, `trailer`, `ad`, `advert`, `commercial`, `short`, `tiktok`,
		`youtube`, `channel`, `project`, `magazine`, `newsletter`, `photography`) + `)\b`))
	// gameVerb is the verbs people beat each other with at a game.
	gameVerb = regexp.MustCompile(canon(`\b` + words(`kill`, `killing`, `murder`, `murdering`, `slaughter`,
		`slaughtering`, `massacre`, `massacring`, `destroy`, `destroying`, `beat`, `beating`, `attack`,
		`attacking`) + `\b`))
	// gameWords are games and their occasions. A sport played with a bat or
	// a club that is also a weapon ("baseball", "golf") is left out, since a
	// game word in the sentence excuses carrying one: "bringing a baseball
	// bat to school tomorrow and he's getting it" holds.
	gameWords = canonSetOf(`match matches game games chess football soccer cricket basketball volleyball hockey
		tennis badminton kabaddi tournament finals final semis semifinal semifinals quiz debate competition race
		fifa fortnite valorant pubg bgmi minecraft roblox cod league olympiad hackathon contest sports carrom
		ludo uno mock battle bands`)
	// sportWords are school sports that excuse nothing but a kill or a
	// murder said of the other side, named straight after "at", "in" or
	// "on": "we're going to murder them at dodgeball". They are kept out of
	// gameWords, which also excuses a blade carried ("a knife to rugby
	// practice"), a teacher who won't see monday ("the rugby coach") and a
	// shooting at a place, all of which a sport named nearby leaves a threat.
	sportWords = canonSetOf(`dodgeball netball rugby handball rounders futsal kickball frisbee pickleball`)
	// sportKill is the verbs sportWords excuse: not "beat him up" or
	// "attack them", which a sport's own verbs do not say.
	sportKill = regexp.MustCompile(canon(`\b` + words(`kill`, `killing`, `murder`, `murdering`, `slaughter`,
		`slaughtering`, `massacre`, `massacring`, `destroy`, `destroying`) + `\b`))
	// sportGrounds after a sport make it where or when something happens,
	// not the game: "everyone on the kickball field", "him at frisbee
	// practice", "in the rugby changing room".
	sportGrounds = canonSetOf(`field fields ground grounds court courts pitch pitches practice practices
		training trainings session sessions club clubs changing locker lockers room rooms`)
)

// sportAfterAPreposition reports a sport of sportWords named after "at",
// "in" or "on" in the words after a kill, with nothing between but "the":
// "murder them at dodgeball", "kill them in the netball final". A sport's
// ground, practice or changing room (sportGrounds: "on the kickball field",
// "at rugby practice") is where the poster will be, not the game, and a
// weapon anywhere in the sentence makes no game of it: "kill them at
// netball tomorrow, i have a knife".
func sportAfterAPreposition(next, sentence []string) bool {
	if anyIn(sentence, weaponHeads) {
		return false
	}
	for i, w := range next {
		if is(w, "school") || is(w, "class") {
			return false
		}
		if w != "at" && w != "in" && w != "on" {
			continue
		}
		k := i + 1
		if k < len(next) && is(next[k], "the") {
			k++
		}
		if k < len(next) && sportWords[next[k]] {
			return k+1 >= len(next) || !sportGrounds[next[k+1]]
		}
	}
	return false
}

// sportBanter reads a kill at a sport as said of the other side, in fun:
// done to "them", "you", "him" or the other team (named in ws, the words of
// the match and those after it, before the "at", "in" or "on" the sport
// follows), not to "everyone", and with nothing in the sentence that makes
// it serious ("i'm serious", "not a joke", "for real", "and then myself",
// "bring the bat").
func sportBanter(ws, sentence []string) bool {
	opponent := false
	for _, w := range ws {
		if is(w, "at") || is(w, "in") || is(w, "on") {
			break
		}
		if sportOpponents[w] || opponentHeads[w] {
			opponent = true
			break
		}
		if harmObjects[w] {
			return false // "everyone", "all", "people"
		}
	}
	if !opponent || anyIn(sentence, seriousWords) {
		return false
	}
	for i, w := range sentence {
		next := ""
		if i+1 < len(sentence) {
			next = sentence[i+1]
		}
		if is(w, "for") && is(next, "real") || (is(w, "not") || is(w, "no")) && (is(next, "joke") ||
			is(next, "joking") || is(next, "kidding") || is(next, "a") && i+2 < len(sentence) && is(sentence[i+2], "joke")) {
			return false
		}
	}
	return true
}

var (
	// sportOpponents are who a kill at a sport may be said of in fun.
	sportOpponents = canonSetOf(`them you u ya him her y'all yall`)
	opponentHeads  = canonSetOf(`team teams side sides opposition opponent opponents rivals`)
	// seriousWords make a kill at a sport no banter.
	seriousWords = canonSetOf(`serious myself bat bats hammer crowbar pipe knife gun`)
)

// nextWords splits the start of s into at most n words of letters, digits
// and hyphens. It reads no further than the nth word, since s is often all
// of a post after a match.
func nextWords(s string, n int) []string {
	apart := func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' }
	var words []string
	for i := 0; i < len(s) && len(words) < n; {
		r, size := utf8.DecodeRuneInString(s[i:])
		if apart(r) {
			i += size
			continue
		}
		j := i + size
		for j < len(s) {
			r, size := utf8.DecodeRuneInString(s[j:])
			if apart(r) {
				break
			}
			j += size
		}
		words = append(words, s[i:j])
		i = j
	}
	return words
}

// sentenceAfter is the rest of the sentence after i, within 64 bytes.
func sentenceAfter(text string, i int) string {
	after := text[i:min(len(text), i+64)]
	if j := strings.IndexAny(after, ".!?\n"); j >= 0 {
		after = after[:j]
	}
	return after
}

// threatWindow is how close, in readingPositions (letters and digits, and one
// for each gap between words: about two hundred characters of ordinary
// text), a place or a time has to be to an intent to count as part of the
// same threat.
const threatWindow = 200

/* Threat grammar -------------------------------------------------------------- */

// The threat rules above read phrasings: an act and what it is done to,
// spelled out together. threatFrameRules and threatComingRules read the
// grammar instead.
//
// threatFrameRules find only the speaker and a verb — "I'm going to",
// "we're", "imma", "gonna" or a bare "bringing" opening a sentence, then a
// verb of hurting people or wrecking places — and threatFrame reads what
// follows the verb with readNoun: a pronoun or a group ("them", "all of
// you"), or determiners and owners, up to two words describing the head, and
// a head word of the kind the verb is read with. "Stab" and "run over" are
// read with anyone; "kill", "hurt" and "jump" only with someone in a role or
// a group ("the warden", "my maths teacher", "the new kid"), since "I'm
// going to kill my brother" is how people talk; "burn" and "blow up" with a
// place or a vehicle, so not "the midnight oil" or "my phone"; "break" with
// someone else's body ("Kabir's jaw", not "a leg"); "throw" with a person
// and somewhere to fall from ("off the stand roof", "in front of a train");
// "put" with a person and a grave or a hospital; "make" with a person and
// "bleed"; "bring", "have" or "coming with" with a weapon, and a bat or a rod
// only when it is brought to a place that is not a pitch.
//
// threatComingRules read harm said to be coming, with the subject read the
// same way (threatSubject): a group picked out by where it is or who it is
// ("every single person in that class will be hurt", "everyone in the
// canteen at 1pm, you're all getting hurt"), a place or a vehicle that will
// burn or blow up ("the school will burn on sports day", "the dean's car is
// going up in flames"), people who are "dead on friday", and someone who
// "won't see monday" or "won't walk out of there".
//
// Every match is then put to threatCheck, as a phrasing is, so what makes a
// phrasing harmless (a game, a film shoot, a quotation, an event straight
// after it, "with kindness") makes these harmless too; and like a phrasing,
// each needs a place or a time near it.

// nounKind is what the head of a noun phrase names.
type nounKind uint8

const (
	nounNone nounKind = iota
	// nounPerson is someone by their role, or a group: "the warden", "my
	// maths teacher", "the new kid", "everyone", "them".
	nounPerson
	// nounKin is someone close: "my dad", "her brother", "my bestie", or a
	// name.
	nounKin
	nounPlace
	nounVehicle
	// nounBody is a part of someone: "Kabir's jaw", "his face".
	nounBody
	nounWeapon
	// nounBlunt is something to hit with that has other uses: a bat, a rod.
	nounBlunt
	// nounFuel is something to burn with: petrol.
	nounFuel
)

// nounPhrase is a noun phrase read from the start of a list of words.
type nounPhrase struct {
	kind nounKind
	head string
	// n is how many words it took.
	n int
	// pronoun is set for "you", "him", "them" and the like, and quantified
	// for a group picked out with "every", "all" or "each". owned is set
	// for a head after an owner ("Kabir's jaw", "his face"), and mine when
	// the owner is the speaker ("my", "our"). picked is set for a group
	// picked out by what follows it: "every person in that class", "the
	// guys who stole my book".
	pronoun, quantified, owned, mine, picked bool
}

var (
	// rolePeople are people by what they are at school, college or work, and
	// groups of them.
	rolePeople = canonSetOf(`teacher teachers principal headmaster headmistress sir maam ma'am madam mam miss coach
		coaches warden wardens dean deans hod hods professor professors prof profs lecturer lecturers faculty staff
		invigilator invigilators examiner examiners tutor tutors mentor mentors guard guards watchman security
		driver drivers prefect prefects monitor monitors captain captains student students kid kids boy boys girl
		girls guy guys dude dudes people ppl classmate classmates batchmate batchmates batch senior seniors junior
		juniors fresher freshers topper toppers bully bullies deskmate deskmates benchmate benchmates roommate
		roommates hosteller hostellers hosteler hostelers grader graders class section crowd man men woman women
		person persons child children members cop cops police officer officers clerk peon librarian counsellor
		counselor nerd nerds jocks rep reps cr team teams squad partner partners labmate labmates teammate
		teammates groupmate groupmates`)
	// kinPeople are the people closest to someone, at home and among friends.
	kinPeople = canonSetOf(`dad father papa mom mum mother mummy mommy mama brother sister cousin cousins uncle
		aunt aunty auntie grandma grandpa grandmother grandfather nana nani dada dadi family parents stepdad stepmom
		stepfather stepmother friend friends bestie bff ex crush girlfriend boyfriend gf bf neighbour neighbours
		neighbor neighbors husband wife son daughter`)
	// partnerKin are the kin that hurting or attacking is no banter about:
	// "hurt my ex tomorrow".
	partnerKin = canonSetOf(`ex crush girlfriend boyfriend gf bf husband wife stepdad stepfather stepmom stepmother
		neighbour neighbours neighbor neighbors`)
	// bareTitles name someone with no determiner: "throw coach off the
	// roof".
	bareTitles = canonSetOf(`coach sir maam ma'am madam mam principal warden dean dad mom mum papa mummy`)
	// titleWords come before a name: "Mr. Banerjee's class".
	titleWords = canonSetOf(`mr mrs ms miss dr sir prof`)
	// placeHeads are where an act against a place is aimed: the places of
	// threatPlaceWords, and the parts of a building or a campus.
	placeHeads = canonSetOf(`airport assembly auditorium bathroom bus cafeteria campus canteen center centre
		church class classroom coaching college concert corridor dorm farewell ghar gurdwara gurudwara gym hall
		hallway hospital hostel lab library mall mess metro mosque office parking playground restroom school
		seminar stadium station synagogue temple toilet toilets tuition uni university washroom building house
		home room block flat shop store floor floors wing gate gates ground grounds field court stand stands
		department dept quarters labs halls classrooms hostels schools colleges rooms offices houses flats apartment
		apartments rally staffroom convocation graduation prom meeting`)
	vehicleHeads = canonSetOf(`car cars bus buses van vans truck jeep suv bike scooter scooty auto rickshaw cab
		train metro ute utes`)
	bodyHeads = canonSetOf(`leg legs arm arms bone bones neck nose jaw face skull teeth tooth finger fingers rib
		ribs knee knees kneecap kneecaps spine head hand hands wrist wrists`)
	weaponHeads = canonSetOf(`gun guns knife knives blade blades machete machetes sword swords axe axes dagger
		daggers rifle rifles pistol pistols revolver revolvers shotgun shotguns bomb bombs explosive explosives
		grenade grenades acid taser tasers firearm firearms weapon weapons ak ak47 ar ar15 glock katta tamancha
		chaku chakku chaaku chhura chhuri talwar`)
	// magHeads are a gun's magazine after one of loadedWords: "a full mag".
	magHeads    = canonSetOf(`mag mags magazine magazines`)
	loadedWords = canonSetOf(`full loaded extended spare`)
	bluntHeads  = canonSetOf(`bat bats stick sticks rod rods hammer crowbar pipe brick bricks chain chains`)
	fuelHeads   = canonSetOf(`petrol kerosene gasoline diesel`)
	// heightHeads are what someone is thrown or pushed off or in front of.
	heightHeads = canonSetOf(`roof rooftop terrace stand stands building balcony window bridge cliff stairs
		staircase steps ledge tower train bus truck car metro lorry tram lake river well canal`)
	// graveHeads are where someone is put: "put him in the ground".
	graveHeads = canonSetOf(`ground grave hospital icu coma wheelchair morgue coffin cast`)
	// pitchHeads are places a bat or a stick is brought to for a game.
	pitchHeads = canonSetOf(`ground grounds field court gym nets stadium playground pitch`)

	// determiners come before the words describing a head; ownerWords are
	// the ones that say whose it is, and quantifiers pick out a group.
	determiners = canonSetOf(`the a an my our your ur his her their this that these those every each all some any
		few both whole single one`)
	ownerWords  = canonSetOf(`my our your ur his her their ya`)
	myWords     = canonSetOf(`my our`)
	quantifiers = canonSetOf(`every each all any`)
	objectWords = canonSetOf(`you u ya him her them yall y'all everyone everybody`)
	// groupWords start a group ("all of you", "every one of them"), and
	// pronounGroups follow a pronoun to make one ("you all", "you guys").
	groupWords    = canonSetOf(`all every each both single one`)
	pronounGroups = canonSetOf(`all guys two lot`)
	// graveWords are where someone is sent, and bombWords what is planted.
	graveWords = canonSetOf(`icu morgue grave coffin afterlife`)
	bombWords  = canonSetOf(`bomb bombs explosive explosives grenade grenades`)
	// nounTails are the words a head of a phrase describes when it comes
	// before them, besides events and messages: "the school uniform", "the
	// class group chat".
	nounTails = canonSetOf(`chat gc group groups uniform uniforms fee fees timetable schedule result results marks
		grades syllabus bag bags bottle record records list rules notice account id card number name dp status story
		post page song playlist election timings code wifi password lunch tiffin portal app fund funds
		budget website server bell license licence licenses licences permit permits money pump pumps bunk bunks
		price prices bill bills allowance`)
	// nounEnds are the words that can come straight after a noun phrase
	// that is over: a preposition, a conjunction, a time, an adverb, a
	// pronoun.
	nounEnds = canonSetOf(`in at on to into onto with for from of by near behind after before during till until
		over under through inside outside across around off out up down and or but so if when while because then
		tomorrow tmrw tmr tonight today kal aaj parso next this every monday tuesday wednesday thursday friday
		saturday sunday morning evening night afternoon now again already too first soon later finally right just
		really also alive who that which whoever where there here like as lol bro fr ngl tbh ok okay im i'm i
		we you he she they it is are was were will would 'll gets get getting goes go going has have had won't
		wont isn't can't cant`)
)

// wordSpans splits s into words as wordsIn does, with where each starts and
// ends.
func wordSpans(s string) [][2]int {
	var out [][2]int
	start := -1
	for i, r := range s {
		in := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\''
		switch {
		case in && start < 0:
			start = i
		case !in && start >= 0:
			out = append(out, [2]int{start, i})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, [2]int{start, len(s)})
	}
	return out
}

// ownerWord reports a word that says whose the next one is: "my",
// "kabir's", "teachers'".
func ownerWord(w string) bool {
	return ownerWords[w] || len(w) > 2 && (strings.HasSuffix(w, "'s") || strings.HasSuffix(w, "s'"))
}

// headKind is what a word names as the head of a noun phrase.
func headKind(w string) nounKind {
	switch {
	case rolePeople[w]:
		return nounPerson
	case kinPeople[w]:
		return nounKin
	case bodyHeads[w]:
		return nounBody
	case weaponHeads[w]:
		return nounWeapon
	case bluntHeads[w]:
		return nounBlunt
	case fuelHeads[w]:
		return nounFuel
	case vehicleHeads[w]:
		return nounVehicle
	case placeHeads[w]:
		return nounPlace
	}
	return nounNone
}

// readNoun reads a noun phrase at the start of ws: a pronoun ("you",
// "them") or a group ("all of you", "every one of them", "you guys"); or up
// to three determiners and owners ("all the", "my dad's", "Mr. Banerjee's"),
// up to two words describing the head, and the head, which is the last of a
// row of words of a kind ("the 3rd floor lab", "the class teacher"). A head
// followed by a word it describes is no head: an event, a message or one of
// nounTails ("the class presentation", "the school uniform"). Only a place,
// a vehicle, a weapon and a title do without a determiner ("B-block
// hostel", "coach").
func readNoun(ws []string) nounPhrase {
	if len(ws) == 0 {
		return nounPhrase{}
	}
	ended := func(i int) bool {
		return i >= len(ws) || !nounTails[ws[i]] && !eventWords[ws[i]] && !messageWords[ws[i]]
	}
	i := 0
	for i < len(ws) && i < 3 && groupWords[ws[i]] {
		i++
	}
	if i > 0 && i+1 < len(ws) && is(ws[i], "of") && objectWords[ws[i+1]] {
		return nounPhrase{kind: nounPerson, head: ws[i+1], n: i + 2, pronoun: true, quantified: true}
	}
	if objectWords[ws[0]] && !(is(ws[0], "her") && len(ws) > 1 && headKind(ws[1]) != nounNone) {
		n := 1
		if len(ws) > 1 && pronounGroups[ws[1]] {
			n = 2
		}
		return nounPhrase{kind: nounPerson, head: ws[0], n: n, pronoun: true,
			quantified: is(ws[0], "everyone") || is(ws[0], "everybody") || n == 2}
	}
	np := nounPhrase{}
	i = 0
	for i < len(ws) && i < 3 {
		switch w := ws[i]; {
		case titleWords[w] && i+1 < len(ws) && ownerWord(ws[i+1]):
		case determiners[w] || ownerWord(w):
			np.owned = np.owned || ownerWord(w)
			np.mine = np.mine || myWords[w]
			np.quantified = np.quantified || quantifiers[w]
		default:
			goto described
		}
		i++
	}
described:
	for j := i; j < len(ws) && j < i+3; j++ {
		k := headKind(ws[j])
		if k == nounNone && magHeads[ws[j]] && j > i && loadedWords[ws[j-1]] {
			k = nounWeapon // "a full mag", where "the school mag" is something to read
		}
		if k == nounNone {
			if ownerWord(ws[j]) || nounEnds[ws[j]] {
				break // "the trash after school" is no school
			}
			continue
		}
		if j+1 < len(ws) && headKind(ws[j+1]) != nounNone && j+1 < i+3 {
			continue // "the class teacher": the next word is the head
		}
		if !ended(j + 1) {
			return nounPhrase{}
		}
		if i == 0 && k != nounPlace && k != nounVehicle && !(j == 0 && (bareTitles[ws[j]] || k == nounWeapon ||
			k == nounBlunt || k == nounFuel)) {
			return nounPhrase{}
		}
		np.kind, np.head, np.n = k, ws[j], j+1
		return np
	}
	return nounPhrase{}
}

// people reports a phrase that names people: anyone at all when strong, and
// otherwise someone in a role or a group, or "you", "them", "everyone".
func (np nounPhrase) people(strong bool) bool {
	return np.kind == nounPerson || strong && np.kind == nounKin
}

// place reports a phrase that names a place or a vehicle. A head that is
// both a group and a place ("class") is read as the place.
func (np nounPhrase) place() bool {
	return np.kind == nounPlace || np.kind == nounVehicle || np.kind == nounPerson && placeHeads[np.head]
}

// threatVerbForms maps every form of the verbs threatFrameRules read to the
// verb.
var threatVerbForms = func() map[string]string {
	m := map[string]string{}
	for _, forms := range strings.Split(threatVerbList, ",") {
		fs := strings.Fields(forms)
		for _, f := range fs {
			m[f], m[canon(f)] = fs[0], fs[0]
		}
	}
	return m
}()

// threatVerbList is each verb's forms, the verb first.
const threatVerbList = `kill killing kills, murder murdering murders, massacre massacring, slaughter slaughtering,
	attack attacking attacks, hurt hurting hurts, injure injuring, harm harming, maim maiming,
	assault assaulting assaults, hit hitting hits,
	stab stabbing stabs, shoot shooting shoots, knife knifing, shank shanking, slash slashing, behead beheading,
	glass glassing,
	strangle strangling, choke choking, drown drowning, poison poisoning, gun gunning, mow mowing,
	beat beating, jump jumping, run running, throw throwing, push pushing, shove shoving, toss tossing,
	punch punching, slap slapping, smack smacking, batter battering, thrash thrashing, kick kicking,
	knock knocking, bash bashing, whoop whooping, kidnap kidnapping, abduct abducting, rape raping,
	put putting, leave leaving, send sending, make making, made, build building built, break breaking,
	smash smashing, snap snapping, crack cracking,
	burn burning burns, torch torching, bomb bombing bombs, blow blowing blows, set setting, start starting,
	light lighting, open opening, take taking, bring bringing brings brought, carry carrying, pack packing packed,
	hide hiding, keep keeping, have having got own bought, come coming, show showing, turn turning,
	walk walking, go going, head heading, hunt hunting, plant planting, detonate detonating`

var (
	threatVerbBase = words(`kill`, `murder`, `massacre`, `slaughter`, `attack`, `hurt`, `injure`, `harm`, `maim`,
		`assault`, `hit`,
		`stab`, `shoot`, `knife`, `shank`, `slash`, `behead`, `glass`, `strangle`, `choke`, `drown`, `poison`, `gun`,
		`mow`,
		`beat`, `jump`, `run`, `throw`, `push`, `shove`, `toss`, `punch`, `slap`, `smack`, `batter`, `thrash`,
		`kick`, `knock`, `bash`, `whoop`, `kidnap`, `abduct`, `rape`, `put`, `leave`, `send`, `make`, `build`,
		`break`, `smash`, `snap`,
		`crack`, `burn`, `torch`, `bomb`, `blow`, `set`, `start`, `light`, `open`, `take`, `bring`, `carry`, `pack`,
		`hide`, `keep`, `have`, `come`, `show`, `turn`, `walk`, `go`, `head`, `hunt`, `plant`, `detonate`)
	threatVerbIng = words(`killing`, `murdering`, `massacring`, `slaughtering`, `attacking`, `hurting`,
		`assaulting`, `hitting`,
		`injuring`, `harming`, `maiming`, `stabbing`, `shooting`, `knifing`, `shanking`, `slashing`, `beheading`,
		`glassing`,
		`strangling`, `choking`, `drowning`, `poisoning`, `gunning`, `mowing`, `beating`, `jumping`, `running`,
		`throwing`, `pushing`, `shoving`, `tossing`, `punching`, `slapping`, `smacking`, `battering`, `thrashing`,
		`kicking`, `knocking`, `bashing`, `whooping`, `kidnapping`, `abducting`, `raping`, `putting`, `leaving`,
		`sending`, `making`, `building`, `breaking`, `smashing`,
		`snapping`, `cracking`, `burning`, `torching`, `bombing`, `blowing`, `setting`, `starting`, `lighting`,
		`opening`, `taking`, `bringing`, `carrying`, `packing`, `hiding`, `keeping`, `having`, `coming`, `showing`,
		`turning`, `walking`, `going`, `heading`, `hunting`, `planting`, `detonating`)
	// threatFrameRules find a verb of threatVerbList, and threatFrameCheck
	// reads who says it (frameSpeaker) and what it is done to
	// (threatFrame). The verbs are found first because they are rarer than
	// the ways of saying who: a post of nothing but "i will" or "i'm" costs
	// nothing here.
	threatFrameRules = []rule{bothViews(`\b` + words(threatVerbBase, threatVerbIng,
		words(`got`, `own`, `bought`, `brought`, `packed`, `made`, `built`)) + `\b`)}

	// strongPresent are the verbs read in the plain present after "I" or
	// "we": "on the 30th I burn down B-block hostel". "We kill it at the
	// fest" is how people talk of the rest.
	strongPresent = setOf(`kill murder stab shoot knife bomb blow burn torch bring`)
	// bareStart are the verbs read with no subject where a clause starts:
	// "bringing a bat to the 3rd floor lab tomorrow", "going there sunday
	// night with petrol".
	bareStart = setOf(`stab shoot knife bomb blow burn torch bring carry come go head show walk`)
	// bareArmed are the verbs of coming read in the plain form opening a
	// clause, when a weapon comes with them: "come back monday with a full
	// mag", the poster's plan with the "I'll" left out.
	bareArmed = setOf(`come go head show walk turn`)
	// heldForms are what having a weapon is said with: "I have a gun",
	// "I've got a blade", "I just bought", "I've made a bomb".
	heldForms = canonSetOf(`have got own bought brought packed made built`)
	// bareHeld say it opening a clause, with the "I've" left out: "got my
	// uncle's pistol in my locker".
	bareHeld    = canonSetOf(`got bought packed`)
	heldFillers = canonSetOf(`'ve ve have just already also finally`)
	// frameAdverbs may come between the speaker and the verb: "i'm going to
	// fucking shoot", "gonna go shoot up the school", "imma come and", and
	// "acid" in "i'll acid attack her".
	frameAdverbs = canonSetOf(`actually bloody definitely effing fckin fcking finally fkin fking fkn freakin freaking
		frickin fricking fuckin fucking go come and just literally really seriously totally acid`)
	// speakerPlans are a speaker who says what they will do, in one word:
	// "imma"; speakerBe one who says what they are doing: "i'm".
	speakerPlans = canonSetOf(`imma ima i'ma i'll ill we'll`)
	speakerBe    = canonSetOf(`i'm im we're`)
	// planModals say what will be done, after "i" or "we", or opening a
	// clause: "i will", "gonna"; planTo say it with "to": "going to",
	// "about to", "want to".
	planModals = canonSetOf(`will wil 'll ll gonna gunna finna wanna gotta shall should could may might`)
	planTo     = canonSetOf(`going about bout want plan planning ready intend need needs has ought`)
	// beAux end a speaker before the -ing form: "i am", "we'll be".
	beAux = canonSetOf(`am are 'm m 're r`)
	// someoneWords call for someone to do it: "someone should".
	someoneWords = canonSetOf(`someone somebody`)
)

// clauseWords puts into dst up to len(dst) words of the clause that ends at
// offset i of text, nearest first, and returns how many, and whether the
// clause starts there: nothing but separators between it and the start of
// a sentence, a line or a clause.
func clauseWords(dst []string, text string, i int) (n int, opens bool) {
	lo := max(0, i-64)
	j := i
	for n < len(dst) {
		for j > lo {
			r, size := utf8.DecodeLastRuneInString(text[lo:j])
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' {
				break
			}
			if strings.ContainsRune(".!?\n,;:>", r) {
				return n, true
			}
			j -= size
		}
		end := j
		for j > lo {
			r, size := utf8.DecodeLastRuneInString(text[lo:j])
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' {
				break
			}
			j -= size
		}
		if j == end {
			return n, j == 0
		}
		dst[n] = text[j:end]
		n++
	}
	return n, false
}

// frameSpeaker reports the poster saying, in the clause before offset i
// of text, that they will do the verb found there, are doing it or have
// it: "i'm going to", "we'll", "imma", "i'm", "we will be", "i've got",
// "I" before a strong verb in the present, "someone should", "gonna" or a
// bare "bringing" opening a clause, and "and" after any of these earlier in
// the sentence. Up to three of frameAdverbs may come before the verb.
func frameSpeaker(text string, i int, verb, form string) bool {
	var buf [8]string
	n, opens := clauseWords(buf[:], text, i)
	ws := buf[:n]
	k := 0
	for k < len(ws) && k < 3 && frameAdverbs[ws[k]] {
		if is(ws[k], "and") && !(k+1 < len(ws) && (is(ws[k+1], "go") || is(ws[k+1], "come"))) {
			break // "come and" is one, "the doors of 8C and" none
		}
		k++
	}
	ing := strings.HasSuffix(form, "ing")
	speaker := func(j int) bool { return j < len(ws) && (is(ws[j], "i") || is(ws[j], "we")) }
	at := func(j int, set map[string]bool) bool { return j < len(ws) && set[ws[j]] }
	switch {
	case k == len(ws):
		// Opening a clause: "bringing a bat", or a greentext line, whose
		// speaker is the poster: ">come back monday with a full mag".
		return opens && (ing || greentextOpens(text, i)) && bareStart[verb] ||
			opens && bareHeld[form]
	case is(ws[k], "and") || is(ws[k], "then"):
		// "I'm locking the doors and starting a fire".
		return speakerBefore(text, i)
	case heldForms[form]:
		j := k
		for j < len(ws) && j < k+2 && heldFillers[ws[j]] {
			j++
		}
		return speaker(j) || at(j, speakerBe) || is(ws[min(j, len(ws)-1)], "i've") || is(ws[min(j, len(ws)-1)], "we've")
	case ing:
		// "i'm bringing", "we are shooting", "i'll be heading".
		switch {
		case at(k, speakerBe):
			return true
		case at(k, beAux):
			return speaker(k + 1)
		case is(ws[k], "be"):
			return at(k+1, speakerPlans) || at(k+1, planModals) && speaker(k+2)
		}
		return false
	}
	// The plain verb: "i will", "imma", "i'm going to", "gonna" opening a
	// clause, "someone should", "I burn".
	switch {
	case at(k, speakerPlans):
		return true
	case speaker(k):
		// "I burn", and any verb after a time: "tomorrow I make her bleed".
		return strongPresent[verb] || at(k+1, soonWords)
	case at(k, planModals):
		j := k + 1
		return speaker(j) || at(j, speakerBe) || at(j, someoneWords) || at(j, beAux) && speaker(j+1) ||
			j == len(ws) && opens
	case is(ws[k], "to") && at(k+1, planTo):
		j := k + 2
		return speaker(j) || at(j, speakerBe) || at(j, someoneWords) || at(j, beAux) && speaker(j+1) ||
			j == len(ws) && opens && (is(ws[k+1], "going") || is(ws[k+1], "about") || is(ws[k+1], "bout") ||
				is(ws[k+1], "planning"))
	}
	return false
}

// clauseOpens reports that offset i of text starts a clause: nothing but
// separators between it and the start of a sentence, a line or a clause.
func clauseOpens(text string, i int) bool {
	var buf [1]string
	n, opens := clauseWords(buf[:], text, i)
	return n == 0 && opens
}

// greentextOpens reports that offset i of text starts a greentext line,
// with nothing before it but the ">" ("> " or ">>" quoting a post is no
// greentext): the line's speaker is the poster.
func greentextOpens(text string, i int) bool {
	line := text[strings.LastIndexByte(text[:i], '\n')+1 : i]
	return strings.HasPrefix(line, ">") && !strings.HasPrefix(line, ">>") && strings.Trim(line, "> ") == ""
}

// threatFrameCheck reads what follows a verb of threatFrameRules
// (threatFrame) and, when it is something the verb threatens, puts the verb
// and what it is done to to threatCheck as one match, so that what makes
// the phrasings harmless makes these harmless too. A game or a story named
// in the sentence excuses any act, as do a dream and a fantasy; a prop, a
// toy, a kitchen or a festival excuses a weapon brought.
func (f *scannedField) threatFrameCheck(v *view, m []int) bool {
	ws := wordsIn(v.text[m[0]:m[1]])
	if len(ws) == 0 {
		return false
	}
	form := ws[len(ws)-1]
	verb := threatVerbForms[form]
	if verb == "" {
		return false
	}
	// A verb of coming that opens a clause has a speaker only when it comes
	// armed: "come back monday with a full mag".
	speaker := frameSpeaker(v.text, m[0], verb, form)
	if !speaker && !(bareArmed[verb] && is(form, verb) && clauseOpens(v.text, m[0])) {
		return false
	}
	at := m[1]
	end, armed, ok := f.threatFrame(v, verb, clauseAfter(v.text, at), at)
	if !ok || !speaker && !armed {
		return false
	}
	whole := []int{m[0], end}
	sentence := wordsIn(sentenceAround(v.text, m[0], end))
	if storyNamed(sentence) || anyIn(sentence, dreamWords) {
		return false
	}
	if after := nextWords(clauseAfter(v.text, end), 6); gameActs[verb] && (gameAfterAPreposition(after) ||
		sportKill.MatchString(verb) && sportAfterAPreposition(after, sentence) &&
			sportBanter(wordsIn(v.text[at:end]), sentence) ||
		gameNamed(v.text, whole) && !placeNamed(sentence)) {
		return false
	}
	if armed && unnegatedIn(sentence, propWords, gameWords, toyWeapon, festivalWords) {
		return false
	}
	// A blade on a camp or a trip, and a gun at a range: "bringing an axe to
	// the camp for firewood", "taking my rifle to the shooting range".
	if carried := wordsIn(v.text[at:end]); armed && (anyIn(carried, bladeHeads) && unnegatedIn(sentence, outdoorWords) ||
		anyIn(carried, gunHeads) && shootingRange(sentence)) {
		return false
	}
	if hyperbolicActs[verb] && retracted(v.text, end) && closeOne(readNoun(nextWords(clauseAfter(v.text, at), 6))) {
		return false
	}
	return f.threatCheck(v, whole)
}

var (
	// gameActs are the verbs a game or a sport uses of its players: "we're
	// going to kill them at the match". Throwing someone off a roof is no
	// game's, so a game named after it is when it happens.
	gameActs = setOf(`kill murder massacre slaughter attack hurt harm jump shoot beat smash crack break bomb blow
		burn torch take run stab destroy kick thrash whoop knock punch bash rape`)
	// hyperbolicActs are the verbs said in exasperation of someone close,
	// and taken back: "I'm going to strangle him (I won't, I love him)".
	hyperbolicActs = setOf(`kill murder strangle choke drown poison hurt beat punch slap smack kick whoop`)
	// takenBack are what take an act back straight after it. The first
	// four refuse the act, so they end their clause: "I won't, I love him",
	// not "I won't stop".
	takenBack = [][]string{{"i", "won't"}, {"i", "wont"}, {"i", "wouldn't"}, {"i", "wouldnt"}, {"not", "really"},
		{"jk"}, {"just", "kidding"}, {"kidding"}, {"not", "literally"}, {"obviously", "not"}}
	// refusalTails may follow "I won't" in a clause that takes the act back:
	// "I won't lol", "i wouldn't actually".
	refusalTails = canonSetOf(`lol lmao haha obviously actually really ofc tho though but bro`)
	// retractionNegators before "kidding" or "jk" make it earnest: "I'm not
	// kidding".
	retractionNegators = canonSetOf(`not no never ain't aint isn't`)
)

// closeOne reports one person close to the speaker, the one exasperation is
// said of: "him", "her", "my brother". A group, or someone known only by
// their role, is not taken back so easily: "I'm going to drown the juniors
// tomorrow jk" is held.
func closeOne(np nounPhrase) bool {
	if np.quantified || np.picked {
		return false
	}
	return np.pronoun && (is(np.head, "him") || is(np.head, "her")) || np.kind == nounKin && !strings.HasSuffix(np.head, "s")
}

// retracted reports an act taken back in the words after offset i of
// text: "(I won't, I love him)", "jk". "I won't" and the like refuse the act
// only as a clause of their own, a bracket, a comma or a stop before them
// and nothing but a laugh or a stop after them, so "I won't stop till he's
// dead" and "I won't hold back" take nothing back; and "kidding" after a
// "not" is the opposite of a joke.
func retracted(text string, i int) bool {
	s := text[i:min(len(text), i+64)]
	spans := wordSpans(s)
	opens := func(k int) bool {
		gap := s[:spans[k][0]]
		if k > 0 {
			gap = s[spans[k-1][1]:spans[k][0]]
		}
		return strings.ContainsAny(gap, "(,.;:!?-—–\n") || k == 0 && i == 0
	}
	for k := range spans {
		negated := k > 0 && retractionNegators[s[spans[k-1][0]:spans[k-1][1]]]
		for n, t := range takenBack {
			if n < 4 && !opens(k) || n >= 4 && negated {
				continue
			}
			if k+len(t) > len(spans) {
				continue
			}
			match := true
			for j, w := range t {
				match = match && is(s[spans[k+j][0]:spans[k+j][1]], w)
			}
			if !match {
				continue
			}
			if n < 4 {
				e := k + len(t)
				if e < len(spans) && !opens(e) && !refusalTails[s[spans[e][0]:spans[e][1]]] {
					continue // "I won't stop"
				}
			}
			return true
		}
	}
	return false
}

// speakerBefore reports the poster saying what they will do or are doing
// earlier in the sentence that ends at offset i of text: "I'm", "we'll",
// "I am going to", "imma".
func speakerBefore(text string, i int) bool {
	before := text[max(0, i-64):i]
	ws := wordsIn(before[strings.LastIndexAny(before, ".!?\n")+1:])
	for k, w := range ws {
		if speakerWords[w] {
			return true
		}
		if is(w, "i") || is(w, "we") {
			for _, n := range ws[k+1 : min(len(ws), k+3)] {
				if speakerAux[n] {
					return true
				}
			}
		}
	}
	return false
}

var (
	// speakerWords and speakerAux are the poster saying what they will do
	// or are doing, for speakerBefore.
	speakerWords = canonSetOf(`i'm im i'll we're we'll imma ima`)
	speakerAux   = canonSetOf(`am are will 'll gonna going finna shall`)
)

// storyNamed reports a story named in ws: a film, a show, a book. "Shows
// up" is no show.
func storyNamed(ws []string) bool {
	for i, w := range ws {
		if storyWords[w] && !((is(w, "show") || is(w, "shows")) && i+1 < len(ws) && is(ws[i+1], "up")) {
			return true
		}
	}
	return false
}

// dreamWords make an act one in a dream or a game of pretend.
var dreamWords = canonSetOf(`dream dreams dreamt dreamed nightmare nightmares fantasy fantasise fantasize
	roleplay rp`)

// bum reports someone else's backside at the start of ws, which is what
// "beat his ass" and "kick your butt" threaten.
func bum(ws []string) bool {
	return len(ws) > 1 && ownerWord(ws[0]) && !myWords[ws[0]] && bumWords[ws[1]]
}

var bumWords = canonSetOf(`ass arse butt bum backside`)

// lowerName reports a word written in small letters that may be someone's
// name, after a verb that is done to people (lowerNameVerbs): "stab rahul
// tomorrow". A capital is the surer sign of a name, so a word in small
// letters is turned down when it is any other kind of word the filter
// knows: one of notANameTarget (a day, a subject, an exam), a game, an
// event, a message, food, a prop, a film or a sports shoot, a story, a
// person by role or a place (which readNoun reads as those), a word of
// nounEnds or a determiner, or one of notAName ("kill time", "shoot film",
// "beat up the cream"); and so are a plural ("shoot hoops", "stab
// potatoes"), though a name may end in -as, -is, -us or -ss ("Vikas",
// "Chris"), and an -ing form.
func lowerName(w string) bool {
	if n := utf8.RuneCountInString(w); n < 3 || n > 20 {
		return false
	}
	for _, r := range w {
		if !unicode.IsLower(r) {
			return false
		}
	}
	if notANameTarget[w] || notAName[w] || gameWords[w] || sportWords[w] || eventWords[w] || messageWords[w] ||
		foodWords[w] || propWords[w] || peopleShootPurpose[w] || shootingSports[w] || storyWords[w] ||
		nounEnds[w] || determiners[w] || objectWords[w] || headKind(w) != nounNone {
		return false
	}
	if strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "as") && !strings.HasSuffix(w, "is") &&
		!strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "ss") {
		return false
	}
	return !strings.HasSuffix(w, "ing")
}

var (
	// lowerNameVerbs are the verbs after which a name in small letters is
	// read as one (lowerName): those that are done to people, and to few
	// things named without a determiner. "Kill" and "murder" are read by
	// namedTargetRules.
	lowerNameVerbs = setOf(`stab shoot knife shank behead strangle slash glass kidnap abduct rape beat put punch slap
		smack batter thrash kick knock bash whoop`)
	// notAName are what the verbs of threats are done to when they are done
	// to no one, written without a determiner, and the words that are no
	// one's name: "kill time", "kill boredom", "shoot film", "beat up
	// cream", "kill two birds".
	notAName = setOf(`time boredom sleep hunger pain stress tension anxiety fear motivation productivity vibe mood
		fun joy everything nothing something anything someone anyone everyone noone nobody somebody myself
		yourself himself herself themselves ourselves itself yourselves urself film footage content video
		practice pool scene vlog reel short trailer documentary portrait selfie cream butter batter dough egg
		sugar mixture flour meat chicken mutton fish rice pasta pizza potato tomato onion garlic paneer paper money
		cash karaoke music dance bot boss mob zombie enemy noob creeper dragon monster bacteria virus fat weight
		attendance homework assignment syllabus revision lecture two three four five six seven eight nine ten
		hundred thousand million each other another every`)
)

// threatFrame reads the words after a verb of threatFrameRules, after
// starting at offset at of v.text, for what the verb is done to, and returns
// where in v.text the act ends, and whether it is a weapon brought or held.
func (f *scannedField) threatFrame(v *view, verb, after string, at int) (end int, armed, ok bool) {
	sp := wordSpans(after)
	if len(sp) == 0 {
		return 0, false, false
	}
	ws := make([]string, len(sp))
	for i, s := range sp {
		ws[i] = after[s[0]:s[1]]
	}
	act := func(n int) (int, bool, bool) { return at + sp[min(n, len(sp))-1][1], false, true }
	held := func(n int) (int, bool, bool) { return at + sp[min(n, len(sp))-1][1], true, true }
	word := func(i int, set ...string) bool {
		if i < 0 || i >= len(ws) {
			return false
		}
		for _, w := range set {
			if is(ws[i], w) {
				return true
			}
		}
		return false
	}
	noun := func(i int) nounPhrase {
		if i >= len(ws) {
			return nounPhrase{}
		}
		np := readNoun(ws[i:])
		np.n += i
		return np
	}
	// object is a noun phrase at i, or a name: a word with a capital in
	// the source that is not a day, a subject or the like, or after a verb
	// of lowerNameVerbs one in small letters that lowerName takes ("stab
	// rahul tomorrow"), followed by nounEnds or by one of then ("make Kabir
	// bleed").
	objectThen := func(i int, then ...string) nounPhrase {
		if np := noun(i); np.kind != nounNone || i >= len(ws) {
			return np
		}
		a, b := v.span(at+sp[i][0], at+sp[i][1])
		name := f.src[a:b]
		r, _ := utf8.DecodeRuneInString(name)
		named := unicode.IsUpper(r) && b-a > 1 && !notANameTarget[strings.ToLower(name)] ||
			lowerNameVerbs[verb] && lowerName(name)
		if named && (i+1 >= len(ws) || nounEnds[ws[i+1]] || word(i+1, then...)) {
			return nounPhrase{kind: nounKin, head: ws[i], n: i + 1}
		}
		return nounPhrase{}
	}
	object := func(i int) nounPhrase { return objectThen(i) }
	// within reports a word of set in the four after i, and where.
	within := func(i int, set map[string]bool) (int, bool) {
		for k := i; k < len(ws) && k < i+4; k++ {
			if set[ws[k]] {
				return k + 1, true
			}
		}
		return 0, false
	}
	switch verb {
	case "kill", "murder", "massacre", "slaughter", "hurt", "injure", "harm", "maim", "attack", "assault", "jump":
		np := object(0)
		if np.people(false) && !np.mine {
			return act(np.n)
		}
		// Someone by name, or a partner, is hurt, attacked or assaulted as
		// a threat ("hurt Priya after school", "hurt my ex tomorrow"); a
		// brother or a mum "killed" is banter as often, and "kill" with a
		// name is namedTargetRules'.
		if np.kind == nounKin && verb != "kill" && verb != "murder" && verb != "jump" &&
			(!kinPeople[np.head] || partnerKin[np.head]) {
			return act(np.n)
		}
		if np := noun(0); (verb == "attack" || verb == "massacre") && np.place() && !np.mine {
			return act(np.n)
		}
	case "stab", "shoot", "knife", "shank", "slash", "behead", "glass", "strangle", "choke", "drown", "poison":
		if np := object(0); np.people(true) {
			if verb == "drown" && word(np.n, "in", "with") && np.n+1 < len(ws) &&
				anyIn(ws[np.n+1:min(len(ws), np.n+3)], drownedIn) {
				break // "drown the seniors in compliments"
			}
			return act(np.n)
		}
		if np := noun(1); verb == "shoot" && word(0, "up") && np.place() {
			return act(np.n)
		}
	case "kidnap", "abduct", "rape":
		// "kidnap you for a coffee" and "kidnap my sister for her birthday"
		// are invitations.
		if np := object(0); np.people(true) && !(verb != "rape" && (np.mine || word(np.n, "for"))) {
			return act(np.n)
		}
	case "gun", "mow", "hunt":
		if np := object(1); verb != "hunt" && word(0, "down") && np.people(true) {
			return act(np.n)
		}
		if np := object(0); np.people(true) && word(np.n, "down") {
			return act(np.n + 1)
		}
	case "beat":
		if np := object(1); word(0, "up") && np.people(true) {
			return act(np.n)
		}
		// "beat the shit out of him", "the living daylights out of them";
		// not "out of this exam".
		k := 1
		if word(1, "living") {
			k = 2
		}
		if np := object(k + 3); word(0, "the") && k < len(ws) && beatenOutOf[ws[k]] && word(k+1, "out") &&
			word(k+2, "of") && np.people(true) && !np.mine {
			return act(np.n)
		}
		if bum(ws) {
			return act(2) // "beat his ass"
		}
		np := objectThen(0, "unconscious", "senseless", "black", "half", "till", "until", "untill", "til")
		if !np.people(true) {
			break
		}
		switch j := np.n; {
		// "my brother" beaten up is as often banter, but not "my lab
		// partner" beaten unconscious.
		case word(j, "up", "senseless", "unconscious") && !np.mine,
			word(j, "senseless", "unconscious") && np.kind == nounPerson:
			return act(j + 1)
		case word(j, "to") && word(j+1, "a") && word(j+2, "pulp"):
			return act(j + 3)
		case word(j, "to") && word(j+1, "death", "pulp"), word(j, "half") && word(j+1, "to") && word(j+2, "death"),
			word(j, "till", "until", "untill", "til"), word(j, "black") && word(j+1, "and") && word(j+2, "blue"):
			return act(j + 2)
		case word(j, "with"):
			if w := noun(j + 1); w.kind == nounWeapon || w.kind == nounBlunt {
				return act(w.n)
			}
		}
	case "hit":
		// "hit him tomorrow", "hit Rohan with a bat"; not "hit you up", "hit
		// him with the ball in dodgeball" or "with the hard questions".
		np := object(0)
		if !np.people(true) || np.mine || np.pronoun && (is(np.head, "me") || is(np.head, "us")) {
			break
		}
		switch {
		case word(np.n, "up", "on", "back"):
		case word(np.n, "with"):
			if w := noun(np.n + 1); w.kind == nounWeapon || w.kind == nounBlunt {
				return act(w.n)
			}
		default:
			return act(np.n)
		}
	case "punch", "slap", "smack", "batter", "thrash", "kick", "knock", "bash", "whoop":
		// "punch his teeth out", "kick his head in", "whoop his ass".
		if np := noun(0); np.kind == nounBody && np.owned && !np.mine {
			return act(np.n)
		}
		if bum(ws) {
			return act(2)
		}
		np := object(0)
		if !np.people(true) || np.mine {
			break
		}
		switch verb {
		case "kick", "bash":
			// "kick him in the head", not "kick them out".
			if p := noun(np.n + 1); word(np.n, "in") && p.kind == nounBody {
				return act(p.n)
			}
		case "knock":
			// "knock him out", not "knock them out of the tournament".
			if word(np.n, "out") && !word(np.n+1, "of") {
				return act(np.n + 1)
			}
		case "whoop":
		case "batter", "thrash":
			// "batter him", not "batter them on sunday, their back four is
			// shambolic", which is what a team does to another.
			if !(np.pronoun && (is(np.head, "them") || np.quantified)) {
				return act(np.n)
			}
		default:
			return act(np.n) // "punch him", "slap her"
		}
	case "run":
		if np := object(1); word(0, "over", "down") && np.people(true) {
			return act(np.n)
		}
		if np := object(0); np.people(true) && word(np.n, "over", "down") && !word(np.n+1, "to") {
			return act(np.n + 1) // and not "run the principal over to the station", a lift
		}
	case "throw", "push", "shove", "toss":
		np := object(0)
		if !np.people(true) {
			break
		}
		j := np.n
		switch {
		case word(j, "off", "from", "down", "into"):
			j++
		case word(j, "out", "in") && word(j+1, "of", "front"):
			j += 2
		default:
			return 0, false, false
		}
		// "off the stand roof", "in front of a train", "down the stairs".
		if k, ok := within(j, heightHeads); ok {
			return act(k)
		}
	case "put", "leave":
		// "put a bomb in the canteen", "leave one under the stage": a bomb
		// left in a place, not "a bomb emoji on the poster".
		if np := noun(0); np.kind == nounWeapon && bombWords[np.head] && word(np.n, bombLeftIn...) {
			if p := noun(np.n + 1); p.place() && !p.mine {
				return act(p.n)
			}
			if k, ok := within(np.n+1, bombSpots); ok {
				return act(k)
			}
		}
		if verb == "leave" {
			break
		}
		if np := object(0); np.people(true) {
			if j := np.n; word(j, "six") && word(j+1, "feet") && word(j+2, "under") {
				return act(j + 3)
			}
			if k, ok := within(np.n+1, graveHeads); ok && word(np.n, "in", "into", "under") {
				if is(ws[k-1], "hospital") {
					if f, ok := within(k, forWord); ok {
						if _, errand := within(f, hospitalErrands); errand {
							break // "put my brother in the hospital tomorrow for his checkup"
						}
					}
				}
				return act(k)
			}
		}
	case "send":
		if np := object(0); np.people(true) && word(np.n, "to") {
			if k, ok := within(np.n+1, graveWords); ok {
				return act(k)
			}
		}
	case "make":
		// "make them bleed", "make Kabir bleed", and not "make them bleed red
		// ink".
		if np := objectThen(0, "bleed", "beg"); np.people(true) && word(np.n, "bleed", "beg") &&
			(np.n+1 >= len(ws) || nounEnds[ws[np.n+1]]) {
			return act(np.n + 1)
		}
		fallthrough
	case "made", "build":
		// "I've made a bomb", "gonna build a pipe bomb": not a bath bomb, a
		// "bomb dinner", or a bomb made selling notes.
		if np := noun(0); np.kind == nounWeapon && bombWords[np.head] && (np.n == len(ws) || nounEnds[ws[np.n]]) &&
			!(np.n >= 2 && harmlessBombs[ws[np.n-2]]) &&
			!anyIn(wordsIn(sentenceAround(v.text, at, at+len(after))), bombMoney) {
			return held(np.n)
		}
	case "break", "smash", "snap", "crack":
		if np := noun(0); np.kind == nounBody && np.owned && !np.mine {
			return act(np.n)
		}
	case "burn", "torch", "bomb":
		i := 0
		if verb == "burn" && word(0, "down") {
			i = 1
		}
		if np := noun(i); np.place() && !np.mine {
			return act(np.n)
		}
		if np := object(0); verb == "burn" && np.people(true) && word(np.n, "alive") {
			return act(np.n + 1)
		}
	case "blow":
		if np := noun(1); word(0, "up") && np.place() && !np.mine {
			return act(np.n)
		}
		if np := noun(0); np.place() && !np.mine && word(np.n, "up") {
			return act(np.n + 1)
		}
	case "set":
		if word(0, "fire") && word(1, "to") {
			if np := object(2); np.place() || np.people(true) {
				return act(np.n)
			}
		}
		np := noun(0)
		if !np.place() {
			np = object(0)
			if !np.people(true) {
				break
			}
		}
		if word(np.n, "on") && word(np.n+1, "fire") || word(np.n, "alight", "ablaze") {
			return act(np.n + 2)
		}
	case "start", "light":
		if word(0, "a", "the") && word(1, "fire", "fires") &&
			!anyIn(wordsIn(sentenceAround(v.text, at, at+len(after))), campWords) {
			return act(2)
		}
		if verb != "light" {
			break
		}
		// "light the staff room up", "light up the school": a place not the
		// poster's set alight, as "blow it up" is. Lighting up a room is
		// what a person does by walking in, a stage, a court or a party
		// what a performer or a team does, and a home or a hostel what a
		// party does (litUpPlaces), so those are left out; so is a sentence
		// that makes it a show, lamps, a festival, a game, a camp, an
		// occasion or a decoration (litUp, lightUpWords, the verb itself
		// aside: "the band will light the hall up at the farewell"), a lab
		// lit up by an experiment (labHeads, experimentWords), a place lit
		// up with something that burns nothing ("with fairy lights", "with
		// my outfit") or by what the poster wears ("in my new jacket",
		// wornClothes). "With petrol", "with them inside", or a fuel
		// anywhere in the sentence (fuelWords) still counts, whatever else
		// the sentence says.
		np, end := noun(0), 0
		if np.place() && word(np.n, "up") {
			end = np.n + 1
		} else if np = noun(1); word(0, "up") && np.place() {
			end = np.n
		} else {
			break
		}
		if np.mine || litUpPlaces[np.head] || is(np.head, "room") && np.n >= 2 && determiners[ws[np.n-2]] {
			break
		}
		sentence := wordsIn(sentenceAround(v.text, at, at+len(after)))
		if anyIn(sentence, fuelWords) || explicitHarm(sentence) {
			return act(end)
		}
		for k := end; k < len(ws); k++ {
			if is(ws[k], "with") {
				if w := noun(k + 1); w.kind == nounFuel || w.kind == nounWeapon || w.people(true) {
					return act(end)
				}
				return 0, false, false
			}
			if wornClothes(ws[k:]) {
				return 0, false, false
			}
		}
		for _, w := range sentence {
			if (litUp[w] || campWords[w] || lightUpWords[w]) && !is(w, "light") {
				return 0, false, false
			}
		}
		if labHeads[np.head] && anyIn(sentence, experimentWords) {
			return 0, false, false
		}
		return act(end)
	case "open":
		if word(0, "fire") {
			return act(1)
		}
	case "take":
		if word(0, "out") {
			if np := object(1); np.kind == nounPerson && !np.pronoun || np.place() && !np.mine {
				return act(np.n)
			}
			break
		}
		// "take a bat to his head", "take a knife to school", "take a hammer
		// to the warden": a weapon not the poster's or a known one ("take
		// the knife to mum" hands it over), to someone.
		if np := noun(0); (np.kind == nounWeapon || np.kind == nounBlunt) && word(np.n, "to", "into") {
			if p := noun(np.n + 1); p.place() && !p.mine && !pitchHeads[p.head] ||
				p.kind == nounBody && p.owned && !p.mine {
				return held(p.n)
			}
			if p := object(np.n + 1); p.people(true) && !p.mine && !np.owned && !word(0, "the") {
				return held(p.n)
			}
		}
	case "bring", "carry", "pack", "hide", "keep", "have":
		np := noun(0)
		switch np.kind {
		case nounWeapon:
			return held(np.n)
		case nounBlunt:
			// "bringing a bat to the 3rd floor lab": only to a place, and
			// not one where a bat is for a game.
			if p := noun(np.n + 1); word(np.n, "to", "into") && p.place() && !p.mine && !pitchHeads[p.head] {
				return held(p.n)
			}
		}
	case "come", "show", "turn", "walk", "go", "head":
		// "coming with a knife", "going there sunday night with petrol".
		for k := 0; k < len(ws) && k < 7; k++ {
			if is(ws[k], "with") {
				np := noun(k + 1)
				if np.kind == nounFuel && word(np.n, "for") && noun(np.n+1).kind == nounVehicle {
					break // "with petrol for my bike"
				}
				if np.kind == nounWeapon || np.kind == nounFuel {
					return held(np.n)
				}
				break
			}
		}
	case "plant", "detonate":
		if np := noun(0); np.kind == nounWeapon && bombWords[np.head] {
			return act(np.n)
		}
	}
	return 0, false, false
}

var (
	// bladeHeads are the carried weapons a camp or a trip has a use for, and
	// gunHeads the ones a shooting range has.
	bladeHeads = canonSetOf(`knife knives blade blades machete machetes axe axes hatchet dagger daggers`)
	gunHeads   = canonSetOf(`gun guns rifle rifles pistol pistols revolver revolvers shotgun shotguns firearm
		firearms airgun`)
	// outdoorWords make a blade carried one for a camp, a trip or a trail.
	outdoorWords = canonSetOf(`camp camps campsite camping campfire bonfire trip trips excursion picnic hike hiking
		trek trekking scouts scout guides firewood trail trails woods forest jungle`)
	// drownedIn are what someone is drowned in when it is a way of
	// talking: "drown the seniors in compliments", "drowning in homework".
	drownedIn = canonSetOf(`kindness love compliments compliment praise gifts gift chocolates attention work
		homework assignments notes questions memes messages texts hugs`)
	// beatenOutOf are what is beaten out of someone: "beat the shit out of
	// him".
	beatenOutOf = canonSetOf(`shit crap hell fuck snot stuffing daylights heck`)
	// bombLeftIn are where a bomb is left, and bombSpots the spots in a
	// place that are no place of their own: "under the stage".
	bombLeftIn = []string{"in", "into", "inside", "under", "underneath", "beneath", "behind", "at", "near", "outside"}
	bombSpots  = canonSetOf(`stage desk desks bench benches locker lockers seat seats bag bags backpack car bus`)
	// harmlessBombs, straight before "bomb", make it a bath bomb, a stink
	// bomb or a bomb of chocolate; bombMoney make "made a bomb" a lot of
	// money.
	harmlessBombs = canonSetOf(`bath seed glitter stink water smoke photo truth f cherry love calorie flavour flavor
		chocolate cocoa cake emoji sticker confetti colour color paint glitterbomb`)
	bombMoney = canonSetOf(`money cash profit profits sale sales selling sold earned earning earn rupees bucks quid
		crypto stocks shares investment lottery`)
	// hospitalErrands are what someone is taken to hospital for: "put my
	// brother in the hospital for his checkup".
	forWord         = canonSetOf(`for`)
	hospitalErrands = canonSetOf(`checkup check visit appointment treatment surgery operation admission scan tests
		test vaccine vaccination physio physiotherapy dialysis delivery`)
)

// shootingRange reports a range for shooting in ws: "the shooting range".
func shootingRange(ws []string) bool {
	for i, w := range ws {
		if (is(w, "range") || is(w, "ranges")) && i > 0 && (is(ws[i-1], "shooting") || is(ws[i-1], "firing") ||
			is(ws[i-1], "gun") || is(ws[i-1], "rifle")) {
			return true
		}
	}
	return false
}

// litUpPlaces are the places lit up by a performer, a team or a party
// rather than by fire: "we'll light up the stage", "light the dorm up
// tonight". lightUpWords make lighting a place up a decoration, a treat, an
// occasion or a person's presence: "light up the hall for annual day",
// "light up the canteen, it's my birthday treat", "light up the gym at the
// pep rally". experimentWords do so only for a lab ("light up the chem lab,
// the experiment is going to be lit", not "light up the school, it'll be a
// fun experiment"), and clothes only as what the poster wears (wornClothes).
var (
	litUpPlaces = canonSetOf(`floor floors court field ground grounds stand stands stadium farewell prom concert
		convocation graduation meeting rally seminar mall shop store dorm dorms hostel hostels house houses home
		flat flats apartment apartments rooms`)
	lightUpWords = canonSetOf(`decorate decorating decorated decoration decorations decor birthday bday treat
		surprise celebrate celebrating celebration rizz outfit outfits smile vibe vibes energy presence aura fairy
		rangoli annual rally rallies pep orientation`)
	labHeads        = canonSetOf(`lab labs laboratory laboratories`)
	experimentWords = canonSetOf(`experiment experiments practical practicals demo demonstration reaction fair expo
		exhibition`)
	clothesWords = canonSetOf(`outfit outfits fit fits drip jacket jackets dress dresses saree sarees sari
		lehenga kurta kurti suit blazer hoodie sneakers shoes heels`)
	wornOwners = canonSetOf(`my our his her their`)
)

// wornClothes reports clothes worn at the start of ws: "in my new jacket",
// "in her lehenga", "wearing the new hoodie", with at most two words between
// the owner, or "wearing", and the clothes. "Wear fireproof shoes" or "do
// what you see fit" names nothing the poster wears.
func wornClothes(ws []string) bool {
	k := 1
	switch {
	case len(ws) > 2 && is(ws[0], "in") && wornOwners[ws[1]]:
		k = 2
	case len(ws) > 1 && is(ws[0], "wearing"):
	default:
		return false
	}
	for _, w := range ws[k:min(len(ws), k+3)] {
		if clothesWords[w] {
			return true
		}
	}
	return false
}

// campWords make starting a fire a campfire or a stove, or a fire in the
// comments: "start a fire at the campsite tonight".
var campWords = canonSetOf(`camp camps campsite campfire camping bonfire bbq barbecue barbeque grill marshmallows
	smores fireplace chulha stove kitchen cook cooking diya lamp hearth survival scouts guides trek trekking
	firewood wood logs comments chat gc twitter insta internet timeline stage dancefloor`)

var (
	// threatComingRules are harm said to be coming to people or a place,
	// with nobody named as doing it; threatComingCheck reads who or what
	// it is said of.
	threatComingRules = []rule{bothViews(`\b` + words(
		// "will be hurt", "are all getting hurt", "is going to get jumped".
		words(`will`, `'?ll`, `is~going~to`, `are~going~to`, `'?s~going~to`, `'?re~going~to`, `is~gonna`,
			`are~gonna`, `'?s~gonna`, `'?re~gonna`, `gonna`, `going~to`, `is`, `are`, `'?s`, `'?re`, `r`)+
			`~(?:`+words(`all`, `definitely`, `finally`, `really`, `literally`)+`~)?`+
			words(`be`, `get`, `getting`, `be~getting`)+`~`+words(`hurt`, `harmed`, `injured`, `attacked`,
			`jumped`, `beaten(?:~up)?`, `run~over`, `burn(?:ed|t)~alive`, `stabbed`, `shot`, `killed`, `murdered`),
		// "the school will burn", "the car is going up in flames", "the
		// auditorium blows up".
		words(`will`, `'?ll`, `is~going~to`, `'?s~going~to`, `is~gonna`, `'?s~gonna`, `gonna`)+`~`+
			words(`burn(?:~down|~to~the~ground)?`, `blow~up`, `go~up~in~flames`, `be~blown~up`, `be~bombed`,
				`be~burn(?:ed|t)~down`, `be~torched`, `be~set~on~fire`),
		words(`is`, `'?s`)+`~`+words(`going~up~in~flames`, `burning~down`, `blowing~up`),
		words(`blows~up`, `burns~down`, `goes~up~in~flames`),
		// "12 science is dead on friday", "you're all dead tomorrow".
		words(`is`, `are`, `'?s`, `'?re`, `r`)+`~(?:`+words(`all`, `so`)+`~)?dead`,
		// "the warden won't see monday", "they won't walk out of there".
		wontLive+`~`+words(
			surviveTill+`~`+words(weekdays, `tomorrow`, `tmrw`, `next~week`),
			words(`walk`, `make~it`, `get`, `come`)+`~out~`+words(`alive`, `of~there`, `of~here`)),
	) + `\b`)}
	// threatUnseenRules are a time someone is said not to live to see that
	// is no time of day or place a threat is paired with, so that it is
	// the threat's own: "the warden won't see the weekend", "won't last
	// until the morning". threatComingCheck reads them as it reads the rest.
	threatUnseenRules = []rule{bothViews(`\b` + wontLive + `~` + surviveTill + `~` +
		words(`the~weekend`, `another~day`, `the~morning`) + `\b`)}
	wontLive = words(`won'?t`, `wont`, `will~not`, `will~never`, `'?ll~never`, `isn'?t~going~to`, `is~not~going~to`,
		`aren'?t~going~to`, `ain'?t~gonna`, `not~gonna`)
	surviveTill = words(`see`, `live~to~see`, `survive`, `make~it~to`, `last~(?:till|until|to)`)
	// threatSymbolRules are a weapon drawn at a school: "🔫🏫".
	threatSymbolRules = []rule{bothViews(`(?:🔫|🔪|💣|🧨|🗡)[^\pL\pN]{0,3}(?:🏫|🏛|🏢)`)}
)

// threatSymbolCheck turns down a weapon drawn at a school in a sentence
// that names a game, a toy or a festival: "🔫🏫 game night at school".
func (f *scannedField) threatSymbolCheck(v *view, m []int) bool {
	ws := wordsIn(sentenceAround(v.text, m[0], m[1]))
	return !unnegatedIn(ws, gameWords, toyWeapon, festivalWords) && !storyNamed(ws)
}

// unnegatedIn reports a word of any of sets in ws, and none with a
// negation in the three words before it: a weapon "for the cake" or "at the
// game" is excused, but "this is not a game", "no more games", "it's not a
// toy" and "it's not for cutting fruit" say the opposite, and so does "to
// the camp, not for firewood".
func unnegatedIn(ws []string, sets ...map[string]bool) bool {
	found := false
	for k, w := range ws {
		in := false
		for _, set := range sets {
			in = in || set[w]
		}
		if in {
			if anyIn(ws[max(0, k-3):k], excuseNegators) {
				return false
			}
			found = true
		}
	}
	return found
}

// excuseNegators, just before a prop, a game or a toy, deny it.
var excuseNegators = canonSetOf(`not no never isn't isnt ain't aint wasn't wasnt nahi nhi na without`)

// threatComingCheck reads who or what harm is said to be coming to
// (threatSubject), and turns down the readings that are not a threat. Harm
// to people needs a group picked out by where it is or who it is ("every
// single person in that class", "everyone in the canteen at 1pm", "the guys
// who stole my book") or all of them ("you're all getting hurt"), not people
// at large ("people will get hurt at this concert" worries) and not one
// person ("you'll get hurt" cautions); said in the present ("are getting
// hurt") it needs a day as well, or it reports. A place or a vehicle that
// will burn needs one picked out, not the speaker's ("my house will burn
// down if I leave the iron on"); "dead" needs a time straight after it;
// "won't see" needs a time that is not an owner ("monday's class"). A
// condition that demands nothing, a "by" that names what does it ("hurt by
// the news"), a story, a game, an event and anything that happens again and
// again excuse all of them.
func (f *scannedField) threatComingCheck(v *view, m []int) bool {
	text := v.text
	ws := wordsIn(text[m[0]:m[1]])
	if len(ws) == 0 {
		return false
	}
	subject := threatSubject(text, m[0])
	next := nextWords(clauseAfter(text, m[1]), 6)
	last := ws[len(ws)-1]
	dead := is(last, "dead")
	survive := !dead && (hasWord(ws, "see") || hasWord(ws, "survive") || hasWord(ws, "last") ||
		hasWord(ws, "make") && !hasWord(ws, "out"))
	out := !dead && !survive && hasWord(ws, "out")
	ruin := !dead && !survive && !out && anyIn(ws, ruinWords)
	// What each reading needs of its subject, and "dead" of its time, is
	// read before the sentence round it, which costs more.
	switch {
	case dead:
		// "dead on friday", "dead tomorrow", "dead after school".
		if !subject.people(false) || len(next) == 0 || !soonWords[next[0]] && !(len(next) > 1 &&
			deadWhen[next[0]] && (soonWords[next[1]] || is(next[1], "school") || is(next[1], "class"))) {
			return false
		}
	case survive, out:
		if !subject.people(true) {
			return false
		}
	case ruin:
		if !subject.place() {
			return false
		}
	default:
		if !subject.people(false) {
			return false
		}
	}
	around := sentenceAround(text, m[0], m[1])
	sentence := wordsIn(around)
	// A condition of being somewhere is the threat's own: "if anyone from
	// 4th period shows up to the pep rally they're getting shot", "along
	// with the dean if he's in it".
	present := presenceCondition(sentence)
	if subject.pronoun && subjectPronouns[subject.head] && !present {
		return false // "he won't see monday" is as often a prognosis
	}
	if subject.mine && !(survive && subject.kind == nounPerson && !prognosis(text, sentence, m[1])) ||
		habitual(sentence) || storyNamed(sentence) || anyIn(sentence, dreamWords) ||
		untimed(sentence, gameWords) || harmlessCondition(around) && !present {
		return false
	}
	switch {
	case dead:
		// An exam, a test, results or homework in the sentence make it the
		// hyperbole of "you're all dead tomorrow in the exam" or "the class
		// is dead tomorrow, ma'am is checking homework", unless a weapon is
		// named as well.
		if schoolHyperbole(sentence) {
			return false
		}
		// threatCheck reads "dead on" as the idiom; the time read above says
		// it is not.
		return true
	case survive:
		if len(next) > 0 && !nounEnds[next[0]] || strings.HasPrefix(text[m[1]:], "'") || anyIn(sentence, eventWords) ||
			schoolHyperbole(sentence) {
			return false // and not "the teachers won't survive monday, three classes of us"
		}
	case out:
		// "won't walk out of there", with nothing more said of it.
		if (is(last, "there") || is(last, "here")) && len(next) > 0 && !trailFillers[next[0]] && !is(next[0], "alive") &&
			!soonWords[next[0]] {
			return false
		}
	case ruin:
		if untimed(sentence, stageWords) || untimed(sentence, eventWords) {
			return false
		}
		if is(last, "burn") && len(next) > 0 && (!nounEnds[next[0]] || is(next[0], "through")) {
			return false // "will burn a hole in my pocket", "will burn through the budget": no fire
		}
		if len(next) > 1 && is(next[0], "with") && notRuin[next[1]] {
			return false // "the hall will blow up with cheers"
		}
	default:
		// Harm to people: a group picked out, all of them, or whoever is
		// somewhere.
		if !(subject.quantified || subject.picked || hasWord(ws, "all") || present) {
			return false
		}
		if progressiveBe[strings.TrimLeft(ws[0], "'")] && !anyIn(sentence, soonWords) {
			return false // "the kids in that school are getting hurt" reports
		}
		if len(next) > 1 && is(next[0], "by") && !is(next[1], "me") && !is(next[1], "us") && !soonWords[next[1]] {
			return false
		}
		if len(next) > 0 && (strings.HasSuffix(next[0], "ing") || hurtNotThreat[next[0]] || is(next[0], "emotionally")) {
			return false // "you'll all get hurt playing like that", "hurt feelings"
		}
		if eventAfterPreposition(next) {
			return false
		}
	}
	return f.threatCheck(v, m)
}

// prognosis reports an illness, a hospital or a leaving in sentence or just
// after offset i of text, which make "my grandpa won't see monday" or "our
// coach won't see monday" a prognosis or a goodbye. The poster's family is
// read as one always; one of their teachers or classmates ("our maths sir
// won't see monday 🙂") only with one of these named.
func prognosis(text string, sentence []string, i int) bool {
	after := wordsIn(text[i:min(len(text), i+prognosisReach)])
	for _, ws := range [][]string{sentence, after} {
		if anyIn(ws, medicalWords) || anyIn(ws, prognosisWords) || anyIn(ws, leavingWords) {
			return true
		}
	}
	return false
}

// prognosisReach is how many bytes after "won't see monday" prognosis reads
// for a reason.
const prognosisReach = 160

// prognosisWords name a grave illness or where it is treated, or an absence:
// "won't see monday, he's on leave".
var prognosisWords = canonSetOf(`hospital hospitalised hospitalized icu ventilator cancer critical dying hospice
	terminal leave absent holiday holidays trip`)

var (
	// deadWhen are the words that put "dead" at a time: "dead on friday".
	deadWhen = canonSetOf(`on by after this before`)
	// schoolWorkWords are the exams, results and work, and the load of
	// lessons, that a class or its teachers are said to be dead over or not
	// to survive.
	schoolWorkWords = canonSetOf(`exam exams test tests quiz quizzes paper papers midterm midterms boards viva
		vivas practical practicals olympiad assessment mock mocks dpp homework hw assignment assignments project
		projects presentation presentations submission submissions deadline deadlines worksheet worksheets copies
		notebooks checking result results marks grades report card ptm classes periods lectures lessons`)
	// ruinWords are what threatComingRules say happens to a place, and
	// notRuin what makes it a celebration: "blow up with cheers".
	ruinWords = canonSetOf(`burn burned burnt blow blown blows bombed torched flames fire`)
	notRuin   = canonSetOf(`laughter excitement joy cheers cheering noise energy hype love`)
	// subjectFillers open a clause without being part of its subject.
	subjectFillers = canonSetOf(`so and but ok okay lol bro yeah yes well also then now just tbh fr ngl honestly
		listen trust me remember mark my words and`)
	// pickWords, after a group, pick it out: "everyone in that class", "the
	// guys who stole my book".
	pickWords = canonSetOf(`in at from on inside of who that which near around`)
	// sectionWords name a class's section or stream: "12 science", "9 b".
	sectionWords = canonSetOf(`science sci commerce comm arts humanities medical nonmedical a b c d e f`)
	// subjectPronouns stand for people already named.
	subjectPronouns = canonSetOf(`they he she`)
	// presenceVerbs and presenceAfter say someone is somewhere: "shows up",
	// "is in", "comes to".
	presenceVerbs = canonSetOf(`show shows showed turn turns come comes coming step steps walk walks enter enters
		go goes is are 's 're am was were be`)
	presenceAfter = canonSetOf(`up in into inside at to there here around near still`)
	// timePrepositions put an event at a time: "on sports day", "during the
	// annual function".
	timePrepositions = canonSetOf(`on during at before after till until by`)
)

// schoolHyperbole reports a sentence about exams, results, homework or a
// load of lessons that names no weapon, which makes someone "dead tomorrow"
// or not surviving monday a way of talking about school.
func schoolHyperbole(ws []string) bool {
	return anyIn(ws, schoolWorkWords) && !anyIn(ws, weaponHeads) && !anyIn(ws, bluntHeads) && !anyIn(ws, fuelHeads)
}

// presenceCondition reports an "if" whose condition is someone being
// somewhere: "if anyone shows up to the rally", "if he's in it".
func presenceCondition(ws []string) bool {
	for i, w := range ws {
		if w != "if" {
			continue
		}
		for k := i + 1; k < len(ws) && k < i+6; k++ {
			v := strings.TrimLeft(ws[k], "'")
			if j := strings.IndexByte(ws[k], '\''); j > 0 {
				v = ws[k][j:] // "he's"
			}
			if (presenceVerbs[v] || presenceVerbs[ws[k]]) && k+1 < len(ws) && presenceAfter[ws[k+1]] {
				return true
			}
		}
	}
	return false
}

// untimed reports a word of set in ws that is not put at a time: "the
// school will burn on sports day" names when, and "the stage will be on
// fire at the fest" is a performance.
func untimed(ws []string, set map[string]bool) bool {
	for i, w := range ws {
		if !set[w] {
			continue
		}
		timed := false
		for k := max(0, i-3); k < i; k++ {
			timed = timed || timePrepositions[ws[k]]
		}
		if !timed {
			return true
		}
	}
	return false
}

// threatSubject reads who or what the clause ending at offset i of text is
// about: its first noun phrase when what follows that picks it out ("every
// single person in that class", "the guys who stole my book"), or else the
// noun phrase that ends at i ("the warden", "you", "the dean's car"), or a
// class named by its year ("12 science", "9B").
func threatSubject(text string, i int) nounPhrase {
	ws := wordsIn(subjectClause(text, i))
	for len(ws) > 0 && subjectFillers[ws[0]] {
		ws = ws[1:]
	}
	if len(ws) == 0 {
		return nounPhrase{}
	}
	if np := readNoun(ws); np.kind != nounNone {
		switch {
		case np.n == len(ws):
			return np
		case pickWords[ws[np.n]]:
			np.picked = true
			return np
		}
	}
	for k := 1; k < len(ws); k++ {
		if np := readNoun(ws[k:]); np.kind != nounNone && np.n == len(ws)-k {
			return np
		}
	}
	if w := ws[len(ws)-1]; subjectPronouns[w] {
		return nounPhrase{kind: nounPerson, head: w, n: len(ws), pronoun: true}
	}
	if classSection(ws) {
		return nounPhrase{kind: nounPerson, head: ws[len(ws)-1], n: len(ws), quantified: true}
	}
	return nounPhrase{}
}

// subjectClause is clauseBefore read past the dot of a title: "Mr.
// Banerjee's maths class" is one clause.
func subjectClause(text string, i int) string {
	from := max(0, i-64)
	before := text[from:i]
	for {
		j := strings.LastIndexAny(before, ".!?\n,;:")
		if j < 0 {
			return before
		}
		if before[j] == '.' {
			if ws := wordsIn(before[:j]); len(ws) > 0 && titleWords[ws[len(ws)-1]] {
				before = before[:j] + " " + before[j+1:]
				continue
			}
		}
		return before[j+1:]
	}
}

// classSection reports words that end in a class named by its year: "12
// science", "11th commerce", "9b", "class 10".
func classSection(ws []string) bool {
	// year reads "12", "11th" or "9b", and whether a section letter ends it.
	year := func(w string) (ok, lettered bool) {
		n := 0
		for n < len(w) && w[n] >= '0' && w[n] <= '9' {
			n++
		}
		if n == 0 || n > 2 {
			return false, false
		}
		if y, _ := strconv.Atoi(w[:n]); y < 1 || y > 12 {
			return false, false
		}
		switch rest := w[n:]; {
		case rest == "":
			return true, false
		case len(rest) == 1 && rest[0] >= 'a' && rest[0] <= 'f':
			return true, true
		case rest == "st" || rest == "nd" || rest == "rd" || rest == "th":
			return true, false
		}
		return false, false
	}
	k := len(ws)
	if ok, lettered := year(ws[k-1]); ok && (lettered || k >= 2 && (is(ws[k-2], "class") || is(ws[k-2], "grade"))) {
		return true
	}
	ok, _ := year(ws[max(0, k-2)])
	return k >= 2 && ok && sectionWords[ws[k-1]]
}

// Grooming cues: a question about age, school year or where someone lives or
// studies, together with a private-contact or meet-up cue, anywhere in one
// post, in English or some Hinglish. Either half alone is ordinary
// conversation.
var (
	ageQuestionRules = []rule{bothViews(`\b` + words(
		`how~old~(?:`+words(`are`, `r`, `is`)+`~)?`+words(`you`, `u`, `ya`, `yu`),
		`what'?s?~(?:is~)?`+words(`your`, `ur`, `yr`)+`~age`,
		// "what age are you", "you are what age", "what year were you
		// born", "are you still in school": ageCheck turns down "what age
		// are you supposed to start".
		`what~age~`+words(`are`, `r`)+`~`+words(`you`, `u`),
		words(`you`, `u`)+`~`+words(`are`, `r`)+`~what~age`,
		words(`what`, `which`)+`~year~were~`+words(`you`, `u`)+`~born`,
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~still~in~`+words(`school`, `high~school`, `middle~school`,
			`secondary~school`, words(`class`, `grade`, `year`, `std`)+`~\d{1,2}`),
		words(`what`, `which`)+`~`+words(`year`, `grade`, `class`, `standard`, `std`, `form`, `school~year`,
			`year~\d{1,2}~form`, `j?ss`, `sss`, `js`)+`~(?:of~`+words(`school`, `college`, `uni`)+`~)?`+
			words(`are`, `r`)+`~`+words(`you`, `u`),
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~(?:in~)?`+words(`high~school`, `middle~school`,
			`secondary~school`, `year~\d{1,2}`, `grade~\d{1,2}`, `class~\d{1,2}`, `std~\d{1,2}`,
			words(`jss`, `js`, `sss?`)+`~?[1-3]`,
			`\d{1,2}(?:st|nd|rd|th)~`+words(`grade`, `class`, `standard`, `std`)),
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~`+words(`under`, `over`, `below`, `above`)+`~1\d`,
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~1\d`,
		`where~(?:`+words(`do`, `d`)+`~)?`+words(`you`, `u`)+`~`+words(`live`, `stay`, `go~to~school`, `study`),
		`where~(?:`+words(`are`, `r`)+`~)?`+words(`you`, `u`)+`~from`,
		// "where in Delhi do you live", "where exactly do you stay".
		`where~(?:in~\pL+|exactly)~`+words(`do`, `d`)+`~`+words(`you`, `u`)+`~`+words(`live`, `stay`, `study`,
			`go~to~school`),
		// "tu kaunse school se hai"
		words(`tu`, `tum`, `aap`)+`~`+words(`kaunse`, `konse`, `kis`, `kaunsa`, `konsa`, `kaunsi`, `konsi`)+`~`+
			words(`school`, `college`, `class`, `area`, `city`)+`~`+words(`mein`, `me`, `se`)+`~`+words(`hai`, `ho`, `h`),
		// Looking for someone of an age: "any 14yo girls here?", "13-15 yo
		// girls", "any 14 yr olds?", "any girls in class 9?".
		ageNumber+ageUnit+`~`+youthNoun,
		words(`any`, `looking~for`, `want`, `need`, `searching~for`)+`~(?:`+words(`a`, `an`, `some`)+`~)?`+
			ageNumber+ageUnit,
		// "girls under 16", "any girls aged 14", "looking for a girl
		// 13-15", "any class 9 girls".
		youthNoun+`~(?:here~)?`+words(`under`, `below`, `aged?`)+`~\d{1,2}`,
		// "girls 13-16 dm me", "girls 14+": ageCheck wants nothing after
		// the age but who is wanted or how to reach the poster, not "girls
		// 13-16 category results".
		youthNoun+`[ \t]+\d{1,2}(?:~?(?:-|to)~?\d{1,2})?`,
		// "any 9th graders here", "any 8th class girls".
		// With no "any", ageCheck wants more than one youth and something
		// asked of them after: "8th class girls dm me", "9th grade girls
		// hmu", "class 8 girls dm", not "class 8 girls relay results".
		`(?:any~)?\d{1,2}(?:st|nd|rd|th)~`+words(`graders?`, words(`class`, `grade`, `std`, `standard`)+`~`+
			youthNoun),
		words(`any`, `looking~for`, `searching~for`)+`~(?:`+words(`a`, `an`, `some`)+`~)?`+youthNoun+`~\d{1,2}`+
			`(?:~?(?:-|to)~?\d{1,2})?`,
		`(?:any~)?`+words(`class`, `grade`, `std`, `standard`, `year`)+`~\d{1,2}(?:st|nd|rd|th)?~`+youthNoun,
		`any~`+youthNoun+`~(?:here~)?`+words(`in`, `from`)+`~(?:`+words(`class`, `grade`, `std`, `standard`,
			`year`)+`~\d{1,2}|\d{1,2}(?:st|nd|rd|th)(?:~`+words(`grade`, `class`, `std`, `standard`)+`)?)`,
		`where~(?:exactly~)?`+words(`is`, `'?s`)+`~`+words(`your`, `ur`)+`~`+readerPlaces,
		// "which floor is your room on", "which floor of the girls hostel is
		// your room on".
		words(`what`, `which`)+`~`+readerPlaces+`~(?:of~(?:`+words(`the`, `your`, `ur`)+`~)?(?:[\pL']+~){0,2}?)?`+
			words(`is`, `'?s`)+`~`+words(`your`, `ur`)+`~`+readerPlaces,
		// Said of the reader, a question or not: "you're in grade 6", "you're
		// in 9th right", "you're in which grade", "you're 14". ageCheck turns
		// down "when you're in 12th" and "you're 15 minutes late".
		words(`you'?re`, `youre`, `you~are`, `u'?re`)+`~(?:`+words(`in`, `doing`, `studying~in`)+`~)?`+readerGrade,
		words(`ur`, `u~r`)+`~in~`+readerGrade,
		words(`you'?re`, `youre`, `you~are`, `u'?re`, `ur`, `u~r`)+`~1[0-7]`,
		// "tum 8th mein ho na", "class 9 mein ho".
		words(`class~\d{1,2}`, `\d{1,2}(?:st|nd|rd|th)(?:~`+words(`class`, `std`, `standard`)+`)?`)+`~`+
			words(`mein`, `me`, `main`, `mai`)+`~`+words(`ho`, `padhte~ho`, `padhti~ho`),
		// "do you go to the school on MG Road", "that's your school right?",
		// "tell me your school name", "what time do you get out".
		words(`do`, `d`)+`~`+words(`you`, `u`)+`~`+words(`go~to`, `study~at`, `attend`)+`~`+words(`the`, `that`, `this`)+
			`~`+readerPlaces,
		words(`tell`, `send`, `share`, `drop`, `give`, `dm`, `text`)+`~me~`+words(`your`, `ur`)+`~`+
			words(readerPlaces, `address`, `age`, `class`, `grade`, `area`, `location`),
		`what~time~(?:`+words(`do`, `d`)+`~)?`+words(`you`, `u`)+`~`+words(`get~out`, `leave`, `finish`, `get~off`,
			`get~home`, `reach~home`, `go~home`, `come~out`),
		// "what time does your school finish", "which gate do you use".
		`what~time~`+words(`does`, `do`, `d`)+`~`+words(`your`, `ur`)+`~`+words(`school`, `tuition`, `coaching`,
			`lessons?`)+`~`+words(`finish`, `end`, `get~out`, `let~out`, `close`, `get~over`, `over`),
		words(`what`, `which`)+`~`+words(`gate`, `entrance`, `exit`, `bus`, `route`)+`~`+words(`do`, `d`)+`~`+
			words(`you`, `u`)+`~`+words(`use`, `take`, `leave~from`, `come~out~of`, `get~off~at`, `wait~at`),
		// Nigerian Pidgin: "where you dey stay", "which estate for Lagos you
		// dey stay".
		`where~`+words(`you`, `u`)+`~dey~`+words(`stay`, `live`, `school`),
		`which~`+words(`estate`, `area`, `street`, `place`, `side`, `part`, `school`, `compound`, `hostel`, `town`,
			`city`)+`~(?:for~\pL+~)?`+words(`you`, `u`)+`~dey~`+words(`stay`, `live`, `school`, `attend`),
		// The reader's age remarked on: "you sound young", "u look so young".
		words(`you`, `u`)+`~`+words(`sound`, `sounds`, `seem`, `seems`, `look`, `looks`)+`~(?:`+words(`so`, `really`,
			`very`, `kinda`, `pretty`, `quite`, `real`)+`~)?`+words(`young`, `younger`, `underage`),
		// "kis city se ho", "kaunse area mein rehti ho".
		words(`kis`, `kaunse`, `konse`, `kaunsa`, `konsa`, `kaunsi`, `konsi`)+`~`+words(`city`, `area`, `school`,
			`college`, `class`, `saal`, `jagah`, `sheher`, `shehar`, `mohalla`, `colony`, `sector`, `hostel`, `coaching`, `tuition`,
			`gaon`)+`~`+words(`se`, `mein`, `me`, `main`)+`~(?:`+words(`rehte`, `rehti`, `padhte`, `padhti`, `jaate`,
			`jaati`, `jate`, `jati`)+`~)?`+words(`ho`, `hai`, `h`),
		// "which college do you go to", "what area are you in": the verb has
		// to be about being there, so "which college do you suggest" is
		// advice.
		words(`what`, `which`)+`~`+words(readerPlaces, readerAreas)+`~`+
			words(`do`, `d`)+`~`+words(`you`, `u`)+`~`+words(`go~to`, `attend`, `study~at`, `study~in`, `study`,
			`live~in`, `live`, `stay~in`, `stay`, `come~from`, `belong~to`),
		words(`what`, `which`)+`~`+words(readerPlaces, readerAreas)+`~(?:`+
			words(`are`, `r`)+`~)?`+words(`you`, `u`)+`~`+words(`in`, `at`, `from`, `studying`, `living`, `staying`),
		`what'?s?~(?:is~)?`+words(`your`, `ur`)+`~`+words(`school`, `address`, `location`, `city`, `town`, `area`,
			`postcode`, `pincode`, `zip`),
		words(`do`, `d`)+`~`+words(`you`, `u`)+`~live~`+words(`near`, `in`, `around`, `close`),
		`mature~for~`+words(`your`, `ur`)+`~age`,
		// "so mature for 13", "mature for a 14 yr old".
		`mature~for~(?:`+words(`a`, `an`)+`~)?1\d`,
		// Hinglish: "kitne saal ke ho", "kahan rehte ho", "kaunsa school".
		words(`kitne`, `kitni`)+`~saal~`+words(`ke`, `ki`)+`~`+words(`ho`, `hai`, `h`),
		words(`kitni`, `kya`)+`~`+words(`umar`, `umr`, `age`)+`~`+words(`hai`, `h`),
		// "umar kya hai", "age kya hai tumhari": ageCheck wants nobody
		// else's age before or after it ("uski age kya hai", "jee ke liye
		// age kya hai").
		words(`umar`, `umr`, `age`)+`~kya~`+words(`hai`, `h`),
		words(`tumhari`, `teri`, `aapki`, `apki`)+`~`+words(`umar`, `umr`, `age`),
		words(`kahan`, `kaha`, `kahaan`)+`~`+words(`rehte`, `rehti`, `rahte`, `rahti`, `padhte`, `padhti`)+`~`+
			words(`ho`, `hai`, `h`),
		// "kaunse school mein ho": with "ho", about the reader, not "kaunsa
		// college best hai" (which college is best).
		words(`kaunsa`, `kaun~sa`, `konsa`, `kaunse`, `konse`, `kaunsi`, `konsi`)+`~`+words(`school`, `college`, `class`)+
			`~(?:`+words(`mein`, `me`, `main`, `se`)+`~)?(?:`+words(`padhte`, `padhti`, `jaate`, `jate`, `jaati`, `jati`,
			`rehte`, `rehti`)+`~)?`+words(`ho`, `hai~tumhara`, `hai~tera`, `hai~aapka`),
		words(`tumhara`, `tumhari`, `tera`, `teri`, `aapka`, `aapki`, `apka`, `apki`)+`~`+words(`school`, `college`, `ghar`)+
			`~`+words(`kaunsa`, `konsa`, `kaunsi`, `konsi`, `kahan`, `kaha`, `kahaan`),
	) + `\b`),
		// Questions that end in their "?", where no word boundary follows:
		// "your age?", "u 14?", and "age?", "how old?" or "which school?"
		// opening a sentence.
		bothViews(words(
			`\b`+words(`your`, `ur`)+`~age[^\pL\pN?]*\?`,
			`\b`+words(`you`, `u`)+`~1\d[^\pL\pN?]*\?`,
			`(?:^|[.!?\n,;:])[^\pL\pN]*`+words(`age`, `how~old`, words(`which`, `what`)+`~`+
				words(`school`, `college`, `class`, `grade`, `year`, `area`, `locality`, `city`, `town`, `hostel`,
					`room`, `block`, `floor`, `building`, `flat`, `house`, `street`, `colony`, `sector`, `part`,
					`hall`, `centre`, `center`, `gate`, `suburb`, `estate`, `neighbou?rhood`, `village`)+
				`(?:~`+words(`exactly`, `and~room~(?:number|no)`, `and~room`, `and~block`, `and~floor`)+`)?`)+
				`[^\pL\pN?]*\?`,
			// A child's year asked alone: "Year 7?", "6th standard?" ("Year
			// 12?" is as often a question about a syllabus); "that's your
			// school right?"; "school kaunsa?".
			`(?:^|[.!?\n,;:])[^\pL\pN]*`+words(words(`year`, `grade`, `class`, `std`, `standard`)+`~(?:[1-9]|10)`,
				`(?:[1-9]|10)(?:st|nd|rd|th)(?:~`+words(`grade`, `class`, `std`, `standard`)+`)?`)+`[^\pL\pN?]*\?`,
			`\b`+words(`that'?s`, `thats`, `is~that`, `this~is`, `is~this`)+`~`+words(`your`, `ur`)+`~`+readerPlaces+
				`(?:~`+words(`right`, `ryt`, `na`, `no`, `yeah`)+`)?[^\pL\pN?]*\?`,
			`(?:^|[.!?\n,;:])[^\pL\pN]*`+words(`school`, `college`, `class`, `tuition`, `coaching`, `area`, `city`)+`~`+
				words(`kaunsa`, `konsa`, `kaunsi`, `konsi`, `kaunse`, `konse`, `kahan`, `kaha`)+`[^\pL\pN?]*\?`,
			// A guess put as a question: "so you're in 10th?", "you're 14?",
			// "13 or 14?", "year 8 or year 9?".
			`\b`+words(`you'?re`, `you~are`, `u~r`, `ur`, `u'?re`)+`~(?:in~)?`+words(`1\d`,
				`\d{1,2}(?:st|nd|rd|th)(?:~`+words(`grade`, `class`, `std`, `standard`)+`)?`,
				words(`year`, `grade`, `class`, `std`)+`~\d{1,2}`)+`[^\pL\pN?]*\?`,
			`\b`+words(`you`, `u`)+`~in~`+words(words(`year`, `grade`, `class`, `std`, `form`)+`~\d{1,2}(?:~or~\d{1,2})?`,
				words(`high`, `middle`, `secondary`, `junior`, `primary`)+`~school`)+`[^\pL\pN?]*\?`,
			`(?:^|[.!?\n,;:])[^\pL\pN]*`+words(`1[0-7]`, words(`year`, `grade`, `class`, `std`)+`~\d{1,2}`)+`~or~`+
				words(`1[0-7]`, words(`year`, `grade`, `class`, `std`)+`~\d{1,2}`)+`[^\pL\pN?]*\?`,
		)),
	}
	// readerPlaces are where a reader is asked to say they study or live:
	// "which hostel are you in", "where exactly is your coaching". Not a
	// branch, a batch or a section, which a college asks of everyone.
	readerPlaces = words(`school`, `college`, `hostel`, `coaching`, `tuition`, `institute`, `academy`, `campus`,
		`block`, `building`, `floor`, `room`, `flat`, `apartment`, `society`, `colony`, `sector`, `street`, `road`,
		`house`, `home`)
	// readerGrade is a year at school: "grade 6", "9th", "class 10th",
	// "which grade".
	readerGrade = words(words(`grade`, `class`, `year`, `std`, `standard`, `form`)+`~\d{1,2}(?:st|nd|rd|th)?`,
		`\d{1,2}(?:st|nd|rd|th)(?:~`+words(`grade`, `class`, `std`, `standard`)+`)?`,
		words(`jss`, `js`, `sss?`)+`~?[1-3]`,
		words(`which`, `what`)+`~`+words(`grade`, `class`, `year`, `std`, `standard`, `j?ss`, `sss`))
	// readerAreas are where a reader lives or studies besides readerPlaces,
	// asked with a verb of being there: "which suburb are you in", "which
	// coaching centre are you at", "which uni do you go to".
	readerAreas = words(`uni`, `city`, `town`, `area`, `place`, `locality`, `suburb`, `estate`, `neighbou?rhood`,
		`village`, `hall`, `(?:coaching~|tuition~|exam~)?cent(?:re|er)`)
	// lureGifts are what a stranger offers a child: a gift, a phone, game
	// currency, money or a recharge.
	lureGifts = words(`gifts?`, `presents?`, `phone`, `iphone`, `chocolates?`, `treats?`, `something~nice`, `robux`,
		`v~?bucks`, `uc`, `diamonds`, `gems`, `skins?`, `money`, `cash`, `paise`, `recharge`, `top~?up`,
		words(`whatever`, `anything`)+`~`+words(`you`, `u`)+`~`+words(`want`, `like`))
	// ageNumber, ageUnit and youthNoun make up an age someone is looked for
	// at: "14yo girls", "13-15 year old boys".
	ageNumber = `\d{1,2}(?:~?(?:-|to)~?\d{1,2})?~?`
	ageUnit   = words(`yo`, `y~o`, `y/o`, `yos`, `yrs?~old`, `years?~old`, `yrs?~olds`, `years?~olds`)
	youthNoun = words(`girls?`, `boys?`, `guys?`, `kids?`, `gf`, `bf`, `girlfriend`, `boyfriend`, `females?`,
		`males?`, `teens?`, `students?`)
	// "asl" (age, sex, location) is asked at the start of what is said:
	// "asl?", "hey asl pls". After a word it is the slang intensifier ("bored
	// asl") or the sign language ("learning ASL"), and so it is before
	// "club" or "class". Plain view only: the loose view reads l as i, and
	// "a s i" is "as I". aslCheck reads where it stands.
	aslRules        = []rule{plainOnly(`\ba~s~l\b`)}
	privateCueRules = []rule{bothViews(`\b` + words(
		words(`dm`, `pm`, `message`, `msg`, `text`, `txt`, `inbox`, `snap`, `call`, `ping`, `add`)+`~me`,
		words(`talk`, `talking`, `chat`, `chatting`, `text`, `texting`, `message`, `messaging`, `speak`, `speaking`)+
			`~`+words(`privately`, `in~private`, `alone`, `somewhere~else`, `elsewhere`, `in~dms?`, `one~on~one`,
			`1~on~1`),
		// "we can talk after, alone"
		words(`talk`, `chat`, `meet`)+`~(?:[\pL\pN]+~){1,2}?alone`,
		// "let's talk where nobody else can read it"
		`where~`+words(`nobody`, `no~one`, `noone`)+`~(?:else~)?`+words(`can`, `will`, `could`)+`~`+
			words(`see`, `read`, `find`, `hear`),
		// "we could meet", "just us two", "keep it quiet", "I'll come by
		// tonight", "I'll wait for you after school".
		`we~`+words(`could`, `can`, `will`, `'?ll`)+`~meet`,
		`just~`+words(`us~two`, `us~2`, `the~two~of~us`, `you~and~me`, `u~and~me`, `you~and~i`, `u~and~i`),
		`keep~`+words(`this`, `it`)+`~`+words(`quiet`, `on~the~down~low`, `on~the~dl`, `hush~hush`, `to~yourself`,
			`to~urself`, `private`, `just~us`),
		words(`i'?ll`, `i~will`, `i~can`, `i~could`, `ill`, `i'?d`, `lemme`, `let~me`, `i'?m~gonna`, `i~am~going~to`,
			`i~wanna`, `i~want~to`)+`~`+words(`come~by`, `come~over`, `come~round`, `come~around`, `drop~by`,
			words(`visit`, `meet`, `come~(?:and~)?`+words(`meet`, `see`, `visit`, `get`, `find`))+`~`+words(`you`, `u`),
			`pick~`+words(`you`, `u`)+`~up`, `wait~for~`+words(`you`, `u`), `be~waiting~for~`+words(`you`, `u`),
			`keep~`+words(`you`, `u`)+`~company`, `be~waiting`, `wait~there`, `walk~(?:with~)?`+words(`you`, `u`),
			`come~up`, `wait~outside`, `be~outside`,
			// "I'll buy you a gift", "i want to send you a present", "I'll
			// get you a new phone", "i can give you free vbucks", "I'll pay
			// you", "I'll recharge your phone".
			words(`buy`, `get`, `send`, `give`, `bring`)+`~`+words(`you`, `u`)+`~(?:`+words(`a`, `an`, `some`, `a~new`,
				`new`, `a~nice`, `nice`, `some~free`, `free`, `more`)+`~)?`+lureGifts,
			`pay~`+words(`you`, `u`),
			words(`recharge`, `top~up`, `topup`)+`~`+words(`your`, `ur`)+`~`+words(`phone`, `mobile`, `number`, `sim`,
				`account`)),
		// "can i see what you look like", "send me what you look like", "do
		// your parents let you go out alone".
		words(`see`, `know`, `send~me`, `show~me`)+`~what~`+words(`you`, `u`)+`~`+words(`look`, `looks`)+`~like`,
		words(`go`, `going`, `goes`)+`~out~`+words(`alone`, `by~yourself`, `by~urself`, `on~your~own`, `on~ur~own`),
		// "I fit come pick you after lesson", "do u walk home alone?".
		`come~pick~`+words(`you`, `u`),
		words(`you`, `u`)+`~(?:`+words(`usually`, `always`, `ever`, `often`, `still`)+`~)?`+words(`walk`, `go`, `get`,
			`come`, `travel`, `cycle`)+`~home~`+words(`alone`, `by~yourself`, `by~urself`, `on~your~own`, `on~ur~own`),
		// "come alone", "come without your friends", "come to my room",
		// "wait for me after the evening batch", "meet me at the gate".
		`come~`+words(`alone`, `by~yourself`, `by~urself`, `on~your~own`),
		`come~`+words(`without`, `w/o`)+`~`+words(`your`, `ur`, `telling`, `anyone`),
		`come~to~`+words(`my`, `our`, `your`, `ur`)+`~`+words(`place`, `house`, `home`, `room`, `flat`, `hostel`, `car`,
			`office`, `apartment`),
		// "I'll come to your school", "i can come to ur tuition".
		`come~to~`+words(`your`, `ur`)+`~`+words(`school`, `college`, `class`, `tuition`, `coaching`, `classes`),
		// "can i see a pic", "could i get a selfie": privateCue wants it
		// not of something else ("can i see a pic of the question").
		words(`can`, `could`, `may`)+`~i~`+words(`see`, `get`, `have`)+`~(?:`+words(`a`, `some`, `your`, `ur`,
			`another`, `more`, `a~new`)+`~)?`+words(`pics?`, `pictures?`, `photos?`, `selfies?`),
		words(`wait`, `waiting`)+`~for~me`, `meet~me~`+words(`at`, `after`, `outside`, `near`, `behind`, `by`, `in`),
		// "your parents at work till late?", "your mum works nights", "do
		// your parents know".
		words(`your`, `ur`)+`~`+words(`parents`, `mom`, `mum`, `dad`, `mummy`, `papa`)+`~(?:`+words(`are`, `is`, `'?s`,
			`r`)+`~)?(?:`+words(`usually`, `always`, `still`)+`~)?`+words(`at~work`, `out`, `away`, `not~home`,
			`working~late`, `work~nights`, `works~nights`, `working~nights`, `out~of~town`, `travelling`, `traveling`),
		words(`do`, `does`)+`~`+words(`your`, `ur`)+`~`+words(`parents`, `mom`, `mum`, `dad`, `family`)+`~know`,
		// "they never have to find out", "your parents don't need to know".
		words(`they`, `nobody`, `no~one`, `noone`, `your~parents`, `ur~parents`, `anyone`, `your~mom`, `your~mum`)+
			`~(?:`+words(`will`, `would`, `don'?t`, `dont`, `doesn'?t`)+`~)?(?:never~)?`+words(`has`, `have`, `need`,
			`needs`, `gotta`)+`~to~`+words(`know`, `find~out`),
		words(`don'?t`, `dont`, `do~not`)+`~`+words(`tell`, `mention`, `show`)+`~`+words(`the`, `my`, `him`, `her`,
			`them`, `this`, `it`, `me~to`),
		`secret~`+words(`friends?`, `chats?`, `bf`, `gf`, `boyfriend`, `girlfriend`, `relationship`, `meetings?`),
		`delete~`+words(`this`, `it`, `the~chat`, `our~chat`, `this~chat`, `this~thread`, `the~thread`, `these~messages`,
			`the~messages`, `everything`)+`~after`,
		words(`go`, `move`, `switch`, `talk`, `chat`, `take~this`, `take~it`)+`~private`,
		words(`where`, `somewhere`)+`~`+words(`nobody`, `no~one`, `noone`, `they`, `the~mods`, `mods`, `your~parents`,
			`ur~parents`, `anyone`, `people`, `others`)+`~(?:else~)?`+words(`can'?t`, `cant`, `cannot`, `can~not`,
			`won'?t`, `wont`, `will~not`)+`~`+words(`see`, `read`, `find`, `hear`),
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~(?:`+words(`usually`, `ever`, `always`, `often`, `mostly`)+`~)?`+
			words(`by~yourself`, `by~urself`, `on~your~own`, `on~ur~own`),
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~`+words(`usually`, `ever`, `always`, `often`, `mostly`)+`~alone`,
		// "without your parents knowing", "tell your parents you're at a
		// friend's", "does your mom check your phone".
		`without~`+words(`your`, `ur`)+`~`+words(`parents`, `mom`, `mum`, `dad`, `family`, `folks`)+`~`+
			words(`knowing`, `finding~out`, `noticing`),
		`tell~`+words(`your`, `ur`)+`~`+words(`parents`, `mom`, `mum`, `dad`, `family`, `folks`)+`~`+
			words(`you'?re`, `ur`, `u~r`, `you~are`)+`~`+words(`at`, `with`, `going`, `staying`),
		words(`does`, `do`)+`~`+words(`your`, `ur`)+`~`+words(`parents`, `mom`, `mum`, `dad`)+`~`+
			words(`check`, `see`, `read`, `look~at`, `go~through`)+`~`+words(`your`, `ur`)+`~`+
			words(`phone`, `messages`, `chats`, `texts`, `dms`),
		`private~`+words(`chat`, `talk`, `messages?`, `convo`, `conversation`),
		`somewhere~(?:more~)?private`,
		words(`chat`, `continue`, `move~this`, `take~this`, `move~it`, `take~it`)+`~`+words(`somewhere~else`, `elsewhere`),
		`meet~`+words(`up`, `irl`, `in~person`, `me`, `somewhere`, `in~real~life`),
		`meetup`, `wanna~meet`, `let'?s~meet`, `lets~meet`, `can~we~meet`, `we~should~meet`,
		`send~(?:me~)?(?:`+words(`a`, `some`, `your`, `ur`)+`~)?`+words(`pics?`, `pictures?`, `photos?`, `selfies?`,
			`nudes`, `snaps`, `vids?`, `videos?`),
		// "I'd love to see a photo of you".
		words(`see`, `get`, `have`)+`~(?:`+words(`a`, `some`, `more`, `another`)+`~)?`+words(`pics?`, `pictures?`,
			`photos?`, `selfies?`)+`~of~`+words(`you`, `u`, `yourself`, `urself`),
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~(?:home~)?alone`,
		words(`are`, `r`)+`~`+words(`your`, `ur`)+`~`+words(`parents`, `mom`, `mum`, `dad`)+`~`+
			words(`home`, `around`, `there`, `awake`),
		words(`don'?t`, `dont`, `do~not`)+`~tell~`+words(`your`, `ur`, `anyone`, `anybody`),
		// "tell no one", "no need to tell anyone", and Pidgin "no tell your
		// mummy".
		`tell~`+words(`no~one`, `noone`, `nobody`),
		words(`no`, `no~need~to`)+`~tell~`+words(`anyone`, `anybody`, `them`, `person`, words(`your`, `ur`)+`~`+
			words(`parents`, `mum`, `mom`, `mummy`, `mama`, `dad`, `papa`, `family`, `folks`, `friends`)),
		words(`our`, `a`)+`~(?:little~)?secret`,
		`keep~`+words(`this`, `it`)+`~(?:`+words(`a`, `our`)+`~)?secret`,
		`keep~(?:`+words(`our`, `these`, `the`, `this`)+`~)?`+words(`this`, `it`, `chats?`, `convos?`,
			`conversations?`, `messages`, `texts`, `dms`, `talks?`)+`~between~`+words(`us`, `you~and~me`, `u~and~me`,
			`the~two~of~us`),
		`between~`+words(`you`, `u`)+`~and~me`, `between~us`,
		`send~(?:me~)?`+words(`your`, `ur`)+`~(?:live~)?`+words(`location`, `loc`, `address`, `pin`, `pincode`),
		`share~`+words(`your`, `ur`)+`~(?:live~)?`+words(`location`, `loc`, `address`),
		`hmu`, `come~over`,
		// A call or a camera, or meeting: "facetime me", "video call", "vc
		// me", "on cam", "hang out", "pick you up", "show me your face".
		words(`facetime`, `face~time`)+`(?:~`+words(`me`, `us`)+`)?`, `video~?`+words(`call`, `chat`), `vc~me`,
		`on~cam`, `cam~`+words(`me`, `chat`, `show`), `webcam`, `hang~out`, `hangout`,
		`pick~`+words(`you`, `u`)+`~up`,
		`show~me~`+words(`yourself`, `urself`, words(`your`, `ur`)+`~`+words(`face`, `body`, `room`, `pics?`, `photos?`)),
		// Hinglish: "dm karo", "insta pe aao", "akele ho", "milte hain",
		// "photo bhejo".
		words(`dm`, `pm`, `msg`, `message`, `text`, `call`, `baat`)+`~`+karo,
		words(`photo`, `photos`, `pic`, `pics`, `selfie`, `video`)+`~`+words(`bhejo`, `bhej`, `bhejna`, `bhej~do`,
			`dikhao`, `dikha`),
		words(`pe`, `par`, `pr`)+`~`+words(`aao`, `aaja`, `aajao`, `aa~jao`, `milo`),
		words(`akele`, `akeli`, `akela`)+`~`+words(`ho`, `hai`, `h`, `aana`, `aao`, `aaja`, `milna`, `mil`),
		`ko~`+words(`mat`, `na`, `nahi`)+`~`+words(`bolna`, `batana`, `bata`, `bol`, `batao`),
		`kisi~ko~`+words(`bolna`, `batana`, `bata`, `bol`, `batao`)+`~`+words(`mat`, `na`, `nahi`, `nai`),
		`private~`+words(`rakhna`, `rakho`, `rakh`),
		words(`ek~baar`, `kabhi`, `akele`)+`~`+words(`milo`, `milte`, `mil~lo`, `milna`),
		`milne~`+words(`aa`, `aao`, `aaja`, `aajao`, `aa~jao`, `aa~jaao`, `aana`),
		words(`gift`, `gifts`, `chocolate`, `chocolates`, `toffee`, `phone`, `paise`)+`~`+words(`dunga`, `dungi`,
			`dilaunga`, `dilaungi`, `doonga`),
		words(`milte`, `milna`)+`~`+words(`hain`, `hai`, `h`),
		`private~`+words(`mein`, `me`, `main`)+`~baat`,
	) + `\b`),
		// "pic?": a request for one that ends in its "?"; "alone" where it
		// ends a sentence, which privateCue reads.
		bothViews(`\b` + words(`pics?`, `pictures?`, `photos?`, `selfies?`) + `[^\pL\pN?]*\?`),
		// "want a gift?", "want some free robux?": an offer asked.
		bothViews(`\b(?:` + words(`do`, `d`) + `~)?(?:` + words(`you`, `u`) + `~)?` + words(`want`, `wanna`, `need`) +
			`~(?:` + words(`a`, `an`, `some`, `some~free`, `free`, `more`, `a~new`, `new`) + `~)?` + lureGifts +
			`[^\pL\pN?]*\?`),
		bothViews(`\balone[^\pL\pN]*(?:[.!?\n]|$)`),
		// "let's meet at the metro station, just you", where it ends.
		bothViews(`\bjust~` + words(`you`, `u`, `us`) + `[^\pL\pN]*(?:[.!?\n]|$)`),
		// "age? dm", "how old are you? pm pls": a bare "dm" said as a
		// sentence of its own.
		bothViews(`(?:^|[.!?\n,;:])[^\pL\pN]*(?:` + words(`pls`, `plz`, `please`, `just`) + `~)?` +
			words(`dm`, `pm`, `inbox`, `dms`) + `(?:~` + words(`pls`, `plz`, `please`, `me`) + `)?[^\pL\pN]*(?:[.!?\n]|$)`),
		// "girls 13-15 dm": a bare "dm" ending the post after its words,
		// which privateCue reads where it stands, not "I asked in dm".
		bothViews(`\b` + words(`dm`, `pm`, `dms`) + `(?:~` + words(`pls`, `plz`, `please`) + `)?[^\pL\pN]*$`),
		// "I can train you privately", "I can help with your project
		// privately": cueCheck wants the poster offering it to the reader.
		bothViews(`\b` + words(`privately`, `in~private`) + `\b`),
	}
)

// cueCheck turns down the idioms that share their words with a private-contact
// or meet-up cue: "call me crazy", "we should meet the deadline", "are you
// alone in thinking", "a secret that", "between you and me, the food is
// awful", "campus pics?", "seniors se baat karo" (talk to the seniors) and
// "library mein milte hain" (they are in the library).
func cueCheck(v *view, m []int) bool { return !v.reported(m) && privateCue(v, m, true) }

// cueBesideAnAge is cueCheck for a post that asks the reader's age, where a
// meeting is a cue wherever it is and whoever else is there.
func cueBesideAnAge(v *view, m []int) bool { return !v.reported(m) && privateCue(v, m, false) }

// privateCue is cueCheck; openMeetings turns down a meeting in the open.
func privateCue(v *view, m []int, openMeetings bool) bool {
	text := v.text
	ws := wordsIn(text[m[0]:m[1]])
	if len(ws) == 0 {
		return true
	}
	first, last := ws[0], ws[len(ws)-1]
	next := nextWords(clauseAfter(text, m[1]), 2)
	prev := wordsIn(clauseBefore(text, m[0]))
	nextIn := func(k int, set map[string]bool) bool { return len(next) > k && set[next[k]] }
	// A meeting somewhere public or with a group, and a visit with an
	// errand, are not a private one: "we can meet at the library for group
	// study", "let's meet at the mess for dinner", "I'll come by tonight with
	// the notes", "come to my place for group study". A gate, a street or
	// anywhere outside is not public in this sense, and nothing excuses a cue
	// that asks for the reader alone ("come alone", "come without your
	// friends", "just us two").
	if openMeetings && anyIn(ws, meetingCues) {
		sentence := wordsIn(sentenceAround(text, m[0], m[1]))
		if !anyIn(sentence, isolationWords) && (anyIn(sentence, publicPlaces) || groupMeeting(sentence) ||
			anyIn(ws, visitCues) && errand(wordsIn(sentenceAfter(text, m[1])))) {
			return false
		}
	}
	switch {
	case is(last, "know") && (is(first, "do") || is(first, "does")):
		// "do your parents know about the fee hike?" asks about a thing;
		// "do your parents know about us", "know you talk to strangers"
		// and "do your parents know?" are about the poster and the reader.
		about := nextWords(clauseAfter(text, m[1]), 3)
		return !(len(about) > 1 && is(about[0], "about") && !personalAbout[about[1]] &&
			!(len(about) > 2 && personalAbout[about[2]]))
	case len(ws) == 1 && is(first, "privately") || len(ws) == 2 && is(first, "in") && is(last, "private"):
		// Offered by the poster to the reader, not "ask the prof privately".
		sentence := wordsIn(sentenceAround(text, m[0], m[1]))
		return anyIn(sentence, offerWords) && anyIn(sentence, secondPerson)
	case is(last, "waiting"):
		// "I'll be waiting", not "I'll be waiting for your reply".
		return !(nextIn(0, setOf(`for`)) && len(next) > 1 && !secondPerson[next[1]] && !is(next[1], "you"))
	case is(first, "call") && is(last, "me"):
		return !nextIn(0, callMeIdioms)
	case hasWord(ws, "pay") && (is(last, "you") || is(last, "u")):
		// "I'll pay you", not "I'll pay you back".
		return !nextIn(0, setOf(`back`))
	case bareDMs[first] && !is(last, "me") && unicode.IsLetter(rune(text[m[0]])):
		// "girls 13-15 dm", not "I asked in dm" or "check your dms".
		return !(len(prev) > 0 && dmLeads[prev[len(prev)-1]])
	case hasWord(ws, "out") && (is(first, "go") || is(first, "going") || is(first, "goes")):
		// "do your parents let you go out alone", not "is it safe for
		// girls to go out alone".
		return anyIn(wordsIn(sentenceAround(text, m[0], m[1])), secondPerson)
	case is(first, "just") && is(last, "us"):
		// "let's be friends, just us", "we could hang out just us", not
		// "everyone or just us".
		return len(prev) == 0 || justUsLeads[prev[len(prev)-1]]
	case is(first, "just"):
		// "it's not just you."
		return !(len(prev) > 0 && canonSetOf(`not isnt isn't aint`)[prev[len(prev)-1]])
	case is(last, "meet") && len(ws) > 1:
		// "we should meet the deadline", not "let's meet this weekend".
		return !anyIn(nextWords(clauseAfter(text, m[1]), 3), meetObjects)
	case is(last, "alone") && (is(first, "are") || is(first, "r")):
		return !(nextIn(0, setOf(`in`)) && len(next) > 1 && strings.HasSuffix(next[1], "ing") ||
			nextIn(0, setOf(`on`)) && nextIn(1, setOf(`this that it`)))
	case is(last, "secret") && !is(first, "keep"):
		return !nextIn(0, secretGoesOn)
	case is(first, "between"):
		// "between you and me" ends what it says; as the idiom it opens a
		// sentence that goes on. "between us" is a secret only said "just"
		// or "stays" so: "just between us", "this stays between us", not
		// "the gap between us".
		if is(last, "us") && !(len(prev) > 0 && betweenUsLeads[prev[len(prev)-1]]) {
			return false
		}
		for _, w := range wordsIn(sentenceAfter(text, m[1])) {
			if !secretTail[w] {
				return false
			}
		}
	case is(first, "baat"):
		// "X se baat karo" is "talk to X"; only "mere se" is "to me".
		if n := len(prev); n > 0 && seWords[prev[n-1]] {
			return n > 1 && firstPersonSe[prev[n-2]]
		}
	case is(first, "milte") || is(first, "milna"):
		return !(len(prev) > 0 && hindiIn[prev[len(prev)-1]])
	case is(first, "tell") && len(ws) > 1 && (is(ws[1], "no") || is(ws[1], "noone") || is(ws[1], "nobody")),
		is(first, "no") && hasWord(ws, "tell"):
		// "tell no one" or "no need to tell your parents" told to the
		// reader and ending what is said, or said about the two of them:
		// not "I tell nobody my marks", "no need to tell anyone your rank".
		return !anyIn(prev, tellers) && secretEnds(wordsIn(sentenceAfter(text, m[1])))
	case is(last, "up") && hasWord(ws, "come"):
		// "I'll come up tonight", not "I'll come up with something".
		return !nextIn(0, setOf(`with`))
	case hasWord(ws, "walk") && !hasWord(ws, "home"):
		// "I could walk with you", not "I'll walk you through it".
		return !nextIn(0, setOf(`through thru`))
	case len(ws) == 1 && is(first, "alone"):
		// "alone?" opening what is said asks it; "alone" after a comma at
		// the end of an order to come is "come alone" said last: "come to
		// the chai stall after class, alone".
		at := m[0] + strings.IndexFunc(text[m[0]:m[1]], unicode.IsLetter)
		if len(wordsIn(clauseBefore(text, at))) > 0 {
			return false
		}
		if strings.Contains(text[at:m[1]], "?") {
			return true
		}
		sentence := wordsIn(sentenceAround(text, at, at))
		for _, w := range sentence {
			if !orderFillers[w] {
				return comeWords[w] && !hasWord(sentence, "think")
			}
		}
		return false
	case pictureWords[first]:
		for _, w := range prev {
			if !picLeads[w] {
				return false
			}
		}
	case pictureWords[last] && canModals[first]:
		// "can i see a pic", not "can i see a pic of the question".
		return !(nextIn(0, setOf(`of`)) && len(next) > 1 && !secondPerson[next[1]] && !is(next[1], "you") &&
			!is(next[1], "yourself") && !is(next[1], "urself"))
	}
	return true
}

// secretEnds reports the words after a secret kept as nothing more than a
// tag ("ok", "x") or what the secret is about when that is the poster and
// the reader ("about this", "about us").
func secretEnds(ws []string) bool {
	for i, w := range ws {
		if !secretTail[w] && !kissTags[w] && !(i == 0 && is(w, "about") && len(ws) > 1) && !(i > 0 && secretAbout[w]) {
			return false
		}
	}
	return true
}

// groupMeeting reports a sentence that makes a meeting a group's: "group
// study", "our team is meeting at 5", "everyone's coming".
func groupMeeting(ws []string) bool {
	for i, w := range ws {
		if groupNouns[w] || is(w, "group") && (i+1 < len(ws) && is(ws[i+1], "study") || i > 0 && is(ws[i-1], "study")) {
			return true
		}
	}
	return false
}

// errand reports a visit's errand in the words after it: "with the notes",
// "to return your charger", "to drop off the book".
func errand(ws []string) bool {
	for i, w := range ws {
		if (is(w, "with") || errandVerbs[w] && i > 0 && is(ws[i-1], "to")) &&
			anyIn(ws[i+1:min(len(ws), i+4)], errandObjects) {
			return true
		}
	}
	return false
}

var (
	// schoolYearNouns name a school year before its number: "grade 11".
	schoolYearNouns = canonSetOf(`grade class year std standard form sem semester`)
	// meetingCues are the words of a meet-up or a visit cue, and visitCues
	// the ones of a visit.
	meetingCues = canonSetOf(`meet meeting meetup wait waiting hang hangout come visit drop walk`)
	visitCues   = canonSetOf(`come visit drop`)
	// publicPlaces are where a meeting is in front of everyone: a library,
	// a canteen, a fest, a class.
	publicPlaces = canonSetOf(`library libraries mess canteen cafeteria cafe caf foodcourt fest auditorium classroom
		lab labs seminar lecture lectures practice tutorial`)
	// groupNouns make a meeting a group's.
	groupNouns = canonSetOf(`team teams squad gang everyone everybody everyone's batchmates classmates others`)
	// isolationWords ask for the reader alone, which no place or group
	// excuses.
	isolationWords = canonSetOf(`alone without w/o just nobody noone secret private privately yourself urself own`)
	errandVerbs    = canonSetOf(`return give drop collect lend borrow submit bring pass hand`)
	errandObjects  = canonSetOf(`notes note book books charger cable assignment assignments project projects file files
		keys key laptop calculator copy copies register bag umbrella card form forms textbook textbooks pendrive
		headphones earphones jacket bottle tiffin`)
	// personalAbout, after "do your parents know about", make it about the
	// poster or the reader.
	personalAbout = canonSetOf(`us me this it that him her them our my you u ur your chat chats messages dms texts
		talking`)
)

var (
	// offerWords are the poster offering something: "I can", "let's".
	offerWords   = canonSetOf(`i i'm im i'll ill i'd me my we we'll let's lets us`)
	callMeIdioms = canonSetOf(`crazy weird old old-fashioned oldfashioned a an stupid dumb lazy naive biased
		paranoid cynical petty picky extra whatever anything names what mad insane nuts sentimental selfish
		boring pessimistic optimistic skeptical sceptical cringe ishmael out mr ms miss sir maam`)
	meetObjects = canonSetOf(`deadline deadlines target targets requirement requirements criteria expectation
		expectations standard standards goal goals quota demand demands need needs cutoff cutoffs eligibility`)
	secretGoesOn = canonSetOf(`that to of for behind garden ingredient recipe sauce weapon santa service agent
		door room passage level code menu technique trick tip tips hack formula why how about`)
	secretTail = canonSetOf(`ok okay k kk right yeah yes ya alright pls please plz only just now na haan`)
	// betweenUsLeads make "between us" a secret: "just between us", "this
	// stays between us".
	betweenUsLeads = canonSetOf(`just only strictly stays stay remains remain`)
	kissTags       = setOf(`x xx xxx xo`)
	secretAbout    = canonSetOf(`us this it that me our chat chats messages dms texts talking meeting`)
	// tellers are the poster or someone else saying who they tell, not
	// telling the reader.
	tellers = canonSetOf(`i i'll ill i'd i'm im we we'll they he she`)
	// orderFillers may open an order before its verb; comeWords are the
	// verbs of an order to come.
	orderFillers  = canonSetOf(`just pls please plz so then and ok okay now but also bas`)
	comeWords     = canonSetOf(`come cum meet aa aaja aao aajao`)
	seWords       = setOf(`se say sey`)
	firstPersonSe = setOf(`mere mujh hum humare hamare mera meri apne`)
	hindiIn       = setOf(`mein me main mai`)
	pictureWords  = canonSetOf(`pic pics picture pictures photo photos selfie selfies`)
	// bareDMs are a bare "dm" at the end of a post, and dmLeads make it
	// where something was said or seen, not a cue: "I asked in dm",
	// "check your dms".
	bareDMs = canonSetOf(`dm dms pm`)
	// justUsLeads may come before a "just us" that ends a sentence in its
	// clause: "we could hang out just us".
	justUsLeads = canonSetOf(`out over alone together meet chat talk hang private privately tonight today tomorrow
		weekend sometime`)
	dmLeads = canonSetOf(`in via by over through on into the my your ur his her their our a check checked sent
		send slide slid open opened see saw read`)
	picLeads = canonSetOf(`any ur your yo a send share got have u you ya can i see get more some pls plz please
		hey hi so ok and face body full mirror cute new recent latest ek apni apna teri tumhari koi achhi achi acchi
		nayi naya`)
)

// aslCheck takes "asl" where it opens what is said, with only a greeting or
// "what's your" before it in its clause, and is not followed by a word that
// makes it the language ("ASL club", "ASL is a good elective").
func aslCheck(v *view, m []int) bool {
	if v.reported(m) {
		return false
	}
	for _, w := range wordsIn(clauseBefore(v.text, m[0])) {
		if !aslLeads[w] {
			return false
		}
	}
	next := nextWords(clauseAfter(v.text, m[1]), 1)
	return len(next) == 0 || !aslNouns[next[0]]
}

var (
	aslLeads = setOf(`hi hey hello helo hii heyy yo so ok okay um umm hmm pls plz please bro sis and ur your u ya
		what what's whats wats tell me send drop give quick first`)
	aslNouns = setOf(`club clubs class classes course courses interpreter interpreters sign signs signing teacher
		teachers lesson lessons elective is was are alphabet grammar students student community dictionary app
		translation translator video videos tutor tutorial certificate exam test level`)
)

// ageCheck turns down "what's your area" when it is a field of study: "what's
// your area of interest", "your location preference".
func ageCheck(v *view, m []int) bool {
	if v.reported(m) {
		return false
	}
	ws := wordsIn(v.text[m[0]:m[1]])
	if len(ws) == 0 {
		return true
	}
	next := nextWords(clauseAfter(v.text, m[1]), 1)
	switch {
	case len(ws) > 1 && is(ws[0], "what") && is(ws[1], "age"):
		// "what age are you supposed to start", "if anyone asks what age
		// are you": an age in general, or a question reported.
		before := wordsBefore(v.text, m[0], 1)
		return !(len(next) > 0 && generalAgeNext[next[0]] || len(before) > 0 && askedWords[before[0]])
	case len(ws) > 1 && is(ws[1], "kya") && hinglishAge[ws[0]]:
		// "umar kya hai tumhari", not "uski age kya hai" or "jee ke liye age
		// kya hai".
		for _, w := range wordsIn(clauseBefore(v.text, m[0])) {
			if !hinglishAgeLeads[w] {
				return false
			}
		}
		return len(next) == 0 || hinglishAgeTails[next[0]]
	case len(ws) > 1 && youthNouns[ws[0]] && isASCIIDigit(ws[1][0]):
		// "girls 13-16 dm me", "girls 14+ here?", not "girls 13-16 category
		// results" or "the girls 14+ team".
		before := wordsIn(clauseBefore(v.text, m[0]))
		return len(next) == 0 || seekingTails[next[0]] && !(len(before) > 0 && groupLeads[before[len(before)-1]])
	case len(ws) > 2 && (schoolYearNouns[ws[0]] || ordinalWord.MatchString(ws[0])) && youthWord(ws[len(ws)-1]):
		// With no "any", a school year's youths, more than one, are looked
		// for only with something asked of them after: "class 8 girls dm
		// me", "9th graders hmu", not "class 8 girls relay results are out",
		// "the class 8 girls dm me every day", "grade 9 student here, dm me"
		// or "grade 9 student. message me".
		if !strings.HasSuffix(ws[len(ws)-1], "s") {
			return false
		}
		before := wordsIn(clauseBefore(v.text, m[0]))
		lead := ""
		if len(before) > 0 {
			lead = before[len(before)-1]
		}
		return len(next) > 0 && seekingTails[next[0]] && !groupLeads[lead] && !articles[lead]
	}
	if readerStatement[strings.TrimRight(ws[0], "'")] || is(ws[0], "you") && len(ws) > 1 && is(ws[1], "are") ||
		is(ws[0], "u") && len(ws) > 1 && is(ws[1], "r") {
		// "when you're in 12th you'll get it" says when, not who; "you're
		// 15 minutes late" is no age.
		if before := wordsBefore(v.text, m[0], 3); len(before) > 0 && (whenWords[before[0]] ||
			len(before) > 2 && is(before[0], "time") && is(before[1], "the") && is(before[2], "by")) {
			return false
		}
		next := nextWords(v.text[m[1]:min(len(v.text), m[1]+32)], 1)
		if len(next) > 0 && ageUnits[next[0]] || strings.HasPrefix(v.text[m[1]:], "%") {
			return false
		}
	}
	// "if someone asks how old you are": a question reported, not asked.
	// Only its own clause: "how old are you? are you alone?" asks. "how old
	// are you supposed to be to go out alone" asks an age in general.
	if is(ws[0], "how") && !is(ws[len(ws)-1], "old") {
		next := nextWords(clauseAfter(v.text, m[1]), 1)
		before := wordsBefore(v.text, m[0], 1)
		if len(next) > 0 && (is(next[0], "are") || generalOldNext[next[0]]) ||
			len(before) > 0 && askedWords[before[0]] {
			return false
		}
	}
	// "u 14? the competition is for 14 and above": an age asked for a rule
	// that is all in the open.
	if strings.ContainsAny(v.text[m[0]:m[1]], "0123456789") && ageRuleAfter(v.text[m[1]:min(len(v.text), m[1]+80)]) {
		return false
	}
	if !is(ws[len(ws)-1], "area") && !is(ws[len(ws)-1], "location") {
		return true
	}
	next = nextWords(v.text[m[1]:min(len(v.text), m[1]+32)], 1)
	return len(next) == 0 || !areaTopics[next[0]]
}

// youthWord reports one of youthNouns, or a grader: "girls", "9th graders".
func youthWord(w string) bool { return youthNouns[w] || is(w, "grader") || is(w, "graders") }

// ageRuleAfter reports an age limit stated in s: "for 14 and above", "14+
// only", "under 15 only", "the age limit is 16", "eligible". An adult's age
// in it ("this server is 18+", "the age limit is 18") is a reason to keep the
// reader out, not to ask them in, so it is none.
func ageRuleAfter(s string) bool {
	plus := false
	for i := 0; i < len(s); i++ {
		if !isASCIIDigit(s[i]) {
			continue
		}
		j := i
		for j < len(s) && isASCIIDigit(s[j]) {
			j++
		}
		if n, err := strconv.Atoi(s[i:j]); err == nil && n >= 18 && j-i <= 2 {
			return false
		}
		plus = plus || j < len(s) && s[j] == '+'
		i = j
	}
	if plus {
		return true
	}
	ws := wordsIn(s)
	for i, w := range ws {
		switch {
		case ageRuleWords[w]:
			return true
		case (is(w, "and") || is(w, "or")) && i+1 < len(ws) && ageBounds[ws[i+1]]:
			return true
		case is(w, "age") && i+1 < len(ws) && ageLimits[ws[i+1]]:
			return true
		}
	}
	return false
}

var (
	ageRuleWords = canonSetOf(`eligible eligibility`)
	ageBounds    = canonSetOf(`above over up older under below younger`)
	ageLimits    = canonSetOf(`limit limits group groups category categories criteria bracket cutoff`)
)

var (
	// readerStatement opens a statement about the reader, and whenWords
	// make it a time: "once you're in college".
	readerStatement = canonSetOf(`you're youre u're ur`)
	whenWords       = canonSetOf(`if when whenever once until till til whether as after before since unless while`)
	ageUnits        = canonSetOf(`minutes minute mins min hours hour hrs hr days day weeks week months month seconds secs
		percent marks mark points point rank km kms kg kgs times x`)
	areaTopics = canonSetOf(`of preference preferences expertise interest`)
	askedWords = canonSetOf(`asks asked ask asking know knows wonder wondering tell tells told says said`)
	// generalAgeNext, after "what age are you", make it an age in general:
	// "what age are you supposed to start", "what age are you allowed to
	// drive".
	generalAgeNext = canonSetOf(`supposed meant allowed expected required likely when if usually generally typically
		normally gonna going able eligible considered legally once until till before after`)
	// generalOldNext, after "how old are you", make it an age in general:
	// "how old are you allowed to be to go out alone". Fewer than
	// generalAgeNext: "how old are you going to be" asks the reader's.
	generalOldNext = canonSetOf(`supposed meant allowed expected required eligible considered legally usually
		typically generally normally`)
	// hinglishAge is the age asked in "umar kya hai"; hinglishAgeLeads may
	// come before it in its clause and hinglishAgeTails after it, so it is
	// the reader's.
	hinglishAge      = canonSetOf(`umar umr age`)
	hinglishAgeLeads = canonSetOf(`tumhari tumhaari teri aapki apki bhai yaar yar sis di didi beta baby babu hey hi
		hello hii toh to waise btw aur and so acha accha achha ok okay ur your`)
	hinglishAgeTails = canonSetOf(`tumhari tumhaari teri aapki apki bhai yaar yar beta baby babu btw na abhi exactly
		sach sachi pls plz please batao bata bolo`)
	// youthNouns are youthNoun's words; seekingTails may follow an age
	// they are looked for at ("girls 13-16 dm me", "girls 14+ here?"),
	// and groupLeads make it a group's ("the girls 14+ team").
	youthNouns = canonSetOf(`girl girls boy boys guy guys kid kids gf bf girlfriend boyfriend female females
		male males teen teens student students`)
	seekingTails = canonSetOf(`here only pls plz please needed wanted dm dms pm inbox msg message text hmu snap add
		wanna want interested single online around near nearby who u you any hit reply ping call chat lets let's
		looking`)
	groupLeads = canonSetOf(`the our their his her my these those with vs versus and both all for of in`)
)

// canonSetOf is setOf for words looked up in the loose view as well.
func canonSetOf(list string) map[string]bool {
	m := setOf(list)
	for w := range setOf(list) {
		m[canon(w)] = true
	}
	return m
}

func setOf(list string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(list) {
		m[w] = true
	}
	return m
}

/* Matching -------------------------------------------------------------------- */

// matchCheck vets one match: the view it was found in and the submatch
// indexes into the view's text. It lets a table of patterns stay readable
// while a few small validators make the decisions a regular expression cannot.
type matchCheck func(v *view, m []int) bool

// checkWork is what a check is counted as costing besides the match itself:
// what most checks read round a match, a clause or a sentence each side, cut
// to 64 bytes (clauseAfter, sentenceAround). The searches a check runs over
// those count their own work. Some checks read further, in windows of a
// fixed width that this does not count: quotationAround 240 back on its line,
// to count the straight quotes before a mark; leavingTheBoard 200 bytes each
// side; deathAround, quotationAround, closingMark, quotesTheReply and the "end
// it" readings in selfHarmCheck 120; appFrame's readOn 96; imagineNot and
// ageRuleAfter 80; reported 64 round a quotation and before the words, as
// well as quotationAround; menace's hints readReach words each way;
// gameNamed 64 before the sentence; and privateCue the sentence after a
// visit as well as the one round it. Each is a constant per check, so the
// count stays linear in the post, but a counted step stands for more time
// in a body whose matches take those checks (TestFilterCostIsBounded says
// how much).
const checkWork = 2 * 64

// checked puts match m to check, counting it in the view's work.
func (v *view) checked(check matchCheck, m []int) bool {
	v.work += checkWork + m[1] - m[0]
	return check(v, m)
}

// viewSearch is one view of a field and the search a rule has for it.
type viewSearch struct {
	v  *view
	re *search
}

// views returns the field's views paired with the search a rule has for each,
// leaving out the plain view when the loose one stands for it.
func (f *scannedField) views(r rule) []viewSearch {
	if r.loose == nil {
		return []viewSearch{{&f.plain, r.plain}}
	}
	if f.looseOnly {
		return []viewSearch{{&f.loose, r.loose}}
	}
	return []viewSearch{{&f.plain, r.plain}, {&f.loose, r.loose}}
}

// first returns the source span of the earliest match of r, in either view,
// that check accepts.
func (f *scannedField) first(r rule, check matchCheck) (int, int, bool) {
	from, to, found := 0, 0, false
	for _, vs := range f.views(r) {
		vs.re.each(vs.v, func(m []int) bool {
			if m[1] <= m[0] || check != nil && !vs.v.checked(check, m) {
				// The first match is the leftmost one, so with no check
				// there is nothing more to find in this view.
				return check != nil
			}
			if a, b := vs.v.span(m[0], m[1]); !found || a < from {
				from, to, found = a, b, true
			}
			return false
		})
	}
	return from, to, found
}

// firstHandle is first for a rule that ends at a handle (handleRules, and
// the mail rules written like them), whose groups are an "@", the handle and
// a Discord discriminator. A handle glued to an opening bracket is a call in
// code ("ig = ig_client(token)"), which the byte after it tells, so it is
// turned down there, counted as the match read, before check, which is
// counted as reading a clause each side: in a post made of such calls ("@s_(ig
// " over and over, TestFilterCostIsBounded) every rule and view finds every
// one, and each would otherwise be charged a clause it never reads.
func (f *scannedField) firstHandle(r rule, check matchCheck) (int, int, bool) {
	from, to, found := 0, 0, false
	for _, vs := range f.views(r) {
		vs.re.each(vs.v, func(m []int) bool {
			if m[1] <= m[0] {
				return true
			}
			if gluedToBracket(vs.v, m) {
				vs.v.work += m[1] - m[0]
				return true
			}
			if !vs.v.checked(check, m) {
				return true
			}
			if a, b := vs.v.span(m[0], m[1]); !found || a < from {
				from, to, found = a, b, true
			}
			return false
		})
	}
	return from, to, found
}

// gluedToBracket reports a handle rule's handle with an opening bracket
// straight after it.
func gluedToBracket(v *view, m []int) bool { return strings.HasPrefix(v.text[m[5]:], "(") }

// anyMatch reports whether any of rules matches in either view.
func (f *scannedField) anyMatch(rules []rule, check matchCheck) bool {
	for _, r := range rules {
		if check == nil {
			for _, vs := range f.views(r) {
				found := false
				vs.re.each(vs.v, func([]int) bool { found = true; return false })
				if found {
					return true
				}
			}
			continue
		}
		if _, _, ok := f.first(r, check); ok {
			return true
		}
	}
	return false
}

// spans returns the source spans of every match of rules, in either view,
// that check accepts, ordered by where they start.
func (f *scannedField) spans(rules []rule, check matchCheck) [][2]int {
	var out [][2]int
	for _, r := range rules {
		for _, vs := range f.views(r) {
			vs.re.each(vs.v, func(m []int) bool {
				if m[1] > m[0] && (check == nil || vs.v.checked(check, m)) {
					a, b := vs.v.span(m[0], m[1])
					out = append(out, [2]int{a, b})
				}
				return true
			})
		}
	}
	sortSpans(out)
	return out
}

// work is what searching the field has cost so far, in the steps
// searchIndex counts.
func (f *scannedField) work() int {
	n := 0
	for _, v := range []*view{&f.plain, &f.loose} {
		n += v.work
		for _, ix := range v.index {
			if ix != nil {
				n += ix.work
			}
		}
	}
	return n
}

/* Threats by implication ------------------------------------------------------ */

// A threat can be told with no act in it: by what will be left after it ("he's
// leaving in an ambulance", "nobody walks out of the gym alive", "gonna make
// the news tomorrow"), by what the poster has ready ("bringing what i need",
// "I've got enough for every single one of them", "there will be a device
// under one of the desks"), by a weapon named with no verb ("the boys and the
// iron rods"), by a verdict ("he's done", "it's happening", "they gon learn",
// "you have until friday"), by a warning to stay away ("don't come", "leave
// early") or that it is one ("this is a warning", "remember i said it first",
// "you know who you are"), or by the wrong it answers ("the teacher who
// failed me", "everybody who ever said a word about me"). Each of them is said
// innocently every day, so menace holds a post only for hints of two kinds
// close together and close to a place or a time (threatPlaceOrTimeRules), or
// for one of the few hints that are a threat by themselves (strong): someone
// leaving in an ambulance, nobody walking out of a place alive, enough for
// every single one of them, a device hidden under the desks, a list of names
// kept for no reason given. A grievance with only a warning that it is one is
// a callout, not a threat: "whoever took my charger, you know who you are". A
// hint in a story, a dream or a game, in a sentence about exams or results
// (schoolHyperbole), said of something that happens again and again, said
// with the poster as its subject where the hint is about someone else, or
// reported as someone else's words, is no hint.

// menaceKind is a kind of hint that a post threatens.
type menaceKind uint8

const (
	menaceAfter     menaceKind = 1 << iota // what will be left: a hospital bed, nobody alive, the news
	menaceReady                            // what the poster has ready: what I need, a list, a device
	menaceArmed                            // a weapon, or a crew with a club, named with no verb
	menaceVerdict                          // a sentence passed: he's done, they'll learn, you have until friday
	menaceStayAway                         // a warning to stay away: don't come, stay inside, leave early
	menaceWarned                           // a warning that it is one: this is a warning, mark my words
	menaceGrievance                        // the wrong it answers: who failed me, said a word about me

	numMenaceKinds = iota
)

// menacing reports that hints of kinds, read together, threaten: two kinds,
// unless they are only a grievance and a warning that it is one.
func menacing(kinds menaceKind) bool {
	return bits.OnesCount8(uint8(kinds)) >= 2 && kinds != menaceGrievance|menaceWarned
}

// menaceHint is a hint found in a field, placed in the post in
// readingPositions.
type menaceHint struct {
	kind   menaceKind
	strong bool
	// placed is set for a hint whose sentence names a place or a time in
	// words threatPlaceOrTimeRules leave to the grammar: a "form room", "his
	// weekend".
	placed bool
	at     [2]int
}

// menace reports a threat told by implication: hints that are menacing
// together, or one strong one, within threatWindow of each other and of a
// place or a time. The places and times are asked for only when the hints
// call for them.
func menace(fields []*scannedField, placesAndTimes func() nearSpans) bool {
	var hs []menaceHint
	for _, f := range fields {
		hs = append(hs, f.menaceHints()...)
	}
	slices.SortFunc(hs, func(a, b menaceHint) int { return a.at[0] - b.at[0] })
	// A hint is near another when each starts within threatWindow of the
	// other's end. The hints of each kind, and the placed ones, are kept in
	// order of where they start, with the furthest any of them up to each
	// reaches, so whether one of a kind is near a hint is a binary search,
	// not a look at every hint near it: a post of 4,000 spaced gun emoji,
	// which all stand at one reading position, made that a look at every
	// pair, and none of it was counted (TestFilterCostIsBounded, "spaced
	// guns"). Filing each hint is counted as a step for each kind, and each
	// search as a step for each halving it takes, as the search takes it, so
	// the count is the work done and not an estimate of it
	// (TestFilterCountsMenacePairing).
	steps := 0
	if len(fields) > 0 {
		defer func() { fields[0].plain.work += steps }()
	}
	var near [numMenaceKinds + 1]struct{ starts, reach []int }
	for _, h := range hs {
		steps += numMenaceKinds + 1
		for k := range near {
			if k < numMenaceKinds && h.kind&(1<<k) == 0 || k == numMenaceKinds && !h.placed {
				continue
			}
			n := &near[k]
			reach := h.at[1]
			if len(n.reach) > 0 {
				reach = max(reach, n.reach[len(n.reach)-1])
			}
			n.starts, n.reach = append(n.starts, h.at[0]), append(n.reach, reach)
		}
	}
	// nearby finds the first hint of kind k that starts past h's reach, by
	// halving, and asks whether any before it reaches back to h.
	nearby := func(k int, h menaceHint) bool {
		n := &near[k]
		lo, hi := 0, len(n.starts)
		for lo < hi {
			steps++
			if mid := int(uint(lo+hi) >> 1); n.starts[mid] <= h.at[1]+threatWindow {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		steps++
		return lo > 0 && n.reach[lo-1] >= h.at[0]-threatWindow
	}
	for _, h := range hs {
		kinds := h.kind
		for k := range numMenaceKinds {
			if nearby(k, h) {
				kinds |= 1 << k
			}
		}
		if (h.strong || menacing(kinds)) && (h.placed || nearby(numMenaceKinds, h) ||
			placesAndTimes().near(h.at, threatWindow)) {
			return true
		}
	}
	return false
}

// menaceHints reads the field's plain view for hints of a threat: the
// phrasings of menacePhrases, each read from its first word, and a weapon
// drawn as an emoji.
func (f *scannedField) menaceHints() []menaceHint {
	v := &f.plain
	r := f.wordReading()
	pos := f.readingPositions()
	var out []menaceHint
	add := func(kind menaceKind, strong, placed bool, a, b int) {
		if v.reported([]int{a, b}) {
			return
		}
		s, e := v.span(a, b)
		out = append(out, menaceHint{kind, strong, placed, [2]int{int(pos[s]) + f.offset, int(pos[e]) + f.offset}})
	}
	for i, w := range r.ws {
		for _, p := range menacePhrasesAt[w] {
			// A run is a few words compared; a match is read in windows
			// of a few dozen words round it, as a check reads its clause.
			v.work += len(p.run)
			hi := p.run.match(r.ws, i, r.sentenceEnd[i])
			if hi < 0 || p.final && !r.endsSentence(hi) {
				continue
			}
			v.work += checkWork
			strong, ok := p.strong, true
			if p.check != nil {
				var checked bool
				checked, ok = p.check(r, i, hi)
				strong = strong || checked
			}
			if ok && !r.excused(p, i) {
				ws := r.sentenceWords(i)
				placed := anyIn(ws, placeHeads) || anyIn(ws, vehicleHeads) || anyIn(ws, menaceTimes) ||
					anyIn(ws, menacePlaces)
				add(p.kind, strong, placed, r.sp[i][0], r.sp[hi-1][1])
			}
		}
	}
	// A run of weapon emoji is one hint, read from its first: the rest
	// stand at the same reading position, so they would add the same hint,
	// each read again for what is round it (TestFilterCostIsBounded,
	// "knives").
	for k, run := 0, false; k < len(v.text); {
		c, size := utf8.DecodeRuneInString(v.text[k:])
		if weaponEmoji[c] && !run {
			v.work += checkWork
			ws := wordsIn(sentenceAround(v.text, k, k+size))
			if !unnegatedIn(ws, gameWords, toyWeapon, festivalWords) && !storyNamed(ws) {
				add(menaceArmed, false, false, k, k+size)
			}
		}
		run = weaponEmoji[c]
		k += size
	}
	return out
}

// menaceTimes are the times a hint may be put at that are no time of
// threatTimes: "his weekend", "before term ends".
var (
	menaceTimes = canonSetOf(`weekend weekends term semester holidays break vacation night morning evening
		afternoon lunch recess period`)
	// menacePlaces are places a hint may be put at that are no place of
	// threatPlaces: the school stage, which a band sets on fire, and places
	// as Hinglish writes them short: "clg".
	menacePlaces = canonSetOf(`stage clg kolej skool schl`)
)

// weaponEmoji are the weapons drawn as emoji that arm a hint.
var weaponEmoji = map[rune]bool{'🔫': true, '🔪': true, '💣': true, '🧨': true, '🗡': true, '🪓': true}

// wordReading is a text split into words, with apostrophes that open or
// close a quotation taken off them, and where each word's sentence and
// clause start and end. What it reads round a word is cut to readReach
// words each way, and what it reads of the whole text is read once
// (postHas), so reading a hint costs the same in a post of any length.
type wordReading struct {
	text                   string
	sp                     [][2]int
	ws                     []string
	sentence, sentenceEnd  []int
	clauseStart, clauseEnd []int
	has                    map[string]bool
}

// readReach is how many words a hint is read for round it.
const readReach = 24

// postHas reports a word of set anywhere in the text, read once for each
// name.
func (r *wordReading) postHas(name string, set map[string]bool) bool {
	if r.has == nil {
		r.has = map[string]bool{}
	}
	if found, ok := r.has[name]; ok {
		return found
	}
	r.has[name] = anyIn(r.ws, set)
	return r.has[name]
}

// postHasPair reports a word of set followed by second anywhere in the text,
// read once for each name: "break up".
func (r *wordReading) postHasPair(name string, set map[string]bool, second string) bool {
	if r.has == nil {
		r.has = map[string]bool{}
	}
	if found, ok := r.has[name]; ok {
		return found
	}
	found := false
	for k := 0; k+1 < len(r.ws) && !found; k++ {
		found = set[r.ws[k]] && is(r.ws[k+1], second)
	}
	r.has[name] = found
	return found
}

// wordReading reads the plain view's words once, for menace and goads.
func (f *scannedField) wordReading() *wordReading {
	if f.words == nil {
		f.plain.work += len(f.plain.text)
		v := &f.plain
		f.words = newWordReading(v.text, func(i int) bool {
			r, _ := utf8.DecodeRuneInString(f.src[v.start[i]:v.end[i]])
			return clauseDashes[r]
		})
	}
	return f.words
}

// clauseDashes are the dashes that set off a clause, which the plain view
// reads as "-" like a hyphen: "you know who you are—don't come tomorrow".
var clauseDashes = map[rune]bool{'—': true, '–': true, '―': true, '⸺': true, '⸻': true}

// newWordReading reads the words of text, a plain view, with their sentences
// and clauses. clauseDash reports that the "-" at text[i] was written as a
// dash that sets off a clause, which a hyphen is not.
func newWordReading(text string, clauseDash func(i int) bool) *wordReading {
	r := &wordReading{text: text}
	for _, s := range wordSpans(text) {
		for s[0] < s[1] && text[s[0]] == '\'' {
			s[0]++
		}
		for s[1] > s[0] && text[s[1]-1] == '\'' {
			s[1]--
		}
		if s[0] < s[1] {
			r.sp = append(r.sp, s)
			r.ws = append(r.ws, text[s[0]:s[1]])
		}
	}
	n := len(r.ws)
	r.sentence, r.sentenceEnd = make([]int, n), make([]int, n)
	r.clauseStart, r.clauseEnd = make([]int, n), make([]int, n)
	for k := range r.ws {
		r.sentence[k], r.clauseStart[k] = k, k
		if k == 0 {
			continue
		}
		from := r.sp[k-1][1]
		gap := text[from:r.sp[k][0]]
		if !strings.ContainsAny(gap, ".!?\n") {
			r.sentence[k] = r.sentence[k-1]
			if !strings.ContainsAny(gap, ",;:") && !strings.Contains(gap, " - ") && !strings.Contains(gap, "--") &&
				!dashIn(gap, from, clauseDash) {
				r.clauseStart[k] = r.clauseStart[k-1]
			}
		}
	}
	for k := n - 1; k >= 0; k-- {
		r.sentenceEnd[k], r.clauseEnd[k] = n, n
		if k+1 < n {
			if r.sentence[k+1] == r.sentence[k] {
				r.sentenceEnd[k] = r.sentenceEnd[k+1]
			} else {
				r.sentenceEnd[k] = k + 1
			}
			if r.clauseStart[k+1] == r.clauseStart[k] {
				r.clauseEnd[k] = r.clauseEnd[k+1]
			} else {
				r.clauseEnd[k] = k + 1
			}
		}
	}
	return r
}

// dashIn reports a "-" in gap, which starts at from in the text, that
// clauseDash says sets off a clause.
func dashIn(gap string, from int, clauseDash func(i int) bool) bool {
	for j := 0; j < len(gap); j++ {
		if gap[j] == '-' && clauseDash != nil && clauseDash(from+j) {
			return true
		}
	}
	return false
}

// endsSentence reports that nothing but fillers follows word i in its
// sentence: "he's done.", "he's done lol".
func (r *wordReading) endsSentence(i int) bool {
	if i == 0 {
		return true
	}
	if r.sentenceEnd[i-1]-i > 3 {
		return false
	}
	for k := i; k < r.sentenceEnd[i-1]; k++ {
		if !trailFillers[r.ws[k]] && !laughs[r.ws[k]] {
			return false
		}
	}
	return true
}

// sentenceWords are the words of the sentence word i is in.
func (r *wordReading) sentenceWords(i int) []string {
	return r.ws[max(r.sentence[i], i-readReach):min(r.sentenceEnd[i], i+readReach)]
}

// clauseFrom is where word i's clause starts, or readReach words back.
func (r *wordReading) clauseFrom(i int) int { return max(r.clauseStart[i], i-readReach) }

// restOfClause is up to n words after a phrase that ends at word hi, in the
// clause the phrase ends in. A phrase is matched over its sentence, so it
// can end in a later clause than it starts in: "don't - come to school".
func (r *wordReading) restOfClause(hi, n int) []string {
	return r.ws[hi:min(r.clauseEnd[hi-1], hi+n)]
}

// subjectBefore is the word before i, in its clause, that heads what it is
// said of: the first going back past auxiliaries and adverbs ("he'll
// definitely be"), or "" at the clause's start.
func (r *wordReading) subjectBefore(i int) string {
	for k := i - 1; k >= r.clauseFrom(i); k-- {
		if !auxWords[r.ws[k]] {
			return r.ws[k]
		}
	}
	return ""
}

// excused reports a hint of phrase p starting at word i that its sentence
// makes no hint: a story, a dream or a game; something habitual; exams or
// results for what will be left of people after, or a verdict; and the
// poster as the subject of what is said of someone else ("I'll be in
// hospital", "I'm done", "I don't go to the gym").
func (r *wordReading) excused(p *menacePhrase, i int) bool {
	ws := r.sentenceWords(i)
	if storyNamed(ws) || anyIn(ws, dreamWords) || gameIn(ws) || r.likened(i) {
		return true
	}
	switch p.kind {
	case menaceAfter, menaceVerdict:
		if p.ruin && explicitHarm(ws) {
			return false // "I'm going to light up the school, they'll never know it was me"
		}
		if !p.ruin && schoolHyperbole(ws) || untimed(ws, eventWords) {
			return true
		}
	case menaceStayAway:
	default:
		return false
	}
	return habitual(ws) || posterSubject[r.subjectBefore(i)]
}

// likened reports a hint at word i that its sentence makes a likeness:
// "staring at me as if I'd brought a gun", "aise ghoor raha tha jaise main
// bomb le ke aaya hoon".
func (r *wordReading) likened(i int) bool {
	before := r.ws[max(r.sentence[i], i-readReach):i]
	for k, w := range before {
		next := ""
		if k+1 < len(before) {
			next = before[k+1]
		}
		// "jaise hi" is "as soon as".
		if likenWords[w] && !is(next, "hi") || is(w, "as") && (is(next, "if") || is(next, "though")) {
			return true
		}
	}
	return false
}

// likenWords make what follows them a likeness.
var likenWords = canonSetOf(`jaise jese jaisa jaisi`)

// gameIn reports a game in ws, none of them denied ("this is not a game").
func gameIn(ws []string) bool {
	for k, w := range ws {
		if gameWords[w] && !notAGame(ws, k) {
			return !anyIn(ws[max(0, k-3):k], excuseNegators)
		}
	}
	return false
}

// gamesIn reports a word of gameWords in ws that names a game.
func gamesIn(ws []string) bool {
	for k, w := range ws {
		if gameWords[w] && !notAGame(ws, k) {
			return true
		}
	}
	return false
}

// notAGame reports that the word of gameWords at k of ws names no game: a
// "final warning", a "final chance", "finals week", "final exams", or a
// school's "sports day", which is a day at school.
func notAGame(ws []string, k int) bool {
	next := ""
	if k+1 < len(ws) {
		next = ws[k+1]
	}
	return (is(ws[k], "final") || is(ws[k], "finals")) && (lastThings[next] || examPeriod[next]) ||
		is(ws[k], "sports") && is(next, "day")
}

// notAsked reads words that are not a question: "he's done." and not "is he
// done?".
func notAsked(r *wordReading, lo, hi int) (strong, ok bool) {
	end := r.sentenceEnd[lo]
	rest := r.text[r.sp[end-1][1]:]
	if end < len(r.sp) {
		rest = r.text[r.sp[end-1][1]:r.sp[end][0]]
	}
	return false, !strings.Contains(rest, "?")
}

var (
	// auxWords stand between a subject and what is said of it.
	auxWords = canonSetOf(`will 'll wil shall is are am was were be been being 's 're gonna gunna finna going to
		gotta got have has had do does did not never just really definitely def prob probably literally actually
		all so then also still finally soon even ever totally officially better`)
	// posterSubject are the poster as a subject.
	posterSubject = canonSetOf(`i i'm im i'll i've ive i'd we we're we'll we've us`)
)

// menacePhrase is a hint's phrasing: a run of words, which must end its
// sentence when final, and a check of what is round it, which says too
// whether it is strong.
type menacePhrase struct {
	kind   menaceKind
	strong bool
	final  bool
	// ruin is set for what is left of a place, which exams named as a
	// time do not make hyperbole: "burnt to the ground before exams".
	ruin  bool
	run   wordRun
	check func(r *wordReading, lo, hi int) (strong, ok bool)
}

// wordRun is a run of words: each step one of a few words ("will|'ll"),
// "?" after a step making it optional, and "_n" standing for up to n words
// of any kind. A step of the form "$name" is the words of runWords[name].
type wordRun []runStep

type runStep struct {
	set map[string]bool // nil for a gap
	opt bool
	gap int
}

func runOf(src string) wordRun {
	var run wordRun
	for _, t := range strings.Fields(runBreak.ReplaceAllString(src, "|")) {
		var s runStep
		if strings.HasSuffix(t, "?") {
			s.opt, t = true, strings.TrimSuffix(t, "?")
		}
		if t[0] == '_' {
			s.gap = int(t[1] - '0')
			run = append(run, s)
			continue
		}
		s.set = map[string]bool{}
		for _, w := range strings.Split(t, "|") {
			if strings.HasPrefix(w, "$") {
				for x := range runWords[w[1:]] {
					s.set[x] = true
				}
				continue
			}
			s.set[w] = true
		}
		run = append(run, s)
	}
	return run
}

// runBreak is a run's line broken after a "|", which joins the words on
// either side.
var runBreak = regexp.MustCompile(`\|\s+`)

// match reports where the run ends when it starts at ws[i] and goes no
// further than ws[end-1], or -1. A gap takes as few words as it can.
func (p wordRun) match(ws []string, i, end int) int {
	if len(p) == 0 {
		return i
	}
	s := p[0]
	if s.set == nil {
		for k := 0; k <= s.gap && i+k <= end; k++ {
			if j := p[1:].match(ws, i+k, end); j >= 0 {
				return j
			}
		}
		return -1
	}
	if i < end && s.set[ws[i]] {
		if j := p[1:].match(ws, i+1, end); j >= 0 {
			return j
		}
	}
	if s.opt {
		return p[1:].match(ws, i, end)
	}
	return -1
}

// runWords are the sets a wordRun may name with "$".
var runWords = map[string]map[string]bool{
	// Someone else as a subject, and with "is" or "are" joined on.
	"they":   setOf(`he she they you u ya y'all yall everyone everybody`),
	"theyre": setOf(`he's hes she's shes they're theyre you're youre ur u're`),
	"theyll": setOf(`he'll she'll they'll you'll u'll theyll youll`),
	"will":   setOf(`will 'll wil gonna gunna finna shall gon`),
	"have":   setOf(`got have has 've had bringing bring brought bought carrying carry packed packing grabbed`),
	"people": setOf(`them you u y'all yall everyone everybody every1 him her`),
	"wrong": setOf(`fail failed failing report reported reporting kick kicked kicking bully bullied bullying
		hit hitting punch punched punching slap slapped slapping insult insulted insulting dump dumped dumping
		snitch snitched snitching complain complained complaining laugh laughed laughing mock mocked mocking
		humiliate humiliated humiliating embarrass embarrassed embarrassing betray betrayed betraying rat ratted
		ratting expose exposed exposing beat beating flog flogged flogging rag ragged ragging cheat cheated
		cheating lie lied lying reject rejected rejecting expel expelled expelling suspend suspended suspending
		disrespect disrespected disrespecting push pushed pushing shove shoved trip tripped tease teased teasing
		troll trolled trolling roast roasted diss dissed shame shamed blame blamed frame framed accuse accused
		harass harassed harassing threaten threatened abuse abused attack attacked jump jumped spit spat rob
		robbed scam scammed wrong wronged screw screwed`),
	"damage": setOf(`key keyed keying steal stole stolen stealing nick nicked broke break breaking smash smashed
		scratch scratched ruin ruined ruining wreck wrecked trash trashed burn burnt burned take took taken`),
}

// menacePhrases are the hints' phrasings, and menacePhrasesAt the same
// listed by the words that can start them.
var (
	menacePhrases = []menacePhrase{
		// What will be left after: an ambulance, a hospital, a body bag.
		{kind: menaceAfter, run: runOf(`$will|be|end|ends|ending|leave|leaves|leaving|wake|wakes|land|lands|go|goes|
			going|come|comes|coming|up|out _2 in|into|on an|a|the|their|his|her|ur|your? hospital|icu|ambulance|
			ambulances|morgue|coffin|coffins|stretcher|stretchers|wheelchair|bodybag|bodybags|body`), check: leftIn},
		{kind: menaceAfter, run: runOf(`nobody|noone|nobody's|nobodys|none|no|not`), check: nobodyLeaves},
		{kind: menaceAfter, run: runOf(`won't|wont|never|not make|making|reach|get it? to|home|there|back|out`),
			check: notMakingIt},
		{kind: menaceAfter, run: runOf(`bloodbath|massacre|carnage|bodycount|warzone`), check: notPast},
		{kind: menaceAfter, run: runOf(`body count`), check: notPast},
		{kind: menaceAfter, run: runOf(`make|makes|making|be|hit|hits _2 the news|headlines|headline|papers`),
			check: newsOfIt},
		{kind: menaceAfter, run: runOf(`news|akhbaar|akhbar|tv mein|me|pe dekhenge|dekhoge|dekhna|aayega|ayega|
			aayegi|ayegi|aaega|hoga|dikhega|chhapega|chapega|aaenge`)},
		{kind: menaceAfter, run: runOf(`won't|wont|will|there'll|isn't|isnt|ain't|aint|gonna _1 be any|no|nothing|
			nobody|noone|anyone|anything _3 left`), check: nothingLeft},
		{kind: menaceAfter, final: true, run: runOf(`his|her|their|your|ur|its last one|day|week|weekend|breath|
			class|lesson|ride|trip|chance`), check: notALeaving},
		{kind: menaceAfter, run: runOf(`last time it|they|he|she|anyone|you|u|we|this|that _2 ever`),
			check: lastCrowd},
		{kind: menaceAfter, run: runOf(`carry|carrying|carried|carries|drag|dragging|dragged people|bodies|them|
			kids|students|everyone|everybody|ppl|someone|somebody _1 out`), check: notFainting},
		{kind: menaceAfter, run: runOf(`see|seeing who's|whos|who|whoever is|'s? still? standing|alive|breathing|
			left`)},
		{kind: menaceAfter, run: runOf(`$will|is|are|'s|'re|gonna|going to? be|get|getting hurt|harmed|injured|
			jumped|stabbed|shot|beaten|bashed|attacked|knifed|killed|murdered`), check: harmComing},
		{kind: menaceAfter, ruin: true, run: runOf(`burnt|burned|burn|burning|razed|torched to the ground|ashes`)},
		{kind: menaceAfter, ruin: true, run: runOf(`light|lighting|lit|set|setting it|them|everything|the|this|that
			place? up|alight|ablaze`), check: lightingUp},
		{kind: menaceReady, run: runOf(`lock|locking|locked|chain|chaining|block|blocking the|all|every|them|everyone
			_1 in? from? the? outside`)},
		{kind: menaceReady, run: runOf(`lock|locking|chain|chaining|trap|trapping|barricade|barricading`),
			check: lockedIn},
		{kind: menaceAfter, run: runOf(`with him|her|them|everyone|everybody|people|you|u still? in|inside it`)},
		{kind: menaceAfter, run: runOf(`ghar _1 nahi|nhi|na _1 pahuchega|pahunchega|pahuchegi|pahunchegi|pohchega|
			pohochega|pahuchenge|pahunchenge|lautega|lautegi`)},
		{kind: menaceAfter, run: runOf(`zinda nahi|nhi bachega|bachegi|bachenge|jayega|jaayega|rahega|lautega`)},

		// What the poster has ready.
		{kind: menaceReady, final: true, run: runOf(`$have|ready|sorted|arranged what|everything|all i|we
			need|needed`)},
		{kind: menaceReady, final: true, run: runOf(`$have|ready|sorted|arranged what|everything|all i|we
			need|needed in|inside my|the bag|backpack|locker|car|boot|trunk|pocket|room`)},
		{kind: menaceReady, run: runOf(`$have enough for every|each|all|everyone|everybody|every1`),
			check: enoughForAll},
		{kind: menaceReady, run: runOf(`list`), check: listOfNames},
		{kind: menaceReady, run: runOf(`there'll|there|will|i've|i|i'll|ive|we've|we'll|we _2 leave|left|leaving|
			plant|planted|planting|hide|hid|hidden|put|placed|place|placing|be a|the|some|my device|devices|
			package|parcel|something|thing under|beneath|inside|behind|in`), check: hiddenThing},
		{kind: menaceReady, run: runOf(`putting|put|slip|slipping|slipped|spike|spiking|adding|add|mix|mixing|
			drop|dropping|lace|lacing something|stuff|smth|sth|it in|into the|their|his|her|ur|your|everyone's|
			everyones? _2 tea|coffee|food|water|drink|drinks|lunch|juice|bottle|bottles|tiffin|chai|dinner|meal|
			cooler|biryani|lunchbox|snacks|cake|soup|dal|curry`)},
		{kind: menaceReady, final: true, run: runOf(`made|make|making sure of it|that`)},
		{kind: menaceReady, run: runOf(`i'll|i|ill see to it|that`)},
		{kind: menaceReady, run: runOf(`been|i've|ive|was|am|i'm|im planning|plotting|preparing it|this|that
			since|for|all`)},
		{kind: menaceReady, run: runOf(`i'm|im|i am? the one doing|behind it|this`)},
		{kind: menaceReady, run: runOf(`built|building|made|making something|smth in|at|over`)},
		{kind: menaceReady, run: runOf(`found|find|know|knows|figured out? where|which he|she|they|you|u|ur|his|her|
			their _2 live|lives|stay|stays|staying|living|sleeps|parks|room|hostel|house|class|flat`)},
		{kind: menaceReady, run: runOf(`know|knows which|what hostel|room|class|house|flat|floor|bus|batch
			you're|youre|ur|you|u|he's|hes|she's|shes|he|she|they're|they _1 in|at|on|take|takes`)},

		// A weapon, or a club and a crew.
		{kind: menaceArmed, run: runOf(`gun|guns|knife|knives|blade|blades|machete|machetes|sword|swords|axe|
			dagger|rifle|rifles|pistol|pistols|revolver|shotgun|explosive|explosives|grenade|grenades|taser|
			firearm|firearms|ak47|ar15|glock|katta|tamancha|cutlass|cutlasses|ammo|bullets|petrol|kerosene|
			gasoline`), check: armedWith},
		{kind: menaceArmed, run: runOf(`full|extended|loaded mag|mags|clip|magazine`), check: armedWith},
		{kind: menaceArmed, run: runOf(`with|bringing|the|my|a the|my|a? strap|straps`), check: armedWith},
		{kind: menaceArmed, run: runOf(`something|smth worse|sharper|deadlier|heavier than`)},
		{kind: menaceArmed, run: runOf(`bat|bats|stick|sticks|rod|rods|hammer|crowbar|pipe|pipes|brick|bricks|
			chain|chains`), check: clubAndCrew},

		// A verdict.
		{kind: menaceVerdict, final: true, run: runOf(`$theyre|$they|that is|are|'s|r? so|really|officially|
			actually|literally|totally? done|finished|toast|history|deadmeat`), check: notAsked},
		{kind: menaceVerdict, final: true, run: runOf(`$they|$theyll $will? learn|pay`)},
		{kind: menaceVerdict, run: runOf(`$they|$theyll $will|are|is|r? regret|regrets`)},
		{kind: menaceVerdict, final: true, run: runOf(`it's|its|it|this|shit's is? happening`)},
		{kind: menaceVerdict, final: true, run: runOf(`it's|its|it|this|shit's is? going down`)},
		{kind: menaceVerdict, final: true, run: runOf(`is|are|'s|r so|really|officially|actually|literally|totally?
			done|finished|toast|history|deadmeat`), check: personBefore},
		{kind: menaceVerdict, final: true, run: runOf(`$theyll|will|gon|gonna finally? see|learn|understand`)},
		{kind: menaceVerdict, run: runOf(`get|gets|getting|got what's|whats coming`)},
		{kind: menaceVerdict, run: runOf(`it|this|it'll|this'll all? ends|end|ending today|tomorrow|tonight|here|
			now|on|this|monday|tuesday|wednesday|thursday|friday|saturday|sunday`)},
		{kind: menaceVerdict, run: runOf(`find|finds|found|finding|see|sees|learn|learns|show|showing|teach out?
			what happens`)},
		{kind: menaceVerdict, run: runOf(`what happens when|if you|u|people|anyone|someone|they|ppl mess|messes|
			cross|crosses|snitch|snitches|step|steps|play|plays|disrespect|disrespects|test|tests`)},
		{kind: menaceVerdict, run: runOf(`$they|you've|youve|they've|theyve|he's|she's have|has|got|'ve? got?
			until|till|til|untill`), check: ultimatum},
		{kind: menaceVerdict, run: runOf(`i'm|im|imma|we're|i'll|we'll|ill be? coming|come for|after you|u|them|
			him|her|every|all|each|everyone|everybody|y'all|yall|ya`), check: comingFor},
		{kind: menaceVerdict, run: runOf(`say|kiss|wave your|ur? goodbye|goodbyes|bye to? them|him|her`)},
		{kind: menaceVerdict, strong: true, run: runOf(`sabko|sabki|unko|usko|tujhe|tumko|sab|un|us|tum _1
			uda|udaa dunga|denge|dungi|daalunga|dalunga`), check: hindiTarget},
		{kind: menaceArmed, strong: true, run: runOf(`katta|kattaa|tamancha|bandook|banduk|pistol|gun|chaku|chakku|
			chhura|talwar|knife|petrol|bomb leke|lekar|leker|le`), check: armedWith},
		{kind: menaceVerdict, run: runOf(`nahi|nhi|na chhodunga|chodunga|chhodenge|chodenge|chhodungi|chodungi|
			bakshunga|bakhshunga|bakshenge`)},
		{kind: menaceVerdict, run: runOf(`sabak|sabaq sikhaunga|sikhayenge|sikhaenge|sikha|sikhana`)},
		{kind: menaceVerdict, run: runOf(`dekh lunga|lenge|lungi`)},
		{kind: menaceVerdict, final: true, run: runOf(`go|goin collect`)},
		{kind: menaceVerdict, run: runOf(`go|goin hear|see|feel am`)},
		{kind: menaceVerdict, run: runOf(`better|betta watch|pray|run|hide|enjoy|sleep|count|kiss|make|say`),
			check: someoneElseBetter},
		{kind: menaceVerdict, run: runOf(`gets|get|getting it`), check: getsIt},
		{kind: menaceVerdict, run: runOf(`i'll|ill|imma|we'll|i|we will|gonna|'ll? find you|u|him|her|them`)},
		{kind: menaceVerdict, run: runOf(`won't|wont|can't|cant|nothing|nobody|noone will|can? help|save|protect
			you|u|them|him|her`)},

		// A warning to stay away.
		{kind: menaceStayAway, run: runOf(`don't|dont|never|shouldn't|shouldnt|not come|go|show|turn|be|step|
			bother|attend|enter`), check: stayAway},
		{kind: menaceStayAway, run: runOf(`stay|staying|keep home|inside|indoors|away|clear|out`), check: stayAway},
		{kind: menaceStayAway, run: runOf(`leave|get out? early|now|before|while`), check: stayAway},
		{kind: menaceStayAway, run: runOf(`skip|bunk|miss school|class|college|assembly|it|tomorrow|today`),
			check: stayAway},
		{kind: menaceStayAway, run: runOf(`avoid|steer _1 the|that`), check: stayAway},

		// A warning that it is one.
		{kind: menaceWarned, run: runOf(`this|that's|thats|it's|its|here's is? a|my|your|ur|the? last|final|only|first|fair|
			friendly? warning`)},
		{kind: menaceWarned, run: runOf(`last|final warning`)},
		{kind: menaceWarned, run: runOf(`been warned`)},
		{kind: menaceWarned, run: runOf(`consider|considered yourself|yourselves|urself|this|it warned`)},
		{kind: menaceWarned, run: runOf(`didn't|didnt|never warn|warned`)},
		{kind: menaceWarned, run: runOf(`i'm|im warning you|u|everyone|y'all|yall|ya`)},
		{kind: menaceWarned, run: runOf(`not|no a? joke|joking|kidding|kiddin`)},
		{kind: menaceWarned, run: runOf(`remember i|we said|asked|told|warned|called|predicted`)},
		{kind: menaceWarned, run: runOf(`i|we warned you|u|them|y'all|yall|ya|everyone`)},
		{kind: menaceWarned, run: runOf(`mark my words`)},
		{kind: menaceWarned, run: runOf(`you|u know who|what you|u|ur|you're|youre are|did?`)},
		{kind: menaceWarned, run: runOf(`that's|thats all i'm|im|i'll|ill saying|say`)},
		{kind: menaceWarned, run: runOf(`asked|told you? nicely|politely`)},
		{kind: menaceWarned, run: runOf(`swear down|to|on god|my|everything? life|mum|mom|mother?`)},
		{kind: menaceWarned, run: runOf(`yaad rakhna|rakhiyo|rakhiye`)},
		{kind: menaceWarned, run: runOf(`dekhna tum|sab|sabko|ab`)},
		{kind: menaceWarned, run: runOf(`last chance`), check: notAnOffer},

		// The wrong it answers.
		{kind: menaceGrievance, run: runOf(`$wrong at|about|on|off|out|over? me|us`), check: wrongedBy},
		{kind: menaceGrievance, run: runOf(`$damage my|our`), check: wrongedBy},
		{kind: menaceGrievance, run: runOf(`said|says|saying|say|talked|talking|talk|talks|spread|spreading|
			posted|posting|wrote|telling|told _2 about|behind me|us|my`), check: wrongedBy},
		{kind: menaceGrievance, run: runOf(`made|make|makes|making me|us cry|suffer`), check: wrongedBy},
		{kind: menaceGrievance, run: runOf(`mess|messes|messed|messing|cross|crossed|crossing with? me|us`),
			check: wrongedBy},
	}
	menacePhrasesAt = func() map[string][]*menacePhrase {
		at := map[string][]*menacePhrase{}
		for k := range menacePhrases {
			p := &menacePhrases[k]
			for w := range p.run[0].set {
				at[w] = append(at[w], p)
			}
		}
		return at
	}()
)

// harmComing reads harm said to be coming to someone, as a hint: not on a
// condition ("if you run in the corridor") and not by a cause named after
// it ("hurt by the news").
func harmComing(r *wordReading, lo, hi int) (strong, ok bool) {
	if harmlessCondition(strings.Join(r.sentenceWords(lo), " ")) || hi < r.sentenceEnd[lo] && is(r.ws[hi], "by") {
		return false, false
	}
	return false, !anyIn(r.sentenceWords(lo), hurtNotThreat)
}

// lightingUp reads lighting a place up as setting it alight: not a team's
// or a performer's doing it brilliantly ("our team will light the place up
// at sports day"), and not lamps or a festival ("light it up with diyas").
func lightingUp(r *wordReading, lo, hi int) (strong, ok bool) {
	from, to := max(r.sentence[lo], lo-readReach), min(r.sentenceEnd[lo], lo+readReach)
	if explicitHarm(r.ws[from:to]) {
		return true, true // "light up the gym at the pep rally so nobody gets out"
	}
	// The phrase's own "light" is no lamp.
	return false, !anyIn(r.ws[from:lo], litUp) && !anyIn(r.ws[min(hi, to):to], litUp)
}

// lockedIn reads people locked or trapped in a place: "locking them all in
// the hall", "lock everyone inside the gym during assembly". It is strong
// for a crowd ("everyone", "them all", "all the teachers") locked in by the
// poster or by nobody named, and no hint at all for a lockdown or a drill,
// an exam, a talk, a party, an event or a game ("for the group project",
// "for the treasure hunt", an escape room), a time it ends ("until they
// finish the mock", "with my 3 hour presentation") or a purpose of study
// or attendance ("so they actually study", "so nobody bunks"), though "so
// they can't escape" is none, and none of those is an excuse in a sentence
// that sets something alight ("for the event and setting it on fire",
// alightIn); something locked away ("my bike in the shed") and "us" ("the
// teacher locked us all in the hall") are not read.
func lockedIn(r *wordReading, lo, hi int) (strong, ok bool) {
	end := r.sentenceEnd[lo]
	np := readNoun(r.ws[hi:end])
	k := hi + np.n
	if !np.people(false) || k >= end || !lockedInWords[r.ws[k]] {
		return false, false
	}
	if p := readNoun(r.ws[k+1 : end]); !p.place() && !(k+1 < end && menacePlaces[r.ws[k+1]]) {
		return false, false
	}
	if ws := r.sentenceWords(lo); !alightIn(ws) && !explicitHarm(ws) &&
		(anyIn(ws, lockedInExcuses) || escapeRoom(ws) || lockedToStudy(ws) || lockedFor(ws)) {
		return false, false
	}
	s := r.subjectBefore(lo)
	return np.quantified && (s == "" || posterSubject[s]), true
}

// lockedFor reports a purpose of study or a school occasion after "for":
// "for the test", "for revision", "for the class photo", "for the 24h study
// marathon".
func lockedFor(ws []string) bool {
	for i, w := range ws {
		if !is(w, "for") {
			continue
		}
		for _, x := range ws[i+1 : min(len(ws), i+5)] {
			if lockedPurposes[x] {
				return true
			}
		}
	}
	return false
}

// explicitHarm reports harm named outright in ws, which no occasion or
// purpose excuses: a weapon, people shot, stabbed, gassed or killed
// ("and shoot them", "the experiment will kill them all"), nobody getting
// out or escaping, the poster not being found out ("they'll never know it
// was me"), or a lesson taught ("so they learn their lesson").
func explicitHarm(ws []string) bool {
	if anyIn(ws, weaponHeads) {
		return true
	}
	for i, w := range ws {
		rest := ws[i+1 : min(len(ws), i+4)]
		switch {
		case harmVerbs[w] && anyIn(rest[:min(len(rest), 2)], harmObjects):
			return true
		case (is(w, "nobody") || is(w, "noone") || is(w, "nobody's") || is(w, "no") && len(rest) > 0 &&
			is(rest[0], "one")) && anyIn(rest, escapeWords):
			return true
		case cantWords[w] && len(rest) > 0 && (escapeWords[rest[0]] || len(rest) > 1 && is(rest[0], "get") &&
			is(rest[1], "out")):
			return true
		case is(w, "never") && len(rest) > 0 && is(rest[0], "know"):
			return true
		case is(w, "learn") && len(rest) > 1 && (is(rest[0], "their") || is(rest[0], "a")) && is(rest[1], "lesson"):
			return true
		}
	}
	return false
}

var (
	lockedPurposes = canonSetOf(`test tests practical practicals revision revising meeting meetings photo photos
		picture pictures fair marathon session sessions study studying lesson lessons review quiz quizzes class
		classes extra rehearsal assembly`)
	harmVerbs = canonSetOf(`shoot shooting shot stab stabbing stabbed gas gassing gassed kill killing murder
		murdering slaughter slaughtering burn burning poison poisoning`)
	harmObjects = canonSetOf(`them everyone everybody all him her you y'all yall people us`)
	escapeWords = canonSetOf(`out escape escapes escaping leave leaves leaving survive survives surviving alive`)
	cantWords   = canonSetOf(`can't cant cannot won't wont never`)
)

// escapeRoom reports an escape room in ws, which locks people in for fun.
func escapeRoom(ws []string) bool {
	for i, w := range ws[:max(0, len(ws)-1)] {
		if is(w, "escape") && (is(ws[i+1], "room") || is(ws[i+1], "rooms")) {
			return true
		}
	}
	return false
}

// alightIn reports something set alight in ws: a fuel (fuelWords), a fire
// set ("on fire", "set fire", "setting fire"), "ablaze", "alight" or a
// burning: "lock them all in the gym for the event and set it on fire".
//
// "Burnt out" is tiredness, and someone being "on fire" ("the team is on
// fire", "we're on fire") doing well.
func alightIn(ws []string) bool {
	for i, w := range ws {
		switch {
		case fuelWords[w]:
			return true
		case alightWords[w]:
			if i+1 < len(ws) && is(ws[i+1], "out") {
				continue
			}
			return true
		case is(w, "fire") && i > 0 && setVerbs[ws[i-1]]:
			return true
		case is(w, "fire") && i > 0 && is(ws[i-1], "on"):
			if onFireIdiom(ws[:i-1]) {
				continue
			}
			return true
		}
	}
	return false
}

// onFireIdiom reports that the words before "on fire" make it someone
// doing well: "the team is", "we're", "i'm".
func onFireIdiom(before []string) bool {
	if len(before) == 0 {
		return false
	}
	last := before[len(before)-1]
	if onFireSubjects[last] {
		return true
	}
	return len(before) > 1 && (is(last, "is") || is(last, "are") || is(last, "am")) &&
		onFirePeople[before[len(before)-2]]
}

var (
	onFireSubjects = canonSetOf(`we're were i'm im he's hes she's shes they're theyre you're youre ur team's`)
	onFirePeople   = canonSetOf(`team squad we class i everyone he she they you batch group`)
	alightWords    = canonSetOf(`ablaze alight burn burns burning burnt burned`)
	setVerbs       = canonSetOf(`set sets setting`)
)

// lockedToStudy reports a "so" with studying or attending in the four
// words after it: "so they actually study", "so nobody bunks".
func lockedToStudy(ws []string) bool {
	for i, w := range ws {
		if is(w, "so") && anyIn(ws[i+1:min(len(ws), i+5)], studyPurposes) {
			return true
		}
	}
	return false
}

var (
	lockedInWords   = canonSetOf(`in inside into`)
	lockedInExcuses = canonSetOf(`until till til untill unless drill drills lockdown lockdowns evacuation rehearsal
		exam exams sleepover presentation presentations lecture speech party surprise project projects hunt
		orientation rally fest event events workshop`)
	// studyPurposes, after a "so", make locking people in a teacher's or a
	// monitor's doing.
	studyPurposes = canonSetOf(`study studies revise revises learn learns focus focuses concentrate practise practice
		rehearse prepare attend attends bunk bunks skip skips`)
)

// litUp are the words that make lighting a place up a performance or a
// celebration: a stage, lamps, a festival, an event, a game, a team, an
// occasion ("our team will light it up", "light the place up at the
// farewell").
var litUp = func() map[string]bool {
	m := canonSetOf(`team teams squad band choir dancers farewell freshers fresher prom sangeet wedding reunion`)
	for _, set := range []map[string]bool{stageWords, lampWords, festivalWords, hypeWords, eventWords, gameWords,
		sportWords} {
		for w := range set {
			m[w] = true
		}
	}
	return m
}()

// hindiTarget reads "sabko uda dunga" with someone to blow up or gun down
// before it, "sab ko" written apart included.
func hindiTarget(r *wordReading, lo, hi int) (strong, ok bool) {
	w := r.ws[lo]
	if is(w, "sab") || is(w, "un") || is(w, "us") || is(w, "tum") {
		return true, lo+1 < hi && is(r.ws[lo+1], "ko")
	}
	return true, true
}

// leftIn reads where someone will be left: in hospital, in an ambulance, in
// a body bag. A stay for an illness, an injury or a birth is not one, and an
// ambulance, a stretcher, a coffin, a morgue or a body bag is strong.
func leftIn(r *wordReading, lo, hi int) (strong, ok bool) {
	last := r.ws[hi-1]
	if is(last, "body") {
		if hi >= len(r.ws) || !is(r.ws[hi], "bag") && !is(r.ws[hi], "bags") {
			return false, false
		}
		last = "bodybag"
	}
	ws := r.sentenceWords(lo)
	return leftInStrong[last], !anyIn(ws, medicalWords) && !anyIn(ws, harmlessCauses)
}

// notAnOffer reads "last chance" as a warning, not the last chance to sign
// up, to buy tickets or to get a seat.
func notAnOffer(r *wordReading, lo, hi int) (strong, ok bool) {
	after := r.restOfClause(hi, 3)
	if len(after) > 1 && (is(after[0], "to") || is(after[0], "for")) && anyIn(after[1:], lastChanceOffers) {
		return false, false
	}
	return false, true
}

var (
	// lastChanceOffers are what there is a last chance to do or to have.
	lastChanceOffers = canonSetOf(`sign register enrol enroll apply submit join book buy order get grab vote enter rsvp
		pay see catch tickets ticket entries entry registration registrations signups signup applications seats
		seat spots places discount sale deal`)
	// harmlessCauses put someone in an ambulance or a hospital without
	// anyone meaning to: the canteen's food, a run in PE, the heat.
	harmlessCauses = canonSetOf(`food foods pizza burger burgers biryani curry spicy chilli chili run running laps lap
		5k 10k marathon beep pacer workout exercise heat heatwave sun`)
)

// nobodyLeaves reads "nobody walks out of the gym alive", "not everyone's
// going home from that hall" and "nobody in our form room is making it to
// the holidays": nobody, or not everyone, said to come out alive, to go
// home, or to make it to a time. Alive, or not going home from somewhere, is
// strong; going home early, sober or tonight is no death.
func nobodyLeaves(r *wordReading, lo, hi int) (strong, ok bool) {
	ws, end := r.ws, min(r.clauseEnd[lo], lo+readReach)
	k := lo + 1
	switch w := ws[lo]; {
	case is(w, "no"):
		if k >= end || !is(ws[k], "one") && !is(ws[k], "one's") {
			return false, false
		}
		k++
	case is(w, "not"):
		if k >= end || !notEveryone[ws[k]] {
			return false, false
		}
		k++
	}
	for ; k < end && k < lo+12; k++ {
		verb := ws[k]
		if !survivalVerbs[verb] {
			continue
		}
		after := ws[k+1 : min(r.sentenceEnd[lo], k+7)]
		for j, a := range after {
			switch {
			case is(a, "alive"):
				return true, true
			case j == 0 && is(a, "home") && homeVerbs[verb]:
				if len(after) > 1 && notForGood[after[1]] {
					return false, false
				}
				return len(after) > 1 && (is(after[1], "from") || is(after[1], "after")) || r.endsSentence(k+2), true
			case j == 0 && is(a, "it") && makingVerbs[verb] && len(after) > 1 && reachWords[after[1]]:
				// Late for a class is no threat: "nobody makes it to
				// the 8am lecture".
				if hasWord(after, "time") || anyIn(after, lateFor) {
					return false, false
				}
				// "nobody in our form room": a group picked out by where
				// it is.
				return anyIn(ws[lo+1:k], placeHeads), true
			}
		}
		return false, false
	}
	return false, false
}

// notMakingIt reads a vehicle or people said not to make it somewhere: "the
// trip bus won't make it to the zoo". In time, on time and a phone are not
// one.
func notMakingIt(r *wordReading, lo, hi int) (strong, ok bool) {
	after := r.restOfClause(hi, 4)
	return false, !hasWord(after, "time") && !anyIn(r.sentenceWords(lo), deviceWords)
}

// notPast reads a bloodbath or a massacre still to come, not one that was,
// and not in the markets.
func notPast(r *wordReading, lo, hi int) (strong, ok bool) {
	before := r.ws[max(r.clauseFrom(lo), lo-3):lo]
	return false, !anyIn(before, pastBe) && !anyIn(r.sentenceWords(lo), marketWords)
}

// newsOfIt reads making the news for something that is not an achievement.
func newsOfIt(r *wordReading, lo, hi int) (strong, ok bool) {
	return false, !anyIn(r.sentenceWords(lo), achievementWords)
}

// nothingLeft reads a place or people said to have none of it left: "won't
// be any school left", not "won't be any pizza left".
func nothingLeft(r *wordReading, lo, hi int) (strong, ok bool) {
	for _, w := range r.ws[lo:hi] {
		if placeHeads[w] || rolePeople[w] {
			return false, true
		}
	}
	return false, false
}

// notALeaving reads someone's last day, weekend or time, or the last time
// something ever happens, where no one is retiring, moving or leaving.
func notALeaving(r *wordReading, lo, hi int) (strong, ok bool) {
	return false, !r.postHas("leaving", leavingWords)
}

// lastCrowd reads "the last time it ever does" when nothing is said to be
// leaving: strong when a place filling with people "will be the last time
// it ever does" ("next time lecture hall 2 fills up for the 9am will be the
// last time it ever does"), unless the post says the place is closing.
func lastCrowd(r *wordReading, lo, hi int) (strong, ok bool) {
	if r.postHas("leaving", leavingWords) {
		return false, false
	}
	willBe := lo >= 2 && is(r.ws[lo-1], "the") && is(r.ws[lo-2], "be")
	ends := hi < r.sentenceEnd[lo] && everDoes[r.ws[hi]] && r.endsSentence(hi+1)
	return willBe && ends && anyIn(r.sentenceWords(lo), crowdWords) && !r.postHas("closing", closingWords), true
}

var (
	// everDoes end "the last time it ever does".
	everDoes = canonSetOf(`does do is are will happens happen fills fill`)
	// crowdWords say a place fills with people.
	crowdWords = canonSetOf(`fills fill filled full packed crowded`)
	// closingWords say a place is closing or coming down.
	closingWords = canonSetOf(`shut shuts shutting closing closes closed close demolish demolished demolishing
		demolition renovate renovating renovation renovated`)
)

// notFainting reads people carried out where nobody fainted from the heat.
func notFainting(r *wordReading, lo, hi int) (strong, ok bool) {
	return false, !r.postHas("faint", faintWords)
}

// enoughForAll reads "I've got enough for every single one of them", with
// nothing named that there is enough of, and nothing a crowd is given
// (supplyWords, foodWords) in the post: strong for every single one, or
// every one of them.
func enoughForAll(r *wordReading, lo, hi int) (strong, ok bool) {
	if r.postHas("supply", supplyWords) || r.postHas("food", foodWords) {
		return false, false
	}
	after := r.restOfClause(hi, 4)
	return hasWord(after, "single") || everyOne[r.ws[hi-1]] && hasWord(after, "one"), true
}

// listOfNames reads a list of names or people the poster has: strong when
// the post gives it no purpose (listPurposes).
func listOfNames(r *wordReading, lo, hi int) (strong, ok bool) {
	before := r.ws[max(r.clauseFrom(lo), lo-2):lo]
	if !anyIn(before, listKeepers) {
		return false, false
	}
	for _, w := range r.ws[hi:min(len(r.ws), hi+10)] {
		if is(w, "names") || is(w, "people") {
			return !r.postHas("purposes", listPurposes), true
		}
	}
	return false, false
}

// hiddenThing reads a device or a package hidden under, inside or behind
// something, which is strong, or "something" put in a place, which is not.
func hiddenThing(r *wordReading, lo, hi int) (strong, ok bool) {
	noun, prep := r.ws[hi-2], r.ws[hi-1]
	if hasWord(r.sentenceWords(lo), "for") {
		return false, false // "a box under each desk for your phones"
	}
	return hiddenDevices[noun] && !is(prep, "in"), hiddenDevices[noun] || is(noun, "something") || is(noun, "thing")
}

// armedWith reads a weapon named in a sentence that names no toy, prop,
// festival, range, camp or study of it.
func armedWith(r *wordReading, lo, hi int) (strong, ok bool) {
	ws := r.sentenceWords(lo)
	return false, !unnegatedIn(ws, toyWeapon, festivalWords, propWords) && !shootingRange(ws) &&
		!anyIn(ws, outdoorWords) && !anyIn(ws, studyWords)
}

// clubAndCrew reads a club named with a crew: "my brother's bat and his
// mates", "the boys and the iron rods", not at a ground or a game.
func clubAndCrew(r *wordReading, lo, hi int) (strong, ok bool) {
	ws := r.sentenceWords(lo)
	return false, anyIn(ws, crewWords) && !anyIn(ws, pitchHeads)
}

// ultimatum reads "you have until friday" with no task to do by then: not
// "you have until friday to submit".
func ultimatum(r *wordReading, lo, hi int) (strong, ok bool) {
	rest := r.restOfClause(hi, 6)
	return false, !hasWord(rest, "to") && !hasWord(rest, "for") && len(rest) <= 5
}

// comingFor reads coming for someone that is not collecting them: strong for
// every one of them or all of them.
func comingFor(r *wordReading, lo, hi int) (strong, ok bool) {
	after := r.ws[hi:min(r.sentenceEnd[lo], hi+6)]
	if anyIn(after, pickUpWords) || anyIn(r.sentenceWords(lo), rankWords) {
		return false, false
	}
	return everyWords[r.ws[hi-1]], true
}

// someoneElseBetter reads "the vice principal better enjoy his weekend" and
// "you better watch your back", not "you better enjoy the holidays".
func someoneElseBetter(r *wordReading, lo, hi int) (strong, ok bool) {
	s := r.subjectBefore(lo)
	if is(s, "you") || is(s, "u") || is(s, "ya") {
		return false, !is(r.ws[hi-1], "enjoy") && !is(r.ws[hi-1], "make") && !is(r.ws[hi-1], "say")
	}
	return false, s != ""
}

// personBefore reads someone said to be done: a person named before it in its
// clause, "the senior who ragged me is done", not "the hostel is done".
func personBefore(r *wordReading, lo, hi int) (strong, ok bool) {
	for _, w := range r.ws[r.clauseFrom(lo):lo] {
		if rolePeople[w] || kinPeople[w] {
			return notAsked(r, lo, hi)
		}
	}
	return false, false
}

// getsIt reads "the school gets it on sports day": at the end of its
// sentence, or with a time straight after.
func getsIt(r *wordReading, lo, hi int) (strong, ok bool) {
	return false, r.endsSentence(hi) || hi < r.sentenceEnd[lo] && (soonWords[r.ws[hi]] || timePrepositions[r.ws[hi]])
}

// stayAway reads a warning to stay away from a place or at a time: said as
// an instruction to someone else, with the place or the time, or ending its
// sentence, and with no reason given for it (stayAwayReasons) in its sentence
// or the ones either side ("the canteen pizza on friday is a crime. stay
// away"), nor a purpose after it ("leave early to catch the bus").
func stayAway(r *wordReading, lo, hi int) (strong, ok bool) {
	ws := r.sentenceWords(lo)
	if anyIn(ws, stayAwayReasons) || r.reasonNextTo(lo) {
		return false, false
	}
	after := r.restOfClause(hi, 8)
	for k := 0; k+1 < len(after); k++ {
		if (is(after[k], "to") || is(after[k], "and")) && stayPurposes[after[k+1]] {
			return false, false
		}
	}
	// Said to someone ("pls don't come", "better stay inside"), not of
	// them ("he never comes to class", "the bus doesn't go there").
	for k := lo - 1; k >= r.clauseFrom(lo); k-- {
		if insistWords[r.ws[k]] {
			break
		}
		if !auxWords[r.ws[k]] {
			return false, false
		}
	}
	if r.endsSentence(hi) {
		return false, true
	}
	for _, w := range r.restOfClause(hi, 6) {
		if placeHeads[w] || soonWords[w] || is(w, "weekend") {
			return false, true
		}
	}
	return false, false
}

// reasonNextTo reports a reason to stay away in the sentence before word i's
// or the one after it: food, a crowd, an exam, a teacher in a mood ("the
// maths teacher is so done with us").
func (r *wordReading) reasonNextTo(i int) bool {
	var near [][]string
	if s := r.sentence[i]; s > 0 {
		near = append(near, r.ws[max(r.sentence[s-1], s-readReach):s])
	}
	if e := r.sentenceEnd[i]; e < len(r.ws) {
		near = append(near, r.ws[e:min(r.sentenceEnd[e], e+readReach)])
	}
	for _, ws := range near {
		if anyIn(ws, stayAwayReasons) || anyIn(ws, teacherWords) && (anyIn(ws, moodWords) || doneWith(ws)) {
			return true
		}
	}
	return false
}

// doneWith reports "done with us" or "done with you" in ws: fed up, not
// finished off.
func doneWith(ws []string) bool {
	for k := 0; k+2 < len(ws); k++ {
		if is(ws[k], "done") && is(ws[k+1], "with") && (is(ws[k+2], "us") || is(ws[k+2], "you") || is(ws[k+2], "u")) {
			return true
		}
	}
	return false
}

// wrongedBy reads a wrong done to the poster by someone else: not "I failed
// my exam", "hit me up" or "posted about my trip", and a person, not a song,
// that made them cry.
func wrongedBy(r *wordReading, lo, hi int) (strong, ok bool) {
	s := r.subjectBefore(lo)
	if posterSubject[s] {
		return false, false
	}
	last := r.ws[hi-1]
	switch first := r.ws[lo]; {
	case is(first, "hit") && hi < len(r.ws) && is(r.ws[hi], "up"):
		return false, false
	case sayingVerbs[first] || is(first, "talked") || is(first, "talking") || is(first, "talk") ||
		is(first, "talks") || is(first, "spread") || is(first, "spreading") || is(first, "posted") ||
		is(first, "posting") || is(first, "wrote") || is(first, "telling") || is(first, "told"):
		if is(last, "my") {
			return false, hi < len(r.ws) && is(r.ws[hi], "back")
		}
	case is(first, "made") || is(first, "make") || is(first, "makes") || is(first, "making"):
		return false, rolePeople[s] || kinPeople[s] || subjectPronouns[s] || relativeWords[s]
	}
	return false, true
}

var (
	leftInStrong = canonSetOf(`ambulance ambulances stretcher stretchers coffin coffins morgue bodybag bodybags`)
	medicalWords = canonSetOf(`sick ill illness fever surgery operation checkup appointment injury injured injuries
		broke broken fractured fracture sprain sprained accident fainted fainting asthma allergy allergic covid
		dengue malaria typhoid jaundice flu infection admitted discharged born birth baby delivery pregnant volunteer
		volunteering internship duty shift nurse doctor doctors treatment chemo therapy recovering recovery appendix
		stitches xray mri vaccine dentist`)
	// notEveryone, after "not", say not all of them: "not everyone's
	// going home".
	notEveryone   = canonSetOf(`everyone everyone's everybody everybody's all every many one`)
	survivalVerbs = canonSetOf(`walk walks walking come comes coming get gets getting make makes making go goes going
		leave leaves leaving survive survives surviving live lives living`)
	homeVerbs   = canonSetOf(`go goes going get gets getting make makes making come comes coming walk walks walking`)
	makingVerbs = canonSetOf(`make makes making`)
	lateFor     = canonSetOf(`lecture lectures class classes lesson lessons school college exam exams paper test
		assembly practical practicals bus train tuition`)
	reachWords = canonSetOf(`to through past out home till until`)
	// notForGood, after "going home", make it an evening's end: "nobody's
	// going home early tonight".
	notForGood = canonSetOf(`early sober yet tonight till until before without for on by with alone happy empty
		hungry tired late`)
	deviceWords = canonSetOf(`phone phones laptop laptops battery charger plant plants marks grade grades attendance
		pen printer wifi internet car bike scooty`)
	pastBe           = canonSetOf(`was were got had been`)
	achievementWords = canonSetOf(`win won winning wins champion champions championship award awards prize trophy
		record rank topper toppers selected selection viral famous trending scholarship medal gold`)
	leavingWords = canonSetOf(`retire retiring retirement retires leaving farewell transfer transferred transferring
		graduating graduation graduate moving shifting resign resigning resigned quit quitting promotion
		vacation`)
	faintWords = canonSetOf(`heat hot heatstroke faint fainted fainting exhausted tired drunk passed asleep sleeping
		dehydrated`)
	supplyWords = canonSetOf(`snacks snack food foods cake cakes sweets sweet chocolates chocolate candy candies
		pizza pizzas samosa samosas biscuits cookies cupcakes chips drinks juice notes copies copy handouts sheets
		tickets ticket passes gifts gift presents chairs seats pens pencils masks money cash ladoo laddoo mithai
		boxes plates cups shirts jerseys badges stickers popcorn sandwiches books`)
	everyOne     = canonSetOf(`every each`)
	listKeepers  = canonSetOf(`got have made making wrote writing keeping keep kept started i've ive 've a my`)
	listPurposes = canonSetOf(`team quiz trip party invite invites invited invitation guest guests rsvp attendance
		register registration signup shortlist shortlisted selection selected merit rank results award awards prize
		vote votes voting election elections nominate nominations cast audition auditions roll reading songs movies
		grocery shopping todo wishlist playlist birthday bday wedding farewell passed toppers scholarship santa fees
		dues submitted submit duty recommend recommendations`)
	hiddenDevices = canonSetOf(`device devices package packages parcel parcels`)
	studyWords    = canonSetOf(`essay history debate project assignment law laws control policy ban banned research
		article documentary museum exhibit class syllabus chapter`)
	crewWords   = canonSetOf(`boys lads mates gang crew squad homies bros boyz goons`)
	pickUpWords = canonSetOf(`pick picking drop dropping lift ride dinner lunch party birthday visit visiting`)
	rankWords   = canonSetOf(`rank ranks ranking leaderboard title crown spot top position record trophy`)
	everyWords  = canonSetOf(`every all each everyone everybody y'all yall`)
	// insistWords may come before an instruction to stay away: "better
	// stay inside", "pls don't come".
	insistWords = canonSetOf(`better should gotta pls please just so then seriously and but i'd id`)
	lastThings  = canonSetOf(`warning warnings chance call notice`)
	// examPeriod, after "finals", make them exams: "finals week".
	examPeriod      = canonSetOf(`week weeks exam exams paper papers`)
	stayAwayReasons = canonSetOf(`sick ill fever unwell cold flu covid virus rain raining rains storm cyclone flood
		floods weather heat heatwave hot snow snowing holiday holidays vacation off closed closure cancelled
		canceled bandh strike protest hartal curfew pollution smog aqi traffic jam crowd crowded queue queues
		rush construction repair repairs maintenance renovation drill fumigation exam exams test results revision
		study sale packed full busy rammed jammed noisy loud smell smells smelly stinks stinking stink food foods
		pizza burger burgers biryani curry lunch menu meal meals mopped slippery wet painted paint pest pests rats
		cockroaches`)
	// stayPurposes, after "to" or "and", give a reason to leave early or
	// stay away: "leave early to catch the bus".
	stayPurposes  = canonSetOf(`catch get grab beat avoid make reach board find save`)
	teacherWords  = canonSetOf(`teacher teachers sir maam ma'am miss principal prof professor coach hod warden`)
	moodWords     = canonSetOf(`mood moody angry mad furious cranky grumpy strict fuming raging annoyed irritated`)
	relativeWords = canonSetOf(`who whoever that wey jo jisne`)
)

// threatens reports a threat: an intent with a place or a time within
// threatWindow of it, a warning with a cue that it is one, or "school
// shooting" with a time still to come close by. The fields are read as one
// text, in form order, because a subject is shown as the title right above
// the body: "School tomorrow" over "I'm going to shoot everyone" is one
// threat. The places and times are found first, once, so that an intent
// with none near it is never read for what it means.
func threatens(fields []*scannedField) bool {
	for i := 1; i < len(fields); i++ {
		prev := fields[i-1]
		fields[i].offset = prev.offset + int(prev.readingPositions()[len(prev.src)]) + 1
	}
	spansOf := func(rules []rule) nearSpans {
		var b [][2]int
		for _, f := range fields {
			b = append(b, f.spansFrom(rules, nil)...)
		}
		return newNearSpans(b)
	}
	var placeOrTime *nearSpans
	placesAndTimes := func() nearSpans {
		if placeOrTime == nil {
			s := spansOf(threatPlaceOrTimeRules)
			placeOrTime = &s
		}
		return *placeOrTime
	}
	pair := func(first []rule, second func() nearSpans, check func(f *scannedField) matchCheck, w int) bool {
		b := second()
		if len(b.s) == 0 {
			return false
		}
		for _, f := range fields {
			// A threat someone else made, reported, is not the poster's.
			c := check(f)
			own := func(v *view, m []int) bool { return !v.reported(m) && (c == nil || c(v, m)) }
			if f.anyNear(first, own, b, w) {
				return true
			}
		}
		return false
	}
	byField := func(check matchCheck) func(*scannedField) matchCheck {
		return func(*scannedField) matchCheck { return check }
	}
	of := func(rules []rule) func() nearSpans { return func() nearSpans { return spansOf(rules) } }
	return pair(threatIntentRules, placesAndTimes, func(f *scannedField) matchCheck { return f.threatCheck }, threatWindow) ||
		pair(namedTargetRules, placesAndTimes, func(f *scannedField) matchCheck { return f.namedTargetCheck }, threatWindow) ||
		pair(threatFrameRules, placesAndTimes, func(f *scannedField) matchCheck { return f.threatFrameCheck }, threatWindow) ||
		pair(threatComingRules, placesAndTimes, func(f *scannedField) matchCheck { return f.threatComingCheck }, threatWindow) ||
		pair(threatUnseenRules, of(threatUnseenRules), func(f *scannedField) matchCheck { return f.threatComingCheck }, 0) ||
		pair(threatSymbolRules, placesAndTimes, func(f *scannedField) matchCheck { return f.threatSymbolCheck }, threatWindow) ||
		pair(threatWarningRules, of(threatWarningCues), byField(nil), threatWindow) ||
		pair(schoolShootingRules, of(shootingTimeRules), byField(schoolShootingCheck), schoolShootingWindow) ||
		menace(fields, placesAndTimes)
}

// nearSpans are spans in reading positions, ordered by where they start,
// with the furthest end any of the first j reaches, so that whether a span
// is near one of them is a binary search.
type nearSpans struct {
	s     [][2]int
	reach []int
}

func newNearSpans(s [][2]int) nearSpans {
	reach := make([]int, len(s))
	for j, x := range s {
		reach[j] = x[1]
		if j > 0 {
			reach[j] = max(reach[j], reach[j-1])
		}
	}
	return nearSpans{s, reach}
}

// near reports a span within w reading positions of x: the first one that
// ends no more than w before x starts, if it starts no more than w after x
// ends. Every span before it ends too early, and every one after it starts
// later still.
func (n nearSpans) near(x [2]int, w int) bool {
	j, _ := slices.BinarySearch(n.reach, x[0]-w)
	return j < len(n.s) && n.s[j][0] <= x[1]+w
}

// anyNear reports a match of rules, in either view, that check accepts and
// that is within w reading positions of one of spans. A match too far from
// all of them is not checked.
func (f *scannedField) anyNear(rules []rule, check matchCheck, spans nearSpans, w int) bool {
	pos := f.readingPositions()
	found := false
	for _, r := range rules {
		for _, vs := range f.views(r) {
			vs.re.each(vs.v, func(m []int) bool {
				if m[1] <= m[0] {
					return true
				}
				a, b := vs.v.span(m[0], m[1])
				if !spans.near([2]int{int(pos[a]) + f.offset, int(pos[b]) + f.offset}, w) {
					return true
				}
				found = check == nil || vs.v.checked(check, m)
				return !found
			})
			if found {
				return true
			}
		}
	}
	return false
}

// schoolShootingWindow is how close, in readingPositions, a time to come has
// to be to "school shooting" for the two to be one statement.
const schoolShootingWindow = 40

// spansFrom is spans placed in the post as a whole, in readingPositions:
// offset by where the field starts when the fields are read one after
// another, in form order, each a line apart, as the page shows them.
func (f *scannedField) spansFrom(rules []rule, check matchCheck) [][2]int {
	spans := f.spans(rules, check)
	pos := f.readingPositions()
	for i := range spans {
		spans[i][0] = int(pos[spans[i][0]]) + f.offset
		spans[i][1] = int(pos[spans[i][1]]) + f.offset
	}
	return spans
}

// readingPositions says, for every byte offset of the source and its end,
// how far into the field a reader is there: one for each letter or digit the
// plain view reads before it, and one for each run of anything else, so that a
// gap counts once whatever fills it. Padding a threat out with spaces,
// invisible characters or emoji does not move its time further from it.
func (f *scannedField) readingPositions() []int32 {
	if f.reading != nil {
		return f.reading
	}
	pos := make([]int32, len(f.src)+1)
	var n int32
	at, gap := 0, false
	text := f.plain.text
	for k := 0; k < len(text); {
		r, size := utf8.DecodeRuneInString(text[k:])
		for s := f.plain.start[k]; at <= s && at < len(pos); at++ {
			pos[at] = n
		}
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			n++
			gap = false
		case !gap:
			n++
			gap = true
		}
		k += size
	}
	for ; at < len(pos); at++ {
		pos[at] = n
	}
	f.reading = pos
	return pos
}

func sortSpans(s [][2]int) {
	slices.SortFunc(s, func(a, b [2]int) int { return a[0] - b[0] })
}

// clauseBefore is the clause that ends at i, within 64 bytes of it, and
// clauseAfter the one that starts there: a clause ends at a sentence's end,
// a line break, or a comma, semicolon or colon.
func clauseBefore(text string, i int) string {
	before := text[max(0, i-64):i]
	return before[strings.LastIndexAny(before, ".!?\n,;:")+1:]
}

func clauseAfter(text string, i int) string {
	after := text[i:min(len(text), i+64)]
	if j := strings.IndexAny(after, ".!?\n,;:"); j >= 0 {
		after = after[:j]
	}
	return after
}

// sentenceAround is the sentence round text[a:b], within 64 bytes either side.
func sentenceAround(text string, a, b int) string {
	before := text[max(0, a-64):a]
	after := text[b:min(len(text), b+64)]
	if j := strings.IndexAny(after, ".!?\n"); j >= 0 {
		after = after[:j]
	}
	return before[strings.LastIndexAny(before, ".!?\n")+1:] + text[a:b] + after
}

// wordsIn splits s into words of letters, digits and apostrophes.
func wordsIn(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
}

// is reports that w is word, as either view writes it: word itself, or
// canon(word), compared a byte at a time rather than built, since the
// checks put every word they read to it ("l" is one byte in UTF-8 and
// never part of another character).
func is(w, word string) bool {
	if w == word {
		return true
	}
	if len(w) != len(word) {
		return false
	}
	for i := 0; i < len(w); i++ {
		c := word[i]
		if c == 'l' {
			c = 'i'
		}
		if w[i] != c {
			return false
		}
	}
	return true
}

func hasWord(ws []string, word string) bool {
	for _, w := range ws {
		if is(w, word) {
			return true
		}
	}
	return false
}

func anyIn(ws []string, set map[string]bool) bool {
	for _, w := range ws {
		if set[w] {
			return true
		}
	}
	return false
}

func hasFirstPerson(ws []string) bool { return anyIn(ws, firstPerson) }

var firstPerson = canonSetOf(`i i'm im i'll i'd i've me my myself mine we we're us`)

// selfCompound reports a match that ends in "self" where the word goes on as
// a compound: "kill your self-doubt", "end your self esteem".
func selfCompound(text string, end int) bool {
	tail := text[max(0, end-4):end]
	if !strings.HasSuffix(tail, "self") && !strings.HasSuffix(tail, canon("self")) {
		return false
	}
	rest := text[end:]
	if strings.HasPrefix(rest, "-") {
		r, _ := utf8.DecodeRuneInString(rest[1:])
		return unicode.IsLetter(r)
	}
	next := nextWords(rest[:min(len(rest), 24)], 1)
	return len(next) == 1 && selfCompounds[next[0]]
}

var selfCompounds = canonSetOf(`doubt doubts esteem confidence worth image portrait portraits control respect
	discipline love care belief awareness sabotage pity consciousness interest defense defence help study
	improvement reflection expression`)

// gameDeath reports a death in a game among the words after a match: "go die
// to the boss", "fastest way to die in minecraft hardcore".
func gameDeath(next []string) bool {
	return anyIn(next[:min(len(next), 5)], gameDeathWords)
}

var gameDeathWords = canonSetOf(`minecraft boss bosses mob mobs lava respawn hardcore fortnite valorant roblox
	pubg bgmi game games ingame level levels raid dungeon creeper creepers zombie zombies nether ender cod
	tutorial checkpoint elden speedrun`)

// supportCheck turns down crisis-language matches that context makes
// something else: "kms" as kilometres or a key service, "live" in a hostel,
// "life" sciences, "cut myself some slack", "die of embarrassment", "end it
// with her", "dying my hair", "suicide prevention" and "self-doubt".
func (f *scannedField) supportCheck(v *view, m []int) bool {
	text := v.text
	ws := wordsIn(text[m[0]:m[1]])
	if len(ws) == 0 {
		return true
	}
	last := ws[len(ws)-1]
	next := wordsIn(clauseAfter(text, m[1]))
	nextIn := func(k int, set map[string]bool) bool { return len(next) > k && set[next[k]] }
	if aboutSomeoneElse(text, m, ws) || quotedNotSaid(v, m, next) || jokingCause(text, m) || shrugged(text, m, ws) ||
		inYourShoesBefore(text, m[0]) {
		return false
	}
	if ok, decided := f.supportPhrasing(text, m, ws, next); decided {
		return ok
	}
	first := ws[0]
	switch {
	case is(first, "if"):
		return counterfactualCheck(text, m, ws, next)
	case is(first, "way") || len(ws) > 1 && is(ws[1], "way"):
		// "the easiest way out is to just not be alive anymore" and "the
		// only way out for me is to end it" are the poster's own; with
		// someone else in the sentence and not the poster, it is theirs
		// ("for him the only way out was death").
		if sentence := wordsIn(sentenceAround(text, m[0], m[1])); !hasFirstPerson(sentence) &&
			(anyIn(sentence, otherPeople) || anyIn(sentence, otherSubjects) || anyIn(sentence, otherObjects)) {
			return false
		}
	case stockpileWords[first]:
		// "saving up pills", not "collecting pill bottles", and only the
		// poster doing it.
		return ownSubject(text, m[0]) && !(len(next) > 0 && pillContainers[next[0]]) && !forAPurpose(next)
	case ownActs[first] || is(last, "again") && (is(first, "cut") || is(first, "cutting")):
		// Hurting oneself without an "I" before it, in any tense: the
		// poster has to be the one doing it, and a bare "self harming" or
		// "cut again" has to be something done, not a topic ("self-harming
		// teens", "cut again by the selectors").
		if !ownSubject(text, m[0]) || !notNegated(v, m) {
			return false
		}
		if (is(first, "self") || is(last, "again")) && !doneAgain(text, m, next) {
			return false
		}
		if is(last, "self") || is(last, "myself") || cutBodyParts[last] {
			if hurtByAccident(next) {
				return false
			}
		}
	case wantWords[first] && unicode.IsLetter(rune(text[m[0]])):
		if !ownSubject(text, m[0]) {
			return false
		}
	case thinkWords[first]:
		// "thinking about suicide" with no "I" is the poster's unless its
		// subject is someone else: "my mum is thinking about suicide",
		// "Hamlet considering suicide", "why do teens think about suicide".
		if f.someoneElseThinks(v, m, ws) {
			return false
		}
	case !f.supportSaid(v, m, ws, next):
		return false
	case is(first, "suicidal") && len(ws) == 2 && !is(last, "again"):
		// "suicidal thoughts aren't a character flaw" says what they are,
		// not that the poster has them; "suicidal thoughts are back" does.
		if len(next) > 0 && genericVerbs[next[0]] && !(len(next) > 1 && thoughtsReturn[next[1]]) &&
			!hasFirstPerson(wordsIn(clauseBefore(text, m[0]))) {
			return false
		}
	case reasonWords[first] && !is(first, "no") && !is(first, "nothing") && !is(first, "zero") && !is(last, "me"):
		if !futile(text, m[0]) {
			return false
		}
	}
	switch {
	case is(last, "kms"):
		return f.kmsCheck(v, m, next)
	case (hasWord(ws, "cut") || hasWord(ws, "cutting")) && cutsSomethingElse(text, m, ws, next):
		return false
	case is(last, "cut"):
		return !cutObject(next)
	case is(last, "overdose") || is(last, "od"):
		// "overdose on my meds", not "on caffeine".
		return len(next) == 0 || !is(next[0], "on") || anyIn(next[1:min(len(next), 4)], pillWords) ||
			anyIn(next[1:min(len(next), 4)], medicineNames)
	case is(last, "myself") && accidentAhead(next):
		return false
	case is(last, "myself") && (hasWord(ws, "drown") || hasWord(ws, "drowning")) && nextIn(0, canonSetOf(`in`)):
		// "drown myself in coffee", not "in the river".
		return anyIn(next[1:min(len(next), 4)], waterWords)
	case is(last, "myself") && (hasWord(ws, "shoot") || hasWord(ws, "shooting")) && len(next) > 0 &&
		(messageWords[next[0]] || shootFillers[next[0]] && len(next) > 1 && messageWords[next[1]] ||
			is(next[0], "in") && anyIn(next[1:min(len(next), 4)], canonSetOf(`foot feet leg legs`))):
		return false // "shoot myself a reminder", "shoot myself in the foot"
	case (is(last, "pills") || is(last, "meds") || is(last, "tablets") || is(last, "medicine") ||
		is(last, "medicines") || is(last, "medication") || is(last, "medications")) && hasWord(ws, "all"):
		// "take all my pills before breakfast, doctor said".
		return !anyIn(wordsIn(sentenceAround(text, m[0], m[1])), medRoutine)
	case is(last, "forever") || is(last, "bus") || is(last, "car") || is(last, "truck") || is(last, "train") ||
		is(last, "lorry"):
		// "i want to sleep forever after this exam week lol", "hope i get
		// hit by a bus so i can skip the exam lol", and sleep after an exam
		// with no laugh: "i want to sleep forever after the last exam".
		sentence := wordsIn(sentenceAround(text, m[0], m[1]))
		if is(last, "forever") && nextIn(0, sleepUntil) && anyIn(next[1:min(len(next), 5)], examsSleptAfter) {
			return false
		}
		return !(anyIn(sentence, schoolWorkWords) && anyIn(sentence, laughs))
	case is(last, "dead") && hasWord(ws, "rather") || is(last, "death"):
		// "i'd rather be dead than wear that", "rather be dead last", "i'd
		// rather be dead tired than fail", "i want Death Note".
		return len(next) == 0 || !rankOrThan[next[0]] && !deadIntensified[next[0]] &&
			(is(last, "dead") || deathTrail[next[0]])
	case is(last, "jumping"):
		// "think about jumping" off something, or with somewhere high in
		// the sentence; not "jumping to CSE".
		return nextIn(0, canonSetOf(`off from`)) ||
			len(next) == 0 && anyIn(wordsIn(sentenceAround(text, m[0], m[1])), heights)
	case selfCompound(text, m[1]):
		return false
	case anyIn(ws, makeWords) && hasWord(ws, "myself") || anyIn(ws, makeWords) && hasWord(ws, "self"):
		// "i make myself throw up after meals"; "make myself sick" only
		// after a meal, not "with worry"; not "making myself throw up
		// laughing". Read before livingWords, which has the "up" of
		// "throw up" for "give up".
		return ownSubject(text, m[0]) && (!is(last, "sick") || len(next) == 0) &&
			!(len(next) > 0 && strings.HasSuffix(next[0], "ing") && !is(next[0], "eating"))
	case livingWords[last] || is(last, "life") && hasWord(ws, "with") ||
		is(last, "going") && len(next) > 0 && is(next[0], "to"):
		// "keep going" is read like "go on" only with "to" after it: "keep
		// going to tuition".
		return livingCheck(text, m, ws, next)
	case (hasWord(ws, "cut") || hasWord(ws, "cutting")) && nextIn(0, cutIdioms):
		return false
	case len(ws) >= 2 && is(ws[0], "i") && simplePast[ws[1]] && hurtByAccident(next):
		// "I burned myself on the Bunsen burner", "i cut myself shaving".
		return false
	case is(last, "myself") && anyIn(ws, burnWords) && nextIn(0, canonSetOf(`out`)):
		return false // "burning myself out studying"
	case anyIn(ws, hitSelfWords) && (is(last, "myself") || is(last, "self") || is(last, "head") || hitBodyParts[last]):
		// "i hit myself when i'm stressed", "till i bleed"; not "i hit
		// myself on the door frame lol" or "i scratch myself when
		// mosquitoes bite". Banging one's head is an idiom of frustration
		// unless it bleeds or bruises: "banging my head against the wall
		// over this proof, so stressed"; and a part of the body is
		// scratched or hit as often for an itch or by accident, so it
		// wants bleeding or bruises too, and no itch: "i scratch my arms
		// till they bleed", not "i scratched my legs till they bled, these
		// mosquito bites".
		s := wordsIn(sentenceAround(text, m[0], m[1]))
		part := is(last, "head") || hitBodyParts[last]
		return (anyIn(s, bleedSigns) || !part && anyIn(s, hurtSigns)) && !(hitBodyParts[last] && anyIn(s, itchSigns)) &&
			!hurtByAccident(next) && !accidentAhead(next) && !(len(next) > 0 && (is(next[0], "on") || is(next[0], "into")))
	case is(last, "die"):
		// "ready to die for my country"; not "want to die for real", or a
		// death owed for something: "i deserve to die for what i did", "i
		// should just die for being this stupid".
		if len(next) > 0 && is(next[0], "for") && !(len(next) > 1 && (is(next[1], "real") || is(next[1], "good"))) &&
			!hasWord(ws, "deserve") && !hasWord(ws, "should") && !hasWord(ws, "shud") && !hasWord(ws, "shld") {
			return false
		}
		return !dieIdiom(next) && !hillToDieOn(text, m, next) && !dyesSomething(next)
	case is(last, "disappear") && nextIn(0, setOf(`from for`)):
		return anyIn(next[1:min(len(next), 3)], canonSetOf(`world earth existence life everyone everything good ever forever`))
	case is(last, "myself") && (hasWord(ws, "kill") || hasWord(ws, "killing")) && nextIn(0, canonSetOf(`laughing laughin lol`)):
		return false
	case is(last, "myself") && (hasWord(ws, "hang") || hasWord(ws, "hanging")) && hangsFromABar(next):
		return false
	case wantWords[first] && (is(last, "end") || is(last, "stop") || is(last, "over")):
		// "i just want it all to end", not "i just want it all to end, this
		// exam season is brutal" or "i want everything to stop for a minute".
		s := wordsIn(sentenceAround(text, m[0], m[1]))
		return (len(next) == 0 || endItTimes[next[0]] || trailFillers[next[0]] || shrugs[next[0]]) &&
			!anyIn(s, examsSleptAfter) && !anyIn(s, schoolWorkWords) && !anyIn(s, endedThings)
	case is(last, "me") && hasWord(ws, "without"):
		// "my family would be better off without me", not "without me
		// hogging the tv".
		return !(len(next) > 0 && strings.HasSuffix(next[0], "ing"))
	case is(last, "life") && hasWord(ws, "up"):
		// "i give up on life", not "i give up on life admin" or "chem
		// homework done, i give up on life lol".
		s := sentenceAround(text, m[0], m[1])
		sw := wordsIn(s)
		return livingCheck(text, m, ws, next) &&
			!(anyIn(sw, schoolWorkWords) && (anyIn(sw, laughs) || strings.ContainsAny(s, "💀☠😂🤣")))
	case is(last, "it") && (hasWord(ws, "end") || hasWord(ws, "ending")):
		// "end it" is the end of something else unless nothing, or a time,
		// comes after it: "i'm going to end it tonight", not "end it with her"
		// or "ending it tonight with a final revision of organic chem".
		before, after := clausesBeside(text, m[0], m[1])
		v.work += 2 * besideWindow
		return (len(next) == 0 || nextIn(0, endItTimes)) && !endsWithWork(next) && !opensOnAnEnd(before) &&
			!opensOnAnEnd(after)
	case is(last, "things") && hasWord(ws, "ending") && nextIn(0, canonSetOf(`with between`)):
		// "ending things with my boyfriend", but not "ending things with pills".
		return !anyIn(next[1:min(len(next), 4)], partnerWords)
	case is(last, "dying") && nextIn(0, dyeObjects):
		return false
	case is(last, "suicide") && nextIn(0, suicideTopics):
		return false
	}
	return true
}

// supportSaid reads the support phrasings whose words are as often said of
// something else, by what opens them: an overdose just taken only of ten
// or more, by the poster, with no headache or doctor round it ("just took 2
// paracetamol for my headache"); "ending it" and "gonna end it" only by the
// poster and not of an essay or a series; cutting "for 2 years" and a
// relapse only the poster's, "clean from sh" only with a relapse in the
// post and the poster's "I" or only a duration before it; "by the time you read this i'll be gone" not "to goa"; a suicide
// note not one written for a class or found in a book; "life isn't worth
// living" not someone else's ("his life"); and the Hinglish "jaan de dunga"
// not "tere liye" or "desh ke liye", "zindagi khatam kar dunga" not
// "teri", and sleeping pills taken not two of them for a headache.
func (f *scannedField) supportSaid(v *view, m []int, ws, next []string) bool {
	text := v.text
	r := f.wordReading()
	first := ws[0]
	prev := ""
	if p := wordsBefore(text, m[0], 1); len(p) > 0 {
		prev = p[0]
	}
	sentence := func() []string { return wordsIn(sentenceAround(text, m[0], m[1])) }
	switch {
	case tookWords[first]:
		if !ownSubject(text, m[0]) || anyIn(sentence(), medRoutine) {
			return false
		}
		for _, w := range ws[1:min(len(ws), 3)] {
			if n, err := strconv.Atoi(w); err == nil {
				return n >= 10
			}
		}
	case is(first, "ending") || is(first, "gonna"):
		// "i'm ending it here, the essay is long enough": the clause beside
		// it opens on what is ended, not "sorry for this post" or "too much
		// drama at home".
		before, after := clausesBeside(text, m[0], m[1])
		v.work += 2 * besideWindow
		return ownSubject(text, m[0]) && !opensOnAnEnd(before) && !opensOnAnEnd(after)
	case is(first, "been") || is(first, "relapsed"):
		return ownSubject(text, m[0])
	case is(first, "clean"):
		// "i was 3 months clean from sh and relapsed": the poster said
		// "I" in the clause, not "my friend was".
		before := wordsIn(clauseBefore(text, m[0]))
		return r.postHas("relapse", relapseWords) &&
			(ownSubject(text, m[0]) || anyIn(before, selfSubjects) || onlyADuration(before))
	case is(first, "by"):
		return !(len(next) > 0 && travelWords[next[0]])
	case is(ws[len(ws)-1], "dead") && hasWord(ws, "off"):
		// "you'd be better off dead" is said to the reader, a goad the
		// self_harm rule holds, not the poster's own crisis.
		return !anyIn(wordsIn(clauseBefore(text, m[0])), secondSubjects) && !(len(next) > 0 && deadIntensified[next[0]])
	case thoughtsHad[first] && hasWord(ws, "thoughts") && len(next) > 0 && genericVerbs[next[0]]:
		// "having thoughts of suicide is common" says what they are.
		return false
	case is(ws[len(ws)-1], "suicidal") && !selfSubjects[first] || planWords[first] || giveUpWords[first] ||
		thoughtsHad[first] && hasWord(ws, "thoughts"):
		// "feeling suicidal", "planning to end my life", "giving up on
		// life", "having thoughts of suicide": the poster's only.
		return ownSubject(text, m[0])
	case is(first, "suicide") && len(ws) > 3:
		// "suicide is the only option for me", not "for you" (a goad) or
		// "for him".
		s := wordsIn(sentenceAround(text, m[0], m[1]))
		if anyIn(s, storyWords) || anyIn(s, writingWork) || anyIn(s, otherObjects) {
			return false // "in the play, suicide is the only option left for the heroine"
		}
		return !anyIn(s, secondPerson) && !anyIn(s, readerWords) && (hasFirstPerson(s) || !anyIn(s, otherPeople) && !anyIn(s, otherSubjects))
	case is(first, "nahi") || is(first, "nhi"),
		(is(first, "jeene") || is(first, "jine")) && len(ws) == 4 && is(ws[1], "ka") && (is(ws[2], "mann") ||
			is(ws[2], "man") || is(ws[2], "dil")):
		// "mujhe nahi jeena", "ab jeene ka mann nahi", not "mujhe nahi jeena
		// aise boring life mein bina chai ke lol" or "jeene ka mann nahi
		// bharta is gaane se".
		for _, w := range next {
			if !hindiLifeTails[w] {
				return false
			}
		}
		return !anyIn(sentence(), laughs)
	case is(ws[len(ws)-1], "note") || is(ws[len(ws)-1], "letter") || len(ws) > 3 && is(ws[2], "note") ||
		len(ws) > 3 && is(ws[2], "letter"):
		// A note for a class or in a book, said so in its own clause: "the
		// teacher made us analyse the suicide note in the play", not "i
		// wrote my suicide note. nobody in my class will notice".
		clause := append(wordsIn(clauseBefore(text, m[0])), wordsIn(clauseAfter(text, m[1]))...)
		return !anyIn(clause, writingWork) && !anyIn(clause, storyWords)
	case is(first, "life") || is(first, "my") && len(ws) > 1 && is(ws[1], "life"):
		return !otherOwners[prev] && !hindiOthers[prev]
	case is(first, "apni") || is(first, "jaan"):
		// "tere liye jaan de dunga", and after it, "jaan de dunga is
		// project ke liye".
		return !anyIn(wordsIn(clauseBefore(text, m[0])), forSomeone) && !anyIn(next, forSomeone)
	case is(first, "zindagi") || is(first, "zindgi") || is(first, "jindagi"):
		return !hindiOthers[prev]
	case is(first, "neend") || is(first, "goliyan") || is(first, "goliya") || is(first, "golia"):
		if n, err := strconv.Atoi(prev); err == nil && n < 10 || fewWords[prev] {
			return false
		}
		ss := sentence()
		return !anyIn(ss, medRoutine) && !anyIn(ss, hindiPillRoutine)
	}
	return true
}

var (
	// planWords, giveUpWords and thoughtsHad open a support phrasing that
	// needs the poster as its subject.
	planWords = canonSetOf(`planning planned plotting decided deciding made`)
	// hitSelfWords hurt oneself other than by cutting or burning, which
	// supportCheck wants with hurtSigns in the sentence.
	hitSelfWords = canonSetOf(`scratch scratched scratching hit hitting punch punched punching bang banged banging slap
		slapped slapping`)
	hurtSigns = canonSetOf(`bleed bleeds bleeding bled blood bloody bruise bruises bruised bruising marks scars stress
		stressed anxious anxiety angry upset panic panicking overwhelmed hate punish punishment numb`)
	bleedSigns = canonSetOf(`bleed bleeds bleeding bled blood bloody bruise bruises bruised bruising marks scars`)
	// hitBodyParts are the parts of the body the self-hitting phrasings
	// name besides the head, and itchSigns what scratches them otherwise.
	hitBodyParts = canonSetOf(`arm arms wrist wrists leg legs thigh thighs skin stomach face`)
	itchSigns    = canonSetOf(`itch itches itchy itching mosquito mosquitoes mosquitos mozzie bite bites rash rashes
		eczema allergy allergic hives sunburn`)
	makeWords = canonSetOf(`make made making`)
	// secondSubjects are the reader as the subject of a clause.
	secondSubjects = canonSetOf(`you u ya yall y'all you're youre ur you'd youd u'd you'll youll r`)
	giveUpWords    = canonSetOf(`give gave given giving`)
	thoughtsHad    = canonSetOf(`having have had get getting got keep`)
	// thoughtsReturn, after "suicidal thoughts are", say the poster has
	// them: "are back", "are getting worse".
	thoughtsReturn = canonSetOf(`back again worse bad so really getting coming creeping constant louder unbearable
		everywhere returning killing`)
	// hindiLifeTails may follow "nahi jeena": a time, "anymore" or a
	// filler, nothing that names what life is not wanted like.
	hindiLifeTails = canonSetOf(`ab abhi aur anymore yaar yar bas bus please pls plz sach mein me really
		honestly bhai bro dost mujhe main`)
	tookWords  = canonSetOf(`took taken swallowed downed popped`)
	medRoutine = canonSetOf(`doctor doctors dr prescribed prescription dose dosage breakfast lunch dinner headache
		headaches fever cold cough pain cramps migraine vitamin vitamins supplements routine usual daily`)
	hindiPillRoutine = canonSetOf(`dard bukhar sardard sir sar dawai daktar`)
	fewWords         = canonSetOf(`do teen char ek 2 3 4`)
	// endedThings are what "ending it" may end besides a life: "i'm ending
	// it here, the essay is long enough", a series or a story. They are
	// storyWords less "ending", which the match itself is.
	endedThings = canonSetOf(`essay essays article answer chapter chapters video vlog stream rant thread post
		story stories poem speech relationship situationship friendship contract streak episode episodes season
		seasons movie movies film films show shows series book books novel novels anime manga scene scenes
		character characters finale plot netflix drama serial webseries cartoon comic comics fanfic fiction`)
	// endedLeads may come before what is ended at the start of a clause;
	// endCauses after "end it with" are why; farewellPosts with "last" are
	// a goodbye.
	endedLeads    = canonSetOf(`the this that my our its last final next one a an of whole entire`)
	endCauses     = canonSetOf(`results result marks mark grades grade scores score rank ranks percentile`)
	farewellPosts = canonSetOf(`post posts message messages msg note letter goodbye`)
	relapseWords  = canonSetOf(`relapse relapsed relapsing`)
	selfSubjects  = canonSetOf(`i i'm im i've ive i'd i'll`)
	travelWords   = canonSetOf(`to for on at in into`)
	// writingWork is a suicide note written for a class or read in one.
	writingWork = canonSetOf(`assignment essay essays english class creative writing poem prompt analyse analyze
		play novel character`)
	otherOwners = canonSetOf(`his her their your ur whose its a the`)
	forSomeone  = canonSetOf(`liye khatir vaaste waaste`)
	hindiOthers = canonSetOf(`teri tera tumhari tumhara uski uska unki unka inki iski`)
)

// onlyADuration reports words before "clean from sh" that say only for how
// long, with no one named: "3 months clean from sh and relapsed", "was
// almost a year clean".
func onlyADuration(ws []string) bool {
	unit := false
	for _, w := range ws {
		switch {
		case durationUnits[w]:
			unit = true
		case isASCIIDigit(w[0]) || durationWords[w] || carriers[w]:
		default:
			return false
		}
	}
	return unit
}

// hangsFromABar reports "hang myself" from a bar or the rings, or upside
// down: a hang in a gym or a playground, not from a fan, a tree or a rope.
func hangsFromABar(next []string) bool {
	if len(next) > 0 && is(next[0], "upside") {
		return true
	}
	return len(next) > 1 && hangPlaces[next[0]] && anyIn(next[1:min(len(next), 5)], hangBars)
}

// cutsSomethingElse reports a cut that is of something named, short of the
// object straight after it that cutObject reads: a session cut short ("i cut
// last night's study session short", "cut practice"), hair or nails cut ("cut
// my bangs"), or, after a cut with nothing named, hair or nails in the
// sentence ("i want to cut again, my bangs grew out"). A cut of the poster
// ("cut myself", "cutting my wrists") is never something else, whatever is
// round it ("after practice", "under my hair"), and nor is hair in an idiom
// or a hiding place ("tearing my hair out", "hid it under my hair").
func cutsSomethingElse(text string, m []int, ws, next []string) bool {
	if anyIn(ws, selfCuts) || anyIn(ws, cutBodyParts) || cutOfTheBody(next) {
		return false
	}
	if hasWord(next[:min(len(next), 5)], "short") {
		return true
	}
	object := next
	if len(object) > 1 && cutDets[object[0]] {
		object = object[1:]
	}
	if len(object) > 0 && (cutShort[object[0]] || hairAndNails[object[0]]) {
		return true
	}
	sentence := wordsIn(sentenceAround(text, m[0], m[1]))
	for i, w := range sentence {
		if hairAndNails[w] && !anyIn(sentence[max(0, i-3):i], hairIdioms) {
			return true
		}
	}
	return false
}

// cutOfTheBody reports a part of the poster's body cut, named after "cut":
// "my wrists", "my arm".
func cutOfTheBody(next []string) bool {
	return len(next) > 1 && is(next[0], "my") && cutBodyParts[next[1]]
}

// endsWithWork reports "end it" or "ending it" with a piece of school work or
// something watched: "ending it tonight with a final revision of organic
// chem", "end it with a question", not "with pills". Results or marks after
// "with" are why, not what ("end it tonight with these results"), and a
// last post is a goodbye ("ending it tonight with one last post").
func endsWithWork(next []string) bool {
	for i, w := range next {
		if is(w, "with") {
			later := next[i+1 : min(len(next), i+6)]
			if anyIn(later, endCauses) || hasWord(later, "last") && anyIn(later, farewellPosts) {
				return false
			}
			return anyIn(later, schoolWorkWords) || anyIn(later, endedThings) || anyIn(later, revisionWords)
		}
	}
	return false
}

// opensOnAnEnd reports a clause that opens on what "ending it" ends, past
// its determiners: "the essay is long enough", "this series has dragged on",
// "last episode of the series".
func opensOnAnEnd(ws []string) bool {
	for _, w := range ws[:min(len(ws), 4)] {
		if endedThings[w] {
			return true
		}
		if !endedLeads[w] {
			return false
		}
	}
	return false
}

// besideWindow is how far clausesBeside reads each way.
const besideWindow = 120

// clausesBeside returns the words of the clause before the one at text[a:b]
// and of the clause after it, within its sentence and besideWindow bytes.
func clausesBeside(text string, a, b int) (before, after []string) {
	s := text[max(0, a-besideWindow):a]
	if i := strings.LastIndexAny(s, ".!?\n"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndexAny(s, ",;:"); i >= 0 {
		s = s[:i]
		if j := strings.LastIndexAny(s, ",;:"); j >= 0 {
			s = s[j+1:]
		}
		before = wordsIn(s)
	}
	t := text[b:min(len(text), b+besideWindow)]
	if i := strings.IndexAny(t, ".!?\n"); i >= 0 {
		t = t[:i]
	}
	if i := strings.IndexAny(t, ",;:"); i >= 0 {
		t = t[i+1:]
		if j := strings.IndexAny(t, ",;:"); j >= 0 {
			t = t[:j]
		}
		after = wordsIn(t)
	}
	return before, after
}

var (
	// eatWords are "eat" in Hinglish, said whole: "jaa mar kha le", not
	// "khatam" (finish) or "khaai" (a ditch).
	eatWords      = canonSetOf(`kha khale khaa khao khana khaana khaake khake khaale`)
	durationUnits = canonSetOf(`day days week weeks month months year years yr yrs`)
	durationWords = canonSetOf(`a an one two three four five six seven eight nine ten eleven twelve few couple of
		almost nearly over about around like half whole full`)
	hangPlaces   = canonSetOf(`from on off by`)
	hangBars     = canonSetOf(`bar bars rings ring monkey pull pullup chinup chin jungle gym trapeze`)
	cutShort     = canonSetOf(`session sessions nap trip practice`)
	hairAndNails = canonSetOf(`hair hairs bangs fringe nails nail beard mullet layers`)
	// selfCuts make a cut the poster's; cutDets may come before what is
	// cut; cutBodyParts are where on themselves; hairIdioms, before hair,
	// make it no haircut ("tearing my hair out", "under my hair").
	selfCuts     = canonSetOf(`myself meself mysef`)
	cutDets      = canonSetOf(`the my our this that today's tonight's tomorrow's a`)
	cutBodyParts = canonSetOf(`wrist wrists arm arms thigh thighs leg legs skin vein veins stomach hip hips
		forearm forearms ankle ankles`)
	hairIdioms = canonSetOf(`pull pulling pulled tear tearing tore rip ripping under behind hide hid hiding cover
		covering through`)
	revisionWords = canonSetOf(`revision revisions revise revising notes flashcards question questions problem problems
		chapter syllabus chem physics maths math bio biology history`)
	// sleepUntil and examsSleptAfter: "sleep forever after the last exam".
	sleepUntil      = canonSetOf(`after till until once when`)
	examsSleptAfter = canonSetOf(`exam exams boards finals final paper papers sem semester sems test tests mock mocks
		jee neet assignment assignments submission deadline project olympiad`)
)

// cutObject reports something cut named after "cut": "cut my hair", "cut
// down on sugar", "cut 5 kg", "cut the video". With nothing named, or
// "again" or "so bad" after it, the cut is the poster's own.
func cutObject(next []string) bool {
	if len(next) == 0 || cutOfTheBody(next) {
		return false
	}
	return isASCIIDigit(next[0][0]) || cutObjects[next[0]]
}

var cutObjects = canonSetOf(`my the a an his her their your ur our it them that this these those some down off
	out up back class classes school lectures lecture carbs sugar ties corners costs weight kg hair nails losses
	deal deals screen time`)

// accidentAhead reports an accident named in the words after a match: "stab
// myself with the compass by accident".
func accidentAhead(next []string) bool {
	for i, w := range next {
		if is(w, "accidentally") || is(w, "by") && i+1 < len(next) && (is(next[i+1], "accident") ||
			is(next[i+1], "mistake")) {
			return true
		}
	}
	return false
}

// supportPhrasing reads the support phrasings that need the post round
// them, and says whether it decided: a past attempt with a past time after
// "end it" ("I tried to end it last weekend", not "with her"), an overdose
// of medicine, not of caffeine; "I don't think I'll make it" to an age or a
// birthday, not to a class or with school work in the sentence; "this is my
// last post" among goodbyes, not before a break; "a plan and a date" with
// nothing after it; "top myself" that is not topping up; and the Hinglish
// that needs a goodbye ("sab khatam kar dunga. sorry mummy papa") or not
// staying ("na is duniya mein") in the post.
func (f *scannedField) supportPhrasing(text string, m []int, ws, next []string) (ok, decided bool) {
	r := f.wordReading()
	first, last := ws[0], ws[len(ws)-1]
	switch {
	case len(ws) > 2 && (is(ws[1], "tried") || is(ws[1], "attempted")):
		if is(last, "it") || is(last, "all") {
			if endingARelationship(r, next) {
				return false, true
			}
			return len(next) == 0 || pastTimes[next[0]] || trailFillers[next[0]], true
		}
		return true, true
	case is(last, "overdosed"):
		return len(next) == 0 || !is(next[0], "on") || anyIn(next[1:min(len(next), 4)], pillWords) ||
			anyIn(next[1:min(len(next), 4)], medicineNames), true
	case is(last, "myself") && (is(ws[len(ws)-2], "top") || is(ws[len(ws)-2], "topping")):
		return !(len(next) > 0 && is(next[0], "up")), true
	case hasWord(ws, "think") && (is(last, "to") || is(last, "till") || is(last, "until") || is(last, "past")):
		sentence := wordsIn(sentenceAround(text, m[0], m[1]))
		if anyIn(sentence, schoolWorkWords) || anyIn(sentence, hindiStudyWords) || len(next) == 0 {
			return false, true
		}
		// An age, not a time of day or a count: "to 18", not "till 12
		// tonight", "to 18:00" or "to 18 credits".
		n, err := strconv.Atoi(next[0])
		age := err == nil && n >= 13 && n <= 30 && !(len(next) > 1 && (clockWords[next[1]] || countNouns[next[1]])) &&
			!clockAfter(text, m[1])
		return age || ageMarks[next[0]] || len(next) > 1 && is(next[0], "my") && ageMarks[next[1]], true
	case is(last, "post") || is(last, "message") || is(last, "thread"):
		// "my last post on this thread" leaves a thread, not the board.
		if anyIn(next[:min(len(next), 3)], threadWords) {
			return false, true
		}
		return r.postHas("farewell", farewellWords) && !r.postHas("break", breakWords), true
	case (is(first, "i'm") || is(first, "im") || is(first, "i")) && hasWord(ws, "on") && (is(last, "roof") ||
		is(last, "rooftop") || is(last, "terrace") || is(last, "ledge") || is(last, "bridge") || is(last, "edge")):
		return r.postHas("jump", jumpWords) && r.postHas("now", nowWords) && !r.postHas("pool", poolWords) &&
			!r.postHas("jumpSport", jumpSports), true
	case is(last, "date"):
		for _, w := range next {
			if !trailFillers[w] {
				return false, true
			}
		}
		return true, true
	case (is(first, "khud") || is(first, "apne")) && (is(last, "kiya") || is(last, "kia") || is(last, "liya") ||
		is(last, "diya")):
		// "apne aap ko hurt kiya football mein" is an injury.
		return !r.postHas("mishap", hindiMishaps), true
	case is(last, "jaunga") || is(last, "jaungi") || is(last, "jaaunga") || is(last, "jaaungi") ||
		is(last, "jaoonga") || is(last, "jaoongi"):
		// "kood jaunga agar india jeeta" is jumping for joy.
		return !r.postHas("win", winWords), true
	case is(first, "sab") || is(first, "sabkuch") || is(first, "sb"):
		return r.postHas("apology", apologyWords) && !r.postHas("study", hindiStudyWords), true
	case is(first, "is") && len(ws) > 1 && is(ws[1], "duniya"):
		return r.postHas("stay", stayWords) && r.postHas("hindiNot", hindiNot), true
	}
	return false, false
}

// endingARelationship reads "I tried to end it" as a break-up: "with my
// boyfriend" after it, or a break-up named in the post ("should i break up
// with him"), or a partner and a fight ("me and my gf have been fighting"),
// with nothing in the post that an attempt leaves behind (a hospital,
// pills, a rope).
func endingARelationship(r *wordReading, next []string) bool {
	for k := 0; k+1 < len(next) && k < 5; k++ {
		if is(next[k], "with") && (romanticPartners[next[k+1]] ||
			k+2 < len(next) && (is(next[k+1], "my") || is(next[k+1], "him") || is(next[k+1], "her")) &&
				romanticPartners[next[k+2]]) {
			return true
		}
	}
	if r.postHas("attemptSigns", attemptSigns) {
		return false
	}
	return r.postHas("breakup", breakupWords) || r.postHasPair("breakUp", breakVerbs, "up") ||
		r.postHas("partner", romanticPartners) && r.postHas("fight", partnerFights)
}

// clockAfter reports a time of day written with a colon after offset i:
// "18:00".
func clockAfter(text string, i int) bool {
	rest := strings.TrimLeft(text[i:min(len(text), i+12)], " ")
	j := 0
	for j < len(rest) && isASCIIDigit(rest[j]) {
		j++
	}
	return j > 0 && j+1 < len(rest) && (rest[j] == ':' || rest[j] == '.') && isASCIIDigit(rest[j+1])
}

var (
	romanticPartners = canonSetOf(`boyfriend girlfriend bf gf partner ex husband wife crush relationship`)
	breakupWords     = canonSetOf(`breakup dump dumped dumping dumps`)
	breakVerbs       = canonSetOf(`break broke breaking breaks`)
	partnerFights    = canonSetOf(`fight fighting fights fought argue arguing argued argument arguments cheated
		cheating`)
	attemptSigns = canonSetOf(`hospital ambulance icu pills overdose overdosed rope noose wrists survived pumped
		paramedics`)
	// countNouns after a number make it a count, not an age: "make it to 18
	// credits".
	countNouns = canonSetOf(`credits credit points marks percent hours hrs minutes mins pages units subjects
		courses classes lectures questions km kms miles laps reps days weeks months followers subs subscribers`)
	threadWords = canonSetOf(`thread threads topic board subject`)
)

var (
	// medicineNames are medicines by name: "overdosed on my antidepressants".
	medicineNames = canonSetOf(`antidepressants antidepressant tylenol acetaminophen ibuprofen aspirin advil panadol
		crocin dolo insulin xanax benadryl melatonin capsules`)
	// pastTimes put an attempt in the past: "tried to end it last weekend".
	pastTimes = canonSetOf(`last yesterday ago before once twice earlier in at when this a few two three`)
	// ageMarks are an age or a time of life: "make it to 18", "to my birthday".
	ageMarks = canonSetOf(`eighteen sixteen seventeen twenty adulthood birthday 18th 16th 17th 21st 20s graduation
		christmas summer`)
	// farewellWords and breakWords: "this is my last post" among goodbyes,
	// letters and thanks, not before a break.
	farewellWords = canonSetOf(`letters letter goodbye goodbyes bye thank thanks sorry forgive love`)
	breakWords    = canonSetOf(`before till until break hiatus back moving switching exams boards semester
		vacation holidays tomorrow week month`)
	// jumpWords and nowWords: on a roof in a post about jumping, now.
	jumpWords  = canonSetOf(`jump jumping fall falling high enough`)
	nowWords   = canonSetOf(`rn now right currently tonight`)
	jumpSports = canonSetOf(`long triple pole practice parkour`)
	clockWords = canonSetOf(`am pm baje oclock o'clock tonight midnight noon hours hrs minutes mins`)
	// hindiMishaps make hurting oneself an injury, and winWords jumping off
	// a roof a celebration.
	hindiMishaps = canonSetOf(`football cricket khelte khel match accident gir gira giri girke ankle twist chot moch
		bike cycle kitchen`)
	winWords = canonSetOf(`jeeta jeeti jeete jeet win wins won`)
	// apologyWords are a goodbye in Hinglish: "sorry mummy papa".
	apologyWords    = canonSetOf(`sorry maaf maafi alvida goodbye bye`)
	hindiStudyWords = canonSetOf(`syllabus chapter chapters homework kaam notes revision assignment padhai portion
		sawal questions`)
	// stayWords and hindiNot: "mujhe ab yahan nahi rehna. na is duniya mein".
	stayWords = canonSetOf(`rehna rahna jeena jina`)
	hindiNot  = canonSetOf(`nahi nhi na`)
)

var (
	stockpileWords = canonSetOf(`saving saved stockpiling stockpiled hoarding hoarded stashing stashed collecting collected`)
	pillContainers = canonSetOf(`bottle bottles box boxes packet packets case cases organiser organizer wrapper wrappers strip strips`)
	ownActs        = canonSetOf(`hurt hurting harmed harming harm cut cutting burned burnt burning burn starved starving starve self`)
	wantWords      = canonSetOf(`want wanted wanting wanna wana need wish wished wishing`)
	thinkWords     = canonSetOf(`think thinking thought considering contemplating`)
	reasonWords    = canonSetOf(`no nothing any a the zero`)
	// genericVerbs, straight after "suicidal thoughts", make it a statement
	// about them: "suicidal thoughts are more common than people think".
	genericVerbs = canonSetOf(`are aren't arent is isn't isnt can can't cant cannot don't dont do does doesn't may might
		affect affects happen happens mean means make makes come comes should shouldn't need needs among`)
)

// jokingCause reports crisis words as the hyperbole they are when something
// is said to cause them and the poster laughs: "statistics homework making
// me want to die lol", "this group project is going to make me kms 💀". The
// cause is named, it is not life itself ("life makes me want to die") or
// someone at home, bullying or a partner ("my stepdad makes me want to die
// 💀"), and a laugh or a skull is in the sentence; a crying face is not one.
// Without them the same words are read as meant: "i want to die lol" can be
// distress written lightly.
func jokingCause(text string, m []int) bool {
	before := wordsIn(clauseBefore(text, m[0]))
	n := len(before)
	if n < 3 || !is(before[n-1], "me") || !causeVerbs[before[n-2]] || anyIn(before[:n-2], lifeItself) ||
		anyIn(before[:n-2], seriousCauses) {
		return false
	}
	// From the start of the sentence to a little past the match, where a
	// laugh or an emoji after a full stop still belongs to it.
	lo := max(0, m[0]-64)
	lo += strings.LastIndexAny(text[lo:m[0]], ".!?\n") + 1
	around := text[lo:min(len(text), m[1]+48)]
	return strings.ContainsAny(around, "💀☠😂🤣") || anyIn(wordsIn(around), laughs)
}

// shrugged reports crisis words said as an exclamation at a mishap the post
// names and makes light of, as jokingCause does for "makes me": "kms" opening
// its sentence with what happened straight after it ("kms the wifi died right
// as I hit submit … it went through thank god"), or "I wanted to die" in the
// simple past about a moment named in its sentence ("forgot my lines in front
// of the whole year, I wanted to die 💀"). A laugh, a 💀 or relief ("thank
// god", "phew", "it went through") has to be within the sentence or just
// after it; a crying face is not one. Life itself, a serious cause, or a time
// that makes it an episode rather than a moment ("last night", "again",
// "still", "every day") keeps the words as meant.
func shrugged(text string, m []int, ws []string) bool {
	lo := max(0, m[0]-64)
	lo += strings.LastIndexAny(text[lo:m[0]], ".!?\n") + 1
	before := wordsIn(text[lo:m[0]])
	if len(ws) > 1 && is(ws[0], "i") {
		before, ws = append(before, ws[0]), ws[1:]
	}
	switch {
	case len(ws) == 1 && is(ws[0], "kms") && len(before) == 0:
		// What happened, straight after it: a verb in the past within
		// its first four words.
		next := nextWords(text[m[1]:min(len(text), m[1]+48)], 4)
		if len(next) == 0 || strings.ContainsAny(text[m[1]:min(len(text), m[1]+2)], ".!?,;:") {
			return false
		}
		past := false
		for _, w := range next {
			past = past || pastVerb(w)
		}
		if !past {
			return false
		}
	case len(ws) >= 3 && is(ws[0], "wanted") && len(before) > 0 && is(before[len(before)-1], "i") &&
		is(ws[len(ws)-1], "die"):
		// A moment named before it in the sentence.
		if len(before) < 4 {
			return false
		}
	default:
		return false
	}
	around := text[lo:min(len(text), m[1]+120)]
	aw := wordsIn(around)
	if anyIn(aw, lifeItself) || anyIn(aw, seriousCauses) || anyIn(aw, episodeWords) {
		return false
	}
	return strings.ContainsAny(around, "💀☠😂🤣") || anyIn(aw, laughs) || anyIn(aw, reliefWords) ||
		strings.Contains(around, "thank god") || strings.Contains(around, "went through") ||
		strings.Contains(around, "all good")
}

// pastVerb reports a verb in the simple past: a regular "-ed", or one of
// pastIrregulars.
func pastVerb(w string) bool {
	return len(w) > 3 && strings.HasSuffix(w, "ed") || pastIrregulars[w]
}

var (
	pastIrregulars = canonSetOf(`died went forgot said hit got came ate fell broke lost left saw told made did had was
		were froze spilt ran sent wrote took gave thought felt`)
	// episodeWords make crisis words an episode, not a moment: a time, or
	// a habit.
	episodeWords = canonSetOf(`night yesterday today tonight again still always every everyday since weeks months
		lately recently often sometimes constantly anymore`)
	reliefWords = canonSetOf(`thankfully phew nvm jk`)
)

var (
	causeVerbs = canonSetOf(`make makes making made`)
	lifeItself = canonSetOf(`life living existing existence everything myself`)
	// seriousCauses are causes no laugh makes a joke of.
	seriousCauses = canonSetOf(`stepdad stepmom stepmum stepfather stepmother dad mom mum father mother parents
		parent family uncle aunt brother sister bullied bullying bullies bully abuse abused abusing hitting beating
		boyfriend girlfriend bf gf ex`)
	// laughs are a laugh: not "😭", "bruh" or "ded", which can as well be
	// someone crying or past caring.
	laughs = canonSetOf(`lol lmao lmfao rofl haha hahaha hahahaha hehe xd`)
)

// ownSubject reports that the poster is the one doing what starts at offset
// i: the nearest subject before it in its clause is "I" ("I've been",
// "i used to", "I relapsed and"), or there is none and the clause opens
// with it or with words that only carry it ("been", "started",
// "honestly"). Another person turns it down ("she's been", "we"), and so
// does any other word, which could be someone else ("the editor cut again",
// "doctors are stockpiling"), unless "and" or "then" comes after it, joining
// a second verb to the first one's subject.
func ownSubject(text string, i int) bool {
	ws := wordsIn(quoteLink.ReplaceAllString(clauseBefore(text, i), " "))
	joined := false
	for k := len(ws) - 1; k >= 0; k-- {
		switch w := ws[k]; {
		case firstPersonSingular[w] || hindiFirstPerson[w]:
			return true
		case otherPeople[w] || otherSubjects[w] || hindiOtherPeople[w]:
			return false
		case joiners[w]:
			joined = true
		case carriers[w]:
		case !joined:
			return false
		}
	}
	return true
}

var (
	firstPersonSingular = canonSetOf(`i i'm im i'll i'd i've ive me my myself`)
	otherObjects        = canonSetOf(`him them character characters protagonist hero heroine victim victims`)
	otherSubjects       = canonSetOf(`we we're we've we'll we'd us our they'd their his her its it who`)
	joiners             = canonSetOf(`and then n &`)
	// carriers carry a verb without being its subject: "been",
	// "started", "still", "honestly".
	carriers = canonSetOf(`been have has had keep kept keeps still started start starting again just really
		honestly literally actually also always often sometimes lowkey finally secretly basically even already
		now today yesterday tonight lately recently back to used am was 'm do did so ngl tbh fr like ok okay
		anyway anyways yeah well because cause cuz bc but tho though lol idk`)
)

// someoneElseThinks reports that the thinking a support match starts with
// ("thinking about suicide", "considering ending it") is someone else's.
// The poster's own needs no "I" ("been thinking about suicide every day",
// "staring at the ceiling thinking about suicide"), so it is turned down
// only when, walking back from it over the words that only carry a verb, a
// subject that is not the poster turns up: a person or a pronoun ("my mum",
// "teens", "a classmate who"), a name with a capital ("is Hamlet thinking"),
// any word before "is" or "was" ("the narrator of The Bell Jar is
// thinking"), or any word at all before "think" or "thought", which take
// their subject straight before them ("why do teenagers think").
func (f *scannedField) someoneElseThinks(v *view, m []int, ws []string) bool {
	text := v.text
	if hasFirstPerson(ws) || ownSubject(text, m[0]) {
		return false
	}
	clause := clauseBefore(text, m[0])
	at := m[0] - len(clause)
	finite := !strings.HasSuffix(ws[0], "ing")
	aux := false
	sp := wordSpans(clause)
	for k := len(sp) - 1; k >= 0; k-- {
		w := clause[sp[k][0]:sp[k][1]]
		switch {
		case thirdAux[w]:
			aux = true
			continue
		case carriers[w]:
			continue
		case otherPeople[w] || otherSubjects[w] && !is(w, "it") || otherObjects[w] || hindiOtherPeople[w] ||
			otherThinkers[w]:
			return true
		case strings.HasSuffix(w, "'s") && !notPossessive[w]:
			return true // "my mum's been thinking"
		}
		if finite || aux {
			return true
		}
		if k > 0 {
			a, b := v.span(at+sp[k][0], at+sp[k][1])
			r, _ := utf8.DecodeRuneInString(f.src[a:b])
			return unicode.IsUpper(r)
		}
		return false
	}
	return false
}

var (
	// thirdAux are the forms of "be" and "have" that carry another
	// person's verb: "my roommate is thinking", "the poet was thinking".
	thirdAux = canonSetOf(`is isn't isnt are aren't arent was wasn't wasnt were weren't werent has hasn't hasnt been`)
	// otherThinkers are people the poster talks about.
	otherThinkers = canonSetOf(`teens teen teenagers teenager adolescents youth young children child kids kid mum mom
		mummy mommy mother dad daddy father papa mama parent parents roommate roommates roomie classmate classmates
		mate mates bestie bff cousin uncle aunt aunty auntie grandma grandpa nan boyfriend girlfriend bf gf partner
		husband wife narrator speaker poet author writer novelist patient patients men women boys girls guy guys
		girl boy person student pupil pupils teacher sibling siblings`)
	notPossessive = canonSetOf(`it's that's what's there's here's let's who's where's how's`)
)

// doneAgain reads a bare "self harming" or "cut again" as something the
// poster does: an "I" or a verb that carries it before it ("i'm", "been",
// "started"), or "again" or a time after it; and not "by" or "from" after
// it, which make it something done to them ("cut again by the selectors").
func doneAgain(text string, m []int, next []string) bool {
	if len(next) > 0 && (is(next[0], "by") || is(next[0], "from")) {
		return false
	}
	if is(lastWord(text[m[0]:m[1]]), "again") || len(next) > 0 && againTimes[next[0]] {
		return true
	}
	before := wordsIn(clauseBefore(text, m[0]))
	return len(before) > 0 && (firstPersonSingular[before[len(before)-1]] || carriers[before[len(before)-1]])
}

var againTimes = canonSetOf(`again since for every daily last yesterday today tonight lately recently now`)

// hurtByAccident reads the words after "hurt myself" or "cut myself" for
// an accident or an activity, named: "accidentally", "by accident", "on the
// stove", "on a nail", "while cooking", "shaving", "lifting weights", "at
// football". What is not named as one is not one: "on my wrist", "on
// monday", "using a razor blade", "trying to feel something", "when I'm sad".
func hurtByAccident(next []string) bool {
	if len(next) == 0 {
		return false
	}
	w := next[0]
	switch {
	case is(w, "accidentally"):
		return true
	case is(w, "by"):
		return len(next) > 1 && (is(next[1], "accident") || is(next[1], "mistake"))
	case is(w, "on"):
		return anyIn(next[1:min(len(next), 4)], accidentObjects)
	case is(w, "while") || is(w, "whilst") || is(w, "when") || is(w, "during") || is(w, "at") || is(w, "in"):
		ahead := next[1:min(len(next), 5)]
		return anyIn(ahead, activities) || anyIn(ahead, activityGerunds)
	}
	return activityGerunds[w]
}

var (
	// activities, after "at", "in", "during" or "while", make hurting
	// oneself an accident.
	activities = canonSetOf(`football cricket basketball volleyball hockey tennis badminton kabaddi practice
		training gym match game pe sports sport nets workout lab kitchen work rehearsal`)
	// activityGerunds are what someone was doing when they got hurt:
	// "shaving", "cooking", "chopping onions". Cutting and burning are not
	// among them, being what "hurt myself cutting" means.
	activityGerunds = canonSetOf(`shaving cooking ironing making opening chopping slicing dicing peeling grating
		carving whittling sharpening playing cleaning washing gardening climbing running cycling biking riding
		skating skateboarding falling tripping slipping fixing repairing building working sewing knitting baking
		frying grilling trimming lifting training practicing practising boxing hiking welding soldering moving
		unpacking packing sawing drilling hammering dancing jumping swimming surfing`)
	// accidentObjects, after "on", are what someone hurt themselves on.
	accidentObjects = canonSetOf(`stove hob oven pan pot kettle iron burner bunsen grill toaster glass nail nails
		fence wire paper can tin lid edge corner table desk door metal rock rocks stone stones bike chain stapler
		zip zipper broken sharp radiator heater exhaust tray`)
)

// forAPurpose reports "for" and an ordinary purpose named after a match:
// "saving my meds for the trip", "for the school trip", "for exams". Nothing
// else is one: "for weeks" is how long, and "for when I'm ready", "for
// tonight", "for the end", "for the right moment" and "for it" are the
// stockpile's purpose said without naming it.
func forAPurpose(next []string) bool {
	if len(next) < 2 || !is(next[0], "for") {
		return false
	}
	for _, w := range next[1:min(len(next), 5)] {
		switch {
		case ordinaryPurposes[w]:
			return true
		case !purposeLeads[w]:
			return false
		}
	}
	return false
}

var (
	// ordinaryPurposes are what medicines are put by for, and purposeLeads
	// the words that may come before one: "for my school trip".
	ordinaryPurposes = canonSetOf(`trip trips camp camps exam exams journey travel travelling traveling holiday holidays
		vacation tour trek flight flights hostel semester term emergency emergencies`)
	purposeLeads = canonSetOf(`the my a an our this that next upcoming school college class field winter summer`)
)

// futile reports a "not" or a "what's" in the clause before offset i,
// which makes "any reason to keep living" or "the point of going on"
// hopeless: "i don't see any reason", "what's the point", "is there any
// point". "I found a reason to live" has none.
func futile(text string, i int) bool {
	ws := wordsIn(clauseBefore(text, i))
	for k, w := range ws {
		if futileWords[w] || strings.HasSuffix(w, "n't") || is(w, "there") && k > 0 && (is(ws[k-1], "is") ||
			is(ws[k-1], "isn't")) {
			return true
		}
	}
	return false
}

var futileWords = canonSetOf(`no not never dont cant cannot couldnt doesnt isnt without lost losing hardly barely
	what's whats wats what`)

// counterfactualCheck reads "if i died" and the like for what it imagines:
// other people's reaction to the poster's death or absence, in the same
// sentence ("nobody would care", "everyone would be happier", "would
// anyone even notice"); not a game ("if i died in minecraft") or an exam
// ("if i died in the exam"), dyed hair ("if i died my hair"), an absence for
// a while ("if i was gone for a week"), or anything else that would follow
// ("if i wasn't here, who would answer your questions").
func counterfactualCheck(text string, m []int, ws, next []string) bool {
	if gameDeath(next) || len(next) > 0 && (is(next[0], "for") || is(next[0], "from") || is(next[0], "to")) &&
		!(len(next) > 1 && is(next[1], "good")) {
		return false
	}
	// "if i died in the exam", "if i died my hair purple".
	if len(next) > 1 && (is(next[0], "in") || is(next[0], "during") || is(next[0], "at")) &&
		anyIn(next[1:min(len(next), 4)], schoolWorkWords) || dyesSomething(next) {
		return false
	}
	sentence := wordsIn(sentenceAround(text, m[0], m[1]))
	others := false
	for k, w := range sentence {
		if reactors[w] || is(w, "one") && k > 0 && is(sentence[k-1], "no") {
			others = true
			break
		}
	}
	return others && anyIn(sentence, reactions)
}

var (
	// reactors are who a counterfactual imagines reacting.
	reactors = canonSetOf(`nobody noone anyone anybody everyone everybody people family parents mum mom dad mummy
		papa friends world who nobody'd everyone'd they`)
	// reactions are what they would do.
	reactions = canonSetOf(`care cares notice miss happier better relieved mourn cry sad upset matter funeral
		remember forget`)
)

// dieIdiom reports "die of embarrassment", "die from boredom", "die
// laughing" or "die trying" in the words after "die".
func dieIdiom(next []string) bool {
	return len(next) > 1 && (is(next[0], "of") || is(next[0], "from")) && dieIdioms[next[1]] ||
		len(next) > 0 && (is(next[0], "laughing") || is(next[0], "laughin") || is(next[0], "trying") ||
			is(next[0], "tryin"))
}

// hillToDieOn reports the idiom "a hill to die on", "die on this hill".
func hillToDieOn(text string, m []int, next []string) bool {
	return len(next) > 0 && is(next[0], "on") && (hasWord(next[:min(len(next), 4)], "hill") ||
		hasWord(wordsIn(clauseBefore(text, m[0])), "hill"))
}

// dyesSomething reports hair or clothes after "die", the spelling of "dye"
// that is also the word: "go die your hair blue", but not "you should die
// your hair is ugly".
func dyesSomething(next []string) bool {
	for i, w := range next[:min(len(next), 3)] {
		if dyeThings[w] {
			return (i == 0 || dyeObjects[next[0]]) && !(i+1 < len(next) && describes[next[i+1]])
		}
	}
	return false
}

// gameInClauseBefore reports a game named after "in" or "on" before a match,
// in its clause: "in minecraft you need to die to get back to spawn", not
// "minecraft noob, go die".
func gameInClauseBefore(text string, m []int) bool {
	ws := wordsIn(clauseBefore(text, m[0]))
	for i, w := range ws {
		if (is(w, "in") || is(w, "on")) && anyIn(ws[i+1:min(len(ws), i+4)], gameDeathWords) {
			return true
		}
	}
	return false
}

var (
	burnWords     = canonSetOf(`burn burning burned burnt`)
	partnerWords  = canonSetOf(`her him them boyfriend girlfriend bf gf partner ex crush husband wife friend friends bestie bff us`)
	dyeThings     = canonSetOf(`hair clothes shirt shirts jeans hoodie tshirt dress`)
	describes     = canonSetOf(`is isn't isnt was looks look looked sucks so too 's are`)
	livingWords   = canonSetOf(`live living exist up on`)
	lifeTopics    = canonSetOf(`admin science sciences skills skill insurance cycle cycles style styles span coach coaching lessons lesson hacks goals choices story stories partner expectancy support`)
	livingPlaces  = canonSetOf(`in with on at there here near abroad without together alone by like tomorrow today tonight next this until after before during for early late`)
	livingCues    = []string{"anymore", "any more", "like this", "this world", "this life"}
	dontWords     = canonSetOf(`don't dont dnt do`)
	dontSubjects  = canonSetOf(`just really honestly seriously literally so and but lol tbh ngl man bro yaar rn now today tonight anymore okay ok also still kinda like yeah i`)
	cutIdioms     = canonSetOf(`off a an some slack short loose out free`)
	simplePast    = canonSetOf(`cut burned burnt hurt`)
	dieIdioms     = canonSetOf(`laughing laughter embarrassment shame boredom cringe cuteness happiness excitement jealousy envy hunger thirst heat curiosity suspense anticipation secondhand shock fright cold heatstroke stress exhaustion tiredness dehydration hypothermia tension`)
	endItTimes    = canonSetOf(`tonight today now soon all tomorrow already finally forever here this`)
	dyeObjects    = canonSetOf(`my her his their our your the hair it them clothes shirt`)
	suicideTopics = canonSetOf(`prevention awareness essay essays rates rate statistics stats hotline helpline helplines squad bombing bomber bombers attack attacks mission missions doors door run runs research project report article documentary`)
	kmsQuantity   = canonSetOf(`many few some several couple more less fewer extra total thousand thousands hundred hundreds lakh lakhs million millions of the those these in kitne kitna`)
	kmsAfter      = canonSetOf(`away per from long far ahead left of an a hai`)
	kmsContext    = canonSetOf(`aws key keys encrypt encrypted encryption activation activate windows server license licence gcp azure cloud cmk s3 miles mile metres meters kilometres kilometers distance convert conversion speed kmph kmh mileage petrol diesel litre liter`)
)

// aboutSomeoneElse reports crisis language that is not in the poster's own
// voice: a match with no "I", "me" or "my" in it (nor "mujhe", "main", nor
// "kms", which is "kill myself") whose
// nearest person before it, in its clause, is someone else: "if a friend
// tells you they're thinking about suicide".
func aboutSomeoneElse(text string, m []int, ws []string) bool {
	if anyIn(ws, firstPerson) || hindiFirstPerson[ws[0]] || is(ws[0], "kms") {
		return false
	}
	before := wordsIn(clauseBefore(text, m[0]))
	for i := len(before) - 1; i >= 0; i-- {
		switch w := before[i]; {
		case firstPerson[w] || hindiFirstPerson[w]:
			return false
		case otherPeople[w] || hindiOtherPeople[w]:
			return true
		}
	}
	return false
}

var (
	hindiFirstPerson = canonSetOf(`mujhe main mai mein mera meri mere hum humko`)
	hindiOtherPeople = canonSetOf(`woh wo vo voh usko use unko usne unhone dost`)
	otherPeople      = canonSetOf(`they they're theyre he he's hes she she's shes someone somebody friend friends
		people you you're youre u ur everyone he'll she'll they'll kids students others brother sister`)
	heights = canonSetOf(`balcony roof rooftop terrace bridge building window floor ledge cliff tower edge`)
)

// livingCheck reads "I don't want to live", "done with life" and "no reason
// to go on" in their clause: "anymore" or "like this" keeps them crisis
// language; a place, a time or a subject straight after makes them about
// something else ("live in the hostel", "wake up at 5", "life sciences"), and
// so does a subject other than "I" before an i-less "don't".
func livingCheck(text string, m []int, ws, next []string) bool {
	after := clauseAfter(text, m[1])
	for _, cue := range livingCues {
		if strings.Contains(after, cue) || strings.Contains(after, canon(cue)) {
			return true
		}
	}
	if len(ws) > 0 && dontWords[ws[0]] {
		if prev := wordsIn(clauseBefore(text, m[0])); len(prev) > 0 && !dontSubjects[prev[len(prev)-1]] {
			return false
		}
	}
	// "I don't want to wake up tomorrow", with nothing after it to say when
	// or what for ("tomorrow at 5 for the exam").
	if len(ws) > 0 && is(ws[len(ws)-1], "up") && len(next) > 0 && canonSetOf(`tomorrow again ever`)[next[0]] {
		rest := true
		for _, w := range next[1:] {
			rest = rest && trailFillers[w]
		}
		if rest {
			return true
		}
	}
	// "going on to a phd", "going on to the next chapter", "keep going to
	// tuition", "wake up to another 8am lecture"; not "wake up to another
	// day", "keep going to be honest", nor "live to see", which can be
	// crisis language.
	if len(next) > 0 && is(next[0], "to") && len(ws) > 0 && (is(ws[len(ws)-1], "on") || is(ws[len(ws)-1], "up") ||
		is(ws[len(ws)-1], "going")) {
		return anyIn(next[1:min(len(next), 4)], toCrisis) || len(next) > 2 && is(next[1], "be") && is(next[2], "honest")
	}
	return len(next) == 0 || !lifeTopics[next[0]] && !livingPlaces[next[0]]
}

var toCrisis = canonSetOf(`day days morning mornings tomorrow life world reality this it`)

var (
	waterWords = canonSetOf(`river lake sea ocean pool bath bathtub tub well canal tank water pond dam nadi`)
	rankOrThan = canonSetOf(`than last first`)
	// deadIntensified after "dead" make it "very": "dead tired", "dead
	// broke", "dead serious".
	deadIntensified = canonSetOf(`tired broke serious wrong sure certain set centre center even quiet silent asleep
		bored beat scared bang`)
	// deathTrail may follow "i want death" or "praying for death".
	deathTrail = canonSetOf(`lol tbh honestly fr ngl anyway anyways though tho every each daily now already again
		so to at rn tonight please pls`)
)

// kmsCheck turns down "kms" as kilometres ("5 kms", "how many kms", "kms per
// litre", "convert miles to kms") or a key management service ("AWS KMS",
// "KMS activation"), and "KMS" as the capitals of a name ("KMS College").
func (f *scannedField) kmsCheck(v *view, m []int, next []string) bool {
	text := v.text
	if before := strings.TrimRight(text[:m[0]], " "); before != "" && isASCIIDigit(before[len(before)-1]) {
		return false
	}
	if prev := wordsIn(clauseBefore(text, m[0])); len(prev) > 0 && kmsQuantity[prev[len(prev)-1]] {
		return false
	}
	if strings.HasPrefix(text[m[1]:], "/") || len(next) > 0 && kmsAfter[next[0]] {
		return false
	}
	if anyIn(wordsIn(text[max(0, m[0]-40):min(len(text), m[1]+40)]), kmsContext) {
		return false
	}
	a, b := v.span(m[0], m[1])
	if f.src[a:b] == "KMS" {
		r, _ := utf8.DecodeRuneInString(strings.TrimLeft(f.src[b:], " "))
		if unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

// notNegated turns down encouragement that is the opposite: "don't cut
// yourself on the glass", "be careful not to kill yourself", "never drink
// bleach", "please don't kys", "a bad rank doesn't end your life", "failing
// isn't going to end your life", "don't let one exam end your life". The
// negation has to come straight before the words matched, in the same clause
// — with at most "you", "ever", "even", "dare", "go" or "just" between, or
// "let" and a short noun phrase that is not a person ("don't let one exam") —
// and not after "why", which makes it a question that urges ("why don't you
// just kys"). A negation further back is about something else: "i don't care,
// kys", "dont reply. just kill yourself", "never mind, kys", "it doesn't
// matter, end your life".
//
// It also turns down a warning: a "you'll", "could" or "might" straight
// before the words, with "careful" earlier in the sentence ("be careful with
// the scalpel, you could cut yourself") or "or" just before it ("wear gloves
// or you'll cut yourself"). "You're not careful enough, kys" has no modal.
func notNegated(v *view, m []int) bool {
	text := v.text
	before := text[max(0, m[0]-64):m[0]]
	sentence := before[strings.LastIndexAny(before, ".!?\n")+1:]
	if sw := wordsIn(sentence); len(sw) >= 2 && carefulModals[sw[len(sw)-1]] && warning(sw[:len(sw)-1]) {
		return false
	}
	words := strings.Fields(sentence[strings.LastIndexAny(sentence, ",;:()\"")+1:])
	for len(words) > 0 && negationFillers[words[len(words)-1]] {
		words = words[:len(words)-1]
	}
	if negatedLet(words) {
		return false
	}
	return !endsInNegation(words)
}

// endsInNegation reports words that end in one of negations, not straight
// after "why".
func endsInNegation(words []string) bool {
	tail := " " + strings.Join(words, " ")
	for _, neg := range negations {
		for _, form := range []string{neg, canon(neg)} {
			if !strings.HasSuffix(tail, " "+form) {
				continue
			}
			if prev := strings.Fields(strings.TrimSuffix(tail, " "+form)); len(prev) > 0 && whyWords[prev[len(prev)-1]] {
				continue
			}
			return true
		}
	}
	return false
}

// negatedLet reads "don't let one exam" and "never let a rank": a negation,
// "let", and up to three words naming what it is that should not, none of
// them a person, since "don't let anyone stop you" is the opposite.
func negatedLet(words []string) bool {
	for i := len(words) - 2; i >= max(0, len(words)-4); i-- {
		if !is(words[i], "let") {
			continue
		}
		if anyIn(words[i+1:], letPeople) {
			return false
		}
		head := words[:i]
		for len(head) > 0 && negationFillers[head[len(head)-1]] {
			head = head[:len(head)-1]
		}
		return endsInNegation(head)
	}
	return false
}

// warning reports the words before a modal that make what follows a warning:
// "careful" anywhere in them, or "or" (and "you") just before the modal.
func warning(ws []string) bool {
	if hasWord(ws, "careful") || hasWord(ws, "carefully") {
		return true
	}
	if n := len(ws); n > 0 && (is(ws[n-1], "or") || n > 1 && is(ws[n-2], "or") && negationFillers[ws[n-1]]) {
		return true
	}
	return false
}

// negations are what turn "kill yourself" into advice against it, when they
// come straight before it; negationFillers may come between. carefulModals
// end a warning: "careful, you could cut yourself".
var (
	negations = []string{"don't", "dont", "do not", "never", "not to", "didn't", "didnt", "shouldn't",
		"shouldnt", "won't", "wont", "will not", "mustn't", "doesn't", "doesnt", "does not",
		"isn't going to", "isnt going to", "is not going to", "isn't gonna", "isnt gonna", "is not gonna",
		"aren't going to", "arent going to", "are not going to", "aren't gonna", "arent gonna"}
	negationFillers = canonSetOf(`you u ever even dare go just please pls`)
	carefulModals   = canonSetOf(`you'll youll u'll ull could might can may will`)
	whyWords        = canonSetOf(`why y`)
	letPeople       = canonSetOf(`you u ya yourself urself anyone anybody nobody noone everyone everybody them him her
		me us people`)
)

// selfHarmCheck is notNegated with the places a self-harm phrase is something
// else: "rope yourself in" (to a project); "self-doubt"; "cut yourself some
// slack" or "off"; "cut deeper into the topic"; a death in a game; a warning
// about overwork ("you'll kill yourself studying"); "no one will care if you
// skip the party", where nothing about dying follows; and an imperative with
// no "you" ("drink bleach", "just die") that is not the start of what is said
// ("i'd rather drink bleach", "i just die every time").
func selfHarmCheck(v *view, m []int) bool {
	if !notNegated(v, m) || calledOnSomeoneElse(v.text, m) || askedWhether(v.text, m) {
		return false
	}
	text := v.text
	ws := wordsIn(text[m[0]:m[1]])
	if len(ws) == 0 {
		return true
	}
	first, last := ws[0], ws[len(ws)-1]
	next := wordsIn(clauseAfter(text, m[1]))
	nextIn := func(k int, set map[string]bool) bool { return len(next) > k && set[next[k]] }
	switch {
	case is(first, "rope"):
		return !nextIn(0, setOf(`in into`))
	case selfCompound(text, m[1]), gameDeath(next), gameInClauseBefore(text, m), v.reported(m),
		mentionedAsAKind(next), glossed(text, m):
		return false
	case is(last, "it") && hasWord(ws, "end") || is(last, "all") && hasWord(ws, "end"):
		// "just end it" and "you should end it" are a relationship, an
		// argument or a series as often: only with nothing but a shrug
		// after it, and none of those before or after it.
		if !onlyShrugs(next) || endsSomethingElse(wordsIn(text[max(0, m[0]-120):m[0]])) ||
			endsSomethingElse(wordsIn(text[m[1]:min(len(text), m[1]+120)])) || namesWhatIsEnded(text, m) {
			return false
		}
	case is(last, "starve") || is(last, "eating"):
		// "go starve", "you should stop eating", "stop eating fatty"; not
		// "starve the fire" or "stop eating so much sugar". "starve to
		// death" is the act itself.
		if !onlyShrugs(next) && !(len(next) == 2 && is(next[0], "to") && is(next[1], "death")) {
			return false
		}
	case is(ws[len(ws)-1], "fatty") || is(ws[len(ws)-1], "pig") || hasWord(ws, "fat") || is(last, "fatso") ||
		is(last, "fatass") || is(last, "piggy") || is(last, "lardass") || is(last, "cow") || is(last, "slob") ||
		is(last, "fuck") || is(last, "ass"):
		// "stop eating fatty", not "fatty foods".
		if !onlyShrugs(next) {
			return false
		}
	case (is(first, "ja") || is(first, "jaa") || is(first, "jao")) && len(ws) > 1 && strings.HasPrefix(ws[1], "mar"):
		// "ja mar gaya" is "died", told; "jaa mar kha le" is a beating taken
		// ("maar khana") or "go on, eat first".
		if nextIn(0, diedWords) || len(ws) == 2 && len(next) > 0 && strings.HasPrefix(next[0], "gay") ||
			nextIn(0, eatWords) {
			return false
		}
	case is(first, "take") && nextIn(0, pillRoutine) && !(is(next[0], "at") && len(next) > 1 && is(next[1], "once")):
		return false // "take all your meds on time" is a reminder; "at once" is not
	case is(last, "die") && (dyesSomething(next) || dieIdiom(next) || hillToDieOn(text, m, next)):
		return false
	case hasWord(ws, "cut") && !is(last, "deeper") && nextIn(0, cutIdioms):
		return false
	case is(last, "deeper") && nextIn(0, deeperObjects):
		return false
	case hasWord(ws, "miss") || hasWord(ws, "care") || hasWord(ws, "cry") || hasWord(ws, "notice"):
		return caresIfYouDie(next)
	case is(last, "exist") || is(last, "live") || is(last, "breathe") || is(last, "existed") || is(last, "born") ||
		is(last, "alive"):
		// "why do you even exist", "you shouldn't exist (on this planet)";
		// not "why do you live in delhi", "exist in the database" or "you
		// shouldn't exist on this planet without sunscreen".
		if onlyShrugs(next) {
			return true
		}
		if len(next) < 2 || !is(next[0], "on") && !is(next[0], "in") {
			return false
		}
		for j := 1; j < min(len(next), 4); j++ {
			if earthWords[next[j]] {
				return onlyShrugs(next[j+1:])
			}
		}
		return false
	case is(last, "dead") && hasWord(ws, "better"):
		// "you're better off dead", "u r honestly better off dead"; not
		// "i'd be better off dead", the poster's own, or "better off dead
		// tired than hungover".
		return youBefore(wordsIn(clauseBefore(text, m[0]))) && !nextIn(0, deadIntensified)
	}
	if prev := wordsIn(clauseBefore(text, m[0])); len(prev) > 0 && len(next) > 0 && strings.HasSuffix(next[0], "ing") {
		p := prev[len(prev)-1]
		if overworkModals[p] || len(prev) >= 2 && is(prev[len(prev)-2], "going") && is(p, "to") {
			return false // "you'll kill yourself studying like that"
		}
	}
	if is(first, "delete") && nextIn(0, setOf(`from`)) {
		return false // "delete yourself from the group"
	}
	bare := jumpVerbs[first] || is(first, "drink") || is(first, "just") || is(first, "take") || is(first, "stop") ||
		is(first, "cut") && is(last, "deeper") || is(first, "mar") || is(first, "ja") || is(first, "jaa") || is(first, "jaake") ||
		is(first, "an") || is(first, "off") || is(first, "delete") || is(first, "fanda") || is(first, "phanda") ||
		is(first, "fandaa") || is(first, "phandaa")
	before := clauseBefore(text, m[0])
	if is(first, "just") {
		before += " just" // read with the condition before it: "if you failed just end it"
	}
	if bare && !saidFirst(before) {
		return false
	}
	return true
}

// youBefore reports the words of the clause before "better off dead"
// saying it of the reader: "you're", "you'd be", "u r honestly", "you
// are", with at most four words such as "be", "really" or "so much" after
// it.
func youBefore(prev []string) bool {
	k := len(prev)
	for n := 0; k > 0 && n < 4 && betterFillers[prev[k-1]]; n++ {
		k--
	}
	return k > 0 && (youForms[prev[k-1]] || k > 1 && is(prev[k-1], "r") && is(prev[k-2], "u"))
}

var (
	youForms      = canonSetOf(`you u ur you're youre you'd youd u'd ud ya`)
	betterFillers = canonSetOf(`be are would will honestly really all probably literally seriously just so much way`)
	earthWords    = canonSetOf(`planet earth world`)
)

// urgeCheck vets a selfHarmUrgeRules match. The acts need a frame that
// says them to someone (urgeFramed); what judges the act itself needs it
// said of someone else, not the poster ("for me, death is the only
// option" is crisis language, which the support step answers). Either is
// turned down when it is negated, talked about rather than said ("what
// does kys mean"), in a game, or the act is a word with other uses here:
// "overdose on caffeine", "stop breathing so loud", "jump into the pool",
// "just do it" with no death in the post, "latak ja" on a bus.
func urgeCheck(v *view, m []int) bool {
	text := v.text
	ws := wordsIn(text[m[0]:m[1]])
	if len(ws) == 0 || calledOnSomeoneElse(text, m) {
		return false
	}
	first, last := ws[0], ws[len(ws)-1]
	kysing := kysForm.MatchString(first)
	// rope time, the act judged, Hinglish and "kys" say who it is for in
	// themselves; any other act has to be said to someone, which is read
	// first because it is the cheapest to turn down.
	timed := len(ws) == 2 && is(last, "time")
	judged := len(ws) >= 4 && (hasWord(ws, "better") || hasWord(ws, "solution") || hasWord(ws, "option") ||
		hasWord(ws, "answer") || hasWord(ws, "out") || hasWord(ws, "choice") || hasWord(ws, "cure") ||
		hasWord(ws, "fix") || hasWord(ws, "would") || hasWord(ws, "will") || hasWord(ws, "is"))
	hindi := is(first, "zeher") || is(first, "zehar") || is(first, "zahar") || is(first, "jeher") ||
		is(first, "zaher") || is(first, "apni") || strings.HasPrefix(first, "mar") || is(first, "latak") ||
		is(first, "lattak")
	if !timed && !judged && !hindi && !kysing && !urgeFramed(text, m[0]) {
		return false
	}
	next := wordsIn(clauseAfter(text, m[1]))
	sentence := wordsIn(sentenceAround(text, m[0], m[1]))
	if !(kysing && imagineNot(text, m[0])) && !notNegated(v, m) {
		return false
	}
	if v.reported(m) || mentionedAsAKind(next) || glossed(text, m) || gameDeath(next) || gameInClauseBefore(text, m) ||
		anyIn(sentence, gameDeathWords) || selfCompound(text, m[1]) {
		return false
	}
	if hasWord(ws, "cut") && len(next) > 0 && cutIdioms[next[0]] ||
		(strings.HasSuffix(last, "self") || strings.HasSuffix(last, "selves")) && len(next) > 0 &&
			strings.HasSuffix(next[0], "ing") && !is(next[0], "thing") {
		return false // "cut yourself some slack", "kill yourself studying"
	}
	// Only what comes after the act may follow it: nothing, a filler, a
	// time, or a place to do it in.
	settled := func(from int) bool {
		for _, w := range next[min(from, len(next)):] {
			if !urgeTails[w] && !trailFillers[w] && !imperativeFillers[w] {
				return false
			}
		}
		return true
	}
	switch {
	case timed:
		// "rope time for OP".
		return !hasFirstPerson(sentence) && (len(next) == 0 || is(next[0], "for") || settled(0))
	case judged:
		// The act judged: said of someone else, to them.
		if hasFirstPerson(sentence) && !hasWord(ws, "we") {
			return false
		}
		if hasWord(ws, "solution") || hasWord(ws, "option") || hasWord(ws, "answer") || hasWord(ws, "choice") ||
			hasWord(ws, "cure") || hasWord(ws, "fix") || hasWord(ws, "out") && hasWord(ws, "way") {
			return anyIn(sentence, secondPerson) || anyIn(sentence, othersWords)
		}
		// "better off without you know who's tweets" is no "you".
		return !(len(next) > 0 && is(next[0], "know") && (is(last, "you") || is(last, "u")))
	case is(first, "zeher") || is(first, "zehar") || is(first, "zahar") || is(first, "jeher") || is(first, "zaher") ||
		is(first, "apni") || strings.HasPrefix(first, "mar"):
		return true
	case is(first, "latak") || is(first, "lattak"):
		// "bus mein latak ja" is how a bus is boarded.
		return !anyIn(sentence, hangOnWords) || anyIn(sentence, hangWords)
	case kysing:
		return true
	}
	switch {
	case is(first, "overdose") || is(first, "od"):
		// "overdose on caffeine", but "overdose on your pills".
		if len(next) > 1 && is(next[0], "on") && !anyIn(next[1:min(len(next), 4)], pillWords) {
			return false
		}
		return settled(0) || len(next) > 0 && is(next[0], "on")
	case is(first, "rope") || is(first, "jump") && len(ws) == 1 || is(first, "die") && len(ws) == 1:
		if anyIn(sentence, poolWords) || dyesSomething(next) || dieIdiom(next) || hillToDieOn(text, m, next) {
			return false
		}
		return settled(0)
	case is(first, "do"):
		// "just do it already": only in a post about dying.
		return settled(0) && deathAround(text, m)
	case is(first, "stop") || is(first, "quit") || is(first, "cease"):
		return settled(0)
	case is(first, "self") || strings.HasPrefix(first, "selfharm"):
		return settled(0) || len(next) > 0 && urgePlaces[next[0]]
	}
	return true
}

var (
	// urgeTails may follow an act urged: "just jump already", "overdose
	// tonight", "self harm in a corner".
	urgeTails = canonSetOf(`already now tonight today tomorrow pls please plz asap immediately quickly fast
		permanently forever bro dude lol lmao man op tbh ffs nobody cares`)
	urgePlaces = canonSetOf(`in at somewhere quietly`)
	// othersWords are who an act is judged for besides "you": "for someone
	// with your marks", "for people like OP".
	othersWords = canonSetOf(`someone somebody people op anyone everyone losers loser`)
	poolWords   = canonSetOf(`pool water lake river sea beach trampoline bungee parachute skipping skip climbing gym
		queue line conclusions conclusion gun ship bandwagon pit sandpit hurdle hurdles athletics`)
	pillWords = canonSetOf(`pills pill meds medication medicines medicine tablets tablet sleeping paracetamol`)
	// hangOnWords make "latak ja" holding on, and hangWords hanging.
	hangOnWords = canonSetOf(`bus train metro local auto gate door darwaza rod handle tree jhoola swing bar`)
	hangWords   = canonSetOf(`pankha pankhe fan rassi rope phanda phansi fanda`)
	// deathTopic words make "just do it" one about dying: "nobody would
	// even come to your funeral. just do it".
	deathTopic = canonSetOf(`funeral grave coffin die dying dead death suicide kys rope noose bridge jump overdose
		unalive`)
)

// goads reports encouragement to self-harm in the phrasings selfHarmRules
// and selfHarmUrgeRules leave to the grammar, read as menace reads the hints
// of a threat: goadPhrases, each a wordRun read from its first word over the
// plain view's words and vetted by its check, and a knot drawn as an emoji
// with a nudge in the post ("you know what to do 🪢"). Like the rules, a goad
// reported or negated is turned down.
func (f *scannedField) goads() bool {
	v := &f.plain
	r := f.wordReading()
	g := &goadReading{wordReading: r, v: v}
	for i, w := range r.ws {
		for _, p := range goadPhrasesAt[w] {
			v.work += len(p.run)
			hi := p.run.match(r.ws, i, r.sentenceEnd[i])
			if hi < 0 {
				continue
			}
			v.work += checkWork
			m := []int{r.sp[i][0], r.sp[hi-1][1]}
			if p.check(g, i, hi) && !v.reported(m) && notNegated(v, m) {
				return true
			}
		}
	}
	v.work += len(v.text)
	if k := strings.Index(v.text, "🪢"); k >= 0 {
		m := []int{k, k + len("🪢")}
		return g.nudged() && !r.postHas("knots", knotHobbies) && !v.reported(m)
	}
	return false
}

// goadReading is a field's words as goads reads them, with what is read of
// the whole post read once: whether it nudges, and whether it says to finish.
type goadReading struct {
	*wordReading
	v               *view
	nudge, finisher int8
}

// nudged reports a nudge anywhere in the post, with nothing after it in its
// sentence: "just saying", "you know what to do", "go on then", "what are
// you waiting for", "nobody's stopping you"; not "go on then, apply".
func (g *goadReading) nudged() bool { return g.anyRun(&g.nudge, nudgeRuns, true) }

// finished reports words in the post that say to finish: "be done with it",
// "get on with it", "a few won't do it".
func (g *goadReading) finished() bool { return g.anyRun(&g.finisher, finishRuns, false) }

// anyRun reports a match of any of runs anywhere in the words, ending its
// sentence when final, read once and kept in *known: 0 not yet read, 1 no,
// 2 yes.
func (g *goadReading) anyRun(known *int8, runs []wordRun, final bool) bool {
	if *known == 0 {
		*known = 1
		for i := range g.ws {
			for _, run := range runs {
				g.v.work += len(run)
				if hi := run.match(g.ws, i, g.sentenceEnd[i]); hi >= 0 && (!final || g.endsSentence(hi)) {
					*known = 2
					return true
				}
			}
		}
	}
	return *known == 2
}

// goadPhrase is one phrasing of goads: its words, and the check that reads
// what is round them.
type goadPhrase struct {
	run   wordRun
	check func(g *goadReading, lo, hi int) bool
}

var (
	goadPhrases = []goadPhrase{
		// "if I were you I'd have offed myself years ago".
		{run: runOf(`if i were|was|had|got|looked|lived`), check: inYourShoes},
		// "you'd be doing everyone a favour if you didn't wake up tomorrow".
		{run: runOf(`doing|do|did everyone|everybody|us|the|all|world _2 favour|favor|service if you|u|ya`),
			check: goneAsAFavour},
		// "just take the whole bottle and be done with it".
		{run: runOf(`take|swallow|down|neck the|a whole|entire|full bottle|pack|packet|box|strip|jar|tub|sheet`),
			check: wholeSupply},
		// "the top floor of the car park has no barrier. just saying".
		{run: runOf(`no barrier|barriers|railing|railings|fence|fencing|net|nets|grille|guardrail|guardrails`),
			check: openDrop},
		// "then disappear. permanently."
		{run: runOf(`disappear|vanish`), check: goneForGood},
		// "should've jumped when you had the chance".
		{run: runOf(`should've|shouldve|shoulda|should have? jumped|died|roped|hanged|offed|killed`),
			check: tooLate},
		// "if you actually meant any of it you'd have done it by now".
		{run: runOf(`done|did it by now`), check: meantIt},
		{run: runOf(`done|did it already`), check: meantIt},
		// "khatam kar le apne aap ko", "apne aap ko khatam kar lo".
		{run: runOf(`khatam|khatm|khtm kar|kr le|lo apne aap`), check: always},
		{run: runOf(`khatam|khatm|khtm karle|krle|karlo|krlo apne aap`), check: always},
		{run: runOf(`apne aap ko khatam|khatm|khtm kar|kr le|lo`), check: always},
		{run: runOf(`apne aap ko khatam|khatm|khtm karle|krle|karlo|krlo`), check: always},
	}
	goadPhrasesAt = func() map[string][]*goadPhrase {
		at := map[string][]*goadPhrase{}
		for k := range goadPhrases {
			p := &goadPhrases[k]
			for w := range p.run[0].set {
				at[w] = append(at[w], p)
			}
		}
		return at
	}()
	nudgeRuns = []wordRun{runOf(`just saying`), runOf(`justsaying`), runOf(`hint hint`),
		runOf(`you|u|ya know what to do`), runOf(`go on then`), runOf(`go ahead`), runOf(`get on with it`),
		runOf(`what are|r you|u waiting for`), runOf(`nobody's|nobodys|nobody|noone stopping you|u`),
		runOf(`nobody|noone is stopping you|u`)}
	finishRuns = []wordRun{runOf(`done with it`), runOf(`get on with it`), runOf(`get it over with`),
		runOf(`won't|wont do it`), runOf(`will not do it`), runOf(`won't|wont be enough`), runOf(`end it`)}
	// knotHobbies make a knot drawn a knot: "knot practice for scouts".
	knotHobbies = canonSetOf(`knot knots scouts scout guides sailing climbing macrame bracelet bracelets camping
		tie tying lace laces`)
)

func always(*goadReading, int, int) bool { return true }

// inYourShoes reads "if I were you", "if I had your marks" or "if I looked
// like you" and then, in the same sentence, the poster saying they would
// have killed themselves: "if I were you I'd have offed myself years ago",
// "if i had your marks i'd jump off the roof". The words are a jab at the
// reader, not the poster's crisis (supportCheck reads them so too).
func inYourShoes(g *goadReading, lo, hi int) bool {
	ws := g.ws[hi:min(g.sentenceEnd[lo], lo+readReach)]
	if !readersShoes(g.ws[lo:min(g.sentenceEnd[lo], hi+3)]) {
		return false
	}
	for j, w := range ws {
		if !wouldWords[w] {
			continue
		}
		k := j + 1
		for k < len(ws) && wouldFillers[ws[k]] {
			k++
		}
		return k < len(ws) && selfAct(ws[k:])
	}
	return false
}

// readersShoes reports words that open with the poster put in the reader's
// place: "if i were you", "if i had your marks", "if i looked like u".
func readersShoes(ws []string) bool {
	return len(ws) >= 4 && is(ws[0], "if") && is(ws[1], "i") && shoesVerbs[ws[2]] &&
		anyIn(ws[3:min(len(ws), 6)], readerWords)
}

// inYourShoesBefore reports that the sentence before offset i of text opens
// with the poster in the reader's place, so that "I'd kill myself" after it
// is said at the reader.
func inYourShoesBefore(text string, i int) bool {
	before := text[max(0, i-64):i]
	ws := wordsIn(before[strings.LastIndexAny(before, ".!?\n")+1:])
	for k := range ws {
		if is(ws[k], "if") && readersShoes(ws[k:]) {
			return true
		}
	}
	return false
}

// selfAct reports words that open with the poster's own death or harm, as
// "I'd" goes on: "kill myself", "offed myself", "jump off". Not "end it",
// which after "if I were you" is advice to end something else.
func selfAct(ws []string) bool {
	w, next := ws[0], ""
	if len(ws) > 1 {
		next = ws[1]
	}
	switch {
	case selfActVerbs[w]:
		return is(next, "myself") && !(len(ws) > 2 && strings.HasSuffix(ws[2], "ing") && !is(ws[2], "thing"))
	case selfActBare[w]:
		return next == "" || goadTails[next]
	}
	return false
}

var (
	shoesVerbs   = canonSetOf(`were was had got looked lived`)
	readerWords  = canonSetOf(`you u ya your ur yours`)
	wouldWords   = canonSetOf(`i'd id would i'll`)
	wouldFillers = canonSetOf(`have 've just already probably prolly literally definitely honestly genuinely
		seriously simply actually of`)
	selfActVerbs = canonSetOf(`kill killed off offed top topped hang hanged neck necked unalive unalived shoot shot
		drown drowned`)
	selfActBare = canonSetOf(`kms jump jumped rope roped die died overdose overdosed od`)
	// goadTails may follow an act: "jump off", "died years ago".
	goadTails = canonSetOf(`off from already years ago long back tbh lol lmao honestly fr ngl immediately instantly
		ages then by now`)
)

// goneAsAFavour reads "doing everyone a favour if you" going on to the
// reader's death and nothing after it but a time: "if you didn't wake up
// tomorrow", "if you weren't here", "if you disappeared", "if you ended
// it". Not "if you didn't wake up late" or "if you didn't come tomorrow".
func goneAsAFavour(g *goadReading, lo, hi int) bool {
	ws := g.ws[hi:min(g.sentenceEnd[lo], hi+10)]
	at := func(k int, set map[string]bool) bool { return k < len(ws) && set[ws[k]] }
	n := 0
	switch {
	case at(0, setOf(`didn't didnt never`)) && at(1, setOf(`wake`)) && at(2, setOf(`up`)):
		n = 3
	case len(ws) > 3 && is(ws[0], "did") && is(ws[1], "not") && is(ws[2], "wake") && is(ws[3], "up"):
		n = 4
	case at(0, canonSetOf(`weren't werent wasn't wasnt`)) && at(1, canonSetOf(`here around alive born`)):
		n = 2
	case at(0, canonSetOf(`were was`)) && at(1, canonSetOf(`dead gone`)):
		n = 2
	case at(0, canonSetOf(`died disappeared vanished jumped`)):
		n = 1
	case at(0, canonSetOf(`ended`)) && at(1, canonSetOf(`it`)):
		n = 2
	case at(0, canonSetOf(`killed offed hanged`)) && at(1, canonSetOf(`yourself urself`)):
		n = 2
	case at(0, canonSetOf(`stopped`)) && at(1, canonSetOf(`breathing existing living`)):
		n = 2
	default:
		return false
	}
	for _, w := range ws[n:] {
		if !goadTails[w] && !urgeTails[w] && !soonWords[w] && !is(w, "ever") && !is(w, "again") {
			return false
		}
	}
	return true
}

// wholeSupply reads the whole of a supply taken, said to the reader ("just
// take the whole bottle", "you'd have to take the whole pack") in a post
// that says to finish ("and be done with it", "a few won't do it"), and
// not of food, notes or a course.
func wholeSupply(g *goadReading, lo, hi int) bool {
	if !urgeFramed(g.text, g.sp[lo][0]) && !haveTo(g.ws[max(g.clauseFrom(lo), lo-4):lo]) {
		return false
	}
	sentence := g.sentenceWords(lo)
	if anyIn(sentence, foodWords) || anyIn(sentence, supplyWords) || anyIn(sentence, studyWords) ||
		anyIn(sentence, pillRoutine) && hasWord(sentence, "course") {
		return false
	}
	// What it is a bottle or a pack of, when it says, is medicine or
	// left unsaid: "of whatever's in your mum's cabinet".
	if hi < g.sentenceEnd[lo] && is(g.ws[hi], "of") {
		if of := g.ws[hi+1 : min(g.sentenceEnd[lo], hi+5)]; !anyIn(of, pillWords) && !anyIn(of, unsaidSupply) {
			return false
		}
	}
	return g.finished()
}

// unsaidSupply leave what a bottle holds unsaid, or say where it is kept.
var unsaidSupply = canonSetOf(`whatever whatever's whatevers it them those these that cabinet medicine medicines
	drugs`)

// haveTo reports words that end in the reader's having to: "you'd have to",
// "you need to", "u have to".
func haveTo(ws []string) bool {
	n := len(ws)
	return n >= 3 && is(ws[n-1], "to") && urgeModalsTo[ws[n-2]] && youWords[ws[n-3]]
}

// youWords are the reader as the subject, with "would" run on: "you'd".
var youWords = canonSetOf(`you u ya you'd youd u'd ud`)

// openDrop reads a place to fall from said to have no barrier, with a nudge
// in the post ("just saying") and nothing that asks for it to be made safe:
// "the top floor of the car park has no barrier. just saying".
func openDrop(g *goadReading, lo, hi int) bool {
	return anyIn(g.sentenceWords(lo), heights) && g.nudged() && !g.postHas("safety", safetyWords)
}

// safetyWords make a drop with no barrier something to fix.
var safetyWords = canonSetOf(`report reported council complain complained complaint dangerous unsafe safety careful
	fix fixed fixing petition admin management accident accidents hazard warning fall falls falling fell slip`)

// goneForGood reads "disappear" said to the reader, with "permanently",
// "forever" or "for good" after it, in a sentence of its own or not, in a
// post that names a death or that nobody would miss them: "then disappear.
// permanently. nobody will miss your threads".
func goneForGood(g *goadReading, lo, hi int) bool {
	after := g.ws[hi:min(len(g.ws), hi+3)]
	if !(len(after) > 0 && (is(after[0], "permanently") || is(after[0], "forever")) ||
		len(after) > 1 && is(after[0], "for") && is(after[1], "good")) {
		return false
	}
	return urgeFramed(g.text, g.sp[lo][0]) && (g.postHas("death", deathTopic) || g.postHas("miss", missWords))
}

// missWords say nobody would miss someone.
var missWords = canonSetOf(`miss`)

// tooLate reads "should've jumped when you had the chance": said of the
// reader or of no one, with nothing after the act but the chance missed.
// Not "should've jumped on that deal" or "I should've jumped".
func tooLate(g *goadReading, lo, hi int) bool {
	if subj := g.subjectBefore(lo); subj != "" && !secondPerson[subj] || anyIn(g.sentenceWords(lo), firstPersonSubjects) {
		return false
	}
	ws := g.restOfClause(hi, 10)
	act := g.ws[hi-1]
	k := 0
	switch {
	case is(act, "killed") || is(act, "offed") || is(act, "hanged"):
		if len(ws) == 0 || !is(ws[0], "yourself") && !is(ws[0], "urself") {
			return false
		}
		k = 1
	case is(act, "jumped") && len(ws) > 0 && is(ws[0], "off"):
		return true
	}
	for i := k; i < len(ws); i++ {
		switch w := ws[i]; {
		case is(w, "at") && i+1 < len(ws) && is(ws[i+1], "birth"):
			i++
		case !chanceTails[w]:
			return false
		}
	}
	return true
}

// chanceTails may follow an act left too late: "when you had the chance",
// "years ago".
var chanceTails = canonSetOf(`when while you u had the a chance chances years ago back then already lol lmao tbh
	honestly bro instead last year time`)

// meantIt reads "you'd have done it by now" said to the reader of what they
// said they meant, with a nudge in the post: "if you actually meant any of
// it you'd have done it by now. go on then".
func meantIt(g *goadReading, lo, hi int) bool {
	sentence := g.sentenceWords(lo)
	return hasWord(sentence, "meant") && anyIn(sentence, secondPerson) && g.nudged()
}

// calledOnSomeoneElse reports an act written as a method called in code on
// someone other than the reader: in "me.die();" the receiver, "me", is who
// dies, so it urges nothing on anyone, where "you.die()" would. Code is read
// like any other text otherwise: "bring(\"gun\")" is still carrying a gun.
func calledOnSomeoneElse(text string, m []int) bool {
	if m[0] < 2 || text[m[0]-1] != '.' || !alnum(rune(text[m[0]-2])) || m[1] >= len(text) || text[m[1]] != '(' {
		return false
	}
	receiver := wordsBefore(text, m[0], 1)
	return len(receiver) == 1 && !secondPerson[receiver[0]]
}

// urgeFramed reports an act said to someone, in the clause that ends at
// offset i of text: an imperative opening the clause, with only fillers
// before it ("bro just rope"), "and" after a request that is not the
// poster's own ("do us all a favour and overdose"), or a modal said to
// them ("you should", "why don't you", "maybe you should", "time for you to",
// "you need to go").
func urgeFramed(text string, i int) bool {
	before := clauseBefore(text, i)
	if saidFirst(before) {
		return true
	}
	ws := wordsIn(before)
	k := len(ws)
	for k > 0 && imperativeFillers[ws[k-1]] && !secondPerson[ws[k-1]] {
		k--
	}
	ws = ws[:k]
	switch {
	case k == 0:
		return false
	case is(ws[k-1], "to") && k >= 3 && urgeModalsTo[ws[k-2]] && secondPerson[ws[k-3]]:
		return true // "you need to", "you deserve to"
	case is(ws[k-1], "to") && k >= 4 && (secondPerson[ws[k-2]] || othersWords[ws[k-2]]) && is(ws[k-3], "for") &&
		is(ws[k-4], "time"):
		return true // "time for you to"; "time to die" alone is a joke about exams
	case urgeModals[ws[k-1]] && k >= 2 && secondPerson[ws[k-2]]:
		return true // "you should", "u must"
	case k >= 3 && is(ws[k-1], "you") && is(ws[k-2], "don't") && is(ws[k-3], "why"):
		return true
	case k >= 2 && (is(ws[k-1], "dont") || is(ws[k-1], "don't")) && is(ws[k-2], "why"):
		return true
	case askedWhen(ws):
		return true // "when are you finally going to"
	}
	return false
}

// askedWhen reports words before an act that ask when the reader will do
// it, which urges: "when are you finally going to", "are you ever gonna",
// "when will you". Asked with neither "when" nor an adverb ("are you going
// to") it asks after them (askedWhether).
func askedWhen(ws []string) bool {
	k, nudged := len(ws), false
	skip := func() {
		for k > 0 && whenAdverbs[ws[k-1]] {
			nudged, k = true, k-1
		}
	}
	skip()
	going := false
	switch {
	case k >= 2 && is(ws[k-1], "to") && is(ws[k-2], "going"):
		k, going = k-2, true
	case k >= 1 && is(ws[k-1], "gonna"):
		k, going = k-1, true
	}
	skip()
	if k < 2 || !secondPerson[ws[k-1]] {
		return false
	}
	if aux := ws[k-2]; !(going && (is(aux, "are") || is(aux, "r")) || !going && is(aux, "will")) {
		return false
	}
	return nudged || k >= 3 && is(ws[k-3], "when")
}

// askedWhether reports a question whether the reader means to do it or has
// done it, which asks after them rather than urging: "are you going to kill
// yourself?", "do you want to hurt yourself?", "did you try to kill
// yourself?". "When are you going to", "are you finally going to" and a
// question that goes on ("or what") urge.
func askedWhether(text string, m []int) bool {
	ws := wordsIn(clauseBefore(text, m[0]))
	k := len(ws)
	switch {
	case k >= 2 && intentTo[ws[k-2]] && (is(ws[k-1], "to") || is(ws[k-1], "of") || is(ws[k-1], "about") ||
		is(ws[k-1], "on")):
		k -= 2
	case k >= 1 && (is(ws[k-1], "gonna") || is(ws[k-1], "wanna") || is(ws[k-1], "tryna")):
		k--
	default:
		return false
	}
	if k < 2 || !secondPerson[ws[k-1]] || !whetherAux[ws[k-2]] || k >= 3 && (whyWords[ws[k-3]] || is(ws[k-3], "when")) {
		return false
	}
	after := text[m[1]:min(len(text), m[1]+24)]
	q := strings.IndexByte(after, '?')
	return q >= 0 && !strings.ContainsAny(after[:q], ".!\n") && len(wordsIn(after[:q])) <= 2 &&
		!hasWord(wordsIn(after[:q]), "or")
}

var (
	whenAdverbs = canonSetOf(`finally ever even just actually already`)
	intentTo    = canonSetOf(`going planning trying tried try want wanted thinking about`)
	whetherAux  = canonSetOf(`are r do did have were will would`)
)

var (
	urgeModals   = canonSetOf(`should shud must better gotta can could`)
	urgeModalsTo = canonSetOf(`need needs deserve deserves ought have has`)
)

// imagineNot reports "imagine ... and not" straight before offset i: a
// taunt that says the thing it names should be done, "imagine posting this
// and not kysing immediately".
func imagineNot(text string, i int) bool {
	ws := wordsIn(text[max(0, i-80):i])
	n := len(ws)
	return n >= 2 && is(ws[n-1], "not") && hasWord(ws, "imagine")
}

// quotedNotSaid reports crisis words the poster passes on or talks about
// rather than says: reported, unless the report makes the poster its subject
// (reportedOfOthers); a word explained ("what does kms mean"); or the name of
// a kind of post ("the kms jokes").
func quotedNotSaid(v *view, m []int, next []string) bool {
	return v.reportedOfOthers(m) || glossed(v.text, m) || mentionedAsAKind(next)
}

// quotesTheReply reports that offset i is on a greentext line quoting the
// post being answered: the line opens with one ">", a reply link (">>123")
// comes before it in the post, and the poster's own words round the quote do
// not take it up as theirs ("same", "me too", "mood"). Without a reply link,
// or with "be me", "tfw" or "mfw" opening the run of greentext lines it is
// in, a greentext line is the poster's own story: ">>123 >be me >bullied
// every day >bringing my dad's glock tomorrow" is the poster's.
func (v *view) quotesTheReply(i int) bool {
	text := v.text
	start := v.lineStart(i)
	if !greentext(text[start:]) {
		return false
	}
	if link := v.firstReplyLink(); link < 0 || link >= start {
		return false
	}
	first := v.greentextRun(start)
	opening := nextWords(text[first+1:min(len(text), first+24)], 2)
	if len(opening) > 0 && narration[opening[0]] || len(opening) == 2 && is(opening[0], "be") && is(opening[1], "me") {
		return false
	}
	// The poster's own words: the line before the quote, and the first line
	// after it that is not quoted, a few quoted lines on at most, each cut to
	// a short reply's length.
	before := text[max(0, start-121):max(0, start-1)]
	before = before[strings.LastIndexByte(before, '\n')+1:]
	read := len(before) + 24
	after := ""
	for j, lines := i, 0; j < len(text) && lines < 4; lines++ {
		line := text[j:min(len(text), j+160)]
		end := strings.IndexByte(line, '\n')
		read += len(line)
		if end < 0 {
			break
		}
		j += end + 1
		if !strings.HasPrefix(text[j:], ">") || strings.HasPrefix(text[j:], ">>") {
			after = text[j:min(len(text), j+120)]
			if k := strings.IndexByte(after, '\n'); k >= 0 {
				after = after[:k]
			}
			read += len(after)
			break
		}
	}
	v.work += read
	return !agrees(wordsIn(before)) && !agrees(wordsIn(after))
}

// greentext reports a line that opens with one ">": a quote, not a reply
// link (">>123").
func greentext(line string) bool {
	return strings.HasPrefix(line, ">") && !strings.HasPrefix(line, ">>")
}

// greentextRun is where the run of greentext lines holding the line that
// starts at start begins. The view keeps the last answer, so the lines of
// a run are read back over once, not once for every match in them.
func (v *view) greentextRun(start int) int {
	text := v.text
	first := start
	for first > 0 {
		if first == v.runLine {
			first = v.runFirst
			break
		}
		prev := strings.LastIndexByte(text[:first-1], '\n') + 1
		v.work += first - prev
		if !greentext(text[prev:first]) {
			break
		}
		first = prev
	}
	v.runLine, v.runFirst = start, first
	return first
}

// lineStart is where the line holding offset i starts. Matches are read in
// order, so the view keeps the last answer and reads back only to it.
func (v *view) lineStart(i int) int {
	from := 0
	if v.lineAt <= i {
		from = v.lineAt
	}
	start := from + strings.LastIndexByte(v.text[from:i], '\n') + 1
	if start == from && from > 0 {
		start = v.line
	}
	v.work += i - from
	v.lineAt, v.line = i, start
	return start
}

// firstReplyLink is where the view's first reply link starts, or -1.
func (v *view) firstReplyLink() int {
	if v.link == 0 {
		v.work += len(v.text)
		v.link = -1
		if loc := quoteLink.FindStringIndex(v.text); loc != nil {
			v.link = loc[0] + 1
		}
	}
	if v.link < 0 {
		return -1
	}
	return v.link - 1
}

// agrees reports a reply that takes a quote up as the poster's own: "same",
// "me too", "mood", "this is me".
func agrees(ws []string) bool {
	k := 0
	for k < len(ws) && k < 2 && agreeFillers[ws[k]] {
		k++
	}
	if k == len(ws) {
		return false
	}
	at := func(j int, w string) bool { return j < len(ws) && is(ws[j], w) }
	switch w := ws[k]; {
	case agreement[w]:
		// "same", "same tbh", "mood": not "same guy", "the same thing".
		return k+1 == len(ws) || agreeTails[ws[k+1]] || allAgreeing(ws[k:])
	case is(w, "me"):
		return at(k+1, "too") || at(k+1, "2") || at(k+1, "as") && at(k+2, "well")
	case is(w, "i"):
		j := k + 1
		if at(j, "can") || at(j, "do") || at(j, "really") || at(j, "so") {
			j++
		}
		return at(j, "relate") || at(j, "felt") && (at(j+1, "that") || at(j+1, "this"))
	case is(w, "literally"):
		return at(k+1, "me")
	case is(w, "this") || is(w, "that") || is(w, "thats") || is(w, "that's"):
		j := k + 1
		if is(w, "this") || is(w, "that") {
			if !at(j, "is") {
				return false
			}
			j++
		}
		if at(j, "literally") || at(j, "so") {
			j++
		}
		return at(j, "me") && (j+1 == len(ws) || agreeTails[ws[j+1]] || is(ws[j+1], "every") ||
			is(ws[j+1], "rn"))
	}
	return false
}

// allAgreeing reports a line made only of agreement: "same same lol".
func allAgreeing(ws []string) bool {
	for _, w := range ws {
		if !agreement[w] && !agreeTails[w] && !agreeFillers[w] {
			return false
		}
	}
	return true
}

var (
	narration = canonSetOf(`tfw mfw mrw be`)
	// agreement opens a reply that takes up the post it quotes as the
	// poster's own: "same", "mood", "felt". It is agreement only as the
	// reply itself, alone or with one of agreeTails after it ("same tbh",
	// "same here"), after at most two of agreeFillers ("lol same", "ngl
	// mood"); "same guy as yesterday" and "happened to me too" report.
	agreement    = canonSetOf(`same samee sameee mood relatable relate felt ditto metoo`)
	agreeTails   = canonSetOf(`tbh fr lol lmao lmfao here bro sis tho though honestly ngl af fam dude man rn`)
	agreeFillers = canonSetOf(`lol lmao lmfao ngl fr tbh honestly deadass ok okay yeah ya yea bro real so ugh omg`)
)

// reported reports words the post passes on rather than says: someone
// else's, quoted or reported, which every rule that reads the poster's own
// voice turns down — crisis language, encouragement, threats, grooming cues,
// method questions and asks to talk elsewhere. They are passed on when they
// are:
//
//   - on a greentext line quoting the post being answered (quotesTheReply);
//   - in a quotation whose speaker is named and is not the poster
//     (quoteSpeaker): "she said 'kys'", "'…' my mum asked", "teacher: '…'",
//     "'…' — some guy in my class"; or that came to the poster: "told me
//     '…'", "got a message saying '…'", "I was told '…'", "someone wrote '…'
//     on my desk", "saw '…' on the wall";
//   - in a quotation named as a kind of post: "the 'add me on snap' guys";
//   - the same report without quotation marks (reportedClause): "my bf told
//     me to go kill myself", "a guy in my class keeps saying you should all
//     kys", "he said he'll shoot up the school".
//
// The poster's own words stay theirs however they are wrapped. A quotation
// no one is named as saying is read as written ("'I'm bringing a gun
// tomorrow' lol"), and so is one the poster gives themselves: "I said '…'",
// "me: '…'", "'…' - me", "you heard me", a label naming only who it is said
// to ("note to my bully: '…'", "to OP: '…'"), an instruction ("just tell him
// '…'", "you should say '…'") and a verb with no subject, or only an adverb
// before it ("already told everyone '…'"), which leaves out an "I"; and so
// is a greentext story the poster opens with "be me", after a reply link or
// not. A source that is not a person — "news: '…'", "'…' - police", "'…'
// reportedly" — names no speaker, so a threat dressed as a headline is still
// a threat.
//
// So a poster reporting a threat, a grooming attempt or being told to kill
// themselves is not held for it, and one who quotes their own threat is: the
// difference is who the post says the words belong to.
func (v *view) reported(m []int) bool { return v.passedOn(m, false) }

// reportedOfOthers is reported for crisis language, which stays the poster's
// own when a report without quotation marks makes the poster its subject:
// "my therapist says I'm suicidal", "mum asked if I want to die". Being told
// to is still someone else's words: "told me to go kill myself", "said I
// should just die".
func (v *view) reportedOfOthers(m []int) bool { return v.passedOn(m, true) }

func (v *view) passedOn(m []int, ownState bool) bool {
	text := v.text
	if v.quotesTheReply(m[0]) {
		return true
	}
	from := max(0, m[0]-reportReach)
	if open, end := quotationAround(text, m[0]); open >= 0 {
		switch quoteSpeaker(text, open, end) {
		case otherSpeaks:
			return true
		case posterSpeaks:
			return false
		}
		if end >= 0 && namedAsAKind(text, open, end) {
			return true
		}
		// Unattributed, so what is inside it is read as the poster's: a
		// report inside it is still a report ("'he told me to kys' is all
		// he said").
		_, size := utf8.DecodeRuneInString(text[open:])
		from = max(from, open+size)
	}
	who, ofPoster := reportedClause(text, from, m[0])
	if ws := wordsIn(text[m[0]:m[1]]); len(ws) > 0 && posterSubjects[ws[0]] &&
		!(len(ws) > 1 && urgingModals[ws[1]]) {
		ofPoster = true // "says I'm suicidal", the "I" matched
	}
	return who == otherSpeaks && !(ownState && ofPoster)
}

// reportReach is how far before a match reportedClause reads for a verb of
// saying, in bytes.
const reportReach = 64

// speaker is whose words a quotation or a report gives them as.
type speaker int

const (
	noSpeaker    speaker = iota // no one is named
	posterSpeaks                // the poster: "I said", "me:", "- me"
	otherSpeaks                 // someone else, or the words came to the poster
)

// quoteSpeaker says whose words the quotation opened at open (and closed
// where its closing mark ends, at end, or -1) are: from a label before it
// ("teacher:", "me:", "his dm:"), an attribution after a dash ("- me", "—
// my mum"), or a verb of saying, asking, writing or coming across within
// four words before it or six after it ("he said", "told me", "got a message
// saying", "'…' I said", "'…' said my mum", "'…' was what his message said").
func quoteSpeaker(text string, open, end int) speaker {
	lead := text[max(0, open-64):open]
	b := quotationBreak(lead)
	if strings.TrimSpace(lead[b+1:]) == "" && b >= 0 && lead[b] == '\n' {
		// A label on a line of its own, the message pasted under it: "this
		// guy sent me:\n'…'".
		i := open - len(lead) + b
		prev := text[max(0, i-64):i]
		if p := prev[quotationBreak(prev)+1:]; strings.HasSuffix(strings.TrimRight(p, " \t"), ":") {
			lead = p
			b = -1
		}
	}
	lead = lead[b+1:]
	if t := strings.TrimRight(lead, " \t"); strings.HasSuffix(t, ":") {
		// A label names who speaks, and whoever comes after "to", "for" or
		// "dear" in it is spoken to: "me to him:", "my bully to me:", "note
		// to my bully:", "psa for everyone:", "dear class 10B:" (the last
		// three the poster's, named by no one). A verb of saying makes it a
		// clause, read wherever the verb is in it: "he messaged me:", "she
		// said to me:", "some guy messaged me this at 2am:".
		ws := wordsIn(t)
		if k := lastSayingVerb(ws); k >= 0 {
			return speakerOf(ws[:k], bare(ws[k]), ws[k+1:])
		}
		ws = ws[max(0, len(ws)-4):]
		addressed := false
		for k, w := range ws {
			if is(bare(w), "to") || is(bare(w), "for") || is(bare(w), "dear") {
				ws, addressed = ws[:k], true
				break
			}
		}
		who := labelSpeaker(ws, true)
		if addressed && who == otherSpeaks && !namesSomeoneElse(ws) {
			who = noSpeaker // "note to my bully:", a note no one else is said to have sent
		}
		if who != noSpeaker {
			return who
		}
	}
	var after []string
	if end >= 0 {
		tail := text[end:min(len(text), end+48)]
		if j := strings.IndexAny(tail, ".!?\n"); j >= 0 {
			tail = tail[:j]
		}
		if t := strings.TrimLeft(tail, " "); strings.HasPrefix(t, "-") || strings.HasPrefix(t, "~") {
			if who := attributionSpeaker(wordsIn(t)); who != noSpeaker {
				return who
			}
		}
		after = wordsIn(tail)
	}
	before := wordsIn(lead)
	for k := len(before) - 1; k >= max(0, len(before)-4); k-- {
		w := bare(before[k])
		// "goes", "went" and "like" say what someone said only straight
		// before it: "one guy going 'add me on snap'", "she was like '…'".
		if sayingNoun(before, k) {
			continue // "his reply", "my post": a noun
		}
		if sayingVerbs[w] || heardVerbs[w] || receivedVerbs[w] || quoteVerbs[w] && (k == len(before)-1 ||
			k == len(before)-2 && is(bare(before[k+1]), "like")) {
			return speakerOf(before[:k], w, before[k+1:])
		}
	}
	for k := 0; k < min(len(after), 6); k++ {
		w := bare(after[k])
		if !sayingVerbs[w] || sayingNoun(after, k) {
			continue
		}
		// "'…' he said", "'…' was what my mum said"; or, put after the
		// verb, "'…' said my mum", "'…' said I". A passive's subject is
		// who it was said to: "'…' - you have been warned".
		passive := k > 0 && passiveLeads[bare(after[k-1])]
		if who := labelSpeaker(after[:k], true); who != noSpeaker && !passive {
			return who
		}
		if who := labelSpeaker(after[k+1:min(len(after), k+3)], false); who != noSpeaker {
			return who
		}
		if participles[w] && k == 0 {
			return otherSpeaks // "'kys' written on my desk"
		}
		return noSpeaker
	}
	return noSpeaker
}

// quotationBreak is where the stretch before a quotation stops being about
// it: the last sentence end, line break or quotation mark in s, or -1. A
// label or a verb further back belongs to something else.
func quotationBreak(s string) int {
	for j := len(s); j > 0; {
		r, size := utf8.DecodeLastRuneInString(s[:j])
		j -= size
		switch r {
		case '.', '!', '?', '\n', '"', '“', '”', '«', '»', '„', '「', '」', '『', '』':
			return j
		case '\'':
			prev, _ := utf8.DecodeLastRuneInString(s[:j])
			next, _ := utf8.DecodeRuneInString(s[j+size:])
			if (!alnum(prev) || !alnum(next)) && !elided(s, j) && !(j > 0 && unicode.IsDigit(prev)) {
				return j
			}
		}
	}
	return -1
}

// labelSpeaker reads who a label or an attribution names, in words: from the
// last word back when last is set ("my mum:", "his dm:"), else from the
// first on ("- me", "— some guy"). "Me", "I" and the poster's own reply are
// the poster; a person, or a message someone else sent, is someone else.
// Anything else, a source such as "news" or "police" among it, names no one.
func labelSpeaker(ws []string, last bool) speaker {
	for n := 0; n < len(ws); n++ {
		k := n
		if last {
			k = len(ws) - 1 - n
		}
		w := bare(ws[k])
		mine := k > 0 && posterOwners[bare(ws[k-1])]
		switch {
		case posterLabels[w]:
			return posterSpeaks
		case messageNouns[w]:
			if mine {
				return posterSpeaks // "my reply:"
			}
			return otherSpeaks
		case otherSpeakers[w]:
			return otherSpeaks
		}
	}
	return noSpeaker
}

// lastSayingVerb is the index of the last verb of saying, hearing or
// receiving among ws, as a verb rather than a noun ("his reply" is a
// noun), or -1.
func lastSayingVerb(ws []string) int {
	for k := len(ws) - 1; k >= 0; k-- {
		if w := bare(ws[k]); (sayingVerbs[w] || heardVerbs[w] || receivedVerbs[w]) && !sayingNoun(ws, k) {
			return k
		}
	}
	return -1
}

// namesSomeoneElse reports a person among ws, or something sent that is
// said to be someone's: "my bully", "his message", "kabir's note".
func namesSomeoneElse(ws []string) bool {
	for k, w := range ws {
		w = bare(w)
		if otherSpeakers[w] || messageNouns[w] && k > 0 && (otherSpeakers[bare(ws[k-1])] ||
			strings.HasSuffix(bare(ws[k-1]), "'s") || nounLeads[bare(ws[k-1])] && !articles[bare(ws[k-1])]) {
			return true
		}
	}
	return false
}

// articles say only that what follows is a thing, not whose it is.
var articles = canonSetOf(`a an the this that`)

// attributionSpeaker reads who an attribution after a dash names, from the
// words after the dash: the poster ("- me", "— me, deadly serious", a lone
// "- I") or someone else ("— my mum", "- some guy in my class"), named
// first, after at most a word or two that only say which ("my", "some",
// "a random"). Anything else after a dash is the poster going on, not an
// attribution: "— should I tell someone?", "- I reported it", "— am I
// overreacting?", and so is the reader or everyone ("- you know who you
// are", "- nobody is safe") and a pronoun that a verb follows ("- they
// know what they did", "- he's next").
func attributionSpeaker(ws []string) speaker {
	for k, w := range ws[:min(len(ws), 3)] {
		w = bare(w)
		switch {
		case posterLabels[w] && (!is(w, "i") || len(ws) == 1):
			return posterSpeaks
		case notSources[w]:
			return noSpeaker
		case otherPronouns[w] && (strings.Contains(w, "'") || k+1 < len(ws) && pronounVerbs[bare(ws[k+1])]):
			return noSpeaker
		case otherSpeakers[w] || messageNouns[w]:
			return otherSpeaks
		case k < 2 && (nounLeads[w] || posterOwners[w] || attributionLeads[w]):
			continue
		}
		break
	}
	return noSpeaker
}

// speakerOf says who the verb of saying (or of coming across) verb is said
// of, from the words before it in its sentence, nearest first, and rest, the
// words after it. Said to the poster ("was told", "got asked") or come
// across by them ("heard", "saw", "got"), the words are someone else's; an
// "I" or a "we" makes them the poster's, and anyone else, someone else's. A
// verb with no subject is the poster's — an instruction or an "I" left out
// ("just tell him", "said it to his face") — unless it was said to them:
// "told me".
func speakerOf(before []string, verb string, rest []string) speaker {
	n := len(before)
	// "you heard me": the poster standing by their own words.
	if heardVerbs[verb] && len(rest) > 0 && is(bare(rest[0]), "me") &&
		(n == 0 || secondPerson[bare(before[n-1])] || is(bare(before[n-1]), "you")) {
		return posterSpeaks
	}
	if heardVerbs[verb] || receivedVerbs[verb] || n > 0 && passiveLeads[bare(before[n-1])] && participles[verb] {
		return otherSpeaks
	}
	// "you should tell him", "u could just say": the poster telling the
	// reader what to say.
	k := n
	for k > 0 && subjectSkips[bare(before[k-1])] && !instructionModals[bare(before[k-1])] {
		k--
	}
	if k > 0 && instructionModals[bare(before[k-1])] && (k == 1 || secondPerson[bare(before[k-2])]) {
		return posterSpeaks
	}
	named := false
	for k := n - 1; k >= max(0, n-8); k-- {
		w := bare(before[k])
		switch {
		case posterSubjects[w] || posterLeads[w] || mindWords[w]:
			return posterSpeaks // "I said"; "imma tell him"; "my head keeps telling me to"
		case otherSpeakers[w] || messageNouns[w]:
			return otherSpeaks
		case !subjectSkips[w] && w != "":
			named = true // "Rahul said", "the guy next to me said"
		}
	}
	if named || len(rest) > 0 && receivers[bare(rest[0])] {
		return otherSpeaks
	}
	return posterSpeaks
}

// reportedClause reports words at i that a verb of saying before them, in
// the same sentence and within reportReach bytes (none before from), gives
// to someone else: "my bf told me to go kill myself", "people keep saying
// you should all kys", "he said he'll shoot up the school", "usne bola mar
// ja", "he said: kys". Only the words a report puts between the verb and
// what was said may come between them — who it was said to, "to", "that", a
// subject, a modal — and a comma ends the report: "didn't ask, kys".
// ofPoster says the poster is the subject of what was said, with no modal
// urging it on them: "says I'm suicidal", not "said I should just die".
func reportedClause(text string, from, i int) (who speaker, ofPoster bool) {
	before := text[max(from, i-reportReach):i]
	before = before[strings.LastIndexAny(before, ".!?\n,;")+1:]
	ws := wordsIn(before)
	for k := len(ws) - 1; k >= max(0, len(ws)-6); k-- {
		w := bare(ws[k])
		switch {
		case (sayingVerbs[w] || heardVerbs[w]) && !sayingNoun(ws, k):
			return speakerOf(ws[:k], w, ws[k+1:]), ofPoster
		case !reportedLinks[w]:
			return noSpeaker, false
		case posterSubjects[w] && !(k+1 < len(ws) && urgingModals[bare(ws[k+1])]):
			ofPoster = true
		}
	}
	return noSpeaker, false
}

// namedAsAKind reports a quotation used as the name of a kind of post, not
// said: "the 'add me on snap' guys", "every 'kys' comment", "stuff like
// 'dm me'".
func namedAsAKind(text string, open, end int) bool {
	before := wordsBefore(text, open, 1)
	if len(before) > 0 && quoteLeads[bare(before[0])] {
		return true
	}
	after := nextWords(text[end:min(len(text), end+32)], 2)
	if len(after) == 0 || !quoteKinds[after[0]] {
		return false
	}
	// "guys" and "people" after a quotation are as often the poster
	// calling to the readers ("'…' guys this is not a joke", "'…' people,
	// i'm serious"): they name a kind only going on into a clause about
	// it, "'add me on snap' guys are so annoying".
	if !quoteVocatives[after[0]] {
		return true
	}
	rest := text[end:min(len(text), end+32)]
	rest = rest[max(0, strings.IndexFunc(rest, func(r rune) bool { return unicode.IsLetter(r) })):]
	if j := strings.IndexFunc(rest, func(r rune) bool { return !wordRune(r) }); j >= 0 {
		rest = rest[j:]
	}
	return len(after) > 1 && kindClauses[after[1]] && !strings.HasPrefix(strings.TrimLeft(rest, " "), ",")
}

// mentionedAsAKind reports words named as a kind of post by the word after
// them: "the kys comments", "suicide jokes".
func mentionedAsAKind(next []string) bool { return len(next) > 0 && mentionNouns[next[0]] }

// nounBefore reports a word that makes the one after it a noun, when that
// one could be a verb of saying as well: "my post", "his reply", "her
// brother's message".
func nounBefore(w string) bool {
	w = bare(w)
	return nounLeads[w] || strings.HasSuffix(w, "'s") && !posterSubjects[w] && !otherSpeakers[w]
}

// sayingNoun reports a verb of saying at ws[k] that is a noun there: a word
// that can be either ("post", "reply", "message"), after a word that makes
// it the noun.
func sayingNoun(ws []string, k int) bool {
	return k > 0 && nounVerbs[bare(ws[k])] && nounBefore(ws[k-1])
}

// bare is a word without the quotation marks wordsIn leaves on its ends.
func bare(w string) string { return strings.Trim(w, "'") }

var (
	// sayingVerbs say who said something, wrote it, asked it or sent it, in
	// English and in Hinglish ("bola", "kaha", "likha", "pucha").
	sayingVerbs = canonSetOf(`say says saying said tell tells telling told write writes writing wrote written
		post posts posting posted comment comments commenting commented reply replies replying replied
		text texts texting texted message messages messaging messaged dm dms dming dmed dm'd ask asks asking asked
		type types typing typed tweet tweets tweeted send sends sending sent shout shouts shouting shouted
		yell yells yelling yelled scream screams screaming screamed whisper whispers whispered
		threaten threatens threatening threatened warn warns warned claim claims claimed state states stated
		spam spams spammed spamming captioned scrawled
		bola boli bole bolta bolti bolte bolke kaha kehta kehti kehte keh likha likhta likhti likhte pucha puchha
		poocha puchta puchti`)
	// quoteVerbs say what someone said only straight before a quotation.
	quoteVerbs = canonSetOf(`go goes going went like`)
	// heardVerbs are the poster coming across words, someone else's; and
	// receivedVerbs, straight before a quotation, the poster sent them: "got
	// '…' in my dms".
	heardVerbs    = canonSetOf(`heard hear hears saw seen read reads found noticed received`)
	receivedVerbs = canonSetOf(`got get gets getting receive receives`)
	// urgingModals, after the poster as a subject, make what was said an
	// urging: "said I should".
	urgingModals = canonSetOf(`should shud must need needs gotta better deserve deserved ought`)
	// participles, after passiveLeads, are words said to the poster: "was
	// told", "got asked", "keep getting sent".
	participles  = canonSetOf(`told asked sent called messaged texted dmed written posted threatened warned typed`)
	passiveLeads = canonSetOf(`was were got get gets getting been being be is are am 'm im`)
	// nounVerbs are the verbs of saying that are nouns as well, which
	// nounLeads before them make the noun: "my post", "his reply".
	nounVerbs = canonSetOf(`post posts comment comments reply replies text texts message messages dm dms tweet
		tweets type types spam claim claims state states`)
	nounLeads = canonSetOf(`my our your his her their its the a an this that these those every each some any`)
	// mindWords are the poster's own mind saying something to them, which
	// is the poster's crisis: "my head keeps telling me to", "the voices
	// tell me", "my brain says".
	mindWords = canonSetOf(`head brain mind thoughts thought voice voices heart gut depression anxiety demons
		intrusive`)
	// receivers, straight after a verb with no subject, say it was said to
	// the poster: "told me to".
	receivers = canonSetOf(`me us mujhe humko`)
	// instructionModals before a verb of saying make it the poster's
	// instruction: "you should say", "just tell him".
	instructionModals = canonSetOf(`should shud must gotta could can`)
	// posterSubjects are the poster as a subject; posterLabels the poster
	// named in a label or an attribution, and posterOwners as an owner.
	posterSubjects = canonSetOf(`i i'm im i've ive i'd i'll we we're we've we'll we'd maine mene mainne humne hamne`)
	posterLabels   = canonSetOf(`me i myself mine moi`)
	posterOwners   = canonSetOf(`my our mera meri mere`)
	// posterLeads carry the poster as their subject: "imma tell him",
	// "lemme text her".
	posterLeads = canonSetOf(`imma ima i'ma lemme`)
	// otherSpeakers are someone other than the poster: people, by pronoun,
	// role or relation.
	otherSpeakers = canonSetOf(`he she they he's she's they're hes shes theyre he'd she'd they'd he'll she'll
		they'll him her them you u ya y'all yall someone somebody anyone anybody everyone everybody nobody noone
		whoever people ppl person guy guys girl girls boy boys kid kids man men woman women dude dudes lad lads
		child children teen teens friend friends mate mates bestie classmate classmates teacher teachers sir maam
		ma'am miss mr mrs ms principal headteacher prof professor lecturer tutor coach mum mom mummy mother dad
		daddy father papa parents parent brother sister sis sibling siblings cousin cousins uncle aunt aunty auntie
		bf gf boyfriend girlfriend ex crush partner bully bullies stranger strangers senior seniors junior juniors
		student students anon anons op poster user users troll trolls account accounts
		ladka ladki ladke log usne unhone uska uski unka woh wo vo voh dost`)
	// messageNouns are something someone sent or wrote: "a message saying",
	// "the note said", "his bio:".
	messageNouns = canonSetOf(`message messages msg msgs text texts dm dms note notes comment comments caption bio
		post posts reply replies email emails mail letter letters sms notification notifications tweet tweets
		story stories snap snaps screenshot screenshots ss sign graffiti answer response`)
	// subjectSkips are the words between a verb of saying and its subject
	// that are not the subject: auxiliaries, adverbs, fillers, prepositions
	// and the poster as an object or owner ("the guy next to me", "my mum").
	subjectSkips = canonSetOf(`keeps keep kept kinda always just literally also even still actually really would will
		'd 'll did does do doesn't don't didn't has have had who that which then and or but lol lmao lmfao bro ngl
		tbh fr so ok okay yeah honestly basically should shud must gotta could can might may once twice never ever randomly suddenly straight right back
		used to my our your his their its the a an this these those in on at of from with under next by about
		after before for me us mujhe mere mera meri roz hamesha bhi ne ko phir fir aur ki ke ka hai tha thi the
		haha hahaha omg oh yo hey well like legit lowkey highkey deadass fucking even why how what
		already seriously totally definitely def finally recently today yesterday earlier apparently openly
		publicly clearly officially proudly casually calmly quietly loudly simply
		gonna gunna gon finna tryna wanna going to tomorrow tmrw tmr tonight later now up`)
	// reportedLinks are the words a report puts between a verb of saying and
	// what was said: "told me to go", "said that you should all", "said
	// he'll", "bolta hai ki tu".
	reportedLinks = canonSetOf(`to go just that should shud could would must gotta better need needs have has had can
		will might gonna going was were is are be 'm i i'd i'm im i'll you u ur you'd you'll youd youll he she they
		he'd she'd they'd he'll she'll they'll he's she's me him her them us everyone everybody all someone people
		ppl a an the kid kids guy girl person should've shouldve please pls plz literally hai ki ke tu tum aap
		mujhe usko unko ko jaa ja i've ive if whether`)
	// attributionLeads say which person an attribution after a dash names
	// before it names them: "— some random guy", "- one of the teachers".
	attributionLeads = canonSetOf(`some random one certain other another of`)
	// notSources are never who a quotation is from: the reader, everyone
	// and no one. otherPronouns are a source only on their own ("- him"),
	// not as the subject of one of pronounVerbs ("- they know who they
	// are").
	notSources    = canonSetOf(`you u ya y'all yall nobody noone everyone everybody anyone anybody`)
	otherPronouns = canonSetOf(`he she they him her them hes shes theyre he's she's they're he'd she'd they'd
		he'll she'll they'll`)
	pronounVerbs = canonSetOf(`is are was were will has have had can could should would must might may know knows
		knew think thinks did does do don't dont doesn't doesnt didn't didnt can't cant won't wont deserve
		deserves better never always gonna`)
	// quoteLeads, before a quotation, and quoteKinds, after it, make it the
	// name of a kind of post.
	quoteLeads = canonSetOf(`the a those these every all like`)
	quoteKinds = canonSetOf(`guys people posts post thing stuff spam bots comments comment crowd requests
		messages types kind kinds replies`)
	// quoteVocatives are the quoteKinds that are also a call to the reader,
	// and kindClauses what goes on to say something about a kind.
	quoteVocatives = canonSetOf(`guys people`)
	kindClauses    = canonSetOf(`are r is were was who that which keep keeps always never get gets need should
		deserve can't cant won't wont don't dont do make makes have has`)
)

// quotationAround finds the quotation offset i is in, within 120 bytes
// either side on its line: where its opening mark starts and where its
// closing mark ends, or -1. Every kind of quotation mark opens one: straight
// and curly double quotes, straight single quotes (which curly ones are
// read as, since the view folds them), guillemets, low-9 quotes and corner
// brackets. A straight double quote opens one only when an even number of
// them come before it on the line, since the same mark closes it. An
// apostrophe opens a quotation only after something that is not a letter or
// a digit, and closes one only before such a thing, so the ones inside
// "what's" and "ma'am" are neither; nor is one that stands for letters or
// digits left out ("'cause", "'til", "'n", "the class of '09"), nor a mark
// of feet and inches ("5'10\"").
func quotationAround(text string, i int) (int, int) {
	lo := max(0, i-120)
	for j := i; j > lo; {
		r, size := utf8.DecodeLastRuneInString(text[lo:j])
		j -= size
		switch r {
		case '\n':
			return -1, -1
		case '"':
			if inchMark(text, j) {
				continue
			}
			if doubleQuotesBefore(text, j)%2 == 1 {
				return -1, -1 // it closes one
			}
			return j, closingMark(text, i, r)
		case '“', '«', '„', '‚', '「', '『', '‹':
			return j, closingMark(text, i, r)
		case '”', '»', '」', '』', '›':
			return -1, -1
		case '\'':
			prev, _ := utf8.DecodeLastRuneInString(text[:j])
			next, _ := utf8.DecodeRuneInString(text[j+size:])
			if j > 0 && unicode.IsDigit(prev) || elided(text, j) {
				continue
			}
			if j > 0 && prev == '\'' {
				// A doubled mark, '' or `` (and ″, which the view folds to
				// two): the quotation opens at the first of the two.
				return j - 1, closingMark(text, i, r)
			}
			if j == 0 || !alnum(prev) {
				return j, closingMark(text, i, r)
			}
			if !alnum(next) {
				return -1, -1 // a closing one: the quotation ended before i
			}
		}
	}
	return -1, -1
}

// doubleQuotesBefore counts the straight double quotes before offset j on
// its line, within 240 bytes, leaving out marks of inches.
func doubleQuotesBefore(text string, j int) int {
	from := max(0, j-240)
	from += strings.LastIndexByte(text[from:j], '\n') + 1
	n := 0
	for k := from; k < j; k++ {
		if text[k] == '"' && !inchMark(text, k) {
			n++
		}
	}
	return n
}

// inchMark reports that the straight double quote at text[j] marks inches
// after feet, "5'10\"", not a quotation.
func inchMark(text string, j int) bool {
	k := j
	for k > 0 && k > j-2 && text[k-1] >= '0' && text[k-1] <= '9' {
		k--
	}
	return k < j && k >= 2 && text[k-1] == '\'' && text[k-2] >= '0' && text[k-2] <= '9'
}

// elided reports that the apostrophe at text[j] stands for what was left
// out of the start of a word or a year: "'cause", "'til", "'em", "'n",
// "'09", "the '90s".
func elided(text string, j int) bool {
	k := j + 1
	for k < len(text) && k < j+8 && (text[k] >= 'a' && text[k] <= 'z' || text[k] >= '0' && text[k] <= '9') {
		k++
	}
	if k < len(text) && (text[k] >= 'a' && text[k] <= 'z' || text[k] >= '0' && text[k] <= '9' || text[k] == '\'') {
		return false // a longer word, or one closed at once: "'cause'"
	}
	w := text[j+1 : k]
	if len(w) >= 2 && w[0] >= '0' && w[0] <= '9' && w[1] >= '0' && w[1] <= '9' {
		return len(w) == 2 || w[2:] == "s"
	}
	// "'sup, how old are u? … dont tell ur parents'" opens a quotation
	// that a mark further on the line closes: only an apostrophe with no
	// closing one after it is an elision ("tell 'em i said hi").
	return elisions[w] && !closedOnLine(text, k)
}

// closedOnLine reports that the first quotation apostrophe from offset k of
// text on its line, within 120 bytes, closes a quotation: a letter or a
// digit before it and none after ("parents' wtf"). An apostrophe inside a
// word ("dont", "i'm") is passed over, and one that opens another
// quotation, or none at all, closes nothing.
func closedOnLine(text string, k int) bool {
	hi := min(len(text), k+120)
	for p := k; p < hi; p++ {
		switch text[p] {
		case '\n':
			return false
		case '\'':
			prev, _ := utf8.DecodeLastRuneInString(text[:p])
			next, _ := utf8.DecodeRuneInString(text[p+1:])
			if alnum(prev) && alnum(next) {
				continue
			}
			return alnum(prev)
		}
	}
	return false
}

// elisions are words that begin with an apostrophe, for what was left out.
var elisions = canonSetOf(`cause cos coz cuz til till n em bout round nother kay sup tis twas ere ave ello`)

// closingMark is where the mark closing a quotation opened with open ends,
// searching from offset i, or -1.
func closingMark(text string, i int, open rune) int {
	closes := map[rune]string{'"': `"`, '“': `”"`, '«': `»`, '\'': `'`, '„': `“”"`, '‚': `'`,
		'「': `」`, '『': `』`, '‹': `›`}[open]
	hi := min(len(text), i+120)
	for j := i; j < hi; {
		r, size := utf8.DecodeRuneInString(text[j:hi])
		if r == '\n' {
			return -1
		}
		if strings.ContainsRune(closes, r) && !(r == '"' && inchMark(text, j)) {
			next, _ := utf8.DecodeRuneInString(text[j+size:])
			if r == '\'' && next == '\'' {
				return j + 2 // a doubled close: ''
			}
			if r != '\'' || j+size >= len(text) || !alnum(next) {
				return j + size
			}
		}
		j += size
	}
	return -1
}

// alnum reports a letter or a digit, as unicode.IsLetter and IsDigit do,
// from a table for the first two planes, where every character a post is
// likely to be made of is: the word readers ask it of every character they
// pass, and an emoji's answer from the unicode tables costs a binary search.
func alnum(r rune) bool {
	if uint32(r) < alnumTableSize {
		return alnumTable[r>>6]&(1<<(r&63)) != 0
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

const alnumTableSize = 0x20000

var alnumTable = func() *[alnumTableSize / 64]uint64 {
	var t [alnumTableSize / 64]uint64
	for r := rune(0); r < alnumTableSize; r++ {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			t[r>>6] |= 1 << (r & 63)
		}
	}
	return &t
}()

// glossed reports a word being explained rather than said: "kys = keep
// yourself safe?", "kys means", "what does kys mean".
func glossed(text string, m []int) bool {
	after := strings.TrimLeft(text[m[1]:min(len(text), m[1]+24)], " ")
	if strings.HasPrefix(after, "=") {
		return true
	}
	next := wordsIn(after)
	if len(next) > 0 && glossWords[next[0]] {
		return true
	}
	before := wordsIn(text[max(0, m[0]-24):m[0]])
	return len(before) >= 2 && is(before[len(before)-1], "does") && is(before[len(before)-2], "what")
}

var (
	glossWords = canonSetOf(`means mean meant stands`)
	kysForm    = regexp.MustCompile(`^k+y+s+(?:ing|in|ed)$`)
)

// deathAround reports a post about dying near offset m: within 120
// characters either side.
func deathAround(text string, m []int) bool {
	ws := wordsIn(text[max(0, m[0]-120):min(len(text), m[1]+120)])
	for i, w := range ws {
		// "you die of embarrassment" and "dead laughing" are no death.
		if deathTopic[w] && !dieIdiom(ws[i+1:]) {
			return true
		}
	}
	return false
}

// saidFirst reports that an imperative is the first thing said in its
// clause: nothing before it but fillers ("just", "bro", "lol") and the links
// to the post it answers (">>51290"), or, after "and" or "then", what came
// before is not about the poster: "stop threatening it and just off
// yourself", "do us all a favour and jump off the roof", but not "i'd rather
// drink bleach" or "i went home and just died". A condition said of the
// reader, with "just" after it, opens the clause as a comma would: "if ur
// that stressed just jump off the roof", "if you failed that badly just
// end it".
func saidFirst(before string) bool {
	ws := wordsIn(quoteLink.ReplaceAllString(before, " "))
	if anyIn(ws, firstPersonSubjects) {
		return false
	}
	for i := len(ws) - 1; i >= 0; i-- {
		if is(ws[i], "and") || is(ws[i], "then") {
			return true
		}
		if !imperativeFillers[ws[i]] {
			return i+1 < len(ws) && is(ws[i+1], "just") && len(ws) > 2 && conditionWords[ws[0]] &&
				anyIn(ws[1:i+1], secondPerson)
		}
	}
	return true
}

// endsSomethingElse reports a word of endItOthers in ws, what "just end it"
// may end, unless it judges (endItJudges): "our group chat voted" and "the
// whole class agrees" are people telling the reader to.
func endsSomethingElse(ws []string) bool {
	for i, w := range ws {
		if endItOthers[w] && !(i+1 < len(ws) && endItJudges[ws[i+1]]) {
			return true
		}
	}
	return false
}

// namesWhatIsEnded reports that the clause next to "just end it" or "you
// should end it" opens on what is being ended, and it is a lecture, an essay
// or a season rather than a life: "just end it already, this lecture is 3
// hours long", "you should end it now, the essay is long enough", "sick of
// this chapter, just end it". The clause after the urging is read past the
// shrugs that end its own ("already", "now"), and the one before it as well
// as what comes before the urging in its own clause. A clause that speaks to
// the reader ("this exam was easy and you still failed") or that does not
// open on the thing ("failed the exam again, just end it") names nothing
// ended.
func namesWhatIsEnded(text string, m []int) bool {
	const marks = ".!?\n,;:()"
	var near []string
	before := text[max(0, m[0]-120):m[0]]
	i := strings.LastIndexAny(before, marks)
	near = append(near, before[i+1:])
	if i >= 0 {
		prev := before[:i]
		near = append(near, prev[strings.LastIndexAny(prev, marks)+1:])
	}
	after := text[m[1]:min(len(text), m[1]+120)]
	if j := strings.IndexAny(after, marks); j >= 0 {
		rest := after[j+1:]
		if k := strings.IndexAny(rest, marks); k >= 0 {
			rest = rest[:k]
		}
		near = append(near, rest)
	}
	for _, clause := range near {
		if opensOnEnded(wordsIn(clause)) {
			return true
		}
	}
	return false
}

// opensOnEnded reads a clause's words for namesWhatIsEnded: past any
// imperativeFillers, an endItThings word, or a determiner and one within two
// words after it, the determiner opening the clause or straight after an "of"
// in its first four words ("sick of this lecture"); and no one spoken to.
func opensOnEnded(ws []string) bool {
	if anyIn(ws, readerWords) || anyIn(ws, secondPerson) {
		return false
	}
	for len(ws) > 0 && imperativeFillers[ws[0]] {
		ws = ws[1:]
	}
	if len(ws) == 0 {
		return false
	}
	if endItThings[ws[0]] {
		return endedClass(ws, ws[0])
	}
	for k := 0; k < min(len(ws), 5); k++ {
		if !endItDeterminers[ws[k]] || k > 0 && !is(ws[k-1], "of") {
			continue
		}
		for _, w := range ws[k+1 : min(len(ws), k+4)] {
			if endItThings[w] {
				return endedClass(ws, w)
			}
		}
		return false
	}
	return false
}

// endedClass reports a clause opening on thing as about the thing ended. A
// class is a lesson only said to go on ("the class is 3 hours long", "this
// class never ends"); judged or feeling ("the whole class agrees", "the
// class would be better off") it is the people in it.
func endedClass(ws []string, thing string) bool {
	if !is(thing, "class") && !is(thing, "classes") {
		return true
	}
	return anyIn(ws, lessonLengths)
}

var (
	// lessonLengths say a class goes on: "is 3 hours", "never ends", "is
	// dragging".
	lessonLengths = canonSetOf(`hours hour hrs hr minutes mins long longer forever endless never ends ending
		dragging drags dragged boring over overtime`)
	// endItThings are what "just end it" may be said of on a student board
	// when the clause beside it names them: a class, a piece of work, a
	// meeting, something watched or read.
	endItThings = canonSetOf(`lecture lectures lec class classes period periods chapter chapters term terms semester
		semesters sem sems exam exams paper papers test tests quiz essay essays assignment assignments homework hw
		syllabus portion portions meeting meetings call calls movie movies film films video videos book books novel
		season seasons episode episodes ep anime manga arc project projects presentation presentations speech lesson
		lessons module modules unit units session sessions practice rehearsal seminar webinar lab labs viva shift
		training workshop tournament round podcast stream livestream song playlist`)
	endItDeterminers = canonSetOf(`this that the these those my our`)
)

// conditionWords open a condition: "if", "agar".
var conditionWords = canonSetOf(`if agar`)

var (
	quoteLink           = regexp.MustCompile(`>>+\d+`)
	firstPersonSubjects = canonSetOf(`i i'm im i'll i'd i've ive we we're we'll we've`)
	jumpVerbs           = canonSetOf(`jump leap dive flip backflip do swan`)
	// endItOthers, before "just end it", make it a relationship, an
	// argument or a game.
	endItJudges = canonSetOf(`voted votes vote agrees agree agreed says said say thinks think decided decides
		reckons reckon laughed laughs`)
	endItOthers = canonSetOf(`her him she he they them boyfriend girlfriend bf gf relationship partner ex crush
		friendship marriage situationship cheating cheated cheats dating breakup thread debate argument discussion
		convo conversation chat game match series show beef fight drama story subscription streak course courses
		degree program programme coaching tuition membership internship job`)
	pillRoutine = canonSetOf(`on with before after daily regularly every at as properly in for together`)
	// mentionNouns, after encouragement, make it the name of a kind of post.
	mentionNouns = canonSetOf(`comments comment jokes joke posts post thing stuff culture spam spammers replies reply
		messages memes meme trend trolls bots`)
)

var (
	deeperObjects     = canonSetOf(`into in through the a an on with than to`)
	overworkModals    = canonSetOf(`you'll youll u'll ull will gonna gon`)
	imperativeFillers = canonSetOf(`just go pls please plz now then lol lmao bro bruh dude man mate honestly seriously ok
		okay so and or yeah fr ngl also you u like literally really already oh omg tbh idc well yo hey bhai yaar abe chal`)
	deathWords   = canonSetOf(`die died dead disappear disappeared vanish vanished gone killed kill kys ended jumped hanged overdosed od`)
	careFillers  = canonSetOf(`if when you u you're youre ur were was are just simply even all`)
	trailFillers = canonSetOf(`lol lmao tbh honestly fr ngl bro anyway anyways though tho`)
	// shrugs are what may follow an urging that is one: "already", "pls".
	shrugs = canonSetOf(`already pls plz please now lol lmao tbh honestly fr ngl bro anyway anyways though tho
		lmfao bruh srsly seriously`)
	diedWords = canonSetOf(`gaya gayi gaye gya gyi gye`)
)

// onlyShrugs reports that the words after an urging are none, or only
// shrugs: "just end it already lol", not "already, the series has dragged
// on", which the clause stops before anyway.
func onlyShrugs(next []string) bool {
	for _, w := range next {
		if !shrugs[w] {
			return false
		}
	}
	return true
}

// caresIfYouDie reads what follows "no one would miss you" or "nobody will
// care if you": nothing (or a shrug) or dying makes it encouragement;
// anything else is about something smaller ("if you skip the party").
func caresIfYouDie(next []string) bool {
	rest := next
	for len(rest) > 0 && trailFillers[rest[len(rest)-1]] {
		rest = rest[:len(rest)-1]
	}
	if len(rest) == 0 {
		return true
	}
	for len(rest) > 0 && careFillers[rest[0]] {
		rest = rest[1:]
	}
	// "nobody would care if you failed and killed yourself": a death in
	// the first few words.
	for i, w := range rest[:min(len(rest), 6)] {
		if deathWords[w] {
			return !dieIdiom(rest[i+1:])
		}
	}
	return false
}

// methodCheck is notNegated with the method questions that are about the
// world, not oneself, turned down: a "how much is lethal" or a "lethal dose"
// with no medicine and no "I" or "me" ("how much caffeine is lethal", "the
// lethal dose for half the test animals"), "how deep should I cut" followed
// by something to cut ("the wood"), a death in a game, and "self-doubt".
func methodCheck(v *view, m []int) bool {
	if !notNegated(v, m) || v.reported(m) {
		return false
	}
	text := v.text
	matched := text[m[0]:m[1]]
	ws := wordsIn(matched)
	if len(ws) < 2 {
		return true
	}
	next := wordsIn(clauseAfter(text, m[1]))
	if selfCompound(text, m[1]) || gameDeath(next) {
		return false
	}
	switch first := ws[0]; {
	case is(ws[len(ws)-1], "asking") || is(ws[len(ws)-1], "friend") || is(ws[len(ws)-1], "pooch") ||
		is(ws[len(ws)-1], "puch"):
		// The disclaimed question, unless it names what the pills are for.
		return !anyIn(ws, medicalWords) && !anyIn(ws, ailments)
	case is(first, "how") && (is(ws[1], "many") || is(ws[1], "much")), is(first, "what") && doseWords[ws[1]]:
		// The medicine may be named before the question: "paracetamol, how
		// many to die?"; or a height asked in floors: "how many floors to
		// die". A lethal dose asked for is asked for whatever weight or
		// study label is round it: "how many mg/kg of paracetamol is a
		// lethal dose?", "for the tox exam, how many tablets of paracetamol
		// is a lethal dose".
		sentence := sentenceAround(text, m[0], m[1])
		return namesAMedicine(v, sentence) || hasFirstPerson(ws) ||
			is(ws[1], "many") && anyIn(wordsIn(sentence), heightUnits) && fallFatal(wordsIn(sentence))
	case is(first, "lethal") || is(first, "fatal") || is(first, "deadly"):
		sentence := sentenceAround(text, m[0], m[1])
		sws := wordsIn(sentence)
		if anyIn(sws, selfWords) {
			return true
		}
		return namesAMedicine(v, text[m[1]:min(len(text), m[1]+48)]) && doseAsked(text, m)
	case is(first, "how") && is(ws[1], "deep"):
		// "how deep do you have to cut", not "the dovetail joint".
		return len(next) == 0 || deepCutTargets[next[0]]
	case is(ws[len(ws)-1], "cut"):
		// "where to cut to bleed out", "which vein to cut", not "where to
		// cut the cake" or "where do i cut my hair", or "the vein to cut"
		// in a dissection.
		if !is(first, "where") && !anyIn(wordsBefore(text, m[0], 2), whichWords) {
			return false
		}
		return len(next) == 0 || cutPlaces[next[0]] ||
			len(next) > 1 && is(next[0], "my") && cutPlaces[next[1]]
	case is(first, "cut") && hasWord(ws, "without"):
		return anyIn(wordsBefore(text, m[0], 4), methodAsks)
	case killNouns[first] && is(ws[len(ws)-2], "kill"):
		// "what pills can kill you", not "these pills can kill you if
		// mixed".
		return anyIn(wordsBefore(text, m[0], 3), whichWords)
	case is(first, "how") && is(ws[1], "long"):
		// "how long does it take to die from hanging", not "of old age".
		if is(ws[len(ws)-1], "die") {
			return len(next) > 1 && dieHow[next[0]] && anyIn(next[1:min(len(next), 3)], dyingMethods)
		}
	case is(ws[1], "high") || is(ws[1], "tall") || is(ws[len(ws)-1], "floor") || is(ws[len(ws)-1], "height") ||
		is(ws[len(ws)-1], "storey"):
		// "how high to jump to die", "which floor is fatal"; not "how high
		// to jump to dunk" or "fatal for an egg drop".
		return fallFatal(wordsIn(sentenceAround(text, m[0], m[1])))
	case is(first, "eat"):
		// "eat less than 500 calories", not "less than 2000 on rest days"
		// or "eat less junk".
		k := 0
		if len(next) > 0 && is(next[0], "than") {
			k = 1
		}
		if len(next) < k+2 || !calorieWords[next[k+1]] {
			return false
		}
		n, err := strconv.Atoi(next[k])
		return err == nil && n <= 800
	case is(ws[len(ws)-1], "eating") && hasWord(ws, "stop"):
		// "how to stop eating", "completely"; not "junk food at night" or
		// "so fast".
		return len(next) == 0 || stopEatingTrail[next[0]]

	case is(first, "for") || is(ws[len(ws)-1], "survive") || is(ws[len(ws)-1], "up"):
		// "for me to die" asks how only in a question: "which bridge is
		// high enough for me to die", not "time for me to die (in this exam)".
		if is(ws[len(ws)-1], "die") && (dieIdiom(next) || len(next) > 0 && is(next[0], "for")) {
			return false
		}
		return anyIn(wordsIn(sentenceAround(text, m[0], m[1])), methodAsks) || questionAfter(text, m[1])
	case is(first, "die") && (is(ws[len(ws)-1], "wrist") || is(ws[len(ws)-1], "wrists")):
		// "can you die from cutting your wrists", not "you can die from
		// cutting your wrists, so" or "on a tin can".
		return dieAsked(wordsBefore(text, m[0], 3)) && !hurtByAccident(next) && !accidentAhead(next)
	case len(ws) == 3 && searchKinds[first] && (is(ws[1], "to") || is(ws[1], "of") || is(ws[1], "for")):
		return searchedMethod(ws[2], next)
	case is(ws[len(ws)-1], "purge") || is(ws[len(ws)-1], "purging") ||
		searchWays[first] && (is(ws[len(ws)-1], "die") || is(ws[len(ws)-1], "dying") || is(ws[len(ws)-1], "suicide")):
		// "how to purge", "easiest way to die", with nothing after it: not
		// "how to purge the nginx cache" or "the best way to die with
		// dignity"; "how to purge" also unseen, "without anyone knowing".
		return len(next) == 0 || onlyShrugs(next) || is(ws[len(ws)-1], "purge") && unseenAfter(next)
	case is(ws[len(ws)-1], "go"):
		// "the most painless way to go", with nowhere to go to after it.
		return len(next) == 0 && (hasWord(ws, "painless") || hasWord(ws, "painful"))
	case is(ws[len(ws)-1], "it") || is(ws[len(ws)-1], "all"):
		// "the best way to end it with my girlfriend"
		return len(next) == 0 || !is(next[0], "with") && !is(next[0], "between")
	}
	return true
}

// doseAsked reports a lethal dose of a medicine asked for, whatever weight or
// study word is near it: as a question ("what's a lethal dose of
// paracetamol", "lethal dose of paracetamol per kg?", "pharmacology
// question: lethal dose of paracetamol?"), or on its own as a search is
// ("lethal dose of ibuprofen"); and not a fact about the world: a statement
// with its verb ("the lethal dose of paracetamol is about 150 mg/kg", "the
// fatal dose varies with liver health"), a "why" asked about it ("Nursing
// exam: why is the lethal dose of insulin so unpredictable?"), or a topic
// named after a studyLabel ("For my pharmacology viva: lethal dose of
// morphine in opioid-naive patients vs tolerant ones"), though not the dose
// named bare after one ("pharmacology: lethal dose of paracetamol").
func doseAsked(text string, m []int) bool {
	labelled, clause := studyLabel(sentenceBefore(text, m[0]))
	if len(clause) > 0 && is(clause[0], "why") {
		return false
	}
	if questionAfter(text, m[1]) || len(clause) > 0 && doseQuestions[clause[0]] {
		return true
	}
	rest := text[m[1]:min(len(text), m[1]+64)]
	if j := strings.IndexAny(rest, ".!?\n"); j >= 0 {
		rest = rest[:j]
	}
	after := wordsIn(rest)
	if anyIn(after, doseStatements) {
		return false
	}
	// "of paracetamol", "of my mum's medicine", "of sleeping pills per kg":
	// the dose, what it is of and a weight, and nothing about it.
	for len(after) > 0 && (doseOf[after[0]] || possessives[after[0]] || strings.HasSuffix(after[0], "'s")) {
		after = after[1:]
	}
	for len(after) > 0 && perWeight[after[len(after)-1]] {
		after = after[:len(after)-1]
	}
	return !labelled || len(after) <= 2
}

// sentenceBefore is the sentence before text[i], within 64 bytes.
func sentenceBefore(text string, i int) string {
	before := text[max(0, i-64):i]
	return before[strings.LastIndexAny(before, ".!?\n")+1:]
}

// studyLabel reads the start of a sentence, before a lethal dose, for a label
// naming the subject it is studied for: a few words with one of studyFrames
// among them and a colon or a dash after them ("Nursing exam:", "Pharmacology
// -"), or a few opening on "for" and closed by a comma ("for the tox exam,").
// It returns whether there is one and the words after it, or all the words
// when there is none. A study word anywhere else is no label: "failed my
// exam. what's the lethal dose", "after these exam results, what's the lethal
// dose", "lethal dose of my medicine".
func studyLabel(before string) (bool, []string) {
	j, k := strings.IndexAny(before, ":\u2013\u2014"), 0
	if j >= 0 {
		_, k = utf8.DecodeRuneInString(before[j:])
	} else if j = strings.Index(before, " - "); j >= 0 {
		k = 3
	} else if ws := wordsIn(before); len(ws) > 0 && is(ws[0], "for") {
		if j = strings.IndexByte(before, ','); j >= 0 {
			k = 1
		}
	}
	if j >= 0 {
		if label := wordsIn(before[:j]); len(label) <= 5 && anyIn(label, studyFrames) {
			return true, wordsIn(before[j+k:])
		}
	}
	return false, wordsIn(before)
}

// searchedMethod reads what a search for ways, methods or tips asks after,
// from its first word w and the words after it: "ways to die", with nothing
// after it or a way of dying ("painlessly", "in my sleep"), "methods of
// suicide" with nothing after it, and "tips for killing myself", "ending my
// life", "ending it all", "hanging myself", "starving myself" and "self
// harm"; not "ways to die in minecraft", "methods of suicide prevention",
// "ways to kill time" or "tips for ending the essay".
func searchedMethod(w string, next []string) bool {
	nothing := len(next) == 0 || onlyShrugs(next)
	self := len(next) > 0 && (selfObjects[next[0]] || len(next) > 1 && possessives[next[0]] && is(next[1], "self"))
	switch {
	case is(w, "die"):
		return nothing || dyingWays[next[0]] || len(next) > 1 && is(next[0], "without") && is(next[1], "pain") ||
			len(next) > 2 && is(next[0], "in") && possessives[next[1]] && is(next[2], "sleep")
	case is(w, "dying") || is(w, "suicide") || is(w, "purge") || is(w, "purging"):
		return nothing
	case is(w, "commit") || is(w, "committing"):
		return len(next) > 0 && is(next[0], "suicide")
	case is(w, "end") || is(w, "ending"):
		return len(next) > 1 && (possessives[next[0]] || is(next[0], "one's") || is(next[0], "ones")) &&
			is(next[1], "life") || len(next) > 1 && is(next[0], "it") && is(next[1], "all")
	case is(w, "kill") || is(w, "killing") || is(w, "hang") || is(w, "hanging") || is(w, "starve") ||
		is(w, "starving"):
		return self
	}
	return true
}

// unseenAfter reports the words after a method saying it is to go unseen:
// "quietly", "without anyone knowing", "without my parents noticing".
func unseenAfter(next []string) bool {
	if len(next) > 0 && unseenWays[next[0]] {
		return true
	}
	if len(next) < 3 || !is(next[0], "without") {
		return false
	}
	k := 1
	if possessives[next[1]] {
		k = 2
	}
	return len(next) > k+1 && unseenBy[next[k]] && unseenVerbs[next[k+1]]
}

// dieAsked reports the words before "die", nearest first, asking it: "can
// you", "will i", "could a person".
func dieAsked(before []string) bool {
	if len(before) >= 3 && is(before[0], "person") && is(before[1], "a") {
		return dieAsks[before[2]]
	}
	return len(before) >= 2 && dieSubjects[before[0]] && dieAsks[before[1]]
}

var (
	// searchKinds are what a search asks for: "ways to", "tips for".
	searchKinds = canonSetOf(`ways methods tips techniques tricks`)
	// dyingWays are ways of dying a search asks for: "ways to die painlessly".
	dyingWays = canonSetOf(`painlessly quickly fast peacefully`)
	// selfObjects and possessives make a method one's own: "kill myself",
	// "end my life", "hang my self".
	selfObjects = canonSetOf(`myself yourself oneself urself`)
	possessives = canonSetOf(`my your ur`)
	// unseenWays, unseenBy and unseenVerbs make a method unseen: "how to
	// purge quietly", "without my roommates hearing".
	unseenWays  = canonSetOf(`quietly secretly silently`)
	unseenBy    = canonSetOf(`anyone anybody parents family people roommate roommates`)
	unseenVerbs = canonSetOf(`knowing noticing hearing finding`)
	// dieAsks and dieSubjects ask whether one would die: "can you", "will i".
	dieAsks     = canonSetOf(`can will would could`)
	dieSubjects = canonSetOf(`i you u someone`)
	// searchWays open a method asked the way a search is: "ways to",
	// "easiest methods of", "tips for".
	searchWays = canonSetOf(`painless easiest easy quickest quick best fastest surest least good simple effective ways
		methods tips techniques tricks`)
	// heightUnits make a how-many question one about a height: "how many
	// floors to die".
	heightUnits = canonSetOf(`floors floor stories storeys storey metres meters feet ft`)
	// selfWords are the poster as a subject or an object, not as an owner:
	// "lethal dose for me", not "my pharm notes".
	selfWords = canonSetOf(`i i'm im i'll i'd i've ive me myself`)
	// studyFrames name the subject a dose is studied for, in a studyLabel:
	// not a word as plain as "exam", "notes" or "medicine", which a post
	// about the poster's own exam or medicine has as often.
	studyFrames = canonSetOf(`pharmacology pharmacy pharm pharma pharmacokinetics toxicology tox nursing mbbs
		physiology biochem biochemistry viva ld50`)
	// doseOf are the words between a dose and the medicine it is of: "lethal
	// dose of this medicine", "of my mums pills"; perWeight, after it, give
	// it per weight: "per kg", "in mg/kg".
	doseOf    = canonSetOf(`of the a an this that these those his her their mums moms dads`)
	perWeight = canonSetOf(`per in kg kgs mg g mcg ml body weight bodyweight`)
	// doseQuestions open a question for the dose: "what's a lethal dose of".
	doseQuestions = canonSetOf(`what what's whats wats wat how which where`)
	// doseStatements, after the dose, make the sentence say what it is.
	doseStatements = canonSetOf(`is are was were varies vary depends depend differs differ explains explain
		means causes cause makes leads lead`)
)

// ailments name what pills are taken for: "how many pills for a headache?
// asking for a friend".
var ailments = canonSetOf(`headache headaches migraine pain pains cramps period periods cold cough acidity gas
	jetlag lag sore throat bukhar sardi dard`)

// methodAsks make a sentence ask how: "how", "which bridge", "is there a
// way", "high enough".
var methodAsks = canonSetOf(`how which what where enough way ways method methods`)

// fallFatal reads the sentence of a height question for a death: "to
// die", "is fatal", "kill me", "not survive"; and not one "for an egg".
func fallFatal(ws []string) bool {
	for k, w := range ws {
		var after string
		if k+1 < len(ws) {
			after = ws[k+1]
		}
		switch {
		case fatalWords[w]:
			if is(after, "for") && !anyIn(ws[k+2:min(len(ws), k+4)], fallPeople) {
				return false
			}
			return true
		case is(w, "die") && k > 0 && is(ws[k-1], "to"), is(w, "kill") && fallPeople[after],
			is(w, "survive") && k > 0 && (is(ws[k-1], "not") || is(ws[k-1], "never")):
			return true
		}
	}
	return false
}

var fatalWords = canonSetOf(`fatal lethal deadly`)

// questionAfter reports a question mark ending the sentence that runs on
// from offset i.
func questionAfter(text string, i int) bool {
	after := text[i:min(len(text), i+64)]
	j := strings.IndexAny(after, ".!?\n")
	return j >= 0 && after[j] == '?'
}

var (
	medicinePlain  = newWindowSearch(`\b` + phrase(medicines) + `\b`)
	medicineLoose  = newWindowSearch(`\b` + canon(phrase(medicines)) + `\b`)
	deepCutTargets = canonSetOf(`my myself to so for without wrist wrists arm arms thigh thighs leg legs skin`)
	// stopEatingTrail, after "how to stop eating", keep it about not eating
	// at all.
	doseWords       = canonSetOf(`dose amount quantity dosage`)
	stopEatingTrail = canonSetOf(`completely entirely altogether forever without`)
	fallPeople      = canonSetOf(`me you u someone somebody person people human humans`)
	whichWords      = canonSetOf(`what which`)
	killNouns       = canonSetOf(`pills meds medicine medicines tablets drugs chemicals poison poisons`)
	dieHow          = canonSetOf(`from by of after`)
	dyingMethods    = canonSetOf(`hanging overdose overdosing od bleeding cutting slitting drowning poison poisoning
		pills jumping suffocation suffocating a an`)
	calorieWords = canonSetOf(`calories calorie cals cal kcal kcals`)
	cutPlaces    = canonSetOf(`to so without deep deeper wrist wrists arm arms thigh thighs leg legs skin vein veins`)
)

// namesAMedicine reports a medicine named in s, a stretch of v round a match.
func namesAMedicine(v *view, s string) bool {
	return medicinePlain.matchIn(v, s) || medicineLoose.matchIn(v, s)
}

/* Blocks ---------------------------------------------------------------------- */

type blockFinding struct {
	from, to int
	kind     findingKind
}

// firstBlock returns the earliest thing in the field a block rule refuses.
func (f *scannedField) firstBlock() (blockFinding, bool) {
	var best blockFinding
	found := false
	// The earliest finding is the one named, and of two that start together
	// the longer, so the poster sees the whole of what to take out.
	consider := func(from, to int, ok bool, kind findingKind) {
		if ok && (!found || from < best.from || from == best.from && to > best.to) {
			best, found = blockFinding{from: from, to: to, kind: kind}, true
		}
	}

	if run, kind, ok := f.firstNumber(); ok {
		from, to := f.plain.span(run.from, run.to)
		consider(from, to, true, kind)
	}
	if from, to, ok := f.firstSpelledNumber(); ok {
		from, to = f.plain.span(from, to)
		consider(from, to, true, kindPhone)
	}
	from, to, ok := f.first(niRule, func(v *view, m []int) bool {
		return validNIPrefix(v.text[m[2]], v.text[m[4]]) && (f.upperInSource(m[0], m[1]) || niLabelled(v.text, m[0]))
	})
	consider(from, to, ok, kindNI)

	from, to, ok = f.first(emailRule, func(v *view, m []int) bool {
		text := v.text
		local, domain, dot, tld := text[m[2]:m[3]], text[m[4]:m[5]], text[m[6]:m[7]], text[m[8]:m[9]]
		// A dot with a space beside it and no brackets round it.
		spaced := dot != "." && strings.TrimSpace(dot) == "."
		return emailAddress(local, bracketedDots(domain), readTLD(tld), spaced) &&
			!domainNamed(text, m[2], m[1], local, text[m[3]:m[4]], bracketedDots(domain))
	})
	consider(from, to, ok, kindEmail)
	from, to, ok = f.first(providerEmailRule, func(v *view, m []int) bool {
		local, at := v.text[m[2]:m[3]], v.text[m[4]:m[5]]
		return providerEmail(local, at) || mailNamedBefore(v.text, m[2]) && plainLocalPart(local)
	})
	consider(from, to, ok, kindEmail)
	for _, s := range []struct {
		r rule
		// pieces is how many groups the name is in; the rest follow it.
		pieces int
	}{{spelledEmailRule, 1}, {spelledLocalEmailRule, 2}} {
		from, to, ok = f.first(s.r, func(v *view, m []int) bool {
			text, k := v.text, 2*s.pieces
			local, at, domain, dot, tld := spelledDomain(text[m[2]:m[k+1]]), text[m[k+2]:m[k+3]], text[m[k+4]:m[k+5]],
				text[m[k+6]:m[k+7]], text[m[k+8]:m[k+9]]
			plainDots := dot == "." && !spelledDotPattern.MatchString(domain)
			return spelledEmail(local, at, spelledDomain(domain), tld, plainDots, text[m[k+9]:]) &&
				!domainNamed(text, m[2], m[1], local, text[m[k+1]:m[k+4]], spelledDomain(domain))
		})
		consider(from, to, ok, kindEmail)
	}
	if from, to, ok := f.firstSpacedEmail(); ok {
		from, to = f.plain.span(from, to)
		consider(from, to, true, kindEmail)
	}
	for _, r := range []rule{myMailRule, mailColonHandle} {
		from, to, ok = f.firstHandle(r, f.handleCheck)
		consider(from, to, ok, kindEmail)
	}

	from, to, ok = f.first(linkRule, func(v *view, m []int) bool {
		return contactLink(v.text[m[2]:m[3]], v.text[m[4]:m[5]])
	})
	consider(from, to, ok, kindLink)
	from, to, ok = f.first(spacedInviteRule, nil)
	consider(from, to, ok, kindLink)
	from, to, ok = f.first(linkedInPathRule, linkedInCheck)
	consider(from, to, ok, kindLink)

	for _, r := range askRules {
		from, to, ok = f.first(r, askMade(askCheck))
		consider(from, to, ok, kindAsk)
	}
	from, to, ok = f.first(numbersAskRule, askMade(numbersCheck))
	consider(from, to, ok, kindAsk)
	from, to, ok = f.first(offBoardRule, askMade(offBoardCheck))
	consider(from, to, ok, kindAsk)
	if from, to, kind, ok := f.firstAppFrame(); ok {
		consider(from, to, ok, kind)
	}
	from, to, ok = f.first(atAskRule, func(v *view, m []int) bool {
		r, _ := utf8.DecodeRuneInString(v.text[m[1]:])
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	})
	consider(from, to, ok, kindAsk)
	from, to, ok = f.first(myAppRule, f.myAppCheck)
	consider(from, to, ok, kindHandle)
	for _, r := range accountNumberRules {
		from, to, ok = f.first(r, func(v *view, m []int) bool { return !v.reported(m) })
		consider(from, to, ok, kindHandle)
	}
	from, to, ok = f.first(meetingIDRule, func(v *view, m []int) bool { return !v.reported(m) })
	consider(from, to, ok, kindAsk)
	from, to, ok = f.first(myAmbiguousAppRule, func(v *view, m []int) bool {
		a, b := v.span(m[2], m[3])
		handle := f.src[a:b]
		return plausibleHandle(strings.TrimPrefix(handle, "@"), strings.HasPrefix(handle, "@"))
	})
	consider(from, to, ok, kindHandle)
	// The handle rules find the app's name in either view, so "s.n.a.p" and
	// "sn4pch4t" count, and judge the handle from the source, where its
	// underscores, dots and digits are as the poster wrote them.
	for _, r := range handleRules {
		check := f.handleCheck
		switch r {
		case handleBeforeApp:
			check = f.handleFirstCheck
		case appLabelHandle:
			check = f.labelHandleCheck
		}
		from, to, ok = f.firstHandle(r, check)
		consider(from, to, ok, kindHandle)
	}

	return best, found
}

// firstAppFrame is the earliest ask or handle appFrame reads round an app's
// name, as a source span, and what it is.
func (f *scannedField) firstAppFrame() (int, int, findingKind, bool) {
	from, to, found := 0, 0, false
	var kind findingKind
	// Only the loose view is read: it has every name the plain view has,
	// spaced or in leetspeak too, and the handles are judged from the
	// source.
	vs := viewSearch{&f.loose, appNameRule.loose}
	{
		// appFrame counts what it reads itself, which for most names is a
		// few short words, where a check is counted as reading a clause.
		vs.re.each(vs.v, func(m []int) bool {
			if m[1] <= m[0] {
				return true
			}
			a, b, k, ok := f.appFrame(vs.v, m)
			if !ok || k == kindAsk && askNotMade(vs.v, []int{a, b}) {
				return true
			}
			if sa, sb := vs.v.span(a, b); !found || sa < from {
				from, to, kind, found = sa, sb, k, true
			}
			return false
		})
	}
	return from, to, kind, found
}

// askMade is check, for an ask for contact, and the ask made rather than
// named or taken back (askNotMade).
func askMade(check matchCheck) matchCheck {
	return func(v *view, m []int) bool { return check(v, m) && !askNotMade(v, m) }
}

// askNotMade reports an ask for contact that the words after it name rather
// than make, or take back. Named: what someone else says or said ("send me a
// dm on insta is what every spam bot says", "snap me later if you need the
// notes is what she said"), or a kind of thing to ask ("can we facetime? is a
// weird thing to ask a stranger"), in the rest of its sentence and with no
// "I" or "we" saying it. Taken back: "jk", "i mean" and the like, then a place
// on the board with no "not" between ("snap me a pic of the homework page
// please? jk, just post it here", "can we talk privately? i mean in a
// separate thread here"). Contact details are blocked however they are
// named; only an ask can be a mention.
func askNotMade(v *view, m []int) bool {
	text := v.text
	after := text[m[1]:min(len(text), m[1]+askAfterReach)]
	v.work += len(after)
	sentence := after
	if j := strings.IndexAny(sentence, ".!\n,;:"); j >= 0 {
		sentence = sentence[:j]
	}
	ws := wordsIn(sentence)
	for k := 0; k+1 < len(ws); k++ {
		if !is(ws[k], "is") && !is(ws[k], "was") {
			continue
		}
		rest := ws[k+1 : min(len(ws), k+6)]
		if is(rest[0], "what") {
			for i, w := range rest[1:] {
				if sayingVerbs[w] {
					return i > 0 && !anyIn(rest[1:1+i], firstPersonSubjects)
				}
			}
		}
		for i := 0; i+2 < len(rest); i++ {
			if (is(rest[i], "thing") || is(rest[i], "things")) && is(rest[i+1], "to") && askingVerbs[rest[i+2]] {
				// "... is a weird thing to ask but pls do" makes it after
				// all.
				return !askedAnyway(ws[k+1+i+3:])
			}
		}
	}
	spans := wordSpans(after)
	word := func(j int) string { return after[spans[j][0]:spans[j][1]] }
	for k := range spans {
		from := k + 1
		if w := word(k); !takeBacks[w] {
			if !is(w, "i") || k+1 == len(spans) || !is(word(k+1), "mean") && !is(word(k+1), "meant") {
				continue
			}
			from = k + 2
		}
		// What the take-back sends the talk to, in the rest of its
		// sentence: a place on the board after a verb of posting or a
		// preposition ("jk, just post it here", "i mean in a separate
		// thread here"), not a place complained about ("i mean this
		// thread is dying", "jk posting here is so slow", "i mean nobody
		// reads this board").
		for j := from; j < min(len(spans), from+10); j++ {
			if j > from && strings.ContainsAny(after[spans[j-1][1]:spans[j][0]], ".!?\n") {
				break
			}
			x := word(j)
			if negators[x] {
				return false
			}
			if !boardPlaces[x] {
				continue
			}
			if j+1 < len(spans) && placeComplaints[word(j+1)] {
				break
			}
			for i := max(from, j-3); i < j; i++ {
				if postingHere[word(i)] {
					return true
				}
			}
		}
		return false
	}
	return false
}

// askedAnyway reports an ask named as a kind of thing to ask and then made
// all the same: "but pls do", "but do it".
func askedAnyway(ws []string) bool {
	for k, w := range ws {
		if is(w, "but") {
			for _, x := range ws[k+1 : min(len(ws), k+3)] {
				if askAnyway[x] {
					return true
				}
			}
		}
	}
	return false
}

// askAfterReach is how far after an ask for contact askNotMade reads, in
// bytes.
const askAfterReach = 96

var (
	// askingVerbs, after "is a ... thing to", make an ask a kind of thing to
	// ask.
	askingVerbs = canonSetOf(`ask say send text type write message dm`)
	// takeBacks take an ask back: "jk", "nvm", "scratch that". "Actually"
	// is said in passing as often: "whatsapp me actually, i'm new here".
	takeBacks = canonSetOf(`jk jkjk kidding nvm nevermind scratch`)
	// negators between a take-back and a place keep the ask: "jk not here".
	negators = canonSetOf(`not no dont don't off outside never nahi mat`)
	// boardPlaces are places on the board an ask can be moved back to.
	boardPlaces = canonSetOf(`here thread threads board boards`)
	// postingHere, a word or three before a place on the board, send the
	// talk there: "post it here", "reply in this thread", "in a separate
	// thread here", "on the board".
	postingHere = canonSetOf(`post posting reply replying answer comment share put drop write keep stay continue
		discuss talk chat ask asking in on to into`)
	// placeComplaints after a place make it the subject of a reason for
	// leaving: "this thread is dead", "replying here takes forever".
	placeComplaints = canonSetOf(`is isn't was wasn't are were 's s takes take took sucks gets got feels seems looks
		has hasn't moves keeps`)
	// askAnyway, after "but", make a named ask anyway: "but pls do".
	askAnyway = canonSetOf(`pls plz please do just still dm text add snap message`)
)

// niLabelled reports a National Insurance number named just before it,
// which makes one in lower case one too: "national insurance no. sh 71 58
// 20 d", "NI number: ab 12 34 56 c".
func niLabelled(text string, i int) bool {
	ws := wordsBefore(text, i, 4)
	for k, w := range ws {
		switch {
		case is(w, "insurance") || is(w, "nino"):
			return true
		case is(w, "ni") && k > 0:
			return true
		case !idFillers[w]:
			return false
		}
	}
	return false
}

// upperInSource reports whether every letter of the source behind text[a:b]
// is upper case. National Insurance numbers are written in capitals; the same
// shape in lower case is far more often a hash or an identifier in code.
func (f *scannedField) upperInSource(a, b int) bool {
	from, to := f.plain.span(a, b)
	for _, r := range f.src[from:to] {
		if unicode.IsLetter(r) && !unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

// handleCheck judges a handle rule's match, whose groups are an "@", the
// handle and a Discord discriminator, from the handle as written. A handle
// glued to an opening bracket is a call in code ("ig = ig_client(token)"),
// and "@everyone" addresses a channel.
func (f *scannedField) handleCheck(v *view, m []int) bool {
	at := m[2] >= 0 && m[3] > m[2]
	discriminator := m[6] >= 0 && m[7] > m[6]
	if gluedToBracket(v, m) {
		return false
	}
	a, b := v.span(m[4], m[5])
	handle := f.src[a:b]
	if at && notHandles[strings.ToLower(handle)] {
		return false
	}
	return plausibleHandle(handle, at || discriminator)
}

// handleFirstCheck is handleCheck for a handle written before its app,
// which is not one when it is a tool or an exam with its number:
// "python3 on discord".
func (f *scannedField) handleFirstCheck(v *view, m []int) bool {
	at := m[2] >= 0 && m[3] > m[2]
	a, b := v.span(m[4], m[5])
	if !at && toolNames.MatchString(strings.ToLower(f.src[a:b])) {
		return false
	}
	return f.handleCheck(v, m)
}

// labelHandleCheck is handleCheck for a word straight after an app's name,
// with nothing but a space between: a tool with its version or a degree
// written with its points is the topic, not a handle ("ig python3 is better",
// "TG B.Tech counselling", "ok ig M.Sc is the better option"). After a label
// ("insta id python3") or an "@" it is a handle.
func (f *scannedField) labelHandleCheck(v *view, m []int) bool {
	at := m[2] >= 0 && m[3] > m[2]
	a, b := v.span(m[4], m[5])
	word := strings.ToLower(f.src[a:b])
	if !at && !handleLabels[lastWord(v.text[m[0]:m[4]])] && (toolNames.MatchString(word) || dottedAbbreviation(word)) {
		return false
	}
	return f.handleCheck(v, m)
}

var handleLabels = canonSetOf(`id username user userid name handle tag me is hai h`)

// myAppCheck judges "my snap is <word>" by the word: an "@" before it or a
// username's marks make it a handle, notAHandle words do not, and a plain
// word is a name only where the sentence ends on it and it reads as no
// description ("my insta is madeuppriya", not "my insta is underrated").
func (f *scannedField) myAppCheck(v *view, m []int) bool {
	word := v.text[m[2]:m[3]]
	if strings.HasPrefix(word, "@") {
		return true
	}
	if notAHandle[canon(word)] {
		return false
	}
	a, b := v.span(m[2], m[3])
	if plausibleHandle(f.src[a:b], false) {
		return true
	}
	for _, suffix := range []string{"ed", "ly", "ing"} {
		if strings.HasSuffix(word, suffix) {
			return false
		}
	}
	rest := strings.TrimLeft(v.text[m[3]:], " \t")
	r, _ := utf8.DecodeRuneInString(rest)
	return rest == "" || strings.ContainsRune(".!?\n,;", r)
}

// plausibleHandle accepts what looks like a username rather than a word or a
// measure: two letters or more (one after an "@"), and an "@" before it or a
// digit, dot or underscore in it, and not the shape of a time, a count, a
// version or an exponent ("3h", "2nd", "9am", "1.5k", "v2.0", "1.5e-3").
func plausibleHandle(handle string, at bool) bool {
	// A full stop after the word ends the sentence: "on reddit instead.".
	handle = strings.TrimRight(handle, ".")
	letters, marked := 0, at
	for _, r := range handle {
		switch {
		case unicode.IsLetter(r):
			letters++
		case r == '_' || r == '.' || unicode.IsDigit(r):
			marked = true
		}
	}
	if letters == 0 || letters == 1 && !at || !marked {
		return false
	}
	return at || !measureShape.MatchString(strings.ToLower(handle))
}

// measureShape is a time, a count, a version or an exponent: "3h", "2h30m",
// "4yrs", "2nd", "9am", "1.5k", "v2.0", "1.5e-3".
var measureShape = regexp.MustCompile(`^(?:v\d+(?:\.\d+)*[a-z]?|(?:\d+(?:[.,]\d+)?(?:st|nd|rd|th|am|pm|hrs?|mins?|secs?|ms|kg|km|cm|mm|gb|mb|kb|fps|yrs?|years?|mos?|months?|wks?|weeks?|days?|x|k|e\d*|h|m|s|d|y|w))+|\d+(?:\.\d+)?e[+-]?\d+|e\.g|i\.e)\.?$`)

// emailAddress vets a matched address. spaced says the dot before the
// top-level domain had a space beside it, which is allowed only for ".com",
// where it is a known dodge; anywhere else it is a sentence ending.
func emailAddress(local, domain, tld string, spaced bool) bool {
	if !emailTLDs[tld] && !academicSecondLevel(domain, tld) || (spaced && tld != "com") {
		return false
	}
	if local == "git" { // git@github.com:owner/repo is an SSH remote, not a mailbox
		return false
	}
	return !exampleDomain(domain + "." + tld)
}

// spelledEmail vets a spelled-out address. A "@" written next to the name, a
// bracketed "at" or "at the rate" says it is one. A bare "at", or an "@" with
// a space before it, needs one more sign that it is an address and not a
// sentence: a name-like part before it ("madeup_k", "madeup12"), or a mail
// provider's domain with something other than "sign in" or "me" before it;
// with "dot" spelled out, an institution's domain will do too ("madeup at
// iitb dot ac dot in"). A site followed by a path is a link: "docs at
// docs.python.org/3/". A problem, a level or a job title ("Q3 at
// leetcode.com", "SDE2 at amazon.com", "Div2 at codeforces.com") is shaped
// like a name but is not one, except at a mail provider or a university; and
// a degree, a numbered part of a text, an ordinal or a tool's version ("B.Tech
// at josaa.nic.in", "vol.1 at jstor.org", "2nd-year at iitb.ac.in",
// "Python3.12 at python.org") is not one anywhere but at a mail provider.
func spelledEmail(local, at, domain, tld string, plainDots bool, next string) bool {
	if strings.HasPrefix(next, "/") || !emailAddress(local, domain, tld, false) {
		return false
	}
	if !weakAt(at) {
		return true
	}
	label, _, _ := strings.Cut(domain, ".")
	academic := tld == "edu" || strings.HasSuffix(domain, ".ac") || strings.HasSuffix(domain, ".edu")
	switch {
	case nameLike(local) && mailProviders[label]:
		return true
	case nameLike(local):
		if dottedAbbreviation(local) || ordinalWord.MatchString(local) || toolNames.MatchString(local) {
			return false
		}
		return !codeShape.MatchString(local) || academic
	case mailProviders[label]:
		return !notALocalPart[local]
	case plainDots:
		return false
	}
	return strings.IndexFunc(local, unicode.IsLetter) >= 0 && academic
}

// codeShape is one to three letters and a short number: "q3", "l4", "sde2",
// "div2", "abc300".
var codeShape = regexp.MustCompile(`^[a-z]{1,3}\d{1,4}$`)

// mailNamedBefore reports an address asked for or labelled just before i,
// in its clause: "my email is madeupriya at gmail", "mail me madeupriya at
// gmail", not "email the prof at gmail".
func mailNamedBefore(text string, i int) bool {
	ws := wordsIn(clauseBefore(text, i))
	n := len(ws)
	if n == 0 || !mailLeads[ws[n-1]] {
		return false
	}
	for _, w := range ws[max(0, n-3):] {
		if mailWords[w] || is(w, "e-mail") {
			return true
		}
	}
	return false
}

var mailLeads = canonSetOf(`is me us hai h id address mail email gmail`)

// plainLocalPart reports a word with no marks that may still be an
// address's name where one is asked for: "madeupriya", not "broken",
// "down" or "email".
func plainLocalPart(local string) bool {
	if strings.IndexFunc(local, unicode.IsLetter) < 0 || notALocalPart[local] || mailWords[local] ||
		notAHandle[canon(local)] {
		return false
	}
	for _, suffix := range []string{"ed", "ly", "ing"} {
		if strings.HasSuffix(local, suffix) {
			return false
		}
	}
	return true
}

// providerEmail vets a provider named with no ending, "name @ gmail" or
// "name12 at gmail": a "@" glued to the name, brackets or "at the rate" say
// it is an address, and a bare "at" or a spaced "@" needs a name-like part
// before it ("lol @ gmail" is a reaction).
func providerEmail(local, at string) bool {
	return !weakAt(at) || nameLike(local)
}

// weakAt reports an "at" that is ordinary English or punctuation as often as
// an address's "@": "at" on its own, or "@" with a space before it.
func weakAt(at string) bool {
	trimmed := strings.TrimSpace(at)
	return trimmed == "at" || strings.Trim(trimmed, "@") == "" && strings.TrimLeft(at, " \t\n") != at
}

// nameLike reports a word shaped like the part of an address before the "@":
// a letter, and a digit, dot or underscore, and not a version number ("v2.0").
func nameLike(local string) bool {
	return strings.IndexFunc(local, unicode.IsLetter) >= 0 && strings.ContainsAny(local, "._0123456789") &&
		!versionShape.MatchString(local) && !strings.HasSuffix(local, ".")
}

var versionShape = regexp.MustCompile(`^v?\d+(?:\.\d+)*[a-z]?$`)

// notALocalPart are the words that come before "at gmail.com" in a sentence,
// or before a domain named with its "@" ("signed up with my @iitb.ac.in
// address", "is a @yahoo.com address okay", "spam from @madeup.com").
var notALocalPart = setOf(`in it me us up on out log sign login signin logged signed account accounts email
	emails mail mails inbox them him her you there here is are available also now check find visit go look
	a an the my your ur our their his its this that these those with from to for of at by via into onto
	and or nor but vs than like as if only not no just even all any every each both some most new old
	was were be been being am have has had having do does did get gets got use uses used using end ends
	ending ended start starts starting want wants need needs accept accepts accepted take takes allow
	allows give gives make makes send sends sent what which who whose where when why how same own one ones`)

// domainNamed reports a domain named with its "@", "@iitb.ac.in" or
// "@gmail.com", after a word of the sentence and not a mailbox's name: an
// "@" with a space before it and none after, glued to the domain, reads as
// an address only after a name-like word ("madeup12 @gmail.com"), a name at a
// mail provider ("madeup @gmail.com"), or a name with an email word a few
// words either side ("my email is riya @madeup.com", "riya @madeup.com is my
// email"). Whatever the spacing, a function word before the "@" is the
// sentence's own ("ending in @ gmail.com").
func domainNamed(text string, start, end int, local, gap, domain string) bool {
	trimmed := strings.TrimSpace(gap)
	if trimmed == "" || strings.Trim(trimmed, "@") != "" || strings.TrimLeft(gap, " \t\n") == gap {
		return false
	}
	if notALocalPart[local] {
		return true
	}
	if strings.TrimRight(gap, " \t\n") != gap {
		return false
	}
	label, _, _ := strings.Cut(domain, ".")
	if nameLike(local) || mailProviders[label] {
		return false
	}
	for _, w := range append(wordsBefore(text, start, 4), nextWords(text[end:], 3)...) {
		if addressCues[w] {
			return false
		}
	}
	return true
}

// addressCues, a few words before a name and a domain, say the two are an
// address: "my email is riya @madeup.com".
var addressCues = setOf(`email emails mail gmail id address contact reach write`)

var mailProviders = setOf(`gmail googlemail yahoo ymail rocketmail outlook hotmail live msn icloud mac aol
	proton protonmail pm tutanota tuta zoho rediffmail rediff yandex mail gmx qq 163 126 fastmail`)

// spelledDomain turns "gmail (dot) co dot in" back into "gmail.co.in".
func spelledDomain(domain string) string {
	return spelledDotPattern.ReplaceAllString(domain, ".")
}

var spelledDotPattern = regexp.MustCompile(`\s*[(\[{<]\s*(?:d[o0]t|\.)\s*[)\]}>]\s*|[\s_]+d[o0]t[\s_]+`)

// firstSpacedEmail finds an address written a character at a time with a
// space after each, "m a d e u p . 0 9 @ g m a i l . c o m", in the plain
// view: the loose view closes up spaced letters, but not the digits, dots and
// "@" among them. The word next to the "@" may be written whole
// ("madeup09 @ g m a i l . c o m"), and four characters at least must be
// spaced out. One pass from each "@" each way, which stops at the "@" before.
func (f *scannedField) firstSpacedEmail() (int, int, bool) {
	v := &f.plain
	t := v.text
	for i := 0; i < len(t); {
		at := strings.IndexByte(t[i:], '@')
		if at < 0 {
			break
		}
		at += i
		i = at + 1
		lo, local, left := spacedPart(t, at, -1)
		hi, domain, right := spacedPart(t, at+1, 1)
		v.work += hi - lo
		if left+right < 4 || local == "" || strings.IndexFunc(local, unicode.IsLetter) < 0 {
			continue
		}
		name, tld, dotted := strings.Cut(domain, ".")
		if dotted {
			k := strings.LastIndexByte(domain, '.')
			name, tld = domain[:k], domain[k+1:]
		}
		// A provider with no ending needs a name before it that is spaced
		// out too, or shaped like one: "lol @ g m a i l" is a reaction.
		if dotted && emailAddress(local, name, readTLD(tld), false) ||
			!dotted && mailProviders[domain] && (left >= 2 || nameLike(local)) {
			return lo, hi, true
		}
	}
	return 0, 0, false
}

// spacedPart reads one half of a spaced-out address from the "@" outwards,
// backwards from t[i] when dir is -1 and on from it when 1: single characters
// with a space between each, or one whole word next to the "@". It returns
// how far it read, the half closed up, and how many single characters it
// read.
func spacedPart(t string, i, dir int) (int, string, int) {
	addressByte := func(c byte) bool {
		return 'a' <= c && c <= 'z' || isASCIIDigit(c) || strings.IndexByte("._%+-", c) >= 0
	}
	var parts []string
	singles, edge := 0, i
	j := i
	if dir < 0 && j > 0 && t[j-1] == ' ' {
		j--
	} else if dir > 0 && j < len(t) && t[j] == ' ' {
		j++
	}
	for len(parts) < 64 {
		k := j
		if dir < 0 {
			for k > 0 && j-k < 64 && addressByte(t[k-1]) {
				k--
			}
		} else {
			for k < len(t) && k-j < 64 && addressByte(t[k]) {
				k++
			}
		}
		a, b := min(j, k), max(j, k)
		if a == b || len(parts) > 0 && b-a > 1 {
			break
		}
		parts = append(parts, t[a:b])
		edge = k
		if b-a > 1 {
			break // a whole word, next to the "@"
		}
		singles++
		// On past one space to the next character, if there is one.
		if dir < 0 {
			if k < 2 || t[k-1] != ' ' || !addressByte(t[k-2]) {
				break
			}
			j = k - 1
		} else {
			if k+1 >= len(t) || t[k] != ' ' || !addressByte(t[k+1]) {
				break
			}
			j = k + 1
		}
	}
	if dir < 0 {
		slices.Reverse(parts)
	}
	return edge, strings.Join(parts, ""), singles
}

/* Numbers --------------------------------------------------------------------- */

// digitRun is a number as written: digit groups separated by spaces, line
// breaks, dashes, dots or underscores, or by a comma on its own, perhaps with a
// leading "+" and the first group in brackets, the "+" inside them or not
// ("(+91) 98765 43210"). Inside a group an o reads as 0,
// and an l or i as 1, when a digit is beside it ("98765 4321O").
type digitRun struct {
	from, to int // in the plain view's text
	digits   []byte
	groups   []int
	seps     []string
	plus     bool
	paren    bool // the first group is in brackets: "(495) 867-5309"
	// apartAfter says that something the plain view leaves out comes straight
	// after the run, so a letter there does not glue the run to a word.
	apartAfter bool
}

// firstNumber scans the plain view for the first number that is a phone
// number or an ID number. One pass: every byte is looked at a bounded number
// of times.
func (f *scannedField) firstNumber() (digitRun, findingKind, bool) {
	t := f.plain.text
	for i := 0; i < len(t); {
		c := t[i]
		starts := isASCIIDigit(c) ||
			(c == '+' || c == '(') && i+1 < len(t) && isASCIIDigit(t[i+1]) ||
			(c == '+' && t[i+1:min(len(t), i+2)] == "(" || c == '(' && t[i+1:min(len(t), i+2)] == "+") &&
				i+2 < len(t) && isASCIIDigit(t[i+2])
		if !starts {
			i++
			continue
		}
		if i > 0 {
			// Glued to a word or a longer number: part of something else,
			// such as C6H12O6, a hex string or the tail of an ISBN. Where
			// something the plain view leaves out came between, a spoiler tag
			// or an invisible character, the word ends there: "call
			// me[spoiler]98765…".
			if r, _ := utf8.DecodeLastRuneInString(t[:i]); unicode.IsDigit(r) || unicode.IsLetter(r) && !f.plain.apart(i) {
				i++
				continue
			}
		}
		run := scanDigitRun(t, i)
		run.apartAfter = run.to < len(t) && f.plain.apart(run.to)
		if kind, ok := classifyNumber(&f.plain, run); ok {
			return run, kind, true
		}
		if from, to, ok := piecedNumber(&f.plain, i); ok {
			return digitRun{from: from, to: to}, kindPhone, true
		}
		if to, ok := halvedNumber(&f.plain, run); ok {
			return digitRun{from: run.from, to: to}, kindPhone, true
		}
		i = max(run.to, i+1)
	}
	return digitRun{}, 0, false
}

func isASCIIDigit(c byte) bool { return '0' <= c && c <= '9' }

// spelledDigits are the words a digit is read out as, in English and in
// Hinglish: "nine eight 7 six", "oh seven seven double oh", "nau aath saat".
var spelledDigits = map[string]byte{
	"zero": '0', "oh": '0', "o": '0', "shunya": '0', "sunya": '0',
	"one": '1', "ek": '1',
	"two": '2', "do": '2',
	"three": '3', "teen": '3',
	"four": '4', "char": '4', "chaar": '4',
	"five": '5', "paanch": '5', "panch": '5',
	"six": '6', "chhe": '6', "chhah": '6', "che": '6',
	"seven": '7', "saat": '7', "sat": '7',
	"eight": '8', "aath": '8',
	"nine": '9', "nau": '9', "nao": '9',
}

// digitRepeats say that the digit after them comes twice or three times.
var digitRepeats = map[string]int{"double": 2, "triple": 3}

// maxSpelledDigits is where a run of number words stops being read: longer
// than any phone number, so that each word is read by a bounded number of the
// runs that start before it and the scan stays linear.
const maxSpelledDigits = 16

// firstSpelledNumber finds the first phone number read out a digit at a time
// in words, or in words and digits mixed: "nine eight 7 six five 4 three two 1
// zero", "9 eight 3 one 0 four 2 2 7 seven", "zero four nine one, five seven
// one". It needs two number words at least, which leaves a run of digits to
// firstNumber, and is held to the phone shapes alone (spelledPhone), which a
// sentence that happens to hold a few number words does not have.
func (f *scannedField) firstSpelledNumber() (from, to int, ok bool) {
	t := f.plain.text
	// A token is a run of ASCII letters and digits; one glued to any other
	// letter is part of a word, never a number.
	alnum := func(c byte) bool { return 'a' <= c && c <= 'z' || isASCIIDigit(c) || c >= utf8.RuneSelf }
	for i := 0; i < len(t); {
		if !alnum(t[i]) || i > 0 && alnum(t[i-1]) {
			i++
			continue
		}
		var digits []byte
		words, repeat, end, next := 0, 0, i, len(t)
		for j := i; len(digits) <= maxSpelledDigits; {
			k := j
			for k < len(t) && alnum(t[k]) {
				k++
			}
			next = min(next, k)
			tok, add := t[j:k], ""
			if d, isWord := spelledDigits[tok]; isWord {
				add = string(d)
				words++
			} else if tok != "" && len(tok) <= maxSpelledDigits && strings.Trim(tok, "0123456789") == "" {
				add = tok
			} else if r := digitRepeats[tok]; r > 0 && repeat == 0 {
				repeat = r
			} else {
				break
			}
			if add != "" {
				if repeat > 0 {
					if len(add) > 1 {
						break // "double 98" is not how a number is read out
					}
					add, repeat = strings.Repeat(add, repeat), 0
				}
				digits = append(digits, add...)
				end = k
			}
			s := k
			for s < len(t) && s-k < 3 && strings.IndexByte(" \t\n,.-", t[s]) >= 0 {
				s++
			}
			if s == k {
				break
			}
			j = s
		}
		d := string(digits)
		if words >= 2 && spelledPhone(&f.plain, i, d) {
			return i, end, true
		}
		// Like firstNumber, go on after the run rather than inside it, so
		// that the tail of a number that is not one to call ("oh seven seven
		// double oh, nine double oh …", a drama number) is not read as one.
		// A run that starts on a word that is as often not a number ("oh",
		// "do", "sat") is read again from its second word.
		if end > i && !(ambiguousDigitWords[t[i:next]] && !dramaNumber(d) && !helpline(d)) {
			i = max(end, next)
		} else {
			i = next
		}
	}
	return 0, 0, false
}

// ambiguousDigitWords are the number words that are as often other words:
// "oh nine eight …", "do 9 8 7 …".
var ambiguousDigitWords = setOf(`o oh do sat che ek`)

// spelledPhone reports digits read out in words that make a phone number: an
// Indian, UK or Australian mobile in any case, and a North American number
// after a word that says it is one to call. A label or a word for a set of
// values before it makes it something else, and so does a count up or down
// ("nine eight seven … zero, liftoff") unless that word is there.
func spelledPhone(v *view, from int, d string) bool {
	if len(d) < 9 || len(d) > 13 || dramaNumber(d) || helpline(d) || labelled(v.text, from) {
		return false
	}
	near := windowBefore(v.text, from, 40)
	if dataWords.matchIn(v, near) {
		return false
	}
	cued := phoneCue.matchIn(v, near)
	if counting(d) && !cued {
		return false
	}
	one := []int{len(d)}
	return indianMobile(d, one, nil) || ukPhone(d, one, nil) || auMobile(d, one, nil) ||
		cued && (nanp(d) || len(d) == 11 && d[0] == '1' && nanp(d[1:]))
}

// counting reports digits that go up or down by one each time: 0123456789,
// 9876543210.
func counting(d string) bool {
	up, down := true, true
	for i := 1; i < len(d); i++ {
		up = up && d[i] == d[i-1]+1
		down = down && d[i] == d[i-1]-1
	}
	return up || down
}

func scanDigitRun(t string, i int) digitRun {
	run := digitRun{from: i}
	// "(+91)": the bracket opens before the "+".
	plusInParen := strings.HasPrefix(t[i:], "(+")
	if plusInParen {
		i++
	}
	if t[i] == '+' {
		run.plus = true
		i++
	}
	for {
		paren := plusInParen || i < len(t) && t[i] == '('
		if paren && !plusInParen {
			i++
		}
		plusInParen = false
		g := 0
		for i < len(t) {
			d, ok := t[i], isASCIIDigit(t[i])
			if !ok {
				d, ok = digitLookalike(t, i)
			}
			if !ok {
				break
			}
			run.digits = append(run.digits, d)
			g++
			i++
		}
		if g == 0 {
			break
		}
		run.groups = append(run.groups, g)
		closed := false
		if paren && i < len(t) && t[i] == ')' {
			i++
			closed = true
			run.paren = run.paren || len(run.groups) == 1
		}
		run.to = i
		j := groupSeparator(t, i)
		_, lookalike := digitLookalike(t, j)
		next := j < len(t) && (isASCIIDigit(t[j]) || lookalike || t[j] == '(' && j+1 < len(t) && isASCIIDigit(t[j+1]))
		if !next || (j == i && !closed) {
			break
		}
		run.seps = append(run.seps, t[i:j])
		i = j
	}
	if run.to == run.from {
		run.to = run.from + 1
	}
	return run
}

// lookalikeDigit is the digit a letter is written for in a number: o for 0, l
// and i for 1.
func lookalikeDigit(c byte) (byte, bool) {
	switch c {
	case 'o':
		return '0', true
	case 'l', 'i':
		return '1', true
	}
	return 0, false
}

// maxLookalikeWord is as far either way as digitLookalike reads the word a
// letter is in, longer than any group of a phone number.
const maxLookalikeWord = 16

// digitLookalike reads the letter at t[i] as the digit it is written for, in
// a word that is otherwise digits and has one ("98765 4321O", "07911 4OO
// 133"), or standing alone between two digits spaced out one at a time ("8 l
// 2 O 3 3 4 5 6 1"). A word with any other letter in it is a word.
func digitLookalike(t string, i int) (byte, bool) {
	if i >= len(t) {
		return 0, false
	}
	d, ok := lookalikeDigit(t[i])
	if !ok {
		return 0, false
	}
	inNumber := func(c byte) bool {
		_, ok := lookalikeDigit(c)
		return ok || isASCIIDigit(c)
	}
	digit := false
	lo := i
	for lo > 0 && i-lo < maxLookalikeWord && inNumber(t[lo-1]) {
		lo--
		digit = digit || isASCIIDigit(t[lo])
	}
	hi := i + 1
	for hi < len(t) && hi-i < maxLookalikeWord && inNumber(t[hi]) {
		digit = digit || isASCIIDigit(t[hi])
		hi++
	}
	letter := func(c byte) bool { return 'a' <= c && c <= 'z' || c >= utf8.RuneSelf || c == '_' }
	switch {
	case lo > 0 && letter(t[lo-1]) || hi < len(t) && letter(t[hi]):
		return 0, false
	case digit:
		return d, true
	}
	alone := lo == i && hi == i+1 && i >= 2 && hi+1 < len(t) && t[i-1] == ' ' && t[hi] == ' ' &&
		isASCIIDigit(t[i-2]) && isASCIIDigit(t[hi+1])
	return d, alone
}

// groupSeparator returns where the separator after a digit group ending at i
// ends: up to three spaces, line breaks, dashes, dots and underscores, or one
// comma with nothing after it. A comma and a space is how a list is written,
// so it ends the number.
func groupSeparator(t string, i int) int {
	if i < len(t) && t[i] == ',' {
		return i + 1
	}
	j := i
	for j < len(t) && j-i < 3 && strings.IndexByte(" \n-._", t[j]) >= 0 {
		j++
	}
	return j
}

// mathsBefore and mathsAfter are the operators that make a number one side of
// a calculation rather than something to dial, when there is something on
// their other side to calculate with.
const (
	mathsBefore = "=≈≠≤≥<>×*÷/^±~≡"
	mathsAfter  = "=≈≠≤≥<>×*÷/^"
)

// numberLabels, as the word just before a number (numberFillers aside), say
// what kind of number it is: a reference, a rank, an amount of money, or what
// a program printed or should print ("Output: 8589934592", "it returns
// 9876543210", and "got" after "expected": "expected 9876543210 got
// 9876543211").
var numberLabels = setOf(`isbn issn doi arxiv pnr utr roll order invoice transaction txn ticket serial
	receipt receipts challan voucher bill booking confirmation ack acknowledgement acknowledgment token complaint
	docket consignment shipment parcel courier membership customer
	application app form registration reg enrol enroll enrolment enrollment account acc acct a/c policy
	jee neet cuet seat rank imei tracking awb reference ref
	rs inr usd eur gbp salary stipend fee fees price cost costs budget revenue profit turnover population
	output outputs print prints printed return returns returned stdout expected result`)

// labelledSubject reports a number given as what a label is, in a sentence
// whose subject the label heads: "the ISBN on my copy is 9353501989", "the
// order ID from the refund mail was …". After "is", "was" or "hai" straight
// before the number, up to four words may stand between the label and it,
// none of them a way of being reached (contactWords) or a person, and no
// clause break.
func labelledSubject(t string, from int) bool {
	clause := t[max(0, from-64):from]
	clause = clause[strings.LastIndexAny(clause, ".!?\n,;")+1:]
	ws := wordsIn(clause)
	k := len(ws) - 1
	for k >= 0 && numberFillers[ws[k]] && !copulas[ws[k]] {
		k--
	}
	if k < 0 || !copulas[ws[k]] {
		return false
	}
	for j, n := k-1, 0; j >= 0 && n <= 4; j-- {
		w := ws[j]
		switch {
		case numberLabels[w]:
			return true
		case contactWords[w] || otherSpeakers[w] || strings.HasSuffix(w, "'s"):
			return false // "the parcel guy's number is …" is his phone
		case !numberFillers[w]:
			n++
		}
	}
	return false
}

var (
	copulas = setOf(`is was hai`)
	// contactWords are the ways of being reached, which no label's phrase
	// names: "the order guy's whatsapp is …" is a phone number.
	contactWords = canonSetOf(`call text txt whatsapp wa phone mobile cell contact dm reach ping msg message telegram
		snap insta signal me`)
)

// labelLinks may stand between a reference's label and its number, up to
// three of them: "PNR status for 8177091875", "order id for 7834567890",
// "PNR of my train is 8177091875".
var labelLinks = setOf(`for of my mine our the this status details train flight booking`)

// numberFillers are the words that sit between a label and its number: "roll
// no. 23", "order number is", "transaction id".
var numberFillers = setOf(`no nos number num nmbr id is was hai`)

// quantityWords, straight after a number, say it counts something.
var quantityWords = setOf(`bytes byte kb mb gb tb kib mib gib bits bit
	rupees rupee rs inr dollars dollar usd eur euros euro pounds gbp yen crore crores cr lakh lakhs lac
	million millions billion billions trillion
	people persons users students voters votes views followers subscribers downloads
	years yrs months days hours hrs minutes mins seconds secs ms ns
	km kms metres meters miles cm mm kg kgs grams tonnes tons litres liters ml
	times steps iterations words characters chars digits cells atoms molecules particles stars units`)

// helplineNumbers are crisis lines whose numbers look like phone numbers to the
// rules, so that a reader passing one on to someone who needs it is not told
// to take it out. Each is the national number without its trunk zero or a
// toll-free number's leading 1: in India iCall, the Vandrevala Foundation,
// AASRA and Tele-MANAS (1-800-891-4416); in the UK PAPYRUS HOPELINE247 (0800
// 068 4141), the NSPCC (0808 800 5000), The Mix (0808 808 4994) and Mind's
// infoline (0300 123 3393); in the US the old National Suicide Prevention
// Lifeline number (it still connects to 988), the Trevor Project, the National
// Domestic Violence Hotline, Childhelp, RAINN (800-656-4673) and SAMHSA's
// helpline (800-662-4357).
var helplineNumbers = setOf(`9152987821 9999666555 9820466726 8008914416 8000684141 8088005000 8088084994
	3001233393 8002738255 8664887386 8007997233 8004224453 8006564673 8006624357`)

// indianNational is an Indian number without its country code or trunk zero.
func indianNational(d string) string {
	switch {
	case len(d) == 12 && strings.HasPrefix(d, "91"):
		return d[2:]
	case len(d) == 11 && d[0] == '0':
		return d[1:]
	}
	return d
}

// helpline reports a run of digits that is one of helplineNumbers, with or
// without the country code or trunk prefix it is dialled with.
func helpline(d string) bool {
	switch {
	case len(d) == 11 && d[0] == '1': // a North American number with its 1, or an Indian 1800 one
		d = d[1:]
	case len(d) == 12 && strings.HasPrefix(d, "44"): // a UK one with its country code
		d = d[2:]
	}
	return helplineNumbers[indianNational(d)]
}

// mathsContext, as whole words near a number, makes it the subject of a
// maths question or the result of one: "is 9999999967 prime?", "the answer is
// 9876543210", "13! is 6227020800".
var (
	mathsContext = newWindowSearch(`\b(?:` + words(`prime`, `composite`, `divisib(?:le|ility)`, `divided`,
		`divisors?`, `factors?`, `factori[sz](?:e|ed|ation)`, `factorial`, `modulo`, `mod`, `remainder`,
		`quotient`, `gcd`, `hcf`, `lcm`, `multipl(?:y|ied|ies|ying|ication|es?)`, `fibonacci`, `power~of`,
		`squared`, `cubed`, `square~roots?`, `cube~roots?`, `binary`, `hexadecimal`, `octal`, `sum`,
		`product~of`, `equals`, `digits~of`, `palindromes?`, `integers?`, `calculate`, `compute`,
		`evaluate`, `simplify`, `approx`, `approximately`, `population`) +
		`)\b|\bans(?:wer)?[ \t]*(?:[=:]|is\b|was\b)`)
	// mathsResult is a calculation just before a number: "2^33 is", "13! =",
	// and a comment after one in code: "print(2**33)  # 8589934592".
	mathsResult = newWindowSearch(`[\d)][ \t]*(?:\^[ \t]*\d+|!|\*\*[ \t]*\d+)\)*[ \t]*(?:is|=|equals|\n?[ \t]*(?:#|//))?[ \t]*$`)
	// dataWords, before a run of short groups or a number's pieces, make it a
	// set of values: "the mode of 7 9 6 8 ...", "batch sizes 256 512 1024",
	// "scores 65-70-78", "Q 98, 45, 67", "routes 98 / 45 / 67".
	dataWords = newWindowSearch(`\b(?:` + words(`mode`, `mean`, `median`, `average`, `avg`, `range`, `data`,
		`dataset`, `values`, `sequence`, `series`, `list`, `array`, `numbers`, `set`, `digits`, `dice`,
		`rolls`, `scores`, `marks`, `readings`, `samples`, `observations`, `sizes`, `dims`, `dimensions`,
		`shape`, `resolution`, `prices`, `fees`, `salaries`, `cutoffs?`, `percentiles?`, `ranks`, `rankings?`,
		`mocks`, `sgpas?`, `cgpas?`, `gpas?`, `pointers?`, `grades`, `sections`, `exercises?`, `chapters?`,
		`questions`, `q`, `qs`, `qns`, `ques`, `problems`, `pages`, `percentages`, `rooms`, `routes`, `buses`) +
		`)\b`)
	// phoneCue, shortly before a number, says it is one to call.
	phoneCue = newWindowSearch(`\b(?:` + words(`call`, `text`, `txt`, `ring`, `phone`, `ph`, `mob`, `mobile`,
		`cell`, `number`, `num`, `no`, `whatsapp`, `dial`, `contact`, `tel`, `sms`, `hmu`) + `)\b`)
	// callCue, shortly before a number, asks for the poster to be called or
	// says the number is theirs: "call me", "text me", "hmu", "my number".
	callCue = newWindowSearch(phrase(`\b(?:` + words(`call`, `text`, `txt`, `ring`, `dial`, `sms`, `msg`,
		`message`, `ping`, `whatsapp`, `wa`, `reach`) + `~` + words(`me`, `us`) + `|` + words(`my`, `mera`, `meri`) +
		`~(?:` + words(`phone`, `mobile`, `cell`, `whatsapp`, `contact`) + `~)?` +
		words(`number`, `no`, `num`, `nmbr`) + `|` + words(`my`, `mera`, `meri`) + `~` + words(`hp`, `handphone`) +
		`|text~` + words(`only`, `dont~call`, `don'?t~call`, `no~calls?`) + `|hmu)\b`))
	// contactCue, with nothing but punctuation between it and a number, asks
	// for the number to be called or says whose it is: "call me on", "text
	// me", "wa:", "my number is". "The number" is not one: "is the number
	// 9999999967 prime?".
	contactCue = newWindowSearch(`\b` + phrase(words(
		words(`call`, `text`, `txt`, `ring`, `dial`, `sms`, `contact`, `msg`, `message`, `ping`, `dm`, `reach`, `hmu`,
			whatsApp, `wa`, `telegram`, `signal`)+`(?:~`+words(`me`, `us`)+`)?(?:~`+words(`on`, `at`, `via`, `@`)+`)?`,
		words(`my`, `mera`, `meri`)+`~(?:`+words(`phone`, `mobile`, `cell`, `whatsapp`, `contact`)+`~)?`+
			words(`number`, `no`, `num`, `nmbr`)+`(?:~`+words(`is`, `hai`)+`)?`,
	)) + `[^\pL\pN]*$`)
	// reachCue, anywhere in the words round a number, asks for the poster to
	// be reached: "dm me", "add me", "find me", "dm for more", "hmu", "@x_y".
	// A number given as a maths answer, or about which a maths question is
	// asked, has none round it (reachedNear).
	reachCue = newWindowSearch(`\b` + phrase(words(
		words(`call`, `text`, `txt`, `ring`, `dial`, `sms`, `contact`, `msg`, `message`, `ping`, `dm`, `pm`,
			`reach`, `add`, `inbox`, `hit`, `find`)+`~`+words(`me`, `us`),
		`dm`, `dms`, `hmu`,
	)) + `\b|(?:^|[ \t\n,;:(])@[\pL\pN_][\pL\pN_.]{2,}`)
	// appOwners, before an accountApp, and appTags, after one, make it an
	// account: "my insta", "snap me", "telegram id".
	appOwners = setOf(`my mera meri mere your ur`)
	appTags   = setOf(`me us id handle username acc account`)
	// phoneLabel, with nothing but punctuation between it and a number, names
	// the line it is: "Google Voice: 2954820174", "cell no. 2954820174".
	phoneLabel = newWindowSearch(`\b` + phrase(words(`phone`, `ph`, `cell`, `mobile`, `mob`, `tel`, `voice`,
		`landline`)+`(?:~`+words(`no`, `number`, `num`)+`)?(?:~`+words(`is`, `hai`)+`)?`) + `[^\pL\pN]*$`)
	// evasionCue says a number or an address is written oddly to get it past
	// a filter: "split it so the mods dont catch", "spaced so it doesnt
	// autofill", "so bots can't read it".
	evasionCue = newWindowSearch(`\b` + phrase(words(`so`, `that`)+`~(?:the~)?`+words(`mods?`, `moderators?`,
		`admins?`, `bots?`, `filters?`, `scrapers?`, `spammers?`, `it`, `they`, `nobody`, `no~one`, `ai`)+`~`+
		words(`don'?t`, `do~not`, `can'?t`, `cannot`, `can~not`, `won'?t`, `will~not`, `doesn'?t`, `does~not`)+`~`+
		words(`catch`, `read`, `see`, `detect`, `find`, `flag`, `notice`, `scrape`, `autofill`, `block`, `ban`,
			`pick`)) + `\b`)
	// reverseCue says the digits are written backwards: "my number in
	// reverse", "backwards", "ulta".
	reverseCue = newWindowSearch(`\b` + words(`reverse`, `reversed`, `backwards?`, `ulta`, `ulte`, `ulti`) + `\b`)
	// areaCode gives a North American number's area code apart from the
	// rest of it: "482-0196 (area code 404)".
	areaCode = newWindowSearch(phrase(`\barea~code~(?:is~)?`) + `(\d[ \t]?\d[ \t]?\d)\b`)
	// accountApp is an app whose account is a phone number: the messaging
	// apps, and the payment apps an Indian account is opened with a number on.
	// Before "acc" or "account", it makes the number after them that phone
	// number: "my whatsapp acc 9876543210".
	accountApp = regexp.MustCompile(`^` + phrase(words(contactApps, `signal`, `wa`, `gpay`, `phone~?pe`, `paytm`,
		`upi`, `bhim`, `mobikwik`, `freecharge`, `imo`)) + `$`)
	accountLabels = setOf(`account acc acct a/c app`)
	payBrands     = setOf(`google amazon samsung phone`)
)

// classifyNumber decides what a run is, in the context it sits in. Digits
// spaced out one at a time are read in the groups any dashes or points among
// them make (regrouped), and a run said to be written backwards is read the
// right way round too.
func classifyNumber(v *view, run digitRun) (findingKind, bool) {
	n := len(run.digits)
	if n < 7 || n > 17 {
		return 0, false
	}
	shaped := regrouped(run)
	local := n == 7 && localNumber(shaped)
	short := n == 8 && sgMobile(v, shaped)
	if n < 9 && !local && !short {
		return 0, false
	}
	if local && slashAreaCoded(v, shaped) {
		return kindPhone, true
	}
	alone, cued := standsAlone(v, run)
	if !alone || formattedNumber(run) || dataSet(v, run) || roundRange(run) || decimalRange(v, run) ||
		ipv4(v, run) {
		return 0, false
	}
	kind, ok := numberKind(v, run, shaped, local, short)
	// A number given as a maths answer is none, unless a request to call or
	// text comes straight before it. It is asked last, of a number that is
	// one, because it reads the text both sides of the number: asked of every
	// number a post of numbers holds, it took most of the post's time.
	if !ok || !cued && mathsAnswer(v, run) {
		return 0, false
	}
	return kind, true
}

// numberKind is classifyNumber's reading of a run that stands alone: a local
// (seven-digit) number with an area code given, a short (eight-digit)
// mobile, a number of a known shape, or one said to be written backwards
// whose digits the right way round are a phone number.
func numberKind(v *view, run, shaped digitRun, local, short bool) (findingKind, bool) {
	if local {
		return kindPhone, areaCoded(v, shaped)
	}
	if short {
		return kindPhone, true
	}
	if kind, ok := numberShape(v, shaped); ok {
		return kind, true
	}
	if !run.plus {
		near := windowBefore(v.text, run.from, 64)
		if reverseCue.matchIn(v, near) && callCue.matchIn(v, near) {
			if back := reversed(shaped); !counting(string(back.digits)) {
				if kind, ok := numberShape(v, back); ok && kind == kindPhone {
					return kindPhone, true
				}
			}
		}
	}
	return 0, false
}

// numberShape reports the kind of number a run's digits and grouping make.
func numberShape(v *view, run digitRun) (findingKind, bool) {
	d := string(run.digits)
	if dramaNumber(d) || helpline(d) || freephone(d, run.groups) || nanpFiction(d, run.groups) || repdigit(d) {
		return 0, false
	}
	switch {
	case run.plus:
		return kindPhone, internationalPhone(d)
	case strings.HasPrefix(d, "00") && run.groups[0] <= 4:
		if internationalPhone(d[2:]) {
			return kindPhone, true
		}
	}
	if landline(d, run.groups) && officeLine(v, run) {
		return 0, false
	}
	switch {
	case indianMobile(d, run.groups, run.seps), nanpPhone(v, run), ukPhone(d, run.groups, run.seps),
		auMobile(d, run.groups, run.seps), auLandline(d, run.groups), tenDigitMobile(d, run.groups, run.seps),
		tenDigitCalled(v, run), calledMobile(v, run):
		return kindPhone, true
	case aadhaar(d, run.groups, run.seps), idLabelled(v, run, aadhaarLabels) && aadhaarShape(d, run.groups, run.seps):
		return kindAadhaar, true
	case ssn(d, run.groups, run.seps), idLabelled(v, run, ssnLabels) && ssnShape(d, run.groups, run.seps, " "):
		return kindSSN, true
	}
	return 0, false
}

// landline reports a fixed line written the national way, which may be an
// organisation's: a UK (or Indian) number starting 01, 02 or 03 in more than
// one group, or an Australian landline (auLandline). A mobile is a person's.
func landline(d string, g []int) bool {
	return len(d) == 11 && d[0] == '0' && strings.IndexByte("123", d[1]) >= 0 && len(g) > 1 || auLandline(d, g)
}

// officeLine reports a landline named, in the four words before it, as an
// organisation's: "UNSW switchboard is (02) 9385 1000", "UQ admissions (07)
// 3365 1111", "Uni of Adelaide (08) 8313 4455", "headspace national office
// (03) 9027 0100". A line said to be the poster's ("my office", "home
// number"), or one a callCue asks the reader to call the poster on ("call me
// at uni"), is not.
func officeLine(v *view, run digitRun) bool {
	if callCue.matchIn(v, windowBefore(v.text, run.from, 40)) {
		return false
	}
	office := false
	for _, w := range wordsBefore(v.text, run.from, 4) {
		if personalLines[w] {
			return false
		}
		office = office || officeWords[w]
	}
	return office
}

var (
	officeWords = setOf(`switchboard admissions admission reception enquiries enquiry inquiries inquiry helpdesk
		office offices department dept faculty registrar university uni college campus helpline hotline`)
	personalLines = setOf(`my mine mera meri mere our home personal private me`)
)

// regrouped reads digits spaced out one at a time, with a dash or a point
// between some of them, in the groups those marks set off: "8 4 3 2 - 1 0 9 7
// 6 5" is 8432-109765, and "6 1 4 - 3 9 - 2 8 5 7" is 614-39-2857. Any other
// run is returned as it is.
func regrouped(run digitRun) digitRun {
	if len(run.groups) < 3 {
		return run
	}
	for _, g := range run.groups {
		if g != 1 {
			return run
		}
	}
	mark := ""
	groups, seps := []int{1}, []string(nil)
	for _, s := range run.seps {
		m := strings.Trim(s, " \n")
		switch {
		case m == "":
			groups[len(groups)-1]++
			continue
		case len(m) > 1 || mark != "" && m != mark:
			return run
		}
		mark = m
		groups = append(groups, 1)
		seps = append(seps, m)
	}
	if mark == "" || len(groups) == len(run.groups) {
		return run
	}
	out := run
	out.groups, out.seps = groups, seps
	return out
}

// reversed is a run read backwards.
func reversed(run digitRun) digitRun {
	out := run
	out.digits = slices.Clone(run.digits)
	slices.Reverse(out.digits)
	out.groups = slices.Clone(run.groups)
	slices.Reverse(out.groups)
	out.seps = slices.Clone(run.seps)
	slices.Reverse(out.seps)
	out.paren = false
	return out
}

// localNumber is a North American number without its area code, written to
// be dialled: 482-0196, 482.0196.
func localNumber(run digitRun) bool {
	g := run.groups
	if len(g) != 2 || g[0] != 3 || g[1] != 4 || run.digits[0] < '2' {
		return false
	}
	sep := strings.Trim(run.seps[0], " ")
	return sep == "-" || sep == "."
}

// areaCoded reports a local number with its area code given apart, just
// after it or just before: "482-0196 (area code 404)", "area code 718, my # is
// 482-0139". The two make a North American number.
func areaCoded(v *view, run digitRun) bool {
	for _, w := range []string{windowAfter(v.text, run.to, 32), windowBefore(v.text, run.from, 40)} {
		if m := areaCode.findIn(v, w); m != nil {
			area := strings.ReplaceAll(strings.ReplaceAll(w[m[2]:m[3]], " ", ""), "\t", "")
			return nanp(area + string(run.digits))
		}
	}
	return false
}

// slashAreaCoded reports a local number with its area code set off by a
// slash straight before it, after a word naming the line or asking for a
// call: "phone 495/418-3927", "call me on 495 / 418-3927". The slash is a
// sum's or a ratio's as well, so the word has to be there: "120/555-1234"
// alone is not one.
func slashAreaCoded(v *view, run digitRun) bool {
	t := v.text
	i := run.from
	for i > 0 && (t[i-1] == ' ' || t[i-1] == '\t') {
		i--
	}
	if i == 0 || t[i-1] != '/' {
		return false
	}
	i--
	for i > 0 && (t[i-1] == ' ' || t[i-1] == '\t') {
		i--
	}
	if i < 3 || i > 3 && (isASCIIDigit(t[i-4]) || isASCIILetterByte(t[i-4]) || strings.IndexByte("/.,", t[i-4]) >= 0) {
		return false
	}
	area := t[i-3 : i]
	if !isASCIIDigit(area[0]) || !isASCIIDigit(area[1]) || !isASCIIDigit(area[2]) {
		return false
	}
	if !nanp(area + string(run.digits)) {
		return false
	}
	near := windowBefore(t, i-3, 32)
	return phoneLabel.matchIn(v, near) || contactCue.matchIn(v, near) || callCue.matchIn(v, near)
}

// piecedNumber reads a phone number given in pieces from t[i], where the
// post says whose number it is or that it is written to get past the
// filter: three or more runs of digits joined by a word ("my num starts
// with 99 then 30 then 412 then 875"), by a slash, a bar or a comma ("split
// it so the mods dont catch: 98 / 1 / 67 / 45 / 231"), or quoted strings
// joined by "+" ("PHONE = '+91' + '98450' + '17723' use this to reach me");
// or two such runs straight after a contactCue ("call me on 98450/12345",
// "my number is 98450 then 12345"). An unquoted "+" is a sum, and a list of
// values is a list, so a word that only says a number is near ("number",
// "phone") will do only for the quoted strings, which are put together
// rather than added, and a dataWords word just before the pieces makes them
// values whatever cue is near ("hmu if you get stuck on questions 87, 88,
// 91, 92, 93", "my scores went 98 then 76 then 84 …, call me consistent"),
// unless the cue comes straight before them.
func piecedNumber(v *view, i int) (int, int, bool) {
	t := v.text
	quoted := i > 0 && strings.IndexByte(quoteMarks, t[i-1]) >= 0
	var digits []byte
	plus, pieces, end := false, 0, i
	for j := i; pieces < maxPieces && len(digits) <= maxPiecedDigits; pieces++ {
		k := j
		if quoted && pieces > 0 {
			if k >= len(t) || t[k] != t[i-1] {
				break
			}
			k++
		}
		if pieces == 0 && k < len(t) && t[k] == '+' {
			plus = true
			k++
		}
		s := k
		for k < len(t) && k-s < 6 && isASCIIDigit(t[k]) {
			k++
		}
		if k == s || k < len(t) && (isASCIIDigit(t[k]) || 'a' <= t[k] && t[k] <= 'z' || t[k] >= utf8.RuneSelf) {
			break
		}
		digits = append(digits, t[s:k]...)
		// A piece spaced out a digit at a time: "07 9 1 5 / 3 3 0 / 2 1 8".
		for n := k - s; !quoted && n < 6 && k+1 < len(t) && t[k] == ' ' && isASCIIDigit(t[k+1]) &&
			(k+2 == len(t) || !isASCIIDigit(t[k+2]) && !('a' <= t[k+2] && t[k+2] <= 'z') && t[k+2] < utf8.RuneSelf); n++ {
			digits = append(digits, t[k+1])
			k += 2
		}
		if quoted {
			if k >= len(t) || t[k] != t[i-1] {
				break
			}
			k++
		}
		end = k
		j = pieceJoiner(t, k, quoted)
		if j < 0 {
			pieces++
			break
		}
	}
	v.work += end - i
	if pieces < 2 || !piecedPhone(string(digits), plus) {
		return 0, 0, false
	}
	before, after := windowBefore(t, i, 64), windowAfter(t, end, 48)
	near := windowBefore(t, i, 32)
	if quoted {
		near = windowBefore(t, i-1, 32)
	}
	cued := contactCue.matchIn(v, near)
	if pieces == 2 && !cued {
		return 0, 0, false
	}
	listed := !cued && !quoted && dataWords.matchIn(v, near)
	if cued || !listed && (callCue.matchIn(v, before) || callCue.matchIn(v, after)) || evasionCue.matchIn(v, before) ||
		evasionCue.matchIn(v, after) || quoted && phoneCue.matchIn(v, before) {
		if quoted {
			i-- // the opening quote
		}
		return i, end, true
	}
	return 0, 0, false
}

// halvedNumber reads a phone number given in two parts, each labelled as
// which part it is: "first 5: 93104 / last 5: 72286", "first half 98450,
// second half 12345". Labels like that say the parts make one number, so
// no other cue is needed. It returns where the second part ends.
func halvedNumber(v *view, run digitRun) (int, bool) {
	t := v.text
	if len(run.groups) != 1 || len(run.digits) < 2 || len(run.digits) > 6 ||
		!partLabelled(wordsBefore(t, run.from, 3), firstParts) {
		return 0, false
	}
	rest := t[run.to:min(len(t), run.to+32)]
	ws := nextWords(rest, 4)
	v.work += len(rest)
	for k := 0; k+1 < len(ws); k++ {
		if !lastParts[ws[k]] {
			continue
		}
		n := k + 1
		if partSizes[ws[n]] || len(ws[n]) == 1 && isASCIIDigit(ws[n][0]) {
			n++
		}
		if n >= len(ws) {
			return 0, false
		}
		d := ws[n]
		if len(d) < 2 || len(d) > 6 || strings.Trim(d, "0123456789") != "" {
			return 0, false
		}
		at := strings.Index(rest, ws[k]) + len(ws[k])
		end := run.to + at + strings.Index(rest[at:], d) + len(d)
		if end < len(t) && isASCIIDigit(t[end]) || !piecedPhone(string(run.digits)+d, false) {
			return 0, false
		}
		return end, true
	}
	return 0, false
}

// partLabelled reports a label for the first part of a number in ws, the
// words before it nearest first: "first", "first 5", "first 5 digits",
// "first half".
func partLabelled(ws []string, parts map[string]bool) bool {
	for _, w := range ws {
		switch {
		case parts[w]:
			return true
		case partSizes[w] || len(w) == 1 && isASCIIDigit(w[0]):
		default:
			return false
		}
	}
	return false
}

var (
	firstParts = setOf(`first 1st starts start beginning`)
	lastParts  = setOf(`last second 2nd next rest then ends end ending other`)
	partSizes  = setOf(`half part bit digits nums numbers four five six`)
)

// quoteMarks are the quotes a string in code or a word in a sentence is put
// in.
const quoteMarks = `'"` + "`"

// maxPieces is the most pieces piecedNumber reads, and maxPiecedDigits the
// most digits: a phone number is at most 17 digits (classifyNumber), so
// one digit at a time is the most pieces it can be cut into ("call me 9 / 8
// / 4 / 5 / 0 / 1 / 7 / 7 / 2 / 3"), and more digits than that are not one.
const (
	maxPieces       = 17
	maxPiecedDigits = 17
)

// pieceJoiners are the words a number given in pieces is joined with: "99
// then 30 then 412", "98450 aur 17723", "followed by", "and ends in".
var pieceJoiners = setOf(`then and plus aur phir fir followed ends ending`)

// pieceJoiner reads what joins one piece of a number to the next, from
// t[k]: a space or two, then "+" between quoted strings, or a slash, a bar,
// a comma or a joining word ("and then", "followed by") between bare ones,
// then a space or two. It returns where the next piece starts, or -1.
func pieceJoiner(t string, k int, quoted bool) int {
	skip := func(k int) int {
		for n := 0; n < 2 && k < len(t) && t[k] == ' '; n++ {
			k++
		}
		return k
	}
	k = skip(k)
	if k >= len(t) {
		return -1
	}
	switch {
	case quoted:
		if t[k] != '+' {
			return -1
		}
		k++
	case strings.IndexByte("/|,", t[k]) >= 0:
		k++
		// "76, then 1180, and ends in 5249": a word may follow the mark.
		if j := skip(k); j < len(t) && pieceJoiners[firstWordAfter(t, j)] {
			return pieceJoiner(t, j, false)
		}
	default:
		for n := 0; n < 2; n++ {
			w := firstWordAfter(t, k)
			if !pieceJoiners[w] {
				if n == 0 {
					return -1
				}
				break
			}
			k = skip(k + len(w))
			switch w {
			case "followed":
				if !strings.HasPrefix(t[k:], "by") {
					return -1
				}
				k += 2
			case "ends", "ending":
				// "and ends in 5249", "ending with 5249".
				switch {
				case strings.HasPrefix(t[k:], "in"):
					k += 2
				case strings.HasPrefix(t[k:], "with"):
					k += 4
				default:
					return -1
				}
			}
		}
	}
	k = skip(k)
	if k == len(t) {
		return -1
	}
	return k
}

// piecedPhone reports digits put together from pieces that make a phone
// number, the North American kind included, since a word has already said
// they are one.
func piecedPhone(d string, plus bool) bool {
	if dramaNumber(d) || helpline(d) || counting(d) {
		return false
	}
	if plus {
		return internationalPhone(d)
	}
	one := []int{len(d)}
	return indianMobile(d, one, nil) || ukPhone(d, one, nil) || auMobile(d, one, nil) || nanp(d) ||
		len(d) == 11 && d[0] == '1' && nanp(d[1:])
}

// calledMobile is an Indian mobile in any grouping of two or more digits at a
// time, one group of a single digit allowed among them, after a word that
// says it is a number to call: "call me 93 22 41 87 60", "call me" and a pair
// on each line. Without the word, a run of pairs is as often a list of marks
// (mobileGrouping).
func calledMobile(v *view, run digitRun) bool {
	if len(run.groups) > 5 || !indianMobile(string(run.digits), []int{len(run.digits)}, nil) {
		return false
	}
	singles := 0
	for _, g := range run.groups {
		if g < 2 {
			singles++
		}
	}
	return singles <= 1 && callCue.matchIn(v, windowBefore(v.text, run.from, 40))
}

// aadhaarLabels and ssnLabels, as the word before a number (fillers such as
// "no" and "my" aside), say which ID number it is: "aadhaar no 6824 3317
// 9052", "ssn 418 62 7351". The label is the evidence then, not the check
// digit or the dashes, since a number posted as one's Aadhaar is one whether
// or not it was typed right.
var (
	aadhaarLabels = setOf(`aadhaar aadhar adhaar adhar aadhaarcard aadharcard uid uidai`)
	ssnLabels     = setOf(`ssn ssns`)
	idFillers     = setOf(`no nos number num nmbr id is was hai my mera meri card`)
)

// idLabelled reports one of labels as the word before a run, with idFillers
// skipped, or "social security" for ssnLabels.
func idLabelled(v *view, run digitRun, labels map[string]bool) bool {
	for k, w := range wordsBefore(v.text, run.from, 5) {
		switch {
		case idFillers[w]:
			continue
		case labels[w]:
			return true
		}
		ws := wordsBefore(v.text, run.from, k+2)
		return w == "security" && len(ws) == k+2 && ws[k+1] == "social" && labels["ssn"]
	}
	return false
}

// dataSet reports a run of three or more short groups that a word just before
// it says is a set of values, not a number spaced out to be read. A run that
// starts with "+" starts with a country code, which no set of values does:
// "past questions sharing, +234 803 123 4567".
func dataSet(v *view, run digitRun) bool {
	if len(run.groups) < 3 || run.plus {
		return false
	}
	for _, g := range run.groups {
		if g > 4 {
			return false
		}
	}
	return dataWords.matchIn(v, windowBefore(v.text, run.from, 32))
}

// roundRange reports two amounts set off by one dash, the second the larger,
// each of them round or written with thousands separators: "60000-80000 per
// month" is a salary and "~70,000–80,000 killed" a count, not a phone number.
func roundRange(run digitRun) bool {
	k := -1
	for i, s := range run.seps {
		if strings.Trim(s, " ") == "-" {
			if k >= 0 {
				return false
			}
			k = i
		}
	}
	if k < 0 {
		return false
	}
	split := 0
	for _, g := range run.groups[:k+1] {
		split += g
	}
	side := func(digits []byte, groups []int, seps []string) (uint64, bool) {
		amount := len(seps) == 0 && strings.HasSuffix(string(digits), "00") ||
			len(seps) > 0 && seps[0] == "," && formattedNumber(digitRun{digits: digits, groups: groups, seps: seps})
		n, err := strconv.ParseUint(string(digits), 10, 64)
		return n, amount && err == nil
	}
	x, okA := side(run.digits[:split], run.groups[:k+1], run.seps[:k])
	y, okB := side(run.digits[split:], run.groups[k+1:], run.seps[k+1:])
	return okA && okB && y > x
}

// decimalRange reports two decimals set off by a dash, with no phone cue
// before them: "656.28-686.72 nm", "96.123 - 97.456".
func decimalRange(v *view, run digitRun) bool {
	return len(run.groups) == 4 && run.seps[0] == "." && run.seps[2] == "." && strings.Trim(run.seps[1], " ") == "-" &&
		!phoneCue.matchIn(v, windowBefore(v.text, run.from, 32))
}

// ipv4 reports an IPv4 address: four groups of up to three digits, each 255
// or less, set off by points alone, with no phone cue before it ("ping
// 93.184.216.34", not "call 98.123.145.67").
func ipv4(v *view, run digitRun) bool {
	if len(run.groups) != 4 || run.plus || run.paren {
		return false
	}
	for _, s := range run.seps {
		if s != "." {
			return false
		}
	}
	at := 0
	for _, g := range run.groups {
		if g > 3 {
			return false
		}
		n, err := strconv.Atoi(string(run.digits[at : at+g]))
		if err != nil || n > 255 {
			return false
		}
		at += g
	}
	return !phoneCue.matchIn(v, windowBefore(v.text, run.from, 32))
}

// formattedNumber reports a number written the way a quantity is: a decimal,
// one point with nothing round it ("63.43494882", a calculator's answer), or
// thousands set off by commas or by points alone ("9,876,543,210"). Nobody
// writes a phone number either way.
func formattedNumber(run digitRun) bool {
	if len(run.seps) == 0 {
		return false
	}
	if len(run.seps) == 1 && run.seps[0] == "." {
		return true
	}
	sep := run.seps[0]
	if sep != "," && sep != "." {
		return false
	}
	for _, s := range run.seps {
		if s != sep {
			return false
		}
	}
	if run.groups[0] > 3 {
		return false
	}
	for _, g := range run.groups[1:] {
		if g != 3 {
			return false
		}
	}
	return true
}

// standsAlone reports that a run is a number in its own right and not one side
// of a sum, a decimal, a quantity, a function's argument or a labelled
// reference, and whether a request to call or text comes straight before it
// (cued), which classifyNumber needs again.
func standsAlone(v *view, run digitRun) (alone, cued bool) {
	t := v.text
	after := t[run.to:]
	if r, _ := utf8.DecodeRuneInString(after); unicode.IsDigit(r) || unicode.IsLetter(r) && !run.apartAfter {
		return false, false
	}
	if operandAfter(after) {
		return false, false
	}
	// "0.9876543210" is a decimal, not a number with a trunk prefix.
	if len(run.groups) > 1 && run.groups[0] == 1 && run.digits[0] == '0' && run.seps[0] == "." {
		return false, false
	}
	if len(after) > 1 && (after[0] == '.' || after[0] == ',') && isASCIIDigit(after[1]) {
		return false, false
	}
	before := t[:run.from]
	// A request to call or text, straight before the number, says what it is
	// whatever else is near it: "call me = 9876543210", "wa/9876543210", "text
	// me 9876543210 if you can do the integers question".
	cued = contactCue.matchIn(v, windowBefore(t, run.from, 32))
	if !cued && operandBefore(v, before, after) {
		return false, false
	}
	// A sign glued to an operand is arithmetic: "2+9876543210", "x-9876543210".
	if !run.plus && len(before) >= 2 && (before[len(before)-1] == '+' || before[len(before)-1] == '-') {
		if r, _ := utf8.DecodeLastRuneInString(before[:len(before)-1]); unicode.IsLetter(r) || unicode.IsDigit(r) || r == ')' {
			return false, false
		}
	}
	// A spaced sign with an operand before it: "1 + 9876543210", "x - 9876543210";
	// "call me - 9876543210" has a word there, not an operand.
	if s := strings.TrimRight(before, " "); len(s) < len(before) && len(s) >= 2 && (s[len(s)-1] == '+' || s[len(s)-1] == '-') {
		if o := strings.TrimRight(s[:len(s)-1], " "); len(o) < len(s)-1 && len(o) > 0 {
			r, n := utf8.DecodeLastRuneInString(o)
			p, _ := utf8.DecodeLastRuneInString(o[:len(o)-n])
			if unicode.IsDigit(r) || r == ')' || unicode.IsLetter(r) && !unicode.IsLetter(p) {
				return false, false
			}
		}
	}
	if len(before) >= 2 {
		last, prev := before[len(before)-1], before[len(before)-2]
		// A decimal point or thousands separator glued to a digit before it.
		if (last == '.' || last == ',') && isASCIIDigit(prev) {
			return false, false
		}
		// A bracket glued to a name: a function's argument or an index,
		// "print(7777777777)", "a[9876543210]".
		if r, _ := utf8.DecodeLastRuneInString(before[:len(before)-1]); (last == '(' || last == '[') &&
			(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == ')' || r == ']') {
			return false, false
		}
	}
	// An amount: "$9876543210", "₹ 9876543210".
	if r, _ := utf8.DecodeLastRuneInString(strings.TrimRight(before, " ")); strings.ContainsRune("$₹€£¥", r) {
		return false, false
	}
	if labelled(t, run.from) || quantityWords[firstWordAfter(t, run.to)] {
		return false, false
	}
	near := windowBefore(t, run.from, 32)
	if !cued && mathsResult.matchIn(v, near) {
		return false, false
	}
	// A number a maths question is about, or given as an answer, is none,
	// unless a way to reach the poster is near it: "is 9999999967 prime?",
	// not "the answer is 9845012345 dm me" or "text me, the final answer is
	// 9845012345".
	if !cued && mathsContext.matchIn(v, near+" "+windowAfter(t, run.to, 32)) && !reachedNear(v, run) {
		return false, false
	}
	return true, cued
}

// reachedNear reports a way to reach someone within 64 bytes of a run on
// either side: a call or a number said to be someone's (callCue,
// ownNumber), a message asked for or a handle (reachCue), or an account on
// an app, an accountApp with one of appOwners before it or appTags after it
// ("my insta is in bio", "telegram id in bio"); not an app named for itself,
// "saw this on reddit: is 9845012345 prime?".
func reachedNear(v *view, run digitRun) bool {
	wide := windowBefore(v.text, run.from, 64) + " " + windowAfter(v.text, run.to, 64)
	if callCue.matchIn(v, wide) || ownNumber.matchIn(v, wide) || reachCue.matchIn(v, wide) {
		return true
	}
	v.work += len(wide)
	ws := wordsIn(wide)
	for i, w := range ws {
		if (i > 0 && appOwners[ws[i-1]] || i+1 < len(ws) && appTags[ws[i+1]]) && accountApp.MatchString(w) {
			return true
		}
	}
	return false
}

// mathsAnswer reports a number given as the answer to a maths question in
// the words people give one, with no way to reach anyone round it
// (reachedNear: a call or a message asked for, "dm me", "add me", an app, a
// handle, or a number said to be someone's): a largest or smallest number of some kind ("the
// largest 10-digit number with distinct digits is 9876543210"), a result
// that "came out as" or was got "for part (b)", the answer key's, a final
// answer or the answer to a part or a question, one "according to the
// solutions", a number after a power or factorial in the sentence before
// ("How many digits does 2^33 have? It's 8589934592"); and digits that count
// up or down by one (0123456789, 9876543210) beside talk of digits, examples
// or a program's input and output. A phone number is given with none of
// these, so they are looked for first, and a way to reach anyone only once
// one is found.
func mathsAnswer(v *view, run digitRun) bool {
	t := v.text
	before := windowBefore(t, run.from, 64)
	wide := before + " " + windowAfter(t, run.to, 64)
	if !mathsResultWords.matchIn(v, wide) && !powerBefore.matchIn(v, before) &&
		!(counting(string(run.digits)) && countingTalk.matchIn(v, wide)) {
		return false
	}
	return !reachedNear(v, run)
}

var (
	// mathsResultWords give a number as a maths result.
	mathsResultWords = newWindowSearch(phrase(`\b(?:` + words(
		words(`largest`, `smallest`, `biggest`, `greatest`, `least`)+`~(?:\d+~)?`+words(`digit`, `square`,
			`cube`, `prime`, `number`, `integer`, `palindrome`),
		`\d+~digits?~`+words(`numbers?`, `integers?`, `answers?`),
		`perfect~`+words(`squares?`, `cubes?`, `numbers?`),
		`answer~key`, words(`came`, `comes`, `come`)+`~out~`+words(`as`, `to`), `for~part`, `pandigital`,
		`self~descriptive`, `calculator`,
		// "the answer to part c is", "the final answer", "according to the
		// solutions".
		`final~answers?`, `answers?~`+words(`to`, `for`)+`~(?:the~)?`+words(`part`, `q\d*`, `question`, `qn`,
			`ques`, `problem`, `sum`), words(`the`, `my`)+`~answer~`+words(`is`, `was`, `came`),
		`according~to~the~`+words(`solutions?`, `answers?`, `book`, `textbook`)) + `)\b`))
	// powerBefore is a power or a factorial written before a number.
	powerBefore = newWindowSearch(`\d[ \t]*(?:\^|\*\*)[ \t]*\d|\d!`)
	// ownNumber names a number as someone's: "my 10 digit number is".
	ownNumber = newWindowSearch(`\b` + words(`my`, `mera`, `meri`, `mere`, `your`, `ur`, `his`, `her`, `their`) +
		`\b[^.?!\n]{0,24}\b` + words(`number`, `no`, `num`, `contact`, `phone`, `mobile`) + `\b`)
	// countingTalk, beside digits that count up or down, makes them an
	// example: "e.g. 9876543210 is the largest", "input 1234567890".
	countingTalk = newWindowSearch(`\b(?:` + words(`digits?`, `e\.?g`, `example`, `examples`, `input`, `output`,
		`program`, `reverse`, `reversed`, `largest`, `smallest`, `biggest`, `ascending`, `descending`, `order`) +
		`)\b`)
)

// operandAfter reports an operator after a number with an operand after it:
// "= 2", "/ 2", "% of", "+ 1". An emoticon ("=)", "^^", "<3"), an arrow
// pointing back at it ("<-") or a sarcasm tag ("/s") is not one.
func operandAfter(after string) bool {
	s := strings.TrimLeft(after, " ")
	spaced := len(s) < len(after)
	r, size := utf8.DecodeRuneInString(s)
	switch {
	case r == '%':
		return true
	case r == '<' && strings.HasPrefix(s, "<3") && !(len(s) > 2 && isASCIIDigit(s[2])):
		return false
	case r == '<' && strings.HasPrefix(s, "<-"):
		// An arrow pointing back at it: "1-793-502-8814 <- text me".
		return false
	case r == '/' && spaced && strings.HasPrefix(s, "/s") &&
		(len(s) == 2 || !unicode.IsLetter(rune(s[2])) && !isASCIIDigit(s[2])):
		return false
	case r == '+' || r == '-':
		// Only spaced, so that "9876543210-ask for ravi" and a range of
		// pages stay what they were: "9876543210 + 1", "9876543210 - x".
		if !spaced || !strings.HasPrefix(s[size:], " ") {
			return false
		}
		rest := strings.TrimLeft(s[size:], " ")
		o, n := utf8.DecodeRuneInString(rest)
		if unicode.IsDigit(o) || o == '(' {
			return true
		}
		next, _ := utf8.DecodeRuneInString(rest[n:])
		return unicode.IsLetter(o) && !unicode.IsLetter(next)
	case !strings.ContainsRune(mathsAfter, r):
		return false
	}
	rest := s[size:]
	if len(rest) > 0 && strings.IndexByte("=*/<>", rest[0]) >= 0 {
		rest = rest[1:]
	}
	rest = strings.TrimLeft(rest, " ([{-")
	o, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsLetter(o) || unicode.IsDigit(o)
}

// operandBefore reports an operator before a number with an operand before it:
// "x = 9876543210", "x >= 9876543210", "1/9876543210", and R's assignment
// arrow, "x <- 9876543210" or "x <<- 9876543210", which assigns as "=" does
// when a variable stands before it (rAssigns). A greentext ">" or a quote
// link's ">>" at the start of a line has nothing before it, an arrow ("->",
// "=>") points rather than compares, and a phone cue before an "=" or an
// arrow ("number = 9876543210", "num <- 9876543210") is a label. after is
// the text after the number.
func operandBefore(v *view, before, after string) bool {
	// An opening bracket after "=" still leaves the number the right-hand side
	// of something: "x = (9876543210 + 1)", "arr = [9876543210, …]".
	s := strings.TrimRight(before, " ([{")
	if strings.HasSuffix(s, "<-") {
		s = strings.TrimSuffix(s[:len(s)-2], "<")
		if !rAssigns(v, s, after) {
			return false
		}
	} else {
		r, size := utf8.DecodeLastRuneInString(s)
		if !strings.ContainsRune(mathsBefore, r) {
			return false
		}
		s = s[:len(s)-size]
		if r == '>' && len(s) > 0 && (s[len(s)-1] == '-' || s[len(s)-1] == '=') {
			return false
		}
		if len(s) > 0 && strings.IndexByte("=*/<>!", s[len(s)-1]) >= 0 {
			s = s[:len(s)-1]
		}
	}
	s = strings.TrimRight(s, " ")
	o, _ := utf8.DecodeLastRuneInString(s)
	if !(unicode.IsLetter(o) || unicode.IsDigit(o) || o == ')' || o == ']') {
		return false
	}
	return !phoneCue.matchIn(v, lastWord(s))
}

// rAssigns reports R's "<-" as an assignment to a variable, s being the text
// before the arrow and after the text after the number: "x <- 9876543210",
// "[code]total <- 9876543210[/code]", "why does x <- 9876543210 print as
// 9.876543e+09?". In chat an arrow points as often, "add me <- …", so the
// name must be one only code has (one letter, or with "_", ".", or a digit
// in it, or R's "$" or "@": "df$id") or start a line or a statement (after
// ";", "{", "(", ":" or ","), and must not be a word that points at the
// poster or the number ("me", "mine", "here", "digits"). A phone word in
// the rest of its line ("whatsapp", "text only", "hmu") makes it the
// poster's number after all, and so does a call cue in a comment ("# my
// number", "# call me"), where a phone word alone only says what the value
// is ("# number of rows").
func rAssigns(v *view, s, after string) bool {
	s = strings.TrimRight(s, " \t")
	i := len(s)
	for i > 0 {
		c := s[i-1]
		if !(isASCIILetterByte(c) || isASCIIDigit(c) || strings.IndexByte("_.$@", c) >= 0) {
			break
		}
		i--
	}
	name := s[i:]
	if name == "" || !isASCIILetterByte(name[0]) && name[0] != '.' || arrowPointers[strings.ToLower(name)] {
		return false
	}
	lead := strings.TrimRight(s[:i], " \t")
	opens := lead == "" || strings.IndexByte("\n;{(:,", lead[len(lead)-1]) >= 0
	coded := len(name) == 1 || strings.ContainsAny(name, "_.$@0123456789")
	if !opens && !coded {
		return false
	}
	line, _, _ := strings.Cut(windowAfter(after, 0, 48), "\n")
	// A comment says what the value is ("# number of rows"), so only a
	// call cue there, "# call me", is read as one.
	code, comment, _ := strings.Cut(line, "#")
	return !phoneCue.matchIn(v, code) && !callCue.matchIn(v, comment)
}

// isASCIILetterByte reports an ASCII letter.
func isASCIILetterByte(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// arrowPointers, before "<-", point at what follows, or say whose it is,
// rather than name a variable: "add me <- 9123456780", "here <- 9123456780
// hmu", "digits <- 9123456780".
var arrowPointers = setOf(`me mine my us our ours you u ur your yours here there this that these those it him her
	his hers them their digits deets details info`)

// labelled reports a label as the word just before a number, with the fillers
// that go between them skipped: "order 7834567890", "seat number 7012345678",
// "JEE app no. 240310123456", "admit card 9876543210". A label may carry its
// edition ("ISBN-10: 8177091875", "ISBN13 9789351199465"), and a few
// labelLinks may come between ("PNR status for 8177091875").
func labelled(t string, from int) bool {
	if labelledSubject(t, from) {
		return true
	}
	ws := wordsBefore(t, from, 6)
	links := 0
	for k, w := range ws {
		switch {
		case numberFillers[w]:
			continue
		case accountLabels[w] && accountAppBefore(ws[k+1:]):
			return false // "my whatsapp acc 9876543210", "gpay account no 9876543210"
		case numberLabels[w] || w == "card" && k+1 < len(ws) && ws[k+1] == "admit":
			return true
		case w == "got" && slices.Contains(ws[k+1:], "expected"):
			return true // "expected 9876543210 got 9876543211"
		case numberLabels[strings.TrimRight(w, "0123456789")]:
			return true // "isbn10"
		case len(w) <= 2 && strings.Trim(w, "0123456789") == "" && k+1 < len(ws) && numberLabels[ws[k+1]]:
			return true // "isbn-10"
		case labelLinks[w] && links < 3:
			links++
			continue
		}
		return false
	}
	return false
}

// accountAppBefore reports an accountApp as the words before an account label,
// nearest first: "whatsapp", or "google pay".
func accountAppBefore(ws []string) bool {
	switch {
	case len(ws) == 0:
		return false
	case accountApp.MatchString(ws[0]):
		return true
	}
	return ws[0] == "pay" && len(ws) > 1 && payBrands[ws[1]]
}

// wordRune is a character of a word for labelled and its helpers: letters,
// digits, and the slash of "a/c".
func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '/' }

// wordsBefore returns up to n whole words ending before i, nearest first,
// from the 64 bytes before it, so a label is never read out of the end of a
// longer word ("frank" is not "rank").
func wordsBefore(t string, i, n int) []string {
	lo := max(0, i-64)
	var out []string
	for j := i; len(out) < n; {
		for j > lo {
			r, size := utf8.DecodeLastRuneInString(t[lo:j])
			if wordRune(r) {
				break
			}
			j -= size
		}
		end := j
		for j > lo {
			r, size := utf8.DecodeLastRuneInString(t[lo:j])
			if !wordRune(r) {
				break
			}
			j -= size
		}
		if j == end {
			break
		}
		if j == lo && lo > 0 {
			if r, _ := utf8.DecodeLastRuneInString(t[:lo]); wordRune(r) {
				break // cut off by the window: not a whole word
			}
		}
		out = append(out, t[j:end])
	}
	return out
}

// lastWord is the word s ends with.
func lastWord(s string) string {
	if ws := wordsBefore(s, len(s), 1); len(ws) == 1 {
		return ws[0]
	}
	return ""
}

// firstWordAfter is the word straight after i, spaces between allowed.
func firstWordAfter(t string, i int) string {
	j := i
	for j < len(t) && t[j] == ' ' {
		j++
	}
	k := j
	for k < len(t) && k-j < 32 {
		r, size := utf8.DecodeRuneInString(t[k:])
		if !unicode.IsLetter(r) {
			break
		}
		k += size
	}
	return t[j:k]
}

// windowBefore and windowAfter are about n bytes of t on one side of i,
// widened to the edge of any word the cut would split.
func windowBefore(t string, i, n int) string {
	lo := max(0, i-n)
	for lo > 0 && !utf8.RuneStart(t[lo]) {
		lo--
	}
	for lo > 0 && lo > i-n-16 {
		r, size := utf8.DecodeLastRuneInString(t[:lo])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			break
		}
		lo -= size
	}
	return t[lo:i]
}

func windowAfter(t string, i, n int) string {
	hi := min(len(t), i+n)
	for hi < len(t) && !utf8.RuneStart(t[hi]) {
		hi++
	}
	for hi < len(t) && hi < i+n+16 {
		r, size := utf8.DecodeRuneInString(t[hi:])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			break
		}
		hi += size
	}
	return t[i:hi]
}

// internationalPhone vets the digits after a "+" or "00": a country code and a
// national number, 8 to 15 digits in all (E.164), with the three countries
// most of educhan's readers dial checked properly.
func internationalPhone(d string) bool {
	n := len(d)
	if n < 8 || n > 15 || d[0] == '0' {
		return false
	}
	switch {
	case strings.HasPrefix(d, "91"):
		return n == 12
	case d[0] == '1':
		return n == 11 && nanp(d[1:])
	case strings.HasPrefix(d, "44"):
		national := strings.TrimPrefix(d[2:], "0") // +44 (0)7911 ...
		return len(national) == 9 || len(national) == 10
	}
	return n >= 9
}

// indianMobile is a ten-digit mobile number starting 6-9, with or without a
// leading 0 or 91, in one of the groupings people write them in.
func indianMobile(d string, groups []int, seps []string) bool {
	g := groups
	switch {
	case len(d) == 12 && strings.HasPrefix(d, "91"):
		d, g = d[2:], dropLeading(g, 2)
	case len(d) == 11 && d[0] == '0':
		d, g = d[1:], dropLeading(g, 1)
	}
	if len(d) != 10 || d[0] < '6' || g == nil {
		return false
	}
	// A round number — 7000000000 people — is a population, not a phone.
	if strings.HasSuffix(d, "000000") {
		return false
	}
	return mobileGrouping(g, seps)
}

// dropLeading takes the first n digits off a grouping, or returns nil when
// they do not make up the front of it (a prefix split across groups).
func dropLeading(groups []int, n int) []int {
	switch {
	case len(groups) == 0:
		return nil
	case groups[0] == n:
		return groups[1:]
	case groups[0] > n:
		return append([]int{groups[0] - n}, groups[1:]...)
	}
	return nil
}

// mobileGrouping accepts the ways a number is grouped when it is written to
// be dialled: all together, in groups of two or more with at most two pairs
// among them (98765 43210, 987-654-3210, 98 7654 3210, 98765 432 10, 987 654
// 32 10), in pairs or longer set off by dashes or by points alone
// (98-76-54-32-10, 98.76.54.32.10), every digit spaced out the same way
// (9 8 7 6 5 4 3 2 1 0, 9.8.7.6.5.4.3.2.1.0), or one digit set apart among
// groups of three or more (98450 1 2345, 07911 1 23456). It turns down runs of
// three or more pairs set off by spaces or line breaks, which is how a list
// of marks or scores reads (87 91 76 88 92), a single digit beside a pair or
// another single digit, which is a list of small values, and single digits
// set off by points and spaces in turn, which is a list of one-decimal values
// (SGPAs 8.1 8.4 8.7 9.0 9.2, exercises 6.1 6.2 6.3 7.1 7.2).
func mobileGrouping(g []int, seps []string) bool {
	if len(g) == 0 {
		return false
	}
	ones := true
	for _, n := range g {
		ones = ones && n == 1
	}
	if ones {
		return len(g) >= 9 && oneSeparatorKind(seps)
	}
	if len(g) > 5 && !(len(g) == 6 && dashed(seps)) {
		return false
	}
	pairs, singles := 0, 0
	for _, n := range g {
		switch n {
		case 1:
			singles++
		case 2:
			pairs++
		}
	}
	switch {
	case singles > 0:
		// Pairs set off by points or dashes may leave one digit over at
		// an end: "07.88.42.61.39.0".
		return singles == 1 && (pairs == 0 && len(g) <= 4 ||
			dashed(seps) && pairs == len(g)-1 && (g[0] == 1 || g[len(g)-1] == 1))
	case dashed(seps):
		return true
	}
	return pairs <= 2
}

// oneSeparatorKind reports that every separator in a run is of one kind:
// whitespace alone, or the same one of "-", "." and "_" with any whitespace
// round it.
func oneSeparatorKind(seps []string) bool {
	kind := func(s string) string {
		if t := strings.Trim(s, " \n"); t != "" {
			return t[:1]
		}
		return " "
	}
	for _, s := range seps {
		if kind(s) != kind(seps[0]) {
			return false
		}
	}
	return true
}

// dashed reports that every separator in a run is a dash, spaces allowed
// round it, or that the first is a point on its own and every one after it a
// point or a dash: "98-76-54-32-10", "98.76.54.32.10", "98.76-54.32.10". A
// mixed run is read as dashed so that switching separators halfway through
// does not make a number a list of pairs; two decimals with a dash between
// are decimalRange's, and turned down before this is asked.
func dashed(seps []string) bool {
	if len(seps) == 0 {
		return false
	}
	for _, s := range seps {
		if strings.Trim(s, " ") != "-" && !(seps[0] == "." && s == ".") {
			return false
		}
	}
	return true
}

// nanpPhone is a North American number written the North American way: (495)
// 867-5309, 495-867-5309, 495.867.5309, 1 495 867 5309. Ten bare digits are not
// taken as one, and nor are three groups set off by spaces or commas alone
// unless a word before them says it is a number to call ("call 495 867
// 5309"): "batch sizes 256 512 1024" is a list.
func nanpPhone(v *view, run digitRun) bool {
	d, g := string(run.digits), run.groups
	switch {
	case len(d) == 10 && len(g) == 3 && g[0] == 3 && g[1] == 3 && g[2] == 4:
		return nanp(d) && (nanpSeparators(run) || phoneCue.matchIn(v, windowBefore(v.text, run.from, 32)))
	case len(d) == 11 && d[0] == '1' && len(g) == 4 && g[0] == 1 && g[1] == 3 && g[2] == 3 && g[3] == 4:
		return nanp(d[1:])
	case len(d) == 10 && len(g) == 1:
		// Ten bare digits, straight after a word naming the line or asking
		// for a call: "Google Voice: 2954820174", "text me 2954820174".
		near := windowBefore(v.text, run.from, 32)
		return nanp(d) && (phoneLabel.matchIn(v, near) || contactCue.matchIn(v, near))
	}
	return false
}

// nanpSeparators reports a North American number's groups set off the way
// one is written to be dialled: by dashes or points, after a bracketed area
// code by a space or nothing.
func nanpSeparators(run digitRun) bool {
	for i, s := range run.seps {
		switch strings.Trim(s, " ") {
		case "-", ".":
		case "":
			if i > 0 || !run.paren {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// nanpFiction reports a number grouped the North American way, 3-3-4, in
// 555-0100 to 555-0199, which are set aside for fiction: "917-555-0168"
// would otherwise read as an Indian mobile.
func nanpFiction(d string, g []int) bool {
	if len(d) == 11 && d[0] == '1' && len(g) == 4 && g[0] == 1 {
		d, g = d[1:], g[1:]
	}
	return len(d) == 10 && len(g) == 3 && g[0] == 3 && g[1] == 3 && g[2] == 4 && d[3:6] == "555" && d[6:8] == "01"
}

// nanp checks a ten-digit North American number: area code and exchange
// start 2-9, and 555-0100 to 555-0199, which are reserved for fiction, pass.
func nanp(d string) bool {
	return len(d) == 10 && d[0] >= '2' && d[3] >= '2' && !(d[3:6] == "555" && d[6:8] == "01")
}

// repdigit reports a number that is one digit over and over, alone or after
// a country code or a trunk zero: "9999999999" is a placeholder, a test value
// or a joke, and no one's number.
func repdigit(d string) bool {
	if len(d) > 10 {
		d = d[len(d)-10:]
	}
	return len(d) > 0 && strings.Count(d, d[:1]) == len(d)
}

// dramaNumber reports the numbers set aside for drama, which no one can be
// called on: Ofcom's UK ranges, 07700 900000-900999, 020 7946 0000-0999 and
// 496 0000-0999 in the area codes in ofcomDramaAreas, with or without +44;
// and the Australian numbers ACMA
// sets aside for the same use, the mobiles in acmaDramaMobiles (0491 570 006
// and the rest) and the landlines (0X) 5550 xxxx and (0X) 7010 xxxx in each
// area code, with or without +61. The NANP's 555-0100 to 555-0199 are nanp's.
func dramaNumber(d string) bool {
	if strings.HasPrefix(d, "00") {
		d = d[2:]
	}
	if strings.HasPrefix(d, "61") && len(d) == 11 {
		d = "0" + d[2:]
	}
	if acmaDramaMobiles[d] || len(d) == 10 && d[0] == '0' && strings.IndexByte("2378", d[1]) >= 0 &&
		(d[2:6] == "5550" || d[2:6] == "7010") {
		return true
	}
	if strings.HasPrefix(d, "44") {
		d = "0" + strings.TrimPrefix(d[2:], "0")
	}
	return strings.HasPrefix(d, "07700900") || strings.HasPrefix(d, "02079460") ||
		len(d) == 11 && ofcomDramaAreas[d[:4]] && d[4:8] == "4960"
}

// ofcomDramaAreas are the UK area codes in which Ofcom sets 496 0000-0999
// aside for drama.
var ofcomDramaAreas = setOf(`0113 0114 0115 0116 0117 0118 0121 0131 0141 0151 0161 0191`)

// acmaDramaMobiles are the mobile numbers the Australian Communications and
// Media Authority sets aside for films, TV and other creative works.
var acmaDramaMobiles = setOf(`0491570006 0491570156 0491570157 0491570158 0491570159 0491570110
	0491570313 0491570737 0491571266 0491571491 0491571804 0491572549 0491572665 0491572983
	0491573770 0491573087 0491574118 0491574632 0491575254 0491575789 0491576398 0491576801
	0491577426 0491577644 0491578957 0491578148 0491578888 0491579212 0491579760 0491579455`)

// ukPhone is a UK mobile (07...) or, written with spaces, a landline (01, 02,
// 03), eleven digits in all.
func ukPhone(d string, g []int, seps []string) bool {
	if len(d) != 11 || d[0] != '0' {
		return false
	}
	switch d[1] {
	case '7':
		return mobileGrouping(g, seps)
	case '1', '2', '3':
		return len(g) > 1 && mobileGrouping(g, seps)
	}
	return false
}

// sgMobile is a Singapore mobile, eight digits starting 8 or 9, in one
// block, in fours or spaced out a digit at a time, after a word asking for it
// to be called or naming it the poster's: "whatsapp 9137 2684", "my hp is
// 9628-4017". Eight digits alone are as often a date or a roll number.
func sgMobile(v *view, run digitRun) bool {
	d, g := string(run.digits), run.groups
	if len(d) != 8 || d[0] != '8' && d[0] != '9' || repdigit(d) {
		return false
	}
	ones := len(g) == 8 && oneSeparatorKind(run.seps)
	if !(len(g) == 1 || len(g) == 2 && g[0] == 4 || ones) {
		return false
	}
	near := windowBefore(v.text, run.from, 32)
	return contactCue.matchIn(v, near) || callCue.matchIn(v, near)
}

// tenDigitMobile is a mobile written the national way in South Africa or
// Ireland, ten digits starting 06, 07 or 08 (not 070, 080, a free line, or
// 088), set off 3-3-4 or 3-7: "082 541 7703", "087 214 6639". An Indian or a
// UK number with its trunk zero has eleven digits, so the two do not meet.
func tenDigitMobile(d string, g []int, seps []string) bool {
	if !tenDigitPrefix(d) {
		return false
	}
	switch {
	case len(g) == 3 && g[0] == 3 && g[1] == 3 && g[2] == 4:
		return oneSeparatorKind(seps)
	case len(g) == 2 && g[0] == 3 && g[1] == 7:
		return true
	}
	return false
}

// tenDigitPrefix reports ten digits a South African or Irish mobile can
// start with: 06, 07 or 08, but not 070, 080 or 088.
func tenDigitPrefix(d string) bool {
	return len(d) == 10 && d[0] == '0' && d[1] >= '6' && d[1] <= '8' && d[1:3] != "80" && d[1:3] != "88" &&
		d[1:3] != "70"
}

// tenDigitCalled is such a mobile run together, straight after a word
// naming the line or asking for a call, as a North American number is
// read: "my number is 0821234567", "cell: 0871234567".
func tenDigitCalled(v *view, run digitRun) bool {
	if len(run.groups) != 1 || !tenDigitPrefix(string(run.digits)) {
		return false
	}
	near := windowBefore(v.text, run.from, 32)
	return phoneLabel.matchIn(v, near) || contactCue.matchIn(v, near)
}

// auMobile is an Australian mobile written the national way, 04 and eight
// more digits: 0412 345 678.
func auMobile(d string, g []int, seps []string) bool {
	return len(d) == 10 && strings.HasPrefix(d, "04") && mobileGrouping(g, seps)
}

// auLandline is an Australian landline written the national way, its area
// code (02, 03, 07 or 08) set apart and the eight digits after it in fours:
// (07) 3221 3317, 02 9876 5432. Local numbers start 3 to 9, which leaves out
// a date ("02 2024 1130").
func auLandline(d string, g []int) bool {
	return len(d) == 10 && d[0] == '0' && strings.IndexByte("2378", d[1]) >= 0 && d[2] >= '3' &&
		len(g) == 3 && g[0] == 2 && g[1] == 4 && g[2] == 4
}

// freephone reports a UK freephone number written the UK way, 0800 or 0808 as
// a group of its own: 0800 068 4141. A freephone line is a service's, which
// no one has as their own phone; written as one block the same digits read
// as an Indian mobile with its trunk zero, and are left to that.
func freephone(d string, g []int) bool {
	return len(d) == 11 && (strings.HasPrefix(d, "0800") || strings.HasPrefix(d, "0808")) && len(g) > 1 && g[0] == 4
}

// aadhaar is a twelve-digit Aadhaar number, as one block or as 4-4-4, whose
// Verhoeff check digit is right.
func aadhaar(d string, g []int, seps []string) bool {
	return aadhaarShape(d, g, seps) && verhoeffValid(d)
}

// aadhaarShape is twelve digits, the first 2-9, as one block, as 4-4-4 or a
// digit at a time. A run of years (2019 2020 2021) is not one.
func aadhaarShape(d string, g []int, seps []string) bool {
	if len(d) != 12 || d[0] < '2' {
		return false
	}
	switch {
	case len(g) == 1:
	case len(g) == 12:
		// A digit at a time, every gap the same: "8 3 4 1 2 9 0 6 7 5 2 9".
		if !oneSeparatorKind(seps) {
			return false
		}
	case len(g) == 3 && g[0] == 4 && g[1] == 4 && g[2] == 4:
		for _, s := range seps {
			if s != " " && s != "-" {
				return false
			}
		}
		years := true
		for i := 0; i < 12; i += 4 {
			years = years && (d[i:i+2] == "19" || d[i:i+2] == "20")
		}
		if years {
			return false
		}
	default:
		return false
	}
	return true
}

// ssn is a US Social Security number written with dashes, in the ranges the
// SSA issues from. The two numbers printed in old advertising pass.
func ssn(d string, g []int, seps []string) bool {
	return ssnShape(d, g, seps, "-")
}

// ssnShape is 3-2-4 digits in the ranges the SSA issues from, set off by sep
// both times.
func ssnShape(d string, g []int, seps []string, sep string) bool {
	if len(d) != 9 || len(g) != 3 || g[0] != 3 || g[1] != 2 || g[2] != 4 || seps[0] != sep || seps[1] != sep {
		return false
	}
	area, group, serial := d[:3], d[3:5], d[5:]
	if area == "000" || area == "666" || area[0] == '9' || group == "00" || serial == "0000" {
		return false
	}
	return d != "078051120" && d != "219099999"
}

// The Verhoeff scheme's tables: the dihedral group D5's multiplication, and
// the permutation applied at each position.
var (
	verhoeffD = [10][10]int{
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		{1, 2, 3, 4, 0, 6, 7, 8, 9, 5},
		{2, 3, 4, 0, 1, 7, 8, 9, 5, 6},
		{3, 4, 0, 1, 2, 8, 9, 5, 6, 7},
		{4, 0, 1, 2, 3, 9, 5, 6, 7, 8},
		{5, 9, 8, 7, 6, 0, 4, 3, 2, 1},
		{6, 5, 9, 8, 7, 1, 0, 4, 3, 2},
		{7, 6, 5, 9, 8, 2, 1, 0, 4, 3},
		{8, 7, 6, 5, 9, 3, 2, 1, 0, 4},
		{9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
	}
	verhoeffP = [8][10]int{
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		{1, 5, 7, 6, 2, 8, 3, 0, 9, 4},
		{5, 8, 0, 3, 7, 9, 6, 1, 4, 2},
		{8, 9, 1, 6, 0, 4, 3, 5, 2, 7},
		{9, 4, 5, 3, 1, 2, 6, 8, 7, 0},
		{4, 2, 8, 6, 5, 7, 3, 9, 0, 1},
		{2, 7, 9, 3, 8, 0, 6, 4, 1, 5},
		{7, 0, 4, 6, 9, 1, 3, 2, 5, 8},
	}
)

// verhoeffValid reports whether a string of digits ends in its Verhoeff check
// digit, as every Aadhaar number does.
func verhoeffValid(digits string) bool {
	c := 0
	for i := 0; i < len(digits); i++ {
		c = verhoeffD[c][verhoeffP[i%8][digits[len(digits)-1-i]-'0']]
	}
	return c == 0
}

// Student-ID formats (build plan 2.9, "configurable student-ID formats") are
// not built. They belong beside aadhaar and ssn above: a validator over a
// digitRun, or a plainOnly rule like niRule for formats with letters, added to
// classifyNumber or firstBlock with kindAadhaar's sibling kind, and loaded
// from configuration rather than written in here.
