package app

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Board is a top-level category threads are posted under.
type Board struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ThreadSummary is a thread as shown in a board index.
type ThreadSummary struct {
	ID        int64     `json:"id"`
	Subject   string    `json:"subject"`
	Replies   int       `json:"replies"`
	CreatedAt time.Time `json:"created_at"`
	BumpedAt  time.Time `json:"bumped_at"`
	Preview   string    `json:"preview"`
}

// Post is a single message within a thread.
type Post struct {
	ID        int64     `json:"id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// ThreadDetail is a thread with its full list of posts.
type ThreadDetail struct {
	ID        int64     `json:"id"`
	Board     string    `json:"board"`
	Subject   string    `json:"subject"`
	CreatedAt time.Time `json:"created_at"`
	Posts     []Post    `json:"posts"`
}

func (a *App) listBoards(ctx context.Context) ([]Board, error) {
	rows, err := a.pool.Query(ctx, `SELECT slug, name, description FROM boards ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	boards := []Board{}
	for rows.Next() {
		var b Board
		if err := rows.Scan(&b.Slug, &b.Name, &b.Description); err != nil {
			return nil, err
		}
		boards = append(boards, b)
	}
	return boards, rows.Err()
}

func (a *App) boardID(ctx context.Context, slug string) (int, bool, error) {
	var id int
	err := a.pool.QueryRow(ctx, `SELECT id FROM boards WHERE slug = $1`, slug).Scan(&id)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, err
	}
	return id, true, nil
}

func (a *App) listThreads(ctx context.Context, boardID int) ([]ThreadSummary, error) {
	rows, err := a.pool.Query(ctx, `
		SELECT t.id, t.subject, t.created_at, t.bumped_at,
		       COALESCE(GREATEST(COUNT(p.id) - 1, 0), 0) AS replies,
		       COALESCE((SELECT body FROM posts WHERE thread_id = t.id ORDER BY id LIMIT 1), '') AS preview
		FROM threads t
		LEFT JOIN posts p ON p.thread_id = t.id
		WHERE t.board_id = $1
		GROUP BY t.id
		ORDER BY t.bumped_at DESC`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	threads := []ThreadSummary{}
	for rows.Next() {
		var t ThreadSummary
		if err := rows.Scan(&t.ID, &t.Subject, &t.CreatedAt, &t.BumpedAt, &t.Replies, &t.Preview); err != nil {
			return nil, err
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

// createThread creates a thread and its opening post in one transaction.
func (a *App) createThread(ctx context.Context, boardID int, subject, author, body string) (int64, error) {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var threadID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO threads (board_id, subject) VALUES ($1, $2) RETURNING id`,
		boardID, subject).Scan(&threadID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO posts (thread_id, author, body) VALUES ($1, $2, $3)`,
		threadID, author, body); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return threadID, nil
}

func (a *App) getThread(ctx context.Context, id int64) (*ThreadDetail, error) {
	var detail ThreadDetail
	err := a.pool.QueryRow(ctx, `
		SELECT t.id, b.slug, t.subject, t.created_at
		FROM threads t JOIN boards b ON b.id = t.board_id
		WHERE t.id = $1`, id).Scan(&detail.ID, &detail.Board, &detail.Subject, &detail.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	rows, err := a.pool.Query(ctx,
		`SELECT id, author, body, created_at FROM posts WHERE thread_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	detail.Posts = []Post{}
	for rows.Next() {
		var p Post
		if err := rows.Scan(&p.ID, &p.Author, &p.Body, &p.CreatedAt); err != nil {
			return nil, err
		}
		detail.Posts = append(detail.Posts, p)
	}
	return &detail, rows.Err()
}

// addPost appends a reply to a thread and bumps the thread to the top of its board.
func (a *App) addPost(ctx context.Context, threadID int64, author, body string) (bool, error) {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE threads SET bumped_at = now() WHERE id = $1`, threadID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO posts (thread_id, author, body) VALUES ($1, $2, $3)`,
		threadID, author, body); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
