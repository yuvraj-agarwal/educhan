#!/usr/bin/env bash
# Per-boot startup: bring up a local PostgreSQL cluster owned by the current
# user and ensure the educhan role/database exist. Idempotent and safe to run on
# every boot — it detects an already-running cluster and existing objects.
set -euo pipefail

PGBIN="$(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | sort -V | tail -1 || true)"
if [ -n "$PGBIN" ]; then
  export PATH="$PGBIN:$PATH"
fi

if ! command -v pg_ctl >/dev/null 2>&1; then
  echo "postgres binaries not found; run .cursor/scripts/install.sh first" >&2
  exit 1
fi

PGDATA="${PGDATA:-$HOME/.local/share/educhan/pgdata}"
PGLOG="$HOME/.local/share/educhan/postgres.log"
PGPORT="${PGPORT:-5432}"
mkdir -p "$(dirname "$PGDATA")"

if [ ! -s "$PGDATA/PG_VERSION" ]; then
  echo "==> Initializing PostgreSQL cluster at $PGDATA"
  initdb -D "$PGDATA" -U "$USER" -A trust >/dev/null
fi

if pg_ctl -D "$PGDATA" status >/dev/null 2>&1; then
  echo "==> PostgreSQL already running"
else
  echo "==> Starting PostgreSQL on localhost:$PGPORT"
  pg_ctl -D "$PGDATA" -l "$PGLOG" -w \
    -o "-p $PGPORT -c listen_addresses='localhost' -c unix_socket_directories='/tmp'" \
    start
fi

psql_super() { psql -h localhost -p "$PGPORT" -U "$USER" -d postgres -tAc "$1"; }

if [ "$(psql_super "SELECT 1 FROM pg_roles WHERE rolname='educhan'")" != "1" ]; then
  echo "==> Creating role 'educhan'"
  psql_super "CREATE ROLE educhan LOGIN PASSWORD 'educhan'" >/dev/null
fi

if [ "$(psql_super "SELECT 1 FROM pg_database WHERE datname='educhan'")" != "1" ]; then
  echo "==> Creating database 'educhan'"
  psql_super "CREATE DATABASE educhan OWNER educhan" >/dev/null
fi

echo "==> PostgreSQL ready: postgres://educhan:educhan@localhost:$PGPORT/educhan"
