package app

import (
	"io/fs"
	"regexp"
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

func TestWebAssetsHaveNoInlineStylesOrScripts(t *testing.T) {
	assets := webFS()

	err := fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		body, err := fs.ReadFile(assets, path)
		if err != nil {
			return err
		}
		markup := string(body)

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
