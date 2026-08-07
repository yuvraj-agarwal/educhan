package app

import (
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
		wantPrefix  string
	}{
		{name: "empty defaults to anonymous", field: "", wantName: anonymousName},
		{name: "whitespace defaults to anonymous", field: "   ", wantName: anonymousName},
		{name: "plain name is trimmed", field: "  ana  ", wantName: "ana"},
		{name: "insecure tripcode", field: "ana#secret", wantName: "ana", wantTripSet: true, wantPrefix: "!"},
		{name: "secure tripcode", field: "ana##secret", wantName: "ana", wantTripSet: true, wantPrefix: "!!"},
		{name: "tripcode without a name", field: "#secret", wantName: anonymousName, wantTripSet: true, wantPrefix: "!"},
		{name: "empty secret is ignored", field: "ana#", wantName: "ana"},
		{name: "blank secret is ignored", field: "ana#   ", wantName: "ana"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseIdentity(tc.field, salt)
			if got.Name != tc.wantName {
				t.Errorf("name = %q, want %q", got.Name, tc.wantName)
			}
			if tc.wantTripSet {
				if got.Tripcode == "" {
					t.Fatal("expected a tripcode")
				}
				if !strings.HasPrefix(got.Tripcode, tc.wantPrefix) {
					t.Errorf("tripcode %q does not start with %q", got.Tripcode, tc.wantPrefix)
				}
			} else if got.Tripcode != "" {
				t.Errorf("unexpected tripcode %q", got.Tripcode)
			}
		})
	}
}

func TestParseIdentityTripcodeStability(t *testing.T) {
	insecure := parseIdentity("ana#secret", "salt-a").Tripcode
	if other := parseIdentity("bob#secret", "salt-b").Tripcode; other != insecure {
		t.Errorf("insecure tripcodes must not depend on name or salt: %q != %q", other, insecure)
	}
	if changed := parseIdentity("ana#other", "salt-a").Tripcode; changed == insecure {
		t.Error("different secrets must produce different tripcodes")
	}

	secure := parseIdentity("ana##secret", "salt-a").Tripcode
	if same := parseIdentity("ana##secret", "salt-b").Tripcode; same == secure {
		t.Error("secure tripcodes must depend on the server salt")
	}
	if secure == "!"+strings.TrimPrefix(insecure, "!") {
		t.Error("secure and insecure tripcodes must differ for the same secret")
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
