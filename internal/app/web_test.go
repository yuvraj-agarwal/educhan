package app

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"path"
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
const vendorDir = "vendor/"

// vendoredWebAssets is every file under web/vendor/, with its SHA-256. The
// inline-markup scan is a heuristic for markup this project writes, and a
// minified library — whose grammars and templates are full of strings like
// "<style" and "<script" — is not that, so vendored files are vetted here
// instead: a file reaches vendor/ only by being listed, and a listed file cannot
// change without its hash changing here too. Updating a library means reading
// the new build, then updating its README.md and its lines below.
var vendoredWebAssets = map[string]string{
	"vendor/highlightjs/LICENSE":                         "6c081431591d9df696c82dc598fe1423765b8a299b200ed00b281afd0f64c490",
	"vendor/highlightjs/README.md":                       "7092a29eb5f1990525239889a7b4e152d71679dcff114f28f6d0c505b9a3f9bf",
	"vendor/highlightjs/highlight.min.js":                "8ab71eb09c51f501e5e25157d9cff100e46cc29bcbfc744d0b746d451fca7f53",
	"vendor/katex/LICENSE":                               "766ccc1f306c885aa45542a9846bbd0a505b27a0374f146778171c2254ce18e3",
	"vendor/katex/README.md":                             "55d0e20b916ad68f30b754c78d5cd1dfe0fed00dc4ec9418f04d7eee7dd8e1af",
	"vendor/katex/fonts/KaTeX_AMS-Regular.woff2":         "0cdd387c9590a1a9f9794560022dbb59654a7d86f187aa0c81495ad42d3a7308",
	"vendor/katex/fonts/KaTeX_Caligraphic-Bold.woff2":    "de7701e42cf1f4cf0b766c03fb27977207eee2f4fd5d76fa82188406da43ea4c",
	"vendor/katex/fonts/KaTeX_Caligraphic-Regular.woff2": "5d53e70ad607c2352162dec9e0923fb54ecdafaccbf604cd8dcf7d00facb989b",
	"vendor/katex/fonts/KaTeX_Fraktur-Bold.woff2":        "74444efd593c005e3f4573b44524704c0af0a937fe911cca9e94068d0d140d3f",
	"vendor/katex/fonts/KaTeX_Fraktur-Regular.woff2":     "51814d270d06ff0255dba0799994fa4d8c84d11f09951d47595f4abb1f3602dc",
	"vendor/katex/fonts/KaTeX_Main-Bold.woff2":           "0f60d1b897938ec918c8ce073092411baf9438f6739465693ff18b0f9d20b021",
	"vendor/katex/fonts/KaTeX_Main-BoldItalic.woff2":     "99cd42a3c072d918f2f44984a807cf7aa16e13545fd0875fc07c6c65f99e715b",
	"vendor/katex/fonts/KaTeX_Main-Italic.woff2":         "97479ca6cce906abc961ecac96faa5f9ca2e61b8e7670d475826bcdee9a7c267",
	"vendor/katex/fonts/KaTeX_Main-Regular.woff2":        "c2342cd8b869e01752a9321dc17213fc40d4d04c79688c1d43f2cf316abd7866",
	"vendor/katex/fonts/KaTeX_Math-BoldItalic.woff2":     "dc47344dbb6cb5b655c8460d561f4df5f501b90c804ad3c6cec65fe322351ab1",
	"vendor/katex/fonts/KaTeX_Math-Italic.woff2":         "7af58c5ec8f132a2ddde9027c6d7814decce4d3b822a11192a42a20e2e973264",
	"vendor/katex/fonts/KaTeX_SansSerif-Bold.woff2":      "e99ae51144bf1232efcc1bfe5add36262c6866b0faab24fa75740e1b98577a62",
	"vendor/katex/fonts/KaTeX_SansSerif-Italic.woff2":    "00b26ac825e2095056396e0553b8ac26d3f8ad158c3826e28b4c45b385c4714a",
	"vendor/katex/fonts/KaTeX_SansSerif-Regular.woff2":   "68e8c73ef42afd3ccec58bf0fba302cce448938e7fc020a5e31f8a952eee1342",
	"vendor/katex/fonts/KaTeX_Script-Regular.woff2":      "036d4e95149b69ff9bcc0cd55771efeb25ffa3947293e69acd78d5ac328c684b",
	"vendor/katex/fonts/KaTeX_Size1-Regular.woff2":       "6b47c40166b6dbe21a5dfca7718413f2147fd2399be1ba605d8ad39cedf25dfe",
	"vendor/katex/fonts/KaTeX_Size2-Regular.woff2":       "d04c54219f9eaec6d4d4fd42dfb28785975a4794d6b2fc71e566b9cd6db842dd",
	"vendor/katex/fonts/KaTeX_Size3-Regular.woff2":       "73d591271b1604960cb10bb90fee021670af7297017e0e98480b332d11f51995",
	"vendor/katex/fonts/KaTeX_Size4-Regular.woff2":       "a4af7d414440a1c1790825cfb700cf9cf43b0f2c4b04f0ebc523011ad9853ec0",
	"vendor/katex/fonts/KaTeX_Typewriter-Regular.woff2":  "71d517d67827787cfabdf186914cc3358eda539e37931941f2b2fd4a21f68c0b",
	"vendor/katex/katex.min.css":                         "551905675edbd85e78862903bc58724476d63478e0c129b527d8844ebad1c6dd",
	"vendor/katex/katex.min.js":                          "155f6c2d673c5912e3b48f45d8830eaad18ae1953915939ceb648ea8b9c3e7e2",
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

// app.js loads the highlighter and KaTeX by path at run time, so a moved or
// renamed file would fail only in a browser; this catches it at build time.
func TestAppLoadsVendoredScriptsThatExist(t *testing.T) {
	script, err := fs.ReadFile(webFS(), "app.js")
	if err != nil {
		t.Fatalf("reading app.js: %v", err)
	}
	refs := regexp.MustCompile(`"/(vendor/[^"]+)"`).FindAllStringSubmatch(string(script), -1)
	var loaded []string
	for _, ref := range refs {
		loaded = append(loaded, ref[1])
		if _, listed := vendoredWebAssets[ref[1]]; !listed {
			t.Errorf("app.js loads /%s, which is not a pinned vendored file", ref[1])
		}
	}
	for _, want := range []string{
		"vendor/highlightjs/highlight.min.js",
		"vendor/katex/katex.min.js",
		"vendor/katex/katex.min.css",
	} {
		if !slices.Contains(loaded, want) {
			t.Errorf("app.js does not load /%s; found %v", want, loaded)
		}
	}
}

// A vendored stylesheet fetches what its url()s name, relative to itself — the
// KaTeX fonts — and a name with no file behind it is a 404 in every reader's
// network log and a fallback font in their maths. Every url() has to resolve to
// a pinned file under vendor/.
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
				t.Errorf("%s: url(%s) leaves vendor/; a vendored stylesheet may only name vendored files", asset, ref[1])
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
