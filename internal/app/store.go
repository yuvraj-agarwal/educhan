package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors returned by the store and mapped to HTTP statuses by the
// handlers.
var (
	errNotFound     = errors.New("not found")
	errThreadLocked = errors.New("thread is locked")
)

// indexPageSize and indexRepliesPerThread mirror the classic imageboard index:
// a handful of threads per page, each showing its opening post and the tail of
// the conversation.
const (
	indexPageSize         = 10
	indexRepliesPerThread = 3
	catalogLimit          = 200
)

// Store is the PostgreSQL-backed persistence layer.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps an existing pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const boardColumns = `b.id, b.slug, b.name, b.description, b.bump_limit, b.thread_limit`

func scanBoard(row pgx.Row) (Board, error) {
	var b Board
	err := row.Scan(&b.ID, &b.Slug, &b.Name, &b.Description, &b.BumpLimit, &b.ThreadLimit)
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

// Index returns one page of a board's threads. Pages are 1-based; a page past
// the end comes back empty rather than as an error.
func (s *Store) Index(ctx context.Context, board Board, page int) (BoardIndex, error) {
	if page < 1 {
		page = 1
	}

	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM threads WHERE board_id = $1`, board.ID).Scan(&total); err != nil {
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
		WHERE t.board_id = $1
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
		SELECT `+threadColumns+`, p.body
		FROM threads t
		JOIN posts p ON p.thread_id = t.id AND p.is_op
		WHERE t.board_id = $1
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
		)
		if err := rows.Scan(threadDest(&t, &body)...); err != nil {
			return Catalog{}, err
		}
		t.AtBumpLimit = t.Replies >= board.BumpLimit
		catalog.Threads = append(catalog.Threads, CatalogThread{Thread: t, Excerpt: excerpt(body, excerptLen)})
	}
	return catalog, rows.Err()
}

// Thread loads a thread and all of its posts by board slug and thread number.
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
		SELECT `+postColumns+`
		FROM posts p
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

		var threadID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO threads (board_id, no, subject)
			VALUES ($1, $2, $3)
			RETURNING id`, in.Board.ID, no, in.Subject).Scan(&threadID); err != nil {
			return fmt.Errorf("insert thread: %w", err)
		}

		created, err = insertPost(ctx, tx, newPost{
			BoardID:  in.Board.ID,
			ThreadID: threadID,
			No:       no,
			Identity: in.Identity,
			PosterID: in.PosterID(threadID),
			Body:     in.Body,
			OP:       true,
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
	// Sage suppresses the bump, letting a poster reply without moving the thread
	// back to the top of the board.
	Sage bool
}

// CreateReply appends a post to a thread. The thread is bumped unless the reply
// is saged or the thread has reached its board's bump limit. It returns
// errNotFound for a missing thread and errThreadLocked for a locked one.
func (s *Store) CreateReply(ctx context.Context, in NewReply) (Post, error) {
	var created Post
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var (
			threadID   int64
			locked     bool
			replyCount int
		)
		// Lock the thread row so concurrent replies serialise on the counters.
		err := tx.QueryRow(ctx, `
			SELECT t.id, t.locked, t.reply_count
			FROM threads t
			WHERE t.board_id = $1 AND t.no = $2
			FOR UPDATE`, in.Board.ID, in.ThreadNo).Scan(&threadID, &locked, &replyCount)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if locked {
			return errThreadLocked
		}

		no, err := nextPostNo(ctx, tx, in.Board.ID)
		if err != nil {
			return err
		}
		created, err = insertPost(ctx, tx, newPost{
			BoardID:  in.Board.ID,
			ThreadID: threadID,
			No:       no,
			Identity: in.Identity,
			PosterID: in.PosterID(threadID),
			Body:     in.Body,
		})
		if err != nil {
			return err
		}
		created.ThreadNo = in.ThreadNo

		bump := !in.Sage && replyCount+1 <= in.Board.BumpLimit
		_, err = tx.Exec(ctx, `
			UPDATE threads
			SET reply_count  = reply_count + 1,
			    last_post_at = now(),
			    bumped_at    = CASE WHEN $2 THEN now() ELSE bumped_at END
			WHERE id = $1`, threadID, bump)
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
}

func insertPost(ctx context.Context, tx pgx.Tx, in newPost) (Post, error) {
	p := Post{
		No:       in.No,
		Name:     in.Identity.Name,
		Tripcode: in.Identity.Tripcode,
		PosterID: in.PosterID,
		Body:     in.Body,
		OP:       in.OP,
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO posts (board_id, thread_id, no, name, tripcode, poster_id, body, is_op)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at`,
		in.BoardID, in.ThreadID, in.No, p.Name, p.Tripcode, p.PosterID, p.Body, p.OP,
	).Scan(&p.ID, &p.CreatedAt); err != nil {
		return Post{}, fmt.Errorf("insert post: %w", err)
	}

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

// pruneThreads drops the least recently bumped non-sticky threads once a board
// is over its thread limit, which is how imageboards stay bounded without
// explicit deletion.
func pruneThreads(ctx context.Context, tx pgx.Tx, board Board) error {
	if board.ThreadLimit <= 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		DELETE FROM threads
		WHERE id IN (
		    SELECT id FROM threads
		    WHERE board_id = $1 AND NOT sticky
		    ORDER BY bumped_at DESC
		    OFFSET $2
		)`, board.ID, board.ThreadLimit)
	if err != nil {
		return fmt.Errorf("prune threads: %w", err)
	}
	return nil
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

const postColumns = `p.id, p.no, p.name, p.tripcode, p.poster_id, p.body, p.is_op, p.created_at`

func scanPost(row pgx.Row) (Post, error) {
	p := Post{Quotes: []PostRef{}, Backlinks: []PostRef{}}
	err := row.Scan(&p.ID, &p.No, &p.Name, &p.Tripcode, &p.PosterID, &p.Body, &p.OP, &p.CreatedAt)
	return p, err
}

func (s *Store) opPosts(ctx context.Context, threadIDs []int64) (map[int64]Post, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.thread_id, t.no, `+postColumns+`
		FROM posts p
		JOIN threads t ON t.id = p.thread_id
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
			p        = Post{Quotes: []PostRef{}, Backlinks: []PostRef{}}
		)
		if err := rows.Scan(&threadID, &threadNo,
			&p.ID, &p.No, &p.Name, &p.Tripcode, &p.PosterID, &p.Body, &p.OP, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.ThreadNo = threadNo
		ops[threadID] = p
	}
	return ops, rows.Err()
}

func (s *Store) lastReplies(ctx context.Context, threadIDs []int64, perThread int) (map[int64][]Post, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT thread_id, thread_no, id, no, name, tripcode, poster_id, body, is_op, created_at
		FROM (
		    SELECT p.thread_id, t.no AS thread_no, p.id, p.no, p.name, p.tripcode, p.poster_id,
		           p.body, p.is_op, p.created_at,
		           row_number() OVER (PARTITION BY p.thread_id ORDER BY p.no DESC) AS rank
		    FROM posts p
		    JOIN threads t ON t.id = p.thread_id
		    WHERE p.thread_id = ANY($1) AND NOT p.is_op
		) ranked
		WHERE rank <= $2
		ORDER BY thread_id, no`, threadIDs, perThread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	replies := make(map[int64][]Post, len(threadIDs))
	for rows.Next() {
		var (
			threadID int64
			p        = Post{Quotes: []PostRef{}, Backlinks: []PostRef{}}
		)
		if err := rows.Scan(&threadID, &p.ThreadNo,
			&p.ID, &p.No, &p.Name, &p.Tripcode, &p.PosterID, &p.Body, &p.OP, &p.CreatedAt); err != nil {
			return nil, err
		}
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

const threadColumns = `t.id, t.no, t.subject, t.sticky, t.locked, t.reply_count, t.created_at, t.bumped_at, t.last_post_at`

func threadDest(t *Thread, extra ...any) []any {
	dest := []any{&t.ID, &t.No, &t.Subject, &t.Sticky, &t.Locked, &t.Replies, &t.CreatedAt, &t.BumpedAt, &t.LastPostAt}
	return append(dest, extra...)
}

func scanThread(row pgx.Row, board Board) (Thread, error) {
	var t Thread
	if err := row.Scan(threadDest(&t)...); err != nil {
		return Thread{}, err
	}
	t.AtBumpLimit = t.Replies >= board.BumpLimit
	return t, nil
}
