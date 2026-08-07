package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
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
		"bump_limit":   50,
		"thread_limit": 20,
	})
	want := Board{
		Slug: "comp-sci", Name: "Computer Science",
		Description: "Algorithms and all that", BumpLimit: 50, ThreadLimit: 20,
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

	// A name that is only whitespace is no name at all, so it takes the same
	// fallback as an omitted one.
	blank := c.createBoard(map[string]any{"slug": "blank-name", "name": "   "})
	if blank.Name != "blank-name" {
		t.Errorf("name = %q, want the slug when the given name is blank", blank.Name)
	}
}

func TestCreateBoardRejectsDuplicateSlug(t *testing.T) {
	c := modServer(t)

	var out map[string]any
	status := c.modBody(http.MethodPost, "/api/boards", testModKey,
		map[string]any{"slug": "general", "name": "General again"}, &out)
	if status != http.StatusConflict {
		t.Fatalf("duplicate slug status = %d, want 409", status)
	}

	// The seeded board must be untouched.
	var general Board
	c.get("/api/boards/general", &general)
	if general.Name != "General" {
		t.Errorf("existing board name = %q, want General", general.Name)
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
		{"reserved slug", map[string]any{"slug": "api"}},
		{"reserved media slug", map[string]any{"slug": "media"}},
		{"zero bump limit", map[string]any{"slug": "zero", "bump_limit": 0}},
		{"negative thread limit", map[string]any{"slug": "negative", "thread_limit": -1}},
		{"absurd thread limit", map[string]any{"slug": "absurd", "thread_limit": maxBoardThreadLimit + 1}},
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

	var before Board
	c.get("/api/boards/general", &before)

	var updated Board
	status := c.modBody(http.MethodPatch, "/api/boards/general", testModKey, map[string]any{
		"name":         "General Discussion",
		"description":  "Anything about campus life",
		"bump_limit":   42,
		"thread_limit": 7,
	}, &updated)
	if status != http.StatusOK {
		t.Fatalf("update status = %d, want 200", status)
	}

	want := Board{
		Slug: "general", Name: "General Discussion",
		Description: "Anything about campus life", BumpLimit: 42, ThreadLimit: 7,
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
}

func containsBoard(boards []Board, slug string) bool {
	for _, b := range boards {
		if b.Slug == slug {
			return true
		}
	}
	return false
}
