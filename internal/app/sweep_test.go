package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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

// A sweep whose verification step fails as well — as it does every night for
// a role that may not vacuum pg_statistic — still says that the files were
// not in its media store: the error it exits with carries both, so neither
// hides the other. A plain VACUUM stands in for that role, as in
// TestSweepFailsWhenVacuumLeavesPgStatisticInItsFile.
func TestSweepErrorNamesEveryStepThatFailed(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("elsewhere")
	h.exec(`UPDATE boards SET archive_days = 0 WHERE slug = 'elsewhere'`)
	thread := h.imageThread("elsewhere", "stored where the sweeper is not looking")
	h.archiveThread("elsewhere", thread.ThreadNo, time.Minute)

	store := h.app.media
	wrong, err := media.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("empty store: %v", err)
	}
	h.app.media = wrong
	h.app.store.statisticsVacuumCommand = "VACUUM (SKIP_LOCKED)"
	report, err := h.app.Sweep(context.Background())
	h.app.media = store
	h.app.store.statisticsVacuumCommand = vacuumCommand
	if err == nil {
		t.Fatal("a sweep that failed twice reported success")
	}
	for _, want := range []string{"EDUCHAN_MEDIA_BACKEND", "must own the database"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if report.BlobsMissing != 2 {
		t.Errorf("sweep report = %+v, want both files missing", report)
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

// cancelledRewrite runs RewriteCredentials on a store of its own, as a sweeper
// process would, holds its rewrite at the gate with the credentials table
// locked, and cancels it there, as SIGTERM does. Every connection the store
// makes, cancel requests included, goes through dialFunc, which stands in for
// the sweeper's network. The process exits as soon as the sweep returns, so a
// dial that gets through its hold only once the rewrite has returned fails:
// what pgx has not sent by then never reaches the server.
type cancelledRewrite struct {
	h         *harness
	cancelled atomic.Bool // the sweep has been cancelled
	exited    atomic.Bool // the rewrite has returned, and the process with it
	err       error       // what the rewrite returned
}

const cancelledSweeper = "cancelled-sweep"

// sqlstateQueryCanceled is the code of PostgreSQL's "canceling statement due
// to user request", its answer to a cancel request.
const sqlstateQueryCanceled = "57014"

// dialFunc is a stand-in for the sweeper's network: hold says what a dial
// made after the cancel, given its context, does before it goes out.
func (c *cancelledRewrite) dialFunc(hold func(ctx context.Context) error) pgconn.DialFunc {
	var dialer net.Dialer
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if c.cancelled.Load() {
			if err := hold(ctx); err != nil {
				return nil, err
			}
		}
		if c.exited.Load() {
			return nil, errors.New("the sweeper has exited")
		}
		return dialer.DialContext(ctx, network, addr)
	}
}

// waitFor is a hold of d, or less if the dial's context ends first.
func waitFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run starts the rewrite on a store whose dials go through hold and whose
// cancel grace is grace, cancels it at the gate, and returns once it has
// returned, with its error in c.err.
func (c *cancelledRewrite) run(grace time.Duration, hold func(ctx context.Context) error) {
	h := c.h
	h.t.Helper()
	gate := newRewriteGate(h)
	config := h.app.pool.Config()
	config.ConnConfig.RuntimeParams["application_name"] = cancelledSweeper
	config.ConnConfig.DialFunc = c.dialFunc(hold)
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		h.t.Fatalf("sweeper's pool: %v", err)
	}
	h.t.Cleanup(pool.Close)
	store := NewStore(pool)
	store.cancelGrace = grace

	gate.set(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := store.RewriteCredentials(ctx)
		done <- err
	}()
	waitUntil(h.t, "the rewrite holds the credentials table", gate.lock("ExclusiveLock", true))
	waitUntil(h.t, "the rewrite's statement is at the gate", func() bool {
		var n int
		if err := h.app.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE application_name = $1 AND state = 'active' AND query LIKE '%WITH copy%'`,
			cancelledSweeper).Scan(&n); err != nil {
			h.t.Fatalf("read pg_stat_activity: %v", err)
		}
		return n > 0
	})

	c.cancelled.Store(true)
	cancel()
	c.err = await(h.t, "the cancelled rewrite", done)
	c.exited.Store(true)
	if c.err == nil {
		h.t.Fatal("a cancelled rewrite reported success")
	}
	if !errors.Is(c.err, context.Canceled) {
		h.t.Errorf("a cancelled rewrite says %q, which does not say it was cancelled", c.err)
	}
}

// assertStopped fails the test unless the rewrite's statement has stopped on
// the server and rolled back whole. The backend ends once it reads what the
// sweeper sent last, which it can only do once the statement has stopped; the
// gate stays shut, so nothing else would stop it.
func (c *cancelledRewrite) assertStopped(issued int) {
	h := c.h
	h.t.Helper()
	backends := func() int {
		var n int
		if err := h.app.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, cancelledSweeper).Scan(&n); err != nil {
			h.t.Fatalf("read pg_stat_activity: %v", err)
		}
		return n
	}
	locked := rewriteGate{h}.lock("ExclusiveLock", true)
	deadline := time.Now().Add(3 * time.Second)
	for backends() > 0 {
		if time.Now().After(deadline) {
			h.t.Fatalf("the cancelled rewrite's statement is still running on the server after the sweep "+
				"returned (%v), holding the credentials table: %t", c.err, locked())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if locked() {
		h.t.Error("the credentials table is still locked after the cancelled rewrite ended")
	}
	// And it rolled back whole: the rows are those issued, each still under
	// its own transaction id, for the next sweep to rewrite.
	if n := h.countRows("verification_credentials"); n != issued {
		h.t.Errorf("%d credential rows after a cancelled rewrite, want the %d issued", n, issued)
	}
	if n := h.credentialXmins(nil); n != issued {
		h.t.Errorf("the credentials carry %d transaction ids after a cancelled rewrite, want their own %d",
			n, issued)
	}
}

// A sweep cancelled while a statement of RewriteCredentials runs has had
// PostgreSQL stop that statement by the time it returns. The process exits as
// soon as it does, and PostgreSQL does not notice a closed socket until the
// statement ends, so a cancel request not yet sent then never comes: a VACUUM
// FULL runs on, holding its table against every read, and commits, and a
// rewrite holds its lock to its end. Here every dial after the cancel takes
// half a second, the cancel request's included, and PostgreSQL answers it
// well within the grace.
//
// Either of two things gets the cancel request out in time, so the statement
// stopping fails only without both: the handler sweepConnection sets, which
// sends it at once and waits for PostgreSQL's answer before the statement
// returns, and closeSweepConnection's wait for the cancel request pgx's
// default sends from a goroutine. What tells the handler from the wait is the
// error: only with the handler is it PostgreSQL's, the statement cancelled
// before the rewrite returned. TestCancelledSweepWaitsForPgxsOwnCancelRequest
// covers the wait, where PostgreSQL does not answer in time.
func TestCancelledSweepStopsItsStatementOnTheServer(t *testing.T) {
	h := testServer(t, nil)
	h.issueDirectly(3)
	c := &cancelledRewrite{h: h}
	c.run(cancelGrace, func(ctx context.Context) error { return waitFor(ctx, 500*time.Millisecond) })

	var pgErr *pgconn.PgError
	if !errors.As(c.err, &pgErr) || pgErr.Code != sqlstateQueryCanceled {
		t.Errorf("a cancelled rewrite returned %q, not PostgreSQL's answer to the cancel request: "+
			"the statement may still have been running when the sweep returned", c.err)
	}
	c.assertStopped(3)
}

// When PostgreSQL does not answer the cancel request within the grace — here
// it never arrives, its dial held until it gives up — pgx gives up on the
// connection and sends a cancel request of its own from a goroutine as it
// closes it, which is all pgx's default does. The sweep waits for that one to
// go out before it returns; otherwise the process would exit first, and the
// statement would run on. The handler's cancel request is the one whose dial
// must end within the grace; pgx's own has longer.
func TestCancelledSweepWaitsForPgxsOwnCancelRequest(t *testing.T) {
	h := testServer(t, nil)
	h.issueDirectly(3)
	c := &cancelledRewrite{h: h}
	const grace = 1500 * time.Millisecond
	c.run(grace, func(ctx context.Context) error {
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= grace {
			<-ctx.Done()
			return ctx.Err()
		}
		// pgx's own waits a third of the grace before it goes out, by when
		// a sweep that did not wait for it would have returned.
		return waitFor(ctx, grace/3)
	})
	var pgErr *pgconn.PgError
	if errors.As(c.err, &pgErr) {
		t.Fatalf("the rewrite returned PostgreSQL's %q, so the handler's cancel request got through "+
			"and this test does not test the wait", pgErr.Message)
	}
	c.assertStopped(3)
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

// fileWatch follows the files some tables are stored in, which VACUUM FULL
// replaces each time it runs, to count the runs. It polls far more often than
// the sweep runs VACUUM FULL on any one table: the sweep waits at least
// vacuumFirstWait between tries, and on a test's small tables a run takes
// milliseconds.
type fileWatch struct {
	mu    sync.Mutex
	files map[string][]int64 // every file each table has been seen in, in order
	stop  chan struct{}
	done  chan struct{}
}

// watchFiles starts a fileWatch on tables, on a connection of its own so
// that it takes nothing from the server's pool.
func (h *harness) watchFiles(tables ...string) *fileWatch {
	h.t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, h.app.pool.Config().ConnConfig)
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	read := func() (map[string]int64, error) {
		rows, err := conn.Query(ctx, `
			SELECT relname::text, pg_relation_filenode(oid)::bigint FROM pg_class
			WHERE oid = ANY ($1::text[]::regclass[])`, tables)
		if err != nil {
			return nil, err
		}
		seen := map[string]int64{}
		var name string
		var file int64
		_, err = pgx.ForEachRow(rows, []any{&name, &file}, func() error {
			seen[name] = file
			return nil
		})
		return seen, err
	}
	first, err := read()
	if err != nil {
		conn.Close(ctx)
		h.t.Fatalf("read the files of %v: %v", tables, err)
	}
	w := &fileWatch{files: map[string][]int64{}, stop: make(chan struct{}), done: make(chan struct{})}
	for _, table := range tables {
		w.files[table] = []int64{first[table]}
	}
	go func() {
		defer close(w.done)
		defer conn.Close(ctx)
		for {
			select {
			case <-w.stop:
				return
			case <-time.After(time.Millisecond):
			}
			seen, err := read()
			if err != nil {
				h.t.Errorf("read the files of %v: %v", tables, err)
				return
			}
			w.mu.Lock()
			for table, file := range seen {
				if files := w.files[table]; files[len(files)-1] != file {
					w.files[table] = append(files, file)
				}
			}
			w.mu.Unlock()
		}
	}()
	h.t.Cleanup(w.close)
	return w
}

// runs counts the times VACUUM FULL has moved table to a new file so far.
func (w *fileWatch) runs(table string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.files[table]) - 1
}

// close stops the watch and waits for its last poll; it may be called more
// than once.
func (w *fileWatch) close() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}

// lockWaitWatch looks out for requests for a lock on one table that have to
// wait, which every later request for the table queues behind. It polls
// pg_locks about every millisecond, so it sees any wait that lasts a few: one
// of vacuumLockTimeout spans dozens of polls.
type lockWaitWatch struct {
	mu       sync.Mutex
	seen     []string // each waiting request found, by lock mode and query
	started  int      // polls begun
	finished int      // the number of the last poll to finish
	stop     chan struct{}
	done     chan struct{}
}

// watchLockWaits starts a lockWaitWatch on table, on a connection of its own
// so that it takes nothing from the server's pool.
func (h *harness) watchLockWaits(table string) *lockWaitWatch {
	h.t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, h.app.pool.Config().ConnConfig)
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	var oid uint32
	if err := conn.QueryRow(ctx, `SELECT $1::text::regclass::oid`, table).Scan(&oid); err != nil {
		conn.Close(ctx)
		h.t.Fatalf("find %s: %v", table, err)
	}
	w := &lockWaitWatch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer conn.Close(ctx)
		for {
			select {
			case <-w.stop:
				return
			case <-time.After(time.Millisecond):
			}
			w.mu.Lock()
			w.started++
			poll := w.started
			w.mu.Unlock()
			rows, err := conn.Query(ctx, `
				SELECT l.mode || ' for ' || coalesce(left(a.query, 60), 'pid ' || l.pid, 'a prepared transaction')
				FROM pg_locks l LEFT JOIN pg_stat_activity a ON a.pid = l.pid
				WHERE l.relation = $1 AND NOT l.granted`, oid)
			if err != nil {
				h.t.Errorf("read pg_locks: %v", err)
				return
			}
			waiting, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				h.t.Errorf("read pg_locks: %v", err)
				return
			}
			w.mu.Lock()
			for _, request := range waiting {
				if !slices.Contains(w.seen, request) {
					w.seen = append(w.seen, request)
				}
			}
			w.finished = poll
			w.mu.Unlock()
		}
	}()
	h.t.Cleanup(w.close)
	return w
}

// take returns the waiting requests seen since it was last called, once a
// poll begun after the call has finished, so that a wait which ended before
// the call is never reported by the next one.
func (w *lockWaitWatch) take() []string {
	w.mu.Lock()
	after := w.started
	w.mu.Unlock()
	for {
		w.mu.Lock()
		if w.finished > after {
			seen := w.seen
			w.seen = nil
			w.mu.Unlock()
			return seen
		}
		w.mu.Unlock()
		select {
		case <-w.done:
			// The watch failed, and said so; nothing more will come.
			return nil
		case <-time.After(time.Millisecond):
		}
	}
}

// close stops the watch and waits for its last poll; it may be called more
// than once.
func (w *lockWaitWatch) close() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
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
// and VACUUM FULL writes both tables to new files without the old versions or
// the pending rows confirmations deleted, and drops the files they were in,
// which are not wiped (the README says what that leaves). That is the
// credential rows alone: what the posts' own transaction ids still say about
// an issuance is TestIssuanceLeavesGapsAmongThePostsTransactionIDs.
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

// expiredCodesHeldByCredentials counts the pending rows of expired codes, and
// of those the ones whose email hash is a credential's: each says when the
// address holding that credential asked for a code it never confirmed, which
// was after the credential was issued.
func (h *harness) expiredCodesHeldByCredentials() (expired, joined int) {
	h.t.Helper()
	if err := h.app.pool.QueryRow(context.Background(), `
		SELECT count(*), count(c.email_hash)
		FROM verification_pending p LEFT JOIN verification_credentials c USING (email_hash)
		WHERE p.expires_at <= now()`).Scan(&expired, &joined); err != nil {
		h.t.Fatalf("count expired codes: %v", err)
	}
	return expired, joined
}

// A code that expires unconfirmed keeps its pending row until the next request
// prunes it, whenever that comes. The row's expiry, less the code's lifetime,
// is the time the code was asked for, to the microsecond, and its email hash
// is the address's: for an address that already holds a credential — a second
// browser asking for a code it never confirms, say — it joins that credential
// and says it was issued before then. So the sweep deletes those rows, before
// its rewrite, and VACUUM FULL leaves them out of the table's new file with
// the rest. A code still in flight stays, and still confirms.
func TestSweepDeletesCodesThatExpiredUnconfirmed(t *testing.T) {
	h := verifyServer(t, nil)
	h.verify("holder@iitb.ac.in")
	// The holder asks for a code again and never confirms it. Then another
	// address asks for one, which is still in flight when the sweep runs;
	// asking prunes expired codes, so it comes before they expire.
	for _, email := range []string{"holder@iitb.ac.in", "inflight@iitb.ac.in"} {
		if status, out := h.requestCode(email); status != http.StatusAccepted {
			t.Fatalf("request a code for %s: status %d (%s)", email, status, out.Error)
		}
	}
	inflight := h.mail.last(t).Code
	// The holder's code expires, as it would by the night. Beside it, enough
	// codes that expired unconfirmed that a file still holding their rows is
	// plainly bigger than one holding the code in flight alone.
	h.exec(`UPDATE verification_pending SET expires_at = now() - interval '1 second'
		WHERE email_hash IN (SELECT email_hash FROM verification_credentials)`)
	const abandoned = 300
	h.exec(`INSERT INTO verification_pending (email_hash, token_hash, code_hash, label, expires_at)
		SELECT sha256(int4send(i)), sha256(int4send(-i)), sha256(int4send(i) || int4send(i)), 'Somewhere',
		       now() - interval '1 minute'
		FROM generate_series(1, $1::int) AS i`, abandoned)

	// Positive control: before the sweep the holder's code joins the
	// credential, so the check below can see what it is looking for.
	if expired, joined := h.expiredCodesHeldByCredentials(); expired != abandoned+1 || joined != 1 {
		t.Fatalf("before the sweep %d expired codes, %d of them an address with a credential; want %d and 1",
			expired, joined, abandoned+1)
	}
	before := h.tableSize("verification_pending")

	if _, err := h.app.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if expired, joined := h.expiredCodesHeldByCredentials(); expired != 0 || joined != 0 {
		t.Errorf("after the sweep %d expired codes are left, %d of them dating a credential", expired, joined)
	}
	if n := h.countRows("verification_pending"); n != 1 {
		t.Errorf("after the sweep the pending table holds %d codes, want the one in flight", n)
	}
	// Deleted after the vacuum, the rows would be gone from the table but
	// not from its file.
	if after := h.tableSize("verification_pending"); after >= before {
		t.Errorf("the pending table took %d bytes before the sweep and %d after: "+
			"the expired codes' rows are still in its file", before, after)
	}
	h.assertNothingButLiveRows("verification_pending")

	if status, out := h.confirmCode(inflight); status != http.StatusOK {
		t.Fatalf("confirm the code in flight across the sweep: status %d (%s)", status, out.Error)
	}
}

// A confirmation locks its code's pending row first and writes to the
// credentials table after, so the sweep must not hold the credentials table
// while its delete of expired codes waits for a pending row: each would wait
// for the other until PostgreSQL broke the deadlock by failing one. That is
// why the delete runs before the rewrite's transaction rather than in it.
// Here the code expires while its confirmation is under way — checked in
// time, its row locked, and held at a gate before it writes anything else —
// so the sweep's delete wants that row and waits; then the confirmation goes
// on, and both finish.
func TestSweepDeletesExpiredCodesWithoutHoldingTheCredentialsTable(t *testing.T) {
	h := verifyServer(t, nil)
	if status, out := h.requestCode("late@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatalf("request a code: status %d (%s)", status, out.Error)
	}
	code := h.mail.last(t).Code
	h.exec(`UPDATE verification_pending SET expires_at = now() + interval '1 second'`)

	// The gate holds the confirmation's own delete of its row, which comes
	// after the row is locked and before the credentials table is touched;
	// the sweep's delete of expired codes passes straight through.
	h.exec(`CREATE TABLE confirm_gate (open boolean NOT NULL)`)
	h.exec(`INSERT INTO confirm_gate VALUES (false)`)
	h.exec(`CREATE FUNCTION wait_at_confirm_gate() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF current_query() LIKE '%token_hash%' THEN
				WHILE NOT (SELECT open FROM confirm_gate) LOOP
					PERFORM pg_sleep(0.002);
				END LOOP;
			END IF;
			RETURN NULL;
		END $$`)
	h.exec(`CREATE TRIGGER wait_at_confirm_gate BEFORE DELETE ON verification_pending
		FOR EACH STATEMENT EXECUTE FUNCTION wait_at_confirm_gate()`)
	openGate := func() {
		_, _ = h.app.pool.Exec(context.Background(), `UPDATE confirm_gate SET open = true`)
	}
	t.Cleanup(openGate)

	confirmed := h.confirmLater(code)
	var confirmPID int32
	waitUntil(t, "the confirmation has locked its code and waits at the gate", func() bool {
		err := h.app.pool.QueryRow(context.Background(), `
			SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND state = 'active'
			  AND query LIKE 'DELETE FROM verification_pending WHERE token_hash%'`).Scan(&confirmPID)
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		if err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		return true
	})
	waitUntil(t, "the code has expired", func() bool {
		var expired bool
		if err := h.app.pool.QueryRow(context.Background(),
			`SELECT expires_at <= now() FROM verification_pending`).Scan(&expired); err != nil {
			t.Fatalf("read the code's expiry: %v", err)
		}
		return expired
	})

	swept := h.sweepLater()
	waitUntil(t, "the sweep's delete waits for the confirmation", func() bool {
		var n int
		if err := h.app.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY (pg_blocking_pids(pid))`,
			confirmPID).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		return n > 0
	})
	openGate()

	confirm := await(t, "the confirmation", confirmed)
	if confirm.err != nil || confirm.status != http.StatusOK {
		t.Errorf("confirmation: status %d, %v (%s)", confirm.status, confirm.err, confirm.out.Error)
	}
	sweep := await(t, "the sweep", swept)
	if sweep.err != nil {
		t.Errorf("sweep: %v", sweep.err)
	}
	if n := h.countRows("verification_credentials"); n != 1 {
		t.Errorf("%d credential rows, want the one confirmed", n)
	}
	if n := h.countRows("verification_pending"); n != 0 {
		t.Errorf("%d pending rows, want none", n)
	}
}

// postsGap counts the transaction ids handed out after one post on a board was
// written and before another, and used by neither: what a plain query on
// posts, which the application's own role can run, finds missing between
// them. age() counts back from one transaction id for the whole statement, so
// the difference holds across wraparound.
func (h *harness) postsGap(board string, earlier, later int64) int64 {
	h.t.Helper()
	var gap int64
	if err := h.app.pool.QueryRow(context.Background(), `
		SELECT age(a.xmin)::bigint - age(b.xmin)::bigint - 1
		FROM boards
		JOIN posts a ON a.board_id = boards.id AND a.no = $2
		JOIN posts b ON b.board_id = boards.id AND b.no = $3
		WHERE boards.slug = $1`, board, earlier, later).Scan(&gap); err != nil {
		h.t.Fatalf("transaction ids between posts %d and %d: %v", earlier, later, err)
	}
	return gap
}

// The rewrite takes the issuance's transaction id off the credential row, but
// not out of the database. Requesting a code and confirming one are writes,
// and each takes a transaction id from the counter posts take theirs from;
// posts keep theirs beside their created_at, and nothing rewrites them. So
// after the sweep the ids the request and the confirmation used are still
// missing from the posts' sequence, between the two posts written around
// each, and place the issuance there as closely as the credential's own xmin
// did before the rewrite.
//
// This is a limit, not a property, and the README states it ("Transaction
// ids, and the nightly rewrite"). The test holds the README to it: the day it
// fails — the verification tables moved to a PostgreSQL instance of their
// own, say — the README can promise more. Other writers on the instance,
// other tests among them, only ever widen a gap, so it asks for at least one
// missing id in each and logs what it found; on a quiet instance each gap is
// exactly one id, and the gap between two posts with nothing between them is
// none.
func TestIssuanceLeavesGapsAmongThePostsTransactionIDs(t *testing.T) {
	h := verifyServer(t, nil)
	h.addBoard("gaps")
	ctx := context.Background()
	thread := h.createThread("gaps", "", "", "before anyone verified")
	before := h.reply("gaps", thread.ThreadNo, "", "before the request", false)
	if status, out := h.requestCode("gapped@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatalf("request a code: status %d (%s)", status, out.Error)
	}
	between := h.reply("gaps", thread.ThreadNo, "", "between the request and the confirmation", false)
	if status, out := h.confirmCode(h.mail.last(t).Code); status != http.StatusOK {
		t.Fatalf("confirm the code: status %d (%s)", status, out.Error)
	}
	after := h.reply("gaps", thread.ThreadNo, "", "after the confirmation", false)

	var lastPost int64
	if err := h.app.pool.QueryRow(ctx, `SELECT max(id) FROM posts`).Scan(&lastPost); err != nil {
		t.Fatalf("last post: %v", err)
	}
	if n := h.credentialsAmongPosts(lastPost); n != 1 {
		t.Fatalf("before the sweep %d credentials sit between two posts, want the one just issued", n)
	}
	if _, err := h.app.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// The sweep did its part: the credential's own row no longer dates it.
	if n := h.credentialsAmongPosts(lastPost); n != 0 {
		t.Fatalf("after the sweep %d credentials still sit between two posts written before it", n)
	}

	quiet := h.postsGap("gaps", thread.No, before.No)
	request := h.postsGap("gaps", before.No, between.No)
	confirm := h.postsGap("gaps", between.No, after.No)
	t.Logf("transaction ids missing between posts after the sweep: %d with nothing between them, "+
		"%d around the request, %d around the confirmation", quiet, request, confirm)
	if request < 1 {
		t.Errorf("after the sweep no transaction id is missing between the posts written around the code "+
			"request (%d); the README says the request leaves a gap there", request)
	}
	if confirm < 1 {
		t.Errorf("after the sweep no transaction id is missing between the posts written around the "+
			"confirmation (%d); the README says the confirmation leaves a gap there", confirm)
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

// The rewrite waits a few seconds at most for its lock, the vacuum keeps
// trying for a while a table it cannot take, and a sweep that could not finish
// either says so in its exit status. A session that writes to the credentials
// table and stays open stops the rewrite; one that only reads the pending
// table, as an export does, stops the vacuum after the rewrite has committed;
// and one that reads the statistics views, which holds pg_statistic, stops
// the vacuum of that catalog. Each holds back only what it holds: the vacuum
// takes the others in the same passes, pg_statistic after the verification
// tables but not waiting for them. Either way the next sweep finishes.
func TestSweepFailsWhenTheVerificationTablesAreHeld(t *testing.T) {
	var logs syncBuffer
	h := testServerLogging(t, nil, &logs)
	h.app.store.vacuumPatience = time.Second
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

	vacuumed := []string{"verification_credentials", "verification_pending", statisticsCatalog}
	files := h.relationFiles(vacuumed...)
	reader := hold(`SELECT count(*) FROM verification_pending`)
	report, err = h.app.Sweep(ctx)
	if err == nil || !strings.Contains(err.Error(), "could not take verification_pending") {
		t.Fatalf("a sweep held off the pending table returned %v, want it to say it could not take the table", err)
	}
	if report.Credentials != 2 {
		t.Errorf("the sweep reports %d credentials rewritten, want the 2 it rewrote before the vacuum stopped", report.Credentials)
	}
	// The pending table held back nothing else: the credentials table, before
	// it in the first pass, and pg_statistic, after it, went to new files then.
	for i, file := range h.relationFiles(vacuumed...) {
		switch held := vacuumed[i] == "verification_pending"; {
		case held && file != files[i]:
			t.Errorf("the pending table moved to a new file while a reader held it")
		case !held && file == files[i]:
			t.Errorf("%s is still in file %d after a sweep that could not take the pending table: "+
				"want it in a new file, not held back with that table", vacuumed[i], file)
		}
	}
	if err := reader.Rollback(ctx); err != nil {
		t.Fatalf("end the reader: %v", err)
	}
	for _, text := range []string{logs.String(), err.Error()} {
		if strings.Contains(text, "Held College") {
			t.Errorf("the failing sweep logged a credential's label: %s", text)
		}
	}

	// While VACUUM FULL waited for pg_statistic, every query planned with
	// statistics its connection had not cached would wait behind it, on any
	// table; so it passes over the catalog too, rather than wait for it.
	files = h.relationFiles("verification_credentials", "verification_pending")
	statsReader := hold(`SELECT count(*) FROM pg_stats`)
	if _, err = h.app.Sweep(ctx); err == nil || !strings.Contains(err.Error(), "could not take "+statisticsCatalog) {
		t.Fatalf("a sweep held off pg_statistic returned %v, want it to say it could not take the catalog", err)
	}
	if err := statsReader.Rollback(ctx); err != nil {
		t.Fatalf("end the statistics reader: %v", err)
	}
	for i, file := range h.relationFiles("verification_credentials", "verification_pending") {
		if file == files[i] {
			t.Errorf("a sweep held off pg_statistic left the verification tables in their files: %v", files)
			break
		}
	}

	h.app.store.vacuumPatience = vacuumPatience
	if _, err := h.app.Sweep(ctx); err != nil {
		t.Fatalf("sweep once nothing holds the tables: %v", err)
	}
	after := h.relationFiles("verification_credentials", "verification_pending")
	if after[0] == files[0] || after[1] == files[1] {
		t.Errorf("the tables are still in files %v after a sweep that finished, want new ones", after)
	}
}

// A sweep whose vacuum runs out of patience says how long it tried, which is
// not its patience: it gives up once the next pause would take it past
// vacuumPatience, and with the pauses doubling up to vacuumMaxWait that is
// about four seconds early on the minute it has in production. With three
// seconds of patience and nothing else slow, it pauses for 0.1, 0.2, 0.4 and
// 0.8 seconds, and gives up at 1.5 seconds rather than pause 1.6 more; the
// error must give a time no shorter than those pauses and no longer than the
// sweep took.
func TestSweepSaysHowLongItTriedToVacuum(t *testing.T) {
	h := testServer(t, nil)
	h.app.store.vacuumPatience = 3 * time.Second
	h.holdOpen(`SELECT count(*) FROM verification_pending`)

	start := time.Now()
	_, err := h.app.Sweep(context.Background())
	took := time.Since(start)
	if err == nil {
		t.Fatal("a sweep held off the pending table succeeded")
	}
	_, rest, found := strings.Cut(err.Error(), "; tried for ")
	tried, _, ended := strings.Cut(rest, ", and the next sweep tries again")
	if !found || !ended {
		t.Fatalf("a sweep whose vacuum ran out of patience returned %q, want it to say how long it tried", err)
	}
	said, err := time.ParseDuration(tried)
	if err != nil {
		t.Fatalf("the sweep says it tried for %q: %v", tried, err)
	}
	const paused = 1500 * time.Millisecond
	if said < paused || said > took {
		t.Errorf("the sweep says it tried for %s, want the time it spent: no less than the %s it paused "+
			"between tries, and no more than the %s the whole sweep took", said, paused,
			took.Round(time.Millisecond))
	}
}

// tableSize is the size of a table's file, which holds every row version
// VACUUM FULL kept, live or not.
func (h *harness) tableSize(table string) int64 {
	h.t.Helper()
	var size int64
	if err := h.app.pool.QueryRow(context.Background(),
		`SELECT pg_relation_size($1::text::regclass)`, table).Scan(&size); err != nil {
		h.t.Fatalf("size of %s: %v", table, err)
	}
	return size
}

// issueDirectly writes n credentials straight into the table, each in a
// transaction of its own, as n confirmations would: n rows with n
// transaction ids, enough of them that a file holding their old versions as
// well is plainly bigger than one holding the rows alone.
func (h *harness) issueDirectly(n int) {
	h.t.Helper()
	for i := range n {
		h.exec(`INSERT INTO verification_credentials (email_hash, credential_id_hash, label, expires_at)
			VALUES (int4send($1::int), sha256(int4send($1::int)), 'Somewhere',
			        date_trunc('day', now()) + interval '181 days')`, i)
	}
}

// assertNothingButLiveRows fails the test unless a table's file holds only its
// rows: a VACUUM FULL run now, with nothing written since the sweep, has
// nothing left to take out.
func (h *harness) assertNothingButLiveRows(table string) {
	h.t.Helper()
	swept := h.tableSize(table)
	h.exec(`VACUUM FULL ` + pgx.Identifier{table}.Sanitize())
	if again := h.tableSize(table); again != swept {
		h.t.Errorf("%s took %d bytes after the sweep and %d after one more VACUUM FULL: "+
			"the sweep left deleted row versions in it", table, swept, again)
	}
}

// VACUUM FULL keeps a deleted row, xmin and all, while any snapshot older than
// its cutoff could still read it, and that cutoff is held back by any
// statement or transaction in the database that began before the rewrite
// committed, or by a transaction anywhere on the instance that had written
// something by then. So every version the rewrite replaced would go into the
// new file, issuance xmins and all, with the file's number changed as if
// nothing were wrong, after a run that held up every request on the table.
// The sweep must neither report success then nor run VACUUM FULL on a
// verification table at all: it waits for the older transaction to end, and
// fails when it outlasts the sweep's patience. Both kinds of holder are here,
// on the posts rather than a verification table — a snapshot, as a long read
// or an export keeps, and a transaction that has written something, as a
// reply in flight is.
func TestSweepFailsWhileAnOlderTransactionCanReadTheOldRows(t *testing.T) {
	const issued = 200
	for _, holder := range []struct {
		name      string
		isolation pgx.TxIsoLevel
		statement string
	}{
		{"snapshot", pgx.RepeatableRead, `SELECT count(*) FROM posts`},
		// Asking for its id is what writing anything would give it.
		{"writer", pgx.ReadCommitted, `SELECT pg_current_xact_id()`},
	} {
		t.Run(holder.name, func(t *testing.T) {
			var logs syncBuffer
			h := testServerLogging(t, nil, &logs)
			h.app.store.vacuumPatience = time.Second
			ctx := context.Background()
			h.issueDirectly(issued)

			held, err := h.app.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: holder.isolation})
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			t.Cleanup(func() { _ = held.Rollback(ctx) })
			if _, err := held.Exec(ctx, holder.statement); err != nil {
				t.Fatalf("hold: %v", err)
			}

			files := h.relationFiles(verificationTables[:]...)
			probe := h.relationFiles(vacuumProbeTable)[0]
			report, err := h.app.Sweep(ctx)
			if err == nil || !strings.Contains(err.Error(), "would keep the rows the rewrite deleted") {
				t.Fatalf("a sweep that overlapped an older transaction returned %v, "+
					"want it to say VACUUM FULL would keep the old rows", err)
			}
			if report.Credentials != issued {
				t.Errorf("the sweep rewrote %d credentials, want %d", report.Credentials, issued)
			}
			for i, file := range h.relationFiles(verificationTables[:]...) {
				if file != files[i] {
					t.Errorf("the sweep ran VACUUM FULL on %s while an older transaction kept it from "+
						"taking the old rows out, holding up every request on the table for nothing",
						verificationTables[i])
				}
			}
			if h.relationFiles(vacuumProbeTable)[0] == probe {
				t.Errorf("%s is still in its file: the sweep never probed the cutoff", vacuumProbeTable)
			}
			if strings.Contains(logs.String(), "Somewhere") || strings.Contains(err.Error(), "Somewhere") {
				t.Errorf("the failing sweep logged a credential's label")
			}
			// Positive controls: the file the failed sweep left is the bigger
			// for the old versions, and a snapshot from before the rewrite
			// still reads them out of it, each with its issuance xmin.
			kept := h.tableSize("verification_credentials")
			if holder.isolation == pgx.RepeatableRead {
				var xmins int
				if err := held.QueryRow(ctx,
					`SELECT count(DISTINCT xmin::text) FROM verification_credentials`).Scan(&xmins); err != nil {
					t.Fatalf("read the old rows: %v", err)
				}
				if xmins != issued {
					t.Errorf("the older snapshot reads %d transaction ids, want the %d issuances'", xmins, issued)
				}
			}
			if err := held.Rollback(ctx); err != nil {
				t.Fatalf("end the older transaction: %v", err)
			}

			h.app.store.vacuumPatience = vacuumPatience
			if _, err := h.app.Sweep(ctx); err != nil {
				t.Fatalf("sweep once the older transaction has ended: %v", err)
			}
			if size := h.tableSize("verification_credentials"); size >= kept {
				t.Errorf("the credentials table takes %d bytes after a sweep that finished and took %d after "+
					"one that kept the old rows; the size cannot tell them apart", size, kept)
			}
			h.assertNothingButLiveRows("verification_credentials")
			if n := h.credentialXmins(nil); n != 1 {
				t.Errorf("the credentials carry %d transaction ids, want the last sweep's alone", n)
			}
		})
	}
}

// An older transaction that ends while the sweep is trying costs a wait, not
// a failed run, and not a run of VACUUM FULL on a verification table either:
// a run then would hold every request on the table up and leave the old
// versions in the new file, so the sweep probes the cutoff on a table of its
// own until the older transaction has ended, and then takes each
// verification table once.
func TestSweepWaitsForAnOlderTransactionToEnd(t *testing.T) {
	const issued = 200
	h := testServer(t, nil)
	ctx := context.Background()
	h.issueDirectly(issued)

	held, err := h.app.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = held.Rollback(ctx) })
	if _, err := held.Exec(ctx, `SELECT count(*) FROM posts`); err != nil {
		t.Fatalf("hold a snapshot: %v", err)
	}
	watch := h.watchFiles(append([]string{vacuumProbeTable}, verificationTables[:]...)...)
	swept := h.sweepLater()
	// Four probes are three decisions to wait, the last after 0.3 s of
	// waiting; a run on the credentials table also ends the wait, so that a
	// sweep that takes it too soon fails below rather than here.
	waitUntil(t, "the sweep has probed the cutoff four times", func() bool {
		return watch.runs(vacuumProbeTable) >= 4 || watch.runs("verification_credentials") > 0
	})
	// The rewrite has committed: the file holds its rows and the old versions.
	kept := h.tableSize("verification_credentials")
	for _, table := range verificationTables {
		if n := watch.runs(table); n != 0 {
			t.Errorf("the sweep ran VACUUM FULL on %s %d times while a transaction older than the rewrite "+
				"was open, holding up every request on it each time; want none until that transaction ends",
				table, n)
		}
	}

	// Positive control: the snapshot predates the rewrite, and still reads
	// the old versions, each with its issuance xmin.
	var xmins int
	if err := held.QueryRow(ctx,
		`SELECT count(DISTINCT xmin::text) FROM verification_credentials`).Scan(&xmins); err != nil {
		t.Fatalf("read the old rows: %v", err)
	}
	if xmins != issued {
		t.Fatalf("the older snapshot reads %d transaction ids, want the %d issuances'", xmins, issued)
	}
	if err := held.Rollback(ctx); err != nil {
		t.Fatalf("end the older transaction: %v", err)
	}

	sweep := await(t, "the sweep", swept)
	if sweep.err != nil {
		t.Fatalf("a sweep whose older transaction ended while it waited: %v", sweep.err)
	}
	watch.close()
	for _, table := range verificationTables {
		if n := watch.runs(table); n != 1 {
			t.Errorf("the sweep ran VACUUM FULL on %s %d times, want once, after the older transaction ended",
				table, n)
		}
	}
	if size := h.tableSize("verification_credentials"); size >= kept {
		t.Errorf("the credentials table takes %d bytes after the sweep, as many as the %d it took "+
			"with the old versions in it", size, kept)
	}
	h.assertNothingButLiveRows("verification_credentials")
}

// VACUUM FULL skips, with nothing worse than a warning, a table its role may
// not vacuum, which leaves the table in its old file with every old version
// in it. Waiting will not change that, so the sweep fails at once and says
// why, and its log is still counts alone. Here a plain VACUUM, which also
// leaves each table where it is and warns of nothing, stands in for a role
// that does not own the tables, which a test cannot make.
func TestSweepFailsWhenVacuumLeavesATableInItsFile(t *testing.T) {
	var logs syncBuffer
	h := testServerLogging(t, nil, &logs)
	h.exec(`INSERT INTO verification_credentials (email_hash, credential_id_hash, label, expires_at)
		VALUES ('\x01', '\x02', 'Unowned College', '2033-03-03T00:00:00Z')`)
	h.app.store.vacuumCommand = "VACUUM (SKIP_LOCKED)"

	start := time.Now()
	before := len(logs.String())
	report, err := h.app.Sweep(context.Background())
	if err == nil || !strings.Contains(err.Error(), "must own the verification tables") {
		t.Fatalf("a sweep whose VACUUM FULL left the tables in their files returned %v, "+
			"want it to say the role must own them", err)
	}
	if took := time.Since(start); took > vacuumPatience/2 {
		t.Errorf("the sweep took %s to give up on something waiting cannot change", took)
	}
	if report.Credentials != 1 {
		t.Errorf("the sweep reports %d credentials rewritten, want the 1 it rewrote", report.Credentials)
	}

	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()[before:]), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		records = append(records, entry)
	}
	if len(records) != 1 || records[0]["msg"] != "sweep" {
		t.Fatalf("the sweep logged %v, want exactly one sweep record", records)
	}
	record := records[0]
	delete(record, "time")
	delete(record, "level")
	delete(record, "msg")
	want := map[string]any{"threads": 0.0, "posts": 0.0, "attachments": 0.0,
		"blobs_removed": 0.0, "blob_failures": 0.0, "blobs_missing": 0.0, "credentials_rewritten": 1.0}
	if fmt.Sprint(record) != fmt.Sprint(want) {
		t.Errorf("sweep record = %v, want exactly %v", record, want)
	}
	for _, text := range []string{logs.String(), err.Error()} {
		if strings.Contains(text, "Unowned") || strings.Contains(text, "2033-03-03") {
			t.Errorf("the failing sweep logged something of a credential's row: %s", text)
		}
	}
}

// pg_statistic can be vacuumed only by a superuser or a role with the
// privileges of the database's owner: the owner, or a member of its role that
// inherits them. Owning the verification tables, which is enough for them, is
// not enough for it, and nor is a membership granted WITH INHERIT FALSE. A
// role that cannot leaves it in its files with the samples 0015 deleted, with
// nothing worse than a warning, and the sweep fails at once and says what the
// role lacks and how to check for it, since a member that does not inherit
// would otherwise read the message as already met. A plain VACUUM stands in
// for that role here, as for the tables above.
func TestSweepFailsWhenVacuumLeavesPgStatisticInItsFile(t *testing.T) {
	h := testServer(t, nil)
	h.app.store.statisticsVacuumCommand = "VACUUM (SKIP_LOCKED)"
	files := h.relationFiles(verificationTables[:]...)

	start := time.Now()
	_, err := h.app.Sweep(context.Background())
	if err == nil || !strings.Contains(err.Error(), "must own the database") {
		t.Fatalf("a sweep whose VACUUM FULL left pg_statistic in its file returned %v, "+
			"want it to say the role must own the database", err)
	}
	for _, want := range []string{"inherits its privileges", "pg_has_role(datdba, 'USAGE')"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if took := time.Since(start); took > vacuumPatience/2 {
		t.Errorf("the sweep took %s to give up on something waiting cannot change", took)
	}
	for i, file := range h.relationFiles(verificationTables[:]...) {
		if file == files[i] {
			t.Errorf("%s is still in its file: the verification tables come before pg_statistic",
				verificationTables[i])
		}
	}
}

// While the sweep cannot vacuum a table another session holds — an export
// reading the credentials, say — badged posts must not queue behind it: each
// one waiting would hold one of its instance's few database connections, and
// once they held them all every request on the instance would wait. VACUUM
// FULL waiting for its lock queues every request for the table behind it for
// as long as it waits: with the sweep's lock_timeout, vacuumLockTimeout at a
// time, try after try, which no timing of a post can tell from a slow
// machine. So the test watches pg_locks for the length of the sweep for what
// a post would queue behind, a request for the credentials table that is
// waiting, and there must never be one: the sweep's vacuum takes a table only
// when nobody holds it (SKIP_LOCKED), and otherwise tries again later. (Its
// run, once it has a table, holds them up for as long as it lasts, which is
// why the sweep runs it only when it can do its job:
// TestBadgedPostsDoNotQueueBehindVacuumsAnOlderSnapshotWouldSpoil.)
func TestBadgedPostsDoNotQueueBehindTheSweepsVacuum(t *testing.T) {
	h := verifyServer(t, nil)
	h.app.store.vacuumPatience = 3 * time.Second
	h.addBoard("busy")
	thread := h.createThread("busy", "", "", "posting through the sweep")
	path := fmt.Sprintf("/api/boards/busy/threads/%d/posts", thread.ThreadNo)
	credential := h.verify("export@iitb.ac.in").Credential
	ctx := context.Background()

	export, err := h.app.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = export.Rollback(ctx) })
	if _, err := export.Exec(ctx, `SELECT count(*) FROM verification_credentials`); err != nil {
		t.Fatalf("read the credentials: %v", err)
	}

	waits := h.watchLockWaits("verification_credentials")
	// Positive control: the watch sees a wait as short as one of the vacuum's
	// would be, were it to wait for the export.
	if err := pgx.BeginFunc(ctx, h.app.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, vacuumLockTimeout); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `LOCK TABLE verification_credentials IN ACCESS EXCLUSIVE MODE`)
		return err
	}); err == nil || !strings.Contains(err.Error(), sqlstateLockNotAvailable) {
		t.Fatalf("asking for the table the export holds returned %v, want it to wait out its lock timeout", err)
	}
	if seen := waits.take(); len(seen) == 0 {
		t.Fatalf("the watch saw nothing of a request that waited %s for the credentials table: "+
			"it cannot see the wait it is for", vacuumLockTimeout)
	}

	pending := h.relationFiles("verification_pending")[0]
	swept := h.sweepLater()
	// The vacuum takes the credentials table first and the pending table next,
	// so the pending table in a new file means the sweep has tried the
	// credentials once.
	waitUntil(t, "the sweep reaches the vacuum", func() bool {
		return h.relationFiles("verification_pending")[0] != pending
	})

	start := time.Now()
	h.badgedPost(path, credential, "IIT Bombay")
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("a badged post took %s while the sweep tried to vacuum a table an export held: "+
			"it queued behind the vacuum", took)
	}

	sweep := await(t, "the sweep", swept)
	if seen := waits.take(); len(seen) > 0 {
		t.Errorf("while the sweep ran, requests for the credentials table waited behind the export "+
			"(%s): VACUUM FULL waited for the table, and every request on it queued behind it",
			strings.Join(seen, "; "))
	}
	waits.close()
	if sweep.err == nil || !strings.Contains(sweep.err.Error(), "could not take verification_credentials") {
		t.Errorf("a sweep that could not take the credentials table returned %v, want it to say so", sweep.err)
	}
	if err := export.Rollback(ctx); err != nil {
		t.Fatalf("end the export: %v", err)
	}
	h.app.store.vacuumPatience = vacuumPatience
	if _, err := h.app.Sweep(ctx); err != nil {
		t.Fatalf("sweep once the export has ended: %v", err)
	}
}

// VACUUM FULL holds the table it rewrites against every read as well as every
// write, so a run on the credentials table holds up each badged post that
// arrives during it, and once the waiting posts hold all of an instance's
// pooled connections, every request. While a transaction older than the
// rewrite is open — here a long report on the posts, which never touches a
// verification table, so SKIP_LOCKED has nothing to pass over — no run can
// take the old rows out, and the sweep must run none: it probes the cutoff,
// waits, and fails when its patience runs out, and the badged posts sent
// throughout never meet a VACUUM FULL. The pool is the size an instance has
// by default.
func TestBadgedPostsDoNotQueueBehindVacuumsAnOlderSnapshotWouldSpoil(t *testing.T) {
	h := verifyServer(t, func(cfg *Config) { cfg.DBMaxConns = defaultDBMaxConns })
	h.app.store.vacuumPatience = 2 * time.Second
	h.addBoard("report")
	thread := h.createThread("report", "", "", "posting through a long report")
	path := fmt.Sprintf("/api/boards/report/threads/%d/posts", thread.ThreadNo)
	credential := h.verify("report@iitb.ac.in").Credential
	ctx := context.Background()

	// The report reads from one snapshot on a connection of its own, as a
	// psql session or an export would.
	conn, err := pgx.ConnectConfig(ctx, h.app.pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	report, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := report.Exec(ctx, `SELECT count(*) FROM posts`); err != nil {
		t.Fatalf("read the posts: %v", err)
	}

	watch := h.watchFiles(append([]string{vacuumProbeTable}, verificationTables[:]...)...)
	swept := h.sweepLater()
	var (
		sweep   sweepOutcome
		slowest time.Duration
	)
	for deadline, done := time.Now().Add(30*time.Second), false; !done; {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the sweep")
		}
		start := time.Now()
		h.badgedPost(path, credential, "IIT Bombay")
		slowest = max(slowest, time.Since(start))
		select {
		case sweep = <-swept:
			done = true
		case <-time.After(20 * time.Millisecond):
		}
	}
	if sweep.err == nil || !strings.Contains(sweep.err.Error(), "would keep the rows the rewrite deleted") {
		t.Errorf("a sweep that overlapped a long report returned %v, want it to say VACUUM FULL "+
			"would keep the old rows", sweep.err)
	}
	if sweep.report.Credentials != 1 {
		t.Errorf("the sweep rewrote %d credentials, want 1", sweep.report.Credentials)
	}
	for _, table := range verificationTables {
		if n := watch.runs(table); n != 0 {
			t.Errorf("the sweep ran VACUUM FULL on %s %d times while a report older than the rewrite was "+
				"open, holding up every badged post each time for a run that could not do its job", table, n)
		}
	}
	// Positive control: the sweep was trying, and waiting, while the posts
	// went up.
	if n := watch.runs(vacuumProbeTable); n < 2 {
		t.Errorf("the sweep probed the cutoff %d times while the report was open, want it to keep trying", n)
	}
	if slowest > 2*time.Second {
		t.Errorf("a badged post took %s while the sweep waited for a long report to end", slowest)
	}

	if err := report.Rollback(ctx); err != nil {
		t.Fatalf("end the report: %v", err)
	}
	h.app.store.vacuumPatience = vacuumPatience
	if _, err := h.app.Sweep(ctx); err != nil {
		t.Fatalf("sweep once the report has ended: %v", err)
	}
	watch.close()
	for _, table := range verificationTables {
		if n := watch.runs(table); n != 1 {
			t.Errorf("the sweep after the report ran VACUUM FULL on %s %d times, want once", table, n)
		}
	}
	h.badgedPost(path, credential, "IIT Bombay")
}

// The sweep tells a table VACUUM skipped because another session held it from
// one it skipped because the role may not vacuum it by the warning VACUUM
// sends, and client_min_messages decides whether the client is sent warnings
// at all. Set to error — in DATABASE_URL, as here, or as a role's or the
// database's default — it would make a moment's reader on a verification
// table look like a role that does not own it, and the sweep fail at once
// instead of waiting the reader out. The sweeper's own connection asks for
// warnings whatever the pool's connections ask for.
func TestSweepHearsVacuumsWarningsWhateverTheConnectionAsksFor(t *testing.T) {
	h := testServer(t, func(cfg *Config) { cfg.DatabaseURL += "&client_min_messages=error" })
	ctx := context.Background()
	h.exec(`INSERT INTO verification_credentials (email_hash, credential_id_hash, label, expires_at)
		VALUES ('\x01', '\x02', 'Quiet College', date_trunc('day', now()) + interval '181 days')`)
	// Positive control: the pool's connections really are sent no warnings.
	var level string
	if err := h.app.pool.QueryRow(ctx, `SHOW client_min_messages`).Scan(&level); err != nil {
		t.Fatalf("read client_min_messages: %v", err)
	}
	if level != "error" {
		t.Fatalf("the pool's connections have client_min_messages %q, want error; the test cannot see the bug", level)
	}

	reader, err := h.app.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = reader.Rollback(ctx) })
	if _, err := reader.Exec(ctx, `SELECT count(*) FROM verification_credentials`); err != nil {
		t.Fatalf("read the credentials: %v", err)
	}

	files := h.relationFiles("verification_credentials", "verification_pending")
	swept := h.sweepLater()
	// The vacuum takes the credentials table first and the pending table
	// next, so the pending table in a new file means the credentials were
	// tried, and passed over, at least once.
	deadline := time.After(10 * time.Second)
	for h.relationFiles("verification_pending")[0] == files[1] {
		select {
		case sweep := <-swept:
			t.Fatalf("the sweep ended (%v) while a reader held the credentials table, "+
				"before it had vacuumed the pending table; want it to pass over the credentials and wait", sweep.err)
		case <-deadline:
			t.Fatal("timed out waiting for the sweep to vacuum the pending table")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := reader.Rollback(ctx); err != nil {
		t.Fatalf("end the reader: %v", err)
	}

	sweep := await(t, "the sweep", swept)
	if sweep.err != nil {
		t.Fatalf("a sweep whose reader ended while it waited: %v", sweep.err)
	}
	if after := h.relationFiles("verification_credentials")[0]; after == files[0] {
		t.Errorf("the credentials table is still in file %d after a sweep that finished", after)
	}
}

// issueMany writes n credentials straight into the table in one statement:
// enough of them that a run of VACUUM FULL on the table takes long enough to
// be seen copying it, which issueDirectly's transaction a row would take far
// longer to write.
func (h *harness) issueMany(n int) {
	h.t.Helper()
	h.exec(`INSERT INTO verification_credentials (email_hash, credential_id_hash, label, expires_at)
		SELECT int4send(i), sha256(int4send(i)), 'Somewhere', date_trunc('day', now()) + interval '181 days'
		FROM generate_series(1, $1::int) AS i`, n)
}

// waitLonger is waitUntil for a wait that copying a big table, or the sweep's
// growing pauses between tries, can stretch past waitUntil's few seconds.
func waitLonger(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// vacuumRunWatch follows the runs of VACUUM FULL on one table, whether or not
// they moved it to a new file: a run that gives the table up leaves it in its
// old file, where a fileWatch sees nothing, though it held every request on
// the table up for as long as it ran. It polls pg_stat_progress_cluster about
// every millisecond, where a run shows from the moment it has the table until
// it ends, and tells one run from the next by its backend and the time its
// statement began. A run on a table of a hundred thousand rows lasts hundreds
// of polls.
type vacuumRunWatch struct {
	mu   sync.Mutex
	seen []vacuumRun // every run seen, in the order they began
	stop chan struct{}
	done chan struct{}
}

// vacuumRun is what a vacuumRunWatch saw of one run.
type vacuumRun struct {
	id      string   // its backend and the time its statement began
	phases  []string // each phase it was seen in, in order
	written int64    // the most rows it was seen to have written to its new file
}

func (r vacuumRun) String() string {
	return fmt.Sprintf("{wrote %d rows; %s}", r.written, strings.Join(r.phases, ", "))
}

// watchVacuumRuns starts a vacuumRunWatch on table, on a connection of its own
// so that it takes nothing from the server's pool.
func (h *harness) watchVacuumRuns(table string) *vacuumRunWatch {
	h.t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, h.app.pool.Config().ConnConfig)
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	var oid uint32
	if err := conn.QueryRow(ctx, `SELECT $1::text::regclass::oid`, table).Scan(&oid); err != nil {
		conn.Close(ctx)
		h.t.Fatalf("find %s: %v", table, err)
	}
	w := &vacuumRunWatch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer conn.Close(ctx)
		for {
			select {
			case <-w.stop:
				return
			case <-time.After(time.Millisecond):
			}
			rows, err := conn.Query(ctx, `
				SELECT p.pid::text || ' ' || a.query_start::text, p.phase, p.heap_tuples_written
				FROM pg_stat_progress_cluster p JOIN pg_stat_activity a USING (pid)
				WHERE p.relid = $1 AND p.command = 'VACUUM FULL'
				ORDER BY a.query_start`, oid)
			if err != nil {
				h.t.Errorf("read pg_stat_progress_cluster: %v", err)
				return
			}
			var now vacuumRun
			var phase string
			w.mu.Lock()
			_, err = pgx.ForEachRow(rows, []any{&now.id, &phase, &now.written}, func() error {
				i := slices.IndexFunc(w.seen, func(r vacuumRun) bool { return r.id == now.id })
				if i < 0 {
					w.seen = append(w.seen, vacuumRun{id: now.id})
					i = len(w.seen) - 1
				}
				run := &w.seen[i]
				if len(run.phases) == 0 || run.phases[len(run.phases)-1] != phase {
					run.phases = append(run.phases, phase)
				}
				run.written = max(run.written, now.written)
				return nil
			})
			w.mu.Unlock()
			if err != nil {
				h.t.Errorf("read pg_stat_progress_cluster: %v", err)
				return
			}
		}
	}()
	h.t.Cleanup(w.close)
	return w
}

// runs returns every run seen so far.
func (w *vacuumRunWatch) runs() []vacuumRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.seen)
}

// close stops the watch and waits for its last poll; it may be called more
// than once.
func (w *vacuumRunWatch) close() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}

// vacuumConn connects as the sweep does, with its lock timeout.
func (h *harness) vacuumConn() *pgx.Conn {
	h.t.Helper()
	ctx := context.Background()
	config := h.app.pool.Config().ConnConfig
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["lock_timeout"] = vacuumLockTimeout
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	h.t.Cleanup(func() { conn.Close(ctx) })
	return conn
}

// SKIP_LOCKED covers only the table VACUUM FULL is given. Once it has that
// table it needs more, and waits for each while it holds the table against
// every read (vacuumLockTimeout says what). Two come only after it has copied
// the whole table: its row in pg_class, which it updates, and so waits for
// any transaction that has updated the row and not ended — a GRANT on the
// table, which takes no lock on the table itself — and a lock on each of its
// indexes, which it rebuilds — held by a COMMENT ON INDEX, say. A run while a
// session holds either would hold every request on the table up for the
// length of the copy and then give the table up, and tried again would do the
// same, try after try for the sweep's patience. So the sweep looks first, and
// runs no VACUUM FULL on a table while such a holder is there; it waits for
// the holder to end, and then takes the table once. The table is big enough
// that a run takes a while: the positive control shows a run copying all of
// it and then waiting out the sweep's lock timeout. (A transaction older than
// the rewrite would keep the vacuum off the tables altogether, so the holder
// begins after the rewrite: a reader holds the credentials table until the
// vacuum has passed over it once, which it does after the rewrite has
// committed.)
func TestSweepRunsNoVacuumWhileItWouldWait(t *testing.T) {
	const issued = 100_000
	for _, holder := range []struct{ name, statement string }{
		{"GRANT", `GRANT SELECT ON verification_credentials TO PUBLIC`},
		{"COMMENT ON INDEX", `COMMENT ON INDEX verification_credentials_expires_idx IS NULL`},
	} {
		t.Run(holder.name, func(t *testing.T) {
			h := testServer(t, nil)
			ctx := context.Background()
			h.issueMany(issued)

			// Positive control: a run while the holder is open copies the
			// whole table, then waits out the sweep's lock timeout, and leaves
			// the table in its old file.
			before := h.relationFiles("verification_credentials")[0]
			held := h.holdOpen(holder.statement)
			control := h.watchVacuumRuns("verification_credentials")
			_, err := h.vacuumConn().Exec(ctx, h.app.store.vacuumCommand+" verification_credentials")
			if pgErr := (*pgconn.PgError)(nil); !errors.As(err, &pgErr) || pgErr.Code != sqlstateLockNotAvailable {
				t.Fatalf("VACUUM FULL returned %v while a %s was open, want it to wait out its lock timeout: "+
					"the test cannot see what it is for", err, holder.name)
			}
			control.close()
			if runs := control.runs(); len(runs) != 1 || runs[0].written != issued {
				t.Fatalf("the watch saw %v of a VACUUM FULL that waited for a %s, want one run that had "+
					"copied all %d rows: the test cannot see what it is for", runs, holder.name, issued)
			}
			if after := h.relationFiles("verification_credentials")[0]; after != before {
				t.Fatalf("a VACUUM FULL that waited out its lock timeout moved the table to file %d", after)
			}
			if err := held.Rollback(ctx); err != nil {
				t.Fatalf("end the %s: %v", holder.name, err)
			}

			reader := h.holdOpen(`SELECT count(*) FROM verification_credentials`)
			runs := h.watchVacuumRuns("verification_credentials")
			files := h.watchFiles(vacuumProbeTable, "verification_pending")
			swept := h.sweepLater()
			waitLonger(t, "the sweep has vacuumed the pending table", func() bool {
				return files.runs("verification_pending") > 0
			})
			held = h.holdOpen(holder.statement)
			if err := reader.Rollback(ctx); err != nil {
				t.Fatalf("end the reader: %v", err)
			}
			// Each try probes the cutoff first. A run on the credentials table
			// also ends the wait, so that a sweep that runs one fails below
			// rather than here.
			tries := files.runs(vacuumProbeTable)
			waitLonger(t, "the sweep has tried four more times", func() bool {
				return files.runs(vacuumProbeTable) >= tries+4 || len(runs.runs()) > 0
			})
			if seen := runs.runs(); len(seen) > 0 {
				t.Errorf("the sweep ran VACUUM FULL on the credentials table %d times while a %s was open (%v), "+
					"holding up every request on it each time for a run that would give it up; want none until "+
					"the %[2]s ends", len(seen), holder.name, seen)
			}
			select {
			case sweep := <-swept:
				t.Fatalf("the sweep ended (%v) while a %s was open; want it to wait for it", sweep.err, holder.name)
			default:
			}
			if err := held.Rollback(ctx); err != nil {
				t.Fatalf("end the %s: %v", holder.name, err)
			}

			sweep := await(t, "the sweep", swept)
			if sweep.err != nil {
				t.Fatalf("a sweep whose %s ended while it waited: %v", holder.name, sweep.err)
			}
			runs.close()
			if seen := runs.runs(); len(seen) != 1 {
				t.Errorf("the sweep ran VACUUM FULL on the credentials table %d times (%v), want once, after the "+
					"%s ended", len(seen), seen, holder.name)
			}
			if after := h.relationFiles("verification_credentials")[0]; after == before {
				t.Errorf("the credentials table is still in file %d after a sweep that finished", after)
			}
			h.assertNothingButLiveRows("verification_credentials")
		})
	}
}

// What the sweep cannot see coming is a GRANT that begins during a run, after
// the sweep has looked: the run copies the table, then waits for the GRANT.
// It gives the table up after vacuumLockTimeout, rather than hold it for the
// connection's lock_timeout and then fail, and the sweep tries again later;
// and from then on the look before each try sees the GRANT, so the sweep runs
// no VACUUM FULL on the table again until it has ended, rather than copy the
// table only to give it up, try after try. The table is big enough that the
// copy takes a while, so the GRANT can begin during it.
func TestVacuumLetsGoOfATableWhileAnotherLockItNeedsIsHeld(t *testing.T) {
	const issued = 200_000
	h := testServer(t, nil)
	ctx := context.Background()
	h.issueMany(issued)
	before := h.relationFiles("verification_credentials")[0]

	runs := h.watchVacuumRuns("verification_credentials")
	probes := h.watchFiles(vacuumProbeTable)
	swept := h.sweepLater()
	waitLonger(t, "the sweep's VACUUM FULL is copying the credentials table", func() bool {
		var copying bool
		if err := h.app.pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT FROM pg_stat_progress_cluster
			               WHERE relid = 'verification_credentials'::regclass
			                 AND phase IN ('initializing', 'seq scanning heap'))`).Scan(&copying); err != nil {
			t.Fatalf("read pg_stat_progress_cluster: %v", err)
		}
		return copying
	})
	granter := h.holdOpen(`GRANT SELECT ON verification_credentials TO PUBLIC`)
	var grantPID int32
	if err := granter.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&grantPID); err != nil {
		t.Fatalf("read the GRANT's backend: %v", err)
	}

	waiting := func() bool {
		var n int
		if err := h.app.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY (pg_blocking_pids(pid))`,
			grantPID).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		return n > 0
	}
	waitLonger(t, "the run waits for the GRANT once it has copied the table", waiting)
	start := time.Now()
	for waiting() {
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("VACUUM FULL has held the credentials table for %s waiting for a GRANT to end", took)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case sweep := <-swept:
		t.Fatalf("the sweep ended (%v) when its vacuum let go of the credentials table; "+
			"want it to try again", sweep.err)
	default:
	}

	// Each try probes the cutoff first. A second run on the credentials table
	// also ends the wait, so that a sweep that runs one fails below rather
	// than here.
	tries := probes.runs(vacuumProbeTable)
	waitLonger(t, "the sweep has tried four more times", func() bool {
		return probes.runs(vacuumProbeTable) >= tries+4 || len(runs.runs()) > 1
	})
	if seen := runs.runs(); len(seen) != 1 || seen[0].written != issued {
		t.Errorf("while the GRANT was open the sweep ran VACUUM FULL on the credentials table %d times (%v); "+
			"want just the one the GRANT began during, which copied all %d rows: every run held up every "+
			"request on the table, only to give it up", len(seen), seen, issued)
	}
	if after := h.relationFiles("verification_credentials")[0]; after != before {
		t.Errorf("the credentials table moved to file %d while a GRANT VACUUM FULL waits for was open", after)
	}
	if err := granter.Rollback(ctx); err != nil {
		t.Fatalf("end the GRANT: %v", err)
	}

	sweep := await(t, "the sweep", swept)
	if sweep.err != nil {
		t.Fatalf("a sweep whose GRANT ended while it waited: %v", sweep.err)
	}
	runs.close()
	if seen := runs.runs(); len(seen) != 2 {
		t.Errorf("the sweep ran VACUUM FULL on the credentials table %d times (%v), want twice: the run the "+
			"GRANT began during, and one after it ended", len(seen), seen)
	}
	if after := h.relationFiles("verification_credentials")[0]; after == before {
		t.Errorf("the credentials table is still in file %d after a sweep that finished", after)
	}
	h.assertNothingButLiveRows("verification_credentials")
}

// holdOpen begins a transaction on the server's pool, runs statement in it,
// and leaves it open, holding what the statement took, until the test ends it
// or ends.
func (h *harness) holdOpen(statement string) pgx.Tx {
	h.t.Helper()
	ctx := context.Background()
	tx, err := h.app.pool.Begin(ctx)
	if err != nil {
		h.t.Fatalf("begin: %v", err)
	}
	h.t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, statement); err != nil {
		h.t.Fatalf("hold: %v", err)
	}
	return tx
}

// A VACUUM ends, unless it is told to skip it, with a step across the whole
// database: in a transaction of its own, after its run on the table has
// committed, it updates the database's row in pg_database, and since
// PostgreSQL 16.5 it first waits for any transaction that has updated that
// row and not ended — an ALTER DATABASE, or a GRANT on the database — which
// takes no lock on any table. The sweep's vacuumLockTimeout ends that wait
// with 55P03 as an error, as it ends a wait for the table's TOAST table, but
// with the table already in its new file. The sweep took such a run for one
// that had given the table up: it ran VACUUM FULL on the table again, holding
// every request on it up a second time, and while the transaction outlasted
// its patience it failed, blaming a lock on a TOAST table or a row in
// pg_class. So the sweep's runs skip that step (SKIP_DATABASE_STATS), which
// autovacuum takes on its own, and the sweep finishes while the transaction is
// open, with one run on each table. In the second case pg_statistic's run
// takes the step, standing in for any wait that ends a run after it has
// committed: the sweep tells such a run by its table's new file, and counts it
// as the run it was.
func TestVacuumDoesNotWaitForTheDatabasesRow(t *testing.T) {
	for _, statistics := range []struct{ name, command string }{
		{"skipped", vacuumCommand},
		{"taken after the run", "VACUUM (FULL, SKIP_LOCKED)"},
	} {
		t.Run(statistics.name, func(t *testing.T) {
			h := testServer(t, nil)
			h.app.store.statisticsVacuumCommand = statistics.command
			h.app.store.vacuumPatience = 5 * time.Second
			ctx := context.Background()
			h.exec(`INSERT INTO verification_credentials (email_hash, credential_id_hash, label, expires_at)
				VALUES ('\x01', '\x02', 'Altered College', date_trunc('day', now()) + interval '181 days')`)

			// A transaction that began before the rewrite keeps the vacuum off
			// the tables altogether, so the one on the database's row has to
			// come after it: a reader holds the credentials table and
			// pg_statistic, which the vacuum passes over, until the vacuum has
			// moved the pending table, which it does after the rewrite has
			// committed.
			reader := h.holdOpen(`SELECT (SELECT count(*) FROM verification_credentials),
				(SELECT count(*) FROM pg_stats)`)
			vacuumed := []string{"verification_credentials", "verification_pending", "pg_statistic"}
			watch := h.watchFiles(append([]string{vacuumProbeTable}, vacuumed...)...)
			swept := h.sweepLater()
			waitUntil(t, "the sweep has vacuumed the pending table", func() bool {
				return watch.runs("verification_pending") > 0
			})
			// It sets the connection limit the database already has, and rolls
			// back.
			altered := h.holdOpen(`DO $$ BEGIN
				EXECUTE (SELECT format('ALTER DATABASE %I CONNECTION LIMIT %s', datname, datconnlimit)
				         FROM pg_database WHERE datname = current_database());
			END $$`)

			// Positive control: the step across the database waits for that
			// transaction, and gives up after the sweep's lock timeout.
			config := h.app.pool.Config().ConnConfig
			if config.RuntimeParams == nil {
				config.RuntimeParams = map[string]string{}
			}
			config.RuntimeParams["lock_timeout"] = vacuumLockTimeout
			conn, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(func() { conn.Close(ctx) })
			_, err = conn.Exec(ctx, `VACUUM (ONLY_DATABASE_STATS)`)
			if pgErr := (*pgconn.PgError)(nil); !errors.As(err, &pgErr) || pgErr.Code != sqlstateLockNotAvailable {
				t.Fatalf("VACUUM (ONLY_DATABASE_STATS) returned %v while a transaction held the database's row, "+
					"want it to wait out its lock timeout: the test cannot see what it is for", err)
			}

			if err := reader.Rollback(ctx); err != nil {
				t.Fatalf("end the reader: %v", err)
			}
			sweep := await(t, "the sweep", swept)
			watch.close()
			if sweep.err != nil {
				t.Errorf("a sweep that ran while a transaction held the database's row returned %v, "+
					"want it to finish", sweep.err)
			}
			for _, table := range vacuumed {
				if n := watch.runs(table); n != 1 {
					t.Errorf("the sweep ran VACUUM FULL on %s %d times, want once: every run holds every "+
						"request on the table up", table, n)
				}
			}
			// The runs on the probe and the verification tables leave the step
			// out altogether: one that took it would count all the same, but
			// only after waiting out the lock timeout every time.
			if _, err := conn.Exec(ctx, h.app.store.vacuumCommand+" "+vacuumProbeTable); err != nil {
				t.Errorf("the sweep's VACUUM returned %v while a transaction held the database's row, "+
					"want it to leave the step across the database out", err)
			}
			if err := altered.Rollback(ctx); err != nil {
				t.Fatalf("end the ALTER DATABASE: %v", err)
			}
			h.assertNothingButLiveRows("verification_credentials")
		})
	}
}

// PostgreSQL's statistics keep the time of the last scan of every table and
// index (last_seq_scan, last_idx_scan), and a confirmation checking the
// credentials' primary key is such a scan: on a day with a single issuance it
// would date that credential to the second, though its row says only the day.
// After the sweep, every one of those times on the verification tables is the
// sweep's.
func TestSweepLeavesNoScanTimeFromBeforeIt(t *testing.T) {
	h := verifyServer(t, nil)
	h.verify("scanned@iitb.ac.in")
	ctx := context.Background()

	type stamp struct {
		what string
		at   *time.Time
	}
	stamps := func() []stamp {
		rows, err := h.app.pool.Query(ctx, `
			SELECT relname || ' read whole', last_seq_scan FROM pg_stat_user_tables
			WHERE schemaname = current_schema() AND relname = ANY ($1)
			UNION ALL
			SELECT relname || ' read by index', last_idx_scan FROM pg_stat_user_tables
			WHERE schemaname = current_schema() AND relname = ANY ($1)
			UNION ALL
			SELECT indexrelname, last_idx_scan FROM pg_stat_user_indexes
			WHERE schemaname = current_schema() AND relname = ANY ($1)
			ORDER BY 1`, verificationTables[:])
		if err != nil {
			t.Fatalf("read the scan times: %v", err)
		}
		all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (stamp, error) {
			var s stamp
			err := row.Scan(&s.what, &s.at)
			return s, err
		})
		if err != nil {
			t.Fatalf("read the scan times: %v", err)
		}
		// Both tables, each read whole and by index, and their six indexes.
		if len(all) != 10 {
			t.Fatalf("found %d scan times, want 10: %v", len(all), all)
		}
		return all
	}

	// Every table and index scanned once before the sweep, as restampScans
	// scans them, on a connection that then closes, which hands its counts
	// to the statistics at once: a request's connection goes back to the pool
	// and hands them over when it is next idle long enough, which can be ten
	// seconds away.
	conn, err := pgx.ConnectConfig(ctx, h.app.pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := restampScans(ctx, conn); err != nil {
		t.Fatalf("scan the verification tables: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitUntil(t, "the statistics have a time for every scan", func() bool {
		for _, s := range stamps() {
			if s.at == nil {
				return false
			}
		}
		return true
	})
	var sweepStart time.Time
	if err := h.app.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&sweepStart); err != nil {
		t.Fatalf("read the clock: %v", err)
	}

	if _, err := h.app.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// The sweeper's connection closes as the sweep returns, and the
	// statistics take its counts a moment later.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var stale []string
		for _, s := range stamps() {
			if s.at == nil || s.at.Before(sweepStart) {
				stale = append(stale, fmt.Sprintf("%s at %v", s.what, s.at))
			}
		}
		if len(stale) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the sweep, scan times from before it (%s) remain: %s", sweepStart, stale)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
