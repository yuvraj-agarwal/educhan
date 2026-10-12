package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

	first := posterID(salt, "10.0.0.1", 7)
	if len(first) != posterIDLen {
		t.Fatalf("poster ID %q length = %d, want %d", first, len(first), posterIDLen)
	}
	if again := posterID(salt, "10.0.0.1", 7); again != first {
		t.Errorf("poster ID is not stable: %q != %q", again, first)
	}
	if otherThread := posterID(salt, "10.0.0.1", 8); otherThread == first {
		t.Error("poster IDs must differ between threads")
	}
	if otherClient := posterID(salt, "10.0.0.2", 7); otherClient == first {
		t.Error("poster IDs must differ between clients")
	}
	if otherSalt := posterID("other", "10.0.0.1", 7); otherSalt == first {
		t.Error("poster IDs must depend on the salt")
	}
	for _, r := range first {
		if !strings.ContainsRune(posterIDAlpha, r) {
			t.Errorf("poster ID %q contains unexpected rune %q", first, r)
		}
	}
}
