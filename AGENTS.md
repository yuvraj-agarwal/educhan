# Agent guide — educhan

educhan is an anonymous imageboard for tertiary education. This repository contains the
working prototype: a Go HTTP API plus an embedded web UI, backed by PostgreSQL. Boards,
anonymous posting (tripcodes, per-thread poster IDs), image attachments, a paginated index,
a catalog view, threads with quote links, and key-authenticated moderation endpoints are
implemented. Verification badges, janitor accounts, bans, and reports are not.

## Stack & layout

- **Go 1.22** (standard library HTTP, `log/slog`), **PostgreSQL 16**, `pgx`/`pgxpool`,
  `golang.org/x/image` (WebP decoding and thumbnail scaling).
- `cmd/server` — entrypoint (config, logger, graceful shutdown).
- `internal/app`
  - `app.go` — wiring and routes; `config.go` — environment configuration.
  - `handlers.go` — HTTP handlers, validation, JSON responses.
  - `attachments.go` — upload parsing, media serving.
  - `moderation.go` — moderation key check and the sticky/lock/delete handlers.
  - `store.go` — all SQL; `models.go` — the JSON-facing types.
  - `identity.go` — anonymous names, tripcodes, poster IDs; `text.go` — post text rules.
  - `middleware.go` — logging, panic recovery, security headers, client IP.
  - `ratelimit.go` — per-client flood control.
  - `migrate.go` + `migrations/*.sql` — versioned migrations.
  - `web/` — the embedded single-page UI (`index.html`, `app.css`, `app.js`).
- `internal/media` — image validation, thumbnailing, and the blob-storage interface with
  its local-filesystem implementation.
- `.cursor/` — Cloud Agent environment (`environment.json` + `scripts/`).

## Canonical commands

- Build: `go build ./...`
- Test: `go test ./...` (the integration tests need PostgreSQL; they skip without it)
- Vet / format: `go vet ./...` and `gofmt -l .` (must be empty), or `make check`
- Bring up Postgres: `bash .cursor/scripts/start.sh` (or `make db-up`)
- Run the server: `make run` (expects Postgres up), or the `server` terminal

## Cursor Cloud specific instructions

The environment is repository-managed via `.cursor/environment.json`:

- `install` runs `.cursor/scripts/install.sh` (installs PostgreSQL if missing; the base
  image already ships Go; then `go mod download` + `go build`).
- `start` runs `.cursor/scripts/start.sh` (starts a user-owned Postgres cluster on
  `localhost:5432` and ensures the `educhan` role/database). It is idempotent and clears a
  stale `postmaster.pid` left by a snapshot taken while Postgres was running.
- A `server` terminal runs `.cursor/scripts/run-server.sh`, which waits for Postgres and
  then serves on port **8080**.

### Starting the app manually

If services are not already running (for example in a plain shell session), start them
yourself — do not assume they are up:

```bash
bash .cursor/scripts/start.sh # Postgres on :5432
nohup bash .cursor/scripts/run-server.sh > /tmp/educhan-server.log 2>&1 & # server on :8080
```

Add the Postgres client binaries to `PATH` when using `psql`/`pg_isready` directly:

```bash
export PATH="$(ls -d /usr/lib/postgresql/*/bin):$PATH"
```

### Testing the app end to end

Request bodies must carry `Content-Type: application/json`, and unknown JSON fields are
rejected, so pass the header explicitly.

```bash
# Health + seeded boards
curl -s localhost:8080/healthz    # -> {"status":"ok"}
curl -s localhost:8080/api/boards # -> placements / research / general

# Create a thread, reply to it with a quote link, and read it back
TNO=$(curl -s -X POST localhost:8080/api/boards/general/threads \
  -H 'Content-Type: application/json' \
  -d '{"subject":"smoke test","name":"ana#trip","body":"hello"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["no"])')
curl -s -X POST localhost:8080/api/boards/general/threads/$TNO/posts \
  -H 'Content-Type: application/json' -d "{\"body\":\">>$TNO\nreply\"}"
curl -s localhost:8080/api/boards/general/threads/$TNO   # both posts, with backlinks
curl -s "localhost:8080/api/boards/general/threads?page=1" # index page
curl -s localhost:8080/api/boards/general/catalog          # catalog tiles

# Confirm persistence in PostgreSQL
psql -h localhost -p 5432 -U educhan -d educhan \
  -c "SELECT b.slug, p.no, p.name, p.poster_id FROM posts p JOIN boards b ON b.id = p.board_id ORDER BY p.id;"
```

Attachments use `multipart/form-data` on the same two endpoints; `image` is the file
field, and everything else stays the same:

```bash
# Thread with an image, then read back the attachment metadata
TNO=$(curl -s -X POST localhost:8080/api/boards/general/threads \
  -F subject="with image" -F body="look at this" -F image=@/path/to/photo.png \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["no"])')
curl -s localhost:8080/api/boards/general/threads/$TNO # posts[].attachment.{url,thumb_url,…}

# Reply with an image; the body may be empty when an image is attached
curl -s -X POST localhost:8080/api/boards/general/threads/$TNO/posts -F image=@/path/to/photo.jpg
```

Moderation needs `EDUCHAN_MOD_KEY` set on the server; without it every moderation
endpoint answers `403`. Restart the server with the key exported before testing it:

```bash
EDUCHAN_MOD_KEY=dev-mod-key make run
```

```bash
curl -s -X POST localhost:8080/api/boards/general/threads/$TNO/sticky \
  -H 'Authorization: Bearer dev-mod-key'   # also /unsticky, /lock, /unlock
curl -s -X DELETE localhost:8080/api/boards/general/threads/$TNO/posts/$PNO \
  -H 'Authorization: Bearer dev-mod-key'   # one post plus its attachment files
curl -s -X DELETE localhost:8080/api/boards/general/threads/$TNO \
  -H 'Authorization: Bearer dev-mod-key'   # the thread, its posts, its attachments
```

Flood control is on by default (`EDUCHAN_THREAD_COOLDOWN=15`,
`EDUCHAN_POST_COOLDOWN=3` seconds), so scripted loops will see `429` with a
`Retry-After` header. Set both to `0` when a test needs to post rapidly.

For UI changes, the app is served at `http://localhost:8080` (dark-themed single page:
board index at `/general/`, catalog at `/general/catalog`, thread at
`/general/thread/{no}`). Prefer a browser walkthrough for anything that affects
`internal/app/web/`.

### Attachments

- One image per post. JPEG, PNG, WebP, and GIF are accepted, decided by magic bytes rather
  than by the filename or the client-supplied content type.
- Files land under `$EDUCHAN_UPLOAD_DIR` as `originals/{board}/{YYYY}/{MM}/{random}.{ext}`
  and `thumbs/{board}/{YYYY}/{MM}/…`, served from `/media/original/…` and `/media/thumb/…`
  with immutable caching.
- Thumbnails are capped at 250px on the longest side, preserve aspect ratio, and are never
  upscaled. JPEG sources keep JPEG thumbnails; PNG, GIF, and WebP get PNG thumbnails so
  transparency survives.
- `internal/media.Store` is the only route to blob storage. Adding Google Cloud Storage
  means writing another `Store`, not touching handlers or SQL.

### Notes

- Migrations live in `internal/app/migrations/` and are applied on startup inside a
  Postgres advisory lock, recorded in `schema_migrations`. Add a new numbered file rather
  than editing an applied one; `0001_init.sql` is deliberately written with
  `IF NOT EXISTS` so it is a no-op on databases created before migrations existed.
- Post numbers are allocated per board from `boards.next_post_no`; a thread's number is
  the number of its opening post. Thread primary keys stay internal apart from the
  `/api/threads/{id}` compatibility routes.
- Config via env vars: `DATABASE_URL`, `EDUCHAN_ADDR`, `EDUCHAN_POSTER_ID_SALT`,
  `EDUCHAN_TRUST_PROXY`, `EDUCHAN_THREAD_COOLDOWN`, `EDUCHAN_POST_COOLDOWN`,
  `EDUCHAN_LOG_FORMAT`, `EDUCHAN_UPLOAD_DIR`, `EDUCHAN_MAX_UPLOAD_BYTES`,
  `EDUCHAN_MOD_KEY`. See `.env.example` and the README table.
- Moderation is one shared key rather than accounts: `authorizeMod` in `moderation.go` is
  the single gate, so replacing it with real janitor roles later touches only that file.
  Deleting a post or thread removes the attachment blobs after the transaction commits; a
  failure there is logged and leaves an orphaned file rather than a broken post.
- Integration tests (`internal/app/api_test.go`, `internal/app/attachments_api_test.go`,
  `internal/app/moderation_test.go`) run the whole app against an isolated schema in
  `TEST_DATABASE_URL` (falling back to `DATABASE_URL`), so they are safe to run against
  the development database.
