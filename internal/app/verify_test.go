package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/mailer"
)

func testAllowlist(t *testing.T) *domainAllowlist {
	t.Helper()
	list, err := loadDomainAllowlist("")
	if err != nil {
		t.Fatalf("load embedded allowlist: %v", err)
	}
	return list
}

func TestDomainAllowlistLookup(t *testing.T) {
	list := testAllowlist(t)

	cases := []struct {
		domain string
		want   string
	}{
		// An institution entry names the institution, and a subdomain of one
		// inherits it.
		{domain: "iitb.ac.in", want: "IIT Bombay"},
		{domain: "cse.iitb.ac.in", want: "IIT Bombay"},
		{domain: "mit.edu", want: "MIT"},
		// A suffix match badges the domain one level below the suffix, so two
		// departments of one university read the same.
		{domain: "nitk.ac.in", want: "nitk.ac.in"},
		{domain: "cse.nitk.ac.in", want: "nitk.ac.in"},
		{domain: "example.edu", want: "example.edu"},
		{domain: "sub.dept.example.edu", want: "example.edu"},
		{domain: "warwick.ac.uk", want: "warwick.ac.uk"},
		{domain: "unsw.edu.au", want: "unsw.edu.au"},
		// Not academic.
		{domain: "gmail.com", want: ""},
		{domain: "iitb.ac.in.example.com", want: ""},
		{domain: "notiitb.ac.in", want: "notiitb.ac.in"},
		// A namespace is not an institution: nobody is verified as ".ac.in".
		{domain: "ac.in", want: ""},
		{domain: "edu", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.domain, func(t *testing.T) {
			label, ok := list.lookup(tc.domain)
			if tc.want == "" {
				if ok {
					t.Errorf("%s was accepted as %q, want rejected", tc.domain, label)
				}
				return
			}
			if !ok {
				t.Fatalf("%s was rejected, want %q", tc.domain, tc.want)
			}
			if label != tc.want {
				t.Errorf("%s = %q, want %q", tc.domain, label, tc.want)
			}
		})
	}
}

// An institution entry beats a suffix entry even when the suffix is the longer
// match, so listing an institution can never make its badge worse.
func TestDomainAllowlistPrefersInstitutions(t *testing.T) {
	list, err := parseDomainAllowlist("ac.in\nexample.ac.in\nuni.ac.in = Uni\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if label, _ := list.lookup("dept.uni.ac.in"); label != "Uni" {
		t.Errorf("label = %q, want Uni", label)
	}
	if label, _ := list.lookup("dept.example.ac.in"); label != "dept.example.ac.in" {
		t.Errorf("longest suffix should win among suffixes, got %q", label)
	}
}

func TestDomainAllowlistRejectsBadFiles(t *testing.T) {
	cases := map[string]string{
		"not a domain":             "hello world\n",
		"empty label":              "iitb.ac.in =\n",
		"long label":               "iitb.ac.in = " + strings.Repeat("x", maxBadgeLabelLen+1) + "\n",
		"nothing at all":           "# only comments\n\n",
		"single-label institution": "edu = Everyone\n",
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDomainAllowlist(source); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// A configured file replaces the built-in list rather than extending it, so a
// deployment sees exactly the domains it wrote down.
func TestDomainAllowlistFileReplacesTheDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "domains.txt")
	if err := os.WriteFile(path, []byte("# ours only\nuni.example = Example University\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	list, err := loadDomainAllowlist(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if label, ok := list.lookup("uni.example"); !ok || label != "Example University" {
		t.Errorf("configured domain = %q/%v, want Example University", label, ok)
	}
	if _, ok := list.lookup("mit.edu"); ok {
		t.Error("the embedded list should not be merged into a configured one")
	}
}

func TestParseEmail(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		key     string
		domain  string
		wantErr bool
	}{
		{name: "plain", raw: "Ana@IITB.ac.in", key: "ana@iitb.ac.in", domain: "iitb.ac.in"},
		{name: "padded", raw: "  ana@iitb.ac.in  ", key: "ana@iitb.ac.in", domain: "iitb.ac.in"},
		// Plus-addressing is stripped for the uniqueness key: one mailbox, one
		// identity, however many tags it can invent.
		{name: "plus tag", raw: "ana+educhan@iitb.ac.in", key: "ana@iitb.ac.in", domain: "iitb.ac.in"},
		// Dots are not, because collapsing them would fold two real people at
		// an institution into one.
		{name: "dotted", raw: "a.na@iitb.ac.in", key: "a.na@iitb.ac.in", domain: "iitb.ac.in"},
		{name: "empty", raw: "   ", wantErr: true},
		{name: "no at", raw: "ana.iitb.ac.in", wantErr: true},
		{name: "two at", raw: "ana@a@iitb.ac.in", wantErr: true},
		{name: "no local part", raw: "@iitb.ac.in", wantErr: true},
		{name: "only a tag", raw: "+tag@iitb.ac.in", wantErr: true},
		{name: "bare domain", raw: "ana@localhost", wantErr: true},
		{name: "spaces", raw: "an a@iitb.ac.in", wantErr: true},
		{name: "too long", raw: strings.Repeat("a", maxEmailLen) + "@iitb.ac.in", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, err := parseEmail(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseEmail(%q) = %+v, want an error", tc.raw, addr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEmail(%q): %v", tc.raw, err)
			}
			if addr.Key != tc.key || addr.Domain != tc.domain {
				t.Errorf("parseEmail(%q) = %q/%q, want %q/%q", tc.raw, addr.Key, addr.Domain, tc.key, tc.domain)
			}
			if addr.Delivery != strings.ToLower(strings.TrimSpace(tc.raw)) {
				t.Errorf("delivery address = %q, want the address as given", addr.Delivery)
			}
		})
	}
}

// Two addresses that differ only by a plus tag are one identity; two that
// differ anywhere else are two.
func TestEmailHashIdentity(t *testing.T) {
	a := &App{cfg: Config{VerifySecret: "secret"}}

	same := []string{"ana@iitb.ac.in", "ANA@iitb.ac.in", "ana+one@iitb.ac.in", "ana+two@iitb.ac.in"}
	want := a.emailHash(mustParseEmail(t, same[0]).Key)
	for _, raw := range same[1:] {
		if got := a.emailHash(mustParseEmail(t, raw).Key); !bytes.Equal(got, want) {
			t.Errorf("%s should hash to the same key as %s", raw, same[0])
		}
	}
	for _, raw := range []string{"an.a@iitb.ac.in", "ana@iitd.ac.in", "anab@iitb.ac.in"} {
		if got := a.emailHash(mustParseEmail(t, raw).Key); bytes.Equal(got, want) {
			t.Errorf("%s should not hash to the same key as %s", raw, same[0])
		}
	}

	// The secret keys the hash, so a database taken without it cannot be tested
	// against a list of addresses.
	other := &App{cfg: Config{VerifySecret: "another"}}
	if bytes.Equal(other.emailHash(mustParseEmail(t, same[0]).Key), want) {
		t.Error("the email hash must depend on EDUCHAN_VERIFY_SECRET")
	}
}

func TestCredentialRoundTrip(t *testing.T) {
	a := &App{cfg: Config{VerifySecret: "secret"}}
	issued := credential{Label: "IIT Bombay", ID: "abcdef0123456789", Expires: time.Now().Add(time.Hour).Unix()}

	token, err := a.signCredential(issued)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// The claims are readable by their holder, so the one thing that must not
	// be in there is anything about the address.
	if strings.Contains(token, "@") {
		t.Errorf("credential %q looks like it carries an address", token)
	}

	parsed, ok := a.parseCredential(token)
	if !ok {
		t.Fatal("a freshly signed credential should parse")
	}
	if parsed != issued {
		t.Errorf("parsed = %+v, want %+v", parsed, issued)
	}

	tampered := []string{
		"",
		"garbage",
		token[:len(token)-1],
		strings.TrimPrefix(token, credentialVersion+"."),
		"v2." + strings.TrimPrefix(token, credentialVersion+"."),
	}
	for _, bad := range tampered {
		if _, ok := a.parseCredential(bad); ok {
			t.Errorf("parseCredential(%q) accepted a credential it should not", bad)
		}
	}

	// A credential signed with another secret is somebody else's.
	other := &App{cfg: Config{VerifySecret: "another"}}
	if _, ok := other.parseCredential(token); ok {
		t.Error("a credential must not verify under a different secret")
	}
}

// The stored expiry is deliberately coarse: it dates an issuance to the day
// rather than the second, and still outlives the credential it revokes.
func TestNextDayBoundary(t *testing.T) {
	at := time.Date(2026, 8, 8, 14, 21, 33, 500, time.UTC)
	got := nextDayBoundary(at)
	want := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextDayBoundary(%s) = %s, want %s", at, got, want)
	}
	if !got.After(at) {
		t.Error("the stored expiry must outlive the credential it belongs to")
	}
	if got.Sub(at) > 24*time.Hour {
		t.Errorf("rounding added %s, want less than a day", got.Sub(at))
	}
}

func TestVerificationCodeShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		code, err := verificationCode()
		if err != nil {
			t.Fatalf("verificationCode: %v", err)
		}
		if len(code) != verifyCodeDigits {
			t.Fatalf("code %q is %d digits, want %d", code, len(code), verifyCodeDigits)
		}
		if strings.Trim(code, "0123456789") != "" {
			t.Fatalf("code %q is not all digits", code)
		}
		seen[code] = true
	}
	if len(seen) < 40 {
		t.Errorf("50 codes produced %d distinct values; they should not repeat", len(seen))
	}
}

func TestDevSenderPrintsTheCode(t *testing.T) {
	var out bytes.Buffer
	sender := mailer.NewDev(&out)
	err := sender.SendCode(context.Background(), mailer.Code{
		Address: "ana@iitb.ac.in",
		Code:    "123456",
		TTL:     15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("SendCode: %v", err)
	}
	for _, want := range []string{"ana@iitb.ac.in", "123456", "15m"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("printed %q, want it to contain %q", out.String(), want)
		}
	}
}

/* Helpers ------------------------------------------------------------------- */

func mustParseEmail(t *testing.T, raw string) address {
	t.Helper()
	addr, err := parseEmail(raw)
	if err != nil {
		t.Fatalf("parseEmail(%q): %v", raw, err)
	}
	return addr
}
