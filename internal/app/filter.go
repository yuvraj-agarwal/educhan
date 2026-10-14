package app

import (
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
//
// Be honest about what this is. These are word and number patterns. They are
// easy to get round — a number spelled out in words, a username with no app
// named beside it, contact details written inside [math], which is skipped
// because TeX has a use for every character — and they catch posts that are
// fine, which is why a block says exactly what it matched and a hold waits
// for a person instead of deleting anything. They raise the cost of moving a
// child off the site; they do not prevent it.
//
// The cost is bounded and linear in the length of the post. Each field is
// normalised once into two views, every pattern is a Go regexp (linear time,
// no backtracking), the number scanner is one pass, and the checks that pair
// matches up (a threat with a time, an age question with a contact cue) work
// on the sorted match lists rather than rescanning.

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
	// poster wrote it.
	Field string
	Match string
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
	fields := make([]*scannedField, 0, 3)
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
	for i := 1; i < len(fields); i++ {
		fields[i].offset = fields[i-1].offset + len(fields[i-1].src) + 1
	}

	for _, f := range fields {
		if found, ok := f.firstBlock(); ok {
			return contentVerdict{
				Outcome: contentBlock,
				Rule:    found.kind.rule(),
				Field:   f.field,
				Match:   quoteSource(f.src, found.from, found.to),
				Message: blockMessage(found.kind, f.field),
			}
		}
	}
	if !supportSeen {
		for _, f := range fields {
			if f.anyMatch(supportRules, f.supportCheck) || f.asksForAMethod() {
				return contentVerdict{Outcome: contentSupport, Message: supportMessage}
			}
		}
	}
	if reason := holdReason(fields); reason != "" {
		return contentVerdict{Outcome: contentHold, Hold: reason}
	}
	return contentVerdict{Outcome: contentPublish}
}

// holdReason picks the first hold that applies, most urgent first: a threat
// is what a moderator most needs to see quickly.
func holdReason(fields []*scannedField) string {
	if threatens(fields) {
		return holdThreat
	}
	for _, f := range fields {
		if f.anyMatch(selfHarmRules, selfHarmCheck) || f.asksForAMethod() {
			return holdSelfHarm
		}
	}
	// A grooming cue is the pair: an age, school or location question and a
	// private-contact or meet-up cue, anywhere in the same post.
	asks, cues := false, false
	for _, f := range fields {
		asks = asks || f.anyMatch(ageQuestionRules, ageCheck)
		cues = cues || f.anyMatch(privateCueRules, nil)
	}
	if asks && cues {
		return holdGrooming
	}
	return ""
}

// quoteSource is the text a block quotes back: the poster's own, from the
// source string, trimmed, and cut to maxMatchLen characters. A cut quote ends
// in "…", so the page knows to select from where it starts to the end of the
// run it finds, not just the characters quoted.
func quoteSource(src string, from, to int) string {
	q := strings.TrimSpace(src[from:to])
	if utf8.RuneCountInString(q) <= maxMatchLen {
		return q
	}
	return strings.TrimRight(truncateRunes(q, maxMatchLen-1), " \t\r\n") + "…"
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
// number, and "sn[spoiler]ap[/spoiler]" is "snap". The exception is a tag
// between a letter and a digit, which reads as a blank, so that a number hidden
// in a spoiler straight after a word ("call me[spoiler]98765…") still stands on
// its own.
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
	// A spoiler tag's reading depends on what is either side of it once the
	// tags next to it have been read too, so it is settled in a second pass.
	for i := range out {
		if out[i].as != "" {
			continue
		}
		before, after := runeBefore(body, out, i), runeAfter(body, out, i)
		if unicode.IsLetter(before) && unicode.IsDigit(after) || unicode.IsDigit(before) && unicode.IsLetter(after) {
			out[i].as = " "
		}
	}
	return out
}

// runeBefore is the source character just before reading i, looking through
// the zero-width readings before it; 0 at the start.
func runeBefore(body string, rs []reading, i int) rune {
	at := rs[i].from
	for j := i - 1; j >= 0 && rs[j].to == at && rs[j].as == ""; j-- {
		at = rs[j].from
	}
	r, _ := utf8.DecodeLastRuneInString(body[:at])
	return r
}

// runeAfter is the source character just after reading i, looking through
// the zero-width readings after it; 0 at the end.
func runeAfter(body string, rs []reading, i int) rune {
	at := rs[i].to
	for j := i + 1; j < len(rs) && rs[j].from == at && rs[j].as == ""; j++ {
		at = rs[j].to
	}
	r, _ := utf8.DecodeRuneInString(body[at:])
	return r
}

/* Normalising ---------------------------------------------------------------- */

// view is one normalised rendering of a field, with every byte mapped back to
// the source character it came from, so that a match is reported as the
// poster wrote it.
type view struct {
	text  string
	start []int // start[i]: where the source character behind text[i] starts
	end   []int // end[i]: where it ends
}

// span maps text[a:b] back to the source.
func (v *view) span(a, b int) (int, int) { return v.start[a], v.end[b-1] }

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
	// text: the lengths of the fields before it, plus one for each.
	offset int
	// method caches asksForAMethod, which both the support step and the
	// holds ask: 0 not yet known, 1 no, 2 yes.
	method int8
}

// asksForAMethod reports a selfHarmMethodRules match.
func (f *scannedField) asksForAMethod() bool {
	if f.method == 0 {
		f.method = 1
		if f.anyMatch(selfHarmMethodRules, methodCheck) {
			f.method = 2
		}
	}
	return f.method == 2
}

func scanField(field, src string, readings []reading) *scannedField {
	f := &scannedField{field: field, src: src}
	f.plain = plainView(src, readings)
	f.loose = looseView(&f.plain)
	f.looseOnly = f.loose.text == canon(f.plain.text)
	return f
}

// plainView builds the plain view of src. readings, sorted and apart, are the
// stretches that read as something other than their characters: markup tags,
// and maths, whose blank keeps the words either side of it apart.
func plainView(src string, readings []reading) view {
	b := newViewBuilder(len(src))
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
		foldRune(r, func(out rune) { b.add(out, from, to) })
		i = to
	}
	return b.view()
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

func looseView(p *view) view {
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
	for i, sr := range runes {
		r := sr.r
		// A letter written three or more times running is read twice:
		// "shooot", "schoool", "killll". Twice, not once, because the words
		// looked for have double letters of their own. The rune compared is
		// the one written, before l is read as i, so "kill" stays "kiii".
		if unicode.IsLetter(r) && i >= 2 && runes[i-1].r == r && runes[i-2].r == r {
			continue
		}
		if letter, ok := leetLetters[r]; ok && (letterAt(i-1, -1) || letterAt(i+1, 1)) {
			r = letter
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
	return b.view()
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
	plain, loose *regexp.Regexp
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
	return rule{plain: regexp.MustCompile(src), loose: regexp.MustCompile(canon(src))}
}

func plainOnly(src string) rule {
	return rule{plain: regexp.MustCompile(phrase(src))}
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
		`fortnite`, `valorant`, `minecraft`, `omegle`)
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
		`share`, `post`, `tell`, `dm`)
	asksTo = words(`on`, `at`, `in`, `via`, `thru`, `through`, `over`, `@`)
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
	// "hmu on snap"
	`hmu~`+words(`on`, `at`, `in`, `via`, `over`, `@`)+`~(?:my~)?`+anyApps,
	// "add my snap", "follow my insta"
	words(`add`, `follow`, `dm`, `message`, `msg`, `text`, `check~out`, `sub~to`, `subscribe~to`)+`~my~`+contactApps,
	// "what's your snap", "drop your insta"
	whatsYour+`~(?:me~)?`+yours+`~`+contactApps,
	// "what's your number", "give me your number"
	words(`what'?s`, `whats`, `wats`, `wat'?s`, `wht'?s`, `what~is`, `wuts?`, `send`, `give`, `drop`, `dm`, `text`)+
		`~(?:me~)?`+yours+`~(?:(?:phone|mobile|cell|whatsapp|contact|personal)~)?`+
		words(`number`, `no`, `num`, `digits`),
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
	words(`let'?s`, `lets`, `we~can`, `can~we`, `wanna`, `want~to`)+`~`+
		words(`move`, `go`, `continue`)+`~`+words(`to`, `over~to`, `on~to`, `onto`)+`~`+placeApps,
	`continue~(?:this~)?`+words(`on`, `over~on`, `in`)+`~`+placeApps,
	// "are you on snap?"
	words(`are`, `r`)+`~`+words(`you`, `u`, `ya`)+`~on~`+placeApps,
	// Hinglish: "insta pe add karo", "whatsapp pe baat karte hain"
	placeApps+`~`+words(`pe`, `par`, `pr`)+`~(?:mujhe~)?`+words(`add`, `follow`, `message`, `msg`, `dm`, `text`, `ping`)+
		`~`+karo,
	placeApps+`~`+words(`pe`, `par`, `pr`)+`~baat~`+words(karo, `karte`, `krte`, `karenge`, `krenge`, `karein`, `karen`),
	// "apna number do", "tera number de"
	words(`apna`, `apni`, `tera`, `teri`, `tumhara`, `tumhari`, `aapka`, `aapki`, `apka`, `apki`)+`~`+
		words(`number`, `no`, `num`, `nmbr`, `id`, contactApps)+`~`+
		words(`do`, `de`, `dedo`, `de~do`, `dena`, `bhejo`, `bhej`, `bhej~do`, `batao`, `bata`, `bta`, `share~`+karo),
) + `\b(?:[ \t]*@[ \t]*[\pL\pN._]+)?`),
	// "whatsapp me", "kik me": only a space between, so "WhatsApp, me and my
	// friends" is a list.
	bothViews(`\b` + words(whatsApp, `kik`, `telegram`, `snapchat`) + `[ \t]+me\b`),
	// "ur snap?"
	bothViews(`\b` + words(`your`, `ur`) + `~` + contactApps + `[ \t]*\?`),
}

// atAskRule asks for a handle by its "@" alone: "drop ur @", "what's your @".
// Its check reads what follows, so that the "@" is not the start of one.
var atAskRule = bothViews(`\b` + whatsYour + `~(?:me~)?` + yours + `[ \t]*@`)

// topicWords, straight after an app's name, make it the app's score, game or
// look that is being talked about, not a place to talk.
var topicWords = canonSetOf(`score scores karma avatar streak streaks level levels rank elo rating bio pfp
	aesthetic filter filters theme wallpaper game games build builds map maps obby clip clips song songs edit
	edits skin skins outfit emoji emojis stats playlist screen bot bots story stories feed algorithm`)

func askCheck(v *view, m []int) bool {
	return !topicWords[firstWordAfter(v.text, m[1])]
}

// myAppRule is "my snap is ...", whose last word decides it (myAppCheck): a
// username, or a word about the app ("my discord is lagging").
// The gap after "my" may be empty, for "mysnap" closed up from "m y s n a p",
// but not an underscore: "my_snap = snap(x)" is code.
var myAppRule = bothViews(`\b` + words(`my`, `mera`, `meri`, `mere`, `apna`, `apni`) + `[^\pL\pN_]*` + contactApps +
	`\b(?:~` + words(`id`, `username`, `user~name`, `handle`, `name`, `acc`, `account`, `tag`, `@`) +
	`)?\s*(?:` + words(`is`, `hai`, `h`) + `\b|[=:\-])\s*(@?[\pL\pN._]+)`)

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
// part before the "@": "my gmail is coolkid12", "gmail: coolkid12". They
// need the same evidence as a handle.
var (
	myMailRule = bothViews(`\b` + words(`my`, `mera`, `meri`) + `[^\pL\pN_]*` + mailApps + `\b\s*(?:` +
		words(`is`, `hai`) + `\b|[=:])\s*(@?)([\pL\pN._]{2,30})()`)
	mailColonHandle = bothViews(`\b` + mailApps + `\s*:\s*(@?)([\pL\pN._]{2,30})()`)
)

// handleRules name an account on an app directly: "insta: x_y_z", "snap
// @ my_name", "insta id x_y_z", "discord: name#1234", "snap -> x_y_z",
// "@x_y_z on snap". The "@", the underscores and the digits are the
// evidence, which the caller reads from the source. Each has the same
// groups: an "@" before the handle, the handle, and a Discord discriminator
// after it.
var (
	appColonHandle = bothViews(`\b` + handleApps + `\b(?:~` +
		words(`id`, `username`, `user~name`, `handle`, `acc`, `account`, `tag`) +
		`)?\s*(?:-+>|=>|→|[:=]|-\s)\s*(@?)([\pL\pN._]{2,30})(#\d{4})?`)
	appAtHandle = bothViews(`\b` + handleApps + `\s*(?:is\s*)?(@)\s*([\pL\pN._]{2,30})(#\d{4})?`)
	// "insta id x_y_z", "snap me x_y_z", "snap x_y_z", "insta id hai x_y_z":
	// a label, "me" or a copula, or nothing, and no colon.
	appLabelHandle = bothViews(`\b` + handleApps + `(?:~` +
		words(`id`, `username`, `user~name`, `handle`, `me`) + `)?(?:[ \t]+` + words(`is`, `hai`, `h`) +
		`)?\s+(@?)([\pL\pN._]{2,30})(#\d{4})?`)
	// "mera insta coolkid_12 hai": the handle before the copula.
	hindiHandle = bothViews(`\b` + words(`mera`, `meri`, `mere`, `apna`, `apni`) + `~` + handleApps +
		`(?:~id)?[ \t]+(@?)([\pL\pN._]{2,30})(#\d{4})?[ \t]+` + words(`hai`, `h`) + `\b`)
	// "@x_y_z on snap", "x_y_z is my snap", "x_y_z (snap)": the handle first.
	handleBeforeApp = bothViews(`(?:(@)|\b)([\pL\pN][\pL\pN._]{1,29})(#\d{4})?[ \t]*(?:` +
		words(`on`, `is~my`, `on~my`, `in~my`, `-+>`, `=>`, `→`, `\(`) + `)[ \t]*` + handleApps + `\b`)
)

// handleRules are the rules above that end at the handle.
var handleRules = []rule{appColonHandle, appAtHandle, appLabelHandle, hindiHandle, handleBeforeApp}

// notHandles are words written with an "@" that address a group, not a
// person: "@everyone on discord".
var notHandles = setOf(`everyone here channel all`)

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
	spelledEmailRule = plainOnly(`\b([\pL\pN][\pL\pN._%+\-]{0,63})\s*([(\[{<]\s*(?:at|@)\s*[)\]}>]|\s+at\s+|@+|` +
		atTheRate + `)\s*([\pL\pN][\pL\pN\-]{0,62}(?:` + spelledDot + `[\pL\pN][\pL\pN\-]{0,62})*)(` + spelledDot +
		`)([a-z]{2,24})\b`)
)

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

// emailTLDs are the top-level domains an address is recognised under. An
// address anywhere else gets through, a deliberate gap: code is scanned, and
// "x@self.weight" or "user@host.sh" in a code block is not an email address.
var emailTLDs = setOf(`com net org edu gov mil int info biz co io ai me app dev xyz online site
	tech live email mail cloud in uk us ca au nz de fr es it nl be ch at se no dk fi ie pt ru ua cn jp
	kr sg my id ph th vn pk bd lk np ae sa qa za ng ke br mx ar cl tv cc ac ly gg fm`)

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
// gg/abc").
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
		long("join.skype.com"), long("linktr.ee"))
	// Compiled as written: a path may contain "~", which phrase would read
	// as a separator.
	return rule{plain: regexp.MustCompile(`\b(?:https?\W{0,3})?(?:www\s?\.\s?)?(` + hosts + `)\s?/\s?` +
		`([\pL\pN_@~%+.\-]+(?:/[\pL\pN_@~%+.\-]*)*)`)}
}()

// spacedInviteRule is a Discord invite with its host spelled out letter by
// letter ("d i s c o r d . g g / abc"), which only the loose view closes up.
// Every Discord invite is a way to talk somewhere else, so it needs no
// contactLink check.
var spacedInviteRule = bothViews(`\bdiscord~(?:dot~)?gg[^\pL\pN/]*/[^\pL\pN]*[\pL\pN]{2,}`)

var (
	instagramPages = setOf(`p reel reels tv explore accounts about developer legal web`)
	facebookPages  = setOf(`groups watch share events photo photos story.php permalink.php reel marketplace
		help policies privacy login gaming pages hashtag search`)
	twitterPages = setOf(`home explore search i intent hashtag settings notifications login signup tos privacy`)
)

// contactLink decides whether a link opens a profile or an invite. A link to
// a post, a video or a page of the app's own is content, and passes.
func contactLink(host, path string) bool {
	host = strings.NewReplacer(" ", "", "(", "", ")", "", "[", "", "]", "", "dot", ".").Replace(host)
	host = strings.TrimPrefix(host, "www.")
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
	}
	return false
}

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
var (
	selfHurt = words(`hurt~myself`, `cut~myself`, `harm~myself`, `self~harm`, `starve~myself`, `burn~myself`)
	jumpOff  = `jump~off~(?:a|the)~` + words(`bridge`, `building`, `roof`, `cliff`)
)

// supportRules are crisis language in the poster's own voice, in English and
// in some Hinglish. Second-person versions ("kill yourself") are not here:
// those are encouragement, a hold. supportCheck turns down the matches that
// are something else in context: a distance in "kms", a hostel in "I don't
// want to live in the hostel", an idiom in "die of embarrassment".
var supportRules = []rule{bothViews(`\b` + words(
	`i~(?:(?:really|just|honestly|kinda|actually|literally|seriously|so)~)*`+
		words(`want`, `wanna`, `wana`, `wish`, `need`)+
		`~(?:(?:to|2)~)?`+words(`die`, `be~dead`, `not~be~alive`, `not~exist`, `not~wake~up`, `not~be~here`,
		`disappear`, `end~it~all`, `end~my~life`, `end~it`, `end~myself`, `end~everything`,
		`kill~my~self`, `stop~existing`, `commit~suicide`, `take~my~(?:own~)?life`, selfHurt, jumpOff),
	// Wherever the "i" is: "I hate myself and want to self harm".
	words(`want`, `wanna`, `wana`, `need`)+`~(?:(?:to|2)~)?`+selfHurt,
	`i~(?:'?m|am|m)?~`+words(`going~to`, `gonna`, `gunna`, `about~to`, `ready~to`, `thinking~about`,
		`thinking~of`, `planning~to`, `planning~on`)+`~`+words(`kill(?:ing)?~my~self`, `end(?:ing)?~it~all`,
		`end(?:ing)?~my~life`, `end(?:ing)?~myself`, `end(?:ing)?~it`, `commit(?:ting)?~suicide`,
		`tak(?:e|ing)~my~(?:own~)?life`, `hurt(?:ing)?~myself`, `cut(?:ting)?~myself`, `overdos(?:e|ing)`,
		`jump(?:ing)?~off~(?:a|the)~`+words(`bridge`, `building`, `roof`, `cliff`)),
	`i~`+words(`'?ll`, `will`, `wil`, `shall`, `might`, `may`, `could`)+`~(?:just~)?`+
		words(`kill~my~self`, `end~it~all`, `end~my~life`, `commit~suicide`, `take~my~(?:own~)?life`),
	words(`kill(?:ing)?`, `unaliv(?:e|ing)`)+`~my~self`,
	`kms`,
	`i~`+words(`'?m`, `am`, `m`, `feel`, `feeling`, `felt`, `been`, `'?ve~been`, `have~been`, `get`, `got`)+
		`~(?:(?:so|really|very|kinda|pretty|a~bit|super)~)?suicidal`,
	`suicidal~`+words(`thoughts`, `feelings`, `again`, `ideation`),
	`(?:i~)?`+words(`don'?t`, `do~not`, `dont`, `dnt`)+`~(?:really~)?`+words(`want~to`, `wanna`, `wana`)+`~`+
		words(`be~alive`, `live`, `exist`, `be~here~anymore`, `wake~up`),
	`i~(?:'?m~|am~|m~|'?ve~|have~)?(?:been~)?`+words(`cutting`, `cut`, `burning`, `burned`, `burnt`, `hurting`,
		`harming`, `starving`)+`~myself`,
	`i~(?:'?ve~been~|have~been~|keep~|still~)?self~harm(?:ing)?`,
	`feel~like~`+words(words(`hurting`, `cutting`, `harming`, `killing`, `starving`, `burning`)+`~myself`, `dying`),
	words(`thinking`, `think`)+`~`+words(`about`, `of`)+`~`+words(
		words(`hurting`, `cutting`, `harming`, `killing`, `starving`, `burning`)+`~myself`,
		`ending~`+words(`it~all`, `it`, `my~life`, `everything`, `things`, `myself`), `suicide`, `dying`),
	words(`done`, `finished`)+`~with~`+words(`life`, `living`),
	words(`no`, `nothing`)+`~`+words(`reason`, `point`)+`~`+words(`to`, `in`)+`~`+
		words(`live`, `living`, `go~on`, `going~on`),
	`nothing~(?:left~)?to~live~for`,
	`better~off~dead`,
	`i~wish~i~`+words(`was`, `were`)+`~dead`,
	`i~wish~i~`+words(`wasn'?t`, `weren'?t`, `was~never`, `were~never`, `had~never~been`, `hadn'?t~been`)+
		`~`+words(`born`, `alive`),
	`everyone~`+words(`would`, `will`)+`~be~better~off~without~me`,
	words(`nobody`, `no~one`, `noone`)+`~would~`+words(`miss`, `care~if`)+`~me`,
	// Hinglish: "mujhe marna hai", "suicide karna chahta hoon".
	`mujhe~`+words(`marna`, `mar~jana`, `mar~jaana`)+`~(?:hai|h)`,
	words(`mar~jana`, `mar~jaana`, `marna`)+`~(?:hai|h)~mujhe`,
	words(`main`, `mai`, `me`)+`~`+words(`marna`, `mar~jana`)+`~`+words(`chahta`, `chahti`),
	`suicide~`+words(`karna`, `krna`, `kar~lunga`, `kar~lungi`, `karunga`, `karungi`)+
		`(?:~`+words(`chahta`, `chahti`, `hai`, `h`)+`)?`,
	words(`jeena`, `jina`)+`~`+words(`nahi`, `nhi`)+`~`+words(`chahta`, `chahti`, `hai`),
	`marne~ka~(?:mann|man)~`+words(`hai`, `h`, `karta`, `karti`, `kar~raha`, `kar~rha`),
) + `\b`)}

// selfHarmRules are encouragement to hurt or kill oneself, and pro-eating-
// disorder content, in English and some Hinglish. They are held, through
// selfHarmCheck.
var selfHarmRules = []rule{bothViews(`\b` + words(
	`k+y+s+`,
	`kill~`+words(`your`, `ur`, `yo`)+`~sel(?:f|ves)`, `kill~urself`, `kill~yourselves`,
	`unalive~`+words(`your`, `ur`)+`~self`,
	words(`go`, `pls`, `please`, `plz`, `why~don'?t~you`, `why~dont~you`, `you~should`, `u~should`,
		`you~shud`, `u~shud`, `you~need~to`, `u~need~to`, `you~deserve~to`, `u~deserve~to`, `you~can~go`,
		`u~can~go`, `you~ought~to`, `you~must`)+`~(?:just~)?`+
		words(`die`, `kill~(?:your|ur)~self`, `hang~(?:your|ur)~self`, `cut~(?:your|ur)~self`,
			`end~your~life`, `starve~(?:your|ur)~self`, jumpOff, `cut~deeper`, `drink~bleach`),
	words(`cut`, `hang`, `starve`, `neck`, `rope`, `end`)+`~`+words(`your`, `ur`)+`~sel(?:f|ves)`,
	`end~`+words(`your`, `ur`)+`~life`,
	`slit~`+words(`your`, `ur`)+`~wrists?`,
	// Imperatives with no "you" before them, which selfHarmCheck takes only
	// at the start of what is said: "just die", not "i just die every time".
	jumpOff, `drink~bleach`, `just~die`, `cut~deeper`,
	words(`you`, `u`)+`~`+words(`don'?t`, `dont`, `do~not`)+`~deserve~to~`+words(`live`, `be~alive`, `exist`),
	words(`nobody`, `no~one`, `noone`)+`~`+words(`would`, `will`, `'?d`, `is~gonna`, `is~going~to`)+
		`~(?:even~)?`+words(`miss`, `care~if`)+`~`+words(`you`, `u`),
	`(?:the~)?world~`+words(`would`, `will`)+`~be~better~(?:off~)?without~`+words(`you`, `u`),
	// Hinglish: "tu mar ja", "suicide kar le".
	words(`tu`, `tum`, `aap`)+`~(?:`+words(`jaake`, `ja~ke`)+`~)?mar~`+words(`ja`, `jaa`, `jao`, `jaiye`),
	`mar~`+words(`ja`, `jaa`, `jao`),
	`suicide~`+words(`kar~le`, `kar~lo`, `karle`, `karlo`, `kr~le`, `kr~lo`, `krle`, `krlo`),
	words(`jaake`, `ja~ke`)+`~mar`,
	// Pro-eating-disorder content.
	`thinspo`, `thinspiration`, `meanspo`, `bonespo`, `skinnyspo`, `pro~?ana`, `pro~?mia`,
	words(`ana`, `mia`)+`~`+words(`coach`, `buddy`, `tips?`),
) + `\b`)}

// selfHarmMethodRules ask or tell how to hurt or kill oneself: how, how many,
// what dose. Someone asking may be asking for themselves, so a post with one
// is answered with the support message first, as crisis language is, and is
// held once the poster has seen it. methodCheck turns down the questions
// about the world rather than oneself: "how much caffeine is lethal", "the
// lethal dose for half the test animals", "fastest way to die in minecraft".
var selfHarmMethodRules = []rule{bothViews(`\b` + words(
	`how~`+words(`do`, `can`, `could`, `should`, `would`, `does`)+`~`+
		words(`i`, `you`, `u`, `one`, `someone`, `a~person`, `people`)+`~`+methods,
	`how~to~`+words(methods, `cut~without~`+words(`anyone`, `people`, `parents`)+`~`+
		words(`knowing`, `noticing`, `seeing`), `hide~(?:my~)?`+words(`cuts`, `scars`, `self~harm`)),
	`how~`+words(`many`, `much`)+`~[^.?!\n]{0,60}?~`+words(`to~die`, `to~kill~`+words(`my`, `your`)+`~self`,
		`to~overdose`, `to~od`, `overdose`, `be~fatal`, `be~lethal`, `be~deadly`, `is~fatal`, `is~lethal`,
		`is~deadly`, `would~kill~`+words(`me`, `you`, `someone`, `a~person`), `to~end~it`, `to~end~my~life`),
	// "how many sleeping pills are lethal": "are" only with a medicine named,
	// since "how many snakes are deadly" is a biology question.
	`how~many~[^.?!\n]{0,40}?`+medicines+`[^.?!\n]{0,20}?~are~`+
		words(`lethal`, `fatal`, `deadly`, `enough~to~`+words(`die`, `kill~`+words(`me`, `you`, `someone`), `od`, `overdose`)),
	// "will 20 pills kill me"
	words(`will`, `would`, `could`, `can`, `does`, `do`)+`~(?:\d+~)?`+medicines+`~`+
		words(`kill~me`, `kill~you`, `kill~someone`, `be~fatal`, `be~lethal`, `be~enough`),
	`what~`+words(`dose`, `amount`, `quantity`, `dosage`)+`~[^.?!\n]{0,40}?~`+
		words(`lethal`, `fatal`, `deadly`, `kill~`+words(`me`, `you`, `myself`, `yourself`), `to~die`,
			`overdose`, `od`),
	words(`lethal`, `fatal`, `deadly`)+`~`+words(`dose`, `doses`, `dosage`, `amount`, `quantity`),
	words(`painless`, `easiest`, `easy`, `quickest`, `quick`, `best`, `fastest`, `surest`, `least~painful`)+
		`~`+words(`way`, `ways`, `method`, `methods`)+`~(?:to~)?`+
		words(`die`, `kill~`+words(`my`, `your`, `ur`, `one`)+`~self`, `commit~suicide`,
			`end~`+words(`my`, `your`, `one'?s`)+`~life`, `od`, `overdose`),
	`suicide~`+words(`method`, `methods`, `guide`, `instructions`, `tips`),
	// "how deep should i cut"
	`how~deep~`+words(`should`, `do`, `can`, `would`, `to`, `must`)+`~(?:`+words(`i`, `you`, `u`, `one`)+`~)?cut`,
) + `\b`)}

// medicines are what an overdose question names.
var medicines = words(`pills?`, `tablets?`, `capsules?`, `paracetamol`, `tylenol`, `acetaminophen`, `ibuprofen`,
	`aspirin`, `advil`, `panadol`, `crocin`, `dolo`, `meds`, `medicines?`, `medications?`, `antidepressants?`,
	`sleeping~pills`, `insulin`, `xanax`, `benadryl`, `melatonin`)

// methods are the ways of hurting or killing oneself a how-question asks for.
var methods = words(
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

// Threats: an intent to hurt people or attack a place, held only when a place
// or a time comes with it ("at school", "tomorrow", "in 3rd period") — which an
// intent to attack a school always does, since it names one; the warning
// "don't come to school tomorrow" with a cue that it is one ("you've been
// warned"); and "school shooting" with a time still to come. Only the first
// person and the future count: a history question, or news that reports an
// attack ("he threatened to shoot up his school"), names no intent of the
// poster's, so it passes. Some Hinglish is read too ("sabko goli maar dunga").
var (
	threatPeople = words(`everyone`, `everybody`, `every~one`, `people`, `you~all`, `y'?all`, `yall`,
		`all~of~`+words(`you`, `u`, `them`), `you`, `u`, `them~all`, `them`, `him`, `her`,
		// One word may come between: "my maths teacher".
		words(`the`, `my`, `our`, `all~the`, `some`)+`~(?:\pL+[^\pL\pN]+)?`+words(`teachers?`, `students?`,
			`principal`, `class`, `classmates`, `kids`, `bully`, `bullies`, `staff`, `school`),
		`teachers`, `students`, `kids`, `classmates`, `some(?:one|body)`, `the~whole~`+words(`school`, `class`))
	threatPlaces = words(`school`, `class`, `classroom`, `campus`, `college`, `uni`, `university`, `hostel`, `dorm`,
		`canteen`, `cafeteria`, `library`, `assembly`, `auditorium`, `gym`, `exam~hall`, `lab`, `school~bus`, `bus`,
		`mall`, `temple`, `mosque`, `church`, `gurudwara`, `gurdwara`, `synagogue`, `coaching`, `tuition`, `office`,
		`playground`, `stadium`, `station`, `airport`, `hospital`, `concert`, `prom`, `graduation`, `washroom`,
		`bathroom`, `restroom`, `toilets?`, `corridor`, `hallway`, `staffroom`, `staff~room`, `parking~lot`, `metro`,
		`columbine`)
	weekdays    = words(`mon`, `tues`, `wednes`, `thurs`, `fri`, `satur`, `sun`) + `day`
	threatTimes = words(`today`, `tonight`, `tomorrow`, `tmrw`, `tmr`, `tmrrw`, `tomoz`, `2moro`, `2morrow`, `kal`,
		`aaj`, words(`on`, `this`, `next`, `coming`)+`~`+weekdays, weekdays,
		`next~week`, `this~week(?:end)?`, words(`after`, `before`)+`~`+words(`school`, `class`),
		`during~`+words(`lunch`, `recess`, `break`, `assembly`, `class`, `school`, `the~exam`, `exams`, `prayer`),
		`at~`+words(`lunch`, `recess`, `break`, `noon`, `midnight`, `assembly`),
		`(?:(?:in|during)~)?`+words(`\d+(?:st|nd|rd|th)`, `first`, `second`, `third`, `fourth`, `fifth`, `sixth`,
			`seventh`, `eighth`, `last`)+`~period`,
		`at~\d{1,2}(?::\d\d)?~?(?:am|pm)`, `at~\d{1,2}:\d\d`,
		`this~`+words(`morning`, `afternoon`, `evening`), `on~the~\d{1,2}(?:st|nd|rd|th)`)
	// futureTimes are the times that can only be to come, which is what
	// tells "school shooting tomorrow" from news of one "today".
	futureTimes = words(`tomorrow`, `tmrw`, `tmr`, `tmrrw`, `tomoz`, `2moro`, `2morrow`, `tonight`, `next~week`,
		words(`next`, `this`, `coming`)+`~`+weekdays,
		`in~\d+~`+words(`mins?`, `minutes`, `hours?`, `hrs?`, `days?`))

	threatModals = words(`will`, `'?ll`, `wil`, `shall`, `wanna`, `want~to`, `gonna`, `gunna`, `going~to`,
		`planning~to`, `planning~on`, `plan~to`, `plan~on`, `intend~to`, `about~to`, `ready~to`, `finna`, `gotta`,
		`bout~to`)
	threatAdverbs  = `(?:(?:really|totally|seriously|definitely|actually|literally|just|finally)~)?`
	firstPersonOn  = words(`(?:i|we)~(?:(?:'?m|am|are|'?re|r)~)?`+threatAdverbs+threatModals, `imma`, `ima`, `i~'?ma`)
	firstPersonIng = `(?:i|we)~` + words(`'?m`, `am`, `are`, `'?re`, `r`, words(`'?ll`, `will`, `wil`)+`~be`)

	threatIntentRules = []rule{
		bothViews(`\b` + words(
			// "i will", "we're gonna", "imma": then what.
			firstPersonOn+`~`+threatAdverbs+threatActs(false),
			// "i'm bringing", "we're shooting up", "i'll be shooting".
			firstPersonIng+`~`+threatAdverbs+threatActs(true),
			// "tomorrow i shoot up the school": the present, with the strongest
			// acts only, since "we kill it at the fest" is how people talk.
			`(?:i|we)~`+threatAdverbs+threatStrongActs,
			words(`you`, `u`, `y'?all`, `yall`, `you~all`, `everyone`, `everybody`, `all~of~you`)+
				`~(?:`+words(`'?re`, `are`, `r`, `is`, `will`, `'?ll`)+`~)?(?:all~)?`+words(`going~to`, `gonna`, `gunna`, `will`)+
				`~`+words(`die`, `get~`+words(`shot`, `stabbed`, `hurt`, `killed`, `blown~up`)),
			// "you're dead after school"
			words(`you`, `u`)+`~`+words(`'?re`, `are`, `r`)+`~dead(?:~meat)?`,
			`there~`+words(`will`, `'?ll`, `is~going~to`, `'?s~going~to`, `s~going~to`, `'?s~gonna`, `is~gonna`,
				`s~gonna`, `might`)+`~be~(?:a~)?`+
				words(`shooting`, `bombing`, `massacre`, `bloodbath`, `blood~bath`, `bomb`, `attack`, `school~shooting`),
			// "there's a bomb in the school"
			`there~`+words(`'?s`, `is`, `are`)+`~(?:`+words(`a`, `an`)+`~)?`+
				words(`bombs?`, `guns?`, `shooter`, `active~shooter`)+`~`+words(`in`, `at`, `inside`, `on`),
			// Hinglish: "sabko goli maar dunga", "tujhe maar dalunga".
			words(`sabko`, `sab~ko`, `sabhi~ko`, `tujhe`, `tumhe`, `tumko`, `usko`, `unko`, `isko`, `tereko`)+
				`~(?:goli~)?maar~`+words(`dunga`, `dungi`, `denge`, `daalunga`, `dalunga`, `daalungi`, `dalungi`),
			`goli~maar~`+words(`dunga`, `dungi`, `denge`),
		) + `\b`),
		// With no subject, at the start of a sentence or line: "gonna shoot up
		// the school tmrw", "shooting up the school tomorrow, don't come".
		bothViews(`(?:^|[.!?;:\n])[^\pL\pN]*` + words(
			words(`gonna`, `gunna`, `going~to`, `finna`, `bout~to`, `about~to`)+`~`+threatAdverbs+threatActs(false),
			threatStrongProgressive,
		) + `\b`),
	}
	// namedTargetRules threaten someone by name: "I'm going to shoot Rahul
	// tomorrow". namedTargetCheck takes the name only when it is written
	// with a capital and is not a day, a subject or a game.
	namedTargetRules = []rule{bothViews(`\b` + words(
		firstPersonOn+`~`+threatAdverbs+words(`shoot`, `stab`, `kill`, `murder`),
		firstPersonIng+`~`+threatAdverbs+words(`shooting`, `stabbing`, `killing`, `murdering`),
	) + `[^\pL\pN]+(\pL{2,20})\b`)}
	threatPlaceOrTimeRules = []rule{bothViews(`\b` + words(threatPlaces, threatTimes) + `\b`)}

	// "school shooting tomorrow at 10", with only a time still to come, close.
	schoolShootingRules = []rule{bothViews(`\bschool~shooting\b`)}
	futureTimeRules     = []rule{bothViews(`\b` + futureTimes + `\b`)}

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
func threatActs(ing bool) string {
	v := func(base, progressive string) string {
		if ing {
			return progressive
		}
		return base
	}
	det := `(?:` + words(`the`, `my`, `our`, `this`, `that`, `ur`, `your`) + `~)?`
	return words(
		words(v(`shoot(?:~up)?`, `shooting(?:~up)?`), v(`stab`, `stabbing`), v(`kill`, `killing`),
			v(`murder`, `murdering`), v(`massacre`, `massacring`), v(`slaughter`, `slaughtering`),
			v(`gun~down`, `gunning~down`), v(`knife`, `knifing`), v(`attack`, `attacking`), v(`hurt`, `hurting`),
			v(`beat~up`, `beating~up`), v(`behead`, `beheading`), v(`shank`, `shanking`))+`~`+threatPeople,
		words(v(`shoot~up`, `shooting~up`), v(`shoot~(?:at|in|inside)`, `shooting~(?:at|in|inside)`),
			v(`bomb`, `bombing`), v(`blow~up`, `blowing~up`), v(`burn~down`, `burning~down`), v(`torch`, `torching`),
			v(`set~fire~to`, `setting~fire~to`), v(`attack`, `attacking`))+`~`+det+threatPlaces,
		v(`set`, `setting`)+`~`+det+threatPlaces+`~on~fire`,
		v(`burn`, `burning`)+`~`+det+threatPlaces+`~down`,
		v(`shoot`, `shooting`)+`~(?:the~)?`+words(`place`, `school`, `it`)+`~up`, v(`shoot~up`, `shooting~up`),
		v(`bring`, `bringing`)+`~`+weapon+`~`+words(`to`, `into`, `in`)+`~(?:`+words(`the`, `my`, `our`)+`~)?`+threatPlaces,
		// "I'm bringing a gun": a firearm or a bomb needs no place after it.
		v(`bring`, `bringing`)+`~`+firearm,
		v(`come`, `coming`)+`~`+words(`to`, `into`)+`~(?:`+words(`the`, `my`, `our`)+`~)?`+threatPlaces+`~with~`+weapon,
		v(`come`, `coming`)+`~with~`+weapon+`~`+words(`to`, `into`)+`~(?:`+words(`the`, `my`, `our`)+`~)?`+threatPlaces,
		v(`pull`, `pulling`)+`~(?:a~)?columbine`,
		words(v(`plant`, `planting`), v(`set~off`, `setting~off`), v(`detonate`, `detonating`))+
			`~(?:`+words(`a`, `the`)+`~)?`+words(`bomb`, `explosives?`, `device`),
	)
}

// weapon is what a threat says will be brought: "a gun", "my dad's gun".
// firearm leaves out the knife, which also cuts cake.
var (
	weaponOwner = `(?:` + words(`a`, `my`, `the`, `an`, `my~dad'?s`, `my~brother'?s`, `his`) + `~)?`
	weapon      = weaponOwner + words(`guns?`, `knife`, `knives`, `bombs?`, `weapons?`, `rifles?`, `pistols?`,
		`explosives?`, `ak`, `ar`, `shotguns?`)
	firearm = weaponOwner + words(`guns?`, `bombs?`, `rifles?`, `pistols?`, `explosives?`, `weapons?`, `ak(?:~?47)?`,
		`ar(?:~?15)?`, `shotguns?`)
)

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
		`bringing~`+weapon+`~`+words(`to`, `into`)+`~(?:`+words(`the`, `my`, `our`)+`~)?`+threatPlaces,
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
		event club contest game lesson tournament`)
	metaphorAttack = regexp.MustCompile(canon(`blood[^\pL\pN]*bath|massacre`))
	// deadNotThreat, after "you're dead", is the idiom: "dead to me", "dead
	// tired", "dead wrong", "dead serious".
	deadNotThreat = canonSetOf(`to tired wrong right serious on inside set lucky funny cute gorgeous`)
	dieNotThreat  = canonSetOf(`someday eventually old age`)
	// conditionalPlace is a condition about being somewhere: "if you come to
	// school tomorrow you will die" threatens; "you will die if you eat that
	// canteen food" warns.
	conditionalPlace = regexp.MustCompile(`\b(?:come|coming|go|going|show[^\pL\pN]*up|turn[^\pL\pN]*up|step|walk|enter|be)` +
		`[^\pL\pN]+(?:(?:to|into|in|at|inside)[^\pL\pN]+)?(?:(?:the|my|our)[^\pL\pN]+)?` + phrase(threatPlaces) + `\b`)
	conditionalPlaceLoose = regexp.MustCompile(canon(conditionalPlace.String()))
	// peopleShootPurpose is what makes a shoot of people a photo shoot.
	peopleShootPurpose = canonSetOf(`photos photo pictures pics portraits headshots yearbook photoshoot film movie video
		documentary photography`)
)

// threatCheck turns down the commonest harmless readings of an intent match:
// an event straight after it ("bomb the class presentation"), unless the act
// is one no event excuses; a message after "shoot" ("shoot you the notes"); a
// shoot that is for a film, the yearbook or photography club; a game named
// after "at", "in" or "on" ("we're gonna kill them at the match"); "a
// shooting star" and "a bloodbath in the maths exam"; "you're dead to me";
// "you will die" in a game, someday, or on a condition that is not being
// somewhere; and a threat quoted in a report of one ("'I will shoot up the
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
	switch {
	case threatThere.MatchString(cm):
		if len(next) > 0 && thereNotThreat[next[0]] || gameAfterAPreposition(next) {
			return false
		}
		if metaphorAttack.MatchString(cm) && anyIn(wordsIn(sentence), eventWords) {
			return false
		}
	case is(last, "dead") || is(last, "meat"):
		return !(len(next) > 0 && deadNotThreat[next[0]])
	case is(last, "die"):
		if gameDeath(next) || anyIn(next, dieNotThreat) || strings.Contains(canon(after), canon("one day")) {
			return false
		}
		if conditional(sentence) && !conditionalPlace.MatchString(sentence) && !conditionalPlaceLoose.MatchString(sentence) {
			return false
		}
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
	if gameVerb.MatchString(cm) && gameAfterAPreposition(next) {
		return false
	}
	return !reportedThreat(text, m)
}

// gameAfterAPreposition reports a game named after "at", "in" or "on" in the
// words after a match: "kill them at the match", not "kill my teacher after
// the game". A school or a class first ends the look.
func gameAfterAPreposition(next []string) bool {
	for i, w := range next {
		if is(w, "school") || is(w, "class") {
			return false
		}
		if w != "at" && w != "in" && w != "on" {
			continue
		}
		for _, g := range next[i+1 : min(len(next), i+4)] {
			if gameWords[g] {
				return true
			}
		}
	}
	return false
}

// conditional reports an "if" or "unless" in a sentence.
func conditional(sentence string) bool {
	for _, w := range wordsIn(sentence) {
		if w == "if" || w == "unless" || w == canon("unless") {
			return true
		}
	}
	return false
}

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

// reportedThreat reports a threat quoted inside a report of it: the match
// starts inside quotation marks that close after it, a report word stands
// outside the quotation, and nothing outside it is in the first person.
// "\"I will shoot up the school tomorrow\" and the police can't stop me" is
// the poster's own.
func reportedThreat(text string, m []int) bool {
	if !quoted(text, m[0]) {
		return false
	}
	open, _ := utf8.DecodeLastRuneInString(strings.TrimRight(text[:m[0]], " "))
	closing := map[rune]string{'"': `"”`, '\'': `'’`, '“': `”"`, '‘': `’'`, '«': `»`}[open]
	end := strings.IndexAny(text[m[1]:min(len(text), m[1]+threatWindow)], closing)
	if end < 0 {
		return false
	}
	start := strings.LastIndex(text[:m[0]], string(open))
	outside := text[max(0, start-100):start] + " " + text[m[1]+end:min(len(text), m[1]+end+100)]
	ws := wordsIn(canon(outside))
	return anyIn(ws, reportWords) && !hasFirstPerson(wordsIn(outside))
}

// namedTargetCheck takes "shoot Rahul" when the name is written with a
// capital in the source and is not a day, a month, a subject or a game, and
// then reads it as threatCheck reads any threat.
func (f *scannedField) namedTargetCheck(v *view, m []int) bool {
	a, b := v.span(m[2], m[3])
	name := f.src[a:b]
	r, _ := utf8.DecodeRuneInString(name)
	if !unicode.IsUpper(r) || notANameTarget[strings.ToLower(name)] || gameWords[strings.ToLower(name)] {
		return false
	}
	return f.threatCheck(v, m)
}

var notANameTarget = setOf(`monday tuesday wednesday thursday friday saturday sunday january february march april
	may june july august september october november december physics chemistry maths math mathematics biology
	english hindi history geography economics accounts sanskrit french german science it cs the this that it everyone
	everybody time god you him her them me myself himself herself all`)

// schoolShootingCheck turns down "school shooting" in a drill, a lesson or
// the news: "a school shooting drill tomorrow", "an essay on school shooting
// prevention".
func schoolShootingCheck(v *view, m []int) bool {
	before := wordsIn(clauseBefore(v.text, m[0]))
	near := append(before[max(0, len(before)-4):], nextWords(clauseAfter(v.text, m[1]), 4)...)
	return !anyIn(near, schoolShootingTopics)
}

var schoolShootingTopics = canonSetOf(`drill drills prevention essay essays debate debates discussion discuss discussing
	news article articles report reports documentary awareness lockdown training survivors survivor victims memorial
	statistics stats project presentation research policy laws law safety protest protests`)

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
	gameWords = canonSetOf(`match matches game games chess football soccer cricket basketball volleyball hockey
		tennis badminton kabaddi tournament finals final semis semifinal semifinals quiz debate competition race
		fifa fortnite valorant pubg bgmi minecraft roblox cod league olympiad hackathon contest sports carrom
		ludo uno mock battle bands`)
	// reportWords, round a quoted threat, say it is reported rather than made.
	reportWords = canonSetOf(`headline headlines news court police arrested charged accused reportedly allegedly
		reported reports hears heard trial jury sentenced convicted article according journalist`)
)

// quoted reports that the match at i starts just inside a quotation mark.
func quoted(text string, i int) bool {
	r, _ := utf8.DecodeLastRuneInString(strings.TrimRight(text[:i], " "))
	return strings.ContainsRune(`"'“‘«`, r)
}

// nextWords splits the start of s into at most n words of letters and digits.
func nextWords(s string, n int) []string {
	words := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-'
	})
	return words[:min(n, len(words))]
}

// sentenceAfter is the rest of the sentence after i, within 64 bytes.
func sentenceAfter(text string, i int) string {
	after := text[i:min(len(text), i+64)]
	if j := strings.IndexAny(after, ".!?\n"); j >= 0 {
		after = after[:j]
	}
	return after
}

// threatWindow is how close, in bytes of the post, a place or a time has to
// be to an intent to count as part of the same threat.
const threatWindow = 200

// Grooming cues: a question about age, school year or where someone lives or
// studies, together with a private-contact or meet-up cue, anywhere in one
// post, in English or some Hinglish. Either half alone is ordinary
// conversation.
var (
	ageQuestionRules = []rule{bothViews(`\b` + words(
		`how~old~(?:`+words(`are`, `r`, `is`)+`~)?`+words(`you`, `u`, `ya`, `yu`),
		`what'?s?~(?:is~)?`+words(`your`, `ur`, `yr`)+`~age`,
		`a~s~l`,
		words(`what`, `which`)+`~`+words(`year`, `grade`, `class`, `standard`, `std`, `form`)+`~`+
			words(`are`, `r`)+`~`+words(`you`, `u`),
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~(?:in~)?`+words(`high~school`, `middle~school`,
			`secondary~school`, `year~\d{1,2}`, `grade~\d{1,2}`, `class~\d{1,2}`, `std~\d{1,2}`,
			`\d{1,2}(?:st|nd|rd|th)~`+words(`grade`, `class`, `standard`, `std`)),
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~`+words(`under`, `over`, `below`, `above`)+`~1\d`,
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~1\d`,
		`where~`+words(`do`, `d`)+`~`+words(`you`, `u`)+`~`+words(`live`, `stay`, `go~to~school`, `study`),
		`where~`+words(`are`, `r`)+`~`+words(`you`, `u`)+`~from`,
		`where~`+words(`is`, `'?s`)+`~`+words(`your`, `ur`)+`~`+words(`school`, `house`, `home`),
		// "which college do you go to", "what area are you in": the verb has
		// to be about being there, so "which college do you suggest" is
		// advice.
		words(`what`, `which`)+`~`+words(`school`, `college`, `uni`, `city`, `town`, `area`, `place`, `locality`)+`~`+
			words(`do`, `d`)+`~`+words(`you`, `u`)+`~`+words(`go~to`, `attend`, `study~at`, `study~in`, `study`,
			`live~in`, `live`, `stay~in`, `stay`, `come~from`, `belong~to`),
		words(`what`, `which`)+`~`+words(`school`, `college`, `uni`, `city`, `town`, `area`, `place`, `locality`)+`~`+
			words(`are`, `r`)+`~`+words(`you`, `u`)+`~`+words(`in`, `at`, `from`, `studying`, `living`, `staying`),
		`what'?s?~(?:is~)?`+words(`your`, `ur`)+`~`+words(`school`, `address`, `location`, `city`, `town`, `area`,
			`postcode`, `pincode`, `zip`),
		words(`do`, `d`)+`~`+words(`you`, `u`)+`~live~`+words(`near`, `in`, `around`, `close`),
		`mature~for~`+words(`your`, `ur`)+`~age`,
		// Hinglish: "kitne saal ke ho", "kahan rehte ho", "kaunsa school".
		words(`kitne`, `kitni`)+`~saal~`+words(`ke`, `ki`)+`~`+words(`ho`, `hai`, `h`),
		words(`kitni`, `kya`)+`~`+words(`umar`, `umr`, `age`)+`~`+words(`hai`, `h`),
		words(`tumhari`, `teri`, `aapki`, `apki`)+`~`+words(`umar`, `umr`, `age`),
		words(`kahan`, `kaha`, `kahaan`)+`~`+words(`rehte`, `rehti`, `rahte`, `rahti`, `padhte`, `padhti`)+`~`+
			words(`ho`, `hai`, `h`),
		words(`kaunsa`, `kaun~sa`, `konsa`, `kaunse`, `konse`)+`~`+words(`school`, `college`, `class`),
	) + `\b`),
		// Questions that end in their "?", where no word boundary follows:
		// "your age?", "u 14?", and "age?", "how old?" or "which school?"
		// opening a sentence.
		bothViews(words(
			`\b`+words(`your`, `ur`)+`~age[^\pL\pN?]*\?`,
			`\b`+words(`you`, `u`)+`~1\d[^\pL\pN?]*\?`,
			`(?:^|[.!?\n,;:])[^\pL\pN]*`+words(`age`, `how~old`, words(`which`, `what`)+`~`+
				words(`school`, `college`, `class`, `grade`, `year`))+`[^\pL\pN?]*\?`,
		)),
	}
	privateCueRules = []rule{bothViews(`\b` + words(
		words(`dm`, `pm`, `message`, `msg`, `text`, `txt`, `inbox`, `snap`, `call`, `ping`, `add`)+`~me`,
		`talk~`+words(`privately`, `in~private`, `alone`, `somewhere~else`, `elsewhere`, `in~dms?`, `one~on~one`, `1~on~1`),
		`chat~`+words(`privately`, `in~private`, `alone`, `one~on~one`),
		`private~`+words(`chat`, `talk`, `messages?`, `convo`, `conversation`),
		`somewhere~`+words(`private`, `else`),
		`meet~`+words(`up`, `irl`, `in~person`, `me`, `somewhere`, `in~real~life`),
		`meetup`, `wanna~meet`, `let'?s~meet`, `lets~meet`, `can~we~meet`, `we~should~meet`,
		`send~(?:me~)?(?:`+words(`a`, `some`, `your`, `ur`)+`~)?`+words(`pics?`, `pictures?`, `photos?`, `selfies?`,
			`nudes`, `snaps`, `vids?`, `videos?`),
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~(?:home~)?alone`,
		words(`are`, `r`)+`~`+words(`your`, `ur`)+`~`+words(`parents`, `mom`, `mum`, `dad`)+`~`+
			words(`home`, `around`, `there`, `awake`),
		words(`don'?t`, `dont`, `do~not`)+`~tell~`+words(`your`, `ur`, `anyone`, `anybody`),
		words(`our`, `a`)+`~(?:little~)?secret`,
		`keep~`+words(`this`, `it`)+`~(?:`+words(`a`, `our`)+`~)?secret`,
		`keep~`+words(`this`, `it`)+`~between~`+words(`us`, `you~and~me`, `u~and~me`, `the~two~of~us`),
		`between~`+words(`you`, `u`)+`~and~me`,
		`send~(?:me~)?`+words(`your`, `ur`)+`~(?:live~)?`+words(`location`, `loc`, `address`, `pin`, `pincode`),
		`share~`+words(`your`, `ur`)+`~(?:live~)?`+words(`location`, `loc`, `address`),
		`hmu`, `come~over`,
		// Hinglish: "dm karo", "insta pe aao", "akele ho", "milte hain".
		words(`dm`, `pm`, `msg`, `message`, `text`, `call`, `baat`)+`~`+karo,
		words(`pe`, `par`, `pr`)+`~`+words(`aao`, `aaja`, `aajao`, `aa~jao`, `milo`),
		words(`akele`, `akeli`, `akela`)+`~`+words(`ho`, `hai`, `h`),
		words(`milte`, `milna`)+`~`+words(`hain`, `hai`, `h`),
		`private~`+words(`mein`, `me`, `main`)+`~baat`,
	) + `\b`),
		// "pic?": a request for one that ends in its "?".
		bothViews(`\b` + words(`pics?`, `pictures?`, `photos?`, `selfies?`) + `[^\pL\pN?]*\?`),
	}
)

// ageCheck turns down "what's your area" when it is a field of study: "what's
// your area of interest", "your location preference".
func ageCheck(v *view, m []int) bool {
	ws := wordsIn(v.text[m[0]:m[1]])
	if len(ws) == 0 || !is(ws[len(ws)-1], "area") && !is(ws[len(ws)-1], "location") {
		return true
	}
	next := nextWords(v.text[m[1]:min(len(v.text), m[1]+32)], 1)
	return len(next) == 0 || !areaTopics[next[0]]
}

var areaTopics = canonSetOf(`of preference preferences expertise interest`)

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

// viewRegexp is one view of a field and the regexp a rule has for it.
type viewRegexp struct {
	v  *view
	re *regexp.Regexp
}

// views returns the field's views paired with the regexp a rule has for each,
// leaving out the plain view when the loose one stands for it.
func (f *scannedField) views(r rule) []viewRegexp {
	if r.loose == nil {
		return []viewRegexp{{&f.plain, r.plain}}
	}
	if f.looseOnly {
		return []viewRegexp{{&f.loose, r.loose}}
	}
	return []viewRegexp{{&f.plain, r.plain}, {&f.loose, r.loose}}
}

// first returns the source span of the earliest match of r, in either view,
// that check accepts.
func (f *scannedField) first(r rule, check matchCheck) (int, int, bool) {
	from, to, found := 0, 0, false
	for _, vr := range f.views(r) {
		if check == nil {
			if m := vr.re.FindStringIndex(vr.v.text); m != nil && m[1] > m[0] {
				if a, b := vr.v.span(m[0], m[1]); !found || a < from {
					from, to, found = a, b, true
				}
			}
			continue
		}
		for _, m := range vr.re.FindAllStringSubmatchIndex(vr.v.text, -1) {
			if m[1] <= m[0] || !check(vr.v, m) {
				continue
			}
			a, b := vr.v.span(m[0], m[1])
			if !found || a < from {
				from, to, found = a, b, true
			}
			break
		}
	}
	return from, to, found
}

// anyMatch reports whether any of rules matches in either view.
func (f *scannedField) anyMatch(rules []rule, check matchCheck) bool {
	for _, r := range rules {
		if check == nil {
			for _, vr := range f.views(r) {
				if vr.re.MatchString(vr.v.text) {
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
		for _, vr := range f.views(r) {
			for _, m := range vr.re.FindAllStringSubmatchIndex(vr.v.text, -1) {
				if m[1] > m[0] && (check == nil || check(vr.v, m)) {
					a, b := vr.v.span(m[0], m[1])
					out = append(out, [2]int{a, b})
				}
			}
		}
	}
	sortSpans(out)
	return out
}

// threatens reports a threat: an intent with a place or a time within
// threatWindow of it, a warning with a cue that it is one, or "school
// shooting" with a time still to come close by. The fields are read as one
// text, in form order, because a subject is shown as the title right above
// the body: "School tomorrow" over "I'm going to shoot everyone" is one
// threat.
func threatens(fields []*scannedField) bool {
	pair := func(first, second []rule, check func(f *scannedField) matchCheck, w int) bool {
		var a, b [][2]int
		for _, f := range fields {
			a = append(a, f.spansFrom(first, check(f))...)
		}
		if len(a) == 0 {
			return false
		}
		for _, f := range fields {
			b = append(b, f.spansFrom(second, nil)...)
		}
		return near(a, b, w)
	}
	byField := func(check matchCheck) func(*scannedField) matchCheck {
		return func(*scannedField) matchCheck { return check }
	}
	return pair(threatIntentRules, threatPlaceOrTimeRules, func(f *scannedField) matchCheck { return f.threatCheck }, threatWindow) ||
		pair(namedTargetRules, threatPlaceOrTimeRules, func(f *scannedField) matchCheck { return f.namedTargetCheck }, threatWindow) ||
		pair(threatWarningRules, threatWarningCues, byField(nil), threatWindow) ||
		pair(schoolShootingRules, futureTimeRules, byField(schoolShootingCheck), schoolShootingWindow)
}

// schoolShootingWindow is how close a time to come has to be to "school
// shooting" for the two to be one statement.
const schoolShootingWindow = 40

// spansFrom is spans placed in the post as a whole: offset by where the
// field starts when the fields are read one after another, in form order,
// each a line apart, as the page shows them.
func (f *scannedField) spansFrom(rules []rule, check matchCheck) [][2]int {
	spans := f.spans(rules, check)
	for i := range spans {
		spans[i][0] += f.offset
		spans[i][1] += f.offset
	}
	return spans
}

// near reports whether some span of b lies within w bytes of some span of a.
// Both are sorted by start; the scan is linear in their lengths.
func near(a, b [][2]int, w int) bool {
	j := 0
	for _, x := range a {
		for j < len(b) && b[j][1] < x[0]-w {
			j++
		}
		if j < len(b) && b[j][0] <= x[1]+w {
			return true
		}
	}
	return false
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

// is reports that w is word, as either view writes it.
func is(w, word string) bool { return w == word || w == canon(word) }

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
	pubg bgmi game games ingame level levels raid dungeon creeper creepers zombie zombies nether ender cod`)

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
	switch {
	case is(last, "kms"):
		return f.kmsCheck(v, m, next)
	case selfCompound(text, m[1]):
		return false
	case livingWords[last] || is(last, "life") && hasWord(ws, "with"):
		return livingCheck(text, m, ws, next)
	case (hasWord(ws, "cut") || hasWord(ws, "cutting")) && nextIn(0, cutIdioms):
		return false
	case len(ws) >= 2 && is(ws[0], "i") && simplePast[ws[1]] && nextIn(0, accidents):
		// "I burned myself on the Bunsen burner", "i cut myself shaving".
		return false
	case is(last, "die"):
		switch {
		case nextIn(0, setOf(`of from`)) && nextIn(1, dieIdioms), nextIn(0, setOf(`laughing laughin`)):
			return false
		case len(next) > 0 && is(next[0], "on") && (hasWord(next[:min(len(next), 4)], "hill") ||
			hasWord(wordsIn(clauseBefore(text, m[0])), "hill")):
			return false
		}
	case is(last, "disappear") && nextIn(0, setOf(`from for`)):
		return anyIn(next[1:min(len(next), 3)], canonSetOf(`world earth existence life everyone everything good ever forever`))
	case is(last, "myself") && (hasWord(ws, "kill") || hasWord(ws, "killing")) && nextIn(0, canonSetOf(`laughing laughin lol`)):
		return false
	case is(last, "it") && (hasWord(ws, "end") || hasWord(ws, "ending")):
		// "end it" is the end of something else unless nothing, or a time,
		// comes after it: "i'm going to end it tonight", not "end it with her".
		return len(next) == 0 || nextIn(0, endItTimes)
	case is(last, "dying") && nextIn(0, dyeObjects):
		return false
	case is(last, "suicide") && nextIn(0, suicideTopics):
		return false
	}
	return true
}

var (
	livingWords   = canonSetOf(`live living exist up on`)
	lifeTopics    = canonSetOf(`science sciences skills skill insurance cycle cycles style styles span coach coaching lessons lesson hacks goals choices story stories partner expectancy support`)
	livingPlaces  = canonSetOf(`in with on at there here near abroad without together alone by like tomorrow today tonight next this until after before during for early late`)
	livingCues    = []string{"anymore", "any more", "like this", "this world", "this life"}
	dontWords     = canonSetOf(`don't dont dnt do`)
	dontSubjects  = canonSetOf(`just really honestly seriously literally so and but lol tbh ngl man bro yaar rn now today tonight anymore okay ok also still kinda like yeah i`)
	cutIdioms     = canonSetOf(`off a an some slack short loose out free`)
	simplePast    = canonSetOf(`cut burned burnt`)
	accidents     = canonSetOf(`on while shaving cooking ironing accidentally by during making opening chopping slicing playing when trying`)
	dieIdioms     = canonSetOf(`laughing laughter embarrassment shame boredom cringe cuteness happiness excitement jealousy envy hunger thirst heat curiosity suspense anticipation secondhand`)
	endItTimes    = canonSetOf(`tonight today now soon all tomorrow already finally forever here this`)
	dyeObjects    = canonSetOf(`my her his their our your the hair it them clothes shirt`)
	suicideTopics = canonSetOf(`prevention awareness essay essays rates rate statistics stats hotline helpline helplines squad bombing bomber bombers attack attacks mission missions doors door run runs research project report article documentary`)
	kmsQuantity   = canonSetOf(`many few some several couple more less fewer extra total thousand thousands hundred hundreds lakh lakhs million millions of the those these in kitne kitna`)
	kmsAfter      = canonSetOf(`away per from long far ahead left of an a hai`)
	kmsContext    = canonSetOf(`aws key keys encrypt encrypted encryption activation activate windows server license licence gcp azure cloud cmk s3 miles mile metres meters kilometres kilometers distance convert conversion speed kmph kmh mileage petrol diesel litre liter`)
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
	return len(next) == 0 || !lifeTopics[next[0]] && !livingPlaces[next[0]]
}

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
// bleach", "please don't kys", "careful, you'll cut yourself". The negation
// has to come straight before the words matched, in the same clause — "don't"
// or "never", with at most "you", "ever", "even", "dare", "go" or "just"
// between — and "careful" straight before a "you'll", "could" or "might". A
// negation further back is about something else: "i don't care, kys", "dont
// reply. just kill yourself", "never mind, kys", "you're not careful enough,
// kys".
func notNegated(v *view, m []int) bool {
	text := v.text
	before := text[max(0, m[0]-64):m[0]]
	sentence := before[strings.LastIndexAny(before, ".!?\n")+1:]
	if sw := wordsIn(sentence); len(sw) >= 2 && carefulModals[sw[len(sw)-1]] &&
		hasWord(sw[max(0, len(sw)-3):len(sw)-1], "careful") {
		return false
	}
	words := strings.Fields(sentence[strings.LastIndexAny(sentence, ",;:()\"")+1:])
	for len(words) > 0 && negationFillers[words[len(words)-1]] {
		words = words[:len(words)-1]
	}
	tail := " " + strings.Join(words, " ")
	for _, neg := range negations {
		if strings.HasSuffix(tail, " "+neg) || strings.HasSuffix(tail, " "+canon(neg)) {
			return false
		}
	}
	return true
}

// negations are what turn "kill yourself" into advice against it, when they
// come straight before it; negationFillers may come between. carefulModals
// end a warning that starts "careful": "careful, you could cut yourself".
var (
	negations = []string{"don't", "dont", "do not", "never", "not to", "didn't", "didnt", "shouldn't",
		"shouldnt", "won't", "wont", "mustn't"}
	negationFillers = canonSetOf(`you u ever even dare go just please pls`)
	carefulModals   = canonSetOf(`you'll youll u'll ull could might can may will`)
)

// selfHarmCheck is notNegated with the places a self-harm phrase is something
// else: "rope yourself in" (to a project); "self-doubt"; "cut yourself some
// slack" or "off"; "cut deeper into the topic"; a death in a game; a warning
// about overwork ("you'll kill yourself studying"); "no one will care if you
// skip the party", where nothing about dying follows; and an imperative with
// no "you" ("drink bleach", "just die") that is not the start of what is said
// ("i'd rather drink bleach", "i just die every time").
func selfHarmCheck(v *view, m []int) bool {
	if !notNegated(v, m) {
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
	case selfCompound(text, m[1]), gameDeath(next):
		return false
	case hasWord(ws, "cut") && !is(last, "deeper") && nextIn(0, cutIdioms):
		return false
	case is(last, "deeper") && nextIn(0, deeperObjects):
		return false
	case hasWord(ws, "miss") || hasWord(ws, "care"):
		return caresIfYouDie(next)
	}
	if prev := wordsIn(clauseBefore(text, m[0])); len(prev) > 0 && len(next) > 0 && strings.HasSuffix(next[0], "ing") {
		p := prev[len(prev)-1]
		if overworkModals[p] || len(prev) >= 2 && is(prev[len(prev)-2], "going") && is(p, "to") {
			return false // "you'll kill yourself studying like that"
		}
	}
	bare := is(first, "jump") || is(first, "drink") || is(first, "just") && is(last, "die") ||
		is(first, "cut") && is(last, "deeper") || is(first, "mar") || is(first, "ja") || is(first, "jaake")
	if bare {
		for _, w := range wordsIn(clauseBefore(text, m[0])) {
			if !imperativeFillers[w] {
				return false
			}
		}
	}
	return true
}

var (
	deeperObjects     = canonSetOf(`into in through the a an on with than to`)
	overworkModals    = canonSetOf(`you'll youll u'll ull will gonna gon`)
	imperativeFillers = canonSetOf(`just go pls please plz now then lol lmao bro dude man honestly seriously ok okay so
		and or yeah fr ngl also you u like literally really already oh omg tbh idc well yo hey bhai yaar abe chal`)
	deathWords   = canonSetOf(`die died dead disappear disappeared vanish vanished gone killed kill kys ended jumped hanged overdosed od`)
	careFillers  = canonSetOf(`if when you u you're youre ur were was are just simply even all`)
	trailFillers = canonSetOf(`lol lmao tbh honestly fr ngl bro anyway anyways though tho`)
)

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
	return len(rest) > 0 && deathWords[rest[0]]
}

// methodCheck is notNegated with the method questions that are about the
// world, not oneself, turned down: a "how much is lethal" or a "lethal dose"
// with no medicine and no "I" or "me" ("how much caffeine is lethal", "the
// lethal dose for half the test animals"), "how deep should I cut" followed
// by something to cut ("the wood"), a death in a game, and "self-doubt".
func methodCheck(v *view, m []int) bool {
	if !notNegated(v, m) {
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
	case is(first, "how") && (is(ws[1], "many") || is(ws[1], "much")), is(first, "what"):
		return namesAMedicine(matched) || hasFirstPerson(ws)
	case is(first, "lethal") || is(first, "fatal") || is(first, "deadly"):
		return namesAMedicine(text[m[1]:min(len(text), m[1]+48)]) || hasFirstPerson(wordsIn(sentenceAround(text, m[0], m[1])))
	case is(first, "how") && is(ws[1], "deep"):
		return len(next) == 0 || deepCutTargets[next[0]]
	}
	return true
}

var (
	medicinePlain  = regexp.MustCompile(`\b` + phrase(medicines) + `\b`)
	medicineLoose  = regexp.MustCompile(`\b` + canon(phrase(medicines)) + `\b`)
	deepCutTargets = canonSetOf(`my myself to so for without wrist wrists arm arms thigh thighs leg legs skin`)
)

func namesAMedicine(s string) bool {
	return medicinePlain.MatchString(s) || medicineLoose.MatchString(s)
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
	from, to, ok := f.first(niRule, func(v *view, m []int) bool {
		return validNIPrefix(v.text[m[2]], v.text[m[4]]) && f.upperInSource(m[0], m[1])
	})
	consider(from, to, ok, kindNI)

	from, to, ok = f.first(emailRule, func(v *view, m []int) bool {
		text := v.text
		local, domain, dot, tld := text[m[2]:m[3]], text[m[4]:m[5]], text[m[6]:m[7]], text[m[8]:m[9]]
		// A dot with a space beside it and no brackets round it.
		spaced := dot != "." && strings.TrimSpace(dot) == "."
		return emailAddress(local, bracketedDots(domain), readTLD(tld), spaced)
	})
	consider(from, to, ok, kindEmail)
	from, to, ok = f.first(providerEmailRule, func(v *view, m []int) bool {
		return providerEmail(v.text[m[2]:m[3]], v.text[m[4]:m[5]])
	})
	consider(from, to, ok, kindEmail)
	from, to, ok = f.first(spelledEmailRule, func(v *view, m []int) bool {
		text := v.text
		local, at, domain, dot, tld := text[m[2]:m[3]], text[m[4]:m[5]], text[m[6]:m[7]], text[m[8]:m[9]], text[m[10]:m[11]]
		plainDots := dot == "." && !spelledDotPattern.MatchString(domain)
		return spelledEmail(local, at, spelledDomain(domain), tld, plainDots, text[m[11]:])
	})
	consider(from, to, ok, kindEmail)
	for _, r := range []rule{myMailRule, mailColonHandle} {
		from, to, ok = f.first(r, f.handleCheck)
		consider(from, to, ok, kindEmail)
	}

	from, to, ok = f.first(linkRule, func(v *view, m []int) bool {
		return contactLink(v.text[m[2]:m[3]], v.text[m[4]:m[5]])
	})
	consider(from, to, ok, kindLink)
	from, to, ok = f.first(spacedInviteRule, nil)
	consider(from, to, ok, kindLink)

	for _, r := range askRules {
		from, to, ok = f.first(r, askCheck)
		consider(from, to, ok, kindAsk)
	}
	from, to, ok = f.first(atAskRule, func(v *view, m []int) bool {
		r, _ := utf8.DecodeRuneInString(v.text[m[1]:])
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	})
	consider(from, to, ok, kindAsk)
	from, to, ok = f.first(myAppRule, f.myAppCheck)
	consider(from, to, ok, kindHandle)
	// The handle rules find the app's name in either view, so "s.n.a.p" and
	// "sn4pch4t" count, and judge the handle from the source, where its
	// underscores, dots and digits are as the poster wrote them.
	for _, r := range handleRules {
		from, to, ok = f.first(r, f.handleCheck)
		consider(from, to, ok, kindHandle)
	}

	return best, found
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
	if strings.HasPrefix(v.text[m[5]:], "(") {
		return false
	}
	a, b := v.span(m[4], m[5])
	handle := f.src[a:b]
	if at && notHandles[strings.ToLower(handle)] {
		return false
	}
	return plausibleHandle(handle, at || discriminator)
}

// myAppCheck judges "my snap is <word>" by the word: an "@" before it or a
// username's marks make it a handle, notAHandle words do not, and a plain
// word is a name only where the sentence ends on it and it reads as no
// description ("my insta is priya", not "my insta is underrated").
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

var measureShape = regexp.MustCompile(`^(?:v\d+(?:\.\d+)*[a-z]?|\d+(?:[.,]\d+)?(?:st|nd|rd|th|am|pm|hrs?|mins?|secs?|ms|kg|km|cm|mm|gb|mb|kb|fps|x|k|e\d*|h|m|s)|\d+(?:\.\d+)?e[+-]?\d+|e\.g|i\.e)\.?$`)

// emailAddress vets a matched address. spaced says the dot before the
// top-level domain had a space beside it, which is allowed only for ".com",
// where it is a known dodge; anywhere else it is a sentence ending.
func emailAddress(local, domain, tld string, spaced bool) bool {
	if !emailTLDs[tld] || (spaced && tld != "com") {
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
// sentence: a name-like part before it ("priya_k", "coolkid12"), or a mail
// provider's domain with something other than "sign in" or "me" before it;
// with "dot" spelled out, an institution's domain will do too ("priya at iitb
// dot ac dot in"). A site followed by a path is a link: "docs at
// docs.python.org/3/".
func spelledEmail(local, at, domain, tld string, plainDots bool, next string) bool {
	if strings.HasPrefix(next, "/") || !emailAddress(local, domain, tld, false) {
		return false
	}
	if !weakAt(at) {
		return true
	}
	label, _, _ := strings.Cut(domain, ".")
	switch {
	case nameLike(local):
		return true
	case mailProviders[label]:
		return !notALocalPart[local]
	case plainDots:
		return false
	}
	return strings.IndexFunc(local, unicode.IsLetter) >= 0 &&
		(tld == "edu" || strings.HasSuffix(domain, ".ac") || strings.HasSuffix(domain, ".edu"))
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

// notALocalPart are the words that come before "at gmail.com" in a sentence.
var notALocalPart = setOf(`in it me us up on out log sign login signin logged signed account accounts email
	emails mail mails inbox them him her you there here is are available also now check find visit go look`)

var mailProviders = setOf(`gmail googlemail yahoo ymail rocketmail outlook hotmail live msn icloud mac aol
	proton protonmail pm tutanota tuta zoho rediffmail rediff yandex mail gmx qq 163 126 fastmail`)

// spelledDomain turns "gmail (dot) co dot in" back into "gmail.co.in".
func spelledDomain(domain string) string {
	return spelledDotPattern.ReplaceAllString(domain, ".")
}

var spelledDotPattern = regexp.MustCompile(`\s*[(\[{<]\s*(?:d[o0]t|\.)\s*[)\]}>]\s*|[\s_]+d[o0]t[\s_]+`)

/* Numbers --------------------------------------------------------------------- */

// digitRun is a number as written: digit groups separated by spaces, line
// breaks, dashes, dots or underscores, or by a comma on its own, perhaps with a
// leading "+" and the first group in brackets. Inside a group an o reads as 0,
// and an l or i as 1, when a digit is beside it ("98765 4321O").
type digitRun struct {
	from, to int // in the plain view's text
	digits   []byte
	groups   []int
	seps     []string
	plus     bool
	paren    bool // the first group is in brackets: "(415) 867-5309"
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
			c == '+' && i+2 < len(t) && t[i+1] == '(' && isASCIIDigit(t[i+2])
		if !starts {
			i++
			continue
		}
		if i > 0 {
			// Glued to a word or a longer number: part of something else,
			// such as C6H12O6, a hex string or the tail of an ISBN.
			if r, _ := utf8.DecodeLastRuneInString(t[:i]); unicode.IsLetter(r) || unicode.IsDigit(r) {
				i++
				continue
			}
		}
		run := scanDigitRun(t, i)
		if kind, ok := classifyNumber(t, run); ok {
			return run, kind, true
		}
		i = max(run.to, i+1)
	}
	return digitRun{}, 0, false
}

func isASCIIDigit(c byte) bool { return '0' <= c && c <= '9' }

func scanDigitRun(t string, i int) digitRun {
	run := digitRun{from: i}
	if t[i] == '+' {
		run.plus = true
		i++
	}
	for {
		paren := i < len(t) && t[i] == '('
		if paren {
			i++
		}
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
		next := j < len(t) && (isASCIIDigit(t[j]) || t[j] == '(' && j+1 < len(t) && isASCIIDigit(t[j+1]))
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

// digitLookalike reads the letter at t[i] as the digit it is written for in a
// number — o for 0, l and i for 1 — when a digit is beside it.
func digitLookalike(t string, i int) (byte, bool) {
	var d byte
	switch t[i] {
	case 'o':
		d = '0'
	case 'l', 'i':
		d = '1'
	default:
		return 0, false
	}
	if i > 0 && isASCIIDigit(t[i-1]) || i+1 < len(t) && isASCIIDigit(t[i+1]) {
		return d, true
	}
	return 0, false
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
// what kind of number it is: a reference, a rank, or an amount of money.
var numberLabels = setOf(`isbn issn doi arxiv pnr utr roll order invoice transaction txn ticket serial
	application app form registration reg enrol enroll enrolment enrollment account acc acct a/c policy
	jee neet cuet seat rank imei tracking awb reference ref
	rs inr usd eur gbp salary stipend fee fees price cost costs budget revenue profit turnover population`)

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
// to take it out. Each is the ten-digit national number: in India iCall, the
// Vandrevala Foundation and AASRA; in the US the old National Suicide
// Prevention Lifeline number (it still connects to 988), the Trevor Project,
// the National Domestic Violence Hotline and Childhelp.
var helplineNumbers = setOf(`9152987821 9999666555 9820466726 8002738255 8664887386 8007997233 8004224453`)

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
	if len(d) == 11 && d[0] == '1' { // a North American number with its 1
		d = d[1:]
	}
	return helplineNumbers[indianNational(d)]
}

// mathsContext, as whole words near a number, makes it the subject of a
// maths question or the result of one: "is 9999999967 prime?", "the answer is
// 9876543210", "13! is 6227020800".
var (
	mathsContext = regexp.MustCompile(`\b(?:` + words(`prime`, `composite`, `divisib(?:le|ility)`, `divided`,
		`divisors?`, `factors?`, `factori[sz](?:e|ed|ation)`, `factorial`, `modulo`, `mod`, `remainder`,
		`quotient`, `gcd`, `hcf`, `lcm`, `multipl(?:y|ied|ies|ying|ication|es?)`, `fibonacci`, `power~of`,
		`squared`, `cubed`, `square~roots?`, `cube~roots?`, `binary`, `hexadecimal`, `octal`, `sum`,
		`product~of`, `equals`, `digits~of`, `palindromes?`, `integers?`, `calculate`, `compute`,
		`evaluate`, `simplify`, `approx`, `approximately`, `population`) +
		`)\b|\bans(?:wer)?[ \t]*(?:[=:]|is\b|was\b)`)
	// mathsResult is a calculation just before a number: "2^33 is", "13! =".
	mathsResult = regexp.MustCompile(`[\d)][ \t]*(?:\^[ \t]*\d+|!|\*\*[ \t]*\d+)[ \t]*(?:is|=|equals)?[ \t]*$`)
	// dataWords, before a run of short groups, make it a set of values: "the
	// mode of 7 9 6 8 ...", "batch sizes 256 512 1024", "scores 65-70-78".
	dataWords = regexp.MustCompile(`\b(?:` + words(`mode`, `mean`, `median`, `average`, `avg`, `range`, `data`,
		`dataset`, `values`, `sequence`, `series`, `list`, `array`, `numbers`, `set`, `digits`, `dice`,
		`rolls`, `scores`, `marks`, `readings`, `samples`, `observations`, `sizes`, `dims`, `dimensions`,
		`shape`, `resolution`, `prices`, `fees`, `salaries`, `cutoffs?`, `percentiles?`) + `)\b`)
	// phoneCue, shortly before a number, says it is one to call.
	phoneCue = regexp.MustCompile(`\b(?:` + words(`call`, `text`, `txt`, `ring`, `phone`, `ph`, `mob`, `mobile`,
		`cell`, `number`, `num`, `no`, `whatsapp`, `dial`, `contact`, `tel`, `sms`) + `)\b`)
)

// classifyNumber decides what a run is, in the context it sits in.
func classifyNumber(t string, run digitRun) (findingKind, bool) {
	if len(run.digits) < 9 || len(run.digits) > 17 {
		return 0, false
	}
	if !standsAlone(t, run) || formattedNumber(run) || dataSet(t, run) || roundRange(run) {
		return 0, false
	}
	d := string(run.digits)
	if dramaNumber(d) || helpline(d) {
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
	switch {
	case indianMobile(d, run.groups, run.seps), nanpPhone(t, run), ukPhone(d, run.groups, run.seps),
		auMobile(d, run.groups, run.seps):
		return kindPhone, true
	case aadhaar(d, run.groups, run.seps):
		return kindAadhaar, true
	case ssn(d, run.groups, run.seps):
		return kindSSN, true
	}
	return 0, false
}

// dataSet reports a run of three or more short groups that a word just before
// it says is a set of values, not a number spaced out to be read.
func dataSet(t string, run digitRun) bool {
	if len(run.groups) < 3 {
		return false
	}
	for _, g := range run.groups {
		if g > 4 {
			return false
		}
	}
	return dataWords.MatchString(windowBefore(t, run.from, 32))
}

// roundRange reports two round amounts set off by a dash, the second the
// larger: "60000-80000 per month" is a salary, not a phone number.
func roundRange(run digitRun) bool {
	if len(run.groups) != 2 || !dashed(run.seps) {
		return false
	}
	a, b := string(run.digits[:run.groups[0]]), string(run.digits[run.groups[0]:])
	if !strings.HasSuffix(a, "00") || !strings.HasSuffix(b, "00") {
		return false
	}
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)
	return errA == nil && errB == nil && y > x
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
// reference.
func standsAlone(t string, run digitRun) bool {
	after := t[run.to:]
	if r, _ := utf8.DecodeRuneInString(after); unicode.IsLetter(r) || unicode.IsDigit(r) {
		return false
	}
	if operandAfter(after) {
		return false
	}
	// "0.9876543210" is a decimal, not a number with a trunk prefix.
	if len(run.groups) > 1 && run.groups[0] == 1 && run.digits[0] == '0' && run.seps[0] == "." {
		return false
	}
	if len(after) > 1 && (after[0] == '.' || after[0] == ',') && isASCIIDigit(after[1]) {
		return false
	}
	before := t[:run.from]
	if operandBefore(before) {
		return false
	}
	// A sign glued to an operand is arithmetic: "2+9876543210", "x-9876543210".
	if !run.plus && len(before) >= 2 && (before[len(before)-1] == '+' || before[len(before)-1] == '-') {
		if r, _ := utf8.DecodeLastRuneInString(before[:len(before)-1]); unicode.IsLetter(r) || unicode.IsDigit(r) || r == ')' {
			return false
		}
	}
	// A spaced sign with an operand before it: "1 + 9876543210", "x - 9876543210";
	// "call me - 9876543210" has a word there, not an operand.
	if s := strings.TrimRight(before, " "); len(s) < len(before) && len(s) >= 2 && (s[len(s)-1] == '+' || s[len(s)-1] == '-') {
		if o := strings.TrimRight(s[:len(s)-1], " "); len(o) < len(s)-1 && len(o) > 0 {
			r, n := utf8.DecodeLastRuneInString(o)
			p, _ := utf8.DecodeLastRuneInString(o[:len(o)-n])
			if unicode.IsDigit(r) || r == ')' || unicode.IsLetter(r) && !unicode.IsLetter(p) {
				return false
			}
		}
	}
	if len(before) >= 2 {
		last, prev := before[len(before)-1], before[len(before)-2]
		// A decimal point or thousands separator glued to a digit before it.
		if (last == '.' || last == ',') && isASCIIDigit(prev) {
			return false
		}
		// A bracket glued to a name: a function's argument or an index,
		// "print(7777777777)", "a[9876543210]".
		if r, _ := utf8.DecodeLastRuneInString(before[:len(before)-1]); (last == '(' || last == '[') &&
			(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == ')' || r == ']') {
			return false
		}
	}
	// An amount: "$9876543210", "₹ 9876543210".
	if r, _ := utf8.DecodeLastRuneInString(strings.TrimRight(before, " ")); strings.ContainsRune("$₹€£¥", r) {
		return false
	}
	if labelled(t, run.from) || quantityWords[firstWordAfter(t, run.to)] {
		return false
	}
	near := windowBefore(t, run.from, 32)
	if mathsResult.MatchString(near) || mathsContext.MatchString(near+" "+windowAfter(t, run.to, 32)) {
		return false
	}
	return true
}

// operandAfter reports an operator after a number with an operand after it:
// "= 2", "/ 2", "% of", "+ 1". An emoticon ("=)", "^^", "<3") or a sarcasm
// tag ("/s") is not one.
func operandAfter(after string) bool {
	s := strings.TrimLeft(after, " ")
	spaced := len(s) < len(after)
	r, size := utf8.DecodeRuneInString(s)
	switch {
	case r == '%':
		return true
	case r == '<' && strings.HasPrefix(s, "<3") && !(len(s) > 2 && isASCIIDigit(s[2])):
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
// "x = 9876543210", "x >= 9876543210", "1/9876543210". A greentext ">" or a
// quote link's ">>" at the start of a line has nothing before it, an arrow
// ("->", "=>") points rather than compares, and a phone cue before an "="
// ("number = 9876543210") is a label.
func operandBefore(before string) bool {
	// An opening bracket after "=" still leaves the number the right-hand side
	// of something: "x = (9876543210 + 1)", "arr = [9876543210, …]".
	s := strings.TrimRight(before, " ([{")
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
	s = strings.TrimRight(s, " ")
	o, _ := utf8.DecodeLastRuneInString(s)
	if !(unicode.IsLetter(o) || unicode.IsDigit(o) || o == ')' || o == ']') {
		return false
	}
	return !phoneCue.MatchString(lastWord(s))
}

// labelled reports a label as the word just before a number, with the fillers
// that go between them skipped: "order 7834567890", "seat number 7012345678",
// "JEE app no. 240310123456", "admit card 9876543210".
func labelled(t string, from int) bool {
	ws := wordsBefore(t, from, 6)
	for k, w := range ws {
		if numberFillers[w] {
			continue
		}
		return numberLabels[w] || w == "card" && k+1 < len(ws) && ws[k+1] == "admit"
	}
	return false
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
// (98-76-54-32-10, 98.76.54.32.10), or every digit spaced out. It turns down
// runs of three or more pairs set off by spaces or line breaks, which is how a
// list of marks or scores reads (87 91 76 88 92).
func mobileGrouping(g []int, seps []string) bool {
	if len(g) == 0 {
		return false
	}
	ones := true
	for _, n := range g {
		ones = ones && n == 1
	}
	if ones {
		return len(g) >= 9
	}
	if dashed(seps) {
		if len(g) > 5 {
			return false
		}
		for _, n := range g {
			if n < 2 {
				return false
			}
		}
		return true
	}
	if len(g) > 5 {
		return false
	}
	pairs := 0
	for _, n := range g {
		switch {
		case n < 2:
			return false
		case n == 2:
			pairs++
		}
	}
	return pairs <= 2
}

// dashed reports that every separator in a run is a dash, spaces allowed
// round it, or that every one is a point on its own.
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

// nanpPhone is a North American number written the North American way: (415)
// 867-5309, 415-867-5309, 415.867.5309, 1 415 867 5309. Ten bare digits are not
// taken as one, and nor are three groups set off by spaces or commas alone
// unless a word before them says it is a number to call ("call 415 867
// 5309"): "batch sizes 256 512 1024" is a list.
func nanpPhone(t string, run digitRun) bool {
	d, g := string(run.digits), run.groups
	switch {
	case len(d) == 10 && len(g) == 3 && g[0] == 3 && g[1] == 3 && g[2] == 4:
		return nanp(d) && (nanpSeparators(run) || phoneCue.MatchString(windowBefore(t, run.from, 32)))
	case len(d) == 11 && d[0] == '1' && len(g) == 4 && g[0] == 1 && g[1] == 3 && g[2] == 3 && g[3] == 4:
		return nanp(d[1:])
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

// nanp checks a ten-digit North American number: area code and exchange
// start 2-9, and 555-0100 to 555-0199, which are reserved for fiction, pass.
func nanp(d string) bool {
	return len(d) == 10 && d[0] >= '2' && d[3] >= '2' && !(d[3:6] == "555" && d[6:8] == "01")
}

// dramaNumber reports the UK numbers Ofcom sets aside for drama, which no one
// can be called on: 07700 900000-900999, 020 7946 0000-0999 and the 01xx 496
// ranges, with or without +44.
func dramaNumber(d string) bool {
	if strings.HasPrefix(d, "0044") {
		d = d[2:]
	}
	if strings.HasPrefix(d, "44") {
		d = "0" + strings.TrimPrefix(d[2:], "0")
	}
	return strings.HasPrefix(d, "07700900") || strings.HasPrefix(d, "02079460") ||
		(len(d) == 11 && strings.HasPrefix(d, "01") && d[4:7] == "496")
}

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

// auMobile is an Australian mobile written the national way, 04 and eight
// more digits: 0412 345 678.
func auMobile(d string, g []int, seps []string) bool {
	return len(d) == 10 && strings.HasPrefix(d, "04") && mobileGrouping(g, seps)
}

// aadhaar is a twelve-digit Aadhaar number, as one block or as 4-4-4, whose
// Verhoeff check digit is right. A run of years (2019 2020 2021) is not one.
func aadhaar(d string, g []int, seps []string) bool {
	if len(d) != 12 || d[0] < '2' {
		return false
	}
	switch {
	case len(g) == 1:
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
	return verhoeffValid(d)
}

// ssn is a US Social Security number written with dashes, in the ranges the
// SSA issues from. The two numbers printed in old advertising pass.
func ssn(d string, g []int, seps []string) bool {
	if len(d) != 9 || len(g) != 3 || g[0] != 3 || g[1] != 2 || g[2] != 4 || seps[0] != "-" || seps[1] != "-" {
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
