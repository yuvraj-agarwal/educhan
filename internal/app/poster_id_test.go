package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Poster IDs are meant to tell the people in one thread apart and nothing
// more. These tests hold the three sources an ID can come from — the poster key
// a browser sends, the credential on a badged post, and the address as a last
// resort — to that, and hold the poster key itself to never being kept.

/* Harness ------------------------------------------------------------------- */

// newPosterKey makes a key the way the browser does: 16 random bytes,
// base64url without padding.
func newPosterKey(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("poster key: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// browser is one client as the front end behaves: a post that asks for a
// badge carries its credential and nothing else, and any other post carries
// its poster key. The server's handling of a post that carries both, which the
// client never sends, is tested by posting with explicit headers.
type browser struct {
	*harness
	key        string
	credential string
	// forwardedFor, when set, makes the request appear to arrive through one
	// proxy from that address, for tests that run with a trusted hop.
	forwardedFor string
}

func (b browser) headers(badge bool) http.Header {
	header := http.Header{}
	switch {
	case badge && b.credential != "":
		header.Set(credentialHeader, b.credential)
	case b.key != "":
		header.Set(posterKeyHeader, b.key)
	}
	if b.forwardedFor != "" {
		header.Set("X-Forwarded-For", b.forwardedFor)
	}
	return header
}

// postWithHeaders posts a JSON submission with extra request headers and
// returns the status.
func (c *client) postWithHeaders(header http.Header, path string, body any, out any) int {
	c.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		c.t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(encoded))
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	for k, values := range header {
		req.Header[k] = values
	}
	req.Header.Set("Content-Type", "application/json")
	return c.send(req, out)
}

// thread opens a thread on board and returns its number.
func (b browser) thread(board, body string, badge bool) int64 {
	b.t.Helper()
	var out createdPost
	status := b.postWithHeaders(b.headers(badge), "/api/boards/"+board+"/threads",
		map[string]any{"body": body, "badge": badge}, &out)
	if status != http.StatusCreated {
		b.t.Fatalf("thread %q: status %d (%s)", body, status, out.Error)
	}
	return out.ThreadNo
}

// reply posts to a thread and returns the new post's number.
func (b browser) reply(board string, threadNo int64, body string, badge bool) int64 {
	b.t.Helper()
	var out createdPost
	status := b.postWithHeaders(b.headers(badge), fmt.Sprintf("/api/boards/%s/threads/%d/posts", board, threadNo),
		map[string]any{"body": body, "badge": badge}, &out)
	if status != http.StatusCreated {
		b.t.Fatalf("reply %q: status %d (%s)", body, status, out.Error)
	}
	return out.No
}

// imageReply is reply over multipart with an image, which carries the same
// headers as a JSON post.
func (b browser) imageReply(board string, threadNo int64, body string) int64 {
	b.t.Helper()
	var out createdPost
	status := b.uploadWithHeaders(b.headers(false), fmt.Sprintf("/api/boards/%s/threads/%d/posts", board, threadNo),
		map[string]string{"body": body}, "reply.png", pngBytes(b.t, 16, 16), &out)
	if status != http.StatusCreated {
		b.t.Fatalf("image reply %q: status %d (%s)", body, status, out.Error)
	}
	return out.No
}

// posterIDs maps each post in a thread to its poster ID.
func posterIDs(t *testing.T, h *harness, board string, threadNo int64) map[int64]string {
	t.Helper()
	ids := map[int64]string{}
	for _, post := range fetchThread(t, h, board, threadNo).Posts {
		ids[post.No] = post.PosterID
	}
	return ids
}

// threadID reads a thread's primary key, which is what a poster ID is keyed
// on, so a test can recompute one.
func (h *harness) threadID(board string, no int64) int64 {
	h.t.Helper()
	var id int64
	if err := h.app.pool.QueryRow(context.Background(), `
		SELECT t.id FROM threads t JOIN boards b ON b.id = t.board_id
		WHERE b.slug = $1 AND t.no = $2`, board, no).Scan(&id); err != nil {
		h.t.Fatalf("thread id of /%s/%d: %v", board, no, err)
	}
	return id
}

// credentialID reads the random id out of a credential the way the server
// does once it has checked the signature.
func credentialID(t *testing.T, h *harness, token string) string {
	t.Helper()
	cred, ok := h.app.parseCredential(token)
	if !ok {
		t.Fatal("the issued credential does not parse")
	}
	return cred.ID
}

/* Poster keys ---------------------------------------------------------------- */

// The fix for a campus NAT: two browsers behind one address are two posters.
// Every request in these tests arrives from the loopback address, so the
// address alone would have given all of them one ID.
func TestPosterKeysSeparateBrowsersOnOneAddress(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("ids")
	ana := browser{harness: h, key: newPosterKey(t)}
	bob := browser{harness: h, key: newPosterKey(t)}
	keyless := browser{harness: h}

	thread := ana.thread("ids", "ana opens", false)
	bobReply := bob.reply("ids", thread, "bob replies", false)
	anaReply := ana.reply("ids", thread, "ana again", false)
	keylessReply := keyless.reply("ids", thread, "an older client, no key", false)

	ids := posterIDs(t, h, "ids", thread)
	if ids[thread] != ids[anaReply] {
		t.Errorf("one poster key, one thread: IDs %q and %q, want one", ids[thread], ids[anaReply])
	}
	if ids[thread] == ids[bobReply] {
		t.Errorf("two poster keys on one address share the ID %q", ids[thread])
	}
	for _, other := range []int64{thread, bobReply} {
		if ids[keylessReply] == ids[other] {
			t.Errorf("a post without a key shares the ID %q with a keyed post", ids[other])
		}
	}
}

// A key keeps one ID within a thread, over JSON and multipart alike, and a
// different one in every other thread.
func TestPosterKeyKeepsOneIDPerThread(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("ids")
	ana := browser{harness: h, key: newPosterKey(t)}

	first := ana.thread("ids", "first thread", false)
	reply := ana.reply("ids", first, "a text reply", false)
	image := ana.imageReply("ids", first, "and one with an image")
	second := ana.thread("ids", "second thread", false)
	secondReply := ana.reply("ids", second, "replying here too", false)

	ids := posterIDs(t, h, "ids", first)
	if ids[first] != ids[reply] || ids[first] != ids[image] {
		t.Errorf("one key in one thread got IDs %q, %q, %q; want one", ids[first], ids[reply], ids[image])
	}
	other := posterIDs(t, h, "ids", second)
	if other[second] != other[secondReply] {
		t.Errorf("one key in the second thread got IDs %q and %q; want one", other[second], other[secondReply])
	}
	if other[second] == ids[first] {
		t.Errorf("one key got the ID %q in two threads; IDs must not link threads", ids[first])
	}
}

// Without a key the ID comes from the address, as it always did, so a client
// that predates the poster key still posts.
func TestNoPosterKeyFallsBackToTheClientAddress(t *testing.T) {
	h := testServer(t, func(cfg *Config) { cfg.TrustedProxyHops = 1 })
	h.addBoard("ids")
	const address = "203.0.113.9"
	here := browser{harness: h, forwardedFor: address}
	there := browser{harness: h, forwardedFor: "203.0.113.10"}
	keyed := browser{harness: h, forwardedFor: address, key: newPosterKey(t)}

	thread := here.thread("ids", "no key", false)
	again := here.reply("ids", thread, "still no key", false)
	elsewhere := there.reply("ids", thread, "another address", false)
	withKey := keyed.reply("ids", thread, "the same address, with a key", false)

	ids := posterIDs(t, h, "ids", thread)
	want := posterID(h.app.cfg.PosterIDSalt, addressSource(address), h.threadID("ids", thread))
	if ids[thread] != want || ids[again] != want {
		t.Errorf("keyless posts from %s got %q and %q, want the address-derived %q", address, ids[thread], ids[again], want)
	}
	if ids[elsewhere] == want {
		t.Error("two addresses without keys share a poster ID")
	}
	if ids[withKey] == want {
		t.Error("a post with a key got the address's ID; the key must replace the address")
	}
}

// A header that is there but is not a key is refused, on both endpoints and
// in both encodings, before anything is stored and before a cooldown starts.
func TestMalformedPosterKeyIsRefused(t *testing.T) {
	h := testServer(t, func(cfg *Config) {
		cfg.ThreadCooldown = time.Hour
		cfg.PostCooldown = time.Hour
		cfg.UploadCooldown = time.Hour
	})
	h.addBoard("ids")

	malformed := []string{
		"",                             // present but empty
		"tooShort",                     // not 22 characters
		strings.Repeat("A", 23),        // one too many
		strings.Repeat("A", 20) + "==", // padded
		strings.Repeat("A", 21) + "+",  // standard base64, not base64url
		strings.Repeat("A", 21) + ".",
	}
	refuse := func(path string, value string, multipart bool) {
		t.Helper()
		header := http.Header{posterKeyHeader: {value}}
		var out createdPost
		var status int
		if multipart {
			status = h.uploadWithHeaders(header, path, map[string]string{"body": "refused"},
				"refused.png", pngBytes(t, 16, 16), &out)
		} else {
			status = h.postWithHeaders(header, path, map[string]any{"body": "refused"}, &out)
		}
		if status != http.StatusBadRequest || out.Error != "invalid poster key" {
			t.Errorf("POST %s with key %q (multipart %v): %d %q, want 400 invalid poster key",
				path, value, multipart, status, out.Error)
		}
	}

	for _, value := range malformed {
		refuse("/api/boards/ids/threads", value, false)
		refuse("/api/boards/ids/threads", value, true)
	}
	if n := h.countRows("posts"); n != 0 {
		t.Fatalf("%d posts exist after refused thread creations, want none", n)
	}

	// Nothing was charged for the refusals: a well-formed post goes straight
	// through even with hour-long cooldowns.
	ana := browser{harness: h, key: newPosterKey(t)}
	thread := ana.thread("ids", "the first post that counts", false)

	replies := fmt.Sprintf("/api/boards/ids/threads/%d/posts", thread)
	for _, value := range malformed {
		refuse(replies, value, false)
		refuse(replies, value, true)
	}
	if n := h.countRows("posts"); n != 1 {
		t.Fatalf("%d posts exist after refused replies, want only the opening post", n)
	}
	ana.imageReply("ids", thread, "the first reply that counts")

	// The refused multipart posts were turned away before their images were
	// read, so the only files on disk are the one accepted reply's.
	if files := filesUnder(t, h.app.cfg.UploadDir); len(files) != 2 {
		t.Errorf("upload dir holds %v, want only the accepted reply's original and thumbnail", files)
	}
}

/* Badged posts --------------------------------------------------------------- */

// A badged post must not share the browser's ID, or an unbadged reply from the
// same browser would sit under the same ID and be tied to the institution.
func TestBadgedPostsGetTheirOwnPosterID(t *testing.T) {
	h := verifyServer(t, nil)
	h.addBoard("ids")
	ana := browser{harness: h.harness, key: newPosterKey(t), credential: h.verify("ana@iitb.ac.in").Credential}

	thread := ana.thread("ids", "badged opening post", true)
	plain := ana.reply("ids", thread, "the same browser, unbadged", false)
	badged := ana.reply("ids", thread, "badged again", true)

	// The credential sent on a post that did not ask for a badge is never
	// read, so it cannot move the ID either.
	var out createdPost
	if status := h.postWithHeaders(http.Header{
		posterKeyHeader:  {ana.key},
		credentialHeader: {ana.credential},
	}, fmt.Sprintf("/api/boards/ids/threads/%d/posts", thread),
		map[string]any{"body": "credential present, badge not asked for"}, &out); status != http.StatusCreated {
		t.Fatalf("unbadged reply carrying a credential: status %d (%s)", status, out.Error)
	}
	carried := out.No

	ids := posterIDs(t, h.harness, "ids", thread)
	if ids[thread] != ids[badged] {
		t.Errorf("one credential in one thread got IDs %q and %q, want one", ids[thread], ids[badged])
	}
	if ids[thread] == ids[plain] {
		t.Errorf("a badged and an unbadged post from one browser share the ID %q", ids[plain])
	}
	if ids[carried] != ids[plain] {
		t.Errorf("an unbadged post carrying a credential got %q, want the key's %q", ids[carried], ids[plain])
	}

	// A second verified address in the same browser is a second badged ID.
	bo := browser{harness: h.harness, key: ana.key, credential: h.verify("bo@iitb.ac.in").Credential}
	boReply := bo.reply("ids", thread, "another credential, same browser", true)
	if got := posterIDs(t, h.harness, "ids", thread)[boReply]; got == ids[thread] || got == ids[plain] {
		t.Errorf("a second credential got %q, which is already in use in the thread", got)
	}

	// And across threads the credential's IDs differ, as any poster's do.
	second := ana.thread("ids", "a second badged thread", true)
	secondPlain := ana.reply("ids", second, "and unbadged", false)
	other := posterIDs(t, h.harness, "ids", second)
	if other[second] == ids[thread] {
		t.Errorf("one credential got the ID %q in two threads", ids[thread])
	}
	if other[second] == other[secondPlain] {
		t.Errorf("badged and unbadged share the ID %q in the second thread too", other[second])
	}
}

// A badged post's poster ID is derived from the credential's random id as the
// poster presented it, not from anything the database keeps. Were it derived
// from a stored value, anyone holding the database and the salt could compute
// every credential's ID in every thread and tie each badged post back to a
// verification row. This tries every way of deriving it from the verification
// tables that a plausible mistake would produce, and requires that none works.
func TestBadgedPosterIDCannotBeRecomputedFromTheDatabase(t *testing.T) {
	h := verifyServer(t, nil)
	h.addBoard("ids")
	issued := h.verify("ana@iitb.ac.in")
	ana := browser{harness: h.harness, key: newPosterKey(t), credential: issued.Credential}

	// A second verification left in flight, so the pending table has a row to
	// try as well.
	if status, out := h.requestCode("bo@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatalf("request a second code: status %d (%s)", status, out.Error)
	}

	thread := ana.thread("ids", "badged", true)
	reply := ana.reply("ids", thread, "badged reply", true)
	stored := posterIDs(t, h.harness, "ids", thread)
	if stored[thread] != stored[reply] {
		t.Fatalf("the two badged posts got %q and %q, want one ID", stored[thread], stored[reply])
	}
	target := stored[thread]
	threadKeys := []int64{h.threadID("ids", thread), thread}
	keys := []string{h.app.cfg.PosterIDSalt, h.app.cfg.VerifySecret}

	matches := func(value string) bool {
		for _, enc := range encodings(value) {
			for _, source := range []posterSource{
				credentialSource(enc), posterKeySource(enc), addressSource(enc), posterSource(enc),
			} {
				for _, key := range keys {
					for _, threadKey := range threadKeys {
						if posterID(key, source, threadKey) == target {
							return true
						}
					}
				}
			}
		}
		return false
	}

	// Positive control: the search finds the ID from what actually derives
	// it, the id inside the presented credential. A search that cannot find
	// the true source proves nothing by failing to find a false one.
	if !matches(credentialID(t, h.harness, issued.Credential)) {
		t.Fatal("the candidate search cannot recompute the ID from the credential id itself, so it cannot be trusted")
	}

	tried := 0
	for _, table := range []string{"verification_credentials", "verification_pending"} {
		for _, row := range h.rowValues(table) {
			for column, value := range row {
				tried++
				if matches(value) {
					t.Errorf("the badged poster ID %q can be recomputed from %s.%s", target, table, column)
				}
			}
		}
	}
	if tried == 0 {
		t.Fatal("no verification rows were tried; the check would pass vacuously")
	}
}

// encodings renders a stored value every way a careless derivation might have
// used it.
func encodings(value string) []string {
	raw := []byte(value)
	return []string{
		value,
		hex.EncodeToString(raw),
		strings.ToUpper(hex.EncodeToString(raw)),
		base64.StdEncoding.EncodeToString(raw),
		base64.RawStdEncoding.EncodeToString(raw),
		base64.URLEncoding.EncodeToString(raw),
		base64.RawURLEncoding.EncodeToString(raw),
	}
}

// rowValues reads every row of a table as column name to value, with bytea as
// its raw bytes and everything else in its text form.
func (h *harness) rowValues(table string) []map[string]string {
	h.t.Helper()
	columns := h.columns(table)
	rows, err := h.app.pool.Query(context.Background(), "SELECT * FROM "+pgx.Identifier{table}.Sanitize())
	if err != nil {
		h.t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()

	names := rows.FieldDescriptions()
	var out []map[string]string
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			h.t.Fatalf("read %s: %v", table, err)
		}
		row := map[string]string{}
		for i, value := range values {
			name := string(names[i].Name)
			switch v := value.(type) {
			case []byte:
				row[name] = string(v)
			case time.Time:
				row[name] = v.UTC().Format(time.RFC3339)
				row[name+" (unix)"] = strconv.FormatInt(v.Unix(), 10)
			default:
				row[name] = fmt.Sprint(v)
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("read %s: %v", table, err)
	}
	if len(out) > 0 && len(out[0]) < len(columns) {
		h.t.Fatalf("read %d of %s's %d columns", len(out[0]), table, len(columns))
	}
	return out
}

/* The key is never kept ------------------------------------------------------ */

// The poster key is HMAC input and nothing else. Stored or logged, it would
// be one identifier across every thread a browser posts in — the linkage per-
// thread IDs exist to prevent — so the whole posting surface is driven with
// one, and then every column of every table and every log line is searched for
// it.
func TestPosterKeyIsNeverStoredOrLogged(t *testing.T) {
	var logs syncBuffer
	mail := &captureSender{}
	h := testServerLogging(t, func(cfg *Config) {
		cfg.VerifySecret = "test-verification-secret"
		cfg.VerifyCodeTTL = defaultVerifyCodeTTL
		cfg.VerifyCredentialTTL = defaultVerifyCredentialTTL
		cfg.Sender = mail
	}, &logs)
	h.addBoard("ids")
	vh := &verifyHarness{harness: h, mail: mail}
	issued := vh.verify("ana@iitb.ac.in")

	key := newPosterKey(t)
	ana := browser{harness: h, key: key, credential: issued.Credential}
	const marker = "body-marker-for-the-scan"

	thread := ana.thread("ids", marker, false)
	ana.reply("ids", thread, "a text reply", false)
	ana.imageReply("ids", thread, "an image reply")
	ana.reply("ids", thread, "a badged reply", true)
	ana.thread("ids", "a badged thread", true)
	// The client never sends the key with a badged post, but the server must
	// not keep either value when one arrives with both.
	var both createdPost
	if status := h.postWithHeaders(http.Header{
		posterKeyHeader:  {key},
		credentialHeader: {issued.Credential},
	}, fmt.Sprintf("/api/boards/ids/threads/%d/posts", thread),
		map[string]any{"body": "badged, with the key as well", "badge": true}, &both); status != http.StatusCreated {
		t.Fatalf("badged reply carrying the key too: status %d (%s)", status, both.Error)
	}

	// Refusals too: a malformed key, and a key sent to a board that does not
	// exist, are both turned away without the key being written anywhere.
	malformed := key + "!"
	var refused createdPost
	if status := h.postWithHeaders(http.Header{posterKeyHeader: {malformed}},
		"/api/boards/ids/threads", map[string]any{"body": "refused"}, &refused); status != http.StatusBadRequest {
		t.Fatalf("malformed key: status %d, want 400", status)
	}
	if status := h.postWithHeaders(ana.headers(false),
		"/api/boards/nope/threads", map[string]any{"body": "no such board"}, &refused); status != http.StatusNotFound {
		t.Fatalf("unknown board: status %d, want 404", status)
	}

	raw, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		t.Fatalf("decode the key: %v", err)
	}
	digest := sha256.Sum256(raw)
	textDigest := sha256.Sum256([]byte(key))

	// Positive controls first. The text columns are shown to be searched by
	// finding the post body and the poster ID, and the bytea columns by
	// finding the credential's stored hash, so a scan that finds no key has
	// actually looked.
	if n := h.rowsContaining("posts", []byte(marker)); n != 1 {
		t.Fatalf("the scan found the body marker in %d posts, want 1; it cannot be trusted", n)
	}
	if id := posterIDs(t, h, "ids", thread)[thread]; h.rowsContaining("posts", []byte(id)) == 0 {
		t.Fatal("the scan cannot find a stored poster ID, so it cannot be trusted to find a key")
	}
	if n := h.rowsContaining("verification_credentials", hashToken(credentialID(t, h, issued.Credential))); n != 1 {
		t.Fatalf("the scan found the credential hash in %d rows, want 1; it misses bytea columns", n)
	}

	// The credential's id is what a badged poster ID is derived from, and
	// the verification tables keep only its hash; the id itself, the token
	// that carries it, and the tagged strings the IDs are computed over must
	// be nowhere, or a badged post could be tied back to its verification.
	credID := credentialID(t, h, issued.Credential)
	credRaw, err := base64.RawURLEncoding.DecodeString(credID)
	if err != nil {
		t.Fatalf("decode the credential id: %v", err)
	}
	for name, needle := range map[string][]byte{
		"the key":                  []byte(key),
		"the key's bytes":          raw,
		"SHA-256 of the key bytes": digest[:],
		"SHA-256 of the key text":  textDigest[:],
		"the malformed key":        []byte(malformed),
		"the key's source string":  []byte(posterKeySource(key)),
		"the credential":           []byte(issued.Credential),
		"the credential's id":      []byte(credID),
		"the credential id bytes":  credRaw,
		"the credential's source":  []byte(credentialSource(credID)),
		"the verified address":     []byte("ana@iitb.ac.in"),
	} {
		for _, table := range h.tables() {
			if n := h.rowsContaining(table, needle); n != 0 {
				t.Errorf("%d rows of %s contain %s; it must never be stored", n, table, name)
			}
		}
	}

	// The log, by the same standard. JSON leaves the base64url alphabet
	// unescaped, so the key would appear verbatim; its bytes are tried as
	// slog renders a []byte (standard base64) and as hex.
	out := logs.String()
	if !strings.Contains(out, `"path":"/api/boards/ids/threads"`) {
		t.Fatalf("expected the posting requests in the log, got:\n%s", out)
	}
	for name, needle := range map[string]string{
		"the key":             key,
		"the key's bytes":     base64.StdEncoding.EncodeToString(raw),
		"the key's hex":       hex.EncodeToString(raw),
		"the malformed key":   malformed,
		"the credential":      issued.Credential,
		"the credential's id": credentialID(t, h, issued.Credential),
	} {
		if strings.Contains(out, needle) {
			t.Errorf("the log contains %s:\n%s", name, out)
		}
	}
}
