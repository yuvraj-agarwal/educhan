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
// the server needs to know where the tags pair only so that code is not read
// as quotes and a spoiler does not leak into an excerpt, and markupBlocks
// follows the same rules for that.
const (
	codeOpenTag     = "[code]"
	codeCloseTag    = "[/code]"
	spoilerOpenTag  = "[spoiler]"
	spoilerCloseTag = "[/spoiler]"
)

// markupBlock is a [code] block or a [spoiler] that renders as one. start and
// end span the tags; inner spans what is between them.
type markupBlock struct {
	spoiler    bool
	start, end int
	inner      [2]int
}

// markupBlocks finds the code blocks and spoilers in a body, in order of their
// opening tags; a code block inside a spoiler is listed after it. Each tag pairs
// with the first closing tag after it. A [code] needs a [/code] with something
// between them and claims everything up to it, tags included; a [spoiler] needs
// a [/spoiler] after it, outside any code block, with something between them,
// and a [spoiler] inside one is text. Anything that does not pair is text.
func markupBlocks(body string) []markupBlock {
	type tag struct {
		kind       string
		start, end int
	}
	var tags []tag
	// Checked first, so a body full of unclosed [code]s is still one pass.
	lastCodeClose := strings.LastIndex(body, codeCloseTag)
	for i := 0; i < len(body); {
		rest := body[i:]
		switch {
		case strings.HasPrefix(rest, codeOpenTag) && i+len(codeOpenTag) <= lastCodeClose:
			inner := i + len(codeOpenTag)
			if end := strings.Index(body[inner:], codeCloseTag); end > 0 {
				tags = append(tags, tag{"code", i, inner + end + len(codeCloseTag)})
				i = inner + end + len(codeCloseTag)
				continue
			}
		case strings.HasPrefix(rest, spoilerOpenTag):
			tags = append(tags, tag{"open", i, i + len(spoilerOpenTag)})
			i += len(spoilerOpenTag)
			continue
		case strings.HasPrefix(rest, spoilerCloseTag):
			tags = append(tags, tag{"close", i, i + len(spoilerCloseTag)})
			i += len(spoilerCloseTag)
			continue
		}
		next := strings.IndexByte(body[i+1:], '[')
		if next < 0 {
			break
		}
		i += 1 + next
	}

	lastClose := -1
	for n, t := range tags {
		if t.kind == "close" {
			lastClose = n
		}
	}

	var blocks []markupBlock
	open := -1 // index into blocks of the spoiler that is open, if any
	for n, t := range tags {
		switch t.kind {
		case "code":
			blocks = append(blocks, markupBlock{
				start: t.start, end: t.end,
				inner: [2]int{t.start + len(codeOpenTag), t.end - len(codeCloseTag)},
			})
		case "open":
			empty := n+1 < len(tags) && tags[n+1].kind == "close" && tags[n+1].start == t.end
			if open < 0 && n < lastClose && !empty {
				open = len(blocks)
				blocks = append(blocks, markupBlock{spoiler: true, start: t.start, inner: [2]int{t.end, 0}})
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

// plainText is a body as a summary should show it: a code block's text without
// its tags, and a spoiler as the word, never what it hides.
func plainText(body string) string {
	blocks := markupBlocks(body)
	if len(blocks) == 0 {
		return body
	}
	var b strings.Builder
	pos := 0
	for _, blk := range blocks {
		if blk.start < pos {
			continue // a code block inside a spoiler already written
		}
		b.WriteString(body[pos:blk.start])
		if blk.spoiler {
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
// Code blocks are left out, because the front end does not link inside them: a
// code sample such as "x >>2" is not a reply to post 2.
func extractQuotes(body string) []int64 {
	if blocks := markupBlocks(body); len(blocks) > 0 {
		var b strings.Builder
		pos := 0
		for _, blk := range blocks {
			if blk.spoiler {
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
