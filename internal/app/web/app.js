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
  "api", "media", "healthz", "verify", "mod", "u", "rules", "faq", "legal",
  "terms", "privacy", "dmca", "report", "archive",
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
// without taking an old outcome for a new one.
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
  // enabled comes from the server; badge is read out of the credential this
  // browser holds, which the server is never asked about outside of using it.
  verification: { enabled: false, badge: null },
};

const el = {
  main: document.getElementById("main"),
  nav: document.getElementById("board-nav"),
  // The links to the site's own pages — Verify in the header, Rules and FAQ in
  // the footer — each named by the view it opens.
  pageLinks: document.querySelectorAll("a[data-page]"),
  notice: document.getElementById("notice"),
  status: document.getElementById("route-status"),
};

/* API ---------------------------------------------------------------------- */

async function api(path, options = {}) {
  // The caller's headers are copied rather than used in place: this function
  // adds to them, and a caller passing the same object twice should not
  // accumulate whatever the first call decided.
  const init = { ...options, headers: { ...(options.headers || {}) } };
  // FormData sets its own multipart content type, boundary included.
  if (init.body !== undefined && !(init.body instanceof FormData)) {
    init.headers["Content-Type"] = "application/json";
  }

  const res = await fetch(path, init);
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
function errorMessage(err) {
  if (err.status === 413) {
    return "That image is too large for this board. Try a smaller file.";
  }
  if (err.status === 429) {
    const seconds = err.payload && err.payload.retry_after;
    return seconds ? `Flood control: wait ${plural(seconds, "second", "seconds")} and post again.` : err.message;
  }
  // The form stays on screen after a refusal, so these say what happened and
  // leave the half-written post where it is.
  if (err.status === 403 && err.message === "image limit reached") {
    return "This thread reached its image limit while you were writing, so the image has been taken off your reply. Post again to send the text on its own.";
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
      // be able to scroll it.
      out.push(`<pre class="code" tabindex="0"><code>${esc(blockSource(source, node))}</code></pre>`);
      ctx.lineStart = true;
      return;
    }
    if (node.kind === "eqn") {
      out.push(mathHTML(blockSource(source, node), { display: true }));
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
// focusable for a keyboard to scroll it.
function mathHTML(tex, { display }) {
  return display
    ? `<div class="math math-display" tabindex="0" data-tex="${esc(tex)}">${esc(tex)}</div>`
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

const HIGHLIGHTER_SRC = "/vendor/highlightjs/highlight.min.js";
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

function highlightCode(root) {
  const blocks = Array.from(root.querySelectorAll("pre.code > code:not([data-highlighted])"));
  if (blocks.length === 0) {
    return;
  }
  loadHighlighter()
    .then((hljs) => highlightBlocks(hljs, blocks))
    .catch(() => {
      // The blocks keep their plain rendering.
    });
}

function highlightBlocks(hljs, blocks) {
  const deadline = performance.now() + HIGHLIGHT_SLICE_MS;
  while (blocks.length > 0) {
    if (performance.now() > deadline) {
      setTimeout(() => highlightBlocks(hljs, blocks), 0);
      return;
    }
    const code = blocks.shift();
    // A re-render may have replaced the block since it was queued, and an
    // earlier pass may already have done it; highlight.js complains about both.
    if (!code.isConnected || code.dataset.highlighted) {
      continue;
    }
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
}

/* Maths -------------------------------------------------------------------- */

const KATEX_SRC = "/vendor/katex/katex.min.js";
const KATEX_CSS = "/vendor/katex/katex.min.css";

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
    // Focusable while it scrolls, for a keyboard to scroll it.
    if (wide[n]) {
      block.tabIndex = 0;
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
function disabledCommands(katex) {
  const macros = {};
  for (const name of KATEX_DISABLED) {
    macros[name] = () => {
      throw new katex.ParseError(`${name} is not available in posts`);
    };
  }
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
// tree, its links rejoin the tab order, and it stops being a button. It stays
// focusable (tabindex -1) so focus is not dropped when it was the thing focused.
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

// Registered ahead of the page's own click handler, which skips any event this
// one has handled. A tap on a hidden spoiler only reveals it: following a link
// inside it would follow one the reader has not seen. A mouse click reveals it
// too, but the hover already showed it, so a link inside works as normal.
document.addEventListener("click", (event) => {
  const spoiler = event.target instanceof Element ? event.target.closest(".spoiler:not(.revealed)") : null;
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
  if (target instanceof Element && target.matches(".spoiler:not(.revealed)")) {
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

function revealSpoiler(button) {
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

function badgesHTML(thread) {
  const badges = [];
  if (thread.sticky) badges.push('<span class="badge sticky">sticky</span>');
  if (thread.archived_at) badges.push('<span class="badge archived">archived</span>');
  if (thread.locked) badges.push('<span class="badge locked">locked</span>');
  if (thread.at_bump_limit) badges.push('<span class="badge bumplimit">bump limit</span>');
  if (thread.at_image_limit) badges.push('<span class="badge imagelimit">image limit</span>');
  return badges.join(" ");
}

// postHTML renders one post. `context` carries the board slug, the thread the
// post belongs to, and whether the post number should act as a quote button.
function postHTML(post, context) {
  const { slug, subject = "", thread = null, quotable = false } = context;
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
    const badges = badgesHTML(thread);
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
  return `
    <div class="file-field${blocked ? " blocked" : ""}" data-file-field>
      <input class="file-input" type="file" id="${FIELD_IDS.image}" name="image"
             accept="${ACCEPTED_IMAGES}" aria-labelledby="f-image-label"
             aria-describedby="f-image-blocked"${blocked ? " disabled" : ""} />
      <label class="btn secondary file-button" for="${FIELD_IDS.image}">Choose image</label>
      <span class="file-hint">or drop one on this form &middot; JPEG, PNG, WebP, GIF</span>
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
    return `<p class="archived-note"><span class="badge archived">archived</span>
      <span>Archived threads take no new replies. To carry on the conversation,
        <a href="${boardPath(slug)}">start a new thread on ${esc(boardLabel(slug))}</a>.</span></p>`;
  }
  if (thread && thread.locked) {
    return `<p class="locked-note"><span class="badge locked">locked</span> This thread is locked; no new replies.</p>`;
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
  const badge = held
    ? `<label class="sage"><input type="checkbox" name="badge" /> post with ${verifiedBadgeHTML(held.label)}</label>`
    : "";

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
          <textarea id="${FIELD_IDS.body}" name="body" maxlength="8000"
                    placeholder="${isThread ? "What's on your mind?" : "Write a reply. Use >>123 to quote a post."}"></textarea>
        </div>
        <div class="field">
          <span class="label" id="f-image-label">Image (optional)</span>
          ${fileFieldHTML(thread && thread.at_image_limit ? imageLimitReason(thread.board) : "")}
        </div>
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

// imageLimitReason is why a thread's reply form takes no image.
function imageLimitReason(board) {
  const limit = board && board.image_limit;
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
  const { board } = index;

  const threads = index.threads
    .map((summary) => {
      const context = { slug: board.slug, subject: summary.subject, thread: summary };
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
      // place here; it stays a plain tile and opens in the thread.
      let media = `<div class="tile-media empty-media">no image</div>`;
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
          ${badgesHTML(t)}
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

  const posts = detail.posts
    .map((post) =>
      postHTML(post, {
        slug: board.slug,
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
      <span class="muted">${threadCounts(detail, board, archived).join(" &middot; ")}</span>
    </div>
    ${archiveNote}
    <section class="thread detail">${posts}</section>
    ${formHTML({ kind: "reply", slug: board.slug, thread: detail, open: true })}`;
  announce(`${detail.subject || `Thread No. ${detail.no}`} — ${boardLabel(board.slug)}`);
}

// threadCounts is the thread view's summary line. The image count carries the
// board's cap, because the cap is what a poster with an image needs to know.
function threadCounts(thread, board, archived) {
  const images = Number(thread.images) || 0;
  const counts = [
    plural(thread.replies, "reply", "replies"),
    board.image_limit > 0 ? `${images} of ${plural(board.image_limit, "image", "images")}` : plural(images, "image", "images"),
  ];
  if (!archived && thread.at_bump_limit) counts.push("at bump limit, no longer bumping");
  if (!archived && thread.at_image_limit) counts.push("image limit reached, text replies only");
  return counts;
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
  return state.verification;
}

// The claim the page is making, in the words the README uses. It belongs on
// screen next to the form, not only in the repository.
const VERIFY_INTRO = `
  <p class="verify-note">
    Verifying proves you hold an address at a recognised institution and earns
    you an optional badge. The address itself is never stored: what is kept is
    a keyed hash of it, in a table that has no link to any post or thread. The
    badge you get carries only the institution and an expiry, so no one — this
    server included — can work back from a post to your address.
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
      <p class="empty">Verification is not configured on this server.</p>`;
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
            proves your institution.</p>
          <p class="muted">Forgetting is final: the server issues a badge once
            and cannot show you this one again, so you would verify from the
            start. It removes the badge from this browser only &mdash; the server
            was never told which browser held it.</p>
          <button class="btn danger" type="button" data-action="forget-verification">Forget this credential</button>
        </div>
        <div class="actions">
          <button class="btn subtle" type="button" data-action="verify-again">Verify another address</button>
        </div>
      </div>`;
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
      </form>`;
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
      <div class="actions">
        <button class="btn primary" type="submit">Send code</button>
      </div>
    </form>`;
}

async function renderVerify() {
  await loadVerification();
  el.main.innerHTML = verifyViewHTML();
  // Explained once, on the page that can do something about it.
  badgeLost = "";
  announce("Institutional verification");
}

/* Rules and FAQ ------------------------------------------------------------ */

// Both pages are readable before the age screen is answered, so neither may
// lean on anything fetched: they are the site's own words, not the API's.

// audienceText is who the site is open to, in the one form every page uses.
function audienceText() {
  return MIN_AGE > 0 ? `anyone aged ${MIN_AGE} and over` : "anyone";
}

function renderRules() {
  el.main.innerHTML = `
    <article class="page" aria-labelledby="page-title">
      <h1 id="page-title">Rules</h1>
      <p class="lede">These rules apply to every post on every board. educhan is open to
        ${audienceText()}, school students included, so keep everything you post fit for a
        school student to read.</p>
      <ol class="rules-list">
        <li><strong>Keep it safe for work, on every board.</strong> No sexual content, nudity,
          gore or shock images anywhere, not as a joke and not behind a spoiler.</li>
        <li><strong>Nothing illegal.</strong> Don't post anything, or link to anything, that
          breaks the law.</li>
        <li><strong>No personal or contact details, your own included.</strong> Never post
          phone numbers, home or school addresses, email addresses, social media, Discord,
          Snapchat or Instagram handles, or timetables, whoever they belong to. Never ask
          anyone to move to a private chat or another app.</li>
        <li><strong>No bullying, threats or harassment.</strong> Don't name private people,
          such as classmates, teachers or staff, or describe them so that they could be
          identified.</li>
        <li><strong>No spam or flooding.</strong> No advertising, no posting the same thing
          again and again, and no filling threads with junk.</li>
        <li><strong>Don't impersonate.</strong> Don't pretend to speak for a school, university
          or any other institution, and don't fake the verified badge, in your name or
          anywhere else.</li>
        <li><strong>Stay on topic.</strong> Post on the board your post belongs on, and keep
          to what that board is for.</li>
      </ol>
      <p>Some boards add rules of their own. You'll find them under <strong>Rules</strong> at
        the top of the board.</p>
      <h2>When a post breaks the rules</h2>
      <p>Moderators remove posts that break these rules, and a removed post shows the reason
        it was removed.</p>
      <h2>If someone is in danger</h2>
      <p class="callout">If you think someone is in danger, contact your local emergency
        services straight away.</p>
      <p class="muted">The <a href="/faq">FAQ</a> explains how anonymity works here and how
        to stay safe.</p>
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
      <p>educhan is an anonymous imageboard for students. There are boards for school and
        university subjects and for student life, such as admissions, exams, careers and
        studying abroad, and anyone can read and post without an account. Institutions can
        also have boards of their own, at addresses like <code>/u/iitb/</code>.</p>
      <p>Threads don't last forever. A reply moves its thread to the top of its board, and
        threads that stop getting replies sink, move to the board's archive, and are
        deleted after a while.</p>`,
  },
  {
    id: "faq-who",
    question: "Who can use it?",
    answer:
      MIN_AGE > 0
        ? `
      <p>Anyone aged ${MIN_AGE} and over. The first time you visit, educhan asks when
        you were born. Your answer is checked in your browser and is never stored or sent
        anywhere, not even to educhan's server. The browser keeps only the result, so you
        aren't asked on every visit; if it blocks site storage, you're asked each time
        instead.</p>
      <p>Everyone who uses educhan agrees to its <a href="/rules">rules</a>.</p>`
        : `
      <p>Anyone. There is nothing to sign up for. Everyone who uses educhan agrees to its
        <a href="/rules">rules</a>.</p>`,
  },
  {
    id: "faq-anonymity",
    question: "How anonymous am I?",
    answer: `
      <p>There are no accounts, and every post is signed &ldquo;Anonymous&rdquo; unless you
        type a name. Beyond that:</p>
      <ul>
        <li>Each post carries an ID that is the same for all your posts in one thread and
          different in every other, so a conversation can be followed without you being
          followed around the site (<a href="#faq-ids">more about IDs</a>).</li>
        <li>educhan's server keeps no IP addresses in its logs, and posts are not stored with
          one.</li>
        <li>The verified badge proves you have an address at an institution, never who you
          are. It's optional, except to post on an institution's own board.</li>
      </ul>
      <p>Anonymity has limits, and it's worth knowing them:</p>
      <ul>
        <li>What you write can give you away. Details about your class, school, timetable or
          friends can be enough for someone who knows you.</li>
        <li>Photos can carry hidden details, such as where they were taken, and educhan
          doesn't remove them yet. Post a screenshot of a photo rather than the original.</li>
        <li>A tripcode links every post that carries it. A badge narrows you down to your
          institution, which at a small one can say a lot.</li>
        <li>Your internet provider, the network you're on (a school's, say) and anyone who
          can use your device can see that you visit educhan.</li>
      </ul>`,
  },
  {
    id: "faq-safety",
    question: "How do I stay safe?",
    answer: `
      <ul>
        <li>Never share contact details, yours or anyone else's: phone numbers, home or school
          addresses, email addresses, or social media, Discord, Snapchat or Instagram
          handles.</li>
        <li>educhan has no private messages. Be wary of anyone who asks you to move to a
          private chat or another app: it's a common way to get someone on their own, away
          from everyone else. You don't owe anyone a reply.</li>
        <li>If something here upsets or worries you, step away and talk to someone you
          trust.</li>
        <li>If someone is in danger, contact your local emergency services.</li>
      </ul>`,
  },
  {
    id: "faq-badge",
    question: "How does the verified badge work?",
    answer: `
      <ol>
        <li>On the <a href="/verify">Verify</a> page, enter an email address at a recognised
          institution.</li>
        <li>Type in the six-digit code that's emailed to you.</li>
        <li>Your browser is given a badge naming the institution, such as
          ${verifiedBadgeHTML("IIT Bombay")}, and keeps it until it expires. The Verify page
          shows when that is.</li>
        <li>To use it, tick <strong>post with</strong> the badge on the posting form. It's off
          by default, and it's sent only with the posts you tick it on.</li>
      </ol>
      <p>The address itself is never stored. The server keeps only a keyed fingerprint of
        it, so that one address holds one badge at a time, in a table with no link to any
        post. The badge holds the institution's name, an expiry date and a random number,
        and nothing about you. So a badge proves an institution, never a person, and nothing
        connects a badged post to your email address or to the posts you make without
        it.</p>
      <p>On a shared computer, forget the badge on the Verify page before you leave: it stays
        in that browser for whoever uses it next.</p>`,
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
        your posts without one. Clearing this site's data starts a new key, and with it new
        IDs.</p>`,
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
      <p>An image can be marked <strong>Spoiler image</strong>, which hides it behind a plain
        tile until someone clicks it.</p>`,
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
        <li><strong>Image limit.</strong> Each board caps how many images one thread can hold.
          A thread shows its count, such as &ldquo;12 of 150 images&rdquo;, and once it's
          full, text replies still work.</li>
        <li><strong>The archive.</strong> Each board holds a set number of threads. When a new
          thread pushes the least recently bumped one off the board, that thread moves to
          the board's archive, where it can still be read and linked to but takes no
          replies. It's deleted after the number of days the archive page shows.</li>
      </ul>`,
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
// question once it has said no. It is the browser's own check, not proof, and
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
      <p class="age-note" id="age-note">Your answer is checked in this browser and is never
        stored or sent anywhere.</p>
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
      <p class="age-note">The <a href="/faq">FAQ</a> explains how anonymity works here and
        how to stay safe.</p>
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
  const eligible = ageFrom(Number(month.value), Number(year.value)) >= MIN_AGE;
  // Either way the question goes, and the month and year with it: from here on
  // they are nowhere on the page, let alone anywhere else.
  if (eligible) {
    ageScreen.innerHTML = ageAgreeHTML();
    focusAgeScreen();
  } else {
    refuseAgeScreen();
  }
}

function passAgeScreen() {
  storageSet(AGE_OK_STORAGE_KEY, "1");
  ageAnswer = "ok";
  closeAgeScreen({ remove: true });
  render();
}

// A refusal is final in this browser: the screen is removed rather than reset,
// and nothing on the page offers to ask again.
function refuseAgeScreen() {
  storageSet(AGE_REFUSED_STORAGE_KEY, "1");
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
  const route = parseRoute();
  state.route = route;
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
    // are better served by a way out than by the raw error.
    let message = `Could not load this page: ${err.message}`;
    if (err.status === 404) {
      message =
        route.view === "post"
          ? `No. ${route.no} is not on ${boardLabel(route.slug)} — it may have been deleted, or the number may be wrong.`
          : "That board or thread is not here — it may have been deleted, or have aged out of the archive.";
    }
    el.main.innerHTML = emptyStateHTML(message, route.view === "post" ? route.slug : null);
    announce(err.status === 404 ? "Not found" : "Could not load this page");
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
    return null;
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
    return;
  }
  for (const name of ["subject", "name", "body"]) {
    const node = field(form, name);
    if (node && draft[name]) {
      node.value = draft[name];
    }
  }
  const sage = field(form, "sage");
  if (sage) sage.checked = draft.sage;
  const badge = field(form, "badge");
  if (badge) badge.checked = draft.badge;

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
    revealSpoiler(spoiler);
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

window.addEventListener("popstate", () => render());

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
});

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
  const button = form.querySelector('button[type="submit"]');
  const data = new FormData(form);
  const name = String(data.get("name") || "");
  const body = String(data.get("body") || "");
  const image = field(form, "image");
  const file = image && !image.disabled && image.files[0];

  if (!body.trim() && !file) {
    showNotice("Write something or attach an image.");
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
    storageSet(NAME_STORAGE_KEY, name);
    // Clear before navigating so the draft carried across the re-render is the
    // empty form rather than the post that just went through.
    form.reset();
    const fileField = form.querySelector("[data-file-field]");
    if (fileField) {
      clearFileField(fileField);
    }
    await navigate(`${threadPath(created.board, created.thread_no)}#p${created.no}`);
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
      } else if (box) {
        box.checked = false;
      }
    }
    // The cap was reached after this form was drawn: take the image off and
    // switch the picker off, so the next press posts the text that is left.
    if (err.status === 403 && err.message === "image limit reached") {
      const fileField = form.querySelector("[data-file-field]");
      if (fileField) {
        blockFileField(fileField, imageLimitReason(null));
      }
    }
    showNotice(errorMessage(err));
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

  button.disabled = true;
  button.textContent = requesting ? "Sending…" : "Verifying…";
  clearNotice();
  try {
    if (requesting) {
      const sent = await api("/api/verify/request", {
        method: "POST",
        body: JSON.stringify({ email: String(data.get("email") || "") }),
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
    showNotice(errorMessage(err));
    button.disabled = false;
    button.textContent = label;
  }
}

/* Bootstrap ---------------------------------------------------------------- */

// The board list, and whether verification exists here, are fetched the first
// time a page past the age screen needs them, and kept from then on. A failure
// is not kept: the next navigation tries again.
let siteLoaded = false;

async function loadSite() {
  if (siteLoaded) {
    return;
  }
  const { boards } = await api("/api/boards");
  state.boards = boards;
  // The posting form needs to know whether to offer the badge checkbox, and a
  // failure here must not stop the board from loading.
  await loadVerification();
  siteLoaded = true;
}

render();
