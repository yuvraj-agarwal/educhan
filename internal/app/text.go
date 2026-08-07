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

// extractQuotes returns the distinct post numbers quoted by a body, in order of
// first appearance and capped so a single post cannot fan out without bound.
func extractQuotes(body string) []int64 {
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
// rune in half.
func excerpt(body string, limit int) string {
	body = strings.TrimSpace(body)
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
