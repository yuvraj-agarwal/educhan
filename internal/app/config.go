package app

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds runtime configuration sourced from the environment.
type Config struct {
	// Addr is the TCP address the HTTP server listens on.
	Addr string
	// DatabaseURL is a libpq-style PostgreSQL connection string.
	DatabaseURL string
	// PosterIDSalt keys the per-thread poster IDs and secure tripcodes. It must
	// stay secret and stable: rotating it changes every future ID.
	PosterIDSalt string
	// PosterIDSaltEphemeral reports that no salt was configured and a random one
	// was generated for this process.
	PosterIDSaltEphemeral bool
	// TrustProxyHeaders makes the server read the client address from
	// X-Forwarded-For. Only enable it behind a proxy that overwrites the header.
	TrustProxyHeaders bool
	// ThreadCooldown and PostCooldown are the minimum intervals between two new
	// threads and two replies from the same client. Zero disables the check.
	ThreadCooldown time.Duration
	PostCooldown   time.Duration
	// LogFormat selects the slog handler: "text" or "json".
	LogFormat string
	// UploadDir is the root the media store writes originals and thumbnails to.
	UploadDir string
	// MaxUploadBytes is the largest attachment accepted with a post.
	MaxUploadBytes int64
}

// defaultMaxUploadBytes caps a single attachment. Imageboard posts are small;
// 8 MiB comfortably covers a phone photo without inviting abuse.
const defaultMaxUploadBytes int64 = 8 << 20

// ConfigFromEnv builds a Config, applying local-development defaults so the
// server runs out of the box against the Postgres instance provisioned by the
// Cloud Agent environment.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Addr:              envOr("EDUCHAN_ADDR", ":8080"),
		DatabaseURL:       envOr("DATABASE_URL", "postgres://educhan:educhan@localhost:5432/educhan?sslmode=disable"),
		PosterIDSalt:      os.Getenv("EDUCHAN_POSTER_ID_SALT"),
		TrustProxyHeaders: os.Getenv("EDUCHAN_TRUST_PROXY") == "true",
		LogFormat:         envOr("EDUCHAN_LOG_FORMAT", "text"),
		UploadDir:         envOr("EDUCHAN_UPLOAD_DIR", "./data/uploads"),
	}

	var err error
	if cfg.ThreadCooldown, err = envDuration("EDUCHAN_THREAD_COOLDOWN", 15*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.PostCooldown, err = envDuration("EDUCHAN_POST_COOLDOWN", 3*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.MaxUploadBytes, err = envBytes("EDUCHAN_MAX_UPLOAD_BYTES", defaultMaxUploadBytes); err != nil {
		return Config{}, err
	}
	if cfg.LogFormat != "text" && cfg.LogFormat != "json" {
		return Config{}, fmt.Errorf("EDUCHAN_LOG_FORMAT: want \"text\" or \"json\", got %q", cfg.LogFormat)
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
