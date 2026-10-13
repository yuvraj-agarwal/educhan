package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yuvraj-agarwal/educhan/internal/mailer"
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
	// domains and sender back institutional verification: which addresses count
	// as academic, and how a code reaches one.
	domains *domainAllowlist
	sender  mailer.Sender

	threadLimiter *cooldownLimiter
	postLimiter   *cooldownLimiter
	uploadLimiter *cooldownLimiter
	verifyLimiter *cooldownLimiter
	// refusals counts the posts the content rules sent back to each client.
	refusals *refusalBudget
	// cooldownSweep drops the limiters' expired entries, which are client
	// addresses: on a timer, and before a request when the timer has fallen
	// behind. stopCooldownSweep stops the timer; Close calls it.
	cooldownSweep     *cooldownSweep
	stopCooldownSweep func()

	// schemaAhead lists the migrations the database has and this binary does
	// not: a newer release migrated it. The sweeper refuses to run on one.
	schemaAhead []int
}

// New connects to Postgres, verifies connectivity, applies pending migrations,
// and prepares the media store.
func New(ctx context.Context, cfg Config, log *slog.Logger) (*App, error) {
	if err := checkMailer(cfg); err != nil {
		return nil, err
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if cfg.DBMaxConns > 0 {
		poolCfg.MaxConns = cfg.DBMaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	schemaAhead, err := migrate(ctx, pool, log)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	blobs, err := newMediaStore(ctx, cfg)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("prepare media store: %w", err)
	}
	// Parsed whether or not verification is switched on: a domain file that
	// does not load is a configuration mistake worth hearing about at startup
	// rather than the first time somebody tries to verify.
	domains, err := loadDomainAllowlist(cfg.VerifyDomainsFile)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("load verification domains: %w", err)
	}

	sender := cfg.Sender
	if sender == nil {
		sender = mailer.NewDev(os.Stdout)
	}
	a := &App{
		cfg:           cfg,
		log:           log,
		pool:          pool,
		store:         NewStore(pool),
		media:         blobs,
		domains:       domains,
		sender:        sender,
		threadLimiter: newCooldownLimiter(cfg.ThreadCooldown),
		postLimiter:   newCooldownLimiter(cfg.PostCooldown),
		uploadLimiter: newCooldownLimiter(cfg.UploadCooldown),
		verifyLimiter: newCooldownLimiter(cfg.VerifyCooldown),
		refusals:      newRefusalBudget(cfg.RefusalLimit, refusalWindow),
		schemaAhead:   schemaAhead,
	}
	every := cfg.cooldownSweepEvery
	if every <= 0 {
		every = cooldownSweepInterval
	}
	a.cooldownSweep = newCooldownSweep(every,
		a.threadLimiter, a.postLimiter, a.uploadLimiter, a.verifyLimiter, a.refusals)
	a.stopCooldownSweep = a.cooldownSweep.start()
	return a, nil
}

// WarnServingSettings logs the settings a web server is missing or should not
// have: the salt that keeps poster IDs stable across restarts, the moderation
// key, verification, and the development mail sender. It is the serving path's
// to call, not New's, because the sweeper builds an App too and runs without
// any of them by design; warnings about a server it is not running would only
// teach an operator to give it secrets it does not need.
func (a *App) WarnServingSettings() {
	if a.cfg.PosterIDSaltEphemeral {
		a.log.Warn("EDUCHAN_POSTER_ID_SALT is unset; poster IDs and secure tripcodes will change on restart")
	}
	if a.cfg.ModKey == "" {
		a.log.Warn("EDUCHAN_MOD_KEY is unset; moderation endpoints will refuse every request")
	}
	switch {
	case a.cfg.VerifySecret == "":
		a.log.Warn("EDUCHAN_VERIFY_SECRET is unset; institutional verification is disabled")
	case a.cfg.Sender == nil:
		a.log.Warn("EDUCHAN_MAIL_DEV is set; verification codes are printed to stdout with their address")
	}
}

// checkMailer refuses to start verification with nowhere safe to send codes.
// The development sender prints every address and code to stdout, which on
// Cloud Run is the log, so falling back to it silently would turn a missing
// mail provider into a log of who verified where. It takes EDUCHAN_MAIL_DEV to
// say that is what was meant.
func checkMailer(cfg Config) error {
	if cfg.VerifySecret != "" && cfg.Sender == nil && !cfg.MailDev {
		return errors.New("EDUCHAN_VERIFY_SECRET is set but no mail sender is configured; " +
			"set EDUCHAN_MAIL_DEV=1 to print codes to stdout for local development")
	}
	return nil
}

// newMediaStore builds the blob store the configured backend calls for. It is
// the single place a media.Store is constructed: everything above the interface
// — handlers, SQL, the media package's own Save and Remove — is the same code
// whichever backend answers.
func newMediaStore(ctx context.Context, cfg Config) (media.Store, error) {
	switch cfg.MediaBackend {
	case MediaBackendGCS:
		// The client outlives startup, so it must not hold a context that is
		// cancelled once startup is over: cmd/server bounds app.New with a
		// timeout, and a client whose credentials could not refresh past it
		// would fail its first upload half a minute in.
		return media.NewGCSStore(context.WithoutCancel(ctx), cfg.GCSBucket, cfg.GCSPrefix)
	case MediaBackendLocal, "":
		// Empty means local as well, so a Config built in code — the tests do —
		// needs no backend field to behave as it always has.
		return media.NewLocalStore(cfg.UploadDir)
	default:
		return nil, fmt.Errorf("unknown media backend %q", cfg.MediaBackend)
	}
}

// Close stops the flood-control sweep and releases database resources and, for
// a backend that holds a connection of its own, the media store's.
func (a *App) Close() {
	if a.stopCooldownSweep != nil {
		a.stopCooldownSweep()
	}
	if a.pool != nil {
		a.pool.Close()
	}
	if closer, ok := a.media.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			a.log.Warn("closing media store", "error", err)
		}
	}
}

// Routes returns the HTTP handler for the whole application.
func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("GET /api/site", a.handleSite)

	mux.HandleFunc("GET /api/boards", a.handleListBoards)
	mux.HandleFunc("GET /api/boards/{slug}", a.handleGetBoard)
	mux.HandleFunc("GET /api/boards/{slug}/threads", a.handleIndex)
	mux.HandleFunc("POST /api/boards/{slug}/threads", a.handleCreateThread)
	mux.HandleFunc("GET /api/boards/{slug}/catalog", a.handleCatalog)
	mux.HandleFunc("GET /api/boards/{slug}/archive", a.handleArchive)
	mux.HandleFunc("GET /api/boards/{slug}/threads/{no}", a.handleGetThread)
	mux.HandleFunc("POST /api/boards/{slug}/threads/{no}/posts", a.handleCreatePost)
	mux.HandleFunc("GET /api/boards/{slug}/posts/{no}", a.handleResolvePost)

	// Institutional verification. Nothing here is addressed by anything a post
	// carries, which is the point.
	// Forgetting a badge needs no endpoint: the client holds the credential, so
	// dropping it is a client-side act the server has nothing to say about.
	mux.HandleFunc("GET /api/verify", a.handleVerifyStatus)
	mux.HandleFunc("POST /api/verify/request", a.handleVerifyRequest)
	mux.HandleFunc("POST /api/verify/confirm", a.handleVerifyConfirm)

	// Thread routes keyed by primary key, kept for callers that hold an ID from
	// an earlier revision of the API.
	mux.HandleFunc("GET /api/threads/{id}", a.handleGetThreadByID)
	mux.HandleFunc("POST /api/threads/{id}/posts", a.handleCreatePostByThreadID)

	// Moderation, guarded by the shared moderation key.
	mux.HandleFunc("POST /api/boards", a.handleCreateBoard)
	mux.HandleFunc("PATCH /api/boards/{slug}", a.handleUpdateBoard)
	mux.HandleFunc("POST /api/boards/{slug}/threads/{no}/sticky", a.handleSticky)
	mux.HandleFunc("POST /api/boards/{slug}/threads/{no}/unsticky", a.handleUnsticky)
	mux.HandleFunc("POST /api/boards/{slug}/threads/{no}/lock", a.handleLock)
	mux.HandleFunc("POST /api/boards/{slug}/threads/{no}/unlock", a.handleUnlock)
	mux.HandleFunc("DELETE /api/boards/{slug}/threads/{no}", a.handleDeleteThread)
	mux.HandleFunc("DELETE /api/boards/{slug}/threads/{no}/posts/{post}", a.handleDeletePost)
	mux.HandleFunc("GET /api/held", a.handleListHeld)
	mux.HandleFunc("POST /api/boards/{slug}/threads/{no}/posts/{post}/approve", a.handleApprovePost)

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
		a.cooldownSweep.beforeEachRequest,
		requestLogger(a.log),
		securityHeaders,
	)
}
