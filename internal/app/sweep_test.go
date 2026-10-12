package app

import (
	"context"
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
	if want := (SweepReport{Threads: 3, Posts: 9, Attachments: 6}); report != want {
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

	// Nothing is left to do, and doing nothing is not a failure.
	again, err := h.app.Sweep(context.Background())
	if err != nil || again != (SweepReport{}) {
		t.Errorf("second sweep = %+v, %v; want nothing swept and no error", again, err)
	}
}

// Rows and blobs are deleted in that order, so a blob that cannot be removed
// is an orphaned file rather than a broken post. The sweep carries on with the
// rest and then reports failure, which is what gives the job a non-zero exit.
func TestSweepReportsBlobsItCouldNotRemove(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("stuck")
	h.exec(`UPDATE boards SET archive_days = 0 WHERE slug = 'stuck'`)

	thread := h.imageThread("stuck", "an image the store will not let go of")
	h.archiveThread("stuck", thread.ThreadNo, time.Minute)
	blobs := threadBlobs(t, h, "stuck", thread.ThreadNo)
	h.app.media = refusingDeletes{h.app.media}

	report, err := h.app.Sweep(context.Background())
	if err == nil {
		t.Fatal("a sweep that could not remove an attachment reported success")
	}
	if report.Threads != 1 || report.Attachments != 1 || report.BlobFailures != 1 {
		t.Errorf("sweep report = %+v, want one thread and one attachment, its blobs not removed", report)
	}
	if status := h.threadStatus("stuck", thread.ThreadNo); status != http.StatusNotFound {
		t.Errorf("the thread's rows should be gone even so, GET answers %d", status)
	}
	for _, path := range blobs {
		requireFile(t, path, true)
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

// The sweep's log is a count of what went, never which boards or threads.
func TestSweepLogsOnlyCounts(t *testing.T) {
	var logs syncBuffer
	h := testServerLogging(t, nil, &logs)
	h.addBoard("quietly")
	h.exec(`UPDATE boards SET archive_days = 0 WHERE slug = 'quietly'`)
	thread := h.imageThread("quietly", "gone without a trace in the log")
	h.archiveThread("quietly", thread.ThreadNo, time.Minute)

	if _, err := h.app.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var record map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err == nil && entry["msg"] == "sweep" {
			record = entry
		}
	}
	if record == nil {
		t.Fatalf("no sweep record in the log:\n%s", logs.String())
	}
	delete(record, "time")
	delete(record, "level")
	delete(record, "msg")
	want := map[string]any{"threads": 1.0, "posts": 1.0, "attachments": 1.0, "blob_failures": 0.0}
	if fmt.Sprint(record) != fmt.Sprint(want) {
		t.Errorf("sweep record = %v, want exactly %v", record, want)
	}
}

// refusingDeletes is a media store whose deletes always fail, standing in for
// a bucket that has stopped cooperating.
type refusingDeletes struct{ media.Store }

func (refusingDeletes) Delete(context.Context, string) error {
	return errors.New("delete refused")
}
