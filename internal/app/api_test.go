package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaCounter keeps the per-test schema names unique within a run.
var schemaCounter atomic.Int64

// harness is a running application: an HTTP client bound to a test server plus
// direct database access for setup that the API deliberately does not expose,
// such as moderation flags and board limits.
type harness struct {
	*client
	app *App
}

// exec runs a statement against the test schema.
func (h *harness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.app.pool.Exec(context.Background(), query, args...); err != nil {
		h.t.Fatalf("exec %q: %v", query, err)
	}
}

// testServer starts the application against an isolated schema in the
// development database. Tests are skipped when no PostgreSQL instance is
// reachable.
func testServer(t *testing.T, tune func(*Config)) *harness {
	t.Helper()

	baseURL := firstEnv("TEST_DATABASE_URL", "DATABASE_URL")
	if baseURL == "" {
		baseURL = "postgres://educhan:educhan@localhost:5432/educhan?sslmode=disable"
	}

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := admin.Ping(pingCtx); err != nil {
		admin.Close()
		t.Skipf("postgres unavailable at %s: %v", baseURL, err)
	}

	schema := fmt.Sprintf("educhan_test_%d_%d", os.Getpid(), schemaCounter.Add(1))
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		admin.Close()
	})

	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	cfg := Config{
		DatabaseURL:  baseURL + separator + "search_path=" + schema,
		PosterIDSalt: "test-salt",
		LogFormat:    "text",
	}
	if tune != nil {
		tune(&cfg)
	}

	app, err := New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("start app: %v", err)
	}
	t.Cleanup(app.Close)

	srv := httptest.NewServer(app.Routes())
	t.Cleanup(srv.Close)
	return &harness{client: &client{t: t, base: srv.URL}, app: app}
}

type client struct {
	t    *testing.T
	base string
}

// get performs a GET and decodes a successful JSON response into out.
func (c *client) get(path string, out any) {
	c.t.Helper()
	status := c.do(http.MethodGet, path, nil, out)
	if status != http.StatusOK {
		c.t.Fatalf("GET %s: status %d", path, status)
	}
}

// post sends a JSON body and decodes the response, returning the status.
func (c *client) post(path string, body any, out any) int {
	c.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		c.t.Fatalf("marshal body: %v", err)
	}
	return c.do(http.MethodPost, path, encoded, out)
}

func (c *client) do(method, path string, body []byte, out any) int {
	c.t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	payload, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	if out != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, out); err != nil {
			c.t.Fatalf("%s %s: decode %q: %v", method, path, payload, err)
		}
	}
	return res.StatusCode
}

// createdPost is the API acknowledgement for a new thread or reply.
type createdPost struct {
	Board    string `json:"board"`
	No       int64  `json:"no"`
	ThreadNo int64  `json:"thread_no"`
	Error    string `json:"error"`
}

func (c *client) createThread(board, subject, name, body string) createdPost {
	c.t.Helper()
	var out createdPost
	status := c.post("/api/boards/"+board+"/threads",
		map[string]any{"subject": subject, "name": name, "body": body}, &out)
	if status != http.StatusCreated {
		c.t.Fatalf("create thread: status %d (%s)", status, out.Error)
	}
	return out
}

func (c *client) reply(board string, threadNo int64, name, body string, sage bool) createdPost {
	c.t.Helper()
	var out createdPost
	path := fmt.Sprintf("/api/boards/%s/threads/%d/posts", board, threadNo)
	status := c.post(path, map[string]any{"name": name, "body": body, "sage": sage}, &out)
	if status != http.StatusCreated {
		c.t.Fatalf("reply: status %d (%s)", status, out.Error)
	}
	return out
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return ""
}

/* Tests -------------------------------------------------------------------- */

func TestBoardsAreSeeded(t *testing.T) {
	c := testServer(t, nil)

	var out struct {
		Boards []Board `json:"boards"`
	}
	c.get("/api/boards", &out)
	if len(out.Boards) == 0 {
		t.Fatal("expected the migration to seed boards")
	}

	slugs := map[string]Board{}
	for _, b := range out.Boards {
		slugs[b.Slug] = b
	}
	for _, want := range []string{"placements", "research", "general"} {
		board, ok := slugs[want]
		if !ok {
			t.Fatalf("board %q missing from %v", want, slugs)
		}
		if board.BumpLimit <= 0 || board.ThreadLimit <= 0 {
			t.Errorf("board %q has unusable limits: %+v", want, board)
		}
	}
}

func TestHealthz(t *testing.T) {
	c := testServer(t, nil)

	var out map[string]string
	c.get("/healthz", &out)
	if out["status"] != "ok" {
		t.Errorf("status = %q, want ok", out["status"])
	}
}

func TestPostNumbersArePerBoard(t *testing.T) {
	c := testServer(t, nil)

	general := c.createThread("general", "first", "", "hello /general/")
	if general.No != 1 || general.ThreadNo != 1 {
		t.Errorf("first post on a board should be No. 1, got %+v", general)
	}
	reply := c.reply("general", general.ThreadNo, "", "second post", false)
	if reply.No != 2 {
		t.Errorf("second post = No. %d, want 2", reply.No)
	}

	research := c.createThread("research", "", "", "hello /research/")
	if research.No != 1 {
		t.Errorf("numbering must restart per board, got No. %d", research.No)
	}
}

func TestAnonymousIdentity(t *testing.T) {
	c := testServer(t, nil)

	thread := c.createThread("general", "identity", "", "no name given")
	c.reply("general", thread.ThreadNo, "ana#secret", "with a tripcode", false)

	detail := fetchThread(t, c, "general", thread.ThreadNo)
	if len(detail.Posts) != 2 {
		t.Fatalf("thread has %d posts, want 2", len(detail.Posts))
	}

	op, reply := detail.Posts[0], detail.Posts[1]
	if op.Name != anonymousName {
		t.Errorf("op name = %q, want %q", op.Name, anonymousName)
	}
	if op.Tripcode != "" {
		t.Errorf("op should have no tripcode, got %q", op.Tripcode)
	}
	if reply.Name != "ana" || !strings.HasPrefix(reply.Tripcode, "!") {
		t.Errorf("reply identity = %q/%q, want ana with a tripcode", reply.Name, reply.Tripcode)
	}
	if op.PosterID == "" || op.PosterID != reply.PosterID {
		t.Errorf("both posts come from the same client, so poster IDs should match: %q vs %q",
			op.PosterID, reply.PosterID)
	}

	other := c.createThread("general", "another", "", "different thread")
	otherDetail := fetchThread(t, c, "general", other.ThreadNo)
	if otherDetail.Posts[0].PosterID == op.PosterID {
		t.Error("poster IDs must not be linkable across threads")
	}
}

func TestQuotesAndBacklinks(t *testing.T) {
	c := testServer(t, nil)

	thread := c.createThread("general", "quoting", "", "the original post")
	first := c.reply("general", thread.ThreadNo, "", fmt.Sprintf(">>%d\nagreed", thread.No), false)
	c.reply("general", thread.ThreadNo, "", fmt.Sprintf(">>%d and >>%d and >>9999", thread.No, first.No), false)

	detail := fetchThread(t, c, "general", thread.ThreadNo)
	op, second, third := detail.Posts[0], detail.Posts[1], detail.Posts[2]

	if len(second.Quotes) != 1 || second.Quotes[0].No != op.No {
		t.Errorf("second post quotes = %+v, want just the OP", second.Quotes)
	}
	if second.Quotes[0].ThreadNo != thread.ThreadNo {
		t.Errorf("quote thread_no = %d, want %d", second.Quotes[0].ThreadNo, thread.ThreadNo)
	}
	if len(third.Quotes) != 2 {
		t.Errorf("third post quotes = %+v, want 2 resolvable quotes (>>9999 does not exist)", third.Quotes)
	}
	if len(op.Backlinks) != 2 {
		t.Errorf("op backlinks = %+v, want 2", op.Backlinks)
	}
	if len(second.Backlinks) != 1 || second.Backlinks[0].No != third.No {
		t.Errorf("second post backlinks = %+v, want just the third post", second.Backlinks)
	}
}

func TestIndexOrdersByBumpAndHonoursSage(t *testing.T) {
	c := testServer(t, nil)

	older := c.createThread("general", "older", "", "posted first")
	newer := c.createThread("general", "newer", "", "posted second")

	index := fetchIndex(t, c, "general", 1)
	if index.Threads[0].No != newer.ThreadNo {
		t.Fatalf("newest thread should lead the index, got No. %d", index.Threads[0].No)
	}

	c.reply("general", older.ThreadNo, "", "bumping this back up", false)
	index = fetchIndex(t, c, "general", 1)
	if index.Threads[0].No != older.ThreadNo {
		t.Fatalf("a reply should bump its thread, got No. %d", index.Threads[0].No)
	}

	c.reply("general", newer.ThreadNo, "", "saged reply", true)
	index = fetchIndex(t, c, "general", 1)
	if index.Threads[0].No != older.ThreadNo {
		t.Errorf("a saged reply must not bump, index leads with No. %d", index.Threads[0].No)
	}
	if index.Threads[1].Replies != 1 {
		t.Errorf("saged thread reply count = %d, want 1", index.Threads[1].Replies)
	}
}

func TestIndexShowsOpAndLastReplies(t *testing.T) {
	c := testServer(t, nil)

	thread := c.createThread("general", "tail", "", "opening post")
	for i := 1; i <= indexRepliesPerThread+2; i++ {
		c.reply("general", thread.ThreadNo, "", fmt.Sprintf("reply %d", i), false)
	}

	index := fetchIndex(t, c, "general", 1)
	summary := index.Threads[0]
	if summary.OP.Body != "opening post" || !summary.OP.OP {
		t.Errorf("index should carry the opening post, got %+v", summary.OP)
	}
	if len(summary.LastReplies) != indexRepliesPerThread {
		t.Fatalf("index shows %d replies, want %d", len(summary.LastReplies), indexRepliesPerThread)
	}
	if summary.OmittedReplies != 2 {
		t.Errorf("omitted replies = %d, want 2", summary.OmittedReplies)
	}
	if got := summary.LastReplies[len(summary.LastReplies)-1].Body; got != "reply 5" {
		t.Errorf("last reply = %q, want the newest one", got)
	}
}

func TestIndexPagination(t *testing.T) {
	c := testServer(t, nil)

	for i := 0; i < indexPageSize+2; i++ {
		c.createThread("general", "", "", fmt.Sprintf("thread %d", i))
	}

	first := fetchIndex(t, c, "general", 1)
	if first.Pages != 2 {
		t.Errorf("pages = %d, want 2", first.Pages)
	}
	if len(first.Threads) != indexPageSize {
		t.Errorf("page 1 has %d threads, want %d", len(first.Threads), indexPageSize)
	}

	second := fetchIndex(t, c, "general", 2)
	if len(second.Threads) != 2 {
		t.Errorf("page 2 has %d threads, want 2", len(second.Threads))
	}
	if empty := fetchIndex(t, c, "general", 3); len(empty.Threads) != 0 {
		t.Errorf("page past the end should be empty, got %d threads", len(empty.Threads))
	}

	var out map[string]string
	if status := c.do(http.MethodGet, "/api/boards/general/threads?page=0", nil, &out); status != http.StatusBadRequest {
		t.Errorf("page=0 status = %d, want 400", status)
	}
}

func TestCatalog(t *testing.T) {
	c := testServer(t, nil)

	long := strings.Repeat("verbose ", 60)
	thread := c.createThread("general", "catalog subject", "", long)
	c.reply("general", thread.ThreadNo, "", "a reply", false)

	var catalog Catalog
	c.get("/api/boards/general/catalog", &catalog)
	if len(catalog.Threads) != 1 {
		t.Fatalf("catalog has %d threads, want 1", len(catalog.Threads))
	}

	tile := catalog.Threads[0]
	if tile.Subject != "catalog subject" || tile.Replies != 1 {
		t.Errorf("unexpected tile: %+v", tile)
	}
	if !strings.HasSuffix(tile.Excerpt, "\u2026") {
		t.Errorf("long bodies should be excerpted, got %q", tile.Excerpt)
	}
	if len([]rune(tile.Excerpt)) > excerptLen+1 {
		t.Errorf("excerpt is %d runes, want at most %d", len([]rune(tile.Excerpt)), excerptLen+1)
	}
}

func TestValidation(t *testing.T) {
	c := testServer(t, nil)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{name: "empty body", body: map[string]any{"body": "   "}, want: http.StatusBadRequest},
		{name: "body too long", body: map[string]any{"body": strings.Repeat("a", maxBodyLen+1)}, want: http.StatusBadRequest},
		{name: "too many lines", body: map[string]any{"body": strings.Repeat("a\n", maxBodyLines+1)}, want: http.StatusBadRequest},
		{name: "subject too long", body: map[string]any{"body": "ok", "subject": strings.Repeat("s", maxSubjectLen+1)}, want: http.StatusBadRequest},
		{name: "unknown field", body: map[string]any{"body": "ok", "author": "legacy"}, want: http.StatusBadRequest},
		{name: "valid", body: map[string]any{"body": "ok"}, want: http.StatusCreated},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out createdPost
			if status := c.post("/api/boards/general/threads", tc.body, &out); status != tc.want {
				t.Errorf("status = %d, want %d (%s)", status, tc.want, out.Error)
			}
		})
	}
}

func TestNotFoundResponses(t *testing.T) {
	c := testServer(t, nil)

	cases := []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: "/api/boards/nope", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/api/boards/nope/threads", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/api/boards/general/threads/999", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/api/boards/general/threads/abc", want: http.StatusBadRequest},
		{method: http.MethodGet, path: "/api/threads/999", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/api/nonsense", want: http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var out map[string]any
			if status := c.do(tc.method, tc.path, nil, &out); status != tc.want {
				t.Errorf("status = %d, want %d", status, tc.want)
			}
			if _, ok := out["error"]; !ok {
				t.Errorf("expected a JSON error body, got %v", out)
			}
		})
	}
}

func TestThreadRoutesByPrimaryKey(t *testing.T) {
	c := testServer(t, nil)

	// Thread 1 on /general/ and thread 1 on /research/ share a number but not a
	// primary key, so the legacy id route must resolve the right one.
	c.createThread("general", "general thread", "", "on general")
	c.createThread("research", "research thread", "", "on research")

	var detail ThreadDetail
	c.get("/api/threads/2", &detail)
	if detail.Board.Slug != "research" {
		t.Errorf("board = %q, want research", detail.Board.Slug)
	}

	var created createdPost
	if status := c.post("/api/threads/2/posts", map[string]any{"body": "reply by id"}, &created); status != http.StatusCreated {
		t.Fatalf("reply by id: status %d (%s)", status, created.Error)
	}
	if created.Board != "research" {
		t.Errorf("reply landed on board %q, want research", created.Board)
	}
}

func TestFloodControl(t *testing.T) {
	c := testServer(t, func(cfg *Config) {
		cfg.ThreadCooldown = time.Minute
		cfg.PostCooldown = time.Minute
	})

	thread := c.createThread("general", "first", "", "allowed")

	var out map[string]any
	status := c.post("/api/boards/general/threads", map[string]any{"body": "too soon"}, &out)
	if status != http.StatusTooManyRequests {
		t.Fatalf("second thread status = %d, want 429", status)
	}
	if _, ok := out["retry_after"]; !ok {
		t.Errorf("429 body should include retry_after, got %v", out)
	}

	// Replies have their own cooldown, so the first one still goes through.
	c.reply("general", thread.ThreadNo, "", "first reply", false)
	status = c.post(fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo),
		map[string]any{"body": "too soon"}, &out)
	if status != http.StatusTooManyRequests {
		t.Errorf("second reply status = %d, want 429", status)
	}
}

func TestThreadPruning(t *testing.T) {
	c := testServer(t, nil)

	const limit = 3
	c.exec(`UPDATE boards SET thread_limit = $1 WHERE slug = 'general'`, limit)

	var kept []int64
	for i := 0; i < limit+2; i++ {
		created := c.createThread("general", "", "", fmt.Sprintf("thread %d", i))
		kept = append(kept, created.ThreadNo)
	}

	index := fetchIndex(t, c, "general", 1)
	if len(index.Threads) != limit {
		t.Fatalf("board holds %d threads, want the limit of %d", len(index.Threads), limit)
	}

	survivors := map[int64]bool{}
	for _, thread := range index.Threads {
		survivors[thread.No] = true
	}
	for _, no := range kept[len(kept)-limit:] {
		if !survivors[no] {
			t.Errorf("thread No. %d should have survived pruning", no)
		}
	}
	for _, no := range kept[:len(kept)-limit] {
		if survivors[no] {
			t.Errorf("thread No. %d should have been pruned", no)
		}
	}
}

func TestLockedThreadRejectsReplies(t *testing.T) {
	c := testServer(t, nil)

	thread := c.createThread("general", "locked", "", "no more replies please")
	c.exec(`UPDATE threads SET locked = TRUE WHERE no = $1`, thread.ThreadNo)

	var out map[string]any
	path := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)
	if status := c.post(path, map[string]any{"body": "let me in"}, &out); status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}

	detail := fetchThread(t, c, "general", thread.ThreadNo)
	if !detail.Locked {
		t.Error("thread should report itself as locked")
	}
	if detail.Replies != 0 {
		t.Errorf("replies = %d, want 0", detail.Replies)
	}
}

func TestStickyThreadLeadsIndex(t *testing.T) {
	c := testServer(t, nil)

	pinned := c.createThread("general", "read me first", "", "board rules")
	c.exec(`UPDATE threads SET sticky = TRUE WHERE no = $1`, pinned.ThreadNo)
	newer := c.createThread("general", "newer", "", "posted later")
	c.reply("general", newer.ThreadNo, "", "and bumped", false)

	index := fetchIndex(t, c, "general", 1)
	if index.Threads[0].No != pinned.ThreadNo || !index.Threads[0].Sticky {
		t.Errorf("sticky thread should lead the index, got %+v", index.Threads[0].Thread)
	}
}

func TestBumpLimitStopsBumping(t *testing.T) {
	c := testServer(t, nil)

	c.exec(`UPDATE boards SET bump_limit = 1 WHERE slug = 'general'`)
	first := c.createThread("general", "will stop bumping", "", "opening post")
	second := c.createThread("general", "the other thread", "", "opening post")

	c.reply("general", first.ThreadNo, "", "this one still bumps", false)
	if index := fetchIndex(t, c, "general", 1); index.Threads[0].No != first.ThreadNo {
		t.Fatalf("reply within the bump limit should bump, index leads with No. %d", index.Threads[0].No)
	}

	c.reply("general", second.ThreadNo, "", "bumping the other thread", false)
	c.reply("general", first.ThreadNo, "", "past the bump limit", false)

	index := fetchIndex(t, c, "general", 1)
	if index.Threads[0].No != second.ThreadNo {
		t.Errorf("a reply past the bump limit must not bump, index leads with No. %d", index.Threads[0].No)
	}

	detail := fetchThread(t, c, "general", first.ThreadNo)
	if !detail.AtBumpLimit {
		t.Error("thread should report that it reached the bump limit")
	}
	if detail.Replies != 2 {
		t.Errorf("replies = %d, want 2 (posting still works past the bump limit)", detail.Replies)
	}
}

func TestSinglePageAppFallback(t *testing.T) {
	c := testServer(t, nil)

	for _, path := range []string{"/", "/general/", "/general/catalog", "/general/thread/1"} {
		res, err := http.Get(c.base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", path, res.StatusCode)
		}
		if !strings.Contains(string(body), `<title>educhan</title>`) {
			t.Errorf("GET %s did not serve the app shell", path)
		}
		if got := res.Header.Get("Content-Security-Policy"); got == "" {
			t.Errorf("GET %s: missing Content-Security-Policy", path)
		}
	}

	res, err := http.Get(c.base + "/app.js")
	if err != nil {
		t.Fatalf("GET /app.js: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /app.js: status %d", res.StatusCode)
	}
}

/* Test helpers ------------------------------------------------------------- */

func fetchThread(t *testing.T, c *harness, board string, no int64) ThreadDetail {
	t.Helper()
	var detail ThreadDetail
	c.get(fmt.Sprintf("/api/boards/%s/threads/%d", board, no), &detail)
	return detail
}

func fetchIndex(t *testing.T, c *harness, board string, page int) BoardIndex {
	t.Helper()
	var index BoardIndex
	c.get(fmt.Sprintf("/api/boards/%s/threads?page=%d", board, page), &index)
	return index
}
