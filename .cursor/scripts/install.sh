#!/usr/bin/env bash
# Idempotent repository setup for the educhan Cloud Agent environment.
#
# The default Cloud Agent image already ships the Go toolchain, so this script
# only needs to (1) ensure PostgreSQL is present and (2) prepare Go module
# dependencies. It must be safe to run repeatedly and safe to run even before
# the application source (go.mod) has been merged.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

echo "==> Ensuring PostgreSQL is installed"
if ! ls /usr/lib/postgresql/*/bin/pg_ctl >/dev/null 2>&1; then
  sudo apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postgresql postgresql-contrib >/dev/null
  echo "    installed postgresql $(ls -d /usr/lib/postgresql/* | xargs -n1 basename | tr '\n' ' ')"
else
  echo "    already present ($(ls -d /usr/lib/postgresql/* | xargs -n1 basename | tr '\n' ' '))"
fi

if [ -f go.mod ]; then
  echo "==> Downloading Go module dependencies"
  go mod download
  echo "==> Building Go packages"
  go build ./...
else
  echo "==> No go.mod yet; skipping Go dependency setup"
fi

echo "==> install.sh complete"
