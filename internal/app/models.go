package app

import "time"

// Board is a top-level category threads are posted under.
type Board struct {
	ID          int    `json:"-"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// BumpLimit is the number of replies after which a thread stops moving back
	// to the top of the board.
	BumpLimit int `json:"bump_limit"`
	// ThreadLimit is how many threads a board keeps before the least recently
	// bumped ones are pruned.
	ThreadLimit int `json:"thread_limit"`
}

// Post is a single message. Posts are numbered per board ("No. 42"), which is
// the identifier posters quote and the API exposes in URLs.
type Post struct {
	ID        int64     `json:"-"`
	No        int64     `json:"no"`
	ThreadNo  int64     `json:"thread_no"`
	Name      string    `json:"name"`
	Tripcode  string    `json:"tripcode,omitempty"`
	PosterID  string    `json:"poster_id"`
	Body      string    `json:"body"`
	OP        bool      `json:"op"`
	CreatedAt time.Time `json:"created_at"`
	// Quotes are the posts this post links to; Backlinks are the posts linking
	// back to it. Both are limited to the current board.
	Quotes    []PostRef `json:"quotes"`
	Backlinks []PostRef `json:"backlinks"`
}

// PostRef points at a post elsewhere on the same board, carrying enough context
// for a client to build a link to it.
type PostRef struct {
	No       int64 `json:"no"`
	ThreadNo int64 `json:"thread_no"`
}

// Thread is the shared shape of a thread in every view. Reply counts and
// timestamps are maintained on write so listings need no aggregation.
type Thread struct {
	ID          int64     `json:"-"`
	No          int64     `json:"no"`
	Subject     string    `json:"subject"`
	Sticky      bool      `json:"sticky"`
	Locked      bool      `json:"locked"`
	Replies     int       `json:"replies"`
	CreatedAt   time.Time `json:"created_at"`
	BumpedAt    time.Time `json:"bumped_at"`
	LastPostAt  time.Time `json:"last_post_at"`
	AtBumpLimit bool      `json:"at_bump_limit"`
}

// ThreadSummary is a thread as shown on a board index page: the opening post
// plus its most recent replies.
type ThreadSummary struct {
	Thread
	OP Post `json:"op"`
	// OmittedReplies is how many replies are not included in LastReplies.
	OmittedReplies int    `json:"omitted_replies"`
	LastReplies    []Post `json:"last_replies"`
}

// CatalogThread is a thread as shown in the catalog: a dense tile with the
// opening post reduced to an excerpt.
type CatalogThread struct {
	Thread
	Excerpt string `json:"excerpt"`
}

// ThreadDetail is a thread with all of its posts, used by the thread view.
type ThreadDetail struct {
	Thread
	Board Board  `json:"board"`
	Posts []Post `json:"posts"`
}

// BoardIndex is one page of a board's threads, newest bump first.
type BoardIndex struct {
	Board   Board           `json:"board"`
	Page    int             `json:"page"`
	Pages   int             `json:"pages"`
	Threads []ThreadSummary `json:"threads"`
}

// Catalog is every live thread on a board, ordered like the index.
type Catalog struct {
	Board   Board           `json:"board"`
	Threads []CatalogThread `json:"threads"`
}
