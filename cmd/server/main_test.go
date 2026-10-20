package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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
// scheduler reports every run as a success.
func TestSweepModeFailsWithoutADatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://educhan:educhan@127.0.0.1:1/educhan?sslmode=disable&connect_timeout=2")
	t.Setenv("EDUCHAN_MEDIA_BACKEND", "local")
	t.Setenv("EDUCHAN_UPLOAD_DIR", t.TempDir())
	t.Setenv("EDUCHAN_VERIFY_SECRET", "")

	if err := run([]string{"-sweep"}, io.Discard); err == nil {
		t.Error("a sweep with no database reported success")
	}
}

// A mistyped flag must not fall through to serving: a Job that started a
// server instead of sweeping would never finish.
func TestRunRefusesWhatItDoesNotUnderstand(t *testing.T) {
	for _, args := range [][]string{{"sweep"}, {"-sweep", "now"}, {"-swept"}} {
		if err := run(args, io.Discard); err == nil {
			t.Errorf("run(%q) succeeded, want an error", args)
		}
	}
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
	sweepURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	sweepURL.Host = proxy.addr
	query := sweepURL.Query()
	query.Del("host")
	query.Set("application_name", appName)
	sweepURL.RawQuery = query.Encode()
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

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), mainArgsEnv+"=-sweep", "DATABASE_URL="+sweepURL.String())
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the sweep: %v", err)
	}
	type line struct {
		text string
		at   time.Time
	}
	lines := make(chan line, 100)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			lines <- line{scanner.Text(), time.Now()}
		}
	}()
	var logged []line
	exited := make(chan error, 1)
	go func() {
		for l := range lines {
			logged = append(logged, l)
		}
		exited <- cmd.Wait()
	}()

	waiting := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE application_name = $1 AND wait_event_type = 'Lock')`, appName).Scan(&waiting); err != nil {
			t.Fatalf("look for the sweep's query: %v", err)
		}
		if waiting {
			break
		}
	}
	if !waiting {
		_ = cmd.Process.Kill()
		<-exited
		t.Fatalf("the sweep's query never waited on the lock; its log:\n%v", logged)
	}

	proxy.freeze()
	stopped := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("stop the sweep: %v", err)
	}
	var waitErr error
	select {
	case waitErr = <-exited:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		t.Fatalf("the stopped sweep had not exited 30 seconds after SIGTERM")
	}
	took := time.Since(stopped)

	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.ExitCode() != 1 {
		t.Errorf("the stopped sweep ended with %v, want exit status 1", waitErr)
	}
	if took >= cloudRunGrace {
		t.Errorf("the stopped sweep exited %v after SIGTERM, after Cloud Run's %v", took, cloudRunGrace)
	}
	var record, failure *line
	failures := 0
	for i, l := range logged {
		switch {
		case strings.Contains(l.text, "msg=sweep "):
			record = &logged[i]
		case strings.Contains(l.text, "level=ERROR"):
			failures++
			failure = &logged[i]
		}
	}
	text := make([]string, len(logged))
	for i, l := range logged {
		text[i] = fmt.Sprintf("+%v %s", l.at.Sub(stopped).Round(10*time.Millisecond), l.text)
	}
	if record == nil {
		t.Errorf("the stopped sweep logged no record of counts; its log:\n%s", strings.Join(text, "\n"))
	}
	if failures != 1 || !strings.Contains(failure.text, "msg=fatal") || !strings.Contains(failure.text, "context canceled") {
		t.Fatalf("the stopped sweep logged %d error lines, want one fatal line with the stop; its log:\n%s",
			failures, strings.Join(text, "\n"))
	}
	// Written before the close is waited on, not after.
	if wrote := failure.at.Sub(stopped); wrote >= pruneBound+time.Second {
		t.Errorf("the stopped sweep wrote its error %v after SIGTERM, want it once the prune gives up at %v; its log:\n%s",
			wrote, pruneBound, strings.Join(text, "\n"))
	}
	if record != nil && record.at.After(failure.at) {
		t.Errorf("the stopped sweep wrote its error before its record of counts; its log:\n%s", strings.Join(text, "\n"))
	}
	t.Logf("exited %v after SIGTERM; its log:\n%s", took.Round(10*time.Millisecond), strings.Join(text, "\n"))
}

// stallingProxy forwards connections to the database until it is frozen, and
// from then on forwards nothing and answers nothing, holding every connection
// open: a database that has stopped answering, as behind a network partition,
// rather than one that has gone, whose refusals would come at once.
type stallingProxy struct {
	addr   string
	frozen atomic.Bool
	done   chan struct{}
	mu     sync.Mutex
	conns  []net.Conn
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
			if p.frozen.Load() {
				continue
			}
			server, err := net.Dial(network, upstream)
			if err != nil {
				client.Close()
				continue
			}
			p.hold(server)
			go p.pump(server, client)
			go p.pump(client, server)
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
// what it reads goes nowhere, and both stay open until the test ends.
func (p *stallingProxy) pump(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if p.frozen.Load() {
			<-p.done
			return
		}
		if n > 0 {
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
