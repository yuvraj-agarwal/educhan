package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
)

// anonymousName is the display name used when a poster supplies none, which is
// the default and by far the common case.
const anonymousName = "Anonymous"

const (
	maxNameLen    = 64
	tripcodeLen   = 10
	posterIDLen   = 8
	posterIDAlpha = "0123456789abcdefghijklmnopqrstuvwxyz"
	// tripcodePrefix marks a tripcode. It is "!!", the prefix imageboards use
	// for a secure (server-keyed) tripcode, because that is the only kind
	// educhan makes: a single "!" would advertise the classic unkeyed kind,
	// which can be cracked offline from the tripcode alone.
	tripcodePrefix = "!!"
)

// identity is the public, non-account-based identity attached to a post.
type identity struct {
	Name     string
	Tripcode string
}

// parseIdentity splits the free-form name field into a display name and a
// tripcode, following imageboard convention:
//
//	"" or "  "        -> Anonymous
//	"ana"             -> name "ana"
//	"ana#secret"      -> name "ana" + tripcode "!!xxxxxxxxxx"
//	"ana##secret"     -> the same name and the same tripcode
//	"#secret"         -> Anonymous + that tripcode
//
// Tripcodes let a poster prove continuity between posts without an account.
// Every tripcode is keyed by the server salt. The classic "#secret" form is an
// unkeyed hash, and an unkeyed hash of a short, human-chosen secret can be
// brute-forced offline by anyone who sees the tripcode — which is everyone — so
// it would prove nothing. "#" and "##" are both accepted so that posters used
// to either convention get the tripcode they expect, and they give the same
// one, so nobody has to know which form they first used.
func parseIdentity(field, salt string) identity {
	name, secret, hasSecret := strings.Cut(field, "#")
	id := identity{Name: clampName(name)}
	if id.Name == "" {
		id.Name = anonymousName
	}
	if !hasSecret {
		return id
	}

	secret = strings.TrimPrefix(secret, "#")
	if strings.TrimSpace(secret) == "" {
		return id
	}
	id.Tripcode = tripcode(salt, secret)
	return id
}

// tripcode derives the tripcode for a secret. The derivation is the one secure
// tripcodes have always used here, so a "##secret" tripcode from before the
// unkeyed kind was retired is still the same tripcode.
func tripcode(salt, secret string) string {
	sum := hmacSum([]byte(salt), "trip:"+secret)
	return tripcodePrefix + base64.RawURLEncoding.EncodeToString(sum)[:tripcodeLen]
}

// posterID derives the short per-thread identifier shown next to a post. It is
// stable for one client within one thread and unlinkable across threads, which
// is what keeps a discussion followable without deanonymising anyone.
func posterID(salt, clientKey string, threadID int64) string {
	sum := hmacSum([]byte(salt), "poster:"+strconv.FormatInt(threadID, 10)+":"+clientKey)
	out := make([]byte, posterIDLen)
	for i := range out {
		out[i] = posterIDAlpha[int(sum[i])%len(posterIDAlpha)]
	}
	return string(out)
}

func hmacSum(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))
	return mac.Sum(nil)
}

func clampName(name string) string {
	name = strings.TrimSpace(collapseControl(name))
	return truncateRunes(name, maxNameLen)
}
