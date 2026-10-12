// educhan front end: a small hand-rolled SPA over the JSON API.
//
// Routes mirror the API so links are shareable:
//   /                     board list
//   /verify               institutional verification
//   /{slug}/?page=N       board index
//   /{slug}/catalog       catalog
//   /{slug}/thread/{no}   thread

const NAME_STORAGE_KEY = "educhan:name";
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
  verifyLink: document.querySelector(".verify-link"),
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
  if (err.status === 403) {
    return `${err.message}.`;
  }
  return err.message;
}

/* Rendering helpers -------------------------------------------------------- */

const ESCAPES = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };

function esc(value) {
  return String(value ?? "").replace(/[&<>"']/g, (c) => ESCAPES[c]);
}

function boardPath(slug) {
  return `/${encodeURIComponent(slug)}/`;
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
//
// Each tag pairs with the first closing tag after it. A tag that never closes,
// an empty pair, and a spoiler opened inside another stay the literal text that
// was typed, so no input can render as half an element. Code is opaque: nothing
// inside a [code] block is markup, and extractQuotes in text.go follows the same
// pairing so that a code sample — `x >>2` — creates no backlink.

const CODE_OPEN = "[code]";
const CODE_CLOSE = "[/code]";
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
// Top-level paths that are never boards (reservedBoardSlugs in boards.go), so a
// link to one would claim a board that cannot exist.
const RESERVED_SLUGS = new Set([
  "api", "media", "healthz", "verify", "mod", "u", "rules", "faq",
  "legal", "terms", "privacy", "dmca", "report", "archive",
]);

// The name a hidden spoiler has for assistive tech. Its text is aria-hidden
// until it is revealed, so a screen reader announces that something is hidden
// rather than reading it out — the same choice a sighted reader gets.
const SPOILER_LABEL = "Hidden spoiler: activate to reveal";

// parseMarkup splits a body into text runs, code blocks, and spoilers. Runs are
// kept as offsets into the source, so rendering can still tell where a line
// starts. A spoiler holds text runs and code blocks; nothing nests deeper.
function parseMarkup(source) {
  // Pass one finds the tags. A [code] counts only when a [/code] follows with
  // something in between, and it claims everything up to that [/code] — tags
  // included — so the scan jumps straight past it. Checking against the last
  // [/code] first keeps a body full of unclosed [code]s linear.
  const tags = [];
  const lastCodeClose = source.lastIndexOf(CODE_CLOSE);
  for (let i = source.indexOf("["); i !== -1; i = source.indexOf("[", i)) {
    if (source.startsWith(CODE_OPEN, i) && i + CODE_OPEN.length <= lastCodeClose) {
      const close = source.indexOf(CODE_CLOSE, i + CODE_OPEN.length);
      if (close > i + CODE_OPEN.length) {
        tags.push({ kind: "code", at: i, end: close + CODE_CLOSE.length });
        i = close + CODE_CLOSE.length;
        continue;
      }
    } else if (source.startsWith(SPOILER_OPEN, i)) {
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
    if (tag.kind === "code") {
      text(tag.at);
      into.push({ kind: "code", start: tag.at + CODE_OPEN.length, end: tag.end - CODE_CLOSE.length });
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

// A spoiler holding a code block is laid out as a block itself, so it hides the
// whole box rather than a run of text that the block breaks out of.
function isBlockNode(node) {
  return node.kind === "code" || (node.kind === "spoiler" && node.children.some(isBlockNode));
}

// crossLinkHref turns a cross-board match into the SPA route it links to, or
// null when it is not a link after all: a reserved path, a slug too long to be
// a board, or a number too long to be a post.
function crossLinkHref(match) {
  const [, institution, name, no] = match;
  const slug = institution ? `u-${name}` : name;
  if (slug.length > MAX_SLUG_LENGTH || RESERVED_SLUGS.has(slug) || (no && no.length > MAX_POST_DIGITS)) {
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
      let { start, end } = node;
      if (source[start] === "\n") start += 1;
      if (end > start && source[end - 1] === "\n") end -= 1;
      // Focusable, because a long line scrolls sideways and a keyboard has to
      // be able to scroll it.
      out.push(`<pre class="code" tabindex="0"><code>${esc(source.slice(start, end))}</code></pre>`);
      ctx.lineStart = true;
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
// blocks are highlighted here, and maths will be typeset here. Every view that
// inserts posts calls it on what it inserted. It is safe on any element and as
// often as wanted — work already done is skipped — and nothing it does can fail
// a render: a block it cannot finish keeps its plain, escaped rendering.
function enhancePosts(root) {
  if (!root) {
    return;
  }
  highlightCode(root);
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
  return `<div class="post-file">${caption}${link}</div>`;
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
  if (thread.locked) badges.push('<span class="badge locked">locked</span>');
  if (thread.at_bump_limit) badges.push('<span class="badge bumplimit">bump limit</span>');
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

function fileFieldHTML() {
  return `
    <div class="file-field" data-file-field>
      <input class="file-input" type="file" id="${FIELD_IDS.image}" name="image"
             accept="${ACCEPTED_IMAGES}" aria-labelledby="f-image-label" />
      <label class="btn secondary file-button" for="${FIELD_IDS.image}">Choose image</label>
      <span class="file-hint">or drop one on this form &middot; JPEG, PNG, WebP, GIF</span>
      <div class="file-preview" aria-live="polite" hidden>
        <img class="file-thumb" alt="" />
        <span class="file-meta"></span>
        <button class="btn subtle file-remove" type="button" data-action="clear-file">Remove</button>
      </div>
    </div>`;
}

function formHTML({ kind, threadNo = null, open = false, locked = false }) {
  if (locked) {
    return `<p class="locked-note"><span class="badge locked">locked</span> This thread is locked; no new replies.</p>`;
  }

  const isThread = kind === "thread";
  const name = localStorage.getItem(NAME_STORAGE_KEY) || "";
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
          ${fileFieldHTML()}
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
      return `<a href="${boardPath(b.slug)}" title="${esc(b.description)}"${current}>/${esc(b.slug)}/</a>`;
    })
    .join("");

  if (!el.verifyLink) {
    return;
  }
  if (route.view === "verify") {
    el.verifyLink.setAttribute("aria-current", "page");
  } else {
    el.verifyLink.removeAttribute("aria-current");
  }
}

function boardHeadHTML(board, view) {
  const views = [
    { href: boardPath(board.slug), label: "Index", key: "index" },
    { href: `${boardPath(board.slug)}catalog`, label: "Catalog", key: "catalog" },
  ];
  const links = views
    .map((v) => `<a href="${v.href}"${v.key === view ? ' aria-current="page"' : ""}>${v.label}</a>`)
    .join("");

  return `
    <div class="board-head">
      <h1>/${esc(board.slug)}/ <span class="board-name">${esc(board.name)}</span></h1>
      <span class="desc">${esc(board.description)}</span>
      <nav class="views" aria-label="Board views">${links}</nav>
    </div>`;
}

function renderHome() {
  const cards = state.boards
    .map(
      (b) => `
      <a class="board-card" href="${boardPath(b.slug)}">
        <strong>/${esc(b.slug)}/ &mdash; ${esc(b.name)}</strong>
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
            <a href="${threadPath(board.slug, summary.no)}">Open thread (${plural(summary.replies, "reply", "replies")})</a>
          </div>
          ${omitted}
          ${replies}
        </section>`;
    })
    .join("");

  el.main.innerHTML = `
    ${boardHeadHTML(board, "index")}
    ${formHTML({ kind: "thread", open: index.threads.length === 0 })}
    ${threads || '<p class="empty">No threads yet — start the first one.</p>'}
    ${pagerHTML(board.slug, index.page, index.pages)}`;
  announce(`/${board.slug}/${index.pages > 1 ? ` page ${index.page}` : ""}`);
}

async function renderCatalog(route) {
  const catalog = await api(`/api/boards/${encodeURIComponent(route.slug)}/catalog`);
  const { board } = catalog;

  const tiles = catalog.threads
    .map((t) => {
      const media = t.attachment
        ? `<div class="tile-media">${thumbHTML(t.attachment, { alt: "" })}</div>`
        : `<div class="tile-media empty-media">no image</div>`;
      return `
      <a class="tile" href="${threadPath(board.slug, t.no)}">
        <div class="stats">
          <span class="replies">${plural(t.replies, "reply", "replies")}</span>
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
  announce(`/${board.slug}/ catalog`);
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

  el.main.innerHTML = `
    ${boardHeadHTML(board, "index")}
    <div class="thread-actions">
      <a href="${boardPath(board.slug)}">&larr; Back to /${esc(board.slug)}/</a>
      <button class="link" data-action="refresh" type="button">Update</button>
      <span class="muted">${plural(detail.replies, "reply", "replies")}${
        detail.at_bump_limit ? " &middot; at bump limit, no longer bumping" : ""
      }</span>
    </div>
    <section class="thread detail">${posts}</section>
    ${formHTML({ kind: "reply", threadNo: detail.no, open: true, locked: detail.locked })}`;
  announce(`${detail.subject || `Thread No. ${detail.no}`} — /${board.slug}/`);
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

/* Routing ------------------------------------------------------------------ */

function parseRoute(location = window.location) {
  const parts = location.pathname.split("/").filter(Boolean).map(decodeURIComponent);
  const query = new URLSearchParams(location.search);

  if (parts.length === 0) {
    return { view: "home" };
  }
  // Checked before the board route, and reserved server-side too, so no board
  // can take this path out from under it.
  if (parts.length === 1 && parts[0] === "verify") {
    return { view: "verify" };
  }
  const [slug, section, no] = parts;
  if (parts.length === 1) {
    return { view: "index", slug, page: Math.max(Number(query.get("page")) || 1, 1) };
  }
  if (parts.length === 2 && section === "catalog") {
    return { view: "catalog", slug };
  }
  if (parts.length === 3 && section === "thread" && /^\d+$/.test(no)) {
    return { view: "thread", slug, no: Number(no) };
  }
  return { view: "notfound" };
}

function routeKey(route) {
  return `${route.view}:${route.slug || ""}:${route.no || ""}`;
}

function emptyStateHTML(message) {
  return `<p class="empty">${esc(message)}<br /><a href="/">Back to the board list</a></p>`;
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

async function render() {
  const draft = captureDraft();
  const route = parseRoute();
  state.route = route;
  renderNav(route);
  clearNotice();
  hideQuotePreview();
  // Whatever was expanded belongs to the view being replaced.
  expandedLink = null;

  const finish = beginLoading();
  try {
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
      case "thread":
        await renderThread(route);
        break;
      case "verify":
        await renderVerify();
        break;
      default:
        el.main.innerHTML = emptyStateHTML("That is not a page on this site.");
        announce("Page not found");
    }
  } catch (err) {
    // A pruned thread and a mistyped board look the same from here, and both
    // are better served by a way out than by the raw error.
    const message =
      err.status === 404
        ? "That board or thread is not here — it may have been deleted or pruned."
        : `Could not load this page: ${err.message}`;
    el.main.innerHTML = emptyStateHTML(message);
    announce(err.status === 404 ? "Not found" : "Could not load this page");
    return;
  } finally {
    finish();
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
  return {
    key: routeKey(state.route),
    open: details ? details.open : true,
    subject: value("subject"),
    name: value("name"),
    body: value("body"),
    sage: sage ? sage.checked : false,
    badge: badge ? badge.checked : false,
    file: image && image.files ? image.files[0] : null,
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
  if (fileField && draft.file) {
    setFile(fileField, draft.file);
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

window.addEventListener("popstate", render);

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
  if (field) {
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
  const file = image && image.files[0];

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
  const headers = held ? { [CREDENTIAL_HEADER]: held.token } : undefined;

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
    localStorage.setItem(NAME_STORAGE_KEY, name);
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

async function start() {
  try {
    const { boards } = await api("/api/boards");
    state.boards = boards;
  } catch (err) {
    el.main.innerHTML = `<p class="empty">Could not reach the server: ${esc(err.message)}</p>`;
    return;
  }
  // The posting form needs to know whether to offer the badge checkbox, and a
  // failure here must not stop the board from loading.
  await loadVerification();
  await render();
}

start();
