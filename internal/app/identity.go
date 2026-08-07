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
	maxNameLen     = 64
	tripcodeLen    = 10
	posterIDLen    = 8
	posterIDAlpha  = "0123456789abcdefghijklmnopqrstuvwxyz"
	tripcodeAlpha1 = "!"  // name#secret  — anyone with the secret reproduces it
	tripcodeAlpha2 = "!!" // name##secret — additionally keyed by the server salt
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
//	"ana#secret"      -> name "ana" + insecure tripcode "!xxxxxxxxxx"
//	"ana##secret"     -> name "ana" + secure tripcode "!!xxxxxxxxxx"
//	"#secret"         -> Anonymous + insecure tripcode
//
// Tripcodes let a poster prove continuity between posts without an account.
// Unlike the original crypt(3) scheme they are derived with SHA-256 (and, for
// secure tripcodes, HMAC keyed by the server salt) so they cannot be cracked
// with off-the-shelf DES tooling.
func parseIdentity(field, salt string) identity {
	name, secret, hasSecret := strings.Cut(field, "#")
	id := identity{Name: clampName(name)}
	if id.Name == "" {
		id.Name = anonymousName
	}
	if !hasSecret {
		return id
	}

	secure := false
	if rest, ok := strings.CutPrefix(secret, "#"); ok {
		secure, secret = true, rest
	}
	if strings.TrimSpace(secret) == "" {
		return id
	}

	if secure {
		id.Tripcode = tripcodeAlpha2 + digest([]byte(salt), "trip:"+secret)
	} else {
		id.Tripcode = tripcodeAlpha1 + digest(nil, "trip:"+secret)
	}
	return id
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

// digest returns a short URL-safe digest of value. When key is nil the digest is
// unkeyed, so it can be reproduced by anyone holding the same value.
func digest(key []byte, value string) string {
	sum := hmacSum(key, value)
	return base64.RawURLEncoding.EncodeToString(sum)[:tripcodeLen]
}

func hmacSum(key []byte, value string) []byte {
	if key == nil {
		sum := sha256.Sum256([]byte(value))
		return sum[:]
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))
	return mac.Sum(nil)
}

func clampName(name string) string {
	name = strings.TrimSpace(collapseControl(name))
	return truncateRunes(name, maxNameLen)
}
