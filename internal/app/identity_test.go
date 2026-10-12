package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseIdentity(t *testing.T) {
	const salt = "test-salt"

	tests := []struct {
		name        string
		field       string
		wantName    string
		wantTripSet bool
	}{
		{name: "empty defaults to anonymous", field: "", wantName: anonymousName},
		{name: "whitespace defaults to anonymous", field: "   ", wantName: anonymousName},
		{name: "plain name is trimmed", field: "  ana  ", wantName: "ana"},
		{name: "single hash tripcode", field: "ana#secret", wantName: "ana", wantTripSet: true},
		{name: "double hash tripcode", field: "ana##secret", wantName: "ana", wantTripSet: true},
		{name: "tripcode without a name", field: "#secret", wantName: anonymousName, wantTripSet: true},
		{name: "double hash without a name", field: "##secret", wantName: anonymousName, wantTripSet: true},
		{name: "empty secret is ignored", field: "ana#", wantName: "ana"},
		{name: "empty double hash secret is ignored", field: "ana##", wantName: "ana"},
		{name: "blank secret is ignored", field: "ana#   ", wantName: "ana"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseIdentity(tc.field, salt)
			if got.Name != tc.wantName {
				t.Errorf("name = %q, want %q", got.Name, tc.wantName)
			}
			if !tc.wantTripSet {
				if got.Tripcode != "" {
					t.Errorf("unexpected tripcode %q", got.Tripcode)
				}
				return
			}
			if len(got.Tripcode) != len(tripcodePrefix)+tripcodeLen || !strings.HasPrefix(got.Tripcode, "!!") {
				t.Errorf("tripcode %q, want \"!!\" and %d characters", got.Tripcode, tripcodeLen)
			}
		})
	}
}

// The unkeyed "!" tripcode is gone: an unsalted hash of a short secret can be
// cracked from the tripcode alone, so both conventions make the salted one.
func TestParseIdentityTripcodesAreAllSalted(t *testing.T) {
	single := parseIdentity("ana#secret", "salt-a").Tripcode
	double := parseIdentity("ana##secret", "salt-a").Tripcode
	if single != double {
		t.Errorf("#secret gave %q and ##secret gave %q; they must be one tripcode", single, double)
	}
	if bare := parseIdentity("#secret", "salt-a").Tripcode; bare != single {
		t.Errorf("a tripcode must not depend on the name: %q != %q", bare, single)
	}
	if other := parseIdentity("bob#secret", "salt-a").Tripcode; other != single {
		t.Errorf("a tripcode must not depend on the name: %q != %q", other, single)
	}
	if changed := parseIdentity("ana#other", "salt-a").Tripcode; changed == single {
		t.Error("different secrets must produce different tripcodes")
	}
	for _, field := range []string{"ana#secret", "ana##secret"} {
		if resalted := parseIdentity(field, "salt-b").Tripcode; resalted == single {
			t.Errorf("%s: the tripcode must depend on the server salt", field)
		}
	}

	// Pinned to the derivation secure tripcodes have always had, computed
	// here independently, so a "##secret" tripcode from before the change is
	// unchanged by it.
	mac := hmac.New(sha256.New, []byte("salt-a"))
	mac.Write([]byte("trip:secret"))
	if want := "!!" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:tripcodeLen]; double != want {
		t.Errorf("##secret = %q, want %q: the secure derivation must not move", double, want)
	}
}

func TestParseIdentityClampsLongName(t *testing.T) {
	got := parseIdentity(strings.Repeat("a", maxNameLen+40), "salt")
	if len(got.Name) != maxNameLen {
		t.Errorf("name length = %d, want %d", len(got.Name), maxNameLen)
	}
}

func TestPosterID(t *testing.T) {
	const salt = "salt"
	source := addressSource("10.0.0.1")

	first := posterID(salt, source, 7)
	if len(first) != posterIDLen {
		t.Fatalf("poster ID %q length = %d, want %d", first, len(first), posterIDLen)
	}
	if again := posterID(salt, source, 7); again != first {
		t.Errorf("poster ID is not stable: %q != %q", again, first)
	}
	if otherThread := posterID(salt, source, 8); otherThread == first {
		t.Error("poster IDs must differ between threads")
	}
	if otherClient := posterID(salt, addressSource("10.0.0.2"), 7); otherClient == first {
		t.Error("poster IDs must differ between clients")
	}
	if otherSalt := posterID("other", source, 7); otherSalt == first {
		t.Error("poster IDs must depend on the salt")
	}
	for _, r := range first {
		if !strings.ContainsRune(posterIDAlpha, r) {
			t.Errorf("poster ID %q contains unexpected rune %q", first, r)
		}
	}
}

// The three sources are tagged so that equal values from different sources
// never meet in the HMAC: a client choosing its own poster key must not be
// able to land on the ID an address or a credential gets.
func TestPosterSourcesNeverCollide(t *testing.T) {
	const (
		salt  = "salt"
		value = "AAAAAAAAAAAAAAAAAAAAAA"
	)
	ids := map[string]string{
		"key":  posterID(salt, posterKeySource(value), 7),
		"ip":   posterID(salt, addressSource(value), 7),
		"cred": posterID(salt, credentialSource(value), 7),
	}
	seen := map[string]string{}
	for source, id := range ids {
		if other, ok := seen[id]; ok {
			t.Errorf("%s and %s sources gave the same poster ID %q for one value", source, other, id)
		}
		seen[id] = source
	}
}

func TestPosterSourceFor(t *testing.T) {
	const key, ip = "AAAAAAAAAAAAAAAAAAAAAA", "203.0.113.9"
	badged := badgeGrant{Label: "IIT Bombay", CredentialID: "random-credential-id"}

	tests := []struct {
		name    string
		granted badgeGrant
		key     string
		want    posterSource
	}{
		{name: "badged post uses the credential, not the key", granted: badged, key: key, want: credentialSource(badged.CredentialID)},
		{name: "badged post without a key", granted: badged, want: credentialSource(badged.CredentialID)},
		{name: "unbadged post uses the key", key: key, want: posterKeySource(key)},
		{name: "no key falls back to the address", want: addressSource(ip)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := posterSourceFor(tc.granted, tc.key, ip); got != tc.want {
				t.Errorf("source = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPosterKeyHeader(t *testing.T) {
	const valid = "abcdefghijklmnopqrst-_"

	tests := []struct {
		name    string
		values  []string
		wantKey string
		wantOK  bool
	}{
		{name: "absent", wantOK: true},
		{name: "well formed", values: []string{valid}, wantKey: valid, wantOK: true},
		{name: "every alphabet class", values: []string{"AZaz09-_AZaz09-_AZaz09"}, wantKey: "AZaz09-_AZaz09-_AZaz09", wantOK: true},
		{name: "empty", values: []string{""}},
		{name: "one short", values: []string{valid[:21]}},
		{name: "one long", values: []string{valid + "A"}},
		{name: "padded", values: []string{valid[:20] + "=="}},
		{name: "standard base64 plus", values: []string{valid[:21] + "+"}},
		{name: "standard base64 slash", values: []string{valid[:21] + "/"}},
		{name: "space", values: []string{valid[:21] + " "}},
		{name: "non-ASCII", values: []string{valid[:20] + "é"}},
		{name: "sent twice", values: []string{valid, valid}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/boards/general/threads", nil)
			for _, v := range tc.values {
				r.Header.Add(posterKeyHeader, v)
			}
			w := httptest.NewRecorder()
			key, ok := posterKey(w, r)
			if ok != tc.wantOK || key != tc.wantKey {
				t.Fatalf("posterKey = (%q, %v), want (%q, %v)", key, ok, tc.wantKey, tc.wantOK)
			}
			if !ok {
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalid poster key"`) {
					t.Errorf("refusal = %d %s, want 400 invalid poster key", w.Code, w.Body.String())
				}
			}
		})
	}
}
