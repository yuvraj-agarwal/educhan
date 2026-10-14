# Agent guide — educhan

educhan is an anonymous imageboard for students, open to anyone aged 13 and over. This
repository contains the working prototype: a Go HTTP API plus an embedded web UI, backed
by PostgreSQL. Boards, anonymous posting (tripcodes, per-thread poster IDs), image
attachments, a paginated index, a catalog view, a per-board archive, threads with quote
links and post markup (spoilers, highlighted code, maths, cross-board links), per-client
flood control, content rules that send back or hold posts (with a moderator queue for the
held ones), key-authenticated moderation and board management, and optional
institutional verification badges are implemented. Janitor accounts, bans, and reports are
not.

## Stack & layout

- **Go 1.22** (standard library HTTP, `log/slog`), **PostgreSQL 16**, `pgx`/`pgxpool`,
  `golang.org/x/image` (WebP decoding and thumbnail scaling).
- `cmd/server` — entrypoint (config, logger, graceful shutdown), plus the one-shot
  `-sweep` mode that deletes archived threads past their board's archive period (one
  with a held reply leaves public view instead, until the reply is decided) and exits.
- `internal/app`
  - `app.go` — wiring and routes; `config.go` — environment configuration.
  - `handlers.go` — HTTP handlers, validation, JSON responses.
  - `attachments.go` — upload parsing, media serving.
  - `moderation.go` — moderation key check, the sticky/lock/delete handlers, and the
    held-post queue and approval.
  - `filter.go` — the content filter: normalisation, the rule tables and validators,
    and `checkContent`, which every post goes through before it is stored.
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
  browser's "Failed to fetch". A `429` is worded for what was refused: `/verify` passes
  `errorMessage` `{ action: "code" }`, so a code request is told to wait and request a
  code again, never to post again. A not-found page names and links a board only when it is
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
between two letters read as `i`, and `l` read as `i`). Patterns are tables of Go regexps compiled for
both views (`bothViews`, through `canon`, which makes the same `l`→`i` change in the
pattern; `TestFilterPatternsSurviveCanon`) or for the plain view only (`plainOnly`, for
anything whose `@`, dots or slashes matter), plus small validators (`matchCheck`).

| Outcome | Rule / reason | Built from |
| --- | --- | --- |
| block, `422` | `contact` | `firstNumber` (one pass over the plain view; groups split by up to three spaces, line breaks, `-`, `.` or `_`, or a bare `,`, and `o`/`l` beside digits read as `0`/`1` (`digitLookalike`); `formattedNumber`, `dataSet`, `roundRange`, `decimalRange` and `ipv4` let decimals, thousands, lists, ranges and IPv4 addresses through; `classifyNumber` with `standsAlone` (maths on either side through `operandBefore`/`operandAfter`, a label (`labelled`: `numberLabels`, which include a program's "output" and "returns", with an edition such as "ISBN-10" and up to three `labelLinks` between; an account label after an `accountApp`, "whatsapp acc", is not one) or quantity word, currency, a bracket glued to a name; a `contactCue` straight before the number, "call me =", "wa/", "my number is", outweighs an operator or a maths word near it), `internationalPhone`, `indianMobile` (`mobileGrouping`; single digits only when every separator is of one kind, `oneSeparatorKind`), `nanpPhone` (`nanpSeparators`, or a `phoneCue` before spaced groups), `ukPhone`, `auMobile`, `dramaNumber`, `helpline` (`helplineNumbers`: three Indian lines and four US ones)), `emailRule`/`providerEmailRule` (`providerEmail`)/`spelledEmailRule` (`emailTLDs` and `academicSecondLevel`, `exampleDomain`, `spelledEmail`, which lets `codeShape` through at a bare "at" ("Q3 at leetcode.com") except at a mail provider or a university, and a `dottedAbbreviation` ("B.Tech", "vol.1"), an `ordinalWord` ("2nd-year") or a `toolNames` version ("Python3.12") anywhere but a mail provider; bracketed `[.]` and `[@]`), `myMailRule`, `mailColonHandle` (a colon, a dash or an arrow, not the "=", "=>" or ">" of code), `linkRule` + `contactLink` (a slash may be spelled, `linkSlash`; a path may start with Signal's `#`), `spacedInviteRule`, `askRules` + `askCheck` (`topicWords`, and `rankedNumber` for "number one" and "number of"), `atAskRule`, `myAppRule` + `myAppCheck` (`notAHandle`), and `handleRules` (`appColonHandle`, `appAtHandle`, `appLabelHandle`, `hindiHandle`, `handleBeforeApp`; `handleSeparator` between an app and a handle) + `handleCheck` (`plausibleHandle`, `measureShape`; for `handleBeforeApp`, `handleFirstCheck` and `toolNames`: "python3 on discord"; for `appLabelHandle` with no label, `labelHandleCheck`, `toolNames` and `dottedAbbreviation`: "ig python3 is better", "TG B.Tech") |
| block, `422` | `id_number` | `aadhaar` (Verhoeff), `ssn`, `niRule` + `validNIPrefix` |
| support, `422` | — | `supportRules` (first person only) filtered by `supportCheck` (idioms such as "die of embarrassment", "cut off", "end it with her", "ending things with my boyfriend", "burning myself out", "live in the hostel" and "going on to a PhD", through `livingCheck`; `kmsCheck` for "5 kms" and AWS KMS), and `asksForAMethod` (`selfHarmMethodRules` + `methodCheck`) |
| hold, `202` | `threat` | `threatens` over every field at once (a subject and a body pair as if adjacent): `threatIntentRules` (the first person with `threatModals` and up to two `threatAdverbs`, swear words and "go" among them; "someone should"; "i planted a bomb"; some Hinglish) near (`threatWindow`, counted in `readingPositions`: letters and digits, and one for each gap however it is filled, so invisible characters and emoji pad nothing) `threatPlaceOrTimeRules`, `schoolShootingRules` near `futureTimeRules`, filtered by `threatCheck` (film and sports shoots, games but not for "there will be a shooting" or "a bomb", a quotation with a verb of saying beside it (`sayingWords`), a condition that is not a demand (`harmlessCondition`: not "if you tell anyone", not "unless"), a metaphor in an exam, the markets or the polls, a drill, hurt feelings and killing with kindness (`harmlessObject`), hyperbole such as "die of boredom", "die in the exam", "die when you see the results" or "when the results come out"); `namedTargetRules` with `namedTargetCheck` (a name with a capital, or any after a title such as "mr" or "sir"; not an exam, subject, team, acronym or someone's occasion: "kill JEE", "kill Rahul's wedding"); `schoolShootingCheck` (a drill, an essay, a debate); `threatWarningRules` near `threatWarningCues` |
| hold, `202` | `self_harm` | `selfHarmRules` filtered by `selfHarmCheck` (`notNegated`: a negation ending the same clause, as in "don't you dare kill yourself" or "a bad rank doesn't end your life", not one anywhere before it, and a warning, "careful" earlier in the sentence or "or" before the modal; idioms such as "die your hair" and "a hill to die on", games after the words or after "in" before them, and "self-" compounds, and "delete yourself from" a group) and `selfHarmMethodRules` filtered by `methodCheck` (a how-many question in either order needs a medicine named in its sentence, so "how many volts would it take to kill you" is not one) |
| hold, `202` | `grooming` | any `ageQuestionRules` match that `ageCheck` keeps (a question, or an age looked for as a number, `ageNumber` and `ageUnit` before a `youthNoun`: "any 14yo girls", "13-15 yo girls"; "our 15 year old students are doing well" matches too and is held only beside a cue) (or `aslRules` with `aslCheck`: "asl" opening what is said, not "bored asl" or "ASL club") and any `privateCueRules` match that `cueCheck` keeps ("call me crazy", "meet the deadline", "between you and me, the food is awful" and "seniors se baat karo" are not cues) in the same post |

Precedence is block, then support unless `support_seen`, then hold (threat, self-harm,
grooming), then publish. A question about a method ("how many pills would kill me")
gets the support screen first and is held once the poster goes on, because the person
asking may be the one at risk (`TestFilterAsksAboutAMethodBeforeHoldingIt`). Every
regexp is RE2 (linear), the number scanner and the view builders are single passes, and
the pairing checks walk sorted span lists, so the cost is linear in the post
(`TestFilterCostIsBounded`, `BenchmarkCheckContent`: about 30–40 ms for a maximum-length
post on one 2.1 GHz core, up to about 400 ms for the slowest crafted one — characters
that each fold to `maxFoldRunes`, such as `㏂` — which is why refusals are budgeted; see
the API contract). The plain view is skipped when the loose one is
only it with `l` read as `i` (`looseOnly`).
**Student-ID formats** (2.9's "configurable student-ID formats") are not built; the
extension point is the comment at the end of `filter.go` — a validator beside `aadhaar`
or a `plainOnly` rule beside `niRule`, loaded from configuration, with its own
`findingKind`. A new rule needs true and false positives in `filter_test.go`; the false
positives are the harder half, and the table there (maths, dates, ISBNs, scores, prices,
bare app names, "this exam is killing me", history questions, Hinglish and Hindi) is the
floor. The rules read English and some Hinglish (Hindi in Latin letters: "insta pe add
karo", "mujhe marna hai", "goli maar dunga"); Devanagari and other languages are not
read, and the README says so.

**Be honest about the limits**, in code, docs and copy. Keyword filters are easy to get
round (spelled-out digits, a separator `groupSeparator` does not know, a bare username,
contact details inside `[math]`, an email under a TLD not in `emailTLDs` (unless it is an
academic second-level domain, `academicSecondLevel`: "edu.tr", "ac.il"), any language
the tables are not written in) and they have false positives (gaming trash talk held as
self-harm, a countdown blocked as a phone number, a dose question held, a twelve-digit
reference that passes the Verhoeff check). That is why a block quotes exactly what it
matched and a hold waits for a person.

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
Approval does not bring it back; the next sweep deletes it
(`TestHeldReplyKeepsItsThreadFromTheSweep`). The image cap counts published posts only, so
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
  format and the header's dimensions without decoding, and `Process` runs it first, so
  the early refusal in `parseSubmission` and the full one in `media.Save` cannot
  disagree about a file's header.
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
  There are no down migrations, so a rollback runs old code on a newer schema: `migrate`
  logs an error naming the versions it does not know, and `App.schemaAhead` makes the
  sweeper refuse to run. A rollback across `0005` is unsafe whatever the code says —
  phase-0 code shows archived threads as live and its prune deletes them, leaving
  their files behind (`deploy/README.md`). So is a rollback across `0009`: older code
  does not know `held_at`, so it shows held posts and held threads publicly. Under the same lock, after the migrations,
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
  its key. It logs one record of counts and nothing that names a board or thread,
  failures included, and the error it returns has the key taken out
  (`TestSweepLogsOnlyCounts`); it returns an error — a non-zero exit — on a database
  failure, a file it could not remove or did not find, or a database a newer release
  migrated (`TestSchemaFromANewerReleaseStopsTheSweeper`). It builds its App with `New`
  like the server, and runs without the salt, the moderation key and the verification
  secret on purpose, so `New` warns about none of them: those warnings are
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
  other; `allowAll` is what enforces that. Nothing is refunded once a cooldown starts,
  so every refusal that can be known up front comes before `allow`: a malformed poster
  key, the body, a badge the poster cannot have, uploads switched off, a file its header
  shows to be unusable (`media.Inspect` in `parseSubmission`: not an accepted format,
  or more than `media.MaxPixels`), and a reply the thread would refuse
  (`ReplyRefusal`). Decoding the pixels is not up front, because it is the cost the
  upload cooldown limits, so an image that fails to decode is refused by `storeUpload`
  after `allow` (`TestImagesRefusedByTheirHeaderStartNoCooldown` pins both sides).
  A post the content rules refuse starts no cooldown and is counted by `refusalBudget`
  instead, and a post a cooldown would refuse is answered by `notCoolingDown` (the same
  check, recording nothing) before the rules read it (see **API contract** under
  Content rules).
  State is per process and in memory — a shared backend means replacing
  `cooldownLimiter` and `refusalBudget`, which nothing else depends on.
  The keys carry client addresses, so the limiters are also a privacy surface: `New`
  builds a `cooldownSweep` over all four limiters and the refusal budget (anything
  `expiring`) and starts its timer, which drops every expired entry each
  `cooldownSweepInterval` (one minute), and `Close` stops it.
  **While the process has CPU, an address is held in memory no longer than the
  longest cooldown that applies to it plus one minute**, counted from its last action
  that started one (for the refusal budget, from the first refusal of its minute),
  busy server or quiet. A host that gives CPU only during requests,
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
  `internal/app/filter_api_test.go`, and
  `TestClientKnowsTheServersImageRefusals` and
  `TestClientReadsTheContentRulesAnswersTheServerGives` in
  `internal/app/web_invariants_test.go`) run
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
