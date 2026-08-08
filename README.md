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
a paginated board index, a catalog view, threads with quote links, optional
institutional verification, and key-authenticated moderation. Janitor accounts,
bans, and reports are not built yet.

## Verified Anonymity

A poster can prove they hold an address at a real institution and carry a badge
for it — `[Verified: IIT Bombay]` — while posting as anonymously as everybody
else. It is entirely optional, off by default on every individual post, and
switched off for the whole server unless `EDUCHAN_VERIFY_SECRET` is set.

```
  you@iitb.ac.in
       │   checked against the domain allowlist
       ▼
  six-digit code, emailed — stored hashed, expires in 15 minutes
       │   confirmed
       ▼
  signed credential, returned to the client: { label, expiry, random id }
       │   kept in sessionStorage; sent in X-Educhan-Credential
       │   on the posts that tick the box, and on nothing else
       ▼
  [Verified: IIT Bombay]

  All the database ever holds: HMAC(secret, address), the label, and an
  expiry rounded up to the day — in a table with no path to any post.
```

1. Submit an institutional address on `/verify`.
2. The domain is checked against the allowlist, which decides both whether it
   counts as academic and what the badge will say.
3. A six-digit code is emailed. It is stored hashed and expires in 15 minutes.
4. Submitting the code exchanges it for a signed, opaque credential, returned in
   the response body and kept by the client in `sessionStorage`.
5. The posting form grows a **post with [Verified: …]** checkbox. Tick it per
   post; leave it alone and the post is indistinguishable from any other — the
   credential is not sent at all.

### The credential travels only when it is used

The credential is **not a cookie**, and that is deliberate. A cookie is attached
by the browser to every request to the origin: anonymous posts, page loads,
catalog fetches, images. A token that arrives on all of them is a stable
identifier for the whole of a poster's session whether or not they ever badge
anything — it would let the server (or anything reading its logs) group a
poster's entire activity, which is the opposite of what this feature is for.

So the credential is handed to the client once, at the end of the exchange, kept
in `sessionStorage`, and sent in an `X-Educhan-Credential` header **only on the
posts that asked for a badge**. The server reads it from that header and from
nowhere else — there is no cookie fallback. Two tests hold the line from both
directions:

- `TestCredentialIsReadOnlyOnTheBadgePath` parses the package and asserts the
  call graph: the header is read in exactly one function, that function has
  exactly one caller (`postBadge`), and that caller has exactly two (the thread
  and reply handlers). A new handler that reaches for the credential fails here.
- `TestCredentialHasNoEffectOffTheBadgePath` presents a valid credential to
  every other endpoint and asserts the responses are byte-identical to the same
  requests without it — including that an unbadged post carrying a credential
  still gets no badge.

`GET /api/verify` therefore reports only whether the feature is configured. It
cannot answer "am I verified?", because answering would mean the credential
travelling on a request that is not posting a badge. The client already holds
the credential and reads the label and expiry out of it locally; a credential
the server has since revoked is discovered on the next post that uses it.

**The trade-off.** `sessionStorage` is readable by JavaScript running on the
page, where an `HttpOnly` cookie would not have been. That is the cost of the
property above, and it is worth paying here:

- **The page runs no inline script.** The CSP in `middleware.go` is
  same-origin with no `unsafe-inline` exception, so an injected `<script>`, a
  `style` attribute, or an inline handler is refused by the browser before it
  runs. `TestWebAssetsHaveNoInlineStylesOrScripts` fails the build if any
  appear in the shipped assets, so the exception is never quietly added.
- **A stolen credential is worth very little.** It grants exactly one thing:
  the ability to post under that institution's label until it expires. There is
  no account behind it, no posting history reachable through it, no settings, no
  contact details, and no way to get from it to the address that earned it —
  it contains a label, a timestamp, and a random number. Compare it to a session
  cookie, which is what the alternative would effectively have been.
- **It is bounded by the tab.** `sessionStorage` is per-tab and cleared when the
  tab closes, so a badge on a shared or borrowed computer does not outlive the
  sitting. That also means the credential TTL below is an upper bound rather
  than the lifetime in practice: close the tab and you verify again. The server
  shows a credential once and cannot reissue it, so there is nothing to recover.

Sending the badge in a header rather than a cookie also removes the CSRF
question from posting entirely: a cross-site form submission carries no header a
browser would attach for it.

### The privacy property, exactly

The design goal is that **no one — the operator included — can link a badge to
an email address or to a poster's other posts.** Concretely, this is what an
operator with full database access, root on the machine, and the server secret
can and cannot work out.

**What they can see**

- That an address at some institution is verified, as
  `HMAC(EDUCHAN_VERIFY_SECRET, normalised address)` and a badge label.
- Roughly when a credential was issued — to the *day*, because the stored
  expiry is rounded up to a UTC day boundary and nothing else is timestamped.
- Which posts carry which badge, because a badge is displayed on the post.
- **Confirmation of a guess.** The email hash is keyed, not encrypted: holding
  the secret, an operator who suspects a particular address can recompute the
  HMAC and check whether that person is verified. This is inherent to a
  uniqueness key and is the sharpest edge of the design. It answers "is
  ana@iitb.ac.in verified?" — it does not answer "who is verified?" or "which
  posts are theirs?".

**What they cannot do**

- Recover an address from the database. It is never written, in any table, at
  any point in the flow — not even transiently, and not in the logs. A test
  (`TestNoEmailAddressReachesTheDatabase`) verifies the whole flow and then
  searches every column of every table for it, with positive controls so it
  cannot pass by failing to look.
- Link a credential to a post. The credential carries a label, an expiry, and a
  random identifier — nothing derived from the address. Posts store the badge
  as text and hold no reference to the record it came from.
- Link a post to a verification record by timing. Verification tables carry no
  `created_at`, and using a credential updates nothing, so there is no "last
  used" to line up against a post.
- Group a poster's requests by their badge. The credential is sent only on the
  posts that carry one, so an operator watching the request layer sees it on
  exactly the posts that already display it, and on nothing else.
- Join the two. There is no foreign key and no shared column between the
  verification tables and `posts` or `threads`; `TestVerificationTablesCannotBeJoinedToPosts`
  asserts the exact column list of both tables so a future migration cannot
  quietly add one.
- Link two badged posts to each other. Two posts showing the same institution
  say only that: an institution with one verified user and one badged post is
  as anonymous as its user base is large. **A badge narrows you to your
  institution — that is the whole of what it discloses, and it is not nothing
  at a small one.**

Rotating `EDUCHAN_VERIFY_SECRET` invalidates every credential and orphans every
uniqueness key, so everyone re-verifies and their old rows match nothing.

### Domain allowlist

The allowlist is a **file**, not a table: the mapping from a domain to a badge
is configuration rather than data about anybody, so it stays out of the
database and gets reviewed in a diff. The default is embedded in the binary at
[`internal/app/verify_domains.txt`](internal/app/verify_domains.txt).

```
ac.in                     # a suffix: ana@cse.nitk.ac.in earns [nitk.ac.in]
iitb.ac.in = IIT Bombay   # an institution: ana@cse.iitb.ac.in earns [IIT Bombay]
```

A suffix entry accepts anything under it and badges the domain one level below
the suffix, so two departments of one university read the same. An institution
entry names the place instead, and wins over any suffix. An address sitting
directly on a suffix (`someone@ac.in`) is refused: a namespace is not an
institution. `EDUCHAN_VERIFY_DOMAINS` points at a file that **replaces** the
built-in list rather than extending it, so a deployment sees exactly what it
configured.

### Email delivery

Delivery goes through `internal/mailer.Sender`, the same way blob storage goes
through `internal/media.Store`. With no provider configured the development
sender prints the code to stdout, so the feature is usable and testable out of
the box:

```
[educhan] verification code for ana@iitb.ac.in: 481920 (valid for 15m0s)
```

That sender prints the address, which makes it a development tool only — it is
the one place in educhan an address is rendered at all. Adding SMTP or Google
Workspace means writing another `Sender` and assigning it to `Config.Sender`;
no handler and no SQL changes.

### Anti-abuse

| Limit | Effect |
| --- | --- |
| One credential per address | Re-verifying issues a new credential and revokes the previous one, so a mailbox is one identity however many browsers it uses |
| Plus-addressing stripped | `ana+one@` and `ana+two@` are the same mailbox. Dots are *not* stripped: that is a Gmail convention, and applying it everywhere would fold two real people at one institution together |
| Code request cooldown | `EDUCHAN_VERIFY_COOLDOWN`, applied per client address **and** per address hash, so neither a client can walk a list of addresses nor a mailbox be flooded from a pool of hosts |
| Five wrong codes | The pending verification is destroyed and a fresh code is the only way on, which is what makes a six-digit code safe |
| Credential expiry | `EDUCHAN_VERIFY_CREDENTIAL_TTL`, 180 days by default, and in practice the tab, whichever ends first |

The one cookie this feature sets is the in-flight verification token, which is
scoped to `/api/verify` and lives for one code's lifetime: it goes to the
verification endpoints and nowhere else. It is `HttpOnly`, `SameSite=Lax`, and
`Secure` whenever the request arrived over TLS. The credential is not a cookie
at all — see above.

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
| Verification | Optional institutional badge from an emailed code; the address is never stored and the badge cannot be linked to a post |
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
- **Deployment**: multi-stage Docker build onto a distroless base; Cloud Run with Cloud SQL (see [Deployment](#deployment))
- **Planned**: Google Cloud Storage, Gemini API + Cloud Vision for moderation, Cloud CDN + Cloud Armor

## Running locally

Requires Go 1.22+ and a reachable PostgreSQL instance.

```bash
bash .cursor/scripts/start.sh   # start a local PostgreSQL cluster (or use your own)
make run                        # serve on http://localhost:8080
```

Migrations in `internal/app/migrations/` are applied automatically at startup,
guarded by a Postgres advisory lock so concurrent instances are safe.

### With Docker

Docker with the Compose plugin is the only prerequisite — no Go, no PostgreSQL:

```bash
docker compose up --build       # http://localhost:8080
```

That builds the image, starts PostgreSQL 16, waits for it to become healthy, and
runs educhan against it. Named volumes hold the database and the uploads, so both
survive `docker compose down` and a rebuild; `down -v` discards them. See
[`deploy/README.md`](deploy/README.md#1-locally-with-docker-compose) for the
settings the compose file exposes.

### Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://educhan:educhan@localhost:5432/educhan?sslmode=disable` | PostgreSQL connection string; a `host=/path` makes it a Unix socket |
| `EDUCHAN_ADDR` | `:8080` | Listen address; overrides `PORT` |
| `PORT` | unset | Bare port number, honoured when `EDUCHAN_ADDR` is unset — the contract Cloud Run and similar runtimes inject |
| `EDUCHAN_POSTER_ID_SALT` | random per process | Keys poster IDs and secure tripcodes; set it so they survive a restart |
| `EDUCHAN_TRUST_PROXY` | `0` | Number of reverse proxies in front of the server; see [Running behind a proxy](#running-behind-a-proxy) |
| `EDUCHAN_THREAD_COOLDOWN` | `30` (seconds) | Minimum interval between new threads from one client, per board |
| `EDUCHAN_POST_COOLDOWN` | `5` (seconds) | Minimum interval between replies from one client, per board |
| `EDUCHAN_UPLOAD_COOLDOWN` | `15` (seconds) | Minimum interval between attachments from one client, across all boards |
| `EDUCHAN_LOG_FORMAT` | `text` | `text` or `json` structured logs |
| `EDUCHAN_UPLOAD_DIR` | `./data/uploads` | Root the media store writes originals and thumbnails to |
| `EDUCHAN_MAX_UPLOAD_BYTES` | `8388608` (8 MiB) | Largest accepted attachment |
| `EDUCHAN_MOD_KEY` | empty | Shared secret for the moderation and board management endpoints; unset means they refuse every request |
| `EDUCHAN_VERIFY_SECRET` | empty | Keys institutional verification: the address hash, the pending code hash, and the credential signature. **Unset switches the whole feature off**; rotating it revokes every credential |
| `EDUCHAN_VERIFY_CODE_TTL` | `15m` | How long an emailed code stays usable |
| `EDUCHAN_VERIFY_CREDENTIAL_TTL` | `4320h` (180 days) | How long a badge lasts before the holder verifies again |
| `EDUCHAN_VERIFY_COOLDOWN` | `60` (seconds) | Minimum interval between code requests, per client and per address |
| `EDUCHAN_VERIFY_DOMAINS` | embedded list | Path to an allowlist file replacing the built-in one |

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
| `POST` | `/api/boards/{slug}/threads` | Create a thread — `{"subject","name","body","badge"}` |
| `GET` | `/api/boards/{slug}/catalog` | Every live thread as excerpt tiles |
| `GET` | `/api/boards/{slug}/threads/{no}` | One thread with all posts |
| `POST` | `/api/boards/{slug}/threads/{no}/posts` | Reply — `{"name","body","sage","badge"}` |
| `GET` | `/api/threads/{id}` | Thread by primary key |
| `POST` | `/api/threads/{id}/posts` | Reply by thread primary key |
| `GET` | `/api/verify` | Whether verification is configured — `{"enabled"}`. It does not read your credential |
| `POST` | `/api/verify/request` | Email a code — `{"email"}` |
| `POST` | `/api/verify/confirm` | Exchange a code for a credential — `{"code"}`; returns `{"credential","label","expires_at"}` |
| `GET` | `/media/original/{path}` | Full-size attachment |
| `GET` | `/media/thumb/{path}` | Thumbnail |

`"badge": true` posts with the institution named by the credential in the
request's `X-Educhan-Credential` header, which is the only header the server
reads it from. Asking for a badge without a usable credential is refused with
`403` rather than quietly posted bare, and refusing it costs no posting
cooldown; the body carries `"credential": "missing" | "expired" | "revoked"`
so a client can discard one the server will not honour again. There is no
endpoint to forget a badge, because the client holds it — dropping it is a
local act.

`/api/verify/confirm` sets no credential cookie, but the two-step exchange does
use a short-lived one scoped to `/api/verify`, so an HTTP client still needs a
cookie jar (`curl -c/-b`) to get through it.

JSON request bodies must be `application/json`; unknown fields are rejected. The
two posting endpoints also accept `multipart/form-data` with the same fields
plus an `image` file. Errors are `{"error": "..."}`, and `429` responses add
`retry_after` plus a `Retry-After` header.

```bash
curl -s localhost:8080/api/boards
curl -s -X POST localhost:8080/api/boards/general/threads \
  -H 'Content-Type: application/json' \
  -d '{"subject":"placement season","name":"ana#trip","body":"how did yours go?"}'

# Verification, end to end. With no mail provider configured the code is
# printed to the server's stdout. Confirming returns the credential; hold on
# to it, because the server will not show it again.
curl -s -c jar -X POST localhost:8080/api/verify/request \
  -H 'Content-Type: application/json' -d '{"email":"you@iitb.ac.in"}'
CRED=$(curl -s -b jar -c jar -X POST localhost:8080/api/verify/confirm \
  -H 'Content-Type: application/json' -d '{"code":"481920"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["credential"])')

# The badge is the header plus the flag. Leave both off and the post is
# anonymous — the credential is not sent at all.
curl -s -X POST localhost:8080/api/boards/general/threads \
  -H "X-Educhan-Credential: $CRED" -H 'Content-Type: application/json' \
  -d '{"subject":"from the inside","body":"we saw the same numbers","badge":true}'
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

## Deployment

The full walkthrough is in [`deploy/README.md`](deploy/README.md); this is the
shape of it.

educhan builds to one static binary with the web UI and the migrations embedded,
so a deployment is that container plus a PostgreSQL. The `Dockerfile` is a
multi-stage build onto a distroless base — no shell, no package manager, running
as a non-root user, about 21 MB — and `deploy/cloudrun/service.yaml` is the
Cloud Run service that runs it.

```bash
# Build and push
gcloud builds submit --tag "$REGION-docker.pkg.dev/$PROJECT_ID/educhan/educhan:$(git rev-parse --short HEAD)"

# Deploy, with Cloud SQL attached and secrets from Secret Manager
gcloud run deploy educhan --image "$IMAGE" --region "$REGION" \
  --service-account "$RUN_SA" \
  --add-cloudsql-instances "$CLOUDSQL_INSTANCE" \
  --set-secrets "DATABASE_URL=educhan-database-url:latest,EDUCHAN_POSTER_ID_SALT=educhan-poster-id-salt:latest,EDUCHAN_MOD_KEY=educhan-mod-key:latest" \
  --set-env-vars "EDUCHAN_TRUST_PROXY=1,EDUCHAN_UPLOAD_DIR=/tmp/uploads,EDUCHAN_LOG_FORMAT=json" \
  --allow-unauthenticated
```

Cloud Run mounts the Cloud SQL instance's Unix socket at
`/cloudsql/{connection name}`, and `pgx` connects to a socket whenever the
connection string's `host` is a path, so no proxy process and no code change is
needed:

```
postgres://educhan:PASSWORD@/educhan?host=/cloudsql/PROJECT:REGION:INSTANCE&sslmode=disable
```

Anywhere without that mount — a VM, a laptop — the [Cloud SQL Auth
Proxy](https://cloud.google.com/sql/docs/postgres/sql-proxy) opens a local port
and `DATABASE_URL` becomes an ordinary TCP connection string.

Three things need setting for a real deployment, and the deploy guide covers
each: `EDUCHAN_POSTER_ID_SALT` has to be a stable secret or poster IDs differ
between instances, `EDUCHAN_TRUST_PROXY` is `1` behind Cloud Run's front end, and
`EDUCHAN_UPLOAD_DIR` must point at `/tmp` because that is the writable path
there.

### What is not stateless yet

Everything the API serves lives in PostgreSQL, so instances start in any order
and any number. Two things do not:

- **Uploads.** Cloud Run's `/tmp` is per-instance and in-memory, so an image
  uploaded to one instance answers `404` from another and is lost when the
  container stops. `internal/media.Store` exists precisely so a Cloud Storage
  backend replaces this without touching handlers or SQL; until then, pin
  `--max-instances 1` or mount a bucket.
- **Flood control.** `cooldownLimiter` keeps its state in process memory, so N
  instances enforce the cooldowns N times over.

## Development

```bash
make build   # go build ./...
make test    # go test ./... (integration tests need PostgreSQL; they skip without it)
make check   # gofmt check, go vet, go test

make docker-build  # build the container image as educhan:local
make compose-up    # the whole stack on :8080
make compose-down  # stop it, keeping the volumes
```

## Contributing

This project is in early stages. Contribution guidelines will be added once the initial prototype is ready.

## License

MIT
