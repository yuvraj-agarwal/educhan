#!/usr/bin/env bash
# Long-running development server for the "server" terminal. Waits for Postgres
# to accept connections (started by start.sh) and then runs the Go server with
# live logs.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

PGPORT="${PGPORT:-5432}"
PGBIN="$(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | sort -V | tail -1 || true)"
if [ -n "$PGBIN" ]; then
  export PATH="$PGBIN:$PATH"
fi

echo "==> Waiting for PostgreSQL on localhost:$PGPORT"
for _ in $(seq 1 30); do
  if pg_isready -h localhost -p "$PGPORT" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

export DATABASE_URL="${DATABASE_URL:-postgres://educhan:educhan@localhost:$PGPORT/educhan?sslmode=disable}"
echo "==> Starting educhan server"
exec go run ./cmd/server
