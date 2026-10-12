package app

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
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
	"vendor/highlightjs/LICENSE":          "6c081431591d9df696c82dc598fe1423765b8a299b200ed00b281afd0f64c490",
	"vendor/highlightjs/README.md":        "7092a29eb5f1990525239889a7b4e152d71679dcff114f28f6d0c505b9a3f9bf",
	"vendor/highlightjs/highlight.min.js": "8ab71eb09c51f501e5e25157d9cff100e46cc29bcbfc744d0b746d451fca7f53",
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
