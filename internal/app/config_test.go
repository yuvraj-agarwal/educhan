package app

import (
	"testing"
	"time"
)

func TestConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("PORT", "")
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
	if cfg.TrustedProxyHops != 0 {
		t.Errorf("TrustedProxyHops = %d, want 0: proxy headers must not be trusted by default",
			cfg.TrustedProxyHops)
	}
	if cfg.ThreadCooldown <= 0 || cfg.PostCooldown <= 0 || cfg.UploadCooldown <= 0 {
		t.Errorf("flood control must be on by default, got %+v", cfg)
	}
}

func TestConfigFromEnvOverrides(t *testing.T) {
	t.Setenv("EDUCHAN_ADDR", "127.0.0.1:9000")
	t.Setenv("DATABASE_URL", "postgres://example/db")
	t.Setenv("EDUCHAN_POSTER_ID_SALT", "pepper")
	t.Setenv("EDUCHAN_TRUST_PROXY", "true")
	t.Setenv("EDUCHAN_THREAD_COOLDOWN", "45")
	t.Setenv("EDUCHAN_POST_COOLDOWN", "1500ms")
	t.Setenv("EDUCHAN_UPLOAD_COOLDOWN", "20")
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
	if cfg.TrustedProxyHops != 1 {
		t.Errorf("TrustedProxyHops = %d, want 1: EDUCHAN_TRUST_PROXY=true means one proxy",
			cfg.TrustedProxyHops)
	}
	if cfg.ThreadCooldown != 45*time.Second {
		t.Errorf("ThreadCooldown = %s, want 45s (bare numbers are seconds)", cfg.ThreadCooldown)
	}
	if cfg.PostCooldown != 1500*time.Millisecond {
		t.Errorf("PostCooldown = %s, want 1.5s", cfg.PostCooldown)
	}
	if cfg.UploadCooldown != 20*time.Second {
		t.Errorf("UploadCooldown = %s, want 20s", cfg.UploadCooldown)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
}

// Cloud Run and comparable runtimes hand the container a port in PORT and
// expect the server to listen on it, so it has to beat the built-in default —
// but never an explicit EDUCHAN_ADDR.
func TestConfigListenAddr(t *testing.T) {
	t.Run("PORT is honoured", func(t *testing.T) {
		t.Setenv("EDUCHAN_ADDR", "")
		t.Setenv("PORT", "9090")
		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if cfg.Addr != ":9090" {
			t.Errorf("Addr = %q, want :9090", cfg.Addr)
		}
	})
	t.Run("EDUCHAN_ADDR wins", func(t *testing.T) {
		t.Setenv("EDUCHAN_ADDR", "127.0.0.1:9000")
		t.Setenv("PORT", "9090")
		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if cfg.Addr != "127.0.0.1:9000" {
			t.Errorf("Addr = %q, want 127.0.0.1:9000", cfg.Addr)
		}
	})
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
	t.Run("proxy hops", func(t *testing.T) {
		t.Setenv("EDUCHAN_TRUST_PROXY", "sometimes")
		if _, err := ConfigFromEnv(); err == nil {
			t.Error("expected an error for an unparseable proxy hop count")
		}
	})
	t.Run("negative proxy hops", func(t *testing.T) {
		t.Setenv("EDUCHAN_TRUST_PROXY", "-1")
		if _, err := ConfigFromEnv(); err == nil {
			t.Error("expected an error for a negative proxy hop count")
		}
	})
}

func TestConfigProxyHops(t *testing.T) {
	cases := map[string]int{
		"":      0,
		"false": 0,
		"off":   0,
		"true":  1,
		"on":    1,
		"0":     0,
		"1":     1,
		"2":     2,
		" 3 ":   3,
	}
	for raw, want := range cases {
		t.Run("value "+raw, func(t *testing.T) {
			t.Setenv("EDUCHAN_TRUST_PROXY", raw)
			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatalf("ConfigFromEnv: %v", err)
			}
			if cfg.TrustedProxyHops != want {
				t.Errorf("EDUCHAN_TRUST_PROXY=%q gave %d hops, want %d", raw, cfg.TrustedProxyHops, want)
			}
		})
	}
}
