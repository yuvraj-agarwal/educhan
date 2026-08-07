package app

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
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
		Addr:         envOr("EDUCHAN_ADDR", ":8080"),
		DatabaseURL:  envOr("DATABASE_URL", "postgres://educhan:educhan@localhost:5432/educhan?sslmode=disable"),
		PosterIDSalt: os.Getenv("EDUCHAN_POSTER_ID_SALT"),
		LogFormat:    envOr("EDUCHAN_LOG_FORMAT", "text"),
		UploadDir:    envOr("EDUCHAN_UPLOAD_DIR", "./data/uploads"),
		ModKey:       strings.TrimSpace(os.Getenv("EDUCHAN_MOD_KEY")),
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
