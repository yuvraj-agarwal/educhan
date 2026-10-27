package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// TestDomainAllowlistRefusesSchoolNamespaces pins the current badge scope in
// the embedded list: school addresses that share a namespace with universities do
// not verify, and the universities beside them still do.
func TestDomainAllowlistRefusesSchoolNamespaces(t *testing.T) {
	list := testAllowlist(t)

	for _, domain := range []string{
		"school.sch.uk",
		"students.edu.sg",
		"x.students.edu.sg",
		"rafflesgirlssch.edu.sg",
		"barker.nsw.edu.au",
		"school.vic.edu.au",
		"moe-dl.edu.my",
		"school.edu.hk",
		"liceo.edu.it",
		"someschool.edu.in",
		"x.kvs.ac.in",
		"kvs.ac.in",
		"someschool.ac.th",
		"kosen.ac.jp",
		"univie.ac.at",
	} {
		if label, ok := list.lookup(domain); ok {
			t.Errorf("%s was accepted as %q, want refused", domain, label)
		}
	}

	for domain, want := range map[string]string{
		"unsw.edu.au":            "unsw.edu.au",
		"student.unimelb.edu.au": "University of Melbourne",
		"e.ntu.edu.sg":           "Nanyang Technological University",
		"u.nus.edu":              "National University of Singapore",
		"connect.hku.hk":         "University of Hong Kong",
		"link.cuhk.edu.hk":       "Chinese University of Hong Kong",
		"um.edu.my":              "um.edu.my",
		"nitk.ac.in":             "nitk.ac.in",
		"cse.iitb.ac.in":         "IIT Bombay",
		"ashoka.edu.in":          "Ashoka University",
		"g.ecc.u-tokyo.ac.jp":    "University of Tokyo",
		"student.chula.ac.th":    "Chulalongkorn University",
	} {
		if label, ok := list.lookup(domain); !ok || label != want {
			t.Errorf("%s = %q/%v, want %q", domain, label, ok, want)
		}
	}
}

// A denial wins over every other kind of entry, and covers its subdomains.
func TestDomainAllowlistDenials(t *testing.T) {
	list, err := parseDomainAllowlist("edu\nuni.edu = Uni\n!school.edu\n!labs.uni.edu # comment\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, domain := range []string{"school.edu", "class.school.edu", "labs.uni.edu", "x.labs.uni.edu"} {
		if label, ok := list.lookup(domain); ok {
			t.Errorf("%s was accepted as %q, want refused", domain, label)
		}
	}
	if label, _ := list.lookup("cs.uni.edu"); label != "Uni" {
		t.Errorf("cs.uni.edu = %q, want Uni", label)
	}
	if label, _ := list.lookup("college.edu"); label != "college.edu" {
		t.Errorf("college.edu = %q, want college.edu", label)
	}
}

func TestDomainAllowlistRejectsBadFiles(t *testing.T) {
	cases := map[string]string{
		"single-label denial":      "edu\n!edu\n",
		"empty denial":             "edu\n!\n",
		"denial with a label":      "edu\n!school.edu = School\n",
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

// The stored expiry is deliberately coarse: it is counted from the end of the
// credential day an issuance falls in, so it dates the issuance to that day
// rather than the second, and still outlives the credential it revokes. A day
// runs from 05:00 UTC to 05:00 UTC, whatever the date and whatever the
// issuance's time zone.
func TestCredentialDayEnd(t *testing.T) {
	india := time.FixedZone("IST", 5*3600+1800)
	for _, c := range []struct {
		at, want time.Time
	}{
		{time.Date(2026, 8, 8, 14, 21, 33, 500, time.UTC), time.Date(2026, 8, 9, 5, 0, 0, 0, time.UTC)},
		{time.Date(2026, 8, 8, 4, 59, 59, 999999999, time.UTC), time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC)},
		{time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC), time.Date(2026, 8, 9, 5, 0, 0, 0, time.UTC)},
		{time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC)},
		{time.Date(2026, 8, 8, 3, 17, 0, 0, time.UTC), time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 5, 0, 0, 0, time.UTC)},
		{time.Date(2028, 2, 28, 6, 0, 0, 0, time.UTC), time.Date(2028, 2, 29, 5, 0, 0, 0, time.UTC)},
		// 09:00 in India is 03:30 UTC, before the day's end that morning.
		{time.Date(2026, 8, 8, 9, 0, 0, 0, india), time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC)},
	} {
		got := credentialDayEnd(c.at)
		if !got.Equal(c.want) || got.Location() != time.UTC {
			t.Errorf("credentialDayEnd(%s) = %s, want %s", c.at, got, c.want)
		}
		if !got.After(c.at) {
			t.Errorf("credentialDayEnd(%s) = %s: the stored expiry must outlive the credential it belongs to", c.at, got)
		}
		if got.Sub(c.at) > 24*time.Hour {
			t.Errorf("credentialDayEnd(%s) added %s, want no more than a day", c.at, got.Sub(c.at))
		}
	}

	// Every moment of one day has the same end, and the next day another.
	start := time.Date(2026, 10, 26, 5, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	for at := start; at.Before(end); at = at.Add(7 * time.Minute) {
		if got := credentialDayEnd(at); !got.Equal(end) {
			t.Fatalf("credentialDayEnd(%s) = %s, want %s like the rest of its day", at, got, end)
		}
	}
	if got := credentialDayEnd(end); !got.Equal(end.Add(24 * time.Hour)) {
		t.Errorf("credentialDayEnd(%s) = %s, want the next day's end", end, got)
	}
}

// The nightly sweep gives every credential row its transaction id, and a row
// issued after it keeps the issuance's until the next night's. So the day a
// row's expiry gives must end after the sweep, not before it: a day that
// ended at midnight, with the sweep at 03:17, had its rows from midnight to
// the sweep carry the sweep's id and the rest their own, and the expiry and
// the sweep's id together dated the first to those three hours, until the
// next sweep. The test reads when the sweep is scheduled, and how long it and
// its retry can run, from the Cloud Scheduler job and Cloud Run Job that the
// deploy guide and first-deploy.sh each set up, and checks that the day the
// sweep runs in ends after the latest of them, with half an hour to spare for
// the Job's starts, and within two hours of the scheduled time: then the rows
// of that day carrying the sweep's id were issued over the 22 hours or more
// before it, and those issued after it, which carry their own ids anyway,
// over two hours at most.
func TestCredentialDaysEndAfterTheSweep(t *testing.T) {
	for _, path := range []string{"deploy/README.md", "deploy/first-deploy.sh"} {
		t.Run(path, func(t *testing.T) {
			setup := repositoryFile(t, path)
			schedule := regexp.MustCompile(`--schedule "(\d+) (\d+) \* \* \*" --time-zone "Etc/UTC"`).
				FindStringSubmatch(setup)
			job := regexp.MustCompile(`--max-retries (\d+) --task-timeout (\d+)m\b`).FindStringSubmatch(setup)
			if schedule == nil || job == nil {
				t.Fatalf("%s has no daily UTC schedule (%q) or no Job retries and timeout (%q) for the sweep: "+
					"the test cannot tell when it runs", path, schedule, job)
			}
			number := func(s string) time.Duration {
				n, err := strconv.Atoi(s)
				if err != nil {
					t.Fatalf("%q: %v", s, err)
				}
				return time.Duration(n)
			}
			scheduled := number(schedule[2])*time.Hour + number(schedule[1])*time.Minute
			latest := scheduled + (number(job[1])+1)*number(job[2])*time.Minute

			for _, date := range []time.Time{
				time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC),
				time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
				time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
			} {
				dayEnd := credentialDayEnd(date.Add(scheduled))
				if spare := dayEnd.Sub(date.Add(latest)); spare < 30*time.Minute {
					t.Errorf("a credential day ends at %s, %s after the latest a sweep scheduled at %s and "+
						"retried can still be running (%s): want it to end half an hour or more after that, or "+
						"the sweep splits it into the credentials issued before it and those after",
						dayEnd.Format(time.RFC3339), spare, date.Add(scheduled).Format(time.RFC3339),
						date.Add(latest).Format(time.RFC3339))
				}
				if after := dayEnd.Sub(date.Add(scheduled)); after > 2*time.Hour {
					t.Errorf("the day the sweep scheduled at %s runs in ends %s after it: want two hours at "+
						"most, so that the credentials of that day carrying its id are those issued over the "+
						"rest of the day", date.Add(scheduled).Format(time.RFC3339), after)
				}
				// Each moment a scheduled sweep or its retry may run in falls in that day.
				for at := date.Add(scheduled); !at.After(date.Add(latest)); at = at.Add(time.Minute) {
					if got := credentialDayEnd(at); !got.Equal(dayEnd) {
						t.Fatalf("credentialDayEnd(%s) = %s, want %s: a day ends while the sweep can be running",
							at.Format(time.RFC3339), got.Format(time.RFC3339), dayEnd.Format(time.RFC3339))
					}
				}
			}
		})
	}
}

// repositoryFile reads a file of the repository by its path from the root,
// which is the directory above the test's that holds go.mod.
func repositoryFile(t *testing.T, path string) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("no go.mod above the test's directory")
		}
		root = parent
	}
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
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

// TestStartupRefusesTheDevSenderByDefault: with verification on and no mail
// provider, the server must not quietly print addresses and codes into its
// log. Only an explicit EDUCHAN_MAIL_DEV allows that.
func TestStartupRefusesTheDevSenderByDefault(t *testing.T) {
	// New fails before it touches the database, so an unreachable one is fine.
	cfg := Config{DatabaseURL: "postgres://nobody@127.0.0.1:1/none", VerifySecret: "secret"}
	_, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "EDUCHAN_MAIL_DEV") {
		t.Fatalf("New = %v, want a refusal naming EDUCHAN_MAIL_DEV", err)
	}

	for name, ok := range map[string]Config{
		"verification off":       {},
		"dev sender asked for":   {VerifySecret: "secret", MailDev: true},
		"real sender configured": {VerifySecret: "secret", Sender: mailer.NewDev(io.Discard)},
	} {
		if err := checkMailer(ok); err != nil {
			t.Errorf("%s: checkMailer = %v, want nil", name, err)
		}
	}
}
