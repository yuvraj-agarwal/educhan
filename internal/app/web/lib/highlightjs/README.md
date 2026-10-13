# highlight.js (vendored)

- **Package:** `@highlightjs/cdn-assets` **11.12.0** on npm — the prebuilt browser
  bundle with highlight.js's "common" language set built in.
- **Source:** <https://registry.npmjs.org/@highlightjs/cdn-assets/-/cdn-assets-11.12.0.tgz>,
  whose sha512 matched the registry's published integrity
  (`sha512-KvOKXODaiFmId9xaq3xc5xCL66wVLUuOngDbO9B/kewbFTqdGbn2nJxNhN3H5R1cgDTVj6R8vH0zgiNDEGjpDw==`).
  Upstream: <https://github.com/highlightjs/highlight.js>.
- **Files:** `highlight.min.js` and `LICENSE`, copied unmodified from the package root.
- **License:** BSD-3-Clause, in `LICENSE`.

`app.js` loads `highlight.min.js` with a same-origin script element the first time a
post with a `[code]` block is on screen, so readers who never meet one never download it.
No highlight.js theme is vendored: the token colours are educhan's own, in `app.css`,
chosen to clear WCAG AA on the code block background.

To update, replace both files from the new package's tarball, update this note, and
update the pinned hashes in `vendoredWebAssets` in `internal/app/web_test.go` — the test
fails on any file under `lib/` that it does not list with its exact hash.
