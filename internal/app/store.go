package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
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
	// archiveBodyWindow is how much of each opening post the archive reads.
	// It leaves room past the excerpt for the tags in it to close: a tag
	// whose close lies further on is cut away, spoilers included, by
	// excerptOfStart.
	archiveBodyWindow = 500
)

// Store is the PostgreSQL-backed persistence layer.
type Store struct {
	pool *pgxpool.Pool
	// What RewriteCredentials runs on the probe and verification tables, and
	// on pg_statistic, and how long it keeps trying: vacuumCommand (both of
	// the first two) and vacuumPatience, which tests change to see a vacuum
	// that skips a table, or a sweep that gives up sooner.
	vacuumCommand           string
	statisticsVacuumCommand string
	vacuumPatience          time.Duration
	// How long RewriteCredentials's connection waits on a cancel request
	// (cancelGrace), which a test shortens to see PostgreSQL not answer one.
	cancelGrace time.Duration
	// How long after the rewrite RewriteCredentials reads the verification
	// tables a last time (statsHandOver). The app package's test harness sets
	// it to zero, as only TestSweepLeavesNoScanTimeFromBeforeIt there is about
	// it, and every other sweep would wait it out for nothing; cmd/server's
	// tests sweep from an App built by New, as the Job does, and wait it out.
	statsHandOver time.Duration
	// vacuumed, which tests set, is called with the table of each VACUUM FULL
	// after which vacuumFull finds the table in a new file. Every schema in
	// the database shares pg_statistic, so another process's sweep — the
	// cmd/server tests', run alongside the app package's — moves its file
	// too, and only the sweep can say which runs on it were its own.
	vacuumed func(table string)
}

// NewStore wraps an existing pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, vacuumCommand: vacuumCommand, statisticsVacuumCommand: vacuumCommand,
		vacuumPatience: vacuumPatience, cancelGrace: cancelGrace, statsHandOver: statsHandOver}
}

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

// Boards lists every board in display order: the seeded set in the order
// 0006_board_set.sql gives it, then boards opened over the API, oldest first.
func (s *Store) Boards(ctx context.Context) ([]Board, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+boardColumns+` FROM boards b ORDER BY b.sort_order NULLS LAST, b.id`)
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
		SELECT t.no, t.subject, t.reply_count, t.created_at, t.archived_at,
		       left(p.body, $3), length(p.body) > $3
		FROM threads t
		JOIN posts p ON p.thread_id = t.id AND p.is_op
		WHERE t.board_id = $1 AND t.archived_at IS NOT NULL
		ORDER BY t.archived_at DESC, t.no DESC
		LIMIT $2`, board.ID, archiveLimit, archiveBodyWindow)
	if err != nil {
		return Archive{}, err
	}
	defer rows.Close()

	archive := Archive{Board: board, Threads: []ArchivedThread{}}
	for rows.Next() {
		var (
			t     ArchivedThread
			start string
			cut   bool
		)
		if err := rows.Scan(&t.No, &t.Subject, &t.Replies, &t.CreatedAt, &t.ArchivedAt, &start, &cut); err != nil {
			return Archive{}, err
		}
		t.Excerpt = excerptOfStart(start, cut, archiveExcerptLen)
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

// PostThreadNo resolves a post number on a board to the number of the thread
// holding it, live or archived. It returns errNotFound when the board has no
// such post, which includes one a moderator has deleted.
func (s *Store) PostThreadNo(ctx context.Context, board Board, no int64) (int64, error) {
	var threadNo int64
	err := s.pool.QueryRow(ctx, `
		SELECT t.no FROM posts p JOIN threads t ON t.id = p.thread_id
		WHERE p.board_id = $1 AND p.no = $2`, board.ID, no).Scan(&threadNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errNotFound
	}
	return threadNo, err
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

// ReplyRefusal reports what CreateReply would refuse a reply for, as far as the
// thread can say right now: errNotFound, errThreadArchived, errThreadLocked,
// and, for a reply bringing an image, errImageLimit, in CreateReply's order.
// It takes no lock and decides nothing. It lets the handler turn away a reply
// that cannot land before flood control charges for it and before its upload
// is stored, which is what a poster told to send the text on its own needs;
// CreateReply makes the same checks under the board and thread locks, and
// that is what enforces them when replies race. Like that check, the image
// count is taken from the posts, not from threads.image_count.
func (s *Store) ReplyRefusal(ctx context.Context, board Board, threadNo int64, withImage bool) error {
	var (
		archived, locked bool
		images           int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT t.archived_at IS NOT NULL, t.locked,
		       CASE WHEN $3 THEN (
		           SELECT count(*) FROM posts p JOIN attachments a ON a.post_id = p.id
		           WHERE p.thread_id = t.id)
		       ELSE 0 END
		FROM threads t
		WHERE t.board_id = $1 AND t.no = $2`, board.ID, threadNo, withImage).Scan(&archived, &locked, &images)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return errNotFound
	case err != nil:
		return err
	case archived:
		return errThreadArchived
	case locked:
		return errThreadLocked
	case withImage && images >= board.ImageLimit:
		return errImageLimit
	}
	return nil
}

// CreateReply appends a post to a thread. The thread is bumped unless the reply
// is saged or the thread has reached its board's bump limit. It returns
// errNotFound for a missing thread, errThreadArchived for an archived one,
// errThreadLocked for a locked one, and errImageLimit for a reply bringing an
// image to a thread that already holds its board's image limit.
func (s *Store) CreateReply(ctx context.Context, in NewReply) (Post, error) {
	var created Post
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// The number comes first because it takes the board's row, and a new
		// thread takes that row before it archives the threads it pushes off.
		// Taking the thread first, as this did, deadlocks with a new thread
		// archiving it: each would hold what the other waits for. A refused
		// reply rolls back, so it costs no number.
		no, err := nextPostNo(ctx, tx, in.Board.ID)
		if err != nil {
			return err
		}

		var (
			threadID   int64
			locked     bool
			archived   bool
			replyCount int
		)
		// Replies to one board already wait for each other on the board row
		// nextPostNo took, and so do the new threads that archive this one;
		// that is what keeps two replies from both taking the last image slot.
		// Locking the thread row as well keeps out what does not go through
		// the board row — a moderator locking, archiving-by-sweep or deleting
		// the thread, or a deletion recounting its counters — between these
		// checks and the insert.
		err = tx.QueryRow(ctx, `
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
	//
	// FOR KEY SHARE is what keeps a quote of a post that is being deleted —
	// by the sweeper, or a moderator — from failing the reply. Without it the
	// SELECT still sees a target whose deletion has not committed, and the
	// foreign-key check then waits on that row and fails once the delete
	// commits. With it, the wait happens here, and a target that turns out to
	// be gone is skipped like any other dangling reference. Taking the locks
	// in id order keeps two replies, or a reply and a sweep batch, from
	// deadlocking over several targets.
	if _, err := tx.Exec(ctx, `
		INSERT INTO post_quotes (from_post_id, to_post_id)
		SELECT $1, target.id
		FROM posts target
		WHERE target.board_id = $2 AND target.no = ANY($3)
		ORDER BY target.id
		FOR KEY SHARE
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

// SetThreadFlag pins or locks a live thread. It returns errNotFound when the
// board has no thread with that number, and errThreadArchived when the thread
// is in the archive: nothing brings a thread back from there, so a sticky
// would not keep it on the board or out of the sweep, and a lock would change
// nothing a reply could notice. Refusing says so, where a quiet success would
// let a moderator believe an archived thread had been saved.
func (s *Store) SetThreadFlag(ctx context.Context, board Board, no int64, flag threadFlag, value bool) error {
	// The column is chosen from literal SQL rather than interpolated, so the
	// flag name never reaches the database as data.
	var query string
	switch flag {
	case flagSticky:
		query = `UPDATE threads SET sticky = $3 WHERE board_id = $1 AND no = $2 AND archived_at IS NULL`
	case flagLocked:
		query = `UPDATE threads SET locked = $3 WHERE board_id = $1 AND no = $2 AND archived_at IS NULL`
	default:
		return fmt.Errorf("unknown thread flag %q", flag)
	}

	tag, err := s.pool.Exec(ctx, query, board.ID, no, value)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var archived bool
	err = s.pool.QueryRow(ctx,
		`SELECT archived_at IS NOT NULL FROM threads WHERE board_id = $1 AND no = $2`,
		board.ID, no).Scan(&archived)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return errNotFound
	case err != nil:
		return err
	case archived:
		return errThreadArchived
	}
	// A live thread here would have matched the update, and nothing takes a
	// thread back out of the archive, so this is not reached in practice.
	return errNotFound
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

// SweptThreads reports what one sweep batch deleted. The batch's files are not
// in it: their keys went into media_deletions with the rows, for the caller to
// remove through NextMediaDeletion once the transaction has committed.
type SweptThreads struct {
	Threads     int
	Posts       int
	Attachments int
}

// SweepArchived deletes up to limit archived threads whose board's
// archive_days had run out by asOf, oldest first, along with their posts and
// attachments. Rows go through ON DELETE CASCADE, exactly as a moderator's
// deletion does, and in the same transaction the storage keys of their files
// are queued in media_deletions: whatever happens to the sweep after the
// commit, nothing loses track of a file it was meant to remove.
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

		images, err := attachedImages(ctx, tx,
			`SELECT a.original_path, a.thumb_path
			 FROM attachments a JOIN posts p ON p.id = a.post_id
			 WHERE p.thread_id = ANY($1)`, ids)
		if err != nil {
			return err
		}
		swept.Attachments = len(images)
		keys := make([]string, 0, 2*len(images))
		for _, img := range images {
			keys = append(keys, media.OriginalKey(img.Path), media.ThumbKey(img.ThumbPath))
		}
		if len(keys) > 0 {
			if _, err := tx.Exec(ctx,
				`INSERT INTO media_deletions (key) SELECT unnest($1::text[])`, keys); err != nil {
				return fmt.Errorf("queue media deletions: %w", err)
			}
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

// NextMediaDeletion takes the first key queued in media_deletions after
// afterID that no other sweep is working on, hands it to remove, and forgets
// it when remove reports the file gone. It returns the key's id, so the caller
// can carry on past a key it could not remove, and found false once nothing is
// left after afterID.
//
// One key per transaction keeps the gap between a file going and its key
// going as short as it can be: a sweep killed in between leaves a key whose
// file is already gone, which the next sweep reports as missing.
func (s *Store) NextMediaDeletion(ctx context.Context, afterID int64, remove func(key string) bool) (id int64, found bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var key string
		err := tx.QueryRow(ctx, `
			SELECT id, key FROM media_deletions
			WHERE id > $1
			ORDER BY id
			LIMIT 1
			FOR UPDATE SKIP LOCKED`, afterID).Scan(&id, &key)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("next media deletion: %w", err)
		}
		found = true
		if !remove(key) {
			return nil
		}
		if _, err := tx.Exec(ctx, `DELETE FROM media_deletions WHERE id = $1`, id); err != nil {
			return fmt.Errorf("forget media deletion: %w", err)
		}
		return nil
	})
	return id, found, err
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
		// Pending rows are swept here, and by the nightly sweep
		// (RewriteCredentials), rather than on a timer: they are the one part
		// of this feature that carries a fresh timestamp, so the sooner spent
		// ones go the better.
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
			if attempts+1 >= maxVerifyAttempts {
				// The wrong code that spends the last attempt ends the
				// verification there and then, and says so. Answered as just
				// a wrong code, it would leave the page offering a box that not
				// even the right code could get through.
				outcome = errVerifyAttempts
				break
			}
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

// verificationLockTimeout bounds every wait RewriteCredentials makes for a
// lock outside the vacuum, which has a bound of its own (vacuumLockTimeout).
// The one it can wait on in practice is the rewrite's lock on the
// credentials table, which lets plain reads through and waits for everything
// else on the table: a transaction that has written to it or locked rows in
// it, a VACUUM, ANALYZE or index build on it, a change to its schema, or
// another sweep's rewrite or vacuum. While the rewrite waits, confirmations
// queue behind it, each holding one of its instance's few database
// connections, and once they hold them all every request on that instance
// waits too; so a transaction left open on the table — a psql session that
// wrote to it, say — fails the sweep within seconds rather than stalling the
// site until it ends.
//
// It bounds only the wait. Once the lock is granted, confirmations queue for
// as long as the rewrite's statement runs as well, and that grows with the
// table: about three seconds for 220,000 credentials, as measured on a
// development machine. So a confirmation that arrives as the rewrite starts
// can wait five seconds plus the rewrite. Reads do not queue behind the
// rewrite.
//
// The vacuum after it does not wait for a table another session holds, since
// it passes over one, and waits at most vacuumLockTimeout for any other lock;
// but VACUUM FULL holds a table it has taken against every read and write,
// CredentialLabel's included, until it has written the table out, or given
// it up. Every badged post and confirmation that reaches the table meanwhile
// waits, each holding a pooled connection, and three of them hold all an
// instance has by default (EDUCHAN_DB_MAX_CONNS), so for the length of the run
// every request on that instance can wait. That too grows with the table:
// between about half a second and two and a half seconds for 220,000
// credentials, as measured on development machines. So vacuumVerificationTables
// runs on a table only once a probe says the run can do its job and nothing it
// would wait for once it had the table is held (vacuumLockTimeout), rather
// than again and again while an older transaction keeps it from doing its job
// or another session makes it give the table up: once a night, unless a
// session begins to hold such a thing during a run.
const verificationLockTimeout = "5s"

// vacuumLockTimeout bounds every wait for a lock in vacuumVerificationTables.
// SKIP_LOCKED makes VACUUM FULL pass over a table another session holds
// rather than wait for it, but only the table it is given. Once it has that
// table it needs more, and waits for each while it holds the table against
// every read. Before it copies the table it locks the table's TOAST table,
// which a VACUUM of the TOAST table by name holds, with no lock on the table.
// Autovacuum holds pg_statistic's the same way, since 0015 left autovacuum on
// for the catalog. It leaves the verification tables' TOAST tables alone: a
// TOAST table with no setting of its own takes its table's, and 0015 turned
// autovacuum off on both, so it comes to them only to keep their transaction
// ids from wrapping around, which the nightly VACUUM FULL keeps from coming
// due. After it has copied the whole table it updates the table's row in
// pg_class, and so waits for any transaction that has updated the row and not
// ended — a GRANT or REVOKE on the table, say, which takes no lock on the
// table itself — and then it rebuilds each index, waiting for any lock on it:
// a COMMENT ON INDEX left open, say. Meanwhile CredentialLabel waits on the
// credentials table, and every query whose plan needs statistics its
// connection has not cached waits on pg_statistic, on any table.
//
// So before each run vacuumFull looks for any of those held by a session that
// does not hold the table itself, which SKIP_LOCKED would pass over anyway
// (vacuumWaitsQuery), and while it finds one it leaves the table alone
// (vacuumWouldWait) and the sweep tries again later. A run that waited for one
// after the copy would have held every request on the table up for the whole
// copy and then given the table up, and tried again it would do the same, try
// after try. What vacuumFull cannot see coming — a GRANT that begins during the
// run — VACUUM FULL waits for this long at most, and then gives the table up
// (vacuumBlocked). A GRANT that commits sooner ends the wait sooner, and the run
// with it, the table still in its old file: VACUUM FULL cannot update a row
// the GRANT has changed under it (concurrentlyUpdated), and that run has given
// the table up too. Either way the sweep tries again a moment later, and by
// then the look before the run sees the GRANT, or that it has ended
// (TestVacuumLetsGoOfATableWhileAnotherLockItNeedsIsHeld,
// TestVacuumTriesAgainWhenTheGrantItWaitsForCommits). An autocommit GRANT,
// the usual kind, commits within milliseconds, and so ends a run the second
// way. An uncontended lock is granted at once, so the bound
// need be no longer than a request should wait; and it is below PostgreSQL's
// deadlock_timeout, so the sweep gives way to an autovacuum rather than having
// it cancelled. The bound covers the whole statement, so it would also end a
// wait that came after the run had committed and let go of the table, with the
// table in its new file: the sweep's runs leave out the one step that waits
// there (vacuumCommand), and a run that ends so all the same counts as the run
// it was (vacuumFull).
const vacuumLockTimeout = "100ms"

// vacuumWaitsQuery says whether VACUUM FULL, once it had the table $1, would
// wait for another session (vacuumLockTimeout says for what): whether a
// session that holds no lock on the table holds one on its TOAST table or one
// of its indexes, or has an open transaction that has updated the table's row
// in pg_class or one of theirs. A row's xmax is the id of the transaction that
// updated it, and a transaction holds a lock on its own id until it ends, so
// the lock says the update has not. A session that holds the table is left to
// SKIP_LOCKED, which passes over the table with the warning vacuumFull reads.
const vacuumWaitsQuery = `
	WITH target AS (
	    SELECT oid, reltoastrelid FROM pg_class WHERE oid = $1::text::regclass
	), taken AS (
	    SELECT reltoastrelid AS oid FROM target WHERE reltoastrelid <> 0
	    UNION ALL
	    SELECT indexrelid FROM pg_index WHERE indrelid = (SELECT oid FROM target)
	), here AS (
	    SELECT oid FROM pg_database WHERE datname = current_database()
	), holders AS (
	    SELECT l.virtualtransaction, l.pid FROM pg_locks l
	    WHERE l.locktype = 'relation' AND l.granted
	      AND l.database = (SELECT oid FROM here) AND l.relation IN (SELECT oid FROM taken)
	    UNION ALL
	    SELECT l.virtualtransaction, l.pid FROM pg_class c
	    JOIN pg_locks l ON l.locktype = 'transactionid' AND l.granted AND l.transactionid = c.xmax
	    WHERE c.oid IN (SELECT oid FROM target UNION ALL SELECT oid FROM taken)
	)
	SELECT EXISTS (
	    SELECT FROM holders h
	    WHERE h.pid IS DISTINCT FROM pg_backend_pid()
	      AND NOT EXISTS (
	          SELECT FROM pg_locks t
	          WHERE t.locktype = 'relation' AND t.granted AND t.database = (SELECT oid FROM here)
	            AND t.relation = (SELECT oid FROM target) AND t.virtualtransaction = h.virtualtransaction))`

// How long RewriteCredentials keeps trying VACUUM FULL on the verification
// tables — while a transaction older than the rewrite is open, while another
// session holds a table or something VACUUM FULL would wait for once it had
// the table (vacuumLockTimeout), or while a table's new file still holds rows
// deleted before the rewrite — before the sweep fails; and how long it waits
// between tries: vacuumFirstWait at first, doubling each time up to
// vacuumMaxWait.
const (
	vacuumPatience  = time.Minute
	vacuumFirstWait = 100 * time.Millisecond
	vacuumMaxWait   = 5 * time.Second
)

// vacuumCommand is what RewriteCredentials runs on the probe table, each
// verification table, and pg_statistic.
//
// SKIP_DATABASE_STATS leaves out the step a VACUUM otherwise ends with, after
// its run on the table has committed: moving the database's oldest
// transaction id up in its row in pg_database (datfrozenxid). That step holds
// no table, but it waits for any other session taking the same step, and,
// since PostgreSQL 16.5, for any transaction that has updated the row and not
// ended — an ALTER DATABASE, or a GRANT on the database. With the connection's
// lock_timeout at vacuumLockTimeout, such a wait would fail the statement with
// the table already in its new file, and the sweep would run VACUUM FULL on
// the table again, holding up every request on it a second time, or fail
// although its runs had done their job (TestVacuumDoesNotWaitForTheDatabasesRow).
// Autovacuum takes the step whenever it works on the database, so leaving it
// out costs nothing but the time until then.
const vacuumCommand = "VACUUM (FULL, SKIP_LOCKED, SKIP_DATABASE_STATS)"

// sqlstateLockNotAvailable is the code of the warning VACUUM gives when
// SKIP_LOCKED makes it pass over a table another session holds, and of the
// error it fails with when lock_timeout ends a wait for any other lock.
const sqlstateLockNotAvailable = "55P03"

// sqlstateInternalError and concurrentlyUpdated are the code and message of
// the error VACUUM FULL fails with when it has waited to update its table's
// row in pg_class, or an index's, for a transaction that had updated the row
// — a GRANT on the table, say — and that transaction commits within
// vacuumLockTimeout: it cannot update a row another transaction has changed
// since its snapshot. PostgreSQL raises it with elog, so the message is never
// translated; the code is the one every internal error has, so it takes both.
const (
	sqlstateInternalError = "XX000"
	concurrentlyUpdated   = "tuple concurrently updated"
)

// verificationTables are the tables RewriteCredentials rewrites and vacuums.
var verificationTables = [...]string{"verification_credentials", "verification_pending"}

// statisticsCatalog is the catalog ANALYZE copied samples of the verification
// tables' columns into before 0015, which RewriteCredentials vacuums with
// them, after them in each pass (vacuumVerificationTables says why).
const statisticsCatalog = "pg_catalog.pg_statistic"

// vacuumProbeTable is the empty table (0016) on which RewriteCredentials runs
// VACUUM FULL to learn whether a run on a verification table would drop what
// the rewrite deleted, before it takes one (vacuumVerificationTables).
const vacuumProbeTable = "vacuum_probe"

// RewriteCredentials takes from the verification tables what PostgreSQL
// records about every row whatever the schema leaves out, and returns how many
// credential rows it rewrote. The archive sweeper calls it once a night.
//
// Every row carries the id of the transaction that wrote it (xmin). Posts are
// written by transactions too, and carry created_at, so a credential's xmin
// falls between two posts' and dates its issuance to the minutes between them;
// with its label, it points at the first badged posts after it. That is what
// leaving out created_at was for, and freezing does not undo it: a frozen row
// keeps its xmin. A row's place in the table (ctid) follows the order rows
// were written in, which is the order of issuance again.
//
// So the credentials are deleted and inserted back from the copy the delete
// returns, in random order, in one statement: afterwards every row carries
// this transaction's id, and where a row sits says nothing about when it was
// issued. The id says of a row only that it was issued before the sweep:
// nothing its expiry does not for one issued on an earlier day, and for one of
// the day the sweep runs in, which ends after the scheduled sweep
// (credentialDayEnd), that it came in the 22 hours of that day before the
// sweep. The lock is the one that holds back writers and lets readers
// through. The statement after it takes its snapshot once the lock is
// granted, so a confirmation that committed before then is in the copy; one
// that arrives after waits for the commit and then writes over the rewritten
// rows, whether it issues a new credential or replaces one. CredentialLabel
// reads throughout: the old rows until the commit, the new ones after, never
// neither.
//
// The old versions are still in the table's file with their xmin, as are the
// pending rows confirmations deleted, each with an email hash and an expiry to
// the microsecond, and vacuumVerificationTables writes both tables to new
// files without them; then pg_statistic, whose files can still hold the
// samples ANALYZE took of the pending table before 0015, which are much the
// same. The times PostgreSQL's statistics keep of the last scan of each table
// and index would date the last confirmation before the sweep, so restampScans
// makes them the sweep's: once after the rewrite, and once more after the
// vacuum, when every connection has handed the statistics what it read before
// the rewrite (statsHandOver).
//
// A code that expired unconfirmed leaves its pending row live until the next
// request prunes it (StartVerification), whenever that comes. Its expiry, less
// the code's lifetime, is the time it was asked for, to the microsecond, and
// its email hash is the address's: an address that already holds a credential
// — a second browser asking for a code it never confirms, say — joins it to
// that credential, which it then says was issued before that time. So before
// the rewrite those rows are deleted. The delete must come no later than the
// rewrite: what the vacuum is checked against is the rewrite's transaction
// id, which says nothing of rows a later transaction deleted (no test holds
// this order). And it runs in a transaction of its own rather than in the
// rewrite's, because a confirmation locks its code's pending row before it
// writes to the credentials table: in the rewrite's transaction, which holds
// that table, a delete waiting for such a row would wait for a confirmation
// waiting for the rewrite, until PostgreSQL failed one of them
// (TestSweepDeletesExpiredCodesWithoutHoldingTheCredentialsTable). A code
// still in flight stays, and still confirms; the README says what its row
// keeps, and until when.
//
// None of this reaches the posts. Requesting a code and confirming one are
// writes, and each takes a transaction id from the counter posts take theirs
// from, so the ids they took are missing from the posts' xmins, between the
// two posts written around each, for as long as those posts exist; the
// README's "Transaction ids, and the nightly rewrite" says what that still
// dates, and TestIssuanceLeavesGapsAmongThePostsTransactionIDs holds it.
//
// It works on a connection of its own rather than one from the pool, for what
// the pool's connections should not carry: the lock timeout, a handler for
// the warnings with which VACUUM reports a table it skipped, and a
// cancellation that stops the statement on the server before it returns
// (sweepConnection). The connection asks for those warnings itself
// (client_min_messages), because DATABASE_URL or a role's or the database's
// default can turn them off, and without the warning a table another session
// held looks like one the role may not vacuum (vacuumFull).
func (s *Store) RewriteCredentials(ctx context.Context) (rewritten int, err error) {
	// A statement the cancellation stopped fails with PostgreSQL's
	// "canceling statement due to user request"; the error says why as well.
	defer func() {
		if err != nil && ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
			err = fmt.Errorf("%w: %w", ctx.Err(), err)
		}
	}()
	notices := &noticeLog{}
	conn, err := s.sweepConnection(ctx, notices)
	if err != nil {
		return 0, fmt.Errorf("connect: %w", err)
	}
	defer closeSweepConnection(ctx, conn, s.cancelGrace)

	if _, err := conn.Exec(ctx, `DELETE FROM verification_pending WHERE expires_at <= now()`); err != nil {
		return 0, fmt.Errorf("delete expired codes: %w", err)
	}

	var rewrite uint32 // the rewriting transaction's id
	err = pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE verification_credentials IN EXCLUSIVE MODE`); err != nil {
			return fmt.Errorf("lock credentials: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			WITH copy AS (
			    DELETE FROM verification_credentials
			    RETURNING email_hash, credential_id_hash, label, expires_at
			)
			INSERT INTO verification_credentials (email_hash, credential_id_hash, label, expires_at)
			SELECT email_hash, credential_id_hash, label, expires_at FROM copy
			ORDER BY random()`)
		if err != nil {
			return fmt.Errorf("rewrite credentials: %w", err)
		}
		rewritten = int(tag.RowsAffected())
		// The old versions carry this id as the transaction that deleted
		// them, and the vacuum is measured against it. Asking for it gives
		// the transaction one even when there was nothing to delete.
		if err := tx.QueryRow(ctx, `SELECT pg_current_xact_id()::xid`).Scan(&rewrite); err != nil {
			return fmt.Errorf("read the rewrite's transaction id: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	committed := time.Now()
	if err := restampScans(ctx, conn); err != nil {
		return rewritten, err
	}
	if err := s.vacuumVerificationTables(ctx, conn, notices, rewrite); err != nil {
		return rewritten, err
	}
	timer := time.NewTimer(time.Until(committed.Add(s.statsHandOver)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return rewritten, fmt.Errorf("wait for the statistics: %w", ctx.Err())
	case <-timer.C:
	}
	return rewritten, restampScans(ctx, conn)
}

// statsHandOver is how long after the rewrite has committed RewriteCredentials
// reads the verification tables a second time (restampScans): long enough for
// every other connection to have handed the statistics the counts of the
// scans it made before the commit.
//
// A connection keeps its counts and hands them over later. When one of its
// transactions ends it hands over everything it has kept, but no more than
// once a second, and if it then sits idle it hands the rest over within ten
// seconds. If it finds the statistics' entry for a table locked, it keeps
// that table's counts rather than wait, and waits for the lock only once it
// has sat idle for ten seconds, or when a transaction of its ends a minute or
// more after it first kept counts back (PGSTAT_MIN_INTERVAL,
// PGSTAT_IDLE_INTERVAL and PGSTAT_MAX_INTERVAL in PostgreSQL's pgstat.c). The
// time it hands over as a scan's is the end of its latest transaction by
// then, or, when that transaction left none, the moment it hands the counts
// over. So the counts of a confirmation that commits on a server's connection
// less than a second after that connection last handed its counts over reach
// the statistics up to ten seconds after the commit, or up to a minute after
// under contention, with the time of the next request that connection served,
// or of the hand-over: after the sweep's first read, and the statistics keep
// the latest time they are given. Those are the indexes a confirmation reads,
// so that time would date the last issuance before the sweep to within ten
// seconds. The second read, a minute and five seconds after the commit, puts
// the sweep's time back over it; the five seconds are for the transaction
// that ends the minute. Scans made after the commit need no such cover: a
// confirmation then writes a row with a transaction id of its own, which
// dates it until the next sweep anyway, and a badged post's read of its
// credential says when the post was written, as the post itself does
// (TestSweepLeavesNoScanTimeFromBeforeIt).
//
// The vacuum's patience is a minute too, so a sweep that the vacuum holds up
// waits little or nothing more, and one that it does not takes a minute
// longer than it would. A cancelled sweep stops waiting at once
// (TestCancelledSweepStopsWaitingForTheStatistics).
const statsHandOver = 65 * time.Second

// cancelGrace is how long a statement on RewriteCredentials's connection
// waits, once the sweep is cancelled, for PostgreSQL to answer the cancel
// request, and then how long the sweep waits for pgx to send its own if it
// did not.
const cancelGrace = 3 * time.Second

// sweepConnection opens the connection RewriteCredentials works on, set up as
// that function says, with notices collecting the codes of the warnings it is
// sent.
//
// A sweep is cancelled by SIGTERM, which a Job past its task timeout gets, or
// by Ctrl-C on make sweep, and the process exits as soon as the sweep returns.
// PostgreSQL does not notice a closed socket until the statement on it ends,
// so a statement nobody has asked it to cancel by then runs on: a VACUUM FULL
// in flight holds its table against every read to its end, and commits, and a
// rewrite holds its lock to the end of its statement. Two things see that the
// cancel request has gone out before the sweep returns, and while PostgreSQL
// answers promptly either would on its own.
//
// Here the context's cancellation sends the cancel request first, and the
// statement waits up to the store's cancelGrace for PostgreSQL's answer,
// which is the statement stopping: it fails with PostgreSQL's "canceling
// statement due to user request". That is pgx's
// CancelRequestContextWatcherHandler. pgx's default, which the pool's
// connections keep, gives up on the socket at once instead, and sends the
// cancel request from a goroutine as it closes the connection; that is also
// what this connection falls back to when no answer comes within the grace,
// and closeSweepConnection waits for that goroutine. (Closing the pool waits
// for its connections' goroutines the same way, and the sweeper closes it
// before it exits.)
//
// TestCancelledSweepStopsItsStatementOnTheServer fails only without both; its
// check that the error is PostgreSQL's fails without the handler, and
// TestCancelledSweepWaitsForPgxsOwnCancelRequest fails without the wait.
func (s *Store) sweepConnection(ctx context.Context, notices *noticeLog) (*pgx.Conn, error) {
	config := s.pool.Config().ConnConfig
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["lock_timeout"] = verificationLockTimeout
	config.RuntimeParams["client_min_messages"] = "warning"
	config.OnNotice = notices.add
	grace := s.cancelGrace
	config.BuildContextWatcherHandler = func(conn *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: conn, DeadlineDelay: grace}
	}
	return pgx.ConnectConfig(ctx, config)
}

// closeSweepConnection closes RewriteCredentials's connection. When
// PostgreSQL did not answer a cancel request within the grace, pgx gave up on
// the connection and is sending a cancel request of its own, and then closing
// it, from a goroutine, so this waits up to grace for that to finish before
// the sweep returns and the process exits.
func closeSweepConnection(ctx context.Context, conn *pgx.Conn, grace time.Duration) {
	_ = conn.Close(context.WithoutCancel(ctx))
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-conn.PgConn().CleanupDone():
	case <-timer.C:
	}
}

// vacuumVerificationTables runs VACUUM FULL on each verification table once it
// can leave no row deleted by the rewrite (transaction id rewrite) or before
// it in the table's new file, trying for up to s.vacuumPatience.
//
// VACUUM FULL writes a table to a new file and drops the old one; it cannot
// run in a transaction. For as long as it runs it holds the table against
// every read and write, CredentialLabel's included (verificationLockTimeout
// says what that costs), so it runs on each table only when it can do its
// job: once a night, unless something gets in its way. Three things can.
//
// Another session can hold the table. VACUUM FULL waiting for it would queue
// every badged post and confirmation behind its wait as well as its run, so it
// runs with SKIP_LOCKED: it passes over a table it cannot take at once, with a
// warning, and this tries again a moment later. SKIP_LOCKED does not cover
// what VACUUM FULL needs once it has the table — locks on its TOAST table and
// its indexes, and its row in pg_class — and it holds the table while it waits
// for one, some only after it has copied the whole table (vacuumLockTimeout
// says who can hold them). So vacuumFull first looks for a session holding
// one, and while there is one leaves the table alone and this tries again
// later; and this sets the connection's lock_timeout to vacuumLockTimeout, so
// that a run that meets one begun since the look gives the table up after
// that long, or sooner if the holder commits a change to the row it waits for,
// with the table still in its old file, and is tried again the same way. The
// step a VACUUM ends with across the whole database, which waits
// for any transaction that has updated the database's row in pg_database,
// comes after the run has committed and holds no table; the runs leave it out
// (vacuumCommand).
//
// It copies, rather than drops, a deleted row that some snapshot could still
// read: one deleted by a transaction no older than its cutoff. The cutoff is
// the oldest snapshot or transaction still open in this database, or older:
// VACUUM FULL's own snapshot counts, and that is older than every transaction
// still open anywhere on the instance that has written something. So a
// snapshot in this database taken before the rewrite committed and still in
// use — a statement running then, which a request in flight is enough for, or
// a transaction reading from one snapshot, as an export does — or a
// transaction anywhere on the instance that had written something by then
// would put every row the rewrite deleted into the new file, each with the
// xmin it was issued under, and the pending rows confirmations deleted with
// them, after holding up every request on the table for nothing. VACUUM FULL
// freezes what it keeps and moves the table's relfrozenxid up to its cutoff,
// so those rows are gone exactly when relfrozenxid is newer than the rewrite.
// A run on vacuum_probe, which holds nothing and which nothing else uses,
// computes the same cutoff in a few milliseconds and holds up nobody; so
// before each attempt this runs one there, and until the probe's relfrozenxid
// is newer than the rewrite it waits for the older transactions to end rather
// than take a verification table. A run on a verification table is still
// checked the same way, and one that kept the rows is tried again after the
// next probe.
//
// And it skips, with a different warning, a table its role may not vacuum —
// one it owns neither itself nor through owning the database. Waiting does
// not change that, so the sweep fails at once. The table's file is compared
// before and after to see whether VACUUM FULL ran at all.
//
// It takes pg_statistic the same way, in the same passes: after the
// verification tables in each, whether or not they are done, so a table left
// for the next pass does not hold the catalog back, nor the catalog a table
// (TestSweepFailsWhenTheVerificationTablesAreHeld). Before 0015, ANALYZE
// copied samples of every column of the verification tables into that
// catalog — for the pending table, the email hashes of codes in flight beside
// their expiries, to the microsecond — and 0015 deleted them;
// but a delete leaves the old versions in the catalog's file, and a sample big
// enough to be stored out of line, as the email hashes' histogram is, in its
// TOAST table's file, until something writes them out anew. Only VACUUM FULL
// does: a plain vacuum, autovacuum's included, marks the space free and
// leaves the bytes. No query sees an old version, so the sweep cannot tell
// whether a database upgraded across 0015 still holds them, and it rewrites
// the catalog every night. The catalog is small, so a run is short; but while
// it runs, a query whose plan needs statistics its connection has not cached
// waits, on any table, so here too the sweep passes over the catalog while
// anybody holds it rather than queue them all behind its wait, and leaves it
// alone while anybody else holds its TOAST table, which autovacuum still
// vacuums on its own. Only a superuser or a role with the
// privileges of the database's owner may vacuum it — the owner, or a member of
// the owner's role that inherits its privileges, which pg_has_role(datdba,
// 'USAGE') says; a membership that does not inherit is not enough, and nor is
// owning the tables — and a logical replication slot holds its cutoff back as
// an older transaction does.
func (s *Store) vacuumVerificationTables(ctx context.Context, conn *pgx.Conn, notices *noticeLog,
	rewrite uint32) error {
	if _, err := conn.Exec(ctx, `SELECT set_config('lock_timeout', $1, false)`, vacuumLockTimeout); err != nil {
		return fmt.Errorf("set the vacuum's lock timeout: %w", err)
	}
	start := time.Now()
	deadline := start.Add(s.vacuumPatience)
	wait := vacuumFirstWait
	left := slices.Concat(verificationTables[:], []string{statisticsCatalog})
	for {
		var why []string
		switch probe, err := s.vacuumFull(ctx, conn, notices, vacuumProbeTable, rewrite); {
		case err != nil:
			return err
		case probe == vacuumHeld || probe == vacuumWouldWait || probe == vacuumBlocked:
			why = append(why, lockedOut(probe, vacuumProbeTable))
		case probe == vacuumTooSoon:
			why = append(why, "VACUUM FULL would keep the rows the rewrite deleted, which a transaction "+
				"older than the rewrite could still read: a statement or transaction in this database, "+
				"or one anywhere on the instance that had written something")
		default:
			var held []string
			for _, table := range left {
				outcome, err := s.vacuumFull(ctx, conn, notices, table, rewrite)
				if err != nil {
					return err
				}
				switch outcome {
				case vacuumHeld, vacuumWouldWait, vacuumBlocked:
					why = append(why, lockedOut(outcome, table))
				case vacuumTooSoon:
					reader := "a transaction older than the rewrite"
					if table == statisticsCatalog {
						reader += ", or a logical replication slot,"
					}
					why = append(why, fmt.Sprintf("VACUUM FULL kept rows deleted from %s before the rewrite, "+
						"which %s could still read", table, reader))
				default:
					continue
				}
				held = append(held, table)
			}
			if len(held) == 0 {
				return nil
			}
			left = held
		}
		// It stops once the next pause would take it past its patience, which
		// can be seconds before the patience is up, so it says how long it
		// tried rather than how long it might have.
		if time.Until(deadline) < wait {
			return fmt.Errorf("%s; tried for %s, and the next sweep tries again",
				strings.Join(why, "; "), time.Since(start).Truncate(100*time.Millisecond))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("vacuum verification tables: %w", ctx.Err())
		case <-timer.C:
		}
		wait = min(2*wait, vacuumMaxWait)
	}
}

// vacuumOutcome is what one VACUUM FULL of a table came to, when it is
// something waiting can change.
type vacuumOutcome int

const (
	// The table is in a new file holding no row deleted by the rewrite or
	// before it.
	vacuumDone vacuumOutcome = iota
	// Another session held the table, and VACUUM FULL passed over it.
	vacuumHeld
	// Another session held something VACUUM FULL would wait for once it had
	// the table (vacuumWaitsQuery), so it was not run.
	vacuumWouldWait
	// VACUUM FULL took the table, but another session held something it
	// needs with it, a lock on its TOAST table or an index or its row in
	// pg_class, for longer than vacuumLockTimeout, or committed a change to
	// that row while the run waited for it, and the run gave the table up in
	// its old file. (A run that lock_timeout ends after it has moved the table
	// to a new file is not this: it counts as done, or too soon, as any run
	// does.)
	vacuumBlocked
	// The table is in a new file, but a transaction older than the rewrite
	// held VACUUM FULL's cutoff back, so the file holds what the rewrite
	// deleted from it.
	vacuumTooSoon
)

// lockedOut says why VACUUM FULL left table in its old file, for the
// outcomes another session's lock decides.
func lockedOut(outcome vacuumOutcome, table string) string {
	switch outcome {
	case vacuumWouldWait:
		return fmt.Sprintf("VACUUM FULL was not run on %s while another session held what it would wait "+
			"for once it had the table: a lock on its TOAST table or an index, or its row in pg_class or "+
			"an index's, updated by a transaction still open (a GRANT on the table, say)", table)
	case vacuumBlocked:
		return fmt.Sprintf("VACUUM FULL took %s, then gave it up waiting for another session: for a lock "+
			"on its TOAST table or an index, or for its row in pg_class or an index's, which that session "+
			"held for longer than %s or changed meanwhile (a GRANT on the table, say)", table,
			vacuumLockTimeout)
	}
	return fmt.Sprintf("VACUUM FULL could not take %s, which another session held", table)
}

// vacuumFull runs VACUUM FULL on table once and says what it came to, or
// returns an error for what waiting cannot change.
func (s *Store) vacuumFull(ctx context.Context, conn *pgx.Conn, notices *noticeLog,
	table string, rewrite uint32) (vacuumOutcome, error) {
	var waits bool
	if err := conn.QueryRow(ctx, vacuumWaitsQuery, table).Scan(&waits); err != nil {
		return 0, fmt.Errorf("read what VACUUM FULL would wait for on %s: %w", table, err)
	}
	if waits {
		return vacuumWouldWait, nil
	}
	before, err := relationFile(ctx, conn, table)
	if err != nil {
		return 0, err
	}
	command := s.vacuumCommand
	if table == statisticsCatalog {
		command = s.statisticsVacuumCommand
	}
	notices.take()
	_, err = conn.Exec(ctx, command+" "+pgx.Identifier(strings.Split(table, ".")).Sanitize())
	// Another session ended a wait for something other than the table, which
	// SKIP_LOCKED would have passed over: lock_timeout ended it, or the
	// transaction the run waited for to update a row in pg_class committed
	// first, having changed the row (concurrentlyUpdated). A wait in the run
	// rolled the run back, and left the table in its old file; a wait after
	// the run had committed — in the step SKIP_DATABASE_STATS leaves out
	// (vacuumCommand), say — left it in its new one, and the run counts as the
	// run it was.
	var pgErr *pgconn.PgError
	cutShort := errors.As(err, &pgErr) && (pgErr.Code == sqlstateLockNotAvailable ||
		pgErr.Code == sqlstateInternalError && pgErr.Message == concurrentlyUpdated)
	if err != nil && !cutShort {
		return 0, fmt.Errorf("vacuum %s: %w", table, err)
	}
	warnings := notices.take()
	after, err := relationFile(ctx, conn, table)
	if err != nil {
		return 0, err
	}
	if after != before && s.vacuumed != nil {
		s.vacuumed(table)
	}
	if after == before {
		switch {
		case slices.Contains(warnings, sqlstateLockNotAvailable):
			return vacuumHeld, nil
		case cutShort:
			return vacuumBlocked, nil
		case table == statisticsCatalog:
			return 0, fmt.Errorf("VACUUM FULL left %s in its old file: only a superuser or a role with "+
				"the privileges of the database's owner may vacuum it, so the sweeper's database role "+
				"must own the database, or be a member of the owner's role that inherits its privileges; "+
				"SELECT pg_has_role(datdba, 'USAGE') FROM pg_database WHERE datname = current_database(), "+
				"run as that role, says whether it does", table)
		}
		return 0, fmt.Errorf("VACUUM FULL left %s in its old file: the sweeper's database role "+
			"must own the verification tables and %s, as the role that ran the migrations does, "+
			"or the database", table, vacuumProbeTable)
	}
	var clean bool
	if err := conn.QueryRow(ctx,
		`SELECT age(relfrozenxid) < age($2::xid) FROM pg_class WHERE oid = $1::text::regclass`,
		table, rewrite).Scan(&clean); err != nil {
		return 0, fmt.Errorf("read what VACUUM FULL kept of %s: %w", table, err)
	}
	if !clean {
		return vacuumTooSoon, nil
	}
	return vacuumDone, nil
}

// restampScans reads each verification table through each of its indexes, and
// once whole, so that the times PostgreSQL's statistics keep of the last scan
// of each — last_idx_scan in pg_stat_user_indexes, last_seq_scan and
// last_idx_scan in pg_stat_user_tables — are the sweep's. Otherwise they
// would keep the time a request last read a table that way: a confirmation
// checking the credentials' primary key, say, which on a day with a single
// issuance dates that credential to the second though its row says only the
// day. The statistics keep the latest time they are given, so a time they
// were given before this read is gone after it. One that a connection had
// yet to hand over can still come after it, later than this read's, which is
// why RewriteCredentials reads the tables a second time (statsHandOver).
func restampScans(ctx context.Context, conn *pgx.Conn) error {
	return pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		// The second read comes after the vacuum, which leaves the
		// connection's lock_timeout at vacuumLockTimeout. A read waits only
		// for a session that holds the whole table, a schema change, say,
		// and holds nobody up while it waits; so it waits as long as the
		// sweep's other waits outside the vacuum do.
		if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`,
			verificationLockTimeout); err != nil {
			return fmt.Errorf("set the reads' lock timeout: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT c.relname, a.attname
			FROM pg_index i
			JOIN pg_class c ON c.oid = i.indrelid
			JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
			WHERE i.indrelid = ANY ($1::text[]::regclass[])`, verificationTables[:])
		if err != nil {
			return fmt.Errorf("list the verification tables' indexes: %w", err)
		}
		var probes [][2]string
		for rows.Next() {
			var table, column string
			if err := rows.Scan(&table, &column); err != nil {
				rows.Close()
				return fmt.Errorf("list the verification tables' indexes: %w", err)
			}
			probes = append(probes, [2]string{table, column})
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list the verification tables' indexes: %w", err)
		}

		// For this transaction only: with reading the table and sorting
		// priced out, ordering by an index's first column reads that index.
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off; SET LOCAL enable_bitmapscan = off;
			SET LOCAL enable_sort = off`); err != nil {
			return fmt.Errorf("plan index scans: %w", err)
		}
		for _, p := range probes {
			if _, err := tx.Exec(ctx, `SELECT FROM `+pgx.Identifier{p[0]}.Sanitize()+
				` ORDER BY `+pgx.Identifier{p[1]}.Sanitize()+` LIMIT 1`); err != nil {
				return fmt.Errorf("scan an index of %s: %w", p[0], err)
			}
		}
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = on; SET LOCAL enable_indexscan = off;
			SET LOCAL enable_indexonlyscan = off`); err != nil {
			return fmt.Errorf("plan table scans: %w", err)
		}
		for _, table := range verificationTables {
			if _, err := tx.Exec(ctx, `SELECT FROM `+pgx.Identifier{table}.Sanitize()); err != nil {
				return fmt.Errorf("scan %s: %w", table, err)
			}
		}
		return nil
	})
}

// noticeLog collects the codes of the notices and warnings a connection is
// sent, which is how VACUUM reports a table it skipped.
type noticeLog struct {
	mu    sync.Mutex
	codes []string
}

func (n *noticeLog) add(_ *pgconn.PgConn, notice *pgconn.Notice) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.codes = append(n.codes, notice.Code)
}

// take returns the codes collected since it was last called.
func (n *noticeLog) take() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	codes := n.codes
	n.codes = nil
	return codes
}

// relationFile returns the number of the file a table is stored in, which
// VACUUM FULL replaces.
func relationFile(ctx context.Context, conn *pgx.Conn, table string) (int64, error) {
	var file int64
	if err := conn.QueryRow(ctx,
		`SELECT pg_relation_filenode($1::text::regclass)::bigint`, table).Scan(&file); err != nil {
		return 0, fmt.Errorf("find the file %s is stored in: %w", table, err)
	}
	return file, nil
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
		postID, in.Name, img.Path, img.ThumbPath, img.ContentType, img.ThumbContentType,
		img.Size, img.Width, img.Height, img.ThumbWidth, img.ThumbHeight, img.Checksum, in.Spoiler); err != nil {
		return nil, fmt.Errorf("insert attachment: %w", err)
	}
	return &Attachment{
		Name:        in.Name,
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
