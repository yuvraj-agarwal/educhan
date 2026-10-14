package app

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"photo.png":                    "photo.png",
		"  spaced name.jpg  ":          "spaced name.jpg",
		`C:\Users\me\Desktop\shot.png`: "shot.png",
		"/etc/passwd":                  "passwd",
		"../../escape.png":             "escape.png",
		"tab\tand\nnewline.png":        "tabandnewline.png",
		"":                             "",
		"..":                           "",
		"/":                            "",
		"café ☕.png":                   "café ☕.png",
		"photo\u202egnp.exe":           "photognp.exe",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeFilenameStaysValidUTF8(t *testing.T) {
	// A filename arrives as arbitrary bytes; the result has to survive a text
	// column, so invalid sequences are dropped and truncation respects runes.
	got := sanitizeFilename("bad\xff\xfebytes.png")
	if !utf8.ValidString(got) {
		t.Fatalf("sanitizeFilename returned invalid UTF-8: %q", got)
	}
	if got != "badbytes.png" {
		t.Errorf("sanitizeFilename = %q, want badbytes.png", got)
	}

	long := strings.Repeat("é", 300) + ".png"
	got = sanitizeFilename(long)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != maxOriginalNameLen {
		t.Errorf("truncated to %d runes, want %d", n, maxOriginalNameLen)
	}
	if !strings.HasSuffix(got, ".png") {
		t.Errorf("truncated name %q lost its extension", got)
	}
}
