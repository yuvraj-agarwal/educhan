package app

import (
	"regexp"
	"slices"
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
// Nothing is sent anywhere, and nothing it reads or matches is logged or kept;
// the only trace of a decision is held_reason on a held post.
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
			if f.anyMatch(supportRules, notAfterANumber) || f.asksForAMethod() {
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
		asks = asks || f.anyMatch(ageQuestionRules, nil)
		cues = cues || f.anyMatch(privateCueRules, nil)
	}
	if asks && cues {
		return holdGrooming
	}
	return ""
}

// quoteSource is the text a block quotes back: the poster's own, from the
// source string, trimmed, and cut to maxMatchLen characters.
func quoteSource(src string, from, to int) string {
	return truncateRunes(strings.TrimSpace(src[from:to]), maxMatchLen)
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
		if f.anyMatch(selfHarmMethodRules, notNegated) {
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
// compatibility form a digit or a Latin letter has folds to fewer: "⑽" is
// "(10)", "ﬃ" is "ffi".
const maxFoldRunes = 4

// invisibleRune reports characters that render as nothing: format characters
// (zero-width spaces and joiners, the soft hyphen, direction marks, tags),
// the other default-ignorable code points (Hangul fillers and the like) and
// variation selectors.
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

// lookalikes maps the Cyrillic and Greek letters most often swapped in for
// Latin ones, in lower case, to the Latin letter they imitate. It is a short
// list on purpose: it only has to cover what a poster can type to dodge a
// word, and it never changes what is stored.
var lookalikes = map[rune]rune{
	'а': 'a', 'в': 'b', 'е': 'e', 'і': 'i', 'ј': 'j', 'к': 'k', 'м': 'm', 'н': 'h',
	'о': 'o', 'п': 'n', 'р': 'p', 'с': 'c', 'т': 't', 'у': 'y', 'х': 'x', 'ѕ': 's',
	'ԁ': 'd', 'ԛ': 'q', 'ԝ': 'w', 'һ': 'h', 'ӏ': 'l', 'ɡ': 'g', 'ı': 'i', 'ɩ': 'i',
	'α': 'a', 'β': 'b', 'ε': 'e', 'ι': 'i', 'κ': 'k', 'ο': 'o', 'ρ': 'p', 'τ': 't',
	'χ': 'x', 'ζ': 'z', 'μ': 'm',
}

// leetLetters is what each leetspeak character stands for in the loose view.
var leetLetters = map[rune]rune{
	'0': 'o', '1': 'i', '3': 'e', '4': 'a', '5': 's', '7': 't', '@': 'a', '$': 's',
}

// spacingRunes are what a poster puts between the letters of a spaced-out
// word: "s n a p", "s.n.a.p", "s-n-a-p", "s_n_a_p", "s*n*a*p".
func spacingRune(r rune) bool {
	switch r {
	case ' ', '.', '-', '_', '*', '|', '~', '+', '·', '•':
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
		if letter, ok := leetLetters[r]; ok && (letterAt(i-1, -1) || letterAt(i+1, 1)) {
			r = letter
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
// own; anyApps adds those that only mean an app after "add me on" and the like.
// Words that are an app only some of the time stay out: "line" ("find me in
// line at the canteen"), "threads", "teams" and "zoom" (school uses them), and
// "phone" or "number", which name no place to talk without the number itself,
// and the number is caught on its own.
var (
	contactApps = words(`snap~chat`, `snapchat`, `snap`, `snp`, `insta~gram`, `insta`, `ig`, `discord`,
		`telegram`, `tele`, `tg`, `wh?ats?~?ap+`, `kik`, `wechat`, `skype`, `viber`, `tik~tok`,
		`twitter`, `facebook`, `fb`, `reddit`, `roblox`)
	anyApps = words(contactApps, `messenger`, `signal`, `sc`, `dc`, `wa`, `wp`, `imo`, `bereal`, `hangouts`,
		`gmail`, `e~?mail`, `steam`, `xbox`, `psn`, `playstation`, `epic`, `fortnite`, `valorant`, `minecraft`)
)

// askRules ask someone to talk on another app, or for their details there.
var askRules = []rule{bothViews(`\b` + words(
	// "add me on snap", "dm me on insta", "hit me up on discord"
	words(`add`, `dm`, `pm`, `msg`, `message`, `text`, `txt`, `call`, `ring`, `ping`, `follow`, `contact`,
		`reach`, `find`, `inbox`, `snap`, `chat~with`, `talk~to`, `hit`)+
		`~(?:me|us)~(?:up~)?`+words(`on`, `at`, `in`, `via`, `thru`, `through`, `over`, `@`)+`~(?:my~)?`+anyApps,
	// "hmu on snap"
	`hmu~`+words(`on`, `at`, `in`, `via`, `over`, `@`)+`~(?:my~)?`+anyApps,
	// "add my snap", "follow my insta"
	words(`add`, `follow`, `dm`, `message`, `msg`, `text`, `check~out`, `sub~to`, `subscribe~to`)+`~my~`+contactApps,
	// "what's your snap", "drop your insta"
	words(`what'?s`, `whats`, `wats`, `wat'?s`, `wht'?s`, `what~is`, `wuts?`, `send`, `give`, `drop`, `share`,
		`post`, `tell`, `dm`)+`~(?:me~)?`+words(`your`, `ur`, `yo`, `ya`, `u'?r`)+`~`+contactApps,
	// "what's your number", "give me your number"
	words(`what'?s`, `whats`, `wats`, `wat'?s`, `wht'?s`, `what~is`, `wuts?`, `send`, `give`, `drop`, `dm`, `text`)+
		`~(?:me~)?`+words(`your`, `ur`, `yo`, `ya`, `u'?r`)+`~(?:(?:phone|mobile|cell|whatsapp|contact|personal)~)?`+
		words(`number`, `no`, `num`, `digits`),
	// "can i get your snap"
	words(`can`, `could`, `may`)+`~i~`+words(`get`, `have`, `add`, `grab`)+`~`+words(`your`, `ur`, `ya`)+`~`+
		words(contactApps, `number`, `no`, `num`, `digits`),
	// "ur snap pls"
	words(`your`, `ur`)+`~`+words(contactApps, `number`)+`~`+words(`pls`, `plz`, `please`, `plox`, `plis`),
	// "join my discord"
	`join~(?:my|our)~`+words(`discord`, `telegram`, `tele`, `tg`, `wh?ats?~?ap+`, `snap`, `insta`, `signal`),
	// "let's talk on discord", "can we move to snap"
	words(`let'?s`, `lets`, `we~can`, `can~we`, `wanna`, `want~to`)+`~`+
		words(`talk`, `chat`, `move`, `continue`, `text`, `go`, `talk~more`, `speak`)+`~`+
		words(`on`, `in`, `over~on`, `over`, `to`, `via`)+`~`+anyApps,
	// "whatsapp me", "kik me"
	words(`wh?ats?~?ap+`, `kik`, `telegram`, `snapchat`)+`~me`,
) + `\b(?:[ \t]*@[ \t]*[\pL\pN._]+)?`)}

// myAppRule is "my snap is ...", whose last word decides it: a username, or
// a word about the app ("my discord is lagging"). notAHandle lists the words
// that make it the second.
// The gap after "my" may be empty, for "mysnap" closed up from "m y s n a p",
// but not an underscore: "my_snap = snap(x)" is code.
var myAppRule = bothViews(`\bmy[^\pL\pN_]*` + contactApps + `\b(?:~` +
	words(`id`, `username`, `user~name`, `handle`, `name`, `acc`, `account`, `tag`, `@`) +
	`)?\s*(?:is\b|[=:\-])\s*(@?[\pL\pN._]+)`)

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
		no yes lol lmao ngl tbh idk rly`)
	m := make(map[string]bool, len(list))
	for _, w := range list {
		m[canon(w)] = true
	}
	return m
}()

// handleRules name an account on an app directly: "insta: x_y_z", "snap
// @ my_name", "insta id x_y_z", "discord: name#1234". The "@", the
// underscores and the digits are the evidence, which the caller reads from
// the source. Each has the same groups: an "@" before the handle, the
// handle, and a Discord discriminator after it.
var (
	appColonHandle = bothViews(`\b` + contactApps + `\b(?:~` +
		words(`id`, `username`, `user~name`, `handle`, `acc`, `account`, `tag`) +
		`)?\s*(?:[:=]|-\s)\s*(@?)([\pL\pN._]{2,30})(#\d{4})?`)
	appAtHandle = bothViews(`\b` + contactApps + `\s*(?:is\s*)?(@)\s*([\pL\pN._]{2,30})(#\d{4})?`)
	// "insta id x_y_z", "snap me x_y_z": a label or "me" with no colon.
	appLabelHandle = bothViews(`\b` + contactApps + `~` +
		words(`id`, `username`, `user~name`, `handle`, `me`) + `\s+(@?)([\pL\pN._]{2,30})(#\d{4})?`)
)

// Email addresses: the usual form, allowing spaces round the "@" and before
// ".com", and a bracketed "[@]" or "[.]"; the form with no top-level domain
// at a mail provider ("name@gmail"); and the spelled-out form ("name at gmail
// dot com", "name [at] gmail [dot] com", "name@gmail dot com"). The
// spelled-out form is ordinary English too ("look at youtube dot com"), so
// spelledEmail asks for more than its shape.
var (
	emailRule = plainOnly(`\b([\pL\pN][\pL\pN._%+\-]{0,63})` + emailAt +
		`([\pL\pN][\pL\pN\-]{0,62}(?:` + emailInnerDot + `[\pL\pN][\pL\pN\-]{0,62})*)(\s?\.\s?|` + bracketedDot + `)` +
		`([a-z0-9]{2,24})\b`)
	providerEmailRule = plainOnly(`\b[\pL\pN][\pL\pN._%+\-]{0,63}(?:@|[(\[{<]\s*@\s*[)\]}>])\s?` +
		words(`gmail`, `googlemail`, `yahoo`, `ymail`, `outlook`, `hotmail`, `icloud`, `protonmail`, `proton`,
			`rediffmail`, `yandex`, `aol`) + `\b`)
	spelledEmailRule = plainOnly(`\b([\pL\pN][\pL\pN._%+\-]{0,63})\s*([(\[{<]\s*(?:at|@)\s*[)\]}>]|\s+at\s+|@)\s*` +
		`([\pL\pN][\pL\pN\-]{0,62}(?:` + spelledDot + `[\pL\pN][\pL\pN\-]{0,62})*)(` + spelledDot + `)([a-z]{2,24})\b`)
)

const (
	emailAt       = `(?:\s?@\s?|\s*[(\[{<]\s*@\s*[)\]}>]\s*)`
	bracketedDot  = `\s*[(\[{<]\s*\.\s*[)\]}>]\s*`
	emailInnerDot = `(?:\.|` + bracketedDot + `)`
	spelledDot    = `(?:\.|\s*[(\[{<]\s*(?:dot|\.)\s*[)\]}>]\s*|\s+dot\s+)`
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

// supportRules are crisis language in the poster's own voice. Second-person
// versions ("kill yourself") are not here: those are encouragement, a hold.
var supportRules = []rule{bothViews(`\b` + words(
	`i~(?:(?:really|just|honestly|kinda|actually|literally|seriously|so)~)*`+words(`want`, `wanna`, `wish`, `need`)+
		`~(?:to~)?`+words(`die`, `be~dead`, `not~be~alive`, `not~exist`, `not~wake~up`, `not~be~here`,
		`disappear`, `end~it~all`, `end~my~life`, `end~it`, `end~myself`, `end~everything`,
		`kill~myself`, `stop~existing`, selfHurt, jumpOff),
	// Wherever the "i" is: "I hate myself and want to self harm".
	words(`want`, `wanna`, `need`)+`~(?:to~)?`+selfHurt,
	`i~(?:'?m|am|m)?~`+words(`going~to`, `gonna`, `gunna`, `about~to`, `ready~to`, `thinking~about`,
		`thinking~of`, `planning~to`, `planning~on`)+`~`+words(`kill(?:ing)?~myself`, `end(?:ing)?~it~all`,
		`end(?:ing)?~my~life`, `end(?:ing)?~myself`, `commit(?:ting)?~suicide`, `hurt(?:ing)?~myself`,
		`cut(?:ting)?~myself`, `overdos(?:e|ing)`, `jump(?:ing)?~off~(?:a|the)~`+words(`bridge`, `building`, `roof`, `cliff`)),
	`i~`+words(`'?ll`, `will`, `wil`, `shall`, `might`, `may`, `could`)+`~(?:just~)?`+
		words(`kill~myself`, `end~it~all`, `end~my~life`, `commit~suicide`),
	words(`kill(?:ing)?`, `unaliv(?:e|ing)`)+`~myself`,
	`kms`,
	`i~`+words(`'?m`, `am`, `m`, `feel`, `feeling`, `felt`, `been`, `'?ve~been`, `have~been`, `get`, `got`)+
		`~(?:(?:so|really|very|kinda|pretty|a~bit|super)~)?suicidal`,
	`suicidal~`+words(`thoughts`, `feelings`, `again`, `ideation`),
	`i~`+words(`don'?t`, `do~not`, `dont`, `dnt`)+`~(?:really~)?want~to~`+
		words(`be~alive`, `live`, `exist`, `be~here~anymore`, `wake~up`),
	`i~(?:'?ve~|have~)?(?:been~)?`+words(`cutting`, `cut`, `burning`, `burned`, `burnt`, `hurting`,
		`harming`, `starving`)+`~myself`,
	`i~(?:'?ve~been~|have~been~|keep~|still~)?self~harm(?:ing)?`,
	`feel~like~`+words(`hurting`, `cutting`, `harming`, `killing`, `starving`, `burning`)+`~myself`,
	words(`thinking`, `think`)+`~`+words(`about`, `of`)+`~`+words(
		words(`hurting`, `cutting`, `harming`, `killing`, `starving`, `burning`)+`~myself`,
		`ending~`+words(`it~all`, `it`, `my~life`, `everything`, `things`, `myself`)),
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
) + `\b`)}

// selfHarmRules are encouragement to hurt or kill oneself, and pro-eating-
// disorder content. They are held, through selfHarmCheck.
var selfHarmRules = []rule{bothViews(`\b` + words(
	`kys`,
	`kill~`+words(`your`, `ur`, `yo`)+`~?sel(?:f|ves)`, `kill~urself`, `kill~yourselves`,
	`unalive~`+words(`your`, `ur`)+`~?self`,
	words(`go`, `pls`, `please`, `plz`, `why~don'?t~you`, `why~dont~you`, `you~should`, `u~should`,
		`you~shud`, `u~shud`, `you~need~to`, `u~need~to`, `you~deserve~to`, `u~deserve~to`, `you~can~go`,
		`u~can~go`, `you~ought~to`, `you~must`)+`~(?:just~)?`+
		words(`die`, `kill~(?:your|ur)~?self`, `hang~(?:your|ur)~?self`, `cut~(?:your|ur)~?self`,
			`end~it`, `end~your~life`, `starve~(?:your|ur)~?self`, jumpOff),
	words(`cut`, `hang`, `starve`, `neck`, `rope`, `end`)+`~`+words(`your`, `ur`)+`~?sel(?:f|ves)`,
	jumpOff,
	`slit~`+words(`your`, `ur`)+`~wrists?`,
	`drink~bleach`,
	words(`nobody`, `no~one`, `noone`)+`~`+words(`would`, `will`, `'?d`, `is~gonna`, `is~going~to`)+
		`~(?:even~)?`+words(`miss`, `care~if`)+`~`+words(`you`, `u`),
	`(?:the~)?world~`+words(`would`, `will`)+`~be~better~(?:off~)?without~`+words(`you`, `u`),
	// Pro-eating-disorder content.
	`thinspo`, `thinspiration`, `meanspo`, `bonespo`, `skinnyspo`, `pro~?ana`, `pro~?mia`,
	words(`ana`, `mia`)+`~`+words(`coach`, `buddy`, `tips?`),
) + `\b`)}

// selfHarmMethodRules ask or tell how to hurt or kill oneself: how, how many,
// what dose. Someone asking may be asking for themselves, so a post with one
// is answered with the support message first, as crisis language is, and is
// held once the poster has seen it.
var selfHarmMethodRules = []rule{bothViews(`\b` + words(
	`how~`+words(`do`, `can`, `could`, `should`, `would`, `does`, `did`)+`~`+
		words(`i`, `you`, `u`, `one`, `someone`, `a~person`, `people`)+`~`+methods,
	`how~to~`+words(methods, `cut~without~`+words(`anyone`, `people`, `parents`)+`~`+
		words(`knowing`, `noticing`, `seeing`), `hide~(?:my~)?`+words(`cuts`, `scars`, `self~harm`)),
	`how~`+words(`many`, `much`)+`~[^.?!\n]{0,60}?~`+words(`to~die`, `to~kill~`+words(`my`, `your`)+`~?self`,
		`to~overdose`, `to~od`, `overdose`, `be~fatal`, `be~lethal`, `be~deadly`, `is~fatal`, `is~lethal`,
		`is~deadly`, `would~kill~`+words(`me`, `you`, `someone`, `a~person`), `to~end~it`, `to~end~my~life`),
	// "how many sleeping pills are lethal": "are" only with a medicine named,
	// since "how many snakes are deadly" is a biology question.
	`how~many~[^.?!\n]{0,40}?`+medicines+`[^.?!\n]{0,20}?~are~`+
		words(`lethal`, `fatal`, `deadly`, `enough~to~`+words(`die`, `kill~`+words(`me`, `you`, `someone`), `od`, `overdose`)),
	`what~`+words(`dose`, `amount`, `quantity`, `dosage`)+`~[^.?!\n]{0,40}?~`+
		words(`lethal`, `fatal`, `deadly`, `kill~`+words(`me`, `you`, `myself`, `yourself`), `to~die`,
			`overdose`, `od`),
	words(`lethal`, `fatal`, `deadly`)+`~`+words(`dose`, `doses`, `dosage`, `amount`, `quantity`),
	words(`painless`, `easiest`, `easy`, `quickest`, `quick`, `best`, `fastest`, `surest`, `least~painful`)+
		`~`+words(`way`, `ways`, `method`, `methods`)+`~(?:to~)?`+
		words(`die`, `kill~`+words(`my`, `your`, `ur`, `one`)+`~?self`, `commit~suicide`,
			`end~`+words(`my`, `your`, `one'?s`)+`~life`, `end~it`, `od`, `overdose`),
	`suicide~`+words(`method`, `methods`, `guide`, `instructions`, `tips`),
) + `\b`)}

// medicines are what an overdose question names.
var medicines = words(`pills?`, `tablets?`, `capsules?`, `paracetamol`, `tylenol`, `acetaminophen`, `ibuprofen`,
	`aspirin`, `advil`, `panadol`, `crocin`, `dolo`, `meds`, `medicines?`, `medications?`, `antidepressants?`,
	`sleeping~pills`)

// methods are the ways of hurting or killing oneself a how-question asks for.
var methods = words(
	`kill~`+words(`my`, `your`, `ur`, `them`, `one`)+`~?sel(?:f|ves)`,
	`commit~suicide`,
	`end~`+words(`my`, `your`, `their`, `a`, `one'?s`)+`~life`,
	`overdose`, `od`,
	`hang~`+words(`my`, `your`, `ur`, `them`, `one`)+`~?sel(?:f|ves)`,
	`die~`+words(`painlessly`, `quickly`, `fast`, `peacefully`, `without~pain`, `in~`+words(`my`, `your`, `their`)+`~sleep`),
	`cut~`+words(`deeper`, words(`my`, `your`)+`~wrists?`),
	`starve~`+words(`my`, `your`)+`~?self`,
	words(`tie`, `make`)+`~(?:a~)?noose`,
)

// Threats: an intent to hurt people or attack a place, held only when a place
// or a time comes with it ("at school", "tomorrow", "in 3rd period") — which an
// intent to attack a school always does, since it names one; the warning
// "don't come to school tomorrow" with a cue that it is one ("you've been
// warned"); and "school shooting" with a time still to come. Only the first
// person and the future count: a history question, or news that reports an
// attack ("he threatened to shoot up his school"), names no intent of the
// poster's, so it passes.
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
		`playground`, `stadium`, `station`, `airport`, `hospital`, `concert`, `prom`, `graduation`)
	threatTimes = words(`today`, `tonight`, `tomorrow`, `tmrw`, `tmr`, `tmrrw`, `tomoz`, `2moro`, `2morrow`,
		words(`on`, `this`, `next`, `coming`)+`~`+words(`mon`, `tues`, `wednes`, `thurs`, `fri`, `satur`, `sun`)+`day`,
		words(`mon`, `tues`, `wednes`, `thurs`, `fri`, `satur`, `sun`)+`day`,
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
		`in~\d+~`+words(`mins?`, `minutes`, `hours?`, `hrs?`, `days?`))

	threatModals = words(`will`, `'?ll`, `wil`, `shall`, `wanna`, `want~to`, `gonna`, `gunna`, `going~to`,
		`planning~to`, `planning~on`, `plan~to`, `plan~on`, `intend~to`, `about~to`, `ready~to`, `finna`, `gotta`,
		`bout~to`)
	threatAdverbs = `(?:(?:really|totally|seriously|definitely|actually|literally|just|finally)~)?`

	threatIntentRules = []rule{
		bothViews(`\b` + words(
			// "i will", "we're gonna", "imma": then what.
			words(`(?:i|we)~(?:(?:'?m|am|are|'?re|r)~)?`+threatAdverbs+threatModals, `imma`, `ima`, `i~'?ma`)+
				`~`+threatAdverbs+threatActs(false),
			// "i'm bringing", "we're shooting up", "i'll be shooting".
			`(?:i|we)~`+words(`'?m`, `am`, `are`, `'?re`, `r`, words(`'?ll`, `will`, `wil`)+`~be`)+`~`+
				threatAdverbs+threatActs(true),
			// "tomorrow i shoot up the school": the present, with the strongest
			// acts only, since "we kill it at the fest" is how people talk.
			`(?:i|we)~`+threatAdverbs+threatStrongActs,
			words(`you`, `u`, `y'?all`, `yall`, `you~all`, `everyone`, `everybody`, `all~of~you`)+
				`~(?:`+words(`'?re`, `are`, `r`, `is`, `will`, `'?ll`)+`~)?(?:all~)?`+words(`going~to`, `gonna`, `gunna`, `will`)+
				`~`+words(`die`, `get~`+words(`shot`, `stabbed`, `hurt`, `killed`, `blown~up`)),
			`there~`+words(`will`, `'?ll`, `is~going~to`, `'?s~gonna`, `is~gonna`, `s~gonna`, `might`)+`~be~(?:a~)?`+
				words(`shooting`, `bombing`, `massacre`, `bloodbath`, `blood~bath`, `bomb`, `attack`, `school~shooting`),
		) + `\b`),
		// With no subject, at the start of a sentence or line: "gonna shoot up
		// the school tmrw", "shooting up the school tomorrow, don't come".
		bothViews(`(?:^|[.!?;:\n])[^\pL\pN]*` + words(
			words(`gonna`, `gunna`, `going~to`, `finna`, `bout~to`, `about~to`)+`~`+threatAdverbs+threatActs(false),
			threatStrongProgressive,
		) + `\b`),
	}
	threatPlaceOrTimeRules = []rule{bothViews(`\b` + words(threatPlaces, threatTimes) + `\b`)}

	// "school shooting tomorrow at 10", with only a time still to come, close.
	schoolShootingRules = []rule{bothViews(`\bschool~shooting\b`)}
	futureTimeRules     = []rule{bothViews(`\b` + futureTimes + `\b`)}

	// "Don't come to school tomorrow" is far more often about a holiday than a
	// warning, so it counts only beside a cue that it is one.
	threatWarningRules = []rule{bothViews(`\b` + words(`don'?t`, `dont`, `do~not`) + `~` + words(`come`, `go`) +
		`~(?:in~)?(?:to~)?` + words(`school`, `class`, `college`, `campus`, `uni`) + `~` + threatTimes + `\b`)}
	threatWarningCues = []rule{bothViews(`\b` + words(
		words(`you'?ve`, `u'?ve`, `you~have`, `y'?all~have`, `yall~have`, `you~all~have`)+`~been~warned`,
		`consider~`+words(`this`, `yourself`, `yourselves`, `urself`, `yourselfs`)+`~warned`,
		`i'?m~warning~`+words(`you`, `u`, `everyone`, `everybody`, `y'?all`, `yall`),
		`this~is~(?:`+words(`a`, `my`, `your`, `ur`)+`~)?(?:`+words(`last`, `final`, `only`, `first`)+`~)?warning`,
		`if~`+words(`you`, `u`)+`~`+words(`want~to`, `wanna`, `wish~to`)+`~`+words(`live`, `stay~alive`, `be~safe`, `survive`),
		words(`you'?ll`, `u'?ll`, `you~will`, `y'?all~will`)+`~see`,
		`something~`+words(`big`, `bad`, `terrible`, `crazy`, `huge`)+`~`+
			words(`is~going~to`, `will`, `is~gonna`, `gonna`, `'?s~gonna`, `'?s~going~to`)+`~happen`,
	) + `\b`)}
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
	return words(
		words(v(`shoot(?:~up)?`, `shooting(?:~up)?`), v(`stab`, `stabbing`), v(`kill`, `killing`),
			v(`murder`, `murdering`), v(`massacre`, `massacring`), v(`slaughter`, `slaughtering`),
			v(`gun~down`, `gunning~down`), v(`knife`, `knifing`), v(`attack`, `attacking`), v(`hurt`, `hurting`),
			v(`beat~up`, `beating~up`), v(`behead`, `beheading`), v(`shank`, `shanking`))+`~`+threatPeople,
		words(v(`shoot~up`, `shooting~up`), v(`shoot~(?:at|in|inside)`, `shooting~(?:at|in|inside)`),
			v(`bomb`, `bombing`), v(`blow~up`, `blowing~up`), v(`burn~down`, `burning~down`), v(`torch`, `torching`),
			v(`set~fire~to`, `setting~fire~to`), v(`attack`, `attacking`))+
			`~(?:`+words(`the`, `my`, `our`, `this`, `that`, `ur`, `your`)+`~)?`+threatPlaces,
		v(`shoot`, `shooting`)+`~(?:the~)?`+words(`place`, `school`, `it`)+`~up`, v(`shoot~up`, `shooting~up`),
		v(`bring`, `bringing`)+`~`+weapon+`~`+words(`to`, `into`, `in`)+`~(?:`+words(`the`, `my`, `our`)+`~)?`+threatPlaces,
		words(v(`plant`, `planting`), v(`set~off`, `setting~off`), v(`detonate`, `detonating`))+
			`~(?:`+words(`a`, `the`)+`~)?`+words(`bomb`, `explosives?`, `device`),
	)
}

// weapon is what a threat says will be brought: "a gun", "my dad's gun".
var weapon = `(?:` + words(`a`, `my`, `the`, `an`, `my~dad'?s`, `my~brother'?s`, `his`) + `~)?` +
	words(`guns?`, `knife`, `knives`, `bomb`, `weapons?`, `rifle`, `pistol`, `explosives?`, `ak`, `ar`)

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

// eventWords, straight after a place, make it an event rather than a place: "I
// will bomb the class presentation" is about failing it. messageWords, after
// "shoot you", make it a message: "I'll shoot you an email tomorrow".
var (
	eventWords = canonSetOf(`presentation presentations test tests exam exams quiz play photo photos photoshoot project
		assignment report trip reunion party fest festival function shoot paper essay homework notes match game
		performance debate competition`)
	messageWords = canonSetOf(`message msg text txt email mail dm pm note line pic photo picture video link reply
		invite snap call ping`)
)

// threatCheck turns down the commonest harmless readings of an intent match:
// an event straight after it ("bomb the class presentation"), a message after
// "shoot" ("shoot you an email"), a shoot that is for a film or the yearbook,
// a game ("we're gonna kill them at the match"), and a threat quoted in a
// report of one ("'I will shoot up the school', court hears").
func threatCheck(v *view, m []int) bool {
	text := v.text
	matched := text[m[0]:m[1]]
	next := nextWords(text[m[1]:min(len(text), m[1]+64)], 6)
	if len(next) > 0 && eventWords[next[0]] {
		return false
	}
	around := canon(text[max(0, m[0]-100):min(len(text), m[1]+100)])
	if strings.Contains(matched, "shoot") && !shootUp.MatchString(canon(matched)) {
		i := 0
		if i < len(next) && (next[i] == "guys" || next[i] == "aii" || next[i] == "all" || next[i] == "both") {
			i++
		}
		if i < len(next) && (next[i] == "a" || next[i] == "an" || next[i] == "some" || next[i] == "my" || next[i] == "the") {
			i++
		}
		if i < len(next) && messageWords[next[i]] {
			return false
		}
		if filmPurpose.MatchString(around) {
			return false
		}
		// "shooting at the gym tonight for basketball practice".
		if shootAtPlace.MatchString(canon(matched)) {
			for _, w := range next {
				if gameWords[w] || shootingSports[w] {
					return false
				}
			}
		}
	}
	if gameVerb.MatchString(canon(matched)) {
		for _, w := range next {
			if gameWords[w] {
				return false
			}
			if w == "school" || w == "class" {
				break
			}
		}
	}
	if quoted(text, m[0]) {
		for _, w := range strings.FieldsFunc(around, func(r rune) bool { return !unicode.IsLetter(r) }) {
			if reportWords[w] {
				return false
			}
		}
	}
	return true
}

var (
	// shootUp is "shoot up" and "shooting up", which is never a film shoot.
	shootUp = regexp.MustCompile(`shoot(?:ing)?[^\pL\pN]*up\b`)
	// shootAtPlace is shooting at or in a place, rather than at people,
	// which a sport does as well.
	shootAtPlace   = regexp.MustCompile(`shoot(?:ing)?[^\pL\pN]*(?:at|in|inside)\b`)
	shootingSports = canonSetOf(`practice training hoops baskets goals penalties archery arrows range nets`)
	// filmPurpose says what a shoot is for: "for our film project", "for the
	// yearbook". Compiled through canon, and matched against canon'd text, so
	// it reads either view.
	filmPurpose = regexp.MustCompile(canon(`\bfor[^\pL\pN]+(?:\pL+[^\pL\pN]+){0,3}?` + words(`film`, `films`,
		`movie`, `video`, `videos`, `vlog`, `reel`, `reels`, `yearbook`, `documentary`, `photoshoot`, `photos?`,
		`pictures`, `pics`, `portraits`, `headshots`, `trailer`, `ad`, `advert`, `commercial`, `short`, `tiktok`,
		`youtube`, `channel`, `project`, `magazine`, `newsletter`) + `\b`))
	// gameVerb is the verbs people beat each other with at a game.
	gameVerb = regexp.MustCompile(canon(`\b` + words(`kill`, `killing`, `murder`, `murdering`, `slaughter`,
		`slaughtering`, `massacre`, `massacring`, `destroy`, `destroying`, `beat`, `beating`) + `\b`))
	gameWords = canonSetOf(`match matches game games chess football soccer cricket basketball volleyball hockey
		tennis badminton kabaddi tournament finals final semis semifinal semifinals quiz debate competition race
		fifa fortnite valorant pubg bgmi minecraft roblox cod league olympiad hackathon contest sports carrom
		ludo uno mock`)
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

// threatWindow is how close, in bytes of the post, a place or a time has to
// be to an intent to count as part of the same threat.
const threatWindow = 200

// Grooming cues: a question about age, school year or where someone lives or
// studies, together with a private-contact or meet-up cue, anywhere in one
// post. Either half alone is ordinary conversation.
var (
	ageQuestionRules = []rule{bothViews(`\b` + words(
		`how~old~(?:`+words(`are`, `r`, `is`)+`~)?`+words(`you`, `u`, `ya`, `yu`),
		`what'?s?~(?:is~)?`+words(`your`, `ur`, `yr`)+`~age`,
		`a~s~l`,
		`what~`+words(`year`, `grade`, `class`, `standard`, `std`, `form`)+`~`+words(`are`, `r`)+`~`+words(`you`, `u`)+`~in`,
		`which~`+words(`year`, `grade`, `class`, `standard`, `std`, `form`, `school`, `college`)+`~`+
			words(`are`, `r`, `do`)+`~`+words(`you`, `u`),
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~(?:in~)?`+words(`high~school`, `middle~school`,
			`secondary~school`, `year~\d{1,2}`, `grade~\d{1,2}`, `class~\d{1,2}`, `std~\d{1,2}`,
			`\d{1,2}(?:st|nd|rd|th)~`+words(`grade`, `class`, `standard`, `std`)),
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~`+words(`under`, `over`, `below`, `above`)+`~1\d`,
		words(`are`, `r`)+`~`+words(`you`, `u`)+`~1\d`,
		`where~`+words(`do`, `d`)+`~`+words(`you`, `u`)+`~`+words(`live`, `stay`, `go~to~school`, `study`),
		`where~`+words(`are`, `r`)+`~`+words(`you`, `u`)+`~from`,
		`where~`+words(`is`, `'?s`)+`~`+words(`your`, `ur`)+`~`+words(`school`, `house`, `home`),
		words(`what`, `which`)+`~`+words(`school`, `college`, `city`, `town`, `area`, `place`)+`~`+
			words(`do`, `d`, `are`, `r`)+`~`+words(`you`, `u`),
		`what'?s?~(?:is~)?`+words(`your`, `ur`)+`~`+words(`school`, `address`, `location`, `city`, `town`, `area`,
			`postcode`, `pincode`, `zip`),
		words(`do`, `d`)+`~`+words(`you`, `u`)+`~live~`+words(`near`, `in`, `around`, `close`),
	) + `\b`),
		// Questions that end in their "?", where no word boundary follows:
		// "your age?", "u 14?", and "age?" opening a sentence.
		bothViews(words(
			`\b`+words(`your`, `ur`)+`~age[^\pL\pN?]*\?`,
			`\b`+words(`you`, `u`)+`~1\d[^\pL\pN?]*\?`,
			`(?:^|[.!?\n,;:])[^\pL\pN]*age[^\pL\pN?]*\?`,
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
		`hmu`, `come~over`,
	) + `\b`)}
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
	pair := func(first, second []rule, check matchCheck, w int) bool {
		var a, b [][2]int
		for _, f := range fields {
			a = append(a, f.spansFrom(first, check)...)
		}
		if len(a) == 0 {
			return false
		}
		for _, f := range fields {
			b = append(b, f.spansFrom(second, nil)...)
		}
		return near(a, b, w)
	}
	return pair(threatIntentRules, threatPlaceOrTimeRules, threatCheck, threatWindow) ||
		pair(threatWarningRules, threatWarningCues, nil, threatWindow) ||
		pair(schoolShootingRules, futureTimeRules, nil, schoolShootingWindow)
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

// notAfterANumber turns down "kms" written as a distance: "5 kms", "10kms".
func notAfterANumber(v *view, m []int) bool {
	text := v.text
	if !strings.HasSuffix(text[m[0]:m[1]], "kms") {
		return true
	}
	before := strings.TrimRight(text[:m[1]-3], " ")
	return before == "" || !isASCIIDigit(before[len(before)-1])
}

// notNegated turns down encouragement that is the opposite: "don't cut
// yourself on the glass", "be careful not to kill yourself", "never drink
// bleach", "please don't kys". The negation has to come straight before the
// words matched, in the same clause — "don't" or "never", with at most "you",
// "ever", "even", "dare", "go" or "just" between — or "careful" has to come
// earlier in the same sentence. A negation further back is about something
// else: "i don't care, kys", "dont reply. just kill yourself", "never mind,
// kys".
func notNegated(v *view, m []int) bool {
	text := v.text
	before := text[max(0, m[0]-64):m[0]]
	sentence := before[strings.LastIndexAny(before, ".!?\n")+1:]
	if strings.Contains(sentence, "careful") || strings.Contains(sentence, canon("careful")) {
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
// come straight before it; negationFillers may come between.
var (
	negations = []string{"don't", "dont", "do not", "never", "not to", "didn't", "didnt", "shouldn't",
		"shouldnt", "won't", "wont", "mustn't"}
	negationFillers = canonSetOf(`you u ever even dare go just please pls`)
)

// selfHarmCheck is notNegated with the two places a self-harm phrase is
// something else: "rope yourself in" (to a project), and "jump off a bridge"
// said of the poster themselves ("i'm going to jump off a bridge"), which is
// crisis language and has the support rules, not encouragement.
func selfHarmCheck(v *view, m []int) bool {
	if !notNegated(v, m) {
		return false
	}
	text := v.text
	matched := text[m[0]:m[1]]
	switch {
	case strings.HasPrefix(matched, "rope"):
		next := nextWords(text[m[1]:min(len(text), m[1]+16)], 1)
		return len(next) == 0 || (next[0] != "in" && next[0] != "into")
	case strings.HasPrefix(matched, "jump"):
		before := text[max(0, m[0]-48):m[0]]
		clause := before[strings.LastIndexAny(before, ".!?,;:\n")+1:]
		for _, w := range strings.FieldsFunc(clause, func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' }) {
			if firstPerson[w] {
				return false
			}
		}
	}
	return true
}

var firstPerson = canonSetOf(`i i'm im i'll i'd i've me my myself we we're`)

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
	from, to, ok = f.first(providerEmailRule, nil)
	consider(from, to, ok, kindEmail)
	from, to, ok = f.first(spelledEmailRule, func(v *view, m []int) bool {
		text := v.text
		local, at, domain, tld := text[m[2]:m[3]], text[m[4]:m[5]], text[m[6]:m[7]], text[m[10]:m[11]]
		return spelledEmail(local, at, spelledDomain(domain), tld)
	})
	consider(from, to, ok, kindEmail)

	from, to, ok = f.first(linkRule, func(v *view, m []int) bool {
		return contactLink(v.text[m[2]:m[3]], v.text[m[4]:m[5]])
	})
	consider(from, to, ok, kindLink)
	from, to, ok = f.first(spacedInviteRule, nil)
	consider(from, to, ok, kindLink)

	for _, r := range askRules {
		from, to, ok = f.first(r, nil)
		consider(from, to, ok, kindAsk)
	}
	from, to, ok = f.first(myAppRule, func(v *view, m []int) bool {
		word := v.text[m[2]:m[3]]
		return strings.HasPrefix(word, "@") || !notAHandle[canon(word)]
	})
	consider(from, to, ok, kindHandle)
	// The handle rules find the app's name in either view, so "s.n.a.p" and
	// "sn4pch4t" count, and judge the handle from the source, where its
	// underscores, dots and digits are as the poster wrote them.
	for _, r := range []rule{appColonHandle, appAtHandle, appLabelHandle} {
		from, to, ok = f.first(r, func(v *view, m []int) bool {
			at := m[2] >= 0 && m[3] > m[2]
			discriminator := m[6] >= 0 && m[7] > m[6]
			a, b := v.span(m[4], m[5])
			return plausibleHandle(f.src[a:b], at || discriminator)
		})
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

// plausibleHandle accepts what looks like a username rather than a word: it
// has a letter, and an "@" before it or a digit, dot or underscore in it.
func plausibleHandle(handle string, at bool) bool {
	letter, marked := false, at
	for _, r := range handle {
		switch {
		case unicode.IsLetter(r):
			letter = true
		case r == '_' || r == '.' || unicode.IsDigit(r):
			marked = true
		}
	}
	return letter && marked
}

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

// spelledEmail vets a spelled-out address. Besides the shape, it needs one sign
// that it is an address and not a sentence: a bracketed "at", a local part
// with a dot, an underscore or a digit in it, a mail provider's domain, or an
// institution's.
func spelledEmail(local, at, domain, tld string) bool {
	if !emailAddress(local, domain, tld, false) {
		return false
	}
	label, _, _ := strings.Cut(domain, ".")
	return strings.ContainsAny(at, "@([{<") ||
		strings.ContainsAny(local, "._0123456789") ||
		mailProviders[label] ||
		tld == "edu" || strings.HasSuffix(domain, ".ac") || strings.HasSuffix(domain, ".edu")
}

var mailProviders = setOf(`gmail googlemail yahoo ymail rocketmail outlook hotmail live msn icloud mac aol
	proton protonmail pm tutanota tuta zoho rediffmail rediff yandex mail gmx qq 163 126 fastmail`)

// spelledDomain turns "gmail (dot) co dot in" back into "gmail.co.in".
func spelledDomain(domain string) string {
	return spelledDotPattern.ReplaceAllString(domain, ".")
}

var spelledDotPattern = regexp.MustCompile(`\s*[(\[{<]\s*(?:dot|\.)\s*[)\]}>]\s*|\s+dot\s+`)

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

// mathsBefore and mathsAfter are the characters that make a number one side
// of a calculation rather than something to dial.
const (
	mathsBefore = "=≈≠≤≥<>×*÷/^±~≡"
	mathsAfter  = "=≈≠≤≥<>×*÷/^%"
)

// numberLabels, just before a number, say what kind of number it is.
var numberLabels = []string{"isbn", "issn", "doi", "arxiv", "pnr", "utr", "roll", "order", "invoice",
	"transaction", "txn", "ticket", "serial", "application", "app no", "app. no", "form no", "registration",
	"reg no", "reg. no", "enrol", "account", "a/c", "policy", "jee", "neet", "cuet", "admit card", "seat no",
	"seat number", "hall ticket", "rank", "imei", "tracking", "awb", "reference", "ref no", "ref. no"}

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

// helplineNumbers are crisis lines whose numbers look like phone numbers to the
// rules, so that a reader passing one on to someone who needs it is not told
// to take it out. Each is the ten-digit national number: in India iCall, the
// Vandrevala Foundation and AASRA; in the US the old National Suicide
// Prevention Lifeline number (it still connects to 988), the Trevor Project,
// the National Domestic Violence Hotline and Childhelp.
var helplineNumbers = setOf(`9152987821 9999666555 9820466726 8002738255 8664887386 8007997233 8004224453`)

// helpline reports a run of digits that is one of helplineNumbers, with or
// without the country code or trunk prefix it is dialled with.
func helpline(d string) bool {
	if len(d) == 11 && d[0] == '1' { // a North American number with its 1
		d = d[1:]
	}
	return helplineNumbers[indianNational(d)]
}

// mathsWords, near a number, make it the subject of a maths question:
// "is 9999999967 prime?".
var mathsWords = []string{"prime", "divisib", "divided", "factor", "modulo", " mod ", "remainder", "quotient",
	"gcd", "lcm", "multipl", "fibonacci", "power of", "squared", "cubed", "square root", "binary", "hexadecimal",
	" sum ", "sum of", "product of", "equals"}

// classifyNumber decides what a run is, in the context it sits in.
func classifyNumber(t string, run digitRun) (findingKind, bool) {
	if len(run.digits) < 9 || len(run.digits) > 17 {
		return 0, false
	}
	if !standsAlone(t, run) {
		return 0, false
	}
	if formattedNumber(run) {
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
	case indianMobile(d, run.groups, run.seps), nanpPhone(d, run.groups), ukPhone(d, run.groups, run.seps),
		auMobile(d, run.groups, run.seps):
		return kindPhone, true
	case aadhaar(d, run.groups, run.seps):
		return kindAadhaar, true
	case ssn(d, run.groups, run.seps):
		return kindSSN, true
	}
	return 0, false
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
// of a sum, a decimal, a measurement or a labelled reference.
func standsAlone(t string, run digitRun) bool {
	after := t[run.to:]
	if r, _ := utf8.DecodeRuneInString(after); unicode.IsLetter(r) || unicode.IsDigit(r) {
		return false
	}
	if r, _ := utf8.DecodeRuneInString(strings.TrimLeft(after, " ")); strings.ContainsRune(mathsAfter, r) {
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
	// An opening bracket after "=" still leaves the number the right-hand side
	// of something: "x = (9876543210 + 1)", "arr = [9876543210, …]".
	trimmed := strings.TrimRight(before, " ([{")
	if r, _ := utf8.DecodeLastRuneInString(trimmed); strings.ContainsRune(mathsBefore, r) {
		return false
	}
	// A sign glued to an operand is arithmetic: "2+9876543210", "x-9876543210".
	if !run.plus && len(before) >= 2 && (before[len(before)-1] == '+' || before[len(before)-1] == '-') {
		if r, _ := utf8.DecodeLastRuneInString(before[:len(before)-1]); unicode.IsLetter(r) || unicode.IsDigit(r) || r == ')' {
			return false
		}
	}
	// A decimal point or thousands separator glued to a digit before it.
	if len(before) >= 2 && (before[len(before)-1] == '.' || before[len(before)-1] == ',') && isASCIIDigit(before[len(before)-2]) {
		return false
	}
	window := t[max(0, run.from-24):run.from]
	for _, label := range numberLabels {
		if strings.Contains(window, label) {
			return false
		}
	}
	around := window + " " + t[run.to:min(len(t), run.to+24)]
	for _, word := range mathsWords {
		if strings.Contains(around, word) {
			return false
		}
	}
	return true
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
// be dialled: all together, in threes to fives (98765 43210, 987-654-3210,
// 98 7654 3210), in pairs or longer set off by dashes (98-76-54-32-10), or
// every digit spaced out. It turns down runs of pairs set off by spaces or
// line breaks, which is how a list of marks or scores reads (87 91 76 88 92).
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
	if len(g) > 4 {
		return false
	}
	for i, n := range g {
		if n < 3 && !(i == 0 && n == 2) {
			return false
		}
	}
	return true
}

// dashed reports that every separator in a run is a dash, spaces allowed
// round it.
func dashed(seps []string) bool {
	for _, s := range seps {
		if strings.Trim(s, " ") != "-" {
			return false
		}
	}
	return len(seps) > 0
}

// nanpPhone is a North American number written the North American way: (415)
// 867-5309, 415-867-5309, 1 415 867 5309. Ten bare digits are not taken as
// one; too many other numbers have ten digits.
func nanpPhone(d string, g []int) bool {
	switch {
	case len(d) == 10 && len(g) == 3 && g[0] == 3 && g[1] == 3 && g[2] == 4:
		return nanp(d)
	case len(d) == 11 && d[0] == '1' && len(g) == 4 && g[0] == 1 && g[1] == 3 && g[2] == 3 && g[3] == 4:
		return nanp(d[1:])
	}
	return false
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
	d = strings.TrimPrefix(d, "0044")
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
