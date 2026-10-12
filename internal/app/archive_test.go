package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// addBoard inserts a board straight into the database, for tests that need
// settings of their own and should not depend on which boards the migrations
// happen to seed. Settings are then changed with exec, like any other setup
// the API does not expose to a poster.
func (h *harness) addBoard(slug string) {
	h.t.Helper()
	h.exec(`INSERT INTO boards (slug, name) VALUES ($1, $1)`, slug)
}

// archivedAt reads a thread's archived_at straight from the database, nil
// while the thread is live.
func (h *harness) archivedAt(board string, no int64) *time.Time {
	h.t.Helper()
	var at *time.Time
	if err := h.app.pool.QueryRow(context.Background(), `
		SELECT t.archived_at FROM threads t JOIN boards b ON b.id = t.board_id
		WHERE b.slug = $1 AND t.no = $2`, board, no).Scan(&at); err != nil {
		h.t.Fatalf("archived_at of /%s/%d: %v", board, no, err)
	}
	return at
}

// filesUnder lists the regular files below root, relative to it. The local
// media backend keeps nothing else there, so an empty list means no blob was
// left behind.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return files
}

func fetchArchive(t *testing.T, h *harness, board string) Archive {
	t.Helper()
	var archive Archive
	h.get("/api/boards/"+board+"/archive", &archive)
	return archive
}

// liveThreadNos lists the threads a board's index shows, in index order.
func liveThreadNos(t *testing.T, h *harness, board string) []int64 {
	t.Helper()
	var nos []int64
	for _, thread := range fetchIndex(t, h, board, 1).Threads {
		nos = append(nos, thread.No)
	}
	return nos
}

func catalogThreadNos(t *testing.T, h *harness, board string) []int64 {
	t.Helper()
	var catalog Catalog
	h.get("/api/boards/"+board+"/catalog", &catalog)
	var nos []int64
	for _, thread := range catalog.Threads {
		nos = append(nos, thread.No)
	}
	return nos
}

/* Archiving ---------------------------------------------------------------- */

// A thread pushed past the thread limit used to be deleted. It is archived
// instead: off the index and the catalog, still readable, closed to replies,
// and listed in the board's archive.
func TestThreadsPastTheLimitAreArchivedNotDeleted(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("arch")
	h.exec(`UPDATE boards SET thread_limit = 2 WHERE slug = 'arch'`)

	oldest := h.createThread("arch", "the oldest", "", "first on the board")
	h.reply("arch", oldest.ThreadNo, "", "a reply that outlives the archiving", false)
	middle := h.createThread("arch", "the middle", "", "second on the board")
	newest := h.createThread("arch", "the newest", "", "third, which pushes the oldest off")

	want := []int64{newest.ThreadNo, middle.ThreadNo}
	if got := liveThreadNos(t, h, "arch"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("index lists %v, want %v", got, want)
	}
	if got := catalogThreadNos(t, h, "arch"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("catalog lists %v, want %v", got, want)
	}

	// Nothing was deleted: the thread loads with both of its posts, and says
	// when it was archived.
	detail := fetchThread(t, h, "arch", oldest.ThreadNo)
	if len(detail.Posts) != 2 || detail.Replies != 1 {
		t.Errorf("archived thread has %d posts and %d replies, want 2 and 1", len(detail.Posts), detail.Replies)
	}
	if detail.ArchivedAt == nil {
		t.Fatal("archived thread should carry archived_at")
	}
	if age := time.Since(*detail.ArchivedAt); age < 0 || age > time.Minute {
		t.Errorf("archived_at = %v, want about now", *detail.ArchivedAt)
	}
	if live := fetchThread(t, h, "arch", newest.ThreadNo); live.ArchivedAt != nil {
		t.Errorf("live thread has archived_at %v, want null", *live.ArchivedAt)
	}

	// Replies are refused, text and image alike, and the refusal changes
	// nothing.
	path := fmt.Sprintf("/api/boards/arch/threads/%d/posts", oldest.ThreadNo)
	var out map[string]any
	if status := h.post(path, map[string]any{"body": "too late"}, &out); status != http.StatusForbidden {
		t.Errorf("reply to an archived thread: status %d, want 403", status)
	}
	if out["error"] != "thread is archived" {
		t.Errorf("error = %v, want %q", out["error"], "thread is archived")
	}
	if status := h.upload(path, map[string]string{"body": "still too late"},
		"late.png", pngBytes(t, 40, 40), &out); status != http.StatusForbidden {
		t.Errorf("image reply to an archived thread: status %d, want 403", status)
	}
	if after := fetchThread(t, h, "arch", oldest.ThreadNo); len(after.Posts) != 2 || after.Replies != 1 {
		t.Errorf("refused replies changed the thread: %d posts, %d replies", len(after.Posts), after.Replies)
	}
	// The refused image was written before the thread was checked; it must
	// not be left behind.
	if files := filesUnder(t, h.app.cfg.UploadDir); len(files) != 0 {
		t.Errorf("a refused upload left %v behind", files)
	}

	archive := fetchArchive(t, h, "arch")
	if archive.Board.Slug != "arch" {
		t.Errorf("archive board = %q, want arch", archive.Board.Slug)
	}
	if len(archive.Threads) != 1 {
		t.Fatalf("archive lists %d threads, want 1: %+v", len(archive.Threads), archive.Threads)
	}
	entry := archive.Threads[0]
	if entry.No != oldest.ThreadNo || entry.Subject != "the oldest" || entry.Excerpt != "first on the board" ||
		entry.Replies != 1 {
		t.Errorf("archive entry = %+v", entry)
	}
	if !entry.ArchivedAt.Equal(*detail.ArchivedAt) || !entry.CreatedAt.Equal(detail.CreatedAt) {
		t.Errorf("archive entry times = %v/%v, want the thread's %v/%v",
			entry.CreatedAt, entry.ArchivedAt, detail.CreatedAt, *detail.ArchivedAt)
	}
}

// Every view of a thread carries archived_at, as null while the thread is live
// rather than missing, so a client can test it without guessing.
func TestArchivedAtIsAlwaysPresent(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("present")
	thread := h.createThread("present", "live", "", "still here")

	for _, view := range []struct{ path, list string }{
		{"/api/boards/present/threads", "threads"},
		{"/api/boards/present/catalog", "threads"},
		{fmt.Sprintf("/api/boards/present/threads/%d", thread.ThreadNo), ""},
	} {
		var raw map[string]json.RawMessage
		h.get(view.path, &raw)
		object := raw
		if view.list != "" {
			var list []map[string]json.RawMessage
			if err := json.Unmarshal(raw[view.list], &list); err != nil || len(list) != 1 {
				t.Fatalf("%s: %s = %s (%v)", view.path, view.list, raw[view.list], err)
			}
			object = list[0]
		}
		value, ok := object["archived_at"]
		if !ok {
			t.Errorf("%s: archived_at is missing", view.path)
		} else if string(value) != "null" {
			t.Errorf("%s: archived_at = %s, want null", view.path, value)
		}
	}
}

// Archived threads stop counting against the limit the moment they are
// archived. If they still counted, an archived thread that sorted among the
// live ones would hold a slot and push a live thread off early, and every
// later post would stamp the archived ones again.
func TestArchivedThreadsDoNotCountAgainstTheLimit(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("limit")
	h.exec(`UPDATE boards SET thread_limit = 2 WHERE slug = 'limit'`)

	pinned := h.createThread("limit", "pinned", "", "sticky threads are never archived")
	h.exec(`UPDATE threads SET sticky = TRUE, bumped_at = now() - interval '1 day' WHERE no = $1
		AND board_id = (SELECT id FROM boards WHERE slug = 'limit')`, pinned.ThreadNo)

	first := h.createThread("limit", "", "", "first")
	second := h.createThread("limit", "", "", "second")
	third := h.createThread("limit", "", "", "third, archives the first")
	stamped := h.archivedAt("limit", first.ThreadNo)
	if stamped == nil {
		t.Fatal("the first thread should have been archived")
	}

	// Give the archived thread the newest bump on the board. Counted among the
	// live threads, it would now take a slot from one of them.
	h.exec(`UPDATE threads SET bumped_at = now() + interval '1 hour' WHERE no = $1
		AND board_id = (SELECT id FROM boards WHERE slug = 'limit')`, first.ThreadNo)

	fourth := h.createThread("limit", "", "", "fourth, archives the second and nothing else")

	want := []int64{pinned.ThreadNo, fourth.ThreadNo, third.ThreadNo}
	if got := liveThreadNos(t, h, "limit"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("index lists %v, want the sticky thread plus the two newest, %v", got, want)
	}
	if h.archivedAt("limit", second.ThreadNo) == nil {
		t.Error("the second thread should have been archived")
	}
	if again := h.archivedAt("limit", first.ThreadNo); again == nil || !again.Equal(*stamped) {
		t.Errorf("first thread archived_at moved from %v to %v: it was archived again", *stamped, again)
	}
	if h.archivedAt("limit", pinned.ThreadNo) != nil {
		t.Error("a sticky thread was archived")
	}
}

// The index counts pages over live threads only, or a board with a full
// archive would advertise pages of nothing.
func TestIndexPagesCountLiveThreadsOnly(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("pages")
	h.exec(`UPDATE boards SET thread_limit = $1 WHERE slug = 'pages'`, indexPageSize)

	for i := 0; i < indexPageSize+3; i++ {
		h.createThread("pages", "", "", fmt.Sprintf("thread %d", i))
	}

	index := fetchIndex(t, h, "pages", 1)
	if index.Pages != 1 || len(index.Threads) != indexPageSize {
		t.Errorf("index has %d pages and %d threads, want 1 and %d", index.Pages, len(index.Threads), indexPageSize)
	}
	if archived := len(fetchArchive(t, h, "pages").Threads); archived != 3 {
		t.Errorf("archive lists %d threads, want 3", archived)
	}
}

/* The archive listing ------------------------------------------------------ */

func TestArchiveListsNewestArchivedFirst(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("order")
	h.exec(`UPDATE boards SET thread_limit = 1 WHERE slug = 'order'`)

	long := strings.Repeat("long-winded ", 40)
	a := h.createThread("order", "a", "", long)
	b := h.createThread("order", "b", "", "archived second")
	c := h.createThread("order", "c", "", "archived third")
	live := h.createThread("order", "live", "", "still on the board")

	archive := fetchArchive(t, h, "order")
	var got []int64
	for _, thread := range archive.Threads {
		got = append(got, thread.No)
	}
	if want := []int64{c.ThreadNo, b.ThreadNo, a.ThreadNo}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("archive order = %v, want %v (newest archived first, live No. %d absent)",
			got, want, live.ThreadNo)
	}
	for i := 1; i < len(archive.Threads); i++ {
		if archive.Threads[i].ArchivedAt.After(archive.Threads[i-1].ArchivedAt) {
			t.Errorf("entry %d was archived after entry %d", i, i-1)
		}
	}

	excerpt := archive.Threads[2].Excerpt
	if !strings.HasSuffix(excerpt, "…") || len([]rune(excerpt)) > archiveExcerptLen+1 {
		t.Errorf("long opening post excerpt = %q, want at most %d runes ending in an ellipsis",
			excerpt, archiveExcerptLen+1)
	}

	// An empty archive is an empty list, not null, and an unknown board is a
	// 404 like everywhere else.
	h.addBoard("empty")
	var raw map[string]json.RawMessage
	h.get("/api/boards/empty/archive", &raw)
	if string(raw["threads"]) != "[]" {
		t.Errorf("empty archive threads = %s, want []", raw["threads"])
	}
	var out map[string]any
	if status := h.do(http.MethodGet, "/api/boards/nope/archive", nil, &out); status != http.StatusNotFound {
		t.Errorf("unknown board archive: status %d, want 404", status)
	}
}

// The listing is bounded; anything older still loads by number until the
// sweeper deletes it.
func TestArchiveListingIsBounded(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("deep")

	// Built in SQL: posting archiveLimit threads through the API would take a
	// while and prove nothing extra. Thread n is archived n minutes ago, so
	// No. 1 is the newest entry and the last one is past the bound.
	h.exec(`
		WITH board AS (SELECT id FROM boards WHERE slug = 'deep'),
		     made AS (
		         INSERT INTO threads (board_id, no, subject, archived_at)
		         SELECT board.id, n, 'thread ' || n, now() - n * interval '1 minute'
		         FROM board, generate_series(1, $1) AS n
		         RETURNING id, board_id, no
		     )
		INSERT INTO posts (board_id, thread_id, no, body, is_op)
		SELECT board_id, id, no, 'archived body', TRUE FROM made`, archiveLimit+1)

	archive := fetchArchive(t, h, "deep")
	if len(archive.Threads) != archiveLimit {
		t.Fatalf("archive lists %d threads, want the bound of %d", len(archive.Threads), archiveLimit)
	}
	if first, last := archive.Threads[0].No, archive.Threads[archiveLimit-1].No; first != 1 || last != archiveLimit {
		t.Errorf("archive runs from No. %d to No. %d, want 1 to %d", first, last, archiveLimit)
	}
	if detail := fetchThread(t, h, "deep", archiveLimit+1); detail.ArchivedAt == nil {
		t.Error("a thread past the listing's bound should still load as archived")
	}
}
