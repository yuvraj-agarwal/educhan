package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sweepEnv points the process configuration at an isolated schema, the way the
// app package's harness does, and pins every setting that would otherwise come
// from whatever environment runs the tests.
func sweepEnv(t *testing.T) *pgxpool.Pool {
	t.Helper()

	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		baseURL = os.Getenv("DATABASE_URL")
	}
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

	schema := fmt.Sprintf("educhan_cmd_test_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	schemaURL := baseURL + separator + "search_path=" + schema
	t.Setenv("DATABASE_URL", schemaURL)
	t.Setenv("EDUCHAN_MEDIA_BACKEND", "local")
	t.Setenv("EDUCHAN_UPLOAD_DIR", t.TempDir())
	t.Setenv("EDUCHAN_VERIFY_SECRET", "")
	t.Setenv("EDUCHAN_LOG_FORMAT", "text")

	pool, err := pgxpool.New(ctx, schemaURL)
	if err != nil {
		t.Fatalf("connect to the test schema: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// -sweep is a whole process run: configuration, connection, migrations, one
// pass over the archive, and an exit status, with no server left listening.
func TestSweepModeSweepsAndExits(t *testing.T) {
	db := sweepEnv(t)
	ctx := context.Background()

	// The first run has an empty schema to work on, which also proves the
	// mode migrates before it sweeps.
	if err := run([]string{"-sweep"}); err != nil {
		t.Fatalf("sweep on an empty database: %v", err)
	}

	if _, err := db.Exec(ctx, `
		WITH board AS (
		    INSERT INTO boards (slug, name, archive_days) VALUES ('job', 'job', 7) RETURNING id
		), made AS (
		    INSERT INTO threads (board_id, no, archived_at)
		    SELECT board.id, n, now() - n * interval '4 days'
		    FROM board, generate_series(1, 2) AS n
		    RETURNING id, board_id, no
		)
		INSERT INTO posts (board_id, thread_id, no, body, is_op)
		SELECT board_id, id, no, 'archived', TRUE FROM made`); err != nil {
		t.Fatalf("seed archived threads: %v", err)
	}

	if err := run([]string{"-sweep"}); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// No. 1 was archived four days ago and keeps; No. 2, eight days ago, does not.
	rows, err := db.Query(ctx, `SELECT no FROM threads ORDER BY no`)
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	defer rows.Close()
	var left []int64
	for rows.Next() {
		var no int64
		if err := rows.Scan(&no); err != nil {
			t.Fatalf("scan: %v", err)
		}
		left = append(left, no)
	}
	if fmt.Sprint(left) != "[1]" {
		t.Errorf("threads left after the sweep = %v, want [1]", left)
	}
}

// A sweep that cannot do its job has to say so in its exit status, or the
// scheduler reports every run as a success.
func TestSweepModeFailsWithoutADatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://educhan:educhan@127.0.0.1:1/educhan?sslmode=disable&connect_timeout=2")
	t.Setenv("EDUCHAN_MEDIA_BACKEND", "local")
	t.Setenv("EDUCHAN_UPLOAD_DIR", t.TempDir())
	t.Setenv("EDUCHAN_VERIFY_SECRET", "")

	if err := run([]string{"-sweep"}); err == nil {
		t.Error("a sweep with no database reported success")
	}
}

// A mistyped flag must not fall through to serving: a Job that started a
// server instead of sweeping would never finish.
func TestRunRefusesWhatItDoesNotUnderstand(t *testing.T) {
	for _, args := range [][]string{{"sweep"}, {"-sweep", "now"}, {"-swept"}} {
		if err := run(args); err == nil {
			t.Errorf("run(%q) succeeded, want an error", args)
		}
	}
}
