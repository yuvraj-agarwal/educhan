// educhan front end: a small hand-rolled SPA over the JSON API.
//
// Routes mirror the API so links are shareable:
//   /                     board list
//   /verify               institutional verification
//   /rules, /faq          the site's rules and questions; readable before the age screen
//   /{slug}/?page=N       board index
//   /{slug}/catalog       catalog
//   /{slug}/archive       archived threads, read-only
//   /{slug}/thread/{no}   thread
//   /{slug}/post/{no}     any post by number; resolved to its thread, then replaced
//
// Institution boards are stored as "u-{name}" and live under /u/{name}/ instead
// of /{slug}/, with the same routes beneath.

const NAME_STORAGE_KEY = "educhan:name";
// Top-level paths that are pages or reserved for them, never boards. The server
// refuses these as board slugs too, so a board cannot take a path out from
// under a page that is added later. "u" is the institution namespace.
const RESERVED_PATHS = new Set([
  "api", "media", "healthz", "lib", "verify", "mod", "u", "rules", "faq",
  "legal", "terms", "privacy", "dmca", "report", "archive", "safety",
]);
// The reserved words that have a page of their own so far.
const PAGE_VIEWS = new Set(["verify", "rules", "faq"]);
const INSTITUTION_PREFIX = "u-";
// The youngest age educhan is open to. Below 13, COPPA requires verifiable
// parental consent, and an anonymous site has no parent to ask, so it cannot
// give it. Every sentence on the site that names the age is built from this,
// and 0 switches the age screen off altogether.
const MIN_AGE = 13;
// All the age screen remembers is its outcome — passed, or refused — and never
// the answer: the month and year live in the dialog's two selects only until
// they have been compared. The version suffix lets a changed screen ask again
// without taking an old pass for a new one. Only the pass key is ever bumped:
// a refusal is final, and a new screen is no reason to give a child it has
// already turned away another go, so the refusal key keeps its name for good.
const AGE_OK_STORAGE_KEY = "educhan:age-ok-v1";
const AGE_REFUSED_STORAGE_KEY = "educhan:age-refused-v1";
// How far back the year list goes.
const AGE_YEARS = 100;
const MONTHS = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
];
// What anyone can read before answering the age screen: what they would be
// agreeing to, and how the site treats them.
const UNGATED_VIEWS = new Set(["rules", "faq"]);
// The verification credential lives in localStorage and is sent only on the
// posts that ask for a badge, in this header. It is deliberately not a cookie:
// a cookie would ride along on every request to the origin — anonymous posts,
// page loads, images — and a token attached to everything is a session
// identifier however it is described. localStorage rather than sessionStorage
// because the server issues a credential once and cannot reissue it: per-tab
// storage discards the badge on every tab close and charges a full email round
// trip to get another, which also spends the poster's own verification
// cooldown — per address hash, and per client address behind a campus NAT.
// Persisting is what makes the credential TTL mean anything.
const CREDENTIAL_STORAGE_KEY = "educhan:credential";
const CREDENTIAL_HEADER = "X-Educhan-Credential";
// The poster key makes a per-thread poster ID belong to a browser rather than
// to a network: behind a campus NAT every client shares one address, and IDs
// keyed on it merge a lecture hall into one poster. It is random, made here,
// and sent only on the two requests that create a post — never on a read, an
// image, or anything else — because a key attached to everything would be a
// session identifier. The server mixes it into its HMAC and never stores it.
const POSTER_KEY_STORAGE_KEY = "educhan:poster-key";
const POSTER_KEY_HEADER = "X-Educhan-Poster-Key";
const POSTER_KEY_PATTERN = /^[A-Za-z0-9_-]{22}$/;
const ACCEPTED_IMAGES = "image/jpeg,image/png,image/webp,image/gif";
const ACCEPTED_EXTENSIONS = /\.(jpe?g|png|webp|gif)$/i;
// Fine pointers get quote previews on hover; touch screens have no hover to
// preview with, so the link just navigates.
const HAS_HOVER = window.matchMedia("(hover: hover) and (pointer: fine)").matches;

const state = {
  boards: [],
  route: { view: "home" },
  // The thread on screen, in the thread view.
  thread: null,
  // enabled comes from the server, and answered says whether it has said yet:
  // until it has, enabled is false only for want of an answer
  // (loadVerification). badge is read out of the credential this browser
  // holds, which the server is never asked about outside of using it.
  verification: { answered: false, enabled: false, badge: null },
  // What this server takes, from GET /api/site: whether it accepts images at
  // all, the largest it accepts, and whether a post needs the captcha. Until
  // that answers, or if it never does, the picker is offered as it always was,
  // the size limit is unknown (0) and no answer is sent; the server's refusal
  // is the last word either way.
  site: { uploads: true, maxUploadBytes: 0, captcha: false },
};

const el = {
  main: document.getElementById("main"),
  nav: document.getElementById("board-nav"),
  // The links to the site's own pages — Verify in the header, Rules and FAQ in
  // the footer — each named by the view it opens.
  pageLinks: document.querySelectorAll("a[data-page]"),
  notice: document.getElementById("notice"),
  status: document.getElementById("route-status"),
  // The footer's invitation to verify, hidden until the server says the
  // feature is switched on here. The header's Verify link stays regardless:
  // the page also holds Forget this browser.
  verifyFooter: document.querySelector("[data-verify-footer]"),
};

/* API ---------------------------------------------------------------------- */

// UNREACHABLE is what a request that got no answer at all says: fetch's own
// error is a browser's wording ("Failed to fetch", "Load failed", "NetworkError
// when attempting to fetch resource") and says nothing a reader can use.
const UNREACHABLE = "Couldn't reach educhan. Check your connection and try again.";

function unreachableError() {
  const err = new Error(UNREACHABLE);
  err.unreachable = true;
  return err;
}

// The server's own words for the two refusals of a post's image that the form
// recovers from by taking the image off. web_invariants_test.go checks them
// against what the server answers.
const IMAGE_LIMIT_REACHED = "image limit reached";
const UPLOADS_SWITCHED_OFF = "image uploads are switched off on this server";
// UPLOADS_OFF_REASON is why a form on this site has no image field, or why the
// one it had was switched off.
const UPLOADS_OFF_REASON = "This site doesn't take images.";

async function api(path, options = {}) {
  // The caller's headers are copied rather than used in place: this function
  // adds to them, and a caller passing the same object twice should not
  // accumulate whatever the first call decided.
  const init = { ...options, headers: { ...(options.headers || {}) } };
  // FormData sets its own multipart content type, boundary included.
  if (init.body !== undefined && !(init.body instanceof FormData)) {
    init.headers["Content-Type"] = "application/json";
  }

  let res;
  try {
    res = await fetch(path, init);
  } catch {
    // No answer at all: offline, a dropped connection, a blocked request, or
    // one given up when its signal fired (verifyDeadline).
    throw unreachableError();
  }
  // A request given up while its answer was still coming in has had no answer
  // either, however much of it came: its body ends unread.
  const payload =
    res.status === 204
      ? null
      : await res.json().catch(() => {
          if (init.signal && init.signal.aborted) throw unreachableError();
          return null;
        });
  if (!res.ok) {
    const err = new Error((payload && payload.error) || `${res.status} ${res.statusText}`);
    err.status = res.status;
    err.payload = payload;
    err.headers = res.headers;
    throw err;
  }
  return payload;
}

// errorMessage turns an API failure into something worth reading. The server's
// own message is kept; only the cases where it needs context are rewritten.
// What was being sent stays in its form after a refusal, as far as the form
// still holds it, so these say what it holds: hasText, whether it holds text,
// hasImage, whether it holds an image, and imageOff, why the image is gone when
// a redraw while the post was held had release take it off ("" when it did not:
// holdForm). The caller adds that to anything but the two refusals of the image
// itself (imageTakenOff). A form holds what it sent at the answer, but for what
// release took off; a notice worded later, by a redraw or in place
// (rewordRefusal), reads what the reader has left in it since, its text erased,
// say, or its image taken out. action is what was refused — "post", or "code"
// for a verification code request — so a cooldown tells the reader to do that
// again, not something they were not doing. kept is false for a refusal said
// over a form that was not the one sent, and so does not hold what was written:
// a Verify step drawn again while its request was out (sayVerifyRefusal).
function errorMessage(err, { hasText = true, hasImage = true, imageOff = "", action = "post", kept = true } = {}) {
  // Nothing is left of the post, neither text nor image: an image-only post
  // whose image release took off, say, or a text-only post whose text the
  // reader has erased since.
  const nothingLeft = !hasText && (imageOff !== "" || !hasImage);
  if (err.unreachable) {
    return nothingLeft || !kept
      ? UNREACHABLE
      : hasText
        ? `${UNREACHABLE} What you wrote is still in the box.`
        : `${UNREACHABLE} Your image is still attached.`;
  }
  if (err.status === 413) {
    // An image taken off since cannot be swapped for a smaller one.
    return imageOff ? "That image was too large." : imageTooLargeMessage();
  }
  if (err.status === 429) {
    const seconds = err.payload && err.payload.retry_after;
    if (!seconds) {
      return err.message;
    }
    const wait = plural(seconds, "second", "seconds");
    return action === "code"
      ? `Codes can't be requested that often. Wait ${wait}, then request a code again.`
      : `Flood control: wait ${wait} and post again.`;
  }
  // The form stays on screen after a refusal, so these say what happened and
  // leave the half-written post where it is.
  if (err.status === 403 && err.message === IMAGE_LIMIT_REACHED) {
    return hasText
      ? "This thread reached its image limit while you were writing, so the image has been taken off your reply. Post again to send the text on its own."
      : "This thread reached its image limit while you were writing, so it takes no more images. Write a reply to post text instead.";
  }
  // A page drawn before it knew, or one whose server changed its mind since:
  // the refusal comes before anything is counted against the poster, so the
  // text can go straight away.
  if (err.status === 403 && err.message === UPLOADS_SWITCHED_OFF) {
    return hasText
      ? `${UPLOADS_OFF_REASON} The image has been taken off your post. Post again to send the text on its own.`
      : `${UPLOADS_OFF_REASON} Write something to post text instead.`;
  }
  // The captcha refused the fresh answer sent after it refused the first, which
  // a working server does not: nothing was posted or sent, and pressing the
  // button again solves another. A first refusal is answered by that second
  // send or, when the form has gone or the browser was forgotten since the
  // press, by an error marked notSent instead, which the callers answer without
  // coming here, but for a post whose form had gone, in a browser not forgotten
  // since the press (below).
  if (err.status === 403 && err.payload && err.payload.captcha) {
    if (action === "code") {
      return "The anti-spam check didn't go through, so no code was sent. Request a code again.";
    }
    if (nothingLeft) {
      return "The anti-spam check didn't go through, so nothing was posted.";
    }
    return hasText
      ? "The anti-spam check didn't go through, so nothing was posted. Your text is still in the box; post again."
      : "The anti-spam check didn't go through, so nothing was posted. Your image is still attached; post again.";
  }
  // A post not sent because its form had left the screen, the reader having
  // gone to another page, is told so only by a redraw that carries its form
  // held (captureDraft), after a post link's lookup into its thread say, as it
  // puts its values back into its own page (restoreDraft) or keeps that page
  // (render): on screen it would have gone, so it is told that nothing was
  // posted and, unless it is left with nothing to post, neither text nor image,
  // to post again.
  if (err.notSent) {
    if (nothingLeft) {
      return "You left this page before the post went through, so nothing was posted.";
    }
    return hasText
      ? "You left this page before the post went through, so nothing was posted. Your text is still in the box; post again."
      : "You left this page before the post went through, so nothing was posted. Your image is still attached; post again.";
  }
  if (err.status === 403 && err.message === "thread is archived") {
    const kept = hasText ? " Your text is still in the box." : "";
    return `This thread was archived while you were writing, so it no longer takes replies.${kept}`;
  }
  if (err.status === 403) {
    return `${err.message}.`;
  }
  return err.message;
}

// imageTakenOff follows a notice about a post with why its image is gone, when
// release took it off (reason, the note the picker was switched off with:
// holdForm), and tells a post left with nothing to send to write something
// instead. A post left with its text needs nothing more: the next press sends
// the text on its own. A refusal of the thread itself (closed: archived,
// locked, or gone) leaves nothing a post can do there, so the notice says only
// that the image was taken off: the picker's reason, which says text replies
// still work, and the advice to write one would point at a press the server
// will refuse again.
function imageTakenOff(message, reason, { hasText, isThread, closed = false }) {
  if (!reason) {
    return message;
  }
  // The server's own words, passed on as they are, end without a full stop.
  const said = /[.!?]$/.test(message) ? message : `${message}.`;
  if (closed) {
    return `${said} The image has been taken off your post.`;
  }
  const next = hasText ? "" : isThread ? " Write something to post text instead." : " Write a reply to post text instead.";
  return `${said} The image has been taken off your post. ${reason}${next}`;
}

/* Storage ------------------------------------------------------------------ */

// localStorage throws rather than degrading in some privacy modes. A browser
// that will not keep a value should cost only what the value was for — a
// remembered name, an age screen asked again next visit — never the page.
function storageGet(key) {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function storageSet(key, value) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // Not kept; whatever it was for is asked for or made again next time.
  }
}

function storageRemove(key) {
  try {
    localStorage.removeItem(key);
  } catch {
    // Nothing was stored, so nothing needs removing.
  }
}

/* Rendering helpers -------------------------------------------------------- */

const ESCAPES = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };

function esc(value) {
  return String(value ?? "").replace(/[&<>"']/g, (c) => ESCAPES[c]);
}

// boardPath is the one place a board's address is decided; every link to a
// board, thread, or post is built on it, the post-text markup included.
function boardPath(slug) {
  if (slug.startsWith(INSTITUTION_PREFIX)) {
    return `/u/${encodeURIComponent(slug.slice(INSTITUTION_PREFIX.length))}/`;
  }
  return `/${encodeURIComponent(slug)}/`;
}

// boardLabel is how a board is named on screen: its path, so what a reader
// sees is what they would type. Unescaped, like any other value from the API.
function boardLabel(slug) {
  return slug.startsWith(INSTITUTION_PREFIX) ? `/u/${slug.slice(INSTITUTION_PREFIX.length)}/` : `/${slug}/`;
}

function threadPath(slug, no) {
  return `${boardPath(slug)}thread/${no}`;
}

// timeHTML keeps timestamps short enough for a phone; the full date stays in
// the tooltip and in the machine-readable attribute.
function timeHTML(iso) {
  const date = new Date(iso);
  const sameYear = date.getFullYear() === new Date().getFullYear();
  const short = date.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    year: sameYear ? undefined : "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
  return `<time datetime="${esc(iso)}" title="${esc(date.toLocaleString())}">${esc(short)}</time>`;
}

function plural(count, one, many) {
  return `${count} ${count === 1 ? one : many}`;
}

function fileSize(bytes) {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  if (bytes >= 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${bytes} B`;
}

// uploadLimitText is the largest image this server takes, in words ("8 MB"),
// or "" while that is unknown. The limit is the server's, the same for every
// board, so nothing that uses it names a board.
function uploadLimitText() {
  const max = state.site.maxUploadBytes;
  return max > 0 ? fileSize(max).replace(".0 ", " ") : "";
}

// imageTooLargeMessage is what a file over the limit is told, whether the page
// caught it when it was picked or the server's 413 did.
function imageTooLargeMessage() {
  const limit = uploadLimitText();
  return limit
    ? `That image is too large: images can be up to ${limit}. Try a smaller file.`
    : "That image is too large. Try a smaller file.";
}

function showNotice(message) {
  el.notice.innerHTML =
    '<span class="notice-text"></span>' +
    '<button class="notice-dismiss" type="button" data-action="dismiss-notice" aria-label="Dismiss">&times;</button>';
  el.notice.querySelector(".notice-text").textContent = message;
  el.notice.hidden = false;
}

function clearNotice() {
  el.notice.hidden = true;
  el.notice.textContent = "";
}

// noticeSaid is what the notice on screen says, or "" when none is up.
function noticeSaid() {
  return el.notice.hidden ? "" : el.notice.querySelector(".notice-text").textContent;
}

// announce names the page for assistive tech, which otherwise gets no signal
// that a client-side navigation replaced the content.
function announce(title) {
  document.title = title === "educhan" ? title : `${title} — educhan`;
  el.status.textContent = title;
}

/* Post markup -------------------------------------------------------------- */

// Post bodies are stored exactly as typed and get their markup here, at
// display time:
//
//   >text                 greentext, on a line whose text starts with ">"
//   >>123                 a quote link, live when 123 is a post on this board
//   >>>/math/             a link to another board, >>>/math/123 to a post on
//                         it, and >>>/u/iitb/123 to one on an institution board
//   [spoiler]…[/spoiler]  hidden until hovered, focused or tapped
//   [code]…[/code]        a code block, verbatim and highlighted
//   [math]…[/math]        inline maths in TeX, typeset by KaTeX
//   [eqn]…[/eqn]          display maths: an equation set on its own line
//
// Each tag pairs with the first closing tag of its kind after it. A tag that
// never closes, an empty pair, and a spoiler opened inside another stay the
// literal text that was typed, so no input can render as half an element. Code
// and maths are opaque: nothing inside a [code], [math] or [eqn] block is
// markup, and extractQuotes in text.go follows the same pairing so that a code
// sample — `x >>2` — creates no backlink.

// The opaque blocks, in the order a tie would be settled (none can be, as no
// opening tag is a prefix of another). opaqueTags in text.go lists the same.
const OPAQUE_TAGS = [
  { kind: "code", open: "[code]", close: "[/code]" },
  { kind: "math", open: "[math]", close: "[/math]" },
  { kind: "eqn", open: "[eqn]", close: "[/eqn]" },
];
const SPOILER_OPEN = "[spoiler]";
const SPOILER_CLOSE = "[/spoiler]";

// INLINE_LINK finds the links in a run of text: a cross-board link first, so
// ">>>/math/123" is never read as ">" plus a quote, then a quote. Slugs follow
// boardSlugPattern in boards.go, and ">>>/u/iitb/" names the stored slug
// "u-iitb". LINE_LINK is the same pattern anchored, for asking whether a line
// opens with a link.
const INLINE_LINK = />>>\/(?:(u)\/)?([a-z0-9]+(?:-[a-z0-9]+)*)\/(\d+)?|>>(\d+)/g;
const LINE_LINK = new RegExp(INLINE_LINK.source, "y");
const MAX_SLUG_LENGTH = 24; // maxBoardSlugLen in boards.go
const MAX_POST_DIGITS = 18; // what an int64 post number can have, as in text.go

// The name a hidden spoiler has for assistive tech. Its text is aria-hidden
// until it is revealed, so a screen reader announces that something is hidden
// rather than reading it out — the same choice a sighted reader gets.
const SPOILER_LABEL = "Hidden spoiler: activate to reveal";

// parseMarkup splits a body into text runs, opaque blocks (code and maths), and
// spoilers. Runs are kept as offsets into the source, so rendering can still
// tell where a line starts. A spoiler holds text runs and opaque blocks;
// nothing nests deeper.
function parseMarkup(source) {
  // Pass one finds the tags. An opaque tag counts only when its closing tag
  // follows with something in between, and it claims everything up to that
  // close — tags included — so the scan jumps straight past it. Checking
  // against each kind's last close first keeps a body full of unclosed
  // openings linear.
  const tags = [];
  const lastCloses = OPAQUE_TAGS.map(({ close }) => source.lastIndexOf(close));
  scan: for (let i = source.indexOf("["); i !== -1; i = source.indexOf("[", i)) {
    for (const [n, { kind, open, close }] of OPAQUE_TAGS.entries()) {
      const inner = i + open.length;
      if (!source.startsWith(open, i) || inner > lastCloses[n]) {
        continue;
      }
      const at = source.indexOf(close, inner);
      if (at > inner) {
        tags.push({ kind, at: i, end: at + close.length, inner: [inner, at] });
        i = at + close.length;
        continue scan;
      }
    }
    if (source.startsWith(SPOILER_OPEN, i)) {
      tags.push({ kind: "open", at: i, end: i + SPOILER_OPEN.length });
      i += SPOILER_OPEN.length;
      continue;
    } else if (source.startsWith(SPOILER_CLOSE, i)) {
      tags.push({ kind: "close", at: i, end: i + SPOILER_CLOSE.length });
      i += SPOILER_CLOSE.length;
      continue;
    }
    i += 1;
  }

  // Pass two pairs the spoilers. One opens only if a close comes after it and
  // the pair is not empty; until that close, further opens are literal text.
  let lastClose = -1;
  tags.forEach((tag, n) => {
    if (tag.kind === "close") lastClose = n;
  });

  const root = [];
  let into = root;
  let pos = 0;
  const text = (end) => {
    if (end > pos) into.push({ kind: "text", start: pos, end });
  };
  tags.forEach((tag, n) => {
    if (tag.inner) {
      text(tag.at);
      into.push({ kind: tag.kind, start: tag.inner[0], end: tag.inner[1] });
      pos = tag.end;
    } else if (tag.kind === "open") {
      const next = tags[n + 1];
      const empty = next && next.kind === "close" && next.at === tag.end;
      if (into === root && n < lastClose && !empty) {
        text(tag.at);
        const spoiler = { kind: "spoiler", children: [] };
        root.push(spoiler);
        into = spoiler.children;
        pos = tag.end;
      }
    } else if (into !== root) {
      text(tag.at);
      into = root;
      pos = tag.end;
    }
  });
  text(source.length);
  return root;
}

// Code blocks and display maths are blocks. A spoiler holding one is laid out
// as a block itself, so it hides the whole box rather than a run of text that
// the block breaks out of.
function isBlockNode(node) {
  return node.kind === "code" || node.kind === "eqn" || (node.kind === "spoiler" && node.children.some(isBlockNode));
}

// crossLinkHref turns a cross-board match into the SPA route it links to, or
// null when it is not a link after all: a reserved path, a slug too long to be
// a board, or a number too long to be a post.
function crossLinkHref(match) {
  const [, institution, name, no] = match;
  const slug = institution ? `u-${name}` : name;
  // A reserved path is never a board, so a link to one would claim a board
  // that cannot exist.
  if (slug.length > MAX_SLUG_LENGTH || RESERVED_PATHS.has(slug) || (no && no.length > MAX_POST_DIGITS)) {
    return null;
  }
  return no ? `${boardPath(slug)}post/${no}` : boardPath(slug);
}

// lineIsGreen reports whether the line whose text starts at offset is
// greentext: it starts with ">", but not with a link. A line that opens with
// ">>123" or ">>>/math/" is a reference, as it always was for quotes.
function lineIsGreen(source, offset) {
  if (source[offset] !== ">") {
    return false;
  }
  LINE_LINK.lastIndex = offset;
  const match = LINE_LINK.exec(source);
  return !match || (match[4] === undefined && crossLinkHref(match) === null);
}

// linkHTML renders one INLINE_LINK match, or returns null to leave it as text.
// Links inside a hidden spoiler are out of the tab order until it is revealed:
// focus must not land on something the reader cannot see.
function linkHTML(match, ctx, hidden) {
  const tab = hidden ? ' tabindex="-1"' : "";
  const quote = match[4];
  if (quote !== undefined) {
    const no = Number(quote);
    const threadNo = ctx.targets.get(no);
    if (threadNo === undefined) {
      return `<a class="quotelink dead" href="#" title="post not found"${tab}>${esc(match[0])}</a>`;
    }
    return `<a class="quotelink" href="${esc(`${threadPath(ctx.slug, threadNo)}#p${no}`)}"${tab}>${esc(match[0])}</a>`;
  }
  const href = crossLinkHref(match);
  return href === null ? null : `<a class="quotelink crosslink" href="${esc(href)}"${tab}>${esc(match[0])}</a>`;
}

function inlineHTML(text, ctx, hidden) {
  let html = "";
  let last = 0;
  for (const match of text.matchAll(INLINE_LINK)) {
    const link = linkHTML(match, ctx, hidden);
    if (link !== null) {
      html += esc(text.slice(last, match.index)) + link;
      last = match.index + match[0].length;
    }
  }
  return html + esc(text.slice(last));
}

// textHTML renders source[start:end] line by line. Whether a line is greentext
// is settled by its first character of text, wherever that falls — after a
// newline, after a code block, or just inside a spoiler — and holds to the end
// of the line, across spoiler edges. Each run is wrapped on its own, so a
// greentext span never straddles a spoiler's.
function textHTML(start, end, ctx, hidden) {
  let offset = start;
  ctx.source
    .slice(start, end)
    .split("\n")
    .forEach((line, n) => {
      if (n > 0) {
        ctx.out.push("\n");
        ctx.lineStart = true;
      }
      if (line) {
        if (ctx.lineStart) {
          ctx.green = lineIsGreen(ctx.source, offset);
          ctx.lineStart = false;
        }
        const html = inlineHTML(line, ctx, hidden);
        ctx.out.push(ctx.green ? `<span class="greentext">${html}</span>` : html);
      }
      offset += line.length + 1;
    });
}

// renderMarkup writes a parsed node list into ctx.out. The body is laid out
// with white-space: pre-wrap, so a line break beside a block would show as a
// blank line: the one newline that separates a block from its neighbours is
// dropped, as is the one just inside each end of a block.
function renderMarkup(nodes, ctx, { block = false, hidden = false } = {}) {
  const { source, out } = ctx;
  nodes.forEach((node, k) => {
    if (node.kind === "code") {
      // Focusable, because a long line scrolls sideways and a keyboard has to
      // be able to scroll it — except inside a hidden spoiler, where, as for a
      // link, focus would land on something the reader cannot see, and show
      // it to them through :focus-within without their asking. revealSpoiler
      // puts it back in the tab order.
      out.push(`<pre class="code" tabindex="${hidden ? -1 : 0}"><code>${esc(blockSource(source, node))}</code></pre>`);
      ctx.lineStart = true;
      return;
    }
    if (node.kind === "eqn") {
      out.push(mathHTML(blockSource(source, node), { display: true, hidden }));
      ctx.lineStart = true;
      return;
    }
    if (node.kind === "math") {
      // Inline maths is part of its line: a line that opens with it is not
      // greentext, and in a greentext line it is green with the rest.
      if (ctx.lineStart) {
        ctx.green = false;
        ctx.lineStart = false;
      }
      const html = mathHTML(source.slice(node.start, node.end), { display: false });
      out.push(ctx.green ? `<span class="greentext">${html}</span>` : html);
      return;
    }
    if (node.kind === "spoiler") {
      const spoilerBlock = isBlockNode(node);
      out.push(
        `<span class="spoiler${spoilerBlock ? " spoiler-block" : ""}" role="button" tabindex="0" ` +
          `aria-label="${esc(SPOILER_LABEL)}"><span class="spoiler-text" aria-hidden="true">`
      );
      renderMarkup(node.children, ctx, { block: spoilerBlock, hidden: true });
      out.push("</span></span>");
      if (spoilerBlock) ctx.lineStart = true;
      return;
    }
    let { start, end } = node;
    const before = k > 0 ? isBlockNode(nodes[k - 1]) : block;
    const after = k < nodes.length - 1 ? isBlockNode(nodes[k + 1]) : block;
    if (before && source[start] === "\n") start += 1;
    if (after && end > start && source[end - 1] === "\n") end -= 1;
    textHTML(start, end, ctx, hidden);
  });
}

// blockSource is a block's text without the one newline just inside each end,
// so a block written on lines of its own does not open or close on a blank one.
function blockSource(source, { start, end }) {
  if (source[start] === "\n") start += 1;
  if (end > start && source[end - 1] === "\n") end -= 1;
  return source.slice(start, end);
}

// mathHTML is what a [math] or [eqn] block renders as until KaTeX typesets it:
// the TeX, escaped, in data-tex for typesetMaths to read, and as the text, so a
// reader whose KaTeX never arrives still reads the source. Display maths
// scrolls sideways when it is wider than the post, so, like a code block, it is
// focusable for a keyboard to scroll it, unless a hidden spoiler holds it.
function mathHTML(tex, { display, hidden = false }) {
  return display
    ? `<div class="math math-display" tabindex="${hidden ? -1 : 0}" data-tex="${esc(tex)}">${esc(tex)}</div>`
    : `<span class="math" data-tex="${esc(tex)}">${esc(tex)}</span>`;
}

// bodyHTML renders a post's text with its markup. Every piece of the source
// reaches the page through esc(); the only markup is what is written here.
function bodyHTML(post, slug) {
  const ctx = {
    source: post.body,
    slug,
    targets: new Map((post.quotes || []).map((q) => [q.no, q.thread_no])),
    out: [],
    lineStart: true,
    green: false,
  };
  renderMarkup(parseMarkup(post.body), ctx);
  return ctx.out.join("");
}

/* Post enhancement --------------------------------------------------------- */

// enhancePosts finishes rendered posts once they are in the document: code
// blocks are highlighted here, and maths typeset. Every view that inserts posts
// calls it on what it inserted. It is safe on any element and as often as
// wanted — work already done is skipped — and nothing it does can fail a
// render: a block it cannot finish keeps its plain, escaped rendering.
function enhancePosts(root) {
  if (!root) {
    return;
  }
  highlightCode(root);
  typesetMaths(root);
}

const HIGHLIGHTER_SRC = "/lib/highlightjs/highlight.min.js";
// Auto-detection tries every candidate language on a block, so the candidates
// are kept to what students post: fewer passes, and fewer wild guesses.
const HIGHLIGHT_LANGUAGES = [
  "bash", "c", "cpp", "csharp", "css", "diff", "go", "java", "javascript", "json",
  "kotlin", "lua", "makefile", "markdown", "php", "python", "r", "ruby", "rust",
  "shell", "sql", "swift", "typescript", "xml", "yaml",
];
// On a longer block the language is detected from its opening stretch alone and
// the whole block then highlighted in that one language: detection is the
// expensive part, and it costs a pass per candidate over whatever it is given.
const HIGHLIGHT_DETECT_CHARS = 1000;
// A block longer than this stays plain. One highlight.js call cannot be
// interrupted, and several of the grammars (java, typescript, c and more) take
// time that grows with the square of a long run of identifier characters: a
// post-length block of one took the better part of a second on a desktop and
// several on a phone, for each such reply in a thread. At this length the worst
// case is tens of milliseconds, and a code block a student posts is shorter.
const HIGHLIGHT_MAX_CHARS = 2000;
// Highlighting yields to the page once a batch has run this long, so a thread
// full of code never stalls scrolling or typing.
const HIGHLIGHT_SLICE_MS = 12;

let highlighter = null;

// loadHighlighter fetches highlight.js the first time a code block is on
// screen, so a reader who never meets one never downloads it. It is a
// same-origin script element, which script-src 'self' allows. A failed load is
// not retried until the page is reloaded: plain code is still readable, and one
// failure is enough to report.
function loadHighlighter() {
  if (!highlighter) {
    highlighter = new Promise((resolve, reject) => {
      const script = document.createElement("script");
      script.src = HIGHLIGHTER_SRC;
      script.onload = () => (window.hljs ? resolve(window.hljs) : reject(new Error("highlight.js did not load")));
      script.onerror = () => reject(new Error("highlight.js did not load"));
      document.head.append(script);
    }).then((hljs) => {
      hljs.configure({ languages: HIGHLIGHT_LANGUAGES });
      return compileGrammars(hljs);
    });
  }
  return highlighter;
}

// compileGrammars prepares every candidate language ahead of the first
// detection. highlight.js compiles a grammar the first time it is used, and
// detection uses them all at once — close to a second of frozen page on a
// mid-range phone. One at a time, yielding between batches, the same work
// leaves the page responsive while the first code block waits for it.
function compileGrammars(hljs) {
  const pending = [...HIGHLIGHT_LANGUAGES];
  return new Promise((resolve) => {
    const step = () => {
      const deadline = performance.now() + HIGHLIGHT_SLICE_MS;
      while (pending.length > 0 && performance.now() < deadline) {
        try {
          hljs.highlight("", { language: pending.shift(), ignoreIllegals: true });
        } catch {
          // Detection simply goes without a grammar that will not compile.
        }
      }
      if (pending.length > 0) {
        setTimeout(step, 0);
      } else {
        resolve(hljs);
      }
    };
    step();
  });
}

// Like maths, a block is highlighted only once it comes within a screen of the
// viewport (TYPESET_AHEAD): a thread can hold hundreds of code blocks, and the
// reader may never scroll to most of them. That is also when highlight.js is
// first fetched.
let codeObserver = null;
// Code blocks being watched that have not yet come near the viewport.
const watchedCode = new Set();
// Blocks near the viewport, waiting for a batch.
const highlightQueue = [];
let highlightPending = false;

function highlightCode(root) {
  const blocks = root.querySelectorAll("pre.code > code:not([data-highlighted])");
  if (blocks.length === 0) {
    return;
  }
  if (!codeObserver) {
    codeObserver = new IntersectionObserver(
      (entries) => {
        for (const { target, isIntersecting } of entries) {
          if (!isIntersecting) {
            continue;
          }
          codeObserver.unobserve(target);
          watchedCode.delete(target);
          highlightQueue.push(target);
        }
        scheduleHighlight();
      },
      { rootMargin: TYPESET_AHEAD }
    );
  }
  // The observer holds on to what it watches, so blocks a re-render replaced
  // before they came near are let go here.
  for (const code of watchedCode) {
    if (!code.isConnected) {
      codeObserver.unobserve(code);
      watchedCode.delete(code);
    }
  }
  for (const code of blocks) {
    if (code.textContent.length > HIGHLIGHT_MAX_CHARS) {
      code.dataset.highlighted = "skipped";
    } else if (!watchedCode.has(code)) {
      watchedCode.add(code);
      codeObserver.observe(code);
    }
  }
}

function scheduleHighlight() {
  if (highlightPending || highlightQueue.length === 0) {
    return;
  }
  highlightPending = true;
  loadHighlighter().then(
    (hljs) => setTimeout(() => highlightBatch(hljs), 0),
    () => {
      // The blocks keep their plain rendering.
      highlightQueue.length = 0;
      highlightPending = false;
    }
  );
}

function highlightBatch(hljs) {
  const deadline = performance.now() + HIGHLIGHT_SLICE_MS;
  while (highlightQueue.length > 0 && performance.now() <= deadline) {
    const code = highlightQueue.shift();
    // A re-render may have replaced the block since it was queued, and an
    // earlier pass may already have done it; highlight.js complains about both.
    if (code.isConnected && !code.dataset.highlighted) {
      highlightBlock(hljs, code);
    }
  }
  if (highlightQueue.length > 0) {
    setTimeout(() => highlightBatch(hljs), 0);
  } else {
    highlightPending = false;
  }
}

function highlightBlock(hljs, code) {
  try {
    const text = code.textContent;
    if (text.length > HIGHLIGHT_DETECT_CHARS) {
      const guess = hljs.highlightAuto(text.slice(0, HIGHLIGHT_DETECT_CHARS)).language;
      code.classList.add(`language-${guess || "plaintext"}`);
    }
    hljs.highlightElement(code);
  } catch {
    code.dataset.highlighted = "failed";
  }
}

/* Maths -------------------------------------------------------------------- */

const KATEX_SRC = "/lib/katex/katex.min.js";
const KATEX_CSS = "/lib/katex/katex.min.css";

// TeX commands that are switched off in posts. maxExpand caps how many macro
// expansions an expression makes, not what each one produces, so a post that
// defines one long macro and uses it a thousand times builds millions of nodes
// in the tab of everyone who opens the thread. KaTeX's own macros are short and
// none repeats an argument, so with definitions gone the cap bounds the output
// too. The console commands go as well: \errmessage would write an error into
// every reader's console.
const KATEX_DISABLED = [
  "\\def", "\\gdef", "\\edef", "\\xdef", "\\newcommand", "\\renewcommand",
  "\\providecommand", "\\message", "\\errmessage", "\\show",
];
// Typesetting yields to the page once a batch has run this long, as
// highlighting does.
const TYPESET_SLICE_MS = 12;

let typesetter = null;

// loadTypesetter fetches KaTeX and its stylesheet the first time a post with
// maths is rendered, so a reader who never meets any never downloads them. Both are
// same-origin elements, which the CSP allows, and typesetting waits for both:
// KaTeX's output laid out without its stylesheet is a scramble. A failed load
// is not retried until the page is reloaded; the TeX stays readable.
function loadTypesetter() {
  if (!typesetter) {
    const script = new Promise((resolve, reject) => {
      const tag = document.createElement("script");
      tag.src = KATEX_SRC;
      tag.onload = () => (window.katex ? resolve(window.katex) : reject(new Error("KaTeX did not load")));
      tag.onerror = () => reject(new Error("KaTeX did not load"));
      document.head.append(tag);
    });
    const sheet = new Promise((resolve, reject) => {
      const link = document.createElement("link");
      link.rel = "stylesheet";
      link.href = KATEX_CSS;
      link.onload = () => resolve();
      link.onerror = () => reject(new Error("KaTeX's stylesheet did not load"));
      document.head.append(link);
    });
    typesetter = Promise.all([script, sheet]).then(([katex]) => {
      // A glyph's width is its font's, and the fonts load as glyphs need them.
      if (document.fonts) {
        document.fonts.addEventListener("loadingdone", scheduleFitMaths);
      }
      return katex;
    });
  }
  return typesetter;
}

// Typesetting waits until a block comes within a screen's height of the
// viewport. A long thread can hold a thousand formulas, and KaTeX's output is
// heavy: typesetting them all up front kept a mid-range phone busy for most of
// a minute, and made every frame after it slower, for maths the reader may
// never scroll to. A block typeset a screen ahead is ready before it is seen,
// and the change in its size lands off screen, where scroll anchoring keeps
// the reader's place.
const TYPESET_AHEAD = "100% 0px";

let mathsObserver = null;
// Blocks being watched that have not yet come near the viewport.
const watchedMaths = new Set();
// Blocks near the viewport, waiting for a batch: on screen first.
const typesetQueue = [];
let typesetPending = false;

function typesetMaths(root) {
  const blocks = root.querySelectorAll(".math:not([data-typeset])");
  if (blocks.length === 0) {
    return;
  }
  loadTypesetter()
    .then((katex) => watchMaths(katex, blocks))
    .catch(() => {
      // The maths keeps its TeX.
    });
}

function watchMaths(katex, blocks) {
  if (!mathsObserver) {
    mathsObserver = new IntersectionObserver(
      (entries) => {
        for (const { target, isIntersecting, boundingClientRect: box } of entries) {
          if (!isIntersecting) {
            continue;
          }
          mathsObserver.unobserve(target);
          watchedMaths.delete(target);
          if (box.bottom > 0 && box.top < window.innerHeight) {
            typesetQueue.unshift(target);
          } else {
            typesetQueue.push(target);
          }
        }
        scheduleTypeset(katex);
      },
      { rootMargin: TYPESET_AHEAD }
    );
  }
  // The observer holds on to what it watches, so blocks a re-render replaced
  // before they came near are let go here.
  for (const block of watchedMaths) {
    if (!block.isConnected) {
      mathsObserver.unobserve(block);
      watchedMaths.delete(block);
    }
  }
  for (const block of blocks) {
    if (!watchedMaths.has(block)) {
      watchedMaths.add(block);
      mathsObserver.observe(block);
    }
  }
}

function scheduleTypeset(katex) {
  if (!typesetPending && typesetQueue.length > 0) {
    typesetPending = true;
    setTimeout(() => typesetBatch(katex), 0);
  }
}

function typesetBatch(katex) {
  typesetPending = false;
  const deadline = performance.now() + TYPESET_SLICE_MS;
  while (typesetQueue.length > 0 && performance.now() <= deadline) {
    const block = typesetQueue.shift();
    // Gone in a re-render since it was queued, or done by an earlier pass.
    if (block.isConnected && !block.dataset.typeset) {
      typeset(katex, block);
    }
  }
  if (typesetQueue.length > 0) {
    scheduleTypeset(katex);
  } else {
    scheduleFitMaths();
  }
}

// typeset renders one placeholder in place with katex.render, which builds DOM
// nodes and gives them their styles through the CSSOM. renderToString and
// innerHTML would carry those styles as attributes, which the CSP refuses.
//
// Nothing in a post is trusted: TeX makes no links, images, classes, ids or
// data attributes, and \href, \url and the like show as unsupported commands. maxSize caps
// any size the TeX asks for at 20em, maxExpand caps macro expansion, and a TeX
// error renders in place as its source, in the error colour, with the message
// as its title. Anything that throws all the same is KaTeX failing outright —
// nesting too deep for the stack, say — and the block keeps its TeX.
function typeset(katex, block) {
  try {
    withStyleAttributesThroughCSSOM(() =>
      katex.render(block.dataset.tex, block, {
        displayMode: block.classList.contains("math-display"),
        throwOnError: false,
        trust: false,
        maxExpand: 1000,
        maxSize: 20,
        strict: "ignore",
        errorColor: "var(--danger)",
        macros: disabledCommands(katex),
      })
    );
    block.dataset.typeset = "done";
  } catch {
    block.textContent = block.dataset.tex;
    block.dataset.typeset = "failed";
  }
}

// fitWideMaths gives inline maths that cannot fit its post a line of its own
// that scrolls sideways, as display maths has. KaTeX breaks a long formula only
// between its top-level pieces, so a single piece wider than the post — a long
// fraction inline, say — would otherwise push the whole page sideways on a
// phone. Pieces keep their width whatever the viewport, but the room for them
// does not, so this runs again when the window resizes or KaTeX's fonts arrive.
function fitWideMaths() {
  const blocks = Array.from(document.querySelectorAll(".math[data-typeset='done']:not(.math-display)"));
  // Every measurement first, then every change, so the page lays out once.
  const wide = blocks.map((block) => {
    const body = block.closest(".post-body");
    const room = body ? body.clientWidth : Infinity;
    return Array.from(block.querySelectorAll(".katex-html > .katex-base")).some((base) => base.getBoundingClientRect().width > room);
  });
  blocks.forEach((block, n) => {
    if (wide[n] === block.classList.contains("math-wide")) {
      return;
    }
    block.classList.toggle("math-wide", wide[n]);
    // Focusable while it scrolls, for a keyboard to scroll it — but not in a
    // hidden spoiler, until revealSpoiler makes it so.
    if (wide[n]) {
      block.tabIndex = block.closest(".spoiler:not(.revealed)") ? -1 : 0;
    } else {
      block.removeAttribute("tabindex");
    }
  });
}

let fitPending = false;

function scheduleFitMaths() {
  if (!fitPending) {
    fitPending = true;
    requestAnimationFrame(() => {
      fitPending = false;
      fitWideMaths();
    });
  }
}

window.addEventListener("resize", () => {
  if (typesetter) {
    scheduleFitMaths();
  }
});

// disabledCommands is a fresh macro table for one expression, defining each of
// KATEX_DISABLED as an error. User macros are consulted before KaTeX's own
// commands, so these stand in front of the real ones, and fresh because KaTeX
// writes \global definitions back into the table it is given: one post's
// \global\let must never reach another post's maths.
//
// KaTeX builds \nonumber (and \notag, which is \nonumber) and \tag, \tag* on
// \gdef, so with \gdef refused they would fail with an error naming a command
// the poster never typed. The table replaces the two macros that call it with
// ones that set the same values directly: \@eqnsw, which says whether a line
// of an equation or align is numbered, and \df@tag, the tag \tag gives it —
// taken once, as \gdef\df@tag{\text{#1}} would, so no expansion repeats.
function disabledCommands(katex) {
  const macros = {};
  for (const name of KATEX_DISABLED) {
    macros[name] = () => {
      throw new katex.ParseError(`${name} is not available in posts`);
    };
  }
  macros["\\nonumber"] = (context) => {
    context.macros.set("\\@eqnsw", "0", true);
    return "";
  };
  macros["\\tag@literal"] = (context) => {
    if (context.macros.get("\\df@tag")) {
      throw new katex.ParseError("Multiple \\tag");
    }
    // The argument's tokens come in stack order, last first, and the start
    // token is a token like any other, which gives the constructor for the
    // three around them.
    const { tokens, start } = context.consumeArg();
    const Token = start.constructor;
    context.macros.set(
      "\\df@tag",
      { tokens: [new Token("}"), ...tokens, new Token("{"), new Token("\\text")], numArgs: 0 },
      true
    );
    return "";
  };
  return macros;
}

// KaTeX sets most styles through the CSSOM, but a few as a style attribute: the
// width of the SVG it draws for \vec, \oiint or a tall brace (as in a long
// \begin{cases}), styles in the MathML for \pmb and \fcolorbox, and the colour
// of an expression it could not parse. The CSP refuses a style attribute set
// from script just as it refuses one in markup, so while KaTeX builds — which
// it does synchronously, with nothing else running — setAttribute("style") is
// sent to the element's CSSOM declarations instead: the same styles, by the
// route the CSP allows. The original is back before anything else can run.
function withStyleAttributesThroughCSSOM(build) {
  const setAttribute = Element.prototype.setAttribute;
  Element.prototype.setAttribute = function setAttributeThroughCSSOM(name, value) {
    if (String(name).toLowerCase() === "style" && this.style) {
      this.style.cssText = value;
      return undefined;
    }
    return setAttribute.call(this, name, value);
  };
  try {
    build();
  } finally {
    Element.prototype.setAttribute = setAttribute;
  }
}

/* Spoilers ----------------------------------------------------------------- */

// revealSpoiler shows a spoiler for good: its text joins the accessibility
// tree, its links, code blocks and wide maths rejoin the tab order, and it
// stops being a button. It stays focusable (tabindex -1) so focus is not
// dropped when it was the thing focused.
function revealSpoiler(spoiler) {
  spoiler.classList.add("revealed");
  spoiler.removeAttribute("role");
  spoiler.removeAttribute("aria-label");
  spoiler.tabIndex = -1;
  const text = spoiler.querySelector(":scope > .spoiler-text");
  if (text) {
    text.removeAttribute("aria-hidden");
  }
  spoiler.querySelectorAll("a[tabindex]").forEach((link) => link.removeAttribute("tabindex"));
  spoiler.querySelectorAll("pre.code, .math-display, .math-wide").forEach((block) => {
    block.tabIndex = 0;
  });
}

// The pointer behind the most recent press, so a click can tell a tap — which
// had no hover to show the spoiler first — from a mouse that has already seen it.
let lastPointerType = "mouse";

document.addEventListener(
  "pointerdown",
  (event) => {
    lastPointerType = event.pointerType || "mouse";
  },
  { capture: true, passive: true }
);

// A hidden spoiler in post text. The selector stays inside .post-body because
// a spoilered image's wrapper is a .spoiler too, and its tile is the page's
// own click handler's to reveal: matched here, a tap on it would be cancelled.
const TEXT_SPOILER = ".post-body .spoiler:not(.revealed)";

// Registered ahead of the page's own click handler, which skips any event this
// one has handled. A tap on a hidden spoiler only reveals it: following a link
// inside it would follow one the reader has not seen. A mouse click reveals it
// too, but the hover already showed it, so a link inside works as normal.
document.addEventListener("click", (event) => {
  const spoiler = event.target instanceof Element ? event.target.closest(TEXT_SPOILER) : null;
  if (!spoiler) {
    return;
  }
  revealSpoiler(spoiler);
  if (event.detail > 0 && lastPointerType !== "mouse") {
    event.preventDefault();
  }
});

// Focus shows a spoiler (in CSS); Enter or Space reveals it, as for any button.
document.addEventListener("keydown", (event) => {
  if (event.key !== "Enter" && event.key !== " ") {
    return;
  }
  const target = event.target;
  if (target instanceof Element && target.matches(TEXT_SPOILER)) {
    event.preventDefault();
    revealSpoiler(target);
  }
});

/* Post rendering ----------------------------------------------------------- */

function backlinksHTML(post, slug) {
  if (!post.backlinks || post.backlinks.length === 0) {
    return "";
  }
  const links = post.backlinks
    .map((ref) => `<a class="quotelink" href="${threadPath(slug, ref.thread_no)}#p${ref.no}">&gt;&gt;${ref.no}</a>`)
    .join("");
  return `<div class="backlinks"><span>${plural(post.backlinks.length, "reply", "replies")}:</span>${links}</div>`;
}

/* Attachments -------------------------------------------------------------- */

// thumbHTML always carries the thumbnail's intrinsic size so the browser can
// reserve the space before the image arrives, and keeps a copy in data-
// attributes for the collapse back from an expanded image.
function thumbHTML(att, { className = "", alt = null } = {}) {
  const label = alt === null ? `Attached image: ${att.name || "untitled"}` : alt;
  return `<img class="thumb${className ? ` ${className}` : ""}" src="${esc(att.thumb_url)}"
    width="${att.thumb_width}" height="${att.thumb_height}"
    data-tw="${att.thumb_width}" data-th="${att.thumb_height}"
    loading="lazy" decoding="async" alt="${esc(label)}" />`;
}

// attachmentHTML renders a post's image. Inside a thread the thumbnail expands
// in place, which is the classic imageboard behaviour; elsewhere the full image
// opens in a new tab so the surrounding link still navigates.
function attachmentHTML(att, { expandable = false } = {}) {
  if (!att) {
    return "";
  }
  const caption = att.name
    ? `<div class="filename">${esc(att.name)} <span class="muted">(${att.width}&times;${att.height}, ${fileSize(att.size)})</span></div>`
    : "";
  const link = expandable
    ? `<a class="thumb-link expandable" href="${esc(att.url)}" data-full="${esc(att.url)}"
         data-thumb="${esc(att.thumb_url)}" aria-expanded="false"
         title="Expand image">${thumbHTML(att)}</a>`
    : `<a class="thumb-link" href="${esc(att.url)}" target="_blank" rel="noopener"
         title="Open the full image">${thumbHTML(att)}</a>`;
  if (!att.spoiler) {
    return `<div class="post-file">${caption}${link}</div>`;
  }
  // A spoilered image is a neutral tile until someone asks for it. The
  // filename waits behind it with the thumbnail — a name can give away as much
  // as a picture — and the lazy thumbnail is not fetched while it is hidden.
  // Once shown it is an ordinary attachment and expands like one.
  return `
    <div class="post-file spoiler">
      <button class="spoiler-tile" type="button" data-action="reveal-spoiler">Spoiler image, click to show</button>
      <div class="spoiler-content" hidden>${caption}${link}</div>
    </div>`;
}

function revealImageSpoiler(button) {
  const file = button.closest(".post-file");
  const content = file && file.querySelector(".spoiler-content");
  if (!content) {
    return;
  }
  content.hidden = false;
  file.classList.remove("spoiler");
  button.remove();
  // Focus follows the image, so a keyboard reader's next Enter expands it.
  const link = content.querySelector("a.thumb-link");
  if (link) link.focus();
}

// verifiedBadgeHTML renders the institution a post was signed with. It says
// where the poster is, and deliberately nothing else: two posts carrying the
// same badge are no more linked to each other than any two posts.
function verifiedBadgeHTML(label) {
  return `<span class="badge verified" title="Institutional email verified; not linked to this poster">Verified: ${esc(label)}</span>`;
}

// The image limit badge marks a thread that has filled up. On a board whose
// limit is 0 every thread is "at" it from the start, and a badge on all of them
// would say nothing; the reply form says why there is no picker instead. On a
// site that takes no images there is no cap to be at.
function badgesHTML(thread, board = null) {
  const badges = [];
  if (thread.sticky) badges.push('<span class="badge sticky">sticky</span>');
  if (thread.archived_at) badges.push('<span class="badge archived">archived</span>');
  if (thread.locked) badges.push('<span class="badge locked">locked</span>');
  if (thread.at_bump_limit) badges.push('<span class="badge bumplimit">bump limit</span>');
  if (state.site.uploads && thread.at_image_limit && !(board && board.image_limit === 0)) {
    badges.push('<span class="badge imagelimit">image limit</span>');
  }
  return badges.join(" ");
}

// postHTML renders one post. `context` carries the board slug, the thread the
// post belongs to, and whether the post number should act as a quote button.
function postHTML(post, context) {
  const { slug, board = null, subject = "", thread = null, quotable = false } = context;
  const head = [];

  // The subject is the opening post's heading, so it gets its own line above
  // the metadata rather than competing with it.
  if (post.op && subject) {
    head.push(`<h2 class="subject">${esc(subject)}</h2>`);
  }
  head.push(`<span class="name">${esc(post.name)}</span>`);
  if (post.tripcode) {
    head.push(`<span class="trip">${esc(post.tripcode)}</span>`);
  }
  if (post.badge) {
    head.push(verifiedBadgeHTML(post.badge));
  }
  head.push(`<span class="pid" title="Poster ID for this thread">ID: ${esc(post.poster_id)}</span>`);
  head.push(timeHTML(post.created_at));

  if (post.op && thread) {
    const badges = badgesHTML(thread, board);
    if (badges) head.push(badges);
  }

  const numberAttrs = quotable ? ` data-quote="${post.no}" title="Quote this post"` : "";
  head.push(`<a class="no mono" href="${threadPath(slug, post.thread_no)}#p${post.no}"${numberAttrs}>No.&nbsp;${post.no}</a>`);

  return `
    <article class="post ${post.op ? "op" : "reply"}" id="p${post.no}">
      <div class="post-head">${head.join("\n")}</div>
      ${attachmentHTML(post.attachment, { expandable: quotable })}
      <div class="post-body">${bodyHTML(post, slug)}</div>
      ${backlinksHTML(post, slug)}
    </article>`;
}

/* Forms -------------------------------------------------------------------- */

// Only one posting form is on screen at a time, so these ids are unique.
const FIELD_IDS = { subject: "f-subject", name: "f-name", body: "f-body", image: "f-image" };

// What a posting form says beside its button while the captcha is on: a form
// that offers the badge says a badged post skips the check, and one without
// the option, drawn so or with it since taken out (drawBadgeOption), does not.
const CHECK_HINT = "Your browser does a quick anti-spam check first.";
const CHECK_HINT_WITH_BADGE = "Unless the post carries your badge, your browser does a quick anti-spam check first.";

// fileFieldHTML draws the image picker. `blocked` is the reason it cannot be
// used, when there is one; the field then renders exactly as blockFileField
// leaves a live one, so a refusal mid-draft and a fresh render look the same.
function fileFieldHTML(blocked = "") {
  const limit = uploadLimitText();
  return `
    <div class="file-field${blocked ? " blocked" : ""}" data-file-field>
      <input class="file-input" type="file" id="${FIELD_IDS.image}" name="image"
             accept="${ACCEPTED_IMAGES}" aria-labelledby="f-image-label"
             aria-describedby="f-image-blocked"${blocked ? " disabled" : ""} />
      <label class="btn secondary file-button" for="${FIELD_IDS.image}">Choose image</label>
      <span class="file-hint">or drop one on this form &middot; JPEG, PNG, WebP, GIF${limit ? `, up to ${esc(limit)}` : ""}</span>
      <span class="file-blocked" id="f-image-blocked"${blocked ? "" : " hidden"}>${esc(blocked)}</span>
      <div class="file-preview" aria-live="polite" hidden>
        <img class="file-thumb" alt="" />
        <span class="file-meta"></span>
        <label class="sage spoiler-option"><input type="checkbox" name="spoiler" value="1" /> Spoiler image</label>
        <button class="btn subtle file-remove" type="button" data-action="clear-file">Remove</button>
      </div>
    </div>`;
}

// formHTML is the posting form for a board (kind "thread") or for one thread
// (kind "reply", with that thread passed in so the form can follow its state).
function formHTML({ kind, slug, thread = null, open = false }) {
  // Archived is for good, so the form gives way to where the conversation can
  // go instead rather than to a box that could only fail.
  if (thread && thread.archived_at) {
    return `<p class="archived-note" data-replies-closed><span class="badge archived">archived</span>
      <span>Archived threads take no new replies. To carry on the conversation,
        <a href="${boardPath(slug)}">start a new thread on ${esc(boardLabel(slug))}</a>.</span></p>`;
  }
  if (thread && thread.locked) {
    return `<p class="locked-note" data-replies-closed><span class="badge locked">locked</span> This thread is locked; no new replies.</p>`;
  }
  const threadNo = thread ? thread.no : null;

  const isThread = kind === "thread";
  const name = storageGet(NAME_STORAGE_KEY) || "";
  const subject = isThread
    ? `<div class="field">
         <label class="label" for="${FIELD_IDS.subject}">Subject</label>
         <input type="text" id="${FIELD_IDS.subject}" name="subject" placeholder="Optional" maxlength="100" />
       </div>`
    : "";
  const sage = isThread
    ? ""
    : `<label class="sage"><input type="checkbox" name="sage" /> sage (reply without bumping)</label>`;
  // Off by default and per post: a verified poster is anonymous unless they
  // say otherwise on this particular post, and the credential is sent only
  // when they do. The box sits in a slot that says which badge it offers, or
  // that it offers none, so that the post goes out under the badge the box
  // named or not at all (offerHeldBadge).
  const held = state.verification.badge;
  const badge = `<span class="badge-option" data-badge-option="${held ? held.number : ""}">${badgeOptionHTML(held)}</span>`;

  // A site that takes no images gets no image field at all, rather than a
  // picker that every post using it would be refused for.
  const image = state.site.uploads
    ? `<div class="field">
          <span class="label" id="f-image-label">Image (optional)</span>
          ${fileFieldHTML(thread && thread.at_image_limit ? imageLimitReason(thread.board) : "")}
        </div>`
    : "";

  // Said where the button is, since pressing it may wait on the puzzle. No
  // link to the FAQ's entry here: leaving the page would lose the post.
  let check = "";
  if (state.site.captcha) {
    check = ` <span data-check-hint>${held ? CHECK_HINT_WITH_BADGE : CHECK_HINT}</span>`;
  }

  // Under the message, on every form: no contact details, because they are
  // how a conversation leaves the site for a private one, which educhan
  // deliberately does not have.
  return `
    <details class="post-form" ${open ? "open" : ""}>
      <summary>${isThread ? "Start a new thread" : "Reply to this thread"}</summary>
      <form class="fields" data-kind="${kind}" ${threadNo ? `data-thread="${threadNo}"` : ""}>
        <div class="row">
          ${subject}
          <div class="field">
            <label class="label" for="${FIELD_IDS.name}">Name</label>
            <input type="text" id="${FIELD_IDS.name}" name="name" value="${esc(name)}"
                   placeholder="Anonymous (add #secret for a tripcode)" maxlength="80" autocomplete="nickname" />
          </div>
        </div>
        <div class="field">
          <label class="label" for="${FIELD_IDS.body}">${isThread ? "Message" : "Reply"}</label>
          <textarea id="${FIELD_IDS.body}" name="body" maxlength="8000" aria-describedby="f-body-hint f-contact-hint"
                    placeholder="${isThread ? "What's on your mind?" : "Write a reply. Use >>123 to quote a post."}"></textarea>
          <p class="hint body-hint" id="f-body-hint"><code>[spoiler]…[/spoiler]</code> hides text,
            <code>[code]…[/code]</code> shows code, <code>[math]…[/math]</code> typesets maths,
            and <code>&gt;&gt;&gt;/board/</code> links a board.</p>
          <p class="hint body-hint" id="f-contact-hint">Don't post contact details like phone
            numbers or Snapchat or Discord names. educhan has no private messages, on
            purpose.</p>
        </div>
        ${image}
        <div class="actions">
          <button class="btn primary" type="submit">${isThread ? "Create thread" : "Post reply"}</button>
          ${sage}
          ${badge}
          <span class="hint">Posting is anonymous.${check}</span>
        </div>
      </form>
    </details>`;
}

// badgeOptionHTML is the posting form's badge box, for the badge held, or
// nothing when there is none. A badged post takes its ID from the credential
// rather than the poster key, which is worth saying where the choice is made —
// and so is the one thing that would tie it to the unbadged posts all the
// same: a name or tripcode used on both. Ticking the box empties the Name
// field (badgeToggled).
function badgeOptionHTML(held) {
  if (!held) return "";
  return `<label class="sage"><input type="checkbox" name="badge" aria-describedby="f-badge-hint" /> post with ${verifiedBadgeHTML(held.label)}</label>
       <span class="hint" id="f-badge-hint">A badged post gets its own ID in each thread. A name or
         tripcode you also use without the badge would tie those posts to it, so the badge starts
         with the name empty.</span>`;
}

// badgeOffered is the number (from readBadge) of the badge a posting form's
// box offers, or "" when the form offers none.
function badgeOffered(form) {
  const slot = form.querySelector("[data-badge-option]");
  return slot ? slot.dataset.badgeOption : "";
}

// drawBadgeOption brings a posting form's badge box into line with the badge
// this browser holds now. A form offers the badge held when it was drawn, and
// the badge can change under it: Verify pressed for another address, and the
// reader gone on to a board before the answer came, say, or the badge
// forgotten, refused or replaced in another tab. That form went on saying "post
// with" the badge before, while a post sent from it carried the new credential,
// so a box ticked for one institution put the post out under another. Now a box
// naming a badge no longer held is drawn again, unticked, for the badge held
// now, or taken out when there is none, and the half-written post is left where
// it is. The form's note on the anti-spam check is worded again to match, since
// only a form that offers a badge says a badged post skips it. A box that had
// the focus hands it to the box drawn in its place, or to the form's button
// when there is none, rather than dropping it to the page. The Name field is
// left as it is too: a remembered name or tripcode put back on a post written
// to go out badged would tie it to the reader's unbadged posts. A box drawn
// into a form held while its request is pending, whether or not it had a box
// before, is drawn switched off, as holdForm switched off the controls that
// were on, and switched on with them when the hold ends: what is sent was read
// at the press, so a box ticked meanwhile would show a badge the post does not
// carry. A box taken out of a held form stays out. It says whether the box it
// drew over was ticked.
function drawBadgeOption(form) {
  const held = state.verification.badge;
  const number = held ? held.number : "";
  const slot = form.querySelector("[data-badge-option]");
  if (!slot || slot.dataset.badgeOption === number) return false;
  const box = slot.querySelector('input[name="badge"]');
  const ticked = Boolean(box && box.checked);
  const focused = slot.contains(document.activeElement);
  slot.dataset.badgeOption = number;
  slot.innerHTML = badgeOptionHTML(held);
  const drawn = slot.querySelector('input[name="badge"]');
  if (drawn && heldForms.has(form)) {
    drawn.disabled = true;
    heldForms.get(form).push(drawn);
  }
  const check = form.querySelector("[data-check-hint]");
  if (check) check.textContent = held ? CHECK_HINT_WITH_BADGE : CHECK_HINT;
  const next = drawn || form.querySelector('button[type="submit"]');
  if (focused && next) next.focus({ preventScroll: true });
  return ticked;
}

// The posting forms whose ticked badge went from under them: their box drawn
// again for the badge held now, unticked, or taken out (drawBadgeOption),
// because another tab forgot, refused or replaced the badge
// (followCredentialStorage), or this tab replaced it, refused it on a post from
// another page or heard it was switched off or had run out, whether the form
// was idle or its badged post was on its way, or by a press that found the page
// holding a badge other than the one the box was drawn for (badgeOffered), or
// drawn again by a redraw that offers another badge or none (restoreDraft), or
// because the server refused it on the form's own post while the form was off
// the screen, where the refusal says nothing then (submit); a redraw that
// carries the form held says the refusal instead, as it puts its values back
// into its own page, not carrying the note (restoreDraft), or keeps that page,
// dropping it (render), unless a press refused before the note could refuse it,
// as empty say, has dropped that refusal first, when the note stays, for the
// form that redraw draws or the page it keeps. Said nothing, the form's next
// press would post without the badge, under the ID the reader's other posts in
// the thread without one share, which is what a badged post's own ID is there
// to keep apart. So that press is refused, once, saying so (submit), and the
// one after it posts. A badged post that goes through takes the note with it,
// as does a refusal of it that tells the form, on screen, that its badge was
// replaced, that notice being its one refusal (submit), or that notice worded
// again, in place or by a redraw for the form it draws (rewordRefusal,
// restoreDraft), and so does the reader ticking or unticking the box. Each is
// kept with the number of the badge that went (badgeNumber), and the poster key
// storage held then, for the notice to name that ID only while the key is the
// same (badgeWithdrawnNotice). It lives with the form, and a draft carries it
// across a re-render (formDraft, restoreDraft). Forget this browser drops it:
// the key goes too, so the next post has an ID of its own.
const badgeWithdrawn = new WeakMap();

// withdrawBadgeOption is drawBadgeOption for a form whose badge has gone from
// under it, noting the form in badgeWithdrawn if its box was ticked. A ticked
// box drawn again unticked, in a form with some of the post in it, also starts
// the puzzle the form now needs, as the reader's own untick would
// (captchaBadgeToggled), so the press after the one the note refuses usually
// finds it solved; but not with startNothing: as /verify is drawn over the form
// (loadVerification), or a redraw puts a held form back (restoreDraft), since
// drawing a page starts nothing, or as the form's own badged post is refused
// while the form is off the screen, where nobody is writing in it (submit). A
// form noted then starts it at the press the note refuses (submit). A box drawn
// again for another badge or none, ticked or not, has a refusal the form was
// told worded again for it while that is still the notice on screen
// (rewordRefusal).
function withdrawBadgeOption(form, startNothing) {
  const offered = badgeOffered(form);
  const key = storageGet(POSTER_KEY_STORAGE_KEY) || "";
  const ticked = drawBadgeOption(form);
  if (ticked) {
    badgeWithdrawn.set(form, { number: offered, key });
  }
  // A box drawn again, ticked or not, changes what a refusal the form was told
  // can say about it (rewordRefusal).
  if (badgeOffered(form) !== offered) {
    rewordRefusal(form);
  }
  if (ticked && !startNothing && captchaFormNeedsIt(form) && captchaFormHasWriting(form)) {
    prepareCaptcha();
  }
}

// offerHeldBadge brings every posting form on screen but except into line
// with the badge this browser holds now, whatever changed it, noting each form
// whose ticked box that unticks (withdrawBadgeOption).
function offerHeldBadge(startNothing = false, except = null) {
  for (const form of el.main.querySelectorAll("form[data-kind]")) {
    if (form !== except) withdrawBadgeOption(form, startNothing);
  }
}

// What a posting form says when its badge has gone from the browser and it
// was asked to post with it.
const BADGE_GONE = "Your badge is no longer available in this browser — verify again to post with it.";

// badgeWithdrawnNotice is what the press of a form in badgeWithdrawn is told:
// that the badge has gone, and what pressing again does (pressedWithoutBadge).
// The form offers the badge held now, if there is one, not ticked: ticking it
// is the reader's choice, and takes the note off.
function badgeWithdrawnNotice(form, { key }) {
  const held = state.verification.badge;
  const gone = held
    ? `The badge you ticked for this post has gone from this browser, and the one this form now offers, Verified: ${held.label}, is not ticked.`
    : BADGE_GONE;
  return `${gone} ${pressedWithoutBadge(form, key)}`;
}

// pressedWithoutBadge says what the next press of a form that has lost its
// badge does: it posts without one. A reply then goes out under the same ID as
// the reader's posts in the thread without a badge while storage holds key, the
// poster key it held when the badge went, the key that ID is made from: a
// forget in another tab that this one hears of only as the badge going, as a
// tab does where storage would not take the emptying and the browser has no
// BroadcastChannel (FORGET_CHANNEL), takes the key away too, and a new thread
// has no posts yet to share an ID with.
function pressedWithoutBadge(form, key) {
  const sameID = form.dataset.kind === "reply" && key !== "" && key === storageGet(POSTER_KEY_STORAGE_KEY);
  return sameID
    ? "Pressing again posts this without a badge, under the same ID as the posts you make in this thread without one."
    : "Pressing again posts this without a badge.";
}

// What a posting form says when the server refuses its badged post as expired
// or revoked while storage holds another badge, verified since in this browser,
// in another tab or in this one by a confirmation answered after the reader
// left /verify, and the browser has not been forgotten since the press by the
// time the form is told: storage holding that badge at the answer, or by the
// time the notice is worded again, in place while it is on screen
// (rewordRefusal) or by a redraw that draws the form again (restoreDraft), and
// for a form answered off the screen, by the time a redraw tells it
// (unsaidNotices). A badge stored after a forget is someone else's, so the form
// is then told the server's words (submit).
const BADGE_REPLACED = "The badge this post carried was replaced by one verified since in this browser, so nothing was posted.";

// badgeReplacedNotice is that form's notice, in place of the server's, which
// says to verify again: that would cost another emailed code, and replace the
// badge the browser already holds. It is the form's one refusal for the badge
// that went, so it says what pressing again does (pressedWithoutBadge), and how
// to post with the new badge: the form offers it, not ticked (drawBadgeOption).
// A post left with nothing in it, neither text nor image (errorMessage's
// hasImage), has nothing to press again for, and the notice then says only what
// went (imageTakenOff adds why, and to write something, for an image release
// took off). It reads the badge held, the box of the form it is given, the
// poster key and what the post holds when it is worded, as it is said: on
// screen at the answer; for a form answered off the screen, by the redraw that
// tells it, for the form drawn or kept then (unsaidNotices); again by a redraw
// loading meanwhile that draws the form again while the notice is still up,
// Update say, or that takes the image off, for the form it draws and the text
// and image put back into it (restoreDraft); and again in place while it is
// still the notice on screen, when the form's box is drawn again for another
// badge or none, a badge verified or forgotten in another tab say, or the
// browser is forgotten (rewordRefusal). Each time, the form loses a note left
// on it since, the notice being its one refusal. Each of those can change by
// then: with no badge held by then, forgotten since, it ends with what pressing
// again does, there being no box to tick; with another, verified since, it
// names that one, which the form offers; with the new badge's box ticked since,
// it says that pressing again posts with that badge; and for a post left with
// nothing in it since, its text erased say, it says only what went. The
// reader's own changes to the form while the notice is on screen, a tick of its
// box or its text erased say, do not word it again: the reader makes them with
// the form in sight, the box ticked as the notice says to, and the notice, an
// alert, would be heard again at each; the next wording reads them. In a
// browser forgotten since the press, the form is told the server's words
// instead (submit).
function badgeReplacedNotice(form, { hasText, hasImage, imageOff }) {
  if (!hasText && (imageOff !== "" || !hasImage)) {
    return BADGE_REPLACED;
  }
  const held = state.verification.badge;
  const box = field(form, "badge");
  if (held && box && box.checked && badgeOffered(form) === held.number) {
    return `${BADGE_REPLACED} The box for the new badge, Verified: ${held.label}, is ticked, so pressing again posts this with that badge.`;
  }
  const press = pressedWithoutBadge(form, storageGet(POSTER_KEY_STORAGE_KEY) || "");
  if (!held) {
    return `${BADGE_REPLACED} ${press}`;
  }
  return `${BADGE_REPLACED} ${press} To post it with the new badge, Verified: ${held.label}, tick its box.`;
}

/* File field behaviour ------------------------------------------------------ */

function isAcceptedImage(file) {
  return ACCEPTED_IMAGES.split(",").includes(file.type) || ACCEPTED_EXTENSIONS.test(file.name);
}

// setFile puts a dropped file into the form's file input, and says whether it
// took it, for the caller to show it (updateFileField). Assigning a FileList
// needs a DataTransfer, which is also how the draft is carried across a
// re-render.
function setFile(field, file) {
  const input = field.querySelector(".file-input");
  if (!input || !file) {
    return false;
  }
  // A drop reaches a blocked field too; say why it was not taken. A picker
  // switched off only while its post is pending (holdForm) takes nothing
  // either: the post has already been read.
  if (input.disabled) {
    if (field.classList.contains("blocked")) {
      showNotice(field.querySelector(".file-blocked").textContent);
    }
    return false;
  }
  if (!isAcceptedImage(file)) {
    showNotice("That file is not an accepted image. Use JPEG, PNG, WebP, or GIF.");
    return false;
  }
  try {
    const transfer = new DataTransfer();
    transfer.items.add(file);
    input.files = transfer.files;
  } catch {
    return false;
  }
  return true;
}

// imageLimitReason is why a thread's reply form takes no image. On a site that
// takes none, that is the whole reason: there is no cap to talk about.
function imageLimitReason(board) {
  if (!state.site.uploads) {
    return UPLOADS_OFF_REASON;
  }
  const limit = board ? board.image_limit : undefined;
  if (limit === 0) {
    return "Replies on this board can't carry images.";
  }
  return limit > 0
    ? `This thread has reached its limit of ${plural(limit, "image", "images")}. Text replies still work.`
    : "This thread has reached its image limit. Text replies still work.";
}

// blockFileField takes the image off a live form and switches the picker off
// with the reason on screen, for when the server says no after the form was
// drawn. A disabled input is also left out of the form's data.
function blockFileField(field, reason) {
  clearFileField(field);
  const input = field.querySelector(".file-input");
  const note = field.querySelector(".file-blocked");
  input.disabled = true;
  note.textContent = reason;
  note.hidden = false;
  field.classList.add("blocked");
}

// unblockFileField switches a picker back on, as fileFieldHTML draws a live
// one, for a held form whose page was redrawn with the picker live while it
// was held (holdForm). It leaves whatever image the picker holds.
function unblockFileField(field) {
  const input = field.querySelector(".file-input");
  const note = field.querySelector(".file-blocked");
  input.disabled = false;
  note.textContent = "";
  note.hidden = true;
  field.classList.remove("blocked");
}

// blockRefusedPicker switches off the picker of a form whose image the server
// refused itself, the thread's image limit reached (limitReached) or uploads
// switched off, with the reason, and puts the thread on screen, the form's own,
// in step with what the refusal taught the page: on screen at the refusal
// (submit), or for a form answered off the screen meanwhile on the page a
// failed redraw keeps, form and all (render), or at the form's next press if
// that comes first, unless an image has been attached since (submit). A page
// drawn afresh draws both from what the server says by then, and needs neither.
function blockRefusedPicker(form, limitReached) {
  const fileField = form.querySelector("[data-file-field]");
  const thread = state.thread;
  if (limitReached) {
    if (fileField) {
      blockFileField(fileField, imageLimitReason(thread && thread.board));
    }
    // The summary line was drawn before the thread filled up; it says so now,
    // as a fresh render of the thread would.
    if (thread) {
      thread.at_image_limit = true;
      if (thread.board && thread.board.image_limit > 0) {
        thread.images = Math.max(Number(thread.images) || 0, thread.board.image_limit);
      }
    }
  } else if (fileField) {
    blockFileField(fileField, UPLOADS_OFF_REASON);
  }
  redrawThreadCounts();
}

function clearFileField(field) {
  const input = field.querySelector(".file-input");
  if (input) {
    input.value = "";
  }
  updateFileField(field);
}

// updateFileField syncs the preview with whatever the input now holds. The
// preview is a data: URL because the CSP allows those but not blob:.
function updateFileField(field) {
  const input = field.querySelector(".file-input");
  const preview = field.querySelector(".file-preview");
  const thumb = field.querySelector(".file-thumb");
  const meta = field.querySelector(".file-meta");
  const file = input && input.files && input.files[0];

  if (!file) {
    field.classList.remove("has-file");
    preview.hidden = true;
    thumb.removeAttribute("src");
    meta.textContent = "";
    // The spoiler box lives in the preview and means nothing without a file,
    // so it does not stay ticked for the next one.
    const spoiler = field.querySelector('input[name="spoiler"]');
    if (spoiler) spoiler.checked = false;
    return;
  }

  if (!isAcceptedImage(file)) {
    showNotice("That file is not an accepted image. Use JPEG, PNG, WebP, or GIF.");
    clearFileField(field);
    return;
  }
  // Said now, not after the upload: the server would read the whole file
  // only to refuse it. An unknown limit leaves the call to the server.
  const max = state.site.maxUploadBytes;
  if (max > 0 && file.size > max) {
    showNotice(imageTooLargeMessage());
    clearFileField(field);
    return;
  }

  field.classList.add("has-file");
  preview.hidden = false;
  meta.textContent = `${file.name} · ${fileSize(file.size)}`;
  thumb.alt = `Preview of ${file.name}`;

  const reader = new FileReader();
  reader.onload = () => {
    if (input.files[0] === file) {
      thumb.src = reader.result;
    }
  };
  reader.readAsDataURL(file);
}

/* Views -------------------------------------------------------------------- */

function renderNav(route) {
  el.nav.innerHTML = state.boards
    .map((b) => {
      const current = b.slug === route.slug ? ' aria-current="page"' : "";
      return `<a href="${boardPath(b.slug)}" title="${esc(b.description)}"${current}>${esc(boardLabel(b.slug))}</a>`;
    })
    .join("");
  // The row scrolls sideways when the boards outrun it, so the board being
  // shown is brought into it: the highlight is what says where the reader is.
  // Only the row moves, never the page.
  const current = el.nav.querySelector('[aria-current="page"]');
  if (current) {
    const row = el.nav.getBoundingClientRect();
    const pill = current.getBoundingClientRect();
    if (pill.left < row.left) {
      el.nav.scrollLeft -= row.left - pill.left + 12;
    } else if (pill.right > row.right) {
      el.nav.scrollLeft += pill.right - row.right + 12;
    }
  }

  for (const link of el.pageLinks) {
    if (link.dataset.page === route.view) {
      link.setAttribute("aria-current", "page");
    } else {
      link.removeAttribute("aria-current");
    }
  }
}

function boardHeadHTML(board, view) {
  const views = [
    { href: boardPath(board.slug), label: "Index", key: "index" },
    { href: `${boardPath(board.slug)}catalog`, label: "Catalog", key: "catalog" },
    { href: `${boardPath(board.slug)}archive`, label: "Archive", key: "archive" },
  ];
  const links = views
    .map((v) => `<a href="${v.href}"${v.key === view ? ' aria-current="page"' : ""}>${v.label}</a>`)
    .join("");
  // Rules are a board's own words, closed until asked for so they do not push
  // the threads down on every visit. Escaped like anything else from the API;
  // the stylesheet keeps their line breaks.
  const rules = board.rules
    ? `<details class="board-rules"><summary>Rules</summary><div class="rules-text">${esc(board.rules)}</div></details>`
    : "";

  return `
    <div class="board-head">
      <h1>${esc(boardLabel(board.slug))} <span class="board-name">${esc(board.name)}</span></h1>
      <span class="desc">${esc(board.description)}</span>
      <nav class="views" aria-label="Board views">${links}</nav>
      ${rules}
    </div>`;
}

function renderHome() {
  const cards = state.boards
    .map(
      (b) => `
      <a class="board-card" href="${boardPath(b.slug)}">
        <strong>${esc(boardLabel(b.slug))} &mdash; ${esc(b.name)}</strong>
        <small>${esc(b.description)}</small>
      </a>`
    )
    .join("");

  el.main.innerHTML = `
    <div class="board-head"><h1>Boards</h1></div>
    <div class="board-list">${cards || '<p class="empty">No boards yet.</p>'}</div>`;
  announce("educhan");
}

function pagerHTML(slug, page, pages) {
  if (pages <= 1) {
    return "";
  }
  const prev =
    page > 1
      ? `<a href="${boardPath(slug)}?page=${page - 1}" rel="prev">&larr; Newer</a>`
      : `<span>&larr; Newer</span>`;
  const next =
    page < pages
      ? `<a href="${boardPath(slug)}?page=${page + 1}" rel="next">Older &rarr;</a>`
      : `<span>Older &rarr;</span>`;
  return `<nav class="pager" aria-label="Pagination">${prev}<span class="position">Page ${page} of ${pages}</span>${next}</nav>`;
}

async function renderIndex(route) {
  const index = await api(`/api/boards/${encodeURIComponent(route.slug)}/threads?page=${route.page}`);
  if (overtaken(route)) {
    return;
  }
  const { board } = index;

  const threads = index.threads
    .map((summary) => {
      const context = { slug: board.slug, board, subject: summary.subject, thread: summary };
      const replies = summary.last_replies
        .map((post) => postHTML(post, { slug: board.slug }))
        .join("");
      const omitted =
        summary.omitted_replies > 0
          ? `<p class="omitted">${plural(summary.omitted_replies, "reply", "replies")} omitted.</p>`
          : "";
      const label = summary.subject || `Thread No. ${summary.no}`;

      return `
        <section class="thread" aria-label="${esc(label)}">
          ${postHTML(summary.op, context)}
          <div class="thread-actions">
            <a href="${threadPath(board.slug, summary.no)}">Open thread (${plural(summary.replies, "reply", "replies")}${
              summary.images > 0 ? `, ${plural(summary.images, "image", "images")}` : ""
            })</a>
          </div>
          ${omitted}
          ${replies}
        </section>`;
    })
    .join("");

  el.main.innerHTML = `
    ${boardHeadHTML(board, "index")}
    ${formHTML({ kind: "thread", slug: board.slug, open: index.threads.length === 0 })}
    ${threads || '<p class="empty">No threads yet — start the first one.</p>'}
    ${pagerHTML(board.slug, index.page, index.pages)}`;
  announce(`${boardLabel(board.slug)}${index.pages > 1 ? ` page ${index.page}` : ""}`);
}

async function renderCatalog(route) {
  const catalog = await api(`/api/boards/${encodeURIComponent(route.slug)}/catalog`);
  if (overtaken(route)) {
    return;
  }
  const { board } = catalog;

  const tiles = catalog.threads
    .map((t) => {
      // The tile is one link to the thread, so a spoiler cannot be revealed in
      // place here; it stays a plain tile and opens in the thread. Where the
      // site takes no images, a thread without one has no empty box saying so:
      // every tile would carry one.
      let media = state.site.uploads ? `<div class="tile-media empty-media">no image</div>` : "";
      if (t.attachment && t.attachment.spoiler) {
        media = `<div class="tile-media spoiler-media">Spoiler image</div>`;
      } else if (t.attachment) {
        media = `<div class="tile-media">${thumbHTML(t.attachment, { alt: "" })}</div>`;
      }
      return `
      <a class="tile" href="${threadPath(board.slug, t.no)}">
        <div class="stats">
          <span class="replies">${plural(t.replies, "reply", "replies")}</span>
          ${t.images > 0 ? `<span class="images">&middot; ${plural(t.images, "image", "images")}</span>` : ""}
          ${badgesHTML(t, board)}
        </div>
        ${media}
        ${t.subject ? `<h2 class="subject">${esc(t.subject)}</h2>` : ""}
        <div class="excerpt">${esc(t.excerpt)}</div>
      </a>`;
    })
    .join("");

  el.main.innerHTML = `
    ${boardHeadHTML(board, "catalog")}
    <div class="catalog">${tiles || '<p class="empty">No threads yet — start the first one.</p>'}</div>`;
  announce(`${boardLabel(board.slug)} catalog`);
}

async function renderThread(route) {
  const detail = await api(`/api/boards/${encodeURIComponent(route.slug)}/threads/${route.no}`);
  if (overtaken(route)) {
    return;
  }
  const { board } = detail;
  // Kept for what the page learns about the thread after drawing it: a reply
  // refused at the image limit updates its summary line from here.
  state.thread = detail;

  const posts = detail.posts
    .map((post) =>
      postHTML(post, {
        slug: board.slug,
        board,
        subject: detail.subject,
        thread: detail,
        quotable: true,
      })
    )
    .join("");

  // An archived thread no longer changes, so it has nothing to update and its
  // state is said once, above the posts, as well as in place of the form.
  const archived = Boolean(detail.archived_at);
  const archiveNote = archived
    ? `<p class="archived-note"><span class="badge archived">archived</span>
        <span>This thread was archived ${timeHTML(detail.archived_at)}. It can still be read and
          linked to${archiveKeptFor(board)}.</span></p>`
    : "";

  el.main.innerHTML = `
    ${boardHeadHTML(board, archived ? "archive" : "index")}
    <div class="thread-actions">
      <a href="${boardPath(board.slug)}">&larr; Back to ${esc(boardLabel(board.slug))}</a>
      ${archived ? "" : '<button class="link" data-action="refresh" type="button">Update</button>'}
      <span class="muted" data-thread-counts>${threadCounts(detail, board, archived).join(" &middot; ")}</span>
    </div>
    ${archiveNote}
    <section class="thread detail">${posts}</section>
    ${formHTML({ kind: "reply", slug: board.slug, thread: detail, open: true })}`;
  announce(`${detail.subject || `Thread No. ${detail.no}`} — ${boardLabel(board.slug)}`);
}

// threadCounts is the thread view's summary line. The image count carries the
// board's cap, because the cap is what a poster with an image needs to know.
// A site that takes no images has no cap to show, and counts only the images
// a thread still holds from before they were switched off.
function threadCounts(thread, board, archived) {
  const images = Number(thread.images) || 0;
  const counts = [plural(thread.replies, "reply", "replies")];
  if (!state.site.uploads) {
    if (images > 0) counts.push(plural(images, "image", "images"));
  } else {
    counts.push(
      board.image_limit > 0 ? `${images} of ${plural(board.image_limit, "image", "images")}` : plural(images, "image", "images")
    );
  }
  if (!archived && thread.at_bump_limit) counts.push("at bump limit, no longer bumping");
  if (state.site.uploads && !archived && thread.at_image_limit) {
    counts.push(board.image_limit === 0 ? "replies can't carry images" : "image limit reached, text replies only");
  }
  return counts;
}

// redrawThreadCounts puts the summary line back in step with state.thread,
// for when a refusal teaches the page something it was drawn without.
function redrawThreadCounts() {
  const thread = state.thread;
  const counts = el.main.querySelector("[data-thread-counts]");
  if (thread && counts) {
    counts.innerHTML = threadCounts(thread, thread.board, false).join(" &middot; ");
  }
}

// archiveKeptFor says how long an archived thread stays, when the board says.
function archiveKeptFor(board) {
  return board.archive_days > 0 ? `, and is deleted after ${plural(board.archive_days, "day", "days")} in the archive` : "";
}

async function renderArchive(route) {
  const archive = await api(`/api/boards/${encodeURIComponent(route.slug)}/archive`);
  if (overtaken(route)) {
    return;
  }
  const { board } = archive;
  const label = boardLabel(board.slug);

  const rows = archive.threads
    .map((t) => {
      // A subject heads the row when there is one; otherwise the opening
      // post's own words do, so no row is just a number.
      const title = t.subject
        ? `<strong class="archive-subject">${esc(t.subject)}</strong>`
        : "";
      const excerpt = t.excerpt ? `<span class="archive-excerpt">${esc(t.excerpt)}</span>` : "";
      return `
        <li>
          <a class="archive-row" href="${threadPath(board.slug, t.no)}">
            <span class="archive-no mono">No.&nbsp;${t.no}</span>
            <span class="archive-text">${title || excerpt ? `${title}${excerpt}` : `Thread No. ${t.no}`}</span>
            <span class="archive-meta">${plural(t.replies, "reply", "replies")} &middot; archived ${timeHTML(t.archived_at)}</span>
          </a>
        </li>`;
    })
    .join("");

  const kept = board.archive_days > 0 ? ` for ${plural(board.archive_days, "day", "days")}, then deleted` : "";
  el.main.innerHTML = `
    ${boardHeadHTML(board, "archive")}
    <p class="archive-intro">Threads that have left ${esc(label)} are kept here, read-only${kept}.
      Newest archived first.</p>
    ${rows ? `<ol class="archive-list">${rows}</ol>` : `<p class="empty">Nothing on ${esc(label)} has been archived yet.</p>`}`;
  announce(`${label} archive`);
}

/* Verification ------------------------------------------------------------- */

// Which half of the verification page is on screen. A code has been sent when
// this is "code"; nothing about the address is kept here, only the badge it
// would earn, so a reload starts over rather than resuming. replacing is for
// the address step alone: it was asked for over a badge this browser holds
// ("Verify another address"), where the page would otherwise show that badge.
// While a code is pending the code box shows whatever is held. ended is for the
// address step too: why the code box it replaced has gone, when the server
// dropped the code being typed (CODE_ENDED_NOTES). A request out from a step
// marks it without changing it: solving, while a code request waits on its
// puzzle, for the step drawn again to say so (verifyViewHTML); confirming,
// while a code is being confirmed, and spentElsewhere, once another tab's
// verification has put that code past use (codeSpentElsewhere). Nothing else
// edits a step in place, and a change of step puts a new object here:
// submitVerification tells an answer that is still wanted from one the reader
// has moved on from by whether verifyStep is the object it was sent from.
let verifyStep = { step: "email", label: "" };

// The step a Verify request is out from, while it is out, and null otherwise
// (submitVerification). The wait is kept here as well as on the buttons of the
// form that sent it, because a reader who leaves the page and comes back before
// the answer gets the step drawn afresh, and verifyViewHTML draws it waiting
// from this. Drawn with live buttons, it let the reader give the step up while
// the server was issuing its badge — for the same address, retiring the badge
// held here — or send a second code while the page went on to show the first
// one's box.
let verifyBusy = null;

// The last Verify request submitVerification took on, as a promise that settles
// once it is done with that request, whatever the answer was and whether or not
// it was sent. The next request, from whichever step, waits for it before it is
// sent. An answer from a step the reader has left is dropped, but not the
// cookie it carries: the server checks a code against the pending-code cookie
// the browser holds, and the browser holds whichever one landed last. Sent side
// by side, a code asked for from the address step that Forget this browser drew
// could land before one still out from the step it replaced, and the code box
// then on screen refused its own right code, spending its attempts, while the
// server checked it against the other address. So the wait has to end, and
// every request it can wait on is given up once its time is out
// (verifyDeadline), and so is the puzzle a code request solves in its turn: its
// challenge is asked for within the same time, and its workers give up after
// CAPTCHA_TIMEOUT_MS (solveCaptcha). Without that, one request whose answer
// never came, over a connection gone silent, held every later one from any step
// until a reload, the fresh step Forget this browser draws included, while the
// page said only that it was sending.
let verifyRequest = Promise.resolve();

// VERIFY_TIMEOUT_MS is how long a Verify request is given, from when it is
// sent, before the page gives it up. Send code and Verify then say they
// couldn't reach educhan (UNREACHABLE); the GET that asks whether verification
// is on leaves the page with what the server said last (loadVerification), so a
// badge just issued is still drawn and offered. It is longer than the server
// takes to answer or give up — ten seconds to read a request's headers and
// thirty to write its answer, in cmd/server/main.go — so a request given up is
// one whose answer was not coming. Giving it up cancels it: an answer that
// comes after is not taken, the pending-code cookie with it, so the request
// that goes next still sets the cookie the browser keeps.
const VERIFY_TIMEOUT_MS = 45000;

// verifyDeadline is the signal a Verify request is sent with, which fires once
// VERIFY_TIMEOUT_MS has passed, timed from when the request is sent: a code
// request's puzzle before it is not counted (submitVerification). It is a
// controller and a timer rather than AbortSignal.timeout, which Safari has only
// since 16: the page opens on iPhones whose last update is iOS 15 (the age
// screen's <dialog> needs 15.4), and there that call would throw before Send
// code went out. The challenge a puzzle is solved from is asked for with it
// too, following the signal that stops the puzzle (stop), since AbortSignal.any
// is newer still: a code request waits for that puzzle in its turn, and every
// Verify request after it waits for that turn (verifyRequest).
function verifyDeadline(stop) {
  const deadline = new AbortController();
  setTimeout(() => deadline.abort(), VERIFY_TIMEOUT_MS);
  if (stop) stop.addEventListener("abort", () => deadline.abort(), { once: true });
  return deadline.signal;
}

// The step the Verify page was last drawn for, the first element of that
// drawing, and when it was drawn (drawVerifyStep). Anything drawn into the
// page since — the loading skeleton, or a page the reader left whose answer
// came in late — takes that element out of it, which is how verifyStepOnScreen
// tells a step still on screen from one that was drawn and then drawn over.
// When it was drawn is how the page's click listener tells a press aimed at
// the drawing from one aimed at what was there before it (VERIFY_SETTLE_MS).
// A refusal said in the drawing moves it under the reader as a drawing does,
// and that time is then when the refusal was said (refuseVerification).
let verifyShown = { step: null, node: null, at: -Infinity };

// VERIFY_SETTLE_MS is how long the Verify page's own buttons, each of which
// moves the page to another step or forgets what this browser holds
// (VERIFY_MOVES), take no press once a step has been drawn. A step is drawn in
// place, so the button just pressed is gone from under the reader's finger,
// and another can be drawn where it was: the second tap of a double tap, or a
// second tap from a reader who thought the first did nothing, pressed that
// one. On a phone, a double tap on Use a different address pressed Keep my
// badge, drawn where it had been, and undid the reader's choice; on a wide
// screen, a double click on Verify another address pressed Forget this
// browser, which the shorter address step had brought up into its place, and
// took the badge, the post key and the name. A press that soon after a drawing
// was aimed at what was there before it — nobody reads a new step and aims at
// one of its buttons in half a second — so it is let go, and a reader who did
// mean it presses again. A refusal said in a step's form moves its buttons
// too, and starts the same wait (refuseVerification).
const VERIFY_SETTLE_MS = 500;
const VERIFY_MOVES = [
  '[data-action="forget-verification"]',
  '[data-action="forget-browser"]',
  '[data-action="verify-keep"]',
  '[data-action="verify-again"]',
  '[data-action="verify-restart"]',
].join(", ");

// VERIFY_ATTESTATION is the 18+ attestation, the one box the Verify page
// draws. Its label holds the box and the sentence, so a press anywhere on it
// ticks the box. The box is ticked by the reader alone and starts unticked on
// every drawing (build plan 2.14), and a step drawn in place can put it where
// the button just pressed was, so it lets a press go for VERIFY_SETTLE_MS as
// the buttons do. Before, a double click on Use a different address ticked it
// on a wide screen, where the address step, brought clear of the header,
// moved the label under the pointer; and a double tap on Verify another
// address could land on it on a phone. So could a double tap on Send code
// with the box unticked, on a phone, once the refusal saying why scrolled the
// label down into the button's place.
const VERIFY_ATTESTATION = "label.attest";

// A refusal of the request sent from a step, as its form says it: what it
// says, the field it is about, and how (refuseVerification). It is kept with
// the step so that the step drawn again says it again. The page can be drawn
// again while an answer is on its way — the reader presses Verify in the
// header, or leaves and comes back — and then the form that sent the request
// has gone, and the step may not be drawn yet, when the refusal comes in. Like
// the notice it belongs to one visit to the page: render() puts it away, and
// sending the form again does too (clearVerifyRefusal).
let verifyRefusal = null;

// Why the badge that was here has gone, when it is worth saying: it ran out,
// the server refused it, or the reader forgot it (CREDENTIAL_FORGOTTEN). Shown
// once on the verification page, which is the only page that can do anything
// about it: in the first step drawn there after it went (drawVerifyStep).
let badgeLost = "";

const CREDENTIAL_FORGOTTEN = "This browser no longer holds a badge.";

// How many times "Forget this browser" has been pressed, on this page or on any
// other educhan page open in the browser that this one heard it from
// (forgottenElsewhere). A request notes it at the press: one still waiting on
// its puzzle after another press is not sent, and an answer that arrives after
// one keeps nothing. Forgetting has to cover what was already on its way.
let browserForgets = 0;

// How the other open educhan tabs hear "Forget this browser" pressed in one, so
// that a forget covers what they hold and have on their way, as it does its
// own. A tab hears each forget once. Where storage tells it of changes, it
// hears the forget from storage, as the credential emptied there
// (followCredentialStorage): by then its own view of storage has everything the
// pressing tab cleared, which is what lets it tell what it stored itself since
// (storedHere). A message on FORGET_CHANNEL, a BroadcastChannel, can reach a
// tab before its view of storage has caught up, so the message is the hearing
// only for a tab that storage tells nothing: FORGET_MESSAGE for one that cannot
// read storage, and FORGET_UNSTORED for every tab, when storage would not take
// the emptying in the tab the button was pressed in. Both are constants: they
// say nothing about anyone, only that the browser was forgotten.
const FORGET_CHANNEL = "educhan:forget-browser";
const FORGET_MESSAGE = "forget";
const FORGET_UNSTORED = "forget-unstored";
// What a posting form says when the browser was forgotten before its post went
// through, which stops it going: while it waited on its puzzle for its first
// send or, once the first had gone, before the second that the server's
// refusal of its answer calls for, when a request had gone but nothing was
// posted.
const FORGOTTEN_BEFORE_SENDING = "This browser was forgotten before the post went through, so nothing was posted.";

// What this tab has itself written to storage of what a forget removes, by
// storage key: the credential, the poster key and the remembered name, each as
// it was last written here. The tab a forget is pressed in removes all three,
// and a tab that handled a confirmation or a post before it heard may have
// written one back since; when it hears from storage, it removes each that its
// view of storage still holds as this tab wrote it (forgottenElsewhere). That
// view has the forget by then, so what this tab stored before the forget is
// never one, and nor is a badge confirmed or a key made in another tab since,
// however late it hears, since both are random; a name is compared by its text,
// so the same name stored since by another tab goes too.
const storedHere = new Map();

// localStorage throws rather than degrading in some privacy modes, so every
// access is guarded. Losing the credential costs a full re-verification: the
// server hands a badge out once and cannot show the same one again.
function storedCredential() {
  try {
    return localStorage.getItem(CREDENTIAL_STORAGE_KEY) || "";
  } catch {
    return "";
  }
}

// BADGE_NOT_STORED is what the page says when storage refuses a badge just
// earned.
const BADGE_NOT_STORED = "This browser will not let the page store your badge, so it cannot be used here.";

// storeCredential stores a badge just earned, and says whether it could.
function storeCredential(token) {
  try {
    localStorage.setItem(CREDENTIAL_STORAGE_KEY, token);
    storedHere.set(CREDENTIAL_STORAGE_KEY, token);
    return true;
  } catch {
    showNotice(BADGE_NOT_STORED);
    return false;
  }
}

function forgetCredential() {
  try {
    localStorage.removeItem(CREDENTIAL_STORAGE_KEY);
  } catch {
    // Nothing was stored, so nothing needs removing.
  }
  state.verification.badge = null;
}

// forgetBrowser removes everything this browser keeps that ties posts to it:
// the badge, the poster key every unbadged post's ID is made from — the next
// person at a shared computer would otherwise get the same ID in the same
// threads, and see which posts there were yours — and the remembered name,
// with its tripcode secret. The age answer stays: it says nothing about any
// post. Like forgetting the badge, it is entirely local.
// It also cancels what is on its way (forgetOnThisPage), here and in every
// other open tab, each as it hears the forget (FORGET_CHANNEL): a code request
// or a post still waiting on its puzzle is not sent, and a code request, a
// confirmation or a post sent before it keeps nothing when its answer comes
// back after it, neither the badge the server issued nor the code step nor the
// name, nor a note that a badge was lost.
function forgetBrowser() {
  forgetOnThisPage();
  for (const key of [POSTER_KEY_STORAGE_KEY, NAME_STORAGE_KEY]) {
    try {
      localStorage.removeItem(key);
    } catch {
      // Nothing was stored, so nothing needs removing.
    }
  }
  // The credential goes last, and is emptied before it is removed. The emptying
  // is what the other tabs hear the forget by (followCredentialStorage), and a
  // tab's view of storage has, by the time it is told of a change, every change
  // made before it: the key and the name are gone from the view too, so what a
  // tab finds there as it stored it, it wrote back since (storedHere).
  const emptied = emptyCredential();
  forgetCredential();
  // Posted either way. A tab that storage tells of the emptying leaves
  // FORGET_MESSAGE alone; one that storage tells nothing hears the forget by
  // it, as every tab does by FORGET_UNSTORED.
  if (forgetChannel) {
    forgetChannel.postMessage(emptied ? FORGET_MESSAGE : FORGET_UNSTORED);
  }
}

// emptyCredential empties the stored credential, which is how the other tabs
// hear a forget (followCredentialStorage), and says whether storage took it.
// Storage tells the other tabs of a change only, so a credential it holds
// emptied already, left so by a forget cut short before the removal, is removed
// first.
function emptyCredential() {
  try {
    if (localStorage.getItem(CREDENTIAL_STORAGE_KEY) === "") {
      localStorage.removeItem(CREDENTIAL_STORAGE_KEY);
    }
    localStorage.setItem(CREDENTIAL_STORAGE_KEY, "");
    return true;
  } catch {
    return false;
  }
}

// forgetOnThisPage is the part of forgetting the browser that a page holds
// itself rather than in storage, run in the tab where it was pressed and in
// every tab that hears it. It cancels what is on its way from the page: a post
// or a redraw's draft by browserForgets (restoreDraft), and a Verify request
// by moving the page to a new step, which drops an answer sent from the step
// before (submitVerification). It drops the badge read from storage, the
// poster key the page keeps for itself, what it noted it stored, and the code
// step and the note about a lost badge, which would greet the next person
// with the last one's institution; and forgets every posting form on screen
// (forgetForm). A held form a redraw has taken off the screen, while a post
// link's lookup draws its skeleton say, is forgotten as the redraw puts it
// back (restoreDraft).
function forgetOnThisPage() {
  browserForgets += 1;
  state.verification.badge = null;
  badgeLost = "";
  verifyStep = { step: "email", label: "" };
  pagePosterKey = "";
  storedHere.clear();
  for (const form of el.main.querySelectorAll("form[data-kind]")) {
    forgetForm(form);
  }
}

// forgetForm is what forgetting the browser does to a posting form: it empties
// the Name, drawn with the forgotten name, which would post under its tripcode,
// and takes out the option to post with the badge, which names the institution
// as well (drawBadgeOption, the page holding no badge by then). Nothing is said
// at the form's next press, though the badge was ticked or the form noted
// (badgeWithdrawn), as after another tab took a badge away: that would tell
// whoever presses it that this browser held a badge, and with the poster key
// gone too the post has an ID of its own, shared with none of the posts made
// before the forget. It notes the forget it did (forgottenAt). A refusal the
// form was told, while it is still the notice on screen, is worded again for
// the browser forgotten (rewordRefusal), so it names no badge the browser held:
// the notice of a replaced badge gives way to the server's words, and the
// badge's note said at a press before the forget goes, the badge and the ID it
// speaks of gone with it.
function forgetForm(form) {
  const name = field(form, "name");
  if (name) {
    name.defaultValue = "";
    name.value = "";
  }
  badgeWithdrawn.delete(form);
  drawBadgeOption(form);
  forgottenAt.set(form, browserForgets);
  rewordRefusal(form);
}

// The forget that last reached each posting form, by form, as browserForgets
// counted it (forgetForm). A held form a redraw carries across is forgotten as
// it is put back only for a forget it missed while off the screen, not again
// for one it had on screen: the Name it shows then was typed after that
// forget, and a post pressed since may be going out under it (restoreDraft).
const forgottenAt = new WeakMap();

// forgottenElsewhere is a tab's answer to Forget this browser pressed in
// another, run once for each forget, where the tab hears it (FORGET_CHANNEL):
// at the credential emptied in storage (byStorage), or at the message, in a tab
// that storage tells nothing. Heard from storage, what this tab wrote back
// since that tab cleared it goes first (storedHere): the tab's view of storage
// has the forget by then, so one of the three that it still holds as this tab
// stored it was stored after the forget, before this tab heard. Heard from the
// message, nothing is compared: a tab's view of storage can be behind the
// message, and what it then shows as this tab's own may have gone from storage,
// a badge confirmed or a key made in another tab since in its place, which the
// removal would take. Storage has no removal that looks first, so the look and
// the removal are two steps here too: if another tab stores over what this tab
// wrote back, and this tab hears the forget before its view shows that, the
// removal takes the other tab's value, which forgets more than it needs to,
// never less. Then what the page holds goes, as it did in the tab the button
// was pressed in, and /verify, when it is on screen here, is drawn again
// without the badge or the code step, unless the reader has drawn another step
// there by the time the page has asked the server (renderVerify). Its notice
// says why, at the top of the page: the reader was not at the button that would
// say it below (confirmBrowserForgotten). A badge or a key storage holds that
// this tab did not write stays, whenever this tab hears: it was stored since
// the forget, by someone else. So the notice says what was done, and that what
// the browser held then is gone, not what the browser holds
// (BROWSER_FORGOTTEN_ELSEWHERE): /verify drawn again shows such a badge's panel
// under it. A name stored since in the words this tab last wrote goes too,
// which again forgets more than it needs to.
function forgottenElsewhere(byStorage) {
  if (byStorage) {
    for (const [key, value] of storedHere) {
      if (storageGet(key) === value) {
        storageRemove(key);
      }
    }
  }
  forgetOnThisPage();
  if (onVerifyPage() && ageStatus() === "ok") {
    renderVerify().then((drawn) => {
      if (drawn) {
        showNotice(BROWSER_FORGOTTEN_ELSEWHERE);
      }
    });
  }
}

// forgetChannel is this tab's end of FORGET_CHANNEL, opened once at start-up,
// or null in a browser without BroadcastChannel, or one that will not open it:
// there the tabs hear a forget from storage alone, and a tab that storage tells
// nothing does not hear it.
const forgetChannel = openForgetChannel();

function openForgetChannel() {
  if (typeof BroadcastChannel !== "function") {
    return null;
  }
  try {
    const channel = new BroadcastChannel(FORGET_CHANNEL);
    channel.addEventListener("message", (event) => {
      if (event.data === FORGET_UNSTORED || (event.data === FORGET_MESSAGE && !storageAnswers())) {
        forgottenElsewhere(false);
      }
    });
    return channel;
  } catch {
    return null;
  }
}

// storageAnswers says whether this tab can read storage. One that cannot is
// told nothing by it, a forget in another tab included.
function storageAnswers() {
  try {
    localStorage.getItem(CREDENTIAL_STORAGE_KEY);
    return true;
  } catch {
    return false;
  }
}

// followCredentialStorage is a tab's answer to the badge changing in another
// tab of this browser: Forget this credential or Forget this browser pressed
// there, the server refusing the badge there as run out or replaced, or another
// address verified there. The browser holds one badge, in storage, so this tab
// reads it again, and a badged post from here sends only the badge the browser
// still holds; the next form or /verify page this tab draws shows it. If that
// is not the one this tab held, every posting form on screen offers the badge
// read now, unticked, or none (drawBadgeOption), and one that had the badge
// before ticked is noted to say so at its next press (withdrawBadgeOption,
// badgeWithdrawn), as is a held form a redraw has taken off the screen, once it
// is put back (restoreDraft); a refusal a form on screen was told, while it is
// still the notice on screen, is worded again for the badge read now
// (rewordRefusal); and /verify, when it shows the badge panel, is drawn again,
// unless the reader has drawn another step there by the time the page has asked
// the server (renderVerify). /verify showing the address step is left as it is,
// whatever badge it was drawn over, so a reader filling it in, or waiting on a
// code request, loses nothing, and what the page said there of a forget done in
// this tab (CREDENTIAL_FORGOTTEN in that step, BROWSER_FORGOTTEN under its
// button) stays as it was said. So is a code step, unless the badge read in is
// one newly verified there (codeSpentElsewhere). An emptied credential is the
// browser forgotten in another tab (forgetBrowser), and where this tab hears
// the forget (forgottenElsewhere) in place of all this, which starts no puzzle,
// since a forget starts nothing. The credential's removal after it, and a badge
// verified since, are changes of badge like any other.
function followCredentialStorage(event) {
  if (event.key !== CREDENTIAL_STORAGE_KEY && event.key !== null) {
    return;
  }
  if (event.key === CREDENTIAL_STORAGE_KEY && event.newValue === "") {
    forgottenElsewhere(true);
    return;
  }
  if (!readStoredBadge()) {
    return;
  }
  offerHeldBadge();
  const panel = el.main.querySelector('[data-action="forget-verification"]');
  if (onVerifyPage() && ageStatus() === "ok" && panel) {
    renderVerify();
  }
}

window.addEventListener("storage", followCredentialStorage);

// readStoredBadge reads the badge storage holds into the page, as it stands
// now, and says whether it is not the one the page held. A badge newly
// verified in another tab puts away a code step this tab drew
// (codeSpentElsewhere).
function readStoredBadge() {
  const offered = state.verification.badge;
  state.verification.badge = state.verification.enabled ? readBadge() : null;
  const held = state.verification.badge;
  const changed = held ? !(offered && held.token === offered.token) : offered !== null;
  if (held && changed) {
    codeSpentElsewhere();
  }
  return changed;
}

// What /verify says when the code step it showed is put away after another tab
// verified an address (codeSpentElsewhere), at once or once a confirmation of
// the step from here, on its way meanwhile, is refused (submitVerification). It
// says the two things that are so whenever it is shown, and not that the one
// caused the other: a confirmation from here that reached the server first and
// lost its answer spent the code itself.
const CODE_SPENT_ELSEWHERE = "An address was verified in another tab, and the code asked for here no longer works.";

// codeSpentElsewhere is a tab's answer to a badge newly verified in another:
// the browser has one verification in progress at a time, held by its cookie,
// so a code step this tab drew, with no confirmation of it on its way, is for a
// code that verified the badge there or was replaced by the one that did, and
// the server takes it no more. The step goes, so /verify next shows the badge
// panel, and is drawn again now, with a word on why, when a code step is on
// screen, unless the reader has drawn another step there by the time the page
// has asked the server (renderVerify): an address step the reader moved to
// meanwhile is left as it is. A confirmation of the step already on its way
// (Verify requests go one at a time, so there is at most one), counted on the
// step (confirming) whether its form is still on screen or /verify has been
// drawn again over it since, may have reached the server before the other
// tab's verification and verified this tab's address, so it is left to its
// answer: the step is only marked (spentElsewhere), and submitVerification
// settles it. A success stores the badge it earned and draws the panel for it,
// on /verify, once the page has asked the server; a refusal of any kind comes
// back here, and the step goes as above. A code request from this tab still on
// its way is not a code step yet, and draws its own when answered: the cookie
// that answer sets holds the browser's verification from then on, and its code
// is the one asked for.
function codeSpentElsewhere() {
  if (verifyStep.step !== "code") {
    return;
  }
  if (verifyStep.confirming > 0) {
    verifyStep.spentElsewhere = true;
    return;
  }
  verifyStep = { step: "email", label: "" };
  const step = el.main.querySelector('form[data-verify="confirm"]');
  if (onVerifyPage() && ageStatus() === "ok" && step) {
    renderVerify().then((drawn) => {
      if (drawn) {
        showNotice(CODE_SPENT_ELSEWHERE);
      }
    });
  }
}

// forgetBrowserHTML is the control for forgetBrowser, on the Verify page
// whatever state verification is in, since the key and the name are there
// for every poster. Its confirmation is said under the button
// (confirmBrowserForgotten), where the reader is looking: on a phone the
// notice at the top of the page is a screen or more above it.
function forgetBrowserHTML() {
  return `
    <div class="verify-forget forget-browser" id="forget-browser">
      <p><strong>Leaving a shared or borrowed computer?</strong> This browser keeps the random
        key your post IDs are made from and the name you last posted with, tripcode secret
        included. Whoever uses it next would get your IDs in the threads you posted in, which
        shows them which posts there are yours, and could post under your tripcode.</p>
      <p class="muted">Forgetting this browser removes the key, the name and any badge. Your
        next post starts a new key, and with it new IDs. Posts you have made stay as they
        are.</p>
      <button class="btn danger" type="button" data-action="forget-browser">Forget this browser</button>
      <p class="verify-done" id="forget-browser-done" role="status" tabindex="-1" hidden></p>
    </div>`;
}

const BROWSER_FORGOTTEN = "This browser no longer holds your post key, your name or a badge.";

// What /verify says in a tab that hears Forget this browser from another
// (forgottenElsewhere). It is worded as what happened, like
// CODE_SPENT_ELSEWHERE, so it is so whenever the tab hears and whatever is
// stored afterwards: a tab can hear late, over a badge or a key stored since
// the forget, which stays, and a badge can be verified in another tab while the
// notice is up. The tab the button was pressed in says BROWSER_FORGOTTEN under
// it (confirmBrowserForgotten).
const BROWSER_FORGOTTEN_ELSEWHERE =
  "This browser was forgotten in another tab, and whatever post key, name and badge it held then are gone.";

// revealClearOfHeader scrolls the page just far enough to bring el into view
// clear of the sticky header. focus() and scrollIntoView() do not know about
// the header: both take an element under it for one in view and leave it
// there. The Verify page draws each step at the scroll position the last one
// left, and on a phone, where the header wraps to three rows, a badge holder
// who had scrolled down to "Verify another address" got the address field,
// then the code box and its sentence, then the new badge, all drawn under the
// header, with only the buttons below them in sight. The page moves only when
// part of el is hidden, and by as little as it can; an el taller than the room
// under the header is shown from its top. The header is measured rather than
// assumed, since its height changes with the width and with the reader's text
// size. Whatever is then focused is focused with preventScroll, so that
// focus() does not move the page again.
function revealClearOfHeader(el) {
  const gap = 12;
  const header = document.querySelector(".site-header");
  const top = Math.max(0, header ? header.getBoundingClientRect().bottom : 0) + gap;
  const bottom = window.innerHeight - gap;
  const box = el.getBoundingClientRect();
  if (box.top < top || box.height > bottom - top) {
    window.scrollBy(0, box.top - top);
  } else if (box.bottom > bottom) {
    window.scrollBy(0, box.bottom - bottom);
  }
}

// confirmBrowserForgotten says that Forget this browser did its work, in the
// block the button is in, once the Verify page has been drawn again without
// what was forgotten. The redrawn page took the button, and with it the
// focus, so the confirmation takes both: it is brought into view and focused,
// for a screen reader to read and a keyboard to go on from. What was drawn
// above it may be shorter than what was there, and the block moves up with
// it, so it is brought into view clear of the header.
function confirmBrowserForgotten() {
  const done = document.getElementById("forget-browser-done");
  if (!done) return;
  done.textContent = BROWSER_FORGOTTEN;
  done.hidden = false;
  revealClearOfHeader(done);
  done.focus({ preventScroll: true });
}

// Each credential this page reads is numbered the first time it is read, and
// keeps that number for the visit, however often it is read again. A posting
// form's badge box carries the number of the badge it offers
// (data-badge-option), which is how a form tells the badge it named from one
// issued since (offerHeldBadge) without the credential going into the page.
// The numbers are this page's own count and say nothing about any badge.
const badgeNumbers = new Map();

function badgeNumber(token) {
  if (!badgeNumbers.has(token)) badgeNumbers.set(token, String(badgeNumbers.size + 1));
  return badgeNumbers.get(token);
}

// readBadge decodes the claims out of the credential this browser holds. The
// signature is not checked here and could not be — the key is the server's.
// This only decides what to display and whether to offer the checkbox; the
// server re-derives the label from its own record on every badged post, so a
// forged local claim buys nothing but a 403.
function readBadge() {
  const token = storedCredential();
  if (!token) {
    return null;
  }
  const parts = token.split(".");
  if (parts.length !== 3 || parts[0] !== "v1") {
    forgetCredential();
    return null;
  }
  try {
    const base64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    const padded = base64 + "=".repeat((4 - (base64.length % 4)) % 4);
    // Decoded as UTF-8 rather than through atob's byte string, so an
    // institution with an accent in its name reads correctly.
    const bytes = Uint8Array.from(atob(padded), (c) => c.charCodeAt(0));
    const claims = JSON.parse(new TextDecoder().decode(bytes));
    if (!claims.label || !claims.exp) {
      forgetCredential();
      return null;
    }
    const expires = new Date(claims.exp * 1000);
    if (expires <= new Date()) {
      forgetCredential();
      badgeLost = "Your badge has expired.";
      return null;
    }
    return { token, label: claims.label, expires, number: badgeNumber(token) };
  } catch {
    forgetCredential();
    return null;
  }
}

// loadVerification asks the server whether verification is on here, and reads
// the badge this browser holds when it is. Only an answer changes what the
// page holds about the server. A request that got none — given up
// (verifyDeadline), unreachable, or refused — says nothing about whether
// verification is on, so what the server said last stands, and the badge is
// read as it is after that answer; a body that could not be read is no answer
// either. Taken as "off", the GET given up after a Verify drew "not
// configured" where the badge just stored belonged, and took that badge off
// every posting form, and the footer's Verify link away, for the rest of the
// visit. With no answer yet, the page says it couldn't reach educhan rather
// than that verification is not configured (verifyViewHTML).
async function loadVerification() {
  let answer = null;
  try {
    // The only thing worth asking the server: whether the feature exists here.
    // What this browser holds is a local question with a local answer. An
    // issued badge is drawn after this request, while the next Verify request
    // waits, so it is given up in time like the others (verifyDeadline).
    answer = await api("/api/verify", { signal: verifyDeadline() });
  } catch {
    // Verification is an extra; the board works without knowing about it. A
    // failed request withdraws nothing the page knew: the feature stays as the
    // last answer had it, off only until one has come, and the badge is read
    // from storage as after any answer, so one confirmed just before this
    // request is still offered.
  }
  // What the server said last is read once the request is over, not before
  // it: another request to ask can have had its answer meanwhile.
  const answered = !!answer || state.verification.answered;
  const enabled = answer ? !!answer.enabled : state.verification.enabled;
  state.verification = { answered, enabled, badge: enabled ? readBadge() : null };
  // A posting form still on screen, the page before /verify while it is drawn,
  // offers the badge it was drawn for: switched off on the server, run out, or
  // replaced, that badge gives way to the one read now as for one gone in
  // another tab (followCredentialStorage), but starts no puzzle: this is
  // /verify being drawn over the form, and drawing a page never starts one. A
  // form noted here and drawn again by a later render starts it at the press
  // the note refuses.
  offerHeldBadge(true);
  if (el.verifyFooter) {
    el.verifyFooter.hidden = !enabled;
  }
  return state.verification;
}

// The badge is for students aged 18 and over, on their own word (build plan
// 2.14): the address step has a box to tick, and nothing is sent until it is
// ticked. BADGE_AGE_REFUSAL is what the page says when it is not;
// BADGE_AGE_REFUSED is the server's wording of the same refusal, which the
// page shows as BADGE_AGE_REFUSAL too. The answer is never stored, here or
// there: the box starts unticked on every render. It is not `required`, since
// the browser's own message would stand in for this one, which says what the
// visitor can still do.
const BADGE_AGE_REFUSAL =
  "The badge is for students aged 18 and over. You can still read and post anonymously without it.";
const BADGE_AGE_REFUSED = "the badge is for students aged 18 and over; you can still read and post anonymously";

// The server's refusals of a code after which no code is pending any more: it
// has dropped the verification, so the code box can do nothing and the page
// goes back to the address step, where a new code is asked for. They are known
// by the server's own words, which TestClientKnowsWhenNoCodeIsPending checks
// against what the server answers. A wrong code is not among them: the right
// one still works, since the server answers the wrong code that spends the
// last attempt with CODE_GUESSES_SPENT.
//
// An expired code reaches a browser as NO_CODE_PENDING, not CODE_EXPIRED: the
// cookie a code is confirmed against lasts exactly as long as the code, so
// once the code has expired the browser no longer sends it, and the server
// cannot tell which verification it was. CODE_EXPIRED is left the moment
// between the two (TestAnExpiredCodeIsSaidToHaveExpired).
const NO_CODE_PENDING = "no verification in progress, request a code first";
const CODE_EXPIRED = "that code has expired, request a new one";
const CODE_GUESSES_SPENT = "too many wrong codes, request a new one";

// Why the code box has gone, said in the address step that replaces it, next
// to the field the reader is sent to. The notice at the top of the page is
// not enough on its own: on a phone the box sits a screen below it, and the
// box would turn into an empty address form with its reason out of sight.
// NO_CODE_PENDING's note puts expiry first, since that is what it usually
// means; otherwise the code was confirmed in another tab, or replaced by a
// newer one asked for the same address somewhere else.
const CODE_ENDED_NOTES = {
  [NO_CODE_PENDING]: "That code has expired or can no longer be used.",
  [CODE_EXPIRED]: "That code has expired.",
  [CODE_GUESSES_SPENT]: "Too many wrong codes were tried, so that code no longer works.",
};

// Where a step's form says why it was refused: a wrong code, the 18+ box left
// unticked, an address the allowlist does not know, a cooldown, no answer. It
// sits under the form's fields, above its buttons, so it is next to the field
// it is about and in view of the button just pressed, and the fields are
// described by it. The notice at the top of the page was a screen above the
// form on a phone: a wrong code changed nothing in view but the button's
// label, and focus fell to the page.
const VERIFY_REFUSED_HTML = `<p class="verify-refused" id="f-verify-refused" role="alert" hidden></p>`;

// What a step's submit button says while its request is out: in the form that
// sent it (submitVerification), and in the step drawn again for a reader who
// came back to the page before the answer (verifyViewHTML). VERIFY_SOLVING is
// what Send code says before that, while the request waits on its puzzle
// (sendWithCaptcha), as a posting form's button does.
const VERIFY_SENDING = "Sending…";
const VERIFY_CHECKING = "Verifying…";
const VERIFY_SOLVING = "Checking…";

// refuseVerification says why the form was refused, in the form, and puts the
// reader back in the field it is about. invalid marks that field as what was
// refused, rather than the moment (a cooldown, a dropped connection); select
// selects what was typed there, for a code to be typed again whole. The form
// is brought into view clear of the header, and with it the field, the
// refusal under it and the button that was pressed.
//
// Saying it moves the step under the reader's finger, though nothing is drawn:
// the refusal opens above the buttons, and bringing the form clear of the
// header scrolls the page, down or up. So the step's buttons and its
// attestation let a press go for VERIFY_SETTLE_MS from here, as they do once a
// step is drawn (verifyShown). On a phone, the second tap of a double tap on
// Verify, a wrong code refused, pressed Keep my badge, which the scroll had
// brought up into Verify's place, and the code box went with the code still
// pending; and the second tap on Send code with the 18+ box unticked ticked
// it, its label brought down into Send code's place, under the refusal
// saying why it had to be ticked.
function refuseVerification(form, message, target, { invalid = false, select = false } = {}) {
  const why = form.querySelector(".verify-refused");
  why.textContent = message;
  why.hidden = false;
  revealClearOfHeader(form);
  verifyShown.at = performance.now();
  if (!target) return;
  if (invalid) target.setAttribute("aria-invalid", "true");
  target.focus({ preventScroll: true });
  if (select) target.select();
}

// clearVerifyRefusal takes a refusal back down when the form is sent again,
// and forgets it, so the step drawn again does not say it.
function clearVerifyRefusal(form) {
  verifyRefusal = null;
  const why = form.querySelector(".verify-refused");
  why.hidden = true;
  why.textContent = "";
  for (const input of form.querySelectorAll('[aria-invalid="true"]')) input.removeAttribute("aria-invalid");
}

// onVerifyPage says whether the Verify page is still the one on screen. Its
// requests can outlast it: a reader who presses Send code or Verify and then
// follows a link is on another page by the time the answer comes, and the
// answer must not draw the Verify page over that one. A reader who stays on
// the page but moves it to another step is submitVerification's sentFrom.
function onVerifyPage() {
  return state.route.view === "verify";
}

// The claim the page is making, in the words the README uses. It belongs on
// screen next to the form, not only in the repository. Who the badge is for
// comes first: most readers need to know they don't need one.
const VERIFY_INTRO = `
  <p class="verify-note">
    The badge is for students aged 18 and over who have a university or college
    email address. Getting one means confirming that you're 18 or over, and that
    answer isn't saved anywhere. You don't need a badge to use educhan:
    ${audienceText()} can still read and post anonymously without one.
  </p>
  <p class="verify-note">
    Verifying proves you hold an address at a recognised institution and earns
    you an optional badge. The address itself is never stored: what is kept is
    a keyed hash of it, in a table that has no link to any post or thread. The
    badge you get carries only the institution, an expiry and a random number,
    so no one — this server included — can work back from a post to your
    address. The random number is what keeps your ID the same within a thread
    on the posts you badge. It is not made from your address, and cannot be
    traced back to it.
  </p>
  <p class="verify-note muted">
    The badge is off by default on every post, and is sent to the server only
    on the posts you tick it for &mdash; never on the ones you don't.
  </p>`;

// badgeLostHTML says why the badge that was here has gone (badgeLost), at the
// top of the step drawn without it, then what the reader can do there. The
// code step says it as well as the address step: a badge can run out while a
// code for another is on its way, and the code step drawn again then lost its
// Keep my badge button with nothing to say why.
function badgeLostHTML(then) {
  return badgeLost ? `<p class="verify-note" id="badge-lost" tabindex="-1">${esc(badgeLost)} ${esc(then)}</p>` : "";
}

function verifyViewHTML() {
  const v = state.verification;

  // Off here, or not known to be on: the server said it is off, or every time
  // the page asked it got no answer (loadVerification), and then it says it
  // couldn't reach educhan, not that the server is not configured for it.
  // Forget this browser is local, and stays on offer either way.
  if (!v.enabled) {
    return `
      <div class="board-head"><h1>Institutional verification</h1></div>
      ${VERIFY_INTRO}
      <p class="empty">${v.answered ? "Verification is not configured on this server." : esc(UNREACHABLE)}</p>
      ${forgetBrowserHTML()}`;
  }

  // A request from this step is still out, and the step is being drawn again
  // for a reader who left the page and came back before the answer. Its form
  // is drawn as the one that sent it is: every button in it waiting, and the
  // submit button saying what it waits for, so nothing in the form can give
  // the step up, or send from it again, while the server acts on the request.
  // Forget this browser is outside the form and stays live.
  const busy = verifyBusy === verifyStep;
  const wait = busy ? " disabled" : "";

  // While a badge held here is being replaced, either step can go back to it:
  // asking for a new badge retires nothing until the new one is confirmed.
  const keep = v.badge
    ? `<button class="btn subtle" type="button" data-action="verify-keep"${wait}>Keep my badge</button>`
    : "";

  // A code is pending: the box to type it in is the page until the code is
  // confirmed or given up, whether or not this browser already holds a badge.
  // A badge held here is replaced only once the new one is confirmed, so it is
  // no reason to hide the box, and this step is decided on verifyStep.step
  // alone: a badge holder who asked for a code once saw the badge panel in its
  // place, with nowhere to type the code.
  if (verifyStep.step === "code") {
    return `
      <div class="board-head"><h1>Institutional verification</h1></div>
      ${VERIFY_INTRO}
      ${badgeLostHTML("The code on its way earns you a new one.")}
      <form class="verify-panel fields" data-verify="confirm">
        <p>A code is on its way. It earns you ${verifiedBadgeHTML(verifyStep.label)} and expires shortly.</p>
        <div class="field">
          <label class="label" for="f-code">Code from the email</label>
          <input type="text" id="f-code" name="code" inputmode="numeric" autocomplete="one-time-code"
                 maxlength="12" placeholder="6 digits" required aria-describedby="f-verify-refused" />
        </div>
        ${VERIFY_REFUSED_HTML}
        <div class="actions">
          <button class="btn primary" type="submit"${wait}>${busy ? VERIFY_CHECKING : "Verify"}</button>
          <button class="btn subtle" type="button" data-action="verify-restart"${wait}>Use a different address</button>
          ${keep}
        </div>
      </form>
      ${forgetBrowserHTML()}`;
  }

  // The badge panel, unless the reader is replacing the badge: a code asked
  // for over a badge, after Verify another address or with its request on its
  // way while another tab verified one, is typed in all the same, in the code
  // step above, since the badge it earns replaces the one held. A code step
  // drawn before another tab verified a badge is put away, once any
  // confirmation of it on its way is answered (codeSpentElsewhere).
  if (v.badge && !verifyStep.replacing) {
    return `
      <div class="board-head"><h1>Institutional verification</h1></div>
      ${VERIFY_INTRO}
      <div class="verify-panel">
        <p id="verify-badge" tabindex="-1">Your badge in this browser is ${verifiedBadgeHTML(v.badge.label)}, good until
          ${timeHTML(v.badge.expires.toISOString())}.</p>
        <p class="muted">
          Tick &ldquo;post with&rdquo; on the posting form to use it. It is kept
          in this browser and stays until it expires or you forget it &mdash;
          closing the tab no longer discards it. Verifying another address
          replaces it and retires this one.
        </p>
        <div class="verify-forget">
          <p><strong>On a shared or borrowed computer, forget it before you walk away.</strong>
            The badge stays in this browser for whoever sits down next, and it is
            usable by anyone who has the browser &mdash; it is the whole of what
            proves your institution. Forgetting this browser, below, removes it
            along with everything else that ties your posts to this browser.</p>
          <p class="muted">Forgetting is final: the server issues a badge once
            and cannot show you this one again, so you would verify from the
            start. It removes the badge from this browser only &mdash; the server
            was never told which browser held it.</p>
          <button class="btn danger" type="button" data-action="forget-verification">Forget this credential</button>
        </div>
        <div class="actions">
          <button class="btn subtle" type="button" data-action="verify-again">Verify another address</button>
        </div>
      </div>
      ${forgetBrowserHTML()}`;
  }

  // The code box this step replaced, and why it went, said at the top of the
  // form, where the box was, and read out with the address field, as is the
  // form's refusal once there is one.
  const ended = verifyStep.ended
    ? `<p class="verify-ended" id="f-email-ended">${esc(verifyStep.ended)} Send a new code to carry on.</p>`
    : "";
  const endedRef = verifyStep.ended ? "f-email-ended f-verify-refused" : "f-verify-refused";

  return `
    <div class="board-head"><h1>Institutional verification</h1></div>
    ${VERIFY_INTRO}
    ${badgeLostHTML("Verify again to get a new one.")}
    <form class="verify-panel fields" data-verify="request">
      ${ended}
      <div class="field">
        <label class="label" for="f-email">Institutional email</label>
        <input type="email" id="f-email" name="email" maxlength="254" autocomplete="email"
               placeholder="you@institution.edu" required aria-describedby="${endedRef}" />
        <span class="hint">Academic domains only &mdash; .edu, .ac.in, .ac.uk, .edu.au and others.</span>
      </div>
      <label class="attest" for="f-adult">
        <input type="checkbox" id="f-adult" name="adult" autocomplete="off" aria-describedby="f-verify-refused" />
        <span>I confirm I am 18 or over</span>
      </label>
      ${VERIFY_REFUSED_HTML}
      <div class="actions">
        <button class="btn primary" type="submit"${wait}>${busy ? (verifyStep.solving ? VERIFY_SOLVING : VERIFY_SENDING) : "Send code"}</button>
        ${keep}
      </div>
    </form>
    ${forgetBrowserHTML()}`;
}

// drawVerifyStep draws the Verify page for the step it is on, in place of
// whatever the page shows, and notes what it drew and when (verifyShown). Every
// drawing of a Verify step goes through here, so this is where why a badge went
// (badgeLost) is said once and then put away: the address step and the code
// step say it, and a badge panel drawn instead is for a badge held since.
// Said and put away only in renderVerify, it was lost to the code step, which
// did not say it, and never said by Keep my badge, which draws in place. It is
// also where the page notes what it was drawn for as a render notes it
// (drawnFor), with no thread and no image open, so that a press of its form is
// judged on that page (submitVerification) whatever the drawing replaced: the
// page a render of /verify still loading has yet to replace, say, or, for a
// drawing after a forget, the error of a /verify that would not load, which
// notes nothing. It notes that only while the Verify page is the one on screen
// (onVerifyPage), as every caller draws it: a step drawn over a page the reader
// is leaving, still on screen while the next loads, would have its press judged
// on the page they went to, and sent from there.
function drawVerifyStep() {
  el.main.innerHTML = verifyViewHTML();
  verifyShown = { step: verifyStep, node: el.main.firstElementChild, at: performance.now() };
  state.thread = null;
  expandedLink = null;
  if (onVerifyPage()) {
    drawnFor = { path: renderedPath, route: state.route, thread: null };
  }
  badgeLost = "";
}

// verifyStepOnScreen says whether the page still shows the drawing made for
// the step it is on: drawn for that step, and not drawn over since.
function verifyStepOnScreen() {
  return verifyShown.step === verifyStep && el.main.contains(verifyShown.node);
}

// sayVerifyRefusal says the refusal kept with the step (verifyRefusal) in the
// step's form, when there is one for the step the page is on. sentIt says the
// form is the one that sent the request, which still holds what was typed in
// it. Any other is the step drawn again, its fields empty, since nothing typed
// is kept with the step, so it is told the refusal without "What you wrote is
// still in the box" (redrawn): said there, under an empty field with the focus
// in it, that told the reader their address or code was where it was not.
function sayVerifyRefusal(form, sentIt = false) {
  const refused = verifyRefusal;
  if (!refused || refused.step !== verifyStep) return;
  refuseVerification(form, sentIt ? refused.message : refused.redrawn, field(form, refused.about), refused);
}

// renderVerify draws the Verify page once it has asked the server whether
// verification is on here, and says whether it did. render() comes here for a
// reader arriving at the page, and the page's own buttons and answers come here
// to draw a step after that request, as do the changes this tab hears of from
// another (forgottenElsewhere, followCredentialStorage, codeSpentElsewhere) and
// a badge refused on a post sent from another page (submit). The reader can
// leave while it asks — after pressing Verify, say, since an issued badge is
// drawn through here — and then nothing is drawn over the page they went to.
// Nothing is drawn or said either once a newer render has begun while it asks,
// for Verify in the header pressed again, say (overtaken): as for the other
// views, the page is that render's to draw.
//
// The page can also move to another step while it asks, and be drawn for that
// step in place, by an answer, a button or a forget heard from another tab.
// That drawing, while it is on screen, is left alone: drawing the step again
// would take away what was said there and where the reader was put. Forget this
// browser pressed while an issued badge was being drawn had its confirmation
// wiped, and the focus with it, by the badge's late redraw. But a drawing that
// has itself been drawn over is drawn again. A code box drawn while render()
// was still asking was replaced by the page the reader had just left, whose
// answer came in late, or by the loading skeleton; left out, it was nowhere,
// and that page stayed under the Verify address until the reader went somewhere
// else.
//
// The page is titled whether or not it is drawn here: render() titles each page
// as it draws it, and the drawing left alone is one it did not make.
async function renderVerify() {
  const step = verifyStep;
  const route = state.route;
  await loadVerification();
  if (!onVerifyPage() || overtaken(route)) return false;
  announce("Institutional verification");
  if (verifyStep !== step && verifyStepOnScreen()) return false;
  drawVerifyStep();
  const form = el.main.querySelector("form[data-verify]");
  if (form) sayVerifyRefusal(form);
  return true;
}

// showVerifyField brings a step's form, freshly drawn in place of another,
// into view clear of the header, from what it opens with to its buttons, and
// puts the reader in its field: the code box once a code is sent, the address
// field when another address is asked for.
function showVerifyField(id) {
  const input = document.getElementById(id);
  if (!input) return;
  revealClearOfHeader(input.form);
  input.focus({ preventScroll: true });
}

// showHeldBadge brings the badge panel's first line, the badge this browser
// holds, into view clear of the header and focuses it, once the panel is drawn
// for a badge just issued, or kept rather than replaced. That line is what
// changed: the rest of the panel reads the same whichever badge is held, so
// with the line out of sight a replaced badge looked like nothing had happened.
// On a phone the panel runs past the foot of the screen, so a control further
// down would be focused out of sight. The badge can have gone by the time the
// panel would be drawn — run out while the page was on another step — and the
// address step drawn in its place opens by saying so (badgeLost): that is
// what is shown and focused then.
function showHeldBadge() {
  const held = document.getElementById("verify-badge") || document.getElementById("badge-lost");
  if (!held) return;
  revealClearOfHeader(held);
  held.focus({ preventScroll: true });
}

/* Rules and FAQ ------------------------------------------------------------ */

// Both pages are readable before the age screen is answered, so neither may
// lean on anything fetched: they are the site's own words, not the API's.

// audienceText is who the site is open to, in the one form every page uses.
function audienceText() {
  return MIN_AGE > 0 ? `anyone aged ${MIN_AGE} and over` : "anyone";
}

// HELPLINES_HTML is where to get help, the same on the rules and the FAQ. It is
// one fixed list for everyone, because educhan never works out where a reader
// is: it names the countries and lets each reader find their own. The numbers
// are the build plan's (2.12), and the links leave without a referrer.
const HELPLINES_HTML = `
  <p class="callout">If someone is in danger right now, call your local emergency number.</p>
  <p>If something here upsets or worries you, step away and talk to someone you trust. If
    you're having a hard time, or you're worried about someone else, you can also call one of
    these helplines:</p>
  <ul>
    <li><strong>US:</strong> call or text <a href="tel:988">988</a></li>
    <li><strong>UK and Ireland:</strong> Samaritans, <a href="tel:116123">116 123</a></li>
    <li><strong>UK:</strong> Childline, <a href="tel:08001111">0800 1111</a></li>
    <li><strong>India:</strong> Tele-MANAS, <a href="tel:14416">14416</a></li>
    <li><strong>Australia:</strong> Lifeline, <a href="tel:131114">13 11 14</a>, and Kids
      Helpline, <a href="tel:1800551800">1800 55 1800</a></li>
    <li><strong>Anywhere else:</strong> find one near you at
      <a href="https://findahelpline.com" rel="noopener noreferrer">findahelpline.com</a></li>
  </ul>`;

// The rules name every kind of post that is not allowed, for everyone on every
// board, and say what happens to one using only what is built: moderators can
// delete posts and threads and lock a thread, and nothing else yet.
function renderRules() {
  el.main.innerHTML = `
    <article class="page" aria-labelledby="page-title">
      <h1 id="page-title">Rules</h1>
      <p class="lede">These rules are for everyone, on every board. educhan is open to
        ${audienceText()}, school students included, so keep everything you post fit for a
        school student to read.</p>
      <ol class="rules-list">
        <li><strong>Nothing sexual.</strong> No porn, nudity or sexual posts, and never any
          sexual talk with or about anyone under 18. Not as a joke, and not behind a
          spoiler.</li>
        <li><strong>Nothing that encourages suicide, self-harm or eating disorders,</strong>
          and nothing that shows how. Saying you're having a hard time is fine, and so is
          asking for help: see <a href="#help">where to get help</a>.</li>
        <li><strong>Never tell anyone to kill themselves.</strong> &ldquo;kys&rdquo; is not
          allowed here, not even as a joke.</li>
        <li><strong>No bullying.</strong> Don't pick on, insult, mock or gang up on
          anyone.</li>
        <li><strong>No hate.</strong> Don't attack people for who they are, such as their
          race, religion, nationality, sex, gender, sexuality or disability.</li>
        <li><strong>No threats.</strong> Don't threaten to hurt anyone, or to attack a school
          or anywhere else, not even as a joke.</li>
        <li><strong>No violence or gore.</strong> Don't encourage anyone to seriously hurt
          people, and don't post gory or shocking pictures or descriptions.</li>
        <li><strong>No dangerous stunts or challenges.</strong> Don't dare anyone, or show them
          how, to do something that could hurt them or someone else.</li>
        <li><strong>No pushing drugs or other harmful substances.</strong> Don't encourage
          anyone to take anything that could harm them.</li>
        <li><strong>No contact details, and no moving to other apps.</strong> Never post a
          phone number, an email address, a home or school address, or a Discord, Snapchat,
          Instagram, Telegram or WhatsApp name or link, yours or anyone else's. Don't ask
          anyone to talk to you somewhere else.</li>
        <li><strong>Don't name or point out real people you know.</strong> Never name a
          classmate, teacher or anyone else from your own life, or describe them so they could
          be recognised. You can ask for advice about a friend or teacher, as long as nobody
          could tell who they are.</li>
        <li><strong>Nothing illegal.</strong> Don't post or link to anything that breaks the
          law.</li>
        <li><strong>No spam.</strong> No ads, no posting the same thing again and again, and no
          filling threads with junk.</li>
        <li><strong>Don't pretend to be someone you're not.</strong> Don't pretend to speak for
          a school, a university or anyone else, and don't fake the verified badge.</li>
        <li><strong>Stay on topic.</strong> Post on the board your post belongs on.</li>
      </ol>
      <p>A board can add rules of its own. You'll find them under <strong>Rules</strong> at
        the top of the board.</p>
      <h2>No private messages, on purpose</h2>
      <p>educhan has no private messages, no profiles and no friend lists. Everything here is
        said in public, where everyone can see it. That's deliberate: someone who wants to get
        you on your own usually starts by asking to talk somewhere private. If anyone asks you
        to move to another app, you don't have to reply.</p>
      <h2>When a post breaks the rules</h2>
      <p>Moderators can delete posts and whole threads that break these rules, and can lock a
        thread so it takes no more replies. There's no report button yet, but one is
        coming.</p>
      <h2 id="help" tabindex="-1">Where to get help</h2>
      ${HELPLINES_HTML}
      <p class="muted">The <a href="/faq">FAQ</a> explains how anonymous you are
        here${MIN_AGE > 0 ? ", and why educhan asks when you were born" : ""}.</p>
    </article>`;
  announce("Rules");
}

// FAQ_SECTIONS are the FAQ's questions in order, for the page and its contents
// list alike. Every claim in an answer is one the code holds to; change the
// two together.
const FAQ_SECTIONS = [
  {
    id: "faq-what",
    question: "What is educhan?",
    answer: `
      <p>An anonymous imageboard for students: a set of boards where anyone can read and post
        without an account. There are boards for school and university subjects, and for
        student life, such as admissions, exams, careers and studying abroad.</p>
      <p>Threads don't last forever. A reply moves its thread to the top of its board, and
        threads that stop getting replies sink, move to the board's archive, and are deleted
        after a while.</p>`,
  },
  {
    id: "faq-who",
    question: "Who is educhan for?",
    answer:
      MIN_AGE > 0
        ? `
      <p>Students, and anyone else aged ${MIN_AGE} or over. If you're under ${MIN_AGE},
        educhan isn't for you.</p>
      <p>Everyone who uses educhan agrees to its <a href="/rules">rules</a>.</p>`
        : `
      <p>Students, and anyone else. There is nothing to sign up for. Everyone who uses
        educhan agrees to its <a href="/rules">rules</a>.</p>`,
  },
  // With the age screen switched off there is no question to explain.
  ...(MIN_AGE > 0
    ? [
        {
          id: "faq-age",
          question: "Why does educhan ask when I was born?",
          answer: `
      <p>Because educhan is only for people aged ${MIN_AGE} and over. Everyone is asked the
        same question, once, the first time they visit.</p>
      <p>Your answer never leaves your browser. The browser works out from the month and year
        whether you're old enough, then forgets them: they are never saved, and never sent to
        educhan or anyone else. All it keeps is the result, so you aren't asked on every
        visit. If the result is no, it doesn't ask again, unless you might have become old
        enough this month: it doesn't know the day you were born, so it can't be sure, and
        then it keeps nothing and asks again next time.</p>`,
        },
      ]
    : []),
  {
    id: "faq-private",
    question: "Why are there no accounts or private messages?",
    answer: `
      <p>On purpose. With no accounts, there's no profile, no followers or friends, and no list
        of your posts for anyone to look through. There's nothing to sign up for and no
        password to lose.</p>
      <p>With no private messages, everything said on educhan is said in public, where
        everyone can see it. Someone who wants to get you on their own, away from everyone
        else, usually starts by asking to talk privately, often on another app. So educhan
        has no private messages, and its <a href="/rules">rules</a> don't allow contact
        details or asking people to move to other apps. If anyone asks you to, you don't have
        to reply.</p>`,
  },
  {
    id: "faq-anonymity",
    question: "How anonymous am I?",
    answer: `
      <p>Fairly anonymous, but not completely. Here's what educhan does:</p>
      <ul>
        <li>There are no accounts, and every post says &ldquo;Anonymous&rdquo; unless you type
          a name.</li>
        <li>educhan doesn't save IP addresses, the numbers that identify an internet
          connection. They aren't written to its logs or its database, and posts aren't
          stored with one. When you post, ask for a badge code or start the
          <a href="#faq-check">anti-spam check</a>, the server holds your IP address in its
          memory, and only there, so it can stop anyone doing any of these too often. Once it
          no longer needs it for that, it clears it out the next time it tidies up.</li>
        <li>educhan never asks for your email address unless you want a badge, and even then
          it keeps only a scrambled fingerprint of it, never the address itself
          (<a href="#faq-badge">more about the badge</a>).</li>
        <li>Each post has an ID that's the same for all your posts in one thread and different
          in every other, so people can follow a conversation without following you around
          the site (<a href="#faq-ids">more about IDs</a>).</li>
      </ul>
      <p>But what you write can still give you away. Details about your class, your school,
        your town, your timetable or your friends can be enough for someone who knows you to
        work out that it's you. So:</p>
      <ul>
        <li>Don't post your name, your school, or photos of yourself or anyone else.</li>
        <li>Remember that photos can carry hidden details, such as where they were taken, and
          educhan doesn't remove them yet.</li>
        <li>A tripcode links every post that carries it, and a badge shows which university
          or college you're at.</li>
        <li>Your internet provider, the network you're on (a school's, say) and anyone who can
          use your device can see that you visit educhan.</li>
        <li>Your browser keeps the key your IDs are made from and the name you last posted
          with. On a shared computer, use <strong>Forget this browser</strong> on the
          <a href="/verify#forget-browser">Verify</a> page before you leave, or the next person
          gets your IDs in the threads you posted in, and your tripcode.</li>
      </ul>`,
  },
  {
    id: "faq-badge",
    question: "What is the verified badge?",
    answer: `
      <p>An optional label on a post, such as ${verifiedBadgeHTML("IIT Bombay")}, which shows
        that the poster has an email address at that university or college. It's for people
        aged 18 and over at universities and colleges, not schools, so please don't use a
        school email address. You don't need it to read or to post.</p>
      <p>Not every educhan site has the badge switched on. If this one doesn't, the
        <a href="/verify">Verify</a> page says so.</p>
      <ol>
        <li>On the <a href="/verify">Verify</a> page, enter your university or college email
          address and tick the box to confirm that you're 18 or over. educhan doesn't save
          your answer.</li>
        <li>Type in the six-digit code that's emailed to you.</li>
        <li>Your browser is given a badge naming the university or college, and keeps it until
          it expires. The Verify page shows when that is.</li>
        <li>To use it, tick <strong>post with</strong> the badge on the posting form. It's off
          by default, and it's sent only with the posts you tick it on.</li>
      </ol>
      <p>The address itself is never stored. The server keeps only a scrambled fingerprint of
        it, so that one address holds one badge at a time, in a table with no link to any
        post. The badge holds the institution's name, an expiry date and a random number, and
        nothing about you. So nothing stored with a badged post points to your email address,
        or to the posts you make without the badge &mdash; unless you give both the same name or tripcode,
        which would show they're yours. That's why the Name field empties when you tick the
        badge.</p>
      <p>On a shared computer, use <strong>Forget this browser</strong> on the
        <a href="/verify#forget-browser">Verify</a> page before you leave. The badge stays in
        that browser for whoever uses it next, and so do the key your IDs are made from and
        your name.</p>`,
  },
  {
    id: "faq-check",
    question: "What is the anti-spam check?",
    answer: `
      <p>On sites that have it switched on, your browser solves a small maths puzzle before
        each post goes, and before it asks for a badge code. You don't have to do anything:
        the puzzle starts when you move into a box to write in, quote a post or attach an
        image, and usually takes a few seconds, while you write. That costs a person almost
        nothing, and slows down a program that posts over and over. If the puzzle hasn't
        finished when you press the button, the button says <strong>Checking…</strong>
        until it has.</p>
      <p>The puzzle comes from educhan itself, not from Google or any other company, and no
        cookie is set for it. Each answer is used once, for one post or one code request. So
        that it can't be used twice, the server keeps a scrambled fingerprint of the puzzle for
        a day or so, which holds nothing about you or what you wrote. Posts with the
        <a href="#faq-badge">verified badge</a> skip the check.</p>`,
  },
  {
    id: "faq-tripcodes",
    question: "What are tripcodes?",
    answer: `
      <p>Type a name, then <code>#</code> and a secret, in the Name field.
        <code>ana#my-long-secret</code> posts as <strong>ana</strong> followed by a tripcode
        such as <code>!!Ab3dE5fG7h</code>. The same secret always gives the same tripcode, so
        you can show that several posts are yours without an account.
        <code>ana##my-long-secret</code> works exactly the same way.</p>
      <p>The server mixes a secret of its own into yours before making the tripcode, so
        nobody can crack it offline by trying guesses on their own computer. But anyone who
        learns your secret can post with your tripcode, so pick a long one and use it
        nowhere else, least of all as a password. And since a tripcode links every post that
        carries it, use one only when that's what you want.</p>`,
  },
  {
    id: "faq-ids",
    question: "What is the ID on each post?",
    answer: `
      <p>Every post shows <strong>ID:</strong> and eight characters. All your posts in one
        thread share an ID, and you get a different one in every thread, so readers can tell
        who's who in a conversation but can't follow anyone from thread to thread.</p>
      <p>The ID is made on the server from the thread and a random key your browser creates
        the first time you post. The browser keeps the key and sends it only with your
        posts; the server uses it to make the ID and doesn't store it. A post with the
        verified badge takes its ID from the badge instead, so it never shares an ID with
        your posts without one. <strong>Forget this browser</strong> on the
        <a href="/verify#forget-browser">Verify</a> page, or clearing this site's data, starts a
        new key, and with it new IDs.</p>`,
  },
  {
    id: "faq-markup",
    question: "How do I format a post?",
    answer: `
      <dl class="markup-list">
        <dt><code>&gt;text</code> at the start of a line</dt>
        <dd>Greentext: the line shows in green. Used for quoting and for telling stories.</dd>
        <dt><code>&gt;&gt;123</code></dt>
        <dd>Links to post 123 on the same board. In a thread, clicking a post's number adds
          this to your reply.</dd>
        <dt><code>&gt;&gt;&gt;/math/</code>, <code>&gt;&gt;&gt;/math/123</code></dt>
        <dd>Link to another board, or to post 123 on it. For an institution's board, write
          <code>&gt;&gt;&gt;/u/iitb/</code>.</dd>
        <dt><code>[spoiler]text[/spoiler]</code></dt>
        <dd>Hides the text until a reader reveals it.</dd>
        <dt><code>[code]text[/code]</code></dt>
        <dd>A block of code, in a fixed-width font with its spacing kept.</dd>
        <dt><code>[math]\\frac{a}{b}[/math]</code></dt>
        <dd>Maths written in TeX, inside a line of text.</dd>
        <dt><code>[eqn]\\sqrt{x^2 + y^2}[/eqn]</code></dt>
        <dd>Maths written in TeX, on a line of its own.</dd>
      </dl>
      <p>Not every educhan site lets you post images. Where you can, an image can be marked
        <strong>Spoiler image</strong>, which hides it behind a plain tile until someone clicks
        it.</p>`,
  },
  {
    id: "faq-threads",
    question: "What are the bump limit, image limit and archive?",
    answer: `
      <p>Each board lists its most recently bumped threads first. A reply bumps its thread
        to the top, unless you tick <strong>sage</strong>, which replies without bumping.</p>
      <ul>
        <li><strong>Bump limit.</strong> Once a thread has as many replies as its board's bump
          limit, replies stop bumping it, so it sinks and makes room for new threads. It
          still takes replies until it's archived.</li>
        <li><strong>Image limit.</strong> On a site that lets you post images, each board caps
          how many images one thread can hold. A thread shows its count, such as &ldquo;12 of
          150 images&rdquo;, and once it's full, text replies still work.</li>
        <li><strong>The archive.</strong> Each board holds a set number of threads. When a new
          thread pushes the least recently bumped one off the board, that thread moves to
          the board's archive, where it can still be read and linked to but takes no
          replies. It's deleted after the number of days the archive page shows.</li>
      </ul>`,
  },
  {
    id: "faq-help",
    question: "Where can I get help?",
    answer: HELPLINES_HTML,
  },
];

function renderFAQ() {
  const contents = FAQ_SECTIONS.map((s) => `<li><a href="#${s.id}">${s.question}</a></li>`).join("");
  const sections = FAQ_SECTIONS.map(
    (s) => `
      <section aria-labelledby="${s.id}">
        <h2 id="${s.id}" tabindex="-1">${s.question}</h2>
        ${s.answer}
      </section>`
  ).join("");
  el.main.innerHTML = `
    <article class="page" aria-labelledby="page-title">
      <h1 id="page-title">Frequently asked questions</h1>
      <nav class="page-contents" aria-label="Questions on this page"><ul>${contents}</ul></nav>
      ${sections}
    </article>`;
  announce("FAQ");
}

/* Age screen --------------------------------------------------------------- */

// The age screen is a neutral question asked once, before anything of the site
// is shown or fetched. Neutral means it gives nothing away: no threshold, no
// preselected year, no "I am old enough" box to tick, and no second go at the
// question once it has said no. It is a question taken on trust, not proof, and
// the answer never leaves the page: not stored, not sent, not put in the URL.

let ageScreen = null;
// This page's own outcome, for a browser whose storage will not keep it: the
// screen is then asked again on the next visit, but not on every navigation.
let ageAnswer = "";

// ageStatus is "ok" once the screen is passed, "refused" once it has said no,
// and "ask" until either. A refusal outranks a pass, wherever either came from.
function ageStatus() {
  if (MIN_AGE <= 0) {
    return "ok";
  }
  if (ageAnswer === "refused" || storageGet(AGE_REFUSED_STORAGE_KEY) === "1") {
    return "refused";
  }
  if (ageAnswer === "ok" || storageGet(AGE_OK_STORAGE_KEY) === "1") {
    return "ok";
  }
  return "ask";
}

// ageFrom is the age someone born in that month and year has certainly reached.
// With no day to go on, a birthday in the current month may not have come yet,
// so it is counted as not yet: the guess only ever errs young.
function ageFrom(month, year, now = new Date()) {
  const age = now.getFullYear() - year;
  return month >= now.getMonth() + 1 ? age - 1 : age;
}

// The selects have no name and the form submits to the dialog, so even with
// the handler gone there is nothing for the browser to send anywhere.
// Autocomplete is off so the browser does not offer to remember a birth date
// the site itself refuses to keep.
function ageQuestionHTML() {
  const thisYear = new Date().getFullYear();
  const months = MONTHS.map((name, i) => `<option value="${i + 1}">${name}</option>`).join("");
  const years = Array.from({ length: AGE_YEARS + 1 }, (_, i) => thisYear - i)
    .map((year) => `<option value="${year}">${year}</option>`)
    .join("");
  return `
    <form class="age-step fields" method="dialog" novalidate>
      <h2 class="age-title" id="age-title">When were you born?</h2>
      <p class="age-note" id="age-note">Your answer stays in this browser and is never saved
        or sent anywhere.</p>
      <div class="age-fields" role="group" aria-labelledby="age-title" aria-describedby="age-note">
        <div class="field">
          <label class="label" for="age-month">Month</label>
          <select id="age-month" autocomplete="off" required aria-describedby="age-error">
            <option value="">Select&hellip;</option>${months}
          </select>
        </div>
        <div class="field">
          <label class="label" for="age-year">Year</label>
          <select id="age-year" autocomplete="off" required aria-describedby="age-error">
            <option value="">Select&hellip;</option>${years}
          </select>
        </div>
      </div>
      <p class="age-error" id="age-error" role="alert" hidden></p>
      <div class="actions">
        <button class="btn primary" type="submit">Next</button>
      </div>
    </form>`;
}

function ageAgreeHTML() {
  return `
    <div class="age-step">
      <h2 class="age-title" id="age-title">Welcome to educhan</h2>
      <p id="age-agree">By continuing you agree to the <a href="/rules">rules</a>.</p>
      <p class="age-note">The <a href="/faq">FAQ</a> explains how anonymous you are here,
        and where to get help.</p>
      <div class="actions">
        <button class="btn primary" type="button" data-action="age-continue"
                aria-describedby="age-agree">Continue</button>
      </div>
    </div>`;
}

function ageRefusedHTML() {
  return `
    <section class="age-refused" aria-labelledby="page-title">
      <h1 id="page-title">Sorry, you can't use educhan</h1>
      <p>educhan is for people aged ${MIN_AGE} and over.</p>
    </section>`;
}

// renderAgeGate stands in for a page the age screen has not let through.
function renderAgeGate(status) {
  if (status === "refused") {
    closeAgeScreen({ remove: true });
    el.main.innerHTML = ageRefusedHTML();
    announce("Not available");
    return;
  }
  // Nothing of the site sits behind the screen, not even the loading text.
  el.main.innerHTML = "";
  document.title = "educhan";
  openAgeScreen();
}

// openAgeScreen shows the screen as it was last left: a reader who went to
// read the rules from it comes back to the same step, not to the question.
function openAgeScreen() {
  if (!ageScreen) {
    ageScreen = document.createElement("dialog");
    ageScreen.className = "age-screen";
    ageScreen.setAttribute("aria-labelledby", "age-title");
    ageScreen.innerHTML = ageQuestionHTML();
    // Escape must not wave anyone through. Cancelling the keydown stops the
    // close request in the first place, cancelling "cancel" covers any that
    // get through, and the close handler covers the rest.
    ageScreen.addEventListener("keydown", (event) => {
      if (event.key === "Escape") event.preventDefault();
    });
    ageScreen.addEventListener("cancel", (event) => event.preventDefault());
    ageScreen.addEventListener("close", reopenAgeScreen);
    ageScreen.addEventListener("submit", answerAgeQuestion);
    ageScreen.addEventListener("change", clearAgeError);
    ageScreen.addEventListener("click", (event) => {
      if (event.target.closest('[data-action="age-continue"]')) {
        passAgeScreen();
      }
    });
    document.body.append(ageScreen);
  }
  if (!ageScreen.open) {
    ageScreen.showModal();
  }
  focusAgeScreen();
}

function focusAgeScreen() {
  const target = ageScreen && ageScreen.querySelector('[data-action="age-continue"], select');
  if (target) target.focus();
}

// closeAgeScreen puts the screen away: kept, for a page readable without it,
// or removed for good once it has an answer.
function closeAgeScreen({ remove = false } = {}) {
  const dialog = ageScreen;
  if (!dialog) {
    return;
  }
  if (remove) {
    ageScreen = null;
  }
  if (dialog.open) {
    dialog.close();
  }
  if (remove) {
    dialog.remove();
  }
}

// Chrome lets a repeated Escape through even when "cancel" is cancelled, if
// nothing was clicked in between. A screen that closes with no answer, on a
// page that needs one, therefore opens again.
function reopenAgeScreen() {
  if (
    ageScreen &&
    ageScreen.isConnected &&
    !ageScreen.open &&
    ageStatus() === "ask" &&
    !UNGATED_VIEWS.has(state.route.view)
  ) {
    ageScreen.showModal();
    focusAgeScreen();
  }
}

function clearAgeError() {
  const error = ageScreen && ageScreen.querySelector("#age-error");
  if (!error) {
    return;
  }
  error.hidden = true;
  error.textContent = "";
  ageScreen.querySelectorAll("select").forEach((select) => select.removeAttribute("aria-invalid"));
}

function answerAgeQuestion(event) {
  event.preventDefault();
  const month = ageScreen.querySelector("#age-month");
  const year = ageScreen.querySelector("#age-year");
  const missing = [month, year].filter((select) => !select.value);
  if (missing.length > 0) {
    const error = ageScreen.querySelector("#age-error");
    error.textContent =
      missing.length === 2 ? "Choose a month and a year." : `Choose a ${missing[0] === month ? "month" : "year"}.`;
    error.hidden = false;
    missing.forEach((select) => select.setAttribute("aria-invalid", "true"));
    missing[0].focus();
    return;
  }
  const age = ageFrom(Number(month.value), Number(year.value));
  // ageFrom errs young, so someone born in this month MIN_AGE years ago may
  // already be old enough. They are refused for now, but the refusal is not
  // kept: a final refusal would shut out for good someone who turned MIN_AGE a
  // few days ago, and by next month the same answer passes.
  const unsure = age === MIN_AGE - 1 && Number(month.value) === new Date().getMonth() + 1;
  // Either way the question goes, and the month and year with it: from here on
  // they are nowhere on the page, let alone anywhere else.
  if (age >= MIN_AGE) {
    ageScreen.innerHTML = ageAgreeHTML();
    focusAgeScreen();
  } else {
    refuseAgeScreen({ keep: !unsure });
  }
}

function passAgeScreen() {
  storageSet(AGE_OK_STORAGE_KEY, "1");
  ageAnswer = "ok";
  closeAgeScreen({ remove: true });
  render();
}

// A refusal is final in this browser: the screen is removed rather than reset,
// and nothing on the page offers to ask again. The one refusal not kept is for
// an answer that may already be old enough (see answerAgeQuestion); it lasts
// until the page is reloaded or closed.
function refuseAgeScreen({ keep = true } = {}) {
  if (keep) {
    storageSet(AGE_REFUSED_STORAGE_KEY, "1");
  }
  ageAnswer = "refused";
  closeAgeScreen({ remove: true });
  render();
}

/* Routing ------------------------------------------------------------------ */

function parseRoute(location = window.location) {
  let parts;
  try {
    parts = location.pathname.split("/").filter(Boolean).map(decodeURIComponent);
  } catch {
    // A malformed escape in a hand-typed address names nothing.
    return { view: "notfound" };
  }
  const query = new URLSearchParams(location.search);

  if (parts.length === 0) {
    return { view: "home" };
  }
  // Checked before the board route, and reserved server-side too, so no board
  // can take one of these paths out from under its page.
  if (parts.length === 1 && PAGE_VIEWS.has(parts[0])) {
    return { view: parts[0] };
  }
  if (parts[0] === "u") {
    // /u/ alone is the namespace, not a board.
    if (parts.length < 2) {
      return { view: "notfound" };
    }
    return boardRoute(INSTITUTION_PREFIX + parts[1], parts.slice(2), query);
  }
  // A reserved word without a page of its own yet is still not a board: it
  // gets the not-found view rather than a request for a board by that name.
  if (RESERVED_PATHS.has(parts[0])) {
    return { view: "notfound" };
  }
  return boardRoute(parts[0], parts.slice(1), query);
}

// boardRoute reads the part of a path below a board's own address, which is
// the same whether the board lives at /{slug}/ or /u/{name}/.
function boardRoute(slug, rest, query) {
  const [section, no] = rest;
  if (rest.length === 0) {
    return { view: "index", slug, page: Math.max(Number(query.get("page")) || 1, 1) };
  }
  if (rest.length === 1 && section === "catalog") {
    return { view: "catalog", slug };
  }
  if (rest.length === 1 && section === "archive") {
    return { view: "archive", slug };
  }
  if (rest.length === 2 && section === "thread" && /^\d+$/.test(no)) {
    return { view: "thread", slug, no: Number(no) };
  }
  if (rest.length === 2 && section === "post" && /^\d+$/.test(no)) {
    return { view: "post", slug, no: Number(no) };
  }
  return { view: "notfound" };
}

function routeKey(route) {
  return `${route.view}:${route.slug || ""}:${route.no || ""}`;
}

// emptyStateHTML is a dead end with a way out: back to the board list, and to
// the board itself when the board is known and only something on it is not.
function emptyStateHTML(message, slug = null) {
  const board = slug ? `<a href="${boardPath(slug)}">Go to ${esc(boardLabel(slug))}</a> &middot; ` : "";
  return `<p class="empty">${esc(message)}<br />${board}<a href="/">Back to the board list</a></p>`;
}

// The loading state, the dimming and the skeleton that may follow it, is the
// latest render's. rendersBegun counts the renders begun, whatever their view,
// and beginning one ends the loading state of any before it (endLoading), so a
// page that loads nothing, the rules say, is never left dimmed, or blanked, by
// an older render still loading. loadingTimer times the latest one's skeleton.
let rendersBegun = 0;
let loadingTimer = 0;

// endLoading takes the loading state off the page: the dimming, and the
// skeleton not yet drawn.
function endLoading() {
  clearTimeout(loadingTimer);
  el.main.removeAttribute("aria-busy");
}

// beginLoading dims the current view and only falls back to a skeleton if the
// request is slow enough to notice, which keeps fast navigations from flashing.
// skeleton says, when that time comes, whether to draw it: while it says no,
// the current view stays, dimmed, however long it takes. The skeleton is drawn,
// and the finish it returns takes the dimming off, only while its render is
// still the latest: an overtaken render's skeleton would blank the newer page
// for good, since the overtaken render then draws nothing, and its finish would
// show a newer render still loading, a kept page say, as loaded. The latest is
// counted rather than asked of overtaken: a kept page whose redraw fails puts
// back the route it was drawn for, which overtakes that redraw too. A skeleton
// drawn leaves the page it replaces (leavePage).
//
// The skeleton stands in for the view it dims, and for nothing drawn since: a
// Verify step that an answer drew in place while the page was loading stays
// as it was drawn, with the field it put the reader in, rather than going
// under a skeleton to be drawn again without it (renderVerify), and so does
// /verify drawn again for a forget heard from another tab while a render of it
// loads (forgottenElsewhere). What it hands back runs once the page is drawn,
// a pending code's box included, so it is held whole
// (TestVerifyCodeBoxShowsWhileACodeIsPending).
function beginLoading({ skeleton = () => true } = {}) {
  const begun = rendersBegun;
  el.main.setAttribute("aria-busy", "true");
  const dimmed = el.main.firstChild;
  loadingTimer = setTimeout(() => {
    if (begun === rendersBegun && el.main.firstChild === dimmed && skeleton()) {
      el.main.innerHTML = '<div class="skeleton"><div class="skeleton-row"></div><div class="skeleton-row"></div><div class="skeleton-row"></div></div>';
      leavePage();
    }
  }, 250);
  return () => {
    if (begun === rendersBegun) {
      endLoading();
    }
  };
}

// resolvePost finds the thread a board-wide post number belongs to, so a link
// can name a post — a cross-board quote, say — without knowing where it lives.
async function resolvePost(route) {
  const found = await api(`/api/boards/${encodeURIComponent(route.slug)}/posts/${route.no}`);
  return `${threadPath(found.board || route.slug, found.thread_no)}#p${found.no}`;
}

// overtaken says whether a newer render has begun since the one for route. An
// overtaken render draws nothing, loaded or not, and leaves the screen to the
// newer one: the board and thread views ask once their request is answered,
// and render after the board list and before anything else it does to the
// page. A redraw that fails and keeps its page puts back the route that page
// was drawn for, which overtakes every render still loading. /verify asks once
// it has asked the server, for a render or a redraw of the page alike, and asks
// too whether it is still the page on screen, and whether the reader has drawn
// another step there meanwhile (renderVerify).
function overtaken(route) {
  return state.route !== route;
}

// leavePage forgets what the page on screen gave its controls to read, its
// thread and the image open on it, as render draws the skeleton, the age screen
// or an error in its place. A page drawn in its place sets both for itself
// (render, drawVerifyStep).
function leavePage() {
  state.thread = null;
  expandedLink = null;
}

// The draft defaults to whatever is on screen now; a render that hands over to
// another passes its own along, because by then the screen may be a skeleton.
async function render({ draft = captureDraft() } = {}) {
  // /u-{name}/ is the stored slug typed as a path. The board answers to it,
  // but the address it is shared under is /u/{name}/.
  const { pathname, search, hash } = window.location;
  if (/^\/u-[^/]/.test(pathname)) {
    window.history.replaceState(null, "", pathname.replace(/^\/u-/, "/u/") + search + hash);
  }
  // The address asked for, less its fragment, which a jump within the page
  // moves without going anywhere else.
  const requested = window.location.pathname + window.location.search;
  renderedPath = requested;
  const route = parseRoute();
  state.route = route;
  // The loading state is this render's from here: whatever an older one left
  // loading ends (beginLoading).
  rendersBegun += 1;
  endLoading();
  // What the page on screen gives its controls to read, its thread and the
  // image open on it, stays as it is while this render loads, whatever it loads
  // (a post link's lookup, say, and the thread it goes on to), until something
  // is drawn in the page's place (leavePage, drawVerifyStep, and below): a
  // refusal answered meanwhile updates the page it is shown on, and Escape
  // closes the image open there. A redraw with the routeKey of the page a post
  // is on its way from, or whose form has yet to be told what it missed off the
  // screen, or whose form is pressed while it loads, Update say, keeps the page
  // on screen (below), where a post link's lookup, whose routeKey is its own,
  // does not. That is read as the draft stands when it is used (carriedDraft).
  const keepPage = () => {
    const carried = carriedDraft(draft);
    return Boolean(carried && carried.held && carried.key === routeKey(route));
  };
  renderNav(route);
  // The notice goes as a render begins, but for a refusal said before anything
  // was sent at a press of the form this render keeps the page on screen for,
  // while it is still the notice on screen (unsentOnScreen): one said as a post
  // link's lookup into the form's own thread loaded stays up while the thread
  // loads, as one said during Update does, for the page drawn to word it again
  // for its form (restoreDraft) or the page kept to keep it (below).
  if (!(keepPage() && unsentOnScreen(carriedDraft(draft).held))) {
    clearNotice();
  }
  // A refusal said in a Verify form goes with the notice: the page arrived at
  // is a new visit, and a step drawn there starts without it.
  verifyRefusal = null;
  hideQuotePreview();

  // Until the age screen has its answer, nothing of the site is shown or even
  // fetched, apart from the two pages that say what answering means. The screen
  // takes the page's place, and a refusal left up above for the page this
  // render would have kept goes with it: it speaks of a form that has gone.
  const age = ageStatus();
  const ungated = UNGATED_VIEWS.has(route.view);
  if (age !== "ok" && !ungated) {
    clearNotice();
    leavePage();
    renderAgeGate(age);
    return;
  }
  // Those two are read with the screen put away; it comes back, as it was
  // left, on the next page that needs it.
  closeAgeScreen();

  // A redraw with the routeKey of the page a post is on its way from, or whose
  // form has yet to be told what it missed off the screen (captureDraft), keeps
  // the page on screen, dimmed, rather than a skeleton, so the form stays in
  // the document until it goes into the new page (restoreDraft), and so does
  // one whose form is pressed before the skeleton would be drawn
  // (carriedDraft).
  const finish = beginLoading({ skeleton: () => !keepPage() });
  let resolved = null;
  try {
    if (age === "ok") {
      try {
        await loadSite();
      } catch (err) {
        // The rules and the FAQ need nothing from the server, so a board list
        // that will not load costs them only the navigation.
        if (!ungated) throw err;
      }
      if (overtaken(route)) {
        return;
      }
      renderNav(route);
    }
    switch (route.view) {
      case "home":
        renderHome();
        break;
      case "index":
        await renderIndex(route);
        break;
      case "catalog":
        await renderCatalog(route);
        break;
      case "archive":
        await renderArchive(route);
        break;
      case "thread":
        await renderThread(route);
        break;
      case "post":
        resolved = await resolvePost(route);
        break;
      case "verify":
        await renderVerify();
        break;
      case "rules":
        renderRules();
        break;
      case "faq":
        renderFAQ();
        break;
      default:
        el.main.innerHTML = emptyStateHTML("That is not a page on this site.");
        announce("Page not found");
    }
  } catch (err) {
    if (overtaken(route)) {
      return;
    }
    // An expired thread and a mistyped board look the same from here, and both
    // are better served by a way out than by the raw error. A post number the
    // server cannot take at all (0, or more digits than a post number has) is
    // a post that is not there either.
    const notFound = err.status === 404 || (route.view === "post" && err.status === 400);
    let message = err.unreachable ? UNREACHABLE : `Could not load this page: ${err.message}`;
    let board = null;
    if (notFound) {
      // The board is named, and linked, only when it is one this site has:
      // the slug is whatever the address said, and echoing an arbitrary one
      // would let a crafted link put a sentence of its choosing on the page.
      const known = route.view === "post" && state.boards.some((b) => b.slug === route.slug);
      message = known
        ? `${Number.isSafeInteger(route.no) ? `No. ${route.no}` : "That post"} is not on ${boardLabel(route.slug)} — it may have been deleted, or the number may be wrong.`
        : "That board or thread is not here — it may have been deleted, or have aged out of the archive.";
      board = known ? route.slug : null;
    }
    // A redraw that kept the page for its form (above) leaves the page there,
    // form and all, when the new one will not load: a post still on its way
    // goes ahead and is answered on its form, and a success goes to the new
    // post, as without the redraw. The address bar, renderedPath and the route
    // are put back as the page was drawn for them (drawnFor), however many
    // redraws of it have failed, and so is its thread, which a refusal answered
    // meanwhile has updated; the image open on it stays open. The address bar
    // goes back by replacing the history entry the reader is on, the fragment
    // kept, so a jump within the page or a reload stays on the page kept. After
    // a link, with nothing pressed while the redraw loads, the history then
    // holds that page twice, and the first Back from it stays on it (popstate
    // finds the address the page was drawn for and only moves to the fragment);
    // a Back or Forward that will not load leaves the entry it went to holding
    // the page kept. A link pressed while a redraw for another address loads,
    // the same one again or a jump within the page, adds an entry, and that is
    // the one replaced, so the entry for the page that failed is left behind it
    // and Back loads that page again. After Update, whose page is the one kept,
    // the entry a jump within the page leaves behind is the kept page's own,
    // and Back stays on it. The error is a notice above the page, after
    // anything said since the redraw began, the post's own answer say. The
    // notice is an alert, so it is heard, and the title stays the page's. A
    // form answered while it was off the screen, as a post link's lookup into
    // its thread loaded say, is told first what it would have been told on
    // screen (unsaidNotices), worded as it is told, for the form as it stands
    // then, its text, its image and its badge box included, its button given
    // the focus and a refusal of its badge said in place of the note the
    // refusal left, as restoreDraft does when the redraw draws; and a refusal
    // of its image switches its picker off and puts the thread's counts in
    // step, as on screen (blockRefusedPicker), which a page drawn afresh does
    // itself. The draft is read as it stands now, so a form pressed while the
    // redraw loaded is that form, held (carriedDraft). A refusal the form was
    // told while the redraw loaded, still the notice on screen, one said before
    // anything was sent at a press during a post link's lookup into its thread
    // included (above), stays with the error after it, as does one told now,
    // and each but a refusal of the image itself is kept with what follows it
    // (refusalNotices), for it to be worded again in place with that left after
    // it (rewordRefusal).
    draft = carriedDraft(draft);
    if (keepPage() && draft.held.isConnected) {
      renderedPath = drawnFor.path;
      window.history.replaceState(window.history.state, "", drawnFor.path + window.location.hash);
      state.route = drawnFor.route;
      state.thread = drawnFor.thread;
      const unsaid = takeUnsaidNotice(draft.held);
      if (unsaid && unsaid.badgeRefused) {
        badgeWithdrawn.delete(draft.held);
      }
      if (unsaid && unsaid.imageRefused) {
        blockRefusedPicker(draft.held, unsaid.limitReached);
      }
      const { text, image } = formHolds(draft.held);
      const words = unsaid ? unsaid.word(draft.held, text, image) : "";
      const said = noticeSaid();
      const told = [words, said, message].filter(Boolean).join(" ");
      showNotice(told);
      const refused = refusalNotices.get(draft.held);
      if (refused && (unsaid || (said !== "" && said === refused.told))) {
        const after = [unsaid ? said : refused.after, message].filter(Boolean).join(" ");
        refusalNotices.set(draft.held, { ...refused, told, after });
      }
      if (unsaid && unsaid.focused && (!document.activeElement || document.activeElement === document.body)) {
        draft.held.querySelector('button[type="submit"]')?.focus({ preventScroll: true });
      }
      return;
    }
    // An error drawn in the page's place takes off a refusal said before
    // anything was sent at a press of the form, while it is still the notice on
    // screen (unsentOnScreen), as a page drawn for another address does
    // (restoreDraft): it speaks of a form that has gone.
    if (draft && unsentOnScreen(draft.held)) {
      clearNotice();
    }
    leavePage();
    el.main.innerHTML = emptyStateHTML(message, board);
    announce(notFound ? "Not found" : "Could not load this page");
    return;
  } finally {
    finish();
  }

  // The Verify page has no posts to finish and no posting form to give a draft
  // back to, so once it is drawn two things run on it. A refusal said before
  // anything was sent at a press of the form the draft carries, made while the
  // page loaded with the old one still in sight, is taken off while it is still
  // the notice on screen (unsentOnScreen), as a page drawn for another address
  // takes it off (restoreDraft): it speaks of a form that has gone. Then the
  // jump to a fragment: the FAQ links to /verify#forget-browser. A render a
  // newer one overtook while it loaded drew nothing (renderVerify), and runs
  // neither, as on every other page (below): the notice is the newer render's
  // to leave or take off, and the jump would move the page the reader went to.
  // What runs on a pending code's box once it is drawn is held whole
  // (TestVerifyCodeBoxShowsWhileACodeIsPending), and the rest of render()
  // serves every page: a gate on a held badge there could take the box away
  // from a badge holder coming back to the page with every check passing.
  if (route.view === "verify") {
    if (!overtaken(route)) {
      const carried = carriedDraft(draft);
      if (carried && unsentOnScreen(carried.held)) {
        clearNotice();
      }
      focusHash();
    }
    return;
  }
  if (resolved !== null) {
    // The history entry is replaced rather than added: /post/N is an address,
    // not a page, and Back should not land on it only to bounce forward again.
    // Skipped if the reader went somewhere else while the number was looked up,
    // but not for a jump within the page meanwhile, the skip link or a quote
    // of a post that is not on it, say: that moves only the fragment, and the
    // thread is drawn at the post as if nothing had been pressed.
    if (!overtaken(route) && window.location.pathname + window.location.search === requested) {
      window.history.replaceState(null, "", resolved);
      await render({ draft });
    }
    return;
  }
  // A render overtaken while it loaded drew nothing, and leaves the screen to
  // the newer one. Otherwise the page is this render's now, with no image open
  // on it yet, and a thread for its controls to read only if it is a thread's,
  // which renderThread noted as it drew it.
  if (overtaken(route)) {
    return;
  }
  if (route.view !== "thread") {
    state.thread = null;
  }
  drawnFor = { path: renderedPath, route, thread: state.thread };
  expandedLink = null;
  enhancePosts(el.main);
  restoreDraft(carriedDraft(draft));
  focusHash();
}

// focusHash highlights the post referenced by the URL fragment, which is where
// quote links point. It runs on the Verify page too, a pending code's box
// included, so it is held whole (TestVerifyCodeBoxShowsWhileACodeIsPending).
function focusHash() {
  if (!window.location.hash) {
    return;
  }
  const target = document.getElementById(window.location.hash.slice(1));
  if (target) {
    target.scrollIntoView({ block: "center" });
    target.classList.add("flash");
    // A heading jumped to from a page's contents takes focus as well, so the
    // next Tab carries on from the question rather than from the top.
    if (target.hasAttribute("tabindex")) {
      target.focus({ preventScroll: true });
    }
  }
}

// navigate waits on render(), which can draw a pending code's box, so it ends
// there (TestVerifyCodeBoxShowsWhileACodeIsPending).
async function navigate(href, { replace = false } = {}) {
  const url = new URL(href, window.location.origin);
  if (replace) {
    window.history.replaceState(null, "", url);
  } else {
    window.history.pushState(null, "", url);
  }
  await render();
}

/* Drafts ------------------------------------------------------------------- */

// field looks a control up by name. Reaching through the form object directly
// (form.name) is ambiguous — HTMLFormElement has properties of its own by those
// names — so every lookup goes through the control collection.
function field(form, name) {
  const node = form.elements.namedItem(name);
  return node instanceof Element ? node : null;
}

// A re-render replaces the form, so anything half-written is carried across —
// "Update" on a busy thread must not cost you the reply you were typing.
// A form whose post is on its way (holdForm) is carried as itself: restoreDraft
// puts it, still held, in the new page, so the post on its way is what that
// page shows, and is read for its values only if its post has been answered by
// then (emptied after a success, released after a refusal). So is a form
// answered off the screen that has not been told yet what it missed
// (unsaidNotices), whatever begins the redraw, Update or Back say, so that the
// redraw tells it as one that carried it held from before the answer would. It
// is carried with browserForgets as it was, so that a forget the form misses
// while the redraw has it off the screen reaches it as it goes back. A draft is
// keyed by the page on screen, which the render that drew it noted (drawnFor),
// not by the newest render begun: one begun before the last has drawn, the
// second of a double click say, would carry this page's form, held or not, into
// the form of the page it goes to. A form read for its values is noted with
// them, with how often it had been pressed (presses), so that one pressed while
// the redraw loads is carried held after all (carriedDraft).
function captureDraft() {
  const key = routeKey(drawnFor.route || state.route);
  const form = el.main.querySelector("form[data-kind]");
  if (!form) {
    // A reply the thread closed on is carried as far as its text goes.
    const unsent = el.main.querySelector("textarea[data-unsent]");
    return unsent ? { key, open: true, body: unsent.value } : null;
  }
  if (heldForms.has(form) || unsaidNotices.has(form)) {
    return { key, held: form, forgets: browserForgets };
  }
  return { key, ...formDraft(form), from: form, pressed: presses.get(form) ?? 0 };
}

// carriedDraft is the draft a redraw carries, as of now. One read from a form
// for its values stands for that form only until the form is pressed: any press
// made while the redraw loads, its page still on screen, form and all, has the
// form itself carried held, whatever has become of the press since. A post
// pressed then is carried as one pressed just before the redraw began would
// have been (captureDraft), and so is a press refused then before anything is
// sent, an empty one say, for its refusal to be worded again for the form
// drawn, or taken off with the form (refuseUnsent), though the same press made
// just before the redraw began has the form read for its values. So a redraw
// with the form's routeKey, Update say, keeps the page on screen for any press
// made while it loads, with no skeleton, and keeps it, form and all, when the
// new page will not load (render), while a post link's lookup into its thread
// blanks the page all the same once it is slow enough to draw its skeleton; and
// the page drawn, by the redraw or the thread the lookup goes on to, puts the
// form back, or reads it as it is by then (restoreDraft), rather than the
// values read before the press, which the form a post went from would leave in
// the new form, to be posted twice.
function carriedDraft(draft) {
  if (!draft || !draft.from || (presses.get(draft.from) ?? 0) === draft.pressed) {
    return draft;
  }
  return { key: draft.key, held: draft.from, forgets: draft.forgets };
}

// formDraft reads what a posting form holds, noting browserForgets as it was,
// so that restoreDraft can tell a Name read before a forget. A ticked badge is
// read as the badge it was ticked for, by the number of the one its box offers
// (badgeOffered), and a form whose ticked badge went from under it
// (badgeWithdrawn) as ticked for the badge that went, with the key it was
// noted with.
function formDraft(form) {
  const image = field(form, "image");
  const details = form.closest("details");
  const value = (name) => {
    const node = field(form, name);
    return node ? node.value : "";
  };
  const sage = field(form, "sage");
  const badge = field(form, "badge");
  const spoiler = field(form, "spoiler");
  const ticked = badgeWithdrawn.get(form) || (badge && badge.checked ? { number: badgeOffered(form) } : null);
  return {
    forgets: browserForgets,
    open: details ? details.open : true,
    subject: value("subject"),
    name: value("name"),
    body: value("body"),
    sage: sage ? sage.checked : false,
    badge: ticked,
    file: image && image.files ? image.files[0] : null,
    spoiler: spoiler ? spoiler.checked : false,
  };
}

function restoreDraft(draft) {
  // A page drawn for another address says nothing of a press of the form
  // carried refused before anything was sent, and takes its refusal off while
  // it is still the notice on screen (unsentOnScreen): it speaks of that form,
  // which is not drawn again.
  if (!draft || draft.key !== routeKey(state.route)) {
    if (draft && unsentOnScreen(draft.held)) {
      clearNotice();
    }
    return;
  }
  const form = el.main.querySelector("form[data-kind]");
  // A form carried held whose post has been answered by now: on screen while
  // its page was drawn again, its notice said then, or off the screen, before
  // the redraw began or while it loaded, its notice said here (below). So is
  // one pressed while the redraw loaded and refused before anything was sent,
  // an empty press say, its notice said then and worded again here for the form
  // drawn while it is still the notice on screen (carriedDraft, below).
  let answeredForm = null;
  if (draft.held) {
    const held = draft.held;
    // The newest forget the form has had: the last before the redraw carried
    // it (captureDraft), or one since that reached it on screen, its page kept
    // there while the redraw loads say (forgottenAt).
    const forgets = Math.max(draft.forgets, forgottenAt.get(held) ?? 0);
    // Still held, its post still on its way: the form itself takes the place
    // of the one just drawn for the same thread, or the same board's new
    // thread, so the post is answered there.
    if (
      heldForms.has(held) &&
      form &&
      form.dataset.kind === held.dataset.kind &&
      form.dataset.thread === held.dataset.thread
    ) {
      // Its picker goes as the new page drew one: switched off with that
      // page's reason, the thread having reached its image limit since the
      // press say, or with the site's where it drew none, the site taking no
      // images; or live, the limit raised since a press on a full thread say.
      // Until the hold ends it shows what is being sent, so that waits until
      // then (holdForm).
      if (held.querySelector("[data-file-field]")) {
        redrawnPickers.set(held, pickerRefusal(form));
      }
      // A forget and a change of the badge reach the forms on screen
      // (forgetOnThisPage, followCredentialStorage), and this one may have
      // been off it, while a post link's lookup drew its skeleton say, so what
      // either would have done to it there is done now. A forget it has not
      // had (forgets) forgets it (forgetForm); one it had on screen is not
      // done again, so a Name typed since, which a post pressed since may be
      // going out under, stays. A box offering a badge other than the one the
      // page holds now (badgeOffered) is drawn again for that one, as another
      // tab's change draws it, noting a ticked box, but starts no puzzle, since
      // drawing a page never does (withdrawBadgeOption). That costs one number
      // on the draft and one per form forgotten, the badge being kept with the
      // form's box already. Keeping the forms off the screen where every forget
      // and change of badge could reach them would cost more: the list would
      // have to keep a form answered while away, too, until its page is drawn
      // (below).
      if (forgets !== browserForgets) {
        forgetForm(held);
      } else {
        withdrawBadgeOption(held, true);
      }
      form.replaceWith(held);
      const details = held.closest("details");
      if (details) details.open = true;
      return;
    }
    // Otherwise it is read for its values, and as of the newest forget it has
    // had: one answered while away missed any forget since, so its Name, the
    // one forgotten, is not restored, nor its badge (below), while a Name typed
    // after a forget it had on screen is. A ticked badge refused as expired or
    // revoked while it was away left a note (submit), which the refusal's
    // notice, said here, takes the place of (below).
    answeredForm = heldForms.has(held) ? null : held;
    draft = { key: draft.key, ...formDraft(held), forgets };
  }
  if (!form) {
    // A thread drawn locked or archived meanwhile, with no form, takes that
    // refusal off too.
    if (unsentOnScreen(answeredForm)) {
      clearNotice();
    }
    keepUnsentDraft(draft);
    return;
  }
  // A form answered while off the screen, while a post link's lookup into its
  // thread loaded say, was told nothing then. It is told now, once, what it
  // would have been told on screen (unsaidNotices), after its values go back
  // into its page (below). A thread drawn locked or archived meanwhile has no
  // form to tell, and its own note says why no post can go there
  // (keepUnsentDraft, above).
  const unsaid = answeredForm ? takeUnsaidNotice(answeredForm) : undefined;
  // An empty field is restored as empty: a Name the poster cleared stays
  // cleared, rather than the form's remembered name coming back. A Name read
  // before "Forget this browser" was pressed, here or in another tab, is not
  // restored at all: the forget emptied the box on screen, but the draft of a
  // redraw then on its way still held the forgotten name, tripcode secret
  // included. The new form keeps the Name it was drawn with.
  const forgotten = draft.forgets !== browserForgets;
  for (const name of ["subject", "name", "body"]) {
    const node = field(form, name);
    if (node && name in draft && !(name === "name" && forgotten)) {
      node.value = draft[name];
    }
  }
  const sage = field(form, "sage");
  if (sage) sage.checked = draft.sage;
  // A badge ticked in the draft is ticked again only if the new form offers the
  // same one. One the new form does not offer, gone from the browser meanwhile,
  // is noted as one taken out of the form would be, and so is one whose new
  // form offers another, verified in another tab or in this one meanwhile:
  // ticked here, that one would go out unasked for, and offered unticked, it
  // would let the next press post without a badge unsaid. A draft read before a
  // forget brings neither back (forgetOnThisPage). Nor is any noted on a form
  // whose notice, said below, refuses its badge: that notice is the form's
  // refusal for its badge, as on screen, where the refusal notes none, and as
  // on the page a failed redraw keeps, which drops the note (render). The draft
  // holds the badge refused as ticked, noted when it went (formDraft), unless
  // the reader has changed the box since, on the form while it was still in
  // sight, which takes the note off; a box ticked then for the badge the new
  // form offers is ticked again, and the notice of a replaced badge, worded
  // once the box is set, says so (badgeReplacedNotice).
  const badge = field(form, "badge");
  const ticked = forgotten ? null : draft.badge;
  const same = Boolean(ticked && badge && ticked.number === badgeOffered(form));
  if (badge) badge.checked = same;
  if (ticked && !same && !(unsaid && unsaid.badgeRefused)) {
    badgeWithdrawn.set(form, { number: ticked.number, key: ticked.key ?? (storageGet(POSTER_KEY_STORAGE_KEY) || "") });
  }
  // A form answered off the screen is told now, with its values back, what it
  // would have been told on screen (unsaidNotices), such as the refusal or, for
  // a post not sent, that nothing was posted, worded for the new form as it
  // stands: its text and the image going back into it (below), and for the
  // notice of a replaced badge, the badge held, the box as just set and the
  // poster key (badgeReplacedNotice). Each can have changed since the answer:
  // the text erased or the image taken out while the form was still in sight,
  // say, or the badge forgotten or another verified; and after Forget this
  // browser the form is told the server's words (submit). Its button gets the
  // focus as it would have (below). After a refusal of its image, which took
  // the image off, the new page's picker and counts, drawn from what the server
  // says by then, stand as drawn (blockRefusedPicker).
  //
  // A press refused before anything was sent, made on the form while it was
  // still in sight as the redraw loaded, an empty press or one the badge's note
  // stopped (refuseUnsent), has its refusal left up meanwhile, through the
  // render that draws the thread after a post link's lookup too (render), and
  // while it is still the notice on screen it is worded again for this form,
  // like any refusal still on screen (below): left as it is in words that come
  // out the same, and so not heard again, and taken off once its words no
  // longer hold, the reader having written something since an empty press say.
  // While it is up, its button gets the focus if the focus is lost, the focus
  // the press left in the old form having gone with it (below). A refusal the
  // reader has dismissed, or that another notice has taken the place of, is
  // said no more.
  const refused = answeredForm ? refusalNotices.get(answeredForm) : undefined;
  const text = Boolean(draft.body.trim());
  const words = unsaid ? unsaid.word(form, text, Boolean(draft.file)) : "";
  if (unsaid) {
    showNotice(words);
  }
  const said = noticeSaid();
  const unchanged = Boolean(refused && said) && said === (unsaid ? words : refused.told);

  // A form with no image field, on a site that takes no images, gets no file
  // back; a switched-off field refuses it in setFile and says why. A held form
  // answered while its page was drawn again, refused say, has its notice on
  // screen already, said then or above, and an image the new page takes none of
  // is taken off as release takes it off when the redraw comes first, the
  // notice saying so rather than giving way to the picker's reason: the
  // refusal's own notice, still on screen, is worded again with that reason
  // (below), and any other has it added (imageTakenOff). So whichever comes
  // first, the answer or the redraw, the notice ends the same. The one case
  // with nothing added is the notice of a refusal of the image itself said
  // above, the image limit reached or uploads switched off, which says already
  // that the image was taken off and why: an image attached since goes with
  // nothing said, the picker left as the new page drew it, as on the page a
  // failed redraw keeps (blockRefusedPicker), so the refusal is said once.
  const fileField = form.querySelector("[data-file-field]");
  const refusal = answeredForm && draft.file ? pickerRefusal(form) : "";
  if (refusal && unsaid && unsaid.imageRefused) {
    // Taken off: the new form is left without it.
  } else if (refusal && !unchanged) {
    showNotice(said ? imageTakenOff(said, refusal, { hasText: text, isThread: form.dataset.kind === "thread" }) : refusal);
  } else if (!refusal && fileField && draft.file && setFile(fileField, draft.file)) {
    updateFileField(fileField);
    // Only alongside the file it was ticked for.
    const spoiler = field(form, "spoiler");
    if (spoiler) spoiler.checked = draft.spoiler;
  }
  // A refusal's own notice still on screen, said while the redraw loaded or
  // above, speaks of this form now: it is kept with it and worded again for it
  // (rewordRefusal), as the form stands with its values back, the text put back
  // into it, its image or the picker's reason for taking it off, and for the
  // notice of a replaced badge the box as just set too (badgeReplacedNotice).
  // That notice is the form's one refusal for the badge, as on screen: a note
  // set on it above, for a box ticked since and taken out since with its badge
  // say, goes, the notice saying what pressing again does then. Words that come
  // out the same are left as they are, and not heard again.
  if (unchanged) {
    refusalNotices.set(form, { ...refused, off: refusal || refused.off, told: said, after: "" });
    rewordRefusal(form);
  }
  const details = form.closest("details");
  if (details && (draft.open || draft.body || draft.file)) {
    details.open = true;
  }
  // As a refusal on screen gives it (holdForm): on the button, without
  // scrolling, if the form had the focus at the press and it is lost now, on
  // the page itself or nowhere. A form refused on screen while the redraw
  // loaded had it given back then, on a button that has gone with the old page
  // since (refocusedForms), so the new form's button takes its place. So does a
  // press refused before anything was sent, while its refusal is the notice of
  // this form (above): the focus went where the press left it in the old form,
  // its text box, its badge box or, with neither, its button say (submit), and
  // went with it.
  const focused = unsaid
    ? unsaid.focused
    : refused && refused.unsent
      ? refusalNotices.has(form)
      : refocusedForms.has(answeredForm);
  if (focused && (!document.activeElement || document.activeElement === document.body)) {
    form.querySelector('button[type="submit"]')?.focus({ preventScroll: true });
  }
}

// pickerRefusal is why a posting form just drawn takes no image: the reason
// its picker was drawn switched off with, or the site's where it drew none,
// the site taking no images; "" for a picker drawn live.
function pickerRefusal(form) {
  const drawn = form.querySelector("[data-file-field]");
  if (!drawn) {
    return UPLOADS_OFF_REASON;
  }
  return drawn.classList.contains("blocked") ? drawn.querySelector(".file-blocked").textContent : "";
}

// keepUnsentDraft is for a draft whose thread was archived or locked under it:
// the re-render has no form to put it back in, and the draft is the only copy
// of what the poster wrote. Its text goes into a read-only box under the note
// that says why, to be copied somewhere it can still go.
function keepUnsentDraft(draft) {
  const note = el.main.querySelector("[data-replies-closed]");
  const body = draft.body || "";
  if (!note || (!body.trim() && !draft.file)) {
    return;
  }
  const box = document.createElement("div");
  box.className = "fields unsent-draft";
  if (body.trim()) {
    box.innerHTML = `
      <div class="field">
        <label class="label" for="f-unsent">Your reply was not sent. Copy it from here to keep it.</label>
        <textarea id="f-unsent" data-unsent readonly rows="6"></textarea>
      </div>`;
    box.querySelector("textarea").value = body;
  } else {
    box.innerHTML = `<p class="hint">Your reply's image was not sent.</p>`;
  }
  note.after(box);
}

/* Images ------------------------------------------------------------------- */

let expandedLink = null;

// toggleImage swaps a thumbnail for the full image and back. The full image is
// decoded before the swap so the picture never blinks through an empty frame,
// and the thumbnail's intrinsic size is restored on the way back so collapsing
// does not shift the page.
function toggleImage(link) {
  const img = link.querySelector("img");
  if (!img || link.classList.contains("loading")) {
    return;
  }
  if (link.classList.contains("expanded")) {
    collapseImage(link);
    return;
  }

  link.classList.add("loading");
  const full = new Image();
  full.decoding = "async";
  full.onload = () => {
    link.classList.remove("loading");
    img.removeAttribute("width");
    img.removeAttribute("height");
    img.src = link.dataset.full;
    img.classList.add("expanded");
    link.classList.add("expanded");
    link.setAttribute("aria-expanded", "true");
    link.title = "Collapse image";
    link.closest(".post-file")?.classList.add("expanded");
    expandedLink = link;
  };
  full.onerror = () => {
    link.classList.remove("loading");
    showNotice("Could not load the full image.");
  };
  full.src = link.dataset.full;
}

function collapseImage(link) {
  const img = link.querySelector("img");
  if (!img) {
    return;
  }
  const post = link.closest(".post");
  const before = post ? post.getBoundingClientRect().top : 0;

  img.src = link.dataset.thumb;
  img.classList.remove("expanded");
  if (img.dataset.tw) img.setAttribute("width", img.dataset.tw);
  if (img.dataset.th) img.setAttribute("height", img.dataset.th);
  link.classList.remove("expanded");
  link.setAttribute("aria-expanded", "false");
  link.title = "Expand image";
  link.closest(".post-file")?.classList.remove("expanded");
  if (expandedLink === link) {
    expandedLink = null;
  }

  // A tall image collapsing above the fold drags the page with it; put the post
  // back where the reader was looking.
  if (post && before < 0) {
    post.scrollIntoView({ block: "start" });
  }
}

/* Quote previews ----------------------------------------------------------- */

let quotePreview = null;

// showQuotePreview floats the quoted post next to the link, but only when that
// post is already on the page — a preview should never need a request.
function showQuotePreview(link) {
  hideQuotePreview();
  if (!link.hash) {
    return;
  }
  const source = document.getElementById(link.hash.slice(1));
  if (!source || !source.classList.contains("post") || !el.main.contains(source)) {
    return;
  }

  const clone = source.cloneNode(true);
  clone.removeAttribute("id");
  clone.classList.remove("flash", "reply");
  clone.querySelectorAll("[id]").forEach((node) => node.removeAttribute("id"));

  const box = document.createElement("div");
  box.className = "quote-preview";
  box.append(clone);
  enhancePosts(box);
  box.style.visibility = "hidden";
  document.body.append(box);

  const rect = link.getBoundingClientRect();
  const margin = 8;
  const left = Math.max(margin, Math.min(rect.left, window.innerWidth - box.offsetWidth - margin));
  let top = rect.bottom + 6;
  if (top + box.offsetHeight > window.innerHeight - margin) {
    top = Math.max(margin, rect.top - box.offsetHeight - 6);
  }
  box.style.left = `${Math.round(left)}px`;
  box.style.top = `${Math.round(top)}px`;
  box.style.visibility = "";
  quotePreview = box;
}

function hideQuotePreview() {
  if (quotePreview) {
    quotePreview.remove();
    quotePreview = null;
  }
}

/* Quoting ------------------------------------------------------------------ */

// A reply box switched off while its post is pending (holdForm) takes no
// quote, which would be left out of the post and wiped by the reset after it;
// the post number then goes to the post, as on a page with no reply form.
function quoteTarget() {
  return el.main.querySelector('form[data-kind="reply"] textarea[name="body"]:not(:disabled)');
}

function insertQuote(no) {
  const textarea = quoteTarget();
  if (!textarea) {
    return;
  }
  const details = textarea.closest("details");
  if (details) {
    details.open = true;
  }
  const prefix = textarea.value && !textarea.value.endsWith("\n") ? "\n" : "";
  textarea.value += `${prefix}>>${no}\n`;
  textarea.focus();
  textarea.selectionStart = textarea.selectionEnd = textarea.value.length;
}

/* Events ------------------------------------------------------------------- */

function isInternalLink(anchor) {
  return (
    anchor &&
    anchor.origin === window.location.origin &&
    !anchor.hasAttribute("download") &&
    anchor.target !== "_blank"
  );
}

// pressedTooSoon says whether a press is on one of the Verify page's own
// buttons, or on its attestation, within VERIFY_SETTLE_MS of a step being
// drawn, or moved by a refusal said in it: aimed at what was there before,
// and let go. A click let go on the attestation leaves the box as it was,
// unticked. The event's time is when the reader pressed, which is before the
// drawing, or the refusal, when the press was waiting behind it.
function pressedTooSoon(event) {
  const aimed = event.target.closest(VERIFY_MOVES) || event.target.closest(VERIFY_ATTESTATION);
  return !!aimed && event.timeStamp - verifyShown.at < VERIFY_SETTLE_MS;
}

// A press let go leaves the focus where the step just drawn put it. The
// button it landed on would take it otherwise, and on a wide screen that was
// Forget this browser, for the reader's next Enter to press.
document.addEventListener("mousedown", (event) => {
  if (pressedTooSoon(event)) event.preventDefault();
});

document.addEventListener("click", (event) => {
  if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey) {
    return;
  }

  if (event.target.closest('[data-action="dismiss-notice"]')) {
    event.preventDefault();
    clearNotice();
    return;
  }

  const clearFile = event.target.closest('[data-action="clear-file"]');
  if (clearFile) {
    event.preventDefault();
    clearFileField(clearFile.closest("[data-file-field]"));
    return;
  }

  const quoteButton = event.target.closest("[data-quote]");
  if (quoteButton && quoteTarget()) {
    event.preventDefault();
    insertQuote(Number(quoteButton.dataset.quote));
    return;
  }

  const spoiler = event.target.closest('[data-action="reveal-spoiler"]');
  if (spoiler) {
    event.preventDefault();
    hideQuotePreview();
    revealImageSpoiler(spoiler);
    return;
  }

  const expandable = event.target.closest("a.thumb-link.expandable");
  if (expandable) {
    event.preventDefault();
    hideQuotePreview();
    toggleImage(expandable);
    return;
  }

  if (event.target.closest('[data-action="refresh"]')) {
    event.preventDefault();
    render();
    return;
  }

  // Before any of the Verify page's own buttons is handled, a press on one
  // of them aimed at the step before is let go (VERIFY_SETTLE_MS).
  if (pressedTooSoon(event)) {
    event.preventDefault();
    return;
  }

  // Forgetting a badge is entirely local: the server was never told which
  // browser held it, so there is nothing there to tell. The browser's other
  // tabs see it go from storage and stop offering it (followCredentialStorage).
  // The address step that takes the badge panel's place opens by saying the
  // badge has gone (badgeLost), and that is brought into view clear of the
  // header and focused, with the address form under it. The redraw took the
  // button and the focus with it, and left a keyboard on the page itself, with
  // nothing to tell a screen reader that the badge was gone. Either forget
  // draws /verify again only while it is the page on screen, and not over a
  // step the reader has drawn since (renderVerify): pressed on a page the
  // reader has just left, it leaves the screen to the page they went to.
  if (event.target.closest('[data-action="forget-verification"]')) {
    event.preventDefault();
    forgetCredential();
    badgeLost = CREDENTIAL_FORGOTTEN;
    verifyStep = { step: "email", label: "" };
    renderVerify().then((drawn) => {
      const lost = drawn && document.getElementById("badge-lost");
      if (lost) {
        revealClearOfHeader(lost);
        lost.focus({ preventScroll: true });
      }
    });
    return;
  }

  // Forgetting the browser also moves the page to a new step
  // (forgetOnThisPage), so a badge still on its way from Verify is dropped when
  // it lands (submitVerification) rather than stored after the browser was
  // forgotten, and a code request still waiting on its puzzle or its turn is
  // not sent. The other tabs hear it too (forgetBrowser).
  if (event.target.closest('[data-action="forget-browser"]')) {
    event.preventDefault();
    clearNotice();
    forgetBrowser();
    renderVerify().then((drawn) => {
      if (drawn) confirmBrowserForgotten();
    });
    return;
  }

  // Keeping the badge gives up replacing it: the page goes back to the badge
  // held here, and a code already sent is left to expire unused. The badge is
  // read again first, since it can run out, or be forgotten in another tab,
  // while the page is on another step. Drawn from what was read before, a badge
  // that had expired came back as if it were live, "good until" a time gone by,
  // and went on being offered on posting forms for the server to refuse. A
  // badge that has gone leaves the address step, which opens by saying why
  // (badgeLost), and showHeldBadge shows and focuses that instead. Pressed on a
  // Verify page the reader has just left, still on screen while the next page
  // loads, it does nothing, as the forget buttons draw nothing there
  // (renderVerify): the page is the one they went to.
  if (event.target.closest('[data-action="verify-keep"]')) {
    event.preventDefault();
    if (!onVerifyPage()) return;
    clearNotice();
    state.verification.badge = readBadge();
    if (!state.verification.badge && !badgeLost) badgeLost = CREDENTIAL_FORGOTTEN;
    verifyStep = { step: "email", label: "" };
    drawVerifyStep();
    showHeldBadge();
    return;
  }

  // Verifying again from here means starting at the address field, with the
  // badge already held staying valid until the new one replaces it. Whatever
  // was said was about the step being left, so it goes with it: a refusal in
  // that step's form goes with the form, and the notice is put away. Pressed on
  // a Verify page the reader has just left, it does nothing, as Keep my badge
  // does: a Send code pressed on the step it drew would go from the page they
  // went to.
  const restart = event.target.closest('[data-action="verify-again"], [data-action="verify-restart"]');
  if (restart) {
    event.preventDefault();
    if (!onVerifyPage()) return;
    clearNotice();
    // replacing shows the address form over a badge that is still held and
    // still usable — it is retired by the new one, not by asking for one.
    verifyStep = { step: "email", label: "", replacing: true };
    drawVerifyStep();
    showVerifyField("f-email");
    return;
  }

  const anchor = event.target.closest("a");
  if (!isInternalLink(anchor)) {
    return;
  }
  const href = anchor.getAttribute("href");
  if (!href || href.startsWith("#")) {
    return;
  }
  hideQuotePreview();
  if (anchor.pathname === window.location.pathname && anchor.hash) {
    // Same page: let the browser jump to the anchor, then highlight it.
    setTimeout(focusHash, 0);
    return;
  }
  event.preventDefault();
  navigate(anchor.href);
});

// The address the view on screen was rendered for, less its fragment.
let renderedPath = null;

// What the page on screen was drawn for: its address, less the fragment, its
// route, and the thread its controls read. The render that draws a page notes
// them, and drawVerifyStep notes them for every Verify step it draws, whatever
// drew it: render, an answer, a button, or a redraw after a forget say. Nothing
// is noted for the skeleton, the age screen or the error of a page that would
// not load, none of which draws a form that reads it: drawnFor names the page
// drawn before them until another is drawn. A redraw that keeps the page for a
// post on its way, and then will not load, puts them back, the address in the
// address bar too (render).
let drawnFor = { path: null, route: null, thread: null };

// A same-page quote link or backlink only moves the fragment, and the browser
// reports that as a history change too. Re-rendering for it would fetch the
// thread again and rebuild it, hiding every spoiler the reader had revealed and
// collapsing any open image, to show what is already on screen; the jump and
// its highlight are all it needs. Back and Forward between such entries are the
// same.
window.addEventListener("popstate", () => {
  if (window.location.pathname + window.location.search === renderedPath) {
    focusHash();
    return;
  }
  render();
});

document.addEventListener("keydown", (event) => {
  if (event.key !== "Escape") {
    return;
  }
  hideQuotePreview();
  if (expandedLink) {
    collapseImage(expandedLink);
  }
});

el.main.addEventListener("change", (event) => {
  const field = event.target.closest("[data-file-field]");
  if (field && event.target.matches(".file-input")) {
    clearNotice();
    updateFileField(field);
    captchaImageAttached(event.target.form);
  }
  if (event.target.matches('input[type="checkbox"][name="badge"]')) {
    // Ticking or unticking the badge a form offers is the reader's choice,
    // made after the one a note on the form is about (badgeWithdrawn).
    badgeWithdrawn.delete(event.target.form);
    badgeToggled(event.target);
    captchaBadgeToggled(event.target);
  }
});

// The Name each form held when its badge box was ticked, to put back if the
// box is unticked before posting. It lives with the form, so a re-render
// forgets it and an unticked box then leaves the field as it is.
const nameBeforeBadge = new WeakMap();

// badgeToggled empties the Name field when the badge is ticked. The field
// starts with the name remembered from unbadged posts, and that name — a
// tripcode especially — on a badged post would show everyone which unbadged
// posts belong to the badge, which per-badge IDs exist to prevent. A name the
// poster types after ticking is theirs to choose.
function badgeToggled(box) {
  const form = box.form;
  const name = form && field(form, "name");
  if (!name) {
    return;
  }
  if (box.checked) {
    nameBeforeBadge.set(form, name.value);
    name.value = "";
  } else if (nameBeforeBadge.has(form)) {
    if (name.value === "") {
      name.value = nameBeforeBadge.get(form);
    }
    nameBeforeBadge.delete(form);
  }
}

if (HAS_HOVER) {
  el.main.addEventListener("mouseover", (event) => {
    const link = event.target.closest("a.quotelink:not(.dead)");
    if (link) {
      showQuotePreview(link);
    }
  });
  el.main.addEventListener("mouseout", (event) => {
    if (event.target.closest("a.quotelink")) {
      hideQuotePreview();
    }
  });
  window.addEventListener("scroll", hideQuotePreview, { passive: true });
}

/* Drag and drop ------------------------------------------------------------ */

// Dropping an image on the form attaches it. Dropping one anywhere else on the
// page is swallowed rather than letting the browser navigate away from a
// half-written post.
function isFileDrag(event) {
  return Array.from(event.dataTransfer ? event.dataTransfer.types : []).includes("Files");
}

function dropField(target) {
  const form = target instanceof Element ? target.closest(".post-form") : null;
  return form ? form.querySelector("[data-file-field]") : null;
}

document.addEventListener("dragover", (event) => {
  if (!isFileDrag(event)) {
    return;
  }
  event.preventDefault();
  const field = dropField(event.target);
  // A switched-off picker, blocked or held while its post is pending, is not a
  // drop target; the drop on a blocked one says why.
  if (field && !field.querySelector(".file-input").disabled) {
    field.classList.add("dragging");
  }
});

document.addEventListener("dragleave", (event) => {
  const field = dropField(event.target);
  if (field && !field.contains(event.relatedTarget)) {
    field.classList.remove("dragging");
  }
});

document.addEventListener("drop", (event) => {
  if (!isFileDrag(event)) {
    return;
  }
  event.preventDefault();
  const field = dropField(event.target);
  if (!field) {
    // A form on a site that takes no images has no field to drop into; the
    // file goes nowhere, and the reader is told why.
    const form = event.target instanceof Element ? event.target.closest(".post-form") : null;
    if (form && !state.site.uploads) {
      showNotice(UPLOADS_OFF_REASON);
    }
    return;
  }
  field.classList.remove("dragging");
  // A file the form took is shown as a picked one is, in place of whatever was
  // said before it. setFile takes the file and says whether it did, and says
  // why itself when it refuses one because the picker is blocked or the file is
  // not an accepted image; any other refusal, such as a picker switched off
  // only while its post is pending, no file, or a browser that cannot build a
  // DataTransfer or set input.files, says nothing, and what is on screen stays.
  // Only a file the form took starts the puzzle: a held form's own image is its
  // pending post's, which already has its answer or is waiting on the puzzle
  // being solved for it.
  if (setFile(field, event.dataTransfer.files[0])) {
    clearNotice();
    updateFileField(field);
    captchaImageAttached(field.closest("form"));
  }
});

/* Poster key --------------------------------------------------------------- */

// Held for the life of the page when storage refuses it, so posts made in one
// sitting still share an ID; it is only lost on reload.
let pagePosterKey = "";

// newPosterKey is 16 random bytes as unpadded base64url: 22 characters.
function newPosterKey() {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// posterKey returns this browser's key, making it on the first post rather
// than on the first visit: a reader who never posts never has one.
function posterKey() {
  try {
    const stored = localStorage.getItem(POSTER_KEY_STORAGE_KEY);
    if (stored && POSTER_KEY_PATTERN.test(stored)) {
      return stored;
    }
  } catch {
    // Storage is unavailable; the page's own key stands in below.
  }
  if (!pagePosterKey) {
    pagePosterKey = newPosterKey();
  }
  try {
    localStorage.setItem(POSTER_KEY_STORAGE_KEY, pagePosterKey);
    storedHere.set(POSTER_KEY_STORAGE_KEY, pagePosterKey);
  } catch {
    // Kept for this page only.
  }
  return pagePosterKey;
}

/* Captcha ----------------------------------------------------------------- */

// The captcha (build plan 2.6) is a puzzle the browser solves before a post,
// or a request for a verification code, goes out. educhan serves it itself:
// GET /api/captcha hands out a challenge the server signed, ALTCHA's worker,
// vendored under /lib/altcha/, finds the counter whose PBKDF2 key starts with
// the prefix the challenge gives, and the request carries the answer. Nothing
// goes to anyone but educhan, and nothing is asked of the reader.
//
// The puzzle is solved in the background, and starts only on something done in
// or to a form that needs it, never when a page is drawn: focus moving into a
// box to write in, however it gets there (a click or tap, Tab, or the page
// putting it there to quote a post, for "Verify another address" or "Use a
// different address", or after a press with nothing written); an image picked
// or dropped on it and attached (not one a re-render puts back); unticking the
// badge on a form whose tick stopped a puzzle, or that already has something
// written or attached, and the page taking a ticked badge out of a form on
// screen with something written or attached, the badge having gone from the
// browser or given way to another, in another tab or in this one, by anything
// but Forget this browser or /verify being drawn over the form, or been refused
// as expired or revoked (a form off the screen then, while a post link's lookup
// loads say, starts none, nor does the form a redraw draws in its place, noted
// or told of the refusal), and the press then refused once to say so, of a form
// with something written or attached; a refusal saying it spent the answer,
// while the form is on screen; and a press with no good answer held and none
// being solved. By the time a post is written its puzzle is usually solved. The
// answer is kept in memory only, never in storage or a draft, and is good for
// one request.
const CAPTCHA_WORKER_SRC = "/lib/altcha/pbkdf2.js";
// The server sets this header, to "spent", on every response to a request whose
// answer it has used up, a refusal included, but one: a 500 for a spend whose
// reply from the database was lost (captchaSpentHeader in captcha.go).
const CAPTCHA_SPENT_HEADER = "Educhan-Captcha";
// Up to this many copies of the worker share a puzzle, each trying every nth
// counter, for a browser that runs them side by side. How much the extra
// copies help differs by browser: Chromium runs every copy's Web Crypto on one
// thread, so there they take turns.
const CAPTCHA_MAX_WORKERS = 4;
// CAPTCHA_LIFETIME_MS is how long after its challenge was fetched an answer is
// used for. The server takes one for half an hour (captchaTTL in captcha.go);
// this leaves the slack for a slow clock and a slow request, and an answer
// older than this is replaced before a post goes out with it.
const CAPTCHA_LIFETIME_MS = 25 * 60 * 1000;
// A copy of the worker gives up after this long, so a puzzle that cannot be
// solved, which a working server never sends, stops using the processor. The
// hardest puzzle a server gives a reader posting at a person's pace asks at
// most 15,000 attempts (maxCaptchaWork and captchaMaxScale in captcha.go):
// at most a minute for a slow phone at 4 ms an attempt, half of this
// (TestClientAndServerAgreeOnTheCaptcha).
const CAPTCHA_TIMEOUT_MS = 2 * 60 * 1000;
// What a post or a code request says when the browser could not solve the
// puzzle: it has no Web Worker or Web Crypto, or the worker would not run.
const CAPTCHA_FAILED =
  "Your browser couldn't finish the anti-spam check, so nothing was sent. Try again, or try another browser.";
// What it says on a page that is not a secure context, plain HTTP to anywhere
// but this computer: no browser gives such a page the Web Crypto the worker
// needs, so trying again, or in another browser, would not help.
const CAPTCHA_INSECURE = "The anti-spam check needs a secure (https) connection to this site, so nothing was sent.";
// What it says when every copy of the worker gave up at CAPTCHA_TIMEOUT_MS:
// the server asks an address for harder puzzles while it asks for many, and
// eases off within a minute.
const CAPTCHA_TOO_SLOW = "The anti-spam check took too long, so nothing was sent. Wait a minute and try again.";
// What it says when the server could not make a challenge.
const CAPTCHA_UNAVAILABLE = "The anti-spam check couldn't start, so nothing was sent. Try again in a moment.";
// captchaBusy is what it says when the server hands out no more puzzles to
// this address for now (a 429 from /api/captcha, with how long to wait): too
// many have been asked for from it this minute, which a whole campus behind
// one address can do. It is not a post's or a code request's cooldown, so it
// is not worded as one.
function captchaBusy(err) {
  const seconds = err.payload && err.payload.retry_after;
  const wait = seconds ? `Wait ${plural(seconds, "second", "seconds")}` : "Wait a minute";
  return `Too many anti-spam checks have been started from your network, so nothing was sent. ${wait} and try again.`;
}

// The answer this page holds, until a request takes it: goodUntil is when it is
// replaced rather than sent, by this browser's clock. solving is the puzzle in
// progress, which a post waits for rather than starting another; stop stops
// it, and waiting counts the requests waiting for it.
const captcha = { answer: "", goodUntil: 0, solving: null, stop: null, waiting: 0 };

// captchaFormNeedsIt says whether a form's request will need an answer: a post
// without a badge, or a request for a verification code, while the server has
// the captcha on.
function captchaFormNeedsIt(form) {
  if (!state.site.captcha || !(form instanceof HTMLFormElement)) {
    return false;
  }
  if (form.dataset.verify) {
    return form.dataset.verify === "request";
  }
  const badge = field(form, "badge");
  return Boolean(form.dataset.kind) && !(badge && badge.checked);
}

// prepareCaptcha starts solving in the background, if nothing is solved or
// solving already. A failure here is not shown: the request that needs the
// answer tries again and says what went wrong.
function prepareCaptcha() {
  captchaAnswer().catch(() => {});
}

// captchaAnswer resolves to an answer to send: the one held, if it is still
// good, or a fresh one. Either stays held until a request takes it
// (takeCaptcha).
function captchaAnswer() {
  if (captcha.answer && Date.now() < captcha.goodUntil) {
    return Promise.resolve(captcha.answer);
  }
  if (!captcha.solving) {
    const stop = new AbortController();
    const solving = solveCaptcha(stop.signal).finally(() => {
      if (captcha.solving === solving) {
        captcha.solving = null;
        captcha.stop = null;
      }
    });
    captcha.solving = solving;
    captcha.stop = stop;
  }
  return captcha.solving;
}

// stopCaptcha stops the puzzle solving in the background, unless a request is
// waiting for it: a post that will carry the badge needs none. It says whether
// it stopped one.
function stopCaptcha() {
  if (captcha.stop && captcha.waiting === 0) {
    captcha.stop.abort();
    captcha.solving = null;
    captcha.stop = null;
    return true;
  }
  return false;
}

// captchaFormHasWriting says whether a posting form holds any of the post yet:
// a subject, a message or an image. The name is left out, because the form is
// drawn with the remembered one and the badge box empties and refills it.
function captchaFormHasWriting(form) {
  const written = (name) => {
    const box = field(form, name);
    return Boolean(box && box.value.trim());
  };
  const image = field(form, "image");
  return written("subject") || written("body") || Boolean(image && image.files && image.files.length);
}

// An image picked or dropped is some of the post, as writing in a box is: the
// puzzle starts for it in a form that needs it, once the image is attached (a
// file over the size limit is taken straight back off, and starts nothing).
// Not when a re-render puts a draft's image back (restoreDraft), which is
// drawing a page.
function captchaImageAttached(form) {
  const image = form instanceof HTMLFormElement ? field(form, "image") : null;
  if (captchaFormNeedsIt(form) && image && image.files && image.files.length) {
    prepareCaptcha();
  }
}

// The forms whose badge, when ticked, stopped a puzzle in progress.
const captchaStoppedByBadge = new WeakSet();

// Ticking the badge stops the puzzle, unless a request is waiting for it.
// Unticking it starts the puzzle again where the tick stopped one, or where the
// form already has something written or attached; on any other form it waits,
// as on a form whose badge was never ticked, for focus to move into a box to
// write in.
function captchaBadgeToggled(box) {
  const form = box.form;
  if (box.checked) {
    if (stopCaptcha() && form) {
      captchaStoppedByBadge.add(form);
    }
    return;
  }
  const stopped = Boolean(form) && captchaStoppedByBadge.delete(form);
  if (captchaFormNeedsIt(form) && (stopped || captchaFormHasWriting(form))) {
    prepareCaptcha();
  }
}

// takeCaptcha takes an answer captchaAnswer gave out of the page, for one
// request: no other request is handed it while that one is on its way, and a
// press elsewhere meanwhile solves a fresh puzzle. The server would take one
// answer from only one of two requests, and refuse the other as used. It says
// what it took, and until when it is good, or null when another request took
// it first.
function takeCaptcha(answer) {
  if (!answer || captcha.answer !== answer) {
    return null;
  }
  const taken = { answer, goodUntil: captcha.goodUntil };
  captcha.answer = "";
  captcha.goodUntil = 0;
  return taken;
}

// keepCaptcha puts back an answer a request took and did not spend, or that may
// have spent it without saying so: it was not sent, the server refused the
// request without using it, or what came back did not say, as when nothing came
// back at all or what came was the 500 for a spend whose reply from the
// database was lost. It goes back only while the page holds no other answer,
// and only while it is still good.
function keepCaptcha(taken) {
  if (taken && !captcha.answer && Date.now() < taken.goodUntil) {
    captcha.answer = taken.answer;
    captcha.goodUntil = taken.goodUntil;
  }
}

// solveCaptcha fetches a challenge and solves it, unless signal stops it
// first. The answer is ALTCHA's payload: the challenge as issued, and the
// solution, as base64 JSON. The time the worker took is left out; the server
// would not read it. A page that cannot run the worker, which needs Web Crypto
// and so a secure context, is told so before a challenge is fetched for
// nothing. A response that is not a challenge, a proxy's page say, is taken
// for a server that could not make one. The challenge is given up once
// VERIFY_TIMEOUT_MS has passed (verifyDeadline), as a Verify request is, so a
// connection gone silent fails the puzzle, as unreachable, rather than holding
// for ever the requests waiting on it.
async function solveCaptcha(signal) {
  if (!window.isSecureContext) {
    throw new Error(CAPTCHA_INSECURE);
  }
  if (typeof Worker !== "function" || !(window.crypto && crypto.subtle)) {
    throw new Error(CAPTCHA_FAILED);
  }
  let challenge;
  try {
    challenge = await api("/api/captcha", { signal: verifyDeadline(signal) });
  } catch (err) {
    if (err.status === 429) {
      throw new Error(captchaBusy(err));
    }
    throw err.status >= 500 ? new Error(CAPTCHA_UNAVAILABLE) : err;
  }
  if (
    !challenge ||
    !challenge.parameters ||
    typeof challenge.parameters !== "object" ||
    typeof challenge.signature !== "string"
  ) {
    throw new Error(CAPTCHA_UNAVAILABLE);
  }
  const fetched = Date.now();
  const solution = await runCaptchaWorkers(challenge, signal);
  captcha.answer = btoa(
    JSON.stringify({
      challenge: { parameters: challenge.parameters, signature: challenge.signature },
      solution: { counter: solution.counter, derivedKey: solution.derivedKey },
    }),
  );
  captcha.goodUntil = fetched + CAPTCHA_LIFETIME_MS;
  return captcha.answer;
}

// runCaptchaWorkers solves a challenge across copies of the worker, each
// counting from its own start in steps of their number, and stops them all at
// the first answer, or when signal says to. Exactly one counter answers a
// challenge, so whichever copy it falls to finds it, and the rest are stopped
// short.
function runCaptchaWorkers(challenge, signal) {
  const failed = () => new Error(CAPTCHA_FAILED);
  const count = Math.min(CAPTCHA_MAX_WORKERS, Math.max(1, navigator.hardwareConcurrency || 1));
  const workers = [];
  return new Promise((resolve, reject) => {
    let left = count;
    let timedOut = 0;
    let settled = false;
    const finish = (settle, value) => {
      if (settled) return;
      settled = true;
      for (const worker of workers) worker.terminate();
      settle(value);
    };
    if (signal.aborted) {
      reject(signal.reason);
      return;
    }
    signal.addEventListener("abort", () => finish(reject, signal.reason), { once: true });
    // A copy that gave up, threw, or never loaded is out; the puzzle fails
    // only when every copy is, and took too long when every copy ran out of
    // time.
    const giveUp = (outOfTime) => {
      left -= 1;
      if (outOfTime) timedOut += 1;
      if (left === 0) finish(reject, timedOut === count ? new Error(CAPTCHA_TOO_SLOW) : failed());
    };
    // Anything that throws while the copies are started, a worker the browser
    // will not make or a message it will not send, fails the puzzle and stops
    // every copy started so far.
    try {
      for (let i = 0; i < count; i++) {
        const worker = new Worker(CAPTCHA_WORKER_SRC);
        workers.push(worker);
        worker.onmessage = (event) => {
          const solution = event.data;
          if (solution && typeof solution.derivedKey === "string" && Number.isInteger(solution.counter)) {
            finish(resolve, solution);
          } else {
            // The worker answers null when it ran out of time, and an error
            // when it could not work at all.
            giveUp(solution === null);
          }
        };
        worker.onerror = (event) => {
          event.preventDefault();
          giveUp(false);
        };
        worker.postMessage({
          type: "work",
          challenge: { parameters: challenge.parameters },
          counterMode: "uint32",
          counterStart: i,
          counterStep: count,
          timeout: CAPTCHA_TIMEOUT_MS,
        });
      }
    } catch {
      finish(reject, failed());
    }
  });
}

// sendWithCaptcha sends a request that may need the captcha: needsIt() says
// whether it does, and send(answer) makes the request, with "" for no answer.
// The answer is taken out of the page for this request alone (takeCaptcha):
// another press while this request is on its way, from a form on another page
// say, solves a fresh puzzle rather than sending the same answer, which the
// server would let only one of the two use. It is put back (keepCaptcha, while
// it is good and only into an empty place) only when this request left it
// unspent, or may have, as when it brought no response, or the one refusal that
// cannot say (below).
// A refusal of the answer itself, for being missing, out of date, used or
// wrong, comes before anything is stored or sent, so the answer is dropped and
// the request sent once more with a fresh one, and the reader presses the
// button once. (A used answer was spent by an earlier request, whose cooldowns
// can refuse the resend; that refusal keeps the fresh answer, as below.) Any
// other refusal but that one says whether the server spent the answer before
// refusing, with the Educhan-Captcha: spent header. A refusal with the header
// drops the answer and, while onScreen() holds (below), starts the next puzzle
// at once, so it is ready, or nearly, when the reader presses again: an image
// whose pixels failed to decode once the answer was spent, say, or a cooldown
// that another request from this address started between flood control's check
// before the spend and its check after it, a 429 like the one for a cooldown
// found before the spend. A refusal without the header, such as one of what was
// written or for a cooldown found before the spend, leaves the answer held: it
// is put back for the next press, on keepCaptcha's terms, and that press goes
// straight out. With no answer from the server at all the answer is put back
// too; if it had been spent, the first request whose captcha check on the
// server can look it up is refused as "used" and solves a fresh one here, while
// one refused before that check, a reply to a thread locked meanwhile or an
// image whose header does not read say, or whose lookup fails, a 500 with no
// header, puts it back again, and one checked after the server's secret was
// rotated is refused as invalid instead, and solves a fresh one too. So it is
// after the one refusal that cannot say: a 500 for a spend the database applied
// but whose reply was lost, which carries no header though the answer is spent.
// solving(true) and solving(false) bracket the time spent on the puzzle.
// onScreen() says whether the form the request came from is still on screen
// (formOnScreen), and the browser not forgotten since the press. A request
// whose form has gone, because the reader went to another page, or that Forget
// this browser cancelled meanwhile, is not sent, the first time or once more
// after a refused answer. That is asked once it has waited for the puzzle,
// solved since the press, its answer then put back for the next press, on
// keepCaptcha's terms, and again when a refusal of the answer arrives, perhaps
// after a forget heard while the first send was on its way. Either way
// sendWithCaptcha throws the error notSent makes, for a caller that says
// nothing on a page the reader went to and, on a form still on screen, which
// only the forget stops, says why. A post's form keeps how to word what it
// would have been told, for a redraw that carries it held to word and say
// (unsaidNotices). Neither the second send nor the next puzzle is started for
// one. A request with an answer held already, or needing none, goes with the
// press, made on a form on screen: one pressed while a render for another page
// loads, its page still drawn, form and all, goes as it would have just before
// that render began, its answer judged by where the reader is when it comes
// (onScreen).
// Pressing Update while a post waits does not take its form away: the redrawn
// page shows the same form, still held, as does another page of the same
// board's index, drawn with the same new-thread form, and a redraw that will
// not load leaves the page on screen, form and all.
async function sendWithCaptcha(needsIt, send, solving, onScreen) {
  for (let retried = false; ; retried = true) {
    let taken = null;
    let waited = false;
    if (needsIt()) {
      solving(true);
      captcha.waiting += 1;
      try {
        // Taken as soon as it is handed over, in the same step. An answer
        // another request waiting on the same puzzle took first is not shared:
        // this one solves another. One held already is handed over at once;
        // anything else is the puzzle, waited for.
        do {
          const ready = captchaAnswer();
          waited = waited || ready === captcha.solving;
          taken = takeCaptcha(await ready);
        } while (!taken);
      } catch (err) {
        // The server has switched the captcha off since this page learned it
        // was on, so the request goes without an answer.
        if (err.status !== 404) {
          throw err;
        }
        state.site.captcha = false;
      } finally {
        captcha.waiting -= 1;
        solving(false);
      }
    }
    const answer = taken ? taken.answer : "";
    if (waited && !onScreen()) {
      keepCaptcha(taken);
      throw notSent();
    }
    try {
      return await send(answer);
    } catch (err) {
      const reason = err.payload && err.payload.captcha;
      if (!reason) {
        if (!(err.headers && err.headers.get(CAPTCHA_SPENT_HEADER) === "spent")) {
          keepCaptcha(taken);
        } else if (answer && needsIt() && onScreen()) {
          prepareCaptcha();
        }
        throw err;
      }
      // The page was drawn without knowing the captcha is on: /api/site did
      // not answer, or the server has switched it on since.
      if (reason === "required") {
        state.site.captcha = true;
      }
      if (retried || !needsIt()) {
        throw err;
      }
      if (!onScreen()) {
        throw notSent();
      }
    }
  }
}

// notSent is the error sendWithCaptcha throws for a request it did not send,
// marked so that the caller can tell it from a refusal: its message is for
// nobody.
function notSent() {
  const err = new Error("not sent");
  err.notSent = true;
  return err;
}

// formOnScreen says whether a form is still the one the reader is looking at:
// in the document, on the page that drew it. Going to another page changes
// state.route at once, while the old form stays in the document until the new
// page is drawn over it. A form's route is the page it is on at the press, as
// what drew that page noted it (drawnFor), the render or, for /verify,
// drawVerifyStep, never a render begun since that has not drawn yet: pressed
// just after a link, a post is answered as off the screen while state.route has
// another routeKey, and on the form again once a redraw of its own page, a post
// link's lookup into its thread say, carries it, and a code request waiting on
// its puzzle as off the screen (below). A redraw of the same page, Update say,
// or of another page of the same board's index (the same routeKey), keeps a
// posting form whose post is on its way in the document, one pressed while the
// redraw loads included (carriedDraft): it puts the form in place of the one
// the new page draws for the same thread or board (restoreDraft) or, when the
// new page will not load, leaves the page on screen, form and all (render).
// Either way the post goes ahead and is answered on that form. Only a new page
// that draws no such form, the thread locked or archived meanwhile, takes it
// off the screen. No redraw carries a /verify form, so one is on screen only
// until the next render, of /verify too, one begun just before its press
// included, or the next step drawn: a code request whose form has gone by the
// time its puzzle is solved is not sent (submitVerification). One sent already
// is answered on the step it was sent from, whichever drawing of it is on
// screen.
function formOnScreen(form, route) {
  return form.isConnected && (form.dataset.kind ? routeKey(state.route) === routeKey(route) : state.route === route);
}

// The forms holdForm holds, each with the controls it switched off, for release
// to switch back on: a badge box drawn into the form during the hold among them
// (drawBadgeOption).
const heldForms = new WeakMap();

// How many times each posting form has been pressed (submit), for a redraw to
// tell whether the form it read its draft from has been pressed since
// (carriedDraft).
const presses = new WeakMap();

// The posting forms whose last press was refused on screen with its button
// given the focus back (holdForm), for a redraw loading meanwhile that puts
// such a form's values into its new page, the old form, button and all, gone as
// the page is drawn again, to give the new form's button the focus as well
// (restoreDraft). Every press takes its form out first (submit).
const refocusedForms = new WeakSet();

// How the last redraw of a held form's page drew the picker there, by form:
// the reason it was drawn switched off with, or "" for one drawn live, or the
// site's reason where it drew none, on a site that takes no images
// (restoreDraft). The held picker shows the image of the post on its way until
// the hold ends, and goes as the redraw drew it then (holdForm).
const redrawnPickers = new WeakMap();

// How each posting form's last refusal is worded, by form, for it to be worded
// again while it is still the notice on screen (rewordRefusal) and by a redraw
// that draws the form again (restoreDraft): notice, a function of why its image
// was taken off ("" for not at all), of the form it is said to and of whether
// that form holds text and an image; replaced, whether it is the notice of a
// replaced badge as things stand when it is worded; off, why its image was
// taken off, by release or by a redraw since; told, the words on screen, ""
// while a form answered off the screen has yet to be told (unsaidNotices);
// after, what a failed redraw that kept the page added after them (render); and
// unsent, set for a refusal said before anything was sent, an empty press or
// the badge's note (refuseUnsent), which a redraw that carries the form, the
// press made on it while it was still in sight, leaves up while it loads,
// through the render that draws the thread after a post link's lookup too
// (render), and words again for the form it draws, or takes off when it does
// not draw that form again (unsentOnScreen). A refusal answered while a redraw
// of the page loads, which then draws the picker switched off, is worded again
// with the picker's reason for the form that redraw draws and the text put back
// into it, so it reads as it would had the redraw come first, and the notice of
// a replaced badge, its one refusal for the badge, then takes off a note that
// form was given (badgeWithdrawn). The words told say whether its notice is
// still the one on screen, however the page has changed since, a badge
// forgotten or verified meanwhile say, which the wording reads as it stands
// then (badgeReplacedNotice): a notice the reader has dismissed, or that
// another has taken the place of, is not worded again. The two refusals of the
// image itself take the image off themselves, and keep nothing here, and nor
// does a post the forget stopped (FORGOTTEN_BEFORE_SENDING): none of them
// speaks of a badge held, its box or the poster key, and what the last says of
// the forget that stopped it stays so, so no change of badge and no forget
// since makes what they say of the post untrue. A change of badge that takes
// away the badge of a ticked box notes the form all the same, whatever notice
// is up, so its next press with something to post is refused once by the note,
// which says why (badgeWithdrawn), unless the notice on screen is worded again
// as that of a replaced badge, which takes the note off (rewordRefusal); a
// forget takes the box out and leaves the form with no note (forgetForm). Every
// press of the form takes its entry out (submit), so a refusal is worded again
// only for the press it answers.
const refusalNotices = new WeakMap();

// What each posting form would have been told when its post was answered, or
// dropped unsent, had it been on screen then, by form: how to word the notice
// the submit handler would have shown, as a function of the form it is said to
// and of whether that form holds text and an image (word), whether the form had
// the focus at the press, whether the notice is the refusal of its badge, and
// whether it is a refusal of the image itself, for the thread's image limit or
// not. A form a redraw has taken off the screen, while a post link's lookup
// loads say, is told nothing then. A redraw that carries it held, begun before
// the answer or after it, Update or Back say (captureDraft), tells it then,
// once, if it puts its values back into its own page (restoreDraft) or keeps
// that page, form and all, when it will not load (render), switching its picker
// off there after a refusal of the image (blockRefusedPicker). It is worded as
// it is told, for the form drawn or kept and as things stand then: its text and
// its image, which the reader can still change while the form is in sight, as a
// post link's lookup loads say, and for the notice of a replaced badge, the
// badge held, the form's box and the poster key (badgeReplacedNotice), which a
// forget of the badge or another badge verified can change, the notice then
// naming no badge or the one held then, and a badge stored where none was held
// at the answer has the form told that notice rather than the server's words,
// while after Forget this browser the form is told the server's words (submit).
// Every press of the form takes out what is left unsaid, one refused before it
// is sent included, acting first on a refusal of the image unless an image has
// been attached since (submit), so a form is told only of the press it is
// answering, and a form that does not come back, to a page drawn for another
// address say, takes its entry with it.
const unsaidNotices = new WeakMap();

// takeUnsaidNotice takes out what a form answered off the screen was not told
// (unsaidNotices), for the redraw that tells it, so it is told once.
function takeUnsaidNotice(form) {
  const unsaid = unsaidNotices.get(form);
  unsaidNotices.delete(form);
  return unsaid;
}

// formHolds is whether a posting form holds text and an image now, for a notice
// worded as it is said (refusalNotices).
function formHolds(form) {
  const body = field(form, "body");
  const image = field(form, "image");
  return { text: Boolean(body && body.value.trim()), image: Boolean(image && image.files.length) };
}

// rewordRefusal words again the refusal a posting form was told, while it is
// still the notice on screen (refusalNotices), when what it speaks of can have
// changed under it before the next press: the form's badge box drawn again for
// another badge or none (withdrawBadgeOption), on a change of badge in another
// tab say (followCredentialStorage); the browser forgotten, in another tab say
// (forgetForm); or the form drawn again by a redraw that puts its values back
// (restoreDraft). It is worded by the rule it was first worded by, for the
// badge held, the box on that form, the poster key and the text and image the
// form holds then, as a form answered off the screen is when a redraw tells it
// (unsaidNotices): the notice of a replaced badge names the badge held then, or
// none, and after Forget this browser the form is told the server's words
// (submit), so the screen names no badge the browser held. The notice of a
// replaced badge is the form's one refusal for the badge, so a note left on the
// form since goes (badgeWithdrawn). A refusal said before anything was sent
// whose words no longer hold, the badge's note after a forget say, is taken
// off, with what was added after it left on screen. Only the notice's words
// change: the focus stays where it is, on its dismiss button too, and the page
// is not scrolled. The notice is an alert, so a screen reader hears the new
// words, as it hears the notice a failed redraw adds to (render); words that
// come out the same are left as they are, and not heard again. The reader's own
// changes to the form, a tick of its box or its text erased say, word nothing
// again: the reader makes them with the form in sight, and the notice would be
// heard again at each; the next wording reads them.
function rewordRefusal(form) {
  const refused = refusalNotices.get(form);
  const said = noticeSaid();
  if (!refused || !refused.told || said !== refused.told) {
    return;
  }
  const { text, image } = formHolds(form);
  const words = refused.notice(refused.off, form, text, image);
  if (refused.replaced()) {
    badgeWithdrawn.delete(form);
  }
  const told = [words, refused.after].filter(Boolean).join(" ");
  if (words) {
    refused.told = told;
  } else {
    refusalNotices.delete(form);
  }
  if (!told) {
    clearNotice();
  } else if (told !== said) {
    el.notice.querySelector(".notice-text").textContent = told;
  }
}

// unsentOnScreen is the refusal a posting form was told at a press refused
// before anything was sent (refuseUnsent), while it is still the notice on
// screen in the words it was told, and undefined otherwise: one the reader has
// dismissed, or that another notice has taken the place of, is not.
function unsentOnScreen(form) {
  const refused = form ? refusalNotices.get(form) : undefined;
  return refused && refused.unsent && refused.told && refused.told === noticeSaid() ? refused : undefined;
}

// holdForm switches off a form's controls while its request is pending, from
// the press until the answer, with whatever the reader wrote left where it is.
// What is sent was read from the form at the press, so a change made in the
// meantime would be left out of it, and wiped by the reset after a success. It
// returns release, which switches back on the controls it switched off, a badge
// box drawn into the form meanwhile among them (drawBadgeOption), and no
// others: a picker switched off for its own reason stays off. When the hold
// ends, success or refusal, release leaves the picker as a redraw of the page
// meanwhile drew it (redrawnPickers): switched off with that drawing's reason,
// or on, or off with the site's reason where the redraw drew none. A picker it
// switches off loses the image it held, and release then returns the reason,
// for the notice to say why the image went (imageTakenOff), and "" otherwise. A
// refusal of the image itself, the image limit reached or uploads switched off,
// then switches the picker off (blockRefusedPicker): the submit handler does
// that for a form on screen, and for one answered off the screen the page a
// failed redraw keeps for it does (render), or the form's next press if that
// comes first, unless an image has been attached since, which is left for the
// server to answer (submit), while a page drawn afresh draws its picker as the
// server says by then. With refocus, a form that had the focus at the press,
// which disabling the focused control took away, gets it back on its button,
// ready to press again (put back in a box to write in, it would start another
// puzzle), and, for a posting form, the only kind noted for it
// (refocusedForms), so does the button of the form a redraw loading meanwhile
// then draws in its place (restoreDraft). That is only while the focus is still
// lost, on the page itself or nowhere: a reader who moved it during the wait,
// to read a post say, keeps it where they put it. And it does not scroll the
// page: a reader who scrolled away during the wait stays where they are. The
// notice saying what happened is at the top of the page, in view only of a
// reader who is there. Until release, the form is in heldForms, and a redraw of
// its page carries it across as it is (captureDraft, restoreDraft).
function holdForm(form) {
  const controls = Array.from(form.elements).filter((control) => !control.disabled);
  const focused = form.contains(document.activeElement);
  for (const control of controls) {
    control.disabled = true;
  }
  heldForms.set(form, controls);
  return (refocus) => {
    heldForms.delete(form);
    for (const control of controls) {
      control.disabled = false;
    }
    let imageOff = "";
    if (redrawnPickers.has(form)) {
      const reason = redrawnPickers.get(form);
      const picker = form.querySelector("[data-file-field]");
      redrawnPickers.delete(form);
      if (reason) {
        if (picker.querySelector(".file-input").files.length > 0) {
          imageOff = reason;
        }
        blockFileField(picker, reason);
      } else {
        unblockFileField(picker);
      }
    }
    const lost = !document.activeElement || document.activeElement === document.body;
    if (refocus && focused && lost) {
      form.querySelector('button[type="submit"]')?.focus({ preventScroll: true });
      if (form.dataset.kind) {
        refocusedForms.add(form);
      }
    }
    return imageOff;
  };
}

// The puzzle starts when focus moves into a box to write in, in a form that
// needs it, however it gets there: a click or tap, Tab, or the page putting it
// there (quoting a post, "Verify another address" and "Use a different
// address" on /verify, and a press with nothing written). Not on the badge box,
// and not in a form whose badge is ticked (see captchaBadgeToggled for a reader
// who ticks it later).
el.main.addEventListener("focusin", (event) => {
  const box = event.target;
  if (box.matches("textarea, input[type=text], input[type=email]") && captchaFormNeedsIt(box.form)) {
    prepareCaptcha();
  }
});

/* Submitting --------------------------------------------------------------- */

// refuseUnsent says why a press of a posting form is refused before anything is
// sent, an empty press or one the badge's note stops (submit), and keeps with
// the form how to word it (refusalNotices): words, a function of the form it is
// said to and of whether that form holds text and an image, which gives "" once
// what it says no longer holds. A redraw loading meanwhile, the press made on
// the form while it was still in sight, during a post link's lookup into its
// thread, Update, another page of the board's index or Back say, carries the
// form for that press (carriedDraft) and leaves the refusal up while it loads,
// the render that draws the thread after a post link's lookup leaving it up too
// (render). If the page it draws is the form's own, the refusal, while it is
// still the notice on screen, is worded again for the form drawn and kept with
// it, that form's button given the focus if the focus is lost, the focus the
// press gave having gone with the old form (restoreDraft), so the press is
// answered on its form as a post the server answered is; if the page is kept,
// its redraw failing to load, the refusal stays with the error after it
// (render). A redraw that draws another page, a thread with no form, the age
// screen or an error in the page's place takes the refusal off and says nothing
// of it, the form it speaks of being gone (unsentOnScreen). Until the next
// press, the refusal is worded again in place while it is the notice on screen
// (rewordRefusal).
function refuseUnsent(form, words, text, image) {
  const notice = (off, on, hasText, hasImage) => {
    const said = words(on, hasText, hasImage);
    return said ? imageTakenOff(said, off, { hasText, isThread: on.dataset.kind === "thread" }) : "";
  };
  const told = notice("", form, text, image);
  refusalNotices.set(form, { notice, replaced: () => false, off: "", told, after: "", unsent: true });
  showNotice(told);
}

// emptyPostNotice is what a press with nothing to post is told. An image is
// offered as the alternative only where the form can take one.
function emptyPostNotice(form) {
  const image = field(form, "image");
  return image && !image.disabled ? "Write something or attach an image." : "Write something to post.";
}

// badgeNoteWords is how to word the badge's note for a press it refuses
// (badgeWithdrawnNotice), for the form it is said to as that form stands: the
// badge held, the box drawn for it and the poster key, which another tab's
// verification or forget can change while the note is on screen, or a redraw
// that draws the form again. Its words no longer hold, and are "", once the
// browser has been forgotten since the press, which takes out the badge box and
// the key the note speaks of and says nothing at the next press (forgetForm),
// once the form's box is ticked, which posts with that badge, or once the form
// has a note of its own again, for a box ticked since and drawn again for
// another badge, which refuses the next press itself.
function badgeNoteWords(withdrawn) {
  const forgets = browserForgets;
  return (on) => {
    const box = field(on, "badge");
    if (forgets !== browserForgets || (box && box.checked) || badgeWithdrawn.has(on)) {
      return "";
    }
    return badgeWithdrawnNotice(on, withdrawn);
  };
}

el.main.addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = event.target;
  // A Verify form's answer can draw a pending code's box, so nothing runs
  // here once submitVerification is done with it
  // (TestVerifyCodeBoxShowsWhileACodeIsPending).
  if (form.dataset.verify) {
    await submitVerification(form);
    return;
  }
  // What an earlier press answered off the screen left unsaid goes at every
  // press, one refused here before anything is sent included, an empty one or
  // one the badge's note stops say: from here the form is told only of this
  // press, whatever it is told. A refusal of the image itself is acted on
  // first, as on screen it was at once and as the page a failed redraw keeps
  // would have acted on it: the picker is switched off and the thread's counts
  // put in step (blockRefusedPicker), so this press is judged as it would have
  // been there. An image attached since is left for the server to answer
  // instead, rather than taken off unsaid.
  const owed = takeUnsaidNotice(form);
  const image = field(form, "image");
  if (owed && owed.imageRefused && !(image && image.files.length)) {
    blockRefusedPicker(form, owed.limitReached);
  }
  // Every press is counted, one refused below before anything is sent included,
  // so that a redraw loading meanwhile, whose draft was read from this form
  // before it, carries the form itself, held, as it carries a post pressed
  // before it began (carriedDraft), and the focus an earlier refusal gave back
  // is no longer this press's to give again (refocusedForms), nor is the
  // refusal the form was told this press's to word again (refusalNotices).
  presses.set(form, (presses.get(form) ?? 0) + 1);
  refocusedForms.delete(form);
  refusalNotices.delete(form);
  const button = form.querySelector('button[type="submit"]');
  const data = new FormData(form);
  const name = String(data.get("name") || "");
  const body = String(data.get("body") || "");
  const file = image && !image.disabled && image.files[0];

  // Nothing to post. The refusal is worded again for the form a redraw loading
  // meanwhile draws while it is still the notice on screen, and so taken off if
  // the reader has written something since (refuseUnsent); attaching an image
  // takes it off at once, as it takes off any notice.
  if (!body.trim() && !file) {
    refuseUnsent(form, (on, text, image) => (text || image ? "" : emptyPostNotice(on)), false, false);
    (field(form, "body") || form).focus();
    return;
  }

  // A box ticked for a badge the page no longer holds is a badge taken out
  // from under the form, now: the credential goes only as the badge the box
  // named (badgeOffered), so one stored since, verified in this tab or
  // another, never goes out under a box that named another, and none goes once
  // the page holds none. The box is drawn again, unticked, for the badge held
  // now, or taken out (withdrawBadgeOption).
  const badgeBox = field(form, "badge");
  const offered = state.verification.badge;
  if (badgeBox && badgeBox.checked && !(offered && badgeOffered(form) === offered.number)) {
    withdrawBadgeOption(form, false);
  }

  // A form whose ticked badge was taken out from under it says so before its
  // post goes without one, once, worded again for the form a redraw loading
  // meanwhile draws (refuseUnsent); the press after it posts (badgeWithdrawn).
  // The box drawn in its place, if the page holds a badge now, takes the focus,
  // for the reader to tick it there. That post needs the puzzle the badge
  // would have skipped, which starts now if it has not: the page taking the
  // badge out starts it only for a form on screen as it goes
  // (withdrawBadgeOption), and a redraw that carries the note (restoreDraft),
  // or /verify drawn over the form (loadVerification), starts none, since
  // drawing a page never does.
  if (badgeWithdrawn.has(form)) {
    const withdrawn = badgeWithdrawn.get(form);
    badgeWithdrawn.delete(form);
    refuseUnsent(form, badgeNoteWords(withdrawn), Boolean(body.trim()), Boolean(file));
    const box = field(form, "badge");
    if (box) box.focus();
    if (captchaFormNeedsIt(form) && captchaFormHasWriting(form)) {
      prepareCaptcha();
    }
    return;
  }

  // The one request in the application that carries the credential. It goes
  // out only because this box is ticked, and only as the badge it names
  // (above), so an anonymous post is anonymous at the request layer too, not
  // merely in what it displays.
  const wantsBadge = data.get("badge") === "on";
  const held = wantsBadge ? state.verification.badge : null;
  // A badged post carries the credential and not the poster key. The server
  // derives a badged post's ID from the credential, so the key would do
  // nothing there, and sending the two together would put this browser's key
  // beside its credential in one request — the only place they could meet.
  const headers = held ? { [CREDENTIAL_HEADER]: held.token } : { [POSTER_KEY_HEADER]: posterKey() };

  // The page the form is on, which the render that drew it noted (drawnFor),
  // never one begun since that has not drawn yet, after a link pressed just
  // before say: its board is where the post goes, and it is the page the post
  // is answered on (formOnScreen).
  const route = drawnFor.route;
  const isThread = form.dataset.kind === "thread";
  const payload = { name, body, badge: wantsBadge };
  let path;
  if (isThread) {
    payload.subject = String(data.get("subject") || "");
    path = `/api/boards/${encodeURIComponent(route.slug)}/threads`;
  } else {
    payload.sage = data.get("sage") === "on";
    path = `/api/boards/${encodeURIComponent(route.slug)}/threads/${form.dataset.thread}/posts`;
  }

  const label = button.textContent;
  const sending = file ? "Uploading…" : "Posting…";
  const onScreen = () => formOnScreen(form, route);
  // "Forget this browser", pressed in another tab while the post waits on its
  // puzzle, stops it as it stops a code request: sent, it would go under the
  // Name and the poster key the forget took away, while its form shows
  // neither. A post that went before the forget is answered on its form, and
  // keeps nothing (below); one refused for its answer is not sent again.
  const forgets = browserForgets;
  const forgotten = () => forgets !== browserForgets;
  // Read above, and held from here until the post has gone or been refused, so
  // what the form shows meanwhile is what is sent, but for what a forget or a
  // change of badge does to it meanwhile, such as the Name emptied (forgetForm)
  // or the badge box taken out, or drawn for another badge, switched off and
  // unticked (drawBadgeOption), while the post, if it goes, carries what was
  // read at the press. Whether the form has the focus at the press is noted for
  // a refusal answered off the screen (unsaidNotices); holdForm notes it for
  // one on screen.
  const focused = form.contains(document.activeElement);
  const release = holdForm(form);
  button.textContent = sending;
  clearNotice();
  try {
    // A badged post skips the captcha; any other carries an answer when the
    // server asks for one. A post with an image goes as multipart, with the
    // answer as a field beside it; text-only posts stay on JSON.
    const created = await sendWithCaptcha(
      () => state.site.captcha && !held,
      (answer) => {
        // The second send, after a refused answer, carries the fresh one or,
        // when the server has switched the captcha off, none.
        data.delete("captcha");
        delete payload.captcha;
        if (answer && file) {
          data.set("captcha", answer);
        } else if (answer) {
          payload.captcha = answer;
        }
        return api(path, { method: "POST", body: file ? data : JSON.stringify(payload), headers });
      },
      (solving) => {
        button.textContent = solving ? "Checking…" : sending;
      },
      () => onScreen() && !forgotten(),
    );
    // The remembered name is the one for unbadged posts; a badged post's name
    // is never offered on the next form, where the badge starts unticked. A
    // post that went before "Forget this browser" and was answered after it
    // leaves the name forgotten, in storage and in the form below.
    if (!held && forgets === browserForgets) {
      storageSet(NAME_STORAGE_KEY, name);
      storedHere.set(NAME_STORAGE_KEY, name);
    }
    // Cleared, wherever the form is, so the draft carried across a re-render
    // is the empty form rather than the post that just went through: the
    // navigation below, or a redraw of the page that carried the form while
    // it was held (captureDraft). reset() puts the Name back to what the form
    // was drawn with, so it is set again: to the name just used — empty, if
    // the poster cleared it — or, after a badged post or a forget, to the
    // remembered one.
    form.reset();
    // A badge taken out while the post was on its way went with the post.
    badgeWithdrawn.delete(form);
    const nameField = field(form, "name");
    if (nameField) {
      nameField.value = held || forgets !== browserForgets ? storageGet(NAME_STORAGE_KEY) || "" : name;
    }
    const fileField = form.querySelector("[data-file-field]");
    if (fileField) {
      clearFileField(fileField);
    }
    release(false);
    // A reader who left while the post was on its way has a page of their own
    // choosing on screen now, perhaps another post being written: the post
    // went, and that page is left as it is.
    if (!onScreen()) {
      return;
    }
    await navigate(`${threadPath(created.board, created.thread_no)}#p${created.no}`);
  } catch (err) {
    // Switched back on first, so a picker switched off below stays off. A
    // redraw while the post was held may have release switch the picker off,
    // taking the image off, and say why (imageOff).
    const shown = onScreen();
    const imageOff = release(shown);
    button.textContent = label;
    // A credential the server will not honour again is dropped rather than
    // offered for the next post; the response says which case this is. The
    // option goes with it (drawBadgeOption), offering the badge storage holds
    // now in its place, unticked, or none — drawing the box again in this form
    // rather than re-rendering leaves the half-written post exactly where it is
    // — so the retry that follows is an ordinary post instead of the same
    // refusal. Why it went is kept for /verify to say, unless the browser was
    // forgotten meanwhile: the badge went with it, a badge stored since is
    // someone else's, and the note would tell the next person there had been
    // one. What was refused is the credential this post sent (held), and only
    // that one goes, and only while it is the one stored: storage may hold
    // another by now, verified since in this browser, in another tab or by a
    // confirmation in this one, which is what revoked this one, or none,
    // forgotten since. Deleting whatever was stored then deleted that badge,
    // which the server hands out once and cannot show again. The page reads
    // that one (readStoredBadge), as it does for any change of storage
    // (followCredentialStorage), so a code step drawn here before that
    // verification goes as it would there (codeSpentElsewhere). The form
    // offered the one refused, so its box goes either way. Its box was ticked,
    // so the post it goes on to is one the box no longer skips the puzzle for:
    // that starts now, as for the reader's own untick (captchaBadgeToggled), on
    // a form still on screen with some of the post in it. A form still on
    // screen is told by the refusal's notice (below). One off the screen, while
    // a post link's lookup loads say, is told nothing here, so it is noted as
    // for any ticked badge that goes from under a form (withdrawBadgeOption,
    // starting nothing), and keeps what it would have been told, worded when it
    // is said (below). A redraw that carries it held, begun before the answer
    // or after it while that notice is still unsaid (captureDraft), says that
    // notice in place of the note, as it puts its values back into its own page
    // (restoreDraft) or keeps that page (render). A press before then drops
    // that notice and is refused once by the note, rather than posting without
    // the badge unsaid (submit), unless it is refused before that, as empty
    // say, when the note stays, for the next press or for the form a redraw
    // then draws (formDraft). When the page holds another badge by then, and
    // the browser was not forgotten meanwhile, the form is told so in its own
    // words (badgeReplacedNotice), on screen now or, off it, when a redraw
    // tells it, if the browser has not been forgotten by then either, as a form
    // off the screen is too when the page holds none now but one by the time a
    // redraw tells it; not in the server's words, which send the reader to
    // verify again. That notice is its one refusal for the badge: a note left
    // on it while the post was on its way goes, whatever left it, so the next
    // press is not refused for the badge again. What can leave one then is a
    // change of badge in another tab (followCredentialStorage), a confirmation
    // answered in this one (submitVerification), another badged post's refusal
    // in this one (below), /verify loading over the form (loadVerification), or
    // a redraw that puts it back (restoreDraft). The other posting forms on the
    // page the reader went to, drawn for the badge that has gone, are brought
    // into line with the one held now as for a badge gone in another tab, and
    // so is /verify there showing the panel for it, drawn again to say why it
    // went.
    const reason = err.payload && err.payload.credential;
    let replaced = false;
    if (held && (reason === "expired" || reason === "revoked")) {
      if (forgets === browserForgets && storedCredential() === held.token) {
        forgetCredential();
        badgeLost =
          reason === "expired"
            ? "Your badge has expired."
            : "That badge was replaced by a newer verification.";
      } else if (forgets === browserForgets) {
        readStoredBadge();
      }
      replaced = forgets === browserForgets && state.verification.badge !== null;
      offerHeldBadge(false, form);
      const panel = el.main.querySelector('[data-action="forget-verification"]');
      if (onVerifyPage() && ageStatus() === "ok" && panel) {
        renderVerify();
      }
      if (shown) {
        drawBadgeOption(form);
        if (replaced) {
          badgeWithdrawn.delete(form);
        }
        if (captchaFormNeedsIt(form) && captchaFormHasWriting(form)) {
          prepareCaptcha();
        }
      } else {
        withdrawBadgeOption(form, true);
      }
    }
    // The site takes no images, which this page was drawn without knowing:
    // /api/site did not answer, or the server has been switched over since.
    // The page knows for the rest of the visit, so the next form it draws has
    // no image field, and the image comes off this one as below.
    const uploadsOff = err.status === 403 && err.message === UPLOADS_SWITCHED_OFF;
    if (uploadsOff) {
      state.site.uploads = false;
    }
    // What a notice says the form still holds is what it holds when the notice
    // is worded, here after release: its text, read from the box, and the image
    // it sent, if any, unless release took that off, which the notice then
    // says, with why (imageTakenOff). The two refusals of the image itself say
    // so in their own words. A notice worded again later, by a redraw say,
    // reads the text and the image then (below).
    const box = field(form, "body");
    const hasText = Boolean(box && box.value.trim());
    const hasImage = Boolean(file);
    const limitReached = err.status === 403 && err.message === IMAGE_LIMIT_REACHED;
    // A thread archived, locked or gone (or, for a new thread, its board gone)
    // takes no post at all, whatever is left of this one.
    const closed =
      err.status === 404 || (err.status === 403 && (err.message === "thread is archived" || err.message === "thread is locked"));
    // Worded for the image release took off, or for none, for the form it is
    // said to and for whether that form holds text and an image, and kept with
    // the form, with whether it is the notice of a replaced badge as things
    // stand when it is worded (refusalNotices), but for a post the forget
    // stopped, whose words no change of badge since makes untrue: a redraw
    // still loading that then draws the form again words it again, for the form
    // that redraw draws and the text and image put back into it, with the
    // picker's reason where it takes the image off, as it would have read had
    // the redraw come first (restoreDraft); a change of badge or a forget heard
    // while it is still the notice on screen words it again in place
    // (rewordRefusal); and a form off the screen, told nothing here, is told it
    // worded as things stand when it is told (below). The notice of a replaced
    // badge is worded so only while the browser has not been forgotten since
    // the press: a badge stored after a forget is someone else's, so the form
    // is then told the server's words (submit). It is the notice of a replaced
    // badge too when storage holds none at the answer but holds one by the time
    // it is worded again, for a form on screen while its notice is still up
    // (rewordRefusal) and for one off the screen when a redraw tells it,
    // verified in another tab meanwhile say, so the form is not sent to verify
    // again for a badge the browser holds. A post not sent from a form off the
    // screen (below) is worded as the forget's if the browser had been
    // forgotten since the press by the time it was dropped, which would have
    // stopped it on screen too, and otherwise as one not sent because the
    // reader left its page (errorMessage).
    const badgeRefused = Boolean(held) && (reason === "expired" || reason === "revoked");
    const stoppedByForget = err.notSent && forgotten();
    const replacedNow = () => (replaced || (badgeRefused && state.verification.badge !== null)) && !forgotten();
    const said = (off, on, text, image) =>
      stoppedByForget
        ? FORGOTTEN_BEFORE_SENDING
        : replacedNow()
          ? badgeReplacedNotice(on, { hasText: text, hasImage: image, imageOff: off })
          : errorMessage(err, { hasText: text, hasImage: image, imageOff: off });
    const notice = (off, on = form, text = hasText, image = hasImage) =>
      imageTakenOff(said(off, on, text, image), off, { hasText: text, isThread, closed });
    // A form that has left the screen, with or without its post sent, is said
    // nothing about on the page the reader went to, and that page is changed
    // for it only as above, for the badge the refusal forgot: its posting forms
    // lose the option, and /verify showing that badge's panel is drawn again,
    // saying why the badge went. What the refusal said about the badge and the
    // server is kept, above, and so is a note on the form, which refuses a
    // press made before a redraw tells it. So is what the form would have been
    // told on screen, not as words but as the way to word it (unsaidNotices),
    // with whether it had the focus at the press and whether that refuses its
    // image, and its image goes, as on screen, after a refusal of the image
    // itself, so that a redraw that carries it held, after a post link's lookup
    // into its thread say, or begun after this, Update or Back say, can say it
    // then, worded for the form then drawn and the text and image it holds
    // then, as it puts its values back into its own page (restoreDraft) or
    // keeps that page when it will not load (render), whose picker it then
    // switches off as on screen (blockRefusedPicker), as the form's next press
    // does if it comes first, unless an image has been attached since (above).
    if (!shown) {
      const imageRefused = limitReached || uploadsOff;
      const fileField = form.querySelector("[data-file-field]");
      if (imageRefused && fileField) {
        clearFileField(fileField);
      }
      const word = (on, text, image) =>
        imageRefused ? errorMessage(err, { hasText: text, imageOff }) : notice(imageOff, on, text, image);
      if (!imageRefused && !stoppedByForget) {
        refusalNotices.set(form, { notice, replaced: replacedNow, off: imageOff, told: "", after: "" });
      }
      unsaidNotices.set(form, {
        word,
        focused,
        badgeRefused,
        imageRefused,
        limitReached,
      });
      return;
    }
    // A post sendWithCaptcha did not send from a form still on screen was
    // stopped by the forget: before its first send, or before the second that
    // a refused answer calls for, the forget heard while the first was on its
    // way or while the second puzzle was solving. It says so rather than
    // wording a refusal, and keeps its text for whoever presses again.
    if (err.notSent) {
      showNotice(imageTakenOff(FORGOTTEN_BEFORE_SENDING, imageOff, { hasText, isThread }));
      return;
    }
    // The cap was reached after this form was drawn, or the site stopped taking
    // images: take the image off and switch the picker off, so the next press
    // posts the text that is left (blockRefusedPicker).
    if (limitReached || uploadsOff) {
      blockRefusedPicker(form, limitReached);
      showNotice(errorMessage(err, { hasText, imageOff }));
      return;
    }
    const told = notice(imageOff);
    refusalNotices.set(form, { notice, replaced: replacedNow, off: imageOff, told, after: "" });
    showNotice(told);
  }
});

// submitVerification drives both halves of the verification form. Nothing is
// remembered between them but the badge on offer: the code is confirmed against
// a short-lived cookie the server set, so the address is typed once and never
// travels again.
//
// An answer belongs to the step it was sent from (sentFrom), and is dropped
// when the reader has moved the page to another step while it was on its way:
// no badge stored, no step set, nothing drawn or said. Forget this browser,
// pressed here or in another tab, is the button that can do that, and has to
// win: a badge stored after it would stay for whoever uses the computer next,
// under a confirmation saying there is none. The form's own other buttons —
// Keep my badge, Use a different address — wait for the answer with the submit
// button, so a reader does not give up a step whose badge the server may be
// issuing; a dropped badge is one nobody holds, and verifying the same address
// again retires the old one. Leaving the page is not a change of step: the
// answer is kept for when the reader comes back (onVerifyPage). Coming back
// draws the step afresh, so the wait is kept in verifyBusy as well as on the
// form's buttons, and the step is drawn again waiting (verifyViewHTML); a
// second request from a step whose first is out is never sent, since the page
// could then show the first one's answer while the server acts on the second. A
// request from another step is sent only once submitVerification is done with
// the one before it (verifyRequest), so their answers set the pending-code
// cookie in the order they were asked for and the newest one's is the cookie
// the browser keeps; if the reader moves the page to yet another step while it
// waits, it is never sent. That wait ends, because every request is given up
// once its time is out (verifyDeadline), and a code request's puzzle gives up
// too (solveCaptcha): a Send code or Verify given up is refused as unreachable
// on its own step and dropped on any other, and the GET renderVerify asks once
// a badge is issued leaves the page with what it knew, that badge drawn
// (loadVerification). One whose answer never came once held every later
// request, from any step, until a reload.
//
// A code request needs the captcha while it is on (sendWithCaptcha). It solves
// its puzzle in its turn, so one request at a time holds across the puzzle: a
// second press of the step meanwhile sends nothing, and a request from another
// step waits for it. Send code says "Checking…" while the puzzle is solved
// (VERIFY_SOLVING), here and in the step drawn again, and "Sending…" once the
// request goes; its time is counted from then, not from the press. A request
// whose form has left the screen by the time its puzzle is solved, the reader
// gone to another page or /verify drawn again, is not sent, and its answer is
// kept for the next press while it is still good and the page holds no other
// (keepCaptcha); the step drawn again is let go. A forget moves the page to a
// new step, so a request still waiting on its puzzle or its turn is not sent
// either. The form's fields wait with its buttons (holdForm), so what it shows
// meanwhile is what is sent.
//
// What an answer draws is brought into view clear of the sticky header — the
// code box with the sentence above it, the badge now held (revealClearOfHeader)
// — and focused without scrolling the page again.
async function submitVerification(form) {
  if (verifyBusy === verifyStep) return;
  const button = form.querySelector('button[type="submit"]');
  const label = button.textContent;
  const data = new FormData(form);
  const requesting = form.dataset.verify === "request";
  const sentFrom = verifyStep;
  clearVerifyRefusal(form);

  // The badge is for students aged 18 and over. Without the box ticked the
  // page refuses on its own and sends nothing, the address included. It says
  // so under the box.
  const adult = requesting ? field(form, "adult") : null;
  if (requesting && !(adult && adult.checked)) {
    refuseVerification(form, BADGE_AGE_REFUSAL, adult, { invalid: true });
    return;
  }

  // The page the form is on, as for a post: the one noted as it was drawn
  // (drawnFor), never a render begun since that has not drawn yet, after a link
  // pressed just before say. A code request whose form has left it, or the
  // screen, by the time its puzzle is solved is not sent (formOnScreen), and
  // nor is one whose step the page has left meanwhile.
  const route = drawnFor.route;
  const onScreen = () => formOnScreen(form, route) && verifyStep === sentFrom;

  const buttons = [...form.querySelectorAll("button")];
  for (const b of buttons) b.disabled = true;
  // The fields wait too, until the answer, so what the form shows meanwhile is
  // what is sent: the address and the box, or the code (holdForm).
  const unhold = holdForm(form);
  button.textContent = requesting ? VERIFY_SENDING : VERIFY_CHECKING;
  verifyBusy = sentFrom;
  clearNotice();
  const earlier = verifyRequest;
  let release;
  verifyRequest = new Promise((resolve) => {
    release = resolve;
  });
  try {
    // Whatever the request before this one was, its answer, and the cookie
    // with it, is in before this one goes.
    await earlier;
    if (verifyStep !== sentFrom) return;
    if (requesting) {
      // A code request always needs the captcha when it is on: no badge
      // travels with it to skip one. The puzzle is solved in this request's
      // turn, so a request from another step waits for it too.
      const sent = await sendWithCaptcha(
        () => state.site.captcha,
        (answer) => {
          // adult is true here, because an unticked box stopped above.
          const request = { email: String(data.get("email") || ""), adult: adult.checked };
          if (answer) {
            request.captcha = answer;
          }
          // Its time is counted from here, as it is sent, not from the press:
          // the puzzle before it is not the request's to wait for.
          return api("/api/verify/request", {
            method: "POST",
            signal: verifyDeadline(),
            body: JSON.stringify(request),
          });
        },
        (solving) => {
          sentFrom.solving = solving;
          button.textContent = solving ? VERIFY_SOLVING : VERIFY_SENDING;
        },
        onScreen,
      );
      if (verifyStep !== sentFrom) return;
      verifyStep = { step: "code", label: sent.label || "" };
      // A reader who has gone on to another page finds the code box when
      // they come back; this one is not drawn over the page they are on.
      if (!onVerifyPage()) return;
      drawVerifyStep();
      showVerifyField("f-code");
      return;
    }
    // The one time the credential is ever handed out. It is kept here rather
    // than set as a cookie, so nothing sends it until a post asks for a badge.
    // Until it is answered it is counted on the code step it confirms, so
    // another tab's verification meanwhile leaves the step to this answer
    // (codeSpentElsewhere), even once /verify is drawn again over this form.
    sentFrom.confirming = (sentFrom.confirming || 0) + 1;
    let issued;
    try {
      issued = await api("/api/verify/confirm", {
        method: "POST",
        signal: verifyDeadline(),
        body: JSON.stringify({ code: String(data.get("code") || "") }),
      });
    } finally {
      sentFrom.confirming -= 1;
    }
    if (verifyStep !== sentFrom) return;
    storeCredential(issued.credential);
    // Read locally at once, so whatever is drawn next — this page, or the next
    // posting form for a reader who has gone on — shows the badge now held,
    // and so does a posting form already on screen for that reader, drawn
    // offering the badge before, one that had it ticked noted to say so at its
    // next press (offerHeldBadge). On the Verify page there is no posting
    // form, and nothing for it to do. Drawing /verify again withdraws nothing
    // if its GET /api/verify fails (loadVerification). The badge goes only
    // when the server says the feature is off, when it runs out, and when it
    // is refused on a post, forgotten or replaced, in this tab or in another
    // (followCredentialStorage).
    state.verification.badge = readBadge();
    offerHeldBadge();
    verifyStep = { step: "email", label: "" };
    if (!onVerifyPage()) return;
    if (await renderVerify()) showHeldBadge();
  } catch (err) {
    unhold(false);
    if (verifyStep !== sentFrom) return;
    // A confirmation another tab's verification found on its way: refused,
    // whatever the refusal said, its step goes now as it would have then, and
    // /verify, showing it, is drawn again saying why (codeSpentElsewhere).
    if (sentFrom.spentElsewhere) {
      codeSpentElsewhere();
      return;
    }
    // Nothing is pending any more, so the code box goes: back to the address
    // step, still over a badge held here, which is kept until a new one is
    // confirmed, and saying in that step why the box went.
    const ended = !requesting && [NO_CODE_PENDING, CODE_EXPIRED, CODE_GUESSES_SPENT].includes(err.message);
    if (ended) {
      verifyStep = { step: "email", label: "", replacing: true, ended: CODE_ENDED_NOTES[err.message] };
    }
    // The answer is about the Verify page. A reader who has gone on to
    // another page is not shown it there; the step it left is what they find
    // when they come back.
    if (!onVerifyPage()) return;
    if (ended) {
      drawVerifyStep();
      // The reason, not the notice at the top, is brought into view: on a
      // phone that is a screen above the form.
      const why = document.getElementById("f-email-ended");
      if (why) why.scrollIntoView({ block: "center" });
      const input = document.getElementById("f-email");
      if (input) input.focus({ preventScroll: true });
      return;
    }
    // The form on screen is this one, or, for a reader who went to another
    // page and came back while the answer was on its way, the same step drawn
    // again, waiting as this one was, with its fields empty. Its buttons are
    // live again either way. With neither on screen the step is still to be
    // drawn for that reader: renderVerify draws it, its buttons live by then,
    // and says the refusal as the step drawn again. It can do that after this
    // form has said it, too: Verify in the header, pressed just before the
    // answer came, leaves this form up while render() asks the server, then
    // draws the step afresh.
    const shown = form.isConnected ? form : el.main.querySelector("form[data-verify]");
    // A code request not sent, its form gone from the screen by the time its
    // puzzle was solved (sendWithCaptcha), is said nowhere: the reader was not
    // there to see it go, and the step drawn again has nothing in it to send.
    // That step is let go, and the answer kept for its press.
    if (err.notSent) {
      if (shown) {
        for (const b of shown.querySelectorAll("button")) b.disabled = false;
        shown.querySelector('button[type="submit"]').textContent = label;
      }
      return;
    }
    // Any other refusal leaves the step as it is, and is said in its form,
    // under the field it is about (refuseVerification): a wrong code puts the
    // reader back in the code box with the code selected, to be typed again.
    // It is kept with the step (verifyRefusal), so the step drawn again says
    // it too, as it is said there (redrawn): that step's fields are empty, so
    // only this form, which still holds what was typed, is told a request
    // with no answer left it in the box (sayVerifyRefusal). The server's
    // refusal of a request without the attestation reads the same as the
    // page's own.
    const refusedForAge = err.status === 400 && err.message === BADGE_AGE_REFUSED;
    verifyRefusal = {
      step: sentFrom,
      message: refusedForAge ? BADGE_AGE_REFUSAL : errorMessage(err, { action: "code" }),
      redrawn: refusedForAge ? BADGE_AGE_REFUSAL : errorMessage(err, { action: "code", kept: false }),
      about: refusedForAge ? "adult" : requesting ? "email" : "code",
      invalid: err.status === 400,
      select: !requesting && err.status === 400,
    };
    if (!shown) return;
    for (const b of shown.querySelectorAll("button")) b.disabled = false;
    shown.querySelector('button[type="submit"]').textContent = label;
    sayVerifyRefusal(shown, shown === form);
  } finally {
    // The answer is in, kept or dropped. A request sent since from another
    // step — after Forget this browser — is still out, and keeps its wait;
    // it goes now, if it was waiting for this one.
    if (verifyBusy === sentFrom) verifyBusy = null;
    release();
  }
}

/* Bootstrap ---------------------------------------------------------------- */

// The board list, whether verification exists here, and what the site takes
// are fetched the first time a page past the age screen needs them, and kept
// from then on. A board list that fails is not kept: the next navigation tries
// again.
let siteLoaded = false;

async function loadSite() {
  if (siteLoaded) {
    return;
  }
  const [{ boards }] = await Promise.all([
    api("/api/boards"),
    // The posting form needs to know whether to offer the badge checkbox, and a
    // failure here must not stop the board from loading.
    loadVerification(),
    // And whether to offer an image at all, how large, and whether posting
    // needs the captcha. Like the board list it says nothing about the reader,
    // and nothing about the reader is sent for it. Without an answer the forms
    // stay as they always were.
    api("/api/site")
      .then((site) => {
        const max = Number(site.max_upload_bytes);
        state.site = {
          uploads: site.uploads !== false,
          maxUploadBytes: max > 0 ? max : 0,
          captcha: site.captcha === true,
        };
      })
      .catch(() => {}),
  ]);
  state.boards = boards;
  siteLoaded = true;
}

render();
