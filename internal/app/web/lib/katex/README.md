# KaTeX (vendored)

- **Package:** `katex` **0.18.9** on npm.
- **Source:** <https://registry.npmjs.org/katex/-/katex-0.18.9.tgz>, whose sha512 matched
  the registry's published integrity
  (`sha512-8ad9RyoKsb/g8/yLFE+KAlP+DhbCTRUNi/V9XGsxn0R+trJJltNwzcDNo0q/DEkOy5fQUQTAQyCXCYSE+OakTQ==`).
  Upstream: <https://github.com/KaTeX/KaTeX>.
- **Files:** `LICENSE` from the package root, and `katex.min.js`, `katex.min.css` and the
  twenty `fonts/*.woff2` from `dist/`. `katex.min.js` and the fonts are unmodified.
- **License:** MIT, in `LICENSE`.

## The one change: `katex.min.css` lists WOFF2 only

The published stylesheet gives each `@font-face` three sources: WOFF2, then WOFF and
TrueType fallbacks. Every browser that runs this front end takes the WOFF2, so only the
WOFF2 files are vendored, and the fallbacks are cut from the stylesheet so that no
browser is ever pointed at a file that is not here. Nothing else in the file changes.
The published file's SHA-256 is
`b9ce0e8ce93f0c18c4986fe1f1c3c269d921b56a69e6c97f83a507916b38aab5`; the vendored one is
exactly this, run from the package root:

```sh
sed -E 's/,url\(fonts\/KaTeX_[A-Za-z0-9-]+\.woff\) format\("woff"\),url\(fonts\/KaTeX_[A-Za-z0-9-]+\.ttf\) format\("truetype"\)//g' \
  dist/katex.min.css > katex.min.css
```

`TestVendoredStylesheetsReferenceVendoredFiles` in `internal/app/web_test.go` checks that
every `url()` left in the stylesheet names a vendored file.

## How it is used

`app.js` appends a same-origin `<link>` for the stylesheet and a `<script>` for
`katex.min.js` the first time a post with `[math]` or `[eqn]` is on screen, so readers who
never meet one never download either. It typesets with `katex.render` into a placeholder
element, never `renderToString` and `innerHTML`: KaTeX's HTML carries its layout in inline
`style` attributes, which the CSP refuses, while `katex.render` sets styles through the
CSSOM, which the CSP allows. See `typesetMaths` in `app.js` for the options and why.

To update, replace the files from the new package's tarball, reapply the stylesheet
change above (and check it still leaves only WOFF2 sources), update this note, and update
the pinned hashes in `vendoredWebAssets` in `internal/app/web_test.go` — the test fails
on any file under `lib/` that it does not list with its exact hash.
