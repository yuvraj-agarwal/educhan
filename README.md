# educhan

**Anonymous imageboard for students**

educhan is a fast, high-signal imageboard designed for students, researchers, and academic communities. It combines classic anonymous discussion with optional institutional verification, creating a space for authentic conversations about courses, exams, admissions, careers, and student life.

## Core Features

- **Verified Anonymity** — Students aged 18 and over can optionally verify institutional email domains (`.ac.in`, `.edu`, etc.) to receive a badge naming the institution, never the person
- **Imageboard-native experience** — Fast, ephemeral, image-first discussion
- **One image per post** — JPEG, PNG, WebP or GIF
- **Archive** — Threads that fall off a board stay readable in its archive for a while, then are deleted
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
  you@iitb.ac.in   + "I confirm I am 18 or over"
       │   refused unless ticked; then checked against the domain allowlist
       ▼
  six-digit code, emailed — stored hashed, expires in 15 minutes
       │   confirmed
       ▼
  signed credential, returned to the client: { label, expiry, random id }
       │   kept in localStorage; sent in X-Educhan-Credential
       │   on the posts that tick the box, and on nothing else
       ▼
  [Verified: IIT Bombay]

  The columns hold only HMAC(secret, address), the label, and an expiry
  rounded up to the day — in a table with no path to any post. PostgreSQL's
  transaction ids date the issuance more closely: the row's own until the
  nightly sweep rewrites it, and the gaps the request and the confirmation
  leave among the posts' for as long as those posts exist (see
  "Transaction ids, and the nightly rewrite").
```

1. Submit an institutional address on `/verify`, ticking the box that confirms
   you are 18 or over. Without it the page sends nothing, and the server
   refuses any request that does not carry `"adult": true`.
2. The domain is checked against the allowlist, which decides both whether it
   counts as academic and what the badge will say.
3. A six-digit code is emailed. It is stored hashed and expires in 15 minutes.
4. Submitting the code exchanges it for a signed, opaque credential, returned in
   the response body and kept by the client in `localStorage`.
5. The posting form grows a **post with [Verified: …]** checkbox. Tick it per
   post; leave it alone and the post is indistinguishable from any other — the
   credential is not sent at all.

### The badge is 18+, by attestation

The badge is for students aged 18 and over, on their own word. Everyone the
site is open to, 13 and over, can read and post anonymously without one; only
the badge has an age limit. `POST /api/verify/request` refuses a body without
`"adult": true` with `400` straight after decoding it: the address is not
parsed, no cooldown starts, nothing is stored and no mail is sent
(`TestVerificationRequiresTheAdultAttestation`).

The answer is checked and then dropped. It is not a column, a log field, a
claim in the credential or anything the browser keeps: the answer itself is
recorded nowhere (`TestAdultAttestationIsNeverKept`, and
`TestBadgeAttestationIsNeverPreselectedOrKept` for the page's unticked box).
Why adult-only: the badge is the one feature that keeps something tied to a
person, a keyed hash of an address for up to 180 days. Keeping it to adults
keeps that record out of the rules for under-18s' data, such as India's
parental-consent requirement from 13 May 2027 (build plan 2.14,
design-recommendations §3.3 and §4.4). It is a tick-box and nothing more: a
17-year-old who ticks it gets a badge, which is one more reason the badge says
so little — an institution, an expiry, and a random number.

### The credential travels only when it is used

The credential is **not a cookie**, and that is deliberate. A cookie is attached
by the browser to every request to the origin: anonymous posts, page loads,
catalog fetches, images. A token that arrives on all of them is a stable
identifier for the whole of a poster's session whether or not they ever badge
anything — it would let the server (or anything reading its logs) group a
poster's entire activity, which is the opposite of what this feature is for.

So the credential is handed to the client once, at the end of the exchange, kept
in `localStorage`, and sent in an `X-Educhan-Credential` header **only on the
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

**The trade-off.** `localStorage` is readable by JavaScript running on the
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

Sending the badge in a header rather than a cookie also removes the CSRF
question from posting entirely: a cross-site form submission carries no header a
browser would attach for it.

**Why `localStorage` and not `sessionStorage`.** `sessionStorage` is per-tab and
cleared when the tab closes, which sounds like the safer default and reads well
until you follow what it costs. The server issues a credential exactly once and
cannot reissue it, so a closed tab is not a re-login — it is a fresh email round
trip through `/verify`. That has three consequences:

- `EDUCHAN_VERIFY_CREDENTIAL_TTL` stops meaning anything. A 180-day expiry that
  is in practice bounded by a browsing session is not a 180-day expiry.
- A poster re-verifying after a tab close spends their own cooldown. The code
  request cooldown is applied per address hash, so an ordinary user closing tabs
  can lock themselves out of the feature with no abuse involved.
- Behind a shared campus NAT, where the per-client-address cooldown is shared by
  everyone on the network, those forced re-verifications are charged to the
  whole institution.

So the credential persists in `localStorage`. The cost is real and is the one
worth stating plainly: **on a shared or borrowed browser the badge stays behind
after the sitting**, available to whoever uses the browser next, until it
expires or someone clears it. That is why `/verify` gives **Forget this
credential** its own block on the page rather than a quiet button in a row of
actions, and says what forgetting costs — the server cannot show the credential
again, so it means verifying from the start. Beside it, in every state of the
page, **Forget this browser** removes the badge together with the poster key and
the remembered name (see below), which are what would otherwise let the next
person at a shared computer post under the same IDs and tripcode.

A badge is a weak thing to inherit: it grants posting under an institution's
label and nothing else, with no account, no history, and no route back to the
address that earned it — and the remembered posting name already sat in
`localStorage` beside it. But it is not nothing, and the honest framing is a
trade: a persistent credential that makes the TTL real, against a browser that
now holds something worth forgetting on the way out.

### The privacy property, exactly

The design goal is that **no one — the operator included — can link a badge to
an email address or to a poster's other posts.** Concretely, this is what an
operator with full database access, root on the machine, and the server secret
can and cannot work out.

**What they can see**

- That an address at some institution is verified, as
  `HMAC(EDUCHAN_VERIFY_SECRET, normalised address)` and a badge label.
- When a credential was issued: to the interval between two posts, for as long
  as those posts exist, whatever the nightly sweep does. The verification tables
  themselves, once the sweep has run, say only the *day*: the stored expiry is
  rounded up to a UTC day boundary, the sweep deletes the codes that expired
  unconfirmed, whose expiries are the only other times in the tables, PostgreSQL
  keeps no sample of their values, and the sweep rewrites the credentials so
  that the transaction id PostgreSQL keeps on every row is the sweep's rather
  than the issuance's. A code still in flight as the sweep ran is the exception,
  which [Transaction ids, and the nightly
  rewrite](#transaction-ids-and-the-nightly-rewrite) spells out. But requesting
  a code and confirming one are writes, and each takes a transaction id from the
  counter posts take theirs from. Posts keep theirs beside their `created_at`,
  and nothing rewrites them, so the ids a request and a confirmation took stay
  missing from the posts' sequence, between the two posts written around each.
  One plain query on `posts`, which needs no superuser and no secret, finds
  those gaps, in the live database and in every backup
  (`TestIssuanceLeavesGapsAmongThePostsTransactionIDs`). Before the sweep, and
  in a backup, the credential's own transaction id places the issuance the same
  way, and the write-ahead log the server keeps for crash recovery dates it
  exactly until PostgreSQL deletes or reuses the last log segment holding its
  row, which can be the one the sweep's own rewrite wrote to; [Transaction ids,
  and the nightly rewrite](#transaction-ids-and-the-nightly-rewrite) says
  exactly when. In the application's log, for as long as it is kept, to the
  second, because the request line for `POST /api/verify/confirm` and a bare
  `verification issued` line each carry a time. Neither the gaps nor those lines
  say which institution or address the credential was for
  (`TestIssuanceIsLoggedWithoutTheInstitution`), but a badged post made soon
  after an issuance does name the institution, and its time is kept to the
  second too. So an issuance followed by a badged post ties that post to its
  stored record — the one row with that label and that expiry day — on any day
  its institution issued only one credential, however many other institutions
  issued that day. The posts' gaps keep that possible for as long as the posts
  are kept; the request log adds the second for as long as it is kept.
- Which posts carry which badge, because a badge is displayed on the post.
- Which badged posts in one thread came from the same credential, because they
  share a poster ID there, as any poster's posts in a thread do. In different
  threads the same credential's IDs differ.
- **Confirmation of a guess.** The email hash is keyed, not encrypted: holding
  the secret, an operator who suspects a particular address can recompute the
  HMAC and check whether that person is verified. This is inherent to a
  uniqueness key and is the sharpest edge of the design. It answers "is
  ana@iitb.ac.in verified?" — it does not answer "who is verified?", nor
  "which posts are theirs?" except for a post tied to its record by the
  issuance times above.

**What they cannot do**

- Recover an address from the database. It is never written, in any table, at
  any point in the flow — not even transiently, and not in the logs. A test
  (`TestNoEmailAddressReachesTheDatabase`) verifies the whole flow and then
  searches every column of every table for it, with positive controls so it
  cannot pass by failing to look.
- Link a credential to a post. The credential carries a label, an expiry, and a
  random identifier — nothing derived from the address. Posts store the badge
  as text and hold no reference to the record it came from.
- Link a post to a verification record by timing, beyond the issuance times
  above. Verification tables carry no `created_at`, and using a credential
  updates nothing, so there is no "last used" to line up against a post; the
  transaction id on a rewritten credential row dates the sweep that rewrote it,
  not the issuance. But the issuance times above are enough on some days:
  neither the gaps among the posts' transaction ids nor the log's lines name an
  institution, but a badged post soon after an issuance does, and that ties the
  post to a single record on a day its institution issued only one credential.
- Group a poster's requests by their badge. The credential is sent only on the
  posts that carry one, so an operator watching the request layer sees it on
  exactly the posts that already display it, and on nothing else.
- Join the two. There is no foreign key and no shared column between the
  verification tables and `posts` or `threads`; `TestVerificationTablesCannotBeJoinedToPosts`
  asserts the exact column list of both tables so a future migration cannot
  quietly add one.
- Recompute a badged post's poster ID. It is derived from the random id inside
  the credential as the poster presents it; the database holds only that id's
  SHA-256, and no derivation from anything in the verification tables
  reproduces it (`TestBadgedPosterIDCannotBeRecomputedFromTheDatabase`).
- Link two badged posts in different threads to each other. Their poster IDs
  differ, and two posts showing the same institution say only that: an
  institution with one verified user and one badged post is as anonymous as its
  user base is large. **A badge narrows you to your institution — that is the
  whole of what it discloses, and it is not nothing at a small one.**

Rotating `EDUCHAN_VERIFY_SECRET` invalidates every credential and orphans every
uniqueness key, so everyone re-verifies and their old rows match nothing.

### Transaction ids, and the nightly rewrite

PostgreSQL keeps something on every row besides its columns: `xmin`, the id of
the transaction that wrote it. Transaction ids are handed out in the order
transactions first write, and posts are written by transactions too, with their
times on the page, so a credential row's `xmin` falls between two posts' and
dates its issuance to the minutes between them — what leaving `created_at` out
of the table was for — and, with its label, points at the first badged posts
after it. Freezing a row does not clear its `xmin`. The order rows sit in the
table's file, which is the order a dump lists them in, is the order they were
written. And a row that is deleted or replaced stays in the table's file, `xmin`
and all, until `VACUUM FULL` writes the table out anew: a plain vacuum, which
is what autovacuum runs, marks the space free for reuse but does not erase
what was in it. That covers every pending row a confirmation deleted, with its
email hash and an expiry to the microsecond, and every credential a second
verification replaced.

The same ids date a verification from the posts' side, and that no rewrite of
the verification tables can undo. A code request and a confirmation are writes,
so each takes the next id, and the posts written either side keep theirs: the
ids the verification used are missing from the posts' sequence for as long as
those posts exist. What the sweep below takes away is the credential row's own
id, the direct link from a row to a time; what the posts' gaps still say is the
second point under "What that protects, exactly".

PostgreSQL keeps statistics about each table too, and two kinds would say more
than the rows. `ANALYZE`, which autovacuum runs by itself after enough writes,
copies a sample of each column into `pg_statistic`, where `pg_stats` shows it:
for the pending table, the email hashes of codes in flight beside their
expiries, kept after the rows are gone. And the statistics views keep the time
of the last scan of each table and index, and of autovacuum's last run on each
table, which follow the requests that read and wrote it.

So migration `0015` stops PostgreSQL sampling either table, drops the samples
it already had, and turns autovacuum off on both
(`TestVerificationTablesKeepNoStatistics`), and the archive sweeper ends every
run on the verification tables (`RewriteCredentials` in `store.go`):

1. It deletes the codes that expired unconfirmed. A request for a code deletes
   them too, but requests are few at the hour the sweep runs, and until one
   comes an abandoned code's row keeps the time it was asked for, to the
   microsecond (its expiry less the code's lifetime), under the address's email
   hash. When the address already holds a credential — a second browser asking
   for a code it never confirms, say — that is the credential's email hash too,
   and the row says the credential was issued before then, on the day its
   expiry gives, which points at the confirmations just before then among the
   gaps in the posts' transaction ids (below). It does this in a transaction of
   its own, before the rewrite, so that the vacuum in step 4 takes the rows out
   of the table's file as well (`TestSweepDeletesCodesThatExpiredUnconfirmed`).
2. In one transaction, holding a lock that makes writers wait and lets readers
   through, it deletes every credential row and inserts the copy back in random
   order. Every row then carries the sweep's transaction id, and where a row
   sits says nothing about when it was issued. Badged posts read the table
   throughout. A confirmation that already has the table is waited for and
   rewritten with the rest; one that arrives during the rewrite waits for it
   and is then written as usual — once either way
   (`TestCredentialConfirmedAsTheRewriteStartsIsRewrittenOnce`,
   `TestCredentialConfirmedDuringTheRewriteIsKeptOnce`).
3. It reads each table once whole and once through each of its indexes, so
   that every last-scan time the statistics show for them is the sweep's
   (`TestSweepLeavesNoScanTimeFromBeforeIt`).
4. Then `VACUUM FULL` writes `verification_credentials` and
   `verification_pending` to new files without their dead rows, and drops the
   old files; with autovacuum off, it is the vacuum both tables get. It does not
   wait for a table another session holds, so requests do not queue behind a
   vacuum waiting to start: it passes over the table and tries again later
   (`TestBadgedPostsDoNotQueueBehindTheSweepsVacuum`). But once it has a table
   it holds it against every read and write until it has written the table out,
   so badged posts and confirmations wait for the whole run — between about half
   a second and two and a half seconds for 220,000 credentials, measured on
   development machines — each holding one of its server instance's few database
   connections, and once they hold them all (three by default) every request on
   that instance waits too. And it copies, rather than drops, any old row a
   snapshot from before the rewrite could still read: any statement or
   transaction in the database that started before the rewrite committed and is
   still running — the server's own requests included — or any transaction
   anywhere on the instance that had written something by then. A run then would
   hold the site up and leave the old rows in the new file. So before each
   attempt the sweep runs `VACUUM FULL` on `vacuum_probe`, an empty table of its
   own (migration `0016`), which works out the same cutoff in milliseconds and
   holds up nobody, and it takes the verification tables only once the probe's
   `relfrozenxid` is newer than the rewrite — once the older transactions have
   ended — which makes one run on each table a night
   (`TestSweepWaitsForAnOlderTransactionToEnd`,
   `TestBadgedPostsDoNotQueueBehindVacuumsAnOlderSnapshotWouldSpoil`). It checks
   each table's new file the same way afterwards, and tries again if it still
   holds those rows.

The sweep keeps at this for up to a minute. A session that holds a table, or an
old snapshot, for longer than that — an export, a `psql` left in a transaction
— fails the sweep, which exits non-zero
(`TestSweepFailsWhileAnOlderTransactionCanReadTheOldRows`,
`TestSweepFailsWhenTheVerificationTablesAreHeld`). It does not report success
with the old rows still in the tables' files.

`TestSweepTakesIssuanceTimeOutOfCredentials` issues credentials between posts
and checks that afterwards they all carry one transaction id, that none lies
between two posts written before the sweep, and that both tables are in new
files; `TestSweepStoresCredentialsOutOfIssuanceOrder` checks the stored order.
Those are the credential rows' own ids;
`TestIssuanceLeavesGapsAmongThePostsTransactionIDs` checks that the gaps among
the posts' are still there after the sweep.

What that protects, exactly:

- **The verification tables of the live database, after the nightly sweep at
  03:17 UTC.** Nothing in either table dates a credential more closely than its
  expiry, rounded to the day: no live row and no row version in either table's
  file, no sample in `pg_statistic`, and no last-scan time says more — but for
  a code still in flight as the sweep ran (below). Times autovacuum recorded
  before migration `0015` stay in the statistics views until the statistics are
  reset, which takes a superuser. That is the tables, not the database: see the
  next point.
- **Not the posts' own transaction ids**, which no sweep touches. Every code
  request that reaches the database, and every attempt at a code in flight, is a
  write and takes the next id, so each leaves a gap in the posts' `xmin` between
  the two posts written around it. A plain query on `posts`, which the
  application's own role can run, finds those gaps in the live database and in
  every backup, for as long as those posts exist, and places each issuance
  between two posts as closely as its credential's own `xmin` did before the
  rewrite (`TestIssuanceLeavesGapsAmongThePostsTransactionIDs`). With the label
  of the first badged post after it and the expiry day, that still points at
  the credential's row on a day its institution issued only one credential; on
  a day with a single issuance, the expiry day alone does. Other writes leave
  gaps too — a moderator's, a refused reply's, the sweep's, autovacuum's on
  other tables, another database's on the same instance — so a gap alone proves
  nothing; but a request's gap, a confirmation's within the code's lifetime
  (fifteen minutes by default), then a badged post, is the shape of an
  issuance, and on a quiet instance the gaps are exact. What the rewrite takes
  away is the direct link from a credential row to a time, not the time.
  Transaction ids belong to the whole PostgreSQL instance, so only verification
  tables on an instance of their own would close this.
- **A credential issued since the last sweep** carries its own transaction id
  and can be placed between two timed posts until the next sweep rewrites it.
  Until then, too, the pending row its confirmation deleted can still be in the
  table's file, where anyone who can read the file would find the time its code
  was requested, and the last-scan times can date the latest confirmation.
- **A code in flight as the sweep ran**, which the sweep cannot delete without
  making it fail. Its row keeps the time it was asked for, to the microsecond,
  and the id of the transaction that last wrote it (the request, or a wrong
  guess at the code), until it is confirmed or, once it has expired, until the
  next request or the next sweep deletes it — and in a
  backup taken meanwhile, for as long as the backup is kept. If its address
  already holds a credential, as when a second browser asks for a code, the
  row's email hash joins it to that credential, which it says was issued before
  then; if it is confirmed, a backup that holds it dates the credential it
  becomes to within the code's lifetime. Requests are few at 03:17 UTC, so
  this is seldom, but no sweep can rule it out.
- **Backups** hold rows as they stood when they were taken. The instance keeps a
  daily backup, the last seven — about a week — each started in a window that
  opens at 05:00 UTC ([deploy guide](deploy/README.md) section 2.2). A
  credential issued between the sweep and the backup, which can start as late as
  09:00 UTC, is in that backup as it was before the next sweep — with its own
  transaction id, and its deleted pending row perhaps still in the file — for
  as long as the backup is kept. So is every credential on a night the sweep
  failed. Every backup holds the posts, and with them the gaps above, whatever
  the sweep did; and one that copies the write-ahead log with the instance's
  files can hold that night's sweep's own copies of the credential rows as they
  were before it (next point), which date every credential issued since the
  night before.
- **The write-ahead log the server keeps for its own crash recovery.** Every
  write reaches it first: each issuance's insert into `verification_credentials`,
  with the row in it, and the commit record of its transaction, with the time
  to the microsecond. Later writes copy rows into it again. With full-page
  writes on, as PostgreSQL has them by default, the first change to a page after
  each checkpoint logs an image of the whole page as it stood, and the sweep's
  own delete changes every page of the credentials table. So the log written
  since the last checkpoint before the sweep holds a copy of every credential
  row as it was before the rewrite — in the sweep's own records, or in an
  earlier change's since that checkpoint — each with its own transaction id,
  which the posts' times date again: every credential issued since the previous
  sweep, and on the first sweep that rewrites the table, every credential in
  it. A segment of the log stays until later checkpoints let PostgreSQL delete
  it or reuse it for new log — within hours on a busy instance, days on a quiet
  one — so the log dates a credential until the newest segment holding its row
  goes, which is the sweep's or close to it, not the segment its issuance was
  written to. Until then a superuser (with `pg_walinspect`) or root on the
  machine (with `pg_waldump`) can read it, sweep or no sweep. A backup that
  copies the instance's files copies the log with them.
- **Point-in-time recovery is off.** Turning it on would archive a week of
  that log, and with it the commit time of every transaction, which dates every
  write exactly; rewriting rows does nothing to a log of how they were written,
  and the rewrite itself logs the rows it replaced.
- **`track_commit_timestamp` must stay off**, as the deploy guide's section 2.2
  says. With it on, PostgreSQL records when each transaction committed, and a
  row's `xmin` gives the time it was written to well under a second.

Nothing more than that. In particular, the files `VACUUM FULL` drops and the
log segments PostgreSQL deletes or reuses go the ordinary way, not wiped: what
the disk under them still holds until it is written over is outside all of
this.

### Domain allowlist

The allowlist is a **file**, not a table: the mapping from a domain to a badge
is configuration rather than data about anybody, so it stays out of the
database and gets reviewed in a diff. The default is embedded in the binary at
[`internal/app/verify_domains.txt`](internal/app/verify_domains.txt).

```
ac.in                     # a suffix: ana@cse.nitk.ac.in earns [nitk.ac.in]
iitb.ac.in = IIT Bombay   # an institution: ana@cse.iitb.ac.in earns [IIT Bombay]
!nsw.edu.au               # a denial: refused, with every subdomain
```

A suffix entry accepts anything under it and badges the domain one level below
the suffix, so two departments of one university read the same. An institution
entry names the place instead, and wins over any suffix. An address sitting
directly on a suffix (`someone@ac.in`) is refused: a namespace is not an
institution. A denial refuses a domain and everything under it, whatever else
matches; it exists for school namespaces that share a suffix with universities.
School addresses are refused on purpose, for child safety: a badge naming a
school marks its poster as a child at a findable place, and school mailboxes are
often monitored and closed when a pupil leaves. That limits the badge, not who
may use the site. The aim is that every badge names a university or college, but
that is a goal, not a guarantee: `ac.uk` also admits sixth-form colleges, and no
suffix maps exactly onto higher education. Suffixes shared with schools are
either guarded by denials (`edu.au`, `kvs.ac.in`) or left out in favour of named
universities (`edu.in`, `ac.jp`, `ac.th`, `edu.sg`, `edu.hk`), and `sch.uk` is
not listed. Every suffix in the file is marked reviewed or not checked yet. No
EU member-state suffixes are listed while the site does not target the EU.
`EDUCHAN_VERIFY_DOMAINS` points at a file that **replaces** the
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
the one place in educhan an address is rendered at all. So the server refuses to
start with `EDUCHAN_VERIFY_SECRET` set and no real sender unless
`EDUCHAN_MAIL_DEV=1` says the development sender is meant. Adding SMTP or Google
Workspace means writing another `Sender` and assigning it to `Config.Sender`;
no handler and no SQL changes.

### Anti-abuse

| Limit | Effect |
| --- | --- |
| One credential per address | Re-verifying issues a new credential and revokes the previous one, so a mailbox is one identity however many browsers it uses |
| Plus-addressing stripped | `ana+one@` and `ana+two@` are the same mailbox. Dots are *not* stripped: that is a Gmail convention, and applying it everywhere would fold two real people at one institution together |
| Code request cooldown | `EDUCHAN_VERIFY_COOLDOWN`, applied per client address **and** per address hash, so neither a client can walk a list of addresses nor a mailbox be flooded from a pool of hosts |
| Five wrong codes | The pending verification is destroyed and a fresh code is the only way on, which is what makes a six-digit code safe |
| Credential expiry | `EDUCHAN_VERIFY_CREDENTIAL_TTL`, 180 days by default. The credential is kept in `localStorage`, so this is the lifetime in practice rather than an upper bound the browser cuts short |

The one cookie this feature sets is the in-flight verification token, which is
scoped to `/api/verify` and lives for one code's lifetime: it goes to the
verification endpoints and nowhere else. It is `HttpOnly`, `SameSite=Lax`, and
`Secure` whenever the request arrived over TLS. The credential is not a cookie
at all — see above.

## Posting identity

### Poster IDs

Every post shows an eight-character ID that is the same for one poster
throughout a thread and different in every other thread, so a conversation can
be followed without anybody being followable from thread to thread. It is
`HMAC(EDUCHAN_POSTER_ID_SALT, thread, source)`, and the source is the first of:

1. **On a badged post, the credential's random id.** Never the browser's key or
   address: if a badged post and an unbadged reply from one browser showed the
   same ID, the ID would tie the unbadged reply to the institution. It is the id
   inside the credential the poster presented, not anything the database keeps
   (which is only its SHA-256), so a badged post's ID can be recomputed only by
   somebody holding the credential itself. One credential's badged posts share
   an ID within a thread, as anybody's posts do.
2. **Otherwise the poster key**: 16 random bytes the browser makes once, keeps
   in `localStorage`, and sends base64url-encoded in `X-Educhan-Poster-Key` on
   thread and reply requests and nothing else. This is what gives two people
   behind one campus NAT two IDs.
3. **Otherwise the client address**, which is what every ID used to be, so a
   client that sends no key can still post.

The three are tagged before hashing (`key:`, `ip:`, `cred:`), so no choice of
key can land on the ID an address or a credential gets.

The poster key is HMAC input and nothing else: it is never written to the
database and never logged. `TestPosterKeyIsNeverStoredOrLogged` posts every way
there is with one, then searches every column of every table and every log line
for it. Kept anywhere, it would be a single identifier across all of a
browser's threads, which is exactly what per-thread IDs avoid. Because the
client chooses it, a poster ID is a courtesy rather than proof: clearing site
data gives a fresh one, as changing networks always did, and flood control
stays keyed on the address for that reason.

### Tripcodes

A name of `ana#secret` posts as **ana** with a tripcode such as `!!Xq3vB0e9kT`,
which lets a poster show that two posts came from whoever knows the secret,
without an account. `#secret` and `##secret` are the same thing: both make the
one kind of tripcode educhan has, `HMAC(EDUCHAN_POSTER_ID_SALT, secret)`, shown
with the `!!` prefix imageboards use for a server-keyed tripcode.

The classic unkeyed tripcode, with a single `!`, is not offered. It is a plain
hash of a secret somebody chose by hand, so anyone who sees the tripcode can
search for the secret offline and then post as its owner. Keyed by the salt, a
tripcode can only be produced through this server. The salt is also what keeps
tripcodes stable: rotate `EDUCHAN_POSTER_ID_SALT` and every one of them changes,
along with every poster ID.

## Image attachments

One image per post, on both thread creation and replies, **when
`EDUCHAN_UPLOADS_ENABLED` is on**. It is off by default, and a post carrying an
image is then refused with `403` and
`{"error":"image uploads are switched off on this server"}`, because originals
are stored and served as uploaded: no metadata stripping and no content scanning
yet. With uploads off a multipart post is held to the 64 KiB a JSON post may
be, so the server stops reading an image once the body passes that, and a file
over the size limit gets the same `403` rather than a `413`; a multipart post
with no file, or with the empty file part a browser sends for an untouched
input, posts as usual. `GET /api/site`
reports the switch and the size limit as `{"uploads","max_upload_bytes"}`, so a
client can leave the image picker off a server that would refuse what it sends.

- **Formats**: JPEG, PNG, WebP, and GIF, identified by magic bytes rather than by filename
  or the content type the client claims. Anything else, or an image whose header claims
  more than 50 megapixels, is refused with `400` from its header alone, before flood
  control.
- **Spoilers**: a `spoiler` field (`1`, `true`, or `on` in a form; a boolean in JSON) marks
  the image to be covered until it is clicked. The attachment comes back with
  `"spoiler": true`; the thumbnail is made exactly as for any other image, because covering
  it is the client's job.
- **Per-thread cap**: a thread holds at most its board's `image_limit` posts with an image
  (150 by default), the opening post included. Past it a reply with an image answers
  `403` with `{"error":"image limit reached"}`; text replies carry on. Thread views report
  `images` and `at_image_limit` so a client can hide the picker before anyone tries.
- **Size limit**: 8 MiB by default (`EDUCHAN_MAX_UPLOAD_BYTES`).
- **Thumbnails**: generated at upload time, 250px on the longest side, aspect ratio
  preserved and never upscaled. JPEG sources keep JPEG thumbnails; the formats that can
  carry transparency get PNG thumbnails.
- **Storage**: originals and thumbnails are keyed
  `originals/{board}/{YYYY}/{MM}/{random}.{ext}` and `thumbs/{board}/{YYYY}/{MM}/…`, under a
  generated random filename. Two backends implement that key layout — the local filesystem
  and Google Cloud Storage — and [Storage backends](#storage-backends) covers choosing one.
  All writes and reads go through the `internal/media.Store` interface, so which one is in
  use is invisible to the handlers and to the database.
- **Serving**: `/media/original/{path}` and `/media/thumb/{path}`, with the correct
  `Content-Type`, range support, and long-lived immutable caching. Media streams through
  the application on every backend; a Cloud Storage bucket is never public.
- **UI**: thumbnails appear in the board index, the catalog, and inside threads. In a
  thread a thumbnail expands to the full image in place — the full file is decoded before
  the swap, so the picture never blinks — and collapses again on a second click or
  <kbd>Esc</kbd>. Elsewhere it opens in a new tab. Attaching an image is a button, a drop
  target on the posting form, or both, with a preview of what will be sent.

Posting an image over HTTP is a plain multipart form:

```bash
curl -X POST localhost:8080/api/boards/chem/threads \
  -F subject="lab setup" -F body="finally works" -F image=@photo.jpg
```

The JSON API (`-d '{"body":"…"}'`) still works for text-only posts.

### Implemented

| Area | Behaviour |
| --- | --- |
| Boards | Twenty seeded from a migration — subjects from `/math/` to `/lang/`, then student life from `/adm/` to `/meta/` — and more created and reconfigured over the API with the moderation key; each has its own bump limit, thread limit, and post numbering |
| Posting | Anonymous by default; `name#secret` and `name##secret` both produce the same salted `!!` tripcode; a per-thread poster ID, from a random key the browser keeps, keeps a conversation followable |
| Threads | Per-board post numbers, `>>123` quote links with backlinks, greentext, `[spoiler]` text, highlighted `[code]` blocks, `[math]` and `[eqn]` maths typeset by KaTeX, `>>>/board/` and `>>>/board/123` cross-board links, sage, bump limits, sticky and locked flags |
| Index | Paginated, newest bump first, showing each thread's opening post and last three replies |
| Catalog | Every live thread on one page as excerpt tiles with thumbnails; an excerpt shows a spoiler as `[spoiler]`, never what it hides |
| Attachments | One image per post and a per-board cap per thread, validated by magic bytes, thumbnailed at upload, served from `/media/…`; off unless switched on, and `GET /api/site` says which |
| Verification | Optional institutional badge from an emailed code; the address is never stored and the badge cannot be linked to a post |
| Moderation | Sticky, lock, and delete a post or a whole thread, plus creating and reconfiguring boards, behind a shared moderation key |
| Archive | Threads pushed past a board's thread limit are archived rather than deleted: they leave the index and the catalog, refuse replies, and still load by number |
| Housekeeping | `educhan -sweep` deletes archived threads, media included, once their board's `archive_days` have passed, then rewrites the verification credentials so no row carries its issuance's transaction id; per-client cooldowns limit new threads, replies, and uploads, and forget each address within a minute of its cooldown ending while the process has CPU between requests (see Flood control for an idle Cloud Run instance) |

## Front end

The UI is three files served from the Go binary — `index.html`, `app.css`, `app.js` in
`internal/app/web/` — with no build step and no framework, plus a vendored highlight.js
and KaTeX, each fetched only when a post that needs it is on screen. `app.js` renders the
JSON API into routes that mirror it (`/`, `/{slug}/`, `/{slug}/catalog`,
`/{slug}/archive`, `/{slug}/thread/{no}`), so every view is a shareable link and the back
button works. `/{slug}/post/{no}` names a post by number alone: it asks the server which
thread the post is in and replaces itself with that thread, scrolled to the post, so Back
does not land on it twice. Institution boards are stored as `u-{name}` and live at
`/u/{name}/`, with the same routes beneath. The top-level words kept for pages — `verify`,
`mod`, `u`, `rules`, `faq`, `legal`, `terms`, `privacy`, `dmca`, `report`, `archive`,
`safety`, and the server's own `api`, `media`, `healthz`, and `lib` — are never read as
boards.
`/verify`, `/rules`, and `/faq` are pages; a word without a page yet shows the not-found
view.

- **Age screen**: educhan is open to anyone aged 13 and over (`MIN_AGE` in `app.js`; below
  13, COPPA's verifiable parental consent applies, which an anonymous site cannot give).
  Before anything is shown or fetched, a first visit gets a modal asking only "When were
  you born?", as a month and a year with nothing preselected and nothing hinting at the
  threshold. Age is counted conservatively, so a birthday in the current month has not
  happened yet. The answer is compared in the browser and then removed from the page: it
  is never stored, sent, or put in the URL. All that is kept, in `localStorage`, is the
  outcome — `educhan:age-ok-v1` after **Continue**, or `educhan:age-refused-v1`, which
  shows a plain page saying who educhan is for, with no way to answer again in that
  browser. The one refusal not kept is an answer in the current month exactly 13 years
  back, since that reader may have turned 13 this month: it lasts until the page is
  reloaded or closed, and the FAQ says a reader who may only just have become old enough is asked
  again. Escape does not dismiss the screen. If storage is blocked, the screen is asked
  once per visit. `/rules` and `/faq` are readable without answering it, and `MIN_AGE = 0`
  switches it off.
- **Rules and FAQ**: `/rules` holds the global rules in plain words for readers as young
  as 13. It lists what is not allowed for anyone, kind by kind (sexual content,
  encouraging suicide, self-harm or eating disorders, "kys", bullying, hate, threats,
  violence and gore, dangerous challenges, harmful substances, contact details and moving
  to other apps, and posts about real people you know), says there are no private
  messages on purpose, and says what moderators can do today: delete posts and threads,
  and lock a thread. Boards can add their own rules under the header's **Rules**
  disclosure. `/faq` covers what educhan is, who it is for, why it asks when you were
  born, why there are no accounts or private messages, what anonymity here does and does
  not hide, the badge, tripcodes, per-thread IDs, markup, and the bump limit, image cap,
  and archive. Both end with the same fixed list of helplines (US, UK and Ireland, India,
  Australia, and Find A Helpline for everyone else), which is static text: nothing works
  out where the reader is. The footer links to both on every view; its invitation to
  verify an address shows only on a server with verification switched on.
- **Reading**: a subject heads its thread, with the poster's name, tripcode, per-thread ID,
  time, and post number as secondary metadata; sticky, locked, archived, and bump-limit
  badges are tinted chips. Each board's archive lists the threads that have left it,
  newest first; an archived thread still opens, says when it was archived and how long it
  is kept, and shows why it takes no replies where the reply form would be. A board with
  rules shows them under a **Rules** disclosure in its header, as plain text with the line
  breaks kept. On a fine pointer, hovering `>>123` floats a preview of the quoted post
  when it is already on the page — a preview never costs a request.
- **Post markup**, rendered at display time from the text as typed:
  - `>text` is greentext; `>>123` links to a post on the same board and adds a backlink.
  - `>>>/math/` links to a board, `>>>/math/123` to a post on it, and `>>>/u/iitb/123` to
    one on an institution board.
  - `[spoiler]…[/spoiler]` is a bar until hovered, focused, or tapped; a screen reader
    hears "hidden spoiler" until the reader activates it, and catalog and archive
    excerpts show the word, not the text.
  - `[code]…[/code]` is a block shown verbatim and syntax-highlighted with the language
    auto-detected, as it comes near the screen; a block over 2,000 characters is shown
    plain, because highlighting one that long could stall a reader's tab. Nothing inside
    it is markup, so `x >>2` in code links nothing and creates no backlink.
  - `[math]…[/math]` is inline maths and `[eqn]…[/eqn]` an equation on a line of its own,
    both in TeX and typeset by KaTeX as they come near the screen. Like code, nothing
    inside is markup. Until KaTeX arrives, or if it never does, the TeX shows as written;
    a TeX error shows the source in red with the reason on hover. TeX in a post makes no
    links, images, or HTML of its own (`\href`, `\url`, `\includegraphics` and the
    `\html…` commands show as unsupported), and cannot define macros (`\def`,
    `\newcommand` and the like): a macro is how a short post would expand into millions
    of symbols in every reader's tab. Equation tags and numbering still work (`\tag{1}`,
    `\tag*{A}`, `\nonumber`, `\notag`). An equation too wide for a phone scrolls sideways
    instead of widening the page. Catalog excerpts show maths as its TeX. Inside a hidden
    spoiler, maths and code show nothing — not a TeX error's red source, nor a diff's line
    tints — and are out of the tab order until the spoiler is revealed.
  - A tag pairs with the first closing tag of its kind after it; one that never closes,
    an empty pair, or a `[spoiler]` opened inside another shows as the text that was
    typed.
- **Posting**: labelled fields, an image picker that previews the chosen file with its name
  and size, and drag-and-drop anywhere on the form. On a server with uploads switched off,
  which the page learns from `GET /api/site`, the form has no image field at all and the
  thread view leaves out the image cap; a page that missed the switch takes the image off
  when the server refuses it, keeps the text, and the next press posts it. The picker
  names the server's size limit, and a file over it is turned away when it is picked
  rather than after the upload. A half-written reply survives
  **Update**, image included, so refreshing a busy thread never costs a draft; if the
  thread was archived or locked meanwhile, the text stays on the page in a read-only box
  to copy. Following a quote link within a thread jumps to the post without reloading
  the thread, so revealed spoilers stay revealed. Clicking a
  post number quotes it into the reply box. With an image attached, **Spoiler image**
  sends it hidden: readers see a neutral tile, filename included, until they click it or
  press Enter on it, and it then expands like any other; the catalog shows the tile only.
- **Image cap**: the index and catalog show how many images a thread has, and on a server
  that takes images the thread view shows the count against its board's cap. A thread at the cap keeps its reply form
  with the image picker switched off and the reason beside it; if the cap is reached
  while a reply is being written, the refusal takes the image off, keeps the text, and
  the next press posts it. On a board whose `image_limit` is 0 the picker says replies
  there can't carry images, and no thread is badged as at its limit.
- **Poster key**: the first post from a browser makes a random key and keeps it in
  `localStorage`; it travels in `X-Educhan-Poster-Key` on new threads and replies only, so
  two people on one campus network get different poster IDs without the key riding on
  anything else. A badged post sends the credential instead, never both. The Name field
  remembers the last unbadged post's name; ticking the badge empties it, because a
  tripcode on both kinds of post would tie them together. **Forget this browser** on
  `/verify` drops the key, the name and any badge, for a shared computer.
- **Phones**: a single fluid column with the catalog at two tiles across, thumbnails that
  never overflow the viewport, and 16px form text so iOS does not zoom on focus.
- **Accessibility**: a skip link, one visible focus ring on every control, headings and
  landmarks that match the layout, live-region announcements for client-side navigation,
  colours that clear WCAG AA on their own background, and `prefers-reduced-motion` support.

The page runs under the same-origin CSP in `middleware.go`, which has no `unsafe-inline`
exception: no inline `<script>`, no inline `<style>`, and no `style` attributes. A unit
test (`internal/app/web_test.go`) fails the build if any appear in `index.html`, `app.js`,
or `app.css`, and the image preview is a `data:` URL because that is what `img-src`
allows. Third-party code is vendored under `internal/app/web/lib/` and served from the
same origin; each library keeps its licence and a note of its source, and the same test
file pins every vendored file by SHA-256. The directory is not called `vendor/` because a
Go module zip leaves out every directory of that name, so a binary built from the module
proxy would have shipped without the libraries. KaTeX is the reason maths is typeset with
`katex.render` into the page rather than `renderToString` into `innerHTML`: its HTML
carries layout in `style` attributes, which this CSP refuses, while `katex.render` sets
styles through the CSSOM, which it allows.

None of these files has a version in its URL — `/app.js` is `/app.js` in every release —
so each goes out with `Cache-Control: no-cache` and a strong `ETag` from a hash of its
bytes: a browser keeps its copy and revalidates it, and an unchanged file costs a `304`
with no body. Text files (HTML, JavaScript, CSS) are gzipped once at startup and sent
compressed to a client that accepts gzip, with their own `ETag` and `Vary:
Accept-Encoding`; the fonts are compressed already. JSON from the API is not compressed.

## Tech Stack

- **Backend**: Go 1.22 standard library HTTP server, `pgx` for PostgreSQL
- **Database**: PostgreSQL 16, versioned SQL migrations applied on startup
- **Front end**: dependency-free HTML/CSS/JS served from the Go binary
- **Deployment**: multi-stage Docker build onto a distroless base, built by Cloud Build into Artifact Registry; Cloud Run with Cloud SQL (see [Deployment](#deployment))
- **Planned**: Google Cloud Storage, Gemini API + Cloud Vision for moderation, Cloud CDN + Cloud Armor

## Running locally

Requires Go 1.22+ and a reachable PostgreSQL instance. No containers are
involved: this is the documented way to run educhan on a development machine.

```bash
make db-up                      # start a local PostgreSQL cluster (or use your own)
make run                        # go run ./cmd/server, on http://localhost:8080
```

`make db-up` is `bash .cursor/scripts/start.sh`: it starts a user-owned
PostgreSQL on `localhost:5432` and ensures the `educhan` role and database exist.
It is idempotent, so re-running it is how you check the database is up. Point at
a different server instead by exporting `DATABASE_URL`.

Migrations in `internal/app/migrations/` are applied automatically at startup,
guarded by a Postgres advisory lock so concurrent instances are safe. There are
no down migrations. A release started on a database that a newer release has
migrated still serves, but logs an error naming the versions it does not know,
and the sweeper refuses to run on it. After the migrations, any board whose slug
has since become a reserved word (a page such as `/archive/` or `/rules/`) is
renamed to `<slug>-board`, with a warning in the log, because the UI would read
its address as the page.

### The container image

The image Cloud Run runs is built by **Google Cloud Build** from the `Dockerfile`
and pushed to **Artifact Registry**, tagged with the short commit SHA and
`latest`:

```bash
make cloud-build    # gcloud builds submit --config cloudbuild.yaml
make cloud-deploy   # roll that image onto the Cloud Run service
```

So building the image needs a Google Cloud project rather than a Docker daemon,
and a change to the `Dockerfile` is checked by submitting a build. The
[Deployment](#deployment) section is the short version;
[`deploy/README.md`](deploy/README.md) has the one-time project setup, the IAM
the build needs, and the rest of the walkthrough.

### Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://educhan:educhan@localhost:5432/educhan?sslmode=disable` | PostgreSQL connection string; a `host=/path` makes it a Unix socket |
| `EDUCHAN_ADDR` | `:8080` | Listen address; overrides `PORT` |
| `PORT` | unset | Bare port number, honoured when `EDUCHAN_ADDR` is unset — the contract Cloud Run and similar runtimes inject |
| `EDUCHAN_POSTER_ID_SALT` | random per process | Keys poster IDs and secure tripcodes; set it so they survive a restart |
| `EDUCHAN_DB_MAX_CONNS` | `3` | Connection pool cap per instance, overriding any `pool_max_conns` in `DATABASE_URL`. Instances times this must fit the database's connection limit |
| `EDUCHAN_TRUST_PROXY` | `0` | Number of reverse proxies in front of the server; see [Running behind a proxy](#running-behind-a-proxy) |
| `EDUCHAN_THREAD_COOLDOWN` | `30` (seconds) | Minimum interval between new threads from one client, per board |
| `EDUCHAN_POST_COOLDOWN` | `5` (seconds) | Minimum interval between replies from one client, per board |
| `EDUCHAN_UPLOAD_COOLDOWN` | `15` (seconds) | Minimum interval between attachments from one client, across all boards |
| `EDUCHAN_LOG_FORMAT` | `text` | `text` or `json` structured logs |
| `EDUCHAN_MEDIA_BACKEND` | `local` | Where attachments are stored: `local` or `gcs`; see [Storage backends](#storage-backends) |
| `EDUCHAN_UPLOAD_DIR` | `./data/uploads` | Root the `local` backend writes originals and thumbnails to; ignored by `gcs` |
| `EDUCHAN_GCS_BUCKET` | empty | Cloud Storage bucket the `gcs` backend uses. **Required** when the backend is `gcs`, ignored otherwise |
| `EDUCHAN_GCS_PREFIX` | empty | Optional key prefix inside that bucket, so it can be shared with other content |
| `EDUCHAN_MAX_UPLOAD_BYTES` | `8388608` (8 MiB) | Largest accepted attachment |
| `EDUCHAN_UPLOADS_ENABLED` | off | Accept image attachments. Off refuses any post carrying an image with `403`, reading no more of it than a text post may be, and `GET /api/site` reports which |
| `EDUCHAN_MOD_KEY` | empty | Shared secret for the moderation and board management endpoints; unset means they refuse every request |
| `EDUCHAN_VERIFY_SECRET` | empty | Keys institutional verification: the address hash, the pending code hash, and the credential signature. **Unset switches the whole feature off**; rotating it revokes every credential |
| `EDUCHAN_VERIFY_CODE_TTL` | `15m` | How long an emailed code stays usable |
| `EDUCHAN_VERIFY_CREDENTIAL_TTL` | `4320h` (180 days) | How long a badge lasts before the holder verifies again |
| `EDUCHAN_VERIFY_COOLDOWN` | `60` (seconds) | Minimum interval between code requests, per client and per address |
| `EDUCHAN_VERIFY_DOMAINS` | embedded list | Path to an allowlist file replacing the built-in one |
| `EDUCHAN_MAIL_DEV` | off | Allow the development mail sender, which prints codes and addresses to stdout. Without it the server refuses to start with verification on and no real sender |

### Storage backends

Attachments are written through one interface, `internal/media.Store`, with two
implementations. `EDUCHAN_MEDIA_BACKEND` picks between them, and nothing above
the interface — no handler, no query, no column — knows which one answered. Both
are held to the same contract by a shared conformance suite in
`internal/media/conformance_test.go`.

**`local` (the default)** keeps files under `EDUCHAN_UPLOAD_DIR`. Nothing else
needs configuring, which is why it is the default: an existing deployment and a
fresh checkout both keep working untouched.

```bash
EDUCHAN_MEDIA_BACKEND=local            # implied when unset
EDUCHAN_UPLOAD_DIR=./data/uploads
```

**`gcs`** keeps objects in a Google Cloud Storage bucket, which is what lets
uploads outlive a container. Use it on Cloud Run, where the local filesystem is
per-instance and in-memory.

```bash
EDUCHAN_MEDIA_BACKEND=gcs
EDUCHAN_GCS_BUCKET=educhan-media       # required
EDUCHAN_GCS_PREFIX=media               # optional; omit to use the bucket root
```

- **Credentials come from [Application Default Credentials][adc]** — the attached
  service account on Cloud Run, or `GOOGLE_APPLICATION_CREDENTIALS` pointing at a
  key file locally. There is no key file in this repository or in the image, and
  no setting that would accept one. The bucket needs `roles/storage.objectAdmin`
  granted on the bucket itself, and nothing broader;
  [`deploy/README.md`](deploy/README.md) has the exact commands.
- **The bucket stays private.** Media is streamed through the application on
  `/media/original/…` and `/media/thumb/…` exactly as with the local backend, so
  the same `Content-Type`, immutable caching, and range handling apply. Serving
  signed URLs instead would take that hop out, and is a later optimisation rather
  than a change of contract.
- **Keys are identical on both backends**, including the traversal rejection that
  validates them, so the same object layout works either way and a bucket can be
  synced to disk or back.
- **A misconfigured bucket stops the server starting.** The API answers `404` for
  a missing bucket and a missing object alike, so a mistyped name or a missing
  grant would otherwise show up as every image quietly 404ing. The store probes
  the bucket at startup instead, using the object-list permission
  `roles/storage.objectAdmin` already carries.

[adc]: https://cloud.google.com/docs/authentication/application-default-credentials

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
request rejected for any other reason — an empty body, an oversized image, a
file that is not an accepted image or whose header claims too many pixels, a
reply to a missing, locked or archived thread or an image past a thread's cap —
does not start a cooldown, so a typo never costs a wait. Two refusals come only
after the cooldowns have started: an image whose header reads but whose pixels
do not decode, which only decoding it can find, and decoding is the cost the
upload cooldown is there for; and the loser of two replies racing for a
thread's last image slot (see the HTTP API).

The cooldowns are keyed on client addresses, and they are the only place the
server holds one. A sweep runs once a minute and drops every entry whose
cooldown has run out, so **an address is held in memory no longer than the
longest cooldown that applies to it plus one minute**, counted from its last
post (or code request, for verification's cooldown), whether or not anyone
else posts in the meantime, **as long as the process has CPU between
requests**. A host that gives it CPU only while it answers a request, which is
Cloud Run's default (request-based billing), holds the sweep's timer back while
an instance is idle. The first request to arrive after a sweep fell due runs
it before it is handled, but until one arrives, or the instance is shut down,
an idle instance's memory still holds the addresses it last saw. On Cloud Run
the one-minute bound needs CPU allocated for the instance's whole life, which
is also how it is then billed: `run.googleapis.com/cpu-throttling: "false"` on
the revision, or `--no-cpu-throttling` on `gcloud run deploy`.

The address itself is never written to the database or the logs. A post from a
client that sends no poster key does store a poster ID derived from it (see
[Poster IDs](#poster-ids)): eight characters of an HMAC keyed by
`EDUCHAN_POSTER_ID_SALT`, which anyone holding the salt could test candidate
addresses against, one thread at a time. The web client always sends a key.

State is held in the process, so each instance of a horizontally scaled
deployment enforces the cooldowns it sees. Moving to shared storage means
replacing `cooldownLimiter` in `internal/app/ratelimit.go`, which nothing else
depends on.

### Running behind a proxy

Flood control is keyed on the client address, and so is the poster ID of a
client that sends no poster key, so the server has to know how to find it.
`EDUCHAN_TRUST_PROXY` is the number of reverse proxies in front of it:

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
| `GET` | `/api/site` | Posting settings — `{"uploads","max_upload_bytes"}`: whether posts may carry an image, and the largest one taken. The same answer for every caller; it reads nothing from the request |
| `GET` | `/api/boards` | All boards |
| `GET` | `/api/boards/{slug}` | One board and its limits |
| `GET` | `/api/boards/{slug}/threads?page=N` | Index page: opening post and last replies per thread |
| `POST` | `/api/boards/{slug}/threads` | Create a thread — `{"subject","name","body","badge","spoiler"}` |
| `GET` | `/api/boards/{slug}/catalog` | Every live thread as excerpt tiles |
| `GET` | `/api/boards/{slug}/archive` | Archived threads, most recently archived first: `{"board","threads":[{"no","subject","excerpt","replies","created_at","archived_at"}]}`, at most 3000 |
| `GET` | `/api/boards/{slug}/threads/{no}` | One thread with all posts, live or archived |
| `GET` | `/api/boards/{slug}/posts/{no}` | Which thread a post is in — `{"board","no","thread_no"}`, live or archived; `404` when the board has no such post |
| `POST` | `/api/boards/{slug}/threads/{no}/posts` | Reply — `{"name","body","sage","badge","spoiler"}` |
| `GET` | `/api/threads/{id}` | Thread by primary key |
| `POST` | `/api/threads/{id}/posts` | Reply by thread primary key |
| `GET` | `/api/verify` | Whether verification is configured — `{"enabled"}`. It does not read your credential |
| `POST` | `/api/verify/request` | Email a code — `{"email","adult"}`; `"adult"` must be `true` |
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

The two posting endpoints read the poster key from `X-Educhan-Poster-Key` (22
base64url characters, no padding) and no other endpoint reads it. Without the
header the poster ID comes from the client address; with a malformed one the
post is refused with `400` and `{"error":"invalid poster key"}` before its body
is read, so the refusal costs no cooldown.

`/api/verify/request` without `"adult": true` answers `400` with
`{"error":"the badge is for students aged 18 and over; you can still read and post anonymously"}`
and does nothing else: no code, no row, no cooldown. `"adult"` is the
requester's own word that they are 18 or over, and it is never stored.

`/api/verify/confirm` sets no credential cookie, but the two-step exchange does
use a short-lived one scoped to `/api/verify`, so an HTTP client still needs a
cookie jar (`curl -c/-b`) to get through it.

Every thread view carries `archived_at`: `null` while the thread is live, and
the time it was pushed off the board once it is archived. It also carries
`images`, the posts in the thread with an image, and `at_image_limit`. A reply
to an archived thread answers `403` with `{"error":"thread is archived"}`, to a
locked one `{"error":"thread is locked"}`, and one bringing an image to a thread
at its board's image limit `{"error":"image limit reached"}`. These refusals,
and a reply to a thread that does not exist, come before flood control and
before the image is stored, so they start no cooldown, and a reply refused at
the image limit posts at once when sent again without the image. (Two replies
racing for a thread's last image slot can both get that far; the loser is
refused inside the transaction, after its cooldowns have started.)

JSON request bodies must be `application/json`; unknown fields are rejected. The
two posting endpoints also accept `multipart/form-data` with the same fields
plus an `image` file. Errors are `{"error": "..."}`, and `429` responses add
`retry_after` plus a `Retry-After` header.

```bash
curl -s localhost:8080/api/boards
curl -s -X POST localhost:8080/api/boards/career/threads \
  -H 'Content-Type: application/json' \
  -d '{"subject":"placement season","name":"ana#trip","body":"how did yours go?"}'

# Verification, end to end. "adult": true says you are 18 or over; without
# it the request is refused. With no mail provider configured the code is
# printed to the server's stdout. Confirming returns the credential; hold on
# to it, because the server will not show it again.
curl -s -c jar -X POST localhost:8080/api/verify/request \
  -H 'Content-Type: application/json' -d '{"email":"you@iitb.ac.in","adult":true}'
CRED=$(curl -s -b jar -c jar -X POST localhost:8080/api/verify/confirm \
  -H 'Content-Type: application/json' -d '{"code":"481920"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["credential"])')

# The badge is the header plus the flag. Leave both off and the post is
# anonymous — the credential is not sent at all.
curl -s -X POST localhost:8080/api/boards/career/threads \
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
| `POST` | `/api/boards` | Create a board — `{"slug","name","description","rules","bump_limit","thread_limit","image_limit","archive_days"}` |
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
curl -s -X POST localhost:8080/api/boards/meta/threads/1/sticky \
  -H "Authorization: Bearer $MOD_KEY"
curl -s -X DELETE localhost:8080/api/boards/meta/threads/1/posts/4 \
  -H "X-Mod-Key: $MOD_KEY"
```

#### Board management

The seeded boards are a starting point, not the whole list: a board can be
opened and reconfigured with the same key, without a migration or a release.
Listing boards stays public.

A slug is the board's address in every URL, so it is lowercase letters and
digits in dash-separated groups (`math`, `comp-sci`), at most 24 characters,
and not a first path segment that already means something else: the server's
own routes (`api`, `media`, `healthz`, and `lib` for the UI's libraries), the
UI's pages (`verify`, `mod`, `rules`, `faq`, `legal`, `terms`, `privacy`,
`dmca`, `report`, `archive`, `safety`), or `u`. It is normalised before use, so `/CompSci/` and `/compsci/`
cannot both exist. An institution board is stored as `u-<name>` — `u-iitb` — and the UI
shows it at `/u/iitb/`; the API always addresses it by the stored slug. Everything but the slug is optional: the name falls back to the
slug, the limits to the schema defaults (a bump limit of 300, a thread limit of
100, an image limit of 150, and 7 days in the archive), and the rules to none. A
duplicate slug answers `409`.

| Setting | Range | Meaning |
| --- | --- | --- |
| `bump_limit` | 1–10000 | Replies after which a reply no longer bumps the thread |
| `thread_limit` | 1–1000 | Live threads the board keeps |
| `image_limit` | 0–1000 | Posts with an image one thread may hold, the opening post included; `0` means replies can never carry one |
| `archive_days` | 0–365 | Days an archived thread is kept before it is deleted; `0` means at the next sweep |
| `rules` | up to 4000 characters | The board's own rules, multi-line, normalised like a post body |

None of the numbers has an "unlimited" value: zero is refused for the two
limits where it would break the board, and read literally for the two where it
is a real choice.

```bash
curl -s -X POST localhost:8080/api/boards \
  -H "X-Mod-Key: $MOD_KEY" -H 'Content-Type: application/json' \
  -d '{"slug":"comp-sci","name":"Computer Science","description":"Algorithms and all that"}'
```

`PATCH` changes any of those settings, and `name` and `description`; an
omitted field keeps its stored value, and at least one must be given. The slug
is not updatable — it is what every bookmark and stored quote link points at.

```bash
curl -s -X PATCH localhost:8080/api/boards/comp-sci \
  -H "X-Mod-Key: $MOD_KEY" -H 'Content-Type: application/json' \
  -d '{"description":"Systems, theory, and coursework","thread_limit":50}'
```

A new board is a board like any other: it appears in the board navigation, the
index, and the catalog, and its post numbering starts at No. 1. A lowered
`thread_limit` is applied by the next thread posted to that board, which
archives whatever no longer fits.

## Deployment

The full walkthrough is in [`deploy/README.md`](deploy/README.md); this is the
shape of it.

educhan builds to one static binary with the web UI and the migrations embedded,
so a deployment is that container plus a PostgreSQL. The `Dockerfile` is a
multi-stage build onto a distroless base — no shell, no package manager, running
as a non-root user — `cloudbuild.yaml` is how that image is built and pushed, and
`deploy/cloudrun/service.yaml` is the Cloud Run service that runs it.

```bash
# Build on Cloud Build, push to Artifact Registry as :<short SHA> and :latest
make cloud-build

# Roll it onto the existing service, keeping every other setting
make cloud-deploy
```

Both are thin wrappers, so the underlying commands are no secret:

```bash
gcloud builds submit --config cloudbuild.yaml \
  --substitutions "_REGION=$REGION,_REPOSITORY=educhan,_IMAGE=educhan,_TAG=$(git rev-parse --short HEAD)"

gcloud run deploy educhan --image "$IMAGE" --region "$REGION"
```

The first deploy is longer, because it is what establishes the configuration that
later deploys inherit — the runtime service account, the Cloud SQL instance, and
the secrets. `bash deploy/first-deploy.sh PROJECT_ID`, run in Cloud Shell, does
all of it, the archive sweeper Job included; by hand it is:

```bash
gcloud run deploy educhan --image "$IMAGE" --region "$REGION" \
  --service-account "$RUN_SA" \
  --add-cloudsql-instances "$CLOUDSQL_INSTANCE" \
  --set-secrets "DATABASE_URL=educhan-database-url:latest,EDUCHAN_POSTER_ID_SALT=educhan-poster-id-salt:latest,EDUCHAN_MOD_KEY=educhan-mod-key:latest" \
  --set-env-vars "EDUCHAN_TRUST_PROXY=1,EDUCHAN_MEDIA_BACKEND=gcs,EDUCHAN_GCS_BUCKET=$MEDIA_BUCKET,EDUCHAN_GCS_PREFIX=media,EDUCHAN_LOG_FORMAT=json,EDUCHAN_UPLOADS_ENABLED=false" \
  --allow-unauthenticated
```

One-time setup — enabling the Cloud Build and Artifact Registry APIs, creating
the repository, and the roles the build service account needs to push — is in
[`deploy/README.md`](deploy/README.md#21-one-time-project-setup).

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

Four things need setting for a real deployment, and the deploy guide covers
each: `EDUCHAN_POSTER_ID_SALT` has to be a stable secret or poster IDs differ
between instances; `EDUCHAN_TRUST_PROXY` is `1` behind Cloud Run's front end;
media belongs in Cloud Storage — `EDUCHAN_MEDIA_BACKEND=gcs` with an
`EDUCHAN_GCS_BUCKET` and `EDUCHAN_GCS_PREFIX`, which the sweeper Job must match
exactly, because the container filesystem does not outlive the container; and
Cloud Run's request logs, which carry every visitor's IP, must be excluded from
Cloud Logging before the service takes traffic (deploy guide section 2.8), or
the promise that IPs are not logged does not hold.

### The archive sweeper

Archived threads are deleted by a one-shot mode of the same binary rather than
by anything running inside the server:

```bash
educhan -sweep          # or, from a checkout: make sweep
```

It loads the usual configuration, connects, applies any pending migrations,
deletes every archived thread whose board's `archive_days` have passed — posts,
attachment rows, and then the images and thumbnails in the media store — and
exits. It works in batches of 100 threads, one transaction each. Each batch
records the storage keys of its threads' files in the `media_deletions` table in
the same transaction as it deletes the rows; once the batches are done the
sweep removes each queued file and forgets its key. A file the store refuses to
delete keeps its key, so the next sweep tries it again rather than leaving an
image public with nothing left to say it exists.

A file the media store does not hold also keeps its key and fails the sweep.
Every queued key is a file the server stored, so a store without it is almost
always the wrong store: **the sweeper must run with the server's media
settings** — `EDUCHAN_MEDIA_BACKEND`, `EDUCHAN_GCS_BUCKET`, `EDUCHAN_GCS_PREFIX`,
`EDUCHAN_UPLOAD_DIR` — or it reports every file missing. Once the settings are
fixed, the next sweep removes them. A file that really is gone for good (lost
with a container's disk, or removed by hand) fails every sweep until its row is
deleted from `media_deletions` by hand.

Last, whatever became of the files, it deletes the verification codes that
expired unconfirmed, rewrites the verification credentials, and runs
`VACUUM FULL` on both verification tables, so that a credential row carries the
sweep's transaction id rather than its own and no abandoned code keeps the time
it was asked for ([Transaction ids, and the nightly
rewrite](#transaction-ids-and-the-nightly-rewrite) says why, and exactly what
that protects). It needs no verification secret for that, but its database
role must own the tables, or the database: `VACUUM FULL` skips any other table
with only a warning, so the sweep checks that both tables moved to new files and
fails at once if not (`TestSweepFailsWhenVacuumLeavesATableInItsFile`). Run it
as the role that ran the migrations, which is the server's. The rewrite waits at
most five seconds for its lock. That lock lets plain reads through and waits for
everything else on the credentials table: a transaction that has written to it
or locked rows in it, a `VACUUM`, `ANALYZE` or index build on it, a change to
its schema, or another sweep's rewrite or vacuum. While it waits, confirmations
queue behind it, each holding one of its server instance's few database
connections, and once they hold them all every request on that instance waits
too. Five seconds bounds the wait, not the queue: once the lock is granted,
confirmations wait out the rewrite as well, which grows with the table — about
three seconds for 220,000 credentials, measured on a development machine — so a
confirmation can wait five seconds plus the rewrite. The vacuum never waits for
a lock. It passes over a table another session holds, which VACUUM reports only
as a warning; the sweep's connection asks for warnings itself, so a
`client_min_messages` in `DATABASE_URL` or a role's default cannot make a
moment's reader look like a role that may not vacuum the table
(`TestSweepHearsVacuumsWarningsWhateverTheConnectionAsksFor`). Once it has a
table, though, it holds it against reads as well as writes until it has written
the table out, and badged posts and confirmations wait for the whole run:
between about half a second and two and a half seconds for 220,000 credentials,
measured on development machines, and once they hold all of a server instance's
database connections (three by default) every request on that instance waits
too. So the sweep takes a table only when the run can leave none of the old rows
in its new file, which is not until the oldest transaction that began before the
rewrite has ended; until then it waits, trying the cutoff on an empty table of
its own (`vacuum_probe`) that holds nobody up. It keeps at that for a minute and
then fails, so a session holding a table or an old snapshot for longer — an
export, say — costs a failed run, which the Job's retry or the next night's run
repeats, and for an old snapshot no run of `VACUUM FULL` on a verification table
at all, rather than a site stalled again and again or a run that reports success
with the old rows still in the tables' files. The run that succeeds still holds
badged posts up for as long as it takes, once a night.

It logs one line of counts (threads, posts, attachments, files removed, failed,
and missing, and credentials rewritten) and nothing about which boards or
threads went or what any credential says, and exits non-zero if the database
fails, any file could not be removed or was missing, or the rewrite or the
vacuum did not finish; the error it exits with says why without naming the file.
It also refuses to run on a database a newer release has migrated (see below).
It needs none of the web server's secrets — the poster-ID salt, the moderation
key, the verification secret — and, run without them, it does not warn about
them: those warnings come from the server alone.

On Google Cloud it is meant to run as a Cloud Run Job from the service's image,
started by Cloud Scheduler: Cloud Run gives a service CPU only while it answers
a request, so a timer inside the server would fire whenever it happened to, and
a Job leaves no sweep endpoint to protect. [`deploy/README.md`](deploy/README.md)
section 2.9 sets up that Job and its daily schedule.

### What is not stateless yet

Everything the API serves lives in PostgreSQL, so instances start in any order
and any number. Uploads used to be the exception and are not any more: set
`EDUCHAN_MEDIA_BACKEND=gcs` with a bucket and they live in Cloud Storage,
outliving any one container. Leave the backend at `local` on Cloud Run and the
old caveat still applies — `/tmp` is per-instance and in-memory, so an image
uploaded to one instance answers `404` from another and is lost when the
container stops.

One thing remains per instance:

- **Flood control.** `cooldownLimiter` keeps its state in process memory, so N
  instances enforce the cooldowns N times over. Moving it to shared storage means
  replacing that one type, which nothing else depends on.

## Development

```bash
make db-up   # local PostgreSQL for development and the integration tests
make run     # go run ./cmd/server
make sweep   # go run ./cmd/server -sweep: delete expired archived threads once
make build   # go build ./...
make test    # go test ./... (integration tests need PostgreSQL; they skip without it)
make check   # gofmt check, go vet, go test

make cloud-build   # build the image on Cloud Build, push to Artifact Registry
make cloud-deploy  # deploy that image to Cloud Run
make cloud-image   # print the image reference the two above use
```

The `cloud-*` targets take `PROJECT_ID`, `REGION`, `AR_REPO`, `IMAGE_NAME`,
`IMAGE_TAG`, and `SERVICE` as overrides — `make cloud-build REGION=europe-west1`
— and default the project to `gcloud config get-value project` and the tag to the
short commit SHA.

## Contributing

This project is in early stages. Contribution guidelines will be added once the initial prototype is ready.

## License

MIT
