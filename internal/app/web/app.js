// educhan front end: a small hand-rolled SPA over the JSON API.
//
// Routes mirror the API so links are shareable:
//   /                     board list
//   /{slug}/?page=N       board index
//   /{slug}/catalog       catalog
//   /{slug}/thread/{no}   thread

const NAME_STORAGE_KEY = "educhan:name";
const ACCEPTED_IMAGES = "image/jpeg,image/png,image/webp,image/gif";

const state = {
  boards: [],
  route: { view: "home" },
};

const el = {
  main: document.getElementById("main"),
  nav: document.getElementById("board-nav"),
  notice: document.getElementById("notice"),
};

/* API ---------------------------------------------------------------------- */

async function api(path, options = {}) {
  const init = { headers: {}, ...options };
  // FormData sets its own multipart content type, boundary included.
  if (init.body !== undefined && !(init.body instanceof FormData)) {
    init.headers["Content-Type"] = "application/json";
  }

  const res = await fetch(path, init);
  const payload = res.status === 204 ? null : await res.json().catch(() => null);
  if (!res.ok) {
    const err = new Error((payload && payload.error) || `${res.status} ${res.statusText}`);
    err.status = res.status;
    throw err;
  }
  return payload;
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

function timeHTML(iso) {
  const date = new Date(iso);
  return `<time datetime="${esc(iso)}">${esc(date.toLocaleString())}</time>`;
}

function plural(count, one, many) {
  return `${count} ${count === 1 ? one : many}`;
}

function showNotice(message) {
  el.notice.textContent = message;
  el.notice.hidden = false;
}

function clearNotice() {
  el.notice.hidden = true;
  el.notice.textContent = "";
}

/* Post rendering ----------------------------------------------------------- */

// bodyHTML renders post text: lines starting with ">" become greentext and
// ">>123" becomes a link when it resolves to a real post on this board.
function bodyHTML(post, slug) {
  const targets = new Map((post.quotes || []).map((q) => [q.no, q.thread_no]));

  return post.body
    .split("\n")
    .map((line) => {
      const quoteLine = /^>>\d/.test(line);
      const greentext = line.startsWith(">") && !quoteLine;
      const html = esc(line).replace(/&gt;&gt;(\d+)/g, (match, digits) => {
        const no = Number(digits);
        const threadNo = targets.get(no);
        if (threadNo === undefined) {
          return `<a class="quotelink dead" href="#" title="post not found">${match}</a>`;
        }
        return `<a class="quotelink" href="${threadPath(slug, threadNo)}#p${no}">${match}</a>`;
      });
      return greentext ? `<span class="greentext">${html}</span>` : html;
    })
    .join("\n");
}

function backlinksHTML(post, slug) {
  if (!post.backlinks || post.backlinks.length === 0) {
    return "";
  }
  const links = post.backlinks
    .map((ref) => `<a href="${threadPath(slug, ref.thread_no)}#p${ref.no}">&gt;&gt;${ref.no}</a>`)
    .join("");
  return `<div class="backlinks">Replies: ${links}</div>`;
}

/* Attachments -------------------------------------------------------------- */

function thumbHTML(att, extraClass = "") {
  return `<img class="thumb${extraClass ? ` ${extraClass}` : ""}" src="${esc(att.thumb_url)}"
    width="${att.thumb_width}" height="${att.thumb_height}" loading="lazy"
    alt="${esc(att.name) || "attached image"}" />`;
}

function fileSize(bytes) {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  if (bytes >= 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${bytes} B`;
}

// attachmentHTML renders a post's image. Inside a thread the thumbnail expands
// in place, which is the classic imageboard behaviour; elsewhere the full image
// opens in a new tab so the surrounding link still navigates.
function attachmentHTML(att, { expandable = false } = {}) {
  if (!att) {
    return "";
  }
  const caption = att.name
    ? `<div class="filename">${esc(att.name)} (${att.width}&times;${att.height}, ${fileSize(att.size)})</div>`
    : "";
  const link = expandable
    ? `<a class="thumb-link expandable" href="${esc(att.url)}" data-full="${esc(att.url)}"
         data-thumb="${esc(att.thumb_url)}">${thumbHTML(att)}</a>`
    : `<a class="thumb-link" href="${esc(att.url)}" target="_blank" rel="noopener">${thumbHTML(att)}</a>`;
  return `<div class="attachment">${caption}${link}</div>`;
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

  if (post.op && subject) {
    head.push(`<span class="subject">${esc(subject)}</span>`);
  }
  head.push(`<span class="name">${esc(post.name)}</span>`);
  if (post.tripcode) {
    head.push(`<span class="trip">${esc(post.tripcode)}</span>`);
  }
  head.push(`<span class="pid">ID: ${esc(post.poster_id)}</span>`);
  head.push(timeHTML(post.created_at));

  const numberAttrs = quotable ? ` data-quote="${post.no}"` : "";
  head.push(`<a class="no mono" href="${threadPath(slug, post.thread_no)}#p${post.no}"${numberAttrs}>No.&nbsp;${post.no}</a>`);

  if (post.op && thread) {
    const badges = badgesHTML(thread);
    if (badges) head.push(badges);
  }

  return `
    <article class="post ${post.op ? "op" : "reply"}" id="p${post.no}">
      <div class="post-head">${head.join("\n")}</div>
      ${attachmentHTML(post.attachment, { expandable: quotable })}
      <div class="post-body">${bodyHTML(post, slug)}</div>
      ${backlinksHTML(post, slug)}
    </article>`;
}

/* Forms -------------------------------------------------------------------- */

function formHTML({ kind, threadNo = null, open = false, locked = false }) {
  if (locked) {
    return `<p class="muted">This thread is locked; no new replies.</p>`;
  }

  const isThread = kind === "thread";
  const name = localStorage.getItem(NAME_STORAGE_KEY) || "";
  const subject = isThread
    ? `<input type="text" name="subject" placeholder="Subject (optional)" maxlength="100" />`
    : "";
  const sage = isThread
    ? ""
    : `<label class="sage"><input type="checkbox" name="sage" /> sage (reply without bumping)</label>`;

  return `
    <details class="post-form" ${open ? "open" : ""}>
      <summary>${isThread ? "Start a new thread" : "Reply to this thread"}</summary>
      <form class="fields" data-kind="${kind}" ${threadNo ? `data-thread="${threadNo}"` : ""}>
        <div class="row">
          ${subject}
          <input type="text" name="name" value="${esc(name)}" placeholder="Anonymous (add #secret for a tripcode)" maxlength="80" />
        </div>
        <textarea name="body" placeholder="${isThread ? "What's on your mind?" : "Write a reply. Use >>123 to quote a post."}" maxlength="8000"></textarea>
        <input type="file" name="image" accept="${ACCEPTED_IMAGES}" />
        <div class="actions">
          <button class="primary" type="submit">${isThread ? "Create thread" : "Post reply"}</button>
          ${sage}
          <span class="hint">Posting is anonymous.</span>
        </div>
      </form>
    </details>`;
}

/* Views -------------------------------------------------------------------- */

function renderNav(activeSlug) {
  el.nav.innerHTML = state.boards
    .map((b) => {
      const active = b.slug === activeSlug ? " active" : "";
      return `<a class="board-nav-link${active}" href="${boardPath(b.slug)}" title="${esc(b.description)}">/${esc(b.slug)}/</a>`;
    })
    .join("");
}

function boardHeadHTML(board, view) {
  const views = [
    { href: boardPath(board.slug), label: "Index", key: "index" },
    { href: `${boardPath(board.slug)}catalog`, label: "Catalog", key: "catalog" },
  ];
  const links = views
    .map((v) => `<a href="${v.href}" class="${v.key === view ? "active" : ""}">${v.label}</a>`)
    .join("");

  return `
    <div class="board-head">
      <h1>/${esc(board.slug)}/ &mdash; ${esc(board.name)}</h1>
      <span class="desc">${esc(board.description)}</span>
      <div class="views">${links}</div>
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
    <div class="board-list">${cards || '<p class="muted">No boards yet.</p>'}</div>`;
}

function pagerHTML(slug, page, pages) {
  if (pages <= 1) {
    return "";
  }
  const prev =
    page > 1
      ? `<a href="${boardPath(slug)}?page=${page - 1}">&larr; Newer</a>`
      : `<span>&larr; Newer</span>`;
  const next =
    page < pages
      ? `<a href="${boardPath(slug)}?page=${page + 1}">Older &rarr;</a>`
      : `<span>Older &rarr;</span>`;
  return `<div class="pager">${prev}<span>Page ${page} of ${pages}</span>${next}</div>`;
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

      return `
        <section class="thread">
          ${postHTML(summary.op, context)}
          ${omitted}
          ${replies}
          <div class="thread-actions">
            <a href="${threadPath(board.slug, summary.no)}">Open thread (${plural(summary.replies, "reply", "replies")})</a>
          </div>
        </section>`;
    })
    .join("");

  el.main.innerHTML = `
    ${boardHeadHTML(board, "index")}
    ${formHTML({ kind: "thread", open: index.threads.length === 0 })}
    ${threads || '<p class="muted">No threads yet — start the first one.</p>'}
    ${pagerHTML(board.slug, index.page, index.pages)}`;
}

async function renderCatalog(route) {
  const catalog = await api(`/api/boards/${encodeURIComponent(route.slug)}/catalog`);
  const { board } = catalog;

  const tiles = catalog.threads
    .map(
      (t) => `
      <a class="tile" href="${threadPath(board.slug, t.no)}">
        <div class="stats">
          <span class="mono">R: ${t.replies}</span>
          ${badgesHTML(t)}
        </div>
        ${t.attachment ? thumbHTML(t.attachment, "tile-thumb") : '<div class="no-image">no image</div>'}
        ${t.subject ? `<div class="subject">${esc(t.subject)}</div>` : ""}
        <div class="excerpt">${esc(t.excerpt)}</div>
      </a>`
    )
    .join("");

  el.main.innerHTML = `
    ${boardHeadHTML(board, "catalog")}
    <div class="catalog">${tiles || '<p class="muted">No threads yet — start the first one.</p>'}</div>`;
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
}

/* Routing ------------------------------------------------------------------ */

function parseRoute(location = window.location) {
  const parts = location.pathname.split("/").filter(Boolean).map(decodeURIComponent);
  const query = new URLSearchParams(location.search);

  if (parts.length === 0) {
    return { view: "home" };
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

async function render() {
  const route = parseRoute();
  state.route = route;
  renderNav(route.slug);
  clearNotice();

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
      default:
        el.main.innerHTML = '<p class="muted">Page not found.</p>';
    }
  } catch (err) {
    el.main.innerHTML = `<p class="muted">Could not load this page: ${esc(err.message)}</p>`;
    return;
  }
  focusHash();
}

// focusHash highlights the post referenced by the URL fragment, which is where
// quote links point.
function focusHash() {
  if (!window.location.hash) {
    return;
  }
  const target = document.querySelector(window.location.hash);
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

  const quoteButton = event.target.closest("[data-quote]");
  if (quoteButton && quoteTarget()) {
    event.preventDefault();
    insertQuote(Number(quoteButton.dataset.quote));
    return;
  }

  const expandable = event.target.closest("a.thumb-link.expandable");
  if (expandable) {
    event.preventDefault();
    toggleImage(expandable);
    return;
  }

  if (event.target.closest('[data-action="refresh"]')) {
    event.preventDefault();
    render();
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
  if (anchor.pathname === window.location.pathname && anchor.hash) {
    // Same page: let the browser jump to the anchor, then highlight it.
    setTimeout(focusHash, 0);
    return;
  }
  event.preventDefault();
  navigate(anchor.href);
});

window.addEventListener("popstate", render);

// toggleImage swaps a thumbnail for the full image and back, leaving the layout
// to the stylesheet.
function toggleImage(link) {
  const img = link.querySelector("img");
  if (!img) {
    return;
  }
  const expanded = img.classList.toggle("expanded");
  if (expanded) {
    img.removeAttribute("width");
    img.removeAttribute("height");
  }
  img.src = expanded ? link.dataset.full : link.dataset.thumb;
}

function quoteTarget() {
  return el.main.querySelector('form[data-kind="reply"] textarea[name="body"]');
}

function insertQuote(no) {
  const textarea = quoteTarget();
  if (!textarea) {
    return;
  }
  const form = textarea.closest("form");
  const details = form.closest("details");
  if (details) {
    details.open = true;
  }
  const prefix = textarea.value && !textarea.value.endsWith("\n") ? "\n" : "";
  textarea.value += `${prefix}>>${no}\n`;
  textarea.focus();
  textarea.selectionStart = textarea.selectionEnd = textarea.value.length;
}

el.main.addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = event.target;
  const button = form.querySelector('button[type="submit"]');
  const data = new FormData(form);
  const name = String(data.get("name") || "");
  const body = String(data.get("body") || "");
  const file = form.image && form.image.files[0];

  if (!body.trim() && !file) {
    showNotice("Write something or attach an image.");
    return;
  }

  const isThread = form.dataset.kind === "thread";
  const payload = { name, body };
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

  button.disabled = true;
  clearNotice();
  try {
    const created = await api(path, { method: "POST", body: requestBody });
    localStorage.setItem(NAME_STORAGE_KEY, name);
    await navigate(`${threadPath(created.board, created.thread_no)}#p${created.no}`);
  } catch (err) {
    showNotice(err.message);
    button.disabled = false;
  }
});

/* Bootstrap ---------------------------------------------------------------- */

async function start() {
  try {
    const { boards } = await api("/api/boards");
    state.boards = boards;
  } catch (err) {
    el.main.innerHTML = `<p class="muted">Could not reach the server: ${esc(err.message)}</p>`;
    return;
  }
  await render();
}

start();
