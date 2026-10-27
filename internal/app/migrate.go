package app

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrateLockID namespaces the advisory lock held while migrations run, so
// concurrently starting instances apply them one at a time.
const migrateLockID = 8125437190021

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads the embedded migrations, ordered by version. File names
// must look like "0001_init.sql".
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}

	migrations := make([]migration, 0, len(entries))
	seen := map[int]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(strings.TrimSuffix(name, ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: want <version>_<name>.sql", name)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version: %w", name, err)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("migration %q duplicates version of %q", name, other)
		}
		seen[version] = name

		body, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, migration{version: version, name: name, sql: string(body)})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	return migrations, nil
}

// migrate applies every migration that has not been recorded in
// schema_migrations yet. Each migration runs in its own transaction, so a
// failure leaves the database on the last successfully applied version.
// Then, still under the lock and on every start, it brings rows an older
// release may have written since up to the latest rules: boards under a slug
// now reserved (renameReservedBoards) and attachments named by the poster's
// file (renameAttachments).
//
// It returns the versions the database has that this binary does not know: a
// database a newer release migrated, which this one is serving after a
// rollback. There are no down migrations, and code older than the schema reads
// it without the newer rules — a phase-0 binary on an archive-era schema shows
// archived threads as live and deletes them on its first prune — so that is
// logged as an error, and the sweeper refuses to run at all.
func migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) ([]int, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	return applyMigrations(ctx, pool, log, migrations)
}

// applyMigrations is migrate over a given list, which lets a test stop a
// database at an earlier version, put data in front of a later migration, and
// then let migrate bring it up to date the way a deploy would.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, migrations []migration) ([]int, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    version    INTEGER PRIMARY KEY,
		    name       TEXT NOT NULL,
		    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockID); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLockID); err != nil {
			log.Warn("could not release migration lock", "error", err)
		}
	}()

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}

	known := map[int]bool{}
	for _, m := range migrations {
		known[m.version] = true
		if applied[m.version] {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("record migration %s: %w", m.name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit migration %s: %w", m.name, err)
		}
		log.Info("applied migration", "version", m.version, "name", m.name)
	}

	var ahead []int
	for v := range applied {
		if !known[v] {
			ahead = append(ahead, v)
		}
	}
	sort.Ints(ahead)
	if len(ahead) > 0 {
		log.Error("the database was migrated by a newer release than this one; this binary does not "+
			"know its newest rules, so it may show or delete data it should not (a rollback across "+
			"0005 shows archived threads as live and deletes them)",
			"unknown_versions", ahead)
	}

	if err := renameReservedBoards(ctx, conn, log); err != nil {
		return nil, err
	}
	if known[attachmentNamesVersion] {
		if err := renameAttachments(ctx, conn, log); err != nil {
			return nil, err
		}
	}
	return ahead, nil
}

// attachmentNamesVersion is 0008_attachment_names.sql, the migration
// renameAttachments repeats.
const attachmentNamesVersion = 8

// renameAttachments gives every attachment the name this release shows it
// under, "image" and the extension of its stored file (attachmentName), as
// 0008 did for the rows before it. 0008 ran once, but a release from before
// it can still be serving beside this one while a deploy rolls out, and it
// writes the poster's filename into original_name for every image it stores;
// so this runs under the migration lock, after the migrations, on every
// start, and the next start of this release renames what such an instance
// stored. Once the names are right it changes nothing, at the cost of a scan
// of the attachments table. It logs how many it renamed and never a name.
func renameAttachments(ctx context.Context, conn *pgxpool.Conn, log *slog.Logger) error {
	tag, err := conn.Exec(ctx, `
		UPDATE attachments
		SET original_name = 'image' || COALESCE(substring(original_path FROM '\.[^./]*$'), '')
		WHERE original_name IS DISTINCT FROM
		      'image' || COALESCE(substring(original_path FROM '\.[^./]*$'), '')`)
	if err != nil {
		return fmt.Errorf("rename attachments: %w", err)
	}
	if n := tag.RowsAffected(); n > 0 {
		log.Warn("renamed attachments an older release stored under their poster's filename", "attachments", n)
	}
	return nil
}

// renameReservedBoards moves any board whose slug has since become a reserved
// word — a page of the site, like /archive/ or /rules/ — to "<slug>-board"
// ("board-u" for "u", whose "u-board" would be an institution's address).
// The UI reads such a path as the page, so the board would stay in the board
// list with every link to it landing on something else. Board creation
// refuses reserved slugs, so only a board made before its word was reserved
// is ever renamed, and it keeps its row, its threads and its post numbers.
// It runs under the migration lock, after the migrations, on every start, so
// it follows reservedBoardSlugs as the list grows.
func renameReservedBoards(ctx context.Context, conn *pgxpool.Conn, log *slog.Logger) error {
	reserved := make([]string, 0, len(reservedBoardSlugs))
	for slug := range reservedBoardSlugs {
		reserved = append(reserved, slug)
	}
	rows, err := conn.Query(ctx, `SELECT slug FROM boards WHERE slug = ANY($1) ORDER BY slug`, reserved)
	if err != nil {
		return fmt.Errorf("find boards under reserved slugs: %w", err)
	}
	clashing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("find boards under reserved slugs: %w", err)
	}
	for _, slug := range clashing {
		base := slug + "-board"
		if strings.HasPrefix(base, "u-") {
			// "u-board" would be the institution board shown at /u/board/.
			base = "board-" + slug
		}
		for n := 1; ; n++ {
			if n > 100 {
				return fmt.Errorf("rename /%s/: no free slug to rename it to", slug)
			}
			renamed := base
			if n > 1 {
				renamed += "-" + strconv.Itoa(n)
			}
			tag, err := conn.Exec(ctx, `
				UPDATE boards SET slug = $2
				WHERE slug = $1 AND NOT EXISTS (SELECT 1 FROM boards WHERE slug = $2)`, slug, renamed)
			if err != nil {
				return fmt.Errorf("rename /%s/: %w", slug, err)
			}
			if tag.RowsAffected() == 1 {
				log.Warn("renamed a board whose address is now a page of the site", "from", slug, "to", renamed)
				break
			}
		}
	}
	return nil
}

func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[int]bool, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}
