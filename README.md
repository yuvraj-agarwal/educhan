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
institutional verification, content rules that send back posts with contact
details or ID numbers and hold possible threats, self-harm encouragement and
grooming cues for a moderator, and key-authenticated moderation. Janitor
accounts, bans, and reports are not built yet.

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

## Content rules

Every post is read by a small, offline filter (`internal/app/filter.go`) before
anything about it is stored: its body, its subject, and the part of its name
that is shown. The tripcode secret after `#` is never read. Direction controls
(the Unicode characters that make text show in a different order from the one
it is stored in) are taken out of every field first, and are never stored, so a
reader sees the text in the order the filter read it. Markup is read the
way the page shows it: `[math]` and `[eqn]` blocks are skipped, because TeX has
a use for every character; `[code]` is read as its own lines; and `[spoiler]`
tags are read as nothing, so a number, a username or an app name split across
them reads whole, as it does once the spoiler is opened. The text is first normalised —
compatibility forms folded (so full-width digits and styled letters read as
plain ones), lower case, accents, keycaps and zero-width characters dropped,
every script's digits read as 0–9, spaced or dotted-out letters closed up, and
leetspeak beside letters read as letters — and then checked against tables of
patterns with a few small validators. A subject and a body are read together
for threats, as a reader sees the title above the post, and how near a time or
a place is to a threat is counted in letters and digits, with any gap between
words counting once, so spaces, invisible characters or emoji between them do
not pull them apart. Each rule has one outcome:

| Outcome | Rule | What it looks for |
| --- | --- | --- |
| Block (`422`) | `contact` | Phone numbers (`+` and a country code, the `+` inside a bracket or not; Indian mobiles; US numbers written the US way; UK numbers starting 07, or 01/02/03 when spaced; Australian mobiles starting 04, and landlines starting 02, 03, 07 or 08 written 2-4-4 ("(07) 3221 3317"); South African and Irish mobiles, ten digits starting 06, 07 or 08 written 3-3-4 or 3-7 ("087 214 6639"), or run together straight after the name of a line ("my number is 0821234567"); Singapore mobiles, eight digits starting 8 or 9, after "call me", "whatsapp", "my hp" and the like ("whatsapp 9137 2684")), written digit by digit or in groups, one digit set apart among groups of three or more ("98450 1 2345") or among pairs after "call me", set off by a comma or by up to three spaces, line breaks, dashes, dots or underscores (pairs set off by dots or dashes may leave one digit over at an end: "07.88.42.61.39.0"), with `O`, `l` or `I` for a digit inside a group or standing alone between spaced digits ("07911 4OO 133", "8 l 2 O 3 3 …"); an Indian mobile in any grouping after "call me", "text me", "reach me" or "my number"; a US number's ten digits run together after the name of a line ("cell", "Google Voice:", "my number is"), or spaced 3-3-4 after "call", "text" or "hmu", or pointed at with an arrow ("1-793-502-8814 <- text me"), or its seven digits with the area code given apart ("482-0196 (area code 404)") or set off by a slash after the name of a line or an ask to call ("phone 495/418-3927"); a number given in three or more pieces, as small as one digit each or spaced out a digit at a time, joined by `/`, `|`, `,`, "then", "and", "plus", "followed by", "ends in", "aur" or "phir" ("starts with 76, then 1180, and ends in 5249"), or as quoted strings joined by `+` ("'+91' + '98450' + …"), when the pieces make a phone number and a "call me", "my number", "text dont call" or a word about getting it past the moderators or the filter ("so the mods dont catch") is beside them, unless a word for a list of values comes just before them ("questions 87, 88, 91, 92, 93 — hmu"), or in two pieces straight after "call me on" or "my number is" ("call me on 98450/12345"), or in two pieces each labelled as which part it is ("first 5: 93104, last 5: 72286"); a number said to be written backwards, read the right way round; a mobile read out a digit at a time in words, or in words and digits mixed ("nine eight 7 six …", "oh seven nine one one, double two three …", "nau aath saat …"); email addresses (not the RFC 2606 example domains, nor a domain named with its `@` after a word of the sentence: "signed up with my @iitb.ac.in address", "is a @yahoo.com address okay?", though "madeup12 @gmail.com" and "my email is riya @madeup.com" are addresses), also with a bracketed or spelled-out `@` and dot, a mail provider with no ending ("name@gmail"), spelled out a character at a time ("m a d e u p . 0 9 @ g m a i l . c o m"), or given in two halves, the name labelled beside the provider ("[at] gmail [dot] com — prefix is …"), and an address asked for ("give me your email", "drop ur email and I'll share the folder", not "send me your email draft"); invite and profile links for messaging and social apps, Signal's included, links to join a video call (Zoom, its web client's "/wc/join/" page included, Google Meet, Microsoft Teams, its "/meet/" links included, Webex, Jitsi) and LinkedIn profile links, or a profile's "/in/" path or LinkedIn's own shape of a profile's name after the word ("LinkedIn - /in/x-y", "linkedin: x-y-1a2b3c"), but not those services' own pages ("zoom.us/download", "meet.google.com/new", a company's LinkedIn page), also with "slash" spelled out and, on hosts too long to turn up inside other words, "dot" ("discord dot gg slash …"); a video call's meeting ID after its name, with or without a label, which is as good as the link ("zoom id 845 2219 6630", "join zoom 845 2219 6630", "webex: 2634 123 4567", "meeting ID: 845-2219-6630", Webex's own "Meeting number (access code): 2634 123 4567", "meet code: qvx-trmk-hpd", "gmeet qvx-trmk-hpd"), while the words alone are fine ("zoom class at 9", "join the teams meeting", "meet me at the library"); and asking to talk on one, read from the app's name outwards rather than from a list of phrases: a verb of moving, talking or following with its object and preposition, said as an ask ("come to tg", "follow me back on insta pls", "can we switch to signal?", "let's move this to discord", "can we move the study group to telegram?", "holla at me on IG", "abeg chat me up on WhatsApp", "facetime me later", "snap me", "send me a dm on insta", "can we video call on whatsapp", "lets facetime", and after a condition, a relative or whom it asks, "if ur bored kik me", "anyone who wants the notes whatsapp me", "lonely girls whatsapp me", not "my mum will whatsapp me tonight", "she keeps trying to kik me" or "snap me out of it"), a question about the reader's account ("u got insta?", "what's your insta", "exchange snaps?", "do u have telegram", "is anyone here on discord", but a game only with its question mark: "r u on minecraft rn" is about playing), messages there ("slide into my dms on insta", "lets take this to dms on insta", "dms open on discord"), a streak partner looked for there ("who wants to be my snap streak buddy"), a group or a call set up there, the app before the group or after it ("can someone make a discord vc", "make a gc on snap", "creating a whatsapp group for neet aspirants", "made a discord server for jee 2027, join", "can someone add me to the whatsapp group", not "my mom made a whatsapp group" or "creating a discord server is easy"), the Hinglish either way round ("insta pe aaja", "whatsapp pe aa jaldi", "dm kar insta pe", "telegram pe join karo", "mujhe whatsapp karo", "WhatsApp number do", "apna whatsapp number comment karo", "apna gmail id bata", "number bhejo"), a handle a few words after the app ("my discord is x_y_z", "snap: x_y_z", "snap pe aaja: madeup.0p", "telegram: madeup underscore jee", "disc: name#1234", "line id: x_y", "riot id: name#IND1", "follow me on X @x_y", "👻 = x_y", "my roblox user is x_y", "insta 👉 x.y", "snap ~ x_y", "my reddit is u/x_y", "bluesky: @x.bsky.social", and Threads only with the "@", "my threads is @x.y", since a thread is also a conversation here), an account written as a number (a game's friend code, "steam friend code 4410877305"; a Session ID, 66 hexadecimal digits starting 05), a handle that is another field of the post when that field is filled in ("my snap is my name here"), talking "there" once an app or a link to one is named ("reddit u/x_y, pm me there"), and talking off the board with no app named, with the poster or the reader ("anyone want to text instead of posting here?", "dm me instead of posting here", "can u text me instead?", "is there somewhere more private we could talk?", "can we talk privately?", "lets talk in private", "can we talk 1 on 1?", "lets talk somewhere else", "lets go private"; talking privately with someone else ("talk privately with my parents"), elsewhere on the board ("can we talk elsewhere? this thread is for physics") and "lets go private colleges" are fine, as are telling someone to call a helpline, a counsellor or their parents instead and "dm me" with nowhere named, which names no way to reach anyone); also with a letter of the app's name in brackets ("i[n]sta"), and "leave your number", and everyone's numbers asked for with a way to reach them named, in the ask or round it ("drop your phone numbers", "anyone wanna make a whatsapp group? drop your numbers", "drop ur numbers i'll add you"; after results the way to reach them has to be named, and scores added to a sheet, "a close call", "the text section" or a study group are not one), or where the ask ends there in a post that says nothing about results ("drop your numbers below"; not "results are out! drop your numbers below", "what did everyone get? drop your numbers", "post your numbers from the mock" or "drop your numbers in the poll"). Not read: a LinkedIn name alone ("my linkedin: x-y", "add me on linkedin x y", which have the shape of "linkedin: self-promotion"), a lnkd.in link (as often to a post or a job as to a person), and asks to connect on LinkedIn, which name no one. An app named on its own or as a topic ("my snap score", "go to tg for counselling updates", "switch to whatsapp web"), an ask quoted from someone else (a greentext line quoting the post being answered, "my mum said 'add me on whatsapp'"), an ask named after it as what someone else says or as a kind of thing to ask ("send me a dm on insta is what every spam bot says", "can we facetime? is a weird thing to ask a stranger", not "… is what I always say" or "… is a weird thing to ask but pls do"), and an ask taken back for a place on the board ("snap me a pic of the homework page please? jk, just post it here", "can we talk privately? i mean in a separate thread here", not "i mean not here", "i mean this thread is dead", "add me on snap nvm this thread is dead" or "whatsapp me actually, i'm new here"), are fine; a contact detail is blocked however it is named. Contact details in a post that also asks the reader's age, year, school or where they live are not sent back but held as `grooming` (below) |
| Block (`422`) | `id_number` | Aadhaar numbers (twelve digits, first 2–9, in one block, in fours or a digit at a time, with a valid Verhoeff check digit, or with any check digit after the word "Aadhaar": "my aadhaar no …"), US Social Security numbers written with dashes, or with spaces after "SSN" or "social security number", UK National Insurance numbers (in capitals, or in any case after "national insurance" or "NI number") |
| Support (`422`) | — | Crisis language in the poster's own voice, in any tense and with or without the "I": wanting, wishing or needing to die, not to exist, not to wake up, or to hurt oneself ("I want to die", "i used to want to die and now i do again", "i wish i could just disappear and never wake up"); feeling like it or thinking about it ("i feel like ending it all", "thinking of ending my life tonight"); hurting oneself ("hurt myself last night", "I'm self harming again", "I've been cutting myself again"); no reason left ("I don't see any reason to keep living", "what's the point of going on"); a world without the poster ("nobody would care if I died", "everyone would be happier if I just wasn't alive anymore"); a way out ("the easiest way out is to just not be alive anymore"); putting pills aside for it ("I've been saving up pills", "saving my pills for when I'm ready"); "I'm so tired of being alive", "kms", "topping myself", "I want to go to sleep and not wake up"; a way of dying named ("I want to hang myself", "gonna overdose on my meds", "i'm going to take all my pills tonight", "I need to cut", "the urge to cut is so strong tonight"; not "hang out", "hang my coat", hanging from a pull-up bar or upside down on the monkey bars, "cut my hair", "cut down on sugar", "drown myself in coffee", "shoot myself a reminder", "stab myself with the compass by accident" or all the pills a doctor said to take); hoping or longing to be dead ("I hope I die in my sleep", "I hope I don't wake up tomorrow", "can't wait to die", "I'd rather be dead", "praying for death", "life isn't worth living"; not "rather be dead than wear that", "dead last", "dead tired" or "dead broke", "can't wait to die my hair blue", someone else's life, a bus wished for to skip an exam with a laugh, or sleep forever after the last exam); an overdose just taken ("just took 20 paracetamol", "I swallowed a whole strip of pills"; not two for a headache or pills as the doctor said); "I'm ending it tonight" and "gonna end it soon" (not an essay, a series or a boyfriend, nor ending it with a revision); a suicide note written (not one analysed in class) and "by the time you read this I'll be gone" (not to Goa); a relapse ("I relapsed on self harm last night", "3 months clean from sh and relapsed"; not on sugar); cutting or an attempt told with a time ("I cut last night", "been cutting for 2 years", "I attempted last month"; not hair, bangs or nails in the sentence, carbs, a paper or a study session cut short); a past attempt ("I tried to end it last weekend", "i overdosed last month", not "on caffeine", nor a relationship ended: "i tried to end it yesterday, should i break up with him"); not expecting to reach an age ("i don't think i'll make it to 18", not to a lecture, over a syllabus, to "18:00" or to "18 credits"); "this is my last post" among goodbyes and letters, not before a break or on one thread; "I have a plan and a date" with nothing after it; "I'm on the roof rn" in a post about jumping; Hinglish ("mujhe marna hai", "marna chahta hoon", "marne ke baare mein sochta hoon", "khud ko khatam kar lunga", "jeene ka mann nahi karta", "main khudkushi kar lunga", "khudkushi ka khayal roz aata hai", "apne aap ko hurt kiya", not an injury at football, "terrace se kood jaungi", not "agar india jeeta", "na is duniya mein" with not staying, "aaj raat sab khatam kar dunga" with a goodbye, not over the syllabus, "khud ko khatam karna chahta hu", "suicide karne wala hoon", "mujhe mar jana chahiye", "fanda laga lunga", "apni jaan de dunga", not "tere liye", "desh ke liye" or "is project ke liye" after it, "neend ki goliyan kha lungi", "mann karta hai mar jaun", "suicide ke thoughts aa rahe hain", "zindagi khatam kar dunga", "jeene ka koi matlab nahi") and Nigerian Pidgin ("I don tire for this life", "I wan die", "make I just die", "I no wan live again", "life don tire me", "I dey cut myself"). Not someone else's ("my friend keeps saying she wants to die", "he said he wants to die after that exam lol", "my mum is thinking about suicide", "why is Hamlet considering suicide"), not words quoted or asked about (a greentext line quoting the post being answered, "kys = keep yourself safe?"), and not an accident or an activity named as one ("i cut myself shaving", "I burned myself on the Bunsen burner"; "I cut myself on my wrist" and "using a razor blade" still count), pills put by for a named trip, camp or exam ("saving up my meds for the trip"), a game or an exam ("if i died in minecraft", "if i died in the exam"), going on to somewhere ("no reason to keep going to tuition", "going on to the next chapter"), dyed hair ("if I died my hair purple"), something said to make the poster want it, with a laugh or a 💀 ("this group project is going to make me kms 💀"; not someone at home or bullying, not a crying face, and "I want to die because of this assignment lol" and "i want to die lol" still count), a mishap told and laughed off or got over ("kms the wifi died right as I hit submit … it went through thank god", "forgot my lines in the school play, I wanted to die 💀"; not "last night", "again" or a serious cause) or a reason found ("I found a reason to live"), or the poster put in the reader's place ("if I were you I'd kill myself", which is held as `self_harm`). And a question about a method ("how many pills would it take to kill me", "what's the most painless way to go", a how-many-pills question with "asking for a friend" or "bas pooch raha hoon" after it and no headache named, "is 20 paracetamol enough to die", "is there a way for me to overdose", "how high does a building have to be for me to not survive the fall", "how to end it all", "how to make myself throw up", "how to slit wrists", "how deep do you have to cut", "where to cut to bleed out", "which vein to cut", "best way to cut without getting caught", "how high to jump to die", "what height is fatal" (not for an egg drop), "how long does it take to die from hanging", "how many ibuprofen will kill you", "what pills can kill you", "what household chemicals can kill you", "can you die from taking 20 paracetamol", "how to throw up after eating", "how to stop eating" (not junk food at night or so fast), "how to hide not eating from parents", "tips to eat less than 500 calories a day", at 800 or fewer), which is then held as `self_harm`. It never blocks: sent again with `"support_seen": true`, the post goes on to the rules below |
| Hold (`202`) | `threat` | An intent to hurt people or attack a place, read from who says it, what they say they will do, and who or what it is done to, whatever the words between: the poster ("I", "we", "imma", "I'm going to", "on monday I", "tomorrow I make her bleed", a greentext line, "got my uncle's pistol in my locker" with the "I've" left out), a verb of harm (stab, shoot, glass, beat up, beat unconscious, beat to a pulp, beat the shit out of, beat his ass, punch, slap, batter, kick his head in, knock out, kidnap, rape, acid attack, run over, push down the stairs, throw off the roof, put in hospital, make bleed, beat to death, burn down, torch, set on fire, blow up, light up, take a hammer to), and a person, a group, a place or a vehicle ("I'm going to run over the PE teacher in the parking lot on monday", "I'll break Kabir's jaw after class", "gonna set the staff room on fire monday", "i'm going to light the staff room up tomorrow"), a person's name read with a capital, or in small letters after a verb done to people where the word is no subject, exam, pastime, food or plural ("i'm going to stab rahul tomorrow at school", not "kill physics tomorrow" or "shoot hoops after school"); bringing or getting a weapon or fuel, a gun's magazine when it is full or loaded, or coming with one, the "I'll" left out ("I'm bringing a machete to the canteen tomorrow", "come back monday with a full mag", "I've got a gun and I'm coming to school tomorrow", "going to the hostel tonight with petrol"); a bomb put or left in a place, or made, though not a bath bomb, a bomb emoji or a bomb made selling notes ("i'll leave a bomb in the library on friday", "i've made a bomb and i'm bringing it to school monday"); a school shooting, a mass shooting, the next school shooter or the next Columbine, or a massacre at school, beside a day or a time still to come, unless the words round it put it in the past or make it a drill, a lesson, the news or a day of exams ("school shooting on monday. stay home", not "there was a school shooting in texas on monday"); a call for someone to ("someone should shoot up the school"); harm said to be coming to a group picked out by where it is or who it is, or to all of them ("everyone in the computer lab at 2pm will get hurt tomorrow", "the guys who stole my bag will be beaten up after school", "whoever snitched gets shot", "they'll finally pay"), to a place or a vehicle ("the library will burn on friday", "the principal's car will blow up tomorrow"), or to someone said to be dead by a day or not to see it ("10B is dead tomorrow", "the warden won't see monday", "won't see the weekend", which is a time of its own, and a teacher or a classmate of the poster's, "our maths sir won't see monday", unless a hospital, an illness, a leaving or an absence is named); a weapon emoji beside a school one; and Hinglish ("tujhe kal school ke baad peet dunga", "kal canteen mein bomb laga denge", "kal school mein uski gardan tod dunga", "uss ladki ko kal college ke bahar utha lenge") — each with a place or a time near it or in the subject — and school-attack warnings; and a threat told by implication, with no act named: two kinds of hint close together, beside a place or a time, of what will be left after ("will be in hospital by friday", "nobody walks out of the canteen alive", "gonna be all over the news", "people are going to get hurt", "burnt to the ground", "lighting it up", "with her in it"), what the poster has ready ("bringing what i need", "i've got what i need in my bag", "i've got enough for every single one of you", "locking the door from outside"), a weapon or a crew with a club ("bringing the lads and some rods"), a verdict ("he's done", "it's happening", "they gon learn", "they'll finally understand", "you have until friday", "kal school mein sabko uda dunga"), a warning to stay away ("don't come", "stay home") or that it is one ("this is not a joke", "remember i warned you", "you know who you are"), and the wrong it answers ("the coach who kicked me off the team"), or one hint that is a threat by itself ("going home in a body bag", "i'm locking them all in the hall tomorrow", "there'll be a package under the stage", a list of names kept for no reason given, a hall that fills up for the 9am "will be the last time it ever does", "kal clg aa rha hu katta leke"); a hint's own sentence can give the place or the time ("his weekend", "our form room", "under the stage"), and "finals week" and a school's sports day are no game. Not threats: the same verbs said of an exam, a meal, a record, the news, a party, photos or the speaker ("I'm going to kill this exam tomorrow", "break the record at sports day", "shoot my shot at the farewell", "drown myself in coffee"), and "aag laga denge" about a presentation or a show; a knife for the cake, a bat for the nets, a prop, a toy, an airsoft gun, a kitchen knife, a water gun at Holi, a blade on a camp or a trip, a gun at a shooting range, petrol for a bike (but not when the post denies it: "this is not a game", "it's not a toy"); a film or sports shoot, a game, laser tag, ping pong or Mario Kart included, with "after school" read as when it is ("gonna murder you at Tekken after school"; though not a shooting or a bomb at one, nor a kill at a sport said of "everyone" or "for real"), in Hinglish too ("tujhe kal cricket mein dho dunga"), a story's plot, news, and a report that quotes what someone said; an exam, a team or a market ("kill JEE tomorrow", "a bloodbath on Dalal Street"); a drill, hurt feelings, killing with kindness or drowning in compliments, a hospital checkup, a lift ("run the principal over to the station"); harm to people at large, to the poster's own things or to one person as a caution ("people will get hurt at this concert", "you'll get hurt if you climb that wall"), someone of the poster's family who won't see monday, or a teacher in hospital or retiring, a sibling beaten up, harm that happens every week, a hall that will "blow up with cheers", a place a team, a festival, an occasion ("at the pep rally"), a decoration or what the poster wears ("in my new jacket") will "light up", a lab an experiment or a science fair will, or a room someone walks into, unless petrol or another fuel is named or harm is named outright (people shot, stabbed, gassed or killed, a weapon, nobody getting out, "they'll never know it was me"); people locked in for a drill, an exam, an event, a project, a test, a practical, revision, the class photo or an escape room, until something ends ("locking them all in the hall until they finish the mock"), or so that they study or turn up ("so nobody bunks"), though not "so they can't escape", "so they learn their lesson", with the same harm named or with the place set on fire ("we're all burnt out" and "the team is on fire" set nothing alight); exasperation with one person taken back at once ("I'm going to strangle him at lunch tomorrow (I won't, I love him)", though not "I won't stop till he's dead"); the same hints said of an exam, a party, a match, someone leaving or falling ill, or as a callout ("the people who stole my charger, you know who you are", "nobody is getting out of this exam alive", "stay home if you're sick"); a stay-away with its reason in the sentence before or after (the food, a crowd, an exam, a teacher in a mood: "the canteen pizza on friday is a crime. stay away") or a purpose ("leave early to catch the bus"), a last chance to sign up or to buy, and an ambulance or a hospital that food, a run or the heat is blamed for ("someone's leaving in an ambulance if the PE teacher makes us run 5k again"); or as a likeness ("staring at me as if I'd brought a gun", "jaise main bomb le ke aaya hoon"); the school magazine; and hyperbole ("you'll die when you see the results", "you're going to die in the exam", "the whole class is dead tomorrow, ma'am is checking homework", "the teachers won't survive monday, three classes of us"). A condition that demands something ("if you tell anyone", "unless you pay") does not make one harmless |
| Hold (`202`) | `self_harm` | Encouragement or method: "kys", "go die", "cut yourself", "cut your wrists", "neck yourself", "off yourself", "an hero", "why are you still alive", "people like that should just kill themselves", "take all your pills", "khudkushi kar le", "chhat se kood ja", "jaa mar" (not "jaa mar kha le pehle", a beating taken or "eat first"), "drink some bleach", "you should end it" and "just end it already" with nothing ended named after or before them (a clause beside the urging that opens on a lecture, a chapter, a term or an essay names one: "just end it already, this lecture is 3 hours long"; "failed the exam again, just end it" does not), "you should stop eating", "stop eating fatty", "go starve", how-many and what-dose questions in either order, "enough to die", lethal doses asked for, whatever weight or study word is near them ("lethal dose of paracetamol per kg?", "pharmacology question: lethal dose of paracetamol?", "failed my exam. whats the lethal dose of paracetamol", "for the tox exam, how many tablets of paracetamol is a lethal dose"; not a pharmacology fact or a "why": "the lethal dose of paracetamol is about 150 mg/kg", "Nursing exam: why is the lethal dose of insulin so unpredictable?", nor a topic after a label naming the subject: "For my pharmacology viva: lethal dose of morphine in opioid-naive patients vs tolerant ones"), how to tie or make a noose, how to purge or the easiest way to, thinspo, meanspo, "ana coach"; and an act urged on someone, in any of the ways of telling them to: an order with nothing but fillers before it ("bro just rope", "pls just od already", "just jump already", "go self harm in a corner"), one after a favour asked ("do us all a favour and overdose", "do the world a favour and stop breathing", "do us all a favour and jump off a building"), a modal said to them ("you need to overdose tonight", "why don't you just stop existing", "time for you to rope"), the act judged to be what they should do ("ending yourself would be the best thing you did all year", "suicide is the only solution for someone with your marks", "the world would be better if you ended your life tonight"), "rope time for OP", "jump in front of a train", "just do it" in a post about dying, "top yourself", "go commit sudoku", a question of when ("when are you finally going to off yourself", "are you ever gonna"), an order after a condition on the reader ("if ur that stressed just jump off the roof", "if you failed that badly just end it"), and Hinglish ("zeher kha le", "apni jaan le le", "pankhe se latak ja", "marr kyun nahi jaata tu", "jaa ke mar ja", "khatam kar le apne aap ko"); and encouragement told in other words: the poster in the reader's place ("if I were you I'd have offed myself years ago", "if i had your marks i'd jump off the roof"), a favour ("you'd be doing everyone a favour if you didn't wake up tomorrow"), the whole of a supply with a push to finish ("just take the whole bottle and be done with it", "a few won't do it, you'd have to take the whole pack"), a drop with no barrier and a nudge ("the top floor of the car park has no barrier. just saying"), a knot drawn with a nudge ("you know what to do 🪢"), "then disappear. permanently." in a post that says nobody would miss them, a chance missed ("should've jumped when you had the chance"), and "if you meant it you'd have done it by now" with a nudge — not when the sentence says not to ("please don't kys", "a bad rank doesn't end your life") or warns ("be careful with the scalpel, you could cut yourself"), not a question that asks after the reader ("are you going to kill yourself? please talk to me", "did you try to kill yourself?"), and not "if I were you I'd end it" or "I'd jump at the offer", "die trying", a drop with no barrier reported as a danger, a whole box of chocolates, a course of medicine finished, a knot for scouts, "disappear for a week", "should've jumped on that deal", "go on then, apply", "die your hair", "a hill to die on", "overdose on caffeine", "jump into the pool", "just jump already, the long jump pit is right there", "jump at the chance", "stop breathing on my neck", "stop living in the past", "cut yourself some slack", "rope in the seniors", "od" for on-duty leave, a boss fight, an act called in code on someone other than the reader ("me.die(); // jk"), "just do it" about a deadline or with nothing worse than dying of embarrassment, "bus mein latak ja", "you should end it with him", "just end it already, the series has dragged for 6 seasons", "you should stop eating so much sugar", "i stop eating fatty food" or "ja mar gaya bhai" |
| Hold (`202`) | `grooming` | A question that places the reader — an age, a school year or grade, a school, a hostel or a room, where exactly someone lives or studies, when they get out ("which year of school are you in?", "what grade are you in?", "where exactly do you live?", "where exactly is your coaching?", "which hostel and room number?", "what time do you get out of school?", "tell me your class and your address", "you're in 9th?", "you're 12?", "that's your tuition right?", "kis school mein padhte ho?", "tum 9th mein ho?", "which hall?", "which centre?", "which coaching centre are you at?", "which suburb are you in?", "which year 10 form are you in?", "you're in JSS3?", "which SS are you in?", "u in class 8 or 9?", "which floor of the girls hostel is your room on?", "what time does your school finish?", "which gate do you use?", Nigerian Pidgin "where you dey stay?", "kaunse saal mein ho?", "what school u at?", "u in high school?") or remarks on their age ("u sound young", "so mature for 13") — or someone looked for by an age or a class ("any 14yo girls here?", "13-15 yo girls", "girls under 16", "any girls aged 14?", "looking for a girl 13-15", "any class 9 girls here?"), and a private-contact, meet-up, visit, secrecy, video-call, gift or photo cue, before the question or after it ("dm me", a "dm" said on its own, "we could meet", "are you alone?", "are your parents home?", "I'll buy you a gift", "I'll come to your school", "can i see a pic", "I'll come by tonight", "come alone", "come without your friends", "wait for me outside", "I'll pick you up after the batch", "somewhere your parents can't see", "do your parents know you talk to strangers?", "your parents don't need to know", "no need to tell your parents", "tell no one", "just between us", "keep our chats between us", "keep it quiet", "delete this after reading", "let's go private", "somewhere more private", "facetime me", "I'd love to see a photo of you", "do u walk home alone?", "I could walk with you", "I'll come up tonight", "come to the chai stall after class, alone", "photo bhejo", "kisi ko mat batana", "kisi ko bolna mat", "ek baar milo", "gift dilaunga", Pidgin "I fit come pick you", "no tell your mummy") in the same post, or contact details the `contact` rule would send back, which count as that cue. An age or a year said for a reason that is all in the open is not one ("when you're in 12th you'll understand", "you're 15 minutes late", "u 14? the competition is for 14 and above", but not an adult's limit: "u 14? this server is 18+"). Nor is asking whether the reader's parents know about a thing ("do your parents know about the fee hike?"), a meeting somewhere public or with a group ("we can meet at the library for group study", "let's meet at the mess for dinner", "our team is meeting at 5"), or a visit with an errand ("I'll come by tonight with the notes"), unless the post asks the reader's age ("how old are you? meet me at the library") or asks for them alone ("come alone to the library", "just us two"). A gate or outside is not somewhere public: "meet me at the gate" is a cue. The same words where nothing is hidden or nobody is asked to be alone are not cues either: "the gap between us", "tell no one your password", "no need to tell anyone your rank", "I'll walk you through the syllabus", "I'll come up with a timetable", "I live alone", "come early, alone is fine but bring your admit card" |

The patterns are written for English, with some Nigerian Pidgin ("I don tire for
this life", "I wan die", "where you dey stay?", "no tell your mummy"), and for some Hinglish
(Hindi written in Latin letters, as in the examples above). Hindi in Devanagari,
and every other language, is not read at all.

Every rule that reads the poster's own voice — crisis language, encouragement,
threats, grooming cues, questions about a method, and asks to talk elsewhere —
reads quoted and reported speech the same way. Words the post gives to someone
else ("people under my art post said 'kys'", "'…' — some guy in my class",
"teacher: '…'"), or says came to the poster ("my bf told me to go kill myself",
"got a message saying "you won't see monday"", "someone wrote 'kys' on my
desk", "I was told to kys"), or names as a kind of post ("the 'add me on snap'
guys") are someone else's, so a student reporting a threat, a grooming attempt
or being told to kill themselves is not held for it. Words the poster gives
themselves are theirs however they are wrapped ("'I'm bringing a gun tomorrow'
- me", "I told him 'kys'", "me: '…'", "note to my bully: '…'", "you heard me"), and
so is a quotation that no one, or only a source such as "news" or "police", is
named as saying, and a greentext story the poster opens with "be me", reply link
or not. A report that
makes the poster the subject of crisis language ("my therapist says I'm
suicidal"), or gives the words to the poster's own mind ("my head keeps telling
me to kill myself"), is still the poster's. Contact details are blocked whoever
the post says they belong to.

Blocks come first, then support (unless `support_seen`), then holds, with one
exception: contact details in a post that also asks the reader's age, year,
school or where they live, or looks for someone of a given age, are held as
`grooming` rather than sent back, since
sending them back would tell the poster which half of the cue to take out. An
ID number is still sent back when it comes before any contact details in its
field. A number
is a phone number only when it stands on its own: dates, times, years, scores,
prices, ISBNs, sums, decimals and ranges of them, numbers with thousands
separators, IPv4 addresses, a program's output ("Output: …", "it returns …"),
labelled references (an order, roll, seat, application, receipt or challan
number, a hall ticket, "ISBN-10:", "PNR status for …", "the ISBN on my copy
is …"), a number of one digit over and over ("9999999999"), digit runs inside longer numbers and a number
given as a maths answer ("the largest 10-digit number with distinct digits is
9876543210", "Answer for Q4 came out as 7364181325", "Got 8247301956 for part
(b)", "How many digits does 2^33 have? It's 8589934592"), unless a call, a message
or an account ("text me", "dm me", "add me", "my insta", a handle) or "my
number" is near it, pass, and so do Ofcom's, ACMA's and the NANP's numbers
for drama and the crisis lines whose numbers look like phone numbers (iCall,
the Vandrevala Foundation, AASRA and Tele-MANAS in India; the old National
Suicide Prevention Lifeline, the Trevor Project, the National Domestic Violence
Hotline, Childhelp, RAINN and SAMHSA's helpline in the US; PAPYRUS HOPELINE247,
the NSPCC helpline, The Mix and Mind's infoline in the UK), written with or
without a country code, a trunk zero or a leading 1. UK freephone numbers (0800
and 0808, grouped as they are printed) pass too, since they are an
organisation's rather than a person's, and so does a landline with an organisation's office named in the four words before it ("UQ admissions (07) 3365 1111", "UNSW switchboard is (02) …"), unless those words say it is the poster's ("my office", "home number") or ask for a call. Numbers in words are read only in the shapes
of a mobile, a North American number only after a word such as "call", and not
as a count ("ten nine eight … one zero").

**Be clear about what this is.** These are keyword and number patterns. They
are easy to get round — a number in words other than one digit at a time
("ninety-eight seventy-six …"), or split some other way (a `·` between the
groups, a wider gap, or pieces split by `/`, `|` or spaces with nothing beside
them saying it is a number to call), a username with no app named beside it, or
one pointed to in words other than the post's own name or subject ("same as my
reddit"), contact details hidden in `[math]` or encoded (base64), an email
address under a top-level domain the filter does not list (it knows the common
generic ones and about fifty countries', India's, the UK's and the US's among
them, plus any university's `edu` or `ac` domain under a country's), the same
request in Devanagari or another language, a threat that names no act, weapon or harm and gives no more than
one of the hints it is read for, or a push towards self-harm that names none
and leaves the reader to work it out ("they'll need to carry you home", "you know what to do" with no knot
drawn, "just do it then" in a reply, "the last one any of them ever have"), or a threat put in someone else's mouth ("my friend
said 'I'm bringing a gun tomorrow'"), which is read as a report — and they
catch posts that
are fine: gaming trash talk, a countdown written as spaced digits, a ten-digit
figure after a word such as "cell" ("cell 2954820174 in the spreadsheet"), a
question about a medicine's dose, a twelve-digit reference number that happens to pass
the Aadhaar check, a sentence that says something "will be killed tomorrow"
when the something is a bill, a debater who will "open fire on the opposition
tomorrow". The rules for crisis language read who is speaking, and a poster who
says it of themselves as a joke, with no cause and no laugh beside it, is shown
the support message all the same. That is why a block says exactly what it matched, and why a
hold waits for a person rather than deleting anything. The filter raises the
cost of moving a conversation off educhan; it does not prevent it, and nothing
about it makes educhan safe for anyone.

What it does with posts like the ones it is written for is pinned by a corpus:
3,578 made-up posts in `internal/app/testdata/filter_corpus`, in twelve files,
three for each kind of post: `academic.jsonl`, `academic_2.jsonl` and
`academic_3.jsonl` (770 posts), the same three for `life` (943, student life),
for `contact` (898, contact details) and for `risk` (967, risky posts), each
labelled with what the filter should do. Every address, username and profile in
them says "madeup" or is at a reserved domain, so none is a real person's
(`TestFilterCorpusDetailsAreMadeUp`); a number meant to be blocked is ordinary
digits given with no name, school or handle, since the filter passes the ranges
set aside for drama, and a North American one is in an area code that is never
assigned, so it is no one's.
`TestFilterCorpus` runs every one through the filter and fails on any it gets
wrong, except those marked as known misses, each with a note saying why the
miss is accepted, and logs how many ordinary posts were stopped and how much of
each kind was caught (`go test -run TestFilterCorpus -v ./internal/app`).
**The rules were fitted to all 3,578 posts.** They were first written against
the files without `_2` or `_3`; the `_2` files were written apart from them and
then used to rewrite the threat, self-harm and grooming rules (which first held
55 of their 135 risky posts) and then the contact and support rules (which
first blocked 98 of their 131 contact details, showed support for 17 of their
30 crisis posts, and stopped 7 of their 613 ordinary posts). The `_3` files
were written apart from the rules as well, and measured as first written and
labelled, before they were read: the filter stopped 18 of their 667 ordinary
posts, blocked 146 of their 183 contact details, showed support for 24 of their
41 crisis posts and held 85 of their 172 risky posts. They were then used to
put those wrong stops right and to widen every kind of rule, cause by cause,
and three posts whose numbers the filter passes on purpose (two drama numbers
and a voided SSN) were given ordinary digits. The last 693 were written to pin
what a review of the rules found they missed or stopped wrongly, each beside
the change that put it right, so they were fitted too. So the rates the test
logs — no ordinary post stopped, and all but three contact details, all but one
crisis post and all but eight risky posts caught — say how closely the rules
fit these posts. They are not a measurement, and say nothing of what the filter
does with posts no one has written yet. The twelve posts left as known misses
are an address in base64, which is not decoded because text like it is ordinary
in code posts, a number given as the value of a sum in `[math]`, a request in
Bengali, eight risky posts whose harm is only implied or written as code, with
no act, weapon or harm named, and a crisis post that never names the act.

Nothing the filter reads or matches is logged, and nothing of a post it sends
back is stored: not the text, not what matched, and not which rule. A refused
post leaves what any request leaves: the request log's line of method, path, status and duration, so a
`422` or `202` on a posting route shows that a post to that board or thread
was refused or held, though not what it said or why; and a count against the
client's address in the refusal budget, kept in memory only and forgotten
within a minute of its window ending. A held post is stored as any post is,
with two additions: `held_at`, when it was held, and `held_reason`, which names
the rule (`threat`, `grooming` or `self_harm`); a held opening post also sets
its thread's `held_at`. Nothing else the filter matched is kept. A moderator's
approval is logged with the board, thread and post number, as every
moderation action is.
It runs in-process, sends nothing anywhere, and takes time linear in the
length of the post. On one core of a shared Intel Xeon at 2.8 GHz, with the
machine's load at about 4 to 6, an ordinary 500-character post took about
1.3 ms, an ordinary post of the maximum length about 8 ms, and the slowest of
the 29,397 posts built to be slow, and of the 151 that
`TestFilterCostIsBounded` runs, about 30 to 33 ms; one sentence repeated to the maximum length took
about 4.7 to 4.9 ms, since the same words over and over give the rules less
to read. That sentence is the yardstick: the ordinary posts take about a
quarter of its time and 1.7 times it, and the slowest 6.5 to 7 times it, and
all of them move with it (on an Intel Xeon at 2.1 GHz, earlier, it took 2.6
to 3 ms). The ordinary posts are the corpus's published ones (below) run
together and cut into 445 pieces of 500 characters and 27 of 8,000, and the
figures are the median piece's (0.8 to 2.1 ms and 6.1 to 10.7 ms across the
pieces). The slowest built posts are a weapon emoji and a mark or a letter
over and over, each read as a hint of a threat, a weapon named over and over
beside a day, and a method question asked three times before an app's name.
Each was measured one post at a time with `GOMAXPROCS=1` under `taskset`, as
the process's CPU time, the slower verdict's best of 11 to 15 runs;
`go test -run - -bench CheckContent ./internal/app` times both kinds of
maximum-length post. Each server prepares the
rules once, as it starts: about 3.5 seconds of one core, with the heap reaching
330–370 MiB on one core while it does (270–300 MiB on four) and about 65 MiB
kept afterwards, the rest given back to the system over the next few minutes;
a post that arrives before that is done waits for it. A
refused post costs no cooldown, so refusals are counted instead: after
`EDUCHAN_REFUSAL_LIMIT` of them (10 by default) in a minute, a client's posts
are answered `429` without being read until the minute is up (see
[Flood control](#flood-control)).

The web page answers each outcome in the posting form: a block gives the
server's sentence, quotes what to remove and selects that copy in its field, and support shows the helplines with
**Post anyway** and **Go back**, both keeping the draft; a held post empties the
form as a published one does and is acknowledged as waiting for a moderator,
with a link to where to get help, without opening anything. Any other refusal of
a post, such as flood control after **Post anyway**, is shown in the form too
(see [Front end](#front-end)).

### Held posts

A held post is stored with `held_at` and `held_reason` set and is in no public
view until a moderator approves it: not in its thread, the index (its last
replies or its counts), the catalog, the archive, post resolution, or anyone's
quotes and backlinks. It leaves its thread's reply and image counts,
`last_post_at` and bump order as they were. It does use up its post number,
which is taken when the post is stored: the next post on the board skips it,
and the gap answers `404` exactly as a post a moderator deleted does, until
an approval fills it. A held opening post holds its whole
thread: the thread answers `404`, takes no replies, cannot be stickied or
locked, archives nothing when it is created, does not count against the
board's thread limit, and is never archived or swept. An archived thread with
a held reply is not deleted by the sweep until the reply has been approved or
deleted, but it leaves public view when it would have been deleted: the sweep
that deletes the threads archived alongside it sets the thread's `expired_at`
instead, and from then on it answers `404`, is gone from the archive, post
resolution and every quote and backlink, and its images answer as a held
post's do. How long a thread stays readable therefore says nothing about a
held post in it. Once its last held reply is approved or deleted, the next
sweep that finds it past its board's `archive_days` deletes it.

A held image stays at its ordinary storage key, and its URL is never published
while the post is held. Its file answers `404`, with `Cache-Control: no-store`,
to any request without the moderation key; with the key it is served, also
`no-store`, because the same URL serves everyone once the post is approved and
is then cached for good. A file whose post or thread has been deleted answers
the same way for as long as it is still stored — while a sweep is removing a
swept thread's files, or after the store refused a removal — so a swept
thread's images and an expired thread's look alike. The quarantine prefix of build plan 2.2, which would
keep the file out of the public namespace altogether, is not built.

**Never roll back across migration `0009`.** Code from before it does not know
`held_at`: every held post and held thread — threats, grooming cues,
self-harm methods — appears in its thread, the index and the catalog, and can
be replied to. Code from before `0011` does not know `expired_at` either, and
shows again every archived thread a held reply kept. The rollback warning in
`deploy/README.md` names only `0005` so far; `0009` and `0011` belong beside
it.

## Image attachments

One image per post, on both thread creation and replies, **when
`EDUCHAN_UPLOADS_ENABLED` is on**. It is off by default, and a post carrying an
image is then refused with `403` and
`{"error":"image uploads are switched off on this server"}`, because nothing
checks what an image shows yet: there is no known-image hash matching and no
content scanning. Metadata is already stripped (below). With uploads off a
multipart post is held to the 64 KiB a JSON post may be, so the server stops
reading an image once the body passes that, and a file over the size limit gets
the same `403` rather than a `413`; a multipart post with no file, or with the
empty file part a browser sends for an untouched input, posts as usual.
`GET /api/site` reports the switch and the size limit as
`{"uploads","max_upload_bytes"}`, so a client can leave the image picker off a
server that would refuse what it sends.

- **Formats**: JPEG, PNG, WebP, and GIF, identified by magic bytes rather than by filename
  or the content type the client claims. Anything else, an image whose header claims
  more than 25 megapixels (room for the 24-megapixel photos phones and cameras save, an
  iPhone's 5712×4284 included), a GIF whose frames add up to more than four times that,
  whose blocks cannot be read through to its end, or whose first frame reaches past its
  canvas (browsers disagree on how to show that one), or an image that would take more
  memory to process than the server sets aside for all of them at once, is refused with
  `400` before anything is decoded and before flood control. A GIF that ends after its
  last frame without the closing byte, or whose later frames reach past its canvas, is
  stored as browsers show it: whole, and with each frame cut to the part on the canvas.
  The memory depends on the encoding as well as the size, and on the shape: a longer,
  thinner picture costs more for its pixels, because a JPEG's sides are rounded up to
  whole blocks and making the thumbnail takes 64 bytes and more for every pixel of the
  longer side. At the full 25 megapixels, from an upload of up to the default 8 MiB, at
  any shape the format allows:
  - *accepted*: every baseline JPEG, in colour at any chroma subsampling or in grey,
    whether or not it has to be turned upright, and progressive ones whose colour is at
    a quarter resolution or less (4:2:0, 4:1:1, 4:1:0) or grey, but for a 4:2:0 or
    4:1:1 one that a turn stores at 4:4:4 (below); baseline CMYK and RGB JPEGs that need
    no turning; palette PNGs and grey PNGs with no transparency; 8-bit colour PNGs,
    unless they are interlaced and have to be turned; and opaque lossy WebPs, stored as
    JPEGs. A grey PNG with a transparent colour or an alpha channel counts as a colour
    PNG throughout here, since it decodes the same way — four bytes a pixel at 8 bits,
    eight at 16. The PNGs are accepted until their longer side passes about 750,000
    pixels in colour, or 1.3 to 1.5 million otherwise: at this size, tens of thousands
    of times longer than they are wide, which only a PNG can be (a JPEG's sides stop at
    65,535 pixels and a WebP's at 16,383).
  - *refused*: progressive JPEGs with full-resolution colour or in CMYK; CMYK, RGB and
    progressive 4:2:2 and 4:4:0 JPEGs, and interlaced 8-bit colour PNGs, that have to be
    turned; 16-bit colour PNGs that are interlaced or have to be turned; and lossy WebPs
    with transparency, which are stored as PNGs (below) and so take four bytes a pixel.
  - *either*, by the picture's shape: a progressive 4:2:0 or 4:1:1 JPEG that a turn
    stores at 4:4:4, accepted at every shape up to about 8:1 at 4:2:0 and about 2:1 at
    4:1:1 and refused at some shapes past that; progressive 4:2:2 and 4:4:0 JPEGs that
    need no turning, accepted up to about 60:1; and, by its shape and the upload's size,
    a 16-bit colour PNG that is neither interlaced nor turned.

  At a quarter of the size, every kind is accepted at any shape up to a thousand to one,
  and a PNG of any size is refused once its longer side passes about 4 million pixels,
  however narrow it is. A GIF of many thousands of frames is refused however small they
  are. An animated WebP passes those checks and is refused with `400` (`image could not
  be decoded`) once it is processed, after flood control: nothing in Go can decode one
  to thumbnail it, so the animation flag in its header turns it away as the file is
  rewritten, before any of it is decoded. An image whose rewritten file would come to
  more than four times the upload plus half a byte a pixel is refused after flood
  control too, once it is encoded (`image would be too large once re-encoded for
  storage`), which only a file made to compress in a way Go's encoders cannot follow
  reaches. A lossy WebP with transparency is held instead to what a PNG of its size can
  come to, because stored losslessly it is meant to be many times the size it came in
  at; it is refused from its header when that is too large to process.

  A lossless WebP, and a lossy one's transparency when it is compressed (as libwebp
  compresses it by default), are the one case refused by the *size* of what is
  compressed rather than by the picture's resolution: Go's WebP decoder sizes its
  internal tables from the compressed data, not from any header field, so a crafted file
  of 160 KiB can make it hold about 170 MiB as it decodes, and a larger one more. There
  is no way to tell a benign file from a crafted one without decoding it, so a lossless
  WebP whose picture data comes to more than 224 KiB is refused from its header — less
  for a larger picture, about 205 KiB at a megapixel and 130 KiB at nine, and a little
  less in a larger upload — and so is a lossy WebP whose compressed transparency alone
  comes to that much. A small lossless picture (an icon or a sticker) is still stored,
  and so is an ordinary cut-out, whose transparency is a few hundred bytes to a few
  kilobytes however large its picture; what else the file carries, a colour profile or
  EXIF, does not count. The decoder also sets the largest of those tables aside before
  it reads what is meant to fill them, so a broken file of a few dozen bytes can make it
  take 167.5 MiB before it finds out: every such WebP, however small, takes at least
  171.5 MiB of the processing budget (see **Memory**) while it is processed, so two are
  never processed at once. Opaque lossy WebPs, and those whose transparency is
  uncompressed, are not affected.
- **No metadata is stored**: the original kept and served is never the uploaded file but
  the picture rewritten without what a camera or an editor puts around it — EXIF with
  its GPS position, camera and timestamps, XMP, IPTC captions, comments, and colour
  profiles. Both storage backends get the same rewritten file. Files stored before this
  are the exception (below).
  - *JPEG* is decoded, turned upright by its EXIF orientation, and re-encoded, so a
    phone photo stored on its side is upright everywhere, thumbnail included, and its
    reported width and height are the upright ones. It keeps its chroma subsampling, so
    colour saved at full resolution (4:4:4, as editors save screenshots and diagrams at
    high quality) stays sharp. The exception is a picture whose sampling grid a turn
    cannot line up, which is stored at finer 4:4:4 instead, a larger file with its
    colour kept rather than lost: one that the turn mirrors along a side that is not a
    whole number of colour samples long (an odd width at 4:2:2 or 4:2:0, an odd height
    at 4:4:0, 4:2:0 or 4:1:0, or a width that is not a multiple of four at 4:1:1 or
    4:1:0), and a 4:1:1 or 4:1:0 picture turned on its side. Which sides a turn reverses
    depends on the turn, so an odd-sided picture can keep its subsampling through one
    turn and not through another. It is re-encoded at the lowest quality that compresses
    no detail more coarsely than the upload did, and at 90 at most: a photo saved at
    quality 30 by most software is stored at 30, at about the size it came in at and
    looking as it did, and one saved at 95 is stored at 90, a little smaller. A file
    whose encoder fitted its Huffman tables to the picture or saved it progressive, as
    the "optimise" and "progressive" options of an editor do, comes out larger, because
    Go writes the standard Huffman tables in a single scan: a little larger for a
    high-quality save (around a tenth at quality 90), but as much as twice the size for
    a heavily compressed one, where those fixed tables outweigh the picture's own coded
    data.
  - *PNG* is decoded, turned upright by an EXIF orientation in its `eXIf` chunk (which
    browsers apply too), and re-encoded, keeping none of its ancillary chunks. A gamma
    it is tagged with in a `gAMA` chunk, which browsers apply as well, is applied to its
    colours first, so that it shows as it did (see below). Every opaque pixel otherwise
    keeps the value it decodes to, and a transparent or partly transparent one only as
    much colour as it shows (see the bullet on hidden channels below), though not always
    in the form it came in: 1-, 2- and 4-bit grey is stored at 8 bits, grey or colour
    with a transparent colour key is stored as colour with an alpha channel (or with
    none, if no pixel has the key's colour), and a palette keeps only the colours its
    pixels use, written at as few bits a pixel (1, 2, 4 or 8) as that many colours need.
    16-bit pictures stay 16-bit. An animated PNG keeps its still image.
  - *GIF* is re-encoded frame by frame, keeping every frame, delay, disposal and the loop
    count (from either of the two extensions browsers read it from), and dropping
    comments and application data such as XMP. Its colour tables keep only the colours
    its frames show.
  - *WebP* cannot be encoded in Go, so it is stored as another format made from its
    pixels: a lossy, opaque WebP as a JPEG at quality 90 that keeps the WebP's own
    half-resolution (4:2:0) colour, and any other (lossless, or with transparency) as a
    PNG, which keeps a lossless picture exactly. A WebP's quality setting does not mean
    what a JPEG's does, so there is no lower quality to match, and the JPEG is larger
    the lower the quality the WebP was saved at: measured on photos, up to 1.4 times the
    WebP's size from quality 90, two to three times from 75, three and a half to five
    and a half from 25, and up to eight and a half from 5. A lossy WebP's colours come
    back from the decoder in a narrower range than they were saved in, so they are
    expanded again before storing, or black would be kept as dark grey and white as
    light grey. Its `name`, URL and content type are the stored format's (`image.jpg`,
    `image.png`). Keeping the WebP with only its metadata chunks removed would not be
    enough: a decoder ignores anything after the picture inside the chunks that hold it,
    so a second file could ride through unseen. WebPs stored before this stay WebPs.
  - Besides metadata, a picture can carry a whole second file in channels it never
    shows, and those are dropped too. A fully transparent pixel is stored with no
    colour, because an editor that erases part of a picture by clearing its alpha often
    leaves the erased part behind. A pixel that is only slightly transparent shows
    almost none of its colour, yet keeps every bit of it, so each pixel's colour is cut
    to the precision its alpha can actually show — a photo laid under an alpha of 1
    composites to one faint tint and is stored as that tint, not as the photo. A palette
    or colour table is rebuilt from the colours its pixels show, which drops the entries
    no pixel uses, the duplicate entries of one colour, the order of the palette, and
    which of several equal entries each pixel points at — any of which can hold a second
    file, EXIF and all, in the pixel bytes of a flat picture. What is left is the
    picture: a fully opaque pixel keeps its exact value, including the low byte of a
    16-bit channel, because that is the picture and a lossless format stores it exactly
    (but for a PNG's gamma, applied as above, which changes the values so that the
    picture shows as it did). So data hidden in the low bits of a visible picture, as
    classic steganography does, survives — the aim is to strip the metadata and the
    channels that do not show, not to alter the picture a reader sees.

  Dropping the ICC profile means a wide-gamut photo (Display P3, Adobe RGB) can look
  slightly duller than it did; most photos are sRGB and look the same. A profile can
  name the device or the editor that made it, and converting colours properly needs a
  colour engine Go does not have. A PNG's `iCCP` profile and `cHRM` primaries are
  dropped the same way. Its `gAMA` chunk is not, because dropping it shifts far more
  than slightly: a picture tagged with the linear gamma of 1.0, as renderers write,
  would show its mid-grey at 128 where it showed at about 188. So its colours are
  converted to the sRGB values that show the same light, as Chromium converts them —
  unless an `sRGB` or `iCCP` chunk takes its place, as it does in a browser, or the
  gamma is within 5% of the usual 1/2.2, which browsers leave alone. The checksum
  recorded for an attachment is of the stored file, so it does not depend on the
  metadata an upload arrived with, beyond an orientation that turns the picture.
- **Files stored before this** keep their metadata: a server that took uploads before
  it rewrote them kept them as they came, and nothing rewrites them now, so they are
  served that way until their posts are deleted. The deployment in `deploy/` has kept
  uploads off, so it has none. On a server that took them, migration `0008` ran when
  this release first started, and this lists the posts whose files came before it:

  ```sql
  SELECT b.slug, t.no AS thread, p.no AS post
  FROM attachments a
  JOIN posts p ON p.id = a.post_id
  JOIN threads t ON t.id = p.thread_id
  JOIN boards b ON b.id = p.board_id
  WHERE a.created_at < (SELECT applied_at FROM schema_migrations WHERE version = 8);
  ```

  Deleting those posts through the [Moderation API](#moderation-api) removes their
  files; deleting an opening post deletes its thread. An older instance still serving
  while a release rolls out stores uploads as they came after that moment, so on a
  rolling deploy move the cutoff to when the last of them stopped.
- **No filename is kept**: a camera's or a phone's filename can name a date and a person
  (`IMG_20261013_Priya.jpg`), so every attachment's `name` is `image` and the extension
  of the format it was stored in (`image.jpg`, `image.png`, …), whatever the file was
  called. Migration `0008` renamed the attachments stored before this the same way.
- **Memory**: how much memory an image takes to decode and re-encode depends on how it is
  encoded as well as on its size, so each upload's share is estimated from its header.
  The uploads being processed at once share a fixed budget (256 MiB per process), each
  holding its share until its files are stored; one whose estimate alone is over it is
  refused (see **Formats**), and the rest wait for room in the order they arrived. The
  uploads' own bytes have a budget of their own (64 MiB), taken before a request body is
  read, so how many requests an instance is let take at once does not decide how many
  images it holds. Between them that is 320 MiB at most, beside the content rules
  prepared at start (about 65 MiB): eighty 8 MiB uploads sent at once to a server run
  with `GOMEMLIMIT=400MiB` and one core peaked at 416–424 MiB resident, whether they came
  while the rules were being prepared or after, so an instance of 512 MiB has room with
  that limit set; without it the same burst reached anywhere from about 470 to 540 MiB
  (469–540 MiB over the runs of two separate measurements, on a shared machine of four
  vCPUs), which is no room at all.
  A post waits for room in either budget for about ten seconds at most. Once there is
  room to read it, its body has 30 seconds to arrive, and once it has arrived its image
  has 25 seconds to be processed and stored. A post that cannot be taken in time — no
  room within the wait, or its image not stored within those 25 seconds — answers `503`
  (see [Flood control](#flood-control)) with nothing stored, rather than being made after
  its answer could no longer be sent. A body that takes longer than its 30 seconds is the
  connection's doing, not the server's, and answers `408` with
  `{"error":"upload too slow"}`, with nothing stored and no cooldown started; the time a
  post spent waiting for room is not counted against its body.
- **Spoilers**: a `spoiler` field (`1`, `true`, or `on` in a form; a boolean in JSON) marks
  the image to be covered until it is clicked. The attachment comes back with
  `"spoiler": true`; the thumbnail is made exactly as for any other image, because covering
  it is the client's job.
- **Per-thread cap**: a thread holds at most its board's `image_limit` posts with an image
  (150 by default), the opening post included. Past it a reply with an image answers
  `403` with `{"error":"image limit reached"}`; text replies carry on. Thread views report
  `images` and `at_image_limit` so a client can hide the picker before anyone tries.
- **Size limit**: 8 MiB by default (`EDUCHAN_MAX_UPLOAD_BYTES`), and 63 MiB at most.
- **Thumbnails**: generated at upload time, 250px on the longest side, aspect ratio
  preserved and never upscaled. A thumbnail follows the stored format, not the uploaded
  one: an image stored as a JPEG (a JPEG upload, or a lossy, opaque WebP) has a JPEG
  thumbnail, and one stored as a PNG or a GIF (any other WebP included) has a PNG
  thumbnail, so its transparency survives.
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
| Attachments | One image per post and a per-board cap per thread, validated by magic bytes, stored without metadata (photos turned upright first), thumbnailed at upload, served from `/media/…`; off unless switched on, and `GET /api/site` says which |
| Verification | Optional institutional badge from an emailed code; the address is never stored and the badge cannot be linked to a post |
| Content rules | Posts with contact details or ID numbers are sent back with what to take out; possible threats, self-harm encouragement and grooming cues are held for a moderator; crisis language is answered once with a support message. Keyword patterns, with the gaps that implies |
| Moderation | Sticky, lock, and delete a post or a whole thread, approve held posts, plus creating and reconfiguring boards, behind a shared moderation key |
| Archive | Threads pushed past a board's thread limit are archived rather than deleted: they leave the index and the catalog, refuse replies, and still load by number |
| Housekeeping | `educhan -sweep` deletes archived threads, media included, once their board's `archive_days` have passed — except one with a held reply, which leaves public view then and is deleted by a later sweep once the reply is decided; per-client cooldowns limit new threads, replies, and uploads, and forget each address within a minute of its cooldown ending while the process has CPU between requests (see Flood control for an idle Cloud Run instance) |

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
  messages on purpose, says what the automatic checks do (send a post with contact
  details or an ID number back, keep a possible threat, self-harm encouragement or
  grooming cue for a moderator, and show where to get help before a post about wanting
  to hurt yourself, which can still be posted) and that they are word patterns that miss
  plenty, and says what moderators can do today: approve a held post, delete posts and
  threads, and lock a thread. Boards can add their own rules under the header's **Rules**
  disclosure. `/faq` covers what educhan is, who it is for, why it asks when you were
  born, why there are no accounts or private messages, whether posts are checked before
  they appear, what anonymity here does and does not hide, the badge, tripcodes,
  per-thread IDs, markup, and the bump limit, image cap, and archive. Both end with the same fixed list of helplines (US, UK and Ireland, India,
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
  sends it hidden: readers see a neutral tile, caption included, until they click it or
  press Enter on it, and it then expands like any other; the catalog shows the tile only.
- **Content rules in the form**: the server's answers appear in the posting form, above
  its buttons, and the draft is never lost. A post sent back for contact details or an ID
  number says what kind of thing is not allowed and quotes the part to remove (naming the
  Name field or the Subject when it is there), and that field takes the focus with the
  part selected. A post with crisis language opens a **Before you post** panel with the
  same helplines as the rules and the FAQ and two buttons: **Post anyway**, which sends
  the same post again with `support_seen` for that one request (never stored), and **Go
  back**, which closes the panel. A post kept for a moderator empties the form as any post
  does, stays on the thread or board, and says it is waiting for a moderator and will only
  appear if they approve it; nothing navigates to it.
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
| `EDUCHAN_REFUSAL_LIMIT` | `10` | Posts the content rules may send back to one client in a minute before its posts get `429` unread for the rest of it; `0` switches it off |
| `EDUCHAN_LOG_FORMAT` | `text` | `text` or `json` structured logs |
| `EDUCHAN_MEDIA_BACKEND` | `local` | Where attachments are stored: `local` or `gcs`; see [Storage backends](#storage-backends) |
| `EDUCHAN_UPLOAD_DIR` | `./data/uploads` | Root the `local` backend writes originals and thumbnails to; ignored by `gcs` |
| `EDUCHAN_GCS_BUCKET` | empty | Cloud Storage bucket the `gcs` backend uses. **Required** when the backend is `gcs`, ignored otherwise |
| `EDUCHAN_GCS_PREFIX` | empty | Optional key prefix inside that bucket, so it can be shared with other content |
| `EDUCHAN_MAX_UPLOAD_BYTES` | `8388608` (8 MiB) | Largest accepted attachment; at most `66060288` (63 MiB), so the request carrying one fits the 64 MiB the server sets aside for upload bodies, and a larger value stops the server starting |
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
cooldowns and a refusal budget apply:

| Action | Cooldown | Counted per |
| --- | --- | --- |
| Creating a thread | `EDUCHAN_THREAD_COOLDOWN` | client and board |
| Replying | `EDUCHAN_POST_COOLDOWN` | client and board |
| Attaching an image | `EDUCHAN_UPLOAD_COOLDOWN` | client, across every board |
| Being sent back by the content rules | `EDUCHAN_REFUSAL_LIMIT` a minute | client, across every board |

Posting is counted per board so one client can follow a conversation on two
boards at once. Uploads are counted across the whole server instead, because
decoding, thumbnailing, and storing an image costs the same wherever the post
lands. A post with an image has to clear both its posting cooldown and the
upload cooldown; when it is refused, neither cooldown is started, so the two
limits never compound.

A refused request answers `429` with a `Retry-After` header in seconds and a
`retry_after` field in the body. Set a cooldown to `0` to switch it off.

A post with an image the server is too busy to take in time — no room to read or
process it within about ten seconds, or its image not stored within 25 seconds of
its body arriving (see **Memory** under [Image attachments](#image-attachments)) —
answers `503` the same way, with a `Retry-After` and `retry_after` of 10 seconds.
Nothing is stored for it, and a cooldown it started is given back, so it can be sent
again as soon as the wait is over. A post whose body takes longer than 30 seconds to
arrive, counted from when there was room to read it, answers `408` with
`{"error":"upload too slow"}` and no `Retry-After`, since waiting does not make a
connection faster; it is refused before flood control, so it starts no cooldown. A
request rejected for any other reason — an empty body, an oversized image, a file
that is not an accepted image or whose header shows it too large to process, a reply
to a missing, locked or archived thread or an image past a thread's cap — does not
start a cooldown, so a typo never costs a wait. Two refusals come only after the
cooldowns have started: an image whose header reads but whose pixels do not decode,
or whose rewritten file would be too large to store, which only decoding and
encoding it can find, and those are the cost the upload cooldown is there for; and
the loser of two replies racing for a thread's last image slot (see the HTTP API).

A post the [content rules](#content-rules) send back, with a block or the
support screen, starts no cooldown either, so a poster told to take a phone
number out can post again at once. A post that a cooldown would turn away is
answered `429` before the rules read it, so a client cannot use the wait to
learn what they would have said. Each one is counted instead, and a client
sent back `EDUCHAN_REFUSAL_LIMIT` times (10 by default) within a minute of the
first is answered `429` on every post, clean or not and without it being read,
until that minute is up. A person correcting a post is sent back once or twice;
the limit is there because reading a post is the expensive part of refusing
it. `0` switches it off.

The cooldowns and the refusal budget are keyed on client addresses, and they
are the only places the server holds one. A sweep runs once a minute and drops every entry whose
cooldown has run out, so **an address is held in memory no longer than the
longest cooldown that applies to it plus one minute**, counted from its last
post (or code request, for verification's cooldown; or, for the refusal
budget, from the first refusal of its minute, so two minutes), whether or not anyone
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
deployment enforces the cooldowns and the refusal budget it sees. Moving to
shared storage means replacing `cooldownLimiter` and `refusalBudget` in
`internal/app/ratelimit.go`, which nothing else depends on.

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
| `POST` | `/api/boards/{slug}/threads` | Create a thread — `{"subject","name","body","badge","spoiler","support_seen"}` |
| `GET` | `/api/boards/{slug}/catalog` | Every live thread as excerpt tiles |
| `GET` | `/api/boards/{slug}/archive` | Archived threads, most recently archived first: `{"board","threads":[{"no","subject","excerpt","replies","created_at","archived_at"}]}`, at most 3000 |
| `GET` | `/api/boards/{slug}/threads/{no}` | One thread with its published posts, live or archived; `404` for a held thread, and for an archived one kept past its board's archive period by a held reply |
| `GET` | `/api/boards/{slug}/posts/{no}` | Which thread a published post is in — `{"board","no","thread_no"}`, live or archived; `404` when the board has no such published post (a held post's number answers as a deleted post's does) |
| `POST` | `/api/boards/{slug}/threads/{no}/posts` | Reply — `{"name","body","sage","badge","spoiler","support_seen"}` |
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

Both posting endpoints apply the content rules (see Content rules) after the
body, subject and badge checks — and for a reply after the checks above — and
between the two halves of flood control, before the image is stored: a post
from a client still in a cooldown is answered `429` without being read, and
one the rules send back starts no cooldown and leaves no file. They apply to badged and unbadged posts alike.

- A block answers `422` with
  `{"error","outcome":"block","rule","field","match","occurrence"}`: `error` is
  one plain sentence naming what to take out and from where, `rule` is
  `"contact"` or `"id_number"`, `field` is `"body"`, `"subject"` or `"name"`,
  and `match` is the text that tripped it as the poster wrote it, trimmed and
  at most 80 characters; a longer one is cut short and ends in `…`. The same
  text can be in the field more than once with only one copy refused (a number
  in `[math]`, then the same number after "call"), so `occurrence` says which:
  how many times `match` (without a closing `…`) is found earlier in the field,
  its words in order with any run of whitespace between them, each search
  starting one character on from the last place found. It is `0` for the
  first.
- Support answers `422` with `{"error","outcome":"support"}`. Sending the same
  post again with `"support_seen": true` takes it past this step and no other.
- A hold stores the post and answers `202` with `{"held":true,"board"}`, plus
  `"thread_no"` for a reply. It says nothing of why, and gives no post number.
  A held post takes the posting cooldown like any other.

JSON request bodies must be `application/json`; unknown fields are rejected. The
two posting endpoints also accept `multipart/form-data` with the same fields
plus an `image` file; the flags (`sage`, `badge`, `spoiler`, `support_seen`)
accept `1`, `true`, `on` or `yes`. Errors are `{"error": "..."}`. A request
refused by flood control or the refusal budget (`429`) and a post turned away
because the server is too busy (`503`) add `retry_after` plus a `Retry-After`
header; the other `429` and `503`, too many wrong verification codes and
`/healthz` with the database down, carry neither. An image post whose body does
not arrive within 30 seconds of there being room to read it answers `408`
(`upload too slow`).

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
| `GET` | `/api/held` | The held-post queue, threats first and then oldest first, at most 500, with the total held |
| `POST` | `/api/boards/{slug}/threads/{no}/posts/{post}/approve` | Publish a held post |

Deleting a post removes its stored image and thumbnail from the media store as
well as its rows. Deleting a thread's opening post deletes the thread, which the
response reports as `"thread_deleted": true`. After a deleted reply the thread's
reply and image counts are recounted from its published posts, so bump-limit
state stays accurate.

`GET /api/held` answers `{"posts":[…],"total":N}`, each post with `board`,
`thread_no`, `no`, `is_op`, `held_reason` (`threat`, `grooming` or
`self_harm`), `created_at`, `subject` (the thread's, for an opening post; empty
for a reply), `name`, `tripcode`, `poster_id`, `body`, `badge` and `attachment`
— everything a reader would see, so a moderator can decide from the queue alone
and tell which of a thread's posters sent it. Threats
come first, oldest first among them, and then every other held post oldest
first: a threat names a place or a time that may be today, while every other
held post is already harmless where it is, so a backlog of other holds can
never push a threat off the first 500. `total` counts every held post, so a
moderator can see how far behind the queue is. A held post's image is served
to a request with the moderation key, as in [Held posts](#held-posts). Approving answers
`{"board","thread_no","post_no","approved":true}`, `409` with
`{"error":"post is not held"}` for a published post, and `404` for one that is
not in that thread. An approved reply takes its place in the thread by number
and the thread's counts are recounted to include it, without a bump: approval
can come hours later, and a late reply should not move a thread nobody else has
touched. An approved opening post puts its thread on the board as new, and the
board is pruned back to its thread limit exactly as a new thread prunes it.
Approval can take a thread past its image limit, which counts published posts
only. Rejecting a held post is deleting it, with the two `DELETE` routes.

```bash
export MOD_KEY=…
curl -s -X POST localhost:8080/api/boards/meta/threads/1/sticky \
  -H "Authorization: Bearer $MOD_KEY"
curl -s -X DELETE localhost:8080/api/boards/meta/threads/1/posts/4 \
  -H "X-Mod-Key: $MOD_KEY"
curl -s localhost:8080/api/held -H "X-Mod-Key: $MOD_KEY"
curl -s -X POST localhost:8080/api/boards/meta/threads/1/posts/7/approve \
  -H "X-Mod-Key: $MOD_KEY"
```

Nothing pages a moderator when a post is held, threats included (build plan
2.11); the queue has to be read.

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
the secrets:

```bash
gcloud run deploy educhan --image "$IMAGE" --region "$REGION" \
  --service-account "$RUN_SA" \
  --add-cloudsql-instances "$CLOUDSQL_INSTANCE" \
  --set-secrets "DATABASE_URL=educhan-database-url:latest,EDUCHAN_POSTER_ID_SALT=educhan-poster-id-salt:latest,EDUCHAN_MOD_KEY=educhan-mod-key:latest" \
  --set-env-vars "EDUCHAN_TRUST_PROXY=1,EDUCHAN_MEDIA_BACKEND=gcs,EDUCHAN_GCS_BUCKET=$MEDIA_BUCKET,EDUCHAN_LOG_FORMAT=json" \
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

Three things need setting for a real deployment, and the deploy guide covers
each: `EDUCHAN_POSTER_ID_SALT` has to be a stable secret or poster IDs differ
between instances, `EDUCHAN_TRUST_PROXY` is `1` behind Cloud Run's front end, and
media belongs in Cloud Storage — `EDUCHAN_MEDIA_BACKEND=gcs` with an
`EDUCHAN_GCS_BUCKET`, because the container filesystem does not outlive the
container.

### The archive sweeper

Archived threads are deleted by a one-shot mode of the same binary rather than
by anything running inside the server:

```bash
educhan -sweep          # or, from a checkout: make sweep
```

It loads the usual configuration, connects, applies any pending migrations,
deletes every archived thread whose board's `archive_days` have passed — posts,
attachment rows, and then the images and thumbnails in the media store — and
exits. The exception is a thread with a held reply: that one leaves public view
instead (`expired_at`; see [Held posts](#held-posts)) and is deleted by a
later sweep once the reply is approved or deleted: the first that finds it past
the board's `archive_days`. It works in batches of 100 threads, one transaction each. Each batch
records the storage keys of its threads' files in the `media_deletions` table in
the same transaction as it deletes the rows; once the batches are done the
sweep removes each queued file and forgets its key. A file the store refuses to
delete keeps its key, so the next sweep tries it again rather than leaving an
image stored with nothing left to say it exists. From the moment its rows are
deleted until it is removed, the file answers `404` to everyone but a
moderator, as an expired thread's images do.

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

One kind of state remains per instance:

- **Flood control.** `cooldownLimiter` (the cooldowns) and `refusalBudget`
  (`EDUCHAN_REFUSAL_LIMIT`) keep their state in process memory, so N instances
  enforce each of them N times over. Moving them to shared storage means
  replacing those two types, which nothing else depends on.

## Development

```bash
make db-up   # local PostgreSQL for development and the integration tests
make run     # go run ./cmd/server
make sweep   # go run ./cmd/server -sweep: delete archived threads past their archive period once
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
