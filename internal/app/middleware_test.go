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
			name:       "bracketed ipv6",
			remoteAddr: "10.0.0.1:41000",
			forwarded:  []string{"[2001:db8::1]:443"},
			hops:       1,
			want:       "2001:db8::1",
		},
		{
			name:       "bare ipv6",
			remoteAddr: "10.0.0.1:41000",
			forwarded:  []string{"2001:db8::1"},
			hops:       1,
			want:       "2001:db8::1",
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
