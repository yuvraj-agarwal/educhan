package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLoadMigrations(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migrations) < 2 {
		t.Fatalf("loaded %d migrations, want at least 2", len(migrations))
	}

	for i, m := range migrations {
		if i > 0 && m.version <= migrations[i-1].version {
			t.Errorf("migrations are not ordered: %q follows %q", m.name, migrations[i-1].name)
		}
		if strings.TrimSpace(m.sql) == "" {
			t.Errorf("migration %q is empty", m.name)
		}
	}
	if migrations[0].version != 1 {
		t.Errorf("first migration version = %d, want 1", migrations[0].version)
	}
}

// databaseAt returns a fresh schema migrated up to and including version, as a
// pool for writing data into it and the URL a server can later be started on.
// Starting one applies the remaining migrations, the way a deploy would.
func databaseAt(t *testing.T, version int) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	url := testDatabaseURL(t)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	all, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	var upTo []migration
	for _, m := range all {
		if m.version <= version {
			upTo = append(upTo, m)
		}
	}
	if _, err := applyMigrations(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), upTo); err != nil {
		t.Fatalf("migrate to version %d: %v", version, err)
	}
	return pool, url
}

// serverOn starts the application on a database databaseAt prepared. The
// schema testServer makes for itself is left empty and dropped with the rest.
func serverOn(t *testing.T, url string) *harness {
	t.Helper()
	return testServer(t, func(cfg *Config) { cfg.DatabaseURL = url })
}

// seedThread writes a one-post thread straight into a version 5 schema. It is
// SQL rather than the store because the store follows the latest schema, and
// this has to keep working against the old one after the store moves on.
func seedThread(t *testing.T, pool *pgxpool.Pool, board, subject string) {
	t.Helper()
	ctx := context.Background()
	for _, step := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO threads (board_id, no, subject) SELECT id, 1, $2 FROM boards WHERE slug = $1`,
			[]any{board, subject}},
		{`INSERT INTO posts (thread_id, board_id, no, body, is_op)
		  SELECT t.id, t.board_id, 1, $2, TRUE FROM threads t JOIN boards b ON b.id = t.board_id
		  WHERE b.slug = $1 AND t.no = 1`, []any{board, "the opening post"}},
		{`UPDATE boards SET next_post_no = 2 WHERE slug = $1`, []any{board}},
	} {
		if tag, err := pool.Exec(ctx, step.query, step.args...); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("seed a thread on /%s/: %v (%d rows)", board, err, tag.RowsAffected())
		}
	}
}

func boardID(t *testing.T, pool *pgxpool.Pool, slug string) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(), `SELECT id FROM boards WHERE slug = $1`, slug).Scan(&id); err != nil {
		t.Fatalf("id of /%s/: %v", slug, err)
	}
	return id
}

// The three boards 0001 seeded are renamed in place by 0006: same row, new
// address, and every thread on them still there and still taking replies.
func TestBoardSetRenamesKeepTheirThreads(t *testing.T) {
	pool, url := databaseAt(t, 5)
	renames := map[string]string{"placements": "career", "research": "grad", "general": "meta"}
	ids := map[string]int{}
	for old := range renames {
		seedThread(t, pool, old, "posted on /"+old+"/")
		ids[old] = boardID(t, pool, old)
	}

	h := serverOn(t, url)
	for old, renamed := range renames {
		if id := boardID(t, pool, renamed); id != ids[old] {
			t.Errorf("/%s/ is row %d, want row %d, the /%s/ it was renamed from", renamed, id, ids[old], old)
		}
		var out map[string]any
		if status := h.do(http.MethodGet, "/api/boards/"+old, nil, &out); status != http.StatusNotFound {
			t.Errorf("GET /api/boards/%s: status %d, want 404 once it is renamed", old, status)
		}

		detail := fetchThread(t, h, renamed, 1)
		if detail.Subject != "posted on /"+old+"/" || detail.Board.Slug != renamed {
			t.Errorf("/%s/ thread 1 = %q on /%s/, want the thread from /%s/", renamed, detail.Subject, detail.Board.Slug, old)
		}
		// Numbering carries on from where the old board left it.
		if reply := h.reply(renamed, 1, "", "still open", false); reply.No != 2 {
			t.Errorf("first reply on /%s/ = No. %d, want 2", renamed, reply.No)
		}
	}

	var meta Board
	h.get("/api/boards/meta", &meta)
	if meta.Name != "Meta" || !strings.Contains(meta.Description, "educhan") {
		t.Errorf("/meta/ = %+v, want the new name and description", meta)
	}
}

// A database that already has a board under one of the new slugs — one a
// moderator opened before 0006 ran — keeps it as it was. The rename that would
// collide with it is skipped, and the old board stays where it was.
func TestBoardSetLeavesExistingBoardsAlone(t *testing.T) {
	pool, url := databaseAt(t, 5)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO boards (slug, name, description, thread_limit) VALUES
		    ('meta', 'Ours', 'opened by hand', 7),
		    ('math', 'Maths', 'also by hand', 9)`); err != nil {
		t.Fatalf("insert boards: %v", err)
	}
	seedThread(t, pool, "general", "still on /general/")

	h := serverOn(t, url)
	for slug, want := range map[string]Board{
		"meta": {Slug: "meta", Name: "Ours", Description: "opened by hand", ThreadLimit: 7},
		"math": {Slug: "math", Name: "Maths", Description: "also by hand", ThreadLimit: 9},
	} {
		var got Board
		h.get("/api/boards/"+slug, &got)
		if got.Name != want.Name || got.Description != want.Description || got.ThreadLimit != want.ThreadLimit {
			t.Errorf("/%s/ = %+v, want it untouched: %+v", slug, got, want)
		}
	}
	if detail := fetchThread(t, h, "general", 1); detail.Subject != "still on /general/" {
		t.Errorf("/general/ thread 1 = %q, want it where it was", detail.Subject)
	}

	// The renames that had room still happened, and the listing puts the whole
	// set in its order whichever row holds each slug, with the leftover after.
	var out struct {
		Boards []Board `json:"boards"`
	}
	h.get("/api/boards", &out)
	var slugs []string
	for _, b := range out.Boards {
		slugs = append(slugs, b.Slug)
	}
	if want := append(slices.Clone(seededBoards), "general"); !slices.Equal(slugs, want) {
		t.Errorf("boards = %v, want %v", slugs, want)
	}
}

// Phase 0 reserved only the server's own routes, so a moderator could open
// /archive/ or /rules/; those words are now pages of the site, and the UI
// reads the path as the page. Starting up moves such a board to
// "<slug>-board", keeping its row and its threads, rather than leaving it in
// the board list with every link to it landing somewhere else.
func TestBoardsUnderNewlyReservedSlugsAreRenamed(t *testing.T) {
	pool, url := databaseAt(t, 4)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO boards (slug, name) VALUES
		    ('archive', 'Archive'), ('rules', 'Rules'), ('rules-board', 'Taken'), ('u', 'U')`); err != nil {
		t.Fatalf("insert boards: %v", err)
	}
	seedThread(t, pool, "archive", "posted on /archive/")
	ids := map[string]int{"archive": boardID(t, pool, "archive"), "rules": boardID(t, pool, "rules"), "u": boardID(t, pool, "u")}

	var logs syncBuffer
	h := testServerLogging(t, func(cfg *Config) { cfg.DatabaseURL = url }, &logs)
	for old, renamed := range map[string]string{"archive": "archive-board", "rules": "rules-board-2", "u": "board-u"} {
		if id := boardID(t, pool, renamed); id != ids[old] {
			t.Errorf("/%s/ is row %d, want row %d, the /%s/ it was renamed from", renamed, id, ids[old], old)
		}
		if !strings.Contains(logs.String(), `"from":"`+old+`","to":"`+renamed+`"`) {
			t.Errorf("the rename of /%s/ to /%s/ was not logged:\n%s", old, renamed, logs.String())
		}
	}
	if detail := fetchThread(t, h, "archive-board", 1); detail.Subject != "posted on /archive/" {
		t.Errorf("/archive-board/ thread 1 = %q, want the thread from /archive/", detail.Subject)
	}
	var taken Board
	h.get("/api/boards/rules-board", &taken)
	if taken.Name != "Taken" {
		t.Errorf("/rules-board/ = %+v, want the board that already had the slug, untouched", taken)
	}
}

// A database a newer release migrated is one this binary does not fully
// understand. It still serves — a rollback has to be possible — but says so
// loudly, and the sweeper, which deletes for good, refuses to run on it.
func TestSchemaFromANewerReleaseStopsTheSweeper(t *testing.T) {
	pool, url := databaseAt(t, 1000)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO schema_migrations (version, name) VALUES (1001, '1001_from_the_future.sql')`); err != nil {
		t.Fatalf("record a future migration: %v", err)
	}

	var logs syncBuffer
	h := testServerLogging(t, func(cfg *Config) { cfg.DatabaseURL = url }, &logs)
	if !strings.Contains(logs.String(), `"unknown_versions":[1001]`) {
		t.Errorf("starting on a newer schema logged nothing about it:\n%s", logs.String())
	}
	if _, err := h.app.Sweep(context.Background()); err == nil || !strings.Contains(err.Error(), "1001") {
		t.Errorf("sweep on a newer schema = %v, want a refusal naming the version", err)
	}
	var out map[string]any
	if status := h.do(http.MethodGet, "/api/boards", nil, &out); status != http.StatusOK {
		t.Errorf("GET /api/boards on a newer schema: status %d, want the server still serving", status)
	}
}

// Attachments used to keep the filename the poster's browser sent. 0008
// renames every existing one the way new uploads are named, "image" and the
// stored file's extension, so no filename a poster sent survives the
// migration.
func TestAttachmentNamesAreRewritten(t *testing.T) {
	pool, url := databaseAt(t, 7)
	ctx := context.Background()
	seedThread(t, pool, "meta", "photos from the fest")
	for no, row := range []struct{ name, path string }{
		{"IMG_20261013_Priya.jpg", "meta/2026/10/aaaa.jpg"},
		{"Screenshot from 2026-10-12 Priya.PNG", "meta/2026/10/bbbb.png"},
		{"", "meta/2026/10/cccc.webp"},
		{"Priya's cat.webp", "meta/2026/10/dddd.gif"},
	} {
		if _, err := pool.Exec(ctx, `
			WITH p AS (
			    INSERT INTO posts (thread_id, board_id, no, body, is_op)
			    SELECT t.id, t.board_id, $1, 'a reply', FALSE
			    FROM threads t JOIN boards b ON b.id = t.board_id
			    WHERE b.slug = 'meta' AND t.no = 1
			    RETURNING id
			)
			INSERT INTO attachments (post_id, original_name, original_path, thumb_path, content_type,
			    thumb_content_type, byte_size, width, height, thumb_width, thumb_height)
			SELECT id, $2, $3, $3, 'image/jpeg', 'image/jpeg', 1, 1, 1, 1, 1 FROM p`,
			no+2, row.name, row.path); err != nil {
			t.Fatalf("seed attachment %q: %v", row.name, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE boards SET next_post_no = 6 WHERE slug = 'meta'`); err != nil {
		t.Fatalf("advance numbering: %v", err)
	}

	h := serverOn(t, url)
	var names []string
	for _, p := range fetchThread(t, h, "meta", 1).Posts[1:] {
		if p.Attachment == nil {
			t.Fatalf("post No. %d lost its attachment", p.No)
		}
		names = append(names, p.Attachment.Name)
	}
	if got, want := strings.Join(names, ","), "image.jpg,image.png,image.webp,image.gif"; got != want {
		t.Errorf("attachment names = %s, want %s", got, want)
	}
	for _, table := range h.tables() {
		if n := h.rowsContaining(table, []byte("Priya")); n != 0 {
			t.Errorf("%d rows of %s still contain a poster's filename", n, table)
		}
	}
}
