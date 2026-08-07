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
- **UI**: thumbnails appear in the board index, the catalog, and inside threads; clicking
  one opens the full image.

Posting an image over HTTP is a plain multipart form:

```bash
curl -X POST localhost:8080/api/boards/general/threads \
  -F subject="lab setup" -F body="finally works" -F image=@photo.jpg
```

The JSON API (`-d '{"body":"…"}'`) still works for text-only posts.

### Implemented

| Area | Behaviour |
| --- | --- |
| Boards | Seeded from a migration, each with its own bump limit, thread limit, and post numbering |
| Posting | Anonymous by default; `name#secret` and `name##secret` produce tripcodes; a per-thread poster ID keeps a conversation followable |
| Threads | Per-board post numbers, `>>123` quote links with backlinks, greentext, sage, bump limits, sticky and locked flags |
| Index | Paginated, newest bump first, showing each thread's opening post and last three replies |
| Catalog | Every live thread on one page as excerpt tiles with thumbnails |
| Attachments | One image per post, validated by magic bytes, thumbnailed at upload, served from `/media/…` |
| Housekeeping | Threads past a board's thread limit are pruned; flood control limits how often one client can post |

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
| `EDUCHAN_TRUST_PROXY` | `false` | Read the client address from `X-Forwarded-For`; only enable behind a proxy that overwrites it |
| `EDUCHAN_THREAD_COOLDOWN` | `15` (seconds) | Minimum interval between new threads from one client |
| `EDUCHAN_POST_COOLDOWN` | `3` (seconds) | Minimum interval between replies from one client |
| `EDUCHAN_LOG_FORMAT` | `text` | `text` or `json` structured logs |
| `EDUCHAN_UPLOAD_DIR` | `./data/uploads` | Root the media store writes originals and thumbnails to |
| `EDUCHAN_MAX_UPLOAD_BYTES` | `8388608` (8 MiB) | Largest accepted attachment |

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
