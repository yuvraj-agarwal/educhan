# educhan

**Anonymous imageboard for tertiary education**

educhan is a fast, high-signal imageboard designed for students, researchers, and academic communities. It combines classic anonymous discussion with optional institutional verification, creating a space for authentic conversations about placements, research, courses, and campus life.

## Core Features

- **Verified Anonymity** — Optionally verify institutional email domains (`.ac.in`, `.edu`, etc.) to receive a trusted badge while remaining fully anonymous
- **Imageboard-native experience** — Fast, ephemeral, image-first discussion
- **Technical media support** — Accepts raw camera files and high-bitrate video with automatic web-optimised proxies
- **Selective permanence** — High-value threads can be automatically archived
- **Google Cloud native** — Built for scalability and low operational overhead

## Current Status

- Domain: [educhan.org](https://educhan.org)
- Stage: Technology Concept (TRL 2)
- Architecture and product design completed
- Prototype under development

The prototype in this repository implements the imageboard end to end: boards,
anonymous posting with tripcodes and per-thread poster IDs, one image per post,
a paginated board index, a catalog view, and threads with quote links.
Verification badges and moderation tooling are not built yet.

## Image attachments

One image per post, on both thread creation and replies.

- **Formats**: JPEG, PNG, WebP, and GIF, identified by magic bytes rather than by filename
  or the content type the client claims.
- **Size limit**: 8 MiB by default (`EDUCHAN_MAX_UPLOAD_BYTES`).
- **Thumbnails**: generated at upload time, 250px on the longest side, aspect ratio
  preserved and never upscaled. JPEG sources keep JPEG thumbnails; the formats that can
  carry transparency get PNG thumbnails.
- **Storage**: originals and thumbnails are written to
  `$EDUCHAN_UPLOAD_DIR/originals/{board}/{YYYY}/{MM}/` and `…/thumbs/{board}/{YYYY}/{MM}/`
  under a generated random filename (default root `./data/uploads`). All writes and reads
  go through the `internal/media.Store` interface, so moving to Google Cloud Storage is a
  new implementation of that interface rather than a rewrite of the handlers.
- **Serving**: `/media/original/{path}` and `/media/thumb/{path}`, with the correct
  `Content-Type`, range support, and long-lived immutable caching.
- **UI**: thumbnails appear in the board index, the catalog, and inside threads. In a
  thread a thumbnail expands to the full image in place — the full file is decoded before
  the swap, so the picture never blinks — and collapses again on a second click or
  <kbd>Esc</kbd>. Elsewhere it opens in a new tab. Attaching an image is a button, a drop
  target on the posting form, or both, with a preview of what will be sent.

Posting an image over HTTP is a plain multipart form:

```bash
curl -X POST localhost:8080/api/boards/general/threads \
  -F subject="lab setup" -F body="finally works" -F image=@photo.jpg
```

The JSON API (`-d '{"body":"…"}'`) still works for text-only posts.

### Implemented

| Area | Behaviour |
| --- | --- |
| Boards | Seeded from a migration, then created and reconfigured over the API with the moderation key; each has its own bump limit, thread limit, and post numbering |
| Posting | Anonymous by default; `name#secret` and `name##secret` produce tripcodes; a per-thread poster ID keeps a conversation followable |
| Threads | Per-board post numbers, `>>123` quote links with backlinks, greentext, sage, bump limits, sticky and locked flags |
| Index | Paginated, newest bump first, showing each thread's opening post and last three replies |
| Catalog | Every live thread on one page as excerpt tiles with thumbnails |
| Attachments | One image per post, validated by magic bytes, thumbnailed at upload, served from `/media/…` |
| Moderation | Sticky, lock, and delete a post or a whole thread, plus creating and reconfiguring boards, behind a shared moderation key |
| Housekeeping | Threads past a board's thread limit are pruned; per-client cooldowns limit new threads, replies, and uploads |

## Front end

The UI is three files served from the Go binary — `index.html`, `app.css`, `app.js` in
`internal/app/web/` — with no build step and no framework. `app.js` renders the JSON API
into the same four routes the API exposes (`/`, `/{slug}/`, `/{slug}/catalog`,
`/{slug}/thread/{no}`), so every view is a shareable link and the back button works.

- **Reading**: a subject heads its thread, with the poster's name, tripcode, per-thread ID,
  time, and post number as secondary metadata; sticky, locked, and bump-limit badges are
  tinted chips. On a fine pointer, hovering `>>123` floats a preview of the quoted post
  when it is already on the page — a preview never costs a request.
- **Posting**: labelled fields, an image picker that previews the chosen file with its name
  and size, and drag-and-drop anywhere on the form. A half-written reply survives
  **Update**, image included, so refreshing a busy thread never costs a draft. Clicking a
  post number quotes it into the reply box.
- **Phones**: a single fluid column with the catalog at two tiles across, thumbnails that
  never overflow the viewport, and 16px form text so iOS does not zoom on focus.
- **Accessibility**: a skip link, one visible focus ring on every control, headings and
  landmarks that match the layout, live-region announcements for client-side navigation,
  colours that clear WCAG AA on their own background, and `prefers-reduced-motion` support.

The page runs under the same-origin CSP in `middleware.go`, which has no `unsafe-inline`
exception: no inline `<script>`, no inline `<style>`, and no `style` attributes. A unit
test (`internal/app/web_test.go`) fails the build if any appear, and the image preview is
a `data:` URL because that is what `img-src` allows.

## Tech Stack

- **Backend**: Go 1.22 standard library HTTP server, `pgx` for PostgreSQL
- **Database**: PostgreSQL 16, versioned SQL migrations applied on startup
- **Front end**: dependency-free HTML/CSS/JS served from the Go binary
- **Planned**: Cloud Run, Cloud SQL, Google Cloud Storage, Gemini API + Cloud Vision for moderation, Cloud CDN + Cloud Armor

## Running locally

Requires Go 1.22+ and a reachable PostgreSQL instance.

```bash
bash .cursor/scripts/start.sh   # start a local PostgreSQL cluster (or use your own)
make run                        # serve on http://localhost:8080
```

Migrations in `internal/app/migrations/` are applied automatically at startup,
guarded by a Postgres advisory lock so concurrent instances are safe.

### Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://educhan:educhan@localhost:5432/educhan?sslmode=disable` | PostgreSQL connection string |
| `EDUCHAN_ADDR` | `:8080` | Listen address |
| `EDUCHAN_POSTER_ID_SALT` | random per process | Keys poster IDs and secure tripcodes; set it so they survive a restart |
| `EDUCHAN_TRUST_PROXY` | `0` | Number of reverse proxies in front of the server; see [Running behind a proxy](#running-behind-a-proxy) |
| `EDUCHAN_THREAD_COOLDOWN` | `30` (seconds) | Minimum interval between new threads from one client, per board |
| `EDUCHAN_POST_COOLDOWN` | `5` (seconds) | Minimum interval between replies from one client, per board |
| `EDUCHAN_UPLOAD_COOLDOWN` | `15` (seconds) | Minimum interval between attachments from one client, across all boards |
| `EDUCHAN_LOG_FORMAT` | `text` | `text` or `json` structured logs |
| `EDUCHAN_UPLOAD_DIR` | `./data/uploads` | Root the media store writes originals and thumbnails to |
| `EDUCHAN_MAX_UPLOAD_BYTES` | `8388608` (8 MiB) | Largest accepted attachment |
| `EDUCHAN_MOD_KEY` | empty | Shared secret for the moderation and board management endpoints; unset means they refuse every request |

### Flood control

Posting is rate limited per client address, in memory, per process. Three
cooldowns apply:

| Action | Cooldown | Counted per |
| --- | --- | --- |
| Creating a thread | `EDUCHAN_THREAD_COOLDOWN` | client and board |
| Replying | `EDUCHAN_POST_COOLDOWN` | client and board |
| Attaching an image | `EDUCHAN_UPLOAD_COOLDOWN` | client, across every board |

Posting is counted per board so one client can follow a conversation on two
boards at once. Uploads are counted across the whole server instead, because
decoding, thumbnailing, and storing an image costs the same wherever the post
lands. A post with an image has to clear both its posting cooldown and the
upload cooldown; when it is refused, neither cooldown is started, so the two
limits never compound.

A refused request answers `429` with a `Retry-After` header in seconds and a
`retry_after` field in the body. Set a cooldown to `0` to switch it off. A
request rejected for any other reason — an empty body, an oversized image — does
not start a cooldown, so a typo never costs a wait.

State is held in the process, so each instance of a horizontally scaled
deployment enforces the cooldowns it sees. Moving to shared storage means
replacing `cooldownLimiter` in `internal/app/ratelimit.go`, which nothing else
depends on.

### Running behind a proxy

Flood control and poster IDs are keyed on the client address, so the server has
to know how to find it. `EDUCHAN_TRUST_PROXY` is the number of reverse proxies
in front of it:

- **`0` (default)** — `X-Forwarded-For` is ignored and the peer address is used.
  Correct whenever the server is reachable directly.
- **`1`** — one load balancer or reverse proxy, the common case.
- **`N`** — a longer chain, for example a CDN in front of a load balancer.

Each proxy appends the address it saw, so with `N` trusted hops the client is
the Nth entry counted from the right of the combined header; everything to its
left was supplied by the client and is ignored. Reading the leftmost entry — the
usual mistake — would let anyone pick their own rate-limit bucket by sending a
header of their own.

Anything that does not match the declared shape falls back to the peer address:
a chain shorter than the configured depth, or an entry that is not an IP
address. **Set this to the real number of hops.** A value higher than the actual
chain makes the server read an entry the client controls, which forfeits flood
control entirely; a value lower than the chain buckets every client behind the
proxy together.

Both proxy styles work: one that appends (`proxy_add_x_forwarded_for`) and one
that overwrites the header with just the peer address are each a single hop.

### HTTP API

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/healthz` | Liveness plus a database ping |
| `GET` | `/api/boards` | All boards |
| `GET` | `/api/boards/{slug}` | One board and its limits |
| `GET` | `/api/boards/{slug}/threads?page=N` | Index page: opening post and last replies per thread |
| `POST` | `/api/boards/{slug}/threads` | Create a thread — `{"subject","name","body"}` |
| `GET` | `/api/boards/{slug}/catalog` | Every live thread as excerpt tiles |
| `GET` | `/api/boards/{slug}/threads/{no}` | One thread with all posts |
| `POST` | `/api/boards/{slug}/threads/{no}/posts` | Reply — `{"name","body","sage"}` |
| `GET` | `/api/threads/{id}` | Thread by primary key |
| `POST` | `/api/threads/{id}/posts` | Reply by thread primary key |
| `GET` | `/media/original/{path}` | Full-size attachment |
| `GET` | `/media/thumb/{path}` | Thumbnail |

JSON request bodies must be `application/json`; unknown fields are rejected. The
two posting endpoints also accept `multipart/form-data` with the same fields
plus an `image` file. Errors are `{"error": "..."}`, and `429` responses add
`retry_after` plus a `Retry-After` header.

```bash
curl -s localhost:8080/api/boards
curl -s -X POST localhost:8080/api/boards/general/threads \
  -H 'Content-Type: application/json' \
  -d '{"subject":"placement season","name":"ana#trip","body":"how did yours go?"}'
```

### Moderation API

Moderation is authenticated with one shared secret, `EDUCHAN_MOD_KEY`, presented
as `Authorization: Bearer <key>` or `X-Mod-Key: <key>`. Every endpoint answers
`403` when the key is missing, wrong, or not configured on the server. Real
janitor accounts, bans, and reports are still to come.

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/api/boards` | Create a board — `{"slug","name","description","bump_limit","thread_limit"}` |
| `PATCH` | `/api/boards/{slug}` | Update a board's settings; every field is optional |
| `POST` | `/api/boards/{slug}/threads/{no}/sticky` | Pin a thread to the top of its board |
| `POST` | `/api/boards/{slug}/threads/{no}/unsticky` | Unpin it |
| `POST` | `/api/boards/{slug}/threads/{no}/lock` | Refuse further replies |
| `POST` | `/api/boards/{slug}/threads/{no}/unlock` | Allow replies again |
| `DELETE` | `/api/boards/{slug}/threads/{no}` | Delete a thread with every post and attachment |
| `DELETE` | `/api/boards/{slug}/threads/{no}/posts/{post}` | Delete one post and its attachment |

Deleting a post removes its stored image and thumbnail from the media store as
well as its rows. Deleting a thread's opening post deletes the thread, which the
response reports as `"thread_deleted": true`. A deleted reply is subtracted from
the thread's reply count, so bump-limit state stays accurate.

```bash
export MOD_KEY=…
curl -s -X POST localhost:8080/api/boards/general/threads/1/sticky \
  -H "Authorization: Bearer $MOD_KEY"
curl -s -X DELETE localhost:8080/api/boards/general/threads/1/posts/4 \
  -H "X-Mod-Key: $MOD_KEY"
```

#### Board management

The three seeded boards are a starting point, not the whole list: a board can be
opened and reconfigured with the same key, without a migration or a release.
Listing boards stays public.

A slug is the board's address in every URL, so it is lowercase letters and
digits in dash-separated groups (`general`, `comp-sci`), at most 24 characters,
and not one of the paths the server already answers on (`api`, `media`,
`healthz`). It is normalised before use, so `/CompSci/` and `/compsci/` cannot
both exist. Everything but the slug is optional: the name falls back to the
slug, and the limits to the schema defaults of 300 and 100. A duplicate slug
answers `409`.

```bash
curl -s -X POST localhost:8080/api/boards \
  -H "X-Mod-Key: $MOD_KEY" -H 'Content-Type: application/json' \
  -d '{"slug":"comp-sci","name":"Computer Science","description":"Algorithms and all that"}'
```

`PATCH` changes `name`, `description`, `bump_limit`, and `thread_limit`; an
omitted field keeps its stored value, and at least one must be given. The slug
is not updatable — it is what every bookmark and stored quote link points at.

```bash
curl -s -X PATCH localhost:8080/api/boards/comp-sci \
  -H "X-Mod-Key: $MOD_KEY" -H 'Content-Type: application/json' \
  -d '{"description":"Systems, theory, and coursework","thread_limit":50}'
```

A new board is a board like any other: it appears in the board navigation, the
index, and the catalog, and its post numbering starts at No. 1. A lowered
`thread_limit` is applied by the next post to that board, through the same
pruning the limit has always driven.

## Development

```bash
make build   # go build ./...
make test    # go test ./... (integration tests need PostgreSQL; they skip without it)
make check   # gofmt check, go vet, go test
```

## Contributing

This project is in early stages. Contribution guidelines will be added once the initial prototype is ready.

## License

MIT
