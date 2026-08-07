package app

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds runtime configuration sourced from the environment.
type Config struct {
	Addr        string
	DatabaseURL string
}

// ConfigFromEnv builds a Config, applying sensible local-development defaults so
// the server runs out of the box against the Postgres instance provisioned by
// the Cloud Agent environment.
func ConfigFromEnv() Config {
	return Config{
		Addr:        firstNonEmpty(os.Getenv("EDUCHAN_ADDR"), ":8080"),
		DatabaseURL: firstNonEmpty(os.Getenv("DATABASE_URL"), "postgres://educhan:educhan@localhost:5432/educhan?sslmode=disable"),
	}
}

// App wires together the database pool and HTTP handlers.
type App struct {
	pool *pgxpool.Pool
}

// New connects to Postgres, verifies connectivity, and applies the schema.
func New(ctx context.Context, cfg Config) (*App, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &App{pool: pool}, nil
}

// Close releases database resources.
func (a *App) Close() {
	if a.pool != nil {
		a.pool.Close()
	}
}

// Routes returns the HTTP handler for the whole application.
func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("GET /api/boards", a.handleListBoards)
	mux.HandleFunc("GET /api/boards/{slug}/threads", a.handleListThreads)
	mux.HandleFunc("POST /api/boards/{slug}/threads", a.handleCreateThread)
	mux.HandleFunc("GET /api/threads/{id}", a.handleGetThread)
	mux.HandleFunc("POST /api/threads/{id}/posts", a.handleCreatePost)

	mux.Handle("GET /", http.FileServerFS(webFS()))

	return withCommonHeaders(mux)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
