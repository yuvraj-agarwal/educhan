# Agent guide — educhan

educhan is an anonymous imageboard for students, open to anyone aged 13 and over. This
repository contains the working prototype: a Go HTTP API plus an embedded web UI, backed
by PostgreSQL. Boards, anonymous posting (tripcodes, per-thread poster IDs), image
attachments, a paginated index, a catalog view, a per-board archive, threads with quote
links and post markup (spoilers, highlighted code, maths, cross-board links), per-client
flood control, key-authenticated moderation and board management, optional institutional
verification badges, and the optional educhan Pass (a captcha skip sold on Stripe's hosted
checkout) are implemented. Janitor accounts, bans, and reports are not.

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
  - `pass.go` — the educhan Pass: its handlers, code format and SQL; `pass_stripe.go`
    — Stripe over plain HTTPS (hosted checkout, the price, webhook signatures);
    `pass_config.go` — its settings: off unless `EDUCHAN_PASS_SECRET` is set, every
    other one refused without it, and `EDUCHAN_PASS_SALES` to pause sales.
  - `verify.go` — institutional verification: handlers, credential format, cookies;
    `verify_domains.go` + `verify_domains.txt` — the academic domain allowlist.
  - `store.go` — all SQL but the Pass's (`pass.go`); `models.go` — the JSON-facing types.
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
curl -s localhost:8080/api/site   # -> {"uploads":true,"max_upload_bytes":8388608,"pass":false}

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

Without `EDUCHAN_PASS_SECRET` the Pass is off, and the server will not start if any other
Pass setting is set; with it, not unless `EDUCHAN_STRIPE_SECRET_KEY`,
`EDUCHAN_STRIPE_WEBHOOK_SECRET`, `EDUCHAN_STRIPE_PRICE_ID` and `EDUCHAN_PUBLIC_URL` are
all set (`pass_config.go`). A whole purchase needs Stripe to call the webhook, so its
end-to-end checks live in `pass_api_test.go` against a fake Stripe (`fakeStripe`) rather
than in curl, and they cover `EDUCHAN_PASS_SALES=paused` beside the default `open`
(`TestPassSalesPaused`). Paused, checkout answers `403 the Pass is not on sale right now`
and `GET /api/pass` says `"paused": true`; every other Pass endpoint and the webhook go
on as usual. With `EDUCHAN_STRIPE_AUTOMATIC_TAX` on, the Price's own `tax_behavior`
decides what the page says of tax: `GET /api/pass` adds `"tax": "included"` for an
`inclusive` Price and `"added"` for an `exclusive` one (the page's "tax included" and
"plus any tax"), and a Price with neither is not sold: the status leaves the price out
and checkout answers `503 the Pass can't be bought right now`, as it does for a Price
archived, recurring or without a fixed amount (`TestPassRefusesAPriceItCannotSell`). Its
`502` before any cooldown comes only while the Price can't be read, which might say
neither (`TestPassTaxFollowsThePrice`). A failed read or a refused Price is kept a minute
and a good read an hour (`TestPassPriceIsKeptForItsTime`); Stripe never changes a Price's
amount, so a new amount takes a new Price ID and a restart. Checkout always sends
Stripe `automatic_tax[enabled]`, `false` with the setting off, so an account that
collects tax by default adds none the page did not show. A real checkout uses Stripe's
test mode: test keys, a test Price (one with a tax behavior, for Stripe Tax), and
`stripe listen --forward-to localhost:8080/api/pass/stripe-webhook` for the webhook,
with `EDUCHAN_PUBLIC_URL=http://localhost:8080`. A lost Pass is reissued, behind
`EDUCHAN_MOD_KEY`, with `curl -X POST localhost:8080/api/pass/reissue -H 'X-Mod-Key: …'
-H 'Content-Type: application/json' -d '{"payment_intent":"pi_…"}'`, which answers with
a one-time reissue code (`REISSUE-` and seven groups of four) for the owner to send the
buyer, never a Pass code and never a link; `POST /api/pass/claim` takes it in its body,
with a nonce of its own (`{"reissue","nonce"}`).

CI runs no browser, so the client's privacy and safety invariants are pinned by Go tests
that read `app.js` (`web_invariants_test.go`): the poster key, the credential and the
Pass only in `submit` (the Pass also in the Pass page's own requests), the Pass never
beside the credential, a reissue code only typed, sent once in a claim's body and never
kept, what a claim brings stored before anything looks at the page, a renewal checked
against the Pass held before it is reported, and the Pass held checked when a lost
answer's retry is refused, an answer about a Pass that has left the browser dropped,
every Pass answer drawn only on the Pass page, a claim's nonce and the forgetting of a
claim, a reissue code's nonce kept through a refusal and dropped by Remove and Forget
but not by Stop, nothing collected while a claim is being dropped, a code copied here
with Copy the code emptied from the clipboard by Remove and Forget, another tab's
removal and forgetting followed, the age screen storing its outcome alone and sending
nothing, the badge's 18+ box and the Pass's never ticked in advance or kept, `/api/site`
fetched only past the age screen, the header's Pass link only where there is a Pass, the
paused wording the server's own, KaTeX's options, and the highlighting cap. They are a
floor, not a proof; a change that trips one has changed one of those properties and
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
- The educhan Pass lives in `localStorage` under `educhan:pass`, as the code and its
  expiry, or the code alone marked run out. `submit` sends it in `X-Educhan-Pass` on
  unbadged posts only (a badged post sends the credential and nothing else), and the Pass
  page sends it in the body of the requests that use it on purpose: checking a code
  (`/api/pass/activate`, one typed in or the one held, checked again) and renewing
  (`/api/pass/checkout`). "Show the code" puts it on the screen and nowhere else, and
  "Copy the code" on the clipboard, which Remove the Pass and Forget this browser empty
  again inside their click, whatever is on it by then, when this tab copied it with Copy
  the code since it was loaded and no copy or cut in this tab has certainly put something
  on the clipboard since, unless another tab has since removed or replaced the Pass
  (`passCodeCopied`, `emptyCopiedPassCode`, `followPassStorage`). A document event that
  may have put nothing there does not count, so the two err toward emptying the clipboard
  (`forgetCopiedPassCode`): a `copy` counts when a text box's own selection holds
  something, where the box says, when the document's does for an email or number box, and
  anywhere else (a checkbox, the file picker, a button, a link or the page) when the
  document's selection holds text of the page's own, since Chromium goes on reporting a
  text box's text there, in a collapsed range, once the focus has left the box; a `cut`
  counts only that way and in a box that can be typed in, neither read-only nor disabled,
  or in editable content; `navigator.clipboard`'s own write fires neither. A code copied
  any other way, by hand included, stays until something else is copied, and the Pass
  page, the Verify page and the FAQ name the button, say that it empties the clipboard
  even after a copy educhan can't see took anything (from an email box, in some browsers),
  and promise no more than that. Buying and renewing leave for Stripe's hosted page by
  `location.assign`: no Stripe script, frame or stylesheet ever loads on this origin, and
  the CSP has no exception for Stripe. While `GET /api/pass` says `"claim"`, the page
  draws a collecting state with no form to pay with (or, with a Pass already here, says a
  checkout from this browser hasn't been collected yet, which may be a payment Stripe is
  still confirming, and keeps Renew) and asks `/api/pass/claim` on every visit, with a
  pause that grows while it is open (`collectPass`). Each checkout's claim holds its own
  order and no other, so starting another stops this browser collecting the one before;
  Start again and the open-checkout note say so, and that a payment made on the earlier
  one after all is still the buyer's, by a reissue code asked for at the support address
  on Stripe's receipt, and the forms drawn again over a checkout left behind
  (`passStartedAgain`: Start again, Stripe's cancel link, or a checkout answered after the
  reader left the Pass page or after the Pass held changed meanwhile, in another tab say,
  which `submitPass` marks before it declines to follow it) keep Stop collecting it here
  (`passEarlierCheckoutHTML`). A checkout still on its way when Remove, Forget or Stop is
  pressed, here or in another tab, is not followed either, and the claim it set does not
  outlive that forget. Pressed in its own tab, the forget calls it off first
  (`callOffPassCheckouts`, the first step of `forgetPassClaim`): one whose answer's
  headers have not arrived (`api`'s `answered`, called once they are in, since by then the
  browser has kept the cookie they set) has its request aborted, and one already answered
  is noted `forgotten`, since the forget's own request then goes after its cookie and
  drops it with the rest. Pressed in another tab, the forget may have gone too soon to see
  the claim the answer set, which is dropped at once by the name the answer gives its
  order (`forgetPassOrder`, `{"forget": true, "order"}`; the claim's hash, signed by the
  server with the Pass secret, so a copy of `pass_orders` makes no name): that claim
  alone, since it sets no mark, moves no counter and clears no cookie, so a checkout
  started after the forget, in any tab, keeps its claim. The dropped claim's cookie can
  stay with nothing under it, and a drop that cannot reach educhan says so on whatever
  page the tab is on, the one notice in the Pass section not kept to the Pass page, in
  words for the forget pressed in the other tab (`PASS_ORDER_KEPT`, by what its mark
  holds, `passForgetHeardLast`) that drop the claim with no more than it did: Stop
  collecting it here again after a Stop, Stop collecting it here after Remove, which
  leaves no Pass to remove, and Forget this browser again after Forget. Checkouts go one
  at a time, since each answer's cookie takes the place of the one before, whichever
  started first: each keeps a mark while its request is on its way (`openPassCheckout`,
  under `educhan:pass-checkout:` and a random token, holding the time), and a checkout
  waits in `passForgetsSettled` until no other's mark is left (`passCheckoutLeft`, each
  for at most `PASS_FORGET_WAIT_MS`), so one pressed after a forget is never overtaken by
  one the forget overtook. Leaving the document (`pagehide`) calls off a checkout not yet
  answered too, and moves `passLeft`, so a press still waiting in `passForgetsSettled`
  then sends nothing (`PASS_LEFT_MEANWHILE`): its button comes back without a notice, and
  a page brought back from the back-forward cache has it ready. A mark older than the
  wait, left by a tab that crashed or was killed before its answer, is taken off when a
  page loads (`passCheckoutLeft`), and Forget this browser takes every one off, after
  `callOffPassCheckouts` (`forgetBrowser`); a tab whose checkout is still on its way puts
  its own back (`keepPassCheckoutMark`), so checkouts still go one at a time. A checkout
  whose answer reaches its tab in the moment before that tab hears of a forget from
  another is followed to Stripe, and keeps its claim unless its cookie was in the browser
  when that forget's request went. Every claim carries the collection's nonce
  (`educhan:pass-nonce`, kept until an answer says nothing more is waiting): the server
  derives the code from the claim cookie and the nonce together, so a claim whose answer
  was lost is asked again and handed the same code, and a nonce is worth nothing beside
  another cookie. What a claim brings is stored (`keepClaimed`) before anything asks
  whether the reader is still on the page, and a code just handed over stays in
  `passJustIssued`, drawn on every drawing, until the reader leaves the Pass page, beside
  any code it took the place of. A renewal's date is never stored from the claim's
  answer, since the Pass it renewed may not be the one held here now: `confirmRenewal`
  checks the held code with `/api/pass/activate` (`recheckPass`, asked once more after a
  `429`'s wait), stores what that says unless the Pass held changed meanwhile, and says
  the Pass is renewed only when the date it checks is the renewal's and later than the
  one it had; a renewal a reissue code brought is said to come from the code, not from a
  payment in this browser. A renewal hands over no code, so its claim or reissue code
  asked again is refused: refused after a retry with a kept nonce, the page checks the
  held code the same way (`passAfterLostAnswer`) and says only that the Pass is now good
  until a later date, never where the time came from. A reissue code is typed or pasted
  into the code box, which takes a Pass code too (`isReissueCode` tells them apart by the
  server's word and length), and never rides in an address. Pressing the button is the
  reader's say-so: a Pass held here is replaced at once and its code shown beside the new
  one, while a Pass code typed over a good one is refused. The code goes once in a
  claim's body with a nonce of its own (`educhan:pass-reissue-nonce`), the Pass code
  derived from the two; it is never stored, and stays in the box, in memory
  (`passCodeTyped`), until an answer arrives, so pressing again after a lost answer asks
  with the same nonce. That nonce is dropped only when an answer says the code issued or
  renewed a Pass, or by Remove the Pass and Forget this browser, never on a refusal: an
  earlier try may have reached the server, so the right code typed after a mistyped one
  still gets the same Pass code. Each try first sets a mark,
  `educhan:pass-reissue-unanswered`, which its own answer takes off unless an earlier try
  had left it there unanswered (unreachable, or a `5xx`); the mark goes otherwise only
  with the nonce, on issued or renewed or by Remove and Forget. Only with an earlier try
  unanswered is a refusal followed by the held code's check (`passAfterLostAnswer`) and
  worded for that try (it may have renewed the Pass; otherwise the code may be mistyped,
  used, run out or cancelled), and a held Pass the check finds no longer valid is removed
  with word that the right reissue code, entered again, gets its new code. Otherwise a
  refusal says to check the code. A code typed or pasted in, of either kind, is sent as
  the server writes it (`canonicalCode`: capitals, the separators `CODE_SEPARATORS` skips
  removed, O as 0 and I or L as 1 after a reissue code's word, whose own I stays as it is
  (`CODE_FOLDS`), dashes between groups). The page and the server skip the same separators
  and fold the same letters, which is what `TestPageAndServerSkipTheSameSeparators` and
  `TestPageAndServerFoldTheSameLetters` hold. The box has `autocomplete="off"`, and Remove
  the Pass, Forget this browser and leaving the page (`render`, `pagehide`) empty it
  (`forgetTypedCode`). Remove the Pass, Forget this browser and Stop collecting it here
  stop the collection under way (`passVisit`) and drop its nonce before they drop the
  claim (`forgetPassClaim`, `{"forget": true}`), and an answer to a claim that arrives
  after one of them is dropped unstored (`passForgets`), so the next person at a shared
  computer collects nothing. Remove and Forget also drop the reissue code's nonce and its
  mark, and `passLost`, and bump `passCodeForgets`, which every other Pass answer (a
  reissue code, a code checked with `/api/pass/activate`, Check it again, a checkout)
  notes before its request: one that arrives after them, or while their request is on its
  way (`passCodeForgetting`), stores nothing (`passForgotten`). Four presses wait while a
  forget is on its way, Stop's included, from this tab or another: Use this code here (a
  Pass code or a reissue code), Check it again, Buy on Stripe and Renew on Stripe. Each is
  sent once every forget the tab knows of is settled (`passForgetsSettled`, which waits on
  `passForgetting` until none is left) and notes the counters then, but
  `passForgetsSettled` notes `passCodeForgets` and `passForgets` as the button is pressed
  too, and if either has moved by the end of the wait the press sends nothing: its button
  comes back and the notice says why, `PASS_FORGOTTEN_MEANWHILE` for Remove or Forget
  pressed meanwhile, here or in another tab, and `PASS_STOPPED_MEANWHILE` for Stop, which
  can come meanwhile only while a checkout waits on another, or from a tab that has given
  up waiting after `PASS_FORGET_WAIT_MS`, since Check now and Stop are off until then. A
  press made after the Remove goes, once every forget then on its way is settled too: a
  checkout started before a forget's request was answered could have its claim dropped by
  it. Stop leaves a reissue code and those answers alone, since it is about a checkout and
  a reissue it dropped would be spent for nothing, though a reissue answer that lands
  after it does not draw back the claim Stop dropped (`passForgets`, read before the
  request). Until the server has that request (`passForgetting`), nothing collects:
  `renderPass` and `collectPass` wait on it, and Check now and Stop collecting it here
  stay off meanwhile, in every tab and in anything drawn meanwhile
  (`PASS_COLLECT_BUTTONS`, which `drawPass` and `stopForPassForget` turn off); turned
  away, Stop turns them back on, says so and goes on collecting. The browser's other tabs
  do all of this too: each forget keeps a mark of its own while its request is on its way,
  under `educhan:pass-forget:` and a random token, holding the forget pressed (`remove:`,
  `browser:` or `stop:`) and the time it was set, so two forgets at once never share one.
  Every forget a tab knows of, its own and those it has heard of, is a wait in
  `passForgetWaits`, from which `settlePassForgetting` makes `passForgetting` and
  `passCodeForgetting`. A tab that sees a mark appear (`followPassForget`, by the
  `storage` event) stops as the tab it was pressed in did (`stopForPassForget`): it drops
  the collection's nonce, for Remove and Forget the reissue code's too, its mark,
  `passLost` and its code box, turns Check now and Stop off, and waits for the mark to go,
  or `PASS_FORGET_WAIT_MS` (30 seconds, which the README states) from the time it holds at
  most (`heedPassForget`), then draws the Pass page if it is on it, as `redrawPass` does:
  focus that was on the page just before the drawing, and that the drawing dropped, goes
  to what took its place, and focus on the body or outside the page stays where it is, so
  a phone's page is not scrolled to a button the reader never chose; Check now or Stop,
  turned off with the focus on it, hands it to its panel meanwhile. A tab opened, or
  brought back from the back-forward cache, while a mark is there heard no `storage` event
  for it, so it looks for marks before it draws anything (`heedPassForgetsUnderWay`, at
  load and on `pageshow`), stops for each it does not yet wait on through
  `stopForPassForget` too, and removes one older than the wait. At load that changes
  nothing, but a page brought back holds what it held when it was left (a collection's
  nonce, a reissue code's, the last holder's news), and the `storage` event for the mark,
  which comes after `pageshow`, finds it already waited on. An answer to a code check or a
  post acts only on the Pass it was about, if this browser still holds it
  (`passStillHeld`), so a Pass removed or replaced meanwhile, here or in another tab, is
  not written back. A notice a press on the Pass page raises goes through
  `showPassNotice`, which brings it into view when it is out of view, as on a phone,
  where the forms sit far below it: a turn later, so focus goes where the press gives it
  first and stays there, only as far as it must (`block: "nearest"`), not smoothly, and
  clear of the sticky header (`.notice`'s `scroll-margin-top`); what the page says on its
  own, `forgetPassOrder`'s notice and every other page's notices go through `showNotice`,
  which moves nothing. Whatever a Pass answer stores, it draws, focuses or announces only
  while the reader is on the Pass page (`passPageShown`, the one test of the route in the
  Pass section), so an answer that arrives after they left draws nothing over the page
  they went to. When `GET /api/pass` fails, a Pass held here is still drawn, with Show and
  Remove and no Renew, above the code box and the error, and the page is asked for again
  (`refreshFailedPass`) once educhan answers a code typed in or checked there, so the
  error does not stay under the answer. Another tab's Remove or Forget reaches this one by
  the `storage` event (`followPassStorage`), which drops a code just handed over that the
  browser no longer holds and draws the Pass page again. A page brought back from the
  back-forward cache is drawn again through `render` (the `pageshow` listener), so the age
  screen still comes first. A post whose Pass was no good is a `201` like any other,
  carrying `"pass": "invalid"` or `"expired"`: the page forgets the first, marks the
  second run out without a date, and says the post was sent. A code typed in after it ran
  out is kept with the date the server gives. While sales are paused the page says
  `PASS_PAUSED` where Buy and Renew were; `PASS_NOT_ON_SALE` is the server's
  `passNotOnSale` word for word (`TestPassPageWhileSalesArePaused`), and
  `PASS_REISSUE_SPENT` its `passReissueSpent`. A browser that keeps no cookies or storage
  is not sent to Stripe (`passCanBeKept`). The header's Pass link (`data-pass-link`) stays
  hidden until `GET /api/site` says `"pass"` or the browser holds one (`showPassLink`).
  `TestOnlyThePostingRequestCarriesThePass` pins the header and every read of the held
  code; the Pass page's other promises have their own tests beside it.
- The remembered name (`educhan:name`) is the unbadged posts' name. Ticking the badge
  empties the Name field (`badgeToggled`), and a badged post's name is never remembered:
  a tripcode on both kinds of post would publicly tie the unbadged ones to the
  institution, which the per-credential IDs exist to prevent. A Name the poster cleared
  stays cleared across re-renders and after posting (`restoreDraft` restores empty
  values; `submit` sets the field after `form.reset()`).
- "Forget this browser" on `/verify` (`forgetBrowser`), shown in every state — the
  code-entry step included (`TestEveryVerifyStateOffersToForgetTheBrowser`) — removes the
  credential, the Pass, the poster key and the remembered name from storage, drops any
  Pass purchase still being collected, every checkout's mark (`educhan:pass-checkout:`)
  and any note that a Pass was found gone (`passLost`), empties the Pass page's code box
  and a Pass code this tab copied, and leaves the age answer. It is what the FAQ's
  shared-computer advice points to: the key left behind would give the next person at the
  computer the same IDs in the same threads, and a claim left behind would hand them the
  buyer's Pass. The browser's other tabs follow the Pass and what goes with it
  (`followPassStorage`, `followPassForget`: the claim, the code box, a code they copied,
  which stops being theirs to empty, and `passLost`), but not the rest: a tab already
  open can keep the poster key it made (`pagePosterKey`), the badge
  (`state.verification`) and the name in a form it drew, and a post from it puts the key
  and the name back in storage for every page loaded afterwards (`posterKey` writes the
  key it made back to an empty store, as `submit` does the name). The Verify page and the
  FAQ say so, and to close those tabs first
  (`TestForgetThisBrowserSaysWhatOtherTabsKeep`).

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
- **The Pass gates the captcha skip and nothing else.** It is bought, which makes it the
  nearest thing educhan has to an account. Like the badge, it is sold only after an
  unticked 18+ confirmation that stores nothing about age. It carries no name, profile or
  history, and its code travels only in `X-Educhan-Pass` on the posting request, and in
  the body of the requests that check or renew it on the Pass page. A badged post sends
  the credential instead. Nothing about a Pass is written on a post or used in a poster
  ID, and nothing identifying a Pass is logged. Its hourly budget is kept in memory,
  keyed on a hash of the code, and forgotten by the flood-control sweep within a minute
  of its hour ending, on the same terms as an address. A Pass that is no good on a post
  is treated as no Pass, never refused with a status of its own, because a request-log
  line only a Pass can cause would tie its holder to the thread
  (`TestPassNoGoodOnPostsIsNoPass`). The Pass skips the captcha only, never the rules, a
  refusal limit or a cooldown. `TestPassIsNeverStoredOrLogged`,
  `TestPassTablesCannotBeJoinedToPosts` and `TestPassSharesOneHourlyBudget` hold it
  there.
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
  board's `archive_days` have passed, and prunes the Pass tables (`prunePasses`), on sale
  or not (`TestSweepPrunesThePassTables`), with no Pass setting: each order keeps the term
  it was sold with (`pass_orders.days`, `TestPassOrderKeepsItsTerm`), so nothing about the
  Pass has to be given to the Job. It is a one-shot mode rather than a timer because
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
  `EDUCHAN_DB_MAX_CONNS`, and the Pass's: `EDUCHAN_PASS_SECRET`,
  `EDUCHAN_STRIPE_SECRET_KEY`, `EDUCHAN_STRIPE_WEBHOOK_SECRET`, `EDUCHAN_STRIPE_PRICE_ID`,
  `EDUCHAN_STRIPE_AUTOMATIC_TAX`, `EDUCHAN_PUBLIC_URL`, `EDUCHAN_PASS_DAYS` (1 to 36500,
  stored on each order at checkout), `EDUCHAN_PASS_HOURLY_POSTS`, `EDUCHAN_PASS_COOLDOWN`
  and `EDUCHAN_PASS_SALES` (`open` or `paused`). See `.env.example` and the README table.
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
  State is per process and in memory — a shared backend means replacing
  `cooldownLimiter`, which nothing else depends on.
  The keys carry client addresses, so the limiters are also a privacy surface: `New`
  builds a `cooldownSweep` over every limiter (thread, post, upload, verification code,
  and the Pass's checkout and code check) and the Pass's hourly budget, and starts its
  timer, which drops every expired entry each `cooldownSweepInterval` (one minute), and
  `Close` stops it. **While the process has CPU, an address is held in memory no longer
  than the longest cooldown that applies to it plus one minute**, counted from its last
  action that started one, busy server or quiet. A host that gives CPU only during
  requests, which is Cloud Run's default (request-based billing), holds the timer back on
  an idle instance, so `beforeEachRequest` (in `Routes`) runs an overdue sweep before a
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
  `internal/app/pass_api_test.go`, and `TestClientKnowsTheServersImageRefusals` in
  `internal/app/web_invariants_test.go`) run the whole app against an isolated schema in
  `TEST_DATABASE_URL` (falling back to `DATABASE_URL`), so they are safe to run against
  the development database. `testServer` builds its `Config` directly, which leaves every
  cooldown at zero unless a test asks for one, and gives the client a cookie jar so the
  two-step verification exchange behaves as it does in a browser. `verifyServer` adds a
  secret and a capturing `mailer.Sender`, which is how a test reads the code back; the
  credential it returns is then presented explicitly with `postWithCredential`, because
  nothing attaches it on a caller's behalf. `passServer` adds the Pass's settings and
  `fakeStripe`, which stands in for Stripe's API; a test completes a checkout by sending
  the webhook a signed event itself, and the cookie jar carries the claim as a browser's
  would. Poster ID tests post through `browser` (`poster_id_test.go`), which sends what
  the client sends: the credential alone on a post that asks for a badge, and the poster
  key on any other. A post carrying both is never sent by the client, and the tests that
  cover it set the headers explicitly. Tests post to boards they create with `addBoard`
  (`api_test.go`), never to seeded ones, so they do not depend on which boards the
  migrations seed; `TestBoardsAreSeeded` is the one test that names the seeded set.
  A migration's effect on existing data is tested by stopping a schema at an earlier
  version with `databaseAt` (`migrate_test.go`), writing rows into it with plain SQL, and
  starting a server on it with `serverOn`, which applies the rest. Sweep tests call
  `app.sweep` with a small batch to cover the loop, and backdate `archived_at` with
  `archiveThread` rather than waiting. `cmd/server/main_test.go` runs the `-sweep` mode as
  a whole process against its own schema in the same database.
