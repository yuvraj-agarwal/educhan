package app

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxSubjectLen = 100
	maxBodyLen    = 8000
	maxBodyLines  = 100
	maxQuotes     = 20
	excerptLen    = 220
)

// quotePattern matches a quote link: ">>123" referring to post number 123 on the
// same board.
var quotePattern = regexp.MustCompile(`>>([0-9]{1,18})`)

// blankRunPattern matches three or more consecutive newlines.
var blankRunPattern = regexp.MustCompile(`\n{3,}`)

// normalizeBody canonicalises post text: CRLF is normalised, control characters
// are dropped, trailing whitespace per line is removed and long runs of blank
// lines are collapsed. The result is stored verbatim; rendering (greentext,
// quote links) happens at display time.
func normalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = collapseControl(body)

	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	body = strings.Join(lines, "\n")
	body = blankRunPattern.ReplaceAllString(body, "\n\n")
	return strings.TrimSpace(body)
}

// collapseControl removes control characters that have no business in a post,
// keeping newlines and tabs.
func collapseControl(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t':
			return r
		}
		if r == utf8.RuneError || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// Post markup tags. Rendering is the front end's (parseMarkup in web/app.js);
// the server needs to know where the tags pair only so that code and maths are
// not read as quotes and a spoiler does not leak into an excerpt, and
// markupBlocks follows the same rules for that.
const (
	spoilerOpenTag  = "[spoiler]"
	spoilerCloseTag = "[/spoiler]"
)

// blockKind is what a markup block renders as.
type blockKind int

const (
	blockSpoiler blockKind = iota
	blockCode              // [code]: a code block
	blockMath              // [math]: inline TeX
	blockEqn               // [eqn]: display TeX
)

// opaqueTags are the blocks whose text is not markup: code, and TeX, which has
// a use of its own for every character. Each claims everything up to the first
// closing tag of its kind after it.
var opaqueTags = []struct {
	kind        blockKind
	open, close string
}{
	{blockCode, "[code]", "[/code]"},
	{blockMath, "[math]", "[/math]"},
	{blockEqn, "[eqn]", "[/eqn]"},
}

// markupBlock is a [code], [math] or [eqn] block, or a [spoiler] that renders as
// one. start and end span the tags; inner spans what is between them.
type markupBlock struct {
	kind       blockKind
	start, end int
	inner      [2]int
}

// markupBlocks finds the opaque blocks and spoilers in a body, in order of their
// opening tags; a block inside a spoiler is listed after it. Each tag pairs with
// the first closing tag of its kind after it. An opaque block needs its closing
// tag with something between them and claims everything up to it, tags
// included; a [spoiler] needs a [/spoiler] after it, outside any opaque block,
// with something between them, and a [spoiler] inside one is text. Anything
// that does not pair is text.
func markupBlocks(body string) []markupBlock {
	type tag struct {
		kind       string // "opaque", or a spoiler's "open" or "close"
		start, end int
		block      markupBlock // for "opaque", the block it claimed
	}
	var tags []tag
	// Checked first, so a body full of unclosed openings is still one pass.
	lastClose := make([]int, len(opaqueTags))
	for k, t := range opaqueTags {
		lastClose[k] = strings.LastIndex(body, t.close)
	}
scan:
	for i := 0; i < len(body); {
		rest := body[i:]
		for k, t := range opaqueTags {
			inner := i + len(t.open)
			if !strings.HasPrefix(rest, t.open) || inner > lastClose[k] {
				continue
			}
			if end := strings.Index(body[inner:], t.close); end > 0 {
				blk := markupBlock{
					kind: t.kind, start: i, end: inner + end + len(t.close),
					inner: [2]int{inner, inner + end},
				}
				tags = append(tags, tag{kind: "opaque", start: blk.start, end: blk.end, block: blk})
				i = blk.end
				continue scan
			}
		}
		switch {
		case strings.HasPrefix(rest, spoilerOpenTag):
			tags = append(tags, tag{kind: "open", start: i, end: i + len(spoilerOpenTag)})
			i += len(spoilerOpenTag)
			continue
		case strings.HasPrefix(rest, spoilerCloseTag):
			tags = append(tags, tag{kind: "close", start: i, end: i + len(spoilerCloseTag)})
			i += len(spoilerCloseTag)
			continue
		}
		next := strings.IndexByte(body[i+1:], '[')
		if next < 0 {
			break
		}
		i += 1 + next
	}

	lastSpoilerClose := -1
	for n, t := range tags {
		if t.kind == "close" {
			lastSpoilerClose = n
		}
	}

	var blocks []markupBlock
	open := -1 // index into blocks of the spoiler that is open, if any
	for n, t := range tags {
		switch t.kind {
		case "opaque":
			blocks = append(blocks, t.block)
		case "open":
			empty := n+1 < len(tags) && tags[n+1].kind == "close" && tags[n+1].start == t.end
			if open < 0 && n < lastSpoilerClose && !empty {
				open = len(blocks)
				blocks = append(blocks, markupBlock{kind: blockSpoiler, start: t.start, inner: [2]int{t.end, 0}})
			}
		case "close":
			if open >= 0 {
				blocks[open].end = t.end
				blocks[open].inner[1] = t.start
				open = -1
			}
		}
	}
	return blocks
}

// plainText is a body as a summary should show it: the text of a code block or
// of maths without its tags — TeX source is the readable form of an equation
// in plain text — and a spoiler as the word, never what it hides.
func plainText(body string) string {
	blocks := markupBlocks(body)
	if len(blocks) == 0 {
		return body
	}
	var b strings.Builder
	pos := 0
	for _, blk := range blocks {
		if blk.start < pos {
			continue // a block inside a spoiler already written
		}
		b.WriteString(body[pos:blk.start])
		if blk.kind == blockSpoiler {
			b.WriteString("[spoiler]")
		} else {
			b.WriteString(body[blk.inner[0]:blk.inner[1]])
		}
		pos = blk.end
	}
	b.WriteString(body[pos:])
	return b.String()
}

// extractQuotes returns the distinct post numbers quoted by a body, in order of
// first appearance and capped so a single post cannot fan out without bound.
// Code and maths are left out, because the front end does not link inside
// them: a code sample such as "x >>2" is not a reply to post 2, and neither is
// a shift written in TeX.
func extractQuotes(body string) []int64 {
	if blocks := markupBlocks(body); len(blocks) > 0 {
		var b strings.Builder
		pos := 0
		for _, blk := range blocks {
			if blk.kind == blockSpoiler {
				continue // its text is linked like any other
			}
			// A space where the block was, so the text either side of it
			// cannot join up into a quote that was never written.
			b.WriteString(body[pos:blk.start])
			b.WriteByte(' ')
			pos = blk.end
		}
		b.WriteString(body[pos:])
		body = b.String()
	}

	matches := quotePattern.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[int64]bool, len(matches))
	quotes := make([]int64, 0, min(len(matches), maxQuotes))
	for _, m := range matches {
		no, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || no <= 0 || seen[no] {
			continue
		}
		seen[no] = true
		quotes = append(quotes, no)
		if len(quotes) == maxQuotes {
			break
		}
	}
	return quotes
}

// excerpt shortens a body for catalog tiles and index previews without cutting a
// rune in half. It works on the plain text, so a spoiler stays hidden there too.
func excerpt(body string, limit int) string {
	body = strings.TrimSpace(plainText(body))
	if utf8.RuneCountInString(body) <= limit {
		return body
	}
	return strings.TrimRight(truncateRunes(body, limit), " \t\n") + "\u2026"
}

// excerptOfStart is excerpt for the first runes of a body, as the archive reads
// them; cut says whether the body ran on past them. Cutting can change how tags
// pair — a [spoiler] whose [/spoiler] was cut off reads as text, and so would
// what it hides — so a cut start is first shortened to settledStart, and its
// excerpt always ends in an ellipsis.
func excerptOfStart(start string, cut bool, limit int) string {
	if !cut {
		return excerpt(start, limit)
	}
	text := strings.TrimSpace(plainText(settledStart(start)))
	return strings.TrimRight(truncateRunes(text, limit), " \t\n") + "\u2026"
}

// markupTags is every tag markupBlocks knows, for recognising one cut in half.
var markupTags = func() []string {
	tags := []string{spoilerOpenTag, spoilerCloseTag}
	for _, t := range opaqueTags {
		tags = append(tags, t.open, t.close)
	}
	return tags
}()

// settledStart shortens the start of a longer body to the part whose tags pair
// the same way whatever followed it. That part ends before the first opening
// tag that pairs with nothing in it, because what followed could hold its
// close, and before a tag cut in half. Shortening can unpair an earlier tag —
// a spoiler whose close sat after an unclosed [code], which the rest of the
// body may well have closed — so it repeats until nothing changes.
func settledStart(s string) string {
	for {
		at := firstUnsettledTag(s)
		if at < 0 {
			return s
		}
		s = s[:at]
	}
}

// firstUnsettledTag returns where settledStart has to cut s, or -1 when every
// tag in s pairs as it would in any longer body.
func firstUnsettledTag(s string) int {
	blocks := markupBlocks(s)
	inside := func(i int, spoiler bool) bool {
		for _, blk := range blocks {
			if (blk.kind == blockSpoiler) == spoiler && blk.start < i && i < blk.end {
				return true
			}
		}
		return false
	}
	opens := func(i int) bool {
		for _, blk := range blocks {
			if blk.start == i {
				return true
			}
		}
		return false
	}

	for i := strings.IndexByte(s, '['); i >= 0; {
		rest := s[i:]
		switch {
		case inside(i, false):
			// Text of a code or maths block, which closed within s.
		case strings.HasPrefix(rest, spoilerOpenTag+spoilerCloseTag):
			// An empty pair is text whatever follows it.
		case strings.HasPrefix(rest, spoilerOpenTag):
			// Inside an open spoiler a [spoiler] is text, and the spoiler
			// around it is settled or found unsettled on its own account.
			if !opens(i) && !inside(i, true) {
				return i
			}
		default:
			for _, t := range opaqueTags {
				if strings.HasPrefix(rest, t.open) && !opens(i) {
					return i
				}
			}
			for _, tag := range markupTags {
				if len(rest) < len(tag) && strings.HasPrefix(tag, rest) {
					return i
				}
			}
		}
		next := strings.IndexByte(s[i+1:], '[')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return -1
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	count := 0
	for i := range s {
		if count == limit {
			return s[:i]
		}
		count++
	}
	return s
}

func countLines(s string) int {
	return strings.Count(s, "\n") + 1
}
