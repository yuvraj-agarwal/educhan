package app

import (
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

func TestCountLines(t *testing.T) {
	if got := countLines("a"); got != 1 {
		t.Errorf("countLines = %d, want 1", got)
	}
	if got := countLines("a\nb\nc"); got != 3 {
		t.Errorf("countLines = %d, want 3", got)
	}
}
