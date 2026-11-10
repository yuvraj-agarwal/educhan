package app

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// An IPv6 client is keyed on its /64 (clientIP), because a subscriber is
// handed at least a /64 and may use any address in it. These tests drive the
// real handlers through one trusted proxy and hold every limit that keys on the
// client, and the poster ID of a post sent with no poster key, to that: a
// second address in the same /64 is the same client, and the first address of
// the next /64 is another.
const (
	// The first and the last address of one /64: the furthest apart two
	// addresses of one client can be.
	sameNetworkFirst = "2001:db8:1:2::1"
	sameNetworkLast  = "2001:db8:1:2:ffff:ffff:ffff:ffff"
	// The address straight after sameNetworkLast, and the first of the
	// neighbouring /64.
	nextNetwork = "2001:db8:1:3::"
)

// fromAddress is the header that makes a request appear to arrive through one
// trusted proxy from address.
func fromAddress(address string) http.Header {
	return http.Header{"X-Forwarded-For": {address}}
}

// refusedForFlooding fails the test unless the last response was flood
// control's refusal: 429 with a Retry-After of the full minute.
func refusedForFlooding(t *testing.T, c *client, what string, status int) {
	t.Helper()
	if status != http.StatusTooManyRequests {
		t.Errorf("%s: status %d, want 429: another address in the same /64 is the same client", what, status)
		return
	}
	if got := c.lastHeader.Get("Retry-After"); got != "60" {
		t.Errorf("%s: Retry-After = %q, want 60", what, got)
	}
}

func TestThreadCooldownCoversAnIPv6Clients64(t *testing.T) {
	h := testServer(t, func(cfg *Config) {
		cfg.TrustedProxyHops = 1
		cfg.ThreadCooldown = time.Minute
	})
	h.addBoard("general")
	const path = "/api/boards/general/threads"

	var out createdPost
	if status := h.postFrom(sameNetworkFirst, path, map[string]any{"body": "first"}, &out); status != http.StatusCreated {
		t.Fatalf("first thread: status %d (%s)", status, out.Error)
	}
	refusedForFlooding(t, h.client, "a second thread from the same /64",
		h.postFrom(sameNetworkLast, path, map[string]any{"body": "too soon"}, &out))
	if status := h.postFrom(nextNetwork, path, map[string]any{"body": "a neighbour"}, &out); status != http.StatusCreated {
		t.Errorf("thread from the neighbouring /64: status %d (%s), want 201", status, out.Error)
	}
}

func TestReplyCooldownCoversAnIPv6Clients64(t *testing.T) {
	h := testServer(t, func(cfg *Config) {
		cfg.TrustedProxyHops = 1
		cfg.PostCooldown = time.Minute
	})
	h.addBoard("general")
	thread := h.createThread("general", "", "", "a thread")
	path := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)

	var out createdPost
	if status := h.postFrom(sameNetworkFirst, path, map[string]any{"body": "first"}, &out); status != http.StatusCreated {
		t.Fatalf("first reply: status %d (%s)", status, out.Error)
	}
	refusedForFlooding(t, h.client, "a second reply from the same /64",
		h.postFrom(sameNetworkLast, path, map[string]any{"body": "too soon"}, &out))
	if status := h.postFrom(nextNetwork, path, map[string]any{"body": "a neighbour"}, &out); status != http.StatusCreated {
		t.Errorf("reply from the neighbouring /64: status %d (%s), want 201", status, out.Error)
	}
}

// The upload cooldown is server-wide, so the second image goes to another
// board: only the client can have refused it.
func TestUploadCooldownCoversAnIPv6Clients64(t *testing.T) {
	h := testServer(t, func(cfg *Config) {
		cfg.TrustedProxyHops = 1
		cfg.UploadCooldown = time.Minute
	})
	h.addBoard("general", "research")
	image := pngBytes(t, 24, 24)

	var out createdPost
	if status := h.uploadWithHeaders(fromAddress(sameNetworkFirst), "/api/boards/general/threads",
		map[string]string{"body": "first image"}, "first.png", image, &out); status != http.StatusCreated {
		t.Fatalf("first upload: status %d (%s)", status, out.Error)
	}
	refusedForFlooding(t, h.client, "a second upload from the same /64",
		h.uploadWithHeaders(fromAddress(sameNetworkLast), "/api/boards/research/threads",
			map[string]string{"body": "too soon"}, "second.png", image, &out))
	if status := h.uploadWithHeaders(fromAddress(nextNetwork), "/api/boards/research/threads",
		map[string]string{"body": "a neighbour"}, "third.png", image, &out); status != http.StatusCreated {
		t.Errorf("upload from the neighbouring /64: status %d (%s), want 201", status, out.Error)
	}
}

// Each request names a different mailbox, so only the client's half of the
// limiter can refuse the second.
func TestCodeRequestCooldownCoversAnIPv6Clients64(t *testing.T) {
	h := verifyServer(t, func(cfg *Config) {
		cfg.TrustedProxyHops = 1
		cfg.VerifyCooldown = time.Minute
	})
	const path = "/api/verify/request"

	var out verifyResponse
	if status := h.postFrom(sameNetworkFirst, path,
		map[string]any{"email": "ana@iitb.ac.in", "adult": true}, &out); status != http.StatusAccepted {
		t.Fatalf("first code request: status %d (%s)", status, out.Error)
	}
	refusedForFlooding(t, h.client, "a code request from the same /64",
		h.postFrom(sameNetworkLast, path, map[string]any{"email": "bo@iitb.ac.in", "adult": true}, &out))
	if status := h.postFrom(nextNetwork, path,
		map[string]any{"email": "cy@iitb.ac.in", "adult": true}, &out); status != http.StatusAccepted {
		t.Errorf("code request from the neighbouring /64: status %d (%s), want 202", status, out.Error)
	}
	if got := h.mail.count(); got != 2 {
		t.Errorf("%d codes mailed, want 2: the refused request must send nothing", got)
	}
}

// Without a poster key the ID comes from the client's key, so two addresses in
// one /64 post under one ID in a thread, as two browsers behind one IPv4 NAT
// address do.
func TestNoPosterKeyGivesAnIPv6Clients64OneID(t *testing.T) {
	h := testServer(t, func(cfg *Config) { cfg.TrustedProxyHops = 1 })
	h.addBoard("ids")
	opener := browser{harness: h, forwardedFor: "203.0.113.20"}
	first := browser{harness: h, forwardedFor: sameNetworkFirst}
	last := browser{harness: h, forwardedFor: sameNetworkLast}
	neighbour := browser{harness: h, forwardedFor: nextNetwork}

	thread := opener.thread("ids", "a thread", false)
	fromFirst := first.reply("ids", thread, "from one end of the /64", false)
	fromLast := last.reply("ids", thread, "from the other end", false)
	fromNeighbour := neighbour.reply("ids", thread, "from the next /64", false)

	ids := posterIDs(t, h, "ids", thread)
	want := posterID(h.app.cfg.PosterIDSalt, addressSource("2001:db8:1:2::"), h.threadID("ids", thread))
	if ids[fromFirst] != want || ids[fromLast] != want {
		t.Errorf("keyless replies from one /64 got %q and %q, want both %q, the ID of the /64",
			ids[fromFirst], ids[fromLast], want)
	}
	if ids[fromNeighbour] == ids[fromFirst] {
		t.Errorf("the neighbouring /64 got the same poster ID %q", ids[fromNeighbour])
	}
}
