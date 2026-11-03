package app

import (
	"errors"
	"testing"

	"github.com/yuvraj-agarwal/educhan/internal/media"
)

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

// What the posts waiting for room hold between them is capped (waitingRoom),
// so it has to go back down as each stops waiting: kept, it would only grow,
// until every post that holds room and finds none free was turned away busy at
// once for the life of the process. A client waits once at a time, and is free
// to wait again once it no longer does; the function that says so says it
// once, however often it is called.
func TestUploadSharesGiveBackWhatTheWaitingHeld(t *testing.T) {
	var s uploadShares
	const (
		client = "203.0.113.7"
		other  = "203.0.113.8"
		limit  = 10 << 20
	)
	waitHeld := func() int64 {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.waitHeld
	}

	first, err := s.wait(client, limit, limit)
	if err != nil {
		t.Fatalf("a wait holding the whole waiting room was refused: %v", err)
	}
	if _, err := s.wait(client, 0, limit); !errors.Is(err, errUploadShare) {
		t.Errorf("a client's second wait at once: %v, want %v", err, errUploadShare)
	}
	if _, err := s.wait(other, 1, limit); !errors.Is(err, media.ErrBusy) {
		t.Errorf("a wait holding a byte past the waiting room: %v, want %v", err, media.ErrBusy)
	}
	nothing, err := s.wait(other, 0, limit)
	if err != nil {
		t.Fatalf("a wait holding nothing, beside one holding the whole waiting room, was refused: %v", err)
	}
	nothing()
	if got := waitHeld(); got != limit {
		t.Errorf("the posts waiting hold %d bytes once one holding nothing stops, want %d", got, limit)
	}

	first()
	first()
	if got := waitHeld(); got != 0 {
		t.Errorf("the posts waiting hold %d bytes once the only one holding any has stopped (said twice), want 0", got)
	}
	if n := len(s.waiting); n != 0 {
		t.Errorf("%d clients wait once none does, want none: %v", n, s.waiting)
	}

	// The client, and the room, are free again: the same wait is taken, as
	// often as the room it held was given back.
	for i := 0; i < 3; i++ {
		again, err := s.wait(client, limit, limit)
		if err != nil {
			t.Fatalf("wait %d, holding what the first held, was refused: %v", i+2, err)
		}
		again()
	}
	if got := waitHeld(); got != 0 {
		t.Errorf("the posts waiting hold %d bytes once each has stopped, want 0", got)
	}
}
