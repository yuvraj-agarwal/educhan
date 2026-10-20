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

// A post's share of the upload budget is counted per address, but per /64 for
// IPv6, where one host can usually take any address in its /64.
func TestShareKey(t *testing.T) {
	cases := map[string]string{
		"203.0.113.7":                 "203.0.113.7",
		"2001:db8:0:1::1":             "2001:db8:0:1::/64",
		"2001:db8:0:1:ffff:ffff:1:2":  "2001:db8:0:1::/64",
		"2001:db8:0:2::1":             "2001:db8:0:2::/64",
		"fe80::1%eth0":                "fe80::/64",
		"::1":                         "::/64",
		"::ffff:203.0.113.7":          "::ffff:203.0.113.7", // clientIP unmaps these; left whole if one comes
		"not an address":              "not an address",
		"2001:db8:0:1:0:0:0:1/64":     "2001:db8:0:1:0:0:0:1/64",
		"2001:0db8:0000:0001:0::0001": "2001:db8:0:1::/64",
	}
	for client, want := range cases {
		if got := shareKey(client); got != want {
			t.Errorf("shareKey(%q) = %q, want %q", client, got, want)
		}
	}
}

// The upload shares are keyed on client addresses (shareKey), so they hold
// one only while that client's image posts hold room: an entry goes the
// moment the last of them gives its share back, with no sweep to wait for,
// and a share given back twice gives back nothing more.
func TestUploadSharesForgetAClientOnceItHoldsNothing(t *testing.T) {
	var s uploadShares
	const client = "203.0.113.7"
	first, ok := s.take(client, 3<<20)
	if !ok {
		t.Fatal("the first share was refused")
	}
	second, ok := s.take(client, 2<<20)
	if !ok {
		t.Fatal("the second share was refused")
	}
	if _, ok := s.take(client, clientUploadShare); ok {
		t.Fatal("a share past clientUploadShare was taken")
	}
	first()
	first()
	if got := s.of(client); got != 2<<20 {
		t.Errorf("after one of two shares was given back (twice), the client holds %d bytes, want %d", got, 2<<20)
	}
	second()
	if n := len(s.held); n != 0 {
		t.Errorf("the shares hold %d clients once every share is given back, want none: %v", n, s.held)
	}
}
