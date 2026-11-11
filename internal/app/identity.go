package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
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
	_, secret, hasSecret := strings.Cut(field, "#")
	id := identity{Name: displayedName(field)}
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

// displayedName is the part of a Name field that is shown: everything before
// the first "#", as it will be stored. It is all the content filter reads of a
// name, so the tripcode secret is never inspected or quoted back. Empty means
// the post will be shown as Anonymous.
func displayedName(field string) string {
	name, _, _ := strings.Cut(field, "#")
	return clampName(name)
}

// tripcode derives the tripcode for a secret. The derivation is the one secure
// tripcodes have always used here, so a "##secret" tripcode from before the
// unkeyed kind was retired is still the same tripcode.
func tripcode(salt, secret string) string {
	sum := hmacSum([]byte(salt), "trip:"+secret)
	return tripcodePrefix + base64.RawURLEncoding.EncodeToString(sum)[:tripcodeLen]
}

/* Poster IDs ---------------------------------------------------------------- */

// posterKeyHeader carries the poster key: 16 random bytes the browser makes
// once, keeps in localStorage, and sends base64url-encoded on the two requests
// that create posts and on nothing else. It exists because the client address
// is a poor stand-in for a person — everyone behind one campus NAT shared a
// poster ID when the address was all there was.
//
// The key is read in the posting handlers, used as HMAC input, and dropped. It
// is never stored and never logged (TestPosterKeyIsNeverStoredOrLogged): kept
// anywhere, it would be a stable identifier tying a browser's posts together
// across every thread, which is exactly what per-thread IDs exist to prevent.
const posterKeyHeader = "X-Educhan-Poster-Key"

// posterKeyLen is the length of 16 bytes in unpadded base64url.
const posterKeyLen = 22

// posterKey reads the poster key a posting request presents, answering 400
// itself when it is malformed. A request without the header gets "" and posts
// with a poster ID derived from its address instead; a header that is present
// but not a well-formed key is refused rather than quietly ignored, because a
// client sending a broken key would otherwise post under an ID its poster does
// not expect and never find out why.
func posterKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values(posterKeyHeader)
	if len(values) == 0 {
		return "", true
	}
	if len(values) != 1 || !validPosterKey(values[0]) {
		writeError(w, http.StatusBadRequest, "invalid poster key")
		return "", false
	}
	return values[0], true
}

// validPosterKey accepts exactly the shape the client generates: 22 characters
// of the base64url alphabet, no padding. Nothing about the key is checked
// beyond its shape — it is chosen by the client, so there is nothing else to
// check it against.
func validPosterKey(key string) bool {
	if len(key) != posterKeyLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		switch c := key[i]; {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// posterSource is what a post's poster ID is derived from, tagged with where it
// came from. The tags keep the three kinds of input apart, so a poster key, an
// address, and a credential id can never yield the same HMAC input whatever
// their values — a client cannot, for instance, pick a poster key that lands
// on the ID some address gets. The constructors below are the only way to make
// one, which keeps an untagged value from reaching posterID by accident.
type posterSource string

func posterKeySource(key string) posterSource { return posterSource("key:" + key) }
func addressSource(ip string) posterSource    { return posterSource("ip:" + ip) }
func credentialSource(id string) posterSource { return posterSource("cred:" + id) }

// posterSourceFor picks what a post's poster ID is derived from:
//
//   - A badged post uses the random id inside the credential it presented, and
//     never the poster key or the address. Sharing the browser's ID would put a
//     badged post and an unbadged reply from the same browser under one ID in a
//     thread, publicly tying the unbadged post to the institution.
//   - Otherwise the poster key, when the browser sent one.
//   - Otherwise the client address, which is what every poster ID used to be.
//
// The credential id is the raw one from the signed token the poster presented,
// not credential_id_hash or anything else the database holds. That is
// deliberate: an ID derived from a stored value could be recomputed by anyone
// holding the database and the salt, for every credential and every thread,
// which would tie each badged post across threads back to a verification row.
// Derived from the presented id, a badged post's poster ID can be reproduced
// only by someone holding the credential itself
// (TestBadgedPosterIDCannotBeRecomputedFromTheDatabase).
func posterSourceFor(granted badgeGrant, key, ip string) posterSource {
	switch {
	case granted.CredentialID != "":
		return credentialSource(granted.CredentialID)
	case key != "":
		return posterKeySource(key)
	default:
		return addressSource(ip)
	}
}

// posterID derives the short per-thread identifier shown next to a post. It is
// stable for one source within one thread and unlinkable across threads, which
// is what keeps a discussion followable without deanonymising anyone.
//
// It is a courtesy, not proof of anything: the poster key is chosen by the
// client, so a poster can take a fresh ID whenever they like, as they always
// could by changing networks.
func posterID(salt string, source posterSource, threadID int64) string {
	sum := hmacSum([]byte(salt), "poster:"+strconv.FormatInt(threadID, 10)+":"+string(source))
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
