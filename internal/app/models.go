package app

import (
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/media"
)

// Attachment is the image belonging to a post, as exposed by the API. Paths are
// rendered as URLs so clients never need to know how blobs are addressed.
type Attachment struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	ThumbURL    string `json:"thumb_url"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	ThumbWidth  int    `json:"thumb_width"`
	ThumbHeight int    `json:"thumb_height"`
	// Spoiler means the poster asked for the image to be covered until it is
	// clicked. ThumbURL is a real thumbnail regardless; covering it is the
	// client's job.
	Spoiler bool `json:"spoiler"`
}

// newAttachment is an image that has been written to the media store and is
// waiting for its database row.
type newAttachment struct {
	OriginalName string
	Image        media.StoredImage
	Spoiler      bool
}

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
	// ImageLimit is the most posts with an image a thread may hold, the opening
	// post included. Zero means replies can never carry one.
	ImageLimit int `json:"image_limit"`
	// ArchiveDays is how long an archived thread is kept before the sweeper
	// deletes it.
	ArchiveDays int `json:"archive_days"`
	// Rules is the board's own rules text, shown alongside the site-wide ones.
	Rules string `json:"rules"`
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
	// Badge is the institution the poster verified an address at, when they
	// chose to post with it. It says nothing about who they are beyond that,
	// and nothing links it back to the credential it was copied from.
	Badge string `json:"badge,omitempty"`
	// Attachment is the post's image, if it has one.
	Attachment *Attachment `json:"attachment,omitempty"`
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
	// Images is how many posts in the thread carry an image, the opening post
	// included. Once it reaches the board's image limit, replies can still be
	// posted but cannot bring another image.
	Images       int  `json:"images"`
	AtImageLimit bool `json:"at_image_limit"`
	// ArchivedAt is when the thread was pushed off its board, nil while it is
	// live. An archived thread still loads but takes no replies, and is
	// deleted with its images once its board's archive_days have passed.
	ArchivedAt *time.Time `json:"archived_at"`
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
	// Attachment is the opening post's image, shown as the tile's thumbnail.
	Attachment *Attachment `json:"attachment,omitempty"`
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

// ArchivedThread is a thread as listed in its board's archive: enough to
// recognise it and follow the link, with the opening post cut to a short
// excerpt.
type ArchivedThread struct {
	No         int64     `json:"no"`
	Subject    string    `json:"subject"`
	Excerpt    string    `json:"excerpt"`
	Replies    int       `json:"replies"`
	CreatedAt  time.Time `json:"created_at"`
	ArchivedAt time.Time `json:"archived_at"`
}

// Archive is a board's archived threads, most recently archived first.
type Archive struct {
	Board   Board            `json:"board"`
	Threads []ArchivedThread `json:"threads"`
}
