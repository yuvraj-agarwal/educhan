package app

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
		"/verify", "/rules", "/faq", "/legal", "/terms", "/privacy",
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
