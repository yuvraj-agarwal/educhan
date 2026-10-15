package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

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

// addBoard inserts boards straight into the database, with the column defaults
// for every setting. A test posts to boards of its own rather than to seeded
// ones, so it does not depend on which boards the migrations happen to seed;
// one that needs other settings changes them with exec, like any other setup
// the API does not expose to a poster.
func (h *harness) addBoard(slugs ...string) {
	h.t.Helper()
	for _, slug := range slugs {
		h.exec(`INSERT INTO boards (slug, name) VALUES ($1, $1)`, slug)
	}
}

// testServer starts the application against an isolated schema in the
// development database. Tests are skipped when no PostgreSQL instance is
// reachable.
func testServer(t *testing.T, tune func(*Config)) *harness {
	t.Helper()
	return testServerLogging(t, tune, io.Discard)
}

// testServerLogging is testServer with the application's log written to logs
// as JSON, for tests that assert on what the server logs.
func testServerLogging(t *testing.T, tune func(*Config), logs io.Writer) *harness {
	t.Helper()

	ctx := context.Background()
	cfg := Config{
		DatabaseURL:    testDatabaseURL(t),
		PosterIDSalt:   "test-salt",
		LogFormat:      "text",
		UploadDir:      t.TempDir(),
		MaxUploadBytes: defaultMaxUploadBytes,
		UploadsEnabled: true,
	}
	if tune != nil {
		tune(&cfg)
	}

	app, err := New(ctx, cfg, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatalf("start app: %v", err)
	}
	t.Cleanup(app.Close)

	srv := httptest.NewServer(app.Routes())
	t.Cleanup(srv.Close)

	// A jar so the harness behaves like a browser: verification hands out
	// cookies, and posting with a badge depends on one coming back.
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &harness{
		client: &client{t: t, base: srv.URL, http: &http.Client{Jar: jar}},
		app:    app,
	}
}

// testDatabaseURL creates an isolated schema in the development database,
// dropped when the test ends, and returns a connection string whose
// search_path points into it. Tests are skipped when no PostgreSQL instance is
// reachable.
func testDatabaseURL(t *testing.T) string {
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
	return baseURL + separator + "search_path=" + schema
}

type client struct {
	t    *testing.T
	base string
	http *http.Client
	// lastHeader holds the response headers of the most recent request. Flood
	// control asserts on Retry-After, and every other caller wants the decoded
	// body rather than the response.
	lastHeader http.Header
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
	return c.send(req, out)
}

// send performs a prepared request, decoding a JSON response into out and
// returning the status.
func (c *client) send(req *http.Request, out any) int {
	c.t.Helper()

	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer res.Body.Close()
	c.lastHeader = res.Header

	payload, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	if out != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, out); err != nil {
			c.t.Fatalf("%s %s: decode %q: %v", req.Method, req.URL.Path, payload, err)
		}
	}
	return res.StatusCode
}

// createdPost is the API acknowledgement for a new thread or reply, plus the
// fields a refusal carries: Error, and for a refused badge the machine-readable
// state of the credential that was presented.
type createdPost struct {
	Board      string `json:"board"`
	No         int64  `json:"no"`
	ThreadNo   int64  `json:"thread_no"`
	Error      string `json:"error"`
	Credential string `json:"credential"`
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

// seededBoards is the board set the migrations open with, in listing order:
// the subjects, then student life, with /meta/ last.
var seededBoards = []string{
	"math", "phys", "chem", "bio", "cs", "eng", "med", "law", "econ", "lit", "his", "phil", "lang",
	"adm", "exam", "grad", "career", "abroad", "adv", "meta",
}

func TestBoardsAreSeeded(t *testing.T) {
	c := testServer(t, nil)
	// Opened later and alphabetically first, so it would lead a listing ordered
	// by either; the seeded set has to come first regardless.
	c.addBoard("aaa")

	var out struct {
		Boards []Board `json:"boards"`
	}
	c.get("/api/boards", &out)

	var slugs []string
	for _, b := range out.Boards {
		slugs = append(slugs, b.Slug)
	}
	if want := append(slices.Clone(seededBoards), "aaa"); !slices.Equal(slugs, want) {
		t.Fatalf("boards = %v, want exactly %v in that order", slugs, want)
	}

	// The migration writes these rows without going through the API, so hold
	// them to what the API would have accepted.
	for _, b := range out.Boards[:len(seededBoards)] {
		switch {
		case !boardSlugPattern.MatchString(b.Slug) || reservedBoardSlugs[b.Slug]:
			t.Errorf("/%s/ is not a slug the API would accept", b.Slug)
		case b.Name == "" || utf8.RuneCountInString(b.Name) > maxBoardNameLen:
			t.Errorf("/%s/ has an unusable name %q", b.Slug, b.Name)
		case b.Description == "" || utf8.RuneCountInString(b.Description) > maxBoardDescLen:
			t.Errorf("/%s/ has an unusable description %q", b.Slug, b.Description)
		case b.BumpLimit != defaultBoardBumpLimit || b.ThreadLimit != defaultBoardThreadLimit ||
			b.ImageLimit != defaultBoardImageLimit || b.ArchiveDays != defaultBoardArchiveDays:
			t.Errorf("/%s/ does not have the default limits: %+v", b.Slug, b)
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
	c.addBoard("general", "research")

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
	c.addBoard("general")

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
	// "#secret" makes the salted tripcode, the only kind there is.
	if reply.Name != "ana" || !strings.HasPrefix(reply.Tripcode, "!!") {
		t.Errorf("reply identity = %q/%q, want ana with a \"!!\" tripcode", reply.Name, reply.Tripcode)
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
	c.addBoard("general")

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
	c.addBoard("general")

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
	c.addBoard("general")

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
	c.addBoard("general")

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
	c.addBoard("general")

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

// The catalog is in bump order and nothing else decides it: a thread with
// more replies does not outrank a newer one. Ranking by how busy a thread is
// would make the catalog a popularity feed, which the child-safety rules in
// AGENTS.md keep out.
func TestCatalogOrdersByBumpNotByReplies(t *testing.T) {
	c := testServer(t, nil)
	c.addBoard("general")

	busy := c.createThread("general", "busy", "", "lots of replies")
	for i := 1; i <= 5; i++ {
		c.reply("general", busy.ThreadNo, "", fmt.Sprintf("saged reply %d", i), true)
	}
	quiet := c.createThread("general", "quiet", "", "no replies at all")

	order := func() []int64 {
		var catalog Catalog
		c.get("/api/boards/general/catalog", &catalog)
		var nos []int64
		for _, tile := range catalog.Threads {
			nos = append(nos, tile.No)
		}
		return nos
	}
	if got, want := order(), []int64{quiet.ThreadNo, busy.ThreadNo}; !slices.Equal(got, want) {
		t.Fatalf("catalog = %v, want %v: the last bumped thread leads, however many replies the other has", got, want)
	}

	c.reply("general", busy.ThreadNo, "", "a reply that bumps", false)
	if got, want := order(), []int64{busy.ThreadNo, quiet.ThreadNo}; !slices.Equal(got, want) {
		t.Errorf("catalog = %v, want %v after a bumping reply", got, want)
	}
}

func TestValidation(t *testing.T) {
	c := testServer(t, nil)
	c.addBoard("general")

	cases := []struct {
		name      string
		body      map[string]any
		want      int
		wantError string
	}{
		{name: "empty body", body: map[string]any{"body": "   "}, want: http.StatusBadRequest},
		{name: "body too long", body: map[string]any{"body": strings.Repeat("a", maxBodyLen+1)}, want: http.StatusBadRequest},
		{name: "too many lines", body: map[string]any{"body": strings.Repeat("a\n", maxBodyLines+1)}, want: http.StatusBadRequest},
		{name: "subject too long", body: map[string]any{"body": "ok", "subject": strings.Repeat("s", maxSubjectLen+1)}, want: http.StatusBadRequest},
		{name: "unknown field", body: map[string]any{"body": "ok", "author": "legacy"}, want: http.StatusBadRequest, wantError: `unknown field "author"`},
		{name: "valid", body: map[string]any{"body": "ok"}, want: http.StatusCreated},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out createdPost
			if status := c.post("/api/boards/general/threads", tc.body, &out); status != tc.want {
				t.Errorf("status = %d, want %d (%s)", status, tc.want, out.Error)
			}
			if tc.wantError != "" && out.Error != tc.wantError {
				t.Errorf("error = %q, want %q", out.Error, tc.wantError)
			}
		})
	}
}

func TestNotFoundResponses(t *testing.T) {
	c := testServer(t, nil)
	c.addBoard("general")

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
	c.addBoard("general", "research")

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

// A post link names a board and a number and nothing else, so the resolver is
// what turns >>>/slug/123 into a thread to open and a post to scroll to. It
// answers exactly the board, the post, and the thread, and nothing about the
// post itself.
func TestResolvePost(t *testing.T) {
	h := modServer(t)
	h.addBoard("here", "there")

	thread := h.createThread("here", "", "", "opening post")
	reply := h.reply("here", thread.ThreadNo, "", "a reply", false)
	other := h.createThread("here", "", "", "another thread")
	// Numbered from 1 like every board, so its numbers collide with /here/'s.
	elsewhere := h.createThread("there", "", "", "on another board")
	elsewhereReply := h.reply("there", elsewhere.ThreadNo, "", "No. 2 on /there/", false)

	resolve := func(path string) (int, map[string]any) {
		t.Helper()
		var out map[string]any
		return h.do(http.MethodGet, path, nil, &out), out
	}
	expect := func(board string, no, threadNo int64) {
		t.Helper()
		path := fmt.Sprintf("/api/boards/%s/posts/%d", board, no)
		status, out := resolve(path)
		want := map[string]any{"board": board, "no": float64(no), "thread_no": float64(threadNo)}
		if status != http.StatusOK || !maps.Equal(out, want) {
			t.Errorf("GET %s: status %d, %v; want 200, %v", path, status, out, want)
		}
	}

	expect("here", thread.No, thread.ThreadNo)
	expect("here", reply.No, thread.ThreadNo)
	expect("here", other.No, other.ThreadNo)
	expect("there", elsewhere.No, elsewhere.ThreadNo)
	expect("there", elsewhereReply.No, elsewhere.ThreadNo)

	// An archived thread still loads, so its posts still resolve.
	h.archiveThread("here", thread.ThreadNo, 0)
	expect("here", reply.No, thread.ThreadNo)

	// /there/ has a No. 2 and so does /here/; a deleted one is gone from
	// its own board only.
	h.modAction(http.MethodDelete, fmt.Sprintf("/api/boards/there/threads/%d/posts/%d", elsewhere.ThreadNo, elsewhereReply.No))
	expect("here", reply.No, thread.ThreadNo)

	for path, wantStatus := range map[string]int{
		fmt.Sprintf("/api/boards/there/posts/%d", elsewhereReply.No): http.StatusNotFound,
		"/api/boards/here/posts/999":                                 http.StatusNotFound,
		"/api/boards/nowhere/posts/1":                                http.StatusNotFound,
		"/api/boards/here/posts/abc":                                 http.StatusBadRequest,
		"/api/boards/here/posts/0":                                   http.StatusBadRequest,
		"/api/boards/here/posts/-1":                                  http.StatusBadRequest,
	} {
		status, out := resolve(path)
		if status != wantStatus || out["error"] == nil {
			t.Errorf("GET %s: status %d, %v; want %d with an error", path, status, out, wantStatus)
		}
	}
}

func TestFloodControl(t *testing.T) {
	c := testServer(t, func(cfg *Config) {
		cfg.ThreadCooldown = time.Minute
		cfg.PostCooldown = time.Minute
	})
	c.addBoard("general")

	thread := c.createThread("general", "first", "", "allowed")

	var out map[string]any
	status := c.post("/api/boards/general/threads", map[string]any{"body": "too soon"}, &out)
	if status != http.StatusTooManyRequests {
		t.Fatalf("second thread status = %d, want 429", status)
	}
	if _, ok := out["retry_after"]; !ok {
		t.Errorf("429 body should include retry_after, got %v", out)
	}
	if got := c.lastHeader.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}

	// Replies have their own cooldown, so the first one still goes through.
	c.reply("general", thread.ThreadNo, "", "first reply", false)
	status = c.post(fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo),
		map[string]any{"body": "too soon"}, &out)
	if status != http.StatusTooManyRequests {
		t.Errorf("second reply status = %d, want 429", status)
	}
	if got := c.lastHeader.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
}

// Posting cooldowns are per board, so a client can follow a conversation on two
// boards without being told to slow down.
func TestFloodControlIsPerBoard(t *testing.T) {
	c := testServer(t, func(cfg *Config) { cfg.ThreadCooldown = time.Minute })
	c.addBoard("general", "research")

	c.createThread("general", "first", "", "allowed")
	c.createThread("research", "first", "", "a different board has its own cooldown")

	var out map[string]any
	if status := c.post("/api/boards/general/threads",
		map[string]any{"body": "too soon"}, &out); status != http.StatusTooManyRequests {
		t.Errorf("second thread on the same board = %d, want 429", status)
	}
}

// Uploads are limited across the whole server: thumbnailing costs the same
// wherever the post lands, so a second board is no way around it.
func TestUploadFloodControl(t *testing.T) {
	c := testServer(t, func(cfg *Config) { cfg.UploadCooldown = time.Minute })
	c.addBoard("general", "research")
	image := pngBytes(t, 32, 32)

	var out map[string]any
	if status := c.upload("/api/boards/general/threads",
		map[string]string{"body": "with an image"}, "first.png", image, &out); status != http.StatusCreated {
		t.Fatalf("first upload: status %d (%v)", status, out["error"])
	}

	status := c.upload("/api/boards/research/threads",
		map[string]string{"body": "too soon"}, "second.png", image, &out)
	if status != http.StatusTooManyRequests {
		t.Fatalf("second upload status = %d, want 429", status)
	}
	if got := c.lastHeader.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}

	// Text-only posting is unaffected: the upload cooldown limits attachments,
	// not conversation.
	if status := c.post("/api/boards/general/threads",
		map[string]any{"body": "no image"}, &out); status != http.StatusCreated {
		t.Errorf("text-only thread status = %d, want 201 (%v)", status, out["error"])
	}
}

// A post refused by the upload cooldown must not consume the reply cooldown as
// well, or the two limits compound.
func TestUploadRejectionDoesNotConsumeThePostCooldown(t *testing.T) {
	c := testServer(t, func(cfg *Config) {
		cfg.PostCooldown = time.Millisecond
		cfg.UploadCooldown = time.Minute
	})
	c.addBoard("general")
	image := pngBytes(t, 32, 32)

	thread := c.createThread("general", "uploads", "", "opening post")

	var out map[string]any
	path := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)
	if status := c.upload(path, map[string]string{"body": "first image"},
		"first.png", image, &out); status != http.StatusCreated {
		t.Fatalf("first image reply: status %d (%v)", status, out["error"])
	}
	if status := c.upload(path, map[string]string{"body": "second image"},
		"second.png", image, &out); status != http.StatusTooManyRequests {
		t.Fatalf("second image reply status = %d, want 429", status)
	}

	time.Sleep(5 * time.Millisecond)
	if status := c.post(path, map[string]any{"body": "text only"}, &out); status != http.StatusCreated {
		t.Errorf("text reply status = %d, want 201 (%v): the refused upload consumed the reply cooldown",
			status, out["error"])
	}
}

// Flood control keys on the client address, and the address is not meant to
// outlive the cooldowns it is held for. The running app sweeps every limiter
// on a timer of its own, so an address that posts once and never comes back is
// forgotten without another request arriving to prompt it.
func TestFloodControlForgetsAddressesOnAQuietServer(t *testing.T) {
	h := testServer(t, func(cfg *Config) {
		cfg.TrustedProxyHops = 1
		cfg.ThreadCooldown = time.Minute
		cfg.PostCooldown = time.Minute
		cfg.UploadCooldown = time.Minute
		cfg.VerifyCooldown = time.Minute
		cfg.cooldownSweepEvery = time.Millisecond
	})
	h.addBoard("general")

	const address = "198.51.100.7"
	var thread createdPost
	if status := h.uploadWithHeaders(http.Header{"X-Forwarded-For": {address}}, "/api/boards/general/threads",
		map[string]string{"body": "posted once"}, "once.png", pngBytes(t, 24, 24), &thread); status != http.StatusCreated {
		t.Fatalf("thread: status %d (%s)", status, thread.Error)
	}
	if status := h.postFrom(address, fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo),
		map[string]any{"body": "and replied"}, &createdPost{}); status != http.StatusCreated {
		t.Fatalf("reply: status %d", status)
	}
	// Verification is off here, so its limiter is fed the way a code
	// request would feed it.
	h.app.verifyLimiter.take("ip|" + address)

	limiters := map[string]*cooldownLimiter{
		"thread": h.app.threadLimiter, "post": h.app.postLimiter,
		"upload": h.app.uploadLimiter, "verify": h.app.verifyLimiter,
	}
	holding := func() []string {
		var held []string
		for name, l := range limiters {
			for _, key := range trackedKeys(l) {
				if strings.Contains(key, address) {
					held = append(held, name+": "+key)
				}
			}
		}
		slices.Sort(held)
		return held
	}
	if got := holding(); len(got) != 4 {
		t.Fatalf("the limiters hold %v, want the address in all four", got)
	}

	// A minute on, every cooldown has run out; nothing else is sent.
	later := time.Now().Add(time.Minute)
	for _, l := range limiters {
		setClock(l, func() time.Time { return later })
	}
	waitUntil(t, "no limiter holds the address", func() bool { return len(holding()) == 0 })
}

// A host that gives the process CPU only while it answers a request, which is
// Cloud Run's default, does not run the timer while the instance is idle. The
// request that wakes it runs the overdue sweep before it is handled, so an
// address whose cooldowns have run out is gone before anything else happens,
// whatever that request is for.
func TestFloodControlForgetsAddressesBeforeTheNextRequest(t *testing.T) {
	const every = 20 * time.Millisecond
	h := testServer(t, func(cfg *Config) {
		cfg.TrustedProxyHops = 1
		cfg.PostCooldown = time.Minute
		cfg.cooldownSweepEvery = every
	})
	h.addBoard("general")
	thread := h.createThread("general", "", "", "a thread")

	const address = "198.51.100.8"
	if status := h.postFrom(address, fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo),
		map[string]any{"body": "posted once"}, &createdPost{}); status != http.StatusCreated {
		t.Fatalf("reply: status %d", status)
	}

	// The instance goes idle: its timer stops, the reply's cooldown runs out,
	// and a sweep falls due with nothing to run it.
	h.app.stopCooldownSweep()
	later := time.Now().Add(time.Minute)
	setClock(h.app.postLimiter, func() time.Time { return later })
	time.Sleep(every)
	if got := trackedKeys(h.app.postLimiter); len(got) != 1 || !strings.Contains(got[0], address) {
		t.Fatalf("while idle the post limiter holds %v, want the address still there for a request to sweep", got)
	}

	if status := h.do(http.MethodGet, "/healthz", nil, nil); status != http.StatusOK {
		t.Fatalf("healthz: status %d", status)
	}
	if got := trackedKeys(h.app.postLimiter); len(got) != 0 {
		t.Errorf("after the next request the post limiter holds %v, want nothing", got)
	}
}

func TestThreadPruning(t *testing.T) {
	c := testServer(t, nil)
	c.addBoard("general")

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
	c.addBoard("general")

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
	c.addBoard("general")

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
	c.addBoard("general")

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

// TestPoolSizeIsCapped: the configured cap is what the pool actually uses,
// since instances times this is the database's connection budget.
func TestPoolSizeIsCapped(t *testing.T) {
	h := testServer(t, func(cfg *Config) { cfg.DBMaxConns = 2 })
	if got := h.app.pool.Config().MaxConns; got != 2 {
		t.Errorf("pool MaxConns = %d, want 2", got)
	}
}
