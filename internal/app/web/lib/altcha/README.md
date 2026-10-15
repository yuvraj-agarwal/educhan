# ALTCHA PBKDF2 worker (vendored)

- **Package:** `altcha` **3.3.0** on npm.
- **Source:** <https://registry.npmjs.org/altcha/-/altcha-3.3.0.tgz>, whose sha512 matched
  the registry's published integrity
  (`sha512-olIeb1A5K2wzbFSxV3qgeknygbdJt7qmzCfAZn704PWsE9iGrfc/oTw1FfiESjsvaJf8Uk6elJX8opKq5u7dTw==`).
  Upstream: <https://github.com/altcha-org/altcha>.
- **Files:** `pbkdf2.js`, copied unmodified from `dist/workers/`, and `LICENSE`, which is
  `LICENSE.txt` from the package root. The worker is the same file in 3.4.0.
- **License:** MIT, in `LICENSE`.

Only the worker is vendored, not the widget. The worker is 6.5 KB of plain JavaScript
with no imports, no network access and no WebAssembly: it takes a challenge in a
message, runs PBKDF2-SHA-256 through the browser's own Web Crypto over each counter
from `counterStart` in steps of `counterStep`, and posts back the first counter whose
key starts with the challenge's prefix. The widget (85 KB minified) was left out
because educhan needs none of what it adds: a checkbox, themes, translations, code and
audio challenges, a floating layout, and a "human interaction signature" collector
that records pointer and keyboard behaviour and is on by default. `app.js` fetches the
challenge, runs this worker in up to four copies, and builds the answer in ALTCHA's own
payload format (see `internal/app/captcha.go`), so the widget could still be dropped in
later without a server change.

`app.js` starts the worker with `new Worker("/lib/altcha/pbkdf2.js")` when a reader
moves into a box to write in, in a form that needs the captcha, never when a page is
drawn, and stops every copy once one has found the answer.
The CSP needs no change for it: `worker-src` falls back to `default-src 'self'`.

To update, replace `pbkdf2.js` from the new package's `dist/workers/` and `LICENSE` from
its `LICENSE.txt`, read the new worker in full (it should still make no request and load
nothing), update this note, and update the pinned hashes in `vendoredWebAssets` in
`internal/app/web_test.go`, which fails on any file under `lib/` it does not list with its
exact hash. `TestCaptchaKeyMatchesTheWorker` in `captcha_test.go` holds a solution the
worker produced; if a new worker derives keys differently, that test is the one to fail.
