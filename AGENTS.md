# Agent guide — educhan

educhan is an anonymous imageboard for students, open to anyone aged 13 and over. This
repository contains the working prototype: a Go HTTP API plus an embedded web UI, backed
by PostgreSQL. Boards, anonymous posting (tripcodes, per-thread poster IDs), image
attachments stored without their metadata, a paginated index, a catalog view, a per-board archive, threads with quote
links and post markup (spoilers, highlighted code, maths, cross-board links), per-client
flood control, key-authenticated moderation and board management, and optional
institutional verification badges are implemented. Janitor accounts, bans, and reports are
not.

## Stack & layout

- **Go 1.22** (standard library HTTP, `log/slog`), **PostgreSQL 16**, `pgx`/`pgxpool`,
  `golang.org/x/image` (WebP decoding and thumbnail scaling).
- `cmd/server` — entrypoint (config, logger, graceful shutdown), plus the one-shot
  `-sweep` mode that deletes expired archived threads and exits.
- `internal/app`
  - `app.go` — wiring and routes; `config.go` — environment configuration.
  - `handlers.go` — HTTP handlers, validation, JSON responses.
  - `attachments.go` — upload parsing, media serving.
  - `moderation.go` — moderation key check and the sticky/lock/delete handlers.
  - `boards.go` — board creation and settings updates, behind the same key.
  - `sweep.go` — the archive sweeper behind `educhan -sweep`.
  - `verify.go` — institutional verification: handlers, credential format, cookies;
    `verify_domains.go` + `verify_domains.txt` — the academic domain allowlist.
  - `store.go` — all SQL; `models.go` — the JSON-facing types.
  - `identity.go` — anonymous names, tripcodes, the poster key, poster IDs; `text.go` —
    post text rules, including where `[code]`, `[math]`, `[eqn]` and `[spoiler]` tags
    pair (`markupBlocks`), which the front end's `parseMarkup` mirrors.
  - `middleware.go` — logging, panic recovery, security headers, client IP.
  - `ratelimit.go` — per-client flood control, and the sweep that forgets expired entries.
  - `migrate.go` + `migrations/*.sql` — versioned migrations.
  - `web/` — the embedded single-page UI (`index.html`, `app.css`, `app.js`), plus
    third-party browser builds under `web/lib/` (highlight.js, KaTeX).
- `internal/media` — image validation, metadata stripping (`sanitize.go`, with the EXIF
  orientation reader in `orient.go` and the WebP chunk check in `webp.go`), the
  header-based cost estimate (`cost.go`) and the two memory budgets it is held to
  (`budget.go`), thumbnailing, and the blob-storage interface
  with its two implementations: the local filesystem (`local.go`) and Google Cloud
  Storage (`gcs.go`), selected by `EDUCHAN_MEDIA_BACKEND`. `conformance_test.go` is one
  suite run against both. `internal/media/mediatest` starts an in-process Cloud Storage
  emulator and is imported only by tests. `internal/media/jpegenc` is Go's own JPEG
  encoder, copied with its licence and changed to keep a picture's chroma subsampling
  (its package comment lists every change).
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
- Sweep the archive once: `make sweep` (`go run ./cmd/server -sweep`, same configuration)
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
curl -s localhost:8080/api/boards # -> /math/ … /lang/, then /adm/ … /meta/

# Create a thread, reply to it with a quote link, and read it back
TNO=$(curl -s -X POST localhost:8080/api/boards/meta/threads \
  -H 'Content-Type: application/json' \
  -d '{"subject":"smoke test","name":"ana#trip","body":"hello"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["no"])')
curl -s -X POST localhost:8080/api/boards/meta/threads/$TNO/posts \
  -H 'Content-Type: application/json' -d "{\"body\":\">>$TNO\nreply\"}"
curl -s localhost:8080/api/boards/meta/threads/$TNO   # both posts, with backlinks
curl -s "localhost:8080/api/boards/meta/threads?page=1" # index page
curl -s localhost:8080/api/boards/meta/catalog          # catalog tiles
curl -s localhost:8080/api/boards/meta/archive          # threads pushed off the board
curl -s localhost:8080/api/boards/meta/posts/$TNO       # {"board","no","thread_no"}: where a post lives

# A poster key, as the browser makes one: 16 random bytes, base64url, no padding.
# Posts carrying it get their poster ID from it; posts without it, from the address.
KEY=$(head -c 16 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')
curl -s -X POST localhost:8080/api/boards/meta/threads/$TNO/posts \
  -H 'Content-Type: application/json' -H "X-Educhan-Poster-Key: $KEY" -d '{"body":"keyed"}'

# Confirm persistence in PostgreSQL
psql -h localhost -p 5432 -U educhan -d educhan \
  -c "SELECT b.slug, p.no, p.name, p.poster_id FROM posts p JOIN boards b ON b.id = p.board_id ORDER BY p.id;"
```

Attachments use `multipart/form-data` on the same two endpoints; `image` is the file
field, and everything else stays the same. Uploads are off unless
`EDUCHAN_UPLOADS_ENABLED=1` is set (otherwise an image post answers `403`), and
`GET /api/site` says which, with the size limit:

```bash
curl -s localhost:8080/api/site   # -> {"uploads":true,"max_upload_bytes":8388608}

# Thread with an image, then read back the attachment metadata
TNO=$(curl -s -X POST localhost:8080/api/boards/meta/threads \
  -F subject="with image" -F body="look at this" -F image=@/path/to/photo.png \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["no"])')
curl -s localhost:8080/api/boards/meta/threads/$TNO # posts[].attachment.{url,thumb_url,…}

# Reply with an image; the body may be empty when an image is attached
curl -s -X POST localhost:8080/api/boards/meta/threads/$TNO/posts -F image=@/path/to/photo.jpg
```

Moderation needs `EDUCHAN_MOD_KEY` set on the server; without it every moderation
endpoint answers `403`. Restart the server with the key exported before testing it:

```bash
EDUCHAN_MOD_KEY=dev-mod-key make run
```

```bash
curl -s -X POST localhost:8080/api/boards/meta/threads/$TNO/sticky \
  -H 'Authorization: Bearer dev-mod-key'   # also /unsticky, /lock, /unlock
curl -s -X DELETE localhost:8080/api/boards/meta/threads/$TNO/posts/$PNO \
  -H 'Authorization: Bearer dev-mod-key'   # one post plus its attachment files
curl -s -X DELETE localhost:8080/api/boards/meta/threads/$TNO \
  -H 'Authorization: Bearer dev-mod-key'   # the thread, its posts, its attachments
```

Boards are created and reconfigured with the same key. Everything but the slug is
optional; the slug is lowercase letters and digits in dash-separated groups, at most 24
characters, and may not be one of `reservedBoardSlugs` (`boards.go`). An institution
board is stored as `u-<name>` and shown by the UI at `/u/<name>/`:

```bash
curl -s -X POST localhost:8080/api/boards \
  -H 'X-Mod-Key: dev-mod-key' -H 'Content-Type: application/json' \
  -d '{"slug":"comp-sci","name":"Computer Science"}'          # 409 if the slug is taken
curl -s -X PATCH localhost:8080/api/boards/comp-sci \
  -H 'X-Mod-Key: dev-mod-key' -H 'Content-Type: application/json' \
  -d '{"description":"Systems and theory","thread_limit":50,"image_limit":100}' # omitted fields are unchanged
curl -s localhost:8080/api/boards/comp-sci/catalog            # a new board works everywhere
curl -s -X POST localhost:8080/api/boards \
  -H 'X-Mod-Key: dev-mod-key' -H 'Content-Type: application/json' \
  -d '{"slug":"u-iitb","name":"IIT Bombay"}'                  # page at /u/iitb/, API at /api/boards/u-iitb
```

Archiving and the sweeper are easiest to see on a board with room for one thread and no
archive period, with the server running with `EDUCHAN_THREAD_COOLDOWN=0` so the two
threads can be posted back to back. The sweeper is the same binary in a one-shot mode,
run against the same database while the server keeps running:

```bash
curl -s -X PATCH localhost:8080/api/boards/comp-sci \
  -H 'X-Mod-Key: dev-mod-key' -H 'Content-Type: application/json' \
  -d '{"thread_limit":1,"archive_days":0}'
for body in first second; do curl -s -X POST localhost:8080/api/boards/comp-sci/threads \
  -H 'Content-Type: application/json' -d "{\"body\":\"$body\"}"; done
curl -s localhost:8080/api/boards/comp-sci/archive   # "first", archived by "second"
make sweep                                           # logs: msg=sweep threads=1 posts=1 …
curl -s localhost:8080/api/boards/comp-sci/archive   # empty; the thread now answers 404
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
  -H 'Content-Type: application/json' -d '{"email":"you@iitb.ac.in","adult":true}'
# -> {"status":"sent","label":"IIT Bombay","expires_in":900}; the code is on stdout
# without "adult":true -> 400, and no code, no row, no cooldown
CRED=$(curl -s -b jar -c jar -X POST localhost:8080/api/verify/confirm \
  -H 'Content-Type: application/json' -d '{"code":"481920"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["credential"])')
curl -s localhost:8080/api/verify                   # -> {"enabled":true}, and nothing about you
curl -s -X POST localhost:8080/api/boards/meta/threads \
  -H "X-Educhan-Credential: $CRED" -H 'Content-Type: application/json' \
  -d '{"body":"badged","badge":true}'
curl -s -X POST localhost:8080/api/boards/meta/threads \
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
board index at `/meta/`, catalog at `/meta/catalog`, thread at
`/meta/thread/{no}`). Prefer a browser walkthrough for anything that affects
`internal/app/web/`, and check a phone viewport as well as a desktop one — the catalog,
the index, and the thread view each have their own narrow-screen layout. A fresh browser
meets the age screen first: answer it, or, when a check is not about the screen itself,
set `localStorage["educhan:age-ok-v1"] = "1"` before the page loads (Playwright's
`addInitScript`). Playwright's headless Chromium hides scrollbars unless launched with
`ignoreDefaultArgs: ["--hide-scrollbars"]`; a check on the board row's scrollbar needs it.

CI runs no browser, so the client's privacy and safety invariants are pinned by Go tests
that read `app.js` (`web_invariants_test.go`): the poster key and the credential only in
`submit`, the age screen storing its outcome alone and sending nothing, the badge's 18+
box never ticked in advance or kept, `/api/site` fetched only past the age screen,
KaTeX's options, and the highlighting cap. They are
a floor, not a proof; a change that trips one has changed one of those properties and
needs a browser check before the test changes.

### Front end conventions

- **No inline styles or scripts.** The CSP in `middleware.go` has no `unsafe-inline`
  exception, so `<style>`, `style="…"`, and inline `<script>` are all refused by the
  browser. `TestWebAssetsHaveNoInlineStylesOrScripts` in `web_test.go` catches them first,
  in `index.html` and in the markup `app.js` builds as strings (it scans `app.js` and
  `app.css` too). Setting `element.style.left` from JavaScript is fine — CSP governs
  markup, not the CSSOM — which is how the quote preview positions itself, and how KaTeX
  lays out maths. `setAttribute("style", …)` is not fine: the browser refuses it like a
  `style="…"` in markup.
- **Vendored libraries** live in `web/lib/<name>/` — never a directory named `vendor`,
  which a Go module zip drops wherever it sits (`TestNoEmbeddedDirectoryIsNamedVendor`):
  the published build unmodified, its `LICENSE`, and a `README.md` naming the package,
  version, and where it came from. They
  are served from `'self'` like everything else — never from a CDN — and are left out of
  the inline-markup scan, a heuristic for markup this project writes that a minified
  library's grammars and templates are not. Instead `vendoredWebAssets` in `web_test.go`
  pins every file under `lib/` by SHA-256, so nothing lands there unlisted and nothing
  listed changes unnoticed: adding or updating a library means vetting the build and
  updating its hashes. Theme CSS is not vendored; colours stay in `app.css` with the rest
  of the tokens. The one modified file is KaTeX's `katex.min.css`, cut down to WOFF2 font
  sources because only the WOFF2 fonts are vendored; its README gives the exact `sed`,
  and `TestVendoredStylesheetsReferenceVendoredFiles` fails if any `url()` in a vendored
  stylesheet names a file that is not pinned.
- **Static caching.** `webHandler` reads every embedded file once (`webAssets`) and
  serves it with `Cache-Control: no-cache` and a content-hash `ETag`, plus a gzipped
  copy with its own `ETag` for text types, through `http.ServeContent`
  (`TestWebAssetsRevalidateAndCompress`). The URLs are unversioned, so never mark an
  asset `immutable` or give it a `max-age`: a returning reader would keep an old
  `app.js` after a deploy. Versioned URLs would have to come first.
- `img-src` is `'self' data:`, so the file picker previews an attachment as a `data:` URL.
  A `blob:` URL would need a CSP change.
- `app.css` is tokens first (`:root`), then base, layout, and components; colours live in
  custom properties and are chosen to clear WCAG AA on the panel they sit on. `--faint` is
  the one exception and is for decoration only.
- `app.js` renders HTML strings through `esc()`. Anything interpolated from the API must
  go through it.
- `app.js` is loaded as a module (`type="module"`), so it runs in strict mode and a name
  declared twice at its top level stops the whole file from loading.
  `TestAppDeclaresEachTopLevelNameOnce` reads the top-level declarations off the first
  column; keep them there. `node --check` treats a `.js` file as a classic script and
  lets a duplicate through, so check a copy named `.mjs` instead.
- Post text is markup only at display time: `bodyHTML` and the helpers above it
  (`parseMarkup`, `renderMarkup`) turn the stored body into HTML, every piece of it
  through `esc()`. Tags pair by one rule, mirrored in `markupBlocks` in `text.go` so that
  code and maths never yield quotes and a spoiler never reaches an excerpt; change both
  together, and `TestMarkupBlocks` with them (`OPAQUE_TAGS` and `opaqueTags` list the
  blocks whose text is not markup). The archive reads only the start of each opening
  post, and a cut can unpair a tag — a spoiler whose close was cut off reads as text — so
  its excerpts go through `excerptOfStart`, which first cuts back to `settledStart`, the
  part that pairs as the whole body would. Cross-board links build their href with
  `boardPath`.
- `enhancePosts(root)` is the one hook for work on posts after they are in the document
  (code highlighting and maths): `render()` and `showQuotePreview()` each call it once.
  It must stay idempotent and must never throw — a block it cannot finish keeps its
  plain rendering. highlight.js and KaTeX are loaded from `/lib/` by script (and, for
  KaTeX, stylesheet) elements the first time a post that needs them is on screen, never
  up front.
- Code: a `[code]` block is highlighted only once it comes within a screen of the
  viewport (an `IntersectionObserver`, as for maths), and a block over
  `HIGHLIGHT_MAX_CHARS` stays plain (`data-highlighted="skipped"`). One highlight.js call
  cannot be interrupted, and several grammars are quadratic on a long identifier run, so
  a crafted thread of post-length blocks froze readers' tabs; raise the cap only with a
  timing check on a throttled phone (`TestCodeHighlightingIsBoundedAndLazy` holds it at
  3000 or less).
- Maths: `[math]`/`[eqn]` render as a placeholder carrying the TeX in `data-tex`, and
  `typesetMaths` fills it with `katex.render` once the block is within a screen of the
  viewport (an `IntersectionObserver`; typesetting a long thread up front froze phones).
  **Never `renderToString` into `innerHTML`**: KaTeX's HTML carries layout in `style`
  attributes, which the CSP refuses. A few of KaTeX's own nodes still call
  `setAttribute("style")` (`\vec`, a tall brace, a TeX error and more), so
  `withStyleAttributesThroughCSSOM` routes exactly those through `style.cssText` for the
  synchronous length of one render. The options are part of the security story —
  `trust: false`, `maxExpand`, `maxSize`, and a fresh `macros` table per expression that
  turns `\def`, `\newcommand` and friends into errors, because `maxExpand` counts
  expansions, not what they produce, and one defined macro used a thousand times is
  millions of nodes. KaTeX builds `\nonumber`/`\notag` and `\tag`/`\tag*` on `\gdef`, so
  the table also replaces `\nonumber` and `\tag@literal` with stand-ins that set
  `\@eqnsw` and `\df@tag` directly; never let one return TeX that reaches `\gdef`.
  `TestKaTeXIsCalledWithItsSafetyOptions` (`web_invariants_test.go`) pins the options and
  the list; don't loosen them, or change the test to match, without a browser check that
  repeats the probes (`\href`, `\url`, `\htmlData`, `\def`, a fraction and a root under
  the real CSP).
- Every link to a board, thread, or post is built on `boardPath(slug)` (and
  `threadPath`), and every on-screen board name on `boardLabel(slug)`: an institution
  board's slug is `u-{name}` but its address is `/u/{name}/`, and those two helpers are
  the only places that know it. `parseRoute` maps `/u/{name}/…` back to the slug, and
  `RESERVED_PATHS` lists the top-level words that are never a board; keep it in step with
  `reservedBoardSlugs` on the server (`TestUIReservesTheSameTopLevelWordsAsTheServer`
  reads both and fails when they differ). Cross-board links in post text use the same
  list, so a link to a reserved word stays text. `/{board}/post/{no}` resolves through
  `GET /api/boards/{slug}/posts/{no}` and replaces its own history entry with the thread.
- `formHTML` takes the thread it replies to and follows its state: an archived thread
  gets an explanation instead of a form, a locked one a note. A refusal that arrives
  after the form was drawn (`thread is archived`, say) is a notice from `errorMessage`
  that leaves the draft in place, and a re-render that then finds the thread closed puts
  the draft's text in a read-only box under the note (`keepUnsentDraft`) rather than
  dropping it. A thread at its image cap renders the picker through
  `fileFieldHTML(reason)`, switched off with the reason on screen; `blockFileField` puts
  a live form into the same state when `image limit reached` arrives mid-draft, and the
  summary line is redrawn from `state.thread`. `imageLimitReason` and `badgesHTML` treat
  `image_limit` 0 as a board whose replies never carry images, not a full thread. A
  network failure (`err.unreachable`, set by `api()`) gets its own wording, never the
  browser's "Failed to fetch". A `429` is worded for what was refused: `/verify` passes
  `errorMessage` `{ action: "code" }`, so a code request is told to wait and request a
  code again, never to post again. A `503` with `retry_after` is a post the server was
  too busy to take, worded as that and not as flood control; the form keeps the post,
  image included, and nothing was charged for it. A not-found page names and links a board only when it is
  in `state.boards`: the slug comes from the address, and echoing any slug would let a
  crafted link write the page's copy. A history change that keeps the rendered path (a
  same-thread quote link) only calls `focusHash()`, so revealed spoilers stay revealed. A
  disabled file input is left out of `FormData`, and `submit` posts as JSON when it is.
- **A server that takes no images.** `loadSite` reads `GET /api/site`
  (`{"uploads","max_upload_bytes"}`) into `state.site`. With `uploads` false, `formHTML`
  draws no image field at all — no picker, no drop hint, no Spoiler image box — a file
  dropped on the form goes nowhere and `UPLOADS_OFF_REASON` says why, `threadCounts` and
  `badgesHTML` say nothing about the cap (a count of images posted before the switch
  stays), the catalog leaves out its empty "no image" box, and the empty-post notice
  stops offering an image. Until `/api/site` answers, or if it never does, the forms are
  drawn as before. Either way the server's `image uploads are switched off on this
  server` is handled like `image limit reached`: `blockFileField` takes the file off with
  the reason, the text stays for the next press, and `state.site.uploads` turns false for
  the rest of the visit. The client knows both refusals by `UPLOADS_SWITCHED_OFF` and
  `IMAGE_LIMIT_REACHED`, which `TestClientKnowsTheServersImageRefusals` checks against
  what the server answers, so change their words on both sides together.
  `max_upload_bytes` is the server-wide limit: a picked file over it is taken off the
  form at once, and the picker's hint and the `413` notice name it through
  `uploadLimitText` — never a board, since no board has a limit of its own.
- A re-render replaces the posting form, so `captureDraft`/`restoreDraft` carry the
  half-written post — the attached file included, via `DataTransfer` — across it. New
  fields need adding there too. The spoiler box lives in the file preview and is
  restored only alongside the file it was ticked for.
- A spoilered attachment (`attachment.spoiler`) renders as a `<button>` tile with the
  caption and thumbnail in a `hidden` wrapper behind it; the server thumbnails it as
  usual, and hiding it is entirely the client's job. Keep the thumbnail inside the
  hidden wrapper: a lazy image there is not fetched until it is revealed.
- **The age screen** (`MIN_AGE` in `app.js`: 13, and 0 switches it off) stands in front
  of every view but `UNGATED_VIEWS` (`/rules`, `/faq`). `render` asks `ageStatus()` before
  anything else, and nothing is fetched until it says "ok": `loadSite` gets the board list,
  the verification flag and the site's settings (`GET /api/site`) once, on the first page
  past the screen, not at start-up (`TestSiteSettingsAreFetchedOnlyPastTheAgeScreen`).
  The screen is a `<dialog>` opened with `showModal()`; Escape is blocked three ways
  (keydown, `cancel`, and reopening on `close`), because Chrome lets a repeated Escape
  through a cancelled `cancel`. Only the outcome is stored, as `educhan:age-ok-v1` or
  `educhan:age-refused-v1`, through `storageGet`/`storageSet` so blocked storage means
  asking again next visit rather than a broken page. **The birth month and year are
  never stored, sent, or logged**: they live in two unnamed selects in a
  `method="dialog"` form and are removed from the page once compared. Keep it that way,
  keep the question neutral (no threshold in the copy, nothing preselected, no "I am old
  enough" box), and never add a way back from a refusal. Renaming the refusal key would
  be one, so a changed screen bumps the pass key's version and never the refusal's.
  With no day to go on, `ageFrom` errs young, so an answer in the current month exactly
  `MIN_AGE` years back is refused without being stored: that person may have turned
  `MIN_AGE` this week, and next month the same answer passes.
  Every sentence that names the age is built from `MIN_AGE`.
- The FAQ (`FAQ_SECTIONS`) makes claims about what the code does — poster IDs and the
  poster key, tripcodes, what is logged, the badge, the age screen. So do the rules,
  about what a moderator can do to a post (delete it or its thread, lock the thread) and
  what is not built yet (reports). A change to any of those changes the pages in the
  same commit, and neither page describes a feature before it exists. Both end with
  `HELPLINES_HTML`, one static list for every reader: no lookup of where they are, and
  external links carry `rel="noopener noreferrer"`. The rules and FAQ are readable before
  the age screen, so they must not depend on anything fetched: what a server can switch
  off — images, the badge — they qualify in words ("Not every educhan site …") rather
  than by reading a flag. The footer is not one of those pages, so its sentence inviting
  readers to verify (`data-verify-footer` in `index.html`) stays hidden until
  `GET /api/verify` says the feature is on. The header's Verify link stays regardless:
  `/verify` also holds Forget this browser.
- The verification credential lives in `localStorage` under `educhan:credential` and
  is attached to exactly one request in the whole application: the badged post, in
  `submit`. `readBadge()` decodes its claims locally to decide what to show — the
  signature is the server's to check, and it re-derives the label from its own record on
  every badged post, so a forged local claim buys a 403 and nothing else.
- The poster key lives in `localStorage` under `educhan:poster-key`: 16 random bytes as
  22 characters of unpadded base64url, made by `posterKey()` on the first post rather
  than the first visit. `submit` sends it in `X-Educhan-Poster-Key` on thread-create and
  reply requests and nowhere else, so it cannot become a session identifier. A badged
  post sends the credential **instead of** the key: the server takes a badged post's ID
  from the credential, and the two travelling together would be the one place a
  browser's key met its credential.
  `TestOnlyThePostingRequestCarriesThePosterKeyOrCredential` reads `app.js` and fails if
  either header is attached anywhere but that one line.
- The remembered name (`educhan:name`) is the unbadged posts' name. Ticking the badge
  empties the Name field (`badgeToggled`), and a badged post's name is never remembered:
  a tripcode on both kinds of post would publicly tie the unbadged ones to the
  institution, which the per-credential IDs exist to prevent. A Name the poster cleared
  stays cleared across re-renders and after posting (`restoreDraft` restores empty
  values; `submit` sets the field after `form.reset()`).
- "Forget this browser" on `/verify` (`forgetBrowser`), shown in every state — the
  code-entry step included (`TestEveryVerifyStateOffersToForgetTheBrowser`) — removes
  the credential, the poster key and the remembered name, and leaves the age answer. It
  is what the FAQ's shared-computer advice points to: the key left behind would give the
  next person at the computer the same IDs in the same threads.

### Child safety

educhan is open to anyone aged 13 and over, so some of its readers are children, and
these rules hold for every user rather than for the young ones only. They sit beside the
privacy rules above and work the same way: each is something educhan does not have, and
a change that adds one breaks a promise the rules, the FAQ and the risk assessments are
written on, not just the code. Section numbers are design-recommendations.md's.

- **No private channels.** No direct messages, private replies, profiles, follows,
  friend lists, mentions that notify anyone, or display of where a poster is, not even
  as an opt-in: moving a child from a public thread into a private conversation is how
  grooming usually starts, and having nowhere private to move to is educhan's biggest
  safety asset (§3.4 item 1). The rules and the posting form say so in as many words.
- **The credential gates three things and nothing else**: the badge on a post, skipping
  the captcha, and posting on `/u/` boards, the last two not built yet. Anything more
  behind it starts to make the badge an account, and accounts are what Australia's
  under-16 law and the US state laws on minors' accounts attach to (§4.5, §4.6).
  `TestCredentialIsReadOnlyOnTheBadgePath` and `TestCredentialHasNoEffectOffTheBadgePath`
  hold the code to the badge alone today.
- **Bump order only.** The index and the catalog list stickies, then threads by their
  last bump, and nothing else moves a thread: no "popular", "trending" or "hot", and no
  ranking by views, likes, replies or images, because ranking by popularity is what
  Ofcom's recommender measures regulate (§4.1). The reply and image counts on a catalog
  tile are facts about the thread, as on 4chan, and stay; never add view or like counts,
  or show any count as a score of how others engaged with a post, which is one of the
  "logged-in features" Australia's under-16 law looks for (§4.5).
  `TestCatalogOrdersByBumpNotByReplies` and `TestIndexOrdersByBumpAndHonoursSage` pin the
  order.
- **The age screen is never called verification**, in copy, code, comments or docs. It
  asks a question and takes the answer on trust, which UK law says is no check of age at
  all (OSA s.230(4)), so calling it more claims a protection educhan does not have
  (§4.1). `TestAgeScreenIsNeverCalledVerification` searches the repository for the
  wording.
- **Safe for work, for everyone.** No NSFW board, no adult-content area, and no opt-in
  to one: the rules ban pornography, self-harm and eating-disorder promotion and the
  rest for every user, and that ban is what keeps the UK's highly effective age checks
  out of the plan. 4chan was fined £450,000 for adult boards without them (§3.4 item 2,
  §4.1). The badge is 18+ by attestation (build plan 2.14,
  `TestVerificationRequiresTheAdultAttestation`). That limits who can carry a badge, and
  later who can post on `/u/` boards (not built yet), never what may be posted: the same
  rules hold on every board, for everyone.
- **Never market educhan as safe for teenagers.** No "safe space", no claim that a
  filter catches bullying or anything else, and no feature described to users before it
  is built. NGL, an anonymous app that did the first two, was banned by the FTC from
  offering itself to under-18s (§3.4 item 10).

### Attachments

- One image per post. JPEG, PNG, WebP, and GIF are accepted, decided by magic bytes rather
  than by the filename or the client-supplied content type. `media.Inspect` checks the
  format and the header's dimensions without decoding (and for a GIF the frames' total
  area and count, `gifFramePixels`, a walk over the block structure), and estimates the
  memory processing it will take (**Memory** below); `Process` runs it first,
  so the early refusal in `parseSubmission` and the full one in `media.Save` cannot
  disagree about a file's header. The GIF walk reads every block exactly as `image/gif`
  does — a graphic control extension is a fixed 6 bytes and a plain-text one a fixed 13
  before its sub-blocks, whatever their size bytes say, and an application one starts
  with its own length byte — because a walk that read one extension differently from
  the decoder lost its place there and counted none of the frames after it, which the
  decoder went on to decode. It refuses (`ErrCorruptImage`) a file it cannot follow to
  the trailer or to the end of a whole block. Two kinds of file browsers play and
  `image/gif` refuses are read as browsers read them, and `sanitizeGIF` has the decoder
  read them so too (`gifReader`): one that ends after its last block without the
  trailer, which gets one, and one with a frame after the first reaching past the
  canvas, for which the decoder is told the screen takes in every frame, and which
  `clipGIF` then cuts to the canvas, a frame wholly off it becoming one transparent
  pixel left in place for its delay (`TestGIFIsStoredAsBrowsersShowIt`). A first frame
  past the canvas is refused: Chromium widens the canvas, Firefox moves the frame to
  the corner. `TestGIFFramePixelsReadsWhatTheDecoderReads` and `FuzzGIFFramePixels`
  hold the walk to the decoder: whatever `gif.DecodeAll` decodes from what `gifReader`
  makes of a file, the walk must have read exactly its frames, their area, the largest
  of them (charged again for an interlaced frame's reordering, as the canvas was) and,
  short of an ANIMEXTS1.0 extension, its loop count; and whatever it decodes of a file
  the walk refused is a disagreement. Change the two together.
- **What is stored is never the upload.** `media.Process` rewrites every image without
  its metadata before `Save` hands it to a backend, so both backends store the same
  file (`internal/media/sanitize.go`). Files stored before this release are the
  exception: they were kept as uploaded, metadata and all, nothing rewrites them, and
  they are served that way until their posts are deleted. The README gives the query
  that lists them, cut off at migration `0008`'s `applied_at`; the deployment in
  `deploy/` kept uploads off, so it has none. JPEG is decoded, turned upright by its EXIF
  Orientation (`jpegOrientation`, `orient` in `orient.go`), and re-encoded by
  `jpegenc` at the picture's own chroma subsampling, as `orient` left it, and at
  `jpegQuality`: the lowest quality whose tables quantise no coefficient more coarsely
  than the upload's own did, and `storedJPEGQuality` (90) at most. An upload saved with
  the standard tables at quality 30 is stored at 30, at about its own size and looking
  as it did, where at 90 it came out at about twice the size
  (`TestLowQualityJPEGsAreNotStoredLarger`); one saved at 4:4:4 keeps its colour detail,
  which 4:2:0 averaged away (`TestJPEGKeepsItsChromaSubsampling`,
  `TestColourDetailIsKept`). The upload's tables are read and never copied: all the
  stored file takes from them is that one number. `jpegenc` writes no APP or COM
  segment, so EXIF, XMP, APP13 IPTC/Photoshop, the APP2 ICC profile and comments all
  go. It is `image/jpeg`'s `writer.go` and `fdct.go` from Go 1.24.7, changed only where
  marked `educhan:`; `TestFourTwoZeroIsImageJPEGsOwn` holds the paths it did not change
  to `image/jpeg` byte for byte, so a Go release that changes that encoder fails it
  rather than leaving the copy quietly behind. Thumbnails still go through
  `image/jpeg`. PNG is decoded,
  turned upright by the EXIF Orientation of an eXIf chunk before the first IDAT
  (`pngOrientation`; browsers apply it, and read it nowhere else), and re-encoded,
  which keeps only IHDR, PLTE, tRNS, IDAT and IEND (an APNG keeps its still image); the
  pixels are what `image/png` decodes them to, so 1-, 2- and 4-bit grey comes back at 8
  bits and a colour key (tRNS on grey or truecolour) as colour with an alpha channel,
  or with none when no pixel has the key's colour
  (`TestPNGIsStoredInTheFormItDecodesTo`). `orient`
  keeps every pixel exactly, in a picture of the kind it was given: YCbCr in its
  planes where the chroma samples line up with the turned picture, and otherwise at
  4:4:4 a pixel at a time, because turning a plane whose last sample covers fewer pixels
  shifts every colour along that side. `turnedSubsampling` is the rule: 4:4:4 when the
  turn reverses a side that is not a whole number of chroma samples long — the width
  (reversed at orientations 2, 3, 7 and 8) when it is odd at 4:2:2 or 4:2:0 or not a
  multiple of four at 4:1:1 or 4:1:0, the height (reversed at 3, 4, 6 and 7) when it is
  odd at 4:4:0, 4:2:0 or 4:1:0 — and when 4:1:1 or 4:1:0 turns on its side (5 to 8),
  which no ratio in the image package can hold (`TestOrientKeepsEverySubsamplingExact`,
  `TestOrientKeepsEveryPixelKindExact`). GIF goes through `gif.DecodeAll` and `EncodeAll`, which keep frames,
  positions, delays and disposal and drop comments and every application extension.
  The loop count is written back as NETSCAPE2.0, from whichever loop extension came
  last, NETSCAPE2.0 or ANIMEXTS1.0, as browsers read them; `image/gif` reads only the
  first, so `gifFramePixels` reads both (`TestGIFLoopCountIsReadFromEitherLoopExtension`).
  WebP cannot be
  encoded in Go, so a WebP upload is stored as another format (`sanitizeWebP`): a JPEG
  at `storedJPEGQuality` when it is lossy and opaque, from its own 4:2:0 planes taken
  to full range where they lie (`expandLimitedRange`,
  `TestLossyWebPIsStoredAtItsOwnColourResolution`; its own quality is no guide: a
  WebP's quality setting is not a JPEG's, and the lower it was the larger the JPEG,
  measured on photos up to 1.4 times the WebP from quality 90, two to three times
  from 75 and up to eight and a half from 5), otherwise a PNG (lossless pixels
  exactly, or a lossy picture's alpha), and `Processed.Format`, the stored path's
  extension, the content type, the attachment's `name` and the thumbnail's format all
  follow the stored format, not the upload's. Before decoding, `stripWebP` (`webp.go`)
  rewrites the RIFF so the decoder sees only the picture: it keeps VP8X (flags masked
  to alpha and animation, reserved bits cleared, and a still picture's alpha flag set
  exactly when an ALPH is kept, because x/image refuses the mismatches libwebp ignores,
  a lossless picture with transparency and metadata under the flag among them:
  `TestWebPAlphaFlagFollowsTheChunks`), ALPH, VP8, VP8L, ANIM and ANMF (each
  frame cut to its picture), drops EXIF, XMP, ICCP and every unknown chunk, recomputes
  the RIFF size, drops bytes past the RIFF's end, and refuses a file whose canvas and
  bitstream sizes disagree. Its output is never stored: a decoder ignores whatever
  follows the picture inside a VP8, VP8L or ALPH chunk, so a whole second file, EXIF
  and all, survives stripping (`TestFilesHiddenInsideWebPChunksAreNotStored`,
  `TestUploadedWebPIsServedAsAFileMadeFromItsPixels`). Do not go back to storing the
  stripped WebP to save the re-encode. An animated WebP is still refused as
  `ErrCorruptImage`, as before stripping: x/image cannot decode one. A WebP stored
  before this change stays a `.webp` and is served as `image/webp`
  (`ContentTypeForExt`). `Processed.Data` is the stored file, `Width`/`Height` are the upright picture's,
  the thumbnail is made from the upright pixels, and `StoredImage.Checksum` and `Size`
  are of the stored bytes — the plan's `blocked_media` check on the re-encoded output
  (2.2) depends on that. Tests build their fixtures in code (`fixtures_test.go`) and
  check every needle is in the upload before checking it is gone from what is stored:
  `TestJPEGIsStoredWithoutMetadata`, `TestEveryOrientationIsApplied`,
  `TestPNGOrientationIsApplied`,
  `TestPNGIsStoredWithoutAncillaryChunks`, `TestGIFKeepsItsAnimationAndDropsItsExtensions`,
  `TestWebPIsStrippedChunkByChunk`, `TestWebPIsStoredAsPNGOrJPEG`, the conformance
  suite's "save stores no metadata", and `TestUploadedPhotoKeepsNoMetadataOnEveryBackend`
  end to end over both backends. Every stored original is a file Go encoded from
  decoded pixels; never store, hash or serve the upload's own bytes, or any run of
  them, such as a chunk copied across.
- **Metadata aside, what is stored is the picture, not the channels a picture can carry
  unseen.** A decoded picture still holds what it does not show, and a re-encode would
  carry it through: the colour of a fully transparent pixel (the erased part of a photo
  under cleared alpha), the colour a barely-visible pixel keeps beyond what its alpha
  shows (a photo laid under an alpha of 1 shows one tint yet keeps three full channels),
  and, in a paletted picture, the entries no pixel uses, duplicate entries of one
  colour, the palette order, and which of several equal entries a pixel points at — any
  of which can hold a whole second file, EXIF and all, in the pixel bytes of a flat
  picture. So after decoding, `reduceHiddenColour` cuts each pixel's colour to the
  precision its alpha can show — `reduce8`/`unpremul8` and `unpremul16`, a
  premultiply/unpremultiply round trip, zero under alpha 0 — across `NRGBA` and
  `NRGBA64` (and `lossyWebPToNRGBA` does the same per pixel for a lossy WebP with
  alpha), and `cleanPaletted` (PNG) and `cleanGIF` rebuild a palette through a
  `colourFolder`: one entry per distinct colour its pixels show, in first-use order, a
  single transparent entry, every pixel remapped to it. `cleanGIF` folds the frames that
  copied the global table together into one union table so they keep sharing it (and
  falls back to a table per frame if their colours overflow 256 or none copies the
  global table); a crafted overflow in a lone picture never happens, so `cleanPaletted`
  ignores it. What is left is the picture, kept exactly, and that is the whole guarantee:
  it is not that nothing unseen survives. A fully opaque pixel keeps its exact value,
  including the low byte of a 16-bit channel (`TestPNGKeepsSixteenBitPixels`), so low-bit
  steganography in a visible picture survives by design; removing it would change the
  picture. The one exception is a PNG's gamma, applied so that the picture shows as it
  did (below). `hidden_test.go` holds each path —
  `TestPartlyTransparentColourIsReducedToItsAlpha` the alpha-precision cut and
  `TestFlatPaletteCannotHideAFileInItsPixels` the index channel of a flat palette — with
  the needles checked present in the upload first.
- Besides the decoders, educhan reads structures an uploader controls in a few
  places of its own: the EXIF reader, the JPEG segment walks (`jpegOrientation`, and
  `readJPEGFrame` with its frame header and quantisation tables), the PNG chunk walks,
  the GIF block walk and the WebP RIFF rewrite. Each reads in place and bounds-checks
  every offset and length before use. None collects what it walks into a list: the WebP
  rewrite reads its chunks one at a time (`nextChunk`/`walkChunks`) and keeps only the
  ones that make the picture, so a file of a million empty chunks costs it the picture it
  keeps and a short id string per chunk as it goes, not a struct for each chunk in the
  file (`manyChunkWebP`, `TestWebPChunkWalkDoesNotHoldAStructPerChunk`); the rewritten
  file is the only sizable thing any of them allocates.
  Each gives a safe answer for what it cannot read — orientation 1, the dearest cost,
  `storedJPEGQuality` — except the GIF walk and the RIFF rewrite, which refuse the
  file. The EXIF reader follows no offset but IFD0's (no IFD chain, so no loop).
  Whether the file is an image is the decoder's call alone (`TestHostileEXIF`,
  `FuzzJPEGOrientation`, `FuzzPNGOrientation`, `FuzzJPEGQuality`,
  `FuzzGIFFramePixels`; `FuzzStripWebP` does the same for the RIFF rewrite and checks
  it is idempotent). No EXIF library is needed and none should be added.
- **No filename is kept.** A camera's or a phone's filename can name a date and a
  person, so `parseSubmission` reads only the file's bytes, and an attachment's name
  is `attachmentName(stored path)`: `image` plus the stored format's extension
  (`image.jpg`), whatever the client sent, and so whatever extension it claimed. It is
  stored in `attachments.original_name`, a column that kept its name;
  `0008_attachment_names.sql` rewrote the rows from before the same way, from
  `original_path`, and has to stay in step with `attachmentName`
  (`TestUploadedFilenameIsNotKept`, `TestAttachmentNamesAreRewritten`). The client
  shows it as the caption under the thumbnail.
- Dropping the ICC profile (and a PNG's `iCCP` and `cHRM`) is deliberate: most photos
  are sRGB, which is what a browser assumes untagged, and look the same; a wide-gamut
  one (Display P3, Adobe RGB) shifts slightly. A profile can name a device or an editor,
  and converting to sRGB properly needs a colour engine Go does not have. A PNG's `gAMA`
  is the exception, because dropping it shifts far more than slightly (a picture tagged
  1.0, linear light, showed its mid-grey of 128 at about 188) and applying it needs only
  a curve. `pngGamma` (`gamma.go`) reads it as Chromium does — the first `gAMA` before
  the first `IDAT`, and none beside an `sRGB` or `iCCP` chunk, at 0, or within 5% of
  1/2.2 — and `applyPNGGamma` converts the colour samples, never alpha, to the sRGB
  values that show the same light, at the picture's own depth, before
  `reduceHiddenColour`. `TestPNGGammaIsShownAsBrowsersShowIt` holds a stored grey ramp
  to within a level of what Chromium 141 drew for each tag, and has a case for each of
  those rules; `TestPNGGammaConvertsColourOnly` covers every picture type `image/png`
  decodes to. A change there needs the same browser comparison.
- **Memory.** `MaxPixels` is 25 megapixels, so that the 24-megapixel photos phones and
  cameras save are taken whole (an iPhone's are 5712×4284, 24.5 million pixels;
  `TestTwentyFourMegapixelPhotosAreAccepted`), and a GIF's frames together may not pass
  `maxGIFFramePixels` (four times that), but neither is the memory bound: what an upload
  holds while it is processed depends on how it is encoded as much as on its size.
  `Inspect` estimates it from the header (`processingCost`, `cost.go`): the decoder's
  buffers (a progressive JPEG keeps 256 bytes of coefficients for every 8×8 block until
  its last scan; CMYK and RGB JPEGs decode to 4 bytes a pixel and 16-bit PNGs to 8;
  `image/gif` keeps `gifFrameOverhead` for every frame besides its pixels, so a GIF of
  thousands of empty frames is not free), the copy `orient` makes, the encoder's output
  buffer (`storedBuffer`, which refuses to grow past `storedLimit` with
  `ErrStoredTooLarge`: four times the upload plus half a byte a pixel, so a file Go's
  encoders cannot compress the way it came is refused rather than stored many times
  larger; a lossy WebP with transparency, stored as a PNG and so meant to grow, is held
  to `pngLimit` of its size instead, which the header gives, so its cost refuses it up
  front rather than after decoding), and the thumbnail scaler's scratch (`thumbScratch`,
  for the picture as it is stored, turned upright by `storedOnItsSide`): its buffer, as
  wide as the thumbnail and as tall as the picture, and its tables of weights, 64 bytes
  for every pixel of the two sides, which are most of it for a long strip — a PNG a
  pixel wide and 25 million long would have taken 1.6 GB of them before they were
  charged. `TestThumbScratchCoversTheScaler` measures both, and the turned PNG strip in
  `costFixtures` the whole.
  `peakOf` takes them phase by phase and counts each phase's first allocation on top of
  the previous phase's peak, because a collection that began before the boundary still
  holds that phase's scratch. An estimate over `processingBudget` (256 MiB) is refused
  from the header as `ErrImageTooLarge`, before any cooldown. Which kinds that refuses
  at 25 megapixels is listed in the README, and two tests hold the lists to the model.
  `TestCostlyEncodingsAreRefusedFromTheirHeader` takes each accepted kind as a square
  from an 8 MiB upload, each refused kind at its cheapest (a wide picture from a small
  upload), and every kind at a quarter of the size. A square is not the dearest shape,
  though: a JPEG's sides are rounded up to whole blocks, a turn that cannot keep the
  subsampling copies at 4:4:4, and the thumbnail's weights grow with the longer side, so
  `TestEncodingsAreJudgedAtEveryShape` takes every shape of the full size each format
  allows (`eachShape`: for every shorter side, the four longest sides that fit, both ways
  round), at every orientation, and holds each kind to the shape the README gives for
  it. It reads `processingCost` with the frame header rewritten in place rather than
  calling `Inspect` millions of times, and checks `Inspect` agrees at the shapes that
  decide. A change to `cost.go` that moves a kind, or the shape a kind is refused at,
  changes the README's lists in the same commit. `Save` then holds its estimate of the
  budget from the start of decoding until both renditions are stored, because until
  then it holds what it made (`acquireProcessing`, a first-come-first-served weighted
  semaphore in `budget.go`, so a large upload is not starved by small ones;
  `TestSaveHoldsItsShareUntilItIsStored`), and `processHeld` gives that share back by a
  deferred call until it hands it over, because the server recovers from a panic in a
  decoder or an encoder and goes on with whatever the panic left held
  (`TestAPanicWhileProcessingGivesItsShareBack`). Nothing it writes is copied again on the way
  out: `GCSStore.Put` sets the writer's `ChunkSize` to 0, because the library's default
  is a 16 MiB buffer for every object, kept only for retries a write without
  preconditions never makes (`TestGCSPutAllocatesNoBufferOfItsOwn`).
  The uploads' own bytes have a budget of their own, `media.UploadBudget` (64 MiB), which
  `parseSubmission` takes from (`media.ReserveUpload`) before it reads a multipart body,
  as much as the request's `Content-Length` or the most the body may be, and which the
  handler gives back when the post is answered (`submission.release`). For that to be
  what the body costs, `readMultipart` reads the body itself rather than through
  `ParseMultipartForm`, which keeps a file over its memory limit in a temporary file
  (memory again on Cloud Run) and then hands it over to be copied: the image is read
  once into a buffer the size of what is left of the body (`TestAnUploadIsReadOnce`).
  Its text fields may come to `maxRequestBytes` between them, as a JSON post may, the
  first value of each name is the one read, and parts past `maxFormParts` refuse the
  form (`TestMultipartPostsAreReadAsTheFormSendsThem`). Between them the two budgets
  hold 320 MiB at most, whatever `containerConcurrency` lets in; the rest of the process
  is about 40 MiB idle, and eighty 8 MiB uploads sent at once to a server with
  `GOMEMLIMIT=400MiB` peaked at 330 MiB resident (786 MiB before the upload budget),
  so a 512 MiB instance needs that limit, which this code does not set.
  **Every wait is bounded, and a slow client is told apart from a busy server.** A
  post waits for room in the upload budget under `reserveContext` (`postDeadline`, 25 s,
  from when its handler starts); a wait that does not get room gives up
  `processingAllowance` (15 s, about what the dearest upload takes on one core, with
  margin) before that deadline, with `media.ErrBusy`, and room that is there is taken
  however near the deadline. Its body then has `uploadReadTimeout` (30 s) to arrive,
  counted from when the wait ends, so time spent waiting for room is not taken out of
  the body's: a read deadline set on the connection through an
  `http.ResponseController` once `ReserveUpload` has returned, whatever it returned (the
  logging middleware's `statusRecorder` carries an `Unwrap` so the controller reaches
  the connection underneath); a body slower than that is the client's doing, answered
  `408` (`upload too slow`, `isReadTimeout`), not the `503` a server with no room gives,
  and a body that waited for room is not answered `408` for the server's load.
  Once the body is read, the image has a fresh `postDeadline` (25 s, `processContext`)
  to be processed and both renditions stored — measured from after the body, not from
  the start of the handler, so a slow body no longer spends the time the image has and
  is no longer mistaken for a busy server
  (`TestSlowUploadBodyIsNotMistakenForABusyServer`); `processContext` also resets the
  response's write deadline to cover that processing and the answer, the body read
  having possibly used more than one `WriteTimeout` (30 s, `app.WriteTimeout`, which
  `cmd/server` uses) from the end of the headers. `Save` writes nothing once that
  processing deadline has passed, on either backend (`LocalStore.Put` honours an ended
  context as Cloud Storage does, pinned in `testStoreContract`), and removes a written
  original on a context that has not ended (`context.WithoutCancel`). The database
  commit runs on the request's own context, never a deadline: a deadline passing
  mid-commit could otherwise leave a post made and its image removed. A budget wait or a
  processing deadline that runs out is `tooBusy`, sent to `answerBusy`: `503` with a
  `Retry-After` of `busyRetryAfter` (10 s) and `retry_after`, logged by the request log
  as a warning, not an error; the body of a post turned away for a full budget before it
  was read is still read to its end and dropped, so the browser gets the answer rather
  than a connection closed under its upload. Before the bound, a queued upload was stored
  and its post made after its answer could no longer be sent, and the poster, told the
  network had failed, posted it twice (`TestBusyServerTurnsAnUploadAwayBeforeKeepingIt`,
  `TestPostPastItsDeadlineIsNotMadeAndNotCharged`, `TestWaitingForRoomEndsInTimeForTheWork`).
  A cancelled wait reaches the handler as `context.Canceled`, which `fail` answers like
  any request whose client left. `TestProcessingCostCoversWhatIsHeld` processes a file
  of each kind the model distinguishes (`costFixtures`), at sizes where every term
  charged for decoding comes to megabytes, with the collector running continually, and
  fails if what it marked live passes the estimate for the whole of the processing or,
  before the first byte is encoded, the estimate of the decoding phases alone
  (`decodingPhases`), which the whole's larger encoder and thumbnail terms would
  otherwise hide; `TestThumbScratchCoversTheScaler` holds the thumbnail's scratch the
  same way, and `TestGIFFrameOverheadCoversWhatDecodingKeeps` measures what
  `image/gif` keeps per frame. What a collection marks can be more than was held but
  never less, so each check takes the least of up to three runs (`peakLive`,
  `leastPeak`), and one busy moment does not fail it. They catch a term left out, which
  comes to more than `processingSlack` in some fixture, unless another term charged for
  the same moment is at least as large in every fixture, as the alpha plane and the
  NRGBA copy of a transparent lossy WebP are. They do not catch a term cut short by
  less than that slack: the turn term can be halved and every test still passes. So
  they guard against a term left out, not against every under-estimate, as the comment
  at the top of `cost.go` says, and a new term needs a fixture where it is megabytes
  over the slack, as the comment on `costFixtures` sets out. They are the package's slowest
  tests, over two minutes under `-race`. A change to a decode path, to `orient`, to the
  stored buffer or to the thumbnail changes `cost.go` in the same commit. A CMYK
  JPEG is turned into RGBA where it lies (`rgbaOfCMYK`), not into a second picture.
  Tests narrow the budget with `narrowBudget`. x/image's VP8L decoder allocates its
  Huffman groups — a 167.5 MiB backing array at most (one `hGroup` per meta-image index,
  `vp8lGroupBacking`), plus a node slice per tree — from counts deep inside the
  compressed bitstream that no header or resolution shows. The only bound the file gives
  is the size of the bitstream the decoder reads — a lossless picture's VP8L chunk, or a
  VP8L-compressed ALPH chunk with the 5-byte header the decoder puts in front of it, each
  read through a reader that ends with the chunk (`webpPicture`) — so `vp8lMemory`
  charges that backing (capped by the bitstream size) plus a per-byte bound on the node
  slices (`vp8lNodesPerByte`, with margin over the worst encoding) on those bytes, not on
  the rest of the file. A crafted file at each extreme measures inside it: 160 KiB of
  one-symbol groups that holds about 170 MiB, and 215 KiB of full trees that holds
  about 43 MiB (`TestVP8LHuffmanMemoryIsHeldWithinTheEstimate`). A bitstream over
  224 KiB is refused from its header as `ErrImageTooLarge`, before any cooldown, and
  less in a larger picture, about 205 KiB at a megapixel and 130 KiB at nine
  (`TestVP8LHuffmanMemoryIsCharged` pins the line the README gives). So the 320 MiB
  bound above holds for these files too; the cost of that bound is that large lossless
  WebPs, and WebPs whose compressed transparency alone is that large, are refused — not
  an ordinary cut-out, whose alpha plane is a few kilobytes however large its picture,
  nor a small picture behind a large colour profile
  (`TestLargeWebPsWithSmallVP8LBitstreamsAreStored`). An uncompressed alpha plane is not
  decoded through VP8L and carries no such charge.
- Uploads are off unless `EDUCHAN_UPLOADS_ENABLED` is set (`Config.UploadsEnabled`).
  `GET /api/site` (`handleSite`) reports the switch and the size limit behind the 413
  as `{"uploads","max_upload_bytes"}`, so the client can draw the form to match. It is
  configuration: it reads nothing from the request, sets no cookie, and answers every
  caller the same (`TestSiteReportsTheUploadSettings`, and it is on
  `TestCredentialHasNoEffectOffTheBadgePath`'s list). Keep it that way; anything about
  the caller belongs nowhere near it.
- With uploads off, `parseSubmission` holds a multipart body to `maxRequestBytes` (the
  JSON cap, well above the longest legal post) instead of `MaxUploadBytes` plus
  overhead, and answers an overflow with the switched-off `403`
  (`uploadsSwitchedOff`, a string clients match on) rather than `413`: a body that size
  is carrying a file, and the poster should hear the reason that will not change. A
  smaller file is refused after the form is parsed, and only a part with bytes in it
  counts, because an untouched file input still sends an empty one. Both refusals come
  before flood control and store nothing
  (`TestUploadsSwitchedOffRefusesALargeFileUnread`, which also counts the bytes read).
- Files are keyed `originals/{board}/{YYYY}/{MM}/{random}.{ext}` and
  `thumbs/{board}/{YYYY}/{MM}/…`, served from `/media/original/…` and `/media/thumb/…`
  with immutable caching. The `local` backend puts them under `$EDUCHAN_UPLOAD_DIR`; the
  `gcs` backend puts them in `$EDUCHAN_GCS_BUCKET` under an optional
  `$EDUCHAN_GCS_PREFIX`. The key layout is identical either way.
- Spoilers are a flag on the attachment row (`attachments.spoiler`), accepted as
  `spoiler` in JSON and in multipart (through `isTruthy`), and returned on every
  attachment as `spoiler`, `false` included. The server still makes and serves a normal
  thumbnail; covering it is the client's job. A JSON post can carry the flag but never
  an image, so there it has nothing to apply to.
- **Per-thread image cap.** `boards.image_limit` caps the posts with an image in one
  thread, the opening post included; thread creation is not capped (there is no
  conversation yet to cap). `CreateReply` enforces it for a reply bringing an image by
  counting the thread's posts with attachments inside the transaction that took the
  board row for its post number (`nextPostNo`). Every reply to the board waits on that
  row, so concurrent replies cannot both take the last slot
  (`TestImageLimitHoldsUnderConcurrentReplies`); the thread row lock it also takes keeps
  out a moderator's lock, delete or recount, which do not touch the board row. It
  refuses with `errImageLimit` (`403 image limit reached`). `threads.image_count` is
  the denormalised copy the listings read for `images`/`at_image_limit`: set on thread
  creation, set from that count on every image reply, and recounted when a post is
  deleted. Nothing enforces the cap against the counter. The order in `createPost` is:
  parse the body, resolve the badge, then `ReplyRefusal` — a lock-free read that
  makes `CreateReply`'s checks in its order (missing, archived, locked, and for an
  image the cap, counted from the posts like the real check) — then flood control,
  then the upload is stored, then `CreateReply`. So the common refusal costs no
  cooldown and stores nothing, and the client's "send the text on its own" goes
  through at once (`TestRefusedRepliesCostNoCooldown`). The pre-check decides
  nothing: replies racing for the last slot both pass it, and the check inside
  `CreateReply`'s transaction is the one that holds (`TestCreateReplyRefusesOnItsOwn`
  calls it with no handler in front). A reply refused there has already been charged
  its cooldowns and stored its upload, which `discardUpload` removes. Never move the
  enforcement into the pre-check or drop it from the transaction.
- Thumbnails are capped at 250px on the longest side, preserve aspect ratio, and are never
  upscaled. A stored JPEG (a JPEG upload, or a lossy opaque WebP) gets a JPEG
  thumbnail; a stored PNG or GIF gets a PNG one so transparency survives. They are made
  from the rewritten, upright picture; a GIF's is its first frame placed on the canvas.
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
  There are no down migrations, so a rollback runs old code on a newer schema: `migrate`
  logs an error naming the versions it does not know, and `App.schemaAhead` makes the
  sweeper refuse to run. A rollback across `0005` is unsafe whatever the code says —
  phase-0 code shows archived threads as live and its prune deletes them, leaving
  their files behind (`deploy/README.md`). Under the same lock, after the migrations,
  `renameReservedBoards` moves any board whose slug has since been reserved to
  `<slug>-board` (`TestBoardsUnderNewlyReservedSlugsAreRenamed`), so reserving a new
  word needs no migration of its own.
  A new column on `posts` also means adding it to `TestPostsTableColumnsArePinned`
  (`verify_api_test.go`) in the same change: everything on that table is stored about
  every poster, so each column is there because somebody decided it should be.
- **Threads are archived, not pruned.** A new thread that takes a board past its
  `thread_limit` sets `archived_at` on the least recently bumped non-sticky live threads
  (`pruneThreads`). Every live view — the index count and list, the catalog, and the
  prune subquery itself — filters `archived_at IS NULL`, which is what keeps archived
  threads from counting against the limit or being archived a second time
  (`TestArchivedThreadsDoNotCountAgainstTheLimit`); the partial index
  `threads_board_live_order_idx` covers exactly that set. `Thread()` still serves an
  archived thread, with `archived_at` set, and `CreateReply` refuses it with
  `errThreadArchived` (`403 thread is archived`), checked before the lock flag so the
  poster is told the reason that will not change. `/api/boards/{slug}/archive` lists at
  most `archiveLimit` (3000) of them, newest archived first, with only the start of each
  opening post read from the database. A new query over threads has to decide which set
  it means.
- **The sweeper** (`educhan -sweep`, `sweep.go`) deletes archived threads once their
  board's `archive_days` have passed. It is a one-shot mode rather than a timer because
  Cloud Run only gives a service CPU during requests; in production it is a Cloud Run Job
  on a Cloud Scheduler trigger, which `deploy/README.md` section 2.9 sets up. `SweepArchived` in
  `store.go` takes batches of `sweepBatchSize` (100) with `FOR UPDATE SKIP LOCKED` against
  a cutoff fixed when the sweep starts, so a sweep always terminates. Each batch queues
  the storage keys of its files in `media_deletions` (`0007`) in the same transaction
  as it deletes the rows; afterwards `removeQueuedMedia` takes the queue one key per
  transaction (`NextMediaDeletion`, `SKIP LOCKED`), removes the file and forgets the
  key. A file the store refuses to delete keeps its key and is retried by the next
  sweep (`TestSweepRetriesFilesItCouldNotRemove`). A file the store does not hold keeps
  its key too and fails the sweep, because every queued key is a file the server
  stored: a sweeper whose media settings differ from the server's would otherwise
  delete nothing and report success (`TestSweepAgainstAnotherMediaStoreFails`). That is
  why the sweep checks with `Get` before `Delete` rather than going through
  `removeBlobs`, whose `Delete` treats a missing key as done, as a moderator's
  deletion should. A cancelled sweep stops between files, never between a file and
  its key. It logs one record of counts and nothing that names a board or thread,
  failures included, and the error it returns has the key taken out
  (`TestSweepLogsOnlyCounts`); it returns an error — a non-zero exit — on a database
  failure, a file it could not remove or did not find, or a database a newer release
  migrated (`TestSchemaFromANewerReleaseStopsTheSweeper`). It builds its App with `New`
  like the server, and runs without the salt, the moderation key and the verification
  secret on purpose, so `New` warns about none of them: those warnings are
  `WarnServingSettings`, which only `serve` in `cmd/server` calls
  (`TestSweepModeLogsNoServerWarnings`, `TestServingWarningsAreTheServersAlone`). A new
  warning about a server-only setting goes there, not in `New`. **Legal holds (build
  plan 2.10) are not built**; when they are, `SweepArchived` must skip held threads, and
  its comment says so.
- **Poster IDs** are `posterID(salt, source, thread id)`, and `posterSourceFor` in
  `identity.go` picks the source: on a badged post the credential's random id, otherwise
  the browser's poster key, otherwise the client address. The poster key is 16 random
  bytes the client keeps in `localStorage` under `educhan:poster-key` and sends,
  base64url-encoded, in `X-Educhan-Poster-Key` on thread and reply requests only;
  `posterKey` reads it in the two posting handlers and nowhere else. `posterSource` is a
  tagged type (`key:`, `ip:`, `cred:`) made only through its three constructors, so the
  sources can never produce the same HMAC input and an untagged string cannot reach
  `posterID` by accident. What keeps it honest:
  - The poster key is never stored and never logged. `TestPosterKeyIsNeverStoredOrLogged`
    drives every posting path with one, then searches every column of every table and
    the captured log for it, with positive controls. Do not put it in a log line, a
    column, a cooldown key, or anything else that outlives the request: kept anywhere, it
    is one identifier across all of a browser's threads.
  - Flood control stays on `clientIP`. The key is client-chosen, so a limit keyed on it
    is one any client can step around; for the same reason a poster ID is a courtesy for
    following a thread, not proof of anything.
  - A badged post's ID comes from the raw credential id as presented, never from
    `credential_id_hash` or any other stored value: an ID derived from stored data could
    be recomputed by anyone holding the database and the salt, for every credential in
    every thread, tying badged posts across threads back to a verification row. It never
    uses the poster key either, or a badged post and an unbadged reply from one browser
    would share an ID and tie the reply to the institution. `postBadge` returns the id in
    `badgeGrant.CredentialID`; `TestCredentialIDOnlyFeedsThePosterID` pins that only
    `postBadge` and `posterSourceFor` touch it, and
    `TestBadgedPosterIDCannotBeRecomputedFromTheDatabase` tries every plausible
    derivation from the verification tables, with the true one as its positive control.
  - Header absent means the address fallback; present but not exactly 22 base64url
    characters (empty, padded, sent twice) is `400 invalid poster key`, checked before the
    body is parsed, so it costs no cooldown and stores no upload.
  - Changing the derivation changes every live thread's IDs mid-conversation. There was
    no production data when the poster key arrived, so that change was free; later ones
    will not be.
- **Tripcodes are salted, always.** `name#secret` and `name##secret` give the same
  tripcode, `"!!"` plus `HMAC(EDUCHAN_POSTER_ID_SALT, "trip:"+secret)` (`tripcode` in
  `identity.go`), which is the derivation `##` always had, so existing secure tripcodes
  did not change. The unkeyed single-`!` kind was retired because a plain hash of a
  hand-picked secret can be cracked offline from the tripcode alone; do not bring it
  back as a convenience. `TestParseIdentityTripcodesAreAllSalted` pins the derivation.
- Post numbers are allocated per board from `boards.next_post_no`; a thread's number is
  the number of its opening post. Thread primary keys stay internal apart from the
  `/api/threads/{id}` compatibility routes. Allocating a number takes the board's row
  until the transaction ends, so **posting takes the board's row before any thread's**:
  `CreateThread` allocates and then archives the threads it pushes off, and
  `CreateReply` allocates before it locks the thread it replies to. Locking the other
  way round deadlocks a reply against the new thread archiving it
  (`TestReplyRacingTheArchivingThreadIsRefusedNotDeadlocked`). A reply's quotes are
  recorded with `FOR KEY SHARE` on their targets, in id order, so a quote of a post
  whose thread is being swept or deleted waits for the deletion and is then dropped,
  rather than failing the foreign key and the reply with it
  (`TestReplyQuotingAPostBeingDeletedStillPosts`).
- Config via env vars: `DATABASE_URL`, `EDUCHAN_ADDR`, `EDUCHAN_POSTER_ID_SALT`,
  `EDUCHAN_TRUST_PROXY`, `EDUCHAN_THREAD_COOLDOWN`, `EDUCHAN_POST_COOLDOWN`,
  `EDUCHAN_UPLOAD_COOLDOWN`, `EDUCHAN_LOG_FORMAT`, `EDUCHAN_MEDIA_BACKEND`,
  `EDUCHAN_UPLOAD_DIR`, `EDUCHAN_GCS_BUCKET`, `EDUCHAN_GCS_PREFIX`,
  `EDUCHAN_MAX_UPLOAD_BYTES`, `EDUCHAN_MOD_KEY`, `EDUCHAN_VERIFY_SECRET`,
  `EDUCHAN_VERIFY_CODE_TTL`, `EDUCHAN_VERIFY_CREDENTIAL_TTL`, `EDUCHAN_VERIFY_COOLDOWN`,
  `EDUCHAN_VERIFY_DOMAINS`, `EDUCHAN_UPLOADS_ENABLED`, `EDUCHAN_MAIL_DEV`,
  `EDUCHAN_DB_MAX_CONNS`. See `.env.example` and the README table.
  `listenAddr` in `config.go` also reads `PORT` — the bare port number Cloud Run injects —
  when `EDUCHAN_ADDR` is unset, so a container listens where the platform expects. An
  explicit `EDUCHAN_ADDR` still wins, which is why the deploy config leaves it unset.
- Flood control lives in `ratelimit.go` and is applied by `postCooldowns`/`allow` in
  `handlers.go`. Thread and reply cooldowns are keyed per board and client, the upload
  cooldown per client across the whole server. A submission clears every cooldown that
  applies to it or none is started, so a post refused by one limit is not charged for the
  other; `allowAll` is what enforces that. A cooldown once started is kept, with one
  exception, so every refusal that can be known up front comes before `allow`: a malformed poster
  key, the body, a badge the poster cannot have, uploads switched off, a file its header
  shows to be unusable (`media.Inspect` in `parseSubmission`: not an accepted format,
  more than `media.MaxPixels`, a GIF whose frames come to too many pixels or whose
  blocks do not reach the end, or one whose processing would hold more than the whole
  budget — which turns on how it is encoded, not its size alone), and a reply the thread
  would refuse (`ReplyRefusal`). Decoding the pixels is not up front, because it is the
  cost the upload cooldown limits, so an image that fails to decode, or whose re-encoded
  file would pass `storedLimit` (`ErrStoredTooLarge`), is refused by `storeUpload` after
  `allow`, its cooldown kept (`TestImagesRefusedByTheirHeaderStartNoCooldown` pins both
  sides, `ErrStoredTooLarge` among them).
  The exception is a post the server turns away as busy after `allow` (`tooBusy`: no
  room to process its image in time, or its deadline passing before the image is
  stored): `allow` hands back the refund `chargeAll` makes, which takes back the
  cooldowns that post started and leaves alone any another post has started since
  (`TestChargeAllRefundsWhatItStarted`). A post turned away for want of room to read it
  is turned away before `allow`, so it has nothing to refund.
  State is per process and in memory — a shared backend means replacing
  `cooldownLimiter`, which nothing else depends on.
  The keys carry client addresses, so the limiters are also a privacy surface: `New`
  builds a `cooldownSweep` over all four limiters and starts its timer, which drops
  every expired entry each `cooldownSweepInterval` (one minute), and `Close` stops it.
  **While the process has CPU, an address is held in memory no longer than the
  longest cooldown that applies to it plus one minute**, counted from its last action
  that started one, busy server or quiet. A host that gives CPU only during requests,
  which is Cloud Run's default (request-based billing), holds the timer back on an
  idle instance, so `beforeEachRequest` (in `Routes`) runs an overdue sweep before a
  request is handled; until a request arrives or the instance stops, an idle instance
  still holds what it last saw, and on Cloud Run the one-minute bound needs CPU always
  allocated (`--no-cpu-throttling`). The README's Flood control section states the
  bound with that condition, and anything the client says about addresses rests on
  it, which is why the FAQ promises no time limit. A longer interval, a new limiter
  left out of the sweep, a limiter keyed on something that outlives its cooldown, or
  the request-path sweep dropped from `Routes` breaks it
  (`TestSweepExpiredKeepsOnlyLiveCooldowns`, `TestSweepCooldownsRunsUntilStopped`,
  `TestCooldownSweepRunsBeforeARequestOnceItIsOverdue`, and
  `TestFloodControlForgetsAddressesOnAQuietServer` and
  `TestFloodControlForgetsAddressesBeforeTheNextRequest`, which shorten the period
  through `Config.cooldownSweepEvery`). The record-time sweep past
  `cooldownSweepThreshold` only bounds a burst between two ticks.
- `EDUCHAN_TRUST_PROXY` is a hop count, not a boolean (`true` still means 1). `clientIP`
  in `middleware.go` reads the Nth `X-Forwarded-For` entry **from the right**, because each
  proxy appends and everything to the left is client-supplied; a chain shorter than the
  configured depth or a non-IP entry falls back to the peer address. Setting it higher than
  the real chain hands clients control of their own rate-limit bucket, and of their poster
  ID when they send no poster key.
- Moderation is one shared key rather than accounts: `authorizeMod` in `moderation.go` is
  the single gate, so replacing it with real janitor roles later touches only that file.
  Deleting a post or thread removes the attachment blobs after the transaction commits; a
  failure there is logged and leaves an orphaned file rather than a broken post.
  `removeBlobs` ignores the caller's cancellation, because the rows are already gone and
  nothing would come back for the files later. Sticky and lock apply to live threads
  only: `SetThreadFlag` refuses an archived one with `errThreadArchived` (`403 thread is
  archived`), because nothing brings a thread back from the archive and a sticky that
  answered success would not keep it out of the sweep
  (`TestArchivedThreadsCannotBeStickiedOrLocked`).
- **Routes.** The UI reads any first path segment it does not recognise as a board's
  slug, so `reservedBoardSlugs` in `boards.go` holds every first segment that means
  something else — the server's own routes, `lib` (the vendored libraries), the UI's
  pages, and `u` — and has to agree
  with `RESERVED_PATHS` in `app.js`, which `parseRoute` reads; a new top-level page means
  adding its name to both. `TestReservedBoardSlugs` covers the list, and
  `TestUIReservesTheSameTopLevelWordsAsTheServer` that the two agree. An institution board is stored as
  `u-<name>` and shown at `/u/<name>/`: the UI does the mapping, and the server needs no
  alias because `webHandler` serves the shell for every path that is not an asset, an
  API route, or media (`TestPageLoadsServeTheShell`). The API only ever takes the
  stored slug. A post link — `/<board>/post/<no>`, which is where `>>>/slug/123` in post
  text points — names no thread, so the UI asks `GET /api/boards/{slug}/posts/{no}`
  (`handleResolvePost`) for the thread number before opening it at `#p<no>`. It answers
  for archived threads too, since they still load, and says nothing about the post but
  where it is.
- **The board set** is seeded by `0006_board_set.sql`: thirteen subject boards (`/math/`
  to `/lang/`) and seven student-life ones (`/adm/` to `/meta/`). It renamed the three
  boards `0001` seeded in place — `placements` to `career`, `research` to `grad`,
  `general` to `meta` — so their rows and threads carried over, and it skips any rename
  or insert whose slug is already taken, so it cannot clobber a board a moderator
  opened. `boards.sort_order` is the listing order: `Boards()` lists the seeded set in
  that order and then everything opened over the API (`NULL`), oldest first.
  `/adm/` is admissions and entrance exams at every level, school-leaving included;
  `/meta/` is about educhan itself.
- Board management (`boards.go`) reuses that gate. `0002` gives `boards` its bump and
  thread limits and `next_post_no`; `0005` adds `image_limit`, `archive_days`, and
  `rules`. The API defaults in `boards.go` mirror the column defaults, and
  `TestCreateBoardDefaults` holds the two together. Slugs are validated and normalised
  before they reach SQL, a duplicate comes back as `errConflict` from the unique index
  (`409`), and the slug itself is not updatable because it is the board's address.
  No numeric setting has an "unlimited" value: `boardLimit` refuses zero for the bump
  and thread limits and reads it literally for `image_limit` (no images in replies) and
  `archive_days` (gone at the next sweep).
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
    The one thing besides the label that leaves the badge path is the credential's random
    id, and only as far as the badged post's poster ID (see **Poster IDs** above).
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
    gives "Forget this credential" its own block and states that forgetting is final,
    and offers "Forget this browser" for the poster key and name as well.
    Do not "fix" this back to `sessionStorage` without resolving the reissue problem.
  - Never log an address, and never pass one to anything that persists. The development
    `mailer.Dev` sender prints one to stdout, which is why it is a development tool only.
    The credential is not logged either: it is shown once, in one response body. The
    issuance line (`verification issued`) is bare: no label, no institution, nothing
    from the record. The row dates an issuance to the day; a label stamped to the
    second would date it exactly whenever it was its institution's only one that day.
    A badged post soon after an issuance still names the institution, so the request
    log's time for `/api/verify/confirm` can tie that post to its record on such a day;
    the README says so, and how long that log is kept is what limits it.
    `TestIssuanceIsLoggedWithoutTheInstitution` holds the line bare, and the README's
    privacy section says what the log's times do and do not reveal; keep the two in step.
  - `TestNoEmailAddressReachesTheDatabase` runs the whole flow and then searches every
    column of every table for the address. It has positive controls, so it fails rather
    than passing vacuously if the scan stops working.
  - **The badge is 18+ by attestation, and the attestation is checked, never stored.**
    `POST /api/verify/request` refuses a body without `"adult": true` with `400`
    (`adultRequired`) straight after decoding, before the address is parsed, a cooldown
    starts, a row is written or mail is sent
    (`TestVerificationRequiresTheAdultAttestation`); `/api/verify/confirm` does not ask
    again. Why 18+: the badge is the one feature that keeps something tied to a person,
    an email HMAC for up to 180 days, and keeping it adult-only keeps that record out of
    India's parental-consent rule for under-18s from 13 May 2027 (design-recommendations
    §3.3, §4.4). Why never stored: a kept answer would be a record of age about every
    badge holder, the very data those rules attach to, and a tick-box proves nothing
    worth keeping. So no column, no log field, no credential claim, no cookie and no
    client storage key holds it (`TestAdultAttestationIsNeverKept`,
    `TestBadgeAttestationIsNeverPreselectedOrKept`), and the `/verify` box starts
    unticked on every render.
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
  `internal/app/archive_test.go`, `internal/app/imagelimit_test.go`,
  `internal/app/sweep_test.go`, `internal/app/poster_id_test.go`,
  `internal/app/verify_api_test.go`, `internal/app/verify_transport_test.go`,
  `internal/app/migrate_test.go`, `internal/app/markup_api_test.go`,
  `internal/app/logging_test.go`, `internal/app/media_backend_test.go`,
  `internal/app/busy_test.go`, and
  `TestClientKnowsTheServersImageRefusals` in `internal/app/web_invariants_test.go`) run
  the whole app against an isolated schema in `TEST_DATABASE_URL` (falling back to
  `DATABASE_URL`), so
  they are safe to run against the development database. `testServer` builds its `Config` directly, which leaves every
  cooldown at zero unless a test asks for one, and gives the client a cookie jar so the
  two-step verification exchange behaves as it does in a browser. `verifyServer` adds a
  secret and a capturing `mailer.Sender`, which is how a test reads the code back; the
  credential it returns is then presented explicitly with `postWithCredential`, because
  nothing attaches it on a caller's behalf. Poster ID tests post through `browser`
  (`poster_id_test.go`), which sends what the client sends: the credential alone on a
  post that asks for a badge, and the poster key on any other. A post carrying both is
  never sent by the client, and the tests that cover it set the headers explicitly.
  Tests post to boards they create with `addBoard` (`api_test.go`), never to seeded
  ones, so they do not depend on which boards the migrations seed;
  `TestBoardsAreSeeded` is the one test that names the seeded set.
  A migration's effect on existing data is tested by stopping a schema at an earlier
  version with `databaseAt` (`migrate_test.go`), writing rows into it with plain SQL, and
  starting a server on it with `serverOn`, which applies the rest. Sweep tests call
  `app.sweep` with a small batch to cover the loop, and backdate `archived_at` with
  `archiveThread` rather than waiting. `cmd/server/main_test.go` runs the `-sweep` mode as
  a whole process against its own schema in the same database.
