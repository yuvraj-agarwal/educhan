package app

import "testing"

// An image is shown under "image" and its stored format's extension. The name
// comes from the path the server chose, so nothing a poster sent can reach it.
func TestAttachmentName(t *testing.T) {
	cases := map[string]string{
		"general/2026/10/6f1c0e.jpg":   "image.jpg",
		"general/2026/10/6f1c0e.png":   "image.png",
		"u-iitb/2026/10/6f1c0e.webp":   "image.webp",
		"a.b/2026/10/6f1c0e.gif":       "image.gif",
		"general/2026/10/no-extension": "image",
	}
	for stored, want := range cases {
		if got := attachmentName(stored); got != want {
			t.Errorf("attachmentName(%q) = %q, want %q", stored, got, want)
		}
	}
}
