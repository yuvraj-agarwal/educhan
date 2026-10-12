# Agent guide — educhan

educhan is an anonymous imageboard for tertiary education. This repository contains the
working prototype: a Go HTTP API plus an embedded web UI, backed by PostgreSQL. Boards,
anonymous posting (tripcodes, per-thread poster IDs), image attachments, a paginated index,
a catalog view, threads with quote links, per-client flood control, key-authenticated
moderation and board management, and optional institutional verification badges are
implemented. Janitor accounts, bans, and reports are not.

## Stack & layout

- **Go 1.22** (standard library HTTP, `log/slog`), **PostgreSQL 16**, `pgx`/`pgxpool`,
  `golang.org/x/image` (WebP decoding and thumbnail scaling).
- `cmd/server` — entrypoint (config, logger, graceful shutdown).
- `internal/app`
  - `app.go` — wiring and routes; `config.go` — environment configuration.
  - `handlers.go` — HTTP handlers, validation, JSON responses.
  - `attachments.go` — upload parsing, media serving.
  - `moderation.go` — moderation key check and the sticky/lock/delete handlers.
  - `boards.go` — board creation and settings updates, behind the same key.
  - `verify.go` — institutional verification: handlers, credential format, cookies;
    `verify_domains.go` + `verify_domains.txt` — the academic domain allowlist.
  - `store.go` — all SQL; `models.go` — the JSON-facing types.
  - `identity.go` — anonymous names, tripcodes, poster IDs; `text.go` — post text rules.
  - `middleware.go` — logging, panic recovery, security headers, client IP.
  - `ratelimit.go` — per-client flood control.
  - `migrate.go` + `migrations/*.sql` — versioned migrations.
  - `web/` — the embedded single-page UI (`index.html`, `app.css`, `app.js`).
- `internal/media` — image validation, thumbnailing, and the blob-storage interface with
  its two implementations: the local filesystem (`local.go`) and Google Cloud Storage
  (`gcs.go`), selected by `EDUCHAN_MEDIA_BACKEND`. `conformance_test.go` is one suite run
  against both. `internal/media/mediatest` starts an in-process Cloud Storage emulator and
  is imported only by tests.
- `internal/mailer` — the `Sender` interface verification codes go out through, with a
  development implementation that prints them to stdout.
- `Dockerfile`, `cloudbuild.yaml`, `.gcloudignore`, `deploy/` — the container image, the
  Cloud Build config that builds and pushes it to Artifact Registry, what that build
  uploads, and the Cloud Run service definition (`deploy/cloudrun/service.yaml`). All
  documented in `deploy/README.md`.
- `.cursor/` — Cloud Agent environment (`environment.json` + `scripts/`).

## Canonical commands

- Build: `go build ./...`
- Test: `go test ./...` (the integration tests need PostgreSQL; they skip without it)
- Vet / format: `go vet ./...` and `gofmt -l .` (must be empty), or `make check`
- Bring up Postgres: `make db-up` (`bash .cursor/scripts/start.sh`) — a native PostgreSQL
  on `localhost:5432`, not a container
- Run the server: `make run` (`go run ./cmd/server`, expects Postgres up), or the `server`
  terminal
- **The local loop has no containers in it**: no compose stack, no local image build
- Build the image: `make cloud-build` — Cloud Build builds the `Dockerfile` and pushes to
  Artifact Registry as `:<short SHA>` and `:latest`. It needs gcloud and a GCP project,
  not a Docker daemon, so a `Dockerfile` change is verified by submitting a build.
- Deploy: `make cloud-deploy` — `gcloud run deploy` with the new image onto the existing
  service; every other setting comes from the current revision. See `deploy/README.md`.

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
field, and everything else stays the same. Uploads are off unless
`EDUCHAN_UPLOADS_ENABLED=1` is set (otherwise an image post answers `403`):

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

Boards are created and reconfigured with the same key. Everything but the slug is
optional; the slug is lowercase letters and digits in dash-separated groups, at most 24
characters, and may not be `api`, `media`, or `healthz`:

```bash
curl -s -X POST localhost:8080/api/boards \
  -H 'X-Mod-Key: dev-mod-key' -H 'Content-Type: application/json' \
  -d '{"slug":"comp-sci","name":"Computer Science"}'          # 409 if the slug is taken
curl -s -X PATCH localhost:8080/api/boards/comp-sci \
  -H 'X-Mod-Key: dev-mod-key' -H 'Content-Type: application/json' \
  -d '{"description":"Systems and theory","thread_limit":50}' # omitted fields are unchanged
curl -s localhost:8080/api/boards/comp-sci/catalog            # a new board works everywhere
```

Institutional verification needs `EDUCHAN_VERIFY_SECRET`; without it `/api/verify` reports
`{"enabled":false}` and everything that writes answers `403`. With no mail provider
configured the code is printed to the server's **stdout**, so read it from the log. The
flow sets cookies, so use a jar:

```bash
EDUCHAN_VERIFY_SECRET=dev-verify-secret EDUCHAN_MAIL_DEV=1 EDUCHAN_VERIFY_COOLDOWN=0 make run
```

```bash
curl -s -c jar -X POST localhost:8080/api/verify/request \
  -H 'Content-Type: application/json' -d '{"email":"you@iitb.ac.in"}'
# -> {"status":"sent","label":"IIT Bombay","expires_in":900}; the code is on stdout
CRED=$(curl -s -b jar -c jar -X POST localhost:8080/api/verify/confirm \
  -H 'Content-Type: application/json' -d '{"code":"481920"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["credential"])')
curl -s localhost:8080/api/verify                   # -> {"enabled":true}, and nothing about you
curl -s -X POST localhost:8080/api/boards/general/threads \
  -H "X-Educhan-Credential: $CRED" -H 'Content-Type: application/json' \
  -d '{"body":"badged","badge":true}'
curl -s -X POST localhost:8080/api/boards/general/threads \
  -H 'Content-Type: application/json' -d '{"body":"anonymous"}'  # no header, no badge
```

The credential is returned once and never again: it lives in the client's
`localStorage`, not in a cookie, so `$CRED` is the only copy a shell session has.

Confirm that nothing about the address landed in PostgreSQL — this is the property the
feature exists to hold up, so check it rather than assuming it:

```bash
psql -h localhost -p 5432 -U educhan -d educhan \
  -c "SELECT email_hash, credential_id_hash, label, expires_at FROM verification_credentials;"
psql -h localhost -p 5432 -U educhan -d educhan \
  -c "SELECT no, name, badge FROM posts ORDER BY id;"   # a badge is text, not a reference
```

Flood control is on by default (`EDUCHAN_THREAD_COOLDOWN=30`, `EDUCHAN_POST_COOLDOWN=5`,
`EDUCHAN_UPLOAD_COOLDOWN=15` seconds), so scripted loops will see `429` with a
`Retry-After` header. Set them to `0` when a test needs to post rapidly:

```bash
EDUCHAN_THREAD_COOLDOWN=0 EDUCHAN_POST_COOLDOWN=0 EDUCHAN_UPLOAD_COOLDOWN=0 make run
```

For UI changes, the app is served at `http://localhost:8080` (dark-themed single page:
board index at `/general/`, catalog at `/general/catalog`, thread at
`/general/thread/{no}`). Prefer a browser walkthrough for anything that affects
`internal/app/web/`, and check a phone viewport as well as a desktop one — the catalog,
the index, and the thread view each have their own narrow-screen layout.

### Front end conventions

- **No inline styles or scripts.** The CSP in `middleware.go` has no `unsafe-inline`
  exception, so `<style>`, `style="…"`, and inline `<script>` are all refused by the
  browser. `TestWebAssetsHaveNoInlineStylesOrScripts` in `web_test.go` catches them first.
  Setting `element.style.left` from JavaScript is fine — CSP governs markup, not the CSSOM
  — which is how the quote preview positions itself.
- `img-src` is `'self' data:`, so the file picker previews an attachment as a `data:` URL.
  A `blob:` URL would need a CSP change.
- `app.css` is tokens first (`:root`), then base, layout, and components; colours live in
  custom properties and are chosen to clear WCAG AA on the panel they sit on. `--faint` is
  the one exception and is for decoration only.
- `app.js` renders HTML strings through `esc()`. Anything interpolated from the API must
  go through it.
- A re-render replaces the posting form, so `captureDraft`/`restoreDraft` carry the
  half-written post — the attached file included, via `DataTransfer` — across it. New
  fields need adding there too.
- The verification credential lives in `localStorage` under `educhan:credential` and
  is attached to exactly one request in the whole application: the badged post, in
  `submit`. `readBadge()` decodes its claims locally to decide what to show — the
  signature is the server's to check, and it re-derives the label from its own record on
  every badged post, so a forged local claim buys a 403 and nothing else.

### Attachments

- One image per post. JPEG, PNG, WebP, and GIF are accepted, decided by magic bytes rather
  than by the filename or the client-supplied content type.
- Files are keyed `originals/{board}/{YYYY}/{MM}/{random}.{ext}` and
  `thumbs/{board}/{YYYY}/{MM}/…`, served from `/media/original/…` and `/media/thumb/…`
  with immutable caching. The `local` backend puts them under `$EDUCHAN_UPLOAD_DIR`; the
  `gcs` backend puts them in `$EDUCHAN_GCS_BUCKET` under an optional
  `$EDUCHAN_GCS_PREFIX`. The key layout is identical either way.
- Thumbnails are capped at 250px on the longest side, preserve aspect ratio, and are never
  upscaled. JPEG sources keep JPEG thumbnails; PNG, GIF, and WebP get PNG thumbnails so
  transparency survives.
- `internal/media.Store` is the only route to blob storage, and `newMediaStore` in
  `app.go` is the only place one is constructed. Both backends are held to
  `testStoreContract` in `conformance_test.go`, and `internal/app/media_backend_test.go`
  runs the whole attachment story end to end over each. A new backend is a new `Store`
  plus a case in that switch — never a change to a handler or to SQL.
- `GCSStore` streams through the process rather than redirecting, so the bucket is never
  public and the URLs, headers, and range handling are the backends' shared behaviour
  rather than each one's own. Its `Get` returns a reader that seeks by deferring the range
  read until the first `Read`, which is what makes `http.ServeContent` cost one download
  of exactly the range served. Signed URLs are a later optimisation and are noted as such
  in `deploy/README.md`; nothing in the schema or the routes would have to change for
  them.
- Cloud Storage credentials are Application Default Credentials only — the attached
  service account on Cloud Run, `GOOGLE_APPLICATION_CREDENTIALS` locally. There is no
  setting that accepts a key file, and none belongs in the repository or the image. The
  tests need no credentials at all: `mediatest.StartGCS` runs an emulator in process and
  points the client at it through `STORAGE_EMULATOR_HOST`, so nothing skips for want of
  cloud access — which matters, because CI fails the build if any test skips.

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
  `EDUCHAN_UPLOAD_COOLDOWN`, `EDUCHAN_LOG_FORMAT`, `EDUCHAN_MEDIA_BACKEND`,
  `EDUCHAN_UPLOAD_DIR`, `EDUCHAN_GCS_BUCKET`, `EDUCHAN_GCS_PREFIX`,
  `EDUCHAN_MAX_UPLOAD_BYTES`, `EDUCHAN_MOD_KEY`, `EDUCHAN_VERIFY_SECRET`,
  `EDUCHAN_VERIFY_CODE_TTL`, `EDUCHAN_VERIFY_CREDENTIAL_TTL`, `EDUCHAN_VERIFY_COOLDOWN`,
  `EDUCHAN_VERIFY_DOMAINS`. See `.env.example` and the README table.
  `listenAddr` in `config.go` also reads `PORT` — the bare port number Cloud Run injects —
  when `EDUCHAN_ADDR` is unset, so a container listens where the platform expects. An
  explicit `EDUCHAN_ADDR` still wins, which is why the deploy config leaves it unset.
- Flood control lives in `ratelimit.go` and is applied by `postCooldowns`/`allow` in
  `handlers.go`. Thread and reply cooldowns are keyed per board and client, the upload
  cooldown per client across the whole server. A submission clears every cooldown that
  applies to it or none is started, so a post refused by one limit is not charged for the
  other; `allowAll` is what enforces that. State is per process and in memory — a shared
  backend means replacing `cooldownLimiter`, which nothing else depends on.
- `EDUCHAN_TRUST_PROXY` is a hop count, not a boolean (`true` still means 1). `clientIP`
  in `middleware.go` reads the Nth `X-Forwarded-For` entry **from the right**, because each
  proxy appends and everything to the left is client-supplied; a chain shorter than the
  configured depth or a non-IP entry falls back to the peer address. Setting it higher than
  the real chain hands clients control of their own rate-limit bucket and poster ID.
- Moderation is one shared key rather than accounts: `authorizeMod` in `moderation.go` is
  the single gate, so replacing it with real janitor roles later touches only that file.
  Deleting a post or thread removes the attachment blobs after the transaction commits; a
  failure there is logged and leaves an orphaned file rather than a broken post.
- Board management (`boards.go`) reuses that gate and needs no migration: `0002` already
  gives `boards` its limits and `next_post_no`. Slugs are validated and normalised before
  they reach SQL, a duplicate comes back as `errConflict` from the unique index (`409`),
  and the slug itself is not updatable because it is the board's address.
- **Verified Anonymity is a privacy guarantee before it is a feature, and the README
  states it in full.** Three rules make it true, and a change that breaks any of them
  breaks the promise rather than just the code: the address is never written anywhere
  (only `HMAC(EDUCHAN_VERIFY_SECRET, normalised address)` is, as a uniqueness key); the
  credential carries only a label, an expiry, and a random id; and nothing links a
  credential to a post. Practical consequences when editing `verify.go`, `store.go`, or
  `0004_verification.sql`:
  - Never add a column to `verification_pending` or `verification_credentials` that could
    be joined to a post, and never add `created_at` or a "last used" — an issuance or use
    timestamp correlates with the posts around it. `CredentialLabel` is a read and has to
    stay one. Credential expiry is stored rounded up to a UTC day boundary for the same
    reason; `TestVerificationTablesCannotBeJoinedToPosts` pins both tables' exact columns.
  - **The credential is a header, not a cookie, and this is load-bearing.** It is
    returned by `/api/verify/confirm`, kept by the client in `localStorage`, and sent
    in `X-Educhan-Credential` only on posts that ticked the badge box. A cookie would be
    attached to every request to the origin, which would turn the badge into a stable
    identifier for a poster's whole session whether or not they ever used it. Never add
    a cookie fallback, and never read the credential from a handler that is not creating
    a post — `TestCredentialIsReadOnlyOnTheBadgePath` parses this package and pins the
    call graph (header read in `credential`, called only by `postBadge`, called only by
    the two posting handlers), and `TestCredentialHasNoEffectOffTheBadgePath` presents a
    valid credential to every other endpoint and diffs the responses. In particular
    `GET /api/verify` reports whether the feature is configured and nothing about the
    caller: answering "am I verified?" would mean the credential travelling off the
    badge path. The client reads its own label and expiry out of the credential locally.
  - The trade-off `localStorage` buys that with is documented in the README: it is
    JS-readable where an HttpOnly cookie was not, which is acceptable because the CSP
    forbids inline scripts (`TestWebAssetsHaveNoInlineStylesOrScripts` keeps it that
    way) and a stolen credential grants only posting under that label — no account, no
    history, no PII, and no route back to the address.
  - **Storage is `localStorage`, not `sessionStorage`, and that was a deliberate
    reversal.** Per-tab storage looks like the safer default, but the server issues a
    credential once and cannot reissue it, so a closed tab costs a full email round
    trip: it makes `EDUCHAN_VERIFY_CREDENTIAL_TTL` meaningless and spends the poster's
    own code-request cooldown, which is applied per address hash and per client address
    — the latter shared by everyone behind a campus NAT. The cost accepted in exchange
    is that a badge outlives the sitting on a shared browser, which is why `/verify`
    gives "Forget this credential" its own block and states that forgetting is final.
    Do not "fix" this back to `sessionStorage` without resolving the reissue problem.
  - Never log an address, and never pass one to anything that persists. The development
    `mailer.Dev` sender prints one to stdout, which is why it is a development tool only.
    The credential is not logged either: it is shown once, in one response body.
  - `TestNoEmailAddressReachesTheDatabase` runs the whole flow and then searches every
    column of every table for the address. It has positive controls, so it fails rather
    than passing vacuously if the scan stops working.
- Verification lives in `verify.go` (handlers, credential signing, cookies) and
  `verify_domains.go` (allowlist, address normalisation); its SQL is in `store.go` with
  everything else. The allowlist is a **file** — embedded `verify_domains.txt`, replaced
  wholesale by `EDUCHAN_VERIFY_DOMAINS` — because a domain-to-badge mapping is
  configuration, not data about anybody. Suffix entries (`ac.in`) badge the domain one
  level below; institution entries (`iitb.ac.in = IIT Bombay`) name the place and win over
  any suffix. Plus-addressing is stripped from the uniqueness key and dots deliberately
  are not: that is a Gmail convention, and applying it everywhere would fold two real
  people at one institution into one identity.
- Code requests reuse `cooldownLimiter` through `allowAction`, keyed per client **and** per
  address hash in one `allowAll` call, so a refused request starts neither cooldown. The
  attempt cap on a pending row is what keeps a six-digit code safe; a wrong guess is
  committed rather than rolled back, which is why `ConfirmVerification` returns its outcome
  beside the transaction error instead of through it.
- Deployment (`Dockerfile`, `cloudbuild.yaml`, `deploy/`): the runtime image is
  distroless, so there is **no shell, no package manager, and no `curl`** in it — a
  container healthcheck or a debugging `docker exec … sh` will not work; read the logs or
  hit `/healthz` from outside. The binary is built with `CGO_ENABLED=0` because distroless
  static cannot run a dynamically linked one. `/data/uploads` is created in the build stage
  and copied in owned by uid 65532: the runtime image cannot `mkdir` it, and a volume
  mounted there inherits that ownership, which is what lets the non-root user write
  uploads. Cloud Build consumes that `Dockerfile` unchanged — none of this is
  build-environment-specific.
- `cloudbuild.yaml` tags the image `:<short SHA>` and `:latest` from one `docker build`, so
  both tags always name the same digest. The commit arrives as the `_TAG` substitution
  rather than through `$SHORT_SHA`, which Cloud Build populates only for trigger-started
  builds; `make cloud-build` passes it and refuses to submit with an empty tag or project.
  What the build uploads is `.gcloudignore`, and `.dockerignore` then trims the context
  again inside the build.
- On Cloud Run: attachments belong in Cloud Storage (`EDUCHAN_MEDIA_BACKEND=gcs` plus
  `EDUCHAN_GCS_BUCKET`, with `roles/storage.objectAdmin` on that bucket and nothing
  broader) — the `local` backend can only write to `/tmp`, which is in-memory and
  per-instance, so uploads would not survive. `EDUCHAN_TRUST_PROXY` is `1`, and
  `EDUCHAN_POSTER_ID_SALT` has to come from Secret Manager or poster IDs differ per
  instance. Cloud SQL is reached over the socket Cloud Run mounts at
  `/cloudsql/{connection name}`; `pgx` takes that as `?host=/cloudsql/…` in `DATABASE_URL`,
  so no proxy process and no code change. Details and the caveats in `deploy/README.md`.
- Integration tests (`internal/app/api_test.go`, `internal/app/attachments_api_test.go`,
  `internal/app/moderation_test.go`, `internal/app/boards_test.go`,
  `internal/app/verify_api_test.go`, `internal/app/verify_transport_test.go`) run the
  whole app against an isolated schema in
  `TEST_DATABASE_URL` (falling back to `DATABASE_URL`), so they are safe to run against the
  development database. `testServer` builds its `Config` directly, which leaves every
  cooldown at zero unless a test asks for one, and gives the client a cookie jar so the
  two-step verification exchange behaves as it does in a browser. `verifyServer` adds a
  secret and a capturing `mailer.Sender`, which is how a test reads the code back; the
  credential it returns is then presented explicitly with `postWithCredential`, because
  nothing attaches it on a caller's behalf.
