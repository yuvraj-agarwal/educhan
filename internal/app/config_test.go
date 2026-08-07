package app

import (
	"testing"
	"time"
)

func TestConfigFromEnvDefaults(t *testing.T) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.DatabaseURL == "" {
		t.Error("DatabaseURL should have a development default")
	}
	if !cfg.PosterIDSaltEphemeral || cfg.PosterIDSalt == "" {
		t.Error("an unset salt should be replaced by a generated one")
	}
	if cfg.TrustProxyHeaders {
		t.Error("proxy headers must not be trusted by default")
	}
}

func TestConfigFromEnvOverrides(t *testing.T) {
	t.Setenv("EDUCHAN_ADDR", "127.0.0.1:9000")
	t.Setenv("DATABASE_URL", "postgres://example/db")
	t.Setenv("EDUCHAN_POSTER_ID_SALT", "pepper")
	t.Setenv("EDUCHAN_TRUST_PROXY", "true")
	t.Setenv("EDUCHAN_THREAD_COOLDOWN", "45")
	t.Setenv("EDUCHAN_POST_COOLDOWN", "1500ms")
	t.Setenv("EDUCHAN_LOG_FORMAT", "json")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Addr != "127.0.0.1:9000" || cfg.DatabaseURL != "postgres://example/db" {
		t.Errorf("unexpected address or database URL: %+v", cfg)
	}
	if cfg.PosterIDSalt != "pepper" || cfg.PosterIDSaltEphemeral {
		t.Error("a configured salt should be used verbatim")
	}
	if !cfg.TrustProxyHeaders {
		t.Error("EDUCHAN_TRUST_PROXY=true should be honoured")
	}
	if cfg.ThreadCooldown != 45*time.Second {
		t.Errorf("ThreadCooldown = %s, want 45s (bare numbers are seconds)", cfg.ThreadCooldown)
	}
	if cfg.PostCooldown != 1500*time.Millisecond {
		t.Errorf("PostCooldown = %s, want 1.5s", cfg.PostCooldown)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
}

func TestConfigFromEnvRejectsInvalidValues(t *testing.T) {
	t.Run("cooldown", func(t *testing.T) {
		t.Setenv("EDUCHAN_POST_COOLDOWN", "soon")
		if _, err := ConfigFromEnv(); err == nil {
			t.Error("expected an error for an unparseable duration")
		}
	})
	t.Run("negative cooldown", func(t *testing.T) {
		t.Setenv("EDUCHAN_POST_COOLDOWN", "-5")
		if _, err := ConfigFromEnv(); err == nil {
			t.Error("expected an error for a negative duration")
		}
	})
	t.Run("log format", func(t *testing.T) {
		t.Setenv("EDUCHAN_LOG_FORMAT", "yaml")
		if _, err := ConfigFromEnv(); err == nil {
			t.Error("expected an error for an unknown log format")
		}
	})
}
