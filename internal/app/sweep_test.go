package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

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
// public for good with nothing left to say they exist. The failure holds up
// nothing else: the spent captcha answers past their day go all the same.
func TestSweepRetriesFilesItCouldNotRemove(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("stuck")
	h.exec(`UPDATE boards SET archive_days = 0 WHERE slug = 'stuck'`)

	thread := h.imageThread("stuck", "an image the store will not let go of")
	h.archiveThread("stuck", thread.ThreadNo, time.Minute)
	blobs := threadBlobs(t, h, "stuck", thread.ThreadNo)
	spentKey := sha256.Sum256([]byte("an answer spent two days ago"))
	h.exec(`INSERT INTO captcha_spent (signature_hash, expires_at) VALUES ($1, $2)`,
		spentKey[:], time.Now().UTC().Add(-48*time.Hour).Truncate(24*time.Hour))
	store := h.app.media
	h.app.media = refusingDeletes{store}

	report, err := h.app.Sweep(context.Background())
	if err == nil {
		t.Fatal("a sweep that could not remove an attachment reported success")
	}
	if want := (SweepReport{Threads: 1, Posts: 1, Attachments: 1, BlobFailures: 2, SpentCaptchas: 1}); report != want {
		t.Errorf("sweep report = %+v, want %+v", report, want)
	}
	if n := h.countRows("captcha_spent"); n != 0 {
		t.Errorf("%d spent captcha answers kept past their day by a sweep whose files failed, want none", n)
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

// The spent captcha answers go last and on their own: a sweep whose captcha
// cleanup fails still removes the files of the threads it deleted, and says
// what failed.
func TestSweepRemovesFilesWhenTheCaptchaCleanupFails(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("gone")
	h.exec(`UPDATE boards SET archive_days = 0 WHERE slug = 'gone'`)

	thread := h.imageThread("gone", "an image to remove")
	h.archiveThread("gone", thread.ThreadNo, time.Minute)
	blobs := threadBlobs(t, h, "gone", thread.ThreadNo)
	h.exec(`ALTER TABLE captcha_spent RENAME TO captcha_spent_elsewhere`)

	report, err := h.app.Sweep(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sweep spent captchas") {
		t.Fatalf("a sweep whose captcha cleanup failed: error %v, want one saying so", err)
	}
	if want := (SweepReport{Threads: 1, Posts: 1, Attachments: 1, BlobsRemoved: 2}); report != want {
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
	// And a spent captcha answer two days past its day, which the sweep
	// prunes and counts in the same record.
	spentKey := sha256.Sum256([]byte("an answer long spent"))
	h.exec(`INSERT INTO captcha_spent (signature_hash, expires_at) VALUES ($1, $2)`,
		spentKey[:], time.Now().UTC().Add(-48*time.Hour).Truncate(24*time.Hour))
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
		"blobs_removed": 2.0, "blob_failures": 0.0, "blobs_missing": 0.0, "spent_captchas": 1.0}
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
