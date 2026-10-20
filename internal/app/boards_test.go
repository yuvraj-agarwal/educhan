package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// modBody sends an authenticated moderation request carrying a JSON body.
// Passing an empty key sends no credentials at all.
func (c *client) modBody(method, path, key string, body any, out any) int {
	c.t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		c.t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(encoded))
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return c.send(req, out)
}

// createBoard creates a board with the moderation key and fails unless it works.
func (c *client) createBoard(body any) Board {
	c.t.Helper()
	var out struct {
		Board
		Error string `json:"error"`
	}
	if status := c.modBody(http.MethodPost, "/api/boards", testModKey, body, &out); status != http.StatusCreated {
		c.t.Fatalf("create board: status %d (%s)", status, out.Error)
	}
	return out.Board
}

/* Creating ----------------------------------------------------------------- */

func TestCreateBoardRequiresTheKey(t *testing.T) {
	c := modServer(t)
	body := map[string]any{"slug": "unauthorised", "name": "Nope"}

	var out map[string]any
	if status := c.modBody(http.MethodPost, "/api/boards", "", body, &out); status != http.StatusForbidden {
		t.Errorf("no credentials: status = %d, want 403", status)
	}
	if status := c.modBody(http.MethodPost, "/api/boards", "wrong", body, &out); status != http.StatusForbidden {
		t.Errorf("wrong key: status = %d, want 403", status)
	}

	// The board must not exist, whichever way the request was refused.
	var boards struct {
		Boards []Board `json:"boards"`
	}
	c.get("/api/boards", &boards)
	for _, b := range boards.Boards {
		if b.Slug == "unauthorised" {
			t.Fatal("a refused request created the board anyway")
		}
	}
}

func TestCreateBoardAndPostToIt(t *testing.T) {
	c := modServer(t)

	created := c.createBoard(map[string]any{
		"slug":         "comp-sci",
		"name":         "Computer Science",
		"description":  "Algorithms and all that",
		"rules":        "1. Show your working.\n2. No homework dumps.",
		"bump_limit":   50,
		"thread_limit": 20,
		"image_limit":  30,
		"archive_days": 14,
	})
	want := Board{
		Slug: "comp-sci", Name: "Computer Science",
		Description: "Algorithms and all that", BumpLimit: 50, ThreadLimit: 20,
		ImageLimit: 30, ArchiveDays: 14, Rules: "1. Show your working.\n2. No homework dumps.",
	}
	if created != want {
		t.Errorf("created board = %+v, want %+v", created, want)
	}

	// The public views must see it exactly as they see a seeded board.
	var fetched Board
	c.get("/api/boards/comp-sci", &fetched)
	if fetched != want {
		t.Errorf("fetched board = %+v, want %+v", fetched, want)
	}

	var listed struct {
		Boards []Board `json:"boards"`
	}
	c.get("/api/boards", &listed)
	if !containsBoard(listed.Boards, "comp-sci") {
		t.Errorf("new board missing from the listing: %+v", listed.Boards)
	}

	// Posting, numbering, the index, and the catalog all work on it.
	thread := c.createThread("comp-sci", "first thread", "", "hello /comp-sci/")
	if thread.No != 1 {
		t.Errorf("first post on a new board = No. %d, want 1", thread.No)
	}
	reply := c.reply("comp-sci", thread.ThreadNo, "", "and a reply", false)
	if reply.No != 2 {
		t.Errorf("second post = No. %d, want 2", reply.No)
	}

	index := fetchIndex(t, c, "comp-sci", 1)
	if len(index.Threads) != 1 || index.Threads[0].No != thread.ThreadNo {
		t.Errorf("index does not show the new thread: %+v", index.Threads)
	}
	if index.Board != want {
		t.Errorf("index board = %+v, want %+v", index.Board, want)
	}

	var catalog Catalog
	c.get("/api/boards/comp-sci/catalog", &catalog)
	if len(catalog.Threads) != 1 || catalog.Threads[0].No != thread.ThreadNo {
		t.Errorf("catalog does not show the new thread: %+v", catalog.Threads)
	}
}

func TestCreateBoardDefaults(t *testing.T) {
	c := modServer(t)

	created := c.createBoard(map[string]any{"slug": "minimal"})
	if created.Name != "minimal" {
		t.Errorf("name = %q, want the slug when none is given", created.Name)
	}
	if created.Description != "" {
		t.Errorf("description = %q, want empty", created.Description)
	}
	if created.BumpLimit != defaultBoardBumpLimit || created.ThreadLimit != defaultBoardThreadLimit {
		t.Errorf("limits = %d/%d, want the schema defaults %d/%d",
			created.BumpLimit, created.ThreadLimit, defaultBoardBumpLimit, defaultBoardThreadLimit)
	}
	if created.ImageLimit != defaultBoardImageLimit || created.ArchiveDays != defaultBoardArchiveDays {
		t.Errorf("image limit and archive days = %d/%d, want the schema defaults %d/%d",
			created.ImageLimit, created.ArchiveDays, defaultBoardImageLimit, defaultBoardArchiveDays)
	}
	if created.Rules != "" {
		t.Errorf("rules = %q, want empty", created.Rules)
	}

	// The API defaults have to be the column defaults, or a board opened over
	// the API and one inserted by a migration would behave differently.
	c.addBoard("by-column")
	var inserted Board
	c.get("/api/boards/by-column", &inserted)
	if inserted.BumpLimit != created.BumpLimit || inserted.ThreadLimit != created.ThreadLimit ||
		inserted.ImageLimit != created.ImageLimit || inserted.ArchiveDays != created.ArchiveDays {
		t.Errorf("column defaults %+v differ from the API defaults %+v", inserted, created)
	}

	// A name that is only whitespace is no name at all, so it takes the same
	// fallback as an omitted one.
	blank := c.createBoard(map[string]any{"slug": "blank-name", "name": "   "})
	if blank.Name != "blank-name" {
		t.Errorf("name = %q, want the slug when the given name is blank", blank.Name)
	}
}

func TestCreateBoardRejectsDuplicateSlug(t *testing.T) {
	c := modServer(t)
	c.createBoard(map[string]any{"slug": "taken", "name": "First"})

	var out map[string]any
	status := c.modBody(http.MethodPost, "/api/boards", testModKey,
		map[string]any{"slug": "taken", "name": "Second"}, &out)
	if status != http.StatusConflict {
		t.Fatalf("duplicate slug status = %d, want 409", status)
	}

	// The existing board must be untouched.
	var taken Board
	c.get("/api/boards/taken", &taken)
	if taken.Name != "First" {
		t.Errorf("existing board name = %q, want First", taken.Name)
	}
}

func TestCreateBoardValidation(t *testing.T) {
	c := modServer(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing slug", map[string]any{"name": "No slug"}},
		{"blank slug", map[string]any{"slug": "   "}},
		{"slug with a slash", map[string]any{"slug": "a/b"}},
		{"slug with a space", map[string]any{"slug": "two words"}},
		{"slug with punctuation", map[string]any{"slug": "hey!"}},
		{"slug with a dot", map[string]any{"slug": "app.js"}},
		{"slug with a leading dash", map[string]any{"slug": "-lead"}},
		{"slug with a double dash", map[string]any{"slug": "a--b"}},
		{"slug too long", map[string]any{"slug": "abcdefghijklmnopqrstuvwxyz"}},
		{"zero bump limit", map[string]any{"slug": "zero", "bump_limit": 0}},
		{"negative thread limit", map[string]any{"slug": "negative", "thread_limit": -1}},
		{"absurd thread limit", map[string]any{"slug": "absurd", "thread_limit": maxBoardThreadLimit + 1}},
		{"negative image limit", map[string]any{"slug": "negative", "image_limit": -1}},
		{"absurd image limit", map[string]any{"slug": "absurd", "image_limit": maxBoardImageLimit + 1}},
		{"negative archive days", map[string]any{"slug": "negative", "archive_days": -1}},
		{"absurd archive days", map[string]any{"slug": "absurd", "archive_days": maxBoardArchiveDays + 1}},
		{"rules too long", map[string]any{"slug": "verbose", "rules": strings.Repeat("r", maxBoardRulesLen+1)}},
		{"image limit as a string", map[string]any{"slug": "stringly", "image_limit": "150"}},
		{"unknown field", map[string]any{"slug": "extra", "colour": "blue"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out map[string]any
			status := c.modBody(http.MethodPost, "/api/boards", testModKey, tc.body, &out)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%v)", status, out)
			}
			if out["error"] == nil || out["error"] == "" {
				t.Errorf("400 should explain itself, got %v", out)
			}
		})
	}
}

// Every first path segment that means something other than a board is closed
// to boards, whatever its case: the server's own routes, the UI's pages, and
// "u", the prefix of institution boards' /u/<name>/ routes. The u-<name> slugs
// those boards are stored under are ordinary slugs.
func TestReservedBoardSlugs(t *testing.T) {
	c := modServer(t)

	reserved := []string{
		"api", "media", "healthz", "lib", "verify", "pass", "mod", "u", "rules",
		"faq", "legal", "terms", "privacy", "dmca", "report", "archive", "safety",
	}
	for _, slug := range reserved {
		for _, given := range []string{slug, strings.ToUpper(slug)} {
			t.Run(given, func(t *testing.T) {
				var out map[string]any
				status := c.modBody(http.MethodPost, "/api/boards", testModKey, map[string]any{"slug": given}, &out)
				if status != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "reserved") {
					t.Errorf("status = %d (%v), want 400 because %q is reserved", status, out["error"], slug)
				}
			})
		}
	}

	institution := c.createBoard(map[string]any{"slug": "u-iitb", "name": "IIT Bombay"})
	if institution.Slug != "u-iitb" {
		t.Fatalf("slug = %q, want u-iitb", institution.Slug)
	}
	thread := c.createThread("u-iitb", "first", "", "an institution board is a board")
	if detail := fetchThread(t, c, "u-iitb", thread.ThreadNo); detail.Board.Slug != "u-iitb" {
		t.Errorf("thread is on /%s/, want /u-iitb/", detail.Board.Slug)
	}
}

// A slug is normalised the way it will be addressed, so /CompSci/ and /compsci/
// cannot both exist.
func TestCreateBoardNormalisesSlug(t *testing.T) {
	c := modServer(t)

	created := c.createBoard(map[string]any{"slug": "  MixedCase  "})
	if created.Slug != "mixedcase" {
		t.Fatalf("slug = %q, want mixedcase", created.Slug)
	}

	var out map[string]any
	if status := c.modBody(http.MethodPost, "/api/boards", testModKey,
		map[string]any{"slug": "MIXEDCASE"}, &out); status != http.StatusConflict {
		t.Errorf("status = %d, want 409: the normalised slug is taken", status)
	}
}

/* Updating ----------------------------------------------------------------- */

func TestUpdateBoardRequiresTheKey(t *testing.T) {
	c := modServer(t)
	c.addBoard("general")
	body := map[string]any{"name": "Hijacked"}

	var out map[string]any
	if status := c.modBody(http.MethodPatch, "/api/boards/general", "", body, &out); status != http.StatusForbidden {
		t.Errorf("no credentials: status = %d, want 403", status)
	}
	if status := c.modBody(http.MethodPatch, "/api/boards/general", "wrong", body, &out); status != http.StatusForbidden {
		t.Errorf("wrong key: status = %d, want 403", status)
	}

	var general Board
	c.get("/api/boards/general", &general)
	if general.Name == "Hijacked" {
		t.Fatal("a refused request updated the board anyway")
	}
}

func TestUpdateBoardSettings(t *testing.T) {
	c := modServer(t)
	c.addBoard("general")

	var before Board
	c.get("/api/boards/general", &before)

	var updated Board
	status := c.modBody(http.MethodPatch, "/api/boards/general", testModKey, map[string]any{
		"name":         "General Discussion",
		"description":  "Anything about campus life",
		"rules":        "Be kind.",
		"bump_limit":   42,
		"thread_limit": 7,
		"image_limit":  12,
		"archive_days": 3,
	}, &updated)
	if status != http.StatusOK {
		t.Fatalf("update status = %d, want 200", status)
	}

	want := Board{
		Slug: "general", Name: "General Discussion",
		Description: "Anything about campus life", BumpLimit: 42, ThreadLimit: 7,
		ImageLimit: 12, ArchiveDays: 3, Rules: "Be kind.",
	}
	if updated != want {
		t.Errorf("update returned %+v, want %+v", updated, want)
	}

	var fetched Board
	c.get("/api/boards/general", &fetched)
	if fetched != want {
		t.Errorf("stored board = %+v, want %+v", fetched, want)
	}
}

// A partial update must leave the settings it does not name alone.
func TestUpdateBoardIsPartial(t *testing.T) {
	c := modServer(t)
	c.addBoard("general")

	var before Board
	c.get("/api/boards/general", &before)

	var updated Board
	status := c.modBody(http.MethodPatch, "/api/boards/general", testModKey,
		map[string]any{"description": "Only the description changes"}, &updated)
	if status != http.StatusOK {
		t.Fatalf("update status = %d, want 200", status)
	}

	want := before
	want.Description = "Only the description changes"
	if updated != want {
		t.Errorf("update returned %+v, want %+v", updated, want)
	}
}

// The thread limit is not just stored: the existing pruning path has to read it.
func TestUpdateBoardThreadLimitPrunes(t *testing.T) {
	c := modServer(t)
	c.addBoard("general")

	const limit = 2
	var updated Board
	if status := c.modBody(http.MethodPatch, "/api/boards/general", testModKey,
		map[string]any{"thread_limit": limit}, &updated); status != http.StatusOK {
		t.Fatalf("update status = %d, want 200", status)
	}

	var kept []int64
	for i := 0; i < limit+2; i++ {
		kept = append(kept, c.createThread("general", "", "", fmt.Sprintf("thread %d", i)).ThreadNo)
	}

	index := fetchIndex(t, c, "general", 1)
	if len(index.Threads) != limit {
		t.Fatalf("board holds %d threads, want the new limit of %d", len(index.Threads), limit)
	}
	survivors := map[int64]bool{}
	for _, thread := range index.Threads {
		survivors[thread.No] = true
	}
	for _, no := range kept[len(kept)-limit:] {
		if !survivors[no] {
			t.Errorf("thread No. %d should have survived pruning", no)
		}
	}
}

func TestUpdateBoardValidation(t *testing.T) {
	c := modServer(t)
	c.addBoard("general")

	cases := []struct {
		name       string
		path       string
		body       map[string]any
		wantStatus int
	}{
		{"unknown board", "/api/boards/nosuchboard", map[string]any{"name": "Ghost"}, http.StatusNotFound},
		{"no settings", "/api/boards/general", map[string]any{}, http.StatusBadRequest},
		{"blank name", "/api/boards/general", map[string]any{"name": "  "}, http.StatusBadRequest},
		{"zero bump limit", "/api/boards/general", map[string]any{"bump_limit": 0}, http.StatusBadRequest},
		{"negative thread limit", "/api/boards/general", map[string]any{"thread_limit": -5}, http.StatusBadRequest},
		{"negative image limit", "/api/boards/general", map[string]any{"image_limit": -1}, http.StatusBadRequest},
		{"absurd image limit", "/api/boards/general", map[string]any{"image_limit": maxBoardImageLimit + 1}, http.StatusBadRequest},
		{"negative archive days", "/api/boards/general", map[string]any{"archive_days": -1}, http.StatusBadRequest},
		{"absurd archive days", "/api/boards/general", map[string]any{"archive_days": maxBoardArchiveDays + 1}, http.StatusBadRequest},
		{"rules too long", "/api/boards/general", map[string]any{"rules": strings.Repeat("r", maxBoardRulesLen+1)}, http.StatusBadRequest},
		{"slug is not updatable", "/api/boards/general", map[string]any{"slug": "renamed"}, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out map[string]any
			status := c.modBody(http.MethodPatch, tc.path, testModKey, tc.body, &out)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%v)", status, tc.wantStatus, out)
			}
		})
	}

	// None of the refused updates may have landed.
	var general Board
	c.get("/api/boards/general", &general)
	if general.Slug != "general" || general.Name == "" || general.BumpLimit < 1 || general.ThreadLimit < 1 {
		t.Errorf("board damaged by refused updates: %+v", general)
	}
	if general.ImageLimit != defaultBoardImageLimit || general.ArchiveDays != defaultBoardArchiveDays ||
		general.Rules != "" {
		t.Errorf("board settings changed by refused updates: %+v", general)
	}
}

// Zero is a real value for the image cap and the archive period, at both ends
// of the API, and the maximum is accepted as well as refused one past it.
func TestBoardSettingsBoundaries(t *testing.T) {
	c := modServer(t)

	zero := c.createBoard(map[string]any{"slug": "text-only", "image_limit": 0, "archive_days": 0})
	if zero.ImageLimit != 0 || zero.ArchiveDays != 0 {
		t.Errorf("created image limit and archive days = %d/%d, want 0/0", zero.ImageLimit, zero.ArchiveDays)
	}

	var updated Board
	if status := c.modBody(http.MethodPatch, "/api/boards/text-only", testModKey, map[string]any{
		"image_limit": maxBoardImageLimit, "archive_days": maxBoardArchiveDays,
	}, &updated); status != http.StatusOK {
		t.Fatalf("update to the maxima: status %d", status)
	}
	if updated.ImageLimit != maxBoardImageLimit || updated.ArchiveDays != maxBoardArchiveDays {
		t.Errorf("updated image limit and archive days = %d/%d, want %d/%d",
			updated.ImageLimit, updated.ArchiveDays, maxBoardImageLimit, maxBoardArchiveDays)
	}

	// And back to zero through PATCH, which a pointer field has to tell apart
	// from the setting being left out.
	if status := c.modBody(http.MethodPatch, "/api/boards/text-only", testModKey,
		map[string]any{"image_limit": 0, "archive_days": 0}, &updated); status != http.StatusOK {
		t.Fatalf("update to zero: status %d", status)
	}
	if updated.ImageLimit != 0 || updated.ArchiveDays != 0 {
		t.Errorf("updated image limit and archive days = %d/%d, want 0/0", updated.ImageLimit, updated.ArchiveDays)
	}
}

// Rules are free text, normalised like a post body: line endings, stray
// control characters, trailing spaces, and blank runs, so what is stored is
// what every client will render.
func TestBoardRulesAreNormalised(t *testing.T) {
	c := modServer(t)

	created := c.createBoard(map[string]any{
		"slug":  "ruled",
		"rules": "  1. Be kind.   \r\n\r\n\r\n\r\n2. No\x00 doxxing.\t\n  ",
	})
	if want := "1. Be kind.\n\n2. No doxxing."; created.Rules != want {
		t.Errorf("rules = %q, want %q", created.Rules, want)
	}

	// The cap counts characters, not bytes: a rules page in Devanagari is as
	// long as one in English.
	long := strings.Repeat("\u0928", maxBoardRulesLen)
	var updated Board
	if status := c.modBody(http.MethodPatch, "/api/boards/ruled", testModKey,
		map[string]any{"rules": long}, &updated); status != http.StatusOK {
		t.Fatalf("rules at the cap: status %d", status)
	}
	if updated.Rules != long {
		t.Errorf("rules at the cap were altered")
	}

	// Clearing them is an update like any other, not "no settings given".
	if status := c.modBody(http.MethodPatch, "/api/boards/ruled", testModKey,
		map[string]any{"rules": ""}, &updated); status != http.StatusOK {
		t.Fatalf("clearing rules: status %d", status)
	}
	if updated.Rules != "" {
		t.Errorf("rules = %q, want them cleared", updated.Rules)
	}
}

func containsBoard(boards []Board, slug string) bool {
	for _, b := range boards {
		if b.Slug == slug {
			return true
		}
	}
	return false
}
