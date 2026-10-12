package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yuvraj-agarwal/educhan/internal/media"
)

// Sentinel errors returned by the store and mapped to HTTP statuses by the
// handlers.
var (
	errNotFound       = errors.New("not found")
	errThreadLocked   = errors.New("thread is locked")
	errThreadArchived = errors.New("thread is archived")
	errImageLimit     = errors.New("image limit reached")
	errConflict       = errors.New("already exists")
)

// uniqueViolation is the PostgreSQL SQLSTATE for a unique constraint failure,
// which is how a board slug that is already taken comes back.
const uniqueViolation = "23505"

// indexPageSize and indexRepliesPerThread mirror the classic imageboard index:
// a handful of threads per page, each showing its opening post and the tail of
// the conversation.
const (
	indexPageSize         = 10
	indexRepliesPerThread = 3
	catalogLimit          = 200
)

// archiveLimit bounds a board's archive listing, and archiveExcerptLen the
// excerpt each entry carries. A busy board with a long archive_days can hold
// thousands of archived threads; the listing shows the most recent of them,
// and the rest still load by number until the sweeper takes them.
const (
	archiveLimit      = 3000
	archiveExcerptLen = 120
)

// Store is the PostgreSQL-backed persistence layer.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps an existing pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const boardColumns = `b.id, b.slug, b.name, b.description, b.bump_limit, b.thread_limit,
	b.image_limit, b.archive_days, b.rules`

func scanBoard(row pgx.Row) (Board, error) {
	var b Board
	err := row.Scan(&b.ID, &b.Slug, &b.Name, &b.Description, &b.BumpLimit, &b.ThreadLimit,
		&b.ImageLimit, &b.ArchiveDays, &b.Rules)
	return b, err
}

// Boards lists every board in display order.
func (s *Store) Boards(ctx context.Context) ([]Board, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+boardColumns+` FROM boards b ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	boards := []Board{}
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			return nil, err
		}
		boards = append(boards, b)
	}
	return boards, rows.Err()
}

// Board looks a board up by slug, returning errNotFound when it does not exist.
func (s *Store) Board(ctx context.Context, slug string) (Board, error) {
	b, err := scanBoard(s.pool.QueryRow(ctx, `SELECT `+boardColumns+` FROM boards b WHERE b.slug = $1`, slug))
	if errors.Is(err, pgx.ErrNoRows) {
		return Board{}, errNotFound
	}
	return b, err
}

// BoardSettings is a partial board update. A nil field keeps its stored value,
// so a caller can change one setting without reading the others first.
type BoardSettings struct {
	Name        *string
	Description *string
	BumpLimit   *int
	ThreadLimit *int
	ImageLimit  *int
	ArchiveDays *int
	Rules       *string
}

// CreateBoard inserts a board, returning errConflict when the slug is taken.
// next_post_no is left to its column default, so the new board starts numbering
// at No. 1 like every seeded one.
func (s *Store) CreateBoard(ctx context.Context, in Board) (Board, error) {
	b, err := scanBoard(s.pool.QueryRow(ctx, `
		INSERT INTO boards AS b (slug, name, description, bump_limit, thread_limit, image_limit, archive_days, rules)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+boardColumns,
		in.Slug, in.Name, in.Description, in.BumpLimit, in.ThreadLimit, in.ImageLimit, in.ArchiveDays, in.Rules))
	if isUniqueViolation(err) {
		return Board{}, errConflict
	}
	return b, err
}

// UpdateBoard applies a partial update, returning errNotFound when no board has
// the slug. The slug itself is not updatable: it is the board's address, in
// every stored quote link and every bookmark pointing at it.
func (s *Store) UpdateBoard(ctx context.Context, slug string, in BoardSettings) (Board, error) {
	b, err := scanBoard(s.pool.QueryRow(ctx, `
		UPDATE boards AS b SET
		    name         = COALESCE($2::text, b.name),
		    description  = COALESCE($3::text, b.description),
		    bump_limit   = COALESCE($4::integer, b.bump_limit),
		    thread_limit = COALESCE($5::integer, b.thread_limit),
		    image_limit  = COALESCE($6::integer, b.image_limit),
		    archive_days = COALESCE($7::integer, b.archive_days),
		    rules        = COALESCE($8::text, b.rules)
		WHERE b.slug = $1
		RETURNING `+boardColumns,
		slug, in.Name, in.Description, in.BumpLimit, in.ThreadLimit, in.ImageLimit, in.ArchiveDays, in.Rules))
	if errors.Is(err, pgx.ErrNoRows) {
		return Board{}, errNotFound
	}
	return b, err
}

// isUniqueViolation reports whether err is a PostgreSQL unique constraint
// failure.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

// Index returns one page of a board's threads. Pages are 1-based; a page past
// the end comes back empty rather than as an error.
func (s *Store) Index(ctx context.Context, board Board, page int) (BoardIndex, error) {
	if page < 1 {
		page = 1
	}

	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM threads WHERE board_id = $1 AND archived_at IS NULL`, board.ID).Scan(&total); err != nil {
		return BoardIndex{}, err
	}

	index := BoardIndex{
		Board:   board,
		Page:    page,
		Pages:   max((total+indexPageSize-1)/indexPageSize, 1),
		Threads: []ThreadSummary{},
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+threadColumns+`
		FROM threads t
		WHERE t.board_id = $1 AND t.archived_at IS NULL
		ORDER BY t.sticky DESC, t.bumped_at DESC
		LIMIT $2 OFFSET $3`, board.ID, indexPageSize, (page-1)*indexPageSize)
	if err != nil {
		return BoardIndex{}, err
	}
	defer rows.Close()

	var threadIDs []int64
	for rows.Next() {
		t, err := scanThread(rows, board)
		if err != nil {
			return BoardIndex{}, err
		}
		index.Threads = append(index.Threads, ThreadSummary{Thread: t, LastReplies: []Post{}})
		threadIDs = append(threadIDs, t.ID)
	}
	if err := rows.Err(); err != nil {
		return BoardIndex{}, err
	}
	if len(threadIDs) == 0 {
		return index, nil
	}

	// index.Threads is not appended to again, so pointers into it stay valid.
	byThreadID := make(map[int64]*ThreadSummary, len(index.Threads))
	for i := range index.Threads {
		byThreadID[index.Threads[i].ID] = &index.Threads[i]
	}

	ops, err := s.opPosts(ctx, threadIDs)
	if err != nil {
		return BoardIndex{}, err
	}
	for threadID, op := range ops {
		if summary, ok := byThreadID[threadID]; ok {
			summary.OP = op
		}
	}

	tails, err := s.lastReplies(ctx, threadIDs, indexRepliesPerThread)
	if err != nil {
		return BoardIndex{}, err
	}
	for threadID, replies := range tails {
		summary, ok := byThreadID[threadID]
		if !ok {
			continue
		}
		summary.LastReplies = replies
		summary.OmittedReplies = max(summary.Replies-len(replies), 0)
	}

	postIDs := []int64{}
	byPostID := map[int64]*Post{}
	for i := range index.Threads {
		summary := &index.Threads[i]
		for _, p := range append([]*Post{&summary.OP}, pointersTo(summary.LastReplies)...) {
			postIDs = append(postIDs, p.ID)
			byPostID[p.ID] = p
		}
	}
	if err := s.attachQuotes(ctx, postIDs, byPostID); err != nil {
		return BoardIndex{}, err
	}
	return index, nil
}

func pointersTo(posts []Post) []*Post {
	out := make([]*Post, len(posts))
	for i := range posts {
		out[i] = &posts[i]
	}
	return out
}

// Catalog returns every live thread on a board with its opening post reduced to
// an excerpt, ordered like the index.
func (s *Store) Catalog(ctx context.Context, board Board) (Catalog, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+threadColumns+`, p.body, `+attachmentColumns+`
		FROM threads t
		JOIN posts p ON p.thread_id = t.id AND p.is_op
		LEFT JOIN attachments a ON a.post_id = p.id
		WHERE t.board_id = $1 AND t.archived_at IS NULL
		ORDER BY t.sticky DESC, t.bumped_at DESC
		LIMIT $2`, board.ID, catalogLimit)
	if err != nil {
		return Catalog{}, err
	}
	defer rows.Close()

	catalog := Catalog{Board: board, Threads: []CatalogThread{}}
	for rows.Next() {
		var (
			t    Thread
			body string
			att  attachmentRow
		)
		if err := rows.Scan(threadDest(&t, append([]any{&body}, att.dest()...)...)...); err != nil {
			return Catalog{}, err
		}
		t.applyLimits(board)
		catalog.Threads = append(catalog.Threads, CatalogThread{
			Thread:     t,
			Excerpt:    excerpt(body, excerptLen),
			Attachment: att.toAttachment(),
		})
	}
	return catalog, rows.Err()
}

// Archive lists a board's archived threads, most recently archived first and
// capped at archiveLimit. Only the start of each opening post leaves the
// database: the listing shows an excerpt, and a few thousand whole bodies could
// run to megabytes.
func (s *Store) Archive(ctx context.Context, board Board) (Archive, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.no, t.subject, t.reply_count, t.created_at, t.archived_at, left(p.body, $3)
		FROM threads t
		JOIN posts p ON p.thread_id = t.id AND p.is_op
		WHERE t.board_id = $1 AND t.archived_at IS NOT NULL
		ORDER BY t.archived_at DESC, t.no DESC
		LIMIT $2`, board.ID, archiveLimit, archiveExcerptLen+1)
	if err != nil {
		return Archive{}, err
	}
	defer rows.Close()

	archive := Archive{Board: board, Threads: []ArchivedThread{}}
	for rows.Next() {
		var (
			t    ArchivedThread
			body string
		)
		if err := rows.Scan(&t.No, &t.Subject, &t.Replies, &t.CreatedAt, &t.ArchivedAt, &body); err != nil {
			return Archive{}, err
		}
		// One character more than the excerpt keeps is enough for excerpt to
		// see that the body runs on and mark the cut.
		t.Excerpt = excerpt(body, archiveExcerptLen)
		archive.Threads = append(archive.Threads, t)
	}
	return archive, rows.Err()
}

// Thread loads a thread and all of its posts by board slug and thread number.
// Archived threads load like live ones; ArchivedAt tells them apart.
func (s *Store) Thread(ctx context.Context, board Board, no int64) (ThreadDetail, error) {
	detail := ThreadDetail{Board: board, Posts: []Post{}}
	t, err := scanThread(s.pool.QueryRow(ctx,
		`SELECT `+threadColumns+` FROM threads t WHERE t.board_id = $1 AND t.no = $2`, board.ID, no), board)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ThreadDetail{}, errNotFound
		}
		return ThreadDetail{}, err
	}
	detail.Thread = t

	rows, err := s.pool.Query(ctx, `
		SELECT `+postColumns+`, `+attachmentColumns+`
		FROM posts p
		LEFT JOIN attachments a ON a.post_id = p.id
		WHERE p.thread_id = $1
		ORDER BY p.no`, t.ID)
	if err != nil {
		return ThreadDetail{}, err
	}
	defer rows.Close()

	postIDs := []int64{}
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return ThreadDetail{}, err
		}
		p.ThreadNo = t.No
		detail.Posts = append(detail.Posts, p)
		postIDs = append(postIDs, p.ID)
	}
	if err := rows.Err(); err != nil {
		return ThreadDetail{}, err
	}

	byPostID := make(map[int64]*Post, len(detail.Posts))
	for i := range detail.Posts {
		byPostID[detail.Posts[i].ID] = &detail.Posts[i]
	}
	if err := s.attachQuotes(ctx, postIDs, byPostID); err != nil {
		return ThreadDetail{}, err
	}
	return detail, nil
}

// ThreadByID resolves a thread number from its primary key. It backs the
// /api/threads/{id} compatibility route.
func (s *Store) ThreadByID(ctx context.Context, id int64) (Board, int64, error) {
	var (
		slug string
		no   int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT b.slug, t.no FROM threads t JOIN boards b ON b.id = t.board_id WHERE t.id = $1`, id).
		Scan(&slug, &no)
	if errors.Is(err, pgx.ErrNoRows) {
		return Board{}, 0, errNotFound
	}
	if err != nil {
		return Board{}, 0, err
	}
	board, err := s.Board(ctx, slug)
	return board, no, err
}

// NewThread describes a thread to create along with its opening post.
type NewThread struct {
	Board    Board
	Subject  string
	Identity identity
	Body     string
	// PosterID derives the poster ID once the thread ID is known.
	PosterID func(threadID int64) string
	// Badge is the verified institution to display, empty for an unbadged post.
	Badge string
	// Attachment is the image already written to the media store, if any.
	Attachment *newAttachment
}

// CreateThread inserts a thread and its opening post, then prunes the board back
// to its thread limit. It returns the created post.
func (s *Store) CreateThread(ctx context.Context, in NewThread) (Post, error) {
	var created Post
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		no, err := nextPostNo(ctx, tx, in.Board.ID)
		if err != nil {
			return err
		}

		// The opening post is not held to the image limit: the limit caps a
		// conversation, and there is none yet.
		images := 0
		if in.Attachment != nil {
			images = 1
		}
		var threadID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO threads (board_id, no, subject, image_count)
			VALUES ($1, $2, $3, $4)
			RETURNING id`, in.Board.ID, no, in.Subject, images).Scan(&threadID); err != nil {
			return fmt.Errorf("insert thread: %w", err)
		}

		created, err = insertPost(ctx, tx, newPost{
			BoardID:    in.Board.ID,
			ThreadID:   threadID,
			No:         no,
			Identity:   in.Identity,
			PosterID:   in.PosterID(threadID),
			Body:       in.Body,
			OP:         true,
			Badge:      in.Badge,
			Attachment: in.Attachment,
		})
		if err != nil {
			return err
		}
		created.ThreadNo = no
		return pruneThreads(ctx, tx, in.Board)
	})
	return created, err
}

// NewReply describes a reply to an existing thread.
type NewReply struct {
	Board    Board
	ThreadNo int64
	Identity identity
	Body     string
	// PosterID derives the poster ID once the thread ID is known.
	PosterID func(threadID int64) string
	// Badge is the verified institution to display, empty for an unbadged post.
	Badge string
	// Sage suppresses the bump, letting a poster reply without moving the thread
	// back to the top of the board.
	Sage bool
	// Attachment is the image already written to the media store, if any.
	Attachment *newAttachment
}

// CreateReply appends a post to a thread. The thread is bumped unless the reply
// is saged or the thread has reached its board's bump limit. It returns
// errNotFound for a missing thread, errThreadArchived for an archived one,
// errThreadLocked for a locked one, and errImageLimit for a reply bringing an
// image to a thread that already holds its board's image limit.
func (s *Store) CreateReply(ctx context.Context, in NewReply) (Post, error) {
	var created Post
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var (
			threadID   int64
			locked     bool
			archived   bool
			replyCount int
		)
		// Lock the thread row so concurrent replies serialise on the counters,
		// and so the thread cannot be archived between this check and the
		// insert.
		err := tx.QueryRow(ctx, `
			SELECT t.id, t.locked, t.archived_at IS NOT NULL, t.reply_count
			FROM threads t
			WHERE t.board_id = $1 AND t.no = $2
			FOR UPDATE`, in.Board.ID, in.ThreadNo).Scan(&threadID, &locked, &archived, &replyCount)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		// Archived comes first: an archived thread is closed whatever its lock
		// says, and "archived" tells the poster why it will not reopen.
		if archived {
			return errThreadArchived
		}
		if locked {
			return errThreadLocked
		}

		// The cap is checked against the posts themselves rather than the
		// image_count the listings read: this is the check that enforces it,
		// so it answers to the rows. The thread lock taken above keeps a
		// concurrent image reply from landing between the count and the
		// insert, so two replies cannot both take the last slot.
		var imageCount *int
		if in.Attachment != nil {
			var images int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM posts p JOIN attachments a ON a.post_id = p.id
				WHERE p.thread_id = $1`, threadID).Scan(&images); err != nil {
				return fmt.Errorf("count images: %w", err)
			}
			if images >= in.Board.ImageLimit {
				return errImageLimit
			}
			// Set from the count rather than incremented, so a counter that
			// had drifted is right again after the next image.
			images++
			imageCount = &images
		}

		no, err := nextPostNo(ctx, tx, in.Board.ID)
		if err != nil {
			return err
		}
		created, err = insertPost(ctx, tx, newPost{
			BoardID:    in.Board.ID,
			ThreadID:   threadID,
			No:         no,
			Identity:   in.Identity,
			PosterID:   in.PosterID(threadID),
			Body:       in.Body,
			Badge:      in.Badge,
			Attachment: in.Attachment,
		})
		if err != nil {
			return err
		}
		created.ThreadNo = in.ThreadNo

		bump := !in.Sage && replyCount+1 <= in.Board.BumpLimit
		_, err = tx.Exec(ctx, `
			UPDATE threads
			SET reply_count  = reply_count + 1,
			    image_count  = COALESCE($3::integer, image_count),
			    last_post_at = now(),
			    bumped_at    = CASE WHEN $2 THEN now() ELSE bumped_at END
			WHERE id = $1`, threadID, bump, imageCount)
		return err
	})
	return created, err
}

type newPost struct {
	BoardID  int
	ThreadID int64
	No       int64
	Identity identity
	PosterID string
	Body     string
	OP       bool
	// Badge is the institution this post is signed with, empty for the
	// unbadged posts that are the default. It is text copied from a credential,
	// not a reference to one.
	Badge      string
	Attachment *newAttachment
}

func insertPost(ctx context.Context, tx pgx.Tx, in newPost) (Post, error) {
	p := Post{
		No:       in.No,
		Name:     in.Identity.Name,
		Tripcode: in.Identity.Tripcode,
		PosterID: in.PosterID,
		Body:     in.Body,
		OP:       in.OP,
		Badge:    in.Badge,
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO posts (board_id, thread_id, no, name, tripcode, poster_id, body, is_op, badge)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at`,
		in.BoardID, in.ThreadID, in.No, p.Name, p.Tripcode, p.PosterID, p.Body, p.OP, p.Badge,
	).Scan(&p.ID, &p.CreatedAt); err != nil {
		return Post{}, fmt.Errorf("insert post: %w", err)
	}
	attachment, err := insertAttachment(ctx, tx, p.ID, in.Attachment)
	if err != nil {
		return Post{}, err
	}
	p.Attachment = attachment

	quoted := extractQuotes(in.Body)
	if len(quoted) == 0 {
		return p, nil
	}
	// Only quotes that resolve to an existing post on the same board are
	// recorded; dangling references stay as plain text.
	if _, err := tx.Exec(ctx, `
		INSERT INTO post_quotes (from_post_id, to_post_id)
		SELECT $1, target.id
		FROM posts target
		WHERE target.board_id = $2 AND target.no = ANY($3)
		ON CONFLICT DO NOTHING`, p.ID, in.BoardID, quoted); err != nil {
		return Post{}, fmt.Errorf("record quotes: %w", err)
	}
	return p, nil
}

// nextPostNo allocates the next post number for a board. The row update
// serialises allocation, which keeps numbers gapless and monotonic per board.
func nextPostNo(ctx context.Context, tx pgx.Tx, boardID int) (int64, error) {
	var no int64
	if err := tx.QueryRow(ctx, `
		UPDATE boards SET next_post_no = next_post_no + 1
		WHERE id = $1
		RETURNING next_post_no - 1`, boardID).Scan(&no); err != nil {
		return 0, fmt.Errorf("allocate post number: %w", err)
	}
	return no, nil
}

// pruneThreads archives the least recently bumped non-sticky threads once a
// board holds more live threads than its limit, which is how imageboards stay
// bounded without explicit deletion. An archived thread leaves the index and
// the catalog and stops counting against the limit, so it is never archived
// twice; the sweeper deletes it once its board's archive_days have passed.
func pruneThreads(ctx context.Context, tx pgx.Tx, board Board) error {
	if board.ThreadLimit <= 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE threads SET archived_at = now()
		WHERE id IN (
		    SELECT id FROM threads
		    WHERE board_id = $1 AND NOT sticky AND archived_at IS NULL
		    ORDER BY bumped_at DESC
		    OFFSET $2
		)`, board.ID, board.ThreadLimit)
	if err != nil {
		return fmt.Errorf("archive threads: %w", err)
	}
	return nil
}

/* Moderation --------------------------------------------------------------- */

// threadFlag is a moderator-settable boolean on a thread.
type threadFlag string

const (
	flagSticky threadFlag = "sticky"
	flagLocked threadFlag = "locked"
)

// SetThreadFlag pins or locks a thread. It returns errNotFound when the board
// has no thread with that number.
func (s *Store) SetThreadFlag(ctx context.Context, board Board, no int64, flag threadFlag, value bool) error {
	// The column is chosen from literal SQL rather than interpolated, so the
	// flag name never reaches the database as data.
	var query string
	switch flag {
	case flagSticky:
		query = `UPDATE threads SET sticky = $3 WHERE board_id = $1 AND no = $2`
	case flagLocked:
		query = `UPDATE threads SET locked = $3 WHERE board_id = $1 AND no = $2`
	default:
		return fmt.Errorf("unknown thread flag %q", flag)
	}

	tag, err := s.pool.Exec(ctx, query, board.ID, no, value)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// DeletedThread reports what a thread deletion removed. Images are returned so
// the caller can drop the blobs the rows referenced.
type DeletedThread struct {
	Posts  int
	Images []media.StoredImage
}

// DeleteThread removes a thread with all of its posts and attachments. Rows go
// with the thread through ON DELETE CASCADE; the blobs are the caller's to
// remove once the transaction has committed.
func (s *Store) DeleteThread(ctx context.Context, board Board, no int64) (DeletedThread, error) {
	var deleted DeletedThread
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var threadID int64
		err := tx.QueryRow(ctx,
			`SELECT id FROM threads WHERE board_id = $1 AND no = $2 FOR UPDATE`, board.ID, no).Scan(&threadID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}

		deleted.Images, err = attachedImages(ctx, tx,
			`SELECT a.original_path, a.thumb_path
			 FROM attachments a JOIN posts p ON p.id = a.post_id
			 WHERE p.thread_id = $1`, threadID)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM posts WHERE thread_id = $1`, threadID).Scan(&deleted.Posts); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM threads WHERE id = $1`, threadID)
		return err
	})
	if err != nil {
		return DeletedThread{}, err
	}
	return deleted, nil
}

// DeletedPost reports what a post deletion removed.
type DeletedPost struct {
	// ThreadDeleted records that the post was a thread's opening post, so the
	// whole thread went with it.
	ThreadDeleted bool
	Images        []media.StoredImage
}

// DeletePost removes a single post from a thread. Deleting an opening post
// deletes its thread, which is what an imageboard means by removing an OP.
func (s *Store) DeletePost(ctx context.Context, board Board, threadNo, postNo int64) (DeletedPost, error) {
	var deleted DeletedPost
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var (
			threadID int64
			postID   int64
			isOP     bool
		)
		// Lock the thread so a concurrent reply cannot race the reply count.
		err := tx.QueryRow(ctx, `
			SELECT t.id, p.id, p.is_op
			FROM threads t
			JOIN posts p ON p.thread_id = t.id AND p.no = $3
			WHERE t.board_id = $1 AND t.no = $2
			FOR UPDATE OF t, p`, board.ID, threadNo, postNo).Scan(&threadID, &postID, &isOP)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}

		if isOP {
			deleted.ThreadDeleted = true
			deleted.Images, err = attachedImages(ctx, tx,
				`SELECT a.original_path, a.thumb_path
				 FROM attachments a JOIN posts p ON p.id = a.post_id
				 WHERE p.thread_id = $1`, threadID)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `DELETE FROM threads WHERE id = $1`, threadID)
			return err
		}

		deleted.Images, err = attachedImages(ctx, tx,
			`SELECT original_path, thumb_path FROM attachments WHERE post_id = $1`, postID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM posts WHERE id = $1`, postID); err != nil {
			return err
		}
		// The counters are maintained on write, so they have to be corrected
		// here as well: an index that still counts a deleted reply would report
		// the wrong bump-limit state, and a deleted image has to free its slot
		// under the image limit.
		_, err = tx.Exec(ctx, `
			UPDATE threads
			SET reply_count  = GREATEST(reply_count - 1, 0),
			    image_count  = (SELECT count(*) FROM posts p JOIN attachments a ON a.post_id = p.id
			                    WHERE p.thread_id = $1),
			    last_post_at = COALESCE((SELECT max(created_at) FROM posts WHERE thread_id = $1), created_at)
			WHERE id = $1`, threadID)
		return err
	})
	if err != nil {
		return DeletedPost{}, err
	}
	return deleted, nil
}

/* Archive sweep ------------------------------------------------------------ */

// SweptThreads reports what one sweep batch deleted. Images are returned so
// the caller can drop the blobs once the transaction has committed.
type SweptThreads struct {
	Threads int
	Posts   int
	Images  []media.StoredImage
}

// SweepArchived deletes up to limit archived threads whose board's
// archive_days had run out by asOf, oldest first, along with their posts and
// attachments. Rows go through ON DELETE CASCADE, exactly as a moderator's
// deletion does; the blobs are the caller's to remove after the commit.
//
// asOf is fixed by the caller for a whole sweep, so threads archived while it
// runs wait for the next one and a sweep always finishes. SKIP LOCKED leaves a
// thread somebody else holds — a reply being refused, an overlapping sweep —
// for next time rather than queueing behind it.
//
// Legal holds (build plan 2.10) do not exist yet. When they do, a thread under
// one must be skipped here however long ago it was archived.
func (s *Store) SweepArchived(ctx context.Context, asOf time.Time, limit int) (SweptThreads, error) {
	var swept SweptThreads
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT t.id
			FROM threads t
			JOIN boards b ON b.id = t.board_id
			WHERE t.archived_at IS NOT NULL
			  AND t.archived_at <= $1::timestamptz - b.archive_days * interval '1 day'
			ORDER BY t.archived_at
			LIMIT $2
			FOR UPDATE OF t SKIP LOCKED`, asOf, limit)
		if err != nil {
			return fmt.Errorf("find expired threads: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return fmt.Errorf("find expired threads: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}

		swept.Images, err = attachedImages(ctx, tx,
			`SELECT a.original_path, a.thumb_path
			 FROM attachments a JOIN posts p ON p.id = a.post_id
			 WHERE p.thread_id = ANY($1)`, ids)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM posts WHERE thread_id = ANY($1)`, ids).Scan(&swept.Posts); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM threads WHERE id = ANY($1)`, ids)
		if err != nil {
			return fmt.Errorf("delete expired threads: %w", err)
		}
		swept.Threads = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return SweptThreads{}, err
	}
	return swept, nil
}

/* Verification ------------------------------------------------------------- */

// Outcomes of confirming a code, mapped to responses by handleVerifyConfirm.
var (
	errVerifyCode     = errors.New("wrong code")
	errVerifyExpired  = errors.New("code expired")
	errVerifyAttempts = errors.New("too many attempts")
)

// pendingVerification is a code that has been emailed and not yet confirmed.
// Every field is a hash or a label; see 0004_verification.sql for why there is
// no address among them.
type pendingVerification struct {
	EmailHash []byte
	TokenHash []byte
	CodeHash  []byte
	Label     string
	ExpiresAt time.Time
}

// StartVerification records a code awaiting confirmation, replacing whatever
// that address had in flight — a second request means the first code was not
// the one that arrived, or not the one being used.
func (s *Store) StartVerification(ctx context.Context, in pendingVerification) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		// Pending rows are swept here rather than on a timer: they are the one
		// part of this feature that carries a fresh timestamp, so the sooner
		// spent ones go the better.
		if _, err := tx.Exec(ctx, `DELETE FROM verification_pending WHERE expires_at <= now()`); err != nil {
			return fmt.Errorf("prune pending verifications: %w", err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO verification_pending (email_hash, token_hash, code_hash, label, expires_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (email_hash) DO UPDATE SET
			    token_hash = EXCLUDED.token_hash,
			    code_hash  = EXCLUDED.code_hash,
			    label      = EXCLUDED.label,
			    attempts   = 0,
			    expires_at = EXCLUDED.expires_at`,
			in.EmailHash, in.TokenHash, in.CodeHash, in.Label, in.ExpiresAt)
		if err != nil {
			return fmt.Errorf("start verification: %w", err)
		}
		return nil
	})
}

// confirmVerification is one attempt at a pending code, along with the
// credential to issue if it is the right one. The credential's identifier
// arrives already hashed: the value itself goes to the browser and nowhere
// else.
type confirmVerification struct {
	TokenHash        []byte
	CodeHash         []byte
	CredentialIDHash []byte
	ExpiresAt        time.Time
}

// ConfirmVerification checks a code and, when it matches, issues the address a
// credential and revokes the one it held before. It returns the badge label.
//
// A wrong code is a committed outcome rather than an error from the
// transaction, because the attempt it counts has to survive: rolling back would
// hand an attacker unlimited guesses.
func (s *Store) ConfirmVerification(ctx context.Context, in confirmVerification) (string, error) {
	var (
		label   string
		outcome error
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var (
			emailHash []byte
			codeHash  []byte
			attempts  int
			expiresAt time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT email_hash, code_hash, label, attempts, expires_at
			FROM verification_pending
			WHERE token_hash = $1
			FOR UPDATE`, in.TokenHash).Scan(&emailHash, &codeHash, &label, &attempts, &expiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = errNotFound
			return nil
		}
		if err != nil {
			return err
		}

		switch {
		case !expiresAt.After(time.Now()):
			outcome = errVerifyExpired
		case attempts >= maxVerifyAttempts:
			outcome = errVerifyAttempts
		case subtle.ConstantTimeCompare(codeHash, in.CodeHash) != 1:
			_, err := tx.Exec(ctx,
				`UPDATE verification_pending SET attempts = attempts + 1 WHERE token_hash = $1`, in.TokenHash)
			outcome = errVerifyCode
			return err
		}
		if outcome != nil {
			// Spent one way or the other; a fresh code is the only way on.
			_, err := tx.Exec(ctx, `DELETE FROM verification_pending WHERE token_hash = $1`, in.TokenHash)
			return err
		}

		if _, err := tx.Exec(ctx,
			`DELETE FROM verification_pending WHERE token_hash = $1`, in.TokenHash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM verification_credentials WHERE expires_at <= now()`); err != nil {
			return fmt.Errorf("prune credentials: %w", err)
		}
		// One row per address: the update drops the previous credential's
		// identifier, and a credential whose identifier is not here no longer
		// resolves to a badge.
		if _, err := tx.Exec(ctx, `
			INSERT INTO verification_credentials (email_hash, credential_id_hash, label, expires_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (email_hash) DO UPDATE SET
			    credential_id_hash = EXCLUDED.credential_id_hash,
			    label              = EXCLUDED.label,
			    expires_at         = EXCLUDED.expires_at`,
			emailHash, in.CredentialIDHash, label, in.ExpiresAt); err != nil {
			return fmt.Errorf("issue credential: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if outcome != nil {
		return "", outcome
	}
	return label, nil
}

// CredentialLabel resolves an issued credential to the badge it carries,
// returning errNotFound when it has been revoked or has expired.
//
// This is a read and stays a read. Recording that a credential was used would
// put a timestamp next to it for every post made with it, which is the one
// thing the schema is arranged to avoid.
func (s *Store) CredentialLabel(ctx context.Context, credentialIDHash []byte) (string, error) {
	var label string
	err := s.pool.QueryRow(ctx, `
		SELECT label FROM verification_credentials
		WHERE credential_id_hash = $1 AND expires_at > now()`, credentialIDHash).Scan(&label)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotFound
	}
	return label, err
}

// attachedImages collects the blob paths a query selects, as the pairs
// media.Remove deletes.
func attachedImages(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]media.StoredImage, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var images []media.StoredImage
	for rows.Next() {
		var img media.StoredImage
		if err := rows.Scan(&img.Path, &img.ThumbPath); err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, rows.Err()
}

func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const postColumns = `p.id, p.no, p.name, p.tripcode, p.poster_id, p.body, p.is_op, p.created_at, p.badge`

// postDest returns the scan targets for postColumns followed by
// attachmentColumns, which every post query selects together.
func postDest(p *Post, att *attachmentRow) []any {
	dest := []any{&p.ID, &p.No, &p.Name, &p.Tripcode, &p.PosterID, &p.Body, &p.OP, &p.CreatedAt, &p.Badge}
	return append(dest, att.dest()...)
}

func scanPost(row pgx.Row) (Post, error) {
	var att attachmentRow
	p := Post{Quotes: []PostRef{}, Backlinks: []PostRef{}}
	if err := row.Scan(postDest(&p, &att)...); err != nil {
		return Post{}, err
	}
	p.Attachment = att.toAttachment()
	return p, nil
}

func (s *Store) opPosts(ctx context.Context, threadIDs []int64) (map[int64]Post, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.thread_id, t.no, `+postColumns+`, `+attachmentColumns+`
		FROM posts p
		JOIN threads t ON t.id = p.thread_id
		LEFT JOIN attachments a ON a.post_id = p.id
		WHERE p.thread_id = ANY($1) AND p.is_op`, threadIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ops := make(map[int64]Post, len(threadIDs))
	for rows.Next() {
		var (
			threadID int64
			threadNo int64
			att      attachmentRow
			p        = Post{Quotes: []PostRef{}, Backlinks: []PostRef{}}
		)
		if err := rows.Scan(append([]any{&threadID, &threadNo}, postDest(&p, &att)...)...); err != nil {
			return nil, err
		}
		p.ThreadNo = threadNo
		p.Attachment = att.toAttachment()
		ops[threadID] = p
	}
	return ops, rows.Err()
}

func (s *Store) lastReplies(ctx context.Context, threadIDs []int64, perThread int) (map[int64][]Post, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.thread_id, p.thread_no, p.id, p.no, p.name, p.tripcode, p.poster_id,
		       p.body, p.is_op, p.created_at, p.badge, `+attachmentColumns+`
		FROM (
		    SELECT p.thread_id, t.no AS thread_no, p.id, p.no, p.name, p.tripcode, p.poster_id,
		           p.body, p.is_op, p.created_at, p.badge,
		           row_number() OVER (PARTITION BY p.thread_id ORDER BY p.no DESC) AS rank
		    FROM posts p
		    JOIN threads t ON t.id = p.thread_id
		    WHERE p.thread_id = ANY($1) AND NOT p.is_op
		) p
		LEFT JOIN attachments a ON a.post_id = p.id
		WHERE p.rank <= $2
		ORDER BY p.thread_id, p.no`, threadIDs, perThread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	replies := make(map[int64][]Post, len(threadIDs))
	for rows.Next() {
		var (
			threadID int64
			att      attachmentRow
			p        = Post{Quotes: []PostRef{}, Backlinks: []PostRef{}}
		)
		if err := rows.Scan(append([]any{&threadID, &p.ThreadNo}, postDest(&p, &att)...)...); err != nil {
			return nil, err
		}
		p.Attachment = att.toAttachment()
		replies[threadID] = append(replies[threadID], p)
	}
	return replies, rows.Err()
}

// attachQuotes fills in the quote and backlink references for the given posts.
func (s *Store) attachQuotes(ctx context.Context, ids []int64, byID map[int64]*Post) error {
	if len(ids) == 0 {
		return nil
	}

	outgoing, err := s.pool.Query(ctx, `
		SELECT q.from_post_id, target.no, thread.no
		FROM post_quotes q
		JOIN posts target ON target.id = q.to_post_id
		JOIN threads thread ON thread.id = target.thread_id
		WHERE q.from_post_id = ANY($1)
		ORDER BY target.no`, ids)
	if err != nil {
		return err
	}
	if err := collectRefs(outgoing, byID, func(p *Post, ref PostRef) {
		p.Quotes = append(p.Quotes, ref)
	}); err != nil {
		return err
	}

	incoming, err := s.pool.Query(ctx, `
		SELECT q.to_post_id, source.no, thread.no
		FROM post_quotes q
		JOIN posts source ON source.id = q.from_post_id
		JOIN threads thread ON thread.id = source.thread_id
		WHERE q.to_post_id = ANY($1)
		ORDER BY source.no`, ids)
	if err != nil {
		return err
	}
	return collectRefs(incoming, byID, func(p *Post, ref PostRef) {
		p.Backlinks = append(p.Backlinks, ref)
	})
}

func collectRefs(rows pgx.Rows, byID map[int64]*Post, add func(*Post, PostRef)) error {
	defer rows.Close()
	for rows.Next() {
		var (
			postID int64
			ref    PostRef
		)
		if err := rows.Scan(&postID, &ref.No, &ref.ThreadNo); err != nil {
			return err
		}
		if p, ok := byID[postID]; ok {
			add(p, ref)
		}
	}
	return rows.Err()
}

const threadColumns = `t.id, t.no, t.subject, t.sticky, t.locked, t.reply_count, t.created_at, t.bumped_at,
	t.last_post_at, t.archived_at, t.image_count`

func threadDest(t *Thread, extra ...any) []any {
	dest := []any{&t.ID, &t.No, &t.Subject, &t.Sticky, &t.Locked, &t.Replies, &t.CreatedAt, &t.BumpedAt,
		&t.LastPostAt, &t.ArchivedAt, &t.Images}
	return append(dest, extra...)
}

func scanThread(row pgx.Row, board Board) (Thread, error) {
	var t Thread
	if err := row.Scan(threadDest(&t)...); err != nil {
		return Thread{}, err
	}
	t.applyLimits(board)
	return t, nil
}

// applyLimits derives the flags that compare a thread's counters with its
// board's limits. They are computed on read, so changing a limit takes effect
// on every thread at once.
func (t *Thread) applyLimits(board Board) {
	t.AtBumpLimit = t.Replies >= board.BumpLimit
	t.AtImageLimit = t.Images >= board.ImageLimit
}

// attachmentColumns is the projection shared by every query that joins
// attachments; the order matches attachmentRow.dest.
const attachmentColumns = `a.original_name, a.original_path, a.thumb_path, a.content_type,
	       a.byte_size, a.width, a.height, a.thumb_width, a.thumb_height, a.spoiler`

// attachmentRow scans the nullable columns produced by a LEFT JOIN on
// attachments.
type attachmentRow struct {
	name        *string
	path        *string
	thumbPath   *string
	contentType *string
	size        *int64
	width       *int
	height      *int
	thumbWidth  *int
	thumbHeight *int
	spoiler     *bool
}

func (r *attachmentRow) dest() []any {
	return []any{&r.name, &r.path, &r.thumbPath, &r.contentType,
		&r.size, &r.width, &r.height, &r.thumbWidth, &r.thumbHeight, &r.spoiler}
}

func (r *attachmentRow) toAttachment() *Attachment {
	if r.path == nil || r.thumbPath == nil {
		return nil
	}
	att := &Attachment{
		URL:      originalURL(*r.path),
		ThumbURL: thumbURL(*r.thumbPath),
	}
	deref(&att.Name, r.name)
	deref(&att.ContentType, r.contentType)
	deref(&att.Size, r.size)
	deref(&att.Width, r.width)
	deref(&att.Height, r.height)
	deref(&att.ThumbWidth, r.thumbWidth)
	deref(&att.ThumbHeight, r.thumbHeight)
	deref(&att.Spoiler, r.spoiler)
	return att
}

func deref[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

func insertAttachment(ctx context.Context, tx pgx.Tx, postID int64, in *newAttachment) (*Attachment, error) {
	if in == nil {
		return nil, nil
	}
	img := in.Image
	if _, err := tx.Exec(ctx, `
		INSERT INTO attachments (
		    post_id, original_name, original_path, thumb_path, content_type, thumb_content_type,
		    byte_size, width, height, thumb_width, thumb_height, checksum, spoiler
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		postID, in.OriginalName, img.Path, img.ThumbPath, img.ContentType, img.ThumbContentType,
		img.Size, img.Width, img.Height, img.ThumbWidth, img.ThumbHeight, img.Checksum, in.Spoiler); err != nil {
		return nil, fmt.Errorf("insert attachment: %w", err)
	}
	return &Attachment{
		Name:        in.OriginalName,
		URL:         originalURL(img.Path),
		ThumbURL:    thumbURL(img.ThumbPath),
		ContentType: img.ContentType,
		Size:        img.Size,
		Width:       img.Width,
		Height:      img.Height,
		ThumbWidth:  img.ThumbWidth,
		ThumbHeight: img.ThumbHeight,
		Spoiler:     in.Spoiler,
	}, nil
}
