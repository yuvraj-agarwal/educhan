package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yuvraj-agarwal/educhan/internal/media"
)

// App wires together configuration, the store, the media store, and the HTTP
// handlers.
type App struct {
	cfg   Config
	log   *slog.Logger
	pool  *pgxpool.Pool
	store *Store
	media media.Store

	threadLimiter *cooldownLimiter
	postLimiter   *cooldownLimiter
}

// New connects to Postgres, verifies connectivity, applies pending migrations,
// and prepares the media store.
func New(ctx context.Context, cfg Config, log *slog.Logger) (*App, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := migrate(ctx, pool, log); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	blobs, err := media.NewLocalStore(cfg.UploadDir)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("prepare media store: %w", err)
	}
	if cfg.PosterIDSaltEphemeral {
		log.Warn("EDUCHAN_POSTER_ID_SALT is unset; poster IDs and secure tripcodes will change on restart")
	}
	if cfg.ModKey == "" {
		log.Warn("EDUCHAN_MOD_KEY is unset; moderation endpoints will refuse every request")
	}
	return &App{
		cfg:           cfg,
		log:           log,
		pool:          pool,
		store:         NewStore(pool),
		media:         blobs,
		threadLimiter: newCooldownLimiter(cfg.ThreadCooldown),
		postLimiter:   newCooldownLimiter(cfg.PostCooldown),
	}, nil
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
	mux.HandleFunc("GET /api/boards/{slug}", a.handleGetBoard)
	mux.HandleFunc("GET /api/boards/{slug}/threads", a.handleIndex)
	mux.HandleFunc("POST /api/boards/{slug}/threads", a.handleCreateThread)
	mux.HandleFunc("GET /api/boards/{slug}/catalog", a.handleCatalog)
	mux.HandleFunc("GET /api/boards/{slug}/threads/{no}", a.handleGetThread)
	mux.HandleFunc("POST /api/boards/{slug}/threads/{no}/posts", a.handleCreatePost)

	// Thread routes keyed by primary key, kept for callers that hold an ID from
	// an earlier revision of the API.
	mux.HandleFunc("GET /api/threads/{id}", a.handleGetThreadByID)
	mux.HandleFunc("POST /api/threads/{id}/posts", a.handleCreatePostByThreadID)

	// Moderation, guarded by the shared moderation key.
	mux.HandleFunc("POST /api/boards/{slug}/threads/{no}/sticky", a.handleSticky)
	mux.HandleFunc("POST /api/boards/{slug}/threads/{no}/unsticky", a.handleUnsticky)
	mux.HandleFunc("POST /api/boards/{slug}/threads/{no}/lock", a.handleLock)
	mux.HandleFunc("POST /api/boards/{slug}/threads/{no}/unlock", a.handleUnlock)
	mux.HandleFunc("DELETE /api/boards/{slug}/threads/{no}", a.handleDeleteThread)
	mux.HandleFunc("DELETE /api/boards/{slug}/threads/{no}/posts/{post}", a.handleDeletePost)

	mux.HandleFunc("GET /media/original/{path...}", a.handleMediaOriginal)
	mux.HandleFunc("GET /media/thumb/{path...}", a.handleMediaThumb)

	// Unknown API paths must answer in JSON rather than falling through to the
	// single-page app.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "unknown endpoint")
	})

	mux.Handle("/", a.webHandler())

	return chain(mux,
		recoverPanics(a.log),
		requestLogger(a.log, a.cfg.TrustProxyHeaders),
		securityHeaders,
	)
}
