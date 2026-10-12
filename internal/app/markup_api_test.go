package app

import (
	"fmt"
	"strings"
	"testing"
)

// The server's half of post markup, end to end. Rendering is the front end's;
// what the server must get right is that a code block is not read as quotes and
// a spoiler does not leak into a catalog excerpt. Each test makes its own board
// rather than leaning on the seeded set.

func TestCodeBlocksMakeNoQuotesOrBacklinks(t *testing.T) {
	c := modServer(t)
	c.createBoard(map[string]any{"slug": "markup"})

	thread := c.createThread("markup", "shifts", "", "the original post")
	code := c.reply("markup", thread.ThreadNo, "",
		fmt.Sprintf("[code]\ny = x >>%d;\n[/code]", thread.No), false)
	mixed := c.reply("markup", thread.ThreadNo, "",
		fmt.Sprintf("[spoiler]>>%d[/spoiler] and [code]>>%d[/code]", thread.No, code.No), false)

	detail := fetchThread(t, c, "markup", thread.ThreadNo)
	op, codePost, mixedPost := detail.Posts[0], detail.Posts[1], detail.Posts[2]

	if len(codePost.Quotes) != 0 {
		t.Errorf("a shift inside [code] quoted %+v; code is not markup", codePost.Quotes)
	}
	// A spoiler's text is linked like any other; the code beside it is not.
	if len(mixedPost.Quotes) != 1 || mixedPost.Quotes[0].No != op.No {
		t.Errorf("mixed post quotes = %+v, want just the OP, from inside the spoiler", mixedPost.Quotes)
	}
	if len(op.Backlinks) != 1 || op.Backlinks[0].No != mixed.No {
		t.Errorf("op backlinks = %+v, want only the spoilered quote", op.Backlinks)
	}
	if len(codePost.Backlinks) != 0 {
		t.Errorf("a quote inside [code] made backlinks %+v", codePost.Backlinks)
	}
	// The body is stored as typed; markup is applied when it is shown.
	if !strings.HasPrefix(codePost.Body, "[code]\n") {
		t.Errorf("body was rewritten on the way in: %q", codePost.Body)
	}
}

func TestCatalogExcerptHidesSpoilers(t *testing.T) {
	c := modServer(t)
	c.createBoard(map[string]any{"slug": "markup"})

	c.createThread("markup", "film night", "",
		"the twist: [spoiler]the narrator was the villain[/spoiler]\n[code]print(1)[/code]")

	var catalog Catalog
	c.get("/api/boards/markup/catalog", &catalog)
	if len(catalog.Threads) != 1 {
		t.Fatalf("catalog has %d threads, want 1", len(catalog.Threads))
	}
	got := catalog.Threads[0].Excerpt
	if strings.Contains(got, "villain") {
		t.Errorf("excerpt %q gives the spoiler away", got)
	}
	if want := "the twist: [spoiler]\nprint(1)"; got != want {
		t.Errorf("excerpt = %q, want %q", got, want)
	}
}

func TestMathsMakesNoQuotesAndExcerptsAsTeX(t *testing.T) {
	c := modServer(t)
	c.createBoard(map[string]any{"slug": "markup"})

	thread := c.createThread("markup", "orders of growth", "", `if [math]a \gg b[/math] then`)
	reply := c.reply("markup", thread.ThreadNo, "",
		fmt.Sprintf("[math]x >>%d[/math]\n[eqn]\n>>%d\n[/eqn]", thread.No, thread.No), false)

	detail := fetchThread(t, c, "markup", thread.ThreadNo)
	op, replyPost := detail.Posts[0], detail.Posts[1]
	if replyPost.No != reply.No {
		t.Fatalf("second post is %d, want the reply %d", replyPost.No, reply.No)
	}
	if len(replyPost.Quotes) != 0 || len(op.Backlinks) != 0 {
		t.Errorf("TeX made quotes %+v and backlinks %+v; maths is not markup", replyPost.Quotes, op.Backlinks)
	}

	var catalog Catalog
	c.get("/api/boards/markup/catalog", &catalog)
	if len(catalog.Threads) != 1 {
		t.Fatalf("catalog has %d threads, want 1", len(catalog.Threads))
	}
	if got, want := catalog.Threads[0].Excerpt, `if a \gg b then`; got != want {
		t.Errorf("excerpt = %q, want %q", got, want)
	}
}
