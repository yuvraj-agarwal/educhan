package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		forwarded  []string
		hops       int
		want       string
	}{
		{
			name:       "no proxy trusted",
			remoteAddr: "203.0.113.7:52000",
			want:       "203.0.113.7",
		},
		{
			// The whole point of the hop count: a client that sends its own
			// header while the server is exposed directly gets no say.
			name:       "forwarded header ignored without trusted proxies",
			remoteAddr: "203.0.113.7:52000",
			forwarded:  []string{"198.51.100.9"},
			want:       "203.0.113.7",
		},
		{
			name:       "one proxy appends the peer",
			remoteAddr: "10.0.0.1:41000",
			forwarded:  []string{"198.51.100.9"},
			hops:       1,
			want:       "198.51.100.9",
		},
		{
			// A client that prepends entries cannot move the entry the trusted
			// hop count points at.
			name:       "spoofed entries to the left are ignored",
			remoteAddr: "10.0.0.1:41000",
			forwarded:  []string{"1.1.1.1, 2.2.2.2, 198.51.100.9"},
			hops:       1,
			want:       "198.51.100.9",
		},
		{
			name:       "two proxies count from the right",
			remoteAddr: "10.0.0.1:41000",
			forwarded:  []string{"9.9.9.9, 198.51.100.9, 10.0.0.2"},
			hops:       2,
			want:       "198.51.100.9",
		},
		{
			// Repeated headers are one chain; a client must not be able to split
			// the list to shift the index.
			name:       "repeated headers are one chain",
			remoteAddr: "10.0.0.1:41000",
			forwarded:  []string{"1.1.1.1", "198.51.100.9, 10.0.0.2"},
			hops:       2,
			want:       "198.51.100.9",
		},
		{
			// Shorter than declared means the request did not come through the
			// proxies the operator described.
			name:       "chain shorter than the trusted depth falls back to the peer",
			remoteAddr: "203.0.113.7:52000",
			forwarded:  []string{"198.51.100.9"},
			hops:       2,
			want:       "203.0.113.7",
		},
		{
			name:       "missing header falls back to the peer",
			remoteAddr: "203.0.113.7:52000",
			hops:       1,
			want:       "203.0.113.7",
		},
		{
			// An obfuscated or junk identifier is not a key a client may choose.
			name:       "non-address entry falls back to the peer",
			remoteAddr: "203.0.113.7:52000",
			forwarded:  []string{"_hidden"},
			hops:       1,
			want:       "203.0.113.7",
		},
		{
			name:       "entry with a port",
			remoteAddr: "10.0.0.1:41000",
			forwarded:  []string{"198.51.100.9:1234"},
			hops:       1,
			want:       "198.51.100.9",
		},
		{
			// IPv6 is keyed on its /64, the block a subscriber is handed and
			// may use any address of, so the key is the network's first
			// address however the entry is written.
			name:       "bracketed ipv6 with a port",
			remoteAddr: "192.0.2.1:41000",
			forwarded:  []string{"[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:443"},
			hops:       1,
			want:       "2001:db8:1:2::",
		},
		{
			name:       "bracketed ipv6",
			remoteAddr: "192.0.2.1:41000",
			forwarded:  []string{"[2001:db8:1:2:aaaa:bbbb:cccc:dddd]"},
			hops:       1,
			want:       "2001:db8:1:2::",
		},
		{
			name:       "bare ipv6",
			remoteAddr: "192.0.2.1:41000",
			forwarded:  []string{"2001:db8:1:2:aaaa:bbbb:cccc:dddd"},
			hops:       1,
			want:       "2001:db8:1:2::",
		},
		{
			// Any other address in the same /64 is the same client; otherwise
			// it could take a fresh key, and a fresh cooldown, every request.
			name:       "another address in the same /64 has the same key",
			remoteAddr: "192.0.2.1:41000",
			forwarded:  []string{"2001:db8:1:2::1"},
			hops:       1,
			want:       "2001:db8:1:2::",
		},
		{
			name:       "the last address of a /64",
			remoteAddr: "192.0.2.1:41000",
			forwarded:  []string{"2001:db8:1:2:ffff:ffff:ffff:ffff"},
			hops:       1,
			want:       "2001:db8:1:2::",
		},
		{
			// The next address over is the first of the next /64: a
			// neighbouring network, so a key of its own.
			name:       "the first address of the next /64",
			remoteAddr: "192.0.2.1:41000",
			forwarded:  []string{"2001:db8:1:3::"},
			hops:       1,
			want:       "2001:db8:1:3::",
		},
		{
			// The forwarded entry is cut by the same rule as the peer, and
			// the peer is not consulted when the entry is good.
			name:       "ipv6 entry behind an ipv6 proxy",
			remoteAddr: "[2001:db8:ff::1]:41000",
			forwarded:  []string{"2001:db8:1:2:aaaa:bbbb:cccc:dddd"},
			hops:       1,
			want:       "2001:db8:1:2::",
		},
		{
			// Otherwise one client holds two rate-limit buckets by switching
			// notation.
			name:       "ipv4-mapped ipv6 folds to ipv4",
			remoteAddr: "10.0.0.1:41000",
			forwarded:  []string{"::ffff:198.51.100.9"},
			hops:       1,
			want:       "198.51.100.9",
		},
		{
			name:       "empty entries are skipped",
			remoteAddr: "10.0.0.1:41000",
			forwarded:  []string{" , 198.51.100.9 , "},
			hops:       1,
			want:       "198.51.100.9",
		},
		{
			name:       "ipv4-mapped ipv6 with brackets and a port folds to ipv4",
			remoteAddr: "192.0.2.1:41000",
			forwarded:  []string{"[::ffff:198.51.100.9]:443"},
			hops:       1,
			want:       "198.51.100.9",
		},
		{
			// A zone names an interface on the proxy, not a client.
			name:       "forwarded entry with a zone",
			remoteAddr: "192.0.2.1:41000",
			forwarded:  []string{"2001:db8:1:2::5%eth0"},
			hops:       1,
			want:       "2001:db8:1:2::",
		},
		{
			// The peer goes through the same cut as a forwarded entry, so a
			// server exposed directly over IPv6 groups the same way.
			name:       "ipv6 peer with no trusted proxy",
			remoteAddr: "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:52000",
			want:       "2001:db8:1:2::",
		},
		{
			name:       "ipv6 peer ignores a forwarded header without trusted proxies",
			remoteAddr: "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:52000",
			forwarded:  []string{"2001:db8:9::1"},
			want:       "2001:db8:1:2::",
		},
		{
			name:       "ipv4-mapped peer folds to ipv4",
			remoteAddr: "[::ffff:203.0.113.7]:52000",
			want:       "203.0.113.7",
		},
		{
			name:       "peer with a zone",
			remoteAddr: "[2001:db8:1:2::5%eth0]:52000",
			want:       "2001:db8:1:2::",
		},
		{
			name:       "ipv4 peer is unchanged",
			remoteAddr: "203.0.113.7:52000",
			want:       "203.0.113.7",
		},
		{
			name:       "ipv4 forwarded entry is unchanged",
			remoteAddr: "[2001:db8:ff::1]:41000",
			forwarded:  []string{"203.0.113.7"},
			hops:       1,
			want:       "203.0.113.7",
		},
		{
			// The fallbacks land on the peer's key, not its whole address.
			name:       "short chain falls back to an ipv6 peer's /64",
			remoteAddr: "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:52000",
			forwarded:  []string{"198.51.100.9"},
			hops:       2,
			want:       "2001:db8:1:2::",
		},
		{
			name:       "non-address entry falls back to an ipv6 peer's /64",
			remoteAddr: "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:52000",
			forwarded:  []string{"_hidden"},
			hops:       1,
			want:       "2001:db8:1:2::",
		},
		{
			name:       "missing header falls back to an ipv6 peer's /64",
			remoteAddr: "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:52000",
			hops:       1,
			want:       "2001:db8:1:2::",
		},
		{
			// Whatever a listener hands over that is not an address is kept
			// as it is, as before; there is nothing to cut.
			name:       "peer that is not an ip address",
			remoteAddr: "@",
			want:       "@",
		},
		{
			// A host with a port that is not an address loses the port and
			// keeps the host, as main always has.
			name:       "peer with a port that is not an ip address",
			remoteAddr: "sock:1",
			want:       "sock",
		},
		{
			// net/http's TCP listeners always give host:port, but a peer
			// handed over with no port still goes through addressKey.
			name:       "portless ipv6 peer is cut to its /64",
			remoteAddr: "2001:db8:1:2:aaaa:bbbb:cccc:dddd",
			want:       "2001:db8:1:2::",
		},
		{
			name:       "portless ipv4 peer is unchanged",
			remoteAddr: "203.0.113.7",
			want:       "203.0.113.7",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remoteAddr
			for _, value := range tc.forwarded {
				r.Header.Add("X-Forwarded-For", value)
			}
			if got := clientIP(r, tc.hops); got != tc.want {
				t.Errorf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
