package app

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The allowlist is a file rather than a table, and the reason is the privacy
// property: the mapping from a domain to a badge is configuration, not data
// about anybody, so keeping it out of the database keeps the database down to
// the two tables 0004 documents. It also means the list can be reviewed in a
// diff, which is where a decision about who counts as an institution belongs.

//go:embed verify_domains.txt
var defaultVerifyDomains string

// Bounds on what a badge may say. The label is rendered next to a post, so it
// is held to the same shape as a board name.
const maxBadgeLabelLen = 64

// maxEmailLen is the longest address RFC 5321 allows through a mail path.
const maxEmailLen = 254

// maxEmailLocalLen is the longest local part the same RFC allows.
const maxEmailLocalLen = 64

// hostLabel is one label of a hostname: lowercase alphanumerics, with single
// dashes allowed inside.
const hostLabel = `[a-z0-9]([a-z0-9-]*[a-z0-9])?`

// domainPattern is a mail domain, which always has a name and a namespace:
// "iitb.ac.in", never "in". It applies to submitted addresses and to
// institution entries, both of which name somewhere specific.
//
// suffixPattern is the laxer shape an academic namespace takes, because the
// oldest of them is a single label: ".edu".
var (
	domainPattern = regexp.MustCompile(`^` + hostLabel + `(\.` + hostLabel + `)+$`)
	suffixPattern = regexp.MustCompile(`^` + hostLabel + `(\.` + hostLabel + `)*$`)
)

// domainAllowlist decides whether an address belongs to an educational
// institution, and what the badge for it says.
type domainAllowlist struct {
	// institutions maps a domain to the badge its addresses earn. A subdomain
	// inherits its parent's entry.
	institutions map[string]string
	// suffixes are academic namespaces such as "ac.in". An address under one is
	// accepted and badged with its own domain.
	suffixes map[string]bool
	// denied are domains refused outright, with every subdomain, whatever else
	// matches: school namespaces that sit inside an academic suffix, such as
	// "nsw.edu.au" inside "edu.au".
	denied map[string]bool
}

// loadDomainAllowlist reads the allowlist. An empty path takes the embedded
// default; a path replaces it rather than adding to it.
func loadDomainAllowlist(path string) (*domainAllowlist, error) {
	source := defaultVerifyDomains
	if path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		source = string(body)
	}
	list, err := parseDomainAllowlist(source)
	if err != nil && path != "" {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return list, err
}

func parseDomainAllowlist(source string) (*domainAllowlist, error) {
	list := &domainAllowlist{
		institutions: map[string]string{},
		suffixes:     map[string]bool{},
		denied:       map[string]bool{},
	}

	for n, line := range strings.Split(source, "\n") {
		if comment := strings.IndexByte(line, '#'); comment >= 0 {
			line = line[:comment]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}

		if denied, isDeny := strings.CutPrefix(strings.TrimSpace(line), "!"); isDeny {
			domain := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(denied), ".")))
			if !domainPattern.MatchString(domain) {
				return nil, fmt.Errorf("line %d: %q is not a domain to deny", n+1, domain)
			}
			list.denied[domain] = true
			continue
		}

		entry, label, isInstitution := strings.Cut(line, "=")
		domain := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(entry), ".")))

		if !isInstitution {
			if !suffixPattern.MatchString(domain) {
				return nil, fmt.Errorf("line %d: %q is not a domain suffix", n+1, domain)
			}
			list.suffixes[domain] = true
			continue
		}
		if !domainPattern.MatchString(domain) {
			return nil, fmt.Errorf("line %d: %q is not a domain", n+1, domain)
		}
		label = strings.TrimSpace(collapseControl(label))
		switch {
		case label == "":
			return nil, fmt.Errorf("line %d: %s has an empty label", n+1, domain)
		case utf8.RuneCountInString(label) > maxBadgeLabelLen:
			return nil, fmt.Errorf("line %d: %s has a label longer than %d characters",
				n+1, domain, maxBadgeLabelLen)
		}
		list.institutions[domain] = label
	}

	if len(list.institutions) == 0 && len(list.suffixes) == 0 {
		return nil, errors.New("no domains configured")
	}
	return list, nil
}

// lookup resolves the badge an address at domain earns. It reports false when
// the domain is not recognised as academic.
func (l *domainAllowlist) lookup(domain string) (string, bool) {
	// Institutions win over suffixes whatever their relative length, so they
	// get a pass of their own rather than being interleaved. Within a pass the
	// longest match wins, which is what makes a subdomain follow its parent.
	parts := strings.Split(domain, ".")
	// A denial beats everything, so it is checked across the whole domain
	// before anything is allowed.
	for i := range parts {
		if l.denied[strings.Join(parts[i:], ".")] {
			return "", false
		}
	}
	for i := range parts {
		if label, ok := l.institutions[strings.Join(parts[i:], ".")]; ok {
			return label, true
		}
	}
	for i := range parts {
		// An address sitting directly on a suffix has no institution below it
		// to name, which is what i > 0 refuses: the badge is always the suffix
		// plus one more label.
		if candidate := strings.Join(parts[i:], "."); l.suffixes[candidate] && i > 0 {
			return strings.Join(parts[i-1:], "."), true
		}
	}
	return "", false
}

// address is an institutional email address, split into the two forms educhan
// uses. The address itself is never stored in either form; see verify.go.
type address struct {
	// Delivery is where the code is sent: the address as given, lowercased.
	Delivery string
	// Key is what the uniqueness HMAC is computed over. Plus-addressing is
	// stripped, so one mailbox cannot mint a second identity by appending a
	// tag. Dots are deliberately left alone: collapsing them is a Gmail
	// convention, and applying it everywhere would fold two real people at an
	// institution into one.
	Key string
	// Domain is the part after the "@", for the allowlist check.
	Domain string
}

// parseEmail validates and normalises a submitted address. The error is written
// straight back to the client, so it says what is wrong without echoing input.
func parseEmail(raw string) (address, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case normalized == "":
		return address{}, errors.New("email is required")
	case len(normalized) > maxEmailLen:
		return address{}, fmt.Errorf("email must be at most %d characters", maxEmailLen)
	case strings.ContainsAny(normalized, " \t\r\n"), normalized != collapseControl(normalized):
		return address{}, errors.New("email must not contain spaces")
	}

	local, domain, ok := strings.Cut(normalized, "@")
	switch {
	case !ok, strings.Contains(domain, "@"):
		return address{}, errors.New("email must be one address, in the form you@institution.edu")
	case local == "":
		return address{}, errors.New("email is missing the part before the @")
	case len(local) > maxEmailLocalLen:
		return address{}, fmt.Errorf("the part before the @ must be at most %d characters", maxEmailLocalLen)
	case !domainPattern.MatchString(domain):
		return address{}, errors.New("email has no usable domain after the @")
	}

	key := local
	if tag := strings.IndexByte(key, '+'); tag >= 0 {
		key = key[:tag]
	}
	if key == "" {
		return address{}, errors.New("email is missing the part before the @")
	}
	return address{Delivery: normalized, Key: key + "@" + domain, Domain: domain}, nil
}
