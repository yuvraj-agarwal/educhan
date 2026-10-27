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
institutional verification, an optional [educhan Pass](#educhan-pass) sold on
Stripe's hosted checkout, and key-authenticated moderation. Janitor accounts,
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

  All the database ever holds: HMAC(secret, address), the label, and an
  expiry rounded up to the day — in a table with no path to any post.
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
page, **Forget this browser** removes the badge together with the poster key,
the remembered name (see below) and any Pass, which are what would otherwise let
the next person at a shared computer post under the same IDs and tripcode, and
drops any Pass purchase the browser is still waiting to collect, so that
person cannot collect it either.

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
- When a credential was issued: in the database, only to the *day*, because the
  stored expiry is rounded up to a UTC day boundary and nothing else in the
  verification tables is timestamped; in the application's log, for as long as
  it is kept, to the second, because the request line for
  `POST /api/verify/confirm` and a bare `verification issued` line each carry a
  time. Neither line says which institution or address the credential was for
  (`TestIssuanceIsLoggedWithoutTheInstitution`), but a badged post made soon
  after an issuance does name the institution, and its time is kept to the
  second too. So an issuance followed by a badged post ties that post to its
  stored record on any day its institution issued only one credential, however
  many other institutions issued that day. How long the request log is kept is
  what limits this.
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
  updates nothing, so there is no "last used" to line up against a post. The
  log's issuance times name no institution, but a badged post soon after one
  does, and that ties the post to a single record on a day its institution
  issued only one credential.
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

## educhan Pass

The educhan Pass is modelled on 4chan's: a term of posting without solving the
captcha (a year, unless `EDUCHAN_PASS_DAYS` says otherwise), bought for a small
fee that helps run the site. It is optional, sold only to people who tick an
unticked "I confirm I am 18 or over" box, and it gives nothing else. A post
made with a Pass looks exactly like any other, and the rules and every
cooldown still apply.

Two of 4chan's Pass rules don't carry over. 4chan ties a Pass to one IP
address at a time, which would mean keeping addresses; here a Pass has an
hourly budget instead, and everyone using one Pass shares it. And 4chan's Pass
lifts IP range and country blocks, which educhan doesn't have.

**The hourly budget.** A Pass skips the captcha for at most
`EDUCHAN_PASS_HOURLY_POSTS` posts (30 by default) in each hour, counted in UTC,
so in India, and wherever else the clock is half an hour off UTC, each one
starts at half past. A post counts only once it is saved: one that is
refused, fails or is turned away by flood control costs nothing. When the
hour's budget is spent, the Pass simply stops skipping the captcha until the
hour ends, and posts with it solve the captcha like anyone else's; no post is
ever refused because of the budget.

Sharing a Pass shares the budget. The count lives in the server's memory, per
instance like flood control, keyed on an HMAC of the code that matches nothing
in the database, and all it holds is which hour it is for and how many posts:
no timestamps, so nothing in memory says when a Pass posted. The flood-control
sweep forgets it within a minute of its hour ending, on the same terms as an
address (see [Flood control](#flood-control) for an idle Cloud Run instance).
A Pass adds no cooldown and lifts none, so a post answers exactly as it would
without one, and its answer says nothing about when the Pass was last used.

**How it is bought.** The Pass page asks the server for a checkout, and the
browser goes to Stripe's own hosted page by an ordinary navigation: no Stripe
script, frame or stylesheet loads on educhan's origin, and the CSP has no
exception for Stripe. Google has no checkout of its own for selling on the web
(Google Pay sits on top of a processor such as Stripe, whose page offers it),
so this is a non-Google service by necessity. Stripe tells the server the
payment went through by a signed webhook, and the browser that paid collects
its Pass with a claim it holds in a cookie scoped to `/api/pass`. The code,
four groups of five letters and digits, is handed to that browser, and the
server keeps only its HMAC. A browser that keeps no cookies or storage is told
so on the Pass page and not sent to Stripe, since it could neither collect a
Pass nor keep one.

**Orders.** A checkout records an order with the term it buys, which is
`EDUCHAN_PASS_DAYS` as it is at that moment and is kept on the order, and a
deadline a week on, rounded up to the next midnight UTC. Changing
`EDUCHAN_PASS_DAYS` therefore changes what is sold from then on and nothing
already paid for, whether it is collected, renewed or reissued later.

Every checkout gives the browser a new claim with only its own order under it,
and an order is collected only through the claim its checkout set. A claim
token copied off a shared computer, or planted in the browser, before somebody
buys there collects nothing of what they buy. It also means a browser collects
only its latest checkout: a Stripe tab left open from an earlier one and paid
after all is not collected there. The page says so where it shows a checkout
being collected, beside Start again, beside a checkout not collected yet, and
under the forms drawn again over a checkout left behind (Start again, back from
Stripe's cancel link, or a checkout whose answer came after the reader left the
Pass page or after the Pass held changed meanwhile, in another tab say, which
is not followed to Stripe), each with Stop collecting it here, but not in a
tab drawn before the other checkout started. That payment is still the buyer's,
and the owner reissues it from the payment on the receipt (see **Lost codes**).
The cookie is set at checkout to outlast its order's deadline, and 30 days past
Stripe's 24-hour checkout, which is as long as a slow payment holds its order
(below) when Stripe reports the checkout completed as it happens. Nothing else
sets the cookie, and only forgetting the claim clears it. A paid order nobody
has collected by the time the cookie runs out is kept longer, and is collected
with a reissue code.

Collecting takes the order under the claim once it is paid: a renewal renews
its Pass by the order's own term, and anything else makes a new Pass, as does
a renewal whose Pass has gone meanwhile. `GET /api/pass` says whether the
claim has anything left to collect, which the page cannot see for itself (the
cookie is `HttpOnly`), so the page collects on every visit until nothing is
left under it. Collecting is safe to repeat: the page draws a random nonce for
each collection and sends it with every claim, and the code is derived from
the claim and the nonce together with the server's secret. A claim whose
answer was lost after the server committed it, to a closed tab or a dropped
connection, is asked again with the same cookie and nonce and given the same
code. The page keeps the nonce in `localStorage` until an answer says nothing
more is waiting; the server keeps neither the nonce nor the code, only the
code's HMAC. Neither half is worth anything alone: the nonce with any other
cookie, or the cookie without its nonce, gets nothing that has been handed
over already, and a Pass refunded away or given a new code by a reissue is
under no code a retry can ask for. A renewal hands over no code, and nothing
is kept to answer it again from, so a renewal's claim asked again answers
`400`, as for a browser with nothing to collect; the renewal is on the Pass
all the same. So when a claim the page asks again with a kept nonce is
refused, the page checks the code it holds (`/api/pass/activate`), stores the
date the server gives and, if it moved on, says the Pass is now good until
then, without saying where the time came from, which it cannot know.

`POST /api/pass/claim` with `{"forget": true}` drops the browser's claim, for a
shared computer: its order moves under a random hash nobody holds, and the
cookie is cleared, so whoever uses the browser next collects nothing of it, even
with a copy of the cookie taken before. Paid, or paid later, the order is still
the buyer's, and the owner reissues it from the payment on the receipt (see
**Lost codes**). Remove the Pass, Forget this browser and Stop collecting it
here all send it. The page stops any collection under way and drops its nonce
before it does, and starts none until the server has the request, so a claim
cannot collect what is being dropped. The browser's other tabs do the same:
while the request is on its way the page keeps a mark in `localStorage`
(`educhan:pass-forget:` and a random token, a mark for each forget, holding the
time it was set), and a tab that sees one appear, or finds one when it is opened
or brought back from the back-forward cache, stops as the tab it was pressed in
did, one brought back dropping what it held when it was left, and starts nothing
until each such mark goes, or for 30 seconds at most, in case the tab that set
it was closed first, counted from the time the mark holds; it then draws the
Pass page if it is open, and a tab opened on an older mark removes it. An answer
to a claim already on its way, in any tab, when one of them is pressed is
dropped without being stored or shown; a Pass it brought is still the buyer's,
and a reissue code from its payment gives it a new code. A checkout still on its
way then is called off, or not followed to Stripe once it answers, and keeps no
claim past the forget (below). Remove the Pass and Forget this browser go
further, in every tab: they also drop a reissue code's nonce, and an answer on
its way to a reissue code or to a check of a Pass code, until the server has the
request. Stop collecting it here leaves those alone, since it is about a
checkout, and a reissue it dropped would be spent with nothing to show for it.

A page on another subdomain of the same site can set a cookie of the same name
for the whole site, at a path the browser sends ahead of the server's own, to
hide the buyer's claim behind it. So the server reads every claim cookie a
request carries, up to 16, more than any browser sends. `GET /api/pass` says
whether any of them has something to collect; a claim collects under each in
turn, one order an answer, and asked again after an answer was lost gives the
Pass made last; and forgetting drops what is under every one. The server can
clear only the cookie it set; one set from elsewhere stays in the browser with
nothing under it.

A checkout's answer also names its order (`"order"`): what the database holds of
its claim, a hash that collects nothing, signed with `EDUCHAN_PASS_SECRET`.
Collecting takes the claim itself, which the page cannot see, and only the
server can make a name, so a copy of the database is no way to drop anybody's
order. A forget pressed in the checkout's own tab needs no name: it first calls
off the checkout's request, if no answer has come, and its own request then goes
after any cookie the checkout set, and drops that claim with the rest. The page
uses the name for a checkout whose answer arrives after Remove the Pass, Forget
this browser or Stop collecting it here was pressed in another tab since its
request went: that forget may have gone too soon to see the claim the answer
set. The page does not follow such a checkout to Stripe, and sends
`{"forget": true, "order": …}`, which drops that claim alone, as a forget drops
each of the browser's, and reads no cookie and clears none: another checkout,
started later, may have set its own cookie by the time the request is answered,
and clearing would clear that one. So the cookie of a claim dropped this way can
stay in the browser, with nothing under it, until another checkout replaces it,
Remove the Pass, Forget this browser or Stop collecting it here clears it, or it
runs out. If educhan cannot be reached to drop it, the tab says so on whatever
page it is on. The name is kept in memory for that one request and nowhere else,
and the drop sets no mark and stops nothing in any tab, since it reaches no
other claim. Every checkout's answer puts its cookie in the place of the one
before, whichever checkout started first, so the browser's checkouts go one at a
time: while one is on its way its tab keeps a mark in `localStorage`
(`educhan:pass-checkout:` and a random token, holding the time it was set), and
a checkout waits to go until no other's mark is left, in any tab, each for 30
seconds at most from the time it holds. A tab that leaves educhan, for another
site say, calls off a checkout not yet answered and takes its mark off. A
checkout whose answer reaches its tab in the moment before that tab hears of a
forget pressed in another is followed to Stripe, as one answered before it, and
keeps its claim unless its cookie was already in the browser when that forget's
request went. Every other checkout answered after a forget pressed since its
request went keeps no claim past it, and a checkout pressed after the forget
holds the cookie and is never dropped for it.

An order nobody paid for goes at its deadline, or as soon as Stripe reports
its checkout expired or its payment failed, or reports it paid with a payment
already refunded. A checkout completed with a payment that takes days to
clear, a bank debit say, is held for 30 days from when Stripe's report of it
arrives, to the next midnight UTC. The claim cookie lasts that long when the
report arrives as the checkout completes; one Stripe delivers late, on a
retry, can hold the order past the cookie, by at most as long as it was late,
rounded up to whole days, and its buyer then collects it with a reissue code.
A payment confirmed after its order's deadline still counts while the unpaid
order is kept, which is only until the next prune: a checkout, the daily
sweep, or a claim from its own browser. After that the "paid" event matches
nothing, and the log warns that a paid Stripe checkout matched no open Pass
order, without saying which. The same warning comes for a paid checkout of
anything else sold on the same Stripe account, so the owner checks the payment
against the receipt in Stripe before refunding it. A "paid" event delivered
again after its Pass was collected, or for a payment already refunded, matches
no order either and is not logged. A paid order nobody collected is kept for
its own term past its deadline, by when a Pass bought with it would have run
out, for the owner to reissue. Checkout and the daily sweep (`educhan -sweep`)
prune what is past all that, along with reissue codes past their deadline,
refunds remembered for their month, and Passes a month past expiry, which can
then no longer be renewed and are bought anew. The sweep needs no Pass setting
to do it, since every order carries its own term. A renewal whose Pass was
refunded or ran past that month while Stripe was opening the checkout is
refused with `409`, and the checkout is left to expire unpaid.

**Pausing and retiring.** `EDUCHAN_PASS_SALES=paused` stops selling and
honours what was sold. Checkout refuses a purchase and a renewal alike with
`403` and `the Pass is not on sale right now`, before it reads the request's
body or asks Stripe for anything; `GET /api/pass` says `"paused": true` and
asks Stripe for no price; and the Pass page says "The Pass is not on sale
right now." where Buy and Renew were, and offers no Start again. Everything
else goes on: Stripe's webhooks are checked and recorded, so a checkout opened
before the pause and paid after it is collected as usual, and a refund or a
dispute takes back what it paid for; Passes skip the captcha; the page
collects, shows a code, checks one typed in and drops a claim; and reissue
codes are made and redeemed. Setting it back to `open`, the default, sells
again; any other value refuses to start.

Retiring the Pass is a different thing: unset `EDUCHAN_PASS_SECRET` and every
other Pass setting. Every Pass endpoint then answers as if there were no Pass,
and a Pass sent on a post is ignored, so existing Passes stop skipping the
captcha. Stripe's webhooks answer `404`, and Stripe retries an event for a
while and then gives up, so an event sent meanwhile is lost unless the
settings are back before then: a payment among them never becomes a Pass, and
a refund or a dispute takes nothing back. So pause first, and retire only once
nothing is in flight. The tables stay, and the sweep goes on pruning them;
setting the same secret again brings back every Pass not yet pruned.

**What is kept, and what can be worked out.** An operator with the database,
the server secret and the Stripe account can see:

- in `passes`: that a Pass exists and the day it runs out (`expires_at`),
  with its code only as `HMAC(EDUCHAN_PASS_SECRET, code)`;
- in `pass_payments`: which Stripe payments bought or renewed it, as
  `HMAC(EDUCHAN_PASS_SECRET, payment id)`, with the term each one bought in
  days, the day that term ends, and which one is its origin. With Stripe's
  records beside it, that ties a Pass to whoever paid. This is what lets a
  refund or a chargeback take back what it paid for, and it is the sharpest
  edge of the design;
- in `pass_orders`, while an order waits to be paid or collected: HMACs of its
  Stripe session and payment ids, a hash of the browser's claim, the Pass it
  renews, the term it buys, and its deadline;
- in `pass_revocations`, for a month after a refund or a dispute: the
  payment's HMAC, so that a late "paid" event cannot undo it. Stripe sends
  these for any payment on the account, so a refund of something else the
  account sells is remembered the same way;
- in `pass_reissues`, while a reissue code is unredeemed: its SHA-256 hash,
  the HMAC of the payment it reissues, and its deadline.

There is no `created_at` anywhere, and every date in these tables is rounded
up to a UTC midnight, so it dates a purchase, a refund or a reissue to the day
and not the minute.

They cannot link a Pass to a post. Posts carry nothing about a Pass, using one
writes nothing (no "last used"), the code is stored only as a keyed hash, the
Pass tables reference only each other, and nothing about a Pass goes into a
poster ID. A Pass presented on a post that is no good, because it is unknown,
refunded, replaced or has run out, is treated as no Pass at all: the post is
held to the captcha like any other, so nothing in the request log tells it
apart, and its `201` says `"pass": "invalid"` or `"expired"` in the body,
which no log records, so the page can forget the code or offer to renew it.
Nothing identifying a Pass is logged. The log says that a Pass was issued,
renewed or reissued, that a reissue code was made, that a refund or a dispute
took back an order, a payment's term or a whole Pass, or that a paid Stripe
checkout matched no open Pass order, and never which; a failure to read a Pass
on a post is logged without the post's path, which names its board and thread
(the request's own line keeps the path and its `500`, as for any failed
request). `TestPassIsNeverStoredOrLogged` posts with a Pass on every posting
route, a thread and a reply each as JSON and with an image, and a reply by
thread id, and then searches every table and the log for the code, its hashes,
the budget key, the collection and reissue nonces, the payment and session
ids, a reissue code and the buyer's email and name, with positive controls;
`TestPassTablesCannotBeJoinedToPosts` pins the columns. The webhook handler
reads a handful of fields and never logs the body, which carries the payer's
email, name and address: `TestPassLogsAreBare` delivers every event the Pass
takes, with the payer's details in it, and one whose signature does not
verify, and holds every Pass log line to its message alone, with no field
beside it.

Apart from Stripe's webhook, which its signature guards instead, the Pass's
`POST` endpoints take only a body declared as JSON, which no form can send and
no other origin's script can send without a CORS preflight that is never
answered, and refuse a request the browser marks as coming from another site
(`Sec-Fetch-Site: cross-site`).

**Renewals and refunds.** Renewing from a browser that holds the Pass keeps
the code and adds the order's term to it: exactly that many days to a Pass
that hasn't run out, whose expiry is on a midnight already, and that many days
from now, rounded up to the next midnight UTC, to one that has. Each payment
records the term it bought, as the days it moved the expiry on rounded up to
whole days, and the day that term ends. A full refund or a dispute (a partial
refund changes nothing) takes off only the part of that payment's term still
to come, and never more than the term. With no term paid after it, that is the
days from now to the day it ends, rounded up, so that it stops at once rather
than at midnight. With one, it is those days rounded down, and the terms paid
after it move back by as much, since the Pass now reaches them sooner: the
refunded term runs on to the next midnight UTC, and the term after it keeps
every day it was paid for. A Pass goes on while a payment left on it has time
to come, and is cancelled once none has. So refunding a renewal takes its term
off and leaves the rest; refunding the purchase while its term runs, with
renewals after it, ends that term at the next midnight and brings the renewals
forward, whole; refunding a purchase whose term has run takes nothing off; and
somebody who renews another person's Pass and disputes the payment takes back
only what they gave. Refunding the only payment a Pass has cancels it at once,
as does refunding any payment of a Pass that has run out and is waiting in its
month of grace: neither has any time left to come. Any dispute counts, an
inquiry included, from the moment Stripe reports it opened
(`charge.dispute.created`): nothing reads a dispute's status or how it closed,
so winning one does not bring the term back, and since the payment is
remembered as taken back, a reissue cannot either.

Every change to the Pass tables (an order, a payment, a collection, a
reissue, a refund, a forgotten claim, pruning) takes one Postgres advisory
lock first (`passLockID`), so events that arrive together are applied one
after another: a refund cannot miss a Pass being collected beside it, and a
"paid" cannot slip past a refund. Posting with a Pass only reads, and takes no
lock.

**Lost codes.** A buyer who lost their code writes to the support address on
Stripe's receipt. The owner finds the payment in Stripe and asks for a reissue
code with its id, which needs `EDUCHAN_MOD_KEY` set:

```bash
curl -X POST https://<site>/api/pass/reissue -H 'X-Mod-Key: …' \
  -H 'Content-Type: application/json' -d '{"payment_intent":"pi_…"}'
# -> {"code":"REISSUE-…","expires_at":"…"}
```

and emails the reissue code, from the support address, to the address the buyer
wrote in from. The owner never sees a Pass code. A reissue code is the word
REISSUE and then 28 letters and digits, from the same alphabet as a Pass code,
in groups of four, to read out or paste: 140 random bits, and never mistaken for
a Pass code, which has no U. The server takes it in either case, with or without
the word in front and with dashes, spaces or nothing between the groups, a
no-break space, another dash or a character nobody sees, which a paste out of
mail can bring, included, and after the word it reads an O as 0 and an I or L as
1, as in a Pass code, whose alphabet has none of the three. The page skips and
reads them the same way, and sends a code typed or pasted in, a Pass code or a
reissue code, as the server writes it: in capitals, with those letters as
digits, grouped with dashes. Typed on the Pass page, a reissue code gives the
Pass a new code, shown once, and the old one stops working. For a purchase or a
renewal paid and never collected, it collects it instead: a purchase's reissue
code makes the Pass, with its code shown once, and a renewal's renews the Pass
it was for and shows no code, or makes a new Pass if that one has gone. It works
once, until a deadline like an order's (a week, to the next midnight UTC); a
newer reissue code for the same payment replaces it, and a refund or a dispute
cancels it, as does a renewal that takes its payment's right to reissue away
(below). It is never in an address, so no browser history, address bar or
`Referer` holds it. The page sends it in the body of one claim, with a nonce of
its own, and the new Pass code is derived from the two, so a reissue whose
answer was lost is asked again and gives the same Pass code; the server keeps
only the reissue code's SHA-256 hash. The page keeps that nonce until an answer
says the code issued or renewed a Pass, or until Remove the Pass or Forget this
browser, and never drops it on a refusal: an earlier try may have reached the
server and its answer been lost, so the right code typed after a mistyped one
still gets the same Pass code. A renewal's reissue code asked again is refused
like any spent one, since a renewal hands over no code; the renewal is on the
Pass all the same. So once a try has gone unanswered, which the page notes in
`localStorage` (`educhan:pass-reissue-unanswered`) until the code issues or
renews a Pass, or until Remove the Pass or Forget this browser, a refusal is
followed by a check of the code the page holds, as for a claim, and the page
says its new date. A Pass held here that the check finds no longer valid is what
an earlier try that gave it a new code would leave, so the page removes it and
says that the right reissue code, entered again, gets that code. With no Pass to
check, or no later date, it says only what is known: an earlier try may have
reached educhan and renewed the Pass already, and otherwise the code may be
mistyped, used, run out or cancelled. With no try unanswered before it, a
refusal only asks the reader to check the code.

Only the payment that made a Pass, or a paid purchase or renewal nobody
collected, can be reissued. The payment of a renewal that was collected
answers `409`, because otherwise somebody who saw a code once could renew it
and have the owner reissue it to them. A renewal nobody collected, such as a
renewal checkout paid after another checkout started in its browser, is
reissued like a purchase: its reissue code renews the Pass it was for and
hands over no code.

When the purchase is refunded or disputed and the Pass goes on, the right to
reissue it moves to another payment only while that payment pays for all the
time the Pass has left. If exactly one payment left on it has a term still
running or to come, any others having ended and no paid renewal of the Pass
waiting to be collected, the right moves to that one, and the `409` for
another renewal's payment says to use it; its payer can then have the Pass
reissued without taking a day anybody else paid for on it. Otherwise the Pass
cannot be reissued, and a reissue for any of its payments answers `409` saying
so. Two terms still to come can be two payers', a holder's own renewal beside
one somebody else gave who saw the code once, and the server cannot tell one
payer from another, so handing the whole Pass to either could give them time
the other paid for. Such a Pass goes on working for whoever holds its code,
and a later refund or dispute, of one of its payments or of a paid renewal
not collected yet, that leaves one payment paying for all its time moves the
right to that payment then; a term that simply runs out moves nothing.

For the same reason the right that moved goes again as soon as the Pass is
renewed once more, by its holder or anybody else, since that payment then pays
for only part of its time, and a reissue code made for it is cancelled with it;
a renewal only ever takes the right away. While a renewal of the Pass is under
way, from its checkout until it is collected, the payment the right moved to
gets no reissue code (the same `409`), and one made before is refused with
`409` and not spent, since the renewal, paid for by somebody, would land on the
Pass handed over. A checkout whose code was given to somebody else by a reissue
while Stripe was opening it is refused with `409` instead, as a renewal of a
Pass that is gone. If the renewal goes unpaid, or is refunded or disputed
before it is collected, the reissue code works again until its deadline. An
unpaid checkout goes unpaid when Stripe closes it, or once it is past its
deadline and pruned, by the daily sweep, the next checkout or a claim from its
own browser, since Stripe can still report it paid until then. The purchase
keeps its own right whatever renewal is under way.

**Setting it up.** In Stripe: a one-time Price for the Pass
(`EDUCHAN_STRIPE_PRICE_ID`), which with Stripe Tax on says whether tax is in its
amount (**Tax**, below); an API key (`EDUCHAN_STRIPE_SECRET_KEY`), best a
restricted key (`rk_…`) allowed to write Checkout Sessions and read Prices and
nothing more; and a webhook endpoint at
`EDUCHAN_PUBLIC_URL/api/pass/stripe-webhook`, created with API version
`2024-06-20` (`stripeAPIVersion` in `pass_stripe.go`) and sending the six
events listed in the [HTTP API](#http-api) table, whose signing secret is
`EDUCHAN_STRIPE_WEBHOOK_SECRET`. Turn on Stripe's email receipts and give the
account a public support email: the receipt is how a buyer finds their
payment, and whom to write to about a lost code. Buyers are sent only to
`checkout.stripe.com`, so a custom checkout domain set up in Stripe would make
every checkout fail. Then set `EDUCHAN_PASS_SECRET` beside them, and
`EDUCHAN_MOD_KEY` if it is not set already: a lost code is reissued behind the
moderation key, and without it every reissue answers `403`, which the server
warns about as it starts. Every Pass setting given without the secret refuses
to start rather than leaving the Pass quietly off, and
`EDUCHAN_PASS_SALES=paused` keeps it off sale while everything else is tried.

**Tax.** With `EDUCHAN_STRIPE_AUTOMATIC_TAX` on, checkout asks Stripe Tax to
collect sales tax, VAT or GST, and Stripe's page asks the buyer for a billing
address to work it out. The Price then has to say whether its amount includes
the tax. `inclusive` takes the tax out of the amount, so every buyer pays the
price shown, which the Pass page calls "tax included"; `exclusive` adds it at
checkout, where it can come to nothing for some buyers, so the page says "plus
any tax". A Price that says neither (`unspecified`, or nothing at all on an
older Price) would leave it to a default in the Stripe account that the page
cannot know, so while Stripe Tax is on it is not sold: `GET /api/pass` leaves
the price out, checkout answers `503` ("the Pass can't be bought right now"),
and the log says what to fix. The same holds, while Stripe Tax is on, for any
other Price the Pass can't be sold at: one archived, recurring or without a
fixed amount. Asking again mends none of these. Nor is the Pass sold while
Stripe can't be asked for the Price, which might be such a one: checkout answers
`502` ("try again in a moment") until the Price is read. Neither refusal starts
a cooldown. A failed read and a refused Price are each kept a minute before
Stripe is asked again, and a Price the Pass is sold at is kept an hour
(`TestPassPriceIsKeptForItsTime`). Stripe never changes a Price's amount or
currency, so a new price takes a new Price, a new `EDUCHAN_STRIPE_PRICE_ID` and
a restart. A Price that says neither can be given a tax behavior once, but
Stripe never changes one that says `inclusive` or `exclusive`, so changing that
takes a new Price too. With the setting off, checkout tells Stripe to collect no
tax (`automatic_tax[enabled]=false`), which an account set to collect it by
default would otherwise do, and the page says nothing about tax.

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
| educhan Pass | Optional, off unless configured: a term of posting without the captcha, up to an hourly budget, bought on Stripe's hosted checkout after an unticked 18+ box; the server hands the code over once and keeps only its HMAC, and nothing about a Pass is on posts, in logs or in poster IDs |
| Moderation | Sticky, lock, and delete a post or a whole thread, plus creating and reconfiguring boards, behind a shared moderation key |
| Archive | Threads pushed past a board's thread limit are archived rather than deleted: they leave the index and the catalog, refuse replies, and still load by number |
| Housekeeping | `educhan -sweep` deletes archived threads, media included, once their board's `archive_days` have passed, and prunes the Pass tables; per-client cooldowns limit new threads, replies, uploads, verification code requests, Pass checkouts and Pass code checks, and forget each address, as the Pass's hourly budget forgets its count, within a minute of its cooldown or hour ending while the process has CPU between requests (see Flood control for an idle Cloud Run instance) |

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
`pass`, `mod`, `u`, `rules`, `faq`, `legal`, `terms`, `privacy`, `dmca`, `report`,
`archive`, `safety`, and the server's own `api`, `media`, `healthz`, and `lib` — are never
read as boards.
`/verify`, `/pass`, `/rules`, and `/faq` are pages; a word without a page yet shows the
not-found view.

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
  `/verify` drops the key, the name, any badge and any Pass, any Pass purchase still
  being collected and any note that a Pass was found gone, for a shared computer. The
  browser's other tabs follow it for the Pass and what goes with it (the purchase being
  collected, the code box and that note, though a code copied in one of them stays on
  the clipboard), but a tab already open can keep the key, the name and the badge it
  has, and a post from it puts the key and the name back in storage, so the Verify page
  and the FAQ say to close those tabs first.
- **educhan Pass**: the header links to `/pass` only once `GET /api/site` says the server
  has the Pass, on sale or paused, or while this browser holds one, so a holder can still
  show or remove it. `/pass` says what a Pass does and what it costs (with "tax included"
  or "plus any tax" when `GET /api/pass` says so, nothing about tax while Stripe Tax
  is off, and "a set time" until the server's terms are known), sells and renews it
  through Stripe's own page, and takes a code typed in: a Pass code, or a reissue code
  (see [Lost codes](#educhan-pass)). While sales are paused it says "The Pass is not
  on sale right now." where Buy and Renew were. A browser that keeps no cookies or
  storage is told so rather than sent to Stripe. The browser keeps the Pass in
  `localStorage` and sends it on unbadged posts only (see [educhan Pass](#educhan-pass));
  the page sends it only in the body of a renewal or a check. When `GET /api/pass` cannot
  be read, a Pass held here is still drawn, with Show the code and Remove but no Renew,
  and the page is asked for again once educhan answers a code typed in or checked there.
  While a purchase waits to be collected (`GET /api/pass` says `claim`), the page shows
  that in place of any form to pay with, or, when the browser already has a Pass, says a
  checkout from it has not been collected yet, which may be a payment Stripe is still
  confirming, and keeps Renew; either way it says that starting another checkout, a
  renewal included, stops this browser collecting that one, and the forms drawn again over
  a checkout left behind (Start again, back from Stripe's cancel link, or a checkout whose
  answer came after the reader left the page or after the Pass held changed meanwhile, in
  another tab say, which is not followed to Stripe) still offer **Stop collecting it
  here**. It asks on every visit, with a growing pause while it is open, and sends the
  collection's nonce (`educhan:pass-nonce`) each time until an answer says nothing more is
  waiting, so an answer lost on the way is asked for again and brings the same code;
  refused then, it checks the Pass it holds and says a date that moved on. A code the
  server hands over is stored before anything else and stays on screen, beside any code it
  took the place of, until the reader leaves the page; it can be shown again from the held
  Pass's panel to write down, and a browser that will not store it is told so. A code
  typed in after it ran out, within its month of grace, is kept as run out, with its date,
  to renew. A Pass that has run out here can be checked again, in case it was renewed
  elsewhere. A renewal the page collects is checked against the Pass it holds
  (`/api/pass/activate`) before it says that Pass is renewed; one it cannot match to the
  Pass it holds is reported as a renewal of a Pass, by a payment from this browser or by
  the reissue code that brought it, with that Pass's new date. An answer about a Pass, to
  a check or a post, that arrives after the Pass left the browser (removed or replaced,
  here or in another tab), or after Remove the Pass or Forget this browser was pressed, is
  dropped, not stored back. An answer to anything the Pass page asked is drawn, focused or
  announced only while the reader is still on the Pass page; one that arrives after they
  left stores what is still right to store and draws nothing. Brought back from the
  back-forward cache, the Pass page is drawn again from the start, age screen first. A
  code typed or pasted in is sent as the server writes it, in capitals, with an O, I or L
  as the digit the server reads it as, and grouped with dashes, whatever separators it was
  pasted with. A reissue code is typed or pasted into the code box, never carried in an
  address. Pressing the button spends it at once, even in a browser that holds a Pass,
  whose code is then shown beside the new one to write down; a Pass code typed over a good
  Pass is refused instead. It goes in the body of one claim, with a nonce of its own
  (`educhan:pass-reissue-nonce`), and is never stored: it stays in the box, in memory
  only, until an answer arrives, so pressing again after a lost answer asks with the same
  nonce. The nonce stays until an answer says the code issued or renewed a Pass, never
  going on a refusal, so the right code typed after a mistyped one still gets back a Pass
  whose answer was lost. A try that went unanswered is noted until then too
  (`educhan:pass-reissue-unanswered`), and only after one is a refusal worded for an
  earlier try that may have landed, with the Pass held here checked; a Pass the check
  finds no longer valid is removed, with word that the right reissue code, entered again,
  gets its new code. The box has autocomplete off, and is emptied by **Remove the Pass**
  and **Forget this browser**, pressed in any tab of the browser, and by leaving the page.
  Those two and **Stop collecting it here** stop any collection under way, drop its nonce
  and then the browser's claim, and drop unstored an answer to a claim that was already on
  its way, in every tab of the browser (the `storage` event), a tab brought back from the
  back-forward cache included, so the next person at a shared computer collects nothing. A
  checkout still opening when one of them is pressed, in any tab, is called off, or not
  followed to Stripe once it answers, and keeps no claim past it, but for the moment
  [educhan Pass](#educhan-pass) describes; checkouts go one at a time, so one pressed
  after it holds the claim, and a tab that leaves educhan calls off a checkout still
  opening. Remove and Forget also drop the reissue code's nonce and its note, any note
  that a Pass was found gone, and an answer on its way to a reissue code or to a check of
  a code; Stop leaves those alone, since a reissue it dropped would be spent for nothing.
  Nothing collects again until the server has that request: **Check now** and **Stop
  collecting it here** stay off meanwhile, in every tab and in whatever is drawn
  meanwhile, a tab that opens the Pass page meanwhile draws it then, and a Stop that
  cannot reach educhan says so, turns the buttons back on and goes on collecting. **Use
  this code here** (a Pass code or a reissue code), **Check it again**, **Buy on Stripe**
  and **Renew on Stripe** pressed meanwhile wait, and go once every such request the tab
  knows of is settled, from this tab or another; but if Remove or Forget is pressed while
  they wait, in this tab or another, they send nothing, give the button back and say why,
  and only a press made after the Remove goes. They do the same, saying that the browser
  stopped collecting a Pass first, when Stop is pressed meanwhile in another tab, whose
  Stop is on only once that tab has given up waiting on the request. Remove and Forget
  also empty the clipboard, whatever is on it by then, when the code was copied with
  **Copy the code** in the same tab since educhan was last opened there and no copy or cut
  in that tab has certainly put something there since, unless another tab has removed or
  replaced the Pass meanwhile. A copy counts when what is selected holds something (a text
  box's own selection where the box says, the document's for an email or number box, and
  otherwise text the page itself has selected), and a cut only that way and in a box that
  can be typed in, or in editable text; one the page cannot be sure of, such as a copy
  from an email box in some browsers, does not count, so the clipboard is emptied after it
  too. A code copied any other way, by hand included, stays until something else is
  copied. Removing the Pass, or forgetting the browser, in another tab takes a code just
  handed over off this tab's screen. A post whose Pass was no good goes out like any
  other, and its answer says so: the page forgets an unknown Pass, keeps one that ran out
  to renew, and says the post was sent. - **Phones**: a single fluid column with the
  catalog at two tiles across, thumbnails that
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
| `EDUCHAN_MOD_KEY` | empty | Shared secret for the moderation and board management endpoints, and for reissuing lost Pass codes; unset means they refuse every request |
| `EDUCHAN_VERIFY_SECRET` | empty | Keys institutional verification: the address hash, the pending code hash, and the credential signature. **Unset switches the whole feature off**; rotating it revokes every credential |
| `EDUCHAN_VERIFY_CODE_TTL` | `15m` | How long an emailed code stays usable |
| `EDUCHAN_VERIFY_CREDENTIAL_TTL` | `4320h` (180 days) | How long a badge lasts before the holder verifies again |
| `EDUCHAN_VERIFY_COOLDOWN` | `60` (seconds) | Minimum interval between code requests, per client and per address |
| `EDUCHAN_VERIFY_DOMAINS` | embedded list | Path to an allowlist file replacing the built-in one |
| `EDUCHAN_PASS_SECRET` | empty | Keys the hashes the educhan Pass stores of its codes and of Stripe's payment and session ids (a claim cookie and a reissue code are random, and stored as plain SHA-256), and signs the name a checkout's answer gives its order. **Unset switches the Pass off**, and every other Pass setting below is then refused at startup; at least 32 bytes (`openssl rand -hex 32` makes 64). Rotating it cancels every Pass and orphans every payment record |
| `EDUCHAN_STRIPE_SECRET_KEY` | empty | Stripe API key checkouts are created with. Required with `EDUCHAN_PASS_SECRET` |
| `EDUCHAN_STRIPE_WEBHOOK_SECRET` | empty | Signing secret (`whsec_…`) of the Stripe webhook endpoint at `/api/pass/stripe-webhook`. Required with `EDUCHAN_PASS_SECRET` |
| `EDUCHAN_STRIPE_PRICE_ID` | empty | The one-time Price (`price_…`) a Pass sells for. Required with `EDUCHAN_PASS_SECRET` |
| `EDUCHAN_STRIPE_AUTOMATIC_TAX` | off | Have Stripe Tax collect sales tax, VAT or GST at checkout. Switch Stripe Tax on in the account first, and give the Price a tax behavior: `inclusive` takes the tax out of its amount, and the Pass page says "tax included"; `exclusive` adds it at checkout, and the page says "plus any tax". A Price with neither is not sold while this is on (see [Tax](#educhan-pass)). Off, checkout tells Stripe to collect no tax, even where the account collects it by default, and the page says nothing about tax |
| `EDUCHAN_PUBLIC_URL` | empty | The site's origin (`https://…`), where Stripe sends a buyer back. Required with `EDUCHAN_PASS_SECRET` |
| `EDUCHAN_PASS_DAYS` | `365` | How long a Pass lasts, from 1 to 36500 days. Each order keeps the term it was sold with, so a change applies to checkouts started after it and to nothing already sold |
| `EDUCHAN_PASS_HOURLY_POSTS` | `30` | A Pass's hourly budget: how many posts in one hour, counted in UTC, skip the captcha. At least 1 |
| `EDUCHAN_PASS_COOLDOWN` | `30` (seconds) | Minimum interval between checkouts from one client |
| `EDUCHAN_PASS_SALES` | `open` | `open` sells the Pass; `paused` stops selling it and honours what was sold (checkout refuses, everything else goes on); anything else refuses to start. See [Pausing and retiring](#educhan-pass) |
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
post (or code request, Pass checkout or Pass code check, for the cooldowns of
verification and the Pass), whether or not anyone else posts in the meantime,
**as long as the process has CPU between requests**. A host that gives it CPU
only while it answers a request, which is Cloud Run's default (request-based
billing), holds the sweep's timer back while an instance is idle. The first
request to arrive after a sweep fell due runs it before it is handled, but
until one arrives, or the instance is shut down, an idle instance's memory
still holds the addresses it last saw. On Cloud Run the one-minute bound needs
CPU allocated for the instance's whole life, which is also how it is then
billed: `run.googleapis.com/cpu-throttling: "false"` on the revision, or
`--no-cpu-throttling` on `gcloud run deploy`.

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
| `GET` | `/api/site` | Posting settings — `{"uploads","max_upload_bytes","pass"}`: whether posts may carry an image, the largest one taken, and whether the server has the Pass, on sale or paused. The same answer for every caller; it reads nothing from the request |
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
| `GET` | `/api/pass` | Whether the Pass is on sale — `{"enabled","days","hourly_posts","price":{"amount","currency"},"tax","paused","claim"}`, or `{"enabled":false}` without it. `"tax"` is there only beside the price and with `EDUCHAN_STRIPE_AUTOMATIC_TAX` on: `"included"` when the Price has the tax in its amount (`tax_behavior` `inclusive`), `"added"` when Stripe adds it at checkout (`exclusive`); `price` is left out when Stripe can't be asked for the Price or answers with one the Pass can't be sold at (archived, recurring, without a fixed amount, or, with Stripe Tax on, neither `inclusive` nor `exclusive`), until Stripe is asked again a minute later, and while sales are paused; `"paused":true` only while they are; and `"claim":true` only when a claim cookie the browser sends has something under it to collect |
| `POST` | `/api/pass/checkout` | Start a purchase — `{"adult","renew"}`; `"adult"` must be `true`, `"renew"` is an existing code to extend. Returns `{"url","order"}`, a page on `checkout.stripe.com` and the order's name, a hash of the claim that collects nothing, signed with `EDUCHAN_PASS_SECRET`, and sets a new claim cookie with only this order under it; `400` without `"adult": true`; `403 {"pass":"invalid"}` when the code to renew is not a Pass code, is unknown or is past its month of grace, before any cooldown starts; `403 {"error":"the Pass is not on sale right now"}` while sales are paused; `503 {"error":"the Pass can't be bought right now"}` while Stripe Tax is on and the Price is archived, recurring, without a fixed amount, or neither `inclusive` nor `exclusive`, and `502` while Stripe Tax is on and the Price can't be read, each before any cooldown starts or anything is opened in Stripe; `429` with `Retry-After` inside `EDUCHAN_PASS_COOLDOWN`; `502` when Stripe fails to open the session, after the cooldown has started, so asking again at once gets `429`; `409 {"pass":"invalid"}` when the Pass to renew was refunded, lapsed or given another code by a reissue meanwhile |
| `POST` | `/api/pass/claim` | Collect the order under the claim cookie once it is paid, one order an answer under each claim cookie the browser sends — `{"nonce"}`, 16 random bytes in base64url that the page draws for each collection and sends with every claim until an answer says nothing more is waiting (`"claim":false`); the code is derived from the cookie and the nonce together. Answers `{"status":"issued","pass","expires_at","claim"}` when a Pass was made, and the same code again when asked again with the same cookie and nonce; else `{"status":"renewed","expires_at","claim"}`, else `202 {"status":"pending","claim":true}` while the order waits for Stripe; `400` when nothing is left, which is also the answer to a renewal's claim asked again, since a renewal hands over no code. `"claim"` says whether anything still waits. With `{"reissue":"…","nonce"}`, a reissue code (`REISSUE-…`, in either case, with or without the word and the dashes), it redeems the reissue code instead, answering the same way, with the same Pass code when asked again with the same nonce, or `403 {"pass":"invalid"}` for one used, run out or cancelled, a renewal's asked again included, or `409` for one whose right moved, while its Pass is being renewed, which leaves it unspent. With `{"forget":true}` it drops what is under every claim cookie the browser sends and clears the one the server set — `{"status":"forgotten","claim":false}`; with `{"forget":true,"order"}`, the name a checkout's answer gave its order, it drops what is under that checkout's claim alone, reading no cookie and clearing none — `{"status":"forgotten"}` — and answers `400` for a name not shaped as one or not signed by the server, `200` for one with nothing under it, and `400` for an `"order"` without `"forget"` |
| `POST` | `/api/pass/activate` | Check a code — `{"pass"}`; returns `{"pass","expires_at"}`. A code that has run out is refused with `403 {"pass":"expired","code","expires_at"}` until it is pruned a month on, and an unknown one with `403 {"pass":"invalid"}`. One check per address every 2 seconds, `429` otherwise |
| `POST` | `/api/pass/stripe-webhook` | Stripe's signed events: `checkout.session.completed`, `checkout.session.async_payment_succeeded`, `checkout.session.async_payment_failed`, `checkout.session.expired`, `charge.refunded`, `charge.dispute.created` |
| `POST` | `/api/pass/reissue` | Moderation key: a one-time reissue code that gives a lost Pass a new code — `{"payment_intent":"pi_…"}`; returns `{"code","expires_at"}`, the code being `REISSUE` and seven groups of four letters and digits, for the owner to send on; `404` for a payment that bought nothing or was refunded or disputed; `409` for a collected renewal's payment (use the purchase's, or, if that was refunded or disputed, the one renewal that then paid for all the time left, until the Pass is renewed again), and for any payment of a Pass that cannot be reissued, because its purchase was refunded or disputed and no one payment left pays for all its time, or because a renewal of the Pass is under way when the right has moved. A purchase or a renewal paid and never collected is reissued too, and its reissue code collects it: a renewal's renews the Pass it was for, with no code |
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

The two posting endpoints read a Pass from `X-Educhan-Pass`, on unbadged posts
only: a badged post's request is the credential's, and the Pass header is not
read on it. A Pass that is unknown, refunded or replaced, or has run out, is
treated as no Pass at all, so nothing about the answer's status or the request
log tells it apart from a post made without one; the `201` carries
`{"pass":"invalid"}` or `{"pass":"expired"}` beside the post, so the client can
drop the code or offer to renew it. A Pass header sent while the Pass is
switched off is ignored.

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

JSON request bodies must be `application/json`; unknown fields are rejected.
Stripe's webhook is the exception, read as Stripe sends it. The two posting
endpoints also accept `multipart/form-data` with the same fields plus an `image`
file. Errors are `{"error": "..."}`, and `429` responses add `retry_after` plus
a `Retry-After` header.

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
image public with nothing left to say it exists. Before any of that it prunes
the educhan Pass's tables of what has outlived its use (see
[Orders](#educhan-pass)), whether or not the Pass is on sale. It needs no Pass
setting for that: each order carries the term it was sold with, and every
other deadline is stored on its row.

A file the media store does not hold also keeps its key and fails the sweep.
Every queued key is a file the server stored, so a store without it is almost
always the wrong store: **the sweeper must run with the server's media
settings** — `EDUCHAN_MEDIA_BACKEND`, `EDUCHAN_GCS_BUCKET`, `EDUCHAN_GCS_PREFIX`,
`EDUCHAN_UPLOAD_DIR` — or it reports every file missing. Once the settings are
fixed, the next sweep removes them. A file that really is gone for good (lost
with a container's disk, or removed by hand) fails every sweep until its row is
deleted from `media_deletions` by hand.

It logs one line of counts (threads, posts, attachments, and files removed,
failed, and missing) and nothing about which boards or threads went, and exits
non-zero if the database fails or any file could not be removed or was missing;
the error it exits with says why without naming the file. It also refuses to
run on a database a newer release has migrated (see below). It needs none of
the web server's secrets — the poster-ID salt, the moderation key, the
verification secret — and, run without them, it does not warn about them:
those warnings come from the server alone.

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

Two things remain per instance:

- **Flood control.** `cooldownLimiter` keeps its state in process memory, so N
  instances enforce the cooldowns N times over. Moving it to shared storage means
  replacing that one type, which nothing else depends on.
- **The Pass's hourly budget.** `passBudget` counts in process memory too, so with
  N instances a Pass can skip the captcha up to N times its budget in an hour.

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
