# Agent guide — educhan

educhan is an anonymous imageboard for students, open to anyone aged 13 and over. This
repository contains the working prototype: a Go HTTP API plus an embedded web UI, backed
by PostgreSQL. Boards, anonymous posting (tripcodes, per-thread poster IDs), image
attachments stored without their metadata, a paginated index, a catalog view, a per-board
archive, threads with quote links and post markup (spoilers, highlighted code, maths,
cross-board links), per-client flood control, content rules that send back or hold posts
(with a moderator queue for the held ones), key-authenticated moderation and board
management, and optional institutional verification badges are implemented. Janitor
accounts, bans, and reports are not.

## Stack & layout

- **Go 1.22** (standard library HTTP, `log/slog`), **PostgreSQL 16**, `pgx`/`pgxpool`,
  `golang.org/x/image` (WebP decoding and thumbnail scaling).
- `cmd/server` — entrypoint (config, logger, graceful shutdown), plus the one-shot
  `-sweep` mode that deletes archived threads past their board's archive period (one
  with a held reply leaves public view instead, until the reply is decided), rewrites
  and vacuums the verification tables, and exits.
- `internal/app`
  - `app.go` — wiring and routes; `config.go` — environment configuration.
  - `handlers.go` — HTTP handlers, validation, JSON responses.
  - `attachments.go` — upload parsing, media serving.
  - `moderation.go` — moderation key check, the sticky/lock/delete handlers, and the
    held-post queue and approval.
  - `filter.go` — the content filter: normalisation, the rule tables and validators,
    and `checkContent`, which every post goes through before it is stored;
    `testdata/filter_corpus/` — the labelled made-up posts `TestFilterCorpus` runs
    through it.
  - `filtersearch.go` — how the filter runs its patterns: each split once into
    branches, one walk of a trie finding every place a branch could start, and a copy
    of Go's regexp machine run from those places alone; `filtersearch_test.go` holds
    every search to what Go's regexp finds with the same pattern, and fuzzes it.
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
- `internal/media` — image validation and thumbnailing (`image.go`), metadata stripping
  (`sanitize.go`, with the EXIF orientation readers and the lossless turns in `orient.go`,
  the WebP RIFF rewrite in `webp.go`, the application of a PNG's gamma in `gamma.go`, the
  GIF player that writes a stored GIF from the pictures it shows in `gifplay.go`, and the
  chroma upsampling, as browsers' decoders do it, of a lossy WebP stored with its alpha
  and of a JPEG turned to 4:4:4 in `upsample.go`), the header-based cost estimate
  (`cost.go`) and the two memory budgets it is held to (`budget.go`), `Save`, which stores
  both renditions, and `Remove` (`save.go`), and the blob-storage interface (`store.go`)
  with its two implementations: the local filesystem (`local.go`) and Google Cloud Storage
  (`gcs.go`), selected by `EDUCHAN_MEDIA_BACKEND`. `conformance_test.go` is one suite run
  against both. `internal/media/mediatest` starts an in-process Cloud Storage emulator and
  is imported only by tests. `internal/media/jpegenc` is Go's own JPEG encoder, copied
  with its licence and changed to keep a picture's chroma subsampling (its package comment
  lists every change).
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
`EDUCHAN_UPLOAD_COOLDOWN=15` seconds, and `EDUCHAN_REFUSAL_LIMIT=10` posts the content
rules send back per minute), so scripted loops will see `429` with a `Retry-After`
header. Set them to `0` when a test needs to post rapidly, or to probe the content rules
with many refused posts:

```bash
EDUCHAN_THREAD_COOLDOWN=0 EDUCHAN_POST_COOLDOWN=0 EDUCHAN_UPLOAD_COOLDOWN=0 EDUCHAN_REFUSAL_LIMIT=0 make run
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
`support_seen` sent only on the one resubmission from the support panel and never kept,
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
  browser's "Failed to fetch". So does an answer to a post that is not the post it made
  (`isCreatedPost`, `err.unconfirmed`): it is checked before the form is reset, so an
  empty `200` never clears a draft (`TestThePostFormIsClearedOnlyForAPostMade`). A held
  post's `202` is a post made, though it has no number until it is approved: taken for an
  unconfirmed one, it kept the draft for the poster to send again. A `429` is worded for
  what was refused: `/verify` passes `errorMessage` `{ action: "code" }`, so a code
  request is told to wait and request a code again, never to post again. A `503` with
  `retry_after` is a post the server was too busy to take, worded as that and not as flood
  control; the form keeps the post, image included, and nothing was charged for it. A
  not-found page names and links a board only when it is in `state.boards`: the slug comes
  from the address, and echoing any slug would let a crafted link write the page's copy. A
  history change that keeps the rendered path (a same-thread quote link) only calls
  `focusHash()`, so revealed spoilers stay revealed. A disabled file input is left out of
  `FormData`, and `submit` posts as JSON when it is.
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
  restored only alongside the file it was ticked for, and the badge box only for the
  badge it was ticked for.
- **The content rules' answers** (see Content rules and held posts below) are shown in
  the form, never as a page change. The draft stays put for a block and for support; a
  held post empties the form as a published one does. They go in the form's own notice
  (`data-form-notice`, above the buttons) rather than the page's `#notice`, because on a
  long thread the poster is looking at the bottom of the page — and so does every other
  refusal of a post (flood control, a locked or archived thread, the network), which
  matters most after **Post anyway**, when the support panel is all that is on screen;
  focus goes back to the button that was pressed (switched off while the post was out,
  it had sent focus to the page), or to the form's own if the form was drawn again.
  A `422` block is worded by `blockedMessage` (through `errorMessage`): the server's
  sentence, which names what it found (a request to talk on another app is not a contact
  detail) and the field, then the server's `match` quoted; a refusal without the
  sentence gets the rule in plain words and the field when it is the Name or the
  Subject. `pointAtMatch` focuses that field, selects the copy of the match the server
  named by `occurrence` (a search that lets whitespace runs differ, since the server
  quotes its normalised text, each search one character on from the last place found,
  as `matchOccurrence` counts on the server; the last copy found if there are fewer; a
  match the server cut and ended in "…" is looked for without it and selected to the end
  of its run of non-space text), marks it `aria-invalid` and describes it by the notice
  until the poster edits it, and scrolls the field and the notice into view together,
  the field clear of the sticky header (`revealBelowHeader`: the least scroll leaves a
  field under the header where it is, so one still covered goes to its scroll margin,
  which is taller wherever the header wraps to three rows, the `max-width: 700px`
  block; `TestNarrowScrollMarginsMatchTheHeaderWrap`).
  Every answer is shown in `liveForm`: a re-render while the post was out ("Update")
  replaces the form it was sent from and puts the draft in the new one, so the answer,
  and a held or published post's emptying of the form, goes to that one; with no form
  left on the same route, the page's `#notice` says it. A `422` support answer opens
  `showSupport`'s panel above the buttons — the server's sentence, `HELPLINES_HTML`, and
  **Post anyway** / **Go back** — and hides the form's own buttons while it is open.
  Post anyway is an unnamed submit button of the form, so the resubmission goes through
  the one submit handler and the one headers line: `supportSeen` is read from
  `event.submitter` and adds `support_seen` to that request only, as `true` in JSON or
  `"1"` in multipart. It is never stored, put in the draft, or left on the form, so the
  next post with the same words is asked again
  (`TestSupportSeenIsSentOnlyFromSubmitAndNeverKept`). Go back closes the panel and puts
  the cursor in the message, clear of the sticky header (`revealBelowHeader`: closing
  the panel shortens the page, which can leave the message on screen but under the
  header, where a plain `focus()` does not scroll); nothing here traps the reader or
  blocks the post. A `202`
  held post empties the form as a published one does, stays on the thread or board (the
  answer has no number, and nothing could show the post yet), and says it is waiting
  for a moderator (`heldNoticeHTML`), linking to the FAQ's `faq-checks` and, on every
  held post alike, to `faq-help` for where to get help. The outcome
  names, the two block rules, `created.held` and both `support_seen` encodings are
  checked against the server in `TestClientReadsTheContentRulesAnswersTheServerGives`.
  Copy here follows the rules page: plain, kind, no claim that a check keeps anyone safe
  or that anyone is watching, and no "this was a mistake" button until reports exist.
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
  poster key, tripcodes, what is logged, the badge, the age screen, the content checks
  (`faq-checks`). So do the rules, about what the content checks send back, hold, and
  answer with where to get help first, what a moderator can do to a post
  (approve a held one, delete it or its thread, lock the thread) and what is not built
  yet (reports). A change to any of those changes the pages in the
  same commit, and neither page describes a feature before it exists. Both end with
  `HELPLINES_HTML`, one static list for every reader: no lookup of where they are, and
  external links carry `rel="noopener noreferrer"` and open in a new tab
  (`target="_blank"`, said to screen readers), because the list is also the support
  panel's and leaving in the same tab would lose the unsent draft. The rules and FAQ are readable before
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
  every badged post, so a forged local claim buys a 403 and nothing else. A posting
  form's badge box sits in a slot that names the badge it offers (`data-badge-option`,
  a number `badgeNumber` gives each credential the page reads, never the credential
  itself), and a post goes out only under the badge its box named. The badge can change
  under a form: Verify pressed for another address, and the reader gone on to a board
  before the answer. That answer redraws the box of any form on screen for the new
  badge, unticked, and says so if it had been ticked (`offerHeldBadge`); before it, a box
  ticked for one institution sent the other's credential
  (`TestPostingFormSendsOnlyTheBadgeItNames`). A post refused because the badge it
  carried expired or was revoked forgets that credential only while it is still the one
  stored. One stored since — that answer, or a badge issued in another tab, which is
  what revoked it — is offered in its place, and the form's notice says so, as it says
  every refusal of a post (`badgeReplacedMessage`): forgetting whatever was stored
  deleted a badge the server cannot hand out again. Either way the refusal is said
  once: the badge box it unticks is not said to be unticked in the page's `#notice` as
  well (`offerHeldBadge`'s `quiet`), which put a second message about the one refusal
  at the top of the page. The box sits among the form's own buttons, which the support
  panel hides while it is open, so a form whose ticked box is drawn again, or whose
  badged post is refused for its badge, closes its panel (`closeSupport`): left open, it
  told the reader to tick a box out of sight, and Post anyway sent without a badge a
  post written to carry one. The reader posts again with the form's own button, and the
  content rules ask again. A badge just issued that closes a panel says why in that
  form's notice, where the panel was (`sayBadgeUnticked`'s `form`), not the page's. A
  panel over a box that was not ticked stays open: that post goes out without a badge
  either way (`TestPostingFormSendsOnlyTheBadgeItNames`).
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
- The Verify page's steps (`verifyViewHTML`, `verifyStep`): the code step comes first and
  is decided on `verifyStep.step` alone, so a pending code's box shows whether or not a
  badge is held, since a badge is replaced only once the new one is confirmed
  (`TestVerifyCodeBoxShowsWhileACodeIsPending`). A refusal after which the server holds
  nothing pending (`NO_CODE_PENDING`, `CODE_EXPIRED`, `CODE_GUESSES_SPENT`: its own words,
  held to them by `TestClientKnowsWhenNoCodeIsPending`) goes back to the address step,
  which says why at the top of its form (`verifyStep.ended`, `CODE_ENDED_NOTES`) and
  brings that into view, since on a phone the notice is a screen above it; a wrong code
  keeps the box. An expired code reaches a browser as `NO_CODE_PENDING`, not
  `CODE_EXPIRED`: the pending cookie lasts exactly as long as the code, so the browser
  has dropped it by then, and that note leads with expiry
  (`TestAnExpiredCodeIsSaidToHaveExpired`). The server answers the wrong code that spends
  the last attempt with `CODE_GUESSES_SPENT`, so the box a wrong code leaves up takes the
  right code until it expires. Every other refusal — a wrong code, the 18+ box unticked,
  an address the allowlist does not know, a cooldown, no answer — leaves the step as it
  is and is said in its form, under the fields and above the buttons
  (`VERIFY_REFUSED_HTML`, `refuseVerification`), which puts the reader back in the field
  it is about, a wrong code selected. A refusal is kept with its step for the rest of the
  visit (`verifyRefusal`, put away by `render()` and by sending again), so the step drawn
  again says it: one that came in while `render()` was still asking before it drew the
  step had no form to be said in, and was lost. The step drawn again starts with its
  fields empty, since nothing typed is kept with it, so it says the refusal without "What
  you wrote is still in the box", which only the form that sent the request, still holding
  what was typed, says (`sayVerifyRefusal`'s `sentIt`, the refusal's `redrawn`,
  `TestVerifyStepDrawnAgainIsNotToldWhatWasTypedIsThere`): said there, a request with no
  answer told the reader their address or code was in the empty field the focus was put
  in. Forget this browser's confirmation is said under its button
  (`confirmBrowserForgotten`). `submitVerification` says its refusals in the form, never
  in the notice (`TestVerifyRefusalsAreSaidInTheForm`, which follows its calls down). Two
  calls on an issued badge can raise one: `storeCredential`'s, when the browser will not
  store it, and `offerHeldBadge`'s, on a page with a posting form whose ticked badge box
  it unticked. An answer belongs to the step it was sent from:
  `submitVerification` notes `verifyStep` before it sends (`sentFrom`) and drops an
  answer, storing, setting and drawing nothing, once the page has moved to another step.
  Forget this browser is the button that can do that while a request is out, and it has to
  win over a badge on its way, which would otherwise be stored after the browser was
  forgotten; the form's own other buttons wait on the answer. They wait in the step drawn
  again for a reader who leaves the page and comes back before the answer, too: the wait
  is kept in `verifyBusy`, not only on the buttons of the form that sent the request, and
  `verifyViewHTML` draws that step waiting, so nothing can give up a step whose badge the
  server is issuing (for the same address, retiring the one held here) or send a second
  code while the page goes on to show the first one's box. No second request goes out
  from a step whose first is out, and one from another step waits until
  `submitVerification` is done with the one before it (`verifyRequest`), then goes only
  if the page is still on its step: a dropped answer's pending-code cookie still lands,
  and the browser keeps whichever lands last, so a code box drawn after Forget this
  browser once refused its own right code while the server checked it against the
  address asked for before. That wait ends: each Verify request — the address, the code,
  and the `GET /api/verify` that `renderVerify` asks before an issued badge is drawn —
  goes with `verifyDeadline()`'s signal and is given up once `VERIFY_TIMEOUT_MS` has
  passed, which `api()` turns into the unreachable refusal. Send code and Verify say it
  on their own step and drop it on any other. The GET only asks whether verification is
  on, and `loadVerification` takes nothing from a request that got no answer, given up,
  unreachable or refused: what the server said last stands, so a badge just issued is
  drawn, focused and offered on posting forms all the same. Taken as "off", the GET
  given up after a Verify drew "not configured" in the badge's place and took the badge
  off every posting form, and the footer's Verify link away, for the rest of the visit.
  Only an answer says verification is off (`answered`); until there is one, the page
  says it couldn't reach educhan rather than that the server is not configured for it.
  The deadline is longer than `ReadHeaderTimeout` and `WriteTimeout` together (constants
  in `internal/app/app.go` that `cmd/server` gives its `http.Server`), so nothing given
  up was still to be answered; the test reads both and checks the server uses them, so a
  change to them changes it too. The signal reaches `fetch` only through `api()`'s
  spread of the caller's options, so `api()` is held line for line up to its fetch:
  built field by field, it dropped the signal with every other check passing.
  Without the deadline, one request whose answer never came, over a connection gone
  silent, held every later one, the step Forget this browser drew included, at Sending…
  until a reload. Telling answers apart by step needs every change of step to assign a
  new object: nothing edits one in place, and the places that make one are a list the
  test holds (`TestVerifyAnswersBelongToTheStepTheyWereSentFrom`). An answer that
  arrives after the reader has left the page (`onVerifyPage`) only sets `verifyStep`:
  nothing is drawn over the page they went to, and coming back shows the step it left
  (`TestVerifyAnswersDrawOnlyOnTheVerifyPage`). `renderVerify` is how
  `render()` draws the page for a reader arriving at it, and how an issued badge is drawn,
  each after one more request; when the page has been drawn in place for another step
  meanwhile, it leaves that drawing alone only while it is still on the page. Every step
  is drawn through `drawVerifyStep`, which notes the step and the drawing, and
  `verifyStepOnScreen` asks whether that drawing is still there. Skipping regardless, a
  code box drawn while `render()` was still asking was drawn over by the loading skeleton
  or by the page just left, its answer landing late, and nothing drew it again;
  redrawing regardless, the badge's late redraw wiped Forget this browser's
  confirmation. The skeleton stands in only for the view it dims (`beginLoading`), and
  the page is titled either way (`TestVerifyStepIsDrawnForAReaderArriving`). What runs
  once each answer is in is held statement for statement, and so are `onVerifyPage`,
  `verifyViewHTML` up to its code step, and `loadVerification`, the one place that sets
  whether verification is on and whether the server has said
  (`TestVerifyCodeBoxShowsWhileACodeIsPending`,
  `TestVerifyAnswersDrawOnlyOnTheVerifyPage`, `TestVerifyRefusalsAreSaidInTheForm`): a
  gate on a held badge in any of them, or in `renderVerify`, hides a pending code's box
  with every other check passing. So does one in what runs on the box once it is drawn:
  `drawVerifyStep`, `showVerifyField`, `sayVerifyRefusal`, what `renderVerify`'s callers
  run on what it drew (`focusHash` in `render()`, `showHeldBadge` after Verify,
  `confirmBrowserForgotten` after Forget this browser), what `renderVerify` and the
  requests it waits on run once their answers are in, since an answer can draw the box
  while it is still asking (the rest of `api()`, `loadVerification`'s `readBadge`, and
  `announce`), and every top-level function those call, however far down
  (`revealClearOfHeader`, `refuseVerification` and the helpers the code step's template
  interpolates). The test finds that set by following the calls rather than from a
  list, and holds each function in it whole, so a function new to the path fails it
  until it is held too: holding the answer alone left `showVerifyField` and
  `revealClearOfHeader` checked for a few words, and a gate in either took the code form
  out of the page. Following calls down does not reach what runs once each call that
  drew the box returns, so that is held line for line too, up to where the browser
  takes over: the page's submit listener to the end of its Verify branch (the one
  listener a Verify form's submit reaches), the forget buttons' callbacks, and
  `render()` from its `finally` on, which leaves the Verify page with `focusHash` alone
  run on it, not the posts and drafts every other page gets; `beginLoading`, whose
  `finish` that `finally` calls, and `navigate`, which waits on `render()`, are held
  whole, and every other call of either is a statement nothing waits on. A gate in the
  submit listener after `submitVerification`, in what `render()` ran once it had drawn
  the page for a badge holder coming back to it, or in `announce`, took the box away
  with every function check passing. What runs on the box with no call from any of
  that is not followed: the page's listeners (a click into the code box runs the click
  listener and `isInternalLink`, a scroll runs `hideQuotePreview`, and keys, changes,
  pointers, hovers and drags have theirs), its timers, and the answers to requests sent
  from other pages. Down their calls lie `render()` and every page, more than a test can
  hold whole, and a gate on a held badge in `hideQuotePreview` or `isInternalLink` took
  the box away with every check passing. So what can tell whether a badge is held — the
  verification state or a badge, read as a property (`state.verification`, a `.badge`
  off anything), destructured or bound under its own name (`{ verification: { badge } }
  = state`), or named in a string; `loadVerification`, which returns the state,
  `readBadge`, `storedCredential`, `localStorage` and the credential's key; and the
  markup only a held badge draws (Keep my badge, `#verify-badge`, a posting form's badge
  box and its number) — is named only in the places
  `TestOnlyKnownPlacesTellWhetherABadgeIsHeld` lists, top-level block by block. It counts
  names in the code alone, not in the copy every page shows (`jsCodeText`), so a gate on
  a held badge that names any of them fails it wherever it is put, a listener included;
  counting property reads alone, it let a destructured read in `hideQuotePreview` take
  the box away. A name made at run time — `state[k]` for a computed `k`, or the page's
  text read back for its wording — is past what counting names can see, and is for
  review to catch. `replacing` puts the badge panel away for
  "Verify another address", and both steps then offer "Keep my badge" back to it
  (`TestReplacingABadgeCanGoBackToIt`). Keep my badge reads the badge again before it
  draws, so one that ran out, or was forgotten in another tab, while the page was on
  another step does not come back as if live: the address step is drawn instead, saying
  why at its top, shown and focused in the panel's place (`showHeldBadge`). Why a badge
  went (`badgeLost`, `badgeLostHTML`) is said at the top of the code step as well as the
  address step, and put away by `drawVerifyStep`, so it is said once whichever way the
  step is drawn; put away by `renderVerify` alone, an expiry noticed on the code step
  went unsaid (`TestAGoneBadgeIsSaidToHaveGone`). Forgetting the badge or the browser
  starts over at the address step; Forget this credential says so at the top of it
  (`CREDENTIAL_FORGOTTEN`) and focuses that, since the redraw took the focused button
  (`TestForgettingTheCredentialSaysSo`). A reload drops a pending code too, since nothing
  about it is stored: the page shows the badge held here, or the address step when there
  is none.
  Whatever a step draws in place of another, or says, is brought into view clear of the
  sticky header (`revealClearOfHeader`, which measures it) and focused without scrolling
  (`showVerifyField`, `showHeldBadge`): `focus()` and `scrollIntoView()` take an element
  under the header for one in view, and on a phone, where the header wraps to three rows,
  a badge holder who had scrolled down got the address field, the code box and then the
  new badge all drawn under it (`TestVerifyStepsAreShownClearOfTheHeader`). Being drawn
  in place, a step takes the button just pressed from under the reader and can put
  another where it was, so the page's own buttons — Keep my badge, Use a different
  address, Verify another address and both Forget buttons (`VERIFY_MOVES`), each of
  which moves the page or forgets — let a press go for `VERIFY_SETTLE_MS` after a step
  is drawn: `drawVerifyStep` notes when, and `pressedTooSoon` decides by when the reader
  pressed, for the click and for its mousedown, so the focus stays where the step put
  it. Before, the second tap of a double tap on Use a different address pressed Keep my
  badge, drawn in its place on a phone, and a double click on Verify another address
  pressed Forget this browser, which the shorter address step brought up into its place
  on a wide screen, and took the badge, the post key and the name. The 18+ attestation,
  the one box the page draws, lets a press go the same way (`VERIFY_ATTESTATION`, its
  label, which holds the box): it is ticked by the reader alone, and a double click on
  Use a different address ticked it on a wide screen, where the address step brought
  clear of the header moved its label under the pointer. A refusal said in a step's
  form moves the step too, though nothing is drawn — its sentence opens above the
  buttons, and the form is brought clear of the header, which scrolls the page — so
  `refuseVerification` starts the wait again. On a phone, the second tap of a double
  tap on Verify, a wrong code refused, pressed Keep my badge, scrolled up into Verify's
  place, and the code box went with the code still pending; on Send code with the box
  unticked, it ticked the box, scrolled down into the button's place, under the refusal
  saying why (`TestVerifyButtonsLetGoAPressAimedAtTheStepBefore`).

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
  offering itself to under-18s (§3.4 item 10). The content filter (`filter.go`) is
  described to readers as what it is — word and number patterns that miss plenty and
  sometimes stop a fine post — and never as something that keeps anyone safe.

### Content rules and held posts

Build plan 2.9 (content rules), the hold half of 2.11 (threats) and 2.12 (self-harm).
`checkContent` in `filter.go` reads the body, the subject, and the shown part of the
name (`displayedName`: before the first `#`, so the tripcode secret is never read or
quoted back). Every one of those fields has already lost its bidirectional controls
(`collapseControl`, through `bidiControl`), which are never stored either: an override
would show a reader text in an order the filter never read (`TestBidiControlsAreNotStored`).
The body's markup is read as a reader sees it (`markupReadings`, from
`markupBlocks`): a `[math]`/`[eqn]` block as one blank, `[code]` tags as line breaks
around text that is read, and `[spoiler]` tags as nothing — so a number, handle,
address or app name split by tags reads whole, as it does on screen once the spoiler is
opened. Where a tag or an invisible character was, `firstNumber` sees the edge of a word
(`view.apart`), so a number hidden in a spoiler straight after a word still stands alone. It works on two views of each field, both
mapped byte by byte back to the source so a match is quoted as written: **plain** (NFKD
compatibility folding, lower case, combining and enclosing marks (keycaps, circles) and
invisible characters dropped, every script's digits as 0–9, dashes as `-`, a short list
of Cyrillic and Greek lookalikes; a character whose compatibility form is more than
`maxFoldRunes` long, a whole phrase such as U+FDFA, reads as itself) and **loose** (plain with spaced
or dotted single letters closed up, a letter written three or more times running read
twice (a capital I counted apart from a small i, since it stands for an l: "kiII"),
leetspeak `0 1 3 4 5 7 @ $` read as letters when a letter is beside it or another one
so read is ("ki11"), `!`
between two letters read as `i`, a letter bracketed inside a word read as part of it
("i[n]sta", `closingBracket`), and `l` read as `i`). Patterns are tables of Go regexps, planned as
searches (`newSearch`, see below), for
both views (`bothViews`, through `canon`, which makes the same `l`→`i` change in the
pattern; `TestFilterPatternsSurviveCanon`) or for the plain view only (`plainOnly`, for
anything whose `@`, dots or slashes matter), plus small validators (`matchCheck`).

| Outcome | Rule / reason | Built from |
| --- | --- | --- |
| block, `422` | `contact` | `firstNumber` (one pass over the plain view; groups split by up to three spaces, line breaks, `-`, `.` or `_`, or a bare `,`, and `o`/`l`/`i` read as `0`/`1` inside a word that holds a digit, or as a lone letter between single-spaced digits, "8 l 2 O" (`digitLookalike`, `lookalikeDigit`); digits spaced one at a time with one kind of mark among them are read in the groups the marks make (`regrouped`), so "9 1 7 - 4 8 2 - 0 1 6 8" has a US number's shape; `formattedNumber`, `dataSet`, `roundRange` (two amounts either side of one dash, each round or written with thousands separators, the second larger: "~70,000–80,000"), `decimalRange` and `ipv4` let decimals, thousands, lists, ranges and IPv4 addresses through; `classifyNumber` with `standsAlone` (maths on either side through `operandBefore`/`operandAfter`, R's `<-` assigning as `=` does when a variable stands before it (`rAssigns`: a name only code has, one letter or with "_", ".", "$", "@" or a digit, or any name a line or a statement starts with, never one of `arrowPointers`, "me", "here", "digits", and no `phoneCue` word in the rest of its line before a "#", nor a `callCue` after one, so "here <- 9123456780 hmu" and "x <- 9123456780 # whatsapp me" are chat arrows and "n <- 9876543210 # number of rows" is code), a label (`labelled`: `numberLabels`, which include a program's "output" and "returns" and a receipt, challan, booking or complaint number, with an edition such as "ISBN-10" and up to three `labelLinks` between; an account label after an `accountApp`, "whatsapp acc", is not one; or a label heading the clause the number completes after "is", "was" or "hai", `labelledSubject`: "the ISBN on my copy is …", with no `contactWords` word, person or possessive between, so "the parcel guy's number is …" still counts) or quantity word, currency, a bracket glued to a name; a `contactCue` straight before the number, "call me =", "wa/", "my number is", outweighs an operator or a maths word near it, and a maths word (`mathsContext`, "prime", "the answer is") counts only with no way to reach anyone within 64 bytes either side (`reachedNear`, below), so "is 9845012345 prime? dm me" and "text me, the final answer is …" are numbers; with no cue, a number given as a maths answer passes, `mathsAnswer`: `mathsResultWords` in the number's own sentence (`sinceSentenceEnd`, `untilSentenceEnd`; "the largest 10-digit number …", "came out as", "for part (b)", what an answer key or a calculator says, "the answer is", "the final answer", "the answer to part c", "according to the solutions"; not "Lost my calculator near the canteen. 9845012345", "for part time", "here's the 10 digit number: …" or "selling the answer key …"), a power or a factorial before it (`powerBefore`), or a counting run among `countingTalk` words, unless a way to reach anyone is within 64 bytes either side, `reachedNear`: `callCue`, `ownNumber`, "my number", or `reachCue`, "dm me", "add me", "dm for more", "my insta", "telegram id", a handle, an app or one of `bareReachVerbs` used bare before "for", "whatsapp for steps", "msg for the working", though not an app named for itself or after one of `appPlaceLeads`, "saw this on reddit", "posted it on whatsapp for the class"), `numberShape`, which every shape goes through, so that a number of one digit over and over (`repdigit`, "9999999999", a placeholder), `dramaNumber` (Ofcom's ranges, 496 0000-0999 only in `ofcomDramaAreas`, `acmaDramaMobiles` and ACMA's landline ranges), `helpline` (`helplineNumbers`: four Indian lines, six US ones and four UK ones, with or without `+91`, `+44`, a trunk zero or a leading 1), `freephone` (UK 0800/0808 grouped 4-x, an organisation's line), a `landline` with an `officeWords` word in the four words before it and no `personalLines` word or `callCue` (`officeLine`: "UQ admissions (07) 3365 1111") and `nanpFiction` (555-0100 to 555-0199 in its 3-3-4 layout, the NANP's numbers for fiction) pass in every layout: `internationalPhone` (a `+` inside the bracket too, "(+91)"; `dataSet` never reads a `+` run as data), `indianMobile` (`mobileGrouping`; single digits only when every separator is of one kind, `oneSeparatorKind`, or one digit apart among groups of three or more, "98450 1 2345"), `calledMobile` (any grouping of two or more, one single digit allowed among them, after a `callCue`: "call me", "text me", "reach me", "my number"), `nanpPhone` (`nanpSeparators`, a `phoneCue` before spaced groups, or ten digits run together straight after a `phoneLabel` ("cell", "Google Voice:", "ph no") or a `contactCue`), `ukPhone`, `auMobile`, `auLandline` (0[2378] and 2-4-4 groups), `tenDigitMobile` (South African and Irish mobiles, 06/07/08 but not 070, 080 or 088, `tenDigitPrefix`, 3-3-4 or 3-7), `tenDigitCalled` (the same ten digits run together straight after a `phoneLabel` or a `contactCue`, as a North American number is read: "my number is 0821234567"); an eight-digit `sgMobile` (8 or 9 first, one block, 4-4 or a digit at a time) only after a `contactCue` or `callCue` ("my hp"), since eight digits alone are as often a date or a roll number; `mobileGrouping` lets pairs set off by dots or dashes leave one digit over at an end ("07.88.42.61.39.0"), and `operandAfter` reads "<-" as an arrow; a seven-digit `localNumber` (3-4, "-" or ".") counts only with an area code given apart within 40 bytes before it or 32 after (`areaCoded`, `areaCode`: "482-0196 (area code 404)"), or with it set off by a slash straight before it after a `phoneLabel`, `contactCue` or `callCue` (`slashAreaCoded`: "phone 495/418-3927"), and a run said to be backwards (`reverseCue` with a `callCue` before it) is read `reversed` unless it counts up or down; `piecedNumber`, tried at a run that is no number by itself: three to `maxPieces` pieces of one to six digits, `maxPiecedDigits` in all, each as small as one digit or spaced out a digit at a time ("07 9 1 5 / 3 3 0"), joined by `/`, `|`, `,` (a joining word may follow the mark: "76, then 1180") or `pieceJoiners` ("then", "and", "plus", "followed by", "ends in", "aur", "phir"), or quoted pieces joined by `+` ("'+91' + '98450' + …"), which together make a phone number (`piecedPhone`, which turns down helplines, drama numbers and counting) and have a `callCue` (not past a `dataWords` word straight before the pieces: "questions 87, 88, 91 … hmu") or an `evasionCue` ("so the mods dont catch") beside them, or for quoted pieces a `phoneCue` before; two pieces only straight after a `contactCue`, "call me on 98450/12345"), `halvedNumber` (two parts each labelled, `partLabelled` with `firstParts`, `lastParts` and `partSizes`: "first 5: 93104 / last 5: 72286", no other cue needed), `firstSpelledNumber` (digits read out one at a time, `spelledDigits` in English and Hinglish with `digitRepeats`, at least two words, mixed with digits or not, a run read again from its second word only after one of `ambiguousDigitWords`; `spelledPhone`: a mobile's shape, a North American number only after a `phoneCue`, not after a label or a `dataWords` word, and not `counting` up or down unless cued), `emailRule`/`providerEmailRule` (`providerEmail`)/`spelledEmailRule` (`emailTLDs` and `academicSecondLevel`, `exampleDomain`, `spelledEmail`, which lets `codeShape` through at a bare "at" ("Q3 at leetcode.com") except at a mail provider or a university, and a `dottedAbbreviation` ("B.Tech", "vol.1"), an `ordinalWord` ("2nd-year") or a `toolNames` version ("Python3.12") anywhere but a mail provider; bracketed `[.]` and `[@]`; `domainNamed`: a domain named with its `@` after a word of the sentence, not after an `addressCues` word (a verb of messaging or "at me" among them, "msg me at riya @madeup.ac.in"), a `nameLike` local part or before a provider, "signed up with my @iitb.ac.in address", "is a @yahoo.com address okay?"), `firstSpacedEmail` (an address spelled out a character at a time round an `@`, `spacedPart` reading single characters one space apart either way, at most 64; a provider with no ending only when the local part is spaced out too or `nameLike`, so "lol @ g m a i l" is not one), `myMailRule`, `mailColonHandle` (a colon, a dash or an arrow, not the "=", "=>" or ">" of code), `linkRule` + `contactLink` (a slash may be spelled, `linkSlash`; a path may start with Signal's `#`; a call's join link — Zoom's "/j/", "/w/" or "/s/" with a meeting's number, its web client's "/wc/join/<number>" or "/wc/<number>/join", or "/my/" with a room's name, a Meet code shaped `meetCode`, Teams' "/l/" with one of `teamsJoins` or "/meet/" with a meeting's number, Webex's personal room ("/meet/" or "/join/" and a name), "j.php" or "e.php" meeting or event anywhere after the site's name on any site's subdomain ("/acme/j.php", "/acme/k2/j.php"), or join-service page, any Jitsi room — and a LinkedIn "/in/" or "/pub/" profile, not the services' other pages), `linkedInPathRule` + `linkedInCheck` (a profile's "/in/" path after LinkedIn's name in place of the host, "LinkedIn - /in/x-y", or the name in the shape LinkedIn generates, hyphened words ending in a part of five or more letters and digits with a digit among them, "linkedin: x-y-1a2b3c", not "covid-19"; a vanity name alone has the shape of "linkedin: self-promotion" and is not read, nor is lnkd.in), `meetingIDRule` (a call's meeting number, `meetingNumber`, after "zoom id", "meeting ID", Webex's "meeting number (access code)" and the like, or after Zoom's or Webex's name with no label, "join zoom 845 2219 6630", "webex: 2634 123 4567"; a Meet code, `meetCodeShape`, after "meet code" or Meet's name, "gmeet qvx-trmk-hpd"; not when `reported`), `spacedInviteRule`, `askRules` + `askCheck` (`topicWords`, `rankedNumber` for "number one" and "number of", `notNegated`, `reported` for an ask someone else made, and `numberRefs` before "number"; an address asked for only with nothing after it but `mailTails` (`mailAsked`), and in the shapes read for apps by `mailAskRuns` + `mailAskCheck` ("ur email pls", "email address pls", "can i get your gmail", "may i have your college email id?", "give me ur mail id"; a bare "mail" only with "e" or "g" before it or a label after it, so "can i get your mail" is post; reported by `firstRun`); "whatsapp me", "facetime me", "snap me", "insta me" and the like only asked (`askedOfReaderHere` through `askLead`: `askMood`, a number before it, or any other lead in its clause — a condition, a relative, a purpose or whom it asks, "if ur bored kik me", "anyone who wants the notes whatsapp me", "lonely girls whatsapp me" — unless the lead ends in a subject, its auxiliary or an adverb of how often, `statesAnAct` with `actSubjects`, `actAdverbs`, `modalsPast` and `thirdOwners`, "she keeps trying to kik me", "my mum will whatsapp me", or opens with one of `whWords`; `habitAfter` turns down "kik me all the time", and `snapNotAsked` "snap me out of it" and "oh snap me too"); "lets facetime" and "can we facetime" only with nothing but `callTails` after them, not "can we facetime grandma"; moving a chat to an app, swapping accounts, "leave your number", "number bhejo", "follow me on X @…"), `numbersAskRule` + `numbersCheck` (everyone's numbers asked for, "drop your numbers", with a way to reach people in the ask, `numbersReach` ("dm me your numbers"), or `gatheringWords` round it, "anyone wanna make a whatsapp group? drop your numbers", or where the ask ends, `numbersTails`, unless `aboutResults` reads the words round it as an exam's results, `scoreWords`, "how many", "what did everyone get" ("results are out! drop your numbers below"), where only a way to reach people named round it counts (`reachNamed`: an app, someone added, called or texted, a group made; not scores added to a sheet, `sheetWords`, "a close call" or "the text section"), and not with `numbersTopics` straight after, "post your numbers from the mock"), `accountNumberRules` (a Session ID; a friend code), `atAskRule`, `myAppRule` + `myAppCheck` (`notAHandle`), `myAmbiguousAppRule` ("my signal is …", only with a handle's marks after it), and `handleRules` (`appColonHandle`, `appAtHandle`, `appLabelHandle`, `hindiHandle`, `handleBeforeApp`, `discTagHandle`, `labelledAppHandle` ("line id", "steam username", "riot id: name#TAG"), `riotTagHandle` + `riotTagCheck` (the name and its tag whole words, so hashtags run together are none, "#valentines#love", and a label or a mark after "val", "lol" or "league", not "lol same#mood"), `ghostHandle` (👻); `handleSeparator` between an app and a handle) + `handleCheck` (`plausibleHandle`, `measureShape`; for `handleBeforeApp`, `handleFirstCheck` and `toolNames`: "python3 on discord"; for `appLabelHandle` with no label, `labelHandleCheck`, `toolNames` and `dottedAbbreviation`: "ig python3 is better", "TG B.Tech"); and `appNameRule` + `appFrame` (`firstAppFrame`), which reads the grammar those phrasings are instances of from every app's name (and 👻; Bluesky's, and Threads' only with a handle and its "@" after it, "my threads is @x.y", since a thread is also a conversation here) outwards — seven words back and twelve on, within its sentence (`readBack`, `readOn`), a `topicWords` word straight after making it a topic ("my snap score"), an "M.Sc" turning it down — for: a verb of moving or talking with its object, particle and preposition said as an ask (`asksToMove`, `hollaVerbs` with "at", what is moved when it is `movedTalk` ("the study group"), `askMood`: an imperative, a "let's" or a "we" with a modal (`weFrame`), a "can we", a "can someone" (`anyoneWords`), a "wanna"; a kind of call goes with its verb, "can we video call on whatsapp" (`callKinds`); `askTails` the only words after it: "come to tg", "follow me back on insta pls"), a question about the reader's account (`asksForAccount`: "u got insta?", or in a question's word order with no mark, `accountAux`: "do u have telegram", "is anyone here on discord", except about a game, `gameAppWords`: "r u on minecraft rn" is about playing), messages there (`messagesThere`: "slide into my dms on insta", `takenToDMs`: "take this to dms on insta"; a message asked for there, `sendVerbs` and `sentMessages` after an ask's lead, "send me a dm on insta"; "dms open on discord" opening its clause), a streak partner looked for (`streakWords`, `streakPartners`, `partnerAsks`), the Hinglish either way round (`hinglishAsk`), the app made a verb with "karo" (`appKaro`: "mujhe whatsapp karo" after `hindiMe`, or with only `hindiKarTails` after it to the end of its clause, and never with `hindiPast`), a group or call set up there with the app before it or after it, `groupPreps` (`setsUpAGroup`: "make a gc on snap"; asked or offered with `makeVerbs`, after "wanna" or "who wants to" as well; being made by the poster, `settingUp`, "creating a whatsapp group for neet aspirants", but not as a sentence's subject before `gerundVerbs`, "creating a discord server is easy"; made by the poster already with `joinWords` after it, `madeVerbs`, "made a discord server for jee 2027, join", not "my mom made a whatsapp group"), talking "there" once the app or a link to it is named (`talksThere`), the poster saying they are on it with an invitation to talk in the sentence (`announcedOn`, `invitesTalk`: "dm me", "hmu" or "add me" always, "i'm on snap if anyone wants to talk"; an offer to talk not where the sentence keeps the talk here, a "here" or a "thread" with one of `postingHere` up to five words before it in its clause, "reply here", "in this thread", "talk about the syllabus here", read on past the twelve words if need be (`talksHere`, `talksHereOn`), but not a place complained of or one the poster is new to, "this thread is dead", "new here" (`placeComplaints`), nor where the app is followed by how much the poster is on it, `appUsage` ("a lot", "for hours", "6 hours a day"), and the talk by the app or its use, `aboutUsage` with `usageTopics`, "i'm on insta 6 hours a day, anyone else wanna talk about phone addiction?", not "… about anything"), a handle after a few lead words (`handleAfterApp`, `leadWords`, "user" and "userid" among them, read as written or without an "'s", so "my ig it's x_y" and "my snap's x_y" both count; after a mark, "~" and pointing emoji among them, "insta 👉 x.y"; Reddit's "u/x_y"; quoted after "search"; never one followed by "(" or after code's "=" or "=>" for a mail app), an address in two halves round a mail app (`splitAddress`, `addressAt`, `labelledLocal`: "[at] gmail [dot] com — prefix is …"), and a handle that is another field of the post (`otherFieldNamed`: "my insta is my name", blocked only when that field is filled in); a handle after an ask is quoted with it (`withHandle`); `ambiguousAppWords` ("signal", "dc", "steam") only after a verb of talking, in a "we" frame with nothing after them, or where the post wants to leave the board (`leavingTheBoard`); an ask `reported` (below) is not the poster's, and nor is one the words after it name or take back (`askNotMade`, through `askMade` round `askCheck`, `numbersCheck` and `offBoardCheck` and on an ask `appFrame` finds: "is what" someone not "I" or "we" says, "send me a dm on insta is what every spam bot says"; "is a weird thing to ask"; unless the ask is made all the same after it, to the end of the sentence past a comma (`askedAnyway`: "…, but pls do"); or one of `takeBacks` or "i mean" and then a place on the board, `boardPlaces`, in the same sentence, with none of `negators` between, "jk, just post it here", not "jk not here", nor a place complained of (`placeComplaints`: "i mean this thread is dead") or posting here put second to the app (`postingHere`: "whatsapp me actually, easier than posting here"); "actually" is no take-back), while a contact detail is blocked however it is named; and `offBoardRule` + `offBoardCheck` (talking off the board with no app named, after a verb of talking said as an ask: "text instead of posting here?"; "instead" ending the ask only after a private verb to "me", "us" or "you", and not with a number on its line (`numberGiven`), which the number rules judge; "somewhere more private" after any verb of talking or before "we could talk"; "privately", "in private", "1 on 1" and "one on one" after a verb of talking, but not with or to someone else, "talk privately with my parents"; "somewhere else" and "elsewhere" only ending the ask and with no `boardWords` word on its line, not "can we talk elsewhere? this thread is for physics"; "go private" only said as a suggestion and ending the ask (`askMood`, `askTails`), not "lets go private colleges"; a bare "dm me" names no way to reach anyone and is not one; only `offBoardObjects` between them, so the talk is with the poster or the reader and not advice to call someone else, "call a helpline instead of posting here") |
| block, `422` | `id_number` | `aadhaar` (`aadhaarShape` and Verhoeff), `ssn` (`ssnShape` with dashes), or either shape with its label straight before it (`idLabelled`: `aadhaarLabels`, `ssnLabels` or "social security", `idFillers` skipped; the label is the evidence, so a mistyped check digit or spaces in place of dashes still count), `niRule` + `validNIPrefix` (capitals, or `niLabelled`); `aadhaarShape` takes a digit at a time with one kind of gap |
| support, `422` | — | `supportRules`, read in the poster's own voice in any tense rather than phrase by phrase: wanting, wishing or needing (`want`, `wanted`, `wanting`, `wish`, …) any of `crisisActs` ("i used to want to die", "i wish i could just disappear"), feeling like or thinking about any of `crisisGerunds`, `selfHurt` and its tenses without the "I", "no reason to keep living" and its variants, "if i" with any of `gone`, a "way out", stockpiling `medicines`, and Hinglish (first person included, "marna chahta hoon", "marne ke baare mein sochta hoon", "khud ko khatam kar lunga", "life khatam kar dunga", "ab jeene ka mann nahi" with no verb only before `hindiLifeTails`); filtered by `supportCheck`: first `aboutSomeoneElse` (the nearest person before the words, when it is someone else, Hindi pronouns included (`hindiOtherPeople`): "my friend keeps saying she wants to die"), `quotedNotSaid` (words `reported`, below, unless a report without quotation marks makes the poster the subject of what was said, `reportedOfOthers`: "my therapist says I'm suicidal"; a word explained, `glossed`; a kind of post named, `mentionedAsAKind`, "the kms jokes") `shrugged` ("kms" opening its sentence with a mishap told in the past straight after it, or "I wanted to die" after a moment named in its sentence, with a laugh, a 💀 or relief — "thank god", "phew", "it went through" — in or just after the sentence, and no serious cause, life itself or a time that makes it an episode, `episodeWords`: "last night", "again", "every day") and `jokingCause` (a cause named before "make me" with a laugh or a 💀 in the sentence, "this group project is going to make me kms 💀", not life itself or one of `seriousCauses` (family, bullying, a partner), and 😭, "bruh" and "ded" are not a laugh; a bare "i want to die lol" still counts); then by what opens the match: "if i …" needs other people's reaction in the sentence (`counterfactualCheck`: "nobody would care", not "in minecraft", "in the exam", "died my hair" or "for a week"), a "way out" no one else in its sentence without the poster, a want, a stockpile or a bare act needs the poster as its subject (`ownSubject`, reading back through `carriers` such as "been" and "started" to an "I" or the clause's start; `otherSubjects` and any other word turn it down unless "and" joins a second verb), thinking about or considering one of `crisisGerunds` needs no other subject (`someoneElseThinks`: a person or a pronoun, `otherThinkers` among them, a name with a capital, any word before a `thirdAux` such as "is" or "has", or any word before the finite "think" or "thought": "my mum is thinking about suicide", "is Hamlet thinking", "why do teenagers think about suicide"; "staring at the ceiling thinking about suicide" is still the poster's), a bare "self harming" or "cut again" needs to be done (`doneAgain`), stockpiling not to be of `pillContainers` or `forAPurpose` (one of `ordinaryPurposes` named: "for the trip", never "for when I'm ready" or "for tonight"), "hurt myself" not an accident or an activity named as one (`hurtByAccident`: "accidentally", "by accident", "on" one of `accidentObjects`, one of `activityGerunds` or `activities`, after a word of `hurtDegrees` or not, "less when i fall while skating"; "on my wrist", "using a razor blade" and "when I'm sad" are not), "suicidal thoughts" not a general statement (`genericVerbs`), and "any reason"/"the point" a `futile` clause; then the idioms: "die of embarrassment", "ready to die for my country" (not a death owed: "i deserve to die for what i did", "i should just die for being this stupid"), "cut off", "end it with her", "ending things with my boyfriend", "burning myself out", "live in the hostel", "going on to a PhD" and "keep going to tuition", through `livingCheck`; `kmsCheck` for "5 kms" and AWS KMS; `supportPhrasing` for the phrasings read with the post round them, the post's words read once through the field's `wordReading`: a past attempt with a past time after "end it" (`pastTimes`) and no relationship ended (`endingARelationship`: "with my boyfriend" after it, or a break-up, or a partner and a fight, in a post with none of `attemptSigns`) or an overdose of medicine (`pillWords`, `medicineNames`), "make it to" an age (`ageMarks`, not `clockWords`, `countNouns` or a clock time, `clockAfter`) with no school work in the sentence, "my last post" among `farewellWords` and no `breakWords`, and not on a thread (`threadWords`), "a plan and a date" with nothing after it, "I'm on the roof" with `jumpWords` and `nowWords` and no `jumpSports`, "top myself" not "up", and the Hinglish past hurt (not `hindiMishaps`), "kood jaunga" (not `winWords`), "sab khatam kar dunga" (`apologyWords`, not `hindiStudyWords`) and "is duniya mein" (`stayWords` and `hindiNot`); `supportSaid` for the phrasings as often said of something else, read by what opens them: an overdose just taken by the poster, ten or more if counted, with no `medRoutine` in its sentence; "ending it" and "gonna end it" by the poster with none of `endedThings` in the post, and not ending it "with" school work (`endsWithWork`: "with a final revision of organic chem"), nor with nothing after it in its sentence and a relationship in the 120 bytes after (`endsARelationship`: one of `relationshipEnds` or "different things", "I've decided to end it. He's been cheating for months"; none of `endItOthers`' pronouns, jobs or games, which follow a crisis as often); "been cutting" and a relapse the poster's, and "clean from sh" with a relapse in the post and the poster's "I" in its clause (`selfSubjects`) or only how long before it (`onlyADuration`: "3 months clean from sh and relapsed"); "by the time you read this" not before `travelWords`; a suicide note with no `writingWork` or `storyWords` in the post; "life isn't worth living" not after `otherOwners`; "jaan de dunga" not `forSomeone`, before it or after it ("is project ke liye"); "zindagi khatam" not after `hindiOthers`; sleeping pills not few (`fewWords`) or for a pain (`medRoutine`, `hindiPillRoutine`); then the idioms after the act: a cut that names what is cut (`cutObject`, `cutObjects`), a cut short or of a session (`cutShort`) or with hair or nails in its sentence (`cutsSomethingElse`, `hairAndNails`: "i want to cut again, my bangs grew out"), "hang myself" upside down or from a bar or the rings (`hangsFromABar`), dyed hair after "die" (`dyesSomething`), an overdose on anything but medicine, an accident named after it (`accidentAhead`), "drown myself in" anything but `waterWords`, "shoot myself a message" or "in the foot", all my pills (`pillObjectSet`) with `medRoutine` or taken "along" or "with me", "rope" with anything after it but `endItTimes`, `trailFillers` or `shrugs`, "want to sh" before a shell's command (`shellAfter`), "better off without me" in a team or a group (`memberships`), "sleep forever" or "hit by a bus" with school work and a laugh, "sleep forever" after an exam (`sleepUntil`, `examsSleptAfter`), "rather be dead than" or "dead last" (`rankOrThan`) or "dead tired" (`deadIntensified`) and a death with a name after it (`deathTrail`); and `inYourShoesBefore`, which leaves "if I were you I'd kill myself" to `goads`), and `supportRuns`, read by the same check: `checkedRuns` (`wordRun`s filed by their first word and read in Go over the plain view's words by `anyRun`, so a phrasing in many wordings costs the plan nothing) for "committing suicide" and "do suicide" felt, thought of, considered, planned or tried, "off myself", "neck myself" and "rope" wanted or planned, "sh" wanted and "sh urges" had (not "sh" run on into a shell's command or a censored word, `shellAfter`: "sh build.sh", "sh*t", "sh!t"), and all of `pillObjects` planned to be taken or swallowed; the plain view reads `spellingFolds` first (`foldSpellings`: "sucide", "suiside" and "suicid" as "suicide", "commiting" as "committing", "sh'd" and "sh-ed" as "self harmed", though not "sh'd" before one of `shouldWords`, "sh'd have", and "not" only before a verb, not a stretch of time, "i sh'd not long ago", "i sh'd not even 2 days ago" (`notLongAgo`), "sh'ing" and "sh-ing" as "self harming"), so every rule sees those spellings as the words they stand for; and `asksForAMethod` (`selfHarmMethodRules` + `methodCheck`, and `methodRuns`: "how to", "how do i" and a best or easiest way to "sh", "self harm" or one of the reflexive acts, not "cut myself a fringe" or "burn myself a cd" (`secondObjects`), a particle or a degree after the reflexive that `selfIdiom` reads as a phrasal verb or a degree, "cut myself off from social media", "cut myself off caffeine", "burn myself out completely", "hurt myself less when i fall", "cut myself down to one coffee a day" (`selfPhrasal` before "from", before "of" and what is left (`selfObject`, read before any hiding), before what is given up (`selfObject`), an activity, a manner after "out", or the clause's end; "down to" or "back to" an amount, `selfAmount`; either it or `selfDegrees` before a time, `selfIdiomTimes` (not "every time" or "every day" with nothing after it, `selfOften`), or a degree before an activity named in `activityGerunds` or `selfSports` with no `selfHarmObjects` or fire after "with" (`harmWith`: "hurt myself less playing football", not "burn myself less playing with matches"); not "cut myself up", "cut myself less", the harm hidden, how deep or a scar anywhere after it, a tool anywhere (`selfTools`; a knife or scissors in a clause of its own only without one of `toolUses` after it, `plainUse`: not "hurt myself less when using a knife to cut vegetables") or a place on the body before a clause of its own begins, `selfMarked`: "cut myself down my arm", "hurt myself out of sight", "cut myself out deeply", "cut myself off so it doesn't scar", "hurt myself less without getting caught", "hurt myself less when i use a blade", not "hurt myself less when i fall on my knees"; not the handling of a tool straight after it, `selfHarmings`: "hurt myself less using a blade", "burn myself out using a lighter"; and not, after a degree, the harm or "do it" anywhere, or the handling (`selfHandlings`) of a tool of harm or the body later in the clause (`harmHandled`, `selfHarmObjects`): "hurt myself less after cutting", "burn myself less every time i do it", "hurt myself less when using my nails", not "burn myself less when holding the kettle" or "hurt myself less when hitting the ball in cricket"), "drown myself in" anything but `waterWords`, or "shoot myself in the foot"; a search after overdose or self-harm not for one of `methodTopics`, "tips for self harm recovery"; "for me to die/overdose/not survive" and "i'd need to not wake up" need a question round them, `methodAsks` or `questionAfter`, so "time for me to die in this exam" is not one; "the most painless way to go" only with nothing after it; a lethal dose only for the poster (`selfWords`) or asked after (`doseAsked`: a question mark after it or a `doseQuestions` opener whatever weight or study word is near it, "lethal dose of paracetamol per kg?", "pharmacology question: lethal dose of paracetamol?", or no `doseStatements` after it; never a "why" that opens the dose's own clause or stands just before it ("explain why the lethal dose of paracetamol drops", though "why bother, whats the lethal dose" asks), nor a topic after a `studyLabel` that opens the sentence, a few words with one of `studyFrames` before a colon or a dash or opening on "for" before a comma, "For my pharmacology viva: lethal dose of morphine in opioid-naive patients vs tolerant ones", though the dose named bare after one, of a medicine and per weight at most, or for a person by age, weight or size (`forARecipient`, `doseRecipients`: "for a 45 kg person", "for someone my size"), is asked; a plain study word, "exam", "notes", "medicine", is no label; and a how-many or how-much question about a lethal dose is asked whatever label is before it, "for the tox exam, how many tablets of paracetamol is a lethal dose"); a how-many-pills question disclaimed, "asking for a friend", not with `medicalWords` or `ailments`; the questions opened short and read in Go so as not to copy a long tail into every planned branch: a cut asked where, or which vein, not of something else (`cutPlaces`), "what pills can kill you" only after "what" or "which" (`killNouns`), a height with a death in its sentence (`fallFatal`, `fatalWords`, not "fatal for an egg"), "how long does it take to die" with one of `dyingMethods` after it, "eat less than" 800 calories or fewer (`calorieWords`), cutting unseen (`cutUnseen`) after a `methodAsks` word, "how to stop eating" with nothing or `stopEatingTrail` after it, and `hiddenMethods` asked after "how to" only) |
| hold, `202` | `threat` | `threatens` over every field at once (a subject and a body pair as if adjacent): `threatIntentRules` (the first person with `threatModals` and up to two `threatAdverbs`, swear words and "go" among them; "someone should"; "i planted a bomb"; some Hinglish), `threatFrameRules` + `threatFrameCheck` (grammar rather than phrases, verb first so the search starts at a rare word: a verb of harm or of carrying in any form (`threatVerbForms`), `frameSpeaker` reading back from it for who says it ("I", "we", "imma", "I'm going to", "planning to" opening a clause, "on monday I", any verb after "tomorrow I", `frameAdverbs` skipped, "acid" among them; a clause opened by `bareHeld` or a greentext line, `greentextOpens`, or by one of `bareArmed` when what follows is a weapon: "come back monday with a full mag"), and `threatFrame` reading what it is done to with `readNoun` (determiners and owners, up to two describing words, then a head of a kind: `rolePeople`, `kinPeople`, a capitalised name or one with a title before or after it (`titledName`, `nameTitles`, `titlesAfter`: "Mr Sharma", "Sharma sir"), or after one of `lowerNameVerbs` a name in small letters that `lowerName` takes (not a word of any set the filter knows, `notAName`, a plural or an -ing form), `placeHeads`, `vehicleHeads`, `bodyHeads`, `weaponHeads` (and `magHeads` after `loadedWords`: "a full mag"), fuel; a head followed by `nounTails`, an event or a message is not one, and `nounEnds` stop it; besides, the shit beaten out of someone, `beatenOutOf`, someone's backside, `bum`, a bomb put or left in a place or one of `bombSpots`, `bombLeftIn`, a bomb made or built that is not one of `harmlessBombs` and not in a sentence of `bombMoney`, a kidnap that is not an invitation ("for coffee"), a hit that is a message ("hit you … with the details", or `hitMeUp` in the sentence when the clause after the hit names no force or injury, `hitHard`: "so hard", "till you bleed", "you won't walk", "in the face"), and "batter" or "thrash" said of no team), so "run over the PE teacher", "push the topper down the stairs", "put the warden in the hospital", "take a hammer to the warden's head", "bringing my dad's pistol to class", "going to the hostel with petrol", "light the staff room up" are one rule each verb rather than a phrase each, lighting a place up not counted for a room someone walks into, a stage, a court, a party venue or a home (`litUpPlaces`), a sentence with a show, lamps, a festival, a game, a camp, a decoration or an occasion in it (`litUp`, `campWords`, `lightUpWords`: "at the pep rally"), a lab and an experiment (`labHeads`, `experimentWords`), what the poster wears straight after the place (`wornClothes`: "in my new jacket", "wearing my kurta", not "wear fireproof shoes"), or a "with" that names no fuel, weapon or people ("with my outfit"), and none of those with a fuel in the sentence (`fuelWords`: "with petrol for my birthday"); turned down for a story or a dream in the sentence (`storyNamed`, `dreamWords`), a game for the verbs games use (`gameActs`), a prop, a toy or a festival beside a weapon carried, none of them denied (`unnegatedIn`: "this is not a game"), a blade on a camp or a trip (`bladeHeads`, `outdoorWords`) and a gun at a range (`gunHeads`, `shootingRange`), and exasperation with one person close taken back (`hyperbolicActs`, `retracted`: a refusal that is a clause of its own, not "I won't stop", and no "not kidding"; `closeOne`)), `threatComingRules` + `threatComingCheck` (harm said to be coming, with `threatSubject` reading who or what before it: a group picked out by where or who it is (`pickWords`, a class and section, `classSection`), all of them, or whoever is somewhere (`presenceCondition`: "if anyone shows up to the rally"); a place or a vehicle picked out that will burn, blow up or explode (`explodeRuns`, "is going to explode" among them, read in Go and paired through `anyRunNear`; not "explode" at news, `explodesAtNews`: a clause of "when", "once" or "if" that names news a clock time leaves news (`explodeClockNews`: an announcement, results, marks, rankings or a list put up, "at 10am when the results are declared") or, with no clock time after "explode", other news (`explodeNews`: a cutoff, prizes, prices or fees, something cancelled, a holiday, the wifi; a date sheet, an answer key or a report card, `explodeNewsHeads` after `explodeNewsKinds`; papers given or handed back, `explodeReturn` with `explodePapers`) or a happening a crowd cheers (`explodeHappenings`: something seen or heard, though not something unnamed on a day named, `explodeSees` with `seenUnnamed` ("it", or "this" or "that" with none of `explodeMedia` after it) and `soonWords` before "when" or after it ("tomorrow when he sees it", "when they see it tomorrow", not "when they see this lol" or "tomorrow when ma'am sees this meme"), a win, a show, food served, a fix, a resignation; something found out, come out or brought back, `explodeOut`, `explodeBack`; something read out, `explodeReads`; a six or a century hit, `explodeHits` before `explodeScores`; being let or told to go home early, `explodeLet` and `explodeSay`), and has no poster (though "we" after `explodeSay` is told: "says we can go home"), none of a bomb's timing (`explodeTriggers`), nothing hidden (`explodeHidden`, or "what's" before "in": "the bag", "what's in the locker", "what's in the hall", but not what is in print, `inPrint` with `explodePrints`: "what's in the yearbook", "what's in the news"), no gates or doors closing, place filling or "least expect" (`explodeTiming`), no assembly unless something is announced in it (`explodeAssembly`), and no people arriving, going in or somewhere (`explodeArrive`, `explodeGoIn`, `explodeBe`, read only with people as the verb's subject, `peopleSubject`, so "the results are in" is news); never with assembly before the clause, nor with a bomb or the poster arming one anywhere after "explode", `explodeArmed` (`explodeDevices`, a device among them, unless said in praise, `explodePraise`: "our dance is the bomb", "my results are a bomb", "a bomb of a day", "the atmosphere will be explosive", `explodeMoods`, not "there is a bomb" or "the package is a bomb"; the poster's `explodeRigs`, or `explodePlants` before "it" anywhere but where it is shown, `explodeShown` with `explodeShows`: "as my wallpaper", "on my story", "on the notice board", "up with the dj", not "i set it" or "in the staffroom"), which no stage or event in the sentence excuses either: "when the results come out, i planted it in the staffroom", "the hall will explode tomorrow, the bomb is under the stage"); people who "will pay" with nothing after it but a time, a place or "for it", "for this" or "for that" (`payRuns`, `payCheck`: the reader, all of them, someone picked out or whoever wronged the poster (`whoeverWronged`: "whoever broke my phone"), never the poster's own; not with a sum, a bill, a fee, a receipt or attention in the sentence, `moneyWords`, a thing bought, ordered or signed up for, `purchaseWords` read by `purchasePaid` (in a relative clause before the promise, "everyone who signed up for the trip", or a clause after it, the poster's own too, in the promise's clause or the next, with no way or place of hurting in the promise's clause (`payWays`: "with your life", "outside the gate", "at the gate") and nothing after it in the sentence or in the post's next `threatWindow` characters that `payMenace` reads as a threat's (a weapon, `prepWords`, `poisonWords` or what `fightSet` finds: "i already ordered the pizza. see you at 7", "i booked the turf", not "i bought the pizza, it's poisoned", "... bring a knife" or "... it's gonna be a bloodbath"), unless `purchaseArmed` finds a weapon, something to hurt, tie up or burn with (`prepWords`: rope, zip ties, chloroform; `plainPreps` with no ordinary use after them, `plainPrepUse`: gloves, tape, a mask, not "the gloves for the match" or "the charts and tape"), something left vague or a purchase "for you" in it that `payOrdinary` does not find ordinary (`payGoods` bought, `giftGoods` for "her" or "him", or a `payOccasions` word after the people, with only `payTrail` or more goods after: "the snacks for them", "a gift for her", "a cake for her birthday", "the pizza for you guys", "the cake for you and the candles") ("i bought a bat", "i ordered something special", "i bought a gift for you"); "got" only with `payGoods` after it (`gotGoods`: "i got the hoodies for everyone", not "i got suspended"); never "buy your way out", "in order of" or a sign-off's "signed"; and in a sentence where `fightSet` finds no weapon, death or fight's place: `fightDeaths`, "in blood", "your last birthday" (`lastRites`), "it's your last", "outside the gate"), or a thing broken that is no one's body, `damagePaid`, unless `fightSet` finds a weapon, a death, a fight's place ("behind the school") or, for the poster's own thing with nothing paid "for it", a fight's time ("after school"), after which the promise is held only with a time or a place near it; nor asked as a question in its own clause); a person "dead" at a time (`deadWhen`) or who won't see, survive or walk out of a day (the speaker's own only for a teacher or a classmate with no `prognosis` named, never family), "the weekend", "the morning" and "another day" being their own time (`threatUnseenRules`, paired with themselves); a class dead or teachers not surviving over exams, results, homework or a load of lessons with no weapon named (`schoolHyperbole`, `schoolWorkWords`); turned down for the speaker's own, people at large, one person as a caution, the habitual, a story, a dream, a game, a stage or an event not given a time (`untimed`), a harmless condition, a "by" that names the cause, and "with cheers"), `threatSymbolRules` + `threatSymbolCheck` (a weapon emoji beside a school one, not with a game, a toy or a festival), and Hinglish verbs with the subject left out (`hindiKill`, `hindiBody`, `hindiGuns`, `hindiPronouns`; washing, a lift, a DJ and a game turned down in `threatExcused`, a game also when it comes before "mein" or "pe", `hindiGameNamed`) near (`threatWindow`, counted in `readingPositions`: letters and digits, and one for each gap however it is filled, so invisible characters and emoji pad nothing) `threatPlaceOrTimeRules`, which are found first and once (`nearSpans`), so that a match with none near it is never checked (`anyNear`), `schoolShootingRules` (a school or mass shooting, the next school shooter or Columbine, a massacre at school) near `shootingTimeRules` (a time still to come, or a day), filtered by `threatCheck` (`threatExcused` first, for the phrasings that need their own reading: a passive harm ("gets stabbed", "will be shot") that is the poster's own (not a place, a vehicle or a person: `passiveSubject`, "our school is getting bombed"), killed in an exam (`schoolHyperbole`), by someone named, idiomatic ("shot down", "stabbed in the back", "killed off"), habitual, a story's or a game's, "gets" with nothing soon after it; "gets what's coming" and "pay" as karma; "end with" a visit; "nobody gets out alive" in a story; "everyone … is going to die" of an event or food; Hinglish that names a mosquito, a stage, a lamp, an exam or a toy, or "aag laga" about an event with no fuel named ("presentation mein aag laga denge"); a weapon held as a prop; a stage set on fire; a topic beaten to death), a part in a school play or a musical (`playedPart`: a play named in the sentence or the one before it, `namesAPlay`, with "drama" only of the stage, "our school drama" or "drama club", not "so much drama"; the poster's part named with a capital, `partNamed` ("I'm Brutus", "playing Burr", "cast as Tybalt"), or the act set on the stage in its own clause, `stagedIn` ("in act 3", "in the final scene", "every night"); nothing in that clause setting it off the stage, `offStage`: "for real", where the poster's day is spent (`offStageSettings`: "on his way home", "in his sleep"), "after" or "later" not on the stage (`onStageAfter`: not "later in the play", "after the senate scene" or "after he speaks"), a place that is no stage ("at school", "at her house"), or, unless the clause sets it on the stage ("on opening night" among them), a clock time or a date ("at 3pm", "on the 5th"), a time of day or a period (`offStageTimes`: "tomorrow", "next week", "this weekend") not said of the play straight before it (`playTime`: "in the school play next week and I stab Caesar"), "during" anything but the show, a part of it or the occasion the play is named for (`duringShow`, `showParts`: "during the final act", "during the dress rehearsal", "during annual day", not "during the break" or "during school"), "when" and a clause with no cue (`stageCue`: "when he enters", "when the lights go down", `stageSignals`, not "when I see him"), or a day of the week the play is not said to be on (`playDay`: "on monday", not "on Friday night" or "school play is on friday. … I stab Caesar on friday"); "tonight", "today" and "now" (`showNow`) are the show's once a part is named, though not for an act done to a stranger, a name that is none of the parts of `castNames`, a closed list (`strangerActedOn`: "I stab Rahul tonight", not "I stab Caesar tonight" or "I stab Roderigo tonight"; a part missing from the list is read as a stranger), for whom only a part of the show is a cue ("after he says one more word" is none); no real weapon), a lab blown up or set alight in a practical (`labMishap`, `labKit`: a practical, an experiment or kit named outside the lab's own name, no fuel or harm named, and `labOccupied` finding no weapon, fuel or people after "with", no name or class put in the lab after "with" or "while" ("with Priya in it", "with 10B inside", "while Rahul is in there"), and no place or people joined to the lab by "and" ("and then the whole school", "and the teacher"), where people followed by a verb, `labClauseVerbs`, are the subject of a clause of their own (a pronoun too, `labPronouns`, before a verb or one of `labTails`: "and they'll"), let off only as their answer to the poster's mishap, `labReaction` (the poster as its object or owner, one of `labReactions`, a school's penalty, `labPenalties`, a chore they "have to" do, `labChores`, staying back or a new lab: "and the teacher will kill me", "and the whole class will laugh", "and the whole class will get detention", "and the teacher will give everyone extra homework", "and the whole class will have to redo it", "and my parents will have to pay for it", "and ma'am will write a complaint to my parents"; never with what menaces them anywhere in it, `labMenace`, "too late", "what hit" or "pay" for what they did, `labPays`: "and the teacher won't know what hit her", "and the teacher will regret failing me", "and everyone will run but it'll be too late", nor with death or harm, `labDeaths`, "in hell", "who i am", hanging "from" one of `labHangPoints` (`labHung`), or a chore done to someone's body or life (`bodyHeads`, `labLives`): "and everyone will have to clean up the bodies", "and the teacher will get detention in hell", "and the teachers will finally know who i am", "and the teacher will be suspended from the ceiling", "and the teacher will have to fix her face"), and never when it says they are harmed or in the lab, `labTrapped` (`labHarmed`, `labStuck`, "in it", "there", "out" after a negation, "with it" after going: "and everyone will die", "and the teacher will be in it", "and the teacher won't make it out", "and ma'am goes up with it"); a name followed by a verb is held only for `labTrapped`, "and rahul will be locked in"), film and sports shoots, games, named or after a preposition (`gameNamed`: a title or a map within the sentence or the 64 bytes before it; never when the sentence names a real place, `placeNamed`, a word of `threatPlaceWords` that is not a time, as "after school" and "before class" are (`timeBefore`); after "at", "in" or "on" (`gameAfterAPreposition`), also `gameTitles` and the games whose words are verbs or seasons elsewhere, `playedGames` and `playedPairs` ("in Smash", "at laser tag", not "smash his face in" or "in the fall"), two-word names read in either view through `canonPairs`; the school sports of `sportWords`, "dodgeball", "netball", "rugby", only for a kill or a murder (`sportKill`) with the sport straight after "at", "in" or "on", not its ground, practice or changing room (`sportGrounds`) and with no weapon in the sentence, `sportAfterAPreposition`, said of the other side and not in earnest (`sportBanter`: "them", "you", "him" or the other team, never "everyone", and none of `seriousWords`, "for real" or "not a joke" in the sentence), and for nothing else `gameWords` excuses), but not for "there will be a shooting" or "a bomb", a condition that is not a demand (`harmlessCondition`: not "if you tell anyone", not "unless"; one that names no place excuses "hurt" too), a metaphor in an exam, the markets or the polls, a drill, hurt feelings and killing with kindness (`harmlessObject`), hyperbole such as "die of boredom", "die in the exam", "die when you see the results" or "when the results come out"); `namedTargetRules` with `namedTargetCheck` (a name with a capital, any after a title such as "mr" or "sir", or one in small letters that `lowerName` takes with the phrase ending after it; not an exam, subject, team, acronym or someone's occasion: "kill JEE", "kill Rahul's wedding"); `schoolShootingCheck` (a drill, an essay, a debate, `schoolShootingTopics`; the past, `shootingPast`; a massacre or a bloodbath in a sentence with an event, a game or a market); `threatWarningRules` near `threatWarningCues`; and `menace`, a threat told by implication: `menacePhrases`, each a `wordRun` read from its first word over the plain view's words (`wordReading`, `menacePhrasesAt`), are hints of a `menaceKind` (`menaceAfter`: an ambulance, a hospital or a body bag ahead, not after food, a run or the heat in its sentence (`harmlessCauses`), `nobodyLeaves` alive or home, the news, nothing left, someone's last one, `harmComing`, a place burnt to the ground or lit up (`ruin`, which exams do not make hyperbole; `lightingUp` and `litUp`), someone left inside, a crowd's last time (`lastCrowd`); `menaceReady`: what I need, in a bag or not, `enoughForAll`, a `listOfNames`, a `hiddenThing`, made sure of it, locked in from outside, people locked or trapped in a place (`lockedIn`: strong for a crowd, "them all", "everyone", locked in by the poster or by nobody named; not "us", not for a drill, a lockdown, an exam, a talk, a party, an event, a project or a time it ends, `lockedInExcuses`, an escape room, `escapeRoom`, or a purpose of study or attendance after "so", `lockedToStudy`: "so nobody bunks", though not "so they can't escape", or after "for", `lockedFor` ("for revision", "for the class photo"), and none of them in a sentence that sets something alight, `alightIn`: "for the fest and setting it on fire", or that names harm outright, `explicitHarm`: a weapon, people shot, stabbed, gassed or killed, nobody getting out or escaping, the poster never found out, which overrides every excuse); `menaceArmed`: a weapon, a club with a crew, a weapon emoji, a weapon brought in Hinglish (strong); `menaceVerdict`: done, happening, they'll learn, an `ultimatum`, `comingFor`, `hindiTarget` (strong); `menaceStayAway`: `stayAway` with no reason given, in its sentence or the ones either side (`stayAwayReasons`, `reasonNextTo`: "the canteen pizza on friday is a crime. stay away"), and no purpose after it (`stayPurposes`: "leave early to catch the bus"); `menaceWarned`: a "last chance" that is not one to sign up or buy (`notAnOffer`, `lastChanceOffers`); `menaceGrievance`: `wrongedBy`), turned down by their checks, by `excused` (a story, a dream, a game (`gameIn`; `notAGame` keeps "finals week", a "final warning" and a sports day out, as `gamesIn` does for `threatExcused`), a likeness (`likened`), `schoolHyperbole`, an event not given a time, the habitual, the poster as the subject) and by `reported`; two kinds within `threatWindow` of each other (`menacing`, which a grievance with only a warning is not) or one `strong` hint, within `threatWindow` of a place or a time, or with one in a hint's own sentence that `threatPlaceOrTimeRules` leave to the grammar (`placed`: `placeHeads`, `vehicleHeads`, `menaceTimes`, `menacePlaces`), hold |
| hold, `202` | `self_harm` | `selfHarmRules` filtered by `selfHarmCheck` (`calledOnSomeoneElse`: an act called in code as a method of someone other than the reader, "me.die();", while code is otherwise read as text; `saidFirst`: a bare "jump off a bridge" or "just end it" counts only as an imperative, not with a subject before it, though a condition on the reader then "just" opens it ("if ur that stressed just jump", `conditionWords`); `askedWhether`: a question that asks after the reader ("are you going to kill yourself?"), not one of when; `reported` and `mentionedAsAKind`: words reported or named rather than said ("he said 'just end it'", "the kys comments"); "take all your pills" with a routine after it (`pillRoutine`), not "at once"; "you should end it" and "just end it" with nothing but `shrugs` after them (`onlyShrugs`) and none of `endItOthers` in the 120 bytes before or after, nor a clause beside them that opens on what is ended (`namesWhatIsEnded`, `opensOnEnded`: one of `endItThings` first, or after a determiner that opens the clause or follows "of", "just end it already, this lecture is 3 hours long", "sick of this chapter, just end it", in a clause that speaks to no one, so "this exam was easy and you still failed, just end it" is held); "go starve" and "you should stop eating" with nothing but shrugs after them or "to death", and "stop eating fatty" (`fatInsults`) with nothing after it; "ja mar" not before "gaya" (`diedWords`) or "kha" ("jaa mar kha le pehle"); `notNegated`: a negation ending the same clause, as in "don't you dare kill yourself" or "a bad rank doesn't end your life", not one anywhere before it, and a warning, "careful" earlier in the sentence or "or" before the modal; idioms such as "die your hair" and "a hill to die on", games after the words or after "in" before them, and "self-" compounds, and "delete yourself from" a group); `selfHarmUrgeRules` filtered by `urgeCheck`, which read an act urged on someone the way the threat frames read an act: the act first (overdose or "od", rope, die, jump, jump in front of a vehicle, "do it", stop or quit breathing, existing or living, self harm, the reflexive acts, "kys" in its forms, `kysForm`), also read as `urgeRuns` for "commit suicide" and "do suicide" ("bro just commit suicide", "just do suicide yaar"); then `urgeFramed` reading how it is told: an order opening its clause with only fillers before it ("bro just rope"), "and" after a favour asked ("do us all a favour and overdose"), or a modal said to the reader ("you should", "you need to", "why don't you", "time for you to"), or a question of when (`askedWhen`: "when are you finally going to", "are you ever gonna"); "rope time", the act judged ("ending yourself would be the best thing", "suicide is the only solution for someone with your marks", "the world would be better if you ended your life") and the Hinglish ("zeher kha le", "apni jaan le le", "latak ja", "mar kyun nahi jaata") say who it is for in themselves; turned down by `calledOnSomeoneElse`, `notNegated`, `reported`, `mentionedAsAKind`, `glossed` ("kys = keep yourself safe?"), games, `poolWords` after "jump" (a pool, a trampoline, the long jump pit), "overdose on" anything but pills (`pillWords`), "just do it" without `deathTopic` round it (`deathAround`, where "die of embarrassment" and the other `dieIdiom` readings are no death), "latak ja" on a bus (`hangOnWords`), and anything after the act but `urgeTails`; `selfHarmMethodRules` filtered by `methodCheck` (a how-many question in either order needs a medicine named in its sentence, so "how many volts would it take to kill you" is not one); and `goads`, encouragement told in other words, read as `menace` reads hints: `goadPhrases`, each a `wordRun` over the plain view's words (`wordReading`, shared with `menace`) with its check — `inYourShoes` ("if I were you" and then the poster's own act, `selfAct`; `supportCheck` turns the same words down with `inYourShoesBefore`), `goneAsAFavour`, `wholeSupply` (said to the reader, `haveTo`, of medicine or of something unsaid, with `finishRuns` in the post), `openDrop` (`heights`, a nudge, no `safetyWords`), `goneForGood`, `tooLate`, `meantIt` and the Hinglish reflexive — and a knot emoji with a nudge (`nudgeRuns`, ending their sentence) and no `knotHobbies`; turned down by `reported` and `notNegated` |
| hold, `202` | `grooming` | any `ageQuestionRules` match (an age, a school year, where exactly someone lives, which area, hostel or room, an age or class said back as a question: "you're in 9th?", Hinglish "kaunse school mein ho"; a school year in any of its forms (`readerGrade`: "grade 6", "9th", "which class"), the reader's place (`readerPlaces`: "where exactly is your coaching", "which floor is your room on", "that's your tuition right?", "do you go to the…", "kis school mein ho", "8th mein ho"), the reader's area (`readerAreas`: suburb, estate, village, hall, a coaching or exam centre), a Nigerian school year ("JSS3", "which SS"), "which year 10 form", "u in class 8 or 9?", "which floor of the girls hostel is your room on", "what time does your school finish", "which gate do you use", Pidgin "where you dey stay", "kaunse saal mein ho", "u sound young", "what time do you get out", "tell me your class", "what school u at?" (the "are" left out), "u in high school?", "so mature for 13"; a statement of the reader's year after "when" or "by the time", or followed by a unit or `%`, is not one: "when you're in 12th you'll understand", "you're 15 minutes late") that `ageCheck` keeps (a question, or an age looked for as a number, `ageNumber` and `ageUnit` before a `youthNoun`: "any 14yo girls", "13-15 yo girls"; or a `youthNoun` looked for by an age or a class, "girls under 16", "any girls aged 14", "looking for a girl 13-15", "any class 9 girls"; with no "any", a school year and one of `soughtYouths` (girls, boys, graders: not guys, and students or kids, `pupilWords`, only with a way to reach the poster after them, `pupilsReached` (after "pls" or a "who" clause too, and past a comma: "class 7 kids who want free robux dm me"), and secrecy after that, `pupilsSought`: parents or other adults kept out whatever else is said, "just us" unless a group follows it (`pupilGroups`: "just us girls"), or teachers kept out, "don't tell anyone", "between us", a secret, a private chat, being alone or a meeting not in a school's or a group's place (`pupilPlaces`) with nothing they would study, sign up for or do as a group (`pupilErrands`: "secret santa", a club, a debate, notes; `teacherErrands`, a gift, a party or a plan for a teacher who is not kept out; a surprise in a post for a teacher, a classmate, a group, an occasion or a name, `pupilMates`, `pupilNamed`: "a surprise for ma'am", "a surprise for kabir, keep it between us", a name being a word `lowerName` takes, right after "for", none of `pupilNotNames` ("for free", "for cutie") and with one of `pupilNameEnds` or nothing after it) or with a lure beside it (`pupilLures` given to the reader, or after `pupilGivers`: robux, "i'll pay you", "i'll send you gifts", "i'll send gifts"; gifts only when no `pupilWe` gives them and no one but the reader is named after them, `pupilGiven`, `pupilRecipient`: not "we'll buy gifts", "we're giving gifts to the teachers", "i'll send gifts to everyone" or gifts to the class, the seniors (`pupilGiftGroups`), an occasion (`pupilOccasions`) or a name, but "i'll send gifts to everyone who dms", "to all of you" or "to the girls"; a surprise or "something special" for the reader, `pupilReaders` or "my juniors", `pupilReader`: "i have a surprise for you", "a surprise for my juniors"): "class 8 students dm me, no parents", "class 8 students please dm me, no adults", "class 7 kids dm me, i'll send you gifts, keep it between us", "class 6 kids dm me, i have a surprise for you, it's a secret", "class 7 kids dm me, it's a surprise, don't tell anyone", not "class 12 students dm me for jee notes", "class 9 students dm me for doubts, no parents needed", "class 10 students dm me, we're planning ma'am's birthday surprise, don't tell the teachers" or "class 12 students dm me, lets meet in the library tomorrow") after "looking for" or "searching for" (`seekingVerbs`), or else in the plural with a `seekingTails` word after them, a "who" only before what they are wanted for (`whoSought`: not "9th graders who went on the trip"), or in the singular with a way to reach the poster after (`reachTails`: "class 8 girl dm me"), and no article or `groupLeads` word before, "class 8 girls dm me", "9th graders hmu", "looking for class 8 girls", not "class 8 girls relay results", "the class 8 girls dm me every day", "grade 9 student here, dm me" or "class 8 girl here"; a `youthNoun` and a number in a sentence that counts more than one (`tallied`: "boys 13, girls 14") is a tally, not an age looked for; "our 15 year old students are doing well" matches too and is held only beside a cue; an age followed by an open age rule, `ageRuleAfter`, is not, unless the rule names an adult's age: "u 14? the competition is for 14 and above"; and "how old are you" followed by `generalOldNext` asks an age in general, "how old are you supposed to be to go out alone"; "how old" and "what age" after one of `askedWords` in their own sentence are reported (`askedBefore`: "she wanted to know how old are you"), but not after a sentence that ends in one, "your parents don't need to know. how old are you?") (or `aslRules` with `aslCheck`: "asl" opening what is said, not "bored asl" or "ASL club"; or `ageAndPlaceRuns` with `ageAndPlaceCheck`, "asl" spelled out where it opens what is said and ends it: "age and location?", "age/city?", not "age and location of the exam centre?") and any `privateCueRules` match (talking privately or alone, meeting, coming over, "just us two", keeping it quiet, parents not knowing, "akele aana", "kisi ko mat batana"; "come alone", "come without your friends", "wait for me", "meet me at", "I'll pick you up", "somewhere your parents can't see", "do your parents know", "your parents don't need to know", "delete this after", "let's go private", "keep it a secret", "ek baar milo", "gift dilaunga", "keep our chats between us", "do u walk home alone", "I'd love to see a photo of you", "come pick you", "kisi ko bolna mat"; "I'll buy you a gift", "I'll come to your school"; money as an amount, `lureAmount`, "i'll send you 500 rs", a bare number only with its currency (`lureGivers`: not "i'll send you 2 more notes"); a "dm" or "pm" said as a sentence of its own, "age? dm", or ending the post, but not the "pm" of a time (`hourWords` or a number before it: "starts at 3 pm"); "can i see a pic", which `privateCue` turns down when it is a picture of something else, "can i see a pic of the question"; a bare "privately" only beside an offer to the reader, and "waiting" only for them; "between us" only after `betweenUsLeads` ("just", "stays") and ending what is said; "tell no one" and "no need to tell your parents" only told to the reader (not after `tellers`) and ending what is said or saying it is about the two of them (`secretEnds`); "alone" opening a clause with a "?", or ending an order to come after a comma ("come to the chai stall after class, alone", `orderFillers` then `comeWords`); "walk with you" not "walk you through", "come up" not "come up with") that `cueCheck` keeps, or any `groomingCueRuns` run that `groomingCueCheck` keeps (a video call asked for with no app named, "lets vc", "can we vc", not with a group, `groupCallWords`; a "vc" said on its own, `vcAlone`, not "our vc club"; a camera turned on outside a lesson, `lessonWords`; "nobody will know", "promise you won't tell", "you won't tell anyone", with nothing after them but `secrecyTails`, not "nobody will know the answer") ("call me crazy", "not just you", "meet the deadline", "between you and me, the food is awful" and "seniors se baat karo" are not cues; nor is "do your parents know about" a thing, `personalAbout` naming what is not one; nor a `meetingCues` cue in a sentence with `publicPlaces`, `groupMeeting` or, for a visit, an `errand`, unless the sentence has `isolationWords`; `cueBesideAnAge` takes those meetings back when `asksTheAge` finds the reader's age asked) in the same post, or a `contact` block in the same post, which is the cue (`contactCue`, see the precedence below) |

Precedence is block, then support unless `support_seen`, then hold (threat, self-harm,
grooming), then publish, with one exception: a `contact` block in a post that also
places the reader (`placesTheReader`: an `ageQuestionRules` or `aslRules` match that
its check keeps) is not returned; it sets the private-contact cue `holdReason` reads
for grooming, so the post is held instead of telling the poster which half of the cue
to remove (`TestFilterPrecedence`). An `id_number` block a field finds first still
blocks; one after a contact block in the same field is not looked for, and is held
with the rest. A question about a method ("how many pills would kill me")
gets the support screen first and is held once the poster goes on, because the person
asking may be the one at risk (`TestFilterAsksAboutAMethodBeforeHoldingIt`).

**Reported speech is one reading, shared by every family** (`reported`,
`TestFilterReadsReportedSpeechAsSomeoneElses`). Crisis language, encouragement, threats
(every rule `threatens` pairs, through its `own` wrapper), both halves of a grooming cue
(`cueCheck`, `cueBesideAnAge`, `ageCheck`, `aslCheck`), method questions (`methodCheck`)
and asks to talk elsewhere (`askCheck`, `appFrame`) all turn down words the post passes
on rather than says: a greentext line quoting the post being answered
(`quotesTheReply`, unless the run of greentext lines it is in opens with "be me", "tfw"
or "mfw", `greentextRun`); a quotation, in any kind of quotation mark (`quotationAround`,
which counts straight double quotes on the line to tell an opening one from a closing
one, and reads curly single quotes as the apostrophes the view folds them to; an
apostrophe for letters left out, "'cause", "'til", "'09" (`elided`), and the marks of
feet and inches, "5'10\"" (`inchMark`), open nothing), whose speaker `quoteSpeaker`
finds to be someone else — a label ("teacher:", "his dm:", "my bully to me:"; whoever
comes after "to" or "dear" in a label is spoken to, so "note to my bully:" is the
poster's, and a label with a verb of saying in it, "he messaged me:", is read for its
verb, the last one unless it is in a clause of time or cause after an earlier one with
"this" for its object, `framingVerb`: "a stranger sent me this after i posted:" is the
stranger's), an attribution after a dash that names a person (`attributionSpeaker`:
"— some guy in my class", not "— should I tell someone?"), or a verb of saying, writing,
asking, sending or coming across within four words before it or six after it, whose
subject `speakerOf` reads back to (`otherSpeakers`, `messageNouns`, any word not in
`subjectSkips`), or that was said to the poster ("told me '…'", "was told",
`passiveLeads` + `participles`, so "'…' he's been texting me" and "'…' they've been
texting me" are theirs, and "'…' i've been told this" was said to the poster, while
"'…' i've been texting him this" is the poster's own; `heardVerbs`, `receivedVerbs`);
the poster's own clause after it giving the words as someone else's (`posterDisowns`:
come to the poster, seen or read, "'…' i keep getting messages saying this", "i found
this written on my desk", "i received this today"; refused before the verb of saying or
appealed against, "i'd never say this", "we need to stop saying this", not "i won't stop
saying this"; said by someone in a clause of "if" or "when"; or passed on to a parent, a
teacher or the mods, "i'll show this text to my mum"), the same clause with its "I" left
out ("got this again", `elidedReceipts`), a clause with the words as its subject
(`quoteReceived`: "this was written on my desk", "this keeps happening", not "this is in
my locker"), or someone else leaving them (`leftByOthers`: "someone left this in my
locker"), unless the place is the reader's ("this was left on your desk",
`readerOwners`) with no care for the reader after it (`readerAdvised`: "are you ok? tell
a teacher", "did you report it?", not with a dare, "tell a teacher, i dare you") or a
sentence after them that starts within `threatWindow` owns them, each read whole
(`ownedAfter`; `posterOwns`: "for you", not "here for you", a weapon, "and i'm bringing
it", "and i'll do it", "it was me", "by me", "this time it's real" or "for real", not
with the poster's fear or a call for help in any of those sentences, "it's happening",
"and i agree"; `posterCarries`: the poster, or no one named, carrying, using, taking or
making real what came, "bringing it tomorrow", "and i'm going to use it", "i'll make it
real", "make it a reality", `ownedDoing`; not "bringing it to my teacher", "bringing it
to sir", "using it in my complaint" or "using it as evidence", `handedOver`). For
crisis language a clause gives the words away only when it names who sent them or a
message (`sentByOthers`), so "'i want to die' this keeps happening" stays the poster's;
a quotation named as a kind of post (`namedAsAKind`: "the 'add me on snap' guys"); and
the same report without quotation marks (`reportedClause`: a verb of saying before the
words, in their sentence and before any comma, with only `reportedLinks` between: "my bf
told me to go kill myself", "he said he'll shoot up the school"). A verb that doubles as
a noun is the noun after `nounLeads` or a possessive (`sayingNoun`: "my post", "his
message"). The poster's own words stay theirs however they are wrapped: a label, a dash
or a subject that is the poster (`posterLabels`, `posterSubjects`, the poster's own
mind, `mindWords`: "my head keeps telling me to"), an instruction (`instructionModals`:
"you should tell him to kys"), a verb with no subject or only an adverb before it
("already told everyone '…'"), "you heard me", and a quotation no one is named as
saying. A source that is not a person names no one, so "news: '…'" and "'…' - police"
still hold. This is the rule the owner set: a student reporting a threat, a grooming
attempt or being told to kill themselves is not held for it, and a poster who wraps
their own threat in quotation marks ("'I'm bringing a gun tomorrow' - me") is. The cost
is that a threat put in someone else's mouth ("my friend said 'I'm bringing a gun
tomorrow'") is published; the README says so. Contact details are blocked whoever they
are said to belong to: only asks are read for whose they are.
**The cost is linear in the post, and small.** No pattern is run whole over a view
(`filtersearch.go`): each is split once into branches, one walk of a trie per view finds
every place a branch could start, and a copy of Go's own regexp machine, which never
backtracks, runs each branch from those places alone, putting no thread where nothing
can take the next character (`viability`, worked out per instruction when the branch is
planned; the machine finds the same matches with fewer steps). The number scanner and
the view builders are single passes, the checks read at most a clause or a sentence each
side of a match, cut to 64 bytes (`clauseAfter`, `sentenceAround`), or a window of a
fixed width (`checkWork` lists them: `ownedAfter`, for one, reads each sentence that
starts within `threatWindow` bytes after a quotation, to its end), or `readReach` words
for a hint of a threat told by implication or a goad (whose nudges and finishers are
read once for the post), and the threat pairing finds the places and times first and
looks each intent up among them by binary search (`nearSpans`, `anyNear`), so an intent
with no place or time near it is never put to its check, which is where the grammar's
reading costs. Which copy of its quote a block is about (`matchOccurrence`) is counted in
one pass over the field, the quote's words matched as a sequence of the field's runs
of words, and those steps are counted with the rest; searched for again one character
on from each copy, as the page does, a line of "a a a …" before a spelled-out address
of forty single letters took about 140 ms. `TestFilterCountsCopiesInOnePass` holds the
pass by what it costs rather than by the steps it reports: no more allocations, and no
more than eight times the time, for a copy at nearly every character as for a post of
the same length with copies only in its last line. Measured on one core of a shared
four-vCPU Intel Xeon @ 2.80GHz (`GOMAXPROCS=1` under `taskset`, no race detector, the
slower verdict's best of several runs, timed as the process's CPU time, with a subject
and a name; the machine's load about 4 to 6): ordinary posts are the corpus's published
posts run together a blank line apart and cut into pieces of 500 characters and of the
maximum length, as many as the corpus makes (`ordinaryPieces`), and take about 1 ms and
about 8–9 ms at the median piece (each piece's best of 11: 0.4–2.8 ms, and 6.9–13.9 ms
for the long ones). One sentence repeated to the maximum length, which
`BenchmarkCheckContent/sentence` posts, takes about 4.7–5.3 ms, since the same words
over and over give the rules less to read than varied ones. The figures move together
with the machine, so the sentence is their yardstick: the ordinary pieces take about a
fifth of its time and 1.5 to 1.8 times it, and the slowest built bodies 5.5 to 7 times
it. On the Intel Xeon @ 2.10GHz the earlier figures were taken on the sentence took
2.6–3 ms; runs here at a load of 11 to 30 gave the same medians as at 4 to 6. Of 29,397
bodies built to be slow — each rule's openings and `TestFilterCostIsBounded`'s units
alone, in pairs and stacked; method questions with their heads, marks, fillers and runs
of spaces varied and their words shuffled; a symbol or an emoji beside a letter, a digit
or a mark; and runs made to strain the scanners, each repeated to the maximum length —
each timed at the best of two runs and the slowest 120 again at the best of 15, the
slowest take about 29–33 ms: a weapon emoji and a mark or a letter over and over ("🗡'",
"a🪓", "9🔪"; each emoji is a hint of a threat, read for the sentence round it and for
whose words it is), a weapon named over and over beside a day ("gun gun 10k gun gun gun
gun sunday "), and method questions stacked before an app's tagged name.
`TestFilterCostIsBounded`'s 153 bodies take about 0.9 to 31 ms at the best of 11, the
most "stacked method questions" and then Snapchat's ghost before a short name, about
25 ms (about 47 ms before `appFrame` kept its words). Before the searches were planned
the typical post, the repeated sentence and a post of `㏂`, which folds to
`maxFoldRunes`, took about 4 ms, 36 ms and 400–490 ms on the earlier machine.
`go test -run - -bench CheckContent ./internal/app` times both kinds of maximum-length
post as wall time, GC included. Refusals are budgeted all the same; see the API contract.
Planning takes about 5–5.5 s of one core, once per process (5.0–5.2 s in six fresh
processes under `taskset` at a load of about 4 to 5, 5.3–6.5 s at 11 to 20), and keeps
about 65 MiB of heap; while it runs the heap reaches 270–300 MiB with four cores and
330–370 MiB with one
(`runtime.MemStats.HeapSys`, three and five runs on a shared machine), and the runtime
gives the difference back to the system slowly: a server left idle after it started stayed
at about 350–400 MiB resident for its first two to four minutes and then fell to about
160–205 MiB (350–396 and 160–204 MiB over two separate measurements on a shared machine of
four vCPUs, with `GOMEMLIMIT=400MiB` and without; in the later one the drop came at four
minutes). The server's
512 MiB has to hold that beside everything else (see the upload budgets below). `Routes`
starts it in the background, and a post that comes first waits for it. The plain view is
skipped when the loose one is only it with `l` read as `i` (`looseOnly`). What holds the
cost down:

- `TestFilterCostIsBounded` counts work rather than time (`checkContentCost`: the
  places tried, the characters read, the machines' steps, and each check a match is put
  to, `checkWork`), so it passes or fails alike on any machine and under `-race`. The
  bodies built to be slow may cost at most `workPerByte` (160) steps per byte of the
  post (the most, of its bodies, is a method question asked three times and then an
  app's name with a tag in it, "how many to od how many to od how many to od i[n]sta "
  over and over, about 150, so the bound is about 7% above it, and that is also the most
  of the 29,397 bodies built to be slow the counts were last checked on; the next are
  handles glued to an opening bracket and pointed at an app, "handles to brackets",
  about 127, and a pill question run into such a handle, "howmany@pills_(snap ", about
  116; the steps are the same with Go 1.22, 1.24 and 1.25), at most `workPerChar` (240)
  per character, so that a post of four-byte characters is not given four times the
  steps a character an ASCII one is (the most is Snapchat's ghost over and over, each
  read for an ask or a handle round it, about 193), and eight times the text at most ten
  times the steps. Its bodies include what made searching quadratic before:
  spaces between the places a run can end ("addresses far apart", "arrows far apart",
  read once now, since `nextRun` steps over what no run is made of and `runOf` keeps
  every run it measured), weapon emoji, which all stand at one reading position (a run
  of them is one hint, and `menace` pairs hints by binary search and counts it), a pill
  question whose medicine ends no word, and handles glued to an opening bracket, which
  every handle rule finds in both views and turns down by the bracket: `first` charged
  each the clause each side its check is counted as reading, up to about 198 steps a
  byte, and `firstHandle` turns one down before its check, charged the match it read. A
  pattern whose `~` runs straight into a stretch of any characters, `~[^.?!\n]{0,60}?`,
  keeps a copy of the stretch for every space of a run before it, which cost the pill
  questions up to about 300 steps a byte; `window` starts the stretch at a letter or a
  digit, matches the same text, and keeps one. An address rule keeps its name's first
  piece a group of its own, one character and a bounded run, so that it is planned as a
  run (filtersearch.go) and looked for from what follows the name; `spelledLocalEmailRule`,
  with the name's spelled dots inside that group, was tried with every length of name at
  every letter, which cost a post of "㏂" ("a.m." over and over) four times the steps,
  inside both bounds, and half as much time again (`TestFilterAddressNamesAreRuns`).
  `classifyNumber` asks `mathsAnswer`, which reads 64 bytes either side of a number,
  last, of a number it has found to be a phone, Aadhaar or SSN number: asked of every
  number that stood alone, it cost posts of helpline numbers, ranges and scores nearly
  twice the steps and from 1.6 to 1.8 times the time, inside both bounds
  (`TestFilterNumbersAreReadForAnAnswerLast`). A step is not a fixed time: `checkWork`
  charges a check 128 bytes, and the checks that read wider fixed windows (its comment
  lists them) are not charged the rest, so across those of the bodies that take more than
  a few milliseconds a millisecond is about 18,000 to 91,000 steps (one with next to
  nothing to search is mostly the fixed cost of a post), and the README's timing is
  measured, not derived from the bound. What the count cannot see is held otherwise:
  `appFrame` keeps the words it reads from one app's name to the next, so a post that
  names an app every other character ("👻Sc" over and over) does not build them anew each
  time, which took about twice the time at the same steps (`TestAppFramesReuseTheirWords`
  holds it to its allocations). A new rule that trips the bound is planned or rewritten,
  not given a bigger bound; add the body that tripped it to the table.
- `TestFilterSearchesMatchTheirRegexps` and `FuzzFilterSearches` hold every search to
  what Go's regexp finds with the same pattern, groups included, on every view, and
  `TestFilterSearchesAreAllPlanned` fails if a pattern cannot be planned and would run
  whole. A new rule's pattern needs nothing but to be written. A check may still read
  a Go regexp over a bounded stretch round a match (32 or 64 bytes, or the match
  itself), as most do; one large enough to cost something each match, an alternation
  of many phrasings, is made with `newWindowSearch`, as `conditionalPlace`, the
  medicine names and the number scanner's cues (`contactCue`, `phoneCue` and the rest)
  are, so that it is planned and counted like the rest.
- What planning keeps and how far its heap climbs grow with the patterns. Every branch
  compiles its own copy of what follows its start, so the planner splits an
  alternation only when that tail is at most `maxSplitTail` instructions (the threat
  patterns' subjects, "i will", "someone should", sit in front of the whole list of
  acts), shares one copy of each class's ranges among the programs (`shareRunes`), and
  drops each pattern's tree once it is planned (`forget`). `TestFilterPlanIsBounded`
  holds the compiled programs to 800,000 instructions (about 798,700 now; when it
  was 570,000, the same patterns split without the bound came to 1.4 million). The grammar rules
  keep their patterns small by matching the rarest word, the verb or the harm, and
  reading the speaker and the object in Go (`frameSpeaker`, `threatFrame`,
  `threatSubject`); a pattern that starts at "i" or "the" and spells out what follows
  costs more steps and more instructions both. `threatActs` lists each act verb by verb and `joinActs` puts
  the verbs that take the same people or places in front of one copy of the list, except
  where two acts could start at the same place; `TestJoinActsMatchesTheActsOneByOne`
  holds the joined alternation to the acts listed one by one.

**Student-ID formats** (2.9's "configurable student-ID formats") are not built; the
extension point is the comment at the end of `filter.go` — a validator beside `aadhaar`
or a `plainOnly` rule beside `niRule`, loaded from configuration, with its own
`findingKind`. A new rule needs true and false positives in `filter_test.go`; the false
positives are the harder half, and the table there (maths, dates, ISBNs, scores, prices,
bare app names, "this exam is killing me", history questions, Hinglish and Hindi) is the
floor. The rules read English, with some Nigerian Pidgin ("I don tire for this life",
"I wan die", "where you dey stay"), and some Hinglish (Hindi in Latin letters: "insta pe add karo",
"mujhe marna hai", "goli maar dunga"); Devanagari and other languages are not read, and
the README and the FAQ say so.

**Be honest about the limits**, in code, docs and copy. Keyword filters are easy to get
round (a number in words other than digit by digit, a separator `groupSeparator` does not know, pieces with
no `callCue` or `evasionCue` beside them, a bare username or one pointed to in words other than another field of
the post, contact details inside `[math]` or in base64, an email under a TLD not in `emailTLDs` (unless it is an
academic second-level domain, `academicSecondLevel`: "edu.tr", "ac.il"), any language
the tables are not written in) and they have false positives (gaming trash talk held as
self-harm, a countdown blocked as a phone number, ten digits after "cell" blocked as a US
number, a dose question held, a twelve-digit reference that passes the Verhoeff check, a
joke about dying with no cause or laugh beside it shown the support message). That is why a block quotes exactly what it
matched and a hold waits for a person.

**The corpus** (`testdata/filter_corpus/{academic,life,contact,risk}{,_2,_3}.jsonl`) is
4,371 made-up posts in three parts, one JSON object a line: `id` (unique across the
files), `body`, `subject`, `name`, `expect` (`publish`, `block`, `support` or `hold`; a
method question that shows support and is then held meets `hold`), `why`, and, only
together, `known_miss: true` and a `note` saying why the miss is accepted rather than
fixed. `TestFilterCorpus` runs each post as the handlers would (`normalizeSubject`,
`displayedName`, `normalizeBody`, both `support_seen` verdicts) and fails on an outcome
that is not its label, on a known miss that now comes out right (take the mark off), and
on an unknown field or a repeated id; with `-v` it logs per file how many ordinary posts
were stopped and how much of each kind was caught. To add a post, append a line to the
file for its kind with the next id. The text is invented, never copied: no real person's
number, address, handle or invite, and no name with a real school (the public helplines
are the exception, as in `helplineNumbers`). Every address, username and profile in it
says "madeup" — in the local part, the handle or the path (`madeup.rohan2008@gmail.com`,
`@madeup_organicchem`, `instagram.com/madeup.sketchy`, a Name field's `madeupmint#k92`),
in letters that still read "madeup" when the post spells it out or splits it with markup —
or is at a domain RFC 2606 reserves, and a student number at an institution's domain moves
to a made-up one (`s2214587@madeup-uni.ac.uk`), so none can be a real person's.
`TestFilterCorpusDetailsAreMadeUp` holds the corpus, the addresses in this package's
tests, and the examples the README, this file and the filter's comments quote, to that
("snap: x_y_z" and "name#1234" stand in for a name and need no "madeup"); a group's invite
code and a Session ID are random and name no one, and stay as they are, while a Steam
friend code is the account's 32-bit number, so the corpus's is above 4,294,967,295 and no
phone number's shape ("4410877305": no Indian mobile starts with 4, and no North American
exchange with 0), so it can be no one's account or phone; the test reads each Steam code
in the corpus and the tests on its own to hold that. A number cannot be marked so. In the
first two parts the UK, US and Australian phone numbers come from the ranges set aside for
drama, which the filter passes, so a post meant to be blocked uses an Indian or other
number instead; in `_3` a number meant to be blocked is ordinary digits in a valid shape,
and the three its writers took from a drama range or a voided SSN were regenerated so, as
their `why` says. A North American number meant to be blocked is put in an area code the
numbering plan never assigns (N9X, as 495 or 294; 37X and 96X are held back too), which
the filter blocks like any other and which can be no one's, in the unit tests as in the
corpus ("(495) 867-5309"); `TestFilterCorpusDetailsAreMadeUp` holds both to that, or to
the fiction range or a toll-free line. A number meant to be blocked is never given with a
name, a school or a handle; a tripcode secret holding a mobile uses a drama number, an
exam roll, application, hall ticket, registration or student number the filter should
publish has zeroes for its serial ("Roll No 2304000", "21BCE0000", "my roll number is
21CS0000", both ends of a range: "22CS1000–22CS2000"), and an Aadhaar number fails its
check digit wherever its label alone gets it blocked (`TestFilterCorpusDetailsAreMadeUp`
holds the corpus to both, so no issued number can be among them; "my e-aadhaar says …",
which no label reads, needs a valid one). An invented number or ID in a valid shape may
still happen to be someone's, as with the unit tests' examples. Label a post by what the
filter should do, not by what it does. A rule change that turns a labelled post wrong is a
regression to fix, or a label to correct with the reason in `why`; a `known_miss` is for a
miss whose fix would cost more than it buys, and its `note` says what. **The rules have
been fitted to all 4,371 posts**, all three parts: the `_2` part was written apart from
the rules, then used to rewrite the hold rules to read grammar and, after that, the
contact and support rules to read verb classes, apps with their objects, the poster's own
voice in any tense, number layouts and quotation; the `_3` part was written apart from
them too, measured unseen (the README gives the figures), and then used to put right the
ordinary posts it stopped and to widen each family cause by cause; the last 1,486 pin
what a review of the rules found they missed or stopped wrongly, each written beside the
change that put it right; so the rates `-v` logs describe how closely the rules fit these
posts and are no measurement of anything else. The README says so, and a claim about how
well the filter works needs posts it was not fitted to. Relabelled posts carry the reason
in `why`: drama-range and 555-01xx numbers labelled `block` were relabelled `publish`, as
was a WhatsApp channel link (one-way, so no private conversation), as the README's rules
have them; and two lethal doses asked for under a study label ("for the tox exam, how many
tablets of paracetamol is a lethal dose for an adult") were relabelled `hold`, since a
label lets through a statement, a "why" or a topic, never the dose asked. In `_3`, a bare "inbox me" was relabelled `publish` (it names no way to reach
anyone, as a bare "dm me" does not), and each doubt about a label that was settled the
other way says so with "kept": a Zoom meeting ID with its passcode and a Meet link stay
`block`, a plan or a time in the poster's own voice stays `support` (only a method
question goes on to a hold), and "kms" over a mishap got over stays `publish`. Twelve
known misses are left, each with its note: an address in base64, a number given as a sum
in `[math]`, a request in Bengali, eight risky posts whose harm is implied or written as
code, and a crisis post that never names the act.

**API contract** (`refuseContent` and `heldResponse` in `handlers.go`). Both posting
endpoints take `support_seen` (JSON boolean; multipart through `isTruthy`). The check runs
after the body, subject and badge checks, after `ReplyRefusal` for a reply, and
**between the two halves of flood control, and before the upload is stored**:
`notCoolingDown` first answers `429` for a post that a cooldown would refuse, without
reading it or starting anything, so a client in a cooldown cannot use the rules to test
text; then a block or a support refusal starts no cooldown and stores no file
(`TestRefusedPostsCostNoCooldownAndStoreNoFile`); then `allow` checks again and starts
the cooldowns for a post that is to be stored, so a held post takes the cooldown like
any other (`TestHeldPostsTakeTheCooldown`). A refusal
is counted instead, against `refusalBudget` (`ratelimit.go`), keyed on `clientIP` across
every board: once a client has `EDUCHAN_REFUSAL_LIMIT` (10) in a minute, `refuseContent`
answers its posts `429` before reading them, clean or not, until the minute is up
(`TestRefusalsAreBudgetedPerClient`). It is swept with the cooldowns, so it holds an
address for at most its window plus the sweep interval. Badged and
unbadged posts are filtered alike (`TestBadgedPostsAreFilteredAlike`).
- block: `422 {"error": <one sentence naming what to remove and where>, "outcome":
  "block", "rule": "contact"|"id_number", "field": "body"|"subject"|"name", "match":
  <the source text, trimmed, at most 80 characters (`quoteSource`): a longer one is cut
  and ends in "…", and the client's `pointAtMatch` then selects to the end of the run>,
  "occurrence": <how many times the page's search finds the match earlier in the field
  (`matchOccurrence`), so it selects the copy refused and not an allowed one before it>}`
- support: `422 {"error": supportMessage, "outcome": "support"}`
- hold: `202 {"held": true, "board": slug}` plus `"thread_no"` for a reply — never the
  rule or the post number.

**Nothing the filter reads is logged**: not the text, not the match, not the rule. There
is no "post held" or "post refused" log line, but `requestLogger`'s line (method, path,
status, duration) is written for these requests as for every other, so a `422` or `202`
on a posting route does show that a post there was refused or held; a refusal also
counts against the address in `refusalBudget`, in memory. A held post is stored with
`posts.held_at` and `posts.held_reason` (the rule's name), and a held opening post sets
`threads.held_at` too; nothing else the filter matched is stored, and approval is logged
like every moderation action.
`TestTheFilterLogsNothingItRead` pins exactly those records and their keys, so a new
field on either is a decision; the README says the same, and has to change with it.

**Held posts** (`0009_held_posts.sql`). `posts.held_at` and `posts.held_reason` are set
together (`posts_held_together`; reasons `threat`, `grooming`, `self_harm`), and
`threads.held_at` mirrors the opening post's, so a held OP holds its thread and every
listing stays a query of `threads` alone. The invariant: **a held post appears in no
public view and changes nothing public but its post number.** The number is taken when
the post is stored (`nextPostNo`, which is also what orders posting before any thread's
lock), so the next published post skips it, and `/posts/{no}` answers it exactly as it
answers a post a moderator deleted (`TestHeldPostsAppearInNoPublicView`); only the
timing differs, a gap that appears at once and is filled if the post is approved. Every
public query filters it — the index
count, list, `opPosts` and `lastReplies`; the catalog; the archive; `Thread()` (thread
and posts); `ThreadByID`; `PostThreadNo`; both directions of `attachQuotes` — and a held
reply leaves `reply_count`, `image_count`, `last_post_at` and `bumped_at` alone. A held
thread is unlisted, `404` to GET, replies (`ReplyRefusal` and `CreateReply`) and
sticky/lock, prunes nothing when created, does not count against `thread_limit`, and is
never archived (`threads_held_not_archived`) or swept; `SweepArchived` also skips an
archived thread with a held reply. Such a thread must not outlive the threads archived
with it in public, or its lasting would say a held post was in it: the sweep first calls
`ExpireHeld`, which sets `threads.expired_at` (`0011`) on exactly the expired threads it
is about to skip, and every public read filters `expired_at IS NULL` as it filters
`held_at` — `Archive`, `Thread`, `ThreadByID`, `PostThreadNo`, `ReplyRefusal`,
`CreateReply` (both `404`), both directions of `attachQuotes`, and `MediaPublic`.
Approval does not bring it back; a later sweep deletes it, the first that finds it past
its board's `archive_days`, which may have been raised since it expired
(`TestHeldReplyKeepsItsThreadFromTheSweep`, `TestARaisedArchivePeriodKeepsADecidedExpiredThread`). The image cap counts published posts only, so
approval can take a thread past it. `TestHeldPostsAppearInNoPublicView` walks every
public endpoint with a positive control (published text is found, and the held text is
found once approved); a new public query has to decide whether it means held posts, and
almost always it does not. `threads_board_public_order_idx` (live and not held) replaces
`threads_board_live_order_idx`. The migration is numbered `0009` because the
strip-metadata branch adds `0008_attachment_names.sql`. A rollback across `0009` is
unsafe for the same reason as one across `0005`: older code ignores `held_at`, so every
held post and thread is public and takes replies. `deploy/README.md`'s rollback warning
does not say so yet; the README's **Held posts** does. The same goes for `0011`: older
code shows the expired threads again.

A held post's image is gated where it is served: `serveMedia` asks `MediaPublic` (the
`0010` indexes on `attachments.original_path` and `thumb_path` keep that one lookup per
request), which is true only for a file an attachment row names whose post is published
and whose thread has not expired, and otherwise answers `404` with `Cache-Control:
no-store` unless `isModerator(r)` — the moderation key, read by the same code as
`authorizeMod` — in which case the file is served `no-store`, never `immutable`, because
the URL turns public on approval (`TestHeldImagesAreServedOnlyToModerators`). A file no
row names is withheld too: between a sweep deleting a thread's rows and removing its
files, or after a removal the store refused, a swept thread's images must answer as an
expired thread's do (`TestFilesWhoseRowsAreGoneAreNotServed`).

**Moderator queue**, behind `authorizeMod`: `GET /api/held` (`{"posts","total"}`: threats
first, then oldest first, so a backlog of other holds cannot bury one; at most
`heldQueueLimit` = 500, with `total` counting every held post; board, thread_no, no, is_op,
held_reason, created_at, subject for an OP, name, tripcode, poster_id, body, badge,
attachment;
`TestHeldQueuePutsThreatsFirstAndCountsEverything`) and `POST
/api/boards/{slug}/threads/{no}/posts/{post}/approve`. `ApprovePost` takes the board's
row before the thread's (`TestApprovalRacingTheArchivingThreadDoesNotDeadlock`), clears
the held columns, and recounts (`recountThread`): an approved reply is not bumped
(approval can come hours late); an approved OP goes live with `bumped_at = now()` and the
board is pruned in the same transaction, as `CreateThread` prunes. A post that is not
held is `409 post is not held` (`errNotHeld`). Rejecting is the existing `DELETE` routes,
which work on held posts and threads; `DeletePost` recounts rather than decrements, since
a held reply was never counted.

```bash
EDUCHAN_MOD_KEY=dev-mod-key EDUCHAN_POST_COOLDOWN=0 make run
curl -s -X POST localhost:8080/api/boards/meta/threads/$TNO/posts \
  -H 'Content-Type: application/json' -d '{"body":"add me on snap"}'      # 422 block
curl -s -X POST localhost:8080/api/boards/meta/threads/$TNO/posts \
  -H 'Content-Type: application/json' -d '{"body":"kms"}'                 # 422 support
curl -s -X POST localhost:8080/api/boards/meta/threads/$TNO/posts \
  -H 'Content-Type: application/json' -d '{"body":"kms","support_seen":true}'  # 201
curl -s -X POST localhost:8080/api/boards/meta/threads/$TNO/posts \
  -H 'Content-Type: application/json' -d '{"body":"how old are you? dm me"}'   # 202 held
curl -s localhost:8080/api/held -H 'X-Mod-Key: dev-mod-key'             # the queue
PNO=$(curl -s localhost:8080/api/held -H 'X-Mod-Key: dev-mod-key' | TNO=$TNO python3 -c \
  'import os,sys,json;print(next(p["no"] for p in json.load(sys.stdin)["posts"] if p["board"]=="meta" and p["thread_no"]==int(os.environ["TNO"])))')
curl -s -X POST localhost:8080/api/boards/meta/threads/$TNO/posts/$PNO/approve \
  -H 'X-Mod-Key: dev-mod-key'                                            # publish it
curl -s -X DELETE localhost:8080/api/boards/meta/threads/$TNO/posts/$PNO \
  -H 'X-Mod-Key: dev-mod-key'                                            # or reject it
```

**Not built**: a held image stays at its ordinary random key (served only with the
moderation key, as above); build plan 2.2 moves held images under a `quarantine/`
prefix. Nothing pages an on-duty moderator when a threat is
held (2.11); the queue has to be read. The page handles all three answers in the
posting form (see **The content rules' answers** under Front end conventions).

### Attachments

- One image per post. JPEG, PNG, WebP, and GIF are accepted, decided by magic bytes rather
  than by the filename or the client-supplied content type. `media.Inspect` checks the
  format and the header's dimensions without decoding (and for a GIF the frames' total
  area and count, `gifFramePixels`, a walk over the block structure), and estimates the
  memory processing it will take (**Memory** below); `Process` runs it first, so the early
  refusal in `parseSubmission` and the full one in `media.Save` cannot disagree about a
  file's header. The GIF walk reads every block exactly as `image/gif` does — a graphic
  control extension is a fixed 6 bytes and a plain-text one a fixed 13 before its
  sub-blocks, whatever their size bytes say, and an application one starts with its own
  length byte — because a walk that read one extension differently from the decoder lost
  its place there and counted none of the frames after it, which the decoder went on to
  decode. It refuses (`ErrCorruptImage`) a file it cannot follow to the trailer or to the
  end of a whole block, and one with an extension Chromium ends somewhere else
  (`gifFrames.apart`): a plain-text one whose block size byte is not 12, which Chromium
  reads as a sub-block's length; an application one whose block size byte is 0, which
  Chromium ends at once; and a NETSCAPE2.0 or ANIMEXTS1.0 one whose first sub-block is the
  terminator, which Chromium takes for the loop sub-block before reading on. Past any of
  them Chromium meets a byte that starts no block and shows only the frames before it,
  while `image/gif` decodes the rest, which would be stored and shown
  (`TestGIFExtensionsChromiumEndsElsewhereAreRefused`). Two kinds of file browsers play
  and `image/gif` refuses are read as browsers read them, and `sanitizeGIF` has the
  decoder read them so too (`gifReader`): one that ends after its last block without the
  trailer, which gets one, and one with a frame after the first reaching past the canvas,
  for which the decoder is told the screen takes in every frame, and of which `playGIF`
  plays only the part on the canvas. A frame wholly off it draws nothing, so what shows
  for its delay is what the frame before it left once disposed of: after a kept frame
  (disposal 0 or 1), that frame's picture, into whose delay it folds, as any frame that
  changes nothing does; after one restored to the background (2), the canvas with that
  frame's area cleared; and after one restored to the previous picture (3), the canvas as
  it was before that frame (`TestGIFIsStoredAsBrowsersShowIt`). A first frame past the
  canvas is refused: Chromium widens the canvas, Firefox moves the frame to the corner.
  `TestGIFFramePixelsReadsWhatTheDecoderReads` and `FuzzGIFFramePixels` hold the walk to
  the decoder: whatever `gif.DecodeAll` decodes from what `gifReader` makes of a file, the
  walk must have read exactly its frames, their area, the largest of them (charged again
  for an interlaced frame's reordering, as the canvas was) and, short of an ANIMEXTS1.0
  extension, its loop count; and whatever it decodes of a file the walk refused is a
  disagreement, but for a file refused at an extension Chromium ends elsewhere. Change the
  two together. Where the decoder reads a frame's fields otherwise than Chromium, the
  frames are played from the walk's record of each (`gifFrameHead`, from `walkGIF`, its
  colours through `gifFrameHead.keys`) and take only their pixels from the decoder: a
  frame with no graphic control extension has disposal 0, where `image/gif` carries over
  the last one's; its transparent index is the last extension's alone, where `image/gif`
  keeps one an earlier extension before the same frame named; and an index past the end of
  its colour table is opaque black but for the transparent one, where `image/gif` makes
  all of them up to a transparent index past the table transparent. Two more kinds of file
  browsers play and `image/gif` refuses are refused by the decoder, in `Process` and so
  after flood control (`image could not be decoded`): one with a frame whose pixels index
  past their table, but for indices up to a transparent index that is itself past it,
  which `image/gif` takes, where Chromium shows those pixels black; and one with more data
  after a frame's LZW end code than `image/gif` takes, which is the rest of the sub-block
  the code ends in and, when the code ends a sub-block, one more sub-block of one byte
  (golang.org/issue/16146), where Chromium shows the frame and reads on from its
  terminator (`TestGIFsImageGIFRefusesAreRefusedOnceDecoded`, which also has what
  `image/gif` takes stored as the file without it). `TestGIFIsReadAsChromiumReadsIt`
  holds files to the pictures Chromium was seen to show of them, and `chromiumReads` in
  the tests is that reading.
- **What is stored is never the upload.** `media.Process` rewrites every image without its
  metadata before `Save` hands it to a backend, so both backends store the same file
  (`internal/media/sanitize.go`). Files stored before this release are the exception: they
  were kept as uploaded, metadata and all, nothing rewrites them, and they are served that
  way until their posts are deleted. The README gives the query that lists them, cut off
  at migration `0008`'s `applied_at`; the deployment in `deploy/` kept uploads off, so it
  has none. JPEG is decoded, turned upright by its EXIF Orientation (`jpegOrientation`,
  `orient` in `orient.go`), and re-encoded by `jpegenc` at the picture's own chroma
  subsampling, as `orient` left it, and at `jpegQuality`: the lowest quality whose tables
  quantise no coefficient more coarsely than the upload's own did, and `storedJPEGQuality`
  (90) at most. An upload saved with the standard tables at quality 30 is stored at 30, at
  about its own size and looking as it did, where at 90 it came out at about twice the
  size (`TestLowQualityJPEGsAreNotStoredLarger`); one saved at 4:4:4 keeps its colour
  detail, which 4:2:0 averaged away (`TestJPEGKeepsItsChromaSubsampling`,
  `TestColourDetailIsKept`). The upload's tables are read and never copied: all the stored
  file takes from them is that one number. `jpegenc` writes no APP or COM segment, so
  EXIF, XMP, APP13 IPTC/Photoshop, the APP2 ICC profile and comments all go. It is
  `image/jpeg`'s `writer.go` and `fdct.go` from Go 1.24.7, changed only where marked
  `educhan:`; `TestFourTwoZeroIsImageJPEGsOwn` holds the paths it did not change to
  `image/jpeg` byte for byte, so a Go release that changes that encoder fails it rather
  than leaving the copy quietly behind. Thumbnails still go through `image/jpeg`. PNG is
  decoded, turned upright by the EXIF Orientation of an eXIf chunk before the first IDAT
  (`pngOrientation`; browsers apply it, and read it nowhere else), and re-encoded, which
  keeps only IHDR, PLTE, tRNS, IDAT and IEND (an APNG keeps its still image); the pixels
  are what `image/png` decodes them to, so 1-, 2- and 4-bit grey comes back at 8 bits and
  a colour key (tRNS on grey or truecolour) as colour with an alpha channel, or with none
  when no pixel has the key's colour (`TestPNGIsStoredInTheFormItDecodesTo`). `orient`
  turns a picture into one of the kind it was given, every pixel exactly: YCbCr in its
  planes where the chroma samples line up with the turned picture, and otherwise at 4:4:4
  a pixel at a time, because turning a plane whose last sample covers fewer pixels shifts
  every colour along that side. At 4:4:4 a pixel keeps its luma exactly but takes its
  chroma as libjpeg-turbo, and so Chromium, widens it for display (`jpegChromaRow`): at
  4:2:0, 4:2:2 and 4:4:0 a blend of the samples nearest it rather than the sample it lies
  in, which at a colour edge can be a hundred levels off what the poster saw; at 4:1:1 and
  4:1:0, or where a halved width is two chroma samples across or fewer, the sample it lies
  in, as libjpeg-turbo shows it. `turnedSubsampling` is the rule: 4:4:4 when the turn
  reverses a side that is not a whole number of chroma samples long — the width (reversed
  at orientations 2, 3, 7 and 8) when it is odd at 4:2:2 or 4:2:0 or not a multiple of
  four at 4:1:1 or 4:1:0, the height (reversed at 3, 4, 6 and 7) when it is odd at 4:4:0,
  4:2:0 or 4:1:0 — and when 4:1:1 or 4:1:0 turns on its side (5 to 8), which no ratio in
  the image package can hold (`TestOrientTurnsEverySubsamplingAsShown`,
  `TestOrientKeepsEveryPixelKindExact`). GIF is decoded by `gif.DecodeAll`, which reads
  past comments and every application extension, and written by `playGIF` (`gifplay.go`)
  from what it shows rather than from its frames: the run of pictures Chromium composites,
  equal neighbours merged, each with the time it shows (`shownDelay`: 0 and 1 show as 10),
  and the loop count. How frames make those pictures is bits nobody sees — fifteen
  one-pixel transparent frames after a picture spelled sixty bytes in their positions,
  delays and disposals; frames redrawing a picture after a clear or a put-back spelled a
  message in their delays; pixels a frame repainted in the colour under them, rather than
  left transparent, carried a 32 KiB file in a 512×512 GIF; a disposal the next frame
  painted over, and the split of a picture's time between its frames, carried more — so
  the stored file is written by one rule from the pictures, and two GIFs Chromium shows
  alike are stored as the same bytes. Each picture is one frame, drawing exactly the
  pixels that differ from what the frame before it left and transparent elsewhere, cut to
  the rectangle around those pixels and the ones the next picture has transparent where
  this one does not; it is cleared after its time when there are such pixels, since only a
  clear makes a pixel transparent again, and left in place otherwise, the last always in
  place — but for a GIF of a table a frame (below). Time past 65,535 hundredths goes into
  one-pixel frames that draw nothing after it (the last of them cleared, of the picture's
  rectangle, when the picture is), or for a frame put back into copies of it, each put
  back, none given a delay of 1. A GIF played once gets delay 0 on its last picture, and a
  GIF of one picture is a still: one frame, no delay, disposal or loop count, and a
  control extension only for transparent pixels. The header has background and aspect 0.
  Colours: when the pictures show 255 or fewer, one global table, transparent black at 0
  and then the colours in order of their values, with 0 the transparent index of every
  frame of an animation; otherwise each frame has a table of the colours it draws, in
  order, after a transparent 0 when a pixel of its rectangle is not drawn (`ownTable`). A
  frame of 256 colours has no room for that entry, so it writes each pixel it does not
  draw in the colour already showing there, which has to be one of its 256. A frame left
  in place or cleared can leave the next one more than that to draw (the colours it
  covered, back again, and the next picture's own: ImageMagick's `-layers OptimizeFrame`
  made one that left 304), so `choose` takes, for each frame, the first of three ways
  after which it and the next frame can be written: left in place (if the next picture
  needs nothing cleared), cleared with the rectangle widened as above, and put back to
  what was there before it was drawn, which the player keeps for that (`saved`) and which
  has to be transparent wherever the next picture is. Every choice is a function of the
  pictures and the choices before it, so "shown alike, stored alike" still holds, and
  every GIF stored before frames could be put back that `Inspect` still admits, and whose
  stored frames are each at least `gifRowPixels` wide, is stored as the same bytes, the
  way it took then being still the first that does. Two later charges each refuse some
  GIFs stored before: an animation whose tables give more than 255 colours is now charged
  for `saved` from its header (`gifFrames.many`, `gifPlayScratch`), so one on a large
  canvas from a large upload may be refused that was stored before (the README's bands);
  and a stored frame or a rectangle `choose` looks at is now charged by its rows
  (`gifCharge`, below), so a GIF whose stored frames are tall and narrower than
  `gifRowPixels` may be refused that was stored before (24 one-pixel frames put back at
  the two ends of a 1×65,535 canvas, 969 B, were stored before the row charge and are
  refused now). Where no way will do, the GIF is refused (`ErrImageTooCostly`), as is one
  whose stored frames, with what `choose` looked at for ways it did not take (`tried`, so
  that looking is bounded as writing is), are charged more than `maxGIFFramePixels`
  (`TestGIFOfManyColoursHasATableAFrame`, `TestGIFOfManyColoursPutsAFrameBack`,
  `TestGIFWaysNotTakenAreCharged`; after flood control,
  `TestImagesRefusedByTheirHeaderStartNoCooldown`). A frame, a stored frame and a
  rectangle looked at are each charged their pixels with every row counted as at least
  `gifRowPixels` (64; `gifCharge`), by the walk, by `frame` and by `tried` alike, because
  each row costs setting up whatever its width: charged by its pixels alone, a GIF of
  1,525 frames a pixel wide and 65,535 tall took 21 seconds of CPU to play
  (`TestGIFFrameRowsAreChargedAsAtLeastGIFRowPixels`,
  `TestGIFStoredFramesAreChargedByTheirRows`, `TestGIFWaysNotTakenAreChargedByTheirRows`).
  Both refusals are known only by playing the GIF: `Inspect` reads the header, where
  `gifFramePixels` holds the declared frames to `maxGIFFramePixels`, but the put-back,
  restore-chain and widened-clear work shows only as the frames are played, so a GIF the
  header admits can still be refused here (put back on a 2000² canvas, `Inspect` admits 49
  frames where the player takes 14; a 1000² restore chain, from an upload over the 8 MiB
  default, 199 where the player takes 34), after it has been decoded and its first picture
  thumbnailed, and after flood control, so the cooldown is charged and nothing is written
  (`Save` stores only after `processHeld` succeeds, so `discardUpload` has nothing to
  remove). The work is already spent, so the cap is held where it is found, not moved
  earlier. The choice is greedy, one frame at a time, so some GIFs a file could store one
  frame to a picture are refused: frames put back two in a row, the second to the picture
  before the first, after the stored file left the first in place; or a clear whose
  widened rectangle takes in too many colours. Only frames that clear or put back what
  earlier ones drew can bring that about, since a frame alone draws from one table: none
  of 126 GIFs made with ffmpeg, ImageMagick and Pillow is refused, nor any of the 1,500
  many-coloured GIFs of the Chromium comparison; 1 of 5,000 random GIFs of up to 256
  colours a frame and every disposal method is, and 75 of 2,000 random animations of
  sprites put back, cleared or left over a background of 256 colours (`randomSpritesGIF`).
  The player keeps what the frames before the one being played left (`under`) as a
  four-byte colour key a pixel, with two bit sets over the canvas, and for an animation
  whose tables give more than 255 colours (`gifFrames.many`, read by the walk) four bytes
  a pixel more for `saved` (`gifPlayScratch`, charged in `completedCost`,
  `TestGIFPlayScratchCoversThePlayer`); it lets each decoded frame go once it has played
  it, and keeps the walk's head of each until then (`gifFramePlay`). The thumbnail is of
  the first picture, made before playing (`firstGIFPicture`). `TestGIFIsStoredAsItPlays`
  holds 2000 random GIFs to `browserPlay` (`gifplay_test.go`), a model of Chromium's
  compositing written apart from `playGIF`, and to storing the same bytes when processed
  again; `TestGIFsShownAlikeAreStoredAlike` rewrites 1,100 random GIFs, 300 of them
  animations of sprites over many colours (`randomSpritesGIF`), at least 50 of which are
  stored with frames put back, six times each, in thirteen ways that change nothing
  Chromium shows (`gifRewrites`, each checked against the model first) and requires every
  form stored as the original's bytes; `hidden_test.go` pins the channels found, each as
  uploads differing only in that channel, stored as the same bytes
  (`TestGIFFramesOfOnePictureCarryNothing`, `TestGIFPixelsThatChangeNothingCarryNothing`,
  `TestGIFDisposalPaintedOverCarriesNothing`,
  `TestGIFTimeSplitBetweenFramesCarriesNothing`);
  `TestGIFFramesThatDrawNothingCarryNothing` (`gifplay_test.go`) checks that frames which
  draw nothing are not stored. The model and the forms were compared with Chromium
  (through WebCodecs' `ImageDecoder`, which `<img>` shares its GIF decoder with) over some
  9,000 GIFs, and again, once frames could be put back, over 8,550 stored ones (the 126
  made with real tools, the 1,500 of many colours, 4,999 random ones and 1,925 animations
  of sprites), every one showing alike; change the player and the model only together, and
  only after checking Chromium again. The loop count is written back as NETSCAPE2.0, from
  whichever loop extension came last, NETSCAPE2.0 or ANIMEXTS1.0, as browsers read them;
  `image/gif` reads only the first, so `gifFramePixels` reads both
  (`TestGIFLoopCountIsReadFromEitherLoopExtension`). WebP cannot be encoded in Go, so a
  WebP upload is stored as another format (`sanitizeWebP`): a JPEG at `storedJPEGQuality`
  when it is lossy and opaque, from its own 4:2:0 planes taken to full range where they
  lie (`expandLimitedRange`, `TestLossyWebPIsStoredAtItsOwnColourResolution`; its own
  quality is no guide: a WebP's quality setting is not a JPEG's, and the lower it was the
  larger the JPEG, measured on photos up to 1.4 times the WebP from quality 90, two to
  three times from 75 and up to eight and a half from 5), otherwise a PNG (lossless pixels
  exactly, or a lossy picture with its alpha, its half-resolution chroma widened as
  libwebp, and so Chromium, widens it: `webpChromaRow`,
  `TestLossyWebPIsUpsampledAsLibwebpDoes`), and `Processed.Format`, the stored path's
  extension, the content type, the attachment's `name` and the thumbnail's format all
  follow the stored format, not the upload's. Before decoding, `stripWebP` (`webp.go`)
  rewrites the RIFF so the decoder sees only the picture: it keeps VP8X (flags masked to
  alpha and animation, reserved bits cleared, and a still picture's alpha flag set exactly
  when an ALPH is kept, because x/image refuses the mismatches Chromium ignores, a
  lossless picture with transparency and metadata under the flag among them; an ALPH
  without the flag, which Chromium does not show, is dropped:
  `TestWebPAlphaFlagFollowsTheChunks`), ALPH, VP8, VP8L, ANIM and ANMF (each frame cut to
  its picture), drops EXIF, XMP, ICCP and every unknown chunk, recomputes the RIFF size,
  drops bytes past the RIFF's end, and refuses a file whose canvas and bitstream sizes
  disagree. Its output is never stored: a decoder ignores whatever follows the picture
  inside a VP8, VP8L or ALPH chunk, so a whole second file, EXIF and all, survives
  stripping (`TestFilesHiddenInsideWebPChunksAreNotStored`,
  `TestUploadedWebPIsServedAsAFileMadeFromItsPixels`). Do not go back to storing the
  stripped WebP to save the re-encode. An animated WebP is still refused as
  `ErrCorruptImage`, as before stripping, since x/image cannot decode one, and now from
  its header: `Inspect` refuses one whose VP8X has the animation flag or, after a VP8X,
  has a frame (ANMF) among its chunks (`webpAnimated`), before any cooldown, whatever its
  size and however much of it arrived — a file cut short is walked as far as it goes; it
  used to pass `Inspect` when short and fail after flood control, and when long be charged
  as one lossless bitstream of its whole length and refused as too large
  (`TestAnimatedWebPIsRefusedFromItsHeader`). A simple-format file is a still to
  `stripWebP` and to browsers whatever follows its picture, so a frame there does not
  count (`TestWebPStillWithAFrameAfterItsPictureIsStoredAsAStill`). `Inspect` also reads
  every other WebP's chunks as `stripWebP` does, without writing the file (`webpKept`),
  and refuses whatever `stripWebP` would refuse once processed: a RIFF that runs past the
  end of the file or a chunk that does, no bitstream, and in a simple-format file an ALPH
  before its bitstream, or in an extended one a bitstream that does not match the canvas,
  a second bitstream, an ALPH after its bitstream, beside a lossless one or after another
  ALPH, a second VP8X or an ANIM. Before that `Inspect`'s own checks caught only the RIFF
  of these, leaving the rest to x/image's `DecodeConfig`, which reads no further than a
  VP8X or, in a file without one, the bitstream, so in an extended file any of the rest, a
  chunk that runs past the end wherever it lies among them, passed `Inspect` and was
  refused after flood control, and so was a chunk after the picture that runs past the end
  in a simple-format file (`TestMalformedWebPIsRefused`, whose cases `Inspect` must
  refuse, and `FuzzStripWebP`, which holds `Inspect` to admitting no WebP that `stripWebP`
  refuses). Test fixtures that stand in for a picture never decoded still need a bitstream
  header of the canvas's size (`vp8Stub`, `vp8lStub`). A WebP stored before this change
  stays a `.webp` and is served as `image/webp` (`ContentTypeForExt`). `Processed.Data` is
  the stored file, `Width`/`Height` are the upright picture's, the thumbnail is made from
  the upright pixels, and `StoredImage.Checksum` and `Size` are of the stored bytes — the
  plan's `blocked_media` check on the re-encoded output (2.2) depends on that. Tests build
  their fixtures in code (`fixtures_test.go`) and check every needle is in the upload
  before checking it is gone from what is stored: `TestJPEGIsStoredWithoutMetadata`,
  `TestEveryOrientationIsApplied`, `TestPNGOrientationIsApplied`,
  `TestPNGIsStoredWithoutAncillaryChunks`,
  `TestGIFKeepsItsAnimationAndDropsItsExtensions`, `TestWebPIsStrippedChunkByChunk`,
  `TestWebPIsStoredAsPNGOrJPEG`, the conformance suite's "save stores no metadata", and
  `TestUploadedPhotoKeepsNoMetadataOnEveryBackend` end to end over both backends. Every
  stored original is a file Go encoded from decoded pixels; never store, hash or serve the
  upload's own bytes, or any run of them, such as a chunk copied across.
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
  alpha), and `cleanPaletted` rebuilds a PNG's palette through a `colourFolder`: one entry
  per distinct colour its pixels show, in first-use order, a single transparent entry,
  every pixel remapped to it. It folds the palette indices in the order the pixels first
  use them (`firstUses`, `foldPaletted`), which is the same table as folding pixel by
  pixel at a map lookup a palette entry rather than a pixel; a lone picture never
  overflows, so `cleanPaletted` ignores it. A GIF's tables are written from the colours
  its pictures show (above), never taken from the upload, and so is whether its frames
  share one, which shows nothing either (`TestGIFColourTablesKeepOnlyWhatIsShown`,
  `TestGIFFieldsBrowsersDoNotShowAreWrittenTheSame`). What is left is the picture, kept
  exactly, and that is the whole guarantee: it is not that nothing unseen survives. A
  fully opaque pixel keeps its exact value, including the low byte of a 16-bit channel
  (`TestPNGKeepsSixteenBitPixels`), so low-bit steganography in a visible picture survives
  by design; removing it would change the picture. The one exception is a PNG's gamma,
  applied so that the picture shows as it did (below). `hidden_test.go` holds each path —
  `TestPartlyTransparentColourIsReducedToItsAlpha` the alpha-precision cut and
  `TestFlatPaletteCannotHideAFileInItsPixels` the index channel of a flat palette — with
  the needles checked present in the upload first.
- Besides the decoders, educhan reads structures an uploader controls in a few places of
  its own: the EXIF reader, the JPEG segment walks (`jpegOrientation`, and `readJPEGFrame`
  with its frame header and quantisation tables), the PNG chunk walks, the GIF block walk,
  the WebP RIFF rewrite, `Inspect`'s animation check (`webpAnimated`), and the cost
  model's walk over a WebP's chunks (`webpPicture`). Each reads in place and bounds-checks
  every offset and length before use. None collects what it walks into a list: the WebP
  rewrite reads its chunks one at a time (`nextChunk`/`walkChunks`) and keeps only the
  ones that make the picture, so a file of a million empty chunks costs it the picture it
  keeps and a short id string per chunk as it goes, not a struct for each chunk in the
  file (`manyChunkWebP`, `TestWebPChunkWalkDoesNotHoldAStructPerChunk`); the rewritten
  file is the only sizable thing any of them allocates. Each gives a safe answer for what
  it cannot read — orientation 1, the dearest cost, `storedJPEGQuality`, not animated
  (which leaves the file to the RIFF rewrite) — except the GIF walk and the RIFF rewrite,
  which refuse the file. The EXIF reader follows no offset but IFD0's and, from IFD0
  alone, its Exif sub-IFDs', which are where Chromium reads the orientation from, and it
  reads the entries as Chromium does (`TestOrientationIsReadAsChromiumReadsIt`), from the
  first Exif segment or eXIf chunk with any EXIF in it, broken or not
  (`TestEmptyEXIFBlocksArePassedOver`); no IFD chain is walked, so there is no loop.
  Chromium follows every Exif sub-IFD entry in IFD0, though, and 65535 of them naming IFD0
  is 4.3 billion entries to read, so the reader stops after `maxEXIFEntries` in all, every
  entry of IFD0 and of one sub-IFD (`TestEXIFReadIsBounded`). Whether the file is an image
  is the decoder's call alone (`TestHostileEXIF`, `FuzzJPEGOrientation`,
  `FuzzPNGOrientation`, `FuzzJPEGQuality`, `FuzzGIFFramePixels`; `FuzzInspect` runs every
  walk `Inspect` makes over files of every format, and checks that what it admits is
  estimated at no less than a byte a pixel and no more than the budget; `FuzzStripWebP`
  runs the RIFF rewrite over lossless and lossy WebPs and checks it is idempotent). No
  EXIF library is needed and none should be added.
- **No filename is kept.** A camera's or a phone's filename can name a date and a person,
  so `parseSubmission` reads only the file's bytes, and an attachment's name is
  `attachmentName(stored path)`: `image` plus the stored format's extension (`image.jpg`),
  whatever the client sent, and so whatever extension it claimed. It is stored in
  `attachments.original_name`, a column that kept its name; `0008_attachment_names.sql`
  rewrote the rows from before the same way, from `original_path`, and `renameAttachments`
  repeats it on every start for any row an older release still serving during a rollout
  wrote since; both have to stay in step with `attachmentName`
  (`TestUploadedFilenameIsNotKept`, `TestAttachmentNamesAreRewritten`,
  `TestAttachmentNamesStoredSinceTheMigrationAreRenamedOnStart`). The client shows it as
  the caption under the thumbnail.
- Dropping the ICC profile (and a PNG's `iCCP` and `cHRM`) is deliberate: most photos are
  sRGB, which is what a browser assumes untagged, and look the same; a wide-gamut one
  (Display P3, Adobe RGB) shifts slightly. A profile can name a device or an editor, and
  converting to sRGB properly needs a colour engine Go does not have. A PNG's `gAMA` is
  the exception, because dropping it shifts far more than slightly (a picture tagged 1.0,
  linear light, showed its mid-grey of 128 at about 188) and applying it needs only a
  curve. `pngGamma` (`gamma.go`) reads it as Chromium does: the first `gAMA` before the
  first `IDAT`, and none at 0, within 5% of 1/2.2, or beside an `sRGB` chunk whose
  rendering intent is 0 to 3 or the first `iCCP` chunk that is not empty holding a profile
  Chromium uses, one that inflates whole, checksum and all, with no preset dictionary, to
  no more than 64 MiB and that skcms, Chromium's colour engine, would take
  (`pngProfileUsable`, which reads it as `skcms_Parse` does short of its lookup tables).
  Chromium's own cut is lower, by more than the compressed chunk: its decoder takes the
  profile from a 64 MiB budget its own buffers share, which left about 480 KiB less for a
  291 KB chunk and 7 to 8 MiB less for a 6.5 MB upload of random bytes, so a profile
  within that of 64 MiB is taken as used here, where Chromium applies the `gAMA` or does
  not show the picture (`maxICCProfile`). An `sRGB` or `iCCP` chunk Chromium cannot read
  takes nothing's place there, and the `gAMA` beside it is applied; a `cICP` chunk, which
  Chromium reads before them all, is not read here. `applyPNGGamma` converts the colour
  samples, never alpha, to the sRGB values that show the same light, at the picture's own
  depth, before `reduceHiddenColour`. `TestPNGGammaIsShownAsBrowsersShowIt` holds a stored
  grey ramp to within a level of what Chromium 141 drew for each tag, and has a case for
  each of those rules, for a profile broken in each way `iccProfileUsable` refuses one
  (the one whose tag table runs past it is refused by its tags' bounds as well, so
  `TestICCTagTableMustFitTheProfile` has one only the table's length refuses), and for the
  sound grey, RGB and `cicp`-tagged profiles those differ from; none has a lookup table
  (`A2B0` or `A2B1`), which stands in here for the curves and colourants without being
  read. `TestPNGGammaConvertsColourOnly` covers every picture type `image/png` decodes to.
  A change there needs the same browser comparison.
- **Memory.** `MaxPixels` is 25 megapixels, so that the 24-megapixel photos phones and
  cameras save are taken whole (an iPhone's are 5712×4284, 24.5 million pixels;
  `TestTwentyFourMegapixelPhotosAreAccepted`), and a GIF's frames together may not be
  charged more than `maxGIFFramePixels` (four times that, each row counted as at least 64
  pixels), but neither is the memory bound: what an upload holds while it is processed
  depends on how it is encoded as much as on its size. `Inspect` estimates it from the
  header (`processingCost`, `cost.go`): the decoder's buffers (a progressive JPEG keeps
  256 bytes of coefficients for every 8×8 block until its last scan; CMYK and RGB JPEGs
  decode to 4 bytes a pixel, and which are RGB is read as `image/jpeg` reads it, from the
  APP0 and Adobe segments of the whole file, after its scans too (`readJPEGFrame`,
  `TestJPEGColourModelIsReadAsTheDecoderReadsIt`); a 16-bit PNG to 8, or to 2 when it is
  grey with no transparency, as `pngBytesPerPixel` reads its header, and every PNG through
  two rows of the file beside its picture (`pngRowBytes`), or an interlaced one through
  two rows of each of its seven passes, which come to five and a half rows of its width
  (`pngPassRows`, `TestPNGDecoderRowsAreCharged`), tens of megabytes in a strip;
  `image/gif` keeps `gifFrameOverhead` for every frame besides its pixels, so a GIF of
  thousands of empty frames is not free, and `playGIF` four bytes and two bits a pixel of
  the canvas, and four bytes more for an animation whose tables give more than 255
  colours, `gifPlayScratch`, and `gifFramePlay` a frame), the copy `orient` makes, the
  encoder's output buffer (`storedBuffer`, which refuses to grow past `storedLimit` with
  `ErrStoredTooLarge`: four times the upload plus half a byte a pixel, so a file Go's
  encoders cannot compress the way it came is refused rather than stored many times
  larger; a lossy WebP with transparency, stored as a PNG and so meant to grow, is held to
  `pngLimit` of its size instead, which the header gives, so its cost refuses it up front
  rather than after decoding), the six rows of the stored picture `image/png`'s encoder
  writes a PNG through (`pngEncoderRows`, all made before the first row is written, so
  counted in what the encoder allocates as it begins: on top of both pictures of a turned
  PNG, `TestPNGEncoderRowsAreChargedAsItBegins`), and the thumbnail scaler's scratch
  (`thumbScratch`, for the picture as it is stored, turned upright by `storedOnItsSide`):
  its buffer, as wide as the thumbnail and as tall as the picture, and its tables of
  weights, 64 bytes for every pixel of the two sides, which are most of it for a long
  strip — a PNG a pixel wide and 25 million long would have taken 1.6 GB of them before
  they were charged. `TestThumbScratchCoversTheScaler` measures both, and the turned PNG
  strip in `costFixtures` the whole. `peakOf` takes them phase by phase and counts each
  phase's first allocation on top of the previous phase's peak, because a collection that
  began before the boundary still holds that phase's scratch. An estimate over
  `processingBudget` (256 MiB) is refused from the header as `ErrImageTooCostly` (`image
  too large to process`), before any cooldown, and so is a GIF whose frames' charge passes
  `maxGIFFramePixels`; `ErrImageTooLarge` (`image resolution is too large`) is kept for a
  picture of more than `MaxPixels`, the one refusal that is about the resolution, so the
  answer says which (`TestImagesRefusedByTheirHeaderStartNoCooldown`). Which kinds that
  refuses at 25 megapixels is listed in the README, and two tests hold the lists to the
  model. `TestCostlyEncodingsAreRefusedFromTheirHeader` takes each accepted kind as a
  square from an 8 MiB upload, each refused kind at its cheapest (a wide picture from a
  small upload), and every kind at a quarter of the size. A square is not the dearest
  shape, though: a JPEG's sides are rounded up to whole blocks, a turn that cannot keep
  the subsampling copies at 4:4:4, and the thumbnail's weights grow with the longer side,
  so `TestEncodingsAreJudgedAtEveryShape` takes every shape of the full size each format
  allows (`eachShape`: for every shorter side, the four longest sides that fit, both ways
  round), at every orientation, and holds each kind to the shape the README gives for it.
  It reads `processingCost` with the frame header rewritten in place rather than calling
  `Inspect` millions of times, and checks `Inspect` agrees at the shapes that decide. A
  change to `cost.go` that moves a kind, or the shape a kind is refused at, changes the
  README's lists in the same commit. The same model caps the upload size limit: an upload
  is charged eight bytes for every byte of it whatever its picture (its stored file may
  come to four times its size, and the encoder's buffer holds two of those as it grows),
  so `media.MaxUploadBytes` (31 MiB), derived from `processingBudget` and `storedLimit`'s
  terms in `cost.go`, is the largest size in whole MiB at which the cheapest file of each
  format is admitted. `maxUploadLimit` (`config.go`) is that, and the server refuses to
  start with a larger `EDUCHAN_MAX_UPLOAD_BYTES`; `TestLargestAttachmentLimitIsReachable`
  posts the cheapest file of each format at that size and holds one a MiB larger to a
  refusal. A change to the budget or to `storedLimit` moves it, and the README's figure
  with it. `Save` then holds its estimate of the budget from the start of decoding until
  both renditions are stored, because until then it holds what it made
  (`acquireProcessing`, a first-come-first-served weighted semaphore in `budget.go`, so a
  large upload is not starved by small ones; `TestSaveHoldsItsShareUntilItIsStored`), and
  `processHeld` gives that share back by a deferred call until it hands it over, because
  the server recovers from a panic in a decoder or an encoder and goes on with whatever
  the panic left held (`TestAPanicWhileProcessingGivesItsShareBack`). Nothing it writes is
  copied again on the way out: `GCSStore.Put` sets the writer's `ChunkSize` to 0, because
  the library's default is a 16 MiB buffer for every object, kept only for retries a write
  without preconditions never makes (`TestGCSPutAllocatesNoBufferOfItsOwn`). The uploads'
  own bytes have a budget of their own, `media.UploadBudget` (64 MiB), which
  `parseSubmission` takes from (`media.ReserveUpload`) before it reads a multipart body,
  as much as the request's `Content-Length` says, up to the most a body may be, and which
  the handler gives back when the post is answered (`submission.release`). For that to be
  what the body costs, `readMultipart` reads the body itself rather than through
  `ParseMultipartForm`, which keeps a file over its memory limit in a temporary file
  (memory again on Cloud Run) and then hands it over to be copied: the image is read once
  into a buffer the size of what is left of the body (`TestAnUploadIsReadOnce`). Its text
  fields may come to `maxRequestBytes` between them, as a JSON post may, the first value
  of each name is the one read, and parts past `maxFormParts` refuse the form
  (`TestMultipartPostsAreReadAsTheFormSendsThem`). A body holds its room in the upload
  budget from before it is read, and a post whose body never arrives never reaches flood
  control, so before the share one client's eight connections declaring 8 MiB bodies and
  sending nothing held the whole budget for the read timeout, turned every other image
  post away `503`, and were charged nothing. Now `reserveUpload` takes from the client's
  share first (`clientUploadShare`, half the budget and room for the largest body
  `maxUploadLimit` allows, which `TestConfigFromEnvRejectsInvalidValues` holds; counted in
  `uploadShares` per `clientIP`, but per /64 for an IPv6 address (`shareKey`), since its
  host can usually take any address in its /64 and counted per address the share would
  bound nothing, while flood control stays per address, its cooldowns being a wait a whole
  network would feel), and a post past it is read to its end unkept and answered `429`
  (`errUploadShare`, `busyRetryAfter` to wait, nothing charged) without waiting, or `408`
  if its body falls behind on the way (`turnAway`); and `pacedBody` holds the body to an
  even pace through its read window, moving the read deadline before every read to
  `uploadPaceGrace` (5 s) past when what has arrived would have at the pace that brings it
  all in by the window's end, so a body that sends nothing or a trickle is answered `408`
  and gives its room back after the grace, not at the window's end
  (`TestIdleUploadsCannotHoldTheUploadBudget`). A body without a `Content-Length`
  (chunked) would have to reserve the most a body may be, and could then be held only to
  the pace of that, which cut a small one sent slowly off, or to none, which let seven
  trickles from three addresses hold the whole budget for the window; so, with uploads on,
  such a post is answered `411` on its headers (`refuseUnsized`), before anything is
  reserved for it or read of it, charged nothing, its connection closed
  (`TestPostsWithoutALengthAreRefused`), and every body that holds room is held to its own
  pace. Browsers send a `Content-Length` with every form. Between them the two budgets
  hold 320 MiB at most, whatever `containerConcurrency` lets in; beside them the
  content filter keeps about 65 MiB of planned searches, and the rest of the process is
  about 40 MiB. Eighty 8 MiB uploads sent at once to a server with `GOMEMLIMIT=400MiB`
  and one core peaked at 416–424 MiB resident, whether they came while the filter
  was being planned or after it (three runs each; 330 MiB before the filter, 786 MiB
  before the upload budget), and at about 470–540 MiB with no limit (469–540 MiB
  over two separate measurements on a shared machine of four vCPUs), so a 512 MiB
  instance needs that limit, which this code does not set. The limit is passed rather
  than kept, since what the budgets and the filter hold is live. **Every wait is
  bounded, and a slow client is told apart from a busy server.** A post waits for room in
  the upload budget under `reserveContext` (`postDeadline`, 25 s, from when its handler
  starts); a wait that does not get room gives up `processingAllowance` (15 s, which
  leaves margin: no upload `Inspect` admits is processed for long, because the slowest
  kinds are bounded from the header and a PNG is written at deflate's fastest level
  (`storedPNG`, `TestStoredPNGsAreDeflatedAtBestSpeed`; at the default level what its rows
  held decided how long deflate searched them, and crafted rows took a 16-bit picture of
  1.4 MB about 17 s; the price is size, the 16-bit synthetic pictures tried stored up to
  14 times larger, so a PNG the default level stored near `storedLimit` can now be refused
  `ErrStoredTooLarge`) — a 25-megapixel 16-bit PNG takes 4 to 5 s of CPU on one core, an
  8-bit transparent one 2 to 3, the slowest GIF about 4 (25 frames of a 2000×2000 canvas
  each with a table of its own and a pixel to clear), a lossless WebP of 25 megapixels,
  which a crafted file of a few dozen bytes can be, 4 to 6.5, a lossy WebP with
  transparency at the most pixels the memory estimate admits (19 megapixels square, 21 in
  a strip) 4.5 to 9.5 by what its alpha holds, which with the CMYK JPEG below makes it the
  slowest kind (it was measured in runs in which that JPEG took 8.5 to 10 s, see
  `processingAllowance`), and a JPEG is held to `maxJPEGScanRevisits` passes' work over
  its blocks (`jpegScanBlockVisits`, which weighs a baseline scan's visit at six
  first-pass visits and an AC refinement scan's at two, for what the decoder does on
  each), so the worst admitted one, a 25-megapixel baseline file of four full-resolution
  components, is about 5 to 6 s to decode and 5.5 to 7.5 s to process, and a progressive
  one, which the memory estimate holds to fewer blocks, about 3 to 4 s to decode, rather
  than the thousands of seconds a repeated cheap scan would take) before that deadline,
  with `media.ErrBusy`, and room that is there is taken however near the deadline. Its
  body then has `uploadReadTimeout` (30 s) to arrive, counted from when the wait ends, so
  time spent waiting for room is not taken out of the body's: a read deadline set on the
  connection through an `http.ResponseController` once `ReserveUpload` has returned,
  whatever it returned (the logging middleware's `statusRecorder` carries an `Unwrap` so
  the controller reaches the connection underneath); a body slower than that is the
  client's doing, answered `408` (`upload too slow`, `isReadTimeout`), as is one that
  falls behind its pace (`pacedBody`), not the `503` a server with no room gives, and a
  body that waited for room is not answered `408` for the server's load. A turned-away
  post's body is read past at the same pace, and `turnAway` answers it from how that read
  ended, never from the request's context, which net/http cancels for a read deadline as
  for a client gone: mapped through `fail`, a busy refusal whose body fell behind answered
  nothing, which net/http sent as an empty `200` that the web client took for a post made
  (`TestATurnedAwayBodyIsAnsweredWhateverItsPace`). Once the body is read, the image has a
  fresh `postDeadline` (25 s, `processContext`) to be processed and both renditions stored
  — measured from after the body, not from the start of the handler, so a slow body no
  longer spends the time the image has and is no longer mistaken for a busy server
  (`TestSlowUploadBodyIsNotMistakenForABusyServer`); `processContext` also resets the
  response's write deadline to cover that processing and the answer, the body read having
  possibly used more than one `WriteTimeout` (30 s, `app.WriteTimeout`, which `cmd/server`
  uses) from the end of the headers. `Save` writes nothing once that processing deadline
  has passed, on either backend (`LocalStore.Put` honours an ended context as Cloud
  Storage does, pinned in `testStoreContract`), and removes a written original on a
  context that has not ended (`context.WithoutCancel`). Decoding, re-encoding and playing
  a GIF each watch that context as they run — a reader that fails once it is done, a check
  straight after each decode, a stored-buffer write that refuses once it is done (and
  `jpegenc`, refused, stops at the end of the row of units it is coding), a check before
  each GIF frame, and one before the thumbnail — so a cancelled or timed-out upload stops
  partway rather than running to the end. A decode that has already read its whole input
  is the exception: image/jpeg reads a small progressive file whole, and x/image a lossy
  WebP's partitions, before their costly work, which then runs to its end — a few seconds
  of CPU at most within the caps, holding its share of the budget until then — and nothing
  more is done after it. `cancel_test.go` pins each check on its own by ending the context
  the moment that check asks (`askedContext`), so none of it rests on timing. The database
  commit runs on the request's own context, never a deadline: a deadline passing
  mid-commit could otherwise leave a post made and its image removed. A budget wait or a
  processing deadline that runs out is `tooBusy`, sent to `answerBusy`: `503` with a
  `Retry-After` of `busyRetryAfter` (10 s) and `retry_after`, logged by the request log as
  a warning, not an error; the body of a post turned away for a full budget before it was
  read is still read to its end and dropped, so the browser gets the answer rather than a
  connection closed under its upload. Before the bound, a queued upload was stored and its
  post made after its answer could no longer be sent, and the poster, told the network had
  failed, posted it twice (`TestBusyServerTurnsAnUploadAwayBeforeKeepingIt`,
  `TestPostPastItsDeadlineIsNotMadeAndNotCharged`,
  `TestWaitingForRoomEndsInTimeForTheWork`). A cancelled wait reaches the handler as
  `context.Canceled`, which `fail` answers like any request whose client left.
  `TestProcessingCostCoversWhatIsHeld` processes a file of each kind the model
  distinguishes (`costFixtures`), at sizes where every term charged for decoding comes to
  megabytes, with the collector running continually, and fails if what it marked live
  passes the estimate for the whole of the processing, decoded to the end
  (`completedCost`, as every fixture is), or, before the first byte is encoded, the
  estimate of the decoding phases alone (`decodingPhases`), which the whole's larger
  encoder and thumbnail terms would otherwise hide; `TestThumbScratchCoversTheScaler`
  holds the thumbnail's scratch the same way, and
  `TestGIFFrameOverheadCoversWhatDecodingKeeps` measures what `image/gif` keeps per frame.
  What a collection marks can be more than was held but never less, so each check takes
  the least of up to three runs (`peakLive`, `leastPeak`), and one busy moment does not
  fail it. They catch a term left out, which comes to more than `processingSlack` in some
  fixture, unless another term charged for the same moment is at least as large in every
  fixture, as the alpha plane and the NRGBA copy of a transparent lossy WebP are. They do
  not catch a term cut short by less than that slack: the turn term can be halved and
  every test still passes. So they guard against a term left out, not against every
  under-estimate, as the comment at the top of `cost.go` says, and a new term needs a
  fixture where it is megabytes over the slack, as the comment on `costFixtures` sets out.
  `TestProcessingCostCoversWhatIsHeld` is the package's slowest test, about five minutes
  under `-race`. A change to a decode path, to `orient`, to the stored buffer or to the
  thumbnail changes `cost.go` in the same commit. A CMYK JPEG is turned into RGBA where it
  lies (`rgbaOfCMYK`), not into a second picture. Tests narrow the budget with
  `narrowBudget`. x/image's VP8L decoder allocates its Huffman groups — a 167.5 MiB
  backing array at most (one `hGroup` per meta-image index, `vp8lGroupBacking`), plus a
  node slice per tree — from counts deep inside the compressed bitstream that no header or
  resolution shows, and it makes every group the meta-image names before it reads any of
  their trees. A bitstream of a few dozen bytes that names group 65535 and ends makes it
  set aside all 167.5 MiB and then fail, so `processingCost` is the more of two estimates:
  decoding run to its end (`completedCost`), and that stop (`vp8lStopCost`: the whole
  backing, the nodes of the bytes read, the stripped copy, and the predictor, cross-colour
  and meta-images decoded before the groups and held beside them, up to 12 bytes for every
  4×4 tile of the picture; `TestVP8LGroupsNamedButNeverGivenAreCharged` measures it
  lossless and as a compressed alpha plane, small and at megapixels). Every lossless WebP,
  and every lossy one whose alpha plane is compressed, is charged at least 171.5 MiB
  however small it is, so two are never processed at once. A bitstream the decoder reads
  to its end has given five trees for every group it named, so there the bound is the size
  of the bitstream it reads — a lossless picture's VP8L chunk, or a VP8L-compressed ALPH
  chunk with the 5-byte header the decoder puts in front of it, each read through a reader
  that ends with the chunk (`webpPicture`) — and `vp8lMemory` charges that backing (capped
  by the bitstream size) plus a per-byte bound on the node slices (`vp8lNodesPerByte`,
  with margin over the worst encoding) on those bytes, not on the rest of the file. A
  crafted file at each extreme measures inside it: 160 KiB of one-symbol groups that holds
  about 170 MiB, and 215 KiB of full trees that holds about 43 MiB
  (`TestVP8LHuffmanMemoryIsHeldWithinTheEstimate`). A bitstream over 224 KiB is refused
  from its header as `ErrImageTooCostly`, before any cooldown, and less in a larger
  picture, about 203 KiB at a megapixel and 128 KiB at nine
  (`TestVP8LHuffmanMemoryIsCharged` pins the line the README gives). So the 320 MiB bound
  above holds for these files too; the cost of that bound is that large lossless WebPs,
  and WebPs whose compressed transparency alone is that large, are refused — not an
  ordinary cut-out, whose alpha plane is a few kilobytes however large its picture, nor a
  small picture behind a large colour profile
  (`TestLargeWebPsWithSmallVP8LBitstreamsAreStored`) — and that the ones it stores take
  more than half the budget each while they are processed. An uncompressed alpha plane is
  not decoded through VP8L and carries no such charge.
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
  creation, set from that count on every published image reply (a held one changes
  nothing), and recounted from the published posts when a post is deleted or a held
  one approved (`recountThread`). Nothing enforces the cap against the counter. The
  order in `createPost` is: parse the body, resolve the badge, then `ReplyRefusal` — a
  lock-free read that makes `CreateReply`'s checks in its order (missing, archived,
  locked, and for an image the cap, counted from the posts like the real check) — then
  `notCoolingDown`, then the content rules (`refuseContent`), then flood control
  (`allow`), then the upload is stored, then `CreateReply`. So the common refusal costs no
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
  from the rewritten, upright picture; a GIF's is the first picture it shows, its first
  frame drawn on a transparent canvas and scaled whole (`firstGIFPicture`), made before
  the frames are played, so it never depends on how the stored frames are cut
  (`TestGIFThumbnailIsTheFirstPictureAtTheSizeOfItsCanvas`).
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
  than editing an applied one; `0001_init.sql` is deliberately written with `IF NOT
  EXISTS` so it is a no-op on databases created before migrations existed. There are no
  down migrations, so a rollback runs old code on a newer schema: `migrate` logs an error
  naming the versions it does not know, and `App.schemaAhead` makes the sweeper refuse to
  run. A rollback across `0005` is unsafe whatever the code says — phase-0 code shows
  archived threads as live and its prune deletes them, leaving their files behind
  (`deploy/README.md`). So is a rollback across `0009`: older code does not know
  `held_at`, so it shows held posts and held threads publicly. Under the same lock, after
  the migrations, `renameReservedBoards` moves any board whose slug has since been
  reserved to `<slug>-board` (`TestBoardsUnderNewlyReservedSlugsAreRenamed`), so reserving
  a new word needs no migration of its own, and `renameAttachments` repeats `0008`'s
  rename, so a filename an older release stored during a rollout is gone at the next
  start.
  A new column on `posts` also means adding it to `TestPostsTableColumnsArePinned`
  (`verify_api_test.go`) in the same change: everything on that table is stored about
  every poster, so each column is there because somebody decided it should be.
- **Threads are archived, not pruned.** A new thread that takes a board past its
  `thread_limit` sets `archived_at` on the least recently bumped non-sticky live threads
  (`pruneThreads`). Every live view — the index count and list, the catalog, and the
  prune subquery itself — filters `archived_at IS NULL`, which is what keeps archived
  threads from counting against the limit or being archived a second time
  (`TestArchivedThreadsDoNotCountAgainstTheLimit`). Every one of them also filters
  `held_at IS NULL` (see Content rules and held posts), and the partial index
  `threads_board_public_order_idx` covers exactly that set: live and not held. `Thread()` still serves an
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
  its key. Last — after the media pass, whatever it found, but not after a database
  error or a cancellation — it calls `RewriteCredentials` (see **Verified Anonymity**
  below). It logs one record of counts and nothing that names a board or thread or
  says anything of a credential, failures included, and the error it returns has the
  key taken out (`TestSweepLogsOnlyCounts`). `credentials_rewritten` is the number of
  rows the rewrite copied: on a night its transaction commits, every row in the
  credentials table, expired ones no confirmation has pruned yet included, even if
  a later step then fails; on a night it does not — the sweep stopped before it, or
  the rewrite failed, as when its lock times out — 0, whatever the table holds
  (`TestSweepFailsWhenTheVerificationTablesAreHeld`). Night after night it is a
  history of the table's size, which the database does not keep, but for that: a 0
  on a night that exits non-zero can be a rewrite that did not run, and the error
  says which step failed. Nothing finer belongs beside it. It returns an error —
  a non-zero exit — on a database failure, a file it could not remove or did not
  find, a rewrite or vacuum of the verification tables that did not finish, a
  vacuum of `pg_statistic` that did not finish (which a role that owns the tables
  but not the database gets every night), or a database a newer release migrated
  (`TestSchemaFromANewerReleaseStopsTheSweeper`). When more than one of those fails,
  the error carries each, so that a failing vacuum of `pg_statistic` does not hide
  files missing from the media store (`TestSweepErrorNamesEveryStepThatFailed`).
  It builds its App with `New` like the server, and runs without the salt, the
  moderation key and the verification secret on purpose, so `New` warns about none
  of them: those warnings are
  `WarnServingSettings`, which only `serve` in `cmd/server` calls
  (`TestSweepModeLogsNoServerWarnings`, `TestServingWarningsAreTheServersAlone`). A new
  warning about a server-only setting goes there, not in `New`. `SweepArchived` skips an
  archived thread that has a held post in it, however long ago it was archived, so a
  held threat cannot vanish before a moderator reads it. **Legal holds (build plan 2.10)
  are not built**; when they are, `SweepArchived` must skip held threads the same way,
  and its comment says so.
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
  `EDUCHAN_UPLOAD_COOLDOWN`, `EDUCHAN_REFUSAL_LIMIT`, `EDUCHAN_LOG_FORMAT`, `EDUCHAN_MEDIA_BACKEND`,
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
  exception, so every refusal that can be known up front comes before `allow`: a malformed
  poster key, the body, a badge the poster cannot have, uploads switched off, a file its
  header shows to be unusable (`media.Inspect` in `parseSubmission`: not an accepted
  format, more than `media.MaxPixels`, a GIF whose frames come to too many pixels or whose
  blocks do not reach the end, an animated WebP or one whose chunks do not make one
  picture (one cut short among them), or one whose processing would hold more than the
  whole budget — which turns on how it is encoded, not its size alone), and a reply the
  thread would refuse (`ReplyRefusal`). Decoding the pixels is not up front, because it is
  the cost the upload cooldown limits, so an image that fails to decode, whose re-encoded
  file would pass `storedLimit` (`ErrStoredTooLarge`), or, for a GIF, that cannot be
  stored one frame to a picture (`ErrImageTooCostly` from `playGIF`), is refused by
  `storeUpload` after `allow`, its cooldown kept
  (`TestImagesRefusedByTheirHeaderStartNoCooldown` pins both sides, `ErrStoredTooLarge`
  among them). The exception is a post the server turns away as busy after `allow`
  (`tooBusy`: no room to process its image in time, or its deadline passing before the
  image is stored): `allow` hands back the refund `chargeAll` makes, which takes back the
  cooldowns that post started and leaves alone any that another post has started since
  (`TestChargeAllRefundsWhatItStarted`). A post turned away for want of room to read it is
  turned away before `allow`, so it has nothing to refund. A post the content rules refuse
  starts no cooldown and is counted by `refusalBudget` instead, and a post a cooldown
  would refuse is answered by `notCoolingDown` (the same check, recording nothing) before
  the rules read it (see **API contract** under Content rules). State is per process and
  in memory — a shared backend means replacing `cooldownLimiter` and `refusalBudget`,
  which nothing else depends on. The keys carry client addresses, so the limiters are also
  a privacy surface: `New` builds a `cooldownSweep` over all four limiters and the refusal
  budget (anything `expiring`) and starts its timer, which drops every expired entry each
  `cooldownSweepInterval` (one minute), and `Close` stops it. **While the process has CPU,
  an address is held in memory no longer than the longest cooldown that applies to it plus
  one minute**, counted from its last action that started one (for the refusal budget,
  from the first refusal of its minute), busy server or quiet. A host that gives CPU only
  during requests, which is Cloud Run's default (request-based billing), holds the timer
  back on an idle instance, so `beforeEachRequest` (in `Routes`) runs an overdue sweep
  before a request is handled; until a request arrives or the instance stops, an idle
  instance still holds what it last saw, and on Cloud Run the one-minute bound needs CPU
  always allocated (`--no-cpu-throttling`). The README's Flood control section states the
  bound with that condition, and anything the client says about addresses rests on it,
  which is why the FAQ promises no time limit. A longer interval, a new limiter left out
  of the sweep, a limiter keyed on something that outlives its cooldown, or the
  request-path sweep dropped from `Routes` breaks it
  (`TestSweepExpiredKeepsOnlyLiveCooldowns`, `TestSweepCooldownsRunsUntilStopped`,
  `TestCooldownSweepRunsBeforeARequestOnceItIsOverdue`, and
  `TestFloodControlForgetsAddressesOnAQuietServer` and
  `TestFloodControlForgetsAddressesBeforeTheNextRequest`, which shorten the period through
  `Config.cooldownSweepEvery`). The record-time sweep past `cooldownSweepThreshold` only
  bounds a burst between two ticks. The upload shares (`uploadShares`) are keyed on client
  addresses too, by `shareKey` (the address, or an IPv6 address's /64), and need no sweep:
  a client's entry is taken by `reserveUpload` once a multipart post's headers have
  arrived, given back by `submission.release` when the post is answered, and deleted once
  it holds nothing (`TestUploadSharesForgetAClientOnceItHoldsNothing`), so it is held for
  about a minute at most, the wait for room, `uploadReadTimeout` and `processContext`
  together, besides the time the database takes to store the post. The README's Flood
  control section says so; a share kept past its post, or keyed on anything that outlives
  it, would hold an address beyond that.
- `EDUCHAN_TRUST_PROXY` is a hop count, not a boolean (`true` still means 1). `clientIP`
  in `middleware.go` reads the Nth `X-Forwarded-For` entry **from the right**, because each
  proxy appends and everything to the left is client-supplied; a chain shorter than the
  configured depth or a non-IP entry falls back to the peer address. Setting it higher than
  the real chain hands clients control of their own rate-limit bucket, and of their poster
  ID when they send no poster key.
- Moderation is one shared key rather than accounts: `authorizeMod` in `moderation.go` is
  the single gate, so replacing it with real janitor roles later touches only that file.
  Deleting a post or thread removes the attachment blobs after the transaction commits; a
  failure there is logged and leaves an orphaned file rather than a broken post, which
  `serveMedia` answers `404` to all but a moderator because no row names it.
  `removeBlobs` ignores the caller's cancellation, because the rows are already gone and
  nothing would come back for the files later. Approving a held post goes through the
  same gate (`handleApprovePost`), and logs where it was, never what. Sticky and lock
  apply to live threads only: `SetThreadFlag` refuses an archived one with `errThreadArchived` (`403 thread is
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
  credential to a post. Practical consequences when editing `verify.go`, `store.go`,
  `0004_verification.sql`, `0015_verification_statistics.sql`, or
  `0016_vacuum_probe.sql`:
  - Never add a column to `verification_pending` or `verification_credentials` that could
    be joined to a post, and never add `created_at` or a "last used" — an issuance or use
    timestamp correlates with the posts around it. `CredentialLabel` is a read and has to
    stay one. Credential expiry is stored counted from the end of the day the
    credential was issued in, not from the moment, for the same reason;
    `TestVerificationTablesCannotBeJoinedToPosts` pins both tables' exact columns. That
    day runs from 05:00 to 05:00 UTC (`credentialDayEnd`), ending after the nightly
    sweep and its retry rather than at midnight, because the sweep's transaction id
    splits the day it runs in: with a midnight day, it and the expiry dated every
    credential issued between midnight and the sweep to those three hours.
    `TestCredentialDaysEndAfterTheSweep` reads the schedule and the Job's retries and
    timeout from `deploy/README.md` and `deploy/first-deploy.sh`, and wants the day to
    end half an hour or more after the latest the sweep and its retry can run, and two
    hours at most after the scheduled time: a sweep scheduled at 03:00 UTC or later
    whose retry cannot run past 04:30 (03:00 to 04:00, with one retry of a
    fifteen-minute task). So a change to the schedule, the retries or the timeout that
    leaves that window means moving `credentialDayStart` with it, not only a move past
    05:00: a sweep at 04:40 has its retry running until 05:10, across the day's end,
    and the day of one at 02:30 ends two and a half hours after it. Moving where the day
    ends is itself a leak, once: the credentials issued between the last old boundary
    and the deploy share an expiry with no other credential, and so do those issued
    between the deploy and the first new boundary, which dates each set to those hours
    for as long as it is kept (the README's "What that protects, exactly" says this of
    every move, the one from midnight to 05:00 among them). So a new
    `credentialDayStart` goes out soon after the later of the old start and the new,
    once the log shows nobody has verified since the old start, which leaves the first
    set empty, while a later deploy shortens the second; a start moved earlier can
    instead go out at any time between the new start and the old one, quiet or not, once
    that night's sweep on the old schedule, and its retry if it fails, has finished, and
    with the new revision serving all traffic before the old start, since each set then
    spans at least a day less the move. Changing `EDUCHAN_VERIFY_CREDENTIAL_TTL` is the
    same leak on the day it is deployed: the stored expiry is `credentialDayEnd(issued)`
    plus the lifetime, so that day's credentials before the deploy get one expiry and
    those after it another, which the sweep's one id does not merge, and each set is
    dated to its side of the deploy until another day's credentials share its expiry:
    for a lifetime cut by whole days, those issued that many days earlier (the later
    set, at once) or later (the earlier set); for one lengthened, or changed by part of
    a day, never. The README says to deploy such a change soon after 05:00 UTC, before
    anyone has verified that day.
  - **PostgreSQL dates rows the schema does not, so the sweeper rewrites the
    credentials every night.** Every row carries `xmin`, the id of the transaction that
    wrote it, and posts carry `created_at`, so a credential's `xmin` among the posts'
    dates its issuance to the minutes between two of them; freezing keeps `xmin`, and
    `ctid` follows issuance order. `RewriteCredentials` (`store.go`), the sweep's last
    step, works on a connection of its own, with `lock_timeout` at five seconds,
    `client_min_messages` at `warning` (so that neither `DATABASE_URL` nor a role's or
    the database's default can hide the `55P03` below), a handler that collects
    notices, and pgx's `CancelRequestContextWatcherHandler`. A cancelled sweep
    (SIGTERM, Ctrl-C) must have asked PostgreSQL to stop the statement in flight by
    the time it returns and the process exits: PostgreSQL does not notice a closed
    socket until the statement ends, so a `VACUUM FULL` nobody cancelled runs on,
    holding its table, and commits. Two things see to it, and either alone does
    while PostgreSQL answers promptly. The handler sends the cancel request at once
    and has the statement wait up to `cancelGrace` (three seconds) for PostgreSQL's
    answer, so the statement has stopped, with PostgreSQL's `57014`, before the
    rewrite returns. pgx's default, the fallback when no answer comes in time, gives
    up on the socket and sends the cancel request from a goroutine, and
    `closeSweepConnection` waits up to `cancelGrace` for that goroutine
    (`sweepConnection`). `TestCancelledSweepStopsItsStatementOnTheServer` fails only
    without both, and its check that the error is PostgreSQL's fails without the
    handler; `TestCancelledSweepWaitsForPgxsOwnCancelRequest`, where the handler's
    cancel request never arrives, fails without the wait.
    First it deletes the codes that expired unconfirmed, which a request prunes too, but
    nothing says when one will come: an abandoned code's row keeps the time it was asked
    for, to the microsecond, under the address's email hash, which for an address that
    already holds a credential (a second browser's request, say) is the credential's,
    and says it was issued before then (`TestSweepDeletesCodesThatExpiredUnconfirmed`).
    The delete must come no later than the rewrite, since the vacuum below is checked
    against the rewrite's transaction id, which says nothing of rows a later transaction
    deleted; no test holds that order. And it is a statement of its own, before the
    rewrite's transaction rather than in it: a confirmation locks its code's pending row
    before it writes to the credentials table, so a delete waiting for that row while
    holding the table would deadlock with it
    (`TestSweepDeletesExpiredCodesWithoutHoldingTheCredentialsTable`). A code still in
    flight stays, and still confirms. Then it locks
    `verification_credentials` `IN EXCLUSIVE MODE` in a read-committed transaction
    (`CredentialLabel` reads on; everything else on the table waits, and holds the
    lock up: writers and row lockers, `VACUUM`, `ANALYZE`, index builds, schema
    changes, another sweep), deletes every row and inserts the returned copy back
    `ORDER BY random()` in one statement, and notes the transaction's id.
    `lock_timeout` bounds the wait, not the hold: confirmations queue for the
    rewrite's statement too, which grows with the table (about three seconds for
    220,000 credentials on a development machine). `restampScans` then reads each
    verification table whole and through each of its indexes, which it finds in the
    catalog, so every last-scan time `pg_stat_user_tables` and `pg_stat_user_indexes`
    show for them is the sweep's rather than the last confirmation's; and it reads them
    again after the vacuum, once `statsHandOver` (65 seconds) has passed since the
    rewrite committed, a wait that a cancel ends at once
    (`TestCancelledSweepStopsWaitingForTheStatistics`). A connection hands its counts
    to the statistics when one of its transactions ends, at most once a second, within
    ten seconds once it sits idle, and up to a minute later if it finds an entry
    locked, with the end of its latest transaction by then, or the moment of the
    hand-over, as their time. So the counts of a confirmation just after another
    request on the same server connection could reach the statistics after the first
    read, with the time of the next request that connection served, and date the last
    issuance before the sweep to within ten seconds
    (`TestSweepLeavesNoScanTimeFromBeforeIt`, which fails without the second read: it
    holds the server's next request on a lock until the statistics have the first
    read, since a connection hands nothing over while a statement of its runs, and
    fails if the confirmation's counts were in by then; it sweeps from an `App` of its
    own, as the Job does, and waits the 65 seconds out). The app package's harness sets
    the wait to zero for every other test there; `cmd/server`'s `-sweep` tests build
    their `App` with `New`, as the Job does, and wait the full 65 seconds per sweep,
    three sweeps in all. Then `VACUUM (FULL, SKIP_LOCKED, SKIP_DATABASE_STATS)` on each table,
    outside the transaction, writes both tables to new files without the dead versions
    — or the deleted pending rows, with their email hash and an expiry to the
    microsecond — and drops the old files, which are not wiped (the README's
    "Transaction ids, and the nightly rewrite" ends on what that leaves).
    `SKIP_LOCKED` passes over a table another session holds, with a
    `55P03` warning, rather than queueing every badged post behind the vacuum's wait
    (`TestBadgedPostsDoNotQueueBehindTheSweepsVacuum` watches `pg_locks` for that wait,
    which at `vacuumLockTimeout` a try is too short to time). It covers only the table
    it names: once `VACUUM FULL` has the table it waits, holding the table, for what
    else it needs — before the copy, its TOAST table's lock, which a `VACUUM` of the
    TOAST table by name holds; after copying the whole table, its row in `pg_class`,
    which an open `GRANT` or `REVOKE` on the table has updated, and each index's lock,
    which an open `COMMENT ON INDEX` holds. (Autovacuum takes `pg_statistic`'s TOAST
    table on its own, below, but not these: a TOAST table takes its table's
    `autovacuum_enabled = false`, so autovacuum comes to them only near transaction id
    wraparound, which the nightly `VACUUM FULL` keeps from coming due.) A run that
    waited after the copy would hold every request on the table up for the whole copy
    and then give the table up, and do the same on every try. So `vacuumFull` looks
    first (`vacuumWaitsQuery`: `pg_locks` on the TOAST table and the indexes, and the
    lock an open transaction holds on its own id, the `xmax` of a `pg_class` row it
    updated) for a session holding any of these that does not hold the table itself,
    which `SKIP_LOCKED` passes over anyway, and while it finds one runs nothing and
    tries again later (`TestSweepRunsNoVacuumWhileItWouldWait` counts the runs in
    `pg_stat_progress_cluster`, on a table big enough to take a while to copy). What
    the look cannot see coming, a `GRANT` begun during a run, `lock_timeout` bounds:
    `vacuumVerificationTables` first sets it to `vacuumLockTimeout` (100 ms), and a run
    that times out, with `55P03` as an error, has given the table up and is tried
    again like a skipped one, by which time the look sees the holder
    (`TestVacuumLetsGoOfATableWhileAnotherLockItNeedsIsHeld`: one run, then none until
    the `GRANT` ends; a test's role cannot hold a TOAST table, so no test holds one).
    A `GRANT` that commits while the run waits for it, as an autocommit one does within
    milliseconds, ends the run sooner, with `XX000` and "tuple concurrently updated"
    (it cannot update the row the `GRANT` changed), and `vacuumFull` counts that as
    given up too, by code and message, since every internal error shares the code
    (`TestVacuumTriesAgainWhenTheGrantItWaitsForCommits`); any other error fails the
    sweep at once.
    That bound covers the whole statement, and a `VACUUM` ends, after its run has
    committed, by updating the database's row in `pg_database`, which waits for any
    transaction that has updated that row (an `ALTER DATABASE`, a `GRANT` on the
    database) and holds no table; timed out there, a run that had done its job looked
    given up, and the sweep ran it again or failed. So the runs leave that step out
    (`SKIP_DATABASE_STATS`; autovacuum takes it on its own), and `vacuumFull` counts a
    run that times out as given up only while the table is still in its old file
    (`TestVacuumDoesNotWaitForTheDatabasesRow`, which counts its sweep's runs on
    `pg_statistic` as the sweep saw them, through `Store.vacuumed`: every schema in the
    database shares the catalog, and another process's sweep moves its file too, as
    `cmd/server`'s tests, run alongside, do). None of these does anything about the
    run: once `VACUUM FULL` has a table and its locks it holds it against reads as well
    as writes until it has written it out (between about half a second and two and a
    half seconds at 220,000 credentials on development machines), every badged post and
    confirmation waits for it, and three waiting on one instance hold its whole pool. So
    each table is taken once a night, and never for a run that cannot do its job, unless
    a session begins to hold what the run needs during it, which costs that one run.
    `VACUUM FULL` keeps any deleted row an older snapshot could
    read: any statement or transaction in the database that began before the rewrite
    committed and is still running, the server's own requests included, or a
    transaction anywhere on the instance that had written something by then. So before
    each attempt the sweep runs `VACUUM FULL` on `vacuum_probe` (`0016`), an empty table
    nothing else touches, which computes the same cutoff in milliseconds and holds up
    nobody, and takes the verification tables only once the probe's `relfrozenxid` is
    newer than the rewrite's transaction id, which `VACUUM FULL` makes true exactly
    when it drops those rows. It checks each verification table's `relfrozenxid` the
    same way afterwards, and otherwise probes again, backing off, for up to
    `vacuumPatience` (a minute) before failing
    (`TestBadgedPostsDoNotQueueBehindVacuumsAnOlderSnapshotWouldSpoil` counts the runs).
    It also skips, with a different warning, a table its role owns neither itself nor
    through owning the database: an unchanged `pg_relation_filenode` with no `55P03`
    fails the sweep at once, and the sweeper connects as the role that ran the
    migrations, which owns `vacuum_probe` too. Owning the tables is not enough for
    `pg_statistic`, below, so that role must also own the database or be a member of
    the owner's role that inherits its privileges.
    `0015` sets `STATISTICS 0` on every column of both tables, which keeps `ANALYZE`
    from copying email hashes beside their expiries into `pg_statistic`, deletes what
    it had copied, and turns autovacuum off on both, since the times it last ran follow
    the writes; the nightly `VACUUM FULL` is their vacuum. Deleting the samples takes
    them out of `pg_stats` only: their old versions stay in `pg_statistic`'s file and
    its TOAST table's (the email hashes' histogram is stored out of line), and in every
    backup of those files, until the catalog is written out anew, and no query can see
    whether they are there. So the sweep runs the same `VACUUM` on
    `pg_catalog.pg_statistic` every night, with the same `relfrozenxid` check against
    the rewrite, which is later than `0015`, in the same passes as the verification
    tables: after them in each, but not waiting for them, so a table left for the next
    pass does not hold the catalog back, nor the catalog a table
    (`TestSweepFailsWhenTheVerificationTablesAreHeld`). It takes milliseconds, but while
    it runs a query whose plan needs statistics its connection has not cached waits, on
    any table, which is why it too skips a held catalog (a session reading `pg_stats`
    holds it), and why the look before each run matters most there: `0015` left
    autovacuum on for the catalog, TOAST table included, so the sweep leaves the
    catalog alone while autovacuum holds its TOAST table. Only a
    superuser or a role with the privileges of the database's owner — the owner, or a
    member of its role that inherits them, which `pg_has_role(datdba, 'USAGE')` says —
    may vacuum that catalog; a role that owns every table but not the database may
    not, nor a member granted `WITH INHERIT FALSE`, and anyone else gets it skipped
    with a warning, which fails the sweep with its own message, naming that query,
    every night (`TestSweepFailsWhenVacuumLeavesPgStatisticInItsFile`). The README's
    archive sweeper section tells the operator, and its step 5 says how to check the
    role, on Cloud SQL too, where this has not been tried.
    `TestVerificationTablesKeepNoStatistics` checks that the sweep moves the catalog and
    its TOAST table to new files with a cutoff past `0015`; no test can read an old
    version without a superuser's `pageinspect`, which is how the leak was found. A
    new write path into the credentials table re-dates the rows it writes until the
    next sweep, so treat one like a new column, and a new column on either table gets
    `SET STATISTICS 0` in its migration. The README's "Transaction ids, and the
    nightly rewrite" says exactly what this protects — the verification tables of the
    live database after the 03:17 UTC sweep, where nothing dates a credential more
    closely than the day its expiry gives but the sweep's own id, which says of one of
    the day the sweep ran in that it came in the 22 hours of that day before the sweep —
    and what it does not, a sweep run after 05:00 UTC among it, which splits the day it
    runs in until the next sweep. Above all, the posts: every code request that reaches
    the database, and every attempt at a code in flight, is a write, takes the next
    transaction id, and so leaves a gap in the posts' `xmin` between the two posts
    written around it, which one plain query on `posts` finds, in the live database and
    every backup, for as long as those posts exist. That places each issuance between
    two posts whatever the sweep does; the rewrite takes away only the credential row's
    own issuance id (`TestIssuanceLeavesGapsAmongThePostsTransactionIDs` pins this as a
    known limit, and only verification tables on a PostgreSQL instance of their own
    would close it). Nor does it cover a credential issued since the last sweep, a code
    in flight as the sweep ran (its row keeps the time it was asked for, and once a
    confirmation, or the delete of the expired code, removes the row, its dead version
    keeps that time in the table's file until the next sweep's `VACUUM FULL`; its email
    hash joins its address's credential if there is one; and the hour was chosen for the
    backup window, not for quiet — 03:17 UTC is morning to afternoon across South and
    East Asia and Australia — so expect one on any weekday), backups (taken from 05:00
    UTC, up to 09:00, seven kept), a night the sweep failed, autovacuum times recorded
    before `0015`, or the write-ahead log the server keeps for crash recovery, which
    dates a credential until PostgreSQL deletes or reuses the newest segment holding its
    row. With full-page writes on, the log since the checkpoint before the sweep holds
    every credentials page as it stood before the rewrite, issuance ids and all — the
    sweep's own delete logs the image of each page no change since that checkpoint
    already had — so that segment is the sweep's or close to it, not the issuance's.
    Point-in-time recovery stays off and `track_commit_timestamp` stays off
    (`deploy/README.md` section 2.2); changing either means changing that README section
    in the same commit.
    `TestSweepTakesIssuanceTimeOutOfCredentials`,
    `TestSweepDeletesCodesThatExpiredUnconfirmed`,
    `TestSweepDeletesExpiredCodesWithoutHoldingTheCredentialsTable`,
    `TestSweepStoresCredentialsOutOfIssuanceOrder`,
    `TestCredentialConfirmedAsTheRewriteStartsIsRewrittenOnce`,
    `TestCredentialConfirmedDuringTheRewriteIsKeptOnce`,
    `TestSweepFailsWhenTheVerificationTablesAreHeld`,
    `TestSweepFailsWhileAnOlderTransactionCanReadTheOldRows`,
    `TestSweepWaitsForAnOlderTransactionToEnd`,
    `TestSweepFailsWhenVacuumLeavesATableInItsFile`,
    `TestSweepFailsWhenVacuumLeavesPgStatisticInItsFile`,
    `TestBadgedPostsDoNotQueueBehindTheSweepsVacuum`,
    `TestSweepHearsVacuumsWarningsWhateverTheConnectionAsksFor`,
    `TestSweepRunsNoVacuumWhileItWouldWait`,
    `TestVacuumLetsGoOfATableWhileAnotherLockItNeedsIsHeld`,
    `TestVacuumDoesNotWaitForTheDatabasesRow`,
    `TestSweepLeavesNoScanTimeFromBeforeIt` and
    `TestVerificationTablesKeepNoStatistics` hold it.
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
    from the record. Once the nightly sweep has rewritten it, the row dates an
    issuance to the day; a label stamped to the second would date it exactly whenever
    it was its institution's only one that day.
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
  `internal/app/busy_test.go`, `internal/app/filter_api_test.go`, and
  `TestClientKnowsTheServersImageRefusals`,
  `TestClientReadsTheContentRulesAnswersTheServerGives`,
  `TestClientKnowsWhenNoCodeIsPending` and `TestAnExpiredCodeIsSaidToHaveExpired` in
  `internal/app/web_invariants_test.go`) run the whole app against an isolated schema in
  `TEST_DATABASE_URL` (falling back to `DATABASE_URL`), so they are safe to run against
  the development database. `testServer` builds its `Config` directly, which leaves every
  cooldown at zero unless a test asks for one, and gives the client a cookie jar so the
  two-step verification exchange behaves as it does in a browser. `verifyServer` adds a
  secret and a capturing `mailer.Sender`, which is how a test reads the code back; the
  credential it returns is then presented explicitly with `postWithCredential`, because
  nothing attaches it on a caller's behalf. Poster ID tests post through `browser`
  (`poster_id_test.go`), which sends what the client sends: the credential alone on a post
  that asks for a badge, and the poster key on any other. A post carrying both is never
  sent by the client, and the tests that cover it set the headers explicitly.
  Tests post to boards they create with `addBoard` (`api_test.go`), never to seeded
  ones, so they do not depend on which boards the migrations seed;
  `TestBoardsAreSeeded` is the one test that names the seeded set.
  A migration's effect on existing data is tested by stopping a schema at an earlier
  version with `databaseAt` (`migrate_test.go`), writing rows into it with plain SQL, and
  starting a server on it with `serverOn`, which applies the rest. Sweep tests call
  `app.sweep` with a small batch to cover the loop, and backdate `archived_at` with
  `archiveThread` rather than waiting. `cmd/server/main_test.go` runs the `-sweep` mode as
  a whole process against its own schema in the same database.
