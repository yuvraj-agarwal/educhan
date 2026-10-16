package app

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/mailer"
)

// Config holds runtime configuration sourced from the environment.
type Config struct {
	// Addr is the TCP address the HTTP server listens on.
	Addr string
	// DatabaseURL is a libpq-style PostgreSQL connection string.
	DatabaseURL string
	// DBMaxConns caps this process's connection pool. Every instance holds its
	// own pool, so instances times this is what the database has to allow.
	// Zero leaves the pool at pgx's default or a pool_max_conns in DatabaseURL.
	DBMaxConns int32
	// PosterIDSalt keys the per-thread poster IDs and secure tripcodes. It must
	// stay secret and stable: rotating it changes every future ID.
	PosterIDSalt string
	// PosterIDSaltEphemeral reports that no salt was configured and a random one
	// was generated for this process.
	PosterIDSaltEphemeral bool
	// TrustedProxyHops is how many reverse proxies stand in front of the server,
	// each appending the address it saw to X-Forwarded-For. Zero — the default —
	// ignores the header entirely and keys on the peer address, which is the only
	// safe reading for a server exposed directly. See clientIP.
	TrustedProxyHops int
	// ThreadCooldown, PostCooldown, and UploadCooldown are the minimum intervals
	// between two new threads, two replies, and two attachments from the same
	// client. Zero disables the check.
	ThreadCooldown time.Duration
	PostCooldown   time.Duration
	UploadCooldown time.Duration
	// LogFormat selects the slog handler: "text" or "json".
	LogFormat string
	// ModKey is the shared secret moderators present to the moderation
	// endpoints. An empty key disables them.
	ModKey string
	// MediaBackend selects where originals and thumbnails are kept:
	// MediaBackendLocal (the default) or MediaBackendGCS.
	MediaBackend string
	// UploadDir is the root the local media backend writes originals and
	// thumbnails to. Unused by the GCS backend.
	UploadDir string
	// GCSBucket is the Cloud Storage bucket the GCS backend stores objects in.
	// Required by that backend and ignored by the local one.
	GCSBucket string
	// GCSPrefix is an optional key prefix within GCSBucket, so a bucket can be
	// shared with other content. Empty stores objects at the bucket root.
	GCSPrefix string
	// MaxUploadBytes is the largest attachment accepted with a post.
	MaxUploadBytes int64
	// UploadsEnabled accepts image attachments. It is off unless set: images
	// are stored and served exactly as uploaded, with no metadata stripping
	// and no content scanning yet, so a deployment opts in knowingly.
	UploadsEnabled bool
	// VerifySecret keys institutional verification: the HMAC that stands in for
	// an address, the hash of a pending code, and the signature on an issued
	// credential. An empty secret switches the feature off, because there is
	// nothing to sign a credential with. Rotating it revokes every credential
	// and orphans every uniqueness key.
	VerifySecret string
	// VerifyCodeTTL is how long an emailed code stays usable.
	VerifyCodeTTL time.Duration
	// VerifyCredentialTTL is how long an issued badge lasts before the holder
	// has to verify again.
	VerifyCredentialTTL time.Duration
	// VerifyCooldown is the minimum interval between two code requests, applied
	// per client address and per address hash.
	VerifyCooldown time.Duration
	// VerifyDomainsFile is an allowlist file replacing the embedded default.
	VerifyDomainsFile string
	// Sender delivers verification codes. It is a field rather than an
	// environment setting because choosing a provider means linking one in.
	// Nil selects the development sender, which prints each code and its
	// address to stdout, and New refuses that while verification is on unless
	// MailDev says it was meant.
	Sender mailer.Sender
	// MailDev allows the development sender while verification is on. It is
	// for local work only: in production stdout is the log.
	MailDev bool
	// CaptchaSecret keys the captcha: the signature on every challenge, and on
	// the key each one is made from. An empty secret switches the captcha off,
	// so posts and code requests need none. It must be the same on every
	// instance, since any of them may check an answer another issued, and
	// rotating it only invalidates the challenges out at the time.
	CaptchaSecret string
	// CaptchaWork is the work a challenge asks of the browser at the base
	// difficulty: the average counter its answer is at, one fewer than the
	// attempts it takes on average. Raising it makes every post dearer for a
	// script and slower for a person. Zero means defaultCaptchaWork.
	CaptchaWork int

	// captcha replaces the verifier CaptchaSecret selects, so a test can pass or
	// fail every answer without solving puzzles. No setting reads it.
	captcha captchaVerifier

	// cooldownSweepEvery replaces cooldownSweepInterval, the period of the
	// sweep that drops expired flood-control entries, so a test can watch it
	// run without waiting a minute. No setting reads it; zero means the
	// interval.
	cooldownSweepEvery time.Duration
}

// defaultDBMaxConns keeps one instance's pool small. The smallest Cloud SQL
// tier allows about 25 connections, and Cloud Run runs several instances, each
// with a pool of its own; deploy/README.md section 3 has the budget.
const defaultDBMaxConns = 3

// defaultMaxUploadBytes caps a single attachment. Imageboard posts are small;
// 8 MiB comfortably covers a phone photo without inviting abuse.
const defaultMaxUploadBytes int64 = 8 << 20

// The media backends EDUCHAN_MEDIA_BACKEND selects between. Local is the
// default so an existing deployment, the compose stack, and a developer's
// checkout all keep working with no new configuration.
const (
	MediaBackendLocal = "local"
	MediaBackendGCS   = "gcs"
)

// Verification defaults: long enough to fetch an email from another device,
// long enough that a badge is not a chore to keep, and slow enough that a code
// request is not a way to mail somebody repeatedly.
const (
	defaultVerifyCodeTTL       = 15 * time.Minute
	defaultVerifyCredentialTTL = 180 * 24 * time.Hour
	defaultVerifyCooldown      = time.Minute
)

// ConfigFromEnv builds a Config, applying local-development defaults so the
// server runs out of the box against the Postgres instance provisioned by the
// Cloud Agent environment.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Addr:              listenAddr(),
		DatabaseURL:       envOr("DATABASE_URL", "postgres://educhan:educhan@localhost:5432/educhan?sslmode=disable"),
		PosterIDSalt:      os.Getenv("EDUCHAN_POSTER_ID_SALT"),
		LogFormat:         envOr("EDUCHAN_LOG_FORMAT", "text"),
		MediaBackend:      strings.ToLower(strings.TrimSpace(envOr("EDUCHAN_MEDIA_BACKEND", MediaBackendLocal))),
		UploadDir:         envOr("EDUCHAN_UPLOAD_DIR", "./data/uploads"),
		GCSBucket:         strings.TrimSpace(os.Getenv("EDUCHAN_GCS_BUCKET")),
		GCSPrefix:         strings.TrimSpace(os.Getenv("EDUCHAN_GCS_PREFIX")),
		ModKey:            strings.TrimSpace(os.Getenv("EDUCHAN_MOD_KEY")),
		VerifySecret:      strings.TrimSpace(os.Getenv("EDUCHAN_VERIFY_SECRET")),
		VerifyDomainsFile: strings.TrimSpace(os.Getenv("EDUCHAN_VERIFY_DOMAINS")),
		CaptchaSecret:     strings.TrimSpace(os.Getenv("EDUCHAN_CAPTCHA_SECRET")),
	}

	var err error
	if cfg.TrustedProxyHops, err = envProxyHops("EDUCHAN_TRUST_PROXY"); err != nil {
		return Config{}, err
	}
	if cfg.ThreadCooldown, err = envDuration("EDUCHAN_THREAD_COOLDOWN", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.PostCooldown, err = envDuration("EDUCHAN_POST_COOLDOWN", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.UploadCooldown, err = envDuration("EDUCHAN_UPLOAD_COOLDOWN", 15*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.DBMaxConns, err = envPoolSize("EDUCHAN_DB_MAX_CONNS", defaultDBMaxConns); err != nil {
		return Config{}, err
	}
	if cfg.MaxUploadBytes, err = envBytes("EDUCHAN_MAX_UPLOAD_BYTES", defaultMaxUploadBytes); err != nil {
		return Config{}, err
	}
	if cfg.UploadsEnabled, err = envBool("EDUCHAN_UPLOADS_ENABLED"); err != nil {
		return Config{}, err
	}
	if cfg.MailDev, err = envBool("EDUCHAN_MAIL_DEV"); err != nil {
		return Config{}, err
	}
	if cfg.VerifyCodeTTL, err = envDuration("EDUCHAN_VERIFY_CODE_TTL", defaultVerifyCodeTTL); err != nil {
		return Config{}, err
	}
	if cfg.VerifyCredentialTTL, err = envDuration("EDUCHAN_VERIFY_CREDENTIAL_TTL", defaultVerifyCredentialTTL); err != nil {
		return Config{}, err
	}
	if cfg.VerifyCooldown, err = envDuration("EDUCHAN_VERIFY_COOLDOWN", defaultVerifyCooldown); err != nil {
		return Config{}, err
	}
	if cfg.CaptchaWork, err = envCaptchaWork("EDUCHAN_CAPTCHA_WORK"); err != nil {
		return Config{}, err
	}
	// A zero TTL would mint credentials that have already expired, which reads
	// as the feature silently not working.
	if cfg.VerifyCodeTTL <= 0 || cfg.VerifyCredentialTTL <= 0 {
		return Config{}, fmt.Errorf("EDUCHAN_VERIFY_CODE_TTL and EDUCHAN_VERIFY_CREDENTIAL_TTL must be positive")
	}
	if cfg.LogFormat != "text" && cfg.LogFormat != "json" {
		return Config{}, fmt.Errorf("EDUCHAN_LOG_FORMAT: want \"text\" or \"json\", got %q", cfg.LogFormat)
	}
	switch cfg.MediaBackend {
	case MediaBackendLocal:
	case MediaBackendGCS:
		// Caught here rather than at the first upload: a bucketless GCS backend
		// is a deployment that cannot store anything, and it should fail to
		// start rather than to post.
		if cfg.GCSBucket == "" {
			return Config{}, fmt.Errorf("EDUCHAN_GCS_BUCKET is required when EDUCHAN_MEDIA_BACKEND is %q", MediaBackendGCS)
		}
	default:
		return Config{}, fmt.Errorf("EDUCHAN_MEDIA_BACKEND: want %q or %q, got %q", MediaBackendLocal, MediaBackendGCS, cfg.MediaBackend)
	}

	if cfg.PosterIDSalt == "" {
		salt, err := randomSalt()
		if err != nil {
			return Config{}, err
		}
		cfg.PosterIDSalt = salt
		cfg.PosterIDSaltEphemeral = true
	}
	return cfg, nil
}

// listenAddr resolves the address the server binds to. EDUCHAN_ADDR is the
// project's own setting and wins. PORT is the contract container runtimes such
// as Cloud Run inject — it carries a bare port number and the server must listen
// on it — so it is honoured next, ahead of the local default.
func listenAddr() string {
	if addr := strings.TrimSpace(os.Getenv("EDUCHAN_ADDR")); addr != "" {
		return addr
	}
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		return ":" + port
	}
	return ":8080"
}

func randomSalt() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate poster ID salt: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBytes(key string, fallback int64) (int64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s: must be positive", key)
	}
	return n, nil
}

// envPoolSize reads a connection count, which must be at least one.
func envPoolSize(key string, fallback int32) (int32, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("%s: must be at least 1", key)
	}
	return int32(n), nil
}

// envCaptchaWork reads the captcha's base work, the average counter an answer
// is at, which must be at least one (an answer at counter one, two attempts)
// and at most maxCaptchaWork/captchaMaxScale: small enough that the base
// puzzle stays inside the time app.js gives one, with room for a busy
// address's puzzles to be made harder.
func envCaptchaWork(key string) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defaultCaptchaWork, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if n < 1 || n > maxCaptchaWork/captchaMaxScale {
		return 0, fmt.Errorf("%s: must be between 1 and %d", key, maxCaptchaWork/captchaMaxScale)
	}
	return n, nil
}

// envBool reads an on/off setting that is off unless set. Anything that is not
// recognisably a boolean is an error rather than "off", so a typo in a switch
// is heard about at startup.
func envBool(key string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	switch strings.ToLower(raw) {
	case "", "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	}
	return false, fmt.Errorf("%s: want a boolean, got %q", key, raw)
}

// envProxyHops reads the trusted proxy depth. It accepts a hop count, and the
// booleans the setting used to take: "true" means the single proxy the original
// flag implied, anything false-ish means none.
func envProxyHops(key string) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	switch strings.ToLower(raw) {
	case "", "false", "no", "off":
		return 0, nil
	case "true", "yes", "on":
		return 1, nil
	}
	hops, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: want a proxy hop count or a boolean, got %q", key, raw)
	}
	if hops < 0 {
		return 0, fmt.Errorf("%s: must not be negative", key)
	}
	return hops, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	// Bare numbers are read as seconds, which is what operators tend to expect
	// from a "cooldown" knob.
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0, fmt.Errorf("%s: must not be negative", key)
		}
		return time.Duration(secs) * time.Second, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: must not be negative", key)
	}
	return d, nil
}
