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
		fields = append(fields, scanField(fieldBody, in.Body, mathSpans(in.Body)))
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
			if f.anyMatch(supportRules, notAfterANumber) {
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
	for _, f := range fields {
		if f.threatens() {
			return holdThreat
		}
	}
	for _, f := range fields {
		if f.anyMatch(selfHarmRules, notNegated) {
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

// mathSpans lists the [math] and [eqn] blocks of a body, tags included. The
// filter skips them: TeX has a use for every character, and long numbers in
// equations are the commonest false positive a phone rule has. The pairing
// is markupBlocks', so what is skipped is exactly what the reader sees
// typeset. [code] is scanned like any other text.
func mathSpans(body string) [][2]int {
	var spans [][2]int
	for _, blk := range markupBlocks(body) {
		if blk.kind == blockMath || blk.kind == blockEqn {
			spans = append(spans, [2]int{blk.start, blk.end})
		}
	}
	return spans
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
// ones), lower case, accents and other combining marks dropped, zero-width
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
}

func scanField(field, src string, skip [][2]int) *scannedField {
	f := &scannedField{field: field, src: src}
	f.plain = plainView(src, skip)
	f.loose = looseView(&f.plain)
	f.looseOnly = f.loose.text == canon(f.plain.text)
	return f
}

func plainView(src string, skip [][2]int) view {
	b := newViewBuilder(len(src))
	k := 0
	for i := 0; i < len(src); {
		for k < len(skip) && skip[k][1] <= i {
			k++
		}
		if k < len(skip) && skip[k][0] <= i {
			// A blank stands in for the block, so the words either side of
			// it cannot run together into one.
			b.add(' ', skip[k][0], skip[k][1])
			i = skip[k][1]
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
	for _, d := range norm.NFKD.String(string(r)) {
		switch {
		case unicode.Is(unicode.Mn, d) || invisibleRune(d):
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
var myAppRule = bothViews(`\bmy~` + contactApps + `\b(?:~` +
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

// handleRules name an account on an app directly: "insta: x_y_z",
// "snap @ my_name". The "@" and the underscores are the evidence, so these run
// on the plain view, where both survive.
var (
	appColonHandle = plainOnly(`\b` + contactApps + `\b(?:~` +
		words(`id`, `username`, `user~name`, `handle`, `acc`, `account`, `tag`) +
		`)?\s*(?:[:=]|-\s)\s*(@?)([\pL\pN._]{2,30})`)
	appAtHandle = plainOnly(`\b` + contactApps + `\s*(?:is\s*)?@\s*([\pL\pN._]{2,30})`)
)

// Email addresses: the usual form, allowing spaces round the "@" and before
// ".com"; and the spelled-out form ("name at gmail dot com", "name [at] gmail
// [dot] com"). The spelled-out form is ordinary English too ("look at youtube
// dot com"), so spelledEmail asks for more than its shape.
var (
	emailRule = plainOnly(`\b([\pL\pN][\pL\pN._%+\-]{0,63})\s?@\s?` +
		`([\pL\pN][\pL\pN\-]{0,62}(?:\.[\pL\pN][\pL\pN\-]{0,62})*)(\s?\.\s?)([a-z]{2,24})\b`)
	spelledEmailRule = plainOnly(`\b([\pL\pN][\pL\pN._%+\-]{0,63})\s*([(\[{<]\s*at\s*[)\]}>]|\s+at\s+)\s*` +
		`([\pL\pN][\pL\pN\-]{0,62}(?:` + spelledDot + `[\pL\pN][\pL\pN\-]{0,62})*)(` + spelledDot + `)([a-z]{2,24})\b`)
)

const spelledDot = `(?:\.|\s*[(\[{<]\s*dot\s*[)\]}>]\s*|\s+dot\s+)`

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

// linkRule finds links to the hosts in contactLink. Hosts long enough not to
// turn up inside other words may be written with spaces or "dot" ("discord
// dot gg/abc"); short ones must be written exactly.
var linkRule = func() rule {
	dot := `\s?(?:\.|\(?dot\)?)\s?`
	long := func(host string) string { return strings.ReplaceAll(host, ".", dot) }
	short := func(host string) string { return strings.ReplaceAll(host, ".", `\.`) }
	hosts := words(
		long("discordapp.com"), long("discord.gg"), long("discord.com"), long("discord.me"), long("discord.io"),
		short("dsc.gg"), short("t.me"), long("telegram.me"), long("telegram.dog"), short("wa.me"),
		long("chat.whatsapp.com"), long("api.whatsapp.com"), long("snapchat.com"), short("kik.me"),
		short("ig.me"), short("instagr.am"), long("instagram.com"), short("m.me"), short("fb.me"),
		long("facebook.com"), short("fb.com"), long("messenger.com"), long("signal.me"), long("signal.group"),
		short("line.me"), long("tiktok.com"), long("twitter.com"), short("x.com"), long("reddit.com"),
		long("join.skype.com"), long("linktr.ee"))
	// Compiled as written: a path may contain "~", which phrase would read
	// as a separator.
	return rule{plain: regexp.MustCompile(`\b(?:https?\W{0,3})?(?:www\s?\.\s?)?(` + hosts + `)\s?/\s?` +
		`([\pL\pN_@~%+.\-]+(?:/[\pL\pN_@~%+.\-]*)*)`)}
}()

var (
	instagramPages = setOf(`p reel reels tv explore accounts about developer legal web`)
	facebookPages  = setOf(`groups watch share events photo photos story.php permalink.php reel marketplace
		help policies privacy login gaming pages hashtag search`)
	twitterPages = setOf(`home explore search i intent hashtag settings notifications login signup tos privacy`)
)

// contactLink decides whether a link opens a profile or an invite. A link to
// a post, a video or a page of the app's own is content, and passes.
func contactLink(host, path string) bool {
	host = strings.NewReplacer(" ", "", "(", "", ")", "", "dot", ".").Replace(host)
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
		"wa.me", "chat.whatsapp.com", "kik.me", "ig.me", "m.me", "fb.me", "signal.me", "signal.group",
		"line.me", "join.skype.com", "linktr.ee":
		return true
	case "discord.com", "discordapp.com":
		return first == "invite" || first == "users"
	case "api.whatsapp.com":
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

// supportRules are crisis language in the poster's own voice. Second-person
// versions ("kill yourself") are not here: those are encouragement, a hold.
var supportRules = []rule{bothViews(`\b` + words(
	`i~(?:(?:really|just|honestly|kinda|actually|literally|seriously|so)~)*`+words(`want`, `wanna`, `wish`, `need`)+
		`~(?:to~)?`+words(`die`, `be~dead`, `not~be~alive`, `not~exist`, `not~wake~up`, `not~be~here`,
		`disappear~forever`, `end~it~all`, `end~my~life`, `end~it`, `end~myself`, `end~everything`,
		`kill~myself`, `stop~existing`),
	`i~(?:'?m|am|m)?~`+words(`going~to`, `gonna`, `gunna`, `about~to`, `ready~to`, `thinking~about`,
		`thinking~of`, `planning~to`, `planning~on`)+`~`+words(`kill(?:ing)?~myself`, `end(?:ing)?~it~all`,
		`end(?:ing)?~my~life`, `end(?:ing)?~myself`, `commit(?:ting)?~suicide`, `hurt(?:ing)?~myself`,
		`cut(?:ting)?~myself`, `overdos(?:e|ing)`),
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

// selfHarmRules are encouragement to hurt or kill oneself, and asking or
// telling how. Pro-eating-disorder terms are here too.
var selfHarmRules = []rule{bothViews(`\b` + words(
	`kys`,
	`kill~`+words(`your`, `ur`, `yo`)+`~?sel(?:f|ves)`, `kill~urself`, `kill~yourselves`,
	`unalive~`+words(`your`, `ur`)+`~?self`,
	words(`go`, `pls`, `please`, `plz`, `why~don'?t~you`, `why~dont~you`, `you~should`, `u~should`,
		`you~shud`, `u~shud`, `you~need~to`, `u~need~to`, `you~deserve~to`, `u~deserve~to`, `you~can~go`,
		`u~can~go`, `you~ought~to`, `you~must`)+`~(?:just~)?`+
		words(`die`, `kill~(?:your|ur)~?self`, `hang~(?:your|ur)~?self`, `cut~(?:your|ur)~?self`,
			`end~it`, `end~your~life`, `starve~(?:your|ur)~?self`,
			`jump~off~(?:a|the)~`+words(`bridge`, `building`, `roof`, `cliff`)),
	words(`cut`, `hang`, `starve`)+`~`+words(`your`, `ur`)+`~?sel(?:f|ves)`,
	`slit~`+words(`your`, `ur`)+`~wrists?`,
	`drink~bleach`,
	words(`nobody`, `no~one`, `noone`)+`~`+words(`would`, `will`, `'?d`, `is~gonna`, `is~going~to`)+
		`~(?:even~)?`+words(`miss`, `care~if`)+`~`+words(`you`, `u`),
	`(?:the~)?world~`+words(`would`, `will`)+`~be~better~(?:off~)?without~`+words(`you`, `u`),
	// Method: how, how many, what dose.
	`how~`+words(`do`, `can`, `could`, `should`, `would`, `does`, `did`)+`~`+
		words(`i`, `you`, `u`, `one`, `someone`, `a~person`, `people`)+`~`+methods,
	`how~to~`+words(methods, `cut~without~`+words(`anyone`, `people`, `parents`)+`~`+
		words(`knowing`, `noticing`, `seeing`), `hide~(?:my~)?`+words(`cuts`, `scars`, `self~harm`)),
	`how~`+words(`many`, `much`)+`~[^.?!\n]{0,60}?~`+words(`to~die`, `to~kill~`+words(`my`, `your`)+`~?self`,
		`to~overdose`, `to~od`, `overdose`, `be~fatal`, `be~lethal`, `be~deadly`, `is~fatal`, `is~lethal`,
		`is~deadly`, `would~kill~`+words(`me`, `you`, `someone`, `a~person`), `to~end~it`, `to~end~my~life`),
	`what~`+words(`dose`, `amount`, `quantity`, `dosage`)+`~[^.?!\n]{0,40}?~`+
		words(`lethal`, `fatal`, `deadly`, `kill~`+words(`me`, `you`, `myself`, `yourself`), `to~die`,
			`overdose`, `od`),
	words(`painless`, `easiest`, `easy`, `quickest`, `quick`, `best`, `fastest`, `surest`, `least~painful`)+
		`~`+words(`way`, `ways`, `method`, `methods`)+`~(?:to~)?`+
		words(`die`, `kill~`+words(`my`, `your`, `ur`, `one`)+`~?self`, `commit~suicide`,
			`end~`+words(`my`, `your`, `one'?s`)+`~life`, `end~it`, `od`, `overdose`),
	`suicide~`+words(`method`, `methods`, `guide`, `instructions`, `tips`),
	// Pro-eating-disorder content.
	`thinspo`, `thinspiration`, `meanspo`, `bonespo`, `skinnyspo`, `pro~?ana`, `pro~?mia`,
	words(`ana`, `mia`)+`~`+words(`coach`, `buddy`, `tips?`),
) + `\b`)}

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
)

// Threats: an intent to hurt people or attack a place, held only when a place
// or a time comes with it ("at school", "tomorrow", "in 3rd period") — which an
// intent to attack a school always does, since it names one; and the warning
// "don't come to school tomorrow" with a cue that it is one ("you've been
// warned"). Only the first person and the future count: a history question, or
// news that reports an attack ("he threatened to shoot up his school"), names
// no intent of the poster's, so it passes.
var (
	threatPeople = words(`everyone`, `everybody`, `every~one`, `people`, `you~all`, `y'?all`, `yall`,
		`all~of~`+words(`you`, `u`, `them`), `you`, `u`, `them~all`, `them`, `him`, `her`,
		words(`the`, `my`, `our`, `all~the`, `some`)+`~`+words(`teachers?`, `students?`, `principal`, `class`,
			`classmates`, `kids`, `bully`, `bullies`, `staff`, `school`),
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

	threatIntentRules = []rule{bothViews(`\b` + words(
		words(`(?:i|we)~(?:(?:'?m|am|are|'?re|r)~)?(?:(?:really|totally|seriously|definitely|actually|literally)~)?`+
			words(`will`, `'?ll`, `wil`, `shall`, `wanna`, `want~to`, `gonna`, `gunna`, `going~to`, `planning~to`,
				`planning~on`, `plan~to`, `plan~on`, `intend~to`, `about~to`, `ready~to`, `finna`, `gotta`, `bout~to`),
			`imma`, `ima`, `i~'?ma`)+`~(?:(?:just|really|finally|actually)~)?`+words(
			words(`shoot(?:~up)?`, `stab`, `kill`, `murder`, `massacre`, `slaughter`, `gun~down`, `knife`, `attack`,
				`hurt`, `beat~up`, `behead`, `shank`)+`~`+threatPeople,
			words(`shoot~up`, `shoot~(?:at|in|inside)`, `bomb`, `blow~up`, `burn~down`, `torch`, `set~fire~to`,
				`attack`)+`~(?:`+words(`the`, `my`, `our`, `this`, `that`, `ur`, `your`)+`~)?`+threatPlaces,
			`shoot~(?:the~)?`+words(`place`, `school`, `it`)+`~up`, `shoot~up`,
			`bring~(?:`+words(`a`, `my`, `the`, `an`, `my~dad'?s`)+`~)?`+words(`guns?`, `knife`, `knives`, `bomb`,
				`weapons?`, `rifle`, `pistol`, `explosives?`, `ak`, `ar`)+`~`+words(`to`, `into`, `in`)+
				`~(?:`+words(`the`, `my`, `our`)+`~)?`+threatPlaces,
			words(`plant`, `set~off`, `detonate`)+`~(?:`+words(`a`, `the`)+`~)?`+words(`bomb`, `explosives?`, `device`)),
		words(`you`, `u`, `y'?all`, `yall`, `you~all`, `everyone`, `everybody`, `all~of~you`)+
			`~(?:`+words(`'?re`, `are`, `r`, `is`, `will`, `'?ll`)+`~)?(?:all~)?`+words(`going~to`, `gonna`, `gunna`, `will`)+
			`~`+words(`die`, `get~`+words(`shot`, `stabbed`, `hurt`, `killed`, `blown~up`)),
		`there~`+words(`will`, `'?ll`, `is~going~to`, `'?s~gonna`, `is~gonna`, `s~gonna`, `might`)+`~be~(?:a~)?`+
			words(`shooting`, `bombing`, `massacre`, `bloodbath`, `blood~bath`, `bomb`, `attack`, `school~shooting`),
	) + `\b`)}
	threatPlaceOrTimeRules = []rule{bothViews(`\b` + words(threatPlaces, threatTimes) + `\b`)}

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
		`trust~me`, words(`you'?ll`, `u'?ll`, `you~will`, `y'?all~will`)+`~see`,
		`something~`+words(`big`, `bad`, `terrible`, `crazy`, `huge`)+`~`+
			words(`is~going~to`, `will`, `is~gonna`, `gonna`, `'?s~gonna`, `'?s~going~to`)+`~happen`,
	) + `\b`)}
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

// threatCheck turns down the two commonest harmless readings of an intent
// match, from the words that follow it.
func threatCheck(text string, m []int) bool {
	next := nextWords(text[m[1]:min(len(text), m[1]+48)], 3)
	if len(next) > 0 && eventWords[next[0]] {
		return false
	}
	if strings.Contains(text[m[0]:m[1]], "shoot") {
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
	}
	return true
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
		words(`your`, `ur`)+`~age[^\pL\pN?]*\?`,
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
	) + `\b`)}
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

// matchCheck vets one match: the view's text and the submatch indexes. It
// lets a table of patterns stay readable while a few small validators make
// the decisions a regular expression cannot.
type matchCheck func(text string, m []int) bool

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
			if m[1] <= m[0] || !check(vr.v.text, m) {
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
				if m[1] > m[0] && (check == nil || check(vr.v.text, m)) {
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
// threatWindow of it, or a warning with a cue that it is one.
func (f *scannedField) threatens() bool {
	if intents := f.spans(threatIntentRules, threatCheck); len(intents) > 0 &&
		near(intents, f.spans(threatPlaceOrTimeRules, nil), threatWindow) {
		return true
	}
	if warnings := f.spans(threatWarningRules, nil); len(warnings) > 0 &&
		near(warnings, f.spans(threatWarningCues, nil), threatWindow) {
		return true
	}
	return false
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
func notAfterANumber(text string, m []int) bool {
	if !strings.HasSuffix(text[m[0]:m[1]], "kms") {
		return true
	}
	before := strings.TrimRight(text[:m[1]-3], " ")
	return before == "" || !isASCIIDigit(before[len(before)-1])
}

// notNegated turns down encouragement that is the opposite: "don't cut
// yourself on the glass", "be careful not to kill yourself", "never drink
// bleach". It looks only a few words back.
func notNegated(text string, m []int) bool {
	before := text[max(0, m[0]-24):m[0]]
	for _, neg := range []string{"don't", "dont", "do not", "never", "not to", "didn't", "careful", "shouldn't", "won't"} {
		if strings.Contains(before, neg) || strings.Contains(before, canon(neg)) {
			return false
		}
	}
	return true
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
	consider := func(from, to int, ok bool, kind findingKind) {
		if ok && (!found || from < best.from) {
			best, found = blockFinding{from: from, to: to, kind: kind}, true
		}
	}

	if run, kind, ok := f.firstNumber(); ok {
		from, to := f.plain.span(run.from, run.to)
		consider(from, to, true, kind)
	}
	from, to, ok := f.first(niRule, func(text string, m []int) bool {
		return validNIPrefix(text[m[2]], text[m[4]]) && f.upperInSource(m[0], m[1])
	})
	consider(from, to, ok, kindNI)

	from, to, ok = f.first(emailRule, func(text string, m []int) bool {
		local, domain, dot, tld := text[m[2]:m[3]], text[m[4]:m[5]], text[m[6]:m[7]], text[m[8]:m[9]]
		return emailAddress(local, domain, tld, dot != ".")
	})
	consider(from, to, ok, kindEmail)
	from, to, ok = f.first(spelledEmailRule, func(text string, m []int) bool {
		local, at, domain, tld := text[m[2]:m[3]], text[m[4]:m[5]], text[m[6]:m[7]], text[m[10]:m[11]]
		return spelledEmail(local, at, spelledDomain(domain), tld)
	})
	consider(from, to, ok, kindEmail)

	from, to, ok = f.first(linkRule, func(text string, m []int) bool {
		return contactLink(text[m[2]:m[3]], text[m[4]:m[5]])
	})
	consider(from, to, ok, kindLink)

	for _, r := range askRules {
		from, to, ok = f.first(r, nil)
		consider(from, to, ok, kindAsk)
	}
	from, to, ok = f.first(myAppRule, func(text string, m []int) bool {
		word := text[m[2]:m[3]]
		return strings.HasPrefix(word, "@") || !notAHandle[canon(word)]
	})
	consider(from, to, ok, kindHandle)
	from, to, ok = f.first(appColonHandle, func(text string, m []int) bool {
		return plausibleHandle(text[m[4]:m[5]], m[3] > m[2])
	})
	consider(from, to, ok, kindHandle)
	from, to, ok = f.first(appAtHandle, func(text string, m []int) bool {
		return plausibleHandle(text[m[2]:m[3]], true)
	})
	consider(from, to, ok, kindHandle)

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
	return strings.ContainsAny(at, "([{<") ||
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

var spelledDotPattern = regexp.MustCompile(`\s*[(\[{<]\s*dot\s*[)\]}>]\s*|\s+dot\s+`)

/* Numbers --------------------------------------------------------------------- */

// digitRun is a number as written: digit groups separated by spaces, dashes or
// dots, perhaps with a leading "+" and the first group in brackets.
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
		for i < len(t) && isASCIIDigit(t[i]) {
			run.digits = append(run.digits, t[i])
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
		j := i
		for j < len(t) && j-i < 3 && (t[j] == ' ' || t[j] == '-' || t[j] == '.') {
			j++
		}
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
	"rank", "imei", "tracking", "awb", "reference", "ref no", "ref. no"}

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

// helplineNumbers are crisis lines whose numbers look like Indian mobiles, so
// that a reader passing one on to someone who needs it is not told to take it
// out: iCall, the Vandrevala Foundation and AASRA. Each is the ten-digit
// national number.
var helplineNumbers = setOf(`9152987821 9999666555 9820466726`)

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
	d := string(run.digits)
	if dramaNumber(d) || helplineNumbers[indianNational(d)] {
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
	case indianMobile(d, run.groups), nanpPhone(d, run.groups), ukPhone(d, run.groups):
		return kindPhone, true
	case aadhaar(d, run.groups, run.seps):
		return kindAadhaar, true
	case ssn(d, run.groups, run.seps):
		return kindSSN, true
	}
	return 0, false
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
	trimmed := strings.TrimRight(before, " ")
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
func indianMobile(d string, groups []int) bool {
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
	return mobileGrouping(g)
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
// 98 7654 3210), or every digit spaced out. It turns down runs of pairs, which
// is how a list of marks or scores reads (87 91 76 88 92).
func mobileGrouping(g []int) bool {
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
func ukPhone(d string, g []int) bool {
	if len(d) != 11 || d[0] != '0' {
		return false
	}
	switch d[1] {
	case '7':
		return mobileGrouping(g)
	case '1', '2', '3':
		return len(g) > 1 && mobileGrouping(g)
	}
	return false
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
