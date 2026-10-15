package app

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The front end is served under a same-origin CSP with no 'unsafe-inline'
// exception, so an inline style attribute or an inline script is not a style
// choice — it is markup the browser will refuse to apply. These patterns are
// what that refusal looks like before it reaches a browser.
var (
	inlineStyleAttr = regexp.MustCompile(`(?i)\sstyle\s*=\s*["']`)
	inlineStyleTag  = regexp.MustCompile(`(?i)<style[\s>]`)
	inlineScriptTag = regexp.MustCompile(`(?i)<script(?:(?:\s+[a-z-]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+))?)*)\s*>`)
)

// vendorDir holds third-party builds, served exactly as they were published.
// It is web/lib/ rather than web/vendor/ because a Go module zip leaves out
// every directory named vendor, so `go install` from the module proxy would
// build a binary without the libraries.
const vendorDir = "lib/"

// vendoredWebAssets is every file under web/lib/, with its SHA-256. The
// inline-markup scan is a heuristic for markup this project writes, and a
// minified library — whose grammars and templates are full of strings like
// "<style" and "<script" — is not that, so vendored files are vetted here
// instead: a file reaches lib/ only by being listed, and a listed file cannot
// change without its hash changing here too. Updating a library means reading
// the new build, then updating its README.md and its lines below.
var vendoredWebAssets = map[string]string{
	"lib/altcha/LICENSE":                              "bee1fb9d9c97d42c12c22332c5250f2fb1ef5c2321f6f82beb23e3b77be50a29",
	"lib/altcha/README.md":                            "9cbd65903c9eff0ea7061b6f350fad7bd96c688331d7dd12582f1130e2f7d22f",
	"lib/altcha/pbkdf2.js":                            "f2dcf92ba8cd81a7a6170c2c6181248359ddf5a6e35340c7472a313208273336",
	"lib/highlightjs/LICENSE":                         "6c081431591d9df696c82dc598fe1423765b8a299b200ed00b281afd0f64c490",
	"lib/highlightjs/README.md":                       "0dbbeb0b8768389763c8aafd06edcf9ae2ee7a972b333258aa0053e4c3a5b2b6",
	"lib/highlightjs/highlight.min.js":                "8ab71eb09c51f501e5e25157d9cff100e46cc29bcbfc744d0b746d451fca7f53",
	"lib/katex/LICENSE":                               "766ccc1f306c885aa45542a9846bbd0a505b27a0374f146778171c2254ce18e3",
	"lib/katex/README.md":                             "2c89a985fd04b6f23c104a1a60bd791b9dc7dbbfa6ffcb28641d10f8b47260e1",
	"lib/katex/fonts/KaTeX_AMS-Regular.woff2":         "0cdd387c9590a1a9f9794560022dbb59654a7d86f187aa0c81495ad42d3a7308",
	"lib/katex/fonts/KaTeX_Caligraphic-Bold.woff2":    "de7701e42cf1f4cf0b766c03fb27977207eee2f4fd5d76fa82188406da43ea4c",
	"lib/katex/fonts/KaTeX_Caligraphic-Regular.woff2": "5d53e70ad607c2352162dec9e0923fb54ecdafaccbf604cd8dcf7d00facb989b",
	"lib/katex/fonts/KaTeX_Fraktur-Bold.woff2":        "74444efd593c005e3f4573b44524704c0af0a937fe911cca9e94068d0d140d3f",
	"lib/katex/fonts/KaTeX_Fraktur-Regular.woff2":     "51814d270d06ff0255dba0799994fa4d8c84d11f09951d47595f4abb1f3602dc",
	"lib/katex/fonts/KaTeX_Main-Bold.woff2":           "0f60d1b897938ec918c8ce073092411baf9438f6739465693ff18b0f9d20b021",
	"lib/katex/fonts/KaTeX_Main-BoldItalic.woff2":     "99cd42a3c072d918f2f44984a807cf7aa16e13545fd0875fc07c6c65f99e715b",
	"lib/katex/fonts/KaTeX_Main-Italic.woff2":         "97479ca6cce906abc961ecac96faa5f9ca2e61b8e7670d475826bcdee9a7c267",
	"lib/katex/fonts/KaTeX_Main-Regular.woff2":        "c2342cd8b869e01752a9321dc17213fc40d4d04c79688c1d43f2cf316abd7866",
	"lib/katex/fonts/KaTeX_Math-BoldItalic.woff2":     "dc47344dbb6cb5b655c8460d561f4df5f501b90c804ad3c6cec65fe322351ab1",
	"lib/katex/fonts/KaTeX_Math-Italic.woff2":         "7af58c5ec8f132a2ddde9027c6d7814decce4d3b822a11192a42a20e2e973264",
	"lib/katex/fonts/KaTeX_SansSerif-Bold.woff2":      "e99ae51144bf1232efcc1bfe5add36262c6866b0faab24fa75740e1b98577a62",
	"lib/katex/fonts/KaTeX_SansSerif-Italic.woff2":    "00b26ac825e2095056396e0553b8ac26d3f8ad158c3826e28b4c45b385c4714a",
	"lib/katex/fonts/KaTeX_SansSerif-Regular.woff2":   "68e8c73ef42afd3ccec58bf0fba302cce448938e7fc020a5e31f8a952eee1342",
	"lib/katex/fonts/KaTeX_Script-Regular.woff2":      "036d4e95149b69ff9bcc0cd55771efeb25ffa3947293e69acd78d5ac328c684b",
	"lib/katex/fonts/KaTeX_Size1-Regular.woff2":       "6b47c40166b6dbe21a5dfca7718413f2147fd2399be1ba605d8ad39cedf25dfe",
	"lib/katex/fonts/KaTeX_Size2-Regular.woff2":       "d04c54219f9eaec6d4d4fd42dfb28785975a4794d6b2fc71e566b9cd6db842dd",
	"lib/katex/fonts/KaTeX_Size3-Regular.woff2":       "73d591271b1604960cb10bb90fee021670af7297017e0e98480b332d11f51995",
	"lib/katex/fonts/KaTeX_Size4-Regular.woff2":       "a4af7d414440a1c1790825cfb700cf9cf43b0f2c4b04f0ebc523011ad9853ec0",
	"lib/katex/fonts/KaTeX_Typewriter-Regular.woff2":  "71d517d67827787cfabdf186914cc3358eda539e37931941f2b2fd4a21f68c0b",
	"lib/katex/katex.min.css":                         "551905675edbd85e78862903bc58724476d63478e0c129b527d8844ebad1c6dd",
	"lib/katex/katex.min.js":                          "155f6c2d673c5912e3b48f45d8830eaad18ae1953915939ceb648ea8b9c3e7e2",
}

func TestWebAssetsHaveNoInlineStylesOrScripts(t *testing.T) {
	assets := webFS()
	var scanned []string

	err := fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path+"/" == vendorDir {
				return fs.SkipDir // pinned by TestVendoredWebAssetsArePinned
			}
			return nil
		}
		// app.js builds its markup in strings, so it is scanned as markup too:
		// a style attribute written there is refused just the same.
		if !strings.HasSuffix(path, ".html") && !strings.HasSuffix(path, ".js") && !strings.HasSuffix(path, ".css") {
			return nil
		}
		body, err := fs.ReadFile(assets, path)
		if err != nil {
			return err
		}
		markup := string(body)
		scanned = append(scanned, path)

		if loc := inlineStyleAttr.FindStringIndex(markup); loc != nil {
			t.Errorf("%s: inline style attribute at byte %d; the CSP blocks it", path, loc[0])
		}
		if loc := inlineStyleTag.FindStringIndex(markup); loc != nil {
			t.Errorf("%s: inline <style> block at byte %d; the CSP blocks it", path, loc[0])
		}
		for _, tag := range inlineScriptTag.FindAllString(markup, -1) {
			if !strings.Contains(strings.ToLower(tag), "src=") {
				t.Errorf("%s: inline script %q; the CSP blocks it", path, tag)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking web assets: %v", err)
	}

	// The vendor exclusion must never quietly swallow the files that matter.
	for _, want := range []string{"index.html", "app.js", "app.css"} {
		if !slices.Contains(scanned, want) {
			t.Errorf("%s was not scanned; scanned %v", want, scanned)
		}
	}
}

func TestVendoredWebAssetsArePinned(t *testing.T) {
	assets := webFS()
	found := map[string]bool{}

	err := fs.WalkDir(assets, strings.TrimSuffix(vendorDir, "/"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		found[path] = true
		want, listed := vendoredWebAssets[path]
		if !listed {
			t.Errorf("%s is not in vendoredWebAssets; vet it and pin its hash before shipping it", path)
			return nil
		}
		body, err := fs.ReadFile(assets, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s has SHA-256 %s, pinned %s; a vendored file changed", path, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", vendorDir, err)
	}

	var missing []string
	for path := range vendoredWebAssets {
		if !found[path] {
			missing = append(missing, path)
		}
	}
	slices.Sort(missing)
	for _, path := range missing {
		t.Errorf("%s is pinned but not embedded", path)
	}
}

// A module zip, which is what `go install …@version` builds from, leaves out
// every directory named vendor, wherever it sits; anything embedded from one
// would be missing from such a build, and //go:embed would fail it.
func TestNoEmbeddedDirectoryIsNamedVendor(t *testing.T) {
	err := fs.WalkDir(webFS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "vendor" {
			t.Errorf("web/%s is named vendor; module zips drop it, so rename it", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking web assets: %v", err)
	}
}

// app.js loads the highlighter, KaTeX and the captcha worker by path at run
// time, so a moved or renamed file would fail only in a browser; this catches
// it at build time.
func TestAppLoadsVendoredScriptsThatExist(t *testing.T) {
	script, err := fs.ReadFile(webFS(), "app.js")
	if err != nil {
		t.Fatalf("reading app.js: %v", err)
	}
	refs := regexp.MustCompile(`"/(lib/[^"]+)"`).FindAllStringSubmatch(string(script), -1)
	var loaded []string
	for _, ref := range refs {
		loaded = append(loaded, ref[1])
		if _, listed := vendoredWebAssets[ref[1]]; !listed {
			t.Errorf("app.js loads /%s, which is not a pinned vendored file", ref[1])
		}
	}
	for _, want := range []string{
		"lib/altcha/pbkdf2.js",
		"lib/highlightjs/highlight.min.js",
		"lib/katex/katex.min.js",
		"lib/katex/katex.min.css",
	} {
		if !slices.Contains(loaded, want) {
			t.Errorf("app.js does not load /%s; found %v", want, loaded)
		}
	}
}

// A vendored stylesheet fetches what its url()s name, relative to itself — the
// KaTeX fonts — and a name with no file behind it is a 404 in every reader's
// network log and a fallback font in their maths. Every url() has to resolve to
// a pinned file under lib/.
func TestVendoredStylesheetsReferenceVendoredFiles(t *testing.T) {
	cssURL := regexp.MustCompile(`url\(\s*["']?([^"')]+)["']?\s*\)`)
	checked := 0
	for asset := range vendoredWebAssets {
		if !strings.HasSuffix(asset, ".css") {
			continue
		}
		body, err := fs.ReadFile(webFS(), asset)
		if err != nil {
			t.Fatalf("reading %s: %v", asset, err)
		}
		for _, ref := range cssURL.FindAllStringSubmatch(string(body), -1) {
			checked++
			target := path.Join(path.Dir(asset), ref[1])
			if strings.Contains(ref[1], ":") || !strings.HasPrefix(target, vendorDir) {
				t.Errorf("%s: url(%s) leaves lib/; a vendored stylesheet may only name vendored files", asset, ref[1])
				continue
			}
			if _, listed := vendoredWebAssets[target]; !listed {
				t.Errorf("%s: url(%s) names %s, which is not a pinned vendored file", asset, ref[1], target)
			}
		}
	}
	// KaTeX's stylesheet names its twenty fonts; finding none means the pattern
	// stopped matching, not that every reference is sound.
	if checked < 20 {
		t.Errorf("checked %d url() references, want at least the 20 KaTeX fonts", checked)
	}
}

// app.js is loaded as a module, and a module that declares one name twice at
// its top level is a SyntaxError: the browser runs none of it, so the whole UI
// is a blank page. Two features written apart can each bring a helper of the
// same name — a text spoiler's revealSpoiler and an image spoiler's did — and
// nothing short of a browser would notice. Top-level declarations in app.js
// start at the first column, so they can be read off line by line.
func TestAppDeclaresEachTopLevelNameOnce(t *testing.T) {
	script, err := fs.ReadFile(webFS(), "app.js")
	if err != nil {
		t.Fatalf("reading app.js: %v", err)
	}
	declaration := regexp.MustCompile(`^(?:async\s+)?(?:function\*?|const|let|var|class)\s+([A-Za-z_$][\w$]*)`)
	seen := map[string]int{}
	for n, line := range strings.Split(string(script), "\n") {
		m := declaration.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if first, dup := seen[m[1]]; dup {
			t.Errorf("app.js declares %s on line %d and again on line %d; the module will not load", m[1], first, n+1)
			continue
		}
		seen[m[1]] = n + 1
	}
	// app.js declares well over a hundred names; finding few means the
	// pattern stopped matching, not that the names are unique.
	if len(seen) < 100 {
		t.Errorf("found %d top-level declarations in app.js, want at least 100", len(seen))
	}
}

// The UI and the server each keep the list of top-level words that are never a
// board: the UI so it does not read a page's path as a board, the server so no
// board can take one. A word on one list only is either a board the UI cannot
// show or a page whose path a board can claim.
func TestUIReservesTheSameTopLevelWordsAsTheServer(t *testing.T) {
	script, err := fs.ReadFile(webFS(), "app.js")
	if err != nil {
		t.Fatalf("reading app.js: %v", err)
	}
	list := regexp.MustCompile(`(?s)const RESERVED_PATHS = new Set\(\[(.*?)\]\);`).FindSubmatch(script)
	if list == nil {
		t.Fatal("app.js has no RESERVED_PATHS set; this test reads it")
	}
	var ui []string
	for _, word := range regexp.MustCompile(`"([^"]*)"`).FindAllSubmatch(list[1], -1) {
		ui = append(ui, string(word[1]))
	}
	slices.Sort(ui)

	var server []string
	for word := range reservedBoardSlugs {
		server = append(server, word)
	}
	slices.Sort(server)
	if !slices.Equal(ui, server) {
		t.Errorf("app.js RESERVED_PATHS = %v, boards.go reservedBoardSlugs = %v; keep them the same", ui, server)
	}
}

// ageCheckWording is what the age screen must never be called. The screen asks
// a question and takes the answer on trust, and UK law says self-declaration
// does not count as checking anyone's age at all (Online Safety Act
// s.230(4)), so a name that says otherwise, in copy, a comment, or the docs,
// claims a protection educhan does not have. The institution badge's email
// verification is another thing and keeps its name: these match only the
// words next to "age".
var ageCheckWording = regexp.MustCompile(`(?i)\bage[\s_-]*(?:verif|assur)|\b(?:verif|assur)\w*\s+(?:\w+'?s?\s+)?age\b`)

// The files a reader or a later change takes the project's words from: its
// code, its pages, and its docs. Vendored libraries are not the project's
// words, and the rest is binary or generated.
var wordedFile = regexp.MustCompile(`\.(?:go|js|css|html|md|sql|txt|ya?ml|sh)$|(?:^|/)(?:Makefile|Dockerfile|\.env\.example)$`)

func TestAgeScreenIsNeverCalledVerification(t *testing.T) {
	// The pattern has to catch the wording it exists for. Each phrase is
	// assembled here so that this file does not trip the scan below.
	for _, phrase := range []string{
		"an age " + "verification screen", "Age" + "-verified", "age" + "Verified",
		"verify your " + "age", "verify " + "age", "age " + "assurance",
	} {
		if !ageCheckWording.MatchString(phrase) {
			t.Fatalf("ageCheckWording does not match %q", phrase)
		}
	}
	for _, fine := range []string{"Verify an institutional address", "message verified", "storage check", "the age screen"} {
		if ageCheckWording.MatchString(fine) {
			t.Fatalf("ageCheckWording matches %q, which is not about the age screen", fine)
		}
	}

	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("no go.mod above the test's directory; this test scans the repository from there")
		}
		root = parent
	}

	var scanned []string
	err = filepath.WalkDir(root, func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// Git's own files, local uploads (/data/, as .gitignore has it),
			// and third-party builds.
			if d.Name() == ".git" || rel == "data" || rel == "internal/app/web/"+strings.TrimSuffix(vendorDir, "/") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !wordedFile.MatchString(rel) {
			return nil
		}
		body, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		scanned = append(scanned, rel)
		for _, loc := range ageCheckWording.FindAllIndex(body, -1) {
			line := 1 + strings.Count(string(body[:loc[0]]), "\n")
			t.Errorf("%s:%d calls the age screen %q; it is a question, not a check", rel, line, body[loc[0]:loc[1]])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// A walk that stopped short would pass for the wrong reason.
	for _, want := range []string{"README.md", "AGENTS.md", "internal/app/web/app.js", "internal/app/web/index.html", "internal/app/web_test.go"} {
		if !slices.Contains(scanned, want) {
			t.Errorf("%s was not scanned", want)
		}
	}
}

func TestWebShellReferencesItsAssets(t *testing.T) {
	body, err := fs.ReadFile(webFS(), "index.html")
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	shell := string(body)

	// Each of these is load-bearing: the two asset URLs are what the shell is
	// for, the viewport tag is what makes the mobile layout apply at all, and
	// #main is the skip link's target.
	for _, want := range []string{`href="/app.css"`, `src="/app.js"`, `name="viewport"`, `id="main"`} {
		if !strings.Contains(shell, want) {
			t.Errorf("index.html is missing %s", want)
		}
	}
}

// The UI reads its route from the path, so a page load of any of its views has
// to come back as the shell: a board's views, a post link (where a cross-board
// >>>/meta/5 lands), the institution boards' /u/<name>/ routes, which the UI
// maps to the board stored as u-<name>, and the pages whose names are reserved
// as board slugs. None of them touch the database, so the app has none.
func TestPageLoadsServeTheShell(t *testing.T) {
	shell, err := fs.ReadFile(webFS(), "index.html")
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	routes := (&App{log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Routes()

	for _, path := range []string{
		"/", "/meta/", "/meta/catalog", "/meta/archive", "/meta/thread/1", "/meta/post/5",
		"/u/iitb/", "/u/iitb/catalog", "/u/iitb/thread/1", "/u/iitb/post/5",
		"/verify", "/rules", "/faq", "/legal", "/terms", "/privacy", "/safety",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Accept", "text/html,application/xhtml+xml")
			rec := httptest.NewRecorder()
			routes.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("content type = %q, want text/html", ct)
			}
			if rec.Body.String() != string(shell) {
				t.Errorf("body is not the shell: %.80q", rec.Body.String())
			}
			if rec.Header().Get("Content-Security-Policy") == "" {
				t.Error("the shell went out without its CSP")
			}
		})
	}

	// The /u/ alias is the UI's alone. The API addresses a board by the slug it
	// is stored under, and an unknown API path is a JSON 404, never the shell.
	req := httptest.NewRequest(http.MethodGet, "/api/u/iitb/threads", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); rec.Code != http.StatusNotFound || !strings.HasPrefix(ct, "application/json") {
		t.Errorf("GET /api/u/iitb/threads: status %d, %s; want a JSON 404", rec.Code, ct)
	}
}

// The assets' URLs carry no version, so a browser has to ask before reusing
// one, and the ETag is what lets the answer be a bodiless 304. Text goes out
// gzipped to a client that takes it, as a separate representation with its
// own ETag; fonts, already compressed, never do.
func TestWebAssetsRevalidateAndCompress(t *testing.T) {
	routes := (&App{log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Routes()
	get := func(path string, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		return rec
	}
	file := func(name string) []byte {
		t.Helper()
		body, err := fs.ReadFile(webFS(), name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return body
	}
	gunzip := func(body []byte) []byte {
		t.Helper()
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("not gzip: %v", err)
		}
		out, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		return out
	}

	for _, tc := range []struct {
		path, file, contentType string
		compressed              bool
	}{
		{"/app.js", "app.js", "text/javascript", true},
		{"/app.css", "app.css", "text/css", true},
		{"/lib/katex/katex.min.js", "lib/katex/katex.min.js", "text/javascript", true},
		{"/lib/highlightjs/highlight.min.js", "lib/highlightjs/highlight.min.js", "text/javascript", true},
		{"/lib/katex/fonts/KaTeX_Main-Regular.woff2", "lib/katex/fonts/KaTeX_Main-Regular.woff2", "font/woff2", false},
		// No extension: typed by sniffing, and too rarely read to compress.
		{"/lib/katex/LICENSE", "lib/katex/LICENSE", "text/plain", false},
		// A route of the UI is the shell, with the shell's validators.
		{"/", "index.html", "text/html", true},
		{"/meta/thread/1", "index.html", "text/html", true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			want := file(tc.file)

			plain := get(tc.path, nil)
			if plain.Code != http.StatusOK || !bytes.Equal(plain.Body.Bytes(), want) {
				t.Fatalf("plain GET: status %d, body matches %v", plain.Code, bytes.Equal(plain.Body.Bytes(), want))
			}
			if got := plain.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
				t.Errorf("Content-Type = %q, want %s", got, tc.contentType)
			}
			if got := plain.Header().Get("Cache-Control"); got != "no-cache" {
				t.Errorf("Cache-Control = %q, want no-cache: the URL is not versioned", got)
			}
			if got := plain.Header().Get("Content-Encoding"); got != "" {
				t.Errorf("Content-Encoding = %q to a client that asked for none", got)
			}
			if plain.Header().Get("Content-Security-Policy") == "" {
				t.Error("went out without the CSP")
			}
			etag := plain.Header().Get("ETag")
			if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || len(etag) < 10 {
				t.Fatalf("ETag = %q, want a strong validator", etag)
			}

			again := get(tc.path, map[string]string{"If-None-Match": etag})
			if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
				t.Errorf("revalidation: status %d with %d bytes, want an empty 304", again.Code, again.Body.Len())
			}

			zipped := get(tc.path, map[string]string{"Accept-Encoding": "br, gzip;q=0.8"})
			if !tc.compressed {
				if got := zipped.Header().Get("Content-Encoding"); got != "" {
					t.Errorf("Content-Encoding = %q for a file that is compressed already", got)
				}
				if got := zipped.Header().Get("ETag"); got != etag {
					t.Errorf("ETag = %q for the same bytes, want %q", got, etag)
				}
				return
			}
			if got := zipped.Header().Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip", got)
			}
			if !bytes.Equal(gunzip(zipped.Body.Bytes()), want) {
				t.Error("the gzipped body is not the file")
			}
			if !strings.Contains(zipped.Header().Get("Vary"), "Accept-Encoding") ||
				!strings.Contains(plain.Header().Get("Vary"), "Accept-Encoding") {
				t.Error("a compressible file must say it varies by Accept-Encoding, gzipped or not")
			}
			zipETag := zipped.Header().Get("ETag")
			if zipETag == etag || zipETag == "" {
				t.Errorf("the gzipped body's ETag = %q, want one of its own", zipETag)
			}
			if again := get(tc.path, map[string]string{"Accept-Encoding": "gzip", "If-None-Match": zipETag}); again.Code != http.StatusNotModified {
				t.Errorf("revalidating the gzipped copy: status %d, want 304", again.Code)
			}

			refused := get(tc.path, map[string]string{"Accept-Encoding": "gzip;q=0, deflate"})
			if refused.Header().Get("Content-Encoding") != "" || !bytes.Equal(refused.Body.Bytes(), want) {
				t.Error("gzip;q=0 still got a gzipped body")
			}
		})
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"":                      false,
		"gzip":                  true,
		"GZIP":                  true,
		"gzip, deflate, br":     true,
		"br;q=1.0, gzip;q=0.5":  true,
		"gzip;q=0":              false,
		"gzip; q=0.000":         false,
		"deflate":               false,
		"*":                     true,
		"*;q=0":                 false,
		"*, gzip;q=0":           false,
		"x-gzip":                true,
		"identity":              false,
		"gzip;q=nonsense":       false,
		"br, gzip;q=0.1, *;q=0": true,
	} {
		if got := acceptsGzip([]string{header}); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}
