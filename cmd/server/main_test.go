package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mainArgsEnv, when set, has this test binary run main with those arguments
// in place of the tests: a test that needs the whole process, its signals,
// its last log lines and its exit status, runs the binary again with it.
const mainArgsEnv = "EDUCHAN_TEST_MAIN_ARGS"

func TestMain(m *testing.M) {
	if args, ok := os.LookupEnv(mainArgsEnv); ok {
		os.Args = append(os.Args[:1], strings.Fields(args)...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

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
	if err := run([]string{"-sweep"}, io.Discard); err != nil {
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

	if err := run([]string{"-sweep"}, io.Discard); err != nil {
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

// The sweeper Job runs deliberately without the poster-ID salt, the
// moderation key and the verification secret, which only a web server uses.
// So a sweep says nothing about them: a warning there would read as a fault
// in the Job and invite an operator to hand it secrets it does not need. The
// web server's own warnings are WarnServingSettings', which only serving calls.
func TestSweepModeLogsNoServerWarnings(t *testing.T) {
	sweepEnv(t)
	t.Setenv("EDUCHAN_POSTER_ID_SALT", "")
	t.Setenv("EDUCHAN_MOD_KEY", "")

	var logs strings.Builder
	if err := run([]string{"-sweep"}, &logs); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	out := logs.String()
	// The capture works: the sweep's own record is in it.
	if !strings.Contains(out, "msg=sweep ") {
		t.Fatalf("the sweep's record is missing from its log:\n%s", out)
	}
	for _, setting := range []string{"EDUCHAN_POSTER_ID_SALT", "EDUCHAN_MOD_KEY", "EDUCHAN_VERIFY_SECRET", "EDUCHAN_MAIL_DEV"} {
		if strings.Contains(out, setting) {
			t.Errorf("the sweep logged about %s, which only the web server uses:\n%s", setting, out)
		}
	}
	if strings.Contains(out, "level=WARN") {
		t.Errorf("the sweep logged a warning:\n%s", out)
	}
}

// A sweep that cannot do its job has to say so in its exit status, or the
// scheduler reports every run as a success, and say why in its log, once.
func TestSweepModeFailsWithoutADatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://educhan:educhan@127.0.0.1:1/educhan?sslmode=disable&connect_timeout=2")
	t.Setenv("EDUCHAN_MEDIA_BACKEND", "local")
	t.Setenv("EDUCHAN_UPLOAD_DIR", t.TempDir())
	t.Setenv("EDUCHAN_VERIFY_SECRET", "")
	t.Setenv("EDUCHAN_LOG_FORMAT", "text")

	var logs strings.Builder
	if err := run([]string{"-sweep"}, &logs); err == nil {
		t.Error("a sweep with no database reported success")
	}
	wantOneFatalLine(t, logs.String(), "ping postgres")
}

// A server that cannot listen stops there, and says why in its log, once.
func TestServingFailsWhenTheAddressIsTaken(t *testing.T) {
	sweepEnv(t)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer taken.Close()
	t.Setenv("EDUCHAN_ADDR", taken.Addr().String())
	// serve listens for SIGINT and SIGTERM until the process exits, and this
	// one goes on to run other tests.
	t.Cleanup(func() { signal.Reset(syscall.SIGINT, syscall.SIGTERM) })

	var logs strings.Builder
	if err := run(nil, &logs); err == nil {
		t.Fatal("a server whose address was taken reported success")
	}
	wantOneFatalLine(t, logs.String(), "address already in use")
}

// A mistyped flag must not fall through to serving: a Job that started a
// server instead of sweeping would never finish. Neither may a setting the
// process cannot read. Either is refused before anything connects, and said
// in the log, once, as the fatal line and nothing else: not as well in the
// flag package's own words with the usage after them, which would sit in
// plain text among a JSON log's lines. The whole process is run, and all it
// writes is read: a flag set left to write would write to its stderr, not to
// the log run was given.
func TestRunRefusesWhatItDoesNotUnderstand(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       string
		key, value string
		want       string
	}{
		{"a stray word", "sweep", "", "", "unexpected arguments"},
		{"a word after the flag", "-sweep now", "", "", "unexpected arguments"},
		{"a flag it does not know", "-swept", "", "", "flag provided but not defined: -swept"},
		{"a setting it cannot read", "-sweep", "EDUCHAN_POST_COOLDOWN", "soon", "EDUCHAN_POST_COOLDOWN"},
		{"a log format it does not know", "-sweep", "EDUCHAN_LOG_FORMAT", "xml", "EDUCHAN_LOG_FORMAT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{"EDUCHAN_LOG_FORMAT=text"}
			if tc.key != "" {
				env = append(env, tc.key+"="+tc.value)
			}
			stdout, stderr, err := runProcess(t, tc.args, env...)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("educhan %s ended with %v, want exit status 1; it wrote:\n%s", tc.args, err, stderr)
			}
			wantOneFatalLine(t, stderr, tc.want)
			if lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n"); len(lines) != 1 {
				t.Errorf("educhan %s wrote %d lines, want the fatal line alone:\n%s", tc.args, len(lines), stderr)
			}
			if stdout != "" {
				t.Errorf("educhan %s wrote %q to stdout, want nothing", tc.args, stdout)
			}
		})
	}
}

// -h is no failure: the usage is written to the process's log, once, and
// nothing else is written, which the whole process is run to see.
func TestRunWritesTheUsageWhenAsked(t *testing.T) {
	stdout, stderr, err := runProcess(t, "-h", "EDUCHAN_LOG_FORMAT=text")
	if err != nil {
		t.Fatalf("educhan -h ended with %v, want success; it wrote:\n%s", err, stderr)
	}
	if !strings.HasPrefix(stderr, "Usage of educhan:\n") || strings.Count(stderr, "Usage of educhan:") != 1 ||
		!strings.Contains(stderr, "-sweep") || strings.Contains(stderr, "level=") {
		t.Errorf("educhan -h wrote %q, want the usage once and nothing else", stderr)
	}
	if stdout != "" {
		t.Errorf("educhan -h wrote %q to stdout, want nothing", stdout)
	}
}

// runProcess runs this test binary as a whole educhan process with args, as
// mainArgsEnv takes them, and env added to the test's own environment, and
// returns all it wrote to stdout and to stderr, and how it ended.
func runProcess(t *testing.T, args string, env ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(append(os.Environ(), mainArgsEnv+"="+args), env...)
	var out, errs strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errs
	err = cmd.Run()
	return out.String(), errs.String(), err
}

// wantOneFatalLine fails t unless the log out has exactly one error line, the
// fatal one, and it carries want.
func wantOneFatalLine(t *testing.T, out, want string) {
	t.Helper()
	var failures []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "level=ERROR") {
			failures = append(failures, line)
		}
	}
	if len(failures) != 1 || !strings.Contains(failures[0], "msg=fatal") || !strings.Contains(failures[0], want) {
		t.Errorf("logged %d error lines, want one fatal line carrying %q; the log:\n%s", len(failures), want, out)
	}
}

// A sweep stopped while it is still starting up, connecting to a database that
// does not answer, waiting for the migrations' lock, which a server starting
// from a new release holds while it migrates, checking a Cloud Storage bucket
// that does not answer, or reading a domain file that is slow to come, says why
// it ended: it listens for SIGTERM from before it connects, and every step of
// its startup ends with the stop, or, as the domain file's read does, is
// followed by a look at it, so it writes the stop as its error, and it exits
// with status 1 at once, or once the read ends, with nothing deleted. Before,
// SIGTERM killed it there without a word; with SIGTERM caught, a bucket check
// made on a context the stop does not reach would hold the stop until the
// process was killed; and with no look after the domain file, a stop while it
// was read let the sweep go on to prune. (A database that stops answering once
// the sweep is connected to it holds the error back until pgx gives up the
// connection, fifteen seconds later, which this does not wait for. The two
// answers migrate waits for whatever the stop are
// TestSweepStoppedAsMigrateFinishesEndsThere's.)
func TestSweepStoppedWhileStartingUpWritesItsError(t *testing.T) {
	// Nothing is left to wait for once the stop has ended the startup.
	const quickly = 3 * time.Second

	for _, tc := range []struct {
		name string
		// wait starts what the sweep then waits on, and returns its
		// DATABASE_URL and a func that reports whether it is waiting yet.
		wait func(t *testing.T, db *pgxpool.Pool, databaseURL string) (string, func() bool)
		want string
		// check, when there is one, looks at what the stopped sweep left.
		check func(t *testing.T, db *pgxpool.Pool)
	}{
		{"connecting", func(t *testing.T, _ *pgxpool.Pool, databaseURL string) (string, func() bool) {
			proxy := startStallingProxy(t, databaseURL)
			proxy.freeze()
			return throughProxy(t, databaseURL, proxy, ""), func() bool { return proxy.accepted.Load() > 0 }
		}, "ping postgres", nil},
		{"waiting for the migrations' lock", func(t *testing.T, db *pgxpool.Pool, databaseURL string) (string, func() bool) {
			ctx := context.Background()
			lock, err := db.Acquire(ctx)
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			t.Cleanup(lock.Release)
			// Every schema in the database shares it, so the app package's
			// tests, which may be running beside these, wait on it too: it
			// is held for this case alone.
			if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockID); err != nil {
				t.Fatalf("take the migrations' lock: %v", err)
			}
			t.Cleanup(func() {
				if _, err := lock.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrateLockID); err != nil {
					t.Errorf("give back the migrations' lock: %v", err)
				}
			})
			var lockPID int32
			if err := lock.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&lockPID); err != nil {
				t.Fatalf("find the lock's backend: %v", err)
			}
			appName := fmt.Sprintf("educhan_starting_sweep_%d", time.Now().UnixNano())
			t.Cleanup(func() {
				if _, err := db.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1`, appName); err != nil {
					t.Errorf("end the sweep's connections: %v", err)
				}
			})
			return withApplicationName(t, databaseURL, appName), func() bool {
				var waiting bool
				if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
					WHERE application_name = $1 AND $2 = ANY (pg_blocking_pids(pid)))`, appName, lockPID).Scan(&waiting); err != nil {
					t.Fatalf("look for the sweep's wait: %v", err)
				}
				return waiting
			}
		}, "acquire migration lock", nil},
		{"checking the media store's bucket", func(t *testing.T, _ *pgxpool.Pool, databaseURL string) (string, func() bool) {
			// Frozen from the start, it stands in for a Cloud Storage that
			// takes the bucket check's request and never answers it.
			storage := startStallingProxy(t, databaseURL)
			storage.freeze()
			t.Setenv("EDUCHAN_MEDIA_BACKEND", "gcs")
			t.Setenv("EDUCHAN_GCS_BUCKET", "educhan-test")
			t.Setenv("STORAGE_EMULATOR_HOST", "http://"+storage.addr)
			return databaseURL, func() bool { return storage.accepted.Load() > 0 }
		}, "prepare media store", nil},
		{"reading the domain file", func(t *testing.T, db *pgxpool.Pool, databaseURL string) (string, func() bool) {
			// Migrated first, with a spent answer two days past its day,
			// which a sweep that got as far as pruning would delete.
			if err := run([]string{"-sweep"}, io.Discard); err != nil {
				t.Fatalf("migrate the schema: %v", err)
			}
			if _, err := db.Exec(context.Background(), `INSERT INTO captcha_spent (signature_hash, expires_at)
				VALUES ('\x01', now() - interval '2 days')`); err != nil {
				t.Fatalf("spend an answer: %v", err)
			}
			list, err := os.ReadFile("../../internal/app/verify_domains.txt")
			if err != nil {
				t.Fatalf("read the domain list: %v", err)
			}
			// A FIFO stands in for a file on a mount that is slow to answer:
			// the sweep's read of it waits until something is written.
			fifo := filepath.Join(t.TempDir(), "verify_domains.txt")
			makeFIFO(t, fifo)
			t.Setenv("EDUCHAN_VERIFY_DOMAINS", fifo)
			return databaseURL, func() bool {
				// Opened without waiting, it opens only once the sweep has it
				// open to read.
				w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
				if err != nil {
					return false
				}
				// The stop is sent as this returns, and the list is written
				// after it, so the read ends with the stop already come.
				go func() {
					defer w.Close()
					time.Sleep(300 * time.Millisecond)
					_, _ = w.Write(list)
				}()
				return true
			}
		}, "load verification domains", func(t *testing.T, db *pgxpool.Pool) {
			var spent int
			if err := db.QueryRow(context.Background(), `SELECT count(*) FROM captcha_spent`).Scan(&spent); err != nil {
				t.Fatalf("count spent answers: %v", err)
			}
			if spent != 1 {
				t.Errorf("the sweep stopped while starting up left %d spent answers, want the one past its day kept", spent)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sweepEnv(t)
			sweepURL, waiting := tc.wait(t, db, os.Getenv("DATABASE_URL"))

			sweep := startSweep(t, sweepURL)
			started := false
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if started = waiting(); started {
					break
				}
			}
			if !started {
				sweep.kill()
				t.Fatalf("the sweep never waited; its log:\n%s", sweep.log(time.Now()))
			}

			stopped := time.Now()
			waitErr := sweep.stop(t)
			took := time.Since(stopped)
			text := sweep.log(stopped)

			var exit *exec.ExitError
			if !errors.As(waitErr, &exit) || exit.ExitCode() != 1 {
				t.Errorf("the sweep stopped while starting up ended with %v, want exit status 1; its log:\n%s", waitErr, text)
			}
			if took >= quickly {
				t.Errorf("the sweep stopped while starting up exited %v after SIGTERM, want under %v", took, quickly)
			}
			wantOneFatalLine(t, text, tc.want)
			if !strings.Contains(text, "context canceled") {
				t.Errorf("the sweep's error is not the stop; its log:\n%s", text)
			}
			if strings.Contains(text, "msg=sweep ") {
				t.Errorf("the sweep stopped while starting up swept; its log:\n%s", text)
			}
			if tc.check != nil {
				tc.check(t, db)
			}
			t.Logf("exited %v after SIGTERM; its log:\n%s", took.Round(10*time.Millisecond), text)
		})
	}
}

// migrate waits for two answers whatever the stop, both for two seconds at
// most after the stop (the app's migrateAnswerTimeout): a migration's COMMIT
// once it is sent, and the release of the migrations' lock. A sweep stopped
// while it waits on one still ends with the stop, written as its one fatal
// line, and deletes nothing: New looks at the stop once migrate is done.
// Before, an unlock that the database did not answer held the stop until the
// process was killed, a slow one let the sweep go on to prune, a commit cut
// short was written as a failure though the migration had been applied, and an
// unlock sent once a slow commit was answered had two seconds of its own, so
// the sweep exited nearly four seconds after the stop.
func TestSweepStoppedAsMigrateFinishesEndsThere(t *testing.T) {
	// The two seconds after the stop, and some to spare.
	const quickly = 3 * time.Second

	for _, tc := range []struct {
		name string
		// migrated has the schema migrated before the sweep starts, so the
		// sweep's last statement under the lock is the unlock.
		migrated  bool
		statement string
		hold      time.Duration
		// then, once statement has been sent, has the proxy freeze the first
		// connection to send this one.
		then string
		// check looks at what the stopped sweep left and wrote.
		check func(t *testing.T, db *pgxpool.Pool, appName, text string)
	}{
		{"giving the lock back, unanswered", true, "pg_advisory_unlock", 0, "", func(t *testing.T, db *pgxpool.Pool, appName, text string) {
			wantOneFatalLine(t, text, "migrate: context canceled")
			if !strings.Contains(text, "could not release migration lock") {
				t.Errorf("the unanswered unlock was not reported; its log:\n%s", text)
			}
			// Closed with its connection, the lock is gone with the session.
			held := true
			for deadline := time.Now().Add(5 * time.Second); held && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				held = locked(t, db, appName)
			}
			if held {
				t.Errorf("the migrations' lock was still held five seconds after the stopped sweep exited")
			}
		}},
		{"giving the lock back, slowly", true, "pg_advisory_unlock", 1500 * time.Millisecond, "", func(t *testing.T, db *pgxpool.Pool, _, text string) {
			wantOneFatalLine(t, text, "migrate: context canceled")
			if strings.Contains(text, "could not release migration lock") {
				t.Errorf("the slow unlock was reported as failed; its log:\n%s", text)
			}
			var spent int
			if err := db.QueryRow(context.Background(), `SELECT count(*) FROM captcha_spent`).Scan(&spent); err != nil {
				t.Fatalf("count spent answers: %v", err)
			}
			if spent != 1 {
				t.Errorf("the sweep stopped while starting up left %d spent answers, want the one past its day kept", spent)
			}
		}},
		// The first COMMIT of a fresh schema's migrations is 0001's.
		{"committing a migration", false, "Q\x00\x00\x00\x0bcommit\x00", time.Second, "", func(t *testing.T, db *pgxpool.Pool, _, text string) {
			wantOneFatalLine(t, text, "migrate: begin migration 0002_imageboard.sql: context canceled")
			if strings.Contains(text, "commit migration") {
				t.Errorf("the sweep reported the commit failed; its log:\n%s", text)
			}
			// Ended before the next migration's BEGIN, its connection is
			// still sound, and the lock is given back.
			if strings.Contains(text, "could not release migration lock") {
				t.Errorf("the lock's release failed after the commit; its log:\n%s", text)
			}
			applied := strings.Index(text, `msg="applied migration" version=1 `)
			if applied < 0 || applied > strings.Index(text, "msg=fatal") {
				t.Errorf("the committed migration was not logged as applied before the stop; its log:\n%s", text)
			}
			var versions []int
			rows, err := db.Query(context.Background(), `SELECT version FROM schema_migrations ORDER BY version`)
			if err != nil {
				t.Fatalf("read schema_migrations: %v", err)
			}
			versions, err = pgx.CollectRows(rows, pgx.RowTo[int])
			if err != nil {
				t.Fatalf("read schema_migrations: %v", err)
			}
			if fmt.Sprint(versions) != "[1]" {
				t.Errorf("schema_migrations holds %v, want [1]: the migration committing applied, and no other begun", versions)
			}
		}},
		// Frozen before it is forwarded, the COMMIT never reaches the
		// database: given up two seconds after the stop, it is reported as
		// failed, and its connection closed takes the lock with it.
		{"committing a migration, unanswered", false, "Q\x00\x00\x00\x0bcommit\x00", 0, "", func(t *testing.T, db *pgxpool.Pool, appName, text string) {
			wantOneFatalLine(t, text, "commit migration 0001_init.sql")
			if strings.Contains(text, "msg=\"applied migration\"") {
				t.Errorf("a migration whose commit went unanswered was logged as applied; its log:\n%s", text)
			}
			held := true
			for deadline := time.Now().Add(5 * time.Second); held && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				held = locked(t, db, appName)
			}
			if held {
				t.Errorf("the migrations' lock was still held five seconds after the stopped sweep exited")
			}
		}},
		// The stop comes as 0001 commits, and the database answers the
		// COMMIT 1.8 s later: the unlock, sent then and never answered, has
		// what is left of the two seconds after the stop, not two of its own.
		{"committing a migration slowly, then giving the lock back, unanswered", false, "Q\x00\x00\x00\x0bcommit\x00", 1800 * time.Millisecond, "pg_advisory_unlock", func(t *testing.T, db *pgxpool.Pool, appName, text string) {
			wantOneFatalLine(t, text, "migrate: begin migration 0002_imageboard.sql: context canceled")
			applied := strings.Index(text, `msg="applied migration" version=1 `)
			if applied < 0 || applied > strings.Index(text, "msg=fatal") {
				t.Errorf("the committed migration was not logged as applied before the stop; its log:\n%s", text)
			}
			if !strings.Contains(text, "could not release migration lock") {
				t.Errorf("the unanswered unlock was not reported; its log:\n%s", text)
			}
			held := true
			for deadline := time.Now().Add(5 * time.Second); held && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				held = locked(t, db, appName)
			}
			if held {
				t.Errorf("the migrations' lock was still held five seconds after the stopped sweep exited")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sweepEnv(t)
			ctx := context.Background()
			if tc.migrated {
				if err := run([]string{"-sweep"}, io.Discard); err != nil {
					t.Fatalf("migrate the schema: %v", err)
				}
				// Two days past its day: a sweep that got as far as pruning
				// would delete it.
				if _, err := db.Exec(ctx, `INSERT INTO captcha_spent (signature_hash, expires_at)
					VALUES ('\x01', now() - interval '2 days')`); err != nil {
					t.Fatalf("spend an answer: %v", err)
				}
			}
			databaseURL := os.Getenv("DATABASE_URL")
			proxy := startStallingProxy(t, databaseURL)
			fired := proxy.on(tc.statement, tc.hold)
			appName := fmt.Sprintf("educhan_migrating_sweep_%d", time.Now().UnixNano())
			t.Cleanup(func() {
				if _, err := db.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1`, appName); err != nil {
					t.Errorf("end the sweep's connections: %v", err)
				}
			})

			sweep := startSweep(t, throughProxy(t, databaseURL, proxy, appName))
			select {
			case <-fired:
			case <-time.After(30 * time.Second):
				sweep.kill()
				t.Fatalf("the sweep never sent %q; its log:\n%s", tc.statement, sweep.log(time.Now()))
			}
			var frozen <-chan struct{}
			if tc.then != "" {
				frozen = proxy.on(tc.then, 0)
			}
			// The lock is held while its release waits, frozen or held: the
			// check above can see it.
			if tc.statement == "pg_advisory_unlock" && tc.hold == 0 && !locked(t, db, appName) {
				t.Errorf("the migrations' lock is not held while its release waits")
			}

			stopped := time.Now()
			waitErr := sweep.stop(t)
			took := time.Since(stopped)
			text := sweep.log(stopped)

			var exit *exec.ExitError
			if !errors.As(waitErr, &exit) || exit.ExitCode() != 1 {
				t.Errorf("the sweep stopped as migrate finished ended with %v, want exit status 1; its log:\n%s", waitErr, text)
			}
			if took >= quickly {
				t.Errorf("the sweep stopped as migrate finished exited %v after SIGTERM, want under %v", took, quickly)
			}
			if strings.Contains(text, "msg=sweep ") {
				t.Errorf("the sweep stopped as migrate finished swept; its log:\n%s", text)
			}
			if frozen != nil {
				select {
				case <-frozen:
				default:
					t.Errorf("the sweep never sent %q after %q; its log:\n%s", tc.then, tc.statement, text)
				}
			}
			tc.check(t, db, appName, text)
			t.Logf("exited %v after SIGTERM; its log:\n%s", took.Round(10*time.Millisecond), text)
		})
	}
}

// A migration's COMMIT is waited on as long as the database takes until a
// stop comes: the two seconds a stop allows it are counted from the stop. A
// sweep whose database answers a commit slowly, with no stop, migrates and
// sweeps. Before, the two seconds ran from the commit, so a commit answered
// later failed the sweep, written as a failed migration though it was applied,
// and the server's startup the same way.
func TestASlowCommitIsWaitedOnWithoutAStop(t *testing.T) {
	db := sweepEnv(t)
	databaseURL := os.Getenv("DATABASE_URL")
	proxy := startStallingProxy(t, databaseURL)
	// Longer than the two seconds a stop allows: 0001's COMMIT, the first.
	fired := proxy.on("Q\x00\x00\x00\x0bcommit\x00", 3*time.Second)

	started := time.Now()
	sweep := startSweep(t, throughProxy(t, databaseURL, proxy, ""))
	var err error
	select {
	case err = <-sweep.exited:
	case <-time.After(60 * time.Second):
		sweep.kill()
		t.Fatalf("the sweep had not exited a minute after it started; its log:\n%s", sweep.log(started))
	}
	text := sweep.log(started)
	select {
	case <-fired:
	default:
		t.Fatalf("the sweep never sent its first COMMIT; its log:\n%s", text)
	}
	if err != nil {
		t.Errorf("the sweep whose commit was answered slowly ended with %v, want exit status 0; its log:\n%s", err, text)
	}
	if strings.Contains(text, "msg=fatal") || !strings.Contains(text, `msg="applied migration" version=1 `) || !strings.Contains(text, "msg=sweep ") {
		t.Errorf("the sweep whose commit was answered slowly did not migrate and sweep; its log:\n%s", text)
	}
	var applied int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if applied == 0 {
		t.Errorf("no migration recorded after the sweep whose commit was answered slowly")
	}
}

// The release of the migrations' lock is waited on for two seconds at most
// from when it is sent, and no later than two seconds after a stop. A sweep
// whose database never answers the unlock, with no stop, gives it up two
// seconds after sending it, closes its connection, which takes the lock with
// it, and sweeps. Bounded only from the stop, the unlock would be waited on
// until two seconds after startup's thirty-second bound, and the sweep would
// fail then, as a server's startup would, 32 seconds after the unlock was sent.
func TestAnUnansweredUnlockIsGivenUpWithoutAStop(t *testing.T) {
	// The two seconds from the unlock, and some to spare.
	const quickly = 3 * time.Second
	// Well inside startup's thirty seconds, which the unlock would wait out
	// were it bounded only from a stop.
	const wellInside = 10 * time.Second

	db := sweepEnv(t)
	// Migrated here, so the sweep's last statement under the lock is the
	// unlock.
	if err := run([]string{"-sweep"}, io.Discard); err != nil {
		t.Fatalf("migrate the schema: %v", err)
	}
	databaseURL := os.Getenv("DATABASE_URL")
	proxy := startStallingProxy(t, databaseURL)
	fired := proxy.on("pg_advisory_unlock", 0)
	appName := fmt.Sprintf("educhan_unlocking_sweep_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if _, err := db.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1`, appName); err != nil {
			t.Errorf("end the sweep's connections: %v", err)
		}
	})

	sweep := startSweep(t, throughProxy(t, databaseURL, proxy, appName))
	var sent time.Time
	select {
	case <-fired:
		sent = time.Now()
	case <-time.After(30 * time.Second):
		sweep.kill()
		t.Fatalf("the sweep never sent the unlock; its log:\n%s", sweep.log(time.Now()))
	}
	if !locked(t, db, appName) {
		t.Errorf("the migrations' lock is not held while its release waits")
	}
	var err error
	select {
	case err = <-sweep.exited:
	case <-time.After(60 * time.Second):
		sweep.kill()
		t.Fatalf("the sweep had not exited a minute after it sent the unlock; its log:\n%s", sweep.log(sent))
	}
	took := time.Since(sent)
	text := sweep.log(sent)

	if err != nil {
		t.Errorf("the sweep whose unlock went unanswered ended with %v, want exit status 0; its log:\n%s", err, text)
	}
	if took >= wellInside {
		t.Errorf("the sweep whose unlock went unanswered exited %v after sending it, want under %v", took, wellInside)
	}
	// Timed from when the proxy saw the unlock, not from the exit, which
	// comes after the sweep.
	var warned *logLine
	for i, l := range sweep.logged {
		if strings.Contains(l.text, "could not release migration lock") {
			warned = &sweep.logged[i]
		}
	}
	if warned == nil {
		t.Errorf("the unanswered unlock was not reported; its log:\n%s", text)
	} else if after := warned.at.Sub(sent); after < 1500*time.Millisecond || after >= quickly {
		t.Errorf("the unanswered unlock was given up %v after it was sent, want two seconds; its log:\n%s", after, text)
	}
	if strings.Contains(text, "msg=fatal") || !strings.Contains(text, "msg=sweep ") {
		t.Errorf("the sweep whose unlock went unanswered did not sweep; its log:\n%s", text)
	}
	held := true
	for deadline := time.Now().Add(5 * time.Second); held && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		held = locked(t, db, appName)
	}
	if held {
		t.Errorf("the migrations' lock was still held five seconds after the sweep exited")
	}
	t.Logf("exited %v after the unlock was sent; its log:\n%s", took.Round(10*time.Millisecond), text)
}

// locked says whether a connection named appName holds an advisory lock.
func locked(t *testing.T, db *pgxpool.Pool, appName string) bool {
	t.Helper()
	var held bool
	if err := db.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_stat_activity a USING (pid)
		WHERE l.locktype = 'advisory' AND a.application_name = $1)`, appName).Scan(&held); err != nil {
		t.Fatalf("look for the migrations' lock: %v", err)
	}
	return held
}

// migrateLockID is the app package's own, the advisory lock its migrations
// run under, which TestSweepStoppedWhileStartingUpWritesItsError holds: were
// the two to differ, the sweep would never wait on the test's.
const migrateLockID = 8125437190021

// throughProxy is databaseURL with its host the proxy's, and with
// application_name set to appName unless that is empty.
func throughProxy(t *testing.T, databaseURL string, proxy *stallingProxy, appName string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	u.Host = proxy.addr
	query := u.Query()
	query.Del("host")
	if appName != "" {
		query.Set("application_name", appName)
	}
	u.RawQuery = query.Encode()
	return u.String()
}

// withApplicationName is databaseURL with application_name set, to find its
// connections in pg_stat_activity.
func withApplicationName(t *testing.T, databaseURL, appName string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	query := u.Query()
	query.Set("application_name", appName)
	u.RawQuery = query.Encode()
	return u.String()
}

// sweepProcess is this test binary run as a whole `educhan -sweep` process,
// its signals, its log and its exit status included, with each line it
// writes kept with the time it came.
type sweepProcess struct {
	cmd *exec.Cmd
	// logged is read only once exited has said how the process ended.
	logged []logLine
	exited chan error
}

type logLine struct {
	text string
	at   time.Time
}

func startSweep(t *testing.T, databaseURL string) *sweepProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), mainArgsEnv+"=-sweep", "DATABASE_URL="+databaseURL)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the sweep: %v", err)
	}
	p := &sweepProcess{cmd: cmd, exited: make(chan error, 1)}
	lines := make(chan logLine, 100)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			lines <- logLine{scanner.Text(), time.Now()}
		}
	}()
	go func() {
		for l := range lines {
			p.logged = append(p.logged, l)
		}
		p.exited <- cmd.Wait()
	}()
	return p
}

// stop sends the process SIGTERM and returns how it ended, failing t if it
// has not exited 30 seconds later.
func (p *sweepProcess) stop(t *testing.T) error {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("stop the sweep: %v", err)
	}
	select {
	case err := <-p.exited:
		return err
	case <-time.After(30 * time.Second):
		p.kill()
		t.Fatalf("the stopped sweep had not exited 30 seconds after SIGTERM; its log:\n%s", p.log(time.Now()))
		return nil
	}
}

// kill ends the process, for a test that is giving up on it.
func (p *sweepProcess) kill() {
	_ = p.cmd.Process.Kill()
	<-p.exited
}

// log is what the process wrote, each line marked with when it came relative
// to since; it is read only once the process has ended.
func (p *sweepProcess) log(since time.Time) string {
	text := make([]string, len(p.logged))
	for i, l := range p.logged {
		text[i] = fmt.Sprintf("+%v %s", l.at.Sub(since).Round(10*time.Millisecond), l.text)
	}
	return strings.Join(text, "\n")
}

// A Cloud Run Job stopped at its task timeout is killed ten seconds after its
// SIGTERM. A sweep stopped while its database has stopped answering, behind a
// network partition say, writes its error and exits before then: the stop
// fails the query on its way, the spent answers' prune gives up at its five
// seconds, the error is written, and the close, which pgx gives fifteen
// seconds for a connection that cannot answer, is waited on for
// sweepCloseTimeout at most.
func TestStoppedSweepExitsWhenTheDatabaseStopsAnswering(t *testing.T) {
	// The app's spentPruneTimeout, which TestSweepsPruneIsGivenFiveSeconds
	// pins, and what Cloud Run waits between SIGTERM and SIGKILL.
	const pruneBound = 5 * time.Second
	const cloudRunGrace = 10 * time.Second

	db := sweepEnv(t)
	ctx := context.Background()
	// Migrated here, so the process below has nothing to change before it
	// sweeps.
	if err := run([]string{"-sweep"}, io.Discard); err != nil {
		t.Fatalf("migrate the schema: %v", err)
	}

	databaseURL := os.Getenv("DATABASE_URL")
	proxy := startStallingProxy(t, databaseURL)
	// Named, to find the process's connection in pg_stat_activity.
	appName := fmt.Sprintf("educhan_stalled_sweep_%d", time.Now().UnixNano())
	sweepURL := throughProxy(t, databaseURL, proxy, appName)
	t.Cleanup(func() {
		if _, err := db.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1`, appName); err != nil {
			t.Errorf("end the stalled sweep's connections: %v", err)
		}
	})

	// The sweep's first query waits on this lock, so the stop finds it on its
	// way.
	lock, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = lock.Rollback(context.Background()) })
	if _, err := lock.Exec(ctx, `LOCK TABLE threads IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock threads: %v", err)
	}
	var lockPID int32
	if err := lock.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&lockPID); err != nil {
		t.Fatalf("find the lock's backend: %v", err)
	}

	sweep := startSweep(t, sweepURL)

	// Waiting on this lock, not on any: before it sweeps, the process waits
	// for the migrations' advisory lock, which every schema in the database
	// shares and the app package's tests take as they run, and a SIGTERM then
	// would stop it while it is still starting up, before it sweeps
	// (TestSweepStoppedWhileStartingUpWritesItsError).
	waiting := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE application_name = $1 AND $2 = ANY (pg_blocking_pids(pid)))`, appName, lockPID).Scan(&waiting); err != nil {
			t.Fatalf("look for the sweep's query: %v", err)
		}
		if waiting {
			break
		}
	}
	if !waiting {
		sweep.kill()
		t.Fatalf("the sweep's query never waited on the lock; its log:\n%s", sweep.log(time.Now()))
	}

	proxy.freeze()
	stopped := time.Now()
	waitErr := sweep.stop(t)
	took := time.Since(stopped)

	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.ExitCode() != 1 {
		t.Errorf("the stopped sweep ended with %v, want exit status 1", waitErr)
	}
	if took >= cloudRunGrace {
		t.Errorf("the stopped sweep exited %v after SIGTERM, after Cloud Run's %v", took, cloudRunGrace)
	}
	var record, failure *logLine
	failures := 0
	for i, l := range sweep.logged {
		switch {
		case strings.Contains(l.text, "msg=sweep "):
			record = &sweep.logged[i]
		case strings.Contains(l.text, "level=ERROR"):
			failures++
			failure = &sweep.logged[i]
		}
	}
	text := sweep.log(stopped)
	if record == nil {
		t.Errorf("the stopped sweep logged no record of counts; its log:\n%s", text)
	}
	if failures != 1 || !strings.Contains(failure.text, "msg=fatal") || !strings.Contains(failure.text, "context canceled") {
		t.Fatalf("the stopped sweep logged %d error lines, want one fatal line with the stop; its log:\n%s", failures, text)
	}
	// Written before the close is waited on, not after.
	if wrote := failure.at.Sub(stopped); wrote >= pruneBound+time.Second {
		t.Errorf("the stopped sweep wrote its error %v after SIGTERM, want it once the prune gives up at %v; its log:\n%s",
			wrote, pruneBound, text)
	}
	if record != nil && record.at.After(failure.at) {
		t.Errorf("the stopped sweep wrote its error before its record of counts; its log:\n%s", text)
	}
	t.Logf("exited %v after SIGTERM; its log:\n%s", took.Round(10*time.Millisecond), text)
}

// stallingProxy forwards connections to the database until it is frozen, and
// from then on forwards nothing and answers nothing, holding every connection
// open: a database that has stopped answering, as behind a network partition,
// rather than one that has gone, whose refusals would come at once. Told to
// watch for a statement (on), it can instead stall the one connection that
// sends it.
type stallingProxy struct {
	addr   string
	frozen atomic.Bool
	// accepted counts the connections it has taken, frozen or not.
	accepted atomic.Int32
	done     chan struct{}
	mu       sync.Mutex
	conns    []net.Conn
	watched  atomic.Pointer[watch]
}

// watch is a statement stallingProxy watches for in what its clients send, and
// what it does to the first connection that sends it: freeze that connection
// alone, or, with a hold, hold back the database's answers to it.
type watch struct {
	statement []byte
	hold      time.Duration
	once      sync.Once
	fired     chan struct{}
}

// link is one connection through the proxy, frozen alone or with the
// database's answers held back until heldUntil (Unix nanoseconds).
type link struct {
	frozen    atomic.Bool
	heldUntil atomic.Int64
}

// on has the proxy watch for statement in what its clients send. With hold
// zero the first connection to send it is frozen alone, before it is
// forwarded: that connection forwards nothing either way from then on, though
// its client's close still reaches the database, as for a connection whose
// packets are lost while the database and the others are fine. Otherwise the
// statement goes through, and the database's answers to that connection are
// held back for hold. The channel is closed once it has.
func (p *stallingProxy) on(statement string, hold time.Duration) <-chan struct{} {
	w := &watch{statement: []byte(statement), hold: hold, fired: make(chan struct{})}
	p.watched.Store(w)
	return w.fired
}

func startStallingProxy(t *testing.T, databaseURL string) *stallingProxy {
	t.Helper()
	cfg, err := pgconn.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	network, upstream := "tcp", net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
	if strings.HasPrefix(cfg.Host, "/") {
		network, upstream = "unix", filepath.Join(cfg.Host, fmt.Sprintf(".s.PGSQL.%d", cfg.Port))
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &stallingProxy{addr: ln.Addr().String(), done: make(chan struct{})}
	t.Cleanup(func() {
		close(p.done)
		ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conns {
			c.Close()
		}
	})
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			p.hold(client)
			p.accepted.Add(1)
			if p.frozen.Load() {
				continue
			}
			server, err := net.Dial(network, upstream)
			if err != nil {
				client.Close()
				continue
			}
			p.hold(server)
			l := &link{}
			go p.pump(server, client, l, true)
			go p.pump(client, server, l, false)
		}
	}()
	return p
}

func (p *stallingProxy) hold(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns = append(p.conns, c)
}

func (p *stallingProxy) freeze() { p.frozen.Store(true) }

// pump copies src to dst until either ends, or until the proxy is frozen: then
// what it reads goes nowhere, and both stay open until the test ends. On a
// link frozen alone, what it reads goes nowhere either, but an end is passed
// on. fromClient says src is the client's end, whose statements are watched.
func (p *stallingProxy) pump(dst, src net.Conn, l *link, fromClient bool) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if p.frozen.Load() {
			<-p.done
			return
		}
		if n > 0 && fromClient {
			p.watchFor(l, buf[:n])
		}
		if n > 0 && !l.frozen.Load() {
			if !fromClient {
				time.Sleep(time.Until(time.Unix(0, l.heldUntil.Load())))
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			dst.Close()
			return
		}
	}
}

// watchFor acts on l if what its client sent holds the statement watched for,
// the first time any client sends it.
func (p *stallingProxy) watchFor(l *link, sent []byte) {
	w := p.watched.Load()
	if w == nil || !bytes.Contains(sent, w.statement) {
		return
	}
	w.once.Do(func() {
		if w.hold == 0 {
			l.frozen.Store(true)
		} else {
			l.heldUntil.Store(time.Now().Add(w.hold).UnixNano())
		}
		close(w.fired)
	})
}
