# Agent guide — educhan

educhan is an anonymous imageboard for tertiary education. This repository currently
contains a small backend **skeleton** for the planned product: a Go HTTP API plus a
minimal embedded web UI, backed by PostgreSQL.

## Stack & layout

- **Go 1.22**, **PostgreSQL 16**, `pgx`/`pgxpool`.
- `cmd/server` — entrypoint (config, graceful shutdown).
- `internal/app` — HTTP handlers, `pgx` store, idempotent schema (`schema.sql`), and the
  embedded single-page UI (`web/index.html`).
- `.cursor/` — Cloud Agent environment (`environment.json` + `scripts/`).

## Canonical commands

- Build: `go build ./...`
- Test: `go test ./...`
- Vet / format: `go vet ./...` and `gofmt -l .` (must be empty)
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
bash .cursor/scripts/start.sh                                   # Postgres on :5432
nohup bash .cursor/scripts/run-server.sh > /tmp/educhan-server.log 2>&1 &   # server on :8080
```

Add the Postgres client binaries to `PATH` when using `psql`/`pg_isready` directly:

```bash
export PATH="$(ls -d /usr/lib/postgresql/*/bin):$PATH"
```

### Testing the app end to end

```bash
# Health + seeded boards
curl -s localhost:8080/healthz            # -> {"status":"ok"}
curl -s localhost:8080/api/boards         # -> placements / research / general

# Create a thread, reply, and read it back
TID=$(curl -s -X POST localhost:8080/api/boards/general/threads \
  -d '{"subject":"smoke test","author":"me","body":"hello"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -s -X POST localhost:8080/api/threads/$TID/posts -d '{"author":"me2","body":"reply"}'
curl -s localhost:8080/api/threads/$TID   # contains both posts

# Confirm persistence in PostgreSQL
psql -h localhost -p 5432 -U "$USER" -d educhan -c "SELECT count(*) FROM posts;"
```

For UI changes, the app is served at `http://localhost:8080` (dark-themed single page:
pick a board, create a thread, open it, reply). Prefer a browser walkthrough for anything
that affects `internal/app/web/index.html`.

### Notes

- The schema (`internal/app/schema.sql`) is applied on every startup with
  `CREATE TABLE IF NOT EXISTS` / `INSERT ... ON CONFLICT DO NOTHING`, so it is safe to
  re-run and to run against an existing database.
- Config via env vars: `DATABASE_URL`
  (default `postgres://educhan:educhan@localhost:5432/educhan?sslmode=disable`) and
  `EDUCHAN_ADDR` (default `:8080`).
