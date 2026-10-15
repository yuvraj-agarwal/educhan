package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yuvraj-agarwal/educhan/internal/media"
)

// archiveThread backdates a thread into the archive, as if it had been pushed
// off its board the given time ago.
func (h *harness) archiveThread(board string, no int64, ago time.Duration) {
	h.t.Helper()
	h.exec(`UPDATE threads SET archived_at = now() - $3 * interval '1 second'
		WHERE no = $2 AND board_id = (SELECT id FROM boards WHERE slug = $1)`, board, no, int64(ago.Seconds()))
}

// threadBlobs lists the files behind every attachment in a thread.
func threadBlobs(t *testing.T, h *harness, board string, no int64) []string {
	t.Helper()
	var files []string
	for _, p := range fetchThread(t, h, board, no).Posts {
		if p.Attachment != nil {
			original, thumb := blobPaths(t, h.app.cfg.UploadDir, p.Attachment)
			files = append(files, original, thumb)
		}
	}
	return files
}

func (h *harness) threadStatus(board string, no int64) int {
	h.t.Helper()
	var out map[string]any
	return h.do(http.MethodGet, fmt.Sprintf("/api/boards/%s/threads/%d", board, no), nil, &out)
}

// The sweep deletes exactly the archived threads whose board's archive_days
// have run out — rows and files — and leaves everything else alone: threads
// archived more recently, threads on boards that keep their archive longer,
// and live threads, however old.
func TestSweepDeletesOnlyExpiredArchivedThreadsAndTheirMedia(t *testing.T) {
	h := testServer(t, nil)
	for slug, days := range map[string]int{"brief": 1, "lasting": 30, "instant": 0} {
		h.addBoard(slug)
		h.exec(`UPDATE boards SET archive_days = $2 WHERE slug = $1`, slug, days)
	}
	day := 24 * time.Hour

	type thread struct {
		board   string
		no      int64
		blobs   []string
		expired bool
	}
	var threads []*thread
	add := func(board string, archivedAgo time.Duration, expired bool) *thread {
		created := h.imageThread(board, "opening post with an image")
		if status, msg := h.imageReply(board, created.ThreadNo, "a reply with an image"); status != http.StatusCreated {
			t.Fatalf("image reply: status %d (%s)", status, msg)
		}
		h.reply(board, created.ThreadNo, "", "and one without", false)
		if archivedAgo >= 0 {
			h.archiveThread(board, created.ThreadNo, archivedAgo)
		}
		th := &thread{board: board, no: created.ThreadNo, expired: expired}
		th.blobs = threadBlobs(t, h, board, th.no)
		threads = append(threads, th)
		return th
	}
	const live = -1
	add("brief", 2*day, true)         // a day past its board's archive_days
	add("brief", 12*time.Hour, false) // archived, but not for long enough
	add("brief", live, false)         // live threads are never swept
	add("lasting", 2*day, false)      // the same age, on a board that keeps it longer
	add("lasting", 31*day, true)      // past even that board's period
	add("instant", time.Minute, true) // archive_days 0: gone at the next sweep
	add("instant", live, false)
	// Age alone does not expire a live thread.
	h.exec(`UPDATE threads SET created_at = now() - interval '400 days', bumped_at = now() - interval '400 days'
		WHERE archived_at IS NULL`)

	// A batch of two over three expired threads takes the loop round more than
	// once, which a single batch would not.
	report, err := h.app.sweep(context.Background(), time.Now(), 2)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if want := (SweepReport{Threads: 3, Posts: 9, Attachments: 6, BlobsRemoved: 12}); report != want {
		t.Errorf("sweep report = %+v, want %+v", report, want)
	}

	for _, th := range threads {
		status := h.threadStatus(th.board, th.no)
		switch {
		case th.expired && status != http.StatusNotFound:
			t.Errorf("/%s/%d should have been swept, GET answers %d", th.board, th.no, status)
		case !th.expired && status != http.StatusOK:
			t.Errorf("/%s/%d should have survived the sweep, GET answers %d", th.board, th.no, status)
		}
		for _, path := range th.blobs {
			requireFile(t, path, !th.expired)
		}
	}
	if posts := h.countRows("posts"); posts != 12 {
		t.Errorf("%d posts survive, want the 12 of the four surviving threads", posts)
	}
	if attachments := h.countRows("attachments"); attachments != 8 {
		t.Errorf("%d attachment rows survive, want 8", attachments)
	}
	if archived := fetchArchive(t, h, "brief").Threads; len(archived) != 1 {
		t.Errorf("/brief/ archive lists %d threads, want the one not yet expired", len(archived))
	}
	if queued := h.countRows("media_deletions"); queued != 0 {
		t.Errorf("%d files are still queued for removal, want none", queued)
	}

	// Nothing is left to do, and doing nothing is not a failure.
	again, err := h.app.Sweep(context.Background())
	if err != nil || again != (SweepReport{}) {
		t.Errorf("second sweep = %+v, %v; want nothing swept and no error", again, err)
	}
}

// The rows go first and the files after, so a file the store will not let go
// of is an orphan rather than a broken post. Its key stays queued, the sweep
// reports failure — which is what gives the job a non-zero exit — and the next
// sweep tries again, so a storage outage costs a retry rather than files kept
// public for good with nothing left to say they exist.
func TestSweepRetriesFilesItCouldNotRemove(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("stuck")
	h.exec(`UPDATE boards SET archive_days = 0 WHERE slug = 'stuck'`)

	thread := h.imageThread("stuck", "an image the store will not let go of")
	h.archiveThread("stuck", thread.ThreadNo, time.Minute)
	blobs := threadBlobs(t, h, "stuck", thread.ThreadNo)
	store := h.app.media
	h.app.media = refusingDeletes{store}

	report, err := h.app.Sweep(context.Background())
	if err == nil {
		t.Fatal("a sweep that could not remove an attachment reported success")
	}
	if want := (SweepReport{Threads: 1, Posts: 1, Attachments: 1, BlobFailures: 2}); report != want {
		t.Errorf("sweep report = %+v, want %+v", report, want)
	}
	if status := h.threadStatus("stuck", thread.ThreadNo); status != http.StatusNotFound {
		t.Errorf("the thread's rows should be gone even so, GET answers %d", status)
	}
	for _, path := range blobs {
		requireFile(t, path, true)
	}
	if queued := h.countRows("media_deletions"); queued != 2 {
		t.Fatalf("%d files queued for removal, want the original and its thumbnail", queued)
	}

	// The store comes back, and the next sweep finishes the job.
	h.app.media = store
	report, err = h.app.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep after the store recovered: %v", err)
	}
	if want := (SweepReport{BlobsRemoved: 2}); report != want {
		t.Errorf("sweep report = %+v, want %+v", report, want)
	}
	for _, path := range blobs {
		requireFile(t, path, false)
	}
	if queued := h.countRows("media_deletions"); queued != 0 {
		t.Errorf("%d files still queued, want none", queued)
	}
}

// The sweeper is a separate process, and its media settings have to match the
// server's. One that does not — a Job left on the local backend, a different
// bucket, prefix or upload directory — finds every file missing. Deleting a
// missing file is not an error for a moderator removing a post, but here it
// means the sweeper is looking at the wrong store, so the sweep fails and
// keeps the keys, where it used to report success and leave every image
// public at its URL.
func TestSweepAgainstAnotherMediaStoreFails(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("elsewhere")
	h.exec(`UPDATE boards SET archive_days = 0 WHERE slug = 'elsewhere'`)

	thread := h.imageThread("elsewhere", "stored where the sweeper is not looking")
	h.archiveThread("elsewhere", thread.ThreadNo, time.Minute)
	attachment := fetchThread(t, h, "elsewhere", thread.ThreadNo).Posts[0].Attachment
	blobs := threadBlobs(t, h, "elsewhere", thread.ThreadNo)

	store := h.app.media
	wrong, err := media.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("empty store: %v", err)
	}
	h.app.media = wrong
	report, err := h.app.Sweep(context.Background())
	h.app.media = store
	if err == nil {
		t.Fatal("a sweep against a store that holds none of the files reported success")
	}
	if !strings.Contains(err.Error(), "EDUCHAN_MEDIA_BACKEND") {
		t.Errorf("error %q does not point at the media settings", err)
	}
	if want := (SweepReport{Threads: 1, Posts: 1, Attachments: 1, BlobsMissing: 2}); report != want {
		t.Errorf("sweep report = %+v, want %+v", report, want)
	}
	for _, path := range blobs {
		requireFile(t, path, true)
	}

	// A sweeper with the server's settings removes them, and the URLs stop
	// answering.
	if report, err := h.app.Sweep(context.Background()); err != nil || report.BlobsRemoved != 2 {
		t.Fatalf("sweep with the server's store = %+v, %v; want both files removed", report, err)
	}
	for _, url := range []string{attachment.URL, attachment.ThumbURL} {
		var out map[string]any
		if status := h.do(http.MethodGet, url, nil, &out); status != http.StatusNotFound {
			t.Errorf("GET %s after the sweep answers %d, want 404", url, status)
		}
	}
}

// A cancelled sweep stops and says so, rather than reporting a pass it did
// not make.
func TestSweepStopsWhenCancelled(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("halted")
	h.exec(`UPDATE boards SET archive_days = 0 WHERE slug = 'halted'`)
	thread := h.createThread("halted", "", "", "archived and due")
	h.archiveThread("halted", thread.ThreadNo, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.app.Sweep(ctx); err == nil {
		t.Error("a cancelled sweep reported success")
	}
	if status := h.threadStatus("halted", thread.ThreadNo); status != http.StatusOK {
		t.Errorf("a cancelled sweep deleted the thread anyway: GET answers %d", status)
	}
}

// The sweep's log is one record of counts, never which boards or threads
// went — not even when a file cannot be removed, whose key names its board.
func TestSweepLogsOnlyCounts(t *testing.T) {
	var logs syncBuffer
	h := testServerLogging(t, nil, &logs)
	h.addBoard("quietly", "noisily")
	h.exec(`UPDATE boards SET archive_days = 0`)

	sweepLog := func(t *testing.T) ([]map[string]any, error) {
		t.Helper()
		before := len(logs.String())
		_, err := h.app.Sweep(context.Background())
		var lines []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(logs.String()[before:]), "\n") {
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("log line %q: %v", line, err)
			}
			lines = append(lines, entry)
		}
		return lines, err
	}

	thread := h.imageThread("quietly", "gone without a trace in the log")
	h.archiveThread("quietly", thread.ThreadNo, time.Minute)
	// A credential for the sweep to rewrite: it is counted, and nothing of
	// its row — the label, the expiry — reaches the log.
	h.exec(`INSERT INTO verification_credentials (email_hash, credential_id_hash, label, expires_at)
		VALUES ('\x01', '\x02', 'Hushed College', '2033-03-03T00:00:00Z')`)
	lines, err := sweepLog(t)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(lines) != 1 || lines[0]["msg"] != "sweep" {
		t.Fatalf("the sweep logged %v, want exactly one sweep record", lines)
	}
	record := lines[0]
	delete(record, "time")
	delete(record, "level")
	delete(record, "msg")
	want := map[string]any{"threads": 1.0, "posts": 1.0, "attachments": 1.0,
		"blobs_removed": 2.0, "blob_failures": 0.0, "blobs_missing": 0.0, "credentials_rewritten": 1.0}
	if fmt.Sprint(record) != fmt.Sprint(want) {
		t.Errorf("sweep record = %v, want exactly %v", record, want)
	}

	// A file the store refuses to delete is counted, and its key — which
	// names the board — appears neither in the log nor in the error the job
	// exits with.
	thread = h.imageThread("noisily", "an image the store will not let go of")
	h.archiveThread("noisily", thread.ThreadNo, time.Minute)
	h.app.media = refusingDeletes{h.app.media}
	lines, err = sweepLog(t)
	if err == nil {
		t.Fatal("a sweep that could not remove its files reported success")
	}
	if len(lines) != 1 || lines[0]["msg"] != "sweep" || lines[0]["blob_failures"] != 2.0 {
		t.Errorf("the failing sweep logged %v, want exactly one sweep record with two failures", lines)
	}
	for _, text := range []string{fmt.Sprint(lines), err.Error()} {
		if strings.Contains(text, "noisily") || strings.Contains(text, "quietly") {
			t.Errorf("the sweep named a board: %s", text)
		}
	}
	for _, text := range []string{logs.String(), err.Error()} {
		if strings.Contains(text, "Hushed") || strings.Contains(text, "2033-03-03") {
			t.Errorf("the sweep logged something of a credential's row: %s", text)
		}
	}
	if !strings.Contains(err.Error(), "delete refused") {
		t.Errorf("error %q does not say why the files were not removed", err)
	}
}

// refusingDeletes is a media store whose deletes always fail, standing in for
// a bucket that has stopped cooperating.
type refusingDeletes struct{ media.Store }

func (refusingDeletes) Delete(_ context.Context, key string) error {
	return fmt.Errorf("delete %s: %w", key, errors.New("delete refused"))
}

/* The verification tables ---------------------------------------------------- */

// credentialsAmongPosts counts the credentials whose row was written after one
// of the given posts and before another: a credential dated, by PostgreSQL's
// own transaction ids, to the time between two posts that show theirs. age()
// orders transaction ids the way the server does, so the comparison holds
// across wraparound.
func (h *harness) credentialsAmongPosts(lastPostID int64) int {
	h.t.Helper()
	var n int
	if err := h.app.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM verification_credentials c
		WHERE EXISTS (SELECT 1 FROM posts p WHERE p.id <= $1 AND age(p.xmin) > age(c.xmin))
		  AND EXISTS (SELECT 1 FROM posts p WHERE p.id <= $1 AND age(p.xmin) < age(c.xmin))`,
		lastPostID).Scan(&n); err != nil {
		h.t.Fatalf("place credentials among posts: %v", err)
	}
	return n
}

// credentialXmins counts the distinct transaction ids among the credential
// rows, leaving out the row for one address when except is set.
func (h *harness) credentialXmins(except []byte) int {
	h.t.Helper()
	var n int
	if err := h.app.pool.QueryRow(context.Background(), `
		SELECT count(DISTINCT xmin::text) FROM verification_credentials
		WHERE $1::bytea IS NULL OR email_hash <> $1`, except).Scan(&n); err != nil {
		h.t.Fatalf("count credential transaction ids: %v", err)
	}
	return n
}

// relationFiles returns the file each table is stored in.
func (h *harness) relationFiles(tables ...string) []int64 {
	h.t.Helper()
	files := make([]int64, len(tables))
	for i, table := range tables {
		if err := h.app.pool.QueryRow(context.Background(),
			`SELECT pg_relation_filenode($1::text::regclass)::bigint`, table).Scan(&files[i]); err != nil {
			h.t.Fatalf("file of %s: %v", table, err)
		}
	}
	return files
}

// credentialRows is the credentials table's contents, in a fixed order, for
// comparing before and after a rewrite.
func (h *harness) credentialRows() []string {
	h.t.Helper()
	rows, err := h.app.pool.Query(context.Background(), `
		SELECT concat_ws(' ', encode(email_hash, 'hex'), encode(credential_id_hash, 'hex'), label, expires_at)
		FROM verification_credentials ORDER BY email_hash`)
	if err != nil {
		h.t.Fatalf("read credentials: %v", err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		h.t.Fatalf("read credentials: %v", err)
	}
	return all
}

// badgedPost posts a reply with a credential and fails the test unless it
// went up with the badge.
func (h *verifyHarness) badgedPost(path, credential, label string) {
	h.t.Helper()
	var out createdPost
	if status := h.postWithCredential(credential, path,
		map[string]any{"body": "with a badge", "badge": true}, &out); status != http.StatusCreated {
		h.t.Fatalf("badged post: status %d (%s)", status, out.Error)
	}
	var badge string
	if err := h.app.pool.QueryRow(context.Background(),
		`SELECT badge FROM posts WHERE no = $1`, out.No).Scan(&badge); err != nil {
		h.t.Fatalf("read the badge back: %v", err)
	}
	if badge != label {
		h.t.Fatalf("badged post carries %q, want %q", badge, label)
	}
}

// Every row PostgreSQL stores carries the id of the transaction that wrote it,
// and posts are written by transactions too, with their times shown. So a
// credential row written between two posts can be dated to the minutes
// between them — what keeping created_at out of the table was for. The sweep
// rewrites the table so every credential carries the sweep's transaction id,
// and VACUUM FULL takes the old versions, and the pending rows confirmations
// deleted, off disk with the files they were in.
func TestSweepTakesIssuanceTimeOutOfCredentials(t *testing.T) {
	h := verifyServer(t, nil)
	h.addBoard("dated")

	// Issuances between posts, as on a live site.
	const issued = 4
	thread := h.createThread("dated", "", "", "before anyone verified")
	path := fmt.Sprintf("/api/boards/dated/threads/%d/posts", thread.ThreadNo)
	var credentials []string
	for i := range issued {
		credentials = append(credentials, h.verify(fmt.Sprintf("student%d@iitb.ac.in", i)).Credential)
		h.reply("dated", thread.ThreadNo, "", fmt.Sprintf("after issuance %d", i), false)
	}
	// A code in flight across the sweep, whose pending row VACUUM FULL moves.
	if status, out := h.requestCode("midway@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatalf("request a code: status %d (%s)", status, out.Error)
	}
	midway := h.mail.last(t).Code
	h.badgedPost(path, credentials[0], "IIT Bombay")

	var lastPost int64
	if err := h.app.pool.QueryRow(context.Background(), `SELECT max(id) FROM posts`).Scan(&lastPost); err != nil {
		t.Fatalf("last post: %v", err)
	}
	// Positive controls: before the sweep every credential sits between two
	// posts, each with its own transaction id, so the checks below can see
	// what they are looking for.
	if n := h.credentialsAmongPosts(lastPost); n != issued {
		t.Fatalf("before the sweep %d of %d credentials sit between two posts; the check cannot be trusted", n, issued)
	}
	if n := h.credentialXmins(nil); n != issued {
		t.Fatalf("before the sweep the credentials carry %d transaction ids, want %d", n, issued)
	}
	tables := []string{"verification_credentials", "verification_pending"}
	files := h.relationFiles(tables...)
	rows := h.credentialRows()

	report, err := h.app.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Credentials != issued {
		t.Errorf("the sweep rewrote %d credentials, want all %d", report.Credentials, issued)
	}
	if n := h.credentialXmins(nil); n != 1 {
		t.Errorf("after the sweep the credentials carry %d transaction ids, want the sweep's alone", n)
	}
	if n := h.credentialsAmongPosts(lastPost); n != 0 {
		t.Errorf("after the sweep %d credentials still sit between two posts written before it", n)
	}
	for i, file := range h.relationFiles(tables...) {
		if file == files[i] {
			t.Errorf("%s is still in file %d: VACUUM FULL did not replace it", tables[i], file)
		}
	}
	if after := h.credentialRows(); !slices.Equal(after, rows) {
		t.Errorf("the rewrite changed the credentials:\nbefore %q\nafter  %q", rows, after)
	}

	// Nothing a poster holds stopped working: every credential still badges,
	// and the code that was in flight still confirms.
	for _, credential := range credentials {
		h.badgedPost(path, credential, "IIT Bombay")
	}
	if status, out := h.confirmCode(midway); status != http.StatusOK {
		t.Fatalf("confirm the code requested before the sweep: status %d (%s)", status, out.Error)
	} else {
		h.badgedPost(path, out.Credential, "IIT Bombay")
	}
}

// Rows sit in a table's file in the order they were written, and a dump lists
// them in that order, so a table rewritten with every row's transaction id
// the same would still give issuance order away if it kept the old order.
// The rewrite inserts in random order; with 200 rows, the number of pairs
// left in issuance order lands within a quarter of half of them every time
// but about once in 10^25.
func TestSweepStoresCredentialsOutOfIssuanceOrder(t *testing.T) {
	h := testServer(t, nil)
	const issued = 200
	for i := range issued {
		h.exec(`INSERT INTO verification_credentials (email_hash, credential_id_hash, label, expires_at)
			VALUES (int4send($1::int), sha256(int4send($1::int)), 'Somewhere',
			        date_trunc('day', now()) + interval '181 days')`, i)
	}
	storedOrder := func() []int {
		rows, err := h.app.pool.Query(context.Background(),
			`SELECT email_hash FROM verification_credentials ORDER BY ctid`)
		if err != nil {
			t.Fatalf("read the stored order: %v", err)
		}
		hashes, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			t.Fatalf("read the stored order: %v", err)
		}
		order := make([]int, len(hashes))
		for i, hash := range hashes {
			order[i] = int(binary.BigEndian.Uint32(hash))
		}
		return order
	}
	// inversions counts the pairs stored the other way round from the order
	// they were issued in: none for issuance order, every pair for its
	// reverse, and about half of them for a shuffle.
	inversions := func(order []int) int {
		n := 0
		for i := range order {
			for j := i + 1; j < len(order); j++ {
				if order[i] > order[j] {
					n++
				}
			}
		}
		return n
	}
	pairs := issued * (issued - 1) / 2

	if n := inversions(storedOrder()); n != 0 {
		t.Fatalf("before the sweep %d pairs are out of issuance order, want none: the check cannot be trusted", n)
	}
	rows := h.credentialRows()

	report, err := h.app.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Credentials != issued {
		t.Errorf("the sweep rewrote %d credentials, want %d", report.Credentials, issued)
	}
	order := storedOrder()
	if len(order) != issued {
		t.Fatalf("%d credentials after the sweep, want %d", len(order), issued)
	}
	if n := inversions(order); n < pairs/4 || n > 3*pairs/4 {
		t.Errorf("after the sweep %d of %d pairs are out of issuance order; a shuffle leaves about half", n, pairs)
	}
	if after := h.credentialRows(); !slices.Equal(after, rows) {
		t.Error("the rewrite changed the credentials, not just where they are stored")
	}
}

// rewriteGate stops every DELETE from the credentials table at a gate the test
// opens: a statement trigger that waits for it inside the deleting
// transaction, after that transaction has taken its lock on the table. The
// rewrite deletes every row, and a confirmation prunes expired ones before it
// writes, so either can be held while the other runs into it. The trigger
// polls rather than waiting on a lock, so the sweep's lock_timeout does not
// apply to the hold.
type rewriteGate struct{ h *harness }

func newRewriteGate(h *harness) rewriteGate {
	h.t.Helper()
	h.exec(`CREATE TABLE rewrite_gate (open boolean NOT NULL)`)
	h.exec(`INSERT INTO rewrite_gate VALUES (true)`)
	h.exec(`CREATE FUNCTION wait_at_rewrite_gate() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			WHILE NOT (SELECT open FROM rewrite_gate) LOOP
				PERFORM pg_sleep(0.002);
			END LOOP;
			RETURN NULL;
		END $$`)
	h.exec(`CREATE TRIGGER wait_at_rewrite_gate BEFORE DELETE ON verification_credentials
		FOR EACH STATEMENT EXECUTE FUNCTION wait_at_rewrite_gate()`)
	// Whatever the test does, nothing is left waiting at the gate when the
	// server shuts down and the schema is dropped.
	h.t.Cleanup(func() {
		_, _ = h.app.pool.Exec(context.Background(), `UPDATE rewrite_gate SET open = true`)
	})
	return rewriteGate{h}
}

func (g rewriteGate) set(open bool) {
	g.h.t.Helper()
	g.h.exec(`UPDATE rewrite_gate SET open = $1`, open)
}

// lock reports whether a transaction holds the credentials table in mode, or
// is waiting to, when granted is false.
func (g rewriteGate) lock(mode string, granted bool) func() bool {
	return func() bool {
		var n int
		if err := g.h.app.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_locks
			WHERE relation = 'verification_credentials'::regclass AND mode = $1 AND granted = $2`,
			mode, granted).Scan(&n); err != nil {
			g.h.t.Fatalf("read pg_locks: %v", err)
		}
		return n > 0
	}
}

type sweepOutcome struct {
	report SweepReport
	err    error
}

// sweepLater runs a sweep in the background.
func (h *harness) sweepLater() <-chan sweepOutcome {
	done := make(chan sweepOutcome, 1)
	go func() {
		report, err := h.app.Sweep(context.Background())
		done <- sweepOutcome{report, err}
	}()
	return done
}

type confirmOutcome struct {
	status int
	out    verifyResponse
	err    error
}

// confirmLater sends a code for confirmation in the background, as a browser
// does whose request arrives in the middle of something.
func (h *verifyHarness) confirmLater(code string) <-chan confirmOutcome {
	done := make(chan confirmOutcome, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{"code": code})
		req, err := http.NewRequest(http.MethodPost, h.base+"/api/verify/confirm", bytes.NewReader(body))
		if err != nil {
			done <- confirmOutcome{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := h.http.Do(req)
		if err != nil {
			done <- confirmOutcome{err: err}
			return
		}
		defer res.Body.Close()
		var out verifyResponse
		err = json.NewDecoder(res.Body).Decode(&out)
		done <- confirmOutcome{status: res.StatusCode, out: out, err: err}
	}()
	return done
}

// await takes what a background request delivered, failing the test rather
// than hanging it if nothing comes.
func await[T any](t *testing.T, what string, done <-chan T) T {
	t.Helper()
	select {
	case v := <-done:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// A confirmation that has the credentials table when the rewrite starts
// holds the rewrite up until it commits, and is then in the copy: rewritten
// with the rest, once.
func TestCredentialConfirmedAsTheRewriteStartsIsRewrittenOnce(t *testing.T) {
	h := verifyServer(t, nil)
	h.addBoard("during")
	thread := h.createThread("during", "", "", "credentials come and go")
	path := fmt.Sprintf("/api/boards/during/threads/%d/posts", thread.ThreadNo)
	earlier := h.verify("earlier@iitb.ac.in").Credential
	gate := newRewriteGate(h.harness)

	if status, out := h.requestCode("arriving@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatalf("request a code: status %d (%s)", status, out.Error)
	}
	code := h.mail.last(t).Code

	gate.set(false)
	confirmed := h.confirmLater(code)
	waitUntil(t, "the confirmation has the credentials table", gate.lock("RowExclusiveLock", true))
	swept := h.sweepLater()
	waitUntil(t, "the rewrite waits for the confirmation", gate.lock("ExclusiveLock", false))
	gate.set(true)

	confirm := await(t, "the confirmation", confirmed)
	if confirm.err != nil || confirm.status != http.StatusOK {
		t.Fatalf("confirmation: status %d, %v (%s)", confirm.status, confirm.err, confirm.out.Error)
	}
	sweep := await(t, "the sweep", swept)
	if sweep.err != nil {
		t.Fatalf("sweep: %v", sweep.err)
	}
	if sweep.report.Credentials != 2 {
		t.Errorf("the sweep rewrote %d credentials, want both, the one confirmed as it started included",
			sweep.report.Credentials)
	}
	arriving := h.app.emailHash(mustParseEmail(t, "arriving@iitb.ac.in").Key)
	if n := h.countRows("verification_credentials"); n != 2 {
		t.Errorf("%d credential rows, want 2", n)
	}
	if n := h.rowsContaining("verification_credentials", arriving); n != 1 {
		t.Errorf("%d rows for the address confirmed as the rewrite started, want 1", n)
	}
	if n := h.credentialXmins(nil); n != 1 {
		t.Errorf("the credentials carry %d transaction ids, want the rewrite's alone", n)
	}
	h.badgedPost(path, earlier, "IIT Bombay")
	h.badgedPost(path, confirm.out.Credential, "IIT Bombay")
}

// A confirmation that arrives while the rewrite has the table waits for it to
// commit and then writes over the rewritten rows. Here it is a second
// verification of an address already holding a credential, the case that
// updates a row the rewrite has just deleted and inserted again: the address
// still has one row, carrying the new credential, and the old one is revoked.
func TestCredentialConfirmedDuringTheRewriteIsKeptOnce(t *testing.T) {
	h := verifyServer(t, nil)
	h.addBoard("during")
	thread := h.createThread("during", "", "", "credentials come and go")
	path := fmt.Sprintf("/api/boards/during/threads/%d/posts", thread.ThreadNo)
	stays := h.verify("stays@iitb.ac.in").Credential
	replaced := h.verify("again@iitb.ac.in").Credential
	gate := newRewriteGate(h.harness)

	if status, out := h.requestCode("again@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatalf("request a code: status %d (%s)", status, out.Error)
	}
	code := h.mail.last(t).Code

	gate.set(false)
	swept := h.sweepLater()
	waitUntil(t, "the rewrite has the credentials table", gate.lock("ExclusiveLock", true))
	confirmed := h.confirmLater(code)
	waitUntil(t, "the confirmation waits for the rewrite", gate.lock("RowExclusiveLock", false))
	gate.set(true)

	sweep := await(t, "the sweep", swept)
	if sweep.err != nil {
		t.Fatalf("sweep: %v", sweep.err)
	}
	confirm := await(t, "the confirmation", confirmed)
	if confirm.err != nil || confirm.status != http.StatusOK {
		t.Fatalf("confirmation: status %d, %v (%s)", confirm.status, confirm.err, confirm.out.Error)
	}
	if sweep.report.Credentials != 2 {
		t.Errorf("the sweep rewrote %d credentials, want the 2 there were when it started", sweep.report.Credentials)
	}
	again := h.app.emailHash(mustParseEmail(t, "again@iitb.ac.in").Key)
	if n := h.countRows("verification_credentials"); n != 2 {
		t.Errorf("%d credential rows, want 2", n)
	}
	if n := h.rowsContaining("verification_credentials", again); n != 1 {
		t.Errorf("%d rows for the address that verified again, want 1", n)
	}
	// The rewrite committed first: the row the confirmation left alone carries
	// the rewrite's transaction id, and the confirmed one an id of its own,
	// for the next sweep to take. (Not a later one: a transaction gets its id
	// when it first writes, and the confirmation wrote to the pending table
	// before the rewrite deleted anything.)
	if n := h.credentialXmins(again); n != 1 {
		t.Errorf("the rows the confirmation did not touch carry %d transaction ids, want the rewrite's", n)
	}
	var own bool
	if err := h.app.pool.QueryRow(context.Background(), `
		SELECT a.xmin::text <> s.xmin::text
		FROM verification_credentials a, verification_credentials s
		WHERE a.email_hash = $1 AND s.email_hash <> $1`, again).Scan(&own); err != nil {
		t.Fatalf("compare transaction ids: %v", err)
	}
	if !own {
		t.Error("the confirmed credential carries the rewrite's transaction id, so the rewrite saw it: " +
			"this test did not make the confirmation wait")
	}

	var out createdPost
	if status := h.postWithCredential(replaced, path,
		map[string]any{"body": "with the replaced credential", "badge": true}, &out); status != http.StatusForbidden {
		t.Errorf("the replaced credential: status %d, want 403", status)
	}
	h.badgedPost(path, stays, "IIT Bombay")
	h.badgedPost(path, confirm.out.Credential, "IIT Bombay")
}

// The rewrite and the vacuum each wait a few seconds at most for a lock, and
// a sweep that could not finish either says so in its exit status. A session
// that writes to the credentials table and stays open stops the rewrite; one
// that only reads the pending table, as an export does, stops the vacuum
// after the rewrite has committed. Either way the next sweep finishes.
func TestSweepFailsWhenTheVerificationTablesAreHeld(t *testing.T) {
	var logs syncBuffer
	h := testServerLogging(t, nil, &logs)
	ctx := context.Background()
	h.exec(`INSERT INTO verification_credentials (email_hash, credential_id_hash, label, expires_at)
		VALUES ('\x01', '\x02', 'Held College', date_trunc('day', now()) + interval '181 days')`)

	hold := func(statement string) pgx.Tx {
		tx, err := h.app.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("hold: %v", err)
		}
		return tx
	}

	writer := hold(`INSERT INTO verification_credentials (email_hash, credential_id_hash, label, expires_at)
		VALUES ('\x03', '\x04', 'Held College', date_trunc('day', now()) + interval '181 days')`)
	report, err := h.app.Sweep(ctx)
	if err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("a sweep held off the credentials table returned %v, want a lock timeout", err)
	}
	if report.Credentials != 0 {
		t.Errorf("the sweep reports %d credentials rewritten by a rewrite that never ran", report.Credentials)
	}
	// The writer was only kept waiting, and its row is there once it commits.
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the writer: %v", err)
	}
	if n := h.countRows("verification_credentials"); n != 2 {
		t.Errorf("%d credential rows, want both", n)
	}

	files := h.relationFiles("verification_credentials", "verification_pending")
	reader := hold(`SELECT count(*) FROM verification_pending`)
	report, err = h.app.Sweep(ctx)
	if err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("a sweep held off the pending table returned %v, want a lock timeout", err)
	}
	if report.Credentials != 2 {
		t.Errorf("the sweep reports %d credentials rewritten, want the 2 it rewrote before the vacuum stopped", report.Credentials)
	}
	if err := reader.Rollback(ctx); err != nil {
		t.Fatalf("end the reader: %v", err)
	}
	for _, text := range []string{logs.String(), err.Error()} {
		if strings.Contains(text, "Held College") {
			t.Errorf("the failing sweep logged a credential's label: %s", text)
		}
	}

	if _, err := h.app.Sweep(ctx); err != nil {
		t.Fatalf("sweep once nothing holds the tables: %v", err)
	}
	after := h.relationFiles("verification_credentials", "verification_pending")
	if after[0] == files[0] || after[1] == files[1] {
		t.Errorf("the tables are still in files %v after a sweep that finished, want new ones", after)
	}
}
