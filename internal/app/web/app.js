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
  // enabled comes from the server; badge is read out of the credential this
  // browser holds, which the server is never asked about outside of using it.
  verification: { enabled: false, badge: null },
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
    // No answer at all: offline, a dropped connection, a blocked request.
    const err = new Error(UNREACHABLE);
    err.unreachable = true;
    throw err;
  }
  const payload = res.status === 204 ? null : await res.json().catch(() => null);
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
// and imageOff, why the image is gone when a redraw while the post was held
// had release take it off ("" when it did not: holdForm). The caller adds
// that to anything but the two refusals of the image itself (imageTakenOff).
// action is what was refused — "post", or "code" for a verification code
// request — so a cooldown tells the reader to do that again, not something
// they were not doing.
function errorMessage(err, { hasText = true, imageOff = "", action = "post" } = {}) {
  // A post of only an image, which release took off: nothing is left of it.
  const nothingLeft = !hasText && imageOff !== "";
  if (err.unreachable) {
    return nothingLeft
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
  // The captcha refused the fresh answer sent after it refused the first,
  // which a working server does not: nothing was posted or sent, and pressing
  // the button again solves another. A first refusal is answered by that
  // second send or, when the form has gone or the browser was forgotten since
  // the press, by an error marked notSent instead, which the callers answer
  // without coming here.
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
// the option, drawn so or with it since taken out (dropBadgeOption), does not.
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
  // when they do.
  const held = state.verification.badge;
  // A badged post takes its ID from the credential rather than the poster key,
  // which is worth saying where the choice is made — and so is the one thing
  // that would tie it to the unbadged posts all the same: a name or tripcode
  // used on both. Ticking the box empties the Name field (badgeToggled).
  const badge = held
    ? `<label class="sage"><input type="checkbox" name="badge" aria-describedby="f-badge-hint" /> post with ${verifiedBadgeHTML(held.label)}</label>
       <span class="hint" id="f-badge-hint">A badged post gets its own ID in each thread. A name or
         tripcode you also use without the badge would tie those posts to it, so the badge starts
         with the name empty.</span>`
    : "";

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

// dropBadgeOption takes the option to post with the badge out of a posting
// form, with the hint under it, and words the form's note on the anti-spam
// check as on a form drawn without the option: a note that still offered the
// badge would describe an option that has gone, and tell whoever reads it
// next that this browser held one. Focus on the box would go with it, to
// nowhere, so it goes to the form's button, beside where the box was.
function dropBadgeOption(form) {
  const box = field(form, "badge");
  const option = box && box.closest("label");
  if (!option) {
    if (box) box.checked = false;
    return;
  }
  const focused = option.contains(document.activeElement);
  option.remove();
  form.querySelector("#f-badge-hint")?.remove();
  const check = form.querySelector("[data-check-hint]");
  if (check) {
    check.textContent = CHECK_HINT;
  }
  if (focused) {
    form.querySelector('button[type="submit"]')?.focus({ preventScroll: true });
  }
}

// The posting forms whose ticked badge went from under them: their option to
// post with it taken out because another tab forgot, refused or replaced it
// (followCredentialStorage), or this tab replaced it, refused it on a post from
// another page or heard it was switched off or had run out, whether the form
// was idle or its badged post was on its way, or by a press that found the page
// holding a badge other than the one the box was drawn for (badgeOffered), or
// drawn again by a redraw that offers another badge or none (restoreDraft), or
// because the server refused it on the form's own post while the form was off
// the screen, where the refusal says nothing (submit).
// Said nothing, the form's next press would post without the badge, under the
// ID the reader's other posts in the thread without one share, which is what a
// badged post's own ID is there to keep apart. So that press is refused, once,
// saying so (submit), and the one after it posts. Each is kept with the badge
// that went, by its token, and the poster key storage held then, for the notice
// to name that ID only while the key is the same (badgeWithdrawnNotice). It
// lives with the form, and a draft carries it across a re-render (formDraft,
// restoreDraft). Forget this browser drops it: the key goes too, so the next
// post has an ID of its own.
const badgeWithdrawn = new WeakMap();

// badgeOffered is the badge each posting form's box was drawn for, by its
// token: the one the page held when formHTML drew the form. A press sends the
// credential only while the page still holds that badge, and treats any other
// as a badge taken out from under the form (badgeWithdrawn): one stored since,
// in this tab or another, is not the one the box names, though the box still
// says "post with" it.
const badgeOffered = new WeakMap();

// noteBadgeOffered notes the badge each posting form on screen with a badge
// box was drawn for. The views call it as they put the forms on screen, in
// the same task as formHTML drew them, so that badge is the one the page holds.
function noteBadgeOffered() {
  const held = state.verification.badge;
  for (const form of el.main.querySelectorAll("form[data-kind]")) {
    if (held && field(form, "badge")) {
      badgeOffered.set(form, held.token);
    }
  }
}

// withdrawBadgeOption is dropBadgeOption for a badge that has gone from under
// the form, offered, the one the page held and the box was drawn for, noting
// the form in badgeWithdrawn if its box was ticked. A ticked box taken out of
// a form with some of the post in it also starts the puzzle the form now
// needs, as the reader's own untick would (captchaBadgeToggled), so the press
// after the one the note refuses usually finds it solved; but not with
// startNothing: when the badge went with the browser forgotten in another tab,
// which starts nothing here, or as /verify is drawn over the form
// (loadVerification), or a redraw puts a held form back (restoreDraft), since
// drawing a page starts nothing, or as the form's own badged post is refused
// while the form is off the screen, where nobody is writing in it (submit). A
// form noted then starts it at the press the note refuses (submit).
function withdrawBadgeOption(form, offered, startNothing) {
  const box = field(form, "badge");
  const ticked = Boolean(box && box.checked);
  if (ticked) {
    badgeWithdrawn.set(form, { token: offered.token, key: storageGet(POSTER_KEY_STORAGE_KEY) || "" });
  }
  dropBadgeOption(form);
  if (ticked && !startNothing && captchaFormNeedsIt(form) && captchaFormHasWriting(form)) {
    prepareCaptcha();
  }
}

// What a posting form says when its badge has gone from the browser and it
// was asked to post with it.
const BADGE_GONE = "Your badge is no longer available in this browser — verify again to post with it.";

// badgeWithdrawnNotice is what the press of a form in badgeWithdrawn is told:
// that the badge has gone, and what pressing again does (pressedWithoutBadge).
// A badge another tab stored since is not offered on a form drawn before it,
// and on one drawn since it is not ticked: ticking it is the reader's choice,
// and takes the note off.
function badgeWithdrawnNotice(form, { key }) {
  const gone = !state.verification.badge
    ? BADGE_GONE
    : field(form, "badge")
      ? "The badge you ticked for this post has gone from this browser, and the one this form now offers is not ticked."
      : "The badge this form offered has gone from this browser, and the form offers no other.";
  return `${gone} ${pressedWithoutBadge(form, key)}`;
}

// pressedWithoutBadge says what the next press of a form that has lost its
// badge does: it posts without one. A reply then goes out under the same ID as
// the reader's posts in the thread without a badge while storage holds key, the
// poster key it held when the badge went, the key that ID is made from: a
// forget in another tab that this one hears of only as the badge going, in a
// browser without BroadcastChannel, takes the key away too, and a new thread
// has no posts yet to share an ID with.
function pressedWithoutBadge(form, key) {
  const sameID = form.dataset.kind === "reply" && key !== "" && key === storageGet(POSTER_KEY_STORAGE_KEY);
  return sameID
    ? "Pressing again posts this without a badge, under the same ID as the posts you make in this thread without one."
    : "Pressing again posts this without a badge.";
}

// What a posting form on screen says when the server refuses its badged post
// as expired or revoked while storage holds another badge, verified since in
// another tab of this browser, and the browser was not forgotten since the
// press: a badge stored after a forget is someone else's, so the form is then
// told the server's words (submit).
const BADGE_REPLACED = "The badge this post carried was replaced by one verified since in this browser, so nothing was posted.";

// badgeReplacedNotice is that form's notice, in place of the server's, which
// says to verify again: that would cost another emailed code, and replace the
// badge the browser already holds. It is the form's one refusal for the badge
// that went, so it says what pressing again does (pressedWithoutBadge), and how
// to post with the new badge: the form drawn again offers it, not ticked, as
// for any badge stored since (restoreDraft), by Update on a thread and on the
// board's Index for a new thread. A post release left with nothing in it, its
// image taken off, has nothing to press again for, and the notice then says
// only what went (imageTakenOff says the rest).
function badgeReplacedNotice(form, { hasText, imageOff }) {
  if (!hasText && imageOff !== "") {
    return BADGE_REPLACED;
  }
  const press = pressedWithoutBadge(form, storageGet(POSTER_KEY_STORAGE_KEY) || "");
  const redraw = form.dataset.kind === "reply" ? "press Update" : "open this board's Index again";
  return `${BADGE_REPLACED} ${press} To post it with the new badge, ${redraw}, then tick the badge.`;
}

/* File field behaviour ------------------------------------------------------ */

function isAcceptedImage(file) {
  return ACCEPTED_IMAGES.split(",").includes(file.type) || ACCEPTED_EXTENSIONS.test(file.name);
}

// setFile puts a dropped file into the form's file input, and says whether it
// took it. Assigning a FileList needs a DataTransfer, which is also how the
// draft is carried across a re-render.
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
  updateFileField(field);
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
  noteBadgeOffered();
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
  noteBadgeOffered();
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
// would earn, so a reload starts over rather than resuming.
let verifyStep = { step: "email", label: "" };

// Why the badge that was here has gone, when it is worth saying: it ran out, or
// the server refused it. Shown once on the verification page, which is the only
// page that can do anything about it.
let badgeLost = "";

// How many times "Forget this browser" has been pressed, on this page or, where
// the browser has BroadcastChannel, on any other educhan page open in it. A
// request notes it at the press: one still waiting on its puzzle after another
// press is not sent, and an answer that arrives after one keeps nothing.
// Forgetting has to cover what was already on its way.
let browserForgets = 0;

// Every open educhan tab hears "Forget this browser" pressed in any other on
// FORGET_CHANNEL, so a forget covers what the others hold and have on their
// way, as it does its own. The one message is FORGET_MESSAGE, a constant: it
// says nothing about anyone, only that the browser was forgotten.
const FORGET_CHANNEL = "educhan:forget-browser";
const FORGET_MESSAGE = "forget";
// What the page says once the browser has been forgotten.
const BROWSER_FORGOTTEN = "This browser no longer holds your post key, your name or a badge.";
// What a posting form says when the browser was forgotten before its post went
// through, which stops it going: while it waited on its puzzle for its first
// send or, once the first had gone, before the second that the server's
// refusal of its answer calls for, when a request had gone but nothing was
// posted.
const FORGOTTEN_BEFORE_SENDING = "This browser was forgotten before the post went through, so nothing was posted.";

// What this tab has itself written to storage of what a forget removes, by
// storage key: the credential, the poster key and the remembered name, each as
// it was last written here. The tab a forget is pressed in removes all three
// and then tells the others, so a tab that handled a confirmation or a post in
// between may have written one back before it heard; when it hears, it removes
// each that storage still holds as this tab wrote it (forgottenElsewhere). A
// badge confirmed or a key made in another tab since the forget is never one,
// however late it hears, since both are random; a name is compared by its
// text, so the same name stored since by another tab goes too.
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
// It also cancels what is on its way (forgetOnThisPage), here and, through
// FORGET_CHANNEL, in every other open tab that hears it: a code request or a
// post still waiting on its puzzle is not sent, and a code request, a
// confirmation or a post sent before it keeps nothing when its answer comes
// back after it, neither the badge the server issued nor the code step nor the
// name, nor a note that a badge was lost.
function forgetBrowser() {
  forgetOnThisPage();
  // Emptied before it is removed, so that every other tab hears the badge go
  // as the browser forgotten, in the order storage keeps, whether this
  // forget's message on FORGET_CHANNEL reaches it before the storage event or
  // after, which no browser promises either way (followCredentialStorage).
  storageSet(CREDENTIAL_STORAGE_KEY, "");
  forgetCredential();
  for (const key of [POSTER_KEY_STORAGE_KEY, NAME_STORAGE_KEY]) {
    try {
      localStorage.removeItem(key);
    } catch {
      // Nothing was stored, so nothing needs removing.
    }
  }
  // Storage is shared, and has been cleared here, so the other tabs remove
  // only what storage still holds as they last wrote it themselves
  // (storedHere): what they wrote back before they heard, or a name stored
  // since in the same words.
  if (forgetChannel) {
    forgetChannel.postMessage(FORGET_MESSAGE);
  }
}

// forgetOnThisPage is the part of forgetting the browser that a page holds
// itself rather than in storage, run in the tab where it was pressed and in
// every tab that hears it. It cancels what is on its way from the page
// (browserForgets), a redraw's draft included (restoreDraft); drops the badge
// read from storage, the poster key the page keeps for itself, what it noted
// it stored, and the code step and the note about a lost badge, which would
// greet the next person with the last one's institution; and forgets every
// posting form on screen (forgetForm). A held form a redraw has taken off the
// screen, while a post link's lookup draws its skeleton say, is forgotten as
// the redraw puts it back (restoreDraft).
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
// as well. Nothing is said at the form's next press, though the badge was
// ticked or the form noted after another tab took a badge away
// (badgeWithdrawn): that would tell whoever presses it that this browser held
// a badge, and with the poster key gone too the post has an ID of its own,
// shared with none of the posts made before the forget. It notes the forget it
// did (forgottenAt).
function forgetForm(form) {
  const name = field(form, "name");
  if (name) {
    name.defaultValue = "";
    name.value = "";
  }
  badgeWithdrawn.delete(form);
  dropBadgeOption(form);
  forgottenAt.set(form, browserForgets);
}

// The forget that last reached each posting form, by form, as browserForgets
// counted it (forgetForm). A held form a redraw carries across is forgotten as
// it is put back only for a forget it missed while off the screen, not again
// for one it had on screen: the Name it shows then was typed after that
// forget, and a post pressed since may be going out under it (restoreDraft).
const forgottenAt = new WeakMap();

// forgottenElsewhere is a tab's answer to Forget this browser pressed in
// another: what this tab wrote back to storage since that tab cleared it goes
// (storedHere), and what the page holds goes, as it did there, and /verify,
// when it is on screen here, is drawn again without the badge or the code
// step. A badge or a key storage holds that this tab did not write stays,
// whenever this tab hears: it was stored since the forget, by someone else. A
// name stored since in the words this tab last wrote goes too, which forgets
// more than it needs to, never less.
function forgottenElsewhere() {
  for (const [key, value] of storedHere) {
    if (storageGet(key) === value) {
      storageRemove(key);
    }
  }
  forgetOnThisPage();
  if (state.route.view === "verify" && ageStatus() === "ok") {
    renderVerify(state.route).then((drawn) => {
      if (drawn) {
        showNotice(BROWSER_FORGOTTEN);
      }
    });
  }
}

// forgetChannel is this tab's end of FORGET_CHANNEL, opened once at start-up,
// or null in a browser without BroadcastChannel, or one that will not open it:
// there a forget covers the tab it was pressed in, and the other tabs drop
// only the badge, as they do for Forget this credential
// (followCredentialStorage).
const forgetChannel = openForgetChannel();

function openForgetChannel() {
  if (typeof BroadcastChannel !== "function") {
    return null;
  }
  try {
    const channel = new BroadcastChannel(FORGET_CHANNEL);
    channel.addEventListener("message", (event) => {
      if (event.data === FORGET_MESSAGE) {
        forgottenElsewhere();
      }
    });
    return channel;
  } catch {
    return null;
  }
}

// followCredentialStorage is a tab's answer to the badge changing in another
// tab of this browser: Forget this credential or Forget this browser pressed
// there, the server refusing the badge there as run out or replaced, or another
// address verified there. The browser holds one badge, in storage, so this tab
// reads it again, and a badged post from here sends only the badge the browser
// still holds; the next form or /verify page this tab draws shows it. If that
// is not the one this tab offered, every posting form on screen loses the
// option, which named the badge that has gone, and one that had it ticked says
// so at its next press (withdrawBadgeOption), as does a held form a redraw has
// taken off the screen, once it is put back (restoreDraft); and /verify, when
// it shows the badge panel, is drawn again, if the panel is still on screen
// once the page has asked the server (renderVerify). /verify showing the
// address step is left as it is, whatever badge it was drawn over, as is one
// the reader moves to while the panel is drawn again, so a reader filling it
// in, or waiting on a code request, loses nothing. So is a code step, unless
// the badge read in is one newly verified there (codeSpentElsewhere). An
// emptied credential is the browser forgotten in another tab (forgetBrowser):
// the options go as for any badge gone, before or after its message on
// FORGET_CHANNEL is heard here, but start no puzzle, since a forget starts
// nothing.
function followCredentialStorage(event) {
  if (event.key !== CREDENTIAL_STORAGE_KEY && event.key !== null) {
    return;
  }
  const offered = state.verification.badge;
  state.verification.badge = state.verification.enabled ? readBadge() : null;
  const held = state.verification.badge;
  if (held && !(offered && held.token === offered.token)) {
    codeSpentElsewhere();
  }
  if (!offered || (held && held.token === offered.token)) {
    return;
  }
  const forgotten = event.key === CREDENTIAL_STORAGE_KEY && event.newValue === "";
  for (const form of el.main.querySelectorAll("form[data-kind]")) {
    withdrawBadgeOption(form, offered, forgotten);
  }
  const panel = el.main.querySelector('[data-action="forget-verification"]');
  if (state.route.view === "verify" && ageStatus() === "ok" && panel) {
    renderVerify(state.route, panel);
  }
}

window.addEventListener("storage", followCredentialStorage);

// What /verify says when the code step it showed is put away after another tab
// verified an address, and what the step's form says of a code refused after
// that while another confirmation of the step is on its way
// (submitVerification). It says the two things that are so whenever it is
// shown, and not that the one caused the other: a confirmation from here that
// reached the server first and lost its answer spent the code itself.
const CODE_SPENT_ELSEWHERE = "An address was verified in another tab, and the code asked for here no longer works.";

// codeSpentElsewhere is a tab's answer to a badge newly verified in another:
// the browser has one verification in progress at a time, held by its cookie,
// so a code step this tab drew, with no confirmation of it on its way, is for a
// code that verified the badge there or was replaced by the one that did, and
// the server takes it no more. The step goes, so /verify next shows the badge
// panel, and is drawn again now, with a word on why, when a code step is on
// screen and still is once the page has asked the server (renderVerify): an
// address step the reader moved to meanwhile is left as it is. A confirmation
// of the step already on its way, counted on the step (confirming) whether its
// form is still on screen or /verify has been drawn again over it since, may
// have reached the server before the other tab's verification and verified this
// tab's address, so it is left to its answer: the step is only marked
// (spentElsewhere), and submitVerification settles it. A success stores the
// badge it earned and draws the panel for it wherever /verify shows a code step
// or the panel, both when it is answered and once the page has asked the
// server; a refusal of any kind comes back here, and the step goes as above
// once no other confirmation of it is on its way; while one is, the refused
// form, still on screen, says why its code was refused (CODE_SPENT_ELSEWHERE).
// A code request from this tab still on its way is not a code step yet, and
// draws its own when answered: the cookie that answer sets holds the browser's
// verification from then on, and its code is the one asked for.
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
  if (state.route.view === "verify" && ageStatus() === "ok" && step) {
    renderVerify(state.route, step).then((drawn) => {
      if (drawn) {
        showNotice(CODE_SPENT_ELSEWHERE);
      }
    });
  }
}

// forgetBrowserHTML is the control for forgetBrowser, on the Verify page
// whatever state verification is in, since the key and the name are there
// for every poster.
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
    </div>`;
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
    return { token, label: claims.label, expires };
  } catch {
    forgetCredential();
    return null;
  }
}

async function loadVerification() {
  let enabled;
  try {
    // The only thing worth asking the server: whether the feature exists here.
    // What this browser holds is a local question with a local answer.
    enabled = !!(await api("/api/verify")).enabled;
  } catch {
    // Verification is an extra; the board works without knowing about it. A
    // failed request withdraws nothing the page knew: the feature stays as the
    // last answer had it, off only until one has come, and the badge is read
    // from storage as after any answer, so one confirmed just before this
    // request is still offered.
    enabled = state.verification.enabled;
  }
  const offered = state.verification.badge;
  state.verification = { enabled, badge: enabled ? readBadge() : null };
  // A posting form still on screen, the page before /verify while it is
  // drawn, offers the badge it was drawn for: switched off on the server, or
  // run out, that badge goes from it as for one gone in another tab
  // (followCredentialStorage), but starts no puzzle: this is /verify being
  // drawn over the form, and drawing a page never starts one. A form noted
  // here and drawn again by a later render starts it at the press the note
  // refuses.
  const held = state.verification.badge;
  if (offered && !(held && held.token === offered.token)) {
    for (const form of el.main.querySelectorAll("form[data-kind]")) {
      withdrawBadgeOption(form, offered, true);
    }
  }
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

function verifyViewHTML() {
  const v = state.verification;

  if (!v.enabled) {
    return `
      <div class="board-head"><h1>Institutional verification</h1></div>
      ${VERIFY_INTRO}
      <p class="empty">Verification is not configured on this server.</p>
      ${forgetBrowserHTML()}`;
  }

  // The badge panel, unless the reader is replacing the badge or a code is
  // pending: a code asked for over a badge, after Verify another address or
  // with its request on its way while another tab verified one, is typed in
  // all the same, since the badge it earns replaces the one held. A code step
  // drawn before another tab verified a badge is put away, once any
  // confirmation of it on its way is answered (codeSpentElsewhere).
  if (v.badge && !verifyStep.replacing && verifyStep.step !== "code") {
    return `
      <div class="board-head"><h1>Institutional verification</h1></div>
      ${VERIFY_INTRO}
      <div class="verify-panel">
        <p>Your badge in this browser is ${verifiedBadgeHTML(v.badge.label)}, good until
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

  if (verifyStep.step === "code") {
    return `
      <div class="board-head"><h1>Institutional verification</h1></div>
      ${VERIFY_INTRO}
      <form class="verify-panel fields" data-verify="confirm">
        <p>A code is on its way. It earns you ${verifiedBadgeHTML(verifyStep.label)} and expires shortly.</p>
        <div class="field">
          <label class="label" for="f-code">Code from the email</label>
          <input type="text" id="f-code" name="code" inputmode="numeric" autocomplete="one-time-code"
                 maxlength="12" placeholder="6 digits" required />
        </div>
        <div class="actions">
          <button class="btn primary" type="submit">Verify</button>
          <button class="btn subtle" type="button" data-action="verify-restart">Use a different address</button>
        </div>
      </form>
      ${forgetBrowserHTML()}`;
  }

  const lost = badgeLost
    ? `<p class="verify-note">${esc(badgeLost)} Verify again to get a new one.</p>`
    : "";

  return `
    <div class="board-head"><h1>Institutional verification</h1></div>
    ${VERIFY_INTRO}
    ${lost}
    <form class="verify-panel fields" data-verify="request">
      <div class="field">
        <label class="label" for="f-email">Institutional email</label>
        <input type="email" id="f-email" name="email" maxlength="254" autocomplete="email"
               placeholder="you@institution.edu" required />
        <span class="hint">Academic domains only &mdash; .edu, .ac.in, .ac.uk, .edu.au and others.</span>
      </div>
      <label class="attest" for="f-adult">
        <input type="checkbox" id="f-adult" name="adult" autocomplete="off" />
        <span>I confirm I am 18 or over</span>
      </label>
      <div class="actions">
        <button class="btn primary" type="submit">Send code</button>
      </div>
    </form>
    ${forgetBrowserHTML()}`;
}

// renderVerify draws /verify for route, the render or the press that asked for
// it, unless a newer render has begun while it asked the server: then, like the
// other views, it draws nothing and says nothing (overtaken). A redraw that is
// not a render passes over, what it found on screen that called for it, or for
// Forget this credential the button pressed, and draws nothing either if that
// has left the screen while it asked: the reader has moved on within /verify
// meanwhile, to an address step they may be filling in, say, or a forget has
// drawn the page again. Forget this browser's redraw, pressed here or heard
// from another tab, passes nothing: it draws whatever /verify then shows, so
// that its notice is said. It returns whether it drew.
async function renderVerify(route, over) {
  await loadVerification();
  if (overtaken(route) || (over && !over.isConnected)) {
    return false;
  }
  el.main.innerHTML = verifyViewHTML();
  // Explained once, on the page that can do something about it.
  badgeLost = "";
  announce("Institutional verification");
  return true;
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
// With skeleton false the current view stays, dimmed, however long it takes.
// The skeleton is drawn, and the finish it returns takes the dimming off, only
// while its render is still the latest: an overtaken render's skeleton would
// blank the newer page for good, since the overtaken render then draws
// nothing, and its finish would show a newer render still loading, a kept page
// say, as loaded. The latest is counted rather than asked of overtaken: a kept
// page whose redraw fails puts back the route it was drawn for, which
// overtakes that redraw too.
function beginLoading({ skeleton = true } = {}) {
  const begun = rendersBegun;
  el.main.setAttribute("aria-busy", "true");
  loadingTimer = setTimeout(() => {
    if (skeleton && begun === rendersBegun) {
      el.main.innerHTML = '<div class="skeleton"><div class="skeleton-row"></div><div class="skeleton-row"></div><div class="skeleton-row"></div></div>';
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
// newer one: the board, thread and /verify views ask once their request is
// answered, /verify drawn again after a forget or a confirmation too, and
// render after the board list and before anything else it does to the page. A
// redraw that fails and keeps its page puts back the route that page was drawn
// for, which overtakes every render still loading.
function overtaken(route) {
  return state.route !== route;
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
  // A redraw of the page a post is on its way from keeps the page on screen
  // (below), and with it what the page's controls read, its thread and the
  // image open on it, until a new page is drawn over it: a refusal answered
  // meanwhile updates the page it is shown on.
  const keepPage = Boolean(draft && draft.held && draft.key === routeKey(route));
  if (!keepPage) {
    state.thread = null;
    // Whatever was expanded belongs to the view being replaced.
    expandedLink = null;
  }
  renderNav(route);
  clearNotice();
  hideQuotePreview();

  // Until the age screen has its answer, nothing of the site is shown or even
  // fetched, apart from the two pages that say what answering means.
  const age = ageStatus();
  const ungated = UNGATED_VIEWS.has(route.view);
  if (age !== "ok" && !ungated) {
    renderAgeGate(age);
    return;
  }
  // Those two are read with the screen put away; it comes back, as it was
  // left, on the next page that needs it.
  closeAgeScreen();

  // A redraw of the page a post is on its way from keeps the page on screen,
  // dimmed, rather than a skeleton, so the form the post went from stays in
  // the document until it goes into the new page (restoreDraft).
  const finish = beginLoading({ skeleton: !keepPage });
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
        await renderVerify(route);
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
    // A redraw that kept the page for a post on its way leaves the page there,
    // with the form the post went from, when the new one will not load: the
    // post goes ahead and is answered on its form, and a success goes to the
    // new post, as without the redraw. The address bar, renderedPath and the
    // route are put back as the page was drawn for them (drawnFor), however
    // many redraws of it have failed, and so is its thread, which a refusal
    // answered meanwhile has updated; the image open on it stays open. The
    // address bar goes back by replacing the history entry the reader is on,
    // the fragment kept, so a jump within the page or a reload stays on the
    // page kept. After a link, with nothing pressed while the redraw loads, the
    // history then holds that page twice, and the first Back from it stays on
    // it (popstate finds the address the page was drawn for and only moves to
    // the fragment); a Back or Forward that will not load leaves the entry it
    // went to holding the page kept. A link pressed while a redraw for another
    // address loads, the same one again or a jump within the page, adds an
    // entry, and that is the one replaced, so the entry for the page that
    // failed is left behind it and Back loads that page again. After Update,
    // whose page is the one kept, the entry a jump within the page leaves
    // behind is the kept page's own, and Back stays on it. The error is a
    // notice above the page, after anything said since the redraw began, the
    // post's own answer say. The notice is an alert, so it is heard, and the
    // title stays the page's.
    if (keepPage && draft.held.isConnected) {
      renderedPath = drawnFor.path;
      window.history.replaceState(window.history.state, "", drawnFor.path + window.location.hash);
      state.route = drawnFor.route;
      state.thread = drawnFor.thread;
      const said = el.notice.hidden ? "" : el.notice.querySelector(".notice-text").textContent;
      showNotice(said ? `${said} ${message}` : message);
      return;
    }
    expandedLink = null;
    el.main.innerHTML = emptyStateHTML(message, board);
    announce(notFound ? "Not found" : "Could not load this page");
    return;
  } finally {
    finish();
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
  // on it yet.
  if (overtaken(route)) {
    return;
  }
  drawnFor = { path: renderedPath, route, thread: state.thread };
  expandedLink = null;
  enhancePosts(el.main);
  restoreDraft(draft);
  focusHash();
}

// focusHash highlights the post referenced by the URL fragment, which is where
// quote links point.
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
// then (emptied after a success, released after a refusal). It is carried
// with browserForgets as it was, so that a forget the form misses while the
// redraw has it off the screen reaches it as it goes back. A draft is keyed by
// the page on screen, which the render that drew it noted (drawnFor), not by
// the newest render begun: one begun before the last has drawn, the second of a
// double click say, would carry this page's form, held or not, into the form of
// the page it goes to.
function captureDraft() {
  const key = routeKey(drawnFor.route || state.route);
  const form = el.main.querySelector("form[data-kind]");
  if (!form) {
    // A reply the thread closed on is carried as far as its text goes.
    const unsent = el.main.querySelector("textarea[data-unsent]");
    return unsent ? { key, open: true, body: unsent.value } : null;
  }
  if (heldForms.has(form)) {
    return { key, held: form, forgets: browserForgets };
  }
  return { key, ...formDraft(form) };
}

// formDraft reads what a posting form holds, noting browserForgets as it was,
// so that restoreDraft can tell a Name read before a forget. A ticked badge is
// read as the badge it was ticked for, the one the box was drawn for
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
  const ticked = badgeWithdrawn.get(form) || (badge && badge.checked ? { token: badgeOffered.get(form) } : null);
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
  if (!draft || draft.key !== routeKey(state.route)) {
    return;
  }
  const form = el.main.querySelector("form[data-kind]");
  // A held form whose post was answered while its page was drawn again, its
  // notice on screen (below).
  let answered = null;
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
      // going out under, stays. A badge other than the one its box was drawn
      // for (badgeOffered) takes the option out, as another tab's change does,
      // noting a ticked box, but starts no puzzle, since drawing a page never
      // does. That costs one number on the draft and one per form forgotten,
      // the badge being kept with the form already. Keeping the forms off the
      // screen where every forget and change of badge could reach them would
      // cost more: the list would have to keep a form answered while away,
      // too, until its page is drawn (below).
      const pageBadge = state.verification.badge;
      const drawnWith = badgeOffered.get(held);
      if (forgets !== browserForgets) {
        forgetForm(held);
      } else if (field(held, "badge") && !(pageBadge && drawnWith === pageBadge.token)) {
        withdrawBadgeOption(held, { token: drawnWith }, true);
      }
      form.replaceWith(held);
      const details = held.closest("details");
      if (details) details.open = true;
      return;
    }
    // Otherwise it is read for its values, and as of the newest forget it has
    // had: one answered while away missed any forget since, so its Name, the
    // one forgotten, is not restored, nor its badge (below), while a Name
    // typed after a forget it had on screen is. A ticked badge refused as
    // expired or revoked while it was away is read from the note the refusal
    // left (submit), so the new form is noted as for any badge gone (below).
    answered = heldForms.has(held) ? null : held;
    draft = { key: draft.key, ...formDraft(held), forgets };
  }
  if (!form) {
    keepUnsentDraft(draft);
    return;
  }
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
  // A badge ticked in the draft is ticked again only if the new form offers
  // the same one. One the new form does not offer, gone from the browser
  // meanwhile, is noted as one taken out of the form would be, and so is one
  // whose new form offers another, verified in another tab meanwhile: ticked
  // here, that one would go out unasked for. A draft read before a forget
  // brings neither back (forgetOnThisPage).
  const badge = field(form, "badge");
  const ticked = forgotten ? null : draft.badge;
  const offered = state.verification.badge;
  const same = Boolean(ticked && badge && offered && ticked.token === offered.token);
  if (badge) {
    badge.checked = same;
  }
  if (ticked && !same) {
    badgeWithdrawn.set(form, { token: ticked.token, key: ticked.key ?? (storageGet(POSTER_KEY_STORAGE_KEY) || "") });
  }

  // A form with no image field, on a site that takes no images, gets no file
  // back; a switched-off field refuses it in setFile and says why. A held form
  // answered while its page was drawn again, refused say, has its notice on
  // screen already, and an image the new page takes none of is taken off as
  // release takes it off when the redraw comes first, the notice saying so
  // rather than giving way to the picker's reason: the refusal's own notice,
  // still on screen, is worded again with that reason (refusalNotices), and
  // any other has it added (imageTakenOff). So whichever comes first, the
  // answer or the redraw, the notice ends the same.
  const fileField = form.querySelector("[data-file-field]");
  const refusal = answered && draft.file ? pickerRefusal(form) : "";
  if (refusal) {
    const said = el.notice.hidden ? "" : el.notice.querySelector(".notice-text").textContent;
    const notice = refusalNotices.get(answered);
    const kept = { hasText: Boolean(draft.body.trim()), isThread: form.dataset.kind === "thread" };
    showNotice(notice && said === notice("") ? notice(refusal) : said ? imageTakenOff(said, refusal, kept) : refusal);
  } else if (fileField && draft.file && setFile(fileField, draft.file)) {
    // Only alongside the file it was ticked for.
    const spoiler = field(form, "spoiler");
    if (spoiler) spoiler.checked = draft.spoiler;
  }
  const details = form.closest("details");
  if (details && (draft.open || draft.body || draft.file)) {
    details.open = true;
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

  // Forgetting a badge is entirely local: the server was never told which
  // browser held it, so there is nothing there to tell. The browser's other
  // tabs see it go from storage and stop offering it (followCredentialStorage).
  // Either forget draws /verify again only while it is the page being drawn:
  // pressed on a page the reader has just left, it leaves the screen to the
  // page they went to. Forget this credential draws it only if the panel it
  // was pressed on is still on screen once the page has asked the server
  // (renderVerify): an address step the reader moved to meanwhile, by Verify
  // another address, already shows no badge, and is left as it is. Forget
  // this browser draws whatever /verify then shows, an address step being
  // filled in included, as it does when heard from another tab
  // (forgottenElsewhere), so that its notice is said.
  const forget = event.target.closest('[data-action="forget-verification"]');
  if (forget) {
    event.preventDefault();
    forgetCredential();
    verifyStep = { step: "email", label: "" };
    if (state.route.view === "verify") {
      renderVerify(state.route, forget);
    }
    return;
  }

  if (event.target.closest('[data-action="forget-browser"]')) {
    event.preventDefault();
    forgetBrowser();
    if (state.route.view === "verify") {
      renderVerify(state.route).then((drawn) => {
        if (drawn) {
          showNotice(BROWSER_FORGOTTEN);
        }
      });
    }
    return;
  }

  // Verifying again from here means starting at the address field, with the
  // badge already held staying valid until the new one replaces it.
  const restart = event.target.closest('[data-action="verify-again"], [data-action="verify-restart"]');
  if (restart) {
    event.preventDefault();
    // replacing shows the address form over a badge that is still held and
    // still usable — it is retired by the new one, not by asking for one.
    verifyStep = { step: "email", label: "", replacing: true };
    el.main.innerHTML = verifyViewHTML();
    const input = document.getElementById("f-email");
    if (input) input.focus();
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

// What the page on screen was drawn for, noted by the render that drew it: its
// address, less the fragment, its route, and the thread its controls read. A
// redraw that keeps the page for a post on its way, and then will not load,
// puts them back, the address in the address bar too (render).
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
  clearNotice();
  // Only a file the form took starts the puzzle: a held form's own image is
  // its pending post's, which already has its answer or is waiting on the
  // puzzle being solved for it.
  if (setFile(field, event.dataTransfer.files[0])) {
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
// or dropped on it (not one a re-render puts back); unticking the badge on a
// form whose tick stopped a puzzle, or that already has something written or
// attached, and the page taking a ticked badge out of a form with something
// written or attached, the badge having gone from the browser or given way to
// another, in another tab or in this one, by anything but Forget this browser
// or /verify being drawn over the form, or been refused as expired or revoked,
// and the press then refused once to say so, of a form with something written
// or attached; a refusal that spent the answer, while the form is on screen;
// and a press with no good answer held and none being solved. By the time a
// post is written its puzzle is usually solved. The answer is kept in memory
// only, never in storage or a draft, and is good for one request.
const CAPTCHA_WORKER_SRC = "/lib/altcha/pbkdf2.js";
// The server sets this header, to "spent", on any response to a request whose
// answer it has used up, a refusal included (captchaSpentHeader in captcha.go).
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

// keepCaptcha puts back an answer a request took and did not spend, or that
// brought no response at all, which may have spent it: it was not sent, the
// server refused the request without using it, or nothing came back. It goes
// back only while the page holds no other answer, and only while it is still
// good.
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
// for a server that could not make one.
async function solveCaptcha(signal) {
  if (!window.isSecureContext) {
    throw new Error(CAPTCHA_INSECURE);
  }
  if (typeof Worker !== "function" || !(window.crypto && crypto.subtle)) {
    throw new Error(CAPTCHA_FAILED);
  }
  let challenge;
  try {
    challenge = await api("/api/captcha", { signal });
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
// server would let only one of the two use. It is put back (keepCaptcha) only
// when this request left it unspent, or brought no response, as below.
// A refusal of the answer itself, for being missing, out of date, used or
// wrong, comes before anything is stored or sent, so the answer is dropped and
// the request sent once more with a fresh one, and the reader presses the
// button once. (A used answer was spent by an earlier request, whose cooldowns
// can refuse the resend; that refusal keeps the fresh answer, as below.) Any
// other refusal says whether the server spent the answer before refusing, with
// the Educhan-Captcha: spent header. A refusal with the header drops the
// answer and starts the next puzzle at once, so it is ready, or nearly, when
// the reader presses again: an image the server could not read once the
// answer was spent, say, or a cooldown that another request from this address
// started between flood control's check before the spend and its check after
// it, a 429 like the one for a cooldown found before the spend. A refusal
// without the header, of what was written or for a cooldown found before the
// spend, leaves the answer held: it is put back for the next press, which then
// goes straight out. With no answer from the server at all the answer is put
// back too; if it had been spent, the next request is refused as "used" and
// solves a fresh one here.
// solving(true) and solving(false) bracket the time spent on the puzzle.
// onScreen() says whether the form the request came from is still on screen
// (formOnScreen), and the browser not forgotten since the press. A request
// whose form has gone, because the reader went to another page, or that
// Forget this browser cancelled meanwhile, is not sent, the first time or once
// more after a refused answer. That is asked when its answer is ready, which
// then goes back for the next press, and again when a refusal of the answer
// arrives, perhaps after a forget heard while the first send was on its way.
// Either way sendWithCaptcha throws the error notSent makes, for a caller
// that says nothing on a page the reader went to and, on a form still on
// screen, which only the forget stops, says why. Neither the second send nor
// the next puzzle is started for one.
// Pressing Update while a post waits does not take its form away: the redrawn
// page shows the same form, still held, as does another page of the same
// board's index, drawn with the same new-thread form, and a redraw that will
// not load leaves the page on screen, form and all.
async function sendWithCaptcha(needsIt, send, solving, onScreen) {
  for (let retried = false; ; retried = true) {
    let taken = null;
    if (needsIt()) {
      solving(true);
      captcha.waiting += 1;
      try {
        // Taken as soon as it is handed over, in the same step. An answer
        // another request waiting on the same puzzle took first is not shared:
        // this one solves another.
        do {
          taken = takeCaptcha(await captchaAnswer());
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
    if (!onScreen()) {
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
// page is drawn over it. A redraw of the same page, Update say, or of another
// page of the same board's index (the same routeKey), keeps a posting form
// whose post is on its way in the document: it puts the form in place of the
// one the new page draws for the same thread or board (restoreDraft) or, when
// the new page will not load, leaves the page on screen, form and all
// (render). Either way the post goes ahead and is answered on that form. Only
// a new page that draws no such form, the thread locked or archived meanwhile,
// takes it off the screen. No redraw carries a /verify form, so one is on
// screen only until the next render, of /verify too: a code requested from the
// form a redraw is about to replace would be answered on a page that shows the
// address step, with nothing to say a code was sent.
function formOnScreen(form, route) {
  return form.isConnected && (form.dataset.kind ? routeKey(state.route) === routeKey(route) : state.route === route);
}

// The forms holdForm holds.
const heldForms = new WeakSet();

// How the last redraw of a held form's page drew the picker there, by form:
// the reason it was drawn switched off with, or "" for one drawn live, or the
// site's reason where it drew none, on a site that takes no images
// (restoreDraft). The held picker shows the image of the post on its way until
// the hold ends, and goes as the redraw drew it then (holdForm).
const redrawnPickers = new WeakMap();

// How each posting form's last refusal was worded, by form, as a function of
// why release took its image off ("" for not at all): a refusal answered while
// a redraw of the page loads, which then draws the picker switched off, is
// worded again with the picker's reason (restoreDraft), so it reads as it
// would had the redraw come first. The two refusals of the image itself take
// the image off themselves, and keep nothing here.
const refusalNotices = new WeakMap();

// holdForm switches off a form's controls while its request is pending, from
// the press until the answer, with whatever the reader wrote left where it is.
// What is sent was read from the form at the press, so a change made in the
// meantime would be left out of it, and wiped by the reset after a success. It
// returns release, which switches back on the controls it switched off, and no
// others: a picker switched off for its own reason stays off. When the hold
// ends, success or refusal, release leaves the picker as a redraw of the page
// meanwhile drew it (redrawnPickers): switched off with that drawing's reason,
// or on, or off with the site's reason where the redraw drew none. A picker it
// switches off loses the image it held, and release then returns the reason,
// for the notice to say why the image went (imageTakenOff), and "" otherwise. A
// refusal of the image itself, the image limit reached or uploads switched off,
// then switches the picker off in the submit handler. With refocus, a form that
// had the focus at the press, which disabling the focused control took away,
// gets it back on its button, ready to press again; put back in a box to write
// in, it would start another puzzle. That is only while the focus is still
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
  heldForms.add(form);
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

el.main.addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = event.target;
  if (form.dataset.verify) {
    await submitVerification(form);
    return;
  }
  const button = form.querySelector('button[type="submit"]');
  const data = new FormData(form);
  const name = String(data.get("name") || "");
  const body = String(data.get("body") || "");
  const image = field(form, "image");
  const file = image && !image.disabled && image.files[0];

  if (!body.trim() && !file) {
    // An image is offered as the alternative only where the form can take one.
    showNotice(image && !image.disabled ? "Write something or attach an image." : "Write something to post.");
    (field(form, "body") || form).focus();
    return;
  }

  // A box ticked for a badge the page no longer holds is a badge taken out
  // from under the form, now: the credential goes only as the badge the box
  // was drawn for (badgeOffered), so one stored since, verified in this tab or
  // another, never goes out under a box that named another, and none goes once
  // the page holds none.
  const badgeBox = field(form, "badge");
  const offered = state.verification.badge;
  if (badgeBox && badgeBox.checked && !(offered && badgeOffered.get(form) === offered.token)) {
    withdrawBadgeOption(form, { token: badgeOffered.get(form) }, false);
  }

  // A form whose ticked badge was taken out from under it says so before its
  // post goes without one, once; the press after it posts (badgeWithdrawn).
  // That post needs the puzzle the badge would have skipped, which starts now
  // if it has not: the page taking the badge out starts it only for a form on
  // screen as it goes (withdrawBadgeOption), and a redraw that carries the
  // note (restoreDraft), or /verify drawn over the form (loadVerification),
  // starts none, since drawing a page never does.
  if (badgeWithdrawn.has(form)) {
    const withdrawn = badgeWithdrawn.get(form);
    badgeWithdrawn.delete(form);
    showNotice(badgeWithdrawnNotice(form, withdrawn));
    if (captchaFormNeedsIt(form) && captchaFormHasWriting(form)) {
      prepareCaptcha();
    }
    return;
  }

  // The one request in the application that carries the credential. It goes
  // out only because this box is ticked, and only as the badge it was drawn
  // for (above), so an anonymous post is anonymous at the request layer too,
  // not merely in what it displays.
  const wantsBadge = data.get("badge") === "on";
  const held = wantsBadge ? state.verification.badge : null;
  // A badged post carries the credential and not the poster key. The server
  // derives a badged post's ID from the credential, so the key would do
  // nothing there, and sending the two together would put this browser's key
  // beside its credential in one request — the only place they could meet.
  const headers = held ? { [CREDENTIAL_HEADER]: held.token } : { [POSTER_KEY_HEADER]: posterKey() };

  const isThread = form.dataset.kind === "thread";
  const payload = { name, body, badge: wantsBadge };
  let path;
  if (isThread) {
    payload.subject = String(data.get("subject") || "");
    path = `/api/boards/${encodeURIComponent(state.route.slug)}/threads`;
  } else {
    payload.sage = data.get("sage") === "on";
    path = `/api/boards/${encodeURIComponent(state.route.slug)}/threads/${form.dataset.thread}/posts`;
  }

  const label = button.textContent;
  const sending = file ? "Uploading…" : "Posting…";
  const route = state.route;
  const onScreen = () => formOnScreen(form, route);
  // "Forget this browser", pressed in another tab while the post waits on its
  // puzzle, stops it as it stops a code request: sent, it would go under the
  // Name and the poster key the forget took away, while its form shows
  // neither. A post that went before the forget is answered on its form, and
  // keeps nothing (below); one refused for its answer is not sent again.
  const forgets = browserForgets;
  const forgotten = () => forgets !== browserForgets;
  // Read above, and held from here until the post has gone or been refused, so
  // what the form shows meanwhile is what is sent.
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
    // option goes with it, and the form's note on the anti-spam check says so
    // (dropBadgeOption) — taking the checkbox out of this form rather than
    // re-rendering leaves the half-written post exactly where it is — so the
    // retry that follows is an ordinary post instead of the same refusal.
    // Why it went is kept for /verify to say, unless the browser was forgotten
    // meanwhile: the badge went with it, a badge stored since is someone
    // else's, and the note would tell the next person there had been one.
    // What was refused is the credential this post sent (held), and only that
    // one goes: storage may hold another by now, verified in another tab while
    // the post was on its way, or none, forgotten there, and this tab follows
    // that from storage as for any change there (followCredentialStorage). The
    // form offered the one refused, so its option goes either way. Its box was
    // ticked, so the post it goes on to is one the box no longer skips the
    // puzzle for: that starts now, as for the reader's own untick
    // (captchaBadgeToggled), on a form still on screen with some of the post in
    // it. A form still on screen is told by the refusal's notice (below). One
    // off the screen, while a post link's lookup loads say, is told nothing
    // here, so it is noted as for any ticked badge that goes from under a form
    // (withdrawBadgeOption, starting nothing): the redraw that draws its page
    // again reads the note with its values (formDraft) and notes the form it
    // draws (restoreDraft), whose next press is refused once, rather than
    // posting without the badge unsaid. When storage holds another badge, which
    // the page keeps, and the browser was not forgotten meanwhile, a form on
    // screen is told so in its own words (badgeReplacedNotice), not the
    // server's, and that notice is its one refusal for the badge: a note left
    // on it while the post was on its way, by another tab's change or a redraw
    // that put it back (followCredentialStorage, restoreDraft), goes, so the
    // next press posts without a badge, as the notice says.
    const reason = err.payload && err.payload.credential;
    let replaced = false;
    if (held && (reason === "expired" || reason === "revoked")) {
      if (forgets === browserForgets && storedCredential() === held.token) {
        const pageBadge = state.verification.badge;
        forgetCredential();
        badgeLost =
          reason === "expired"
            ? "Your badge has expired."
            : "That badge was replaced by a newer verification.";
        // A posting form on the page the reader went to while this was on its
        // way was drawn for the badge that has just gone: it loses the option
        // as for a badge forgotten in another tab (followCredentialStorage).
        if (pageBadge) {
          for (const posting of el.main.querySelectorAll("form[data-kind]")) {
            if (posting !== form) {
              withdrawBadgeOption(posting, pageBadge, false);
            }
          }
        }
        // /verify, the page the reader went to, showing the panel for that
        // badge, is drawn again, as for a badge forgotten in another tab
        // (followCredentialStorage), and says why it went.
        const panel = el.main.querySelector('[data-action="forget-verification"]');
        if (state.route.view === "verify" && ageStatus() === "ok" && panel) {
          renderVerify(state.route, panel);
        }
      }
      const kept = storedCredential();
      replaced = forgets === browserForgets && kept !== "" && kept !== held.token;
      if (shown) {
        dropBadgeOption(form);
        if (replaced) {
          badgeWithdrawn.delete(form);
        }
        if (captchaFormNeedsIt(form) && captchaFormHasWriting(form)) {
          prepareCaptcha();
        }
      } else {
        withdrawBadgeOption(form, held, true);
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
    // A form that has left the screen, with or without its post sent, is said
    // nothing about on the page the reader went to, and that page is changed
    // for it only as above, for the badge the refusal forgot: its posting
    // forms lose the option, and /verify showing that badge's panel is drawn
    // again, saying why the badge went. What the refusal said about the badge
    // and the server is kept, above, and so is a note on the form, for the
    // redraw that reads it.
    if (!shown) {
      return;
    }
    // What a notice says the form still holds is what it holds now, after
    // release: its text, read from the box, and its image unless release took
    // that off, which the notice then says, with why (imageTakenOff). The two
    // refusals of the image itself say so in their own words.
    const box = field(form, "body");
    const hasText = Boolean(box && box.value.trim());
    // A post sendWithCaptcha did not send from a form still on screen was
    // stopped by the forget: before its first send, or before the second that
    // a refused answer calls for, the forget heard while the first was on its
    // way or while the second puzzle was solving. It says so rather than
    // wording a refusal, and keeps its text for whoever presses again.
    if (err.notSent) {
      showNotice(imageTakenOff(FORGOTTEN_BEFORE_SENDING, imageOff, { hasText, isThread }));
      return;
    }
    // The cap was reached after this form was drawn: take the image off and
    // switch the picker off, so the next press posts the text that is left.
    const limitReached = err.status === 403 && err.message === IMAGE_LIMIT_REACHED;
    if (limitReached) {
      const fileField = form.querySelector("[data-file-field]");
      const thread = state.thread;
      if (fileField) {
        blockFileField(fileField, imageLimitReason(thread && thread.board));
      }
      // The summary line was drawn before the thread filled up; it says so
      // now, as a fresh render of the thread would.
      if (thread) {
        thread.at_image_limit = true;
        if (thread.board && thread.board.image_limit > 0) {
          thread.images = Math.max(Number(thread.images) || 0, thread.board.image_limit);
        }
        redrawThreadCounts();
      }
    }
    if (uploadsOff) {
      const fileField = form.querySelector("[data-file-field]");
      if (fileField) {
        blockFileField(fileField, UPLOADS_OFF_REASON);
      }
      redrawThreadCounts();
    }
    if (limitReached || uploadsOff) {
      showNotice(errorMessage(err, { hasText, imageOff }));
      return;
    }
    // A thread archived, locked or gone (or, for a new thread, its board gone)
    // takes no post at all, whatever is left of this one.
    const closed =
      err.status === 404 || (err.status === 403 && (err.message === "thread is archived" || err.message === "thread is locked"));
    // Worded for the image release took off, or for none, and kept with the
    // form: a redraw still loading that then takes the image off words it
    // again, as it would have read had the redraw come first (restoreDraft).
    const said = (off) =>
      replaced ? badgeReplacedNotice(form, { hasText, imageOff: off }) : errorMessage(err, { hasText, imageOff: off });
    const notice = (off) => imageTakenOff(said(off), off, { hasText, isThread, closed });
    refusalNotices.set(form, notice);
    showNotice(notice(imageOff));
  }
});

// submitVerification drives both halves of the verification form. Nothing is
// remembered between them but the badge on offer: the code is confirmed against
// a short-lived cookie the server set, so the address is typed once and never
// travels again.
async function submitVerification(form) {
  const button = form.querySelector('button[type="submit"]');
  const label = button.textContent;
  const data = new FormData(form);
  const requesting = form.dataset.verify === "request";

  // The badge is for students aged 18 and over. Without the box ticked the
  // page refuses on its own and sends nothing, the address included.
  const adult = requesting ? field(form, "adult") : null;
  if (requesting && !(adult && adult.checked)) {
    showNotice(BADGE_AGE_REFUSAL);
    if (adult) adult.focus();
    return;
  }

  const sending = requesting ? "Sending…" : "Verifying…";
  const route = state.route;
  // "Forget this browser", pressed while the request is on its way, cancels
  // it: nothing goes that has not gone yet, and what comes back keeps nothing,
  // neither the badge nor the code step, and is said nowhere.
  const forgets = browserForgets;
  const forgotten = () => forgets !== browserForgets;
  const onScreen = () => formOnScreen(form, route) && !forgotten();
  // Read above, and held from here until the request is answered, so what the
  // form shows meanwhile is what is sent: the address, and the box.
  const release = holdForm(form);
  button.textContent = sending;
  clearNotice();
  try {
    if (requesting) {
      // A code request always needs the captcha when it is on: no badge
      // travels with it to skip one.
      const sent = await sendWithCaptcha(
        () => state.site.captcha,
        (answer) => {
          // adult is true here, because an unticked box stopped above.
          const request = { email: String(data.get("email") || ""), adult: adult.checked };
          if (answer) {
            request.captcha = answer;
          }
          return api("/api/verify/request", { method: "POST", body: JSON.stringify(request) });
        },
        (solving) => {
          button.textContent = solving ? "Checking…" : sending;
        },
        onScreen,
      );
      if (forgotten()) {
        return;
      }
      // The code is on its way, so /verify asks for it next time it is drawn;
      // a reader who left meanwhile is not pulled back to it.
      verifyStep = { step: "code", label: sent.label || "" };
      if (!onScreen()) {
        return;
      }
      el.main.innerHTML = verifyViewHTML();
      const input = document.getElementById("f-code");
      if (input) input.focus();
      return;
    }
    // The one time the credential is ever handed out. It is kept here rather
    // than set as a cookie, so nothing sends it until a post asks for a badge.
    // Until it is answered it is counted on the code step it confirms, so
    // another tab's verification meanwhile leaves the step to this answer
    // (codeSpentElsewhere), even once /verify is drawn again over this form.
    const step = verifyStep;
    step.confirming = (step.confirming || 0) + 1;
    let issued;
    try {
      issued = await api("/api/verify/confirm", {
        method: "POST",
        body: JSON.stringify({ code: String(data.get("code") || "") }),
      });
    } finally {
      step.confirming -= 1;
    }
    if (forgotten()) {
      return;
    }
    const offered = state.verification.badge;
    const stored = storeCredential(issued.credential);
    // Offered from now on, on every posting form drawn, so a reader who has
    // left /verify, or leaves before it is drawn again, has it on the next one.
    // Drawing /verify again withdraws nothing if its GET /api/verify fails
    // (loadVerification). The badge goes only when the server says the feature
    // is off, when it runs out, and when it is refused on a post, forgotten or
    // replaced, in this tab or in another (followCredentialStorage).
    state.verification.badge = readBadge();
    // A posting form on screen, the reader having left /verify while this was
    // on its way, was drawn for the badge this one replaces: it loses the
    // option, which names that badge, as for a badge replaced in another tab
    // (followCredentialStorage), and one that had it ticked says so at its
    // next press (withdrawBadgeOption).
    const held = state.verification.badge;
    if (offered && !(held && held.token === offered.token)) {
      for (const posting of el.main.querySelectorAll("form[data-kind]")) {
        withdrawBadgeOption(posting, offered, false);
      }
    }
    verifyStep = { step: "email", label: "" };
    // Each redraw below draws only if what it is drawn over is still on screen
    // once the page has asked the server (renderVerify): a forget pressed
    // meanwhile may have drawn /verify itself, or the reader moved on to an
    // address step.
    const overtook = el.main.querySelector('form[data-verify="confirm"], [data-action="forget-verification"]');
    if (onScreen()) {
      await renderVerify(route, form);
    } else if (state.route.view === "verify" && ageStatus() === "ok" && overtook) {
      // /verify drawn again while this was on its way, the reader back from
      // another page say, shows what this answer has overtaken: a code step,
      // for this code, now spent, or for one asked for since, whose
      // verification in progress ended when this answer, coming after that
      // request's, cleared the cookie; or the panel, for a badge this one
      // replaces. It is drawn again for the badge just earned, as this form
      // would have been, and what the page said about the step it overtook,
      // a later confirmation of it refused say, goes with the step; what this
      // answer said itself, that storage refused the badge, stays, as it does
      // over this form. The address step, which the reader may be filling in,
      // is left as it is, as is one they move to, or one a forget draws with
      // its notice, while this redraw asks the server.
      renderVerify(state.route, overtook).then((drawn) => {
        if (drawn) {
          clearNotice();
          if (!stored) {
            showNotice(BADGE_NOT_STORED);
          }
        }
      });
    }
  } catch (err) {
    const shown = onScreen();
    release(shown);
    button.textContent = label;
    // A confirmation another tab's verification found on its way: refused,
    // whatever the refusal said, its step goes now as it would have then
    // (codeSpentElsewhere), on screen or not, once no other confirmation of it
    // is on its way. While one is, the step stays for that one's answer, and
    // this form, on screen, says why its code was refused, as the step drawn
    // again would.
    if (verifyStep.spentElsewhere) {
      codeSpentElsewhere();
      if (shown && verifyStep.spentElsewhere) {
        showNotice(CODE_SPENT_ELSEWHERE);
      }
      return;
    }
    // Nothing is said on a page the reader went to instead.
    if (!shown) {
      return;
    }
    // The server's refusal of a request without the attestation reads the same
    // as the page's own.
    const refusedForAge = err.status === 400 && err.message === BADGE_AGE_REFUSED;
    showNotice(refusedForAge ? BADGE_AGE_REFUSAL : errorMessage(err, { action: "code" }));
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
