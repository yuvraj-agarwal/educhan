package app

import "testing"

func TestAuthorOrAnon(t *testing.T) {
	cases := map[string]string{
		"":            "Anonymous",
		"   ":         "Anonymous",
		"  qwerty  ":  "qwerty",
		"studentX":    "studentX",
	}
	for in, want := range cases {
		if got := authorOrAnon(in); got != want {
			t.Errorf("authorOrAnon(%q) = %q, want %q", in, got, want)
		}
	}

	long := make([]byte, 100)
	for i := range long {
		long[i] = 'a'
	}
	if got := authorOrAnon(string(long)); len(got) != 64 {
		t.Errorf("expected author truncated to 64 chars, got %d", len(got))
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "x"); got != "x" {
		t.Errorf("firstNonEmpty = %q, want x", got)
	}
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Errorf("firstNonEmpty = %q, want a", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("firstNonEmpty() = %q, want empty", got)
	}
}
