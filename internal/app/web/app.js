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
  "api", "media", "healthz", "lib", "verify", "pass", "mod", "u", "rules", "faq",
  "legal", "terms", "privacy", "dmca", "report", "archive", "safety",
]);
// The reserved words that have a page of their own so far.
const PAGE_VIEWS = new Set(["verify", "pass", "rules", "faq"]);
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
// The educhan Pass this browser holds, if any, and the header it rides in on an
// unbadged post. See "educhan Pass" below.
const PASS_STORAGE_KEY = "educhan:pass";
const PASS_HEADER = "X-Educhan-Pass";
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
  // all, and the largest it accepts. Until that answers, or if it never does,
  // the picker is offered as it always was and the size limit is unknown (0);
  // the server's refusal is the last word either way. pass is whether it has
  // the Pass, which only decides whether the header links to its page.
  site: { uploads: true, maxUploadBytes: 0, pass: false },
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
  // The header's Pass link, hidden until the server says it has the Pass or
  // this browser holds one (showPassLink).
  passLink: document.querySelector("[data-pass-link]"),
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
  // answered, if given, is called once the answer's headers are in, before its
  // body is read: a checkout is called off only until then
  // (callOffPassCheckouts), since by the time the headers reach the page the
  // browser has kept the cookie they set.
  const { answered, ...rest } = options;
  // The caller's headers are copied rather than used in place: this function
  // adds to them, and a caller passing the same object twice should not
  // accumulate whatever the first call decided.
  const init = { ...rest, headers: { ...(rest.headers || {}) } };
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
  if (answered) {
    answered();
  }
  const payload = res.status === 204 ? null : await res.json().catch(() => null);
  if (!res.ok) {
    const err = new Error((payload && payload.error) || `${res.status} ${res.statusText}`);
    err.status = res.status;
    err.payload = payload;
    throw err;
  }
  return payload;
}

// errorMessage turns an API failure into something worth reading. The server's
// own message is kept; only the cases where it needs context are rewritten.
// What is being sent stays in its form after a refusal, so these say so.
// action is what was refused — "post", or "code" for a verification code
// request — so a cooldown tells the reader to do that again, not something
// they were not doing.
function errorMessage(err, { hasText = true, action = "post" } = {}) {
  if (err.unreachable) {
    return `${UNREACHABLE} What you wrote is still in the box.`;
  }
  if (err.status === 413) {
    return imageTooLargeMessage();
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
  if (err.status === 403 && err.message === "thread is archived") {
    return "This thread was archived while you were writing, so it no longer takes replies. Your text is still in the box.";
  }
  if (err.status === 403) {
    return `${err.message}.`;
  }
  return err.message;
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
          <span class="hint">Posting is anonymous.</span>
        </div>
      </form>
    </details>`;
}

/* File field behaviour ------------------------------------------------------ */

function isAcceptedImage(file) {
  return ACCEPTED_IMAGES.split(",").includes(file.type) || ACCEPTED_EXTENSIONS.test(file.name);
}

// setFile puts a dropped file into the form's file input. Assigning a FileList
// needs a DataTransfer, which is also how the draft is carried across a
// re-render.
function setFile(field, file) {
  const input = field.querySelector(".file-input");
  if (!input || !file) {
    return false;
  }
  // A drop reaches a blocked field too; say why it was not taken.
  if (input.disabled) {
    showNotice(field.querySelector(".file-blocked").textContent);
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
  showPassLink();
}

// showPassLink shows the header's Pass link on a server that has the Pass, and
// in a browser that holds one whatever the server does now, so its holder can
// still show the code or remove it. A server without it, the default, does not
// advertise a paid product it does not sell.
function showPassLink() {
  if (el.passLink) {
    el.passLink.hidden = !(state.site.pass || storedPass());
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

function storeCredential(token) {
  try {
    localStorage.setItem(CREDENTIAL_STORAGE_KEY, token);
  } catch {
    showNotice("This browser will not let the page store your badge, so it cannot be used here.");
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

// forgetBrowser removes everything this browser keeps that ties it to posts or
// to a purchase: the badge, the Pass, the poster key every unbadged post's ID
// is made from (which would otherwise give the next person at a shared
// computer the same ID in the same threads, and show them which posts there
// were yours) and the remembered name, with its tripcode secret, and what the
// Pass page would have said about a Pass that has gone (passLost), which is
// the last holder's news, not the next person's. The age answer stays: it
// says nothing about any post. All of that is local. A Pass bought here and
// not yet collected is not: the claim on it is a cookie only the server can
// drop, so it is asked to (forgetPassClaim), and what that comes to is
// returned. Once that has called off this tab's checkouts, every checkout's
// mark goes too (PASS_CHECKOUT_STORAGE_KEY): one left by a tab that crashed or
// was killed before its answer came says when a checkout was started here. A
// tab whose checkout is still on its way puts its own back
// (keepPassCheckoutMark). It is called inside the click on Forget this
// browser, which lets it empty the clipboard after this tab copied a Pass code.
function forgetBrowser() {
  forgetCredential();
  forgetPass();
  passJustIssued = null;
  passLost = "";
  forgetTypedCode();
  emptyCopiedPassCode();
  for (const key of [POSTER_KEY_STORAGE_KEY, NAME_STORAGE_KEY]) {
    try {
      localStorage.removeItem(key);
    } catch {
      // Nothing was stored, so nothing needs removing.
    }
  }
  pagePosterKey = "";
  const dropped = forgetPassClaim("browser");
  try {
    const marks = [];
    for (let at = 0; at < localStorage.length; at += 1) {
      const key = localStorage.key(at);
      if (key && key.startsWith(`${PASS_CHECKOUT_STORAGE_KEY}:`)) {
        marks.push(key);
      }
    }
    for (const key of marks) {
      localStorage.removeItem(key);
    }
  } catch {
    // Storage that cannot be read holds no mark.
  }
  return dropped;
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
      <p class="muted">Forgetting this browser removes the key, the name, any badge and any
        Pass. Your next post starts a new key, and with it new IDs. Posts you have made stay
        as they are. Other educhan tabs open in this browser let go of the Pass too, but can
        keep the key, the name and any badge they have, and a post from one of them puts the
        key and the name back, so close them first. To use the Pass again afterwards, show its
        code on the <a href="/pass">Pass</a> page and write it down first. It also stops this
        browser collecting a Pass bought here that hasn't arrived yet, even one whose checkout
        was still opening; if you've paid for one, write to the support address on the receipt
        Stripe emailed you, and you'll be sent a reissue code to collect it with. If you used
        Copy the code on the Pass page since you last opened educhan in this tab, and have
        copied nothing in this tab since, it empties the clipboard too, whatever is on it by
        then, unless another tab removed or replaced the Pass meanwhile. It does so even after
        a copy in this tab that educhan can't see took anything, such as one from an email box
        in some browsers. A Pass code copied any other way, by hand included, stays there
        until something else is copied.</p>
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
  let enabled = false;
  try {
    // The only thing worth asking the server: whether the feature exists here.
    // What this browser holds is a local question with a local answer.
    enabled = !!(await api("/api/verify")).enabled;
  } catch {
    // Verification is an extra; the board works without knowing about it.
    enabled = false;
  }
  state.verification = { enabled, badge: enabled ? readBadge() : null };
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

  if (v.badge && !verifyStep.replacing) {
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

async function renderVerify() {
  await loadVerification();
  el.main.innerHTML = verifyViewHTML();
  // Explained once, on the page that can do something about it.
  badgeLost = "";
  announce("Institutional verification");
}

/* educhan Pass ------------------------------------------------------------- */

// The Pass is kept in this browser under PASS_STORAGE_KEY as { code, expires }:
// the code as the server wrote it, and its expiry as an ISO date. A Pass a
// post's answer said had run out, when this browser had a later date for it,
// is kept as { code, expired: true }, with no date rather than an invented one.
// The code leaves the browser on an unbadged post, in submit, and in the body
// of the requests that use it on purpose: checking a code on the Pass page
// (one typed in, or the one held, checked again) and renewing it. "Show the
// code" puts it on the screen and "Copy the code" on the clipboard, which
// Remove the Pass and Forget this browser empty again (emptyCopiedPassCode).
// Nothing else reads it.

// The code as the server writes it: four groups of five from Crockford's
// base32. What is typed in is checked by the server, which also reads it with
// the dashes left out, in lower case, or with O for 0 and I or L for 1.
const PASS_CODE_PATTERN = /^[0-9A-HJKMNP-TV-Z]{5}(-[0-9A-HJKMNP-TV-Z]{5}){3}$/;
const PASS_CODE_LENGTH = 20;
// While this browser has a purchase to collect (GET /api/pass says "claim"),
// the page asks for it on every visit and keeps asking while it is open: soon
// at first, since Stripe usually confirms a card within seconds, then less and
// less often, as a bank payment can take days.
const PASS_CLAIM_FIRST_WAIT_MS = 2000;
const PASS_CLAIM_LONGEST_WAIT_MS = 60000;

// A purchase is collected with a nonce the page draws: the code a claim hands
// over is made from it and the claim cookie (passClaimCode in pass.go), so a
// claim whose answer never arrived, asked again with the same nonce, is
// answered with the same code. The nonce is sent with every claim until an
// answer says nothing more is waiting under the claim, and is then dropped,
// and so is any nonce of a claim this browser has stopped collecting. A
// reissue code is spent with a nonce of its own, and the Pass code it gives
// is made from the reissue code as well. That nonce is kept until an answer
// says a reissue code issued or renewed a Pass, or Remove the Pass or Forget
// this browser drops it: never on a refusal, so the right code typed after a
// mistyped one still asks with the nonce of a try whose answer was lost.
const PASS_NONCE_STORAGE_KEY = "educhan:pass-nonce";
const PASS_REISSUE_NONCE_STORAGE_KEY = "educhan:pass-reissue-nonce";
// Beside it, a mark that a try with that nonce may have gone unanswered: put
// there before each try, and taken off by the answer to a try that found none
// there, so it stays after an answer lost to a dropped connection, a failure
// or a closed page, and through any refusal after that, until it goes with the
// nonce. A refusal while it is there may follow a try that spent the code,
// which the page says (redeemPassReissue); two refusals with no answer lost
// are only refusals. It is kept as a nonce is, a random value of its own that
// is never sent.
const PASS_REISSUE_UNANSWERED_STORAGE_KEY = "educhan:pass-reissue-unanswered";
const PASS_NONCE_PATTERN = /^[A-Za-z0-9_-]{22}$/;
// While Remove the Pass, Forget this browser or Stop collecting it here waits
// for the server to drop the claim, the tab it was pressed in keeps a mark of
// its own under this key, a colon and a random token, so the browser's other
// tabs stop as that one does (followPassForget), and a tab opened meanwhile
// waits as well (heedPassForgetsUnderWay). Its value names the forget pressed,
// "remove:" or "browser:" for the first two, which empty the code box too and
// drop a reissue code's nonce and its answer, or "stop:" for the third, which
// leaves a reissue code alone, and holds the time it was set after that. The
// name lets a tab that could not drop a checkout's claim say what to press once
// back online (forgetPassOrder). Marks never take each other's place, so a tab
// waits until every one it knows of has gone, each for at most
// PASS_FORGET_WAIT_MS from when it was set, in case the tab that set it closes
// before its request is answered and never takes it off; one found older than
// that is taken off.
const PASS_FORGET_STORAGE_KEY = "educhan:pass-forget";
const PASS_FORGET_WAIT_MS = 30000;
// A checkout's answer puts its claim cookie in the place of the one before,
// whichever checkout started first, so checkouts go one at a time: while one's
// request is on its way, the tab it was pressed in keeps a mark of its own
// under this key, a colon and a random token, holding "checkout:" and the time
// it was set (openPassCheckout), and a checkout waits to go until no other's
// mark is left, in any tab (passCheckoutLeft). The one that goes last then
// holds the cookie, so one pressed after a forget is never overtaken by one the
// forget overtook, which the forget calls off or drops (submitPass). A mark is
// waited on for at most PASS_FORGET_WAIT_MS from when it was set, in case its
// tab closed before taking it off, and one found older is taken off, when a
// checkout waits and when a page is loaded. Forget this browser takes every one
// off (forgetBrowser), and a tab whose checkout is still on its way puts its
// own back (keepPassCheckoutMark). A waiting checkout looks again every
// PASS_CHECKOUT_LOOK_MS.
const PASS_CHECKOUT_STORAGE_KEY = "educhan:pass-checkout";
const PASS_CHECKOUT_LOOK_MS = 100;
// A reissue code is what the owner sends a buyer who lost their code: the word
// REISSUE and 28 characters of a Pass code's alphabet, in groups of four. No
// Pass code has a U, so the code box tells the two apart by the word, or,
// typed without it, by the length. The server reads it in either case, with or
// without the word and the dashes. It is typed in, never put in an address,
// and never stored.
const REISSUE_CODE_WORD = "REISSUE";
const REISSUE_CODE_LENGTH = 28;
// What may stand between a code's groups, which the page skips: exactly what
// the server skips (codeSeparators in pass.go), the hyphen and the dashes it
// may become, every Unicode space and line break, and the invisible characters
// a paste can carry. TestPageAndServerSkipTheSameSeparators reads both.
const CODE_SEPARATORS =
  "-\u2010\u2011\u2012\u2013\u2014\u2015\u2212" +
  " \t\n\v\f\r\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006" +
  "\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000" +
  "\u00ad\u200b\u2060\ufeff";
// The letters a code's alphabet leaves out that the server reads as digits, in
// capitals as codeCharacters puts them: O as 0, and I or L as 1 (parseCode in
// pass.go). The page sends a code with them written as those digits, which is
// how the server writes it. TestPageAndServerFoldTheSameLetters reads both.
const CODE_FOLDS = { O: "0", I: "1", L: "1" };
// Written and read back before a checkout, to learn whether this browser keeps
// anything at all, and removed straight away.
const STORAGE_PROBE_KEY = "educhan:storage-probe";

// The server's words for the Pass's 18+ refusal, recognised so the page can
// say it in its own.
const PASS_AGE_REFUSED = "the Pass is sold only to people aged 18 and over; you can still read and post without one";
const PASS_AGE_REFUSAL =
  "The Pass is sold only to people aged 18 and over. You can still read and post without one.";
// The server's words for a checkout while sales are paused, and the page's,
// which also stand where Buy and Renew would be.
const PASS_NOT_ON_SALE = "the Pass is not on sale right now";
const PASS_PAUSED = "The Pass is not on sale right now.";
// The server's words for a reissue code it will not take again, recognised so
// the page can say what that means for the reader: mistyped, or asked for
// afresh, or, when an earlier try's answer may have been lost, perhaps used
// for them already.
const PASS_REISSUE_SPENT = "this reissue code has been used, has run out or was cancelled";
// Said instead of sending what was asked for while it waited on a forget, or
// a checkout on another checkout, once Remove the Pass or Forget this browser
// has been pressed since (passForgetsSettled): whoever pressed it meant this
// browser to keep no Pass. The second is said once only Stop collecting it here
// has been, which can come meanwhile only while a checkout waits on another, or
// from a tab that has waited out PASS_FORGET_WAIT_MS.
const PASS_FORGOTTEN_MEANWHILE = "Nothing was sent: the Pass was removed, or this browser forgotten, before it could go.";
const PASS_STOPPED_MEANWHILE = "Nothing was sent: this browser stopped collecting a Pass before it could go.";
// What passForgetsSettled comes to for a press whose document was left
// (pagehide) while it waited: nothing is sent and nothing said, since the
// reader has gone. A page brought back from the back-forward cache is drawn
// afresh (the pageshow listener), with the button ready.
const PASS_LEFT_MEANWHILE = "left";
// Said when the browser will not keep a Pass: its storage for the site is
// blocked or full, so the Pass cannot ride on the next post.
const PASS_NOT_KEPT =
  "This browser couldn't keep the Pass, because it isn't letting educhan store anything, so it can't send it with your posts. Allow this site to store data, then enter the code again.";
// Said instead of sending a buyer to Stripe from a browser that would keep
// neither the claim that collects a purchase nor the Pass it brings.
const PASS_NO_STORAGE =
  "This browser isn't letting educhan keep anything, so it couldn't collect or keep a Pass. Allow this site to store data first.";
// Why a Pass the server no longer knows may have gone, likeliest first: one
// that ran out is deleted after its month of grace.
const PASS_GONE_REASONS = "It may have run out more than a month ago, or been refunded or replaced.";

// Why the Pass that was here has gone, said on the Pass page until the reader
// leaves it.
let passLost = "";

// passVisit counts the times the Pass page has been drawn from the server's
// answer, so that a collection started for one drawing stops once another has
// replaced it, or once this browser stops collecting (forgetPassClaim).
let passVisit = 0;

// passAsked is set by Check now for the drawing it asks for, which takes it
// (renderPass), so that what that drawing's first claim comes to, a renewal
// or the Pass held checked after a lost answer, is said in view as the press's
// answer (showClaimed, showPassNotice). What a later claim brings is the
// page's own news, said where showNotice puts it.
let passAsked = false;

// passForgets counts the times this browser has stopped collecting
// (forgetPassClaim), here or in another tab (followPassForget). A claim whose
// answer arrives after that is dropped without being stored or drawn: whoever
// pressed it meant this browser to keep nothing more of what was bought here.
// A Pass that answer brought is still its buyer's, and a reissue code from
// its payment gives it a new code.
let passForgets = 0;

// passLeft counts the times this document has been left (pagehide), for
// another site, say, or for Stripe: a press still waiting to go then
// (passForgetsSettled) sends nothing.
let passLeft = 0;

// passCodeForgets counts the forgets that take the Pass with them, Remove the
// Pass and Forget this browser, here or in another tab (a "remove:" or
// "browser:" mark), and passCodeForgetting settles once all of them this tab
// knows to be on their way have, or is null while none is. An answer about a
// Pass, to a code checked or a reissue code spent, whose request went before
// one of them is dropped unstored, and so is one that arrives while one is on
// its way (passForgotten). Stop collecting it here is not one of them: it drops
// only what is being collected, and leaves a reissue code, its nonce and its
// answer alone.
let passCodeForgets = 0;
let passCodeForgetting = null;

// passForgetting settles once every request to drop this browser's claim that
// this tab knows to be on its way has: its own (forgetPassClaim), and another
// tab's, by that tab's mark going (followPassForget, heedPassForgetsUnderWay).
// It is null while there is none. Until the server has them, the claim is
// still this browser's, and GET /api/pass still says there is something to
// collect, so nothing starts collecting meanwhile: the Pass page waits for
// them before asking (renderPass), and so does a collection before each claim
// (collectPass). Waiting on every one, a checkout started once Remove's was
// answered cannot have its claim dropped by a Stop pressed before it and
// answered after it, in this tab or another.
let passForgetting = null;

// passForgetWaits holds each of those forgets, by its mark's key, as { code,
// done }: whether it takes the Pass too, and the wait that settles once it is
// over, from which passForgetting and passCodeForgetting are made
// (waitOnPassForget).
const passForgetWaits = new Map();

// passForgetHeardLast names the forget this tab last heard of from another tab,
// by its mark ("remove", "browser" or "stop"; stopForPassForget), so that a
// checkout's claim this tab cannot drop after it is said to be still collected
// in words for what was pressed there (forgetPassOrder).
let passForgetHeardLast = "";

// passCheckouts holds each checkout this tab has on its way, from its request
// to its answer (openPassCheckout), as { mark, abort, answered, forgotten }:
// the key of its mark, the controller that calls its request off, whether its
// answer has arrived, and whether a forget was pressed here since then. A
// forget pressed in this tab, and the tab's leaving the document, call off each
// one not yet answered (callOffPassCheckouts), so its answer never arrives to
// set a claim; a forget pressed here after an answer arrived goes after the
// cookie that answer set, and drops it with the rest.
const passCheckouts = new Set();

// passStartedAgain is set when the visitor leaves Stripe without paying, or
// says so while a purchase is being collected, or when a checkout opened that
// the page did not send them to, because they had left the Pass page or the
// Pass held changed meanwhile, in another tab say (submitPass): the page then
// offers to buy again, and goes on collecting underneath until another checkout
// starts, so a payment that does go through meanwhile is still collected. A
// checkout answered after a forget is set aside in the same way, but leaves
// nothing to collect, since its claim is dropped too (forgetPassClaim,
// forgetPassOrder). A checkout gives the browser a new claim, and the earlier
// one is no longer collected here (passCollectingHTML says so).
let passStartedAgain = false;

// passCodeTyped is what is in the Pass page's code box, a Pass code or a
// reissue code, kept in memory and nowhere else, so a drawing of the page
// while the reader types, or while its answer is awaited, does not lose it
// (drawPass). A reissue code stays until an answer arrives, so one whose
// answer was lost is asked again by pressing again. It goes on an answer,
// and with Remove the Pass, Forget this browser and leaving the page
// (forgetTypedCode).
let passCodeTyped = "";

// passCodeCopied is the Pass code this tab has put on the clipboard (Copy the
// code) since it was loaded, so Remove the Pass and Forget this browser empty
// the clipboard again (emptyCopiedPassCode). It is forgotten once a copy or a
// cut in this tab certainly puts something there in the code's place
// (forgetCopiedPassCode), and once the browser no longer holds that code
// because another tab removed or replaced it (followPassStorage).
let passCodeCopied = "";

// passForgetHeard ends the wait on each forget another tab of this browser has
// on its way, by its mark's key, once the mark goes (followPassForget).
const passForgetHeard = new Map();

// passJustIssued is the code a claim or a reissue code has just handed over,
// as { code, expires, kept, replaced, reissued, shown }: replaced is the code
// of a different Pass it took the place of in this browser, and shown is
// whether the Pass page has drawn it. It is drawn on every drawing of the Pass
// page, so no redraw or poll loses a code the server will not show again,
// until the reader leaves the page (render), or the document, for another site
// say (pagehide), once drawn, kept or not (forgetShownCode): one this browser
// could not store goes as its panel says, and a page brought back from the
// back-forward cache shows neither kind to whoever uses the browser next. One
// not yet drawn, its answer having come while the reader was elsewhere, or
// they having left before its drawing came, waits for them to come back to the
// Pass page: one this browser could not keep is nowhere else, and nor is the
// code of a Pass one it keeps took the place of (replaced). Remove the Pass
// and Forget this browser, here or in another tab (stopForPassForget), drop it
// too, and so does another tab's removing the Pass, for whatever reason, which
// takes any code it took the place of too (followPassStorage). One this
// browser kept and no longer holds, another code having taken its place, typed
// in here or stored by another tab, or its data cleared, stays until then,
// worded as no longer here (passIssuedHTML), since it may be nowhere else.
// Once educhan tells this tab the code is no longer valid, as the Pass held
// here, it is worth nothing and goes, and only a code it took the place of
// stays, worded for whether this browser holds that one again
// (forgetInvalidCode).
let passJustIssued = null;

// lastPassInfo is what GET /api/pass said for the drawing on screen, so a
// button that changes only what this browser holds draws the page again at
// once (drawPass), without waiting on the server.
let lastPassInfo = { enabled: false };

// passNonces keeps the nonces in memory too, for a browser that will not
// store them.
const passNonces = {};

function storedPass() {
  const raw = storageGet(PASS_STORAGE_KEY);
  if (!raw) {
    return null;
  }
  try {
    const saved = JSON.parse(raw);
    if (typeof saved.code !== "string" || !PASS_CODE_PATTERN.test(saved.code)) {
      forgetPass();
      return null;
    }
    if (saved.expired === true) {
      return { code: saved.code, expires: null, expired: true };
    }
    const expires = new Date(saved.expires);
    if (Number.isNaN(expires.getTime())) {
      forgetPass();
      return null;
    }
    return { code: saved.code, expires, expired: expires <= new Date() };
  } catch {
    forgetPass();
    return null;
  }
}

// storePass keeps a Pass in this browser, and says whether it was kept: a
// browser that blocks storage, or has run out of it, keeps nothing, and the
// page must not then say the Pass is here.
function storePass(code, expires) {
  storageSet(PASS_STORAGE_KEY, JSON.stringify({ code, expires }));
  const kept = storedPass();
  return Boolean(kept && kept.code === code);
}

// markPassExpired keeps a Pass the server says has run out, for renewing,
// without the date this browser had wrong.
function markPassExpired(code) {
  storageSet(PASS_STORAGE_KEY, JSON.stringify({ code, expired: true }));
}

function forgetPass() {
  try {
    localStorage.removeItem(PASS_STORAGE_KEY);
  } catch {
    // Nothing was stored, so nothing needs removing.
  }
}

// forgetInvalidCode follows educhan's word that the Pass held here (code) is
// no longer valid, which removed it, for a code just handed over: if that was
// the code, its panel would go on showing a code worth nothing, so the code
// goes. The code of a different Pass it took the place of (replaced), which
// may still work and is nowhere else, stays on the panel until the reader
// leaves the page, as it was; with none, the panel goes too.
function forgetInvalidCode(code) {
  if (!passJustIssued || passJustIssued.code !== code) {
    return;
  }
  if (passJustIssued.replaced) {
    passJustIssued.code = "";
  } else {
    passJustIssued = null;
  }
}

// newPassNonce is 16 random bytes as unpadded base64url, the form the server
// takes a nonce in.
function newPassNonce() {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// passNonce is the nonce kept under key, drawn if there is none.
function passNonce(key) {
  const stored = storageGet(key);
  if (stored && PASS_NONCE_PATTERN.test(stored)) {
    passNonces[key] = stored;
    return stored;
  }
  if (!passNonces[key]) {
    passNonces[key] = newPassNonce();
  }
  storageSet(key, passNonces[key]);
  return passNonces[key];
}

// passNoncePending says whether a nonce is kept under key from a request
// already sent: a collection under way when this page was last left, or a
// claim or reissue code whose answer may never have arrived, so it is asked
// for again.
function passNoncePending(key) {
  const stored = storageGet(key);
  return Boolean((stored && PASS_NONCE_PATTERN.test(stored)) || passNonces[key]);
}

function dropPassNonce(key) {
  delete passNonces[key];
  try {
    localStorage.removeItem(key);
  } catch {
    // Nothing was stored, so nothing needs removing.
  }
}

// forgetPassClaim stops this browser collecting anything bought here that has
// not arrived yet. The page stops first: the collection's nonce is dropped and
// the collection on screen ends (passVisit), so no claim is sent after this,
// and none draws a new nonce, and an answer already on its way is dropped when
// it arrives (passForgets). Then the server drops the claim, which only it can
// do, since the claim is a cookie the page cannot see; the purchase stays the
// buyer's, for a reissue code from the receipt. Until the server has the
// request, nothing starts collecting again (passForgetting), and a mark of its
// own under PASS_FORGET_STORAGE_KEY has the browser's other tabs do the same
// (followPassForget), and one opened meanwhile wait (heedPassForgetsUnderWay).
// The mark names what was pressed (pressed): "remove" for Remove the Pass,
// "browser" for Forget this browser, or "stop" for Stop collecting it here. For
// the first two (emptyBoxes), which take the Pass too, the reissue code's nonce
// goes as well, an answer about a Pass already on its way is dropped
// (passCodeForgets), and the other tabs empty their code boxes; Stop collecting
// it here leaves a reissue code being spent alone. A checkout this tab has on
// its way is called off before any of it, if its answer has not arrived, so
// that answer sets no claim; one whose answer has arrived set its cookie before
// this request goes, which drops it with the rest (callOffPassCheckouts). It
// resolves, once every forget this tab knows to be on its way is over, to
// whether nothing is left to collect here: true once the server has answered,
// or when it sells no Pass, and false when it could not be reached, since the
// claim is then still this browser's.
function forgetPassClaim(pressed) {
  callOffPassCheckouts(true);
  const emptyBoxes = pressed !== "stop";
  passVisit += 1;
  passForgets += 1;
  dropPassNonce(PASS_NONCE_STORAGE_KEY);
  if (emptyBoxes) {
    passCodeForgets += 1;
    dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);
    dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);
  }
  const mark = `${PASS_FORGET_STORAGE_KEY}:${newPassNonce()}`;
  storageSet(mark, `${pressed}:${Date.now()}`);
  const request = (async () => {
    try {
      await api("/api/pass/claim", { method: "POST", body: JSON.stringify({ forget: true }) });
      return true;
    } catch (err) {
      return err.status === 403;
    }
  })();
  // Its mark comes off once it is answered, and until then the page waits on
  // it, with every other forget on its way.
  waitOnPassForget(
    mark,
    emptyBoxes,
    request.then(() => {
      try {
        localStorage.removeItem(mark);
      } catch {
        // Nothing was stored, so nothing needs removing.
      }
    })
  );
  return passForgetting.then(() => request);
}

// forgetPassOrder drops the claim a checkout's answer has just set, by the name
// that answer gives its order: the checkout was on its way when Remove the
// Pass, Forget this browser or Stop collecting it here was pressed in another
// tab, whose request may have gone too soon to see the claim, and that claim
// would otherwise outlive the forget, for the next person at the computer to
// find being collected (submitPass, which does not follow it to Stripe). A
// forget pressed in this tab needs none: it calls off a checkout not yet
// answered, and goes after the cookie of one answered (callOffPassCheckouts).
// That claim and no other: a checkout started after the forget, here or in
// another tab, may have set its own by now, which a forget of the browser's
// claims would take, so this reads no cookie and clears none, and the cookie it
// set can stay, with nothing under it, until another checkout replaces it,
// Remove the Pass, Forget this browser or Stop collecting it here clears it, or
// it runs out. The name is sent once, here, and kept nowhere. It needs no mark
// and stops nothing, since it reaches no other claim. A server that gave no
// name is asked nothing: a forget without one is a forget of every claim. What
// it says when educhan cannot be reached is worded for the forget pressed in
// the other tab (pressed, from its mark; PASS_ORDER_KEPT).
function forgetPassOrder(order, pressed) {
  if (typeof order !== "string" || !order) {
    return;
  }
  api("/api/pass/claim", { method: "POST", body: JSON.stringify({ forget: true, order }) }).catch((err) => {
    // The claim is still this browser's, unless the server sells no Pass now,
    // and the Pass page offers to stop collecting it once it is drawn again.
    // Said on whatever page this tab is on, since the reader who pressed the
    // forget, in another tab, was told it was done: the one exception to
    // passPageShown in the Pass section. On the Pass page it answers this
    // tab's own Buy or Renew, pressed there, so it is brought into view
    // (showPassNotice) like any other answer to a press there.
    if (err.status !== 403) {
      const notice = PASS_ORDER_KEPT[pressed] || PASS_ORDER_KEPT.browser;
      if (passPageShown()) {
        showPassNotice(notice);
      } else {
        showNotice(notice);
      }
    }
  });
}

// What forgetPassOrder says when educhan cannot be reached to drop a
// checkout's claim, by the forget pressed in the other tab: how to drop it once
// back online, with no more than that forget did. After Stop collecting it here
// or Forget this browser, that is the same button again. After Remove the Pass,
// which leaves no Pass and so no Remove to press, it is Stop collecting it
// here, which the Pass page offers while a claim is collected, and which keeps
// the poster key, the name and any badge, as Remove did. The checkout was
// never followed to Stripe, so the Pass page, which collects as soon as it is
// opened, finds nothing paid under it.
const PASS_ORDER_KEPT = {
  stop:
    "educhan couldn't be reached to drop the checkout that was still opening when Stop collecting it here was pressed, so this browser may still be collecting it. Open the Pass page when you're back online and press Stop collecting it here again.",
  remove:
    "educhan couldn't be reached to drop the checkout that was still opening when the Pass was removed, so this browser may still be collecting it. Open the Pass page when you're back online and press Stop collecting it here.",
  browser:
    "educhan couldn't be reached to drop the checkout that was still opening when this browser was forgotten, so the browser may still be collecting it. Press Forget this browser on the Verify page again when you're back online.",
};

// waitOnPassForget notes a forget on its way under its mark's key, one that
// takes the Pass too when code is set, until over settles, and keeps
// passForgetting and passCodeForgetting to the forgets still on their way.
// Each is taken off them before anything that waits on them goes on.
function waitOnPassForget(key, code, over) {
  const wait = { code };
  wait.done = over.then(() => {
    if (passForgetWaits.get(key) === wait) {
      passForgetWaits.delete(key);
      settlePassForgetting();
    }
  });
  passForgetWaits.set(key, wait);
  settlePassForgetting();
  return wait.done;
}

// settlePassForgetting makes passForgetting and passCodeForgetting again from
// the forgets still on their way (passForgetWaits), or null when none is.
function settlePassForgetting() {
  const all = [];
  const codes = [];
  for (const wait of passForgetWaits.values()) {
    all.push(wait.done);
    if (wait.code) {
      codes.push(wait.done);
    }
  }
  passForgetting = all.length ? Promise.all(all) : null;
  passCodeForgetting = codes.length ? Promise.all(codes) : null;
}

// passForgetLeft is how much longer a tab waits on another tab's forget, or a
// checkout on another checkout, by its mark: until PASS_FORGET_WAIT_MS after it
// was set, and never longer than that.
function passForgetLeft(mark) {
  const left = Number(mark.slice(mark.indexOf(":") + 1)) + PASS_FORGET_WAIT_MS - Date.now();
  return left > 0 ? Math.min(left, PASS_FORGET_WAIT_MS) : 0;
}

// openPassCheckout notes a checkout as its request goes (passCheckouts), with a
// mark of its own for the browser's other checkouts to wait on
// (PASS_CHECKOUT_STORAGE_KEY), and returns it, with the controller that calls
// its request off. The mark holds nothing but the time.
function openPassCheckout() {
  const checkout = {
    mark: `${PASS_CHECKOUT_STORAGE_KEY}:${newPassNonce()}`,
    abort: new AbortController(),
    answered: false,
    forgotten: false,
  };
  storageSet(checkout.mark, `checkout:${Date.now()}`);
  passCheckouts.add(checkout);
  return checkout;
}

// closePassCheckout takes a checkout and its mark off, once its answer is in or
// it is called off, so the browser's other checkouts go on.
function closePassCheckout(checkout) {
  passCheckouts.delete(checkout);
  try {
    localStorage.removeItem(checkout.mark);
  } catch {
    // Nothing was stored, so nothing needs removing.
  }
}

// callOffPassCheckouts calls off each checkout this tab has on its way whose
// answer has not arrived: its request is dropped, so the browser keeps no
// cookie from it, and the page follows it nowhere (submitPass). A forget
// pressed in this tab (forgotten) does so before its own request goes, and
// notes each checkout whose answer has arrived instead: that answer's cookie is
// already in the browser, so the forget drops its claim with the rest, and it
// needs no drop of its own (forgetPassOrder). Leaving the document (pagehide)
// calls them off too, since nothing here follows an answer that arrives while
// the page is gone or in the back-forward cache, and a forget pressed in
// another tab meanwhile would never see its claim.
function callOffPassCheckouts(forgotten) {
  for (const checkout of [...passCheckouts]) {
    if (checkout.answered) {
      checkout.forgotten = checkout.forgotten || forgotten;
    } else {
      closePassCheckout(checkout);
      checkout.abort.abort();
    }
  }
}

// passCheckoutLeft is how much longer a checkout waits on the others this
// browser has on their way, here or in another tab, by their marks: until the
// last of them goes, each for at most PASS_FORGET_WAIT_MS from when it was set
// (passForgetLeft). A mark older than that is taken off. Storage that cannot be
// read holds no mark, and a browser like that is not sent to Stripe
// (passCanBeKept).
function passCheckoutLeft() {
  let left = 0;
  try {
    const marks = [];
    for (let at = 0; at < localStorage.length; at += 1) {
      const key = localStorage.key(at);
      if (key && key.startsWith(`${PASS_CHECKOUT_STORAGE_KEY}:`)) {
        marks.push(key);
      }
    }
    for (const key of marks) {
      const wait = passForgetLeft(localStorage.getItem(key) || "");
      if (wait > 0) {
        left = Math.max(left, wait);
      } else {
        localStorage.removeItem(key);
      }
    }
  } catch {
    return 0;
  }
  return left;
}

// keepPassCheckoutMark puts back the mark of a checkout this tab has on its way
// when another tab takes it off (the storage event): Forget this browser takes
// every mark off (forgetBrowser), and a checkout pressed after it, in any tab,
// still waits for this one's answer, so the one pressed last holds the claim
// cookie (passCheckoutLeft). It goes back as it was, with the time it was set,
// and one past its wait, taken off as a tab closed before its answer would be,
// stays off.
function keepPassCheckoutMark(event) {
  if (event.newValue !== null || !event.oldValue || passForgetLeft(event.oldValue) === 0) {
    return;
  }
  for (const checkout of passCheckouts) {
    if (checkout.mark === event.key) {
      storageSet(checkout.mark, event.oldValue);
    }
  }
}

// heedPassForget waits on a forget another tab has on its way under key, until
// its mark goes (followPassForget) or passForgetLeft runs out, and returns the
// wait.
function heedPassForget(key, mark) {
  const over = new Promise((resolve) => {
    passForgetHeard.set(key, resolve);
    setTimeout(resolve, passForgetLeft(mark));
  }).then(() => {
    passForgetHeard.delete(key);
  });
  return waitOnPassForget(key, !mark.startsWith("stop:"), over);
}

// stopForPassForget stops this tab as the tab that pressed Remove the Pass,
// Forget this browser or Stop collecting it here stopped (forgetPassClaim), for
// a forget under key that this tab has only now heard of, by its mark's storage
// event (followPassForget) or by finding the mark there
// (heedPassForgetsUnderWay), and returns the wait on it. What it is collecting
// ends, an answer to a claim already on its way is dropped unstored
// (passForgets), the collection's nonce goes, and nothing collects here until
// the mark goes, or for at most PASS_FORGET_WAIT_MS from when it was set
// (heedPassForget), and until every other forget on its way is over too
// (passForgetting). What was pressed there is noted (passForgetHeardLast), for
// a checkout's claim this tab may yet fail to drop (forgetPassOrder). Remove
// and Forget ("remove:" and "browser:") also drop a reissue code's nonce and
// its mark, and any answer about a Pass on its way or arriving while the mark
// is there (passCodeForgets, passCodeForgetting), and empty the code box here,
// and drop what this tab would say about a Pass found gone (passLost) and a
// code just handed over here (passJustIssued), kept or not, which the drawing
// once the wait is over leaves off (its panel stays on the page until then, or
// until the reader leaves the page or the document, forgetShownCode), so
// neither a reissue code typed in this tab nor the last holder's news or code
// is left for the next person at the computer; Stop ("stop:") leaves a reissue
// code alone, and a code just handed over too.
// Check now and Stop go off, as in the tab Stop was pressed in, and in a tab
// that hears of the forget (followPassForget), focus on one of them stays on
// its panel until the page is drawn again once the wait is over, so the drawing
// gives it to what took its place; a page brought back from the back-forward
// cache is drawn afresh by render.
function stopForPassForget(key, mark) {
  const code = !mark.startsWith("stop:");
  passForgetHeardLast = mark.slice(0, mark.indexOf(":"));
  passVisit += 1;
  passForgets += 1;
  dropPassNonce(PASS_NONCE_STORAGE_KEY);
  if (code) {
    passCodeForgets += 1;
    dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);
    dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);
    forgetTypedCode();
    passLost = "";
    passJustIssued = null;
  }
  const waited = heedPassForget(key, mark);
  const focused = document.activeElement;
  for (const button of el.main.querySelectorAll(PASS_COLLECT_BUTTONS)) {
    button.disabled = true;
    if (button === focused) {
      (button.closest('[tabindex="-1"]') || el.main).focus({ preventScroll: true });
    }
  }
  return waited;
}

// heedPassForgetsUnderWay waits on every forget another tab has on its way that
// this page has not heard of, which no storage event will tell it of: one sent
// before it was loaded, or while it was in the back-forward cache, when a mark
// that went meanwhile ends its wait too. It stops for each as for one it hears
// of (stopForPassForget). At load that changes nothing: nothing has been asked
// for or kept in this page yet, and the tab that set the mark dropped the
// nonces before it set it. A page brought back from the back-forward cache,
// though, may hold a collection's nonce, a reissue code's or the last holder's
// news, and the storage event for the mark, which comes after the pageshow
// event, finds it already waited on. Nothing collects here until it is over
// (renderPass, collectPass), and Check now and Stop are drawn off meanwhile
// (drawPass). A mark set more than PASS_FORGET_WAIT_MS ago is one a tab closed
// before its answer left, and is taken off.
function heedPassForgetsUnderWay() {
  const marks = new Map();
  try {
    for (let at = 0; at < localStorage.length; at += 1) {
      const key = localStorage.key(at);
      if (key && key.startsWith(`${PASS_FORGET_STORAGE_KEY}:`)) {
        marks.set(key, localStorage.getItem(key) || "");
      }
    }
  } catch {
    return;
  }
  for (const [key, heard] of passForgetHeard) {
    if (!marks.has(key)) {
      heard();
    }
  }
  for (const [key, mark] of marks) {
    if (passForgetWaits.has(key)) {
      continue;
    }
    if (passForgetLeft(mark) > 0) {
      stopForPassForget(key, mark);
    } else {
      try {
        localStorage.removeItem(key);
      } catch {
        // Nothing was stored, so nothing needs removing.
      }
    }
  }
}

// passForgotten says whether an answer about a Pass, to a request sent when
// passCodeForgets was forgets, has to be dropped unstored: Remove the Pass or
// Forget this browser was pressed meanwhile, here or in another tab, or one is
// still on its way. Whoever pressed it meant this browser to keep no Pass.
function passForgotten(forgets) {
  return forgets !== passCodeForgets || passCodeForgetting !== null;
}

// passForgetsSettled waits until no forget is on its way, here or in another
// tab, which a request about a Pass the reader asks for meanwhile goes after: a
// code typed in once Remove the Pass was pressed is then not dropped for being
// sent before Remove's own request was done (passForgotten), and a checkout's
// claim is not dropped by a forget sent before it. A checkout (checkout) waits
// as well until no other checkout is on its way, here or in another tab
// (passCheckoutLeft), so the claim cookie its answer sets is not replaced by an
// earlier one's that answers later, one a forget overtook included. It resolves
// to what to say if another forget came while it waited, or "": the forgets are
// noted when it is called, which is when the button was pressed, so a press
// made before that forget sends nothing, and one made after it goes. Remove the
// Pass or Forget this browser, here or in another tab, is
// PASS_FORGOTTEN_MEANWHILE. A Stop alone is PASS_STOPPED_MEANWHILE, and can
// come only while a checkout waits on another, or from a tab that has waited
// the forget on its way out (PASS_FORGET_WAIT_MS), since Check now and Stop are
// off until then in every tab that knows of it, and every tab does, opened
// meanwhile included. A press whose document is left while it waits (pagehide,
// which moves passLeft), for another site say, stops waiting and comes to
// PASS_LEFT_MEANWHILE, ahead of either: it sends nothing and says nothing,
// since the reader has gone, where it would otherwise go once the page came
// back from the back-forward cache, with no new press. Each wait ends a turn
// after the forgets have, so what a forget does with its own answer (Stop's
// word that this browser has stopped collecting, say) comes first, and what the
// press says is not drawn over.
async function passForgetsSettled(checkout = false) {
  const codes = passCodeForgets;
  const claims = passForgets;
  const left = passLeft;
  while (left === passLeft && (passForgetting || (checkout && passCheckoutLeft() > 0))) {
    await (passForgetting || new Promise((resolve) => setTimeout(resolve, PASS_CHECKOUT_LOOK_MS)));
    await new Promise((resolve) => setTimeout(resolve));
  }
  if (left !== passLeft) {
    return PASS_LEFT_MEANWHILE;
  }
  if (codes !== passCodeForgets) {
    return PASS_FORGOTTEN_MEANWHILE;
  }
  return claims !== passForgets ? PASS_STOPPED_MEANWHILE : "";
}

// passPageShown says whether the reader is still on the Pass page, which
// every drawing of it after a wait asks first: an answer that arrives once
// they have moved on, to a board or the Verify page, stores what it is still
// right to store, and draws, focuses and says nothing over the page they are
// on. The Pass page shows what was stored when they come back.
function passPageShown() {
  return state.route.view === "pass";
}

// forgetTypedCode empties the Pass page's code box, and what is kept of it.
function forgetTypedCode() {
  passCodeTyped = "";
  const box = document.getElementById("f-pass-code");
  if (box) {
    box.value = "";
  }
}

// forgetShownCode drops a code just handed over once the Pass page has drawn
// it, kept or not, for a reader leaving the page (render) or the document
// (pagehide), and takes the panel of a code just handed over off the page in
// any case: a page brought back from the back-forward cache is shown as it was
// left until it is drawn again (the pageshow listener), and a panel on the page
// has been drawn, even one whose code has gone already, dropped by another
// tab's Remove or Forget whose drawing still waits on the forget
// (stopForPassForget), say. One this browser could not keep, or no longer
// holds, goes as its panel promises, and one it holds can still be shown from
// the held Pass's panel. One not yet drawn, its answer having come while the
// reader was on another page, or they having left before its drawing came, has
// been seen by nobody, so it is left to be drawn when they come back to the
// Pass page: one this browser could not keep, or no longer holds, is nowhere
// else, and beside one it keeps may be the code of the Pass it took the place
// of, which is nowhere else either.
function forgetShownCode() {
  if (passJustIssued && passJustIssued.shown) {
    passJustIssued = null;
  }
  const panel = document.getElementById("pass-issued");
  if (panel) {
    panel.remove();
  }
}

// emptyCopiedPassCode empties the clipboard after this tab put a Pass code
// there (Copy the code), for Remove the Pass and Forget this browser. The page
// cannot read the clipboard without asking the reader's leave, so it empties
// whatever is on it by then, unless a copy or a cut in this tab has certainly
// put something there since (forgetCopiedPassCode); something copied in
// another tab or app meanwhile goes too, and so does a copy here the page
// cannot be sure of, and the page says so. Writing to the clipboard takes the
// reader's click, so it is called inside one; a browser that will not write
// leaves the clipboard as it was. A code copied any other way, by hand, in
// another tab, before this tab was last loaded, or before another tab removed
// or replaced the Pass (followPassStorage), is beyond it, and the page says
// so; so is any clipboard history the computer keeps.
function emptyCopiedPassCode() {
  if (!passCodeCopied) {
    return;
  }
  passCodeCopied = "";
  if (navigator.clipboard) {
    navigator.clipboard.writeText("").catch(() => {});
  }
}

// forgetCopiedPassCode follows a copy or a cut in this tab that certainly put
// something on the clipboard, in the code's place, or the code again by hand:
// Remove the Pass and Forget this browser then leave the clipboard as it is.
// Either event goes off with nothing selected too, or for a cut that removes
// nothing, and leaves the clipboard as it was, code and all, so one that may
// have put nothing there does not count, and the two err toward emptying the
// clipboard of the code. A copy takes what is selected: in a text box whose
// selection can be read, the box's own has to hold something; in one that does
// not say (an email box, which Chromium counts in the document's selection and
// Firefox does not), the document's selection does; and anywhere else (a
// checkbox, the file picker, a button, a link or the page) the document's
// selection has to hold text of the page's own. Chromium goes on reporting a
// text box's selection as the document's once the focus has left the box, as
// a collapsed range beside it that says the box's text, though a copy then
// takes nothing. A cut takes something only from a box that can be typed in,
// neither read-only nor disabled, or from editable content, with something
// selected there. Copy the code sets off neither event, since the clipboard
// API's own write does not, and nor does a copy from the browser's own menus
// (Copy link address, say), which the page never hears of.
function forgetCopiedPassCode(event) {
  const target = event.target;
  const box = target instanceof HTMLInputElement || target instanceof HTMLTextAreaElement ? target : null;
  const readable = box !== null && box.selectionStart !== null;
  const typed = readable || (box !== null && (box.type === "email" || box.type === "number"));
  const selection = document.getSelection();
  const range = selection && selection.rangeCount ? selection.getRangeAt(0) : null;
  const selected = readable
    ? box.selectionStart !== box.selectionEnd
    : typed
      ? String(selection || "") !== ""
      : String(range || "") !== "";
  const editable = box
    ? typed && !box.readOnly && !box.disabled
    : target instanceof HTMLElement && target.isContentEditable;
  if (selected && (event.type === "copy" || editable)) {
    passCodeCopied = "";
  }
}

// passCanBeKept says whether this browser would keep what a checkout buys:
// the purchase is collected with a cookie, and the Pass is kept in storage. A
// browser that keeps neither would send its buyer to pay for a Pass it could
// never collect.
function passCanBeKept() {
  if (!navigator.cookieEnabled) {
    return false;
  }
  storageSet(STORAGE_PROBE_KEY, "1");
  const kept = storageGet(STORAGE_PROBE_KEY) === "1";
  try {
    localStorage.removeItem(STORAGE_PROBE_KEY);
  } catch {
    // Nothing was stored, so nothing needs removing.
  }
  return kept;
}

// loadPassInfo asks whether the Pass is on sale and on what terms, and whether
// this browser has a purchase to collect. A server that did not answer is
// kept apart from one that does not sell the Pass, so the page does not say
// the second when it means the first.
async function loadPassInfo() {
  try {
    const info = await api("/api/pass");
    return info && info.enabled ? info : { enabled: false };
  } catch (err) {
    return { enabled: false, failed: err };
  }
}

// passErrorText is a failed Pass request in words: a server that did not
// answer is UNREACHABLE, as on any page, and not "not on sale". errorMessage
// is for the posting form, and speaks of a post and its box, which the Pass
// page does not have. The server's words, or a proxy's status line (with no
// reason phrase over HTTP/2), are made a sentence, capital and full stop,
// since the page often goes on after them.
function passErrorText(err) {
  if (err.unreachable) {
    return UNREACHABLE;
  }
  const seconds = err.status === 429 && err.payload && err.payload.retry_after;
  if (seconds) {
    return `Wait ${plural(seconds, "second", "seconds")}, then try again.`;
  }
  const said = err.message.trim();
  const text = said.charAt(0).toUpperCase() + said.slice(1);
  return /[.!?]$/.test(text) ? text : `${text}.`;
}

// passRunOutText says that a Pass has run out, and what the page offers.
function passRunOutText() {
  return lastPassInfo.paused
    ? `This Pass has run out. ${PASS_PAUSED}`
    : "This Pass has run out. You can renew it below.";
}

// passPriceText writes Stripe's amount, kept in the currency's smallest unit,
// in the reader's own format. How many decimals that unit stands for is
// Stripe's to say, and the server sends it beside the amount (exponent): the
// browser's own table differs, with ISK and UGX whole where Stripe keeps them
// in hundredths, so 300000 is 3,000 ISK, not 300,000. Intl then writes a whole
// amount with the currency's usual digits, and one with a part of a unit with
// Stripe's decimals, which the usual digits could round away: ALL is whole in
// the browser's table too, and 1050 is ALL 10.50, not 11.
function passPriceText(price) {
  if (
    !price ||
    !Number.isSafeInteger(price.amount) ||
    typeof price.currency !== "string" ||
    !Number.isSafeInteger(price.exponent) ||
    price.exponent < 0
  ) {
    return "";
  }
  const options = { style: "currency", currency: price.currency.toUpperCase() };
  if (price.amount % 10 ** price.exponent !== 0) {
    options.minimumFractionDigits = price.exponent;
    options.maximumFractionDigits = price.exponent;
  }
  try {
    const format = new Intl.NumberFormat(undefined, options);
    return format.format(price.amount / 10 ** price.exponent);
  } catch {
    return "";
  }
}

// passUntilText is when a Pass runs out, in words for a notice, in the
// reader's own time zone like the badge's expiry (timeHTML): the server keeps
// a UTC midnight, which is the evening before west of UTC.
function passUntilText(iso) {
  return new Date(iso).toLocaleString(undefined, {
    day: "numeric",
    month: "long",
    year: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}

function passTermText(days) {
  return days === 365 ? "a year" : plural(days, "day", "days");
}

// What a Pass is, before anything about this browser's. It is the page's
// promise, so it says what Stripe holds and what educhan does not. Without the
// server's terms it says only what is true of every Pass.
function passIntroHTML(info) {
  const term = info.enabled ? passTermText(info.days) : "a set time";
  const price = info.enabled ? passPriceText(info.price) : "";
  // With Stripe Tax on, the server says whether the tax is in the price or
  // added to it at checkout, where it can come to nothing for some buyers.
  // Without it no tax is collected, and the server says nothing of tax.
  const priced =
    info.tax === "included"
      ? `${price} for ${term}, tax included`
      : info.tax === "added"
        ? `${price} plus any tax for ${term}`
        : `${price} for ${term}`;
  const cost = price
    ? `<p class="verify-note"><strong>${esc(priced)}</strong>, paid once. It
      doesn't renew by itself.</p>`
    : "";
  const budget = info.enabled ? plural(info.hourly_posts, "post", "posts") : "a set number of posts";
  return `
    <p class="verify-note">
      An educhan Pass lets you post without solving the captcha, for ${esc(term)}, and it helps
      pay for running educhan. You don't need one: ${audienceText()} can read and post without it.
    </p>
    ${cost}
    <ul class="verify-note pass-list">
      <li>A Pass skips the captcha for up to ${esc(budget)} an hour. Hours are counted in UTC,
        so in India, for one, each starts at half past. After that you solve the captcha as
        usual until the next hour starts. Everyone using a shared Pass shares that budget.</li>
      <li>Your posts look exactly like everyone else's. A Pass adds no name, badge or mark,
        and the rules and the wait between posts apply as usual.</li>
      <li>A Pass is sold only to people aged 18 and over. Buying one means confirming that
        you're 18 or over, and that answer isn't saved anywhere.</li>
    </ul>
    <p class="verify-note">
      You pay on Stripe's own page, not on educhan. Stripe keeps your payment details, such as
      your card, email, name and billing address, as any shop's payment company does, and
      educhan stores none of them. Whoever runs educhan can see in Stripe who bought a Pass,
      but never which posts it was used on: educhan keeps only a scrambled reference to the
      payment, so that a refund or a chargeback can take back the time that payment bought,
      and so that a lost code can be replaced from the payment on your receipt. It keeps
      nothing that links a Pass to any post.
    </p>
    <p class="verify-note muted">
      Your Pass code is kept in this browser. It's sent with your posts, and from this page
      when you renew it or check it, and nowhere else. Posts with the verified badge don't send
      it, so a payment and a university never meet in one post.
    </p>`;
}

// passHeldHTML is this browser's Pass, with what can be done about it.
// codeOnScreen is set when the panel of a code just handed over shows this
// one, which then needs no second showing.
function passHeldHTML(info, held, codeOnScreen) {
  let status = "";
  if (!held.expired) {
    status = `<p>This browser has a Pass, good until ${timeHTML(held.expires.toISOString())}.</p>`;
  } else {
    status = held.expires
      ? `<p>The Pass in this browser ran out on ${timeHTML(held.expires.toISOString())}.</p>`
      : `<p>The Pass in this browser has run out.</p>`;
  }
  // A Pass renewed on another device looks expired here until the server is
  // asked again.
  const check = held.expired
    ? `
      <p class="muted">Renewed it somewhere else? Check it again, or enter a newer code below.</p>
      <div class="actions">
        <button class="btn subtle" type="button" data-action="check-pass">Check it again</button>
      </div>`
    : "";
  const show = codeOnScreen
    ? ""
    : `
      <div class="actions">
        <button class="btn subtle" type="button" data-action="show-pass" aria-expanded="false"
                aria-controls="pass-shown">Show the code</button>
      </div>
      <div class="pass-shown" id="pass-shown" hidden></div>`;
  let renew = "";
  if (info.paused) {
    renew = `<p class="muted">${esc(PASS_PAUSED)}</p>`;
  } else if (info.enabled) {
    renew = `
      <form class="fields pass-renew" data-pass="renew">
        <p class="muted">${held.expired
          ? `Renewing gives it ${esc(passTermText(info.days))} from when this browser collects the renewal, usually as soon as you've paid.`
          : `Renewing adds ${esc(passTermText(info.days))} to the time it has left.`} Your code stays the same.</p>
        ${passAttestHTML("f-pass-renew-attest")}
        <div class="actions">
          <button class="btn primary" type="submit">Renew on Stripe</button>
        </div>
      </form>`;
  }
  return `
    <div class="verify-panel" id="pass-held" tabindex="-1">
      ${status}
      ${check}
      ${show}
      ${renew}
      <div class="verify-forget">
        <p><strong>On a shared or borrowed computer, remove it before you walk away.</strong>
          Anyone using this browser could post with it. Removing it also stops this browser
          collecting anything bought here that hasn't arrived yet, even a checkout still
          opening. If you used Copy the code since you last opened educhan in this tab,
          and have copied nothing in this tab since, removing it empties the clipboard too,
          whatever is on it by then, unless another tab removed or replaced the Pass meanwhile.
          It does so even after a copy in this tab that educhan can't see took anything, such
          as one from an email box in some browsers. A code copied any other way, by hand
          included, stays on the clipboard until something else is copied. To use the Pass
          again afterwards you need its code, so show it and write it down first. If you lose
          it, write to the support address on the receipt Stripe emailed you, and you'll be
          sent a reissue code to enter on this page, which gives your Pass a new code; if the
          payment that bought it was refunded or disputed, that may no longer be possible.</p>
        <button class="btn danger" type="button" data-action="forget-pass">Remove the Pass from this browser</button>
      </div>
    </div>`;
}

// showPassCode shows or hides the code of the Pass this browser holds, to be
// written down before it is removed, or typed in on another device. The code
// goes on the screen and nowhere else, with an id of its own, since a code
// just handed over may be on the page beside it (#pass-code), and focus goes
// to the one it shows.
function showPassCode(button) {
  const slot = document.getElementById("pass-shown");
  const held = storedPass();
  if (!slot || !held) {
    return;
  }
  if (!slot.hidden) {
    slot.hidden = true;
    slot.textContent = "";
    button.textContent = "Show the code";
    button.setAttribute("aria-expanded", "false");
    return;
  }
  slot.innerHTML = `
    <p class="pass-code" id="pass-held-code" tabindex="-1">${esc(held.code)}</p>
    <div class="actions">
      <button class="btn subtle" type="button" data-action="copy-pass">Copy the code</button>
    </div>`;
  slot.hidden = false;
  button.textContent = "Hide the code";
  button.setAttribute("aria-expanded", "true");
  slot.querySelector(".pass-code").focus();
}

// The 18+ box for buying or renewing. Never ticked in advance, never
// required (the page's own refusal says what the visitor can still do), and
// never stored: it is read once, in submitPass.
function passAttestHTML(id) {
  return `
    <label class="attest" for="${id}">
      <input type="checkbox" id="${id}" name="adult" autocomplete="off" />
      <span>I confirm I am 18 or over</span>
    </label>`;
}

// passIssuedHTML is a code the server has just handed over, which it will
// never show again, worded for what this browser holds as it is drawn (held,
// the Pass stored here), not for what it held when the code came: the code
// itself, ready to go with posts, or run out, and so not sent with them until
// it is renewed; or another code, or none, another code having taken its
// place, here or in another tab, or the browser's data cleared, so it goes
// once the reader leaves the page, as one the browser could not keep does. It
// is drawn already filled, so the page moves focus to it (focusPassPanel) for
// a screen reader to read it out. A different Pass it took the place of in
// this browser is shown beside it, to be written down, and is all the panel
// shows once educhan has said the code itself is no longer valid
// (forgetInvalidCode), worded then, as the code itself is, for whether this
// browser holds it as it is drawn, typed back in here or stored by another
// tab say, when it can be shown again from the held Pass's panel.
function passIssuedHTML(issued, held) {
  const here = Boolean(held && held.code === issued.code);
  const took = here ? "takes" : "took";
  let replaced = "";
  if (issued.replaced && issued.reissued) {
    replaced = `
      <p>It ${took} the place of the code this browser had,
        <span class="pass-code-old">${esc(issued.replaced)}</span>. If that code was for another
        Pass, that Pass isn't affected: write its code down to go on using it. If it was for the
        Pass this reissue code was for, it no longer works.</p>`;
  } else if (issued.replaced) {
    replaced = `
      <p>It ${took} the place of the Pass this browser had, whose code is
        <span class="pass-code-old">${esc(issued.replaced)}</span>. That Pass isn't affected:
        write its code down to go on using it.</p>`;
  }
  if (!issued.code) {
    const back = Boolean(held && held.code === issued.replaced);
    const keep = back
      ? `<p><strong>This browser has that code again.</strong> educhan keeps no copy of it, and this
        browser can show it below only until its data is cleared, so write it down if you still
        need it.</p>`
      : `<p><strong>Write that code down now if you still need it.</strong> educhan keeps no copy of
        it, and this browser no longer has it, so it won't be shown again once you leave this page.</p>`;
    return `
    <div class="verify-panel pass-issued" id="pass-issued" tabindex="-1">
      <p><strong>The new code handed over here is no longer valid, so this browser no longer has it.</strong></p>
      ${replaced}
      ${keep}
    </div>`;
  }
  let status = "Your Pass is ready.";
  let said = "This browser has it, and will send it with your posts.";
  let write = "Write this code down or save it somewhere safe now.";
  let again = "educhan keeps no copy of it: this browser can show it again only until its data is cleared.";
  let need = "You need it to use the Pass on another device, or in this one once its data is cleared.";
  if (here && held.expired) {
    status = "Your Pass has run out.";
    said = "This browser has its code, but won't send it with your posts until the Pass is renewed.";
  } else if (!here && issued.kept) {
    status = "This browser no longer has this code.";
    said = held
      ? "Another code has taken its place here, so it isn't sent with your posts."
      : "It isn't sent with your posts.";
    write = "Write this code down or save it somewhere safe now if you still need it.";
    again = "educhan keeps no copy of it, and this browser no longer has it, so it won't be shown again once you leave this page.";
    need = "You need it to use its Pass again, here or on another device.";
  } else if (!here) {
    said =
      "This browser couldn't keep it, because it isn't letting educhan store anything, so it can't send it with your posts. Allow this site to store data, then enter the code below.";
    again = "educhan keeps no copy of it, and this browser couldn't keep it either, so it won't be shown again once you leave this page.";
  }
  return `
    <div class="verify-panel pass-issued" id="pass-issued" tabindex="-1">
      <p><strong>${esc(status)}</strong> ${esc(said)}</p>
      <p class="pass-code" id="pass-code">${esc(issued.code)}</p>
      <p><strong>${esc(write)}</strong> ${esc(again)} ${esc(need)}</p>
      ${replaced}
      <div class="actions">
        <button class="btn subtle" type="button" data-action="copy-pass">Copy the code</button>
      </div>
    </div>`;
}

// passStopCollectingHTML offers to drop the purchase being collected here, for
// a buyer leaving a shared computer before it arrives.
function passStopCollectingHTML() {
  return `
      <p class="muted">Not your computer?
        <button class="link" type="button" data-action="stop-pass-claim">Stop collecting it here</button>,
        so whoever uses this browser next can't. That stops a checkout still opening in this
        browser too. If it's paid, write to the support address on the receipt Stripe emailed
        you, and you'll be sent a reissue code to collect it with.</p>`;
}

// passCollectingHTML stands where the forms would be while this browser has a
// purchase to collect and no Pass yet, so nobody pays again for a Pass that is
// on its way. A visitor who left Stripe without paying can still start again,
// while the Pass is on sale.
function passCollectingHTML(info) {
  const restart = info.paused
    ? ""
    : `
      <p class="muted">Left Stripe without paying?
        <button class="link" type="button" data-action="restart-pass">Start again</button>.
        Once you buy again, this browser stops collecting the earlier checkout. If that one is
        paid after all, it's still yours: write to the support address on the receipt Stripe
        emailed you, and you'll be sent a reissue code to collect it with.</p>`;
  return `
    <div class="verify-panel" id="pass-collecting" tabindex="-1">
      <p><strong>Collecting your Pass…</strong> Stripe hasn't confirmed a payment from this
        browser yet. A card usually takes seconds, and a bank payment can take days. This page
        keeps checking while it's open, and checks again whenever you come back to it.</p>
      <p class="muted" id="pass-collect-note" role="status"></p>
      <div class="actions">
        <button class="btn subtle" type="button" data-action="collect-pass">Check now</button>
      </div>
      ${restart}
      <p class="muted">Paid by card and still waiting after a day, or by bank after two weeks?
        Write to the support address on the receipt Stripe emailed you.</p>
      ${passStopCollectingHTML()}
    </div>`;
}

// passOpenCheckoutHTML is what is still being collected in a browser that has
// a Pass: the latest checkout from it, which the page cannot tell apart from
// a payment just made, back from Stripe before its confirmation, so it says
// what either comes to. It is not a second charge on its way, so it says
// what paying it would do, and the held Pass's panel keeps its renewal.
function passOpenCheckoutHTML() {
  return `
    <div class="verify-panel" id="pass-collecting" tabindex="-1">
      <p><strong>A checkout from this browser hasn't been collected yet.</strong> If you've just
        paid, Stripe is confirming the payment: a card usually takes seconds, and a bank payment
        can take days. If you left it without paying, it stays open on Stripe for up to a day.
        Once it's paid, this page collects it: a renewal is added to the Pass it renews, and a
        new Pass is shown here with its code. This page keeps checking while it's open.</p>
      <p class="muted">Starting another checkout here, a renewal included, stops this browser
        collecting this one. A payment made on it is still yours: write to the support address
        on the receipt Stripe emailed you, and you'll be sent a reissue code to collect it
        with.</p>
      <p class="muted" id="pass-collect-note" role="status"></p>
      <div class="actions">
        <button class="btn subtle" type="button" data-action="collect-pass">Check now</button>
      </div>
      ${passStopCollectingHTML()}
    </div>`;
}

// passEarlierCheckoutHTML stands under the forms while a checkout this browser
// left is still collected underneath (passStartedAgain), so the reader who
// starts again, or comes back from Stripe's cancel link, can still stop it
// being collected here before leaving a shared computer.
function passEarlierCheckoutHTML() {
  return `
    <div class="verify-panel" id="pass-earlier" tabindex="-1">
      <p>Until you start another checkout, this browser goes on collecting the one you left, in
        case it's paid after all.</p>
      ${passStopCollectingHTML()}
    </div>`;
}

// passCodeFieldHTML is the code box: a Pass code to use here, or a reissue
// code to give a Pass a new one. With a good Pass held, only a reissue code is
// taken, since another Pass's code would put this one out of the browser
// without a word.
function passCodeFieldHTML(held) {
  let label = "Have a Pass code, or a reissue code? Enter it";
  let placeholder = "XXXXX-XXXXX-XXXXX-XXXXX";
  if (held && held.expired) {
    label = "Have a newer code, or a reissue code? Enter it";
  } else if (held) {
    label = "Sent a reissue code? Enter it";
    placeholder = "REISSUE-XXXX-XXXX-…";
  }
  return `
      <form class="verify-panel fields" data-pass="activate">
        <div class="field">
          <label class="label" for="f-pass-code">${label}</label>
          <input type="text" id="f-pass-code" name="pass" autocomplete="off" autocapitalize="characters"
                 spellcheck="false" maxlength="64" placeholder="${placeholder}" required />
        </div>
        <div class="actions">
          <button class="btn subtle" type="submit">Use this code here</button>
        </div>
      </form>`;
}

// passViewHTML draws the Pass page from what the server said (info) and what
// this browser holds, with a code just handed over (passJustIssued) on top.
function passViewHTML(info) {
  const held = storedPass();
  const issued = passJustIssued;
  const parts = [`<div class="board-head" data-pass-page><h1>educhan Pass</h1></div>`];
  if (issued) {
    parts.push(passIssuedHTML(issued, held));
    issued.shown = true;
  }
  parts.push(passIntroHTML(info));
  if (passLost) {
    parts.push(`<p class="verify-note">${esc(passLost)}</p>`);
  }
  const shown = Boolean(held && issued && issued.kept && issued.code === held.code);
  // Without the server's answer, a Pass held here can still be shown and
  // removed, which needs nothing from the server, and a code still typed in;
  // nothing is offered for sale, and nothing is said about sales either.
  if (info.failed) {
    if (held) {
      parts.push(passHeldHTML({ enabled: false }, held, shown));
    }
    parts.push(passCodeFieldHTML(held));
    parts.push(`<p class="empty">${esc(passErrorText(info.failed))}</p>`);
    return parts.join("");
  }
  // has is whether this browser has a Pass, kept, or just handed over and
  // shown here because it could not keep it, so Buy is offered beside the
  // code of a Pass it no longer holds, but not beside one it could not keep;
  // open is a checkout under its claim that is still to be collected.
  const has = Boolean(held || (issued && !issued.kept));
  const open = info.enabled && info.claim === true && !passStartedAgain;
  if (held) {
    parts.push(passHeldHTML(info, held, shown));
  }
  if (!info.enabled) {
    parts.push(`<p class="empty">The Pass is not on sale on this server.</p>`);
    return parts.join("");
  }
  if (open && !has) {
    parts.push(passCollectingHTML(info));
    return parts.join("");
  }
  if (open) {
    parts.push(passOpenCheckoutHTML());
  }
  if (!has && info.paused) {
    parts.push(`<p class="empty">${esc(PASS_PAUSED)}</p>`);
  } else if (!has) {
    parts.push(`
      <form class="verify-panel fields" data-pass="buy">
        ${passAttestHTML("f-pass-attest")}
        <div class="actions">
          <button class="btn primary" type="submit">Buy on Stripe</button>
        </div>
      </form>`);
  }
  parts.push(passCodeFieldHTML(held));
  if (info.claim === true && passStartedAgain) {
    parts.push(passEarlierCheckoutHTML());
  }
  return parts.join("");
}

// PASS_COLLECT_BUTTONS are Check now and Stop collecting it here.
const PASS_COLLECT_BUTTONS = '[data-action="collect-pass"], [data-action="stop-pass-claim"]';

// drawPass draws the Pass page, and keeps what it was drawn from for the next
// drawing that changes only what this browser holds. What is in the code box
// is carried across (passCodeTyped), and so is a reader typing in it, caret
// and all, for a drawing that something else set off (a claim's answer, or
// another tab's change to the Pass). Drawn while this browser is stopping
// collecting (passForgetting), here or in another tab, a panel of what is
// being collected has Check now and Stop off, as Stop's own click leaves them,
// until that is settled and says what is left.
function drawPass(info) {
  lastPassInfo = info;
  const box = document.getElementById("f-pass-code");
  const typing = box && document.activeElement === box ? [box.selectionStart, box.selectionEnd] : null;
  if (box) {
    passCodeTyped = box.value;
  }
  el.main.innerHTML = passViewHTML(info);
  if (passForgetting) {
    for (const button of el.main.querySelectorAll(PASS_COLLECT_BUTTONS)) {
      button.disabled = true;
    }
  }
  const again = document.getElementById("f-pass-code");
  if (again) {
    again.value = passCodeTyped;
    if (typing) {
      again.focus();
      again.setSelectionRange(typing[0], typing[1]);
    }
  }
}

// focusPassPanel moves focus to a panel drawn already filled, which a screen
// reader would otherwise pass over in silence.
function focusPassPanel(id) {
  const panel = document.getElementById(id);
  if (panel) {
    panel.focus();
  }
}

// passFocus moves focus, after a button on the Pass page has drawn it again, to
// the first of selectors still on the page: what took the button's place,
// rather than the top of the document, where a keyboard would start again. An
// empty selector is passed over.
function passFocus(...selectors) {
  for (const selector of selectors) {
    const target = selector && el.main.querySelector(selector);
    if (target) {
      target.focus();
      return;
    }
  }
  el.main.focus();
}

// showPassNotice says what a press on the Pass page came to (showNotice), and
// brings the notice into view when it is out of view, above the screen or
// under the sticky header: on a phone the forms sit far below it, and a
// refusal said out of sight looks like nothing happened. It waits a turn
// first, so the focus the press gives (refocus, passFocus) is given before,
// and it moves nothing else: focus stays where the press put it, a notice in
// view stays where it is, and the page goes straight there, not smoothly
// (.notice's scroll-margin-top in app.css takes it to its top, where the
// notice sits). Check now's answer is said this way too (showClaimed). What
// the page says on its own (a collection's later news, say) and every other
// page's notices stay where showNotice puts them.
function showPassNotice(message) {
  showNotice(message);
  setTimeout(() => {
    const box = el.notice.getBoundingClientRect();
    const header = document.querySelector(".site-header").getBoundingClientRect();
    if (!el.notice.hidden && (box.top < header.bottom || box.bottom > window.innerHeight)) {
      el.notice.scrollIntoView({ block: "nearest" });
    }
  });
}

// passTwin is a selector for control as the Pass page draws it again, so focus
// on it can stay on it across a drawing (redrawPass, refocus): its id, else its
// action within the panel it is in, else the kind of its form (form[data-pass])
// and its part there, the button that sends it or the box. A Copy the code
// beside the held Pass's code is not one beside a code just handed over. It is
// "" for anything else.
function passTwin(control) {
  if (control.id) {
    return `#${CSS.escape(control.id)}`;
  }
  const panel = control.closest(".verify-panel[id]");
  const within = panel ? `#${CSS.escape(panel.id)} ` : "";
  if (control.dataset.action) {
    return `${within}[data-action="${control.dataset.action}"]`;
  }
  const form = control.closest("form[data-pass]");
  if (form && control.matches('button[type="submit"], input')) {
    const part = control.matches("input") ? "input" : 'button[type="submit"]';
    return `form[data-pass="${form.dataset.pass}"] ${part}`;
  }
  return "";
}

// refocus gives focus back to a button that was disabled while it waited,
// which dropped it to the top of the document, once it is enabled again.
// Focus the reader has moved on meanwhile stays where they put it. A button the
// page was drawn again over while it waited (by another tab's forget, say) is
// gone, so focus goes to the same button drawn again (passTwin), else the
// first of the Pass forms' controls, else the first panel, never the top of
// the document.
function refocus(button) {
  if (document.activeElement && document.activeElement !== document.body) {
    return;
  }
  if (button.isConnected) {
    button.focus();
    return;
  }
  passFocus(passTwin(button), "form[data-pass] input, form[data-pass] button", '.verify-panel[tabindex="-1"]');
}

// renderPass draws the Pass page from the server's answer, and resolves to
// whether it did: a drawing overtaken by another, or by a forget, or by the
// reader leaving the page, draws nothing. Given selectors, for a drawing that
// something other than the reader set off, it draws as redrawPass does,
// keeping a keyboard where it was.
async function renderPass(...selectors) {
  const visit = ++passVisit;
  const asked = passAsked;
  passAsked = false;
  const query = new URLSearchParams(window.location.search);
  const back = query.has("paid") ? "paid" : query.has("cancelled") ? "cancelled" : "";
  // Back from Stripe. The address is tidied first, so a reload or a shared
  // link does not say it again.
  if (back) {
    window.history.replaceState(null, "", "/pass");
    renderedPath = "/pass";
    passStartedAgain = back === "cancelled";
  }
  // A claim being dropped is still this browser's until the server has the
  // request, and asked about before then it would be collected again.
  if (passForgetting) {
    await passForgetting;
  }
  const info = await loadPassInfo();
  if (visit !== passVisit || !passPageShown()) {
    return false;
  }
  const fresh = Boolean(passJustIssued && !passJustIssued.shown);
  if (selectors.length) {
    redrawPass(info, ...selectors);
  } else {
    drawPass(info);
  }
  announce("educhan Pass");
  if (fresh) {
    focusPassPanel("pass-issued");
  }
  if (back === "cancelled") {
    showNotice("Checkout cancelled. Nothing was charged.");
  } else if (back === "paid" && info.enabled && !info.claim && !passNoncePending(PASS_NONCE_STORAGE_KEY)) {
    showNotice(
      "There's nothing waiting to be collected in this browser. If you paid and don't have your code, write to the support address on the receipt Stripe emailed you."
    );
  }
  if (info.enabled && (info.claim || passNoncePending(PASS_NONCE_STORAGE_KEY)) && visit === passVisit) {
    // Not awaited: it goes on while the page is open, and the page is usable
    // meanwhile. A nonce left from a claim whose answer never arrived is
    // asked for again even with nothing left under the claim, since that
    // claim may have handed over a code.
    collectPass(visit, asked);
  }
  return true;
}

// keepClaimed stores what a claim or a reissue code brought, before anything
// else is done with it: the server hands a code over once, so it is kept even
// if the visitor has moved on to another page since asking. It returns what
// the page has to say about it: "issued" for a code this browser has not had
// before, "renewed" for a renewal, or "". A renewal is of the Pass its
// checkout was for, which may not be the one this browser holds now, so its
// expiry is not written onto the held Pass here (confirmRenewal).
function keepClaimed(res, reissued = false) {
  if (res.status === "renewed") {
    return "renewed";
  }
  if (res.status !== "issued") {
    return "";
  }
  const held = storedPass();
  const just = passJustIssued && passJustIssued.code === res.pass ? passJustIssued : null;
  if (!just && !(held && held.code === res.pass)) {
    const kept = storePass(res.pass, res.expires_at);
    passJustIssued = {
      code: res.pass,
      expires: res.expires_at,
      kept,
      replaced: kept && held ? held.code : "",
      reissued,
      shown: false,
    };
    return "issued";
  }
  // A code this browser has already, handed over again to a request whose
  // first answer was lost, with the expiry its Pass has now.
  if (held && held.code === res.pass) {
    storePass(res.pass, res.expires_at);
  }
  if (just) {
    just.expires = res.expires_at;
  }
  return "";
}

// recheckPass asks the server about the Pass this browser holds (held), as
// Check it again does, after something may have changed it there: a renewal
// collected here (confirmRenewal), or a claim or reissue code whose answer
// was lost (passAfterLostAnswer). Code checks are spaced out per address,
// which a campus shares, so one turned away for that is asked again once,
// after the wait the server names. What the server says is stored, unless the
// Pass checked left this browser while it was asked about (removed, or the
// browser forgotten, here or in another tab, or either still on its way):
// then nothing is stored back, and nothing removed in its name. It resolves
// to what it found, as state: "dated", with the expiry the server gives;
// "invalid", once the Pass is removed, with a code just handed over for it
// (forgetInvalidCode); "expired", kept as run out with the server's date;
// "changed", for a Pass that left meanwhile; or "" when it could not be
// checked.
async function recheckPass(held) {
  const forgets = passCodeForgets;
  let res = null;
  let failed = null;
  for (let attempt = 0; attempt < 2 && !res; attempt += 1) {
    try {
      res = await api("/api/pass/activate", { method: "POST", body: JSON.stringify({ pass: held.code }) });
    } catch (err) {
      failed = err;
      const seconds = err.status === 429 && err.payload && err.payload.retry_after;
      if (!seconds) {
        break;
      }
      await new Promise((resolve) => setTimeout(resolve, Math.min(seconds, 5) * 1000));
    }
  }
  const still = storedPass();
  if (passForgotten(forgets) || !still || still.code !== held.code) {
    return { state: "changed" };
  }
  if (res) {
    storePass(res.pass, res.expires_at);
    return { state: "dated", expires: res.expires_at };
  }
  const reason = failed.payload && failed.payload.pass;
  if (reason === "invalid") {
    forgetPass();
    forgetInvalidCode(held.code);
    passLost = `The Pass that was in this browser is no longer valid, so it has been removed. ${PASS_GONE_REASONS}`;
    return { state: "invalid" };
  }
  if (reason === "expired" && failed.payload.code === held.code) {
    storePass(held.code, failed.payload.expires_at);
    return { state: "expired" };
  }
  return { state: "" };
}

// confirmRenewal says what a renewal collected here, by a claim or by a
// reissue code (reissued), came to for the Pass this browser holds. The
// renewal is of the Pass its checkout was for, which may not be the one held
// here any more (another code typed in, or a reissue code used, since), so its
// expiry is never taken on trust: the held code is checked again
// (recheckPass), and the notice it resolves to says what is true of that Pass.
async function confirmRenewal(renewed, reissued = false) {
  const until = passUntilText(renewed.expires_at);
  const by = reissued ? "This reissue code" : "A payment from this browser";
  const held = storedPass();
  if (!held) {
    return `${by} renewed a Pass this browser doesn't have, until ${until}. Enter its code below to use it here.`;
  }
  const another = `${by} renewed a Pass until ${until}.`;
  const found = await recheckPass(held);
  if (found.state === "dated") {
    const checked = new Date(found.expires);
    if (checked.getTime() === new Date(renewed.expires_at).getTime() && !(held.expires && held.expires >= checked)) {
      return `Your Pass is renewed until ${passUntilText(found.expires)}.`;
    }
    return `${another} The Pass in this browser is good until ${passUntilText(found.expires)}.`;
  }
  if (found.state === "changed") {
    return another;
  }
  if (found.state === "invalid") {
    return `${another} The Pass that was in this browser is no longer valid, so it has been removed.`;
  }
  if (found.state === "expired") {
    return `${another} The Pass in this browser has run out.`;
  }
  return `${another} The Pass in this browser couldn't be checked just now, so the date shown for it may be out of date.`;
}

// passAfterLostAnswer checks the Pass this browser holds once a claim or a
// reissue code, asked again with a nonce kept from a try whose answer never
// arrived, is refused: that try may have renewed it, and a renewal hands over
// no code, so the server has nothing to answer it again with. What the server
// says of the held code is stored (recheckPass), and a date that moved on is
// said in words that do not say where the time came from, which the page
// cannot know. It resolves to that notice, to gone for a held Pass that is no
// longer valid, or to "" when there is none.
async function passAfterLostAnswer(gone = "The Pass that was in this browser is no longer valid, so it has been removed.") {
  const held = storedPass();
  if (!held) {
    return "";
  }
  const found = await recheckPass(held);
  if (found.state === "dated" && !(held.expires && held.expires >= new Date(found.expires))) {
    return `Your Pass is now good until ${passUntilText(found.expires)}.`;
  }
  if (found.state === "invalid") {
    return gone;
  }
  return "";
}

// refreshFailedPass asks educhan for the Pass page again after an answer from
// it, when the page on screen was drawn without one (GET /api/pass failed): it
// would otherwise go on saying educhan couldn't be reached, under what
// educhan has just answered. Something other than the reader sets the new
// drawing off, so it draws as redrawPass does: focus on a control it draws
// again stays there, without scrolling the page, and focus it drops, or that
// was on the body, goes to the first of selectors still on the page.
function refreshFailedPass(...selectors) {
  if (!lastPassInfo.failed) {
    return;
  }
  renderPass(...selectors).then((drawn) => {
    if (drawn && (!document.activeElement || document.activeElement === document.body)) {
      passFocus(...selectors);
    }
  });
}

// redrawPass draws the Pass page again, as drawPass does, for something that
// arrived while the reader was on it, and keeps a keyboard where it was: focus
// that was on the page stays on the same control when the drawing draws it
// again (passTwin), without scrolling the page, since it is where it was, and
// otherwise moves to the first of selectors still on the page (passFocus),
// what took its place, rather than falling to the top of the document with the
// element that had it. Focus drawPass kept in the code box stays there.
function redrawPass(info, ...selectors) {
  const focused = el.main.contains(document.activeElement) ? document.activeElement : null;
  const twin = focused ? passTwin(focused) : "";
  drawPass(info);
  if (focused && !el.main.contains(document.activeElement)) {
    const same = twin && el.main.querySelector(twin);
    if (same) {
      same.focus({ preventScroll: true });
    } else {
      passFocus(...selectors);
    }
  }
}

// showClaimed shows what a claim came to, if the visitor is still on the Pass
// page: a code just handed over, a renewal, with the notice confirmRenewal
// gave, the Pass held checked after a lost answer ("checked", with the notice
// passAfterLostAnswer gave), or the end of the collecting. It draws from what
// the page on screen was drawn from, so an answer to a request an earlier
// drawing sent is shown too. An answer that changes nothing only clears the
// note left by an attempt that failed. A notice that answers Check now
// (asked) is said in view (showPassNotice).
function showClaimed(claim, news, notice = "", asked = false) {
  if (!passPageShown()) {
    return;
  }
  const unseen = Boolean(passJustIssued && !passJustIssued.shown);
  if (!news && !unseen && claim === (lastPassInfo.claim === true)) {
    const note = document.getElementById("pass-collect-note");
    if (note) {
      note.textContent = "";
    }
    return;
  }
  redrawPass({ ...lastPassInfo, claim }, '[data-action="collect-pass"]', "#pass-held", "form[data-pass] input");
  if (news === "issued" || unseen) {
    focusPassPanel("pass-issued");
  }
  if (notice && asked) {
    showPassNotice(notice);
  } else if (notice) {
    showNotice(notice);
  }
}

// collectPass collects what this browser paid for. Stripe sends the buyer
// back before or after it tells educhan the payment went through, and a bank
// payment can take days, so "not yet" is asked again with a growing pause
// while the page stays open, and the next visit asks again. Every claim
// carries the collection's nonce, so one whose answer was lost is answered
// again with the same code. It stops when nothing is left to collect, or the
// page has been drawn again, or this browser has stopped collecting: then no
// claim is sent and no nonce drawn. An answer already on its way is drawn
// only if it brought something, and dropped unstored if this browser has
// stopped collecting since it was asked (passForgets). A claim refused when
// asked again with a nonce kept from an earlier one may have renewed the
// Pass held in an answer that never arrived, so that Pass is checked
// (passAfterLostAnswer). The first claim of a drawing Check now asked for
// (asked) answers that press, and what it comes to is said in view; later
// ones are the page's own news (showClaimed).
async function collectPass(visit, asked = false) {
  let wait = PASS_CLAIM_FIRST_WAIT_MS;
  for (;;) {
    if (passForgetting) {
      await passForgetting;
    }
    if (visit !== passVisit || !passPageShown()) {
      return;
    }
    let res = null;
    let failed = null;
    const forgets = passForgets;
    const again = passNoncePending(PASS_NONCE_STORAGE_KEY);
    try {
      res = await api("/api/pass/claim", {
        method: "POST",
        body: JSON.stringify({ nonce: passNonce(PASS_NONCE_STORAGE_KEY) }),
      });
    } catch (err) {
      failed = err;
    }
    // This browser stopped collecting while the claim was on its way.
    if (forgets !== passForgets) {
      return;
    }
    if (res) {
      const news = keepClaimed(res);
      if (res.claim !== true) {
        dropPassNonce(PASS_NONCE_STORAGE_KEY);
      }
      const notice = news === "renewed" ? await confirmRenewal(res) : "";
      if (forgets !== passForgets) {
        return;
      }
      if (visit === passVisit || news) {
        showClaimed(res.claim === true, news, notice, asked);
      }
      if (res.claim !== true) {
        return;
      }
    } else if (failed.status === 400 || failed.status === 403) {
      // Nothing left under the claim: collected in another tab, or by a
      // claim whose answer was lost, its checkout lapsed unpaid, or the claim
      // was dropped.
      dropPassNonce(PASS_NONCE_STORAGE_KEY);
      const notice = again ? await passAfterLostAnswer() : "";
      if (forgets !== passForgets) {
        return;
      }
      if (visit === passVisit || notice) {
        showClaimed(false, notice ? "checked" : "", notice, asked);
      }
      return;
    } else {
      // Unreachable, or a server error: asked again after the pause, with
      // the same nonce.
      const note = document.getElementById("pass-collect-note");
      if (note && visit === passVisit) {
        note.textContent = `${passErrorText(failed)} This page will ask again shortly.`;
      }
    }
    asked = false;
    await new Promise((resolve) => setTimeout(resolve, wait));
    wait = Math.min(Math.round(wait * 1.5), PASS_CLAIM_LONGEST_WAIT_MS);
  }
}

// codeCharacters is what was typed in the code box in capitals, with the
// separators the server skips (CODE_SEPARATORS) left out.
function codeCharacters(typed) {
  let bare = "";
  for (const c of typed.toUpperCase()) {
    if (!CODE_SEPARATORS.includes(c)) {
      bare += c;
    }
  }
  return bare;
}

// foldedCode is text with each letter the server reads as a digit written as
// that digit (CODE_FOLDS).
function foldedCode(text) {
  let folded = "";
  for (const c of text) {
    folded += CODE_FOLDS[c] ?? c;
  }
  return folded;
}

// isReissueCode says whether what was typed in the code box is a reissue code
// rather than a Pass code: it starts with the word, or, typed without it, has
// a reissue code's length once the separators the server skips are left out.
function isReissueCode(typed) {
  const bare = codeCharacters(typed);
  return bare.startsWith(REISSUE_CODE_WORD) || bare.length === REISSUE_CODE_LENGTH;
}

// codeGroups is text cut into groups of size characters.
function codeGroups(text, size) {
  const groups = [];
  for (let at = 0; at < text.length; at += size) {
    groups.push(text.slice(at, at + size));
  }
  return groups;
}

// canonicalCode is what was typed in the code box written as the server
// writes a code, which is how the page sends it: in capitals, with the
// separators left out (codeCharacters), the letters the server reads as
// digits written as those digits (foldedCode), and grouped with dashes, a Pass
// code in fives and a reissue code in fours after its word. The server folds
// a reissue code only after its word, which has an I of its own, and so does
// the page. What has neither length goes without its separators, for the
// server to refuse.
function canonicalCode(typed) {
  const bare = codeCharacters(typed);
  const word = bare.startsWith(REISSUE_CODE_WORD) ? REISSUE_CODE_WORD : "";
  const rest = foldedCode(bare.slice(word.length));
  if (rest.length === REISSUE_CODE_LENGTH) {
    return [REISSUE_CODE_WORD, ...codeGroups(rest, 4)].join("-");
  }
  if (bare.length === PASS_CODE_LENGTH) {
    return codeGroups(word + rest, 5).join("-");
  }
  return word + rest;
}

// redeemPassReissue spends a reissue code typed in the code box (typed, as
// canonicalCode writes it), which goes to the server in the body of a claim,
// with a nonce of its own. Pressing the button is the reader's say-so, so a
// Pass this browser holds is replaced at once, and its code is shown beside the
// new one (passIssuedHTML). Once an answer arrives the code leaves the box;
// without one it stays, so pressing again asks again with the same nonce and is
// handed the same code. The nonce goes only once an answer says the code issued
// or renewed a Pass: kept after a refusal too, it lets the right code, typed
// after a lost answer and then a mistyped retry, still ask with the nonce of
// the try whose answer was lost. Whether a try may have gone unanswered is
// marked beside it (PASS_REISSUE_UNANSWERED_STORAGE_KEY), and only then is a
// refusal taken for what may follow a code spent here. A renewal, which hands
// over no code, is refused when asked again, so the Pass held is checked then
// (passAfterLostAnswer). Asked for while a forget is on its way, it is sent
// once that is done (passForgetsSettled), and not at all if Remove the Pass or
// Forget this browser was pressed meanwhile, or Stop in a tab that had given up
// waiting for it, or the reader left the document meanwhile (pagehide); an
// answer that arrives after Remove or Forget, here or in another tab, is
// dropped unstored (passForgotten). What it comes to is said in view
// (showPassNotice). Stop collecting it here leaves it alone otherwise, but for
// what it says about the claim Stop dropped (passForgets).
async function redeemPassReissue(typed) {
  let res = null;
  let failed = null;
  const meanwhile = await passForgetsSettled();
  if (meanwhile) {
    if (passPageShown() && meanwhile !== PASS_LEFT_MEANWHILE) {
      showPassNotice(meanwhile);
    }
    return;
  }
  const forgets = passCodeForgets;
  const claims = passForgets;
  const again = passNoncePending(PASS_REISSUE_UNANSWERED_STORAGE_KEY);
  passNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);
  try {
    res = await api("/api/pass/claim", {
      method: "POST",
      body: JSON.stringify({ reissue: typed, nonce: passNonce(PASS_REISSUE_NONCE_STORAGE_KEY) }),
    });
  } catch (err) {
    failed = err;
  }
  if (passForgotten(forgets)) {
    return;
  }
  const answered = Boolean(res) || (!failed.unreachable && failed.status < 500);
  const news = res ? keepClaimed(res, true) : "";
  // An answer takes off the mark its own try put there, but not one an
  // earlier try left without an answer.
  if (res && (res.status === "issued" || res.status === "renewed")) {
    dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);
    dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);
  } else if (answered && !again) {
    dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);
  }
  if (answered) {
    forgetTypedCode();
  }
  let notice = "";
  if (news === "renewed") {
    notice = await confirmRenewal(res, true);
  } else if (failed && again && (failed.status === 400 || failed.status === 403)) {
    // A Pass held here that no longer works is what an earlier try would
    // leave that gave it a new code, which the right reissue code, asked
    // again with this nonce, hands over.
    notice = await passAfterLostAnswer(
      "The Pass that was in this browser no longer works, so it has been removed. An earlier try may have reached educhan and given it a new code: enter the reissue code again, checked against the one you were sent, to get that code. Otherwise the reissue code may be mistyped, used, run out or cancelled."
    );
  }
  if (passForgotten(forgets)) {
    return;
  }
  // Moved on meanwhile: what the answer brought is kept, and drawn when the
  // reader comes back.
  if (!passPageShown()) {
    return;
  }
  // The answer says whether anything waits under the claim as it was when
  // the code was asked about: once this browser has stopped collecting since,
  // here or in another tab, the page keeps what that left it saying.
  drawPass(res && claims === passForgets ? { ...lastPassInfo, claim: res.claim === true } : lastPassInfo);
  if (news === "issued") {
    focusPassPanel("pass-issued");
  } else {
    passFocus(res ? "#pass-held" : "#f-pass-code", "#pass-held", "form[data-pass] input");
  }
  if (answered) {
    refreshFailedPass("#pass-issued", "#pass-held", "form[data-pass] input");
  }
  if (res || notice) {
    if (notice) {
      showPassNotice(notice);
    }
    return;
  }
  if (failed.message === PASS_REISSUE_SPENT && again) {
    // An earlier try with this nonce went unanswered. It may have reached the
    // server, and its answer been lost, so it may have been this browser that
    // used the code; or not, and this one is mistyped, used, run out or
    // cancelled.
    showPassNotice(
      "This reissue code didn't work this time. If an earlier try reached educhan, it may have renewed your Pass already; otherwise check it against the one you were sent: it may be mistyped, used, run out or cancelled."
    );
  } else if (failed.message === PASS_REISSUE_SPENT) {
    // The server cannot tell a code mistyped into another well-formed one
    // from one used up, so the reader is asked to check it first.
    showPassNotice(
      "This reissue code didn't work. Check it against the one you were sent and enter it again. If it's right, it has been used, has run out or was cancelled: write to the support address on your Stripe receipt again if you still need one."
    );
  } else if (failed.unreachable || failed.status >= 500) {
    showPassNotice(`${passErrorText(failed)} The code is still in the box.`);
  } else {
    showPassNotice(passErrorText(failed));
  }
}

// passStillHeld says whether this browser still holds the Pass it held when a
// request about it was sent (before, which may be null): not removed, and not
// replaced by another code, here or in another tab. An answer about a Pass
// that has gone is dropped rather than stored back, and one that would act on
// the Pass held acts on nothing else.
function passStillHeld(before) {
  const now = storedPass();
  return (now ? now.code : "") === (before ? before.code : "");
}

// checkHeldPass asks the server about the Pass this browser holds, for one that
// looks expired here but may have been renewed on another device. Like a code
// typed in, the code goes in the body of /api/pass/activate, once no forget is
// on its way (passForgetsSettled), and not at all if Remove the Pass or Forget
// this browser was pressed meanwhile, or Stop in a tab that had given up
// waiting for it, or the reader left the document meanwhile (pagehide). What
// the answer says is stored only while this browser still holds that Pass and
// nothing has forgotten it (passForgotten), and drawn only while the reader is
// on the Pass page, with what it comes to said in view (showPassNotice).
async function checkHeldPass(button) {
  const held = storedPass();
  if (!held) {
    return;
  }
  button.disabled = true;
  const meanwhile = await passForgetsSettled();
  if (meanwhile) {
    button.disabled = false;
    if (passPageShown() && meanwhile !== PASS_LEFT_MEANWHILE) {
      refocus(button);
      showPassNotice(meanwhile);
    }
    return;
  }
  const forgets = passCodeForgets;
  let res = null;
  let failed = null;
  try {
    res = await api("/api/pass/activate", { method: "POST", body: JSON.stringify({ pass: held.code }) });
  } catch (err) {
    failed = err;
  }
  // Removed, replaced or forgotten while it was asked about, here or in
  // another tab: nothing is stored back, and the button that waited, or what
  // was drawn in its place, has focus back if nothing else took it and the
  // reader is still on the Pass page.
  if (passForgotten(forgets) || !passStillHeld(held)) {
    button.disabled = false;
    if (passPageShown()) {
      refocus(button);
    }
    return;
  }
  const reason = failed && failed.payload && failed.payload.pass;
  const expired = reason === "expired" && failed.payload.code === held.code;
  let kept = false;
  if (res) {
    kept = storePass(res.pass, res.expires_at);
  } else if (reason === "invalid") {
    forgetPass();
    forgetInvalidCode(held.code);
    passLost = `The Pass in this browser is no longer valid, so it has been removed. ${PASS_GONE_REASONS}`;
  } else if (expired) {
    // Still run out, now with the date the server has for it.
    storePass(held.code, failed.payload.expires_at);
  }
  // Moved on meanwhile: what the answer said is stored, and drawn when the
  // reader comes back.
  if (!passPageShown()) {
    button.disabled = false;
    return;
  }
  if (res) {
    drawPass(lastPassInfo);
    passFocus("#pass-held");
    showPassNotice(kept ? `Your Pass is good until ${passUntilText(res.expires_at)}.` : PASS_NOT_KEPT);
    refreshFailedPass("#pass-held");
  } else if (reason === "invalid") {
    drawPass(lastPassInfo);
    passFocus("form[data-pass] input");
    refreshFailedPass("form[data-pass] input");
  } else if (expired) {
    drawPass(lastPassInfo);
    passFocus('[data-action="check-pass"]', "#pass-held");
    showPassNotice(passRunOutText());
    refreshFailedPass('[data-action="check-pass"]', "#pass-held");
  } else {
    showPassNotice(passErrorText(failed));
    button.disabled = false;
    refocus(button);
  }
}

// submitPass drives the Pass page's three forms. Buying and renewing leave for
// Stripe's page by an ordinary navigation, so nothing of Stripe's ever loads
// here. A request asked for while a forget is on its way is sent once that is
// done (passForgetsSettled), and not at all if Remove the Pass or Forget this
// browser was pressed meanwhile, or Stop in a tab that had given up waiting for
// it, or the reader left the document meanwhile (pagehide); a checkout waits
// for any other this browser has on its way too, so the one pressed last holds
// the claim cookie, and is not sent either if Stop is pressed meanwhile, in
// this tab or another. An answer about a Pass that left this browser while it
// was awaited (removed, or replaced, here or in another tab) is dropped
// (passStillHeld), and so is any answer once Remove the Pass or Forget this
// browser has been pressed (passForgotten), and neither starts a checkout: one
// it opened is left behind (passStartedAgain), or, opened before a forget that
// came after the press, Stop's included, has its claim dropped too. A checkout
// still on its way when a forget is pressed in this tab, or the tab leaves the
// document, is called off (callOffPassCheckouts). One that arrives after the
// reader left the Pass page stores what it is still right to store, and draws
// nothing, and starts no checkout either. What a press comes to is said in view
// (showPassNotice).
async function submitPass(form) {
  const button = form.querySelector('button[type="submit"]');
  const label = button.textContent;
  const kind = form.dataset.pass;
  const held = storedPass();

  let request;
  if (kind === "activate") {
    // Sent as the server writes a code, whatever stood between its groups,
    // and with an O, I or L as the digit the server reads it as.
    const typed = canonicalCode(String(field(form, "pass").value || ""));
    // A reissue code is spent at once: typing it and pressing the button is
    // the reader's say-so to put its Pass in place of one held here.
    if (isReissueCode(typed)) {
      button.disabled = true;
      button.textContent = "Checking…";
      clearNotice();
      await redeemPassReissue(typed);
      button.disabled = false;
      button.textContent = label;
      if (passPageShown()) {
        refocus(button);
      }
      return;
    }
    // Another Pass's code typed over a good one held would put that one out
    // of this browser without a word.
    if (held && !held.expired) {
      showPassNotice("This browser has a Pass already. To use another one here, write this one's code down, then remove it.");
      return;
    }
    request = { path: "/api/pass/activate", body: { pass: typed } };
  } else {
    // The Pass this form renews has left this browser since it was drawn
    // (removed in another tab, say): a checkout now would buy a new Pass, not
    // renew the one the reader meant.
    if (kind === "renew" && !held) {
      drawPass(lastPassInfo);
      passFocus("form[data-pass] input");
      showPassNotice("The Pass you meant to renew is no longer in this browser. Enter its code below to renew it here.");
      return;
    }
    const adult = field(form, "adult");
    if (!(adult && adult.checked)) {
      showPassNotice(PASS_AGE_REFUSAL);
      if (adult) adult.focus();
      return;
    }
    // Not sent to pay for a Pass this browser could not collect or keep.
    if (!passCanBeKept()) {
      showPassNotice(PASS_NO_STORAGE);
      return;
    }
    // adult is true here, because an unticked box stopped above.
    const body = { adult: adult.checked };
    if (kind === "renew") {
      body.renew = held.code;
    }
    request = { path: "/api/pass/checkout", body };
  }

  button.disabled = true;
  button.textContent = kind === "activate" ? "Checking…" : "Opening Stripe…";
  clearNotice();
  const meanwhile = await passForgetsSettled(kind !== "activate");
  if (meanwhile) {
    button.disabled = false;
    button.textContent = label;
    if (passPageShown() && meanwhile !== PASS_LEFT_MEANWHILE) {
      refocus(button);
      showPassNotice(meanwhile);
    }
    return;
  }
  const forgets = passCodeForgets;
  const claims = passForgets;
  // A checkout is noted while it is on its way (openPassCheckout): the
  // browser's other checkouts wait for its answer, and until that answer is in,
  // a forget pressed here, or the tab's leaving the document, calls it off.
  const checkout = kind === "activate" ? null : openPassCheckout();
  const options = { method: "POST", body: JSON.stringify(request.body) };
  if (checkout) {
    options.signal = checkout.abort.signal;
    options.answered = () => {
      checkout.answered = true;
    };
  }
  let res = null;
  let failed = null;
  try {
    res = await api(request.path, options);
  } catch (err) {
    failed = err;
  }
  const calledOff = Boolean(checkout) && checkout.abort.signal.aborted;
  if (checkout) {
    closePassCheckout(checkout);
  }
  // A checkout opened is one left behind, as if the reader had left Stripe
  // without paying, unless they are sent to it below: the page then offers to
  // buy again and goes on collecting it underneath (passStartedAgain), rather
  // than drawing it as a payment Stripe has yet to confirm.
  if (res && kind !== "activate") {
    passStartedAgain = true;
  }
  // A checkout answered after Remove the Pass, Forget this browser or Stop
  // collecting it here was pressed, here or in another tab, is not followed to
  // Stripe, and the claim it set does not outlive that forget. Pressed here,
  // the forget went after the cookie that answer set, and dropped it with the
  // rest (callOffPassCheckouts); pressed in another tab, it may have gone too
  // soon to see it, so that claim is dropped at once, by the order the answer
  // names (forgetPassOrder). A Stop leaves any other answer alone, a code
  // checked or a checkout refused, as it leaves a reissue code.
  const dropped = Boolean(res) && kind !== "activate" && (claims !== passForgets || passForgotten(forgets));
  if (dropped && !checkout.forgotten) {
    forgetPassOrder(res.order, passForgetHeardLast);
  }
  // The forget that dropped it, or called it off, draws the page again once it
  // is settled, and a page left and brought back from the back-forward cache is
  // drawn afresh (the pageshow listener); until then the button that waited has
  // focus back, if nothing else took it and the reader is still on the Pass
  // page.
  if (calledOff || dropped || passForgotten(forgets) || !passStillHeld(held)) {
    button.disabled = false;
    button.textContent = label;
    if (passPageShown()) {
      refocus(button);
    }
    return;
  }
  const refusal = failed && failed.payload && failed.payload.pass;
  // A code typed in that has run out is kept, with the date the server has
  // for it, so the page can offer to renew it.
  const expired = kind === "activate" && refusal === "expired" && failed.payload.code;
  let kept = false;
  if (res && kind === "activate") {
    forgetTypedCode();
    kept = storePass(res.pass, res.expires_at);
  } else if (expired) {
    forgetTypedCode();
    kept = storePass(failed.payload.code, failed.payload.expires_at);
  } else if (kind === "renew" && refusal === "invalid") {
    forgetPass();
    forgetInvalidCode(held.code);
    passLost = "The Pass in this browser can no longer be renewed, so it has been removed. You can buy a new one.";
  }
  // Moved on meanwhile, to a board or the Verify page: what the answer said
  // is stored, and nothing is drawn over that page or opened in its place.
  if (!passPageShown()) {
    button.disabled = false;
    button.textContent = label;
    return;
  }
  if (res && kind === "activate") {
    drawPass(lastPassInfo);
    passFocus("#pass-held");
    showPassNotice(kept ? `This browser now has your Pass, good until ${passUntilText(res.expires_at)}.` : PASS_NOT_KEPT);
    refreshFailedPass("#pass-held");
    return;
  }
  if (res) {
    // Leaving for Stripe, with a checkout that is now this browser's to
    // collect. Coming Back to this page from there can bring it out of the
    // back-forward cache as it was left, button and all, so the pageshow
    // listener draws it again, collecting.
    passStartedAgain = false;
    window.location.assign(res.url);
    return;
  }
  if (expired) {
    drawPass(lastPassInfo);
    passFocus("#pass-held");
    showPassNotice(kept ? passRunOutText() : PASS_NOT_KEPT);
    refreshFailedPass("#pass-held");
    return;
  }
  // Sales were paused after this page was drawn.
  if (failed.status === 403 && failed.message === PASS_NOT_ON_SALE) {
    drawPass({ ...lastPassInfo, paused: true });
    passFocus("#pass-held");
    showPassNotice(PASS_PAUSED);
    return;
  }
  if (kind === "renew" && refusal === "invalid") {
    drawPass(lastPassInfo);
    passFocus("form[data-pass] input");
    showPassNotice(passErrorText(failed));
    return;
  }
  const refusedForAge = failed.status === 400 && failed.message === PASS_AGE_REFUSED;
  showPassNotice(refusedForAge ? PASS_AGE_REFUSAL : passErrorText(failed));
  button.disabled = false;
  button.textContent = label;
  refocus(button);
  if (!failed.unreachable && failed.status < 500) {
    refreshFailedPass("form[data-pass] input");
  }
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
          stored with one. When you post, ask for a badge code, start buying a Pass or check
          a Pass code, the server holds your IP address in its memory, and only there, so it
          can stop anyone doing any of those too often. Once it no longer needs it for that,
          it clears it out the next time it tidies up.</li>
        <li>educhan never asks for your email address unless you want a badge, and even then
          it keeps only a scrambled fingerprint of it, never the address itself
          (<a href="#faq-badge">more about the badge</a>). If you buy a Pass, Stripe's checkout
          asks for your email for the receipt, and educhan doesn't store it
          (<a href="#faq-pass">more about the Pass</a>).</li>
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
          with. On a shared computer, close educhan's other tabs and use <strong>Forget this
          browser</strong> on the <a href="/verify#forget-browser">Verify</a> page before you
          leave, or the next person gets your IDs in the threads you posted in, and your
          tripcode. A tab left open can keep them, and a post from it puts them back.</li>
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
      <p>On a shared computer, close educhan's other tabs and use <strong>Forget this
        browser</strong> on the <a href="/verify#forget-browser">Verify</a> page before you
        leave. The badge stays in that browser for whoever uses it next, and so do the key
        your IDs are made from and your name. A tab left open can keep all three, and a post
        from it puts the key and the name back.</p>`,
  },
  {
    id: "faq-pass",
    question: "What is the educhan Pass?",
    answer: `
      <p>An optional Pass that lets you post without solving the captcha, and helps pay for
        running educhan. It's paid for once, lasts a set time and doesn't renew by itself.
        Not every educhan site sells it; on one that does, the <a href="/pass">Pass</a> page
        says how long it lasts and what it costs. A site can also stop selling it for a while,
        and Passes already sold go on working meanwhile. It's sold only to people aged 18 and
        over, and you don't need one to read or to post.</p>
      <ul>
        <li>A Pass skips the captcha for a set number of posts an hour. Hours are counted in
          UTC, so in India, for one, each starts at half past. After that you solve the captcha
          as usual until the next hour starts, and everyone sharing one Pass shares that number.
          The rules and the wait between posts apply as usual.</li>
        <li>You buy it on the <a href="/pass">Pass</a> page, after ticking the box to confirm
          that you're 18 or over. educhan doesn't save your answer.</li>
        <li>You pay on Stripe's own page, which asks for your email for the receipt. Stripe
          keeps your payment details, such as your card, email, name and billing address, as
          any shop's payment company does, and educhan stores none of them. Whoever runs
          educhan can see in Stripe who bought a Pass, but never which posts it was used on:
          educhan keeps only a scrambled reference to the payment, so that a refund or a
          chargeback can take back the time that payment bought, and so that a lost code can
          be replaced from the payment on your receipt.</li>
        <li>You get a code, shown once, which your browser keeps. It's sent with your posts,
          and from the Pass page when you renew it or check it. Posts with the verified badge
          don't send it.</li>
        <li>Nothing on a post shows or records that a Pass was used, and educhan keeps no
          link from a Pass to any post.</li>
        <li>Keep a copy of the code: the Pass page can show it again while your browser has
          it. If you lose it, write to the support address on the receipt Stripe emailed you,
          and you'll be sent a reissue code. Enter it on the Pass page, and your Pass gets a
          new code there; the old one stops working. If the payment that bought the Pass was
          refunded or disputed, that may no longer be possible. A reissue code is never part
          of a link, so no browser's history keeps it.</li>
      </ul>
      <p>On a shared computer, remove the Pass on the <a href="/pass">Pass</a> page, or use
        <strong>Forget this browser</strong> on the <a href="/verify#forget-browser">Verify</a>
        page, before you leave. Either also stops the browser collecting a Pass bought there
        that hasn't arrived yet, even one whose checkout was still opening, so the next
        person can't collect it; if you've paid for one, write to the support address on the
        receipt, and you'll be sent a reissue code to collect it with. If you used
        <strong>Copy the code</strong> since you last opened educhan in the same tab, and have
        copied nothing in that tab since, either also empties the clipboard, whatever is on
        it by then, unless another tab removed or replaced the Pass meanwhile. They do so even
        after a copy in that tab that educhan can't see took anything, such as one from an
        email box in some browsers. A code copied any other way, by hand included, stays there
        until something else is copied.</p>`,
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

// beginLoading dims the current view and only falls back to a skeleton if the
// request is slow enough to notice, which keeps fast navigations from flashing.
function beginLoading() {
  el.main.setAttribute("aria-busy", "true");
  const timer = setTimeout(() => {
    el.main.innerHTML = '<div class="skeleton"><div class="skeleton-row"></div><div class="skeleton-row"></div><div class="skeleton-row"></div></div>';
  }, 250);
  return () => {
    clearTimeout(timer);
    el.main.removeAttribute("aria-busy");
  };
}

// resolvePost finds the thread a board-wide post number belongs to, so a link
// can name a post — a cross-board quote, say — without knowing where it lives.
async function resolvePost(route) {
  const found = await api(`/api/boards/${encodeURIComponent(route.slug)}/posts/${route.no}`);
  return `${threadPath(found.board || route.slug, found.thread_no)}#p${found.no}`;
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
  const requested = window.location.href;
  renderedPath = window.location.pathname + window.location.search;
  const route = parseRoute();
  // What the Pass page had to say goes once the reader leaves it, with what
  // was typed in its code box and a code just handed over that it has drawn,
  // kept or not (forgetShownCode): one this browser could not keep is nowhere
  // else, and its panel says it won't be shown again, so the next person at a
  // shared computer can't open the Pass page to find it. One the page has not
  // drawn yet, because the reader left before its drawing came, stays to be
  // drawn when they come back, since the server will not hand it over again.
  if (state.route.view === "pass" && route.view !== "pass") {
    passLost = "";
    forgetTypedCode();
    forgetShownCode();
  }
  state.route = route;
  state.thread = null;
  renderNav(route);
  clearNotice();
  hideQuotePreview();
  // Whatever was expanded belongs to the view being replaced.
  expandedLink = null;

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

  const finish = beginLoading();
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
      case "pass":
        await renderPass();
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
    el.main.innerHTML = emptyStateHTML(message, board);
    announce(notFound ? "Not found" : "Could not load this page");
    return;
  } finally {
    finish();
  }

  if (resolved !== null) {
    // The history entry is replaced rather than added: /post/N is an address,
    // not a page, and Back should not land on it only to bounce forward again.
    // Skipped if the reader went somewhere else while the number was looked up.
    if (window.location.href === requested) {
      window.history.replaceState(null, "", resolved);
      await render({ draft });
    }
    return;
  }
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
function captureDraft() {
  const form = el.main.querySelector("form[data-kind]");
  if (!form) {
    // A reply the thread closed on is carried as far as its text goes.
    const unsent = el.main.querySelector("textarea[data-unsent]");
    return unsent ? { key: routeKey(state.route), open: true, body: unsent.value } : null;
  }
  const image = field(form, "image");
  const details = form.closest("details");
  const value = (name) => {
    const node = field(form, name);
    return node ? node.value : "";
  };
  const sage = field(form, "sage");
  const badge = field(form, "badge");
  const spoiler = field(form, "spoiler");
  return {
    key: routeKey(state.route),
    open: details ? details.open : true,
    subject: value("subject"),
    name: value("name"),
    body: value("body"),
    sage: sage ? sage.checked : false,
    badge: badge ? badge.checked : false,
    file: image && image.files ? image.files[0] : null,
    spoiler: spoiler ? spoiler.checked : false,
  };
}

function restoreDraft(draft) {
  if (!draft || draft.key !== routeKey(state.route)) {
    return;
  }
  const form = el.main.querySelector("form[data-kind]");
  if (!form) {
    keepUnsentDraft(draft);
    return;
  }
  // An empty field is restored as empty: a Name the poster cleared stays
  // cleared, rather than the form's remembered name coming back.
  for (const name of ["subject", "name", "body"]) {
    const node = field(form, name);
    if (node && name in draft) {
      node.value = draft[name];
    }
  }
  const sage = field(form, "sage");
  if (sage) sage.checked = draft.sage;
  const badge = field(form, "badge");
  if (badge) badge.checked = draft.badge;

  // A form with no image field, on a site that takes no images, gets no file
  // back; a switched-off field refuses it in setFile and says why.
  const fileField = form.querySelector("[data-file-field]");
  if (fileField && draft.file && setFile(fileField, draft.file)) {
    // Only alongside the file it was ticked for.
    const spoiler = field(form, "spoiler");
    if (spoiler) spoiler.checked = draft.spoiler;
  }
  const details = form.closest("details");
  if (details && (draft.open || draft.body || draft.file)) {
    details.open = true;
  }
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

function quoteTarget() {
  return el.main.querySelector('form[data-kind="reply"] textarea[name="body"]');
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
  // browser held it, so there is nothing there to tell.
  if (event.target.closest('[data-action="forget-verification"]')) {
    event.preventDefault();
    forgetCredential();
    verifyStep = { step: "email", label: "" };
    renderVerify();
    return;
  }

  // Like the badge, removing a Pass is local: the server never knew which
  // browser held it. The page is drawn again at once, so the code is off the
  // screen before the visitor walks away, with whatever was typed in the code
  // box and what the page said about a Pass that went before, and the
  // clipboard is emptied while the click allows it if this tab copied the
  // code. What is still being collected here is dropped too, which only the
  // server can do. If educhan can't be reached for that, the notice points to
  // Forget this browser, not to Stop collecting it here as PASS_ORDER_KEPT
  // does: what is left may be paid for, and the Pass page collects it as soon
  // as it is opened, before Stop can be pressed.
  if (event.target.closest('[data-action="forget-pass"]')) {
    event.preventDefault();
    forgetPass();
    passJustIssued = null;
    passLost = "";
    forgetTypedCode();
    emptyCopiedPassCode();
    const dropped = forgetPassClaim("remove");
    drawPass({ ...lastPassInfo, claim: false });
    passFocus("form[data-pass] input");
    showPassNotice("This browser no longer has a Pass.");
    dropped.then((done) => {
      if (!done && passPageShown()) {
        showPassNotice(
          "This browser no longer has a Pass, but educhan couldn't be reached to stop it collecting anything bought here that hasn't arrived yet. Use Forget this browser on the Verify page when you're back online."
        );
      }
    });
    return;
  }

  // Dropping a purchase still being collected waits for the server: until it
  // answers, the claim is still this browser's, so Check now waits with it.
  // If educhan can't be reached, the claim is still this browser's, and the
  // page goes on collecting it, as the panel and the notice say.
  const stopClaim = event.target.closest('[data-action="stop-pass-claim"]');
  if (stopClaim) {
    event.preventDefault();
    const waiting = [stopClaim, ...el.main.querySelectorAll('[data-action="collect-pass"]')];
    for (const button of waiting) {
      button.disabled = true;
    }
    const dropped = forgetPassClaim("stop");
    const visit = passVisit;
    dropped.then((done) => {
      // And those drawn meanwhile, which drawPass left off.
      for (const button of [...waiting, ...el.main.querySelectorAll(PASS_COLLECT_BUTTONS)]) {
        button.disabled = false;
      }
      // Unless the page has been drawn again since, which collects afresh.
      if (!done && visit === passVisit) {
        collectPass(visit);
      }
      if (done) {
        passStartedAgain = false;
      }
      // Moved on meanwhile: the page they come back to is drawn afresh.
      if (!passPageShown()) {
        return;
      }
      if (!done) {
        refocus(stopClaim);
        showPassNotice("Couldn't reach educhan, so this browser is still collecting it. Try again when you're back online.");
        return;
      }
      drawPass({ ...lastPassInfo, claim: false });
      passFocus("form[data-pass] input", "#pass-held");
      showPassNotice(
        "This browser has stopped collecting it. If it's paid, write to the support address on the receipt Stripe emailed you, and you'll be sent a reissue code to collect it with."
      );
    });
    return;
  }

  // Each Copy the code copies the code in its own panel: the held Pass's,
  // shown, and a code just handed over can be on the page together.
  if (event.target.closest('[data-action="copy-pass"]')) {
    event.preventDefault();
    const code = event.target.closest(".verify-panel")?.querySelector(".pass-code");
    if (code && navigator.clipboard) {
      // Noted only once the code is on the clipboard: a copy the browser
      // refused leaves the reader's own clipboard, which Remove the Pass then
      // has no business emptying.
      const copying = code.textContent.trim();
      navigator.clipboard.writeText(copying).then(
        () => {
          passCodeCopied = copying;
          if (passPageShown()) {
            showPassNotice("Copied. Paste it somewhere safe.");
          }
        },
        () => {
          if (passPageShown()) {
            showPassNotice("This browser wouldn't copy it. Select the code and copy it by hand.");
          }
        }
      );
    } else {
      showPassNotice("This browser can't copy from here. Select the code and copy it by hand.");
    }
    return;
  }

  const showPass = event.target.closest('[data-action="show-pass"]');
  if (showPass) {
    event.preventDefault();
    showPassCode(showPass);
    return;
  }

  const checkPass = event.target.closest('[data-action="check-pass"]');
  if (checkPass) {
    event.preventDefault();
    checkHeldPass(checkPass);
    return;
  }

  // Asking now draws the page again, which starts collecting afresh, and what
  // that drawing's first claim comes to is said in view (passAsked). Focus
  // moves only if this drawing happened: not once the reader has left the
  // page, or a newer drawing has taken its place.
  if (event.target.closest('[data-action="collect-pass"]')) {
    event.preventDefault();
    passAsked = true;
    renderPass().then((drawn) => {
      if (drawn) {
        passFocus('[data-action="collect-pass"]', "#pass-issued", "#pass-held", "form[data-pass] input");
      }
    });
    return;
  }

  // Starting again offers the forms back, and collecting goes on underneath.
  if (event.target.closest('[data-action="restart-pass"]')) {
    event.preventDefault();
    passStartedAgain = true;
    drawPass(lastPassInfo);
    passFocus("form[data-pass] input");
    return;
  }

  if (event.target.closest('[data-action="forget-browser"]')) {
    event.preventDefault();
    const dropped = forgetBrowser();
    verifyStep = { step: "email", label: "" };
    Promise.all([renderVerify(), dropped]).then(([, done]) => {
      showNotice(
        done
          ? "This browser no longer holds your post key, your name, a badge, a Pass or a Pass bought here and still being collected."
          : "This browser no longer holds your post key, your name, a badge or a Pass. educhan couldn't be reached to stop it collecting a Pass bought here that hasn't arrived yet, if there is one, so press Forget this browser again when you're back online."
      );
    });
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

// A page brought back from the back-forward cache is shown as it was left,
// without running anything, and heard no storage event meanwhile, so it looks
// for forgets sent or settled while it was away, and stops for those still on
// their way, before it draws anything (heedPassForgetsUnderWay). The Pass page
// left for Stripe with its button disabled and its state from before the
// checkout, so it is drawn again, by render, which asks the age screen first: a
// page left on the screen, or on its refusal, comes back to the same, and
// nothing is fetched behind it.
window.addEventListener("pageshow", (event) => {
  if (event.persisted) {
    heedPassForgetsUnderWay();
  }
  if (event.persisted && state.route.view === "pass") {
    render();
  }
});

// Leaving the document, for Stripe say, takes what was typed in the Pass page's
// code box with it, and a code just handed over, kept or not, once drawn, as
// leaving the page within the site does (render), and the panel of one still on
// the page, even one whose code another tab's forget has dropped already, so
// the back-forward cache does not bring them back (forgetShownCode). A checkout
// still on its way is called off, since the reader has left it
// (callOffPassCheckouts), and so is a press still waiting to go (passLeft,
// passForgetsSettled), which would otherwise go once the page is brought back.
window.addEventListener("pagehide", () => {
  passLeft += 1;
  forgetTypedCode();
  forgetShownCode();
  callOffPassCheckouts(false);
});

document.addEventListener("copy", forgetCopiedPassCode);
document.addEventListener("cut", forgetCopiedPassCode);

// followPassStorage follows a change another tab of this browser made to the
// Pass it holds (the storage event reaches every tab but the one that made
// it): removed there, by Remove the Pass or Forget this browser say, or
// replaced by another code. A code just handed over here stops being drawn
// once the Pass is removed, kept or not, so a Pass forgotten in another tab is
// not left on this tab's screen, and any code it took the place of goes with
// it, whatever removed the Pass: one educhan found no longer valid there takes
// such a code too, though that code may still work, where the tab that found
// it keeps it on screen (forgetInvalidCode). Another code stored there leaves
// it be, kept or not: one this browser kept is drawn as no longer here
// (passIssuedHTML), beside any code it took the place of, until the reader
// leaves the Pass page, since neither may be anywhere else, and one it could
// not keep was never the Pass it holds. Remove and Forget reach such a code by
// their mark too (stopForPassForget), which also needs the browser to store:
// one that stores nothing at all tells this tab nothing, and a code here then
// stays until the reader leaves the Pass page. A code this tab copied that the
// browser no longer holds no longer counts as its to empty from the clipboard.
// The Pass page, if it is on screen here, is drawn again from what this
// browser holds now, which also hides a code shown on it.
function followPassStorage(event) {
  if (event.key !== PASS_STORAGE_KEY && event.key !== null) {
    return;
  }
  const now = storedPass();
  if (!now) {
    passJustIssued = null;
  }
  if (!now || now.code !== passCodeCopied) {
    passCodeCopied = "";
  }
  if (passPageShown() && el.main.querySelector("[data-pass-page]")) {
    redrawPass(lastPassInfo, "#pass-held", "form[data-pass] input");
  }
}

// followPassForget follows Remove the Pass, Forget this browser or Stop
// collecting it here pressed in another tab of this browser, by the mark that
// tab keeps under PASS_FORGET_STORAGE_KEY while its request is on the way
// (forgetPassClaim), and stops this tab as that one did (stopForPassForget).
// Once the wait ends, the Pass page, if the reader is on it here, is drawn
// again from the server, even if the mark came while it was still loading, and
// drew nothing, keeping a keyboard where it was (redrawPass): focus on the page
// just before the drawing, on a button given back meanwhile say, stays on the
// same control when the drawing draws it again, and otherwise goes to what
// took its place, and focus on the body or outside the page stays there. The
// drawing leaves off what Remove and Forget dropped, a code just handed over
// included.
function followPassForget(event) {
  const key = event.key;
  if (!key || !key.startsWith(`${PASS_FORGET_STORAGE_KEY}:`)) {
    return;
  }
  const mark = event.newValue;
  if (!mark) {
    const heard = passForgetHeard.get(key);
    if (heard) {
      heard();
    }
    return;
  }
  if (passForgetWaits.has(key)) {
    return;
  }
  stopForPassForget(key, mark).then(() => {
    // Not over the age screen: until it is answered, render shows that in
    // the Pass page's place.
    if (passPageShown() && ageStatus() === "ok") {
      renderPass('form[data-pass] button[type="submit"]', "#pass-held", "form[data-pass] input");
    }
  });
}

window.addEventListener("storage", followPassStorage);
window.addEventListener("storage", followPassForget);
window.addEventListener("storage", keepPassCheckoutMark);
heedPassForgetsUnderWay();
// A checkout's mark past its wait is one a tab left behind, crashed or killed
// before its answer came, and goes as the page loads (passCheckoutLeft).
passCheckoutLeft();

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
  }
  if (event.target.matches('input[type="checkbox"][name="badge"]')) {
    badgeToggled(event.target);
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
  // A blocked picker is not a drop target; the drop itself says why.
  if (field && !field.classList.contains("blocked")) {
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
  setFile(field, event.dataTransfer.files[0]);
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
  } catch {
    // Kept for this page only.
  }
  return pagePosterKey;
}

/* Submitting --------------------------------------------------------------- */

el.main.addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = event.target;
  if (form.dataset.verify) {
    await submitVerification(form);
    return;
  }
  if (form.dataset.pass) {
    await submitPass(form);
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

  // The one request in the application that carries the credential. It goes
  // out only because this box is ticked, so an anonymous post is anonymous at
  // the request layer too, not merely in what it displays.
  const wantsBadge = data.get("badge") === "on";
  const held = wantsBadge ? state.verification.badge : null;
  if (wantsBadge && !held) {
    showNotice("Your badge is no longer available in this browser — verify again to post with it.");
    return;
  }
  // A badged post carries the credential and not the poster key. The server
  // derives a badged post's ID from the credential, so the key would do
  // nothing there, and sending the two together would put this browser's key
  // beside its credential in one request — the only place they could meet.
  const headers = held ? { [CREDENTIAL_HEADER]: held.token } : { [POSTER_KEY_HEADER]: posterKey() };
  // The Pass rides on unbadged posts only, for the same reason: a badged post
  // is the credential's request, and a payment and an institution never meet
  // in one. An expired Pass stays for renewing but is not sent.
  const pass = held ? null : storedPass();
  if (pass && !pass.expired) {
    headers[PASS_HEADER] = pass.code;
  }

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

  // A post with an image goes as multipart; text-only posts stay on JSON.
  const requestBody = file ? data : JSON.stringify(payload);

  const label = button.textContent;
  button.disabled = true;
  button.textContent = file ? "Uploading…" : "Posting…";
  clearNotice();
  try {
    const created = await api(path, { method: "POST", body: requestBody, headers });
    // The remembered name is the one for unbadged posts; a badged post's name
    // is never offered on the next form, where the badge starts unticked.
    if (!held) {
      storageSet(NAME_STORAGE_KEY, name);
    }
    // Clear before navigating so the draft carried across the re-render is the
    // empty form rather than the post that just went through. reset() puts the
    // Name back to what the form was drawn with, so it is set again: to the
    // name just used — empty, if the poster cleared it — or after a badged
    // post to the remembered one.
    form.reset();
    const nameField = field(form, "name");
    if (nameField) {
      nameField.value = held ? storageGet(NAME_STORAGE_KEY) || "" : name;
    }
    const fileField = form.querySelector("[data-file-field]");
    if (fileField) {
      clearFileField(fileField);
    }
    // A Pass the server would not take was treated as no Pass, and the answer
    // says why, so this browser stops sending it: one it does not know is
    // forgotten, with a code just handed over for it that the Pass page has
    // yet to draw (forgetInvalidCode), and one that has run out is kept,
    // marked as run out with no date (this browser's was wrong), so the Pass
    // page can still offer to renew it or check it again, and says it has run
    // out. The post itself went out as any other. Only the Pass that went with
    // the post is acted on: one removed since, or replaced by another code,
    // here or in another tab, is left as it is now.
    let passNotice = "";
    const sent = pass && passStillHeld(pass) ? pass : null;
    if (sent && created.pass === "invalid") {
      forgetPass();
      forgetInvalidCode(sent.code);
      passLost = `The Pass that was in this browser is no longer valid. ${PASS_GONE_REASONS}`;
      passNotice = `Your post was sent, but the Pass in this browser is no longer valid, so it has been removed. ${PASS_GONE_REASONS}`;
    } else if (sent && created.pass === "expired") {
      markPassExpired(sent.code);
      passNotice = "Your post was sent, but the Pass in this browser has run out. The Pass page says whether it can be renewed.";
    }
    await navigate(`${threadPath(created.board, created.thread_no)}#p${created.no}`);
    if (passNotice) {
      showNotice(passNotice);
    }
  } catch (err) {
    // A credential the server will not honour again is dropped rather than
    // offered for the next post; the response says which case this is. The
    // option goes with it — taking the checkbox out of this form rather than
    // re-rendering leaves the half-written post exactly where it is — so the
    // retry that follows is an ordinary post instead of the same refusal.
    const reason = err.payload && err.payload.credential;
    if (reason === "expired" || reason === "revoked") {
      forgetCredential();
      badgeLost =
        reason === "expired"
          ? "Your badge has expired."
          : "That badge was replaced by a newer verification.";
      const box = field(form, "badge");
      const option = box && box.closest("label");
      if (option) {
        option.remove();
        form.querySelector("#f-badge-hint")?.remove();
      } else if (box) {
        box.checked = false;
      }
    }
    // The cap was reached after this form was drawn: take the image off and
    // switch the picker off, so the next press posts the text that is left.
    if (err.status === 403 && err.message === IMAGE_LIMIT_REACHED) {
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
    // The site takes no images, which this page was drawn without knowing:
    // /api/site did not answer, or the server has been switched over since.
    // The image comes off the same way, and the page knows for the rest of
    // the visit, so the next form it draws has no image field.
    if (err.status === 403 && err.message === UPLOADS_SWITCHED_OFF) {
      state.site.uploads = false;
      const fileField = form.querySelector("[data-file-field]");
      if (fileField) {
        blockFileField(fileField, UPLOADS_OFF_REASON);
      }
      redrawThreadCounts();
    }
    showNotice(errorMessage(err, { hasText: body.trim() !== "" }));
    button.disabled = false;
    button.textContent = label;
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

  button.disabled = true;
  button.textContent = requesting ? "Sending…" : "Verifying…";
  clearNotice();
  try {
    if (requesting) {
      const sent = await api("/api/verify/request", {
        method: "POST",
        // adult is true here, because an unticked box stopped above.
        body: JSON.stringify({ email: String(data.get("email") || ""), adult: adult.checked }),
      });
      verifyStep = { step: "code", label: sent.label || "" };
      el.main.innerHTML = verifyViewHTML();
      const input = document.getElementById("f-code");
      if (input) input.focus();
      return;
    }
    // The one time the credential is ever handed out. It is kept here rather
    // than set as a cookie, so nothing sends it until a post asks for a badge.
    const issued = await api("/api/verify/confirm", {
      method: "POST",
      body: JSON.stringify({ code: String(data.get("code") || "") }),
    });
    storeCredential(issued.credential);
    verifyStep = { step: "email", label: "" };
    await renderVerify();
  } catch (err) {
    // The server's refusal of a request without the attestation reads the same
    // as the page's own.
    const refusedForAge = err.status === 400 && err.message === BADGE_AGE_REFUSED;
    showNotice(refusedForAge ? BADGE_AGE_REFUSAL : errorMessage(err, { action: "code" }));
    button.disabled = false;
    button.textContent = label;
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
    // And whether to offer an image at all, and how large. Like the board
    // list it says nothing about the reader, and nothing about the reader is
    // sent for it. Without an answer the forms stay as they always were.
    api("/api/site")
      .then((site) => {
        const max = Number(site.max_upload_bytes);
        state.site = { uploads: site.uploads !== false, maxUploadBytes: max > 0 ? max : 0, pass: site.pass === true };
      })
      .catch(() => {}),
  ]);
  state.boards = boards;
  siteLoaded = true;
}

render();
